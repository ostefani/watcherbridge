package briefing

import (
	"encoding/json"
	"testing"

	"github.com/ostefani/watcherbridge/internal/store"
)

func kindPayload(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func changedEvent(prev, cur hostView) GroupEvent {
	return GroupEvent{
		EventType: "host.changed",
		Previous:  &prev,
		Current:   &cur,
	}
}

func TestKindOf(t *testing.T) {
	weak := hostView{Weak: true}
	strong := hostView{Weak: false}
	open22 := portView{Number: 22, Protocol: "tcp", State: "open"}
	open80 := portView{Number: 80, Protocol: "tcp", State: "open"}
	filtered := portView{Number: 22, Protocol: "tcp", State: "filtered"}

	for _, tc := range []struct {
		name   string
		events []GroupEvent
		rule   string
		want   string
	}{
		{"appeared weak", []GroupEvent{{EventType: "host.appeared", Host: &weak}}, RuleDefault, KindWeakAppearance},
		{"appeared strong", []GroupEvent{{EventType: "host.appeared", Host: &strong}}, RuleMedNewIdentity, KindOther},
		{"appeared mixed weak", []GroupEvent{{EventType: "host.appeared", Host: &weak}, {EventType: "host.appeared", Host: &strong}}, RuleDefault, KindOther},
		{"disappeared weak", []GroupEvent{{EventType: "host.disappeared", Host: &weak}}, RuleLowWeakGone, KindWeakDisappearance},
		{"disappeared strong", []GroupEvent{{EventType: "host.disappeared", Host: &strong}}, RuleMedGoneAfterHistory, KindOther},
		{"flap stays other", []GroupEvent{{EventType: "host.appeared", Host: &weak}, {EventType: "host.disappeared", Host: &weak}}, RuleDefault, KindOther},
		{
			"hostname change",
			[]GroupEvent{changedEvent(hostView{Hostname: "a"}, hostView{Hostname: "b"})},
			RuleDefault, KindHostnameChange,
		},
		{
			"hostname unknown to known",
			[]GroupEvent{changedEvent(hostView{}, hostView{Hostname: "b"})},
			RuleDefault, KindHostnameChange,
		},
		{
			"port closed",
			[]GroupEvent{changedEvent(
				hostView{Ports: []portView{open22, open80}},
				hostView{Ports: []portView{open80, {Number: 22, Protocol: "tcp", State: "closed"}}})},
			RuleDefault, KindPortClosed,
		},
		{
			"port vanished without closed state is unknown",
			[]GroupEvent{changedEvent(hostView{Ports: []portView{open22, open80}}, hostView{Ports: []portView{open80}})},
			RuleDefault, KindOther,
		},
		{
			"port opened only",
			[]GroupEvent{changedEvent(hostView{Ports: []portView{open80}}, hostView{Ports: []portView{open80, open22}})},
			RuleDefault, KindOther,
		},
		{
			"filtered noise is not a closure",
			[]GroupEvent{changedEvent(hostView{Ports: []portView{filtered}}, hostView{})},
			RuleDefault, KindOther,
		},
		{
			"os refinement",
			[]GroupEvent{changedEvent(hostView{OS: "Linux"}, hostView{OS: "Debian 12"})},
			RuleDefault, KindOSRefinement,
		},
		{
			"device refinement",
			[]GroupEvent{changedEvent(hostView{}, hostView{Device: "printer"})},
			RuleDefault, KindOSRefinement,
		},
		{
			"changed without differences",
			[]GroupEvent{changedEvent(hostView{Hostname: "a"}, hostView{Hostname: "a"})},
			RuleDefault, KindOther,
		},
		{
			"hostname beats port and os",
			[]GroupEvent{changedEvent(
				hostView{Hostname: "a", OS: "Linux", Ports: []portView{open22}},
				hostView{Hostname: "b", OS: "Debian", Ports: []portView{}})},
			RuleDefault, KindHostnameChange,
		},
		{"undecodable weak-gone rule", []GroupEvent{{EventType: "host.disappeared"}}, RuleLowWeakGone, KindWeakDisappearance},
		{"undecodable default rule", []GroupEvent{{EventType: "host.appeared"}}, RuleDefault, KindOther},
		{"empty other", nil, RuleDefault, KindOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := KindOf(tc.events, tc.rule); got != tc.want {
				t.Fatalf("KindOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// Closure requires positive evidence: a previously open port that is
// filtered, unstated, or missing from the current view is unknown,
// not closed. Only an explicit current closed state counts.
func TestKindNeedsPositiveClosureEvidence(t *testing.T) {
	changed := func(prevPorts, curPorts string) store.TakenGroup {
		payload := kindPayload(map[string]any{
			"previous": map[string]any{"hostname": "a", "ports": mustPorts(t, prevPorts)},
			"current":  map[string]any{"hostname": "a", "ports": mustPorts(t, curPorts)},
		})
		return store.TakenGroup{
			GroupID: "grp-kind",
			Anchor:  "mac:aa",
			Events:  []store.RawEvent{{EventID: "e1", EventType: "host.changed", Payload: payload}},
		}
	}
	open := `[{"number":22,"protocol":"tcp","state":"open"}]`
	for _, tc := range []struct {
		name string
		cur  string
		want string
	}{
		{"open to filtered is unknown", `[{"number":22,"protocol":"tcp","state":"filtered"}]`, KindOther},
		{"open to unstated is unknown", `[{"number":22,"protocol":"tcp","state":""}]`, KindOther},
		{"open to missing ports is unknown", `null`, KindOther},
		{"open to closed is a closure", `[{"number":22,"protocol":"tcp","state":"closed"}]`, KindPortClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := KindForTaken(changed(open, tc.cur), RuleDefault); got != tc.want {
				t.Fatalf("KindForTaken = %q, want %q", got, tc.want)
			}
		})
	}
}

func mustPorts(t *testing.T, raw string) any {
	t.Helper()
	if raw == "null" {
		return nil
	}
	var out []any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The producer's changes[] is authoritative for what changed: a
// cosmetic hostname variation the producer ignored must not outrank
// a real reported port change, and a reported hostname change stands
// on the producer's comparison, not a reimplementation of it.
func TestKindPrefersReportedChanges(t *testing.T) {
	changedWith := func(prevHost, curHost map[string]any, changes string) store.TakenGroup {
		var ch []any
		if err := json.Unmarshal([]byte(changes), &ch); err != nil {
			t.Fatal(err)
		}
		payload := kindPayload(map[string]any{"previous": prevHost, "current": curHost, "changes": ch})
		return store.TakenGroup{
			GroupID: "grp-kind",
			Anchor:  "mac:aa",
			Events:  []store.RawEvent{{EventID: "e1", EventType: "host.changed", Payload: payload}},
		}
	}
	host := func(hostname string, ports string) map[string]any {
		return map[string]any{"hostname": hostname, "ports": mustPorts(t, ports)}
	}
	open := `[{"number":22,"protocol":"tcp","state":"open"}]`
	closed := `[{"number":22,"protocol":"tcp","state":"closed"}]`
	t.Run("reported ports beat cosmetic hostname", func(t *testing.T) {
		taken := changedWith(host("Printer", open), host("printer.", closed),
			`[{"field":"ports","before":"22/tcp(open)","after":"22/tcp(closed)"}]`)
		if got := KindForTaken(taken, RuleDefault); got != KindPortClosed {
			t.Fatalf("KindForTaken = %q, want port_closed", got)
		}
	})
	t.Run("reported hostname stands", func(t *testing.T) {
		taken := changedWith(host("a", open), host("b", open),
			`[{"field":"hostname","before":"a","after":"b"}]`)
		if got := KindForTaken(taken, RuleDefault); got != KindHostnameChange {
			t.Fatalf("KindForTaken = %q, want hostname_change", got)
		}
	})
	t.Run("legacy payload keeps raw comparison", func(t *testing.T) {
		payload := kindPayload(map[string]any{
			"previous": host("Printer", open),
			"current":  host("printer.", open),
		})
		taken := store.TakenGroup{
			GroupID: "grp-kind",
			Anchor:  "mac:aa",
			Events:  []store.RawEvent{{EventID: "e1", EventType: "host.changed", Payload: payload}},
		}
		if got := KindForTaken(taken, RuleDefault); got != KindHostnameChange {
			t.Fatalf("KindForTaken = %q, want hostname_change", got)
		}
	})
}

func TestKindForTakenDecodesPayloads(t *testing.T) {
	payload := kindPayload(map[string]any{
		"previous": map[string]any{"hostname": "a", "ports": []any{}},
		"current":  map[string]any{"hostname": "b", "ports": []any{}},
	})
	taken := store.TakenGroup{
		GroupID: "grp-kind",
		Anchor:  "mac:aa",
		Events:  []store.RawEvent{{EventID: "e1", EventType: "host.changed", Payload: payload}},
	}
	if got := KindForTaken(taken, RuleDefault); got != KindHostnameChange {
		t.Fatalf("KindForTaken = %q, want hostname_change", got)
	}
}
