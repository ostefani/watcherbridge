package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func regressionIncident(t *testing.T, s *Store, base time.Time) ([]string, string) {
	t.Helper()
	bindIncidentSource(t, s, "lab")
	var bids []string
	for i := 0; i < 3; i++ {
		bids = append(bids, seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", fmt.Sprintf("mac:review:%d", i), base.Add(time.Duration(i)*time.Minute), "medium"))
	}
	correlateOnce(t, s, base.Add(3*time.Minute))
	list, err := s.ListIncidents(context.Background(), 50, 0, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("incident: %+v %v", list, err)
	}
	return bids, list[0].IncidentID
}

func TestIncidentMemberEvidenceImmutable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		ref := fmt.Sprintf("mac:epoch:%d", i)
		seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", ref, base.Add(time.Duration(i)*time.Second), "medium")
		payload := []byte(`{"host":` + strings.ReplaceAll(incidentHostJSON(ref, "high"), `"r1"`, `"r2"`) + `}`)
		at := base.Add(time.Minute)
		id := fmt.Sprintf("evt-review-current-%d", i)
		anchor, _ := GroupKeyForEvent("lab", "host.appeared", id, payload)
		_, err := s.PersistEventAndGroup(ctx, RawEvent{EventID: id, EventKey: id, EventType: "host.appeared", SourceScope: "lab", Target: "lan-a", Payload: payload, OccurredAt: at, ReceivedAt: at, SignatureTimestamp: at}, anchor, GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour, GroupingVersion: GroupingScoped})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ProjectReported(ctx, "lab", 64); err != nil {
			t.Fatal(err)
		}
	}
	correlateOnce(t, s, base.Add(90*time.Second))
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("three original qualified reports must form one incident: %+v", list)
	}
	for _, in := range list {
		if in.IdentityRevision != "r1" {
			t.Fatalf("r1 event evidence was relabelled as %s", in.IdentityRevision)
		}
	}
}

func TestIncidentFanoutBoundIsAtomic(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < maxIncidentDecisionMembers; i++ {
		seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", fmt.Sprintf("mac:bound:%d", i), base.Add(time.Duration(i)*time.Millisecond), "medium")
	}
	correlateOnce(t, s, base.Add(time.Minute))
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 || list[0].MemberCount != maxIncidentDecisionMembers {
		t.Fatalf("bounded incident: %+v %v", list, err)
	}
	in := DecisionInput{SubjectKind: DecisionSubjectIncident, SubjectID: list[0].IncidentID, Kind: DecisionExpected, RequestID: "at-limit"}
	res, err := s.AppendIncidentDecision(ctx, in, true)
	if err != nil || len(res.Members) != maxIncidentDecisionMembers {
		t.Fatalf("limit must succeed: %d %v", len(res.Members), err)
	}
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:bound:extra", base.Add(time.Second), "medium")
	correlateOnce(t, s, base.Add(time.Minute))
	in.RequestID = "over-limit"
	if _, err := s.AppendIncidentDecision(ctx, in, true); !errors.Is(err, ErrIncidentFanoutLimit) {
		t.Fatalf("oversized fanout must reject: %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operator_decisions WHERE request_id=?`, in.RequestID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected action wrote parent: %d %v", count, err)
	}
	// Matching v26 replay hashes remain valid without a bulk audit rewrite.
	legacy := decisionHash(in.SubjectKind, in.SubjectID, in.Kind, 0, false, "", "")
	if _, err := s.db.Exec(`UPDATE operator_decisions SET content_hash=? WHERE request_id='at-limit'`, legacy); err != nil {
		t.Fatal(err)
	}
	in.RequestID = "at-limit"
	again, err := s.AppendIncidentDecision(ctx, in, true)
	if err != nil || again.Applied || len(again.Members) != maxIncidentDecisionMembers {
		t.Fatalf("legacy replay must return its original snapshot: %+v %v", again, err)
	}
}

func TestIncidentPendingSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/pending.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 2; i++ {
		seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", fmt.Sprintf("mac:pending:%d", i), base.Add(time.Duration(i)*time.Second), "medium")
	}
	if rep := correlateOnce(t, s, base.Add(30*time.Second)); rep.Created != 0 {
		t.Fatal(rep)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:pending:2", base.Add(time.Minute), "medium")
	if rep := correlateOnce(t, s, base.Add(90*time.Second)); rep.Created != 1 {
		t.Fatalf("pending references lost across restart: %+v", rep)
	}
}

func TestIncidentPinFenceProtectsPostDiscoveryMembership(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	var first string
	for i := 0; i < 3; i++ {
		bid := seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", fmt.Sprintf("mac:fence:%d", i), base.Add(time.Duration(i)*time.Second), "medium")
		if i == 0 {
			first = bid
		}
	}
	tok, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.7.19")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE briefings SET model_raw_json=? WHERE briefing_id=?`, fmt.Sprintf(`{"host":%q}`, tok), first); err != nil {
		t.Fatal(err)
	}
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.changed", "mac:fence:fresh", base.Add(2*time.Hour), "low")
	cut := nanos(base.Add(time.Hour))
	decCut := nanos(base.Add(-24 * time.Hour))
	bf, df, err := s.refillProtectedTokens(ctx, cut, decCut)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := s.incidentPinVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	correlateOnce(t, s, base.Add(time.Minute))
	if _, err := s.db.Exec(`UPDATE pseudonym_map SET last_used_at=? WHERE token=?`, nanos(base.Add(-48*time.Hour)), tok); err != nil {
		t.Fatal(err)
	}
	rep, _, err := s.purgeBatch(ctx, base.Add(2*time.Hour), purgePlan{stages: stageSubjects, tokenCut: cut, tokenFence: bf, tokenDecFence: df, tokenIncidentFence: pin, decisionCut: decCut})
	if err != nil || rep.Pseudonyms != 0 {
		t.Fatalf("new incident pin must defer token deletion: %+v %v", rep, err)
	}
}

func TestIncidentReferenceUpgradeAndPurgeBudget(t *testing.T) {
	path := t.TempDir() + "/upgrade.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	base := time.Now().UTC().Truncate(time.Second)
	_, id := regressionIncident(t, s, base)
	for _, stmt := range []string{`DROP TABLE incident_pending_refs`, `DROP TABLE incident_pending`, `DROP TABLE incident_references`, `DROP TABLE IF EXISTS incident_summaries`, `ALTER TABLE incidents DROP COLUMN identity_namespace`, `ALTER TABLE incident_progress RENAME COLUMN last_briefing_seq TO last_briefing_rowid`,
		`ALTER TABLE incident_progress DROP COLUMN pending_after`, `ALTER TABLE incident_progress DROP COLUMN pin_version`, `ALTER TABLE incident_progress DROP COLUMN refs_cursor`, `ALTER TABLE incident_progress DROP COLUMN refs_through`, `ALTER TABLE purge_runs DROP COLUMN incident_pending_deleted`, `ALTER TABLE purge_runs DROP COLUMN incident_pending_refs_deleted`, `ALTER TABLE purge_runs DROP COLUMN incident_refs_deleted`, `ALTER TABLE purge_runs DROP COLUMN summaries_deleted`, `PRAGMA user_version=26`} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	correlateOnce(t, s, base.Add(3*time.Minute))
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:upgrade:extra", base.Add(3*time.Minute), "high")
	correlateOnce(t, s, base.Add(210*time.Second))
	in, err := s.GetIncident(context.Background(), id)
	if err != nil || in.QualifiedRefs != 4 || in.Floor != "high" {
		t.Fatalf("upgrade lost aggregate references: %+v %v", in, err)
	}
	// More reference rows than a purge transaction must drain over pages,
	// counted against the same budget as memberships and ledger writes.
	if err := s.write(context.Background(), func(tx *sql.Tx) error {
		for i := 0; i < 450; i++ {
			if _, err := tx.Exec(`INSERT INTO incident_references(incident_id,reference) VALUES (?,?)`, id, fmt.Sprintf("synthetic-%d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	correlateOnce(t, s, base.Add(time.Hour))
	plan := purgePlan{stages: stageBriefings, incidentCut: nanos(base.Add(2 * time.Hour))}
	for i := 0; i < 20; i++ {
		rep, more, err := s.purgeBatch(context.Background(), base.Add(3*time.Hour), plan)
		if err != nil {
			t.Fatal(err)
		}
		if rep.total()+rep.Bookkeeping > maxPurgeRowsPerTxn {
			t.Fatalf("purge exceeded budget: %+v", rep)
		}
		if !more {
			break
		}
	}
	if _, err := s.GetIncident(context.Background(), id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("closed incident must drain after its reference pages: %v", err)
	}
}

func BenchmarkIncidentCorrelate503(b *testing.B) {
	for _, quiet := range []bool{false, true} {
		name := "formation"
		if quiet {
			name = "quiet"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				s, err := Open(b.TempDir() + "/bench.db")
				if err != nil {
					b.Fatal(err)
				}
				bindIncidentSource(b, s, "lab")
				base := time.Now().UTC().Truncate(time.Second)
				for j := 0; j < 503; j++ {
					seedIncidentBriefing(b, s, "lab", "lan-a", "host.appeared", fmt.Sprintf("mac:bench:%d", j), base.Add(time.Duration(j)*time.Millisecond), "medium")
				}
				if quiet {
					correlateOnce(b, s, base.Add(time.Minute))
				}
				b.StartTimer()
				rep, err := s.CorrelateIncidents(context.Background(), incidentSettings(), base.Add(time.Minute))
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				if !quiet && rep.Created != 1 {
					b.Fatalf("503 members should form one incident: %+v", rep)
				}
				if err := s.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestIncidentMemberMappingsRetained(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	bids, _ := regressionIncident(t, s, base)
	tok, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.7.99")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO agent_traces (briefing_id,step,tool,args_json,result_json,status,duration_ms) VALUES (?,0,'get_reported_profile',?,'{}','ok',0)`, bids[0], fmt.Sprintf(`{"host":%q}`, tok)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE pseudonym_map SET last_used_at=? WHERE token=?`, nanos(base.Add(-48*time.Hour)), tok); err != nil {
		t.Fatal(err)
	}
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.changed", "mac:review:fresh", base.Add(2*time.Hour), "low")
	if _, err := s.PurgeOnce(ctx, PurgeSettings{BriefingRetention: time.Hour, SubjectRetention: 90 * 24 * time.Hour}, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BriefingByID(ctx, bids[0]); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pseudonym_map WHERE token=?`, tok).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("retained incident trace lost its restoration mapping")
	}
}

func TestIncidentDecisionScopeConflict(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, iid := regressionIncident(t, s, time.Now().UTC().Truncate(time.Second))
	in := DecisionInput{SubjectKind: DecisionSubjectIncident, SubjectID: iid, Kind: DecisionExpected, RequestID: "review-member-scope"}
	if _, err := s.AppendIncidentDecision(ctx, in, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendIncidentDecision(ctx, in, true); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changing member scope must conflict, got %v", err)
	}
}

func TestIncidentThinBurstAcrossBacklog(t *testing.T) {
	s := openTestStore(t)
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 1100; i++ {
		typ := "host.changed"
		if i == 599 || i == 799 || i == 999 {
			typ = "host.appeared"
		}
		seedIncidentBriefing(t, s, "lab", "lan-a", typ, fmt.Sprintf("mac:load:%d", i), base, "medium")
	}
	for pass := 0; pass < 3; pass++ {
		correlateOnce(t, s, base.Add(time.Duration(pass+1)*30*time.Second))
	}
	list, err := s.ListIncidents(context.Background(), 50, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("three qualified same-window reports were skipped behind unrelated reports: %d incidents", len(list))
	}
}

func TestIncidentPendingMemberDelaysClosure(t *testing.T) {
	for _, backlog := range []int{0, 450} {
		t.Run(fmt.Sprintf("backlog-%d", backlog), func(t *testing.T) {
			s := openTestStore(t)
			bindIncidentSource(t, s, "lab")
			base := time.Now().UTC().Truncate(time.Second)
			for i := 0; i < 3; i++ {
				seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", fmt.Sprintf("mac:quiet:%d", i), base.Add(time.Duration(i)*30*time.Second), "medium")
			}
			correlateOnce(t, s, base.Add(90*time.Second))
			for i := 0; i < backlog; i++ {
				seedIncidentBriefing(t, s, "lab", "lan-a", "host.changed", fmt.Sprintf("mac:quiet:unrelated:%d", i), base.Add(100*time.Second), "medium")
			}
			seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:quiet:3", base.Add(179*time.Second), "medium")
			correlateOnce(t, s, base.Add(180*time.Second))
			list, err := s.ListIncidents(context.Background(), 50, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 || list[0].Status != IncidentOpen || list[0].MemberCount != 4 {
				t.Fatalf("matching member completed before quiet deadline must keep incident open: %+v", list)
			}
		})
	}
}
