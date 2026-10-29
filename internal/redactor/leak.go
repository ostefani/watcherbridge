package redactor

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// ScanOutboundLeaks walks decoded bundle JSON and returns the paths of
// string leaves that still contain a raw address, an unvalidated
// address-like pattern, or an issued value. An empty result means the
// bundle is provably clean for those classes. Paths look like
// `events[3].host.ip`, `anchor`, or `target`.
//
// Three independent legs feed one verdict, so no single shared walk can
// hide a class from itself:
//
//   - validated: the same walk tokenization uses — catches what the
//     builder deliberately kept literal (emit-guard glue).
//   - eager: deliberately over-eager patterns with no validation and no
//     boundary guards — catches what the validator rejects but a human
//     would read as an address (leading-zero quads, ranges, URL paths,
//     sentence-final dots, left-glued literals).
//   - values: every issued value must be absent (word-bounded,
//     case-insensitive) — the only backstop for hostnames in free text,
//     which no address pattern covers.
//
// A hit fails the bundle closed upstream; a false positive only costs a
// fallback briefing, never a leak.
//
// Known event IDs (Pro emits dashed UUIDs, sha256-derived and opaque)
// are skipped by exact match: they ride the bundle envelope and are not
// leaks. Without the skip-set every real bundle fails on its own IDs.
func ScanOutboundLeaks(bundleJSON []byte, issued TokenMap, eventIDs []string) ([]string, error) {
	var v any
	if err := json.Unmarshal(bundleJSON, &v); err != nil {
		return nil, fmt.Errorf("decode bundle for leak scan: %w", err)
	}
	skip := make(map[string]bool, len(eventIDs))
	for _, id := range eventIDs {
		if id != "" {
			skip[id] = true
		}
	}
	var hits []string
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				// Keys are Pro schema words, never device text, but the
				// scan judges them anyway: a hostile key would otherwise
				// ride the path, never the value. A known event ID is
				// opaque either way, so the skip applies to keys too —
				// and a child under a hit key inherits "<key>", never
				// the raw key text (M8).
				keyHit := !skip[k] && (leakEager(k) || leakValidated(k) || leakValue(k, issued))
				if keyHit {
					hits = append(hits, path+".<key>")
				}
				childBase := path + "." + sanitizeSegment(k)
				if keyHit {
					childBase = path + ".<key>"
				}
				walk(t[k], childBase)
			}
		case []any:
			for i, e := range t {
				walk(e, fmt.Sprintf("%s[%d]", path, i))
			}
		case string:
			if skip[t] {
				break
			}
			if leakEager(t) || leakValidated(t) || leakValue(t, issued) {
				hits = append(hits, path)
			}
		}
	}
	walk(v, "$")
	return hits, nil
}

// sanitizeSegment keeps a path segment to capped printable runes: segments
// come from payload text, and an address-shaped key must not ride the
// "path, never the value" claim into logs.
func sanitizeSegment(s string) string {
	s = StripControls(s)
	if utf8.RuneCountInString(s) > 64 {
		s = string([]rune(s)[:64])
	}
	return s
}

var (
	// Dotted quads with no validation: catches leading-zero forms the
	// validator rejects ("192.168.001.001"), ranges ("10.0.0.5-10.0.0.9",
	// the trailing side still quads), URL paths, and sentence-final dots.
	eagerQuadRe = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}`)
	// MAC runs with no validation: colon/dash pairs, Cisco dotted
	// triples, and bare 12-hex (the letter-glued leftover case).
	// A run glued to more hex or colons is a longer fragment — the
	// invalid-IPv6 shredding case the tokenizer keeps whole — not an
	// address, so leakEagerMAC judges the neighbour before calling it
	// a hit. A trailing dot is sentence punctuation, still a hit.
	eagerMACRe = regexp.MustCompile(`[0-9A-Fa-f]{2}(?:[:-][0-9A-Fa-f]{2}){5}|[0-9A-Fa-f]{4}(?:\.[0-9A-Fa-f]{4}){2}|[0-9A-Fa-f]{12}`)
	// Full 8-group IPv6 (times have at most 3 groups; zones need "::").
	eagerV6FullRe = regexp.MustCompile(`(?:[0-9A-Fa-f]{0,4}:){7}[0-9A-Fa-f:.]+`)
)

// leakEager reports an address-like pattern by shape alone.
func leakEager(s string) bool {
	if leakEagerQuad(s) || eagerV6FullRe.MatchString(s) || leakEagerMAC(s) {
		return true
	}
	return hasBareZone(s)
}

// leakEagerQuad reports a dotted quad with a nonzero octet. An all-zeros
// quad ("000.0.0.0") is the unspecified address: it carries no identity,
// and the fuzz-pinned shredding case keeps such fragments literal, so the
// scan agrees with the tokenizer instead of failing every bundle closed.
// Dotted ports and versions ("192.168.1.5.443", "1.2.3.4.5") tokenize
// with the glue left literal, so no exemption is needed here either.
// Leading-zero forms with real octets ("192.168.001.001") still hit.
func leakEagerQuad(s string) bool {
	for _, m := range eagerQuadRe.FindAllString(s, -1) {
		for _, part := range strings.Split(m, ".") {
			if strings.TrimLeft(part, "0") != "" {
				return true
			}
		}
	}
	return false
}

// leakEagerMAC reports a MAC-shaped run that is not glued to a longer
// hex/colon fragment.
func leakEagerMAC(s string) bool {
	for _, loc := range eagerMACRe.FindAllStringIndex(s, -1) {
		if loc[1] < len(s) && (isHexByte(s[loc[1]]) || s[loc[1]] == ':') {
			continue
		}
		return true
	}
	return false
}

func isHexByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// hasBareZone reports a "::" run with digit-adjacent hex: real zoned or
// compressed addresses ("fe80::1", "::0") hit, while word-glued pairs
// ("std::fmt", no digits in the run) do not.
func hasBareZone(s string) bool {
	for i := strings.Index(s, "::"); i >= 0; {
		lo, hi := i, i+2
		for lo > 0 && isHexColonDot(s[lo-1]) {
			lo--
		}
		for hi < len(s) && isHexColonDot(s[hi]) {
			hi++
		}
		run := s[lo:hi]
		if len(run) >= 3 && strings.ContainsAny(run, "0123456789") {
			return true
		}
		next := strings.Index(s[hi:], "::")
		if next < 0 {
			return false
		}
		i = hi + next
	}
	return false
}

func isHexColonDot(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == ':' || c == '.'
}

// leakValidated reports a validated address span by the shared walk.
func leakValidated(s string) bool {
	found := false
	discoverSpans(s, func(_, _ int, _ discovered) { found = true })
	return found
}

// leakValue reports an issued value surviving in the text, word-bounded
// and case-insensitive. Values shorter than 3 runes are skipped: single
// letters and digraphs ("a", "tv") collide with prose determiners, and
// typed-field tokenization already replaced their whole-word occurrences.
func leakValue(s string, issued TokenMap) bool {
	seen := map[string]bool{}
	for _, v := range issued {
		if seen[v] {
			continue
		}
		seen[v] = true
		if utf8.RuneCountInString(v) < 3 {
			continue
		}
		if wordBoundedContains(s, v) {
			return true
		}
	}
	return false
}

// wordBoundedContains folds ASCII case and requires a non-word neighbour
// (rune-aware) on both sides of the value. Dotted ports and versions
// tokenize with the glue left literal, so no ".digit" exemption is needed:
// a surviving quad is always a real leftover, never a version fragment.
func wordBoundedContains(s, v string) bool {
	sl, vl := strings.ToLower(s), strings.ToLower(v)
	for off := 0; off <= len(sl); {
		j := strings.Index(sl[off:], vl)
		if j < 0 {
			return false
		}
		i := off + j
		var prev, next rune
		if i > 0 {
			prev, _ = utf8.DecodeLastRuneInString(sl[:i])
		}
		if rest := sl[i+len(vl):]; rest != "" {
			next, _ = utf8.DecodeRuneInString(rest)
		}
		if !wordChar(prev) && !wordChar(next) {
			return true
		}
		off = i + 1
	}
	return false
}
