package briefing

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

var testPolicy = Policy{
	EstablishedAge:   time.Hour,
	StrongHistoryAge: 24 * time.Hour,
	StableFields:     []string{"mac", "hostname", "host_id", "identity_confidence", "identity_source"},
}

func q(s string) string { return strconv.Quote(s) }

func host(m map[string]string, ports string) string {
	seen := "null"
	if m["seen"] != "" {
		seen = q(m["seen"])
	}
	return `{"ip":` + q(m["ip"]) + `,"hostname":` + q(m["hostname"]) + `,"mac":` + q(m["mac"]) +
		`,"os":` + q(m["os"]) + `,"device":` + q(m["device"]) + `,"vendor":` + q(m["vendor"]) +
		`,"weak":` + m["weak"] + `,"alive":true,"identity_confidence":` + q(m["conf"]) +
		`,"last_strong_seen_at":` + seen + `,"ports":[` + ports + `]}`
}

func openPort(n int) string {
	return `{"number":` + itoa(n) + `,"protocol":"tcp","state":"open"}`
}

func changedEv(prev, cur string) GroupEvent {
	return DecodeEvent("host.changed", []byte(`{"event_id":"e1","previous":`+prev+`,"current":`+cur+`}`))
}

func appearedEv(h string) GroupEvent {
	return DecodeEvent("host.appeared", []byte(`{"event_id":"e1","host":`+h+`}`))
}

func disappearedEv(h string) GroupEvent {
	return DecodeEvent("host.disappeared", []byte(`{"event_id":"e1","host":`+h+`}`))
}

func strongBase(extra string) map[string]string {
	return map[string]string{"ip": "192.168.0.5", "hostname": "printer", "mac": "aa:bb:cc:dd:ee:01", "weak": "false", "conf": "high", "seen": "2026-10-20T00:00:00Z"}
}

func TestBaselineMatrix(t *testing.T) {
	asOf := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	stable := strongBase("")
	stableNoPorts := host(stable, "")

	cases := map[string]struct {
		events []GroupEvent
		level  Level
		rule   string
	}{
		"critical os replacement plus new service": {
			[]GroupEvent{changedEv(host(mergeHost(stable, map[string]string{"os": "Debian 11"}), ""), host(mergeHost(stable, map[string]string{"os": "Debian 12"}), openPort(80)))},
			Critical, RuleCritIdentityService,
		},
		"stable new open port is high": {
			[]GroupEvent{changedEv(stableNoPorts, host(stable, openPort(8080)))},
			High, RuleHighNewService,
		},
		"new open on unstable host skips high": {
			[]GroupEvent{changedEv(stableNoPorts, host(mergeHost(stable, map[string]string{"hostname": "other"}), openPort(8080)))},
			Low, RuleDefault,
		},
		"two sensitive kinds without opens is high": {
			[]GroupEvent{changedEv(host(mergeHost(stable, map[string]string{"os": "Debian 11"}), ""), host(mergeHost(stable, map[string]string{"os": "Debian 12", "mac": "aa:bb:cc:dd:ee:02"}), ""))},
			High, RuleHighNewService,
		},
		"new strong identity is medium": {
			[]GroupEvent{appearedEv(host(strongBase(""), openPort(443)))},
			Medium, RuleMedNewIdentity,
		},
		"weak appearance is not medium": {
			[]GroupEvent{appearedEv(host(mergeHost(strongBase(""), map[string]string{"weak": "true", "conf": "low"}), ""))},
			Low, RuleDefault,
		},
		"established disappearance with history is medium": {
			[]GroupEvent{disappearedEv(stableNoPorts)},
			Medium, RuleMedGoneAfterHistory,
		},
		"weak-only disappearance is low": {
			[]GroupEvent{disappearedEv(host(mergeHost(strongBase(""), map[string]string{"weak": "true", "conf": "low", "seen": ""}), ""))},
			Low, RuleLowWeakGone,
		},
		"hostname-only change is low": {
			[]GroupEvent{changedEv(stableNoPorts, host(mergeHost(stable, map[string]string{"hostname": "renamed"}), ""))},
			Low, RuleLowCosmetic,
		},
		"port closed is low": {
			[]GroupEvent{changedEv(host(stable, openPort(22)), stableNoPorts)},
			Low, RuleLowCosmetic,
		},
		"os refinement is low": {
			[]GroupEvent{changedEv(host(stable, ""), host(mergeHost(stable, map[string]string{"os": "Debian 12"}), ""))},
			Low, RuleLowCosmetic,
		},
		"os replacement without opens is default low": {
			[]GroupEvent{changedEv(host(mergeHost(stable, map[string]string{"os": "Debian 11"}), ""), host(mergeHost(stable, map[string]string{"os": "Debian 12"}), ""))},
			Low, RuleDefault,
		},
	}
	for name, tc := range cases {
		out, err := Baseline(tc.events, asOf, testPolicy)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out.Level != tc.level || out.RuleID != tc.rule {
			t.Errorf("%s: got (%v,%s), want (%v,%s) skipped=%v", name, out.Level, out.RuleID, tc.level, tc.rule, out.Skipped)
		}
		if out.Level == Critical && out.RuleID != RuleCritIdentityService {
			t.Errorf("%s: critical reachable only through the allowlisted rule, got %s", name, out.RuleID)
		}
	}
}

func mergeHost(base map[string]string, over map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func TestBaselineUnknownInputs(t *testing.T) {
	asOf := time.Now()
	pol := testPolicy

	if _, err := Baseline(nil, asOf, pol); err == nil {
		t.Fatal("empty group must fail, not invent a priority")
	}
	undec := []GroupEvent{DecodeEvent("host.changed", []byte("not json{{{"))}
	out, err := Baseline(undec, asOf, pol)
	if err != nil {
		t.Fatalf("undecodable payload must take the unknown path, not fail: %v", err)
	}
	if out.Level != Low || out.RuleID != RuleDefault || len(out.Skipped) == 0 {
		t.Fatalf("unknown-input path must be low/default with skips: %+v", out)
	}
	half := []GroupEvent{DecodeEvent("host.changed", []byte(`{"event_id":"e1","current":{"ip":"10.0.0.1"}}`))}
	out, err = Baseline(half, asOf, pol)
	if err != nil || out.Level != Low {
		t.Fatalf("half-known changed must degrade to low: %+v %v", out, err)
	}
}

func TestBaselineDeterministicAsOf(t *testing.T) {
	stable := strongBase("")
	events := []GroupEvent{changedEv(host(stable, ""), host(stable, openPort(22)))}
	a := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	first, err := Baseline(events, a, testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Baseline(events, a, testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if first.Level != second.Level || first.RuleID != second.RuleID {
		t.Fatal("same asOf must give the same outcome")
	}
	if first.Level != High {
		t.Fatalf("want high, got %v", first.Level)
	}
}

func TestBaselineFirstSeenGating(t *testing.T) {
	// Synthetic payloads carrying the not-yet-shipped first_strong_seen_at
	// prove the dormant age gates work when Pro provides the field.
	withFirst := func(first string) GroupEvent {
		prev := `{"ip":"192.168.0.5","hostname":"p","mac":"aa:bb:cc:dd:ee:01","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-28T11:00:00Z","first_strong_seen_at":"` + first + `","ports":[]}`
		cur := `{"ip":"192.168.0.5","hostname":"p","mac":"aa:bb:cc:dd:ee:01","weak":false,"alive":true,"identity_confidence":"high","last_strong_seen_at":"2026-10-28T11:00:00Z","first_strong_seen_at":"` + first + `","ports":[{"number":22,"protocol":"tcp","state":"open"}]}`
		return DecodeEvent("host.changed", []byte(`{"event_id":"e1","previous":`+prev+`,"current":`+cur+`}`))
	}
	asOf := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	fresh, err := Baseline([]GroupEvent{withFirst("2026-10-28T11:30:00Z")}, asOf, testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Level != Low {
		t.Fatalf("first-seen 30m ago must not satisfy the 1h established gate: %+v", fresh)
	}
	old, err := Baseline([]GroupEvent{withFirst("2026-10-20T00:00:00Z")}, asOf, testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if old.Level != High || old.RuleID != RuleHighNewService {
		t.Fatalf("first-seen 8d ago must satisfy the gates: %+v", old)
	}
}

func TestBaselineHostileTwins(t *testing.T) {
	asOf := time.Now()
	mk := func(hostname, vendor, os string) GroupEvent {
		prev := `{"ip":"192.168.0.5","hostname":"printer","mac":"aa:bb:cc:dd:ee:01","weak":false,"alive":true,"identity_confidence":"high","vendor":"Acme","os":"Debian 11","ports":[]}`
		cur := `{"ip":"192.168.0.5","hostname":` + q(hostname) + `,"mac":"aa:bb:cc:dd:ee:01","weak":false,"alive":true,"identity_confidence":"high","vendor":` + q(vendor) + `,"os":` + q(os) + `,"ports":[]}`
		return DecodeEvent("host.changed", []byte(`{"event_id":"e1","previous":`+prev+`,"current":`+cur+`}`))
	}
	// Twins differ in hostile content but share emptiness and equality.
	clean, err := Baseline([]GroupEvent{mk("renamed", "Acme", "Debian 11")}, asOf, testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	hostile, err := Baseline([]GroupEvent{mk("\"><script>alert(1)</script>", "Acme", "Debian 11")}, asOf, testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Level != hostile.Level || clean.RuleID != hostile.RuleID {
		t.Fatalf("hostile twin diverged: %+v vs %+v", clean, hostile)
	}
	if clean.RuleID != RuleLowCosmetic {
		t.Fatalf("hostname-only twin must be cosmetic low: %+v", clean)
	}
}

func TestBaselineCriticalReachability(t *testing.T) {
	asOf := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	stable := strongBase("")
	events := []GroupEvent{changedEv(host(mergeHost(stable, map[string]string{"device": "OldCam"}), ""), host(mergeHost(stable, map[string]string{"device": "NewCam"}), openPort(554)))}
	out, err := Baseline(events, asOf, testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if out.Level != Critical || out.RuleID != RuleCritIdentityService {
		t.Fatalf("device replacement plus new service must be critical via the allowlisted rule: %+v", out)
	}
	if strings.Contains(out.RuleID, "SENSITIVE") {
		t.Fatal("no sensitive-port rule may remain")
	}
}
