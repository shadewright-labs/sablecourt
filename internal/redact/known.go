package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

	// maxRewrites bounds how many times replaced text is scanned again. A
	// replacement that keeps forming new matches past it fails closed.
	maxRewrites = 8

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

// RegisterOwnedUntil is RegisterUntil with an owner key (a task ID), so a later
// ExtendOwned can move the expiry when the owner's lease is renewed.
func RegisterOwnedUntil(owner string, expiresAt time.Time, values ...string) {
	defaultRegistry.RegisterOwnedUntil(owner, expiresAt, values...)
}

// ExtendOwned moves the expiry of every expiring value registered for owner to
// expiresAt when that is later than the current one.
func ExtendOwned(owner string, expiresAt time.Time) {
	defaultRegistry.ExtendOwned(owner, expiresAt)
}

// RegisterSetting adds the credential values loaded from setting to the default
// registry, or refuses them; see Registry.RegisterSetting.
func RegisterSetting(setting string, values ...string) error {
	return defaultRegistry.RegisterSetting(setting, values...)
}

// RegisterSettingBytes is RegisterSetting for binary credential material.
func RegisterSettingBytes(setting string, values ...[]byte) error {
	return defaultRegistry.RegisterSettingBytes(setting, values...)
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
	owner    string
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

// ShortSecretError refuses a credential shorter than MinSecretLength. It names
// the setting the value came from and never the value.
type ShortSecretError struct {
	Setting string
}

func (e *ShortSecretError) Error() string {
	return fmt.Sprintf("%s is shorter than %d bytes; exact-match redaction cannot hold it, so it is refused", e.Setting, MinSecretLength)
}

// RegisterSetting registers credential values loaded from setting (a secret
// name, a flag or an environment variable). A value that is empty after
// trimming is not configured and is skipped. A value shorter than
// MinSecretLength is refused with a *ShortSecretError naming setting: the
// registry cannot hold it, so the caller must not use it. Longer values are
// registered either way.
func (r *Registry) RegisterSetting(setting string, values ...string) error {
	var err error
	for _, v := range values {
		n := len(strings.TrimSpace(v))
		if n == 0 {
			continue
		}
		if n < MinSecretLength {
			err = &ShortSecretError{Setting: setting}
			continue
		}
		r.Register(v)
	}
	return err
}

// RegisterSettingBytes is RegisterSetting for binary credential material.
func (r *Registry) RegisterSettingBytes(setting string, values ...[]byte) error {
	var err error
	for _, b := range values {
		if len(b) == 0 {
			continue
		}
		if len(b) < MinSecretLength {
			err = &ShortSecretError{Setting: setting}
			continue
		}
		r.RegisterBytes(b)
	}
	return err
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

// RegisterOwnedUntil records values like RegisterUntil and tags them with owner.
func (r *Registry) RegisterOwnedUntil(owner string, expiresAt time.Time, values ...string) {
	if expiresAt.IsZero() {
		return
	}
	r.registerOwned(owner, expiresAt, values)
}

// ExtendOwned moves the expiry of every expiring value registered for owner to
// expiresAt when that is later than the current one. Values registered without
// an expiry are left alone.
func (r *Registry) ExtendOwned(owner string, expiresAt time.Time) {
	if r == nil || owner == "" || expiresAt.IsZero() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for v, e := range r.entries {
		if e.owner == owner && !e.until.IsZero() && expiresAt.After(e.until) {
			e.until = expiresAt
			r.entries[v] = e
		}
	}
}

// OwnedExpiry reports the earliest expiry among the expiring values registered
// for owner. It exposes no value.
func (r *Registry) OwnedExpiry(owner string) (time.Time, bool) {
	if r == nil || owner == "" {
		return time.Time{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var earliest time.Time
	found := false
	for _, e := range r.entries {
		if e.owner == owner && !e.until.IsZero() && (!found || e.until.Before(earliest)) {
			earliest, found = e.until, true
		}
	}
	return earliest, found
}

func (r *Registry) register(until time.Time, values []string) {
	r.registerOwned("", until, values)
}

func (r *Registry) registerOwned(owner string, until time.Time, values []string) {
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
		r.entries[v] = registryEntry{until: until, owner: owner, patterns: derivePatterns(v)}
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

// RedactText redacts exact registered secrets first (by the rule of
// RedactExact), then applies pattern rules.
func (r *Registry) RedactText(input string) Result {
	if input == "" {
		return Result{}
	}
	out, exactHits := r.redactExact(input)
	res := redactPatterns(out)
	// A pattern marker next to other text must not rebuild a held value.
	if r.ContainsHeld(res.Text) {
		res.Text = r.marker()
		exactHits++
	}
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
	// Hits count every exact and pattern match, and the refusal of a text in
	// which a pattern marker rebuilt a held value. With the marker off those
	// leave no "[REDACTED:" text, so the text is not checked for one.
	return r.RedactText(input).Hits > 0
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
	keep := func(s string) {
		if len(s) < MinSecretLength {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	// Each form is stored as written, for the raw-byte view, and with line
	// breaks stripped, for the view that skips them.
	add := func(s string) {
		keep(s)
		keep(stripLineBreaks(s))
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
	// marker replaces a match: KnownSecretMarker, or empty when a pattern
	// overlaps it (see markerFor).
	marker string
}

func buildACMatcher(patterns map[string]struct{}) *acMatcher {
	if len(patterns) == 0 {
		return nil
	}
	m := &acMatcher{
		nodes:  make([]acNode, 1, len(patterns)*16),
		marker: markerFor(patterns),
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

// scan feeds s through the automaton and returns the merged match spans with
// the final state, the count of fed bytes and the ring of their source offsets.
// With skipBreaks it skips CR, LF and their escaped forms, so a value wrapped
// across lines still matches; without it every byte is fed (the raw-byte view).
// Both views are needed: skipping "\n" also eats the first byte of a value that
// starts with n or r after a backslash.
//
// With rounds set, a byte whose round is at least below ends any match in
// progress and starts none, so only matches made of earlier rounds are found
// (see Stream.lowest).
func scan[T string | []byte](m *acMatcher, s T, skipBreaks bool, rounds []uint8, below uint8) ([]byteSpan, int32, int, []int) {
	ringSize := m.maxPatLen
	ring := make([]int, ringSize)
	var (
		spans []byteSpan
		st    int32
		k     int
	)
	barrier := func(idx int) bool {
		if rounds != nil && rounds[idx] >= below {
			st = 0
			return true
		}
		return false
	}
	feed := func(idx int, b byte) {
		if barrier(idx) {
			return
		}
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
		if skipBreaks {
			if b == '\r' || b == '\n' {
				barrier(i)
				i++
				continue
			}
			if b == '\\' && i+1 < len(s) {
				next := s[i+1]
				if next == 'r' || next == 'n' {
					barrier(i)
					barrier(i + 1)
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
		}
		feed(i, b)
		i++
	}
	return spans, st, k, ring
}

// matchSpans returns the merged match spans of both views of s.
func matchSpans[T string | []byte](m *acMatcher, s T) []byteSpan {
	skipped, _, _, _ := scan(m, s, true, nil, 0)
	raw, _, _, _ := scan(m, s, false, nil, 0)
	return mergeSpans(skipped, raw)
}

// mergeSpans merges two sorted span lists into one sorted list without overlaps.
func mergeSpans(a, b []byteSpan) []byteSpan {
	if len(b) == 0 {
		return a
	}
	if len(a) == 0 {
		return b
	}
	all := append(slices.Clone(a), b...)
	slices.SortFunc(all, func(x, y byteSpan) int { return x.start - y.start })
	out := all[:0:0]
	for _, sp := range all {
		if n := len(out); n > 0 && sp.start < out[n-1].end {
			if sp.end > out[n-1].end {
				out[n-1].end = sp.end
			}
			continue
		}
		out = append(out, sp)
	}
	return out
}

func (m *acMatcher) hit(s string) bool {
	return len(matchSpans(m, s)) > 0
}

// redactString replaces every match in s with the marker and scans the result
// again, so a marker next to other text cannot form a held value. If that has
// not settled after maxRewrites rounds, the whole of s is replaced by the
// marker, which holds no registered value (see markerFor).
func (m *acMatcher) redactString(s string) (string, int) {
	out, hits := s, 0
	for range maxRewrites {
		spans := matchSpans(m, out)
		if len(spans) == 0 {
			return out, hits
		}
		hits += len(spans)
		out = string(replaceByteSpans([]byte(out), spans, m.marker))
	}
	return m.marker, hits
}

// Stream redacts exact registered secrets across arbitrary write boundaries,
// including secrets split by CR or LF characters.
//
// A replacement is written into the pending buffer and scanned again with
// the bytes around it, so what the stream emits is exactly the text it
// checked: a marker next to other text cannot form a held value unseen.
// How much text stays pending, and why no later round can change emitted
// text, is argued at holdBack.
type Stream struct {
	reg *Registry
	buf []byte
	// round holds, for each byte of buf, the rewrite round that last changed
	// it or the way it is read (0 for input bytes); see rewrite.
	round []uint8
	// closed is set when rewriting did not settle within maxRewrites. The
	// stream then fails closed and emits nothing more.
	closed bool
}

// maxRound is the last rewrite round whose result can still be emitted.
// redactString scans at most maxRewrites times, so text whose matches stop
// after maxRewrites-1 replacement rounds is the last it returns rewritten; one
// more round and it fails closed to the marker. The stream fails closed at the
// same round.
const maxRound = maxRewrites - 1

// The stream keeps the batch redactor's rounds. Round 1 replaces matches in
// the input; round r+1 replaces matches in the text round r left. Each byte of
// the pending buffer carries the last round that changed it or the way it is
// read (0 for an input byte), and a match belongs to one round more than the
// highest round inside it.
//
// A match in round r+1 that was not a match in round r contains a byte round
// r changed: a marker byte, or a byte whose reading changed. Those are the
// byte just before a replacement (a backslash there can stop or start pairing
// with what follows, and with the marker off the two sides of a removed match
// join there) and the run of backslashes right after it. Text left of the
// leftmost change is unchanged and read the same way. A match is at most
// maxPatLen fed bytes, so round r+1 starts at most maxPatLen fed bytes before
// the leftmost replacement of round r, and the byte before a replacement is
// the leftmost byte a round can change.
//
// At a cut, a match not seen yet starts at or after settle (liveCut and the
// spans across settle set it), so it and the rounds after it change no byte
// before settle - (r-1)*maxPatLen - 1 in round r. The marker's length drops
// out: a marker byte lies right of the replacement start that put it there,
// so it never moves the leftmost change. Counting skip-view bytes covers the
// raw-byte view too, because that view feeds every byte the skip view does.

// holdBack is how many fed bytes the stream keeps pending before settle, or
// before the first match it has not replaced yet when that is earlier. A
// chain of rounds started there has at most maxRound rounds, so it reaches
// (maxRound-1)*maxPatLen fed bytes back plus the byte before its last
// replacement: no later round can change an emitted byte.
func (m *acMatcher) holdBack() int {
	return (maxRound-1)*m.maxPatLen + 1
}

// String keeps fmt from printing the buffered start of a possible match.
func (*Stream) String() string { return "[REDACTED:secret_stream]" }

// GoString keeps %#v from printing the buffered start of a possible match.
func (*Stream) GoString() string { return "[REDACTED:secret_stream]" }

// Write filters raw through the bound registry and returns bytes safe to emit.
// Pass eof=true when the stream closes to flush any remaining buffered tail.
func (s *Stream) Write(raw []byte, eof bool) []byte {
	if s.closed {
		return nil
	}
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
		s.buf, s.round = nil, nil
		return out
	}
	if len(raw) == 0 && len(s.buf) == 0 {
		return nil
	}
	s.buf = append(s.buf, raw...)
	s.round = append(s.round, make([]uint8, len(raw))...)

	// Each pass replaces matches of the lowest round present, so the rounds
	// strictly rise and maxRound+1 passes always reach one that replaces none.
	for range maxRound + 1 {
		settle, spans := s.cut(m, eof)
		low, lowSpans, ok := s.lowest(m, spans, eof)
		if !ok {
			break
		}
		if ready := s.ready(m, lowSpans, low, settle, eof); len(ready) > 0 {
			// Rewrite and scan again: the marker is now part of the text.
			s.rewrite(m, ready, uint8(low))
			continue
		}
		limit := settle
		if len(spans) > 0 {
			limit = min(limit, spans[0].start)
		}
		emit := limit
		if limit < len(s.buf) {
			emit = keepBefore(s.buf[:limit], m.holdBack())
		}
		pendingCap := max(maxStreamPending, 4*m.maxPatLen)
		out := bytes.Clone(s.buf[:emit])
		if !eof && len(s.buf)-settle > pendingCap {
			// Fail closed on adversarial CR/LF flooding after an active
			// prefix. The marker stays pending with the text kept before
			// it, so it is scanned with what precedes and follows it. It
			// counts as a first-round replacement at settle, which the
			// text kept before it covers (see holdBack).
			next := append(bytes.Clone(s.buf[emit:settle]), m.marker...)
			rounds := append(bytes.Clone(s.round[emit:settle]), bytes.Repeat([]byte{1}, len(m.marker))...)
			if k := settle - emit - 1; k >= 0 {
				rounds[k] = max(rounds[k], 1)
			}
			s.buf, s.round = next, rounds
			return out
		}
		rem := copy(s.buf, s.buf[emit:])
		copy(s.round, s.round[emit:])
		s.buf, s.round = s.buf[:rem], s.round[:rem]
		return out
	}
	s.closed = true
	s.buf, s.round = nil, nil
	return nil
}

// lowest returns the lowest round r of the pending matches and the merged
// matches of that round, found with every byte of round r or later as a
// barrier: they are the matches the batch redactor finds in round r, since
// those bytes are the only ones that differ from its text after round r-1. It
// reports false when that round is past maxRound, where the batch redactor
// fails closed.
func (s *Stream) lowest(m *acMatcher, spans []byteSpan, eof bool) (int, []byteSpan, bool) {
	if len(spans) == 0 {
		return 0, nil, true
	}
	var top uint8
	for _, v := range s.round {
		top = max(top, v)
	}
	skipView := s.skipView(eof)
	for r := 1; r <= int(top)+1; r++ {
		if r > maxRound {
			return 0, nil, false
		}
		if r == int(top)+1 {
			// No byte is a barrier: these are all the pending matches.
			return r, spans, true
		}
		skipped, _, _, _ := scan(m, skipView, true, s.round, uint8(r))
		raw, _, _, _ := scan(m, s.buf, false, s.round, uint8(r))
		if found := mergeSpans(skipped, raw); len(found) > 0 {
			return r, found, true
		}
	}
	return 0, nil, false
}

// ready returns the matches of round r that no later input can change, so
// the batch redactor replaces them in round r with the same extent. An
// earlier round still to come changes no byte before
// settle - (r-2)*maxPatLen - 1, and a match of round r still to come starts no
// earlier than settle - (r-1)*maxPatLen, so a match of round r that ends there
// is final. At eof nothing is still to come.
func (s *Stream) ready(m *acMatcher, spans []byteSpan, r, settle int, eof bool) []byteSpan {
	limit := settle
	if !eof && r > 1 {
		limit = keepBefore(s.buf[:settle], (r-1)*m.maxPatLen)
	}
	var ready []byteSpan
	for _, sp := range spans {
		if eof || sp.end <= limit {
			ready = append(ready, sp)
		}
	}
	return ready
}

// rewrite replaces the ready spans of round r with the marker and records r
// for each changed byte: the marker bytes, the byte before them and the run of
// backslashes after them, whose reading the replacement can change.
func (s *Stream) rewrite(m *acMatcher, ready []byteSpan, r uint8) {
	next := make([]byte, 0, len(s.buf))
	rounds := make([]uint8, 0, len(s.round))
	type change struct {
		at, end int
		round   uint8
	}
	changes := make([]change, 0, len(ready))
	pos := 0
	for _, sp := range ready {
		next = append(next, s.buf[pos:sp.start]...)
		rounds = append(rounds, s.round[pos:sp.start]...)
		at := len(next)
		next = append(next, m.marker...)
		rounds = append(rounds, bytes.Repeat([]byte{r}, len(m.marker))...)
		changes = append(changes, change{at: at, end: len(next), round: r})
		pos = sp.end
	}
	next = append(next, s.buf[pos:]...)
	rounds = append(rounds, s.round[pos:]...)
	for _, c := range changes {
		if c.at > 0 {
			rounds[c.at-1] = max(rounds[c.at-1], c.round)
		}
		for i := c.end; i < len(next); i++ {
			rounds[i] = max(rounds[i], c.round)
			if next[i] != '\\' {
				break
			}
		}
	}
	s.buf, s.round = next, rounds
}

// cut scans the pending buffer in both views and returns settle (the offset
// where a match that may still complete starts) and the merged match spans.
func (s *Stream) cut(m *acMatcher, eof bool) (settle int, spans []byteSpan) {
	skipView := s.skipView(eof)
	skipped, stS, kS, ringS := scan(m, skipView, true, nil, 0)
	rawSpans, stR, kR, ringR := scan(m, s.buf, false, nil, 0)
	spans = mergeSpans(skipped, rawSpans)
	settle = len(s.buf)
	if eof {
		return settle, spans
	}
	settle = min(m.liveCut(stS, kS, ringS, len(skipView)), m.liveCut(stR, kR, ringR, len(s.buf)))
	if len(skipView) < len(s.buf) && settle > len(skipView) {
		settle = len(skipView)
	}
	for _, sp := range spans {
		if sp.end > settle && sp.start < settle {
			settle = sp.start
		}
	}
	return settle, spans
}

// skipView is the pending buffer as the view that skips line breaks reads it.
// A trailing unpaired backslash may start an escaped line break that the next
// write completes, so before eof that view stops short of it; read as a plain
// byte it would end a partial match that is still live and wrongly settle the
// text before it.
func (s *Stream) skipView(eof bool) []byte {
	if !eof && trailingOddBackslash(s.buf) {
		return s.buf[:len(s.buf)-1]
	}
	return s.buf
}

// keepBefore returns the offset in b of the n-th last byte the view that skips
// line breaks feeds to the automaton, or 0 when it feeds fewer. That view
// feeds no more bytes than the raw one, so the offset also keeps n raw bytes.
// An escaped backslash counts as two fed bytes but is never split, so the
// pending text is read from its first byte as it was read in the whole text.
func keepBefore(b []byte, n int) int {
	fed := make([]int, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c == '\r' || c == '\n' {
			i++
			continue
		}
		if c == '\\' && i+1 < len(b) {
			switch b[i+1] {
			case 'r', 'n':
				i += 2
				continue
			case '\\':
				fed = append(fed, i, i)
				i += 2
				continue
			}
		}
		fed = append(fed, i)
		i++
	}
	if len(fed) <= n {
		return 0
	}
	return fed[len(fed)-n]
}

// liveCut returns the source offset where a match that may still complete
// starts, or n when none is in progress.
func (m *acMatcher) liveCut(st int32, k int, ring []int, n int) int {
	live := st
	for live != 0 && len(m.nodes[live].edges) == 0 {
		live = m.nodes[live].fail
	}
	if live == 0 {
		return n
	}
	return ring[(k-int(m.nodes[live].depth))%m.maxPatLen]
}

func trailingOddBackslash(b []byte) bool {
	n := 0
	for i := len(b) - 1; i >= 0 && b[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

func replaceByteSpans(src []byte, spans []byteSpan, marker string) []byte {
	out := make([]byte, 0, len(src))
	pos := 0
	for _, sp := range spans {
		out = append(out, src[pos:sp.start]...)
		out = append(out, marker...)
		pos = sp.end
	}
	out = append(out, src[pos:]...)
	return out
}
