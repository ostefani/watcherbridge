package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Step 20 overview reads: bounded attention candidates, kind digest,
// fallback share from retained rows, error counts, source health, and
// purge history. Digest must ride the (created_at, kind) index: the
// EXPLAIN assertion pins that plan.
func TestOverviewReads(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-2 * time.Hour)

	brief := func(i int, at time.Time, kind, priority string, fallback bool) {
		body := producerPayload("evt-ov"+string(rune('0'+i)), "k", "host.changed",
			`{"ip":"192.168.0.1","hostname":"h","ports":[]}`)
		gid := persistReported(t, s, "lab", "evt-ov"+string(rune('0'+i)), "k", "host.changed", body, at)
		projectAll(t, s)
		if _, err := s.CloseDue(ctx, at.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		taken, err := s.TakeGroup(ctx, gid)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.InsertBriefing(ctx, Briefing{
			BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
			Baseline: "low", Priority: priority, PriorityReason: "r", Kind: kind, Source: "rules",
			FallbackUsed: fallback,
			WhatChanged:  "w", Context: "c", RecommendedCheck: "k", CreatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	brief(0, old, "port_closed", "high", false)
	brief(1, old, "other", "low", true)
	brief(2, now, "other", "medium", false)

	counts, err := s.PriorityCounts(ctx, old.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if counts["low"] != 1 || counts["high"] != 1 || counts["medium"] != 1 || len(counts) != 3 {
		t.Fatalf("priority counts = %v, want full-window counts", counts)
	}
	candidates, err := s.AttentionCandidates(ctx, now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Kind != "other" || candidates[0].Priority != "medium" {
		t.Fatalf("candidates = %+v, want the one in-window briefing", candidates)
	}
	// The narrow projection serves the ordering from the created_at
	// index, not a table scan.
	for _, row := range explainQuery(t, s, `SELECT briefing_id, priority, created_at, kind FROM briefings
		WHERE created_at >= 0
		ORDER BY CASE priority WHEN 'critical' THEN 3 WHEN 'high' THEN 2 WHEN 'medium' THEN 1 ELSE 0 END DESC,
			created_at DESC, briefing_id ASC LIMIT 10`) {
		if strings.Contains(strings.ToUpper(row), "SCAN briefings") && !strings.Contains(strings.ToUpper(row), "USING") {
			t.Fatalf("candidates scan briefings:\n%s", row)
		}
	}

	digest, err := s.BriefingDigest(ctx, old.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if digest["port_closed"] != 1 || digest["other"] != 2 {
		t.Fatalf("digest = %v", digest)
	}

	var plan string
	for _, row := range explainQuery(t, s, `SELECT kind, COUNT(*) FROM briefings WHERE created_at >= ? GROUP BY kind`) {
		plan += row + "\n"
	}
	if strings.Contains(strings.ToUpper(plan), "SCAN briefings") && !strings.Contains(strings.ToUpper(plan), "USING") {
		t.Fatalf("digest scans briefings:\n%s", plan)
	}

	used, total, err := s.FallbackShare(ctx, old.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if used != 1 || total != 3 {
		t.Fatalf("fallback share = %d/%d", used, total)
	}

	if n, err := s.RecentForwardErrors(ctx, old); err != nil || n != 0 {
		t.Fatalf("forward errors = %d, %v", n, err)
	}
	body := producerPayload("evt-ov9", "k", "host.appeared", `{"ip":"192.168.0.9"}`)
	persistReported(t, s, "lab", "evt-ov9", "k", "host.appeared", body, now)
	if err := s.RecordForwardFailure(ctx, "evt-ov9", "dial tcp: refused"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RecentForwardErrors(ctx, now.Add(-time.Minute)); err != nil || n != 1 {
		t.Fatalf("forward errors = %d, %v", n, err)
	}

	sources, err := s.SourceHealthRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Scope != "lab" || !sources[0].HasLastReport {
		t.Fatalf("sources = %+v", sources)
	}
	if sources[0].HasLastHeartbeat {
		t.Fatal("heartbeat must read absent until H1")
	}

	if totals, err := s.PurgeTotals(ctx); err != nil {
		t.Fatalf("purge totals: %v", err)
	} else if len(totals) != 0 {
		t.Fatalf("purge totals = %v, want empty before any deletion", totals)
	}
	if run, err := s.LastPurgeRun(ctx); err != nil || run.HaveRun {
		t.Fatalf("last purge = %+v, %v; want no run yet", run, err)
	}

	if _, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: 30 * 24 * time.Hour, SubjectRetention: 90 * 24 * time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	if run, err := s.LastPurgeRun(ctx); err != nil || !run.HaveRun || run.Status != "completed" {
		t.Fatalf("last purge = %+v, %v", run, err)
	}
}

// A newer failed run must not hide the last successful completion:
// health reports the latest attempt and the last success separately,
// with per-table visibility on both.
func TestPurgeLastSuccessSurvivesFailure(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: 30 * 24 * time.Hour, SubjectRetention: 90 * 24 * time.Hour}, now); err != nil {
		t.Fatal(err)
	}
	runID, err := s.beginPurgeRun(ctx, nanos(now))
	if err != nil {
		t.Fatal(err)
	}
	// Finalization reports the run's own error to the caller; the ledger
	// row still records the failure for health.
	if err := s.finishPurgeRun(ctx, runID, "failed", "boom", errors.New("boom")); err == nil {
		t.Fatal("expected the run error back")
	}
	last, err := s.LastPurgeRun(ctx)
	if err != nil || !last.HaveRun || last.Status != "failed" || last.Error != "boom" {
		t.Fatalf("latest = %+v, %v", last, err)
	}
	success, err := s.LastSuccessfulPurgeRun(ctx)
	if err != nil || !success.HaveRun || success.Status != "completed" {
		t.Fatalf("last success = %+v, %v", success, err)
	}
	if success.ByTable == nil {
		t.Fatal("success must carry the per-table breakdown, even when empty")
	}
}

// explainQuery returns EXPLAIN QUERY PLAN detail lines for a query.
func explainQuery(t *testing.T, s *Store, query string) []string {
	t.Helper()
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+query, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
