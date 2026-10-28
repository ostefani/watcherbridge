package server

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

// sweep retries unforwarded events that are still inside the freshness window
// and terminally marks older ones, on one worker with bounded batches. It
// shares the forwarder gate with live handlers: a swept event under an
// in-flight forward is skipped (the holder owns it).
func (s *Server) sweep(ctx context.Context) {
	if _, err := s.events.MarkStaleSweep(ctx, s.now(), s.cfg.TimestampTolerance, s.cfg.SweepSafetyMargin); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		batch, err := s.events.SweepCandidates(ctx, s.now(), s.cfg.TimestampTolerance, s.cfg.SweepSafetyMargin, s.cfg.SweepBatch)
		if err != nil || len(batch) == 0 {
			return
		}
		owned := 0
		for _, ev := range batch {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if s.sweepOne(ctx, ev) {
				owned++
			}
		}
		if owned == 0 {
			// Every candidate is owned by a live handler; spinning
			// would burn CPU without progress. Their outcomes decide.
			return
		}
	}
}

// sweepOne forwards a single swept row. Freshness is re-checked immediately
// before the attempt from the row's own signature timestamp, not only when
// the batch was selected; the forwarded flag is re-checked too, so an event
// delivered since selection is skipped. It reports whether the sweeper owned
// the event (false when a live handler holds the gate).
func (s *Server) sweepOne(ctx context.Context, ev store.RawEvent) bool {
	release, wait := s.gate.Acquire(ev.EventID)
	if wait != nil {
		return false // a live handler owns this event; its outcome decides
	}
	defer release()

	if s.now().Sub(ev.SignatureTimestamp) >= s.cfg.TimestampTolerance-s.cfg.SweepSafetyMargin {
		return true
	}
	if fwd, _, _, err := s.events.ForwardState(ctx, ev.EventID); err != nil || fwd {
		return true
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(ev.HeadersJSON), &headers); err != nil {
		return true
	}
	s.forwardOne(ctx, ev.Payload, headers, ev.EventID)
	return true
}

// SweepOnce runs one bounded sweep pass synchronously. Tests use it to avoid
// timing flakes; Start uses the background loop.
func (s *Server) SweepOnce(ctx context.Context) { s.sweep(ctx) }

// SetNow overrides the clock (tests only).
func (s *Server) SetNow(fn func() time.Time) { s.now = fn }
