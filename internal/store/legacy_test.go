package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Step 18d cutover guards: the current tree never implemented the old
// continuity engine, so there is no inference to disable — these tests
// pin that down. Legacy profile rows are never written, read, or
// repurposed by any runtime path, and databases newer than the
// supported schema are refused unchanged rather than opened.

// A newer version must fail before migration dispatch and preserve its
// user_version. This does not assert byte identity of SQLite's files.
func TestOpenRefusesNewerVersion(t *testing.T) {
	for _, version := range []int{supportedSchemaVersion + 1, 99} {
		path := filepath.Join(t.TempDir(), "bridge.db")
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
			db.Close()
			t.Fatal(err)
		}
		db.Close()
		if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than supported") {
			t.Fatalf("version %d must be refused, got %v", version, err)
		}
		db, err = sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var kept int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&kept); err != nil || kept != version {
			t.Fatalf("refused database must keep version %d, got %d %v", version, kept, err)
		}
	}
}

// A legacy row remains unchanged through recovery, projection, briefing
// and purge. A real reported subject and its briefing relations exist
// before checking that legacy labels and IDs were not transferred.
func TestLegacyProfileStateQuarantined(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const oldID, oldLabel, oldSplit = "dev-legacy-1", "operator name", "split:1"
	if _, err := s.db.Exec(`INSERT INTO legacy_profile_state (old_device_id, association_status, operator_label, split_metadata)
		VALUES (?, 'kept', ?, ?)`, oldID, oldLabel, oldSplit); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	const ref = "mac:10:20:30:40:50:60"
	body := producerPayload("evt-leg", "k", "host.appeared",
		`{"host_id":"mac:10:20:30:40:50:60","identity_source":"mac","identity_confidence":"high","hostname":"operator name"}`)
	gid := persistReported(t, s, "lab", "evt-leg", "k", "host.appeared", body, now)
	if _, err := s.Recover(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectReported(ctx, "lab", 64); err != nil {
		t.Fatal(err)
	}
	profileID := subjectProfileID(t, s, "lab", SubjectReportedDevice, "", ref)
	var label sql.NullString
	if err := s.db.QueryRow(`SELECT operator_label FROM reported_subjects WHERE profile_id = ?`, profileID).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if profileID == oldID || label.Valid {
		t.Fatal("reported subject inherited a legacy identity or operator label")
	}
	if _, err := s.CloseDue(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(ctx, Briefing{
		GroupingVersion: GroupingLegacy, // caller-supplied provenance must not override the group
		BriefingID:      "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
		Baseline: "low", Priority: "low", PriorityReason: "r",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM group_subjects WHERE group_id = ? AND profile_id = ? AND role = 'host'`,
		`SELECT COUNT(*) FROM briefing_subjects bs JOIN briefings b ON b.briefing_id = bs.briefing_id
		 WHERE b.group_id = ? AND bs.profile_id = ? AND bs.role = 'host'`,
	} {
		var links int
		if err := s.db.QueryRow(query, gid, profileID).Scan(&links); err != nil || links != 1 {
			t.Fatalf("expected one reported-subject relation before purge, got %d: %v", links, err)
		}
	}
	var legacyRefs int
	if err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM reported_subjects WHERE upstream_reference = ? OR profile_id = ? OR operator_label = ?)
		+ (SELECT COUNT(*) FROM group_subjects WHERE profile_id = ?)
		+ (SELECT COUNT(*) FROM briefing_subjects WHERE profile_id = ?)
		+ (SELECT COUNT(*) FROM briefings WHERE host_anchor_key = ?)`,
		oldID, oldID, oldLabel, oldID, oldID, oldID).Scan(&legacyRefs); err != nil || legacyRefs != 0 {
		t.Fatalf("legacy identity/label leaked into current associations: %d, %v", legacyRefs, err)
	}
	stored, err := s.BriefingForGroup(ctx, gid)
	if err != nil || stored.GroupingVersion != GroupingScoped || taken.GroupingVersion != GroupingScoped {
		t.Fatalf("scoped provenance was not preserved: %+v, %v", stored, err)
	}
	if count, err := s.PurgeBriefings(ctx, now.Add(200*time.Hour), now.Add(time.Hour)); err != nil || count != 1 {
		t.Fatalf("purged %d briefings, want 1: %v", count, err)
	}
	var gotID, gotStatus, gotLabel, gotSplit string
	err = s.db.QueryRow(`SELECT old_device_id, association_status, operator_label, split_metadata
		FROM legacy_profile_state`).Scan(&gotID, &gotStatus, &gotLabel, &gotSplit)
	if err != nil {
		t.Fatal(err)
	}
	if gotID != oldID || gotStatus != "kept" || gotLabel != oldLabel || gotSplit != oldSplit {
		t.Fatalf("legacy row must survive untouched, got %q %q %q %q", gotID, gotStatus, gotLabel, gotSplit)
	}
	var legacyRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM legacy_profile_state`).Scan(&legacyRows); err != nil || legacyRows != 1 {
		t.Fatalf("legacy rows changed: %d, %v", legacyRows, err)
	}
}

// Version 12 overlaps the archived device line. Its schema is refused
// even though the reported-context line now supports the same number.
func TestOpenRefusesArchivedDeviceSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archived.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE device_anchors (anchor_key TEXT PRIMARY KEY)`,
		`INSERT INTO device_anchors VALUES ('keep-me')`,
		`PRAGMA user_version = 12`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("archived schema was admitted")
	} else if !strings.Contains(err.Error(), "unsupported database schema lineage") {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	var anchor string
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 12 {
		t.Fatalf("archived version changed: %d, %v", version, err)
	}
	if err := db.QueryRow(`SELECT anchor_key FROM device_anchors`).Scan(&anchor); err != nil || anchor != "keep-me" {
		t.Fatalf("archived data changed: %q, %v", anchor, err)
	}
}

func TestGroupingVersionChangeClosesPriorGroup(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	policy := GroupPolicy{QuietWindow: time.Hour, GroupingVersion: GroupingLegacy}
	persist := func(id string, pol GroupPolicy) (PersistResult, error) {
		return s.PersistEventAndGroup(ctx, RawEvent{
			EventID: id, EventKey: "k", EventType: "host.appeared", Payload: []byte(`{}`),
			OccurredAt: now, ReceivedAt: now, SignatureTimestamp: now,
		}, "same-anchor", pol)
	}
	old, err := persist("evt-old", policy)
	if err != nil {
		t.Fatal(err)
	}
	policy.GroupingVersion = GroupingScoped
	current, err := persist("evt-current", policy)
	if err != nil {
		t.Fatal(err)
	}
	if old.GroupID == current.GroupID || len(current.ClosedGroupIDs) != 1 || current.ClosedGroupIDs[0] != old.GroupID {
		t.Fatalf("different grouping versions shared a group: old=%+v new=%+v", old, current)
	}
	for id, want := range map[string]int{old.GroupID: GroupingLegacy, current.GroupID: GroupingScoped} {
		var version, count int
		if err := s.db.QueryRow(`SELECT grouping_version, event_count FROM active_groups WHERE group_id = ?`, id).Scan(&version, &count); err != nil || version != want || count != 1 {
			t.Fatalf("group %s provenance/count = %d/%d: %v", id, version, count, err)
		}
	}
	policy.GroupingVersion = GroupingScoped + 1
	if _, err := persist("evt-invalid", policy); err == nil {
		t.Fatal("unsupported grouping version was accepted")
	}
	if _, err := s.StoredEvent(ctx, "evt-invalid"); err != sql.ErrNoRows {
		t.Fatalf("invalid provenance persisted an event: %v", err)
	}
}

// v13 adds purge-run state, purge-serving indexes, and token usage
// tracking. Rows predating tracking get the migration instant as last
// known use, never zero (which would read as long-unused) and never
// creation (which would expire steadily-used tokens on upgrade).
func TestMigrateV13PurgeStateAndTokenUsage(t *testing.T) {
	s := openTestStore(t)
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != supportedSchemaVersion {
		t.Fatalf("user_version = %d, %v; want %d", version, err, supportedSchemaVersion)
	}
	var tables int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = 'purge_state'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("purge_state table missing: %d %v", tables, err)
	}
	var indexes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('reported_observations_received','reported_subjects_last_report')`).Scan(&indexes); err != nil || indexes != 2 {
		t.Fatalf("purge-serving indexes missing: %d %v", indexes, err)
	}
	ctx := context.Background()
	tok, err := s.GetOrAssignPseudonym(ctx, "ip", "10.9.9.9")
	if err != nil || tok == "" {
		t.Fatal(err)
	}
	var created, used int64
	if err := s.db.QueryRow(`SELECT created_at, last_used_at FROM pseudonym_map WHERE token = ?`, tok).Scan(&created, &used); err != nil {
		t.Fatal(err)
	}
	if used < created || used == 0 {
		t.Fatalf("usage must start at assignment: created=%d used=%d", created, used)
	}
	if _, err := s.db.Exec(`UPDATE pseudonym_map SET last_used_at = 0 WHERE token = ?`, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOrAssignPseudonyms(ctx, "ip", "10.9.9.9"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT last_used_at FROM pseudonym_map WHERE token = ?`, tok).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used == 0 {
		t.Fatal("a lookup hit must refresh usage, or steady devices expire")
	}
}

func TestGroupingProvenanceUpgrade(t *testing.T) {
	for _, from := range []int{9, 11} {
		t.Run(fmt.Sprint(from), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "bridge.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			gid := seedFeedGroup(t, s, "host:historical", "evt-historical")
			if _, err := s.CloseDue(ctx, now.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.TakeGroup(ctx, gid); err != nil {
				t.Fatal(err)
			}
			if err := s.InsertBriefing(ctx, Briefing{
				BriefingID: "brf-history", GroupID: gid, HostAnchorKey: "host:historical",
				Baseline: "low", Priority: "low", PriorityReason: "r", WhatChanged: "original text",
				Context: "original context", RecommendedCheck: "original check", ModelRawJSON: `{"audit":"kept"}`, CreatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			before, err := s.BriefingForGroup(ctx, gid)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := s.StoredEvent(ctx, "evt-historical")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			if from == 9 {
				dropProjectionSchema(t, db)
			} else {
				dropGroupingProvenance(t, db)
			}
			dropDecisionSchema(t, db)
			dropTraceSchema(t, db)
			dropRuleSchema(t, db)
			dropSubjectRevision(t, db)
			if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, from)); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			after, err := s.BriefingForGroup(ctx, gid)
			if err != nil {
				t.Fatal(err)
			}
			wantVersion := GroupingUnknown
			if from == 9 {
				wantVersion = GroupingLegacy
			}
			var groupVersion int
			if err := s.db.QueryRow(`SELECT grouping_version FROM active_groups WHERE group_id = ?`, gid).Scan(&groupVersion); err != nil {
				t.Fatal(err)
			}
			if after.GroupingVersion != wantVersion || groupVersion != wantVersion {
				t.Fatalf("historical provenance = %d/%d, want %d", after.GroupingVersion, groupVersion, wantVersion)
			}
			after.GroupingVersion = before.GroupingVersion
			// The seed row was written by current code (source model),
			// then rewound to pre-column history: the conservative
			// v16 backfill must report legacy, never guess the old
			// model value. Normalize like grouping version and pin
			// the backfill explicitly.
			after.Kind, after.Source = before.Kind, "legacy"
			before.Source = "legacy"
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("migration changed historical briefing: before=%+v after=%+v", before, after)
			}
			if after.Kind != "other" {
				t.Fatalf("backfilled kind = %q, want other", after.Kind)
			}
			retained, err := s.StoredEvent(ctx, "evt-historical")
			if err != nil || !bytes.Equal(raw.Payload, retained.Payload) {
				t.Fatalf("migration changed raw evidence: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			restarted, err := s.BriefingForGroup(ctx, gid)
			if err != nil || restarted.GroupingVersion != wantVersion {
				t.Fatalf("restart changed provenance: %+v, %v", restarted, err)
			}
		})
	}
}

// TestMigrateSupportedFromV13 rewinds a database holding retained
// briefings to the v13 schema and reopens it: the upgrade must reach
// the supported schema with the run ledger, the protected-token
// scratch table, the serving indexes, and the v16 kind/source columns
// backfilled conservatively (fallback rows to fallback, everything
// else to legacy) without touching retained rows.
func TestMigrateSupportedFromV13(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v13.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	seedBriefing := func(eventID string, fallback bool, kind, source string) string {
		body := producerPayload(eventID, "k", "host.appeared", `{"ip":"192.168.7.71"}`)
		gid := persistReported(t, s, "lab", eventID, "k", "host.appeared", body, old)
		projectAll(t, s)
		if _, err := s.CloseDue(ctx, old.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		taken, err := s.TakeGroup(ctx, gid)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.InsertBriefing(ctx, Briefing{
			BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: taken.Anchor,
			Baseline: "low", Priority: "low", PriorityReason: "r",
			WhatChanged: "w", Context: "c", RecommendedCheck: "k",
			FallbackUsed: fallback, Kind: kind, Source: source, CreatedAt: old,
		}); err != nil {
			t.Fatal(err)
		}
		return taken.GroupID
	}
	fallbackGID := seedBriefing("evt-v13-fallback", true, "", "")
	modelGID := seedBriefing("evt-v13-model", false, "", "")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	dropHeartbeatSchema(t, raw)
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS purge_runs`,
		`DROP TABLE IF EXISTS purge_protected_tokens`,
		`DROP TABLE IF EXISTS operator_decisions`,
		`DROP INDEX IF EXISTS group_subjects_profile`,
		`DROP INDEX IF EXISTS briefing_subjects_profile`,
		`DROP INDEX IF EXISTS reported_observations_profile_received`,
		`DROP INDEX IF EXISTS briefings_created_kind`,
		`DROP TABLE IF EXISTS agent_traces`,
		`DROP TABLE IF EXISTS review_rules`,
		`PRAGMA user_version = 13`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatal(err)
		}
	}
	dropColumnIfExists(t, raw, "briefings", "kind")
	dropColumnIfExists(t, raw, "briefings", "source")
	for _, col := range []string{"likely_explanation", "confidence", "evidence_refs_json",
		"input_tokens", "output_tokens", "cost_micros"} {
		dropColumnIfExists(t, raw, "briefings", col)
	}
	dropColumnIfExists(t, raw, "reported_subjects", "label_owner")
	dropColumnIfExists(t, raw, "reported_subjects", "identity_revision")
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var version int
	if err := s2.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != supportedSchemaVersion {
		t.Fatalf("user_version = %d, %v; want %d", version, err, supportedSchemaVersion)
	}
	var tables, indexes int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('purge_runs','purge_protected_tokens')`).Scan(&tables); err != nil || tables != 2 {
		t.Fatalf("v14 purge tables missing: %d %v", tables, err)
	}
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('group_subjects_profile','briefing_subjects_profile','reported_observations_profile_received','briefings_created_kind')`).Scan(&indexes); err != nil || indexes != 4 {
		t.Fatalf("association, receipt and digest indexes missing: %d %v", indexes, err)
	}
	for _, tc := range []struct {
		groupID    string
		wantKind   string
		wantSource string
	}{
		{fallbackGID, "other", "fallback"},
		{modelGID, "other", "legacy"},
	} {
		b, err := s2.BriefingForGroup(ctx, tc.groupID)
		if err != nil {
			t.Fatal(err)
		}
		if b.Kind != tc.wantKind || b.Source != tc.wantSource {
			t.Fatalf("backfilled briefing = kind %q source %q, want %q %q", b.Kind, b.Source, tc.wantKind, tc.wantSource)
		}
	}
}
