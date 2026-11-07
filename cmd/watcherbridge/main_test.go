package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/config"
	"github.com/ostefani/watcherbridge/internal/store"
)

func TestRunCancelsStartupSweep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relay := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		cancel()
		<-r.Context().Done()
	}))
	defer relay.Close()

	cfg := config.Default()
	cfg.Mode = config.ModeProxy
	cfg.Secret = "synthetic-secret-for-startup-cancellation"
	cfg.ScanRelayURL = relay.URL
	db := filepath.Join(t.TempDir(), "bridge.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_, seedErr := st.PersistEventAndGroup(ctx, store.RawEvent{
		EventID: "abcd", EventKey: "startup-sweep", EventType: "host.appeared",
		OccurredAt: now, ReceivedAt: now, SignatureTimestamp: now,
		Payload: []byte("{}"), HeadersJSON: "{}",
	}, "startup-sweep", store.GroupPolicy{QuietWindow: time.Minute})
	if err := errors.Join(seedErr, st.Close()); err != nil {
		t.Fatal(err)
	}

	if err := run(ctx, cfg, db); err != nil {
		t.Fatalf("cancelled startup: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("relay did not cancel startup")
	}

	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.LatestPurgeRun(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("startup purge should not run after cancellation: %v", err)
	}
}

func TestRunReturnsOnBindFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	cfg := config.Default()
	cfg.Bind = listener.Addr().String()
	cfg.Mode = config.ModeTap
	cfg.Secret = "synthetic-secret-for-bind-failure"
	cfg.ScanRelayURL = ""
	cfg.ModelProvider = "fake"

	// This is a safety deadline for the test. A failed listener should
	// cancel its own workers and return while the parent is still live.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = run(ctx, cfg, filepath.Join(t.TempDir(), "bridge.db"))
	if !errors.Is(err, syscall.EADDRINUSE) || ctx.Err() != nil {
		t.Fatalf("run = %v, parent = %v; want bind failure before parent cancellation", err, ctx.Err())
	}
}
