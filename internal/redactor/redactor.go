// Package redactor builds model-safe bundles: every IP, MAC, hostname,
// and CIDR in every string of a group's events is replaced by a map-owned
// counter token, with a fail-closed leak scan before anything leaves the
// machine (Designv5 §6). Library-only: the Step 11 worker wires it to the
// provider. Hostnames are tokenized only when already known as fields;
// addresses are regex-discovered, including inside URLs and sockets.
package redactor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"github.com/ostefani/watcherbridge/internal/store"
)

// Categories in the pseudonym map.
const (
	catIP   = "ip"
	catMAC  = "mac"
	catHost = "hostname"
	catNet  = "net"
)

// TokenMap maps token -> real (normalized) value for one bundle. It is the
// issuance record Step 11 checks model output against (§6.3).
type TokenMap map[string]string

// RedactedBundle is the model-bound view of a group. The JSON tags are
// the wire contract: Step 11 sends this exact shape to the provider, so
// the field names are fixed, not Go defaults.
type RedactedBundle struct {
	GroupID    string          `json:"group_id"`
	EventIDs   []string        `json:"event_ids"`
	Anchor     string          `json:"anchor"`
	EventsJSON json.RawMessage `json:"events"`
	Baseline   string          `json:"baseline"`
}

// Policy carries the redactor knobs.
type Policy struct {
	Enabled       bool // REDACTION; off passes raw through and skips the leak scan
	MaxFieldChars int  // rune cap per string, truncation marked
}

// MaxBundleBytes caps the serialized bundle the model receives: with
// field capping the tokenized size is nonlinear in the raw payload, so
// the group byte budget cannot bound the call. Over-cap bundles fall
// back instead of becoming billed multi-megabyte requests.
const MaxBundleBytes = 256 << 10

// InputEvent is one stored event to bundle.
type InputEvent struct {
	EventID   string
	EventType string
	Payload   []byte
}

// BundleInput is everything the builder needs.
type BundleInput struct {
	GroupID  string
	Anchor   string
	Target   string
	Baseline string
	Events   []InputEvent
	// SeedValues are retained identity spellings discovered outside the
	// summarized prose. They are tokenized wherever they recur in leaves.
	SeedValues []string
}

var (
	macColonRe = `(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}`
	macDashRe  = `(?:[0-9A-Fa-f]{2}-){5}[0-9A-Fa-f]{2}`
	macDotRe   = `(?:[0-9A-Fa-f]{4}\.){2}[0-9A-Fa-f]{4}`
	macBareRe  = `[0-9A-Fa-f]{12}`
	ipv4Re     = `\d{1,3}(?:\.\d{1,3}){3}`
	ipv4CIDRRe = `\d{1,3}(?:\.\d{1,3}){3}/\d{1,2}`
	ipv6FullRe = `(?:[0-9A-Fa-f]{1,4}:){7}[0-9A-Fa-f]{1,4}`
	ipv6CompRe = `(?:[0-9A-Fa-f]{0,4}:){2,}[0-9A-Fa-f:.]*(?:%[\w.-]+)?`
)

// addrRe finds address candidates. The leading guard (start or non-word
// char) keeps matches from starting mid-token — so `:` and `/` guards stay
// allowed, and `mac:…`, `ip:…`, `http://…` all match. The address itself is
// group 1. At the same start Go prefers the earliest alternative, so
// longer forms come before their prefixes: otherwise a MAC alternative
// shreds a longer valid IPv6 and the leftover fragment tokenizes glued to
// word characters, which restore can no longer match.
var addrRe = regexp.MustCompile(`(?:^|\W)(` + strings.Join([]string{
	ipv4CIDRRe, ipv6FullRe, ipv6CompRe, macColonRe, macDashRe, macDotRe, macBareRe, ipv4Re,
}, "|") + `)`)

// isWordByte reports ASCII word chars. Non-ASCII bytes (UTF-8 text) never
// count: tokens are ASCII, and a token beside multibyte text still has a
// clean boundary for the restore regex.
func isWordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return b == '_'
}

// trailBad reports a match glued to a longer token (a 12-hex fingerprint
// fragment, a longer version string). ':' stays allowed so socket ports
// survive (192.168.1.1:8080 -> IP_1:8080). Dots, slashes and dashes are
// decided by trailGlueEmits: sentence-final dots, URL paths and ranges
// still tokenize with the glue left literal.
func trailBad(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return b == '_' || b == '.' || b == '%' || b == '-' || b == '/'
}

type discovered struct {
	category   string
	normalized string // canonical form used for map lookup
}

// classify validates a candidate span. ok=false means literal text.
func classify(span string) (d discovered, ok bool) {
	if isMACSpan(span) {
		return discovered{catMAC, canonicalMAC(span)}, true
	}
	if strings.Contains(span, "/") {
		if pfx, err := netip.ParsePrefix(span); err == nil {
			return discovered{catNet, pfx.Masked().String()}, true
		}
		return discovered{}, false
	}
	if addr, err := netip.ParseAddr(span); err == nil {
		return discovered{catIP, addr.String()}, true
	}
	if isBareMAC(span) {
		return discovered{catMAC, canonicalMAC(span)}, true
	}
	return discovered{}, false
}

func isMACSpan(s string) bool {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ':' || r == '-' || r == '.' })
	if len(parts) == 6 {
		for _, p := range parts {
			if len(p) != 2 || !isHex(p) {
				return false
			}
		}
		return true
	}
	if len(parts) == 3 {
		for _, p := range parts {
			if len(p) != 4 || !isHex(p) {
				return false
			}
		}
		return true
	}
	return false
}

func isBareMAC(s string) bool { return len(s) == 12 && isHex(s) }

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return len(s) > 0
}

// canonicalMAC folds colon/dash/dotted/bare forms to lower colon form, so
// one device owns one token whatever the source wrote.
func canonicalMAC(s string) string {
	var hex []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ':' || c == '-' || c == '.' {
			continue
		}
		if c >= 'A' && c <= 'F' {
			c += 'a' - 'A'
		}
		hex = append(hex, c)
	}
	var b strings.Builder
	for i, c := range hex {
		if i > 0 && i%2 == 0 {
			b.WriteByte(':')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// builder holds one BuildBundle pass.
type builder struct {
	store    *store.Store
	maxChars int
	// skip holds the group's known event IDs (Pro's opaque UUIDs). A
	// whole leaf exactly equal to one is envelope identity, not a leak:
	// it bypasses discovery and tokenization verbatim. The match is
	// whole-leaf only, so an ID can never launder an address glued
	// into a longer string.
	skip     map[string]bool
	pending  map[string][]string          // category -> normalized values, encounter order
	original map[string]map[string]string // category -> normalized -> first-seen original spelling
	seen     map[string]map[string]bool
	tokens   map[string]map[string]string // category -> normalized -> token
	hostRe   *regexp.Regexp               // known hostnames, longest first, case-insensitive
	hostTok  map[string]string            // lower hostname -> token
	issued   TokenMap                     // token -> normalized value: the leak-scan record
	display  TokenMap                     // token -> first-seen original spelling: the restore record
	overCap  bool                         // a redaction bound was breached: fail the bundle, never skip
	// tokenSpoof is set when untrusted input already looks like a token
	// (see tokenShaped): the bundle fails closed instead of minting a
	// collision the model could echo back as a real address.
	tokenSpoof bool
}

// Redaction bounds: hostnames cap length (DNS 253) and count (the regex
// alternation must stay compilable); total pending values cap memory and
// keep one malicious event from minting an unbounded token map. Breaching
// a bound fails the bundle closed (Step 11 falls back): silently skipping
// would leave the value literal and unissued, invisible to the leak scan.
const (
	maxHostnameLen   = 253
	maxHostnames     = 1024
	maxPendingValues = 8192
)

func (b *builder) pendingTotal() int {
	n := 0
	for _, vals := range b.pending {
		n += len(vals)
	}
	return n
}

func (b *builder) addPending(cat, normalized, original string) {
	if b.seen[cat] == nil {
		b.seen[cat] = map[string]bool{}
	}
	if b.seen[cat][normalized] {
		return
	}
	if cat == catHost && len(normalized) > maxHostnameLen {
		b.overCap = true
		return
	}
	if cat == catHost && len(b.pending[cat]) >= maxHostnames {
		b.overCap = true
		return
	}
	if b.pendingTotal() >= maxPendingValues {
		b.overCap = true
		return
	}
	b.seen[cat][normalized] = true
	b.pending[cat] = append(b.pending[cat], normalized)
	if b.original[cat] == nil {
		b.original[cat] = map[string]string{}
	}
	if _, ok := b.original[cat][normalized]; !ok {
		if original == "" {
			original = normalized
		}
		b.original[cat][normalized] = original
	}
}

// replaceSpans substitutes every validated span with resolve's token.
// resolve returns "" for unknown values, which stay literal. Spans the
// shared walk marks non-emittable stay literal too — the walk guarantees a
// token there would glue to a word char and be unmatchable by restore, so
// the leak scan judges the literal text as-is.
func replaceSpans(s string, resolve func(category, normalized string) string) string {
	var out strings.Builder
	last := 0
	visitSpans(s, func(glo, ghi int, d discovered, emit bool) {
		out.WriteString(s[last:glo])
		last = ghi
		if !emit {
			out.WriteString(s[glo:ghi])
			return
		}
		if tok := resolve(d.category, d.normalized); tok != "" {
			out.WriteString(tok)
			return
		}
		out.WriteString(s[glo:ghi])
	})
	out.WriteString(s[last:])
	return out.String()
}

// discoverSpans reports every validated span in s via the shared walk,
// with offsets so discovery can keep the first-seen original spelling.
// Discovery ignores the emit flag — a span tokenization keeps literal is
// still a real value that owns a token; the leak scan judges the literal.
func discoverSpans(s string, emit func(glo, ghi int, d discovered)) {
	visitSpans(s, func(glo, ghi int, d discovered, _ bool) { emit(glo, ghi, d) })
}

// trailGlueEmits reports whether a trailing glue char still allows the
// span to tokenize with the glue left literal: sentence-final dots
// ("moved to 192.168.1.5."), path slashes ("http://192.168.1.5/admin"),
// and range dashes ("10.0.0.5-10.0.0.9") — and dotted ports
// ("192.168.1.5.443"), which tokenize exactly like socket ports
// ("192.168.1.1:8080" becomes "IP_1:8080", dotted becomes "IP_1.443").
// Versions ("1.2.3.4.5") tokenize the same way and restore exactly, so
// no exemption is needed anywhere downstream. Alphanumerics, '_' and '%'
// still skip — the first two are longer-token fragments, and a '%' starts
// a zoned remainder the leak scan must judge (TestZoneGlueFailClosed).
func trailGlueEmits(rest string, ghi int) bool {
	switch rest[ghi] {
	case '.', '/', '-':
		return true
	}
	return false
}

// visitSpans walks s with the exact traversal tokenization uses: same
// matches, same trailing skips, same emit-guard. fn receives every
// validated span with absolute offsets and emit=true iff tokenization
// would replace it there. Discovery, replacement, and the leak scan all
// share this walk, so they can never disagree about a span.
func visitSpans(s string, fn func(glo, ghi int, d discovered, emit bool)) {
	var prev byte // last byte the output built so far ends with; 0 when empty
	rest, off := s, 0
	for len(rest) > 0 {
		m := addrRe.FindStringSubmatchIndex(rest)
		if m == nil {
			return
		}
		glo, ghi := m[2], m[3]
		span := rest[glo:ghi]
		d, ok := classify(span)
		if !ok {
			// The compressed-IPv6 alternative swallows a sentence-final
			// '.' into the span and then fails validation. Trim one
			// trailing dot and retry; the dot stays literal.
			if t := strings.TrimSuffix(span, "."); t != span {
				if d2, ok2 := classify(t); ok2 {
					d, ok, ghi = d2, true, glo+len(t)
				}
			}
		}
		if ok && ghi < len(rest) && trailBad(rest[ghi]) && !trailGlueEmits(rest, ghi) {
			prev = rest[ghi-1]
			rest, off = rest[ghi:], off+ghi
			continue
		}
		emit := ok && !(glo == 0 && isWordByte(prev))
		if ok {
			fn(off+glo, off+ghi, d, emit)
		}
		if emit {
			prev = '0' // tokens end in a digit
		} else if ghi > 0 {
			prev = rest[ghi-1]
		}
		rest, off = rest[ghi:], off+ghi
	}
}

// discoverString collects typed hostnames (hostname-bearing keys seed the
// map) and address spans from one leaf.
func (b *builder) discoverString(key, s string) {
	s = StripControls(s)
	if b.skip[s] {
		return
	}
	if key == "hostname" || key == "identity_aliases" || key == "aliases" {
		for _, h := range splitAliases(s) {
			orig := strings.TrimSpace(h)
			if h = strings.ToLower(orig); h != "" {
				b.addPending(catHost, h, orig)
			}
		}
	}
	discoverSpans(s, func(glo, ghi int, d discovered) {
		b.addPending(d.category, d.normalized, s[glo:ghi])
	})
}

// walkLeaves visits every string leaf of decoded JSON in deterministic
// order (sorted object keys, array order), with the leaf's field name.
func walkLeaves(v any, key string, fn func(key, s string)) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkLeaves(t[k], k, fn)
		}
	case []any:
		for _, e := range t {
			walkLeaves(e, key, fn)
		}
	case string:
		fn(key, t)
	}
}

func splitAliases(s string) []string {
	if strings.Contains(s, ",") {
		return strings.Split(s, ",")
	}
	return []string{s}
}

// providerAnchor strips the deployment scope from scoped group anchors
// ("sub:<scope>:<rest>", "addr:<scope>:<rest>"). The scope identifies
// the local deployment, not the observed device: it is dropped before
// discovery and serialization so it can never reach the provider —
// only the tokenized identity remainder is sent. Legacy anchors pass
// through unchanged.
func providerAnchor(anchor string) string {
	for _, class := range []string{"sub:", "addr:"} {
		if rest, ok := strings.CutPrefix(anchor, class); ok {
			if _, remainder, ok := strings.Cut(rest, ":"); ok {
				return class + remainder
			}
			return anchor
		}
	}
	return anchor
}

// discoverAnchor registers the anchor's identity value by its prefix.
// Scoped remainders ("sub:<ns>:<ref>", scope already stripped) type the
// reference by its namespace through the same cases as legacy anchors,
// so a scoped MAC reference registers exactly like "mac:…".
func (b *builder) discoverAnchor(anchor string) {
	kind, val, ok := strings.Cut(anchor, ":")
	if !ok {
		return
	}
	val = strings.TrimSpace(val)
	switch kind {
	case "sub", "addr":
		ns, ref, ok := strings.Cut(val, ":")
		if !ok {
			return
		}
		b.discoverAnchor(ns + ":" + strings.TrimSpace(ref))
	case "mac":
		b.addPending(catMAC, canonicalMAC(val), val)
	case "host":
		if h := strings.ToLower(val); h != "" {
			b.addPending(catHost, h, val)
		}
	case "ip":
		if addr, err := netip.ParseAddr(val); err == nil {
			b.addPending(catIP, addr.String(), val)
		}
	}
}

// discoverTarget registers a scan target: CIDR, IP, or bare hostname.
func (b *builder) discoverTarget(target string) {
	target = strings.TrimSpace(target)
	if target == "" {
		return
	}
	if pfx, err := netip.ParsePrefix(target); err == nil {
		b.addPending(catNet, pfx.Masked().String(), target)
		return
	}
	if addr, err := netip.ParseAddr(target); err == nil {
		b.addPending(catIP, addr.String(), target)
		return
	}
	if !strings.ContainsAny(target, " \t/") {
		b.addPending(catHost, strings.ToLower(target), target)
	}
}

// registerAll assigns tokens per category, preserving encounter order so
// numbering is deterministic for identical input, and compiles the
// hostname matcher longest-first.
func (b *builder) registerAll(ctx context.Context) error {
	b.tokens = map[string]map[string]string{}
	for _, cat := range []string{catIP, catMAC, catHost, catNet} {
		vals := b.pending[cat]
		if len(vals) == 0 {
			continue
		}
		// The store normalizes each input and keeps the first-seen
		// spelling as the display value: pass this bundle's first-seen
		// originals so restoration shows exact text, not the folded
		// form (Printer-UPS, not printer-ups).
		inputs := make([]string, 0, len(vals))
		for _, v := range vals {
			if orig := b.original[cat][v]; orig != "" {
				inputs = append(inputs, orig)
			} else {
				inputs = append(inputs, v)
			}
		}
		got, err := b.store.GetOrAssignPseudonyms(ctx, cat, inputs...)
		if err != nil {
			return err
		}
		m := map[string]string{}
		for i, v := range vals {
			tok := got[inputs[i]]
			m[v] = tok
			b.issued[tok] = v
			b.display[tok] = inputs[i]
		}
		b.tokens[cat] = m
	}
	hostnames := append([]string(nil), b.pending[catHost]...)
	sort.Slice(hostnames, func(i, j int) bool { return len(hostnames[i]) > len(hostnames[j]) })
	if len(hostnames) > 0 {
		alts := make([]string, 0, len(hostnames))
		for _, h := range hostnames {
			alts = append(alts, regexp.QuoteMeta(h))
		}
		// Rune-aware neighbours instead of \b (Go RE2 \b is ASCII-only,
		// so Cyrillic or punctuated names never matched). Guards are
		// consumed by the match; replaceHostnames re-emits them and only
		// substitutes group 1.
		re, err := regexp.Compile(`(?i)(?:^|[^\p{L}\p{N}_-])(` + strings.Join(alts, "|") + `)(?:$|[^\p{L}\p{N}_-])`)
		if err != nil {
			// Pathological alternation (enormous names): no regex, no
			// panic — the bundle fails closed in BuildBundle instead of
			// parking a group with no briefing.
			return fmt.Errorf("compile hostname pattern: %w", err)
		}
		b.hostRe = re
		b.hostTok = map[string]string{}
		for _, h := range hostnames {
			b.hostTok[h] = b.tokens[catHost][h]
		}
	}
	return nil
}

// replaceHostnames substitutes group 1 of each hostRe match, re-emitting
// the consumed guard characters verbatim.
func (b *builder) replaceHostnames(s string) string {
	var out strings.Builder
	rest := s
	for len(rest) > 0 {
		m := b.hostRe.FindStringSubmatchIndex(rest)
		if m == nil {
			out.WriteString(rest)
			break
		}
		glo, ghi := m[2], m[3]
		out.WriteString(rest[:m[0]])
		out.WriteString(rest[m[0]:glo])
		name := rest[glo:ghi]
		if tok := b.hostTok[strings.ToLower(name)]; tok != "" {
			out.WriteString(tok)
			b.issued[tok] = strings.ToLower(name)
		} else {
			out.WriteString(name)
		}
		out.WriteString(rest[ghi:m[1]])
		rest = rest[m[1]:]
	}
	return out.String()
}

// tokenizeString replaces known hostnames then addresses, and caps the
// result. A whole string exactly equal to a known event ID passes
// through verbatim first: it is envelope identity, not content. Anything
// else token-shaped is rejected before anything else: a banner literally
// naming "IP_1" would otherwise let a model echo restore another host's
// address into this briefing. Address spans missing from the map are left
// literal: the leak scan fails the bundle loudly instead of silently
// dropping them.
func (b *builder) tokenizeString(s string) string {
	s = StripControls(s)
	if b.skip[s] {
		return s
	}
	// Check the raw input: replacement below legitimately introduces
	// tokens, so a later check could not tell spoof from success.
	if tokenShaped(s) {
		b.tokenSpoof = true
		return s
	}
	if b.hostRe != nil {
		s = b.replaceHostnames(s)
	}
	return CapField(replaceSpans(s, func(cat, norm string) string {
		tok := b.tokens[cat][norm]
		if tok != "" {
			b.issued[tok] = norm
		}
		return tok
	}), b.maxChars)
}

// tokenizeValue rebuilds decoded JSON with every string leaf tokenized.
// Keys go through the same substitution: an address-shaped key must not
// ride the output while the values are clean (the leak scan judges keys
// too, so an untokenized key would fail the bundle closed).
func (b *builder) tokenizeValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[b.tokenizeString(k)] = b.tokenizeValue(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = b.tokenizeValue(e)
		}
		return out
	case string:
		return b.tokenizeString(t)
	}
	return v
}

// BuildBundle tokenizes a group's events into a RedactedBundle and returns
// the issued token map (token -> normalized value, the leak-scan record)
// and the display map (token -> first-seen original spelling, the restore
// record). Display values never leave the machine: only the bundle crosses
// the provider boundary. With Policy.Enabled=false it passes raw values
// through with empty maps (fail-open, operator-chosen, warned at startup
// by the Step 11 worker).
func BuildBundle(ctx context.Context, st *store.Store, in BundleInput, pol Policy) (*RedactedBundle, TokenMap, TokenMap, error) {
	issued := TokenMap{}
	display := TokenMap{}
	decode := func(id string, payload []byte) (any, error) {
		var v any
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber() // keep integers exact: float64 would mangle
		// integers past 2^53 when the bundle re-marshals for the model
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("decode event %s: %w", id, err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return nil, fmt.Errorf("decode event %s: trailing data", id)
		}
		return v, nil
	}
	if !pol.Enabled {
		events := make([]any, 0, len(in.Events))
		ids := make([]string, 0, len(in.Events))
		for _, e := range in.Events {
			v, err := decode(e.EventID, e.Payload)
			if err != nil {
				return nil, nil, nil, err
			}
			events = append(events, v)
			ids = append(ids, e.EventID)
		}
		raw, err := json.Marshal(events)
		if err != nil {
			return nil, nil, nil, err
		}
		return &RedactedBundle{GroupID: in.GroupID, EventIDs: ids, Anchor: in.Anchor, EventsJSON: raw, Baseline: in.Baseline}, issued, display, nil
	}
	maxChars := pol.MaxFieldChars
	if maxChars <= 0 {
		maxChars = 256
	}
	b := &builder{store: st, maxChars: maxChars, pending: map[string][]string{}, original: map[string]map[string]string{}, seen: map[string]map[string]bool{}, issued: TokenMap{}, display: TokenMap{}, skip: map[string]bool{}}
	for _, e := range in.Events {
		if e.EventID != "" {
			b.skip[e.EventID] = true
		}
	}
	decoded := make([]any, 0, len(in.Events))
	for _, e := range in.Events {
		v, err := decode(e.EventID, e.Payload)
		if err != nil {
			return nil, nil, nil, err
		}
		decoded = append(decoded, v)
	}
	for _, v := range decoded {
		walkLeaves(v, "", b.discoverString)
		b.discoverKeys(v)
	}
	for _, value := range in.SeedValues {
		if value != "" {
			b.addPending(catHost, strings.ToLower(value), value)
		}
	}
	// Anchor and target seed the map typed: the anchor carries the host
	// identity by construction ("mac:…", "host:…", "ip:…", or a scoped
	// remainder), and the target is usually the scan CIDR. The
	// deployment scope never seeds the map and never serializes.
	anchor := providerAnchor(in.Anchor)
	b.discoverAnchor(anchor)
	b.discoverTarget(in.Target)
	b.discoverString("anchor", anchor)
	b.discoverString("target", in.Target)
	if b.overCap {
		return nil, nil, nil, fmt.Errorf("bundle exceeds redaction bounds")
	}
	if err := b.registerAll(ctx); err != nil {
		return nil, nil, nil, err
	}
	events := make([]any, 0, len(decoded))
	ids := make([]string, 0, len(in.Events))
	for i, v := range decoded {
		events = append(events, b.tokenizeValue(v))
		ids = append(ids, in.Events[i].EventID)
	}
	// The anchor is tokenized before the spoof verdict: it is derived
	// from untrusted payload fields (a hostile hostname can be
	// literally "IP_1"), so a token-shaped anchor must trip the same
	// fail-closed path as a token-shaped banner — not ride through
	// unchecked after it.
	anchor = b.tokenizeString(anchor)
	if b.tokenSpoof {
		return nil, nil, nil, fmt.Errorf("input contains token-shaped text")
	}
	raw, err := json.Marshal(events)
	if err != nil {
		return nil, nil, nil, err
	}
	return &RedactedBundle{GroupID: in.GroupID, EventIDs: ids, Anchor: anchor, EventsJSON: raw, Baseline: in.Baseline}, b.issued, b.display, nil
}

// discoverKeys walks decoded JSON for address spans inside object keys.
// Keys are Pro schema words or device text; spans mint tokens, but a key
// is never a hostname alias — seeding "hostname" itself as HOST_n would
// rename the field. Tokenization (tokenizeValue) applies the same
// substitution to keys, so discovery and replacement stay consistent.
func (b *builder) discoverKeys(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			// A key exactly equal to a known event ID is envelope
			// identity, like a value leaf — never a leak.
			if k2 := StripControls(k); k2 != "" && !b.skip[k2] {
				discoverSpans(k2, func(glo, ghi int, d discovered) {
					b.addPending(d.category, d.normalized, k2[glo:ghi])
				})
			}
			b.discoverKeys(e)
		}
	case []any:
		for _, e := range t {
			b.discoverKeys(e)
		}
	}
}
