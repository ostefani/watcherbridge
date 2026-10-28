package redactor

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// truncationMarker marks capped fields so truncation is visible, never silent.
const truncationMarker = "…[truncated]"

// StripControls removes control characters (except whitespace that JSON
// strings legitimately carry: \n, \t) so hostile banners cannot smuggle
// terminal escapes or framing into prompts and UIs. Exported for the
// briefing worker, which applies the same boundary to model output.
func StripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}

// CapField caps a string at maxChars RUNES, marker included: over-long
// input is cut to maxChars-marker runes and the marker appended, so the
// result never exceeds maxChars runes. maxChars <= 0 means 256. Exported
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
	return string(runes[:cut]) + truncationMarker
}
