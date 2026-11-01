package store

import (
	"context"
	"database/sql"
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

// maxPurgeRowsPerTxn caps committed deletions across all statements in
// one purge transaction, including relation and progress rows.
const maxPurgeRowsPerTxn = 200

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
	// ActiveSource names the configured SOURCE_ID. A quiet-but-live
	// registration is never retired by inactivity alone (Designv9 §4.6);
	// when empty, source cleanup keeps its legacy behavior for tests.
	ActiveSource string
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
	Groups        int64
	GroupLinks    int64
	Ports         int64
	Observations  int64
	Progress      int64 // projection_progress + projection_pending rows
	RawEvents     int64
	Subjects      int64
	Sources       int64
	Pseudonyms    int64
	Bookkeeping   int64 // purge_state upserts sharing the row budget
}

func (r *PurgeReport) add(o PurgeReport) {
	r.BriefingLinks += o.BriefingLinks
	r.Briefings += o.Briefings
	r.Groups += o.Groups
	r.GroupLinks += o.GroupLinks
	r.Ports += o.Ports
	r.Observations += o.Observations
	r.Progress += o.Progress
	r.RawEvents += o.RawEvents
	r.Subjects += o.Subjects
	r.Sources += o.Sources
	r.Pseudonyms += o.Pseudonyms
	r.Bookkeeping += o.Bookkeeping
}

func (r PurgeReport) total() int64 {
	return r.BriefingLinks + r.Briefings + r.Groups + r.GroupLinks + r.Ports +
		r.Observations + r.Progress + r.RawEvents + r.Subjects + r.Sources + r.Pseudonyms
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
	// activeSource names the configured SOURCE_ID. A quiet-but-live
	// registration is never retired by inactivity alone (Designv9 §4.6).
	activeSource string
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
		take := func(table string, cutoff int64, stmt string, args ...any) error {
			if remaining <= 0 {
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
			case "briefings":
				report.Briefings += n
			case "active_groups":
				report.Groups += n
			case "group_subjects":
				report.GroupLinks += n
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
			}
			return nil
		}
		if plan.stages&stageBriefings != 0 {
			oldBriefings := `briefing_id IN (SELECT briefing_id FROM briefings WHERE created_at < ? ORDER BY created_at, rowid LIMIT ?)`
			if err := take("briefing_subjects", plan.briefingCut, `DELETE FROM briefing_subjects WHERE rowid IN (SELECT rowid FROM briefing_subjects WHERE `+oldBriefings+` ORDER BY rowid LIMIT ?)`, plan.briefingCut, remaining, remaining); err != nil {
				return err
			}
			if err := take("briefings", plan.briefingCut, `DELETE FROM briefings WHERE rowid IN (SELECT rowid FROM briefings WHERE created_at < ? AND NOT EXISTS (SELECT 1 FROM briefing_subjects WHERE briefing_subjects.briefing_id = briefings.briefing_id) ORDER BY created_at, rowid LIMIT ?)`, plan.briefingCut, remaining); err != nil {
				return err
			}
			deletableDone := `status = 'done'
				AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id)
				AND NOT EXISTS (SELECT 1 FROM group_events WHERE group_events.group_id = active_groups.group_id)`
			if err := take("group_subjects", plan.briefingCut, `DELETE FROM group_subjects WHERE rowid IN (SELECT rowid FROM group_subjects WHERE group_id IN (SELECT group_id FROM active_groups WHERE `+deletableDone+`) ORDER BY rowid LIMIT ?)`, remaining); err != nil {
				return err
			}
			if err := take("active_groups", plan.briefingCut, `DELETE FROM active_groups WHERE rowid IN (SELECT rowid FROM active_groups WHERE `+deletableDone+`
				AND NOT EXISTS (SELECT 1 FROM group_subjects WHERE group_subjects.group_id = active_groups.group_id) ORDER BY opened_at, rowid LIMIT ?)`, remaining); err != nil {
				return err
			}
		}
		if plan.stages&stageEvents != 0 {
			if err := take("reported_ports", plan.rawCut, `DELETE FROM reported_ports WHERE rowid IN (SELECT rowid FROM reported_ports WHERE observation_id IN (SELECT observation_id FROM reported_observations WHERE `+eligibleEvents+`) ORDER BY rowid LIMIT ?)`, plan.rawCut, plan.failedCut, plan.rawCut, remaining); err != nil {
				return err
			}
			if err := take("reported_observations", plan.rawCut, `DELETE FROM reported_observations WHERE rowid IN (SELECT observation_id FROM reported_observations WHERE `+eligibleEvents+` AND NOT EXISTS (SELECT 1 FROM reported_ports WHERE reported_ports.observation_id = reported_observations.observation_id) ORDER BY observation_id LIMIT ?)`, plan.rawCut, plan.failedCut, plan.rawCut, remaining); err != nil {
				return err
			}
			if err := take("projection_progress", plan.rawCut, `DELETE FROM projection_progress WHERE rowid IN (SELECT rowid FROM projection_progress WHERE `+eligibleEvents+` ORDER BY rowid LIMIT ?)`, plan.rawCut, plan.failedCut, plan.rawCut, remaining); err != nil {
				return err
			}
			if err := take("projection_pending", plan.rawCut, `DELETE FROM projection_pending WHERE rowid IN (SELECT rowid FROM projection_pending WHERE `+eligibleEvents+` ORDER BY rowid LIMIT ?)`, plan.rawCut, plan.failedCut, plan.rawCut, remaining); err != nil {
				return err
			}
			if err := take("group_events", plan.rawCut, `DELETE FROM group_events WHERE rowid IN (SELECT rowid FROM group_events WHERE group_id IN (SELECT group_id FROM active_groups WHERE status IN ('done','failed')) AND `+eligibleEvents+` ORDER BY rowid LIMIT ?)`, plan.rawCut, plan.failedCut, plan.rawCut, remaining); err != nil {
				return err
			}
			if err := take("raw_events", plan.rawCut, `DELETE FROM raw_events WHERE rowid IN (SELECT rowid FROM raw_events WHERE received_at < ?
				AND NOT EXISTS (SELECT 1 FROM group_events WHERE group_events.event_id = raw_events.event_id)
				AND NOT EXISTS (SELECT 1 FROM reported_observations WHERE reported_observations.event_id = raw_events.event_id)
				AND NOT EXISTS (SELECT 1 FROM projection_pending WHERE projection_pending.event_id = raw_events.event_id)
				AND NOT EXISTS (SELECT 1 FROM projection_progress WHERE projection_progress.event_id = raw_events.event_id) ORDER BY received_at, rowid LIMIT ?)`, plan.rawCut, remaining); err != nil {
				return err
			}
			failedGroups := `group_id IN (SELECT group_id FROM active_groups WHERE status = 'failed' AND deadline_at < ?
				AND NOT EXISTS (SELECT 1 FROM group_events WHERE group_events.group_id = active_groups.group_id)
				AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id))`
			if err := take("group_subjects", plan.failedCut, `DELETE FROM group_subjects WHERE rowid IN (SELECT rowid FROM group_subjects WHERE `+failedGroups+` ORDER BY rowid LIMIT ?)`, plan.failedCut, remaining); err != nil {
				return err
			}
			if err := take("active_groups", plan.failedCut, `DELETE FROM active_groups WHERE rowid IN (SELECT rowid FROM active_groups WHERE status = 'failed' AND deadline_at < ?
				AND NOT EXISTS (SELECT 1 FROM group_events WHERE group_events.group_id = active_groups.group_id)
				AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id)
				AND NOT EXISTS (SELECT 1 FROM group_subjects WHERE group_subjects.group_id = active_groups.group_id) ORDER BY deadline_at, rowid LIMIT ?)`, plan.failedCut, remaining); err != nil {
				return err
			}
		}
		if plan.stages&stageSubjects != 0 {
			if err := take("reported_subjects", plan.subjectCut, `DELETE FROM reported_subjects WHERE rowid IN (SELECT rowid FROM reported_subjects WHERE last_report_at < ?
				AND NOT EXISTS (SELECT 1 FROM reported_observations WHERE reported_observations.profile_id = reported_subjects.profile_id)
				AND NOT EXISTS (SELECT 1 FROM group_subjects WHERE group_subjects.profile_id = reported_subjects.profile_id)
				AND NOT EXISTS (SELECT 1 FROM briefing_subjects WHERE briefing_subjects.profile_id = reported_subjects.profile_id) ORDER BY last_report_at, rowid LIMIT ?)`, plan.subjectCut, remaining); err != nil {
				return err
			}
			sourceScope := "last_report_at IS NOT NULL AND last_report_at < ?\n\t\t\t\tAND NOT EXISTS (SELECT 1 FROM reported_subjects WHERE reported_subjects.source_scope = report_sources.source_scope)"
			sourceArgs := []any{plan.sourceCut}
			if plan.activeSource != "" {
				sourceScope += " AND source_scope <> ?"
				sourceArgs = append(sourceArgs, plan.activeSource)
			}
			sourceArgs = append(sourceArgs, remaining)
			if err := take("report_sources", plan.sourceCut, `DELETE FROM report_sources WHERE rowid IN (SELECT rowid FROM report_sources WHERE `+sourceScope+` ORDER BY last_report_at, rowid LIMIT ?)`, sourceArgs...); err != nil {
				return err
			}
			if err := take("pseudonym_map", plan.tokenCut, `DELETE FROM pseudonym_map WHERE rowid IN (SELECT rowid FROM pseudonym_map WHERE last_used_at < ?
				AND NOT EXISTS (SELECT 1 FROM purge_protected_tokens WHERE purge_protected_tokens.token = pseudonym_map.token) ORDER BY last_used_at, rowid LIMIT ?)`, plan.tokenCut, remaining); err != nil {
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

// maxProtectedRefsPerBriefing caps candidate references scanned from
// one briefing. Only distinct tokens insert, so the worst-case scratch
// refill per briefing is this many rows.
const maxProtectedRefsPerBriefing = 256

// refillProtectedTokens rebuilds the per-run protected-token scratch
// set from every retained briefing (created_at >= briefingCut). A
// stale scratch row only over-protects, so the rebuild runs once per
// PurgeOnce outside the batch budget; per-batch exclusion then keeps
// referenced tokens alive while expiry deletes the rest.
func (s *Store) refillProtectedTokens(ctx context.Context, briefingCut int64) error {
	type briefingText struct {
		rawJSON string
		reason  string
	}
	// Plain read outside the writer: the scan only collects text for
	// the scratch rebuild below and must not hold the write queue.
	rows, err := s.db.QueryContext(ctx, `SELECT model_raw_json, priority_reason FROM briefings WHERE created_at >= ?`, briefingCut)
	if err != nil {
		return err
	}
	var texts []briefingText
	for rows.Next() {
		var t briefingText
		if err := rows.Scan(&t.rawJSON, &t.reason); err != nil {
			rows.Close()
			return err
		}
		texts = append(texts, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM purge_protected_tokens`); err != nil {
			return err
		}
		seen := map[string]bool{}
		insert := func(s string) error {
			matches := protectedTokenRe.FindAllString(s, maxProtectedRefsPerBriefing+1)
			if len(matches) > maxProtectedRefsPerBriefing {
				matches = matches[:maxProtectedRefsPerBriefing]
			}
			for _, m := range matches {
				if seen[m] {
					continue
				}
				seen[m] = true
				if _, err := tx.Exec(`INSERT OR IGNORE INTO purge_protected_tokens (token) VALUES (?)`, m); err != nil {
					return err
				}
			}
			return nil
		}
		for _, t := range texts {
			if err := insert(t.rawJSON); err != nil {
				return err
			}
			if err := insert(t.reason); err != nil {
				return err
			}
		}
		return nil
	})
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

// PurgeOnce drains all eligible retention work in bounded transactions,
// yielding while writer pressure passes half the queue cap so retention
// never starves ingest. It returns the summed per-table counts and
// records a run row: completed when the drain finishes, interrupted
// when ctx ends mid-run, failed when a batch errors. Partial batches
// commit before the failure, so a later run safely deletes the rest;
// only cumulative counts transfer, and the stored cutoff must not be
// read as a resume position.
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
		activeSource: settings.ActiveSource,
	}
	if err := s.refillProtectedTokens(ctx, plan.briefingCut); err != nil {
		return PurgeReport{}, err
	}
	return s.drainPlan(ctx, now, plan)
}

// drainPlan runs purgeBatch until no eligible row remains. Every run is
// recorded in purge_runs with its status, duration and per-table
// counts, so Step 20 health can tell a completed run from one that
// committed batches and then failed or was interrupted.
func (s *Store) drainPlan(ctx context.Context, now time.Time, plan purgePlan) (PurgeReport, error) {
	runID, err := s.beginPurgeRun(ctx, nanos(now))
	if err != nil {
		return PurgeReport{}, err
	}
	var total PurgeReport
	finish := func(status, errText string, runErr error) (PurgeReport, error) {
		if ferr := s.finishPurgeRun(ctx, runID, status, errText, total); ferr != nil && runErr == nil {
			return total, ferr
		}
		return total, runErr
	}
	for {
		if err := ctx.Err(); err != nil {
			return finish("interrupted", SanitizeErrorText(err.Error()), err)
		}
		s.yieldOnPressure(ctx)
		if err := ctx.Err(); err != nil {
			return finish("interrupted", SanitizeErrorText(err.Error()), err)
		}
		report, progress, err := s.purgeBatch(ctx, now, plan)
		if err != nil {
			return finish("failed", SanitizeErrorText(err.Error()), err)
		}
		total.add(report)
		if !progress {
			return finish("completed", "", nil)
		}
	}
}

// beginPurgeRun inserts a running ledger row and prunes history beyond
// the newest 30 runs. A crash between begin and finish leaves a
// running row whose started_at still bounds the outage window.
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
		_, err = tx.Exec(`DELETE FROM purge_runs WHERE run_id NOT IN (SELECT run_id FROM purge_runs ORDER BY run_id DESC LIMIT 30)`)
		return err
	})
	if err != nil {
		return 0, err
	}
	return runID, nil
}

// finishPurgeRun stamps the run row with its terminal status, finish
// time and cumulative counts.
func (s *Store) finishPurgeRun(ctx context.Context, runID int64, status, errText string, total PurgeReport) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE purge_runs SET finished_at = ?, status = ?, error = ?,
			briefing_links_deleted = ?, briefings_deleted = ?, groups_deleted = ?, group_links_deleted = ?,
			ports_deleted = ?, observations_deleted = ?, progress_deleted = ?, raw_events_deleted = ?,
			subjects_deleted = ?, sources_deleted = ?, pseudonyms_deleted = ?, bookkeeping_writes = ?
			WHERE run_id = ?`,
			nanos(s.now()), status, errText,
			total.BriefingLinks, total.Briefings, total.Groups, total.GroupLinks,
			total.Ports, total.Observations, total.Progress, total.RawEvents,
			total.Subjects, total.Sources, total.Pseudonyms, total.Bookkeeping,
			runID)
		return err
	})
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
		briefing_links_deleted, briefings_deleted, groups_deleted, group_links_deleted,
		ports_deleted, observations_deleted, progress_deleted, raw_events_deleted,
		subjects_deleted, sources_deleted, pseudonyms_deleted, bookkeeping_writes
		FROM purge_runs ORDER BY run_id DESC LIMIT 1`).Scan(
		&run.runID, &run.startedAt, &run.finishedAt, &run.status, &run.errText,
		&run.total.BriefingLinks, &run.total.Briefings, &run.total.Groups, &run.total.GroupLinks,
		&run.total.Ports, &run.total.Observations, &run.total.Progress, &run.total.RawEvents,
		&run.total.Subjects, &run.total.Sources, &run.total.Pseudonyms, &run.total.Bookkeeping)
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
