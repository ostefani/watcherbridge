// Package config owns WatcherBridge's receiver settings: every number the
// ingest path depends on lives here with cross-field validation, so the
// server, store policy, and later steps all read one source instead of
// baking constants into code (Designv5 §12).
package config

import (
	"fmt"
	"net"
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
	WriterQueueCap int // recorded for the store wiring; enforced by the writer queue depth
	RetryAfterSecs int

	SweepSafetyMargin time.Duration
	SweepBatch        int
	SweepWorkers      int // must be 1: a single worker keeps the sweep bounded

	ShutdownGrace time.Duration

	GroupQuietWindow time.Duration
	GroupMaxWindow   time.Duration
	GroupMaxEvents   int
}

// Default returns the approved starting thresholds (Designv5 §12/§15).
func Default() Config {
	return Config{
		Bind:               "127.0.0.1:8081",
		Mode:               ModeProxy,
		ScanRelayURL:       "http://127.0.0.1:8080",
		MaxBodyBytes:       1 << 20,
		MaxHeaderBytes:     16 << 10,
		TimestampTolerance: 300 * time.Second,

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

		GroupQuietWindow: 45 * time.Second,
		GroupMaxWindow:   120 * time.Second,
		GroupMaxEvents:   10,
	}
}

// ResolveSecret returns the webhook secret from Secret or SecretFile. File
// rules: it must be a regular file with no group/other permission bits; a
// single trailing newline is trimmed.
func (c Config) ResolveSecret() (string, error) {
	if c.Secret != "" && c.SecretFile != "" {
		return "", fmt.Errorf("set exactly one of secret or secret file")
	}
	if c.Secret != "" {
		return c.Secret, nil
	}
	if c.SecretFile == "" {
		return "", fmt.Errorf("webhook secret is unset: refusing to start")
	}
	fi, err := os.Stat(c.SecretFile)
	if err != nil {
		return "", fmt.Errorf("stat secret file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("secret file %s is not a regular file", c.SecretFile)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("secret file %s must have no group/other permission bits", c.SecretFile)
	}
	raw, err := os.ReadFile(c.SecretFile)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	secret := strings.TrimSuffix(string(raw), "\n")
	secret = strings.TrimSuffix(secret, "\r")
	if secret == "" {
		return "", fmt.Errorf("webhook secret is empty: refusing to start")
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
	if c.Mode == ModeProxy && c.ScanRelayURL == "" {
		return fmt.Errorf("proxy mode requires a ScanRelay URL")
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
	if c.GroupQuietWindow <= 0 {
		return fmt.Errorf("group quiet window must be positive")
	}
	if c.GroupMaxWindow > 0 && c.GroupMaxWindow < c.GroupQuietWindow {
		return fmt.Errorf("group max window must be >= quiet window")
	}
	return nil
}
