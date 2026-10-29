package provider

import (
	"bytes"
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
