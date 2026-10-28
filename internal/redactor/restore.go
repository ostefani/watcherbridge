package redactor

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// tokenRe extracts whole pseudonym tokens. \b on both sides keeps IP_1
// from matching inside IP_10 (greedy digits) or XIP_1 (no boundary).
var tokenRe = regexp.MustCompile(`\b(?:IP|MAC|HOST|NET)_[0-9]+\b`)

// Restore substitutes tokens with their real values, longest token first
// so IP_10 restores before IP_1 could shadow it. Unknown tokens are left
// as-is; ValidateTokens rejects them separately.
func Restore(text string, issued TokenMap) string {
	found := map[string]bool{}
	for _, tok := range tokenRe.FindAllString(text, -1) {
		found[tok] = true
	}
	ordered := make([]string, 0, len(found))
	for tok := range found {
		ordered = append(ordered, tok)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, tok := range ordered {
		if val, ok := issued[tok]; ok {
			text = strings.ReplaceAll(text, tok, val)
		}
	}
	return text
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
