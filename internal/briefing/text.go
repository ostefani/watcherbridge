package briefing

import (
	"strconv"
	"strings"
)

// Shared sentence helpers for fallback and (Step 19) rules-only text.
// Model text is never truncated for storage (Designv8 §2.5); these shape
// the deterministic side so it needs no truncation either.

// plural renders "1 event" / "3 events" (and likewise for other units).
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// dedupStrings drops repeated values, keeping first-seen order, so a
// device seen three times is named once.
func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// stripAnchorPrefix drops the "kind:" prefix operators never say out
// loud ("host:nebula-1.local" renders as "nebula-1.local"). Strings
// without a prefix pass through untouched.
func stripAnchorPrefix(s string) string {
	if i := strings.Index(s, ":"); i > 0 {
		return s[i+1:]
	}
	return s
}
