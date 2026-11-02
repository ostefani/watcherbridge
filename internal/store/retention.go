package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Retention bounds (Designv9 §4.6). Every purge runs in bounded
// transactions sharing one ≤200-row budget, oldest first, so a large
// backlog drains over resumable batches instead of one unbounded
// commit. Children are explicitly pre-deleted and parents gated on
// childlessness, so cascades stay empty and every deleted row is
// counted against the budget in its own statement.

// maxPurgeRowsPerTxn caps committed writes across all statements in
// one purge transaction: deletions plus purge_state and run-ledger
// bookkeeping. Reserving those auxiliary rows before choosing delete
// limits keeps the committed total within budget.
const maxPurgeRowsPerTxn = 200

// purgeLedgerTimeout bounds terminal run-ledger finalization when the
// caller's context is already cancelled. Shutdown must record the run
// outcome without waiting indefinitely behind writer pressure.
const purgeLedgerTimeout = 5 * time.Second

// heartbeatRetention compacts stale scanner heartbeat rows. A scope
// that still owns subjects is never compacted, however old.
const heartbeatRetention = 7 * 24 * time.Hour

// Checkpoint runs a TRUNCATE checkpoint on the single writer goroutine (but
// outside a transaction, which SQLite forbids for checkpoints) so the -wal
// file drains after load spikes. Checkpoints only run here, never beside
// the writer, so they cannot race an in-flight commit.
func (s *Store) Checkpoint(ctx context.Context) error {
	job := writeJob{ctx: ctx, done: make(chan error, 1)}
	// QueryRow surfaces the busy count: a nonzero second column means a
	// reader blocked the TRUNCATE and the WAL did not drain.
	job.raw = func() error {
		var busy, logFrames, checkpointed int64
		if err := s.db.QueryRowContext(job.ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
			return err
		}
		if busy != 0 {
			return fmt.Errorf("wal checkpoint blocked: busy=%d log=%d checkpointed=%d", busy, logFrames, checkpointed)
		}
		return nil
	}
	return s.enqueue(ctx, job)
}

// MarkUndeliverable parks an event the forwarder can never send (e.g. its
// stored headers no longer parse). Terminal like stale_for_sweep: the sweep
// never selects it again, and the viewer can show the reason.
func (s *Store) MarkUndeliverable(ctx context.Context, eventID, reason string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE raw_events SET delivery_error = ? WHERE event_id = ? AND delivery_error IS NULL`, reason, eventID)
		return err
	})
}

const briefingPurgeFloor = 24 * time.Hour

// purgeFloor is the youngest age PurgeRawEvents will ever accept.
const purgeFloor = time.Hour

// failedEventFloor keeps failed groups' events for debugging a week.
const failedEventFloor = 7 * 24 * time.Hour

// PurgeSettings carries validated retention horizons. Raw events share
// the briefing horizon: briefing history may not outlive its evidence
// while the briefing is retained, so raw eligibility excludes events
// whose group still has a briefing row.
type PurgeSettings struct {
	BriefingRetention time.Duration
	SubjectRetention  time.Duration
	// DecisionRetention bounds the operator-decision audit horizon;
	// materialized labels are columns on reported_subjects and survive
	// audit expiry. Zero means the 180d default, so existing callers
	// keep history. Audit expires purely by row age with no exemption
	// for rows the effective view still uses.
	DecisionRetention time.Duration
	// ActiveSource names the configured SOURCE_ID. A quiet-but-live
	// registration is never retired by inactivity alone (Designv9 §4.6);
	// when empty, source cleanup keeps its legacy behavior for tests.
	ActiveSource string
}

// defaultDecisionRetention is the operator-decision audit horizon when
// PurgeSettings leaves DecisionRetention unset.
const defaultDecisionRetention = 180 * 24 * time.Hour

func decisionRetentionOf(settings PurgeSettings) time.Duration {
	if settings.DecisionRetention > 0 {
		return settings.DecisionRetention
	}
	return defaultDecisionRetention
}

// PurgeReport counts committed deletions per table for Step 20 health.
// Relation, observation, queue and progress rows are counted in their
// own statements; cascades are kept empty by childless gating, never
// relied on for accounting. Bookkeeping counts the purge_state
// upserts that share the transaction's row budget: a 200-row delete
// commits at least 201 affected rows. total (and therefore the drain
// progress check) counts deletions only, so an idle pass with no
// deletions terminates even though no bookkeeping row is written.
type PurgeReport struct {
	BriefingLinks int64
	Briefings     int64
	Traces        int64 // agent_traces rows; deleted with their briefing
	Groups        int64
	GroupLinks    int64 // group_subjects rows; group_events has its own counter below
	GroupEvents   int64
	Ports         int64
	Observations  int64
	Progress      int64 // projection_progress + projection_pending rows
	RawEvents     int64
	Subjects      int64
	Sources       int64
	Pseudonyms    int64
	Decisions     int64 // expired operator-decision audit rows; labels survive in reported_subjects
	Bookkeeping   int64 // purge_state upserts sharing the row budget
}

func (r *PurgeReport) add(o PurgeReport) {
	r.BriefingLinks += o.BriefingLinks
	r.Briefings += o.Briefings
	r.Traces += o.Traces
	r.Groups += o.Groups
	r.GroupLinks += o.GroupLinks
	r.GroupEvents += o.GroupEvents
	r.Ports += o.Ports
	r.Observations += o.Observations
	r.Progress += o.Progress
	r.RawEvents += o.RawEvents
	r.Subjects += o.Subjects
	r.Sources += o.Sources
	r.Pseudonyms += o.Pseudonyms
	r.Decisions += o.Decisions
	r.Bookkeeping += o.Bookkeeping
}

func (r PurgeReport) total() int64 {
	return r.BriefingLinks + r.Briefings + r.Traces + r.Groups + r.GroupLinks + r.GroupEvents + r.Ports +
		r.Observations + r.Progress + r.RawEvents + r.Subjects + r.Sources + r.Pseudonyms + r.Decisions
}

// purgeStages selects which table families one batch transaction covers.
// PurgeOnce runs all three; the legacy wrappers run their own family so
// existing callers keep their contracts while the deletes stay bounded.
type purgeStages uint8

const (
	stageBriefings purgeStages = 1 << iota
	stageEvents
	stageSubjects
	stageAll = stageBriefings | stageEvents | stageSubjects
)

// purgePlan carries one batch's horizons. Cutoffs are absolute nanos so
// every statement in the transaction agrees on eligibility.
type purgePlan struct {
	stages      purgeStages
	briefingCut int64
	rawCut      int64
	failedCut   int64
	subjectCut  int64
	sourceCut   int64
	tokenCut    int64
	decisionCut int64
	// activeSource names the configured SOURCE_ID. A quiet-but-live
	// registration is never retired by inactivity alone (Designv9 §4.6).
	activeSource string
	// skipTokenExpiry keeps token deletions out of the drain when
	// reference discovery could not establish completeness. Every other
	// stage still proceeds; expiry is deferred to a later run.
	skipTokenExpiry bool
	// runID carries the purge_runs ledger row updated atomically with
	// each deleting batch. Zero disables ledger updates for direct
	// bounded-batch tests.
	runID int64
	// tokenFence is the maximum briefing rowid included by protected
	// reference discovery. A newer briefing inserted during discovery
	// blocks token deletions until the next purge includes it.
	tokenFence int64
	// tokenDecFence is the maximum briefing-subject decision rowid
	// seen by protected reference discovery. A decision committed
	// after discovery can pin an aged briefing whose tokens were
	// never scanned, so a newer pin defers token expiry the same
	// way a newer briefing does.
	tokenDecFence int64
}

// eligibleEvents selects raw events old enough to purge: not pending
// projection, not evidence for a retained briefing, and not evidence
// for a group that has not reached a terminal state (Designv9 §4.6).
// Open and closed-unbriefed groups pin their aged observations, ports
// and projection markers until the group closes and briefs or fails;
// processing groups likewise keep their full context across restarts.
// Failed groups use the longer debug floor. Oldest first with a small
// batch cap so the IN lists stay bounded; per-statement LIMITs enforce
// the row budget regardless of per-event fan-out.
const eligibleEvents = `event_id IN (SELECT DISTINCT re.event_id FROM raw_events re
	LEFT JOIN group_events ge0 ON ge0.event_id = re.event_id
	LEFT JOIN active_groups ag0 ON ag0.group_id = ge0.group_id
	WHERE re.received_at < CASE WHEN ag0.status = 'failed' THEN MIN(?, ?) ELSE ? END
	AND NOT EXISTS (SELECT 1 FROM projection_pending p WHERE p.event_id = re.event_id)
	AND NOT EXISTS (SELECT 1 FROM group_events ge JOIN briefings b ON b.group_id = ge.group_id WHERE ge.event_id = re.event_id)
	AND COALESCE(ag0.status, 'done') NOT IN ('open', 'closed', 'processing')
	ORDER BY re.received_at, re.rowid LIMIT 64)`

// purgeBatch deletes at most maxPurgeRowsPerTxn rows in one transaction,
// oldest first, in dependency order. Eligibility is checked inside the
// deletion transaction: retained briefings, pending projection and live
// groups protect their evidence atomically.
func (s *Store) purgeBatch(ctx context.Context, now time.Time, plan purgePlan) (PurgeReport, bool, error) {
	var report PurgeReport
	err := s.write(ctx, func(tx *sql.Tx) error {
		remaining := int64(maxPurgeRowsPerTxn)
		if plan.runID != 0 {
			// Reserve the atomic per-batch ledger update before any
			// delete limit is chosen.
			remaining--
		}
		deleteLimit := func() int64 {
			limit := remaining - 1 // the purge_state upsert for this take
			if limit < 0 {
				return 0
			}
			return limit
		}
		limit := int64(0)
		take := func(table string, cutoff int64, stmt string, limit int64, args ...any) error {
			if limit <= 0 {
				return nil
			}
			n, err := deleteBounded(tx, stmt, args...)
			if err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
			remaining -= n
			// The purge_state upsert is a real write in the same
			// transaction, so it shares the row budget: a 200-row
			// delete commits at least 201 affected rows, and the
			// report charges it instead of hiding it.
			remaining--
			report.Bookkeeping++
			if err := recordPurgeState(tx, table, nanos(now), cutoff, n); err != nil {
				return err
			}
			switch table {
			case "briefing_subjects":
				report.BriefingLinks += n
			case "agent_traces":
				report.Traces += n
			case "briefings":
				report.Briefings += n
			case "active_groups":
				report.Groups += n
			case "group_subjects":
				report.GroupLinks += n
			case "group_events":
				report.GroupEvents += n
			case "reported_ports":
				report.Ports += n
			case "reported_observations":
				report.Observations += n
			case "projection_progress", "projection_pending":
				report.Progress += n
			case "raw_events":
				report.RawEvents += n
			case "reported_subjects":
				report.Subjects += n
			case "report_sources":
				report.Sources += n
			case "pseudonym_map":
				report.Pseudonyms += n
			case "operator_decisions":
				report.Decisions += n
			}
			return nil
		}
		if plan.stages&stageBriefings != 0 {
			// A briefing a retained decision still references cannot
			// purge: its audit outlives it, and deleting the briefing
			// would orphan the trail and unblock evidence the decision
			// still justifies. The pin follows the audit cutoff, never
			// the effective view, so no backup audit is needed. The pin
			// lives inside the age selection before LIMIT: selecting
			// oldest-first and filtering after would stall the drain
			// on a fully pinned batch while eligible work waits behind
			// it. The outer takes repeat the pin as an invariant.
			oldBriefings := `briefing_id IN (SELECT briefing_id FROM briefings WHERE created_at < ?
				AND NOT EXISTS (SELECT 1 FROM operator_decisions d WHERE d.subject_kind = 'briefing' AND d.subject_id = briefings.briefing_id AND d.created_at >= ?) ORDER BY created_at, rowid LIMIT ?)`
			limit = deleteLimit()
			if err := take("briefing_subjects", plan.briefingCut, `DELETE FROM briefing_subjects WHERE rowid IN (SELECT rowid FROM briefing_subjects WHERE `+oldBriefings+`
				AND NOT EXISTS (SELECT 1 FROM operator_decisions d WHERE d.subject_kind = 'briefing' AND d.subject_id = briefing_subjects.briefing_id AND d.created_at >= ?) ORDER BY rowid LIMIT ?)`, limit, plan.briefingCut, plan.decisionCut, limit, plan.decisionCut, limit); err != nil {
				return err
			}
			// Trace steps are audit children of their briefing: they drain
			// under the same decision pin before the briefing row, so no
			// retained audit outlives its briefing and no briefing deletes
			// while steps remain.
			limit = deleteLimit()
			if err := take("agent_traces", plan.briefingCut, `DELETE FROM agent_traces WHERE rowid IN (SELECT rowid FROM agent_traces WHERE briefing_id IN (SELECT briefing_id FROM briefings WHERE created_at < ?
				AND NOT EXISTS (SELECT 1 FROM operator_decisions d WHERE d.subject_kind = 'briefing' AND d.subject_id = briefings.briefing_id AND d.created_at >= ?)) ORDER BY rowid LIMIT ?)`, limit, plan.briefingCut, plan.decisionCut, limit); err != nil {
				return err
			}
			limit = deleteLimit()
			if err := take("briefings", plan.briefingCut, `DELETE FROM briefings WHERE rowid IN (SELECT rowid FROM briefings WHERE created_at < ? AND NOT EXISTS (SELECT 1 FROM briefing_subjects WHERE briefing_subjects.briefing_id = briefings.briefing_id)
				AND NOT EXISTS (SELECT 1 FROM agent_traces WHERE agent_traces.briefing_id = briefings.briefing_id)
				AND NOT EXISTS (SELECT 1 FROM operator_decisions d WHERE d.subject_kind = 'briefing' AND d.subject_id = briefings.briefing_id AND d.created_at >= ?) ORDER BY created_at, rowid LIMIT ?)`, limit, plan.briefingCut, plan.decisionCut, limit); err != nil {
				return err
			}
			deletableDone := `status = 'done'
				AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id)
				AND NOT EXISTS (SELECT 1 FROM group_events WHERE group_events.group_id = active_groups.group_id)`
			limit = deleteLimit()
			if err := take("group_subjects", plan.briefingCut, `DELETE FROM group_subjects WHERE rowid IN (SELECT rowid FROM group_subjects WHERE group_id IN (SELECT group_id FROM active_groups WHERE `+deletableDone+`) ORDER BY rowid LIMIT ?)`, limit, limit); err != nil {
				return err
			}
			limit = deleteLimit()
			if err := take("active_groups", plan.briefingCut, `DELETE FROM active_groups WHERE rowid IN (SELECT rowid FROM active_groups WHERE `+deletableDone+`
				AND NOT EXISTS (SELECT 1 FROM group_subjects WHERE group_subjects.group_id = active_groups.group_id) ORDER BY opened_at, rowid LIMIT ?)`, limit, limit); err != nil {
				return err
			}
		}
		if plan.stages&stageEvents != 0 {
			limit = deleteLimit()
			if err := take("reported_ports", plan.rawCut, `DELETE FROM reported_ports WHERE rowid IN (SELECT rowid FROM reported_ports WHERE observation_id IN (SELECT observation_id FROM reported_observations WHERE `+eligibleEvents+`) ORDER BY rowid LIMIT ?)`, limit, plan.rawCut, plan.failedCut, plan.rawCut, limit); err != nil {
				return err
			}
			limit = deleteLimit()
			if err := take("reported_observations", plan.rawCut, `DELETE FROM reported_observations WHERE rowid IN (SELECT observation_id FROM reported_observations WHERE `+eligibleEvents+` AND NOT EXISTS (SELECT 1 FROM reported_ports WHERE reported_ports.observation_id = reported_observations.observation_id) ORDER BY observation_id LIMIT ?)`, limit, plan.rawCut, plan.failedCut, plan.rawCut, limit); err != nil {
				return err
			}
			limit = deleteLimit()
			if err := take("projection_progress", plan.rawCut, `DELETE FROM projection_progress WHERE rowid IN (SELECT rowid FROM projection_progress WHERE `+eligibleEvents+` ORDER BY rowid LIMIT ?)`, limit, plan.rawCut, plan.failedCut, plan.rawCut, limit); err != nil {
				return err
			}
			limit = deleteLimit()
			if err := take("projection_pending", plan.rawCut, `DELETE FROM projection_pending WHERE rowid IN (SELECT rowid FROM projection_pending WHERE `+eligibleEvents+` ORDER BY rowid LIMIT ?)`, limit, plan.rawCut, plan.failedCut, plan.rawCut, limit); err != nil {
				return err
			}
			limit = deleteLimit()
			if err := take("group_events", plan.rawCut, `DELETE FROM group_events WHERE rowid IN (SELECT rowid FROM group_events WHERE group_id IN (SELECT group_id FROM active_groups WHERE status IN ('done','failed')) AND `+eligibleEvents+` ORDER BY rowid LIMIT ?)`, limit, plan.rawCut, plan.failedCut, plan.rawCut, limit); err != nil {
				return err
			}
			limit = deleteLimit()
			if err := take("raw_events", plan.rawCut, `DELETE FROM raw_events WHERE rowid IN (SELECT rowid FROM raw_events WHERE received_at < ?
				AND NOT EXISTS (SELECT 1 FROM group_events WHERE group_events.event_id = raw_events.event_id)
				AND NOT EXISTS (SELECT 1 FROM reported_observations WHERE reported_observations.event_id = raw_events.event_id)
				AND NOT EXISTS (SELECT 1 FROM projection_pending WHERE projection_pending.event_id = raw_events.event_id)
				AND NOT EXISTS (SELECT 1 FROM projection_progress WHERE projection_progress.event_id = raw_events.event_id) ORDER BY received_at, rowid LIMIT ?)`, limit, plan.rawCut, limit); err != nil {
				return err
			}
			failedGroups := `group_id IN (SELECT group_id FROM active_groups WHERE status = 'failed' AND deadline_at < ?
				AND NOT EXISTS (SELECT 1 FROM group_events WHERE group_events.group_id = active_groups.group_id)
				AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id))`
			limit = deleteLimit()
			if err := take("group_subjects", plan.failedCut, `DELETE FROM group_subjects WHERE rowid IN (SELECT rowid FROM group_subjects WHERE `+failedGroups+` ORDER BY rowid LIMIT ?)`, limit, plan.failedCut, limit); err != nil {
				return err
			}
			limit = deleteLimit()
			if err := take("active_groups", plan.failedCut, `DELETE FROM active_groups WHERE rowid IN (SELECT rowid FROM active_groups WHERE status = 'failed' AND deadline_at < ?
				AND NOT EXISTS (SELECT 1 FROM group_events WHERE group_events.group_id = active_groups.group_id)
				AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id)
				AND NOT EXISTS (SELECT 1 FROM group_subjects WHERE group_subjects.group_id = active_groups.group_id) ORDER BY deadline_at, rowid LIMIT ?)`, limit, plan.failedCut, limit); err != nil {
				return err
			}
		}
		if plan.stages&stageSubjects != 0 {
			limit = deleteLimit()
			if err := take("reported_subjects", plan.subjectCut, `DELETE FROM reported_subjects WHERE rowid IN (SELECT rowid FROM reported_subjects WHERE last_report_at < ?
				AND NOT EXISTS (SELECT 1 FROM reported_observations WHERE reported_observations.profile_id = reported_subjects.profile_id)
				AND NOT EXISTS (SELECT 1 FROM group_subjects WHERE group_subjects.profile_id = reported_subjects.profile_id)
				AND NOT EXISTS (SELECT 1 FROM briefing_subjects WHERE briefing_subjects.profile_id = reported_subjects.profile_id)
				AND NOT EXISTS (SELECT 1 FROM operator_decisions WHERE operator_decisions.subject_kind = 'reported_subject' AND operator_decisions.subject_id = reported_subjects.profile_id AND operator_decisions.created_at >= ?) ORDER BY last_report_at, rowid LIMIT ?)`, limit, plan.subjectCut, plan.decisionCut, limit); err != nil {
				return err
			}
			// Decision audit expires purely by row age: the effective
			// view never exempts a row, so no backup audit is needed.
			// An undo outlives its target by construction (the target
			// is always older, and the batch order is oldest-first),
			// but the pair gate below defers an undo while its target
			// row still exists, so a bounded pass never orphans one.
			// Materialized labels are columns on reported_subjects,
			// never audit rows, so they survive this delete.
			limit = deleteLimit()
			if err := take("operator_decisions", plan.decisionCut, `DELETE FROM operator_decisions WHERE rowid IN (SELECT rowid FROM operator_decisions WHERE created_at < ?
				AND NOT (kind = 'undo' AND EXISTS (SELECT 1 FROM operator_decisions target WHERE target.decision_id = operator_decisions.supersedes)) ORDER BY created_at, rowid LIMIT ?)`, limit, plan.decisionCut, limit); err != nil {
				return err
			}
			sourceScope := "last_report_at IS NOT NULL AND last_report_at < ?\n\t\t\t\tAND NOT EXISTS (SELECT 1 FROM reported_subjects WHERE reported_subjects.source_scope = report_sources.source_scope)"
			sourceArgs := []any{plan.sourceCut}
			if plan.activeSource != "" {
				sourceScope += " AND source_scope <> ?"
				sourceArgs = append(sourceArgs, plan.activeSource)
			}
			limit = deleteLimit()
			sourceArgs = append(sourceArgs, limit)
			if err := take("report_sources", plan.sourceCut, `DELETE FROM report_sources WHERE rowid IN (SELECT rowid FROM report_sources WHERE `+sourceScope+` ORDER BY last_report_at, rowid LIMIT ?)`, limit, sourceArgs...); err != nil {
				return err
			}
			if !plan.skipTokenExpiry {
				// A pin committed after discovery can retain an aged
				// briefing whose tokens never entered the scratch set,
				// so a newer briefing-subject decision defers expiry
				// exactly like a newer briefing: the next run
				// rediscovers with the pin included.
				limit = deleteLimit()
				if err := take("pseudonym_map", plan.tokenCut, `DELETE FROM pseudonym_map WHERE rowid IN (SELECT rowid FROM pseudonym_map WHERE last_used_at < ?
					AND NOT EXISTS (SELECT 1 FROM purge_protected_tokens WHERE purge_protected_tokens.token = pseudonym_map.token)
					AND NOT EXISTS (SELECT 1 FROM briefings WHERE rowid > ?)
					AND NOT EXISTS (SELECT 1 FROM operator_decisions WHERE subject_kind = 'briefing' AND created_at >= ? AND rowid > ?) ORDER BY last_used_at, rowid LIMIT ?)`, limit, plan.tokenCut, plan.tokenFence, plan.decisionCut, plan.tokenDecFence, limit); err != nil {
					return err
				}
			}
		}
		if plan.runID != 0 && report.total() > 0 {
			// Persist this batch atomically with its deletions: a crash
			// leaves completed batches attributed instead of only the
			// final totals. This consumes the ledger row reserved before
			// delete limits were chosen, so no second reservation is
			// taken here: a batch filled exactly to capacity still
			// commits instead of rolling back.
			report.Bookkeeping++
			if err := addPurgeRunCounts(tx, plan.runID, report); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return PurgeReport{}, false, err
	}
	return report, report.total() > 0, nil
}

// deleteBounded deletes the rows matching stmt, which must carry its own
// ORDER BY age-column, rowid LIMIT ? tail: SQLite rejects a bare LIMIT
// without ORDER BY, and the tail keeps every batch deterministic and
// oldest-first. Only directly deleted rows surface in RowsAffected;
// cascades are kept empty by the childless gates above, so the count
// is exact.
func deleteBounded(tx *sql.Tx, stmt string, args ...any) (int64, error) {
	res, err := tx.Exec(stmt, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// protectedTokenRe matches issued tokens (IP_1, MAC_2, HOST_3, NET_4)
// named inside retained briefing text. priority_reason and
// model_raw_json are stored tokenized, so they must be scanned too:
// deleting a token they name would corrupt the retained rendering.
var protectedTokenRe = regexp.MustCompile(`\b(?:IP|MAC|HOST|NET)_[0-9]+\b`)

// protectedTokenScanPage bounds one reference-discovery read: one
// briefing row is decoded at a time and only the page's distinct
// references are retained before the transaction commits.
const protectedTokenScanPage = 100

// protectedTokenWritePage bounds one scratch-write transaction to the
// shared maintenance row budget.
const protectedTokenWritePage = maxPurgeRowsPerTxn

// protectedTokenClearPage bounds one scratch-clear transaction to the
// shared maintenance row budget.
const protectedTokenClearPage = maxPurgeRowsPerTxn

// protectedRefillPasses bounds catch-up discovery when retained
// briefings are inserted concurrently with a purge. Discovery remains
// complete for a quiescent database; sustained inserts fail closed and
// leave token deletions for a later run.
const protectedRefillPasses = 5

// retainedBriefing matches every briefing purge must keep: inside the
// retention window, or pinned by a retained decision at any age. The
// pin predicate mirrors the stageBriefings delete gate exactly —
// discovery and deletion must agree on what is retained, or pinned
// traces lose their restoration mappings while their briefing stands.
const retainedBriefing = `(created_at >= ? OR EXISTS (SELECT 1 FROM operator_decisions d
	WHERE d.subject_kind = 'briefing' AND d.subject_id = briefings.briefing_id AND d.created_at >= ?))`

// refillProtectedTokens rebuilds the per-run protected-token scratch
// set from every retained briefing. A stale scratch row only
// over-protects, so the rebuild runs before any deletion batch;
// per-batch exclusion then keeps referenced tokens alive while expiry
// deletes the rest. Decision-pinned briefings join at any age: their
// traces' mappings retire with them, never before. It returns the
// highest briefing rowid included and the highest briefing-subject
// decision rowid seen, fencing token deletes against briefings
// inserted and pins committed during discovery.
func (s *Store) refillProtectedTokens(ctx context.Context, briefingCut, decisionCut int64) (int64, int64, error) {
	return s.refillProtectedTokensPaged(ctx, briefingCut, decisionCut, protectedTokenScanPage, protectedTokenWritePage, protectedTokenClearPage, protectedRefillPasses)
}

func (s *Store) refillProtectedTokensPaged(ctx context.Context, briefingCut, decisionCut int64, scanPage, writePage, clearPage, maxPasses int) (int64, int64, error) {
	if scanPage <= 0 || writePage <= 0 || clearPage <= 0 || maxPasses <= 0 {
		return 0, 0, fmt.Errorf("protected token page sizes must be positive")
	}
	// Plain reads stay outside the writer; scratch writes use bounded
	// transactions. A briefing inserted during discovery is handled by
	// catch-up passes and, ultimately, by the delete-time rowid fence.
	// A decision appended mid-pass can pin an already-scanned old
	// briefing (rowid order cannot catch that), so the decisions table
	// is fenced too: when it advances, the next pass rescans from the
	// start. Rescans are idempotent (INSERT OR IGNORE) and bounded by
	// maxPasses like every other catch-up.
	var fence, decFence int64
	if err := s.clearProtectedTokensPaged(ctx, clearPage); err != nil {
		return 0, 0, err
	}
	for pass := 1; pass <= maxPasses; pass++ {
		var snapshot, decSnapshot int64
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), 0) FROM briefings WHERE `+retainedBriefing, briefingCut, decisionCut).Scan(&snapshot); err != nil {
			return 0, 0, err
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), 0) FROM operator_decisions WHERE subject_kind = 'briefing' AND created_at >= ?`, decisionCut).Scan(&decSnapshot); err != nil {
			return 0, 0, err
		}
		// Pins standing at pass start are covered by this pass's own
		// scan; only pins appended mid-pass force a rescan.
		decFence = decSnapshot
		if err := s.refillProtectedTokensThrough(ctx, briefingCut, decisionCut, fence, snapshot, scanPage, writePage); err != nil {
			return 0, 0, err
		}
		fence = snapshot
		var current, decCurrent int64
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), 0) FROM briefings WHERE `+retainedBriefing, briefingCut, decisionCut).Scan(&current); err != nil {
			return 0, 0, err
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), 0) FROM operator_decisions WHERE subject_kind = 'briefing' AND created_at >= ?`, decisionCut).Scan(&decCurrent); err != nil {
			return 0, 0, err
		}
		if current <= fence && decCurrent <= decFence {
			return fence, decFence, nil
		}
		if decCurrent > decFence {
			// New pins may cover already-scanned rows: rescan.
			decFence, fence = decCurrent, 0
		}
	}
	return fence, decFence, fmt.Errorf("protected token references changed during discovery; retry purge")
}

func (s *Store) refillProtectedTokensThrough(ctx context.Context, briefingCut, decisionCut, afterRowID, throughRowID int64, scanPage, writePage int) error {
	lastRowID := afterRowID
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.db.QueryContext(ctx, `SELECT rowid, COALESCE(model_raw_json, ''), COALESCE(priority_reason, '') FROM briefings
			WHERE `+retainedBriefing+` AND rowid > ? AND rowid <= ? ORDER BY rowid LIMIT ?`,
			briefingCut, decisionCut, lastRowID, throughRowID, scanPage)
		if err != nil {
			return err
		}
		seen := map[string]struct{}{}
		var tokens []string
		scanned := int64(0)
		pageStart := lastRowID
		for rows.Next() {
			var rowID int64
			var rawJSON, reason string
			if err := rows.Scan(&rowID, &rawJSON, &reason); err != nil {
				rows.Close()
				return err
			}
			tokens = appendProtectedTokens(seen, tokens, rawJSON)
			tokens = appendProtectedTokens(seen, tokens, reason)
			lastRowID = rowID
			scanned++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		// Trace envelopes are tokenized audit for the same window:
		// validated args name issued tokens and results echo them, so
		// their mappings need the same protection. Executions are
		// loop-bounded far below the page multiple, so the cap is a
		// backstop, not a silent occurrence drop.
		trows, err := s.db.QueryContext(ctx, `SELECT args_json, result_json FROM agent_traces
			WHERE briefing_id IN (SELECT briefing_id FROM briefings WHERE `+retainedBriefing+` AND rowid > ? AND rowid <= ?)
			ORDER BY briefing_id, step LIMIT ?`,
			briefingCut, decisionCut, pageStart, lastRowID, scanPage*8)
		if err != nil {
			return err
		}
		for trows.Next() {
			var argsJSON, resultJSON string
			if err := trows.Scan(&argsJSON, &resultJSON); err != nil {
				trows.Close()
				return err
			}
			tokens = appendProtectedTokens(seen, tokens, argsJSON)
			tokens = appendProtectedTokens(seen, tokens, resultJSON)
		}
		if err := trows.Err(); err != nil {
			trows.Close()
			return err
		}
		trows.Close()
		if len(tokens) > 0 {
			if err := s.storeProtectedTokensPaged(ctx, tokens, writePage); err != nil {
				return err
			}
		}
		if scanned < int64(scanPage) {
			return nil
		}
	}
}

// appendProtectedTokens collects every distinct token occurrence. The
// caller bounds memory with paged reads and writes; no occurrence cap
// may silently drop a later retained reference.
func appendProtectedTokens(seen map[string]struct{}, tokens []string, text string) []string {
	for _, match := range protectedTokenRe.FindAllString(text, -1) {
		if _, ok := seen[match]; !ok {
			seen[match] = struct{}{}
			tokens = append(tokens, match)
		}
	}
	return tokens
}

func (s *Store) clearProtectedTokensPaged(ctx context.Context, clearPage int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.yieldOnPressure(ctx)
		var deleted int64
		err := s.write(ctx, func(tx *sql.Tx) error {
			res, err := tx.Exec(`DELETE FROM purge_protected_tokens WHERE rowid IN (SELECT rowid FROM purge_protected_tokens ORDER BY rowid LIMIT ?)`, clearPage)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			deleted = n
			return nil
		})
		if err != nil {
			return err
		}
		if deleted < int64(clearPage) {
			return nil
		}
	}
}

func (s *Store) storeProtectedTokensPaged(ctx context.Context, tokens []string, writePage int) error {
	for start := 0; start < len(tokens); start += writePage {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.yieldOnPressure(ctx)
		end := start + writePage
		if end > len(tokens) {
			end = len(tokens)
		}
		chunk := tokens[start:end]
		if err := s.write(ctx, func(tx *sql.Tx) error {
			for _, token := range chunk {
				if _, err := tx.Exec(`INSERT OR IGNORE INTO purge_protected_tokens (token) VALUES (?)`, token); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// recordPurgeState persists per-table progress in the same transaction
// as the deletions it describes. The rows carry cumulative totals for
// Step 20 health, not resume checkpoints: each batch reselects its own
// remaining work, so a restart deletes whatever is still eligible.
func recordPurgeState(tx *sql.Tx, table string, now, cutoff, added int64) error {
	_, err := tx.Exec(`INSERT INTO purge_state (table_name, last_run_at, last_cutoff, rows_deleted_total)
		VALUES (?, ?, ?, ?) ON CONFLICT(table_name) DO UPDATE SET
		last_run_at = excluded.last_run_at, last_cutoff = excluded.last_cutoff,
		rows_deleted_total = rows_deleted_total + excluded.rows_deleted_total`,
		table, now, cutoff, added)
	return err
}

// addPurgeRunCounts adds one batch's counts to its ledger row in the
// same transaction. Interim attribution survives a crash even when the
// run never reaches terminal finalization.
func addPurgeRunCounts(tx *sql.Tx, runID int64, report PurgeReport) error {
	_, err := tx.Exec(`UPDATE purge_runs SET
		briefing_links_deleted = briefing_links_deleted + ?,
		briefings_deleted = briefings_deleted + ?,
		traces_deleted = traces_deleted + ?,
		groups_deleted = groups_deleted + ?,
		group_links_deleted = group_links_deleted + ?,
		group_events_deleted = group_events_deleted + ?,
		ports_deleted = ports_deleted + ?,
		observations_deleted = observations_deleted + ?,
		progress_deleted = progress_deleted + ?,
		raw_events_deleted = raw_events_deleted + ?,
		subjects_deleted = subjects_deleted + ?,
		sources_deleted = sources_deleted + ?,
		pseudonyms_deleted = pseudonyms_deleted + ?,
		decisions_deleted = decisions_deleted + ?,
		bookkeeping_writes = bookkeeping_writes + ?
		WHERE run_id = ?`,
		report.BriefingLinks, report.Briefings, report.Traces, report.Groups, report.GroupLinks,
		report.GroupEvents,
		report.Ports, report.Observations, report.Progress, report.RawEvents,
		report.Subjects, report.Sources, report.Pseudonyms, report.Decisions, report.Bookkeeping,
		runID)
	return err
}

// PurgeOnce drains all eligible retention work in bounded transactions,
// yielding while writer pressure passes half the queue cap so retention
// never starves ingest. It returns the summed per-table counts and
// records a run row: completed when the drain finishes, interrupted
// when ctx ends mid-run, failed when a batch errors. Partial batches
// commit before the failure, so a later run safely deletes the rest;
// only cumulative counts transfer, and the stored cutoff must not be
// read as a resume position.
//
// The run ledger opens before reference discovery, so a discovery
// failure is recorded instead of escaping run history. When discovery
// cannot establish completeness, token expiry is skipped while every
// independently safe stage still drains; the skip is noted in the run
// and surfaced to the caller, and a later run retries expiry.
func (s *Store) PurgeOnce(ctx context.Context, settings PurgeSettings, now time.Time) (PurgeReport, error) {
	rawCut := nanos(now.Add(-settings.BriefingRetention))
	plan := purgePlan{
		stages:       stageAll,
		briefingCut:  rawCut,
		rawCut:       rawCut,
		failedCut:    min(rawCut, nanos(now.Add(-failedEventFloor))),
		subjectCut:   nanos(now.Add(-settings.SubjectRetention)),
		sourceCut:    nanos(now.Add(-heartbeatRetention)),
		tokenCut:     rawCut,
		decisionCut:  nanos(now.Add(-decisionRetentionOf(settings))),
		activeSource: settings.ActiveSource,
	}
	runID, err := s.beginPurgeRun(ctx, nanos(now))
	if err != nil {
		return PurgeReport{}, err
	}
	plan.runID = runID
	briefFence, decFence, derr := s.refillProtectedTokens(ctx, plan.briefingCut, plan.decisionCut)
	if derr != nil {
		plan.skipTokenExpiry = true
	} else {
		plan.tokenFence, plan.tokenDecFence = briefFence, decFence
	}
	status, errText, runErr, total := s.runPurgeBatches(ctx, now, plan, PurgeReport{})
	if derr != nil {
		note := "token discovery incomplete; token expiry skipped: " + SanitizeErrorText(derr.Error())
		if runErr != nil {
			errText = errText + "; " + note
			runErr = errors.Join(runErr, derr)
		} else {
			errText = note
			runErr = derr
		}
	}
	return total, s.finishPurgeRun(ctx, runID, status, errText, runErr)
}

// drainPlan runs purgeBatch until no eligible row remains. Every run is
// recorded in purge_runs with its status, duration and per-batch plus
// final per-table counts, so Step 20 health can tell a completed run
// from one that committed batches and then failed or was interrupted.
// Batch counts are persisted atomically with their deletions; terminal
// finalization uses detached bounded work so cancellation still records
// the outcome.
func (s *Store) drainPlan(ctx context.Context, now time.Time, plan purgePlan) (PurgeReport, error) {
	runID, err := s.beginPurgeRun(ctx, nanos(now))
	if err != nil {
		return PurgeReport{}, err
	}
	plan.runID = runID
	status, errText, runErr, total := s.runPurgeBatches(ctx, now, plan, PurgeReport{})
	return total, s.finishPurgeRun(ctx, runID, status, errText, runErr)
}

func (s *Store) runPurgeBatches(ctx context.Context, now time.Time, plan purgePlan, total PurgeReport) (string, string, error, PurgeReport) {
	for {
		if err := ctx.Err(); err != nil {
			return "interrupted", SanitizeErrorText(err.Error()), err, total
		}
		s.yieldOnPressure(ctx)
		if err := ctx.Err(); err != nil {
			return "interrupted", SanitizeErrorText(err.Error()), err, total
		}
		report, progress, err := s.purgeBatch(ctx, now, plan)
		if err != nil {
			status, statusErr := batchRunStatus(ctx, err)
			return status, SanitizeErrorText(statusErr.Error()), statusErr, total
		}
		total.add(report)
		if !progress {
			return "completed", "", nil, total
		}
	}
}

// batchRunStatus maps a purgeBatch error to its terminal run status. A
// cancelled context means interrupted even when the batch itself
// reports the failure: the store permits cancellation to race a
// successful commit, so the batch may have committed while its caller
// never collected the report.
func batchRunStatus(ctx context.Context, err error) (string, error) {
	if ctx.Err() != nil {
		return "interrupted", ctx.Err()
	}
	return "failed", err
}

// beginPurgeRun inserts a running ledger row and prunes history beyond
// the newest 30 runs. A previous process that crashed between begin and
// finish leaves a running row; reconciling it here bounds the outage
// window before the new run starts deleting.
func (s *Store) beginPurgeRun(ctx context.Context, startedAt int64) (int64, error) {
	var runID int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO purge_runs (started_at) VALUES (?)`, startedAt)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		runID = id
		_, err = tx.Exec(`UPDATE purge_runs SET finished_at = ?, status = 'interrupted', error = 'previous process ended before finalizing run'
			WHERE status = 'running' AND run_id <> ?`, startedAt, runID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM purge_runs WHERE run_id NOT IN (SELECT run_id FROM purge_runs ORDER BY run_id DESC LIMIT 30)`)
		return err
	})
	if err != nil {
		return 0, err
	}
	return runID, nil
}

// finishPurgeRun stamps the run row with its terminal status, finish
// time and error text. It never rewrites the per-table counters:
// batches persist their counts atomically with their deletions, and
// those committed counters stay authoritative even when cancellation
// raced the commit and the caller-side totals are short. It runs
// detached from the caller's cancellation under a shutdown bound: an
// interrupted run still needs its ledger outcome, but finalization
// must not wait indefinitely.
func (s *Store) finishPurgeRun(ctx context.Context, runID int64, status, errText string, runErr error) error {
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), purgeLedgerTimeout)
	defer cancel()
	err := s.write(finalizeCtx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE purge_runs SET finished_at = ?, status = ?, error = ? WHERE run_id = ?`,
			nanos(s.now()), status, errText, runID)
		return err
	})
	if err != nil {
		if runErr != nil {
			return errors.Join(runErr, err)
		}
		return err
	}
	return runErr
}

// purgeRun describes one ledger row for Step 20 health.
type purgeRun struct {
	runID      int64
	startedAt  int64
	finishedAt int64
	status     string
	errText    string
	total      PurgeReport
}

// LatestPurgeRun returns the newest ledger row, or sql.ErrNoRows when no
// purge has run yet.
func (s *Store) LatestPurgeRun(ctx context.Context) (purgeRun, error) {
	var run purgeRun
	err := s.db.QueryRowContext(ctx, `SELECT run_id, started_at, finished_at, status, error,
		briefing_links_deleted, briefings_deleted, traces_deleted, groups_deleted, group_links_deleted,
		group_events_deleted,
		ports_deleted, observations_deleted, progress_deleted, raw_events_deleted,
		subjects_deleted, sources_deleted, pseudonyms_deleted, decisions_deleted, bookkeeping_writes
		FROM purge_runs ORDER BY run_id DESC LIMIT 1`).Scan(
		&run.runID, &run.startedAt, &run.finishedAt, &run.status, &run.errText,
		&run.total.BriefingLinks, &run.total.Briefings, &run.total.Traces, &run.total.Groups, &run.total.GroupLinks,
		&run.total.GroupEvents,
		&run.total.Ports, &run.total.Observations, &run.total.Progress, &run.total.RawEvents,
		&run.total.Subjects, &run.total.Sources, &run.total.Pseudonyms, &run.total.Decisions, &run.total.Bookkeeping)
	if err != nil {
		return purgeRun{}, err
	}
	return run, nil
}

// yieldOnPressure sleeps while the writer backlog passes half the queue
// cap. Advisory only: a saturated queue still fails fast at enqueue,
// this only spaces retention batches behind live traffic.
func (s *Store) yieldOnPressure(ctx context.Context) {
	cap := s.QueueCap()
	if cap <= 0 {
		return
	}
	for s.QueueDepth() > cap/2 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// PurgeBriefings deletes briefings (and their emptied done groups) created
// before cutoff. Separate from raw-event retention: briefing history may
// outlive the raw log. Batches stay within the shared row budget and
// resume until no eligible row remains.
func (s *Store) PurgeBriefings(ctx context.Context, now, cutoff time.Time) (int64, error) {
	if now.Sub(cutoff) < briefingPurgeFloor {
		return 0, fmt.Errorf("briefing purge cutoff must be at least %s old", briefingPurgeFloor)
	}
	plan := purgePlan{stages: stageBriefings, briefingCut: nanos(cutoff)}
	report, err := s.drainPlan(ctx, now, plan)
	if err != nil {
		return report.Briefings, err
	}
	return report.Briefings, nil
}

// PurgeRawEvents deletes raw events received before cutoff, but only those
// whose group is done (or that belong to no group). Failed groups' events
// use a longer floor (failedEventFloor, 7 days) and the failed row is
// deleted once its events are gone. Events pending projection or backing
// a retained briefing are protected, as are their observations, ports
// and progress rows, which are deleted in the same bounded batches.
// Briefings and groups keep their own retention (PurgeBriefings). The
// cutoff must be at least minAge old, so replay/sweep windows are never
// weakened.
func (s *Store) PurgeRawEvents(ctx context.Context, now, cutoff time.Time, minAge time.Duration) (int64, error) {
	if minAge < purgeFloor {
		minAge = purgeFloor
	}
	if now.Sub(cutoff) < minAge {
		return 0, fmt.Errorf("purge cutoff must be at least %s old", minAge)
	}
	plan := purgePlan{
		stages:    stageEvents,
		rawCut:    nanos(cutoff),
		failedCut: min(nanos(cutoff), nanos(now.Add(-failedEventFloor))),
	}
	report, err := s.drainPlan(ctx, now, plan)
	if err != nil {
		return report.RawEvents, err
	}
	return report.RawEvents, nil
}
