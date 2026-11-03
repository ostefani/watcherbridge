package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestIncidentEarlierBurstSurvivesIsolatedReport(t *testing.T) {
	s := openTestStore(t)
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	for i, sec := range []int{0, 1, 2, 303} {
		seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", fmt.Sprintf("mac:earlier:%d", i), base.Add(time.Duration(sec)*time.Second), "medium")
	}
	correlateOnce(t, s, base.Add(304*time.Second))
	list, err := s.ListIncidents(context.Background(), 50, 0, 0)
	if err != nil || len(list) != 1 || list[0].MemberCount != 3 {
		t.Fatalf("earlier burst must form: %+v %v", list, err)
	}
}

func TestIncidentRetainedDecisionPinsWholeBundle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	bids, id := regressionIncident(t, s, base)

	tok, err := s.GetOrAssignPseudonym(ctx, "ip", "192.168.17.29")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO agent_traces(briefing_id,step,tool,args_json,result_json,status,duration_ms) VALUES (?,0,'get_reported_profile',?,'{}','ok',0)`, bids[0], fmt.Sprintf(`{"host":%q}`, tok)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE incidents SET status='closed',closed_at=? WHERE incident_id=?`, nanos(base.Add(4*time.Minute)), id); err != nil {
		t.Fatal(err)
	}
	res, err := s.AppendIncidentDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectIncident, SubjectID: id, Kind: DecisionNote, Text: "day 89 review", RequestID: "late-review"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE operator_decisions SET created_at=? WHERE decision_id=?`, nanos(base.Add(89*24*time.Hour)), res.Parent.DecisionID); err != nil {
		t.Fatal(err)
	}
	settings := PurgeSettings{BriefingRetention: 24 * time.Hour, SubjectRetention: 90 * 24 * time.Hour, IncidentRetention: 90 * 24 * time.Hour, DecisionRetention: 90 * 24 * time.Hour}
	if _, err = s.PurgeOnce(ctx, settings, base.Add(91*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var count int
	for _, table := range []string{"incidents", "incident_members", "incident_references"} {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE incident_id=?`, id).Scan(&count); err != nil || count == 0 {
			t.Fatalf("retained decision lost %s: %d %v", table, count, err)
		}
	}
	for _, bid := range bids {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM briefings WHERE briefing_id=?`, bid).Scan(&count); err != nil || count != 1 {
			t.Fatalf("retained member lost: %d %v", count, err)
		}
	}

	for _, table := range []string{"briefing_subjects", "agent_traces"} {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE briefing_id=?`, bids[0]).Scan(&count); err != nil || count == 0 {
			t.Fatalf("decision pin lost %s: %d %v", table, count, err)
		}
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pseudonym_map WHERE token=?`, tok).Scan(&count); err != nil || count != 1 {
		t.Fatalf("decision pin lost mapping: %d %v", count, err)
	}
	if _, err = s.PurgeOnce(ctx, settings, base.Add(180*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetIncident(ctx, id); err != sql.ErrNoRows {
		t.Fatalf("expired decision must release incident: %v", err)
	}
}

func TestIncidentLegacyLargeReplay(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	bids, id := regressionIncident(t, s, base)
	in := DecisionInput{SubjectKind: DecisionSubjectIncident, SubjectID: id, Kind: DecisionNote, Text: "legacy review", RequestID: "legacy-large"}
	res, err := s.AppendIncidentDecision(ctx, in, true)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the committed v26 snapshot, which had no admission cap.
	if err = s.write(ctx, func(tx *sql.Tx) error {
		legacy := decisionHash(in.SubjectKind, in.SubjectID, in.Kind, 0, false, in.Text, "")
		if _, err := tx.Exec(`UPDATE operator_decisions SET content_hash=? WHERE decision_id=?`, legacy, res.Parent.DecisionID); err != nil {
			return err
		}
		for i := 3; i < 1201; i++ {
			if _, err := tx.Exec(`INSERT INTO operator_decisions(decision_id,subject_kind,subject_id,kind,text,created_at,generated_by) VALUES (?,'briefing',?,'note',?,?,?)`, fmt.Sprintf("dec-legacy-%d", i), bids[i%len(bids)], in.Text, nanos(base), res.Parent.DecisionID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	again, err := s.AppendIncidentDecision(ctx, in, true)
	if err != nil || again.Applied || again.Parent.DecisionID != res.Parent.DecisionID || len(again.Members) == 0 {
		t.Fatalf("legacy successful operation must replay: members=%d applied=%v err=%v", len(again.Members), again.Applied, err)
	}
	if len(again.Members) != 500 || again.MembersNext == 0 {
		t.Fatalf("large replay must be paged: %d next=%d", len(again.Members), again.MembersNext)
	}
	seen := map[string]bool{}
	for _, d := range again.Members {
		seen[d.DecisionID] = true
	}
	after := again.MembersNext
	for after > 0 {
		page, err := s.GeneratedDecisionPage(ctx, res.Parent.DecisionID, 200, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Members) > 200 || (page.Next > 0 && page.Next <= after) {
			t.Fatalf("unbounded or stalled page: %+v", page)
		}
		for _, d := range page.Members {
			if seen[d.DecisionID] {
				t.Fatalf("duplicate member %s", d.DecisionID)
			}
			seen[d.DecisionID] = true
		}
		after = page.Next
	}
	if len(seen) != 1201 {
		t.Fatalf("legacy snapshot truncated: %d", len(seen))
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operator_decisions WHERE generated_by=?`, res.Parent.DecisionID).Scan(&count); err != nil || count != 1201 {
		t.Fatalf("replay changed audit: %d %v", count, err)
	}
	if _, err := s.AppendIncidentDecision(ctx, in, false); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("scope change must conflict: %v", err)
	}
}

func TestIncidentAlternativeWindowProgressSurvivesUpgradeAndRestart(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/alternative.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 23; i++ {
		sec := i
		if i >= 3 {
			sec = 30 + (i-3)*10
		}
		seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", fmt.Sprintf("mac:search:%d", i), base.Add(time.Duration(sec)*time.Second), "medium")
	}
	// Queue the candidates without forming an incident, then preserve them
	// across the metadata-only v27 upgrade.
	if _, err = s.CorrelateIncidents(ctx, IncidentSettings{Window: 5 * time.Second, Quiet: 1000 * time.Second, MinRefs: 500}, base.Add(240*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`ALTER TABLE incident_pending DROP COLUMN formation_before`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`PRAGMA user_version=27`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	settings := IncidentSettings{Window: 5 * time.Second, Quiet: 1000 * time.Second, MinRefs: 3}
	var report IncidentReport
	more, err := s.correlatePass(ctx, settings, base.Add(240*time.Second), &report)
	if err != nil || !more || report.Created != 0 {
		t.Fatalf("first bounded search: %+v more=%v %v", report, more, err)
	}
	var before int64
	if err = s.db.QueryRow(`SELECT MAX(formation_before) FROM incident_pending`).Scan(&before); err != nil || before == 0 {
		t.Fatalf("search did not checkpoint: %d %v", before, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err = s.CorrelateIncidents(ctx, settings, base.Add(240*time.Second))
	if err != nil || report.Created != 1 {
		t.Fatalf("restarted search lost older window: %+v %v", report, err)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 || list[0].MemberCount != 3 {
		t.Fatalf("wrong recovered burst: %+v %v", list, err)
	}
}
