package redactor

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// tokenRe extracts whole pseudonym tokens. \b on both sides keeps IP_1
// from matching inside IP_10 (greedy digits) or XIP_1 (no boundary).
var tokenRe = regexp.MustCompile(`\b(?:IP|MAC|HOST|NET)_[0-9]+\b`)

// Restore substitutes tokens with their real values in a single pass:
// each token-shaped span is looked up once, so a restored value that
// itself contains a token shape (hostname "IP_2") is never re-substituted,
// and IP_1 never matches inside XIP_1 (the regex, not ReplaceAll, decides
// boundaries). Unknown tokens are left as-is; ValidateTokens rejects them
// separately.
func Restore(text string, issued TokenMap) string {
	return tokenRe.ReplaceAllStringFunc(text, func(tok string) string {
		if val, ok := issued[tok]; ok {
			return val
		}
		return tok
	})
}

// tokenShaped reports whether s contains a pseudonym-token-shaped
// substring. Untrusted input carrying one (a banner literally naming
// "IP_1") must never reach tokenization: if that number was issued for
// this bundle, a model echo would restore another host's address into
// this briefing. Builders reject such input fail-closed instead.
func tokenShaped(s string) bool {
	return tokenRe.MatchString(s)
}

// wordChar reports rune-aware word membership for boundary guards. Go's
// RE2 \b is ASCII-only, so Cyrillic or punctuated hostnames ("Олексій-ПК",
// "iPhone de José") need explicit neighbours instead.
func wordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// ValidateTokens rejects model output containing tokens not issued for
// this bundle (§6.3): an invented token would otherwise restore to itself
// and mislead the operator. The tokens are pseudonymous, safe to name.
func ValidateTokens(text string, issued TokenMap) error {
	var invented []string
	seen := map[string]bool{}
	for _, tok := range tokenRe.FindAllString(text, -1) {
		if _, ok := issued[tok]; !ok && !seen[tok] {
			seen[tok] = true
			invented = append(invented, tok)
		}
	}
	if len(invented) > 0 {
		sort.Strings(invented)
		return fmt.Errorf("invented tokens: %s", strings.Join(invented, ", "))
	}
	return nil
}
