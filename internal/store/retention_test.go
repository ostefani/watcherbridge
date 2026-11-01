package store

import (
	"context"
	"fmt"
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
	plan := purgePlan{
		stages:      stageAll,
		briefingCut: nanos(now.Add(-time.Hour)),
		rawCut:      nanos(now.Add(-time.Hour)),
		failedCut:   nanos(now.Add(-failedEventFloor)),
		subjectCut:  nanos(now.Add(-2 * time.Hour)),
		sourceCut:   nanos(now.Add(-heartbeatRetention)),
		tokenCut:    nanos(now.Add(-time.Hour)),
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
		if report.total() > maxPurgeRowsPerTxn {
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
	if err := s.finishPurgeRun(ctx, runID, "interrupted", "test cancel", PurgeReport{RawEvents: 3}); err != nil {
		t.Fatal(err)
	}
	interrupted, err := s.LatestPurgeRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.status != "interrupted" || interrupted.errText != "test cancel" || interrupted.total.RawEvents != 3 {
		t.Fatalf("interrupted run must keep status, error and counts: %+v", interrupted)
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

// Deleting a token row out from under a pending lookup (the purge race
// the in-transaction recheck closes) surfaces a retryable error, never
// a mapping to a rowless token.
func TestPseudonymVanishedDuringAssignmentRetries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	first, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.8.8")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM pseudonym_map WHERE token = ?`, first); err != nil {
		t.Fatal(err)
	}
	second, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.8.8")
	if err != nil {
		t.Fatal(err)
	}
	var kept string
	if err := s.db.QueryRow(`SELECT token FROM pseudonym_map WHERE real_value = '192.168.8.8' AND category = 'ip'`).Scan(&kept); err != nil || kept != second {
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
