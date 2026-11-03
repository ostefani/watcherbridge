package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// eventID limits keep the gate key and the store index bounded.
const (
	maxEventIDLen  = 128
	maxEventKeyLen = 256
)

// validEventIDChar bounds the gate key and the store index. It is a
// superset of what Pro generates today (UUID-like hex with '-'), kept
// wide for forward compatibility: width is not a security boundary here,
// the HMAC already binds the exact bytes, and a rejection is a visible
// 400 Pro will retry.
func validEventIDChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

type malformedError struct{ msg string }

func (e *malformedError) Error() string { return e.msg }

// parseAndValidate checks the timestamp format and window plus the event_id
// charset/length and the eventType allowlist. It runs after MaxBytesReader
// and before HMAC verification so oversized or garbage requests fail fast.
// Malformed requests return *malformedError (→ 400); timestamp-window and
// signature failures are plain errors (→ 401).
func parseAndValidate(now time.Time, tolerance time.Duration, timestamp, eventID, eventKey, eventType string, body []byte) (time.Time, error) {
	if len(body) == 0 {
		return time.Time{}, &malformedError{"empty body"}
	}
	if eventID == "" || eventKey == "" || eventType == "" {
		return time.Time{}, &malformedError{"event_id, event_key and event_type headers are required"}
	}
	if len(eventID) > maxEventIDLen {
		return time.Time{}, &malformedError{"event_id too long"}
	}
	for i := 0; i < len(eventID); i++ {
		if !validEventIDChar(eventID[i]) {
			return time.Time{}, &malformedError{"event_id has invalid characters"}
		}
	}
	if len(eventKey) > maxEventKeyLen || len(eventKey) == 0 {
		return time.Time{}, &malformedError{"event_key length out of range"}
	}
	if !eventTypes[eventType] {
		return time.Time{}, &malformedError{"unknown event_type"}
	}
	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, &malformedError{"signature timestamp is malformed"}
	}
	sigTime := time.Unix(secs, 0).UTC()
	if math.Abs(now.Sub(sigTime).Seconds()) > tolerance.Seconds() {
		return time.Time{}, fmt.Errorf("stale timestamp")
	}
	return sigTime, nil
}

// checkEnvelopeConsistency requires the body's echoed event_id, event_key
// and event_type to equal the signed headers. The HMAC covers raw body
// bytes, not parsed semantics, so a valid signature alone cannot
// reconcile two envelope identities: grouping keys the header identity
// while projection reads the body, and a split envelope would fork the
// pipeline's view of one delivery. Malformed bodies and mismatches are
// both 400s with static text, carrying no request content.
func checkEnvelopeConsistency(body []byte, eventID, eventKey, eventType string) error {
	var env struct {
		EventID   string `json:"event_id"`
		EventKey  string `json:"event_key"`
		EventType string `json:"event_type"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return &malformedError{"invalid event body"}
	}
	if env.EventID != eventID || env.EventKey != eventKey || env.EventType != eventType {
		return &malformedError{"body envelope does not match signed headers"}
	}
	return nil
}

// validSignature recomputes Pro's HMAC-SHA256 over
// timestamp.eventID.eventKey.eventType.body with constant-time compare.
func validSignature(secret, timestamp, eventID, eventKey, eventType string, body []byte, signature string) bool {
	if secret == "" || timestamp == "" || signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write([]byte(eventID))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write([]byte(eventKey))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write([]byte(eventType))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) == 1
}
