// Command watcherbridge runs the full local pipeline: ingest, grouping
// worker with model provider, loopback viewer, and async ScanRelay
// annotation publishing. See README for the flag/env reference.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ostefani/watcherbridge/internal/briefing"
	"github.com/ostefani/watcherbridge/internal/config"
	"github.com/ostefani/watcherbridge/internal/forwarder"
	"github.com/ostefani/watcherbridge/internal/provider"
	"github.com/ostefani/watcherbridge/internal/redactor"
	"github.com/ostefani/watcherbridge/internal/server"
	"github.com/ostefani/watcherbridge/internal/store"
	"github.com/ostefani/watcherbridge/internal/viewer"
)

func main() {
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
	briefRet := fs.Duration("briefing-retention", def.BriefingRetention, "retained briefing history (min 24h)")
	subjRet := fs.Duration("subject-retention", def.SubjectRetention, "retained reported-subject context (min briefing retention)")
	purgeEvery := fs.Duration("purge-interval", def.PurgeInterval, "purge loop interval (min 1m)")
	tick := fs.Duration("grouper-tick", def.GrouperTick, "worker tick")
	lease := fs.Duration("processing-lease", def.ProcessingLease, "stale processing lease")
	agentTimeout := fs.Duration("agent-timeout", def.AgentTimeout, "per-turn agent budget (Step 26 uses it)")
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
	cfg.GrouperTick = *tick
	cfg.ProcessingLease = *lease
	cfg.AgentTimeout = *agentTimeout
	cfg.ModelProvider = *modelProvider
	cfg.ModelID = *modelID
	cfg.ModelAPIKey = config.Secret(firstNonEmpty(*modelKey, os.Getenv("MODEL_API_KEY")))
	cfg.ModelAPIKeyFile = *modelKeyFile
	cfg.ModelBaseURL = *modelURL
	cfg.ProviderTimeout = *modelTimeout
	cfg.Redaction = *redaction
	cfg.StableIdentityFields = strings.Split(*stableFields, ",")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(*db)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	// Single-source binding: refuse to serve a database that already
	// holds another deployment's scoped receipts instead of mixing
	// their subjects. Pre-scope rows carry '' and stay adoptable.
	if err := st.BindSourceScope(context.Background(), cfg.SourceID); err != nil {
		log.Fatalf("source scope: %v", err)
	}
	// Shutdown closes the store last, after the worker and publisher.
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("close store: %v", err)
		}
	}()

	// Startup ordering is a synchronization boundary, not launch order:
	// recovery first, then the completed startup sweep, then the initial
	// purge — only afterwards do serving and the background loops start.
	// Each step runs to completion so retained work is never purged on
	// a stale view and the sweep never races the first purge.
	if recovered, err := st.Recover(ctx, time.Now()); err != nil {
		log.Fatalf("startup recovery: %v", err)
	} else if len(recovered) > 0 {
		log.Printf("startup recovery: requeued %d groups", len(recovered))
	}
	srv, err := server.New(cfg, st, forwarder.NewGate())
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	srv.SweepOnce(ctx)
	purgeSettings := store.PurgeSettings{BriefingRetention: cfg.BriefingRetention, SubjectRetention: cfg.SubjectRetention}
	if rep, err := st.PurgeOnce(ctx, purgeSettings, time.Now()); err != nil {
		log.Fatalf("startup purge: %v", err)
	} else {
		log.Printf("startup purge: %+v", rep)
	}
	srv.RegisterOnShutdown(srv.Viewer().Close)
	hub := srv.Viewer().Hub()

	var prov provider.Provider
	switch cfg.ModelProvider {
	case "meta":
		key, err := cfg.ResolveModelKey()
		if err != nil {
			log.Fatalf("model key: %v", err)
		}
		prov, err = provider.NewMetaAdapter(cfg.ModelBaseURL, cfg.ModelID, key, nil, cfg.MaxModelResponseBytes)
		if err != nil {
			log.Fatalf("model adapter: %v", err)
		}
	case "fake":
		log.Print("watcherbridge: fake model provider; all briefings are deterministic fallbacks")
		prov = provider.NewFake()
	}
	if !cfg.Redaction {
		log.Print("watcherbridge: WARNING: redaction is off; raw values go to the provider and the leak scan is bypassed")
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

	// The purge loop sits beside the briefing worker: both share only the
	// serialized writer, so a slow purge batch delays individual writes
	// but never blocks provider calls or the briefing tick. Small batches
	// plus pressure yield provide the isolation; join before the store.
	purgeDone := make(chan error, 1)
	go func() { purgeDone <- runPurgeLoop(ctx, st, cfg, purgeSettings) }()

	workerDone := make(chan error, 1)
	go func() {
		workerDone <- briefing.Run(ctx, briefing.Deps{
			Store:       st,
			SourceScope: cfg.SourceID,
			Groups: store.GroupPolicy{QuietWindow: cfg.GroupQuietWindow,
				MaxWindow: cfg.GroupMaxWindow, MaxEvents: cfg.GroupMaxEvents},
			Tick:  cfg.GrouperTick,
			Lease: cfg.ProcessingLease,
			Now:   time.Now,
			Policy: briefing.Policy{EstablishedAge: cfg.EstablishedAge,
				StrongHistoryAge: cfg.StrongHistoryAge, StableFields: cfg.StableIdentityFields},
			Provider:     prov,
			Redaction:    redactor.Policy{Enabled: cfg.Redaction, MaxFieldChars: cfg.MaxFieldChars},
			ModelTimeout: cfg.ProviderTimeout,
			OnBriefing:   onBriefing,
			OnGroups:     onGroups,
		})
	}()

	log.Printf("watcherbridge: serving ingest and viewer on http://%s/ (%s mode)", cfg.Bind, cfg.Mode)
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shut); err != nil {
			log.Printf("server shutdown: %v", err)
		}
	}()
	if err := srv.Start(); err != nil {
		log.Printf("server: %v", err)
	}
	// Ingest stopped: drain the worker and the purge loop, then shut the
	// publisher down so queued annotations flush before the store closes.
	stop()
	if err := <-workerDone; err != nil {
		log.Printf("worker: %v", err)
	}
	if err := <-purgeDone; err != nil {
		log.Printf("purge loop: %v", err)
	}
	log.Print("watcherbridge: stopped")
}

// runPurgeLoop ticks PurgeOnce until ctx ends. Errors are logged and
// retried next tick: a transient failure must not kill the process,
// and the next pass resumes from recorded purge_state cutoffs.
func runPurgeLoop(ctx context.Context, st *store.Store, cfg config.Config, settings store.PurgeSettings) error {
	tick := time.NewTicker(cfg.PurgeInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
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
