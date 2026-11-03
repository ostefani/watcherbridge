// Command send posts one signed webhook in Pro's exact envelope. The
// HMAC scheme mirrors Pro's signWebhookPayload: sha256= over
// timestamp.eventID.eventKey.eventType.body. {{NOW}} in the payload file
// is replaced with the current UTC time so occurred_at stays fresh.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ostefani/watcherbridge/e2e/sig"
)

func main() {
	secret := flag.String("secret", "", "HMAC secret (required)")
	url := flag.String("url", "", "webhook URL (required)")
	id := flag.String("id", "", "event id (required)")
	key := flag.String("key", "e2e-fixture", "event key")
	typ := flag.String("type", "host.changed", "event type")
	payloadFile := flag.String("payload", "", "JSON body file, {{NOW}} expanded (required)")
	outFile := flag.String("out", "", "write the exact signed bytes here for byte-identity checks")
	rewriteEnvelope := flag.Bool("rewrite-envelope", true, "set body event_id/key/type from flags before signing")
	heartbeat := flag.Bool("heartbeat", false, "generate an H1 scan.heartbeat body instead of reading payload")
	hbTarget := flag.String("hb-target", "192.0.2.0/24", "heartbeat target")
	hbStatus := flag.String("hb-status", "ok", "heartbeat status (ok needs zero issues)")
	flag.Parse()
	if *secret == "" || *url == "" || *id == "" || (*payloadFile == "" && !*heartbeat) {
		log.Fatal("secret, url, and id are required, plus payload unless -heartbeat")
	}
	var body string
	if *heartbeat {
		// H1 requires exactly the 12 wire keys with scan_id == event_id
		// (32 lowercase hex), key == "heartbeat:"+id, type
		// scan.heartbeat and occurred_at == completed_at. The id flag
		// carries the scan ID; key and type are forced to match.
		now := time.Now().UTC().Truncate(time.Second)
		*key, *typ = "heartbeat:"+*id, "scan.heartbeat"
		hb := map[string]any{
			"event_id": *id, "event_key": *key, "event_type": *typ,
			"occurred_at": now.Format(time.RFC3339), "cycle": 1, "target": *hbTarget,
			"scan_id": *id, "completed_at": now.Format(time.RFC3339),
			"hosts_seen": 0, "duration_ms": 1000, "status": *hbStatus, "issue_count": 0,
		}
		encoded, err := json.Marshal(hb)
		if err != nil {
			log.Fatal(err)
		}
		body = string(encoded)
	} else {
		raw, err := os.ReadFile(*payloadFile)
		if err != nil {
			log.Fatal(err)
		}
		body = strings.ReplaceAll(string(raw), "{{NOW}}", time.Now().UTC().Format(time.RFC3339))
	}
	if *rewriteEnvelope {
		// The bridge enforces header/body envelope consistency, so the
		// signed body must echo the header identity. Rewrite the three
		// envelope fields from the flags, leaving every other byte of
		// producer-shaped content untouched. Use -rewrite-envelope=false
		// to send the fixture bytes verbatim as an explicit negative.
		var env map[string]any
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			log.Fatalf("rewrite envelope: %v", err)
		}
		env["event_id"], env["event_key"], env["event_type"] = *id, *key, *typ
		rewritten, err := json.Marshal(env)
		if err != nil {
			log.Fatalf("rewrite envelope: %v", err)
		}
		body = string(rewritten)
	}
	if *outFile != "" {
		if err := os.WriteFile(*outFile, []byte(body), 0o600); err != nil {
			log.Fatal(err)
		}
	}
	status, _, err := sig.Post(http.DefaultClient, *url, *secret, *id, *key, *typ, sig.Timestamp(), body)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("STATUS=%d\n", status)
}
