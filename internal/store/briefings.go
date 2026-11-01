package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"
)

type Briefing struct {
	GroupingVersion                              int // copied from the group, never inferred from its anchor
	BriefingID, GroupID, HostAnchorKey, Baseline string
	ModelSuggestion                              *string
	Priority, PriorityReason                     string
	BaselineRule                                 string // matched baseline rule ID, empty when unknown
	Kind                                         string // reported-change kind for digest grouping, default other
	Source                                       string // rules|model|agent|fallback|legacy; every insert sets it
	Clamped, FallbackUsed                        bool
	WhatChanged, Context, RecommendedCheck       string
	ModelRawJSON                                 string
	CreatedAt                                    time.Time
	EventIDs                                     []string // derived from group_events when empty
	Seq                                          int64    // resumable-stream cursor (migration v8), 1-based, gaps after purge
}

// briefingColumns is the single column list for every briefing read;
// seq rides along so the API, the SSE id: field and replay share it.
const briefingColumns = `briefing_id, group_id, host_anchor_key, baseline, model_suggestion,
	priority, priority_reason, baseline_rule, kind, source, clamped, fallback_used, what_changed, context,
	recommended_check, model_raw_json, created_at, event_ids_json, seq, grouping_version`

func scanBriefing(rows interface {
	Scan(dest ...any) error
}, b *Briefing) error {
	var suggestion, rule, rawJSON sql.NullString
	var eventIDs string
	var created int64
	if err := rows.Scan(&b.BriefingID, &b.GroupID, &b.HostAnchorKey, &b.Baseline, &suggestion,
		&b.Priority, &b.PriorityReason, &rule, &b.Kind, &b.Source, &b.Clamped, &b.FallbackUsed, &b.WhatChanged, &b.Context,
		&b.RecommendedCheck, &rawJSON, &created, &eventIDs, &b.Seq, &b.GroupingVersion); err != nil {
		return err
	}
	if suggestion.Valid {
		b.ModelSuggestion = &suggestion.String
	}
	if rule.Valid {
		b.BaselineRule = rule.String
	}
	// model_raw_json remains nullable for retained rows; a NULL audit
	// reads as empty rather than failing the briefing read.
	b.ModelRawJSON = rawJSON.String
	b.CreatedAt = fromNanos(created)
	if eventIDs != "" {
		if err := json.Unmarshal([]byte(eventIDs), &b.EventIDs); err != nil {
			return err
		}
	}
	return nil
}

// BriefingForGroup reads the briefing row for a group, if any.
func (s *Store) BriefingForGroup(ctx context.Context, groupID string) (Briefing, error) {
	var b Briefing
	row := s.db.QueryRowContext(ctx, `SELECT `+briefingColumns+` FROM briefings WHERE group_id = ?`, groupID)
	if err := scanBriefing(row, &b); err != nil {
		return Briefing{}, err
	}
	return b, nil
}

// MaxSeq returns the highest assigned briefing seq, or 0 when no
// briefing exists. The Last-Event-ID rule compares against it.
func (s *Store) MaxSeq(ctx context.Context) (int64, error) {
	var max sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM briefings`).Scan(&max); err != nil {
		return 0, err
	}
	if !max.Valid {
		return 0, nil
	}
	return max.Int64, nil
}

// ReplayAfter fetches at most limit briefings newer than seq, ascending.
// The replay path asks for replayMax+1 and resyncs (replaying nothing)
// when the extra row is present — one bounded query, so no count/fetch
// race and no silent truncation. Unlike BriefingsPage it takes no clamp:
// callers pass replayMax+1 with replayMax validated at startup. The
// briefings_seq index serves the range.
func (s *Store) ReplayAfter(ctx context.Context, seq int64, limit int) ([]Briefing, error) {
	if seq < 0 {
		return nil, fmt.Errorf("negative briefing cursor")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("replay limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+briefingColumns+` FROM briefings WHERE seq > ? ORDER BY seq ASC LIMIT ?`, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Briefing
	for rows.Next() {
		var b Briefing
		if err := scanBriefing(rows, &b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BriefingsPage returns one cursor page of briefings, always in ascending
// seq order. after > 0 selects seq > after (reconnect gap-fill, oldest
// first); otherwise the latest page ending before `before` (0 = no bound)
// is returned. limit <= 0 defaults to 50 and clamps to 500, matching the
// client list cap. Negative cursors are caller errors.
func (s *Store) BriefingsPage(ctx context.Context, after, before int64, limit int) ([]Briefing, error) {
	if after < 0 || before < 0 {
		return nil, fmt.Errorf("negative briefing cursor")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	var (
		query string
		args  []any
	)
	if after > 0 {
		query = `SELECT ` + briefingColumns + ` FROM briefings WHERE seq > ?`
		args = append(args, after)
		if before > 0 {
			query += ` AND seq < ?`
			args = append(args, before)
		}
		query += ` ORDER BY seq ASC LIMIT ?`
	} else {
		query = `SELECT ` + briefingColumns + ` FROM briefings`
		if before > 0 {
			query += ` WHERE seq < ?`
			args = append(args, before)
		}
		query += ` ORDER BY seq DESC LIMIT ?`
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Briefing
	for rows.Next() {
		var b Briefing
		if err := scanBriefing(rows, &b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if after == 0 {
		slices.Reverse(out)
	}
	return out, nil
}

func groupEventIDs(tx *sql.Tx, groupID string) ([]string, error) {
	rows, err := tx.Query(`SELECT event_id FROM group_events WHERE group_id = ? ORDER BY event_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// InsertBriefing stores the group's single briefing and marks it done.
// The one_briefing_per_group unique index makes a second insert fail.
func (s *Store) InsertBriefing(ctx context.Context, b Briefing) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		// The group owns provenance; a caller cannot relabel historical work.
		var groupingVersion int
		if err := tx.QueryRow(`SELECT grouping_version FROM active_groups WHERE group_id = ?`, b.GroupID).Scan(&groupingVersion); err != nil {
			return err
		}
		eventIDs := b.EventIDs
		derived, err := groupEventIDs(tx, b.GroupID)
		if err != nil {
			return err
		}
		if len(eventIDs) == 0 {
			eventIDs = derived
		} else {
			got := append([]string(nil), eventIDs...)
			sort.Strings(got)
			if !slices.Equal(got, derived) {
				return fmt.Errorf("event_ids do not match group_events for group %s", b.GroupID)
			}
			eventIDs = derived
		}
		eventIDsJSON, err := json.Marshal(eventIDs)
		if err != nil {
			return err
		}
		rule := b.BaselineRule
		var ruleArg any
		if rule != "" {
			ruleArg = rule
		}
		// Kind defaults to other and source derives from the fallback
		// flag when the caller leaves them unset, so every insert sets
		// both without guessing historical model/rules output. Explicit
		// rules/agent values pass through for the investigator phase.
		kind := b.Kind
		if kind == "" {
			kind = "other"
		}
		source := b.Source
		if source == "" {
			source = "model"
			if b.FallbackUsed {
				source = "fallback"
			}
		}
		// The resumable-stream cursor: bumped from the durable high-water
		// mark in the same transaction, so ids stay monotonic across
		// purges and restarts under the single writer. MAX(seq)+1 would
		// reuse ids after a purge deletes the maximum. Gaps from purged
		// rows are fine: replay compares by seq > last, never by
		// contiguity. A missing state row fails the insert loudly rather
		// than reusing an id.
		var seq int64
		if err := tx.QueryRow(`UPDATE seq_state SET next_seq = next_seq + 1 WHERE id = 0 RETURNING next_seq - 1`).Scan(&seq); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO briefings (briefing_id, group_id, host_anchor_key, baseline, model_suggestion, priority, priority_reason, baseline_rule, kind, source, clamped, fallback_used, what_changed, context, recommended_check, model_raw_json, created_at, event_ids_json, seq, grouping_version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.BriefingID, b.GroupID, b.HostAnchorKey, b.Baseline, b.ModelSuggestion, b.Priority, b.PriorityReason, ruleArg, kind, source, b.Clamped, b.FallbackUsed, b.WhatChanged, b.Context, b.RecommendedCheck, b.ModelRawJSON, nanos(b.CreatedAt), string(eventIDsJSON), seq, groupingVersion); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE active_groups SET status = 'done' WHERE group_id = ? AND status = 'processing'`, b.GroupID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("group %s is not in processing state", b.GroupID)
		}
		// Briefing-subject relations copy the group's links: explicit
		// rows, never timeline joins on current hostname/IP/MAC
		// ownership. A group with no projected subjects (undecodable
		// payload, or projection lagging the take) stores no relations
		// rather than invented ones.
		if _, err := tx.Exec(`INSERT OR IGNORE INTO briefing_subjects (briefing_id, profile_id, role)
			SELECT ?, profile_id, role FROM group_subjects WHERE group_id = ?`, b.BriefingID, b.GroupID); err != nil {
			return err
		}
		return nil
	})
}
