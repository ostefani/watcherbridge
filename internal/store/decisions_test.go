package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

	// Candidates carry only eligible rows: flagged first even though
	// the flagged briefing is medium and a high row exists, then the
	// undecided high row. The acknowledged and actively-snoozed rows
	// never consume cap slots — they vote only through the counts.
	cands, err := s.AttentionCandidates(ctx, since, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want flagged + undecided", len(cands))
	}
	if cands[0].BriefingID != "brf-"+medFlag || !cands[0].Flagged {
		t.Fatalf("first candidate = %+v, want flagged medium", cands[0])
	}
	if cands[1].BriefingID != "brf-"+high || cands[1].Flagged {
		t.Fatalf("second candidate = %+v, want undecided high", cands[1])
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

// Decided rows must not consume candidate cap slots: with a cap of
// one, an acknowledged briefing loses to an undecided one even
// though the old NULL-last ordering would have ranked the decided
// row first. Flagged rows still win the single slot.
func TestAttentionCandidatesSkipDecided(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	since := at.Add(-time.Hour)
	decided := seedDecidedBriefing(t, s, "evt-ovd-c1", "low", "other", at)
	open := seedDecidedBriefing(t, s, "evt-ovd-c2", "low", "other", at)
	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: decided, Kind: DecisionExpected}); err != nil {
		t.Fatal(err)
	}
	cands, err := s.AttentionCandidates(ctx, since, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].BriefingID != "brf-"+open {
		t.Fatalf("capped candidates = %+v, want only the undecided row", cands)
	}
	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: decided, Kind: DecisionNotExpected}); err != nil {
		t.Fatal(err)
	}
	cands, err = s.AttentionCandidates(ctx, since, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].BriefingID != "brf-"+decided || !cands[0].Flagged {
		t.Fatalf("capped candidates = %+v, want only the flagged row", cands)
	}
}

func TestDecisionOverviewQueriesUseIndexes(t *testing.T) {
	s := openTestStore(t)
	queries := []string{
		`EXPLAIN QUERY PLAN WITH ` + effectiveReviewCTE + ` SELECT b.briefing_id FROM briefings b LEFT JOIN eff e ON e.subject_id = b.briefing_id WHERE b.created_at >= 0 AND (e.kind IS NULL OR e.kind = 'not_expected') ORDER BY CASE WHEN e.kind = 'not_expected' THEN 0 ELSE 1 END LIMIT 500`,
		`EXPLAIN QUERY PLAN WITH ` + effectiveReviewCTE + ` SELECT b.priority, e.kind, COUNT(*) FROM briefings b LEFT JOIN eff e ON e.subject_id = b.briefing_id WHERE b.created_at >= 0 GROUP BY b.priority, e.kind`,
	}
	for _, q := range queries {
		rows, err := s.db.Query(q, 0, 0)
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
	list, err := s.DecisionsForSubject(ctx, DecisionSubjectBriefing, "brf-"+gid, 10, 0, 0)
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
	list, err := s.DecisionsForSubject(ctx, DecisionSubjectBriefing, "brf-"+gid, 10, 0, 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("trail = %+v, %v, want both notes on one subject", list, err)
	}
}

// Undoing a superseded review record conflicts: only the effective
// head can be undone. Repeating the head undo replays as
// already_undone, and notes carry no head, so any live note undoes.
func TestUndoReviewStaleHead(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	gid := seedDecisionBriefing(t, s, "lab", "evt-dec-8", subjectMAC("aa:bb:cc:dd:ee:18"), at)

	append := func(kind, req string) DecisionResult {
		t.Helper()
		res, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: kind, RequestID: req})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	first := append(DecisionExpected, "req-sh-1")
	second := append(DecisionNotExpected, "req-sh-2")
	if _, err := s.UndoDecision(ctx, first.Decision.DecisionID, "req-sh-u1"); !errors.Is(err, ErrUndoConflict) {
		t.Fatalf("undo of superseded review = %v, want conflict", err)
	}
	if _, err := s.UndoDecision(ctx, second.Decision.DecisionID, "req-sh-u2"); err != nil {
		t.Fatalf("undo of head = %v, want success", err)
	}
	again, err := s.UndoDecision(ctx, second.Decision.DecisionID, "req-sh-u3")
	if err != nil || again.Applied || again.Reason != "already_undone" {
		t.Fatalf("repeat undo = %+v, %v; want already_undone", again, err)
	}
	n1, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionNote, Text: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionNote, Text: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UndoDecision(ctx, n1.Decision.DecisionID, ""); err != nil {
		t.Fatalf("undo of older note = %v, want success", err)
	}
}

// Repeating an undo is idempotent only while state is unchanged
// since that undo: A → B → undo B → C → undo B again conflicts,
// because the head moved on. Redo the sequence without C and the
// repeat replays as already_undone.
func TestUndoRepeatAfterHeadMoved(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	gid := seedDecisionBriefing(t, s, "lab", "evt-dec-12", subjectMAC("aa:bb:cc:dd:ee:1c"), at)

	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionExpected}); err != nil {
		t.Fatal(err)
	}
	b, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionNotExpected})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UndoDecision(ctx, b.Decision.DecisionID, "req-um-1"); err != nil {
		t.Fatal(err)
	}
	// Unchanged repeat: same new request, nothing committed since.
	steady, err := s.UndoDecision(ctx, b.Decision.DecisionID, "req-um-2")
	if err != nil || steady.Applied || steady.Reason != "already_undone" {
		t.Fatalf("unchanged repeat = %+v, %v; want already_undone", steady, err)
	}
	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionExpected}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UndoDecision(ctx, b.Decision.DecisionID, "req-um-3"); !errors.Is(err, ErrUndoConflict) {
		t.Fatalf("repeat after head moved = %v, want conflict", err)
	}
}

// An undo of a newer record moves the head without adding a live
// row: A → B → undo B → undo A → repeat undo B conflicts, because
// the head changed from A to none even though no later live review
// row exists. The exact original request still replays idempotently.
func TestUndoRepeatAfterUndoMovedHead(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	gid := seedDecisionBriefing(t, s, "lab", "evt-dec-13", subjectMAC("aa:bb:cc:dd:ee:1d"), at)

	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionExpected}); err != nil {
		t.Fatal(err)
	}
	b, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionNotExpected})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UndoDecision(ctx, b.Decision.DecisionID, "req-uu-1"); err != nil {
		t.Fatal(err)
	}
	a, err := s.DecisionsForSubject(ctx, DecisionSubjectBriefing, "brf-"+gid, 50, 0, 0)
	if err != nil || len(a) == 0 {
		t.Fatalf("trail = %+v, %v", a, err)
	}
	var headID string
	for _, d := range a {
		if d.Kind == DecisionExpected && !d.Undone {
			headID = d.DecisionID
		}
	}
	if headID == "" {
		t.Fatal("no live expected head to undo")
	}
	if _, err := s.UndoDecision(ctx, headID, "req-uu-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UndoDecision(ctx, b.Decision.DecisionID, "req-uu-3"); !errors.Is(err, ErrUndoConflict) {
		t.Fatalf("repeat after undo moved head = %v, want conflict", err)
	}
	replay, err := s.UndoDecision(ctx, b.Decision.DecisionID, "req-uu-1")
	if err != nil || replay.Applied || replay.Reason != "duplicate_request" {
		t.Fatalf("exact replay = %+v, %v; want duplicate", replay, err)
	}
}

// A default snooze resolves its horizon on first commit only: the
// idempotency hash covers specified content, so a retry replays
// against the original row instead of hashing a new deadline. An
// explicit snooze retried after its horizon still replays instead of
// failing validation.
func TestAppendSnoozeIdempotentRetry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	gid := seedDecisionBriefing(t, s, "lab", "evt-dec-9", subjectMAC("aa:bb:cc:dd:ee:19"), at)

	first, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid,
		Kind: DecisionSnooze, RequestID: "req-sn-1", SnoozeDefault: 24 * time.Hour})
	if err != nil || !first.Applied {
		t.Fatalf("default snooze = %+v, %v; want applied", first, err)
	}
	second, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid,
		Kind: DecisionSnooze, RequestID: "req-sn-1", SnoozeDefault: 24 * time.Hour})
	if err != nil || second.Applied || second.Reason != "duplicate_request" {
		t.Fatalf("default snooze retry = %+v, %v; want duplicate replay", second, err)
	}
	if !second.Decision.Until.Equal(first.Decision.Until) {
		t.Fatalf("replay until = %v, want original %v", second.Decision.Until, first.Decision.Until)
	}
	explicit, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid,
		Kind: DecisionSnooze, HasUntil: true, Until: at.Add(time.Hour), RequestID: "req-sn-2"})
	if err != nil || !explicit.Applied {
		t.Fatalf("explicit snooze = %+v, %v; want applied", explicit, err)
	}
	// The horizon lapses after the commit; the retry carries the same
	// specified content and must replay, not fail validation.
	if _, err := s.db.Exec(`UPDATE operator_decisions SET until = ? WHERE decision_id = ?`,
		nanos(at.Add(-time.Hour)), explicit.Decision.DecisionID); err != nil {
		t.Fatal(err)
	}
	retry, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid,
		Kind: DecisionSnooze, HasUntil: true, Until: at.Add(time.Hour), RequestID: "req-sn-2"})
	if err != nil || retry.Applied || retry.Reason != "duplicate_request" {
		t.Fatalf("lapsed snooze retry = %+v, %v; want duplicate replay", retry, err)
	}
}

// Audit past the retention cutoff stops applying even before purge
// deletes it, so a stalled purge never extends a verdict.
func TestEffectiveReviewIgnoresExpiredAudit(t *testing.T) {
	s := openTestStore(t)
	s.SetDecisionRetention(24 * time.Hour)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-25 * time.Hour)
	since := now.Add(-time.Hour)
	gid := seedDecisionBriefing(t, s, "lab", "evt-dec-10", subjectMAC("aa:bb:cc:dd:ee:1a"), now)

	if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid, Kind: DecisionExpected}); err != nil {
		t.Fatal(err)
	}
	if eff, err := s.EffectiveReview(ctx, DecisionSubjectBriefing, []string{"brf-" + gid}, now); err != nil || eff["brf-"+gid].Kind != DecisionExpected {
		t.Fatalf("fresh effective = %+v, %v; want expected", eff, err)
	}
	if _, err := s.db.Exec(`UPDATE operator_decisions SET created_at = ?`, nanos(old)); err != nil {
		t.Fatal(err)
	}
	if eff, err := s.EffectiveReview(ctx, DecisionSubjectBriefing, []string{"brf-" + gid}, now); err != nil || len(eff) != 0 {
		t.Fatalf("expired effective = %+v, %v; want empty", eff, err)
	}
	cands, err := s.AttentionCandidates(ctx, since, 500)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cands {
		if c.BriefingID == "brf-"+gid {
			found = true
		}
	}
	if !found {
		t.Fatalf("briefing with expired audit missing from candidates")
	}
}

// A still-current head or label pushed past the first trail page
// stays reachable: the before cursor pages strictly older rows with
// no gaps and no repeats.
func TestDecisionsForSubjectPages(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	gid := seedDecisionBriefing(t, s, "lab", "evt-dec-11", subjectMAC("aa:bb:cc:dd:ee:1b"), at)
	for i := 0; i < 55; i++ {
		if _, err := s.AppendDecision(ctx, DecisionInput{SubjectKind: DecisionSubjectBriefing, SubjectID: gid,
			Kind: DecisionNote, Text: fmt.Sprintf("n%02d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.DecisionsForSubject(ctx, DecisionSubjectBriefing, "brf-"+gid, 50, 0, 0)
	if err != nil || len(first) != 50 {
		t.Fatalf("first page = %d, %v; want 50", len(first), err)
	}
	last := first[len(first)-1]
	second, err := s.DecisionsForSubject(ctx, DecisionSubjectBriefing, "brf-"+gid, 50, last.CreatedAt.UnixNano(), last.RowID)
	if err != nil || len(second) != 5 {
		t.Fatalf("second page = %d, %v; want 5", len(second), err)
	}
	seen := map[string]bool{}
	for _, d := range append(first, second...) {
		if seen[d.DecisionID] {
			t.Fatalf("duplicate row %q across pages", d.DecisionID)
		}
		seen[d.DecisionID] = true
	}
	if len(seen) != 55 {
		t.Fatalf("paged rows = %d, want 55", len(seen))
	}
	if second[len(second)-1].Text != "n00" {
		t.Fatalf("oldest row = %q, want n00", second[len(second)-1].Text)
	}
}
