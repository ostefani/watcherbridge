package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// noRedirect refuses redirects: a 3xx from the model endpoint is a failure,
// not a new destination. Following one could leak the bundle (and the key)
// to an attacker's URL on a compromised network path.
func noRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// sinkWriter is a no-op http.ResponseWriter. http.MaxBytesReader is a
// server-side API, but its limit enforcement lives in the returned
// ReadCloser; the writer only receives an optional too-large notification
// which is meaningless client-side. A sink keeps the stdlib enforcement
// without importing test helpers into production code.
type sinkWriter struct{}

func (sinkWriter) Header() http.Header { return http.Header{} }
func (sinkWriter) Write(b []byte) (int, error) {
	return len(b), nil
}
func (sinkWriter) WriteHeader(int) {}

// MetaAdapter is the Meta Model API adapter (Muse Spark, Standard tier).
// The endpoint shape (POST {model, bundle} → briefing object) is defined
// against ModelBaseURL and verified in Phase 4 against the live API; until
// then only the fake provider runs in tests.
type MetaAdapter struct {
	baseURL  string
	modelID  string
	apiKey   string
	client   *http.Client
	maxBytes int64
}

// NewMetaAdapter validates its endpoint and key. baseURL is the full
// endpoint to POST (http or https; loopback allowed for tests). apiKey is
// sent as a bearer token and never logged. maxBytes caps the response.
func NewMetaAdapter(baseURL, modelID, apiKey string, client *http.Client, maxBytes int64) (*MetaAdapter, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("model base URL must be an http(s) endpoint, got %q", baseURL)
	}
	if modelID == "" {
		return nil, fmt.Errorf("model ID is required")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("model API key is required: refusing keyless provider calls")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("max response bytes must be positive")
	}
	if client == nil {
		// Nil Transport = stdlib default: TLS verification on, no
		// redirect following beyond noRedirect below.
		client = &http.Client{CheckRedirect: noRedirect}
	}
	return &MetaAdapter{baseURL: baseURL, modelID: modelID, apiKey: apiKey, client: client, maxBytes: maxBytes}, nil
}

// Name implements Provider.
func (a *MetaAdapter) Name() string { return "meta" }

// Brief implements Provider.
func (a *MetaAdapter) Brief(ctx context.Context, bundle []byte, _ map[string]struct{}) (RawBriefing, error) {
	body, err := MarshalBundleRequest(a.modelID, bundle)
	if err != nil {
		return RawBriefing{}, fmt.Errorf("encode bundle request: %w", err)
	}
	return a.post(ctx, body)
}

// Turn implements Turner: one stateless transcript send. The full
// transcript is the request input, so the provider keeps no session and
// a resumed run replays from the local log. Response cap, redirect
// policy, and status mapping match Brief exactly.
func (a *MetaAdapter) Turn(ctx context.Context, transcript []TurnMessage) (string, error) {
	body, err := MarshalTurnRequest(a.modelID, transcript)
	if err != nil {
		return "", fmt.Errorf("encode turn request: %w", err)
	}
	raw, err := a.post(ctx, body)
	if err != nil {
		return "", err
	}
	return string(raw.JSON), nil
}

// Repair implements Provider.
func (a *MetaAdapter) Repair(ctx context.Context, correction string, _ map[string]struct{}) (RawBriefing, error) {
	body, err := MarshalRepairRequest(a.modelID, correction)
	if err != nil {
		return RawBriefing{}, fmt.Errorf("encode repair request: %w", err)
	}
	return a.post(ctx, body)
}

func (a *MetaAdapter) post(ctx context.Context, body []byte) (RawBriefing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL, bytes.NewReader(body))
	if err != nil {
		return RawBriefing{}, fmt.Errorf("build provider request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	resp, err := a.client.Do(req)
	if err != nil {
		return RawBriefing{}, fmt.Errorf("provider call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Status only: the body may carry model text and is never
		// quoted into errors (it can reach logs).
		return RawBriefing{}, fmt.Errorf("provider status %d", resp.StatusCode)
	}
	// Cap before reading: MaxBytesReader errors past the limit instead of
	// buffering an unbounded body. The response stays tokenized, so it is
	// safe to hold, but never safe to hold unbounded.
	raw, err := io.ReadAll(http.MaxBytesReader(sinkWriter{}, resp.Body, a.maxBytes))
	if err != nil {
		return RawBriefing{}, fmt.Errorf("read provider response: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return RawBriefing{}, fmt.Errorf("provider returned an empty body")
	}
	text, err := extractResponseText(raw)
	if err != nil {
		return RawBriefing{}, err
	}
	return RawBriefing{JSON: text}, nil
}

// responsesOutputContent is one text part of a Responses API output item.
type responsesOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesOutputItem struct {
	Type    string                   `json:"type"`
	Content []responsesOutputContent `json:"content"`
}

// responsesEnvelope is the Responses API result body. Only the message
// text is briefing content; everything else is transport.
type responsesEnvelope struct {
	Output []responsesOutputItem `json:"output"`
}

// extractResponseText returns the model's message text: the first
// non-empty content text in the output array. An envelope with no text
// (an error shape, an empty finish) is a provider failure, never a
// briefing — the worker falls back and the log names this cause.
func extractResponseText(raw []byte) ([]byte, error) {
	var env responsesEnvelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("decode provider envelope: %v", strings.TrimSpace(err.Error()))
	}
	for _, item := range env.Output {
		for _, c := range item.Content {
			if c.Text != "" {
				return []byte(c.Text), nil
			}
		}
	}
	return nil, fmt.Errorf("provider returned no text output")
}
