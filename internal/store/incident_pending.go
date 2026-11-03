package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

func (s *Store) incidentPinVersion(ctx context.Context) (int64, error) {
	var version int64
	err := s.db.QueryRowContext(ctx, `SELECT pin_version FROM incident_progress WHERE id=0`).Scan(&version)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return version, err
}

// Pending work is scoped before it enters the queue. Unrelated or thin
// keys cannot occupy a global reconsideration prefix forever.
type pendingEvidence struct {
	Kind, Source, Target, Revision string
	Refs                           []MemberRef
}

func pendingKey(c classifiedBriefing) string {
	b, _ := json.Marshal([]string{c.kind, c.source, c.target, c.revision, c.refs[0].Namespace})
	return string(b)
}

func enqueueIncidentCandidate(tx *sql.Tx, c classifiedBriefing) error {
	key := pendingKey(c)
	data, err := json.Marshal(pendingEvidence{c.kind, c.source, c.target, c.revision, c.refs})
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO incident_pending(briefing_id,correlation_key,created_at,data_json)
		SELECT ?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM incident_members WHERE briefing_id=?)`,
		c.briefingID, key, nanos(c.createdAt), string(data), c.briefingID)
	if err != nil {
		return err
	}
	for _, r := range c.refs {
		_, err = tx.Exec(`INSERT INTO incident_pending_refs(correlation_key,reference,briefing_id,created_at)
			SELECT ?,?,?,? WHERE EXISTS(SELECT 1 FROM incident_pending WHERE briefing_id=?)
			ON CONFLICT(correlation_key,reference) DO UPDATE SET briefing_id=excluded.briefing_id,created_at=excluded.created_at
			WHERE excluded.created_at >= incident_pending_refs.created_at`, key, r.Reference, c.briefingID, nanos(c.createdAt), c.briefingID)
		if err != nil {
			return err
		}
	}
	return nil
}

func deletePendingCandidate(tx *sql.Tx, id string) error {
	if _, err := tx.Exec(`DELETE FROM incident_pending_refs WHERE briefing_id=?`, id); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM incident_pending WHERE briefing_id=?`, id)
	return err
}

func pendingKeys(tx *sql.Tx) ([]string, error) {
	var after string
	if err := tx.QueryRow(`SELECT pending_after FROM incident_progress WHERE id=0`).Scan(&after); err != nil {
		return nil, err
	}
	read := func(after string) ([]string, error) {
		var keys []string
		// Seek past each key rather than scanning all duplicate rows to
		// compute DISTINCT. A thin key can have arbitrarily many reports.
		for len(keys) < 50 {
			var key string
			err := tx.QueryRow(`SELECT correlation_key FROM incident_pending WHERE correlation_key>? ORDER BY correlation_key LIMIT 1`, after).Scan(&key)
			if err == sql.ErrNoRows {
				break
			}
			if err != nil {
				return nil, err
			}
			keys = append(keys, key)
			after = key
		}
		return keys, nil
	}
	keys, err := read(after)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 && after != "" {
		keys, err = read("")
	}
	if err != nil {
		return nil, err
	}
	if len(keys) > 0 {
		_, err = tx.Exec(`UPDATE incident_progress SET pending_after=? WHERE id=0`, keys[len(keys)-1])
	}
	return keys, err
}

func readPending(tx *sql.Tx, key string, from, through time.Time, limit int) ([]classifiedBriefing, error) {
	rows, err := tx.Query(`SELECT p.briefing_id,b.group_id,b.priority,p.created_at,p.data_json
		FROM incident_pending p JOIN briefings b ON b.briefing_id=p.briefing_id
		WHERE p.correlation_key=? AND p.created_at>=? AND p.created_at<=?
		ORDER BY p.created_at,p.briefing_id LIMIT ?`, key, nanos(from), nanos(through), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []classifiedBriefing
	for rows.Next() {
		var c classifiedBriefing
		var at int64
		var raw string
		if err := rows.Scan(&c.briefingID, &c.groupID, &c.priority, &at, &raw); err != nil {
			return nil, err
		}
		var p pendingEvidence
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		c.createdAt = fromNanos(at)
		c.kind, c.source, c.target, c.revision = p.Kind, p.Source, p.Target, p.Revision
		c.refs = p.Refs
		for _, r := range p.Refs {
			c.references = append(c.references, r.Reference)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func pendingSourceQualified(tx *sql.Tx, c classifiedBriefing) (bool, error) {
	var ns, caps string
	err := tx.QueryRow(`SELECT configured_identity_namespace,capabilities FROM report_sources WHERE source_scope=?`, c.source).Scan(&ns, &caps)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ns == "" || !HasCapability(caps, StableIdentityCapability) {
		return false, nil
	}
	for _, r := range c.refs {
		if r.Namespace != ns {
			return false, nil
		}
	}
	return true, nil
}

// formationCandidates selects recent distinct-reference representatives
// through an index, then fills the bounded formation with other reports.
// Repeated judgments on one reference cannot hide other references.
func formationCandidates(tx *sql.Tx, key string, settings IncidentSettings, now time.Time, limit int) ([]classifiedBriefing, error) {
	if limit < settings.MinRefs {
		return nil, nil
	}
	rows, err := tx.Query(`SELECT briefing_id,created_at FROM incident_pending_refs
		WHERE correlation_key=? AND created_at>=? AND created_at<=? ORDER BY created_at DESC LIMIT ?`, key, nanos(now.Add(-settings.Window-settings.Quiet)), nanos(now), settings.MinRefs)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	var newest time.Time
	var oldest time.Time
	count := 0
	for rows.Next() {
		var id string
		var at int64
		if err := rows.Scan(&id, &at); err != nil {
			rows.Close()
			return nil, err
		}
		t := fromNanos(at)
		if count == 0 {
			newest = t
		}
		oldest = t
		ids[id] = true
		count++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if count < settings.MinRefs || newest.Sub(oldest) > settings.Window {
		return nil, nil
	}
	// Representatives may be later than the first 500 repeated reports.
	// Include them explicitly before filling remaining formation capacity.
	var out []classifiedBriefing
	for id := range ids {
		var raw string
		var c classifiedBriefing
		var at int64
		if err := tx.QueryRow(`SELECT p.briefing_id,b.group_id,b.priority,p.created_at,p.data_json FROM incident_pending p JOIN briefings b ON b.briefing_id=p.briefing_id WHERE p.briefing_id=?`, id).Scan(&c.briefingID, &c.groupID, &c.priority, &at, &raw); err != nil {
			return nil, err
		}
		var p pendingEvidence
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		c.createdAt = fromNanos(at)
		c.kind, c.source, c.target, c.revision = p.Kind, p.Source, p.Target, p.Revision
		c.refs = p.Refs
		for _, r := range p.Refs {
			c.references = append(c.references, r.Reference)
		}
		out = append(out, c)
	}
	fill, err := readPending(tx, key, newest.Add(-settings.Window), newest, limit-len(out))
	if err != nil {
		return nil, err
	}
	for _, c := range fill {
		if !ids[c.briefingID] {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].createdAt.Equal(out[j].createdAt) {
			return out[i].briefingID < out[j].briefingID
		}
		return out[i].createdAt.Before(out[j].createdAt)
	})
	return out, nil
}

func insertMemberReferences(tx *sql.Tx, id string, refs []MemberRef) (int, error) {
	added := 0
	for _, r := range refs {
		res, err := tx.Exec(`INSERT OR IGNORE INTO incident_references(incident_id,reference) VALUES (?,?)`, id, r.Reference)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		added += int(n)
	}
	return added, nil
}

// Populate the reference index for pre-v27 memberships in bounded writer
// work. New membership is deferred until this durable upgrade work completes.
func backfillIncidentReferences(tx *sql.Tx) (bool, error) {
	var cursor, through int64
	if err := tx.QueryRow(`SELECT refs_cursor,refs_through FROM incident_progress WHERE id=0`).Scan(&cursor, &through); err != nil {
		return false, err
	}
	if cursor >= through {
		return true, nil
	}
	rows, err := tx.Query(`SELECT rowid,incident_id,refs_json FROM incident_members WHERE rowid>? AND rowid<=? ORDER BY rowid LIMIT 16`, cursor, through)
	if err != nil {
		return false, err
	}
	type entry struct {
		id       int64
		incident string
		refs     []MemberRef
	}
	var entries []entry
	for rows.Next() {
		var e entry
		var raw string
		if err := rows.Scan(&e.id, &e.incident, &raw); err != nil {
			rows.Close()
			return false, err
		}
		e.refs, err = parseMemberRefs(raw)
		if err != nil {
			rows.Close()
			return false, err
		}
		if len(e.refs) > 512 {
			rows.Close()
			return false, fmt.Errorf("incident reference snapshot exceeds bound")
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	if len(entries) == 0 {
		cursor = through
	}
	writes := 0
	for _, e := range entries {
		if writes+len(e.refs) > 512 {
			break
		}
		if _, err := insertMemberReferences(tx, e.incident, e.refs); err != nil {
			return false, err
		}
		if len(e.refs) > 0 {
			if _, err := tx.Exec(`UPDATE incidents SET identity_namespace=? WHERE incident_id=? AND identity_namespace=''`, e.refs[0].Namespace, e.incident); err != nil {
				return false, err
			}
		}
		writes += len(e.refs)
		cursor = e.id
	}
	_, err = tx.Exec(`UPDATE incident_progress SET refs_cursor=? WHERE id=0`, cursor)
	return cursor >= through, err
}
