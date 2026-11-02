package briefing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ostefani/watcherbridge/internal/provider"
	"github.com/ostefani/watcherbridge/internal/redactor"
	"github.com/ostefani/watcherbridge/internal/store"
)

// modelBriefing is the strict model output schema (Designv5 §7.2,
// extended by Step 27 with the display-only explanation contract
// (Designv9 §6.3). The model answers with tokenized references only;
// every free-text field is re-capped and restored after the
// invented-token check passes.
type modelBriefing struct {
	EventIDs   []string `json:"event_ids"`
	Host       string   `json:"host"`
	What       string   `json:"what_changed"`
	Context    string   `json:"context"`
	Suggestion *string  `json:"model_suggestion"`
	Reason     string   `json:"priority_reason"`
	Check      string   `json:"recommended_check"`
	// Likely is the display-only hypothesis; Confidence its display-only
	// self-assessment. Neither feeds priority, identity or the resolver.
	Likely     string            `json:"likely_explanation"`
	Confidence string            `json:"confidence"`
	Refs       []json.RawMessage `json:"evidence_refs"`
}

// maxEvidenceRefs caps the model's citations (Designv9 §6.3): at most
// eight validated real event/tool refs. Fewer — including none — is
// valid; more is a repair, then a fallback.
const maxEvidenceRefs = 8

// modelEvidenceRef is one cited target: exactly one of the two keys.
// Step is a 0-based executed tool step of the producing run.
type modelEvidenceRef struct {
	Event *string `json:"event_id"`
	Step  *int    `json:"tool_step"`
}

// decodeEvidenceRef parses one citation strictly: no unknown fields, no
// trailing data, exactly one target. Errors name the broken rule, never
// values.
func decodeEvidenceRef(raw json.RawMessage) (modelEvidenceRef, error) {
	var r modelEvidenceRef
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return r, fmt.Errorf("unknown field in evidence ref")
		}
		return r, fmt.Errorf("malformed evidence ref")
	}
	if dec.More() {
		return r, fmt.Errorf("trailing data after evidence ref")
	}
	if (r.Event == nil) == (r.Step == nil) {
		return r, fmt.Errorf("evidence ref must name exactly one target")
	}
	if r.Event != nil && *r.Event == "" {
		return r, fmt.Errorf("evidence event ref must be non-empty")
	}
	if r.Step != nil && *r.Step < 0 {
		return r, fmt.Errorf("evidence tool ref must be non-negative")
	}
	return r, nil
}

// decodeModelBriefing parses one attempt strictly: no unknown fields, no
// trailing data. Errors name the broken rule, never values — they are safe
// to log and to quote back in the single repair prompt.
func decodeModelBriefing(raw []byte) (modelBriefing, error) {
	var m modelBriefing
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		// encoding/json exports no typed unknown-field error; the
		// message form is stable stdlib behavior.
		if strings.Contains(err.Error(), "unknown field") {
			return m, fmt.Errorf("unknown field")
		}
		return m, fmt.Errorf("malformed model JSON")
	}
	if dec.More() {
		return m, fmt.Errorf("trailing data after briefing object")
	}
	if len(m.EventIDs) == 0 || m.Host == "" || m.What == "" ||
		m.Context == "" || m.Reason == "" || m.Check == "" ||
		m.Likely == "" || m.Confidence == "" || m.Refs == nil {
		return m, fmt.Errorf("missing or empty required field")
	}
	return m, nil
}

// maxLikelyRunes caps the display-only hypothesis (Designv9 §6.3:
// likely_explanation<=256 chars). Restoration expansion rides the
// separate 256 KiB preflight, never silent truncation.
const maxLikelyRunes = 256

// checkModelBriefing enforces the content rules: the event set is the
// group's, the host is an issued token, the suggestion (when present) is a
// known level, the explanation is bounded display-only text, confidence is
// a known self-assessment, every citation names a real group event or a
// real executed tool step, and no text field invents tokens.
// maxToolStep is the highest citable 0-based executed step of the
// producing run (-1 on the single-call path, which executes nothing, so
// no tool ref is real there). With redaction off no tokens are issued,
// so the host and token rules cannot apply — raw values are the
// operator's explicit choice, and only token-shaped inventions (which
// restore would mishandle) are still rejected.
func checkModelBriefing(m modelBriefing, taken store.TakenGroup, issued redactor.TokenMap, tokensEnforced bool, maxToolStep int) (Level, []store.EvidenceRef, error) {
	want := map[string]bool{}
	for _, ev := range taken.Events {
		want[ev.EventID] = true
	}
	if len(m.EventIDs) != len(want) {
		return Low, nil, fmt.Errorf("event_ids do not match the group")
	}
	for _, id := range m.EventIDs {
		if !want[id] {
			return Low, nil, fmt.Errorf("event_ids do not match the group")
		}
	}
	if tokensEnforced {
		if _, ok := issued[m.Host]; !ok {
			return Low, nil, fmt.Errorf("host is not an issued token")
		}
	}
	var suggestion *Level
	if m.Suggestion != nil {
		lvl, err := ParseLevel(*m.Suggestion)
		if err != nil {
			return Low, nil, fmt.Errorf("model_suggestion is not a known level")
		}
		suggestion = &lvl
	}
	if len([]rune(m.Likely)) > maxLikelyRunes {
		return Low, nil, fmt.Errorf("likely_explanation exceeds 256 chars")
	}
	switch m.Confidence {
	case "low", "medium", "high":
	default:
		return Low, nil, fmt.Errorf("confidence is not low, medium or high")
	}
	if len(m.Refs) > maxEvidenceRefs {
		return Low, nil, fmt.Errorf("too many evidence refs")
	}
	refs := make([]store.EvidenceRef, 0, len(m.Refs))
	for _, raw := range m.Refs {
		r, err := decodeEvidenceRef(raw)
		if err != nil {
			return Low, nil, err
		}
		if r.Event != nil {
			if !want[*r.Event] {
				return Low, nil, fmt.Errorf("evidence ref is not a group event")
			}
			refs = append(refs, store.EventRef(*r.Event))
			continue
		}
		if *r.Step > maxToolStep {
			return Low, nil, fmt.Errorf("evidence ref is not a real tool step")
		}
		refs = append(refs, store.ToolRef(*r.Step))
	}
	joined := m.Host + "\n" + m.What + "\n" + m.Context + "\n" + m.Reason + "\n" + m.Check + "\n" + m.Likely
	if err := redactor.ValidateTokens(joined, issued); err != nil {
		return Low, nil, fmt.Errorf("invented tokens present")
	}
	if suggestion == nil {
		return Low, refs, errAbstain
	}
	return *suggestion, refs, nil
}

// errAbstain marks a schema-valid answer with suggestion null: not a
// failure, no repair — the baseline simply stands.
var errAbstain = fmt.Errorf("model abstains")

// plainText sanitizes stored model text without truncating it (Designv8
// §2.5): controls are stripped, but the full length is stored and the UI
// clamps with CSS — never "[truncated]" in the row. Length caps apply only
// to the provider-bound bundle copy, where the model actually receives it.
func plainText(s string) string {
	return redactor.StripControls(s)
}

// maxRestoredBytes caps the post-restoration stored text. The provider's
// 64 KiB response cap bounds tokenized text, not expanded text: one short
// token can restore into a long network-supplied hostname, so the sum of
// the restored fields is budgeted separately. Over budget the group
// degrades to the deterministic fallback — rejection, never silent
// truncation.
const maxRestoredBytes = 256 << 10

// projectedRestoredSize sums the exact restored lengths of the model text
// fields without building them (redactor.ProjectedRestoreSize is exact for
// a given map, so no allocation can exceed the sum). briefWithModel calls
// it before any Restore, so the budget check bounds restoration memory
// too — checking after Restore would protect storage but not the
// allocation itself. The reason travels raw and contributes its plain
// length. The map must be the effective restoration map (display spellings
// overriding normalized values): projecting with normalized values while
// restoring with originals measures a different, shorter string.
func projectedRestoredSize(m modelBriefing, restored redactor.TokenMap) int {
	total := len(m.Reason)
	for _, f := range []string{m.What, m.Context, m.Check, m.Likely} {
		total += redactor.ProjectedRestoreSize(f, restored)
		if total > maxRestoredBytes {
			return total
		}
	}
	return total
}

// fallbackBriefing builds the deterministic briefing (Designv5 §7.3): the
// baseline owns priority, the suggestion is null, the reason is the exact
// fallback string. Storing it moves processing→done, so the group never
// strands — a fallback always completes the group.
func fallbackBriefing(taken store.TakenGroup, outcome Outcome, now time.Time, contextNote string, traces []store.AgentTrace, usage provider.Usage) store.Briefing {
	return deterministicBriefing(taken, outcome, now, contextNote, "fallback", true, traces, usage)
}

// deterministicBriefing builds the model-less briefing. source names
// the path (Designv9 §6.1): "fallback" for degraded provider paths,
// "rules" for intentional below-floor output — fallback is distinct
// from intentionally rules-only output, so the two never share the
// source or the fallback flag. Collected execution audit (traces) and
// adapter accounting (usage) ride along when the caller supplies them:
// a fallback still stores what the run did, with source fallback, so
// failed runs leave no silent gap in audit or accounting.
func deterministicBriefing(taken store.TakenGroup, outcome Outcome, now time.Time, contextNote, source string, fallbackUsed bool, traces []store.AgentTrace, usage provider.Usage) store.Briefing {
	b := store.Briefing{
		BriefingID:       "brf-" + taken.GroupID,
		GroupID:          taken.GroupID,
		HostAnchorKey:    taken.Anchor,
		Baseline:         outcome.Level.String(),
		ModelSuggestion:  nil,
		Priority:         outcome.Level.String(),
		PriorityReason:   fallbackReason,
		BaselineRule:     outcome.RuleID,
		Kind:             KindForTaken(taken, outcome.RuleID),
		Source:           source,
		FallbackUsed:     fallbackUsed,
		WhatChanged:      describeEvents(taken),
		Context:          contextNote,
		RecommendedCheck: "Review the recent changes on this host.",
		CreatedAt:        now,
		Traces:           traces,
	}
	if usage.HasUsage {
		in, out := usage.InputTokens, usage.OutputTokens
		b.InputTokens, b.OutputTokens = &in, &out
	}
	if usage.HasCost {
		cost := usage.CostMicros
		b.CostMicros = &cost
	}
	return b
}

// insertFallback stores the deterministic briefing; on storage error the
// group is released (or parked at the cap) — never silently dropped. A
// dead context stores nothing: a fallback for a group the model never
// saw would be a permanent wrong record, so shutdown leaves the group
// for the lease (or next startup) instead. Reports whether state changed.
func insertFallback(ctx context.Context, d Deps, taken store.TakenGroup, outcome Outcome, contextNote string, traces []store.AgentTrace, usage provider.Usage) bool {
	return insertDeterministic(ctx, d, taken, outcome, contextNote, "fallback", true, traces, usage)
}

// insertRules stores the intentional below-floor deterministic briefing
// (Designv9 §6.1): same content shape as a fallback, but source "rules"
// with no fallback flag and no fallback health count. Below the model
// threshold the bridge answers from reported context — no provider is
// billed, nothing degraded.
func insertRules(ctx context.Context, d Deps, taken store.TakenGroup, outcome Outcome, contextNote string) bool {
	return insertDeterministic(ctx, d, taken, outcome, contextNote, "rules", false, nil, provider.Usage{})
}

func insertDeterministic(ctx context.Context, d Deps, taken store.TakenGroup, outcome Outcome, contextNote, source string, countFallback bool, traces []store.AgentTrace, usage provider.Usage) bool {
	if ctx.Err() != nil {
		return false
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	b := deterministicBriefing(taken, outcome, now, contextNote, source, countFallback, traces, usage)
	if err := d.Store.InsertBriefing(ctx, b); err != nil {
		log.Printf("briefing: group %s %s insert: %s", taken.GroupID, source, store.SanitizeErrorText(err.Error()))
		if transientStoreErr(ctx, err) {
			return releaseTransientErr(ctx, d, taken, err)
		}
		return releaseOrPark(ctx, d, taken, source+" insert failed")
	}
	if countFallback {
		d.Health.AddProviderFallback()
	}
	notifyBriefing(d, taken, b)
	if len(outcome.Skipped) > 0 {
		log.Printf("briefing: group %s baseline %s skipped %d unknown-input rules", taken.GroupID, outcome.RuleID, len(outcome.Skipped))
	}
	return true
}

// buildScannedBundle builds the redacted bundle both provider paths
// send (Designv9 §6.3: every bundle is leak-scanned fail-closed before
// it leaves the machine): tokenize, serialize, enforce the bundle cap,
// and scan. An empty fail reason means success; otherwise the caller
// stores the deterministic fallback with that reason.
func buildScannedBundle(ctx context.Context, d Deps, taken store.TakenGroup, outcome Outcome) (raw []byte, issued, display redactor.TokenMap, failReason string) {
	timeout := d.ModelTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	events := make([]redactor.InputEvent, 0, len(taken.Events))
	for _, ev := range taken.Events {
		events = append(events, redactor.InputEvent{EventID: ev.EventID, EventType: ev.EventType, Payload: ev.Payload})
	}
	bundle, issued, display, err := redactor.BuildBundle(ctx, d.Store, redactor.BundleInput{
		GroupID:  taken.GroupID,
		Anchor:   taken.Anchor,
		Baseline: outcome.Level.String(),
		Events:   events,
	}, d.Redaction)
	if err != nil {
		log.Printf("briefing: group %s bundle build: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
		return nil, nil, nil, "bundle build failed; briefing from deterministic baseline only"
	}
	var err2 error
	raw, err2 = json.Marshal(bundle)
	if err2 != nil {
		log.Printf("briefing: group %s bundle encode: %s", taken.GroupID, store.SanitizeErrorText(err2.Error()))
		return nil, nil, nil, "bundle encode failed; briefing from deterministic baseline only"
	}
	// The model-side limit is the serialized bundle, not the group
	// payload: field capping makes the mapping nonlinear, so only the
	// artifact the model actually receives can bound the call. Over the
	// cap the group degrades to the deterministic fallback — cheap and
	// explicit, never a billed multi-megabyte call.
	if len(raw) > redactor.MaxBundleBytes {
		log.Printf("briefing: group %s bundle %d bytes exceeds the %d-byte cap", taken.GroupID, len(raw), redactor.MaxBundleBytes)
		return nil, nil, nil, "bundle exceeds size cap; briefing from deterministic baseline only"
	}
	if d.Redaction.Enabled {
		// Last step before anything leaves the machine: fail closed.
		// A hit aborts the provider call — no send, fallback instead.
		if hits, err := redactor.ScanOutboundLeaks(raw, issued, bundle.EventIDs); err != nil || len(hits) > 0 {
			log.Printf("briefing: group %s redaction blocked at %v", taken.GroupID, hits)
			d.Health.AddRedactionBlocked()
			return nil, nil, nil, "redaction blocked the model call; briefing from deterministic baseline only"
		}
	}

	return raw, issued, display, ""
}

// briefWithModel runs the single-call pipeline for a taken group:
// bundle → leak scan → provider (+1 repair) → validate → resolve →
// restore → insert. Every terminal path stores exactly one briefing.
func briefWithModel(ctx context.Context, d Deps, taken store.TakenGroup, outcome Outcome) bool {
	if taken.LastAttempt() {
		// Final take: skip the provider and write the deterministic
		// briefing inline, so a poison group converges to done.
		return insertFallback(ctx, d, taken, outcome, "retry cap reached; briefing from deterministic baseline only", nil, provider.Usage{})
	}
	raw, issued, display, failReason := buildScannedBundle(ctx, d, taken, outcome)
	if failReason != "" {
		return insertFallback(ctx, d, taken, outcome, failReason, nil, provider.Usage{})
	}

	// The adapter gets the token-name set for echo validation — never
	// the token-to-value map, which stays in this layer for Restore.
	valid := make(map[string]struct{}, len(issued))
	for tok := range issued {
		valid[tok] = struct{}{}
	}
	timeout := d.ModelTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	modelCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d.Health.AddProviderCall()
	answer, err := d.Provider.Brief(modelCtx, raw, valid)
	if err != nil {
		log.Printf("briefing: group %s provider: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
		return insertFallback(ctx, d, taken, outcome, "model call failed; briefing from deterministic baseline only", nil, provider.Usage{})
	}
	d.Health.AddUsage(answer.Usage)
	usage := answer.Usage
	// The single call executes no tools, so no tool_step ref is real.
	a := acceptAnswer(answer.JSON, taken, issued, d.Redaction.Enabled, -1)
	if !a.OK {
		// The repair is a second billed send: it counts like the
		// initial call, mirroring the agent loop where every turn —
		// including the repair re-ask — counts.
		d.Health.AddProviderCall()
		repair, err := d.Provider.Repair(modelCtx, repairPrompt(a.Correction), valid)
		if err != nil {
			log.Printf("briefing: group %s repair: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
			return insertFallback(ctx, d, taken, outcome, "model repair failed; briefing from deterministic baseline only", nil, provider.Usage{})
		}
		d.Health.AddUsage(repair.Usage)
		usage = usage.Add(repair.Usage)
		if a = acceptAnswer(repair.JSON, taken, issued, d.Redaction.Enabled, -1); !a.OK {
			log.Printf("briefing: group %s model invalid after repair (%s)", taken.GroupID, a.Correction)
			return insertFallback(ctx, d, taken, outcome, "model output invalid; briefing from deterministic baseline only", nil, usage)
		}
	}
	return insertModelBriefing(ctx, d, taken, outcome, a, issued, display, "model", nil, usage)
}

// insertModelBriefing stores one validated model answer: the suggestion
// resolves against the baseline, tokens restore to display spellings
// within the byte budget, and exactly one briefing inserts. source names
// the path ("model" for the single call, "agent" for the loop, Designv9
// §6.1) so the audit trail distinguishes them. Every error path releases
// or parks the group — never silently drops it.
func insertModelBriefing(ctx context.Context, d Deps, taken store.TakenGroup, outcome Outcome, a answer, issued, display redactor.TokenMap, source string, traces []store.AgentTrace, usage provider.Usage) bool {
	m, suggestion, rawJSON := a.Model, a.Suggestion, a.Raw
	final, clamped := Resolve(outcome.Level, suggestion)
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	var sug *string
	if suggestion != nil {
		s := suggestion.String()
		sug = &s
	}
	// The effective restoration map, built once: display spellings
	// override normalized values, and projection and restoration must
	// measure the same map — projecting normalized values while
	// restoring originals approves strings that exceed the budget.
	effective := restoreMap(issued, display)
	// Preflight before allocating: the projection is exact, so nothing
	// restored can exceed what was just approved.
	if projectedRestoredSize(m, effective) > maxRestoredBytes {
		log.Printf("briefing: group %s restored text exceeds the %d-byte budget", taken.GroupID, maxRestoredBytes)
		return insertFallback(ctx, d, taken, outcome, "restored text exceeds size budget; briefing from deterministic baseline only", traces, usage)
	}
	what := plainText(redactor.Restore(m.What, effective))
	briefCtx := plainText(redactor.Restore(m.Context, effective))
	check := plainText(redactor.Restore(m.Check, effective))
	likely := plainText(redactor.Restore(m.Likely, effective))
	// The 256-char explanation bound constrains the tokenized answer,
	// but short tokens restore long: repeated tokens can expand a
	// compliant answer well past the bound while the aggregate budget
	// stays quiet. Enforce the final display text, not just the input.
	if len([]rune(likely)) > maxLikelyRunes {
		log.Printf("briefing: group %s restored explanation exceeds 256 chars", taken.GroupID)
		return insertFallback(ctx, d, taken, outcome, "restored explanation exceeds 256 chars; briefing from deterministic baseline only", traces, usage)
	}
	reason := plainText(m.Reason)
	b := store.Briefing{
		BriefingID:        "brf-" + taken.GroupID,
		GroupID:           taken.GroupID,
		HostAnchorKey:     taken.Anchor,
		Baseline:          outcome.Level.String(),
		ModelSuggestion:   sug,
		Priority:          final.String(),
		PriorityReason:    reason,
		BaselineRule:      outcome.RuleID,
		Kind:              KindForTaken(taken, outcome.RuleID),
		Source:            source,
		Clamped:           clamped,
		FallbackUsed:      false,
		WhatChanged:       what,
		Context:           briefCtx,
		RecommendedCheck:  check,
		ModelRawJSON:      string(rawJSON),
		CreatedAt:         now,
		LikelyExplanation: &likely,
		Confidence:        m.Confidence,
		EvidenceRefs:      a.Refs,
		Traces:            traces,
	}
	// Adapter-reported accounting rides only when supplied: nil reads
	// as unknown, never as a free call.
	if usage.HasUsage {
		in, out := usage.InputTokens, usage.OutputTokens
		b.InputTokens, b.OutputTokens = &in, &out
	}
	if usage.HasCost {
		cost := usage.CostMicros
		b.CostMicros = &cost
	}
	if err := d.Store.InsertBriefing(ctx, b); err != nil {
		log.Printf("briefing: group %s insert: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
		if transientStoreErr(ctx, err) {
			return releaseTransientErr(ctx, d, taken, err)
		}
		return releaseOrPark(ctx, d, taken, "insert failed")
	}
	notifyBriefing(d, taken, b)
	if len(outcome.Skipped) > 0 {
		log.Printf("briefing: group %s baseline %s skipped %d unknown-input rules", taken.GroupID, outcome.RuleID, len(outcome.Skipped))
	}
	return true
}

// restoreMap merges the issued (normalized) and display (first-seen
// original) maps for Restore: exact operator-facing text wins, the
// normalized value covers any token missing a display entry.
func restoreMap(issued, display redactor.TokenMap) redactor.TokenMap {
	out := make(redactor.TokenMap, len(issued))
	for tok, norm := range issued {
		out[tok] = norm
	}
	for tok, orig := range display {
		out[tok] = orig
	}
	return out
}

// answer is one model attempt: the decoded briefing and suggestion when
// valid (a null suggestion abstains and the baseline stands), the
// validated citations, the raw bytes for the audit trail, or the
// fixed-kind rejection when invalid.
type answer struct {
	Model      modelBriefing
	Suggestion *Level
	Refs       []store.EvidenceRef
	Raw        []byte
	OK         bool
	Correction string // rejection kind when !OK; never bundle content
}

// acceptAnswer validates one model answer. A null suggestion is valid
// (the baseline stands, model text still used). maxToolStep bounds the
// citable executed steps (-1 where the producer executes nothing).
// Correction prompts quote the schema error kind only — never bundle
// content, so a hostile bundle cannot steer the retry.
func acceptAnswer(raw []byte, taken store.TakenGroup, issued redactor.TokenMap, tokensEnforced bool, maxToolStep int) answer {
	decoded, err := decodeModelBriefing(raw)
	if err != nil {
		return answer{Correction: err.Error()}
	}
	lvl, refs, err := checkModelBriefing(decoded, taken, issued, tokensEnforced, maxToolStep)
	if err != nil {
		if err == errAbstain {
			return answer{Model: decoded, Refs: refs, Raw: raw, OK: true}
		}
		return answer{Correction: err.Error()}
	}
	lvlCopy := lvl
	return answer{Model: decoded, Suggestion: &lvlCopy, Refs: refs, Raw: raw, OK: true}
}

// repairPrompt frames the single bounded retry: the schema error kind plus
// an instruction to re-emit the full object. No bundle content travels.
func repairPrompt(kind string) string {
	return "Your briefing object was rejected (" + kind + "). Re-emit the complete briefing object as strict JSON with exactly the documented fields, " +
		"including likely_explanation (your display-only hypothesis, at most 256 chars), confidence (low, medium or high), and evidence_refs " +
		"(at most 8 citations, each {\"event_id\": \"<a group event ID>\"}; there are no tool steps on this path)."
}
