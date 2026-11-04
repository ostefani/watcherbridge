package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// stubTripper answers without the network: httptest servers need a port
// bind, which sandboxes forbid. It records requests for header/body
// assertions and counts them to prove no redirect was followed.
type stubTripper struct {
	respond  func(r *http.Request) (*http.Response, error)
	requests []*http.Request
	bodies   [][]byte
}

func (s *stubTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	s.requests = append(s.requests, r)
	s.bodies = append(s.bodies, body)
	return s.respond(r)
}

func okResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testAdapter(t *testing.T, stub *stubTripper, maxBytes int64) *MetaAdapter {
	t.Helper()
	a, err := NewMetaAdapter("https://model.example/v1/brief", "muse-test", "0123456789abcdef", &http.Client{
		Transport:     stub,
		CheckRedirect: noRedirect,
	}, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestMetaBriefSendsBundleVerbatim(t *testing.T) {
	const briefing = `{"event_ids":["e1"],"host":"HOST_1","what_changed":"w","context":"c","model_suggestion":"high","priority_reason":"r","recommended_check":"k"}`
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) {
		return okResponse(`{"output":[{"type":"message","content":[{"type":"output_text","text":` + strconv.Quote(briefing) + `}]}]}`), nil
	}}
	a := testAdapter(t, stub, 64<<10)
	bundle := []byte(`{"group":"g1","events":[{"s":"IP_1"}]}`)
	raw, err := a.Brief(t.Context(), bundle, map[string]struct{}{"IP_1": {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.requests) != 1 {
		t.Fatalf("expected exactly one request, got %d", len(stub.requests))
	}
	req := stub.requests[0]
	if req.Header.Get("Authorization") != "Bearer 0123456789abcdef" {
		t.Fatalf("bearer key not sent: %q", req.Header.Get("Authorization"))
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
	// Responses API shape: one user message, input_text, no streaming.
	for _, want := range []string{`"role":"user"`, `"type":"input_text"`, `"stream":false`} {
		if !bytes.Contains(stub.bodies[0], []byte(want)) {
			t.Fatalf("Responses shape missing %s in %s", want, stub.bodies[0])
		}
	}
	// The bundle crosses the boundary byte-identical to the leak-scanned
	// input: inside the JSON string it is escaped, so decode the envelope
	// and compare the text suffix exactly — string conversion, never
	// re-serialization.
	text := requestText(t, stub.bodies[0])
	if !strings.HasSuffix(text, string(bundle)) {
		t.Fatalf("bundle not embedded verbatim, text ends: %.60q", text)
	}
	// The briefing is the envelope's message text, not the envelope.
	if string(raw.JSON) != briefing {
		t.Fatalf("briefing not extracted: %s", raw.JSON)
	}
}

func TestMetaNoTextOutputFails(t *testing.T) {
	for name, body := range map[string]string{
		"empty output": `{"output":[]}`,
		"no text":      `{"output":[{"type":"message","content":[{"type":"output_text","text":""}]}]}`,
		"error shape":  `{"error":{"message":"bad key"}}`,
	} {
		stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) { return okResponse(body), nil }}
		a := testAdapter(t, stub, 64<<10)
		if _, err := a.Brief(t.Context(), []byte(`{}`), nil); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestMetaTurnRequestsJSONOutput(t *testing.T) {
	stub := &stubTripper{}
	stub.respond = func(*http.Request) (*http.Response, error) {
		var request struct {
			Text struct {
				Format struct {
					Type string `json:"type"`
				} `json:"format"`
			} `json:"text"`
		}
		if err := json.Unmarshal(stub.bodies[len(stub.bodies)-1], &request); err != nil {
			t.Fatal(err)
		}
		if request.Text.Format.Type != "json_object" {
			t.Fatal("agent turns must request JSON generation, including repairs")
		}
		return okResponse(`{"output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]}]}`), nil
	}
	a := testAdapter(t, stub, 64<<10)
	tr := []TurnMessage{{Role: "system", Text: "Reply with one JSON object."}, {Role: "user", Text: "bundle"}}
	for range 2 {
		res, err := a.Turn(t.Context(), tr)
		if err != nil {
			t.Fatal(err)
		}
		tr = append(tr, TurnMessage{Role: "assistant", Text: res.Text}, TurnMessage{Role: "user", Text: "Correct the JSON fields."})
	}
}

func TestMetaExtractsCompleteAnswerText(t *testing.T) {
	const body = `{"status":"completed","output":[
		{"type":"reasoning","content":[{"type":"output_text","text":"ignore reasoning"}]},
		{"type":"message","phase":"commentary","content":[{"type":"output_text","text":"Checking evidence."}]},
		{"type":"message","content":[{"type":"other","text":"ignore other content"},{"type":"output_text","text":"{\"event_ids\":"},{"type":"output_text","text":"[\"e1\"]}"}]}
	],"usage":{"input_tokens":10,"output_tokens":5}}`
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) { return okResponse(body), nil }}
	res, err := testAdapter(t, stub, 64<<10).Turn(t.Context(), []TurnMessage{{Role: "user", Text: "JSON"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != `{"event_ids":["e1"]}` || !res.Usage.HasUsage || res.Usage.OutputTokens != 5 {
		t.Fatalf("must extract all final output_text parts with usage: %+v", res)
	}
}

func TestMetaRejectsUnfinishedOrNonAnswerOutput(t *testing.T) {
	for _, body := range []string{
		`{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"{\"partial\":"}]}]}`,
		`{"status":"failed","output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]}]}`,
		`{"output":[{"type":"message","phase":"commentary","content":[{"type":"output_text","text":"Checking."}]}]}`,
		`{"output":[{"type":"reasoning","content":[{"type":"output_text","text":"not an answer"}]}]}`,
	} {
		stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) { return okResponse(body), nil }}
		if _, err := testAdapter(t, stub, 64<<10).Turn(t.Context(), nil); err == nil {
			t.Fatal("unfinished or non-answer output must fail at the adapter boundary")
		}
	}
}

func TestMetaRefusesRedirect(t *testing.T) {
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://evil.example/x"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}}
	a := testAdapter(t, stub, 64<<10)
	if _, err := a.Brief(t.Context(), []byte(`{}`), nil); err == nil {
		t.Fatal("3xx must fail, not follow")
	}
	if len(stub.requests) != 1 {
		t.Fatalf("redirect was followed: %d requests", len(stub.requests))
	}
}

func TestMetaStatusAndEmptyBodyFail(t *testing.T) {
	for name, resp := range map[string]*http.Response{
		"500":   {StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("boom"))},
		"empty": okResponse("  \n "),
	} {
		stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) { return resp, nil }}
		a := testAdapter(t, stub, 64<<10)
		if _, err := a.Brief(t.Context(), []byte(`{}`), nil); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestMetaResponseCapEnforced(t *testing.T) {
	big := strings.Repeat("x", 100)
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) { return okResponse(big), nil }}
	a := testAdapter(t, stub, 10)
	if _, err := a.Brief(t.Context(), []byte(`{}`), nil); err == nil {
		t.Fatal("oversize body must fail")
	}
}

func TestMetaRepairSendsCorrectionOnly(t *testing.T) {
	var got []byte
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) {
		return okResponse(`{"output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]}]}`), nil
	}}
	a := testAdapter(t, stub, 64<<10)
	_, err := a.Repair(t.Context(), "rejected (missing field)", map[string]struct{}{"IP_1": {}})
	if err != nil {
		t.Fatal(err)
	}
	got = stub.bodies[0]
	if !bytes.Contains(got, []byte("missing field")) {
		t.Fatalf("correction missing: %s", got)
	}
	if bytes.Contains(got, []byte("10.0.0.1")) {
		t.Fatalf("repair leaked bundle values: %s", got)
	}
	// The repair carries the correction under the repair instruction,
	// never the bundle instruction or bundle content.
	text := requestText(t, got)
	if strings.Contains(text, "Redacted bundle follows") {
		t.Fatalf("repair must not carry a bundle: %.80q", text)
	}
}

// requestText decodes a Responses API request body and returns its input
// text, so tests assert on the model-visible string instead of the
// JSON-escaped wire bytes.
func requestText(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		Input []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request not Responses-shaped: %v", err)
	}
	if len(req.Input) != 1 || len(req.Input[0].Content) != 1 {
		t.Fatalf("one message, one part expected: %s", body)
	}
	return req.Input[0].Content[0].Text
}

func TestNewMetaAdapterValidates(t *testing.T) {
	if _, err := NewMetaAdapter("ftp://x/", "m", "0123456789abcdef", nil, 100); err == nil {
		t.Fatal("non-http URL accepted")
	}
	if _, err := NewMetaAdapter("", "m", "0123456789abcdef", nil, 100); err == nil {
		t.Fatal("empty URL accepted")
	}
	if _, err := NewMetaAdapter("https://m.example/", "", "0123456789abcdef", nil, 100); err == nil {
		t.Fatal("empty model accepted")
	}
	if _, err := NewMetaAdapter("https://m.example/", "m", "", nil, 100); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := NewMetaAdapter("https://m.example/", "m", "0123456789abcdef", nil, 0); err == nil {
		t.Fatal("zero cap accepted")
	}
	// Nil client means the stdlib default transport: TLS verification on,
	// nothing custom to misconfigure.
	a, err := NewMetaAdapter("https://m.example/", "m", "0123456789abcdef", nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if a.client.Transport != nil {
		t.Fatal("default client must use the stdlib default transport")
	}
}

func TestFakeCountsSharedDeadline(t *testing.T) {
	f := NewFake(FakeStep{Err: errors.New("down")})
	_, _ = f.Brief(t.Context(), []byte(`{}`), nil)
	_, _ = f.Repair(t.Context(), "fix", nil)
	if f.Briefs != 1 || f.Repairs != 1 {
		t.Fatalf("counts %d/%d", f.Briefs, f.Repairs)
	}
	if len(f.Received) != 2 || string(f.Received[1]) != "fix" {
		t.Fatalf("repair payload not recorded: %q", f.Received)
	}
}

func TestMarshalTurnRequestResendsTranscript(t *testing.T) {
	tr := []TurnMessage{
		{Role: "system", Text: "sys"},
		{Role: "user", Text: `{"bundle":1}`},
		{Role: "assistant", Text: `{"tool":"get_reported_profile","arguments":{"host":"IP_1"}}`},
		{Role: "tool", Text: `{"result":{"report_count":2}}`},
	}
	raw, err := MarshalTurnRequest("m", tr)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Input  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "m" || body.Stream {
		t.Fatalf("model and stream flags wrong: %+v", body)
	}
	if len(body.Input) != len(tr) {
		t.Fatalf("every transcript turn must cross: %d of %d", len(body.Input), len(tr))
	}
	// Roles map onto Responses roles; tool results travel as user
	// text with an explicit marker — never a new speaker, never a
	// session ID.
	wantRoles := []string{"system", "user", "assistant", "user"}
	for i, want := range wantRoles {
		if body.Input[i].Role != want {
			t.Fatalf("turn %d role: got %q want %q", i, body.Input[i].Role, want)
		}
	}
	if !strings.Contains(body.Input[3].Content[0].Text, "Tool result: ") {
		t.Fatalf("tool result must be marked: %q", body.Input[3].Content[0].Text)
	}
	if strings.Contains(string(raw), "previous_response_id") || strings.Contains(string(raw), "session") {
		t.Fatal("turn request must chain nothing provider-side")
	}
}

func TestMetaTurnSendsFullTranscript(t *testing.T) {
	const answer = `{"event_ids":["e1"],"host":"HOST_1"}`
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) {
		return okResponse(`{"output":[{"type":"message","content":[{"type":"output_text","text":` + strconv.Quote(answer) + `}]}]}`), nil
	}}
	a := testAdapter(t, stub, 64<<10)
	tr := []TurnMessage{{Role: "system", Text: "s"}, {Role: "user", Text: "b"}}
	res, err := a.Turn(t.Context(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != answer {
		t.Fatalf("turn must return the assistant text: %q", res.Text)
	}
	if res.Usage.HasUsage {
		t.Fatalf("envelope without usage must read unknown: %+v", res.Usage)
	}
	if len(stub.requests) != 1 {
		t.Fatalf("one turn is one POST: %d", len(stub.requests))
	}
	// The full transcript is the input: stateless resend, same caps and
	// redirect policy as Brief.
	for _, want := range []string{`"role":"system"`, `"role":"user"`, `"stream":false`} {
		if !bytes.Contains(stub.bodies[0], []byte(want)) {
			t.Fatalf("turn body missing %s in %s", want, stub.bodies[0])
		}
	}
}

// Enforce the endpoint's role/content contract on every POST, including
// continuations. An initial turn has no assistant history and would hide
// input_text incorrectly used for a replayed assistant answer.
func TestMetaTurnReplayContentTypes(t *testing.T) {
	for _, continuation := range []TurnMessage{
		{Role: "user", Text: "That turn was rejected. Reply with corrected JSON."},
		{Role: "tool", Text: `{"result":{"report_count":2}}`},
	} {
		t.Run(continuation.Role, func(t *testing.T) {
			const answer = `{"tool":"get_reported_profile","arguments":{"host":"HOST_1"}}`
			stub := &stubTripper{}
			stub.respond = func(*http.Request) (*http.Response, error) {
				var body responsesRequest
				if err := json.Unmarshal(stub.bodies[len(stub.bodies)-1], &body); err != nil {
					t.Fatal(err)
				}
				for _, msg := range body.Input {
					want := "input_text"
					if msg.Role == "assistant" {
						want = "output_text"
					}
					for _, part := range msg.Content {
						if part.Type != want {
							return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header),
								Body: io.NopCloser(strings.NewReader(`{"error":"invalid content type for role"}`))}, nil
						}
					}
				}
				return okResponse(`{"output":[{"type":"message","content":[{"type":"output_text","text":` + strconv.Quote(answer) + `}]}]}`), nil
			}
			a := testAdapter(t, stub, 64<<10)
			tr := []TurnMessage{{Role: "system", Text: "instructions"}, {Role: "user", Text: `{"host":"HOST_1"}`}}
			first, err := a.Turn(t.Context(), tr)
			if err != nil {
				t.Fatalf("initial turn: %v", err)
			}
			tr = append(tr, TurnMessage{Role: "assistant", Text: first.Text}, continuation)
			if _, err := a.Turn(t.Context(), tr); err != nil {
				t.Fatalf("continuation must accept replayed assistant output: %v", err)
			}
			var replay responsesRequest
			if err := json.Unmarshal(stub.bodies[1], &replay); err != nil {
				t.Fatal(err)
			}
			if len(stub.requests) != 2 || len(replay.Input) != len(tr) {
				t.Fatal("continuation must resend the full transcript in one POST")
			}
			for i, msg := range tr {
				want := msg.Text
				if msg.Role == "tool" {
					want = "Tool result: " + want
				}
				if len(replay.Input[i].Content) != 1 || replay.Input[i].Content[0].Text != want {
					t.Fatalf("message %d text changed during replay", i)
				}
			}
		})
	}
}

func TestFakeTurnScripts(t *testing.T) {
	f := NewFake()
	f.Turns = []FakeStep{{JSON: []byte("one")}, {Err: errors.New("down")}}
	if res, err := f.Turn(context.Background(), []TurnMessage{{Role: "user", Text: "a"}}); err != nil || res.Text != "one" {
		t.Fatalf("scripted turn must play: %q %v", res.Text, err)
	}
	if _, err := f.Turn(context.Background(), []TurnMessage{{Role: "user", Text: "b"}}); err == nil {
		t.Fatal("scripted turn error must surface")
	}
	// Extra calls replay the last step; deadlines ride every turn for
	// the per-turn budget assertions.
	if _, err := f.Turn(context.Background(), []TurnMessage{{Role: "user", Text: "c"}}); err == nil {
		t.Fatal("replay must repeat the last step")
	}
	if f.TurnCalls != 3 || len(f.TurnReceived) != 3 {
		t.Fatalf("turns must record: calls=%d received=%d", f.TurnCalls, len(f.TurnReceived))
	}
	if len(f.Deadlines) != 3 {
		t.Fatalf("deadlines must ride every turn: %d", len(f.Deadlines))
	}
}

func TestTurnUsageRidesWhenSupplied(t *testing.T) {
	const answer = `{"event_ids":["e1"],"host":"HOST_1"}`
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) {
		return okResponse(`{"output":[{"type":"message","content":[{"type":"output_text","text":` +
			strconv.Quote(answer) + `}]}],"usage":{"input_tokens":120,"output_tokens":45}}`), nil
	}}
	a := testAdapter(t, stub, 64<<10)
	res, err := a.Turn(t.Context(), []TurnMessage{{Role: "user", Text: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Usage.HasUsage || res.Usage.InputTokens != 120 || res.Usage.OutputTokens != 45 {
		t.Fatalf("envelope usage must ride the turn: %+v", res.Usage)
	}
	if res.Usage.HasCost {
		t.Fatalf("the API reports no cost, so cost stays unknown: %+v", res.Usage)
	}
}

func TestFakeStepUsageScripts(t *testing.T) {
	f := NewFake(FakeStep{JSON: []byte("one"), Usage: Usage{InputTokens: 7, OutputTokens: 3, HasUsage: true}})
	f.Turns = []FakeStep{{JSON: []byte("one"), Usage: Usage{InputTokens: 7, OutputTokens: 3, HasUsage: true}}}
	res, err := f.Turn(context.Background(), []TurnMessage{{Role: "user", Text: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Usage.HasUsage || res.Usage.InputTokens != 7 || res.Usage.OutputTokens != 3 {
		t.Fatalf("scripted usage must ride the turn: %+v", res.Usage)
	}
	raw, err := f.Brief(context.Background(), []byte("{}"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !raw.Usage.HasUsage || raw.Usage.InputTokens != 7 {
		t.Fatalf("scripted usage must ride briefs too: %+v", raw.Usage)
	}
}

func TestUsageAdd(t *testing.T) {
	sum := Usage{InputTokens: 1, OutputTokens: 2, HasUsage: true}.Add(
		Usage{InputTokens: 10, OutputTokens: 20, HasUsage: true, CostMicros: 5, HasCost: true})
	if sum.InputTokens != 11 || sum.OutputTokens != 22 || !sum.HasUsage {
		t.Fatalf("tokens must add: %+v", sum)
	}
	if !sum.HasCost || sum.CostMicros != 5 {
		t.Fatalf("supplied cost must survive beside unsupplied: %+v", sum)
	}
	if (Usage{}.Add(Usage{})).HasUsage || (Usage{}.Add(Usage{})).HasCost {
		t.Fatal("empty add must stay unknown")
	}
}
