package viewer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

// ruleMACPayload reports one open port on the given MAC with a
// scanner-reported reference: the scope rule proposals attach to.
// The namespace alone qualifies nothing: tests bind the
// registration contract explicitly, like an I1-qualified deployment.
func ruleMACPayload(mac string) string {
	view := `{"ip":"192.168.0.5","hostname":"p","mac":"` + mac + `","host_id":"mac:` + mac + `","identity_source":"mac","identity_namespace":"ns-1","identity_revision":"rev-1","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-20T00:00:00Z","ports":[%s]}`
	open := `{"number":22,"protocol":"tcp","state":"open"}`
	return `{"event_id":"e1","previous":` + strings.Replace(view, "%s", "", 1) +
		`,"current":` + strings.Replace(view, "%s", open, 1) + `}`
}

func bindRuleSource(t *testing.T, s *store.Store, scope string) {
	t.Helper()
	if err := s.BindSourceIdentity(context.Background(), scope, "ns-1", string(store.StableIdentityCapability)); err != nil {
		t.Fatal(err)
	}
}

func seedRuleBriefing(t *testing.T, s *store.Store, anchor, eventID, mac string) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	res, err := s.PersistEventAndGroup(context.Background(), store.RawEvent{
		EventID: eventID, EventKey: "k", EventType: "host.changed",
		OccurredAt: now, Payload: []byte(ruleMACPayload(mac)), ReceivedAt: now, SignatureTimestamp: now,
	}, anchor, store.GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectReported(context.Background(), "test-scope", 64); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(context.Background(), res.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(context.Background(), store.Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: anchor,
		Baseline: "high", Priority: "high", PriorityReason: "r", Kind: "other",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return res.GroupID
}

func postRule(t *testing.T, mux http.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postDecision(t, mux, target, body, "http://127.0.0.1:8081")
}

func TestRuleRoutesLifecycle(t *testing.T) {
	s := openViewerStore(t)
	bindRuleSource(t, s, "test-scope")
	const macA = "aa:bb:cc:dd:ee:0a"
	var gids []string
	for i, id := range []string{"evt-ra1", "evt-ra2", "evt-ra3"} {
		gids = append(gids, seedRuleBriefing(t, s, "host:ra", id, macA))
		_ = i
	}
	_, mux := withHost(t, s, "127.0.0.1:8081")
	for i, gid := range gids {
		rec := postRule(t, mux, "/api/decisions", `{"subject_kind":"briefing","subject_id":"`+gid+`","kind":"expected","request_id":"rule-exp-`+string(rune('a'+i))+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected %d: %s", rec.Code, rec.Body.String())
		}
	}
	// The proposal lists once the third judgment lands.
	rec := serveMux(mux, "GET", "/api/rules/proposals", "127.0.0.1:8081", nil, "")
	var proposals []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &proposals); err != nil || len(proposals) != 1 {
		t.Fatalf("proposals = %s %v", rec.Body.String(), err)
	}
	pid, _ := proposals[0]["proposal_id"].(string)
	if !strings.HasPrefix(pid, "OP_PROP_") {
		t.Fatalf("proposal id = %q", pid)
	}
	// Approve grants, and repeats return the grant without change.
	rec = postRule(t, mux, "/api/rules/approve", `{"proposal_id":"`+pid+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	var granted struct {
		Rule    map[string]any `json:"rule"`
		Applied bool           `json:"applied"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &granted); err != nil || !granted.Applied {
		t.Fatalf("approve body = %s %v", rec.Body.String(), err)
	}
	rid, _ := granted.Rule["rule_id"].(string)
	if !strings.HasPrefix(rid, "OP_RULE_") {
		t.Fatalf("rule id = %q", rid)
	}
	if _, ok := granted.Rule["expires_at"]; !ok {
		t.Fatalf("default ttl must set expiry: %v", granted.Rule)
	}
	rec = postRule(t, mux, "/api/rules/approve", `{"proposal_id":"`+pid+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat approve = %d", rec.Code)
	}
	var replay struct {
		Rule    map[string]any `json:"rule"`
		Applied bool           `json:"applied"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &replay); err != nil || replay.Applied || replay.Rule["rule_id"] != rid {
		t.Fatalf("repeat approve body = %s %v", rec.Body.String(), err)
	}
	// Active grants leave the proposal list...
	rec = serveMux(mux, "GET", "/api/rules/proposals", "127.0.0.1:8081", nil, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &proposals); err != nil || len(proposals) != 0 {
		t.Fatalf("active grant must not list: %s", rec.Body.String())
	}
	// ...until revoked, when the evidence re-proposes.
	rec = postRule(t, mux, "/api/rules/revoke", `{"rule_id":"`+rid+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body.String())
	}
	rec = postRule(t, mux, "/api/rules/revoke", `{"rule_id":"`+rid+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat revoke = %d", rec.Code)
	}
	rec = serveMux(mux, "GET", "/api/rules/proposals", "127.0.0.1:8081", nil, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &proposals); err != nil || len(proposals) != 1 || proposals[0]["proposal_id"] != pid {
		t.Fatalf("revoked scope must re-propose: %s", rec.Body.String())
	}
	// Dismissing declines the proposal; approving it then conflicts.
	rec = postRule(t, mux, "/api/rules/dismiss", `{"proposal_id":"`+pid+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("dismiss = %d %s", rec.Code, rec.Body.String())
	}
	rec = postRule(t, mux, "/api/rules/dismiss", `{"proposal_id":"`+pid+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat dismiss = %d", rec.Code)
	}
	rec = postRule(t, mux, "/api/rules/approve", `{"proposal_id":"`+pid+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve of declined scope = %d", rec.Code)
	}
	rec = serveMux(mux, "GET", "/api/rules/proposals", "127.0.0.1:8081", nil, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &proposals); err != nil || len(proposals) != 0 {
		t.Fatalf("declined scope must not list: %s", rec.Body.String())
	}
}

func TestRuleRoutesGuards(t *testing.T) {
	s := openViewerStore(t)
	_, mux := withHost(t, s, "127.0.0.1:8081")
	cases := []struct {
		target string
		body   string
		code   int
	}{
		{"/api/rules/approve", `{"proposal_id":"OP_PROP_deadbeefdeadbeef"}`, http.StatusNotFound},
		{"/api/rules/revoke", `{"rule_id":"OP_RULE_deadbeefdeadbeef"}`, http.StatusNotFound},
		{"/api/rules/dismiss", `{"proposal_id":"OP_PROP_deadbeefdeadbeef"}`, http.StatusNotFound},
		{"/api/rules/approve", `{"proposal_id":"bogus"}`, http.StatusBadRequest},
		{"/api/rules/revoke", `{"rule_id":"bogus"}`, http.StatusBadRequest},
		{"/api/rules/approve", `{"proposal_id":"OP_RULE_deadbeefdeadbeef"}`, http.StatusBadRequest},
		{"/api/rules/revoke", `{"proposal_id":"OP_PROP_deadbeefdeadbeef"}`, http.StatusBadRequest},
		{"/api/rules/approve", `{}`, http.StatusBadRequest},
		{"/api/rules/dismiss", `{}`, http.StatusBadRequest},
		{"/api/rules/revoke", `{}`, http.StatusBadRequest},
		{"/api/rules/approve", `not json`, http.StatusBadRequest},
		{"/api/rules/approve", `{"proposal_id":"OP_PROP_deadbeefdeadbeef","ttl":366}`, http.StatusBadRequest},
		{"/api/rules/approve", `{"proposal_id":"OP_PROP_deadbeefdeadbeef","ttl":0}`, http.StatusBadRequest},
		{"/api/rules/approve", `{"proposal_id":"OP_PROP_deadbeefdeadbeef","ttl":1.5}`, http.StatusBadRequest},
		{"/api/rules/approve", `{"proposal_id":"OP_PROP_deadbeefdeadbeef","ttl":"90"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if rec := postRule(t, mux, tc.target, tc.body); rec.Code != tc.code {
			t.Fatalf("%s %s = %d, want %d (%s)", tc.target, tc.body, rec.Code, tc.code, rec.Body.String())
		}
	}
}
