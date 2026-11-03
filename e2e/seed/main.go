// Command seed builds synthetic representative databases for Step 31
// retention, purge, migration and load evidence. Everything is written
// through production store APIs (never raw SQL, except the explicit
// schema downgrade), so the database exercises real code paths.
//
// Default workload (documented; override with flags): 600 raw events
// across 300 subjects (mac-qualified, ip-qualified, unqualified),
// occurred spread over 100 days with a recent tail, payloads varying
// 0-8 ports, 20 decisions, 10 rules, 50 pseudonyms, 5 heartbeats,
// traces on every 10th briefing, and one formed incident. The aged
// majority exceeds the 90d briefing horizon so a purge pass must run
// partial (≤200 rows/transaction) across several passes.
//
// -downgrade v13 replays the TestMigrateSupportedFromV13 downgrade
// (drop post-v13 tables/indexes/columns, user_version=13) so the next
// bridge start migrates a supported lineage at size.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

func main() {
	db := flag.String("db", "", "database path (required)")
	scope := flag.String("scope", "default", "source scope")
	namespace := flag.String("namespace", "e2e-age-ns", "identity namespace")
	capabilities := flag.String("capabilities", "stable-identity-v1", "capability tokens")
	nEvents := flag.Int("events", 600, "raw events to seed")
	nSubjects := flag.Int("subjects", 300, "distinct subjects")
	ageDays := flag.Int("age-days", 100, "oldest occurred age in days")
	downgrade := flag.String("downgrade", "", "schema downgrade to apply after seeding (v13)")
	flag.Parse()
	if *db == "" || *nSubjects <= 0 || *nEvents < *nSubjects {
		log.Fatal("db is required; need events >= subjects > 0")
	}
	ctx := context.Background()
	st, err := store.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	if err := st.BindSourceScope(ctx, *scope); err != nil {
		log.Fatal(err)
	}
	if err := st.BindSourceIdentity(ctx, *scope, *namespace, *capabilities); err != nil {
		log.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	seed := &seeder{store: st, scope: *scope, now: now, ageDays: *ageDays, dbPath: *db}

	perSubject := *nEvents / *nSubjects
	var gids []string
	var briefedAt []time.Time
	for s := 0; s < *nSubjects; s++ {
		anchor := fmt.Sprintf("seed-anchor-%04d", s)
		for e := 0; e < perSubject; e++ {
			idx := s*perSubject + e
			// Even age spread; the newest 5% land within the hour so the
			// finished database holds open groups and fresh briefings too.
			occurred := now.Add(-time.Duration(*ageDays*24*3600*(idx+1) / *nEvents) * time.Second)
			if idx >= *nEvents-*nEvents/20 {
				occurred = now.Add(-time.Duration((idx%30)+1) * time.Minute)
			}
			typ := "host.appeared"
			if e%2 == 1 {
				typ = "host.changed"
			}
			id := fmt.Sprintf("seed-%06d", idx)
			payload := seed.hostPayload(s, typ, occurred)
			res, err := st.PersistEventAndGroup(ctx, store.RawEvent{
				EventID: id, EventKey: fmt.Sprintf("seed-key-%04d", s), EventType: typ,
				OccurredAt: occurred, ReceivedAt: occurred, SignatureTimestamp: occurred,
				Payload: payload,
			}, anchor, store.GroupPolicy{QuietWindow: time.Hour})
			if err != nil {
				log.Fatal(err)
			}
			if res.GroupID != "" && (len(gids) == 0 || gids[len(gids)-1] != res.GroupID) {
				gids = append(gids, res.GroupID)
			}
		}
	}
	if _, err := st.ProjectReported(ctx, *scope, 1<<20); err != nil {
		log.Fatal(err)
	}
	if _, err := st.CloseDue(ctx, now); err != nil {
		log.Fatal(err)
	}
	nBriefed, nTraced := 0, 0
	var takenGroups []string
	for i, gid := range gids {
		taken, err := st.TakeGroup(ctx, gid)
		if err != nil {
			continue // still open: stays unbriefed by design
		}
		takenGroups = append(takenGroups, taken.GroupID)
		created := taken.OpenedAt
		if created.IsZero() {
			created = now
		}
		b := store.Briefing{
			BriefingID: "brf-" + taken.GroupID, GroupID: taken.GroupID,
			HostAnchorKey: taken.Anchor, Baseline: "low", Priority: "low",
			PriorityReason: "seed", WhatChanged: "seed", Context: "seed",
			RecommendedCheck: "seed", CreatedAt: created,
		}
		if i%2 == 0 {
			b.FallbackUsed = true
		}
		if i%10 == 0 {
			b.Traces = []store.AgentTrace{{Step: 0, Tool: "get_reported_profile",
				ArgsJSON: `{"host":"seed"}`, ResultJSON: `{}`, Status: "ok"}}
			nTraced++
		}
		if err := st.InsertBriefing(ctx, b); err != nil {
			log.Fatal(err)
		}
		nBriefed++
		briefedAt = append(briefedAt, created)
	}
	_ = briefedAt
	// Decisions on the first briefings, mixed kinds.
	// Decisions bind to seeded briefing rows; the run.sh scenario asserts
	// purge/drain behavior over them.
	if err := seed.decisions(ctx, takenGroups); err != nil {
		log.Fatal(err)
	}
	if err := seed.rules(ctx, now); err != nil {
		log.Fatal(err)
	}
	if err := seed.pseudonyms(ctx); err != nil {
		log.Fatal(err)
	}
	if err := seed.heartbeats(ctx, now); err != nil {
		log.Fatal(err)
	}
	// One incident from three recent qualified subjects sharing a target.
	rep, err := st.CorrelateIncidents(ctx, store.DefaultIncidentSettings(), now)
	if err != nil {
		log.Fatal(err)
	}
	if err := st.Close(); err != nil {
		log.Fatal(err)
	}
	if *downgrade != "" {
		if *downgrade != "v13" {
			log.Fatalf("unsupported downgrade %q (only v13)", *downgrade)
		}
		downgradeToV13(*db)
	}
	fmt.Printf("seeded db=%s subjects=%d groups=%d briefed=%d traced=%d incidents: created=%d appended=%d closed=%d\n",
		*db, *nSubjects, len(gids), nBriefed, nTraced, rep.Created, rep.Appended, rep.Closed)
}

type seeder struct {
	store   *store.Store
	scope   string
	now     time.Time
	ageDays int
	dbPath  string
}

// hostPayload builds a producer-shaped host view. Kind cycles
// mac-qualified, ip-qualified, unqualified; ports vary 0-8 for payload
// size spread. MACs are locally administered (02:53::/32 style
// documentation space would collide textually; 02:00:00/24 varies the
// low octets instead).
func (s *seeder) hostPayload(subject int, typ string, occurred time.Time) []byte {
	mac := fmt.Sprintf("02:00:%02x:%02x:%02x:%02x", subject>>16&0xff, subject>>8&0xff, subject&0xff, subject%251+1)
	ip := fmt.Sprintf("198.51.100.%d", subject%250+1)
	hostID, source, confidence := "", "", ""
	switch subject % 3 {
	case 0:
		hostID, source, confidence = "mac:"+mac[0:2]+mac[3:5]+mac[6:8]+mac[9:11]+mac[12:14]+mac[15:17], "mac", "high"
	case 1:
		hostID, source, confidence = "ip:"+ip, "ip", "low"
	}
	_ = occurred
	ports := ""
	for p := 0; p < subject%9; p++ {
		if p > 0 {
			ports += ","
		}
		ports += fmt.Sprintf(`{"number":%d,"protocol":"tcp","state":"open","service":"s%d"}`, 1000+p, p)
	}
	view := fmt.Sprintf(`{"ip":%q,"hostname":"seed-host-%04d","mac":%q,"randomized_mac":false,`+
		`"vendor":"Seed Systems","os":"","device":"sensor","source":%q,"alive":true,"weak":false,`+
		`"ports":[%s],"host_id":%q,"identity_confidence":%q,"identity_source":%q}`,
		ip, subject, mac, source, ports, hostID, confidence, source)
	body := fmt.Sprintf(`{"cycle":7,"target":"203.0.113.0/24","host":%s`, view)
	if typ == "host.changed" {
		prev := fmt.Sprintf(`{"ip":"203.0.113.%d","hostname":"seed-host-%04d","mac":%q,"randomized_mac":false,`+
			`"vendor":"Seed Systems","os":"","device":"sensor","source":%q,"alive":true,"weak":false,`+
			`"host_id":%q,"identity_confidence":%q,"identity_source":%q}`,
			subject%250+2, subject, mac, source, hostID, confidence, source)
		body = fmt.Sprintf(`{"cycle":7,"target":"203.0.113.0/24","previous":%s,"current":%s,`+
			`"changes":[{"field":"ip","before":"x","after":"y"}]`, prev, view)
	}
	return []byte(body + "}")
}

func (s *seeder) decisions(ctx context.Context, groups []string) error {
	// Decisions bind to taken group IDs, the only subject IDs
	// canonicalBriefingID resolves. Text-bearing kinds (note, label)
	// carry text; expected/not-expected take none; snooze takes an
	// explicit future horizon.
	now := time.Now().UTC()
	for i, gid := range groups {
		if i >= 16 {
			break
		}
		in := store.DecisionInput{SubjectKind: store.DecisionSubjectBriefing,
			SubjectID: gid, RequestID: fmt.Sprintf("seed-dec-%02d", i)}
		switch i % 4 {
		case 0:
			in.Kind = store.DecisionExpected
		case 1:
			in.Kind = store.DecisionNotExpected
		case 2:
			in.Kind, in.Text = store.DecisionNote, "seed note"
		case 3:
			in.Kind, in.HasUntil, in.Until = store.DecisionSnooze, true, now.Add(24*time.Hour)
		}
		if _, err := s.store.AppendDecision(ctx, in); err != nil {
			return err
		}
	}
	// Labels belong to exact profiles: read profile IDs through a
	// short-lived read handle (no writes are in flight here).
	ro, err := sql.Open("sqlite3", s.dbPath+"?mode=ro")
	if err != nil {
		return err
	}
	rows, err := ro.Query(`SELECT profile_id FROM reported_subjects LIMIT 4`)
	if err != nil {
		ro.Close()
		return err
	}
	var pids []string
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			ro.Close()
			return err
		}
		pids = append(pids, pid)
	}
	rows.Close()
	ro.Close()
	for i, pid := range pids {
		kind, text := store.DecisionNote, "seed note"
		if i%2 == 1 {
			kind, text = store.DecisionLabel, "seed-label"
		}
		in := store.DecisionInput{SubjectKind: store.DecisionSubjectProfile,
			SubjectID: pid, Kind: kind, Text: text,
			RequestID: fmt.Sprintf("seed-profdec-%02d", i)}
		if _, err := s.store.AppendDecision(ctx, in); err != nil {
			return err
		}
	}
	return nil
}

func (s *seeder) rules(ctx context.Context, now time.Time) error {
	for i := 0; i < 10; i++ {
		aged := now
		if i%2 == 0 {
			aged = now.Add(-time.Duration(s.ageDays) * 24 * time.Hour)
		}
		r := store.Rule{
			RuleID: fmt.Sprintf("OP_RULE_seed%02d", i), ProposalID: fmt.Sprintf("OP_PROP_seed%02d", i),
			SourceScope: s.scope, ReferenceClass: store.SubjectReportedDevice,
			IdentityNamespace: "e2e-age-ns", UpstreamReference: fmt.Sprintf("mac:02:00:00:00:%02x:%02x", i, i),
			ReportedIdentitySource: "mac", ReportedConfidence: "high",
			Kind: "other", PortsCSV: "tcp/22/open",
			Status: store.RuleApproved, DecidedAt: aged, ApprovedAt: aged, HasApprovedAt: true,
		}
		if err := s.store.PutRule(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func (s *seeder) pseudonyms(ctx context.Context) error {
	names := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		names = append(names, fmt.Sprintf("seed-pseudo-%02d.example", i))
	}
	_, err := s.store.GetOrAssignPseudonyms(ctx, "hostname", names...)
	return err
}

func (s *seeder) heartbeats(ctx context.Context, now time.Time) error {
	for i := 0; i < 5; i++ {
		at := now
		if i < 3 {
			at = now.Add(-time.Duration(s.ageDays) * 24 * time.Hour)
		}
		h := store.Heartbeat{
			HeartbeatSummary: store.HeartbeatSummary{
				ScanID: fmt.Sprintf("%032x", i+1), Target: "203.0.113.0/24",
				CompletedAt: at, ReceivedAt: at, DurationMS: 1000, Status: "ok"},
			SourceScope: s.scope, SignatureAt: at, Payload: []byte("{}"),
		}
		if _, err := s.store.InsertHeartbeat(ctx, h); err != nil {
			return err
		}
	}
	return nil
}

// downgradeToV13 replays the TestMigrateSupportedFromV13 downgrade so the
// next bridge start migrates a supported lineage at size. The statement
// list mirrors internal/store/legacy_test.go; keep them in sync.
func downgradeToV13(path string) {
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		log.Fatal(err)
	}
	defer raw.Close()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS incident_pending_refs`,
		`DROP TABLE IF EXISTS incident_pending`,
		`DROP TABLE IF EXISTS incident_references`,
		`DROP TABLE IF EXISTS incident_members`,
		`DROP TABLE IF EXISTS incidents`,
		`DROP TABLE IF EXISTS incident_progress`,
		`DROP INDEX IF EXISTS incidents_status_closed`,
		`DROP INDEX IF EXISTS decisions_generated`,
		`DROP TABLE IF EXISTS heartbeats`,
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
			log.Fatal(stmt, err)
		}
	}
	for _, col := range []string{"kind", "source", "likely_explanation", "confidence",
		"evidence_refs_json", "input_tokens", "output_tokens", "cost_micros"} {
		_, _ = raw.Exec(fmt.Sprintf(`ALTER TABLE briefings DROP COLUMN %s`, col))
	}
	// Heartbeat columns must rewind too or the migration's ADD COLUMN
	// fails on duplicates (mirrors dropHeartbeatSchema).
	_, _ = raw.Exec(`ALTER TABLE report_sources DROP COLUMN heartbeat_completed_at`)
	_, _ = raw.Exec(`ALTER TABLE report_sources DROP COLUMN heartbeat_summary`)
}
