package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Review-policy rules (Designv9 §5.2, Plan Step 28). A rule automates
// one operator judgment: after RULE_PROPOSE_AFTER matching expected
// decisions, an approved rule selects low priority for later groups
// matching its exact scope — same source, namespace, reference,
// identity standing, change kind and canonical ports. It never
// overrides deterministic critical, never merges identity, and never
// suppresses events; only the priority floor changes. Address, event
// and legacy references are never eligible, and matching is byte
// equality throughout: same attributes under different references or
// sources never borrow a rule.
const (
	RuleApproved  = "approved"
	RuleDismissed = "dismissed"
	RuleRevoked   = "revoked"
)

var (
	// ErrRuleUnknown means no proposal or rule matches the ID: 404 at
	// the API. Purged rows read the same — without tombstones there is
	// no 410, only 404.
	ErrRuleUnknown = errors.New("unknown rule proposal")
	// ErrRuleDismissed means the scope was explicitly declined: approving
	// it is a 409, never silent success.
	ErrRuleDismissed = errors.New("rule proposal dismissed")
	// ErrRuleActive means the scope already carries an unexpired rule:
	// dismiss targets proposals, so declining an active rule is a 409
	// and the caller must revoke instead.
	ErrRuleActive = errors.New("rule already active")
	// ErrRuleInvalid rejects malformed rule input: 400 at the API.
	ErrRuleInvalid = errors.New("invalid rule request")
)

// Rule is one deterministic scope with its approval lifecycle. The
// scope columns are the approved snapshot: application re-matches them
// byte-for-byte against the live subject row, so a namespace change,
// identity revision or confidence drift voids the rule instead of
// applying stale trust. DecidedAt marks the last status transition and
// drives retention; ApprovedAt/ExpiresAt mark the approval grant, with
// a NULL expiry meaning no expiry.
type Rule struct {
	RuleID                 string
	ProposalID             string
	SourceScope            string
	ReferenceClass         string
	IdentityNamespace      string
	UpstreamReference      string
	ReportedIdentitySource string
	ReportedConfidence     string
	Kind                   string
	PortsCSV               string
	Status                 string
	DecidedAt              time.Time
	ApprovedAt             time.Time
	HasApprovedAt          bool
	ExpiresAt              time.Time
	HasExpiresAt           bool
}

const ruleColumns = `rule_id, proposal_id, source_scope, reference_class, identity_namespace,
	upstream_reference, reported_identity_source, reported_confidence, kind, ports_csv,
	status, decided_at, approved_at, expires_at`

func scanRule(row interface {
	Scan(dest ...any) error
}, r *Rule) error {
	var decided int64
	var approved, expires sql.NullInt64
	if err := row.Scan(&r.RuleID, &r.ProposalID, &r.SourceScope, &r.ReferenceClass, &r.IdentityNamespace,
		&r.UpstreamReference, &r.ReportedIdentitySource, &r.ReportedConfidence, &r.Kind, &r.PortsCSV,
		&r.Status, &decided, &approved, &expires); err != nil {
		return err
	}
	r.DecidedAt = fromNanos(decided)
	if approved.Valid {
		r.ApprovedAt, r.HasApprovedAt = fromNanos(approved.Int64), true
	}
	if expires.Valid {
		r.ExpiresAt, r.HasExpiresAt = fromNanos(expires.Int64), true
	}
	return nil
}

// Expired reports whether an approved grant has lapsed as of now. Only
// approved rows expire; terminal states are timeless until purged.
func (r Rule) Expired(now time.Time) bool {
	return r.Status == RuleApproved && r.HasExpiresAt && !r.ExpiresAt.After(now)
}

// GetRule reads one rule row by rule ID, or sql.ErrNoRows.
func (s *Store) GetRule(ctx context.Context, ruleID string) (Rule, error) {
	var r Rule
	if ruleID == "" {
		return r, fmt.Errorf("empty rule id")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM review_rules WHERE rule_id = ?`, ruleID)
	if err := scanRule(row, &r); err != nil {
		return Rule{}, err
	}
	return r, nil
}

// GetRuleByProposal reads one rule row by proposal ID, or sql.ErrNoRows
// when the scope was never declined, approved or revoked.
func (s *Store) GetRuleByProposal(ctx context.Context, proposalID string) (Rule, error) {
	var r Rule
	if proposalID == "" {
		return r, fmt.Errorf("empty proposal id")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM review_rules WHERE proposal_id = ?`, proposalID)
	if err := scanRule(row, &r); err != nil {
		return Rule{}, err
	}
	return r, nil
}

// PutRule inserts a rule row or transitions an existing scope's status.
// Scope columns ride the insert only: the IDs are the scope hash, so an
// existing row already names the same scope and only its lifecycle
// advances. Timestamps travel as nanos; NULL expiry means no expiry.
func (s *Store) PutRule(ctx context.Context, r Rule) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var approved, expires any
		if r.HasApprovedAt {
			approved = nanos(r.ApprovedAt)
		}
		if r.HasExpiresAt {
			expires = nanos(r.ExpiresAt)
		}
		_, err := tx.Exec(`INSERT INTO review_rules (rule_id, proposal_id, source_scope, reference_class,
			identity_namespace, upstream_reference, reported_identity_source, reported_confidence,
			kind, ports_csv, status, decided_at, approved_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(rule_id) DO UPDATE SET status = excluded.status, decided_at = excluded.decided_at,
			approved_at = excluded.approved_at, expires_at = excluded.expires_at`,
			r.RuleID, r.ProposalID, r.SourceScope, r.ReferenceClass, r.IdentityNamespace,
			r.UpstreamReference, r.ReportedIdentitySource, r.ReportedConfidence, r.Kind, r.PortsCSV,
			r.Status, nanos(r.DecidedAt), approved, expires)
		return err
	})
}

// MatchRule finds the approved, unexpired rule for one exact scope, or
// reports false. Every column compares by byte equality and the grant
// must cover now: drifted, expired or terminal scopes simply miss, so
// application fails closed without a separate void check.
func (s *Store) MatchRule(ctx context.Context, sourceScope, referenceClass, namespace, reference,
	idSource, confidence, kind, portsCSV string, now time.Time) (Rule, bool, error) {
	var r Rule
	row := s.db.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM review_rules
		WHERE source_scope = ? AND reference_class = ? AND identity_namespace = ? AND upstream_reference = ?
		AND reported_identity_source = ? AND reported_confidence = ? AND kind = ? AND ports_csv = ?
		AND status = 'approved' AND (expires_at IS NULL OR expires_at > ?)`,
		sourceScope, referenceClass, namespace, reference, idSource, confidence, kind, portsCSV, nanos(now))
	if err := scanRule(row, &r); err != nil {
		if err == sql.ErrNoRows {
			return Rule{}, false, nil
		}
		return Rule{}, false, err
	}
	return r, true, nil
}

// BriefingByID reads one briefing by its briefing ID, or
// sql.ErrNoRows. Proposal derivation walks decisions addressed at
// briefing IDs, not groups.
func (s *Store) BriefingByID(ctx context.Context, briefingID string) (Briefing, error) {
	var b Briefing
	if briefingID == "" {
		return b, fmt.Errorf("empty briefing id")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+briefingColumns+` FROM briefings WHERE briefing_id = ?`, briefingID)
	if err := scanBriefing(row, &b); err != nil {
		return Briefing{}, err
	}
	return b, nil
}

// ExpectedDecision is one live input to proposal derivation: an
// expected decision addressed at a briefing, in storage order.
type ExpectedDecision struct {
	RowID      int64
	DecisionID string
	BriefingID string
	CreatedAt  time.Time
}

// maxExpectedDecisionPage bounds one derivation scan page: complete
// collection over bounded pages, never an unbounded read. Derivation
// runs on operator-triggered routes, never on ingest.
const maxExpectedDecisionPage = 500

// ExpectedBriefingDecisions pages expected decisions on briefings in
// storage order. afterRowID selects rows strictly after it; zero starts
// from the oldest. Undo filtering happens at grouping time (a decision
// with a later undo is dead evidence), so the scan stays a plain
// bounded range.
func (s *Store) ExpectedBriefingDecisions(ctx context.Context, afterRowID int64, limit int) ([]ExpectedDecision, error) {
	if afterRowID < 0 {
		return nil, fmt.Errorf("negative decision cursor")
	}
	if limit <= 0 || limit > maxExpectedDecisionPage {
		limit = maxExpectedDecisionPage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rowid, decision_id, subject_id, created_at
		FROM operator_decisions
		WHERE kind = 'expected' AND subject_kind = 'briefing' AND rowid > ?
		ORDER BY rowid ASC LIMIT ?`, afterRowID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExpectedDecision
	for rows.Next() {
		var d ExpectedDecision
		var created int64
		if err := rows.Scan(&d.RowID, &d.DecisionID, &d.BriefingID, &created); err != nil {
			return nil, err
		}
		d.CreatedAt = fromNanos(created)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// UndoneDecisions reports which decision IDs carry an undo record: dead
// evidence the proposal count must skip.
func (s *Store) UndoneDecisions(ctx context.Context, decisionIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(decisionIDs) == 0 {
		return out, nil
	}
	for start := 0; start < len(decisionIDs); start += maxExpectedDecisionPage {
		end := start + maxExpectedDecisionPage
		if end > len(decisionIDs) {
			end = len(decisionIDs)
		}
		placeholders := ""
		args := make([]any, 0, end-start)
		for _, id := range decisionIDs[start:end] {
			if placeholders != "" {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT supersedes FROM operator_decisions
			WHERE kind = 'undo' AND supersedes IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// GroupPayload is one retained group event with its original bytes for
// scope derivation: the same bytes the briefing path decoded, so both
// sides derive identical port sets.
type GroupPayload struct {
	EventID   string
	EventType string
	Payload   []byte
}

// GroupPayloads reads one group's retained event payloads in take
// order. An empty group (or purged payloads) reads empty: derivation
// fails closed rather than guessing the scope.
func (s *Store) GroupPayloads(ctx context.Context, groupID string) ([]GroupPayload, error) {
	if groupID == "" {
		return nil, fmt.Errorf("empty group id")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.event_id, e.event_type, e.payload
		FROM group_events ge JOIN raw_events e ON e.event_id = ge.event_id
		WHERE ge.group_id = ? ORDER BY e.received_at, e.event_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupPayload
	for rows.Next() {
		var p GroupPayload
		var payload []byte
		if err := rows.Scan(&p.EventID, &p.EventType, &payload); err != nil {
			return nil, err
		}
		p.Payload = append([]byte(nil), payload...)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
