package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Pseudonym assignment and restoration (tokens.go, not pseudonyms.go:
// the ancient runtime-data glob pseudonyms.* in .gitignore would swallow
// that name and once hid this file from a commit, breaking the build.
// Restored from 9386cc2 (where it lived in store.go). v13 adds
// last_used_at so expiry can tell a still-referenced token from a
// dead one; assignment and every later lookup bump it.

var tokenPrefixes = map[string]string{"ip": "IP", "mac": "MAC", "hostname": "HOST", "net": "NET"}

// errPseudonymVanished is a value-free retry signal. The normalized
// private value must not travel in the error because assignment
// failures are logged downstream after sanitization that does not
// cover every hostname shape.
var errPseudonymVanished = errors.New("pseudonym vanished during assignment; retry")

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
	return s.assignPseudonyms(ctx, category, prefix, realValues, normalized, originalOf, found)
}

// assignPseudonyms completes assignment from an explicit pre-read hit
// map. Tests supply a stale map to exercise the exact read-hit,
// purge-delete, then write window: a stale hit is not reissued, and
// the in-transaction recheck returns a value-free retry signal.
func (s *Store) assignPseudonyms(ctx context.Context, category, prefix string, realValues, normalized []string, originalOf, found map[string]string) (map[string]string, error) {
	type pending struct{ normalized, original string }
	var missing []pending // input order: token numbering is deterministic
	seenMissing := map[string]bool{}
	for _, v := range realValues {
		n := normalizePseudonymValue(category, v)
		if _, ok := found[n]; !ok && !seenMissing[n] {
			seenMissing[n] = true
			missing = append(missing, pending{n, originalOf[n]})
		}
	}
	// final holds tokens re-read inside the write transaction. The
	// pre-read above is a fast-path hint only: retention can delete a
	// hit between that read and the commit, so the returned mapping
	// always comes from this in-transaction validation, never from the
	// pre-read (Designv9 §4.6: touch-then-recheck).
	final := map[string]string{}
	err := s.write(ctx, func(tx *sql.Tx) error {
		// One timestamp for the whole transaction: the touch and the
		// assignment agree exactly, regardless of queue delay.
		now := nanos(s.now())
		// Usage tracking runs in the same transaction as assignment, so
		// a token's last use advances exactly when it is (re)issued.
		// The touch covers every requested value, not just misses: an
		// all-hit call must still prove the tokens are referenced, or
		// steady-state devices would expire while actively reported.
		touch := make([]string, 0, len(normalized))
		seen := map[string]bool{}
		for _, n := range normalized {
			if !seen[n] {
				seen[n] = true
				touch = append(touch, n)
			}
		}
		for start := 0; start < len(touch); start += pseudonymLookupBatch {
			end := start + pseudonymLookupBatch
			if end > len(touch) {
				end = len(touch)
			}
			placeholders := strings.Repeat("?,", len(touch[start:end]))
			args := make([]any, 0, len(touch[start:end])+2)
			args = append(args, now, category)
			for _, n := range touch[start:end] {
				args = append(args, n)
			}
			if _, err := tx.Exec(`UPDATE pseudonym_map SET last_used_at = ? WHERE category = ? AND real_value IN (`+
				placeholders[:len(placeholders)-1]+`)`, args...); err != nil {
				return err
			}
		}
		for _, m := range missing {
			normalized, original := m.normalized, m.original
			var token string
			err := tx.QueryRow(`SELECT token FROM pseudonym_map WHERE category = ? AND real_value = ?`, category, normalized).Scan(&token)
			if err == nil {
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
			if _, err := tx.Exec(`INSERT INTO pseudonym_map (category, real_value, display_value, token, created_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?)`, category, normalized, original, token, now, now); err != nil {
				return err
			}
		}
		// Revalidate every requested mapping inside the transaction:
		// a purge that raced the pre-read touch leaves the update
		// rowless, and the re-read below then finds no row instead of
		// returning a deleted token.
		uniq := touch
		for start := 0; start < len(uniq); start += pseudonymLookupBatch {
			end := start + pseudonymLookupBatch
			if end > len(uniq) {
				end = len(uniq)
			}
			placeholders := strings.Repeat("?,", len(uniq[start:end]))
			args := make([]any, 0, len(uniq[start:end])+1)
			args = append(args, category)
			for _, n := range uniq[start:end] {
				args = append(args, n)
			}
			rows, err := tx.Query(`SELECT real_value, token FROM pseudonym_map WHERE category = ? AND real_value IN (`+
				placeholders[:len(placeholders)-1]+`)`, args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var realValue, token string
				if err := rows.Scan(&realValue, &token); err != nil {
					rows.Close()
					return err
				}
				final[realValue] = token
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(realValues))
	for _, v := range realValues {
		n := normalizePseudonymValue(category, v)
		token, ok := final[n]
		if !ok {
			return nil, errPseudonymVanished
		}
		result[v] = token
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
