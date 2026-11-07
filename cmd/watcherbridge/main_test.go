package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
