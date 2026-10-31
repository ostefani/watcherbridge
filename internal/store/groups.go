package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// DefaultBriefingPage bounds one NeedsBriefing drain page. The worker loops
// pages until empty, checking its context between groups.
const DefaultBriefingPage = 64

type GroupPolicy struct {
	QuietWindow time.Duration // debounce: group closes this long after last event
	MaxWindow   time.Duration // hard cap from first event
	MaxEvents   int           // size cap; config requires 1..128, store treats <= 0 as unlimited
}

type PersistResult struct {
	Inserted       bool
	GroupID        string
	Closed         bool     // size cap reached; group is ready for briefing
	ClosedGroupIDs []string // overdue groups closed by this call (hard cap)
}

func groupDeadline(opened, last int64, pol GroupPolicy) int64 {
	deadline := last + pol.QuietWindow.Nanoseconds()
	if pol.MaxWindow > 0 {
		if limit := opened + pol.MaxWindow.Nanoseconds(); deadline > limit {
			deadline = limit
		}
	}
	return deadline
}

type TakenGroup struct {
	GroupID    string
	Anchor     string
	OpenedAt   time.Time
	RetryCount int // see LastAttempt
	// TransientRefunds counts takes forgiven for transient store
	// pressure. The worker drives its release backoff from this, so
	// repeated saturation escalates instead of retrying flat out.
	TransientRefunds int
	Events           []RawEvent
}

// LastAttempt reports whether this take is the final try before the retry
// cap: the worker should skip the provider and write a fallback directly.
func (g TakenGroup) LastAttempt() bool { return g.RetryCount >= maxGroupRetries-1 }

var ErrGroupNotClosed = errors.New("group is not closed")

// TakeGroup atomically moves a closed group -> processing and reads the
// group and its events in the same transaction, so a failure cannot strand
// the group in processing. Only closed groups may be taken.
func (s *Store) TakeGroup(ctx context.Context, groupID string) (TakenGroup, error) {
	var taken TakenGroup
	err := s.write(ctx, func(tx *sql.Tx) error {
		// The take is the attempt: the counter advances here and only
		// here, so a crash between take and briefing still counts and a
		// poison group cannot crash-loop forever. Release paths never
		// increment; they only park at the cap.
		now := nanos(s.now())
		res, err := tx.Exec(`UPDATE active_groups SET status = 'processing', processing_since = ?, retry_count = retry_count + 1 WHERE group_id = ? AND status = 'closed' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)`, now, groupID, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrGroupNotClosed
		}
		taken.GroupID = groupID
		var openedNanos int64
		if err := tx.QueryRow(`SELECT host_anchor_key, opened_at, retry_count, transient_refunds FROM active_groups WHERE group_id = ?`, groupID).Scan(&taken.Anchor, &openedNanos, &taken.RetryCount, &taken.TransientRefunds); err != nil {
			return err
		}
		taken.OpenedAt = fromNanos(openedNanos)
		rows, err := tx.Query(`SELECT e.event_id, e.event_key, e.event_type, e.occurred_at, e.cycle, COALESCE(e.target,''), e.payload, e.headers_json, e.received_at, e.signature_timestamp
			FROM group_events ge JOIN raw_events e ON e.event_id = ge.event_id WHERE ge.group_id = ? ORDER BY e.received_at, e.event_id`, groupID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev RawEvent
			var occurred, received, sig int64
			if err := rows.Scan(&ev.EventID, &ev.EventKey, &ev.EventType, &occurred, &ev.Cycle, &ev.Target, &ev.Payload, &ev.HeadersJSON, &received, &sig); err != nil {
				return err
			}
			ev.OccurredAt, ev.ReceivedAt, ev.SignatureTimestamp = fromNanos(occurred), fromNanos(received), fromNanos(sig)
			taken.Events = append(taken.Events, ev)
		}
		return rows.Err()
	})
	if err != nil {
		return TakenGroup{}, err
	}
	return taken, nil
}

// ReleaseStale returns processing groups older than maxAge to closed (or
// failed at the retry cap) — the runtime counterpart for hung workers.
// maxAge is a lease, not a timeout guess: it must exceed provider timeout
// x2 (call + repair attempt) plus margin, or a slow worker and its
// replacement will both pay for model calls (correctness still holds via
// the state check and unique briefing index). See Designv5 settings.
func (s *Store) ReleaseStale(ctx context.Context, now time.Time, maxAge time.Duration) ([]string, error) {
	cutoff := nanos(now.Add(-maxAge))
	var ids []string
	err := s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT group_id FROM active_groups WHERE status = 'processing' AND processing_since IS NOT NULL AND processing_since <= ?`, cutoff)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			// The lease already waited out maxAge: the group is due
			// immediately, so any stale backoff is cleared.
			if _, err := tx.Exec(`UPDATE active_groups SET status = CASE WHEN retry_count >= ? THEN 'failed' ELSE 'closed' END, processing_since = NULL, next_attempt_at = NULL WHERE group_id = ? AND status = 'processing'`, maxGroupRetries, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// FailedGroups lists groups parked after exhausting retries.
func (s *Store) FailedGroups(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT group_id FROM active_groups WHERE status = 'failed' ORDER BY opened_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Recover (startup only) closes expired open groups, and returns crashed
// processing groups to closed — or to failed once retries are exhausted.
// 'closed' means needs a briefing; 'done' means briefed. Returns nil on error.
func (s *Store) Recover(ctx context.Context, now time.Time) ([]string, error) {
	var ids []string
	err := s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT group_id FROM active_groups
			WHERE (status IN ('open','processing') AND deadline_at <= ?)
			   OR (status = 'processing' AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id))`, nanos(now))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE active_groups SET status = CASE WHEN status = 'processing' AND retry_count >= ? THEN 'failed' ELSE 'closed' END, processing_since = NULL WHERE group_id = ?`, maxGroupRetries, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// CloseDue moves only open groups past their deadline to closed. It is the
// runtime tick counterpart of Recover (which is startup-only): it never
// touches processing groups, so in-flight briefings are not requeued.
func (s *Store) CloseDue(ctx context.Context, now time.Time) ([]string, error) {
	var ids []string
	err := s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT group_id FROM active_groups WHERE status = 'open' AND deadline_at <= ?`, nanos(now))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE active_groups SET status = 'closed' WHERE group_id = ? AND status = 'open'`, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// The group byte budget lives in internal/limits so config validation
// can check it without importing the sqlite driver. See limits.MaxGroupBytes.
const maxGroupRetries = 5

// maxTransientRefunds bounds takes forgiven for transient store pressure.
// The worker drives the release backoff from the refund count (2s doubling
// to the 30s cap), so 20 refunds span roughly ten minutes of persistent
// saturation — generous for incidents, bounded for runaways. The cap counts
// refunds, not takes: real outcomes still park at maxGroupRetries.
const maxTransientRefunds = 20

// ReleaseGroup returns a processing group to closed for a later retry. It
// never increments the counter — takes own counting. At maxGroupRetries the
// group is parked in 'failed' instead, so a poison group cannot loop forever.
// The immediate variant clears any backoff: callers that converge to a
// terminal state (parkFailed) must not be gated by a previous delay.
func (s *Store) ReleaseGroup(ctx context.Context, groupID string) (bool, error) {
	return s.releaseDelayed(ctx, groupID, 0, false)
}

// ReleaseGroupDelayed requeues with a not-before delay: the group stays
// closed but leaves the briefing queue until the backoff elapses, so a
// failure cannot be retried hot inside the same drain pass. A zero delay
// requeues immediately.
func (s *Store) ReleaseGroupDelayed(ctx context.Context, groupID string, delay time.Duration) (bool, error) {
	return s.releaseDelayed(ctx, groupID, delay, false)
}

// ReleaseGroupTransient refunds one take and requeues with a delay. Use it
// when the attempt never really ran — writer saturation, shutdown, or a
// cancelled context: transient store pressure must not burn the retry
// budget that poison groups are capped by.
func (s *Store) ReleaseGroupTransient(ctx context.Context, groupID string, delay time.Duration) (bool, error) {
	return s.releaseDelayed(ctx, groupID, delay, true)
}

func (s *Store) releaseDelayed(ctx context.Context, groupID string, delay time.Duration, refund bool) (bool, error) {
	matched := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		due := sql.NullInt64{}
		if delay > 0 {
			due = sql.NullInt64{Int64: nanos(s.now().Add(delay)), Valid: true}
		}
		var res sql.Result
		var err error
		if refund {
			// A transient failure (saturated queue, shutdown, dead
			// context) is never the group's fault, so a refund
			// requeues with the take unhappened — even at the retry
			// cap. Parking failed is reserved for real outcomes (the
			// non-refund branch below) and for a refund runaway: past
			// maxTransientRefunds the group parks failed instead of
			// retrying forever, which stops the provider spend.
			res, err = tx.Exec(`UPDATE active_groups SET status = CASE WHEN transient_refunds + 1 > ? THEN 'failed' ELSE 'closed' END, processing_since = NULL, next_attempt_at = ?, retry_count = CASE WHEN retry_count > 0 THEN retry_count - 1 ELSE 0 END, transient_refunds = transient_refunds + 1 WHERE group_id = ? AND status = 'processing'`, maxTransientRefunds, due, groupID)
		} else {
			res, err = tx.Exec(`UPDATE active_groups SET status = CASE WHEN retry_count >= ? THEN 'failed' ELSE 'closed' END, processing_since = NULL, next_attempt_at = ? WHERE group_id = ? AND status = 'processing'`, maxGroupRetries, due, groupID)
		}
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		matched = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

// NeedsBriefing lists closed groups with no briefing row yet whose backoff
// (if any) has elapsed, oldest first, at most limit rows. A non-positive
// limit selects the default page.
func (s *Store) NeedsBriefing(ctx context.Context, limit int, now time.Time) ([]string, error) {
	if limit <= 0 {
		limit = DefaultBriefingPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT group_id FROM active_groups WHERE status = 'closed'
		AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id) ORDER BY opened_at LIMIT ?`, nanos(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) GroupEventCount(ctx context.Context, anchor string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_events ge
		JOIN active_groups ag ON ag.group_id = ge.group_id WHERE ag.host_anchor_key = ?`, anchor).Scan(&n)
	return n, err
}

func (s *Store) GroupStatus(ctx context.Context, groupID string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM active_groups WHERE group_id = ?`, groupID).Scan(&status)
	return status, err
}

func (s *Store) GroupDeadline(ctx context.Context, groupID string) (time.Time, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT deadline_at FROM active_groups WHERE group_id = ?`, groupID).Scan(&n)
	return fromNanos(n), err
}

// GroupSnapshot is the viewer-facing view of an in-progress group: all
// fields are safe for local display (anchors are raw local identifiers).
// Open, closed and processing rows all appear. A closed row is queued
// behind a slow provider call, not gone: hiding it would drop queued
// groups from the viewer for the whole provider timeout. Closed renders
// as waiting; processing renders as "Analysing…" until its briefing
// lands, never vanishing early.
type GroupSnapshot struct {
	GroupID    string
	Anchor     string
	Status     string
	Deadline   time.Time
	OpenedAt   time.Time
	LastEvent  time.Time
	EventCount int
}

// OpenGroupSnapshots lists groups still accumulating events plus queued
// and in-flight ones, oldest deadline first. Closed-but-untaken groups
// are the briefing queue: while one group waits on the provider, the
// rest wait closed behind it, and the snapshot must show them waiting.
// The groups_status_deadline index serves the filter and order; the event
// count comes from the join table in the same query, and opened/last-
// event times ride along from the group row the writer already maintains.
func (s *Store) OpenGroupSnapshots(ctx context.Context) ([]GroupSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ag.group_id, ag.host_anchor_key, ag.status, ag.deadline_at,
		ag.opened_at, ag.last_event_at, COUNT(ge.event_id) FROM active_groups ag
		LEFT JOIN group_events ge ON ge.group_id = ag.group_id
		WHERE ag.status IN ('open', 'closed', 'processing') GROUP BY ag.group_id ORDER BY ag.deadline_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupSnapshot
	for rows.Next() {
		var g GroupSnapshot
		var deadline, opened, last int64
		if err := rows.Scan(&g.GroupID, &g.Anchor, &g.Status, &deadline, &opened, &last, &g.EventCount); err != nil {
			return nil, err
		}
		g.Deadline = fromNanos(deadline)
		g.OpenedAt = fromNanos(opened)
		g.LastEvent = fromNanos(last)
		out = append(out, g)
	}
	return out, rows.Err()
}

// ForwardState reports delivery state for one event.
