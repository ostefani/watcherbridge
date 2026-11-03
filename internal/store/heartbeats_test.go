package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func dropHeartbeatSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DROP TABLE IF EXISTS heartbeats`); err != nil {
		t.Fatal(err)
	}
	dropColumnIfExists(t, db, "report_sources", "heartbeat_completed_at")
	dropColumnIfExists(t, db, "report_sources", "heartbeat_summary")
	dropColumnIfExists(t, db, "purge_runs", "heartbeats_deleted")
}

func heartbeatFixture(n int, now time.Time) Heartbeat {
	return Heartbeat{HeartbeatSummary: HeartbeatSummary{ScanID: fmt.Sprintf("%032x", n), Target: "192.0.2.0/24",
		CompletedAt: now, ReceivedAt: now, Status: "ok"}, SourceScope: "lab", SignatureAt: now, Payload: []byte(`{}`)}
}

func TestHeartbeatIdempotencyAndProgress(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.BindSourceIdentity(ctx, "lab", "ns-1", StableIdentityCapability); err != nil {
		t.Fatal(err)
	}
	h := heartbeatFixture(1, now)
	var inserted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.InsertHeartbeat(ctx, h)
			if err != nil {
				t.Error(err)
			}
			if ok {
				inserted.Add(1)
			}
		}()
	}
	wg.Wait()
	if inserted.Load() != 1 {
		t.Fatalf("inserted %d duplicates", inserted.Load())
	}
	// Freshly signed duplicate delivery cannot renew a scan's receipt or age.
	h.ReceivedAt, h.SignatureAt = now.Add(time.Hour), now.Add(time.Hour)
	if ok, err := s.InsertHeartbeat(ctx, h); err != nil || ok {
		t.Fatalf("duplicate: %v %v", ok, err)
	}
	rows, err := s.SourceHealthRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Heartbeat == nil || !rows[0].Heartbeat.FreshAt.Equal(now) || !rows[0].LastHeartbeatAt.Equal(now) {
		t.Fatalf("duplicate renewed source progress: %+v", rows)
	}
	if rows[0].HasLastReport || rows[0].ConfiguredNS != "ns-1" || rows[0].Capabilities != StableIdentityCapability {
		t.Fatalf("H1 changed report age or identity: %+v", rows[0])
	}
	h.Payload = []byte(`{ }`)
	if _, err := s.InsertHeartbeat(ctx, h); !errors.Is(err, ErrHeartbeatConflict) {
		t.Fatalf("conflicting reuse: %v", err)
	}
	// Another queued scan is received now but was completed an hour ago.
	old := heartbeatFixture(2, now.Add(-time.Hour))
	old.ReceivedAt, old.SignatureAt = now.Add(time.Minute), now.Add(time.Minute)
	if _, err := s.InsertHeartbeat(ctx, old); err != nil {
		t.Fatal(err)
	}
	rows, err = s.SourceHealthRows(ctx)
	if err != nil || rows[0].Heartbeat.ScanID != h.ScanID {
		t.Fatalf("out-of-order record regressed summary: %+v %v", rows, err)
	}
	for _, table := range []string{"raw_events", "active_groups", "briefings", "reported_subjects", "projection_pending"} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("H1 entered %s: %d %v", table, count, err)
		}
	}
}

func TestHeartbeatDelayedScanAndClockOffset(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	h := heartbeatFixture(1, now.Add(-time.Hour))
	h.SignatureAt, h.ReceivedAt = now, now.Add(3*time.Second)
	if _, err := s.InsertHeartbeat(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	rows, err := s.SourceHealthRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := now.Add(-time.Hour + 3*time.Second)
	if !rows[0].Heartbeat.FreshAt.Equal(want) || rows[0].Heartbeat.ClockSkewSeconds != -3 {
		t.Fatalf("queued scan was made fresh: %+v", rows[0].Heartbeat)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, err := s.InsertHeartbeat(ctx, heartbeatFixture(3, now)); err == nil || ok {
		t.Fatal("canceled insert committed")
	}
}

func TestHeartbeatQueriesUseIndexes(t *testing.T) {
	s := openTestStore(t)
	assertIndexed(t, explainQuery(t, s, `SELECT payload FROM heartbeats WHERE source_scope='lab' AND event_id='id'`), "H1 duplicate lookup")
	assertIndexed(t, explainQuery(t, s, `SELECT rowid FROM heartbeats WHERE received_at < 100 ORDER BY received_at, rowid LIMIT 198`), "H1 expiry page")
}

func TestHeartbeatPurgeBudgetAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "h1.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	for i := 1; i <= 205; i++ {
		if _, err := s.InsertHeartbeat(ctx, heartbeatFixture(i, now.Add(-8*24*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	// Make the source itself old enough for compaction, so this also
	// exercises the configured-source pin after its last history row drains.
	if _, err := s.db.Exec(`UPDATE report_sources SET last_report_at = ? WHERE source_scope = 'lab'`, nanos(now.Add(-30*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	runID, err := s.beginPurgeRun(ctx, nanos(now))
	if err != nil {
		t.Fatal(err)
	}
	plan := purgePlan{stages: stageSubjects, heartbeatCut: nanos(now.Add(-7 * 24 * time.Hour)), sourceCut: nanos(now.Add(-7 * 24 * time.Hour)), activeSource: "lab", runID: runID}
	first, _, err := s.purgeBatch(ctx, now, plan)
	if err != nil || first.Heartbeats != 198 || first.total()+first.Bookkeeping > 200 {
		t.Fatalf("budget: %+v %v", first, err)
	}
	second, _, err := s.purgeBatch(ctx, now, plan)
	if err != nil || second.Heartbeats != 7 {
		t.Fatalf("remainder: %+v %v", second, err)
	}
	ledger, err := s.LatestPurgeRun(ctx)
	if err != nil || ledger.total.Heartbeats != 205 {
		t.Fatalf("ledger: %+v %v", ledger, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.SourceHealthRows(ctx)
	if err != nil || len(rows) != 1 || !rows[0].HeartbeatEnabled || rows[0].Heartbeat == nil {
		t.Fatalf("summary lost on purge/restart: %+v %v", rows, err)
	}
	before := *rows[0].Heartbeat
	replay := heartbeatFixture(1, now.Add(-8*24*time.Hour))
	replay.ReceivedAt, replay.SignatureAt = now, now
	if _, err := s.InsertHeartbeat(ctx, replay); err != nil {
		t.Fatal(err)
	}
	rows, err = s.SourceHealthRows(ctx)
	if err != nil || *rows[0].Heartbeat != before {
		t.Fatalf("post-retention replay refreshed source: %+v %v", rows, err)
	}
	if err := s.BindSourceScope(ctx, "other"); err == nil {
		t.Fatal("H1-only database lost its source binding")
	}
}

func TestHeartbeatMigrationFromV24(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindSourceIdentity(context.Background(), "lab", "ns-1", StableIdentityCapability); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	dropHeartbeatSchema(t, db)
	if _, err := db.Exec(`PRAGMA user_version = 24`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ns, caps, err := s.SourceIdentity(context.Background(), "lab")
	if err != nil || ns != "ns-1" || caps != StableIdentityCapability {
		t.Fatalf("migration changed identity contract: %s %s %v", ns, caps, err)
	}
	if _, err := s.InsertHeartbeat(context.Background(), heartbeatFixture(1, time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatSaturationRecovers(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "pressure.db"), WithQueueCap(1))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() { first <- s.write(ctx, func(*sql.Tx) error { close(entered); <-release; return nil }) }()
	<-entered
	second := make(chan error, 1)
	go func() { second <- s.write(ctx, func(*sql.Tx) error { return nil }) }()
	deadline := time.Now().Add(time.Second)
	for s.QueueDepth() != 1 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("writer queue did not fill")
		}
		time.Sleep(time.Millisecond)
	}
	h := heartbeatFixture(1, time.Now().UTC())
	ok, err := s.InsertHeartbeat(ctx, h)
	close(release)
	if e := <-first; e != nil {
		t.Fatal(e)
	}
	if e := <-second; e != nil {
		t.Fatal(e)
	}
	if ok || !errors.Is(err, ErrQueueSaturated) {
		t.Fatalf("saturated insert: %v %v", ok, err)
	}
	if ok, err := s.InsertHeartbeat(ctx, h); err != nil || !ok {
		t.Fatalf("retry did not recover: %v %v", ok, err)
	}
}
