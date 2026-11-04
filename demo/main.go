// Command demo runs a self-seeding WatcherBridge stack for local UI
// tryouts: ingest + worker (fake model) + viewer + annotation publisher.
// It posts two signed webhooks to itself a moment after startup, so
// opening the viewer shows open groups counting down, then two briefings.
//
// DEV ONLY: the webhook secret and the fake provider are hardcoded, and
// the database in /tmp is wiped on every start. Not for review as a
// product surface; the real main arrives with Phase 4.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
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

const (
	bind      = "127.0.0.1:8081"
	dbPath    = "/tmp/watcherbridge-demo/demo.db"
	scanRelay = "http://127.0.0.1:8080/api/annotations"
)

// webhookSecret is the dev HMAC key, from DEMO_WEBHOOK_SECRET or a
// hardcoded fallback. Either way it signs only loopback demo traffic;
// the fallback prints a warning so it is never mistaken for a real one.
var webhookSecret string

func resolveSecret() {
	webhookSecret = os.Getenv("DEMO_WEBHOOK_SECRET")
	if webhookSecret == "" {
		webhookSecret = "demo-secret-0123456789abcdef0123"
		log.Print("demo: WARNING: using the hardcoded dev webhook secret; set DEMO_WEBHOOK_SECRET for anything else")
	}
}

func sign(timestamp, eventID, eventKey, eventType string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	for _, part := range [][]byte{[]byte(timestamp), []byte("."), []byte(eventID), []byte("."),
		[]byte(eventKey), []byte("."), []byte(eventType), []byte("."), body} {
		mac.Write(part)
	}
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postEvent(eventID, eventType, payload string) {
	ts := fmt.Sprint(time.Now().UTC().Unix())
	key := "demo-key"
	sig := sign(ts, eventID, key, eventType, []byte(payload))
	req, _ := http.NewRequest(http.MethodPost, "http://"+bind+"/webhook", strings.NewReader(payload))
	req.Header.Set("X-SubnetLens-Event-ID", eventID)
	req.Header.Set("X-SubnetLens-Event-Key", key)
	req.Header.Set("X-SubnetLens-Event-Type", eventType)
	req.Header.Set("X-SubnetLens-Signature-Timestamp", ts)
	req.Header.Set("X-SubnetLens-Signature-256", sig)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("demo: post %s: %v", eventID, err)
		return
	}
	defer resp.Body.Close()
	log.Printf("demo: post %s -> %d", eventID, resp.StatusCode)
}

// fakeAnswer builds a schema-valid model answer from the live bundle.
func fakeAnswer(payload []byte, toks map[string]struct{}) []byte {
	var bundle struct {
		EventIDs []string `json:"event_ids"`
	}
	if err := json.Unmarshal(payload, &bundle); err != nil {
		return []byte(`{"bogus":true}`)
	}
	hosts := sortedWithPrefix(toks, "HOST_")
	host := ""
	if len(hosts) > 0 {
		host = hosts[0]
	}
	ips := sortedWithPrefix(toks, "IP_")
	what := "observed activity"
	if len(ips) > 0 {
		what = "traffic from " + ips[0]
	}
	m := map[string]any{
		"event_ids": bundle.EventIDs, "host": host, "what_changed": what,
		"context": "demo context for " + host, "model_suggestion": "high",
		"priority_reason": "demo: new open port on a stable host", "recommended_check": "verify the service",
	}
	raw, _ := json.Marshal(m)
	return raw
}

func sortedWithPrefix(toks map[string]struct{}, prefix string) []string {
	var out []string
	for tok := range toks {
		if strings.HasPrefix(tok, prefix) {
			out = append(out, tok)
		}
	}
	sort.Strings(out)
	return out
}

func changedPayload(hostname, ip, mac string) string {
	return `{"event_id":"e1","previous":{"ip":"` + ip + `","hostname":"` + hostname + `","mac":"` + mac + `","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-26T00:00:00Z","ports":[]},"current":{"ip":"` + ip + `","hostname":"` + hostname + `","mac":"` + mac + `","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-26T00:00:00Z","ports":[{"number":22,"protocol":"tcp","state":"open"}]}}`
}

func main() {
	resolveSecret()
	_ = os.Remove(dbPath)
	_ = os.MkdirAll("/tmp/watcherbridge-demo", 0o700)

	cfg := config.Default()
	cfg.Bind = bind
	cfg.Mode = config.ModeTap
	cfg.Secret = config.Secret(webhookSecret)
	cfg.GroupQuietWindow = 5 * time.Second
	cfg.GroupMaxWindow = 60 * time.Second
	cfg.GrouperTick = time.Second
	cfg.ProcessingLease = 120 * time.Second
	cfg.Redaction = true

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	srv, err := server.New(cfg, st, forwarder.NewGate())
	if err != nil {
		log.Fatal(err)
	}

	fake := provider.NewFake()
	fake.Handle = func(repair bool, payload []byte, valid map[string]struct{}) (provider.RawBriefing, error) {
		if repair {
			return provider.RawBriefing{}, fmt.Errorf("demo: no repair script")
		}
		return provider.RawBriefing{JSON: fakeAnswer(payload, valid)}, nil
	}

	pub := viewer.NewPublisher(scanRelay, "", "", "", 3*time.Second, nil)
	pub.Start()
	defer pub.Stop()
	onBriefing, onGroups := viewer.Fanout(srv.Viewer().Hub(), pub, 45)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := briefing.Run(ctx, briefing.Deps{
			Store: st,
			Groups: store.GroupPolicy{QuietWindow: cfg.GroupQuietWindow,
				MaxWindow: cfg.GroupMaxWindow, MaxEvents: cfg.GroupMaxEvents},
			Tick:         cfg.GrouperTick,
			Lease:        cfg.ProcessingLease,
			Now:          time.Now,
			Policy:       briefing.Policy{StableFields: cfg.StableIdentityFields},
			Provider:     fake,
			Redaction:    redactor.Policy{Enabled: true, MaxFieldChars: cfg.MaxFieldChars},
			ModelTimeout: cfg.ProviderTimeout,
			OnBriefing:   onBriefing,
			OnGroups:     onGroups,
		}); err != nil {
			log.Printf("demo: worker: %v", err)
		}
	}()

	go func() {
		time.Sleep(2 * time.Second)
		// TEST-NET-1 (RFC 5737) documentation addresses: unroutable
		// and self-evidently fake, so screenshots never leak a real LAN.
		postEvent("demo-evt-1", "host.changed", changedPayload("printer-2", "192.0.2.23", "aa:bb:cc:dd:ee:01"))
		postEvent("demo-evt-2", "host.appeared", `{"event_id":"e2","host":{"ip":"192.0.2.99","hostname":"new-cam","mac":"aa:bb:cc:dd:ee:02","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-28T10:00:00Z"}}`)
	}()

	log.Printf("demo: open http://%s/ in a browser (briefings land ~10s after start)", bind)
	if err := srv.Start(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
