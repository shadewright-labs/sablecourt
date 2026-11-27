package redact

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// streamRun writes doc to a fresh stream of reg in the pieces that cuts give,
// the last one with eof, and returns the joined output and whether the stream
// closed.
func streamRun(reg *Registry, doc string, cuts []int) (string, bool) {
	s := reg.NewStream()
	var out []byte
	prev := 0
	for _, c := range cuts {
		if c < prev || c > len(doc) {
			continue
		}
		out = append(out, s.Write([]byte(doc[prev:c]), false)...)
		prev = c
	}
	out = append(out, s.Write([]byte(doc[prev:]), true)...)
	return string(out), s.closed
}

// checkStreamProperty checks the property of the decision of 2026-11-27 on
// issue 180: for a split of doc into writes, the joined stream output equals
// RedactExact(doc), or the stream closed and its output ends with the marker.
// The output never shows a held value. Below the pending cap, a stream closes
// only where the batch redactor fails closed too.
func checkStreamProperty(t testing.TB, reg *Registry, doc string, cuts []int) {
	t.Helper()
	got, closed := streamRun(reg, doc, cuts)
	if reg.ContainsHeld(got) {
		t.Fatalf("stream cut at %v of %q emitted %q, which shows a held value", cuts, doc, got)
	}
	want := reg.RedactExact(doc)
	if got == want {
		return
	}
	if !closed || !strings.HasSuffix(got, reg.Marker()) {
		t.Fatalf("stream cut at %v of %q emitted %q (closed=%v), batch gave %q", cuts, doc, got, closed, want)
	}
	if len(doc) <= maxStreamPending && batchSettles(reg.matcher.Load(), doc) {
		t.Fatalf("stream cut at %v of %q closed with %q, but batch settled to %q", cuts, doc, got, want)
	}
}

// checkEverySplit runs checkStreamProperty on every single cut, every pair of
// cuts for short docs, byte-by-byte writes and random cuts.
func checkEverySplit(t *testing.T, reg *Registry, doc string, seed uint64) {
	t.Helper()
	n := len(doc)
	for i := 0; i <= n; i++ {
		checkStreamProperty(t, reg, doc, []int{i})
	}
	every := make([]int, 0, n)
	for i := 1; i < n; i++ {
		every = append(every, i)
	}
	checkStreamProperty(t, reg, doc, every)
	if n <= 96 {
		for i := 0; i <= n; i++ {
			for j := i; j <= n; j++ {
				checkStreamProperty(t, reg, doc, []int{i, j})
			}
		}
	}
	rng := rand.New(rand.NewPCG(180, seed))
	for range 64 {
		var cuts []int
		for c := rng.IntN(8); c < n; c += 1 + rng.IntN(16) {
			cuts = append(cuts, c)
		}
		checkStreamProperty(t, reg, doc, cuts)
	}
}

// reproCase is one reproduction from the third check of the stream on issue
// 180, where the round-tracking stream gave output that batch did not.
type reproCase struct {
	name string
	reg  *Registry
	doc  string
}

func streamReproCases() []reproCase {
	var cases []reproCase

	// Finding 1: a round-8 cascade with 1-byte overlaps and pieces of 20
	// bytes reaches back 7*20 fed bytes, past what the stream held back.
	// Batch fails closed; the stream emitted a held value.
	const base = "base_held_value_0456"
	for _, empty := range []bool{false, true} {
		reg := NewRegistry()
		reg.Register(base)
		if empty {
			reg.Register("REDACTED:known")
		}
		var left, right strings.Builder
		for i := maxRewrites - 1; i >= 1; i-- {
			p := fmt.Sprintf("p%d_%s", i, strings.Repeat(string(rune('a'+i)), 17))
			if empty {
				reg.Register(p + string(rune('A'+i)))
			} else {
				reg.Register(p + KnownSecretMarker[:1])
			}
			left.WriteString(p)
		}
		for i := 1; i <= maxRewrites-1 && empty; i++ {
			right.WriteString(string(rune('A' + i)))
		}
		cases = append(cases, reproCase{
			name: fmt.Sprintf("round-8 cascade, empty marker %v", empty),
			reg:  reg,
			doc:  "PRE_" + left.String() + base + right.String() + "_POST",
		})
	}

	// Finding 2: replacing a value before a backslash run that reaches the
	// end of a write flipped the pairing of the next write, so a wrapped
	// value starting with n was skipped (marker off), or a round-2 match ran
	// in round 1.
	s1, s2 := synthSecret("secret_1", "_0000"), synthSecret("n_after", "_bs_12")
	reg := NewRegistry()
	reg.Register("REDACTED:known", s1, s2)
	cases = append(cases, reproCase{
		name: "backslash run split at a write end, empty marker",
		reg:  reg,
		doc:  `\` + s1 + `\\` + "n_after\n_bs_12",
	})
	reg = NewRegistry()
	reg.Register("REDACTED:known", s1, s2, synthSecret("bs_12", "_overlap"))
	cases = append(cases, reproCase{
		name: "backslash run split inside, round 2 demoted",
		reg:  reg,
		doc:  `\` + s1 + `\\\` + "n_after\n_bs_12_overlap",
	})

	// Finding 3: the byte after a replacement was marked as changed even when
	// it was not a backslash, so an adjacent match split across writes moved
	// to the next round: a different output, or a false close.
	a, b := synthSecret("secret_A", "_1234"), synthSecret("secret_B", "_5678")
	reg = NewRegistry()
	reg.Register(a, b, "known_secret]"+b[:8])
	cases = append(cases, reproCase{name: "adjacent split matches", reg: reg, doc: a + b})
	reg = NewRegistry()
	reg.Register(a, b)
	doc := a + b
	for i := 2; i <= maxRewrites-1; i++ {
		suf := fmt.Sprintf("_s%d_tail", i)
		reg.Register("known_secret]" + suf)
		doc += suf
	}
	cases = append(cases, reproCase{name: "adjacent split matches, 7-round cascade", reg: reg, doc: doc})
	return cases
}

// The three reproduced findings of the third check of the round-tracking
// stream: each split of each document must give the batch output, or close
// with the marker where batch fails closed.
func TestStreamMatchesBatchOnReproductions(t *testing.T) {
	t.Parallel()
	for i, tc := range streamReproCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkEverySplit(t, tc.reg, tc.doc, uint64(i))
		})
	}
}

// streamFuzzRegistries returns a registry with the marker on and one with it
// off, holding values that overlap the marker's ends, start with n or r,
// carry backslashes, and chain into each other.
func streamFuzzRegistries() []*Registry {
	held := []string{
		synthSecret("base_held", "_value_0456"),
		synthSecret("n_after", "_bs_12"),
		synthSecret("r_held", "_after_9"),
		synthSecret("tail_", `\\`, "_held_7"),
		synthSecret("secret_A", "_1234"),
		synthSecret("secret_B", "_5678"),
		"known_secret]" + "secret_B",
		"head_value_" + "[REDACT",
		"xx_lead_" + KnownSecretMarker[:1],
		synthSecret("wrap", "\n", "_held_42"),
	}
	on := NewRegistry()
	on.Register(held...)
	off := NewRegistry()
	off.Register(append(held, "REDACTED:known")...)
	return []*Registry{on, off}
}

var streamFuzzTokens = func() []string {
	toks := []string{
		KnownSecretMarker, KnownSecretMarker[:1], KnownSecretMarker[:6], KnownSecretMarker[10:],
		`\`, `\\`, `\\\`, "\n", "\r", "\r\n", `\n`, `\r`, " ", "x", "n", "r", "]", "[", "_", "A",
	}
	for _, h := range []string{
		synthSecret("base_held", "_value_0456"),
		synthSecret("n_after", "_bs_12"),
		synthSecret("r_held", "_after_9"),
		synthSecret("tail_", `\\`, "_held_7"),
		synthSecret("secret_A", "_1234"),
		synthSecret("secret_B", "_5678"),
		"head_value_",
		"xx_lead_",
		"known_secret]",
		"wrap_held_42",
	} {
		toks = append(toks, h, h[:1]+"\n"+h[1:], h[:len(h)/2]+`\n`+h[len(h)/2:], h[:len(h)/2]+"\r\n"+h[len(h)/2:], h[:len(h)/2], h[len(h)/2:])
	}
	return toks
}()

// streamFuzzInput builds a document and cuts from data: the first byte picks
// the registry, each later byte picks a token, and its high bit puts a cut
// after it.
func streamFuzzInput(data []byte) (*Registry, string, []int) {
	regs := streamFuzzRegistries()
	if len(data) == 0 {
		return regs[0], "", nil
	}
	reg := regs[int(data[0])%len(regs)]
	var b strings.Builder
	var cuts []int
	for _, c := range data[1:] {
		b.WriteString(streamFuzzTokens[int(c&0x7f)%len(streamFuzzTokens)])
		if c&0x80 != 0 {
			cuts = append(cuts, b.Len())
		}
	}
	return reg, b.String(), cuts
}

// Random documents made of held values, their halves, markers, backslash runs
// and line breaks, cut at random: the stream gives the batch output or closes
// with the marker.
func TestStreamMatchesBatchOnRandomDocuments(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(180, 3))
	for range 3000 {
		data := make([]byte, 1+rng.IntN(40))
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		reg, doc, cuts := streamFuzzInput(data)
		checkStreamProperty(t, reg, doc, cuts)
		every := make([]int, 0, len(doc))
		for i := 1; i < len(doc); i++ {
			every = append(every, i)
		}
		checkStreamProperty(t, reg, doc, every)
	}
}

func FuzzStreamMatchesBatch(f *testing.F) {
	f.Add([]byte{0, 0x81, 2, 0x83})
	f.Add([]byte{1, 4, 0x85, 6, 7, 0x88})
	f.Add([]byte{1, 0x84, 0x22, 0x8a, 0x30})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 256 {
			data = data[:256]
		}
		reg, doc, cuts := streamFuzzInput(data)
		checkStreamProperty(t, reg, doc, cuts)
	})
}

// The stream emits up to the last byte no pattern holds, keeps the rest, and
// closes with the marker when no such byte arrives before the pending cap.
func TestStreamCutsAndCap(t *testing.T) {
	t.Parallel()
	held := synthSecret("cut_held", "_value_0789")
	reg := NewRegistry()
	reg.Register(held)

	s := reg.NewStream()
	if got := string(s.Write([]byte("one two "+held[:4]), false)); got != "one two " {
		t.Fatalf("first write emitted %q, want the text up to the last space", got)
	}
	if got := s.Write([]byte(held[4:]), false); len(got) != 0 {
		t.Fatalf("write with no safe cut emitted %q", got)
	}
	if got := string(s.Write([]byte(" three"), true)); got != KnownSecretMarker+" three" {
		t.Fatalf("eof emitted %q", got)
	}

	// Line breaks, backslashes, r and n are never cut after, so a flood of
	// them closes the stream at the cap.
	for _, flood := range []string{"\n", "\r\n", `\`, "rn", "_"} {
		s = reg.NewStream()
		var out []byte
		out = append(out, s.Write([]byte("ok "+held[:6]), false)...)
		out = append(out, s.Write([]byte(strings.Repeat(flood, maxStreamPending/len(flood)+1)), false)...)
		if !s.closed {
			t.Fatalf("flood %q: stream did not close at the cap", flood)
		}
		if string(out) != "ok "+KnownSecretMarker {
			t.Fatalf("flood %q: stream emitted %q", flood, out)
		}
		if more := s.Write([]byte(held[6:]+" clean"), true); more != nil {
			t.Fatalf("flood %q: closed stream emitted %q", flood, more)
		}
	}
}

// The safe-cut search resumes where the last write left off, so each byte is
// looked at once and many small writes with no safe cut cost linear time, not
// the whole pending buffer per write.
func TestStreamCutSearchIsLinear(t *testing.T) {
	t.Parallel()
	held := synthSecret("aaaa", "aaaa_lin_0001")
	reg := NewRegistry()
	reg.Register(held)
	s := reg.NewStream()
	const writes = 20000
	for range writes {
		if out := s.Write([]byte("a"), false); len(out) != 0 {
			t.Fatalf("write with no safe cut emitted %q", out)
		}
	}
	if s.examined > writes {
		t.Fatalf("cut search examined %d bytes over %d one-byte writes, want at most %d", s.examined, writes, writes)
	}
	// A safe byte still releases everything, and the batch result holds.
	if got, want := string(s.Write([]byte(" "), false)), strings.Repeat("a", writes)+" "; got != want {
		t.Fatalf("safe byte released %d bytes, want %d", len(got), len(want))
	}
}

func BenchmarkStreamSmallWrites(b *testing.B) {
	held := synthSecret("aaaa", "aaaa_lin_0001")
	reg := NewRegistry()
	reg.Register(held)
	one := []byte("a")
	for b.Loop() {
		s := reg.NewStream()
		for range 32 * 1024 {
			s.Write(one, false)
		}
	}
}
