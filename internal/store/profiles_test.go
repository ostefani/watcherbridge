package store

import (
	"context"
	"database/sql"
	"encoding/json"
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

// History items carry the snapshot's port summary, so a history page
// never reports zero ports alongside a snapshot that captured some.
func TestSubjectHistoryCarriesPortTotals(t *testing.T) {
	s := openTestStore(t)
	at := time.Now().UTC().Truncate(time.Second)
	host := `{"mac":"10:20:30:40:50:65","host_id":"mac:10:20:30:40:50:65","identity_source":"mac","identity_confidence":"high","ports":[{"number":22,"protocol":"tcp","state":"open","service":"ssh"},{"number":80,"protocol":"tcp","state":"open","service":"http"}]}`
	seedSubject(t, s, "lab", "evt-ports", host, at)
	subs, _, err := s.ReportedSubjectsPage(context.Background(), 0, "", 10)
	if err != nil || len(subs) != 1 {
		t.Fatalf("subjects = %+v %v", subs, err)
	}
	page, _, err := s.SubjectHistoryPage(context.Background(), subs[0].ProfileID, 0, 0, 50)
	if err != nil || len(page) != 1 {
		t.Fatalf("history = %+v %v", page, err)
	}
	if page[0].PortsTotal != 2 || page[0].PortsTruncated {
		t.Fatalf("history must carry snapshot totals: %+v attrs %v", page[0], page[0].Attributes)
	}
}

// Fifty fat snapshots exceed the page byte budget: the page stops at
// the last fitting item, continues from there, and the walk still
// covers every observation exactly once.
func TestSubjectHistoryByteBudget(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	const items = 20
	for i := 0; i < items; i++ {
		seedSubject(t, s, "lab", fmt.Sprintf("evt-fat-%02d", i), subjectMAC("10:20:30:40:50:66"), base.Add(time.Duration(i)*time.Second))
	}
	subs, _, err := s.ReportedSubjectsPage(ctx, 0, "", 10)
	if err != nil || len(subs) != 1 {
		t.Fatalf("subjects = %+v %v", subs, err)
	}
	profileID := subs[0].ProfileID
	fat, err := json.Marshal(map[string]any{"ports_total": 0, "ports_truncated": false, "pad": strings.Repeat("x", 100<<10)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE reported_observations SET attributes_json = ? WHERE profile_id = ?`, string(fat), profileID); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var afterNanos, afterID int64
	pages := 0
	for {
		page, next, err := s.SubjectHistoryPage(ctx, profileID, afterNanos, afterID, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			if next != "" {
				t.Fatal("empty page must not carry a cursor")
			}
			break
		}
		pages++
		wire, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		if len(wire) > maxHistoryPageBytes+(100<<10) {
			t.Fatalf("page %d escapes the byte budget: %d bytes", pages, len(wire))
		}
		for _, obs := range page {
			if seen[obs.EventID] {
				t.Fatalf("overlap at %s", obs.EventID)
			}
			seen[obs.EventID] = true
		}
		if next == "" {
			break
		}
		last := page[len(page)-1]
		// The published cursor must decode to the last included
		// item: continuation resumes exactly behind it.
		n, idStr, err := parseTestCursor(next)
		if err != nil {
			t.Fatal(err)
		}
		var id int64
		if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
			t.Fatal(err)
		}
		if n != nanos(last.ReportedAt) || id != last.ObservationID {
			t.Fatalf("cursor %q skips the last item %+v", next, last)
		}
		afterNanos, afterID = n, id
		if pages > items {
			t.Fatal("history walk does not converge")
		}
	}
	if len(seen) != items {
		t.Fatalf("walk covered %d of %d observations in %d pages", len(seen), items, pages)
	}
	if pages < 2 {
		t.Fatalf("20x100KiB must not fit one page: %d pages", pages)
	}
}

// Serving reads behind /api/devices must stay indexed as history
// grows: no serving query scans an association or observation table.
func TestServingQueriesUseIndexes(t *testing.T) {
	s := openTestStore(t)
	queries := []string{
		`EXPLAIN QUERY PLAN SELECT ` + subjectColumns + ` FROM reported_subjects WHERE (last_report_at, profile_id) > (0, '') ORDER BY last_report_at, profile_id LIMIT 50`,
		`EXPLAIN QUERY PLAN SELECT observation_id, event_id, view_role, profile_id, reported_at, received_at, attributes_json FROM reported_observations WHERE profile_id = 'prof-x' AND (reported_at, observation_id) > (0, 0) ORDER BY reported_at, observation_id LIMIT 50`,
		`EXPLAIN QUERY PLAN SELECT observation_id, event_id, view_role, profile_id, reported_at, received_at, attributes_json FROM reported_observations WHERE profile_id = 'prof-x' ORDER BY received_at DESC, observation_id DESC LIMIT 1`,
		`EXPLAIN QUERY PLAN SELECT proto, port, reported_state, reported_service FROM reported_ports WHERE observation_id = 1 ORDER BY port, proto LIMIT 501`,
	}
	for _, q := range queries {
		rows, err := s.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			for _, table := range []string{"group_subjects", "briefing_subjects", "reported_observations", "reported_ports", "reported_subjects"} {
				if strings.Contains(detail, "SCAN "+table) {
					rows.Close()
					t.Fatalf("serving query scans %s: %s\n%s", table, detail, q)
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
}
