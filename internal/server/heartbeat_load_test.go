package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/config"
	"github.com/ostefani/watcherbridge/internal/forwarder"
	"github.com/ostefani/watcherbridge/internal/store"
)

// BenchmarkHeartbeatIngest includes synthetic envelope construction, signing,
// validation and a real SQLite commit; it does not measure socket latency.
func BenchmarkHeartbeatIngest(b *testing.B) {
	cfg := config.Default()
	cfg.Secret = "synthetic-benchmark-signing-secret"
	st, err := store.Open(filepath.Join(b.TempDir(), "h1.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	srv, err := New(cfg, st, forwarder.NewGate())
	if err != nil {
		b.Fatal(err)
	}
	handler := srv.Handler()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now := time.Now().UTC()
		id := fmt.Sprintf("%032x", i+1)
		key := "heartbeat:" + id
		body, _ := json.Marshal(h1Body(id, now))
		ts := strconv.FormatInt(now.Unix(), 10)
		req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
		req.Header.Set("X-SubnetLens-Event-ID", id)
		req.Header.Set("X-SubnetLens-Event-Key", key)
		req.Header.Set("X-SubnetLens-Event-Type", "scan.heartbeat")
		req.Header.Set("X-SubnetLens-Signature-Timestamp", ts)
		req.Header.Set("X-SubnetLens-Signature-256", sign(string(cfg.Secret), ts, id, key, "scan.heartbeat", body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 202 {
			b.Fatalf("ingest %d: %d", i, rec.Code)
		}
	}
}

// TestHeartbeatSoak is opt-in because it deliberately runs the full 60-second
// workload. It uses real loopback HTTP, SQLite, and coalesced overview refreshes.
func TestHeartbeatSoak(t *testing.T) {
	if os.Getenv("WB_HEARTBEAT_SOAK") != "1" {
		t.Skip("set WB_HEARTBEAT_SOAK=1 for 50/s over 60s")
	}
	relay := newFakeRelay(t)
	cfg := config.Default()
	cfg.Mode = config.ModeProxy
	cfg.ScanRelayURL = relay.srv.URL
	cfg.Secret = "synthetic-soak-signing-secret-01234567"
	path := filepath.Join(t.TempDir(), "h1.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err := New(cfg, st, forwarder.NewGate())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{srv: srv, relay: relay, secret: string(cfg.Secret)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.srv.viewer.Overview.Run(ctx, 5*time.Second, func([]byte) {}) }()
	defer func() { cancel(); <-done }()
	server := httptest.NewServer(h.srv.Handler())
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	const n = 3000
	durations := make([]time.Duration, 0, n)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	for i := 0; i < n; i++ {
		<-ticker.C
		now := time.Now().UTC()
		id := fmt.Sprintf("%032x", i+1)
		key := "heartbeat:" + id
		body, _ := json.Marshal(h1Body(id, now))
		ts := strconv.FormatInt(now.Unix(), 10)
		req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/webhook", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-SubnetLens-Event-ID", id)
		req.Header.Set("X-SubnetLens-Event-Key", key)
		req.Header.Set("X-SubnetLens-Event-Type", "scan.heartbeat")
		req.Header.Set("X-SubnetLens-Signature-Timestamp", ts)
		req.Header.Set("X-SubnetLens-Signature-256", sign(h.secret, ts, id, key, "scan.heartbeat", body))
		at := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		durations = append(durations, time.Since(at))
		if resp.StatusCode != 202 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p99 := durations[n*99/100]
	if p99 > 25*time.Millisecond {
		t.Fatalf("H1 proxy-mode p99 %v exceeds 25ms", p99)
	}
	if h.relay.hits.Load() != 0 {
		t.Fatal("H1 reached relay during soak")
	}
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	if mem.HeapInuse > 64<<20 {
		t.Fatalf("heap %d exceeds 64MiB", mem.HeapInuse)
	}
	wal, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if wal.Size() > 64<<20 {
		t.Fatalf("WAL %d exceeds 64MiB", wal.Size())
	}
	t.Logf("H1: %d accepted in %v, 50/s, p99=%v, heap=%d bytes, WAL=%d bytes, relay calls=0", n, time.Since(start), p99, mem.HeapInuse, wal.Size())
}
