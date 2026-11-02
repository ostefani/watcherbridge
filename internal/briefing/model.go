package briefing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ostefani/watcherbridge/internal/redactor"
	"github.com/ostefani/watcherbridge/internal/store"
)

// modelBriefing is the strict model output schema (Designv5 §7.2). The
// model answers with tokenized references only; every free-text field is
// re-capped and restored after the invented-token check passes.
type modelBriefing struct {
	EventIDs   []string `json:"event_ids"`
	Host       string   `json:"host"`
	What       string   `json:"what_changed"`
	Context    string   `json:"context"`
	Suggestion *string  `json:"model_suggestion"`
	Reason     string   `json:"priority_reason"`
	Check      string   `json:"recommended_check"`
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
		m.Context == "" || m.Reason == "" || m.Check == "" {
		return m, fmt.Errorf("missing or empty required field")
	}
	return m, nil
}

// checkModelBriefing enforces the content rules: the event set is the
// group's, the host is an issued token, the suggestion (when present) is a
// known level, and no text field invents tokens. With redaction off no
// tokens are issued, so the host and token rules cannot apply — raw values
// are the operator's explicit choice, and only token-shaped inventions
// (which restore would mishandle) are still rejected.
func checkModelBriefing(m modelBriefing, taken store.TakenGroup, issued redactor.TokenMap, tokensEnforced bool) (Level, error) {
	want := map[string]bool{}
	for _, ev := range taken.Events {
		want[ev.EventID] = true
	}
	if len(m.EventIDs) != len(want) {
		return Low, fmt.Errorf("event_ids do not match the group")
	}
	for _, id := range m.EventIDs {
		if !want[id] {
			return Low, fmt.Errorf("event_ids do not match the group")
		}
	}
	if tokensEnforced {
		if _, ok := issued[m.Host]; !ok {
			return Low, fmt.Errorf("host is not an issued token")
		}
	}
	var suggestion *Level
	if m.Suggestion != nil {
		lvl, err := ParseLevel(*m.Suggestion)
		if err != nil {
			return Low, fmt.Errorf("model_suggestion is not a known level")
		}
		suggestion = &lvl
	}
	joined := m.Host + "\n" + m.What + "\n" + m.Context + "\n" + m.Reason + "\n" + m.Check
	if err := redactor.ValidateTokens(joined, issued); err != nil {
		return Low, fmt.Errorf("invented tokens present")
	}
	if suggestion == nil {
		return Low, errAbstain
	}
	return *suggestion, nil
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
	for _, f := range []string{m.What, m.Context, m.Check} {
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
func fallbackBriefing(taken store.TakenGroup, outcome Outcome, now time.Time, contextNote string) store.Briefing {
	return store.Briefing{
		BriefingID:       "brf-" + taken.GroupID,
		GroupID:          taken.GroupID,
		HostAnchorKey:    taken.Anchor,
		Baseline:         outcome.Level.String(),
		ModelSuggestion:  nil,
		Priority:         outcome.Level.String(),
		PriorityReason:   fallbackReason,
		BaselineRule:     outcome.RuleID,
		Kind:             KindForTaken(taken, outcome.RuleID),
		Source:           "fallback",
		FallbackUsed:     true,
		WhatChanged:      describeEvents(taken),
		Context:          contextNote,
		RecommendedCheck: "Review the recent changes on this host.",
		CreatedAt:        now,
	}
}

// insertFallback stores the deterministic briefing; on storage error the
// group is released (or parked at the cap) — never silently dropped. A
// dead context stores nothing: a fallback for a group the model never
// saw would be a permanent wrong record, so shutdown leaves the group
// for the lease (or next startup) instead. Reports whether state changed.
func insertFallback(ctx context.Context, d Deps, taken store.TakenGroup, outcome Outcome, contextNote string) bool {
	if ctx.Err() != nil {
		return false
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	b := fallbackBriefing(taken, outcome, now, contextNote)
	if err := d.Store.InsertBriefing(ctx, b); err != nil {
		log.Printf("briefing: group %s fallback insert: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
		if transientStoreErr(ctx, err) {
			return releaseTransientErr(ctx, d, taken, err)
		}
		return releaseOrPark(ctx, d, taken, "fallback insert failed")
	}
	d.Health.AddProviderFallback()
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
		return insertFallback(ctx, d, taken, outcome, "retry cap reached; briefing from deterministic baseline only")
	}
	raw, issued, display, failReason := buildScannedBundle(ctx, d, taken, outcome)
	if failReason != "" {
		return insertFallback(ctx, d, taken, outcome, failReason)
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
		return insertFallback(ctx, d, taken, outcome, "model call failed; briefing from deterministic baseline only")
	}
	a := acceptAnswer(answer.JSON, taken, issued, d.Redaction.Enabled)
	if !a.OK {
		repair, err := d.Provider.Repair(modelCtx, repairPrompt(a.Correction), valid)
		if err != nil {
			log.Printf("briefing: group %s repair: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
			return insertFallback(ctx, d, taken, outcome, "model repair failed; briefing from deterministic baseline only")
		}
		if a = acceptAnswer(repair.JSON, taken, issued, d.Redaction.Enabled); !a.OK {
			log.Printf("briefing: group %s model invalid after repair (%s)", taken.GroupID, a.Correction)
			return insertFallback(ctx, d, taken, outcome, "model output invalid; briefing from deterministic baseline only")
		}
	}
	return insertModelBriefing(ctx, d, taken, outcome, a, issued, display, "model")
}

// insertModelBriefing stores one validated model answer: the suggestion
// resolves against the baseline, tokens restore to display spellings
// within the byte budget, and exactly one briefing inserts. source names
// the path ("model" for the single call, "agent" for the loop, Designv9
// §6.1) so the audit trail distinguishes them. Every error path releases
// or parks the group — never silently drops it.
func insertModelBriefing(ctx context.Context, d Deps, taken store.TakenGroup, outcome Outcome, a answer, issued, display redactor.TokenMap, source string) bool {
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
		return insertFallback(ctx, d, taken, outcome, "restored text exceeds size budget; briefing from deterministic baseline only")
	}
	what := plainText(redactor.Restore(m.What, effective))
	briefCtx := plainText(redactor.Restore(m.Context, effective))
	check := plainText(redactor.Restore(m.Check, effective))
	reason := plainText(m.Reason)
	b := store.Briefing{
		BriefingID:       "brf-" + taken.GroupID,
		GroupID:          taken.GroupID,
		HostAnchorKey:    taken.Anchor,
		Baseline:         outcome.Level.String(),
		ModelSuggestion:  sug,
		Priority:         final.String(),
		PriorityReason:   reason,
		BaselineRule:     outcome.RuleID,
		Kind:             KindForTaken(taken, outcome.RuleID),
		Source:           source,
		Clamped:          clamped,
		FallbackUsed:     false,
		WhatChanged:      what,
		Context:          briefCtx,
		RecommendedCheck: check,
		ModelRawJSON:     string(rawJSON),
		CreatedAt:        now,
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
// valid (a null suggestion abstains and the baseline stands), the raw
// bytes for the audit trail, or the fixed-kind rejection when invalid.
type answer struct {
	Model      modelBriefing
	Suggestion *Level
	Raw        []byte
	OK         bool
	Correction string // rejection kind when !OK; never bundle content
}

// acceptAnswer validates one model answer. A null suggestion is valid
// (the baseline stands, model text still used). Correction prompts quote
// the schema error kind only — never bundle content, so a hostile bundle
// cannot steer the retry.
func acceptAnswer(raw []byte, taken store.TakenGroup, issued redactor.TokenMap, tokensEnforced bool) answer {
	decoded, err := decodeModelBriefing(raw)
	if err != nil {
		return answer{Correction: err.Error()}
	}
	lvl, err := checkModelBriefing(decoded, taken, issued, tokensEnforced)
	if err != nil {
		if err == errAbstain {
			return answer{Model: decoded, Raw: raw, OK: true}
		}
		return answer{Correction: err.Error()}
	}
	lvlCopy := lvl
	return answer{Model: decoded, Suggestion: &lvlCopy, Raw: raw, OK: true}
}

// repairPrompt frames the single bounded retry: the schema error kind plus
// an instruction to re-emit the full object. No bundle content travels.
func repairPrompt(kind string) string {
	return "Your briefing object was rejected (" + kind + "). Re-emit the complete briefing object as strict JSON with exactly the documented fields."
}
