package briefing

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
// under one source with the identity revision, change kind, canonical
// ports and canonical changed-field set the operator judged expected.
// Matching is byte equality on every field; nothing is inferred,
// borrowed or approximated.
type RuleScope struct {
	SourceScope            string
	ReferenceClass         string
	IdentityNamespace      string
	IdentityRevision       string
	UpstreamReference      string
	ReportedIdentitySource string
	ReportedConfidence     string
	Kind                   string
	PortsCSV               string
	ChangedFieldsCSV       string
}

// canonical renders the scope unambiguously for hashing: a JSON array
// in fixed field order, so no separator collision can merge two scopes.
func (s RuleScope) canonical() string {
	raw, err := json.Marshal([]string{s.SourceScope, s.ReferenceClass, s.IdentityNamespace,
		s.IdentityRevision, s.UpstreamReference, s.ReportedIdentitySource, s.ReportedConfidence,
		s.Kind, s.PortsCSV, s.ChangedFieldsCSV})
	if err != nil {
		return strings.Join([]string{s.SourceScope, s.ReferenceClass, s.IdentityNamespace,
			s.IdentityRevision, s.UpstreamReference, s.ReportedIdentitySource, s.ReportedConfidence,
			s.Kind, s.PortsCSV, s.ChangedFieldsCSV}, "\x1f")
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

// maxChangeFieldRunes bounds one canonicalized change-field name. A
// hostile changes[] entry cannot bloat rule rows: over-bound names
// truncate deterministically, identically on both scope sides.
const maxChangeFieldRunes = 64

// canonicalChangeFields reduces decoded group events to the sorted
// changed-field set proposal derivation and application match
// exactly: a group carrying changes outside the approved set never
// borrows its rule. Producer-reported changes[] names fields
// directly; without them the salient raw differences stand in —
// hostname, os, device and the other address/identity attributes,
// plus ports when the reported set moves. Timestamps never qualify:
// liveness churn must not fragment scopes.
func canonicalChangeFields(events []GroupEvent) string {
	seen := map[string]bool{}
	var out []string
	add := func(field string) {
		field = strings.TrimSpace(field)
		if field == "" {
			return
		}
		if runes := []rune(field); len(runes) > maxChangeFieldRunes {
			field = string(runes[:maxChangeFieldRunes])
		}
		if !seen[field] {
			seen[field] = true
			out = append(out, field)
		}
	}
	portsKey := func(v *hostView) string {
		var keys []string
		for _, p := range v.Ports {
			keys = append(keys, p.Protocol+"/"+fmt.Sprint(p.Number)+"/"+p.State)
		}
		sort.Strings(keys)
		return strings.Join(keys, ",")
	}
	for _, e := range events {
		if e.EventType != "host.changed" || e.Previous == nil || e.Current == nil {
			continue
		}
		prev, cur := e.Previous, e.Current
		if len(e.Changes) > 0 {
			for _, field := range e.Changes {
				add(field)
			}
			continue
		}
		if prev.Hostname != cur.Hostname && (prev.Hostname != "" || cur.Hostname != "") {
			add("hostname")
		}
		// OS and device compare independently: an OS-only change must
		// not share a scope with a device-only or combined change.
		// The kind derivation still merges the OS refinement family
		// for precedence; the field set records what actually moved.
		if prev.OS != cur.OS && (prev.OS != "" || cur.OS != "") {
			add("os")
		}
		if prev.Device != cur.Device && (prev.Device != "" || cur.Device != "") {
			add("device")
		}
		if prev.IP != cur.IP && (prev.IP != "" || cur.IP != "") {
			add("ip")
		}
		if prev.MAC != cur.MAC && (prev.MAC != "" || cur.MAC != "") {
			add("mac")
		}
		if prev.Vendor != cur.Vendor && (prev.Vendor != "" || cur.Vendor != "") {
			add("vendor")
		}
		if prev.Weak != cur.Weak || prev.Alive != cur.Alive || prev.RandomizedMAC != cur.RandomizedMAC {
			add("liveness")
		}
		if prev.IdentityConfidence != cur.IdentityConfidence &&
			(prev.IdentityConfidence != "" || cur.IdentityConfidence != "") {
			add("identity_confidence")
		}
		if portsKey(prev) != portsKey(cur) {
			add("ports")
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// qualifyRuleSubject is the stable-identity gate (Designv9 §§1.3–1.4,
// §5.2; Plan Step 28): a subject automates only under a qualified
// stable contract — reported_device class, high upstream confidence,
// a non-empty namespace bound to the source registration, a verified
// (non-empty) identity revision, and the negotiated stable-identity
// capability. Anything else, including namespace-bound references
// with unknown revisions, reads as no scope: unknown or changed
// revisions void application, so eligibility stays closed until a
// verified revision is available. Pure so every combination is
// unit-coverable.
func qualifyRuleSubject(sub store.ReportedSubject, configuredNS, capabilities string) (RuleScope, bool) {
	var zero RuleScope
	if sub.ReferenceClass != store.SubjectReportedDevice || sub.SourceScope == "" ||
		sub.ReportedIdentitySource == "" || sub.ReportedConfidence != "high" {
		return zero, false
	}
	if sub.IdentityNamespace == "" || sub.IdentityNamespace != configuredNS || sub.IdentityRevision == "" ||
		!store.HasCapability(capabilities, store.StableIdentityCapability) {
		return zero, false
	}
	return RuleScope{
		SourceScope: sub.SourceScope, ReferenceClass: sub.ReferenceClass,
		IdentityNamespace: sub.IdentityNamespace, IdentityRevision: sub.IdentityRevision,
		UpstreamReference:      sub.UpstreamReference,
		ReportedIdentitySource: sub.ReportedIdentitySource, ReportedConfidence: sub.ReportedConfidence,
	}, true
}

// liveSubjectScope loads one group's subject scope: every linked
// subject must share one identical qualified stable scope. Anything
// else — no links, mixed scopes, an unqualified class, an unbound
// namespace or a missing capability — reads as no scope, so rules
// never fire on ambiguity, incomplete provenance or an unnegotiated
// identity contract.
func liveSubjectScope(ctx context.Context, st *store.Store, groupID string) (RuleScope, bool, error) {
	var zero RuleScope
	links, _, err := st.GroupSubjects(ctx, groupID)
	if err != nil || len(links) == 0 {
		return zero, false, err
	}
	var sub store.ReportedSubject
	for i, link := range links {
		one, err := st.ReportedProfile(ctx, link.ProfileID)
		if err != nil {
			return zero, false, err
		}
		if i == 0 {
			sub = one
			continue
		}
		if one.SourceScope != sub.SourceScope || one.ReferenceClass != sub.ReferenceClass ||
			one.IdentityNamespace != sub.IdentityNamespace || one.IdentityRevision != sub.IdentityRevision ||
			one.UpstreamReference != sub.UpstreamReference ||
			one.ReportedIdentitySource != sub.ReportedIdentitySource || one.ReportedConfidence != sub.ReportedConfidence {
			return zero, false, nil
		}
	}
	if sub.SourceScope == "" {
		return zero, false, nil
	}
	configuredNS, capabilities, err := st.SourceIdentity(ctx, sub.SourceScope)
	if err != nil {
		if err == sql.ErrNoRows {
			// No registration contract: unavailable, not an error.
			return zero, false, nil
		}
		return zero, false, err
	}
	scope, ok := qualifyRuleSubject(sub, configuredNS, capabilities)
	if !ok {
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
	rev, ok := briefingRevision(events)
	if !ok || rev != scope.IdentityRevision {
		// Judgments stay pinned to the epoch they were made in. The
		// subject row always carries the latest reported revision, so
		// without this check old judgments would be reassigned to the
		// newest epoch and counted as evidence for it. Stale (past
		// epoch), unknown (no reported revision) and mixed (straddled
		// epochs) evidence never proposes.
		return zero, false, nil
	}
	scope.Kind = b.Kind
	scope.PortsCSV = canonicalPorts(events)
	scope.ChangedFieldsCSV = canonicalChangeFields(events)
	return scope, true, nil
}

// briefingRevision reduces a briefing's decoded events to the single
// identity revision they unanimously report. Every readable view
// votes; an empty vote (a view from before revisions were reported)
// poisons unanimity exactly like a disagreement. Events without any
// usable view never reach here — scope derivation already voids those
// scopes as incomplete provenance.
func briefingRevision(events []GroupEvent) (string, bool) {
	seen := map[string]bool{}
	var out []string
	for _, e := range events {
		for _, v := range []*hostView{e.Host, e.Current, e.Previous} {
			if v == nil {
				continue
			}
			if !seen[v.IdentityRevision] {
				seen[v.IdentityRevision] = true
				out = append(out, v.IdentityRevision)
			}
		}
	}
	if len(out) != 1 || out[0] == "" {
		return "", false
	}
	return out[0], true
}

// DeriveProposals collects the actionable scopes: distinct briefings
// whose latest lane state is expected (undone and overruled judgments
// excluded, stale audit excluded), grouped by exact scope, keeping
// scopes with at least proposeAfter briefings. Scopes with an active
// grant or an explicit decline are not proposals; revoked scopes
// re-propose (the evidence still stands, the automation stopped).
// maxProposalEvidenceIDs bounds the distinct briefing IDs one
// derivation collects, and maxProposalDecisionRows bounds the decision
// rows it examines to collect them (Designv9 §4.6: explicit
// cardinality bounds on work as well as memory, never reliance on
// retention age). The scan runs newest-first, so a busy history keeps
// recent judgments — the evidence automation acts on — and drops the
// oldest first. A truncated scan yields a proposal subset, never a
// wrong proposal: counts only grow toward the threshold, so missing
// old evidence can only withhold proposals, and the truncation is
// logged. Small histories never notice either bound; operator routes
// stay bounded regardless of fleet size.
const maxProposalEvidenceIDs = 2000

const maxProposalDecisionRows = 10000

func DeriveProposals(ctx context.Context, st *store.Store, proposeAfter int, now time.Time) ([]Proposal, error) {
	if proposeAfter < 1 {
		return nil, fmt.Errorf("%w: propose-after must be positive", store.ErrRuleInvalid)
	}
	var ids []string
	seenBriefing := map[string]bool{}
	var beforeRowID int64
	var examined int64
	incomplete := false
loop:
	for {
		page, err := st.ExpectedBriefingDecisionsDesc(ctx, beforeRowID, 500)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for i, d := range page {
			beforeRowID = d.RowID
			if !seenBriefing[d.BriefingID] {
				seenBriefing[d.BriefingID] = true
				ids = append(ids, d.BriefingID)
			}
			examined++
			if examined >= maxProposalDecisionRows || len(ids) >= maxProposalEvidenceIDs {
				// Rows after this one in the page, or any later
				// page, go unexamined: the scan yields a proposal
				// subset. A bound met exactly at the last row of a
				// short page leaves nothing behind.
				incomplete = i < len(page)-1 || len(page) == 500
				break loop
			}
		}
		if len(page) < 500 {
			break
		}
	}
	if incomplete {
		log.Printf("briefing: proposal derivation truncated at %d rows (%d briefings); counts are lower bounds", examined, len(ids))
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
		IdentityNamespace: row.IdentityNamespace, IdentityRevision: row.IdentityRevision,
		UpstreamReference:      row.UpstreamReference,
		ReportedIdentitySource: row.ReportedIdentitySource, ReportedConfidence: row.ReportedConfidence,
		Kind: row.Kind, PortsCSV: row.PortsCSV, ChangedFieldsCSV: row.ChangedFieldsCSV,
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
		IdentityNamespace: scope.IdentityNamespace, IdentityRevision: scope.IdentityRevision,
		UpstreamReference:      scope.UpstreamReference,
		ReportedIdentitySource: scope.ReportedIdentitySource, ReportedConfidence: scope.ReportedConfidence,
		Kind: scope.Kind, PortsCSV: scope.PortsCSV, ChangedFieldsCSV: scope.ChangedFieldsCSV,
		Status: status, DecidedAt: now,
	}
}

// ApproveProposal approves the deterministic proposal: unknown IDs
// 404, declined scopes 409, and repeated approval returns the existing
// grant. Revoked and lapsed grants re-approve under the same IDs —
// the proposal still stands, only the automation stopped.
func ApproveProposal(ctx context.Context, st *store.Store, proposalID string, proposeAfter, ttlDays int, defaultTTL time.Duration, now time.Time) (store.Rule, bool, error) {
	scope, _, err := resolveScope(ctx, st, proposalID, proposeAfter, now)
	if err != nil {
		return store.Rule{}, false, err
	}
	expires, hasExpires, err := approveTTL(ttlDays, defaultTTL, now)
	if err != nil {
		return store.Rule{}, false, err
	}
	// The lifecycle check rides the write transaction: a concurrent
	// dismiss between resolution and approval loses with a 409 instead
	// of being overwritten.
	return st.TransitionRule(ctx, scope.RuleID(), func(existing *store.Rule) (*store.Rule, error) {
		if existing != nil {
			switch {
			case existing.Status == store.RuleDismissed:
				return nil, store.ErrRuleDismissed
			case existing.Status == store.RuleApproved && !existing.Expired(now):
				return existing, nil
			}
		}
		r := ruleRow(scope, store.RuleApproved, now)
		r.ApprovedAt, r.HasApprovedAt = now, true
		r.ExpiresAt, r.HasExpiresAt = expires, hasExpires
		return &r, nil
	})
}

// DismissProposal declines a proposal: unknown IDs 404, and declining
// an active grant is a 409 (revoke ends automation; dismiss declines
// proposals). Repeat dismissal succeeds without change.
func DismissProposal(ctx context.Context, st *store.Store, proposalID string, proposeAfter int, now time.Time) (store.Rule, bool, error) {
	scope, _, err := resolveScope(ctx, st, proposalID, proposeAfter, now)
	if err != nil {
		return store.Rule{}, false, err
	}
	// Like approval, the conflict check shares the write transaction
	// so a concurrent approve cannot slip past a dismissal.
	return st.TransitionRule(ctx, scope.RuleID(), func(existing *store.Rule) (*store.Rule, error) {
		if existing != nil {
			switch {
			case existing.Status == store.RuleApproved && !existing.Expired(now):
				return nil, store.ErrRuleActive
			case existing.Status == store.RuleDismissed:
				return existing, nil
			}
		}
		r := ruleRow(scope, store.RuleDismissed, now)
		return &r, nil
	})
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
	return st.TransitionRule(ctx, ruleID, func(existing *store.Rule) (*store.Rule, error) {
		if existing == nil {
			return nil, store.ErrRuleUnknown
		}
		row := *existing
		switch row.Status {
		case store.RuleRevoked:
			return existing, nil
		case store.RuleApproved:
			// A lapsed grant is already inactive; revoking records the
			// explicit terminal state with a fresh retention horizon.
			row.Status = store.RuleRevoked
			row.DecidedAt = now
			return &row, nil
		default:
			// Declined scopes never carried a grant: nothing to end.
			return nil, store.ErrRuleUnknown
		}
	})
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
	scope.ChangedFieldsCSV = canonicalChangeFields(events)
	rule, found, err := st.MatchRule(ctx, scope.SourceScope, scope.ReferenceClass, scope.IdentityNamespace,
		scope.IdentityRevision, scope.UpstreamReference, scope.ReportedIdentitySource, scope.ReportedConfidence,
		scope.Kind, scope.PortsCSV, scope.ChangedFieldsCSV, now)
	if err != nil || !found {
		return outcome
	}
	log.Printf("briefing: group %s operator rule %s selects low", taken.GroupID, rule.RuleID)
	outcome.Level = Low
	outcome.RuleID = rule.RuleID
	return outcome
}
