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

// purgeRunColumns pairs purge_runs deletion columns with their table
// names for the health breakdown.
var purgeRunColumns = []struct {
	column string
	table  string
}{
	{"briefing_links_deleted", "briefing_subjects"},
	{"briefings_deleted", "briefings"},
	{"groups_deleted", "active_groups"},
	{"group_links_deleted", "group_subjects"},
	{"group_events_deleted", "group_events"},
	{"ports_deleted", "reported_ports"},
	{"observations_deleted", "reported_observations"},
	{"progress_deleted", "projection_progress"},
	{"raw_events_deleted", "raw_events"},
	{"subjects_deleted", "reported_subjects"},
	{"sources_deleted", "report_sources"},
	{"pseudonyms_deleted", "pseudonym_map"},
}

// scanPurgeRun reads one purge_runs row; ok is false on no match.
func scanPurgeRun(row *sql.Row) (h PurgeRunHealth, ok bool, err error) {
	var started, finished int64
	var status, errText string
	counts := make([]int64, len(purgeRunColumns))
	dest := make([]any, 0, 4+len(counts))
	dest = append(dest, &started, &finished, &status, &errText)
	for i := range counts {
		dest = append(dest, &counts[i])
	}
	if err := row.Scan(dest...); err != nil {
		if err == sql.ErrNoRows {
			return PurgeRunHealth{}, false, nil
		}
		return PurgeRunHealth{}, false, err
	}
	h.HaveRun = true
	h.StartedAt = fromNanos(started)
	h.Status, h.Error = status, errText
	if finished > 0 {
		h.FinishedAt = fromNanos(finished)
	}
	h.ByTable = map[string]int64{}
	for i, c := range purgeRunColumns {
		if counts[i] != 0 {
			h.ByTable[c.table] = counts[i]
		}
		h.RowsDeleted += counts[i]
	}
	return h, true, nil
}

func purgeRunSelect(where string) string {
	cols := "started_at, finished_at, status, error"
	for _, c := range purgeRunColumns {
		cols += ", " + c.column
	}
	return `SELECT ` + cols + ` FROM purge_runs ` + where
}

// LastPurgeRun reads the newest purge_runs row. HaveRun is false when no
// purge has ever written a ledger entry.
func (s *Store) LastPurgeRun(ctx context.Context) (PurgeRunHealth, error) {
	h, _, err := scanPurgeRun(s.db.QueryRowContext(ctx, purgeRunSelect(`ORDER BY run_id DESC LIMIT 1`)))
	return h, err
}

// LastSuccessfulPurgeRun reads the newest cleanly completed purge_runs
// row: the last known-good state, which survives newer failed, running,
// or error-bearing rows. A 'completed' run that still carries error
// text skipped work (for example token expiry after failed discovery),
// so it does not qualify — the latest attempt already reports it.
// HaveRun is false when no clean run exists yet.
func (s *Store) LastSuccessfulPurgeRun(ctx context.Context) (PurgeRunHealth, error) {
	h, _, err := scanPurgeRun(s.db.QueryRowContext(ctx, purgeRunSelect(`WHERE status = 'completed' AND error = '' ORDER BY run_id DESC LIMIT 1`)))
	return h, err
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

// PurgeRunHealth is one purge_runs row: status, duration, and per-table
// counts, so health distinguishes a completed run from one that
// committed batches and then failed. ByTable carries the per-table
// breakdown; RowsDeleted is its sum.
type PurgeRunHealth struct {
	HaveRun     bool
	StartedAt   time.Time
	FinishedAt  time.Time
	Status      string
	Error       string
	RowsDeleted int64
	ByTable     map[string]int64
}

// AttentionCandidate is the narrow overview projection: only the four
// effectiveReviewCTE selects the current review-lane record per
// briefing: latest non-undone expected/not_expected/snooze, with
// lapsed snoozes excluded before the pick so an earlier record
// resumes. Expired audit never reaches this query — purge deletes it
// — so exclusion here covers only the time-based snooze lapse.
const effectiveReviewCTE = `eff AS (
	SELECT subject_id, kind, until FROM (
		SELECT d.subject_id, d.kind, d.until,
			ROW_NUMBER() OVER (PARTITION BY d.subject_id ORDER BY d.created_at DESC, d.decision_id DESC) AS rn
		FROM operator_decisions d
		WHERE d.subject_kind = 'briefing'
			AND d.kind IN ('expected','not_expected','snooze')
			AND NOT EXISTS (SELECT 1 FROM operator_decisions u WHERE u.kind = 'undo' AND u.supersedes = d.decision_id)
			AND (d.kind != 'snooze' OR d.until > ?)
	) WHERE rn = 1
)`

// columns the verdict ordering needs. Restored text, model audit JSON,
// and event IDs never leave the database on this path. Decision
// fields mirror the effective review lane: flagged (not_expected)
// orders before priority rank so a flagged low-priority briefing is
// never cut by the display cap.
type AttentionCandidate struct {
	BriefingID   string
	Priority     string
	CreatedAt    time.Time
	Kind         string
	Flagged      bool
	Acknowledged bool
	SnoozedUntil time.Time
}

// AttentionCandidates returns up to limit briefings created at or after
// since, in display order: flagged first, then priority rank
// descending, newest first, stable ID tiebreaker. Verdict and headline
// counts come from the full-window effective counts instead, so rows
// past the cap still vote.
func (s *Store) AttentionCandidates(ctx context.Context, since time.Time, limit int) ([]AttentionCandidate, error) {
	now := s.now()
	rows, err := s.db.QueryContext(ctx, `WITH `+effectiveReviewCTE+` SELECT
		b.briefing_id, b.priority, b.created_at, b.kind, e.kind, e.until
		FROM briefings b LEFT JOIN eff e ON e.subject_id = b.briefing_id
		WHERE b.created_at >= ?
		ORDER BY (e.kind = 'not_expected') DESC,
			CASE b.priority WHEN 'critical' THEN 3 WHEN 'high' THEN 2 WHEN 'medium' THEN 1 ELSE 0 END DESC,
			b.created_at DESC, b.briefing_id ASC
		LIMIT ?`, nanos(now), nanos(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttentionCandidate
	for rows.Next() {
		var c AttentionCandidate
		var created int64
		var revKind sql.NullString
		var revUntil sql.NullInt64
		if err := rows.Scan(&c.BriefingID, &c.Priority, &created, &c.Kind, &revKind, &revUntil); err != nil {
			return nil, err
		}
		c.CreatedAt = fromNanos(created)
		switch revKind.String {
		case "not_expected":
			c.Flagged = true
		case "expected":
			c.Acknowledged = true
		case "snooze":
			if revUntil.Valid {
				c.SnoozedUntil = fromNanos(revUntil.Int64)
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EffectivePriorityCounts counts in-window briefings by priority for
// the verdict, excluding acknowledged and actively-snoozed rows, and
// separately counts flagged rows for the review floor. Flagged rows
// never escalate on their own: their priority stays out of the map,
// mirroring Compute. Both numbers cover the full window, never the
// capped candidate list.
func (s *Store) EffectivePriorityCounts(ctx context.Context, since, now time.Time) (map[string]int, int, error) {
	rows, err := s.db.QueryContext(ctx, `WITH `+effectiveReviewCTE+` SELECT
		b.priority, e.kind, COUNT(*)
		FROM briefings b LEFT JOIN eff e ON e.subject_id = b.briefing_id
		WHERE b.created_at >= ?
		GROUP BY b.priority, e.kind`, nanos(now), nanos(since))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	counts := map[string]int{}
	flagged := 0
	for rows.Next() {
		var priority string
		var revKind sql.NullString
		var n int
		if err := rows.Scan(&priority, &revKind, &n); err != nil {
			return nil, 0, err
		}
		switch revKind.String {
		case "not_expected":
			flagged += n
		case "expected", "snooze":
			// Explained or deferred: no verdict vote.
		default:
			counts[priority] += n
		}
	}
	return counts, flagged, rows.Err()
}

// ExplainedDigest counts in-window briefings per reported-change kind
// that carry an effective expected decision — the "explained" share of
// the digest. Snooze, notes, labels and descriptive evidence never
// count; the viewer zero-fills absent kinds for a stable wire.
func (s *Store) ExplainedDigest(ctx context.Context, since, now time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `WITH `+effectiveReviewCTE+` SELECT b.kind, COUNT(*)
		FROM briefings b JOIN eff e ON e.subject_id = b.briefing_id
		WHERE b.created_at >= ? AND e.kind = 'expected'
		GROUP BY b.kind`, nanos(now), nanos(since))
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

// PriorityCounts counts briefings by priority at or after since, across
// the entire window. The verdict and headline derive from these counts,
// never from the capped candidate list, so a high-priority briefing is
// never outvoted by newer low-priority rows past the cap.
func (s *Store) PriorityCounts(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT priority, COUNT(*) FROM briefings
		WHERE created_at >= ? GROUP BY priority`, nanos(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var priority string
		var n int
		if err := rows.Scan(&priority, &n); err != nil {
			return nil, err
		}
		out[priority] = n
	}
	return out, rows.Err()
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
