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
