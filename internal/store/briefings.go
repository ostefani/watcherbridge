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
	// LikelyExplanation is the model's display-only hypothesis
	// (Designv9 §6.3): nil for deterministic rows and pre-migration
	// rows, which carry no model output. It never feeds priority,
	// identity or the resolver.
	LikelyExplanation *string
	// Confidence is the model's display-only self-assessment: "" reads
	// as unknown, otherwise low|medium|high. Display only, like the
	// explanation it qualifies.
	Confidence string
	// EvidenceRefs are the model's validated citations: real group
	// events and real 0-based executed tool steps of the producing run.
	EvidenceRefs []EvidenceRef
	// InputTokens, OutputTokens and CostMicros are the adapter-reported
	// accounting for the producing provider calls; nil means the
	// adapter supplied none (unknown, never zero-filled).
	InputTokens  *int64
	OutputTokens *int64
	CostMicros   *int64
	// Traces is write-only input for InsertBriefing: the run's executed
	// tool steps, stored atomically with the briefing so a restart can
	// never orphan or duplicate them. Never populated on reads; use
	// AgentTraces for that.
	Traces []AgentTrace
}

// EvidenceRef is one validated model citation (Designv9 §6.3): either a
// real group event or a real 0-based executed tool step of the run.
// ToolStep is -1 when the ref names an event.
type EvidenceRef struct {
	EventID  string
	ToolStep int
}

// EventRef names a group event citation.
func EventRef(eventID string) EvidenceRef { return EvidenceRef{EventID: eventID, ToolStep: -1} }

// ToolRef names an executed tool-step citation (0-based).
func ToolRef(step int) EvidenceRef { return EvidenceRef{ToolStep: step} }

// AgentTrace is one executed investigator tool step (Designv9 §6.3):
// validated args and the leak-scanned result envelope, both tokenized,
// never raw identifiers or rejected free text. Steps are dense and
// 0-based in execution order, so finals can cite them.
type AgentTrace struct {
	BriefingID string
	Step       int
	Tool       string
	ArgsJSON   string
	ResultJSON string
	Status     string // ok|blocked|error: the result envelope kind
	DurationMS int64
}

// maxAgentTraceRows caps the trace read: executions are bounded by the
// run's step budget, so this is a backstop, not a limit the loop can hit.
const maxAgentTraceRows = 32

// briefingColumns is the single column list for every briefing read;
// seq rides along so the API, the SSE id: field and replay share it.
const briefingColumns = `briefing_id, group_id, host_anchor_key, baseline, model_suggestion,
	priority, priority_reason, baseline_rule, kind, source, clamped, fallback_used, what_changed, context,
	recommended_check, model_raw_json, created_at, event_ids_json, seq, grouping_version,
	likely_explanation, confidence, evidence_refs_json, input_tokens, output_tokens, cost_micros`

func scanBriefing(rows interface {
	Scan(dest ...any) error
}, b *Briefing) error {
	var suggestion, rule, rawJSON sql.NullString
	var eventIDs string
	var created int64
	var explanation, confidence, refsJSON sql.NullString
	var inputTokens, outputTokens, costMicros sql.NullInt64
	if err := rows.Scan(&b.BriefingID, &b.GroupID, &b.HostAnchorKey, &b.Baseline, &suggestion,
		&b.Priority, &b.PriorityReason, &rule, &b.Kind, &b.Source, &b.Clamped, &b.FallbackUsed, &b.WhatChanged, &b.Context,
		&b.RecommendedCheck, &rawJSON, &created, &eventIDs, &b.Seq, &b.GroupingVersion,
		&explanation, &confidence, &refsJSON, &inputTokens, &outputTokens, &costMicros); err != nil {
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
	// The Step 27 fields stay nullable for the same reason: retained
	// and deterministic rows predate model output, so NULL reads as
	// unknown rather than failing the briefing read.
	if explanation.Valid {
		b.LikelyExplanation = &explanation.String
	}
	b.Confidence = confidence.String
	if refsJSON.Valid && refsJSON.String != "" {
		refs, err := parseEvidenceRefs(refsJSON.String)
		if err != nil {
			return err
		}
		b.EvidenceRefs = refs
	}
	if inputTokens.Valid {
		b.InputTokens = &inputTokens.Int64
	}
	if outputTokens.Valid {
		b.OutputTokens = &outputTokens.Int64
	}
	if costMicros.Valid {
		b.CostMicros = &costMicros.Int64
	}
	return nil
}

// marshalEvidenceRefs encodes refs for storage: one object per ref with
// exactly one key, so the read inverse stays unambiguous.
func marshalEvidenceRefs(refs []EvidenceRef) (string, error) {
	if len(refs) == 0 {
		return "", nil
	}
	out := make([]map[string]any, 0, len(refs))
	for _, r := range refs {
		if r.ToolStep >= 0 {
			if r.EventID != "" {
				return "", fmt.Errorf("evidence ref names both an event and a tool step")
			}
			out = append(out, map[string]any{"tool_step": r.ToolStep})
			continue
		}
		if r.EventID == "" {
			return "", fmt.Errorf("evidence ref names neither an event nor a tool step")
		}
		out = append(out, map[string]any{"event_id": r.EventID})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// parseEvidenceRefs decodes the stored refs column: NULL/empty reads as
// no refs; corrupt JSON fails the read loudly rather than inventing or
// dropping citations.
func parseEvidenceRefs(raw string) ([]EvidenceRef, error) {
	var items []map[string]any
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, err
	}
	out := make([]EvidenceRef, 0, len(items))
	for _, item := range items {
		if len(item) != 1 {
			return nil, fmt.Errorf("evidence ref must name exactly one target")
		}
		if id, ok := item["event_id"]; ok {
			s, ok := id.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("evidence event ref must be a non-empty string")
			}
			out = append(out, EventRef(s))
			continue
		}
		if step, ok := item["tool_step"]; ok {
			f, ok := step.(float64)
			if !ok || f < 0 || f != float64(int(f)) {
				return nil, fmt.Errorf("evidence tool ref must be a non-negative integer")
			}
			out = append(out, ToolRef(int(f)))
			continue
		}
		return nil, fmt.Errorf("evidence ref names an unknown target")
	}
	return out, nil
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
	_, err := s.InsertBriefingWithSeq(ctx, b)
	return err
}

// InsertBriefingWithSeq inserts the briefing and returns its sequence after
// the transaction commits.
func (s *Store) InsertBriefingWithSeq(ctx context.Context, b Briefing) (int64, error) {
	var seq int64
	if err := s.write(ctx, func(tx *sql.Tx) error {
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
		if err := tx.QueryRow(`UPDATE seq_state SET next_seq = next_seq + 1 WHERE id = 0 RETURNING next_seq - 1`).Scan(&seq); err != nil {
			return err
		}
		refsJSON, err := marshalEvidenceRefs(b.EvidenceRefs)
		if err != nil {
			return err
		}
		var refsArg any
		if refsJSON != "" {
			refsArg = refsJSON
		}
		var explanationArg any
		if b.LikelyExplanation != nil {
			explanationArg = *b.LikelyExplanation
		}
		var confidenceArg any
		if b.Confidence != "" {
			confidenceArg = b.Confidence
		}
		if _, err := tx.Exec(`INSERT INTO briefings (briefing_id, group_id, host_anchor_key, baseline, model_suggestion, priority, priority_reason, baseline_rule, kind, source, clamped, fallback_used, what_changed, context, recommended_check, model_raw_json, created_at, event_ids_json, seq, grouping_version,
			likely_explanation, confidence, evidence_refs_json, input_tokens, output_tokens, cost_micros)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			b.BriefingID, b.GroupID, b.HostAnchorKey, b.Baseline, b.ModelSuggestion, b.Priority, b.PriorityReason, ruleArg, kind, source, b.Clamped, b.FallbackUsed, b.WhatChanged, b.Context, b.RecommendedCheck, b.ModelRawJSON, nanos(b.CreatedAt), string(eventIDsJSON), seq, groupingVersion,
			explanationArg, confidenceArg, refsArg, b.InputTokens, b.OutputTokens, b.CostMicros); err != nil {
			return err
		}
		// Trace steps ride the briefing transaction: a restart that
		// loses the briefing race inserts nothing, so traces can never
		// orphan or duplicate. Steps must be dense and 0-based —
		// finals cite them by index.
		for i, tr := range b.Traces {
			if tr.Step != i {
				return fmt.Errorf("agent trace steps must be dense 0-based, got step %d at index %d", tr.Step, i)
			}
			if _, err := tx.Exec(`INSERT INTO agent_traces (briefing_id, step, tool, args_json, result_json, status, duration_ms)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				b.BriefingID, tr.Step, tr.Tool, tr.ArgsJSON, tr.ResultJSON, tr.Status, tr.DurationMS); err != nil {
				return err
			}
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
	}); err != nil {
		return 0, err
	}
	return seq, nil
}

// AgentTraces reads one briefing's executed tool steps in 0-based
// order. The read carries its own row cap: executions are bounded by
// the run's step budget, so ordinary runs never approach it.
func (s *Store) AgentTraces(ctx context.Context, briefingID string) ([]AgentTrace, error) {
	if briefingID == "" {
		return nil, fmt.Errorf("empty briefing id")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT briefing_id, step, tool, args_json, result_json, status, duration_ms
		FROM agent_traces WHERE briefing_id = ? ORDER BY step ASC LIMIT ?`, briefingID, maxAgentTraceRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentTrace
	for rows.Next() {
		var tr AgentTrace
		if err := rows.Scan(&tr.BriefingID, &tr.Step, &tr.Tool, &tr.ArgsJSON, &tr.ResultJSON, &tr.Status, &tr.DurationMS); err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SubjectBriefing is the tool-sized briefing projection for Step 26
// (Designv9 §6.2 get_reported_history): identity, verdict, and timing
// only — never briefing text or raw JSON, so one tool result stays far
// under its byte budget.
type SubjectBriefing struct {
	BriefingID      string
	GroupID         string
	Priority        string
	ModelSuggestion *string
	FallbackUsed    bool
	Source          string
	CreatedAt       time.Time
}

// maxSubjectBriefings caps the history tool's briefing section; ten
// verdict rows fit the 4 KiB tool budget with room for the omission
// flags and the associated event lists.
const maxSubjectBriefings = 10

// BriefingsForSubject returns the newest briefings explicitly
// associated with one subject, newest first. The read carries its own
// row and column bounds: no text columns, no cursor — the tool reports
// what is retained, most recent first.
func (s *Store) BriefingsForSubject(ctx context.Context, profileID string, limit int) ([]SubjectBriefing, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > maxSubjectBriefings {
		limit = maxSubjectBriefings
	}
	// One briefing links a profile under several roles (host plus
	// previous/current views); DISTINCT keeps one row per briefing.
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT b.briefing_id, b.group_id, b.priority, b.model_suggestion,
		b.fallback_used, b.source, b.created_at FROM briefings b
		JOIN briefing_subjects s ON s.briefing_id = b.briefing_id
		WHERE s.profile_id = ? ORDER BY b.created_at DESC, b.rowid DESC LIMIT ?`, profileID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubjectBriefing
	for rows.Next() {
		var b SubjectBriefing
		var created int64
		if err := rows.Scan(&b.BriefingID, &b.GroupID, &b.Priority, &b.ModelSuggestion,
			&b.FallbackUsed, &b.Source, &created); err != nil {
			return nil, err
		}
		b.CreatedAt = time.Unix(0, created).UTC()
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
