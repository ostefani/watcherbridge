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
	Brief(ctx context.Context, bundle []byte, issued map[string]string) (RawBriefing, error)
	// Repair sends a short correction prompt (schema error text only, no
	// bundle content) after a validation failure. Exactly one repair per
	// group is attempted, on the same ctx budget as the initial call.
	Repair(ctx context.Context, correction string, issued map[string]string) (RawBriefing, error)
}

// bundleRequest is the Meta wire body: the model ID plus the redacted
// bundle as raw JSON. The bundle is embedded verbatim — it is data the
// model summarizes, framed by the adapter's system instruction (Designv5
// §9.1), never interpolated into prompt prose by string concatenation.
type bundleRequest struct {
	Model  string          `json:"model"`
	Bundle json.RawMessage `json:"bundle"`
}

// repairRequest is the single bounded repair: the schema error plus an
// instruction to re-emit the full briefing object. It carries no bundle
// content, so a hostile bundle cannot steer the retry.
type repairRequest struct {
	Model      string `json:"model"`
	Correction string `json:"correction"`
}

// MarshalBundleRequest renders the exact bytes the leak scan approved into
// the wire body. Adapters send the result untouched.
func MarshalBundleRequest(model string, bundle []byte) ([]byte, error) {
	return json.Marshal(bundleRequest{Model: model, Bundle: bundle})
}

// MarshalRepairRequest renders the correction prompt body.
func MarshalRepairRequest(model, correction string) ([]byte, error) {
	return json.Marshal(repairRequest{Model: model, Correction: correction})
}
