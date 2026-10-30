package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/briefing"
	"github.com/ostefani/watcherbridge/internal/config"
	"github.com/ostefani/watcherbridge/internal/forwarder"
	"github.com/ostefani/watcherbridge/internal/store"
)

// fakeRelay mirrors ScanRelay's /webhook surface: accept (202), replay
// (409 + the exact replay body), fail (500), or hang. It records every
// forwarded body and header set byte-for-byte for golden comparison.
type fakeRelay struct {
	mode    atomic.Value // string: accept|replay|fail|hang
	bodies  [][]byte
	headers []http.Header
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	hits    atomic.Int64
	srv     *httptest.Server
}

func newFakeRelay(t *testing.T) *fakeRelay {
	f := &fakeRelay{entered: make(chan struct{}, 1024), release: make(chan struct{})}
	f.mode.Store("accept")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, bytes.Clone(body))
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		f.hits.Add(1)
		select {
		case f.entered <- struct{}{}:
		default:
		}
		switch f.mode.Load().(string) {
		case "replay":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprintln(w, `{"error":"replayed request"}`)
		case "fail":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
		case "hang":
			select {
			case <-f.release:
				// Re-read the mode after release so a test can flip the
				// outcome (fail/replay) while the forward is held.
				switch f.mode.Load().(string) {
				case "fail":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
				case "replay":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusConflict)
					_, _ = fmt.Fprintln(w, `{"error":"replayed request"}`)
				default:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
				}
			case <-r.Context().Done():
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func sign(secret, ts, id, key, typ string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	for _, s := range []string{ts, ".", id, ".", key, ".", typ, "."} {
		_, _ = mac.Write([]byte(s))
	}
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

var idSeq atomic.Int64

func eventBody(id, key, typ string) []byte {
	return []byte(fmt.Sprintf(`{"event_id":%q,"event_key":%q,"event_type":%q,"occurred_at":%q,"cycle":1,"target":"192.168.0.0/24"}`,
		id, key, typ, time.Now().UTC().Format(time.RFC3339Nano)))
}

type harness struct {
	srv    *Server
	relay  *fakeRelay
	secret string
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	relay := newFakeRelay(t)
	cfg := config.Default()
	cfg.Mode = config.ModeProxy // most tests exercise forwarding; tap tests override
	cfg.ScanRelayURL = relay.srv.URL
	cfg.Secret = "test-secret-long-enough-01234567"
	if mutate != nil {
		mutate(&cfg)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"), store.WithQueueCap(cfg.WriterQueueCap))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := New(cfg, st, forwarder.NewGate())
	if err != nil {
		t.Fatal(err)
	}
	return &harness{srv: srv, relay: relay, secret: "test-secret-long-enough-01234567"}
}

func (h *harness) post(t *testing.T, body []byte, id, key, typ string, ts time.Time, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-SubnetLens-Event-ID", id)
	req.Header.Set("X-SubnetLens-Event-Key", key)
	req.Header.Set("X-SubnetLens-Event-Type", typ)
	req.Header.Set("X-SubnetLens-Signature-Timestamp", strconv.FormatInt(ts.Unix(), 10))
	req.Header.Set("X-SubnetLens-Signature-256", sig)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (h *harness) signedPost(t *testing.T, id, key, typ string, ts time.Time) *httptest.ResponseRecorder {
	t.Helper()
	body := eventBody(id, key, typ)
	return h.post(t, body, id, key, typ, ts, sign(h.secret, strconv.FormatInt(ts.Unix(), 10), id, key, typ, body))
}

func uniqueID() string { return fmt.Sprintf("evt-%d", idSeq.Add(1)) }

func TestContractTable(t *testing.T) {
	h := newHarness(t, nil)
	now := time.Now()

	id := uniqueID()
	if rec := h.signedPost(t, id, "host-a", "host.appeared", now); rec.Code != http.StatusAccepted {
		t.Fatalf("valid tap/proxy request: got %d %s", rec.Code, rec.Body.String())
	}

	// 401: bad signature.
	bad := eventBody(uniqueID(), "host-a", "host.appeared")
	if rec := h.post(t, bad, "evt-bad", "host-a", "host.appeared", now, "sha256=deadbeef"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: got %d", rec.Code)
	}
	// 401: stale timestamp.
	old := eventBody(uniqueID(), "host-a", "host.appeared")
	ts := strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
	if rec := h.post(t, old, "evt-old", "host-a", "host.appeared", now.Add(-time.Hour), sign(h.secret, ts, "evt-old", "host-a", "host.appeared", old)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale timestamp: got %d", rec.Code)
	}
	// 401: timestamp too far in the future.
	futureBody := eventBody(uniqueID(), "host-a", "host.appeared")
	future := now.Add(time.Hour)
	fts := strconv.FormatInt(future.Unix(), 10)
	if rec := h.post(t, futureBody, "evt-future", "host-a", "host.appeared", future, sign(h.secret, fts, "evt-future", "host-a", "host.appeared", futureBody)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("future timestamp: got %d", rec.Code)
	}
	// 400: unknown event type (valid signature over the same bytes).
	weird := []byte(`{"event_id":"evt-x","event_key":"host-a","event_type":"host.mutated"}`)
	wts := strconv.FormatInt(now.Unix(), 10)
	if rec := h.post(t, weird, "evt-x", "host-a", "host.mutated", now, sign(h.secret, wts, "evt-x", "host-a", "host.mutated", weird)); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown event type: got %d", rec.Code)
	}
	// 400: non-JSON body with a valid signature.
	raw := []byte("not json at all{{{")
	rts := strconv.FormatInt(now.Unix(), 10)
	if rec := h.post(t, raw, "evt-raw", "host-a", "host.appeared", now, sign(h.secret, rts, "evt-raw", "host-a", "host.appeared", raw)); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: got %d", rec.Code)
	}
	// 413: oversize.
	big := bytes.Repeat([]byte("x"), int(h.srv.cfg.MaxBodyBytes)+1)
	if rec := h.post(t, big, "evt-big", "host-a", "host.appeared", now, "sha256=irrelevant"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: got %d", rec.Code)
	}
	// 405: non-POST.
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d", rec.Code)
	}
	// 502: ScanRelay down.
	h.relay.mode.Store("fail")
	failID := uniqueID()
	if rec := h.signedPost(t, failID, "host-b", "host.appeared", now); rec.Code != http.StatusBadGateway {
		t.Fatalf("forward failure: got %d", rec.Code)
	}
	// 202 duplicate of an unforwarded event: waiter path returns 502, and
	// the retry after a successful forward returns 202.
	if rec := h.signedPost(t, failID, "host-b", "host.appeared", now); rec.Code != http.StatusBadGateway {
		t.Fatalf("duplicate of unforwarded: got %d", rec.Code)
	}
	h.relay.mode.Store("accept")
	if rec := h.signedPost(t, failID, "host-b", "host.appeared", now); rec.Code != http.StatusAccepted {
		t.Fatalf("retry after failure: got %d %s", rec.Code, rec.Body.String())
	}
	if h.srv.gate.InFlight() != 0 {
		t.Fatal("gate leaked an entry")
	}
}

func TestForwardGoldenBytesAndHeaders(t *testing.T) {
	h := newHarness(t, nil)
	now := time.Now()
	id := uniqueID()
	body := eventBody(id, "host-golden", "host.changed")
	ts := strconv.FormatInt(now.Unix(), 10)
	rec := h.post(t, body, id, "host-golden", "host.changed", now, sign(h.secret, ts, id, "host-golden", "host.changed", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	h.relay.mu.Lock()
	defer h.relay.mu.Unlock()
	if len(h.relay.bodies) != 1 || !bytes.Equal(h.relay.bodies[0], body) {
		t.Fatal("forwarded bytes are not byte-identical to Pro's body")
	}
	got := h.relay.headers[0]
	if got.Get("X-SubnetLens-Event-ID") != id || got.Get("X-SubnetLens-Event-Key") != "host-golden" ||
		got.Get("X-SubnetLens-Event-Type") != "host.changed" || got.Get("X-SubnetLens-Signature-256") == "" {
		t.Fatalf("forwarded headers not verbatim: %v", got)
	}
	if got.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type not forwarded: %v", got)
	}
}

func TestReplayShapeMarksForwarded(t *testing.T) {
	h := newHarness(t, nil)
	now := time.Now()
	h.relay.mode.Store("replay")
	id := uniqueID()
	if rec := h.signedPost(t, id, "host-r", "host.appeared", now); rec.Code != http.StatusAccepted {
		t.Fatalf("replay-shaped 409 must count as delivered: got %d", rec.Code)
	}
	fwd, _, _, err := h.srv.events.ForwardState(context.Background(), id)
	if err != nil || !fwd {
		t.Fatalf("forwarded flag not set after replay shape: %v %v", fwd, err)
	}
}

func TestWaiterFollowsWinner(t *testing.T) {
	h := newHarness(t, nil)
	now := time.Now()
	h.relay.mode.Store("hang")
	id := uniqueID()
	body := eventBody(id, "host-w", "host.appeared")
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := sign(h.secret, ts, id, "host-w", "host.appeared", body)

	first := make(chan int, 1)
	go func() {
		first <- h.post(t, body, id, "host-w", "host.appeared", now, sig).Code
	}()
	<-h.relay.entered // holder is inside the forward; duplicate must wait
	second := make(chan int, 1)
	go func() {
		second <- h.post(t, body, id, "host-w", "host.appeared", now, sig).Code
	}()
	select {
	case code := <-second:
		t.Fatalf("waiter replied before the winner finished: %d", code)
	case <-time.After(200 * time.Millisecond):
	}
	close(h.relay.release) // accept
	if got := <-first; got != http.StatusAccepted {
		t.Fatalf("winner: got %d", got)
	}
	if got := <-second; got != http.StatusAccepted {
		t.Fatalf("waiter: got %d", got)
	}
	if n := h.relay.hits.Load(); n != 1 {
		t.Fatalf("duplicate forwarded inline: %d forward hits", n)
	}
}

func TestWaiterReturns502WhenWinnerFails(t *testing.T) {
	h := newHarness(t, nil)
	now := time.Now()
	h.relay.mode.Store("hang")
	id := uniqueID()
	body := eventBody(id, "host-wf", "host.appeared")
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := sign(h.secret, ts, id, "host-wf", "host.appeared", body)

	first := make(chan int, 1)
	go func() {
		first <- h.post(t, body, id, "host-wf", "host.appeared", now, sig).Code
	}()
	<-h.relay.entered
	second := make(chan int, 1)
	go func() {
		second <- h.post(t, body, id, "host-wf", "host.appeared", now, sig).Code
	}()
	time.Sleep(200 * time.Millisecond)
	h.relay.mode.Store("fail")
	close(h.relay.release)
	if got := <-first; got != http.StatusBadGateway {
		t.Fatalf("winner: got %d", got)
	}
	if got := <-second; got != http.StatusBadGateway {
		t.Fatalf("waiter must return 502 without forwarding inline: got %d", got)
	}
	if n := h.relay.hits.Load(); n != 1 {
		t.Fatalf("waiter forwarded inline: %d hits", n)
	}
	// Pro's retry then succeeds and is accepted.
	h.relay.mode.Store("accept")
	if rec := h.signedPost(t, id, "host-wf", "host.appeared", now); rec.Code != http.StatusAccepted {
		t.Fatalf("retry: got %d", rec.Code)
	}
}

func TestCancelDuringPersistThenRetry(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModeTap })
	now := time.Now()
	id := uniqueID()
	body := eventBody(id, "host-c", "host.appeared")
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := sign(h.secret, ts, id, "host-c", "host.appeared", body)

	// First attempt with a context cancelled mid-flight: whatever status it
	// reports, the retry must converge to exactly one event and one group.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("X-SubnetLens-Event-ID", id)
	req.Header.Set("X-SubnetLens-Event-Key", "host-c")
	req.Header.Set("X-SubnetLens-Event-Type", "host.appeared")
	req.Header.Set("X-SubnetLens-Signature-Timestamp", ts)
	req.Header.Set("X-SubnetLens-Signature-256", sig)
	req.Header.Set("Content-Type", "application/json")
	h.srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	if rec := h.signedPost(t, id, "host-c", "host.appeared", now); rec.Code != http.StatusAccepted {
		t.Fatalf("retry: got %d %s", rec.Code, rec.Body.String())
	}
	if _, _, _, err := h.srv.events.ForwardState(context.Background(), id); err != nil {
		t.Fatalf("event missing after cancel+retry: %v", err)
	}
	groups, err := h.srv.events.GroupEventCount(context.Background(), "host-c")
	if err != nil {
		t.Fatal(err)
	}
	if groups != 1 {
		t.Fatalf("expected exactly one group entry after cancel+retry, got %d", groups)
	}
}

func TestSaturation503WithRetryAfter(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.InFlightCap = 2 })
	now := time.Now()
	h.relay.mode.Store("hang")

	done := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			done <- h.signedPost(t, uniqueID(), "host-s", "host.appeared", now).Code
		}()
	}
	<-h.relay.entered
	<-h.relay.entered // both in-flight slots are held inside the forward

	// N+1 concurrent bodies against an in-flight x MAX_BODY_BYTES bound.
	big := bytes.Repeat([]byte("e"), int(h.srv.cfg.MaxBodyBytes))
	rec := h.post(t, big, uniqueID(), "host-s", "host.appeared", now, "sha256=unused")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated server: got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") != strconv.Itoa(h.srv.cfg.RetryAfterSecs) {
		t.Fatalf("missing Retry-After: %v", rec.Header())
	}
	close(h.relay.release)
	for i := 0; i < 2; i++ {
		if got := <-done; got != http.StatusAccepted {
			t.Fatalf("held request: got %d", got)
		}
	}
}

func TestHungScanRelay502Timing(t *testing.T) {
	h := newHarness(t, nil)
	now := time.Now()
	h.relay.mode.Store("hang")
	start := time.Now()
	id := uniqueID()
	rec := h.signedPost(t, id, "host-h", "host.appeared", now)
	elapsed := time.Since(start)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("hung ScanRelay: got %d", rec.Code)
	}
	want := h.srv.cfg.ProxyForwardTimeout
	if elapsed < want || elapsed > want+2*time.Second {
		t.Fatalf("502 took %v, want ~persist+%v", elapsed, want)
	}
}

func seedUnforwarded(t *testing.T, h *harness, id, key string, sigAge time.Duration) {
	t.Helper()
	now := time.Now()
	body := eventBody(id, key, "host.appeared")
	ts := now.Add(-sigAge)
	stamp := strconv.FormatInt(ts.Unix(), 10)
	headers, _ := json.Marshal(map[string]string{
		"X-SubnetLens-Event-ID":            id,
		"X-SubnetLens-Event-Key":           key,
		"X-SubnetLens-Event-Type":          "host.appeared",
		"X-SubnetLens-Signature-Timestamp": stamp,
		"X-SubnetLens-Signature-256":       sign(h.secret, stamp, id, key, "host.appeared", body),
		"Content-Type":                     "application/json",
	})
	_, err := h.srv.events.PersistEventAndGroup(context.Background(), store.RawEvent{
		EventID: id, EventKey: key, EventType: "host.appeared",
		OccurredAt: now, Payload: body, HeadersJSON: string(headers),
		ReceivedAt: now, SignatureTimestamp: ts,
	}, key, store.GroupPolicy{QuietWindow: 45 * time.Second, MaxWindow: 120 * time.Second, MaxEvents: 10})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSweepRetriesFreshAndMarksStale(t *testing.T) {
	h := newHarness(t, nil)
	fresh := uniqueID()
	stale := uniqueID()
	seedUnforwarded(t, h, fresh, "host-fresh", time.Minute)
	seedUnforwarded(t, h, stale, "host-stale", 10*time.Minute)

	h.srv.SweepOnce(context.Background())

	if fwd, _, _, _ := h.srv.events.ForwardState(context.Background(), fresh); !fwd {
		t.Fatal("in-window event was not retried by the sweep")
	}
	// Swept bytes and headers must be golden-compared identical too.
	h.relay.mu.Lock()
	if len(h.relay.bodies) != 1 {
		h.relay.mu.Unlock()
		t.Fatalf("sweep forwarded %d events, want 1", len(h.relay.bodies))
	}
	var decoded map[string]any
	if err := json.Unmarshal(h.relay.bodies[0], &decoded); err != nil || decoded["event_id"] != fresh {
		h.relay.mu.Unlock()
		t.Fatalf("sweep forwarded the wrong event: %v %v", decoded, err)
	}
	h.relay.mu.Unlock()
	if fwd, _, _, _ := h.srv.events.ForwardState(context.Background(), stale); fwd {
		t.Fatal("out-of-window event must never be forwarded by the sweep")
	}
	if h.relay.hits.Load() != 1 {
		t.Fatal("stale event reached ScanRelay")
	}
}

func TestSweepSkipsGatedEvent(t *testing.T) {
	h := newHarness(t, nil)
	id := uniqueID()
	seedUnforwarded(t, h, id, "host-g", time.Minute)
	release, _ := h.srv.gate.Acquire(id)
	defer release()
	h.srv.SweepOnce(context.Background())
	if fwd, _, _, _ := h.srv.events.ForwardState(context.Background(), id); fwd {
		t.Fatal("sweep forwarded an event owned by a live handler")
	}
	if h.relay.hits.Load() != 0 {
		t.Fatal("sweep forwarded through a held gate")
	}
}

func TestBurstZeroLoss(t *testing.T) {
	for _, mode := range []string{config.ModeTap, config.ModeProxy} {
		h := newHarness(t, func(c *config.Config) { c.Mode = mode })
		now := time.Now()
		for i := 0; i < 512; i++ {
			id := uniqueID()
			rec := h.signedPost(t, id, fmt.Sprintf("host-%d", i%32), "host.appeared", now)
			if rec.Code == http.StatusServiceUnavailable {
				t.Fatalf("%s sequential burst: unexpected 503 at %d", mode, i)
			}
			if rec.Code != http.StatusAccepted {
				t.Fatalf("%s sequential burst: got %d at %d: %s", mode, rec.Code, i, rec.Body.String())
			}
		}
		if h.srv.gate.InFlight() != 0 {
			t.Fatalf("%s: gate leaked", mode)
		}
	}
}

func TestSustainedZeroLoss(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModeTap })
	now := time.Now()
	tick := time.NewTicker(20 * time.Millisecond) // 50 events/s
	defer tick.Stop()
	const n = 500 // 10s at 50/s; the 60s soak is a benchmark, not a unit test
	lost := 0
	for i := 0; i < n; i++ {
		<-tick.C
		if rec := h.signedPost(t, uniqueID(), "host-p", "host.changed", now); rec.Code != http.StatusAccepted {
			lost++
		}
	}
	if lost != 0 {
		t.Fatalf("sustained load lost %d/%d events", lost, n)
	}
}

func walSize(t *testing.T, dbPath string) int64 {
	t.Helper()
	fi, err := os.Stat(dbPath + "-wal")
	if err != nil {
		return 0
	}
	return fi.Size()
}

func TestWALBoundedUnderLoad(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "wal.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := &harness{secret: "test-secret-long-enough-01234567"}
	relay := newFakeRelay(t)
	cfg := config.Default()
	cfg.Mode = config.ModeProxy
	cfg.ScanRelayURL = relay.srv.URL
	cfg.Secret = "test-secret-long-enough-01234567"
	cfg.MaxBodyBytes = 256 << 10
	srv, err := New(cfg, st, forwarder.NewGate())
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	h.relay = relay

	// A concurrent long-lived read transaction: it pins the WAL so a
	// checkpoint cannot reclaim pages while it is open.
	readCtx, stopRead := context.WithCancel(context.Background())
	defer stopRead()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			select {
			case <-readCtx.Done():
				return
			default:
				_, _ = st.GroupEventCount(readCtx, "host-never")
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	now := time.Now()
	pad := string(bytes.Repeat([]byte("p"), 200<<10))
	for i := 0; i < 150; i++ {
		id := uniqueID()
		trimmed := eventBody(id, "host-wal", "host.changed")
		trimmed = trimmed[:len(trimmed)-1] // strip closing brace
		body := append(trimmed, []byte(fmt.Sprintf(`,"pad":%q}`, pad))...)
		ts := strconv.FormatInt(now.Unix(), 10)
		rec := h.post(t, body, id, "host-wal", "host.changed", now, sign(h.secret, ts, id, "host-wal", "host.changed", body))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("wal load event %d: got %d %s", i, rec.Code, rec.Body.String())
		}
	}
	stopRead()
	<-readDone
	if got := walSize(t, dbPath); got > 64<<20 {
		t.Fatalf("WAL reached %d bytes under sustained load with a pinned reader, budget 64MiB", got)
	}
	// After the reader ends and the store goes idle, a checkpoint through
	// the single writer must drain the WAL (SQLite never checkpoints a
	// merely idle database on its own).
	if err := st.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := walSize(t, dbPath); got > 1<<20 {
		t.Fatalf("WAL did not shrink after checkpoint: %d bytes", got)
	}
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	t.Logf("heap in use after WAL load: %d bytes", mem.HeapInuse)
	if mem.HeapInuse > 64<<20 {
		t.Fatalf("heap %d bytes exceeds 64MiB budget", mem.HeapInuse)
	}
}

func TestLatencyAndHeapBudgets(t *testing.T) {
	if testing.Short() {
		t.Skip("latency budgets need a full run")
	}
	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModeTap })
	now := time.Now()
	const n = 200
	durs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		if rec := h.signedPost(t, uniqueID(), "host-l", "host.appeared", now); rec.Code != http.StatusAccepted {
			t.Fatalf("tap request %d: got %d", i, rec.Code)
		}
		durs = append(durs, time.Since(start))
	}
	sort.Slice(durs, func(a, b int) bool { return durs[a] < durs[b] })
	p99 := durs[int(float64(n)*0.99)]
	t.Logf("tap p99 over %d requests: %v (budget 50ms)", n, p99)
	if p99 > 50*time.Millisecond {
		t.Fatalf("tap p99 %v exceeds 50ms", p99)
	}
	var mem runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mem)
	t.Logf("heap in use after %d tap requests: %d bytes", n, mem.HeapInuse)
	if mem.HeapInuse > 64<<20 {
		t.Fatalf("heap %d bytes exceeds 64MiB budget", mem.HeapInuse)
	}

	proxy := newHarness(t, nil)
	var over []time.Duration
	for i := 0; i < n; i++ {
		start := time.Now()
		if rec := proxy.signedPost(t, uniqueID(), "host-p", "host.appeared", now); rec.Code != http.StatusAccepted {
			t.Fatalf("proxy request %d: got %d", i, rec.Code)
		}
		over = append(over, time.Since(start))
	}
	sort.Slice(over, func(a, b int) bool { return over[a] < over[b] })
	p99over := over[int(float64(n)*0.99)]
	t.Logf("proxy overhead p99 over %d requests: %v (budget 25ms)", n, p99over)
	if p99over > 25*time.Millisecond {
		t.Fatalf("proxy overhead p99 %v exceeds 25ms", p99over)
	}
}

func TestShutdownDuringSweepForward(t *testing.T) {
	h := newHarness(t, nil)
	id := uniqueID()
	seedUnforwarded(t, h, id, "host-sh", time.Minute)
	h.relay.mode.Store("hang")

	sweepCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.SweepOnce(sweepCtx)
	}()
	<-h.relay.entered // sweep forward is in flight
	cancel()          // shutdown at this point must not lose the receipt write
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sweep did not honor shutdown")
	}
	// The in-flight forward fails (context gone) but the store must still
	// answer consistently: either forwarded or recorded as a failure.
	fwd, attempts, _, err := h.srv.events.ForwardState(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !fwd && attempts == 0 {
		t.Fatal("in-flight sweep event vanished without a trace")
	}
	if h.srv.gate.InFlight() != 0 {
		t.Fatal("gate leaked after shutdown sweep")
	}
}

func TestAnchorDerivation(t *testing.T) {
	hostBody := func(hostJSON string) []byte {
		return []byte(fmt.Sprintf(`{"event_id":"e1","event_key":"change:x:20260101T0000Z","event_type":"host.appeared","occurred_at":%q,"host":%s}`,
			time.Now().UTC().Format(time.RFC3339Nano), hostJSON))
	}
	cases := map[string]struct {
		body string
		typ  string
		want string
		fb   bool
	}{
		"mac preferred":           {string(hostBody(`{"mac":"AA:BB:CC:DD:EE:FF","hostname":"pi","ip":"192.168.0.5"}`)), "host.appeared", "mac:aa:bb:cc:dd:ee:ff", false},
		"randomized mac skipped":  {string(hostBody(`{"mac":"AA:BB:CC:DD:EE:FF","randomized_mac":true,"hostname":"Pixel"}`)), "host.appeared", "host:pixel", false},
		"hostname only":           {string(hostBody(`{"hostname":"Printer."}`)), "host.disappeared", "host:printer", false},
		"ip passthrough":          {string(hostBody(`{"ip":"192.168.0.5"}`)), "host.appeared", "ip:192.168.0.5", false},
		"zero-padded ip rejected": {string(hostBody(`{"ip":"192.168.000.005"}`)), "host.appeared", "change:x:20260101T0000Z", true},
		"no identifiers":          {string(hostBody(`{"vendor":"acme"}`)), "host.appeared", "change:x:20260101T0000Z", true},
		"no host object":          {`{"event_id":"e1"}`, "host.appeared", "change:x:20260101T0000Z", true},
		"garbage ip falls back":   {string(hostBody(`{"ip":"not-an-ip"}`)), "host.appeared", "change:x:20260101T0000Z", true},
	}
	for name, tc := range cases {
		got, fb := anchorForEvent([]byte(tc.body), tc.typ, "change:x:20260101T0000Z")
		if got != tc.want || fb != tc.fb {
			t.Errorf("%s: got (%q,%v), want (%q,%v)", name, got, fb, tc.want, tc.fb)
		}
	}
	changed := []byte(`{"event_id":"e1","current":{"ip":"10.0.0.9"},"previous":{"ip":"10.0.0.8"}}`)
	if got, _ := anchorForEvent(changed, "host.changed", "k"); got != "ip:10.0.0.9" {
		t.Errorf("changed prefers current: got %q", got)
	}
	prevOnly := []byte(`{"event_id":"e1","previous":{"mac":"11:22:33:44:55:66"}}`)
	if got, _ := anchorForEvent(prevOnly, "host.changed", "k"); got != "mac:11:22:33:44:55:66" {
		t.Errorf("changed falls back to previous: got %q", got)
	}
}

func TestSameHostDifferentWindowsShareAGroup(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModeTap })
	now := time.Now()
	for i, key := range []string{"appear:mac-aa:20260101T0000Z", "change:mac-aa:deadbeef:20260102T0000Z"} {
		id := uniqueID()
		body := []byte(fmt.Sprintf(`{"event_id":%q,"event_key":%q,"event_type":"host.appeared","occurred_at":%q,"host":{"mac":"AA:BB:CC:DD:EE:01"}}`,
			id, key, now.Format(time.RFC3339Nano)))
		ts := strconv.FormatInt(now.Unix(), 10)
		if rec := h.post(t, body, id, key, "host.appeared", now, sign(h.secret, ts, id, key, "host.appeared", body)); rec.Code != http.StatusAccepted {
			t.Fatalf("event %d: got %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if n, err := h.srv.events.GroupEventCount(context.Background(), "mac:aa:bb:cc:dd:ee:01"); err != nil || n != 2 {
		t.Fatalf("same host across windows must share one group: count=%d err=%v", n, err)
	}
}

func TestTapSkipsSweep(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModeTap })
	id := uniqueID()
	seedUnforwarded(t, h, id, "host-t", time.Minute)
	h.srv.SweepOnce(context.Background())
	if fwd, _, _, _ := h.srv.events.ForwardState(context.Background(), id); fwd {
		t.Fatal("tap sweep forwarded an event ScanRelay already has")
	}
	if h.relay.hits.Load() != 0 {
		t.Fatal("tap sweep contacted ScanRelay")
	}
}

func TestSweepStopsWhenNothingLands(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModeProxy })
	h.relay.mode.Store("fail")
	for i := 0; i < 3; i++ {
		seedUnforwarded(t, h, uniqueID(), fmt.Sprintf("host-f%d", i), time.Minute)
	}
	done := make(chan struct{})
	go func() { defer close(done); h.srv.SweepOnce(context.Background()) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("sweep looped on failing forwards instead of stopping")
	}
	// One attempt per row, then the pass ends: bounded, no hot loop.
	if got := h.relay.hits.Load(); got != 3 {
		t.Fatalf("expected one attempt per row, got %d hits", got)
	}
}

func TestForwardAuthInjectedAtSendTime(t *testing.T) {
	var gotAuth, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotKey = r.Header.Get("Authorization"), r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
	}))
	defer srv.Close()

	h := newHarness(t, func(c *config.Config) {
		c.Mode = config.ModeProxy
		c.ScanRelayURL = srv.URL
		c.ForwardBearerToken = "relay-bearer-value"
		c.ForwardAPIKey = "relay-key-value"
	})
	if rec := h.signedPost(t, uniqueID(), "host-a", "host.appeared", time.Now()); rec.Code != http.StatusAccepted {
		t.Fatalf("got %d", rec.Code)
	}
	if gotAuth != "Bearer "+"relay-"+"bearer-value" {
		t.Fatalf("auth header mismatch: %q", gotAuth)
	}
	if gotKey != "" {
		t.Fatalf("api key must lose to bearer, got %q", gotKey)
	}
	// Stored headers must not contain the credentials.
	cands, err := h.srv.events.SweepCandidates(context.Background(), time.Now(), time.Hour, time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	_ = cands
}

func TestClosedStoreIs503(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModeTap })
	_ = h.srv.events.Close()
	if rec := h.signedPost(t, uniqueID(), "host-a", "host.appeared", time.Now()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed store: got %d, want 503 so Pro retries", rec.Code)
	} else if rec.Header().Get("Retry-After") == "" {
		t.Fatal("closed store: missing Retry-After")
	}
}

func TestEventIDCharset(t *testing.T) {
	now := time.Now()
	body := eventBody("e1", "host-a", "host.appeared")
	if _, err := parseAndValidate(now, 300*time.Second, strconv.FormatInt(now.Unix(), 10), "evt.bad", "host-a", "host.appeared", body); err == nil {
		t.Fatal("dots must be rejected from event IDs")
	}
	if _, err := parseAndValidate(now, 300*time.Second, strconv.FormatInt(now.Unix(), 10), "evt-01_2", "host-a", "host.appeared", body); err != nil {
		t.Fatalf("dash/underscore must stay valid: %v", err)
	}
}

func seedClosedGroups(t *testing.T, h *harness, n int) []string {
	t.Helper()
	now := time.Now().UTC()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := uniqueID()
		key := fmt.Sprintf("host-drain-%d", i)
		body := eventBody(id, key, "host.changed")
		res, err := h.srv.events.PersistEventAndGroup(context.Background(), store.RawEvent{
			EventID: id, EventKey: key, EventType: "host.changed",
			OccurredAt: now, Payload: body, ReceivedAt: now, SignatureTimestamp: now,
		}, key, store.GroupPolicy{QuietWindow: time.Second, MaxWindow: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.GroupID)
	}
	// Quiet windows are 1s by construction; one CloseDue far in the future
	// closes every seeded group deterministically.
	if _, err := h.srv.events.CloseDue(context.Background(), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return ids
}

func seededDone(t *testing.T, h *harness, ids []string) bool {
	t.Helper()
	for _, id := range ids {
		st, err := h.srv.events.GroupStatus(context.Background(), id)
		if err != nil || st != "done" {
			return false
		}
	}
	return true
}

func briefingDeps(t *testing.T, h *harness) briefing.Deps {
	t.Helper()
	return briefing.Deps{
		Store:  h.srv.events,
		Groups: store.GroupPolicy{QuietWindow: time.Second, MaxWindow: time.Hour},
		Tick:   10 * time.Millisecond,
		Lease:  h.srv.cfg.ProcessingLease,
		Now:    time.Now,
		Policy: briefing.Policy{
			EstablishedAge:   h.srv.cfg.EstablishedAge,
			StrongHistoryAge: h.srv.cfg.StrongHistoryAge,
			StableFields:     h.srv.cfg.StableIdentityFields,
		},
	}
}

func TestClosureDrainBudgets(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModeProxy })
	now := time.Now()
	seeded := seedClosedGroups(t, h, 256)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- briefing.Run(ctx, briefingDeps(t, h)) }()
	start := time.Now()

	// Background ingest at 50 ev/s on its own goroutine while the main
	// goroutine polls the seeded groups: every request lands while the
	// worker holds the shared writer.
	type ingestResult struct {
		durs      []time.Duration
		lost      int
		saturated int
	}
	ingestDone := make(chan ingestResult, 1)
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		var res ingestResult
		for i := 0; i < 100; i++ {
			<-tick.C
			reqStart := time.Now()
			rec := h.signedPost(t, uniqueID(), "host-bg", "host.appeared", now)
			res.durs = append(res.durs, time.Since(reqStart))
			switch rec.Code {
			case http.StatusAccepted:
			case http.StatusServiceUnavailable:
				res.saturated++
			default:
				res.lost++
			}
		}
		ingestDone <- res
	}()
	for !seededDone(t, h, seeded) {
		if time.Since(start) > 5*time.Second {
			t.Fatal("256 seeded groups not briefed within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	drainElapsed := time.Since(start)
	t.Logf("256-group drain took %v (budget 5s)", drainElapsed)
	res := <-ingestDone
	sort.Slice(res.durs, func(a, b int) bool { return res.durs[a] < res.durs[b] })
	p99 := res.durs[int(float64(len(res.durs))*0.99)]
	t.Logf("ingest p99 during drain over %d requests: %v (budget 50ms)", len(res.durs), p99)
	if p99 > 50*time.Millisecond {
		t.Fatalf("ingest p99 %v exceeds 50ms during drain", p99)
	}
	if res.saturated != 0 || res.lost != 0 {
		t.Fatalf("ingest loss during drain: saturated=%d lost=%d", res.saturated, res.lost)
	}
	var mem runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mem)
	t.Logf("heap after drain: %d bytes (budget 64MiB)", mem.HeapInuse)
	if mem.HeapInuse > 64<<20 {
		t.Fatalf("heap %d exceeds 64MiB", mem.HeapInuse)
	}
}

func FuzzParseAndValidate(f *testing.F) {
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	body := eventBody("evt-seed", "host-a", "host.appeared")
	f.Add(ts, "evt-seed", "host-a", "host.appeared", body)
	f.Add("not-a-number", "evt-seed", "host-a", "host.appeared", body)
	f.Add(ts, "evt bad spaces!!", "host-a", "host.appeared", body)
	f.Add(ts, strings.Repeat("x", 200), "host-a", "host.appeared", body)
	f.Add(ts, "evt-seed", "host-a", "host.nope", body)
	f.Add(ts, "evt-seed", "host-a", "host.appeared", []byte{})
	f.Fuzz(func(t *testing.T, timestamp, eventID, eventKey, eventType string, body []byte) {
		sigTime, err := parseAndValidate(now, 300*time.Second, timestamp, eventID, eventKey, eventType, body)
		if err != nil {
			return
		}
		// Accepted inputs must be narrow: bounded IDs, allowlisted type,
		// non-empty body, timestamp inside the symmetric window.
		if len(eventID) > maxEventIDLen || !eventTypes[eventType] || len(body) == 0 {
			t.Fatalf("over-wide accept: %q %q %d", eventID, eventType, len(body))
		}
		if d := math.Abs(float64(now.Sub(sigTime))); d > float64(300*time.Second) {
			t.Fatalf("timestamp outside window accepted: %v", time.Duration(d))
		}
	})
}

// RegisterOnShutdown hooks run when Shutdown starts, so open SSE tabs stop
// blocking the drain. The server is built directly (no fake relay), so the
// test needs no socket: Start either blocks in ListenAndServe or fails
// fast on bind, and Shutdown must run the hooks and return either way.
func TestRegisterOnShutdownRunsHooks(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeTap
	cfg.Secret = "test-secret-long-enough-01234567"
	st, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := New(cfg, st, forwarder.NewGate())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	hook := func() {
		mu.Lock()
		defer mu.Unlock()
		calls++
	}
	srv.RegisterOnShutdown(hook)
	srv.RegisterOnShutdown(hook)
	startDone := make(chan error, 1)
	go func() { startDone <- srv.Start() }()
	time.Sleep(200 * time.Millisecond) // Start blocks in ListenAndServe or fails fast
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("shutdown hooks ran %d/2", calls)
	}
}

// The Host allowlist gates viewer routes only: a foreign Host is 403 on
// /api/*, while /webhook keeps its own contract (405 on wrong method,
// 401 on bad signature, never 403). Built directly, bind-free.
func TestViewerHostGateLeavesWebhookAlone(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeTap
	cfg.Secret = "test-secret-long-enough-01234567"
	st, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := New(cfg, st, forwarder.NewGate())
	if err != nil {
		t.Fatal(err)
	}
	serve := func(method, target, host string, headers map[string]string, body string) *httptest.ResponseRecorder {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, target, rdr)
		req.Host = host
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := serve("GET", "/api/groups", "evil.example:8081", nil, ""); rec.Code != 403 {
		t.Fatalf("foreign host on viewer route = %d, want 403", rec.Code)
	}
	if rec := serve("GET", "/api/groups", "127.0.0.1:8081", nil, ""); rec.Code != 200 {
		t.Fatalf("loopback host on viewer route = %d, want 200", rec.Code)
	}
	if rec := serve("GET", "/webhook", "evil.example:8081", nil, ""); rec.Code != 405 {
		t.Fatalf("foreign host on GET /webhook = %d, want 405", rec.Code)
	}
	sig := map[string]string{
		"Content-Type":                     "application/json",
		"X-SubnetLens-Signature-Timestamp": fmt.Sprintf("%d", time.Now().Unix()),
		"X-SubnetLens-Event-ID":            "evt-x",
		"X-SubnetLens-Event-Key":           "k",
		"X-SubnetLens-Event-Type":          "host.appeared",
		"X-SubnetLens-Signature-256":       "deadbeef",
	}
	if rec := serve("POST", "/webhook", "evil.example:8081", sig, `{}`); rec.Code != 401 {
		t.Fatalf("foreign host on POST /webhook = %d, want 401", rec.Code)
	}
}

// Mux-generated responses carry the header set too: the viewer mount
// heads its own 405s, and the outer wrapper heads clean-path redirects
// and unknown paths (which stay ungated, outside the viewer subtree).
func TestMuxResponsesCarryHeaders(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeTap
	cfg.Secret = "test-secret-long-enough-01234567"
	st, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := New(cfg, st, forwarder.NewGate())
	if err != nil {
		t.Fatal(err)
	}
	serve := func(method, target, host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	headed := func(rec *httptest.ResponseRecorder) {
		t.Helper()
		if got := rec.Header().Get("Content-Security-Policy"); got == "" {
			t.Fatalf("missing CSP on %d", rec.Code)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("missing nosniff on %d", rec.Code)
		}
	}
	if rec := serve("POST", "/api/groups", "127.0.0.1:8081"); rec.Code != 405 {
		t.Fatalf("POST on a GET route = %d, want 405", rec.Code)
	} else {
		headed(rec)
	}
	if rec := serve("POST", "/api/groups", "evil.example:8081"); rec.Code != 403 {
		t.Fatalf("foreign host on mux 405 = %d, want 403", rec.Code)
	}
	// Go's mux redirects unclean paths with 307 (method-preserving).
	if rec := serve("GET", "/api//groups", "127.0.0.1:8081"); rec.Code != 307 {
		t.Fatalf("unclean path = %d, want 307", rec.Code)
	} else {
		headed(rec)
	}
	// The redirect happens before routing, so the Host policy must apply
	// first: a foreign Host gets no redirect.
	if rec := serve("GET", "/api//groups", "evil.example:8081"); rec.Code != 403 {
		t.Fatalf("foreign host on redirect = %d, want 403", rec.Code)
	}
	// Webhook dispatch covers every method: never the viewer gate.
	if rec := serve("PUT", "/webhook", "127.0.0.1:8081"); rec.Code != 405 {
		t.Fatalf("PUT /webhook = %d, want 405", rec.Code)
	}
	if rec := serve("PUT", "/webhook", "evil.example:8081"); rec.Code != 405 {
		t.Fatalf("foreign PUT /webhook = %d, want 405", rec.Code)
	}
	// Unknown GET paths fall through to the viewer subtree's SPA shell
	// ("GET /" is a subtree wildcard), headed like any viewer response.
	if rec := serve("GET", "/nope", "127.0.0.1:8081"); rec.Code != 200 {
		t.Fatalf("unknown path = %d, want 200", rec.Code)
	} else {
		headed(rec)
		if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Fatalf("SPA shell content type = %q", ct)
		}
	}
}
