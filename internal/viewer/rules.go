package viewer

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ostefani/watcherbridge/internal/briefing"
	"github.com/ostefani/watcherbridge/internal/store"
)

// Review-policy rule routes (Designv9 §5.2, Plan Step 28). Proposals
// derive deterministically from operator judgments; approve/dismiss/
// revoke move one scope through its lifecycle. Guards, strict bodies
// and status codes mirror the decision routes: unknown IDs 404,
// declined/active conflicts 409, malformed input 400. Rules never
// leave the machine — no route here forwards anything upstream.

// maxProposalsPage caps one proposal listing: derivation pages its
// input, but the listing itself stays a bounded operator answer. A
// full page may have more; approve by ID works regardless.
const maxProposalsPage = 100

type proposalWire struct {
	ProposalID             string `json:"proposal_id"`
	SourceScope            string `json:"source_scope"`
	ReferenceClass         string `json:"reference_class"`
	IdentityNamespace      string `json:"identity_namespace"`
	UpstreamReference      string `json:"upstream_reference"`
	ReportedIdentitySource string `json:"reported_identity_source"`
	ReportedConfidence     string `json:"reported_confidence"`
	Kind                   string `json:"kind"`
	PortsCSV               string `json:"ports_csv"`
	MatchingBriefings      int    `json:"matching_briefings"`
}

func proposalWireOf(p briefing.Proposal) proposalWire {
	return proposalWire{
		ProposalID: p.ProposalID, SourceScope: p.SourceScope, ReferenceClass: p.ReferenceClass,
		IdentityNamespace: p.IdentityNamespace, UpstreamReference: p.UpstreamReference,
		ReportedIdentitySource: p.ReportedIdentitySource, ReportedConfidence: p.ReportedConfidence,
		Kind: p.Kind, PortsCSV: p.PortsCSV, MatchingBriefings: p.MatchingBriefings,
	}
}

type ruleWire struct {
	RuleID                 string  `json:"rule_id"`
	ProposalID             string  `json:"proposal_id"`
	SourceScope            string  `json:"source_scope"`
	ReferenceClass         string  `json:"reference_class"`
	IdentityNamespace      string  `json:"identity_namespace"`
	UpstreamReference      string  `json:"upstream_reference"`
	ReportedIdentitySource string  `json:"reported_identity_source"`
	ReportedConfidence     string  `json:"reported_confidence"`
	Kind                   string  `json:"kind"`
	PortsCSV               string  `json:"ports_csv"`
	Status                 string  `json:"status"`
	DecidedAt              string  `json:"decided_at"`
	ApprovedAt             *string `json:"approved_at,omitempty"`
	ExpiresAt              *string `json:"expires_at,omitempty"`
}

func ruleWireOf(r store.Rule) ruleWire {
	w := ruleWire{
		RuleID: r.RuleID, ProposalID: r.ProposalID,
		SourceScope: r.SourceScope, ReferenceClass: r.ReferenceClass,
		IdentityNamespace: r.IdentityNamespace, UpstreamReference: r.UpstreamReference,
		ReportedIdentitySource: r.ReportedIdentitySource, ReportedConfidence: r.ReportedConfidence,
		Kind: r.Kind, PortsCSV: r.PortsCSV, Status: r.Status,
		DecidedAt: r.DecidedAt.UTC().Format(time.RFC3339),
	}
	if r.HasApprovedAt {
		s := r.ApprovedAt.UTC().Format(time.RFC3339)
		w.ApprovedAt = &s
	}
	if r.HasExpiresAt {
		s := r.ExpiresAt.UTC().Format(time.RFC3339)
		w.ExpiresAt = &s
	}
	return w
}

// ruleMutationWire answers approve/dismiss/revoke: the record plus
// whether this call changed state. 201 on a change, 200 with
// applied:false on an idempotent replay — the decision-route shape.
type ruleMutationWire struct {
	Rule    ruleWire `json:"rule"`
	Applied bool     `json:"applied"`
}

func (v *Viewer) proposeAfter() int {
	if v.RuleProposeAfter > 0 {
		return v.RuleProposeAfter
	}
	return 3
}

func (v *Viewer) ruleDefaultTTL() time.Duration {
	if v.RuleDefaultTTL > 0 {
		return v.RuleDefaultTTL
	}
	return 90 * 24 * time.Hour
}

// listProposals serves GET /api/rules/proposals: the currently
// actionable deterministic proposals, oldest scope first.
func (v *Viewer) listProposals(w http.ResponseWriter, r *http.Request) {
	proposals, err := briefing.DeriveProposals(r.Context(), v.store, v.proposeAfter(), time.Now())
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]proposalWire, 0, len(proposals))
	for _, p := range proposals {
		if len(out) >= maxProposalsPage {
			break
		}
		out = append(out, proposalWireOf(p))
	}
	writeJSON(w, out)
}

// approveRule serves POST /api/rules/approve {"proposal_id","ttl?"}:
// ttl absent means the configured default, null means no expiry, and
// 1..365 are days. Unknown proposals 404, declined scopes 409.
func (v *Viewer) approveRuleGuarded(w http.ResponseWriter, r *http.Request) {
	v.guardStateChanging(v.approveRule)(w, r)
}

func (v *Viewer) approveRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProposalID string          `json:"proposal_id"`
		TTL        json.RawMessage `json:"ttl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeViewerError(w, "invalid rule body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.ProposalID) == "" {
		writeViewerError(w, "proposal_id is required", http.StatusBadRequest)
		return
	}
	ttlDays, ok := parseRuleTTL(body.TTL)
	if !ok {
		writeViewerError(w, "ttl must be absent, null or 1..365 days", http.StatusBadRequest)
		return
	}
	rule, applied, err := briefing.ApproveProposal(r.Context(), v.store, body.ProposalID,
		v.proposeAfter(), ttlDays, v.ruleDefaultTTL(), time.Now())
	if err != nil {
		writeRuleError(w, err)
		return
	}
	if applied {
		w.WriteHeader(http.StatusCreated)
	}
	writeJSON(w, ruleMutationWire{Rule: ruleWireOf(rule), Applied: applied})
}

// parseRuleTTL interprets the approve ttl: nil (absent) means the
// configured default (sentinel 0), JSON null means no expiry (-1),
// and an integral 1..365 means days. Anything else is invalid.
func parseRuleTTL(raw json.RawMessage) (int, bool) {
	if raw == nil {
		return 0, true
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return -1, true
	}
	var days float64
	if err := json.Unmarshal(raw, &days); err != nil {
		return 0, false
	}
	if days < 1 || days > 365 || days != float64(int(days)) {
		return 0, false
	}
	return int(days), true
}

// dismissProposal serves POST /api/rules/dismiss {"proposal_id"}:
// declining a proposal is idempotent; declining an active grant is a
// 409 (revoke ends automation, dismiss declines proposals).
func (v *Viewer) dismissProposalGuarded(w http.ResponseWriter, r *http.Request) {
	v.guardStateChanging(v.dismissProposal)(w, r)
}

func (v *Viewer) dismissProposal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProposalID string `json:"proposal_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeViewerError(w, "invalid rule body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.ProposalID) == "" {
		writeViewerError(w, "proposal_id is required", http.StatusBadRequest)
		return
	}
	rule, applied, err := briefing.DismissProposal(r.Context(), v.store, body.ProposalID, v.proposeAfter(), time.Now())
	if err != nil {
		writeRuleError(w, err)
		return
	}
	if applied {
		w.WriteHeader(http.StatusCreated)
	}
	writeJSON(w, ruleMutationWire{Rule: ruleWireOf(rule), Applied: applied})
}

// revokeRule serves POST /api/rules/revoke {"rule_id"}: ending a grant
// is idempotent; unknown IDs 404.
func (v *Viewer) revokeRuleGuarded(w http.ResponseWriter, r *http.Request) {
	v.guardStateChanging(v.revokeRule)(w, r)
}

func (v *Viewer) revokeRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RuleID string `json:"rule_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeViewerError(w, "invalid rule body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.RuleID) == "" {
		writeViewerError(w, "rule_id is required", http.StatusBadRequest)
		return
	}
	rule, applied, err := briefing.RevokeRule(r.Context(), v.store, body.RuleID, time.Now())
	if err != nil {
		writeRuleError(w, err)
		return
	}
	if applied {
		w.WriteHeader(http.StatusCreated)
	}
	writeJSON(w, ruleMutationWire{Rule: ruleWireOf(rule), Applied: applied})
}

// rule errors map to statuses like decision errors: unknown IDs 404,
// declined/active conflicts 409, malformed input 400.
func writeRuleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrRuleUnknown):
		writeViewerError(w, "unknown rule proposal", http.StatusNotFound)
	case errors.Is(err, store.ErrRuleDismissed), errors.Is(err, store.ErrRuleActive):
		writeViewerError(w, "rule conflict", http.StatusConflict)
	case errors.Is(err, store.ErrRuleInvalid):
		writeViewerError(w, "invalid rule request", http.StatusBadRequest)
	default:
		writeError(w, err)
	}
}
