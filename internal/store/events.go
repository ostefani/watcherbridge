package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
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
}

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
	if err := validateHeadersJSON(ev.HeadersJSON); err != nil {
		return PersistResult{}, err
	}
	var result PersistResult
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO raw_events
			(event_id, event_key, event_type, occurred_at, cycle, target, payload, headers_json, received_at, signature_timestamp)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(event_id) DO NOTHING`,
			ev.EventID, ev.EventKey, ev.EventType, nanos(ev.OccurredAt), ev.Cycle, ev.Target, ev.Payload, ev.HeadersJSON, nanos(ev.ReceivedAt), nanos(ev.SignatureTimestamp))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // duplicate: result.Inserted stays false
		}
		result.Inserted = true
		last := nanos(ev.ReceivedAt)
		var opened, existingDeadline int64
		err = tx.QueryRow(`SELECT group_id, opened_at, deadline_at FROM active_groups WHERE host_anchor_key = ? AND status = 'open'`, anchor).Scan(&result.GroupID, &opened, &existingDeadline)
		if err == nil && existingDeadline <= last {
			// Window expired (quiet or hard cap): a late event must not
			// join an expired group. Close it here and open a fresh one.
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
			if _, err := tx.Exec(`INSERT INTO active_groups (group_id, host_anchor_key, opened_at, last_event_at, deadline_at, status) VALUES (?, ?, ?, ?, ?, 'open')`,
				result.GroupID, anchor, opened, last, deadline); err != nil {
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
			if _, err := tx.Exec(`UPDATE active_groups SET event_count = event_count + 1 WHERE group_id = ?`, result.GroupID); err != nil {
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
