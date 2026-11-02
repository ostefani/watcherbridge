package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Step 18e retention: bounded, reference-aware purge with persisted
// progress. Every batch shares one ≤200-row transaction; pending
// projection and retained briefings protect their evidence; subjects,
// scopes and tokens expire only when unreferenced and old.

// A 250-event backlog drains over bounded batches: one batch commits
// at most 200 rows and reports progress, the next finishes the rest.
func TestPurgeBatchRespectsRowBudget(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	var gids []string
	for i := 0; i < 80; i++ {
		body := producerPayload(fmt.Sprintf("evt-b%d", i), "k", "host.appeared", `{"ip":"192.168.0.1"}`)
		gids = append(gids, persistReported(t, s, "lab", fmt.Sprintf("evt-b%d", i), "k", "host.appeared", body, old))
	}
	// Project so the backlog is eligible: unprojected events are
	// protected, not purged, however old. Brief every group: closed
	// but unbriefed groups pin their evidence until the briefing
	// exists, exactly as the worker guarantees.
	projectAll(t, s)
	if _, err := s.CloseDue(ctx, old.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, gid := range gids {
		taken, err := s.TakeGroup(ctx, gid)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.InsertBriefing(ctx, Briefing{
			BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
			Baseline: "low", Priority: "low", PriorityReason: "r",
			WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: old,
		}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	// Run with a live ledger ID: the budget covers deletions plus the
	// per-batch purge_state and run-ledger bookkeeping writes, exactly
	// as production drains.
	runID, err := s.beginPurgeRun(ctx, nanos(now))
	if err != nil {
		t.Fatal(err)
	}
	plan := purgePlan{
		stages:      stageAll,
		briefingCut: nanos(now.Add(-time.Hour)),
		rawCut:      nanos(now.Add(-time.Hour)),
		failedCut:   nanos(now.Add(-failedEventFloor)),
		subjectCut:  nanos(now.Add(-2 * time.Hour)),
		sourceCut:   nanos(now.Add(-heartbeatRetention)),
		tokenCut:    nanos(now.Add(-time.Hour)),
		runID:       runID,
	}
	batches := 0
	for {
		report, progress, err := s.purgeBatch(ctx, now, plan)
		if err != nil {
			t.Fatal(err)
		}
		if !progress {
			break
		}
		batches++
		if committed := report.total() + report.Bookkeeping; committed > maxPurgeRowsPerTxn {
			t.Fatalf("batch %d exceeded the row budget: %+v", batches, report)
		}
		if batches > 10 {
			t.Fatal("backlog must converge in a few bounded batches")
		}
	}
	if batches < 2 {
		t.Fatal("480-row backlog must need more than one 200-row batch")
	}
	var left int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM raw_events`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d events survive bounded batches", left)
	}
}

// An event still awaiting projection is never eligible, however old.
func TestPurgeProtectsPendingProjection(t *testing.T) {
	s := openTestStore(t)
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	body := producerPayload("evt-pend", "k", "host.appeared", `{"ip":"192.168.0.9"}`)
	persistReported(t, s, "lab", "evt-pend", "k", "host.appeared", body, old)
	now := time.Now().UTC()
	if _, err := s.PurgeOnce(context.Background(), PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 2 * time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM raw_events WHERE event_id = 'evt-pend'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatal("pending event must survive the purge")
	}
}

// Raw evidence of a retained briefing is protected until the briefing
// itself ages out; afterwards the same engine collects it.
func TestPurgeProtectsRetainedBriefingEvidence(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	body := producerPayload("evt-ev", "k", "host.appeared", `{"mac":"10:20:30:40:50:70","host_id":"mac:10:20:30:40:50:70","identity_source":"mac","identity_confidence":"high","ports":[{"number":22,"protocol":"tcp","state":"open","service":"ssh"}]}`)
	gid := persistReported(t, s, "lab", "evt-ev", "k", "host.appeared", body, old)
	projectAll(t, s)
	if _, err := s.CloseDue(ctx, old.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	// The briefing itself is fresh (retained) while its evidence is old:
	// eligibility must protect the event through the briefing join.
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: "r",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rep, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 2 * time.Hour}, now)
	if err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM raw_events WHERE event_id = 'evt-ev'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatalf("briefed evidence must survive, report %+v", rep)
	}
	var links int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM briefing_subjects WHERE briefing_id = 'brf-` + taken.GroupID + `'`).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links == 0 {
		t.Fatal("briefing must keep its copied subject links")
	}
	// Age the briefing out as well: evidence and relations follow.
	if _, err := s.db.Exec(`UPDATE briefings SET created_at = ?`, nanos(old)); err != nil {
		t.Fatal(err)
	}
	rep, err = s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 2 * time.Hour}, now)
	if err != nil {
		t.Fatal(err)
	}
	var briefings, events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM briefings`).Scan(&briefings); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM raw_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if briefings != 0 || events != 0 {
		t.Fatalf("aged briefing and evidence must go, report %+v", rep)
	}
	var state int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM purge_state WHERE rows_deleted_total > 0`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state == 0 {
		t.Fatal("purge_state must record per-table progress")
	}
}

// Tokens expire only when unused past the briefing horizon; a lookup
// hit refreshes usage, so steady-state devices keep their numbers.
func TestPseudonymExpiryFollowsLastUse(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	tok, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.9.9")
	if err != nil || tok == "" {
		t.Fatal(err)
	}
	stale := nanos(old)
	if _, err := s.db.Exec(`UPDATE pseudonym_map SET created_at = ?, last_used_at = ? WHERE token = ?`, stale, stale, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOrAssignPseudonyms(ctx, "ip", "192.168.9.9"); err != nil {
		t.Fatal(err)
	}
	var lastUsed int64
	if err := s.db.QueryRow(`SELECT last_used_at FROM pseudonym_map WHERE token = ?`, tok).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if lastUsed <= stale {
		t.Fatalf("lookup hit must refresh usage, got %d", lastUsed)
	}
	now := time.Now().UTC()
	if _, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 2 * time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pseudonym_map WHERE token = ?`, tok).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatal("recently used token must survive")
	}
	if _, err := s.db.Exec(`UPDATE pseudonym_map SET last_used_at = ?`, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 2 * time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pseudonym_map WHERE token = ?`, tok).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 0 {
		t.Fatal("token unused past the briefing horizon must expire")
	}
}

// An old event in an open (never briefed) group keeps its full context:
// the raw row, its observations, ports and projection markers all
// survive, and the report deletes nothing. Pre-fix code deleted the
// children while the group still needed them.
func TestPurgeProtectsUnbriefedGroupContext(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	body := producerPayload("evt-open", "k", "host.appeared", `{"mac":"10:20:30:40:50:71","host_id":"mac:10:20:30:40:50:71","identity_source":"mac","identity_confidence":"high","ports":[{"number":22,"protocol":"tcp","state":"open","service":"ssh"}]}`)
	persistReported(t, s, "lab", "evt-open", "k", "host.appeared", body, old)
	projectAll(t, s)
	// No CloseDue/TakeGroup: the group stays open and unbriefed.
	rep, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 90 * 24 * time.Hour}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rep.RawEvents != 0 || rep.Observations != 0 || rep.Ports != 0 || rep.Progress != 0 {
		t.Fatalf("unbriefed group context must be pinned: %+v", rep)
	}
	for table, want := range map[string]int{
		`raw_events WHERE event_id = 'evt-open'`:            1,
		`reported_observations WHERE event_id = 'evt-open'`: 1,
		`projection_progress WHERE event_id = 'evt-open'`:   1,
		`active_groups WHERE status = 'open'`:               1,
	} {
		var kept int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&kept); err != nil || kept != want {
			t.Fatalf("%s = %d, want %d (%v)", table, kept, want, err)
		}
	}
	var ports int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reported_ports WHERE observation_id IN (SELECT observation_id FROM reported_observations WHERE event_id = 'evt-open')`).Scan(&ports); err != nil || ports == 0 {
		t.Fatalf("unbriefed observation ports must survive: %d %v", ports, err)
	}
	run, err := s.LatestPurgeRun(ctx)
	if err != nil || run.status != "completed" {
		t.Fatalf("purge must ledger a completed run: %+v %v", run, err)
	}
}

// The run ledger records status, duration and cumulative counts; only
// the newest 30 rows survive. begin+finish round-trips carry an
// interrupted run with its error text, so Step 20 health can tell it
// apart from a completed one.
func TestPurgeRunLedgerAndPruning(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	settings := PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 90 * 24 * time.Hour}
	if _, err := s.PurgeOnce(ctx, settings, now); err != nil {
		t.Fatal(err)
	}
	run, err := s.LatestPurgeRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run.status != "completed" || run.finishedAt < run.startedAt || run.startedAt == 0 {
		t.Fatalf("completed run must carry a sane duration: %+v", run)
	}
	runID, err := s.beginPurgeRun(ctx, nanos(now))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.finishPurgeRun(ctx, runID, "interrupted", "test cancel", nil); err != nil {
		t.Fatal(err)
	}
	interrupted, err := s.LatestPurgeRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.status != "interrupted" || interrupted.errText != "test cancel" {
		t.Fatalf("interrupted run must keep status and error: %+v", interrupted)
	}
	for i := 0; i < 31; i++ {
		if _, err := s.PurgeOnce(ctx, settings, now); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM purge_runs`).Scan(&rows); err != nil || rows != 30 {
		t.Fatalf("ledger must prune to 30 rows, got %d (%v)", rows, err)
	}
}

// A cancelled run context must not prevent recording the terminal
// ledger state. Finalization uses bounded shutdown-safe work rather
// than the cancelled caller context.
func TestPurgeFinalizesCancelledRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	runID, err := s.beginPurgeRun(ctx, nanos(time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.finishPurgeRun(cancelled, runID, "interrupted", "test cancel", nil); err != nil {
		t.Fatalf("cancelled context must not block run finalization: %v", err)
	}
	run, err := s.LatestPurgeRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run.status != "interrupted" || run.errText != "test cancel" {
		t.Fatalf("cancelled run must keep status and error: %+v", run)
	}
}

// Cancellation between ledger begin and batch work records an
// interrupted run through detached finalization rather than leaving
// the running row behind.
func TestPurgeDrainRecordsInterruptedAfterBegin(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	runID, err := s.beginPurgeRun(ctx, nanos(now))
	if err != nil {
		t.Fatal(err)
	}
	plan := purgePlan{stages: stageAll, runID: runID}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	status, errText, runErr, _ := s.runPurgeBatches(cancelled, now, plan, PurgeReport{})
	if status != "interrupted" || !errors.Is(runErr, context.Canceled) {
		t.Fatalf("cancelled drain must report interrupted: %q %v", status, runErr)
	}
	if err := s.finishPurgeRun(cancelled, runID, status, errText, runErr); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted drain must preserve cancellation: %v", err)
	}
	run, err := s.LatestPurgeRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run.status != "interrupted" {
		t.Fatalf("cancelled drain must ledger interrupted, got %+v", run)
	}
}

// A run left marked running by a previous process is reconciled at the
// next startup purge instead of remaining an open run forever.
func TestPurgeReconcilesStaleRunningRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := s.db.Exec(`INSERT INTO purge_runs (started_at, status) VALUES (?, 'running')`, nanos(now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 90 * 24 * time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	var status string
	var finished int64
	if err := s.db.QueryRow(`SELECT status, finished_at FROM purge_runs ORDER BY run_id ASC LIMIT 1`).Scan(&status, &finished); err != nil {
		t.Fatal(err)
	}
	if status != "interrupted" || finished == 0 {
		t.Fatalf("stale run must be reconciled as interrupted, got %q at %d", status, finished)
	}
}

// A configured source survives inactivity alone; an unconfigured one
// does not. The quiet registration is childless by construction here.
func TestPurgeProtectsActiveSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active string
		want   int
	}{
		{"configured survives", "quiet", 1},
		{"unconfigured expires", "elsewhere", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			aged := nanos(time.Now().UTC().Add(-30 * 24 * time.Hour))
			if _, err := s.db.Exec(`INSERT INTO report_sources (source_scope, last_report_at) VALUES ('quiet', ?)`, aged); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			_, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 90 * 24 * time.Hour, ActiveSource: tc.active}, now)
			if err != nil {
				t.Fatal(err)
			}
			var kept int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM report_sources WHERE source_scope = 'quiet'`).Scan(&kept); err != nil || kept != tc.want {
				t.Fatalf("quiet source rows = %d, want %d (%v)", kept, tc.want, err)
			}
		})
	}
}

// A token named by a retained briefing's tokenized fields (model raw
// JSON, priority reason) survives expiry; an unreferenced token of the
// same age does not. Deleting the briefing releases the token.
func TestPurgeProtectsReferencedTokens(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	now := time.Now().UTC()
	body := producerPayload("evt-tok", "k", "host.appeared", `{"ip":"192.168.7.7","host_id":"ip:192.168.7.7","identity_source":"ip","identity_confidence":"high"}`)
	gid := persistReported(t, s, "lab", "evt-tok", "k", "host.appeared", body, old)
	projectAll(t, s)
	if _, err := s.CloseDue(ctx, old.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.7.7")
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.9.9")
	if err != nil {
		t.Fatal(err)
	}
	// The briefing names the token in its tokenized fields while the
	// restored text carries no raw address.
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: "saw " + ref,
		WhatChanged: "w", Context: "c", RecommendedCheck: "k",
		ModelRawJSON: `{"host":` + fmt.Sprintf("%q", ref) + `}`,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	aged := nanos(now.Add(-48 * time.Hour))
	if _, err := s.db.Exec(`UPDATE pseudonym_map SET last_used_at = ? WHERE token IN (?, ?)`, aged, ref, ctrl); err != nil {
		t.Fatal(err)
	}
	rep, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 90 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pseudonyms != 1 {
		t.Fatalf("only the unreferenced token must expire: %+v", rep)
	}
	var kept int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pseudonym_map WHERE token = ?`, ref).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("referenced token %q must survive: %d %v", ref, kept, err)
	}
	if tokens, err := s.GetOrAssignPseudonyms(ctx, "ip", "192.168.7.7"); err != nil || tokens["192.168.7.7"] != ref {
		t.Fatalf("referenced token must stay stable: %v %v", tokens, err)
	}
}

// seedRetainedBriefing persists, projects, closes, takes and briefs one
// group in production order. Direct SQL would bypass the worker's
// projection-before-briefing invariant, so tests pay for the full path.
func seedRetainedBriefing(t *testing.T, s *Store, scope, eventID, hostJSON, rawJSON, reason string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	body := producerPayload(eventID, "k", "host.appeared", hostJSON)
	gid := persistReported(t, s, scope, eventID, "k", "host.appeared", body, at)
	if _, err := s.ProjectReported(ctx, scope, 64); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: reason,
		WhatChanged: "w", Context: "c", RecommendedCheck: "k",
		ModelRawJSON: rawJSON, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// A retained NULL audit field is a readable empty reference source, not
// a startup-purge failure. Historical audit rows may store NULL, and
// protection must still see their token names.
func TestPurgeHandlesNullAuditFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	now := time.Now().UTC()
	body := producerPayload("evt-null-audit", "k", "host.appeared", `{"ip":"192.168.7.11","host_id":"ip:192.168.7.11","identity_source":"ip","identity_confidence":"high"}`)
	gid := persistReported(t, s, "lab", "evt-null-audit", "k", "host.appeared", body, old)
	projectAll(t, s)
	if _, err := s.CloseDue(ctx, old.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.7.11")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: "saw " + ref,
		WhatChanged: "w", Context: "c", RecommendedCheck: "k",
		ModelRawJSON: `{"host":` + fmt.Sprintf("%q", ref) + `}`,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE briefings SET model_raw_json = NULL WHERE briefing_id = ?`, "brf-"+taken.GroupID); err != nil {
		t.Fatal(err)
	}
	read, err := s.BriefingForGroup(ctx, taken.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	if read.ModelRawJSON != "" {
		t.Fatalf("NULL audit must read as empty, got %q", read.ModelRawJSON)
	}
	if _, err := s.db.Exec(`UPDATE pseudonym_map SET last_used_at = ? WHERE token = ?`, nanos(now.Add(-48*time.Hour)), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 90 * 24 * time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pseudonym_map WHERE token = ?`, ref).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("referenced token %q must survive NULL audit: %d %v", ref, kept, err)
	}
}

// Reference discovery collects every distinct token shape, not merely
// the first 256 occurrences. Repeating one token cannot push a later
// retained reference out of protection.
func TestPurgeProtectsReferenceAfterRepeatedTokens(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	now := time.Now().UTC()
	body := producerPayload("evt-overflow", "k", "host.appeared", `{"ip":"192.168.7.12","host_id":"ip:192.168.7.12","identity_source":"ip","identity_confidence":"high"}`)
	gid := persistReported(t, s, "lab", "evt-overflow", "k", "host.appeared", body, old)
	projectAll(t, s)
	if _, err := s.CloseDue(ctx, old.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.7.12")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.7.13")
	if err != nil {
		t.Fatal(err)
	}
	repeated := "[" + strings.Repeat(fmt.Sprintf("%q,", first), 300) + fmt.Sprintf("%q]", second)
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: "audit",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k",
		ModelRawJSON: repeated,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE pseudonym_map SET last_used_at = ? WHERE token IN (?, ?)`, nanos(now.Add(-48*time.Hour)), first, second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 90 * 24 * time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{first, second} {
		var kept int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pseudonym_map WHERE token = ?`, token).Scan(&kept); err != nil || kept != 1 {
			t.Fatalf("referenced token %q must survive repeated-token audit: %d %v", token, kept, err)
		}
	}
}

// Scratch maintenance shares the 200-write transaction budget with
// every other purge write, and yields under writer pressure like the
// deletion batches. The page sizes below are the enforcement point.
func TestProtectedScratchPagesRespectWriteBudget(t *testing.T) {
	if protectedTokenWritePage > maxPurgeRowsPerTxn {
		t.Fatalf("scratch write page %d exceeds the %d-write budget", protectedTokenWritePage, maxPurgeRowsPerTxn)
	}
	if protectedTokenClearPage > maxPurgeRowsPerTxn {
		t.Fatalf("scratch clear page %d exceeds the %d-write budget", protectedTokenClearPage, maxPurgeRowsPerTxn)
	}
}

// Paged discovery must remain complete: one-row scan pages still find
// references spread across retained briefings.
func TestProtectedTokenRefillPagesAcrossBriefings(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	first, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.7.21")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.7.22")
	if err != nil {
		t.Fatal(err)
	}
	host := `{"ip":"192.168.7.21","host_id":"ip:192.168.7.21","identity_source":"ip","identity_confidence":"high"}`
	seedRetainedBriefing(t, s, "lab", "evt-page-a", host, `{"host":`+fmt.Sprintf("%q", first)+`}`, "audit", now)
	seedRetainedBriefing(t, s, "lab", "evt-page-b", host, `{"host":`+fmt.Sprintf("%q", second)+`}`, "audit", now)
	fence, _, err := s.refillProtectedTokensPaged(ctx, nanos(now), nanos(now), 1, 1, 1, protectedRefillPasses)
	if err != nil {
		t.Fatal(err)
	}
	var protected int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM purge_protected_tokens WHERE token IN (?, ?)`, first, second).Scan(&protected); err != nil || protected != 2 {
		t.Fatalf("paged refill must protect both references: %d %v", protected, err)
	}
	var maxRowID int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM briefings`).Scan(&maxRowID); err != nil {
		t.Fatal(err)
	}
	if fence != maxRowID {
		t.Fatalf("refill fence must cover the scanned audit: %d vs %d", fence, maxRowID)
	}
}

// When reference discovery cannot complete, token expiry is skipped
// while independently safe retention still proceeds — and the run
// ledger records both. Dropping the scratch table forces discovery to
// fail deterministically before any deletion batch runs.
func TestPurgeOnceProceedsWhenDiscoveryFails(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	host := `{"ip":"192.168.7.51","host_id":"ip:192.168.7.51","identity_source":"ip","identity_confidence":"high"}`
	seedRetainedBriefing(t, s, "lab", "evt-discovery-fail", host, "{}", "audit", old)
	if _, err := s.db.Exec(`DROP TABLE purge_protected_tokens`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 2 * time.Hour}, now)
	if err == nil {
		t.Fatal("incomplete discovery must surface instead of passing silently")
	}
	var briefings int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM briefings`).Scan(&briefings); err != nil || briefings != 0 {
		t.Fatalf("safe briefing retention must proceed despite discovery failure: %d %v", briefings, err)
	}
	run, err := s.LatestPurgeRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run.status != "completed" {
		t.Fatalf("discovery failure must not fail the retention run itself: %+v", run)
	}
	if !strings.Contains(run.errText, "token") {
		t.Fatalf("run history must record the skipped token expiry: %q", run.errText)
	}
}

// Skipped token expiry deletes nothing while every other eligible
// stage still drains. Clearing the flag on the same plan releases the
// deferred expiry without touching anything else.
func TestPurgeBatchSkipsTokenExpiryWhenFlagged(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doomed, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.7.61")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE pseudonym_map SET last_used_at = ? WHERE token = ?`, nanos(now.Add(-48*time.Hour)), doomed); err != nil {
		t.Fatal(err)
	}
	plan := purgePlan{
		stages:          stageSubjects,
		subjectCut:      nanos(now.Add(time.Hour)),
		sourceCut:       nanos(now.Add(time.Hour)),
		tokenCut:        nanos(now.Add(-time.Hour)),
		skipTokenExpiry: true,
	}
	report, _, err := s.purgeBatch(ctx, now, plan)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pseudonyms != 0 {
		t.Fatalf("flagged token expiry must delete nothing: %+v", report)
	}
	plan.skipTokenExpiry = false
	report, _, err = s.purgeBatch(ctx, now, plan)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pseudonyms != 1 {
		t.Fatalf("unflagged expiry must release the deferred deletion: %+v", report)
	}
}

// The delete-time fence blocks token expiry when a retained briefing
// arrived after reference discovery. Rescanning through the new
// maximum releases the gate.
func TestPurgeTokenFenceBlocksNewBriefings(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doomed, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.7.31")
	if err != nil {
		t.Fatal(err)
	}
	host := `{"ip":"192.168.7.31","host_id":"ip:192.168.7.31","identity_source":"ip","identity_confidence":"high"}`
	seedRetainedBriefing(t, s, "lab", "evt-fence-a", host, "{}", "audit", now)
	fence, decFence, err := s.refillProtectedTokens(ctx, nanos(now), nanos(now))
	if err != nil {
		t.Fatal(err)
	}
	seedRetainedBriefing(t, s, "lab", "evt-fence-b", host, "{}", "audit", now)
	if _, err := s.db.Exec(`UPDATE pseudonym_map SET last_used_at = ? WHERE token = ?`, nanos(now.Add(-48*time.Hour)), doomed); err != nil {
		t.Fatal(err)
	}
	plan := purgePlan{
		stages:        stageSubjects,
		subjectCut:    nanos(now.Add(time.Hour)),
		sourceCut:     nanos(now.Add(time.Hour)),
		tokenCut:      nanos(now.Add(-time.Hour)),
		tokenFence:    fence,
		tokenDecFence: decFence,
		decisionCut:   nanos(now.Add(-24 * time.Hour)),
	}
	report, _, err := s.purgeBatch(ctx, now, plan)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pseudonyms != 0 {
		t.Fatalf("new audit must block token deletions: %+v", report)
	}
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM briefings`).Scan(&plan.tokenFence); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM operator_decisions WHERE subject_kind = 'briefing'`).Scan(&plan.tokenDecFence); err != nil {
		t.Fatal(err)
	}
	report, _, err = s.purgeBatch(ctx, now, plan)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pseudonyms != 1 {
		t.Fatalf("current fence must release unrelated expiry: %+v", report)
	}
}

// A pin committed after reference discovery must defer token expiry:
// the aged briefing it retains was never scanned, so its mappings
// cannot be judged unreferenced. The next discovery includes the pin
// and protects them.
func TestPurgeTokenFenceBlocksNewDecisions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doomed, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.7.71")
	if err != nil {
		t.Fatal(err)
	}
	host := `{"ip":"192.168.7.71","host_id":"ip:192.168.7.71","identity_source":"ip","identity_confidence":"high"}`
	seedRetainedBriefing(t, s, "lab", "evt-pinfence-a", host, `{"host":`+fmt.Sprintf("%q", doomed)+`}`, "audit", now.Add(-48*time.Hour))
	// A fresh retained briefing advances the briefing fence past the
	// aged row, so only the decision fence can defer its token.
	seedRetainedBriefing(t, s, "lab", "evt-pinfence-b", host, "{}", "audit", now)
	briefingCut := nanos(now.Add(-time.Hour))
	decisionCut := nanos(now.Add(-24 * time.Hour))
	fence, decFence, err := s.refillProtectedTokens(ctx, briefingCut, decisionCut)
	if err != nil {
		t.Fatal(err)
	}
	var briefingID string
	if err := s.db.QueryRow(`SELECT briefing_id FROM briefings ORDER BY created_at, rowid LIMIT 1`).Scan(&briefingID); err != nil {
		t.Fatal(err)
	}
	// Pin the aged, never-scanned briefing after discovery.
	if _, err := s.AppendDecision(ctx, DecisionInput{
		SubjectKind: DecisionSubjectBriefing, SubjectID: briefingID,
		Kind: DecisionExpected, RequestID: "pin-req-fence",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE pseudonym_map SET last_used_at = ? WHERE token = ?`, nanos(now.Add(-48*time.Hour)), doomed); err != nil {
		t.Fatal(err)
	}
	plan := purgePlan{
		stages:        stageSubjects,
		subjectCut:    nanos(now.Add(time.Hour)),
		sourceCut:     nanos(now.Add(time.Hour)),
		tokenCut:      nanos(now.Add(-time.Hour)),
		tokenFence:    fence,
		tokenDecFence: decFence,
		decisionCut:   decisionCut,
	}
	report, _, err := s.purgeBatch(ctx, now, plan)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pseudonyms != 0 {
		t.Fatalf("post-discovery pin must defer token expiry: %+v", report)
	}
	// The next discovery runs with the pin standing, so the token
	// enters the protected set and expiry stays deferred.
	fence, decFence, err = s.refillProtectedTokens(ctx, briefingCut, decisionCut)
	if err != nil {
		t.Fatal(err)
	}
	var protected int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM purge_protected_tokens WHERE token = ?`, doomed).Scan(&protected); err != nil || protected != 1 {
		t.Fatalf("rediscovery must protect the pinned token: %d %v", protected, err)
	}
	plan.tokenFence, plan.tokenDecFence = fence, decFence
	report, _, err = s.purgeBatch(ctx, now, plan)
	if err != nil {
		t.Fatal(err)
	}
	if report.Pseudonyms != 0 {
		t.Fatalf("protected token must survive with its pin: %+v", report)
	}
}

// A stale read-hit followed by a purge delete must surface a value-free
// retry signal, never a mapping to the deleted row. A fresh lookup after
// the race recovers by assigning a live token.
func TestPseudonymStaleHitRetriesWithoutValue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const value = "192.168.8.8"
	first, err := s.GetOrAssignPseudonym(ctx, "ip", value)
	if err != nil {
		t.Fatal(err)
	}
	normalized := []string{normalizePseudonymValue("ip", value)}
	stale, err := s.lookupPseudonymBatch(ctx, "ip", normalized)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM pseudonym_map WHERE token = ?`, first); err != nil {
		t.Fatal(err)
	}
	_, err = s.assignPseudonyms(ctx, "ip", "IP", []string{value}, normalized, map[string]string{normalized[0]: value}, stale)
	if !errors.Is(err, errPseudonymVanished) {
		t.Fatalf("stale hit must return the retry sentinel, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), value) {
		t.Fatalf("retry error must not disclose the private value: %q", err.Error())
	}
	second, err := s.GetOrAssignPseudonym(ctx, "ip", value)
	if err != nil {
		t.Fatal(err)
	}
	var kept string
	if err := s.db.QueryRow(`SELECT token FROM pseudonym_map WHERE real_value = ? AND category = 'ip'`, value).Scan(&kept); err != nil || kept != second {
		t.Fatalf("recovery must map to a live row: %q %v", kept, err)
	}
}

// Bookkeeping rows share the transaction budget and appear in the
// report: a deleting run always charges at least one purge_state
// upsert, and the ledger carries the same counts.
func TestPurgeReportChargesBookkeeping(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	body := producerPayload("evt-bk", "k", "host.appeared", `{"ip":"192.168.0.2"}`)
	gid := persistReported(t, s, "lab", "evt-bk", "k", "host.appeared", body, old)
	projectAll(t, s)
	if _, err := s.CloseDue(ctx, old.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: "r",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rep, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 2 * time.Hour}, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.total() == 0 {
		t.Fatalf("aged briefing must delete rows: %+v", rep)
	}
	if rep.Bookkeeping == 0 {
		t.Fatalf("report must charge purge_state upserts: %+v", rep)
	}
	run, err := s.LatestPurgeRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run.status != "completed" || run.total.Bookkeeping != rep.Bookkeeping || run.total.Briefings != rep.Briefings {
		t.Fatalf("ledger must mirror the run report: run %+v vs report %+v", run.total, rep)
	}
}

// A batch filled exactly to capacity must still commit its ledger row
// with a live run ID. The ledger reservation is consumed once, not
// charged twice: 198 deletions plus one purge_state upsert plus the
// run-ledger update commit exactly 200 writes.
func TestPurgeBatchFullCapacityWithLedger(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	host := `{"ip":"192.168.7.41","host_id":"ip:192.168.7.41","identity_source":"ip","identity_confidence":"high"}`
	for i := 0; i < 200; i++ {
		seedRetainedBriefing(t, s, "lab", fmt.Sprintf("evt-full-%03d", i), host, "{}", "audit", old)
	}
	runID, err := s.beginPurgeRun(ctx, nanos(time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	plan := purgePlan{stages: stageBriefings, briefingCut: nanos(time.Now().UTC().Add(-time.Hour)), runID: runID}
	report, progress, err := s.purgeBatch(ctx, time.Now().UTC(), plan)
	if err != nil {
		t.Fatalf("full batch must commit with a live ledger: %v", err)
	}
	if !progress {
		t.Fatal("a 200-briefing backlog must leave progress after one full batch")
	}
	if committed := report.total() + report.Bookkeeping; committed != maxPurgeRowsPerTxn {
		t.Fatalf("full batch must commit exactly the budget: %+v", report)
	}
}

// A batch error under cancellation records interrupted, not failed: the
// batch may have committed while its caller never collected the
// report, so the run must not blame the batch. A live-context error
// stays failed.
func TestPurgeBatchErrorClassification(t *testing.T) {
	boom := errors.New("test batch failure")
	if status, err := batchRunStatus(context.Background(), boom); status != "failed" || !errors.Is(err, boom) {
		t.Fatalf("live-context batch error must stay failed: %q %v", status, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if status, err := batchRunStatus(cancelled, boom); status != "interrupted" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled batch error must record interrupted: %q %v", status, err)
	}
}

// One deleting batch updates its ledger row in the same transaction.
// A crash after the commit still attributes that batch, rather than
// leaving progress only in the final run totals.
func TestPurgeBatchPersistsRunCountsAtomically(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	seedRetainedBriefing(t, s, "lab", "evt-batch-ledger", `{"ip":"192.168.0.3"}`, "{}", "audit", old)
	runID, err := s.beginPurgeRun(ctx, nanos(time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	plan := purgePlan{stages: stageBriefings, briefingCut: nanos(time.Now().UTC().Add(-time.Hour)), runID: runID}
	report, _, err := s.purgeBatch(ctx, time.Now().UTC(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if report.Briefings == 0 {
		t.Fatalf("seeded briefing must delete: %+v", report)
	}
	var deleted, writes int64
	if err := s.db.QueryRow(`SELECT briefings_deleted, bookkeeping_writes FROM purge_runs WHERE run_id = ?`, runID).Scan(&deleted, &writes); err != nil {
		t.Fatal(err)
	}
	if deleted != report.Briefings || writes != report.Bookkeeping {
		t.Fatalf("ledger must carry the committed batch: db=(%d,%d) report=%+v", deleted, writes, report)
	}
}

// Step 25 retention: operator-decision audit expires purely by row age
// with no exemption for rows the effective view still uses. An undo
// never purges while its target survives (targets are older, batches
// run oldest-first, and the pair gate defers the orphan case);
// materialized labels live on reported_subjects and survive audit
// expiry; a retained decision pins its profile subject against
// subject expiry.
func TestPurgeDecisionAuditExpiresByAge(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-25 * time.Hour)
	keep := PurgeSettings{BriefingRetention: 400 * 24 * time.Hour, SubjectRetention: 400 * 24 * time.Hour, DecisionRetention: 24 * time.Hour}

	pid := seedDecisionProfile(t, s, "lab", "evt-pdec-1", "aa:bb:cc:dd:ee:41", old)
	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionLabel, Text: "iot"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionNote, Text: "look"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE operator_decisions SET created_at = ?`, nanos(old)); err != nil {
		t.Fatal(err)
	}
	rep, err := s.PurgeOnce(ctx, keep, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Decisions != 2 {
		t.Fatalf("purged decisions = %d, want 2 (label and note)", rep.Decisions)
	}
	// The ledger attributes the audit deletions to their own column,
	// so the health breakdown and deletion total count them.
	h, err := s.LastSuccessfulPurgeRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !h.HaveRun || h.ByTable["operator_decisions"] != 2 {
		t.Fatalf("health decisions = %v, want 2", h.ByTable)
	}
	if ds, err := s.DecisionsForSubject(ctx, DecisionSubjectProfile, pid, 10, 0, 0); err != nil || len(ds) != 0 {
		t.Fatalf("audit after expiry = %d, %v; want empty", len(ds), err)
	}
	sub, err := s.ReportedProfile(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if sub.OperatorLabel == nil || *sub.OperatorLabel != "iot" {
		t.Fatalf("materialized label after audit expiry = %v, want iot", sub.OperatorLabel)
	}
}

// An undo whose target still exists is deferred even when the undo
// itself is expired; once the target expires too, one pass takes the
// pair together, oldest-first within the shared row budget.
func TestPurgeDecisionUndoWaitsForTarget(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-25 * time.Hour)
	keep := PurgeSettings{BriefingRetention: 400 * 24 * time.Hour, SubjectRetention: 400 * 24 * time.Hour, DecisionRetention: 24 * time.Hour}

	pid := seedDecisionProfile(t, s, "lab", "evt-pdec-2", "aa:bb:cc:dd:ee:42", now)
	lbl, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionLabel, Text: "iot"})
	if err != nil {
		t.Fatal(err)
	}
	undo, err := s.UndoDecision(ctx, lbl.Decision.DecisionID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE operator_decisions SET created_at = ? WHERE decision_id = ?`, nanos(old), undo.Decision.DecisionID); err != nil {
		t.Fatal(err)
	}
	if rep, err := s.PurgeOnce(ctx, keep, now); err != nil || rep.Decisions != 0 {
		t.Fatalf("purge with live target = %+v, %v; want no decision deletions", rep, err)
	}
	if ds, err := s.DecisionsForSubject(ctx, DecisionSubjectProfile, pid, 10, 0, 0); err != nil || len(ds) != 2 {
		t.Fatalf("audit with live target = %d, %v; want label and undo", len(ds), err)
	}
	if _, err := s.db.Exec(`UPDATE operator_decisions SET created_at = ? WHERE decision_id = ?`, nanos(old), lbl.Decision.DecisionID); err != nil {
		t.Fatal(err)
	}
	if rep, err := s.PurgeOnce(ctx, keep, now); err != nil || rep.Decisions != 2 {
		t.Fatalf("purge of expired pair = %+v, %v; want 2", rep, err)
	}
}

// A retained decision pins its profile subject: the subject survives
// subject expiry while the audit references it, and becomes eligible
// once that audit expires.
func TestPurgeDecisionPinsSubject(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-25 * time.Hour)

	pid := seedDecisionProfile(t, s, "lab", "evt-pdec-3", "aa:bb:cc:dd:ee:43", old)
	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionNote, Text: "keep"}); err != nil {
		t.Fatal(err)
	}
	// Leave only the decision reference behind: drop the projected
	// evidence links and age the subject past its horizon.
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`DELETE FROM reported_observations WHERE profile_id = ?`, []any{pid}},
		{`DELETE FROM group_subjects WHERE profile_id = ?`, []any{pid}},
		{`DELETE FROM briefing_subjects WHERE profile_id = ?`, []any{pid}},
		{`UPDATE reported_subjects SET last_report_at = ? WHERE profile_id = ?`, []any{nanos(old), pid}},
	} {
		if _, err := s.db.Exec(stmt.query, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	pin := PurgeSettings{BriefingRetention: 400 * 24 * time.Hour, SubjectRetention: 24 * time.Hour, DecisionRetention: 400 * 24 * time.Hour}
	if rep, err := s.PurgeOnce(ctx, pin, now); err != nil || rep.Subjects != 0 {
		t.Fatalf("purge of pinned subject = %+v, %v; want subject retained", rep, err)
	}
	if _, err := s.ReportedProfile(ctx, pid); err != nil {
		t.Fatalf("pinned subject missing: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE operator_decisions SET created_at = ?`, nanos(old)); err != nil {
		t.Fatal(err)
	}
	release := PurgeSettings{BriefingRetention: 400 * 24 * time.Hour, SubjectRetention: 24 * time.Hour, DecisionRetention: 24 * time.Hour}
	rep, err := s.PurgeOnce(ctx, release, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Decisions != 1 || rep.Subjects != 1 {
		t.Fatalf("purge after audit expiry = %+v; want 1 decision and 1 subject", rep)
	}
}

// A briefing a retained decision still references cannot purge: the
// audit outlives the briefing horizon, and deleting the briefing
// would orphan the trail and unblock evidence the decision still
// justifies. Once the audit itself expires, the briefing goes.
func TestPurgeBriefingPinnedByRetainedDecision(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-25 * time.Hour)

	gid := seedDecidedBriefing(t, s, "evt-pdec-b1", "low", "other", old)
	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionExpected}); err != nil {
		t.Fatal(err)
	}
	pin := PurgeSettings{BriefingRetention: 24 * time.Hour, SubjectRetention: 400 * 24 * time.Hour, DecisionRetention: 400 * 24 * time.Hour}
	if rep, err := s.PurgeOnce(ctx, pin, now); err != nil || rep.Briefings != 0 {
		t.Fatalf("purge of pinned briefing = %+v, %v; want briefing retained", rep, err)
	}
	if _, err := s.BriefingForGroup(ctx, gid); err != nil {
		t.Fatalf("pinned briefing missing: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE operator_decisions SET created_at = ?`, nanos(old)); err != nil {
		t.Fatal(err)
	}
	release := PurgeSettings{BriefingRetention: 24 * time.Hour, SubjectRetention: 400 * 24 * time.Hour, DecisionRetention: 24 * time.Hour}
	rep, err := s.PurgeOnce(ctx, release, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Briefings != 1 || rep.Decisions != 1 {
		t.Fatalf("purge after audit expiry = %+v; want briefing and decision", rep)
	}
}

// The pin lives inside the age selection before LIMIT: when a full
// oldest-first batch consists of pinned briefings, the drain must
// still reach and delete eligible work behind it instead of ending
// with zero progress.
func TestPurgeSkipsPinnedWindow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-25 * time.Hour)
	pinnedAt := nanos(old.Add(-time.Hour))
	for i := 0; i < 210; i++ {
		gid := fmt.Sprintf("grp-pin-%d", i)
		bid := "brf-" + gid
		if _, err := s.db.Exec(`INSERT INTO active_groups (group_id, host_anchor_key, opened_at, last_event_at, deadline_at, event_count, status)
			VALUES (?, 'host:pin', ?, ?, ?, 1, 'done')`, gid, pinnedAt, pinnedAt, pinnedAt); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO briefings (briefing_id, group_id, host_anchor_key, baseline, priority, priority_reason, what_changed, context, recommended_check, created_at, seq)
			VALUES (?, ?, 'host:pin', 'low', 'low', 'r', 'w', 'c', 'k', ?, ?)`, bid, gid, pinnedAt, i+1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO operator_decisions (decision_id, subject_kind, subject_id, kind, created_at)
			VALUES (?, 'briefing', ?, 'expected', ?)`, fmt.Sprintf("dec-pin-%d", i), bid, nanos(now)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO active_groups (group_id, host_anchor_key, opened_at, last_event_at, deadline_at, event_count, status)
		VALUES ('grp-open-1', 'host:open', ?, ?, ?, 1, 'done')`, nanos(old), nanos(old), nanos(old)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO briefings (briefing_id, group_id, host_anchor_key, baseline, priority, priority_reason, what_changed, context, recommended_check, created_at, seq)
		VALUES ('brf-grp-open-1', 'grp-open-1', 'host:open', 'low', 'low', 'r', 'w', 'c', 'k', ?, 1000)`, nanos(old)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO report_sources (source_scope) VALUES ('pin-scope')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		pid := fmt.Sprintf("prof-pin-%d", i)
		if _, err := s.db.Exec(`INSERT INTO reported_subjects (profile_id, source_scope, reference_class, upstream_reference, first_report_at, last_report_at)
			VALUES (?, 'pin-scope', 'event', ?, ?, ?)`, pid, pid, nanos(old), nanos(now)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 210; i++ {
		bid := fmt.Sprintf("brf-grp-pin-%d", i)
		if _, err := s.db.Exec(`INSERT INTO briefing_subjects (briefing_id, profile_id, role) VALUES (?, ?, 'current')`,
			bid, fmt.Sprintf("prof-pin-%d", i%5)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO briefing_subjects (briefing_id, profile_id, role) VALUES ('brf-grp-open-1', 'prof-pin-0', 'current')`); err != nil {
		t.Fatal(err)
	}
	pin := PurgeSettings{BriefingRetention: 24 * time.Hour, SubjectRetention: 400 * 24 * time.Hour, DecisionRetention: 400 * 24 * time.Hour}
	rep, err := s.PurgeOnce(ctx, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Briefings != 1 {
		t.Fatalf("purged briefings = %d, want the one eligible row", rep.Briefings)
	}
	var briefings, decisions, links int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM briefings`).Scan(&briefings); err != nil || briefings != 210 {
		t.Fatalf("briefings = %d, %v; want all 210 pinned", briefings, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operator_decisions`).Scan(&decisions); err != nil || decisions != 210 {
		t.Fatalf("decisions = %d, %v; want all 210 retained", decisions, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM briefing_subjects`).Scan(&links); err != nil || links != 210 {
		t.Fatalf("links = %d, %v; want all 210 pinned links", links, err)
	}
}
