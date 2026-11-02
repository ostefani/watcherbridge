package store

import (
	"database/sql"
	"fmt"
)

const schemaV1 = `
CREATE TABLE IF NOT EXISTS raw_events (
    event_id TEXT PRIMARY KEY,
    event_key TEXT NOT NULL,
    event_type TEXT NOT NULL,
    occurred_at INTEGER NOT NULL,
    cycle INTEGER NOT NULL,
    target TEXT,
    payload BLOB NOT NULL,
    headers_json TEXT NOT NULL DEFAULT '',
    received_at INTEGER NOT NULL,
    signature_timestamp INTEGER NOT NULL,
    forwarded INTEGER NOT NULL DEFAULT 0,
    forwarded_at INTEGER,
    forward_attempts INTEGER NOT NULL DEFAULT 0,
    last_forward_error TEXT,
    delivery_error TEXT
);
CREATE TABLE IF NOT EXISTS active_groups (
    group_id TEXT PRIMARY KEY,
    host_anchor_key TEXT NOT NULL,
    opened_at INTEGER NOT NULL,
    last_event_at INTEGER NOT NULL,
    deadline_at INTEGER NOT NULL,
    event_count INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL CHECK(status IN ('open','processing','closed','done','failed'))
);
CREATE TABLE IF NOT EXISTS group_events (
    group_id TEXT NOT NULL REFERENCES active_groups(group_id) ON DELETE CASCADE,
    event_id TEXT NOT NULL REFERENCES raw_events(event_id),
    PRIMARY KEY(group_id, event_id)
);
CREATE TABLE IF NOT EXISTS briefings (
    briefing_id TEXT PRIMARY KEY,
    group_id TEXT NOT NULL REFERENCES active_groups(group_id),
    host_anchor_key TEXT NOT NULL,
    baseline TEXT NOT NULL,
    model_suggestion TEXT,
    priority TEXT NOT NULL,
    priority_reason TEXT NOT NULL,
    clamped INTEGER NOT NULL DEFAULT 0,
    fallback_used INTEGER NOT NULL DEFAULT 0,
    what_changed TEXT NOT NULL,
    context TEXT NOT NULL,
    recommended_check TEXT NOT NULL,
    model_raw_json TEXT,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS pseudonym_map (
    real_value TEXT PRIMARY KEY,
    category TEXT NOT NULL CHECK(category IN ('ip','mac','hostname','net')),
    token TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS pseudonym_counters (
    category TEXT PRIMARY KEY,
    next INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS one_open_group_per_anchor ON active_groups(host_anchor_key) WHERE status = 'open';
CREATE UNIQUE INDEX IF NOT EXISTS one_briefing_per_group ON briefings(group_id);
CREATE INDEX IF NOT EXISTS raw_forward_sweep ON raw_events(forwarded, signature_timestamp);
CREATE INDEX IF NOT EXISTS raw_received ON raw_events(received_at);
CREATE INDEX IF NOT EXISTS groups_status_deadline ON active_groups(status, deadline_at);
`

const supportedSchemaVersion = 22

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > supportedSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported %d; upgrade the binary", version, supportedSchemaVersion)
	}
	// The archived device engine used overlapping version numbers. A
	// version alone must never admit that incompatible schema lineage.
	if version >= 10 {
		var tables int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'
			AND name IN ('report_sources', 'reported_subjects', 'projection_progress')`).Scan(&tables); err != nil {
			return err
		}
		if tables != 3 {
			return fmt.Errorf("unsupported database schema lineage at version %d; archived device databases require a separate upgrade path", version)
		}
	}
	initialVersion := version
	if version == 0 {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='raw_events'`).Scan(&name)
		if err == nil {
			return fmt.Errorf("unversioned legacy database is not supported; delete the database file and restart to recreate it")
		}
		if err != sql.ErrNoRows {
			return err
		}
	}
	for version < supportedSchemaVersion {
		var err error
		switch version {
		case 0:
			err = applyMigration(db, 1, func(tx *sql.Tx) error { _, err := tx.Exec(schemaV1); return err })
		case 1:
			err = applyMigration(db, 2, migrateV2)
		case 2:
			err = applyMigration(db, 3, migrateV3)
		case 3:
			err = applyMigration(db, 4, migrateV4)
		case 4:
			err = applyMigration(db, 5, migrateV5)
		case 5:
			err = applyMigration(db, 6, migrateV6)
		case 6:
			err = applyMigration(db, 7, migrateV7)
		case 7:
			err = applyMigration(db, 8, migrateV8)
		case 8:
			err = applyMigration(db, 9, migrateV9)
		case 9:
			err = applyMigration(db, 10, migrateV10)
		case 10:
			err = applyMigration(db, 11, migrateV11)
		case 11:
			err = applyMigration(db, 12, func(tx *sql.Tx) error {
				return migrateV12(tx, initialVersion <= 9)
			})
		case 12:
			err = applyMigration(db, 13, migrateV13)
		case 13:
			err = applyMigration(db, 14, migrateV14)
		case 14:
			err = applyMigration(db, 15, migrateV15)
		case 15:
			err = applyMigration(db, 16, migrateV16)
		case 16:
			err = applyMigration(db, 17, migrateV17)
		case 17:
			err = applyMigration(db, 18, migrateV18)
		case 18:
			err = applyMigration(db, 19, migrateV19)
		case 19:
			err = applyMigration(db, 20, migrateV20)
		case 20:
			err = applyMigration(db, 21, migrateV21)
		case 21:
			err = applyMigration(db, 22, migrateV22)
		}
		if err != nil {
			return err
		}
		version++
	}
	return nil
}

// migrateV12 adds grouping provenance without rewriting historical payloads,
// text, identifiers or sequence values. SQLite's constant column default
// supplies the historical value without a table-wide UPDATE/rebuild.
// Phase-5 databases used legacy grouping. Existing v10/v11 databases can
// contain both algorithms; without a durable marker their origin is unknown.
// Never recover provenance by interpreting attacker-controlled anchor text.
func migrateV12(tx *sql.Tx, phase5 bool) error {
	version := GroupingUnknown
	if phase5 {
		version = GroupingLegacy
	}
	for _, table := range []string{"active_groups", "briefings"} {
		stmt := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN grouping_version INTEGER NOT NULL DEFAULT %d CHECK(grouping_version IN (0,1,2))`, table, version)
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV13 adds purge-run state, purge-serving indexes, and token
// usage tracking (Designv9 §4.6). purge_state persists per-table purge
// totals so Step 20 health can report them; each batch reselects its
// own remaining work, so a restart deletes whatever is still eligible.
// pseudonym_map gains last_used_at: creation age
// alone cannot prove a token is still referenced, so assignment and
// every later lookup bump it, and expiry compares against it. Rows
// predating tracking get the migration instant as their last known use:
// usage before tracking began is unknown, and deleting a token that a
// retained briefing still references would break viewer restoration.
func migrateV13(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE purge_state (
			table_name TEXT PRIMARY KEY,
			last_run_at INTEGER NOT NULL DEFAULT 0,
			last_cutoff INTEGER NOT NULL DEFAULT 0,
			rows_deleted_total INTEGER NOT NULL DEFAULT 0
		)`,
		`ALTER TABLE pseudonym_map ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0`,
		`UPDATE pseudonym_map SET last_used_at = CAST(strftime('%s','now') AS INTEGER) * 1000000000`,
		`CREATE INDEX IF NOT EXISTS reported_observations_received ON reported_observations(received_at)`,
		`CREATE INDEX IF NOT EXISTS reported_subjects_last_report ON reported_subjects(last_report_at)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV14 adds the purge run ledger, the protected-token scratch
// table, and profile-leading association indexes (Designv9 §§3.3, 4.6).
// purge_runs records per-run status, duration and per-table counts so
// Step 20 health can distinguish a completed run from one that
// committed batches and then failed; only the latest 30 rows are kept.
// purge_protected_tokens is a per-run scratch set of tokens named by
// retained briefings, rebuilt at each purge start (stale rows only ever
// over-protect). The association indexes serve the read API's
// per-subject counts without scanning the relation tables.
func migrateV14(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE purge_runs (
			run_id INTEGER PRIMARY KEY AUTOINCREMENT,
			started_at INTEGER NOT NULL,
			finished_at INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'running' CHECK(status IN ('running','completed','failed','interrupted')),
			error TEXT NOT NULL DEFAULT '',
			briefing_links_deleted INTEGER NOT NULL DEFAULT 0,
			briefings_deleted INTEGER NOT NULL DEFAULT 0,
			groups_deleted INTEGER NOT NULL DEFAULT 0,
			group_links_deleted INTEGER NOT NULL DEFAULT 0,
			ports_deleted INTEGER NOT NULL DEFAULT 0,
			observations_deleted INTEGER NOT NULL DEFAULT 0,
			progress_deleted INTEGER NOT NULL DEFAULT 0,
			raw_events_deleted INTEGER NOT NULL DEFAULT 0,
			subjects_deleted INTEGER NOT NULL DEFAULT 0,
			sources_deleted INTEGER NOT NULL DEFAULT 0,
			pseudonyms_deleted INTEGER NOT NULL DEFAULT 0,
			bookkeeping_writes INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE purge_protected_tokens (
			token TEXT PRIMARY KEY
		)`,
		`CREATE INDEX IF NOT EXISTS group_subjects_profile ON group_subjects(profile_id)`,
		`CREATE INDEX IF NOT EXISTS briefing_subjects_profile ON briefing_subjects(profile_id)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV15 adds the receipt-ordered observation index backing the
// latest-evidence read. The existing profile index orders by reported
// time, so LIMIT 1 by receipt time could scan and sort one subject's
// whole retained history; the new key serves the exact filter and
// ordering directly.
func migrateV15(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS reported_observations_profile_received ON reported_observations(profile_id, received_at, observation_id)`)
	return err
}

// migrateV16 adds the reported-change kind and briefing source to
// briefings (Designv9 §6.1, Plan Step 19). kind classifies the reported
// change for digest grouping and defaults to other; source names the
// briefing producer. The backfill is deliberately conservative: every
// retained row becomes legacy except rows the store knows came from the
// deterministic fallback, which become fallback. Historical model or
// rules output is never guessed. The (created_at, kind) index serves
// the digest window query directly.
func migrateV16(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE briefings ADD COLUMN kind TEXT NOT NULL DEFAULT 'other' CHECK(kind IN ('weak_appearance','weak_disappearance','port_closed','hostname_change','os_refinement','other'))`,
		`ALTER TABLE briefings ADD COLUMN source TEXT NOT NULL DEFAULT 'legacy' CHECK(source IN ('rules','model','agent','fallback','legacy'))`,
		`UPDATE briefings SET source = 'fallback' WHERE fallback_used`,
		`CREATE INDEX IF NOT EXISTS briefings_created_kind ON briefings(created_at, kind)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV17 persists the per-run group_events deletion count the
// retention engine already deletes but never attributed: without its
// own ledger column the run totals and health breakdown omit those
// rows. A plain ADD COLUMN suffices — no table rebuild.
func migrateV17(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE purge_runs ADD COLUMN group_events_deleted INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return nil
}

// migrateV18 adds operator decisions (Designv9 §5.1): immutable local
// audit rows with an explicit undo record instead of mutation, plus
// the durable label-ownership head on reported subjects. supersedes
// carries no foreign key: audit rows and their targets purge as
// age-selected pairs in one batch, so the pointer never needs the
// database to enforce it. label_owner is plain state, never joined —
// it may name a purged audit row, which only reads as "undo
// unavailable" (404), never as corruption.
func migrateV18(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE operator_decisions (
			decision_id TEXT PRIMARY KEY,
			subject_kind TEXT NOT NULL CHECK(subject_kind IN ('briefing','reported_subject')),
			subject_id TEXT NOT NULL,
			kind TEXT NOT NULL CHECK(kind IN ('expected','not_expected','snooze','note','label','undo')),
			until INTEGER,
			text TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			supersedes TEXT NOT NULL DEFAULT '',
			request_id TEXT UNIQUE,
			content_hash TEXT NOT NULL DEFAULT '',
			prior_label TEXT,
			prior_owner TEXT
		)`,
		`CREATE INDEX decisions_subject ON operator_decisions(subject_kind, subject_id, created_at)`,
		`CREATE INDEX decisions_created ON operator_decisions(created_at)`,
		`ALTER TABLE reported_subjects ADD COLUMN label_owner TEXT`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV19 persists the per-run decision-audit deletion count the
// retention engine already deletes but never attributed: without its
// own ledger column the run totals and health breakdown omit those
// rows. A plain ADD COLUMN suffices — no table rebuild.
func migrateV19(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE purge_runs ADD COLUMN decisions_deleted INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return nil
}

// migrateV20 records the resulting review head on every undo row.
// Repeat-undo conflict detection compares the current head against
// the head as the undo left it; recomputing that baseline from
// surviving rows cannot tell "no head existed" from "the head was
// purged", so the baseline must be pinned at commit time. NULL marks
// pre-migration undo rows with unavailable history, which repeat
// handling rejects conservatively. A plain ADD COLUMN suffices.
func migrateV20(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE operator_decisions ADD COLUMN head_asof TEXT`); err != nil {
		return err
	}
	return nil
}

// migrateV21 adds the Step 27 investigator audit surface (Designv9
// §6.3): display-only explanation fields and usage accounting on
// briefings, the tokenized agent_traces audit table, and its purge
// ledger column. Every briefing column stays nullable: retained rows
// predate the fields and deterministic briefings carry no model
// output, so NULL reads as unknown rather than a fabricated value. A
// plain ADD COLUMN plus one new table suffices — no rebuild.
func migrateV21(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE briefings ADD COLUMN likely_explanation TEXT`,
		`ALTER TABLE briefings ADD COLUMN confidence TEXT CHECK(confidence IN ('low','medium','high'))`,
		`ALTER TABLE briefings ADD COLUMN evidence_refs_json TEXT`,
		`ALTER TABLE briefings ADD COLUMN input_tokens INTEGER`,
		`ALTER TABLE briefings ADD COLUMN output_tokens INTEGER`,
		`ALTER TABLE briefings ADD COLUMN cost_micros INTEGER`,
		`CREATE TABLE agent_traces (
			briefing_id TEXT NOT NULL,
			step INTEGER NOT NULL,
			tool TEXT NOT NULL,
			args_json TEXT NOT NULL DEFAULT '',
			result_json TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL CHECK(status IN ('ok','blocked','error')),
			duration_ms INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(briefing_id, step)
		)`,
		`CREATE INDEX agent_traces_briefing ON agent_traces(briefing_id, step)`,
		`ALTER TABLE purge_runs ADD COLUMN traces_deleted INTEGER NOT NULL DEFAULT 0`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV22 adds the Step 28 review-policy rule registry (Designv9
// §5.2): one row per deterministic scope with its approval lifecycle,
// plus the purge ledger column for rule deletes. Scopes are exact
// source/namespace/reference snapshots with change kind and canonical
// ports; matching is byte equality, never attribute similarity. A plain
// new table plus one ledger column suffices — no rebuild.
func migrateV22(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE review_rules (
			rule_id TEXT PRIMARY KEY,
			proposal_id TEXT NOT NULL UNIQUE,
			source_scope TEXT NOT NULL,
			reference_class TEXT NOT NULL CHECK(reference_class = 'reported_device'),
			identity_namespace TEXT NOT NULL DEFAULT '',
			upstream_reference TEXT NOT NULL,
			reported_identity_source TEXT NOT NULL,
			reported_confidence TEXT NOT NULL,
			kind TEXT NOT NULL,
			ports_csv TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL CHECK(status IN ('approved','dismissed','revoked')),
			decided_at INTEGER NOT NULL,
			approved_at INTEGER,
			expires_at INTEGER
		)`,
		`CREATE INDEX review_rules_scope ON review_rules(source_scope, identity_namespace, upstream_reference)`,
		`ALTER TABLE purge_runs ADD COLUMN rules_deleted INTEGER NOT NULL DEFAULT 0`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(db *sql.DB, to int, fn func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, to)); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateV2: retry_count on groups; pseudonym_map keyed by (category,
// real_value) with the original display value kept for exact restoration;
// counters seeded past the highest existing token per category.
func migrateV2(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE active_groups ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE pseudonym_map_v2 (
			category TEXT NOT NULL CHECK(category IN ('ip','mac','hostname','net')),
			real_value TEXT NOT NULL,
			display_value TEXT NOT NULL,
			token TEXT NOT NULL UNIQUE,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (category, real_value)
		)`,
		`INSERT INTO pseudonym_map_v2 (category, real_value, display_value, token, created_at)
			SELECT category, real_value, real_value, token, created_at FROM pseudonym_map`,
		`DROP TABLE pseudonym_map`,
		`ALTER TABLE pseudonym_map_v2 RENAME TO pseudonym_map`,
		`INSERT INTO pseudonym_counters (category, next)
			SELECT category, MAX(CAST(substr(token, instr(token, '_') + 1) AS INTEGER)) + 1 FROM pseudonym_map WHERE true GROUP BY category
			ON CONFLICT(category) DO UPDATE SET next = MAX(next, excluded.next)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV3: processing_since for stale-lease release, briefings gain
// event_ids_json plus CHECK constraints on the priority levels (table
// rebuild), and an event_id index on group_events for purge/FK lookups.
func migrateV3(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE active_groups ADD COLUMN processing_since INTEGER`,
		`CREATE TABLE briefings_v3 (
			briefing_id TEXT PRIMARY KEY,
			group_id TEXT NOT NULL REFERENCES active_groups(group_id),
			host_anchor_key TEXT NOT NULL,
			baseline TEXT NOT NULL CHECK(baseline IN ('low','medium','high','critical')),
			model_suggestion TEXT CHECK(model_suggestion IS NULL OR model_suggestion IN ('low','medium','high','critical')),
			priority TEXT NOT NULL CHECK(priority IN ('low','medium','high','critical')),
			priority_reason TEXT NOT NULL,
			clamped INTEGER NOT NULL DEFAULT 0,
			fallback_used INTEGER NOT NULL DEFAULT 0,
			what_changed TEXT NOT NULL,
			context TEXT NOT NULL,
			recommended_check TEXT NOT NULL,
			model_raw_json TEXT,
			created_at INTEGER NOT NULL,
			event_ids_json TEXT NOT NULL DEFAULT '[]'
		)`,
		`INSERT INTO briefings_v3 (briefing_id, group_id, host_anchor_key, baseline, model_suggestion, priority, priority_reason, clamped, fallback_used, what_changed, context, recommended_check, model_raw_json, created_at)
			SELECT briefing_id, group_id, host_anchor_key, baseline, model_suggestion, priority, priority_reason, clamped, fallback_used, what_changed, context, recommended_check, model_raw_json, created_at FROM briefings`,
		`DROP TABLE briefings`,
		`ALTER TABLE briefings_v3 RENAME TO briefings`,
		`CREATE UNIQUE INDEX one_briefing_per_group ON briefings(group_id)`,
		`CREATE INDEX IF NOT EXISTS group_events_event ON group_events(event_id)`,
		`CREATE INDEX IF NOT EXISTS briefings_created ON briefings(created_at)`,
		`UPDATE briefings SET event_ids_json = COALESCE((SELECT json_group_array(event_id) FROM (SELECT event_id FROM group_events WHERE group_events.group_id = briefings.group_id ORDER BY event_id)), '[]')`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV4: briefings gain the matched baseline rule ID (nullable: rows
// written before v4 have none), and the stale-lease scan gets a partial
// index so the grouper tick stays indexed as history accumulates.
func migrateV4(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE briefings ADD COLUMN baseline_rule TEXT`,
		`CREATE INDEX IF NOT EXISTS groups_processing_since ON active_groups(processing_since) WHERE status = 'processing'`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV5: groups gain next_attempt_at, the earliest time a released
// group may be briefed again. Releases park a bounded backoff there so a
// transient failure cannot hot-loop through the retry budget inside one
// drain pass; the briefing queue filters on it. NULL (pre-v5 rows and
// freshly closed groups) means due immediately.
func migrateV5(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE active_groups ADD COLUMN next_attempt_at INTEGER`,
		`CREATE INDEX IF NOT EXISTS groups_closed_next_attempt ON active_groups(status, next_attempt_at)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV6: groups gain total_bytes, a running payload-byte counter so
// the in-persist byte cap needs no per-event SUM. The backfill covers
// open groups only: closed groups take no more joins, so their counter
// is never read again. CAST to BLOB makes LENGTH count bytes even if a
// value ever lands with TEXT affinity.
func migrateV6(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE active_groups ADD COLUMN total_bytes INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE active_groups SET total_bytes = COALESCE((SELECT SUM(LENGTH(CAST(re.payload AS BLOB))) FROM raw_events re JOIN group_events ge ON ge.event_id = re.event_id WHERE ge.group_id = active_groups.group_id), 0) WHERE status = 'open'`)
	return err
}

// migrateV7: groups gain transient_refunds, counting takes forgiven for
// transient store pressure (saturated queue, shutdown, dead context).
// Without a bound a persistently saturated store would requeue the same
// group forever, each cycle costing a provider call after the model
// already ran. At maxTransientRefunds the next refund parks failed
// instead — the one terminal state that stops the spend.
func migrateV7(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE active_groups ADD COLUMN transient_refunds INTEGER NOT NULL DEFAULT 0`)
	return err
}

// migrateV8: briefings gain seq, the resumable-stream cursor (Designv8
// §2.2). Assigned by the writer as COALESCE(MAX(seq),0)+1 inside the
// briefing-insert transaction, so the single writer makes it race-free.
// Backfill orders by (created_at, rowid): rowid breaks created_at ties
// deterministically. Purge leaves gaps; replay by seq > last is
// unaffected. A rebuild (not ADD COLUMN) enforces NOT NULL at the
// schema level — ADD COLUMN cannot add the constraint afterwards, and a
// UNIQUE index alone still admits multiple NULLs.
func migrateV8(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE briefings_v8 (
			briefing_id TEXT PRIMARY KEY,
			group_id TEXT NOT NULL REFERENCES active_groups(group_id),
			host_anchor_key TEXT NOT NULL,
			baseline TEXT NOT NULL CHECK(baseline IN ('low','medium','high','critical')),
			model_suggestion TEXT CHECK(model_suggestion IS NULL OR model_suggestion IN ('low','medium','high','critical')),
			priority TEXT NOT NULL CHECK(priority IN ('low','medium','high','critical')),
			priority_reason TEXT NOT NULL,
			baseline_rule TEXT,
			clamped INTEGER NOT NULL DEFAULT 0,
			fallback_used INTEGER NOT NULL DEFAULT 0,
			what_changed TEXT NOT NULL,
			context TEXT NOT NULL,
			recommended_check TEXT NOT NULL,
			model_raw_json TEXT,
			created_at INTEGER NOT NULL,
			event_ids_json TEXT NOT NULL DEFAULT '[]',
			seq INTEGER NOT NULL
		)`,
		`INSERT INTO briefings_v8 (briefing_id, group_id, host_anchor_key, baseline, model_suggestion, priority, priority_reason, baseline_rule, clamped, fallback_used, what_changed, context, recommended_check, model_raw_json, created_at, event_ids_json, seq)
			SELECT briefing_id, group_id, host_anchor_key, baseline, model_suggestion, priority, priority_reason, baseline_rule, clamped, fallback_used, what_changed, context, recommended_check, model_raw_json, created_at, event_ids_json,
				ROW_NUMBER() OVER (ORDER BY created_at, rowid)
			FROM briefings`,
		`DROP TABLE briefings`,
		`ALTER TABLE briefings_v8 RENAME TO briefings`,
		`CREATE UNIQUE INDEX one_briefing_per_group ON briefings(group_id)`,
		`CREATE INDEX IF NOT EXISTS briefings_created ON briefings(created_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS briefings_seq ON briefings(seq)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV9: seq gets a durable high-water mark independent of retained
// briefing rows. MAX(seq)+1 reuses ids after a purge deletes the maximum
// (or everything), and a client holding an old cursor would then silently
// skip new briefings on reconnect. seq_state survives purges; the insert
// path bumps it in the same transaction (UPDATE ... RETURNING), so ids
// stay monotonic across purges and restarts under the single writer.
func migrateV9(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE seq_state (
			id INTEGER PRIMARY KEY CHECK(id = 0),
			next_seq INTEGER NOT NULL
		)`,
		`INSERT INTO seq_state (id, next_seq) SELECT 0, COALESCE(MAX(seq), 0) + 1 FROM briefings`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV10: reported-context projection tables (Designv9 §4.1). Subjects
// are opaque read-model rows keyed by exact scoped upstream references,
// never inferred identities. Observations carry one row per event view;
// ports hang off observations; group/briefing relations are explicit
// link rows written in the same transactions as their parents.
// projection_progress records per-event projection state so restarts and
// the bounded projector resume without rework, and the purge (Step 18e)
// can protect unprojected evidence. legacy_profile_state is an empty
// read-only shelf for pre-Step-18 inferred profiles, if that development
// line is ever supported; this checkout creates no legacy rows.
func migrateV10(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE report_sources (
			source_scope TEXT PRIMARY KEY,
			configured_identity_namespace TEXT NOT NULL DEFAULT '',
			capabilities TEXT NOT NULL DEFAULT '',
			last_report_at INTEGER,
			heartbeat_enabled INTEGER NOT NULL DEFAULT 0,
			last_heartbeat_at INTEGER,
			delivery_status TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE reported_subjects (
			profile_id TEXT PRIMARY KEY,
			source_scope TEXT NOT NULL REFERENCES report_sources(source_scope),
			reference_class TEXT NOT NULL CHECK(reference_class IN ('reported_device','address','event','legacy')),
			identity_namespace TEXT NOT NULL DEFAULT '',
			upstream_reference TEXT NOT NULL,
			reported_identity_source TEXT NOT NULL DEFAULT '',
			reported_confidence TEXT NOT NULL DEFAULT '',
			association_status TEXT NOT NULL DEFAULT 'active',
			operator_label TEXT,
			first_report_at INTEGER NOT NULL,
			last_report_at INTEGER NOT NULL,
			UNIQUE(source_scope, reference_class, identity_namespace, upstream_reference)
		)`,
		`CREATE INDEX reported_subjects_scope ON reported_subjects(source_scope, last_report_at)`,
		`CREATE TABLE reported_observations (
			observation_id INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL REFERENCES raw_events(event_id),
			view_role TEXT NOT NULL CHECK(view_role IN ('host','previous','current')),
			profile_id TEXT NOT NULL REFERENCES reported_subjects(profile_id),
			reported_at INTEGER NOT NULL,
			received_at INTEGER NOT NULL,
			attributes_json TEXT NOT NULL DEFAULT '{}',
			UNIQUE(event_id, view_role)
		)`,
		`CREATE INDEX reported_observations_event ON reported_observations(event_id)`,
		`CREATE INDEX reported_observations_profile ON reported_observations(profile_id, reported_at)`,
		`CREATE TABLE reported_ports (
			observation_id INTEGER NOT NULL REFERENCES reported_observations(observation_id) ON DELETE CASCADE,
			proto TEXT NOT NULL,
			port INTEGER NOT NULL,
			reported_state TEXT NOT NULL DEFAULT '',
			reported_service TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(observation_id, proto, port)
		)`,
		`CREATE TABLE group_subjects (
			group_id TEXT NOT NULL REFERENCES active_groups(group_id) ON DELETE CASCADE,
			profile_id TEXT NOT NULL REFERENCES reported_subjects(profile_id),
			role TEXT NOT NULL,
			PRIMARY KEY(group_id, profile_id, role)
		)`,
		`CREATE INDEX group_subjects_group ON group_subjects(group_id)`,
		`CREATE TABLE briefing_subjects (
			briefing_id TEXT NOT NULL REFERENCES briefings(briefing_id) ON DELETE CASCADE,
			profile_id TEXT NOT NULL REFERENCES reported_subjects(profile_id),
			role TEXT NOT NULL,
			PRIMARY KEY(briefing_id, profile_id, role)
		)`,
		`CREATE INDEX briefing_subjects_briefing ON briefing_subjects(briefing_id)`,
		`CREATE TABLE projection_progress (
			event_id TEXT PRIMARY KEY REFERENCES raw_events(event_id) ON DELETE CASCADE,
			projection_version INTEGER NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('pending','done'))
		)`,
		`CREATE INDEX projection_progress_state ON projection_progress(state)`,
		`CREATE TABLE legacy_profile_state (
			old_device_id TEXT PRIMARY KEY,
			association_status TEXT NOT NULL DEFAULT '',
			operator_label TEXT,
			split_metadata TEXT NOT NULL DEFAULT ''
		)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateV11: source ownership and bounded projection discovery.
//
// source_scope records which configured scanner deployment accepted
// each event, in the same transaction as the event itself. Rows
// predating scopes keep ” and are adopted once by the single
// configured scope at projection; rows carrying any other concrete
// scope fail the startup binding check instead of being reattributed.
//
// projection_pending is the indexed pending-work queue. The projector
// drains it in received order instead of anti-joining retention on
// every tick, so steady state costs one small indexed read whether
// ten events or ten million are retained. The migration backfills it
// from retained events, so pre-projection databases resume without a
// full-history scan. It lives here rather than in v10 so dev
// databases created at v10 replay this step instead of silently
// missing the queue table.
func migrateV11(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE raw_events ADD COLUMN source_scope TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	stmts := []string{
		`CREATE TABLE projection_pending (
			event_id TEXT PRIMARY KEY REFERENCES raw_events(event_id) ON DELETE CASCADE,
			received_at INTEGER NOT NULL
		)`,
		`CREATE INDEX projection_pending_received ON projection_pending(received_at)`,
		`INSERT INTO projection_pending (event_id, received_at)
			SELECT event_id, received_at FROM raw_events`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}
