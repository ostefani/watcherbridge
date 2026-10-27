package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPersistEventAndGroupIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-1", EventKey: "k1", EventType: "host.appeared", OccurredAt: now, Cycle: 1, PayloadJSON: `{}`, ReceivedAt: now, SignatureTimestamp: now}
	for i := 0; i < 2; i++ {
		inserted, err := s.PersistEventAndGroup(context.Background(), ev, "host:1", now.Add(time.Minute))
		if err != nil {
			t.Fatalf("persist: %v", err)
		}
		if i == 0 && !inserted {
			t.Fatal("first persist must insert")
		}
		if i == 1 && inserted {
			t.Fatal("duplicate event_id must not insert twice")
		}
	}
	count, err := s.GroupEventCount(context.Background(), "host:1")
	if err != nil || count != 1 {
		t.Fatalf("expected 1 group event, got %d, %v", count, err)
	}
}

func TestRecoveryClosesExpiredGroups(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-9", EventKey: "k9", EventType: "host.changed", OccurredAt: now, Cycle: 2, PayloadJSON: `{}`, ReceivedAt: now, SignatureTimestamp: now}
	if _, err := s.PersistEventAndGroup(context.Background(), ev, "host:9", now.Add(-time.Minute)); err != nil {
		t.Fatalf("persist: %v", err)
	}
	closed, err := s.Recover(context.Background(), now)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(closed) != 1 {
		t.Fatalf("expected 1 recovered group, got %v", closed)
	}
	status, err := s.GroupStatus(context.Background(), closed[0])
	if err != nil || status != "closed" {
		t.Fatalf("expected closed, got %q, %v", status, err)
	}
}

func TestConcurrentWritersSerialize(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev := RawEvent{EventID: "evt-c", EventKey: "k", EventType: "host.changed", OccurredAt: now, Cycle: i, PayloadJSON: `{}`, ReceivedAt: now, SignatureTimestamp: now}
			_, _ = s.PersistEventAndGroup(context.Background(), ev, "host:c", now.Add(time.Minute))
		}(i)
	}
	wg.Wait()
	count, err := s.GroupEventCount(context.Background(), "host:c")
	if err != nil || count != 1 {
		t.Fatalf("expected exactly 1 grouped event under concurrency, got %d, %v", count, err)
	}
}

func TestGroupsForDifferentAnchorsAtSameInstant(t *testing.T) {
	s := openTestStore(t)
	if got := s.db.Stats().MaxOpenConnections; got < 2 {
		t.Fatalf("expected read pool > 1 for WAL readers, got %d", got)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, anchor := range []string{"host:a", "host:b"} {
		ev := RawEvent{EventID: "evt-" + anchor, EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, PayloadJSON: `{}`, ReceivedAt: now, SignatureTimestamp: now}
		inserted, err := s.PersistEventAndGroup(context.Background(), ev, anchor, now.Add(time.Minute))
		if err != nil || !inserted {
			t.Fatalf("persist %s: inserted=%v err=%v", anchor, inserted, err)
		}
	}
	for _, anchor := range []string{"host:a", "host:b"} {
		count, err := s.GroupEventCount(context.Background(), anchor)
		if err != nil || count != 1 {
			t.Fatalf("anchor %s: expected 1 grouped event, got %d, %v", anchor, count, err)
		}
	}
}
