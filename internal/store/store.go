// Package store is WatcherBridge's local persistence core: embedded SQLite
// (mattn/go-sqlite3, WAL) with a single writer goroutine. All writes go through
// one serial queue; reads may run concurrently under WAL. All timestamps are
// stored as INTEGER unix nanoseconds so comparisons are numeric.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type writeJob struct {
	ctx context.Context
	fn  func(*sql.Tx) error
	// raw runs on the writer goroutine without opening a transaction,
	// for statements SQLite forbids inside one (e.g. wal_checkpoint).
	raw  func() error
	done chan error
}

// ErrStoreClosed is returned when the store is shut down; ErrQueueSaturated
// when the bounded writer queue is full. Both mean "retry later", never
// "bad request".
var (
	ErrStoreClosed    = fmt.Errorf("store is closed")
	ErrQueueSaturated = fmt.Errorf("writer queue saturated")
)

// DefaultQueueCap bounds the writer channel when the caller has no number.
const DefaultQueueCap = 64

// Option adjusts Open.
type Option func(*options)

type options struct{ queueCap int }

// WithQueueCap bounds the writer queue; overflow fails fast with
// ErrQueueSaturated instead of blocking ingest.
func WithQueueCap(n int) Option { return func(o *options) { o.queueCap = n } }

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

func nanos(t time.Time) int64 { return t.UTC().UnixNano() }
func fromNanos(n int64) time.Time {
	return time.Unix(0, n).UTC()
}

// Open creates (0600 file in a 0700 directory) or opens the database at path,
// runs migrations, and starts the writer goroutine.
func Open(path string, opts ...Option) (*Store, error) { return openStore(path, nil, opts...) }

// OpenWithWarnFunc is Open with a caller-supplied warning sink (nil = log).
func OpenWithWarnFunc(path string, warn func(format string, args ...any), opts ...Option) (*Store, error) {
	return openStore(path, warn, opts...)
}

func openStore(path string, warn func(format string, args ...any), opts ...Option) (*Store, error) {
	o := options{queueCap: DefaultQueueCap}
	for _, opt := range opts {
		opt(&o)
	}
	if o.queueCap < 1 {
		return nil, fmt.Errorf("writer queue cap must be positive")
	}
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
	s := &Store{db: db, jobs: make(chan writeJob, o.queueCap), quit: make(chan struct{}), now: time.Now}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

// migrate applies ordered migrations keyed by PRAGMA user_version.
// The legacy unversioned layout never shipped in a release, so an existing
// unversioned database is refused with a clear message instead of converted.
// A database newer than this code is refused rather than silently misread.
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
	if job.raw != nil {
		return job.raw()
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
	return s.enqueue(ctx, writeJob{ctx: ctx, fn: fn, done: make(chan error, 1)})
}

// enqueue submits a job without ever blocking the caller: a full queue
// fails fast so ingest can answer 503 instead of piling up memory.
func (s *Store) enqueue(ctx context.Context, job writeJob) error {
	select {
	case <-s.quit:
		return ErrStoreClosed
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	select {
	case <-s.quit:
		return ErrStoreClosed
	case <-ctx.Done():
		return ctx.Err()
	case s.jobs <- job:
	default:
		return ErrQueueSaturated
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
