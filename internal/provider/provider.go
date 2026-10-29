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
// tokenized. Adapters extract it from their wire format; briefing-side
// validation decides whether it is usable.
type RawBriefing struct {
	JSON []byte
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

// responsesRequest is the Meta Responses API wire body, verified against
// the live endpoint: model, one user message, no streaming.
type responsesRequest struct {
	Model  string             `json:"model"`
	Input  []responsesMessage `json:"input"`
	Stream bool               `json:"stream"`
}

// briefingFieldContract is the output contract shared by the bundle and
// repair prompts. It mirrors modelBriefing (internal/briefing): exact
// fields, token references only. It names no values, so a hostile bundle
// cannot steer it; the briefing layer re-validates everything regardless.
const briefingFieldContract = `Respond with exactly one JSON object and no other text, with exactly these fields: "event_ids" (array of all the group's event IDs), "host" (one token copied from the bundle), "what_changed", "context", "model_suggestion" (one of "low", "medium", "high", "critical", or null to abstain), "priority_reason", "recommended_check". Use only tokens that appear in the bundle; invent no new ones.`

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
