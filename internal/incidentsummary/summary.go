package incidentsummary

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ostefani/watcherbridge/internal/briefing"
	"github.com/ostefani/watcherbridge/internal/redactor"
	"github.com/ostefani/watcherbridge/internal/store"
)

// Bounded limits (Plan incident-summary proposal, not Designv9 core).
// They are implementation choices: the snapshot states sampled/omitted
// counts explicitly and never presents a sample as complete evidence.
const (
	// maxSnapshotBytes caps the serialized provider-bound input.
	maxSnapshotBytes = 32 << 10
	// maxRawOutputBytes caps the raw model output.
	maxRawOutputBytes = 8 << 10
	// maxRestoredBytes caps the restored display output.
	maxRestoredBytes = 16 << 10
	// maxHypothesisRunes caps the labelled likely explanation.
	maxHypothesisRunes = 256
	// maxCitations caps returned member citations.
	maxCitations = 8
	// runDeadline bounds one initial send plus at most one repair.
	runDeadline = 30 * time.Second
	// pollTick bounds the worker poll when idle.
	pollTick = 5 * time.Second
)

// snapshotMember is one sampled member briefing as sent: opaque alias
// plus tokenized reported evidence. Raw briefing or event IDs never
// cross; only aliases issued for this snapshot do.
type snapshotMember struct {
	Alias      string `json:"alias"`
	BriefingID string `json:"briefing_id,omitempty"`
	Priority   string `json:"priority"`
	Kind       string `json:"kind"`
	CreatedAt  string `json:"created_at"`
	What       any    `json:"what_changed"`
	Context    any    `json:"context"`
	Check      any    `json:"recommended_check"`
}

// snapshot is the provider-bound input: trusted incident kind, time
// window, exact stored counts, scope limitations and selected member
// evidence with sampled/omitted counts. It is built from tokenized
// member texts and leak-scanned fail-closed immediately before
// sending.
type snapshot struct {
	IncidentID    string           `json:"incident_id"`
	Kind          string           `json:"kind"`
	SourceScope   string           `json:"source_scope"`
	Target        string           `json:"target"`
	Floor         string           `json:"floor"`
	WindowStart   string           `json:"window_start"`
	LastMemberAt  string           `json:"last_member_at"`
	MemberCount   int              `json:"member_count"`
	QualifiedRefs int              `json:"qualified_refs"`
	Sampled       int              `json:"sampled"`
	Omitted       int              `json:"omitted"`
	Limitation    string           `json:"scope_limitation"`
	Members       []snapshotMember `json:"members"`
}

// modelSummary is the strict incident-summary output schema: summary
// text, a labelled hypothesis, display confidence, an optional
// priority suggestion with its reason, and evidence citations. Unknown
// fields and trailing data are rejected; citations must name aliases
// issued for this snapshot.
type modelSummary struct {
	Summary    string            `json:"summary"`
	Hypothesis string            `json:"hypothesis"`
	Confidence string            `json:"confidence"`
	Suggestion *string           `json:"priority_suggestion"`
	Reason     string            `json:"priority_reason"`
	Citations  []json.RawMessage `json:"citations"`
}

type modelCitation struct {
	Member *string `json:"member"`
}

func decodeCitation(raw json.RawMessage) (modelCitation, error) {
	var c modelCitation
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return c, fmt.Errorf("unknown field in citation")
		}
		return c, fmt.Errorf("malformed citation")
	}
	if len(bytes.Trim(raw[dec.InputOffset():], " \t\r\n")) != 0 {
		return c, fmt.Errorf("trailing data after citation")
	}
	if c.Member == nil || *c.Member == "" {
		return c, fmt.Errorf("citation must name a member alias")
	}
	return c, nil
}

func decodeModelSummary(raw []byte) (modelSummary, error) {
	var m modelSummary
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return m, fmt.Errorf("unknown field")
		}
		return m, fmt.Errorf("malformed summary JSON")
	}
	if len(bytes.Trim(raw[dec.InputOffset():], " \t\r\n")) != 0 {
		return m, fmt.Errorf("trailing data after summary object")
	}
	if m.Summary == "" || m.Hypothesis == "" || m.Confidence == "" || m.Reason == "" || m.Citations == nil {
		return m, fmt.Errorf("missing or empty required field")
	}
	return m, nil
}

// acceptedSummary is one validated model answer with its resolver
// outcome: suggestion (nil = abstain), final priority, and whether the
// suggestion was clamped.
type acceptedSummary struct {
	Model      modelSummary
	Suggestion *briefing.Level
	Final      briefing.Level
	Clamped    bool
	Raw        []byte
	Summary    string
	Hypothesis string
}

// checkSummary enforces content rules: known confidence, bounded
// hypothesis, at most 8 citations each naming an issued alias, no
// invented tokens, and a known priority suggestion when present.
// tokensEnforced mirrors the loopback-only redaction exception: with
// redaction off raw values are the operator's explicit choice and only
// token-shaped inventions are still rejected.
func checkSummary(m modelSummary, aliases map[string]string, issued redactor.TokenMap, tokensEnforced bool) (*briefing.Level, error) {
	switch m.Confidence {
	case "low", "medium", "high":
	default:
		return nil, fmt.Errorf("confidence is not low, medium or high")
	}
	if len([]rune(m.Hypothesis)) > maxHypothesisRunes {
		return nil, fmt.Errorf("hypothesis exceeds 256 chars")
	}
	if len(m.Citations) > maxCitations {
		return nil, fmt.Errorf("too many citations")
	}
	for _, raw := range m.Citations {
		c, err := decodeCitation(raw)
		if err != nil {
			return nil, err
		}
		if _, ok := aliases[*c.Member]; !ok {
			return nil, fmt.Errorf("citation is not a snapshot member")
		}
	}
	joined := m.Summary + "\n" + m.Hypothesis + "\n" + m.Reason
	if err := redactor.ValidateTokens(joined, issued); err != nil {
		return nil, fmt.Errorf("invented tokens present")
	}
	_ = tokensEnforced
	if m.Suggestion == nil {
		return nil, nil
	}
	lvl, err := briefing.ParseLevel(*m.Suggestion)
	if err != nil {
		return nil, fmt.Errorf("priority_suggestion is not a known level")
	}
	return &lvl, nil
}

// BuildSnapshot selects at most 20 member briefings by the documented
// deterministic policy (priority rank descending, oldest first,
// briefing ID tiebreaker; the first row establishes the floor), bounds
// database reads and field sizes, tokenizes scope/target/identifiers
// and all sensitive free text including member summaries via the
// durable map-owned counter scheme, and enforces the 32 KiB
// provider-bound budget. It returns the exact bytes to send plus the
// issuance records. failReason is non-empty when the snapshot cannot
// be built; the caller then falls back deterministically.
func BuildSnapshot(ctx context.Context, st *store.Store, incident store.Incident, pol redactor.Policy) (raw []byte, issued, display redactor.TokenMap, aliasMap map[string]string, sampled, omitted int, failReason string) {
	members, omittedCount, err := st.SummaryMembers(ctx, incident.IncidentID, store.MaxSummaryMembersForSnapshot())
	if err != nil {
		return nil, nil, nil, nil, 0, 0, "summary members unreadable; no model summary"
	}
	if len(members) == 0 {
		return nil, nil, nil, nil, 0, 0, "incident has no member evidence; no model summary"
	}
	// Opaque citation aliases issued for this snapshot only.
	aliasMap = make(map[string]string, len(members))
	events := make([]redactor.InputEvent, 0, len(members))
	for i, m := range members {
		alias := fmt.Sprintf("M%d", i+1)
		aliasMap[alias] = m.BriefingID
		payload, err := json.Marshal(map[string]any{
			"priority": m.Priority, "kind": m.Kind, "created_at": m.CreatedAt.UTC().Format(time.RFC3339),
			"what_changed": m.What, "context": m.Context, "recommended_check": m.Check,
		})
		if err != nil {
			return nil, nil, nil, nil, 0, 0, "summary payload unencodable; no model summary"
		}
		events = append(events, redactor.InputEvent{EventID: alias, Payload: payload})
	}
	retainedSeeds := []string{incident.SourceScope}
	seedSet := map[string]struct{}{}
	for _, member := range members {
		for _, seed := range member.Seeds {
			if _, ok := seedSet[seed]; ok {
				continue
			}
			seedSet[seed] = struct{}{}
			retainedSeeds = append(retainedSeeds, seed)
			if len(retainedSeeds) > 200 {
				return nil, nil, nil, nil, 0, 0, "summary identity metadata exceeds redaction bounds; no model summary"
			}
		}
	}
	bundle, issued, display, err := redactor.BuildBundle(ctx, st, redactor.BundleInput{
		GroupID: incident.IncidentID, Anchor: incident.Target, Target: incident.Target,
		Baseline: incident.Floor, Events: events, SeedValues: retainedSeeds,
	}, pol)
	if err != nil {
		return nil, nil, nil, nil, 0, 0, "summary redaction failed; no model summary"
	}
	var tokenized []any
	if err := json.Unmarshal(bundle.EventsJSON, &tokenized); err != nil {
		return nil, nil, nil, nil, 0, 0, "summary redaction failed; no model summary"
	}
	snapMembers := make([]snapshotMember, 0, len(members))
	for i, m := range members {
		alias := fmt.Sprintf("M%d", i+1)
		var what, ctxText, check any
		if i < len(tokenized) {
			if obj, ok := tokenized[i].(map[string]any); ok {
				what, ctxText, check = obj["what_changed"], obj["context"], obj["recommended_check"]
			}
		}
		if what == nil {
			what = m.What
		}
		if ctxText == nil {
			ctxText = m.Context
		}
		if check == nil {
			check = m.Check
		}
		snapMembers = append(snapMembers, snapshotMember{
			Alias: alias, Priority: m.Priority, Kind: m.Kind,
			CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339),
			What:      what, Context: ctxText, Check: check,
		})
	}
	// The snapshot carries no raw network identifiers: the target rides
	// the tokenized bundle anchor (durable map-owned tokens), and the
	// reporting source rides a per-snapshot opaque alias — the leak
	// scan below would otherwise fail the raw values closed. The local
	// incident row keeps the exact scope for display; the alias is
	// snapshot-scoped and never restored. With redaction off (explicit
	// loopback-only exception) values pass raw, mirroring the briefing
	// path.
	target, source := bundle.Anchor, "S1"
	if !pol.Enabled {
		target, source = incident.Target, incident.SourceScope
	}
	snap := snapshot{
		IncidentID: incident.IncidentID, Kind: incident.Kind, SourceScope: source,
		Target: target, Floor: incident.Floor,
		WindowStart:  incident.WindowStart.UTC().Format(time.RFC3339),
		LastMemberAt: incident.LastMemberAt.UTC().Format(time.RFC3339),
		MemberCount:  incident.MemberCount, QualifiedRefs: incident.QualifiedRefs,
		Sampled: len(members), Omitted: omittedCount,
		Limitation: "single reporting source; sampled member evidence only; omitted members are not represented; hypotheses are labelled and never override the deterministic floor",
		Members:    snapMembers,
	}
	raw, err = json.Marshal(snap)
	if err != nil {
		return nil, nil, nil, nil, 0, 0, "summary encoding failed; no model summary"
	}
	if len(raw) > maxSnapshotBytes {
		return nil, nil, nil, nil, 0, 0, "summary snapshot exceeds 32 KiB; no model summary"
	}
	if pol.Enabled {
		aliases := make([]string, 0, len(aliasMap))
		for a := range aliasMap {
			aliases = append(aliases, a)
		}
		if hits, err := redactor.ScanOutboundLeaks(raw, issued, aliases); err != nil || len(hits) > 0 {
			return nil, nil, nil, nil, 0, 0, "summary redaction blocked the send; no model summary"
		}
	}
	return raw, issued, display, aliasMap, len(members), omittedCount, ""
}
