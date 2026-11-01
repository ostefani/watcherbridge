package store

import (
	"context"
	"database/sql"
	"time"
)

// PurgeTotals reads per-table cumulative deletion counts.
func (s *Store) PurgeTotals(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT table_name, rows_deleted_total FROM purge_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var table string
		var n int64
		if err := rows.Scan(&table, &n); err != nil {
			return nil, err
		}
		out[table] = n
	}
	return out, rows.Err()
}

// ReceiptCount counts raw events received at or after since: ingest
// receipts for the window, aggregated in the database.
func (s *Store) ReceiptCount(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM raw_events WHERE received_at >= ?`, nanos(since)).Scan(&n)
	return n, err
}

// LastReceipt returns the newest raw-event receipt time, or false when
// the store holds no events yet.
func (s *Store) LastReceipt(ctx context.Context) (time.Time, bool, error) {
	var received sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(received_at) FROM raw_events`).Scan(&received); err != nil {
		return time.Time{}, false, err
	}
	if !received.Valid {
		return time.Time{}, false, nil
	}
	return fromNanos(received.Int64), true, nil
}

// LastPurgeRun reads the newest purge_runs row. HaveRun is false when no
// purge has ever completed a ledger entry.
func (s *Store) LastPurgeRun(ctx context.Context) (PurgeRunHealth, error) {
	var h PurgeRunHealth
	var started, finished int64
	var status, errText string
	var links, briefings, groups, groupLinks, ports, obs, progress, raw, subjects, sources, pseudonyms int64
	err := s.db.QueryRowContext(ctx, `SELECT started_at, finished_at, status, error,
		briefing_links_deleted, briefings_deleted, groups_deleted, group_links_deleted,
		ports_deleted, observations_deleted, progress_deleted, raw_events_deleted,
		subjects_deleted, sources_deleted, pseudonyms_deleted
		FROM purge_runs ORDER BY run_id DESC LIMIT 1`).Scan(
		&started, &finished, &status, &errText,
		&links, &briefings, &groups, &groupLinks, &ports, &obs, &progress,
		&raw, &subjects, &sources, &pseudonyms)
	if err == sql.ErrNoRows {
		return PurgeRunHealth{}, nil
	}
	if err != nil {
		return PurgeRunHealth{}, err
	}
	h.HaveRun = true
	h.StartedAt = fromNanos(started)
	h.Status, h.Error = status, errText
	if finished > 0 {
		h.FinishedAt = fromNanos(finished)
	}
	h.RowsDeleted = links + briefings + groups + groupLinks + ports + obs +
		progress + raw + subjects + sources + pseudonyms
	return h, nil
}

// Overview read models for Step 20 (Designv9 §3.3). All queries are
// bounded: attention candidates carry a row cap, and digest, fallback
// ratio, and error counts aggregate in the database, never in Go.

// SourceHealth is one row of report_sources: registration and receipt
// evidence. Heartbeat fields stay unset until H1 (Step 21).
type SourceHealth struct {
	Scope            string
	ConfiguredNS     string
	Capabilities     string
	LastReportAt     time.Time
	HasLastReport    bool
	HeartbeatEnabled bool
	LastHeartbeatAt  time.Time
	HasLastHeartbeat bool
	DeliveryStatus   string
}

// PurgeRunHealth is the latest purge_runs row: status, duration, and
// per-table counts, so health distinguishes a completed run from one
// that committed batches and then failed.
type PurgeRunHealth struct {
	HaveRun     bool
	StartedAt   time.Time
	FinishedAt  time.Time
	Status      string
	Error       string
	RowsDeleted int64
}

// OverviewInputs bundles the retained-row evidence behind one health
// computation. Slices stay small: per-table purge totals keyed by
// table, one source row per configured scope.
type OverviewInputs struct {
	Recent        []Briefing // attention candidates, newest first
	Digest        map[string]int
	Receipts      int
	FallbackUsed  int
	FallbackTotal int
	ForwardErrors int
	ParkedGroups  []string
	Sources       []SourceHealth
	PurgeTotals   map[string]int64
	LastPurge     PurgeRunHealth
	LastChange    time.Time
	HasLastChange bool
}

// RecentBriefings returns up to limit briefings created at or after
// since, newest first. The caller caps the attention candidate set;
// ordering and the visible-7 cut happen in the pure computation.
func (s *Store) RecentBriefings(ctx context.Context, since time.Time, limit int) ([]Briefing, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+briefingColumns+` FROM briefings
		WHERE created_at >= ? ORDER BY created_at DESC, briefing_id ASC LIMIT ?`, nanos(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Briefing
	for rows.Next() {
		var b Briefing
		if err := scanBriefing(rows, &b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BriefingDigest counts briefings by kind at or after since. The
// (created_at, kind) index serves it; EXPLAIN in the Step 20 test pins
// that plan.
func (s *Store) BriefingDigest(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM briefings
		WHERE created_at >= ? GROUP BY kind`, nanos(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		out[kind] = n
	}
	return out, rows.Err()
}

// FallbackShare counts fallback-used and total briefings at or after
// since, from retained rows rather than process counters.
func (s *Store) FallbackShare(ctx context.Context, since time.Time) (used, total int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE fallback_used != 0), COUNT(*)
		FROM briefings WHERE created_at >= ?`, nanos(since)).Scan(&used, &total)
	return used, total, err
}

// RecentForwardErrors counts raw events received at or after since
// that carry a forward or delivery error. NULL-safe: missing error
// columns never count.
func (s *Store) RecentForwardErrors(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM raw_events
		WHERE received_at >= ?
		  AND (COALESCE(last_forward_error, '') != '' OR COALESCE(delivery_error, '') != '')`,
		nanos(since)).Scan(&n)
	return n, err
}

// SourceHealthRows reads every registered source. Heartbeat columns
// are NULL until H1; they read as absent, never as zero time.
func (s *Store) SourceHealthRows(ctx context.Context) ([]SourceHealth, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source_scope, configured_identity_namespace,
		capabilities, last_report_at, heartbeat_enabled, last_heartbeat_at, delivery_status
		FROM report_sources ORDER BY source_scope ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceHealth
	for rows.Next() {
		var h SourceHealth
		var lr, lh sql.NullInt64
		var hb sql.NullBool
		if err := rows.Scan(&h.Scope, &h.ConfiguredNS, &h.Capabilities,
			&lr, &hb, &lh, &h.DeliveryStatus); err != nil {
			return nil, err
		}
		if lr.Valid {
			h.LastReportAt, h.HasLastReport = fromNanos(lr.Int64), true
		}
		h.HeartbeatEnabled = hb.Valid && hb.Bool
		if lh.Valid {
			h.LastHeartbeatAt, h.HasLastHeartbeat = fromNanos(lh.Int64), true
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
