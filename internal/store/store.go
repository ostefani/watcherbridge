// Package store is WatcherBridge's local persistence core: embedded SQLite
// (mattn/go-sqlite3, WAL) with a single writer goroutine. All writes go through
// one serial queue; reads may run concurrently under WAL. All timestamps are
// stored as INTEGER unix nanoseconds so comparisons are numeric.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type RawEvent struct {
	EventID            string
	EventKey           string
	EventType          string
	OccurredAt         time.Time
	Cycle              int
	Target             string
	Payload            []byte // original webhook bytes, never re-marshalled
	HeadersJSON        string // forwarded headers (incl. Pro signature), JSON object
	ReceivedAt         time.Time
	SignatureTimestamp time.Time
}

type GroupPolicy struct {
	QuietWindow time.Duration // debounce: group closes this long after last event
	MaxWindow   time.Duration // hard cap from first event
	MaxEvents   int           // size cap; 0 = unlimited
}

type PersistResult struct {
	Inserted       bool
	GroupID        string
	Closed         bool     // size cap reached; group is ready for briefing
	ClosedGroupIDs []string // overdue groups closed by this call (hard cap)
}

type writeJob struct {
	ctx  context.Context
	fn   func(*sql.Tx) error
	done chan error
}

// Store owns the database. The zero value is invalid; use Open.
type Store struct {
	db       *sql.DB
	jobs     chan writeJob // never closed; quit signals shutdown
	quit     chan struct{}
	wg       sync.WaitGroup
	once     sync.Once
	groupSeq atomic.Int64
	now      func() time.Time
}

const schemaV1 = `
CREATE TABLE IF NOT EXISTS raw_events (
    event_id TEXT PRIMARY KEY,
    event_key TEXT NOT NULL,
    event_type TEXT NOT NULL,
    occurred_at INTEGER NOT NULL,
    cycle INTEGER NOT NULL,
    target TEXT,
    payload BLOB NOT NULL,
    headers_json TEXT NOT NULL DEFAULT '',
    received_at INTEGER NOT NULL,
    signature_timestamp INTEGER NOT NULL,
    forwarded INTEGER NOT NULL DEFAULT 0,
    forwarded_at INTEGER,
    forward_attempts INTEGER NOT NULL DEFAULT 0,
    last_forward_error TEXT,
    delivery_error TEXT
);
CREATE TABLE IF NOT EXISTS active_groups (
    group_id TEXT PRIMARY KEY,
    host_anchor_key TEXT NOT NULL,
    opened_at INTEGER NOT NULL,
    last_event_at INTEGER NOT NULL,
    deadline_at INTEGER NOT NULL,
    event_count INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL CHECK(status IN ('open','processing','closed','done','failed'))
);
CREATE TABLE IF NOT EXISTS group_events (
    group_id TEXT NOT NULL REFERENCES active_groups(group_id) ON DELETE CASCADE,
    event_id TEXT NOT NULL REFERENCES raw_events(event_id),
    PRIMARY KEY(group_id, event_id)
);
CREATE TABLE IF NOT EXISTS briefings (
    briefing_id TEXT PRIMARY KEY,
    group_id TEXT NOT NULL REFERENCES active_groups(group_id),
    host_anchor_key TEXT NOT NULL,
    baseline TEXT NOT NULL,
    model_suggestion TEXT,
    priority TEXT NOT NULL,
    priority_reason TEXT NOT NULL,
    clamped INTEGER NOT NULL DEFAULT 0,
    fallback_used INTEGER NOT NULL DEFAULT 0,
    what_changed TEXT NOT NULL,
    context TEXT NOT NULL,
    recommended_check TEXT NOT NULL,
    model_raw_json TEXT,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS pseudonym_map (
    real_value TEXT PRIMARY KEY,
    category TEXT NOT NULL CHECK(category IN ('ip','mac','hostname','net')),
    token TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS pseudonym_counters (
    category TEXT PRIMARY KEY,
    next INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS one_open_group_per_anchor ON active_groups(host_anchor_key) WHERE status = 'open';
CREATE UNIQUE INDEX IF NOT EXISTS one_briefing_per_group ON briefings(group_id);
CREATE INDEX IF NOT EXISTS raw_forward_sweep ON raw_events(forwarded, signature_timestamp);
CREATE INDEX IF NOT EXISTS raw_received ON raw_events(received_at);
CREATE INDEX IF NOT EXISTS groups_status_deadline ON active_groups(status, deadline_at);
`

func nanos(t time.Time) int64 { return t.UTC().UnixNano() }
func fromNanos(n int64) time.Time {
	return time.Unix(0, n).UTC()
}

// Open creates (0600 file in a 0700 directory) or opens the database at path,
// runs migrations, and starts the writer goroutine.
func Open(path string) (*Store, error) { return openStore(path, nil) }

// OpenWithWarnFunc is Open with a caller-supplied warning sink (nil = log).
func OpenWithWarnFunc(path string, warn func(format string, args ...any)) (*Store, error) {
	return openStore(path, warn)
}

func openStore(path string, warn func(format string, args ...any)) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	path = abs
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("tighten data directory permissions: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("stat data directory: %w", err)
	}
	// An existing directory is the caller's choice (e.g. /tmp): never chmod
	// a directory this call did not create, but warn if it is writable by others.
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm()&0o022 != 0 {
		if warn != nil {
			warn("data directory %s is group/world writable (%o)", dir, info.Mode().Perm())
		} else {
			log.Printf("store: warning: data directory %s is group/world writable (%o)", dir, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create database file: %w", err)
		}
		f.Close()
	}
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("_journal_mode", "WAL")
	q.Set("_busy_timeout", "5000")
	q.Set("_foreign_keys", "on")
	q.Set("_txlock", "immediate")
	q.Set("_synchronous", "NORMAL")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("tighten database permissions: %w", err)
	}
	s := &Store{db: db, jobs: make(chan writeJob), quit: make(chan struct{}), now: time.Now}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

// migrate applies ordered migrations keyed by PRAGMA user_version.
// The legacy unversioned layout never shipped in a release, so an existing
// unversioned database is refused with a clear message instead of converted.
// A database newer than this code is refused rather than silently misread.
const supportedSchemaVersion = 3

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > supportedSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported %d; upgrade the binary", version, supportedSchemaVersion)
	}
	if version == 0 {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='raw_events'`).Scan(&name)
		if err == nil {
			return fmt.Errorf("unversioned legacy database is not supported; delete the database file and restart to recreate it")
		}
		if err != sql.ErrNoRows {
			return err
		}
	}
	for version < supportedSchemaVersion {
		var err error
		switch version {
		case 0:
			err = applyMigration(db, 1, func(tx *sql.Tx) error { _, err := tx.Exec(schemaV1); return err })
		case 1:
			err = applyMigration(db, 2, migrateV2)
		case 2:
			err = applyMigration(db, 3, migrateV3)
		}
		if err != nil {
			return err
		}
		version++
	}
	return nil
}

func applyMigration(db *sql.DB, to int, fn func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, to)); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateV2: retry_count on groups; pseudonym_map keyed by (category,
// real_value) with the original display value kept for exact restoration;
// counters seeded past the highest existing token per category.
func migrateV2(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE active_groups ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE pseudonym_map_v2 (
			category TEXT NOT NULL CHECK(category IN ('ip','mac','hostname','net')),
			real_value TEXT NOT NULL,
			display_value TEXT NOT NULL,
			token TEXT NOT NULL UNIQUE,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (category, real_value)
		)`,
		`INSERT INTO pseudonym_map_v2 (category, real_value, display_value, token, created_at)
			SELECT category, real_value, real_value, token, created_at FROM pseudonym_map`,
		`DROP TABLE pseudonym_map`,
		`ALTER TABLE pseudonym_map_v2 RENAME TO pseudonym_map`,
		`INSERT INTO pseudonym_counters (category, next)
			SELECT category, MAX(CAST(substr(token, instr(token, '_') + 1) AS INTEGER)) + 1 FROM pseudonym_map WHERE true GROUP BY category
			ON CONFLICT(category) DO UPDATE SET next = MAX(next, excluded.next)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV3: processing_since for stale-lease release, briefings gain
// event_ids_json plus CHECK constraints on the priority levels (table
// rebuild), and an event_id index on group_events for purge/FK lookups.
func migrateV3(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE active_groups ADD COLUMN processing_since INTEGER`,
		`CREATE TABLE briefings_v3 (
			briefing_id TEXT PRIMARY KEY,
			group_id TEXT NOT NULL REFERENCES active_groups(group_id),
			host_anchor_key TEXT NOT NULL,
			baseline TEXT NOT NULL CHECK(baseline IN ('low','medium','high','critical')),
			model_suggestion TEXT CHECK(model_suggestion IS NULL OR model_suggestion IN ('low','medium','high','critical')),
			priority TEXT NOT NULL CHECK(priority IN ('low','medium','high','critical')),
			priority_reason TEXT NOT NULL,
			clamped INTEGER NOT NULL DEFAULT 0,
			fallback_used INTEGER NOT NULL DEFAULT 0,
			what_changed TEXT NOT NULL,
			context TEXT NOT NULL,
			recommended_check TEXT NOT NULL,
			model_raw_json TEXT,
			created_at INTEGER NOT NULL,
			event_ids_json TEXT NOT NULL DEFAULT '[]'
		)`,
		`INSERT INTO briefings_v3 (briefing_id, group_id, host_anchor_key, baseline, model_suggestion, priority, priority_reason, clamped, fallback_used, what_changed, context, recommended_check, model_raw_json, created_at)
			SELECT briefing_id, group_id, host_anchor_key, baseline, model_suggestion, priority, priority_reason, clamped, fallback_used, what_changed, context, recommended_check, model_raw_json, created_at FROM briefings`,
		`DROP TABLE briefings`,
		`ALTER TABLE briefings_v3 RENAME TO briefings`,
		`CREATE UNIQUE INDEX one_briefing_per_group ON briefings(group_id)`,
		`CREATE INDEX IF NOT EXISTS group_events_event ON group_events(event_id)`,
		`CREATE INDEX IF NOT EXISTS briefings_created ON briefings(created_at)`,
		`UPDATE briefings SET event_ids_json = COALESCE((SELECT json_group_array(event_id) FROM (SELECT event_id FROM group_events WHERE group_events.group_id = briefings.group_id ORDER BY event_id)), '[]')`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writer() {
	defer s.wg.Done()
	for {
		select {
		case <-s.quit:
			return
		case job := <-s.jobs:
			job.done <- s.runJob(job)
		}
	}
}

func (s *Store) runJob(job writeJob) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("store write panicked: %v", r)
		}
	}()
	if err := job.ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(job.ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit; on panic runs before the recover above
	if err := job.fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// write runs fn on the writer goroutine. A context cancelled before the
// job runs means it never executes; a cancellation during execution usually
// rolls the transaction back via BeginTx. A cancel racing a successful
// commit is still possible, so callers must treat a context error as
// "unknown, retry safely" and rely on idempotency (e.g.
// PersistEventAndGroup reports Inserted=false on the retry).
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	job := writeJob{ctx: ctx, fn: fn, done: make(chan error, 1)}
	select {
	case <-s.quit:
		return fmt.Errorf("store is closed")
	case <-ctx.Done():
		return ctx.Err()
	case s.jobs <- job:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-job.done:
		return err
	}
}

func (s *Store) Close() error {
	var err error
	s.once.Do(func() {
		close(s.quit) // never close s.jobs: a blocked sender must not panic
		s.wg.Wait()
		err = s.db.Close()
	})
	return err
}

// PersistEventAndGroup stores the event and joins the anchor's open group in
// one transaction. The deadline extends on every join:
// min(last_event+QuietWindow, opened+MaxWindow). Reaching MaxEvents closes
// the group (result.Closed) so the next event opens a fresh one.
func (s *Store) PersistEventAndGroup(ctx context.Context, ev RawEvent, anchor string, pol GroupPolicy) (PersistResult, error) {
	if ev.EventID == "" || ev.EventKey == "" || ev.EventType == "" || len(ev.Payload) == 0 {
		return PersistResult{}, fmt.Errorf("event_id, event_key, event_type and payload are required")
	}
	if ev.ReceivedAt.IsZero() || ev.OccurredAt.IsZero() || ev.SignatureTimestamp.IsZero() {
		return PersistResult{}, fmt.Errorf("event timestamps must not be zero")
	}
	if pol.QuietWindow <= 0 {
		return PersistResult{}, fmt.Errorf("group policy QuietWindow must be positive")
	}
	if pol.MaxWindow > 0 && pol.MaxWindow < pol.QuietWindow {
		return PersistResult{}, fmt.Errorf("group policy MaxWindow must be >= QuietWindow")
	}
	if pol.MaxEvents < 0 {
		return PersistResult{}, fmt.Errorf("group policy MaxEvents must not be negative")
	}
	if err := validateHeadersJSON(ev.HeadersJSON); err != nil {
		return PersistResult{}, err
	}
	var result PersistResult
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO raw_events
			(event_id, event_key, event_type, occurred_at, cycle, target, payload, headers_json, received_at, signature_timestamp)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(event_id) DO NOTHING`,
			ev.EventID, ev.EventKey, ev.EventType, nanos(ev.OccurredAt), ev.Cycle, ev.Target, ev.Payload, ev.HeadersJSON, nanos(ev.ReceivedAt), nanos(ev.SignatureTimestamp))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // duplicate: result.Inserted stays false
		}
		result.Inserted = true
		last := nanos(ev.ReceivedAt)
		var opened, existingDeadline int64
		err = tx.QueryRow(`SELECT group_id, opened_at, deadline_at FROM active_groups WHERE host_anchor_key = ? AND status = 'open'`, anchor).Scan(&result.GroupID, &opened, &existingDeadline)
		if err == nil && existingDeadline <= last {
			// Window expired (quiet or hard cap): a late event must not
			// join an expired group. Close it here and open a fresh one.
			if _, err := tx.Exec(`UPDATE active_groups SET status = 'closed' WHERE group_id = ?`, result.GroupID); err != nil {
				return err
			}
			result.ClosedGroupIDs = append(result.ClosedGroupIDs, result.GroupID)
			err = sql.ErrNoRows
		}
		if err == sql.ErrNoRows {
			result.GroupID = fmt.Sprintf("grp-%d-%d", last, s.groupSeq.Add(1))
			opened = last
			deadline := groupDeadline(opened, last, pol)
			if _, err := tx.Exec(`INSERT INTO active_groups (group_id, host_anchor_key, opened_at, last_event_at, deadline_at, status) VALUES (?, ?, ?, ?, ?, 'open')`,
				result.GroupID, anchor, opened, last, deadline); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			deadline := groupDeadline(opened, last, pol)
			if _, err := tx.Exec(`UPDATE active_groups SET last_event_at = ?, deadline_at = ? WHERE group_id = ?`, last, deadline, result.GroupID); err != nil {
				return err
			}
		}
		res, err = tx.Exec(`INSERT INTO group_events (group_id, event_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, result.GroupID, ev.EventID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			if _, err := tx.Exec(`UPDATE active_groups SET event_count = event_count + 1 WHERE group_id = ?`, result.GroupID); err != nil {
				return err
			}
		}
		if pol.MaxEvents > 0 {
			var count int
			if err := tx.QueryRow(`SELECT event_count FROM active_groups WHERE group_id = ?`, result.GroupID).Scan(&count); err != nil {
				return err
			}
			if count >= pol.MaxEvents {
				if _, err := tx.Exec(`UPDATE active_groups SET status = 'closed' WHERE group_id = ?`, result.GroupID); err != nil {
					return err
				}
				result.Closed = true
			}
		}
		return nil
	})
	if err != nil {
		return PersistResult{}, err
	}
	return result, nil
}

func groupDeadline(opened, last int64, pol GroupPolicy) int64 {
	deadline := last + pol.QuietWindow.Nanoseconds()
	if pol.MaxWindow > 0 {
		if limit := opened + pol.MaxWindow.Nanoseconds(); deadline > limit {
			deadline = limit
		}
	}
	return deadline
}

type TakenGroup struct {
	GroupID    string
	Anchor     string
	OpenedAt   time.Time
	RetryCount int // see LastAttempt
	Events     []RawEvent
}

// LastAttempt reports whether this take is the final try before the retry
// cap: the worker should skip the provider and write a fallback directly.
func (g TakenGroup) LastAttempt() bool { return g.RetryCount >= maxGroupRetries-1 }

var ErrGroupNotClosed = errors.New("group is not closed")

// TakeGroup atomically moves a closed group -> processing and reads the
// group and its events in the same transaction, so a failure cannot strand
// the group in processing. Only closed groups may be taken.
func (s *Store) TakeGroup(ctx context.Context, groupID string) (TakenGroup, error) {
	var taken TakenGroup
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE active_groups SET status = 'processing', processing_since = ? WHERE group_id = ? AND status = 'closed'`, nanos(s.now()), groupID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrGroupNotClosed
		}
		taken.GroupID = groupID
		var openedNanos int64
		if err := tx.QueryRow(`SELECT host_anchor_key, opened_at, retry_count FROM active_groups WHERE group_id = ?`, groupID).Scan(&taken.Anchor, &openedNanos, &taken.RetryCount); err != nil {
			return err
		}
		taken.OpenedAt = fromNanos(openedNanos)
		rows, err := tx.Query(`SELECT e.event_id, e.event_key, e.event_type, e.occurred_at, e.cycle, COALESCE(e.target,''), e.payload, e.headers_json, e.received_at, e.signature_timestamp
			FROM group_events ge JOIN raw_events e ON e.event_id = ge.event_id WHERE ge.group_id = ? ORDER BY e.received_at`, groupID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev RawEvent
			var occurred, received, sig int64
			if err := rows.Scan(&ev.EventID, &ev.EventKey, &ev.EventType, &occurred, &ev.Cycle, &ev.Target, &ev.Payload, &ev.HeadersJSON, &received, &sig); err != nil {
				return err
			}
			ev.OccurredAt, ev.ReceivedAt, ev.SignatureTimestamp = fromNanos(occurred), fromNanos(received), fromNanos(sig)
			taken.Events = append(taken.Events, ev)
		}
		return rows.Err()
	})
	if err != nil {
		return TakenGroup{}, err
	}
	return taken, nil
}

// ReleaseStale returns processing groups older than maxAge to closed (or
// failed at the retry cap) — the runtime counterpart for hung workers.
// maxAge is a lease, not a timeout guess: it must exceed provider timeout
// x2 (call + repair attempt) plus margin, or a slow worker and its
// replacement will both pay for model calls (correctness still holds via
// the state check and unique briefing index). See Designv5 settings.
func (s *Store) ReleaseStale(ctx context.Context, now time.Time, maxAge time.Duration) ([]string, error) {
	cutoff := nanos(now.Add(-maxAge))
	var ids []string
	err := s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT group_id FROM active_groups WHERE status = 'processing' AND processing_since IS NOT NULL AND processing_since <= ?`, cutoff)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE active_groups SET status = CASE WHEN retry_count + 1 >= ? THEN 'failed' ELSE 'closed' END, retry_count = retry_count + 1, processing_since = NULL WHERE group_id = ? AND status = 'processing'`, maxGroupRetries, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// FailedGroups lists groups parked after exhausting retries.
func (s *Store) FailedGroups(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT group_id FROM active_groups WHERE status = 'failed' ORDER BY opened_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Recover (startup only) closes expired open groups, and returns crashed
// processing groups to closed — or to failed once retries are exhausted.
// 'closed' means needs a briefing; 'done' means briefed. Returns nil on error.
func (s *Store) Recover(ctx context.Context, now time.Time) ([]string, error) {
	var ids []string
	err := s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT group_id FROM active_groups
			WHERE (status IN ('open','processing') AND deadline_at <= ?)
			   OR (status = 'processing' AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id))`, nanos(now))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE active_groups SET status = CASE WHEN status = 'processing' AND retry_count + 1 >= ? THEN 'failed' ELSE 'closed' END, retry_count = retry_count + (CASE WHEN status = 'processing' THEN 1 ELSE 0 END), processing_since = NULL WHERE group_id = ?`, maxGroupRetries, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// CloseDue moves only open groups past their deadline to closed. It is the
// runtime tick counterpart of Recover (which is startup-only): it never
// touches processing groups, so in-flight briefings are not requeued.
func (s *Store) CloseDue(ctx context.Context, now time.Time) ([]string, error) {
	var ids []string
	err := s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT group_id FROM active_groups WHERE status = 'open' AND deadline_at <= ?`, nanos(now))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE active_groups SET status = 'closed' WHERE group_id = ? AND status = 'open'`, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

const maxGroupRetries = 5

// ReleaseGroup returns a processing group to closed for a later retry and
// increments its retry counter. At maxGroupRetries the group is parked in
// 'failed' instead, so a poison group cannot loop forever.
func (s *Store) ReleaseGroup(ctx context.Context, groupID string) (bool, error) {
	matched := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE active_groups SET status = CASE WHEN retry_count + 1 >= ? THEN 'failed' ELSE 'closed' END, retry_count = retry_count + 1, processing_since = NULL WHERE group_id = ? AND status = 'processing'`, maxGroupRetries, groupID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		matched = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

// NeedsBriefing lists closed groups with no briefing row yet.
func (s *Store) NeedsBriefing(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT group_id FROM active_groups WHERE status = 'closed'
		AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id) ORDER BY opened_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type Briefing struct {
	BriefingID, GroupID, HostAnchorKey, Baseline string
	ModelSuggestion                              *string
	Priority, PriorityReason                     string
	Clamped, FallbackUsed                        bool
	WhatChanged, Context, RecommendedCheck       string
	ModelRawJSON                                 string
	CreatedAt                                    time.Time
	EventIDs                                     []string // derived from group_events when empty
}

func groupEventIDs(tx *sql.Tx, groupID string) ([]string, error) {
	rows, err := tx.Query(`SELECT event_id FROM group_events WHERE group_id = ? ORDER BY event_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// InsertBriefing stores the group's single briefing and marks it done.
// The one_briefing_per_group unique index makes a second insert fail.
func (s *Store) InsertBriefing(ctx context.Context, b Briefing) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		eventIDs := b.EventIDs
		derived, err := groupEventIDs(tx, b.GroupID)
		if err != nil {
			return err
		}
		if len(eventIDs) == 0 {
			eventIDs = derived
		} else {
			got := append([]string(nil), eventIDs...)
			sort.Strings(got)
			if !slices.Equal(got, derived) {
				return fmt.Errorf("event_ids do not match group_events for group %s", b.GroupID)
			}
			eventIDs = derived
		}
		eventIDsJSON, err := json.Marshal(eventIDs)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO briefings (briefing_id, group_id, host_anchor_key, baseline, model_suggestion, priority, priority_reason, clamped, fallback_used, what_changed, context, recommended_check, model_raw_json, created_at, event_ids_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.BriefingID, b.GroupID, b.HostAnchorKey, b.Baseline, b.ModelSuggestion, b.Priority, b.PriorityReason, b.Clamped, b.FallbackUsed, b.WhatChanged, b.Context, b.RecommendedCheck, b.ModelRawJSON, nanos(b.CreatedAt), string(eventIDsJSON)); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE active_groups SET status = 'done' WHERE group_id = ? AND status = 'processing'`, b.GroupID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("group %s is not in processing state", b.GroupID)
		}
		return nil
	})
}

func (s *Store) GroupEventCount(ctx context.Context, anchor string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_events ge
		JOIN active_groups ag ON ag.group_id = ge.group_id WHERE ag.host_anchor_key = ?`, anchor).Scan(&n)
	return n, err
}

func (s *Store) GroupStatus(ctx context.Context, groupID string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM active_groups WHERE group_id = ?`, groupID).Scan(&status)
	return status, err
}

func (s *Store) GroupDeadline(ctx context.Context, groupID string) (time.Time, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT deadline_at FROM active_groups WHERE group_id = ?`, groupID).Scan(&n)
	return fromNanos(n), err
}

// ForwardState reports delivery state for one event.
func (s *Store) ForwardState(ctx context.Context, eventID string) (forwarded bool, attempts int, lastErr string, err error) {
	var fwd int
	var last sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT forwarded, forward_attempts, last_forward_error FROM raw_events WHERE event_id = ?`, eventID).Scan(&fwd, &attempts, &last)
	return fwd == 1, attempts, last.String, err
}

// MarkForwarded records a successful delivery once; repeats are no-ops.
func (s *Store) MarkForwarded(ctx context.Context, eventID string, at time.Time) (bool, error) { // returns false on any error (captured var is only read on success)
	matched := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE raw_events SET forwarded = 1, forwarded_at = ?, forward_attempts = forward_attempts + 1 WHERE event_id = ? AND forwarded = 0`, nanos(at), eventID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		matched = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

var (
	urlPattern  = regexp.MustCompile(`https?://\S+`)
	addrPattern = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(:\d+)?\b`)
)

// sanitizeErrorText strips URLs and bare addresses (they can embed hosts or
// credentials) and caps length on a rune boundary before persisting.
// The result stays local; it must never be placed in a provider bundle.
func sanitizeErrorText(s string) string {
	s = urlPattern.ReplaceAllString(s, "[url]")
	s = addrPattern.ReplaceAllString(s, "[addr]")
	if len(s) > 256 {
		r := []rune(s)
		if len(r) > 256 {
			s = string(r[:256])
		}
	}
	return s
}

func (s *Store) RecordForwardFailure(ctx context.Context, eventID, errText string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE raw_events SET forward_attempts = forward_attempts + 1, last_forward_error = ? WHERE event_id = ?`, sanitizeErrorText(errText), eventID)
		return err
	})
}

const briefingPurgeFloor = 24 * time.Hour

// PurgeBriefings deletes briefings (and their emptied done groups) created
// before cutoff. Separate from raw-event retention: briefing history may
// outlive the raw log.
func (s *Store) PurgeBriefings(ctx context.Context, now, cutoff time.Time) (int64, error) {
	if now.Sub(cutoff) < briefingPurgeFloor {
		return 0, fmt.Errorf("briefing purge cutoff must be at least %s old", briefingPurgeFloor)
	}
	var affected int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM briefings WHERE created_at < ?`, nanos(cutoff))
		if err != nil {
			return err
		}
		affected, _ = res.RowsAffected()
		_, err = tx.Exec(`DELETE FROM active_groups WHERE status = 'done' AND group_id NOT IN (SELECT group_id FROM briefings) AND group_id NOT IN (SELECT group_id FROM group_events)`)
		return err
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// SweepCandidates returns unforwarded events still fresh enough to forward
// (signature_timestamp within tolerance-margin); older ones are the caller's
// to mark via MarkStaleSweep.
func (s *Store) SweepCandidates(ctx context.Context, now time.Time, tolerance, margin time.Duration, limit int) ([]RawEvent, error) {
	if margin >= tolerance {
		return nil, fmt.Errorf("sweep margin must be smaller than tolerance")
	}
	if limit <= 0 {
		limit = 20 // payloads can be ~1 MiB; keep one batch bounded
	}
	cutoff := nanos(now.Add(-(tolerance - margin)))
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, event_key, event_type, occurred_at, cycle, COALESCE(target,''), payload, headers_json, received_at, signature_timestamp
		FROM raw_events WHERE forwarded = 0 AND delivery_error IS NULL AND signature_timestamp >= ? ORDER BY received_at LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawEvent
	for rows.Next() {
		var ev RawEvent
		var occurred, received, sig int64
		if err := rows.Scan(&ev.EventID, &ev.EventKey, &ev.EventType, &occurred, &ev.Cycle, &ev.Target, &ev.Payload, &ev.HeadersJSON, &received, &sig); err != nil {
			return nil, err
		}
		ev.OccurredAt, ev.ReceivedAt, ev.SignatureTimestamp = fromNanos(occurred), fromNanos(received), fromNanos(sig)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) MarkStaleSweep(ctx context.Context, now time.Time, tolerance, margin time.Duration) (int64, error) {
	if margin >= tolerance {
		return 0, fmt.Errorf("sweep margin must be smaller than tolerance")
	}
	var affected int64
	cutoff := nanos(now.Add(-(tolerance - margin)))
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE raw_events SET delivery_error = 'stale_for_sweep' WHERE forwarded = 0 AND delivery_error IS NULL AND signature_timestamp < ?`, cutoff)
		if err != nil {
			return err
		}
		affected, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// purgeFloor is the youngest age PurgeRawEvents will ever accept.
const purgeFloor = time.Hour

// failedEventFloor keeps failed groups' events for debugging a week.
const failedEventFloor = 7 * 24 * time.Hour

// PurgeRawEvents deletes raw events received before cutoff, but only those
// whose group is done (or that belong to no group). Failed groups' events
// use a longer floor (failedEventFloor, 7 days) and the failed row is
// deleted once its events are gone. Briefings and groups are
// kept: they have their own retention (PurgeBriefings). The cutoff must be at
// least minAge old, so replay/sweep windows are never weakened.
func (s *Store) PurgeRawEvents(ctx context.Context, now, cutoff time.Time, minAge time.Duration) (int64, error) {
	if minAge < purgeFloor {
		minAge = purgeFloor
	}
	if now.Sub(cutoff) < minAge {
		return 0, fmt.Errorf("purge cutoff must be at least %s old", minAge)
	}
	var affected int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		cut := nanos(cutoff)
		if _, err := tx.Exec(`DELETE FROM group_events
			WHERE group_id IN (SELECT group_id FROM active_groups WHERE status = 'done')
			  AND event_id IN (SELECT event_id FROM raw_events WHERE received_at < ?)`, cut); err != nil {
			return err
		}
		// Failed groups keep their events longer (debugging evidence):
		// their effective cutoff is at least failedEventFloor old.
		failedCut := cut
		if latest := nanos(now.Add(-failedEventFloor)); failedCut > latest {
			failedCut = latest
		}
		if _, err := tx.Exec(`DELETE FROM group_events
			WHERE group_id IN (SELECT group_id FROM active_groups WHERE status = 'failed')
			  AND event_id IN (SELECT event_id FROM raw_events WHERE received_at < ?)`, failedCut); err != nil {
			return err
		}
		res, err := tx.Exec(`DELETE FROM raw_events WHERE received_at < ? AND event_id NOT IN (SELECT event_id FROM group_events)`, cut)
		if err != nil {
			return err
		}
		affected, _ = res.RowsAffected()
		// Failed groups are deleted together with their purged events.
		_, err = tx.Exec(`DELETE FROM active_groups WHERE status = 'failed' AND deadline_at < ?
			AND NOT EXISTS (SELECT 1 FROM group_events WHERE group_events.group_id = active_groups.group_id)
			AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id)`, failedCut)
		return err
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

var tokenPrefixes = map[string]string{"ip": "IP", "mac": "MAC", "hostname": "HOST", "net": "NET"}

func normalizePseudonymValue(category, value string) string {
	value = strings.TrimSpace(value)
	switch category {
	case "ip":
		if ip := net.ParseIP(value); ip != nil {
			return ip.String()
		}
	case "mac":
		if mac, err := net.ParseMAC(strings.ToLower(value)); err == nil {
			return mac.String()
		}
		return strings.ToLower(value)
	case "hostname":
		return strings.TrimSuffix(strings.ToLower(value), ".")
	case "net":
		if _, n, err := net.ParseCIDR(value); err == nil {
			return n.String()
		}
	}
	return value
}

// lookupPseudonym is a plain read; only misses go through the writer.
func (s *Store) lookupPseudonym(ctx context.Context, category, normalized string) (string, bool, error) {
	var token string
	err := s.db.QueryRowContext(ctx, `SELECT token FROM pseudonym_map WHERE category = ? AND real_value = ?`, category, normalized).Scan(&token)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return token, err == nil, err
}

// GetOrAssignPseudonym returns the stable token for a real value, assigning
// the next per-category counter value on first sight. The map is keyed by
// normalized value; display_value keeps the original text for restoration.
func (s *Store) GetOrAssignPseudonym(ctx context.Context, category, realValue string) (string, error) {
	tokens, err := s.GetOrAssignPseudonyms(ctx, category, realValue)
	if err != nil {
		return "", err
	}
	return tokens[realValue], nil
}

// Note: if ctx is cancelled after the writer commits, the caller sees an
// error although tokens were assigned; the next call finds them (read-first),
// so this is harmless.
func (s *Store) GetOrAssignPseudonyms(ctx context.Context, category string, realValues ...string) (map[string]string, error) {
	prefix, ok := tokenPrefixes[category]
	if !ok {
		return nil, fmt.Errorf("unknown pseudonym category %q", category)
	}
	result := make(map[string]string, len(realValues))
	type pending struct{ normalized, original string }
	var missing []pending // input order: token numbering is deterministic
	seenMissing := map[string]bool{}
	for _, v := range realValues {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("pseudonym value must not be empty")
		}
		normalized := normalizePseudonymValue(category, v)
		if token, found, err := s.lookupPseudonym(ctx, category, normalized); err != nil {
			return nil, err
		} else if found {
			result[v] = token
		} else if !seenMissing[normalized] {
			seenMissing[normalized] = true
			missing = append(missing, pending{normalized, v})
		}
	}
	if len(missing) == 0 {
		return result, nil
	}
	assigned := map[string]string{}
	err := s.write(ctx, func(tx *sql.Tx) error {
		for _, m := range missing {
			normalized, original := m.normalized, m.original
			var token string
			err := tx.QueryRow(`SELECT token FROM pseudonym_map WHERE category = ? AND real_value = ?`, category, normalized).Scan(&token)
			if err == nil {
				assigned[normalized] = token
				continue
			}
			if err != sql.ErrNoRows {
				return err
			}
			var next int64 = 1
			err = tx.QueryRow(`SELECT next FROM pseudonym_counters WHERE category = ?`, category).Scan(&next)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			token = fmt.Sprintf("%s_%d", prefix, next)
			if _, err := tx.Exec(`INSERT INTO pseudonym_counters (category, next) VALUES (?, ?) ON CONFLICT(category) DO UPDATE SET next = next + 1`, category, next+1); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO pseudonym_map (category, real_value, display_value, token, created_at) VALUES (?, ?, ?, ?, ?)`, category, normalized, original, token, nanos(s.now())); err != nil {
				return err
			}
			assigned[normalized] = token
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, v := range realValues {
		if _, ok := result[v]; !ok {
			result[v] = assigned[normalizePseudonymValue(category, v)]
		}
	}
	return result, nil
}

// WhitelistForwardHeaders reduces a header set to what proxy forwarding may
// store and replay: Pro signature headers plus Content-Type. Endpoint
// credentials are never persisted. Keys are compared case-insensitively
// because Go canonicalizes header names (X-SubnetLens-* arrives as
// X-Subnetlens-*).
func WhitelistForwardHeaders(headers map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range headers {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-subnetlens-") || lk == "content-type" {
			out[k] = v
		}
	}
	return out
}

const maxHeadersJSONBytes = 4096

// validateHeadersJSON enforces the whitelist inside the store, so no caller
// can persist unfiltered headers by forgetting WhitelistForwardHeaders.
func validateHeadersJSON(headersJSON string) error {
	if headersJSON == "" {
		return nil
	}
	if len(headersJSON) > maxHeadersJSONBytes {
		return fmt.Errorf("headers_json exceeds %d bytes", maxHeadersJSONBytes)
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
		return fmt.Errorf("headers_json must be a JSON object of strings: %w", err)
	}
	for k := range headers {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "x-subnetlens-") && lk != "content-type" {
			return fmt.Errorf("header %q is not whitelisted for storage", k)
		}
	}
	return nil
}

// RestorePseudonym returns the original (display) value for a token.
func (s *Store) RestorePseudonym(ctx context.Context, token string) (display string, category string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT display_value, category FROM pseudonym_map WHERE token = ?`, token).Scan(&display, &category)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return display, category, err == nil, err
}
