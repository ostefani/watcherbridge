package server

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Producer-boundary negatives (Step 30 §2). Tap harness throughout: no
// TCP listener, so sandboxed runners can execute these. Envelope
// rejections mirror the socket-harness contract table without depending
// on a loopback bind; content cases prove hostile or inconsistent
// producer views are preserved byte-for-byte, never repaired, merged,
// or rejected for their view content.

func TestProducerEnvelopeRejectsTap(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now()

	// 413: oversize. The size gate runs before signature verification,
	// so the signature value is irrelevant here.
	big := bytes.Repeat([]byte("x"), int(h.srv.cfg.MaxBodyBytes)+1)
	if rec := h.post(t, big, uniqueID(), "host-a", "host.appeared", now, "sha256=irrelevant"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: got %d %s", rec.Code, rec.Body.String())
	}

	// 400: unknown event type with a valid signature over the same bytes.
	weird := []byte(`{"event_id":"evt-x","event_key":"host-a","event_type":"host.mutated"}`)
	wts := strconv.FormatInt(now.Unix(), 10)
	if rec := h.post(t, weird, "evt-x", "host-a", "host.mutated", now, sign(h.secret, wts, "evt-x", "host-a", "host.mutated", weird)); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown event type: got %d %s", rec.Code, rec.Body.String())
	}

	// 400: non-JSON body with a valid signature.
	raw := []byte("not json at all{{{")
	rts := strconv.FormatInt(now.Unix(), 10)
	if rec := h.post(t, raw, "evt-raw", "host-a", "host.appeared", now, sign(h.secret, rts, "evt-raw", "host-a", "host.appeared", raw)); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: got %d %s", rec.Code, rec.Body.String())
	}
}

// Inconsistent previous/current views must not be repaired or merged:
// the bridge stores the producer's bytes exactly and groups a changed
// event by its current view. Both upstream references must survive in
// the retained payload.
func TestProducerInconsistentPrevCurrentPreserved(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now()
	id := uniqueID()
	body := []byte(`{"event_id":` + strconv.Quote(id) + `,"event_key":"change:mac:102030405061:9f2c:w13",` +
		`"event_type":"host.changed","occurred_at":"2026-10-20T12:05:05Z","cycle":13,"target":"192.168.0.0/24",` +
		`"previous":{"alive":true,"ip":"192.168.0.11","mac":"10:20:30:40:50:61","source":"arp","weak":false,` +
		`"host_id":"mac:10:20:30:40:50:61","identity_source":"mac","identity_confidence":"high"},` +
		`"current":{"alive":true,"ip":"192.168.0.12","mac":"10:20:30:40:50:61","source":"arp","weak":false,` +
		`"host_id":"ip:192.168.0.12","identity_source":"ip","identity_confidence":"low"}}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	key := "change:mac:102030405061:9f2c:w13"
	rec := h.post(t, body, id, key, "host.changed", now, sign(h.secret, ts, id, key, "host.changed", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("inconsistent prev/current: got %d %s", rec.Code, rec.Body.String())
	}
	stored, err := h.srv.events.StoredEvent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.Payload, body) {
		t.Fatal("stored payload differs from submitted body: bridge rewrote producer content")
	}
	for _, want := range []string{`"host_id":"mac:10:20:30:40:50:61"`, `"host_id":"ip:192.168.0.12"`} {
		if !strings.Contains(string(stored.Payload), want) {
			t.Fatalf("stored payload lost view %s", want)
		}
	}
}

// Unknown present and future fields ride along untouched: the bridge
// decodes only its envelope subset and must not drop or normalize the
// rest, so a newer producer never loses data through an older bridge.
func TestProducerUnknownFieldsPreserved(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now()
	id := uniqueID()
	body := []byte(`{"event_id":` + strconv.Quote(id) + `,"event_key":"appear:mac:102030405099:w12",` +
		`"event_type":"host.appeared","occurred_at":"2026-10-20T12:00:05Z","cycle":12,"target":"192.168.0.0/24",` +
		`"future_top_level":{"nested":[1,2,3]},"host":{"alive":true,"ip":"192.168.0.99","weak":false,` +
		`"future_view_field":"keep-me","host_id":"mac:10:20:30:40:50:99","identity_source":"mac",` +
		`"identity_confidence":"high"}}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	key := "appear:mac:102030405099:w12"
	rec := h.post(t, body, id, key, "host.appeared", now, sign(h.secret, ts, id, key, "host.appeared", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unknown fields: got %d %s", rec.Code, rec.Body.String())
	}
	stored, err := h.srv.events.StoredEvent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.Payload, body) {
		t.Fatal("stored payload differs from submitted body: bridge dropped unknown fields")
	}
}

// Failure responses carry no request content (Step 30 §4.8): status
// bodies are static strings, so payload markers, addresses and header
// secrets never echo into diagnostics, whether the failure is a 400
// reject, a 413 oversize, or a 503 closed store.
func TestFailureResponsesCarryNoRequestContent(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now()
	marker := "MARKER-9f2c41aa-secret-value"

	// 400 malformed body containing the marker.
	raw := []byte("not json {{{ " + marker)
	rts := strconv.FormatInt(now.Unix(), 10)
	rec := h.post(t, raw, "evt-raw", "host-a", "host.appeared", now, sign(h.secret, rts, "evt-raw", "host-a", "host.appeared", raw))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed: got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Fatal("400 body echoes request content")
	}

	// 413 oversize body containing the marker.
	big := bytes.Repeat([]byte("x"), int(h.srv.cfg.MaxBodyBytes)+1)
	copy(big[100:], marker)
	if rec := h.post(t, big, uniqueID(), "host-a", "host.appeared", now, "sha256=irrelevant"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: got %d", rec.Code)
	} else if strings.Contains(rec.Body.String(), marker) {
		t.Fatal("413 body echoes request content")
	}

	// 503 closed store with a well-formed marked body.
	id := uniqueID()
	body := []byte(`{"event_id":` + strconv.Quote(id) + `,"event_key":"k","event_type":"host.appeared",` +
		`"occurred_at":"2026-10-20T12:00:05Z","cycle":1,"target":"192.168.0.0/24","note":` + strconv.Quote(marker) + `}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	_ = h.srv.events.Close()
	rec = h.post(t, body, id, "k", "host.appeared", now, sign(h.secret, ts, id, "k", "host.appeared", body))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed store: got %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Fatal("503 body echoes request content")
	}
}

// A signed envelope whose body echoes different event_id, event_key or
// event_type than the signed headers is rejected before persistence:
// the HMAC covers raw bytes, not parsed semantics, so a valid signature
// alone cannot reconcile two envelope identities (Designv9 §1.1
// metadata consistency; the heartbeat path already enforces the same
// comparison). Nothing is stored or grouped from a split envelope.
func TestProducerEnvelopeMismatchRejected(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now()
	base := func(id, key, typ string) []byte {
		return []byte(`{"event_id":` + strconv.Quote(id) + `,"event_key":` + strconv.Quote(key) +
			`,"event_type":` + strconv.Quote(typ) + `,"occurred_at":"2026-10-20T12:00:05Z","cycle":1,` +
			`"target":"192.168.0.0/24","host":{"alive":true,"ip":"192.168.0.99","weak":false}}`)
	}
	for _, tc := range []struct {
		name    string
		bodyID  string
		bodyKey string
		bodyTyp string
	}{
		{"event id", "evt-body-differs", "host-a", "host.appeared"},
		{"event key", "evt-mismatch-1", "key-differs", "host.appeared"},
		{"event type", "evt-mismatch-2", "host-a", "host.changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uniqueID()
			body := base(tc.bodyID, tc.bodyKey, tc.bodyTyp)
			ts := strconv.FormatInt(now.Unix(), 10)
			// Valid signature over the header identity plus these exact
			// body bytes: authentication passes, consistency must fail.
			rec := h.post(t, body, id, "host-a", "host.appeared", now,
				sign(h.secret, ts, id, "host-a", "host.appeared", body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("split envelope: got %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if _, err := h.srv.events.StoredEvent(context.Background(), id); err == nil {
				t.Fatal("split envelope was persisted")
			}
		})
	}
}

// A changed event carrying no previous/current views is still a valid
// producer report: grouping falls back to the event itself and the
// payload is preserved, not rejected for missing views.
func TestProducerMissingViewsAcceptedEventLocal(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now()
	id := uniqueID()
	body := []byte(`{"event_id":` + strconv.Quote(id) + `,"event_key":"change:bare:w13",` +
		`"event_type":"host.changed","occurred_at":"2026-10-20T12:05:05Z","cycle":13,"target":"192.168.0.0/24"}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	rec := h.post(t, body, id, "change:bare:w13", "host.changed", now, sign(h.secret, ts, id, "change:bare:w13", "host.changed", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("viewless changed: got %d %s", rec.Code, rec.Body.String())
	}
	stored, err := h.srv.events.StoredEvent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.Payload, body) {
		t.Fatal("stored payload differs from submitted body")
	}
}
