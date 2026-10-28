package redactor

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ScanOutboundLeaks walks decoded bundle JSON and returns the paths of
// string leaves that still contain a raw IPv4, IPv6, MAC, or CIDR pattern.
// An empty result means the bundle is provably tokenized for those classes.
// Paths look like `events[3].host.ip`, `anchor`, or `target`.
func ScanOutboundLeaks(bundleJSON []byte) ([]string, error) {
	var v any
	if err := json.Unmarshal(bundleJSON, &v); err != nil {
		return nil, fmt.Errorf("decode bundle for leak scan: %w", err)
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
				walk(t[k], path+"."+k)
			}
		case []any:
			for i, e := range t {
				walk(e, fmt.Sprintf("%s[%d]", path, i))
			}
		case string:
			if hasRawAddress(t) {
				hits = append(hits, path)
			}
		}
	}
	walk(v, "$")
	return hits, nil
}

// hasRawAddress reports whether s contains any validated raw address span.
func hasRawAddress(s string) bool {
	found := false
	discoverSpans(s, func(discovered) { found = true })
	return found
}
