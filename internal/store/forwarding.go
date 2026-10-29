package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

func (s *Store) ForwardState(ctx context.Context, eventID string) (forwarded bool, attempts int, lastErr string, err error) {
	var fwd int
	var last sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT forwarded, forward_attempts, last_forward_error FROM raw_events WHERE event_id = ?`, eventID).Scan(&fwd, &attempts, &last)
	return fwd == 1, attempts, last.String, err
}

// MarkForwarded records a successful delivery once; repeats are no-ops.
func (s *Store) MarkForwarded(ctx context.Context, eventID string, at time.Time) (bool, error) { // returns false on any error (captured var is only read on success)
	matched := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE raw_events SET forwarded = 1, forwarded_at = ?, forward_attempts = forward_attempts + 1 WHERE event_id = ? AND forwarded = 0`, nanos(at), eventID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		matched = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

var (
	urlPattern  = regexp.MustCompile(`https?://\S+`)
	addrPattern = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(:\d+)?\b`)
	// IPv6 shapes in errors: a "::" run or 4+ colon groups. Plain
	// clock times ("12:30:45", two colons, no hex need) stay literal.
	v6Pattern  = regexp.MustCompile(`(?i)\b(?:[0-9a-f]*::[0-9a-f:.]*|(?:[0-9a-f]{1,4}:){3,}[0-9a-f:.]*)\b`)
	macPattern = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{2}[:-]){5}[0-9a-f]{2}\b|\b(?:[0-9a-f]{4}\.){2}[0-9a-f]{4}\b|\b[0-9a-f]{12}\b`)
)

// SanitizeErrorText strips URLs, IPv4/IPv6, and MACs from error text so
// logs and stored errors carry IDs and statuses, never payload values,
// and drops control characters so hostile names cannot smuggle terminal
// or log-injection text. Shared by the forwarder receipt path and the
// briefing worker. The control strip mirrors redactor.StripControls;
// it lives here because store cannot import redactor (import cycle).
func SanitizeErrorText(s string) string {
	s = urlPattern.ReplaceAllString(s, "[url]")
	s = addrPattern.ReplaceAllString(s, "[addr]")
	s = v6Pattern.ReplaceAllString(s, "[addr]")
	s = macPattern.ReplaceAllString(s, "[addr]")
	s = strings.Map(func(r rune) rune {
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
	if r := []rune(s); len(r) > 256 {
		s = string(r[:256])
	}
	return s
}

func (s *Store) RecordForwardFailure(ctx context.Context, eventID, errText string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE raw_events SET forward_attempts = forward_attempts + 1, last_forward_error = ? WHERE event_id = ?`, SanitizeErrorText(errText), eventID)
		return err
	})
}

func (s *Store) SweepCandidates(ctx context.Context, now time.Time, tolerance, margin time.Duration, limit int) ([]RawEvent, error) {
	if margin >= tolerance {
		return nil, fmt.Errorf("sweep margin must be smaller than tolerance")
	}
	if limit <= 0 {
		limit = 20 // payloads can be ~1 MiB; keep one batch bounded
	}
	cutoff := nanos(now.Add(-(tolerance - margin)))
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, event_key, event_type, occurred_at, cycle, COALESCE(target,''), payload, headers_json, received_at, signature_timestamp
		FROM raw_events WHERE forwarded = 0 AND delivery_error IS NULL AND signature_timestamp >= ? ORDER BY received_at LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawEvent
	for rows.Next() {
		var ev RawEvent
		var occurred, received, sig int64
		if err := rows.Scan(&ev.EventID, &ev.EventKey, &ev.EventType, &occurred, &ev.Cycle, &ev.Target, &ev.Payload, &ev.HeadersJSON, &received, &sig); err != nil {
			return nil, err
		}
		ev.OccurredAt, ev.ReceivedAt, ev.SignatureTimestamp = fromNanos(occurred), fromNanos(received), fromNanos(sig)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) MarkStaleSweep(ctx context.Context, now time.Time, tolerance, margin time.Duration) (int64, error) {
	if margin >= tolerance {
		return 0, fmt.Errorf("sweep margin must be smaller than tolerance")
	}
	var affected int64
	cutoff := nanos(now.Add(-(tolerance - margin)))
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE raw_events SET delivery_error = 'stale_for_sweep' WHERE forwarded = 0 AND delivery_error IS NULL AND signature_timestamp < ?`, cutoff)
		if err != nil {
			return err
		}
		affected, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// purgeFloor is the youngest age PurgeRawEvents will ever accept.
func WhitelistForwardHeaders(headers map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range headers {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-subnetlens-") || lk == "content-type" {
			out[k] = v
		}
	}
	return out
}

const maxHeadersJSONBytes = 4096

// validateHeadersJSON enforces the whitelist inside the store, so no caller
// can persist unfiltered headers by forgetting WhitelistForwardHeaders.
func validateHeadersJSON(headersJSON string) error {
	if headersJSON == "" {
		return nil
	}
	if len(headersJSON) > maxHeadersJSONBytes {
		return fmt.Errorf("headers_json exceeds %d bytes", maxHeadersJSONBytes)
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
		return fmt.Errorf("headers_json must be a JSON object of strings: %w", err)
	}
	for k := range headers {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "x-subnetlens-") && lk != "content-type" {
			return fmt.Errorf("header %q is not whitelisted for storage", k)
		}
	}
	return nil
}

// RestorePseudonym returns the original (display) value for a token.
