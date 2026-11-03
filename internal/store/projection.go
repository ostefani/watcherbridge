package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"unicode/utf8"
)

// Reference classes for reported subjects (Designv9 §1.3). "legacy" is
// read-only history and is never assigned by projection.
const (
	SubjectReportedDevice = "reported_device"
	SubjectAddress        = "address"
	SubjectEvent          = "event"
	SubjectLegacy         = "legacy"
)

// projectionVersion marks rows written by this projector. A future
// projector revs the constant and reprocesses older versions.
const projectionVersion = 1

const (
	// maxReferenceRunes bounds opaque upstream references. Over-bound
	// references fall back to event-local context (Designv9 §9); they
	// are never truncated into a different-looking key.
	maxReferenceRunes = 256
	// maxNamespaceRunes bounds a reported stable-identity namespace.
	// Over-bound namespaces are dropped, never truncated into a
	// colliding key; the subject keeps its reference with no usable
	// namespace, which rule eligibility reads as unqualified.
	maxNamespaceRunes = 128
	// maxRevisionRunes bounds a reported stable-identity revision.
	// Over-bound revisions are dropped for the same reason: an
	// unverifiable revision keeps the subject usable but the rules
	// ineligible.
	maxRevisionRunes = 64
	// maxDisplayRunes caps stored reported strings. Lists carry explicit
	// truncation flags; missing values are preserved as null, never as
	// false or empty defaults.
	maxDisplayRunes = 256
	// maxAliasEntries caps retained alias/anchor lists per observation.
	maxAliasEntries = 200
	// maxPortsPerObservation caps retained port rows per observation.
	maxPortsPerObservation = 500
)

// subjectPort is one reported port on a host view.
type subjectPort struct {
	proto   string
	number  int
	state   string
	service string
}

// subjectView is one parsed host view with the identity fields the
// contract pins. Booleans are tri-state: nil means the report did not
// say, and only an explicit value is stored as true/false. Free-form
// evidence stays in the raw payload and the capped attributes snapshot.
// Scanner timestamps ride along as reported strings (never substituted
// with event time); missing stays empty, never zero-filled.
type subjectView struct {
	role               string // host, previous or current
	hostID             string
	identitySource     string
	identityConfidence string
	identityNamespace  string
	identityRevision   string
	ip                 string
	hostname           string
	mac                string
	randomized         *bool
	ports              []subjectPort
	vendor             string
	os                 string
	device             string
	osEvidence         string
	deviceEvidence     string
	source             string
	sourceName         string
	alive              *bool
	weak               *bool
	aliases            []string
	aliasesCut         bool
	anchorKeys         []string
	anchorsCut         bool
	fingerprint        string
	livenessCut        bool
	portsTotal         int
	portsCut           bool
	lastSeen           string
	lastStrongSeen     string
	lastWeakSeen       string
	liveness           []subjectLiveness
	confirmSource      string
	confirmProto       string
	confirmPort        int
}

// subjectLiveness is one reported liveness entry: who vouches, whether
// the observation was weak, and when the claim expires ("" = unstated).
// Weak is tri-state like the view booleans: nil means the report did
// not state strength, and only an explicit value stores true/false.
type subjectLiveness struct {
	source    string
	weak      *bool
	expiresAt string
}

// portJSON mirrors one reported port entry.
type portJSON struct {
	Number   int    `json:"number"`
	Protocol string `json:"protocol"`
	State    string `json:"state"`
	Service  string `json:"service"`
}

// livenessJSON mirrors one reported liveness entry. Weak is tri-state:
// nil means the report did not state strength.
type livenessJSON struct {
	Source    string `json:"source"`
	Weak      *bool  `json:"weak"`
	ExpiresAt string `json:"expires_at"`
}

// cappedPorts decodes at most maxPortsPerObservation entries while
// retaining the wire total for ports_total.
type cappedPorts struct {
	kept  []portJSON
	total int
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *cappedPorts) UnmarshalJSON(data []byte) error {
	kept, total, err := decodeCapped[portJSON](data, maxPortsPerObservation)
	if err != nil {
		return err
	}
	c.kept, c.total = kept, total
	return nil
}

// cappedLiveness decodes at most maxAliasEntries entries, flagging a cut.
type cappedLiveness struct {
	kept      []livenessJSON
	truncated bool
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *cappedLiveness) UnmarshalJSON(data []byte) error {
	kept, total, err := decodeCapped[livenessJSON](data, maxAliasEntries)
	if err != nil {
		return err
	}
	c.kept, c.truncated = kept, total > len(kept)
	return nil
}

// cappedStringList decodes at most maxAliasEntries entries, flagging
// a cut. Alias and anchor-key lists share the design bound.
type cappedStringList struct {
	kept      []string
	truncated bool
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *cappedStringList) UnmarshalJSON(data []byte) error {
	kept, total, err := decodeCapped[string](data, maxAliasEntries)
	if err != nil {
		return err
	}
	c.kept, c.truncated = kept, total > len(kept)
	return nil
}

// hostViewJSON mirrors the producer HostState fields projection reads.
// Only identity, addressing, enrichment and port fields are decoded;
// everything else stays in the raw payload. Collections decode capped
// so oversized arrays stay cheap; totals and cuts ride along.
type hostViewJSON struct {
	IP                 string `json:"ip"`
	Hostname           string `json:"hostname"`
	MAC                string `json:"mac"`
	RandomizedMAC      *bool  `json:"randomized_mac"`
	Vendor             string `json:"vendor"`
	OS                 string `json:"os"`
	Device             string `json:"device"`
	OSEvidence         string `json:"os_evidence"`
	DeviceEvidence     string `json:"device_evidence"`
	Source             string `json:"source"`
	SourceName         string `json:"source_name"`
	Alive              *bool  `json:"alive"`
	Weak               *bool  `json:"weak"`
	HostID             string `json:"host_id"`
	IdentityConfidence string `json:"identity_confidence"`
	IdentitySource     string `json:"identity_source"`
	// IdentityNamespace preserves a reported stable-identity
	// namespace verbatim (Designv9 §1.4 I1, when a producer emits
	// one). Preservation is not qualification: rule eligibility
	// still requires the receiver-bound registration contract.
	IdentityNamespace string `json:"identity_namespace"`
	// IdentityRevision preserves a reported stable-identity revision
	// verbatim (Designv9 §1.4 I1, when a producer emits one). Like
	// the namespace, preservation is not qualification.
	IdentityRevision   string           `json:"identity_revision"`
	IdentityAliases    cappedStringList `json:"identity_aliases"`
	IdentityAnchorKeys cappedStringList `json:"identity_anchor_keys"`
	Fingerprint        string           `json:"evidence_fingerprint"`
	LastSeenAt         string           `json:"last_seen_at"`
	LastStrongSeenAt   string           `json:"last_strong_seen_at"`
	LastWeakSeenAt     string           `json:"last_weak_seen_at"`
	Liveness           cappedLiveness   `json:"liveness"`
	ConfirmationSource string           `json:"confirmation_source"`
	ConfirmationProto  string           `json:"confirmation_proto"`
	ConfirmationPort   int              `json:"confirmation_port"`
	Ports              cappedPorts      `json:"ports"`
}

type eventViewsJSON struct {
	Host     *hostViewJSON `json:"host"`
	Previous *hostViewJSON `json:"previous"`
	Current  *hostViewJSON `json:"current"`
}

func toSubjectView(role string, h *hostViewJSON) subjectView {
	v := subjectView{role: role}
	if h == nil {
		return v
	}
	v.hostID = h.HostID
	v.identitySource = h.IdentitySource
	v.identityConfidence = h.IdentityConfidence
	v.identityNamespace = strings.TrimSpace(h.IdentityNamespace)
	if utf8.RuneCountInString(v.identityNamespace) > maxNamespaceRunes {
		v.identityNamespace = ""
	}
	v.identityRevision = strings.TrimSpace(h.IdentityRevision)
	if utf8.RuneCountInString(v.identityRevision) > maxRevisionRunes {
		v.identityRevision = ""
	}
	v.ip = strings.TrimSpace(h.IP)
	v.hostname = h.Hostname
	v.mac = h.MAC
	v.randomized = h.RandomizedMAC
	v.vendor = h.Vendor
	v.os = h.OS
	v.device = h.Device
	v.osEvidence = h.OSEvidence
	v.deviceEvidence = h.DeviceEvidence
	v.source = h.Source
	v.sourceName = h.SourceName
	v.alive = h.Alive
	v.weak = h.Weak
	// Collections arrive decode-capped; the copy only transfers the
	// kept entries, so the in-memory view stays bounded too. Counts
	// and cuts ride along for omission flags and ports_total.
	v.aliases, v.aliasesCut = h.IdentityAliases.kept, h.IdentityAliases.truncated
	v.anchorKeys, v.anchorsCut = h.IdentityAnchorKeys.kept, h.IdentityAnchorKeys.truncated
	v.fingerprint = h.Fingerprint
	v.lastSeen = h.LastSeenAt
	v.lastStrongSeen = h.LastStrongSeenAt
	v.lastWeakSeen = h.LastWeakSeenAt
	v.confirmSource = h.ConfirmationSource
	v.confirmProto = h.ConfirmationProto
	v.confirmPort = h.ConfirmationPort
	for _, l := range h.Liveness.kept {
		v.liveness = append(v.liveness, subjectLiveness{
			source:    l.Source,
			weak:      l.Weak,
			expiresAt: l.ExpiresAt,
		})
	}
	v.livenessCut = h.Liveness.truncated
	v.portsTotal = h.Ports.total
	v.portsCut = h.Ports.total > len(h.Ports.kept)
	for _, p := range h.Ports.kept {
		v.ports = append(v.ports, subjectPort{
			proto:   p.Protocol,
			number:  p.Number,
			state:   p.State,
			service: p.Service,
		})
	}
	return v
}

// groupKeyViewJSON carries only the fields grouping needs. The webhook
// decodes this synchronously on ingest; enrichment arrays and port
// lists never allocate there no matter how large the body is.
type groupKeyViewJSON struct {
	HostID         string `json:"host_id"`
	IdentitySource string `json:"identity_source"`
	IP             string `json:"ip"`
}

// groupKeyEventJSON mirrors the event envelope without collections.
type groupKeyEventJSON struct {
	Host     *groupKeyViewJSON `json:"host"`
	Previous *groupKeyViewJSON `json:"previous"`
	Current  *groupKeyViewJSON `json:"current"`
}

// parseGroupViews extracts role-selected identity fields without
// decoding collections: grouping keys derive from the reported
// reference only, so ingest never pays for enrichment parsing.
func parseGroupViews(payload []byte, eventType string) []subjectView {
	var env groupKeyEventJSON
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil
	}
	toView := func(role string, h *groupKeyViewJSON) subjectView {
		v := subjectView{role: role}
		if h != nil {
			v.hostID = h.HostID
			v.identitySource = h.IdentitySource
			v.ip = strings.TrimSpace(h.IP)
		}
		return v
	}
	if eventType == "host.changed" {
		var out []subjectView
		if env.Previous != nil {
			out = append(out, toView("previous", env.Previous))
		}
		if env.Current != nil {
			out = append(out, toView("current", env.Current))
		}
		return out
	}
	if env.Host == nil {
		return nil
	}
	return []subjectView{toView("host", env.Host)}
}

// parseEventViews extracts the host views an event type carries: host
// for appeared/disappeared, previous plus current for changed.
// Undecodable bodies yield no views; the event itself is still
// preserved and its projection marks done with nothing to attribute.
func parseEventViews(payload []byte, eventType string) []subjectView {
	var env eventViewsJSON
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil
	}
	if eventType == "host.changed" {
		var out []subjectView
		if env.Previous != nil {
			out = append(out, toSubjectView("previous", env.Previous))
		}
		if env.Current != nil {
			out = append(out, toSubjectView("current", env.Current))
		}
		return out
	}
	if env.Host == nil {
		return nil
	}
	return []subjectView{toSubjectView("host", env.Host)}
}

// qualifiedReference maps one parsed view to its reference class under
// Designv9 §1.3 and the contract qualification table. Only references
// the scanner actually reported qualify: a MAC reference needs the
// reported mac source with a host_id, an address needs the reported ip
// source with a consistent ip-derived host_id. Everything else —
// provided without a documented namespace, unknown or missing source,
// a bare IP attribute, an oversized or contradictory reference — is
// event-local. The bridge never parses an attribute into a reference
// it was not given, and never substitutes hostname/MAC/IP fallbacks.
func qualifiedReference(view subjectView) (class, namespace, reference string) {
	if view.hostID == "" || utf8.RuneCountInString(view.hostID) > maxReferenceRunes {
		return SubjectEvent, "", ""
	}
	switch view.identitySource {
	case "mac":
		// Scanner-reported MAC reference: exact ID equality within
		// scope. This means only "the scanner used the same MAC
		// reference", not spoof resistance or proof of physical
		// identity across MAC changes. A reported namespace rides
		// along verbatim; it qualifies nothing by itself.
		return SubjectReportedDevice, view.identityNamespace, view.hostID
	case "ip":
		// A scanner-reported ip-derived reference, consistent with
		// the reported address when one is present.
		rest, ok := strings.CutPrefix(view.hostID, "ip:")
		if !ok {
			return SubjectEvent, "", ""
		}
		ref, err := netip.ParseAddr(strings.TrimSpace(rest))
		if err != nil {
			return SubjectEvent, "", ""
		}
		if view.ip != "" {
			attr, err := netip.ParseAddr(view.ip)
			if err != nil || attr != ref {
				// Contradictory metadata fails to event-local
				// context; the IP attribute never overrides the
				// reported reference.
				return SubjectEvent, "", ""
			}
		}
		return SubjectAddress, "", ref.String()
	default:
		return SubjectEvent, "", ""
	}
}

// GroupKeyForEvent derives the grouping key for one accepted event
// (Designv9 §1.5): eligible references group by exact scoped upstream
// reference, addresses stay explicitly address-scoped, and unresolved
// input groups by event ID. The second return reports event-local
// grouping so the receiver can log it; it is not an error.
func GroupKeyForEvent(sourceScope, eventType, eventID string, payload []byte) (string, bool) {
	views := parseGroupViews(payload, eventType)
	var v *subjectView
	if eventType == "host.changed" {
		for i := range views {
			if views[i].role == "current" {
				v = &views[i]
				break
			}
		}
		if v == nil {
			for i := range views {
				if views[i].role == "previous" {
					v = &views[i]
					break
				}
			}
		}
	} else {
		for i := range views {
			if views[i].role == "host" {
				v = &views[i]
				break
			}
		}
	}
	if v == nil {
		return "evt:" + eventID, true
	}
	class, ns, ref := qualifiedReference(*v)
	switch class {
	case SubjectReportedDevice:
		return "sub:" + sourceScope + ":" + ns + ":" + ref, false
	case SubjectAddress:
		return "addr:" + sourceScope + ":" + ref, false
	default:
		return "evt:" + eventID, true
	}
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}

// decodeCapped decodes a JSON array keeping at most max entries and
// reporting the wire total. Entries past the cap are consumed
// token-wise without decoding, so a hostile oversized collection stays
// cheap: only kept entries allocate. A null array decodes empty; a
// non-array is invalid input and errors.
func decodeCapped[T any](data []byte, max int) (kept []T, total int, err error) {
	if string(bytes.TrimSpace(data)) == "null" {
		return nil, 0, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, 0, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, 0, fmt.Errorf("expected array")
	}
	for dec.More() {
		if len(kept) < max {
			var v T
			if err := dec.Decode(&v); err != nil {
				return nil, 0, err
			}
			kept = append(kept, v)
		} else if err := skipJSONValue(dec); err != nil {
			return nil, 0, err
		}
		total++
	}
	if _, err := dec.Token(); err != nil {
		return nil, 0, err
	}
	return kept, total, nil
}

// skipJSONValue consumes one JSON value positioned at the decoder
// without decoding it: scalars take one token, composites are walked
// by delimiter depth.
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || (d != '[' && d != '{') {
		return nil
	}
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '[', '{':
				depth++
			case ']', '}':
				depth--
			}
		}
	}
	return nil
}

// capRunesFlag caps like capRunes and reports whether content changed.
func capRunesFlag(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	return string([]rune(s)[:n]), true
}

func cappedStrings(in []string, maxRunes int) ([]string, bool) {
	out := make([]string, 0, len(in))
	var truncated bool
	for _, s := range in {
		c, cut := capRunesFlag(s, maxRunes)
		out = append(out, c)
		truncated = truncated || cut
	}
	return out, truncated
}

func newProfileID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "prof-" + hex.EncodeToString(b[:]), nil
}

// BindSourceScope enforces the single-source database binding: refuse
// to serve a database that already holds another deployment's scoped
// receipts instead of mixing their subjects. Pre-scope rows (”) stay
// adoptable and are ignored by the check. Call once at startup before
// serving, sweeping or briefing.
func (s *Store) BindSourceScope(ctx context.Context, scope string) error {
	if scope == "" {
		return fmt.Errorf("source scope must not be empty")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT source_scope FROM raw_events WHERE source_scope <> ''
		UNION SELECT DISTINCT source_scope FROM report_sources WHERE source_scope <> ''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var found string
		if err := rows.Scan(&found); err != nil {
			return err
		}
		if found != scope {
			return fmt.Errorf("database holds source scope %q, refusing scope %q: use a fresh database for a new deployment", found, scope)
		}
	}
	return rows.Err()
}

// ProjectReported attributes retained event views to reported subjects
// through the serialized writer, bounded to limit events per call. It
// drains the indexed pending queue in receipt order — one small read
// no matter how much retention is kept — then loads and projects one
// payload at a time, so peak memory stays near a single event instead
// of the whole batch. Each event commits in its own transaction with
// its progress marker, so a crash or restart resumes from the queue
// without rework and without double counting. defaultScope adopts
// pre-scope rows; rows that already carry a scope project under it.
func (s *Store) ProjectReported(ctx context.Context, defaultScope string, limit int) (int, error) {
	if limit <= 0 {
		limit = 64
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id FROM projection_pending ORDER BY received_at, event_id LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	done := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		var ev struct {
			eventID    string
			eventType  string
			occurredAt int64
			receivedAt int64
			scope      string
			payload    []byte
		}
		err := s.db.QueryRowContext(ctx, `SELECT event_id, event_type, occurred_at, received_at, source_scope, payload
			FROM raw_events WHERE event_id = ?`, id).Scan(
			&ev.eventID, &ev.eventType, &ev.occurredAt, &ev.receivedAt, &ev.scope, &ev.payload)
		if err == sql.ErrNoRows {
			// Retention purged the event between queueing and
			// processing; drop the orphan queue row and move on.
			if derr := s.write(ctx, func(tx *sql.Tx) error {
				_, derr := tx.Exec(`DELETE FROM projection_pending WHERE event_id = ?`, id)
				return derr
			}); derr != nil {
				return done, derr
			}
			continue
		}
		if err != nil {
			return done, err
		}
		scope := ev.scope
		if scope == "" {
			scope = defaultScope
		}
		// Decode outside the writer: parsing never holds the
		// serialization lock, so one large event cannot delay
		// unrelated persistence. The idempotency recheck stays
		// inside the transaction below.
		views := parseEventViews(ev.payload, ev.eventType)
		if err := s.write(ctx, func(tx *sql.Tx) error {
			return projectEvent(tx, scope, ev.eventID, ev.occurredAt, ev.receivedAt, views)
		}); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

// projectEvent attributes one retained event inside its transaction.
// Idempotency is rechecked here, not assumed from the queue: two
// callers can select the same pending event before either commits,
// and only the recheck keeps the second replay from mutating state.
// Views arrive decoded: parsing happens outside the writer so it never
// holds the serialization lock.
func projectEvent(tx *sql.Tx, sourceScope, eventID string, occurredAt, receivedAt int64, views []subjectView) error {
	var already int64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM projection_progress WHERE event_id = ? AND state = 'done'`, eventID).Scan(&already); err != nil {
		return err
	}
	if already > 0 {
		_, err := tx.Exec(`DELETE FROM projection_pending WHERE event_id = ?`, eventID)
		return err
	}
	// Adopt pre-scope rows once, in the same transaction as their
	// projection, so ownership is explicit from here on.
	if _, err := tx.Exec(`UPDATE raw_events SET source_scope = ? WHERE event_id = ? AND source_scope = ''`, sourceScope, eventID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO report_sources (source_scope) VALUES (?)`, sourceScope); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE report_sources SET last_report_at = MAX(COALESCE(last_report_at, 0), ?) WHERE source_scope = ?`, receivedAt, sourceScope); err != nil {
		return err
	}
	var groupID sql.NullString
	if err := tx.QueryRow(`SELECT group_id FROM group_events WHERE event_id = ?`, eventID).Scan(&groupID); err != nil && err != sql.ErrNoRows {
		return err
	}
	for _, view := range views {
		class, namespace, ref := qualifiedReference(view)
		var upstreamRef string
		if class == SubjectEvent {
			// Event-local subject keyed by the receipt it came in
			// on: one subject per unresolved event, never shared
			// across deliveries.
			upstreamRef = eventID
		} else {
			upstreamRef = ref
		}
		profileID, err := ensureSubject(tx, sourceScope, class, namespace, upstreamRef, view, receivedAt)
		if err != nil {
			return err
		}
		observationID, err := ensureObservation(tx, eventID, view.role, profileID, occurredAt, receivedAt, view)
		if err != nil {
			return err
		}
		if err := ensurePorts(tx, observationID, view.ports); err != nil {
			return err
		}
		if groupID.Valid {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO group_subjects (group_id, profile_id, role) VALUES (?, ?, ?)`,
				groupID.String, profileID, view.role); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO projection_progress (event_id, projection_version, state) VALUES (?, ?, 'done')`,
		eventID, projectionVersion); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM projection_pending WHERE event_id = ?`, eventID)
	return err
}

// ensureSubject resolves the subject for one qualified reference. The
// UNIQUE key on the reference columns (never display text) is the only
// association mechanism; re-observation finds the same profile after
// restarts. Current-state columns advance only for observations at or
// past the recorded high-water mark, so a replayed older observation
// can never overwrite newer provenance.
func ensureSubject(tx *sql.Tx, sourceScope, class, namespace, upstreamRef string, view subjectView, receivedAt int64) (string, error) {
	id, err := newProfileID()
	if err != nil {
		return "", err
	}
	// Profile columns are bounded like the snapshot: unbounded
	// upstream strings never widen a stored current-state column.
	if _, err := tx.Exec(`INSERT OR IGNORE INTO reported_subjects
		(profile_id, source_scope, reference_class, identity_namespace, identity_revision, upstream_reference,
		 reported_identity_source, reported_confidence, first_report_at, last_report_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sourceScope, class, namespace, view.identityRevision, upstreamRef,
		capRunes(view.identitySource, maxDisplayRunes),
		capRunes(view.identityConfidence, maxDisplayRunes),
		receivedAt, receivedAt); err != nil {
		return "", err
	}
	var profileID string
	if err := tx.QueryRow(`SELECT profile_id FROM reported_subjects
		WHERE source_scope = ? AND reference_class = ? AND identity_namespace = ? AND upstream_reference = ?`,
		sourceScope, class, namespace, upstreamRef).Scan(&profileID); err != nil {
		return "", err
	}
	// CASE branches read the pre-update mark (assigned last): ties go
	// to the current view, which is processed after previous views. The
	// revision rides the same high-water mark: a replayed older report
	// can never roll a reference back to a previous epoch.
	if _, err := tx.Exec(`UPDATE reported_subjects
		SET reported_identity_source = CASE WHEN ? >= last_report_at THEN ? ELSE reported_identity_source END,
		    reported_confidence = CASE WHEN ? >= last_report_at THEN ? ELSE reported_confidence END,
		    identity_revision = CASE WHEN ? >= last_report_at THEN ? ELSE identity_revision END,
		    last_report_at = MAX(last_report_at, ?)
		WHERE profile_id = ?`,
		receivedAt, capRunes(view.identitySource, maxDisplayRunes),
		receivedAt, capRunes(view.identityConfidence, maxDisplayRunes),
		receivedAt, view.identityRevision,
		receivedAt, profileID); err != nil {
		return "", err
	}
	return profileID, nil
}

// ensureObservation records one event view against its subject. The
// UNIQUE(event_id, view_role) key makes replays idempotent; the
// attributes snapshot is a bounded read model with explicit omission
// flags, while full fidelity stays in the raw payload.
func ensureObservation(tx *sql.Tx, eventID, role, profileID string, reportedAt, receivedAt int64, view subjectView) (int64, error) {
	attrs, err := viewAttributes(view)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO reported_observations
		(event_id, view_role, profile_id, reported_at, received_at, attributes_json)
		VALUES (?, ?, ?, ?, ?, ?)`,
		eventID, role, profileID, reportedAt, receivedAt, attrs); err != nil {
		return 0, err
	}
	var observationID int64
	if err := tx.QueryRow(`SELECT observation_id FROM reported_observations WHERE event_id = ? AND view_role = ?`,
		eventID, role).Scan(&observationID); err != nil {
		return 0, err
	}
	return observationID, nil
}

// capPort bounds one port's display strings, reporting cuts so the
// snapshot can mark shortened evidence as incomplete. ensurePorts
// stores through this same helper, so the flag and the row agree.
func capPort(p subjectPort) (proto, state, service string, cut bool) {
	var c1, c2, c3 bool
	proto, c1 = capRunesFlag(p.proto, maxDisplayRunes)
	state, c2 = capRunesFlag(p.state, maxDisplayRunes)
	service, c3 = capRunesFlag(p.service, maxDisplayRunes)
	return proto, state, service, c1 || c2 || c3
}

// viewAttributes snapshots the reported enrichment a view carried. ID,
// address, hostname, MAC, tri-state presence and the scanner's own
// timestamps survive; display strings are capped, alias/liveness lists
// are capped with omission flags, and attributes_truncated reports any
// scalar, list or port-field capping anywhere in the snapshot.
// reported_at keeps the enclosing event time; scanner observation
// times ride as attributes, never substituted. Ports live in
// reported_ports; their count and field truncation flags ride along.
func viewAttributes(view subjectView) (string, error) {
	var truncated bool
	cap := func(s string) string {
		c, cut := capRunesFlag(s, maxDisplayRunes)
		truncated = truncated || cut
		return c
	}
	aliases, aliasesCut := cappedStrings(view.aliases, maxDisplayRunes)
	anchors, anchorsCut := cappedStrings(view.anchorKeys, maxDisplayRunes)
	liveness, livenessCut := capLiveness(view.liveness)
	var portsCut, portFieldsCut bool
	for _, p := range view.ports {
		_, _, _, cut := capPort(p)
		portFieldsCut = portFieldsCut || cut
	}
	portsCut = view.portsCut || portFieldsCut
	truncated = truncated || view.aliasesCut || aliasesCut ||
		view.anchorsCut || anchorsCut ||
		view.livenessCut || livenessCut || portsCut
	attrs := map[string]any{
		"host_id":               cap(view.hostID),
		"identity_source":       cap(view.identitySource),
		"identity_confidence":   cap(view.identityConfidence),
		"ip":                    cap(view.ip),
		"hostname":              cap(view.hostname),
		"mac":                   cap(view.mac),
		"randomized":            view.randomized,
		"vendor":                cap(view.vendor),
		"os":                    cap(view.os),
		"device":                cap(view.device),
		"os_evidence":           cap(view.osEvidence),
		"device_evidence":       cap(view.deviceEvidence),
		"source":                cap(view.source),
		"source_name":           cap(view.sourceName),
		"alive":                 view.alive,
		"weak":                  view.weak,
		"aliases":               aliases,
		"aliases_truncated":     view.aliasesCut || aliasesCut,
		"anchor_keys":           anchors,
		"anchor_keys_truncated": view.anchorsCut || anchorsCut,
		"ports_total":           view.portsTotal,
		"ports_truncated":       portsCut,
		"evidence_fingerprint":  cap(view.fingerprint),
		"last_seen_at":          cap(view.lastSeen),
		"last_strong_seen_at":   cap(view.lastStrongSeen),
		"last_weak_seen_at":     cap(view.lastWeakSeen),
		"liveness":              liveness,
		"liveness_truncated":    view.livenessCut || livenessCut,
		"confirmation_source":   cap(view.confirmSource),
		"confirmation_proto":    cap(view.confirmProto),
		"confirmation_port":     view.confirmPort,
		"attributes_truncated":  truncated,
	}
	raw, err := json.Marshal(attrs)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// capLiveness bounds one observation's liveness entries (count and
// per-entry strings) and reports any capping. Entries are small maps
// so the snapshot stays readable; the raw payload keeps full fidelity.
func capLiveness(in []subjectLiveness) ([]map[string]any, bool) {
	truncated := len(in) > maxAliasEntries
	if truncated {
		in = in[:maxAliasEntries]
	}
	out := make([]map[string]any, 0, len(in))
	for _, l := range in {
		source, cutSource := capRunesFlag(l.source, maxDisplayRunes)
		expires, cutExpires := capRunesFlag(l.expiresAt, maxDisplayRunes)
		truncated = truncated || cutSource || cutExpires
		out = append(out, map[string]any{
			"source":     source,
			"weak":       l.weak,
			"expires_at": expires,
		})
	}
	return out, truncated
}

// ensurePorts records the reported port states for one observation.
// The view copy is already count-bounded, so one chatty view cannot
// flood the table; field strings go through capPort, the same helper
// the snapshot flag uses. Replays hit the composite primary key and
// change nothing.
func ensurePorts(tx *sql.Tx, observationID int64, ports []subjectPort) error {
	for _, p := range ports {
		proto, state, service, _ := capPort(p)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO reported_ports
			(observation_id, proto, port, reported_state, reported_service)
			VALUES (?, ?, ?, ?, ?)`,
			observationID, proto, p.number, state, service); err != nil {
			return err
		}
	}
	return nil
}
