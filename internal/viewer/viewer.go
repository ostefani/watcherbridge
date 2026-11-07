// Package viewer is Step 12: the bridge's local outputs. It serves the
// operator UI (embedded static assets), machine-readable JSON, and the
// bridge SSE stream (`briefing` + `group` only — never `annotation`,
// which belongs to ScanRelay's stream). Everything here is loopback-only
// local display: briefing rows are already restored before storage, and
// the token map never leaves the process through these handlers.
package viewer

import (
	"context"
	"database/sql"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ostefani/watcherbridge/internal/overview"
	"github.com/ostefani/watcherbridge/internal/store"
)

//go:embed web/dist
var assets embed.FS

// csp is the restrictive policy on every viewer response (Designv8 §2.4):
// same-origin only, no framing, no base hijack, no form exfiltration, no
// inline scripts or styles.
const csp = "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// SetSecurityHeaders applies the Step 16 header set to every viewer
// response. Cache policy stays per-handler. Exported so the server can
// cover mux-generated responses outside the viewer subtree (clean-path
// redirects, unknown paths) without duplicating the values.
func SetSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// writeViewerError answers a viewer error with the security headers set:
// header-bearing failures must not skip them.
func writeViewerError(w http.ResponseWriter, msg string, code int) {
	SetSecurityHeaders(w)
	http.Error(w, msg, code)
}

// Event is one SSE message: the stream name plus the JSON payload. ID
// carries the briefing seq for `briefing` events (the SSE `id:` field,
// which backs `Last-Event-ID` resume). All other events carry no id, so
// `Last-Event-ID` tracks briefings only.
type Event struct {
	Name string
	Data []byte
	ID   string
}

// Subscription is one SSE feed. Lagged is set when Broadcast skips a full
// buffer; the handler consumes it into a `resync` (never a partial
// replay) and clears it.
type Subscription struct {
	C      <-chan Event
	ch     chan Event
	lagged atomic.Bool
}

// Lagged reports a skipped broadcast since the last ConsumeLagged.
func (s *Subscription) Lagged() bool { return s.lagged.Load() }

// ConsumeLagged returns and clears the skipped-broadcast flag.
func (s *Subscription) ConsumeLagged() bool { return s.lagged.Swap(false) }

// Hub fans briefing and group updates out to SSE subscribers. Broadcast
// never blocks the caller (the briefing worker): each subscriber has a
// buffered channel and a slow one is skipped and counted, never waited on.
type Hub struct {
	mu      sync.Mutex
	subs    map[*Subscription]struct{}
	dropped atomic.Int64
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[*Subscription]struct{}{}} }

// Subscribe registers a buffered SSE feed; call unsubscribe when the
// connection ends or the subscriber leaks.
func (h *Hub) Subscribe(buf int) (*Subscription, func()) {
	sub, unsub, _ := h.TrySubscribe(buf, 0)
	return sub, unsub
}

// TrySubscribe registers a feed unless max subscribers are already
// attached (max <= 0 means unbounded). The check and insert hold the hub
// lock, so the cap cannot be raced past. A refused subscriber means the
// handler answers 503, never blocks the ingest path.
func (h *Hub) TrySubscribe(buf, max int) (*Subscription, func(), bool) {
	if buf <= 0 {
		buf = 32
	}
	ch := make(chan Event, buf)
	sub := &Subscription{C: ch, ch: ch}
	h.mu.Lock()
	defer h.mu.Unlock()
	if max > 0 && len(h.subs) >= max {
		return nil, nil, false
	}
	h.subs[sub] = struct{}{}
	return sub, func() {
		h.mu.Lock()
		delete(h.subs, sub)
		h.mu.Unlock()
	}, true
}

// Broadcast encodes payload once and queues it on every subscriber without
// blocking. Marshal failures are caller bugs: they are logged with no
// payload content and skipped.
func (h *Hub) Broadcast(name string, payload any) {
	h.BroadcastID(name, "", payload)
}

// BroadcastID is Broadcast with an SSE id (briefing seq). The id travels
// out of band from the JSON: `Last-Event-ID` compares it, the client
// upserts the data by `briefing_id`.
func (h *Hub) BroadcastID(name, id string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("viewer: hub encode %s: marshal failed", name)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		select {
		case sub.ch <- Event{Name: name, Data: data, ID: id}:
		default:
			h.dropped.Add(1)
			sub.lagged.Store(true)
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

// Stream defaults mirror Designv8 §10; the server overwrites them from
// config, tests leave them zero.
const (
	defaultSSEHeartbeat    = 15 * time.Second
	defaultSSEReplayMax    = 200
	defaultSSEWriteTimeout = 5 * time.Second
)

// Viewer serves the UI, JSON, and SSE from one store and hub.
type Viewer struct {
	store *store.Store
	hub   *Hub

	// Heartbeat is the SSE comment-ping interval (dead-peer detection).
	// ReplayMax caps replay per reconnect; over it is resync.
	Heartbeat time.Duration
	ReplayMax int
	// WriteTimeout bounds every individual stream write. A peer that
	// stops reading fails its next write instead of wedging the handler
	// (and, through it, graceful shutdown) forever.
	WriteTimeout time.Duration
	// MaxSubscribers caps concurrent streams; over it the handler
	// answers 503. Zero means unbounded (tests).
	MaxSubscribers int

	// GroupWindowSeconds is the grouping quiet window behind the snapshot
	// countdowns, set once by the server from config.
	GroupWindowSeconds int64

	// SnoozeDefault is the horizon applied when a snooze arrives
	// without an explicit until, set once by the server from config.
	// Zero means 24h (tests that build a Viewer directly).
	SnoozeDefault time.Duration

	// RuleProposeAfter is the count of matching expected decisions
	// that derives a review-policy proposal, set once by the server
	// from config. Zero means 3 (tests that build a Viewer directly).
	RuleProposeAfter int
	// RuleDefaultTTL is the approval grant applied when approve
	// arrives without a ttl, set once by the server from config.
	// Zero means 90d (tests that build a Viewer directly).
	RuleDefaultTTL time.Duration

	// AllowedHosts gates every viewer route (Designv8 §2.4): r.Host must
	// match an entry exactly (case-insensitive) or the answer is 403.
	// Empty denies all — the server always derives it from the loopback
	// bind, so an unset list fails closed, never open. /webhook is
	// HMAC-protected and stays outside this gate.
	AllowedHosts []string

	// Overview is the Step 20 refresher behind /api/overview, /api/health,
	// and the `overview` SSE event. Nil means unconfigured: both routes
	// answer 503, and the connect sequence skips the overview event.
	Overview *Refresher

	lifetime context.Context
	cancel   context.CancelFunc
}

// New returns a Viewer over store, fanning worker callbacks into hub.
// It attaches a default overview refresher (24h window, 168h maximum,
// retention equal to the maximum so the default validates) so the
// connect sequence and both routes work before the server replaces it
// with the configured one; health and publisher stay detached until
// main attaches them.
func New(s *store.Store, hub *Hub) *Viewer {
	if hub == nil {
		hub = NewHub()
	}
	lifetime, cancel := context.WithCancel(context.Background())
	v := &Viewer{store: s, hub: hub, lifetime: lifetime, cancel: cancel}
	v.Overview = NewRefresher(s, nil, nil,
		overview.DefaultWindow, overview.DefaultMaxWindow, overview.DefaultMaxWindow)
	return v
}

// Close unblocks every open stream handler so server shutdown never waits
// out open tabs.
func (v *Viewer) Close() {
	v.cancel()
}

// Hub exposes the event hub for the briefing worker callbacks.
func (v *Viewer) Hub() *Hub { return v.hub }

// Attach registers the plain viewer routes on mux. Use Handler for the
// production mount: it applies Host validation and headers before any
// mux-generated response (405s, clean-path redirects), which per-handler
// wrapping cannot reach.
func (v *Viewer) Attach(mux *http.ServeMux) {
	mux.HandleFunc("GET /", v.handleIndex)
	mux.HandleFunc("GET /assets/", v.handleAssets)
	mux.HandleFunc("GET /events", v.handleEvents)
	mux.HandleFunc("GET /api/briefings", v.handleBriefings)
	mux.HandleFunc("GET /api/briefing", v.handleBriefing)
	mux.HandleFunc("GET /api/groups", v.handleGroups)
	mux.HandleFunc("GET /api/devices", v.handleDevices)
	mux.HandleFunc("GET /api/devices/{id}", v.handleDevice)
	mux.HandleFunc("GET /api/overview", v.handleOverview)
	mux.HandleFunc("GET /api/health", v.handleHealth)
	mux.HandleFunc("GET /api/decisions", v.listDecisions)
	mux.HandleFunc("GET /api/decisions/{id}/members", v.handleDecisionMembers)
	mux.HandleFunc("POST /api/decisions", v.appendDecisionGuarded)
	mux.HandleFunc("GET /api/incidents", v.handleIncidents)
	mux.HandleFunc("GET /api/incidents/{id}", v.handleIncident)
	mux.HandleFunc("POST /api/decisions/{id}/undo", v.handleDecisionUndo)
	mux.HandleFunc("GET /api/rules/proposals", v.listProposals)
	mux.HandleFunc("POST /api/rules/approve", v.approveRuleGuarded)
	mux.HandleFunc("POST /api/rules/dismiss", v.dismissProposalGuarded)
	mux.HandleFunc("POST /api/rules/revoke", v.revokeRuleGuarded)
	// Unknown GET /api/* paths are 404 JSON, never the index catch-all:
	// Go prefers the registered routes above, so this only fires when
	// nothing else matches. (A methodless "/api/" pattern conflicts
	// with the "GET /" catch-all, so other methods on unknown API
	// paths keep the mux 405.) Without it a typo'd API path serves the
	// HTML shell with 200, which JSON clients cannot distinguish from
	// an empty success.
	mux.HandleFunc("GET /api/", v.handleUnknownAPI)
}

// handleUnknownAPI answers unregistered API paths with a headed 404.
func (v *Viewer) handleUnknownAPI(w http.ResponseWriter, _ *http.Request) {
	writeViewerError(w, "not found", http.StatusNotFound)
}

// Handler returns the viewer subtree with Host validation applied ahead
// of routing, so mux-generated responses (method mismatches,
// clean-path redirects) are gated and headed like handler responses.
// Mount it on "/" beside the exact webhook routes: Go prefers the
// longer method-qualified webhook patterns, so /webhook keeps its
// separate HMAC behavior while everything else falls through here.
func (v *Viewer) Handler() http.Handler {
	mux := http.NewServeMux()
	v.Attach(mux)
	return v.requireLocalHost(mux.ServeHTTP)
}

// allowedHost reports whether an r.Host value may use viewer routes.
// Matching is case-insensitive; a missing or foreign host is refused, so
// a DNS-rebinding page cannot read the loopback viewer.
func (v *Viewer) allowedHost(host string) bool {
	if host == "" {
		return false
	}
	host = strings.ToLower(host)
	for _, h := range v.AllowedHosts {
		if host == strings.ToLower(h) {
			return true
		}
	}
	return false
}

func (v *Viewer) requireLocalHost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !v.AllowedHost(r.Host) {
			writeViewerError(w, "foreign host", http.StatusForbidden)
			return
		}
		// Headers go on before routing so mux-generated responses carry
		// them too; handlers re-assert the same values.
		SetSecurityHeaders(w)
		next(w, r)
	}
}

// AllowedHost reports whether a Host value may use viewer routes.
// Exported so the server enforces the policy before outer-mux
// canonicalization (which otherwise redirects before routing ever
// reaches the mount); the mount re-checks it after routing.
func (v *Viewer) AllowedHost(host string) bool { return v.allowedHost(host) }

// sameOrigin enforces a valid serialized HTTP(S) origin exactly matching
// the request: scheme follows the connection (https only with TLS),
// userinfo, path, query and fragment are rejected (a serialized origin
// carries none), the host must equal r.Host, and r.Host must additionally
// clear the allowlist (kept as a separate check). Without the r.Host
// binding, an allowed-but-different origin (localhost vs 127.0.0.1)
// would pass on a Fetch-Metadata-less request despite being
// cross-origin.
func (v *Viewer) sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.User != nil || u.Opaque != "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
		return false
	}
	wantScheme := "http"
	if r.TLS != nil {
		wantScheme = "https"
	}
	if !strings.EqualFold(u.Scheme, wantScheme) {
		return false
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	return v.allowedHost(r.Host)
}

// LoopbackHosts derives the viewer allowlist from a bind port: the
// loopback IP the server binds, plus the two names a local browser may
// send. The server refuses non-loopback binds at startup, so deriving
// from the bind cannot open a LAN interface.
func LoopbackHosts(port string) []string {
	return []string{"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port}
}

// maxPostBytes caps state-changing bodies (Designv8 §2.4: 8 KiB).
const maxPostBytes = 8 << 10

// guardStateChanging enforces the Step 16 POST contract for decision and
// rule routes (first used in Step 25): POST with a JSON body of at most 8
// KiB, an Origin equal to an allowed host, and — when the header is
// present — Sec-Fetch-Site: same-origin. curl without an Origin is
// refused: browsers always send one on POST.
func (v *Viewer) guardStateChanging(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		SetSecurityHeaders(w)
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if !v.sameOrigin(r.Header.Get("Origin"), r) {
			http.Error(w, "foreign origin", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxPostBytes)
		next(w, r)
	}
}

func (v *Viewer) handleIndex(w http.ResponseWriter, _ *http.Request) {
	raw, err := assets.ReadFile("web/dist/index.html")
	if err != nil {
		panic(fmt.Sprintf("viewer: embedded asset web/dist/index.html missing: %v", err))
	}
	SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

// handleAssets serves the committed Vite bundle. Only .js and .css
// under web/dist/assets/ are reachable, and the request path is
// cleaned and confined — nothing outside the bundle directory can be
// named, and no other extension is served.
func (v *Viewer) handleAssets(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/assets/")
	if rest == "" || strings.Contains(rest, "..") || rest != path.Clean(rest) {
		writeViewerError(w, "not found", http.StatusNotFound)
		return
	}
	var contentType string
	switch path.Ext(rest) {
	case ".js":
		contentType = "text/javascript; charset=utf-8"
	case ".css":
		contentType = "text/css; charset=utf-8"
	default:
		writeViewerError(w, "not found", http.StatusNotFound)
		return
	}
	raw, err := assets.ReadFile("web/dist/assets/" + rest)
	if err != nil {
		writeViewerError(w, "not found", http.StatusNotFound)
		return
	}
	SetSecurityHeaders(w)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

// sseWrite emits one SSE message, with the `id:` field when the event
// carries one (briefing seq). It flushes so the client sees the message
// promptly, and reports a dead peer through the write error.
func sseWrite(w http.ResponseWriter, ev Event) bool {
	if ev.ID != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", ev.ID); err != nil {
			return false
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data); err != nil {
		return false
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
		return true
	}
	return false
}

// sseComment emits an SSE comment: the heartbeat ping that also detects
// dead peers through a failed write.
func sseComment(w http.ResponseWriter) bool {
	if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
		return false
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
		return true
	}
	return false
}

// parseLastEventID applies the Designv8 §2.2 rule to the request header:
// absent, non-numeric, negative or above 2^62 means no replay.
func parseLastEventID(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n > 1<<62 {
		return 0, false
	}
	return int64(n), true
}

// handleEvents streams briefing and group updates. Every write carries
// its own deadline: a peer that stops reading fails the next write
// instead of wedging the handler (and, through it, graceful shutdown)
// forever. On connect the client gets, in order: the group snapshot, a
// freshly computed overview stub, then the Last-Event-ID replay-or-resync,
// then live events. Overlap between replay and live is harmless: the
// client upserts by briefing_id. Over the subscriber cap the handler
// answers 503, never queuing behind the ingest path.
func (v *Viewer) handleEvents(w http.ResponseWriter, r *http.Request) {
	SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	sub, unsub, ok := v.hub.TrySubscribe(32, v.MaxSubscribers)
	if !ok {
		writeViewerError(w, "too many stream subscribers", http.StatusServiceUnavailable)
		return
	}
	defer unsub()

	// Cancel snapshot and replay reads on viewer close or client disconnect.
	streamCtx, cancel := context.WithCancel(r.Context())
	stopClose := context.AfterFunc(v.lifetime, cancel)
	defer func() {
		stopClose()
		cancel()
	}()
	if v.lifetime.Err() != nil {
		return
	}
	rc := http.NewResponseController(w)
	wto := v.WriteTimeout
	if wto <= 0 {
		wto = defaultSSEWriteTimeout
	}
	write := func(ev Event) bool {
		// No new write starts after shutdown: at most the one in-flight
		// write runs to its deadline. Check viewer closure directly because
		// the AfterFunc cancellation runs asynchronously.
		select {
		case <-v.lifetime.Done():
			return false
		case <-streamCtx.Done():
			return false
		default:
		}
		// Best-effort on recorders and middleware that don't implement
		// deadlines; enforced on real connections.
		_ = rc.SetWriteDeadline(time.Now().Add(wto))
		return sseWrite(w, ev)
	}
	comment := func() bool {
		select {
		case <-v.lifetime.Done():
			return false
		case <-streamCtx.Done():
			return false
		default:
		}
		_ = rc.SetWriteDeadline(time.Now().Add(wto))
		return sseComment(w)
	}
	hb := v.Heartbeat
	if hb <= 0 {
		hb = defaultSSEHeartbeat
	}
	replayMax := v.ReplayMax
	if replayMax <= 0 {
		replayMax = defaultSSEReplayMax
	}
	hbTicker := time.NewTicker(hb)
	defer hbTicker.Stop()

	if groups, err := v.store.OpenGroupSnapshots(streamCtx); err == nil {
		if !write(Event{Name: "group", Data: mustJSON(groupWireOf(time.Now().UTC(), v.GroupWindowSeconds, groups))}) {
			return
		}
	} else {
		log.Printf("viewer: initial group snapshot: %s", store.SanitizeErrorText(err.Error()))
	}
	// Connect-time overview: the cached computation when the refresher
	// has run, a fresh default-window computation before the first tick
	// otherwise. A store failure skips the event — the client loads the
	// overview over the API — never a fabricated unknown.
	if ov := v.Overview; ov != nil {
		body := ov.Cached()
		if body == nil {
			var err error
			if body, _, err = ov.compute(streamCtx, ov.window); err != nil {
				log.Printf("viewer: connect overview: %s", store.SanitizeErrorText(err.Error()))
				body = nil
			}
		}
		if body != nil {
			if !write(Event{Name: "overview", Data: body}) {
				return
			}
		}
	}
	if !v.replay(streamCtx, w, r, replayMax, write) {
		return
	}

	for {
		if sub.ConsumeLagged() {
			// A slow subscriber missed broadcasts; never a partial
			// replay — the client refetches overview and the latest page.
			if !v.emitResync(streamCtx, w, write) {
				return
			}
			continue
		}
		select {
		case <-streamCtx.Done():
			// Covers both client disconnect and viewer Close.
			return
		case <-hbTicker.C:
			if !comment() {
				return
			}
		case ev := <-sub.C:
			if !write(ev) {
				return
			}
		}
	}
}

// replay applies the Last-Event-ID rule: absent or garbage replays nothing
// (the client loads state as on first load); a valid id at or below max
// seq replays seq > last ascending, at most replayMax rows — over the cap
// sends resync and replays nothing; above max seq (restored database)
// sends resync. The verdict comes from a single bounded fetch (replayMax+1
// rows): no count/fetch race can slip an over-cap replay past the check.
// A store failure resyncs instead of streaming on silently — the client
// must refetch, never advance its cursor past missed history.
func (v *Viewer) replay(ctx context.Context, w http.ResponseWriter, r *http.Request, replayMax int, write func(Event) bool) bool {
	last, ok := parseLastEventID(r.Header.Get("Last-Event-ID"))
	if !ok {
		return true
	}
	maxSeq, err := v.store.MaxSeq(ctx)
	if err != nil {
		log.Printf("viewer: replay max seq: %s", store.SanitizeErrorText(err.Error()))
		return v.emitResync(ctx, w, write)
	}
	if last > maxSeq {
		return v.emitResync(ctx, w, write)
	}
	rows, err := v.store.ReplayAfter(ctx, last, replayMax+1)
	if err != nil {
		log.Printf("viewer: replay fetch: %s", store.SanitizeErrorText(err.Error()))
		return v.emitResync(ctx, w, write)
	}
	if len(rows) > replayMax {
		return v.emitResync(ctx, w, write)
	}
	for _, b := range rows {
		// Shutdown is observed between records: each write gets a fresh
		// deadline, so without this a merely slow peer could stretch
		// replay past the whole shutdown grace. Check viewer closure directly
		// because the AfterFunc cancellation runs asynchronously.
		select {
		case <-v.lifetime.Done():
			return false
		case <-ctx.Done():
			return false
		default:
		}
		if !write(Event{Name: "briefing", ID: strconv.FormatInt(b.Seq, 10), Data: mustJSON(viewOf(b))}) {
			return false
		}
	}
	return true
}

// resyncBody tells the client to refetch overview and the latest briefing
// page and reset its stored last seq to max_seq.
type resyncBody struct {
	MaxSeq int64 `json:"max_seq"`
}

// emitResync sends `event: resync`. A store failure still resyncs (with
// max_seq 0, so the client refetches the latest page); the live stream
// continues afterwards.
func (v *Viewer) emitResync(ctx context.Context, w http.ResponseWriter, write func(Event) bool) bool {
	maxSeq, err := v.store.MaxSeq(ctx)
	if err != nil {
		log.Printf("viewer: resync max seq: %s", store.SanitizeErrorText(err.Error()))
	}
	return write(Event{Name: "resync", Data: mustJSON(resyncBody{MaxSeq: maxSeq})})
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
	GroupingVersion  int      `json:"grouping_version"`
	BriefingID       string   `json:"briefing_id"`
	GroupID          string   `json:"group_id"`
	Host             string   `json:"host"`
	Baseline         string   `json:"baseline"`
	ModelSuggestion  *string  `json:"model_suggestion"`
	Priority         string   `json:"priority"`
	PriorityReason   string   `json:"priority_reason"`
	BaselineRule     string   `json:"baseline_rule"`
	Kind             string   `json:"kind"`
	Source           string   `json:"source"`
	Clamped          bool     `json:"clamped"`
	FallbackUsed     bool     `json:"fallback_used"`
	WhatChanged      string   `json:"what_changed"`
	Context          string   `json:"context"`
	RecommendedCheck string   `json:"recommended_check"`
	CreatedAt        string   `json:"created_at"`
	EventIDs         []string `json:"event_ids"`
	Seq              int64    `json:"seq"`
	// Profiles carries the persisted briefing-to-profile links only;
	// ProfilesTruncated reports links beyond the read cap.
	Profiles          []briefingSubjectWire `json:"profiles"`
	ProfilesTruncated bool                  `json:"profiles_truncated,omitempty"`
	// ReferencedEvents pairs each group event ID with its recorded
	// type: an evidence index, not the evidence itself.
	// ReferencedTruncated reports pairs beyond the read cap.
	ReferencedEvents    []referencedEventWire `json:"referenced_events"`
	ReferencedTruncated bool                  `json:"referenced_truncated,omitempty"`
	// Observations is the bounded retained projection for exactly the
	// referenced events. ObservationsTruncated reports entries dropped
	// by the count cap or the snapshot byte budget.
	Observations          []observationWire `json:"observations"`
	ObservationsTruncated bool              `json:"observations_truncated,omitempty"`
	// EffectiveReview is the live review-lane state (absent when no
	// operator review applies); Decisions is the newest-first audit
	// trail with undone markers for the list and undo buttons.
	EffectiveReview *reviewWire    `json:"effective_review,omitempty"`
	Decisions       []decisionWire `json:"decisions"`
	// LikelyExplanation and Confidence are the model's display-only
	// hypothesis and self-assessment (Designv9 §6.3): absent for
	// deterministic rows and pre-migration rows, which carry no model
	// output. They never feed priority, identity or the resolver.
	LikelyExplanation *string           `json:"likely_explanation,omitempty"`
	Confidence        string            `json:"confidence,omitempty"`
	EvidenceRefs      []evidenceRefWire `json:"evidence_refs,omitempty"`
	// InputTokens and OutputTokens are the adapter-reported accounting
	// for the producing calls; CostUSD is nil while no adapter has
	// reported cost (unknown, never zero-filled).
	InputTokens  *int64   `json:"input_tokens,omitempty"`
	OutputTokens *int64   `json:"output_tokens,omitempty"`
	CostUSD      *float64 `json:"cost_usd,omitempty"`
	// Traces is the run's executed tool steps in 0-based order,
	// served on the detail fetch only — never on the list or the
	// stream, which stay lean.
	Traces []traceWire `json:"traces,omitempty"`
}

// evidenceRefWire is one model citation: a real group event or a real
// 0-based executed tool step of the producing run.
type evidenceRefWire struct {
	EventID  string `json:"event_id,omitempty"`
	ToolStep *int   `json:"tool_step,omitempty"`
}

// traceWire is one executed investigator tool step: validated args and
// the scanned result envelope, both tokenized, never raw identifiers.
// Outcome/Summary are the server's bounded presentation classification;
// the raw status/args/result stay untouched.
type traceWire struct {
	Step       int    `json:"step"`
	Tool       string `json:"tool"`
	Args       string `json:"args"`
	Result     string `json:"result"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Outcome    string `json:"outcome,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// traceOutcome classifies one stored tool result for display (Go owns
// the classification; the client renders it). It never treats a
// completed transport as a successful evidentiary check: unknown
// subjects, blocked results, errors, empty results and truncated or
// unrecognized envelopes each map distinctly. Invalid JSON reads as
// unavailable, never a guessed success. Counts stay unknown when the
// envelope omits them.
func traceOutcome(tool, status, resultJSON string) (string, string) {
	const unavailable = "Result unavailable; no conclusion can be drawn from this lookup."
	if status == "blocked" {
		return "blocked", "Redaction blocked this result; retained context is unavailable."
	}
	if status == "error" {
		return "error", "Tool execution failed; result unavailable."
	}
	if status != "ok" || len(resultJSON) == 0 || len(resultJSON) > 4096 {
		return "unavailable", unavailable
	}
	var env struct {
		Result    map[string]json.RawMessage `json:"result"`
		Truncated bool                       `json:"truncated"`
		Omitted   int                        `json:"omitted"`
		Tool      string                     `json:"tool"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &env); err != nil || env.Result == nil || env.Tool != tool || env.Omitted < 0 {
		return "unavailable", unavailable
	}
	switch tool {
	case "get_reported_profile", "get_reported_history", "get_reported_services", "compare_reported_context", "find_similar_reported_changes":
	default:
		return "unavailable", unavailable
	}
	if raw, ok := env.Result["error"]; ok {
		var reason string
		if json.Unmarshal(raw, &reason) != nil || reason == "" {
			return "unavailable", unavailable
		}
		switch reason {
		case "unknown subject":
			return "unknown_subject", "The investigator could not resolve this lookup to a permitted reported subject; history remains unknown."
		case "redaction_blocked":
			return "blocked", "Redaction blocked this result; retained context is unavailable."
		default:
			return "error", "Tool execution failed; result unavailable."
		}
	}
	integer := func(key string) (int64, bool) {
		raw, ok := env.Result[key]
		var n int64
		if !ok || string(raw) == "null" || json.Unmarshal(raw, &n) != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	partial := env.Truncated || env.Omitted > 0
	if tool == "find_similar_reported_changes" {
		count, countOK := integer("report_count")
		days, daysOK := integer("window_days")
		var kind string
		if json.Unmarshal(env.Result["kind"], &kind) != nil || (kind != "appeared" && kind != "disappeared" && kind != "changed") || !countOK || !daysOK || days < 1 || days > 30 {
			return "unavailable", unavailable
		}
		portText := ""
		if raw, ok := env.Result["port"]; ok && string(raw) != "null" {
			port, valid := integer("port")
			if !valid || port < 1 || port > 65535 {
				return "unavailable", unavailable
			}
			portText = fmt.Sprintf(" mentioning port %d", port)
		}
		noun := "reports"
		if count == 1 {
			noun = "report"
		}
		summary := fmt.Sprintf("%d retained %q %s%s received within the lookup's %d-day window in the same scanner-source scope; this may include the triggering event.", count, kind, noun, portText, days)
		if partial {
			return "truncated", summary + " Some result fields were omitted."
		}
		return "data", summary
	}
	if partial {
		return "truncated", "Only part of this lookup result is available; omitted fields remain unknown."
	}
	// Only recognized result shapes support a positive presentation. In
	// particular an unknown services marker is not an empty service list.
	switch tool {
	case "get_reported_history":
		var briefings, observations []json.RawMessage
		b, bok := env.Result["briefings"]
		o, ook := env.Result["observations"]
		if !bok || !ook || string(b) == "null" || string(o) == "null" || json.Unmarshal(b, &briefings) != nil || json.Unmarshal(o, &observations) != nil {
			break
		}
		if len(briefings) == 0 && len(observations) == 0 {
			return "empty", "No retained entries were returned by this history lookup."
		}
		return "data", "Retained history entries were returned for the resolved subject."
	case "get_reported_profile":
		var class string
		if json.Unmarshal(env.Result["reference_class"], &class) == nil && (class == "reported_device" || class == "address" || class == "event" || class == "legacy") {
			return "data", "Reported profile context was returned for the resolved subject."
		}
	case "get_reported_services":
		var ports []json.RawMessage
		if raw, ok := env.Result["ports"]; ok && string(raw) != "null" && json.Unmarshal(raw, &ports) == nil {
			if len(ports) == 0 {
				return "empty", "No retained ports matched this services lookup."
			}
			return "data", "Scanner-reported port evidence was returned."
		}
	case "compare_reported_context":
		if n, ok := integer("samples"); ok && n == 0 {
			return "empty", "No retained samples were returned for comparison."
		}
		var samples []json.RawMessage
		if raw, ok := env.Result["samples"]; ok && json.Unmarshal(raw, &samples) == nil && len(samples) > 0 {
			return "data", "Retained samples were returned for comparison; they do not represent the whole network."
		}
	}
	return "unavailable", unavailable
}

// briefingSubjectWire is one persisted profile link with its
// projection role (host, previous or current).
type briefingSubjectWire struct {
	ProfileID string `json:"profile_id"`
	Role      string `json:"role"`
}

// referencedEventWire pairs one group event ID with its type and
// report timing. OccurredAt is the scanner's reported event time,
// ReceivedAt is local receipt, Target is the reported scan target.
type referencedEventWire struct {
	EventID         string `json:"event_id"`
	EventType       string `json:"event_type"`
	OccurredAt      string `json:"occurred_at,omitempty"`
	ReceivedAt      string `json:"received_at,omitempty"`
	Target          string `json:"target,omitempty"`
	TargetTruncated bool   `json:"target_truncated,omitempty"`
}

func viewOf(b store.Briefing) briefingView {
	view := briefingView{
		GroupingVersion: b.GroupingVersion,
		BriefingID:      b.BriefingID, GroupID: b.GroupID, Host: b.HostAnchorKey,
		Baseline: b.Baseline, ModelSuggestion: b.ModelSuggestion, Priority: b.Priority,
		PriorityReason: b.PriorityReason, BaselineRule: b.BaselineRule, Kind: b.Kind, Source: b.Source, Clamped: b.Clamped,
		FallbackUsed: b.FallbackUsed, WhatChanged: b.WhatChanged, Context: b.Context,
		RecommendedCheck: b.RecommendedCheck, CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339),
		EventIDs: append([]string(nil), b.EventIDs...), Seq: b.Seq,
		Profiles: []briefingSubjectWire{}, ReferencedEvents: []referencedEventWire{},
		Observations:      []observationWire{},
		LikelyExplanation: b.LikelyExplanation, Confidence: b.Confidence,
		InputTokens: b.InputTokens, OutputTokens: b.OutputTokens,
	}
	if len(b.EvidenceRefs) > 0 {
		view.EvidenceRefs = make([]evidenceRefWire, 0, len(b.EvidenceRefs))
		for _, r := range b.EvidenceRefs {
			if r.ToolStep >= 0 {
				step := r.ToolStep
				view.EvidenceRefs = append(view.EvidenceRefs, evidenceRefWire{ToolStep: &step})
				continue
			}
			view.EvidenceRefs = append(view.EvidenceRefs, evidenceRefWire{EventID: r.EventID})
		}
	}
	if b.CostMicros != nil {
		cost := float64(*b.CostMicros) / 1e6
		view.CostUSD = &cost
	}
	return view
}

// groupWire is the SSE/API snapshot shape: unix deadlines keep the JSON
// numeric and the browser renders remaining seconds locally.
type groupWire struct {
	GroupID    string `json:"group_id"`
	Anchor     string `json:"anchor"`
	Status     string `json:"status"`
	Now        int64  `json:"now_unix"`
	OpenedAt   int64  `json:"opened_unix"`
	LastEvent  int64  `json:"last_event_unix"`
	DeadlineAt int64  `json:"deadline_unix"`
	Window     int64  `json:"window_seconds"`
	Events     int    `json:"event_count"`
}

func groupWireOf(now time.Time, windowSeconds int64, groups []store.GroupSnapshot) []groupWire {
	out := make([]groupWire, 0, len(groups))
	for _, g := range groups {
		out = append(out, groupWire{
			GroupID: g.GroupID, Anchor: g.Anchor, Status: g.Status,
			Now: now.Unix(), OpenedAt: g.OpenedAt.Unix(), LastEvent: g.LastEvent.Unix(),
			DeadlineAt: g.Deadline.Unix(), Window: windowSeconds, Events: g.EventCount,
		})
	}
	return out
}

// groupHash fingerprints a snapshot so unchanged ticks stay silent: the
// per-tick broadcast would otherwise rewrite the countdown DOM every
// second. FNV over the fields the wire carries is enough — a changed
// snapshot always changes one of them.
func groupHash(groups []store.GroupSnapshot) uint64 {
	h := fnv.New64a()
	var scratch [8]byte
	put := func(n int64) {
		binary.LittleEndian.PutUint64(scratch[:], uint64(n))
		h.Write(scratch[:])
	}
	for _, g := range groups {
		h.Write([]byte(g.GroupID))
		h.Write([]byte(g.Anchor))
		h.Write([]byte(g.Status))
		put(g.Deadline.UnixNano())
		put(g.OpenedAt.UnixNano())
		put(g.LastEvent.UnixNano())
		put(int64(g.EventCount))
	}
	return h.Sum64()
}

func writeJSON(w http.ResponseWriter, v any) {
	SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// handleBriefings serves one cursor page of briefings in ascending seq
// order: ?after=<seq> selects newer rows (reconnect gap-fill),
// ?before=<seq> pages older history, ?limit=N caps the page (default 50,
// max 500). No params returns the latest page. Non-numeric or negative
// cursors are 400 — Sscanf-style silent truncation is refused (Step 16).
func (v *Viewer) handleBriefings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, err := parseCursor(q.Get("after"))
	if err != nil {
		writeViewerError(w, "invalid after", http.StatusBadRequest)
		return
	}
	before, err := parseCursor(q.Get("before"))
	if err != nil {
		writeViewerError(w, "invalid before", http.StatusBadRequest)
		return
	}
	limit := 50
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 || n > 500 {
			writeViewerError(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
	rows, err := v.store.BriefingsPage(r.Context(), after, before, limit)
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

// parseCursor parses an optional seq cursor: empty means unbounded, and
// anything else must be a non-negative integer. "12abc" is rejected.
func parseCursor(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid cursor %q", s)
	}
	return int64(n), nil
}

// handleBriefing serves one briefing by ?id= (briefing or group ID)
// with its persisted profile links, referenced event index, and the
// retained observations for exactly those events. Every association
// comes from stored link rows; nothing is re-matched from the host
// string, and a profile's latest observation is never substituted for
// an older event's retained view.
func (v *Viewer) handleBriefing(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeViewerError(w, "missing id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	// Browser links use the stored briefing ID. Keep group IDs as an API
	// alias, but only fall back on an actual miss, never on a store failure.
	b, err := v.store.BriefingByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		b, err = v.store.BriefingForGroup(ctx, id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeViewerError(w, "briefing not found", http.StatusNotFound)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	view := viewOf(b)
	subjects, subjectsTruncated, err := v.store.BriefingSubjects(ctx, b.BriefingID)
	if err != nil {
		writeError(w, err)
		return
	}
	view.Profiles = make([]briefingSubjectWire, 0, len(subjects))
	for _, s := range subjects {
		view.Profiles = append(view.Profiles, briefingSubjectWire{ProfileID: s.ProfileID, Role: s.Role})
	}
	view.ProfilesTruncated = subjectsTruncated
	refs, refsTruncated, err := v.store.BriefingReferencedEvents(ctx, b.GroupID)
	if err != nil {
		writeError(w, err)
		return
	}
	view.ReferencedEvents = make([]referencedEventWire, 0, len(refs))
	eventIDs := make([]string, 0, len(refs))
	for _, ref := range refs {
		wire := referencedEventWire{EventID: ref.EventID, EventType: ref.EventType}
		if !ref.OccurredAt.IsZero() {
			wire.OccurredAt = ref.OccurredAt.UTC().Format(time.RFC3339)
		}
		if !ref.ReceivedAt.IsZero() {
			wire.ReceivedAt = ref.ReceivedAt.UTC().Format(time.RFC3339)
		}
		wire.Target = ref.Target
		wire.TargetTruncated = ref.TargetTruncated
		view.ReferencedEvents = append(view.ReferencedEvents, wire)
		eventIDs = append(eventIDs, ref.EventID)
	}
	view.ReferencedTruncated = refsTruncated
	obs, obsTruncated, err := v.store.ObservationsForEvents(ctx, eventIDs)
	if err != nil {
		writeError(w, err)
		return
	}
	view.Observations = make([]observationWire, 0, len(obs))
	for _, o := range obs {
		wire := observationWire{
			EventID: o.EventID, ViewRole: o.ViewRole,
			ReportedAt: o.ReportedAt.UTC().Format(time.RFC3339),
			ReceivedAt: o.ReceivedAt.UTC().Format(time.RFC3339),
			Attributes: o.Attributes,
			PortsTotal: o.PortsTotal, PortsTruncated: o.PortsTruncated,
			ProfileID: o.ProfileID, ReferenceClass: o.ReferenceClass, SourceScope: o.SourceScope,
		}
		view.Observations = append(view.Observations, wire)
	}
	view.ObservationsTruncated = obsTruncated
	// The investigator trace rides the detail fetch only: the executed
	// tool steps in 0-based order, tokenized like every provider-bound
	// surface. An empty trace stays absent, never null. Outcome/Summary
	// are the server's bounded display classification; raw fields ride
	// untouched beside them.
	traces, err := v.store.AgentTraces(ctx, b.BriefingID)
	if err != nil {
		writeError(w, err)
		return
	}
	for _, tr := range traces {
		outcome, summary := traceOutcome(tr.Tool, tr.Status, tr.ResultJSON)
		view.Traces = append(view.Traces, traceWire{
			Step: tr.Step, Tool: tr.Tool, Args: tr.ArgsJSON,
			Result: tr.ResultJSON, Status: tr.Status, DurationMS: tr.DurationMS,
			Outcome: outcome, Summary: summary,
		})
	}
	// Operator decisions ride the subject view so the dossier shows
	// live review state and the audit trail without a second fetch;
	// the raw list endpoint stays for API parity and the CLI.
	trail, err := v.store.DecisionsForSubject(ctx, store.DecisionSubjectBriefing, b.BriefingID, 50, 0, 0)
	if err != nil {
		writeError(w, err)
		return
	}
	view.Decisions = make([]decisionWire, 0, len(trail))
	for _, d := range trail {
		view.Decisions = append(view.Decisions, decisionWireOf(d))
	}
	if eff, err := v.store.EffectiveReview(ctx, store.DecisionSubjectBriefing, []string{b.BriefingID}, time.Now().UTC()); err != nil {
		writeError(w, err)
		return
	} else if state, ok := eff[b.BriefingID]; ok {
		wire := reviewWireOf(state)
		view.EffectiveReview = &wire
	}
	writeJSON(w, view)
}

// deviceWire is one reported subject: class, provenance and association
// status are explicit, and identity fields are the scanner's reported
// values — never inferred replacements. Times are RFC3339 like the
// briefing view; cursors stay numeric and tuple-shaped.
type deviceWire struct {
	ProfileID          string            `json:"profile_id"`
	Class              string            `json:"class"`
	SourceScope        string            `json:"source_scope"`
	Namespace          string            `json:"namespace"`
	ReportedID         string            `json:"reported_id"`
	ReportedSource     string            `json:"reported_source"`
	ReportedConfidence string            `json:"reported_confidence"`
	AssociationStatus  string            `json:"association_status"`
	Associations       map[string]int64  `json:"associations"`
	OperatorLabel      *string           `json:"operator_label"`
	FirstReportAt      string            `json:"first_report_at"`
	LastReportAt       string            `json:"last_report_at"`
	Latest             *observationWire  `json:"latest"`
	History            []observationWire `json:"history,omitempty"`
	HistoryNextAfter   string            `json:"history_next_after,omitempty"`
	// Decisions is the newest-first audit trail with undone markers;
	// the materialized label head stays in operator_label above.
	Decisions []decisionWire `json:"decisions"`
}

// observationWire is one retained event view: the bounded attributes
// snapshot plus, for the latest-evidence section only, the retained
// port list. History items carry no per-item port list; the snapshot's
// ports_total/ports_truncated says what exists. The detail fetch adds
// the exact persisted association for the view; profile/history/list
// payloads leave them absent.
type observationWire struct {
	ObservationID  int64          `json:"observation_id"`
	EventID        string         `json:"event_id"`
	ViewRole       string         `json:"view_role"`
	ReportedAt     string         `json:"reported_at"`
	ReceivedAt     string         `json:"received_at"`
	Attributes     map[string]any `json:"attributes"`
	Ports          []portWire     `json:"ports,omitempty"`
	PortsTotal     int64          `json:"ports_total"`
	PortsTruncated bool           `json:"ports_truncated"`
	ProfileID      string         `json:"profile_id,omitempty"`
	ReferenceClass string         `json:"reference_class,omitempty"`
	SourceScope    string         `json:"source_scope,omitempty"`
}

// portWire is one retained port state.
type portWire struct {
	Proto   string `json:"proto"`
	Port    int    `json:"port"`
	State   string `json:"state"`
	Service string `json:"service"`
}

// devicesEnvelope is the compatibility list envelope: the subject page
// plus the tuple cursor for the next one, "" when exhausted.
type devicesEnvelope struct {
	Devices   []deviceWire `json:"devices"`
	NextAfter string       `json:"next_after"`
}

// handleDevices serves GET /api/devices?limit&after: one page of reported
// subjects oldest-first. limit defaults to 50 and 200 is refused, not
// clamped; after is "<last_report_at nanos>:<profile id>" ("" starts at
// the oldest). Malformed cursors are 400, including a well-formed shape
// with a non-profile ID suffix; a well-shaped cursor for an unknown or
// deleted profile simply pages past retained rows to an empty page.
func (v *Viewer) handleDevices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 || n > 200 {
			writeViewerError(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
	var afterNanos int64
	var afterID string
	if s := q.Get("after"); s != "" {
		var err error
		afterNanos, afterID, err = parseTupleCursor(s)
		if err != nil || !validProfileID(afterID) {
			writeViewerError(w, "invalid after", http.StatusBadRequest)
			return
		}
	}
	rows, nextAfter, err := v.store.ReportedSubjectsPage(r.Context(), afterNanos, afterID, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]deviceWire, 0, len(rows))
	for _, sub := range rows {
		out = append(out, deviceWireOf(sub, nil, nil, ""))
	}
	writeJSON(w, devicesEnvelope{Devices: out, NextAfter: nextAfter})
}

// handleDevice serves GET /api/devices/{id}: one reported profile with
// its latest retained evidence and one independently paged history
// window. Unknown IDs are 404; malformed IDs and cursors are 400.
// history_limit defaults to 50 and is refused above 50.
func (v *Viewer) handleDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validProfileID(id) {
		writeViewerError(w, "invalid id", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	historyLimit := 50
	if s := q.Get("history_limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 || n > 50 {
			writeViewerError(w, "invalid history_limit", http.StatusBadRequest)
			return
		}
		historyLimit = n
	}
	var historyAfterNanos, historyAfterID int64
	var historyAfterStr string
	if s := q.Get("history_after"); s != "" {
		var err error
		historyAfterNanos, historyAfterStr, err = parseTupleCursor(s)
		if err != nil {
			writeViewerError(w, "invalid history_after", http.StatusBadRequest)
			return
		}
		var idErr error
		historyAfterID, idErr = strconv.ParseInt(historyAfterStr, 10, 64)
		if idErr != nil || historyAfterID < 0 {
			writeViewerError(w, "invalid history_after", http.StatusBadRequest)
			return
		}
	}
	sub, err := v.store.ReportedProfile(r.Context(), id)
	if err != nil {
		if err == sql.ErrNoRows {
			writeViewerError(w, "unknown profile", http.StatusNotFound)
			return
		}
		writeError(w, err)
		return
	}
	var latest *observationWire
	if obs, ok, err := v.store.LatestSubjectObservation(r.Context(), id); err != nil {
		writeError(w, err)
		return
	} else if ok {
		wire := observationWireOf(obs)
		latest = &wire
	}
	history, historyNext, err := v.store.SubjectHistoryPage(r.Context(), id, historyAfterNanos, historyAfterID, historyLimit)
	if err != nil {
		writeError(w, err)
		return
	}
	historyWire := make([]observationWire, 0, len(history))
	for _, obs := range history {
		historyWire = append(historyWire, observationWireOf(obs))
	}
	trail, err := v.store.DecisionsForSubject(r.Context(), store.DecisionSubjectProfile, id, 50, 0, 0)
	if err != nil {
		writeError(w, err)
		return
	}
	decisions := make([]decisionWire, 0, len(trail))
	for _, d := range trail {
		decisions = append(decisions, decisionWireOf(d))
	}
	wire := deviceWireOf(sub, latest, historyWire, historyNext)
	wire.Decisions = decisions
	writeJSON(w, wire)
}

// parseTupleCursor splits "<nanos>:<id>" and validates both halves.
// Profile IDs are prof-<hex> and observation IDs are decimal rowids;
// anything else is malformed, never a lookup miss.
func parseTupleCursor(s string) (int64, string, error) {
	nanosPart, id, ok := strings.Cut(s, ":")
	if !ok || nanosPart == "" || id == "" {
		return 0, "", fmt.Errorf("malformed cursor %q", s)
	}
	nanosValue, err := strconv.ParseInt(nanosPart, 10, 64)
	if err != nil || nanosValue < 0 {
		return 0, "", fmt.Errorf("malformed cursor %q", s)
	}
	return nanosValue, id, nil
}

// validProfileID allowlists locally generated profile IDs: prof- plus
// 16 lowercase hex digits. Anything else is malformed (400), never a
// store lookup, so unknown schemes cannot probe the table.
func validProfileID(id string) bool {
	if len(id) != len("prof-")+16 || !strings.HasPrefix(id, "prof-") {
		return false
	}
	for _, c := range id[len("prof-"):] {
		if c < '0' || (c > '9' && c < 'a') || c > 'f' {
			return false
		}
	}
	return true
}

func deviceWireOf(sub store.ReportedSubject, latest *observationWire, history []observationWire, historyNext string) deviceWire {
	return deviceWire{
		ProfileID: sub.ProfileID, Class: sub.ReferenceClass, SourceScope: sub.SourceScope,
		Namespace: sub.IdentityNamespace, ReportedID: sub.UpstreamReference,
		ReportedSource: sub.ReportedIdentitySource, ReportedConfidence: sub.ReportedConfidence,
		AssociationStatus: sub.AssociationStatus,
		Associations:      map[string]int64{"groups": sub.Groups, "briefings": sub.Briefings},
		OperatorLabel:     sub.OperatorLabel,
		FirstReportAt:     sub.FirstReportAt.UTC().Format(time.RFC3339),
		LastReportAt:      sub.LastReportAt.UTC().Format(time.RFC3339),
		Latest:            latest, History: history, HistoryNextAfter: historyNext,
	}
}

func observationWireOf(obs store.SubjectObservation) observationWire {
	ports := make([]portWire, 0, len(obs.Ports))
	for _, p := range obs.Ports {
		ports = append(ports, portWire{Proto: p.Proto, Port: p.Port, State: p.State, Service: p.Service})
	}
	return observationWire{
		ObservationID: obs.ObservationID, EventID: obs.EventID, ViewRole: obs.ViewRole,
		ReportedAt: obs.ReportedAt.UTC().Format(time.RFC3339), ReceivedAt: obs.ReceivedAt.UTC().Format(time.RFC3339),
		Attributes: obs.Attributes, Ports: ports,
		PortsTotal: obs.PortsTotal, PortsTruncated: obs.PortsTruncated,
	}
}

// handleGroups serves the current group snapshot: open, closed (queued)
// and processing rows.
func (v *Viewer) handleGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := v.store.OpenGroupSnapshots(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, groupWireOf(time.Now().UTC(), v.GroupWindowSeconds, groups))
}

// Fanout builds the briefing worker callbacks over hub and publisher:
// stored briefings broadcast as `briefing` SSE events (id: seq, for
// Last-Event-ID resume) and enqueue for ScanRelay; group snapshots
// broadcast as `group`. Every path is non-blocking (hub skips slow
// subscribers, publisher drops on a full queue), so the worker never
// stalls on its observers. Either may be nil.
func Fanout(h *Hub, p *Publisher, windowSeconds int64) (func(store.Briefing), func([]store.GroupSnapshot)) {
	onBriefing := func(b store.Briefing) {
		if h != nil {
			id := ""
			if b.Seq > 0 {
				id = strconv.FormatInt(b.Seq, 10)
			}
			h.BroadcastID("briefing", id, viewOf(b))
		}
		if p != nil {
			p.Enqueue(b)
		}
	}
	var lastGroups uint64
	var firstGroups = true
	onGroups := func(groups []store.GroupSnapshot) {
		if h != nil {
			if sum := groupHash(groups); firstGroups || sum != lastGroups {
				firstGroups = false
				lastGroups = sum
				h.Broadcast("group", groupWireOf(time.Now().UTC(), windowSeconds, groups))
			}
		}
	}
	return onBriefing, onGroups
}

func writeError(w http.ResponseWriter, err error) {
	// Store errors carry IDs and constraints, never raw values; still,
	// only the kind crosses into HTTP responses.
	log.Printf("viewer: %s", store.SanitizeErrorText(err.Error()))
	writeViewerError(w, "lookup failed", http.StatusInternalServerError)
}
