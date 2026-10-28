// Package redactor builds model-safe bundles: every IP, MAC, hostname,
// and CIDR in every string of a group's events is replaced by a map-owned
// counter token, with a fail-closed leak scan before anything leaves the
// machine (Designv5 §6). Library-only: the Step 11 worker wires it to the
// provider. Hostnames are tokenized only when already known as fields;
// addresses are regex-discovered, including inside URLs and sockets.
package redactor

import (
	"context"
	"encoding/json"
	"fmt"
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

// trailBad rejects a match glued to a longer token (a 12-hex fingerprint
// fragment, a longer version string). ':' stays allowed so socket ports
// survive (192.168.1.1:8080 -> IP_1:8080).
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
	pending  map[string][]string // category -> normalized values, encounter order
	seen     map[string]map[string]bool
	tokens   map[string]map[string]string // category -> normalized -> token
	hostRe   *regexp.Regexp               // known hostnames, longest first, case-insensitive
	hostTok  map[string]string            // lower hostname -> token
	issued   TokenMap
}

func (b *builder) addPending(cat, normalized string) {
	if b.seen[cat] == nil {
		b.seen[cat] = map[string]bool{}
	}
	if b.seen[cat][normalized] {
		return
	}
	b.seen[cat][normalized] = true
	b.pending[cat] = append(b.pending[cat], normalized)
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

// discoverSpans reports every validated span in s via the shared walk.
// Discovery ignores the emit flag — a span tokenization keeps literal is
// still a real value that owns a token; the leak scan judges the literal.
func discoverSpans(s string, emit func(discovered)) {
	visitSpans(s, func(_, _ int, d discovered, _ bool) { emit(d) })
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
		if ghi < len(rest) && trailBad(rest[ghi]) {
			prev = rest[ghi-1]
			rest, off = rest[ghi:], off+ghi
			continue
		}
		d, ok := classify(rest[glo:ghi])
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
	if key == "hostname" || key == "identity_aliases" || key == "aliases" {
		for _, h := range splitAliases(s) {
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				b.addPending(catHost, h)
			}
		}
	}
	discoverSpans(s, func(d discovered) { b.addPending(d.category, d.normalized) })
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

// discoverAnchor registers the anchor's identity value by its prefix.
func (b *builder) discoverAnchor(anchor string) {
	kind, val, ok := strings.Cut(anchor, ":")
	if !ok {
		return
	}
	val = strings.TrimSpace(val)
	switch kind {
	case "mac":
		b.addPending(catMAC, canonicalMAC(val))
	case "host":
		if h := strings.ToLower(val); h != "" {
			b.addPending(catHost, h)
		}
	case "ip":
		if addr, err := netip.ParseAddr(val); err == nil {
			b.addPending(catIP, addr.String())
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
		b.addPending(catNet, pfx.Masked().String())
		return
	}
	if addr, err := netip.ParseAddr(target); err == nil {
		b.addPending(catIP, addr.String())
		return
	}
	if !strings.ContainsAny(target, " \t/") {
		b.addPending(catHost, strings.ToLower(target))
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
		got, err := b.store.GetOrAssignPseudonyms(ctx, cat, vals...)
		if err != nil {
			return err
		}
		m := map[string]string{}
		for _, v := range vals {
			m[v] = got[v]
			b.issued[got[v]] = v
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
		b.hostRe = regexp.MustCompile(`(?i)\b(` + strings.Join(alts, "|") + `)\b`)
		b.hostTok = map[string]string{}
		for _, h := range hostnames {
			b.hostTok[h] = b.tokens[catHost][h]
		}
	}
	return nil
}

// tokenizeString replaces known hostnames then addresses, and caps the
// result. Address spans missing from the map are left literal: the leak
// scan fails the bundle loudly instead of silently dropping them.
func (b *builder) tokenizeString(s string) string {
	s = StripControls(s)
	if b.hostRe != nil {
		s = b.hostRe.ReplaceAllStringFunc(s, func(m string) string {
			tok := b.hostTok[strings.ToLower(m)]
			b.issued[tok] = strings.ToLower(m)
			return tok
		})
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
func (b *builder) tokenizeValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = b.tokenizeValue(e)
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
// the issued token map. With Policy.Enabled=false it passes raw values
// through with an empty map (fail-open, operator-chosen, warned at startup
// by the Step 11 worker).
func BuildBundle(ctx context.Context, st *store.Store, in BundleInput, pol Policy) (*RedactedBundle, TokenMap, error) {
	issued := TokenMap{}
	decode := func(id string, payload []byte) (any, error) {
		var v any
		if err := json.Unmarshal(payload, &v); err != nil {
			return nil, fmt.Errorf("decode event %s: %w", id, err)
		}
		return v, nil
	}
	if !pol.Enabled {
		events := make([]any, 0, len(in.Events))
		ids := make([]string, 0, len(in.Events))
		for _, e := range in.Events {
			v, err := decode(e.EventID, e.Payload)
			if err != nil {
				return nil, nil, err
			}
			events = append(events, v)
			ids = append(ids, e.EventID)
		}
		raw, err := json.Marshal(events)
		if err != nil {
			return nil, nil, err
		}
		return &RedactedBundle{GroupID: in.GroupID, EventIDs: ids, Anchor: in.Anchor, EventsJSON: raw, Baseline: in.Baseline}, issued, nil
	}
	maxChars := pol.MaxFieldChars
	if maxChars <= 0 {
		maxChars = 256
	}
	b := &builder{store: st, maxChars: maxChars, pending: map[string][]string{}, seen: map[string]map[string]bool{}, issued: TokenMap{}}
	decoded := make([]any, 0, len(in.Events))
	for _, e := range in.Events {
		v, err := decode(e.EventID, e.Payload)
		if err != nil {
			return nil, nil, err
		}
		decoded = append(decoded, v)
	}
	for _, v := range decoded {
		walkLeaves(v, "", b.discoverString)
	}
	// Anchor and target seed the map typed: the anchor carries the host
	// identity by construction ("mac:…", "host:…", "ip:…"), and the target
	// is usually the scan CIDR.
	b.discoverAnchor(in.Anchor)
	b.discoverTarget(in.Target)
	b.discoverString("anchor", in.Anchor)
	b.discoverString("target", in.Target)
	if err := b.registerAll(ctx); err != nil {
		return nil, nil, err
	}
	events := make([]any, 0, len(decoded))
	ids := make([]string, 0, len(in.Events))
	for i, v := range decoded {
		events = append(events, b.tokenizeValue(v))
		ids = append(ids, in.Events[i].EventID)
	}
	raw, err := json.Marshal(events)
	if err != nil {
		return nil, nil, err
	}
	return &RedactedBundle{GroupID: in.GroupID, EventIDs: ids, Anchor: b.tokenizeString(in.Anchor), EventsJSON: raw, Baseline: in.Baseline}, b.issued, nil
}
