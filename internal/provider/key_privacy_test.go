package provider

import (
	"net/http"
	"strings"
	"testing"
)

// Provider credentials travel header-only (Step 30 §5.9): the Bearer [REDACTED]
// is set on the request, never marshaled into the JSON body, and error
// paths quote only the status. The stub tripper captures the exact wire
// bytes without a listener, so sandboxed runners can execute this.
func TestProviderKeyHeaderOnly(t *testing.T) {
	const apiKey = "sk-test-KEY-9f2c41aa-do-not-embed"
	answer := `{"output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]}]}`
	stub := &stubTripper{respond: func(*http.Request) (*http.Response, error) { return okResponse(answer), nil }}
	a, err := NewMetaAdapter("https://model.example/v1/brief", "muse-test", apiKey, nil, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	a.client.Transport = stub

	if _, err := a.Brief(t.Context(), []byte(`{"group":"g1"}`), nil); err != nil {
		t.Fatal(err)
	}
	tr := []TurnMessage{{Role: "system", Text: "s"}, {Role: "user", Text: "b"}}
	if _, err := a.Turn(t.Context(), tr); err != nil {
		t.Fatal(err)
	}
	if len(stub.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(stub.requests))
	}
	for i, req := range stub.requests {
		if got := req.Header.Get("Authorization"); got != "Bearer "+apiKey {
			t.Fatalf("request %d auth = %q, want the Bearer [REDACTED]", i, got)
		}
		if strings.Contains(string(stub.bodies[i]), apiKey) {
			t.Fatalf("request %d body embeds the API key", i)
		}
	}
	// A status failure must quote only the status, never response text
	// (which may carry model output) and never the key.
	fail := &stubTripper{respond: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Header: make(http.Header), Body: http.NoBody}, nil
	}}
	a.client.Transport = fail
	if _, err := a.Brief(t.Context(), []byte(`{}`), nil); err == nil {
		t.Fatal("status 500 must fail")
	} else if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("error quotes key material: %q", err)
	}
}
