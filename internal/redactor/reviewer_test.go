package redactor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReviewerDemoBundlePassesLeakScan(t *testing.T) {
	const id = "review-demo-1793829524411817000"
	payload := []byte(`{"event_id":"` + id + `","event_key":"review-demo","event_type":"host.appeared","occurred_at":"2026-11-04T21:58:44Z","target":"192.0.2.0/24","host":{"ip":"192.0.2.10","mac":"02:00:00:00:00:10","hostname":"review-demo","host_id":"mock:` + id + `","identity_source":"mock","identity_confidence":"high","alive":true}}`)
	bundle, issued, _, err := BuildBundle(t.Context(), testStore(t), BundleInput{
		GroupID: "grp-demo", Anchor: "evt:" + id, Baseline: "medium",
		Events: []InputEvent{{EventID: id, EventType: "host.appeared", Payload: payload}},
	}, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if hits, err := ScanOutboundLeaks(raw, issued, bundle.EventIDs); err != nil || len(hits) > 0 {
		t.Fatalf("synthetic demo blocked at %v: %v", hits, err)
	}
	var events []map[string]any
	if err := json.Unmarshal(bundle.EventsJSON, &events); err != nil || len(events) != 1 {
		t.Fatalf("decode provider events: %v", err)
	}
	event := events[0]
	if event["event_id"] != id {
		t.Fatal("event reference must remain unchanged")
	}
	host := event["host"].(map[string]any)
	for _, value := range []string{bundle.Anchor, host["host_id"].(string)} {
		if strings.Contains(value, "review-demo") {
			t.Fatal("opaque identifiers must not expose embedded hostnames")
		}
	}
	if got := Restore(host["host_id"].(string), issued); got != "mock:"+id {
		t.Fatal("opaque host reference must restore exactly")
	}
	if got := Restore(bundle.Anchor, issued); got != "evt:"+id {
		t.Fatal("event-local anchor must restore exactly")
	}
}

func TestOpaqueHostReferenceRejectsTokenSpoof(t *testing.T) {
	_, _, _, err := BuildBundle(t.Context(), testStore(t), BundleInput{
		GroupID: "grp-demo", Anchor: "evt:demo-event",
		Events: []InputEvent{{EventID: "demo-event", EventType: "host.appeared",
			Payload: []byte(`{"host":{"host_id":"HOST_123"}}`)}},
	}, testPolicy())
	if err == nil || !strings.Contains(err.Error(), "token-shaped") {
		t.Fatal("opaque-reference redaction must still reject token spoofing")
	}
}
