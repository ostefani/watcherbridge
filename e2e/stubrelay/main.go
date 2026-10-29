// Command stubrelay is the E2E stand-in for ScanRelay: it records what
// the bridge forwards and posts, with just enough validation to prove
// the contract. Not for production; the harness drives it.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

var (
	annotations = flag.String("dir", "", "directory for recorded payloads (required)")
	port        = flag.Int("port", 18080, "loopback port")
	failMode    = flag.String("fail", "", "failure injection: annotations-500 or webhook-500")
)

var webhooks, notes atomic.Int64

func main() {
	flag.Parse()
	if *annotations == "" {
		log.Fatal("--dir is required")
	}
	if err := os.MkdirAll(*annotations, 0o700); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if *failMode == "webhook-500" {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		n := webhooks.Add(1)
		sum := sha256.Sum256(raw)
		rec := fmt.Sprintf("sha256=%s\n%s", hex.EncodeToString(sum[:]), raw)
		if err := os.WriteFile(filepath.Join(*annotations, fmt.Sprintf("webhook-%03d.json", n)), []byte(rec), 0o600); err != nil {
			log.Printf("record webhook: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /api/annotations", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if *failMode == "annotations-500" {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		// Minimal contract check mirroring ScanRelay's Validate: required
		// fields present, levels known. Anything deeper is ScanRelay's job.
		s := string(raw)
		for _, k := range []string{`"briefing_id"`, `"event_ids"`, `"priority"`} {
			if !strings.Contains(s, k) {
				http.Error(w, "missing "+k, http.StatusBadRequest)
				return
			}
		}
		f, err := os.OpenFile(filepath.Join(*annotations, "annotations.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			http.Error(w, "store", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		if _, err := f.Write(append(raw, '\n')); err != nil {
			http.Error(w, "store", http.StatusInternalServerError)
			return
		}
		notes.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("stubrelay: on http://%s/ (fail=%s)", addr, *failMode)
	log.Fatal(http.ListenAndServe(addr, mux))
}
