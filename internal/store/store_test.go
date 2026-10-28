package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// seedFeedGroup persists one open group and returns its ID.
func seedFeedGroup(t *testing.T, s *Store, anchor, eventID string) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	res, err := s.PersistEventAndGroup(context.Background(), RawEvent{
		EventID: eventID, EventKey: "k", EventType: "host.appeared",
		OccurredAt: now, Payload: []byte(`{"event_id":"e"}`), ReceivedAt: now, SignatureTimestamp: now,
	}, anchor, GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return res.GroupID
}

func TestViewerFeeds(t *testing.T) {
	s := openTestStore(t)
	g1 := seedFeedGroup(t, s, "host:a", "evt-a1")
	seedFeedGroup(t, s, "host:b", "evt-b1")
	snaps, err := s.OpenGroupSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("open snapshots = %d", len(snaps))
	}
	for _, g := range snaps {
		if g.Status != "open" || g.EventCount != 1 || g.Deadline.IsZero() {
			t.Fatalf("snapshot = %+v", g)
		}
	}
	// Brief one group: it leaves the open set and appears in the feed.
	// CloseDue closes both seeds, so re-seed the bystander afterwards.
	if _, err := s.CloseDue(context.Background(), time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(context.Background(), g1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(context.Background(), Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: "host:a",
		Baseline: "low", Priority: "low", PriorityReason: "r",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// Fresh anchor: a new event on the closed host:b group would rejoin
	// it instead of opening a new one.
	seedFeedGroup(t, s, "host:c", "evt-c1")
	snaps, err = s.OpenGroupSnapshots(context.Background())
	if err != nil || len(snaps) != 1 || snaps[0].Anchor != "host:c" {
		t.Fatalf("after brief: %+v %v", snaps, err)
	}
	rows, err := s.RecentBriefings(context.Background(), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("recent = %+v %v", rows, err)
	}
	if rows[0].GroupID != g1 || len(rows[0].EventIDs) != 1 || rows[0].EventIDs[0] != "evt-a1" {
		t.Fatalf("row = %+v", rows[0])
	}
	if got, _ := s.RecentBriefings(context.Background(), 0); len(got) != 1 {
		t.Fatalf("zero limit must default, got %d", len(got))
	}
}

func TestViewerFeedQueriesUseIndexes(t *testing.T) {
	s := openTestStore(t)
	for _, q := range []string{
		`EXPLAIN QUERY PLAN SELECT briefing_id FROM briefings ORDER BY created_at DESC LIMIT 10`,
		`EXPLAIN QUERY PLAN SELECT ag.group_id FROM active_groups ag LEFT JOIN group_events ge ON ge.group_id = ag.group_id WHERE ag.status = 'open' GROUP BY ag.group_id ORDER BY ag.deadline_at`,
	} {
		rows, err := s.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		used := false
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(detail, "SCAN") || strings.Contains(detail, "USING") {
				used = true
			}
		}
		rows.Close()
		if !used {
			t.Fatalf("query falls back to a full scan: %s", q)
		}
	}
}

func TestQueueCapOption(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "q.db"), WithQueueCap(0)); err == nil {
		t.Fatal("zero queue cap must be rejected")
	}
	s, err := Open(filepath.Join(t.TempDir(), "q2.db"), WithQueueCap(1))
	if err != nil {
		t.Fatalf("cap 1 must open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ev := RawEvent{EventID: "e1", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
	if _, err := s.PersistEventAndGroup(context.Background(), ev, "a", GroupPolicy{QuietWindow: time.Minute}); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("write after close must report closed, got %v", err)
	}
}

func TestMarkUndeliverableExcludesFromSweep(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()
	ev := RawEvent{EventID: "e-u", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
	if _, err := s.PersistEventAndGroup(context.Background(), ev, "a", GroupPolicy{QuietWindow: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cands, err := s.SweepCandidates(context.Background(), now, 300*time.Second, 30*time.Second, 10)
	if err != nil || len(cands) != 1 {
		t.Fatalf("seeded row must be a candidate: %v %v", len(cands), err)
	}
	if err := s.MarkUndeliverable(context.Background(), "e-u", "undeliverable_headers"); err != nil {
		t.Fatal(err)
	}
	cands, err = s.SweepCandidates(context.Background(), now, 300*time.Second, 30*time.Second, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatal("undeliverable row must never be selected again")
	}
}

func TestPersistEventAndGroupIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-1", EventKey: "k1", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
	for i := 0; i < 2; i++ {
		res, err := s.PersistEventAndGroup(context.Background(), ev, "host:1", GroupPolicy{QuietWindow: time.Minute, MaxWindow: time.Hour})
		if err != nil {
			t.Fatalf("persist: %v", err)
		}
		if i == 0 && !res.Inserted {
			t.Fatal("first persist must insert")
		}
		if i == 1 && res.Inserted {
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
	ev := RawEvent{EventID: "evt-9", EventKey: "k9", EventType: "host.changed", OccurredAt: now, Cycle: 2, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
	if _, err := s.PersistEventAndGroup(context.Background(), ev, "host:9", GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	closed, err := s.Recover(context.Background(), now.Add(time.Second))
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
			ev := RawEvent{EventID: "evt-c", EventKey: "k", EventType: "host.changed", OccurredAt: now, Cycle: i, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
			_, _ = s.PersistEventAndGroup(context.Background(), ev, "host:c", GroupPolicy{QuietWindow: time.Minute, MaxWindow: time.Hour})
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
		ev := RawEvent{EventID: "evt-" + anchor, EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
		res, err := s.PersistEventAndGroup(context.Background(), ev, anchor, GroupPolicy{QuietWindow: time.Minute, MaxWindow: time.Hour})
		if err != nil || !res.Inserted {
			t.Fatalf("persist %s: inserted=%v err=%v", anchor, res.Inserted, err)
		}
	}
	for _, anchor := range []string{"host:a", "host:b"} {
		count, err := s.GroupEventCount(context.Background(), anchor)
		if err != nil || count != 1 {
			t.Fatalf("anchor %s: expected 1 grouped event, got %d, %v", anchor, count, err)
		}
	}
}

func TestDebounceExtendsAndTakeGroup(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	pol := GroupPolicy{QuietWindow: 45 * time.Second, MaxWindow: 120 * time.Second, MaxEvents: 10}
	mk := func(id string, at time.Time) RawEvent {
		return RawEvent{EventID: id, EventKey: "k", EventType: "host.changed", OccurredAt: at, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: at, SignatureTimestamp: at}
	}
	r1, err := s.PersistEventAndGroup(context.Background(), mk("evt-d1", now), "host:d", pol)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	d1, _ := s.GroupDeadline(context.Background(), r1.GroupID)
	r2, err := s.PersistEventAndGroup(context.Background(), mk("evt-d2", now.Add(30*time.Second)), "host:d", pol)
	if err != nil || r2.GroupID != r1.GroupID {
		t.Fatalf("second event must join same group: %+v %v", r2, err)
	}
	d2, _ := s.GroupDeadline(context.Background(), r1.GroupID)
	if !d2.After(d1) || !d2.Equal(now.Add(75*time.Second)) {
		t.Fatalf("deadline must extend to last+quiet: %v -> %v", d1, d2)
	}
	if _, err := s.CloseDue(context.Background(), now.Add(200*time.Second)); err != nil {
		t.Fatalf("close due: %v", err)
	}
	taken, err := s.TakeGroup(context.Background(), r1.GroupID)
	if err != nil || len(taken.Events) != 2 || taken.Anchor != "host:d" {
		t.Fatalf("take group: %+v, %v", taken, err)
	}
	if st, _ := s.GroupStatus(context.Background(), r1.GroupID); st != "processing" {
		t.Fatalf("expected processing, got %q", st)
	}
}

func TestPseudonymNormalizationAndCounters(t *testing.T) {
	s := openTestStore(t)
	a, err := s.GetOrAssignPseudonym(context.Background(), "mac", "AA:BB:CC:DD:EE:FF")
	if err != nil || a != "MAC_1" {
		t.Fatalf("first mac token: %q %v", a, err)
	}
	b, err := s.GetOrAssignPseudonym(context.Background(), "mac", "aa-bb-cc-dd-ee-ff")
	if err != nil || b != "MAC_1" {
		t.Fatalf("normalized mac must reuse token: %q %v", b, err)
	}
	c, err := s.GetOrAssignPseudonym(context.Background(), "ip", "192.168.000.001")
	if err != nil || c != "IP_1" {
		t.Fatalf("canonical ip token: %q %v", c, err)
	}
}

func TestForwardState(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-f", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
	if _, err := s.PersistEventAndGroup(context.Background(), ev, "host:f", GroupPolicy{QuietWindow: time.Minute}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := s.RecordForwardFailure(context.Background(), "evt-f", "502"); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	matched, err := s.MarkForwarded(context.Background(), "evt-f", now)
	if err != nil || !matched {
		t.Fatalf("mark forwarded: %v %v", matched, err)
	}
	matched, _ = s.MarkForwarded(context.Background(), "evt-f", now)
	if matched {
		t.Fatal("second MarkForwarded must be a no-op")
	}
	fwd, attempts, _, err := s.ForwardState(context.Background(), "evt-f")
	if err != nil || !fwd || attempts != 2 {
		t.Fatalf("forward state: %v %d %v", fwd, attempts, err)
	}
}

func TestReopenRecoversExpiredGroupAndSidecarPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	now := time.Now().UTC().Truncate(time.Second)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ev := RawEvent{EventID: "evt-r", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
	if _, err := s.PersistEventAndGroup(context.Background(), ev, "host:r", GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	closed, err := s2.Recover(context.Background(), now.Add(time.Second))
	if err != nil || len(closed) != 1 {
		t.Fatalf("recover after reopen: %v %v", closed, err)
	}
	needs, err := s2.NeedsBriefing(context.Background(), 64)
	if err != nil || len(needs) != 1 {
		t.Fatalf("needs briefing: %v %v", needs, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			continue
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", path+suffix, info.Mode().Perm())
		}
	}
}

func TestHeadersJSONWhitelistEnforced(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-h", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), HeadersJSON: `{"Authorization":"Bearer x"}`, ReceivedAt: now, SignatureTimestamp: now}
	if _, err := s.PersistEventAndGroup(context.Background(), ev, "host:h", GroupPolicy{QuietWindow: time.Minute}); err == nil {
		t.Fatal("non-whitelisted header must be rejected")
	}
	ev.HeadersJSON = `{"X-Subnetlens-Signature-256":"sha256=abc","Content-Type":"application/json"}`
	if _, err := s.PersistEventAndGroup(context.Background(), ev, "host:h", GroupPolicy{QuietWindow: time.Minute}); err != nil {
		t.Fatalf("canonical whitelisted headers must pass: %v", err)
	}
	got := WhitelistForwardHeaders(map[string]string{"X-Subnetlens-Signature-256": "sha256=abc", "Authorization": "Bearer x"})
	if len(got) != 1 {
		t.Fatalf("whitelist must keep signature, drop credentials: %v", got)
	}
}

func TestPseudonymRestoreAndOrder(t *testing.T) {
	s := openTestStore(t)
	tokens, err := s.GetOrAssignPseudonyms(context.Background(), "hostname", "B.Example.COM", "a.example.com")
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if tokens["B.Example.COM"] != "HOST_1" || tokens["a.example.com"] != "HOST_2" {
		t.Fatalf("input order must determine numbering: %v", tokens)
	}
	display, _, found, err := s.RestorePseudonym(context.Background(), "HOST_1")
	if err != nil || !found || display != "B.Example.COM" {
		t.Fatalf("restore must return original text: %q %v %v", display, found, err)
	}
	if _, err := s.GetOrAssignPseudonym(context.Background(), "hostname", "  "); err == nil {
		t.Fatal("empty pseudonym value must be rejected")
	}
}

func TestMigrateV1DatabaseWithData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatalf("create v1 schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pseudonym_map (real_value, category, token, created_at) VALUES ('192.168.0.9', 'ip', 'IP_5', 1)`); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set version: %v", err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open with migration: %v", err)
	}
	defer s.Close()
	display, _, found, err := s.RestorePseudonym(context.Background(), "IP_5")
	if err != nil || !found || display != "192.168.0.9" {
		t.Fatalf("migrated token must restore: %q %v %v", display, found, err)
	}
	next, err := s.GetOrAssignPseudonym(context.Background(), "ip", "192.168.0.10")
	if err != nil || next != "IP_6" {
		t.Fatalf("counter must continue past migrated max: %q %v", next, err)
	}
}

func TestPersistReportsOverdueClosedGroupAndStaleRelease(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	pol := GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour}
	mk := func(id string, at time.Time) RawEvent {
		return RawEvent{EventID: id, EventKey: "k", EventType: "host.changed", OccurredAt: at, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: at, SignatureTimestamp: at}
	}
	r1, err := s.PersistEventAndGroup(context.Background(), mk("evt-o1", now), "host:o", pol)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	r2, err := s.PersistEventAndGroup(context.Background(), mk("evt-o2", now.Add(time.Second)), "host:o", pol)
	if err != nil {
		t.Fatalf("persist 2: %v", err)
	}
	if len(r2.ClosedGroupIDs) != 1 || r2.ClosedGroupIDs[0] != r1.GroupID || r2.GroupID == r1.GroupID {
		t.Fatalf("overdue group must be reported closed and a new one opened: %+v vs %+v", r2, r1)
	}
	taken, err := s.TakeGroup(context.Background(), r1.GroupID)
	if err != nil || len(taken.Events) != 1 {
		t.Fatalf("take overdue group: %+v %v", taken, err)
	}
	released, err := s.ReleaseStale(context.Background(), now.Add(2*time.Second), time.Millisecond)
	if err != nil || len(released) != 1 {
		t.Fatalf("release stale: %v %v", released, err)
	}
	if st, _ := s.GroupStatus(context.Background(), r1.GroupID); st != "closed" {
		t.Fatalf("expected closed after stale release, got %q", st)
	}
}

func TestTakeGroupSentinel(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.TakeGroup(context.Background(), "grp-missing"); !errors.Is(err, ErrGroupNotClosed) {
		t.Fatalf("expected ErrGroupNotClosed, got %v", err)
	}
}

func TestMigrateV2BackfillsEventIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatalf("v1 schema: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := migrateV2(tx); err != nil {
		t.Fatalf("apply v2: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit v2: %v", err)
	}
	now := time.Now().UTC().UnixNano()
	stmts := []string{
		`INSERT INTO active_groups (group_id, host_anchor_key, opened_at, last_event_at, deadline_at, event_count, status) VALUES ('g1','host:x',0,0,0,1,'done')`,
		`INSERT INTO raw_events (event_id, event_key, event_type, occurred_at, cycle, payload, received_at, signature_timestamp) VALUES ('evt-m','k','host.appeared',0,1,x'7b7d',0,0)`,
		`INSERT INTO group_events (group_id, event_id) VALUES ('g1','evt-m')`,
		`INSERT INTO briefings (briefing_id, group_id, host_anchor_key, baseline, priority, priority_reason, what_changed, context, recommended_check, created_at) VALUES ('b1','g1','host:x','low','low','r','w','c','check',0)`,
		`PRAGMA user_version = 2`,
	}
	_ = now
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed v2 (%s): %v", stmt[:40], err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open with v3 migration: %v", err)
	}
	defer s.Close()
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	defer raw.Close()
	var eventIDs string
	if err := raw.QueryRow(`SELECT event_ids_json FROM briefings WHERE briefing_id = 'b1'`).Scan(&eventIDs); err != nil {
		t.Fatalf("read backfilled ids: %v", err)
	}
	if eventIDs != `["evt-m"]` {
		t.Fatalf("expected backfilled event ids, got %s", eventIDs)
	}
}

func TestTakeIncrementsOnce(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-tc", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
	res, err := s.PersistEventAndGroup(context.Background(), ev, "host:tc", GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(context.Background(), res.GroupID)
	if err != nil || taken.RetryCount != 1 {
		t.Fatalf("first take must count 1, got %d %v", taken.RetryCount, err)
	}
	if _, err := s.ReleaseGroup(context.Background(), res.GroupID); err != nil {
		t.Fatal(err)
	}
	taken, err = s.TakeGroup(context.Background(), res.GroupID)
	if err != nil || taken.RetryCount != 2 {
		t.Fatalf("take->release->take must count 2, got %d %v", taken.RetryCount, err)
	}
}

func TestReleaseParksAtCapWithoutIncrement(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-cap", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
	res, err := s.PersistEventAndGroup(ctx, ev, "host:cap", GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// Drive to cap-1 = 4 through takes; releases must not move the counter.
	for i := 0; i < 4; i++ {
		taken, err := s.TakeGroup(ctx, res.GroupID)
		if err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
		if taken.RetryCount != i+1 {
			t.Fatalf("take %d counted %d", i, taken.RetryCount)
		}
		if taken.LastAttempt() != (i+1 >= 4) {
			t.Fatalf("take %d LastAttempt = %v, want %v", i, taken.LastAttempt(), i+1 >= 4)
		}
		if _, err := s.ReleaseGroup(ctx, res.GroupID); err != nil {
			t.Fatal(err)
		}
		if st, _ := s.GroupStatus(ctx, res.GroupID); st != "closed" {
			t.Fatalf("count %d must release to closed, got %q", i+1, st)
		}
	}
	// Fifth take hits the cap; the release parks failed.
	if _, err := s.TakeGroup(ctx, res.GroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseGroup(ctx, res.GroupID); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.GroupStatus(ctx, res.GroupID); st != "failed" {
		t.Fatalf("count at cap must park failed, got %q", st)
	}
}

func TestTickQueriesUseIndexes(t *testing.T) {
	s := openTestStore(t)
	check := func(name, q string) {
		t.Helper()
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN ` + q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer rows.Close()
		foundSearch := false
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			if len(detail) >= 4 && detail[:4] == "SCAN" {
				t.Fatalf("%s full-scans: %s", name, detail)
			}
			if len(detail) >= 6 && detail[:6] == "SEARCH" {
				foundSearch = true
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !foundSearch {
			t.Fatalf("%s has no index-backed search step", name)
		}
	}
	check("close-due", `SELECT group_id FROM active_groups WHERE status = 'open' AND deadline_at <= 1`)
	check("release-stale", `SELECT group_id FROM active_groups WHERE status = 'processing' AND processing_since IS NOT NULL AND processing_since <= 1`)
	check("take-events", `SELECT e.event_id FROM group_events ge JOIN raw_events e ON e.event_id = ge.event_id WHERE ge.group_id = 'x'`)
	check("needs-briefing", `SELECT group_id FROM active_groups WHERE status = 'closed' AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id)`)
}

func TestIdleTickWith100kHistoricalGroups(t *testing.T) {
	if testing.Short() {
		t.Skip("100k fixture needs a full run")
	}
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// 100k done groups: history the tick must scan past, not touch.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO active_groups (group_id, host_anchor_key, opened_at, last_event_at, deadline_at, event_count, status) VALUES (?, ?, ?, ?, ?, 1, 'done')`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100000; i++ {
		n := now.Add(-time.Duration(i) * time.Second).UnixNano()
		if _, err := stmt.Exec(fmt.Sprintf("grp-hist-%d", i), "host:hist", n, n, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	const runs = 20
	var durs []time.Duration
	for i := 0; i < runs; i++ {
		start := time.Now()
		if _, err := s.CloseDue(ctx, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReleaseStale(ctx, now, time.Minute); err != nil {
			t.Fatal(err)
		}
		durs = append(durs, time.Since(start))
	}
	sort.Slice(durs, func(a, b int) bool { return durs[a] < durs[b] })
	p99 := durs[int(float64(len(durs))*0.99)]
	t.Logf("idle tick p99 with 100k historical groups: %v (budget 5ms)", p99)
	if p99 > 5*time.Millisecond {
		t.Fatalf("idle tick p99 %v exceeds 5ms", p99)
	}
}

func TestMigrateV3ToV4WithData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-v3", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
	res, err := s.PersistEventAndGroup(context.Background(), ev, "host:v3", GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeGroup(context.Background(), res.GroupID); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(context.Background(), Briefing{
		BriefingID: "brf-v3", GroupID: res.GroupID, HostAnchorKey: "host:v3",
		Baseline: "low", Priority: "low", PriorityReason: "r",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Rewind to v3: drop the v4 column and version, keeping the data.
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE briefings DROP COLUMN baseline_rule`); err != nil {
		t.Fatalf("drop v4 column: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("migrate 3->4: %v", err)
	}
	defer s2.Close()
	got, err := s2.BriefingForGroup(context.Background(), res.GroupID)
	if err != nil {
		t.Fatalf("v3 briefing must stay readable: %v", err)
	}
	if got.BriefingID != "brf-v3" || got.BaselineRule != "" {
		t.Fatalf("unexpected migrated briefing: %+v", got)
	}
}

func TestPurgeFailedGroupEvents(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-p", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
	res, err := s.PersistEventAndGroup(context.Background(), ev, "host:p", GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour})
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := s.CloseDue(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatalf("close due: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.TakeGroup(context.Background(), res.GroupID); err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
		if _, err := s.ReleaseGroup(context.Background(), res.GroupID); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
	}
	if st, _ := s.GroupStatus(context.Background(), res.GroupID); st != "failed" {
		t.Fatalf("expected failed after 5 retries, got %q", st)
	}
	if _, err := s.PurgeRawEvents(context.Background(), now.Add(2*time.Hour), now.Add(time.Second), time.Hour); err != nil {
		t.Fatalf("early purge: %v", err)
	}
	count, err := s.GroupEventCount(context.Background(), "host:p")
	if err != nil || count != 1 {
		t.Fatalf("failed events must survive the normal floor (week floor): %d %v", count, err)
	}
	if _, err := s.PurgeRawEvents(context.Background(), now.Add(8*24*time.Hour), now.Add(time.Second), time.Hour); err != nil {
		t.Fatalf("late purge: %v", err)
	}
	count, err = s.GroupEventCount(context.Background(), "host:p")
	if err != nil || count != 0 {
		t.Fatalf("failed group events must purge after a week: %d %v", count, err)
	}
	if _, err := s.GroupStatus(context.Background(), res.GroupID); err == nil {
		t.Fatal("failed group row must be deleted with its events")
	}
}
