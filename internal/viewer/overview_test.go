package viewer

import (
	"context"
	"encoding/json"
	"fmt"
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
	// No failure yet, so the latest attempt is also the last success.
	if hw.Purge.LastSuccess == nil || hw.Purge.LastSuccess.Status != "completed" {
		t.Fatalf("purge last success = %+v", hw.Purge.LastSuccess)
	}
	// Step 24: identity namespace/capabilities mirror the stored
	// registration; nothing writes them yet, so the API must read
	// empty rather than inventing a capability. Heartbeat enablement
	// and identity capability stay separate claims.
	if hw.Sources[0].IdentityNamespace != "" || hw.Sources[0].Capabilities != "" {
		t.Fatalf("identity fields = %q/%q, want unset until Step 21",
			hw.Sources[0].IdentityNamespace, hw.Sources[0].Capabilities)
	}
	// Retention states policy, never a per-profile boundary: the
	// report horizon mirrors the configured retention and the subject
	// horizon appears once the server wires it.
	if hw.Retention == nil || hw.Retention.ReportsSeconds != int64((30*24*time.Hour)/time.Second) {
		t.Fatalf("retention = %+v, want configured report horizon", hw.Retention)
	}
	if hw.Retention.SubjectsSeconds != nil {
		t.Fatalf("subject horizon = %v, want absent until wired", *hw.Retention.SubjectsSeconds)
	}
	v.Overview.SetSubjectRetention(90 * 24 * time.Hour)
	rec = httptest.NewRecorder()
	v.handleHealth(rec, httptest.NewRequest("GET", "/api/health", nil))
	var hw2 healthWire
	if err := json.NewDecoder(rec.Body).Decode(&hw2); err != nil {
		t.Fatal(err)
	}
	if hw2.Retention == nil || hw2.Retention.SubjectsSeconds == nil ||
		*hw2.Retention.SubjectsSeconds != int64((90*24*time.Hour)/time.Second) {
		t.Fatalf("wired retention = %+v, want both horizons", hw2.Retention)
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
	now := time.Now().UTC().Truncate(time.Second)
	// Retained load: 200 briefings across priorities, so the number
	// reflects a populated window rather than a single row.
	for i := 0; i < 200; i++ {
		pri := "low"
		switch i % 10 {
		case 0:
			pri = "high"
		case 1:
			pri = "medium"
		}
		seedPriorityBriefing(b, s, "host:bench", fmt.Sprintf("evt-bench-%d", i), pri, now.Add(-time.Duration(i)*time.Minute))
	}
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

// One high briefing followed by dozens of low briefings still reads
// act: the verdict counts the whole window, and the attention list
// leads with priority even though the high row is the oldest.
func TestOverviewWindowWideVerdict(t *testing.T) {
	s := openViewerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	seedPriorityBriefing(t, s, "host:wide-high", "evt-wide-high", "high", now.Add(-time.Hour))
	for i := 0; i < 59; i++ {
		seedPriorityBriefing(t, s, "host:wide-low", fmt.Sprintf("evt-wide-low-%d", i),
			"low", now.Add(-30*time.Minute))
	}
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
	if wire.Verdict != "act" || wire.Headline != "1 need a check now" {
		t.Fatalf("verdict = %q headline = %q, want act over the full window", wire.Verdict, wire.Headline)
	}
	if len(wire.Attention) == 0 || wire.Attention[0].Priority != "high" {
		t.Fatalf("attention does not lead with priority: %+v", wire.Attention)
	}
	if wire.Receipts != 60 {
		t.Fatalf("receipts = %d, want the full window", wire.Receipts)
	}
	if wire.Liveness != "unknown" {
		t.Fatalf("liveness = %q, want unknown until H1", wire.Liveness)
	}
}

// A trigger throttled by the 1/s coalescing still refreshes when the
// interval expires: the test tick is an hour, so only the delayed
// refresh can deliver the new briefing.
func TestRefresherDelayedRefresh(t *testing.T) {
	s := openViewerStore(t)
	r := NewRefresher(s, health.New(), nil, 24*time.Hour, 168*time.Hour, 30*24*time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan []byte, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx, time.Hour, func(body []byte) {
			changed <- body
		})
	}()
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("no initial overview")
	}
	seedBriefing(t, s, "host:delay", "evt-delay")
	r.Trigger()
	select {
	case body := <-changed:
		if !strings.Contains(string(body), `"verdict":"act"`) {
			t.Fatalf("delayed refresh missed the briefing: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("throttled trigger never refreshed")
	}
	cancel()
	<-done
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
