package briefing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ostefani/watcherbridge/internal/provider"
	"github.com/ostefani/watcherbridge/internal/redactor"
	"github.com/ostefani/watcherbridge/internal/store"
)

// Pre-send scan failures block without transmitting (Step 30 §5.5–5.6):
// an over-cap transcript and a scanner error both fail closed to the
// deterministic fallback, and the provider sees zero calls.
func TestAgentOversizedTranscriptBlocksWithoutSend(t *testing.T) {
	big := strings.Repeat("x", maxTranscriptBytes+1)
	l := &agentLoop{
		taken: store.TakenGroup{GroupID: "g-cap"},
		env: &agentEnv{
			transcript: []provider.TurnMessage{{Role: "user", Text: big}},
			seenEvents: map[string]bool{},
		},
	}
	if reason := l.scanTranscript(); reason == "" {
		t.Fatal("over-cap transcript must block")
	} else if !strings.Contains(reason, "size cap") {
		t.Fatalf("cap block reason = %q, want the size-cap fallback text", reason)
	}
	fake := provider.NewFake()
	l.turner = fake
	if _, err := l.turn(context.Background()); err == nil {
		t.Fatal("blocked turn must error")
	} else if !errors.As(err, new(*transcriptBlockedError)) {
		t.Fatalf("blocked turn error = %T, want transcriptBlockedError", err)
	}
	if fake.TurnCalls != 0 {
		t.Fatalf("blocked transcript transmitted %d calls, want 0", fake.TurnCalls)
	}
}

// The scanner reports undecodable input as an error, never a silent
// pass. Scope honesty: this pins the scanner half of the fail-closed
// pair. The caller half (`err != nil` in scanTranscript) is defensive
// only — neutralizeReferenceIDs returns either its valid marshaled
// input or a fresh valid marshal, so no reachable transcript makes the
// scan input undecodable. The zero-send mapping it guards is shared
// with the leak and cap blocks and is covered for both; see the matrix
// row for the inspection record.
func TestScannerErrorIsAnError(t *testing.T) {
	if _, err := redactor.ScanOutboundLeaks([]byte("not json {{{"), nil, nil); err == nil {
		t.Fatal("undecodable scan input must error, not pass clean")
	}
}
