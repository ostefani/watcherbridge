// Package briefing is Step 9: the grouper tick worker, the deterministic
// priority baseline, and the raise-only resolver (Designv5 §§8, 10).
//
// Baseline inputs are typed host views decoded from stored webhook
// payloads — never banners, hostnames, or vendor text. Any rule whose
// required input is missing or unparseable is skipped and counted; unknown
// input never yields critical, only the default low.
package briefing

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Policy carries the baseline knobs. Ages apply to first_strong_seen_at
// once Pro ships it; until then the age clauses pass vacuously and the
// exact signals (tracked-ness, ever-strongly-seen) decide.
type Policy struct {
	EstablishedAge   time.Duration
	StrongHistoryAge time.Duration
	StableFields     []string
}

type portView struct {
	Number   int    `json:"number"`
	Protocol string `json:"protocol"`
	State    string `json:"state"`
	Service  string `json:"service"`
}

type hostView struct {
	IP                 string     `json:"ip"`
	Hostname           string     `json:"hostname"`
	MAC                string     `json:"mac"`
	RandomizedMAC      bool       `json:"randomized_mac"`
	OS                 string     `json:"os"`
	Device             string     `json:"device"`
	Vendor             string     `json:"vendor"`
	Weak               bool       `json:"weak"`
	Alive              bool       `json:"alive"`
	IdentityConfidence string     `json:"identity_confidence"`
	LastStrongSeenAt   time.Time  `json:"last_strong_seen_at"`
	FirstStrongSeenAt  time.Time  `json:"first_strong_seen_at"`
	Ports              []portView `json:"ports"`
}

// GroupEvent is one taken event with its payload decoded. Type comes from
// the store column (allowlisted at ingest); host views are nil when the
// stored payload does not decode — that event then takes the unknown-input
// path instead of failing the group.
type GroupEvent struct {
	EventType string
	Host      *hostView
	Current   *hostView
	Previous  *hostView
}

// DecodeEvent decodes one stored payload. Undecodable payloads yield views
// of nil so the group degrades to unknown-input instead of erroring.
func DecodeEvent(eventType string, payload []byte) GroupEvent {
	ev := GroupEvent{EventType: eventType}
	var body struct {
		Host     *hostView `json:"host"`
		Current  *hostView `json:"current"`
		Previous *hostView `json:"previous"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return ev
	}
	ev.Host, ev.Current, ev.Previous = body.Host, body.Current, body.Previous
	return ev
}

// effective returns the host view that describes the event outcome.
func (e GroupEvent) effective() *hostView {
	if e.Current != nil {
		return e.Current
	}
	if e.Host != nil {
		return e.Host
	}
	return e.Previous
}

// Outcome is a baseline decision: the level, the matched rule ID, and the
// rule IDs skipped for unknown input.
type Outcome struct {
	Level   Level
	RuleID  string
	Skipped []string
}

// Rule IDs. Only CRIT_IDENTITY_SERVICE may produce critical.
const (
	RuleCritIdentityService = "CRIT_IDENTITY_SERVICE"
	RuleHighNewService      = "HIGH_NEW_SERVICE"
	RuleHighMultiSensitive  = "HIGH_MULTI_SENSITIVE"
	RuleMedNewIdentity      = "MED_NEW_IDENTITY"
	RuleMedGoneAfterHistory = "MED_GONE_AFTER_HISTORY"
	RuleLowWeakGone         = "LOW_WEAK_GONE"
	RuleLowCosmetic         = "LOW_COSMETIC"
	RuleDefault             = "DEFAULT"
)

// Baseline resolves the deterministic floor for a group. asOf is the
// group's last event time — never wall clock — so the same group always
// yields the same result. It errors only when the group holds no events;
// undecodable payloads degrade to the unknown-input path (default low).
func Baseline(events []GroupEvent, asOf time.Time, pol Policy) (Outcome, error) {
	if len(events) == 0 {
		return Outcome{}, fmt.Errorf("baseline needs at least one event")
	}
	var skipped []string

	appeared, disappeared := false, false
	unknownHost := false
	for _, e := range events {
		switch e.EventType {
		case "host.appeared":
			appeared = true
			if e.Host == nil {
				unknownHost = true
			}
		case "host.disappeared":
			disappeared = true
			if e.Host == nil {
				unknownHost = true
			}
		case "host.changed":
			if e.Current == nil && e.Previous == nil {
				unknownHost = true
			}
		}
	}

	subject := events[len(events)-1].effective()
	highConf := subject != nil && subject.IdentityConfidence == "high"
	// Pro emits changed/disappeared only for hosts already in its tracker,
	// so a group without appeared is established — exact, no history needed.
	established := !appeared
	firstSeen := time.Time{}
	for _, e := range events {
		if h := e.effective(); h != nil && !h.FirstStrongSeenAt.IsZero() {
			if firstSeen.IsZero() || h.FirstStrongSeenAt.Before(firstSeen) {
				firstSeen = h.FirstStrongSeenAt
			}
		}
	}
	ageOK := func(age time.Duration) bool {
		// Dormant until Pro ships first_strong_seen_at: unknown age does
		// not narrow, so current inputs decide on their own.
		return firstSeen.IsZero() || !asOf.Before(firstSeen.Add(age))
	}
	established = established && ageOK(pol.EstablishedAge)
	everStrong := false
	for _, e := range events {
		if h := e.effective(); h != nil && !h.LastStrongSeenAt.IsZero() {
			everStrong = true
			break
		}
	}
	strongHistory := everStrong && ageOK(pol.StrongHistoryAge)

	// Unknown-input accounting. unknownHost (any event without a usable
	// host view) degrades the whole group: every rule is skipped. A
	// single-sided changed pair (only previous or only current) leaves the
	// pair-diff rules (critical, high) without the data they need; an
	// asOf-less evaluation with first-seen data leaves age gates unknown.
	// Gate-false is not a skip — only unknown input is counted.
	if unknownHost {
		skipped = append(skipped, RuleCritIdentityService, RuleHighNewService, RuleHighMultiSensitive, RuleMedNewIdentity, RuleMedGoneAfterHistory, RuleLowWeakGone, RuleLowCosmetic)
		return Outcome{Level: Low, RuleID: RuleDefault, Skipped: skipped}, nil
	}
	pairDiffs, singleSided := false, false
	for _, e := range events {
		if e.EventType != "host.changed" {
			continue
		}
		switch {
		case e.Current != nil && e.Previous != nil:
			pairDiffs = true
		case e.Current != nil || e.Previous != nil:
			singleSided = true
		}
	}
	if singleSided && !pairDiffs {
		skipped = append(skipped, RuleCritIdentityService, RuleHighNewService, RuleHighMultiSensitive)
	}
	if !firstSeen.IsZero() && asOf.IsZero() {
		skipped = append(skipped, RuleCritIdentityService, RuleHighNewService, RuleHighMultiSensitive, RuleMedGoneAfterHistory)
	}

	stableSet := map[string]bool{}
	for _, f := range pol.StableFields {
		stableSet[f] = true
	}
	stable := true
	osDevReplaced, osDevRefined := false, false
	macChanged, identDowngrade := false, false
	onlyHostname := true
	weakOnly := true
	opensBefore := map[string]bool{}
	newOpens := map[string]bool{}
	changedPairs := 0
	for _, e := range events {
		h := e.effective()
		if h == nil {
			// Unknown host input is handled below; it can neither prove
			// weakness nor cosmesis.
			weakOnly = false
			onlyHostname = false
			continue
		}
		if !h.Weak {
			weakOnly = false
		}
		if e.EventType != "host.changed" || e.Current == nil || e.Previous == nil {
			if e.EventType != "host.changed" {
				onlyHostname = false
			} else {
				// Changed without both sides: cannot prove hostname-only.
				onlyHostname = false
			}
			if e.EventType == "host.appeared" {
				for _, p := range h.Ports {
					if p.State == "open" {
						newOpens[portKey(p)] = true
					}
				}
			}
			continue
		}
		changedPairs++
		cur, prev := e.Current, e.Previous
		if prev.OS != cur.OS || prev.Device != cur.Device {
			if (prev.OS != "" && cur.OS != "" && prev.OS != cur.OS) ||
				(prev.Device != "" && cur.Device != "" && prev.Device != cur.Device) {
				osDevReplaced = true
			} else {
				osDevRefined = true
			}
		}
		if normMAC(prev.MAC) != normMAC(cur.MAC) {
			macChanged = true
		}
		if prev.IdentityConfidence == "high" && cur.IdentityConfidence == "low" {
			identDowngrade = true
		}
		// Hostname-only: this pair differs in hostname and in nothing else.
		pairSame := prev.IP == cur.IP && prev.MAC == cur.MAC && prev.OS == cur.OS &&
			prev.Device == cur.Device && prev.Vendor == cur.Vendor &&
			prev.IdentityConfidence == cur.IdentityConfidence && prev.Weak == cur.Weak &&
			prev.Alive == cur.Alive && portsEqual(prev.Ports, cur.Ports)
		onlyHostname = onlyHostname && prev.Hostname != cur.Hostname && pairSame
		for _, p := range prev.Ports {
			if p.State == "open" {
				opensBefore[portKey(p)] = true
			}
		}
		for _, p := range cur.Ports {
			if p.State == "open" && !opensBefore[portKey(p)] {
				newOpens[portKey(p)] = true
			}
		}
		// Unrolled per-pair differ map: the stable fields are fixed, so
		// there is no map to allocate per pair. host_id and
		// identity_source are not in the decoded view; identity moves
		// surface via the fields below.
		if prev.Hostname != cur.Hostname && stableSet["hostname"] {
			stable = false
		}
		if normMAC(prev.MAC) != normMAC(cur.MAC) && stableSet["mac"] {
			stable = false
		}
		if prev.IdentityConfidence != cur.IdentityConfidence && stableSet["identity_confidence"] {
			stable = false
		}
	}
	if changedPairs == 0 {
		onlyHostname = false
	}

	// 1. Critical conjunction: os/device replacement plus a new exposed
	// service on a stable, high-confidence, established host. Gate-false is
	// a non-match, not a skip — unknown input is recorded above.
	if osDevReplaced && len(newOpens) > 0 && stable && highConf && established {
		return Outcome{Level: Critical, RuleID: RuleCritIdentityService, Skipped: skipped}, nil
	}

	// 3. High: a new open service on an established stable host, or two or
	// more distinct sensitive-change kinds on an established host. The
	// second disjunct deliberately carries no stability gate: identity churn
	// plus multiple sensitive changes is itself the urgency.
	if len(newOpens) > 0 && established && stable {
		return Outcome{Level: High, RuleID: RuleHighNewService, Skipped: skipped}, nil
	}
	if sensitiveKinds(osDevReplaced, macChanged, identDowngrade) >= 2 && established {
		return Outcome{Level: High, RuleID: RuleHighMultiSensitive, Skipped: skipped}, nil
	}

	for _, e := range events {
		if e.EventType == "host.appeared" && e.Host != nil && e.Host.IdentityConfidence == "high" && !e.Host.Weak {
			return Outcome{Level: Medium, RuleID: RuleMedNewIdentity, Skipped: skipped}, nil
		}
	}

	for _, e := range events {
		if e.EventType == "host.disappeared" && established && strongHistory && !weakOnly {
			return Outcome{Level: Medium, RuleID: RuleMedGoneAfterHistory, Skipped: skipped}, nil
		}
	}

	if disappeared && weakOnly {
		return Outcome{Level: Low, RuleID: RuleLowWeakGone, Skipped: skipped}, nil
	}

	if onlyHostname || (len(opensBefore) > 0 && len(newOpens) == 0) || osDevRefined {
		return Outcome{Level: Low, RuleID: RuleLowCosmetic, Skipped: skipped}, nil
	}

	return Outcome{Level: Low, RuleID: RuleDefault, Skipped: skipped}, nil
}

func portsEqual(a, b []portView) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sensitiveKinds(osDev, mac, downgrade bool) int {
	n := 0
	for _, b := range []bool{osDev, mac, downgrade} {
		if b {
			n++
		}
	}
	return n
}

func portKey(p portView) string {
	return p.Protocol + "/" + strconv.Itoa(p.Number)
}

func normMAC(m string) string {
	out := make([]byte, 0, len(m))
	for i := 0; i < len(m); i++ {
		c := m[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c == '-' {
			c = ':'
		}
		out = append(out, c)
	}
	return string(out)
}
