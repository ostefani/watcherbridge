// Package overview is Step 20: the pure overview, coverage, and window
// computation behind GET /api/overview, GET /api/health, and
// `watcherbridge status` (Designv9 §3). Every function here is pure —
// items, coverage, and the clock enter as arguments — so the verdict
// matrix is fully testable, including flagged, acknowledged, snoozed,
// and incident behavior that production leaves empty until Steps 25-29.
package overview

import (
	"fmt"
	"sort"
	"time"
)

// Verdicts. Quiet always carries counts, window, and last reported
// change; it never means the network is secure, all hosts are online,
// or the scanner observed no activity.
const (
	VerdictAct    = "act"
	VerdictReview = "review"
	VerdictQuiet  = "quiet"
)

// Priorities in ascending rank. Unknown strings rank below low: an
// unrecognized priority degrades to routine, never to attention.
const (
	PriorityLow      = "low"
	PriorityMedium   = "medium"
	PriorityHigh     = "high"
	PriorityCritical = "critical"
)

// Item is one attention candidate: a briefing or a presentation
// incident. Flagged, Acknowledged, and SnoozedUntil come from local
// decisions; production leaves them unset until Steps 25-29, and tests
// exercise them through synthetic inputs.
type Item struct {
	ID           string // stable tiebreaker
	Incident     bool   // incident before briefing on ties
	Priority     string
	CreatedAt    time.Time
	ChangeKind   string
	Flagged      bool
	Acknowledged bool
	SnoozedUntil time.Time // zero means not snoozed
}

// Item statuses from Designv9 §3.1.
const (
	StatusAttention    = "attention"
	StatusRoutine      = "routine"
	StatusAcknowledged = "acknowledged"
	StatusSnoozed      = "snoozed"
	StatusFlagged      = "flagged" // flagged outranks snooze: it floors review
)

// CandidateCap bounds the attention-candidate read: ordering and the
// visible-7 cut happen in the pure computation, never in the database.
const CandidateCap = 500

// VisibleAttention is the default visible attention count.
const VisibleAttention = 7

// rank orders priorities; unknown strings rank below low.
func rank(p string) int {
	switch p {
	case PriorityCritical:
		return 3
	case PriorityHigh:
		return 2
	case PriorityMedium:
		return 1
	case PriorityLow:
		return 0
	default:
		return -1
	}
}

// statusOf derives one item's status. Flagged outranks everything —
// even a snoozed item still floors review — while acknowledgement and
// an unexpired snooze remove an item from attention.
func statusOf(it Item, asOf time.Time) string {
	if it.Flagged {
		return StatusFlagged
	}
	if !it.SnoozedUntil.IsZero() && asOf.Before(it.SnoozedUntil) {
		return StatusSnoozed
	}
	if it.Acknowledged {
		return StatusAcknowledged
	}
	if rank(it.Priority) >= 1 {
		return StatusAttention
	}
	return StatusRoutine
}

// AttentionItem pairs an item with its derived status for the wire.
type AttentionItem struct {
	Item
	Status string
}

// Coverage carries the assessed coverage and liveness with per-source
// reasons. Liveness is unknown until heartbeat capability exists
// (Step 21); a quiet event stream never implies host presence.
type Coverage struct {
	Status  string // ok|degraded|blind
	Sources []SourceCoverage
	Reasons []string
}

// SourceCoverage tracks one authenticated source independently: one
// healthy source never masks a silent one.
type SourceCoverage struct {
	Scope    string
	Status   string // ok|degraded|blind
	Liveness string // unknown until heartbeat capability exists
	Reasons  []string
}

// CoverageInput feeds AssessCoverage. Heartbeat fields stay unset until
// Step 21; missing heartbeat capability alone never implies blind.
type CoverageInput struct {
	Sources []SourceInput
	// FallbackRatio is the fallback-briefing share over >=3 briefings in
	// the 1h health window, computed from retained rows; -1 when unknown.
	FallbackRatio     float64
	FallbackBriefings int
	RedactionBlocked  bool
	ParkedGroups      int
	ForwardErrors     int
	WriterSaturated   bool
	AsOf              time.Time
}

// SourceInput is one source's health evidence.
type SourceInput struct {
	Scope string
	// HeartbeatCapable is false until H1 (Step 21).
	HeartbeatCapable bool
	HeartbeatStale   bool
	// LastReportAge is informational: silence alone is not a gap.
	LastReportAge time.Duration
	DeliveryGap   bool
}

// Coverage statuses.
const (
	CoverageOK       = "ok"
	CoverageDegraded = "degraded"
	CoverageBlind    = "blind"
	LivenessUnknown  = "unknown"
)

// AssessCoverage derives coverage from health evidence. Blind requires
// heartbeat capability plus a stale heartbeat: without H1 it cannot
// trigger from silence. Any fallback overload, redaction block, parked
// group, forwarding error, reported delivery gap, or writer saturation
// degrades.
func AssessCoverage(in CoverageInput) Coverage {
	cov := Coverage{Status: CoverageOK}
	for _, s := range in.Sources {
		sc := SourceCoverage{Scope: s.Scope, Status: CoverageOK, Liveness: LivenessUnknown}
		if s.HeartbeatCapable && s.HeartbeatStale {
			sc.Status = CoverageBlind
			sc.Reasons = append(sc.Reasons, "heartbeat stale")
		}
		if s.DeliveryGap {
			if sc.Status == CoverageOK {
				sc.Status = CoverageDegraded
			}
			sc.Reasons = append(sc.Reasons, "reported delivery gap")
		}
		cov.Sources = append(cov.Sources, sc)
		if sc.Status == CoverageBlind {
			cov.Status = CoverageBlind
		} else if sc.Status == CoverageDegraded && cov.Status == CoverageOK {
			cov.Status = CoverageDegraded
		}
	}
	var degraded []string
	if in.FallbackBriefings >= 3 && in.FallbackRatio >= 0.5 {
		degraded = append(degraded, "fallback ratio high")
	}
	if in.RedactionBlocked {
		degraded = append(degraded, "redaction blocked provider calls")
	}
	if in.ParkedGroups > 0 {
		degraded = append(degraded, "parked groups need operator review")
	}
	if in.ForwardErrors > 0 {
		degraded = append(degraded, "forwarding delivery errors")
	}
	if in.WriterSaturated {
		degraded = append(degraded, "writer saturated")
	}
	if len(degraded) > 0 {
		cov.Reasons = append(cov.Reasons, degraded...)
		if cov.Status == CoverageOK {
			cov.Status = CoverageDegraded
		}
	}
	return cov
}

// DigestKinds are the reported-change kinds the digest groups by.
var DigestKinds = []string{
	"weak_appearance", "weak_disappearance", "port_closed",
	"hostname_change", "os_refinement", "other",
}

// Overview is the shared computation behind the API and the CLI.
type Overview struct {
	Verdict    string
	Headline   string
	Attention  []AttentionItem
	Digest     map[string]int
	Receipts   int
	Window     time.Duration
	LastChange time.Time
	Coverage   Coverage
}

// Compute derives the verdict, headline, attention list, and digest.
// Digest counts enter pre-aggregated; attention holds at most the
// visible items in Designv9 §3.1 order: flagged, priority descending,
// incident before briefing, newest, stable ID tiebreaker.
func Compute(briefings, incidents []Item, digest map[string]int, receipts int, window time.Duration, lastChange time.Time, cov Coverage, asOf time.Time) Overview {
	var flagged, attention []AttentionItem
	for _, it := range briefings {
		it.Incident = false
		if a, ok := attend(it, asOf); ok {
			if a.Status == StatusFlagged {
				flagged = append(flagged, a)
			} else {
				attention = append(attention, a)
			}
		}
	}
	for _, it := range incidents {
		it.Incident = true
		if a, ok := attend(it, asOf); ok {
			if a.Status == StatusFlagged {
				flagged = append(flagged, a)
			} else {
				attention = append(attention, a)
			}
		}
	}
	sort.SliceStable(flagged, func(i, j int) bool { return lessAttention(flagged[i], flagged[j]) })
	sort.Slice(attention, func(i, j int) bool { return lessAttention(attention[i], attention[j]) })
	// Flagged first in stable order, then attention in order.
	visible := append(flagged, attention...)
	if len(visible) > VisibleAttention {
		visible = visible[:VisibleAttention]
	}
	out := Overview{
		Attention:  visible,
		Digest:     zeroDigest(),
		Receipts:   receipts,
		Window:     window,
		LastChange: lastChange,
		Coverage:   cov,
	}
	for k, n := range digest {
		out.Digest[k] = n
	}
	out.Verdict = VerdictQuiet
	out.Headline = "Nothing needs review"
	if n := len(flagged) + len(attention); n > 0 {
		out.Verdict = VerdictReview
		out.Headline = fmt.Sprintf("%d things to review", n)
		for _, a := range attention {
			// Act needs attention-status high/critical: flagged items
			// floor review and never escalate it on their own.
			if rank(a.Priority) >= 2 {
				out.Verdict = VerdictAct
				out.Headline = fmt.Sprintf("%d need a check now", n)
				break
			}
		}
	}
	return out
}

// attend reports whether an item belongs in the attention list.
func attend(it Item, asOf time.Time) (AttentionItem, bool) {
	st := statusOf(it, asOf)
	if st != StatusAttention && st != StatusFlagged {
		return AttentionItem{}, false
	}
	return AttentionItem{Item: it, Status: st}, true
}

// lessAttention orders flagged first, then priority descending, incident
// before briefing, newest first, stable ID tiebreaker.
func lessAttention(a, b AttentionItem) bool {
	if (a.Status == StatusFlagged) != (b.Status == StatusFlagged) {
		return a.Status == StatusFlagged
	}
	if rank(a.Priority) != rank(b.Priority) {
		return rank(a.Priority) > rank(b.Priority)
	}
	if a.Incident != b.Incident {
		return a.Incident
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID < b.ID
}

// zeroDigest keeps every reported-change kind present for a stable wire,
// even when the window holds no briefings.
func zeroDigest() map[string]int {
	out := make(map[string]int, len(DigestKinds))
	for _, k := range DigestKinds {
		out[k] = 0
	}
	return out
}

// DefaultWindow is the default overview window; DefaultMaxWindow is the
// configured maximum. Both are validated against briefing retention.
const (
	DefaultWindow    = 24 * time.Hour
	DefaultMaxWindow = 168 * time.Hour
)

// ValidateWindow shares the API and CLI contract: default 24h, reject
// invalid or oversized windows, and require window <= max <= retention.
func ValidateWindow(window, max, retention time.Duration) (time.Duration, error) {
	if window <= 0 {
		return 0, fmt.Errorf("invalid window %v: must be positive", window)
	}
	if max <= 0 {
		return 0, fmt.Errorf("invalid maximum window %v: must be positive", max)
	}
	if retention <= 0 {
		return 0, fmt.Errorf("invalid retention %v: must be positive", retention)
	}
	if max > retention {
		return 0, fmt.Errorf("maximum window %v exceeds briefing retention %v", max, retention)
	}
	if window > max {
		return 0, fmt.Errorf("window %v exceeds maximum %v", window, max)
	}
	return window, nil
}

// ExitCodeFor maps a verdict to the `status` exit code. Failures to
// obtain status never use these codes; they exit 3 instead.
func ExitCodeFor(verdict string) int {
	switch verdict {
	case VerdictAct:
		return 2
	case VerdictReview:
		return 1
	default:
		return 0
	}
}
