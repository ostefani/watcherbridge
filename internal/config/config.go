// Package config owns WatcherBridge's receiver settings: every number the
// ingest path depends on lives here with cross-field validation, so the
// server, store policy, and later steps all read one source instead of
// baking constants into code (Designv5 §12).
package config

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ostefani/watcherbridge/internal/limits"
)

// Mode selects tap (persist + 202, all work async) or proxy (persist,
// forward to ScanRelay, then reply).
const (
	ModeTap   = "tap"
	ModeProxy = "proxy"
)

// minSecretBytes floors HMAC secret length at 256 bits. Short keys verify
// correctly but brute-force faster; refusing them is a visible startup
// error, so an operator with an older short secret must rotate it, not
// wonder why ingest silently changed. It applies to the webhook HMAC
// secret only. Provider-issued keys carry their own entropy, which the
// bridge cannot dictate, so those only refuse empty values.
const minSecretBytes = 32

// maxInflightBytes caps InFlightCap * MaxBodyBytes: concurrent
// max-size bodies must fit the process heap budget with room left for
// the writer queue, the store, and the provider call.
const maxInflightBytes = 64 << 20

// maxGroupEvents caps the operator's per-group event setting: with the
// byte budget bounding memory, the count bounds per-group CPU
// (single-transaction read, decode, tokenize) and bundle size.
const maxGroupEvents = 128

// Secret is key material that must never appear in logs, errors, or
// dumps: its formatted forms all render as ***. Convert back to string
// only at the exact use site (HMAC, outgoing header, provider client).
type Secret string

// String hides the value in every fmt verb, including %+v dumps.
func (s Secret) String() string { return "***" }

// GoString hides the value in %#v dumps.
func (s Secret) GoString() string { return "***" }

// LogValue hides the value from slog's structured logging.
func (s Secret) LogValue() slog.Value { return slog.StringValue("***") }

// MarshalJSON keeps the value out of JSON dumps (debug endpoints,
// marshalled configs): the wire form is the same redaction as the logs.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"***"`), nil }

// MarshalText keeps the value out of text encodings for the same reason.
func (s Secret) MarshalText() ([]byte, error) { return []byte("***"), nil }

// Config carries the Step 8 receiver settings. Durations are real
// time.Duration values; callers that read env/flags convert into these.
type Config struct {
	Bind string // loopback only; anything else is refused at startup
	Mode string // "tap" or "proxy"

	// Secret material. Exactly one of Secret / SecretFile must be set; the
	// value is never logged or stored in the DB. The file form is
	// preferred: environment values are visible to same-user processes
	// via /proc, while a 0600 file is readable only by this user.
	Secret     Secret
	SecretFile string

	ScanRelayURL string

	// Forward credentials for a ScanRelay that requires auth. Injected
	// into the outgoing request at send time; never stored or logged.
	// The Authorization header form is preferred when both are set,
	// mirroring ScanRelay's own check order.
	ForwardBearerToken  Secret
	ForwardAPIKey       Secret
	ForwardAPIKeyHeader string

	// ScanRelayTolerance is ScanRelay's own timestamp window. The bridge
	// must not accept events ScanRelay would reject as stale.
	ScanRelayTolerance time.Duration

	// ClockSkewAllowance is the assumed worst-case clock difference
	// between the bridge and ScanRelay hosts. It is added to the live
	// forward-path inequality, so an event accepted at the tolerance
	// edge still lands inside ScanRelay's window when the clocks
	// disagree by up to this much. Zero by default (same host or NTP).
	ClockSkewAllowance time.Duration

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

	SSEHeartbeat time.Duration // SSE comment ping; also dead-peer detection
	SSEReplayMax int           // replay cap per reconnect; over it is resync, never partial

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
	// ModelAPIKeyFile when the provider is meta. Never logged or stored;
	// the file form is preferred for the same reason as the webhook secret.
	ModelAPIKey     Secret
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
		TimestampTolerance:  240 * time.Second,

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

		SSEHeartbeat: 15 * time.Second,
		SSEReplayMax: 200,

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
		s, err := checkSecretLength(c.Secret, "webhook secret")
		return string(s), err
	}
	if c.SecretFile == "" {
		return "", fmt.Errorf("webhook secret is unset: refusing to start")
	}
	s, err := readSecretFile(c.SecretFile, "webhook secret")
	if err != nil {
		return "", err
	}
	floored, err := checkSecretLength(s, "webhook secret")
	return string(floored), err
}

// ResolveModelKey returns the model API key from ModelAPIKey or
// ModelAPIKeyFile. Required exactly when the provider is meta; the fake
// provider needs no key.
func (c Config) ResolveModelKey() (string, error) {
	if c.ModelAPIKey != "" && c.ModelAPIKeyFile != "" {
		return "", fmt.Errorf("set exactly one of model API key or key file")
	}
	if c.ModelAPIKey != "" {
		s, err := checkPresent(c.ModelAPIKey, "model API key")
		return string(s), err
	}
	if c.ModelAPIKeyFile == "" {
		return "", fmt.Errorf("model API key is unset: refusing to start")
	}
	s, err := readSecretFile(c.ModelAPIKeyFile, "model API key")
	return string(s), err
}

// readSecretFile enforces the secret-file rules: it must be a regular file
// with no group/other permission bits; a single trailing newline is
// trimmed. Everything — open, stat, and read — goes through the one
// handle, so the checks apply to the file actually read and a second
// open cannot slip a different file in between.
func readSecretFile(path, what string) (Secret, error) {
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
	// LimitReader caps the read itself: a multi-kilobyte file is a
	// mispointed path (a certificate, a keyring), and truncating it
	// would fail loudly as auth mismatches — refuse it instead of
	// buffering it. One byte over the cap is enough to tell.
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", fmt.Errorf("read %s file: %w", what, err)
	}
	if len(raw) > 4096 {
		return "", fmt.Errorf("%s file %s is too large to be a secret", what, path)
	}
	secret := strings.TrimSuffix(string(raw), "\n")
	secret = strings.TrimSuffix(secret, "\r")
	if secret == "" {
		return "", fmt.Errorf("%s is empty: refusing to start", what)
	}
	return Secret(secret), nil
}

func checkSecretLength(secret Secret, what string) (Secret, error) {
	if len(secret) < minSecretBytes {
		return "", fmt.Errorf("%s must be at least %d bytes", what, minSecretBytes)
	}
	return secret, nil
}

// checkPresent refuses an empty provider-issued key without dictating
// its length: bearer-key entropy is the provider's business.
func checkPresent(secret Secret, what string) (Secret, error) {
	if secret == "" {
		return "", fmt.Errorf("%s is empty: refusing to start", what)
	}
	return secret, nil
}

// loopbackOrHTTPS requires an endpoint that carries secrets or signed
// events to be either encrypted or loopback-local: https anywhere, or
// http only to loopback. "localhost" is matched literally; anything else
// must parse as a loopback IP.
func loopbackOrHTTPS(raw, what string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be a valid http(s) endpoint, got %q", what, raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if host := u.Hostname(); host == "localhost" {
		return nil
	} else if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%s must be https or loopback http, got %q", what, raw)
}

// hopByHopHeaders can never carry credentials: intermediaries may strip
// or rewrite them, and Authorization is the bearer slot.
var hopByHopHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true, "authorization": true,
}

// headerNameRe is the RFC 9110 token rule for field names.
var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+\-.^_` + "`" + `|~]+$`)

// checkForwardHeader keeps endpoint credentials out of signature and
// framing headers: X-SubnetLens-* is Pro's signature slot, hop-by-hop
// names are rewritten in transit, and anything else must be a valid
// field name.
func checkForwardHeader(name string) error {
	if name == "" {
		return fmt.Errorf("forward API key header must name a header")
	}
	if !headerNameRe.MatchString(name) {
		return fmt.Errorf("forward API key header %q is not a valid header name", name)
	}
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "x-subnetlens-") {
		return fmt.Errorf("forward API key header %q collides with the signature headers", name)
	}
	if hopByHopHeaders[lower] {
		return fmt.Errorf("forward API key header %q must not be a hop-by-hop or authorization header", name)
	}
	return nil
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
	if c.Mode == ModeProxy && c.ScanRelayURL == "" {
		return fmt.Errorf("proxy mode requires a ScanRelay URL")
	}
	// The annotation publisher forwards credentials whenever they are
	// set, in tap mode too — so any configured relay URL must be
	// loopback or encrypted, not just the proxy one.
	if c.ScanRelayURL != "" {
		if err := loopbackOrHTTPS(c.ScanRelayURL, "ScanRelay URL"); err != nil {
			return err
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
	// The live forward path costs up to a full handler deadline after
	// acceptance, plus clock disagreement between the hosts: an event
	// accepted at the tolerance edge would arrive past ScanRelay's
	// window and fail forever on every Pro retry.
	if c.ClockSkewAllowance < 0 {
		return fmt.Errorf("clock skew allowance must not be negative")
	}
	if c.TimestampTolerance+c.HandlerDeadline+c.ClockSkewAllowance > c.ScanRelayTolerance {
		return fmt.Errorf("bridge tolerance %v plus handler deadline %v plus skew %v must fit inside ScanRelay's %v, or edge events can never be delivered",
			c.TimestampTolerance, c.HandlerDeadline, c.ClockSkewAllowance, c.ScanRelayTolerance)
	}
	want := c.ProxyForwardTimeout + c.PersistBudget + c.Margin
	if c.HandlerDeadline < want {
		return fmt.Errorf("handler deadline %v must cover proxy %v + persist %v + margin %v = %v, with room for operator slack above",
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
	if product := int64(c.InFlightCap) * c.MaxBodyBytes; product > maxInflightBytes {
		return fmt.Errorf("in-flight cap %d x max body %d = %d bytes exceeds the %d-byte ingest budget",
			c.InFlightCap, c.MaxBodyBytes, product, maxInflightBytes)
	}
	if c.ForwardAPIKeyHeader == "" && c.ForwardAPIKey != "" {
		return fmt.Errorf("forward API key is set but no header names it")
	}
	if c.ForwardAPIKeyHeader != "" {
		if err := checkForwardHeader(c.ForwardAPIKeyHeader); err != nil {
			return err
		}
	}
	if c.RetryAfterSecs <= 0 {
		return fmt.Errorf("retry-after seconds must be positive")
	}
	if c.SSEHeartbeat <= 0 {
		return fmt.Errorf("SSE heartbeat must be positive")
	}
	if c.SSEReplayMax <= 0 {
		return fmt.Errorf("SSE replay cap must be positive")
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
	// Bounded on both axes: the count caps per-group CPU (TakeGroup reads
	// every payload in one write transaction) and the byte counter caps
	// memory; the window caps group lifetime so briefings stay fresh and
	// retention can proceed. A trickle feed with no time cap would hold
	// a group open for hours.
	if c.GroupMaxEvents < 1 || c.GroupMaxEvents > maxGroupEvents {
		return fmt.Errorf("group max events must be between 1 and %d", maxGroupEvents)
	}
	if c.GroupMaxWindow <= 0 {
		return fmt.Errorf("group max window must be positive")
	}
	if c.GroupMaxWindow < c.GroupQuietWindow {
		return fmt.Errorf("group max window must be >= quiet window")
	}
	if c.MaxBodyBytes > limits.MaxGroupBytes {
		return fmt.Errorf("max body bytes %d must fit the %d-byte group budget, or a single event never fits a fresh group",
			c.MaxBodyBytes, limits.MaxGroupBytes)
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
		// The API key and the bundle travel together: http is allowed
		// only to loopback, never across the network in cleartext.
		if err := loopbackOrHTTPS(c.ModelBaseURL, "model base URL"); err != nil {
			return err
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
