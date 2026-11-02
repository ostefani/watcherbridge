package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func ruleFixture(now time.Time) Rule {
	return Rule{
		RuleID: "OP_RULE_aaaaaaaaaaaaaaaa", ProposalID: "OP_PROP_aaaaaaaaaaaaaaaa",
		SourceScope: "test-scope", ReferenceClass: SubjectReportedDevice,
		IdentityNamespace: "", UpstreamReference: "mac:aa:bb:cc:dd:ee:01",
		ReportedIdentitySource: "mac", ReportedConfidence: "high",
		Kind: "other", PortsCSV: "tcp/22/open",
		Status: RuleApproved, DecidedAt: now, ApprovedAt: now, HasApprovedAt: true,
	}
}

func TestRuleMatchIsExact(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	base := ruleFixture(now)
	if err := s.PutRule(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	match := func(r Rule) bool {
		_, found, err := s.MatchRule(context.Background(), r.SourceScope, r.ReferenceClass, r.IdentityNamespace,
			r.UpstreamReference, r.ReportedIdentitySource, r.ReportedConfidence, r.Kind, r.PortsCSV, now)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	if !match(base) {
		t.Fatal("exact scope must match")
	}
	// Every scope field drifts the match closed: source, namespace,
	// reference, identity standing, kind and ports each void alone.
	drifted := base
	drifted.SourceScope = "other-scope"
	if match(drifted) {
		t.Fatal("cross-source must not match")
	}
	drifted = base
	drifted.IdentityNamespace = "ns2"
	if match(drifted) {
		t.Fatal("namespace drift must not match")
	}
	drifted = base
	drifted.UpstreamReference = "mac:aa:bb:cc:dd:ee:02"
	if match(drifted) {
		t.Fatal("reference drift must not match")
	}
	drifted = base
	drifted.ReportedConfidence = "low"
	if match(drifted) {
		t.Fatal("lowered confidence must not match")
	}
	drifted = base
	drifted.ReportedConfidence = ""
	if match(drifted) {
		t.Fatal("unknown confidence must not match")
	}
	drifted = base
	drifted.ReportedIdentitySource = "ip"
	if match(drifted) {
		t.Fatal("source drift must not match")
	}
	drifted = base
	drifted.Kind = "port_closed"
	if match(drifted) {
		t.Fatal("kind drift must not match")
	}
	drifted = base
	drifted.PortsCSV = "tcp/22/open,tcp/80/open"
	if match(drifted) {
		t.Fatal("port drift must not match")
	}
	drifted = base
	drifted.ReferenceClass = SubjectAddress
	if match(drifted) {
		t.Fatal("address class must not match")
	}
}

func TestRuleMatchRespectsLifecycle(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	match := func() bool {
		r := ruleFixture(now)
		_, found, err := s.MatchRule(context.Background(), r.SourceScope, r.ReferenceClass, r.IdentityNamespace,
			r.UpstreamReference, r.ReportedIdentitySource, r.ReportedConfidence, r.Kind, r.PortsCSV, now)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	// No-expiry grants match indefinitely.
	base := ruleFixture(now)
	base.HasApprovedAt = false
	if err := s.PutRule(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if !match() {
		t.Fatal("no-expiry grant must match")
	}
	// Terminal states never match, even unexpired.
	for _, status := range []string{RuleDismissed, RuleRevoked} {
		r := ruleFixture(now)
		r.RuleID, r.ProposalID = "OP_RULE_"+status, "OP_PROP_"+status
		r.Status = status
		if err := s.PutRule(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.GetRule(context.Background(), "OP_RULE_dismissed"); err != nil || got.Status != RuleDismissed {
		t.Fatalf("dismissed row must read: %+v %v", got, err)
	}
	// An expired grant misses at application time. It rides its own
	// scope so the earlier no-expiry grant cannot mask the miss.
	exp := ruleFixture(now)
	exp.RuleID, exp.ProposalID = "OP_RULE_exp1", "OP_PROP_exp1"
	exp.UpstreamReference = "mac:aa:bb:cc:dd:ee:09"
	past := now.Add(-time.Hour)
	exp.ExpiresAt, exp.HasExpiresAt = past, true
	if err := s.PutRule(context.Background(), exp); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.MatchRule(context.Background(), exp.SourceScope, exp.ReferenceClass, exp.IdentityNamespace,
		exp.UpstreamReference, exp.ReportedIdentitySource, exp.ReportedConfidence, exp.Kind, exp.PortsCSV, now); err != nil || found {
		t.Fatalf("expired grant must miss: %v %v", found, err)
	}
	if _, err := s.GetRuleByProposal(context.Background(), "OP_PROP_missing"); err != sql.ErrNoRows {
		t.Fatalf("unknown proposal must miss: %v", err)
	}
}

func TestRulePurgeTerminalAndProtected(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-2 * time.Hour)
	// Dismissed and revoked rows past the horizon drain.
	for i, status := range []string{RuleDismissed, RuleRevoked} {
		r := ruleFixture(old)
		r.RuleID, r.ProposalID = "OP_RULE_old"+string(rune('a'+i)), "OP_PROP_old"+string(rune('a'+i))
		r.Status, r.DecidedAt = status, old
		r.HasApprovedAt = false
		if err := s.PutRule(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	// An expired approved row referenced by a retained briefing is
	// protected: it retires with its briefing, never before.
	exp := ruleFixture(old)
	exp.RuleID, exp.ProposalID = "OP_RULE_kept", "OP_PROP_kept"
	exp.ExpiresAt, exp.HasExpiresAt = old.Add(time.Hour), true
	if err := s.PutRule(context.Background(), exp); err != nil {
		t.Fatal(err)
	}
	gid := seedFeedGroup(t, s, "host:a", "evt-a1")
	takeFeedGroup(t, s, gid)
	if err := s.InsertBriefing(context.Background(), Briefing{
		BriefingID: "brf-" + gid, GroupID: gid, HostAnchorKey: "host:a",
		Baseline: "low", Priority: "low", PriorityReason: "r", BaselineRule: exp.RuleID,
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// A live grant is never eligible, however old its approval.
	live := ruleFixture(old)
	live.RuleID, live.ProposalID = "OP_RULE_live", "OP_PROP_live"
	live.HasApprovedAt = false
	if err := s.PutRule(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	rep, err := s.PurgeOnce(context.Background(), PurgeSettings{
		BriefingRetention: 24 * time.Hour, SubjectRetention: 24 * time.Hour,
		DecisionRetention: 24 * time.Hour, RuleRetention: time.Hour,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rules != 2 {
		t.Fatalf("only terminal unreferenced rows must drain: %+v", rep)
	}
	for _, id := range []string{"OP_RULE_kept", "OP_RULE_live"} {
		if _, err := s.GetRule(context.Background(), id); err != nil {
			t.Fatalf("protected/live rule %s must survive: %v", id, err)
		}
	}
	run, err := s.LatestPurgeRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.total.Rules != 2 {
		t.Fatalf("ledger must attribute rule deletes: %+v", run.total)
	}
}

func TestExpectedBriefingDecisionsPage(t *testing.T) {
	s := openTestStore(t)
	gid := seedFeedGroup(t, s, "host:a", "evt-a1")
	takeFeedGroup(t, s, gid)
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.InsertBriefing(context.Background(), Briefing{
		BriefingID: "brf-" + gid, GroupID: gid, HostAnchorKey: "host:a",
		Baseline: "low", Priority: "low", PriorityReason: "r",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for _, req := range []string{"pg-1", "pg-2", "pg-3"} {
		if _, err := s.AppendDecision(context.Background(), DecisionInput{
			SubjectKind: DecisionSubjectBriefing, SubjectID: "brf-" + gid,
			Kind: DecisionExpected, RequestID: req,
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.ExpectedBriefingDecisions(context.Background(), 0, 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("first page = %+v %v", first, err)
	}
	second, err := s.ExpectedBriefingDecisions(context.Background(), first[1].RowID, 2)
	if err != nil || len(second) != 1 || second[0].BriefingID != "brf-"+gid {
		t.Fatalf("second page = %+v %v", second, err)
	}
	// Notes and other kinds never enter the derivation scan.
	if _, err := s.AppendDecision(context.Background(), DecisionInput{
		SubjectKind: DecisionSubjectBriefing, SubjectID: "brf-" + gid,
		Kind: DecisionNote, Text: "n", RequestID: "pg-note",
	}); err != nil {
		t.Fatal(err)
	}
	all, err := s.ExpectedBriefingDecisions(context.Background(), 0, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("scan must stay expected-only: %+v %v", all, err)
	}
	if _, err := s.BriefingByID(context.Background(), "brf-"+gid); err != nil {
		t.Fatalf("briefing by id must read: %v", err)
	}
	if _, err := s.BriefingByID(context.Background(), "brf-missing"); err != sql.ErrNoRows {
		t.Fatalf("missing briefing must miss: %v", err)
	}
	payloads, err := s.GroupPayloads(context.Background(), gid)
	if err != nil || len(payloads) != 1 || len(payloads[0].Payload) == 0 {
		t.Fatalf("group payloads must read: %+v %v", payloads, err)
	}
}
