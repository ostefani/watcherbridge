package store

import (
	"strings"
	"testing"
)

// M8: error text keeps IDs and statuses, never payload values, and no
// control characters survive for log injection.
func TestSanitizeErrorText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // required in output
		gone []string // must be absent from output
	}{
		{"ipv4 port", "dial tcp 192.168.0.1:8080: refused", []string{"[addr]"}, []string{"192.168.0.1"}},
		{"ipv6 full", "from 2001:db8::1 failed", []string{"[addr]"}, []string{"2001:db8::1"}},
		{"ipv6 zoned", "via fe80::1%eth0 down", []string{"[addr]"}, []string{"fe80::1"}},
		{"mac colon", "host AA:BB:CC:DD:EE:FF lost", []string{"[addr]"}, []string{"AA:BB:CC:DD:EE:FF"}},
		{"mac dash", "saw AA-BB-CC-DD-EE-FF", []string{"[addr]"}, []string{"AA-BB-CC-DD-EE-FF"}},
		{"mac cisco", "got aabb.ccdd.eeff", []string{"[addr]"}, []string{"aabb.ccdd.eeff"}},
		{"url", "get https://x.test/a failed", []string{"[url]"}, []string{"x.test"}},
		{"clock kept", "at 12:30:45 retry", []string{"12:30:45"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeErrorText(tc.in)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("want %q in %q", w, got)
				}
			}
			for _, g := range tc.gone {
				if strings.Contains(got, g) {
					t.Fatalf("value %q survived in %q", g, got)
				}
			}
		})
	}
}

func TestSanitizeErrorTextControls(t *testing.T) {
	got := SanitizeErrorText("host \u0000\u202e\u200bprinter\u0085")
	for _, r := range []rune{0, 0x202e, 0x200b, 0x85} {
		if strings.ContainsRune(got, r) {
			t.Fatalf("control %U survived in %q", r, got)
		}
	}
	if !strings.Contains(got, "printer") {
		t.Fatalf("printable text lost: %q", got)
	}
	// Newlines and tabs are legitimate framing.
	if got := SanitizeErrorText("a\nb\tc"); got != "a\nb\tc" {
		t.Fatalf("framing changed: %q", got)
	}
}
