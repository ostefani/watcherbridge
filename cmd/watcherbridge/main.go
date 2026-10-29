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
	db := fs.String("db", "watcherbridge.db", "sqlite path (0600)")
	secret := fs.String("secret", "", "webhook HMAC secret (or WEBHOOK_SECRET)")
	secretFile := fs.String("secret-file", "", "file holding the webhook secret")
	relay := fs.String("scanrelay-url", def.ScanRelayURL, "ScanRelay base URL; empty disables proxy and publisher")
	skew := fs.Duration("clock-skew-allowance", def.ClockSkewAllowance, "assumed worst-case clock difference with the ScanRelay host")
	forwardBearer := fs.String("forward-bearer", "", "bearer injected into ScanRelay forwards/annotations (or FORWARD_BEARER_TOKEN)")
	forwardKey := fs.String("forward-api-key", "", "API key injected into ScanRelay forwards/annotations (or FORWARD_API_KEY)")
	quiet := fs.Duration("group-quiet-window", def.GroupQuietWindow, "grouping quiet window")
	maxWindow := fs.Duration("group-max-window", def.GroupMaxWindow, "grouping hard cap")
	tick := fs.Duration("grouper-tick", def.GrouperTick, "worker tick")
	lease := fs.Duration("processing-lease", def.ProcessingLease, "stale processing lease")
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
	cfg.Secret = config.Secret(firstNonEmpty(*secret, os.Getenv("WEBHOOK_SECRET")))
	cfg.SecretFile = *secretFile
	cfg.ScanRelayURL = *relay
	cfg.ClockSkewAllowance = *skew
	cfg.ForwardBearerToken = config.Secret(firstNonEmpty(*forwardBearer, os.Getenv("FORWARD_BEARER_TOKEN")))
	cfg.ForwardAPIKey = config.Secret(firstNonEmpty(*forwardKey, os.Getenv("FORWARD_API_KEY")))
	cfg.GroupQuietWindow = *quiet
	cfg.GroupMaxWindow = *maxWindow
	cfg.GrouperTick = *tick
	cfg.ProcessingLease = *lease
	cfg.ModelProvider = *modelProvider
	cfg.ModelID = *modelID
	cfg.ModelAPIKey = config.Secret(firstNonEmpty(*modelKey, os.Getenv("MODEL_API_KEY")))
	cfg.ModelAPIKeyFile = *modelKeyFile
	cfg.ModelBaseURL = *modelURL
	cfg.ProviderTimeout = *modelTimeout
	cfg.Redaction = *redaction
	cfg.StableIdentityFields = strings.Split(*stableFields, ",")

	st, err := store.Open(*db)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	// Shutdown closes the store last, after the worker and publisher.
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("close store: %v", err)
		}
	}()

	srv, err := server.New(cfg, st, forwarder.NewGate())
	if err != nil {
		log.Fatalf("server: %v", err)
	}
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
	onBriefing, onGroups := viewer.Fanout(hub, pub)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- briefing.Run(ctx, briefing.Deps{
			Store: st,
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
	// Ingest stopped: drain the worker, then shut the publisher down so
	// queued annotations flush before the store closes.
	stop()
	if err := <-workerDone; err != nil {
		log.Printf("worker: %v", err)
	}
	log.Print("watcherbridge: stopped")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
