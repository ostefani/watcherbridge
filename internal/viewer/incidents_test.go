package viewer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

// Incident routes (Step 29): bounded list/detail reads, incident
// subject decisions with explicit member scope, and overview collapse
// of member rows behind attention-state incidents.

// seedIncidentBriefing persists one appeared event with stable-identity
// evidence, projects it, and completes a briefing at the report time.
func seedIncidentBriefing(t *testing.T, s *store.Store, scope, target, ref string, at time.Time, priority string) string {
	t.Helper()
	ctx := context.Background()
	eventID := fmt.Sprintf("evt-v-%s-%s-%d", scope, ref, at.UnixNano())
	host := fmt.Sprintf(`{"host_id":%q,"identity_source":"mac","identity_namespace":"ns-1","identity_revision":"r1","identity_confidence":"high"}`, ref)
	payload := []byte(fmt.Sprintf(`{"event_id":%q,"event_key":%q,"event_type":"host.appeared","occurred_at":"2026-10-20T12:00:05Z","cycle":7,"target":%q,"host":%s}`,
		eventID, "kv-"+ref, target, host))
	anchor, _ := store.GroupKeyForEvent(scope, "host.appeared", eventID, payload)
	res, err := s.PersistEventAndGroup(ctx, store.RawEvent{
		EventID: eventID, EventKey: "kv-" + ref, EventType: "host.appeared",
		OccurredAt: at, Cycle: 1, Target: target, Payload: payload,
		ReceivedAt: at, SignatureTimestamp: at, SourceScope: scope,
	}, anchor, store.GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour, GroupingVersion: store.GroupingScoped})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectReported(ctx, scope, 64); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, res.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	bid := "brf-" + taken.GroupID
	if err := s.InsertBriefing(ctx, store.Briefing{
		BriefingID: bid, GroupID: taken.GroupID, HostAnchorKey: anchor,
		Baseline: "low", Priority: priority, PriorityReason: "test",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	return bid
}

// formViewerIncident binds the identity contract, seeds a three-ref
// burst and correlates it. It returns the incident ID.
func formViewerIncident(t *testing.T, s *store.Store, base time.Time) string {
	t.Helper()
	ctx := context.Background()
	if err := s.BindSourceIdentity(ctx, "lab", "ns-1", store.StableIdentityCapability); err != nil {
		t.Fatal(err)
	}
	for i, ref := range []string{"mac:w:1", "mac:w:2", "mac:w:3"} {
		seedIncidentBriefing(t, s, "lab", "lan-a", ref, base.Add(time.Duration(i)*time.Minute), "high")
	}
	rep, err := s.CorrelateIncidents(ctx, store.IncidentSettings{
		Window: 5 * time.Minute, Quiet: 120 * time.Second, MinRefs: 3,
	}, base.Add(3*time.Minute))
	if err != nil || rep.Created != 1 {
		t.Fatalf("form = %+v %v", rep, err)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	return list[0].IncidentID
}

func getJSON(t *testing.T, mux http.Handler, target string) (int, []byte) {
	t.Helper()
	rec := serveMux(mux, "GET", target, "127.0.0.1:8081", nil, "")
	return rec.Code, rec.Body.Bytes()
}

func TestIncidentListAndDetail(t *testing.T) {
	s := openViewerStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	iid := formViewerIncident(t, s, base)
	_, mux := withHost(t, s, "127.0.0.1:8081")

	code, body := getJSON(t, mux, "/api/incidents")
	if code != http.StatusOK {
		t.Fatalf("list = %d: %s", code, body)
	}
	var list []incidentWire
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].IncidentID != iid {
		t.Fatalf("list = %+v", list)
	}
	row := list[0]
	if row.Kind != "burst_appearance" || row.Status != "open" || row.Floor != "high" {
		t.Fatalf("row = %+v", row)
	}
	if row.QualifiedRefs != 3 || row.MemberCount != 3 || row.Cursor == "" {
		t.Fatalf("counts/cursor = %+v", row)
	}
	if row.SourceScope != "lab" || row.Target != "lan-a" {
		t.Fatalf("scope = %+v", row)
	}

	code, body = getJSON(t, mux, "/api/incidents/"+iid)
	if code != http.StatusOK {
		t.Fatalf("detail = %d: %s", code, body)
	}
	var detail incidentDetailWire
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Incident.IncidentID != iid || len(detail.Members) != 3 || detail.Truncated {
		t.Fatalf("detail = %+v", detail)
	}
	for _, m := range detail.Members {
		if len(m.Refs) != 1 || m.Refs[0].Revision != "r1" || m.Priority == "" || m.CreatedAt == "" {
			t.Fatalf("member = %+v", m)
		}
	}

	// List paging continues past the first row's cursor.
	code, body = getJSON(t, mux, "/api/incidents?before="+row.Cursor)
	if code != http.StatusOK {
		t.Fatalf("paged list = %d: %s", code, body)
	}
	var next []incidentWire
	if err := json.Unmarshal(body, &next); err != nil || len(next) != 0 {
		t.Fatalf("paged list = %+v %v, want empty", next, err)
	}

	for _, tc := range []struct{ name, target string }{
		{"bad id", "/api/incidents/nope"},
		{"unknown id", "/api/incidents/inc-1-2"},
		{"bad limit", "/api/incidents?limit=0"},
		{"bad before", "/api/incidents?before=zzz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := getJSON(t, mux, tc.target); code != http.StatusOK && code < 400 {
				t.Fatalf("status = %d, want 4xx", code)
			} else if code == http.StatusOK {
				t.Fatalf("status = 200, want 4xx for %s", tc.target)
			}
		})
	}
}

func TestIncidentDecisionAPI(t *testing.T) {
	s := openViewerStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	iid := formViewerIncident(t, s, base)
	_, mux := withHost(t, s, "127.0.0.1:8081")
	origin := "http://127.0.0.1:8081"

	// Member scope is explicit: absent on incidents is 400, present
	// on briefings is 400.
	rec := postDecision(t, mux, "/api/decisions",
		`{"subject_kind":"incident","subject_id":"`+iid+`","kind":"expected","request_id":"i-1"}`, origin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing scope = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	gid := seedPriorityBriefing(t, s, "host:dec-inc", "evt-dec-inc", "low", base)
	rec = postDecision(t, mux, "/api/decisions",
		`{"subject_kind":"briefing","subject_id":"`+gid+`","kind":"expected","request_id":"i-2","include_members":false}`, origin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("misplaced scope = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// Unknown incident IDs are 404, never silent success.
	rec = postDecision(t, mux, "/api/decisions",
		`{"subject_kind":"incident","subject_id":"inc-1-2","kind":"expected","request_id":"i-3","include_members":false}`, origin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown incident = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	// Incident-only acknowledgement fans out to nothing.
	rec = postDecision(t, mux, "/api/decisions",
		`{"subject_kind":"incident","subject_id":"`+iid+`","kind":"expected","request_id":"i-4","include_members":false}`, origin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("incident-only = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var only mutationWire
	if err := json.Unmarshal(rec.Body.Bytes(), &only); err != nil {
		t.Fatal(err)
	}
	if !only.Applied || len(only.Members) != 0 {
		t.Fatalf("incident-only = %+v, want applied with no members", only)
	}
	// Include-members fans out to the three members.
	rec = postDecision(t, mux, "/api/decisions",
		`{"subject_kind":"incident","subject_id":"`+iid+`","kind":"note","text":"burst reviewed","request_id":"i-5","include_members":true}`, origin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fan-out = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var fan mutationWire
	if err := json.Unmarshal(rec.Body.Bytes(), &fan); err != nil {
		t.Fatal(err)
	}
	if !fan.Applied || len(fan.Members) != 3 {
		t.Fatalf("fan-out = %+v, want applied with 3 members", fan)
	}
	// The detail carries the incident audit trail with the live
	// review state for its decision panel.
	code, body := getJSON(t, mux, "/api/incidents/"+iid)
	if code != http.StatusOK {
		t.Fatalf("detail = %d: %s", code, body)
	}
	var detail incidentDetailWire
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Decisions) != 2 {
		t.Fatalf("detail decisions = %d, want incident-only + fan-out parent", len(detail.Decisions))
	}
	// Notes never enter the review lane: the head stays the
	// earlier incident-only acknowledgement.
	if detail.EffectiveReview == nil || detail.EffectiveReview.Kind != "expected" {
		t.Fatalf("effective review = %+v, want the expected head", detail.EffectiveReview)
	}
	// Undo of the fan-out parent identifies the generated rows
	// without touching them.
	rec = serveMux(mux, "POST", "/api/decisions/"+fan.Decision.DecisionID+"/undo", "127.0.0.1:8081",
		map[string]string{"Content-Type": "application/json", "Origin": origin}, `{"request_id":"i-6"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var undone mutationWire
	if err := json.Unmarshal(rec.Body.Bytes(), &undone); err != nil {
		t.Fatal(err)
	}
	if len(undone.Generated) != 3 {
		t.Fatalf("undo generated = %+v, want 3 IDs", undone)
	}
}

func TestOverviewIncidentCollapse(t *testing.T) {
	s := openViewerStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	iid := formViewerIncident(t, s, base)
	_, mux := withHost(t, s, "127.0.0.1:8081")

	attentionIDs := func(t *testing.T) (incidents, briefings []string) {
		t.Helper()
		code, body := getJSON(t, mux, "/api/overview")
		if code != http.StatusOK {
			t.Fatalf("overview = %d: %s", code, body)
		}
		var ov overviewWire
		if err := json.Unmarshal(body, &ov); err != nil {
			t.Fatal(err)
		}
		for _, a := range ov.Attention {
			if a.Kind == "incident" {
				incidents = append(incidents, a.ID)
			} else {
				briefings = append(briefings, a.ID)
			}
		}
		return incidents, briefings
	}
	// The incident row represents its members: three member rows
	// collapse out of the display cut.
	kinds, bids := attentionIDs(t)
	if len(kinds) != 1 || kinds[0] != iid {
		t.Fatalf("incidents = %v, want [%s]", kinds, iid)
	}
	if len(bids) != 0 {
		t.Fatalf("member rows must collapse: %v", bids)
	}
	// Flagging one member keeps it visible beside the incident.
	members, err := s.IncidentMemberBriefings(context.Background(), []string{iid})
	if err != nil || len(members[iid]) != 3 {
		t.Fatal(err)
	}
	if _, err := s.AppendDecision(context.Background(), store.DecisionInput{
		SubjectKind: store.DecisionSubjectBriefing, SubjectID: members[iid][0],
		Kind: store.DecisionNotExpected, RequestID: "c-flag",
	}); err != nil {
		t.Fatal(err)
	}
	kinds, bids = attentionIDs(t)
	if len(kinds) != 1 || len(bids) != 1 || bids[0] != members[iid][0] {
		t.Fatalf("flagged member must stay: incidents=%v briefings=%v", kinds, bids)
	}
	// Incident-only acknowledgement releases the members: the
	// incident leaves attention and the unflagged members return.
	if _, err := s.AppendIncidentDecision(context.Background(), store.DecisionInput{
		SubjectKind: store.DecisionSubjectIncident, SubjectID: iid,
		Kind: store.DecisionExpected, RequestID: "c-ack",
	}, false); err != nil {
		t.Fatal(err)
	}
	kinds, bids = attentionIDs(t)
	if len(kinds) != 0 {
		t.Fatalf("acked incident must leave: %v", kinds)
	}
	// Two unflagged members return; the flagged one stays flagged.
	if len(bids) != 3 {
		t.Fatalf("members must return after incident-only ack: %v", bids)
	}
}
