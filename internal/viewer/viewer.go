// Package viewer is Step 12: the bridge's local outputs. It serves the
// operator UI (embedded static assets), machine-readable JSON, and the
// bridge SSE stream (`briefing` + `group` only — never `annotation`,
// which belongs to ScanRelay's stream). Everything here is loopback-only
// local display: briefing rows are already restored before storage, and
// the token map never leaves the process through these handlers.
package viewer

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

//go:embed web/index.html web/app.js web/style.css
var assets embed.FS

// csp is the restrictive policy on every viewer response (Designv5 §9.2):
// same-origin only, no inline scripts or styles.
const csp = "default-src 'self'"

// Event is one SSE message: the stream name plus the JSON payload.
type Event struct {
	Name string
	Data []byte
}

// Hub fans briefing and group updates out to SSE subscribers. Broadcast
// never blocks the caller (the briefing worker): each subscriber has a
// buffered channel and a slow one is skipped and counted, never waited on.
type Hub struct {
	mu      sync.Mutex
	subs    map[chan Event]struct{}
	dropped atomic.Int64
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

// Subscribe registers a buffered SSE feed; call unsubscribe when the
// connection ends or the subscriber leaks.
func (h *Hub) Subscribe(buf int) (<-chan Event, func()) {
	if buf <= 0 {
		buf = 32
	}
	ch := make(chan Event, buf)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// Broadcast encodes payload once and queues it on every subscriber without
// blocking. Marshal failures are caller bugs: they are logged with no
// payload content and skipped.
func (h *Hub) Broadcast(name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("viewer: hub encode %s: marshal failed", name)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- Event{Name: name, Data: data}:
		default:
			h.dropped.Add(1)
		}
	}
}

// Dropped reports broadcasts skipped for slow subscribers.
func (h *Hub) Dropped() int64 { return h.dropped.Load() }

// SubscriberCount is for tests.
func (h *Hub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Viewer serves the UI, JSON, and SSE from one store and hub.
type Viewer struct {
	store *store.Store
	hub   *Hub
}

// New returns a Viewer over store, fanning worker callbacks into hub.
func New(s *store.Store, hub *Hub) *Viewer {
	if hub == nil {
		hub = NewHub()
	}
	return &Viewer{store: s, hub: hub}
}

// Hub exposes the event hub for the briefing worker callbacks.
func (v *Viewer) Hub() *Hub { return v.hub }

// Attach registers the viewer routes on mux. The ingest mux owns the
// loopback bind; the viewer adds no listener and no auth beyond it.
func (v *Viewer) Attach(mux *http.ServeMux) {
	mux.HandleFunc("GET /", v.handleIndex)
	mux.HandleFunc("GET /app.js", v.serveAsset("web/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /style.css", v.serveAsset("web/style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /events", v.handleEvents)
	mux.HandleFunc("GET /api/briefings", v.handleBriefings)
	mux.HandleFunc("GET /api/briefing", v.handleBriefing)
	mux.HandleFunc("GET /api/groups", v.handleGroups)
}

func (v *Viewer) serveAsset(name, contentType string) func(http.ResponseWriter, *http.Request) {
	raw, err := assets.ReadFile(name)
	if err != nil {
		// Embedded at compile time; a missing asset is a build bug.
		panic(fmt.Sprintf("viewer: embedded asset %s missing: %v", name, err))
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(raw)
	}
}

func (v *Viewer) handleIndex(w http.ResponseWriter, _ *http.Request) {
	raw, err := assets.ReadFile("web/index.html")
	if err != nil {
		panic(fmt.Sprintf("viewer: embedded asset web/index.html missing: %v", err))
	}
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

// sseFlusher aborts the stream cleanly if the connection cannot flush.
func sseWrite(w http.ResponseWriter, ev Event) bool {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data); err != nil {
		return false
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
		return true
	}
	return false
}

// handleEvents streams briefing and group updates. On connect the client
// gets the current group snapshot immediately, so a reconnect restores
// exact state on the next tick at the latest without replay accounting.
func (v *Viewer) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsub := v.hub.Subscribe(32)
	defer unsub()
	if groups, err := v.store.OpenGroupSnapshots(r.Context()); err == nil {
		if !sseWrite(w, Event{Name: "group", Data: mustJSON(groupWireOf(groups))}) {
			return
		}
	} else {
		log.Printf("viewer: initial group snapshot: %s", store.SanitizeErrorText(err.Error()))
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if !sseWrite(w, ev) {
				return
			}
		}
	}
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return raw
}

// briefingView is the machine-and-UI JSON shape: everything the dossier
// renders, minus the tokenized audit trail, which stays server-side.
type briefingView struct {
	BriefingID       string   `json:"briefing_id"`
	GroupID          string   `json:"group_id"`
	Host             string   `json:"host"`
	Baseline         string   `json:"baseline"`
	ModelSuggestion  *string  `json:"model_suggestion"`
	Priority         string   `json:"priority"`
	PriorityReason   string   `json:"priority_reason"`
	BaselineRule     string   `json:"baseline_rule"`
	Clamped          bool     `json:"clamped"`
	FallbackUsed     bool     `json:"fallback_used"`
	WhatChanged      string   `json:"what_changed"`
	Context          string   `json:"context"`
	RecommendedCheck string   `json:"recommended_check"`
	CreatedAt        string   `json:"created_at"`
	EventIDs         []string `json:"event_ids"`
}

func viewOf(b store.Briefing) briefingView {
	return briefingView{
		BriefingID: b.BriefingID, GroupID: b.GroupID, Host: b.HostAnchorKey,
		Baseline: b.Baseline, ModelSuggestion: b.ModelSuggestion, Priority: b.Priority,
		PriorityReason: b.PriorityReason, BaselineRule: b.BaselineRule, Clamped: b.Clamped,
		FallbackUsed: b.FallbackUsed, WhatChanged: b.WhatChanged, Context: b.Context,
		RecommendedCheck: b.RecommendedCheck, CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339),
		EventIDs: append([]string(nil), b.EventIDs...),
	}
}

// groupWire is the SSE/API snapshot shape: unix deadlines keep the JSON
// numeric and the browser renders remaining seconds locally.
type groupWire struct {
	GroupID    string `json:"group_id"`
	Anchor     string `json:"anchor"`
	Status     string `json:"status"`
	DeadlineAt int64  `json:"deadline_unix"`
	Events     int    `json:"event_count"`
}

func groupWireOf(groups []store.GroupSnapshot) []groupWire {
	out := make([]groupWire, 0, len(groups))
	for _, g := range groups {
		out = append(out, groupWire{
			GroupID: g.GroupID, Anchor: g.Anchor, Status: g.Status,
			DeadlineAt: g.Deadline.Unix(), Events: g.EventCount,
		})
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// handleBriefings serves the newest briefings first: ?limit=N (default 50,
// max 500, enforced by the store).
func (v *Viewer) handleBriefings(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		var n int
		if _, err := fmt.Sscanf(q, "%d", &n); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := v.store.RecentBriefings(r.Context(), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]briefingView, 0, len(rows))
	for _, b := range rows {
		out = append(out, viewOf(b))
	}
	writeJSON(w, out)
}

// handleBriefing serves one briefing by ?id= (briefing or group ID).
func (v *Viewer) handleBriefing(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	b, err := v.store.BriefingForGroup(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, viewOf(b))
}

// handleGroups serves the current open-group snapshot.
func (v *Viewer) handleGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := v.store.OpenGroupSnapshots(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, groupWireOf(groups))
}

// Fanout builds the briefing worker callbacks over hub and publisher:
// stored briefings broadcast as `briefing` SSE events and enqueue for
// ScanRelay; group snapshots broadcast as `group`. Every path is
// non-blocking (hub skips slow subscribers, publisher drops on a full
// queue), so the worker never stalls on its observers. Either may be nil.
func Fanout(h *Hub, p *Publisher) (func(store.Briefing), func([]store.GroupSnapshot)) {
	onBriefing := func(b store.Briefing) {
		if h != nil {
			h.Broadcast("briefing", viewOf(b))
		}
		if p != nil {
			p.Enqueue(b)
		}
	}
	onGroups := func(groups []store.GroupSnapshot) {
		if h != nil {
			h.Broadcast("group", groupWireOf(groups))
		}
	}
	return onBriefing, onGroups
}

func writeError(w http.ResponseWriter, err error) {
	// Store errors carry IDs and constraints, never raw values; still,
	// only the kind crosses into HTTP responses.
	log.Printf("viewer: %s", store.SanitizeErrorText(err.Error()))
	http.Error(w, "lookup failed", http.StatusInternalServerError)
}
