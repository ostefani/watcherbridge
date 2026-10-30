package viewer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

func openViewerStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/viewer.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedOpenGroup(t *testing.T, s *store.Store, anchor, eventID string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	_, err := s.PersistEventAndGroup(context.Background(), store.RawEvent{
		EventID: eventID, EventKey: "k", EventType: "host.appeared",
		OccurredAt: now, Payload: []byte(`{"event_id":"e"}`), ReceivedAt: now, SignatureTimestamp: now,
	}, anchor, store.GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
}

func seedBriefing(t *testing.T, s *store.Store, anchor, eventID string) string {
	t.Helper()
	seedOpenGroup(t, s, anchor, eventID)
	groups, err := s.OpenGroupSnapshots(context.Background())
	if err != nil || len(groups) == 0 {
		t.Fatalf("seed group missing: %v", groups)
	}
	gid := groups[len(groups)-1].GroupID
	// Advance past the quiet window so TakeGroup accepts the group.
	if _, err := s.CloseDue(context.Background(), time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(context.Background(), store.Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: anchor,
		Baseline: "medium", Priority: "high", PriorityReason: "r", Clamped: true,
		FallbackUsed: false, WhatChanged: "w", Context: "c", RecommendedCheck: "k",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return taken.GroupID
}

func TestHubBroadcastAndSlowSkip(t *testing.T) {
	h := NewHub()
	fast, unsubFast := h.Subscribe(8)
	defer unsubFast()
	slow, unsubSlow := h.Subscribe(1)
	defer unsubSlow()
	if got := h.SubscriberCount(); got != 2 {
		t.Fatalf("subscribers = %d", got)
	}
	for i := 0; i < 5; i++ {
		h.Broadcast("briefing", map[string]string{"n": "v"})
	}
	n := 0
	for {
		select {
		case ev := <-fast.C:
			if ev.Name != "briefing" {
				t.Fatalf("event name = %q", ev.Name)
			}
			n++
		default:
			goto drained
		}
	}
drained:
	if n != 5 {
		t.Fatalf("fast subscriber got %d/5", n)
	}
	if d := h.Dropped(); d == 0 {
		t.Fatal("slow subscriber must be skipped, never waited on")
	}
	if !slow.Lagged() {
		t.Fatal("skipped subscriber must be flagged lagged for resync")
	}
	if !slow.ConsumeLagged() || slow.Lagged() {
		t.Fatal("ConsumeLagged must return and clear the flag")
	}
	unsubSlow()
	// One slot taken (fast): max 2 admits one more, then refuses, and the
	// check-and-insert holds the hub lock so the cap cannot be raced past.
	extra, unsubExtra, ok := h.TrySubscribe(8, 2)
	if !ok {
		t.Fatal("one free slot must admit a subscriber")
	}
	_ = extra
	if _, _, ok := h.TrySubscribe(8, 2); ok {
		t.Fatal("a full hub must refuse, so the handler can answer 503")
	}
	unsubExtra()
	if got := h.SubscriberCount(); got != 1 {
		t.Fatalf("unsubscribe leaking: %d", got)
	}
}

// syncRecorder guards httptest's Recorder for the handler goroutine:
// the test polls the body while the stream writes.
type syncRecorder struct {
	mu  sync.Mutex
	rec *httptest.ResponseRecorder
}

func (s *syncRecorder) Header() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Header()
}

func (s *syncRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Write(b)
}

func (s *syncRecorder) WriteHeader(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.WriteHeader(status)
}

func (s *syncRecorder) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Flush()
}

func (s *syncRecorder) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Body.String()
}

func TestSSEOnlyEmitsBridgeNames(t *testing.T) {
	s := openViewerStore(t)
	seedOpenGroup(t, s, "host:sse", "evt-sse")
	v := New(s, NewHub())
	req := httptest.NewRequest("GET", "/events", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := &syncRecorder{rec: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() { defer close(done); v.handleEvents(rec, req) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if strings.Contains(rec.body(), "event: group") || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	body := rec.body()
	if !strings.Contains(body, "event: group") {
		t.Fatalf("no group snapshot on connect: %q", body)
	}
	for _, line := range strings.Split(body, "\n") {
		// The bridge stream carries briefing, group, overview and resync;
		// annotation belongs to ScanRelay's stream and must never appear.
		if strings.HasPrefix(line, "event: ") && line != "event: group" && line != "event: briefing" &&
			line != "event: overview" && line != "event: resync" {
			t.Fatalf("foreign event name on bridge stream: %q", line)
		}
	}
	if ct := rec.rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
}

func TestBriefingEndpointsShapeAndHeaders(t *testing.T) {
	s := openViewerStore(t)
	gid := seedBriefing(t, s, "host:api", "evt-api")
	v := New(s, NewHub())

	serve := func(h func(http.ResponseWriter, *http.Request), path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	rec := serve(v.handleBriefings, "/api/briefings?limit=10")
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("briefings content type = %q", ct)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != csp {
		t.Fatalf("briefings CSP = %q, want %q", got, csp)
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("briefings = %q, err %v", rec.Body.String(), err)
	}
	for _, forbidden := range []string{"model_raw_json", "ModelRawJSON"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("audit trail leaked into API: %s", forbidden)
		}
	}
	if rows[0]["host"] != "host:api" || rows[0]["priority"] != "high" {
		t.Fatalf("row = %v", rows[0])
	}

	rec = serve(v.handleBriefing, "/api/briefing?id="+gid)
	var one map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
		t.Fatalf("single briefing: %v", err)
	}
	if one["group_id"] != gid {
		t.Fatalf("wrong briefing: %v", one)
	}
	if rec := serve(v.handleBriefing, "/api/briefing"); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing id = %d", rec.Code)
	}

	rec = serve(v.handleGroups, "/api/groups")
	var groups []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
		t.Fatalf("groups: %v", err)
	}
	_ = groups
}

func TestBriefingsCursorPaging(t *testing.T) {
	s := openViewerStore(t)
	seedBriefing(t, s, "host:pg1", "evt-pg1")
	seedBriefing(t, s, "host:pg2", "evt-pg2")
	v := New(s, NewHub())
	serve := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		v.handleBriefings(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	decode := func(rec *httptest.ResponseRecorder) []map[string]any {
		t.Helper()
		var rows []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatalf("briefings = %q, err %v", rec.Body.String(), err)
		}
		return rows
	}
	// Default: latest page, ascending seq.
	rows := decode(serve("/api/briefings"))
	if len(rows) != 2 || rows[0]["seq"] != 1.0 || rows[1]["seq"] != 2.0 {
		t.Fatalf("default page = %v", rows)
	}
	// Gap-fill newer than seq 1.
	rows = decode(serve("/api/briefings?after=1"))
	if len(rows) != 1 || rows[0]["seq"] != 2.0 {
		t.Fatalf("after=1 = %v", rows)
	}
	// Older history before seq 2.
	rows = decode(serve("/api/briefings?before=2&limit=10"))
	if len(rows) != 1 || rows[0]["seq"] != 1.0 {
		t.Fatalf("before=2 = %v", rows)
	}
	// Garbage, negative and out-of-range params are 400, never silent.
	for _, path := range []string{
		"/api/briefings?after=12abc",
		"/api/briefings?after=-1",
		"/api/briefings?before=1.5",
		"/api/briefings?limit=12abc",
		"/api/briefings?limit=0",
		"/api/briefings?limit=501",
	} {
		if rec := serve(path); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", path, rec.Code)
		}
	}
}

func TestStaticAssetsHeaders(t *testing.T) {
	v := New(openViewerStore(t), NewHub())
	for path, ct := range map[string]string{
		"/":          "text/html; charset=utf-8",
		"/app.js":    "text/javascript; charset=utf-8",
		"/style.css": "text/css; charset=utf-8",
	} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		switch path {
		case "/":
			v.handleIndex(rec, req)
		case "/app.js":
			v.serveAsset("web/app.js", "text/javascript; charset=utf-8")(rec, req)
		case "/style.css":
			v.serveAsset("web/style.css", "text/css; charset=utf-8")(rec, req)
		}
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != ct {
			t.Fatalf("%s content type = %q", path, got)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != csp {
			t.Fatalf("%s CSP = %q", path, got)
		}
	}
}

func TestShippedJSRendersTextOnly(t *testing.T) {
	raw, err := assets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	if strings.Contains(js, "innerHTML") {
		t.Fatal("viewer JS must not use innerHTML: hostile strings render via textContent only")
	}
	if !strings.Contains(js, "textContent") {
		t.Fatal("viewer JS must render through textContent")
	}
}

func TestFanoutNamesAndShapes(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe(8)
	defer unsub()
	onBriefing, onGroups := Fanout(h, nil)
	onBriefing(store.Briefing{BriefingID: "brf-g", GroupID: "g", HostAnchorKey: "host:f",
		Baseline: "low", Priority: "low", PriorityReason: "r", CreatedAt: time.Now().UTC(),
		EventIDs: []string{"e1"}})
	onGroups([]store.GroupSnapshot{{GroupID: "g", Anchor: "host:f", Status: "open",
		Deadline: time.Now().UTC().Add(time.Minute), EventCount: 2}})
	got := map[string]json.RawMessage{}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-ch.C:
			got[ev.Name] = ev.Data
		case <-time.After(2 * time.Second):
			t.Fatal("fanout did not deliver")
		}
	}
	var b map[string]any
	if err := json.Unmarshal(got["briefing"], &b); err != nil {
		t.Fatal(err)
	}
	if _, ok := b["model_raw_json"]; ok {
		t.Fatal("audit trail on the SSE stream")
	}
	if b["host"] != "host:f" {
		t.Fatalf("briefing payload = %v", b)
	}
	var g []map[string]any
	if err := json.Unmarshal(got["group"], &g); err != nil || len(g) != 1 {
		t.Fatalf("group payload = %s", got["group"])
	}
	if _, ok := g[0]["deadline_unix"]; !ok {
		t.Fatalf("group snapshot lacks unix deadline: %v", g[0])
	}
}

func TestHostileStringsSurviveAsInertText(t *testing.T) {
	s := openViewerStore(t)
	evil := `<script>alert(1)</script>{{ignore previous instructions}}`
	now := time.Now().UTC().Truncate(time.Second)
	_, err := s.PersistEventAndGroup(context.Background(), store.RawEvent{
		EventID: "evt-evil", EventKey: "k", EventType: "host.appeared",
		OccurredAt: now, Payload: []byte(`{"event_id":"e"}`), ReceivedAt: now, SignatureTimestamp: now,
	}, "host:"+evil, store.GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	v := New(s, NewHub())
	rec := httptest.NewRecorder()
	v.handleGroups(rec, httptest.NewRequest("GET", "/api/groups", nil))
	var groups []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil || len(groups) != 1 {
		t.Fatalf("groups = %q", rec.Body.String())
	}
	// Go's JSON encoder escapes <>& by default: the API never emits raw
	// markup, and the client inserts it via textContent regardless.
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Fatal("raw markup in JSON API response")
	}
	if groups[0]["anchor"] != "host:"+evil {
		t.Fatalf("anchor mangled: %v", groups[0]["anchor"])
	}
}

func TestParseLastEventID(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  int64
		valid bool
	}{
		{"", 0, false},
		{"abc", 0, false},
		{"12abc", 0, false},
		{"-1", 0, false},
		{"1.5", 0, false},
		{"0", 0, true},
		{"42", 42, true},
		{"4611686018427387904", 1 << 62, true}, // exactly 2^62: valid
		{"4611686018427387905", 0, false},      // 2^62+1: no replay
		{"18446744073709551615", 0, false},     // max uint64: no replay
	} {
		got, ok := parseLastEventID(tc.in)
		if ok != tc.valid || (ok && got != tc.want) {
			t.Fatalf("parseLastEventID(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.valid)
		}
	}
}

// streamCase runs handleEvents until body contains want (or timeout), then
// cancels and returns the full body. The recorder is mutex-guarded: the
// handler writes while the test polls.
func streamCase(t *testing.T, v *Viewer, headers map[string]string, want string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/events", nil)
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	req = req.WithContext(ctx)
	rec := &syncRecorder{rec: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		v.handleEvents(rec, req)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if strings.Contains(rec.body(), want) {
			cancel()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("stream never contained %q; body:\n%s", want, rec.body())
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-done
	return rec.body()
}

func TestHandleEventsReplay(t *testing.T) {
	s := openViewerStore(t)
	seedBriefing(t, s, "host:re1", "evt-re1")
	seedBriefing(t, s, "host:re2", "evt-re2")
	v := New(s, NewHub())

	// Valid id replays newer briefings with id: lines, ascending.
	body := streamCase(t, v, map[string]string{"Last-Event-ID": "1"}, "id: 2")
	if strings.Count(body, "event: briefing") != 1 {
		t.Fatalf("want exactly the seq-2 replay, body:\n%s", body)
	}
	if !strings.Contains(body, "event: group") || !strings.Contains(body, "event: overview") {
		t.Fatalf("connect must send snapshot and overview first, body:\n%s", body)
	}

	// Garbage replays nothing: snapshot and overview only, no id: lines.
	body = streamCase(t, v, map[string]string{"Last-Event-ID": "12abc"}, "event: overview")
	if strings.Contains(body, "id: ") || strings.Contains(body, "event: briefing") {
		t.Fatalf("garbage Last-Event-ID must replay nothing, body:\n%s", body)
	}

	// Future id (restored database) resyncs with the current maximum.
	body = streamCase(t, v, map[string]string{"Last-Event-ID": "9999"}, "event: resync")
	if strings.Contains(body, "event: briefing") {
		t.Fatalf("future Last-Event-ID must not replay, body:\n%s", body)
	}
	if !strings.Contains(body, `"max_seq":2`) {
		t.Fatalf("resync must carry max_seq 2, body:\n%s", body)
	}

	// Over the replay cap: resync, never a partial replay.
	v.ReplayMax = 1
	body = streamCase(t, v, map[string]string{"Last-Event-ID": "0"}, "event: resync")
	if strings.Contains(body, "event: briefing") {
		t.Fatalf("over-cap replay must send resync only, body:\n%s", body)
	}
}

// deadlineRecorder records every SetWriteDeadline the stream sets, so the
// test proves each write carries a bound (a stalled peer fails fast).
type deadlineRecorder struct {
	*syncRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadlines = append(d.deadlines, t)
	return nil
}

func TestHandleEventsBoundsEveryWrite(t *testing.T) {
	s := openViewerStore(t)
	v := New(s, NewHub())
	v.WriteTimeout = 5 * time.Second
	start := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/events", nil).WithContext(ctx)
	rec := &deadlineRecorder{syncRecorder: &syncRecorder{rec: httptest.NewRecorder()}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		v.handleEvents(rec, req)
	}()
	deadline := start.Add(3 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.deadlines)
		rec.mu.Unlock()
		// Connect sequence alone performs two writes (group, overview).
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.deadlines) < 2 {
		t.Fatalf("only %d writes carried a deadline", len(rec.deadlines))
	}
	for _, d := range rec.deadlines {
		if d.IsZero() || d.Before(start) || d.After(start.Add(10*time.Second)) {
			t.Fatalf("unbounded stream write, deadlines = %v", rec.deadlines)
		}
	}
}

// A refused subscription answers 503 without touching the store: the
// ingest path never queues behind stream demand.
func TestHandleEventsSubscriberCap(t *testing.T) {
	s := openViewerStore(t)
	v := New(s, NewHub())
	v.MaxSubscribers = 1
	_, unsub := v.hub.Subscribe(8)
	defer unsub()
	rec := httptest.NewRecorder()
	v.handleEvents(rec, httptest.NewRequest("GET", "/events", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("over-cap subscriber = %d, want 503", rec.Code)
	}
}

// A replay-path store failure resyncs instead of streaming on silently:
// the client must refetch, never advance its cursor past missed history.
func TestHandleEventsReplayErrorResyncs(t *testing.T) {
	s := openViewerStore(t)
	seedBriefing(t, s, "host:rx", "evt-rx")
	s.Close()
	v := New(s, NewHub())
	body := streamCase(t, v, map[string]string{"Last-Event-ID": "0"}, "event: resync")
	if strings.Contains(body, "event: briefing") {
		t.Fatalf("failed replay must not emit briefings, body:\n%s", body)
	}
}

func TestHandleEventsHeartbeat(t *testing.T) {
	s := openViewerStore(t)
	v := New(s, NewHub())
	v.Heartbeat = 10 * time.Millisecond
	body := streamCase(t, v, nil, ": ping")
	if strings.Count(body, ": ping") < 1 {
		t.Fatalf("no heartbeat comments, body:\n%s", body)
	}
}

func TestViewerCloseUnblocksStream(t *testing.T) {
	s := openViewerStore(t)
	v := New(s, NewHub())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/events", nil).WithContext(ctx)
	rec := &syncRecorder{rec: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		v.handleEvents(rec, req)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if strings.Contains(rec.body(), "event: overview") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream never sent the connect sequence")
		}
		time.Sleep(5 * time.Millisecond)
	}
	v.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock the stream handler")
	}
	v.Close() // idempotent: shutdown paths may race
}

func TestLaggedSubscriberGetsResync(t *testing.T) {
	s := openViewerStore(t)
	v := New(s, NewHub())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/events", nil).WithContext(ctx)
	rec := &syncRecorder{rec: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		v.handleEvents(rec, req)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if strings.Contains(rec.body(), "event: overview") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream never sent the connect sequence")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// White-box: flag the handler's subscription lagged, then deliver one
	// live event. The loop must answer resync, never a partial replay.
	v.hub.mu.Lock()
	for sub := range v.hub.subs {
		sub.lagged.Store(true)
	}
	v.hub.mu.Unlock()
	v.hub.Broadcast("group", map[string]string{"note": "live"})
	deadline = time.Now().Add(3 * time.Second)
	for {
		if strings.Contains(rec.body(), "event: resync") {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("lagged subscriber got no resync, body:\n%s", rec.body())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// A closed viewer replays nothing and returns promptly: snapshot and
// replay reads inherit a context cancelled by Close, and no new write
// starts after it — the handler may end with zero bytes written.
func TestReplayObservesClose(t *testing.T) {
	s := openViewerStore(t)
	seedBriefing(t, s, "host:rc1", "evt-rc1")
	seedBriefing(t, s, "host:rc2", "evt-rc2")
	v := New(s, NewHub())
	v.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/events", nil)
	req.Header.Set("Last-Event-ID", "0")
	req = req.WithContext(ctx)
	rec := &syncRecorder{rec: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		v.handleEvents(rec, req)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("closed viewer did not end the stream")
	}
	if strings.Contains(rec.body(), "event: briefing") {
		t.Fatalf("closed viewer must not replay, body:\n%s", rec.body())
	}
}

// gateWriter blocks one numbered stream write until released, so the test
// can close the viewer mid-replay: the handler must abort promptly
// instead of emitting the remaining rows.
type gateWriter struct {
	*syncRecorder
	mu      sync.Mutex
	n       int
	blockOn int
	release chan struct{}
	started chan struct{}
}

func (g *gateWriter) Write(b []byte) (int, error) {
	g.mu.Lock()
	g.n++
	fire := g.n == g.blockOn
	g.mu.Unlock()
	if fire {
		close(g.started)
		<-g.release
	}
	return g.syncRecorder.Write(b)
}

func TestReplayAbortsOnMidReplayClose(t *testing.T) {
	s := openViewerStore(t)
	seedBriefing(t, s, "host:mc1", "evt-mc1")
	seedBriefing(t, s, "host:mc2", "evt-mc2")
	v := New(s, NewHub())
	v.ReplayMax = 100
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/events", nil)
	req.Header.Set("Last-Event-ID", "0")
	req = req.WithContext(ctx)
	rec := &gateWriter{syncRecorder: &syncRecorder{rec: httptest.NewRecorder()},
		blockOn: 3, release: make(chan struct{}), started: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		v.handleEvents(rec, req)
	}()
	select {
	case <-rec.started:
	case <-time.After(3 * time.Second):
		t.Fatal("replay never reached the first row")
	}
	v.Close()
	close(rec.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("mid-replay Close did not abort the handler")
	}
	// Writes 1-2 are the snapshot and overview; the blocked replay row
	// was in flight. Nothing after it may be emitted once closed.
	if n := strings.Count(rec.body(), "event: briefing"); n != 1 {
		t.Fatalf("replay continued after Close: %d briefing events", n)
	}
}

// withHost returns the viewer with an allowlist for tests, plus the
// handler the browser would talk to: mounted exactly like the server
// mounts it, so mux-generated responses go through the same gate.
func withHost(t *testing.T, s *store.Store, hosts ...string) (*Viewer, http.Handler) {
	t.Helper()
	v := New(s, NewHub())
	v.AllowedHosts = hosts
	mux := http.NewServeMux()
	mux.Handle("/", v.Handler())
	return v, mux
}

func serveMux(mux http.Handler, method, target, host string, headers map[string]string, body string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Host = host
	req.URL.Host = host
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func assertViewerHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if got := h.Get("Content-Security-Policy"); got != csp {
		t.Fatalf("CSP = %q", got)
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff = %q", got)
	}
	if got := h.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("referrer = %q", got)
	}
}

func TestHostAllowlist(t *testing.T) {
	s := openViewerStore(t)
	seedBriefing(t, s, "host:h", "evt-h")
	_, mux := withHost(t, s, "127.0.0.1:8081", "localhost:8081", "[::1]:8081")
	for _, tc := range []struct {
		host string
		code int
	}{
		{"127.0.0.1:8081", 200},
		{"LOCALHOST:8081", 200}, // matching is case-insensitive
		{"[::1]:8081", 200},
		{"evil.example:8081", 403},
		{"127.0.0.1:9999", 403}, // wrong port is foreign
		{"", 403},               // missing Host is refused
	} {
		rec := serveMux(mux, "GET", "/api/groups", tc.host, nil, "")
		if rec.Code != tc.code {
			t.Fatalf("host %q = %d, want %d", tc.host, rec.Code, tc.code)
		}
		if tc.code == 403 {
			assertViewerHeaders(t, rec.Header())
		}
	}
}

// An unset allowlist denies everything: the gate fails closed and only
// the server wiring (derived from the loopback bind) opens it.
func TestHostAllowlistDenyByDefault(t *testing.T) {
	s := openViewerStore(t)
	v := New(s, NewHub())
	mux := http.NewServeMux()
	mux.Handle("/", v.Handler())
	rec := serveMux(mux, "GET", "/api/groups", "127.0.0.1:8081", nil, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("empty allowlist = %d, want 403", rec.Code)
	}
}

// LoopbackHosts covers the bind IP plus the names a local browser sends.
func TestLoopbackHosts(t *testing.T) {
	got := LoopbackHosts("8081")
	want := []string{"127.0.0.1:8081", "localhost:8081", "[::1]:8081"}
	if len(got) != len(want) {
		t.Fatalf("hosts = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hosts = %v, want %v", got, want)
		}
	}
}

func TestViewerSecurityHeaders(t *testing.T) {
	s := openViewerStore(t)
	seedBriefing(t, s, "host:sec", "evt-sec")
	_, mux := withHost(t, s, "127.0.0.1:8081")
	for _, target := range []string{"/", "/app.js", "/api/briefings", "/api/groups", "/api/overview"} {
		rec := serveMux(mux, "GET", target, "127.0.0.1:8081", nil, "")
		if rec.Code != 200 {
			t.Fatalf("%s = %d", target, rec.Code)
		}
		assertViewerHeaders(t, rec.Header())
	}
	// Errors carry the headers too.
	rec := serveMux(mux, "GET", "/api/briefings?limit=12abc", "127.0.0.1:8081", nil, "")
	if rec.Code != 400 {
		t.Fatalf("garbage limit = %d", rec.Code)
	}
	assertViewerHeaders(t, rec.Header())
}

func TestPostGuard(t *testing.T) {
	s := openViewerStore(t)
	v := New(s, NewHub())
	v.AllowedHosts = []string{"127.0.0.1:8081"}
	ok := v.guardStateChanging(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		writeJSON(w, map[string]string{"ok": "true"})
	})
	post := func(headers map[string]string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/decisions", strings.NewReader(body))
		req.Host = "127.0.0.1:8081"
		for k, val := range headers {
			req.Header.Set(k, val)
		}
		rec := httptest.NewRecorder()
		ok(rec, req)
		return rec
	}
	jsonCT := map[string]string{"Content-Type": "application/json"}
	// Wrong method, wrong content type, cross-site, missing and foreign origins.
	if rec := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/decisions", nil)
		req.Host = "127.0.0.1:8081"
		rec := httptest.NewRecorder()
		ok(rec, req)
		return rec
	}(); rec.Code != 405 {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}
	if rec := post(map[string]string{"Content-Type": "text/plain", "Origin": "http://127.0.0.1:8081"}, "{}"); rec.Code != 415 {
		t.Fatalf("text/plain = %d, want 415", rec.Code)
	}
	if rec := post(map[string]string{"Content-Type": "application/json", "Origin": "http://127.0.0.1:8081", "Sec-Fetch-Site": "cross-site"}, "{}"); rec.Code != 403 {
		t.Fatalf("cross-site = %d, want 403", rec.Code)
	}
	if rec := post(jsonCT, "{}"); rec.Code != 403 {
		t.Fatalf("missing origin = %d, want 403", rec.Code)
	}
	if rec := post(map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example/"}, "{}"); rec.Code != 403 {
		t.Fatalf("foreign origin = %d, want 403", rec.Code)
	}
	// Same-origin JSON passes and keeps the header set.
	rec := post(map[string]string{
		"Content-Type": "application/json", "Origin": "http://127.0.0.1:8081", "Sec-Fetch-Site": "same-origin",
	}, `{"decision":"expected"}`)
	if rec.Code != 200 {
		t.Fatalf("same-origin = %d", rec.Code)
	}
	assertViewerHeaders(t, rec.Header())
	// Bodies past 8 KiB cannot be read through the guard.
	big := strings.Repeat("x", (8<<10)+1)
	req := httptest.NewRequest("POST", "/api/decisions", strings.NewReader(`{"pad":"`+big+`"}`))
	req.Host = "127.0.0.1:8081"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:8081")
	capped := v.guardStateChanging(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err == nil {
			t.Error("over-cap body read must fail")
		}
	})
	capped(httptest.NewRecorder(), req)
}

func TestSameOriginBinding(t *testing.T) {
	v := New(nil, NewHub())
	v.AllowedHosts = []string{"127.0.0.1:8081"}
	plain := httptest.NewRequest("POST", "/api/decisions", nil)
	plain.Host = "127.0.0.1:8081"
	for _, tc := range []struct {
		origin string
		want   bool
	}{
		{"http://127.0.0.1:8081", true},
		{"http://localhost:8081", false},      // allowed host, different origin
		{"https://127.0.0.1:8081", false},     // scheme follows the connection
		{"http://user@127.0.0.1:8081", false}, // userinfo is not a serialized origin
		{"http://127.0.0.1:8081/x", false},    // paths are not a serialized origin
		{"http://127.0.0.1:8081/?x=1", false}, // queries are not a serialized origin
		{"http://127.0.0.1:8081#x", false},    // fragments are not a serialized origin
		{"http://127.0.0.1:9999", false},      // port must match the request
		{"ftp://127.0.0.1:8081", false},       // HTTP(S) only
		{"null", false},                       // sandboxed iframes never pass
		{"", false},                           // missing origin is refused
		{"http://[::1]:8081", false},          // must equal r.Host, not just the list
	} {
		if got := v.sameOrigin(tc.origin, plain); got != tc.want {
			t.Fatalf("origin %q = %v, want %v", tc.origin, got, tc.want)
		}
	}
	// An https request accepts only the https twin of its own host.
	tlsReq := httptest.NewRequest("POST", "https://127.0.0.1:8081/api/decisions", nil)
	tlsReq.Host = "127.0.0.1:8081"
	tlsReq.TLS = &tls.ConnectionState{}
	if !v.sameOrigin("https://127.0.0.1:8081", tlsReq) {
		t.Fatal("https twin of the request host must pass")
	}
	if v.sameOrigin("http://127.0.0.1:8081", tlsReq) {
		t.Fatal("http origin on a TLS request must fail")
	}
}

// Router-generated viewer responses go through the same gate and headers:
// a method mismatch is 405 (not a headerless mux default) and still
// refuses foreign hosts.
func TestMuxResponsesGatedAndHeaded(t *testing.T) {
	s := openViewerStore(t)
	_, h := withHost(t, s, "127.0.0.1:8081")
	rec := serveMux(h, "POST", "/api/groups", "127.0.0.1:8081", nil, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST on a GET route = %d, want 405", rec.Code)
	}
	assertViewerHeaders(t, rec.Header())
	if rec := serveMux(h, "POST", "/api/groups", "evil.example:8081", nil, ""); rec.Code != 403 {
		t.Fatalf("foreign host on mux 405 = %d, want 403", rec.Code)
	}
}

func TestOverviewStubShape(t *testing.T) {
	s := openViewerStore(t)
	v := New(s, NewHub())
	rec := httptest.NewRecorder()
	v.handleOverview(rec, httptest.NewRequest("GET", "/api/overview", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("overview = %q", rec.Body.String())
	}
	for _, key := range []string{"verdict", "headline", "receipts", "coverage", "liveness", "attention", "digest"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("overview stub missing frozen key %q: %v", key, body)
		}
	}
	if body["verdict"] == "quiet" || body["verdict"] == "review" || body["verdict"] == "act" {
		t.Fatalf("stub must never claim a computed verdict: %v", body["verdict"])
	}
	if rec.Header().Get("Content-Security-Policy") != csp {
		t.Fatalf("overview CSP = %q", rec.Header().Get("Content-Security-Policy"))
	}
}
