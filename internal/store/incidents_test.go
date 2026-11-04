package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// dropIncidentSchema rewinds the Step-29 presentation-incident
// registry (v26 incident tables, progress cursor and ledger columns)
// so migration-chain tests that rewind to older versions replay
// cleanly through the current version on reopen. The operator_decisions
// table itself stays: replaying v26 rebuilds it from whatever shape
// the rewind left, so no decisions downgrade is needed. It follows the
// repo convention of explicit downgrade drops: a real pre-26 database
// never has these objects, only a downgraded current one does.
func dropIncidentSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS incident_pending_refs`,
		`DROP TABLE IF EXISTS incident_pending`,
		`DROP TABLE IF EXISTS incident_references`,
		`DROP TABLE IF EXISTS incident_members`,
		`DROP TABLE IF EXISTS incident_summaries`,
		`DROP TABLE IF EXISTS incidents`,
		`DROP TABLE IF EXISTS incident_progress`,
		`DROP INDEX IF EXISTS incidents_status_closed`,
		`DROP INDEX IF EXISTS incident_summaries_status`,
		`DROP INDEX IF EXISTS decisions_generated`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("drop incidents: %v", err)
		}
	}
	dropColumnIfExists(t, db, "purge_runs", "incidents_deleted")
	dropColumnIfExists(t, db, "purge_runs", "incident_members_deleted")
	for _, col := range []string{"incident_pending_deleted", "incident_pending_refs_deleted", "incident_refs_deleted", "summaries_deleted"} {
		dropColumnIfExists(t, db, "purge_runs", col)
	}
}

// incidentHostJSON renders one scanner-reported host view carrying a
// stable-identity reference: mac source, bound namespace, revision and
// confidence travel verbatim through projection into the subject row.
func incidentHostJSON(ref, confidence string) string {
	return fmt.Sprintf(`{"host_id":%q,"identity_source":"mac","identity_namespace":"ns-1","identity_revision":"r1","identity_confidence":%q}`,
		ref, confidence)
}

// seedIncidentBriefing persists one appeared/disappeared event with its
// own target, projects it, closes and takes the group, and completes
// a briefing at the given completion time. It returns the briefing ID.
// Each distinct ref founds its own group; repeats of one ref join the
// open group while its quiet window holds, unless maxEvents forces a
// rollover (used to produce several briefings for one reference).
func seedIncidentBriefing(t testing.TB, s *Store, scope, target, eventType, ref string, at time.Time, priority string) string {
	return seedIncidentBriefingMaxEvents(t, s, scope, target, eventType, ref, at, priority, 0)
}

func seedIncidentBriefingMaxEvents(t testing.TB, s *Store, scope, target, eventType, ref string, at time.Time, priority string, maxEvents int) string {
	t.Helper()
	ctx := context.Background()
	eventID := fmt.Sprintf("evt-%s-%s-%s-%d", scope, target, ref, at.UnixNano())
	eventKey := fmt.Sprintf("k-%s-%s-%s", scope, target, ref)
	payload := []byte(fmt.Sprintf(`{"event_id":%q,"event_key":%q,"event_type":%q,"occurred_at":"2026-10-20T12:00:05Z","cycle":7,"target":%q,"host":%s}`,
		eventID, eventKey, eventType, target, incidentHostJSON(ref, "high")))
	anchor, _ := GroupKeyForEvent(scope, eventType, eventID, payload)
	res, err := s.PersistEventAndGroup(ctx, RawEvent{
		EventID: eventID, EventKey: eventKey, EventType: eventType,
		OccurredAt: at, Cycle: 1, Target: target, Payload: payload,
		ReceivedAt: at, SignatureTimestamp: at, SourceScope: scope,
	}, anchor, GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour, MaxEvents: maxEvents, GroupingVersion: GroupingScoped})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectReported(ctx, scope, 64); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeGroup(ctx, res.GroupID); err != nil {
		t.Fatal(err)
	}
	bid := "brf-" + res.GroupID
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: bid, GroupID: res.GroupID, HostAnchorKey: anchor,
		Baseline: "low", Priority: priority, PriorityReason: "test",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	return bid
}

// bindIncidentSource binds the stable-identity contract the incident
// gate requires: a configured namespace plus the advertised
// capability. Without it references stay uncounted by design.
func bindIncidentSource(t testing.TB, s *Store, scope string) {
	t.Helper()
	if err := s.BindSourceIdentity(context.Background(), scope, "ns-1", StableIdentityCapability); err != nil {
		t.Fatal(err)
	}
}

func incidentSettings() IncidentSettings {
	return IncidentSettings{Window: 5 * time.Minute, Quiet: 120 * time.Second, MinRefs: 3}
}

func correlateOnce(t testing.TB, s *Store, now time.Time) IncidentReport {
	t.Helper()
	rep, err := s.CorrelateIncidents(context.Background(), incidentSettings(), now)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// Three distinct qualified references inside five minutes form one
// burst; two do not. The floor is the maximum member final.
func TestIncidentFormationThreshold(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:aa:1", base, "low")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:aa:2", base.Add(time.Minute), "medium")
	if rep := correlateOnce(t, s, base.Add(2*time.Minute)); rep.Created != 0 {
		t.Fatalf("two references must not form: %+v", rep)
	}
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:aa:3", base.Add(2*time.Minute), "high")
	rep := correlateOnce(t, s, base.Add(3*time.Minute))
	if rep.Created != 1 || rep.Appended != 0 {
		t.Fatalf("three references must form exactly one incident: %+v", rep)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d %v, want 1", len(list), err)
	}
	in := list[0]
	if in.Kind != IncidentKindAppearance || in.Status != IncidentOpen {
		t.Fatalf("unexpected incident shape: %+v", in)
	}
	if in.SourceScope != "lab" || in.Target != "lan-a" {
		t.Fatalf("scope must stay explicit: %+v", in)
	}
	if in.QualifiedRefs != 3 || in.MemberCount != 3 {
		t.Fatalf("counts = %d refs %d members, want 3/3: %+v", in.QualifiedRefs, in.MemberCount, in)
	}
	if in.Floor != "high" {
		t.Fatalf("floor = %q, want max member final high", in.Floor)
	}
	if in.IdentityRevision != "r1" {
		t.Fatalf("revision = %q, want r1", in.IdentityRevision)
	}
	detail, err := s.IncidentDetail(ctx, in.IncidentID)
	if err != nil || len(detail.Members) != 3 {
		t.Fatalf("detail members = %d %v, want 3", len(detail.Members), err)
	}
	for _, m := range detail.Members {
		if len(m.Refs) != 1 || m.Refs[0].Revision != "r1" || m.Refs[0].Namespace != "ns-1" {
			t.Fatalf("member snapshot must freeze evidence: %+v", m)
		}
	}
	// Members keep their own priorities: the incident floor never
	// rewrites briefing rows.
	for _, m := range detail.Members {
		b, err := s.BriefingByID(ctx, m.BriefingID)
		if err != nil {
			t.Fatal(err)
		}
		if b.Priority != m.Priority {
			t.Fatalf("member priority rewritten: %+v", b)
		}
	}
}

// Repeated reports from one reference never count as multiple
// devices, however many briefings they produce.
func TestIncidentSameReferenceCountsOnce(t *testing.T) {
	s := openTestStore(t)
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	// One ref observed three times in separate single-event groups:
	// three briefings, one distinct reference, all inside the window.
	for i := 0; i < 3; i++ {
		seedIncidentBriefingMaxEvents(t, s, "lab", "lan-a", "host.appeared",
			"mac:aa:1", base.Add(time.Duration(i)*30*time.Second), "medium", 1)
	}
	if rep := correlateOnce(t, s, base.Add(2*time.Minute)); rep.Created != 0 {
		t.Fatalf("one reference in three briefings must not form: %+v", rep)
	}
}

// Formation window boundaries are inclusive: a newest-anchored span
// of exactly five minutes forms; five minutes plus a beat does not.
func TestIncidentWindowBoundaries(t *testing.T) {
	s := openTestStore(t)
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:bb:1", base, "medium")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:bb:2", base.Add(time.Minute), "medium")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:bb:3", base.Add(5*time.Minute), "medium")
	if rep := correlateOnce(t, s, base.Add(6*time.Minute)); rep.Created != 1 {
		t.Fatalf("exact five-minute span must form: %+v", rep)
	}
	s2 := openTestStore(t)
	bindIncidentSource(t, s2, "lab")
	seedIncidentBriefing(t, s2, "lab", "lan-a", "host.appeared", "mac:cc:1", base, "medium")
	seedIncidentBriefing(t, s2, "lab", "lan-a", "host.appeared", "mac:cc:2", base.Add(time.Minute), "medium")
	seedIncidentBriefing(t, s2, "lab", "lan-a", "host.appeared", "mac:cc:3", base.Add(5*time.Minute+time.Second), "medium")
	if rep := correlateOnce(t, s2, base.Add(6*time.Minute)); rep.Created != 0 {
		t.Fatalf("span past five minutes must not form: %+v", rep)
	}
}

// Quiet closure is inclusive at exactly 120 seconds without a new
// matching member; the next burst forms a new incident. Appends
// inside the window extend the incident and move quiet time.
func TestIncidentQuietCloseAndAppend(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.disappeared", "mac:dd:1", base, "low")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.disappeared", "mac:dd:2", base.Add(time.Minute), "low")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.disappeared", "mac:dd:3", base.Add(2*time.Minute), "low")
	rep := correlateOnce(t, s, base.Add(3*time.Minute))
	if rep.Created != 1 {
		t.Fatalf("must form: %+v", rep)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	first := list[0]
	// Disappearance floors at medium even when every member is low.
	if first.Floor != "medium" {
		t.Fatalf("disappearance floor = %q, want medium", first.Floor)
	}
	// A fourth member inside the window appends and moves quiet time.
	// It must arrive before quiet expiry: the close check runs first
	// in the pass, so a straggler past 120s silence seeds the next
	// burst instead of joining a closed one.
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.disappeared", "mac:dd:4", base.Add(3*time.Minute), "high")
	rep = correlateOnce(t, s, base.Add(3*time.Minute+30*time.Second))
	if rep.Appended != 1 || rep.Created != 0 {
		t.Fatalf("fourth member must append: %+v", rep)
	}
	updated, err := s.GetIncident(ctx, first.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.MemberCount != 4 || updated.QualifiedRefs != 4 {
		t.Fatalf("counts after append = %d/%d, want 4/4: %+v", updated.QualifiedRefs, updated.MemberCount, updated)
	}
	if updated.Floor != "high" {
		t.Fatalf("floor after high member = %q, want high", updated.Floor)
	}
	if !updated.LastMemberAt.Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("quiet must track the newest accepted member: %+v", updated)
	}
	// Duplicate processing moves nothing: the same pass again
	// appends zero and leaves quiet time alone.
	rep = correlateOnce(t, s, base.Add(3*time.Minute+30*time.Second))
	if rep.Appended != 0 || rep.Created != 0 || rep.Closed != 0 {
		t.Fatalf("replay must be a no-op: %+v", rep)
	}
	again, err := s.GetIncident(ctx, first.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.LastMemberAt.Equal(updated.LastMemberAt) || !again.UpdatedAt.Equal(updated.UpdatedAt) {
		t.Fatalf("replay moved incident state: %+v", again)
	}
	// Exactly 120s after the last member the incident closes.
	rep = correlateOnce(t, s, base.Add(5*time.Minute))
	if rep.Closed != 1 {
		t.Fatalf("quiet expiry must close: %+v", rep)
	}
	closed, err := s.GetIncident(ctx, first.IncidentID)
	if err != nil || closed.Status != IncidentClosed || !closed.HasClosedAt {
		t.Fatalf("closed state missing: %+v %v", closed, err)
	}
	// A later burst with fresh references forms a new incident: a
	// close is terminal, never reopened.
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.disappeared", "mac:dd:5", base.Add(7*time.Minute), "low")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.disappeared", "mac:dd:6", base.Add(8*time.Minute), "low")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.disappeared", "mac:dd:7", base.Add(9*time.Minute), "low")
	rep = correlateOnce(t, s, base.Add(10*time.Minute))
	if rep.Created != 1 {
		t.Fatalf("post-close burst must form anew: %+v", rep)
	}
	list, err = s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d %v, want 2", len(list), err)
	}
	if list[1].IncidentID != first.IncidentID || list[0].IncidentID == first.IncidentID {
		t.Fatalf("second incident must be a new row above the closed one: %+v", list)
	}
	if list[0].Status != IncidentOpen || list[1].Status != IncidentClosed {
		t.Fatalf("newest must be open over closed: %+v", list)
	}
}

// Without the bound stable-identity contract every reference stays
// uncounted: reports remain individual, never incidents. Binding a
// namespace without the capability, or the capability under a
// different namespace, fails closed the same way.
func TestIncidentUnavailableWithoutIdentityContract(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	seed := func(t *testing.T, s *Store) {
		t.Helper()
		seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:ff:1", base, "medium")
		seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:ff:2", base.Add(time.Minute), "medium")
		seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:ff:3", base.Add(2*time.Minute), "medium")
	}
	// No contract at all.
	s := openTestStore(t)
	seed(t, s)
	if rep := correlateOnce(t, s, base.Add(3*time.Minute)); rep.Created != 0 {
		t.Fatalf("no contract must not form: %+v", rep)
	}
	// Namespace bound but capability missing.
	s2 := openTestStore(t)
	if err := s2.BindSourceIdentity(context.Background(), "lab", "ns-1", ""); err != nil {
		t.Fatal(err)
	}
	seed(t, s2)
	if rep := correlateOnce(t, s2, base.Add(3*time.Minute)); rep.Created != 0 {
		t.Fatalf("namespace without capability must not form: %+v", rep)
	}
	// Capability advertised but under a different namespace.
	s3 := openTestStore(t)
	if err := s3.BindSourceIdentity(context.Background(), "lab", "ns-other", StableIdentityCapability); err != nil {
		t.Fatal(err)
	}
	seed(t, s3)
	if rep := correlateOnce(t, s3, base.Add(3*time.Minute)); rep.Created != 0 {
		t.Fatalf("capability under another namespace must not form: %+v", rep)
	}
}

// Low and unknown confidence never qualify for device counts, and
// mixed revisions across the formation set fail closed. Each case
// seeds three briefings that differ only in the named property.
func TestIncidentQualificationFailures(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	seedWith := func(t *testing.T, s *Store, hostJSON string, ref, suffix string, at time.Time) {
		t.Helper()
		ctx := context.Background()
		eventID := fmt.Sprintf("evt-q-%s-%s", suffix, ref)
		payload := []byte(fmt.Sprintf(`{"event_id":%q,"event_key":%q,"event_type":"host.appeared","occurred_at":"2026-10-20T12:00:05Z","cycle":7,"target":"lan-a","host":%s}`,
			eventID, "k-"+suffix+ref, hostJSON))
		anchor, _ := GroupKeyForEvent("lab", "host.appeared", eventID, payload)
		res, err := s.PersistEventAndGroup(ctx, RawEvent{
			EventID: eventID, EventKey: "k-" + suffix + ref, EventType: "host.appeared",
			OccurredAt: at, Cycle: 1, Target: "lan-a", Payload: payload,
			ReceivedAt: at, SignatureTimestamp: at, SourceScope: "lab",
		}, anchor, GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour, GroupingVersion: GroupingScoped})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ProjectReported(ctx, "lab", 64); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CloseDue(ctx, at.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.TakeGroup(ctx, res.GroupID); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertBriefing(ctx, Briefing{
			BriefingID: "brf-" + res.GroupID, GroupID: res.GroupID, HostAnchorKey: anchor,
			Baseline: "low", Priority: "medium", PriorityReason: "test",
			WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		host func(ref string) string
	}{
		{"low confidence", func(ref string) string {
			return fmt.Sprintf(`{"host_id":%q,"identity_source":"mac","identity_namespace":"ns-1","identity_revision":"r1","identity_confidence":"low"}`, ref)
		}},
		{"unknown confidence", func(ref string) string {
			return fmt.Sprintf(`{"host_id":%q,"identity_source":"mac","identity_namespace":"ns-1","identity_revision":"r1"}`, ref)
		}},
		{"missing revision", func(ref string) string {
			return fmt.Sprintf(`{"host_id":%q,"identity_source":"mac","identity_namespace":"ns-1","identity_confidence":"high"}`, ref)
		}},
		{"unbound namespace", func(ref string) string {
			return fmt.Sprintf(`{"host_id":%q,"identity_source":"mac","identity_namespace":"ns-other","identity_revision":"r1","identity_confidence":"high"}`, ref)
		}},
		{"provided source", func(ref string) string {
			return fmt.Sprintf(`{"host_id":%q,"identity_source":"provided","identity_namespace":"ns-1","identity_revision":"r1","identity_confidence":"high"}`, ref)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			bindIncidentSource(t, s, "lab")
			for i, ref := range []string{"mac:qq:1", "mac:qq:2", "mac:qq:3"} {
				seedWith(t, s, tc.host(ref), ref, tc.name, base.Add(time.Duration(i)*time.Minute))
			}
			if rep := correlateOnce(t, s, base.Add(4*time.Minute)); rep.Created != 0 {
				t.Fatalf("%s must not form: %+v", tc.name, rep)
			}
		})
	}
	// Mixed revisions across members fail closed even when every
	// briefing is individually well formed.
	t.Run("mixed revisions", func(t *testing.T) {
		s := openTestStore(t)
		bindIncidentSource(t, s, "lab")
		rev := func(ref, revision string) string {
			return fmt.Sprintf(`{"host_id":%q,"identity_source":"mac","identity_namespace":"ns-1","identity_revision":%q,"identity_confidence":"high"}`, ref, revision)
		}
		seedWith(t, s, rev("mac:qr:1", "r1"), "mac:qr:1", "rev", base)
		seedWith(t, s, rev("mac:qr:2", "r1"), "mac:qr:2", "rev", base.Add(time.Minute))
		seedWith(t, s, rev("mac:qr:3", "r2"), "mac:qr:3", "rev", base.Add(2*time.Minute))
		if rep := correlateOnce(t, s, base.Add(4*time.Minute)); rep.Created != 0 {
			t.Fatalf("mixed revisions must not form: %+v", rep)
		}
	})
}

// Mixed appear/disappear event sets, lifecycle-free change reports,
// and silence all stay individual: the correlator never manufactures
// lifecycle direction.
func TestIncidentNoManufacturedLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	// One group carrying both an appearance and a disappearance.
	for i, typ := range []string{"host.appeared", "host.disappeared"} {
		eventID := fmt.Sprintf("evt-mixed-%d", i)
		payload := []byte(fmt.Sprintf(`{"event_id":%q,"event_key":"k-mixed","event_type":%q,"occurred_at":"2026-10-20T12:00:05Z","cycle":7,"target":"lan-a","host":%s}`,
			eventID, typ, incidentHostJSON("mac:mx:1", "high")))
		anchor, _ := GroupKeyForEvent("lab", typ, eventID, payload)
		if _, err := s.PersistEventAndGroup(ctx, RawEvent{
			EventID: eventID, EventKey: "k-mixed", EventType: typ,
			OccurredAt: base, Cycle: 1, Target: "lan-a", Payload: payload,
			ReceivedAt: base, SignatureTimestamp: base, SourceScope: "lab",
		}, anchor, GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour, GroupingVersion: GroupingScoped}); err != nil {
			t.Fatal(err)
		}
	}
	// Pure change reports carry no lifecycle direction.
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.changed", "mac:mx:2", base, "medium")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.changed", "mac:mx:3", base.Add(time.Minute), "medium")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.changed", "mac:mx:4", base.Add(2*time.Minute), "medium")
	if _, err := s.ProjectReported(ctx, "lab", 64); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(ctx, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Take every closed group and brief it, including the mixed one.
	for {
		needs, err := s.NeedsBriefing(ctx, 64, base.Add(2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(needs) == 0 {
			break
		}
		for _, gid := range needs {
			taken, err := s.TakeGroup(ctx, gid)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.InsertBriefing(ctx, Briefing{
				BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
				Baseline: "low", Priority: "medium", PriorityReason: "test",
				WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: base,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if rep := correlateOnce(t, s, base.Add(4*time.Minute)); rep.Created != 0 {
		t.Fatalf("mixed/change-only evidence must not form: %+v", rep)
	}
}

// Sources and targets never mix: identical bursts on different
// scopes form separate incidents, and different sources stay apart.
func TestIncidentScopeIsolation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	bindIncidentSource(t, s, "lab2")
	base := time.Now().UTC().Truncate(time.Second)
	for _, scope := range []string{"lab", "lab2"} {
		for i, ref := range []string{"mac:iso:1", "mac:iso:2", "mac:iso:3"} {
			seedIncidentBriefing(t, s, scope, "lan-a", "host.appeared", ref, base.Add(time.Duration(i)*time.Minute), "medium")
		}
	}
	for i, ref := range []string{"mac:iso:4", "mac:iso:5", "mac:iso:6"} {
		seedIncidentBriefing(t, s, "lab", "lan-b", "host.appeared", ref, base.Add(time.Duration(i)*time.Minute), "medium")
	}
	rep := correlateOnce(t, s, base.Add(4*time.Minute))
	if rep.Created != 3 {
		t.Fatalf("three isolated bursts must form three incidents: %+v", rep)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %d %v, want 3", len(list), err)
	}
	seen := map[string]bool{}
	for _, in := range list {
		seen[in.SourceScope+"\x00"+in.Target] = true
		if in.MemberCount != 3 || in.QualifiedRefs != 3 {
			t.Fatalf("isolated incident must hold exactly its burst: %+v", in)
		}
	}
	for _, k := range []string{"lab\x00lan-a", "lab2\x00lan-a", "lab\x00lan-b"} {
		if !seen[k] {
			t.Fatalf("missing isolated incident %q in %+v", k, list)
		}
	}
}

// Completions older than window+quiet at processing time are
// consumed without formation: a stale burst presents as its
// individual reports, and the cursor still advances past it.
func TestIncidentStaleCompletionsSkipFormation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:st:1", base, "medium")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:st:2", base.Add(time.Minute), "medium")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:st:3", base.Add(2*time.Minute), "medium")
	// A full day later the burst is stale: consumed, never formed.
	if rep := correlateOnce(t, s, base.Add(24*time.Hour)); rep.Created != 0 {
		t.Fatalf("stale burst must not form: %+v", rep)
	}
	cursor, err := s.incidentCursor(ctx)
	if err != nil || cursor == 0 {
		t.Fatalf("cursor must advance past stale completions: %d %v", cursor, err)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 0 {
		t.Fatalf("list = %d %v, want empty", len(list), err)
	}
}

// Downtime past the formation window must not duplicate or invent:
// one late pass over a 500-briefing backlog forms exactly one
// incident with exact membership — no phantom second incident from
// rows the same pass already consumed, and no briefing in two
// incidents.
func TestIncidentDowntimeFormsExactlyOnce(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 500; i++ {
		seedIncidentBriefingMaxEvents(t, s, "lab", "lan-a", "host.appeared",
			"mac:dt:1", base.Add(time.Duration(i)*time.Second), "low", 1)
	}
	for i, ref := range []string{"mac:dt:2", "mac:dt:3", "mac:dt:4"} {
		seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", ref, base.Add(time.Duration(501+i)*time.Second), "low")
	}
	// Far-future processing closes whatever forms inside the same
	// call: the close must not resurrect paged members as a second
	// incident.
	rep, err := s.CorrelateIncidents(ctx, incidentSettings(), base.Add(12*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 1 {
		t.Fatalf("downtime burst must form exactly once: %+v", rep)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d %v, want exactly one incident", len(list), err)
	}
	detail, err := s.IncidentDetail(ctx, list[0].IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Members) != list[0].MemberCount {
		t.Fatalf("stored members = %d, counted %d: phantom rows forbidden",
			len(detail.Members), list[0].MemberCount)
	}
	if list[0].QualifiedRefs != 4 {
		t.Fatalf("refs = %d, want 4", list[0].QualifiedRefs)
	}
	var dup int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM (
		SELECT briefing_id FROM incident_members GROUP BY briefing_id HAVING COUNT(*) > 1)`).Scan(&dup); err != nil {
		t.Fatal(err)
	}
	if dup != 0 {
		t.Fatalf("%d briefings in two incidents", dup)
	}
}

// Backlog past one page drains inside a bounded call: downtime
// accumulation never silently skips candidates, and the burst still
// forms from the reconsidered set.
func TestIncidentBacklogDrainsBounded(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	// 210 single-event briefings of one reference: past maxIncidentPage,
	// still one distinct reference.
	for i := 0; i < 210; i++ {
		seedIncidentBriefingMaxEvents(t, s, "lab", "lan-a", "host.appeared",
			"mac:bl:1", base.Add(time.Duration(i)*time.Second), "low", 1)
	}
	// Three more references complete the burst inside the same window.
	for i, ref := range []string{"mac:bl:2", "mac:bl:3", "mac:bl:4"} {
		seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", ref, base.Add(time.Duration(211+i)*time.Second), "low")
	}
	now := base.Add(5 * time.Minute)
	rep, err := s.CorrelateIncidents(ctx, incidentSettings(), now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created != 1 {
		t.Fatalf("backlog burst must form: %+v", rep)
	}
	cursor, err := s.incidentCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var maxRowID int64
	if err := s.db.QueryRow(`SELECT MAX(rowid) FROM briefings`).Scan(&maxRowID); err != nil {
		t.Fatal(err)
	}
	if cursor != maxRowID {
		t.Fatalf("cursor = %d, want %d: backlog must drain", cursor, maxRowID)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d %v, want 1", len(list), err)
	}
	// 210 briefings of one reference plus three singletons: four
	// distinct references, 213 members.
	if list[0].QualifiedRefs != 4 || list[0].MemberCount != 213 {
		t.Fatalf("backlog counts = %d refs %d members, want 4/213: %+v",
			list[0].QualifiedRefs, list[0].MemberCount, list[0])
	}
}

// Incident-only acknowledgement leaves member attention alone;
// include-members fans out to a durable snapshot atomically, retries
// replay, later joiners stay unacknowledged, and undo of the parent
// identifies (never rewrites) the generated rows.
func TestIncidentDecisions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	bids := make([]string, 0, 3)
	for i, ref := range []string{"mac:dc:1", "mac:dc:2", "mac:dc:3"} {
		bids = append(bids, seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", ref, base.Add(time.Duration(i)*time.Minute), "medium"))
	}
	if rep := correlateOnce(t, s, base.Add(3*time.Minute)); rep.Created != 1 {
		t.Fatalf("must form: %+v", rep)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	iid := list[0].IncidentID
	// Unknown incidents are 404, not silent success.
	if _, err := s.AppendIncidentDecision(ctx, DecisionInput{
		SubjectKind: DecisionSubjectIncident, SubjectID: "inc-nope",
		Kind: DecisionExpected, RequestID: "r-unknown",
	}, false); err == nil {
		t.Fatal("unknown incident must fail")
	}
	// Labels stay profile-only.
	if _, err := s.AppendIncidentDecision(ctx, DecisionInput{
		SubjectKind: DecisionSubjectIncident, SubjectID: iid,
		Kind: DecisionLabel, Text: "x", RequestID: "r-label",
	}, false); err == nil {
		t.Fatal("label on incident must fail")
	}
	// Member fan-out without a request ID is rejected: retries could
	// not replay safely.
	if _, err := s.AppendIncidentDecision(ctx, DecisionInput{
		SubjectKind: DecisionSubjectIncident, SubjectID: iid, Kind: DecisionExpected,
	}, true); err == nil {
		t.Fatal("member fan-out without request id must fail")
	}
	// Incident-only acknowledgement: no member rows.
	only, err := s.AppendIncidentDecision(ctx, DecisionInput{
		SubjectKind: DecisionSubjectIncident, SubjectID: iid,
		Kind: DecisionExpected, RequestID: "r-only",
	}, false)
	if err != nil || !only.Applied || len(only.Members) != 0 {
		t.Fatalf("incident-only decision = %+v %v", only, err)
	}
	for _, bid := range bids {
		trail, err := s.DecisionsForSubject(ctx, DecisionSubjectBriefing, bid, 50, 0, 0)
		if err != nil || len(trail) != 0 {
			t.Fatalf("incident-only ack must not touch members: %+v %v", trail, err)
		}
	}
	// Undo the incident-only row so the fan-out test starts clean.
	if _, err := s.UndoDecision(ctx, only.Parent.DecisionID, "r-undo-only"); err != nil {
		t.Fatal(err)
	}
	// Include-members fans out to the snapshot atomically.
	fan, err := s.AppendIncidentDecision(ctx, DecisionInput{
		SubjectKind: DecisionSubjectIncident, SubjectID: iid,
		Kind: DecisionExpected, RequestID: "r-fan",
	}, true)
	if err != nil || !fan.Applied || len(fan.Members) != 3 {
		t.Fatalf("fan-out = %+v %v", fan, err)
	}
	for i, m := range fan.Members {
		if m.SubjectKind != DecisionSubjectBriefing || m.SubjectID != bids[i] || m.Kind != DecisionExpected {
			t.Fatalf("generated member %d wrong: %+v", i, m)
		}
	}
	// Retry replays parent plus children without duplicating.
	again, err := s.AppendIncidentDecision(ctx, DecisionInput{
		SubjectKind: DecisionSubjectIncident, SubjectID: iid,
		Kind: DecisionExpected, RequestID: "r-fan",
	}, true)
	if err != nil || again.Applied || again.Parent.DecisionID != fan.Parent.DecisionID || len(again.Members) != 3 {
		t.Fatalf("retry must replay: %+v %v", again, err)
	}
	// A later joiner is not automatically acknowledged. It arrives
	// before quiet expiry so it still joins the open incident.
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:dc:4", base.Add(3*time.Minute), "medium")
	if rep := correlateOnce(t, s, base.Add(3*time.Minute+30*time.Second)); rep.Appended != 1 {
		t.Fatalf("joiner must append: %+v", rep)
	}
	detail, err := s.IncidentDetail(ctx, iid)
	if err != nil || len(detail.Members) != 4 {
		t.Fatalf("members = %d %v", len(detail.Members), err)
	}
	trail, err := s.DecisionsForSubject(ctx, DecisionSubjectBriefing, detail.Members[3].BriefingID, 50, 0, 0)
	if err != nil || len(trail) != 0 {
		t.Fatalf("late joiner must stay unacknowledged: %+v %v", trail, err)
	}
	// Undo of the parent identifies the generated rows for
	// individual revisit; the members stay acknowledged.
	gen, err := s.GeneratedDecisions(ctx, fan.Parent.DecisionID)
	if err != nil || len(gen) != 3 {
		t.Fatalf("generated = %v %v, want 3", gen, err)
	}
	if _, err := s.UndoDecision(ctx, fan.Parent.DecisionID, "r-undo-fan"); err != nil {
		t.Fatal(err)
	}
	for _, m := range fan.Members {
		var undone int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM operator_decisions
			WHERE kind = 'undo' AND supersedes = ?`, m.DecisionID).Scan(&undone); err != nil {
			t.Fatal(err)
		}
		if undone != 0 {
			t.Fatalf("member %s must stay live after parent undo", m.DecisionID)
		}
	}
	// Effective review sees the incident head while members keep
	// theirs: incident-only state never leaks across scopes.
	states, err := s.EffectiveReview(ctx, DecisionSubjectIncident, []string{iid}, base.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := states[iid]; ok {
		t.Fatalf("undone parent must leave no incident head: %+v", states)
	}
}

// Open incidents pin their members past briefing retention; closed
// ones pin through the 90d horizon; afterwards members drain before
// parents with exact ledger counts and no dangling member IDs.
func TestIncidentRetentionPinsAndDrains(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	bids := make([]string, 0, 3)
	for i, ref := range []string{"mac:rt:1", "mac:rt:2", "mac:rt:3"} {
		bids = append(bids, seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", ref, base.Add(time.Duration(i)*time.Minute), "medium"))
	}
	if rep := correlateOnce(t, s, base.Add(3*time.Minute)); rep.Created != 1 {
		t.Fatalf("must form: %+v", rep)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	iid := list[0].IncidentID
	settings := PurgeSettings{
		BriefingRetention: time.Hour, SubjectRetention: 90 * 24 * time.Hour,
		DecisionRetention: 180 * 24 * time.Hour, RuleRetention: 180 * 24 * time.Hour,
		HeartbeatRetention: 7 * 24 * time.Hour,
	}
	// Two hours later the briefings are age-eligible, but the open
	// incident pins members, links and traces.
	rep, err := s.PurgeOnce(ctx, settings, base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Briefings != 0 {
		t.Fatalf("open incident must pin members: %+v", rep)
	}
	for _, bid := range bids {
		if _, err := s.BriefingByID(ctx, bid); err != nil {
			t.Fatalf("pinned member %s must survive: %v", bid, err)
		}
	}
	// Close the incident; while retained, the pin holds.
	if rep := correlateOnce(t, s, base.Add(4*time.Minute)); rep.Closed != 1 {
		t.Fatalf("must close: %+v", rep)
	}
	rep, err = s.PurgeOnce(ctx, settings, base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Briefings != 0 {
		t.Fatalf("retained closed incident must pin members: %+v", rep)
	}
	// Past incident retention the membership drains first, then the
	// parent, and the unpinned member briefings age out in the same
	// run — with exact per-table counts throughout.
	settings.IncidentRetention = time.Minute
	rep, err = s.PurgeOnce(ctx, settings, base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.IncidentMembers != 3 || rep.Incidents != 1 || rep.Briefings != 3 {
		t.Fatalf("drain = %+v, want 3 members + 1 incident + 3 briefings", rep)
	}
	members, err := s.IncidentMemberBriefings(ctx, []string{iid})
	if err != nil || len(members) != 0 {
		t.Fatalf("no dangling member IDs may remain: %+v %v", members, err)
	}
	if _, err := s.GetIncident(ctx, iid); err == nil {
		t.Fatal("drained incident must read unknown")
	}
	for _, bid := range bids {
		if _, err := s.BriefingByID(ctx, bid); err == nil {
			t.Fatalf("drained member %s must read unknown", bid)
		}
	}
	// The health ledger attributes the drain per table.
	last, err := s.LastPurgeRun(ctx)
	if err != nil || !last.HaveRun {
		t.Fatal(err)
	}
	if last.ByTable["incident_members"] != 3 || last.ByTable["incidents"] != 1 || last.ByTable["briefings"] != 3 {
		t.Fatalf("ledger must attribute incident drains: %+v", last.ByTable)
	}
}

// Rewinding to v25 and replaying preserves decision audit, rebuilds
// the decisions table with the incident subject kind, and leaves the
// correlator fully functional on the migrated database.
func TestIncidentMigrationV25ToV26(t *testing.T) {
	path := t.TempDir() + "/bridge.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	gid := seedFeedGroup(t, s, "host:mig", "evt-mig")
	takeFeedGroup(t, s, gid)
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: "brf-" + gid, GroupID: gid, HostAnchorKey: "host:mig",
		Baseline: "low", Priority: "low", PriorityReason: "r",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	dec, err := s.AppendDecision(ctx, DecisionInput{
		SubjectKind: DecisionSubjectBriefing, SubjectID: "brf-" + gid,
		Kind: DecisionNote, Text: "kept", RequestID: "r-mig",
	})
	if err != nil || !dec.Applied {
		t.Fatalf("seed decision = %+v %v", dec, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	dropIncidentSchema(t, raw)
	if _, err := raw.Exec(`PRAGMA user_version = 25`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != supportedSchemaVersion {
		t.Fatalf("user_version = %d %v, want %d", version, err, supportedSchemaVersion)
	}
	trail, err := s.DecisionsForSubject(ctx, DecisionSubjectBriefing, "brf-"+gid, 50, 0, 0)
	if err != nil || len(trail) != 1 || trail[0].Text != "kept" {
		t.Fatalf("audit must survive the rebuild: %+v %v", trail, err)
	}
	// The migrated database forms incidents and takes incident
	// decisions end to end. The post-migration completions land an
	// hour later: group IDs embed receipt nanos plus a process-local
	// sequence that restarts at reopen, so reusing the seed second
	// would collide with the pre-migration group.
	bindIncidentSource(t, s, "lab")
	base := now.Add(time.Hour).Truncate(time.Second)
	for i, ref := range []string{"mac:mg:1", "mac:mg:2", "mac:mg:3"} {
		seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", ref, base.Add(time.Duration(i)*time.Minute), "medium")
	}
	if rep := correlateOnce(t, s, base.Add(3*time.Minute)); rep.Created != 1 {
		t.Fatalf("must form after migration: %+v", rep)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	res, err := s.AppendIncidentDecision(ctx, DecisionInput{
		SubjectKind: DecisionSubjectIncident, SubjectID: list[0].IncidentID,
		Kind: DecisionNote, Text: "burst reviewed", RequestID: "r-mig-inc",
	}, false)
	if err != nil || !res.Applied {
		t.Fatalf("incident decision after migration = %+v %v", res, err)
	}
}

// Overview reads cover open and recently closed incidents with
// decision awareness: acknowledged rows leave, flagged rows mark.
func TestIncidentOverviewReads(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	for i, ref := range []string{"mac:ov:1", "mac:ov:2", "mac:ov:3"} {
		seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", ref, base.Add(time.Duration(i)*time.Minute), "high")
	}
	if rep := correlateOnce(t, s, base.Add(3*time.Minute)); rep.Created != 1 {
		t.Fatalf("must form: %+v", rep)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	iid := list[0].IncidentID
	since := base.Add(-time.Hour)
	cands, err := s.IncidentCandidates(ctx, since, 50)
	if err != nil || len(cands) != 1 || cands[0].IncidentID != iid || cands[0].Flagged {
		t.Fatalf("candidates = %+v %v", cands, err)
	}
	counts, flagged, err := s.IncidentEffectiveCounts(ctx, since, base.Add(3*time.Minute))
	if err != nil || counts["high"] != 1 || flagged != 0 {
		t.Fatalf("counts = %v flagged %d %v", counts, flagged, err)
	}
	members, err := s.IncidentMemberBriefings(ctx, []string{iid})
	if err != nil || len(members[iid]) != 3 {
		t.Fatalf("members = %+v %v", members, err)
	}
	// Acknowledgement removes the incident from attention and counts.
	if _, err := s.AppendIncidentDecision(ctx, DecisionInput{
		SubjectKind: DecisionSubjectIncident, SubjectID: iid,
		Kind: DecisionExpected, RequestID: "r-ov-ack",
	}, false); err != nil {
		t.Fatal(err)
	}
	cands, err = s.IncidentCandidates(ctx, since, 50)
	if err != nil || len(cands) != 0 {
		t.Fatalf("acked incident must leave attention: %+v %v", cands, err)
	}
	counts, flagged, err = s.IncidentEffectiveCounts(ctx, since, base.Add(4*time.Minute))
	if err != nil || len(counts) != 0 || flagged != 0 {
		t.Fatalf("acked incident must not vote: %v flagged %d %v", counts, flagged, err)
	}
	// A flag restores visibility and floors review without escalating.
	if _, err := s.AppendIncidentDecision(ctx, DecisionInput{
		SubjectKind: DecisionSubjectIncident, SubjectID: iid,
		Kind: DecisionNotExpected, RequestID: "r-ov-flag",
	}, false); err != nil {
		t.Fatal(err)
	}
	cands, err = s.IncidentCandidates(ctx, since, 50)
	if err != nil || len(cands) != 1 || !cands[0].Flagged {
		t.Fatalf("flagged incident must return marked: %+v %v", cands, err)
	}
	counts, flagged, err = s.IncidentEffectiveCounts(ctx, since, base.Add(4*time.Minute))
	if err != nil || len(counts) != 0 || flagged != 1 {
		t.Fatalf("flagged incident must floor review only: %v flagged %d %v", counts, flagged, err)
	}
}

// The correlator and overview incident reads must ride indexes,
// never full scans: they run every tick and refresh against
// retention-sized tables.
func TestIncidentReadsUseIndexes(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second).UnixNano()
	queries := []struct {
		name  string
		query string
		args  []any
		want  string
	}{
		{"candidates", `EXPLAIN QUERY PLAN WITH ` + incidentReviewCTE + `, ` + incidentEligibleCTE + ` SELECT e2.incident_id FROM eligible e2
			LEFT JOIN ieff e ON e.subject_id = e2.incident_id
			WHERE e.kind IS NULL OR e.kind = 'not_expected' LIMIT 100`,
			[]any{now, now - int64(180*24*time.Hour), 0}, "incidents_status_closed"},
		{"open lookup", `EXPLAIN QUERY PLAN SELECT incident_id FROM incidents
			WHERE kind = 'burst_appearance' AND source_scope = 'lab' AND target = 'lan-a' AND status = 'open'`,
			nil, "incidents_open_key"},
		{"fresh set", `EXPLAIN QUERY PLAN SELECT b.rowid FROM briefings b
			LEFT JOIN incident_members m ON m.briefing_id = b.briefing_id
			WHERE b.created_at >= ? AND m.briefing_id IS NULL LIMIT 500`,
			[]any{now}, "briefings_created"},
		// Group scopes ride the membership uniqueness autoindex
		// (group_id, event_id): no separate index, no extra ingest
		// write cost.
		{"group scopes", `EXPLAIN QUERY PLAN SELECT ge.group_id FROM group_events ge
			JOIN raw_events e ON e.event_id = ge.event_id WHERE ge.group_id IN ('g1','g2')`,
			nil, "group_id=?"},
		{"generated", `EXPLAIN QUERY PLAN SELECT decision_id FROM operator_decisions
   WHERE generated_by = 'dec-1' AND rowid > 100 ORDER BY rowid ASC LIMIT 201`,
			nil, "decisions_generated"},
		{"alternative window", `EXPLAIN QUERY PLAN SELECT briefing_id,created_at FROM incident_pending_refs
   WHERE correlation_key='key' AND created_at>=0 AND created_at<=1000 ORDER BY created_at DESC LIMIT 3`,
			nil, "incident_pending_refs_time"},
		{"search checkpoint", `EXPLAIN QUERY PLAN SELECT briefing_id,formation_before FROM incident_pending
   WHERE correlation_key='key' ORDER BY created_at,briefing_id LIMIT 1`, nil, "incident_pending_key"},
	}
	for _, q := range queries {
		t.Run(q.name, func(t *testing.T) {
			rows, err := s.db.Query(q.query, q.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			indexed := false
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					t.Fatal(err)
				}
				// Plan lines name tables by alias (SCAN i) or
				// directly (SCAN incidents); materialized CTE scans
				// (SCAN (subquery)...) are not table scans.
				trimmed := strings.TrimSpace(detail)
				if trimmed == "SCAN i" || strings.HasPrefix(trimmed, "SCAN i ") ||
					trimmed == "SCAN incidents" || strings.HasPrefix(trimmed, "SCAN incidents ") ||
					strings.Contains(detail, "SCAN briefings") ||
					strings.Contains(detail, "SCAN group_events") ||
					strings.Contains(detail, "SCAN operator_decisions") {
					t.Fatalf("incident read scans: %s", detail)
				}
				if strings.Contains(detail, q.want) {
					indexed = true
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !indexed {
				t.Fatalf("incident read must use %s", q.want)
			}
		})
	}
}

// A cancelled context fails the pass loudly with no partial cursor
// movement: cancellation is "unknown, retry safely", never silent.
func TestIncidentCancelledContext(t *testing.T) {
	s := openTestStore(t)
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:cx:1", base, "medium")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.CorrelateIncidents(ctx, incidentSettings(), base); err == nil {
		t.Fatal("cancelled pass must fail")
	}
	cursor, err := s.incidentCursor(context.Background())
	if err != nil || cursor != 0 {
		t.Fatalf("cursor must not move on cancellation: %d %v", cursor, err)
	}
}

// Restart neither duplicates membership nor loses the cursor: a
// reopened store replays the pass as a no-op and still appends later
// members exactly once.
func TestIncidentRestartDurability(t *testing.T) {
	path := t.TempDir() + "/bridge.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	bindIncidentSource(t, s, "lab")
	base := time.Now().UTC().Truncate(time.Second)
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:ee:1", base, "medium")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:ee:2", base.Add(time.Minute), "medium")
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:ee:3", base.Add(2*time.Minute), "medium")
	if rep := correlateOnce(t, s, base.Add(3*time.Minute)); rep.Created != 1 {
		t.Fatalf("must form: %+v", rep)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if rep := correlateOnce(t, s, base.Add(3*time.Minute)); rep.Created != 0 || rep.Appended != 0 || rep.Closed != 0 || len(rep.Changed) != 0 {
		t.Fatalf("replay after restart must be a no-op: %+v", rep)
	}
	seedIncidentBriefing(t, s, "lab", "lan-a", "host.appeared", "mac:ee:4", base.Add(3*time.Minute), "medium")
	if rep := correlateOnce(t, s, base.Add(3*time.Minute+30*time.Second)); rep.Appended != 1 {
		t.Fatalf("post-restart member must append once: %+v", rep)
	}
	list, err := s.ListIncidents(ctx, 50, 0, 0)
	if err != nil || len(list) != 1 || list[0].MemberCount != 4 {
		t.Fatalf("members = %+v %v, want 4 on one incident", list, err)
	}
}
