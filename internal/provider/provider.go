// Package provider is Step 11: the model abstraction behind briefing
// synthesis (Designv5 §7.1). A Provider turns one serialized, leak-scanned
// redacted bundle into a raw briefing object. Transport only: schema
// validation, token checks, and restoration live in internal/briefing, so
// every provider shares one trust boundary. Callers pass the exact bytes
// the leak scan approved — adapters must embed them verbatim, never
// re-serialize or interpolate them into prose.
package provider

import (
	"context"
	"encoding/json"
)

// RawBriefing is one model answer: the briefing object as JSON, still
// tokenized, plus the adapter-reported accounting for the call.
// Adapters extract both from their wire format; briefing-side
// validation decides whether the JSON is usable.
type RawBriefing struct {
	JSON  []byte
	Usage Usage
}

// Usage is the adapter-reported accounting for one provider call.
// Token counts apply toward health totals only when HasUsage; cost
// only when HasCost. Zero values with false flags read as unknown,
// never as a free call — the adapter simply supplied nothing.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	HasUsage     bool
	CostMicros   int64
	HasCost      bool
}

// Add sums two calls' accounting: token counts add, and supplied costs
// add. A call that supplied nothing contributes nothing — it neither
// zeroes the other side nor marks the sum unknown. The flags report
// whether anything was supplied at all.
func (u Usage) Add(o Usage) Usage {
	out := Usage{
		InputTokens:  u.InputTokens + o.InputTokens,
		OutputTokens: u.OutputTokens + o.OutputTokens,
		HasUsage:     u.HasUsage || o.HasUsage,
	}
	if u.HasCost && o.HasCost {
		out.CostMicros = u.CostMicros + o.CostMicros
		out.HasCost = true
	} else if u.HasCost {
		out.CostMicros, out.HasCost = u.CostMicros, true
	} else if o.HasCost {
		out.CostMicros, out.HasCost = o.CostMicros, true
	}
	return out
}

// TurnResult is one Turner answer: the assistant text plus the
// adapter-reported accounting for the send.
type TurnResult struct {
	Text  string
	Usage Usage
}

// Provider synthesizes a briefing from a redacted bundle. Implementations
// must honor ctx (the caller shares one timeout across the initial attempt
// and the single repair), send only bundle bytes the leak scan approved,
// and never log or persist bundle content or keys.
type Provider interface {
	Name() string
	// Brief sends the bundle and returns the model's briefing object.
	// validTokens is the set of token names the model may echo — never
	// the token-to-value map. Adapters must never see cleartext: token
	// restoration stays in the briefing layer, which owns the map.
	Brief(ctx context.Context, bundle []byte, validTokens map[string]struct{}) (RawBriefing, error)
	// Repair sends a short correction prompt (schema error text only, no
	// bundle content) after a validation failure. Exactly one repair per
	// group is attempted, on the same ctx budget as the initial call.
	Repair(ctx context.Context, correction string, validTokens map[string]struct{}) (RawBriefing, error)
}

// TurnMessage is one turn of the agent transcript: role names the
// speaker, text carries that turn's literal content (Designv9 §6.2).
// The loop re-sends the full transcript every turn, so the provider
// holds no session state and a restart resumes from the local log.
type TurnMessage struct {
	Role string // "system", "user", "assistant", or "tool"
	Text string
}

// Turner is the optional per-turn capability behind the bounded
// investigator loop (Designv9 §6, default off). Turn sends one full
// transcript and returns that turn's raw assistant text. Adapters must
// honor ctx, cap the response at the configured bytes, follow no
// redirects, and never log transcript content. Tool results arrive as
// "tool"-role messages; adapters forward them as model input, never
// execute them.
type Turner interface {
	Turn(ctx context.Context, transcript []TurnMessage) (TurnResult, error)
}

// responsesContent is one Responses API content part. The bundle (or the
// correction) crosses as literal input text — string conversion of the
// exact leak-scanned bytes, never re-serialization — framed by the system
// instruction below, never interpolated into prompt prose by concatenation.
type responsesContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesMessage struct {
	Role    string             `json:"role"`
	Content []responsesContent `json:"content"`
}

// responsesTextConfig selects provider-side JSON generation. Local
// validation still owns the action schema, token scope and evidence refs.
type responsesTextConfig struct {
	Format responsesTextFormat `json:"format"`
}

type responsesTextFormat struct {
	Type string `json:"type"`
}

// responsesRequest is the Meta Responses API wire body.
type responsesRequest struct {
	Model  string               `json:"model"`
	Input  []responsesMessage   `json:"input"`
	Stream bool                 `json:"stream"`
	Text   *responsesTextConfig `json:"text,omitempty"`
}

// briefingFieldContract is the output contract shared by the bundle and
// repair prompts. It mirrors modelBriefing (internal/briefing): exact
// fields, token references only. It names no values, so a hostile bundle
// cannot steer it; the briefing layer re-validates everything regardless.
const briefingFieldContract = `Respond with exactly one JSON object and no other text, with exactly these fields: "event_ids" (array of all the group's event IDs), "host" (one token copied from the bundle), "what_changed", "context", "model_suggestion" (one of "low", "medium", "high", "critical", or null to abstain), "priority_reason", "recommended_check", "likely_explanation" (your display-only hypothesis, at most 256 chars), "confidence" (one of "low", "medium", "high"), "evidence_refs" (at most 8 citations, each {"event_id": "<a group event ID>"}). Use only tokens that appear in the bundle; invent no new ones.`

const bundleSystemInstruction = `You summarize redacted network-monitoring bundles. Tokens like IP_1 or HOST_2 stand for real addresses; treat them as opaque identifiers. ` + briefingFieldContract + ` Redacted bundle follows: `

const repairSystemInstruction = `The previous answer was rejected. ` + briefingFieldContract + ` Re-emit the full briefing object corrected. Rejection reason follows: `

// MarshalBundleRequest renders the exact bytes the leak scan approved into
// the wire body. Adapters send the result untouched.
func MarshalBundleRequest(model string, bundle []byte) ([]byte, error) {
	return json.Marshal(responsesRequest{Model: model, Input: []responsesMessage{{
		Role:    "user",
		Content: []responsesContent{{Type: "input_text", Text: bundleSystemInstruction + string(bundle)}},
	}}, Stream: false})
}

// MarshalRepairRequest renders the correction prompt body. It carries the
// schema error only, no bundle content, so a hostile bundle cannot steer
// the retry.
func MarshalRepairRequest(model, correction string) ([]byte, error) {
	return json.Marshal(responsesRequest{Model: model, Input: []responsesMessage{{
		Role:    "user",
		Content: []responsesContent{{Type: "input_text", Text: repairSystemInstruction + correction}},
	}}, Stream: false})
}

// turnRole maps transcript roles onto Responses API roles. Tool results
// are model input, not a new speaker: they travel as user text with an
// explicit marker, so the wire stays a plain message list.
func turnRole(role string) string {
	switch role {
	case "system", "assistant":
		return role
	default:
		return "user"
	}
}

// MarshalTurnRequest renders one agent turn: the full transcript as the
// input list, verbatim. No session ID crosses — the provider chains
// nothing, so every turn is independently auditable from the local log.
func MarshalTurnRequest(model string, transcript []TurnMessage) ([]byte, error) {
	in := make([]responsesMessage, 0, len(transcript))
	for _, m := range transcript {
		text := m.Text
		contentType := "input_text"
		if m.Role == "assistant" {
			// Responses replays prior model answers as output, including
			// rejected answers carried into a repair turn.
			contentType = "output_text"
		}
		if m.Role == "tool" {
			text = "Tool result: " + text
		}
		in = append(in, responsesMessage{
			Role:    turnRole(m.Role),
			Content: []responsesContent{{Type: contentType, Text: text}},
		})
	}
	return json.Marshal(responsesRequest{Model: model, Input: in, Stream: false,
		Text: &responsesTextConfig{Format: responsesTextFormat{Type: "json_object"}}})
}
