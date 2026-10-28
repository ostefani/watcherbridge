package briefing

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

// fallbackReason is the exact Designv5 §7.3 string for model-less briefings.
const fallbackReason = "Rule-based fallback: model unavailable."

// Deps wires the worker. Tick and Lease come from config (GROUPER_TICK,
// PROCESSING_LEASE); Policy carries the baseline knobs; Now is injectable
// for tests.
type Deps struct {
	Store  *store.Store
	Groups store.GroupPolicy
	Tick   time.Duration
	Lease  time.Duration
	Now    func() time.Time
	Policy Policy
}

// Run ticks until ctx ends: close due groups, reap stale leases, then drain
// the briefing queue page by page. It returns nil when ctx ends; per-group
// panics are recovered so one poison group cannot stop the worker.
func Run(ctx context.Context, d Deps) error {
	if d.Store == nil {
		return fmt.Errorf("briefing worker requires a store")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	tick := d.Tick
	if tick <= 0 {
		tick = time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		drainOnce(ctx, d, now())
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		timer.Reset(tick)
	}
}

func drainOnce(ctx context.Context, d Deps, now time.Time) {
	if _, err := d.Store.CloseDue(ctx, now); err != nil {
		log.Printf("briefing: close-due: %s", store.SanitizeErrorText(err.Error()))
	}
	if _, err := d.Store.ReleaseStale(ctx, now, d.Lease); err != nil {
		log.Printf("briefing: release-stale: %s", store.SanitizeErrorText(err.Error()))
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		ids, err := d.Store.NeedsBriefing(ctx, store.DefaultBriefingPage)
		if err != nil {
			log.Printf("briefing: needs-briefing: %s", store.SanitizeErrorText(err.Error()))
			return
		}
		if len(ids) == 0 {
			return
		}
		for _, id := range ids {
			select {
			case <-ctx.Done():
				return
			default:
			}
			briefOne(ctx, d, id)
		}
	}
}

// briefOne resolves exactly one group. A nil return means the group reached
// a terminal state for this pass (briefed, requeued, or parked failed);
// errors are logged with the group ID only, never payload values.
func briefOne(ctx context.Context, d Deps, groupID string) {
	defer func() {
		if r := recover(); r != nil {
			// A panic is a failed attempt, not a crash: the process keeps
			// running. The recovered value may carry payload text, so only
			// the group ID and the stack are logged.
			log.Printf("briefing: worker panic on group %s\n%s", groupID, debug.Stack())
			parkFailed(ctx, d, groupID, "worker panic")
		}
	}()
	taken, err := d.Store.TakeGroup(ctx, groupID)
	if err != nil {
		return // lost race (already taken/briefed) or store hiccup; next tick retries
	}
	outcome, berr := Baseline(decodeTaken(taken), asOf(taken), d.Policy)
	if berr != nil {
		// Empty group: nothing to resolve. Retry while takes remain; the
		// cap parks it failed rather than inventing a priority.
		log.Printf("briefing: group %s has no events", groupID)
		releaseOrPark(ctx, d, taken, "no events")
		return
	}
	final, clamped := Resolve(outcome.Level, nil) // no provider in Step 9
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	b := store.Briefing{
		BriefingID:       "brf-" + taken.GroupID,
		GroupID:          taken.GroupID,
		HostAnchorKey:    taken.Anchor,
		Baseline:         outcome.Level.String(),
		ModelSuggestion:  nil,
		Priority:         final.String(),
		PriorityReason:   fallbackReason,
		BaselineRule:     outcome.RuleID,
		Clamped:          clamped,
		FallbackUsed:     true,
		WhatChanged:      describeEvents(taken),
		Context:          "provider unavailable; briefing from deterministic baseline only",
		RecommendedCheck: "Review the recent changes on this host.",
		CreatedAt:        now,
	}
	if taken.LastAttempt() {
		// Final attempt: write the deterministic briefing inline instead
		// of releasing, so a poison group converges to done, not failed.
		if err := d.Store.InsertBriefing(ctx, b); err != nil {
			log.Printf("briefing: group %s final insert: %s", groupID, store.SanitizeErrorText(err.Error()))
			parkFailed(ctx, d, groupID, "final insert failed")
		}
		return
	}
	if err := d.Store.InsertBriefing(ctx, b); err != nil {
		log.Printf("briefing: group %s insert: %s", groupID, store.SanitizeErrorText(err.Error()))
		releaseOrPark(ctx, d, taken, "insert failed")
		return
	}
	if len(outcome.Skipped) > 0 {
		log.Printf("briefing: group %s baseline %s skipped %d unknown-input rules", groupID, outcome.RuleID, len(outcome.Skipped))
	}
}

// releaseOrPark requeues for another attempt, or parks failed at the cap.
// Failed groups carry no invented priority; the log line is the record.
func releaseOrPark(ctx context.Context, d Deps, taken store.TakenGroup, reason string) {
	matched, err := d.Store.ReleaseGroup(ctx, taken.GroupID)
	if err != nil {
		log.Printf("briefing: group %s release: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
		return
	}
	if !matched {
		return
	}
	if st, _ := d.Store.GroupStatus(ctx, taken.GroupID); st == "failed" {
		log.Printf("briefing: group %s parked failed (%s) after %d attempts", taken.GroupID, reason, taken.RetryCount)
	}
}

// parkFailed moves a processing group straight to failed with a sanitized
// fixed reason. Used for worker-side faults (panics), never for baseline
// content — baseline has its own unknown-input path.
func parkFailed(ctx context.Context, d Deps, groupID, reason string) {
	// Drain remaining takes to the cap through releases; ReleaseGroup
	// parks failed once retry_count reaches it.
	for i := 0; i < 8; i++ {
		matched, err := d.Store.ReleaseGroup(ctx, groupID)
		if err != nil || !matched {
			return
		}
		if st, _ := d.Store.GroupStatus(ctx, groupID); st == "failed" {
			log.Printf("briefing: group %s parked failed (%s)", groupID, reason)
			return
		}
		// Re-take to keep counting attempts through TakeGroup.
		if _, err := d.Store.TakeGroup(ctx, groupID); err != nil {
			return
		}
	}
}

func decodeTaken(taken store.TakenGroup) []GroupEvent {
	out := make([]GroupEvent, 0, len(taken.Events))
	for _, ev := range taken.Events {
		out = append(out, DecodeEvent(ev.EventType, ev.Payload))
	}
	return out
}

// asOf is the group's last event time, never wall clock: the same group
// resolves identically no matter when the worker runs.
func asOf(taken store.TakenGroup) time.Time {
	var last time.Time
	for _, ev := range taken.Events {
		t := ev.OccurredAt
		if t.IsZero() {
			t = ev.ReceivedAt
		}
		if t.After(last) {
			last = t
		}
	}
	if last.IsZero() {
		last = taken.OpenedAt
	}
	return last
}

// describeEvents renders fixed templates plus event-type counts (enum
// values only) — no payload free text, per the Step 9 scope.
func describeEvents(taken store.TakenGroup) string {
	counts := map[string]int{}
	order := []string{}
	for _, ev := range taken.Events {
		if _, ok := counts[ev.EventType]; !ok {
			order = append(order, ev.EventType)
		}
		counts[ev.EventType]++
	}
	parts := ""
	for i, typ := range order {
		if i > 0 {
			parts += ", "
		}
		parts += fmt.Sprintf("%d %s", counts[typ], typ)
	}
	return fmt.Sprintf("%d events in group: %s", len(taken.Events), parts)
}
