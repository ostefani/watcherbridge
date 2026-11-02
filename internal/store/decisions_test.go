package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// dropDecisionSchema removes the v18 decision artifacts so a fixture
// that downgrades user_version below 18 re-migrates cleanly. It follows
// the repo convention of explicit downgrade drops (never IF NOT EXISTS
// in the migration itself): a real pre-18 database never has these
// objects, only a downgraded current one does.
func dropDecisionSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DROP TABLE IF EXISTS operator_decisions`); err != nil {
		t.Fatalf("drop decisions: %v", err)
	}
	dropColumnIfExists(t, db, "reported_subjects", "label_owner")
}

// Decision-aware overview reads (Step 25): flagged-first candidate
// order before the display cap, full-window effective counts with
// acknowledged/snoozed rows excluded and flagged counted separately,
// and the explained digest. Snooze lapse is verified through the real
// SQL read path, and the serving queries must stay indexed.
func seedDecidedBriefing(t *testing.T, s *Store, eventID, priority, kind string, at time.Time) string {
	t.Helper()
	ctx := context.Background()
	body := producerPayload(eventID, "k", "host.appeared", subjectMAC("aa:bb:cc:dd:ee:20"))
	gid := persistReported(t, s, "lab", eventID, "k", "host.appeared", body, at)
	projectAll(t, s)
	if _, err := s.CloseDue(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: priority, PriorityReason: "r", Kind: kind, Source: "rules",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	return gid
}

func TestDecisionAwareOverview(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	since := at.Add(-time.Hour)

	high := seedDecidedBriefing(t, s, "evt-ovd-1", "high", "port_closed", at)
	lowExp := seedDecidedBriefing(t, s, "evt-ovd-2", "low", "other", at)
	medFlag := seedDecidedBriefing(t, s, "evt-ovd-3", "medium", "hostname_change", at)
	lowSnooze := seedDecidedBriefing(t, s, "evt-ovd-4", "low", "other", at)

	mustAppend := func(in DecisionInput) {
		t.Helper()
		if _, err := s.AppendDecision(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	mustAppend(DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: lowExp, Kind: DecisionExpected})
	mustAppend(DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: medFlag, Kind: DecisionNotExpected})
	mustAppend(DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: lowSnooze,
		Kind: DecisionSnooze, HasUntil: true, Until: at.Add(time.Hour)})

	// Candidates order flagged first even though the flagged briefing
	// is medium and a high row exists; decision fields ride along.
	cands, err := s.AttentionCandidates(ctx, since, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 4 {
		t.Fatalf("candidates = %d, want 4", len(cands))
	}
	if cands[0].BriefingID != "brf-"+medFlag || !cands[0].Flagged {
		t.Fatalf("first candidate = %+v, want flagged medium", cands[0])
	}
	byID := map[string]AttentionCandidate{}
	for _, c := range cands {
		byID[c.BriefingID] = c
	}
	if !byID["brf-"+lowExp].Acknowledged {
		t.Fatalf("expected briefing not marked acknowledged: %+v", byID["brf-"+lowExp])
	}
	if byID["brf-"+lowSnooze].SnoozedUntil.IsZero() {
		t.Fatalf("snoozed briefing carries no until: %+v", byID["brf-"+lowSnooze])
	}
	if byID["brf-"+high].Flagged || byID["brf-"+high].Acknowledged {
		t.Fatalf("plain briefing carries decision state: %+v", byID["brf-"+high])
	}

	// Full-window counts exclude explained and deferred rows; the
	// flagged row votes only through the separate flagged count.
	counts, flagged, err := s.EffectivePriorityCounts(ctx, since, at)
	if err != nil {
		t.Fatal(err)
	}
	if flagged != 1 {
		t.Fatalf("flagged = %d, want 1", flagged)
	}
	if len(counts) != 1 || counts["high"] != 1 {
		t.Fatalf("effective counts = %v, want only the undecided high row", counts)
	}
	explained, err := s.ExplainedDigest(ctx, since, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(explained) != 1 || explained["other"] != 1 {
		t.Fatalf("explained = %v, want the one expected row by kind", explained)
	}

	// After the snooze lapses, the production read path resumes the
	// row into the verdict counts without any new event.
	counts, flagged, err = s.EffectivePriorityCounts(ctx, since, at.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if flagged != 1 || counts["high"] != 1 || counts["low"] != 1 {
		t.Fatalf("post-lapse counts = %v flagged %d, want high+low resumed", counts, flagged)
	}
}

func TestDecisionOverviewQueriesUseIndexes(t *testing.T) {
	s := openTestStore(t)
	queries := []string{
		`EXPLAIN QUERY PLAN WITH ` + effectiveReviewCTE + ` SELECT b.briefing_id FROM briefings b LEFT JOIN eff e ON e.subject_id = b.briefing_id WHERE b.created_at >= 0 ORDER BY (e.kind = 'not_expected') DESC LIMIT 500`,
		`EXPLAIN QUERY PLAN WITH ` + effectiveReviewCTE + ` SELECT b.priority, e.kind, COUNT(*) FROM briefings b LEFT JOIN eff e ON e.subject_id = b.briefing_id WHERE b.created_at >= 0 GROUP BY b.priority, e.kind`,
	}
	for _, q := range queries {
		rows, err := s.db.Query(q, 0)
		if err != nil {
			t.Fatal(err)
		}
		indexed := false
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if strings.Contains(detail, "SCAN operator_decisions") {
				rows.Close()
				t.Fatalf("decision read scans audit: %s", detail)
			}
			if strings.Contains(detail, "decisions_subject") {
				indexed = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !indexed {
			t.Fatalf("decision read must use the subject index: %s", q)
		}
	}
}

// Decision tests cover the Step 25 contract: the action-by-subject
// matrix, idempotent request IDs, explicit undo with ownership, lane
// isolation, snooze lapse through the production read path, and
// canonical briefing aliases. Notes and labels never touch review
// state; audit expiry is a retention concern, not a read concern.

// seedDecisionBriefing persists, projects, closes, takes and briefs
// one group, returning the group ID a decision addresses.
func seedDecisionBriefing(t *testing.T, s *Store, scope, eventID, hostJSON string, at time.Time) string {
	t.Helper()
	ctx := context.Background()
	body := producerPayload(eventID, "k", "host.appeared", hostJSON)
	gid := persistReported(t, s, scope, eventID, "k", "host.appeared", body, at)
	projectAll(t, s)
	if _, err := s.CloseDue(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: "r", Kind: "other", Source: "rules",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	return gid
}

// seedDecisionProfile projects one reported device and returns its
// generated profile ID.
func seedDecisionProfile(t *testing.T, s *Store, scope, eventID, mac string, at time.Time) string {
	t.Helper()
	seedSubject(t, s, scope, eventID, subjectMAC(mac), at)
	subs, _, err := s.ReportedSubjectsPage(context.Background(), 0, "", 10)
	if err != nil || len(subs) == 0 {
		t.Fatalf("seeded subjects = %d, %v", len(subs), err)
	}
	return subs[len(subs)-1].ProfileID
}

func TestDecisionMatrix(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	gid := seedDecisionBriefing(t, s, "lab", "evt-dec-1", subjectMAC("aa:bb:cc:dd:ee:11"), at)
	pid := seedDecisionProfile(t, s, "lab", "evt-dec-2", "aa:bb:cc:dd:ee:12", at)

	appendOK := func(in DecisionInput) Decision {
		t.Helper()
		res, err := s.AppendDecision(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Applied {
			t.Fatalf("append %+v not applied", in)
		}
		return res.Decision
	}
	appendOK(DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionExpected})
	appendOK(DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionNotExpected})
	future := at.Add(48 * time.Hour)
	appendOK(DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionSnooze, HasUntil: true, Until: future})
	appendOK(DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionNote, Text: "n"})
	appendOK(DecisionInput{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionNote, Text: "n"})
	appendOK(DecisionInput{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionLabel, Text: "printer"})

	for _, in := range []DecisionInput{
		{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionLabel, Text: "x"},
		{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionExpected},
		{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionNotExpected},
		{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionSnooze, HasUntil: true, Until: future},
		{SubjectKind: "incident", SubjectID: gid, Kind: DecisionNote, Text: "x"},
		{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionSnooze},
		{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionSnooze, HasUntil: true, Until: at.Add(-time.Hour)},
		{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionNote},
		{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionLabel, Text: "   "},
		{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionLabel, Text: strings.Repeat("x", maxLabelRunes+1)},
		{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionExpected, Text: "extra"},
		{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionSnooze, HasUntil: true, Until: future, Text: "extra", RequestID: "bad id!"},
		{SubjectKind: DecisionSubjectBriefing, SubjectID: "grp-nope", Kind: DecisionNote, Text: "x"},
		{SubjectKind: DecisionSubjectProfile, SubjectID: "prof-0000000000000000", Kind: DecisionNote, Text: "x"},
	} {
		if _, err := s.AppendDecision(ctx, in); err == nil {
			t.Fatalf("append %+v unexpectedly valid", in)
		}
	}

	// The label materialized onto the exact profile.
	sub, err := s.ReportedProfile(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if sub.OperatorLabel == nil || *sub.OperatorLabel != "printer" {
		t.Fatalf("materialized label = %v", sub.OperatorLabel)
	}
	if sub.LabelOwner == "" {
		t.Fatal("label owner not recorded")
	}
}

func TestDecisionRequestID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	gid := seedDecisionBriefing(t, s, "lab", "evt-dec-3", subjectMAC("aa:bb:cc:dd:ee:13"), at)

	in := DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionNote, Text: "n", RequestID: "req-1"}
	first, err := s.AppendDecision(ctx, in)
	if err != nil || !first.Applied {
		t.Fatalf("first append = %v, %v", first, err)
	}
	again, err := s.AppendDecision(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if again.Applied || again.Reason != "duplicate_request" || again.Decision.DecisionID != first.Decision.DecisionID {
		t.Fatalf("replay = %+v, want idempotent no-op", again)
	}
	in.Text = "different"
	if _, err := s.AppendDecision(ctx, in); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflicting reuse = %v, want ErrRequestConflict", err)
	}
}

func TestDecisionUndoReview(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	gid := seedDecisionBriefing(t, s, "lab", "evt-dec-4", subjectMAC("aa:bb:cc:dd:ee:14"), at)

	exp, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionExpected})
	if err != nil {
		t.Fatal(err)
	}
	eff, err := s.EffectiveReview(ctx, DecisionSubjectBriefing, []string{"brf-" + gid}, at)
	if err != nil {
		t.Fatal(err)
	}
	if eff["brf-"+gid].Kind != DecisionExpected {
		t.Fatalf("effective = %+v, want expected", eff["brf-"+gid])
	}
	undo, err := s.UndoDecision(ctx, exp.Decision.DecisionID, "")
	if err != nil || !undo.Applied {
		t.Fatalf("undo = %v, %v", undo, err)
	}
	eff, err = s.EffectiveReview(ctx, DecisionSubjectBriefing, []string{"brf-" + gid}, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := eff["brf-"+gid]; ok {
		t.Fatalf("effective after undo = %+v, want none", eff["brf-"+gid])
	}
	// Notes are unaffected by review undo.
	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionNote, Text: "kept"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.DecisionsForSubject(ctx, DecisionSubjectBriefing, "brf-"+gid, 10)
	if err != nil {
		t.Fatal(err)
	}
	var sawUndone, sawLive bool
	for _, d := range list {
		if d.Kind == DecisionExpected && d.Undone {
			sawUndone = true
		}
		if d.Kind == DecisionNote && !d.Undone {
			sawLive = true
		}
	}
	if !sawUndone || !sawLive {
		t.Fatalf("trail = %+v, want undone expected plus live note", list)
	}

	// Undoing the neutralized target again is a no-op, not an error.
	again, err := s.UndoDecision(ctx, exp.Decision.DecisionID, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.Applied || again.Reason != "already_undone" {
		t.Fatalf("second undo = %+v, want applied:false", again)
	}
	// Undo rows are terminal; unknown targets are unknown.
	if _, err := s.UndoDecision(ctx, undo.Decision.DecisionID, ""); !errors.Is(err, ErrInvalidUndo) {
		t.Fatalf("undo-of-undo = %v, want ErrInvalidUndo", err)
	}
	if _, err := s.UndoDecision(ctx, "dec-0-0", ""); !errors.Is(err, ErrUnknownDecision) {
		t.Fatalf("unknown undo = %v, want ErrUnknownDecision", err)
	}
}

func TestDecisionLabelUndoChain(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	pid := seedDecisionProfile(t, s, "lab", "evt-dec-5", "aa:bb:cc:dd:ee:15", at)

	a, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionLabel, Text: "A"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectProfile, SubjectID: pid, Kind: DecisionLabel, Text: "B"})
	if err != nil {
		t.Fatal(err)
	}
	labelOf := func() (string, string) {
		t.Helper()
		sub, err := s.ReportedProfile(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		if sub.OperatorLabel == nil {
			return "", sub.LabelOwner
		}
		return *sub.OperatorLabel, sub.LabelOwner
	}
	if v, o := labelOf(); v != "B" || o != b.Decision.DecisionID {
		t.Fatalf("label = %q owner %q, want B owned by B", v, o)
	}
	// A concurrent older label can no longer win: undoing A while B
	// owns the head is a conflict, not a silent overwrite.
	if _, err := s.UndoDecision(ctx, a.Decision.DecisionID, ""); !errors.Is(err, ErrUndoConflict) {
		t.Fatalf("undo stale head = %v, want ErrUndoConflict", err)
	}
	// Undoing B restores A and transfers ownership back to A.
	if _, err := s.UndoDecision(ctx, b.Decision.DecisionID, ""); err != nil {
		t.Fatal(err)
	}
	if v, o := labelOf(); v != "A" || o != a.Decision.DecisionID {
		t.Fatalf("after undo B: label = %q owner %q, want A owned by A", v, o)
	}
	// The chain continues: undoing A clears to no label.
	if _, err := s.UndoDecision(ctx, a.Decision.DecisionID, ""); err != nil {
		t.Fatal(err)
	}
	sub, err := s.ReportedProfile(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if sub.OperatorLabel != nil || sub.LabelOwner != "" {
		t.Fatalf("after undo A: label = %v owner %q, want cleared", sub.OperatorLabel, sub.LabelOwner)
	}
}

func TestDecisionSnoozeLapse(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	gid := seedDecisionBriefing(t, s, "lab", "evt-dec-6", subjectMAC("aa:bb:cc:dd:ee:16"), at)

	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionExpected}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid,
		Kind: DecisionSnooze, HasUntil: true, Until: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Through the production read path: active snooze wins...
	eff, err := s.EffectiveReview(ctx, DecisionSubjectBriefing, []string{"brf-" + gid}, at.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if eff["brf-"+gid].Kind != DecisionSnooze {
		t.Fatalf("effective = %+v, want snooze", eff["brf-"+gid])
	}
	// ...and after expiry the earlier expected resumes instead of
	// forcing attention.
	eff, err = s.EffectiveReview(ctx, DecisionSubjectBriefing, []string{"brf-" + gid}, at.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if eff["brf-"+gid].Kind != DecisionExpected {
		t.Fatalf("effective after lapse = %+v, want expected", eff["brf-"+gid])
	}
}

func TestDecisionCanonicalAlias(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	gid := seedDecisionBriefing(t, s, "lab", "evt-dec-7", subjectMAC("aa:bb:cc:dd:ee:17"), at)

	byGroup, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionNote, Text: "g"})
	if err != nil {
		t.Fatal(err)
	}
	byBriefing, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: "brf-" + gid, Kind: DecisionNote, Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if byGroup.Decision.SubjectID != "brf-"+gid || byBriefing.Decision.SubjectID != "brf-"+gid {
		t.Fatalf("subjects = %q/%q, want single canonical %q",
			byGroup.Decision.SubjectID, byBriefing.Decision.SubjectID, "brf-"+gid)
	}
	list, err := s.DecisionsForSubject(ctx, DecisionSubjectBriefing, "brf-"+gid, 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("trail = %+v, %v, want both notes on one subject", list, err)
	}
}
