package server

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/ostefani/watcherbridge/internal/config"
	"github.com/ostefani/watcherbridge/internal/store"
)

// sweep retries unforwarded events that are still inside the freshness
// window and terminally marks older ones, on one worker with bounded
// batches. It shares the forwarder gate with live handlers: a swept event
// under an in-flight forward is skipped (the holder owns it). In tap mode
// there is nothing to re-forward — ScanRelay already got every event
// directly from Pro — so the sweep does not run at all.
func (s *Server) sweep(ctx context.Context) {
	if s.cfg.Mode != config.ModeProxy {
		return
	}
	if _, err := s.events.MarkStaleSweep(ctx, s.now(), s.cfg.TimestampTolerance, s.cfg.SweepSafetyMargin); err != nil {
		log.Printf("ingest: sweep mark-stale: %v", err)
		return
	}
	forwarded, undeliverable := 0, 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		batch, err := s.events.SweepCandidates(ctx, s.now(), s.cfg.TimestampTolerance, s.cfg.SweepSafetyMargin, s.cfg.SweepBatch)
		if err != nil {
			log.Printf("ingest: sweep select: %v", err)
			return
		}
		if len(batch) == 0 {
			break
		}
		progress := false
		for _, ev := range batch {
			select {
			case <-ctx.Done():
				return
			default:
			}
			done, landed := s.sweepOne(ctx, ev)
			progress = progress || done
			if landed {
				forwarded++
			} else if done {
				undeliverable++
			}
		}
		if !progress {
			// Every remaining row is gated or failing: retrying them in
			// a tight loop only burns CPU and increments attempts.
			// Live retries (Pro's outbox) and the next restart cover
			// what is left; rows age out via MarkStaleSweep.
			break
		}
	}
	if forwarded+undeliverable > 0 {
		log.Printf("ingest: sweep done: forwarded=%d terminal=%d", forwarded, undeliverable)
	}
}

// sweepOne forwards a single swept row. Freshness is re-checked immediately
// before the attempt from the row's own signature timestamp, not only when
// the batch was selected; the forwarded flag is re-checked too, so an event
// delivered since selection is skipped. It reports done=true when the row
// needs no further work (forwarded, or terminally marked) and landed=true
// when this call delivered it. done=false means gated or failed —
// retrying it immediately would make no progress.
func (s *Server) sweepOne(ctx context.Context, ev store.RawEvent) (done, landed bool) {
	release, wait := s.gate.Acquire(ev.EventID)
	if wait != nil {
		return false, false // a live handler owns this event; its outcome decides
	}
	defer release()

	if s.now().Sub(ev.SignatureTimestamp) >= s.cfg.TimestampTolerance-s.cfg.SweepSafetyMargin {
		return s.park(ctx, ev.EventID, "stale_for_sweep"), false
	}
	if fwd, _, _, err := s.events.ForwardState(ctx, ev.EventID); err != nil || fwd {
		return true, false
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(ev.HeadersJSON), &headers); err != nil {
		return s.park(ctx, ev.EventID, "undeliverable_headers"), false
	}
	if s.forwardOne(ctx, ev.Payload, headers, ev.EventID) {
		return true, true
	}
	return false, false
}

// park marks a row terminal so the sweep never selects it again.
func (s *Server) park(ctx context.Context, eventID, reason string) bool {
	if err := s.events.MarkUndeliverable(ctx, eventID, reason); err != nil {
		log.Printf("ingest: park %s: %v", eventID, err)
		return false
	}
	return true
}

// SweepOnce finishes recovery forwarding before the startup purge and listener.
func (s *Server) SweepOnce(ctx context.Context) { s.sweep(ctx) }

// SetNow overrides the clock (tests only).
func (s *Server) SetNow(fn func() time.Time) { s.now = fn }
