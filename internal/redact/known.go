package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// MinSecretLength is the shortest secret value accepted by Registry.
	// Shorter values (such as "ok", "true", "admin", or short numbers) collide
	// with ordinary log lines and JSON keys, whereas tokens, API keys, webhook
	// secrets, and signing material are at least 8 bytes.
	MinSecretLength = 8

	// KnownSecretCategory is the redaction category reported for exact-match hits.
	KnownSecretCategory = "known_secret"

	// KnownSecretMarker replaces exact-matched daemon-held secrets.
	KnownSecretMarker = "[REDACTED:known_secret]"

	// maxStreamPending bounds bytes buffered in a Stream when an active prefix
	// is followed by a long run of CR/LF bytes.
	maxStreamPending = 64 * 1024

	// ExpiredSecretRetention is how long a value registered with an expiry
	// (a task lease token) stays registered after it expires. Dropping expired
	// values keeps the registry bounded by the leases issued in the last lease
	// lifetime plus this window instead of every lease since the daemon started.
	ExpiredSecretRetention = time.Hour
)

var defaultRegistry = NewRegistry()

// DefaultRegistry returns the process-wide registry of daemon-held secrets.
func DefaultRegistry() *Registry {
	return defaultRegistry
}

// Register adds exact secret values to the default registry.
func Register(values ...string) {
	defaultRegistry.Register(values...)
}

// RegisterUntil adds exact secret values that stop being valid at expiresAt to
// the default registry.
func RegisterUntil(expiresAt time.Time, values ...string) {
	defaultRegistry.RegisterUntil(expiresAt, values...)
}

// RegisterBytes adds binary secret material (raw, hex, and base64 encodings)
// to the default registry.
func RegisterBytes(values ...[]byte) {
	defaultRegistry.RegisterBytes(values...)
}

// NewStream returns a streaming exact-secret redactor bound to the default registry.
func NewStream() *Stream {
	return defaultRegistry.NewStream()
}

// Registry stores exact secret values held by the daemon and builds a linear-time
// matcher consulted before pattern redaction. Values are never logged or exposed.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]registryEntry
	matcher atomic.Pointer[acMatcher]
	now     func() time.Time
}

// registryEntry holds the derived match forms of one registered value. A zero
// until means the value never expires.
type registryEntry struct {
	until    time.Time
	patterns []string
}

// NewRegistry creates an empty, concurrency-safe secret registry.
func NewRegistry() *Registry {
	return &Registry{
		entries: make(map[string]registryEntry),
		now:     time.Now,
	}
}

// Len reports how many values are registered, without exposing them.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// String prevents fmt and slog from exposing registered secrets.
func (*Registry) String() string { return "[REDACTED:secret_registry]" }

// GoString prevents %#v formatting from exposing registered secrets.
func (*Registry) GoString() string { return "[REDACTED:secret_registry]" }

// MarshalJSON prevents encoding/json from exposing registered secrets.
func (*Registry) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED:secret_registry]"`), nil
}

// Register records one or more exact secret values and their quoted and JSON-escaped
// forms so they are redacted before the pattern filter runs. Values shorter than
// MinSecretLength after trimming whitespace are ignored. Registered values never
// expire.
func (r *Registry) Register(values ...string) {
	r.register(time.Time{}, values)
}

// RegisterUntil records values like Register, for credentials that stop being
// valid at expiresAt (task lease tokens). Such a value is dropped by a later
// registration once it is more than ExpiredSecretRetention past expiresAt, so
// the registry does not grow with every lease the daemon has issued. A value
// also registered without an expiry is never dropped.
func (r *Registry) RegisterUntil(expiresAt time.Time, values ...string) {
	if expiresAt.IsZero() {
		return
	}
	r.register(expiresAt, values)
}

func (r *Registry) register(until time.Time, values []string) {
	if r == nil || len(values) == 0 {
		return
	}
	var pending []string
	r.mu.RLock()
	for _, v := range values {
		if len(strings.TrimSpace(v)) < MinSecretLength {
			continue
		}
		if e, seen := r.entries[v]; !seen || !covers(e.until, until) {
			pending = append(pending, v)
		}
	}
	r.mu.RUnlock()
	if len(pending) == 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	changed := r.pruneLocked()
	for _, v := range pending {
		if e, seen := r.entries[v]; seen {
			if !covers(e.until, until) {
				e.until = until
				r.entries[v] = e
			}
			continue
		}
		r.entries[v] = registryEntry{until: until, patterns: derivePatterns(v)}
		changed = true
	}
	if changed {
		r.rebuildLocked()
	}
}

// covers reports whether an entry valid until have already outlives want.
func covers(have, want time.Time) bool {
	if have.IsZero() {
		return true
	}
	return !want.IsZero() && !want.After(have)
}

// pruneLocked drops values whose expiry plus ExpiredSecretRetention has passed.
func (r *Registry) pruneLocked() bool {
	clock := r.now
	if clock == nil {
		clock = time.Now
	}
	cutoff := clock().Add(-ExpiredSecretRetention)
	pruned := false
	for v, e := range r.entries {
		if !e.until.IsZero() && e.until.Before(cutoff) {
			delete(r.entries, v)
			pruned = true
		}
	}
	return pruned
}

func (r *Registry) rebuildLocked() {
	patterns := make(map[string]struct{}, len(r.entries))
	for _, e := range r.entries {
		for _, p := range e.patterns {
			patterns[p] = struct{}{}
		}
	}
	r.matcher.Store(buildACMatcher(patterns))
}

// RegisterBytes records binary secret material in raw, hex, and base64 forms.
func (r *Registry) RegisterBytes(values ...[]byte) {
	if r == nil || len(values) == 0 {
		return
	}
	var forms []string
	for _, b := range values {
		if len(b) < MinSecretLength {
			continue
		}
		forms = append(forms,
			string(b),
			hex.EncodeToString(b),
			strings.ToUpper(hex.EncodeToString(b)),
			base64.StdEncoding.EncodeToString(b),
			base64.RawStdEncoding.EncodeToString(b),
			base64.URLEncoding.EncodeToString(b),
			base64.RawURLEncoding.EncodeToString(b),
		)
	}
	r.Register(forms...)
}

// RedactText redacts exact registered secrets first, then applies pattern rules.
func (r *Registry) RedactText(input string) Result {
	if input == "" {
		return Result{}
	}
	out := input
	exactHits := 0
	if r != nil {
		if m := r.matcher.Load(); m != nil {
			out, exactHits = m.redactString(out)
		}
	}
	res := redactPatterns(out)
	if exactHits > 0 {
		res.Hits += exactHits
		res.Categories = append(res.Categories, KnownSecretCategory)
		sort.Strings(res.Categories)
	}
	return res
}

// RedactString redacts exact registered secrets first, then applies pattern rules.
func (r *Registry) RedactString(input string) string {
	return r.RedactText(input).Text
}

// ContainsSensitive reports whether input matches any registered secret or pattern rule.
func (r *Registry) ContainsSensitive(input string) bool {
	res := r.RedactText(input)
	return res.Hits > 0 && strings.Contains(res.Text, "[REDACTED:")
}

// NewStream returns a streaming exact-secret redactor bound to r.
func (r *Registry) NewStream() *Stream {
	return &Stream{reg: r}
}

func derivePatterns(v string) []string {
	candidates := []string{v}
	if trimmed := strings.TrimSpace(v); trimmed != v && len(trimmed) >= MinSecretLength {
		candidates = append(candidates, trimmed)
	}
	seen := make(map[string]struct{}, 8)
	out := make([]string, 0, 8)
	add := func(s string) {
		clean := stripLineBreaks(s)
		if len(clean) < MinSecretLength {
			return
		}
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	for _, s := range candidates {
		add(s)
		if q := strconv.Quote(s); len(q) >= 2 {
			add(q[1 : len(q)-1])
		}
		if jb, err := json.Marshal(s); err == nil && len(jb) >= 2 {
			add(string(jb[1 : len(jb)-1]))
		}
		if noHTML := jsonEscapeNoHTML(s); noHTML != "" {
			add(noHTML)
			if strings.Contains(noHTML, "/") {
				add(strings.ReplaceAll(noHTML, "/", `\/`))
			}
		}
		if strings.Contains(s, "'") {
			add(strings.ReplaceAll(s, "'", `\'`))
		}
	}
	return out
}

func jsonEscapeNoHTML(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return ""
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(b) < 2 {
		return ""
	}
	return string(b[1 : len(b)-1])
}

func stripLineBreaks(s string) string {
	if !strings.ContainsAny(s, "\r\n\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\r' || c == '\n' {
			i++
			continue
		}
		if c == '\\' && i+1 < len(s) {
			next := s[i+1]
			if next == 'r' || next == 'n' {
				i += 2
				continue
			}
			if next == '\\' {
				b.WriteByte('\\')
				b.WriteByte('\\')
				i += 2
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

type acEdge struct {
	b    byte
	next int32
}

type acNode struct {
	edges       []acEdge
	fail        int32
	depth       int32
	maxMatchLen int32
}

type acMatcher struct {
	rootNext  [256]int32
	nodes     []acNode
	maxPatLen int
}

func buildACMatcher(patterns map[string]struct{}) *acMatcher {
	if len(patterns) == 0 {
		return nil
	}
	m := &acMatcher{
		nodes: make([]acNode, 1, len(patterns)*16),
	}
	for p := range patterns {
		if len(p) > m.maxPatLen {
			m.maxPatLen = len(p)
		}
		var u int32
		for i := range len(p) {
			b := p[i]
			nxt := m.findEdge(u, b)
			if nxt == 0 {
				nxt = int32(len(m.nodes))
				m.nodes = append(m.nodes, acNode{depth: m.nodes[u].depth + 1})
				m.addEdge(u, b, nxt)
			}
			u = nxt
		}
		if int32(len(p)) > m.nodes[u].maxMatchLen {
			m.nodes[u].maxMatchLen = int32(len(p))
		}
	}

	queue := make([]int32, 0, len(m.nodes))
	for _, e := range m.nodes[0].edges {
		m.rootNext[e.b] = e.next
		m.nodes[e.next].fail = 0
		queue = append(queue, e.next)
	}
	for head := 0; head < len(queue); head++ {
		u := queue[head]
		for _, e := range m.nodes[u].edges {
			f := m.nodes[u].fail
			for f != 0 && m.findEdge(f, e.b) == 0 {
				f = m.nodes[f].fail
			}
			var target int32
			if f == 0 {
				target = m.rootNext[e.b]
			} else {
				target = m.findEdge(f, e.b)
			}
			m.nodes[e.next].fail = target
			if m.nodes[target].maxMatchLen > m.nodes[e.next].maxMatchLen {
				m.nodes[e.next].maxMatchLen = m.nodes[target].maxMatchLen
			}
			queue = append(queue, e.next)
		}
	}
	return m
}

func (m *acMatcher) addEdge(u int32, b byte, next int32) {
	edges := m.nodes[u].edges
	idx, _ := slices.BinarySearchFunc(edges, b, func(e acEdge, target byte) int {
		if e.b < target {
			return -1
		}
		if e.b > target {
			return 1
		}
		return 0
	})
	edges = slices.Insert(edges, idx, acEdge{b: b, next: next})
	m.nodes[u].edges = edges
}

func (m *acMatcher) findEdge(u int32, b byte) int32 {
	edges := m.nodes[u].edges
	if len(edges) <= 8 {
		for _, e := range edges {
			if e.b == b {
				return e.next
			}
		}
		return 0
	}
	idx, found := slices.BinarySearchFunc(edges, b, func(e acEdge, target byte) int {
		if e.b < target {
			return -1
		}
		if e.b > target {
			return 1
		}
		return 0
	})
	if !found {
		return 0
	}
	return edges[idx].next
}

func (m *acMatcher) step(st int32, b byte) int32 {
	if st == 0 {
		return m.rootNext[b]
	}
	for st != 0 {
		if nxt := m.findEdge(st, b); nxt != 0 {
			return nxt
		}
		st = m.nodes[st].fail
	}
	return m.rootNext[b]
}

type byteSpan struct {
	start, end int
}

func appendMergedSpan(spans []byteSpan, start, end int) []byteSpan {
	for len(spans) > 0 && spans[len(spans)-1].end > start {
		if spans[len(spans)-1].start < start {
			start = spans[len(spans)-1].start
		}
		spans = spans[:len(spans)-1]
	}
	return append(spans, byteSpan{start: start, end: end})
}

// scan feeds s through the automaton, skipping CR, LF and their escaped
// forms, and returns the merged match spans with the final state, the count of
// fed bytes and the ring of their source offsets.
func scan[T string | []byte](m *acMatcher, s T) ([]byteSpan, int32, int, []int) {
	ringSize := m.maxPatLen
	ring := make([]int, ringSize)
	var (
		spans []byteSpan
		st    int32
		k     int
	)
	feed := func(idx int, b byte) {
		ring[k%ringSize] = idx
		k++
		st = m.step(st, b)
		if ml := int(m.nodes[st].maxMatchLen); ml > 0 {
			start := ring[(k-ml)%ringSize]
			spans = appendMergedSpan(spans, start, idx+1)
		}
	}
	for i := 0; i < len(s); {
		b := s[i]
		if b == '\r' || b == '\n' {
			i++
			continue
		}
		if b == '\\' && i+1 < len(s) {
			next := s[i+1]
			if next == 'r' || next == 'n' {
				i += 2
				continue
			}
			if next == '\\' {
				feed(i, '\\')
				feed(i+1, '\\')
				i += 2
				continue
			}
		}
		feed(i, b)
		i++
	}
	return spans, st, k, ring
}

func (m *acMatcher) redactString(s string) (string, int) {
	spans, _, _, _ := scan(m, s)
	if len(spans) == 0 {
		return s, 0
	}
	var out strings.Builder
	out.Grow(len(s))
	pos := 0
	for _, sp := range spans {
		out.WriteString(s[pos:sp.start])
		out.WriteString(KnownSecretMarker)
		pos = sp.end
	}
	out.WriteString(s[pos:])
	return out.String(), len(spans)
}

// Stream redacts exact registered secrets across arbitrary write boundaries,
// including secrets split by CR or LF characters.
type Stream struct {
	reg *Registry
	buf []byte
}

// String keeps fmt from printing the buffered start of a possible match.
func (*Stream) String() string { return "[REDACTED:secret_stream]" }

// GoString keeps %#v from printing the buffered start of a possible match.
func (*Stream) GoString() string { return "[REDACTED:secret_stream]" }

// Write filters raw through the bound registry and returns bytes safe to emit.
// Pass eof=true when the stream closes to flush any remaining buffered tail.
func (s *Stream) Write(raw []byte, eof bool) []byte {
	reg := s.reg
	if reg == nil {
		reg = defaultRegistry
	}
	m := reg.matcher.Load()
	if m == nil {
		if len(s.buf) == 0 {
			return raw
		}
		out := make([]byte, 0, len(s.buf)+len(raw))
		out = append(out, s.buf...)
		out = append(out, raw...)
		s.buf = nil
		return out
	}
	if len(raw) == 0 && len(s.buf) == 0 {
		return nil
	}
	if len(s.buf) == 0 && eof {
		spans, _, _, _ := scan(m, raw)
		if len(spans) == 0 {
			return raw
		}
		return replaceByteSpans(raw, spans)
	}

	s.buf = append(s.buf, raw...)
	spans, st, k, ring := scan(m, s.buf)

	safeCut := len(s.buf)
	if !eof {
		live := st
		for live != 0 && len(m.nodes[live].edges) == 0 {
			live = m.nodes[live].fail
		}
		if live != 0 {
			depth := int(m.nodes[live].depth)
			safeCut = ring[(k-depth)%m.maxPatLen]
		}
		if trailingOddBackslash(s.buf) && safeCut > len(s.buf)-1 {
			safeCut = len(s.buf) - 1
		}
		for _, sp := range spans {
			if sp.end > safeCut && sp.start < safeCut {
				safeCut = sp.start
			}
		}
	}

	pendingCap := max(maxStreamPending, 4*m.maxPatLen)
	if !eof && len(s.buf)-safeCut > pendingCap {
		// Fail closed on adversarial CR/LF flooding after an active prefix.
		out := emitPrefix(s.buf, spans, safeCut)
		out = append(out, KnownSecretMarker...)
		s.buf = nil
		return out
	}

	out := emitPrefix(s.buf, spans, safeCut)
	if safeCut == len(s.buf) {
		s.buf = s.buf[:0]
	} else {
		rem := copy(s.buf, s.buf[safeCut:])
		s.buf = s.buf[:rem]
	}
	return out
}

func trailingOddBackslash(b []byte) bool {
	n := 0
	for i := len(b) - 1; i >= 0 && b[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

func emitPrefix(buf []byte, spans []byteSpan, cut int) []byte {
	if cut == 0 {
		return nil
	}
	var prefixSpans []byteSpan
	for _, sp := range spans {
		if sp.end <= cut {
			prefixSpans = append(prefixSpans, sp)
		}
	}
	if len(prefixSpans) == 0 {
		return bytes.Clone(buf[:cut])
	}
	return replaceByteSpans(buf[:cut], prefixSpans)
}

func replaceByteSpans(src []byte, spans []byteSpan) []byte {
	out := make([]byte, 0, len(src))
	pos := 0
	for _, sp := range spans {
		out = append(out, src[pos:sp.start]...)
		out = append(out, KnownSecretMarker...)
		pos = sp.end
	}
	out = append(out, src[pos:]...)
	return out
}
