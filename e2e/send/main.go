// Command send posts one signed webhook in Pro's exact envelope. The
// HMAC scheme mirrors Pro's signWebhookPayload: sha256= over
// timestamp.eventID.eventKey.eventType.body. {{NOW}} in the payload file
// is replaced with the current UTC time so occurred_at stays fresh.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	secret := flag.String("secret", "", "HMAC secret (required)")
	url := flag.String("url", "", "webhook URL (required)")
	id := flag.String("id", "", "event id (required)")
	key := flag.String("key", "e2e-fixture", "event key")
	typ := flag.String("type", "host.changed", "event type")
	payloadFile := flag.String("payload", "", "JSON body file, {{NOW}} expanded (required)")
	outFile := flag.String("out", "", "write the exact signed bytes here for byte-identity checks")
	flag.Parse()
	if *secret == "" || *url == "" || *id == "" || *payloadFile == "" {
		log.Fatal("secret, url, id, and payload are required")
	}
	raw, err := os.ReadFile(*payloadFile)
	if err != nil {
		log.Fatal(err)
	}
	body := strings.ReplaceAll(string(raw), "{{NOW}}", time.Now().UTC().Format(time.RFC3339))
	if *outFile != "" {
		if err := os.WriteFile(*outFile, []byte(body), 0o600); err != nil {
			log.Fatal(err)
		}
	}
	ts := fmt.Sprint(time.Now().UTC().Unix())
	mac := hmac.New(sha256.New, []byte(*secret))
	for _, part := range []string{ts, ".", *id, ".", *key, ".", *typ, "."} {
		mac.Write([]byte(part))
	}
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req, err := http.NewRequest(http.MethodPost, *url, strings.NewReader(body))
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-SubnetLens-Event-ID", *id)
	req.Header.Set("X-SubnetLens-Event-Key", *key)
	req.Header.Set("X-SubnetLens-Event-Type", *typ)
	req.Header.Set("X-SubnetLens-Signature-Timestamp", ts)
	req.Header.Set("X-SubnetLens-Signature-256", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	fmt.Printf("STATUS=%d\n", resp.StatusCode)
}
