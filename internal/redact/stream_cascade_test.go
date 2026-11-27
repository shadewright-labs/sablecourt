package redact

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// cascadeCase is a registry and a document in which each replacement forms
// the next held value with the text before it (or, with the marker off, joins
// the text on both sides into the next held value), so rewriting runs depth+1
// rounds and walks back over the text before the first match.
type cascadeCase struct {
	name string
	reg  *Registry
	doc  string
}

// insertBreak puts a line break (raw or escaped) after the first byte of s, so
// the value only matches in the view that skips line breaks.
func insertBreak(s, br string) string {
	if br == "" || len(s) < 2 {
		return s
	}
	return s[:1] + br + s[1:]
}

func cascadeCases() []cascadeCase {
	const base = "base_held_value_0456"
	var cases []cascadeCase
	for depth := 1; depth <= maxRewrites; depth++ {
		for _, xlen := range []int{3, 17} {
			for _, br := range []string{"", "\n", "\\n"} {
				for _, alphabet := range []string{"abcdefghij", `\a\b\c\d\e`} {
					piece := func(i int, n int) string {
						c := alphabet[(2*i)%len(alphabet) : (2*i)%len(alphabet)+2]
						return strings.Repeat(c, n)[:n]
					}
					// Marker on: x_i followed by the start of the marker is held.
					reg := NewRegistry()
					reg.Register(base)
					var b strings.Builder
					b.WriteString("pre text that is emitted early, ")
					for i := depth; i >= 1; i-- {
						x := piece(i, xlen)
						reg.Register(x + KnownSecretMarker[:6])
						b.WriteString(insertBreak(x, br))
					}
					b.WriteString(insertBreak(base, br))
					b.WriteString(" post")
					if reg.Marker() == KnownSecretMarker {
						cases = append(cases, cascadeCase{
							name: fmt.Sprintf("marker/depth=%d/x=%d/br=%q/%s", depth, xlen, br, alphabet[:2]),
							reg:  reg,
							doc:  b.String(),
						})
					}

					// Marker off: removing a match joins y_i and z_i into a held value.
					reg = NewRegistry()
					reg.Register("REDACTED:known", base)
					var left, right []string
					for i := 1; i <= depth; i++ {
						y, z := piece(i, xlen), piece(i+depth, 5)
						reg.Register(y + z)
						left = append([]string{insertBreak(y, br)}, left...)
						right = append(right, insertBreak(z, br))
					}
					if reg.Marker() == "" {
						cases = append(cases, cascadeCase{
							name: fmt.Sprintf("empty/depth=%d/x=%d/br=%q/%s", depth, xlen, br, alphabet[:2]),
							reg:  reg,
							doc: "pre text that is emitted early, " + strings.Join(left, "") +
								insertBreak(base, br) + strings.Join(right, "") + " post",
						})
					}
				}
			}
		}
	}
	return cases
}

// batchSettles reports whether redactString settles within maxRewrites rounds
// on s, so its output is the rewritten text and not the fail-closed marker.
func batchSettles(m *acMatcher, s string) bool {
	out := s
	for range maxRewrites {
		spans := matchSpans(m, out)
		if len(spans) == 0 {
			return true
		}
		out = string(replaceByteSpans([]byte(out), spans, m.marker))
	}
	return false
}

func streamChunks(reg *Registry, doc string, cuts []int) string {
	s := reg.NewStream()
	var out []byte
	prev := 0
	for _, c := range cuts {
		out = append(out, s.Write([]byte(doc[prev:c]), false)...)
		prev = c
	}
	out = append(out, s.Write([]byte(doc[prev:]), true)...)
	return string(out)
}

// Fix round 2, finding 1: rewriting runs up to maxRewrites rounds, each of
// which can reach back a pattern length before the last replacement, but the
// stream kept only one pattern length before a pending match. A cascade then
// changed text already emitted, and the emitted text and the rest formed a
// held value. For every generated cascade the stream must give exactly what
// the batch redactor gives, in every split, and never show a held value.
func TestStreamMatchesBatchOnRewriteCascades(t *testing.T) {
	t.Parallel()
	for ci, tc := range cascadeCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(180, uint64(ci)))
			m := tc.reg.matcher.Load()
			settles := batchSettles(m, tc.doc)
			want := tc.reg.RedactExact(tc.doc)
			if settles && tc.reg.ContainsHeld(want) {
				t.Fatalf("batch output %q shows a held value", want)
			}
			check := func(cuts []int) {
				t.Helper()
				got := streamChunks(tc.reg, tc.doc, cuts)
				if tc.reg.ContainsHeld(got) {
					t.Fatalf("stream cut at %v emitted %q, which shows a held value", cuts, got)
				}
				if settles && got != want {
					t.Fatalf("stream cut at %v emitted %q, batch gave %q", cuts, got, want)
				}
			}
			n := len(tc.doc)
			for i := 0; i <= n; i++ {
				check([]int{i})
			}
			every := make([]int, 0, n)
			for i := 1; i < n; i++ {
				every = append(every, i)
			}
			check(every)
			if n <= 64 {
				for i := 0; i <= n; i++ {
					for j := i; j <= n; j++ {
						check([]int{i, j})
					}
				}
				return
			}
			for range 32 {
				var cuts []int
				for c := rng.IntN(8); c < n; c += 1 + rng.IntN(24) {
					cuts = append(cuts, c)
				}
				check(cuts)
			}
		})
	}
}

// Finding 3 of the second check: rewriting that does not settle was never
// tested. Past maxRound rounds the batch redactor returns the marker alone,
// and the stream closes: it emits nothing more, even for clean text.
func TestRewritesPastMaxRoundFailClosed(t *testing.T) {
	t.Parallel()
	var seen int
	for _, tc := range cascadeCases() {
		if batchSettles(tc.reg.matcher.Load(), tc.doc) {
			continue
		}
		seen++
		if got := tc.reg.RedactExact(tc.doc); got != tc.reg.Marker() {
			t.Fatalf("%s: RedactExact = %q, want the marker alone", tc.name, got)
		}
		s := tc.reg.NewStream()
		half := len(tc.doc) / 2
		out := string(s.Write([]byte(tc.doc[:half]), false)) + string(s.Write([]byte(tc.doc[half:]), true))
		if tc.reg.ContainsHeld(out) {
			t.Fatalf("%s: stream emitted %q, which shows a held value", tc.name, out)
		}
		if !s.closed {
			t.Fatalf("%s: stream did not close", tc.name)
		}
		if more := s.Write([]byte("clean text"), true); more != nil {
			t.Fatalf("%s: closed stream emitted %q", tc.name, more)
		}
	}
	if seen == 0 {
		t.Fatal("no generated case runs past maxRound")
	}
}
