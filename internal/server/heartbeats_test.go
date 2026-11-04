package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/config"
	"github.com/ostefani/watcherbridge/internal/store"
)

func h1Body(id string, at time.Time) map[string]any {
	return map[string]any{"event_id": id, "event_key": "heartbeat:" + id, "event_type": "scan.heartbeat", "occurred_at": at,
		"cycle": 1, "target": "192.0.2.0/24", "scan_id": id, "completed_at": at, "hosts_seen": 0, "duration_ms": 1000, "status": "ok", "issue_count": 0}
}

func (h *harness) postH1(t *testing.T, body []byte, id string, signedAt time.Time) int {
	t.Helper()
	key := "heartbeat:" + id
	return h.post(t, body, id, key, "scan.heartbeat", signedAt,
		sign(h.secret, strconv.FormatInt(signedAt.Unix(), 10), id, key, "scan.heartbeat", body)).Code
}

func TestHeartbeatIngestExcludesHostPipeline(t *testing.T) {
	for _, mode := range []string{config.ModeTap, config.ModeProxy} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, func(c *config.Config) { c.Mode = mode })
			now := time.Now().UTC().Truncate(time.Second)
			const id = "0123456789abcdef0123456789abcdef"
			body, _ := json.Marshal(h1Body(id, now))
			var wg sync.WaitGroup
			codes := make(chan int, 12)
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); codes <- h.postH1(t, body, id, now) }()
			}
			wg.Wait()
			close(codes)
			for code := range codes {
				if code != http.StatusAccepted {
					t.Fatalf("concurrent retry status: %d", code)
				}
			}
			h.srv.SweepOnce(context.Background())
			if h.relay.hits.Load() != 0 {
				t.Fatal("heartbeat forwarded to ScanRelay")
			}
			if count, err := h.srv.events.ReceiptCount(context.Background(), time.Unix(0, 0)); err != nil || count != 0 {
				t.Fatalf("raw receipts: %d %v", count, err)
			}
			if groups, err := h.srv.events.OpenGroupSnapshots(context.Background()); err != nil || len(groups) != 0 {
				t.Fatalf("heartbeat grouped: %+v %v", groups, err)
			}
			if rows, err := h.srv.events.SourceHealthRows(context.Background()); err != nil || len(rows) != 1 || rows[0].Heartbeat == nil {
				t.Fatalf("source summary: %+v %v", rows, err)
			}
			if code := h.postH1(t, append(append([]byte(nil), body...), ' '), id, now); code != http.StatusConflict {
				t.Fatalf("conflicting retry: %d", code)
			}
		})
	}
}

func TestHeartbeatValidation(t *testing.T) {
	h := newHarness(t, nil)
	now := time.Now().UTC().Truncate(time.Second)
	const id = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"spoofed source", func(m map[string]any) { m["source_scope"] = "another-source" }},
		{"identity claim", func(m map[string]any) { m["capabilities"] = "stable-identity-v1" }},
		{"header mismatch", func(m map[string]any) { m["event_id"] = "other" }},
		{"scan mismatch", func(m map[string]any) { m["scan_id"] = "other" }},
		{"negative count", func(m map[string]any) { m["hosts_seen"] = -1 }},
		{"missing count", func(m map[string]any) { delete(m, "hosts_seen") }},
		{"case alias conceals missing duration", func(m map[string]any) { delete(m, "duration_ms"); m["HOSTS_SEEN"] = 0 }},
		{"null count", func(m map[string]any) { m["hosts_seen"] = nil }},
		{"fractional duration", func(m map[string]any) { m["duration_ms"] = 0.5 }},
		{"overflow duration", func(m map[string]any) { m["duration_ms"] = int64(1<<63 - 1) }},
		{"unknown status", func(m map[string]any) { m["status"] = "healthy" }},
		{"hidden errors", func(m map[string]any) { m["issue_count"] = 1 }},
		{"unexplained partial", func(m map[string]any) { m["status"] = "partial" }},
		{"future completion", func(m map[string]any) {
			m["completed_at"] = now.Add(time.Minute)
			m["occurred_at"] = now.Add(time.Minute)
		}},
		{"unrepresentable time", func(m map[string]any) {
			m["completed_at"] = "9999-01-01T00:00:00Z"
			m["occurred_at"] = m["completed_at"]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := h1Body(id, now)
			tc.change(m)
			body, _ := json.Marshal(m)
			if code := h.postH1(t, body, id, now); code != 400 {
				t.Fatalf("got %d", code)
			}
		})
	}
	body, _ := json.Marshal(h1Body(id, now))
	duplicate := []byte(strings.TrimSuffix(string(body), "}") + `,"hosts_seen":0}`)
	for _, bad := range [][]byte{duplicate, append(append([]byte(nil), body...), []byte(` {}`)...)} {
		if code := h.postH1(t, bad, id, now); code != 400 {
			t.Fatalf("ambiguous JSON: %d", code)
		}
	}
	if code := h.postH1(t, []byte(strings.Repeat("x", store.MaxHeartbeatBytes+1)), id, now); code != 413 {
		t.Fatalf("oversize: %d", code)
	}
	key := "heartbeat:" + id
	if code := h.post(t, body, id, key, "scan.heartbeat", now, "sha256=bad").Code; code != 401 {
		t.Fatalf("signature: %d", code)
	}
	if code := h.postH1(t, body, id, now.Add(-time.Hour)); code != 401 {
		t.Fatalf("stale signing timestamp: %d", code)
	}
	for i, status := range []string{"partial", "error"} {
		id := fmt.Sprintf("%032x", i+1)
		m := h1Body(id, now)
		m["status"], m["issue_count"] = status, 1
		body, _ := json.Marshal(m)
		if code := h.postH1(t, body, id, now); code != 202 {
			t.Fatalf("%s: %d", status, code)
		}
	}
}

func TestHeartbeatBurstAndUnavailableStore(t *testing.T) {
	for _, mode := range []string{config.ModeTap, config.ModeProxy} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, func(c *config.Config) { c.Mode = mode })
			now := time.Now().UTC().Truncate(time.Second)
			for i := 1; i <= 512; i++ {
				id := fmt.Sprintf("%032x", i)
				body, _ := json.Marshal(h1Body(id, now.Add(time.Duration(i))))
				if code := h.postH1(t, body, id, now); code != 202 {
					t.Fatalf("burst request %d: %d", i, code)
				}
			}
			if h.relay.hits.Load() != 0 {
				t.Fatal("burst forwarded H1")
			}
			if err := h.srv.events.Close(); err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprintf("%032x", 513)
			body, _ := json.Marshal(h1Body(id, now))
			if code := h.postH1(t, body, id, now); code != 503 {
				t.Fatalf("unavailable store: %d", code)
			}
		})
	}
}

func TestHeartbeatBodyLimitBoundary(t *testing.T) {
	h := newHarness(t, nil)
	now := time.Now().UTC().Truncate(time.Second)
	for i, size := range []int{store.MaxHeartbeatBytes - 1, store.MaxHeartbeatBytes, store.MaxHeartbeatBytes + 1} {
		id := fmt.Sprintf("%032x", i+1)
		body, _ := json.Marshal(h1Body(id, now))
		body = append(body, []byte(strings.Repeat(" ", size-len(body)))...)
		want := 202
		if size > store.MaxHeartbeatBytes {
			want = 413
		}
		if code := h.postH1(t, body, id, now); code != want {
			t.Fatalf("%d-byte H1: %d, want %d", size, code, want)
		}
	}
}

func TestHeartbeatBypassesHungRelay(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.InFlightCap = 2 })
	h.relay.mode.Store("hang")
	done := make(chan int, 1)
	now := time.Now().UTC().Truncate(time.Second)
	go func() { done <- h.signedPost(t, "blocked-alert", "host-a", "host.appeared", now).Code }()
	select {
	case <-h.relay.entered:
	case <-time.After(time.Second):
		t.Fatal("relay did not block")
	}
	id := fmt.Sprintf("%032x", 1)
	body, _ := json.Marshal(h1Body(id, now))
	code := h.postH1(t, body, id, now)
	close(h.relay.release)
	if got := <-done; got != 202 {
		t.Fatalf("alert did not complete: %d", got)
	}
	if code != 202 || h.relay.hits.Load() != 1 {
		t.Fatalf("H1 touched blocked relay: code=%d hits=%d", code, h.relay.hits.Load())
	}
}

func FuzzDecodeHeartbeat(f *testing.F) {
	now := time.Unix(1800000000, 0).UTC()
	const id = "0123456789abcdef0123456789abcdef"
	body, _ := json.Marshal(h1Body(id, now))
	f.Add(body)
	f.Add([]byte(`{"hosts_seen":null}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) <= store.MaxHeartbeatBytes {
			_, _ = decodeHeartbeat(body, id, "heartbeat:"+id, now, now, "lab")
		}
	})
}
