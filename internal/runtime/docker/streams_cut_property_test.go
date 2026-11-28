package docker

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

// cutSecret is one secret placed in generated output: its text, the values
// that must never be shown, and whether it is a private key (whose body
// must not show even in part) with or without an END line.
type cutSecret struct {
	text       string
	values     []string
	key        bool
	unfinished bool
}

const cutKinds = 7

// makeCutSecret builds a secret of one kind. Keys get bodyLines lines of
// body, so a long body runs from the context into the kept tail. Key-shaped
// literals are split so that no tracked file holds one.
func makeCutSecret(r *rand.Rand, kind, bodyLines int, lease string) cutSecret {
	switch kind {
	case 0, 1:
		s := cutSecret{key: true, unfinished: kind == 1}
		var b strings.Builder
		b.WriteString("-----BEGIN " + "PRIVATE KEY-----\n")
		for range bodyLines {
			v := "KB" + randomFrom(r, alnum, 18+r.IntN(12))
			s.values = append(s.values, v)
			b.WriteString(v + "\n")
		}
		if !s.unfinished {
			b.WriteString("-----END " + "PRIVATE KEY-----")
		}
		s.text = strings.TrimSuffix(b.String(), "\n")
		return s
	case 2:
		v := "sk-" + "ant-" + randomFrom(r, alnum, 24)
		return cutSecret{text: v, values: []string{v}}
	case 3:
		v := "gh" + "p_" + randomFrom(r, alnum, 36)
		return cutSecret{text: v, values: []string{v}}
	case 4:
		v := "AK" + "IA" + randomFrom(r, upper, 16)
		return cutSecret{text: v, values: []string{v}}
	case 5:
		// The value is on the line after the header: only redaction with
		// the context before a cut hides it.
		v := "V" + randomFrom(r, alnum, 23)
		return cutSecret{text: "Authorization: Bearer\n" + v, values: []string{v}}
	default:
		return cutSecret{text: lease, values: []string{lease}}
	}
}

// plainFill returns n bytes of lines no redaction rule changes, ending in
// a newline when n > 0.
func plainFill(r *rand.Rand, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(randomFrom(r, alnum+"   ", r.IntN(40)) + "\n")
	}
	return b.String()[:n-1] + "\n"
}

// checkCut asserts the property of issue 134 on one kept tail: within the
// limit, no byte run of a key body and no other secret value shown, and
// nothing shown from after an unfinished key.
func checkCut(t *testing.T, label, got string, limit int, s cutSecret, after string) {
	t.Helper()
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("%s: "+format+"\nsecret %q\nkept %q", append([]any{label}, append(args, s.text, got)...)...)
	}
	if len(got) > limit {
		fail("kept tail holds %d bytes, limit %d", len(got), limit)
	}
	for _, v := range s.values {
		if !s.key {
			if strings.Contains(got, v) {
				fail("shows secret value %q", v)
			}
			continue
		}
		for i := 0; i+8 <= len(v); i++ {
			if strings.Contains(got, v[i:i+8]) {
				fail("shows key body bytes %q", v[i:i+8])
			}
		}
	}
	if s.unfinished && strings.Contains(got, after) {
		fail("shows output after an unfinished key")
	}
}

// exactClean is the redaction pass of a command stream: the daemon-held
// values by exact match, then the pattern rules.
func exactClean(lease string) func(string) string {
	return func(s string) string {
		return redact.String(strings.ReplaceAll(s, lease, "[REDACTED]"))
	}
}

// TestCutPathsProperty places finished and unfinished private keys, pattern
// tokens, a multi-line header and an exact-match lease at every offset
// around the cut point (redact.Tail, with and without context, with the
// default and the stream cleaner) and around the compaction point of the
// Docker stream (one large write, then random small writes), and checks the
// property of the decision of 2026-11-28 on issue 134: no byte of a key
// body that redaction of the whole output hides appears in the kept tail,
// the tail is within the cap, and overflow is set exactly when a cut
// happened.
func TestCutPathsProperty(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(134, 1128))
	lease := "lease" + randomFrom(r, hexDigits, 24)
	step := 1
	if raceEnabled {
		step = 17 // the race detector slows regular expressions about twentyfold
	}
	t.Run("redact.Tail", func(t *testing.T) {
		const limit = 200
		for kind := range cutKinds {
			s := makeCutSecret(r, kind, 3, lease)
			// Sweep the bytes after the secret so the cut moves from well
			// before its first byte to well after its last.
			for post := limit - len(s.text) - 48; post <= limit+48; post += step {
				after := "AFTER" + randomFrom(r, alnum, 12) + "\n"
				text := plainFill(r, 64+r.IntN(64)) + s.text + "\n" + after + plainFill(r, max(post-len(after), 0))
				for _, mode := range []string{"default", "stream", "context"} {
					if mode == "default" && kind == cutKinds-1 {
						continue // the default cleaner does not hold this lease
					}
					clean, from := (func(string) string)(nil), 0
					switch mode {
					case "stream":
						clean = exactClean(lease)
					case "context":
						clean, from = exactClean(lease), max(len(text)-limit, 1)
					}
					label := fmt.Sprintf("kind %d post %d %s", kind, post, mode)
					got, cut := redact.Tail(text, from, limit, clean)
					checkCut(t, label, got, limit, s, after)
					whole, _ := redact.Tail(text, 0, 1<<30, clean)
					if want := from > 0 || len(whole) > limit; cut != want {
						t.Fatalf("%s: cut = %v, want %v (whole %d bytes)", label, cut, want, len(whole))
					}
				}
			}
		}
	})
	t.Run("stream compaction", func(t *testing.T) {
		const limit, lookback = 256, 128
		const window = limit + lookback
		cases, compacted := 0, 0
		for kind := range cutKinds {
			// Key bodies long enough to run from before the compaction
			// point into the kept tail.
			s := makeCutSecret(r, kind, 2+r.IntN(40), lease)
			pre := plainFill(r, window+64+r.IntN(64))
			after := "AFTER" + randomFrom(r, alnum, 12) + "\n"
			post := after + plainFill(r, window)
			input := pre + s.text + "\n" + post
			for d := -48; d <= len(s.text)+48; d += step {
				for _, mode := range []string{"one write", "small writes"} {
					label := fmt.Sprintf("kind %d drop at %d %s", kind, d, mode)
					out := councilOutput{keepTail: true, limit: limit, lookback: lookback}
					write := func(p string) {
						if _, err := out.Write([]byte(p)); err != nil {
							t.Fatal(err)
						}
					}
					if mode == "one write" {
						// The first write ends window bytes after the drop
						// point, so compaction drops exactly input[:drop].
						drop := len(pre) + d
						k := drop + window
						if k <= 2*window || k >= len(input) {
							continue
						}
						write(input[:k])
						if out.buf.Len() != window {
							t.Fatalf("%s: buffer holds %d bytes after the first write, want %d", label, out.buf.Len(), window)
						}
						rest := input[k:]
						if len(rest) > window {
							rest = rest[:window]
						}
						write(rest)
						input := input[:k+len(rest)]
						checkStream(t, label, &out, input, limit, lease, s, after)
					} else {
						for j := 0; j < len(input); {
							n := min(1+r.IntN(window), len(input)-j)
							write(input[j : j+n])
							j += n
						}
						checkStream(t, label, &out, input, limit, lease, s, after)
					}
					cases++
					if out.buf.Len() < len(input) {
						compacted++
					}
				}
			}
		}
		if compacted*2 < cases {
			t.Fatalf("compaction ran in %d of %d cases, want most", compacted, cases)
		}
	})
}

// checkStream checks one Docker stream after its writes against the
// property, with overflow set exactly when the raw stream or its redacted
// whole is over the limit.
func checkStream(t *testing.T, label string, out *councilOutput, input string, limit int, lease string, s cutSecret, after string) {
	t.Helper()
	got := out.visible("", lease)
	if got != outputLimitMarker {
		checkCut(t, label, got, limit, s, after)
	}
	whole := councilOutput{keepTail: true, limit: 1 << 30}
	_, _ = whole.Write([]byte(input))
	redactedWhole := whole.visible("", lease)
	if want := len(input) > limit || len(redactedWhole) > limit; out.overflow != want {
		t.Fatalf("%s: overflow = %v, want %v", label, out.overflow, want)
	}
}
