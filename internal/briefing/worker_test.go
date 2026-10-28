package briefing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

func testDeps(t *testing.T, s *store.Store) Deps {
	t.Helper()
	return Deps{
		Store:  s,
		Groups: store.GroupPolicy{QuietWindow: time.Minute, MaxWindow: time.Hour, MaxEvents: 10},
		Tick:   10 * time.Millisecond,
		Lease:  time.Minute,
		Now:    time.Now,
		Policy: Policy{
			EstablishedAge:   time.Hour,
			StrongHistoryAge: 24 * time.Hour,
			StableFields:     []string{"mac", "hostname", "host_id", "identity_confidence", "identity_source"},
		},
	}
}

func seedGroup(t *testing.T, s *store.Store, anchor, eventID, eventType, payload string) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	res, err := s.PersistEventAndGroup(context.Background(), store.RawEvent{
		EventID: eventID, EventKey: "k", EventType: eventType,
		OccurredAt: now, Payload: []byte(payload), ReceivedAt: now, SignatureTimestamp: now,
	}, anchor, store.GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return res.GroupID
}

func changedPayload(prevPorts, curPorts string) string {
	return `{"event_id":"e1","previous":{"ip":"192.168.0.5","hostname":"p","mac":"aa:bb:cc:dd:ee:01","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-20T00:00:00Z","ports":[` + prevPorts + `]},"current":{"ip":"192.168.0.5","hostname":"p","mac":"aa:bb:cc:dd:ee:01","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-20T00:00:00Z","ports":[` + curPorts + `]}}`
}

func TestWorkerDrainWritesMinimalBriefing(t *testing.T) {
	s := openWorkerStore(t)
	gid := seedGroup(t, s, "host:w", "evt-w1", "host.changed", changedPayload("", `{"number":22,"protocol":"tcp","state":"open"}`))
	d := testDeps(t, s)
	drainOnce(context.Background(), d, time.Now())

	if needs, err := s.NeedsBriefing(context.Background(), 64); err != nil || len(needs) != 0 {
		t.Fatalf("drain must empty the queue: %v %v", needs, err)
	}
	if st, _ := s.GroupStatus(context.Background(), gid); st != "done" {
		t.Fatalf("group must be done, got %q", st)
	}
	b, err := s.BriefingForGroup(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	if b.Baseline != "high" || b.Priority != "high" || b.BaselineRule != RuleHighNewService {
		t.Fatalf("unexpected briefing: %+v", b)
	}
	if b.ModelSuggestion != nil || b.Clamped || !b.FallbackUsed {
		t.Fatalf("model fields must be null/false/true: %+v", b)
	}
	if b.PriorityReason != fallbackReason {
		t.Fatalf("reason must be the exact §7.3 string, got %q", b.PriorityReason)
	}
	if b.WhatChanged == "" || b.Context == "" || b.RecommendedCheck == "" {
		t.Fatal("fixed-template text fields must be non-empty")
	}
}

func TestWorkerLastAttemptWritesDirectly(t *testing.T) {
	s := openWorkerStore(t)
	gid := seedGroup(t, s, "host:l", "evt-l1", "host.appeared", `{"event_id":"e","host":{"ip":"10.0.0.9","weak":false,"alive":true,"identity_confidence":"low"}}`)
	d := testDeps(t, s)
	// Three take/release cycles (releases do not count); briefOne performs
	// the 4th take itself and must write the fallback directly.
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
	briefOne(context.Background(), d, gid)
	if st, _ := s.GroupStatus(context.Background(), gid); st != "done" {
		t.Fatalf("last attempt must converge to done, got %q", st)
	}
	if _, err := s.BriefingForGroup(context.Background(), gid); err != nil {
		t.Fatalf("last-attempt briefing must exist: %v", err)
	}
}

func TestWorkerParkFailedOnStoreError(t *testing.T) {
	s := openWorkerStore(t)
	gid := seedGroup(t, s, "host:p", "evt-p1", "host.appeared", `{"event_id":"e","host":{"ip":"10.0.0.9"}}`)
	if _, err := s.TakeGroup(context.Background(), gid); err != nil {
		t.Fatal(err)
	}
	d := testDeps(t, s)
	parkFailed(context.Background(), d, gid, "test panic")
	if st, _ := s.GroupStatus(context.Background(), gid); st != "failed" {
		t.Fatalf("parkFailed must converge to failed, got %q", st)
	}
	if _, err := s.BriefingForGroup(context.Background(), gid); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed group must carry no invented briefing: %v", err)
	}
}

func TestWorkerRunStopsOnCancel(t *testing.T) {
	s := openWorkerStore(t)
	d := testDeps(t, s)
	for i := 0; i < 3; i++ {
		seedGroup(t, s, fmt.Sprintf("host:c%d", i), fmt.Sprintf("evt-c%d", i), "host.appeared", `{"event_id":"e","host":{"ip":"10.0.0.9"}}`)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := Run(ctx, d); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancelled Run must return promptly")
	}
	if needs, _ := s.NeedsBriefing(context.Background(), 64); len(needs) != 3 {
		t.Fatal("cancelled run must brief nothing")
	}
	before := runtime.NumGoroutine()
	ctx2, cancel2 := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = Run(ctx2, d) }()
	time.Sleep(50 * time.Millisecond)
	cancel2()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	time.Sleep(50 * time.Millisecond)
	if n := runtime.NumGoroutine(); n > before+2 {
		t.Fatalf("goroutine leak: before=%d after=%d", before, n)
	}
}

func TestWorkerDrainChecksCtx(t *testing.T) {
	s := openWorkerStore(t)
	d := testDeps(t, s)
	seedGroup(t, s, "host:x", "evt-x1", "host.appeared", `{"event_id":"e"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	drainOnce(ctx, d, time.Now())
	if needs, _ := s.NeedsBriefing(context.Background(), 64); len(needs) != 1 {
		t.Fatal("cancelled drain must leave state consistent and untouched")
	}
}

// TestKill9MidProcessing kills a real subprocess between TakeGroup and
// InsertBriefing, then proves the restart yields exactly one briefing.
func TestKill9MidProcessing(t *testing.T) {
	if os.Getenv("BRIEFING_KILL9_CHILD") == "1" {
		kill9Child()
		t.Fatal("child must not return")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kill9.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	res, err := s.PersistEventAndGroup(context.Background(), store.RawEvent{
		EventID: "evt-k9", EventKey: "k", EventType: "host.changed",
		OccurredAt: now, Payload: []byte(changedPayload("", `{"number":22,"protocol":"tcp","state":"open"}`)),
		ReceivedAt: now, SignatureTimestamp: now,
	}, "host:k9", store.GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	s.Close() // only the child may hold the writer now

	child := exec.Command(os.Args[0], "-test.run", "TestKill9MidProcessing")
	child.Env = append(os.Environ(), "BRIEFING_KILL9_CHILD=1", "BRIEFING_KILL9_DB="+dbPath, "BRIEFING_KILL9_GROUP="+res.GroupID)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		probe, err := store.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := probe.GroupStatus(context.Background(), res.GroupID)
		probe.Close()
		if st == "processing" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = child.Process.Wait()

	s2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	d := testDeps(t, s2)
	if _, err := s2.Recover(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	drainOnce(context.Background(), d, time.Now())
	if st, _ := s2.GroupStatus(context.Background(), res.GroupID); st != "done" {
		t.Fatalf("restart must converge to done, got %q", st)
	}
	if _, err := s2.BriefingForGroup(context.Background(), res.GroupID); err != nil {
		t.Fatalf("restart must yield exactly one briefing: %v", err)
	}
}

func kill9Child() {
	dbPath := os.Getenv("BRIEFING_KILL9_DB")
	groupID := os.Getenv("BRIEFING_KILL9_GROUP")
	s, err := store.Open(dbPath)
	if err != nil {
		os.Exit(2)
	}
	defer s.Close()
	if _, err := s.TakeGroup(context.Background(), groupID); err != nil {
		os.Exit(3)
	}
	select {} // killed here: processing, no briefing
}

func FuzzBaselineDecode(f *testing.F) {
	pol := Policy{
		EstablishedAge:   time.Hour,
		StrongHistoryAge: 24 * time.Hour,
		StableFields:     []string{"mac", "hostname", "host_id", "identity_confidence", "identity_source"},
	}
	asOf := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f.Add("host.changed", []byte(`{"event_id":"e","current":{"ip":"1.2.3.4"}}`))
	f.Add("host.appeared", []byte(`{"event_id":"e","host":{"weak":true}}`))
	f.Add("host.disappeared", []byte(`not json`))
	f.Fuzz(func(t *testing.T, eventType string, payload []byte) {
		ev := DecodeEvent(eventType, payload)
		out, err := Baseline([]GroupEvent{ev}, asOf, pol)
		if err != nil {
			return // only empty groups error; never here
		}
		if out.Level < Low || out.Level > Critical || out.RuleID == "" {
			t.Fatalf("invalid outcome: %+v", out)
		}
		if out.Level == Critical && out.RuleID != RuleCritIdentityService {
			t.Fatalf("critical outside the allowlist: %+v", out)
		}
	})
}

func BenchmarkBaseline(b *testing.B) {
	pol := Policy{
		EstablishedAge:   time.Hour,
		StrongHistoryAge: 24 * time.Hour,
		StableFields:     []string{"mac", "hostname", "host_id", "identity_confidence", "identity_source"},
	}
	asOf := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	events := []GroupEvent{DecodeEvent("host.changed", []byte(changedPayload("", `{"number":22,"protocol":"tcp","state":"open"}`)))}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Baseline(events, asOf, pol); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTick(b *testing.B) {
	dir := b.TempDir()
	s, err := store.Open(filepath.Join(dir, "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		payload := []byte(`{"event_id":"e","host":{"ip":"10.0.0.1"}}`)
		_, err := s.PersistEventAndGroup(ctx, store.RawEvent{
			EventID: fmt.Sprintf("evt-b%d", i), EventKey: "k", EventType: "host.appeared",
			OccurredAt: now, Payload: payload, ReceivedAt: now, SignatureTimestamp: now,
		}, fmt.Sprintf("host:b%d", i), store.GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour})
		if err != nil {
			b.Fatal(err)
		}
	}
	if _, err := s.CloseDue(ctx, now.Add(time.Second)); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.CloseDue(ctx, now); err != nil {
			b.Fatal(err)
		}
		if _, err := s.ReleaseStale(ctx, now, time.Minute); err != nil {
			b.Fatal(err)
		}
	}
}

func openWorkerStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
