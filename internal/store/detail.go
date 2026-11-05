package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Explicit read bounds for Step 24 detail serving. Profile links ride
// the persisted briefing_subjects/group_subjects rows only — never a
// hostname, IP or MAC re-match — and one event can contribute several
// links (host plus previous/current views), so subject reads carry
// their own cap and truncation flag instead of inheriting the event
// count. Older groups created under different limits are still
// bounded, because every cap below applies at read time.
const (
	// maxBriefingSubjects caps one briefing's profile links; the
	// query reads one row past the cap to report truncation.
	maxBriefingSubjects = 64
	// maxReferencedEvents caps one group's event index; groups hold
	// at most the configured event cap, so the read cap sits above
	// it and only fires for older groups from wider limits.
	maxReferencedEvents = 129
	// maxReferenceObservations caps the reported-evidence section;
	// entries beyond the cap are dropped with the truncated flag.
	maxReferenceObservations = 64
	// maxReferenceObsBytes caps the cumulative attributes snapshot
	// bytes on that section, mirroring the history page budget.
	maxReferenceObsBytes = 1 << 20
)

// BriefingSubject is one persisted briefing-to-profile link with the
// role the projection assigned (host, previous or current).
type BriefingSubject struct {
	ProfileID string
	Role      string
}

// GroupSubjects returns the profile links projected for one group in
// deterministic profile, role order. The Step 26 tool registry admits
// group-linked subjects (plus subjects granted by find_similar within
// the same run): a valid issued token is never authorization, so tools
// reject any subject outside the registry.
func (s *Store) GroupSubjects(ctx context.Context, groupID string) ([]BriefingSubject, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT profile_id, role FROM group_subjects
		WHERE group_id = ? ORDER BY profile_id ASC, role ASC LIMIT ?`, groupID, maxBriefingSubjects+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []BriefingSubject
	for rows.Next() {
		var link BriefingSubject
		if err := rows.Scan(&link.ProfileID, &link.Role); err != nil {
			return nil, false, err
		}
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > maxBriefingSubjects {
		return out[:maxBriefingSubjects], true, nil
	}
	return out, false, nil
}

// BriefingSubjects returns the persisted profile links for one
// briefing in deterministic profile order. truncated reports that
// links beyond maxBriefingSubjects exist.
func (s *Store) BriefingSubjects(ctx context.Context, briefingID string) ([]BriefingSubject, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT profile_id, role FROM briefing_subjects
		WHERE briefing_id = ? ORDER BY profile_id ASC LIMIT ?`, briefingID, maxBriefingSubjects+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []BriefingSubject
	for rows.Next() {
		var link BriefingSubject
		if err := rows.Scan(&link.ProfileID, &link.Role); err != nil {
			return nil, false, err
		}
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > maxBriefingSubjects {
		return out[:maxBriefingSubjects], true, nil
	}
	return out, false, nil
}

// ReferencedEvent pairs one group event ID with its recorded type: an
// evidence index, not the evidence itself. OccurredAt is the scanner's
// reported event time, ReceivedAt is local receipt; Target is the
// reported scan target, never a derived subnet. Target longer than
// maxDisplayRunes is capped with TargetTruncated.
type ReferencedEvent struct {
	EventID         string
	EventType       string
	OccurredAt      time.Time
	ReceivedAt      time.Time
	Target          string
	TargetTruncated bool
}

// BriefingReferencedEvents returns the group's event index in
// deterministic event order. truncated reports entries beyond
// maxReferencedEvents, which only fires for groups retained from
// older, wider limits.
func (s *Store) BriefingReferencedEvents(ctx context.Context, groupID string) ([]ReferencedEvent, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ge.event_id, re.event_type, re.occurred_at, re.received_at, COALESCE(re.target,'')
		FROM group_events ge JOIN raw_events re ON re.event_id = ge.event_id
		WHERE ge.group_id = ? ORDER BY ge.event_id ASC LIMIT ?`, groupID, maxReferencedEvents+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []ReferencedEvent
	for rows.Next() {
		var ref ReferencedEvent
		var occurred, received int64
		var target string
		if err := rows.Scan(&ref.EventID, &ref.EventType, &occurred, &received, &target); err != nil {
			return nil, false, err
		}
		ref.OccurredAt = fromNanos(occurred)
		ref.ReceivedAt = fromNanos(received)
		if v, cut := capRunesFlag(target, maxDisplayRunes); cut {
			ref.Target = v
			ref.TargetTruncated = true
		} else {
			ref.Target = v
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > maxReferencedEvents {
		return out[:maxReferencedEvents], true, nil
	}
	return out, false, nil
}

// ReferencedObservation is the bounded retained projection for one
// exact event: the attributes snapshot plus its port summary. It never
// substitutes a profile's latest observation for an older event.
// ProfileID, ReferenceClass and SourceScope name the exact persisted
// association for this view; empty means the view predates the link or
// the link is no longer retained, and the caller must render it
// unlinked rather than guessing an event-to-profile match.
type ReferencedObservation struct {
	EventID        string
	ViewRole       string
	ReportedAt     time.Time
	ReceivedAt     time.Time
	Attributes     map[string]any
	PortsTotal     int64
	PortsTruncated bool
	ProfileID      string
	ReferenceClass string
	SourceScope    string
}

// ObservationsForEvents returns the retained observations recorded for
// exactly the given event IDs, in deterministic (event, role) order.
// An empty input returns an empty result without touching the
// database. truncated reports entries dropped by the count cap or the
// cumulative snapshot byte budget.
func (s *Store) ObservationsForEvents(ctx context.Context, eventIDs []string) ([]ReferencedObservation, bool, error) {
	if len(eventIDs) == 0 {
		return nil, false, nil
	}
	placeholders := strings.Repeat("?,", len(eventIDs))
	placeholders = strings.TrimSuffix(placeholders, ",")
	args := make([]any, 0, len(eventIDs))
	for _, id := range eventIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT ro.event_id, ro.view_role, ro.reported_at, ro.received_at, ro.attributes_json,
		ro.profile_id, COALESCE(rs.reference_class,''), COALESCE(rs.source_scope,'')
		FROM reported_observations ro LEFT JOIN reported_subjects rs ON rs.profile_id = ro.profile_id
		WHERE ro.event_id IN (%s)
		ORDER BY ro.event_id ASC, ro.view_role ASC LIMIT ?`, placeholders),
		append(args, maxReferenceObservations+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []ReferencedObservation
	var pageBytes int64
	truncated := false
	for rows.Next() {
		var obs ReferencedObservation
		var reported, received int64
		var attrsRaw string
		if err := rows.Scan(&obs.EventID, &obs.ViewRole, &reported, &received, &attrsRaw, &obs.ProfileID, &obs.ReferenceClass, &obs.SourceScope); err != nil {
			return nil, false, err
		}
		if len(out) >= maxReferenceObservations || pageBytes+int64(len(attrsRaw)) > maxReferenceObsBytes && len(out) > 0 {
			truncated = true
			break
		}
		attrs := map[string]any{}
		if err := json.Unmarshal([]byte(attrsRaw), &attrs); err != nil {
			return nil, false, err
		}
		pageBytes += int64(len(attrsRaw))
		obs.ReportedAt = fromNanos(reported)
		obs.ReceivedAt = fromNanos(received)
		obs.Attributes = attrs
		obs.PortsTotal, obs.PortsTruncated = snapshotPorts(attrs, 0, false)
		out = append(out, obs)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return out, truncated, nil
}
