package forwarder

import (
	"context"
	"testing"
	"time"
)

func TestGateSingleFlightAndRelease(t *testing.T) {
	g := NewGate()
	release, wait := g.Acquire("evt-1")
	if release == nil || wait != nil {
		t.Fatal("first acquire must take the gate")
	}
	if _, wait := g.Acquire("evt-1"); wait == nil {
		t.Fatal("second concurrent acquire must return a wait channel")
	}
	release()
	release() // idempotent: must not block or panic
	if g.InFlight() != 0 {
		t.Fatalf("entry must be removed on release, in-flight=%d", g.InFlight())
	}
	if _, wait := g.Acquire("evt-1"); wait != nil {
		t.Fatal("acquire after release must take the gate again")
	}
}

func TestGateWaiterWakesOnRelease(t *testing.T) {
	g := NewGate()
	release, _ := g.Acquire("evt-2")
	_, wait := g.Acquire("evt-2")
	done := make(chan struct{})
	go func() {
		<-wait
		close(done)
	}()
	release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter must wake when the holder releases")
	}
	if g.InFlight() != 0 {
		t.Fatalf("leaked gate entry: %d", g.InFlight())
	}
}

func TestGateDoWaiterSkipsFnAndPanicReleases(t *testing.T) {
	g := NewGate()
	release, _ := g.Acquire("evt-x")
	ctx, cancel := context.WithTimeout(context.Background(), 50*1000000)
	waited, err := g.Do(ctx, "evt-x", func() error {
		t.Fatal("waiter must not run fn")
		return nil
	})
	cancel()
	if !waited || err == nil {
		t.Fatalf("expected waited duplicate with ctx error, got waited=%v err=%v", waited, err)
	}
	release()

	func() {
		defer func() { _ = recover() }()
		_, _ = g.Do(context.Background(), "evt-y", func() error { panic("boom") })
	}()
	if g.InFlight() != 0 {
		t.Fatalf("panic must still release the gate, in-flight=%d", g.InFlight())
	}
}
