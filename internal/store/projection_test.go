package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dropProjectionSchema rewinds the Step-18 schema additions (v10 tables,
// v11 pending queue and receipt scope column) so migration-chain tests
// that rewind to older versions replay cleanly through the current
// version on reopen. Children drop before parents.
func dropProjectionSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS reported_ports`,
		`DROP TABLE IF EXISTS reported_observations`,
		`DROP TABLE IF EXISTS group_subjects`,
		`DROP TABLE IF EXISTS briefing_subjects`,
		`DROP TABLE IF EXISTS projection_progress`,
		`DROP TABLE IF EXISTS projection_pending`,
		`DROP TABLE IF EXISTS reported_subjects`,
		`DROP TABLE IF EXISTS report_sources`,
		`DROP TABLE IF EXISTS legacy_profile_state`,
		`ALTER TABLE raw_events DROP COLUMN source_scope`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// persistReported persists one accepted event the way the server does:
// grouping key derived from the scoped upstream reference, then
// PersistEventAndGroup. Returns the joined group ID.
func persistReported(t *testing.T, s *Store, sourceScope, eventID, eventKey, eventType string, payload []byte, at time.Time) string {
	t.Helper()
	anchor, _ := GroupKeyForEvent(sourceScope, eventType, eventID, payload)
	res, err := s.PersistEventAndGroup(context.Background(), RawEvent{
		EventID: eventID, EventKey: eventKey, EventType: eventType,
		OccurredAt: at, Cycle: 1, Target: "192.168.0.0/24", Payload: payload,
		ReceivedAt: at, SignatureTimestamp: at,
	}, anchor, GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return res.GroupID
}

func producerPayload(eventID, eventKey, eventType, hostJSON string) []byte {
	return []byte(fmt.Sprintf(`{"event_id":%q,"event_key":%q,"event_type":%q,"occurred_at":"2026-10-20T12:00:05Z","cycle":7,"target":"192.168.0.0/24","host":%s}`,
		eventID, eventKey, eventType, hostJSON))
}

// A closed group with unprojected events is not briefable: TakeGroup
// reports ErrGroupNotReady and NeedsBriefing skips it. Projecting clears
// both, mirroring the worker tick order.
func TestUnprojectedGroupNotReady(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	payload := producerPayload("evt-nr", "k", "host.appeared",
		`{"host_id":"h-nr","identity_source":"host-id","identity_confidence":"high"}`)
	gid := persistReported(t, s, "lab", "evt-nr", "k", "host.appeared", payload, now)
	if _, err := s.CloseDue(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeGroup(ctx, gid); !errors.Is(err, ErrGroupNotReady) {
		t.Fatalf("take before projection = %v, want ErrGroupNotReady", err)
	}
	needs, err := s.NeedsBriefing(ctx, 64, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range needs {
		if id == gid {
			t.Fatalf("unprojected group %q must not need briefing", gid)
		}
	}
	if _, err := s.ProjectReported(ctx, "lab", 64); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil || taken.GroupID != gid {
		t.Fatalf("take after projection = %+v %v", taken, err)
	}
}

func TestGroupKeyDerivation(t *testing.T) {
	qualifiedMAC := `{"mac":"10:20:30:40:50:60","host_id":"mac:10:20:30:40:50:60","identity_source":"mac","identity_confidence":"high"}`
	longID := strings.Repeat("h", maxReferenceRunes+1)
	for name, tc := range map[string]struct {
		typ     string
		body    []byte
		want    string
		evental bool
	}{
		"qualified mac reference": {
			"host.appeared", producerPayload("e1", "k", "host.appeared", qualifiedMAC),
			"sub:lab::mac:10:20:30:40:50:60", false,
		},
		"changed prefers current": {
			"host.changed", []byte(`{"event_id":"e","current":` + qualifiedMAC + `,"previous":{"ip":"192.168.0.9","host_id":"ip:192.168.0.9","identity_source":"ip","identity_confidence":"low"}}`),
			"sub:lab::mac:10:20:30:40:50:60", false,
		},
		"changed falls back to previous": {
			"host.changed", []byte(`{"event_id":"e","previous":{"mac":"11:22:33:44:55:66","host_id":"mac:11:22:33:44:55:66","identity_source":"mac","identity_confidence":"high"}}`),
			"sub:lab::mac:11:22:33:44:55:66", false,
		},
		"ip-derived stays address-scoped": {
			"host.appeared", producerPayload("e1", "k", "host.appeared", `{"ip":"192.168.0.23","host_id":"ip:192.168.0.23","identity_source":"ip","identity_confidence":"low"}`),
			"addr:lab:192.168.0.23", false,
		},
		// A bare IP attribute is not a reported reference: without
		// an ip-derived host_id it stays event-local and never
		// associates across events.
		"bare ip without a reported reference stays event-local": {
			"host.disappeared", producerPayload("e1", "k", "host.disappeared", `{"ip":"192.168.0.23"}`),
			"evt:e1", true,
		},
		"oversized mac reference with an ip stays event-local": {
			"host.appeared", producerPayload("e1", "k", "host.appeared", `{"ip":"192.168.0.23","host_id":"`+strings.Repeat("m", 300)+`","identity_source":"mac","identity_confidence":"high"}`),
			"evt:e1", true,
		},
		"contradictory ip reference and attribute stay event-local": {
			"host.appeared", producerPayload("e1", "k", "host.appeared", `{"ip":"192.168.0.99","host_id":"ip:192.168.0.23","identity_source":"ip","identity_confidence":"low"}`),
			"evt:e1", true,
		},
		"zero-padded ip is not an address": {
			"host.appeared", producerPayload("e1", "k", "host.appeared", `{"ip":"192.168.000.005"}`),
			"evt:e1", true,
		},
		"provided without contract stays event-local": {
			"host.appeared", producerPayload("e1", "k", "host.appeared", `{"host_id":"asset-42","identity_source":"provided","identity_confidence":"high"}`),
			"evt:e1", true,
		},
		"unknown source with host_id stays event-local": {
			"host.appeared", producerPayload("e1", "k", "host.appeared", `{"host_id":"mystery-1"}`),
			"evt:e1", true,
		},
		"over-bound host_id falls back to event-local": {
			"host.appeared", producerPayload("e1", "k", "host.appeared", `{"host_id":"`+longID+`","identity_source":"mac","identity_confidence":"high"}`),
			"evt:e1", true,
		},
		"missing host object groups by event id": {
			"host.appeared", []byte(`{"event_id":"e1"}`),
			"evt:e1", true,
		},
		"undecodable body groups by event id": {
			"host.appeared", []byte("not json{{{"),
			"evt:e1", true,
		},
	} {
		got, evental := GroupKeyForEvent("lab", tc.typ, "e1", tc.body)
		if got != tc.want || evental != tc.evental {
			t.Errorf("%s: got (%q,%v), want (%q,%v)", name, got, evental, tc.want, tc.evental)
		}
	}
}

func subjectProfileID(t *testing.T, s *Store, scope, class, namespace, ref string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT profile_id FROM reported_subjects
		WHERE source_scope = ? AND reference_class = ? AND identity_namespace = ? AND upstream_reference = ?`,
		scope, class, namespace, ref).Scan(&id); err != nil {
		t.Fatalf("subject %s/%s missing: %v", class, ref, err)
	}
	return id
}

func subjectCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reported_subjects`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Same qualified reference re-observed resolves to one subject across
// restarts, observations accumulate, and re-projection changes nothing.
func TestProjectReportedSameReferenceOneSubject(t *testing.T) {
	path := t.TempDir() + "/bridge.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	mac := `{"mac":"10:20:30:40:50:60","host_id":"mac:10:20:30:40:50:60","identity_source":"mac","identity_confidence":"high","vendor":"Synthetic","ports":[{"number":22,"protocol":"tcp","state":"open"}]}`
	persistReported(t, s, "lab", "evt-a1", "k1", "host.appeared", producerPayload("evt-a1", "k1", "host.appeared", mac), at)
	persistReported(t, s, "lab", "evt-a2", "k2", "host.appeared", producerPayload("evt-a2", "k2", "host.appeared", mac), at.Add(time.Minute))
	if n, err := s.ProjectReported(context.Background(), "lab", 64); err != nil || n != 2 {
		t.Fatalf("projected = %d, %v; want 2", n, err)
	}
	prof := subjectProfileID(t, s, "lab", SubjectReportedDevice, "", "mac:10:20:30:40:50:60")
	var obs, ports, links int
	s.db.QueryRow(`SELECT COUNT(*) FROM reported_observations WHERE profile_id = ?`, prof).Scan(&obs)
	s.db.QueryRow(`SELECT COUNT(*) FROM reported_ports`).Scan(&ports)
	s.db.QueryRow(`SELECT COUNT(*) FROM group_subjects WHERE profile_id = ?`, prof).Scan(&links)
	if obs != 2 || ports != 2 || links != 1 {
		t.Fatalf("observations=%d ports=%d group links=%d; want 2/2/1 (one shared group)", obs, ports, links)
	}
	// Idempotent: re-running projects nothing new.
	if n, err := s.ProjectReported(context.Background(), "lab", 64); err != nil || n != 0 {
		t.Fatalf("re-projected = %d, %v; want 0", n, err)
	}
	if got := subjectCount(t, s); got != 1 {
		t.Fatalf("subjects = %d, want 1", got)
	}
	// Restart: the same reference resolves to the same profile, and the
	// progress table resumes instead of duplicating.
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	persistReported(t, s2, "lab", "evt-a3", "k3", "host.appeared", producerPayload("evt-a3", "k3", "host.appeared", mac), at.Add(2*time.Hour))
	if n, err := s2.ProjectReported(context.Background(), "lab", 64); err != nil || n != 1 {
		t.Fatalf("after restart projected = %d, %v; want 1", n, err)
	}
	if got := subjectProfileID(t, s2, "lab", SubjectReportedDevice, "", "mac:10:20:30:40:50:60"); got != prof {
		t.Fatalf("restart changed profile id: %q vs %q", got, prof)
	}
	var last int64
	s2.db.QueryRow(`SELECT last_report_at FROM reported_subjects WHERE profile_id = ?`, prof).Scan(&last)
	if last != at.Add(2*time.Hour).UnixNano() {
		t.Fatalf("last_report_at did not advance to the retained re-observation")
	}
}

// Previous and current views with different IDs stay distinct subjects
// linked to the same group with their own roles; nothing unions them.
func TestProjectPreviousCurrentStayDistinct(t *testing.T) {
	s := openTestStore(t)
	at := time.Now().UTC().Truncate(time.Second)
	body := []byte(`{"event_id":"evt-x","event_key":"k","event_type":"host.changed","occurred_at":"2026-10-20T12:00:05Z","cycle":3,
		"previous":{"ip":"192.168.0.9","host_id":"ip:192.168.0.9","identity_source":"ip","identity_confidence":"low"},
		"current":{"mac":"10:20:30:40:50:61","host_id":"mac:10:20:30:40:50:61","identity_source":"mac","identity_confidence":"high","hostname":"lab-printer"}}`)
	gid := persistReported(t, s, "lab", "evt-x", "k", "host.changed", body, at)
	if n, err := s.ProjectReported(context.Background(), "lab", 64); err != nil || n != 1 {
		t.Fatalf("projected = %d, %v; want 1", n, err)
	}
	dev := subjectProfileID(t, s, "lab", SubjectReportedDevice, "", "mac:10:20:30:40:50:61")
	addr := subjectProfileID(t, s, "lab", SubjectAddress, "", "192.168.0.9")
	if dev == addr {
		t.Fatal("previous and current must not share a profile")
	}
	rows, err := s.db.Query(`SELECT profile_id, role FROM group_subjects WHERE group_id = ? ORDER BY role`, gid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var p, r string
		if err := rows.Scan(&p, &r); err != nil {
			t.Fatal(err)
		}
		got[r] = p
	}
	if got["current"] != dev || got["previous"] != addr {
		t.Fatalf("group links = %+v; want current=%s previous=%s", got, dev, addr)
	}
}

// Briefing inserts copy the group's subject relations; groups whose
// subjects were never projected store no invented relations.
func TestBriefingCopiesSubjects(t *testing.T) {
	s := openTestStore(t)
	at := time.Now().UTC().Truncate(time.Second)
	mac := `{"mac":"10:20:30:40:50:60","host_id":"mac:10:20:30:40:50:60","identity_source":"mac","identity_confidence":"high"}`
	gid := persistReported(t, s, "lab", "evt-a1", "k1", "host.appeared", producerPayload("evt-a1", "k1", "host.appeared", mac), at)
	if _, err := s.ProjectReported(context.Background(), "lab", 64); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseDue(context.Background(), at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(context.Background(), Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: "r",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	var groupLinks, briefingLinks int
	s.db.QueryRow(`SELECT COUNT(*) FROM group_subjects WHERE group_id = ?`, gid).Scan(&groupLinks)
	s.db.QueryRow(`SELECT COUNT(*) FROM briefing_subjects WHERE briefing_id = ?`, "brf-"+gid).Scan(&briefingLinks)
	if groupLinks == 0 || briefingLinks != groupLinks {
		t.Fatalf("group links=%d briefing links=%d; briefing must copy the group", groupLinks, briefingLinks)
	}
	rows, err := s.db.Query(`SELECT b.profile_id, b.role FROM briefing_subjects b JOIN group_subjects g
		ON g.group_id = ? AND g.profile_id = b.profile_id AND g.role = b.role WHERE b.briefing_id = ?`, gid, "brf-"+gid)
	if err != nil {
		t.Fatal(err)
	}
	matched := 0
	for rows.Next() {
		matched++
		var p, r string
		rows.Scan(&p, &r)
	}
	rows.Close()
	if matched != groupLinks {
		t.Fatalf("matched relations = %d, want %d", matched, groupLinks)
	}
}

// Hot joins stay indexed: the clustered reads behind projection and
// the briefing-subject copy must not degrade into table scans.
func TestProjectionQueriesUseIndexes(t *testing.T) {
	s := openTestStore(t)
	at := time.Now().UTC().Truncate(time.Second)
	mac := `{"mac":"10:20:30:40:50:60","host_id":"mac:10:20:30:40:50:60","identity_source":"mac","identity_confidence":"high"}`
	persistReported(t, s, "lab", "evt-a1", "k1", "host.appeared", producerPayload("evt-a1", "k1", "host.appeared", mac), at)
	if _, err := s.ProjectReported(context.Background(), "lab", 64); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`EXPLAIN QUERY PLAN SELECT profile_id FROM reported_subjects
			WHERE source_scope = 'lab' AND reference_class = 'reported_device' AND identity_namespace = '' AND upstream_reference = 'mac:10:20:30:40:50:60'`,
		`EXPLAIN QUERY PLAN SELECT profile_id, role FROM group_subjects WHERE group_id = 'grp-x'`,
		`EXPLAIN QUERY PLAN SELECT profile_id, role FROM group_subjects WHERE group_id = 'grp-x' AND profile_id = 'prof-y'`,
	} {
		rows, err := s.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		used := false
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(detail, "USING") {
				used = true
			}
		}
		rows.Close()
		if !used {
			t.Fatalf("hot join falls back to a scan: %s", q)
		}
	}
}

// Fresh installs land on schema v10 with the projection tables ready;
// supported version and table set are asserted once, here.
func TestMigrateV10ProjectionTables(t *testing.T) {
	s := openTestStore(t)
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 11 {
		t.Fatalf("user_version = %d, %v; want 11", version, err)
	}
	var tables int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN
		('report_sources','reported_subjects','reported_observations','reported_ports','group_subjects','briefing_subjects','projection_progress','legacy_profile_state','projection_pending')`).Scan(&tables); err != nil || tables != 9 {
		t.Fatalf("projection tables = %d, %v; want 9", tables, err)
	}
}

// BenchmarkProjectReported drains fresh qualified events through the
// projector: the per-tick cost the briefing worker absorbs.
func BenchmarkProjectReported(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	mac := `{"mac":"10:20:30:40:50:60","host_id":"mac:10:20:30:40:50:60","identity_source":"mac","identity_confidence":"high","vendor":"Synthetic"}`
	ids := 0
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		for j := 0; j < 20; j++ {
			id := fmt.Sprintf("evt-bench-%d", ids)
			ids++
			payload := producerPayload(id, "k", "host.appeared", mac)
			anchor, _ := GroupKeyForEvent("lab", "host.appeared", id, payload)
			if _, err := s.PersistEventAndGroup(ctx, RawEvent{
				EventID: id, EventKey: "k", EventType: "host.appeared",
				OccurredAt: at, Payload: payload, ReceivedAt: at, SignatureTimestamp: at,
			}, anchor, GroupPolicy{QuietWindow: time.Hour, MaxWindow: 2 * time.Hour}); err != nil {
				b.Fatal(err)
			}
		}
		b.StartTimer()
		if _, err := s.ProjectReported(ctx, "lab", 64); err != nil {
			b.Fatal(err)
		}
	}
}
