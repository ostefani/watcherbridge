package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestIncidentSummaryQueue exercises the durable scheduling contract:
// eligibility is recorded atomically with closure, the outstanding
// queue caps at 100 with skipped_overload beyond it, claims carry a
// token and lease, reservations persist the snapshot identity, and the
// first committed terminal outcome wins over stale completions.
func TestIncidentSummaryQueue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	insertIncident := func(id, floor string) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO incidents (incident_id, kind, source_scope, target, status, floor, identity_revision, identity_namespace, qualified_refs, member_count, window_start, last_member_at, opened_at, closed_at, updated_at) VALUES (?, 'burst_appearance', 'scope', 'target', 'closed', ?, 'r1', 'ns', 3, 3, ?, ?, ?, ?, ?)`,
			id, floor, now.UnixNano(), now.UnixNano(), now.UnixNano(), now.UnixNano(), now.UnixNano()); err != nil {
			t.Fatalf("insert incident: %v", err)
		}
	}
	insertIncident("inc-1", "medium")
	if err := s.write(ctx, func(tx *sql.Tx) error {
		return enqueueIncidentSummary(tx, "inc-1", "medium", now)
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := s.GetIncidentSummary(ctx, "inc-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "pending" || got.Floor != "medium" || got.FinalPriority != "medium" {
		t.Fatalf("pending row = %+v", got)
	}
	job, claim, ok, err := s.ClaimIncidentSummary(ctx, now)
	if err != nil || !ok || job.IncidentID != "inc-1" || claim == "" {
		t.Fatalf("claim = %+v %q %v %v", job, claim, ok, err)
	}
	if err := s.ReserveSummarySnapshot(ctx, "inc-1", claim, `{"incident_id":"inc-1"}`, `{"M1":"brf-x"}`, 1, 2, now); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	sugg := "high"
	outcome := IncidentSummary{
		Status: "model", Floor: "medium", Suggestion: &sugg,
		FinalPriority: "high", Clamped: true,
		Summary: "s", Hypothesis: "h", Confidence: "high", Reason: "r",
		Citations: []SummaryCitation{{Member: "M1"}},
		Sampled:   1, Omitted: 2,
	}
	committed, err := s.CompleteIncidentSummary(ctx, "inc-1", claim, `{"incident_id":"inc-1"}`, outcome, now)
	if err != nil || !committed {
		t.Fatalf("complete = %v %v", committed, err)
	}
	// Stale completion with a wrong claim loses: first terminal wins.
	stale, err := s.CompleteIncidentSummary(ctx, "inc-1", "bad-claim", `{"incident_id":"inc-1"}`, outcome, now)
	if err != nil || stale {
		t.Fatalf("stale complete = %v %v", stale, err)
	}
	final, err := s.GetIncidentSummary(ctx, "inc-1")
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	if final.Status != "model" || final.FinalPriority != "high" || !final.Clamped || final.Suggestion == nil || *final.Suggestion != "high" {
		t.Fatalf("final row = %+v", final)
	}
	if len(final.Citations) != 1 || final.Citations[0].Member != "M1" {
		t.Fatalf("citations = %+v", final.Citations)
	}

	// Queue cap: 100 outstanding pending/running, the rest skip.
	// Fill deterministically with numbered IDs.
	for i := 0; i < 100; i++ {
		id := "inc-q-" + padInt(i)
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO incidents (incident_id, kind, source_scope, target, status, floor, identity_revision, identity_namespace, qualified_refs, member_count, window_start, last_member_at, opened_at, closed_at, updated_at) VALUES (?, 'burst_appearance', 'scope', 'target', 'closed', 'low', 'r1', 'ns', 3, 3, ?, ?, ?, ?, ?)`,
			id, now.UnixNano(), now.UnixNano(), now.UnixNano(), now.UnixNano(), now.UnixNano()); err != nil {
			t.Fatalf("insert cap incident: %v", err)
		}
		if err := s.write(ctx, func(tx *sql.Tx) error {
			return enqueueIncidentSummary(tx, id, "low", now)
		}); err != nil {
			t.Fatalf("enqueue cap: %v", err)
		}
	}
	if n, _ := s.SummaryOutstanding(ctx); n != 100 {
		t.Fatalf("outstanding = %d, want 100", n)
	}
	if _, err := s.db.Exec(`INSERT INTO incidents (incident_id, kind, source_scope, target, status, floor, identity_revision, identity_namespace, qualified_refs, member_count, window_start, last_member_at, opened_at, closed_at, updated_at) VALUES ('inc-over', 'burst_appearance', 'scope', 'target', 'closed', 'low', 'r1', 'ns', 3, 3, ?, ?, ?, ?, ?)`,
		now.UnixNano(), now.UnixNano(), now.UnixNano(), now.UnixNano(), now.UnixNano()); err != nil {
		t.Fatalf("insert over: %v", err)
	}
	if err := s.write(ctx, func(tx *sql.Tx) error {
		return enqueueIncidentSummary(tx, "inc-over", "low", now)
	}); err != nil {
		t.Fatalf("enqueue over: %v", err)
	}
	over, err := s.GetIncidentSummary(ctx, "inc-over")
	if err != nil {
		t.Fatalf("get over: %v", err)
	}
	if over.Status != "skipped" || over.Error != "skipped_overload" {
		t.Fatalf("over row = %+v", over)
	}
}

func TestIncidentSummarySendReservationsSurviveLeaseRecovery(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := s.db.Exec(`INSERT INTO incidents (incident_id, kind, source_scope, target, status, floor, identity_revision, identity_namespace, qualified_refs, member_count, window_start, last_member_at, opened_at, closed_at, updated_at) VALUES ('inc-send', 'burst_appearance', 'scope', 'target', 'closed', 'low', 'r1', 'ns', 3, 3, ?, ?, ?, ?, ?)`, now.UnixNano(), now.UnixNano(), now.UnixNano(), now.UnixNano(), now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, func(tx *sql.Tx) error { return enqueueIncidentSummary(tx, "inc-send", "low", now) }); err != nil {
		t.Fatal(err)
	}
	job, claim, ok, err := s.ClaimIncidentSummary(ctx, now)
	if err != nil || !ok {
		t.Fatalf("claim: %+v %v %v", job, ok, err)
	}
	if err := s.ReserveSummarySnapshot(ctx, job.IncidentID, claim, `{"safe":true}`, `{}`, 1, 0, now); err != nil {
		t.Fatal(err)
	}
	if reserved, err := s.ReserveSummarySend(ctx, job.IncidentID, claim, now); err != nil || !reserved {
		t.Fatalf("first send reservation: %v %v", reserved, err)
	}
	if err := s.RecordSummaryUsage(ctx, job.IncidentID, claim, 23, 7, true, now); err != nil {
		t.Fatal(err)
	}
	recovered, claim2, ok, err := s.ClaimIncidentSummary(ctx, now.Add(summaryLease+time.Second))
	if err != nil || !ok || recovered.SendsReserved != 1 || recovered.SnapshotJSON != `{"safe":true}` || recovered.InputTokens == nil || *recovered.InputTokens != 23 {
		t.Fatalf("recovered state: %+v ok=%v err=%v", recovered, ok, err)
	}
	if reserved, err := s.ReserveSummarySend(ctx, job.IncidentID, claim2, now); err != nil || !reserved {
		t.Fatalf("repair reservation: %v %v", reserved, err)
	}
	if reserved, err := s.ReserveSummarySend(ctx, job.IncidentID, claim2, now); err != nil || reserved {
		t.Fatalf("third send reservation: %v %v", reserved, err)
	}
}

func TestSummaryMemberSelectionUsesBoundedOrderIndex(t *testing.T) {
	s := openTestStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT b.briefing_id, b.priority, b.kind, b.created_at,
		b.what_changed, b.context, b.recommended_check, m.refs_json
		FROM incident_members m JOIN briefings b ON b.briefing_id=m.briefing_id
		WHERE m.incident_id='inc-plan' ORDER BY m.priority_rank DESC, m.member_created_at ASC, m.briefing_id ASC LIMIT 20`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	text := plan.String()
	if !strings.Contains(text, "incident_members_summary_order") || strings.Contains(text, "USE TEMP B-TREE") {
		t.Fatalf("summary member query plan is not index ordered: %s", text)
	}
}

func padInt(i int) string { return fmt.Sprintf("%03d", i) }
