package viewer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/overview"
	"github.com/ostefani/watcherbridge/internal/store"
)

func TestHeartbeatCoverageAgesWithoutEvents(t *testing.T) {
	s := openViewerStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	r := NewRefresher(s, nil, nil, 24*time.Hour, 168*time.Hour, 30*24*time.Hour)
	r.now = func() time.Time { return now }
	if err := s.BindSourceIdentity(ctx, "lab", "ns-1", store.StableIdentityCapability); err != nil {
		t.Fatal(err)
	}
	cov, wires, err := r.coverage(ctx)
	if err != nil || cov.Status != "ok" || wires[0].Liveness != "unknown" {
		t.Fatalf("pre-H1 coverage: %+v %+v %v", cov, wires, err)
	}
	insert := func(id int, status string, completed, received, signed time.Time) {
		t.Helper()
		issues := int64(0)
		if status != "ok" {
			issues = 1
		}
		_, err := s.InsertHeartbeat(ctx, store.Heartbeat{HeartbeatSummary: store.HeartbeatSummary{
			ScanID: fmt.Sprintf("%032x", id), Target: "192.0.2.0/24", CompletedAt: completed, ReceivedAt: received, Status: status, IssueCount: issues,
		}, SourceScope: "lab", SignatureAt: signed, Payload: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
	}
	first := now
	insert(1, "ok", first, first, first)
	cov, wires, err = r.coverage(ctx)
	if err != nil || cov.Status != "ok" || wires[0].Liveness != "reporting" || wires[0].Heartbeat == nil {
		t.Fatalf("fresh: %+v %+v %v", cov, wires, err)
	}
	if wires[0].IdentityNamespace != "ns-1" || wires[0].Capabilities != store.StableIdentityCapability {
		t.Fatal("H1 changed identity binding")
	}
	now = first.Add(10*time.Minute - time.Nanosecond)
	cov, _, err = r.coverage(ctx)
	if err != nil || cov.Status != "ok" {
		t.Fatalf("early stale transition: %+v %v", cov, err)
	}
	now = first.Add(10 * time.Minute)
	cov, wires, err = r.coverage(ctx)
	if err != nil || cov.Status != "blind" || wires[0].Liveness != "stale" {
		t.Fatalf("not stale at boundary: %+v %+v %v", cov, wires, err)
	}
	// A new signature on the old scan cannot revive it, even after history expires.
	insert(1, "ok", first, now, now)
	cov, _, err = r.coverage(ctx)
	if err != nil || cov.Status != "blind" {
		t.Fatalf("duplicate revived source: %+v %v", cov, err)
	}
	// Fresh partial and failed scans prove progress but not full coverage.
	for i, status := range []string{"partial", "error", "ok"} {
		now = now.Add(time.Minute)
		insert(i+2, status, now, now, now)
		cov, wires, err = r.coverage(ctx)
		want := "degraded"
		if status == "ok" {
			want = "ok"
		}
		if err != nil || cov.Status != want || wires[0].Liveness != "reporting" {
			t.Fatalf("%s: %+v %+v %v", status, cov, wires, err)
		}
	}
	// A second source's fresh scan cannot hide the first source's stale scan.
	oldSource := store.Heartbeat{HeartbeatSummary: store.HeartbeatSummary{ScanID: fmt.Sprintf("%032x", 99), Target: "198.51.100.0/24", CompletedAt: first, ReceivedAt: first, Status: "ok"}, SourceScope: "other", SignatureAt: first, Payload: []byte(`{}`)}
	if _, err := s.InsertHeartbeat(ctx, oldSource); err != nil {
		t.Fatal(err)
	}
	cov, _, err = r.coverage(ctx)
	if err != nil || cov.Status != overview.CoverageBlind {
		t.Fatalf("fresh source masked stale one: %+v %v", cov, err)
	}
}
