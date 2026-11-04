// Package sig builds signed bridge webhook requests for the E2E and
// load helpers: one HMAC-SHA256 construction shared by send and
// loadgen, so measured traffic signs exactly like scripted traffic.
package sig

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Post signs body with (timestamp, id, key, type) and POSTs it,
// returning the status code and client-measured latency.
func Post(client *http.Client, url, secret, id, key, typ, timestamp, body string) (int, time.Duration, error) {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s.%s.%s.%s.%s", timestamp, id, key, typ, body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("X-SubnetLens-Event-ID", id)
	req.Header.Set("X-SubnetLens-Event-Key", key)
	req.Header.Set("X-SubnetLens-Event-Type", typ)
	req.Header.Set("X-SubnetLens-Signature-Timestamp", timestamp)
	req.Header.Set("X-SubnetLens-Signature-256", sig)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	lat := time.Since(start)
	if err != nil {
		return 0, lat, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, lat, nil
}

// Timestamp returns the current signature timestamp.
func Timestamp() string { return strconv.FormatInt(time.Now().Unix(), 10) }
