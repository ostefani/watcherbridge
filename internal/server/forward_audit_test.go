package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/config"
	"github.com/ostefani/watcherbridge/internal/forwarder"
	"github.com/ostefani/watcherbridge/internal/store"
)

// stubRelayTripper captures the exact outbound forward without a
// listener: the proxy client needs only a RoundTripper, never a port,
// so sandboxed runners can execute outbound-audit tests.
type stubRelayTripper struct {
	requests []*http.Request
	bodies   [][]byte
	status   int
}

func (s *stubRelayTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	s.requests = append(s.requests, r)
	s.bodies = append(s.bodies, body)
	return &http.Response{StatusCode: s.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
}

func newProxyStubHarness(t *testing.T, stub *stubRelayTripper) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.Mode = config.ModeProxy
	cfg.Secret = "test-secret-long-enough-01234567"
	cfg.ScanRelayURL = "http://127.0.0.1:9/unused"
	st, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"), store.WithQueueCap(cfg.WriterQueueCap))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := New(cfg, st, forwarder.NewGate())
	if err != nil {
		t.Fatal(err)
	}
	srv.proxy.Transport = stub
	return &harness{srv: srv, secret: "test-secret-long-enough-01234567"}
}

// Outbound audit (Step 30 §§7.2–7.5): the relay receives the producer
// body byte-identical with only the permitted headers — no severity or
// content rewrite, no stored decisions/rules/incidents/traces folded
// in, no inbound credential or foreign header passed through.
func TestForwardAuditByteIdenticalAndMinimal(t *testing.T) {
	stub := &stubRelayTripper{status: http.StatusOK}
	h := newProxyStubHarness(t, stub)
	now := time.Now()
	id := uniqueID()
	body := []byte(`{"event_id":` + strconv.Quote(id) + `,"event_key":"host-a","event_type":"host.appeared",` +
		`"occurred_at":"2026-10-20T12:00:05Z","cycle":1,"target":"192.168.0.0/24",` +
		`"host":{"alive":true,"ip":"192.168.0.99","weak":false}}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	// Hostile inbound headers ride the ingest: none may pass through
	// to the relay.
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-SubnetLens-Event-ID", id)
	req.Header.Set("X-SubnetLens-Event-Key", "host-a")
	req.Header.Set("X-SubnetLens-Event-Type", "host.appeared")
	req.Header.Set("X-SubnetLens-Signature-Timestamp", ts)
	req.Header.Set("X-SubnetLens-Signature-256", sign(h.secret, ts, id, "host-a", "host.appeared", body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer inbound-creds-must-not-forward")
	req.Header.Set("X-Custom-Secret", "drop-me")
	req.Header.Set("Cookie", "session=drop-me")
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("proxy ingest: got %d %s", rec.Code, rec.Body.String())
	}
	if len(stub.bodies) != 1 || !bytes.Equal(stub.bodies[0], body) {
		t.Fatal("forwarded bytes are not byte-identical to the producer body")
	}
	out := stub.requests[0]
	for k := range out.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" {
			t.Fatal("inbound Authorization must never forward (creds inject only from config at send time)")
		}
		if !strings.HasPrefix(lk, "x-subnetlens-") && lk != "content-type" && lk != "user-agent" && lk != "content-length" {
			t.Fatalf("unexpected forwarded header %q", k)
		}
	}
	if out.Header.Get("X-SubnetLens-Event-ID") != id || out.Header.Get("X-SubnetLens-Signature-256") == "" {
		t.Fatalf("signature headers not verbatim: %v", out.Header)
	}
	// Nothing local is folded into the relay body: the forwarded bytes
	// equal the producer submission, which carried no decision/rule
	// text — and the store holds no such rows for this event.
	if rows, err := h.srv.events.ListIncidents(context.Background(), 200, 0, 0); err != nil || len(rows) != 0 {
		t.Fatalf("incidents = %+v %v", rows, err)
	}
}
