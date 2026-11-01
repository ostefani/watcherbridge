package viewer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

// seedSubjects persists and projects three reported devices on one
// scope and returns their profile IDs oldest-first.
func seedSubjects(t *testing.T, s *store.Store) []string {
	t.Helper()
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	var ids []string
	for i, mac := range []string{"10:20:30:40:50:71", "10:20:30:40:50:72", "10:20:30:40:50:73"} {
		body := fmt.Sprintf(`{"event_id":"evt-d%d","event_key":"k","event_type":"host.appeared","occurred_at":%q,"cycle":3,
			"host":{"mac":%q,"host_id":"mac:%s","identity_source":"mac","identity_confidence":"high"}}`,
			i, base.Add(time.Duration(i)*time.Hour).Format(time.RFC3339), mac, mac)
		anchor, eventLocal := store.GroupKeyForEvent("lab", "host.appeared", fmt.Sprintf("evt-d%d", i), []byte(body))
		if eventLocal {
			t.Fatal("qualified payload must scope")
		}
		if _, err := s.PersistEventAndGroup(context.Background(), store.RawEvent{
			EventID: fmt.Sprintf("evt-d%d", i), EventKey: "k", EventType: "host.appeared",
			OccurredAt: base, Payload: []byte(body), ReceivedAt: base.Add(time.Duration(i) * time.Hour),
			SignatureTimestamp: base, SourceScope: "lab",
		}, anchor, store.GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ProjectReported(context.Background(), "lab", 64); err != nil {
		t.Fatal(err)
	}
	subs, _, err := s.ReportedSubjectsPage(context.Background(), 0, "", 10)
	if err != nil || len(subs) != 3 {
		t.Fatalf("seeded subjects = %d %v", len(subs), err)
	}
	for _, sub := range subs {
		ids = append(ids, sub.ProfileID)
	}
	return ids
}

func decodeDevices(t *testing.T, rec *httptest.ResponseRecorder) devicesEnvelope {
	t.Helper()
	var env devicesEnvelope
	if err := json.Unmarshal([]byte(rec.Body.String()), &env); err != nil {
		t.Fatalf("devices envelope = %q: %v", rec.Body.String(), err)
	}
	return env
}

func TestDevicesListAndCursor(t *testing.T) {
	s := openViewerStore(t)
	seedSubjects(t, s)
	_, mux := withHost(t, s, "127.0.0.1:8081")
	rec := serveMux(mux, "GET", "/api/devices?limit=2", "127.0.0.1:8081", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("devices = %d", rec.Code)
	}
	assertViewerHeaders(t, rec.Header())
	env := decodeDevices(t, rec)
	if len(env.Devices) != 2 || env.NextAfter == "" {
		t.Fatalf("first page = %+v", env)
	}
	d := env.Devices[0]
	if d.Class != "reported_device" || d.SourceScope != "lab" || d.ReportedID == "" || d.AssociationStatus == "" {
		t.Fatalf("class/provenance/association must be explicit: %+v", d)
	}
	if d.Associations["groups"] != 1 || d.Associations["briefings"] != 0 {
		t.Fatalf("unbriefed subject associations = %v, want groups 1 briefings 0", d.Associations)
	}
	rec = serveMux(mux, "GET", "/api/devices?limit=2&after="+env.NextAfter, "127.0.0.1:8081", nil, "")
	env = decodeDevices(t, rec)
	if len(env.Devices) != 1 || env.NextAfter != "" {
		t.Fatalf("second page = %+v", env)
	}
}

func TestDevicesRejects(t *testing.T) {
	s := openViewerStore(t)
	seedSubjects(t, s)
	_, mux := withHost(t, s, "127.0.0.1:8081")
	for _, target := range []string{
		"/api/devices?limit=0",
		"/api/devices?limit=201",
		"/api/devices?limit=abc",
		"/api/devices?after=nope",
		"/api/devices?after=-5:x",
		"/api/devices?after=5",
	} {
		if rec := serveMux(mux, "GET", target, "127.0.0.1:8081", nil, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", target, rec.Code)
		}
	}
}

func TestDeviceDetailAndHistory(t *testing.T) {
	s := openViewerStore(t)
	ids := seedSubjects(t, s)
	_, mux := withHost(t, s, "127.0.0.1:8081")
	rec := serveMux(mux, "GET", "/api/devices/"+ids[0], "127.0.0.1:8081", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("device = %d", rec.Code)
	}
	assertViewerHeaders(t, rec.Header())
	var got deviceWire
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ProfileID != ids[0] || got.Class != "reported_device" || got.Latest == nil {
		t.Fatalf("detail = %+v", got)
	}
	if len(got.History) != 1 || got.HistoryNextAfter != "" {
		t.Fatalf("single-observation history = %+v", got.History)
	}
	if got.History[0].Attributes["mac"] != "10:20:30:40:50:71" {
		t.Fatalf("history must carry the snapshot: %v", got.History[0].Attributes)
	}
	// Unknown but well-formed IDs are 404; anything else is 400 and
	// never reaches the store.
	if rec := serveMux(mux, "GET", "/api/devices/prof-0123456789abcdef", "127.0.0.1:8081", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown profile = %d, want 404", rec.Code)
	}
	for _, target := range []string{"/api/devices/nope", "/api/devices/prof-XYZ", "/api/devices/" + ids[0] + "?history_limit=51", "/api/devices/" + ids[0] + "?history_after=nope"} {
		if rec := serveMux(mux, "GET", target, "127.0.0.1:8081", nil, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", target, rec.Code)
		}
	}
}

func TestDeviceHistoryPaging(t *testing.T) {
	s := openViewerStore(t)
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	macJSON := func(evt string, at time.Time) string {
		return fmt.Sprintf(`{"event_id":%q,"event_key":"k","event_type":"host.appeared","occurred_at":%q,"cycle":3,
			"host":{"mac":"10:20:30:40:50:74","host_id":"mac:10:20:30:40:50:74","identity_source":"mac","identity_confidence":"high"}}`, evt, at.Format(time.RFC3339))
	}
	for i, evt := range []string{"evt-m1", "evt-m2"} {
		body := macJSON(evt, base.Add(time.Duration(i)*time.Hour))
		anchor, _ := store.GroupKeyForEvent("lab", "host.appeared", evt, []byte(body))
		if _, err := s.PersistEventAndGroup(context.Background(), store.RawEvent{
			EventID: evt, EventKey: "k", EventType: "host.appeared",
			OccurredAt: base, Payload: []byte(body), ReceivedAt: base.Add(time.Duration(i) * time.Hour),
			SignatureTimestamp: base, SourceScope: "lab",
		}, anchor, store.GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ProjectReported(context.Background(), "lab", 64); err != nil {
		t.Fatal(err)
	}
	subs, _, err := s.ReportedSubjectsPage(context.Background(), 0, "", 10)
	if err != nil || len(subs) != 1 {
		t.Fatalf("subjects = %v %v", subs, err)
	}
	_, mux := withHost(t, s, "127.0.0.1:8081")
	rec := serveMux(mux, "GET", "/api/devices/"+subs[0].ProfileID+"?history_limit=1", "127.0.0.1:8081", nil, "")
	var got deviceWire
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 1 || got.History[0].EventID != "evt-m1" || got.HistoryNextAfter == "" {
		t.Fatalf("history page 1 = %+v", got.History)
	}
	rec = serveMux(mux, "GET", "/api/devices/"+subs[0].ProfileID+"?history_limit=1&history_after="+got.HistoryNextAfter, "127.0.0.1:8081", nil, "")
	got = deviceWire{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 1 || got.History[0].EventID != "evt-m2" {
		t.Fatalf("history page 2 = %+v", got.History)
	}
	if !strings.Contains(got.History[0].ViewRole, "host") {
		t.Fatalf("history must label the view role: %+v", got.History[0])
	}
}
