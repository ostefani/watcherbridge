package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// agentSeed persists one host.changed event with an open port, projects
// it, and returns the group ID and anchor. Projection links the group
// to real subjects, so the tool-registry tests exercise stored rows.
func agentSeed(t *testing.T, s *Store) (groupID, anchor string) {
	t.Helper()
	anchor = "host:agent-test"
	now := time.Now().UTC().Truncate(time.Second)
	res, err := s.PersistEventAndGroup(context.Background(), RawEvent{
		EventID: "evt-agent-1", EventKey: "k", EventType: "host.changed",
		OccurredAt: now, ReceivedAt: now, SignatureTimestamp: now,
		Payload: []byte(`{"event_id":"evt-agent-1","previous":{"ip":"192.168.0.5","hostname":"wks1","mac":"aa:bb:cc:dd:ee:01","weak":false,"alive":true,"identity_confidence":"high","ports":[]},"current":{"ip":"192.168.0.5","hostname":"wks1","mac":"aa:bb:cc:dd:ee:01","weak":false,"alive":true,"identity_confidence":"high","ports":[{"number":22,"protocol":"tcp","state":"open"}]}}`),
	}, anchor, GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour, MaxEvents: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectReported(context.Background(), "test-scope", 64); err != nil {
		t.Fatal(err)
	}
	return res.GroupID, anchor
}

func TestGroupSubjectsEmpty(t *testing.T) {
	s := openTestStore(t)
	links, truncated, err := s.GroupSubjects(context.Background(), "grp-missing")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 || truncated {
		t.Fatalf("missing group must read empty: %+v %v", links, truncated)
	}
}

func TestGroupSubjectsFromProjection(t *testing.T) {
	s := openTestStore(t)
	gid, _ := agentSeed(t, s)
	links, truncated, err := s.GroupSubjects(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(links) == 0 {
		t.Fatalf("projected group must link subjects: %+v %v", links, truncated)
	}
	for i := 1; i < len(links); i++ {
		prev, cur := links[i-1].ProfileID, links[i].ProfileID
		if prev > cur || (prev == cur && links[i-1].Role >= links[i].Role) {
			t.Fatalf("links must read in profile, role order: %+v", links)
		}
	}
}

func TestBriefingsForSubjectEmpty(t *testing.T) {
	s := openTestStore(t)
	rows, err := s.BriefingsForSubject(context.Background(), "prof-nobody", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("missing subject must read empty: %+v", rows)
	}
}

func TestBriefingsForSubjectRoundTrip(t *testing.T) {
	s := openTestStore(t)
	gid, _ := agentSeed(t, s)
	if _, err := s.CloseDue(context.Background(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	links, _, err := s.GroupSubjects(context.Background(), gid)
	if err != nil || len(links) == 0 {
		t.Fatal("round trip needs a linked subject")
	}
	now := time.Now()
	b := Briefing{BriefingID: "brf-" + gid, GroupID: gid, HostAnchorKey: taken.Anchor,
		Baseline: "medium", Priority: "high", PriorityReason: "r", WhatChanged: "w",
		Context: "c", RecommendedCheck: "k", CreatedAt: now, Source: "model"}
	if err := s.InsertBriefing(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	rows, err := s.BriefingsForSubject(context.Background(), links[0].ProfileID, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].GroupID != gid || rows[0].Priority != "high" || rows[0].Source != "model" {
		t.Fatalf("subject page must carry the verdict row: %+v", rows)
	}
	if rows[0].CreatedAt.IsZero() {
		t.Fatal("subject page must carry timing")
	}
}

func TestLatestSubjectObservationsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	gid, _ := agentSeed(t, s)
	links, _, err := s.GroupSubjects(context.Background(), gid)
	if err != nil || len(links) == 0 {
		t.Fatal("round trip needs a linked subject")
	}
	items, err := s.LatestSubjectObservations(context.Background(), links[0].ProfileID, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 || items[0].EventID != "evt-agent-1" {
		t.Fatalf("latest observations must start at the newest event: %+v", items)
	}
	if items, err := s.LatestSubjectObservations(context.Background(), "prof-nobody", 4); err != nil || len(items) != 0 {
		t.Fatalf("missing subject must read empty: %+v %v", items, err)
	}
}

func TestSimilarReportsRejects(t *testing.T) {
	s := openTestStore(t)
	if _, _, err := s.SimilarReports(context.Background(), "s", "nope", 0, time.Now()); err == nil {
		t.Fatal("unknown kind must refuse")
	}
	if _, _, err := s.SimilarReports(context.Background(), "s", "changed", 99999, time.Now()); err == nil {
		t.Fatal("out-of-range port must refuse")
	}
}

func TestSimilarReportsCounts(t *testing.T) {
	s := openTestStore(t)
	gid, _ := agentSeed(t, s)
	links, _, err := s.GroupSubjects(context.Background(), gid)
	if err != nil || len(links) == 0 {
		t.Fatal("round trip needs a linked subject")
	}
	since := time.Now().Add(-24 * time.Hour)
	count, profiles, err := s.SimilarReports(context.Background(), "test-scope", "changed", 0, since)
	if err != nil {
		t.Fatal(err)
	}
	if count < 1 {
		t.Fatalf("the seeded observation must count: %d", count)
	}
	if len(profiles) == 0 || len(profiles) > 5 {
		t.Fatalf("one to five subject profiles: %+v", profiles)
	}
	found := false
	for _, id := range profiles {
		if id == links[0].ProfileID {
			found = true
		}
	}
	if !found {
		t.Fatalf("seeded subject must be among similar: %+v vs %+v", profiles, links)
	}
	if n, _, err := s.SimilarReports(context.Background(), "test-scope", "changed", 9999, since); err != nil || n != 0 {
		t.Fatalf("unmentioned port must count zero: %d %v", n, err)
	}
	if n, _, err := s.SimilarReports(context.Background(), "test-scope", "changed", 22, since); err != nil || n < 1 {
		t.Fatalf("mentioned port must count: %d %v", n, err)
	}
	if n, _, err := s.SimilarReports(context.Background(), "other-scope", "changed", 0, since); err != nil || n != 0 {
		t.Fatalf("other scopes must not count: %d %v", n, err)
	}
	if n, _, err := s.SimilarReports(context.Background(), "test-scope", "appeared", 0, since); err != nil || n != 0 {
		t.Fatalf("other kinds must not count: %d %v", n, err)
	}
}

func TestSimilarReportsCountsDistinctEvents(t *testing.T) {
	s := openTestStore(t)
	gid, _ := agentSeed(t, s)
	links, _, err := s.GroupSubjects(context.Background(), gid)
	if err != nil || len(links) == 0 {
		t.Fatal("round trip needs a linked subject")
	}
	// The seed persists one host.changed event with previous and current
	// views: two observation rows, one report. The model-visible count
	// must name reports, never observation views.
	since := time.Now().Add(-24 * time.Hour)
	count, _, err := s.SimilarReports(context.Background(), "test-scope", "changed", 0, since)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("one event with two views must count one report: %d", count)
	}
}

func TestObservationPortsBounded(t *testing.T) {
	s := openTestStore(t)
	gid, _ := agentSeed(t, s)
	links, _, err := s.GroupSubjects(context.Background(), gid)
	if err != nil || len(links) == 0 {
		t.Fatal("round trip needs a linked subject")
	}
	items, err := s.LatestSubjectObservations(context.Background(), links[0].ProfileID, 1)
	if err != nil || len(items) == 0 {
		t.Fatal(err)
	}
	lists, cut, err := s.ObservationPorts(context.Background(), []int64{items[0].ObservationID}, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture retains one port; a cap of one keeps it uncut.
	if len(lists[items[0].ObservationID]) != 1 || cut[items[0].ObservationID] {
		t.Fatalf("capped port list wrong: %+v %+v", lists, cut)
	}
	lists, cut, err = s.ObservationPorts(context.Background(), []int64{items[0].ObservationID, -7}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// perObs clamps to at least one; the missing ID reads empty.
	if len(lists[-7]) != 0 {
		t.Fatalf("missing observation must read empty: %+v", lists)
	}
}

// The Step 26 tool reads must ride indexes: profile links, subject
// history, similar counts, latest evidence, and port batches. A scan
// on any of them fails the test.
func TestAgentReadsUseIndexes(t *testing.T) {
	s := openTestStore(t)
	queries := []string{
		`SELECT profile_id FROM group_subjects WHERE group_id = 'g' ORDER BY profile_id ASC, role ASC LIMIT 65`,
		`SELECT b.briefing_id FROM briefings b JOIN briefing_subjects s ON s.briefing_id = b.briefing_id WHERE s.profile_id = 'p' ORDER BY b.created_at DESC, b.rowid DESC LIMIT 10`,
		`SELECT COUNT(DISTINCT o.event_id) FROM reported_observations o JOIN reported_subjects s ON s.profile_id = o.profile_id JOIN raw_events e ON e.event_id = o.event_id WHERE s.source_scope = 's' AND e.event_type = 'host.changed' AND o.received_at >= 0`,
		`SELECT o.profile_id FROM reported_observations o JOIN reported_subjects s ON s.profile_id = o.profile_id JOIN raw_events e ON e.event_id = o.event_id WHERE s.source_scope = 's' AND e.event_type = 'host.changed' AND o.received_at >= 0 GROUP BY o.profile_id ORDER BY MAX(o.received_at) DESC, MAX(o.observation_id) DESC LIMIT 5`,
		`SELECT observation_id FROM reported_observations WHERE profile_id = 'p' ORDER BY reported_at DESC, observation_id DESC LIMIT 10`,
		`SELECT proto FROM reported_ports WHERE observation_id = 1 ORDER BY port, proto LIMIT 65`,
	}
	for _, q := range queries {
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN ` + q)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plan = append(plan, detail)
			if strings.Contains(detail, "SCAN ") {
				rows.Close()
				t.Fatalf("agent read scans: %s (%s)", detail, q)
			}
		}
		rows.Close()
		if len(plan) == 0 {
			t.Fatalf("no plan for %s", q)
		}
	}
}
