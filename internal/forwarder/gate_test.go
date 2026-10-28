package forwarder

import "testing"

func TestGateSingleFlightAndRelease(t *testing.T) {
	g := NewGate()
	release, ok := g.Acquire("evt-1")
	if !ok {
		t.Fatal("first acquire must succeed")
	}
	if _, ok := g.Acquire("evt-1"); ok {
		t.Fatal("second concurrent acquire must fail")
	}
	release()
	release() // idempotent: must not block or panic
	if g.InFlight() != 0 {
		t.Fatalf("entry must be removed on release, in-flight=%d", g.InFlight())
	}
	if _, ok := g.Acquire("evt-1"); !ok {
		t.Fatal("acquire after release must succeed")
	}
}
