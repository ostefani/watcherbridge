package briefing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

// Review-policy rules (Designv9 §5.2, Plan Step 28). The engine derives
// deterministic proposals from operator judgments and applies approved
// rules as an explicit deterministic baseline policy. Everything here
// fails closed: undecodable input, ambiguous subjects, unknown
// provenance and expired grants all read as "no rule", never as trust.

// RuleScope is the exact scope a rule automates: one upstream subject
// under one source with the change kind and canonical ports the
// operator judged expected. Matching is byte equality on every field;
// nothing is inferred, borrowed or approximated.
type RuleScope struct {
	SourceScope            string
	ReferenceClass         string
	IdentityNamespace      string
	UpstreamReference      string
	ReportedIdentitySource string
	ReportedConfidence     string
	Kind                   string
	PortsCSV               string
}

// canonical renders the scope unambiguously for hashing: a JSON array
// in fixed field order, so no separator collision can merge two scopes.
func (s RuleScope) canonical() string {
	raw, err := json.Marshal([]string{s.SourceScope, s.ReferenceClass, s.IdentityNamespace,
		s.UpstreamReference, s.ReportedIdentitySource, s.ReportedConfidence, s.Kind, s.PortsCSV})
	if err != nil {
		return strings.Join([]string{s.SourceScope, s.ReferenceClass, s.IdentityNamespace,
			s.UpstreamReference, s.ReportedIdentitySource, s.ReportedConfidence, s.Kind, s.PortsCSV}, "\x1f")
	}
	return string(raw)
}

func scopeHash(s RuleScope) string {
	sum := sha256.Sum256([]byte(s.canonical()))
	return hex.EncodeToString(sum[:])[:16]
}

// ProposalID names the deterministic proposal for a scope; RuleID names
// the rule approving it yields. Same scope, same IDs, every run.
func (s RuleScope) ProposalID() string { return "OP_PROP_" + scopeHash(s) }
func (s RuleScope) RuleID() string     { return "OP_RULE_" + scopeHash(s) }

var ruleIDRe = regexp.MustCompile(`^OP_(PROP|RULE)_[0-9a-f]{16}$`)

// checkRuleID rejects malformed proposal/rule IDs: 400 at the API,
// never a silent miss.
func checkRuleID(id string) error {
	if !ruleIDRe.MatchString(id) {
		return fmt.Errorf("%w: malformed rule id", store.ErrRuleInvalid)
	}
	return nil
}

// Proposal is one actionable operator judgment: the scope at least
// proposeAfter distinct briefings called expected, with no active rule
// and no explicit decline standing.
type Proposal struct {
	ProposalID string
	RuleScope
	MatchingBriefings int
}

// canonicalPorts reduces decoded group events to the canonical port
// set: sorted unique proto/port/state triples over each event's
// effective view (current, else host view, else previous — the same
// view the baseline reads). Empty when no event reports ports. Both
// proposal derivation and application decode the same retained bytes
// through this one function, so the two sides cannot drift.
func canonicalPorts(events []GroupEvent) string {
	seen := map[string]bool{}
	var out []string
	for _, e := range events {
		view := e.effective()
		if view == nil {
			continue
		}
		for _, p := range view.Ports {
			key := p.Protocol + "/" + fmt.Sprint(p.Number) + "/" + p.State
			if !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// liveSubjectScope loads one group's subject scope: every linked
// subject must share one identical reported_device scope with known
// identity standing. Anything else — no links, mixed scopes, a
// non-device class, an empty source or unknown confidence — reads as
// no scope, so rules never fire on ambiguity or incomplete provenance.
func liveSubjectScope(ctx context.Context, st *store.Store, groupID string) (RuleScope, bool, error) {
	var zero RuleScope
	links, _, err := st.GroupSubjects(ctx, groupID)
	if err != nil || len(links) == 0 {
		return zero, false, err
	}
	scope := RuleScope{}
	for i, link := range links {
		sub, err := st.ReportedProfile(ctx, link.ProfileID)
		if err != nil {
			return zero, false, err
		}
		part := RuleScope{
			SourceScope: sub.SourceScope, ReferenceClass: sub.ReferenceClass,
			IdentityNamespace: sub.IdentityNamespace, UpstreamReference: sub.UpstreamReference,
			ReportedIdentitySource: sub.ReportedIdentitySource, ReportedConfidence: sub.ReportedConfidence,
		}
		part.Kind, part.PortsCSV = scope.Kind, scope.PortsCSV
		if i == 0 {
			scope = part
			continue
		}
		if part != scope {
			return zero, false, nil
		}
	}
	if scope.ReferenceClass != store.SubjectReportedDevice || scope.SourceScope == "" ||
		scope.ReportedIdentitySource == "" ||
		(scope.ReportedConfidence != "low" && scope.ReportedConfidence != "high") {
		return zero, false, nil
	}
	return scope, true, nil
}

// scopeForBriefing derives one retained briefing's rule scope: the
// stored change kind plus the canonical ports over its retained group
// payloads decoded exactly as the briefing path decoded them. Missing
// payloads or ambiguous subjects fail closed — the briefing simply
// contributes no scope.
func scopeForBriefing(ctx context.Context, st *store.Store, b store.Briefing) (RuleScope, bool, error) {
	var zero RuleScope
	scope, ok, err := liveSubjectScope(ctx, st, b.GroupID)
	if err != nil || !ok {
		return zero, false, err
	}
	payloads, err := st.GroupPayloads(ctx, b.GroupID)
	if err != nil || len(payloads) == 0 {
		return zero, false, err
	}
	events := make([]GroupEvent, 0, len(payloads))
	for _, p := range payloads {
		ev := DecodeEvent(p.EventType, p.Payload)
		// Undecodable input voids the scope: an event without a
		// usable view is incomplete provenance, never an empty port
		// set that could match a genuinely portless scope.
		if ev.effective() == nil {
			return zero, false, nil
		}
		events = append(events, ev)
	}
	scope.Kind = b.Kind
	scope.PortsCSV = canonicalPorts(events)
	return scope, true, nil
}

// DeriveProposals collects the actionable scopes: distinct briefings
// whose latest lane state is expected (undone and overruled judgments
// excluded, stale audit excluded), grouped by exact scope, keeping
// scopes with at least proposeAfter briefings. Scopes with an active
// grant or an explicit decline are not proposals; revoked scopes
// re-propose (the evidence still stands, the automation stopped).
func DeriveProposals(ctx context.Context, st *store.Store, proposeAfter int, now time.Time) ([]Proposal, error) {
	if proposeAfter < 1 {
		return nil, fmt.Errorf("%w: propose-after must be positive", store.ErrRuleInvalid)
	}
	var ids []string
	seenBriefing := map[string]bool{}
	var afterRowID int64
	for {
		page, err := st.ExpectedBriefingDecisions(ctx, afterRowID, 500)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, d := range page {
			afterRowID = d.RowID
			if !seenBriefing[d.BriefingID] {
				seenBriefing[d.BriefingID] = true
				ids = append(ids, d.BriefingID)
			}
		}
		if len(page) < 500 {
			break
		}
	}
	counts := map[RuleScope]map[string]bool{}
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		states, err := st.EffectiveReview(ctx, store.DecisionSubjectBriefing, ids[start:end], now)
		if err != nil {
			return nil, err
		}
		for _, id := range ids[start:end] {
			if states[id].Kind != store.DecisionExpected {
				continue
			}
			b, err := st.BriefingByID(ctx, id)
			if err != nil {
				continue
			}
			scope, ok, err := scopeForBriefing(ctx, st, b)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if counts[scope] == nil {
				counts[scope] = map[string]bool{}
			}
			counts[scope][id] = true
		}
	}
	var out []Proposal
	for scope, briefings := range counts {
		if len(briefings) < proposeAfter {
			continue
		}
		row, err := st.GetRuleByProposal(ctx, scope.ProposalID())
		if err == nil {
			if row.Status == store.RuleDismissed {
				continue
			}
			if row.Status == store.RuleApproved && !row.Expired(now) {
				continue
			}
		}
		out = append(out, Proposal{ProposalID: scope.ProposalID(), RuleScope: scope, MatchingBriefings: len(briefings)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProposalID < out[j].ProposalID })
	return out, nil
}

// resolveScope resolves a proposal ID to its scope plus any stored
// lifecycle row. Live derivation answers first; scopes that left
// derivation (active grants, declined markers) resolve against their
// stored row instead, so repeat approval and repeat dismissal keep
// working after the proposal stops listing. Unknown or malformed IDs
// never resolve.
func resolveScope(ctx context.Context, st *store.Store, proposalID string, proposeAfter int, now time.Time) (RuleScope, *store.Rule, error) {
	var zero RuleScope
	if err := checkRuleID(proposalID); err != nil {
		return zero, nil, err
	}
	if !strings.HasPrefix(proposalID, "OP_PROP_") {
		return zero, nil, fmt.Errorf("%w: not a proposal id", store.ErrRuleInvalid)
	}
	proposals, err := DeriveProposals(ctx, st, proposeAfter, now)
	if err != nil {
		return zero, nil, err
	}
	for _, p := range proposals {
		if p.ProposalID == proposalID {
			row, rerr := st.GetRuleByProposal(ctx, proposalID)
			if rerr != nil {
				return p.RuleScope, nil, nil
			}
			return p.RuleScope, &row, nil
		}
	}
	row, err := st.GetRuleByProposal(ctx, proposalID)
	if err != nil {
		return zero, nil, store.ErrRuleUnknown
	}
	return RuleScope{
		SourceScope: row.SourceScope, ReferenceClass: row.ReferenceClass,
		IdentityNamespace: row.IdentityNamespace, UpstreamReference: row.UpstreamReference,
		ReportedIdentitySource: row.ReportedIdentitySource, ReportedConfidence: row.ReportedConfidence,
		Kind: row.Kind, PortsCSV: row.PortsCSV,
	}, &row, nil
}

// approveTTL resolves the grant length: ttlDays < 0 means none
// (never expires), 0 means the configured default, 1..365 are days.
// Anything else is a 400.
func approveTTL(ttlDays int, defaultTTL time.Duration, now time.Time) (expires time.Time, hasExpires bool, err error) {
	switch {
	case ttlDays < 0:
		return time.Time{}, false, nil
	case ttlDays == 0:
		if defaultTTL <= 0 {
			return time.Time{}, false, fmt.Errorf("%w: default ttl must be positive", store.ErrRuleInvalid)
		}
		return now.Add(defaultTTL), true, nil
	case ttlDays >= 1 && ttlDays <= 365:
		return now.Add(time.Duration(ttlDays) * 24 * time.Hour), true, nil
	default:
		return time.Time{}, false, fmt.Errorf("%w: ttl must be none, default or 1..365 days", store.ErrRuleInvalid)
	}
}

func ruleRow(scope RuleScope, status string, now time.Time) store.Rule {
	return store.Rule{
		RuleID: scope.RuleID(), ProposalID: scope.ProposalID(),
		SourceScope: scope.SourceScope, ReferenceClass: scope.ReferenceClass,
		IdentityNamespace: scope.IdentityNamespace, UpstreamReference: scope.UpstreamReference,
		ReportedIdentitySource: scope.ReportedIdentitySource, ReportedConfidence: scope.ReportedConfidence,
		Kind: scope.Kind, PortsCSV: scope.PortsCSV,
		Status: status, DecidedAt: now,
	}
}

// ApproveProposal approves the deterministic proposal: unknown IDs
// 404, declined scopes 409, and repeated approval returns the existing
// grant. Revoked and lapsed grants re-approve under the same IDs —
// the proposal still stands, only the automation stopped.
func ApproveProposal(ctx context.Context, st *store.Store, proposalID string, proposeAfter, ttlDays int, defaultTTL time.Duration, now time.Time) (store.Rule, bool, error) {
	scope, row, err := resolveScope(ctx, st, proposalID, proposeAfter, now)
	if err != nil {
		return store.Rule{}, false, err
	}
	expires, hasExpires, err := approveTTL(ttlDays, defaultTTL, now)
	if err != nil {
		return store.Rule{}, false, err
	}
	if row != nil {
		switch {
		case row.Status == store.RuleDismissed:
			return store.Rule{}, false, store.ErrRuleDismissed
		case row.Status == store.RuleApproved && !row.Expired(now):
			return *row, false, nil
		}
	}
	r := ruleRow(scope, store.RuleApproved, now)
	r.ApprovedAt, r.HasApprovedAt = now, true
	r.ExpiresAt, r.HasExpiresAt = expires, hasExpires
	if err := st.PutRule(ctx, r); err != nil {
		return store.Rule{}, false, err
	}
	return r, true, nil
}

// DismissProposal declines a proposal: unknown IDs 404, and declining
// an active grant is a 409 (revoke ends automation; dismiss declines
// proposals). Repeat dismissal succeeds without change.
func DismissProposal(ctx context.Context, st *store.Store, proposalID string, proposeAfter int, now time.Time) (store.Rule, bool, error) {
	scope, row, err := resolveScope(ctx, st, proposalID, proposeAfter, now)
	if err != nil {
		return store.Rule{}, false, err
	}
	if row != nil {
		switch {
		case row.Status == store.RuleApproved && !row.Expired(now):
			return store.Rule{}, false, store.ErrRuleActive
		case row.Status == store.RuleDismissed:
			return *row, false, nil
		}
	}
	r := ruleRow(scope, store.RuleDismissed, now)
	if err := st.PutRule(ctx, r); err != nil {
		return store.Rule{}, false, err
	}
	return r, true, nil
}

// RevokeRule ends an approved grant: unknown IDs 404, and repeat
// revocation succeeds without change. Scopes that never carried a
// grant (including declined ones) have no rule to end.
func RevokeRule(ctx context.Context, st *store.Store, ruleID string, now time.Time) (store.Rule, bool, error) {
	if err := checkRuleID(ruleID); err != nil {
		return store.Rule{}, false, err
	}
	if !strings.HasPrefix(ruleID, "OP_RULE_") {
		return store.Rule{}, false, fmt.Errorf("%w: not a rule id", store.ErrRuleInvalid)
	}
	row, err := st.GetRule(ctx, ruleID)
	if err != nil {
		return store.Rule{}, false, store.ErrRuleUnknown
	}
	switch row.Status {
	case store.RuleRevoked:
		return row, false, nil
	case store.RuleApproved:
		// A lapsed grant is already inactive; revoking records the
		// explicit terminal state with a fresh retention horizon.
		row.Status = store.RuleRevoked
		row.DecidedAt = now
		if err := st.PutRule(ctx, row); err != nil {
			return store.Rule{}, false, err
		}
		return row, true, nil
	default:
		// Declined scopes never carried a grant: nothing to end.
		return store.Rule{}, false, store.ErrRuleUnknown
	}
}

// ApplyOperatorRules applies one approved rule as an explicit
// deterministic baseline policy: on exact scope match a non-critical,
// non-low outcome selects low and records the rule ID. Critical is
// never overridden, low needs no change, and every miss — undecodable
// events, ambiguous or ineligible subjects, drifted provenance,
// lapsed grants — returns the outcome untouched. The check re-reads
// the store on every run, so restarts and revocations take effect
// immediately with no cached grants.
func ApplyOperatorRules(ctx context.Context, st *store.Store, taken store.TakenGroup, outcome Outcome, now time.Time) Outcome {
	if outcome.Level == Critical || outcome.Level == Low {
		return outcome
	}
	scope, ok, err := liveSubjectScope(ctx, st, taken.GroupID)
	if err != nil || !ok {
		return outcome
	}
	events := decodeTaken(taken)
	for _, e := range events {
		if e.effective() == nil {
			return outcome
		}
	}
	scope.Kind = KindForTaken(taken, outcome.RuleID)
	scope.PortsCSV = canonicalPorts(events)
	rule, found, err := st.MatchRule(ctx, scope.SourceScope, scope.ReferenceClass, scope.IdentityNamespace,
		scope.UpstreamReference, scope.ReportedIdentitySource, scope.ReportedConfidence,
		scope.Kind, scope.PortsCSV, now)
	if err != nil || !found {
		return outcome
	}
	log.Printf("briefing: group %s operator rule %s selects low", taken.GroupID, rule.RuleID)
	outcome.Level = Low
	outcome.RuleID = rule.RuleID
	return outcome
}
