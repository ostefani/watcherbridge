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

const supportedSchemaVersion = 8

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > supportedSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported %d; upgrade the binary", version, supportedSchemaVersion)
	}
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
		}
		if err != nil {
			return err
		}
		version++
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
				(SELECT COUNT(*) FROM briefings b2 WHERE (b2.created_at, b2.rowid) <= (briefings.created_at, briefings.rowid))
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
