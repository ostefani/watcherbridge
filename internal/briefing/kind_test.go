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
			[]GroupEvent{changedEvent(hostView{Ports: []portView{open22, open80}}, hostView{Ports: []portView{open80}})},
			RuleDefault, KindPortClosed,
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
