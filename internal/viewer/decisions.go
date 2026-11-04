package viewer

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

// Decision routes (Step 25, Designv9 §5.1): local operator memory over
// briefings and reported profiles. Mutations ride guardStateChanging
// (Step 16: POST, JSON, 8 KiB cap, exact Origin, Fetch-Metadata when
// present); those checks defend against browser-origin attacks, not
// against local processes or OS users, and are documented as such.

// decisionWire is one decision record as served: audit content plus
// the undone marker. Prior-label restoration internals stay
// server-side; undo needs only the target ID.
type decisionWire struct {
	DecisionID  string `json:"decision_id"`
	SubjectKind string `json:"subject_kind"`
	SubjectID   string `json:"subject_id"`
	Kind        string `json:"kind"`
	Until       string `json:"until,omitempty"`
	Text        string `json:"text,omitempty"`
	CreatedAt   string `json:"created_at"`
	Supersedes  string `json:"supersedes,omitempty"`
	Undone      bool   `json:"undone"`
	// Cursor continues the trail past this row ("<created nanos>:<rowid>").
	// It is paging state, never row identity: decision_id stays the
	// only identity the UI addresses.
	Cursor string `json:"cursor"`
}

// reviewWire is the effective review-lane state for one subject: the
// latest non-undone expected/not_expected/snooze row, with lapsed
// snoozes already excluded by the store query. Absent means no live
// operator review; the audit trail (with undone markers) carries the
// rest. Profiles have no review lane, so theirs is always absent.
type reviewWire struct {
	Kind       string `json:"kind"`
	Until      string `json:"until,omitempty"`
	DecisionID string `json:"decision_id"`
}

func reviewWireOf(state store.ReviewState) reviewWire {
	out := reviewWire{Kind: state.Kind, DecisionID: state.DecisionID}
	if state.HasUntil {
		out.Until = state.Until.UTC().Format(time.RFC3339)
	}
	return out
}

// mutationWire reports a POST outcome. Applied=false with a reason
// means the valid request changed nothing (identical replay, already
// neutralized target) — never a masked validation failure, which
// stays a 4xx, and never a lost race, which is a 409. Members carries
// the generated member rows of an incident fan-out; Generated carries
// the member decision IDs an undone incident decision generated, for
// individual revisit — undo itself never cascades.
type mutationWire struct {
	Decision      decisionWire   `json:"decision"`
	Applied       bool           `json:"applied"`
	Reason        string         `json:"reason,omitempty"`
	Members       []decisionWire `json:"members,omitempty"`
	Generated     []string       `json:"generated_decision_ids,omitempty"`
	MembersNext   int64          `json:"members_next,omitempty"`
	GeneratedNext int64          `json:"generated_next,omitempty"`
}

func decisionWireOf(d store.Decision) decisionWire {
	w := decisionWire{
		DecisionID: d.DecisionID, SubjectKind: d.SubjectKind, SubjectID: d.SubjectID,
		Kind: d.Kind, Text: d.Text, CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
		Supersedes: d.Supersedes, Undone: d.Undone,
		Cursor: fmt.Sprintf("%d:%d", d.CreatedAt.UnixNano(), d.RowID),
	}
	if d.HasUntil {
		w.Until = d.Until.UTC().Format(time.RFC3339)
	}
	return w
}

// handleDecisionMembers continues a committed fan-out using its parent
// decision, never the incident's mutable membership. The cursor is exclusive.
func (v *Viewer) handleDecisionMembers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validDecisionID(id) {
		writeViewerError(w, "invalid id", http.StatusBadRequest)
		return
	}
	limit := 200
	var after int64
	q := r.URL.Query()
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			writeViewerError(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
	if raw := q.Get("after"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeViewerError(w, "invalid after", http.StatusBadRequest)
			return
		}
		after = n
	}
	page, err := v.store.GeneratedDecisionPage(r.Context(), id, limit, after)
	if errors.Is(err, sql.ErrNoRows) {
		writeViewerError(w, "decision not found", http.StatusNotFound)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	out := struct {
		Members []decisionWire `json:"members"`
		Next    int64          `json:"next,omitempty"`
	}{Members: make([]decisionWire, 0, len(page.Members)), Next: page.Next}
	for _, d := range page.Members {
		out.Members = append(out.Members, decisionWireOf(d))
	}
	writeJSON(w, out)
}

// appendDecisionGuarded enforces the Step 16 POST contract before
// appendDecision; registered as its own method-qualified pattern so
// the mux never sees an unqualified overlap with GET /.
func (v *Viewer) appendDecisionGuarded(w http.ResponseWriter, r *http.Request) {
	v.guardStateChanging(v.appendDecision)(w, r)
}

func (v *Viewer) listDecisions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, subject := q.Get("subject_kind"), q.Get("subject_id")
	if kind == "" || subject == "" {
		writeViewerError(w, "subject_kind and subject_id are required", http.StatusBadRequest)
		return
	}
	if !validDecisionSubject(kind, subject) {
		writeViewerError(w, "unknown decision subject", http.StatusBadRequest)
		return
	}
	limit := 50
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 || n > 200 {
			writeViewerError(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
	// before continues an earlier page: "<created nanos>:<rowid>",
	// strictly older rows only. A malformed cursor is 400, never a
	// lookup; an unknown-but-well-formed one pages past retained rows
	// to an empty page. The response stays a bare newest-first array:
	// a full page may have more.
	var beforeNanos, beforeRowID int64
	if s := q.Get("before"); s != "" {
		nanosPart, rowPart, ok := strings.Cut(s, ":")
		var err error
		if beforeNanos, err = strconv.ParseInt(nanosPart, 10, 64); !ok || err != nil || beforeNanos < 0 {
			writeViewerError(w, "invalid before", http.StatusBadRequest)
			return
		}
		if beforeRowID, err = strconv.ParseInt(rowPart, 10, 64); err != nil || beforeRowID <= 0 {
			writeViewerError(w, "invalid before", http.StatusBadRequest)
			return
		}
	}
	rows, err := v.store.DecisionsForSubject(r.Context(), kind, canonicalSubjectID(kind, subject), limit, beforeNanos, beforeRowID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]decisionWire, 0, len(rows))
	for _, d := range rows {
		out = append(out, decisionWireOf(d))
	}
	writeJSON(w, out)
}

// appendDecision stores one non-undo decision. 201 on a state change;
// 200 with applied:false and a reason on an identical replay.
// Validation failures are 4xx; a reused request ID with different
// content is a 409, never a silent overwrite.
func (v *Viewer) appendDecision(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SubjectKind    string `json:"subject_kind"`
		SubjectID      string `json:"subject_id"`
		Kind           string `json:"kind"`
		Until          string `json:"until"`
		Text           string `json:"text"`
		RequestID      string `json:"request_id"`
		IncludeMembers *bool  `json:"include_members"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeViewerError(w, "invalid decision body", http.StatusBadRequest)
		return
	}
	if !validDecisionSubject(body.SubjectKind, body.SubjectID) {
		writeViewerError(w, "unknown decision subject", http.StatusBadRequest)
		return
	}
	in := store.DecisionInput{
		SubjectKind: body.SubjectKind,
		SubjectID:   canonicalSubjectID(body.SubjectKind, body.SubjectID),
		Kind:        body.Kind,
		Text:        body.Text,
		RequestID:   body.RequestID,
	}
	if body.Until != "" {
		until, err := time.Parse(time.RFC3339, body.Until)
		if err != nil {
			writeViewerError(w, "until must be RFC 3339", http.StatusBadRequest)
			return
		}
		in.HasUntil, in.Until = true, until
	} else if body.Kind == store.DecisionSnooze {
		// A snooze without until inherits the configured default
		// horizon, passed through unresolved: the store hashes the
		// specified content, so a retry replays against the original
		// row instead of hashing a freshly resolved deadline.
		in.SnoozeDefault = v.SnoozeDefault
		if in.SnoozeDefault <= 0 {
			in.SnoozeDefault = 24 * time.Hour
		}
	}
	if body.SubjectKind == store.DecisionSubjectIncident {
		// Acknowledging an incident must state whether members are
		// included: incident-only acknowledgement never implicitly
		// suppresses member attention.
		if body.IncludeMembers == nil {
			writeViewerError(w, "include_members is required for incident decisions", http.StatusBadRequest)
			return
		}
		res, err := v.store.AppendIncidentDecision(r.Context(), in, *body.IncludeMembers)
		if err != nil {
			writeDecisionError(w, err)
			return
		}
		v.Overview.Trigger()
		v.Hub().Broadcast("incident", incidentEvent{Changed: []string{in.SubjectID}})
		out := mutationWire{Decision: decisionWireOf(res.Parent), Applied: res.Applied, Reason: res.Reason, MembersNext: res.MembersNext}
		for _, m := range res.Members {
			out.Members = append(out.Members, decisionWireOf(m))
		}
		if res.Applied {
			w.WriteHeader(http.StatusCreated)
		}
		writeJSON(w, out)
		return
	}
	if body.IncludeMembers != nil {
		writeViewerError(w, "include_members applies to incident subjects only", http.StatusBadRequest)
		return
	}
	res, err := v.store.AppendDecision(r.Context(), in)
	if err != nil {
		writeDecisionError(w, err)
		return
	}
	v.Overview.Trigger()
	out := mutationWire{Decision: decisionWireOf(res.Decision), Applied: res.Applied, Reason: res.Reason}
	if res.Applied {
		w.WriteHeader(http.StatusCreated)
	}
	writeJSON(w, out)
}

// handleDecisionUndo serves POST /api/decisions/{id}/undo. The body
// is an optional JSON object carrying request_id for retry safety;
// an empty body undoes without an idempotency key.
func (v *Viewer) handleDecisionUndo(w http.ResponseWriter, r *http.Request) {
	v.guardStateChanging(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" || !validDecisionID(id) {
			writeViewerError(w, "invalid id", http.StatusBadRequest)
			return
		}
		var body struct {
			RequestID string `json:"request_id"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeViewerError(w, "invalid undo body", http.StatusBadRequest)
				return
			}
		}
		res, err := v.store.UndoDecision(r.Context(), id, body.RequestID)
		if err != nil {
			writeDecisionError(w, err)
			return
		}
		v.Overview.Trigger()
		out := mutationWire{Decision: decisionWireOf(res.Decision), Applied: res.Applied, Reason: res.Reason}
		// Undo never cascades, but when the target generated member
		// rows the response names them so the operator can revisit
		// each member judgment individually.
		if page, err := v.store.GeneratedDecisionPage(r.Context(), id, 500, 0); err == nil {
			for _, d := range page.Members {
				out.Generated = append(out.Generated, d.DecisionID)
			}
			out.GeneratedNext = page.Next
		}
		writeJSON(w, out)
	})(w, r)
}

// validDecisionSubject allowlists subject reference shapes before any
// store lookup: briefing IDs in either spelling, locally generated
// profile IDs, and correlator-assigned incident IDs. Anything else is
// malformed (400), never a store probe. Well-formed but unsupported
// combinations (labels on briefings) travel to the store, which
// rejects them as 422 with an explicit reason instead of silently
// storing them.
func validDecisionSubject(kind, id string) bool {
	switch kind {
	case store.DecisionSubjectBriefing:
		return strings.HasPrefix(id, "brf-") || strings.HasPrefix(id, "grp-")
	case store.DecisionSubjectProfile:
		return validProfileID(id)
	case store.DecisionSubjectIncident:
		return validIncidentID(id)
	case "legacy":
		return strings.HasPrefix(id, "brf-") || strings.HasPrefix(id, "grp-") || validProfileID(id)
	}
	return false
}

// canonicalSubjectID normalizes the briefing-ID prefix alias so both
// spellings address one history; the store keys briefing decisions by
// briefing ID. Profile IDs pass through.
func canonicalSubjectID(kind, id string) string {
	if kind == store.DecisionSubjectBriefing {
		return "brf-" + strings.TrimPrefix(id, "brf-")
	}
	return id
}

// validDecisionID allowlists generated decision IDs: dec- plus digits.
func validDecisionID(id string) bool {
	rest, ok := strings.CutPrefix(id, "dec-")
	if !ok || rest == "" {
		return false
	}
	for _, c := range rest {
		if (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// writeDecisionError maps decision-domain failures to status codes:
// unknown subjects and targets are 404 (no tombstones, so no 410);
// matrix and input violations are 400/422; stale-head and
// conflicting-reuse races are 409; replays surface as 200 bodies,
// never errors.
func writeDecisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrIncidentFanoutLimit):
		writeViewerError(w, "Too many members for one action; review member briefing pages individually.", http.StatusRequestEntityTooLarge)
	case errors.Is(err, store.ErrUnknownSubject), errors.Is(err, store.ErrUnknownDecision):
		writeViewerError(w, "unknown decision subject", http.StatusNotFound)
	case errors.Is(err, store.ErrInvalidDecision):
		writeViewerError(w, "invalid decision", http.StatusBadRequest)
	case errors.Is(err, store.ErrRequestConflict), errors.Is(err, store.ErrUndoConflict):
		writeViewerError(w, "decision conflict", http.StatusConflict)
	case errors.Is(err, store.ErrInvalidUndo):
		writeViewerError(w, "invalid undo target", http.StatusUnprocessableEntity)
	case errors.Is(err, store.ErrUnsupportedSubject):
		writeViewerError(w, "unsupported decision", http.StatusUnprocessableEntity)
	default:
		writeError(w, err)
	}
}
