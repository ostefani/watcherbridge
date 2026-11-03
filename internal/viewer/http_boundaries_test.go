package viewer

import (
	"context"
	"net/http"
	"testing"

	"github.com/ostefani/watcherbridge/internal/store"
)

// Browser/HTTP boundary spot checks (Step 30 §3). No listener is used,
// so sandboxed runners can execute these; socket-level transport claims
// (timeouts, slow clients, shutdown) stay environment-restricted.

// GET routes must be usable without Origin/Fetch-Metadata headers and
// must not change stored state: every write path lives behind POST plus
// guardStateChanging. The snapshot before/after proves no side effects.
func TestGetRoutesHaveNoSideEffectsWithoutOrigin(t *testing.T) {
	s := openViewerStore(t)
	seedBriefing(t, s, "host:h", "evt-h")
	_, mux := withHost(t, s, "127.0.0.1:8081")

	beforeEvents := storedEventTotal(t, s)
	beforeIncidents := incidentTotal(t, s)

	targets := []string{
		"/", "/api/overview", "/api/health", "/api/briefings",
		"/api/groups", "/api/devices",
		"/api/incidents", "/api/rules/proposals",
	}
	for _, target := range targets {
		rec := serveMux(mux, "GET", target, "127.0.0.1:8081", nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s without Origin = %d, want 200 (%s)", target, rec.Code, rec.Body.String())
		}
		assertViewerHeaders(t, rec.Header())
	}
	// Bare /api/decisions needs its subject params (400), but that
	// refusal is Origin-independent and equally side-effect free.
	if rec := serveMux(mux, "GET", "/api/decisions", "127.0.0.1:8081", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("GET /api/decisions without params = %d, want 400", rec.Code)
	}
	if got := storedEventTotal(t, s); got != beforeEvents {
		t.Fatalf("stored events = %d, want %d: GET changed stored state", got, beforeEvents)
	}
	if got := incidentTotal(t, s); got != beforeIncidents {
		t.Fatalf("incidents = %d, want %d: GET changed stored state", got, beforeIncidents)
	}
}

// Incident cursor boundary grammar: malformed cursors and limits are
// 400 with headers; well-formed but unknown cursors page to an empty
// 200, never a lookup error.
func TestIncidentCursorBoundaries(t *testing.T) {
	s := openViewerStore(t)
	seedBriefing(t, s, "host:h", "evt-h")
	_, mux := withHost(t, s, "127.0.0.1:8081")

	for _, target := range []string{
		"/api/incidents?before=1:2:3", // extra segment
		"/api/incidents?before=-5:1",  // negative nanos
		"/api/incidents?before=5:0",   // zero row id
		"/api/incidents?before=5:-1",  // negative row id
		"/api/incidents?before=abc:1", // non-numeric nanos
		"/api/incidents?before=1:abc", // non-numeric row id
		"/api/incidents?before=:1",    // empty nanos
		"/api/incidents?limit=201",    // past the 200 cap
		"/api/incidents?limit=-3",     // negative limit
		"/api/incidents?limit=50x",    // non-numeric limit
		"/api/incidents?limit=0",      // zero limit
	} {
		rec := serveMux(mux, "GET", target, "127.0.0.1:8081", nil, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s = %d, want 400 (%s)", target, rec.Code, rec.Body.String())
		}
		assertViewerHeaders(t, rec.Header())
	}
	// Far-future but well-formed cursor: empty page, not an error.
	rec := serveMux(mux, "GET", "/api/incidents?before=4102444800000000000:1", "127.0.0.1:8081", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown well-formed cursor = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// Manual-checklist §2 API cases: bare detail IDs are 400 (never a
// store lookup on missing input), tuple-shaped cursors on numeric
// routes are 400, and well-formed unknown IDs stay 404.
func TestChecklistInvalidQueries(t *testing.T) {
	s := openViewerStore(t)
	seedBriefing(t, s, "host:h", "evt-h")
	_, mux := withHost(t, s, "127.0.0.1:8081")

	for target, want := range map[string]int{
		"/api/briefing":                         http.StatusBadRequest,
		"/api/briefing?id=does-not-exist":       http.StatusNotFound,
		"/api/briefings?before=5:not-a-profile": http.StatusBadRequest,
		"/api/devices/nope":                     http.StatusBadRequest,
		"/api/devices/prof-XYZ":                 http.StatusBadRequest,
		"/api/devices/prof-0123456789abcdef":    http.StatusNotFound,
	} {
		rec := serveMux(mux, "GET", target, "127.0.0.1:8081", nil, "")
		if rec.Code != want {
			t.Fatalf("GET %s = %d, want %d (%s)", target, rec.Code, want, rec.Body.String())
		}
		assertViewerHeaders(t, rec.Header())
	}
}

// Unknown paths and method mismatches are errors with headers, never
// redirects: a 301 to a "canonical" path would turn a typo into a
// cross-route fetch and leak the viewer onto unexpected methods.
func TestUnknownPathsAndMethodMismatch(t *testing.T) {
	s := openViewerStore(t)
	_, mux := withHost(t, s, "127.0.0.1:8081")

	for _, target := range []string{
		"/api/nope", "/api/incidents/", "/api/devices/",
		"/api/briefings/extra/segment",
	} {
		rec := serveMux(mux, "GET", target, "127.0.0.1:8081", nil, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", target, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Fatalf("GET %s sent a redirect to %q", target, loc)
		}
		assertViewerHeaders(t, rec.Header())
	}
	// Non-API unknown paths keep the SPA catch-all: the index shell
	// with 200, never a redirect. Only /api/* is a 404 boundary.
	if rec := serveMux(mux, "GET", "/typo-page", "127.0.0.1:8081", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /typo-page = %d, want 200 (SPA catch-all)", rec.Code)
	}
	// GET on a POST-only route falls into the unknown-API 404 (the
	// GET /api/ pattern matches the path): still headed, never a
	// guard bypass or a redirect. Method misuse on known GET routes
	// stays 405 (see TestMuxResponsesGatedAndHeaded).
	if rec := serveMux(mux, "GET", "/api/rules/approve", "127.0.0.1:8081", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/rules/approve = %d, want 404", rec.Code)
	}
}

func storedEventTotal(t *testing.T, s *store.Store) int {
	t.Helper()
	snaps, err := s.OpenGroupSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, g := range snaps {
		total += g.EventCount
	}
	// Ungrouped events never appear in snapshots; none are expected
	// here, so a snapshot-only total is an exact state fingerprint.
	return total
}

func incidentTotal(t *testing.T, s *store.Store) int {
	t.Helper()
	rows, err := s.ListIncidents(context.Background(), 200, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}
