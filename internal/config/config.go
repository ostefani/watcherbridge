// Package config owns WatcherBridge's receiver settings: every number the
// ingest path depends on lives here with cross-field validation, so the
// server, store policy, and later steps all read one source instead of
// baking constants into code (Designv5 §12).
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// Mode selects tap (persist + 202, all work async) or proxy (persist,
// forward to ScanRelay, then reply).
const (
	ModeTap   = "tap"
	ModeProxy = "proxy"
)

// minSecretBytes floors the HMAC key length. Short keys verify correctly
// but brute-force faster; refusing them is a visible startup error, so an
// operator with an older short secret must rotate it, not wonder why
// ingest silently changed.
const minSecretBytes = 16

// Config carries the Step 8 receiver settings. Durations are real
// time.Duration values; callers that read env/flags convert into these.
type Config struct {
	Bind string // loopback only; anything else is refused at startup
	Mode string // "tap" or "proxy"

	// Secret material. Exactly one of Secret / SecretFile must be set; the
	// value is never logged or stored in the DB.
	Secret     string
	SecretFile string

	ScanRelayURL string

	// Forward credentials for a ScanRelay that requires auth. Injected
	// into the outgoing request at send time; never stored or logged.
	// The Authorization header form is preferred when both are set,
	// mirroring ScanRelay's own check order.
	ForwardBearerToken  string
	ForwardAPIKey       string
	ForwardAPIKeyHeader string

	// ScanRelayTolerance is ScanRelay's own timestamp window. The bridge
	// must not accept events ScanRelay would reject as stale.
	ScanRelayTolerance time.Duration

	MaxBodyBytes       int64
	MaxHeaderBytes     int
	TimestampTolerance time.Duration

	ProxyForwardTimeout time.Duration
	PersistBudget       time.Duration
	Margin              time.Duration // slack between persist+proxy and the handler deadline
	HandlerDeadline     time.Duration // must equal ProxyForwardTimeout+PersistBudget+Margin
	ProSenderTimeout    time.Duration // Pro's webhook timeout for the bridge endpoint

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	InFlightCap    int
	WriterQueueCap int // enforced by the writer queue depth; overflow is 503
	RetryAfterSecs int

	SweepSafetyMargin time.Duration
	SweepBatch        int
	SweepWorkers      int // must be 1: a single worker keeps the sweep bounded

	ShutdownGrace time.Duration

	Redaction     bool // model-bound bundles are tokenized; off passes raw (warned at startup)
	MaxFieldChars int  // rune cap per bundle string, truncation marked

	// Model provider selection. Only "meta" and "fake" exist; anything
	// else refuses startup, so a typo can never route bundles to an
	// unexpected endpoint.
	ModelProvider string
	ModelID       string
	// Model API key material: exactly one of ModelAPIKey /
	// ModelAPIKeyFile when the provider is meta. Never logged or stored.
	ModelAPIKey     string
	ModelAPIKeyFile string
	ModelBaseURL    string // full endpoint to POST the bundle to

	ProviderTimeout       time.Duration // shared budget for the attempt + one repair
	MaxModelResponseBytes int64         // response cap; oversize fails the call

	GroupQuietWindow time.Duration
	GroupMaxWindow   time.Duration
	GroupMaxEvents   int

	GrouperTick          time.Duration
	ProcessingLease      time.Duration // stale processing age; must exceed 2 x provider timeout + margin
	EstablishedAge       time.Duration // first-seen age for established; dormant until Pro ships the field
	StrongHistoryAge     time.Duration // first-seen age for strong history; dormant until Pro ships the field
	StableIdentityFields []string      // change-field names that break stability; validated at startup
}

// Default returns the approved starting thresholds (Designv5 §12/§15).
// Tap is the default mode: the README prefers it, and it needs no
// ScanRelay credentials or forwarding path to be useful.
func Default() Config {
	return Config{
		Bind:                "127.0.0.1:8081",
		Mode:                ModeTap,
		ScanRelayURL:        "http://127.0.0.1:8080",
		ForwardAPIKeyHeader: "X-API-Key",
		ScanRelayTolerance:  300 * time.Second,
		MaxBodyBytes:        1 << 20,
		MaxHeaderBytes:      16 << 10,
		TimestampTolerance:  300 * time.Second,

		ProxyForwardTimeout: 3 * time.Second,
		PersistBudget:       500 * time.Millisecond,
		Margin:              1 * time.Second,
		HandlerDeadline:     4500 * time.Millisecond,
		ProSenderTimeout:    10 * time.Second,

		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      8 * time.Second,
		IdleTimeout:       60 * time.Second,

		InFlightCap:    16,
		WriterQueueCap: 64,
		RetryAfterSecs: 2,

		SweepSafetyMargin: 30 * time.Second,
		SweepBatch:        20,
		SweepWorkers:      1,

		ShutdownGrace: 10 * time.Second,

		Redaction:     true,
		MaxFieldChars: 256,

		// Fake by default: a keyless start must degrade loudly to
		// deterministic fallbacks (each marked fallback_used), never
		// refuse to boot the local pipeline. The worker warns at
		// startup while the provider is fake, like REDACTION=off.
		ModelProvider:         "fake",
		ModelID:               "test-only",
		ModelBaseURL:          "http://127.0.0.1:9/unused",
		ProviderTimeout:       30 * time.Second,
		MaxModelResponseBytes: 64 << 10,

		GroupQuietWindow: 45 * time.Second,
		GroupMaxWindow:   120 * time.Second,
		GroupMaxEvents:   10,

		GrouperTick:          1 * time.Second,
		ProcessingLease:      90 * time.Second,
		EstablishedAge:       1 * time.Hour,
		StrongHistoryAge:     24 * time.Hour,
		StableIdentityFields: []string{"mac", "hostname", "host_id", "identity_confidence", "identity_source"},
	}
}

// stableFieldVocabulary is every Change.Field name Pro's tracker can emit
// (watch/tracker.go diffHostState). STABLE_IDENTITY_FIELDS must be a subset.
var stableFieldVocabulary = map[string]bool{
	"ip": true, "host_id": true, "identity_confidence": true, "identity_source": true,
	"hostname": true, "mac": true, "vendor": true, "os": true, "device": true,
	"source": true, "source_name": true, "alive": true, "weak": true, "ports": true,
	"metadata": true, "identity_aliases": true, "identity_anchor_keys": true,
}

// ResolveSecret returns the webhook secret from Secret or SecretFile.
func (c Config) ResolveSecret() (string, error) {
	if c.Secret != "" && c.SecretFile != "" {
		return "", fmt.Errorf("set exactly one of secret or secret file")
	}
	if c.Secret != "" {
		return checkSecretLength(c.Secret)
	}
	if c.SecretFile == "" {
		return "", fmt.Errorf("webhook secret is unset: refusing to start")
	}
	return readSecretFile(c.SecretFile, "webhook secret")
}

// ResolveModelKey returns the model API key from ModelAPIKey or
// ModelAPIKeyFile. Required exactly when the provider is meta; the fake
// provider needs no key.
func (c Config) ResolveModelKey() (string, error) {
	if c.ModelAPIKey != "" && c.ModelAPIKeyFile != "" {
		return "", fmt.Errorf("set exactly one of model API key or key file")
	}
	if c.ModelAPIKey != "" {
		return checkSecretLength(c.ModelAPIKey)
	}
	if c.ModelAPIKeyFile == "" {
		return "", fmt.Errorf("model API key is unset: refusing to start")
	}
	return readSecretFile(c.ModelAPIKeyFile, "model API key")
}

// readSecretFile enforces the secret-file rules: it must be a regular file
// with no group/other permission bits; a single trailing newline is
// trimmed. The handle is opened before stat so the checks apply to the
// file actually read.
func readSecretFile(path, what string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s file: %w", what, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s file: %w", what, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s file %s is not a regular file", what, path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s file %s must have no group/other permission bits", what, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s file: %w", what, err)
	}
	secret := strings.TrimSuffix(string(raw), "\n")
	secret = strings.TrimSuffix(secret, "\r")
	if secret == "" {
		return "", fmt.Errorf("%s is empty: refusing to start", what)
	}
	return checkSecretLength(secret)
}

func checkSecretLength(secret string) (string, error) {
	if len(secret) < minSecretBytes {
		return "", fmt.Errorf("webhook secret must be at least %d bytes", minSecretBytes)
	}
	return secret, nil
}

// Validate checks every cross-field inequality the ingest path depends on.
func (c Config) Validate() error {
	host, _, err := net.SplitHostPort(c.Bind)
	if err != nil {
		return fmt.Errorf("invalid bind %q: %w", c.Bind, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("bind %q is not loopback: refusing non-loopback ingest", c.Bind)
	}
	if c.Mode != ModeTap && c.Mode != ModeProxy {
		return fmt.Errorf("mode must be %q or %q", ModeTap, ModeProxy)
	}
	if c.Mode == ModeProxy {
		u, err := url.Parse(c.ScanRelayURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("proxy mode requires a valid http(s) ScanRelay URL, got %q", c.ScanRelayURL)
		}
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("max body bytes must be positive")
	}
	if c.MaxHeaderBytes <= 0 {
		return fmt.Errorf("max header bytes must be positive")
	}
	if c.TimestampTolerance <= 0 {
		return fmt.Errorf("timestamp tolerance must be positive")
	}
	if c.ScanRelayTolerance <= 0 {
		return fmt.Errorf("scanrelay tolerance must be positive")
	}
	if c.TimestampTolerance > c.ScanRelayTolerance {
		return fmt.Errorf("bridge tolerance %v must not exceed ScanRelay's %v, or forwarded events get rejected as stale",
			c.TimestampTolerance, c.ScanRelayTolerance)
	}
	want := c.ProxyForwardTimeout + c.PersistBudget + c.Margin
	if c.HandlerDeadline != want {
		return fmt.Errorf("handler deadline %v must equal proxy %v + persist %v + margin %v = %v",
			c.HandlerDeadline, c.ProxyForwardTimeout, c.PersistBudget, c.Margin, want)
	}
	if c.WriteTimeout <= c.HandlerDeadline {
		return fmt.Errorf("server write timeout %v must exceed handler deadline %v", c.WriteTimeout, c.HandlerDeadline)
	}
	if c.WriteTimeout >= c.ProSenderTimeout {
		return fmt.Errorf("server write timeout %v must stay below Pro sender timeout %v", c.WriteTimeout, c.ProSenderTimeout)
	}
	if c.HandlerDeadline >= c.ProSenderTimeout {
		return fmt.Errorf("handler deadline %v must stay below Pro sender timeout %v", c.HandlerDeadline, c.ProSenderTimeout)
	}
	for name, d := range map[string]time.Duration{
		"proxy forward timeout": c.ProxyForwardTimeout,
		"persist budget":        c.PersistBudget,
		"margin":                c.Margin,
		"read header timeout":   c.ReadHeaderTimeout,
		"read timeout":          c.ReadTimeout,
		"write timeout":         c.WriteTimeout,
		"idle timeout":          c.IdleTimeout,
		"shutdown grace":        c.ShutdownGrace,
	} {
		if d <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if c.ShutdownGrace < c.HandlerDeadline {
		return fmt.Errorf("shutdown grace %v must cover the handler deadline %v", c.ShutdownGrace, c.HandlerDeadline)
	}
	if c.InFlightCap <= 0 || c.WriterQueueCap <= 0 {
		return fmt.Errorf("in-flight and writer-queue caps must be positive")
	}
	if c.RetryAfterSecs <= 0 {
		return fmt.Errorf("retry-after seconds must be positive")
	}
	if c.SweepSafetyMargin <= 0 {
		return fmt.Errorf("sweep safety margin must be positive")
	}
	if c.SweepSafetyMargin >= c.TimestampTolerance {
		return fmt.Errorf("sweep margin %v must be smaller than timestamp tolerance %v", c.SweepSafetyMargin, c.TimestampTolerance)
	}
	if c.SweepBatch <= 0 {
		return fmt.Errorf("sweep batch must be positive")
	}
	if c.SweepWorkers != 1 {
		return fmt.Errorf("sweep workers must be exactly 1, got %d", c.SweepWorkers)
	}
	if c.MaxFieldChars <= 0 {
		return fmt.Errorf("max field chars must be positive")
	}
	if c.GroupQuietWindow <= 0 {
		return fmt.Errorf("group quiet window must be positive")
	}
	if c.GroupMaxWindow > 0 && c.GroupMaxWindow < c.GroupQuietWindow {
		return fmt.Errorf("group max window must be >= quiet window")
	}
	if c.GrouperTick <= 0 {
		return fmt.Errorf("grouper tick must be positive")
	}
	if c.GrouperTick > c.GroupQuietWindow {
		return fmt.Errorf("grouper tick %v must not exceed the quiet window %v, or closures lag a full window", c.GrouperTick, c.GroupQuietWindow)
	}
	if c.ProcessingLease <= 0 {
		return fmt.Errorf("processing lease must be positive")
	}
	if c.GrouperTick >= c.ProcessingLease {
		return fmt.Errorf("grouper tick %v must be below the processing lease %v, or a stale worker is never reaped in time", c.GrouperTick, c.ProcessingLease)
	}
	if c.EstablishedAge <= 0 || c.StrongHistoryAge <= 0 {
		return fmt.Errorf("established and strong-history ages must be positive")
	}
	if c.EstablishedAge > c.StrongHistoryAge {
		return fmt.Errorf("established age %v must not exceed strong-history age %v", c.EstablishedAge, c.StrongHistoryAge)
	}
	if len(c.StableIdentityFields) == 0 {
		return fmt.Errorf("stable identity fields must not be empty")
	}
	for _, f := range c.StableIdentityFields {
		if !stableFieldVocabulary[f] {
			return fmt.Errorf("stable identity field %q is not a known Pro change field", f)
		}
	}
	if c.ModelProvider != "meta" && c.ModelProvider != "fake" {
		return fmt.Errorf("model provider must be %q or %q, got %q", "meta", "fake", c.ModelProvider)
	}
	if c.ModelProvider == "meta" {
		if c.ModelID == "" {
			return fmt.Errorf("model ID is required with the meta provider")
		}
		u, err := url.Parse(c.ModelBaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("model base URL must be a valid http(s) endpoint, got %q", c.ModelBaseURL)
		}
		if c.ModelAPIKey == "" && c.ModelAPIKeyFile == "" {
			return fmt.Errorf("model API key is unset: refusing to start")
		}
		if c.ModelAPIKey != "" && c.ModelAPIKeyFile != "" {
			return fmt.Errorf("set exactly one of model API key or key file")
		}
	}
	if c.ProviderTimeout <= 0 {
		return fmt.Errorf("provider timeout must be positive")
	}
	if c.MaxModelResponseBytes <= 0 {
		return fmt.Errorf("max model response bytes must be positive")
	}
	// The stale-lease reaper must outlive two full provider budgets plus
	// margin: otherwise a slow (not hung) model plus its repair can see
	// the group re-taken while the first attempt is still running.
	if c.ProcessingLease <= 2*c.ProviderTimeout+c.Margin {
		return fmt.Errorf("processing lease %v must exceed 2 x provider timeout %v plus margin %v",
			c.ProcessingLease, c.ProviderTimeout, c.Margin)
	}
	return nil
}
