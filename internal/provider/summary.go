package provider

import (
	"context"
	"encoding/json"
)

// RawSummary is one incident-summary answer: the summary object as JSON,
// still tokenized, plus the adapter-reported accounting for the call.
// Adapters extract both from their wire format; validation, citation
// checks and restoration live outside the provider, so every provider
// shares one trust boundary.
type RawSummary struct {
	JSON  []byte
	Usage Usage
}

// Summarizer is the explicit incident-summary capability behind the
// closed-incident summary (Designv9 §7, default off). It reuses the
// existing provider transport, endpoint security, cancellation,
// response limits and accounting. The snapshot bytes are the exact
// leak-scanned incident payload — adapters embed them verbatim, never
// re-serialize or interpolate them into prose. Repair carries a short
// correction prompt (safe validation classification only, no snapshot
// content and no rejected output) after a validation failure. Exactly
// one repair per incident is attempted, on the same run budget as the
// initial call. No tools execute and no scanner is accessed.
type Summarizer interface {
	Summarize(ctx context.Context, snapshot []byte, validTokens map[string]struct{}) (RawSummary, error)
	RepairSummary(ctx context.Context, correction string, validTokens map[string]struct{}) (RawSummary, error)
}

// incidentSummaryFieldContract is the output contract for the
// closed-incident snapshot and its repair. It names tokenized incident
// fields only: summary text, a labelled hypothesis, display
// confidence, an optional priority suggestion with its reason, and
// evidence citations. It names no values, so a hostile snapshot
// cannot steer it; the summary layer re-validates everything
// regardless.
const incidentSummaryFieldContract = `Respond with exactly one JSON object and no other text, with exactly these fields: "summary" (what the incident's reported evidence shows), "hypothesis" (one labelled likely explanation, at most 256 chars), "confidence" (one of "low", "medium" or "high"), "priority_suggestion" (one of "low", "medium", "high", "critical" or null to abstain), "priority_reason" (why, or why no raise), "citations" (at most 8 aliases, each {"member": "<a snapshot member alias>"}). Use only tokens and member aliases that appear in the snapshot; invent no new ones.`

const incidentSummarySystemInstruction = `You summarize a closed network-monitoring incident from redacted reported evidence. Tokens like IP_1 or HOST_2 stand for real addresses; member aliases like M1 stand for member briefings; treat both as opaque identifiers. ` + incidentSummaryFieldContract + ` Snapshot follows: `

const incidentSummaryRepairInstruction = `The previous incident-summary answer was rejected. ` + incidentSummaryFieldContract + ` Re-emit the full summary object corrected. Rejection reason follows (a safe validation classification only): `

// MarshalSummaryRequest renders the exact leak-scanned snapshot bytes
// into the wire body. Adapters send the result untouched.
func MarshalSummaryRequest(model string, snapshot []byte) ([]byte, error) {
	return json.Marshal(responsesRequest{Model: model, Input: []responsesMessage{{
		Role:    "user",
		Content: []responsesContent{{Type: "input_text", Text: incidentSummarySystemInstruction + string(snapshot)}},
	}}, Stream: false})
}

// MarshalSummaryRepairRequest renders the correction prompt body. It
// carries the validation classification only, no snapshot content and
// no rejected output, so a hostile snapshot cannot steer the retry.
func MarshalSummaryRepairRequest(model, correction string) ([]byte, error) {
	return json.Marshal(responsesRequest{Model: model, Input: []responsesMessage{{
		Role:    "user",
		Content: []responsesContent{{Type: "input_text", Text: incidentSummaryRepairInstruction + correction}},
	}}, Stream: false})
}
