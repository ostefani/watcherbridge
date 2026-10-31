package briefing

import (
	"strings"
	"testing"

	"github.com/ostefani/watcherbridge/internal/store"
)

func TestPlural(t *testing.T) {
	if got := plural(1, "event"); got != "1 event" {
		t.Fatalf("plural(1) = %q", got)
	}
	if got := plural(3, "event"); got != "3 events" {
		t.Fatalf("plural(3) = %q", got)
	}
	if got := plural(0, "check"); got != "0 checks" {
		t.Fatalf("plural(0) = %q", got)
	}
}

func TestDedupStrings(t *testing.T) {
	in := []string{"nebula-1.local", "tv", "nebula-1.local", "pc", "tv"}
	got := dedupStrings(in)
	want := []string{"nebula-1.local", "tv", "pc"}
	if len(got) != len(want) {
		t.Fatalf("dedup = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedup = %v, want %v", got, want)
		}
	}
	if len(in) != 5 {
		t.Fatal("dedup must not mutate its input")
	}
}

func TestStripAnchorPrefix(t *testing.T) {
	if got := stripAnchorPrefix("host:nebula-1.local"); got != "nebula-1.local" {
		t.Fatalf("prefix not dropped: %q", got)
	}
	if got := stripAnchorPrefix("nebula-1.local"); got != "nebula-1.local" {
		t.Fatalf("bare value mangled: %q", got)
	}
}

func takenWithTypes(types ...string) store.TakenGroup {
	taken := store.TakenGroup{GroupID: "grp-1", Anchor: "host:x"}
	for i, typ := range types {
		taken.Events = append(taken.Events, store.RawEvent{EventID: "e" + string(rune('a'+i)), EventType: typ})
	}
	return taken
}

// The screenshot's appeared case names each device once: repeated
// sightings collapse into counts, never repeated values, and the anchor
// renders without its key prefix.
func TestDescribeEventsAppeared(t *testing.T) {
	got := describeEvents(takenWithTypes("host.appeared", "host.appeared", "host.changed"))
	want := "3 events on x: 2 host.appeared, 1 host.changed"
	if got != want {
		t.Fatalf("describe = %q, want %q", got, want)
	}
	if strings.Count(got, "host.appeared") != 1 {
		t.Fatalf("event type repeated in %q", got)
	}
	if strings.Contains(got, "host:") {
		t.Fatalf("anchor-key prefix leaked into %q", got)
	}
	got = describeEvents(takenWithTypes("host.appeared"))
	if want := "1 event on x: 1 host.appeared"; got != want {
		t.Fatalf("describe = %q, want %q", got, want)
	}
	taken := takenWithTypes("host.appeared", "host.appeared")
	taken.Anchor = "host:nebula-1.local"
	if got := describeEvents(taken); got != "2 events on nebula-1.local: 2 host.appeared" {
		t.Fatalf("anchor prefix not stripped: %q", got)
	}
}

// A degenerate anchor (ingest caps hostnames far above display widths)
// cannot blow up the deterministic sentence: the label is capped and
// marked, so fallback text stays orders of magnitude under the restored
// budget even for a 300 KiB hostile hostname.
func TestDescribeEventsCapsHugeAnchor(t *testing.T) {
	taken := takenWithTypes("host.appeared")
	taken.Anchor = "host:" + strings.Repeat("a", 300<<10)
	got := describeEvents(taken)
	if len(got) > 1024 {
		t.Fatalf("huge anchor must be capped, sentence is %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "…: 1 host.appeared") {
		t.Fatalf("capped label must be marked: %q", got[:64])
	}
	if strings.Count(got, "a") > maxAnchorLabelRunes+8 {
		t.Fatalf("oversized anchor reproduced in %q", got[:64])
	}
}

// Stored model text is full-length (Designv8 §2.5): over-long output is
// kept, never marked, for the UI to clamp.
func TestPlainTextKeepsLongInput(t *testing.T) {
	long := strings.Repeat("x", 1000)
	if got := plainText(long); len(got) != 1000 || strings.Contains(got, "…[truncated]") {
		t.Fatalf("stored text must be full-length, got %d chars", len(got))
	}
	if got := plainText("a\x00b"); got != "ab" {
		t.Fatalf("controls must still be stripped: %q", got)
	}
}
