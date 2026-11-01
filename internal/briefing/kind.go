package briefing

import (
	"github.com/ostefani/watcherbridge/internal/store"
)

// Reported-change kinds for digest grouping (Designv9 §3, Plan Step
// 19). Every value describes a reported fact, never an inference: the
// classifier reads event types and previous/current field differences,
// and anything else stays other.
const (
	KindWeakAppearance    = "weak_appearance"
	KindWeakDisappearance = "weak_disappearance"
	KindPortClosed        = "port_closed"
	KindHostnameChange    = "hostname_change"
	KindOSRefinement      = "os_refinement"
	KindOther             = "other"
)

// KindOf classifies a group's reported change from its decoded events
// and the matched baseline rule. Field differences come only from
// host.changed events carrying both previous and current views; the
// baseline rule is consulted solely when no event decoded, so unknown
// input degrades to other (or weak_disappearance for the weak-gone
// rule) instead of inventing a change. Mixed appearance/disappearance
// groups stay other: flap detection is an upstream contract, never a
// bridge inference.
func KindOf(events []GroupEvent, ruleID string) string {
	var appeared, disappeared, changed, decodable bool
	for _, e := range events {
		switch e.EventType {
		case "host.appeared":
			appeared = true
		case "host.disappeared":
			disappeared = true
		case "host.changed":
			changed = true
		}
		if e.Host != nil || e.Current != nil || e.Previous != nil {
			decodable = true
		}
	}
	if !decodable {
		if ruleID == RuleLowWeakGone {
			return KindWeakDisappearance
		}
		return KindOther
	}
	if changed {
		if kind, ok := changedKind(events); ok {
			return kind
		}
		return KindOther
	}
	switch {
	case appeared && !disappeared:
		if allWeak(events) {
			return KindWeakAppearance
		}
		return KindOther
	case disappeared && !appeared:
		if allWeak(events) {
			return KindWeakDisappearance
		}
		return KindOther
	default:
		return KindOther
	}
}

// changedKind reports the first reported field difference across
// host.changed events in fixed precedence: hostname, closed port,
// OS/device refinement. The second return is false when no changed
// event carries a usable previous/current pair with a difference.
func changedKind(events []GroupEvent) (string, bool) {
	var hostname, portClosed, osRefined bool
	for _, e := range events {
		if e.EventType != "host.changed" || e.Previous == nil || e.Current == nil {
			continue
		}
		prev, cur := e.Previous, e.Current
		if prev.Hostname != cur.Hostname && (prev.Hostname != "" || cur.Hostname != "") {
			hostname = true
		}
		if portWasClosed(prev.Ports, cur.Ports) {
			portClosed = true
		}
		if (prev.OS != cur.OS && (prev.OS != "" || cur.OS != "")) ||
			(prev.Device != cur.Device && (prev.Device != "" || cur.Device != "")) {
			osRefined = true
		}
	}
	switch {
	case hostname:
		return KindHostnameChange, true
	case portClosed:
		return KindPortClosed, true
	case osRefined:
		return KindOSRefinement, true
	default:
		return "", false
	}
}

// portWasClosed reports whether a port open in prev is no longer open
// in cur, compared by number and protocol. Non-open states never
// qualify on either side: only a reported open-to-not-open transition
// counts, so filtered noise cannot manufacture a closure.
func portWasClosed(prev, cur []portView) bool {
	type portKey struct {
		number   int
		protocol string
	}
	open := map[portKey]bool{}
	for _, p := range cur {
		if p.State == "open" {
			open[portKey{p.Number, p.Protocol}] = true
		}
	}
	for _, p := range prev {
		if p.State != "open" {
			continue
		}
		if !open[portKey{p.Number, p.Protocol}] {
			return true
		}
	}
	return false
}

// allWeak reports whether every decodable event describes a weak
// observation. Undecodable events abstain rather than vote: a group
// with no decodable view is handled by the rule fallback in KindOf.
func allWeak(events []GroupEvent) bool {
	seen := false
	for _, e := range events {
		v := e.effective()
		if v == nil {
			continue
		}
		seen = true
		if !v.Weak {
			return false
		}
	}
	return seen
}

// KindForTaken classifies a taken group for storage, decoding its
// stored payloads. Undecodable payloads degrade through KindOf's
// unknown-input path instead of failing the briefing.
func KindForTaken(taken store.TakenGroup, ruleID string) string {
	events := make([]GroupEvent, 0, len(taken.Events))
	for _, ev := range taken.Events {
		events = append(events, DecodeEvent(ev.EventType, ev.Payload))
	}
	return KindOf(events, ruleID)
}
