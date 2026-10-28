// Package server is WatcherBridge's ingest endpoint: HMAC-authenticated
// receiver with a bounded proxy forwarder to ScanRelay (Designv5 §4).
//
// Contract (statuses mirror ScanRelay's /webhook where they overlap):
// 202 accepted (persisted; forwarded in proxy mode), 401 bad signature or
// timestamp, 400 malformed, 413 oversize, 405 non-POST, 503 saturated with
// Retry-After, 502 ScanRelay delivery failed. A duplicate of a known event is
// idempotent (202 when forwarded, 502 when not) — never a 401/409.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ostefani/watcherbridge/internal/config"
	"github.com/ostefani/watcherbridge/internal/forwarder"
	"github.com/ostefani/watcherbridge/internal/store"
)

// replayBody is ScanRelay's exact replay response (json.Encoder adds the
// trailing newline; comparison trims it). Only this exact shape marks an
// event forwarded without a 2xx.
const replayBody = `{"error":"replayed request"}`

// eventTypes is the allowlist of Pro webhook event types (Designv5 §4).
var eventTypes = map[string]bool{
	"host.appeared":    true,
	"host.disappeared": true,
	"host.changed":     true,
}

// Server serves the ingest endpoint and runs the startup sweep.
type Server struct {
	cfg    config.Config
	secret string
	events *store.Store
	gate   *forwarder.Gate
	now    func() time.Time

	sem   chan struct{} // in-flight cap; acquired before reading the body
	proxy *http.Client  // never follows redirects; bounded by proxy timeout

	http *http.Server

	sweepCancel context.CancelFunc
	sweepDone   chan struct{}
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
	now := time.Now
	proxy := &http.Client{
		Timeout: cfg.ProxyForwardTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 3xx is a forward failure, not a hop
		},
	}
	s := &Server{
		cfg: cfg, secret: secret, events: events, gate: gate, now: now,
		sem: make(chan struct{}, cfg.InFlightCap), proxy: proxy,
		sweepDone: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", s.handleWebhook)
	s.http = &http.Server{
		Addr:              cfg.Bind,
		Handler:           mux,
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

// Start serves ingest and runs the startup sweep on one background worker.
// It blocks until the listener fails or Shutdown is called.
func (s *Server) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	s.sweepCancel = cancel
	go func() {
		defer close(s.sweepDone)
		s.sweep(ctx)
	}()
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown cancels the sweeper first, waits for it and for in-flight
// handlers, then returns. The caller closes the store after Shutdown
// returns so a sweep forward in flight can still commit its receipt.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.sweepCancel != nil {
		s.sweepCancel()
	}
	err := s.http.Shutdown(ctx)
	select {
	case <-s.sweepDone:
	case <-ctx.Done():
	}
	return err
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		w.Header().Set("Retry-After", strconv.Itoa(s.cfg.RetryAfterSecs))
		writeError(w, http.StatusServiceUnavailable, "server saturated")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.HandlerDeadline)
	defer cancel()

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
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
		var malformed *malformedError
		if errors.As(err, &malformed) {
			writeError(w, http.StatusBadRequest, malformed.Error())
		} else {
			writeError(w, http.StatusUnauthorized, err.Error())
		}
		return
	}
	if !validSignature(s.secret, timestamp, eventID, eventKey, eventType, body, signature) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	release, wait := s.gate.Acquire(eventID)
	if wait != nil {
		// Duplicate while the first handler is in flight: wait for its
		// outcome, then follow the stored state. Waiting caps memory at
		// one waiter per event instead of one body copy each.
		select {
		case <-wait:
		case <-ctx.Done():
			writeError(w, http.StatusServiceUnavailable, "duplicate wait timed out")
			return
		}
		s.replyForStoredState(ctx, w, eventID, eventType)
		return
	}
	defer release()

	ev, perr := s.persist(ctx, body, r, eventID, eventKey, eventType, sigTime)
	if perr != nil {
		if errors.Is(perr, errDuplicate) {
			// Post-completion retry (no holder in flight: we ARE the
			// holder). If an earlier attempt already landed, accept;
			// otherwise forward inline — this is Pro's outbox retry
			// doing its job. In-flight duplicates never reach here:
			// they wait on the gate and follow the stored state.
			if fwd, _, _, err := s.events.ForwardState(ctx, eventID); err == nil && fwd {
				writeAccepted(w, eventID, eventType)
				return
			}
			if s.cfg.Mode == config.ModeTap || s.forwardOne(ctx, body, headerMap(r.Header), eventID) {
				writeAccepted(w, eventID, eventType)
				return
			}
			writeError(w, http.StatusBadGateway, "scanrelay delivery failed")
			return
		}
		// A cancelled persist is "unknown, retry safely": Pro's outbox
		// redelivers and the retry reports Inserted=false.
		if errors.Is(perr, context.Canceled) || errors.Is(perr, context.DeadlineExceeded) {
			writeError(w, http.StatusServiceUnavailable, "persist interrupted")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid event")
		return
	}
	_ = ev

	if s.cfg.Mode == config.ModeTap {
		writeAccepted(w, eventID, eventType)
		return
	}
	if s.forwardOne(ctx, body, headerMap(r.Header), eventID) {
		writeAccepted(w, eventID, eventType)
		return
	}
	writeError(w, http.StatusBadGateway, "scanrelay delivery failed")
}

var errDuplicate = errors.New("duplicate event")

// persist stores the raw event and joins its host group in one transaction.
// Inserted=false surfaces as errDuplicate so the caller follows §4.3.
func (s *Server) persist(ctx context.Context, body []byte, r *http.Request, eventID, eventKey, eventType string, sigTime time.Time) (store.RawEvent, error) {
	pctx, cancel := context.WithTimeout(ctx, s.cfg.PersistBudget)
	defer cancel()

	headers := map[string]string{}
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	headersJSON, err := json.Marshal(store.WhitelistForwardHeaders(headers))
	if err != nil {
		return store.RawEvent{}, err
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
		return store.RawEvent{}, err
	}
	ev := store.RawEvent{
		EventID:            eventID,
		EventKey:           eventKey,
		EventType:          eventType,
		OccurredAt:         decoded.OccurredAt,
		Cycle:              decoded.Cycle,
		Target:             decoded.Target,
		Payload:            bytes.Clone(body),
		HeadersJSON:        string(headersJSON),
		ReceivedAt:         s.now().UTC(),
		SignatureTimestamp: sigTime,
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = ev.ReceivedAt
	}
	res, err := s.events.PersistEventAndGroup(pctx, ev, eventKey, store.GroupPolicy{
		QuietWindow: s.cfg.GroupQuietWindow,
		MaxWindow:   s.cfg.GroupMaxWindow,
		MaxEvents:   s.cfg.GroupMaxEvents,
	})
	if err != nil {
		return store.RawEvent{}, err
	}
	if !res.Inserted {
		return store.RawEvent{}, errDuplicate
	}
	return ev, nil
}

// replyForStoredState answers a duplicate from the forwarded flag: 202 when
// the event landed at ScanRelay, 502 when it did not so Pro retries. It never
// forwards inline — a second forward inside this deadline could exceed it.
func (s *Server) replyForStoredState(ctx context.Context, w http.ResponseWriter, eventID, eventType string) {
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

// forwardOne forwards one event's original bytes with its original headers
// and records the receipt. It reports whether the event counts as delivered.
func headerMap(src http.Header) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

func (s *Server) forwardOne(ctx context.Context, body []byte, src map[string]string, eventID string) bool {
	fctx, cancel := context.WithTimeout(ctx, s.cfg.ProxyForwardTimeout)
	defer cancel()

	out, err := http.NewRequestWithContext(fctx, http.MethodPost, strings.TrimSuffix(s.cfg.ScanRelayURL, "/")+"/webhook", bytes.NewReader(body))
	if err != nil {
		_ = s.events.RecordForwardFailure(context.WithoutCancel(ctx), eventID, "build forward request")
		return false
	}
	for k, v := range src {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-subnetlens-") || lk == "content-type" {
			out.Header.Set(k, v)
		}
	}
	resp, err := s.proxy.Do(out)
	if err != nil {
		_ = s.events.RecordForwardFailure(context.WithoutCancel(ctx), eventID, err.Error())
		return false
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		_ = s.events.RecordForwardFailure(context.WithoutCancel(ctx), eventID, "read forward response")
		return false
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		_, _ = s.events.MarkForwarded(context.WithoutCancel(ctx), eventID, s.now().UTC())
		return true
	case resp.StatusCode == http.StatusConflict && strings.TrimSpace(string(respBody)) == replayBody:
		// ScanRelay proves an earlier attempt landed (e.g. the bridge
		// crashed after accept but before the flag write).
		_, _ = s.events.MarkForwarded(context.WithoutCancel(ctx), eventID, s.now().UTC())
		return true
	default:
		_ = s.events.RecordForwardFailure(context.WithoutCancel(ctx), eventID, "scanrelay status "+resp.Status)
		return false
	}
}
