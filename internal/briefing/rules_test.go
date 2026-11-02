package briefing

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

// rulePayload reports one open port on the shared MAC: every group
// seeded with it shares one reported_device scope, one kind and one
// canonical port set. The host_id/identity_source pair is what makes
// the MAC a scanner-reported reference rather than a bare attribute —
// without it the views project event-local and no rule can attach.
func rulePayload() string {
	view := `{"ip":"192.168.0.5","hostname":"p","mac":"aa:bb:cc:dd:ee:01","host_id":"mac:aa:bb:cc:dd:ee:01","identity_source":"mac","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-20T00:00:00Z","ports":[%s]}`
	open := `{"number":22,"protocol":"tcp","state":"open"}`
	return `{"event_id":"e1","previous":` + strings.Replace(view, "%s", "", 1) +
		`,"current":` + strings.Replace(view, "%s", open, 1) + `}`
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
	view := `{"ip":"192.168.0.5","hostname":"p","mac":"aa:bb:cc:dd:ee:01","host_id":"mac:aa:bb:cc:dd:ee:01","identity_source":"mac","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-20T00:00:00Z","ports":[%s]}`
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
