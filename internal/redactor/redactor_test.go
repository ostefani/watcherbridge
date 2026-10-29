package redactor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/ostefani/watcherbridge/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/redact.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testPolicy() Policy { return Policy{Enabled: true, MaxFieldChars: 256} }

func payload(t *testing.T, host string, extra string) []byte {
	t.Helper()
	return []byte(`{"event_id":"e1","event_key":"k","event_type":"host.changed","host":` + host + extra + `}`)
}

const richHost = `{"ip":"192.168.0.5","hostname":"Printer-2","mac":"AA:BB:CC:DD:EE:FF","weak":false,"alive":true,"identity_confidence":"high","evidence":"banner from 10.0.0.9 via fe80::1%eth0, see also 2001:db8::/32 and AA-BB-CC-DD-EE-00 and aabb.ccdd.eeff and deadbeefcafe"}`

func buildOne(t *testing.T, s *store.Store, p Policy, payload []byte) (*RedactedBundle, TokenMap, TokenMap) {
	t.Helper()
	b, issued, display, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "grp-1", Anchor: "host:Printer-2", Target: "192.168.0.0/24",
		Baseline: "low",
		Events:   []InputEvent{{EventID: "e1", EventType: "host.changed", Payload: payload}},
	}, p)
	if err != nil {
		t.Fatal(err)
	}
	return b, issued, display
}

func TestRoundTrip(t *testing.T) {
	s := testStore(t)
	b, issued, _ := buildOne(t, s, testPolicy(), payload(t, richHost, ""))
	if len(issued) == 0 {
		t.Fatal("expected issued tokens")
	}
	ser, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if hits, err := ScanOutboundLeaks(ser, issued, nil); err != nil || len(hits) != 0 {
		t.Fatalf("leaks in tokenized bundle: %v %v", hits, err)
	}
	// Every token restores; two-digit tokens prove IP_10 vs IP_1 safety.
	for tok, real := range issued {
		if got := Restore(tok, issued); got != real {
			t.Fatalf("restore %s = %q, want %q", tok, got, real)
		}
	}
	// Full-text round trip on the bundle JSON.
	restored := Restore(string(ser), issued)
	if strings.Contains(restored, "IP_") || strings.Contains(restored, "MAC_") || strings.Contains(restored, "HOST_") || strings.Contains(restored, "NET_") {
		t.Fatalf("tokens remain after restore: %s", restored[:200])
	}
	// Restoration returns canonical (normalized) forms: lowercase hostnames.
	if !strings.Contains(restored, "192.168.0.5") || !strings.Contains(restored, "printer-2") {
		t.Fatal("real values missing after restore")
	}
	if err := ValidateTokens(string(ser), issued); err != nil {
		t.Fatalf("own bundle must validate: %v", err)
	}
}

func TestElevenIPsTwoDigitTokens(t *testing.T) {
	s := testStore(t)
	var parts []string
	for i := 1; i <= 11; i++ {
		parts = append(parts, fmt.Sprintf(`"ip%d":"10.0.0.%d"`, i, i))
	}
	p := []byte(`{"event_id":"e1",` + strings.Join(parts, ",") + `}`)
	b, issued, _ := buildOne(t, s, testPolicy(), p)
	ser, _ := json.Marshal(b)
	if hits, _ := ScanOutboundLeaks(ser, issued, nil); len(hits) != 0 {
		t.Fatalf("leaks: %v", hits)
	}
	var found10, found1 bool
	for tok := range issued {
		if strings.HasPrefix(tok, "IP_") {
			if tok == "IP_10" || tok == "IP_11" {
				found10 = true
			}
			if tok == "IP_1" {
				found1 = true
			}
		}
	}
	if !found10 || !found1 {
		t.Fatalf("want IP_1 and two-digit tokens, got %v", issued)
	}
	restored := Restore(string(ser), issued)
	for i := 1; i <= 11; i++ {
		if !strings.Contains(restored, fmt.Sprintf("10.0.0.%d", i)) {
			t.Fatalf("10.0.0.%d lost in restore", i)
		}
	}
}

func TestInvalidIPv6StaysWhole(t *testing.T) {
	// Fuzz-found: a MAC alternative used to shred the longer invalid IPv6
	// into a fragment that tokenized glued to word characters.
	s := testStore(t)
	in := "0000000000000000 00:00:00:00:00:000.0.0.0"
	p := []byte(`{"event_id":"e","s":` + qjson(in) + `}`)
	b, issued, _, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "g", Baseline: "low",
		Events: []InputEvent{{EventID: "e", EventType: "host.appeared", Payload: p}},
	}, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ser, _ := json.Marshal(b)
	if hits, _ := ScanOutboundLeaks(ser, issued, nil); len(hits) != 0 {
		t.Fatalf("leaks: %v", hits)
	}
	if got := Restore(string(ser), issued); !strings.Contains(got, in) {
		t.Fatalf("fragmented input must survive whole, got: %s", got)
	}
}

// A token emitted after '%' re-parses as a zoned address ("%IP_1" is a
// valid zone ID). The bundle stays rejected fail-closed, and restore still
// round-trips: never skip spans preceded by '%', which would pass raw text.
func TestZoneGlueFailClosed(t *testing.T) {
	toks := TokenMap{}
	out := replaceSpans("::0%::0", func(cat, norm string) string {
		toks["IP_1"] = norm
		return "IP_1"
	})
	if out != "::0%IP_1" {
		t.Fatalf("unexpected tokenize: %q", out)
	}
	if hits, _ := ScanOutboundLeaks(mustJSON(out), toks, nil); len(hits) == 0 {
		t.Fatalf("zone-glued remainder not flagged: %q", out)
	}
	// netip normalizes the restored "::0" to "::"; the point is no
	// token survives and the value is recognizable, not byte identity.
	if got := Restore(out, toks); got != "::0%::" {
		t.Fatalf("no round-trip: %q", got)
	}
}

func TestRestoreGluedTokens(t *testing.T) {
	issued := TokenMap{"IP_1": "10.0.0.1"}
	// Glued to non-word chars restores; glued to word chars is not a token.
	if got := Restore("see IP_1:8080 now", issued); got != "see 10.0.0.1:8080 now" {
		t.Fatalf("port-joined token must restore: %q", got)
	}
	if got := Restore("see XIP_1 now", issued); got != "see XIP_1 now" {
		t.Fatalf("word-glued text must stay literal: %q", got)
	}
}

func TestInventedTokenRejected(t *testing.T) {
	issued := TokenMap{"IP_1": "10.0.0.1"}
	if err := ValidateTokens("saw IP_1 and IP_9", issued); err == nil {
		t.Fatal("invented IP_9 must be rejected")
	} else if !strings.Contains(err.Error(), "IP_9") {
		t.Fatalf("error must name the token: %v", err)
	}
	if err := ValidateTokens("saw IP_1 twice IP_1", issued); err != nil {
		t.Fatalf("issued tokens must validate: %v", err)
	}
}

func TestURLAndSocketIsolation(t *testing.T) {
	s := testStore(t)
	p := []byte(`{"event_id":"e1","note":"see http://192.168.1.1:8080/x and 10.1.2.3:22 open"}`)
	b, issued, _ := buildOne(t, s, testPolicy(), p)
	raw, _ := json.Marshal(b)
	ser := string(b.EventsJSON)
	if !strings.Contains(ser, ":8080") || !strings.Contains(ser, ":22") {
		t.Fatalf("ports must survive beside tokens: %s", ser)
	}
	if hits, _ := ScanOutboundLeaks(raw, issued, nil); len(hits) != 0 {
		t.Fatalf("leaks: %v", hits)
	}
	var ev []map[string]any
	if err := json.Unmarshal(b.EventsJSON, &ev); err != nil {
		t.Fatal(err)
	}
	note, _ := ev[0]["note"].(string)
	if !strings.Contains(note, "http://") || strings.Contains(note, "192.168.1.1") {
		t.Fatalf("URL host must be tokenized, scheme/path kept: %q", note)
	}
}

func TestHostileInputRoundTripsUnmangled(t *testing.T) {
	s := testStore(t)
	hostile := "Welcome to \"printer\" <script>alert(1)</script> \x00\x07 nulls"
	p := []byte(`{"event_id":"e1","host":{"hostname":"printer"},"banner":` + qjson(hostile) + `}`)
	b, issued, _ := buildOne(t, s, testPolicy(), p)
	ser, _ := json.Marshal(b)
	if hits, _ := ScanOutboundLeaks(ser, issued, nil); len(hits) != 0 {
		t.Fatalf("leaks: %v", hits)
	}
	restored := Restore(string(ser), issued)
	// Script and quotes pass through; only control chars go. (Go's JSON
	// encoding escapes < > &, so match encoding-independently.)
	if !strings.Contains(restored, `script`) || !strings.Contains(restored, `alert(1)`) {
		t.Fatal("hostile markup must survive unmangled")
	}
	if strings.Contains(restored, "\x00") || strings.Contains(restored, "\x07") {
		t.Fatal("control chars must be stripped")
	}
}

func qjson(s string) string {
	q, _ := json.Marshal(s)
	return string(q)
}

func TestTruncationRuneBound(t *testing.T) {
	s := testStore(t)
	long := strings.Repeat("é", 300) + " 192.168.9.9 tail"
	p := []byte(`{"event_id":"e1","banner":` + qjson(long) + `}`)
	b, issued, _ := buildOne(t, s, testPolicy(), p)
	var ev []map[string]any
	if err := json.Unmarshal(b.EventsJSON, &ev); err != nil {
		t.Fatal(err)
	}
	banner, _ := ev[0]["banner"].(string)
	if n := len([]rune(banner)); n > 256 {
		t.Fatalf("capped field is %d runes, want <= 256", n)
	}
	if !strings.HasSuffix(banner, "…[truncated]") {
		t.Fatalf("truncation must be marked: %q", banner[len(banner)-20:])
	}
	ser, _ := json.Marshal(b)
	if hits, _ := ScanOutboundLeaks(ser, issued, nil); len(hits) != 0 {
		t.Fatalf("leaks after truncation: %v", hits)
	}
}

func TestRedactionOffPassthrough(t *testing.T) {
	s := testStore(t)
	b, issued, _ := buildOne(t, s, Policy{Enabled: false}, payload(t, richHost, ""))
	if len(issued) != 0 {
		t.Fatalf("disabled redaction must issue nothing, got %v", issued)
	}
	if !strings.Contains(string(b.EventsJSON), "192.168.0.5") {
		t.Fatal("disabled redaction must pass raw values through")
	}
}

func TestBatchRestore(t *testing.T) {
	s := testStore(t)
	_, issued, _ := buildOne(t, s, testPolicy(), payload(t, richHost, ""))
	var toks []string
	for tok := range issued {
		toks = append(toks, tok)
	}
	toks = append(toks, "IP_999") // unknown: absent, not an error
	got, err := s.RestorePseudonyms(context.Background(), toks)
	if err != nil {
		t.Fatal(err)
	}
	for tok, real := range issued {
		if got[tok] == "" {
			t.Fatalf("token %s unresolved (real %q)", tok, real)
		}
	}
	if _, ok := got["IP_999"]; ok {
		t.Fatal("unknown token must be absent")
	}
	if empty, err := s.RestorePseudonyms(context.Background(), nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty batch must resolve empty: %v %v", empty, err)
	}
}

func TestAnchorTokenized(t *testing.T) {
	s := testStore(t)
	b, _, _ := buildOne(t, s, testPolicy(), payload(t, richHost, ""))
	if strings.Contains(b.Anchor, "Printer") || strings.Contains(b.Anchor, "printer") {
		t.Fatalf("anchor leaks hostname: %q", b.Anchor)
	}
	// The category prefix survives as signal ("host:HOST_n"); the value is a token.
	if b.Anchor != "host:HOST_1" && !strings.HasPrefix(b.Anchor, "host:HOST_") {
		t.Fatalf("anchor must carry the host token, got %q", b.Anchor)
	}
}

func TestGoldenBundle(t *testing.T) {
	s := testStore(t)
	p := []byte(`{"event_id":"e1","event_key":"k","event_type":"host.changed","host":{"ip":"10.1.2.3","hostname":"db-primary","mac":"02:42:AC:11:00:02"},"changes":[{"field":"ports","before":"","after":"5432/open"}]}`)
	b, _, _, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "grp-golden", Anchor: "host:db-primary", Target: "10.1.0.0/16",
		Baseline: "high",
		Events:   []InputEvent{{EventID: "e1", EventType: "host.changed", Payload: p}},
	}, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ser, _ := json.MarshalIndent(b, "", "  ")
	const golden = "testdata/bundle.golden"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, append(ser, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (UPDATE_GOLDEN=1 to create): %v", err)
	}
	if string(ser)+"\n" != string(want) {
		t.Fatalf("golden mismatch:\n%s", diffLines(string(want), string(ser)+"\n"))
	}
}

func diffLines(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out strings.Builder
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			fmt.Fprintf(&out, "- %s\n+ %s\n", x, y)
		}
	}
	return out.String()
}

func BenchmarkBuildBundle(b *testing.B) {
	dir := b.TempDir()
	st, err := store.Open(dir + "/bench.db")
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	var payloads [][]byte
	for i := 0; i < 10; i++ {
		payloads = append(payloads, []byte(fmt.Sprintf(`{"event_id":"e%d","host":{"ip":"192.168.0.%d","hostname":"host-%d","mac":"AA:BB:CC:DD:EE:%02X"},"evidence":"banner from 10.1.1.%d port 22 open, fe80::%d up","ports":[{"number":22,"protocol":"tcp","state":"open"}]}`, i, i+1, i, i, i+1, i)))
	}
	var events []InputEvent
	for i, p := range payloads {
		events = append(events, InputEvent{EventID: fmt.Sprintf("e%d", i), EventType: "host.changed", Payload: p})
	}
	in := BundleInput{GroupID: "g", Anchor: "host:host-0", Target: "192.168.0.0/24", Baseline: "medium", Events: events}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := BuildBundle(context.Background(), st, in, testPolicy()); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzTokenizeRestore(f *testing.F) {
	f.Add("see printer-2 at AA:BB:CC:DD:EE:FF via http://192.168.1.1:8080/x")
	f.Add("fe80::1%eth0 and 2001:db8::/32 plus aabb.ccdd.eeff and deadbeefcafe")
	f.Add("Linux 5.15.0-generic build 1.2.3 no addresses here")
	// H4 rows and the dotted-port hole as seeds: the oracle must hold
	// on every row the fixtures pin, in both directions.
	f.Add("moved to 192.168.1.5.")
	f.Add("see http://192.168.1.5/admin")
	f.Add("range 10.0.0.5-10.0.0.9 done")
	f.Add("moved to 192.168.001.001.")
	f.Add("dev_10.0.0.5 here")
	f.Add("got fe80::1x.")
	f.Add("netstat 192.168.1.5.443 ok")
	f.Add("firmware 1.2.3.4.5 ok")
	f.Add("550e8400-e29b-41d4-a716-446655440000")
	f.Add("0.0.00.0")
	f.Fuzz(func(t *testing.T, text string) {
		// Fake token assignment through the REAL replaceSpans path: the
		// fuzz harness cannot drift from production by construction.
		toks := TokenMap{}
		counters := map[string]int{}
		prefix := map[string]string{catIP: "IP", catMAC: "MAC", catHost: "HOST", catNet: "NET"}
		tokenized := replaceSpans(text, func(cat, norm string) string {
			counters[cat]++
			tok := fmt.Sprintf("%s_%d", prefix[cat], counters[cat])
			toks[tok] = norm
			return tok
		})
		// Fail-closed contract: no post-hoc walk over the OUTPUT can
		// demand more tokenization. The shared walk is position-sensitive
		// by design (guard context): a span it skips in the input
		// (e.g. "::000000000000" swallowed into an invalid zone-glued
		// run) may match once an emitted token precedes it, and a span
		// embedding an issued token ("::0%IP_1") is untouchable —
		// re-tokenizing it would corrupt the token. Either way the
		// identical walk inside ScanOutboundLeaks flags the leftover,
		// so the disagreement can only reject the bundle upstream,
		// never leak through it. The fuzz pins that direction: every
		// hit must be leg-explainable, and clean output must restore.
		hits, err := ScanOutboundLeaks(mustJSON(tokenized), toks, nil)
		if err != nil {
			t.Fatalf("leak scan error: %v", err)
		}
		// Soundness: every hit must be explainable by one leg — a
		// validated leftover, an eager shape the validator rejects, or
		// a surviving issued value. Unexplained hits mean a blind spot
		// the legs don't cover.
		if len(hits) != 0 && !leakValidated(tokenized) && !leakEager(tokenized) && !leakValue(tokenized, toks) {
			t.Fatalf("unexplained leak hit: hits=%v in %q", hits, tokenized)
		}
		if len(hits) == 0 {
			oracleNoLiteralAddress(t, tokenized)
		}
		if err := ValidateTokens(tokenized, toks); err != nil {
			t.Fatalf("own output invalid: %v", err)
		}
		// Every emitted token restores: rebuild the expectation by
		// re-extracting whole tokens from the output itself.
		for _, tok := range tokenRe.FindAllString(tokenized, -1) {
			if _, ok := toks[tok]; !ok {
				t.Fatalf("unissued token in output: %s", tok)
			}
		}
		restored := Restore(tokenized, toks)
		if tokenRe.MatchString(restored) {
			t.Fatalf("tokens remain after restore: %q", restored)
		}
	})
}

func mustJSON(s string) []byte {
	b, _ := json.Marshal(map[string]string{"s": s})
	return b
}

// stripLeadingZeros folds one quad for the fuzz oracle: "001" reads as
// "1", so a validator-rejected form still counts as an address. A
// "/bits" suffix is folded the same way and reattached.
func stripLeadingZeros(quad string) string {
	suffix := ""
	if i := strings.Index(quad, "/"); i >= 0 {
		suffix, quad = quad[i:], quad[:i]
		suffix = strings.TrimLeft(suffix[1:], "0")
		if suffix == "" {
			suffix = "0"
		}
		suffix = "/" + suffix
	}
	parts := strings.Split(quad, ".")
	for i, p := range parts {
		// Empty parts (leading/trailing dots) stay empty so they can
		// never parse: folding them to "0" would manufacture an
		// address from dot fragments (".0.001." must not read as
		// "0.0.1.0").
		if p == "" {
			continue
		}
		if s := strings.TrimLeft(p, "0"); s != "" {
			parts[i] = s
		} else {
			parts[i] = "0"
		}
	}
	return strings.Join(parts, ".") + suffix
}

var (
	// Independent shapes for the fuzz oracle: deliberately not the
	// redactor's regexes, so a shared typo cannot blind both the legs
	// and the check. Token spans are blanked before scanning.
	oracleTokenRe = regexp.MustCompile(`\b(?:IP|MAC|HOST|NET)_[0-9]+\b`)
	oracleHexRe   = regexp.MustCompile(`[0-9A-Fa-f]+`)
	oracleRunRe   = regexp.MustCompile(`[0-9./]{7,}`)
	oracleV6Re    = regexp.MustCompile(`[0-9A-Fa-f:.]{7,}`)
)

// oracleNoLiteralAddress fails when a scan-clean output still contains
// an identifying address the legs missed: every digit/dot/slash run of
// 7+ chars is windowed 7..15 and parsed (quads via ParseAddr, prefixes
// via ParsePrefix, both after per-octet zero stripping), and every 12+
// hex run is flagged MAC-shaped. The unspecified address is exempt: it
// carries no identity.
//
// False-positive policy: this runs in the fuzzer only, where a flag is
// a triage case, never a production outage. Token spans are blanked so
// a token number glued to version dots ("IP_1.5") cannot read as an
// octet; everything else that parses is fail-closed by design.
func oracleNoLiteralAddress(t *testing.T, tokenized string) {
	t.Helper()
	scrubbed := oracleTokenRe.ReplaceAllString(tokenized, " ")
	for _, run := range oracleRunRe.FindAllString(scrubbed, -1) {
		for i := 0; i+7 <= len(run); i++ {
			for j := i + 7; j <= len(run) && j <= i+15; j++ {
				sub := run[i:j]
				// Octets past 3 digits are malformed beyond trivial
				// zero-padding: no parser reads them, and the repair
				// (which digit to drop) is ambiguous. The legs cover
				// ≤3-digit forms; longer runs are noise, not identity.
				// The check runs on the address core: a "/bits" suffix
				// must not count toward octet length, or the prefix
				// leg below would be dead code.
				core := sub
				if k := strings.Index(core, "/"); k >= 0 {
					core = core[:k]
				}
				skip := false
				for _, part := range strings.Split(core, ".") {
					if len(part) > 3 {
						skip = true
						break
					}
				}
				if skip {
					continue
				}
				sub = stripLeadingZeros(sub)
				if addr, err := netip.ParseAddr(sub); err == nil && !addr.IsUnspecified() {
					t.Fatalf("scan-clean output parses as an address: %q in %q", run[i:j], tokenized)
				}
				if pfx, err := netip.ParsePrefix(sub); err == nil && !pfx.Addr().IsUnspecified() {
					t.Fatalf("scan-clean output parses as a prefix: %q in %q", run[i:j], tokenized)
				}
			}
		}
	}
	// IPv6: the quad pass above cannot see colon forms, so the leak
	// scanner's v6 legs would otherwise go unchecked. Windows are wider
	// (longest textual form is 45 chars); only colon runs are examined,
	// since bare quads belong to the pass above. Zones break the run at
	// "%", matching what the tokenizer can consume.
	for _, run := range oracleV6Re.FindAllString(scrubbed, -1) {
		if !strings.Contains(run, ":") {
			continue
		}
		// Like the zone leg, require a decimal digit: digit-less
		// colon runs are code-like prose ("std::fmt", "AAAAA::A"),
		// not addresses. A mnemonic address without a single digit
		// ("cafe::cafe") is vanishingly rare next to C++ in banners.
		if !strings.ContainsAny(run, "0123456789") {
			continue
		}
		for i := 0; i+7 <= len(run); i++ {
			for j := i + 7; j <= len(run) && j <= i+45; j++ {
				if addr, err := netip.ParseAddr(run[i:j]); err == nil && !addr.IsUnspecified() {
					t.Fatalf("scan-clean output parses as an IPv6 address: %q in %q", run[i:j], tokenized)
				}
			}
		}
	}
	// MAC-shaped runs must be exactly 12 hex digits: longer runs are
	// the deliberate shredding fragments (the tokenizer keeps them
	// whole, the scan agrees), and all-zeros runs carry no identity.
	// A surviving exact-12 run is guard-disputed — the tokenizer takes
	// unguarded bare-12 aggressively, and the eager leg no longer
	// exempts trailing colons — so reaching here means a leg regressed,
	// and the tripwire fires.
	for _, m := range oracleHexRe.FindAllString(scrubbed, -1) {
		if len(m) != 12 || strings.TrimLeft(m, "0") == "" {
			continue
		}
		t.Fatalf("scan-clean output holds a MAC-shaped run: %q in %q", m, tokenized)
	}
}

// bundleBanner builds a one-event bundle from a banner string with no
// anchor or target, so numbering restarts at IP_1/HOST_1 every call.
func bundleBanner(t *testing.T, banner string) ([]byte, TokenMap, TokenMap) {
	t.Helper()
	s := testStore(t)
	p := []byte(`{"event_id":"e1","banner":` + qjson(banner) + `}`)
	b, issued, display, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "g", Baseline: "low",
		Events: []InputEvent{{EventID: "e1", EventType: "host.appeared", Payload: p}},
	}, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ser, _ := json.Marshal(b)
	return ser, issued, display
}

func mustClean(t *testing.T, ser []byte, issued TokenMap) {
	t.Helper()
	if hits, err := ScanOutboundLeaks(ser, issued, nil); err != nil || len(hits) != 0 {
		t.Fatalf("leaks: %v %v", hits, err)
	}
}

// Pro event IDs are dashed UUIDs (sha256-derived, opaque). They ride the
// bundle envelope, so the scan must know them: without the skip-set every
// real bundle fails closed on its own IDs and the provider never fires.
func TestUUIDEventIDsAreNotLeaks(t *testing.T) {
	s := testStore(t)
	p := []byte(`{"event_id":"550e8400-e29b-41d4-a716-446655440000","host":{"ip":"10.9.9.9","hostname":"db-1"}}`)
	b, issued, _, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "g", Baseline: "low", Anchor: "host:db-1",
		Events: []InputEvent{{EventID: "550e8400-e29b-41d4-a716-446655440000", EventType: "host.appeared", Payload: p}},
	}, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ser, _ := json.Marshal(b)
	if hits, err := ScanOutboundLeaks(ser, issued, b.EventIDs); err != nil || len(hits) != 0 {
		t.Fatalf("own event IDs must not fail the scan: %v %v", hits, err)
	}
	// The ID must survive verbatim in the bundle: tokenizing its tail
	// as MAC_n would corrupt the envelope and mint a phantom row.
	if !strings.Contains(string(ser), "550e8400-e29b-41d4-a716-446655440000") {
		t.Fatalf("event ID mangled in bundle: %s", ser)
	}
	for tok := range issued {
		if strings.HasPrefix(tok, "MAC_") {
			t.Fatalf("UUID tail must not mint a MAC token: %v", issued)
		}
	}
}

// A UUID-shaped string that is NOT a known event ID is still scanned:
// the skip-set is exact membership, not a shape exemption.
func TestUnknownUUIDShapeStillScanned(t *testing.T) {
	other := "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	ser := mustJSON(other)
	if hits, err := ScanOutboundLeaks(ser, TokenMap{}, []string{"550e8400-e29b-41d4-a716-446655440000"}); err != nil || len(hits) == 0 {
		t.Fatalf("unlisted UUID shape must still hit the scan: %v %v", hits, err)
	}
}

// The anchor is bridge-derived from untrusted payload fields: a hostile
// hostname literally shaped like a token must fail the bundle closed,
// not ride the anchor past the spoof verdict.
func TestTokenShapedAnchorFailsClosed(t *testing.T) {
	s := testStore(t)
	p := []byte(`{"event_id":"e1","host":{"ip":"10.9.9.9"}}`)
	_, _, _, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "g", Baseline: "low", Anchor: "host:IP_1",
		Events: []InputEvent{{EventID: "e1", EventType: "host.appeared", Payload: p}},
	}, testPolicy())
	if err == nil || !strings.Contains(err.Error(), "token-shaped") {
		t.Fatalf("token-shaped anchor must fail closed, got: %v", err)
	}
}

// H4 glue rows: trailing punctuation, paths and ranges tokenize with the
// glue left literal; versions stay whole and clean.
func TestGluePositionsTokenize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // required in output
		gone []string // must be absent from output
	}{
		{"sentence dot", "moved to 192.168.1.5.", []string{"IP_1."}, []string{"192.168.1.5"}},
		{"url path", "see http://192.168.1.5/admin", []string{"http://IP_1/admin"}, []string{"192.168.1.5"}},
		{"range", "range 10.0.0.5-10.0.0.9 done", []string{"IP_1-IP_2"}, []string{"10.0.0.5", "10.0.0.9"}},
		{"v6 sentence dot", "watch fe80::1.", []string{"IP_1."}, []string{"fe80::1"}},
		// Dotted ports tokenize like socket ports; versions tokenize the
		// same way and restore exactly — no literal quad survives.
		{"dotted port", "netstat 192.168.1.5.443 ok", []string{"IP_1.443"}, []string{"192.168.1.5"}},
		{"version tokenized", "firmware 1.2.3.4.5 ok", []string{"IP_1.5"}, []string{"firmware 1.2.3.4.5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ser, issued, _ := bundleBanner(t, tc.in)
			mustClean(t, ser, issued)
			for _, w := range tc.want {
				if !strings.Contains(string(ser), w) {
					t.Fatalf("want %q in %s", w, ser)
				}
			}
			for _, g := range tc.gone {
				if strings.Contains(string(ser), g) {
					t.Fatalf("raw value %q survived in %s", g, ser)
				}
			}
		})
	}
}

// H4 must-block rows: shapes the validator rejects but a human would
// read as an address fail the scan instead of reaching the provider.
func TestValidatorBlindSpotsBlocked(t *testing.T) {
	for _, in := range []string{
		"moved to 192.168.001.001.", // leading zeros
		"dev_10.0.0.5 here",         // underscore-glued
		"ip10.0.0.5 seen",           // letter-glued
		"got fe80::1x.",             // letter-glued v6 tail
		"xdeadbeefcafe: tag",        // letter-glued MAC with a bare trailing colon
		"xdeadbeefcafe:",            // same, at end of string
	} {
		t.Run(in, func(t *testing.T) {
			ser, issued, _ := bundleBanner(t, in)
			if hits, _ := ScanOutboundLeaks(ser, issued, nil); len(hits) == 0 {
				t.Fatalf("validator blind spot passed clean: %s", ser)
			}
		})
	}
}

// H4 keys: an address-shaped object key tokenizes like a value, so the
// key-walking scan leg never sees it.
func TestAddressKeyTokenized(t *testing.T) {
	s := testStore(t)
	p := []byte(`{"event_id":"e1","hosts":{"192.168.1.1":"open"}}`)
	b, issued, _, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "g", Baseline: "low",
		Events: []InputEvent{{EventID: "e1", EventType: "host.appeared", Payload: p}},
	}, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ser, _ := json.Marshal(b)
	mustClean(t, ser, issued)
	if strings.Contains(string(ser), "192.168.1.1") {
		t.Fatalf("address key survived: %s", ser)
	}
}

// H5: a non-ASCII hostname redacts everywhere, including free text, and
// restores with its exact display spelling.
func TestCyrillicHostnameRoundTrip(t *testing.T) {
	s := testStore(t)
	p := []byte(`{"event_id":"e1","hostname":"Олексій-ПК","banner":"saw Олексій-ПК join"}`)
	b, issued, display, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "g", Baseline: "low",
		Events: []InputEvent{{EventID: "e1", EventType: "host.appeared", Payload: p}},
	}, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ser, _ := json.Marshal(b)
	mustClean(t, ser, issued)
	var tok string
	for k := range issued {
		tok = k
	}
	if len(issued) != 1 || display[tok] != "Олексій-ПК" {
		t.Fatalf("display must keep original spelling, got %v", display)
	}
	if got := Restore(string(ser), display); !strings.Contains(got, "Олексій-ПК") {
		t.Fatalf("display restore lost original spelling: %q", got)
	}
}

// H5: short names still redact everywhere (no free-text length floor —
// the floor would leak); prose garbling is the documented cost.
func TestShortHostnameRedacts(t *testing.T) {
	s := testStore(t)
	p := []byte(`{"event_id":"e1","hostname":"tv","banner":"bought a new tv"}`)
	b, issued2, _, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "g", Baseline: "low",
		Events: []InputEvent{{EventID: "e1", EventType: "host.appeared", Payload: p}},
	}, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	mustClean(t, raw, issued2)
	if strings.Contains(string(raw), "tv") {
		t.Fatalf("short hostname survived: %s", raw)
	}
}

// H5: an over-long hostname fails the bundle closed, never skipped.
func TestOverlongHostnameFailsClosed(t *testing.T) {
	s := testStore(t)
	p := []byte(`{"event_id":"e1","hostname":` + qjson(strings.Repeat("h", 300)) + `}`)
	_, _, _, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "g", Baseline: "low",
		Events: []InputEvent{{EventID: "e1", EventType: "host.appeared", Payload: p}},
	}, testPolicy())
	if err == nil {
		t.Fatal("over-long hostname must fail the bundle closed")
	}
}

// H6: token-shaped input fails closed — a model echo would otherwise
// restore another host's address into this briefing.
func TestTokenSpoofFailsClosed(t *testing.T) {
	s := testStore(t)
	p := []byte(`{"event_id":"e1","banner":"see IP_1 here","host":{"ip":"10.0.0.9"}}`)
	_, _, _, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "g", Baseline: "low",
		Events: []InputEvent{{EventID: "e1", EventType: "host.appeared", Payload: p}},
	}, testPolicy())
	if err == nil {
		t.Fatal("token-shaped input must fail the bundle closed")
	}
}

// H6: Restore is single-pass — a restored value that itself contains a
// token shape is never re-substituted.
func TestRestoreNoResubstitution(t *testing.T) {
	issued := TokenMap{"HOST_1": "x IP_1 y", "IP_1": "10.0.0.1"}
	if got := Restore("see HOST_1!", issued); got != "see x IP_1 y!" {
		t.Fatalf("re-substituted: %q", got)
	}
}

// H6: display restore shows first-seen spelling, not the folded form.
func TestDisplayRestoresOriginalCase(t *testing.T) {
	s := testStore(t)
	p := []byte(`{"event_id":"e1","hostname":"Printer-2","host":{"ip":"192.168.0.5","mac":"AA:BB:CC:DD:EE:FF"}}`)
	b, issued, display, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "g", Baseline: "low",
		Events: []InputEvent{{EventID: "e1", EventType: "host.appeared", Payload: p}},
	}, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ser, _ := json.Marshal(b)
	mustClean(t, ser, issued)
	var hostTok, macTok string
	for tok, norm := range issued {
		switch norm {
		case "printer-2":
			hostTok = tok
		case "aa:bb:cc:dd:ee:ff":
			macTok = tok
		}
	}
	if display[hostTok] != "Printer-2" || display[macTok] != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("display must keep first-seen spelling, got %v", display)
	}
	restored := Restore(string(ser), display)
	if !strings.Contains(restored, "Printer-2") || !strings.Contains(restored, "AA:BB:CC:DD:EE:FF") {
		t.Fatalf("display restore lost spelling: %q", restored)
	}
}
