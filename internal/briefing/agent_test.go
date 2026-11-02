package briefing

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ostefani/watcherbridge/internal/provider"
	"github.com/ostefani/watcherbridge/internal/redactor"
	"github.com/ostefani/watcherbridge/internal/store"
)

func agentTestDeps(t tb, s *store.Store, p provider.Provider) Deps {
	t.Helper()
	d := modelDeps(t, s, p)
	d.AgentEnabled = true
	d.AgentMaxSteps = 4
	d.ModelMinBaseline = Low
	d.AgentMinBaseline = Medium
	d.AgentTimeout = 5 * time.Second
	return d
}

func takeSeeded(t *testing.T, s *store.Store, gid string) store.TakenGroup {
	t.Helper()
	taken, err := s.TakeGroup(context.Background(), gid)
	if err != nil {
		t.Fatal(err)
	}
	return taken
}

var tokenScrape = regexp.MustCompile(`\b[A-Z]+_\d+\b`)

// finalFromTranscript builds a schema-valid final answer from the
// bundle carried in transcript[1], scraping tokens the way the model
// would read them. No token numbers are hardcoded.
func finalFromTranscript(t *testing.T, tr []provider.TurnMessage, suggestion *string) string {
	t.Helper()
	if len(tr) < 2 {
		t.Fatalf("transcript must carry system and bundle: %d messages", len(tr))
	}
	var bundle struct {
		EventIDs []string `json:"event_ids"`
	}
	if err := json.Unmarshal([]byte(tr[1].Text), &bundle); err != nil {
		t.Fatalf("fake received undecodable bundle: %v", err)
	}
	toks := tokenScrape.FindAllString(tr[1].Text, -1)
	host := ""
	for _, tok := range toks {
		if strings.HasPrefix(tok, "HOST_") {
			host = tok
			break
		}
	}
	if host == "" && len(toks) > 0 {
		host = toks[0]
	}
	if host == "" {
		t.Fatal("bundle carries no tokens to echo")
	}
	m := map[string]any{
		"event_ids": bundle.EventIDs, "host": host,
		"what_changed": "changed " + host, "context": "context for " + host,
		"model_suggestion": suggestion, "priority_reason": "because reasons",
		"recommended_check": "look",
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSelectMode(t *testing.T) {
	cases := []struct {
		name               string
		baseline           Level
		modelMin, agentMin Level
		enabled, capable   bool
		want               briefMode
	}{
		{"low serves single", Low, Low, Medium, true, true, modeSingle},
		{"medium investigates", Medium, Low, Medium, true, true, modeAgent},
		{"high investigates", High, Low, Medium, true, true, modeAgent},
		{"disabled stays single", High, Low, Medium, false, true, modeSingle},
		{"incapable stays single", High, Low, Medium, true, false, modeSingle},
		{"below model floor is deterministic", Low, Medium, High, true, true, modeDeterministic},
		{"equal floors give the shared level to the loop", High, High, High, true, true, modeAgent},
		{"above equal floors investigates", Critical, High, High, true, true, modeAgent},
		{"zero deps never investigate", Medium, Low, Low, false, false, modeSingle},
	}
	for _, c := range cases {
		if got := selectMode(c.baseline, c.modelMin, c.agentMin, c.enabled, c.capable); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestAgentToolThenFinal(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	calls := 0
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		calls++
		if calls == 1 {
			return `{"tool":"get_reported_profile","arguments":{"host":"IP_1"}}`, nil
		}
		return finalFromTranscript(t, tr, strptr("high")), nil
	}
	d := agentTestDeps(t, s, fake)
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("agent run must progress the group")
	}
	if fake.TurnCalls != 2 {
		t.Fatalf("one tool turn plus the final: %d calls", fake.TurnCalls)
	}
	b := briefed(t, s, gid)
	if b.Source != "agent" {
		t.Fatalf("loop briefing must carry source agent: %q", b.Source)
	}
	// The second turn's transcript must carry the tool result, with no
	// raw addresses and no redaction block.
	found := false
	for _, m := range fake.TurnReceived[1] {
		if m.Role == "tool" {
			found = true
			if !strings.Contains(m.Text, "get_reported_profile") {
				t.Fatalf("tool result must name its tool: %s", m.Text)
			}
			if strings.Contains(m.Text, "192.168.") {
				t.Fatalf("tool result leaks a raw address: %s", m.Text)
			}
			if strings.Contains(m.Text, "redaction_blocked") {
				t.Fatalf("clean fixture must not block: %s", m.Text)
			}
		}
	}
	if !found {
		t.Fatal("final turn must follow a tool result")
	}
}

func TestAgentFindSimilarGrantsAlias(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	calls := 0
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		calls++
		switch calls {
		case 1:
			return `{"tool":"find_similar_reported_changes","arguments":{"kind":"changed","window_days":7}}`, nil
		case 2:
			return `{"tool":"get_reported_profile","arguments":{"host":"SUBJ_1"}}`, nil
		}
		return finalFromTranscript(t, tr, nil), nil
	}
	d := agentTestDeps(t, s, fake)
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("agent run must progress the group")
	}
	if fake.TurnCalls != 3 {
		t.Fatalf("two tool turns plus the final: %d calls", fake.TurnCalls)
	}
	var firstTool string
	for _, m := range fake.TurnReceived[1] {
		if m.Role == "tool" {
			firstTool = m.Text
		}
	}
	if !strings.Contains(firstTool, "SUBJ_1") {
		t.Fatalf("find_similar must grant a subject token: %s", firstTool)
	}
	if b := briefed(t, s, gid); b.Source != "agent" {
		t.Fatalf("granted alias must resolve: %+v", b)
	}
}

func TestAgentUnissuedTokenRejects(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	calls := 0
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		calls++
		if calls == 1 {
			return `{"tool":"get_reported_profile","arguments":{"host":"IP_99"}}`, nil
		}
		return finalFromTranscript(t, tr, nil), nil
	}
	d := agentTestDeps(t, s, fake)
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("repaired run must progress the group")
	}
	if fake.TurnCalls != 2 {
		t.Fatalf("rejected turn plus one repair: %d calls", fake.TurnCalls)
	}
	if b := briefed(t, s, gid); b.Source != "agent" {
		t.Fatalf("repaired run must store a loop briefing: %+v", b)
	}
}

func TestAgentIssuedButUnknownSubject(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	// The standard fixture projects event-local subjects (no upstream
	// reference), so every bundle token is issued but names no
	// permitted subject: the tool answers unknown and the run goes on.
	calls := 0
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		calls++
		if calls == 1 {
			return `{"tool":"get_reported_history","arguments":{"host":"IP_1","limit":4}}`, nil
		}
		return finalFromTranscript(t, tr, nil), nil
	}
	d := agentTestDeps(t, s, fake)
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("agent run must progress the group")
	}
	seen := false
	for _, m := range fake.TurnReceived[1] {
		if m.Role == "tool" && strings.Contains(m.Text, "unknown subject") {
			seen = true
		}
	}
	if !seen {
		t.Fatal("unlinked token must answer unknown subject in the tool result")
	}
	if b := briefed(t, s, gid); b.Source != "agent" {
		t.Fatalf("unknown subject must not stop the loop: %+v", b)
	}
}

func TestAgentRepairRecoversInvalidTurn(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	calls := 0
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		calls++
		if calls == 1 {
			return `{"tool":"nope","arguments":{}}`, nil
		}
		return finalFromTranscript(t, tr, nil), nil
	}
	d := agentTestDeps(t, s, fake)
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("repaired run must progress the group")
	}
	if fake.TurnCalls != 2 {
		t.Fatalf("invalid turn plus one repair: %d calls", fake.TurnCalls)
	}
	if b := briefed(t, s, gid); b.Source != "agent" || b.FallbackUsed {
		t.Fatalf("repaired final must store a model briefing: %+v", b)
	}
}

func TestAgentFinalTurnToolFallsBack(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	// Every turn names a tool: the tool turn executes, the reserved
	// final answers with another tool call, and the repair re-asks
	// with a third. Tools are spent on the final turn, so the repair
	// response cannot execute — it falls back instead of finishing
	// with a dangling tool result and no briefing.
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		return `{"tool":"find_similar_reported_changes","arguments":{"kind":"changed"}}`, nil
	}
	d := agentTestDeps(t, s, fake)
	d.AgentMaxSteps = 1
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("failed run must still progress the group")
	}
	// One tool turn, the reserved final, and the single repair: the
	// repair response is never executed, so no fourth send exists.
	if fake.TurnCalls != 3 {
		t.Fatalf("tool turn plus final plus one repair: %d calls", fake.TurnCalls)
	}
	b := briefed(t, s, gid)
	if !b.FallbackUsed || b.Source != "fallback" {
		t.Fatalf("final-turn tool call must fall back: %+v", b)
	}
}

func TestAgentTranscriptScanBlocksResend(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	// The hostile address rides inside an otherwise well-formed tool
	// call: argument resolution rejects the unknown token, and the
	// repair re-ask is never transmitted — the pre-send scan fails the
	// composed transcript closed on the model's own hostile text.
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		return `{"tool":"get_reported_profile","arguments":{"host":"IP_1 // 192.168.9.9"}}`, nil
	}
	d := agentTestDeps(t, s, fake)
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("blocked run must still progress the group")
	}
	if fake.TurnCalls != 1 {
		t.Fatalf("blocked transcript must never transmit again: %d calls", fake.TurnCalls)
	}
	b := briefed(t, s, gid)
	if !b.FallbackUsed || !strings.Contains(b.Context, "transcript blocked") {
		t.Fatalf("scan hit must fail closed to a fallback: %+v", b)
	}
}

func TestAgentDoubleInvalidFallsBackOnce(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		return `[{"not":"an object"}]`, nil
	}
	d := agentTestDeps(t, s, fake)
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("failed run must still progress the group")
	}
	if fake.TurnCalls != 2 {
		t.Fatalf("one repair per run, never more: %d calls", fake.TurnCalls)
	}
	b := briefed(t, s, gid)
	if !b.FallbackUsed || b.Source != "fallback" {
		t.Fatalf("unrecoverable turns must fall back: %+v", b)
	}
}

func TestAgentProviderErrorFallsBack(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	fake.Turns = []provider.FakeStep{{Err: errors.New("boom")}}
	d := agentTestDeps(t, s, fake)
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("failed turn must still progress the group")
	}
	b := briefed(t, s, gid)
	if !b.FallbackUsed {
		t.Fatalf("turn error with a live parent must fall back: %+v", b)
	}
}

func TestAgentParentCancelReleases(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	fake.Turns = []provider.FakeStep{{Hang: true}}
	d := agentTestDeps(t, s, fake)
	taken := takeSeeded(t, s, gid)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- runAgent(ctx, d, taken, Outcome{Level: Medium, RuleID: RuleMedNewIdentity})
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case progressed := <-done:
		if progressed {
			t.Fatal("cancelled run must not claim progress")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled run must return")
	}
	if st, _ := s.GroupStatus(context.Background(), gid); st != "processing" {
		t.Fatalf("cancelled group stays processing for the lease: %q", st)
	}
	if _, err := s.BriefingForGroup(context.Background(), gid); err == nil {
		t.Fatal("cancelled run must store nothing")
	}
}

func TestAgentBelowFloorIsDeterministic(t *testing.T) {
	s := openWorkerStore(t)
	// An undecodable payload decodes to no host view: baseline Low.
	gid := seedGroup(t, s, "host:low", "evt-low-1", "host.changed", `{"event_id":"evt-low-1"}`)
	fake := provider.NewFake()
	d := agentTestDeps(t, s, fake)
	d.ModelMinBaseline = Medium
	d.AgentMinBaseline = High
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("deterministic path must progress the group")
	}
	if fake.TurnCalls != 0 || fake.Briefs != 0 {
		t.Fatalf("below the floor bills no provider call: turns=%d briefs=%d", fake.TurnCalls, fake.Briefs)
	}
	b := briefed(t, s, gid)
	if b.FallbackUsed || b.Source != "rules" || !strings.Contains(b.Context, "single-call floor") {
		t.Fatalf("below-floor briefing must be intentional rules-only output: %+v", b)
	}
}

func TestAgentDisabledUsesSingleCall(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	fake.Handle = func(repair bool, payload []byte, valid map[string]struct{}) (provider.RawBriefing, error) {
		return provider.RawBriefing{JSON: validAnswer(t, payload, valid, strptr("high"), nil)}, nil
	}
	d := modelDeps(t, s, fake)
	d.ModelTimeout = 5 * time.Second
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("single-call path must progress the group")
	}
	if b := briefed(t, s, gid); b.Source != "model" {
		t.Fatalf("disabled agent must use the single call: %q", b.Source)
	}
}

func TestAgentFinalTurnReserved(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	calls := 0
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		calls++
		if calls == 1 {
			return `{"tool":"find_similar_reported_changes","arguments":{"kind":"changed"}}`, nil
		}
		return finalFromTranscript(t, tr, nil), nil
	}
	d := agentTestDeps(t, s, fake)
	d.AgentMaxSteps = 1
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("one tool turn plus the reserved final must progress the group")
	}
	if fake.TurnCalls != 2 {
		t.Fatalf("one tool turn plus the reserved final: %d calls", fake.TurnCalls)
	}
	if b := briefed(t, s, gid); b.Source != "agent" {
		t.Fatalf("reserved final must store: %+v", b)
	}
}

func TestAgentProducerUUIDsTransmit(t *testing.T) {
	s := openWorkerStore(t)
	// Producer UUIDs are legitimate event IDs (hex and '-'). Their
	// trailing 12-hex segment matches the scanner's bare-12-hex MAC
	// leg — but only when embedded in larger text. The bundle carries
	// them as exact leaves (skipped); the transcript embeds them in
	// message text, so the pre-send scan must neutralize the cleared
	// IDs structurally instead of blocking the run.
	uuid := "550e8400-e29b-41d4-a716-446655440000"
	payload := strings.Replace(changedPayload("", `{"number":22,"protocol":"tcp","state":"open"}`), `"event_id":"e1"`, `"event_id":"`+uuid+`"`, 1)
	gid := seedGroup(t, s, "host:u", uuid, "host.changed", payload)
	fake := provider.NewFake()
	calls := 0
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		calls++
		if calls == 1 {
			return `{"tool":"get_reported_profile","arguments":{"host":"IP_1"}}`, nil
		}
		return finalFromTranscript(t, tr, strptr("high")), nil
	}
	d := agentTestDeps(t, s, fake)
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("agent run must progress the group")
	}
	if fake.TurnCalls != 2 {
		t.Fatalf("producer UUIDs must not block transmission: %d calls", fake.TurnCalls)
	}
	if b := briefed(t, s, gid); b.Source != "agent" {
		t.Fatalf("UUID run must store a loop briefing: %+v", b)
	}
}

func TestParseTurn(t *testing.T) {
	if call, final, _ := parseTurn(`{"tool":"get_reported_profile","arguments":{"host":"IP_1"},"why":"check"}`); call == nil || final || call.name != "get_reported_profile" {
		t.Fatalf("valid tool call must parse: %+v %v", call, final)
	}
	if _, _, corr := parseTurn(`{"tool":"nope","arguments":{}}`); corr != "unknown tool" {
		t.Fatalf("unknown tool must name its kind: %q", corr)
	}
	if _, _, corr := parseTurn(`{"tool":"find_similar_reported_changes","arguments":{"kind":"changed"},"extra":1}`); corr == "" {
		t.Fatal("unknown field in a tool call must reject")
	}
	// Numbers survive shape parsing (validation rejects them later):
	// the production arguments decoder uses json.Number, so integral
	// values keep their type for the bounded-integer checks.
	if call, _, _ := parseTurn(`{"tool":"compare_reported_context","arguments":{"host":7}}`); call == nil {
		t.Fatal("numeric argument must survive shape parsing")
	}
	if _, reject := intArg(map[string]any{"limit": float64(7)}, "limit", 4, 1, 10); reject == "" {
		t.Fatal("non-Number integers must reject")
	}
	if v, reject := intArg(map[string]any{"limit": json.Number("7")}, "limit", 4, 1, 10); reject != "" || v != 7 {
		t.Fatal("json.Number integers must validate")
	}
	if _, reject := intArg(map[string]any{"limit": json.Number("7.5")}, "limit", 4, 1, 10); reject == "" {
		t.Fatal("fractional numbers must reject")
	}
	if _, final, _ := parseTurn(`{"event_ids":["e"],"host":"H","what_changed":"w","context":"c","priority_reason":"r","recommended_check":"k"}`); !final {
		t.Fatal("final shape must classify as final")
	}
	if _, _, corr := parseTurn(`{"event_ids":[],"bogus":1}`); corr == "" {
		t.Fatal("unknown field in a final must reject")
	}
	if _, _, corr := parseTurn(`just text`); corr == "" {
		t.Fatal("prose must reject")
	}
	if _, _, corr := parseTurn(``); corr == "" {
		t.Fatal("empty turn must reject")
	}
}

func TestTurnSliceFormula(t *testing.T) {
	// Designv9 §6.4: turn k (1-based), turnsLeft=(steps+1)-k,
	// deadline=min(provider, remaining/(turnsLeft+1)); the reserved
	// final (k=steps+1) gets the whole remainder.
	budget := 90 * time.Second
	prov := 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	dl, ok := turnSlice(ctx, prov, 4, 1)
	if !ok {
		t.Fatal("first turn must have a deadline")
	}
	if dl < 17*time.Second || dl > 18*time.Second {
		t.Fatalf("first slice must be ~remaining/5: %v", dl)
	}
	dl, ok = turnSlice(ctx, prov, 4, 5)
	if !ok {
		t.Fatal("final turn must have a deadline")
	}
	// The final turn is not divided further, but one Turn is one
	// provider call: the provider timeout still caps it.
	if dl != prov {
		t.Fatalf("reserved final must get min(provider, remainder): %v", dl)
	}
	short, stopShort := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopShort()
	dl, ok = turnSlice(short, prov, 4, 5)
	if !ok || dl < 4*time.Second || dl > 5*time.Second {
		t.Fatalf("small remainder must pass through to the final: %v %v", dl, ok)
	}
	past, stop := context.WithTimeout(context.Background(), time.Nanosecond)
	defer stop()
	time.Sleep(5 * time.Millisecond)
	if _, ok := turnSlice(past, prov, 4, 1); ok {
		t.Fatal("exhausted budget must refuse another turn")
	}
}

func TestToolResultStaysInBudget(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	taken := takeSeeded(t, s, gid)
	env := newAgentEnv(s, taken, map[string]string{"IP_1": "192.168.0.5"}, time.Now())
	if failReason := env.seed(context.Background(), "test-scope"); failReason != "" {
		t.Fatal(failReason)
	}
	for _, tc := range []toolCall{
		{name: "get_reported_profile", args: map[string]any{"host": "IP_1"}},
		{name: "find_similar_reported_changes", args: map[string]any{"kind": "changed", "window_days": json.Number("7")}},
	} {
		res := env.runTool(context.Background(), &tc)
		if len(res) > maxToolResultBytes {
			t.Fatalf("%s result exceeds its budget: %d bytes", tc.name, len(res))
		}
		if _, rej := isReject(res); rej {
			t.Fatalf("%s must not reject: %s", tc.name, res)
		}
	}
	// A hostile-sized value truncates with flags, never with cut JSON.
	o := &toolOut{}
	for i := 0; i < 64; i++ {
		o.put("k", strings.Repeat("x", 200))
	}
	if !o.truncated {
		t.Fatal("overflowing fields must set the truncated flag")
	}
	raw, err := json.Marshal(map[string]any{"result": o.m, "truncated": o.truncated, "omitted": o.omitted})
	if err != nil || len(raw) > maxToolResultBytes {
		t.Fatalf("truncated result must stay valid and in budget: %v %d", err, len(raw))
	}
}

func TestRenderPortsKeepsCappedRows(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	taken := takeSeeded(t, s, gid)
	env := newAgentEnv(s, taken, map[string]string{}, time.Now())
	ports := make([]store.ReportedPort, 0, 65)
	for i := 1; i <= 65; i++ {
		ports = append(ports, store.ReportedPort{Proto: "tcp", Port: i, State: "open"})
	}
	o := &toolOut{}
	env.renderPorts(o, ports, 64)
	if _, ok := o.m["ports"]; !ok {
		t.Fatal("capped list must still emit the ports key")
	}
	raw, err := json.Marshal(o.m["ports"])
	if err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	// 65 valid ports with a cap of 64 keep the first 64 plus the
	// truncation flag — never an empty list.
	if len(back) != 64 {
		t.Fatalf("capped list must keep 64 rows, kept %d", len(back))
	}
	if !o.truncated {
		t.Fatal("capped list must mark truncation")
	}
}

func TestRenderPortsKeepsFittingPrefix(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	taken := takeSeeded(t, s, gid)
	env := newAgentEnv(s, taken, map[string]string{}, time.Now())
	// Long but valid rows: five-digit ports, filtered states, 24-char
	// service words. The whole list cannot fit, but the fitting prefix
	// must be kept with the truncation flag — not omitted completely.
	ports := make([]store.ReportedPort, 0, 65)
	for i := 0; i < 65; i++ {
		ports = append(ports, store.ReportedPort{Proto: "tcp", Port: 60000 + i, State: "filtered", Service: "abcdefghijklmnopqrstuvwx"})
	}
	o := &toolOut{}
	env.renderPorts(o, ports, 64)
	raw, err := json.Marshal(o.m["ports"])
	if err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) == 0 || len(back) >= 65 {
		t.Fatalf("long rows must keep a non-empty fitting prefix: %d rows", len(back))
	}
	if !o.truncated {
		t.Fatal("truncated list must mark truncation")
	}
	envRaw, err := json.Marshal(map[string]any{"tool": "get_reported_services", "result": o.m, "truncated": o.truncated, "omitted": o.omitted})
	if err != nil || len(envRaw) > maxToolResultBytes {
		t.Fatalf("bounded envelope must stay valid and within 4 KiB: %v %d", err, len(envRaw))
	}
}

func TestNeutralizeReferenceIDsStructural(t *testing.T) {
	// A cleared short ID overlapping an address: only the reference
	// position is exempted, the address stays fully visible to the
	// scanner. Global substring scrubbing would rewrite 10.23.45.67
	// into unscannable xxxx.23.45.67 while the original transmits.
	final := `{"event_ids":["10","bogus"],"host":"IP_1","what_changed":"saw 10.23.45.67","context":"c","model_suggestion":null,"priority_reason":"r","recommended_check":"k"}`
	tr := []provider.TurnMessage{{Role: "assistant", Text: final}}
	raw, _ := json.Marshal(tr)
	out := neutralizeReferenceIDs(raw, map[string]bool{"10": true})
	var back []provider.TurnMessage
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	var shape map[string]any
	if err := json.Unmarshal([]byte(back[0].Text), &shape); err != nil {
		t.Fatal(err)
	}
	ids, _ := shape["event_ids"].([]any)
	if len(ids) != 2 || ids[0] != "xxxx" || ids[1] != "bogus" {
		t.Fatalf("only the cleared reference is exempted: %v", ids)
	}
	if !strings.Contains(back[0].Text, "10.23.45.67") {
		t.Fatalf("the overlapping address must survive verbatim: %s", back[0].Text)
	}
	issued := redactor.TokenMap{"IP_1": "192.168.0.5"}
	if hits, err := redactor.ScanOutboundLeaks(out, issued, []string{"10"}); err != nil || len(hits) == 0 {
		t.Fatalf("the surviving address must still trip the scan: %v %v", hits, err)
	}
}

func TestMessageHasDuplicateKeys(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"flat duplicate", `{"why":"192.168.9.9","why":"safe"}`, true},
		{"nested duplicate", `{"tool":"x","arguments":{"host":"IP_1","host":"IP_2"}}`, true},
		{"duplicate in array element", `[{"a":1},{"a":2,"a":3}]`, true},
		{"distinct keys", `{"tool":"x","arguments":{"host":"IP_1"},"why":"safe"}`, false},
		{"same key in sibling objects", `{"a":{"x":1},"b":{"x":2}}`, false},
		{"array of scalars", `[1,2,2]`, false},
		{"plain prose", `saw 192.168.9.9 on the lan`, false},
		{"malformed JSON passes through verbatim", `{"a":1,`, false},
		{"trailing data is not normalized", `{"a":1,"a":2} trailing`, false},
		{"empty", ``, false},
	}
	for _, c := range cases {
		if got := messageHasDuplicateKeys(c.text); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestAgentOverlappingIDStillBlocks(t *testing.T) {
	s := openWorkerStore(t)
	// Permitted short event ID overlapping a real address. The final is
	// invalid (bogus ID), so the loop attempts a repair: the repair
	// send must be blocked on the raw address even though the "10"
	// inside the reference array is cleared.
	payload := strings.Replace(changedPayload("", `{"number":22,"protocol":"tcp","state":"open"}`), `"event_id":"e1"`, `"event_id":"10"`, 1)
	gid := seedGroup(t, s, "host:neg", "10", "host.changed", payload)
	fake := provider.NewFake()
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		return `{"event_ids":["10","bogus"],"host":"IP_1","what_changed":"saw 10.23.45.67","context":"c","model_suggestion":null,"priority_reason":"r","recommended_check":"k"}`, nil
	}
	d := agentTestDeps(t, s, fake)
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("blocked run must still progress the group")
	}
	if fake.TurnCalls != 1 {
		t.Fatalf("overlapping address must block the repair send: %d calls", fake.TurnCalls)
	}
	b := briefed(t, s, gid)
	if !b.FallbackUsed || !strings.Contains(b.Context, "transcript blocked") {
		t.Fatalf("overlap must fail closed to a blocked fallback: %+v", b)
	}
}

func TestBlockedToolResultShape(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	taken := takeSeeded(t, s, gid)
	// Inject a value that collides with the envelope's own fixed keys:
	// every serialized result names the "tool" key, so the scan must
	// trip and the tool must answer the generic redaction_blocked
	// shape while the run stays alive. Real fixtures never carry such
	// a value (the bundle scan would block first), so this white-box
	// value isolates the tool-result gate itself.
	env := newAgentEnv(s, taken, map[string]string{"X_1": "tool"}, time.Now())
	if failReason := env.seed(context.Background(), "test-scope"); failReason != "" {
		t.Fatal(failReason)
	}
	res := env.runTool(context.Background(), &toolCall{
		name: "find_similar_reported_changes",
		args: map[string]any{"kind": "changed", "window_days": json.Number("7")},
	})
	var env2 struct {
		Tool   string `json:"tool"`
		Result struct {
			Error string `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(res), &env2); err != nil {
		t.Fatalf("blocked result must stay valid JSON: %v", err)
	}
	if env2.Tool != "find_similar_reported_changes" || env2.Result.Error != "redaction_blocked" {
		t.Fatalf("hit must map to the generic blocked shape: %s", res)
	}
	if strings.Contains(res, "reported_observations") || strings.Contains(res, "received_at") {
		t.Fatalf("blocked result must name no values and no paths: %s", res)
	}
}

func TestAgentDuplicateKeysBlockResend(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	// An otherwise valid tool call hides a raw address in the shadowed
	// first "why": map decoding inspects only "safe", but the provider
	// would receive the verbatim text with the address. The pre-send
	// scan must fail the resend closed instead of transmitting it.
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		return `{"tool":"get_reported_profile","arguments":{"host":"IP_1"},"why":"192.168.9.9","why":"safe"}`, nil
	}
	d := agentTestDeps(t, s, fake)
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("blocked run must still progress the group")
	}
	if fake.TurnCalls != 1 {
		t.Fatalf("duplicate-key transcript must never transmit again: %d calls", fake.TurnCalls)
	}
	b := briefed(t, s, gid)
	if !b.FallbackUsed || !strings.Contains(b.Context, "transcript blocked") {
		t.Fatalf("duplicate keys must fail closed to a blocked fallback: %+v", b)
	}
}

func TestAgentSetupBoundedByRunBudget(t *testing.T) {
	s := openWorkerStore(t)
	gid := hostChangedGroup(t, s)
	fake := provider.NewFake()
	fake.TurnHandle = func(tr []provider.TurnMessage) (string, error) {
		return finalFromTranscript(t, tr, strptr("high")), nil
	}
	d := agentTestDeps(t, s, fake)
	// An already-exhausted run budget must bound preparation too: the
	// bundle build and registry seed run under it, so a starved run
	// fails setup — never a turn-budget fallback after unbounded work —
	// without ever billing a provider turn.
	d.AgentTimeout = time.Nanosecond
	if !briefOne(context.Background(), d, gid) {
		t.Fatal("starved run must still progress the group")
	}
	if fake.TurnCalls != 0 {
		t.Fatalf("starved run must bill no provider turn: %d calls", fake.TurnCalls)
	}
	b := briefed(t, s, gid)
	if !b.FallbackUsed || b.Source != "fallback" {
		t.Fatalf("starved run must fall back: %+v", b)
	}
	if !(strings.Contains(b.Context, "bundle build failed") || strings.Contains(b.Context, "registry unavailable")) {
		t.Fatalf("starved run must fail setup under the budget: %+v", b)
	}
}
