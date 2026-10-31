package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/config"
	"github.com/ostefani/watcherbridge/internal/forwarder"
	"github.com/ostefani/watcherbridge/internal/store"
)

// newTapHarness builds a Server without any TCP listener: tap mode
// never forwards, so the fake relay (and its httptest listener) is
// unnecessary. Contract acceptance must not depend on a loopback
// bind, which sandboxed runners forbid.
func newTapHarness(t *testing.T) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.Mode = config.ModeTap
	cfg.Secret = "test-secret-long-enough-01234567"
	st, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"), store.WithQueueCap(cfg.WriterQueueCap))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := New(cfg, st, forwarder.NewGate())
	if err != nil {
		t.Fatal(err)
	}
	return &harness{srv: srv, secret: "test-secret-long-enough-01234567"}
}

// Producer-contract acceptance (Step 18a): every Designv9 §1.3
// reference class is accepted with its evidence preserved and stored
// exactly once — including unknown and provided-without-contract
// metadata, which must never be rejected or resolved by guessing.
// Identity fields below pin the fixtures as qualified references
// (or explicitly missing ones); grouping keys still follow
// anchorForEvent until 18c replaces them, so this test pins
// acceptance, identity fidelity and byte preservation — not anchors.
func TestProducerContractAcceptance(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now()
	files := []producerFixture{
		{
			file: "appeared-mac.json",
			views: map[string]identityView{
				"host": {HostID: "mac:10:20:30:40:50:60", IdentitySource: "mac", IdentityConfidence: "high"},
			},
		},
		{
			file: "changed-previous-current.json",
			views: map[string]identityView{
				"previous": {HostID: "mac:10:20:30:40:50:61", IdentitySource: "mac", IdentityConfidence: "high"},
				"current":  {HostID: "mac:10:20:30:40:50:61", IdentitySource: "mac", IdentityConfidence: "high"},
			},
		},
		{
			file: "appeared-ip.json",
			views: map[string]identityView{
				"host": {HostID: "ip:192.168.0.23", IdentitySource: "ip", IdentityConfidence: "low"},
			},
		},
		{
			file: "appeared-provided.json",
			views: map[string]identityView{
				"host": {HostID: "asset-42", IdentitySource: "provided", IdentityConfidence: "high"},
			},
		},
		{
			// No identity fields at all: must stay missing, never
			// filled by a hostname/MAC fallback matcher.
			file: "appeared-unknown.json",
			views: map[string]identityView{
				"host": {},
			},
		},
		{
			file: "changed-randomized.json",
			views: map[string]identityView{
				"previous": {HostID: "ip:192.168.0.31", IdentitySource: "ip", IdentityConfidence: "low"},
				"current":  {HostID: "ip:192.168.0.31", IdentitySource: "ip", IdentityConfidence: "low"},
			},
		},
	}
	var firstBody []byte
	var firstEnv producerEnvelope
	for _, fx := range files {
		body, err := os.ReadFile(filepath.Join("testdata", "producer", fx.file))
		if err != nil {
			t.Fatal(err)
		}
		var env producerBody
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("%s: %v", fx.file, err)
		}
		if env.EventID == "" || env.EventKey == "" || env.EventType == "" {
			t.Fatalf("%s: envelope identity missing", fx.file)
		}
		assertFixtureIdentity(t, fx, env)
		ts := strconv.FormatInt(now.Unix(), 10)
		rec := h.post(t, body, env.EventID, env.EventKey, env.EventType, now,
			sign(h.secret, ts, env.EventID, env.EventKey, env.EventType, body))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: got %d %s", fx.file, rec.Code, rec.Body.String())
		}
		// Byte preservation: the retained row must carry the exact
		// submitted body and envelope metadata. A group-count-only
		// assertion would pass dropped or rewritten metadata.
		stored, err := h.srv.events.StoredEvent(context.Background(), env.EventID)
		if err != nil {
			t.Fatalf("%s: stored event: %v", fx.file, err)
		}
		if !bytes.Equal(stored.Payload, body) {
			t.Fatalf("%s: stored payload differs from submitted body", fx.file)
		}
		if stored.EventKey != env.EventKey || stored.EventType != env.EventType {
			t.Fatalf("%s: stored envelope = %q/%q, want %q/%q",
				fx.file, stored.EventKey, stored.EventType, env.EventKey, env.EventType)
		}
		if firstBody == nil {
			firstBody, firstEnv = body, env.producerEnvelope
		}
	}
	if got := storedEventCount(t, h); got != len(files) {
		t.Fatalf("stored events = %d, want %d", got, len(files))
	}
	// Duplicate delivery is an idempotent no-op, not a second event.
	ts := strconv.FormatInt(now.Unix(), 10)
	rec := h.post(t, firstBody, firstEnv.EventID, firstEnv.EventKey, firstEnv.EventType, now,
		sign(h.secret, ts, firstEnv.EventID, firstEnv.EventKey, firstEnv.EventType, firstBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("duplicate: got %d %s", rec.Code, rec.Body.String())
	}
	if got := storedEventCount(t, h); got != len(files) {
		t.Fatalf("stored events after duplicate = %d, want %d", got, len(files))
	}
}

type producerFixture struct {
	file  string
	views map[string]identityView // views that must be present, with exact identity
}

type producerEnvelope struct {
	EventID   string `json:"event_id"`
	EventKey  string `json:"event_key"`
	EventType string `json:"event_type"`
}

type identityView struct {
	HostID             string `json:"host_id"`
	IdentitySource     string `json:"identity_source"`
	IdentityConfidence string `json:"identity_confidence"`
}

type producerBody struct {
	producerEnvelope
	Host     *identityView `json:"host"`
	Previous *identityView `json:"previous"`
	Current  *identityView `json:"current"`
}

// assertFixtureIdentity pins each fixture's reference class: qualified
// fixtures carry their exact upstream reference, and missing-ID cases
// assert the fields stay empty rather than standing in for qualified
// coverage.
func assertFixtureIdentity(t *testing.T, fx producerFixture, env producerBody) {
	t.Helper()
	actual := map[string]identityView{}
	if env.Host != nil {
		actual["host"] = *env.Host
	}
	if env.Previous != nil {
		actual["previous"] = *env.Previous
	}
	if env.Current != nil {
		actual["current"] = *env.Current
	}
	if len(actual) != len(fx.views) {
		t.Fatalf("%s: views present = %d, want %d", fx.file, len(actual), len(fx.views))
	}
	for name, want := range fx.views {
		got, ok := actual[name]
		if !ok {
			t.Fatalf("%s: view %q missing", fx.file, name)
		}
		if got != want {
			t.Fatalf("%s: view %q identity = %+v, want %+v", fx.file, name, got, want)
		}
	}
}

func storedEventCount(t *testing.T, h *harness) int {
	t.Helper()
	snaps, err := h.srv.events.OpenGroupSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, s := range snaps {
		total += s.EventCount
	}
	return total
}
