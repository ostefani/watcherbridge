package viewer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/health"
	"github.com/ostefani/watcherbridge/internal/store"
)

// Step 20 overview and health routes: real computation over retained
// rows, shared window validation, and honest unknown liveness.
func TestOverviewAndHealth(t *testing.T) {
	s := openViewerStore(t)
	seedBriefing(t, s, "host:ov", "evt-ov")
	v := New(s, NewHub())
	v.Overview = NewRefresher(s, health.New(), nil, 24*time.Hour, 168*time.Hour, 30*24*time.Hour)

	overview := func(target string) (int, overviewWire) {
		t.Helper()
		req := httptest.NewRequest("GET", target, nil)
		rec := httptest.NewRecorder()
		v.handleOverview(rec, req)
		res := rec.Result()
		defer res.Body.Close()
		var wire overviewWire
		if res.StatusCode == http.StatusOK {
			if err := json.NewDecoder(res.Body).Decode(&wire); err != nil {
				t.Fatal(err)
			}
		}
		return res.StatusCode, wire
	}

	code, wire := overview("/api/overview?window=24h")
	if code != http.StatusOK {
		t.Fatalf("overview status = %d", code)
	}
	if wire.Verdict != "act" || wire.Headline != "1 need a check now" {
		t.Fatalf("verdict = %q headline = %q", wire.Verdict, wire.Headline)
	}
	if len(wire.Attention) != 1 || wire.Attention[0].Status != "attention" || wire.Attention[0].Priority != "high" {
		t.Fatalf("attention = %+v", wire.Attention)
	}
	if wire.Receipts != 1 || wire.Coverage != "ok" {
		t.Fatalf("receipts = %d coverage = %q", wire.Receipts, wire.Coverage)
	}
	if len(wire.Sources) != 1 || wire.Sources[0].Scope != "test-scope" {
		t.Fatalf("sources = %+v", wire.Sources)
	}
	if wire.Sources[0].Liveness != "unknown" {
		t.Fatalf("liveness = %q, want unknown until H1", wire.Sources[0].Liveness)
	}
	for _, bad := range []string{
		"/api/overview?window=bogus",
		"/api/overview?window=-1h",
		"/api/overview?window=200h",
	} {
		if code, _ := overview(bad); code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", bad, code)
		}
	}

	// Production runs a startup purge before serving; the ledger row the
	// health route reports comes from the same call.
	if _, err := s.PurgeOnce(context.Background(), store.PurgeSettings{
		BriefingRetention: 30 * 24 * time.Hour,
		SubjectRetention:  90 * 24 * time.Hour,
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/health", nil)
	rec := httptest.NewRecorder()
	v.handleHealth(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", res.StatusCode)
	}
	var hw healthWire
	if err := json.NewDecoder(res.Body).Decode(&hw); err != nil {
		t.Fatal(err)
	}
	if hw.Coverage != "ok" || hw.Fallback.Total != 1 || hw.ParkedGroups != 0 {
		t.Fatalf("health = %+v", hw)
	}
	if hw.Process.StartedAt.IsZero() {
		t.Fatal("process start time must be exposed")
	}
	if hw.QueueCap <= 0 {
		t.Fatalf("queue cap = %d", hw.QueueCap)
	}
	if hw.Publisher != nil {
		t.Fatal("publisher must be absent when disabled")
	}
	if hw.Purge.Last == nil || hw.Purge.Last.Status != "completed" {
		t.Fatalf("purge last = %+v", hw.Purge.Last)
	}
}

// The refresher broadcasts only on hash change and coalesces triggers.
func TestRefresherHashChange(t *testing.T) {
	s := openViewerStore(t)
	r := NewRefresher(s, health.New(), nil, 24*time.Hour, 168*time.Hour, 30*24*time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var broadcasts atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx, 50*time.Millisecond, func([]byte) {
			broadcasts.Add(1)
		})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.Cached() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.Cached() == nil {
		t.Fatal("no initial overview computed")
	}
	n := broadcasts.Load()
	for i := 0; i < 10; i++ {
		r.Trigger()
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	// Triggers without new briefings coalesce and keep the hash: at most
	// the initial broadcast plus one refresh lands.
	if broadcasts.Load() > n+1 {
		t.Fatalf("broadcasts = %d, want no repeat without change", broadcasts.Load())
	}
	seedBriefing(t, s, "host:ov2", "evt-ov2")
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	changed := make(chan struct{}, 1)
	go r.Run(ctx2, 50*time.Millisecond, func(body []byte) {
		changed <- struct{}{}
	})
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("new briefing did not change the overview hash")
	}
	cancel2()
}

// One overview request costs one bounded candidate read, one grouped
// digest, and a few scalar counts — the number ingest budgeting needs.
func BenchmarkOverviewHandler(b *testing.B) {
	s := openViewerStore(b)
	seedBriefing(b, s, "host:bench", "evt-bench")
	v := New(s, NewHub())
	v.Overview = NewRefresher(s, health.New(), nil, 24*time.Hour, 168*time.Hour, 30*24*time.Hour)
	req := httptest.NewRequest("GET", "/api/overview?window=24h", nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		v.handleOverview(rec, req)
		if rec.Code != 200 {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

// Quiet with no receipts carries counts, window, and no presence claim.
func TestOverviewQuietHonest(t *testing.T) {
	s := openViewerStore(t)
	v := New(s, NewHub())
	v.Overview = NewRefresher(s, health.New(), nil, 24*time.Hour, 168*time.Hour, 30*24*time.Hour)
	req := httptest.NewRequest("GET", "/api/overview?window=24h", nil)
	rec := httptest.NewRecorder()
	v.handleOverview(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	var wire overviewWire
	if err := json.NewDecoder(res.Body).Decode(&wire); err != nil {
		t.Fatal(err)
	}
	if wire.Verdict != "quiet" || wire.Headline != "Nothing needs review" {
		t.Fatalf("verdict = %q headline = %q", wire.Verdict, wire.Headline)
	}
	if wire.Receipts != 0 || wire.Window != (24*time.Hour).String() {
		t.Fatalf("receipts = %d window = %q", wire.Receipts, wire.Window)
	}
	if strings.Contains(string(mustJSON(wire)), "secure") {
		t.Fatal("quiet overview must not claim security")
	}
}
