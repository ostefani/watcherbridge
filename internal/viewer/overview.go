package viewer

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ostefani/watcherbridge/internal/health"
	"github.com/ostefani/watcherbridge/internal/overview"
	"github.com/ostefani/watcherbridge/internal/store"
)

// healthWindow is the 1h health window for fallback ratio and forward
// errors (Designv9 §3.2: ratio over >=3 briefings in 1h).
const healthWindow = time.Hour

// attentionWire is one attention item on the wire. Raw identifiers stay
// server-side; only stable IDs, kinds, and priorities cross.
type attentionWire struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"` // briefing|incident
	Priority   string `json:"priority"`
	ChangeKind string `json:"change_kind"`
	CreatedAt  string `json:"created_at"`
	Status     string `json:"status"`
}

// sourceWire is one source's coverage and liveness with reasons.
// IdentityNamespace and Capabilities mirror the stored registration;
// both stay empty until the upstream identity work (Step 21) reports
// them, and empty reads as unknown, never as a granted capability.
// Heartbeat enablement and identity capability are different claims.
type sourceWire struct {
	Scope             string   `json:"scope"`
	Status            string   `json:"status"`
	Liveness          string   `json:"liveness"`
	IdentityNamespace string   `json:"identity_namespace,omitempty"`
	Capabilities      string   `json:"capabilities,omitempty"`
	LastReportAt      string   `json:"last_report_at,omitempty"`
	HeartbeatEnabled  bool     `json:"heartbeat_enabled"`
	LastHeartbeatAt   string   `json:"last_heartbeat_at,omitempty"`
	DeliveryStatus    string   `json:"delivery_status,omitempty"`
	Reasons           []string `json:"reasons"`
}

// overviewWire is the GET /api/overview body and the `overview` SSE
// payload. Verdict, headline, receipts, coverage/liveness with
// per-source reasons, attention, and digest share one computation.
type overviewWire struct {
	Verdict    string          `json:"verdict"`
	Headline   string          `json:"headline"`
	Attention  []attentionWire `json:"attention"`
	Digest     map[string]int  `json:"digest"`
	Receipts   int             `json:"receipts"`
	Window     string          `json:"window"`
	LastChange string          `json:"last_change,omitempty"`
	Coverage   string          `json:"coverage"`
	Liveness   string          `json:"liveness"`
	Sources    []sourceWire    `json:"sources"`
	Reasons    []string        `json:"reasons,omitempty"`
}

// aggregateLiveness folds per-source liveness into one top-level value:
// the shared value when sources agree, "mixed" when they differ, and
// "unknown" with no sources. Until H1 every source reads unknown, so
// the aggregate honestly reports unknown instead of implying presence.
func aggregateLiveness(sources []sourceWire) string {
	seen := map[string]bool{}
	for _, s := range sources {
		seen[s.Liveness] = true
	}
	switch len(seen) {
	case 1:
		for l := range seen {
			return l
		}
	case 0:
		return overview.LivenessUnknown
	}
	return "mixed"
}

// fallbackWire reports the 1h fallback share from retained rows.
type fallbackWire struct {
	Used  int     `json:"used"`
	Total int     `json:"total"`
	Ratio float64 `json:"ratio"`
}

// purgeWire reports per-table totals and the latest run.
type purgeWire struct {
	Totals      map[string]int64 `json:"totals"`
	Last        *purgeRunWire    `json:"last,omitempty"`
	LastSuccess *purgeRunWire    `json:"last_success,omitempty"`
}

type purgeRunWire struct {
	Status     string           `json:"status"`
	StartedAt  string           `json:"started_at"`
	FinishedAt string           `json:"finished_at,omitempty"`
	Rows       int64            `json:"rows_deleted"`
	Tables     map[string]int64 `json:"tables,omitempty"`
	Error      string           `json:"error,omitempty"`
}

func purgeRunWireOf(run store.PurgeRunHealth) *purgeRunWire {
	lw := &purgeRunWire{
		Status: run.Status, Rows: run.RowsDeleted, Tables: run.ByTable,
		StartedAt: run.StartedAt.UTC().Format(time.RFC3339),
	}
	if !run.FinishedAt.IsZero() {
		lw.FinishedAt = run.FinishedAt.UTC().Format(time.RFC3339)
	}
	if run.Error != "" {
		lw.Error = run.Error
	}
	return lw
}

// handleOverview serves the computed overview for the requested window.
// The query is validated with the shared chain (default 24h, configured
// maximum, window <= max <= briefing retention); invalid or oversized
// windows are 400, never clamped or widened silently.
func (v *Viewer) handleOverview(w http.ResponseWriter, r *http.Request) {
	ov := v.Overview
	if ov == nil {
		writeViewerError(w, "overview unavailable", http.StatusServiceUnavailable)
		return
	}
	window := ov.window
	if q := r.URL.Query().Get("window"); q != "" {
		d, err := time.ParseDuration(q)
		if err != nil {
			writeViewerError(w, "invalid window", http.StatusBadRequest)
			return
		}
		window = d
	}
	if _, err := overview.ValidateWindow(window, ov.maxWindow, ov.retention); err != nil {
		writeViewerError(w, "invalid window", http.StatusBadRequest)
		return
	}
	body, _, err := ov.compute(r.Context(), window)
	if err != nil {
		writeViewerError(w, "overview failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, json.RawMessage(body))
}

// handleHealth serves pipeline, source, provider, purge, and process
// health. Every reading is live: counters are process-lifetime with
// their start time exposed, durable evidence comes from retained rows.
func (v *Viewer) handleHealth(w http.ResponseWriter, r *http.Request) {
	ov := v.Overview
	if ov == nil {
		writeViewerError(w, "health unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()
	now := ov.now().UTC()
	healthSince := now.Add(-healthWindow)
	assessed, sources, err := ov.coverage(ctx)
	if err != nil {
		writeViewerError(w, "health failed", http.StatusInternalServerError)
		return
	}
	used, total, err := ov.store.FallbackShare(ctx, healthSince)
	if err != nil {
		writeViewerError(w, "health failed", http.StatusInternalServerError)
		return
	}
	forwardErrors, err := ov.store.RecentForwardErrors(ctx, healthSince)
	if err != nil {
		writeViewerError(w, "health failed", http.StatusInternalServerError)
		return
	}
	parked, err := ov.store.FailedGroups(ctx)
	if err != nil {
		writeViewerError(w, "health failed", http.StatusInternalServerError)
		return
	}
	totals, err := ov.store.PurgeTotals(ctx)
	if err != nil {
		writeViewerError(w, "health failed", http.StatusInternalServerError)
		return
	}
	lastRun, err := ov.store.LastPurgeRun(ctx)
	if err != nil {
		writeViewerError(w, "health failed", http.StatusInternalServerError)
		return
	}
	lastSuccess, err := ov.store.LastSuccessfulPurgeRun(ctx)
	if err != nil {
		writeViewerError(w, "health failed", http.StatusInternalServerError)
		return
	}
	hw := healthWire{
		Sources: sources, Coverage: assessed.Status, CoverageReasons: assessed.Reasons,
		Fallback:     fallbackWire{Used: used, Total: total},
		ParkedGroups: len(parked), ForwardErrors: forwardErrors,
		Purge:      purgeWire{Totals: totals},
		Process:    ov.health.Load().Snapshot(),
		QueueDepth: ov.store.QueueDepth(), QueueCap: ov.store.QueueCap(),
	}
	if ov.retention > 0 {
		hw.Retention = &retentionWire{ReportsSeconds: int64(ov.retention / time.Second)}
		if ov.subjectRetention > 0 {
			sub := int64(ov.subjectRetention / time.Second)
			hw.Retention.SubjectsSeconds = &sub
		}
	}
	if total > 0 {
		hw.Fallback.Ratio = float64(used) / float64(total)
	}
	if lastRun.HaveRun {
		hw.Purge.Last = purgeRunWireOf(lastRun)
	}
	if lastSuccess.HaveRun {
		hw.Purge.LastSuccess = purgeRunWireOf(lastSuccess)
	}
	if p := ov.publisher.Load(); p != nil {
		hw.Publisher = &publisherWire{Posted: p.Posted(), Dropped: p.Dropped(), Failed: p.Failed()}
	}
	writeJSON(w, hw)
}

// retentionWire states the configured retention policy in whole
// seconds: the report/briefing horizon and, when configured, the
// subject horizon. Policy alone never proves a per-profile boundary;
// the purge section carries the completed work separately.
type retentionWire struct {
	ReportsSeconds  int64  `json:"reports_seconds"`
	SubjectsSeconds *int64 `json:"subjects_seconds,omitempty"`
}

// healthWire is the GET /api/health body.
type healthWire struct {
	Sources         []sourceWire    `json:"sources"`
	Coverage        string          `json:"coverage"`
	CoverageReasons []string        `json:"coverage_reasons,omitempty"`
	Fallback        fallbackWire    `json:"fallback_1h"`
	ParkedGroups    int             `json:"parked_groups"`
	ForwardErrors   int             `json:"forward_errors_1h"`
	Purge           purgeWire       `json:"purge"`
	Retention       *retentionWire  `json:"retention,omitempty"`
	Publisher       *publisherWire  `json:"publisher,omitempty"`
	Process         health.Snapshot `json:"process"`
	QueueDepth      int             `json:"queue_depth"`
	QueueCap        int             `json:"queue_cap"`
}

type publisherWire struct {
	Posted  int64 `json:"posted"`
	Dropped int64 `json:"dropped"`
	Failed  int64 `json:"failed"`
}

// Refresher computes the default-window overview on triggers and a
// timer, broadcasting `overview` only when its hash changes. Ordinary
// ingestion never triggers a full scan: one bounded candidate read,
// one grouped digest, and a few scalar counts per refresh at most.
//
// Health and publisher attach after construction (the publisher is
// created after the server) via SetHealth/SetPublisher, before Run
// starts; all access is atomic so a late attach is safe.
type Refresher struct {
	store     *store.Store
	health    atomic.Pointer[health.Counters]
	publisher atomic.Pointer[Publisher]
	window    time.Duration
	maxWindow time.Duration
	retention time.Duration
	// subjectRetention is the reported-subject horizon from config;
	// zero means unconfigured and the health retention section omits
	// subject ages rather than claiming a policy.
	subjectRetention time.Duration
	now              func() time.Time

	trigger chan struct{}

	mu     sync.Mutex
	hash   uint64
	cached []byte
}

// NewRefresher builds the overview refresher. Health and publisher may
// be nil (tests, publisher disabled); the store must not be.
func NewRefresher(st *store.Store, h *health.Counters, p *Publisher, window, maxWindow, retention time.Duration) *Refresher {
	r := &Refresher{
		store:  st,
		window: window, maxWindow: maxWindow, retention: retention,
		now:     time.Now,
		trigger: make(chan struct{}, 1),
	}
	r.SetHealth(h)
	r.SetPublisher(p)
	return r
}

// SetHealth attaches the operational counters. A nil argument leaves
// the refresher detached; reads through a nil pointer stay nil-safe.
func (r *Refresher) SetHealth(h *health.Counters) {
	if h != nil {
		r.health.Store(h)
	}
}

// SetPublisher attaches the annotation publisher for delivery stats.
// A nil argument leaves the refresher detached.
func (r *Refresher) SetPublisher(p *Publisher) {
	if p != nil {
		r.publisher.Store(p)
	}
}

// SetSubjectRetention attaches the reported-subject retention horizon
// from config for the health retention section. Zero leaves the
// subject age out of the response.
func (r *Refresher) SetSubjectRetention(d time.Duration) {
	r.subjectRetention = d
}

// Trigger requests a refresh, coalesced: a full trigger channel means a
// refresh is already pending, so extra triggers drop.
func (r *Refresher) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Cached returns the last computed overview body, or nil before the
// first refresh.
func (r *Refresher) Cached() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cached
}

// Run refreshes on triggers and the timer until ctx ends, calling
// onChange with the fresh body whenever its hash changes. Refreshes
// coalesce to at most one per second no matter the trigger rate — but
// a throttled trigger is never dropped: it marks the view dirty and a
// delayed refresh runs when the throttle interval expires, so a
// briefing inserted during the interval reaches subscribers without
// waiting for the next periodic tick.
func (r *Refresher) Run(ctx context.Context, tick time.Duration, onChange func([]byte)) {
	t := time.NewTicker(tick)
	defer t.Stop()
	var delay *time.Timer
	var delayCh <-chan time.Time
	var dirty bool
	last := time.Time{}
	refresh := func() {
		if time.Since(last) < time.Second {
			dirty = true
			if delay == nil {
				remaining := time.Until(last.Add(time.Second))
				if remaining < 0 {
					remaining = 0
				}
				delay = time.NewTimer(remaining)
				delayCh = delay.C
			}
			return
		}
		if delay != nil {
			delay.Stop()
			delay, delayCh = nil, nil
		}
		dirty = false
		last = time.Now()
		body, hash, err := r.compute(ctx, r.window)
		if err != nil {
			log.Printf("viewer: overview refresh: %s", store.SanitizeErrorText(err.Error()))
			return
		}
		r.mu.Lock()
		changed := hash != r.hash
		if changed {
			r.hash, r.cached = hash, body
		}
		r.mu.Unlock()
		if changed {
			onChange(body)
		}
	}
	refresh()
	for {
		select {
		case <-ctx.Done():
			if delay != nil {
				delay.Stop()
			}
			return
		case <-t.C:
			refresh()
		case <-r.trigger:
			refresh()
		case <-delayCh:
			delay, delayCh = nil, nil
			if dirty {
				refresh()
			}
		}
	}
}

// hashBody fingerprints an overview body for change detection.
func hashBody(body []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(body)
	return h.Sum64()
}

// compute builds the overview body for one window.
func (r *Refresher) compute(ctx context.Context, window time.Duration) ([]byte, uint64, error) {
	cov, sources, err := r.coverage(ctx)
	if err != nil {
		return nil, 0, err
	}
	now := r.now().UTC()
	since := now.Add(-window)
	// Verdict and headline count across the entire window: the
	// candidate list below is only the display cut.
	rankCounts, err := r.store.PriorityCounts(ctx, since)
	if err != nil {
		return nil, 0, err
	}
	candidates, err := r.store.AttentionCandidates(ctx, since, overview.CandidateCap)
	if err != nil {
		return nil, 0, err
	}
	digest, err := r.store.BriefingDigest(ctx, since)
	if err != nil {
		return nil, 0, err
	}
	receipts, err := r.store.ReceiptCount(ctx, since)
	if err != nil {
		return nil, 0, err
	}
	lastReceipt, haveReceipt, err := r.store.LastReceipt(ctx)
	if err != nil {
		return nil, 0, err
	}
	var last time.Time
	if haveReceipt {
		last = lastReceipt
	}
	items := make([]overview.Item, 0, len(candidates))
	for _, c := range candidates {
		items = append(items, overview.Item{
			ID: c.BriefingID, Priority: c.Priority,
			CreatedAt: c.CreatedAt, ChangeKind: c.Kind,
		})
		if c.CreatedAt.After(last) {
			last = c.CreatedAt
		}
	}
	// Decisions and incidents are typed but empty until Steps 25-29;
	// their behavior is exercised through synthetic pure inputs.
	// Compute orders the display cut; Summarize derives the verdict
	// from window-wide counts so rows past the cap still vote.
	computed := overview.Compute(items, nil, digest, receipts, window, last, cov, now)
	verdict, headline, _ := overview.Summarize(rankCounts, 0)
	wire := overviewWire{
		Verdict:   verdict,
		Headline:  headline,
		Digest:    computed.Digest,
		Receipts:  computed.Receipts,
		Window:    window.String(),
		Coverage:  computed.Coverage.Status,
		Liveness:  aggregateLiveness(sources),
		Sources:   sources,
		Reasons:   computed.Coverage.Reasons,
		Attention: make([]attentionWire, 0, len(computed.Attention)),
	}
	if !computed.LastChange.IsZero() {
		wire.LastChange = computed.LastChange.UTC().Format(time.RFC3339)
	}
	for _, a := range computed.Attention {
		kind := "briefing"
		if a.Incident {
			kind = "incident"
		}
		wire.Attention = append(wire.Attention, attentionWire{
			ID: a.ID, Kind: kind, Priority: a.Priority,
			ChangeKind: a.ChangeKind,
			CreatedAt:  a.CreatedAt.UTC().Format(time.RFC3339),
			Status:     a.Status,
		})
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, 0, err
	}
	return body, hashBody(body), nil
}

// coverage assembles health evidence and assesses coverage. Sources are
// tracked independently: one healthy source never masks a silent one.
// Liveness is unknown until heartbeat capability exists (Step 21); a
// quiet event stream never implies host presence.
func (r *Refresher) coverage(ctx context.Context) (overview.Coverage, []sourceWire, error) {
	now := r.now().UTC()
	healthSince := now.Add(-healthWindow)
	sources, err := r.store.SourceHealthRows(ctx)
	if err != nil {
		return overview.Coverage{}, nil, err
	}
	used, total, err := r.store.FallbackShare(ctx, healthSince)
	if err != nil {
		return overview.Coverage{}, nil, err
	}
	forwardErrors, err := r.store.RecentForwardErrors(ctx, healthSince)
	if err != nil {
		return overview.Coverage{}, nil, err
	}
	parked, err := r.store.FailedGroups(ctx)
	if err != nil {
		return overview.Coverage{}, nil, err
	}
	in := overview.CoverageInput{AsOf: now, ForwardErrors: forwardErrors, ParkedGroups: len(parked)}
	if total > 0 {
		in.FallbackBriefings = total
		in.FallbackRatio = float64(used) / float64(total)
	}
	if snap := r.health.Load().Snapshot(); snap.RedactionBlocked > 0 {
		in.RedactionBlocked = true
	}
	if queueFull(r.store) {
		in.WriterSaturated = true
	}
	wires := make([]sourceWire, 0, len(sources))
	for _, s := range sources {
		w := sourceWire{
			Scope: s.Scope, Status: overview.CoverageOK,
			Liveness:          overview.LivenessUnknown,
			IdentityNamespace: s.ConfiguredNS,
			Capabilities:      s.Capabilities,
			HeartbeatEnabled:  s.HeartbeatEnabled,
			DeliveryStatus:    s.DeliveryStatus,
			Reasons:           []string{},
		}
		si := overview.SourceInput{Scope: s.Scope}
		if s.HeartbeatEnabled {
			si.HeartbeatCapable = true
		}
		if s.HasLastReport {
			w.LastReportAt = s.LastReportAt.UTC().Format(time.RFC3339)
			si.LastReportAge = now.Sub(s.LastReportAt)
		}
		if s.HasLastHeartbeat {
			w.LastHeartbeatAt = s.LastHeartbeatAt.UTC().Format(time.RFC3339)
		}
		if s.DeliveryStatus != "" {
			si.DeliveryGap = true
		}
		in.Sources = append(in.Sources, si)
		wires = append(wires, w)
	}
	assessed := overview.AssessCoverage(in)
	for i, sc := range assessed.Sources {
		wires[i].Status = sc.Status
		wires[i].Reasons = sc.Reasons
	}
	return assessed, wires, nil
}

// queueFull reports live writer pressure: a full job queue means ingest
// is being shed right now, independent of cumulative counters.
func queueFull(st *store.Store) bool {
	return st.QueueDepth() >= st.QueueCap() && st.QueueCap() > 0
}
