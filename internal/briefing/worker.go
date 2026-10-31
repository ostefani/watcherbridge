package briefing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ostefani/watcherbridge/internal/provider"
	"github.com/ostefani/watcherbridge/internal/redactor"
	"github.com/ostefani/watcherbridge/internal/store"
)

// fallbackReason is the exact Designv5 §7.3 string for model-less briefings.
const fallbackReason = "Rule-based fallback: model unavailable."

// Deps wires the worker. Tick and Lease come from config (GROUPER_TICK,
// PROCESSING_LEASE); Policy carries the baseline knobs; Now is injectable
// for tests. Provider is nil in Step 9 scope (nil means the deterministic
// minimal briefing); Step 11 sets it, plus the redaction policy and the
// shared provider timeout.
type Deps struct {
	Store  *store.Store
	Groups store.GroupPolicy
	Tick   time.Duration
	Lease  time.Duration
	Now    func() time.Time
	Policy Policy
	// SourceScope identifies the authenticated scanner deployment for
	// reported-context projection. Empty skips projection (tests that
	// do not exercise it); production always sets it from config.
	SourceScope string

	Provider     provider.Provider
	Redaction    redactor.Policy
	ModelTimeout time.Duration

	// OnBriefing fires after each stored briefing (model or fallback)
	// with the exact row served and published. OnGroups fires once per
	// tick with the open-group snapshot. Both run on the worker
	// goroutine and must never block: the viewer hub broadcast and the
	// publisher enqueue drop rather than wait. Nil means no observer.
	OnBriefing func(store.Briefing)
	OnGroups   func([]store.GroupSnapshot)
}

// notifyBriefing fans a stored briefing out to the observer. Event IDs
// come from the taken events (the insert already proved them equal to
// the derived set), sorted to match the stored order. The writer assigns
// seq inside the insert transaction, so the stored row is re-read for the
// cursor the SSE id: field carries; on a read error the fanout still runs.
func notifyBriefing(d Deps, taken store.TakenGroup, b store.Briefing) {
	if d.OnBriefing == nil {
		return
	}
	ids := make([]string, 0, len(taken.Events))
	for _, ev := range taken.Events {
		ids = append(ids, ev.EventID)
	}
	sort.Strings(ids)
	b.EventIDs = ids
	if stored, err := d.Store.BriefingForGroup(context.Background(), taken.GroupID); err == nil {
		b.Seq = stored.Seq
	} else {
		log.Printf("briefing: group %s reread for seq: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
	}
	d.OnBriefing(b)
}

// Run ticks until ctx ends: close due groups, reap stale leases, then drain
// the briefing queue page by page. It returns nil when ctx ends; per-group
// panics are recovered so one poison group cannot stop the worker.
func Run(ctx context.Context, d Deps) error {
	if d.Store == nil {
		return fmt.Errorf("briefing worker requires a store")
	}
	// Operator-chosen degradations are loud at startup, never silent.
	if d.Provider == nil || d.Provider.Name() == "fake" {
		log.Print("briefing: no model provider; all briefings will be deterministic fallbacks")
	}
	if !d.Redaction.Enabled {
		log.Print("briefing: WARNING: redaction is off; raw values go to the provider and the leak scan is bypassed")
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
		// Reset only after consuming the tick: the timer fired above,
		// so Reset cannot race a pending delivery.
		timer.Reset(tick)
	}
}

// publishGroups emits the current open/processing snapshot. It runs both
// before the briefing drain (so closures show promptly) and after it (so
// the closed→processing transitions TakeGroup committed mid-drain reach
// subscribers in the same tick instead of vanishing until the next one).
func publishGroups(ctx context.Context, d Deps) {
	if d.OnGroups == nil {
		return
	}
	groups, err := d.Store.OpenGroupSnapshots(ctx)
	if err != nil {
		log.Printf("briefing: group snapshot: %s", store.SanitizeErrorText(err.Error()))
		return
	}
	d.OnGroups(groups)
}

func drainOnce(ctx context.Context, d Deps, now time.Time) {
	// Project reported context before closing anything: briefing
	// inserts copy group_subjects, so subjects must already be linked
	// when a group is taken. Bounded per tick; leftovers resume next
	// tick or after a restart via the progress table.
	if d.SourceScope != "" {
		if _, err := d.Store.ProjectReported(ctx, d.SourceScope, 256); err != nil {
			log.Printf("briefing: project reported: %s", store.SanitizeErrorText(err.Error()))
		}
	}
	if _, err := d.Store.CloseDue(ctx, now); err != nil {
		log.Printf("briefing: close-due: %s", store.SanitizeErrorText(err.Error()))
	}
	if _, err := d.Store.ReleaseStale(ctx, now, d.Lease); err != nil {
		log.Printf("briefing: release-stale: %s", store.SanitizeErrorText(err.Error()))
	}
	publishGroups(ctx, d)
	drained := false
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		ids, err := d.Store.NeedsBriefing(ctx, store.DefaultBriefingPage, now)
		if err != nil {
			log.Printf("briefing: needs-briefing: %s", store.SanitizeErrorText(err.Error()))
			return
		}
		if len(ids) == 0 {
			break
		}
		progress := false
		for _, id := range ids {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if briefOne(ctx, d, id) {
				progress = true
			}
		}
		drained = drained || progress
		if !progress {
			// A full page changed no group state (lost races, store
			// hiccups): re-reading would spin on the same rows until
			// the next tick, so end the pass here.
			break
		}
	}
	if drained {
		// The drain took groups closed→processing; republish so the
		// snapshot carries those transitions in this tick. Idle ticks
		// stay silent: the hash dedup downstream drops no-change
		// snapshots anyway, and one publish is enough.
		publishGroups(ctx, d)
	}
}

// releaseBackoff spaces retries after take n: 2s, 4s, 8s … capped at 30s.
// Attempts are cheap but provider calls and store writes are not; a fault
// that survives these delays is a poison group, and the retry cap then
// converges it to a deterministic fallback, then failed.
func releaseBackoff(retry int) time.Duration {
	d := 2 * time.Second << retry
	if d > 30*time.Second || d <= 0 {
		return 30 * time.Second
	}
	return d
}

// transientStoreErr reports failures the attempt should not pay for: the
// write never ran (saturated queue), the store is shutting down, or our
// context died first. Anything else (constraint violations, state
// mismatches) is a real outcome and counts against the retry budget.
func transientStoreErr(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, store.ErrQueueSaturated) || errors.Is(err, store.ErrStoreClosed) || ctx.Err() != nil
}

// briefOne resolves exactly one group and reports whether it changed
// group state. True means the group reached a terminal state for this
// pass (briefed, requeued, or parked failed); false means no attempt ran
// (lost race, store hiccup) and the pass should not spin on it.
// Errors are logged with the group ID only, never payload values.
func briefOne(ctx context.Context, d Deps, groupID string) (progressed bool) {
	defer func() {
		if r := recover(); r != nil {
			// A panic is a failed attempt, not a crash: the process keeps
			// running. The recovered value may carry payload text, so only
			// the group ID and the stack are logged.
			log.Printf("briefing: worker panic on group %s\n%s", groupID, debug.Stack())
			parkFailed(ctx, d, groupID, "worker panic")
			progressed = true
		}
	}()
	taken, err := d.Store.TakeGroup(ctx, groupID)
	if err != nil {
		if !errors.Is(err, store.ErrGroupNotClosed) && !errors.Is(err, store.ErrGroupNotReady) {
			log.Printf("briefing: group %s take: %s", groupID, store.SanitizeErrorText(err.Error()))
		}
		return false // lost race, unprojected group, or store hiccup; next tick retries
	}
	// The claim committed: publish before any provider work, so a group
	// that waits on the model is visible as processing (and the groups
	// queued closed behind it as waiting) for the whole call — not only
	// after the drain finishes, when the group is normally already done.
	publishGroups(ctx, d)
	outcome, berr := Baseline(decodeTaken(taken), asOf(taken), d.Policy)
	if berr != nil {
		// Empty group: nothing to resolve. Retry while takes remain; the
		// cap parks it failed rather than inventing a priority.
		log.Printf("briefing: group %s has no events", groupID)
		return releaseOrPark(ctx, d, taken, "no events")
	}
	if d.Provider != nil {
		return briefWithModel(ctx, d, taken, outcome)
	}
	// No provider in Step 9 scope: the deterministic minimal briefing.
	// Resolve(level, nil) is identity, which fallbackBriefing encodes.
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	b := fallbackBriefing(taken, outcome, now, "provider unavailable; briefing from deterministic baseline only")
	if taken.LastAttempt() {
		// Final attempt: write the deterministic briefing inline instead
		// of releasing, so a poison group converges to done, not failed.
		if err := d.Store.InsertBriefing(ctx, b); err != nil {
			log.Printf("briefing: group %s final insert: %s", groupID, store.SanitizeErrorText(err.Error()))
			if transientStoreErr(ctx, err) {
				return releaseTransient(ctx, d, taken)
			}
			parkFailed(ctx, d, groupID, "final insert failed")
			return true
		}
		notifyBriefing(d, taken, b)
		return true
	}
	if err := d.Store.InsertBriefing(ctx, b); err != nil {
		log.Printf("briefing: group %s insert: %s", groupID, store.SanitizeErrorText(err.Error()))
		if transientStoreErr(ctx, err) {
			return releaseTransient(ctx, d, taken)
		}
		return releaseOrPark(ctx, d, taken, "insert failed")
	}
	notifyBriefing(d, taken, b)
	if len(outcome.Skipped) > 0 {
		log.Printf("briefing: group %s baseline %s skipped %d unknown-input rules", groupID, outcome.RuleID, len(outcome.Skipped))
	}
	return true
}

// retryReleaseOnSaturated runs one release write until it succeeds,
// fails with anything but a saturated queue, or runs out of tries. A
// release that follows a saturated store write (the fallback path after
// a saturated insert) would otherwise fail in the same window, leaving
// the group processing until the lease reaps it without a refund — one
// burned take per incident. Only ErrQueueSaturated retries, bounded and
// ctx-aware, so a saturated window costs milliseconds, not budget.
func retryReleaseOnSaturated(ctx context.Context, attempt func(context.Context) (bool, error)) (bool, error) {
	const tries = 3
	const pause = 50 * time.Millisecond
	var matched bool
	var err error
	for i := 0; i < tries; i++ {
		matched, err = attempt(ctx)
		if !errors.Is(err, store.ErrQueueSaturated) || i == tries-1 {
			return matched, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(pause):
		}
	}
	return matched, err
}

// releaseOrPark requeues with a bounded backoff, or parks failed at the
// cap. Failed groups carry no invented priority; the log line is the
// record. Reports whether the group moved.
func releaseOrPark(ctx context.Context, d Deps, taken store.TakenGroup, reason string) bool {
	if ctx.Err() != nil {
		return false
	}
	matched, err := retryReleaseOnSaturated(ctx, func(ctx context.Context) (bool, error) {
		return d.Store.ReleaseGroupDelayed(ctx, taken.GroupID, releaseBackoff(taken.RetryCount))
	})
	if err != nil {
		log.Printf("briefing: group %s release: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
		return false
	}
	if !matched {
		return false
	}
	if st, _ := d.Store.GroupStatus(ctx, taken.GroupID); st == "failed" {
		log.Printf("briefing: group %s parked failed (%s) after %d attempts", taken.GroupID, reason, taken.RetryCount)
	}
	return true
}

// releaseTransient refunds one take and requeues with a backoff. The
// attempt never really ran (saturated writer, shutdown, dead context),
// so it must not burn the retry budget.
func releaseTransient(ctx context.Context, d Deps, taken store.TakenGroup) bool {
	// The backoff escalates on the refund count, not the take count: a
	// refund unhappens the take, so retry counts would retry flat out
	// (4s every cycle) and 20 refunds would span ~80s instead of the
	// ~10 minutes the cap budgets.
	matched, err := retryReleaseOnSaturated(ctx, func(ctx context.Context) (bool, error) {
		return d.Store.ReleaseGroupTransient(ctx, taken.GroupID, releaseBackoff(taken.TransientRefunds))
	})
	if err != nil {
		log.Printf("briefing: group %s transient release: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
		return false
	}
	if !matched {
		return false
	}
	if st, _ := d.Store.GroupStatus(ctx, taken.GroupID); st == "failed" {
		log.Printf("briefing: group %s parked failed (transient refund cap) after %d refunds", taken.GroupID, taken.TransientRefunds+1)
	}
	return true
}

// parkFailed moves a processing group straight to failed with a sanitized
// fixed reason. Used for worker-side faults (panics), never for baseline
// content — baseline has its own unknown-input path.
func parkFailed(ctx context.Context, d Deps, groupID, reason string) bool {
	if ctx.Err() != nil {
		return false
	}
	// Drain remaining takes to the cap through releases; ReleaseGroup
	// parks failed once retry_count reaches it. Immediate releases (no
	// backoff): the group is converging to a terminal state, not retrying.
	for i := 0; i < 8; i++ {
		matched, err := d.Store.ReleaseGroup(ctx, groupID)
		if err != nil || !matched {
			return i > 0
		}
		if st, _ := d.Store.GroupStatus(ctx, groupID); st == "failed" {
			log.Printf("briefing: group %s parked failed (%s)", groupID, reason)
			return true
		}
		// Re-take to keep counting attempts through TakeGroup.
		if _, err := d.Store.TakeGroup(ctx, groupID); err != nil {
			return true
		}
	}
	return true
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

// maxAnchorLabelRunes caps the anchor label in deterministic text.
// Ingest does not impose the redactor's 253-byte hostname limit on
// anchors, so a hostile multi-hundred-KiB hostname would otherwise sail
// through the model-budget rejection and land verbatim in the fallback
// sentence that replaces it. The cap (one MAX_FIELD_CHARS width, rune
// sound, marked) keeps fallback generation bounded independently of
// model restoration; falling back must not reproduce the oversized value
// that caused rejection. The stored anchor key itself is untouched —
// grouping identity still compares exact values.
const maxAnchorLabelRunes = 256

// anchorLabel strips the anchor-key prefix and caps degenerate labels.
func anchorLabel(anchor string) string {
	label := stripAnchorPrefix(anchor)
	if r := []rune(label); len(r) > maxAnchorLabelRunes {
		return string(r[:maxAnchorLabelRunes]) + "…"
	}
	return label
}

// describeEvents renders fixed templates plus event-type counts (enum
// values only) — no payload free text, per the Step 9 scope. Parts pass
// through dedupStrings and the anchor through stripAnchorPrefix so the
// production sentence never repeats a value or prints an anchor-key
// prefix; both are no-ops on today's already-unique parts, and guard the
// helpers' future callers.
func describeEvents(taken store.TakenGroup) string {
	counts := map[string]int{}
	order := []string{}
	for _, ev := range taken.Events {
		if _, ok := counts[ev.EventType]; !ok {
			order = append(order, ev.EventType)
		}
		counts[ev.EventType]++
	}
	parts := make([]string, 0, len(order))
	for _, typ := range order {
		parts = append(parts, strconv.Itoa(counts[typ])+" "+typ)
	}
	parts = dedupStrings(parts)
	return plural(len(taken.Events), "event") + " on " + anchorLabel(taken.Anchor) + ": " + strings.Join(parts, ", ")
}
