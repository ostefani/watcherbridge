package redactor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

func buildOne(t *testing.T, s *store.Store, p Policy, payload []byte) (*RedactedBundle, TokenMap) {
	t.Helper()
	b, issued, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "grp-1", Anchor: "host:Printer-2", Target: "192.168.0.0/24",
		Baseline: "low",
		Events:   []InputEvent{{EventID: "e1", EventType: "host.changed", Payload: payload}},
	}, p)
	if err != nil {
		t.Fatal(err)
	}
	return b, issued
}

func TestRoundTrip(t *testing.T) {
	s := testStore(t)
	b, issued := buildOne(t, s, testPolicy(), payload(t, richHost, ""))
	if len(issued) == 0 {
		t.Fatal("expected issued tokens")
	}
	ser, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if hits, err := ScanOutboundLeaks(ser); err != nil || len(hits) != 0 {
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
	b, issued := buildOne(t, s, testPolicy(), p)
	ser, _ := json.Marshal(b)
	if hits, _ := ScanOutboundLeaks(ser); len(hits) != 0 {
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
	b, issued, err := BuildBundle(context.Background(), s, BundleInput{
		GroupID: "g", Baseline: "low",
		Events: []InputEvent{{EventID: "e", EventType: "host.appeared", Payload: p}},
	}, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ser, _ := json.Marshal(b)
	if hits, _ := ScanOutboundLeaks(ser); len(hits) != 0 {
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
	if hits, _ := ScanOutboundLeaks(mustJSON(out)); len(hits) == 0 {
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
	b, _ := buildOne(t, s, testPolicy(), p)
	raw, _ := json.Marshal(b)
	ser := string(b.EventsJSON)
	if !strings.Contains(ser, ":8080") || !strings.Contains(ser, ":22") {
		t.Fatalf("ports must survive beside tokens: %s", ser)
	}
	if hits, _ := ScanOutboundLeaks(raw); len(hits) != 0 {
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
	b, issued := buildOne(t, s, testPolicy(), p)
	ser, _ := json.Marshal(b)
	if hits, _ := ScanOutboundLeaks(ser); len(hits) != 0 {
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
	b, issued := buildOne(t, s, testPolicy(), p)
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
	if hits, _ := ScanOutboundLeaks(ser); len(hits) != 0 {
		t.Fatalf("leaks after truncation: %v", hits)
	}
	_ = issued
}

func TestRedactionOffPassthrough(t *testing.T) {
	s := testStore(t)
	b, issued := buildOne(t, s, Policy{Enabled: false}, payload(t, richHost, ""))
	if len(issued) != 0 {
		t.Fatalf("disabled redaction must issue nothing, got %v", issued)
	}
	if !strings.Contains(string(b.EventsJSON), "192.168.0.5") {
		t.Fatal("disabled redaction must pass raw values through")
	}
}

func TestBatchRestore(t *testing.T) {
	s := testStore(t)
	_, issued := buildOne(t, s, testPolicy(), payload(t, richHost, ""))
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
	b, _ := buildOne(t, s, testPolicy(), payload(t, richHost, ""))
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
	b, _, err := BuildBundle(context.Background(), s, BundleInput{
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
		if _, _, err := BuildBundle(context.Background(), st, in, testPolicy()); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzTokenizeRestore(f *testing.F) {
	f.Add("see printer-2 at AA:BB:CC:DD:EE:FF via http://192.168.1.1:8080/x")
	f.Add("fe80::1%eth0 and 2001:db8::/32 plus aabb.ccdd.eeff and deadbeefcafe")
	f.Add("Linux 5.15.0-generic build 1.2.3 no addresses here")
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
		// Fail-closed contract: tokenization must replace every span
		// it safely can. Spans it deliberately keeps literal (a token
		// there would glue to a word char and become unrestorable)
		// stay raw in the output, and the leak scan must flag exactly
		// those — that flag is what rejects the bundle upstream.
		var kept int
		visitSpans(tokenized, func(glo, ghi int, _ discovered, emit bool) {
			span := tokenized[glo:ghi]
			if !emit {
				kept++
				return
			}
			// A surviving span that already embeds an issued token
			// (e.g. "::0%IP_1": the zone re-parse absorbs the token)
			// is partially tokenized and untouchable — re-tokenizing
			// it would corrupt the token. The leak scan still flags
			// it, so the bundle is rejected fail-closed upstream.
			for _, tok := range tokenRe.FindAllString(span, -1) {
				if _, ok := toks[tok]; ok {
					kept++
					return
				}
			}
			t.Fatalf("emittable span survived tokenize: %q in %q", span, tokenized)
		})
		hits, err := ScanOutboundLeaks(mustJSON(tokenized))
		if err != nil {
			t.Fatalf("leak scan error: %v", err)
		}
		if (len(hits) != 0) != (kept != 0) {
			t.Fatalf("leak scan disagrees with tokenize: hits=%v kept=%d in %q", hits, kept, tokenized)
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
