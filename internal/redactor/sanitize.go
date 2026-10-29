package redactor

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// truncationMarker marks capped fields so truncation is visible, never silent.
const truncationMarker = "…[truncated]"

// StripControls removes control and layout-invisible characters (except the
// whitespace JSON strings legitimately carry: \n, \t) so hostile banners
// cannot smuggle terminal escapes, framing, or prompt-injection text into
// prompts and UIs. Beyond Cc it strips Cf (zero-width and bidi overrides
// like U+202E, which spoof hostnames in both UIs), Zl/Zp line separators,
// Co private-use, and Cs surrogates. Exported for the briefing worker,
// which applies the same boundary to model output.
func StripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) ||
			unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) ||
			unicode.Is(unicode.Co, r) || unicode.Is(unicode.Cs, r) {
			return -1
		}
		return r
	}, s)
}

// partialToken matches a token cut mid-way ("IP_1" intact is fine; the
// trailing "IP_" or "IP_1" fragment of "IP_12" is not).
var partialToken = regexp.MustCompile(`(?:IP|MAC|HOST|NET)_[0-9]*$`)

// CapField caps a string at maxChars RUNES, marker included: over-long
// input is cut to maxChars-marker runes and the marker appended, so the
// result never exceeds maxChars runes. maxChars <= 0 means 256. A cut that
// would split a token drops the fragment first, so capping never mints a
// different valid-looking token ("IP_12" must not become "IP_1"). Exported
// for the briefing worker, which caps accepted model text the same way.
func CapField(s string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 256
	}
	markerRunes := utf8.RuneCountInString(truncationMarker)
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	cut := maxChars - markerRunes
	if cut < 0 {
		cut = 0
	}
	runes := []rune(s)
	head := string(runes[:cut])
	head = partialToken.ReplaceAllString(head, "")
	return head + truncationMarker
}
