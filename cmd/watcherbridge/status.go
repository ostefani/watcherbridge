// Command status is `watcherbridge status`: a read-only client of the
// running bridge API (Step 20, Designv9 §3.3). It never opens or migrates
// the database and never starts workers — it sees the same in-memory
// health the API serves. Exit codes share the verdict mapping: quiet 0,
// review 1, act 2. Any failure to obtain status (connection, validation,
// computation) exits 3 and never reads as quiet.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/ostefani/watcherbridge/internal/overview"
)

// statusResult is the overview shape the CLI needs: verdict for the exit
// code, and headline plus coverage, liveness, receipts, window, and
// last reported change for humans, so a quiet result always carries its
// context and never reads as a presence claim. --json prints the raw
// body instead.
type statusResult struct {
	Verdict    string `json:"verdict"`
	Headline   string `json:"headline"`
	Coverage   string `json:"coverage"`
	Liveness   string `json:"liveness"`
	Receipts   int    `json:"receipts"`
	Window     string `json:"window"`
	LastChange string `json:"last_change"`
}

// runStatus implements the status subcommand; it returns the exit code.
func runStatus(args []string) int {
	fs := flag.NewFlagSet("watcherbridge status", flag.ContinueOnError)
	bridge := fs.String("bridge", "http://127.0.0.1:8081", "running bridge base URL")
	window := fs.String("window", "", "overview window (Go duration; empty means the bridge default)")
	jsonOut := fs.Bool("json", false, "print the raw overview body")
	timeout := fs.Duration("timeout", 10*time.Second, "API request timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	url := *bridge + "/api/overview"
	if *window != "" {
		if _, err := time.ParseDuration(*window); err != nil {
			fmt.Fprintf(os.Stderr, "status: invalid window: %v\n", err)
			return 3
		}
		url += "?window=" + *window
	}
	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: bridge unreachable: %v\n", err)
		return 3
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: read overview: %v\n", err)
		return 3
	}
	if *jsonOut && resp.StatusCode == http.StatusOK {
		fmt.Println(string(body))
	}
	return statusExit(resp.StatusCode, body, *jsonOut)
}

// statusExit maps an API answer to the process exit code. Anything but
// a decodable known verdict exits 3: failure to obtain status must
// never read as quiet.
func statusExit(httpStatus int, body []byte, jsonOut bool) int {
	if httpStatus != http.StatusOK {
		fmt.Fprintf(os.Stderr, "status: bridge answered %d: %s\n", httpStatus, truncateLine(body))
		return 3
	}
	var res statusResult
	if err := json.Unmarshal(body, &res); err != nil {
		fmt.Fprintf(os.Stderr, "status: decode overview: %v\n", err)
		return 3
	}
	switch res.Verdict {
	case overview.VerdictQuiet, overview.VerdictReview, overview.VerdictAct:
		if !jsonOut {
			fmt.Printf("%s: %s (coverage %s, liveness %s)\n", res.Verdict, res.Headline, res.Coverage, res.Liveness)
			fmt.Printf("  receipts %d, window %s", res.Receipts, res.Window)
			if res.LastChange != "" {
				fmt.Printf(", last change %s", res.LastChange)
			}
			fmt.Println()
		}
		return overview.ExitCodeFor(res.Verdict)
	default:
		fmt.Fprintf(os.Stderr, "status: unknown verdict %q\n", res.Verdict)
		return 3
	}
}

// truncateLine keeps error output to one short line without payloads.
func truncateLine(body []byte) string {
	if len(body) > 200 {
		body = body[:200]
	}
	for i, b := range body {
		if b == '\n' {
			return string(body[:i])
		}
	}
	return string(body)
}
