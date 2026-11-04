package viewer

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

// Incident routes (Step 29, Designv9 §7): bounded presentation
// incidents and their members, plus incident subject support in the
// decision routes. Mutations ride guardStateChanging like every other
// POST; incident SSE updates carry no id, so clients refetch the list
// on reconnect instead of replaying.

// incidentWire is one incident as served: correlation key, floor,
// counts, window/quiet state and open/closed status. Counts
// distinguish member briefings from distinct qualified upstream
// references: fewer qualified references than reports means some
// members share identity evidence, and the detail view says so.
type incidentWire struct {
	IncidentID    string `json:"incident_id"`
	Kind          string `json:"kind"` // burst_appearance|burst_disappearance
	SourceScope   string `json:"source_scope"`
	Target        string `json:"target"`
	Status        string `json:"status"` // open|closed
	Floor         string `json:"floor"`
	QualifiedRefs int    `json:"qualified_refs"`
	MemberCount   int    `json:"member_count"`
	WindowStart   string `json:"window_start"`
	LastMemberAt  string `json:"last_member_at"`
	OpenedAt      string `json:"opened_at"`
	ClosedAt      string `json:"closed_at,omitempty"`
	UpdatedAt     string `json:"updated_at"`
	// Cursor continues the list past this row ("<opened nanos>:<rowid>").
	Cursor string `json:"cursor"`
}

func incidentWireOf(in store.IncidentListRow) incidentWire {
	w := incidentWire{
		IncidentID: in.IncidentID, Kind: in.Kind, SourceScope: in.SourceScope,
		Target: in.Target, Status: in.Status, Floor: in.Floor,
		QualifiedRefs: in.QualifiedRefs, MemberCount: in.MemberCount,
		WindowStart:  in.WindowStart.UTC().Format(time.RFC3339),
		LastMemberAt: in.LastMemberAt.UTC().Format(time.RFC3339),
		OpenedAt:     in.OpenedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    in.UpdatedAt.UTC().Format(time.RFC3339),
		Cursor:       strconv.FormatInt(in.CursorNanos, 10) + ":" + strconv.FormatInt(in.CursorRowID, 10),
	}
	if in.HasClosedAt {
		w.ClosedAt = in.ClosedAt.UTC().Format(time.RFC3339)
	}
	return w
}

// memberRefWire is one accept-time qualification snapshot: the exact
// upstream reference under its namespace, revision and reported
// confidence. Opaque IDs cross; raw network attributes never do.
type memberRefWire struct {
	Reference  string `json:"reference"`
	Namespace  string `json:"namespace"`
	Revision   string `json:"revision"`
	Confidence string `json:"confidence"`
}

// incidentMemberWire is one member briefing with its stored evidence:
// completion time, final priority, and the frozen reference snapshot.
type incidentMemberWire struct {
	BriefingID string          `json:"briefing_id"`
	CreatedAt  string          `json:"created_at"`
	Priority   string          `json:"priority"`
	Refs       []memberRefWire `json:"refs"`
}

// summaryWire is one closed-incident AI summary as served: local-only
// presentation that never changes membership. Floor is the deterministic
// member-final floor; suggestion/final/clamped record the resolver
// inputs and outcome separately. Confidence stays display-only.
// Citations name opaque snapshot member aliases; sampled/omitted state
// the coverage limits explicitly. A summary may arrive after close;
// UpdatedAt marks that timing.
type summaryWire struct {
	Status        string                `json:"status"`
	Floor         string                `json:"floor"`
	Suggestion    *string               `json:"suggestion,omitempty"`
	FinalPriority string                `json:"final_priority"`
	Clamped       bool                  `json:"clamped"`
	Summary       string                `json:"summary,omitempty"`
	Hypothesis    string                `json:"hypothesis,omitempty"`
	Confidence    string                `json:"confidence,omitempty"`
	Reason        string                `json:"reason,omitempty"`
	Citations     []summaryCitationWire `json:"citations"`
	Sampled       int                   `json:"sampled"`
	Omitted       int                   `json:"omitted"`
	Error         string                `json:"error,omitempty"`
	UpdatedAt     string                `json:"updated_at"`
}

type summaryCitationWire struct {
	Member   string `json:"member"`
	Briefing string `json:"briefing_id"`
}

// incidentDetailWire is one incident with its bounded member
// evidence. Truncated marks a member section cut at the serving
// bound; the counts on the incident row stay exact. Summary is absent
// for open incidents and when the feature is disabled.
type incidentDetailWire struct {
	Incident        incidentWire         `json:"incident"`
	Members         []incidentMemberWire `json:"members"`
	Truncated       bool                 `json:"truncated"`
	Decisions       []decisionWire       `json:"decisions"`
	EffectiveReview *reviewWire          `json:"effective_review,omitempty"`
	Summary         *summaryWire         `json:"summary,omitempty"`
}

// incidentEvent is the `incident` SSE payload: per-kind change
// counts plus the changed incident IDs in deterministic order,
// capped, without an SSE id — incident updates are never recovered
// through briefing replay, so clients refetch the list on reconnect
// and on every such event.
type incidentEvent struct {
	Created   int      `json:"created"`
	Appended  int      `json:"appended"`
	Closed    int      `json:"closed"`
	Changed   []string `json:"changed"`
	Truncated bool     `json:"truncated"`
}

// IncidentEventOf renders one correlator report for broadcast. Empty
// reports never reach the hub: the loop owner skips quiet passes.
func IncidentEventOf(rep store.IncidentReport) incidentEvent {
	return incidentEvent{
		Created: rep.Created, Appended: rep.Appended, Closed: rep.Closed,
		Changed: rep.Changed, Truncated: rep.Truncated,
	}
}

// validIncidentID allowlists stored incident IDs: inc- plus the
// window nanos and sequence the correlator assigns.
func validIncidentID(id string) bool {
	rest, ok := strings.CutPrefix(id, "inc-")
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

// handleIncidents serves the newest-first incident list. limit
// defaults to 50 and clamps to 200; before continues an earlier page
// as "<opened nanos>:<rowid>". Malformed cursors are 400, never a
// lookup; unknown-but-well-formed ones page to an empty page.
func (v *Viewer) handleIncidents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 || n > 200 {
			writeViewerError(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
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
	rows, err := v.store.ListIncidents(r.Context(), limit, beforeNanos, beforeRowID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]incidentWire, 0, len(rows))
	for _, in := range rows {
		out = append(out, incidentWireOf(in))
	}
	writeJSON(w, out)
}

// handleIncident serves one incident with its bounded member
// evidence. Malformed IDs are 400; unknown IDs are 404 — without
// tombstones a purged incident reads like a typo, never 410.
func (v *Viewer) handleIncident(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || !validIncidentID(id) {
		writeViewerError(w, "invalid id", http.StatusBadRequest)
		return
	}
	detail, err := v.store.IncidentDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeViewerError(w, "unknown incident", http.StatusNotFound)
			return
		}
		writeError(w, err)
		return
	}
	wire := incidentDetailWire{
		Incident:  incidentWireOf(detail.IncidentListRow),
		Members:   make([]incidentMemberWire, 0, len(detail.Members)),
		Truncated: detail.Truncated,
		Decisions: []decisionWire{},
	}
	trail, err := v.store.DecisionsForSubject(r.Context(), store.DecisionSubjectIncident, id, 50, 0, 0)
	if err != nil {
		writeError(w, err)
		return
	}
	for _, d := range trail {
		wire.Decisions = append(wire.Decisions, decisionWireOf(d))
	}
	states, err := v.store.EffectiveReview(r.Context(), store.DecisionSubjectIncident, []string{id}, time.Now())
	if err != nil {
		writeError(w, err)
		return
	}
	if st, ok := states[id]; ok {
		rw := reviewWireOf(st)
		wire.EffectiveReview = &rw
	}
	for _, m := range detail.Members {
		mw := incidentMemberWire{
			BriefingID: m.BriefingID,
			CreatedAt:  m.CreatedAt.UTC().Format(time.RFC3339),
			Priority:   m.Priority,
			Refs:       make([]memberRefWire, 0, len(m.Refs)),
		}
		for _, ref := range m.Refs {
			mw.Refs = append(mw.Refs, memberRefWire{
				Reference: ref.Reference, Namespace: ref.Namespace,
				Revision: ref.Revision, Confidence: ref.Confidence,
			})
		}
		wire.Members = append(wire.Members, mw)
	}
	if sum, err := v.store.GetIncidentSummary(r.Context(), id); err == nil {
		sw := &summaryWire{
			Status: sum.Status, Floor: sum.Floor, Suggestion: sum.Suggestion,
			FinalPriority: sum.FinalPriority, Clamped: sum.Clamped,
			Summary: sum.Summary, Hypothesis: sum.Hypothesis,
			Confidence: sum.Confidence, Reason: sum.Reason,
			Citations: []summaryCitationWire{}, Sampled: sum.Sampled, Omitted: sum.Omitted,
			Error: sum.Error, UpdatedAt: sum.UpdatedAt.UTC().Format(time.RFC3339),
		}
		for _, c := range sum.Citations {
			sw.Citations = append(sw.Citations, summaryCitationWire{Member: c.Member, Briefing: sum.AliasMap[c.Member]})
		}
		wire.Summary = sw
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, err)
		return
	}
	writeJSON(w, wire)
}
