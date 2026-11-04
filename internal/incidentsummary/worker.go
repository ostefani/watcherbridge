package incidentsummary

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/ostefani/watcherbridge/internal/briefing"
	"github.com/ostefani/watcherbridge/internal/health"
	"github.com/ostefani/watcherbridge/internal/provider"
	"github.com/ostefani/watcherbridge/internal/redactor"
	"github.com/ostefani/watcherbridge/internal/store"
)

// Deps wires the single summary worker. Provider must expose the
// explicit incident-summary capability (provider.Summarizer); the
// startup wiring asserts this and refuses to start otherwise, so a
// typo can never route snapshots to an unexpected endpoint. Health is
// nil-safe. OnComplete fires after each terminal commit (model,
// fallback or skipped) with the incident ID; it runs on the worker
// goroutine and must never block (hub broadcast drops rather than
// waits in production wiring).
type Deps struct {
	Store     *store.Store
	Provider  provider.Summarizer
	Redaction redactor.Policy
	Now       func() time.Time
	Health    *health.Counters
	// OnComplete invalidates incident SSE and reconnect/resync state:
	// one incident event plus an overview refresh, mirroring the
	// correlator loop. Summary completion never changes membership,
	// reopens incidents, modifies decisions or publishes scanner
	// annotations.
	OnComplete func(incidentID string)
}

func nowOf(d Deps) time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Run owns the single summary worker until ctx ends: claim one durable
// job, build the bounded snapshot, send once plus at most one repair
// sharing the run deadline, validate, resolve via briefing.Resolve,
// and commit against the matching claim and snapshot. It returns nil
// when ctx ends. Cancellation and shutdown are explicit: a dead
// context stores nothing and leaves the running claim for its lease.
func Run(ctx context.Context, d Deps) error {
	if d.Store == nil {
		return fmt.Errorf("summary worker requires a store")
	}
	if d.Provider == nil {
		return fmt.Errorf("summary worker requires a summarizer provider")
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		drained := false
		for {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			claimed, err := runOne(ctx, d)
			if err != nil {
				log.Printf("incident-summary: %s", store.SanitizeErrorText(err.Error()))
				break
			}
			if !claimed {
				break
			}
			drained = true
		}
		_ = drained
		timer.Reset(pollTick)
	}
}

// runOne claims and resolves a single job. It reports whether a job
// reached a terminal state in this call.
func runOne(ctx context.Context, d Deps) (bool, error) {
	now := nowOf(d)
	job, claim, ok, err := d.Store.ClaimIncidentSummary(ctx, now)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, nil
	}
	if job.SendsReserved > 0 {
		// A dispatched send may have consumed allowance even if its
		// response was lost. Never repeat it after lease recovery.
		outcome := store.IncidentSummary{Status: "fallback", FinalPriority: job.Floor,
			InputTokens: job.InputTokens, OutputTokens: job.OutputTokens,
			Error: "summary interrupted after provider dispatch; deterministic presentation retained"}
		if _, err := d.Store.CompleteIncidentSummary(ctx, job.IncidentID, claim, job.SnapshotJSON, outcome, nowOf(d)); err != nil {
			return false, err
		}
		completed(d, job.IncidentID)
		return true, nil
	}
	incident, err := d.Store.GetIncident(ctx, job.IncidentID)
	if err != nil {
		// Incident purged under a retained summary row: keep the
		// deterministic row and record the miss as fallback.
		outcome := store.IncidentSummary{
			Status: "fallback", FinalPriority: job.Floor,
			Error: "incident purged before summary; deterministic presentation retained",
		}
		if _, cerr := d.Store.CompleteIncidentSummary(ctx, job.IncidentID, claim, job.SnapshotJSON, outcome, nowOf(d)); cerr != nil {
			return false, cerr
		}
		completed(d, job.IncidentID)
		return true, nil
	}
	floorLevel, err := briefing.ParseLevel(incident.Floor)
	if err != nil {
		outcome := store.IncidentSummary{
			Status: "fallback", FinalPriority: job.Floor,
			Error: "invalid stored incident floor; deterministic presentation retained",
		}
		if _, cerr := d.Store.CompleteIncidentSummary(ctx, job.IncidentID, claim, job.SnapshotJSON, outcome, nowOf(d)); cerr != nil {
			return false, cerr
		}
		completed(d, job.IncidentID)
		return true, nil
	}
	raw, issued, display, aliasMap, sampled, omitted, failReason := BuildSnapshot(ctx, d.Store, incident, d.Redaction)
	if failReason != "" {
		// Build/redaction failure bills nothing: deterministic
		// fallback with the safe reason, preserving any usage (none).
		outcome := store.IncidentSummary{
			Status: "fallback", FinalPriority: incident.Floor,
			Error: failReason,
		}
		if _, cerr := d.Store.CompleteIncidentSummary(ctx, job.IncidentID, claim, job.SnapshotJSON, outcome, nowOf(d)); cerr != nil {
			return false, cerr
		}
		if d.Health != nil {
			d.Health.AddProviderFallback()
		}
		completed(d, job.IncidentID)
		return true, nil
	}
	aliasJSON, _ := json.Marshal(aliasMap)
	if err := d.Store.ReserveSummarySnapshot(ctx, job.IncidentID, claim, string(raw), string(aliasJSON), sampled, omitted, nowOf(d)); err != nil {
		// Stale claim (expired, completed elsewhere): stop quietly.
		return false, nil
	}
	valid := make(map[string]struct{}, len(issued)+len(aliasMap))
	for tok := range issued {
		valid[tok] = struct{}{}
	}
	for alias := range aliasMap {
		valid[alias] = struct{}{}
	}
	runCtx, cancel := context.WithTimeout(ctx, runDeadline)
	defer cancel()
	reserved, err := d.Store.ReserveSummarySend(ctx, job.IncidentID, claim, nowOf(d))
	if err != nil {
		return false, err
	}
	if !reserved {
		return false, nil
	}
	if d.Health != nil {
		d.Health.AddProviderCall()
	}
	answer, err := d.Provider.Summarize(runCtx, raw, valid)
	if err != nil {
		outcome := store.IncidentSummary{
			Status: "fallback", FinalPriority: incident.Floor,
			Error: "summary call failed; deterministic presentation retained",
		}
		if _, cerr := d.Store.CompleteIncidentSummary(ctx, job.IncidentID, claim, string(raw), outcome, nowOf(d)); cerr != nil {
			return false, cerr
		}
		if d.Health != nil {
			d.Health.AddProviderFallback()
		}
		completed(d, job.IncidentID)
		return true, nil
	}
	usage := answer.Usage
	if err := d.Store.RecordSummaryUsage(ctx, job.IncidentID, claim, usage.InputTokens, usage.OutputTokens, usage.HasUsage, nowOf(d)); err != nil {
		return false, err
	}
	if d.Health != nil {
		d.Health.AddUsage(usage)
	}
	accepted, correction, ok := acceptSummary(answer.JSON, aliasMap, issued, display, d.Redaction.Enabled, floorLevel)
	if !ok {
		reserved, rerr := d.Store.ReserveSummarySend(ctx, job.IncidentID, claim, nowOf(d))
		if rerr != nil {
			return false, rerr
		}
		if !reserved {
			return false, nil
		}
		if d.Health != nil {
			d.Health.AddProviderCall()
		}
		repair, rerr := d.Provider.RepairSummary(runCtx, repairPrompt(correction), valid)
		if rerr != nil {
			outcome := store.IncidentSummary{
				Status: "fallback", FinalPriority: incident.Floor,
				Error: "summary repair failed; deterministic presentation retained",
			}
			// Preserve reported usage from the rejected initial send.
			outcome.InputTokens, outcome.OutputTokens, outcome.CostMicros = usagePtrs(usage)
			if _, cerr := d.Store.CompleteIncidentSummary(ctx, job.IncidentID, claim, string(raw), outcome, nowOf(d)); cerr != nil {
				return false, cerr
			}
			if d.Health != nil {
				d.Health.AddProviderFallback()
			}
			completed(d, job.IncidentID)
			return true, nil
		}
		usage = usage.Add(repair.Usage)
		if err := d.Store.RecordSummaryUsage(ctx, job.IncidentID, claim, repair.Usage.InputTokens, repair.Usage.OutputTokens, repair.Usage.HasUsage, nowOf(d)); err != nil {
			return false, err
		}
		if d.Health != nil {
			d.Health.AddUsage(repair.Usage)
		}
		accepted2, correction2, ok2 := acceptSummary(repair.JSON, aliasMap, issued, display, d.Redaction.Enabled, floorLevel)
		if !ok2 {
			outcome := store.IncidentSummary{
				Status: "fallback", FinalPriority: incident.Floor,
				Error: "summary output invalid (" + correction2 + "); deterministic presentation retained",
			}
			outcome.InputTokens, outcome.OutputTokens, outcome.CostMicros = usagePtrs(usage)
			if _, cerr := d.Store.CompleteIncidentSummary(ctx, job.IncidentID, claim, string(raw), outcome, nowOf(d)); cerr != nil {
				return false, cerr
			}
			if d.Health != nil {
				d.Health.AddProviderFallback()
			}
			completed(d, job.IncidentID)
			return true, nil
		}
		accepted = accepted2
	}
	outcome := commitOutcome(accepted, incident.Floor, string(raw), aliasMap, sampled, omitted, usage)
	if _, cerr := d.Store.CompleteIncidentSummary(ctx, job.IncidentID, claim, string(raw), outcome, nowOf(d)); cerr != nil {
		return false, cerr
	}
	completed(d, job.IncidentID)
	return true, nil
}

func completed(d Deps, incidentID string) {
	if d.OnComplete == nil {
		return
	}
	d.OnComplete(incidentID)
}

func usagePtrs(u provider.Usage) (*int64, *int64, *int64) {
	var in, out, cost *int64
	if u.HasUsage {
		in, out = &u.InputTokens, &u.OutputTokens
	}
	if u.HasCost {
		cost = &u.CostMicros
	}
	return in, out, cost
}

// acceptSummary validates one model answer against the snapshot alias
// set and the issued tokens, enforces byte/character limits after
// restoration, and resolves via briefing.Resolve with the deterministic
// floor. The resolver's raise-only, one-step and high-cap behavior is
// preserved: an existing critical floor remains critical, and a
// previously model-raised incident priority is never used as the next
// baseline (the floor comes from stored member finals, not from a
// prior summary).
func acceptSummary(rawJSON []byte, aliases map[string]string, issued, display redactor.TokenMap, redactionEnabled bool, floor briefing.Level) (acceptedSummary, string, bool) {
	if len(rawJSON) > maxRawOutputBytes {
		return acceptedSummary{}, "output exceeds 8 KiB", false
	}
	decoded, err := decodeModelSummary(rawJSON)
	if err != nil {
		return acceptedSummary{}, err.Error(), false
	}
	suggestion, err := checkSummary(decoded, aliases, issued, redactionEnabled)
	if err != nil {
		return acceptedSummary{}, err.Error(), false
	}
	final, clamped := briefing.Resolve(floor, suggestion)
	// Restore display spellings within the 16 KiB budget; over budget
	// the answer is invalid (repair, then fallback) — never silent
	// truncation. The 256-char hypothesis bound applies to the final
	// display text as well.
	effective := make(redactor.TokenMap, len(issued)+len(display))
	for tok, v := range issued {
		effective[tok] = v
	}
	for tok, v := range display {
		effective[tok] = v
	}
	total := len(decoded.Reason)
	for _, f := range []string{decoded.Summary, decoded.Hypothesis} {
		total += redactor.ProjectedRestoreSize(f, effective)
		if total > maxRestoredBytes {
			return acceptedSummary{}, "restored output exceeds 16 KiB", false
		}
	}
	summary := redactor.StripControls(redactor.Restore(decoded.Summary, effective))
	hypothesis := redactor.StripControls(redactor.Restore(decoded.Hypothesis, effective))
	if len([]rune(hypothesis)) > maxHypothesisRunes {
		return acceptedSummary{}, "hypothesis exceeds 256 chars", false
	}
	return acceptedSummary{Model: decoded, Suggestion: suggestion, Final: final, Clamped: clamped, Raw: rawJSON, Summary: summary, Hypothesis: hypothesis}, "", true
}

// commitOutcome renders one accepted answer into the durable outcome:
// floor, suggestion, final priority and clamp stored separately;
// confidence stays display-only. Citations are the validated alias
// set; reported usage rides only when supplied.
func commitOutcome(a acceptedSummary, floor string, _ string, aliases map[string]string, sampled, omitted int, usage provider.Usage) store.IncidentSummary {
	var suggestion *string
	if a.Suggestion != nil {
		s := a.Suggestion.String()
		suggestion = &s
	}
	cits := make([]store.SummaryCitation, 0, len(a.Model.Citations))
	for _, raw := range a.Model.Citations {
		var c struct {
			Member string `json:"member"`
		}
		_ = json.Unmarshal(raw, &c)
		if _, ok := aliases[c.Member]; ok {
			cits = append(cits, store.SummaryCitation{Member: c.Member})
		}
	}
	out := store.IncidentSummary{
		Status: "model", Floor: floor, Suggestion: suggestion,
		FinalPriority: a.Final.String(), Clamped: a.Clamped,
		Summary: a.Summary, Hypothesis: a.Hypothesis,
		Confidence: a.Model.Confidence, Reason: a.Model.Reason,
		Citations: cits, Sampled: sampled, Omitted: omitted,
	}
	in, outU, cost := usagePtrs(usage)
	out.InputTokens, out.OutputTokens, out.CostMicros = in, outU, cost
	return out
}

// repairPrompt frames the single bounded repair: the safe validation
// classification only — never the snapshot, rejected output, or raw
// identifiers.
func repairPrompt(correction string) string {
	return "Your incident-summary object was rejected (" + correction + "). Re-emit the complete summary object as strict JSON with exactly the documented fields, " +
		"including summary, hypothesis (at most 256 chars), confidence (low, medium or high), priority_suggestion (low, medium, high, critical or null), priority_reason, and citations " +
		"(at most 8, each {\"member\": \"<a snapshot alias>\"})."
}
