package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultPairs(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	if c.Mode != ModeTap {
		t.Fatalf("default mode must be tap (README preference), got %q", c.Mode)
	}
	if !c.Redaction {
		t.Fatal("redaction must default on")
	}
	if c.MaxFieldChars != 256 {
		t.Fatalf("default field cap must be 256, got %d", c.MaxFieldChars)
	}
	// The approved chain: handler = proxy + persist + margin, below Pro's 10s.
	if want := c.ProxyForwardTimeout + c.PersistBudget + c.Margin; c.HandlerDeadline != want {
		t.Fatalf("handler deadline %v != proxy+persist+margin %v", c.HandlerDeadline, want)
	}
	if c.WriteTimeout <= c.HandlerDeadline || c.WriteTimeout >= c.ProSenderTimeout {
		t.Fatalf("write timeout %v must sit between handler %v and Pro %v",
			c.WriteTimeout, c.HandlerDeadline, c.ProSenderTimeout)
	}
	if c.ShutdownGrace < c.HandlerDeadline {
		t.Fatalf("shutdown grace %v must cover handler %v", c.ShutdownGrace, c.HandlerDeadline)
	}
}

func TestOverviewWindowChain(t *testing.T) {
	good := Default()
	if err := good.Validate(); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	cases := map[string]func(*Config){
		"tick below range":    func(c *Config) { c.OverviewTick = 4 * time.Second },
		"tick above range":    func(c *Config) { c.OverviewTick = 6 * time.Minute },
		"window above max":    func(c *Config) { c.OverviewWindow = 169 * time.Hour },
		"max above retention": func(c *Config) { c.OverviewMaxWindow = 31 * 24 * time.Hour },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := Default()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*Config){
		"rule propose-after below one":     func(c *Config) { c.RuleProposeAfter = 0 },
		"rule default ttl non-positive":    func(c *Config) { c.RuleDefaultTTL = 0 },
		"rule retention below a day":       func(c *Config) { c.RulePurgeAfter = 23 * time.Hour },
		"non-loopback bind":                func(c *Config) { c.Bind = "0.0.0.0:8081" },
		"bad mode":                         func(c *Config) { c.Mode = "stream" },
		"briefing retention below a day":   func(c *Config) { c.BriefingRetention = 23 * time.Hour },
		"subject retention below briefing": func(c *Config) { c.SubjectRetention = c.BriefingRetention - time.Hour },
		"purge interval below a minute":    func(c *Config) { c.PurgeInterval = 30 * time.Second },
		"proxy without URL":                func(c *Config) { c.Mode = ModeProxy; c.ScanRelayURL = "" },
		"proxy bad scheme":                 func(c *Config) { c.Mode = ModeProxy; c.ScanRelayURL = "ftp://x/" },
		"proxy URL no host":                func(c *Config) { c.Mode = ModeProxy; c.ScanRelayURL = "http://" },
		"tolerance above relay":            func(c *Config) { c.TimestampTolerance = c.ScanRelayTolerance + time.Second },
		"handler below chain sum":          func(c *Config) { c.HandlerDeadline-- },
		"tolerance plus handler above relay": func(c *Config) {
			c.TimestampTolerance = c.ScanRelayTolerance
		},
		"skew pushing edge over relay window": func(c *Config) {
			c.ClockSkewAllowance = c.ScanRelayTolerance
		},
		"negative skew": func(c *Config) {
			c.ClockSkewAllowance = -time.Second
		},
		"proxy http non-loopback": func(c *Config) {
			c.Mode = ModeProxy
			c.ScanRelayURL = "http://192.0.2.1:8080"
		},
		"tap http non-loopback relay": func(c *Config) {
			c.ScanRelayURL = "http://192.0.2.1:8080"
		},
		"meta http non-loopback base": func(c *Config) {
			c.ModelProvider = "meta"
			c.ModelID = "m"
			c.ModelAPIKey = Secret("0123456789abcdef0123456789abcdef")
			c.ModelBaseURL = "http://192.0.2.1/v1"
		},
		"forward key without header": func(c *Config) {
			c.ForwardAPIKey = "x"
			c.ForwardAPIKeyHeader = ""
		},
		"forward header signature slot": func(c *Config) {
			c.ForwardAPIKey = "x"
			c.ForwardAPIKeyHeader = "X-SubnetLens-Api-Key"
		},
		"forward header hop-by-hop": func(c *Config) {
			c.ForwardAPIKey = "x"
			c.ForwardAPIKeyHeader = "Connection"
		},
		"forward header not a token": func(c *Config) {
			c.ForwardAPIKey = "x"
			c.ForwardAPIKeyHeader = "Bad Header"
		},
		"negative group max events": func(c *Config) { c.GroupMaxEvents = -1 },
		"zero group max events":     func(c *Config) { c.GroupMaxEvents = 0 },
		"group max events above ceiling": func(c *Config) {
			c.GroupMaxEvents = 129
		},
		"zero group max window": func(c *Config) { c.GroupMaxWindow = 0 },
		"max body above in-flight budget": func(c *Config) {
			c.MaxBodyBytes = (8 << 20) + 1
		},
		"in-flight bytes above budget": func(c *Config) { c.InFlightCap = 65 },
		"write below handler":          func(c *Config) { c.WriteTimeout = c.HandlerDeadline },
		"write above pro timeout":      func(c *Config) { c.WriteTimeout = c.ProSenderTimeout },
		"handler above pro":            func(c *Config) { c.HandlerDeadline, c.WriteTimeout = c.ProSenderTimeout, 2*c.ProSenderTimeout },
		"grace below handler":          func(c *Config) { c.ShutdownGrace = time.Second },
		"margin above tolerance":       func(c *Config) { c.SweepSafetyMargin = c.TimestampTolerance },
		"tick above window":            func(c *Config) { c.GrouperTick = c.GroupQuietWindow + time.Second },
		"tick above lease":             func(c *Config) { c.GrouperTick = c.ProcessingLease },
		"ages inverted":                func(c *Config) { c.EstablishedAge, c.StrongHistoryAge = c.StrongHistoryAge, c.EstablishedAge },
		"unknown stable field":         func(c *Config) { c.StableIdentityFields = []string{"banner"} },
		"empty stable fields":          func(c *Config) { c.StableIdentityFields = nil },
		"zero in-flight":               func(c *Config) { c.InFlightCap = 0 },
		"two sweep workers":            func(c *Config) { c.SweepWorkers = 2 },
		"unknown model provider":       func(c *Config) { c.ModelProvider = "openai" },
		"meta without model ID":        func(c *Config) { c.ModelProvider = "meta" },
		"meta without base URL":        func(c *Config) { c.ModelProvider = "meta"; c.ModelID = "m" },
		"meta without key":             func(c *Config) { c.ModelProvider = "meta"; c.ModelID = "m"; c.ModelBaseURL = "https://m.example/v1" },
		"meta key and key file": func(c *Config) {
			c.ModelProvider = "meta"
			c.ModelID = "m"
			c.ModelBaseURL = "https://m.example/v1"
			c.ModelAPIKey = "0123456789abcdef"
			c.ModelAPIKeyFile = "f"
		},
		"zero provider timeout":   func(c *Config) { c.ProviderTimeout = 0 },
		"zero response cap":       func(c *Config) { c.MaxModelResponseBytes = 0 },
		"lease below 2x provider": func(c *Config) { c.ProcessingLease = 2*c.ProviderTimeout + c.Margin },
		"zero SSE heartbeat":      func(c *Config) { c.SSEHeartbeat = 0 },
		"zero SSE replay cap":     func(c *Config) { c.SSEReplayMax = 0 },
		"oversize SSE replay cap": func(c *Config) { c.SSEReplayMax = 501 },
		"zero SSE write timeout":  func(c *Config) { c.SSEWriteTimeout = 0 },
		"SSE write over grace":    func(c *Config) { c.SSEWriteTimeout = c.ShutdownGrace },
		"zero SSE subscribers":    func(c *Config) { c.SSEMaxSubscribers = 0 },
		"blank viewer host":       func(c *Config) { c.ViewerAllowedHosts = []string{"127.0.0.1:8081", " "} },
		"zero agent timeout":      func(c *Config) { c.AgentTimeout = 0 },
		"agent below provider":    func(c *Config) { c.AgentTimeout = c.ProviderTimeout - time.Second },
		"agent below call+repair": func(c *Config) { c.AgentTimeout = 2 * c.ProviderTimeout },
		"lease at 2x agent":       func(c *Config) { c.ProcessingLease = 2*c.AgentTimeout + c.Margin },
		"zero quiet window":       func(c *Config) { c.GroupQuietWindow = 0 },
		"max window below quiet":  func(c *Config) { c.GroupMaxWindow = time.Second },
		"group max below quiet":   func(c *Config) { c.GroupMaxWindow = c.GroupQuietWindow - time.Second },
		"empty source ID":         func(c *Config) { c.SourceID = "" },
		"source ID with space":    func(c *Config) { c.SourceID = "lab 1" },
		"source ID with slash":    func(c *Config) { c.SourceID = "lab/1" },
		"source ID too long":      func(c *Config) { c.SourceID = strings.Repeat("s", 129) },
		// Designv9 §6.1/§6.4: the loop step count and baseline floors
		// are startup refusals, never silent routing.
		"agent steps zero":       func(c *Config) { c.AgentMaxSteps = 0 },
		"agent steps above four": func(c *Config) { c.AgentMaxSteps = 5 },
		"agent steps negative":   func(c *Config) { c.AgentMaxSteps = -1 },
		"unknown model floor":    func(c *Config) { c.ModelMinBaseline = "urgent" },
		"unknown agent floor":    func(c *Config) { c.AgentMinBaseline = "URGENT" },
		"blank model floor":      func(c *Config) { c.ModelMinBaseline = "  " },
		"floors inverted": func(c *Config) {
			c.ModelMinBaseline, c.AgentMinBaseline = "high", "medium"
		},
	}
	for name, mutate := range cases {
		c := Default()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

// The lease margin is the explicit 30s hand-off margin from Designv8
// §6.8, not the 1s handler Margin: 182s with a 90s agent budget refuses,
// 209s refuses, and the 210s default (exactly 2×90s+30s) passes.
func TestProcessingLeaseMargin(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default 210s lease must pass: %v", err)
	}
	for _, lease := range []time.Duration{182 * time.Second, 209 * time.Second} {
		c := Default()
		c.ProcessingLease = lease
		if err := c.Validate(); err == nil {
			t.Fatalf("%v lease with a 90s agent budget must refuse", lease)
		}
	}
}

// The group-budget rule needs a low in-flight cap to isolate: with the
// default cap of 16, any body above 4 MiB already fails the in-flight
// product check first, and the table above would pass without ever
// reaching the group-budget comparison.
func TestMaxBodyAgainstGroupBudget(t *testing.T) {
	c := Default()
	c.InFlightCap = 1
	c.MaxBodyBytes = (8 << 20) + 1
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "group budget") {
		t.Fatalf("over-budget body must fail on the group budget rule, got: %v", err)
	}

	c = Default()
	c.InFlightCap = 1
	c.MaxBodyBytes = 8 << 20
	if err := c.Validate(); err != nil {
		t.Fatalf("a body exactly at the group budget must validate: %v", err)
	}
}

func TestSecretFileRules(t *testing.T) {
	dir := t.TempDir()

	ok := filepath.Join(dir, "secret")
	if err := os.WriteFile(ok, []byte("long-enough-secret-1-0123456789ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.SecretFile = ok
	got, err := c.ResolveSecret()
	if err != nil || got != "long-enough-secret-1-0123456789ab" {
		t.Fatalf("trailing newline must be trimmed: %q, %v", got, err)
	}

	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("tiny"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.SecretFile = short
	if _, err := c.ResolveSecret(); err == nil {
		t.Fatal("short secret file must be refused")
	}

	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, append([]byte("0123456789abcdef0123456789abcdef"), make([]byte, 8192)...), 0o600); err != nil {
		t.Fatal(err)
	}
	c.SecretFile = big
	if _, err := c.ResolveSecret(); err == nil {
		t.Fatal("multi-kilobyte secret file must be refused, not truncated")
	}

	world := filepath.Join(dir, "world")
	if err := os.WriteFile(world, []byte("long-enough-secret-2-0123456789ab"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.SecretFile = world
	if _, err := c.ResolveSecret(); err == nil {
		t.Fatal("group/world-readable secret file must be refused")
	}

	c.Secret = "a"
	c.SecretFile = ok
	if _, err := c.ResolveSecret(); err == nil {
		t.Fatal("both secret and secret file must be refused")
	}

	c = Default()
	c.Secret = "short"
	if _, err := c.ResolveSecret(); err == nil {
		t.Fatal("short inline secret must be refused")
	}

	c = Default()
	if _, err := c.ResolveSecret(); err == nil {
		t.Fatal("missing secret must refuse to start")
	}
}

func TestValidateAllows(t *testing.T) {
	cases := map[string]func(*Config){
		// Operator slack above the chain sum is allowed.
		"handler slack": func(c *Config) { c.HandlerDeadline += time.Second },
		// Encrypted relays and loopback http are both fine.
		"https relay": func(c *Config) {
			c.Mode = ModeProxy
			c.ScanRelayURL = "https://relay.example:8443"
		},
		"localhost relay": func(c *Config) {
			c.ScanRelayURL = "http://localhost:8080/hook"
		},
		"meta https base": func(c *Config) {
			c.ModelProvider = "meta"
			c.ModelID = "m"
			c.ModelAPIKey = Secret("0123456789abcdef0123456789abcdef")
			c.ModelBaseURL = "https://api.example/v1"
		},
		"meta http loopback base": func(c *Config) {
			c.ModelProvider = "meta"
			c.ModelID = "m"
			c.ModelAPIKey = Secret("0123456789abcdef0123456789abcdef")
			c.ModelBaseURL = "http://127.0.0.1:11434/v1"
		},
		"forward key with custom header": func(c *Config) {
			c.ForwardAPIKey = Secret("0123456789abcdef0123456789abcdef")
			c.ForwardAPIKeyHeader = "X-Relay-Key"
		},
		// The count ceiling still leaves headroom: 128 events fit well
		// under the bundle cap at typical sizes.
		"max events at ceiling": func(c *Config) { c.GroupMaxEvents = 128 },
		// The ingest budget headroom: 16 x 1MiB fits in 64MiB.
		"in-flight bytes at budget": func(c *Config) { c.InFlightCap = 64 },
		// Skew filling the edge inequality exactly still fits.
		"skew at edge boundary": func(c *Config) {
			c.ClockSkewAllowance = c.ScanRelayTolerance - c.TimestampTolerance - c.HandlerDeadline
		},
		// Equal floors give the level to the single call; mixed case
		// normalizes. One tool turn is a legal (if minimal) loop.
		"equal floors": func(c *Config) { c.ModelMinBaseline, c.AgentMinBaseline = "high", "high" },
		"mixed-case floors": func(c *Config) {
			c.ModelMinBaseline, c.AgentMinBaseline = "Medium", "HIGH"
		},
		"agent steps at one": func(c *Config) { c.AgentMaxSteps = 1 },
	}
	for name, mutate := range cases {
		c := Default()
		mutate(&c)
		if err := c.Validate(); err != nil {
			t.Errorf("%s: expected valid config, got %v", name, err)
		}
	}
}

func TestSecretNeverPrints(t *testing.T) {
	s := Secret("0123456789abcdef0123456789abcdef")
	for _, got := range []string{
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%+v", s),
		fmt.Sprintf("%#v", s),
		fmt.Sprintf("%+v", Default()),
	} {
		if strings.Contains(got, "0123456789abcdef") || strings.Contains(got, string(s)) {
			t.Fatalf("secret material leaked into formatted output: %q", got)
		}
	}
	if !strings.Contains(fmt.Sprintf("%v", s), "***") {
		t.Fatal("secret redaction marker missing")
	}
	// Encoded forms redact too: a marshalled config or debug dump must
	// never carry key material.
	jb, err := json.Marshal(s)
	if err != nil || string(jb) != `"***"` {
		t.Fatalf("secret JSON must redact, got %q %v", jb, err)
	}
	tb, err := s.MarshalText()
	if err != nil || string(tb) != "***" {
		t.Fatalf("secret text must redact, got %q %v", tb, err)
	}
}

// The 32-byte floor is HMAC-only: provider-issued keys are accepted at
// any non-empty length, while a short webhook secret still refuses.
func TestSecretFloorScope(t *testing.T) {
	c := Default()
	c.ModelProvider = "meta"
	c.ModelID = "m"
	c.ModelBaseURL = "https://m.example/v1"
	c.ModelAPIKey = "short-key"
	if _, err := c.ResolveModelKey(); err != nil {
		t.Fatalf("short provider key must resolve: %v", err)
	}
	c.Secret = "short"
	if _, err := c.ResolveSecret(); err == nil {
		t.Fatal("short webhook secret must be refused")
	}
}
