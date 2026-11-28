package docker

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

// placedSecret is a secret of one redaction rule kind and the value inside
// it that must never be shown unless redaction of the whole stream shows it.
type placedSecret struct {
	text, value string
}

func randomFrom(r *rand.Rand, alphabet string, n int) string {
	var b strings.Builder
	for range n {
		b.WriteByte(alphabet[r.IntN(len(alphabet))])
	}
	return b.String()
}

const (
	alnum     = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	upper     = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	hexDigits = "0123456789abcdef"
)

// secretOfKind builds one secret per rule kind of internal/redact, with
// key-shaped literals split so that no tracked file holds one.
func secretOfKind(r *rand.Rand, kind int) placedSecret {
	val := "V" + randomFrom(r, alnum, 23)
	seps := []string{" ", "\n", " \n  ", "\t"}
	sep := seps[r.IntN(len(seps))]
	switch kind {
	case 0:
		body := "keybody" + randomFrom(r, alnum, 20)
		s := placedSecret{text: "-----BEGIN " + "PRIVATE KEY-----\n" + body + "\n" + randomFrom(r, alnum, 30) + "\n", value: body}
		if r.IntN(3) > 0 { // otherwise an unfinished key
			s.text += "-----END " + "PRIVATE KEY-----"
		}
		return s
	case 1:
		v := "sc_" + "task_" + randomFrom(r, hexDigits, 24)
		return placedSecret{text: v, value: v}
	case 2:
		v := "sk-" + "ant-" + randomFrom(r, alnum, 24)
		return placedSecret{text: v, value: v}
	case 3:
		v := "sk-" + "proj-" + randomFrom(r, alnum, 24)
		return placedSecret{text: v, value: v}
	case 4:
		v := "gh" + "p_" + randomFrom(r, alnum, 36)
		return placedSecret{text: v, value: v}
	case 5:
		v := "AI" + "za" + randomFrom(r, alnum, 24)
		return placedSecret{text: v, value: v}
	case 6:
		v := "AK" + "IA" + randomFrom(r, upper, 16)
		return placedSecret{text: v, value: v}
	case 7:
		return placedSecret{text: "Authorization:" + sep + "Bearer" + sep + val, value: val}
	case 8:
		value := val
		if r.IntN(2) == 0 {
			value = "first\n" + val + "\nlast"
		}
		return placedSecret{text: `"pass` + `word"` + sep + ":" + sep + `"` + value + `"`, value: val}
	default:
		ops := []string{"=", ":", ": ", ":\n", " =\n"}
		return placedSecret{text: "api" + "_key" + ops[r.IntN(len(ops))] + val, value: val}
	}
}

// fillerLine returns a line of text that may hold multi-byte runes, or a
// line whose redaction grows it.
func fillerLine(r *rand.Rand) string {
	switch r.IntN(5) {
	case 0:
		return "pass" + "word=123456\n"
	case 1:
		return strings.Repeat("é", r.IntN(12)) + "日本" + randomFrom(r, alnum, r.IntN(8)) + "\n"
	default:
		return randomFrom(r, alnum+"  ", r.IntN(40)) + "\n"
	}
}

func fill(r *rand.Rand, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(fillerLine(r))
	}
	return b.String()[:n]
}

// TestCommandOutputTailProperty writes generated streams with secrets of
// every rule kind at and around the cut, word characters glued before
// tokens, multi-byte runes at the boundary and lines that redaction grows,
// and checks the visible tail: within the limit, valid UTF-8 when the input
// is, unchanged by another redaction pass, showing no secret value that
// redaction of the whole stream hides, and flagged as overflowed exactly
// when the raw stream or its redacted whole is over the limit. Most streams
// are long enough for Write to compact them.
func TestCommandOutputTailProperty(t *testing.T) {
	t.Parallel()
	const limit, lookback = 1024, 512
	r := rand.New(rand.NewPCG(134, 227))
	// The race detector slows regular expressions about twentyfold.
	cases := 600
	if raceEnabled {
		cases = 40
	}
	const window = limit + lookback
	// Totals up to six windows and writes up to twice the window, so most
	// streams are compacted in Write, some by a single write past the
	// window, and some fit.
	compacted, fits := 0, 0
	for i := range cases {
		total := limit/2 + r.IntN(6*window)
		cutAt := max(total-limit, 0)
		var secrets []placedSecret
		var b strings.Builder
		for b.Len() < total {
			// Put a secret around the cut half the time, filler otherwise.
			if r.IntN(2) == 0 && len(secrets) < 6 {
				s := secretOfKind(r, r.IntN(10))
				// Start it anywhere from well before the cut to just after.
				gap := cutAt - b.Len() - len(s.text) + r.IntN(len(s.text)+64) - 32
				if gap > 0 && gap < lookback/2 {
					b.WriteString(fill(r, gap))
				}
				if r.IntN(4) == 0 {
					b.WriteString("x") // a word character that hides a token
				}
				b.WriteString(s.text)
				b.WriteString("\n")
				secrets = append(secrets, s)
				continue
			}
			b.WriteString(fillerLine(r))
		}
		input := b.String()
		out := councilOutput{keepTail: true, limit: limit, lookback: lookback}
		for j := 0; j < len(input); {
			n := 1 + r.IntN(300)
			if r.IntN(8) == 0 {
				n = 1 + r.IntN(2*window)
			}
			n = min(n, len(input)-j)
			if _, err := out.Write([]byte(input[j : j+n])); err != nil {
				t.Fatal(err)
			}
			j += n
		}
		if out.buf.Len() < len(input) {
			compacted++
		} else if len(input) <= limit {
			fits++
		}
		got := out.visible("", "")
		whole := councilOutput{keepTail: true, limit: 1 << 30}
		_, _ = whole.Write([]byte(input))
		redactedWhole := whole.visible("", "")
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("case %d: "+format+"\ninput %q\nvisible %q", append([]any{i}, append(args, input, got)...)...)
		}
		if len(got) > limit {
			fail("visible holds %d bytes", len(got))
		}
		if utf8.ValidString(input) && !utf8.ValidString(got) {
			fail("visible is not valid UTF-8")
		}
		if got != outputLimitMarker && !strings.HasSuffix(redactedWhole, got) {
			fail("visible is not a suffix of the redacted whole: compaction kept the wrong bytes")
		}
		if got != outputLimitMarker {
			again := councilOutput{keepTail: true, limit: 1 << 30}
			_, _ = again.Write([]byte(got))
			if again.visible("", "") != got {
				fail("another redaction pass changes the visible output")
			}
		}
		for _, s := range secrets {
			if !strings.Contains(redactedWhole, s.value) && strings.Contains(got, s.value) {
				fail("visible shows secret value %q", s.value)
			}
		}
		if got != redactedWhole && !out.overflow {
			fail("visible differs from the redacted whole without overflow")
		}
		if want := len(input) > limit || len(redactedWhole) > limit; out.overflow != want {
			fail("overflow = %v, want %v", out.overflow, want)
		}
	}
	if compacted*2 < cases || fits == 0 {
		t.Fatalf("compacted %d and fitting %d of %d streams, want most compacted and some fitting", compacted, fits, cases)
	}
}
