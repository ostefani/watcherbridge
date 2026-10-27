// Package store is WatcherBridge's local persistence core: embedded SQLite
// (mattn/go-sqlite3, WAL) with a single writer goroutine. All writes go through
// one serial queue; reads may run concurrently under WAL.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
	PayloadJSON        string
	ReceivedAt         time.Time
	SignatureTimestamp time.Time
}

type writeResult struct {
	inserted bool
	err      error
}

type writeJob struct {
	fn   func(*sql.Tx) (bool, error)
	done chan writeResult
}

// Store owns the database. The zero value is invalid; use Open.
type Store struct {
	db       *sql.DB
	writes   chan writeJob
	closed   chan struct{}
	wg       sync.WaitGroup
	once     sync.Once
	groupSeq atomic.Int64
}

const schema = `
CREATE TABLE IF NOT EXISTS raw_events (
    event_id TEXT PRIMARY KEY,
    event_key TEXT NOT NULL,
    event_type TEXT NOT NULL,
    occurred_at DATETIME NOT NULL,
    cycle INTEGER NOT NULL,
    target TEXT,
    payload_json TEXT NOT NULL,
    received_at DATETIME NOT NULL,
    signature_timestamp DATETIME NOT NULL,
    forwarded INTEGER NOT NULL DEFAULT 0,
    forwarded_at DATETIME,
    forward_attempts INTEGER NOT NULL DEFAULT 0,
    last_forward_error TEXT,
    delivery_error TEXT
);
CREATE TABLE IF NOT EXISTS active_groups (
    group_id TEXT PRIMARY KEY,
    host_anchor_key TEXT NOT NULL,
    opened_at DATETIME NOT NULL,
    deadline_at DATETIME NOT NULL,
    event_count INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL CHECK(status IN ('open','processing','closed','failed'))
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
    created_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS pseudonym_map (
    real_value TEXT PRIMARY KEY,
    category TEXT NOT NULL CHECK(category IN ('ip','mac','hostname','net')),
    token TEXT NOT NULL UNIQUE,
    created_at DATETIME NOT NULL
);
`

// Open creates (0600 file in a 0700 directory) or opens the database at path,
// applies pragmas and schema, and starts the writer goroutine.
func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create database file: %w", err)
		}
		f.Close()
	}
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// Writes are serialized by the single writer goroutine, so the pool can
	// safely serve concurrent WAL readers instead of blocking them behind
	// the one connection during a write transaction.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	s := &Store{db: db, writes: make(chan writeJob), closed: make(chan struct{})}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

func (s *Store) writer() {
	defer s.wg.Done()
	for job := range s.writes {
		tx, err := s.db.Begin()
		if err != nil {
			job.done <- writeResult{err: err}
			continue
		}
		inserted, err := job.fn(tx)
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		job.done <- writeResult{inserted: inserted, err: err}
	}
}

func (s *Store) write(ctx context.Context, fn func(*sql.Tx) (bool, error)) (bool, error) {
	job := writeJob{fn: fn, done: make(chan writeResult, 1)}
	select {
	case <-s.closed:
		return false, fmt.Errorf("store is closed")
	case s.writes <- job:
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case res := <-job.done:
		return res.inserted, res.err
	}
}

func (s *Store) Close() error {
	var err error
	s.once.Do(func() {
		close(s.closed)
		close(s.writes)
		s.wg.Wait()
		err = s.db.Close()
	})
	return err
}

// PersistEventAndGroup stores the event and adds it to the anchor's open
// group (creating one with the given deadline) in a single transaction.
// A duplicate event_id inserts nothing and reports inserted=false, so
// retries can never double-group.
func (s *Store) PersistEventAndGroup(ctx context.Context, ev RawEvent, anchor string, deadline time.Time) (bool, error) {
	return s.write(ctx, func(tx *sql.Tx) (bool, error) {
		res, err := tx.Exec(`INSERT OR IGNORE INTO raw_events
			(event_id, event_key, event_type, occurred_at, cycle, target, payload_json, received_at, signature_timestamp)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.EventID, ev.EventKey, ev.EventType, ev.OccurredAt, ev.Cycle, ev.Target, ev.PayloadJSON, ev.ReceivedAt, ev.SignatureTimestamp)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return false, nil
		}
		var groupID string
		err = tx.QueryRow(`SELECT group_id FROM active_groups WHERE host_anchor_key = ? AND status = 'open' ORDER BY opened_at LIMIT 1`, anchor).Scan(&groupID)
		if err == sql.ErrNoRows {
			groupID = fmt.Sprintf("grp-%d-%d", ev.ReceivedAt.UnixNano(), s.groupSeq.Add(1))
			if _, err := tx.Exec(`INSERT INTO active_groups (group_id, host_anchor_key, opened_at, deadline_at, status) VALUES (?, ?, ?, ?, 'open')`,
				groupID, anchor, ev.ReceivedAt, deadline); err != nil {
				return false, err
			}
		} else if err != nil {
			return false, err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO group_events (group_id, event_id) VALUES (?, ?)`, groupID, ev.EventID); err != nil {
			return false, err
		}
		_, err = tx.Exec(`UPDATE active_groups SET event_count = (SELECT COUNT(*) FROM group_events WHERE group_id = ?) WHERE group_id = ?`, groupID, groupID)
		return true, err
	})
}

// Recover closes expired open/processing groups and requeues processing
// groups that never produced a briefing. It returns the affected group IDs.
func (s *Store) Recover(ctx context.Context, now time.Time) ([]string, error) {
	var ids []string
	_, err := s.write(ctx, func(tx *sql.Tx) (bool, error) {
		rows, err := tx.Query(`SELECT group_id FROM active_groups
			WHERE (status IN ('open','processing') AND deadline_at <= ?)
			   OR (status = 'processing' AND NOT EXISTS (SELECT 1 FROM briefings WHERE briefings.group_id = active_groups.group_id))`, now)
		if err != nil {
			return false, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return false, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return false, err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE active_groups SET status = 'closed' WHERE group_id = ?`, id); err != nil {
				return false, err
			}
		}
		return len(ids) > 0, nil
	})
	return ids, err
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

// MarkForwarded flips the forwarded flag after ScanRelay accepts (or reports
// a replay of) the event.
func (s *Store) MarkForwarded(ctx context.Context, eventID string, at time.Time) error {
	_, err := s.write(ctx, func(tx *sql.Tx) (bool, error) {
		_, err := tx.Exec(`UPDATE raw_events SET forwarded = 1, forwarded_at = ?, forward_attempts = forward_attempts + 1 WHERE event_id = ?`, at, eventID)
		return true, err
	})
	return err
}
