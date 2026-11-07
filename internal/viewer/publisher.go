package viewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ostefani/watcherbridge/internal/forwarder"
	"github.com/ostefani/watcherbridge/internal/store"
)

// annotation is the ScanRelay §3.1 contract: the bridge's briefing, in
// restored local form (ScanRelay already holds the raw events, so this
// local-to-local post carries no redaction boundary), idempotent by
// briefing_id so retries are safe no-ops server-side.
type annotation struct {
	BriefingID       string   `json:"briefing_id"`
	EventIDs         []string `json:"event_ids"`
	HostID           string   `json:"host_id"`
	Baseline         string   `json:"baseline"`
	ModelSuggestion  *string  `json:"model_suggestion"`
	Priority         string   `json:"priority"`
	PriorityReason   string   `json:"priority_reason"`
	Clamped          bool     `json:"clamped"`
	WhatChanged      string   `json:"what_changed"`
	Context          string   `json:"context"`
	RecommendedCheck string   `json:"recommended_check"`
	CreatedAt        string   `json:"created_at"`
}

func annotationOf(b store.Briefing) annotation {
	return annotation{
		BriefingID: b.BriefingID, EventIDs: append([]string(nil), b.EventIDs...),
		HostID: b.HostAnchorKey, Baseline: b.Baseline, ModelSuggestion: b.ModelSuggestion,
		Priority: b.Priority, PriorityReason: b.PriorityReason, Clamped: b.Clamped,
		WhatChanged: b.WhatChanged, Context: b.Context, RecommendedCheck: b.RecommendedCheck,
		CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// Publisher posts briefings to ScanRelay asynchronously. It never blocks
// the worker: Enqueue drops (and counts) when the bounded queue is full,
// and the background goroutine treats every ScanRelay failure — refused,
// timeout, 404, 5xx — as a soft event: log the group ID and continue.
// Local inserts and SSE never wait on it.
type Publisher struct {
	endpoint  string
	bearer    string
	apiKey    string
	keyHeader string
	client    *http.Client
	queue     chan store.Briefing
	stop      context.CancelFunc
	done      chan struct{}
	posted    atomic.Int64
	dropped   atomic.Int64
	failed    atomic.Int64
	retryBase time.Duration // backoff unit; production default 1s, tests shrink it
}

// publisherQueueCap bounds memory fed by the worker; overflow drops the
// newest briefing (order barely matters: one briefing per group) and the
// drop counter plus a log line keep the loss visible, never silent.
const publisherQueueCap = 64

// maxPostAttempts bounds the per-briefing retry: idempotent by
// briefing_id, so redelivery is safe, but ScanRelay outages must not turn
// the queue into an unbounded backlog.
const maxPostAttempts = 3

// NewPublisher returns a stopped Publisher. client may be nil (default
// client with timeout); tests inject a stub transport. An empty bearer and
// key mean ScanRelay takes no auth; empty keyHeader defaults to X-API-Key.
func NewPublisher(endpoint, bearer, apiKey, keyHeader string, timeout time.Duration, client *http.Client) *Publisher {
	if keyHeader == "" {
		keyHeader = "X-API-Key"
	}
	if client == nil {
		client = &http.Client{Timeout: timeout, CheckRedirect: noRedirectClient}
	}
	return &Publisher{
		endpoint: endpoint, bearer: bearer, apiKey: apiKey, keyHeader: keyHeader,
		client: client, queue: make(chan store.Briefing, publisherQueueCap), done: make(chan struct{}),
		retryBase: time.Second,
	}
}

// noRedirectClient refuses redirects: a 3xx from ScanRelay is a failure,
// not a new destination for briefing content.
func noRedirectClient(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// Start drains the queue until Stop. Run at most once; nil-safe callers
// simply never start it (ScanRelay unconfigured).
func (p *Publisher) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	p.stop = cancel
	go func() {
		defer close(p.done)
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-p.queue:
				p.post(ctx, b)
			}
		}
	}()
}

// Stop waits for the worker to exit. Queued-but-unsent briefings are
// dropped: local briefings are intact, and ScanRelay convergence is
// best-effort by design.
func (p *Publisher) Stop() {
	if p.stop == nil {
		return
	}
	p.stop()
	<-p.done
}

// Enqueue schedules one briefing without ever blocking the caller.
func (p *Publisher) Enqueue(b store.Briefing) {
	select {
	case p.queue <- b:
	default:
		p.dropped.Add(1)
		log.Printf("publisher: queue full, dropping annotation for group %s", b.GroupID)
	}
}

// Posted, Dropped, and Failed report delivery outcomes.
func (p *Publisher) Posted() int64  { return p.posted.Load() }
func (p *Publisher) Dropped() int64 { return p.dropped.Load() }
func (p *Publisher) Failed() int64  { return p.failed.Load() }

func (p *Publisher) post(ctx context.Context, b store.Briefing) {
	raw, err := json.Marshal(annotationOf(b))
	if err != nil {
		log.Printf("publisher: group %s: encode failed", b.GroupID)
		return
	}
	var last error
	for attempt := 0; attempt < maxPostAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * p.retryBase):
			}
		}
		last = p.postOnce(ctx, raw)
		if last == nil {
			p.posted.Add(1)
			return
		}
	}
	// Every failure mode — refused, timeout, 404, 5xx — lands here: soft
	// by contract, with the group ID as the only identifier in the log.
	p.failed.Add(1)
	log.Printf("publisher: group %s: giving up: %s", b.GroupID, last)
}

func (p *Publisher) postOnce(ctx context.Context, raw []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(raw))
	if err != nil {
		return errors.New("build annotation request")
	}
	req.Header.Set("Content-Type", "application/json")
	// Same check order as the proxy forwarder: bearer first, then key.
	if p.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+p.bearer)
	} else if p.apiKey != "" {
		req.Header.Set(p.keyHeader, p.apiKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return errors.New(forwarder.TransportFailure(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("scanrelay status %d", resp.StatusCode)
	}
	return nil
}
