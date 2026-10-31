package store

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
)

// Pseudonym assignment and restoration (tokens.go, not pseudonyms.go:
// the ancient runtime-data glob pseudonyms.* in .gitignore would swallow
// that name and once hid this file from a commit, breaking the build.
// Restored from 9386cc2 (where it lived in store.go); v9-schema
// compatible — no last_used_at, which arrived with the removed Step-18
// v10 line.

var tokenPrefixes = map[string]string{"ip": "IP", "mac": "MAC", "hostname": "HOST", "net": "NET"}

func normalizePseudonymValue(category, value string) string {
	value = strings.TrimSpace(value)
	switch category {
	case "ip":
		if ip := net.ParseIP(value); ip != nil {
			return ip.String()
		}
	case "mac":
		if mac, err := net.ParseMAC(strings.ToLower(value)); err == nil {
			return mac.String()
		}
		return strings.ToLower(value)
	case "hostname":
		return strings.TrimSuffix(strings.ToLower(value), ".")
	case "net":
		if _, n, err := net.ParseCIDR(value); err == nil {
			return n.String()
		}
	}
	return value
}

// pseudonymLookupBatch bounds one IN-list read. SQLite caps bound
// parameters per statement; 500 keeps margin while keeping batch
// assignment to a handful of round trips.
const pseudonymLookupBatch = 500

// lookupPseudonymBatch is a plain read in IN-list chunks; only misses
// go through the writer.
func (s *Store) lookupPseudonymBatch(ctx context.Context, category string, normalized []string) (map[string]string, error) {
	found := map[string]string{}
	uniq := make([]string, 0, len(normalized))
	seen := map[string]bool{}
	for _, n := range normalized {
		if !seen[n] {
			seen[n] = true
			uniq = append(uniq, n)
		}
	}
	for start := 0; start < len(uniq); start += pseudonymLookupBatch {
		end := min(start+pseudonymLookupBatch, len(uniq))
		chunk := uniq[start:end]
		placeholders := strings.Repeat("?,", len(chunk)-1) + "?"
		args := make([]any, 0, len(chunk)+1)
		args = append(args, category)
		for _, n := range chunk {
			args = append(args, n)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT real_value, token FROM pseudonym_map WHERE category = ? AND real_value IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var realValue, token string
			if err := rows.Scan(&realValue, &token); err != nil {
				rows.Close()
				return nil, err
			}
			found[realValue] = token
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return found, nil
}

// GetOrAssignPseudonym returns the stable token for a real value, assigning
// the next per-category counter value on first sight. The map is keyed by
// normalized value; display_value keeps the original text for restoration.
func (s *Store) GetOrAssignPseudonym(ctx context.Context, category, realValue string) (string, error) {
	tokens, err := s.GetOrAssignPseudonyms(ctx, category, realValue)
	if err != nil {
		return "", err
	}
	return tokens[realValue], nil
}

// Note: if ctx is cancelled after the writer commits, the caller sees an
// error although tokens were assigned; the next call finds them (read-first),
// so this is harmless.
func (s *Store) GetOrAssignPseudonyms(ctx context.Context, category string, realValues ...string) (map[string]string, error) {
	prefix, ok := tokenPrefixes[category]
	if !ok {
		return nil, fmt.Errorf("unknown pseudonym category %q", category)
	}
	normalized := make([]string, 0, len(realValues))
	originalOf := make(map[string]string, len(realValues))
	for _, v := range realValues {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("pseudonym value must not be empty")
		}
		n := normalizePseudonymValue(category, v)
		normalized = append(normalized, n)
		if _, seen := originalOf[n]; !seen {
			originalOf[n] = v
		}
	}
	found, err := s.lookupPseudonymBatch(ctx, category, normalized)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(realValues))
	type pending struct{ normalized, original string }
	var missing []pending // input order: token numbering is deterministic
	seenMissing := map[string]bool{}
	for _, v := range realValues {
		n := normalizePseudonymValue(category, v)
		if token, ok := found[n]; ok {
			result[v] = token
		} else if !seenMissing[n] {
			seenMissing[n] = true
			missing = append(missing, pending{n, originalOf[n]})
		}
	}
	if len(missing) == 0 {
		return result, nil
	}
	assigned := map[string]string{}
	err = s.write(ctx, func(tx *sql.Tx) error {
		for _, m := range missing {
			normalized, original := m.normalized, m.original
			var token string
			err := tx.QueryRow(`SELECT token FROM pseudonym_map WHERE category = ? AND real_value = ?`, category, normalized).Scan(&token)
			if err == nil {
				assigned[normalized] = token
				continue
			}
			if err != sql.ErrNoRows {
				return err
			}
			var next int64 = 1
			err = tx.QueryRow(`SELECT next FROM pseudonym_counters WHERE category = ?`, category).Scan(&next)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			token = fmt.Sprintf("%s_%d", prefix, next)
			if _, err := tx.Exec(`INSERT INTO pseudonym_counters (category, next) VALUES (?, ?) ON CONFLICT(category) DO UPDATE SET next = next + 1`, category, next+1); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO pseudonym_map (category, real_value, display_value, token, created_at) VALUES (?, ?, ?, ?, ?)`, category, normalized, original, token, nanos(s.now())); err != nil {
				return err
			}
			assigned[normalized] = token
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, v := range realValues {
		if _, ok := result[v]; !ok {
			result[v] = assigned[normalizePseudonymValue(category, v)]
		}
	}
	return result, nil
}

// RestorePseudonyms resolves a batch of issued tokens to their display
// values, skipping unknown tokens without error. A nil batch resolves
// to an empty map. Single-row lookups keep this simple; only tests
// exercise the store path, since provider-bound restoration works from
// the issued map directly.
func (s *Store) RestorePseudonyms(ctx context.Context, tokens []string) (map[string]string, error) {
	out := map[string]string{}
	for _, tok := range tokens {
		display, _, found, err := s.RestorePseudonym(ctx, tok)
		if err != nil {
			return nil, err
		}
		if found {
			out[tok] = display
		}
	}
	return out, nil
}

// RestorePseudonym returns the original (display) value for a token.
func (s *Store) RestorePseudonym(ctx context.Context, token string) (display string, category string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT display_value, category FROM pseudonym_map WHERE token = ?`, token).Scan(&display, &category)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return display, category, err == nil, err
}
