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

// Read API over retained reported context: exact keys, bounded pages,
// deterministic tuple cursors. No test here joins on hostname/IP/MAC
// ownership: associations come from explicit link rows only.

func seedSubject(t *testing.T, s *Store, scope, eventID, hostJSON string, at time.Time) string {
	t.Helper()
	body := producerPayload(eventID, "k", "host.appeared", hostJSON)
	gid := persistReported(t, s, scope, eventID, "k", "host.appeared", body, at)
	if _, err := s.ProjectReported(context.Background(), scope, 64); err != nil {
		t.Fatal(err)
	}
	return gid
}

func subjectMAC(mac string) string {
	return fmt.Sprintf(`{"mac":%q,"host_id":"mac:%s","identity_source":"mac","identity_confidence":"high"}`, mac, mac)
}

func TestReportedSubjectsPageCursor(t *testing.T) {
	s := openTestStore(t)
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	for i, mac := range []string{"10:20:30:40:50:61", "10:20:30:40:50:62", "10:20:30:40:50:63"} {
		seedSubject(t, s, "lab", fmt.Sprintf("evt-pg%d", i), subjectMAC(mac), base.Add(time.Duration(i)*time.Hour))
	}
	page1, next, err := s.ReportedSubjectsPage(context.Background(), 0, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || next == "" {
		t.Fatalf("first page = %d items next %q, want 2 and a cursor", len(page1), next)
	}
	if page1[0].UpstreamReference != "mac:10:20:30:40:50:61" || page1[1].UpstreamReference != "mac:10:20:30:40:50:62" {
		t.Fatalf("oldest-first order broken: %+v", page1)
	}
	for _, sub := range page1 {
		if sub.ReferenceClass != "reported_device" || sub.SourceScope != "lab" || sub.AssociationStatus == "" {
			t.Fatalf("class/provenance/association must be explicit: %+v", sub)
		}
	}
	nanos, id, err := parseTestCursor(next)
	if err != nil {
		t.Fatal(err)
	}
	page2, next2, err := s.ReportedSubjectsPage(context.Background(), nanos, id, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || next2 != "" {
		t.Fatalf("second page = %d items next %q, want 1 and exhausted", len(page2), next2)
	}
	if page2[0].UpstreamReference != "mac:10:20:30:40:50:63" {
		t.Fatalf("cursor skipped a subject: %+v", page2)
	}
	if _, _, err := s.ReportedSubjectsPage(context.Background(), -1, "", 2); err == nil {
		t.Fatal("negative cursor must be rejected")
	}
}

func parseTestCursor(s string) (int64, string, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("bad cursor")
	}
	var n int64
	if _, err := fmt.Sscanf(parts[0], "%d", &n); err != nil {
		return 0, "", err
	}
	return n, parts[1], nil
}

func TestReportedProfileUnknown(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.ReportedProfile(context.Background(), "prof-0123456789abcdef"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown profile = %v, want ErrNoRows", err)
	}
	if _, err := s.ReportedProfile(context.Background(), ""); err == nil {
		t.Fatal("empty profile id must be rejected")
	}
}

func TestSubjectHistoryPaging(t *testing.T) {
	s := openTestStore(t)
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	mac := subjectMAC("10:20:30:40:50:64")
	seedSubject(t, s, "lab", "evt-h1", mac, base)
	seedSubject(t, s, "lab", "evt-h2", mac, base.Add(time.Hour))
	subs, _, err := s.ReportedSubjectsPage(context.Background(), 0, "", 10)
	if err != nil || len(subs) != 1 {
		t.Fatalf("same reference must address one subject: %v %v", subs, err)
	}
	profileID := subs[0].ProfileID
	page1, next, err := s.SubjectHistoryPage(context.Background(), profileID, 0, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 1 || next == "" || page1[0].EventID != "evt-h1" {
		t.Fatalf("history oldest-first broken: %+v next %q", page1, next)
	}
	if page1[0].Attributes["mac"] != "10:20:30:40:50:64" {
		t.Fatalf("attributes must carry the reported snapshot: %v", page1[0].Attributes)
	}
	nanos, idStr, err := parseTestCursor(next)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		t.Fatal(err)
	}
	page2, next2, err := s.SubjectHistoryPage(context.Background(), profileID, nanos, id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].EventID != "evt-h2" {
		t.Fatalf("history cursor broken: %+v next %q", page2, next2)
	}
	// Exhaustion is learned on the next page: a full page always carries
	// a cursor, and the following page comes back empty with none.
	nanos2, idStr2, err := parseTestCursor(next2)
	if err != nil {
		t.Fatal(err)
	}
	var id2 int64
	if _, err := fmt.Sscanf(idStr2, "%d", &id2); err != nil {
		t.Fatal(err)
	}
	page3, next3, err := s.SubjectHistoryPage(context.Background(), profileID, nanos2, id2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page3) != 0 || next3 != "" {
		t.Fatalf("exhausted history must page empty: %+v next %q", page3, next3)
	}
	latest, ok, err := s.LatestSubjectObservation(context.Background(), profileID)
	if err != nil || !ok {
		t.Fatalf("latest = %+v %v %v", latest, ok, err)
	}
	if latest.EventID != "evt-h2" {
		t.Fatalf("latest must be the newest receipt, got %s", latest.EventID)
	}
}

func TestSubjectAssociationCounts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	gid := seedSubject(t, s, "lab", "evt-ac", subjectMAC("10:20:30:40:50:65"), at)
	if _, err := s.CloseDue(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: "r",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	subs, _, err := s.ReportedSubjectsPage(ctx, 0, "", 10)
	if err != nil || len(subs) != 1 {
		t.Fatalf("subjects = %v %v", subs, err)
	}
	if subs[0].Groups != 1 || subs[0].Briefings != 1 {
		t.Fatalf("association counts = groups %d briefings %d, want 1/1", subs[0].Groups, subs[0].Briefings)
	}
}

func TestLatestObservationPortsBounded(t *testing.T) {
	s := openTestStore(t)
	at := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	var ports strings.Builder
	for i := 1; i <= 600; i++ {
		if i > 1 {
			ports.WriteByte(',')
		}
		fmt.Fprintf(&ports, `{"number":%d,"protocol":"tcp","state":"open","service":"s%d"}`, i, i)
	}
	body := []byte(`{"event_id":"evt-ports","event_key":"k","event_type":"host.appeared","occurred_at":"2026-10-20T12:00:05Z","cycle":3,
		"host":{"mac":"10:20:30:40:50:66","host_id":"mac:10:20:30:40:50:66","identity_source":"mac","identity_confidence":"high",
		"ports":[` + ports.String() + `]}}`)
	persistReported(t, s, "lab", "evt-ports", "k", "host.appeared", body, at)
	if _, err := s.ProjectReported(context.Background(), "lab", 64); err != nil {
		t.Fatal(err)
	}
	subs, _, err := s.ReportedSubjectsPage(context.Background(), 0, "", 10)
	if err != nil || len(subs) != 1 {
		t.Fatalf("subjects = %v %v", subs, err)
	}
	latest, ok, err := s.LatestSubjectObservation(context.Background(), subs[0].ProfileID)
	if err != nil || !ok {
		t.Fatalf("latest = %+v %v %v", latest, ok, err)
	}
	if len(latest.Ports) != maxProfilePorts || !latest.PortsTruncated || latest.PortsTotal != 600 {
		t.Fatalf("ports bounded at %d total %d truncated %v", len(latest.Ports), latest.PortsTotal, latest.PortsTruncated)
	}
	if latest.Attributes["ports_total"] != float64(600) || latest.Attributes["ports_truncated"] != true {
		t.Fatalf("snapshot flags must agree: %v", latest.Attributes)
	}
}
