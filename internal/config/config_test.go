package config

import (
	"os"
	"path/filepath"
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

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*Config){
		"non-loopback bind":       func(c *Config) { c.Bind = "0.0.0.0:8081" },
		"bad mode":                func(c *Config) { c.Mode = "stream" },
		"proxy without URL":       func(c *Config) { c.Mode = ModeProxy; c.ScanRelayURL = "" },
		"proxy bad scheme":        func(c *Config) { c.Mode = ModeProxy; c.ScanRelayURL = "ftp://x/" },
		"proxy URL no host":       func(c *Config) { c.Mode = ModeProxy; c.ScanRelayURL = "http://" },
		"tolerance above relay":   func(c *Config) { c.TimestampTolerance = c.ScanRelayTolerance + time.Second },
		"handler != chain sum":    func(c *Config) { c.HandlerDeadline++ },
		"write below handler":     func(c *Config) { c.WriteTimeout = c.HandlerDeadline },
		"write above pro timeout": func(c *Config) { c.WriteTimeout = c.ProSenderTimeout },
		"handler above pro":       func(c *Config) { c.HandlerDeadline, c.WriteTimeout = c.ProSenderTimeout, 2*c.ProSenderTimeout },
		"grace below handler":     func(c *Config) { c.ShutdownGrace = time.Second },
		"margin above tolerance":  func(c *Config) { c.SweepSafetyMargin = c.TimestampTolerance },
		"tick above window":       func(c *Config) { c.GrouperTick = c.GroupQuietWindow + time.Second },
		"tick above lease":        func(c *Config) { c.GrouperTick = c.ProcessingLease },
		"ages inverted":           func(c *Config) { c.EstablishedAge, c.StrongHistoryAge = c.StrongHistoryAge, c.EstablishedAge },
		"unknown stable field":    func(c *Config) { c.StableIdentityFields = []string{"banner"} },
		"empty stable fields":     func(c *Config) { c.StableIdentityFields = nil },
		"zero in-flight":          func(c *Config) { c.InFlightCap = 0 },
		"two sweep workers":       func(c *Config) { c.SweepWorkers = 2 },
		"zero quiet window":       func(c *Config) { c.GroupQuietWindow = 0 },
		"max window below quiet":  func(c *Config) { c.GroupMaxWindow = time.Second },
		"group max below quiet":   func(c *Config) { c.GroupMaxWindow = c.GroupQuietWindow - time.Second },
	}
	for name, mutate := range cases {
		c := Default()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestSecretFileRules(t *testing.T) {
	dir := t.TempDir()

	ok := filepath.Join(dir, "secret")
	if err := os.WriteFile(ok, []byte("long-enough-secret-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.SecretFile = ok
	got, err := c.ResolveSecret()
	if err != nil || got != "long-enough-secret-1" {
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

	world := filepath.Join(dir, "world")
	if err := os.WriteFile(world, []byte("long-enough-secret-2"), 0o644); err != nil {
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
