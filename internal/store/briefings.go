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
	BriefingID, GroupID, HostAnchorKey, Baseline string
	ModelSuggestion                              *string
	Priority, PriorityReason                     string
	BaselineRule                                 string // matched baseline rule ID, empty when unknown
	Clamped, FallbackUsed                        bool
	WhatChanged, Context, RecommendedCheck       string
	ModelRawJSON                                 string
	CreatedAt                                    time.Time
	EventIDs                                     []string // derived from group_events when empty
}

// BriefingForGroup reads the briefing row for a group, if any.
func (s *Store) BriefingForGroup(ctx context.Context, groupID string) (Briefing, error) {
	var b Briefing
	var suggestion, rule sql.NullString
	var eventIDs string
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT briefing_id, host_anchor_key, baseline, model_suggestion,
		priority, priority_reason, baseline_rule, clamped, fallback_used, what_changed, context,
		recommended_check, model_raw_json, created_at, event_ids_json
		FROM briefings WHERE group_id = ?`, groupID).Scan(
		&b.BriefingID, &b.HostAnchorKey, &b.Baseline, &suggestion, &b.Priority, &b.PriorityReason,
		&rule, &b.Clamped, &b.FallbackUsed, &b.WhatChanged, &b.Context, &b.RecommendedCheck,
		&b.ModelRawJSON, &created, &eventIDs)
	if err != nil {
		return Briefing{}, err
	}
	b.GroupID = groupID
	if suggestion.Valid {
		b.ModelSuggestion = &suggestion.String
	}
	if rule.Valid {
		b.BaselineRule = rule.String
	}
	b.CreatedAt = fromNanos(created)
	if eventIDs != "" {
		if err := json.Unmarshal([]byte(eventIDs), &b.EventIDs); err != nil {
			return Briefing{}, err
		}
	}
	return b, nil
}

// RecentBriefings returns the newest briefings first, capped at limit.
// The briefings_created index serves the ORDER BY; readers use the
// single-writer-safe read pool, never a long transaction.
func (s *Store) RecentBriefings(ctx context.Context, limit int) ([]Briefing, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT briefing_id, group_id, host_anchor_key, baseline, model_suggestion,
		priority, priority_reason, baseline_rule, clamped, fallback_used, what_changed, context,
		recommended_check, model_raw_json, created_at, event_ids_json
		FROM briefings ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Briefing
	for rows.Next() {
		var b Briefing
		var suggestion, rule sql.NullString
		var eventIDs string
		var created int64
		if err := rows.Scan(&b.BriefingID, &b.GroupID, &b.HostAnchorKey, &b.Baseline, &suggestion,
			&b.Priority, &b.PriorityReason, &rule, &b.Clamped, &b.FallbackUsed, &b.WhatChanged, &b.Context,
			&b.RecommendedCheck, &b.ModelRawJSON, &created, &eventIDs); err != nil {
			return nil, err
		}
		if suggestion.Valid {
			b.ModelSuggestion = &suggestion.String
		}
		if rule.Valid {
			b.BaselineRule = rule.String
		}
		b.CreatedAt = fromNanos(created)
		if eventIDs != "" {
			if err := json.Unmarshal([]byte(eventIDs), &b.EventIDs); err != nil {
				return nil, err
			}
		}
		out = append(out, b)
	}
	return out, rows.Err()
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
		if _, err := tx.Exec(`INSERT INTO briefings (briefing_id, group_id, host_anchor_key, baseline, model_suggestion, priority, priority_reason, baseline_rule, clamped, fallback_used, what_changed, context, recommended_check, model_raw_json, created_at, event_ids_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.BriefingID, b.GroupID, b.HostAnchorKey, b.Baseline, b.ModelSuggestion, b.Priority, b.PriorityReason, ruleArg, b.Clamped, b.FallbackUsed, b.WhatChanged, b.Context, b.RecommendedCheck, b.ModelRawJSON, nanos(b.CreatedAt), string(eventIDsJSON)); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE active_groups SET status = 'done' WHERE group_id = ? AND status = 'processing'`, b.GroupID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("group %s is not in processing state", b.GroupID)
		}
		return nil
	})
}
