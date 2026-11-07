package store

import (
	"bytes"
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

// seedFeedGroup persists one open group and returns its ID. The event is
// projected (persist -> project) so the group is fully projected and
// briefable, mirroring production order.
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
	if _, err := s.ProjectReported(context.Background(), "test-scope", 64); err != nil {
		t.Fatal(err)
	}
	return res.GroupID
}

// projectAll runs the projector over every pending event, mirroring the
// worker tick order (persist -> project -> close -> take) so groups under
// test are fully projected and briefable.
func projectAll(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.ProjectReported(context.Background(), "test-scope", 1<<20); err != nil {
		t.Fatal(err)
	}
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
	// The briefed group is gone; the unbriefed closed host:b group stays
	// visible as queued alongside the fresh open host:c group.
	if err != nil || len(snaps) != 2 {
		t.Fatalf("after brief: %+v %v", snaps, err)
	}
	byAnchor := map[string]GroupSnapshot{}
	for _, g := range snaps {
		byAnchor[g.Anchor] = g
	}
	if byAnchor["host:b"].Status != "closed" {
		t.Fatalf("queued row = %+v", byAnchor["host:b"])
	}
	if byAnchor["host:c"].Status != "open" {
		t.Fatalf("open row = %+v", byAnchor["host:c"])
	}
	rows, err := s.BriefingsPage(context.Background(), 0, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("recent = %+v %v", rows, err)
	}
	if rows[0].GroupID != g1 || len(rows[0].EventIDs) != 1 || rows[0].EventIDs[0] != "evt-a1" {
		t.Fatalf("row = %+v", rows[0])
	}
	if rows[0].Seq != 1 {
		t.Fatalf("first briefing seq = %d, want 1", rows[0].Seq)
	}
	if got, _ := s.BriefingsPage(context.Background(), 0, 0, 0); len(got) != 1 {
		t.Fatalf("zero limit must default, got %d", len(got))
	}
}

// A group being briefed stays visible until its briefing lands: the
// snapshot carries open, closed and processing rows with the times the
// wire needs for countdowns. A group queued behind a taken one stays
// closed in the snapshot — waiting, not gone.
func TestSnapshotKeepsProcessingGroups(t *testing.T) {
	s := openTestStore(t)
	gid := seedFeedGroup(t, s, "host:proc", "evt-proc")
	waiter := seedFeedGroup(t, s, "host:wait", "evt-wait")
	if _, err := s.CloseDue(context.Background(), time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeGroup(context.Background(), gid); err != nil {
		t.Fatal(err)
	}
	snaps, err := s.OpenGroupSnapshots(context.Background())
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshot = %+v %v", snaps, err)
	}
	byID := map[string]GroupSnapshot{}
	for _, g := range snaps {
		byID[g.GroupID] = g
	}
	if byID[gid].Status != "processing" {
		t.Fatalf("taken status = %q, want processing", byID[gid].Status)
	}
	if byID[waiter].Status != "closed" {
		t.Fatalf("queued status = %q, want closed", byID[waiter].Status)
	}
	g := byID[gid]
	if g.OpenedAt.IsZero() || g.LastEvent.IsZero() || g.Deadline.IsZero() {
		t.Fatalf("snapshot times missing: %+v", g)
	}
	if g.EventCount != 1 {
		t.Fatalf("event count = %d", g.EventCount)
	}
}

func TestViewerFeedQueriesUseIndexes(t *testing.T) {
	s := openTestStore(t)
	for _, q := range []string{
		`EXPLAIN QUERY PLAN SELECT briefing_id FROM briefings ORDER BY created_at DESC LIMIT 10`,
		`EXPLAIN QUERY PLAN SELECT briefing_id FROM briefings WHERE seq > 5 ORDER BY seq ASC LIMIT 10`,
		`EXPLAIN QUERY PLAN SELECT briefing_id, group_id FROM briefings WHERE seq > 5 ORDER BY seq ASC LIMIT 11`,
		`EXPLAIN QUERY PLAN SELECT briefing_id FROM briefings WHERE seq < 5 ORDER BY seq DESC LIMIT 10`,
		`EXPLAIN QUERY PLAN SELECT ag.group_id FROM active_groups ag LEFT JOIN group_events ge ON ge.group_id = ag.group_id WHERE ag.status IN ('open','processing') GROUP BY ag.group_id ORDER BY ag.deadline_at`,
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
	if _, err := s.ProjectReported(context.Background(), "test-scope", 64); err != nil {
		t.Fatalf("project: %v", err)
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

// Batch assignment crosses the IN-chunk boundary deterministically: one call
// with more than a chunk of values numbers tokens in input order, folds a
// case-variant duplicate onto one token, and re-reads stably.
func TestPseudonymsBatchAcrossChunks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const n = 2*pseudonymLookupBatch + 7
	vals := make([]string, 0, n+1)
	for i := 0; i < n; i++ {
		vals = append(vals, fmt.Sprintf("AA:BB:CC:%02X:%02X:%02X", i>>16, (i>>8)&0xFF, i&0xFF))
	}
	vals = append(vals, strings.ToLower(vals[3])) // duplicate, different case
	got, err := s.GetOrAssignPseudonyms(ctx, "mac", vals...)
	if err != nil {
		t.Fatalf("batch assign: %v", err)
	}
	if len(got) != len(vals) {
		t.Fatalf("batch result covers every input: got %d want %d", len(got), len(vals))
	}
	for k := 0; k < n; k++ {
		if want := fmt.Sprintf("MAC_%d", k+1); got[vals[k]] != want {
			t.Fatalf("token order: vals[%d] = %q want %q", k, got[vals[k]], want)
		}
	}
	if got[vals[n]] != got[vals[3]] {
		t.Fatalf("case-variant duplicate must share a token: %q vs %q", got[vals[n]], got[vals[3]])
	}
	again, err := s.GetOrAssignPseudonyms(ctx, "mac", vals...)
	if err != nil {
		t.Fatalf("batch re-read: %v", err)
	}
	for _, v := range vals {
		if again[v] != got[v] {
			t.Fatalf("re-read unstable for %q: %q vs %q", v, again[v], got[v])
		}
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
	projectAll(t, s2)
	needs, err := s2.NeedsBriefing(context.Background(), 64, time.Now())
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
	projectAll(t, s)
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
	projectAll(t, s)
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
	projectAll(t, s)
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
	check("needs-briefing", `SELECT group_id FROM active_groups WHERE status = 'closed' AND (next_attempt_at IS NULL OR next_attempt_at <= 1) AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id)`)
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
	projectAll(t, s)
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

	// Rewind to v3: drop the v4, v5, and v6 columns and the v5 index,
	// keeping the data. The reopen replays 3->4->5->6->7->8->9. The rewind must
	// cover every later column or the forward migration re-adds an
	// existing one.
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE briefings DROP COLUMN baseline_rule`); err != nil {
		t.Fatalf("drop v4 column: %v", err)
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS groups_closed_next_attempt`); err != nil {
		t.Fatalf("drop v5 index: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE active_groups DROP COLUMN next_attempt_at`); err != nil {
		t.Fatalf("drop v5 column: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE active_groups DROP COLUMN total_bytes`); err != nil {
		t.Fatalf("drop v6 column: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE active_groups DROP COLUMN transient_refunds`); err != nil {
		t.Fatalf("drop v7 column: %v", err)
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS briefings_seq`); err != nil {
		t.Fatalf("drop v8 index: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE briefings DROP COLUMN seq`); err != nil {
		t.Fatalf("drop v8 column: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS seq_state`); err != nil {
		t.Fatalf("drop v9 state: %v", err)
	}
	dropProjectionSchema(t, db)
	dropDecisionSchema(t, db)
	dropTraceSchema(t, db)
	dropRuleSchema(t, db)
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("migrate 3->5: %v", err)
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
	projectAll(t, s)
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

// H1: shutdown answers every queued job. A slow write occupies the
// writer while Background-context callers queue behind it; Close must
// release them all with ErrStoreClosed instead of hanging them forever.
func TestCloseRejectsQueuedJobs(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "drain.db"), WithQueueCap(16))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	slowDone := make(chan error, 1)
	go func() {
		slowDone <- s.write(context.Background(), func(tx *sql.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("writer never started the first job")
	}

	// Admit jobs explicitly so they are all pending when shutdown starts.
	pending := make([]chan error, 8)
	for i := range pending {
		pending[i] = make(chan error, 1)
		if err := s.submit(writeJob{ctx: ctx, done: pending[i], fn: func(*sql.Tx) error {
			return fmt.Errorf("queued write ran during shutdown")
		}}); err != nil {
			t.Fatal(err)
		}
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-s.quit:
	case <-ctx.Done():
		t.Fatal("close never stopped admission")
	}
	if err := s.write(ctx, func(*sql.Tx) error { return nil }); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("new write during shutdown: %v", err)
	}
	unblock()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("close did not join the writer")
	}
	if err := <-slowDone; err != nil {
		t.Fatalf("in-flight write: %v", err)
	}
	if err := s.write(ctx, func(*sql.Tx) error { return nil }); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("new write after shutdown: %v", err)
	}
	for _, done := range pending {
		select {
		case err := <-done:
			if !errors.Is(err, ErrStoreClosed) {
				t.Fatalf("queued write: %v", err)
			}
		default:
			t.Fatal("writer exited without rejecting a queued job")
		}
	}
	if depth := s.QueueDepth(); depth != 0 {
		t.Fatalf("closed store retained %d queued jobs", depth)
	}
}

func TestSubmitRacesClose(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	started := make(chan struct{})
	signalStarted := sync.OnceFunc(func() { close(started) })
	write := func(*sql.Tx) error { return nil }
	var writers sync.WaitGroup
	for range 32 {
		writers.Go(func() {
			for range 100 {
				err := s.write(ctx, write)
				signalStarted()
				switch {
				case errors.Is(err, ErrStoreClosed):
					return
				case err == nil, errors.Is(err, ErrQueueSaturated):
				default:
					t.Errorf("write: %v", err)
					return
				}
			}
		})
	}
	<-started
	if err := s.Close(); err != nil {
		t.Error(err)
	}
	writers.Wait()
	if depth := s.QueueDepth(); depth != 0 {
		t.Fatalf("closed store retained %d queued jobs", depth)
	}
}

// H2: releases park a bounded backoff the briefing queue honors, takes
// refuse not-due groups, and transient refunds do not burn the budget.
func TestReleaseBackoffRefundAndDueGuard(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	pol := GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour}
	mk := func(id, anchor string) string {
		ev := RawEvent{EventID: id, EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
		res, err := s.PersistEventAndGroup(ctx, ev, anchor, pol)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CloseDue(ctx, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		return res.GroupID
	}
	gid := mk("evt-b1", "host:b")

	projectAll(t, s)
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil || taken.RetryCount != 1 {
		t.Fatalf("first take: %+v %v", taken, err)
	}
	// Transient refund: the take is forgiven, the group requeues closed.
	matched, err := s.ReleaseGroupTransient(ctx, gid, 0)
	if err != nil || !matched {
		t.Fatalf("transient release: %v %v", matched, err)
	}
	taken, err = s.TakeGroup(ctx, gid)
	if err != nil || taken.RetryCount != 1 {
		t.Fatalf("refunded take must not burn budget: %+v %v", taken, err)
	}
	// Backed-off groups leave the queue and refuse takes until due.
	if _, err := s.ReleaseGroupDelayed(ctx, gid, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if needs, err := s.NeedsBriefing(ctx, 64, now); err != nil || len(needs) != 0 {
		t.Fatalf("backed-off group must leave the queue: %v %v", needs, err)
	}
	// Poll past the backoff: the group becomes due and takes again.
	// (Takes use the store clock, so elapse is real time, short by design.)
	took := false
	for i := 0; i < 100 && !took; i++ {
		if _, err := s.TakeGroup(ctx, gid); err == nil {
			took = true
		} else if !errors.Is(err, ErrGroupNotClosed) {
			t.Fatal(err)
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !took {
		t.Fatal("take after backoff elapse must succeed")
	}
	// A long backoff refuses takes and hides the group until due.
	if _, err := s.ReleaseGroupDelayed(ctx, gid, time.Hour); err != nil {
		t.Fatal(err)
	}
	if needs, err := s.NeedsBriefing(ctx, 64, now); err != nil || len(needs) != 0 {
		t.Fatalf("backed-off group must leave the queue: %v %v", needs, err)
	}
	if needs, err := s.NeedsBriefing(ctx, 64, now.Add(2*time.Hour)); err != nil || len(needs) != 1 {
		t.Fatalf("backoff must elapse: %v %v", needs, err)
	}
	if _, err := s.TakeGroup(ctx, gid); !errors.Is(err, ErrGroupNotClosed) {
		t.Fatalf("not-due take must refuse: %v", err)
	}
}

// Same-instant events take in event_id order, so the baseline subject
// (the last event) is deterministic on received_at ties instead of
// depending on SQLite's rowid order.
func TestTakeGroupOrdersTiesByEventID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	var gid string
	for _, id := range []string{"evt-b", "evt-a"} {
		ev := RawEvent{EventID: id, EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
		res, err := s.PersistEventAndGroup(ctx, ev, "host:tie", GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		gid = res.GroupID
	}
	if _, err := s.CloseDue(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	projectAll(t, s)
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken.Events) != 2 || taken.Events[0].EventID != "evt-a" || taken.Events[1].EventID != "evt-b" {
		t.Fatalf("ties must take in event_id order: %+v", taken.Events)
	}
}

// Concurrent closes serialize: every caller waits out the one shutdown and
// receives the same result, instead of a second caller returning early.
func TestCloseConcurrentWaits(t *testing.T) {
	s := openTestStore(t)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Close()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("sequential second close: %v", err)
	}
}

// A transient failure on the fifth take (retry_count at the cap) still
// refunds: the take is unhappened and the group requeues closed instead
// of parking failed with no briefing. Only real outcomes park at the cap.
func TestTransientRefundAtCapRequeues(t *testing.T) {
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
	projectAll(t, s)
	gid := res.GroupID
	var taken TakenGroup
	for want := 1; want <= maxGroupRetries; want++ {
		taken, err = s.TakeGroup(ctx, gid)
		if err != nil || taken.RetryCount != want {
			t.Fatalf("take %d: %+v %v", want, taken, err)
		}
		if want < maxGroupRetries {
			if _, err := s.ReleaseGroup(ctx, gid); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Fifth take fails transiently (saturated queue, shutdown, dead
	// context): refund, don't park.
	if matched, err := s.ReleaseGroupTransient(ctx, gid, 0); err != nil || !matched {
		t.Fatalf("cap refund: %v %v", matched, err)
	}
	if st, _ := s.GroupStatus(ctx, gid); st != "closed" {
		t.Fatalf("refunded cap take must requeue closed, got %q", st)
	}
	taken, err = s.TakeGroup(ctx, gid)
	if err != nil || taken.RetryCount != maxGroupRetries {
		t.Fatalf("refunded take must not burn budget: %+v %v", taken, err)
	}
	// A real failure on the same take still parks failed.
	if _, err := s.ReleaseGroup(ctx, gid); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.GroupStatus(ctx, gid); st != "failed" {
		t.Fatalf("real failure at cap must park failed, got %q", st)
	}
}

// Persistent saturation must not retry forever: past maxTransientRefunds
// the next refund parks failed instead of requeueing, which stops the
// provider spend. Each cycle is one take plus one refund.
func TestTransientRefundRunawayParks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-run", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
	res, err := s.PersistEventAndGroup(ctx, ev, "host:run", GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	projectAll(t, s)
	gid := res.GroupID
	for i := 1; i <= maxTransientRefunds; i++ {
		if _, err := s.TakeGroup(ctx, gid); err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
		matched, err := s.ReleaseGroupTransient(ctx, gid, 0)
		if err != nil || !matched {
			t.Fatalf("refund %d: %v %v", i, matched, err)
		}
		if st, _ := s.GroupStatus(ctx, gid); st != "closed" {
			t.Fatalf("refund %d must requeue closed, got %q", i, st)
		}
	}
	if _, err := s.TakeGroup(ctx, gid); err != nil {
		t.Fatal(err)
	}
	matched, err := s.ReleaseGroupTransient(ctx, gid, 0)
	if err != nil || !matched {
		t.Fatalf("cap refund: %v %v", matched, err)
	}
	if st, _ := s.GroupStatus(ctx, gid); st != "failed" {
		t.Fatalf("refund past the cap must park failed, got %q", st)
	}
}

// H2/migration v5: a v4 database gains next_attempt_at on open, with
// NULL (due immediately) for pre-existing rows.
func TestMigrationV4ToV5KeepsBackoffNull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-v4", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{}`), ReceivedAt: now, SignatureTimestamp: now}
	res, err := s.PersistEventAndGroup(context.Background(), ev, "host:v4", GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Rewind to v4: drop the v5 and v6 columns and version, keeping the
	// data. The rewind must cover every later column or the forward
	// migration re-adds an existing one.
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS groups_closed_next_attempt`); err != nil {
		t.Fatalf("drop v5 index: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE active_groups DROP COLUMN next_attempt_at`); err != nil {
		t.Fatalf("drop v5 column: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE active_groups DROP COLUMN total_bytes`); err != nil {
		t.Fatalf("drop v6 column: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE active_groups DROP COLUMN transient_refunds`); err != nil {
		t.Fatalf("drop v7 column: %v", err)
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS briefings_seq`); err != nil {
		t.Fatalf("drop v8 index: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE briefings DROP COLUMN seq`); err != nil {
		t.Fatalf("drop v8 column: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS seq_state`); err != nil {
		t.Fatalf("drop v9 state: %v", err)
	}
	dropProjectionSchema(t, db)
	dropDecisionSchema(t, db)
	dropTraceSchema(t, db)
	dropRuleSchema(t, db)
	if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("migrate 4->5: %v", err)
	}
	defer s2.Close()
	var version int
	if err := s2.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != supportedSchemaVersion {
		t.Fatalf("want user_version %d, got %d %v", supportedSchemaVersion, version, err)
	}
	// The pre-v5 group has no backoff: due immediately.
	projectAll(t, s2)
	needs, err := s2.NeedsBriefing(context.Background(), 64, now.Add(time.Second))
	if err != nil || len(needs) != 1 || needs[0] != res.GroupID {
		t.Fatalf("migrated group must be due: %v %v", needs, err)
	}
}

// H10: a pre-existing 0644 database (and its sidecars, which hold raw
// payloads) is tightened to 0600 before SQLite opens it; new sidecars
// created under WAL inherit the tightened mode.
func TestSidecarsTightenedTo0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "perm.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if mode := statMode(t, path); mode != 0o600 {
		t.Fatalf("database file must be tightened to 0600, got %o", mode)
	}
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-perm", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{"host":{"ip":"10.9.9.9"}}`), ReceivedAt: now, SignatureTimestamp: now}
	if _, err := s.PersistEventAndGroup(context.Background(), ev, "host:perm", GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour}); err != nil {
		t.Fatal(err)
	}
	// Stat before Close: the WAL frames still live in the sidecars.
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(sidecar); os.IsNotExist(err) {
			continue
		} else if err != nil {
			t.Fatal(err)
		}
		if mode := statMode(t, sidecar); mode != 0o600 {
			t.Fatalf("sidecar %s must be 0600, got %o", sidecar, mode)
		}
	}
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// v6: the crossing event founds the next group (strict byte bound), and a
// replayed event_id moves neither counter.
func TestByteCapRolloverAndReplayIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	pol := GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour, MaxEvents: 1000}
	mk := func(id string, size int) PersistResult {
		ev := RawEvent{EventID: id, EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: bytes.Repeat([]byte("x"), size), ReceivedAt: now, SignatureTimestamp: now}
		res, err := s.PersistEventAndGroup(ctx, ev, "host:roll", pol)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Inserted {
			t.Fatalf("event %s must insert", id)
		}
		return res
	}
	// 6 MiB + 3 MiB crosses the 8 MiB cap: the second event founds a group.
	r1 := mk("evt-r1", 6<<20)
	r2 := mk("evt-r2", 3<<20)
	if r2.GroupID == r1.GroupID {
		t.Fatal("crossing event must found a new group")
	}
	// Closed reports the count cap on the new group (still open here);
	// the byte-cap rollover is reported through ClosedGroupIDs.
	if r2.Closed {
		t.Fatal("new group must stay open after a byte-cap rollover")
	}
	if len(r2.ClosedGroupIDs) != 1 || r2.ClosedGroupIDs[0] != r1.GroupID {
		t.Fatalf("rollover must close the old group: %+v", r2)
	}
	var b1, b2 int64
	if err := s.db.QueryRow(`SELECT total_bytes FROM active_groups WHERE group_id = ?`, r1.GroupID).Scan(&b1); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT total_bytes FROM active_groups WHERE group_id = ?`, r2.GroupID).Scan(&b2); err != nil {
		t.Fatal(err)
	}
	if b1 != 6<<20 || b2 != 3<<20 {
		t.Fatalf("counters must track founders exactly: %d %d", b1, b2)
	}
	// Replay of an existing event_id inserts nothing and moves nothing.
	ev := RawEvent{EventID: "evt-r1", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: bytes.Repeat([]byte("x"), 6<<20), ReceivedAt: now, SignatureTimestamp: now}
	res, err := s.PersistEventAndGroup(ctx, ev, "host:roll", pol)
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted {
		t.Fatal("replay must be idempotent")
	}
	if err := s.db.QueryRow(`SELECT total_bytes FROM active_groups WHERE group_id = ?`, r1.GroupID).Scan(&b1); err != nil || b1 != 6<<20 {
		t.Fatalf("replay must not move the counter: %d %v", b1, err)
	}
}

// v6 with data: an open group keeps its summed payload bytes through the
// migration instead of restarting at zero.
func TestMigrationV5ToV6KeepsByteCounter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	ev := RawEvent{EventID: "evt-v5", EventKey: "k", EventType: "host.appeared", OccurredAt: now, Cycle: 1, Payload: []byte(`{"host":"v5"}`), ReceivedAt: now, SignatureTimestamp: now}
	if _, err := s.PersistEventAndGroup(context.Background(), ev, "host:v5", GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Rewind to v5: drop the v6 and v7 columns, the v8 column and index,
	// and the v9 state, keeping the data.
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE active_groups DROP COLUMN total_bytes`); err != nil {
		t.Fatalf("drop v6 column: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE active_groups DROP COLUMN transient_refunds`); err != nil {
		t.Fatalf("drop v7 column: %v", err)
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS briefings_seq`); err != nil {
		t.Fatalf("drop v8 index: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE briefings DROP COLUMN seq`); err != nil {
		t.Fatalf("drop v8 column: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS seq_state`); err != nil {
		t.Fatalf("drop v9 state: %v", err)
	}
	dropProjectionSchema(t, db)
	dropDecisionSchema(t, db)
	dropTraceSchema(t, db)
	dropRuleSchema(t, db)
	if _, err := db.Exec(`PRAGMA user_version = 5`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("migrate 5->6: %v", err)
	}
	defer s2.Close()
	var version int
	if err := s2.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != supportedSchemaVersion {
		t.Fatalf("want user_version %d, got %d %v", supportedSchemaVersion, version, err)
	}
	var total int64
	if err := s2.db.QueryRow(`SELECT total_bytes FROM active_groups WHERE host_anchor_key = 'host:v5'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != int64(len(`{"host":"v5"}`)) {
		t.Fatalf("backfill must sum stored payloads, got %d", total)
	}
}

// waitNextSecond blocks until the wall clock crosses a second boundary
// (at most ~1s). Group ids embed the received second, so a test that
// reopens the store and founds another group in the same second would
// reuse an id: the in-memory counter restarts at zero on every Open.
func waitNextSecond() {
	cur := time.Now().UTC().Truncate(time.Second)
	for time.Now().UTC().Before(cur.Add(time.Second)) {
		time.Sleep(5 * time.Millisecond)
	}
}

// briefGroup seeds one event group and stores its briefing with an
// explicit creation time, returning the group ID.
func briefGroup(t *testing.T, s *Store, anchor, eventID string, created time.Time) string {
	t.Helper()
	gid := seedFeedGroup(t, s, anchor, eventID)
	if _, err := s.CloseDue(context.Background(), time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(context.Background(), Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: anchor,
		Baseline: "low", Priority: "low", PriorityReason: "r",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	return gid
}

// Migration v8: the writer assigns seq 1, 2, 3 in insert order, and
// MaxSeq tracks the maximum.
func TestBriefingSeqAssignedInOrder(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()
	for i, a := range []string{"host:s1", "host:s2", "host:s3"} {
		briefGroup(t, s, a, fmt.Sprintf("evt-s%d", i+1), now.Add(time.Duration(i)*time.Minute))
	}
	page, err := s.BriefingsPage(context.Background(), 0, 0, 10)
	if err != nil || len(page) != 3 {
		t.Fatalf("page = %+v %v", page, err)
	}
	for i, b := range page {
		if b.Seq != int64(i+1) {
			t.Fatalf("page[%d].Seq = %d, want %d", i, b.Seq, i+1)
		}
	}
	if max, err := s.MaxSeq(context.Background()); err != nil || max != 3 {
		t.Fatalf("max seq = %d, err %v", max, err)
	}
	got, err := s.BriefingForGroup(context.Background(), page[1].GroupID)
	if err != nil || got.Seq != 2 {
		t.Fatalf("single briefing seq = %+v %v", got, err)
	}
}

// BriefingsPage: after selects newer rows ascending, before pages older
// history ascending, the default is the latest page, and negative
// cursors are errors.
func TestBriefingsPageCursors(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		briefGroup(t, s, fmt.Sprintf("host:p%d", i), fmt.Sprintf("evt-p%d", i), now.Add(time.Duration(i)*time.Minute))
	}
	// Gap-fill newer than seq 2.
	rows, err := s.BriefingsPage(context.Background(), 2, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Seq != 3 || rows[2].Seq != 5 {
		t.Fatalf("after=2 = %+v", rows)
	}
	// Older history before seq 5, capped at 2: the two latest below 5.
	rows, err = s.BriefingsPage(context.Background(), 0, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Seq != 3 || rows[1].Seq != 4 {
		t.Fatalf("before=5 limit 2 = %+v", rows)
	}
	// Bounded window: 2 < seq < 5.
	rows, err = s.BriefingsPage(context.Background(), 2, 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Seq != 3 || rows[1].Seq != 4 {
		t.Fatalf("after=2 before=5 = %+v", rows)
	}
	// Default latest page.
	rows, err = s.BriefingsPage(context.Background(), 0, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Seq != 4 || rows[1].Seq != 5 {
		t.Fatalf("latest page = %+v", rows)
	}
	for _, tc := range [][2]int64{{-1, 0}, {0, -1}} {
		if _, err := s.BriefingsPage(context.Background(), tc[0], tc[1], 10); err == nil {
			t.Fatalf("negative cursor %+v must fail", tc)
		}
	}
}

// Migration v8 rewind: dropping back to v7 (index first, then the
// column) replays the backfill ordered by (created_at, rowid), not by
// insert order. The second-inserted briefing has the earlier timestamp
// and must take seq 1.
func TestMigrateV8BackfillsByCreatedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v8.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	briefGroup(t, s, "host:late", "evt-late", now)
	briefGroup(t, s, "host:early", "evt-early", now.Add(-time.Hour))
	s.Close()

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP INDEX briefings_seq`,
		`ALTER TABLE briefings DROP COLUMN seq`,
		`DROP TABLE seq_state`,
		`PRAGMA user_version = 7`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	dropProjectionSchema(t, db)
	dropDecisionSchema(t, db)
	dropTraceSchema(t, db)
	dropRuleSchema(t, db)
	db.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("migrate 7->8: %v", err)
	}
	defer s2.Close()
	var version int
	if err := s2.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != supportedSchemaVersion {
		t.Fatalf("want user_version %d, got %d %v", supportedSchemaVersion, version, err)
	}
	rows, err := s2.BriefingsPage(context.Background(), 0, 0, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("page = %+v %v", rows, err)
	}
	// Ascending: earlier timestamp first regardless of insert order.
	if rows[0].Seq != 1 || rows[1].Seq != 2 {
		t.Fatalf("seqs = %d, %d", rows[0].Seq, rows[1].Seq)
	}
	if rows[0].HostAnchorKey != "host:early" || rows[1].HostAnchorKey != "host:late" {
		t.Fatalf("backfill order wrong: %+v", rows)
	}
	if !rows[0].CreatedAt.Before(rows[1].CreatedAt) {
		t.Fatalf("backfill ignored created_at: %+v", rows)
	}
}

// Purging the maximum (or every) briefing must not recycle ids: the
// high-water mark lives in seq_state, outside the purged rows. A client
// holding an old cursor would otherwise skip the new rows on reconnect.
func TestSeqMonotonicAcrossPurge(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()
	briefGroup(t, s, "host:doomed1", "evt-doomed1", now.Add(-72*time.Hour))
	briefGroup(t, s, "host:doomed2", "evt-doomed2", now.Add(-71*time.Hour))
	if n, err := s.PurgeBriefings(context.Background(), now, now.Add(-48*time.Hour)); err != nil || n != 2 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	gid := briefGroup(t, s, "host:after", "evt-after", now)
	got, err := s.BriefingForGroup(context.Background(), gid)
	if err != nil || got.Seq != 3 {
		t.Fatalf("post-purge briefing seq = %+v %v; id reuse would skip it on reconnect", got, err)
	}
	if max, err := s.MaxSeq(context.Background()); err != nil || max != 3 {
		t.Fatalf("max seq = %d, %v", max, err)
	}
}

// Migration v9 reseed: dropping back to v8 (no seq_state) replays the
// seed from MAX(seq), so a new briefing continues the old numbering
// instead of restarting at 1.
func TestMigrateV9ReseedsHighWater(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v9.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	briefGroup(t, s, "host:v9a", "evt-v9a", now)
	briefGroup(t, s, "host:v9b", "evt-v9b", now.Add(time.Minute))
	s.Close()

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE seq_state`,
		`PRAGMA user_version = 8`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	dropProjectionSchema(t, db)
	dropDecisionSchema(t, db)
	dropTraceSchema(t, db)
	dropRuleSchema(t, db)
	db.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("migrate 8->9: %v", err)
	}
	defer s2.Close()
	var version int
	if err := s2.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != supportedSchemaVersion {
		t.Fatalf("want user_version %d, got %d %v", supportedSchemaVersion, version, err)
	}
	// Group ids carry the received second plus a process-local counter
	// that restarts at zero on reopen: wait for the next second so the
	// post-reopen group cannot reuse a pre-reopen id.
	waitNextSecond()
	gid := briefGroup(t, s2, "host:v9c", "evt-v9c", now.Add(2*time.Minute))
	got, err := s2.BriefingForGroup(context.Background(), gid)
	if err != nil || got.Seq != 3 {
		t.Fatalf("post-reseed briefing seq = %+v %v", got, err)
	}
}

// H10: a planted symlink at the database path is refused, never
// followed — payloads must not land in (or be read from) the target.
func TestOpenRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.db")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("symlinked database path must be refused")
	}
}

// H10: a group/world-writable directory without the sticky bit is
// refused; the sticky bit (shared-temp style) still counts as safe.
func TestOpenRefusesWritableDir(t *testing.T) {
	writable := filepath.Join(t.TempDir(), "wide")
	if err := os.Mkdir(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(writable, "bridge.db")); err == nil {
		t.Fatal("writable directory without sticky bit must be refused")
	}

	sticky := filepath.Join(t.TempDir(), "sticky")
	if err := os.Mkdir(sticky, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(sticky, "bridge.db"))
	if err != nil {
		t.Fatalf("sticky directory must be accepted: %v", err)
	}
	s.Close()
}

// H10: a relative database path resolves against the working
// directory: the create, Lstat and DSN steps in Open all run on the
// absolutized path, so SQLite can never open a different file (or fail
// at the filesystem root) while the helpers touch the local one.
func TestOpenRelativePathStaysLocal(t *testing.T) {
	relDir := t.TempDir()
	t.Chdir(relDir)
	s, err := Open("rel.db")
	if err != nil {
		t.Fatalf("open with relative path: %v", err)
	}
	s.Close()
	// Non-empty proves SQLite opened this file, not just that openStore
	// created it: the migration DDL lands in the opened database, so a
	// wrong-location open would leave a zero-byte file behind.
	fi, err := os.Stat(filepath.Join(relDir, "rel.db"))
	if err != nil {
		t.Fatalf("relative database must be created in the working directory: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("relative database must hold the migrated schema, got an empty file")
	}
}

// H10: the DSN pins the pragmas the store depends on, so a dropped
// setting fails here instead of degrading silently at runtime.
func TestDSNPinsPragmas(t *testing.T) {
	// The Windows branch is pinned directly on the pure helper: through
	// dsnForPath a volume path would first run through Abs, which on
	// Linux resolves it under the working directory and never reaches
	// the forced slash.
	for path, want := range map[string]string{
		filepath.Join("some", "dir", "bridge.db"): "/some/dir/bridge.db",
		`C:/x/y.db`: "/C:/x/y.db",
	} {
		if got := fileURIPath(path); got != want {
			t.Fatalf("fileURIPath(%q) = %q, want %q", path, got, want)
		}
		dsn, err := dsnForPath(path)
		if err != nil {
			t.Fatal(err)
		}
		// url.URL would otherwise read "C:" as the authority (rejected
		// by SQLite) or resolve "rel" at the filesystem root: both must
		// render as file:/// with an absolute path.
		if !strings.HasPrefix(dsn, "file:///") {
			t.Fatalf("DSN must be file:/// with an absolute path, got %q", dsn)
		}
		for _, want := range []string{
			"_journal_mode=WAL", "_busy_timeout=5000",
			"_foreign_keys=on", "_txlock=immediate", "_synchronous=NORMAL",
		} {
			if !strings.Contains(dsn, want) {
				t.Fatalf("DSN must pin %q, got %q", want, dsn)
			}
		}
	}
}
