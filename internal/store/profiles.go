package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Reported-subject read API (Designv9 §§3.3, 4.1). These are read-only
// projections over retained context: exact key lookups and bounded
// ordered scans, never hostname/IP ownership joins across history.
// Every list carries a deterministic nanosecond tuple cursor so equal
// timestamps paginate without skips or repeats.

// Device list bounds: the compatibility envelope pages 50 by default
// and refuses more than 200 per page rather than silently truncating.
const (
	defaultDevicePage = 50
	maxDevicePage     = 200
	// History pages are smaller: each item carries its full bounded
	// attributes snapshot, so 50 is both default and ceiling.
	defaultHistoryPage = 50
	maxHistoryPage     = 50
	// maxProfilePorts bounds the latest-evidence port list; the
	// snapshot's ports_total/ports_truncated say what was cut.
	maxProfilePorts = 500
)

// ReportedSubject is one read-model row: an opaque local surrogate for
// an exact upstream reference, with its provenance and association
// counts. Counts come from explicit link rows, never from matching
// current attributes across history.
type ReportedSubject struct {
	ProfileID              string
	SourceScope            string
	ReferenceClass         string
	IdentityNamespace      string
	UpstreamReference      string
	ReportedIdentitySource string
	ReportedConfidence     string
	AssociationStatus      string
	OperatorLabel          *string
	FirstReportAt          time.Time
	LastReportAt           time.Time
	Groups                 int64
	Briefings              int64
}

// ReportedPort is one retained port state for an observation.
type ReportedPort struct {
	Proto   string
	Port    int
	State   string
	Service string
}

// SubjectObservation is one retained event view: the bounded attributes
// snapshot plus, for the latest-evidence section only, the retained
// port list. History items carry the snapshot (which already reports
// ports_total/ports_truncated) without repeating every port row.
type SubjectObservation struct {
	ObservationID  int64
	EventID        string
	ViewRole       string
	ProfileID      string
	ReportedAt     time.Time
	ReceivedAt     time.Time
	Attributes     map[string]any
	Ports          []ReportedPort
	PortsTotal     int64
	PortsTruncated bool
}

// SubjectProfile bundles a subject with its latest evidence and one
// history page. Latest is nil when no observation is retained.
type SubjectProfile struct {
	Subject     ReportedSubject
	Latest      *SubjectObservation
	History     []SubjectObservation
	HistoryNext string
}

const subjectColumns = `profile_id, source_scope, reference_class, identity_namespace, upstream_reference,
	reported_identity_source, reported_confidence, association_status, operator_label,
	first_report_at, last_report_at,
	(SELECT COUNT(*) FROM group_subjects WHERE group_subjects.profile_id = reported_subjects.profile_id),
	(SELECT COUNT(*) FROM briefing_subjects WHERE briefing_subjects.profile_id = reported_subjects.profile_id)`

func scanSubject(row interface {
	Scan(dest ...any) error
}, s *ReportedSubject) error {
	var label sql.NullString
	var first, last int64
	if err := row.Scan(&s.ProfileID, &s.SourceScope, &s.ReferenceClass, &s.IdentityNamespace,
		&s.UpstreamReference, &s.ReportedIdentitySource, &s.ReportedConfidence, &s.AssociationStatus,
		&label, &first, &last, &s.Groups, &s.Briefings); err != nil {
		return err
	}
	if label.Valid {
		s.OperatorLabel = &label.String
	}
	s.FirstReportAt = fromNanos(first)
	s.LastReportAt = fromNanos(last)
	return nil
}

// ReportedSubjectsPage lists subjects oldest-first by the (last_report_at,
// profile_id) tuple cursor. afterNanos/afterProfileID select rows strictly
// after the tuple; both zero starts from the oldest. limit clamps to the
// compatibility bounds. nextAfter is "" when the page is short, i.e. the
// listing is exhausted. Unknown cursors simply start past retained rows
// and return an empty page, never an error.
func (s *Store) ReportedSubjectsPage(ctx context.Context, afterNanos int64, afterProfileID string, limit int) ([]ReportedSubject, string, error) {
	if afterNanos < 0 {
		return nil, "", fmt.Errorf("negative device cursor")
	}
	if limit <= 0 {
		limit = defaultDevicePage
	}
	if limit > maxDevicePage {
		limit = maxDevicePage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+subjectColumns+` FROM reported_subjects
		WHERE (last_report_at, profile_id) > (?, ?)
		ORDER BY last_report_at, profile_id LIMIT ?`, afterNanos, afterProfileID, limit)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []ReportedSubject
	for rows.Next() {
		var sub ReportedSubject
		if err := scanSubject(rows, &sub); err != nil {
			return nil, "", err
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	nextAfter := ""
	if len(out) == limit {
		last := out[len(out)-1]
		nextAfter = encodeTupleCursor(nanos(last.LastReportAt), last.ProfileID)
	}
	return out, nextAfter, nil
}

// ReportedProfile reads one subject by its opaque profile ID. Unknown IDs
// return sql.ErrNoRows so the viewer can answer 404; empty IDs are caller
// errors. No attribute matching is involved: the ID is the key.
func (s *Store) ReportedProfile(ctx context.Context, profileID string) (ReportedSubject, error) {
	var sub ReportedSubject
	if profileID == "" {
		return sub, fmt.Errorf("empty profile id")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+subjectColumns+` FROM reported_subjects WHERE profile_id = ?`, profileID)
	if err := scanSubject(row, &sub); err != nil {
		return ReportedSubject{}, err
	}
	return sub, nil
}

// LatestSubjectObservation returns the most recently received retained
// observation with its port list (bounded, oldest-port-first is wrong
// here: ports order by number for a stable evidence view). ok is false
// when no observation is retained; the subject row itself survives that.
func (s *Store) LatestSubjectObservation(ctx context.Context, profileID string) (SubjectObservation, bool, error) {
	var obs SubjectObservation
	err := s.db.QueryRowContext(ctx, `SELECT observation_id, event_id, view_role, profile_id, reported_at, received_at, attributes_json
		FROM reported_observations WHERE profile_id = ? ORDER BY received_at DESC, observation_id DESC LIMIT 1`, profileID).Scan(
		&obs.ObservationID, &obs.EventID, &obs.ViewRole, &obs.ProfileID, (*nanosTime)(&obs.ReportedAt), (*nanosTime)(&obs.ReceivedAt), (*jsonText)(&obs.Attributes))
	if err == sql.ErrNoRows {
		return SubjectObservation{}, false, nil
	}
	if err != nil {
		return SubjectObservation{}, false, err
	}
	if err := s.fillObservationPorts(ctx, &obs); err != nil {
		return SubjectObservation{}, false, err
	}
	return obs, true, nil
}

// maxHistoryPageBytes caps one history page's cumulative snapshot
// bytes. Fifty bounded snapshots stay small in practice, but aliases
// and liveness collections are attacker-influenced; the page stops at
// the last item that fits and continues from there instead of
// materializing an unbounded response (Designv9 §4.6 heap budget).
const maxHistoryPageBytes = 1 << 20

// SubjectHistoryPage pages one subject's retained observations oldest-first
// by the (reported_at, observation_id) tuple. Items carry the bounded
// attributes snapshot, which already reports port truncation; per-item
// port lists stay in the latest-evidence section so a 50-item page stays
// small. The snapshot's ports_total/ports_truncated populate the item
// summary fields, so history never reports zero ports alongside a
// snapshot that captured hundreds. limit clamps to the history ceiling;
// the cumulative byte budget can end the page early with continuation
// at the last included item.
func (s *Store) SubjectHistoryPage(ctx context.Context, profileID string, afterReportedAt int64, afterObservationID int64, limit int) ([]SubjectObservation, string, error) {
	if afterReportedAt < 0 || afterObservationID < 0 {
		return nil, "", fmt.Errorf("negative history cursor")
	}
	if limit <= 0 {
		limit = defaultHistoryPage
	}
	if limit > maxHistoryPage {
		limit = maxHistoryPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT observation_id, event_id, view_role, profile_id, reported_at, received_at, attributes_json
		FROM reported_observations WHERE profile_id = ? AND (reported_at, observation_id) > (?, ?)
		ORDER BY reported_at, observation_id LIMIT ?`, profileID, afterReportedAt, afterObservationID, limit)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []SubjectObservation
	var pageBytes int64
	cutShort := false
	for rows.Next() {
		var obs SubjectObservation
		var attrsRaw string
		if err := rows.Scan(&obs.ObservationID, &obs.EventID, &obs.ViewRole, &obs.ProfileID,
			(*nanosTime)(&obs.ReportedAt), (*nanosTime)(&obs.ReceivedAt), &attrsRaw); err != nil {
			return nil, "", err
		}
		if pageBytes+int64(len(attrsRaw)) > maxHistoryPageBytes && len(out) > 0 {
			cutShort = true
			break
		}
		pageBytes += int64(len(attrsRaw))
		attrs := map[string]any{}
		if err := json.Unmarshal([]byte(attrsRaw), &attrs); err != nil {
			return nil, "", err
		}
		obs.Attributes = attrs
		obs.PortsTotal, obs.PortsTruncated = snapshotPorts(attrs, 0, false)
		out = append(out, obs)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	nextAfter := ""
	// A byte-budget cut still has retained rows behind it, so it
	// continues at the last included item exactly like a full page.
	if len(out) == limit || (cutShort && len(out) > 0) {
		last := out[len(out)-1]
		nextAfter = encodeTupleCursor(nanos(last.ReportedAt), fmt.Sprintf("%d", last.ObservationID))
	}
	return out, nextAfter, nil
}

// fillObservationPorts loads the retained port list for one observation,
// ordered by port number for a stable evidence view and capped with an
// explicit truncation flag. Totals come from the observation's own
// snapshot (the wire total at capture, which survives purge); the list
// holds at most maxProfilePorts retained rows.
func (s *Store) fillObservationPorts(ctx context.Context, obs *SubjectObservation) error {
	rows, err := s.db.QueryContext(ctx, `SELECT proto, port, reported_state, reported_service
		FROM reported_ports WHERE observation_id = ? ORDER BY port, proto LIMIT ?`,
		obs.ObservationID, maxProfilePorts+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	listCut := false
	for rows.Next() {
		var p ReportedPort
		if err := rows.Scan(&p.Proto, &p.Port, &p.State, &p.Service); err != nil {
			return err
		}
		if int64(len(obs.Ports)) < maxProfilePorts {
			obs.Ports = append(obs.Ports, p)
		} else {
			listCut = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	obs.PortsTotal, obs.PortsTruncated = snapshotPorts(obs.Attributes, int64(len(obs.Ports)), listCut)
	return nil
}

// snapshotPorts reads the wire total and truncation flag the projector
// stored in the attributes snapshot. Missing keys (rows predating the
// snapshot fields) fall back to the retained list.
func snapshotPorts(attrs map[string]any, retained int64, listCut bool) (int64, bool) {
	total, ok := attrs["ports_total"].(float64)
	if !ok {
		return retained, listCut
	}
	truncated, _ := attrs["ports_truncated"].(bool)
	return int64(total), truncated || listCut
}

// encodeTupleCursor renders a deterministic nanosecond tuple cursor.
// Profile IDs are prof-<hex> and observation IDs are decimal rowids, so
// neither contains the separator; parsing splits on the first colon and
// validates both halves.
func encodeTupleCursor(nanosValue int64, id string) string {
	return fmt.Sprintf("%d:%s", nanosValue, id)
}

// nanosTime scans an INTEGER nanos timestamp into a time.Time.
type nanosTime time.Time

func (n *nanosTime) Scan(v any) error {
	var nanosValue int64
	switch t := v.(type) {
	case int64:
		nanosValue = t
	case int:
		nanosValue = int64(t)
	default:
		return fmt.Errorf("nanos timestamp is %T, not integer", v)
	}
	*n = nanosTime(fromNanos(nanosValue))
	return nil
}

// jsonText unmarshals a TEXT JSON column into a map. The columns are
// bounded at write time (capRunes/cappedStrings/capPort), so serving
// the decoded map adds no new allocation risk beyond one page.
type jsonText map[string]any

func (m *jsonText) Scan(v any) error {
	s, ok := v.(string)
	if !ok {
		if b, ok := v.([]byte); ok {
			s = string(b)
		} else {
			return fmt.Errorf("json column is %T, not text", v)
		}
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return err
	}
	*m = out
	return nil
}
