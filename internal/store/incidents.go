package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Presentation incident bundles (Designv9 §7, Plan Step 29). A durable
// correlator groups completed briefings into burst_appearance or
// burst_disappearance incidents from the scanner's explicit event
// types: same authenticated source and target, a five-minute window,
// and at least three distinct qualified reported-device references.
// Identity is never inferred from MAC/IP/hostname attributes, silence
// never manufactures a lifecycle event, and member briefings are never
// rewritten. Insufficient identity leaves individual reports; there is
// no uncounted-bundle path — keeping reports separate is the simpler
// contract-preserving default.
//
// Deterministic choices the contract leaves to the implementation:
//   - The formation window is governed by briefing completion time
//     (briefings.created_at), the only per-briefing timestamp that
//     survives event purge. Window membership is inclusive:
//     [window_start, window_start+window], with window_start pinned to
//     the oldest member's completion.
//   - One open incident exists per (kind, source, target) key. A
//     candidate outside the open window waits for the next incident;
//     a candidate older than its incident's start never backfills it.
//   - Quiet closure is inclusive: now-last_member_at >= quiet closes.
//     last_member_at moves only when a new member is accepted, never
//     on duplicate processing.
//   - A completed briefing whose completion is older than
//     window+quiet at processing time is consumed without formation:
//     a stale burst presents as its individual reports, never as a
//     freshly opened incident. Qualified pending briefings are
//     indexed by scope and reference, so thin bursts spanning pages,
//     ticks and restarts do not compete with unrelated reports.
//   - A briefing whose retained events mix appear and disappear types,
//     mix sources, or mix/omit targets is not a candidate for either
//     direction. Missing or ambiguous scope never forms an incident.
//   - Qualification reads immutable member evidence first (explicit
//     event types from retained group events; reference class,
//     namespace, revision and confidence decoded from retained event
//     payloads, snapshotted into the member row at accept time), then
//     checks current source qualification (bound namespace plus the
//     advertised stable-identity capability). Low, unknown or mixed
//     confidence, unknown or mixed revisions, and unsupported
//     provenance fail closed. Heartbeat capability alone qualifies
//     nothing.
const (
	// DecisionSubjectIncident addresses presentation incidents in
	// operator decisions (Designv9 §§5.1, 7).
	DecisionSubjectIncident = "incident"

	// IncidentKindAppearance groups explicit scanner-reported
	// appearances; IncidentKindDisappearance groups disappearances.
	IncidentKindAppearance    = "burst_appearance"
	IncidentKindDisappearance = "burst_disappearance"

	IncidentOpen   = "open"
	IncidentClosed = "closed"
)

// Incident is one durable presentation bundle. Floor is the maximum
// member final priority, lifted to medium for disappearances;
// members keep their own priorities. QualifiedRefs counts distinct
// qualified upstream references across members; MemberCount counts
// member briefings. WindowStart pins the formation window;
// LastMemberAt drives quiet closure; ClosedAt is set on close.
type Incident struct {
	IncidentID        string
	Kind              string
	SourceScope       string
	Target            string
	Status            string
	Floor             string
	IdentityRevision  string
	IdentityNamespace string
	QualifiedRefs     int
	MemberCount       int
	WindowStart       time.Time
	LastMemberAt      time.Time
	OpenedAt          time.Time
	ClosedAt          time.Time
	HasClosedAt       bool
	UpdatedAt         time.Time
}

const incidentColumns = `incident_id, kind, source_scope, target, status, floor,
	identity_revision, identity_namespace, qualified_refs, member_count, window_start, last_member_at,
	opened_at, closed_at, updated_at`

func scanIncident(row interface {
	Scan(dest ...any) error
}, in *Incident) error {
	var windowStart, lastMember, opened, updated int64
	var closed sql.NullInt64
	if err := row.Scan(&in.IncidentID, &in.Kind, &in.SourceScope, &in.Target, &in.Status, &in.Floor,
		&in.IdentityRevision, &in.IdentityNamespace, &in.QualifiedRefs, &in.MemberCount, &windowStart, &lastMember,
		&opened, &closed, &updated); err != nil {
		return err
	}
	in.WindowStart, in.LastMemberAt, in.OpenedAt, in.UpdatedAt =
		fromNanos(windowStart), fromNanos(lastMember), fromNanos(opened), fromNanos(updated)
	if closed.Valid {
		in.ClosedAt, in.HasClosedAt = fromNanos(closed.Int64), true
	}
	return nil
}

// MemberRef is one immutable qualification snapshot accepted with a
// member briefing: the exact upstream reference under its namespace,
// revision and reported confidence. The snapshot freezes member-event
// evidence at accept time so later subject drift cannot rewrite burst
// membership or counts.
type MemberRef struct {
	Reference  string `json:"reference"`
	Namespace  string `json:"namespace"`
	Revision   string `json:"revision"`
	Confidence string `json:"confidence"`
}

// IncidentMember pairs a member briefing with its stored evidence:
// completion time, final priority, and the accept-time reference
// snapshot.
type IncidentMember struct {
	BriefingID string
	CreatedAt  time.Time
	Priority   string
	Refs       []MemberRef
}

func marshalMemberRefs(refs []MemberRef) (string, error) {
	if len(refs) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func parseMemberRefs(raw string) ([]MemberRef, error) {
	if raw == "" {
		return nil, nil
	}
	var refs []MemberRef
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return nil, err
	}
	return refs, nil
}

// incidentFloorRank orders final priorities; unknown strings rank
// below low, like the overview computation.
func incidentFloorRank(p string) int {
	switch p {
	case "critical":
		return 3
	case "high":
		return 2
	case "medium":
		return 1
	case "low":
		return 0
	default:
		return -1
	}
}

func incidentFloorName(rank int) string {
	switch {
	case rank >= 3:
		return "critical"
	case rank == 2:
		return "high"
	case rank == 1:
		return "medium"
	default:
		return "low"
	}
}

// incidentFloorOf derives the incident floor from member finals: the
// maximum, lifted to medium for disappearances. Members are never
// rewritten; only the incident row carries the floor.
func incidentFloorOf(kind string, priorities []string) string {
	rank := 0
	for _, p := range priorities {
		if r := incidentFloorRank(p); r > rank {
			rank = r
		}
	}
	if kind == IncidentKindDisappearance && rank < 1 {
		rank = 1
	}
	return incidentFloorName(rank)
}

// IncidentSettings carries the correlator horizons. Window bounds the
// formation span, Quiet bounds member silence before close, MinRefs is
// the distinct-qualified-reference threshold, and Retention bounds
// closed-incident storage measured from close.
type IncidentSettings struct {
	Window    time.Duration
	Quiet     time.Duration
	MinRefs   int
	Retention time.Duration
}

// DefaultIncidentSettings mirrors Designv9 §7: 30s tick (applied by
// the loop owner), 5m window, 120s quiet, 3 references, 90d retention.
func DefaultIncidentSettings() IncidentSettings {
	return IncidentSettings{
		Window:    5 * time.Minute,
		Quiet:     120 * time.Second,
		MinRefs:   3,
		Retention: 90 * 24 * time.Hour,
	}
}

// IncidentReport counts one correlator pass: incidents created,
// member briefings appended, and open incidents quiet-closed.
// Changed carries the affected incident IDs in deterministic order,
// capped so a burst-heavy pass cannot bloat the SSE frame; Truncated
// marks the cap.
type IncidentReport struct {
	Created   int
	Appended  int
	Closed    int
	Changed   []string
	Truncated bool
}

func (r *IncidentReport) markChanged(id string) {
	if len(r.Changed) >= maxIncidentChangedIDs {
		r.Truncated = true
		return
	}
	r.Changed = append(r.Changed, id)
}

// maxIncidentChangedIDs caps the changed-ID list in one report.
const maxIncidentChangedIDs = 100

// maxIncidentPage bounds one catch-up read: backlog drains over
// successive passes instead of one unbounded transaction.
const maxIncidentPage = 200

// maxIncidentPasses bounds one CorrelateIncidents call so downtime
// backlog cannot hold the writer indefinitely; the next tick resumes
// from the durable cursor.
const maxIncidentPasses = 10

// maxIncidentOpen bounds the open-incident scan per pass. Open
// incidents quiet-close within minutes, so hundreds of open rows
// already indicate a stuck correlator; the cap keeps the pass bounded
// while the next tick continues the drain.
const maxIncidentOpen = 500

// maxIncidentMembersPerPass bounds member writes in one pass. Larger
// bursts are not lost: leftover candidates stay unmembered and fresh,
// so the next pass reconsiders them against the same open incident.
// The cap matches the client row budget, keeping transactions small
// while realistic bursts still commit in one pass.
const maxIncidentMembersPerPass = 500

// candidateBriefing is one completed briefing with its retained
// evidence attached for classification. Membered marks rows that
// already belong to an incident: consumed for cursor purposes, never
// reclassified.
type candidateBriefing struct {
	seq        int64
	briefingID string
	groupID    string
	priority   string
	createdAt  time.Time
	membered   bool
}

// classifiedBriefing is a candidate accepted for one incident
// direction with its uniform scope and qualified references.
type classifiedBriefing struct {
	candidateBriefing
	kind       string
	source     string
	target     string
	revision   string
	references []string
	refs       []MemberRef
}

// CorrelateIncidents runs one bounded correlator pass: it pages
// completed briefings through the durable cursor, persists qualified
// pending briefings, forms and extends incidents, quiet-closes idle
// ones, and advances the cursor — progress and incident changes commit
// in the same writer transactions, so retries, cancellation and
// restart neither duplicate membership nor lose the cursor.
func (s *Store) CorrelateIncidents(ctx context.Context, settings IncidentSettings, now time.Time) (IncidentReport, error) {
	if settings.Window <= 0 {
		return IncidentReport{}, fmt.Errorf("incident window must be positive")
	}
	if settings.Quiet <= 0 {
		return IncidentReport{}, fmt.Errorf("incident quiet window must be positive")
	}
	if settings.MinRefs < 1 || settings.MinRefs > maxIncidentMembersPerPass {
		return IncidentReport{}, fmt.Errorf("incident reference threshold must be between 1 and 500")
	}
	var report IncidentReport
	for pass := 0; pass < maxIncidentPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		advanced, err := s.correlatePass(ctx, settings, now, &report)
		if err != nil {
			return report, err
		}
		if !advanced {
			break
		}
	}
	sort.Strings(report.Changed)
	return report, nil
}

// correlatePass processes one page past the cursor plus durable pending
// candidates. It reports whether the cursor advanced, i.e.
// whether another page may remain.
func (s *Store) correlatePass(ctx context.Context, settings IncidentSettings, now time.Time, report *IncidentReport) (bool, error) {
	cursor, err := s.incidentCursor(ctx)
	if err != nil {
		return false, err
	}
	page, err := s.incidentPage(ctx, cursor)
	if err != nil {
		return false, err
	}
	staleCut := now.Add(-settings.Window - settings.Quiet)
	var consider []candidateBriefing
	next := cursor
	for _, c := range page {
		if c.seq > next {
			next = c.seq
		}
		if !c.membered && !c.createdAt.Before(staleCut) {
			consider = append(consider, c)
		}
	}
	classified, err := s.classifyCandidates(ctx, consider)
	if err != nil {
		return false, err
	}
	more := next > cursor
	err = s.write(ctx, func(tx *sql.Tx) error {
		ready, err := backfillIncidentReferences(tx)
		if err != nil {
			return err
		}
		if !ready {
			more = true
			return nil
		}
		for _, c := range classified {
			if err := enqueueIncidentCandidate(tx, c); err != nil {
				return err
			}
		}
		// Expired work is removed in bounded pages, with child rows first.
		if _, err := tx.Exec(`DELETE FROM incident_pending_refs WHERE rowid IN (SELECT r.rowid FROM incident_pending_refs r JOIN incident_pending p ON p.briefing_id=r.briefing_id WHERE p.created_at<? ORDER BY p.created_at,r.rowid LIMIT 200)`, nanos(staleCut)); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM incident_pending WHERE briefing_id IN (SELECT p.briefing_id FROM incident_pending p WHERE p.created_at<? AND NOT EXISTS(SELECT 1 FROM incident_pending_refs r WHERE r.briefing_id=p.briefing_id) ORDER BY p.created_at LIMIT 200)`, nanos(staleCut)); err != nil {
			return err
		}
		open, err := loadOpenIncidents(tx)
		if err != nil {
			return err
		}
		keys, err := pendingKeys(tx)
		if err != nil {
			return err
		}
		budget := maxIncidentMembersPerPass
		changed := false
		for _, key := range keys {
			if budget <= 0 {
				more = true
				break
			}
			sample, err := readPending(tx, key, staleCut, now, 1)
			if err != nil {
				return err
			}
			if len(sample) == 0 {
				continue
			}
			qualified, err := pendingSourceQualified(tx, sample[0])
			if err != nil {
				return err
			}
			if !qualified {
				continue
			}
			k := incidentKey(sample[0].kind, sample[0].source, sample[0].target)
			in := open[k]
			if in != nil && in.IdentityRevision == sample[0].revision && in.IdentityNamespace == sample[0].refs[0].Namespace {
				members, err := readPending(tx, key, in.WindowStart, in.WindowStart.Add(settings.Window), budget)
				if err != nil {
					return err
				}
				n, err := appendIncidentMembers(tx, in, members, settings.Window, settings.Quiet, now)
				if err != nil {
					return err
				}
				if n > 0 {
					changed = true
					report.Appended += n
					report.markChanged(in.IncidentID)
					budget -= n
					more = more || n == len(members)
				}
			}
			if in != nil && len(page) < maxIncidentPage && now.Sub(in.LastMemberAt) >= settings.Quiet {
				// A bounded pass may leave members whose completion precedes the
				// quiet deadline. Defer closure until that pending work is applied.
				oldKey := classifiedBriefing{kind: in.Kind, source: in.SourceScope, target: in.Target, revision: in.IdentityRevision, refs: []MemberRef{{Namespace: in.IdentityNamespace}}}
				waiting, err := readPending(tx, pendingKey(oldKey), in.WindowStart, minTime(in.WindowStart.Add(settings.Window), in.LastMemberAt.Add(settings.Quiet)), 1)
				if err != nil {
					return err
				}
				if len(waiting) == 0 {
					if err := closeIncident(tx, in.IncidentID, now); err != nil {
						return err
					}
					report.Closed++
					report.markChanged(in.IncidentID)
					delete(open, k)
					in = nil
				}
			}
			if in == nil {
				members, err := formationCandidates(tx, key, settings, now, budget)
				if err != nil {
					return err
				}
				id, err := formIncident(tx, s, members, settings, now)
				if err != nil {
					return err
				}
				if id != "" {
					changed = true
					report.Created++
					report.markChanged(id)
					budget -= len(members)
				}
			}
		}
		// Keys with no pending work must also quiet-close.
		for _, in := range open {
			if len(page) == maxIncidentPage || now.Sub(in.LastMemberAt) < settings.Quiet {
				continue
			}
			c := classifiedBriefing{kind: in.Kind, source: in.SourceScope, target: in.Target, revision: in.IdentityRevision, refs: []MemberRef{{Namespace: in.IdentityNamespace}}}
			waiting, err := readPending(tx, pendingKey(c), in.WindowStart, minTime(in.WindowStart.Add(settings.Window), in.LastMemberAt.Add(settings.Quiet)), 1)
			if err != nil {
				return err
			}
			qualified := false
			if len(waiting) > 0 {
				qualified, err = pendingSourceQualified(tx, waiting[0])
				if err != nil {
					return err
				}
			}
			if !qualified {
				if err := closeIncident(tx, in.IncidentID, now); err != nil {
					return err
				}
				report.Closed++
				report.markChanged(in.IncidentID)
			}
		}
		if changed {
			if _, err := tx.Exec(`UPDATE incident_progress SET pin_version=pin_version+1 WHERE id=0`); err != nil {
				return err
			}
		}
		if next > cursor {
			return saveIncidentCursor(tx, next)
		}
		return nil
	})
	return more, err
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func incidentKey(kind, source, target string) string {
	return kind + "\x00" + source + "\x00" + target
}

// incidentCursor reads the durable catch-up position: briefings at or
// below it have durable pending state or were ineligible. The sequence
// never recycles across purge, unlike SQLite rowids.
func (s *Store) incidentCursor(ctx context.Context) (int64, error) {
	var cursor sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT last_briefing_seq FROM incident_progress WHERE id = 0`).Scan(&cursor); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return cursor.Int64, nil
}

func saveIncidentCursor(tx *sql.Tx, rowID int64) error {
	_, err := tx.Exec(`INSERT INTO incident_progress (id, last_briefing_seq) VALUES (0, ?)
		ON CONFLICT(id) DO UPDATE SET last_briefing_seq = MAX(last_briefing_seq, excluded.last_briefing_seq)`, rowID)
	return err
}

// incidentPage reads one catch-up page past the cursor in durable sequence order:
// stable across restarts and purges. Membership is
// terminal, so each row carries whether it already belongs to an
// incident: the cursor advances past every row, but classification
// only considers unmembered ones. Without the flag, a pass that
// quiet-closes an incident would re-form its still-paged members
// into a phantom second incident.
func (s *Store) incidentPage(ctx context.Context, cursor int64) ([]candidateBriefing, error) {
	return queryCandidates(ctx, s.db, `SELECT b.seq, b.briefing_id, b.group_id, b.priority, b.created_at,
		m.briefing_id IS NOT NULL
		FROM briefings b LEFT JOIN incident_members m ON m.briefing_id = b.briefing_id
		WHERE b.seq > ? ORDER BY b.seq ASC LIMIT ?`, cursor, maxIncidentPage)
}

func queryCandidates(ctx context.Context, db *sql.DB, query string, args ...any) ([]candidateBriefing, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidateBriefing
	for rows.Next() {
		var c candidateBriefing
		var created int64
		if err := rows.Scan(&c.seq, &c.briefingID, &c.groupID, &c.priority, &created, &c.membered); err != nil {
			return nil, err
		}
		c.createdAt = fromNanos(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// groupScope is the retained lifecycle evidence for one group: the
// distinct explicit event types with their uniform target and source.
// An empty event set means the retained evidence is gone; the group
// cannot seed or join an incident.
type groupScope struct {
	types   map[string]bool
	targets map[string]bool
	sources map[string]bool
	refs    []refEvidence
	invalid bool
}

// classifyCandidates attaches retained evidence to each considered
// briefing and keeps only unambiguous, currently-qualified members of
// one incident direction. Evidence reads are batched over bounded ID
// sets; classification itself is deterministic.
func (s *Store) classifyCandidates(ctx context.Context, consider []candidateBriefing) ([]classifiedBriefing, error) {
	if len(consider) == 0 {
		return nil, nil
	}
	groupIDs := make([]string, 0, len(consider))
	byGroup := map[string]bool{}
	for _, c := range consider {
		if !byGroup[c.groupID] {
			groupIDs = append(groupIDs, c.groupID)
		}
		byGroup[c.groupID] = true
	}
	scopes, err := groupScopes(ctx, s.db, groupIDs)
	if err != nil {
		return nil, err
	}
	// Current source qualification is one batched read over the
	// distinct scopes the evidence named.
	scopeSet := map[string]bool{}
	for _, sc := range scopes {
		for source := range sc.sources {
			scopeSet[source] = true
		}
	}
	qualified := map[string]bool{}
	if len(scopeSet) > 0 {
		names := make([]string, 0, len(scopeSet))
		for source := range scopeSet {
			names = append(names, source)
		}
		placeholders := strings.Repeat("?,", len(names))
		placeholders = strings.TrimSuffix(placeholders, ",")
		args := make([]any, 0, len(names))
		for _, n := range names {
			args = append(args, n)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT source_scope, configured_identity_namespace, capabilities
			FROM report_sources WHERE source_scope IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var scope, namespace, capabilities string
			if err := rows.Scan(&scope, &namespace, &capabilities); err != nil {
				rows.Close()
				return nil, err
			}
			// The stable-identity capability plus a bound namespace is
			// the current qualification (§1.4 I1): without it the
			// source's references stay uncounted and its reports stay
			// individual, with the reason visible in Health.
			if namespace != "" && HasCapability(capabilities, StableIdentityCapability) {
				qualified[scope+"\x00"+namespace] = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	var out []classifiedBriefing
	for _, c := range consider {
		sc, ok := scopes[c.groupID]
		if !ok || sc.invalid || len(sc.types) == 0 {
			continue
		}
		kind := ""
		switch {
		case sc.types["host.appeared"] && !sc.types["host.disappeared"]:
			kind = IncidentKindAppearance
		case sc.types["host.disappeared"] && !sc.types["host.appeared"]:
			kind = IncidentKindDisappearance
		default:
			// Mixed or lifecycle-free event sets never seed or
			// join: flap diagnosis and silence interpretation both
			// belong upstream.
			continue
		}
		if len(sc.targets) != 1 || len(sc.sources) != 1 {
			continue
		}
		var target, source string
		for t := range sc.targets {
			target = t
		}
		for sr := range sc.sources {
			source = sr
		}
		if target == "" || source == "" {
			continue
		}
		// Distinct qualified references from immutable event views: reported-device class under the source's bound
		// namespace, medium or better confidence, known revision.
		// Low, unknown or mixed confidence and unknown or mixed
		// revisions fail closed to individual reports.
		seen := map[string]MemberRef{}
		revisions := map[string]bool{}
		for _, r := range sc.refs {
			if r.Class != SubjectReportedDevice {
				continue
			}
			if !qualified[source+"\x00"+r.Namespace] {
				continue
			}
			if r.Confidence != "medium" && r.Confidence != "high" {
				continue
			}
			if r.Revision == "" {
				continue
			}
			revisions[r.Revision] = true
			if _, ok := seen[r.Reference]; !ok {
				seen[r.Reference] = MemberRef{
					Reference: r.Reference, Namespace: r.Namespace,
					Revision: r.Revision, Confidence: r.Confidence,
				}
			}
		}
		if len(seen) == 0 || len(revisions) != 1 {
			continue
		}
		references := make([]string, 0, len(seen))
		snap := make([]MemberRef, 0, len(seen))
		for ref, mr := range seen {
			references = append(references, ref)
			snap = append(snap, mr)
		}
		sort.Strings(references)
		sort.Slice(snap, func(a, b int) bool { return snap[a].Reference < snap[b].Reference })
		var revision string
		for r := range revisions {
			revision = r
		}
		out = append(out, classifiedBriefing{
			candidateBriefing: c, kind: kind, source: source,
			target: target, revision: revision, references: references, refs: snap,
		})
	}
	return out, nil
}

// refEvidence is one briefing subject link with the provenance the
// incident gate needs.
type refEvidence struct {
	Class      string
	Namespace  string
	Revision   string
	Reference  string
	Confidence string
}

func groupScopes(ctx context.Context, db *sql.DB, groupIDs []string) (map[string]*groupScope, error) {
	out := map[string]*groupScope{}
	for start := 0; start < len(groupIDs); start += maxIncidentPage {
		end := start + maxIncidentPage
		if end > len(groupIDs) {
			end = len(groupIDs)
		}
		placeholders := strings.Repeat("?,", end-start)
		placeholders = strings.TrimSuffix(placeholders, ",")
		args := make([]any, 0, end-start)
		for _, id := range groupIDs[start:end] {
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, `SELECT ge.group_id, e.event_type,
			COALESCE(e.target,''), COALESCE(e.source_scope,''), e.payload
			FROM group_events ge JOIN raw_events e ON e.event_id = ge.event_id
			WHERE ge.group_id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var gid, typ, target, source string
			var payload []byte
			if err := rows.Scan(&gid, &typ, &target, &source, &payload); err != nil {
				rows.Close()
				return nil, err
			}
			sc, ok := out[gid]
			if !ok {
				sc = &groupScope{types: map[string]bool{}, targets: map[string]bool{}, sources: map[string]bool{}}
				out[gid] = sc
			}
			sc.types[typ] = true
			sc.targets[target] = true
			sc.sources[source] = true
			views := parseEventViews(payload, typ)
			if len(views) == 0 {
				sc.invalid = true
			}
			for _, v := range views {
				class, namespace, reference := qualifiedReference(v)
				if class != SubjectReportedDevice || v.identityRevision == "" ||
					(v.identityConfidence != "high" && v.identityConfidence != "medium") {
					sc.invalid = true
				}
				sc.refs = append(sc.refs, refEvidence{Class: class, Namespace: namespace,
					Revision: v.identityRevision, Reference: reference, Confidence: v.identityConfidence})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// loadOpenIncidents reads every open incident, bounded: quiet closure
// normally keeps this set tiny.
func loadOpenIncidents(tx *sql.Tx) (map[string]*Incident, error) {
	rows, err := tx.Query(`SELECT `+incidentColumns+` FROM incidents
		WHERE status = 'open' ORDER BY updated_at ASC, rowid ASC LIMIT ?`, maxIncidentOpen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*Incident{}
	for rows.Next() {
		var in Incident
		if err := scanIncident(rows, &in); err != nil {
			return nil, err
		}
		out[incidentKey(in.Kind, in.SourceScope, in.Target)] = &in
	}
	return out, rows.Err()
}

func closeIncident(tx *sql.Tx, incidentID string, now time.Time) error {
	res, err := tx.Exec(`UPDATE incidents SET status = 'closed', closed_at = ?, updated_at = ?
		WHERE incident_id = ? AND status = 'open'`, nanos(now), nanos(now), incidentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("incident %s is not open", incidentID)
	}
	return nil
}

// appendIncidentMembers accepts the window- and revision-matching
// prefix of members into an open incident and refreshes its counts,
// floor and quiet state in one row update. Out-of-window and
// revision-mismatched candidates stay unmembered for the next burst.
// Member inserts use OR IGNORE against the briefing UNIQUE: a retried
// pass replays as already-member instead of duplicating, and quiet
// time advances only for newly accepted members.
func appendIncidentMembers(tx *sql.Tx, in *Incident, members []classifiedBriefing, window, quiet time.Duration, now time.Time) (int, error) {
	windowEnd := in.WindowStart.Add(window)
	newRefs := 0
	priorities := []string{in.Floor}
	appended := 0
	lastMember := in.LastMemberAt
	for _, m := range members {
		if m.createdAt.Before(in.WindowStart) || m.createdAt.After(windowEnd) {
			continue
		}
		if m.revision != in.IdentityRevision || m.refs[0].Namespace != in.IdentityNamespace || m.createdAt.After(lastMember.Add(quiet)) {
			continue
		}
		refsJSON, err := marshalMemberRefs(m.refs)
		if err != nil {
			return 0, err
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO incident_members
			(incident_id, briefing_id, member_created_at, refs_json)
			VALUES (?, ?, ?, ?)`, in.IncidentID, m.briefingID, nanos(m.createdAt), refsJSON)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			// Already a member (possibly of another incident under
			// a retried pass): never double-count, never move quiet.
			continue
		}
		nr, err := insertMemberReferences(tx, in.IncidentID, m.refs)
		if err != nil {
			return 0, err
		}
		newRefs += nr
		if err := deletePendingCandidate(tx, m.briefingID); err != nil {
			return 0, err
		}
		appended++
		priorities = append(priorities, m.priority)
		if m.createdAt.After(lastMember) {
			lastMember = m.createdAt
		}
	}
	if appended == 0 {
		return 0, nil
	}
	in.QualifiedRefs += newRefs
	in.MemberCount += appended
	in.Floor = incidentFloorOf(in.Kind, priorities)
	in.LastMemberAt = lastMember
	if _, err := tx.Exec(`UPDATE incidents SET qualified_refs=?,member_count=?,floor=?,last_member_at=?,updated_at=? WHERE incident_id=?`, in.QualifiedRefs, in.MemberCount, in.Floor, nanos(lastMember), nanos(now), in.IncidentID); err != nil {
		return 0, err
	}

	return appended, nil
}

// formIncident opens one incident from the newest-anchored formation
// set: members within [newest-window, newest] sharing one revision
// with at least MinRefs distinct qualified references. Mixed
// revisions or thin identity leaves the reports individual. Creation
// is lookup-first: a concurrent or retried pass that finds an open
// incident for the key appends instead of duplicating.
func formIncident(tx *sql.Tx, s *Store, members []classifiedBriefing, settings IncidentSettings, now time.Time) (string, error) {
	if len(members) == 0 {
		return "", nil
	}
	newest := members[len(members)-1].createdAt
	cut := newest.Add(-settings.Window)
	var set []classifiedBriefing
	revisions := map[string]bool{}
	refs := map[string]bool{}
	for _, m := range members {
		if m.createdAt.Before(cut) {
			continue
		}
		set = append(set, m)
		revisions[m.revision] = true
		for _, r := range m.references {
			refs[r] = true
		}
	}
	if len(revisions) != 1 || len(refs) < settings.MinRefs {
		return "", nil
	}
	var revision string
	for r := range revisions {
		revision = r
	}
	windowStart := set[0].createdAt
	kind, source, target := set[0].kind, set[0].source, set[0].target
	var existing string
	err := tx.QueryRow(`SELECT incident_id FROM incidents
		WHERE kind = ? AND source_scope = ? AND target = ? AND status = 'open'`,
		kind, source, target).Scan(&existing)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if err == nil {
		// Lost a race with another pass: the open incident owns this
		// key now. The next pass reconsiders these members against it.
		return "", nil
	}
	priorities := make([]string, 0, len(set))
	for _, m := range set {
		priorities = append(priorities, m.priority)
	}
	// The ID embeds the window start and a process-unique sequence;
	// on the vanishingly rare collision the insert is ignored and the
	// next sequence value is tried, so creation never aborts a pass.
	var incidentID string
	for attempt := 0; attempt < 5; attempt++ {
		incidentID = fmt.Sprintf("inc-%d-%d", windowStart.UnixNano(), s.incidentSeq.Add(1))
		res, err := tx.Exec(`INSERT OR IGNORE INTO incidents (incident_id, kind, source_scope, target,
			status, floor, identity_revision, identity_namespace, qualified_refs, member_count,
			window_start, last_member_at, opened_at, updated_at)
			VALUES (?, ?, ?, ?, 'open', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			incidentID, kind, source, target, incidentFloorOf(kind, priorities), revision, set[0].refs[0].Namespace,
			len(refs), len(set), nanos(windowStart), nanos(set[len(set)-1].createdAt),
			nanos(now), nanos(now))
		if err != nil {
			return "", err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			break
		}
		incidentID = ""
	}
	if incidentID == "" {
		return "", fmt.Errorf("incident id allocation failed for %s %s %s", kind, source, target)
	}
	// A formation that lands zero members removes its parent row
	// instead of publishing an empty incident with inflated counts:
	// every skipped row is a briefing already owned elsewhere.
	// Callers only offer unmembered rows, which makes this a
	// backstop, not a path — but the row must never exist without
	// members.
	inserted := 0
	for _, m := range set {
		refsJSON, err := marshalMemberRefs(m.refs)
		if err != nil {
			return "", err
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO incident_members
			(incident_id, briefing_id, member_created_at, refs_json)
			VALUES (?, ?, ?, ?)`, incidentID, m.briefingID, nanos(m.createdAt), refsJSON)
		if err != nil {
			return "", err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			if _, err := insertMemberReferences(tx, incidentID, m.refs); err != nil {
				return "", err
			}
			if err := deletePendingCandidate(tx, m.briefingID); err != nil {
				return "", err
			}
			inserted++
		}
	}
	if inserted == 0 {
		if _, err := tx.Exec(`DELETE FROM incidents WHERE incident_id = ?`, incidentID); err != nil {
			return "", err
		}
		return "", nil
	}
	return incidentID, nil
}

// GetIncident reads one incident by ID, or sql.ErrNoRows.
func (s *Store) GetIncident(ctx context.Context, incidentID string) (Incident, error) {
	var in Incident
	if incidentID == "" {
		return in, fmt.Errorf("empty incident id")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE incident_id = ?`, incidentID)
	if err := scanIncident(row, &in); err != nil {
		return Incident{}, err
	}
	return in, nil
}

// maxIncidentDetailMembers caps the member section of one detail
// read; the row reports truncation instead of growing the response
// with the burst.
const maxIncidentDetailMembers = 500

// IncidentDetail is one incident with its bounded member evidence.
// The embedded list row carries the cursor material alongside the
// incident fields.
type IncidentDetail struct {
	IncidentListRow
	Members   []IncidentMember
	Truncated bool
}

// IncidentDetail reads one incident with its members oldest-first. An
// unknown ID reads sql.ErrNoRows (404 at the API, never 410: without
// tombstones a purged incident is indistinguishable from a typo).
func (s *Store) IncidentDetail(ctx context.Context, incidentID string) (IncidentDetail, error) {
	var out IncidentDetail
	if incidentID == "" {
		return out, fmt.Errorf("empty incident id")
	}
	row := s.db.QueryRowContext(ctx, `SELECT rowid, `+incidentColumns+` FROM incidents WHERE incident_id = ?`, incidentID)
	if err := scanIncidentRow(row, &out.CursorRowID, &out.Incident); err != nil {
		return IncidentDetail{}, err
	}
	out.CursorNanos = out.OpenedAt.UnixNano()
	rows, err := s.db.QueryContext(ctx, `SELECT m.briefing_id, m.member_created_at, m.refs_json, b.priority
		FROM incident_members m JOIN briefings b ON b.briefing_id = m.briefing_id
		WHERE m.incident_id = ? ORDER BY m.member_created_at ASC, m.briefing_id ASC LIMIT ?`,
		incidentID, maxIncidentDetailMembers+1)
	if err != nil {
		return IncidentDetail{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var mb IncidentMember
		var created int64
		var raw string
		if err := rows.Scan(&mb.BriefingID, &created, &raw, &mb.Priority); err != nil {
			return IncidentDetail{}, err
		}
		mb.CreatedAt = fromNanos(created)
		refs, err := parseMemberRefs(raw)
		if err != nil {
			return IncidentDetail{}, err
		}
		mb.Refs = refs
		out.Members = append(out.Members, mb)
	}
	if err := rows.Err(); err != nil {
		return IncidentDetail{}, err
	}
	if len(out.Members) > maxIncidentDetailMembers {
		out.Members = out.Members[:maxIncidentDetailMembers]
		out.Truncated = true
	}
	return out, nil
}

// IncidentListRow pairs an incident with its list-cursor material:
// the opened nanos and storage rowid the wire cursor encodes. The
// embedded Incident promotes every field, so readers use the row
// exactly like an Incident.
type IncidentListRow struct {
	Incident
	CursorNanos int64
	CursorRowID int64
}

// ListIncidents pages incidents newest-first over the
// (opened nanos, rowid) cursor. Zero cursor starts at the newest;
// limit clamps to the serving bound. A malformed cursor is a caller
// error; an unknown-but-well-formed one pages to an empty page.
func (s *Store) ListIncidents(ctx context.Context, limit int, beforeNanos, beforeRowID int64) ([]IncidentListRow, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	query := `SELECT rowid, ` + incidentColumns + ` FROM incidents`
	args := []any{}
	if beforeRowID > 0 {
		query += ` WHERE (opened_at < ? OR (opened_at = ? AND rowid < ?))`
		args = append(args, beforeNanos, beforeNanos, beforeRowID)
	}
	query += ` ORDER BY opened_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IncidentListRow
	for rows.Next() {
		var in IncidentListRow
		if err := scanIncidentRow(rows, &in.CursorRowID, &in.Incident); err != nil {
			return nil, err
		}
		in.CursorNanos = in.OpenedAt.UnixNano()
		out = append(out, in)
	}
	return out, rows.Err()
}

// scanIncidentRow scans one incident row prefixed with its storage
// rowid. The rowid backs list cursors; it never leaves the store.
func scanIncidentRow(row interface {
	Scan(dest ...any) error
}, rowID *int64, in *Incident) error {
	var windowStart, lastMember, opened, updated int64
	var closed sql.NullInt64
	if err := row.Scan(rowID, &in.IncidentID, &in.Kind, &in.SourceScope, &in.Target, &in.Status, &in.Floor,
		&in.IdentityRevision, &in.IdentityNamespace, &in.QualifiedRefs, &in.MemberCount, &windowStart, &lastMember,
		&opened, &closed, &updated); err != nil {
		return err
	}
	in.WindowStart, in.LastMemberAt, in.OpenedAt, in.UpdatedAt =
		fromNanos(windowStart), fromNanos(lastMember), fromNanos(opened), fromNanos(updated)
	if closed.Valid {
		in.ClosedAt, in.HasClosedAt = fromNanos(closed.Int64), true
	}
	return nil
}

// IncidentDecisionResult reports an incident decision and the member
// decisions it generated. Members holds the snapshot-order generated
// rows; empty when the decision stayed incident-only.
const maxIncidentDecisionMembers = 500

// ErrIncidentFanoutLimit rejects an oversized atomic review action before
// any decision is written. Individual member reviews remain available.
var ErrIncidentFanoutLimit = errors.New("incident member action exceeds limit")

type IncidentDecisionResult struct {
	Parent  Decision
	Applied bool
	Reason  string
	Members []Decision
}

// AppendIncidentDecision stores one decision on an incident with an
// explicit member scope (Designv9 §7). includeMembers=false leaves
// member decisions untouched: incident-only acknowledgement never
// implicitly suppresses member attention. includeMembers=true applies
// the same kind to a durable snapshot of membership taken in the
// commit transaction — later joiners are not automatically
// acknowledged — and parent plus generated rows commit atomically.
// Generated rows carry generated_by naming the parent, so undo of the
// parent can identify them without touching later independent
// operator judgments on the same briefings.
//
// Retries are safe: member fan-out requires a request ID, and a
// replayed key returns the original parent with its generated rows
// instead of duplicating. A closed incident still takes decisions;
// an unknown incident is 404; an empty membership commits the parent
// with no generated rows.
func (s *Store) AppendIncidentDecision(ctx context.Context, in DecisionInput, includeMembers bool) (IncidentDecisionResult, error) {
	now := s.now()
	if in.SubjectKind != DecisionSubjectIncident {
		return IncidentDecisionResult{}, fmt.Errorf("%w: member scope needs an incident subject", ErrInvalidDecision)
	}
	if err := checkRequestID(in.RequestID); err != nil {
		return IncidentDecisionResult{}, err
	}
	if includeMembers && in.RequestID == "" {
		return IncidentDecisionResult{}, fmt.Errorf("%w: member acknowledgement needs a request id", ErrInvalidDecision)
	}
	if err := checkDecisionMatrix(in.Kind, in.SubjectKind); err != nil {
		return IncidentDecisionResult{}, err
	}
	if err := checkDecisionText(in.Kind, in.Text); err != nil {
		return IncidentDecisionResult{}, err
	}
	var hashUntil int64
	if in.HasUntil {
		hashUntil = in.Until.UnixNano()
	}
	scopeTag := "incident-only"
	if includeMembers {
		scopeTag = "include-members"
	}
	hash := decisionHash(in.SubjectKind, in.SubjectID, in.Kind, hashUntil, in.HasUntil, in.Text, scopeTag)

	var res IncidentDecisionResult
	err := s.write(ctx, func(tx *sql.Tx) error {
		if in.RequestID != "" {
			existing, err := findDecisionByRequest(tx, in.RequestID, hash)
			if errors.Is(err, ErrRequestConflict) {
				// v26 hashes predate explicit member scope. Recover its actual
				// stored scope without rewriting retained audit at upgrade.
				legacyHash := decisionHash(in.SubjectKind, in.SubjectID, in.Kind, hashUntil, in.HasUntil, in.Text, "")
				existing, err = findDecisionByRequest(tx, in.RequestID, legacyHash)
				if err == nil && existing != nil {
					var hadMembers bool
					err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM operator_decisions WHERE generated_by=?)`, existing.DecisionID).Scan(&hadMembers)
					if err == nil && hadMembers != includeMembers {
						err = ErrRequestConflict
					}
				}
			}
			if err != nil {
				return err
			} else if existing != nil {
				members, err := generatedDecisions(tx, existing.DecisionID)
				if err != nil {
					return err
				}
				res = IncidentDecisionResult{Parent: *existing, Applied: false, Reason: "duplicate_request", Members: members}
				return nil
			}
		}
		untilNanos, err := resolveDecisionUntil(in, now)
		if err != nil {
			return err
		}
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM incidents WHERE incident_id = ?`, in.SubjectID).Scan(&one); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("%w: %q", ErrUnknownSubject, in.SubjectID)
			}
			return err
		}
		var members []string
		if includeMembers {
			rows, err := tx.Query(`SELECT briefing_id FROM incident_members WHERE incident_id = ?
				ORDER BY member_created_at ASC, briefing_id ASC LIMIT ?`, in.SubjectID, maxIncidentDecisionMembers+1)
			if err != nil {
				return err
			}
			for rows.Next() {
				var bid string
				if err := rows.Scan(&bid); err != nil {
					rows.Close()
					return err
				}
				members = append(members, bid)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
		}
		if len(members) > maxIncidentDecisionMembers {
			return ErrIncidentFanoutLimit
		}
		parentID := fmt.Sprintf("dec-%d-%d", now.UnixNano(), s.decisionSeq.Add(1))
		var untilVal sql.NullInt64
		if in.Kind == DecisionSnooze {
			untilVal = sql.NullInt64{Int64: untilNanos, Valid: true}
		}
		if _, err := tx.Exec(`INSERT INTO operator_decisions (decision_id, subject_kind, subject_id, kind,
			until, text, created_at, supersedes, request_id, content_hash, prior_label, prior_owner, generated_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, NULL, NULL, '')`,
			parentID, in.SubjectKind, in.SubjectID, in.Kind, untilVal, in.Text, now.UnixNano(),
			nullString(in.RequestID), hash); err != nil {
			return err
		}
		parent := Decision{DecisionID: parentID, SubjectKind: in.SubjectKind, SubjectID: in.SubjectID,
			Kind: in.Kind, Text: in.Text, CreatedAt: now, RequestID: in.RequestID}
		if in.Kind == DecisionSnooze {
			parent.Until, parent.HasUntil = fromNanos(untilNanos), true
		}
		res = IncidentDecisionResult{Parent: parent, Applied: true}
		for i, bid := range members {
			subject, err := canonicalBriefingID(tx, bid)
			if err != nil {
				return err
			}
			childRequest := fmt.Sprintf("%s-m-%d", in.RequestID, i)
			childHash := decisionHash(DecisionSubjectBriefing, subject, in.Kind, hashUntil, in.HasUntil, in.Text, "")
			childID := fmt.Sprintf("dec-%d-%d", now.UnixNano(), s.decisionSeq.Add(1))
			if _, err := tx.Exec(`INSERT INTO operator_decisions (decision_id, subject_kind, subject_id, kind,
				until, text, created_at, supersedes, request_id, content_hash, prior_label, prior_owner, generated_by)
				VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, NULL, NULL, ?)`,
				childID, DecisionSubjectBriefing, subject, in.Kind, untilVal, in.Text, now.UnixNano(),
				childRequest, childHash, parentID); err != nil {
				return err
			}
			child := Decision{DecisionID: childID, SubjectKind: DecisionSubjectBriefing, SubjectID: subject,
				Kind: in.Kind, Text: in.Text, CreatedAt: now, RequestID: childRequest}
			if in.Kind == DecisionSnooze {
				child.Until, child.HasUntil = fromNanos(untilNanos), true
			}
			res.Members = append(res.Members, child)
		}
		return nil
	})
	if err != nil {
		return IncidentDecisionResult{}, err
	}
	return res, nil
}

// generatedDecisions lists the member rows one incident decision
// generated, in snapshot order. Undo of the parent reads these to
// identify — never to rewrite — the fan-out.
func generatedDecisions(tx *sql.Tx, parentID string) ([]Decision, error) {
	rows, err := tx.Query(`SELECT `+decisionColumns+` FROM operator_decisions
		WHERE generated_by = ? ORDER BY rowid ASC LIMIT ?`, parentID, maxIncidentDecisionMembers+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		d, _, err := scanDecision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if len(out) > maxIncidentDecisionMembers {
		return nil, ErrIncidentFanoutLimit
	}
	return out, rows.Err()
}

// GeneratedDecisions lists the member decision IDs one incident
// decision generated. The undo response carries them so the operator
// can revisit each member judgment individually; undo itself never
// cascades. The read carries a backstop limit far above any plausible
// fan-out: membership itself is the real bound.
func (s *Store) GeneratedDecisions(ctx context.Context, parentID string) ([]string, error) {
	if parentID == "" {
		return nil, fmt.Errorf("empty decision id")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT decision_id FROM operator_decisions
		WHERE generated_by = ? ORDER BY rowid ASC LIMIT 1000`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// incidentReviewCTE selects the current review-lane record per
// incident: latest non-undone expected/not_expected/snooze, with
// lapsed snoozes and over-horizon audit excluded before the pick, so
// a stalled purge never extends a verdict past retention.
const incidentReviewCTE = `ieff AS (
	SELECT subject_id, kind, until FROM (
		SELECT d.subject_id, d.kind, d.until,
			ROW_NUMBER() OVER (PARTITION BY d.subject_id ORDER BY d.created_at DESC, d.rowid DESC) AS rn
		FROM operator_decisions d
		WHERE d.subject_kind = 'incident'
			AND d.kind IN ('expected','not_expected','snooze')
			AND NOT EXISTS (SELECT 1 FROM operator_decisions u WHERE u.kind = 'undo' AND u.supersedes = d.decision_id)
			AND (d.kind != 'snooze' OR d.until > ?)
			AND d.created_at >= ?
	) WHERE rn = 1
)`

// incidentEligibleCTE names the review-eligible incidents: open ones
// plus ones closed at or after the window start. The UNION ALL keeps
// both arms index-backed on incidents_status_closed: a single OR
// predicate would scan the table on every refresh.
const incidentEligibleCTE = `eligible AS (
	SELECT incident_id, kind, floor, updated_at FROM incidents WHERE status = 'open'
	UNION ALL
	SELECT incident_id, kind, floor, updated_at FROM incidents
		WHERE status = 'closed' AND closed_at IS NOT NULL AND closed_at >= ?
)`

// IncidentCandidate is the narrow incident projection for overview:
// open incidents plus incidents closed inside the window, with only
// the flagged marker carried — acknowledged and actively-snoozed rows
// are excluded in SQL before LIMIT, like briefing candidates.
type IncidentCandidate struct {
	IncidentID string
	Kind       string
	Floor      string
	UpdatedAt  time.Time
	Flagged    bool
}

// IncidentCandidates returns up to limit eligible incidents in
// display order: flagged first, then floor rank descending, newest
// activity first, stable ID tiebreaker. Eligibility covers open
// incidents and ones closed at or after since: a recently closed
// burst still needs review until an operator acknowledges it.
func (s *Store) IncidentCandidates(ctx context.Context, since time.Time, limit int) ([]IncidentCandidate, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	now := s.now()
	rows, err := s.db.QueryContext(ctx, `WITH `+incidentReviewCTE+`, `+incidentEligibleCTE+` SELECT
		e2.incident_id, e2.kind, e2.floor, e2.updated_at, e.kind
		FROM eligible e2 LEFT JOIN ieff e ON e.subject_id = e2.incident_id
		WHERE e.kind IS NULL OR e.kind = 'not_expected'
		ORDER BY CASE WHEN e.kind = 'not_expected' THEN 0 ELSE 1 END,
			CASE e2.floor WHEN 'critical' THEN 3 WHEN 'high' THEN 2 WHEN 'medium' THEN 1 ELSE 0 END DESC,
			e2.updated_at DESC, e2.incident_id ASC
		LIMIT ?`, nanos(now), s.decisionCutoff(now), nanos(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IncidentCandidate
	for rows.Next() {
		var c IncidentCandidate
		var updated int64
		var revKind sql.NullString
		if err := rows.Scan(&c.IncidentID, &c.Kind, &c.Floor, &updated, &revKind); err != nil {
			return nil, err
		}
		c.UpdatedAt = fromNanos(updated)
		c.Flagged = revKind.String == "not_expected"
		out = append(out, c)
	}
	return out, rows.Err()
}

// IncidentEffectiveCounts counts eligible incidents by floor for the
// verdict and separately counts flagged ones for the review floor,
// mirroring EffectivePriorityCounts: acknowledged and actively-snoozed
// rows never vote, and flagged rows never escalate on their own.
func (s *Store) IncidentEffectiveCounts(ctx context.Context, since, now time.Time) (map[string]int, int, error) {
	rows, err := s.db.QueryContext(ctx, `WITH `+incidentReviewCTE+`, `+incidentEligibleCTE+` SELECT
		e2.floor, e.kind, COUNT(*)
		FROM eligible e2 LEFT JOIN ieff e ON e.subject_id = e2.incident_id
		GROUP BY e2.floor, e.kind`, nanos(now), s.decisionCutoff(now), nanos(since))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	counts := map[string]int{}
	flagged := 0
	for rows.Next() {
		var floor string
		var revKind sql.NullString
		var n int
		if err := rows.Scan(&floor, &revKind, &n); err != nil {
			return nil, 0, err
		}
		switch revKind.String {
		case "not_expected":
			flagged += n
		case "expected", "snooze":
			// Explained or deferred: no verdict vote.
		default:
			counts[floor] += n
		}
	}
	return counts, flagged, rows.Err()
}

// maxIncidentCollapseMembers caps the member-ID read behind attention
// collapse: one bounded query per overview refresh, never a full
// membership scan.
const maxIncidentCollapseMembers = 2000

// IncidentMemberBriefings maps incident IDs to their member briefing
// IDs, oldest first, bounded. The overview pass uses it to collapse
// duplicate presentation: member briefings of an attention-state
// incident leave the display cut (flagged members stay visible).
func (s *Store) IncidentMemberBriefings(ctx context.Context, incidentIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(incidentIDs) == 0 {
		return out, nil
	}
	for start := 0; start < len(incidentIDs); start += maxIncidentPage {
		end := start + maxIncidentPage
		if end > len(incidentIDs) {
			end = len(incidentIDs)
		}
		placeholders := strings.Repeat("?,", end-start)
		placeholders = strings.TrimSuffix(placeholders, ",")
		args := make([]any, 0, end-start)
		for _, id := range incidentIDs[start:end] {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT incident_id, briefing_id FROM incident_members
			WHERE incident_id IN (`+placeholders+`)
			ORDER BY incident_id ASC, member_created_at ASC, briefing_id ASC LIMIT ?`,
			append(args, maxIncidentCollapseMembers)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var iid, bid string
			if err := rows.Scan(&iid, &bid); err != nil {
				rows.Close()
				return nil, err
			}
			out[iid] = append(out[iid], bid)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}
