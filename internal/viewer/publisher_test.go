package viewer

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

// stubTripper answers without the network: httptest servers need a port
// bind, which sandboxes forbid. Full socket runs happen outside.
type stubTripper struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	respond  func(r *http.Request) (*http.Response, error)
}

func (s *stubTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, r)
	s.bodies = append(s.bodies, body)
	s.mu.Unlock()
	return s.respond(r)
}

func okResp() *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}
}

func testBriefing() store.Briefing {
	sug := "high"
	return store.Briefing{
		BriefingID: "brf-g1", GroupID: "g1", HostAnchorKey: "host:w",
		Baseline: "medium", ModelSuggestion: &sug, Priority: "high",
		PriorityReason: "r", BaselineRule: "R1", Clamped: true,
		WhatChanged: "w", Context: "c", RecommendedCheck: "k",
		CreatedAt: time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC),
		EventIDs:  []string{"e1", "e2"},
	}
}

func runPublisher(t *testing.T, stub *stubTripper, bearer, apiKey string) (*Publisher, func()) {
	t.Helper()
	p := NewPublisher("http://127.0.0.1:8080/api/annotations", bearer, apiKey, "",
		2*time.Second, &http.Client{Transport: stub, CheckRedirect: noRedirectClient})
	p.retryBase = time.Millisecond
	p.Start()
	return p, p.Stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPublisherPostsAnnotationContract(t *testing.T) {
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) { return okResp(), nil }}
	p, stop := runPublisher(t, stub, "tok-123", "")
	defer stop()
	p.Enqueue(testBriefing())
	waitFor(t, "post", func() bool { return p.Posted() == 1 })
	if len(stub.requests) != 1 {
		t.Fatalf("requests = %d", len(stub.requests))
	}
	req := stub.requests[0]
	if got := req.Header.Get("Authorization"); got != "Bearer tok-123" {
		t.Fatalf("auth = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(stub.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"briefing_id", "event_ids", "host_id", "baseline",
		"model_suggestion", "priority", "priority_reason", "clamped",
		"what_changed", "context", "recommended_check", "created_at"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("annotation missing %q: %v", key, body)
		}
	}
	if body["briefing_id"] != "brf-g1" || body["priority"] != "high" {
		t.Fatalf("annotation = %v", body)
	}
	if _, err := time.Parse(time.RFC3339, body["created_at"].(string)); err != nil {
		t.Fatalf("created_at not RFC3339: %v", body["created_at"])
	}
}

func TestPublisherAPIKeyFallback(t *testing.T) {
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) { return okResp(), nil }}
	p, stop := runPublisher(t, stub, "", "key-9")
	defer stop()
	p.Enqueue(testBriefing())
	waitFor(t, "post", func() bool { return p.Posted() == 1 })
	if got := stub.requests[0].Header.Get("X-API-Key"); got != "key-9" {
		t.Fatalf("api key header = %q", got)
	}
	if got := stub.requests[0].Header.Get("Authorization"); got != "" {
		t.Fatalf("bearer must win over key, got %q", got)
	}
}

func TestPublisherSoftFailureMatrix(t *testing.T) {
	for name, respond := range map[string]func(*http.Request) (*http.Response, error){
		"404": func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		},
		"500": func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		},
		"refused": func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") },
		"redirect": func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://evil.example/x"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	} {
		stub := &stubTripper{respond: respond}
		p, stop := runPublisher(t, stub, "", "")
		p.Enqueue(testBriefing())
		waitFor(t, name+" give-up", func() bool { return p.Failed() == 1 })
		stop()
		if p.Posted() != 0 {
			t.Fatalf("%s: must never count a failure as posted", name)
		}
		// 3 attempts max, then give up: bounded, never an outage backlog.
		// Redirects must not be followed: same bounded count.
		if n := len(stub.requests); n != maxPostAttempts {
			t.Fatalf("%s: attempts = %d, want %d", name, n, maxPostAttempts)
		}
	}
}

func TestPublisherFullQueueDrops(t *testing.T) {
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) { return okResp(), nil }}
	p := NewPublisher("http://127.0.0.1:8080/api/annotations", "", "", "", time.Second,
		&http.Client{Transport: stub, CheckRedirect: noRedirectClient})
	// Not started: the queue fills, the overflow drops without blocking.
	for i := 0; i < publisherQueueCap+1; i++ {
		p.Enqueue(testBriefing())
	}
	if got := p.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
}

func TestPublisherTimeoutIsSoft(t *testing.T) {
	stub := &stubTripper{respond: func(r *http.Request) (*http.Response, error) {
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(5 * time.Second):
			return okResp(), nil
		}
	}}
	p := NewPublisher("http://127.0.0.1:8080/api/annotations", "", "", "", 50*time.Millisecond,
		&http.Client{Transport: stub, CheckRedirect: noRedirectClient, Timeout: 50 * time.Millisecond})
	p.retryBase = time.Millisecond
	p.Start()
	defer p.Stop()
	p.Enqueue(testBriefing())
	waitFor(t, "timeout give-up", func() bool { return p.Failed() == 1 })
	if p.Posted() != 0 {
		t.Fatal("hung ScanRelay must not count as posted")
	}
}

func TestFanoutNeverBlocksOnFullQueue(t *testing.T) {
	h := NewHub()
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) { return okResp(), nil }}
	p := NewPublisher("http://127.0.0.1:8080/api/annotations", "", "", "", time.Second,
		&http.Client{Transport: stub, CheckRedirect: noRedirectClient})
	// Not started and pre-filled: the worker path must still return.
	for i := 0; i < publisherQueueCap; i++ {
		p.Enqueue(testBriefing())
	}
	onBriefing, onGroups := Fanout(h, p)
	done := make(chan struct{})
	go func() {
		defer close(done)
		onBriefing(testBriefing())
		onGroups(nil)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("observer fan-out blocked the worker path")
	}
	if p.Dropped() == 0 {
		t.Fatal("overflow must be counted, not silent")
	}
}

func TestFanoutToPublisherAndHub(t *testing.T) {
	h := NewHub()
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) { return okResp(), nil }}
	p, stop := runPublisher(t, stub, "", "")
	defer stop()
	onBriefing, _ := Fanout(h, p)
	ch, unsub := h.Subscribe(8)
	defer unsub()
	var wg sync.WaitGroup
	wg.Add(1)
	var got atomic.Value
	go func() {
		defer wg.Done()
		select {
		case ev := <-ch.C:
			got.Store(ev)
		case <-time.After(3 * time.Second):
		}
	}()
	onBriefing(testBriefing())
	wg.Wait()
	ev, ok := got.Load().(Event)
	if !ok || ev.Name != "briefing" {
		t.Fatalf("hub did not receive briefing: %+v", got.Load())
	}
	waitFor(t, "fanout post", func() bool { return p.Posted() == 1 })
}
