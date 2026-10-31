package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Step 18d cutover guards: the current tree never implemented the old
// continuity engine, so there is no inference to disable — these tests
// pin that down. Legacy profile rows are never written, read, or
// repurposed by any runtime path, and databases newer than the
// supported schema are refused unchanged rather than opened.

// Opening a database from a newer development line (v12-v14) or any
// future version must fail before any migration runs, leaving the
// file exactly as found.
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

// A pre-existing legacy profile row survives every runtime path
// byte-identical and is never consulted: recovery, projection,
// briefing and purge neither read it nor derive new identity from it,
// and a fresh flow writes no legacy rows at all.
func TestLegacyProfileStateQuarantined(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const oldID, oldLabel, oldSplit = "dev-legacy-1", "operator name", "split:1"
	if _, err := s.db.Exec(`INSERT INTO legacy_profile_state (old_device_id, association_status, operator_label, split_metadata)
		VALUES (?, 'kept', ?, ?)`, oldID, oldLabel, oldSplit); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	gid := seedFeedGroup(t, s, "host:leg", "evt-leg")
	if _, err := s.Recover(ctx, now); err != nil {
		t.Fatal(err)
	}
	projectAll(t, s)
	if _, err := s.CloseDue(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakeGroup(ctx, gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBriefing(ctx, Briefing{
		BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID, HostAnchorKey: "host:leg",
		Baseline: "low", Priority: "low", PriorityReason: "r",
		WhatChanged: "w", Context: "c", RecommendedCheck: "k", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeBriefings(ctx, now.Add(200*time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
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
	var legacyRefs int
	err = s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM reported_subjects WHERE upstream_reference = ?)
		+ (SELECT COUNT(*) FROM briefings WHERE host_anchor_key = ?)`, oldID, oldID).Scan(&legacyRefs)
	if err != nil {
		t.Fatal(err)
	}
	if legacyRefs != 0 {
		t.Fatalf("no new identity may derive from legacy ids, got %d references", legacyRefs)
	}
}
