// Bounded investigator loop (Designv9 §6, Planv4 Step 26): an opt-in,
// default-off provider path for significant baselines. The loop is an
// adapter capability (provider.Turner), never a new dependency; without
// it, eligible groups use the single call. Every turn re-sends the full
// redacted transcript, so the provider holds no session and a restart
// resumes from the local log alone.
package briefing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/ostefani/watcherbridge/internal/provider"
	"github.com/ostefani/watcherbridge/internal/redactor"
	"github.com/ostefani/watcherbridge/internal/store"
)

const (
	// maxToolResultBytes caps one serialized tool result (Designv9 §6.2:
	// results <=4 KiB). Truncation and omission are marked in the
	// envelope flags, never silent — and never a cut inside the JSON,
	// which is built to fit.
	maxToolResultBytes = 4 << 10
	// maxTranscriptBytes caps the whole run transcript. Worst case by
	// construction: a 256 KiB bundle, six 64 KiB assistant answers
	// (four tool turns, the final turn, one repair), five 4 KiB tool
	// results, and framing — about 670 KiB. The 768 KiB cap is
	// unreachable in practice and enforced anyway, before any send.
	maxTranscriptBytes = 768 << 10
)

// briefMode names the provider path for one group. Exactly one path
// owns every baseline (Designv9 §6.1); the floors partition them
// without overlap.
type briefMode int

const (
	// modeDeterministic answers from the baseline alone: below the
	// single-call floor no provider is billed for low-signal groups.
	modeDeterministic briefMode = iota
	// modeSingle is the Step 11 one-shot Brief call.
	modeSingle
	// modeAgent is the Step 26 bounded loop, when enabled and capable.
	modeAgent
)

// selectMode assigns the provider path (Designv9 §6.1 bands): below the
// single-call floor the deterministic rules output answers; from the
// agent floor up the loop investigates when enabled with a capable
// provider; between the floors the single call answers. Equal floors
// give the shared level to the loop — the agent floor includes its
// level, and the single-call band between equal floors is empty.
func selectMode(baseline, modelMin, agentMin Level, enabled, capable bool) briefMode {
	if baseline < modelMin {
		return modeDeterministic
	}
	if enabled && capable && baseline >= agentMin {
		return modeAgent
	}
	return modeSingle
}

// isTurner reports the optional per-turn capability: a type assertion,
// never a config flag, so capability and configuration cannot disagree.
func isTurner(p provider.Provider) bool {
	if p == nil {
		return false
	}
	_, ok := p.(provider.Turner)
	return ok
}

// repairPromptForTurn corrects an invalid turn: it names the schema
// error kind only, never bundle content, so a hostile bundle cannot
// steer the re-ask — the same rule as the single-call repair prompt.
func repairPromptForTurn(correction string) string {
	return "That turn was rejected (" + correction + "). Reply with exactly one JSON object: either a tool call " +
		`{"tool": "<name>", "arguments": {...}} or the final briefing ` +
		`{"event_ids": [...], "host": "<token>", "what_changed": "...", "context": "...", ` +
		`"model_suggestion": "low"|"medium"|"high"|"critical"|null, "priority_reason": "...", "recommended_check": "...", ` +
		`"likely_explanation": "...", "confidence": "low"|"medium"|"high", "evidence_refs": [...]}. ` +
		`likely_explanation is your display-only hypothesis (at most 256 chars); evidence_refs holds at most 8 citations, ` +
		`each {"event_id": "<a group event ID>"} or {"tool_step": <a 0-based executed tool step of this run>}. ` +
		"Use only issued tokens: bundle tokens or subject tokens from earlier tool results."
}

// agentLoop is one bounded run: the provider, the transcript, the tool
// environment, and the single repair flag shared by every failure in
// the run (Designv9 §6.2: at most one bounded repair sharing the turn
// deadline). done reports a terminal outcome; false means the loop
// continues with the next tool turn.
type agentLoop struct {
	parent   context.Context
	ctx      context.Context // run context: the AgentTimeout budget
	d        Deps
	taken    store.TakenGroup
	outcome  Outcome
	turner   provider.Turner
	env      *agentEnv
	issued   redactor.TokenMap
	display  redactor.TokenMap
	steps    int
	provider time.Duration
	repair   bool // the one repair per run, shared by every failure
	fallback string
	usage    provider.Usage // adapter-reported accounting summed over the run's sends
}

// runAgent runs the bounded investigator loop for one taken group and
// reports whether group state changed, like briefWithModel. Terminal
// paths: an accepted final stores exactly one briefing (source
// "agent"); any failure stores the deterministic fallback — except a
// dead parent context, which stores nothing and returns false so the
// worker release/retry path owns the group.
func runAgent(ctx context.Context, d Deps, taken store.TakenGroup, outcome Outcome) bool {
	if taken.LastAttempt() {
		// Final take: skip the provider and write the deterministic
		// briefing inline, so a poison group converges to done.
		return insertFallback(ctx, d, taken, outcome, "retry cap reached; briefing from deterministic baseline only", nil, provider.Usage{})
	}
	steps := d.AgentMaxSteps
	if steps < 1 {
		steps = 1
	}
	turner, ok := d.Provider.(provider.Turner)
	if !ok {
		// Selection guarantees capability; a swap underneath it degrades
		// to the fallback rather than inventing a path.
		return insertFallback(ctx, d, taken, outcome, "agent capability unavailable; briefing from deterministic baseline only", nil, provider.Usage{})
	}
	budget := d.AgentTimeout
	if budget <= 0 {
		budget = 90 * time.Second
	}
	providerTimeout := d.ModelTimeout
	if providerTimeout <= 0 {
		providerTimeout = 30 * time.Second
	}
	// The run budget starts before preparation: the bundle build and
	// the registry seed run under it, so slow setup cannot hold the
	// serial worker past AGENT_TIMEOUT. The parent context stays
	// separate for fallback writes and cancellation handling.
	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	raw, issued, display, failReason := buildScannedBundle(runCtx, d, taken, outcome)
	if failReason != "" {
		return insertFallback(ctx, d, taken, outcome, failReason, nil, provider.Usage{})
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	env := newAgentEnv(d.Store, taken, issued, now)
	if failReason = env.seed(runCtx, d.SourceScope); failReason != "" {
		return insertFallback(ctx, d, taken, outcome, failReason, nil, provider.Usage{})
	}

	l := &agentLoop{parent: ctx, ctx: runCtx, d: d, taken: taken, outcome: outcome, turner: turner,
		env: env, issued: issued, display: display, steps: steps, provider: providerTimeout}
	l.env.transcript = []provider.TurnMessage{
		{Role: "system", Text: env.systemPrompt(outcome)},
		{Role: "user", Text: string(raw)},
	}
	// Up to steps read-only tool turns (Designv9 §6.2), then one
	// reserved final turn. An early final is accepted — fewer billed
	// calls — but tools can never consume the final turn.
	for k := 1; k <= steps; k++ {
		if done := l.toolTurn(k); done {
			return l.finish()
		}
	}
	return l.finalTurn()
}

// finish maps the terminal state: a dead parent stores nothing and
// reports no progress, so the worker release/retry path owns the group;
// a fallback reason stores the deterministic briefing instead; a stored
// briefing reports progress.
func (l *agentLoop) finish() bool {
	if l.parent.Err() != nil {
		return false
	}
	if l.fallback != "" {
		// The loop may have executed tools and billed turns before
		// failing: their audit and accounting persist on the
		// fallback instead of vanishing with the run.
		return insertFallback(l.parent, l.d, l.taken, l.outcome, l.fallback, l.traces(), l.usage)
	}
	return true
}

// turnSlice computes one turn's deadline (Designv9 §6.4): for turn k
// (1-based), turnsLeft=(steps+1)-k and the deadline is the smaller of
// the provider timeout and remaining/(turnsLeft+1). The reserved final
// turn (k=steps+1) gets the whole remainder. False means no time
// remains — the run degrades to the fallback rather than starting a
// turn that cannot finish inside the budget.
func turnSlice(runCtx context.Context, providerTimeout time.Duration, steps, k int) (time.Duration, bool) {
	deadline, ok := runCtx.Deadline()
	if !ok {
		return providerTimeout, true
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, false
	}
	turnsLeft := (steps + 1) - k
	share := remaining / time.Duration(turnsLeft+1)
	if share > providerTimeout {
		share = providerTimeout
	}
	return share, true
}

// toolTurn runs one read-only tool turn: send the transcript, dispatch
// the assistant text (tool call executes and the loop continues; an
// early final stores the briefing; any rejection spends the run's one
// repair sharing this turn's deadline and then terminates). True means
// terminal: fallback names the reason, or empty after a stored
// briefing.
func (l *agentLoop) toolTurn(k int) (done bool) {
	slice, ok := turnSlice(l.runCtx(), l.provider, l.steps, k)
	if !ok {
		l.fallback = "agent turn budget exhausted; briefing from deterministic baseline only"
		return true
	}
	turnCtx, stopTurn := context.WithTimeout(l.runCtx(), slice)
	defer stopTurn()
	text, err := l.turn(turnCtx)
	if err != nil {
		l.turnFailed(err)
		return true
	}
	if over := l.appendAssistant(text); over != "" {
		log.Printf("briefing: group %s agent transcript over budget", l.taken.GroupID)
		l.fallback = over
		return true
	}
	call, final, correction := parseTurn(text)
	if call != nil {
		res := l.env.runTool(turnCtx, call)
		if corr, rej := isReject(res); rej {
			return l.repairFlow(turnCtx, corr, true)
		}
		if over := l.appendTool(res); over != "" {
			log.Printf("briefing: group %s agent transcript over budget", l.taken.GroupID)
			l.fallback = over
			return true
		}
		return false
	}
	if final {
		a := acceptAnswer([]byte(text), l.taken, l.issued, l.d.Redaction.Enabled, l.maxToolStep())
		if a.OK {
			insertModelBriefing(l.parent, l.d, l.taken, l.outcome, a, l.issued, l.display, "agent", l.traces(), l.usage)
			return true
		}
		return l.repairFlow(turnCtx, a.Correction, true)
	}
	return l.repairFlow(turnCtx, correction, true)
}

// finalTurn runs the reserved final turn on the whole remaining budget
// (Designv9 §6.4): the assistant text must be the final briefing. A
// tool call here is invalid — the tools are spent — and any rejection
// spends the run's one repair (when still unused) before falling back.
func (l *agentLoop) finalTurn() bool {
	slice, ok := turnSlice(l.runCtx(), l.provider, l.steps, l.steps+1)
	if !ok {
		l.fallback = "agent turn budget exhausted; briefing from deterministic baseline only"
		return l.finish()
	}
	turnCtx, stopTurn := context.WithTimeout(l.runCtx(), slice)
	defer stopTurn()
	text, err := l.turn(turnCtx)
	if err != nil {
		l.turnFailed(err)
		return l.finish()
	}
	if over := l.appendAssistant(text); over != "" {
		l.fallback = over
		return l.finish()
	}
	call, final, correction := parseTurn(text)
	if call != nil {
		correction = "the final turn takes the briefing, not a tool call"
		final = false
	}
	if final {
		a := acceptAnswer([]byte(text), l.taken, l.issued, l.d.Redaction.Enabled, l.maxToolStep())
		if a.OK {
			insertModelBriefing(l.parent, l.d, l.taken, l.outcome, a, l.issued, l.display, "agent", l.traces(), l.usage)
			return l.finish()
		}
		l.repairFlow(turnCtx, a.Correction, false)
		return l.finish()
	}
	l.repairFlow(turnCtx, correction, false)
	return l.finish()
}

// repairFlow spends the run's one repair on a correction re-ask and
// dispatches the answer once. allowTool reports whether a tool turn
// remains to use a tool result: tool turns pass true (a tool call
// executes and the loop continues); the reserved final turn passes
// false (a tool call there is terminally invalid — the tools are spent
// and no turn remains to use the result). An accepted final stores the
// briefing; anything else falls back. True means terminal, except a
// tool turn's executed tool call, which continues the loop.
func (l *agentLoop) repairFlow(turnCtx context.Context, correction string, allowTool bool) (done bool) {
	// correction is a fixed validation classification, never model text.
	log.Printf("briefing: group %s agent output rejected (%s)", l.taken.GroupID, correction)
	prompt := repairPromptForTurn(correction)
	if !allowTool {
		prompt += " This is the final turn: return the complete final briefing only; no further tool calls are allowed."
	}
	rep, failReason, ok := l.repairAsk(turnCtx, prompt)
	if !ok {
		if failReason == "" {
			failReason = "agent output invalid; briefing from deterministic baseline only"
		}
		l.fallback = failReason
		return true
	}
	if over := l.appendAssistant(rep); over != "" {
		l.fallback = over
		return true
	}
	c2, f2, correction := parseTurn(rep)
	if c2 != nil {
		if !allowTool {
			log.Printf("briefing: group %s agent tool call on the final turn", l.taken.GroupID)
			l.fallback = "agent output invalid; briefing from deterministic baseline only"
			return true
		}
		res := l.env.runTool(turnCtx, c2)
		if corr, rej := isReject(res); rej {
			log.Printf("briefing: group %s agent tool rejected after repair (%s)", l.taken.GroupID, corr)
			l.fallback = "agent output invalid; briefing from deterministic baseline only"
			return true
		}
		if over := l.appendTool(res); over != "" {
			l.fallback = over
			return true
		}
		return false
	}
	if f2 {
		a := acceptAnswer([]byte(rep), l.taken, l.issued, l.d.Redaction.Enabled, l.maxToolStep())
		if a.OK {
			insertModelBriefing(l.parent, l.d, l.taken, l.outcome, a, l.issued, l.display, "agent", l.traces(), l.usage)
			return true
		}
		log.Printf("briefing: group %s agent final invalid after repair (%s)", l.taken.GroupID, a.Correction)
	} else {
		log.Printf("briefing: group %s agent turn invalid after repair (%s)", l.taken.GroupID, correction)
	}
	l.fallback = "agent output invalid; briefing from deterministic baseline only"
	return true
}

// transcriptBlockedError marks a pre-send scan hit: the transcript is
// never transmitted, so no provider call is counted for it.
type transcriptBlockedError struct{ reason string }

func (e *transcriptBlockedError) Error() string { return e.reason }

// turn sends one transcript and counts the provider call (Designv9
// §6.3: health reports actual calls). Before anything leaves the
// machine, the final serialized transcript is scanned fail-closed:
// the bundle and each tool result were scanned at creation, but
// assistant and repair texts join later turns verbatim, so only the
// composed send is conclusive. A hit stores the deterministic fallback
// without transmitting. The count covers every actual send, including
// one the context then cancels — the call happened.
func (l *agentLoop) turn(turnCtx context.Context) (string, error) {
	if reason := l.scanTranscript(); reason != "" {
		return "", &transcriptBlockedError{reason: reason}
	}
	res, err := l.turner.Turn(turnCtx, l.env.transcript)
	l.d.Health.AddProviderCall()
	l.d.Health.AddUsage(res.Usage)
	l.usage = l.usage.Add(res.Usage)
	return res.Text, err
}

// scanTranscript enforces the run byte cap and the fail-closed leak
// scan on the exact bytes about to cross. The skip set is the cleared
// event IDs only — model-written IDs stay untrusted until validated.
func (l *agentLoop) scanTranscript() string {
	raw, err := json.Marshal(l.env.transcript)
	if err != nil || len(raw) > maxTranscriptBytes {
		return "agent transcript exceeds size cap; briefing from deterministic baseline only"
	}
	// Duplicate JSON keys fail the send closed before anything else:
	// the structural pass below normalizes message JSON through a map,
	// where a shadowed first value escapes inspection while the
	// provider receives the verbatim text.
	for _, m := range l.env.transcript {
		if messageHasDuplicateKeys(m.Text) {
			log.Printf("briefing: group %s agent transcript blocked on duplicate JSON keys", l.taken.GroupID)
			return "agent transcript blocked by redaction; briefing from deterministic baseline only"
		}
	}
	cleared := make(map[string]bool, len(l.env.seenEvents))
	var ids []string
	for id := range l.env.seenEvents {
		if id == "" {
			continue
		}
		cleared[id] = true
		ids = append(ids, id)
	}
	// Cleared event IDs ride the transcript embedded in message text
	// (bundle JSON, finals, tool results), where the scanner's exact
	// whole-leaf skip cannot see them: a producer UUID's trailing
	// 12-hex segment then matches the bare-12-hex MAC leg and blocks a
	// clean run. Exempt them only in structurally identified reference
	// fields — event_id scalars and event_ids array elements in decoded
	// message JSON. Never scrubbed as arbitrary substrings: a short
	// cleared ID such as "10" must not rewrite a raw 10.23.45.67
	// elsewhere in the text into unscannable xxxx.23.45.67 while the
	// original address transmits. Everything outside the reference
	// fields — including uncleared values sitting in an event_id
	// position — is scanned verbatim.
	scrubbed := neutralizeReferenceIDs(raw, cleared)
	if hits, err := redactor.ScanOutboundLeaks(scrubbed, l.issued, ids); err != nil || len(hits) > 0 {
		log.Printf("briefing: group %s agent transcript blocked at %v", l.taken.GroupID, hits)
		return "agent transcript blocked by redaction; briefing from deterministic baseline only"
	}
	return ""
}

// messageHasDuplicateKeys reports whether text parses as exactly one
// JSON value holding a repeated object key at any depth. Only such
// texts are normalized by neutralizeReferenceIDs below; anything else
// (plain prose, malformed JSON, trailing data) is scanned verbatim, so
// no shadowed value can escape inspection there.
func messageHasDuplicateKeys(text string) bool {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 2 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	first, err := dec.Token()
	if err != nil {
		return false
	}
	type frame struct {
		obj     bool
		keys    map[string]bool
		wantKey bool
	}
	var stack []frame
	var found bool
	if d, ok := first.(json.Delim); !ok || (d != '{' && d != '[') {
		return false
	} else {
		var keys map[string]bool
		if d == '{' {
			keys = map[string]bool{}
		}
		stack = append(stack, frame{obj: d == '{', keys: keys, wantKey: d == '{'})
	}
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].obj {
			stack[n-1].wantKey = true
		}
	}
	for len(stack) > 0 {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				top := &stack[len(stack)-1]
				if top.obj {
					if top.wantKey {
						return false
					}
					top.wantKey = true
				}
				var keys map[string]bool
				if d == '{' {
					keys = map[string]bool{}
				}
				stack = append(stack, frame{obj: d == '{', keys: keys, wantKey: d == '{'})
			case '}', ']':
				top := stack[len(stack)-1]
				if (d == '}') != top.obj {
					return false
				}
				stack = stack[:len(stack)-1]
				valueDone()
			default:
				return false
			}
			continue
		}
		top := &stack[len(stack)-1]
		if top.obj && top.wantKey {
			key, ok := tok.(string)
			if !ok {
				return false
			}
			// Record, don't return yet: trailing data below decides
			// whether the structural pass normalizes this text at all.
			if top.keys[key] {
				found = true
			}
			top.keys[key] = true
			top.wantKey = false
			continue
		}
		valueDone()
	}
	// Trailing data means Unmarshal would refuse the text and the
	// structural pass would leave it verbatim: no normalization loss.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return false
	}
	return found
}

// neutralizeReferenceIDs re-marshals the transcript with cleared event
// IDs replaced by xxxx, but only inside event_id scalar fields and
// event_ids arrays of decoded message texts. Messages that are not JSON
// pass through untouched and are scanned verbatim. The xxxx run matches
// no leak leg, so only the exempted references go quiet.
func neutralizeReferenceIDs(raw []byte, cleared map[string]bool) []byte {
	var msgs []provider.TurnMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return raw
	}
	changed := false
	for i := range msgs {
		var inner any
		if err := json.Unmarshal([]byte(msgs[i].Text), &inner); err != nil {
			continue
		}
		switch inner.(type) {
		case map[string]any, []any:
			msgs[i].Text = string(mustMarshal(neutralizeIDRefs(inner, cleared)))
			changed = true
		}
	}
	if !changed {
		return raw
	}
	if out, err := json.Marshal(msgs); err == nil {
		return out
	}
	return raw
}

// neutralizeIDRefs replaces cleared event IDs at reference positions:
// an event_id scalar holding a cleared ID, or cleared elements of an
// event_ids array, at any depth. Anything else — including values the
// cleared set does not contain — is left for the scanner.
func neutralizeIDRefs(v any, cleared map[string]bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k != "event_id" && k != "event_ids" {
				t[k] = neutralizeIDRefs(val, cleared)
				continue
			}
			t[k] = neutralizeIDValue(val, cleared)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = neutralizeIDRefs(e, cleared)
		}
		return t
	default:
		return v
	}
}

// neutralizeIDValue exempts one reference position: a cleared scalar,
// or the cleared elements of an ID array. Uncleared values stay.
func neutralizeIDValue(v any, cleared map[string]bool) any {
	switch t := v.(type) {
	case string:
		if cleared[t] {
			return "xxxx"
		}
		return t
	case []any:
		for i, e := range t {
			if s, ok := e.(string); ok && cleared[s] {
				t[i] = "xxxx"
			}
		}
		return t
	default:
		return v
	}
}

func mustMarshal(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return raw
}

// turnFailed maps a turn error: a dead parent leaves fallback empty so
// finish reports no progress (the worker release/retry path owns the
// group — never a fallback write for a group the model never saw); any
// live-parent failure stores the deterministic fallback.
func (l *agentLoop) turnFailed(err error) {
	log.Printf("briefing: group %s agent turn: %s", l.taken.GroupID, store.SanitizeErrorText(err.Error()))
	if l.parent.Err() != nil {
		return
	}
	var blocked *transcriptBlockedError
	if errors.As(err, &blocked) {
		l.fallback = blocked.reason
		return
	}
	l.fallback = "agent turn failed; briefing from deterministic baseline only"
}

// repairAsk spends the run's one repair on a correction re-ask sharing
// the affected turn's context and deadline. False means the repair was
// already spent (a second failure falls back) or the re-ask failed.
func (l *agentLoop) repairAsk(turnCtx context.Context, prompt string) (rep, failReason string, ok bool) {
	if l.repair {
		return "", "", false
	}
	l.repair = true
	if over := l.appendUser(prompt); over != "" {
		return "", over, false
	}
	rep, err := l.turn(turnCtx)
	if err != nil {
		log.Printf("briefing: group %s agent repair: %s", l.taken.GroupID, store.SanitizeErrorText(err.Error()))
		var blocked *transcriptBlockedError
		if errors.As(err, &blocked) {
			return "", blocked.reason, false
		}
		return "", "", false
	}
	return rep, "", true
}

// runCtx returns the run context. Stored beside the parent because the
// loop derives per-turn children from the run deadline while storing
// and fallback-mapping against the parent.
func (l *agentLoop) runCtx() context.Context {
	return l.ctx
}

// maxToolStep bounds the citable executed steps: the highest 0-based
// index so far, or -1 before the first execution, when no tool ref is
// real yet.
func (l *agentLoop) maxToolStep() int {
	return len(l.env.execs) - 1
}

// traces converts the run's executed steps into store rows addressed
// at this group's briefing for the atomic insert.
func (l *agentLoop) traces() []store.AgentTrace {
	return l.env.traces("brf-" + l.taken.GroupID)
}

// appendAssistant appends one assistant message, enforcing the run byte
// cap before any send. It returns the fallback reason when over budget.
func (l *agentLoop) appendAssistant(text string) string {
	return l.append(provider.TurnMessage{Role: "assistant", Text: text})
}

// appendTool appends one tool result, enforcing the run byte cap.
func (l *agentLoop) appendTool(text string) string {
	return l.append(provider.TurnMessage{Role: "tool", Text: text})
}

// appendUser appends one instruction message, enforcing the run byte cap.
func (l *agentLoop) appendUser(text string) string {
	return l.append(provider.TurnMessage{Role: "user", Text: text})
}

func (l *agentLoop) append(m provider.TurnMessage) string {
	l.env.transcript = append(l.env.transcript, m)
	raw, err := json.Marshal(l.env.transcript)
	if err != nil || len(raw) > maxTranscriptBytes {
		return "agent transcript exceeds size cap; briefing from deterministic baseline only"
	}
	return ""
}

// subjectRef is one registry entry: a group-linked (or run-granted)
// subject the tools may report on. The registry is the authorization
// boundary — a valid issued token is never authorization; only
// membership here admits a subject to the tools.
type subjectRef struct {
	profileID   string
	role        string
	class       string
	upstreamRef string
	alias       string // run-scoped SUBJ_n, "" until granted by find_similar
}

// agentEnv is the tool execution environment: the store, the group, the
// token maps, and the registry. Tools are read-only and bounded; every
// real value renders through the token map or is omitted with a count.
type agentEnv struct {
	st         *store.Store
	scope      string // run source scope; tools never cross it
	taken      store.TakenGroup
	byValue    map[string]string // real value → issued token (exact case)
	byLower    map[string]string // lower(value) → token (first wins)
	now        time.Time
	reg        map[string]*subjectRef // by profileID
	order      []string
	byAlias    map[string]string // SUBJ_n → profileID, minted by find_similar
	byToken    map[string]string // bundle token → profileID, resolved at seed
	issuedSet  map[string]bool   // every token valid this run: bundle + aliases
	seenEvents map[string]bool   // every event ID already cleared to cross:
	// the group's own plus scanned-clean tool result events
	transcript []provider.TurnMessage
	execs      []toolExec // executed tool calls in order; finals cite them by index
}

// newAgentEnv builds the environment with the inverted token map. The
// inversion is exact: only values the bundle issued map to tokens, so
// tool evidence reuses existing pseudonyms and never mints new ones.
func newAgentEnv(st *store.Store, taken store.TakenGroup, issued redactor.TokenMap, now time.Time) *agentEnv {
	byValue := make(map[string]string, len(issued))
	for tok, val := range issued {
		if _, dup := byValue[val]; !dup {
			byValue[val] = tok
		}
	}
	byLower := make(map[string]string, len(issued))
	for tok, val := range issued {
		lower := strings.ToLower(val)
		if _, dup := byLower[lower]; !dup {
			byLower[lower] = tok
		}
	}
	if now.IsZero() {
		now = time.Now()
	}
	issuedSet := make(map[string]bool, len(issued))
	for tok := range issued {
		issuedSet[tok] = true
	}
	seenEvents := make(map[string]bool, len(taken.Events))
	for _, ev := range taken.Events {
		seenEvents[ev.EventID] = true
	}
	return &agentEnv{st: st, taken: taken, byValue: byValue, byLower: byLower, now: now,
		reg: map[string]*subjectRef{}, byAlias: map[string]string{}, byToken: map[string]string{},
		issuedSet: issuedSet, seenEvents: seenEvents}
}

// seed loads the group-linked subjects as the tool registry. A subject
// whose stored source scope differs from the run's is excluded: tools
// never cross a source boundary. Failures here fail the run closed — a
// loop without a registry could not authorize any tool call. Bundle
// tokens resolving to a registry subject's upstream reference are
// indexed, so host-token arguments map to subjects without touching
// raw values outside this layer.
func (e *agentEnv) seed(ctx context.Context, sourceScope string) string {
	links, truncated, err := e.st.GroupSubjects(ctx, e.taken.GroupID)
	if err != nil {
		return "agent subject registry unavailable; briefing from deterministic baseline only"
	}
	_ = truncated
	e.scope = sourceScope
	for _, link := range links {
		rep, err := e.st.ReportedProfile(ctx, link.ProfileID)
		if err != nil {
			continue // purged subject: absent from the registry, never an error
		}
		if sourceScope != "" && rep.SourceScope != sourceScope {
			continue
		}
		if _, dup := e.reg[link.ProfileID]; dup {
			continue
		}
		ref := &subjectRef{profileID: link.ProfileID, role: link.Role,
			class: rep.ReferenceClass, upstreamRef: rep.UpstreamReference}
		e.reg[link.ProfileID] = ref
		e.order = append(e.order, link.ProfileID)
		if tok, ok := e.byValue[rep.UpstreamReference]; ok && rep.UpstreamReference != "" {
			if _, dup := e.byToken[tok]; !dup {
				e.byToken[tok] = link.ProfileID
			}
		}
	}
	return ""
}

// grantSubjects registers find_similar subjects in the run registry
// under run-scoped aliases. Profiles outside the run scope, or gone
// since the search, grant nothing — the omitted count says so. Granted
// aliases join the run token set, so later turns may name them; their
// upstream references index like seeded subjects when a bundle token
// already names the same value.
func (e *agentEnv) grantSubjects(ctx context.Context, profiles []string) []string {
	aliases := make([]string, 0, len(profiles))
	for _, id := range profiles {
		ref, ok := e.reg[id]
		if !ok {
			rep, err := e.st.ReportedProfile(ctx, id)
			if err != nil {
				continue
			}
			if e.scope != "" && rep.SourceScope != e.scope {
				continue
			}
			ref = &subjectRef{profileID: id, role: "similar",
				class: rep.ReferenceClass, upstreamRef: rep.UpstreamReference}
			e.reg[id] = ref
			e.order = append(e.order, id)
		}
		if ref.alias == "" {
			ref.alias = "SUBJ_" + strconv.Itoa(len(e.byAlias)+1)
			e.byAlias[ref.alias] = id
			e.mintAlias(ref.alias)
		}
		if tok, ok := e.byValue[ref.upstreamRef]; ok && ref.upstreamRef != "" {
			if _, dup := e.byToken[tok]; !dup {
				e.byToken[tok] = id
			}
		}
		aliases = append(aliases, ref.alias)
	}
	return aliases
}

// resolveHost maps a host-token argument to a registry subject. Run
// aliases resolve directly; bundle tokens resolve through the upstream
// reference they were issued for. A token outside the run set is a
// scope violation; a run token naming no registry subject is unknown
// data (the model used a real token that names nothing permitted).
func (e *agentEnv) resolveHost(token string) (ref *subjectRef, inRun bool) {
	if id, ok := e.byAlias[token]; ok {
		return e.reg[id], true
	}
	if id, ok := e.byToken[token]; ok {
		return e.reg[id], true
	}
	if e.issuedSet[token] {
		return nil, true
	}
	return nil, false
}

// mintAlias registers a run-scoped subject token in the run set so the
// result scan accepts it and later turns may name it.
func (e *agentEnv) mintAlias(alias string) {
	e.issuedSet[alias] = true
}

// systemPrompt is the fixed instruction text (Designv9 §6.2): tool
// schemas, the baseline, and the token rules. It carries no payload
// free text — no banner, hostname, or evidence string — and no subject
// identifiers: the model reads tokens from the bundle and chains
// subject tokens from earlier tool results.
func (e *agentEnv) systemPrompt(outcome Outcome) string {
	return "You investigate one network-monitoring group. The user message carries the redacted bundle " +
		"(tokens like IP_1 stand for real addresses; treat them as opaque identifiers). " +
		"Baseline priority: " + outcome.Level.String() + " (" + outcome.RuleID + "). " +
		"You may call tools, one per turn, or answer. Reply with exactly one JSON object and no other text: " +
		"a tool call {\"tool\": \"<name>\", \"arguments\": {...}} or the final briefing " +
		"{\"event_ids\": [...all group event IDs...], \"host\": \"<one bundle token>\", \"what_changed\": \"...\", " +
		"\"context\": \"...\", \"model_suggestion\": \"low\"|\"medium\"|\"high\"|\"critical\"|null, " +
		"\"priority_reason\": \"...\", \"recommended_check\": \"...\", " +
		"\"likely_explanation\": \"...\", \"confidence\": \"low\"|\"medium\"|\"high\", \"evidence_refs\": [...]}. " +
		"likely_explanation is your display-only hypothesis (at most 256 chars); evidence_refs holds at most 8 citations, " +
		"each {\"event_id\": \"<a group event ID>\"} or {\"tool_step\": <a 0-based executed tool step of this run>}. " +
		"Tools (integer arguments are JSON numbers): " +
		"get_reported_profile{host} (scanner-reported attributes, provenance, report age, reference class, missing fields), " +
		"get_reported_history{host, limit 1-10 default 4} (retained briefings and associated reported events, newest first), " +
		"find_similar_reported_changes{kind: appeared|disappeared|changed, port 1-65535 optional, window_days 1-30 default 7} " +
		"(counts of retained reports plus up to 5 subject tokens, no identity association), " +
		"get_reported_services{host, proto tcp|udp optional, port 1-65535 optional} (explicitly reported services and states, or unknown), " +
		"compare_reported_context{host} (bounded comparison of retained samples with denominators and time window). " +
		"Rules: host arguments are issued tokens — bundle tokens or subject tokens (SUBJ_n) from earlier tool results. " +
		"Fixed enums and bounded integers only; no other tools exist."
}

// toolCall is one parsed assistant tool turn.
type toolCall struct {
	name string
	args map[string]any
}

// parseTurn classifies one assistant text: a strict tool call, a final
// briefing shape, or invalid with a schema-kind correction (Designv9
// §6.2: invalid actions and unknown fields repair once, then fall
// back). The optional why carries model reasoning and is ignored.
// Unknown fields reject in both shapes, so the model cannot smuggle new
// instructions past validation.
func parseTurn(text string) (call *toolCall, final bool, correction string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, false, "empty turn"
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &shape); err != nil {
		return nil, false, "malformed turn JSON"
	}
	if rawTool, ok := shape["tool"]; ok {
		var name string
		if err := json.Unmarshal(rawTool, &name); err != nil || name == "" {
			return nil, false, "tool name must be a non-empty string"
		}
		args := map[string]any{}
		if rawArgs, ok := shape["arguments"]; ok {
			dec := json.NewDecoder(strings.NewReader(string(rawArgs)))
			dec.UseNumber()
			var anyArgs map[string]any
			if err := dec.Decode(&anyArgs); err != nil {
				return nil, false, "tool arguments must be an object"
			}
			if dec.More() {
				return nil, false, "trailing data after tool arguments"
			}
			args = anyArgs
		}
		if rawWhy, ok := shape["why"]; ok {
			var why string
			if err := json.Unmarshal(rawWhy, &why); err != nil {
				return nil, false, "why must be a string"
			}
		}
		for k := range shape {
			if k != "tool" && k != "arguments" && k != "why" {
				return nil, false, "unknown field in tool call"
			}
		}
		if !agentTools[name] {
			return nil, false, "unknown tool"
		}
		return &toolCall{name: name, args: args}, false, ""
	}
	// Final-briefing shape: event_ids present distinguishes it from a
	// tool call missing its name. Full validation stays in acceptAnswer.
	if _, ok := shape["event_ids"]; ok {
		for k := range shape {
			switch k {
			case "event_ids", "host", "what_changed", "context",
				"model_suggestion", "priority_reason", "recommended_check",
				"likely_explanation", "confidence", "evidence_refs":
			default:
				return nil, false, "unknown field in final briefing"
			}
		}
		return nil, true, ""
	}
	return nil, false, "turn is neither a tool call nor a final briefing"
}

// agentTools is the Designv9 §6.2 tool allowlist: five read-only,
// bounded tools. Nothing else exists.
var agentTools = map[string]bool{
	"get_reported_profile":          true,
	"get_reported_history":          true,
	"find_similar_reported_changes": true,
	"get_reported_services":         true,
	"compare_reported_context":      true,
}

// intArg extracts a bounded integer argument: JSON numbers only, no
// strings, no floats. Missing with a default is allowed; anything else
// is an invalid turn.
func intArg(args map[string]any, key string, def, min, max int) (int, string) {
	raw, ok := args[key]
	if !ok {
		return def, ""
	}
	n, ok := raw.(json.Number)
	if !ok {
		return 0, key + " must be an integer"
	}
	v, err := strconv.Atoi(string(n))
	if err != nil {
		return 0, key + " must be an integer"
	}
	if v < min || v > max {
		return 0, key + " out of range"
	}
	return v, ""
}

// strArg extracts an optional string argument. Present-but-not-string
// is an invalid turn.
func strArg(args map[string]any, key string) (string, bool, string) {
	raw, ok := args[key]
	if !ok {
		return "", false, ""
	}
	s, ok := raw.(string)
	if !ok {
		return "", false, key + " must be a string"
	}
	return s, true, ""
}

// runTool executes one authorized tool call and returns its serialized
// envelope. Schema violations (unknown arguments, bad types, unknown
// or out-of-run tokens) reject the turn — the caller repairs or falls
// back. Store failures and empty data become error/unknown results, so
// the model can try another tool within its step budget. Every
// envelope is leak-scanned before return (Designv9 §6.3): a hit becomes
// a generic redaction_blocked result and the run stays alive.
func (e *agentEnv) runTool(turnCtx context.Context, call *toolCall) string {
	for k := range call.args {
		var allowed bool
		switch call.name {
		case "get_reported_profile", "compare_reported_context":
			allowed = k == "host"
		case "get_reported_history":
			allowed = k == "host" || k == "limit"
		case "find_similar_reported_changes":
			allowed = k == "kind" || k == "port" || k == "window_days"
		case "get_reported_services":
			allowed = k == "host" || k == "proto" || k == "port"
		}
		if !allowed {
			return turnReject("unknown argument")
		}
	}
	args, reject := e.checkArgs(call)
	if reject != "" {
		return turnReject(reject)
	}
	// The call is authorized: whatever follows is an execution the
	// trace records — including error and blocked envelopes, which are
	// fixed safe shapes. Rejected turns (above) never execute and leave
	// no row: their free text cannot be tokenized safely.
	start := time.Now()
	var result map[string]any
	var events []string
	var omitted int
	var truncated bool
	storeFailed := false
	switch call.name {
	case "get_reported_profile":
		result, events, omitted, truncated, storeFailed = e.toolProfile(turnCtx, args.sub)
	case "get_reported_history":
		result, events, omitted, truncated, storeFailed = e.toolHistory(turnCtx, args.sub, args.limit)
	case "find_similar_reported_changes":
		result, events, omitted, truncated, storeFailed = e.toolSimilar(turnCtx, args.kind, args.port, args.windowDays)
	case "get_reported_services":
		result, events, omitted, truncated, storeFailed = e.toolServices(turnCtx, args.sub, args.proto, args.port)
	case "compare_reported_context":
		result, events, omitted, truncated, storeFailed = e.toolCompare(turnCtx, args.sub)
	default:
		return turnReject("unknown tool")
	}
	if storeFailed {
		return e.record(call, toolError("store unavailable"), "error", start)
	}
	env := map[string]any{"tool": call.name, "result": result,
		"truncated": truncated, "omitted": omitted}
	raw, err := json.Marshal(env)
	if err != nil || len(raw) > maxToolResultBytes {
		// Unreachable by construction (fields are budgeted below), but
		// a tool must never emit over-budget or invalid JSON.
		return e.record(call, toolError("result over budget"), "error", start)
	}
	// Final gate before anything leaves the machine (Designv9 §6.3):
	// the serialized result is scanned fail-closed. A hit never reaches
	// the model — the generic result does, and the run stays alive.
	if hits, err := redactor.ScanOutboundLeaks(raw, e.issuedMap(), e.scanEvents(events)); err != nil || len(hits) > 0 {
		log.Printf("briefing: agent tool %s redaction blocked at %v", call.name, hits)
		return e.record(call, toolBlocked(call.name), "blocked", start)
	}
	// Clean results join the transcript skip set: their event IDs are
	// cleared to cross in later turns. Blocked results contribute
	// nothing — the generic envelope names no events.
	for _, id := range events {
		if id != "" {
			e.seenEvents[id] = true
		}
	}
	return e.record(call, string(raw), "ok", start)
}

// toolExec is one executed tool call awaiting storage: the validated
// arguments and the scanned result envelope, both tokenized, in
// execution order. The step index is the slice index — dense and
// 0-based by construction, so finals can cite it.
type toolExec struct {
	tool     string
	args     string
	result   string
	status   string
	duration time.Duration
}

// record appends one executed tool call to the run trace and returns
// the envelope unchanged. Args are the validated call arguments only:
// fixed enums, bounded integers and issued tokens — never model free
// text — so the row is tokenized by construction.
func (e *agentEnv) record(call *toolCall, result, status string, start time.Time) string {
	args, err := json.Marshal(call.args)
	if err != nil {
		args = []byte(`{}`)
	}
	e.execs = append(e.execs, toolExec{
		tool: call.name, args: string(args), result: result,
		status: status, duration: time.Since(start),
	})
	return result
}

// traces converts the executed steps into store rows for the atomic
// briefing insert. Duration rides in whole milliseconds; sub-ms runs
// record zero, which reads as instant, not missing.
func (e *agentEnv) traces(briefingID string) []store.AgentTrace {
	if len(e.execs) == 0 {
		return nil
	}
	out := make([]store.AgentTrace, 0, len(e.execs))
	for i, ex := range e.execs {
		out = append(out, store.AgentTrace{
			BriefingID: briefingID, Step: i, Tool: ex.tool,
			ArgsJSON: ex.args, ResultJSON: ex.result,
			Status: ex.status, DurationMS: ex.duration.Milliseconds(),
		})
	}
	return out
}

// toolError is the error-result envelope: a fixed shape, never
// truncated JSON. The run continues — the model may try another tool.
func toolError(reason string) string {
	raw, _ := json.Marshal(map[string]any{"tool": "", "result": map[string]any{"error": reason},
		"truncated": false, "omitted": 0})
	return string(raw)
}

// toolBlocked is the generic redaction_blocked tool result (Designv9
// §6.3): it names no values and no paths, only the safe tool name. The
// log line at the scan site carries the classification.
func toolBlocked(name string) string {
	raw, _ := json.Marshal(map[string]any{"tool": name, "result": map[string]any{"error": "redaction_blocked"},
		"truncated": false, "omitted": 0})
	return string(raw)
}

// turnReject marks a schema or scope violation: the turn is rejected,
// and the loop repairs once or falls back. It must be distinguishable
// from a tool error result, so the dispatcher never mistakes one for
// the other.
func turnReject(correction string) string {
	return "\x00reject:" + correction
}

// isReject reports a dispatcher-level rejection.
func isReject(s string) (string, bool) {
	if strings.HasPrefix(s, "\x00reject:") {
		return strings.TrimPrefix(s, "\x00reject:"), true
	}
	return "", false
}

// toolArgs are the validated arguments for one tool call: host tokens
// resolve to registry subjects (nil when the token is issued but names
// no permitted subject), enums are fixed, integers bounded.
type toolArgs struct {
	host       string
	sub        *subjectRef
	kind       string
	proto      string
	limit      int
	port       int
	windowDays int
}

// checkArgs validates one tool call's arguments (Designv9 §6.2: fixed
// enums and bounded integers only). A non-empty rejection means the
// turn is rejected — the loop repairs once or falls back. Subject
// resolution happens here: a token outside the run set is a scope
// violation (reject), while an issued token naming no registry subject
// yields a nil sub (unknown data, the run continues).
func (e *agentEnv) checkArgs(call *toolCall) (toolArgs, string) {
	var a toolArgs
	host := ""
	if h, present, reject := strArg(call.args, "host"); reject != "" {
		return a, reject
	} else if present {
		host = h
	}
	switch call.name {
	case "get_reported_profile", "compare_reported_context":
		if host == "" {
			return a, "host is required"
		}
	case "get_reported_history":
		if host == "" {
			return a, "host is required"
		}
		limit, reject := intArg(call.args, "limit", 4, 1, 10)
		if reject != "" {
			return a, reject
		}
		a.limit = limit
	case "find_similar_reported_changes":
		kind, present, reject := strArg(call.args, "kind")
		if reject != "" {
			return a, reject
		}
		if !present || kind == "" {
			return a, "kind is required"
		}
		if _, ok := store.SimilarKinds[kind]; !ok {
			return a, "unknown kind"
		}
		a.kind = kind
		port, reject := intArg(call.args, "port", 0, 1, 65535)
		if reject != "" {
			return a, reject
		}
		a.port = port
		days, reject := intArg(call.args, "window_days", 7, 1, 30)
		if reject != "" {
			return a, reject
		}
		a.windowDays = days
	case "get_reported_services":
		if host == "" {
			return a, "host is required"
		}
		proto, present, reject := strArg(call.args, "proto")
		if reject != "" {
			return a, reject
		}
		if present {
			if normEnum(proto, "tcp", "udp") == "" {
				return a, "unknown proto"
			}
			a.proto = normEnum(proto, "tcp", "udp")
		}
		port, reject := intArg(call.args, "port", 0, 1, 65535)
		if reject != "" {
			return a, reject
		}
		a.port = port
	}
	if host != "" {
		sub, inRun := e.resolveHost(host)
		if !inRun {
			return a, "unknown token"
		}
		a.host = host
		a.sub = sub // nil when issued but naming no permitted subject
	}
	return a, ""
}

// issuedMap rebuilds the token map for the result scan: the scan only
// accepts what the bundle issued, so minted aliases must not ride it.
func (e *agentEnv) issuedMap() redactor.TokenMap {
	out := make(redactor.TokenMap, len(e.byValue))
	for v, t := range e.byValue {
		out[t] = v
	}
	return out
}

// scanEvents gathers every event ID the envelope names so the result
// scan skips known opaque IDs exactly like the bundle scan does: the
// group's own events plus the per-tool associated events.
func (e *agentEnv) scanEvents(toolEvents []string) []string {
	seen := make(map[string]bool, len(e.taken.Events)+len(toolEvents))
	var out []string
	for _, ev := range e.taken.Events {
		if !seen[ev.EventID] {
			seen[ev.EventID] = true
			out = append(out, ev.EventID)
		}
	}
	for _, id := range toolEvents {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// toolFieldBudget bounds the rendered result fields. The envelope adds
// roughly a hundred bytes, so the finished result stays well under the
// 4 KiB result cap; the exact marshal-then-measure check on the finished
// envelope stays the hard guarantee regardless.
const toolFieldBudget = 3840

// portListByteCap bounds one rendered port list: rows accumulate while
// the list JSON stays within it, so long service names and five-digit
// ports truncate rows instead of dropping the whole list at put time.
// 3456 bytes admit 64 compact rows (~3.4 KiB) with room for sibling
// fields and the envelope inside the 4 KiB cap.
const portListByteCap = 3456

// toolOut accumulates one tool result with an exact byte budget.
// Fields are priority-ordered by insertion: the first fields win, the
// rest drop with the truncated flag — never a cut inside the JSON.
type toolOut struct {
	m         map[string]any
	omitted   int
	truncated bool
	bytes     int
}

func (o *toolOut) put(key string, v any) {
	if o.m == nil {
		o.m = map[string]any{}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		o.omitted++
		return
	}
	// An over-budget field is skipped alone and marks truncation;
	// later fields that fit are still kept. Dropping everything past
	// the first overflow would discard usable bounded results.
	if o.bytes+len(key)+len(raw)+8 > toolFieldBudget {
		o.truncated = true
		o.omitted++
		return
	}
	o.m[key] = v
	o.bytes += len(key) + len(raw) + 8
}

// token renders a real value as its issued token, exact case first,
// then case-insensitive (the result scan matches case-insensitively,
// so rendering must too or a "Mail"/"mail" split trips it). Unmapped
// strings are omitted, never leaked: the omitted count says data was
// withheld, and the table's "missing fields" read the same way. Empty
// strings carry nothing and pass through.
func (e *agentEnv) token(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if tok, ok := e.byValue[s]; ok {
		return tok, false
	}
	if tok, ok := e.byLower[strings.ToLower(s)]; ok {
		return tok, false
	}
	return "", true
}

// valueWord reports whether a raw word would trip the result scan:
// some issued value (≥3 runes, the scan's floor) contains it
// word-bounded and case-insensitively. Callers omit such words instead
// of emitting text the scan must block.
func (e *agentEnv) valueWord(word string) bool {
	lower := strings.ToLower(word)
	for v := range e.byValue {
		if len([]rune(v)) < 3 {
			continue
		}
		if lower == strings.ToLower(v) {
			return true
		}
	}
	return false
}

func (e *agentEnv) putToken(o *toolOut, key, s string) {
	tok, withheld := e.token(s)
	if withheld {
		o.omitted++
		return
	}
	o.put(key, tok)
}

// normEnum keeps only fixed-vocabulary enum spellings
// (case-insensitive): any other string — hostile or merely new — is
// rejected or omitted, never passed as free text.
func normEnum(s string, allow ...string) string {
	lower := strings.ToLower(s)
	for _, a := range allow {
		if lower == a {
			return a
		}
	}
	return ""
}

func (e *agentEnv) renderPorts(o *toolOut, ports []store.ReportedPort, cap int) {
	type portRow struct {
		Port    int    `json:"port"`
		Proto   string `json:"proto"`
		State   string `json:"state"`
		Service string `json:"service"`
	}
	rows := make([]portRow, 0, len(ports))
	for _, p := range ports {
		// The count cap bounds the retained list; reaching it keeps the
		// collected rows and marks truncation — it must not discard
		// them by returning before the put below.
		if len(rows) >= cap {
			o.truncated = true
			break
		}
		proto := normEnum(p.Proto, "tcp", "udp")
		state := normEnum(p.State, "open", "closed", "filtered")
		if proto == "" || state == "" {
			o.omitted++
			continue
		}
		// Service labels are scanner-reported: a known value renders
		// as its token; enum-shaped words pass only when no issued
		// value collides with them; anything else is unknown, never
		// free text.
		svc, svcOmitted := e.serviceWord(p.Service)
		if svcOmitted {
			o.omitted++
		}
		rows = append(rows, portRow{Port: p.Port, Proto: proto, State: state, Service: svc})
		// Incremental byte budget: keep the prefix that fits, so long
		// rows truncate the list instead of the whole field being
		// omitted at put time. Row counts stay small, so remarshaling
		// the prefix per row is cheap and exact.
		if listBytes, err := json.Marshal(rows); err != nil || len(listBytes) > portListByteCap {
			rows = rows[:len(rows)-1]
			o.truncated = true
			break
		}
	}
	o.put("ports", rows)
}

// renderAttrs renders retained attribute scalars: numbers, booleans,
// and null pass through; strings tokenize or omit; nested values omit
// by type. Keys must be field-shaped, or they omit too — a key is never
// a value smuggling path.
func (e *agentEnv) renderAttrs(o *toolOut, attrs map[string]any, cap int) {
	out := map[string]any{}
	n := 0
	for k, v := range attrs {
		if n >= cap {
			o.truncated = true
			break
		}
		if len(k) == 0 || len(k) > 64 || !isFieldKey(k) {
			o.omitted++
			continue
		}
		n++
		switch t := v.(type) {
		case string:
			tok, withheld := e.token(t)
			if withheld {
				o.omitted++
				continue
			}
			out[k] = tok
		case bool, float64, int, int64, nil:
			out[k] = t
		default:
			o.omitted++
		}
	}
	o.put("attributes", out)
}

// serviceWord renders one scanner-reported service label: known
// values become their tokens; clean single words pass; colliding or
// strange labels become unknown (or omitted when even "unknown" would
// trip the result scan). The second return counts the omission.
func (e *agentEnv) serviceWord(svc string) (string, bool) {
	if svc == "" {
		return "", false
	}
	if tok, withheld := e.token(svc); !withheld {
		return tok, false
	}
	if !isServiceWord(svc) {
		return "unknown", true
	}
	if e.valueWord(svc) {
		return "", true
	}
	return svc, false
}

// isServiceWord admits single-word service labels: 1-24 lowercase
// letters, digits, or dashes. Anything longer or stranger (banners,
// sentences, hostile instructions) is unknown, not model input.
func isServiceWord(s string) bool {
	if len(s) == 0 || len(s) > 24 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func isFieldKey(k string) bool {
	for _, r := range k {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func renderTime(o *toolOut, key string, t time.Time) {
	if t.IsZero() {
		o.put(key, nil)
		return
	}
	o.put(key, t.UTC().Format(time.RFC3339))
}

// toolProfile implements get_reported_profile (Designv9 §6.2): the
// scanner-reported attributes, provenance, report age, reference
// class, and missing fields for one registry subject. A nil subject —
// an issued token naming nothing permitted — is unknown data, not a
// failure: the run continues.
func (e *agentEnv) toolProfile(ctx context.Context, sub *subjectRef) (map[string]any, []string, int, bool, bool) {
	o := &toolOut{}
	if sub == nil {
		o.put("error", "unknown subject")
		return o.m, nil, o.omitted, o.truncated, false
	}
	rep, err := e.st.ReportedProfile(ctx, sub.profileID)
	if err != nil {
		return nil, nil, 0, false, true
	}
	// The subject handle is the run alias when granted; otherwise the
	// caller already holds the bundle token that resolved here, and
	// the upstream reference below echoes the same token.
	if sub.alias != "" {
		o.put("subject", sub.alias)
	} else {
		o.omitted++
	}
	o.put("reference_class", rep.ReferenceClass)
	if rep.IdentityNamespace == "" {
		o.put("identity_namespace", nil)
	} else {
		e.putToken(o, "identity_namespace", rep.IdentityNamespace)
	}
	e.putToken(o, "upstream_reference", rep.UpstreamReference)
	putVocab(o, "reported_identity_source", rep.ReportedIdentitySource, "ip", "mac", "provided")
	putVocab(o, "reported_confidence", rep.ReportedConfidence, "low", "high")
	e.putToken(o, "operator_label", stringOrEmpty(rep.OperatorLabel))
	renderTime(o, "first_report_at", rep.FirstReportAt)
	renderTime(o, "last_report_at", rep.LastReportAt)
	if !rep.LastReportAt.IsZero() {
		o.put("last_seen_age_s", int64(e.now.Sub(rep.LastReportAt)/time.Second))
	}
	o.put("groups", rep.Groups)
	o.put("briefings", rep.Briefings)
	obs, found, err := e.st.LatestSubjectObservation(ctx, sub.profileID)
	if err != nil {
		return nil, nil, 0, false, true
	}
	if !found {
		o.put("latest", nil)
		return o.m, nil, o.omitted, o.truncated, false
	}
	lo := e.renderObservation(obs, 32, 16)
	o.m["latest"] = lo.m
	o.omitted += lo.omitted
	if lo.truncated {
		o.truncated = true
	}
	return o.m, []string{obs.EventID}, o.omitted, o.truncated, false
}

// putVocab emits a declared-vocabulary provenance string, or omits it
// as a missing/unrecognized field. Anything outside the contract
// vocabulary is never model input.
func putVocab(o *toolOut, key, s string, allow ...string) {
	if s == "" {
		o.omitted++
		return
	}
	if normEnum(s, allow...) == "" {
		o.omitted++
		return
	}
	o.put(key, normEnum(s, allow...))
}

func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// renderObservation renders one retained observation with a bounded
// port list and attribute set.
func (e *agentEnv) renderObservation(obs store.SubjectObservation, portCap, attrCap int) *toolOut {
	o := &toolOut{}
	o.put("event_id", obs.EventID)
	putVocab(o, "view_role", obs.ViewRole, "host", "previous", "current")
	renderTime(o, "reported_at", obs.ReportedAt)
	o.put("port_count", obs.PortsTotal)
	o.put("ports_truncated", obs.PortsTruncated)
	e.renderPorts(o, obs.Ports, portCap)
	e.renderAttrs(o, obs.Attributes, attrCap)
	return o
}

// toolHistory implements get_reported_history (Designv9 §6.2): retained
// briefings explicitly associated with the subject, each with its
// associated reported events, plus the newest retained observations —
// all newest first.
func (e *agentEnv) toolHistory(ctx context.Context, sub *subjectRef, limit int) (map[string]any, []string, int, bool, bool) {
	o := &toolOut{}
	var events []string
	if sub == nil {
		o.put("error", "unknown subject")
		return o.m, nil, o.omitted, o.truncated, false
	}
	rows, err := e.st.BriefingsForSubject(ctx, sub.profileID, limit)
	if err != nil {
		return nil, nil, 0, false, true
	}
	items := make([]any, 0, len(rows))
	for _, b := range rows {
		bo := &toolOut{}
		bo.put("priority", b.Priority)
		bo.put("model_suggestion", b.ModelSuggestion)
		bo.put("fallback_used", b.FallbackUsed)
		bo.put("source", b.Source)
		renderTime(bo, "created_at", b.CreatedAt)
		refs, _, err := e.st.BriefingReferencedEvents(ctx, b.GroupID)
		if err != nil {
			return nil, nil, 0, false, true
		}
		ids := make([]string, 0, len(refs))
		for i, r := range refs {
			if i >= 10 {
				bo.truncated = true
				break
			}
			ids = append(ids, r.EventID)
			events = append(events, r.EventID)
		}
		bo.put("event_ids", ids)
		items = append(items, bo.m)
		o.omitted += bo.omitted
		if bo.truncated {
			o.truncated = true
		}
	}
	o.m = map[string]any{}
	o.put("briefings", items)
	obsItems, err := e.st.LatestSubjectObservations(ctx, sub.profileID, limit)
	if err != nil {
		return nil, nil, 0, false, true
	}
	samples := make([]any, 0, len(obsItems))
	for _, ob := range obsItems {
		ro := e.renderObservation(ob, 0, 8)
		samples = append(samples, ro.m)
		events = append(events, ob.EventID)
		o.omitted += ro.omitted
		if ro.truncated {
			o.truncated = true
		}
	}
	o.put("observations", samples)
	return o.m, events, o.omitted, o.truncated, false
}

// toolSimilar implements find_similar_reported_changes (Designv9 §6.2):
// counts of retained reports (distinct events) of one scanner event
// class in the run's scope and window, optionally mentioning a port,
// plus up to five subject tokens granted for this run. No identity
// association: the counts describe reports, never devices.
func (e *agentEnv) toolSimilar(ctx context.Context, kind string, port, windowDays int) (map[string]any, []string, int, bool, bool) {
	o := &toolOut{}
	since := e.now.Add(-time.Duration(windowDays) * 24 * time.Hour)
	count, profiles, err := e.st.SimilarReports(ctx, e.scope, kind, port, since)
	if err != nil {
		return nil, nil, 0, false, true
	}
	aliases := e.grantSubjects(ctx, profiles)
	o.put("kind", kind)
	if port > 0 {
		o.put("port", port)
	} else {
		o.put("port", nil)
	}
	o.put("window_days", windowDays)
	o.put("report_count", count)
	o.put("subjects", aliases)
	o.put("granted", len(aliases))
	o.omitted += len(profiles) - len(aliases)
	return o.m, nil, o.omitted, o.truncated, false
}

// toolServices implements get_reported_services (Designv9 §6.2): the
// services and states the scanner explicitly reported for the
// subject's latest observation, filtered by proto/port. No lookup
// catalogue, no probe: no observation means unknown.
func (e *agentEnv) toolServices(ctx context.Context, sub *subjectRef, proto string, port int) (map[string]any, []string, int, bool, bool) {
	o := &toolOut{}
	if sub == nil {
		o.put("error", "unknown subject")
		return o.m, nil, o.omitted, o.truncated, false
	}
	obs, found, err := e.st.LatestSubjectObservation(ctx, sub.profileID)
	if err != nil {
		return nil, nil, 0, false, true
	}
	if !found {
		o.put("services", "unknown")
		return o.m, nil, o.omitted, o.truncated, false
	}
	ports := make([]store.ReportedPort, 0, len(obs.Ports))
	for _, p := range obs.Ports {
		if proto != "" && normEnum(p.Proto, "tcp", "udp") != proto {
			continue
		}
		if port > 0 && p.Port != port {
			continue
		}
		ports = append(ports, p)
	}
	o.put("event_id", obs.EventID)
	putVocab(o, "view_role", obs.ViewRole, "host", "previous", "current")
	renderTime(o, "reported_at", obs.ReportedAt)
	e.renderPorts(o, ports, 64)
	return o.m, []string{obs.EventID}, o.omitted, o.truncated, false
}

// maxCompareSamples bounds the compare tool's sample count: eight
// samples with capped port lists stay far under the 4 KiB result
// budget. maxComparePortCap bounds one sample's port list inside it.
const (
	maxCompareSamples = 8
	maxComparePortCap = 64
)

// toolCompare implements compare_reported_context (Designv9 §6.2): a
// bounded comparison of one subject's retained samples — per-sample
// open ports, role breakdown, per-port denominators, and the time
// window. No fleet prevalence, no peer classification: every number
// describes this subject's retained evidence only.
func (e *agentEnv) toolCompare(ctx context.Context, sub *subjectRef) (map[string]any, []string, int, bool, bool) {
	o := &toolOut{}
	if sub == nil {
		o.put("error", "unknown subject")
		return o.m, nil, o.omitted, o.truncated, false
	}
	samples, err := e.st.LatestSubjectObservations(ctx, sub.profileID, maxCompareSamples)
	if err != nil {
		return nil, nil, 0, false, true
	}
	if len(samples) == 0 {
		o.put("samples", 0)
		return o.m, nil, o.omitted, o.truncated, false
	}
	ids := make([]int64, 0, len(samples))
	var events []string
	for _, s := range samples {
		ids = append(ids, s.ObservationID)
		events = append(events, s.EventID)
	}
	portLists, cut, err := e.st.ObservationPorts(ctx, ids, maxComparePortCap)
	if err != nil {
		return nil, nil, 0, false, true
	}
	rows := make([]any, 0, len(samples))
	roles := map[string]int{}
	denom := map[string]int{}
	withPorts := 0
	for _, s := range samples {
		so := &toolOut{}
		so.put("event_id", s.EventID)
		role := normEnum(s.ViewRole, "host", "previous", "current")
		if role == "" {
			o.omitted++
		} else {
			roles[role]++
			so.put("view_role", role)
		}
		renderTime(so, "reported_at", s.ReportedAt)
		open := []string{}
		for _, p := range portLists[s.ObservationID] {
			proto := normEnum(p.Proto, "tcp", "udp")
			if proto == "" || normEnum(p.State, "open", "closed", "filtered") != "open" {
				continue
			}
			open = append(open, proto+"/"+strconv.Itoa(p.Port))
		}
		if len(open) > 0 {
			withPorts++
		}
		for _, k := range open {
			denom[k]++
		}
		so.put("open_ports", open)
		if cut[s.ObservationID] {
			so.truncated = true
			o.truncated = true
		}
		rows = append(rows, so.m)
		o.omitted += so.omitted
	}
	o.put("samples", rows)
	o.put("sample_count", len(samples))
	o.put("samples_with_open_ports", withPorts)
	rolesOut := map[string]int{}
	for k, v := range roles {
		rolesOut[k] = v
	}
	o.put("roles", rolesOut)
	o.put("open_port_denominators", denom)
	renderTime(o, "window_first", samples[len(samples)-1].ReportedAt)
	renderTime(o, "window_last", samples[0].ReportedAt)
	return o.m, events, o.omitted, o.truncated, false
}
