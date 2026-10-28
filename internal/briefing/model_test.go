package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/provider"
	"github.com/ostefani/watcherbridge/internal/redactor"
	"github.com/ostefani/watcherbridge/internal/store"
)

// tb is the testing contract the seed helpers need; *testing.T and
// *testing.B both satisfy it, so benchmarks reuse the same helpers.
type tb interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	TempDir() string
	Cleanup(func())
}

func modelDeps(t tb, s *store.Store, p provider.Provider) Deps {
	t.Helper()
	d := testDeps(t, s)
	d.Provider = p
	d.Redaction = redactor.Policy{Enabled: true, MaxFieldChars: 256}
	d.ModelTimeout = 5 * time.Second
	return d
}

// hostChangedGroup seeds the high-baseline fixture (new open port on an
// established stable host) and returns its closed group ID.
func hostChangedGroup(t tb, s *store.Store) string {
	t.Helper()
	return seedGroup(t, s, "host:w", "evt-w1", "host.changed",
		changedPayload("", `{"number":22,"protocol":"tcp","state":"open"}`))
}

func sortedTokensWithPrefix(issued map[string]string, prefix string) []string {
	var out []string
	for tok := range issued {
		if strings.HasPrefix(tok, prefix) {
			out = append(out, tok)
		}
	}
	sort.Strings(out)
	return out
}

// validAnswer builds a schema-valid model answer from the live bundle:
// group event IDs parsed from the payload, host and references from
// issued tokens. usedHost reports the chosen host token and its value so
// tests assert restoration without hardcoding token numbers.
func validAnswer(t tb, payload []byte, issued map[string]string, suggestion *string, usedHost *string, usedValue *string) []byte {
	t.Helper()
	var bundle struct {
		EventIDs []string `json:"event_ids"`
	}
	if err := json.Unmarshal(payload, &bundle); err != nil {
		t.Fatalf("fake received undecodable bundle: %v", err)
	}
	hosts := sortedTokensWithPrefix(issued, "HOST_")
	host := ""
	if len(hosts) > 0 {
		host = hosts[0]
	} else {
		for tok := range issued {
			host = tok
			break
		}
	}
	ips := sortedTokensWithPrefix(issued, "IP_")
	what := "observed activity"
	if len(ips) > 0 {
		what = "traffic from " + ips[0]
	}
	if usedHost != nil {
		*usedHost = host
	}
	if usedValue != nil {
		*usedValue = issued[host]
	}
	m := map[string]any{
		"event_ids": bundle.EventIDs, "host": host, "what_changed": what,
		"context": "context for " + host, "model_suggestion": suggestion,
		"priority_reason": "because reasons", "recommended_check": "look",
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func strptr(s string) *string { return &s }

func briefed(t tb, s *store.Store, gid string) store.Briefing {
	t.Helper()
	if st, _ := s.GroupStatus(context.Background(), gid); st != "done" {
		t.Fatalf("group must be done, got %q", st)
	}
	if needs, err := s.NeedsBriefing(context.Background(), 64); err != nil || len(needs) != 0 {
		t.Fatalf("queue must drain: %v %v", needs, err)
	}
	b, err := s.BriefingForGroup(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestModelPathStoresResolvedBriefing(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	var usedHost, usedValue string
	fake.Handle = func(repair bool, payload []byte, issued map[string]string) (provider.RawBriefing, error) {
		if repair {
			t.Fatal("valid answer must not trigger repair")
		}
		return provider.RawBriefing{JSON: validAnswer(t, payload, issued, strptr("critical"), &usedHost, &usedValue)}, nil
	}
	briefOne(context.Background(), modelDeps(t, s, fake), gid)
	b := briefed(t, s, gid)

	if b.FallbackUsed || b.ModelSuggestion == nil || *b.ModelSuggestion != "critical" {
		t.Fatalf("model fields wrong: %+v", b)
	}
	// Baseline high, model says critical: clamped to one step capped high.
	if b.Baseline != "high" || b.Priority != "high" || !b.Clamped {
		t.Fatalf("clamp wrong: %+v", b)
	}
	if b.PriorityReason != "because reasons" {
		t.Fatalf("reason must be the model's text, got %q", b.PriorityReason)
	}
	// Restoration: token references resolve to real values.
	if !strings.Contains(b.WhatChanged, "192.168.0.5") {
		t.Fatalf("IP not restored in %q", b.WhatChanged)
	}
	if b.Context != "context for "+usedValue {
		t.Fatalf("host not restored: %q (token %s)", b.Context, usedHost)
	}
	if b.ModelRawJSON == "" || strings.Contains(b.ModelRawJSON, "192.168.0.5") {
		t.Fatalf("audit trail must be tokenized: %q", b.ModelRawJSON)
	}
	// Trust boundary: the provider saw tokens, never raw values.
	if got := string(fake.Received[0]); !strings.Contains(got, "IP_") || strings.Contains(got, "192.168.0.5") {
		t.Fatalf("raw values crossed to provider: %s", got)
	}
}

func TestModelRepairThenSuccess(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	// Repair payload is correction text, not a bundle: stash the bundle
	// from the first call for the valid answer.
	var bundle []byte
	fake.Handle = func(repair bool, payload []byte, issued map[string]string) (provider.RawBriefing, error) {
		if !repair {
			bundle = append([]byte(nil), payload...)
			return provider.RawBriefing{JSON: []byte(`{"bogus":true}`)}, nil
		}
		if !strings.Contains(string(payload), "unknown field") {
			t.Fatalf("correction must cite the schema error, got %q", payload)
		}
		return provider.RawBriefing{JSON: validAnswer(t, bundle, issued, strptr("medium"), nil, nil)}, nil
	}
	briefOne(context.Background(), modelDeps(t, s, fake), gid)
	b := briefed(t, s, gid)
	if b.FallbackUsed || fake.Briefs != 1 || fake.Repairs != 1 {
		t.Fatalf("repair path wrong: %+v briefs=%d repairs=%d", b, fake.Briefs, fake.Repairs)
	}
	// Suggestion below baseline: stands pat, clamped recorded.
	if b.Priority != "high" || !b.Clamped {
		t.Fatalf("below-baseline suggestion must clamp: %+v", b)
	}
}

func TestModelPersistentInvalidFallsBack(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake(provider.FakeStep{JSON: []byte(`{"bogus":true}`)})
	briefOne(context.Background(), modelDeps(t, s, fake), gid)
	b := briefed(t, s, gid)
	if !b.FallbackUsed || b.Priority != "high" || b.ModelSuggestion != nil {
		t.Fatalf("fallback wrong: %+v", b)
	}
	if b.PriorityReason != fallbackReason {
		t.Fatalf("reason must be the exact §7.3 string, got %q", b.PriorityReason)
	}
	if fake.Briefs != 1 || fake.Repairs != 1 {
		t.Fatalf("expected one attempt plus one repair, got %d/%d", fake.Briefs, fake.Repairs)
	}
}

func TestModelTimeoutFallsBackOnSharedBudget(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake(provider.FakeStep{Hang: true})
	d := modelDeps(t, s, fake)
	d.ModelTimeout = 100 * time.Millisecond
	start := time.Now()
	briefOne(context.Background(), d, gid)
	if time.Since(start) > 10*time.Second {
		t.Fatal("hung provider must not stall the worker")
	}
	b := briefed(t, s, gid)
	if !b.FallbackUsed {
		t.Fatalf("timeout must fall back: %+v", b)
	}
	if fake.Briefs != 1 || fake.Repairs != 0 {
		t.Fatalf("provider error calls for no repair: %d/%d", fake.Briefs, fake.Repairs)
	}
}

func TestModelAttemptsShareOneDeadline(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake(
		provider.FakeStep{JSON: []byte(`not json`)},
		provider.FakeStep{JSON: []byte(`{"bogus":true}`)},
	)
	briefOne(context.Background(), modelDeps(t, s, fake), gid)
	if got := briefed(t, s, gid); !got.FallbackUsed {
		t.Fatalf("double-invalid must fall back: %+v", got)
	}
	if len(fake.Deadlines) != 2 {
		t.Fatalf("expected two attempts, got %d", len(fake.Deadlines))
	}
	if fake.Deadlines[0].IsZero() || fake.Deadlines[0] != fake.Deadlines[1] {
		t.Fatalf("both attempts must share one deadline: %v", fake.Deadlines)
	}
}

func TestModelClampNeedsNoRepair(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	fake.Handle = func(repair bool, payload []byte, issued map[string]string) (provider.RawBriefing, error) {
		return provider.RawBriefing{JSON: validAnswer(t, payload, issued, strptr("low"), nil, nil)}, nil
	}
	briefOne(context.Background(), modelDeps(t, s, fake), gid)
	b := briefed(t, s, gid)
	if b.FallbackUsed || fake.Repairs != 0 {
		t.Fatalf("clamping is the resolver's job, not a repair: %+v repairs=%d", b, fake.Repairs)
	}
	if b.Priority != "high" || !b.Clamped || *b.ModelSuggestion != "low" {
		t.Fatalf("below-baseline clamp wrong: %+v", b)
	}
}

func TestModelInventedTokenRepaired(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	var bundle []byte
	fake.Handle = func(repair bool, payload []byte, issued map[string]string) (provider.RawBriefing, error) {
		if !repair {
			bundle = append([]byte(nil), payload...)
			forged := validAnswer(t, payload, issued, strptr("high"), nil, nil)
			var m map[string]any
			if err := json.Unmarshal(forged, &m); err != nil {
				t.Fatal(err)
			}
			m["host"] = "HOST_999"
			fixed, _ := json.Marshal(m)
			return provider.RawBriefing{JSON: fixed}, nil
		}
		return provider.RawBriefing{JSON: validAnswer(t, bundle, issued, strptr("high"), nil, nil)}, nil
	}
	briefOne(context.Background(), modelDeps(t, s, fake), gid)
	b := briefed(t, s, gid)
	if b.FallbackUsed || fake.Repairs != 1 {
		t.Fatalf("invented token must repair once: %+v repairs=%d", b, fake.Repairs)
	}
	if *b.ModelSuggestion != "high" || b.Clamped || b.Priority != "high" {
		t.Fatalf("exact suggestion stands unclamped: %+v", b)
	}
}

func TestModelAbstainKeepsBaselineWithModelText(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	fake.Handle = func(repair bool, payload []byte, issued map[string]string) (provider.RawBriefing, error) {
		return provider.RawBriefing{JSON: validAnswer(t, payload, issued, nil, nil, nil)}, nil
	}
	briefOne(context.Background(), modelDeps(t, s, fake), gid)
	b := briefed(t, s, gid)
	if b.FallbackUsed || b.ModelSuggestion != nil || b.Clamped || b.Priority != "high" {
		t.Fatalf("abstain keeps baseline without fallback: %+v", b)
	}
	if !strings.Contains(b.WhatChanged, "192.168.0.5") {
		t.Fatalf("model text still used and restored: %q", b.WhatChanged)
	}
}

func TestLeakBlockedSkipsProvider(t *testing.T) {
	s := openWorkerStore(t)
	payload := `{"event_id":"e1","previous":{"ip":"192.168.0.5","hostname":"p","mac":"aa:bb:cc:dd:ee:01","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-20T00:00:00Z","ports":[]},"current":{"ip":"192.168.0.5","hostname":"p","mac":"aa:bb:cc:dd:ee:01","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-20T00:00:00Z","ports":[{"number":22,"protocol":"tcp","state":"open"}]},"evidence":"counter 000000000000000000000000 end"}`
	gid := seedGroup(t, s, "host:w", "evt-w9", "host.changed", payload)
	fake := provider.NewFake()
	briefOne(context.Background(), modelDeps(t, s, fake), gid)
	b := briefed(t, s, gid)
	if fake.Briefs != 0 {
		t.Fatal("leak-blocked bundle must never reach the provider")
	}
	if !b.FallbackUsed || !strings.Contains(b.Context, "redaction blocked") {
		t.Fatalf("leak must fall back loudly: %+v", b)
	}
}

func TestLastAttemptSkipsProvider(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	for i := 0; i < 3; i++ {
		taken, err := s.TakeGroup(context.Background(), gid)
		if err != nil {
			t.Fatal(err)
		}
		if taken.LastAttempt() {
			t.Fatalf("take %d must not be last", i+1)
		}
		if _, err := s.ReleaseGroup(context.Background(), gid); err != nil {
			t.Fatal(err)
		}
	}
	fake := provider.NewFake()
	briefOne(context.Background(), modelDeps(t, s, fake), gid)
	b := briefed(t, s, gid)
	if fake.Briefs != 0 {
		t.Fatal("final take must skip the provider")
	}
	if !b.FallbackUsed {
		t.Fatalf("final take writes the deterministic fallback: %+v", b)
	}
}

func TestRedactionOffPassesRawWithWarning(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	fake.Handle = func(repair bool, payload []byte, issued map[string]string) (provider.RawBriefing, error) {
		if len(issued) != 0 {
			t.Fatalf("redaction off issues no tokens, got %v", issued)
		}
		var bundle struct {
			EventIDs []string `json:"event_ids"`
		}
		if err := json.Unmarshal(payload, &bundle); err != nil {
			t.Fatal(err)
		}
		m := map[string]any{
			"event_ids": bundle.EventIDs, "host": "p", "what_changed": "traffic from 192.168.0.5",
			"context": "raw context", "model_suggestion": "high",
			"priority_reason": "raw reason", "recommended_check": "raw check",
		}
		b, _ := json.Marshal(m)
		return provider.RawBriefing{JSON: b}, nil
	}
	d := modelDeps(t, s, fake)
	d.Redaction = redactor.Policy{Enabled: false, MaxFieldChars: 256}
	briefOne(context.Background(), d, gid)
	b := briefed(t, s, gid)
	if b.FallbackUsed {
		t.Fatalf("raw answer must validate with tokens unenforced: %+v", b)
	}
	if !strings.Contains(string(fake.Received[0]), "192.168.0.5") {
		t.Fatal("operator chose raw: the bundle carries real values")
	}
	if !strings.Contains(b.WhatChanged, "192.168.0.5") {
		t.Fatalf("raw values stored: %q", b.WhatChanged)
	}
}

func BenchmarkBriefOneValid(b *testing.B) {
	s, err := store.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	fake := provider.NewFake()
	fake.Handle = func(repair bool, payload []byte, issued map[string]string) (provider.RawBriefing, error) {
		return provider.RawBriefing{JSON: validAnswer(b, payload, issued, strptr("high"), nil, nil)}, nil
	}
	d := testDeps(b, s)
	d.Provider = fake
	d.Redaction = redactor.Policy{Enabled: true, MaxFieldChars: 256}
	d.ModelTimeout = 5 * time.Second
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Unique event per iteration: a repeated event_id would take the
		// idempotent-duplicate path and the bench would measure nothing.
		gid := seedGroup(b, s, "host:w", fmt.Sprintf("evt-b%d", i), "host.changed",
			changedPayload("", `{"number":22,"protocol":"tcp","state":"open"}`))
		b.StartTimer()
		briefOne(context.Background(), d, gid)
	}
}

func TestDecodeModelBriefingRejects(t *testing.T) {
	full := `{"event_ids":["e1"],"host":"HOST_1","what_changed":"w","context":"c","model_suggestion":"high","priority_reason":"r","recommended_check":"k"}`
	for name, raw := range map[string]string{
		"unknown field": strings.Replace(full, `"recommended_check"`, `"bogus","recommended_check"`, 1),
		"trailing data": full + " {}",
		"empty field":   strings.Replace(full, `"what_changed":"w"`, `"what_changed":""`, 1),
		"not json":      `{"event_ids":`,
	} {
		if _, err := decodeModelBriefing([]byte(raw)); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
	if _, err := decodeModelBriefing([]byte(full)); err != nil {
		t.Fatalf("valid answer rejected: %v", err)
	}
}

func TestCheckModelBriefingRules(t *testing.T) {
	taken := store.TakenGroup{Events: []store.RawEvent{{EventID: "e1"}}}
	issued := redactor.TokenMap{"HOST_1": "w", "IP_1": "10.0.0.1"}
	base := modelBriefing{EventIDs: []string{"e1"}, Host: "HOST_1", What: "saw IP_1",
		Context: "c", Reason: "r", Check: "k"}
	sug := "medium"
	base.Suggestion = &sug
	if _, err := checkModelBriefing(base, taken, issued, true); err != nil {
		t.Fatalf("valid rejected: %v", err)
	}
	cases := map[string]func(*modelBriefing){
		"event mismatch": func(m *modelBriefing) { m.EventIDs = []string{"e2"} },
		"foreign host":   func(m *modelBriefing) { m.Host = "HOST_9" },
		"bad level":      func(m *modelBriefing) { s := "urgent"; m.Suggestion = &s },
		"invented token": func(m *modelBriefing) { m.What = "saw IP_9" },
	}
	for name, mutate := range cases {
		m := base
		mutate(&m)
		if _, err := checkModelBriefing(m, taken, issued, true); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
	// Redaction off: unissued host is raw data, not an invention; a
	// token-shaped string still fails.
	off := base
	off.Host = "printer-2"
	off.What = "saw 10.0.0.1"
	if _, err := checkModelBriefing(off, taken, redactor.TokenMap{}, false); err != nil {
		t.Fatalf("redaction-off raw host rejected: %v", err)
	}
	off.What = "saw IP_9"
	if _, err := checkModelBriefing(off, taken, redactor.TokenMap{}, false); err == nil {
		t.Fatal("token-shaped invention must fail even with redaction off")
	}
}
