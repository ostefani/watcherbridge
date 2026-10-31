package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ostefani/watcherbridge/internal/limits"
)

type RawEvent struct {
	EventID            string
	EventKey           string
	EventType          string
	OccurredAt         time.Time
	Cycle              int
	Target             string
	Payload            []byte // original webhook bytes, never re-marshalled
	HeadersJSON        string // forwarded headers (incl. Pro signature), JSON object
	ReceivedAt         time.Time
	SignatureTimestamp time.Time
	// SourceScope records which configured scanner deployment accepted
	// the event. The server sets it from config; empty means accepted
	// before scopes existed and is adopted once at projection.
	SourceScope string
}

// StoredEvent returns the retained original bytes and envelope metadata
// for one accepted event. Acceptance tests compare the persisted payload
// against the submitted body; grouping and briefing layers must never
// depend on this for hot paths (prefer group-scoped reads).
func (s *Store) StoredEvent(ctx context.Context, eventID string) (RawEvent, error) {
	var ev RawEvent
	var occurred, received, sig int64
	err := s.db.QueryRowContext(ctx, `SELECT event_id, event_key, event_type, occurred_at, cycle, COALESCE(target,''), payload, headers_json, received_at, signature_timestamp
		FROM raw_events WHERE event_id = ?`, eventID).Scan(
		&ev.EventID, &ev.EventKey, &ev.EventType, &occurred, &ev.Cycle, &ev.Target, &ev.Payload, &ev.HeadersJSON, &received, &sig)
	if err != nil {
		return RawEvent{}, err
	}
	ev.OccurredAt, ev.ReceivedAt, ev.SignatureTimestamp = fromNanos(occurred), fromNanos(received), fromNanos(sig)
	return ev, nil
}

// PersistEventAndGroup stores the event and joins the anchor's open group in
// one transaction. The deadline extends on every join:
// min(last_event+QuietWindow, opened+MaxWindow). Reaching MaxEvents closes
// the group (result.Closed) so the next event opens a fresh one.
func (s *Store) PersistEventAndGroup(ctx context.Context, ev RawEvent, anchor string, pol GroupPolicy) (PersistResult, error) {
	if ev.EventID == "" || ev.EventKey == "" || ev.EventType == "" || len(ev.Payload) == 0 {
		return PersistResult{}, fmt.Errorf("event_id, event_key, event_type and payload are required")
	}
	if ev.ReceivedAt.IsZero() || ev.OccurredAt.IsZero() || ev.SignatureTimestamp.IsZero() {
		return PersistResult{}, fmt.Errorf("event timestamps must not be zero")
	}
	if pol.QuietWindow <= 0 {
		return PersistResult{}, fmt.Errorf("group policy QuietWindow must be positive")
	}
	if pol.MaxWindow > 0 && pol.MaxWindow < pol.QuietWindow {
		return PersistResult{}, fmt.Errorf("group policy MaxWindow must be >= QuietWindow")
	}
	if pol.MaxEvents < 0 {
		return PersistResult{}, fmt.Errorf("group policy MaxEvents must not be negative")
	}
	if pol.GroupingVersion < GroupingUnknown || pol.GroupingVersion > GroupingScoped {
		return PersistResult{}, fmt.Errorf("unsupported grouping version")
	}
	if err := validateHeadersJSON(ev.HeadersJSON); err != nil {
		return PersistResult{}, err
	}
	var result PersistResult
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO raw_events
			(event_id, event_key, event_type, occurred_at, cycle, target, payload, headers_json, received_at, signature_timestamp, source_scope)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(event_id) DO NOTHING`,
			ev.EventID, ev.EventKey, ev.EventType, nanos(ev.OccurredAt), ev.Cycle, ev.Target, ev.Payload, ev.HeadersJSON, nanos(ev.ReceivedAt), nanos(ev.SignatureTimestamp), ev.SourceScope)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // duplicate: result.Inserted stays false
		}
		result.Inserted = true
		// Queue the event for reported-context projection in the same
		// transaction: only freshly accepted events need work, so a
		// duplicate repost never requeues. The projector deletes the
		// row when the event's projection commits.
		if _, err := tx.Exec(`INSERT OR IGNORE INTO projection_pending (event_id, received_at) VALUES (?, ?)`,
			ev.EventID, nanos(ev.ReceivedAt)); err != nil {
			return err
		}
		last := nanos(ev.ReceivedAt)
		var opened, existingDeadline, existingBytes int64
		var existingVersion int
		err = tx.QueryRow(`SELECT group_id, opened_at, deadline_at, total_bytes, grouping_version FROM active_groups WHERE host_anchor_key = ? AND status = 'open'`, anchor).Scan(&result.GroupID, &opened, &existingDeadline, &existingBytes, &existingVersion)
		// existingBytes > 0 keeps a lone oversize payload on a fresh
		// group: closing an empty group would park it failed with no
		// events. Config refuses such bodies at startup; this is the
		// backstop for callers that bypass validation.
		if err == nil && (existingVersion != pol.GroupingVersion || existingDeadline <= last || (existingBytes > 0 && existingBytes+int64(len(ev.Payload)) > limits.MaxGroupBytes)) {
			// The algorithm changed, the window expired, or the group is
			// byte-full: the crossing event must not join it. Close it
			// here and open a fresh one — the crossing event founds the
			// next group, so the cap is a strict bound and rollover
			// reuses the expired-group path (the unique open-group index
			// requires close-before-insert in the same transaction).
			if _, err := tx.Exec(`UPDATE active_groups SET status = 'closed' WHERE group_id = ?`, result.GroupID); err != nil {
				return err
			}
			result.ClosedGroupIDs = append(result.ClosedGroupIDs, result.GroupID)
			err = sql.ErrNoRows
		}
		if err == sql.ErrNoRows {
			result.GroupID = fmt.Sprintf("grp-%d-%d", last, s.groupSeq.Add(1))
			opened = last
			deadline := groupDeadline(opened, last, pol)
			// Seed zero: the group_events join below adds this event's
			// bytes and count, so founding and joining share one path.
			if _, err := tx.Exec(`INSERT INTO active_groups (group_id, host_anchor_key, opened_at, last_event_at, deadline_at, status, total_bytes, grouping_version) VALUES (?, ?, ?, ?, ?, 'open', 0, ?)`,
				result.GroupID, anchor, opened, last, deadline, pol.GroupingVersion); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			deadline := groupDeadline(opened, last, pol)
			if _, err := tx.Exec(`UPDATE active_groups SET last_event_at = ?, deadline_at = ? WHERE group_id = ?`, last, deadline, result.GroupID); err != nil {
				return err
			}
		}
		res, err = tx.Exec(`INSERT INTO group_events (group_id, event_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, result.GroupID, ev.EventID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			// Real join only: a replayed event_id hits the conflict and
			// moves neither counter, so idempotent retries never inflate
			// the caps. len() is Go-side — no per-persist SUM needed.
			if _, err := tx.Exec(`UPDATE active_groups SET event_count = event_count + 1, total_bytes = total_bytes + ? WHERE group_id = ?`, len(ev.Payload), result.GroupID); err != nil {
				return err
			}
		}
		if pol.MaxEvents > 0 {
			var count int
			if err := tx.QueryRow(`SELECT event_count FROM active_groups WHERE group_id = ?`, result.GroupID).Scan(&count); err != nil {
				return err
			}
			if count >= pol.MaxEvents {
				if _, err := tx.Exec(`UPDATE active_groups SET status = 'closed' WHERE group_id = ?`, result.GroupID); err != nil {
					return err
				}
				result.Closed = true
			}
		}
		return nil
	})
	if err != nil {
		return PersistResult{}, err
	}
	return result, nil
}
