package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxHeartbeatBytes bounds the complete H1 envelope, independently of host events.
const MaxHeartbeatBytes = 8 << 10

// ErrHeartbeatConflict means an immutable event ID was reused with other bytes.
var ErrHeartbeatConflict = errors.New("heartbeat event ID conflict")

// HeartbeatSummary is compact source state retained after H1 history expires.
// FreshAt is receiver-local completion time estimated from the age at signing;
// signing an old queued scan again never makes that scan current.
type HeartbeatSummary struct {
	ScanID           string    `json:"scan_id"`
	Target           string    `json:"target"`
	CompletedAt      time.Time `json:"completed_at"`
	ReceivedAt       time.Time `json:"received_at"`
	FreshAt          time.Time `json:"fresh_at"`
	HostsSeen        int64     `json:"hosts_seen"`
	DurationMS       int64     `json:"duration_ms"`
	Status           string    `json:"status"`
	IssueCount       int64     `json:"issue_count"`
	ClockSkewSeconds int64     `json:"clock_skew_seconds"`
}

// Heartbeat is an authenticated H1 record. SourceScope comes from receiver
// configuration, never a payload field. Payload contains original signed bytes.
type Heartbeat struct {
	HeartbeatSummary
	SourceScope string
	SignatureAt time.Time
	Payload     []byte
}

// Validate checks bounded H1 metadata before storage or arithmetic. A completion
// can precede signing by arbitrarily long queueing; it cannot follow signing
// except for the signature timestamp's subsecond rounding.
func (h Heartbeat) Validate() error {
	if h.SourceScope == "" || len(h.SourceScope) > 256 || len(h.Payload) == 0 || len(h.Payload) > MaxHeartbeatBytes ||
		len(h.ScanID) != 32 || strings.ToLower(h.ScanID) != h.ScanID || len(h.Target) == 0 || len(h.Target) > 256 {
		return fmt.Errorf("invalid heartbeat shape")
	}
	if _, err := hex.DecodeString(h.ScanID); err != nil {
		return fmt.Errorf("invalid heartbeat scan ID")
	}
	for _, t := range []time.Time{h.CompletedAt, h.ReceivedAt, h.SignatureAt} {
		if t.Before(time.Unix(0, 0)) || !t.Equal(time.Unix(0, t.UnixNano())) {
			return fmt.Errorf("invalid heartbeat time")
		}
	}
	if h.CompletedAt.After(h.SignatureAt.Add(time.Second)) || h.HostsSeen < 0 || h.DurationMS < 0 ||
		h.DurationMS > int64((1<<63-1)/time.Millisecond) || h.IssueCount < 0 {
		return fmt.Errorf("invalid heartbeat timing or counts")
	}
	if (h.Status == "ok" && h.IssueCount == 0) || ((h.Status == "partial" || h.Status == "error") && h.IssueCount > 0) {
		return nil
	}
	return fmt.Errorf("invalid heartbeat status")
}

// InsertHeartbeat commits history and source progress together. Duplicate bodies
// do not update receipt/freshness state. Older completions cannot regress the
// summary; retries after history purge likewise cannot refresh its high-water mark.
func (s *Store) InsertHeartbeat(ctx context.Context, h Heartbeat) (bool, error) {
	if err := h.Validate(); err != nil {
		return false, err
	}
	age := h.SignatureAt.Sub(h.CompletedAt)
	if age < 0 {
		age = 0
	}
	h.FreshAt = h.ReceivedAt.Add(-age)
	h.ClockSkewSeconds = int64(h.SignatureAt.Sub(h.ReceivedAt) / time.Second)
	summary, err := json.Marshal(h.HeartbeatSummary)
	if err != nil {
		return false, err
	}
	inserted := false
	err = s.write(ctx, func(tx *sql.Tx) error {
		var old []byte
		err := tx.QueryRow(`SELECT payload FROM heartbeats WHERE source_scope = ? AND event_id = ?`, h.SourceScope, h.ScanID).Scan(&old)
		if err == nil {
			if !bytes.Equal(old, h.Payload) {
				return ErrHeartbeatConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO report_sources (source_scope) VALUES (?)`, h.SourceScope); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO heartbeats (source_scope, event_id, completed_at, received_at, payload)
			VALUES (?, ?, ?, ?, ?)`, h.SourceScope, h.ScanID, nanos(h.CompletedAt), nanos(h.ReceivedAt), h.Payload); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE report_sources SET heartbeat_enabled = 1, last_heartbeat_at = ?,
			heartbeat_completed_at = ?, heartbeat_summary = ? WHERE source_scope = ?
			AND (heartbeat_completed_at IS NULL OR heartbeat_completed_at < ?)`,
			nanos(h.ReceivedAt), nanos(h.CompletedAt), string(summary), h.SourceScope, nanos(h.CompletedAt)); err != nil {
			return err
		}
		inserted = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return inserted, nil
}
