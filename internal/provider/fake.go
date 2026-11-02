package provider

import (
	"context"
	"sync"
	"time"
)

// FakeStep is one scripted answer. Hang blocks until ctx ends (timeout
// path); otherwise JSON or Err is returned immediately. Usage scripts
// the adapter-reported accounting for the step; zero means the adapter
// supplied none.
type FakeStep struct {
	JSON  []byte
	Err   error
	Hang  bool
	Usage Usage
}

// FakeAdapter is the test provider. It records every call (bytes received,
// ctx deadline) so tests assert what crossed the trust boundary and that
// both attempts shared one timeout budget.
type FakeAdapter struct {
	mu     sync.Mutex
	script []FakeStep
	// Handle, when non-nil, answers dynamically instead of the script:
	// repair reports a correction attempt, payload is the bundle (or the
	// correction text), valid is the bundle's token-name set — everything
	// a test needs to echo tokens back, and nothing that maps to values.
	Handle    func(repair bool, payload []byte, valid map[string]struct{}) (RawBriefing, error)
	Briefs    int
	Repairs   int
	Received  [][]byte
	Deadlines []time.Time // ctx deadline observed per call; zero means none
	// Turns scripts the Turner path: one FakeStep per Turn call, JSON as
	// the assistant text. Extra calls replay the last step, like Brief.
	Turns        []FakeStep
	TurnCalls    int
	TurnReceived [][]TurnMessage
	// TurnHandle, when non-nil, answers dynamically from the full
	// transcript: tests parse the bundle out of transcript[1] and
	// build token-echoing answers without hardcoding token numbers.
	TurnHandle func(transcript []TurnMessage) (string, error)
}

// NewFake returns a FakeAdapter playing script in order; extra calls replay
// the last step.
func NewFake(script ...FakeStep) *FakeAdapter {
	return &FakeAdapter{script: script}
}

// Name implements Provider.
func (f *FakeAdapter) Name() string { return "fake" }

// Brief implements Provider.
func (f *FakeAdapter) Brief(ctx context.Context, bundle []byte, valid map[string]struct{}) (RawBriefing, error) {
	return f.next(ctx, false, bundle, valid)
}

// Repair implements Provider.
func (f *FakeAdapter) Repair(ctx context.Context, correction string, valid map[string]struct{}) (RawBriefing, error) {
	return f.next(ctx, true, []byte(correction), valid)
}

// Turn implements Turner for scripted agent tests: unknown/hostile
// turns, truncation signals, hangs, and errors are all scriptable. The
// dynamic handle answers text only (no scripted usage); scripted steps
// carry their own Usage.
func (f *FakeAdapter) Turn(ctx context.Context, transcript []TurnMessage) (TurnResult, error) {
	f.mu.Lock()
	step := FakeStep{}
	if len(f.Turns) > 0 {
		step = f.Turns[0]
		if len(f.Turns) > 1 {
			f.Turns = f.Turns[1:]
		}
	}
	f.TurnCalls++
	cp := make([]TurnMessage, len(transcript))
	copy(cp, transcript)
	f.TurnReceived = append(f.TurnReceived, cp)
	handle := f.TurnHandle
	if dl, ok := ctx.Deadline(); ok {
		f.Deadlines = append(f.Deadlines, dl)
	} else {
		f.Deadlines = append(f.Deadlines, time.Time{})
	}
	f.mu.Unlock()
	if handle != nil {
		text, err := handle(cp)
		return TurnResult{Text: text}, err
	}
	if step.Hang {
		<-ctx.Done()
		return TurnResult{}, ctx.Err()
	}
	if step.Err != nil {
		return TurnResult{}, step.Err
	}
	return TurnResult{Text: string(step.JSON), Usage: step.Usage}, nil
}

func (f *FakeAdapter) next(ctx context.Context, repair bool, payload []byte, valid map[string]struct{}) (RawBriefing, error) {
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
		return handle(repair, payload, valid)
	}
	if step.Hang {
		<-ctx.Done()
		return RawBriefing{}, ctx.Err()
	}
	if step.Err != nil {
		return RawBriefing{}, step.Err
	}
	return RawBriefing{JSON: append([]byte(nil), step.JSON...), Usage: step.Usage}, nil
}
