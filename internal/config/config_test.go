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
		"proxy without URL":       func(c *Config) { c.ScanRelayURL = "" },
		"handler != chain sum":    func(c *Config) { c.HandlerDeadline++ },
		"write below handler":     func(c *Config) { c.WriteTimeout = c.HandlerDeadline },
		"write above pro timeout": func(c *Config) { c.WriteTimeout = c.ProSenderTimeout },
		"handler above pro":       func(c *Config) { c.HandlerDeadline, c.WriteTimeout = c.ProSenderTimeout, 2*c.ProSenderTimeout },
		"grace below handler":     func(c *Config) { c.ShutdownGrace = time.Second },
		"margin above tolerance":  func(c *Config) { c.SweepSafetyMargin = c.TimestampTolerance },
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
	if err := os.WriteFile(ok, []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.SecretFile = ok
	got, err := c.ResolveSecret()
	if err != nil || got != "s3cr3t" {
		t.Fatalf("trailing newline must be trimmed: %q, %v", got, err)
	}

	world := filepath.Join(dir, "world")
	if err := os.WriteFile(world, []byte("x"), 0o644); err != nil {
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
	if _, err := c.ResolveSecret(); err == nil {
		t.Fatal("missing secret must refuse to start")
	}
}
