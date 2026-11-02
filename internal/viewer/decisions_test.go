package viewer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Decision endpoints (Step 25): append with matrix validation and
// idempotent request IDs, explicit undo with ownership and conflict
// semantics, and the Step 16 mutation guard on every POST.
// Unknown-but-well-formed targets are 404; garbage is 400 and never
// reaches the store; matrix violations are 422; lost races are 409.
func postDecision(t *testing.T, mux http.Handler, target, body, origin string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{"Content-Type": "application/json"}
	if origin != "" {
		headers["Origin"] = origin
	}
	return serveMux(mux, "POST", target, "127.0.0.1:8081", headers, body)
}

func TestDecisionAppendAndIdempotency(t *testing.T) {
	s := openViewerStore(t)
	gid := seedPriorityBriefing(t, s, "host:dec-e", "evt-dec-e", "high", time.Now().UTC())
	_, mux := withHost(t, s, "127.0.0.1:8081")
	origin := "http://127.0.0.1:8081"

	body := `{"subject_kind":"briefing","subject_id":"` + gid + `","kind":"expected","request_id":"req-1"}`
	rec := postDecision(t, mux, "/api/decisions", body, origin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("append = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var first mutationWire
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if !first.Applied || first.Decision.DecisionID == "" {
		t.Fatalf("append = %+v, want applied with ID", first)
	}
	// Identical replay returns the original row without duplicating.
	rec = postDecision(t, mux, "/api/decisions", body, origin)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay = %d, want 200", rec.Code)
	}
	var replay mutationWire
	if err := json.Unmarshal(rec.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Applied || replay.Reason != "duplicate_request" || replay.Decision.DecisionID != first.Decision.DecisionID {
		t.Fatalf("replay = %+v, want idempotent no-op", replay)
	}
	// Same key with different content conflicts.
	rec = postDecision(t, mux, "/api/decisions", `{"subject_kind":"briefing","subject_id":"`+gid+`","kind":"note","text":"other","request_id":"req-1"}`, origin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflicting reuse = %d, want 409", rec.Code)
	}
	// Matrix violations are explicit, never silent.
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{"label on briefing", `{"subject_kind":"briefing","subject_id":"` + gid + `","kind":"label","text":"x"}`, http.StatusUnprocessableEntity},
		{"expected on missing profile", `{"subject_kind":"reported_subject","subject_id":"prof-0000000000000000","kind":"expected"}`, http.StatusUnprocessableEntity},
		{"missing subject", `{"subject_kind":"briefing","subject_id":"grp-nope","kind":"expected"}`, http.StatusNotFound},
		{"garbage subject", `{"subject_kind":"briefing","subject_id":"nope","kind":"expected"}`, http.StatusBadRequest},
		{"bad until", `{"subject_kind":"briefing","subject_id":"` + gid + `","kind":"snooze","until":"tomorrow"}`, http.StatusBadRequest},
		{"bad json", `{`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := postDecision(t, mux, "/api/decisions", tc.body, origin); rec.Code != tc.code {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.code, rec.Body.String())
			}
		})
	}
	// A snooze without until inherits the 24h test default horizon.
	rec = postDecision(t, mux, "/api/decisions", `{"subject_kind":"briefing","subject_id":"`+gid+`","kind":"snooze"}`, origin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("default snooze = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var snoozed mutationWire
	if err := json.Unmarshal(rec.Body.Bytes(), &snoozed); err != nil {
		t.Fatal(err)
	}
	until, err := time.Parse(time.RFC3339, snoozed.Decision.Until)
	if err != nil || time.Until(until) < 23*time.Hour || time.Until(until) > 25*time.Hour {
		t.Fatalf("default snooze until = %q, want ~24h out", snoozed.Decision.Until)
	}
	// Mutation guard: missing Origin, wrong content type, wrong method.
	if rec := serveMux(mux, "POST", "/api/decisions", "127.0.0.1:8081",
		map[string]string{"Content-Type": "application/json"}, body); rec.Code != http.StatusForbidden {
		t.Fatalf("missing origin = %d, want 403", rec.Code)
	}
	if rec := postDecision(t, mux, "/api/decisions", body, "http://evil.example"); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin = %d, want 403", rec.Code)
	}
	if rec := serveMux(mux, "POST", "/api/decisions", "127.0.0.1:8081",
		map[string]string{"Content-Type": "text/plain", "Origin": origin}, body); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong content type = %d, want 415", rec.Code)
	}
	// GET history requires both subject selectors.
	if rec := serveMux(mux, "GET", "/api/decisions?subject_kind=briefing", "127.0.0.1:8081", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bare list = %d, want 400", rec.Code)
	}
}

func TestDecisionUndoFlow(t *testing.T) {
	s := openViewerStore(t)
	gid := seedPriorityBriefing(t, s, "host:dec-u", "evt-dec-u", "medium", time.Now().UTC())
	_, mux := withHost(t, s, "127.0.0.1:8081")
	origin := "http://127.0.0.1:8081"

	var created mutationWire
	rec := postDecision(t, mux, "/api/decisions",
		`{"subject_kind":"briefing","subject_id":"`+gid+`","kind":"not_expected"}`, origin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("append = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	undo := func(id, body string) *httptest.ResponseRecorder {
		t.Helper()
		return postDecision(t, mux, "/api/decisions/"+id+"/undo", body, origin)
	}
	rec = undo(created.Decision.DecisionID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("undo = %d: %s", rec.Code, rec.Body.String())
	}
	var done mutationWire
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil {
		t.Fatal(err)
	}
	if !done.Applied || done.Decision.Kind != "undo" {
		t.Fatalf("undo = %+v, want applied undo", done)
	}
	// Repeating the undo is a satisfied no-op, not an error.
	rec = undo(created.Decision.DecisionID, "")
	var again mutationWire
	if err := json.Unmarshal(rec.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if again.Applied || again.Reason != "already_undone" {
		t.Fatalf("second undo = %+v, want applied:false", again)
	}
	// Undoing the undo row itself is invalid; unknown IDs are 404.
	if rec := undo(done.Decision.DecisionID, ""); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("undo-of-undo = %d, want 422", rec.Code)
	}
	if rec := undo("dec-0", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown undo = %d, want 404", rec.Code)
	}
	if rec := undo("bogus", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed undo = %d, want 400", rec.Code)
	}
	// History lists the trail with undone markers, newest first.
	rec = serveMux(mux, "GET", "/api/decisions?subject_kind=briefing&subject_id="+gid, "127.0.0.1:8081", nil, "")
	var trail []decisionWire
	if err := json.Unmarshal(rec.Body.Bytes(), &trail); err != nil || len(trail) != 2 {
		t.Fatalf("trail = %q, %v", rec.Body.String(), err)
	}
	if trail[0].Kind != "undo" || trail[1].Kind != "not_expected" {
		t.Fatalf("trail order = %+v, want undo newest", trail)
	}
	if trail[1].Undone != true || trail[0].Undone != false {
		t.Fatalf("undone markers = %+v", trail)
	}
}

// Subject views carry the decision trail and live review state so the
// dossier renders pills, lists and undo without a second fetch.
func TestDecisionSubjectViewsCarryTrail(t *testing.T) {
	s := openViewerStore(t)
	gid := seedPriorityBriefing(t, s, "host:dec-v", "evt-dec-v", "medium", time.Now().UTC())
	ids := seedSubjects(t, s)
	_, mux := withHost(t, s, "127.0.0.1:8081")
	origin := "http://127.0.0.1:8081"

	postDecision(t, mux, "/api/decisions",
		`{"subject_kind":"briefing","subject_id":"`+gid+`","kind":"expected"}`, origin)
	postDecision(t, mux, "/api/decisions",
		`{"subject_kind":"reported_subject","subject_id":"`+ids[0]+`","kind":"label","text":"iot"}`, origin)

	rec := serveMux(mux, "GET", "/api/briefing?id="+gid, "127.0.0.1:8081", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("briefing = %d: %s", rec.Code, rec.Body.String())
	}
	var bview briefingView
	if err := json.Unmarshal(rec.Body.Bytes(), &bview); err != nil {
		t.Fatal(err)
	}
	if len(bview.Decisions) != 1 || bview.Decisions[0].Kind != "expected" {
		t.Fatalf("briefing trail = %+v, want one expected", bview.Decisions)
	}
	if bview.EffectiveReview == nil || bview.EffectiveReview.Kind != "expected" {
		t.Fatalf("briefing effective = %+v, want expected", bview.EffectiveReview)
	}

	rec = serveMux(mux, "GET", "/api/devices/"+ids[0], "127.0.0.1:8081", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("device = %d: %s", rec.Code, rec.Body.String())
	}
	var dview deviceWire
	if err := json.Unmarshal(rec.Body.Bytes(), &dview); err != nil {
		t.Fatal(err)
	}
	if len(dview.Decisions) != 1 || dview.Decisions[0].Kind != "label" {
		t.Fatalf("device trail = %+v, want one label", dview.Decisions)
	}
}

func TestDecisionLabelUndoConflict(t *testing.T) {
	s := openViewerStore(t)
	ids := seedSubjects(t, s)
	_, mux := withHost(t, s, "127.0.0.1:8081")
	origin := "http://127.0.0.1:8081"
	label := func(pid, text string) mutationWire {
		t.Helper()
		rec := postDecision(t, mux, "/api/decisions",
			`{"subject_kind":"reported_subject","subject_id":"`+pid+`","kind":"label","text":"`+text+`"}`, origin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("label = %d: %s", rec.Code, rec.Body.String())
		}
		var out mutationWire
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := label(ids[0], "A")
	second := label(ids[0], "B")
	// A stale head cannot be restored over the newer label.
	rec := postDecision(t, mux, "/api/decisions/"+first.Decision.DecisionID+"/undo", "", origin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale undo = %d, want 409", rec.Code)
	}
	// Undoing the current head restores the prior label.
	if rec := postDecision(t, mux, "/api/decisions/"+second.Decision.DecisionID+"/undo", "", origin); rec.Code != http.StatusOK {
		t.Fatalf("head undo = %d: %s", rec.Code, rec.Body.String())
	}
	got := serveMux(mux, "GET", "/api/devices/"+ids[0], "127.0.0.1:8081", nil, "")
	var dev deviceWire
	if err := json.Unmarshal(got.Body.Bytes(), &dev); err != nil {
		t.Fatal(err)
	}
	if dev.OperatorLabel == nil || *dev.OperatorLabel != "A" {
		t.Fatalf("label after undo = %v, want A", dev.OperatorLabel)
	}
	if !strings.HasPrefix(first.Decision.DecisionID, "dec-") {
		t.Fatalf("decision ID %q misses the dec- prefix", first.Decision.DecisionID)
	}
}
