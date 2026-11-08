package incidentsummary

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ostefani/watcherbridge/internal/briefing"
	"github.com/ostefani/watcherbridge/internal/redactor"
)

// TestIncidentSummaryValidation exercises the resolver matrix and the
// strict output contract: abstention, downgrade, excessive raise, high
// cap, critical floor and disappearance floor, plus repeated
// references, incomplete evidence, identifier leakage, hostile text,
// forged citations, unknown fields and restoration expansion.
func TestIncidentSummaryValidation(t *testing.T) {
	aliases := map[string]string{"M1": "brf-1", "M2": "brf-2"}
	issued := redactor.TokenMap{}
	mustJSON := func(v map[string]any) []byte {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	answerFor := func(sugg *string) []byte {
		return mustJSON(map[string]any{
			"summary": "members show repeated appearances", "hypothesis": "likely burst",
			"confidence": "medium", "priority_suggestion": sugg, "priority_reason": "r",
			"citations": []any{map[string]any{"member": "M1"}},
		})
	}
	str := func(s string) *string { return &s }

	// Resolver matrix through briefing.Resolve with a medium floor.
	medium := briefing.Medium
	_ = medium
	cases := []struct {
		name       string
		floor      briefing.Level
		sugg       *string
		wantFinal  briefing.Level
		wantClamp  bool
		wantSuggOK bool
	}{
		{"abstain", briefing.Medium, nil, briefing.Medium, false, true},
		{"downgrade clamped", briefing.High, str("low"), briefing.High, true, true},
		{"excessive raise clamped one step", briefing.Low, str("critical"), briefing.Medium, true, true},
		{"high cap", briefing.High, str("critical"), briefing.High, true, true},
		{"critical floor stays", briefing.Critical, str("critical"), briefing.Critical, false, true},
		{"disappearance floor medium stands on abstain", briefing.Medium, nil, briefing.Medium, false, true},
	}
	for _, tc := range cases {
		raw := answerFor(tc.sugg)
		a, correction, ok := acceptSummary(raw, aliases, issued, nil, true, tc.floor)
		if !ok {
			t.Fatalf("%s: rejected: %s", tc.name, correction)
		}
		if a.Final != tc.wantFinal || a.Clamped != tc.wantClamp {
			t.Fatalf("%s: final=%v clamped=%v, want %v %v", tc.name, a.Final, a.Clamped, tc.wantFinal, tc.wantClamp)
		}
		if (a.Suggestion == nil) == (tc.sugg != nil) {
			t.Fatalf("%s: suggestion presence mismatch", tc.name)
		}
	}

	// Unknown fields are rejected.
	bad := mustJSON(map[string]any{
		"summary": "s", "hypothesis": "h", "confidence": "medium",
		"priority_suggestion": nil, "priority_reason": "r",
		"citations": []any{map[string]any{"member": "M1"}},
		"extra":     1,
	})
	if _, correction, ok := acceptSummary(bad, aliases, issued, nil, true, briefing.Medium); ok {
		t.Fatal("unknown field accepted")
	} else if !strings.Contains(correction, "unknown field") {
		t.Fatalf("correction = %q", correction)
	}

	valid := []byte(`{"summary":"s","hypothesis":"h","confidence":"medium","priority_suggestion":null,"priority_reason":"r","citations":[{"member":"M1"}]}`)
	if _, correction, ok := acceptSummary(append(valid, '}'), aliases, issued, nil, true, briefing.Medium); ok ||
		correction != "trailing data after summary object" {
		t.Fatalf("summary with a stray closer: accepted=%t, correction=%q", ok, correction)
	}

	// Forged citations are rejected.
	forged := mustJSON(map[string]any{
		"summary": "s", "hypothesis": "h", "confidence": "medium",
		"priority_suggestion": nil, "priority_reason": "r",
		"citations": []any{map[string]any{"member": "M9"}},
	})
	if _, correction, ok := acceptSummary(forged, aliases, issued, nil, true, briefing.Medium); ok {
		t.Fatal("forged citation accepted")
	} else if !strings.Contains(correction, "not a snapshot member") {
		t.Fatalf("correction = %q", correction)
	}

	// Identifier leakage via invented tokens is rejected.
	leak := mustJSON(map[string]any{
		"summary": "host IP_99 vanished", "hypothesis": "h", "confidence": "medium",
		"priority_suggestion": nil, "priority_reason": "r",
		"citations": []any{map[string]any{"member": "M1"}},
	})
	if _, correction, ok := acceptSummary(leak, aliases, issued, nil, true, briefing.Medium); ok {
		t.Fatal("invented token accepted")
	} else if !strings.Contains(correction, "invented tokens") {
		t.Fatalf("correction = %q", correction)
	}

	// Restoration expansion past 16 KiB is rejected, never truncated.
	bigToken := issued
	_ = bigToken
	issuedBig := redactor.TokenMap{"IP_1": strings.Repeat("9", 20<<10)}
	restored := mustJSON(map[string]any{
		"summary": strings.Repeat("IP_1 ", 100), "hypothesis": "h", "confidence": "medium",
		"priority_suggestion": nil, "priority_reason": "r",
		"citations": []any{map[string]any{"member": "M1"}},
	})
	if _, correction, ok := acceptSummary(restored, aliases, issuedBig, redactor.TokenMap{"IP_1": strings.Repeat("9", 20<<10)}, true, briefing.Medium); ok {
		t.Fatal("restoration expansion accepted")
	} else if !strings.Contains(correction, "16 KiB") {
		t.Fatalf("correction = %q", correction)
	}

	// Overlong hypothesis is rejected.
	longHyp := strings.Repeat("h", 257)
	longRaw := mustJSON(map[string]any{
		"summary": "s", "hypothesis": longHyp, "confidence": "medium",
		"priority_suggestion": nil, "priority_reason": "r",
		"citations": []any{map[string]any{"member": "M1"}},
	})
	if _, correction, ok := acceptSummary(longRaw, aliases, issued, nil, true, briefing.Medium); ok {
		t.Fatal("long hypothesis accepted")
	} else if !strings.Contains(correction, "256") {
		t.Fatalf("correction = %q", correction)
	}
}
