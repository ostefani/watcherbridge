package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/ostefani/watcherbridge/internal/store"
)

// decodeHeartbeat accepts only the fixed flat H1 schema. Detect duplicate keys
// before typed decoding: last-key-wins would make signed metadata ambiguous.
func decodeHeartbeat(body []byte, id, key string, signedAt, receivedAt time.Time, source string) (store.Heartbeat, error) {
	var zero store.Heartbeat
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return zero, errMalformedBody
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		name, ok := tok.(string)
		if err != nil || !ok || seen[name] || len(seen) >= 12 {
			return zero, errMalformedBody
		}
		// encoding/json also accepts case-insensitive tag matches. Only
		// exact H1 keys count toward completeness; aliases could otherwise
		// overwrite a field while concealing another missing field.
		switch name {
		case "event_id", "event_key", "event_type", "occurred_at", "cycle", "target",
			"scan_id", "completed_at", "hosts_seen", "duration_ms", "status", "issue_count":
		default:
			return zero, errMalformedBody
		}
		seen[name] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return zero, errMalformedBody
		}
	}
	if _, err := dec.Token(); err != nil || len(seen) != 12 {
		return zero, errMalformedBody
	}
	var wire struct {
		EventID     string    `json:"event_id"`
		EventKey    string    `json:"event_key"`
		EventType   string    `json:"event_type"`
		OccurredAt  time.Time `json:"occurred_at"`
		Cycle       int64     `json:"cycle"`
		Target      string    `json:"target"`
		ScanID      string    `json:"scan_id"`
		CompletedAt time.Time `json:"completed_at"`
		HostsSeen   int64     `json:"hosts_seen"`
		DurationMS  int64     `json:"duration_ms"`
		Status      string    `json:"status"`
		IssueCount  int64     `json:"issue_count"`
	}
	dec = json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return zero, errMalformedBody
	}
	if err := dec.Decode(new(any)); err != io.EOF || wire.EventID != id || wire.ScanID != id ||
		wire.EventKey != key || key != "heartbeat:"+id || wire.EventType != "scan.heartbeat" ||
		!wire.OccurredAt.Equal(wire.CompletedAt) || wire.Cycle < 1 {
		return zero, errMalformedBody
	}
	h := store.Heartbeat{
		HeartbeatSummary: store.HeartbeatSummary{ScanID: id, Target: wire.Target, CompletedAt: wire.CompletedAt,
			ReceivedAt: receivedAt, HostsSeen: wire.HostsSeen, DurationMS: wire.DurationMS, Status: wire.Status, IssueCount: wire.IssueCount},
		SourceScope: source, SignatureAt: signedAt, Payload: body,
	}
	if h.Validate() != nil {
		return zero, errMalformedBody
	}
	return h, nil
}

// acceptHeartbeat is deliberately outside the forwarding gate and host-event
// pipeline. SQLite supplies idempotency, including concurrent retries.
func (s *Server) acceptHeartbeat(ctx context.Context, w http.ResponseWriter, body []byte, id, key string, signedAt time.Time) {
	h, err := decodeHeartbeat(body, id, key, signedAt, s.now().UTC(), s.cfg.SourceID)
	if err != nil {
		s.Health.AddRejected()
		writeError(w, http.StatusBadRequest, "invalid heartbeat body")
		return
	}
	pctx, cancel := context.WithTimeout(ctx, s.cfg.PersistBudget)
	defer cancel()
	inserted, err := s.events.InsertHeartbeat(pctx, h)
	if errors.Is(err, store.ErrHeartbeatConflict) {
		s.Health.AddRejected()
		writeError(w, http.StatusConflict, "heartbeat event ID conflict")
		return
	}
	if err != nil {
		s.reportPersistError(w, id, err)
		return
	}
	if inserted {
		s.Health.AddAccepted()
		s.viewer.Overview.Trigger()
	} else {
		s.Health.AddDuplicate()
	}
	writeAccepted(w, id, "scan.heartbeat")
}
