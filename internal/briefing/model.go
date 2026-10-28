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

// clean caps accepted model text the same way bundle strings are capped:
// controls stripped, rune-counted, truncation marked.
func clean(s string, maxChars int) string {
	return redactor.CapField(redactor.StripControls(s), maxChars)
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
		FallbackUsed:     true,
		WhatChanged:      describeEvents(taken),
		Context:          contextNote,
		RecommendedCheck: "Review the recent changes on this host.",
		CreatedAt:        now,
	}
}

// insertFallback stores the deterministic briefing; on storage error the
// group is released (or parked at the cap) — never silently dropped.
func insertFallback(ctx context.Context, d Deps, taken store.TakenGroup, outcome Outcome, contextNote string) {
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	b := fallbackBriefing(taken, outcome, now, contextNote)
	if err := d.Store.InsertBriefing(ctx, b); err != nil {
		log.Printf("briefing: group %s fallback insert: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
		releaseOrPark(ctx, d, taken, "fallback insert failed")
		return
	}
	notifyBriefing(d, taken, b)
	if len(outcome.Skipped) > 0 {
		log.Printf("briefing: group %s baseline %s skipped %d unknown-input rules", taken.GroupID, outcome.RuleID, len(outcome.Skipped))
	}
}

// briefWithModel runs the Step 11 pipeline for a taken group: bundle →
// leak scan → provider (+1 repair) → validate → resolve → restore →
// insert. Every terminal path stores exactly one briefing.
func briefWithModel(ctx context.Context, d Deps, taken store.TakenGroup, outcome Outcome) {
	if taken.LastAttempt() {
		// Final take: skip the provider and write the deterministic
		// briefing inline, so a poison group converges to done.
		insertFallback(ctx, d, taken, outcome, "retry cap reached; briefing from deterministic baseline only")
		return
	}
	timeout := d.ModelTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxChars := d.Redaction.MaxFieldChars

	events := make([]redactor.InputEvent, 0, len(taken.Events))
	for _, ev := range taken.Events {
		events = append(events, redactor.InputEvent{EventID: ev.EventID, EventType: ev.EventType, Payload: ev.Payload})
	}
	bundle, issued, err := redactor.BuildBundle(ctx, d.Store, redactor.BundleInput{
		GroupID:  taken.GroupID,
		Anchor:   taken.Anchor,
		Baseline: outcome.Level.String(),
		Events:   events,
	}, d.Redaction)
	if err != nil {
		log.Printf("briefing: group %s bundle build: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
		insertFallback(ctx, d, taken, outcome, "bundle build failed; briefing from deterministic baseline only")
		return
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		log.Printf("briefing: group %s bundle encode: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
		insertFallback(ctx, d, taken, outcome, "bundle encode failed; briefing from deterministic baseline only")
		return
	}
	if d.Redaction.Enabled {
		// Last step before anything leaves the machine: fail closed.
		// A hit aborts the provider call — no send, fallback instead.
		if hits, err := redactor.ScanOutboundLeaks(raw); err != nil || len(hits) > 0 {
			log.Printf("briefing: group %s redaction blocked at %v", taken.GroupID, hits)
			insertFallback(ctx, d, taken, outcome, "redaction blocked the model call; briefing from deterministic baseline only")
			return
		}
	}

	modelCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	answer, err := d.Provider.Brief(modelCtx, raw, issued)
	if err != nil {
		log.Printf("briefing: group %s provider: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
		insertFallback(ctx, d, taken, outcome, "model call failed; briefing from deterministic baseline only")
		return
	}
	a := acceptAnswer(answer.JSON, taken, issued, d.Redaction.Enabled)
	if !a.OK {
		repair, err := d.Provider.Repair(modelCtx, repairPrompt(a.Correction), issued)
		if err != nil {
			log.Printf("briefing: group %s repair: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
			insertFallback(ctx, d, taken, outcome, "model repair failed; briefing from deterministic baseline only")
			return
		}
		if a = acceptAnswer(repair.JSON, taken, issued, d.Redaction.Enabled); !a.OK {
			log.Printf("briefing: group %s model invalid after repair (%s)", taken.GroupID, a.Correction)
			insertFallback(ctx, d, taken, outcome, "model output invalid; briefing from deterministic baseline only")
			return
		}
	}
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
	b := store.Briefing{
		BriefingID:       "brf-" + taken.GroupID,
		GroupID:          taken.GroupID,
		HostAnchorKey:    taken.Anchor,
		Baseline:         outcome.Level.String(),
		ModelSuggestion:  sug,
		Priority:         final.String(),
		PriorityReason:   clean(m.Reason, maxChars),
		BaselineRule:     outcome.RuleID,
		Clamped:          clamped,
		FallbackUsed:     false,
		WhatChanged:      clean(redactor.Restore(m.What, issued), maxChars),
		Context:          clean(redactor.Restore(m.Context, issued), maxChars),
		RecommendedCheck: clean(redactor.Restore(m.Check, issued), maxChars),
		ModelRawJSON:     string(rawJSON),
		CreatedAt:        now,
	}
	if err := d.Store.InsertBriefing(ctx, b); err != nil {
		log.Printf("briefing: group %s insert: %s", taken.GroupID, store.SanitizeErrorText(err.Error()))
		releaseOrPark(ctx, d, taken, "insert failed")
		return
	}
	notifyBriefing(d, taken, b)
	if len(outcome.Skipped) > 0 {
		log.Printf("briefing: group %s baseline %s skipped %d unknown-input rules", taken.GroupID, outcome.RuleID, len(outcome.Skipped))
	}
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
