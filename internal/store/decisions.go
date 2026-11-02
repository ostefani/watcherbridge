package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Operator decisions (Designv9 §5.1, Plan Step 25). Decisions are
// local, immutable audit rows; undo appends an explicit undo record
// instead of mutating history. Nothing here modifies events, scanner
// state, identity confidence or priority; labels materialize onto the
// exact profile only, never through MAC/name matching.
const (
	DecisionSubjectBriefing = "briefing"
	DecisionSubjectProfile  = "reported_subject"

	DecisionExpected    = "expected"
	DecisionNotExpected = "not_expected"
	DecisionSnooze      = "snooze"
	DecisionNote        = "note"
	DecisionLabel       = "label"
	DecisionUndo        = "undo"
)

var (
	// ErrUnsupportedSubject rejects decision kinds and subject kinds
	// outside the Step 25 matrix, and undo addressed at non-records.
	ErrUnsupportedSubject = errors.New("unsupported decision subject")
	// ErrUnknownSubject means the briefing or profile does not exist.
	ErrUnknownSubject = errors.New("unknown decision subject")
	// ErrInvalidDecision rejects malformed decision input.
	ErrInvalidDecision = errors.New("invalid decision")
	// ErrRequestConflict means a request ID was reused with different
	// content; identical replays return the original row instead.
	ErrRequestConflict = errors.New("conflicting request id")
	// ErrUnknownDecision means the undo target does not exist.
	// Purged targets read the same: without tombstones there is no
	// 410, only 404 at the API.
	ErrUnknownDecision = errors.New("unknown decision")
	// ErrInvalidUndo rejects undo addressed at another undo row.
	// Undo rows are terminal; redo by appending a fresh decision.
	ErrInvalidUndo = errors.New("invalid undo target")
	// ErrUndoConflict means the materialization head moved on: another
	// label owns the profile now, so restoring this target would
	// clobber newer state. It is a 409, never silent success.
	ErrUndoConflict = errors.New("undo conflicts with newer state")
)

const (
	maxLabelRunes = 256
	maxNoteRunes  = 2000
	maxRequestID  = 128
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Decision is one immutable audit row. Until applies to snooze only.
// Supersedes names the undo target for undo rows. PriorLabel and
// PriorOwner capture the materialization a label row replaced,
// including NULLs, atomically with its own insert — the only basis
// on which undo may restore, so restoration never depends on rows
// purge may already have deleted.
type Decision struct {
	DecisionID  string
	SubjectKind string
	SubjectID   string
	Kind        string
	Until       time.Time
	HasUntil    bool
	Text        string
	CreatedAt   time.Time
	Supersedes  string
	RequestID   string
	PriorLabel  *string
	PriorOwner  string
	Undone      bool
	// RowID is the storage order behind the newest-first trail. It
	// backs the history cursor; it never leaves the store on the
	// wire, where decision_id is the only row identity.
	RowID int64
}

// DecisionInput carries a new non-undo decision. IDs are assigned by
// the store; undo travels through UndoDecision instead.
type DecisionInput struct {
	SubjectKind string
	SubjectID   string
	Kind        string
	HasUntil    bool
	Until       time.Time
	Text        string
	RequestID   string
	// SnoozeDefault resolves a snooze without an explicit until. It is
	// specified content, never stored time: the idempotency hash covers
	// only whether an until was given, so a retry replays against the
	// original row instead of hashing a freshly resolved deadline.
	SnoozeDefault time.Duration
}

// DecisionResult reports the stored row and whether the call changed
// state. Applied=false with a reason means the valid end-state
// already held (identical replay, already-neutralized target), never
// a masked validation failure — those stay errors.
type DecisionResult struct {
	Decision Decision
	Applied  bool
	Reason   string
}

const decisionColumns = `decision_id, subject_kind, subject_id, kind, until,
	text, created_at, supersedes, request_id, content_hash, prior_label, prior_owner`

func scanDecision(row interface {
	Scan(dest ...any) error
}) (Decision, string, error) {
	var d Decision
	var until, created sql.NullInt64
	var text, supersedes, requestID, hash, priorOwner sql.NullString
	var priorLabel sql.NullString
	if err := row.Scan(&d.DecisionID, &d.SubjectKind, &d.SubjectID, &d.Kind,
		&until, &text, &created, &supersedes, &requestID, &hash, &priorLabel, &priorOwner); err != nil {
		return Decision{}, "", err
	}
	if until.Valid {
		d.Until, d.HasUntil = fromNanos(until.Int64), true
	}
	d.Text = text.String
	if created.Valid {
		d.CreatedAt = fromNanos(created.Int64)
	}
	d.Supersedes = supersedes.String
	d.RequestID = requestID.String
	if priorLabel.Valid {
		d.PriorLabel = &priorLabel.String
	}
	d.PriorOwner = priorOwner.String
	return d, hash.String, nil
}

// decisionHash fingerprints the idempotency-relevant content: a
// request-ID replay with a different hash is a conflict, not a retry.
func decisionHash(subjectKind, subjectID, kind string, until int64, hasUntil bool, text, supersedes string) string {
	untilPart := "none"
	if hasUntil {
		untilPart = fmt.Sprintf("%d", until)
	}
	sum := sha256.Sum256([]byte(subjectKind + "\x00" + subjectID + "\x00" + kind + "\x00" + untilPart + "\x00" + text + "\x00" + supersedes))
	return hex.EncodeToString(sum[:])
}

// checkDecisionMatrix enforces the Step 25 action-by-subject matrix:
// snooze and review verdicts are briefing-only (never profile-wide
// suppression or propagation), labels belong to exact profiles.
// Undo never travels through the append path.
func checkDecisionMatrix(kind, subjectKind string) error {
	switch subjectKind {
	case DecisionSubjectBriefing:
		switch kind {
		case DecisionExpected, DecisionNotExpected, DecisionSnooze, DecisionNote:
			return nil
		}
	case DecisionSubjectProfile:
		switch kind {
		case DecisionNote, DecisionLabel:
			return nil
		}
	}
	return fmt.Errorf("%w: kind %q on %q", ErrUnsupportedSubject, kind, subjectKind)
}

func checkDecisionText(kind, text string) error {
	trimmed := strings.TrimSpace(text)
	switch kind {
	case DecisionNote, DecisionLabel:
		if trimmed == "" {
			return fmt.Errorf("%w: kind %q requires text", ErrInvalidDecision, kind)
		}
		cap := maxNoteRunes
		if kind == DecisionLabel {
			cap = maxLabelRunes
		}
		if utf8.RuneCountInString(text) > cap {
			return fmt.Errorf("%w: kind %q text exceeds %d runes", ErrInvalidDecision, kind, cap)
		}
	default:
		if trimmed != "" {
			return fmt.Errorf("%w: kind %q takes no text", ErrInvalidDecision, kind)
		}
	}
	return nil
}

func checkRequestID(requestID string) error {
	if requestID == "" {
		return nil
	}
	if len(requestID) > maxRequestID || !requestIDPattern.MatchString(requestID) {
		return fmt.Errorf("%w: malformed request id", ErrInvalidDecision)
	}
	return nil
}

// findDecisionByRequest returns the stored row for an idempotency key,
// or nil when the key is new. Same key with a different content hash
// is a conflict, never a silent overwrite.
func findDecisionByRequest(tx *sql.Tx, requestID, hash string) (*Decision, error) {
	row := tx.QueryRow(`SELECT `+decisionColumns+` FROM operator_decisions WHERE request_id = ?`, requestID)
	d, stored, err := scanDecision(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if stored != hash {
		return nil, fmt.Errorf("%w: %q", ErrRequestConflict, requestID)
	}
	return &d, nil
}

// canonicalBriefingID resolves a briefing or group ID to the stored
// briefing ID, so one briefing never acquires two decision histories.
// The briefing ID (not the group ID) is the decision subject key,
// matching every read path that already speaks briefing IDs.
func canonicalBriefingID(tx *sql.Tx, id string) (string, error) {
	gid := strings.TrimPrefix(id, "brf-")
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM briefings WHERE group_id = ?`, gid).Scan(&one); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("%w: %q", ErrUnknownSubject, id)
		}
		return "", err
	}
	return "brf-" + gid, nil
}

// AppendDecision stores one non-undo decision atomically with its
// label materialization. Label text lands on the exact profile with
// the prior value and owner captured; review and note rows only
// append audit. Identical request-ID replays return the original row
// with Applied=false instead of duplicating.
func (s *Store) AppendDecision(ctx context.Context, in DecisionInput) (DecisionResult, error) {
	now := s.now()
	if err := checkRequestID(in.RequestID); err != nil {
		return DecisionResult{}, err
	}
	// The hash covers specified content only: a default snooze carries
	// no until, so every retry of the same operation hashes alike and
	// replays against the original row. The deadline resolves below,
	// after the replay check, from the same now on first commit.
	var hashUntil int64
	if in.HasUntil {
		hashUntil = in.Until.UnixNano()
	}
	hash := decisionHash(in.SubjectKind, in.SubjectID, in.Kind, hashUntil, in.HasUntil, in.Text, "")

	var res DecisionResult
	err := s.write(ctx, func(tx *sql.Tx) error {
		if in.RequestID != "" {
			if existing, err := findDecisionByRequest(tx, in.RequestID, hash); err != nil {
				return err
			} else if existing != nil {
				res = DecisionResult{Decision: *existing, Applied: false, Reason: "duplicate_request"}
				return nil
			}
		}
		if err := checkDecisionMatrix(in.Kind, in.SubjectKind); err != nil {
			return err
		}
		if err := checkDecisionText(in.Kind, in.Text); err != nil {
			return err
		}
		var untilNanos int64
		hasUntil := in.HasUntil
		if in.Kind == DecisionSnooze {
			if !hasUntil {
				if in.SnoozeDefault <= 0 {
					return fmt.Errorf("%w: snooze requires an until or a default horizon", ErrInvalidDecision)
				}
				untilNanos = now.Add(in.SnoozeDefault).UnixNano()
				hasUntil = true
			} else {
				if !in.Until.After(now) {
					return fmt.Errorf("%w: snooze requires a future until", ErrInvalidDecision)
				}
				untilNanos = in.Until.UnixNano()
			}
		} else if hasUntil {
			return fmt.Errorf("%w: kind %q takes no until", ErrInvalidDecision, in.Kind)
		}
		subjectID := in.SubjectID
		if in.SubjectKind == DecisionSubjectBriefing {
			bid, err := canonicalBriefingID(tx, in.SubjectID)
			if err != nil {
				return err
			}
			subjectID = bid
		} else {
			var label, owner sql.NullString
			if err := tx.QueryRow(`SELECT operator_label, label_owner FROM reported_subjects WHERE profile_id = ?`,
				in.SubjectID).Scan(&label, &owner); err != nil {
				if err == sql.ErrNoRows {
					return fmt.Errorf("%w: %q", ErrUnknownSubject, in.SubjectID)
				}
				return err
			}
		}
		id := fmt.Sprintf("dec-%d-%d", now.UnixNano(), s.decisionSeq.Add(1))
		var priorLabel, priorOwner sql.NullString
		if in.Kind == DecisionLabel {
			var curLabel, curOwner sql.NullString
			if err := tx.QueryRow(`SELECT operator_label, label_owner FROM reported_subjects WHERE profile_id = ?`,
				subjectID).Scan(&curLabel, &curOwner); err != nil {
				return err
			}
			priorLabel, priorOwner = curLabel, curOwner
			if _, err := tx.Exec(`UPDATE reported_subjects SET operator_label = ?, label_owner = ? WHERE profile_id = ?`,
				in.Text, id, subjectID); err != nil {
				return err
			}
		}
		var untilVal sql.NullInt64
		if in.Kind == DecisionSnooze {
			untilVal = sql.NullInt64{Int64: untilNanos, Valid: true}
		}
		if _, err := tx.Exec(`INSERT INTO operator_decisions (decision_id, subject_kind, subject_id, kind,
			until, text, created_at, supersedes, request_id, content_hash, prior_label, prior_owner)
			VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?)`,
			id, in.SubjectKind, subjectID, in.Kind, untilVal, in.Text, now.UnixNano(),
			nullString(in.RequestID), hash, priorLabel, priorOwner); err != nil {
			return err
		}
		d := Decision{DecisionID: id, SubjectKind: in.SubjectKind, SubjectID: subjectID, Kind: in.Kind,
			Text: in.Text, CreatedAt: now, RequestID: in.RequestID}
		if in.Kind == DecisionSnooze {
			d.Until, d.HasUntil = fromNanos(untilNanos), true
		}
		if priorLabel.Valid {
			d.PriorLabel = &priorLabel.String
		}
		d.PriorOwner = priorOwner.String
		res = DecisionResult{Decision: d, Applied: true}
		return nil
	})
	if err != nil {
		return DecisionResult{}, err
	}
	return res, nil
}

// reviewHead returns the effective review-lane head: the latest
// non-undone, unlapsed, audit-live expected/not_expected/snooze row.
// Ties break by storage order, the same insertion order the audit
// trail pages by, so history and effective state never disagree
// about the newest decision.
func reviewHead(tx *sql.Tx, subjectKind, subjectID string, now time.Time, cutoff int64) (head string, ok bool, err error) {
	err = tx.QueryRow(`SELECT decision_id FROM (
		SELECT decision_id, ROW_NUMBER() OVER (ORDER BY created_at DESC, rowid DESC) AS rn
		FROM operator_decisions
		WHERE subject_kind = ? AND subject_id = ?
			AND kind IN ('expected','not_expected','snooze')
			AND NOT EXISTS (SELECT 1 FROM operator_decisions u WHERE u.kind = 'undo' AND u.supersedes = operator_decisions.decision_id)
			AND (kind != 'snooze' OR until > ?)
			AND created_at >= ?
	) WHERE rn = 1`, subjectKind, subjectID, now.UnixNano(), cutoff).Scan(&head)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return head, err == nil, err
}

// headChangedSinceUndo reports whether the effective outcome moved
// on after the given undo committed, by comparing the current state
// against the baseline the undo pinned at commit time. Review lane
// compares decision IDs ("" means no head); labels compare the
// current owner against the owner the undo restored. A NULL baseline
// marks a pre-migration undo with unavailable history and always
// conflicts: absence of proof is not proof of no change. Purged
// history therefore fails closed instead of replaying as unchanged.
// Notes carry no head and never reach here.
func headChangedSinceUndo(tx *sql.Tx, target Decision, baseline sql.NullString, now time.Time, cutoff int64) (bool, error) {
	if target.Kind == DecisionLabel {
		var owner sql.NullString
		if err := tx.QueryRow(`SELECT label_owner FROM reported_subjects WHERE profile_id = ?`,
			target.SubjectID).Scan(&owner); err != nil {
			if err == sql.ErrNoRows {
				return true, nil
			}
			return false, err
		}
		current := ""
		if owner.Valid {
			current = owner.String
		}
		return current != target.PriorOwner, nil
	}
	if !baseline.Valid {
		return true, nil
	}
	head, ok, err := reviewHead(tx, target.SubjectKind, target.SubjectID, now, cutoff)
	if err != nil {
		return false, err
	}
	current := ""
	if ok {
		current = head
	}
	return baseline.String != current, nil
}

// UndoDecision appends an explicit undo record for one decision.
// Undo rows are terminal: undoing an undo is rejected, redo by
// appending a fresh decision. Repeating an undo replays as
// already_undone only while state is unchanged since that undo; a
// moved-on head — review lane by effective order, label by owner —
// reports conflict, as does a first-time undo of a superseded
// record. Purged targets read as unknown, leaving state untouched.
func (s *Store) UndoDecision(ctx context.Context, targetID, requestID string) (DecisionResult, error) {
	now := s.now()
	if err := checkRequestID(requestID); err != nil {
		return DecisionResult{}, err
	}
	hash := decisionHash("undo", targetID, DecisionUndo, 0, false, "", targetID)

	var res DecisionResult
	err := s.write(ctx, func(tx *sql.Tx) error {
		if requestID != "" {
			if existing, err := findDecisionByRequest(tx, requestID, hash); err != nil {
				return err
			} else if existing != nil {
				res = DecisionResult{Decision: *existing, Applied: false, Reason: "duplicate_request"}
				return nil
			}
		}
		row := tx.QueryRow(`SELECT `+decisionColumns+` FROM operator_decisions WHERE decision_id = ?`, targetID)
		target, _, err := scanDecision(row)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: %q", ErrUnknownDecision, targetID)
		}
		if err != nil {
			return err
		}
		if target.Kind == DecisionUndo {
			return fmt.Errorf("%w: %q", ErrInvalidUndo, targetID)
		}
		var undoBaseline sql.NullString
		err = tx.QueryRow(`SELECT head_asof FROM operator_decisions
			WHERE kind = 'undo' AND supersedes = ?
			ORDER BY created_at DESC, rowid DESC LIMIT 1`, targetID).Scan(&undoBaseline)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			// The target is already neutralized. An exact request
			// replay is idempotent, but a separate repeat operation
			// must distinguish unchanged from subsequently changed
			// state: when the effective outcome moved on after the
			// undo, the operator acts on a stale view and the repeat
			// conflicts instead of no-oping.
			if target.Kind == DecisionNote {
				res = DecisionResult{Decision: target, Applied: false, Reason: "already_undone"}
				return nil
			}
			moved, err := headChangedSinceUndo(tx, target, undoBaseline, now, s.decisionCutoff(now))
			if err != nil {
				return err
			}
			if moved {
				return fmt.Errorf("%w: decision %q already undone and state moved on", ErrUndoConflict, targetID)
			}
			res = DecisionResult{Decision: target, Applied: false, Reason: "already_undone"}
			return nil
		}
		if target.Kind == DecisionExpected || target.Kind == DecisionNotExpected || target.Kind == DecisionSnooze {
			// Review decisions form a head: only the effective record
			// can be undone. Undoing a superseded record would rewrite
			// history the head already moved past, so it conflicts
			// instead of appending a no-op undo.
			head, ok, err := reviewHead(tx, target.SubjectKind, target.SubjectID, now, s.decisionCutoff(now))
			if err != nil {
				return err
			}
			if !ok || head != targetID {
				return fmt.Errorf("%w: review decision %q is no longer current", ErrUndoConflict, targetID)
			}
		}
		if target.Kind == DecisionLabel {
			var owner sql.NullString
			if err := tx.QueryRow(`SELECT label_owner FROM reported_subjects WHERE profile_id = ?`,
				target.SubjectID).Scan(&owner); err != nil {
				if err == sql.ErrNoRows {
					return fmt.Errorf("%w: %q", ErrUnknownSubject, target.SubjectID)
				}
				return err
			}
			if !owner.Valid || owner.String != targetID {
				return fmt.Errorf("%w: label %q is no longer current", ErrUndoConflict, targetID)
			}
			var priorLabel, priorOwner sql.NullString
			if target.PriorLabel != nil {
				priorLabel = sql.NullString{String: *target.PriorLabel, Valid: true}
			}
			if target.PriorOwner != "" {
				priorOwner = sql.NullString{String: target.PriorOwner, Valid: true}
			}
			if _, err := tx.Exec(`UPDATE reported_subjects SET operator_label = ?, label_owner = ?
				WHERE profile_id = ?`, priorLabel, priorOwner, target.SubjectID); err != nil {
				return err
			}
		}
		id := fmt.Sprintf("dec-%d-%d", now.UnixNano(), s.decisionSeq.Add(1))
		if _, err := tx.Exec(`INSERT INTO operator_decisions (decision_id, subject_kind, subject_id, kind,
			until, text, created_at, supersedes, request_id, content_hash, prior_label, prior_owner)
			VALUES (?, ?, ?, 'undo', NULL, '', ?, ?, ?, ?, NULL, NULL)`,
			id, target.SubjectKind, target.SubjectID, now.UnixNano(), targetID,
			nullString(requestID), hash); err != nil {
			return err
		}
		// Pin the resulting review head on the undo row itself: a
		// later repeat compares against this baseline instead of
		// recomputing it from surviving rows, which cannot tell "no
		// head existed" from "the head was purged". "" records an
		// empty head; pre-migration rows keep NULL (unknown).
		head, ok, err := reviewHead(tx, target.SubjectKind, target.SubjectID, now, s.decisionCutoff(now))
		if err != nil {
			return err
		}
		baseline := ""
		if ok {
			baseline = head
		}
		if _, err := tx.Exec(`UPDATE operator_decisions SET head_asof = ? WHERE decision_id = ?`, baseline, id); err != nil {
			return err
		}
		res = DecisionResult{Decision: Decision{DecisionID: id, SubjectKind: target.SubjectKind,
			SubjectID: target.SubjectID, Kind: DecisionUndo, CreatedAt: now,
			Supersedes: targetID, RequestID: requestID}, Applied: true}
		return nil
	})
	if err != nil {
		return DecisionResult{}, err
	}
	return res, nil
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// ReviewState is the effective review-lane outcome for one subject:
// the latest non-undone expected/not_expected/snooze record, with
// lapsed snoozes already excluded by the query.
type ReviewState struct {
	Kind       string
	Until      time.Time
	HasUntil   bool
	DecisionID string
}

// EffectiveReview returns the review-lane outcome per subject. Audit
// past the retention cutoff is excluded even before purge deletes it,
// so a stalled purge never extends a verdict; snooze rows past their
// until are excluded up front so an earlier expected or not_expected
// resumes.
func (s *Store) EffectiveReview(ctx context.Context, subjectKind string, subjectIDs []string, now time.Time) (map[string]ReviewState, error) {
	out := map[string]ReviewState{}
	if len(subjectIDs) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(subjectIDs))
	placeholders = strings.TrimSuffix(placeholders, ",")
	args := make([]any, 0, len(subjectIDs)+2)
	args = append(args, subjectKind)
	for _, id := range subjectIDs {
		args = append(args, id)
	}
	args = append(args, now.UnixNano(), s.decisionCutoff(now))
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT subject_id, kind, until, decision_id FROM (
			SELECT subject_id, kind, until, decision_id,
				ROW_NUMBER() OVER (PARTITION BY subject_id ORDER BY created_at DESC, rowid DESC) AS rn
			FROM operator_decisions d
			WHERE subject_kind = ? AND subject_id IN (%s)
				AND kind IN ('expected','not_expected','snooze')
				AND NOT EXISTS (SELECT 1 FROM operator_decisions u WHERE u.kind = 'undo' AND u.supersedes = d.decision_id)
				AND (d.kind != 'snooze' OR d.until > ?)
				AND d.created_at >= ?
		) WHERE rn = 1`, placeholders), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var subjectID, kind, decisionID string
		var until sql.NullInt64
		if err := rows.Scan(&subjectID, &kind, &until, &decisionID); err != nil {
			return nil, err
		}
		st := ReviewState{Kind: kind, DecisionID: decisionID}
		if until.Valid {
			st.Until, st.HasUntil = fromNanos(until.Int64), true
		}
		out[subjectID] = st
	}
	return out, rows.Err()
}

// DecisionsForSubject lists one subject's audit trail newest-first
// with each row's undone marker. limit clamps to the serving bound;
// notes and labels coexist — only the undone flag changes reads.
// beforeNanos/beforeRowID continue an earlier page: only rows
// strictly older than that cursor return, so a still-current head or
// label pushed past the first page stays reachable (and undoable).
// Zero cursor starts at the newest row.
func (s *Store) DecisionsForSubject(ctx context.Context, subjectKind, subjectID string, limit int, beforeNanos, beforeRowID int64) ([]Decision, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	query := `SELECT rowid, ` + decisionColumns + `,
		EXISTS(SELECT 1 FROM operator_decisions u WHERE u.kind = 'undo' AND u.supersedes = operator_decisions.decision_id) AS undone
		FROM operator_decisions
		WHERE subject_kind = ? AND subject_id = ?`
	args := []any{subjectKind, subjectID}
	if beforeRowID > 0 {
		query += ` AND (created_at < ? OR (created_at = ? AND rowid < ?))`
		args = append(args, beforeNanos, beforeNanos, beforeRowID)
	}
	query += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		var d Decision
		var until, created sql.NullInt64
		var text, supersedes, requestID, hash, priorOwner sql.NullString
		var priorLabel sql.NullString
		var undone bool
		if err := rows.Scan(&d.RowID, &d.DecisionID, &d.SubjectKind, &d.SubjectID, &d.Kind,
			&until, &text, &created, &supersedes, &requestID, &hash, &priorLabel, &priorOwner, &undone); err != nil {
			return nil, err
		}
		if until.Valid {
			d.Until, d.HasUntil = fromNanos(until.Int64), true
		}
		d.Text = text.String
		if created.Valid {
			d.CreatedAt = fromNanos(created.Int64)
		}
		d.Supersedes = supersedes.String
		d.RequestID = requestID.String
		if priorLabel.Valid {
			d.PriorLabel = &priorLabel.String
		}
		d.PriorOwner = priorOwner.String
		d.Undone = undone
		out = append(out, d)
	}
	return out, rows.Err()
}
