package main

import (
	"testing"
)

// Step 20 status contract: verdicts map to 0/1/2, and any failure to
// obtain status — server error, undecodable or unknown verdict — exits
// 3, never a quiet-looking 0. The live fetch needs a loopback listener,
// which the sandbox forbids, so the suite pins the pure mapping; the
// fetch path is the thin wiring above it.
func TestStatusExit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		httpStatus int
		body       string
		want       int
	}{
		{"quiet 0", 200, `{"verdict":"quiet","headline":"x","coverage":"ok"}`, 0},
		{"review 1", 200, `{"verdict":"review","headline":"x","coverage":"ok"}`, 1},
		{"act 2", 200, `{"verdict":"act","headline":"x","coverage":"ok"}`, 2},
		{"server error 3", 500, "boom", 3},
		{"bad verdict 3", 200, `{"verdict":"bogus"}`, 3},
		{"undecodable 3", 200, "not json", 3},
		{"empty 3", 200, "", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusExit(tc.httpStatus, []byte(tc.body), false); got != tc.want {
				t.Fatalf("exit = %d, want %d", got, tc.want)
			}
		})
	}
}
