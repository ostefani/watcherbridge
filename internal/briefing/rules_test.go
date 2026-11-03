package briefing

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

// ruleNamespace is the reported stable-identity namespace every rule
// fixture carries: the gate binds it to the test scope, simulating an
// I1-qualified deployment. Fixtures without a binding stay
// unavailable, exactly like today's bare-producer reports.
const ruleNamespace = "ns-1"

// ruleRevision is the verified identity revision every rule fixture
// carries: unknown revisions void application, so the revision must
// be reported, not merely bound.
const ruleRevision = "rev-1"

// rulePayload reports one open port on the shared MAC: every group
// seeded with it shares one reported_device scope, one kind and one
// canonical port set. The host_id/identity_source pair is what makes
// the MAC a scanner-reported reference rather than a bare attribute —
// without it the views project event-local and no rule can attach.
// The namespace alone qualifies nothing: only the bound registration
// (bindRuleSource) completes the stable contract.
func rulePayload() string {
	view := `{"ip":"192.168.0.5","hostname":"p","mac":"aa:bb:cc:dd:ee:01","host_id":"mac:aa:bb:cc:dd:ee:01","identity_source":"mac","identity_namespace":"` + ruleNamespace + `","identity_revision":"` + ruleRevision + `","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-20T00:00:00Z","ports":[%s]}`
	open := `{"number":22,"protocol":"tcp","state":"open"}`
	return `{"event_id":"e1","previous":` + strings.Replace(view, "%s", "", 1) +
		`,"current":` + strings.Replace(view, "%s", open, 1) + `}`
}

// bindRuleSource records the negotiated identity contract the gate
// requires for one scope: the bound namespace plus the advertised
// stable-identity capability.
func bindRuleSource(t tb, s *store.Store, scope string) {
	t.Helper()
	if err := s.BindSourceIdentity(context.Background(), scope, ruleNamespace, string(store.StableIdentityCapability)); err != nil {
		t.Fatal(err)
	}
}

func seedRuleGroup(t tb, s *store.Store, eventID string) string {
	t.Helper()
	return seedGroup(t, s, "host:w", eventID, "host.changed", rulePayload())
}

func briefRuleGroup(t tb, s *store.Store, gid string) store.Briefing {
	t.Helper()
	return briefRuleGroupWith(t, s, testDeps(t, s), gid)
}

func briefRuleGroupWith(t tb, s *store.Store, d Deps, gid string) store.Briefing {
	t.Helper()
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("rule group must brief")
	}
	// briefed() asserts the whole queue drains, but rule tests hold
	// several closed groups at once — fetch this group's briefing
	// directly instead.
	if st, _ := s.GroupStatus(context.Background(), gid); st != "done" {
		t.Fatalf("rule group must be done, got %q", st)
	}
	b, err := s.BriefingForGroup(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func expectBriefing(t tb, s *store.Store, gid, req string) string {
	t.Helper()
	in := store.DecisionInput{SubjectKind: store.DecisionSubjectBriefing, SubjectID: "brf-" + gid, Kind: store.DecisionExpected}
	if req != "" {
		in.RequestID = req
	}
	res, err := s.AppendDecision(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return res.Decision.DecisionID
}

func seedThreeExpected(t tb, s *store.Store) []string {
	t.Helper()
	bindRuleSource(t, s, "test-scope")
	gids := []string{
		seedRuleGroup(t, s, "evt-r1"),
		seedRuleGroup(t, s, "evt-r2"),
		seedRuleGroup(t, s, "evt-r3"),
	}
	for i, gid := range gids {
		briefRuleGroup(t, s, gid)
		expectBriefing(t, s, gid, "rule-req-"+string(rune('a'+i)))
	}
	return gids
}

func TestDeriveProposalsNeedsThree(t *testing.T) {
	s := openWorkerStore(t)
	bindRuleSource(t, s, "test-scope")
	gids := []string{seedRuleGroup(t, s, "evt-r1"), seedRuleGroup(t, s, "evt-r2")}
	for i, gid := range gids {
		briefRuleGroup(t, s, gid)
		expectBriefing(t, s, gid, "rule-req-"+string(rune('a'+i)))
	}
	now := time.Now()
	if ps, err := DeriveProposals(context.Background(), s, 3, now); err != nil || len(ps) != 0 {
		t.Fatalf("two expecteds must not propose: %+v %v", ps, err)
	}
	// A repeat judgment on an already-expected briefing adds no new
	// evidence: distinct briefings count, not decision rows.
	expectBriefing(t, s, gids[0], "")
	if ps, err := DeriveProposals(context.Background(), s, 3, now); err != nil || len(ps) != 0 {
		t.Fatalf("repeat judgment must not propose: %+v %v", ps, err)
	}
	// The third distinct briefing completes the evidence.
	last := seedRuleGroup(t, s, "evt-r3")
	briefRuleGroup(t, s, last)
	expectBriefing(t, s, last, "rule-req-c")
	ps, err := DeriveProposals(context.Background(), s, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].MatchingBriefings != 3 {
		t.Fatalf("three expecteds must propose once with count 3: %+v", ps)
	}
	if !strings.HasPrefix(ps[0].ProposalID, "OP_PROP_") {
		t.Fatalf("proposal id shape: %q", ps[0].ProposalID)
	}
	if got := ps[0].RuleScope.RuleID(); !strings.HasPrefix(got, "OP_RULE_") ||
		!strings.HasSuffix(ps[0].ProposalID, got[len("OP_RULE_"):]) {
		t.Fatalf("rule id must share the proposal hash: %+v", ps[0])
	}
	if ps[0].ReferenceClass != store.SubjectReportedDevice || ps[0].Kind == "" || ps[0].PortsCSV == "" {
		t.Fatalf("proposal must carry class, kind and ports: %+v", ps[0])
	}
	if ps[0].ChangedFieldsCSV != "ports" {
		t.Fatalf("proposal must carry the canonical changed fields: %+v", ps[0])
	}
}

// Bare-producer evidence must not propose: without the bound
// registration contract the feature stays unavailable, even with
// three matching expected judgments on identical MAC references.
func TestDeriveProposalsNeedsBoundContract(t *testing.T) {
	s := openWorkerStore(t)
	for i, id := range []string{"evt-u1", "evt-u2", "evt-u3"} {
		gid := seedRuleGroup(t, s, id)
		briefRuleGroup(t, s, gid)
		expectBriefing(t, s, gid, "unbound-req-"+string(rune('a'+i)))
	}
	now := time.Now()
	if ps, err := DeriveProposals(context.Background(), s, 3, now); err != nil || len(ps) != 0 {
		t.Fatalf("unbound scope must not propose: %+v %v", ps, err)
	}
}

// A bounded scan withholds stale proposals without breaking recent
// ones: ten thousand filler judgments push an older scope's three
// expecteds past the examined-rows bound, so the old scope stays
// silent while a newer scope's three judgments still propose. The
// truncated counts are lower bounds: missing evidence withholds
// proposals, it never invents them.
func TestDeriveProposalsTruncatesOldEvidence(t *testing.T) {
	s := openWorkerStore(t)
	bindRuleSource(t, s, "test-scope")
	oldPayload := strings.Replace(rulePayload(), "aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02", -1)
	judge := func(id, payload, req string) {
		gid := seedGroup(t, s, "host:w", id, "host.changed", payload)
		briefRuleGroup(t, s, gid)
		expectBriefing(t, s, gid, req)
	}
	for i, id := range []string{"evt-old1", "evt-old2", "evt-old3"} {
		judge(id, oldPayload, "old-req-"+string(rune('a'+i)))
	}
	fill := seedRuleGroup(t, s, "evt-fill")
	briefRuleGroup(t, s, fill)
	fillBriefing := briefed(t, s, fill)
	for i := 0; i < maxProposalDecisionRows; i++ {
		if _, err := s.AppendDecision(context.Background(), store.DecisionInput{
			SubjectKind: store.DecisionSubjectBriefing, SubjectID: fillBriefing.BriefingID,
			Kind: store.DecisionExpected, RequestID: fmt.Sprintf("fill-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range []string{"evt-new1", "evt-new2", "evt-new3"} {
		judge(id, rulePayload(), "new-req-"+string(rune('a'+i)))
	}
	now := time.Now()
	ps, err := DeriveProposals(context.Background(), s, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 {
		t.Fatalf("only the recent scope must propose: %+v", ps)
	}
	if !strings.Contains(ps[0].UpstreamReference, "aa:bb:cc:dd:ee:01") {
		t.Fatalf("truncation must keep the newest evidence: %+v", ps[0])
	}
}

// Judgments stay pinned to the epoch they were made in. Three rev-1
// judgments, an approval, then one unjudged rev-2 report: the old
// judgments must not be reassigned to rev-2 (no rev-2 proposal), the
// rev-1 grant must not fire on the live rev-2 scope, and three fresh
// rev-2 judgments must propose the new epoch.
func TestDeriveProposalsRejectsStaleRevision(t *testing.T) {
	s := openWorkerStore(t)
	bindRuleSource(t, s, "test-scope")
	rev2Payload := strings.Replace(rulePayload(), ruleRevision, "rev-2", -1)
	judge := func(id, payload, req string) {
		gid := seedGroup(t, s, "host:w", id, "host.changed", payload)
		briefRuleGroup(t, s, gid)
		expectBriefing(t, s, gid, req)
	}
	for i, id := range []string{"evt-e1", "evt-e2", "evt-e3"} {
		judge(id, rulePayload(), "epoch-req-"+string(rune('a'+i)))
	}
	now := time.Now()
	ps, err := DeriveProposals(context.Background(), s, 3, now)
	if err != nil || len(ps) != 1 || ps[0].IdentityRevision != ruleRevision {
		t.Fatalf("rev-1 evidence must propose its own epoch: %+v %v", ps, err)
	}
	grant, applied, err := ApproveProposal(context.Background(), s, ps[0].ProposalID, 3, -1, 90*24*time.Hour, now)
	if err != nil || !applied {
		t.Fatalf("rev-1 grant must apply: %+v %v", grant, err)
	}
	// The epoch turns: one rev-2 report, judged by nobody.
	epochGID := seedGroup(t, s, "host:w", "evt-e4", "host.changed", rev2Payload)
	epochBrief := briefRuleGroup(t, s, epochGID)
	if epochBrief.BaselineRule == grant.RuleID {
		t.Fatalf("rev-1 grant must not fire on the rev-2 scope: %+v", epochBrief)
	}
	if ps, err := DeriveProposals(context.Background(), s, 3, now); err != nil || len(ps) != 0 {
		t.Fatalf("old judgments must not propose the new epoch: %+v %v", ps, err)
	}
	// Three rev-2 judgments propose the new epoch on its own evidence.
	for i, id := range []string{"evt-e5", "evt-e6", "evt-e7"} {
		judge(id, rev2Payload, "epoch2-req-"+string(rune('a'+i)))
	}
	ps, err = DeriveProposals(context.Background(), s, 3, now)
	if err != nil || len(ps) != 1 || ps[0].IdentityRevision != "rev-2" {
		t.Fatalf("rev-2 evidence must propose its own epoch: %+v %v", ps, err)
	}
}

func TestDeriveProposalsSkipsUnknownRevision(t *testing.T) {
	s := openWorkerStore(t)
	bindRuleSource(t, s, "test-scope")
	payload := strings.Replace(rulePayload(), `"identity_revision":"`+ruleRevision+`"`, `"identity_revision":""`, -1)
	for i, id := range []string{"evt-v1", "evt-v2", "evt-v3"} {
		gid := seedGroup(t, s, "host:w", id, "host.changed", payload)
		briefRuleGroup(t, s, gid)
		expectBriefing(t, s, gid, "rev-req-"+string(rune('a'+i)))
	}
	now := time.Now()
	if ps, err := DeriveProposals(context.Background(), s, 3, now); err != nil || len(ps) != 0 {
		t.Fatalf("unknown revision must not propose: %+v %v", ps, err)
	}
}

// Low upstream confidence never qualifies for automatic rules, even
// under a bound contract.
func TestDeriveProposalsSkipsLowConfidence(t *testing.T) {
	s := openWorkerStore(t)
	bindRuleSource(t, s, "test-scope")
	payload := strings.Replace(rulePayload(), `"identity_confidence":"high"`, `"identity_confidence":"low"`, -1)
	for i, id := range []string{"evt-l1", "evt-l2", "evt-l3"} {
		gid := seedGroup(t, s, "host:w", id, "host.changed", payload)
		briefRuleGroup(t, s, gid)
		expectBriefing(t, s, gid, "low-req-"+string(rune('a'+i)))
	}
	now := time.Now()
	if ps, err := DeriveProposals(context.Background(), s, 3, now); err != nil || len(ps) != 0 {
		t.Fatalf("low confidence must not propose: %+v %v", ps, err)
	}
}

func TestQualifyRuleSubjectMatrix(t *testing.T) {
	base := store.ReportedSubject{
		SourceScope: "test-scope", ReferenceClass: store.SubjectReportedDevice,
		IdentityNamespace: ruleNamespace, IdentityRevision: ruleRevision,
		UpstreamReference:      "mac:aa:bb:cc:dd:ee:01",
		ReportedIdentitySource: "mac", ReportedConfidence: "high",
	}
	caps := string(store.StableIdentityCapability)
	cases := []struct {
		name   string
		adjust func(*store.ReportedSubject) (string, string)
		ok     bool
	}{
		{"qualified", func(s *store.ReportedSubject) (string, string) { return ruleNamespace, caps }, true},
		{"low confidence", func(s *store.ReportedSubject) (string, string) {
			s.ReportedConfidence = "low"
			return ruleNamespace, caps
		}, false},
		{"unknown confidence", func(s *store.ReportedSubject) (string, string) {
			s.ReportedConfidence = ""
			return ruleNamespace, caps
		}, false},
		{"empty namespace", func(s *store.ReportedSubject) (string, string) {
			s.IdentityNamespace = ""
			return ruleNamespace, caps
		}, false},
		{"namespace mismatch", func(s *store.ReportedSubject) (string, string) {
			return "other-ns", caps
		}, false},
		{"unknown revision", func(s *store.ReportedSubject) (string, string) {
			s.IdentityRevision = ""
			return ruleNamespace, caps
		}, false},
		{"capability missing", func(s *store.ReportedSubject) (string, string) {
			return ruleNamespace, ""
		}, false},
		{"capability wrong", func(s *store.ReportedSubject) (string, string) {
			return ruleNamespace, "heartbeat-v1"
		}, false},
		{"address class", func(s *store.ReportedSubject) (string, string) {
			s.ReferenceClass = store.SubjectAddress
			return ruleNamespace, caps
		}, false},
		{"empty source", func(s *store.ReportedSubject) (string, string) {
			s.SourceScope = ""
			return ruleNamespace, caps
		}, false},
		{"empty identity source", func(s *store.ReportedSubject) (string, string) {
			s.ReportedIdentitySource = ""
			return ruleNamespace, caps
		}, false},
	}
	for _, tc := range cases {
		sub := base
		ns, capabilities := tc.adjust(&sub)
		scope, ok := qualifyRuleSubject(sub, ns, capabilities)
		if ok != tc.ok {
			t.Fatalf("%s: qualify = %v, want %v", tc.name, ok, tc.ok)
		}
		if ok && (scope.UpstreamReference != sub.UpstreamReference || scope.IdentityNamespace != ruleNamespace ||
			scope.IdentityRevision != ruleRevision) {
			t.Fatalf("%s: qualified scope must carry the subject: %+v", tc.name, scope)
		}
	}
}

func TestDeriveProposalsSkipsIneligible(t *testing.T) {
	s := openWorkerStore(t)
	// Address-class briefings never qualify, however often expected.
	for i, id := range []string{"evt-a1", "evt-a2", "evt-a3"} {
		payload := `{"event_id":"` + id + `","host":{"ip":"192.168.0.9","host_id":"ip:192.168.0.9","identity_source":"ip","identity_confidence":"high"}}`
		gid := seedGroup(t, s, "host:ip", id, "host.appeared", payload)
		briefRuleGroup(t, s, gid)
		expectBriefing(t, s, gid, "ip-req-"+string(rune('a'+i)))
	}
	now := time.Now()
	if ps, err := DeriveProposals(context.Background(), s, 3, now); err != nil || len(ps) != 0 {
		t.Fatalf("address subjects must never propose: %+v %v", ps, err)
	}
}

func TestDeriveProposalsSkipsUndone(t *testing.T) {
	s := openWorkerStore(t)
	bindRuleSource(t, s, "test-scope")
	gids := []string{seedRuleGroup(t, s, "evt-r1"), seedRuleGroup(t, s, "evt-r2"), seedRuleGroup(t, s, "evt-r3")}
	first := ""
	for i, gid := range gids {
		briefRuleGroup(t, s, gid)
		id := expectBriefing(t, s, gid, "undone-req-"+string(rune('a'+i)))
		if i == 0 {
			first = id
		}
	}
	now := time.Now()
	if ps, err := DeriveProposals(context.Background(), s, 3, now); err != nil || len(ps) != 1 {
		t.Fatalf("three expecteds must propose: %+v %v", ps, err)
	}
	// Undoing one judgment drops the live count below threshold: the
	// proposal evaporates without any stored state to clean.
	if _, err := s.UndoDecision(context.Background(), first, "undo-req-a"); err != nil {
		t.Fatalf("undo must apply: %v", err)
	}
	if ps, err := DeriveProposals(context.Background(), s, 3, now); err != nil || len(ps) != 0 {
		t.Fatalf("undone evidence must un-propose: %+v %v", ps, err)
	}
}

func TestApproveApplySelectsLow(t *testing.T) {
	s := openWorkerStore(t)
	seedThreeExpected(t, s)
	now := time.Now()
	ps, err := DeriveProposals(context.Background(), s, 3, now)
	if err != nil || len(ps) != 1 {
		t.Fatalf("proposal missing: %+v %v", ps, err)
	}
	rule, applied, err := ApproveProposal(context.Background(), s, ps[0].ProposalID, 3, -1, 90*24*time.Hour, now)
	if err != nil || !applied {
		t.Fatalf("approve must apply: %+v %v", rule, err)
	}
	if !strings.HasPrefix(rule.RuleID, "OP_RULE_") || rule.Status != store.RuleApproved || rule.HasExpiresAt {
		t.Fatalf("ttl none must grant without expiry: %+v", rule)
	}
	// The approved scope stops proposing: the grant is active.
	if ps, err := DeriveProposals(context.Background(), s, 3, now); err != nil || len(ps) != 0 {
		t.Fatalf("active grant must not re-propose: %+v %v", ps, err)
	}
	// A later group on the exact scope briefs low with the rule recorded.
	b := briefRuleGroup(t, s, seedRuleGroup(t, s, "evt-r4"))
	if b.Priority != "low" {
		t.Fatalf("rule must select low, got %q (%+v)", b.Priority, b)
	}
	if b.BaselineRule != rule.RuleID {
		t.Fatalf("application must record the rule: %+v", b)
	}
	// Repeat approval returns the existing grant, unchanged.
	again, applied, err := ApproveProposal(context.Background(), s, ps[0].ProposalID, 3, -1, 90*24*time.Hour, now)
	if err != nil || applied || again.RuleID != rule.RuleID {
		t.Fatalf("repeat approval must return the grant: %+v %v %v", again, applied, err)
	}
}

func TestRuleGuards(t *testing.T) {
	s := openWorkerStore(t)
	seedThreeExpected(t, s)
	now := time.Now()
	ps, err := DeriveProposals(context.Background(), s, 3, now)
	if err != nil || len(ps) != 1 {
		t.Fatalf("proposal missing: %+v %v", ps, err)
	}
	pid := ps[0].ProposalID
	if _, _, err := ApproveProposal(context.Background(), s, "OP_PROP_deadbeefdeadbeef", 3, 0, 90*24*time.Hour, now); err != store.ErrRuleUnknown {
		t.Fatalf("unknown proposal must 404: %v", err)
	}
	if _, _, err := ApproveProposal(context.Background(), s, "bogus", 3, 0, 90*24*time.Hour, now); err == nil {
		t.Fatal("malformed id must reject")
	}
	if _, _, err := ApproveProposal(context.Background(), s, pid, 3, 366, 90*24*time.Hour, now); err == nil {
		t.Fatal("ttl past 365 days must reject")
	}
	rule, _, err := ApproveProposal(context.Background(), s, pid, 3, 7, 90*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if !rule.HasExpiresAt || rule.ExpiresAt.Sub(now) != 7*24*time.Hour {
		t.Fatalf("ttl days must set expiry: %+v", rule)
	}
	// Dismissing an active grant conflicts: revoke ends automation.
	if _, _, err := DismissProposal(context.Background(), s, pid, 3, now); err != store.ErrRuleActive {
		t.Fatalf("dismiss of active rule must 409: %v", err)
	}
	if _, _, err := RevokeRule(context.Background(), s, "OP_RULE_deadbeefdeadbeef", now); err != store.ErrRuleUnknown {
		t.Fatalf("unknown rule must 404: %v", err)
	}
	revoked, applied, err := RevokeRule(context.Background(), s, rule.RuleID, now)
	if err != nil || !applied || revoked.Status != store.RuleRevoked {
		t.Fatalf("revoke must apply: %+v %v %v", revoked, applied, err)
	}
	if _, applied, err := RevokeRule(context.Background(), s, rule.RuleID, now); err != nil || applied {
		t.Fatalf("repeat revoke must succeed unchanged: %v %v", err, applied)
	}
	// The revoked scope re-proposes (evidence stands, automation
	// stopped) and re-approves under the same IDs.
	if ps, err := DeriveProposals(context.Background(), s, 3, now); err != nil || len(ps) != 1 || ps[0].ProposalID != pid {
		t.Fatalf("revoked scope must re-propose: %+v %v", ps, err)
	}
	if _, applied, err := ApproveProposal(context.Background(), s, pid, 3, 0, 90*24*time.Hour, now); err != nil || !applied {
		t.Fatalf("re-approve after revoke must apply: %v %v", err, applied)
	}
}

func TestDismissBlocksApprove(t *testing.T) {
	s := openWorkerStore(t)
	seedThreeExpected(t, s)
	now := time.Now()
	ps, err := DeriveProposals(context.Background(), s, 3, now)
	if err != nil || len(ps) != 1 {
		t.Fatalf("proposal missing: %+v %v", ps, err)
	}
	pid := ps[0].ProposalID
	if _, applied, err := DismissProposal(context.Background(), s, pid, 3, now); err != nil || !applied {
		t.Fatalf("dismiss must apply: %v %v", err, applied)
	}
	if _, applied, err := DismissProposal(context.Background(), s, pid, 3, now); err != nil || applied {
		t.Fatalf("repeat dismiss must succeed unchanged: %v %v", err, applied)
	}
	if _, _, err := ApproveProposal(context.Background(), s, pid, 3, 0, 90*24*time.Hour, now); err != store.ErrRuleDismissed {
		t.Fatalf("approve of declined scope must 409: %v", err)
	}
	if ps, err := DeriveProposals(context.Background(), s, 3, now); err != nil || len(ps) != 0 {
		t.Fatalf("declined scope must not re-propose: %+v %v", ps, err)
	}
}

func TestRuleStaysInScope(t *testing.T) {
	s := openWorkerStore(t)
	seedThreeExpected(t, s)
	now := time.Now()
	ps, err := DeriveProposals(context.Background(), s, 3, now)
	if err != nil || len(ps) != 1 {
		t.Fatalf("proposal missing: %+v %v", ps, err)
	}
	rule, _, err := ApproveProposal(context.Background(), s, ps[0].ProposalID, 3, -1, 90*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	// The same MAC under another source scope cannot borrow the rule:
	// exact source equality is part of the scope, never inferred.
	payload := rulePayload()
	other := seedScopedGroup(t, s, "other-scope", "evt-x1", payload)
	b := briefRuleGroup(t, s, other)
	if b.Priority == "low" && b.BaselineRule == rule.RuleID {
		t.Fatalf("cross-source MAC must not borrow a rule: %+v", b)
	}
	// A different port set is out of scope even for the same subject.
	view := `{"ip":"192.168.0.5","hostname":"p","mac":"aa:bb:cc:dd:ee:01","host_id":"mac:aa:bb:cc:dd:ee:01","identity_source":"mac","identity_namespace":"` + ruleNamespace + `","identity_revision":"` + ruleRevision + `","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-20T00:00:00Z","ports":[%s]}`
	otherPorts := `{"event_id":"e1","previous":` + strings.Replace(view, "%s", "", 1) +
		`,"current":` + strings.Replace(view, "%s", `{"number":80,"protocol":"tcp","state":"open"}`, 1) + `}`
	portsGID := seedGroup(t, s, "host:w", "evt-r9", "host.changed", otherPorts)
	b2 := briefRuleGroup(t, s, portsGID)
	if b2.BaselineRule == rule.RuleID {
		t.Fatalf("different ports must not match: %+v", b2)
	}
}

func seedScopedGroup(t tb, s *store.Store, scope, eventID, payload string) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	res, err := s.PersistEventAndGroup(context.Background(), store.RawEvent{
		EventID: eventID, EventKey: "k", EventType: "host.changed",
		OccurredAt: now, Payload: []byte(payload), ReceivedAt: now, SignatureTimestamp: now,
		SourceScope: scope,
	}, "host:w", store.GroupPolicy{QuietWindow: time.Millisecond, MaxWindow: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectReported(context.Background(), scope, 64); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return res.GroupID
}

func TestApplyGuardsCriticalAndLow(t *testing.T) {
	s := openWorkerStore(t)
	seedThreeExpected(t, s)
	now := time.Now()
	ps, err := DeriveProposals(context.Background(), s, 3, now)
	if err != nil || len(ps) != 1 {
		t.Fatalf("proposal missing: %+v %v", ps, err)
	}
	if _, _, err := ApproveProposal(context.Background(), s, ps[0].ProposalID, 3, -1, 90*24*time.Hour, now); err != nil {
		t.Fatal(err)
	}
	gid := seedRuleGroup(t, s, "evt-guard")
	taken, err := s.TakeGroup(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	// Critical is never overridden, and low needs no change: the rule
	// only ever lowers medium/high.
	crit := ApplyOperatorRules(context.Background(), s, taken, Outcome{Level: Critical, RuleID: "rule-x"}, now)
	if crit.Level != Critical || crit.RuleID != "rule-x" {
		t.Fatalf("critical must pass through: %+v", crit)
	}
	low := ApplyOperatorRules(context.Background(), s, taken, Outcome{Level: Low, RuleID: "rule-y"}, now)
	if low.Level != Low || low.RuleID != "rule-y" {
		t.Fatalf("low must pass through untagged: %+v", low)
	}
	mid := ApplyOperatorRules(context.Background(), s, taken, Outcome{Level: Medium, RuleID: "rule-z"}, now)
	if mid.Level != Low || !strings.HasPrefix(mid.RuleID, "OP_RULE_") {
		t.Fatalf("medium in scope must select low: %+v", mid)
	}
}

func TestRuleExpiryFailsClosed(t *testing.T) {
	s := openWorkerStore(t)
	seedThreeExpected(t, s)
	t0 := time.Now()
	ps, err := DeriveProposals(context.Background(), s, 3, t0)
	if err != nil || len(ps) != 1 {
		t.Fatalf("proposal missing: %+v %v", ps, err)
	}
	if _, _, err := ApproveProposal(context.Background(), s, ps[0].ProposalID, 3, 1, 90*24*time.Hour, t0); err != nil {
		t.Fatal(err)
	}
	// Past the one-day grant the scope re-proposes and new groups
	// brief on baseline alone: expiry voids application, it never
	// extends it. The worker clock is pinned past expiry so the
	// application path reads the lapsed grant.
	later := t0.Add(48 * time.Hour)
	if ps, err := DeriveProposals(context.Background(), s, 3, later); err != nil || len(ps) != 1 {
		t.Fatalf("lapsed grant must re-propose: %+v %v", ps, err)
	}
	d := testDeps(t, s)
	d.Now = func() time.Time { return later }
	gid := seedRuleGroup(t, s, "evt-r4")
	b := briefRuleGroupWith(t, s, d, gid)
	if b.Priority == "low" && strings.HasPrefix(b.BaselineRule, "OP_RULE_") {
		t.Fatalf("lapsed grant must not apply: %+v", b)
	}
}

func TestCanonicalChangeFields(t *testing.T) {
	view := func(hostname, os, device string) *hostView {
		return &hostView{Hostname: hostname, OS: os, Device: device, Ports: []portView{{Protocol: "tcp", Number: 22, State: "open"}}}
	}
	changed := func(prev, cur *hostView, changes ...string) []GroupEvent {
		return []GroupEvent{{
			EventType: "host.changed",
			Previous:  prev, Current: cur,
			Changes: changes,
		}}
	}
	// Producer-reported changes name fields directly.
	if got := canonicalChangeFields(changed(view("p1", "o1", ""), view("p2", "o2", ""), "hostname", "os")); got != "hostname,os" {
		t.Fatalf("reported changes must canonicalize: %q", got)
	}
	// Without changes[] each raw difference stands on its own: OS-only
	// and combined changes produce different field sets, so neither
	// borrows the other's scope.
	if got := canonicalChangeFields(changed(view("p1", "o1", ""), view("p2", "o2", ""))); got != "hostname,os" {
		t.Fatalf("os-only raw differences must canonicalize: %q", got)
	}
	if got := canonicalChangeFields(changed(view("p", "", "d1"), view("p", "", "d2"))); got != "device" {
		t.Fatalf("device-only raw differences must canonicalize: %q", got)
	}
	if got := canonicalChangeFields(changed(view("p1", "o1", "d1"), view("p2", "o2", "d2"))); got != "device,hostname,os" {
		t.Fatalf("combined raw differences must canonicalize: %q", got)
	}
	// Identical views carry no change; non-change events contribute
	// nothing on their own.
	if got := canonicalChangeFields([]GroupEvent{{EventType: "host.appeared", Host: view("p1", "o1", "")}}); got != "" {
		t.Fatalf("changeless events must canonicalize empty: %q", got)
	}
	// Hostile field names cannot bloat rule rows.
	long := []GroupEvent{{
		EventType: "host.changed",
		Previous:  view("p1", "o1", ""), Current: view("p2", "o1", ""),
		Changes: []string{strings.Repeat("f", 200)},
	}}
	if got := canonicalChangeFields(long); len([]rune(got)) != maxChangeFieldRunes {
		t.Fatalf("field names must truncate deterministically: %q", got)
	}
}

// ruleChangePayload varies hostname and OS independently: the kind
// precedence still selects hostname_change for mixed groups, but the
// changed-field set records every difference.
func ruleChangePayload(hostnameChanged, osChanged bool) string {
	prevHost, curHost := "p", "p"
	prevOS, curOS := "", ""
	if hostnameChanged {
		prevHost, curHost = "p1", "p2"
	}
	if osChanged {
		prevOS, curOS = "o1", "o2"
	}
	mkView := func(hostname, os string) string {
		return `{"ip":"192.168.0.5","hostname":"` + hostname + `","mac":"aa:bb:cc:dd:ee:01","host_id":"mac:aa:bb:cc:dd:ee:01","identity_source":"mac","identity_namespace":"` + ruleNamespace + `","identity_revision":"` + ruleRevision + `","os":"` + os + `","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-20T00:00:00Z","ports":[{"number":22,"protocol":"tcp","state":"open"}]}`
	}
	return `{"event_id":"e1","previous":` + mkView(prevHost, prevOS) + `,"current":` + mkView(curHost, curOS) + `}`
}

// A group carrying changes outside the approved set must not borrow
// its rule: the hostname-only grant stays put when OS changes ride
// along, and the mixed grant stays put for hostname-only reports.
func TestRuleMixedChangesVoid(t *testing.T) {
	s := openWorkerStore(t)
	bindRuleSource(t, s, "test-scope")
	seedHostGroups := func(ids []string, payload string, req string) {
		for i, id := range ids {
			gid := seedGroup(t, s, "host:w", id, "host.changed", payload)
			briefRuleGroup(t, s, gid)
			expectBriefing(t, s, gid, req+"-"+string(rune('a'+i)))
		}
	}
	now := time.Now()
	seedHostGroups([]string{"evt-h1", "evt-h2", "evt-h3"}, ruleChangePayload(true, false), "host")
	ps, err := DeriveProposals(context.Background(), s, 3, now)
	if err != nil || len(ps) != 1 {
		t.Fatalf("hostname proposal missing: %+v %v", ps, err)
	}
	if ps[0].ChangedFieldsCSV != "hostname" {
		t.Fatalf("hostname scope must record its fields: %+v", ps[0])
	}
	rule, applied, err := ApproveProposal(context.Background(), s, ps[0].ProposalID, 3, -1, 90*24*time.Hour, now)
	if err != nil || !applied {
		t.Fatalf("approve must apply: %+v %v", rule, err)
	}
	mixed := seedGroup(t, s, "host:w", "evt-m1", "host.changed", ruleChangePayload(true, true))
	b := briefRuleGroup(t, s, mixed)
	if b.Priority == "low" && b.BaselineRule == rule.RuleID {
		t.Fatalf("mixed changes must not borrow the hostname rule: %+v", b)
	}
}

// Concurrent approve and dismiss on one lapsed grant serialize on
// the writer: exactly one wins and the loser reports the conflict,
// never a silent overwrite.
func TestRuleLifecycleConcurrentApproveDismiss(t *testing.T) {
	s := openWorkerStore(t)
	bindRuleSource(t, s, "test-scope")
	seedHostGroups := func(ids []string, payload string, req string) {
		for i, id := range ids {
			gid := seedGroup(t, s, "host:w", id, "host.changed", payload)
			briefRuleGroup(t, s, gid)
			expectBriefing(t, s, gid, req+"-"+string(rune('a'+i)))
		}
	}
	seedHostGroups([]string{"evt-c1", "evt-c2", "evt-c3"}, rulePayload(), "conc")
	now := time.Now()
	ps, err := DeriveProposals(context.Background(), s, 3, now)
	if err != nil || len(ps) != 1 {
		t.Fatalf("proposal missing: %+v %v", ps, err)
	}
	pid := ps[0].ProposalID
	// Lapse the grant so both lifecycle paths stay resolvable: approve
	// re-grants, dismiss declines.
	if _, applied, err := ApproveProposal(context.Background(), s, pid, 3, 1, 90*24*time.Hour, now); err != nil || !applied {
		t.Fatalf("approve must apply: %v", err)
	}
	later := now.Add(48 * time.Hour)
	var wg sync.WaitGroup
	var approveErr, dismissErr error
	var approveApplied, dismissApplied bool
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, approveApplied, approveErr = ApproveProposal(context.Background(), s, pid, 3, -1, 90*24*time.Hour, later)
	}()
	go func() {
		defer wg.Done()
		_, dismissApplied, dismissErr = DismissProposal(context.Background(), s, pid, 3, later)
	}()
	wg.Wait()
	wins := 0
	if approveErr == nil && approveApplied {
		wins++
	}
	if dismissErr == nil && dismissApplied {
		wins++
	}
	if wins != 1 {
		t.Fatalf("exactly one lifecycle request must win: approve(%v,%v) dismiss(%v,%v)", approveApplied, approveErr, dismissApplied, dismissErr)
	}
	if approveErr != nil && approveErr != store.ErrRuleDismissed {
		t.Fatalf("losing approve must 409 dismissed: %v", approveErr)
	}
	if dismissErr != nil && dismissErr != store.ErrRuleActive {
		t.Fatalf("losing dismiss must 409 active: %v", dismissErr)
	}
	final, err := s.GetRuleByProposal(context.Background(), pid)
	if err != nil {
		t.Fatal(err)
	}
	if approveApplied && final.Status != store.RuleApproved {
		t.Fatalf("approved winner must persist: %+v", final)
	}
	if dismissApplied && final.Status != store.RuleDismissed {
		t.Fatalf("dismissed winner must persist: %+v", final)
	}
}
