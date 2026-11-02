package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// dropTraceSchema rewinds the Step-27 audit additions (v21 briefing
// columns, the agent_traces table and the traces ledger column) so
// migration-chain tests that rewind to older versions replay cleanly
// through the current version on reopen. It follows the repo convention
// of explicit downgrade drops: a real pre-21 database never has these
// objects, only a downgraded current one does.
func dropTraceSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DROP TABLE IF EXISTS agent_traces`); err != nil {
		t.Fatalf("drop traces: %v", err)
	}
	for _, col := range []string{"likely_explanation", "confidence", "evidence_refs_json",
		"input_tokens", "output_tokens", "cost_micros"} {
		dropColumnIfExists(t, db, "briefings", col)
	}
	dropColumnIfExists(t, db, "purge_runs", "traces_deleted")
}

// takeFeedGroup closes and takes a seeded group so briefing inserts have
// a processing group to complete, mirroring the worker take order.
func takeFeedGroup(t *testing.T, s *Store, gid string) {
	t.Helper()
	if _, err := s.CloseDue(context.Background(), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeGroup(context.Background(), gid); err != nil {
		t.Fatal(err)
	}
}

func traceTestBriefing(now time.Time) Briefing {
	return Briefing{
		BriefingID: "brf-trace-1", GroupID: "", HostAnchorKey: "host:a",
		Baseline: "low", Priority: "low", PriorityReason: "r",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: now,
	}
}

func TestBriefingStep27FieldsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	gid := seedFeedGroup(t, s, "host:a", "evt-a1")
	takeFeedGroup(t, s, gid)
	now := time.Now().UTC().Truncate(time.Second)
	explanation := "likely a reappearing host"
	in, out, cost := int64(120), int64(45), int64(3)
	b := traceTestBriefing(now)
	b.GroupID = gid
	b.BriefingID = "brf-" + gid
	b.LikelyExplanation = &explanation
	b.Confidence = "medium"
	b.EvidenceRefs = []EvidenceRef{EventRef("evt-a1"), ToolRef(0)}
	b.InputTokens, b.OutputTokens, b.CostMicros = &in, &out, &cost
	b.Traces = []AgentTrace{
		{Step: 0, Tool: "get_reported_profile", ArgsJSON: `{"host":"IP_1"}`, ResultJSON: `{"tool":"get_reported_profile"}`, Status: "ok", DurationMS: 2},
	}
	if err := s.InsertBriefing(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	got, err := s.BriefingForGroup(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	if got.LikelyExplanation == nil || *got.LikelyExplanation != explanation {
		t.Fatalf("explanation must round-trip: %+v", got.LikelyExplanation)
	}
	if got.Confidence != "medium" {
		t.Fatalf("confidence must round-trip: %q", got.Confidence)
	}
	if len(got.EvidenceRefs) != 2 || got.EvidenceRefs[0] != EventRef("evt-a1") || got.EvidenceRefs[1] != ToolRef(0) {
		t.Fatalf("evidence refs must round-trip: %+v", got.EvidenceRefs)
	}
	if got.InputTokens == nil || *got.InputTokens != in || got.OutputTokens == nil || *got.OutputTokens != out ||
		got.CostMicros == nil || *got.CostMicros != cost {
		t.Fatalf("usage must round-trip: %+v %+v %+v", got.InputTokens, got.OutputTokens, got.CostMicros)
	}
	traces, err := s.AgentTraces(context.Background(), got.BriefingID)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 || traces[0].Step != 0 || traces[0].Tool != "get_reported_profile" ||
		traces[0].Status != "ok" || traces[0].DurationMS != 2 {
		t.Fatalf("trace must round-trip in step order: %+v", traces)
	}
}

func TestBriefingStep27FieldsHonestBackfill(t *testing.T) {
	s := openTestStore(t)
	gid := seedFeedGroup(t, s, "host:a", "evt-a1")
	takeFeedGroup(t, s, gid)
	now := time.Now().UTC().Truncate(time.Second)
	b := traceTestBriefing(now)
	b.GroupID = gid
	b.BriefingID = "brf-" + gid
	// No Step 27 fields set: a deterministic-style insert must read
	// back unknown, never a fabricated explanation, confidence, refs,
	// usage or trace.
	if err := s.InsertBriefing(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	got, err := s.BriefingForGroup(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	if got.LikelyExplanation != nil || got.Confidence != "" || len(got.EvidenceRefs) != 0 ||
		got.InputTokens != nil || got.OutputTokens != nil || got.CostMicros != nil {
		t.Fatalf("missing Step 27 fields must read unknown: %+v", got)
	}
	traces, err := s.AgentTraces(context.Background(), got.BriefingID)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 0 {
		t.Fatalf("no executions means no trace rows: %+v", traces)
	}
}

func TestBriefingTracesFirstCommitWins(t *testing.T) {
	s := openTestStore(t)
	gid := seedFeedGroup(t, s, "host:a", "evt-a1")
	takeFeedGroup(t, s, gid)
	now := time.Now().UTC().Truncate(time.Second)
	b := traceTestBriefing(now)
	b.GroupID = gid
	b.BriefingID = "brf-" + gid
	b.Traces = []AgentTrace{
		{Step: 0, Tool: "get_reported_profile", ArgsJSON: `{}`, ResultJSON: `{}`, Status: "ok"},
	}
	if err := s.InsertBriefing(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	// A restart that re-runs the group loses the briefing race: the
	// second insert fails and the first commit's traces stand alone.
	b.Traces = []AgentTrace{
		{Step: 0, Tool: "get_reported_services", ArgsJSON: `{}`, ResultJSON: `{}`, Status: "ok"},
		{Step: 1, Tool: "compare_reported_context", ArgsJSON: `{}`, ResultJSON: `{}`, Status: "ok"},
	}
	if err := s.InsertBriefing(context.Background(), b); err == nil {
		t.Fatal("second briefing for one group must fail")
	}
	traces, err := s.AgentTraces(context.Background(), b.BriefingID)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 || traces[0].Tool != "get_reported_profile" {
		t.Fatalf("losing commit must leave no trace rows: %+v", traces)
	}
}

func TestPurgeDrainsTracesWithBriefing(t *testing.T) {
	s := openTestStore(t)
	gid := seedFeedGroup(t, s, "host:a", "evt-a1")
	takeFeedGroup(t, s, gid)
	now := time.Now().UTC().Truncate(time.Second)
	b := traceTestBriefing(now.Add(-2 * time.Hour))
	b.GroupID = gid
	b.BriefingID = "brf-" + gid
	b.Traces = []AgentTrace{
		{Step: 0, Tool: "get_reported_profile", ArgsJSON: `{"host":"IP_1"}`, ResultJSON: `{"tool":"get_reported_profile"}`, Status: "ok"},
		{Step: 1, Tool: "get_reported_history", ArgsJSON: `{"host":"IP_1"}`, ResultJSON: `{"tool":"get_reported_history"}`, Status: "ok"},
	}
	if err := s.InsertBriefing(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	rep, err := s.PurgeOnce(context.Background(), PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 2 * time.Hour}, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Traces != 2 || rep.Briefings != 1 {
		t.Fatalf("purge must attribute traces and briefing: %+v", rep)
	}
	if traces, err := s.AgentTraces(context.Background(), b.BriefingID); err != nil || len(traces) != 0 {
		t.Fatalf("traces must drain with their briefing: %+v %v", traces, err)
	}
	if _, err := s.BriefingForGroup(context.Background(), gid); err == nil {
		t.Fatal("briefing must drain once its traces are gone")
	}
	run, err := s.LatestPurgeRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.total.Traces != 2 {
		t.Fatalf("ledger must attribute trace deletes: %+v", run.total)
	}
}

func TestProtectedTokensCoverTraces(t *testing.T) {
	s := openTestStore(t)
	gid := seedFeedGroup(t, s, "host:a", "evt-a1")
	takeFeedGroup(t, s, gid)
	now := time.Now().UTC().Truncate(time.Second)
	b := traceTestBriefing(now)
	b.GroupID = gid
	b.BriefingID = "brf-" + gid
	b.Traces = []AgentTrace{
		{Step: 0, Tool: "get_reported_profile", ArgsJSON: `{"host":"IP_9"}`, ResultJSON: `{"tool":"get_reported_profile","result":{"subject":"SUBJ_1"}}`, Status: "ok"},
	}
	if err := s.InsertBriefing(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.refillProtectedTokens(context.Background(), now.Add(-time.Hour).UnixNano(), nanos(now.Add(-24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM purge_protected_tokens WHERE token = ?`, "IP_9").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("trace token IP_9 must be protected: %d", n)
	}
	// Run-scoped SUBJ_n aliases never enter the pseudonym map, so the
	// discovery regex (bundle token shapes only) rightly ignores them.
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM purge_protected_tokens WHERE token = ?`, "SUBJ_1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("run alias SUBJ_1 must not pin mappings: %d", n)
	}
}

func TestProtectedTokensCoverPinnedTraces(t *testing.T) {
	s := openTestStore(t)
	gid := seedFeedGroup(t, s, "host:a", "evt-a1")
	takeFeedGroup(t, s, gid)
	now := time.Now().UTC().Truncate(time.Second)
	// An aged briefing with a trace naming a token that appears
	// nowhere else: outside the window, it survives only through its
	// decision pin, and its mappings must survive with it.
	b := traceTestBriefing(now.Add(-2 * time.Hour))
	b.GroupID = gid
	b.BriefingID = "brf-" + gid
	b.Traces = []AgentTrace{
		{Step: 0, Tool: "get_reported_profile", ArgsJSON: `{"host":"IP_9"}`, ResultJSON: `{"tool":"get_reported_profile"}`, Status: "ok"},
	}
	if err := s.InsertBriefing(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendDecision(context.Background(), DecisionInput{
		SubjectKind: DecisionSubjectBriefing, SubjectID: "brf-" + gid,
		Kind: DecisionExpected, RequestID: "pin-req-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.refillProtectedTokens(context.Background(), now.Add(-time.Hour).UnixNano(), nanos(now.Add(-24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM purge_protected_tokens WHERE token = ?`, "IP_9").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("decision-pinned trace token must be protected at any age")
	}
}

func TestInsertBriefingRejectsGappedTraceSteps(t *testing.T) {
	s := openTestStore(t)
	gid := seedFeedGroup(t, s, "host:a", "evt-a1")
	takeFeedGroup(t, s, gid)
	now := time.Now().UTC().Truncate(time.Second)
	b := traceTestBriefing(now)
	b.GroupID = gid
	b.BriefingID = "brf-" + gid
	b.Traces = []AgentTrace{
		{Step: 1, Tool: "get_reported_profile", ArgsJSON: `{}`, ResultJSON: `{}`, Status: "ok"},
	}
	if err := s.InsertBriefing(context.Background(), b); err == nil {
		t.Fatal("non-zero-based trace steps must fail the insert")
	}
}
