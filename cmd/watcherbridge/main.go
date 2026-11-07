// Command watcherbridge runs the full local pipeline: ingest, grouping
// worker with model provider, loopback viewer, and async ScanRelay
// annotation publishing. See README for the flag/env reference.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ostefani/watcherbridge/internal/briefing"
	"github.com/ostefani/watcherbridge/internal/config"
	"github.com/ostefani/watcherbridge/internal/forwarder"
	"github.com/ostefani/watcherbridge/internal/health"
	"github.com/ostefani/watcherbridge/internal/incidentsummary"
	"github.com/ostefani/watcherbridge/internal/provider"
	"github.com/ostefani/watcherbridge/internal/redactor"
	"github.com/ostefani/watcherbridge/internal/server"
	"github.com/ostefani/watcherbridge/internal/store"
	"github.com/ostefani/watcherbridge/internal/viewer"
)

func main() {
	// `status` is a read-only API client, not a bridge instance: it must
	// not parse bridge flags, open the database, or start workers.
	if len(os.Args) > 1 && os.Args[1] == "status" {
		os.Exit(runStatus(os.Args[2:]))
	}
	def := config.Default()
	fs := flag.NewFlagSet("watcherbridge", flag.ExitOnError)
	bind := fs.String("bind", def.Bind, "loopback bind address")
	mode := fs.String("mode", def.Mode, "tap or proxy")
	sourceID := fs.String("source-id", "", "authenticated scanner deployment ID scoping reported references (or SOURCE_ID)")
	db := fs.String("db", "watcherbridge.db", "sqlite path (0600)")
	secret := fs.String("secret", "", "webhook HMAC secret (or WEBHOOK_SECRET)")
	secretFile := fs.String("secret-file", "", "file holding the webhook secret")
	relay := fs.String("scanrelay-url", def.ScanRelayURL, "ScanRelay base URL; empty disables proxy and publisher")
	skew := fs.Duration("clock-skew-allowance", def.ClockSkewAllowance, "assumed worst-case clock difference with the ScanRelay host")
	forwardBearer := fs.String("forward-bearer", "", "bearer injected into ScanRelay forwards/annotations (or FORWARD_BEARER_TOKEN)")
	forwardKey := fs.String("forward-api-key", "", "API key injected into ScanRelay forwards/annotations (or FORWARD_API_KEY)")
	quiet := fs.Duration("group-quiet-window", def.GroupQuietWindow, "grouping quiet window")
	maxWindow := fs.Duration("group-max-window", def.GroupMaxWindow, "grouping hard cap")
	overviewWindow := fs.Duration("overview-window", def.OverviewWindow, "default overview window")
	overviewMax := fs.Duration("overview-max-window", def.OverviewMaxWindow, "configured overview maximum")
	overviewTick := fs.Duration("overview-tick", def.OverviewTick, "overview recompute timer (5s-5m)")
	briefRet := fs.Duration("briefing-retention", def.BriefingRetention, "retained briefing history (min 24h)")
	subjRet := fs.Duration("subject-retention", def.SubjectRetention, "retained reported-subject context (min briefing retention)")
	purgeEvery := fs.Duration("purge-interval", def.PurgeInterval, "purge loop interval (min 1m)")
	snoozeDefault := fs.Duration("snooze-default", def.SnoozeDefault, "default briefing snooze horizon")
	decisionRet := fs.Duration("decision-retention", def.DecisionRetention, "retained operator-decision audit (min 24h)")
	heartbeatStale := fs.Duration("heartbeat-stale-after", def.HeartbeatStaleAfter, "source heartbeat freshness limit")
	heartbeatRetention := fs.Duration("heartbeat-retention", def.HeartbeatRetention, "retained heartbeat history")
	ruleProposeAfter := fs.Int("rule-propose-after", def.RuleProposeAfter, "matching expected decisions that derive a rule proposal (min 1)")
	ruleDefaultTTL := fs.Duration("rule-default-ttl", def.RuleDefaultTTL, "default rule approval grant")
	ruleRet := fs.Duration("rule-retention", def.RulePurgeAfter, "retained terminal review-policy rules (min 24h)")
	incidentTick := fs.Duration("incident-tick", def.IncidentTick, "presentation incident correlator tick (5s-5m)")
	incidentWindow := fs.Duration("incident-window", def.IncidentWindow, "incident formation window (within briefing retention)")
	incidentQuiet := fs.Duration("incident-quiet", def.IncidentQuiet, "incident quiet close (min one tick)")
	incidentMinRefs := fs.Int("incident-min-refs", def.IncidentMinRefs, "distinct qualified references per incident (min 2)")
	incidentRet := fs.Duration("incident-retention", def.IncidentRetention, "retained closed incidents (min 24h, max subject retention)")
	incidentSummary := fs.Bool("incident-summary-enabled", def.IncidentSummaryEnabled, "generate one AI summary after each incident closes (opt-in)")
	tick := fs.Duration("grouper-tick", def.GrouperTick, "worker tick")
	lease := fs.Duration("processing-lease", def.ProcessingLease, "stale processing lease")
	agentTimeout := fs.Duration("agent-timeout", def.AgentTimeout, "bounded investigator loop run budget")
	agentEnabled := fs.Bool("agent-enabled", def.AgentEnabled, "enable the bounded investigator loop (opt-in)")
	agentMaxSteps := fs.Int("agent-max-steps", def.AgentMaxSteps, "read-only tool turns per run (1-4)")
	modelMin := fs.String("model-min-baseline", def.ModelMinBaseline, "single-call provider available at/below this baseline")
	agentMin := fs.String("agent-min-baseline", def.AgentMinBaseline, "agent loop investigates at/above this baseline")
	modelProvider := fs.String("model-provider", def.ModelProvider, "meta or fake")
	modelID := fs.String("model-id", def.ModelID, "model ID for the meta provider")
	modelKey := fs.String("model-key", "", "model API key (or MODEL_API_KEY)")
	modelKeyFile := fs.String("model-key-file", "", "file holding the model API key")
	modelURL := fs.String("model-base-url", def.ModelBaseURL, "model endpoint")
	modelTimeout := fs.Duration("provider-timeout", def.ProviderTimeout, "shared attempt+repair budget")
	redaction := fs.Bool("redaction", def.Redaction, "set false to pass raw values (warned, fail-open)")
	stableFields := fs.String("stable-fields", strings.Join(def.StableIdentityFields, ","), "comma-separated identity fields")
	_ = fs.Parse(os.Args[1:])

	cfg := config.Default()
	cfg.Bind = *bind
	cfg.Mode = *mode
	cfg.SourceID = firstNonEmpty(*sourceID, os.Getenv("SOURCE_ID"), def.SourceID)
	cfg.Secret = config.Secret(firstNonEmpty(*secret, os.Getenv("WEBHOOK_SECRET")))
	cfg.SecretFile = *secretFile
	cfg.ScanRelayURL = *relay
	cfg.ClockSkewAllowance = *skew
	cfg.ForwardBearerToken = config.Secret(firstNonEmpty(*forwardBearer, os.Getenv("FORWARD_BEARER_TOKEN")))
	cfg.ForwardAPIKey = config.Secret(firstNonEmpty(*forwardKey, os.Getenv("FORWARD_API_KEY")))
	cfg.GroupQuietWindow = *quiet
	cfg.GroupMaxWindow = *maxWindow
	cfg.BriefingRetention = *briefRet
	cfg.SubjectRetention = *subjRet
	cfg.PurgeInterval = *purgeEvery
	cfg.SnoozeDefault = *snoozeDefault
	cfg.DecisionRetention = *decisionRet
	cfg.HeartbeatStaleAfter = *heartbeatStale
	cfg.HeartbeatRetention = *heartbeatRetention
	cfg.RuleProposeAfter = *ruleProposeAfter
	cfg.RuleDefaultTTL = *ruleDefaultTTL
	cfg.RulePurgeAfter = *ruleRet
	cfg.IncidentTick = *incidentTick
	cfg.IncidentWindow = *incidentWindow
	cfg.IncidentQuiet = *incidentQuiet
	cfg.IncidentMinRefs = *incidentMinRefs
	cfg.IncidentRetention = *incidentRet
	if *incidentSummary {
		cfg.IncidentSummaryEnabled = true
	} else if os.Getenv("INCIDENT_SUMMARY_ENABLED") == "true" {
		cfg.IncidentSummaryEnabled = true
	}
	cfg.GrouperTick = *tick
	cfg.OverviewWindow = *overviewWindow
	cfg.OverviewMaxWindow = *overviewMax
	cfg.OverviewTick = *overviewTick
	cfg.ProcessingLease = *lease
	cfg.AgentTimeout = *agentTimeout
	cfg.AgentEnabled = *agentEnabled
	cfg.AgentMaxSteps = *agentMaxSteps
	cfg.ModelMinBaseline = *modelMin
	cfg.AgentMinBaseline = *agentMin
	cfg.ModelProvider = *modelProvider
	cfg.ModelID = *modelID
	cfg.ModelAPIKey = config.Secret(firstNonEmpty(*modelKey, os.Getenv("MODEL_API_KEY")))
	cfg.ModelAPIKeyFile = *modelKeyFile
	cfg.ModelBaseURL = *modelURL
	cfg.ProviderTimeout = *modelTimeout
	cfg.Redaction = *redaction
	cfg.StableIdentityFields = strings.Split(*stableFields, ",")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	// A second interrupt must be able to terminate a stalled drain.
	stopOnSignal := context.AfterFunc(ctx, stop)
	err := run(ctx, cfg, *db)
	signalErr := ctx.Err()
	stopOnSignal()
	stop()
	if err != nil {
		if !errors.Is(err, signalErr) {
			log.Fatal(err)
		}
		log.Printf("watcherbridge: startup interrupted: %v", err)
	}
	log.Print("watcherbridge: stopped")
}

func run(ctx context.Context, cfg config.Config, db string) error {
	if ctx.Err() != nil {
		return nil
	}
	st, err := store.Open(db)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	// Shutdown closes the store last, after the worker and publisher.
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("close store: %v", err)
		}
	}()

	// Effective-state queries share the configured audit horizon with
	// purge, so a stalled purge never extends a verdict past retention.
	st.SetDecisionRetention(cfg.DecisionRetention)
	// Step 26 floors are validated in server.New before the worker
	// starts; the parse here cannot fail on a validated config.
	modelMinLevel, _ := briefing.ParseLevel(cfg.ModelMinBaseline)
	agentMinLevel, _ := briefing.ParseLevel(cfg.AgentMinBaseline)
	// Single-source binding: refuse to serve a database that already
	// holds another deployment's scoped receipts instead of mixing
	// their subjects. Pre-scope rows carry '' and stay adoptable.
	if err := st.BindSourceScope(ctx, cfg.SourceID); err != nil {
		return fmt.Errorf("source scope: %w", err)
	}

	// Startup ordering is a synchronization boundary, not launch order:
	// recovery first, then the completed startup sweep, then the initial
	// purge — only afterwards do serving and the background loops start.
	// Each step runs to completion so retained work is never purged on
	// a stale view and the sweep never races the first purge.
	if recovered, err := st.Recover(ctx, time.Now()); err != nil {
		return fmt.Errorf("startup recovery: %w", err)
	} else if len(recovered) > 0 {
		log.Printf("startup recovery: requeued %d groups", len(recovered))
	}
	srv, err := server.New(cfg, st, forwarder.NewGate())
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	// Operational counters are process-lifetime: the start time travels
	// with every reading so a restart is interpretable, never silent.
	opHealth := health.New()
	srv.Health = opHealth
	srv.Viewer().Overview.SetHealth(opHealth)
	srv.SweepOnce(ctx)
	if ctx.Err() != nil {
		return nil
	}
	purgeSettings := store.PurgeSettings{BriefingRetention: cfg.BriefingRetention, SubjectRetention: cfg.SubjectRetention, DecisionRetention: cfg.DecisionRetention, RuleRetention: cfg.RulePurgeAfter, HeartbeatRetention: cfg.HeartbeatRetention, IncidentRetention: cfg.IncidentRetention, ActiveSource: cfg.SourceID}
	if rep, err := st.PurgeOnce(ctx, purgeSettings, time.Now()); err != nil {
		return fmt.Errorf("startup purge: %w", err)
	} else {
		log.Printf("startup purge: %+v", rep)
	}
	hub := srv.Viewer().Hub()

	var prov provider.Provider
	switch cfg.ModelProvider {
	case "meta":
		key, err := cfg.ResolveModelKey()
		if err != nil {
			return fmt.Errorf("model key: %w", err)
		}
		prov, err = provider.NewMetaAdapter(cfg.ModelBaseURL, cfg.ModelID, key, nil, cfg.MaxModelResponseBytes)
		if err != nil {
			return fmt.Errorf("model adapter: %w", err)
		}
	case "fake":
		log.Print("watcherbridge: fake model provider; all briefings are deterministic fallbacks")
		prov = provider.NewFake()
	}
	if !cfg.Redaction {
		log.Print("watcherbridge: WARNING: redaction is off; raw values go to the provider and the leak scan is bypassed")
	}

	var summarizer provider.Summarizer
	if cfg.IncidentSummaryEnabled {
		var ok bool
		summarizer, ok = prov.(provider.Summarizer)
		if !ok {
			return fmt.Errorf("incident summaries require a provider with explicit summary capability")
		}
	}

	if ctx.Err() != nil {
		return nil
	}
	var pub *viewer.Publisher
	if cfg.ScanRelayURL != "" {
		timeout := cfg.ProxyForwardTimeout
		pub = viewer.NewPublisher(cfg.ScanRelayURL+"/api/annotations",
			string(cfg.ForwardBearerToken), string(cfg.ForwardAPIKey), cfg.ForwardAPIKeyHeader, timeout, nil)
		pub.Start()
		defer pub.Stop()
	} else {
		log.Print("watcherbridge: no ScanRelay URL; annotations disabled")
	}
	onBriefing, onGroups := viewer.Fanout(hub, pub, int64(cfg.GroupQuietWindow/time.Second))
	srv.Viewer().Overview.SetPublisher(pub)

	// The overview refresher recomputes on briefing inserts and its own
	// timer, coalesced to 1/s, broadcasting only on hash change.
	ov := srv.Viewer().Overview
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	workers.Go(func() {
		ov.Run(ctx, cfg.OverviewTick, func(body []byte) {
			hub.Broadcast("overview", json.RawMessage(body))
		})
	})
	onBriefingChanged := func(b store.Briefing) {
		onBriefing(b)
		ov.Trigger()
	}

	// The purge loop sits beside the briefing worker: both share only the
	// serialized writer, so a slow purge batch delays individual writes
	// but never blocks provider calls or the briefing tick. Small batches
	// plus pressure yield provide the isolation; join before the store.
	workers.Go(func() { runPurgeLoop(ctx, st, cfg.PurgeInterval, purgeSettings) })

	// The incident correlator sits beside them on the same terms: one
	// bounded pass per tick through the serialized writer, joined
	// before the store. A pass that changes incidents broadcasts one
	// `incident` event (no SSE id: clients refetch the list) and
	// triggers an overview refresh for the attention cut.
	incidentSettings := store.IncidentSettings{
		Window: cfg.IncidentWindow, Quiet: cfg.IncidentQuiet, MinRefs: cfg.IncidentMinRefs,
		SummaryEnabled: cfg.IncidentSummaryEnabled,
	}
	workers.Go(func() { runIncidentLoop(ctx, st, hub, ov, cfg.IncidentTick, incidentSettings) })

	// The closed-incident summary worker sits beside them on the same
	// terms: one worker, one durable queue capped at 100 outstanding
	// jobs, no blocking of correlation, ingestion or briefing work.
	// Enabling applies to subsequent closures only. Completion reuses
	// incident SSE invalidation and reconnect/resync behavior (one
	// incident event plus an overview refresh) without changing
	// membership, reopening incidents, modifying decisions or
	// publishing scanner annotations.
	if summarizer != nil {
		onSummary := func(incidentID string) {
			hub.Broadcast("incident", viewer.IncidentEventOf(store.IncidentReport{Changed: []string{incidentID}}))
			ov.Trigger()
		}
		workers.Go(func() {
			if err := incidentsummary.Run(ctx, incidentsummary.Deps{
				Store: st, Provider: summarizer,
				Redaction: redactor.Policy{Enabled: cfg.Redaction, MaxFieldChars: cfg.MaxFieldChars},
				Now:       time.Now, Health: opHealth, OnComplete: onSummary,
			}); err != nil {
				log.Printf("summary worker: %v", err)
			}
		})
	}

	workers.Go(func() {
		if err := briefing.Run(ctx, briefing.Deps{
			Store:       st,
			SourceScope: cfg.SourceID,
			Groups: store.GroupPolicy{QuietWindow: cfg.GroupQuietWindow,
				MaxWindow: cfg.GroupMaxWindow, MaxEvents: cfg.GroupMaxEvents},
			Tick:  cfg.GrouperTick,
			Lease: cfg.ProcessingLease,
			Now:   time.Now,
			Policy: briefing.Policy{EstablishedAge: cfg.EstablishedAge,
				StrongHistoryAge: cfg.StrongHistoryAge, StableFields: cfg.StableIdentityFields},
			Provider:         prov,
			Redaction:        redactor.Policy{Enabled: cfg.Redaction, MaxFieldChars: cfg.MaxFieldChars},
			ModelTimeout:     cfg.ProviderTimeout,
			AgentTimeout:     cfg.AgentTimeout,
			AgentEnabled:     cfg.AgentEnabled,
			AgentMaxSteps:    cfg.AgentMaxSteps,
			ModelMinBaseline: modelMinLevel,
			AgentMinBaseline: agentMinLevel,
			Health:           opHealth,
			OnBriefing:       onBriefingChanged,
			OnGroups:         onGroups,
		}); err != nil {
			log.Printf("briefing worker: %v", err)
		}
	})

	log.Printf("watcherbridge: serving ingest and viewer on http://%s/ (%s mode)", cfg.Bind, cfg.Mode)
	serveDone := make(chan error, 1)
	go func() {
		defer cancel() // Stop the workers if the listener exits on its own.
		serveDone <- srv.Start()
	}()

	<-ctx.Done()
	shut, stopShutdown := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer stopShutdown()
	reportSlowDrain := context.AfterFunc(shut, func() {
		log.Print("watcherbridge: shutdown grace expired; waiting for HTTP and background work to exit")
	})
	shutdownErr := srv.Shutdown(shut)
	serveErr := <-serveDone
	workers.Wait()
	reportSlowDrain()

	// Use remaining grace for the explicit checkpoint. Store close still
	// lets SQLite perform its usual last-connection WAL cleanup.
	if shut.Err() == nil {
		if err := st.Checkpoint(shut); err != nil {
			log.Printf("shutdown checkpoint: %v", err)
		}
	}
	if serveErr != nil {
		serveErr = fmt.Errorf("server: %w", serveErr)
	}
	if shutdownErr != nil {
		shutdownErr = fmt.Errorf("server shutdown: %w", shutdownErr)
	}
	return errors.Join(serveErr, shutdownErr)
}

// runIncidentLoop ticks CorrelateIncidents until ctx ends. A pass
// that creates, extends or closes incidents broadcasts one `incident`
// event and triggers an overview refresh; quiet passes stay silent.
// Errors are logged and retried next tick: the durable cursor makes
// every pass safely repeatable.
func runIncidentLoop(ctx context.Context, st *store.Store, hub *viewer.Hub, ov *viewer.Refresher, tick time.Duration, settings store.IncidentSettings) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		rep, err := st.CorrelateIncidents(ctx, settings, time.Now())
		if err != nil {
			log.Printf("incidents: %v", err)
			continue
		}
		if rep.Created+rep.Appended+rep.Closed == 0 {
			continue
		}
		hub.Broadcast("incident", viewer.IncidentEventOf(rep))
		ov.Trigger()
	}
}

// runPurgeLoop ticks PurgeOnce until ctx ends. Errors are logged and
// retried next tick: a transient failure must not kill the process,
// and the next pass reselects remaining eligible work.
func runPurgeLoop(ctx context.Context, st *store.Store, interval time.Duration, settings store.PurgeSettings) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		rep, err := st.PurgeOnce(ctx, settings, time.Now())
		if err != nil {
			log.Printf("purge: %v", err)
			continue
		}
		if rep != (store.PurgeReport{}) {
			log.Printf("purge: %+v", rep)
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
