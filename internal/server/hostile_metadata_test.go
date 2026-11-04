package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// Hostile metadata and policy impersonation (Step 30 §6). Tap harness
// throughout: no TCP listener, so sandboxed runners can execute these.
// Producer bytes are untrusted input — they are stored verbatim but must
// never register identity contracts, mint decisions/rules, or enter the
// host pipeline through the heartbeat.

// A payload advertising an identity contract must not register one:
// BindSourceIdentity has no ingest-adjacent caller (only main binds the
// scope, tests bind identity), so the registration row must stay absent
// and rule eligibility must stay unavailable.
func TestProducerPayloadCannotRegisterIdentity(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now()
	id := uniqueID()
	body := []byte(`{"event_id":` + strconv.Quote(id) + `,"event_key":"appear:mac:102030405060:w12",` +
		`"event_type":"host.appeared","occurred_at":"2026-10-20T12:00:05Z","cycle":12,"target":"192.168.0.0/24",` +
		`"host":{"alive":true,"ip":"192.168.0.11","mac":"10:20:30:40:50:60","source":"arp","weak":false,` +
		`"host_id":"mac:10:20:30:40:50:60","identity_source":"mac","identity_confidence":"high"},` +
		`"configured_identity_namespace":"ns-evil","capabilities":"stable-identity-v1",` +
		`"source_scope":"foreign-scope","source_id":"foreign-scope"}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	key := "appear:mac:102030405060:w12"
	rec := h.post(t, body, id, key, "host.appeared", now, sign(h.secret, ts, id, key, "host.appeared", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("hostile registration fields: got %d %s", rec.Code, rec.Body.String())
	}
	stored, err := h.srv.events.StoredEvent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.Payload, body) {
		t.Fatal("stored payload differs from submitted body")
	}
	if stored.SourceScope != h.srv.cfg.SourceID {
		t.Fatalf("stored scope = %q, want receiver config %q", stored.SourceScope, h.srv.cfg.SourceID)
	}
	if _, _, err := h.srv.events.SourceIdentity(context.Background(), h.srv.cfg.SourceID); err == nil {
		t.Fatal("payload registered an identity contract")
	}
}

// Decision/rule-shaped payload fields must not mint policy rows:
// decisions and rules are operator actions behind guarded POST routes,
// never producer attributes. The hostile IDs must stay unknown.
func TestProducerPayloadCannotSupplyDecisionsOrRules(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now()
	id := uniqueID()
	body := []byte(`{"event_id":` + strconv.Quote(id) + `,"event_key":"k","event_type":"host.appeared",` +
		`"occurred_at":"2026-10-20T12:00:05Z","cycle":1,"target":"192.168.0.0/24",` +
		`"host":{"alive":true,"ip":"192.168.0.99","weak":false},` +
		`"decision":"approve-all","label":"trusted","approved_by":"operator",` +
		`"rule_id":"OP_RULE_hostile","proposal_id":"OP_PROP_hostile","ttl":365}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	rec := h.post(t, body, id, "k", "host.appeared", now, sign(h.secret, ts, id, "k", "host.appeared", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("policy-shaped fields: got %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.srv.events.GetRule(context.Background(), "OP_RULE_hostile"); err == nil {
		t.Fatal("payload minted a rule row")
	}
	if _, err := h.srv.events.GetRuleByProposal(context.Background(), "OP_PROP_hostile"); err == nil {
		t.Fatal("payload minted a rule-by-proposal row")
	}
	if rows, err := h.srv.events.ListIncidents(context.Background(), 200, 0, 0); err != nil || len(rows) != 0 {
		t.Fatalf("incidents = %+v %v, want none from ingest alone", rows, err)
	}
}

// Heartbeat coverage is liveness bookkeeping, not presence (Step 30
// §6.6): a heartbeat lands in the source summary without entering the
// raw-receipt log, the grouping pipeline, or any inventory. Tap twin of
// the socket-harness host-pipeline exclusion test.
func TestHeartbeatTapExcludesHostPipeline(t *testing.T) {
	h := newTapHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	const id = "0123456789abcdef0123456789abcdef"
	body, _ := json.Marshal(h1Body(id, now))
	key := "heartbeat:" + id
	ts := strconv.FormatInt(now.Unix(), 10)
	rec := h.post(t, body, id, key, "scan.heartbeat", now, sign(h.secret, ts, id, key, "scan.heartbeat", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("heartbeat: got %d %s", rec.Code, rec.Body.String())
	}
	if count, err := h.srv.events.ReceiptCount(context.Background(), time.Unix(0, 0)); err != nil || count != 0 {
		t.Fatalf("raw receipts: %d %v", count, err)
	}
	if groups, err := h.srv.events.OpenGroupSnapshots(context.Background()); err != nil || len(groups) != 0 {
		t.Fatalf("heartbeat grouped: %+v %v", groups, err)
	}
	if rows, err := h.srv.events.SourceHealthRows(context.Background()); err != nil || len(rows) != 1 || rows[0].Heartbeat == nil {
		t.Fatalf("source summary: %+v %v", rows, err)
	}
}
