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
// evidence index, not the evidence itself.
type ReferencedEvent struct {
	EventID   string
	EventType string
}

// BriefingReferencedEvents returns the group's event index in
// deterministic event order. truncated reports entries beyond
// maxReferencedEvents, which only fires for groups retained from
// older, wider limits.
func (s *Store) BriefingReferencedEvents(ctx context.Context, groupID string) ([]ReferencedEvent, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ge.event_id, re.event_type
		FROM group_events ge JOIN raw_events re ON re.event_id = ge.event_id
		WHERE ge.group_id = ? ORDER BY ge.event_id ASC LIMIT ?`, groupID, maxReferencedEvents+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []ReferencedEvent
	for rows.Next() {
		var ref ReferencedEvent
		if err := rows.Scan(&ref.EventID, &ref.EventType); err != nil {
			return nil, false, err
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
type ReferencedObservation struct {
	EventID        string
	ViewRole       string
	ReportedAt     time.Time
	ReceivedAt     time.Time
	Attributes     map[string]any
	PortsTotal     int64
	PortsTruncated bool
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
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT event_id, view_role, reported_at, received_at, attributes_json
		FROM reported_observations WHERE event_id IN (%s)
		ORDER BY event_id ASC, view_role ASC LIMIT ?`, placeholders),
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
		if err := rows.Scan(&obs.EventID, &obs.ViewRole, &reported, &received, &attrsRaw); err != nil {
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
