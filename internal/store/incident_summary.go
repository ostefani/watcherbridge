package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Closed-incident summaries (Plan incident-summary addition,
// Designv9 §7). A summary is generated once after an incident closes,
// from its fixed membership, using briefing.Resolve. The deterministic
// incident stays usable throughout; the summary never changes
// membership, reopens incidents, modifies decisions, or publishes
// scanner annotations.
//
// Queue: one row per closed incident; pending+running rows are the
// outstanding queue capped at 100. Enabling applies to subsequent
// closures only: close paths enqueue, and history is never backfilled
// automatically. When capacity is exhausted the close records
// skipped_overload and keeps deterministic presentation.
//
// Claim: running rows carry an opaque claim token and a lease expiry.
// Send reservations and received usage are durable independently of
// completion. The first terminal outcome committed by the matching
// claim and snapshot wins.
const (
	// maxSummaryQueue caps outstanding summary jobs. The close that
	// would exceed it records skipped_overload instead of blocking
	// correlation, ingestion or briefing work.
	maxSummaryQueue = 100
	// summaryLease bounds one claimed run before another worker may
	// reclaim it. It sits under the 30s provider run deadline
	// sharing: a crashed worker's lease lapses and the next poll
	// reclaims the job for a conservative fallback.
	summaryLease = 60 * time.Second
	// maxSummaryMembers bounds the evidence snapshot: at most 20
	// member briefings, selected by a documented deterministic policy
	// (see SummaryMembers).
	maxSummaryMembers = 20
)

// IncidentSummary is one durable summary job and its outcome. Floor,
// Suggestion, FinalPriority and Clamped record the resolver inputs
// and outcome separately; the incidents row keeps the deterministic
// floor and is never rewritten from a model answer. Suggestion is nil
// when the model abstains. Usage rides only when the adapter supplies
// it; nil reads as unknown, never as a free call. SnapshotJSON is the
// exact provider-bound input; AliasMapJSON the opaque citation
// aliases issued for that snapshot; CitationsJSON the validated
// returned citations.
type IncidentSummary struct {
	IncidentID    string
	Status        string // pending|running|model|fallback|skipped
	Claim         string
	LeaseExpires  time.Time
	Attempts      int
	SendsReserved int
	Floor         string
	Suggestion    *string
	FinalPriority string
	Clamped       bool
	Summary       string
	Hypothesis    string
	Confidence    string
	Reason        string
	Citations     []SummaryCitation
	SnapshotJSON  string
	AliasMap      map[string]string
	Sampled       int
	Omitted       int
	InputTokens   *int64
	OutputTokens  *int64
	CostMicros    *int64
	Error         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// SummaryCitation is one validated returned citation: the opaque
// member alias issued for the snapshot. The alias maps to a member
// briefing ID in AliasMap; raw briefing or event IDs never cross.
type SummaryCitation struct {
	Member string `json:"member"`
}

func scanIncidentSummary(row interface {
	Scan(dest ...any) error
}, out *IncidentSummary) error {
	var suggestion, confidence sql.NullString
	var inputTokens, outputTokens, costMicros sql.NullInt64
	var lease int64
	var created, updated int64
	var citations, snapshot, aliasMap string
	if err := row.Scan(&out.IncidentID, &out.Status, &out.Claim, &lease, &out.Attempts, &out.SendsReserved,
		&out.Floor, &suggestion, &out.FinalPriority, &out.Clamped,
		&out.Summary, &out.Hypothesis, &confidence, &out.Reason,
		&citations, &snapshot, &aliasMap, &out.Sampled, &out.Omitted,
		&inputTokens, &outputTokens, &costMicros, &out.Error, &created, &updated); err != nil {
		return err
	}
	if suggestion.Valid {
		out.Suggestion = &suggestion.String
	}
	out.Confidence = confidence.String
	if citations != "" && citations != "[]" {
		var cits []SummaryCitation
		if err := json.Unmarshal([]byte(citations), &cits); err != nil {
			return err
		}
		out.Citations = cits
	}
	out.SnapshotJSON = snapshot
	out.AliasMap = map[string]string{}
	if aliasMap != "" && aliasMap != "{}" {
		if err := json.Unmarshal([]byte(aliasMap), &out.AliasMap); err != nil {
			return err
		}
	}
	if inputTokens.Valid {
		out.InputTokens = &inputTokens.Int64
	}
	if outputTokens.Valid {
		out.OutputTokens = &outputTokens.Int64
	}
	if costMicros.Valid {
		out.CostMicros = &costMicros.Int64
	}
	out.LeaseExpires = fromNanos(lease)
	out.CreatedAt, out.UpdatedAt = fromNanos(created), fromNanos(updated)
	return nil
}

func newSummaryClaim() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// enqueueIncidentSummary records summary eligibility atomically with
// incident closure (same transaction). When the outstanding queue
// (pending+running) is at capacity the close records skipped_overload
// and keeps deterministic presentation; correlation never blocks.
func enqueueIncidentSummary(tx *sql.Tx, incidentID, floor string, now time.Time) error {
	var outstanding int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM incident_summaries WHERE status IN ('pending','running')`).Scan(&outstanding); err != nil {
		return err
	}
	if outstanding >= maxSummaryQueue {
		_, err := tx.Exec(`INSERT OR IGNORE INTO incident_summaries
			(incident_id, status, floor, final_priority, error, created_at, updated_at)
			VALUES (?, 'skipped', ?, ?, 'skipped_overload', ?, ?)`,
			incidentID, floor, floor, nanos(now), nanos(now))
		return err
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO incident_summaries
		(incident_id, status, floor, final_priority, created_at, updated_at)
		VALUES (?, 'pending', ?, ?, ?, ?)`,
		incidentID, floor, floor, nanos(now), nanos(now))
	return err
}

// SummaryOutstanding counts pending+running jobs. Bounded single-row
// read for the worker and tests.
func (s *Store) SummaryOutstanding(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM incident_summaries WHERE status IN ('pending','running')`).Scan(&n)
	return n, err
}

// GetIncidentSummary reads one incident's summary row, or sql.ErrNoRows.
func (s *Store) GetIncidentSummary(ctx context.Context, incidentID string) (IncidentSummary, error) {
	var out IncidentSummary
	row := s.db.QueryRowContext(ctx, `SELECT incident_id, status, claim, lease_expires, attempts, sends_reserved,
		floor, suggestion, final_priority, clamped, summary, hypothesis, confidence, reason,
		citations_json, snapshot_json, alias_map_json, sampled, omitted,
		input_tokens, output_tokens, cost_micros, error, created_at, updated_at
		FROM incident_summaries WHERE incident_id = ?`, incidentID)
	if err := scanIncidentSummary(row, &out); err != nil {
		return IncidentSummary{}, err
	}
	return out, nil
}

// ClaimIncidentSummary atomically claims the oldest pending job, or
// reclaims an expired running lease. It preserves the exact snapshot,
// send reservations and received usage across claims. The worker uses
// any prior send reservation to settle uncertain dispatched work to
// deterministic fallback instead of repeating an external call.
func (s *Store) ClaimIncidentSummary(ctx context.Context, now time.Time) (IncidentSummary, string, bool, error) {
	var out IncidentSummary
	var claim string
	err := s.write(ctx, func(tx *sql.Tx) error {
		var id, floor string
		err := tx.QueryRow(`SELECT incident_id, floor FROM incident_summaries
			WHERE status = 'pending' OR (status = 'running' AND lease_expires <= ?)
			ORDER BY updated_at ASC, incident_id ASC LIMIT 1`, nanos(now)).Scan(&id, &floor)
		if err == sql.ErrNoRows {
			return sql.ErrNoRows
		}
		if err != nil {
			return err
		}
		c, err := newSummaryClaim()
		if err != nil {
			return err
		}
		claim = c
		res, err := tx.Exec(`UPDATE incident_summaries SET status = 'running', claim = ?,
			lease_expires = ?, attempts = attempts + 1,
			updated_at = ? WHERE incident_id = ? AND (status = 'pending' OR (status = 'running' AND lease_expires <= ?))`,
			claim, nanos(now.Add(summaryLease)), nanos(now), id, nanos(now))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return sql.ErrNoRows
		}
		row := tx.QueryRow(`SELECT incident_id, status, claim, lease_expires, attempts, sends_reserved,
			floor, suggestion, final_priority, clamped, summary, hypothesis, confidence, reason,
			citations_json, snapshot_json, alias_map_json, sampled, omitted,
			input_tokens, output_tokens, cost_micros, error, created_at, updated_at
			FROM incident_summaries WHERE incident_id = ?`, id)
		return scanIncidentSummary(row, &out)
	})
	if err == sql.ErrNoRows {
		return IncidentSummary{}, "", false, nil
	}
	if err != nil {
		return IncidentSummary{}, "", false, err
	}
	return out, claim, true, nil
}

// ReserveSummarySend durably consumes one of the incident's two provider
// sends before dispatch. It is guarded by the active claim.
func (s *Store) ReserveSummarySend(ctx context.Context, incidentID, claim string, now time.Time) (bool, error) {
	var reserved bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE incident_summaries SET sends_reserved=sends_reserved+1, updated_at=?
			WHERE incident_id=? AND claim=? AND status='running' AND sends_reserved<2`, nanos(now), incidentID, claim)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		reserved = n == 1
		return err
	})
	return reserved, err
}

// RecordSummaryUsage persists accounting independently of terminal completion.
func (s *Store) RecordSummaryUsage(ctx context.Context, incidentID, claim string, input, output int64, hasUsage bool, now time.Time) error {
	if !hasUsage {
		return nil
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE incident_summaries SET input_tokens=COALESCE(input_tokens,0)+?, output_tokens=COALESCE(output_tokens,0)+?, updated_at=? WHERE incident_id=? AND claim=? AND status='running'`, input, output, nanos(now), incidentID, claim)
		return err
	})
}

// ReserveSummarySnapshot stores the exact provider-bound snapshot and
// alias map against the matching claim while still running. A crash
// after this point leaves the snapshot durable, so validation can
// reproduce after restart.
func (s *Store) ReserveSummarySnapshot(ctx context.Context, incidentID, claim, snapshotJSON, aliasMapJSON string, sampled, omitted int, now time.Time) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE incident_summaries SET snapshot_json = ?, alias_map_json = ?,
			sampled = ?, omitted = ?, updated_at = ?
			WHERE incident_id = ? AND claim = ? AND status = 'running'`,
			snapshotJSON, aliasMapJSON, sampled, omitted, nanos(now), incidentID, claim)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("summary %s is not running under this claim", incidentID)
		}
		return nil
	})
}

// CompleteIncidentSummary commits an accepted outcome only against the
// matching claim and snapshot. First committed terminal outcome wins:
// rows already terminal are left untouched and the late completion
// reports committed=false.
func (s *Store) CompleteIncidentSummary(ctx context.Context, incidentID, claim, snapshotJSON string, outcome IncidentSummary, now time.Time) (bool, error) {
	var committed bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		var status, snapshot string
		if err := tx.QueryRow(`SELECT status, snapshot_json FROM incident_summaries WHERE incident_id = ?`,
			incidentID).Scan(&status, &snapshot); err != nil {
			return err
		}
		if status != "running" {
			committed = false
			return nil
		}
		var claimOK string
		if err := tx.QueryRow(`SELECT claim FROM incident_summaries WHERE incident_id = ? AND claim = ?`,
			incidentID, claim).Scan(&claimOK); err == sql.ErrNoRows {
			committed = false
			return nil
		} else if err != nil {
			return err
		}
		if snapshot != snapshotJSON {
			committed = false
			return nil
		}
		citations, err := json.Marshal(outcome.Citations)
		if err != nil {
			return err
		}
		var suggestion any
		if outcome.Suggestion != nil {
			suggestion = *outcome.Suggestion
		}
		var in, out, cost any
		if outcome.InputTokens != nil {
			in = *outcome.InputTokens
		}
		if outcome.OutputTokens != nil {
			out = *outcome.OutputTokens
		}
		if outcome.CostMicros != nil {
			cost = *outcome.CostMicros
		}
		res, err := tx.Exec(`UPDATE incident_summaries SET status = ?, suggestion = ?,
			final_priority = ?, clamped = ?, summary = ?, hypothesis = ?, confidence = ?,
			reason = ?, citations_json = ?, cost_micros = ?, input_tokens = COALESCE(?, input_tokens), output_tokens = COALESCE(?, output_tokens), error = ?, claim = '', lease_expires = 0, updated_at = ?
			WHERE incident_id = ? AND claim = ? AND status = 'running'`,
			outcome.Status, suggestion, outcome.FinalPriority, outcome.Clamped,
			outcome.Summary, outcome.Hypothesis, outcome.Confidence, outcome.Reason,
			string(citations), cost, in, out, outcome.Error, nanos(now), incidentID, claim)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		committed = n == 1
		return nil
	})
	return committed, err
}

// SummaryMember is one briefings row behind the deterministic snapshot
// selection: priority, kind and bounded display text.
type SummaryMember struct {
	BriefingID string
	Priority   string
	Kind       string
	CreatedAt  time.Time
	What       string
	Context    string
	Check      string
	Seeds      []string
}

// SummaryMembers selects at most maxSummaryMembers member briefings by
// a documented deterministic policy: priority rank descending, then
// oldest first, then briefing ID. The first row establishes the
// incident's priority floor, so the floor member is always sampled.
// Every text field is capped (512 runes) before serialization; the
// snapshot layer enforces the 32 KiB provider-bound budget over the
// capped selection and states sampled/omitted counts explicitly.
func (s *Store) SummaryMembers(ctx context.Context, incidentID string, limit int) ([]SummaryMember, int, error) {
	if limit <= 0 || limit > maxSummaryMembers {
		limit = maxSummaryMembers
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT member_count FROM incidents WHERE incident_id = ?`,
		incidentID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT b.briefing_id, b.priority, b.kind, b.created_at,
		b.what_changed, b.context, b.recommended_check, m.refs_json
		FROM incident_members m JOIN briefings b ON b.briefing_id = m.briefing_id
		WHERE m.incident_id = ?
		ORDER BY m.priority_rank DESC, m.member_created_at ASC, m.briefing_id ASC LIMIT ?`, incidentID, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []SummaryMember
	for rows.Next() {
		var m SummaryMember
		var created int64
		var refs string
		if err := rows.Scan(&m.BriefingID, &m.Priority, &m.Kind, &created, &m.What, &m.Context, &m.Check, &refs); err != nil {
			return nil, 0, err
		}
		var parsed []MemberRef
		if json.Unmarshal([]byte(refs), &parsed) != nil {
			return nil, 0, fmt.Errorf("invalid incident member references")
		}
		for _, ref := range parsed {
			m.Seeds = append(m.Seeds, ref.Reference)
		}
		seedRows, err := s.db.QueryContext(ctx, `SELECT o.attributes_json FROM briefing_subjects bs
			JOIN reported_observations o ON o.profile_id=bs.profile_id
			WHERE bs.briefing_id=? ORDER BY o.reported_at DESC, o.observation_id DESC LIMIT 201`, m.BriefingID)
		if err != nil {
			return nil, 0, err
		}
		observationCount := 0
		for seedRows.Next() {
			observationCount++
			if observationCount > 200 {
				seedRows.Close()
				return nil, 0, fmt.Errorf("retained summary observation metadata exceeds redaction bounds")
			}
			var attrs string
			if err := seedRows.Scan(&attrs); err != nil {
				seedRows.Close()
				return nil, 0, err
			}
			var values any
			if json.Unmarshal([]byte(attrs), &values) != nil {
				seedRows.Close()
				return nil, 0, fmt.Errorf("invalid retained summary metadata")
			}
			if err := collectSummarySeeds(values, "", &m.Seeds); err != nil {
				seedRows.Close()
				return nil, 0, err
			}
		}
		if err := seedRows.Err(); err != nil {
			seedRows.Close()
			return nil, 0, err
		}
		seedRows.Close()
		if len(m.Seeds) > 200 {
			m.Seeds = m.Seeds[:200]
		}
		m.CreatedAt = fromNanos(created)
		m.What = capSummaryRunes(m.What, 512)
		m.Context = capSummaryRunes(m.Context, 512)
		m.Check = capSummaryRunes(m.Check, 512)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	omitted := total - len(out)
	if omitted < 0 {
		omitted = 0
	}
	return out, omitted, nil
}

func collectSummarySeeds(v any, key string, out *[]string) error {
	if len(*out) >= 200 {
		return fmt.Errorf("retained summary metadata exceeds redaction seed bound")
	}
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			if err := collectSummarySeeds(value, strings.ToLower(k), out); err != nil {
				return err
			}
		}
	case []any:
		for _, value := range x {
			if err := collectSummarySeeds(value, key, out); err != nil {
				return err
			}
		}
	case string:
		switch key {
		case "hostname", "name", "description", "device_type", "vendor", "identity_aliases", "aliases", "reported_id", "reference", "identity":
			if x != "" {
				if utf8.RuneCountInString(x) > 256 {
					return fmt.Errorf("retained summary identifier exceeds redaction bound")
				}
				*out = append(*out, x)
			}
		}
	}
	return nil
}

// MaxSummaryMembersForSnapshot exposes the bounded evidence snapshot
// size to the summary builder without exporting the queue internals.
func MaxSummaryMembersForSnapshot() int { return maxSummaryMembers }

func capSummaryRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
