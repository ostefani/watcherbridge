// Package forwarder owns proxy forwarding helpers. Gate prevents duplicate
// in-flight forwards of the same event within one process (two bridge
// instances against one database are not supported).
package forwarder

import (
	"context"
	"sync"
)

// Gate is a per-event in-flight lock. The holder releases exactly once;
// duplicates wait on a channel and then follow the stored forwarded state.
//
// Call order (the gate cannot enforce this; receivers must):
//  1. Acquire right after signature verification, before persisting.
//  2. Release only after the forward outcome (MarkForwarded / failure) is
//     committed, so a waking duplicate reads the final state.
//  3. A duplicate that waited must not forward itself; it only reads state
//     and replies (202 when forwarded, 502 otherwise).
type Gate struct {
	mu sync.Mutex
	in map[string]chan struct{}
}

func NewGate() *Gate { return &Gate{in: map[string]chan struct{}{}} }

// Acquire either takes the gate (release != nil) or returns a channel that
// closes when the current holder releases (wait != nil). Callers must
// reject empty event IDs before acquiring.
func (g *Gate) Acquire(eventID string) (release func(), wait <-chan struct{}) {
	g.mu.Lock()
	if ch, held := g.in[eventID]; held {
		g.mu.Unlock()
		return nil, ch
	}
	ch := make(chan struct{})
	g.in[eventID] = ch
	g.mu.Unlock()
	return sync.OnceFunc(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		delete(g.in, eventID)
		close(ch)
	}), nil
}

// InFlight reports how many events are currently gated (tests/metrics).
// After any completed flow it must read zero; anything else is a leak.
func (g *Gate) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.in)
}

// Do runs fn while holding the gate for eventID, releasing on every path —
// including panic, which is re-thrown after release. A duplicate does not
// run fn: it waits for the holder (or ctx) and reports waited=true so the
// caller can follow the stored state instead. Prefer Do over Acquire; use
// Acquire only where the release point must span function boundaries, and
// then defer it immediately:
//
//	release, wait := gate.Acquire(id)
//	if release != nil { defer release() }
func (g *Gate) Do(ctx context.Context, eventID string, fn func() error) (waited bool, err error) {
	release, wait := g.Acquire(eventID)
	if wait != nil {
		select {
		case <-wait:
			return true, nil
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}
	defer release()
	return false, fn()
}
