// Package forwarder owns proxy forwarding helpers. Gate prevents duplicate
// in-flight forwards of the same event within one process.
package forwarder

import "sync"

// Gate is a per-event in-flight lock. Entries are removed on release, and
// release is idempotent.
type Gate struct {
	mu sync.Mutex
	in map[string]struct{}
}

func NewGate() *Gate { return &Gate{in: map[string]struct{}{}} }

// Acquire takes the gate for eventID. When ok is false another forward is
// already in flight and the caller must not forward again.
func (g *Gate) Acquire(eventID string) (release func(), ok bool) {
	g.mu.Lock()
	if _, held := g.in[eventID]; held {
		g.mu.Unlock()
		return nil, false
	}
	g.in[eventID] = struct{}{}
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.in, eventID)
			g.mu.Unlock()
		})
	}, true
}

// InFlight reports how many events are currently gated (tests/metrics).
func (g *Gate) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.in)
}
