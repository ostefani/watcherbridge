package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Detail reads serve Step 24: persisted briefing-to-profile links,
// the group's event index, and the retained observations for exactly
// those events. Associations always come from link rows, never from
// re-matching the host string.

// seedBriefedGroup persists one appeared event, projects it, closes
// and takes the group, and stores a briefing. It returns the group,
// briefing and event IDs.
func seedBriefedGroup(t *testing.T, s *Store, scope, eventID, hostJSON string, at time.Time) (groupID, briefingID string) {
	t.Helper()
	ctx := context.Background()
	body := producerPayload(eventID, "k", "host.appeared", hostJSON)
	gid := persistReported(t, s, scope, eventID, "k", "host.appeared", body, at)
	projectAll(t, s)
	if _, err := s.CloseDue(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	briefingID = "brf-" + taken.GroupID
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: briefingID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: "r", Kind: "other", Source: "rules",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	return gid, briefingID
}

func TestBriefingSubjectsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	_, bid := seedBriefedGroup(t, s, "lab", "evt-det-1", subjectMAC("aa:bb:cc:dd:ee:01"), at)

	links, truncated, err := s.BriefingSubjects(ctx, bid)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("single-subject briefing must not truncate")
	}
	if len(links) != 1 || links[0].Role != "host" {
		t.Fatalf("subjects = %+v, want one host link", links)
	}
	if !strings.HasPrefix(links[0].ProfileID, "prof-") {
		t.Fatalf("profile ID %q is not a generated reference", links[0].ProfileID)
	}

	// Previous/current views link with their own roles; the host
	// string is never re-matched, only persisted rows are read.
	body := []byte(`{"event_id":"evt-det-2","event_key":"k","event_type":"host.changed",` +
		`"occurred_at":"2026-10-20T12:00:05Z","cycle":3,"target":"192.168.0.0/24",` +
		`"current":` + subjectMAC("aa:bb:cc:dd:ee:02") + `,"previous":` + subjectMAC("aa:bb:cc:dd:ee:03") + `}`)
	gid := persistReported(t, s, "lab", "evt-det-2", "k", "host.changed", body, at)
	projectAll(t, s)
	if _, err := s.CloseDue(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	bid2 := "brf-" + taken.GroupID
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: bid2, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: "r", Kind: "other", Source: "rules",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	links, truncated, err = s.BriefingSubjects(ctx, bid2)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("two-view change must not truncate")
	}
	roles := map[string]bool{}
	for _, l := range links {
		roles[l.Role] = true
	}
	if !roles["previous"] || !roles["current"] {
		t.Fatalf("roles = %v, want previous and current links", roles)
	}
	// Deterministic profile order across reads.
	again, _, err := s.BriefingSubjects(ctx, bid2)
	if err != nil {
		t.Fatal(err)
	}
	for i := range links {
		if links[i] != again[i] {
			t.Fatalf("unstable order: %+v vs %+v", links, again)
		}
	}

	// Unknown briefings link nothing.
	empty, truncated, err := s.BriefingSubjects(ctx, "brf-nope")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 || truncated {
		t.Fatalf("unknown briefing = %+v/%v, want empty", empty, truncated)
	}
}

func TestBriefingSubjectsTruncate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO report_sources (source_scope) VALUES ('lab')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO active_groups (group_id, host_anchor_key, opened_at, last_event_at, deadline_at, event_count, status)
		VALUES ('grp-det', 'a', 1, 1, 2, 0, 'done')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO briefings (briefing_id, group_id, host_anchor_key, baseline, priority, priority_reason,
		kind, source, what_changed, context, recommended_check, created_at, event_ids_json, seq, grouping_version)
		VALUES ('brf-det', 'grp-det', 'a', 'low', 'low', 'r', 'other', 'rules', 'w', 'c', 'k', 1, '[]', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxBriefingSubjects+5; i++ {
		pid := fmt.Sprintf("prof-%016x", i)
		if _, err := s.db.Exec(`INSERT INTO reported_subjects (profile_id, source_scope, reference_class,
			identity_namespace, upstream_reference, first_report_at, last_report_at)
			VALUES (?, 'lab', 'reported_device', 'ns', ?, 1, 1)`, pid, fmt.Sprintf("ref-%d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO briefing_subjects (briefing_id, profile_id, role) VALUES ('brf-det', ?, 'host')`, pid); err != nil {
			t.Fatal(err)
		}
	}
	links, truncated, err := s.BriefingSubjects(ctx, "brf-det")
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("over-cap links must report truncation")
	}
	if len(links) != maxBriefingSubjects {
		t.Fatalf("got %d links, want cap %d", len(links), maxBriefingSubjects)
	}
	for i := 1; i < len(links); i++ {
		if links[i-1].ProfileID >= links[i].ProfileID {
			t.Fatalf("links not in profile order: %q before %q", links[i-1].ProfileID, links[i].ProfileID)
		}
	}
}

func TestBriefingReferencedEvents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	// Two events sharing the host anchor join one group; the briefing
	// is stored after the join so the group is still open for both.
	persist := func(eventID string) string {
		body := producerPayload(eventID, "k", "host.appeared", subjectMAC("aa:bb:cc:dd:ee:04"))
		return persistReported(t, s, "lab", eventID, "k", "host.appeared", body, at)
	}
	gid := persist("evt-det-3")
	if again := persist("evt-det-4"); again != gid {
		t.Fatalf("second event joined %q, want same group %q", again, gid)
	}
	projectAll(t, s)
	if _, err := s.CloseDue(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: "r", Kind: "other", Source: "rules",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}

	refs, truncated, err := s.BriefingReferencedEvents(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("two-event group must not truncate")
	}
	if len(refs) != 2 || refs[0].EventID != "evt-det-3" || refs[1].EventID != "evt-det-4" {
		t.Fatalf("referenced = %+v, want ordered event pairs", refs)
	}
	for _, r := range refs {
		if r.EventType != "host.appeared" {
			t.Fatalf("referenced = %+v, want recorded types", refs)
		}
	}
}

func TestObservationsForEvents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	_, _ = seedBriefedGroup(t, s, "lab", "evt-det-5", subjectMAC("aa:bb:cc:dd:ee:05"), at)

	obs, truncated, err := s.ObservationsForEvents(ctx, []string{"evt-det-5"})
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(obs) != 1 {
		t.Fatalf("observations = %+v/%v, want exactly this event's row", obs, truncated)
	}
	if obs[0].ViewRole != "host" || len(obs[0].Attributes) == 0 {
		t.Fatalf("observation = %+v, want snapshot attributes", obs[0])
	}

	// Empty and unknown inputs read nothing; a profile's latest
	// observation is never substituted for another event.
	empty, truncated, err := s.ObservationsForEvents(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 || truncated {
		t.Fatalf("nil input = %+v/%v, want empty", empty, truncated)
	}
	miss, _, err := s.ObservationsForEvents(ctx, []string{"evt-nope"})
	if err != nil {
		t.Fatal(err)
	}
	if len(miss) != 0 {
		t.Fatalf("unknown event = %+v, want empty", miss)
	}
}
