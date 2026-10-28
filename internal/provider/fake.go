package provider

import (
	"context"
	"sync"
	"time"
)

// FakeStep is one scripted answer. Hang blocks until ctx ends (timeout
// path); otherwise JSON or Err is returned immediately.
type FakeStep struct {
	JSON []byte
	Err  error
	Hang bool
}

// FakeAdapter is the test provider. It records every call (bytes received,
// ctx deadline) so tests assert what crossed the trust boundary and that
// both attempts shared one timeout budget.
type FakeAdapter struct {
	mu     sync.Mutex
	script []FakeStep
	// Handle, when non-nil, answers dynamically instead of the script:
	// repair reports a correction attempt, payload is the bundle (or the
	// correction text), issued is the bundle's token map — everything a
	// test needs to echo issued tokens back.
	Handle    func(repair bool, payload []byte, issued map[string]string) (RawBriefing, error)
	Briefs    int
	Repairs   int
	Received  [][]byte
	Deadlines []time.Time // ctx deadline observed per call; zero means none
}

// NewFake returns a FakeAdapter playing script in order; extra calls replay
// the last step.
func NewFake(script ...FakeStep) *FakeAdapter {
	return &FakeAdapter{script: script}
}

// Name implements Provider.
func (f *FakeAdapter) Name() string { return "fake" }

// Brief implements Provider.
func (f *FakeAdapter) Brief(ctx context.Context, bundle []byte, issued map[string]string) (RawBriefing, error) {
	return f.next(ctx, false, bundle, issued)
}

// Repair implements Provider.
func (f *FakeAdapter) Repair(ctx context.Context, correction string, issued map[string]string) (RawBriefing, error) {
	return f.next(ctx, true, []byte(correction), issued)
}

func (f *FakeAdapter) next(ctx context.Context, repair bool, payload []byte, issued map[string]string) (RawBriefing, error) {
	f.mu.Lock()
	step := FakeStep{}
	if len(f.script) > 0 {
		step = f.script[0]
		if len(f.script) > 1 {
			f.script = f.script[1:]
		}
	}
	handle := f.Handle
	if repair {
		f.Repairs++
	} else {
		f.Briefs++
	}
	f.Received = append(f.Received, append([]byte(nil), payload...))
	if dl, ok := ctx.Deadline(); ok {
		f.Deadlines = append(f.Deadlines, dl)
	} else {
		f.Deadlines = append(f.Deadlines, time.Time{})
	}
	f.mu.Unlock()
	if handle != nil {
		return handle(repair, payload, issued)
	}
	if step.Hang {
		<-ctx.Done()
		return RawBriefing{}, ctx.Err()
	}
	if step.Err != nil {
		return RawBriefing{}, step.Err
	}
	return RawBriefing{JSON: append([]byte(nil), step.JSON...)}, nil
}
