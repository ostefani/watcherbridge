package server

import (
	"encoding/json"
	"log"
	"net/netip"
	"strings"
)

// hostState mirrors the Pro webhook host object fields the anchor needs.
// Only identity fields are decoded; everything else is ignored.
type hostState struct {
	IP            string `json:"ip"`
	Hostname      string `json:"hostname"`
	MAC           string `json:"mac"`
	RandomizedMAC bool   `json:"randomized_mac"`
}

// anchorForEvent derives the grouping anchor from the payload's host
// object: `host` for appear/disappear, `current` falling back to
// `previous` for changed. Preference is MAC (unless randomized), then
// hostname, then IP. It returns fallback=true with the event key when the
// payload carries no usable host identifier — grouping still works, one
// group per event key, but the operator should see it in the logs.
//
// Pro's `event_key` is never a valid anchor: it appends a rounded time
// window (`alertKey:window`), so the same host gets a different key every
// window (Designv5 §10).
func anchorForEvent(body []byte, eventType, eventKey string) (anchor string, fallback bool) {
	var payload struct {
		Host     *hostState `json:"host"`
		Current  *hostState `json:"current"`
		Previous *hostState `json:"previous"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return eventKey, true
	}
	var h *hostState
	switch eventType {
	case "host.changed":
		h = payload.Current
		if h == nil {
			h = payload.Previous
		}
	default:
		h = payload.Host
	}
	if h == nil {
		return eventKey, true
	}
	if !h.RandomizedMAC {
		if mac := strings.ToLower(strings.TrimSpace(h.MAC)); mac != "" {
			return "mac:" + mac, false
		}
	}
	if name := strings.ToLower(strings.TrimSpace(h.Hostname)); name != "" {
		return "host:" + strings.TrimSuffix(name, "."), false
	}
	if ip := strings.TrimSpace(h.IP); ip != "" {
		if parsed, err := netip.ParseAddr(ip); err == nil {
			return "ip:" + parsed.String(), false
		}
		log.Printf("ingest: ignoring unparsable anchor ip for event_key %s", eventKey)
	}
	return eventKey, true
}
