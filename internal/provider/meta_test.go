package provider

import (
	"bytes"
	"errors"
	"io"
	"net/http"
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
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) {
		return okResponse(`{"event_ids":["e1"],"host":"HOST_1","what_changed":"w","context":"c","model_suggestion":"high","priority_reason":"r","recommended_check":"k"}`), nil
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
	// The bundle crosses the boundary byte-identical to the leak-scanned
	// input: the adapter embeds it raw, never re-serializes prose.
	if !bytes.Contains(stub.bodies[0], bundle) {
		t.Fatalf("bundle not embedded verbatim in %s", stub.bodies[0])
	}
	if len(raw.JSON) == 0 {
		t.Fatal("empty briefing returned")
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
		return okResponse(`{}`), nil
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
	if bytes.Contains(got, []byte("bundle")) {
		t.Fatalf("repair must not carry a bundle: %s", got)
	}
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
