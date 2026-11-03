package server

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// Step 30.1 source authentication and scope gate. All cases run
// through the tap harness: no TCP listener, so sandboxed runners
// execute them. Socket-level transport claims stay with the
// listener-based suite and are recorded as environment-restricted,
// never as passing here.

// The skew window is inclusive at the edge: exactly tolerance is
// admitted, one second past it is a 401. The handler-level cases sit
// one second inside/outside because the handler clock races the test
// clock; the exact edge is pinned by the deterministic unit cases.
func TestAuthTimestampBoundary(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now()
	tol := h.srv.cfg.TimestampTolerance

	inside := now.Add(-tol + time.Second)
	id := uniqueID()
	body := eventBody(id, "host-a", "host.appeared")
	if rec := h.post(t, body, id, "host-a", "host.appeared", inside,
		sign(h.secret, strconv.FormatInt(inside.Unix(), 10), id, "host-a", "host.appeared", body)); rec.Code != http.StatusAccepted {
		t.Fatalf("inside window: got %d %s", rec.Code, rec.Body.String())
	}
	outside := now.Add(-tol - time.Second)
	oid := uniqueID()
	obody := eventBody(oid, "host-a", "host.appeared")
	if rec := h.post(t, obody, oid, "host-a", "host.appeared", outside,
		sign(h.secret, strconv.FormatInt(outside.Unix(), 10), oid, "host-a", "host.appeared", obody)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("outside window: got %d, want 401", rec.Code)
	}

	fixed := time.Unix(1_700_000_000, 0).UTC()
	edge := strconv.FormatInt(fixed.Add(-tol).Unix(), 10)
	if _, err := parseAndValidate(fixed, tol, edge, "evt-edge", "host-a", "host.appeared", []byte(`{}`)); err != nil {
		t.Fatalf("exact tolerance edge must admit: %v", err)
	}
	past := strconv.FormatInt(fixed.Add(-tol-time.Second).Unix(), 10)
	if _, err := parseAndValidate(fixed, tol, past, "evt-past", "host-a", "host.appeared", []byte(`{}`)); err == nil {
		t.Fatal("one second past tolerance must reject")
	}
	future := strconv.FormatInt(fixed.Add(tol).Unix(), 10)
	if _, err := parseAndValidate(fixed, tol, future, "evt-future", "host-a", "host.appeared", []byte(`{}`)); err != nil {
		t.Fatalf("exact future edge must admit: %v", err)
	}
}

// Missing, malformed, and mismatched signatures are all 401: no
// signature shape bypasses admission, and the HMAC binds the exact
// transmitted bytes, not a prefix of them.
func TestAuthMissingAndMalformedSignature(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	id := uniqueID()
	body := eventBody(id, "host-a", "host.appeared")
	good := sign(h.secret, ts, id, "host-a", "host.appeared", body)

	cases := map[string]string{
		"missing":      "",
		"no prefix":    good[len("sha256="):],
		"wrong prefix": "md5=" + good[len("sha256="):],
		"bad hex":      "sha256=zzzz",
		"truncated":    good[:len(good)-4],
		"other secret": sign("another-secret-0123456789012345", ts, id, "host-a", "host.appeared", body),
	}
	for name, sig := range cases {
		if rec := h.post(t, body, id, "host-a", "host.appeared", now, sig); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s signature: got %d, want 401", name, rec.Code)
		}
	}

	// Signed headers over a different body: the signature binds the
	// exact bytes on the wire.
	other := eventBody(uniqueID(), "host-a", "host.appeared")
	if rec := h.post(t, other, id, "host-a", "host.appeared", now, good); rec.Code != http.StatusUnauthorized {
		t.Fatalf("swapped body: got %d, want 401", rec.Code)
	}
	// Signed body under a different event ID header.
	if rec := h.post(t, body, "evt-smuggled", "host-a", "host.appeared", now, good); rec.Code != http.StatusUnauthorized {
		t.Fatalf("swapped event id: got %d, want 401", rec.Code)
	}
}

// Payload scope smuggling: source_scope/source_id/scope fields in the
// body never become the stored source scope, which comes only from
// receiver configuration. Target stays a target (legitimate,
// address-scoped display), never a scope.
func TestScopeSmugglingRejected(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	id := uniqueID()
	body := []byte(`{"event_id":` + strconv.Quote(id) + `,"event_key":"host-a","event_type":"host.appeared",` +
		`"occurred_at":"2026-10-20T12:00:05Z","cycle":7,"target":"evil-target",` +
		`"host":{"host_id":"mac:aa:bb:cc:dd:ee:ff","identity_source":"mac","identity_confidence":"high"},` +
		`"source_scope":"evil-scope","source_id":"evil-id","scope":"evil"}`)
	if rec := h.post(t, body, id, "host-a", "host.appeared", now,
		sign(h.secret, ts, id, "host-a", "host.appeared", body)); rec.Code != http.StatusAccepted {
		t.Fatalf("smuggling body: got %d %s", rec.Code, rec.Body.String())
	}
	stored, err := h.srv.events.StoredEvent(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SourceScope != h.srv.cfg.SourceID {
		t.Fatalf("stored scope = %q, want receiver-configured %q", stored.SourceScope, h.srv.cfg.SourceID)
	}
	if stored.Target != "evil-target" {
		t.Fatalf("target must stay a target, got %q", stored.Target)
	}
}
