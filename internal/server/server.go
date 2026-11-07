// Package server is WatcherBridge's ingest endpoint: HMAC-authenticated
// receiver with a bounded proxy forwarder to ScanRelay (Designv5 §4).
//
// Contract (statuses mirror ScanRelay's /webhook where they overlap):
// 202 accepted (persisted; forwarded in proxy mode), 401 bad signature or
// timestamp, 400 malformed, 413 oversize, 405 non-POST, 503 saturated or
// store unavailable with Retry-After, 502 ScanRelay delivery failed.
// A duplicate host event is idempotent (202 when forwarded, 502 when not).
// H1 is persisted separately and never forwarded; identical retries are 202,
// while conflicting reuse of a heartbeat ID is 409.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ostefani/watcherbridge/internal/config"
	"github.com/ostefani/watcherbridge/internal/forwarder"
	"github.com/ostefani/watcherbridge/internal/health"
	"github.com/ostefani/watcherbridge/internal/store"
	"github.com/ostefani/watcherbridge/internal/viewer"
)

// replayBody is ScanRelay's exact replay response (json.Encoder adds the
// trailing newline; comparison trims it). Only this exact shape marks an
// event forwarded without a 2xx.
const replayBody = `{"error":"replayed request"}`

// forwardRespCap bounds the forwarded-response read: status plus the small
// replay body are all the forwarder ever needs.
const forwardRespCap = 8 << 10

// eventTypes is the allowlist of Pro webhook event types (Designv5 §4).
// Pro generates UUID-like IDs (hex and '-'); the charset below matches that
// generator exactly rather than guessing wider.
var eventTypes = map[string]bool{
	"host.appeared":    true,
	"host.disappeared": true,
	"host.changed":     true,
	"scan.heartbeat":   true,
}

// Server serves the ingest endpoint and runs the startup sweep. The
// viewer (UI, JSON, SSE) shares the same loopback mux; the annotation
// publisher is composed in main over viewer.Fanout.
type Server struct {
	cfg    config.Config
	secret string
	events *store.Store
	gate   *forwarder.Gate
	viewer *viewer.Viewer
	// Health counts ingest outcomes; main sets it after New (nil-safe),
	// and every ingest path below records its coarse outcome.
	Health *health.Counters
	now    func() time.Time

	sem   chan struct{} // in-flight cap; acquired before reading the body
	proxy *http.Client  // never follows redirects; bounded by proxy timeout

	http  *http.Server
	conns sync.WaitGroup
}

// New builds a Server. It resolves the secret, validates the config, and
// refuses to start unauthenticated or on a non-loopback bind.
func New(cfg config.Config, events *store.Store, gate *forwarder.Gate) (*Server, error) {
	if events == nil || gate == nil {
		return nil, fmt.Errorf("server requires a store and a forwarder gate")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	secret, err := cfg.ResolveSecret()
	if err != nil {
		return nil, err
	}
	proxy := &http.Client{
		Timeout: cfg.ProxyForwardTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 3xx is a forward failure, not a hop
		},
	}
	s := &Server{
		cfg: cfg, secret: secret, events: events, gate: gate, now: time.Now,
		sem: make(chan struct{}, cfg.InFlightCap), proxy: proxy,
	}
	s.viewer = viewer.New(events, nil)
	s.viewer.Heartbeat = cfg.SSEHeartbeat
	s.viewer.ReplayMax = cfg.SSEReplayMax
	s.viewer.WriteTimeout = cfg.SSEWriteTimeout
	s.viewer.MaxSubscribers = cfg.SSEMaxSubscribers
	s.viewer.GroupWindowSeconds = int64(cfg.GroupQuietWindow / time.Second)
	s.viewer.SnoozeDefault = cfg.SnoozeDefault
	s.viewer.RuleProposeAfter = cfg.RuleProposeAfter
	s.viewer.RuleDefaultTTL = cfg.RuleDefaultTTL
	hosts := cfg.ViewerAllowedHosts
	if len(hosts) == 0 {
		// Validate already refused non-loopback and malformed binds.
		_, port, err := net.SplitHostPort(cfg.Bind)
		if err != nil {
			return nil, fmt.Errorf("invalid bind %q: %w", cfg.Bind, err)
		}
		hosts = viewer.LoopbackHosts(port)
	}
	s.viewer.AllowedHosts = hosts
	// The overview refresher computes /api/overview, /api/health, and the
	// connect-time `overview` event from retained rows. Health and the
	// annotation publisher attach later in main once both exist.
	s.viewer.Overview = viewer.NewRefresher(events, nil, nil,
		cfg.OverviewWindow, cfg.OverviewMaxWindow, cfg.BriefingRetention)
	s.viewer.Overview.SetSubjectRetention(cfg.SubjectRetention)
	s.viewer.Overview.SetHeartbeatStaleAfter(cfg.HeartbeatStaleAfter)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", s.handleWebhook)
	// GET needs an explicit route: without it the viewer mount below
	// answers with its own 404 instead of the webhook's 405.
	mux.HandleFunc("GET /webhook", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	})
	// Every other method on the webhook path stays in webhook dispatch
	// (405), never falling through to the viewer mount.
	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	})
	// The viewer subtree carries its own Host gate (applied before its
	// mux-generated 405s and redirects); exact webhook patterns win over
	// the "/" mount, so /webhook keeps its HMAC behavior.
	mux.Handle("/", s.viewer.Handler())
	root := mux
	s.http = &http.Server{
		Addr: cfg.Bind,
		ConnState: func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				s.conns.Add(1)
			case http.StateClosed, http.StateHijacked:
				s.conns.Done()
			}
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer.SetSecurityHeaders(w)
			// Host policy applies before outer-mux canonicalization, so
			// a foreign Host cannot pull a redirect out of the router.
			// The webhook path is exempt and keeps HMAC dispatch for
			// every method. Headers are already set above, also for the
			// 403 itself.
			if r.URL.Path != "/webhook" && !s.viewer.AllowedHost(r.Host) {
				http.Error(w, "foreign host", http.StatusForbidden)
				return
			}
			root.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}
	return s, nil
}

// Handler exposes the mux for tests without binding a socket.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Viewer exposes the attached viewer (and its hub) so main can wire the
// briefing worker callbacks through viewer.Fanout.
func (s *Server) Viewer() *viewer.Viewer { return s.viewer }

// Start serves HTTP until the listener fails or Shutdown stops it.
// The caller runs the recovery sweep before serving.
func (s *Server) Start() error {
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown closes SSE streams and waits for the HTTP server to drain.
// If the grace period expires, close the remaining connections to cancel
// their request contexts.
func (s *Server) Shutdown(ctx context.Context) error {
	s.viewer.Close()
	err := s.http.Shutdown(ctx)
	if err != nil {
		err = errors.Join(err, s.http.Close())
	}
	// Shutdown joins the listener, so no more StateNew callbacks can add
	// to conns. Close interrupts sockets but does not join their handlers.
	s.conns.Wait()
	return err
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) writeSaturated(w http.ResponseWriter, msg string) {
	s.Health.AddSaturated()
	w.Header().Set("Retry-After", strconv.Itoa(s.cfg.RetryAfterSecs))
	writeError(w, http.StatusServiceUnavailable, msg)
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// Only POST reaches here ("POST /webhook" pattern; GET has its own
	// explicit 405 above, anything else mismatches in the mux).
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.writeSaturated(w, "server saturated")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.HandlerDeadline)
	defer cancel()

	bodyLimit := s.cfg.MaxBodyBytes
	if r.Header.Get("X-SubnetLens-Event-Type") == "scan.heartbeat" && bodyLimit > store.MaxHeartbeatBytes {
		bodyLimit = store.MaxHeartbeatBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.Health.AddRejected()
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "read request body")
		}
		return
	}

	eventID := r.Header.Get("X-SubnetLens-Event-ID")
	eventKey := r.Header.Get("X-SubnetLens-Event-Key")
	eventType := r.Header.Get("X-SubnetLens-Event-Type")
	timestamp := r.Header.Get("X-SubnetLens-Signature-Timestamp")
	signature := r.Header.Get("X-SubnetLens-Signature-256")
	sigTime, err := parseAndValidate(s.now(), s.cfg.TimestampTolerance, timestamp, eventID, eventKey, eventType, body)
	if err != nil {
		s.Health.AddRejected()
		var malformed *malformedError
		if errors.As(err, &malformed) {
			writeError(w, http.StatusBadRequest, malformed.Error())
		} else {
			writeError(w, http.StatusUnauthorized, err.Error())
		}
		return
	}
	if !validSignature(s.secret, timestamp, eventID, eventKey, eventType, body, signature) {
		s.Health.AddRejected()
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	if eventType == "scan.heartbeat" {
		s.acceptHeartbeat(ctx, w, body, eventID, eventKey, sigTime)
		return
	}

	// Metadata consistency (Designv9 §1.1) before grouping, persistence
	// or forwarding: the body's echoed envelope must equal the signed
	// headers. The heartbeat branch above enforces the same comparison
	// inside decodeHeartbeat; host events need it here.
	if err := checkEnvelopeConsistency(body, eventID, eventKey, eventType); err != nil {
		s.Health.AddRejected()
		var malformed *malformedError
		if errors.As(err, &malformed) {
			writeError(w, http.StatusBadRequest, malformed.Error())
		} else {
			writeError(w, http.StatusUnauthorized, err.Error())
		}
		return
	}

	release, wait := s.gate.Acquire(eventID)
	if wait != nil {
		// Duplicate while the holder is in flight: drop our body copy
		// so a crowd of waiters cannot pile up MaxBodyBytes each, then
		// follow the stored state — 202 when forwarded, 502 when not.
		// A waiter never forwards inline: a second forward inside this
		// deadline could exceed it.
		body = nil
		select {
		case <-wait:
		case <-ctx.Done():
			s.writeSaturated(w, "duplicate wait timed out")
			return
		}
		s.replyForStoredState(ctx, w, eventID, eventType)
		return
	}
	defer release()

	// Grouping keys are exact scoped upstream references, addresses
	// kept address-scoped, or the event itself when the report carries
	// no usable reference (Designv9 §1.5). No MAC/hostname/IP
	// preference heuristic runs here anymore.
	anchor, eventScoped := store.GroupKeyForEvent(s.cfg.SourceID, eventType, eventID, body)
	if eventScoped {
		log.Printf("ingest: event %s has no usable reference, event-local grouping", eventID)
	}
	if perr := s.persistAndReply(ctx, w, body, r, eventID, eventKey, eventType, anchor, sigTime); perr != nil {
		s.reportPersistError(w, eventID, perr)
	}
}

var errMalformedBody = errors.New("invalid event body")

// persistAndReply stores the event (or follows the duplicate path) and
// writes the response. A nil return means the response is written.
func (s *Server) persistAndReply(ctx context.Context, w http.ResponseWriter, body []byte, r *http.Request, eventID, eventKey, eventType, anchor string, sigTime time.Time) error {
	pctx, cancel := context.WithTimeout(ctx, s.cfg.PersistBudget)
	defer cancel()

	headersJSON, err := json.Marshal(store.WhitelistForwardHeaders(headerMap(r.Header)))
	if err != nil {
		return errMalformedBody
	}
	var decoded struct {
		EventID    string    `json:"event_id"`
		EventKey   string    `json:"event_key"`
		EventType  string    `json:"event_type"`
		OccurredAt time.Time `json:"occurred_at"`
		Cycle      int       `json:"cycle"`
		Target     string    `json:"target"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return errMalformedBody
	}
	ev := store.RawEvent{
		EventID:            eventID,
		EventKey:           eventKey,
		EventType:          eventType,
		OccurredAt:         decoded.OccurredAt,
		Cycle:              decoded.Cycle,
		Target:             decoded.Target,
		Payload:            body, // ReadAll-owned; the store keeps it, no second copy
		HeadersJSON:        string(headersJSON),
		ReceivedAt:         s.now().UTC(),
		SignatureTimestamp: sigTime,
		SourceScope:        s.cfg.SourceID,
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = ev.ReceivedAt
	}
	res, err := s.events.PersistEventAndGroup(pctx, ev, anchor, store.GroupPolicy{
		GroupingVersion: store.GroupingScoped,
		QuietWindow:     s.cfg.GroupQuietWindow,
		MaxWindow:       s.cfg.GroupMaxWindow,
		MaxEvents:       s.cfg.GroupMaxEvents,
	})
	if err != nil {
		return err
	}
	if !res.Inserted {
		// Post-completion retry (no holder in flight: we ARE the
		// holder). If an earlier attempt already landed, accept;
		// otherwise forward inline — this is Pro's outbox retry
		// doing its job. In-flight duplicates never reach here:
		// they wait on the gate and follow the stored state.
		s.Health.AddDuplicate()
		if fwd, _, _, ferr := s.events.ForwardState(ctx, eventID); ferr == nil && fwd {
			writeAccepted(w, eventID, eventType)
			return nil
		}
		if s.cfg.Mode == config.ModeTap || s.forwardOne(ctx, body, headerMap(r.Header), eventID) {
			writeAccepted(w, eventID, eventType)
			return nil
		}
		writeError(w, http.StatusBadGateway, "scanrelay delivery failed")
		return nil
	}

	if s.cfg.Mode == config.ModeTap {
		s.Health.AddAccepted()
		writeAccepted(w, eventID, eventType)
		return nil
	}
	if s.forwardOne(ctx, body, headerMap(r.Header), eventID) {
		s.Health.AddAccepted()
		writeAccepted(w, eventID, eventType)
		return nil
	}
	writeError(w, http.StatusBadGateway, "scanrelay delivery failed")
	return nil
}

// reportPersistError maps persist failures to statuses. Only pre-store
// validation is a 400; everything past it (busy, closed, saturated) is a
// 503 so Pro retries, and the failure is logged without request content.
func (s *Server) reportPersistError(w http.ResponseWriter, eventID string, err error) {
	if errors.Is(err, errMalformedBody) {
		s.Health.AddRejected()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("ingest: persist %s: %v", eventID, err)
	s.writeSaturated(w, "persist unavailable")
}

// replyForStoredState answers a duplicate from the forwarded flag: 202 when
// the event landed at ScanRelay, 502 when it did not so Pro retries. It never
// forwards inline — a second forward inside this deadline could exceed it.
func (s *Server) replyForStoredState(ctx context.Context, w http.ResponseWriter, eventID, eventType string) {
	s.Health.AddDuplicate()
	forwarded, _, _, err := s.events.ForwardState(ctx, eventID)
	if err != nil || !forwarded {
		writeError(w, http.StatusBadGateway, "scanrelay delivery failed")
		return
	}
	writeAccepted(w, eventID, eventType)
}

func writeAccepted(w http.ResponseWriter, eventID, eventType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"accepted": true, "event_id": eventID, "event_type": eventType,
	})
}

func headerMap(src http.Header) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// forwardOne forwards one event's original bytes with its original headers
// and records the receipt. It reports whether the event counts as delivered.
func (s *Server) forwardOne(ctx context.Context, body []byte, src map[string]string, eventID string) bool {
	fctx, cancel := context.WithTimeout(ctx, s.cfg.ProxyForwardTimeout)
	defer cancel()

	out, err := http.NewRequestWithContext(fctx, http.MethodPost, strings.TrimSuffix(s.cfg.ScanRelayURL, "/")+"/webhook", bytes.NewReader(body))
	if err != nil {
		s.recordFailure(ctx, eventID, "build forward request")
		return false
	}
	for k, v := range src {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-subnetlens-") || lk == "content-type" {
			out.Header.Set(k, v)
		}
	}
	// Forward credentials are injected here at send time, never stored:
	// the whitelist keeps them out of headers_json by construction.
	if s.cfg.ForwardBearerToken != "" {
		out.Header.Set("Authorization", "Bearer "+string(s.cfg.ForwardBearerToken))
	} else if s.cfg.ForwardAPIKey != "" {
		out.Header.Set(s.cfg.ForwardAPIKeyHeader, string(s.cfg.ForwardAPIKey))
	}
	resp, err := s.proxy.Do(out)
	if err != nil {
		s.recordFailure(ctx, eventID, err.Error())
		return false
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, forwardRespCap))
	if err != nil {
		s.recordFailure(ctx, eventID, "read forward response")
		return false
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		s.recordDelivered(ctx, eventID)
		return true
	case resp.StatusCode == http.StatusConflict && strings.TrimSpace(string(respBody)) == replayBody:
		// ScanRelay proves an earlier attempt landed (e.g. the bridge
		// crashed after accept but before the flag write).
		s.recordDelivered(ctx, eventID)
		return true
	default:
		s.recordFailure(ctx, eventID, "scanrelay status "+resp.Status)
		return false
	}
}

// receiptCtx bounds the receipt writes: without a deadline a stuck writer
// would hold the handler, the gate slot, and the in-flight slot past
// WriteTimeout. The margin is the ingest path's slack by definition.
func (s *Server) receiptCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Margin)
}

func (s *Server) recordDelivered(ctx context.Context, eventID string) {
	rctx, cancel := s.receiptCtx(ctx)
	defer cancel()
	if _, err := s.events.MarkForwarded(rctx, eventID, s.now().UTC()); err != nil {
		// Safe direction: a retry gets ScanRelay's replay shape and
		// heals the flag. Still loud, because silent 202s hide trouble.
		log.Printf("ingest: receipt write %s: %v", eventID, err)
	}
}

func (s *Server) recordFailure(ctx context.Context, eventID, reason string) {
	rctx, cancel := s.receiptCtx(ctx)
	defer cancel()
	if err := s.events.RecordForwardFailure(rctx, eventID, reason); err != nil {
		log.Printf("ingest: receipt write %s: %v", eventID, err)
	}
	log.Printf("ingest: forward %s failed: %s", eventID, reason)
}
