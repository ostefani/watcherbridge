// Package store is WatcherBridge's local persistence core: embedded SQLite
// (mattn/go-sqlite3, WAL) with a single writer goroutine. All writes go through
// one serial queue; reads may run concurrently under WAL. All timestamps are
// stored as INTEGER unix nanoseconds so comparisons are numeric.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	done chan error // buffered so the writer can reply after caller cancellation
}

// ErrStoreClosed means the writer rejected the job before running it.
var ErrStoreClosed = fmt.Errorf("store is closed")

// ErrQueueSaturated means the writer queue was full and did not admit the job.
var ErrQueueSaturated = fmt.Errorf("writer queue saturated")

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
	db          *sql.DB
	jobs        chan writeJob // closed only under submitMu
	submitMu    sync.Mutex    // serializes submission with closing jobs
	quit        chan struct{}
	wg          sync.WaitGroup
	closeOnce   sync.Once
	closeErr    error
	groupSeq    atomic.Int64
	decisionSeq atomic.Int64
	incidentSeq atomic.Int64
	now         func() time.Time
	// decisionRetention bounds how long operator-decision audit stays
	// effective; zero means the 180d default. Purge deletes audit rows
	// past it, and the effective-state queries ignore them even before
	// the purge runs, so a stalled purge never extends a verdict.
	decisionRetention time.Duration
	// protectedRefillPassHook runs synchronously inside protected
	// token discovery, with the 1-based pass number. It fires twice
	// per pass: after the pass snapshots before scanning, and after
	// the end-of-pass check when another pass will run. Nil in
	// production. Tests use it to stage rows at exact pass
	// boundaries — an interleaving a sleep-based race cannot place
	// deterministically. Rows staged at the second site land strictly
	// between passes: invisible to the finished pass's end-of-pass
	// check, visible to the next pass's snapshots.
	protectedRefillPassHook func(pass int)
}

// SetDecisionRetention attaches the operator-decision audit horizon
// from config. Effective-state selection and purge share it, so what
// the UI applies always matches what retention deletes.
func (s *Store) SetDecisionRetention(d time.Duration) { s.decisionRetention = d }

func (s *Store) decisionCutoff(now time.Time) int64 {
	ret := s.decisionRetention
	if ret <= 0 {
		ret = defaultDecisionRetention
	}
	return nanos(now.Add(-ret))
}

// QueueDepth reports the current writer backlog without blocking. The
// purge loop yields while the backlog passes half the cap so retention
// never starves ingest; the value is advisory, never a gate.
func (s *Store) QueueDepth() int { return len(s.jobs) }

// QueueCap reports the writer queue capacity QueueDepth is measured
// against. Zero on a Store that bypassed Open, which never purges.
func (s *Store) QueueCap() int { return cap(s.jobs) }

func nanos(t time.Time) int64 { return t.UTC().UnixNano() }
func fromNanos(n int64) time.Time {
	return time.Unix(0, n).UTC()
}

// fileURIPath renders one filesystem path as the path part of a file:
// URI. ToSlash plus the forced leading slash keeps the DSN valid on
// Windows: without it a volume path renders as file://C:/...
// (authority "C:", rejected by SQLite). Pure and platform-independent,
// so tests pin it directly on both POSIX and Windows inputs.
func fileURIPath(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// dsnForPath builds the sqlite file: DSN with the pragmas the store
// depends on: WAL so readers never block the single writer, a bounded
// busy timeout, foreign keys, immediate write transactions, and NORMAL
// synchronous (acknowledged events survive a process crash; a power
// loss may drop the last frames — see Designv5 §5).
func dsnForPath(path string) (string, error) {
	// Absolutize here too, so the helper is total: openStore already
	// passes an absolute path, but a relative input must resolve
	// against the working directory — never the filesystem root.
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	u := &url.URL{Scheme: "file", Path: fileURIPath(abs)}
	q := u.Query()
	q.Set("_journal_mode", "WAL")
	q.Set("_busy_timeout", "5000")
	q.Set("_foreign_keys", "on")
	q.Set("_txlock", "immediate")
	q.Set("_synchronous", "NORMAL")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Open creates (0600 file in a 0700 directory) or opens the database at path,
// runs migrations, and starts the writer goroutine.
func Open(path string, opts ...Option) (*Store, error) { return openStore(path, opts...) }

func openStore(path string, opts ...Option) (*Store, error) {
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
	// A directory other users can write to is refused: raw payloads
	// live here, and a writable directory admits planted symlinks and
	// replacement files. The sticky bit (e.g. /tmp) still counts as
	// safe — only the owner can rename inside it. A directory this call
	// created is 0700 by construction and passes.
	// Windows reports synthesized modes with no sticky bit, and its
	// permission bits are meaningless anyway, so the check is Unix-only.
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("stat data directory: %w", err)
		} else if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return nil, fmt.Errorf("refusing group/world-writable data directory %s without the sticky bit", dir)
		}
	}
	// Lstat first: a planted symlink must never be followed — payloads
	// would land in (or be read from) the link target. O_EXCL closes
	// the create race; the residual Lstat-to-Chmod window needs
	// directory write access, which the check above refuses.
	// (O_NOFOLLOW would close it fully but does not compile on the
	// Windows build target, so this is the portable best.)
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing symlinked database file %s", path)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("tighten database permissions: %w", err)
		}
		for _, sidecar := range []string{path + "-wal", path + "-shm"} {
			if sfi, serr := os.Lstat(sidecar); serr == nil {
				if sfi.Mode()&os.ModeSymlink != 0 {
					return nil, fmt.Errorf("refusing symlinked sidecar %s", sidecar)
				}
				if err := os.Chmod(sidecar, 0o600); err != nil {
					return nil, fmt.Errorf("tighten sidecar permissions: %w", err)
				}
			} else if !os.IsNotExist(serr) {
				return nil, fmt.Errorf("stat sidecar: %w", serr)
			}
		}
	} else if os.IsNotExist(err) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create database file: %w", err)
		}
		f.Close()
		// Fresh file, but the sidecars may predate it in a sticky
		// directory: a planted symlink at either path is refused before
		// SQLite ever opens it.
		for _, sidecar := range []string{path + "-wal", path + "-shm"} {
			if sfi, serr := os.Lstat(sidecar); serr == nil {
				if sfi.Mode()&os.ModeSymlink != 0 {
					return nil, fmt.Errorf("refusing symlinked sidecar %s", sidecar)
				}
			} else if !os.IsNotExist(serr) {
				return nil, fmt.Errorf("stat sidecar: %w", serr)
			}
		}
	} else {
		return nil, fmt.Errorf("stat database file: %w", err)
	}
	// The normal DSN switches SQLite to WAL on its first connection. Refuse
	// unsupported databases through a read-only connection first, so even
	// the journal header is not rewritten before the refusal.
	readURI := &url.URL{Scheme: "file", Path: fileURIPath(path)}
	readQuery := readURI.Query()
	readQuery.Set("mode", "ro")
	readQuery.Set("_busy_timeout", "5000")
	readURI.RawQuery = readQuery.Encode()
	preflight, err := sql.Open("sqlite3", readURI.String())
	if err != nil {
		return nil, fmt.Errorf("open database for schema check: %w", err)
	}
	_, checkErr := validateSchemaLineage(preflight)
	closeErr := preflight.Close()
	if checkErr != nil {
		return nil, fmt.Errorf("check database schema: %w", checkErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close schema check: %w", closeErr)
	}
	dsn, err := dsnForPath(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dsn)
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
	for job := range s.jobs {
		select {
		case <-s.quit:
			job.done <- ErrStoreClosed
		default:
			job.done <- s.runJob(job)
		}
	}
}

func (s *Store) runJob(job writeJob) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// Type only: the recovered value can carry payload text
			// (job closures close over event data), which must never
			// surface in an error the worker logs.
			err = fmt.Errorf("store write panicked: %T", r)
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
	return s.enqueue(writeJob{ctx: ctx, fn: fn, done: make(chan error, 1)})
}

// enqueue waits for the outcome of an admitted job. A full queue fails
// fast so ingest can answer 503 instead of waiting for queue space.
func (s *Store) enqueue(job writeJob) error {
	if err := s.submit(job); err != nil {
		return err
	}
	select {
	case <-job.ctx.Done():
		return job.ctx.Err()
	case err := <-job.done:
		return err
	}
}

func (s *Store) submit(job writeJob) error {
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	select {
	case <-s.quit:
		return ErrStoreClosed
	default:
	}
	if err := job.ctx.Err(); err != nil {
		return err
	}
	select {
	case s.jobs <- job:
		return nil
	default:
		return ErrQueueSaturated
	}
}

// Close shuts the store down once and reports the result to every caller.
// Concurrent callers wait for the same shutdown result while the writer
// rejects queued jobs and finishes any in-flight work.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.submitMu.Lock()
		close(s.quit)
		close(s.jobs)
		s.submitMu.Unlock()

		s.wg.Wait()
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}
