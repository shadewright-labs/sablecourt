package docker

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func frame(stream byte, text string) []byte {
	buf := make([]byte, 8, 8+len(text))
	buf[0] = stream
	binary.BigEndian.PutUint32(buf[4:], uint32(len(text)))
	return append(buf, text...)
}

func TestDockerStreams(t *testing.T) {
	frames := append(frame(1, "answer"), frame(2, "diagnostic")...)
	for _, tc := range []struct {
		name       string
		input      []byte
		split, bad bool
	}{
		{"coalesced", frames, false, false}, {"split", frames, true, false},
		{"short header", frames[:5], true, true}, {"short body", frames[:len(frames)-1], false, true},
		{"reserved byte", []byte{1, 1, 0, 0, 0, 0, 0, 0}, false, true}, {"system error", frame(3, "failure"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var input io.Reader = bytes.NewReader(tc.input)
			if tc.split {
				input = iotest.OneByteReader(input)
			}
			var out, diag bytes.Buffer
			err := copyDockerStreams(input, &out, &diag)
			if (err != nil) != tc.bad || !tc.bad && (out.String() != "answer" || diag.String() != "diagnostic") {
				t.Fatalf("out=%q stderr=%q error=%v", out.String(), diag.String(), err)
			}
		})
	}
}

func TestCouncilVisibleOutput(t *testing.T) {
	for _, tc := range []struct {
		name, prompt, lease, input string
	}{
		{"split prompt", "reply to me", "lease-value", "reply to me"},
		{"split lease", "reply to me", "lease-value", "lease-value"},
		{"prompt substring of lease", "lease", "lease-value", "lease-value"},
		{"prompt overlaps lease prefix", "ask lease-", "lease-value", "ask lease-value"},
		{"prompt overlaps lease suffix", "value done", "lease-value", "lease-value done"},
		{"credential", "reply to me", "lease-value", "password=sample-secret"},
		{"unfinished private key", "reply to me", "lease-value", "-----BEGIN PRIVATE KEY-----\nprivate bytes"},
		{"large line", "reply to me", "lease-value", strings.Repeat("x", maxCouncilOutput+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf councilOutput
			for i := 0; i < len(tc.input); i += 3 {
				_, _ = io.WriteString(&buf, tc.input[i:min(i+3, len(tc.input))])
			}
			got := buf.visible(tc.prompt, tc.lease)
			if len(got) > maxCouncilOutput || strings.Contains(got, tc.input) || strings.Contains(got, tc.lease) || strings.Contains(got, "lease") || strings.Contains(got, "value") || strings.Contains(got, "private bytes") || !strings.Contains(got, "[REDACTED") {
				t.Fatalf("unsafe output %q, length %d", got, len(got))
			}
		})
	}
}

// TestCommandOutputKeepsTail checks a command stream past the runner's limit:
// it keeps its last whole lines, redacted, within the limit, and drops a
// line or a private key that the cut fell inside.
func TestCommandOutputKeepsTail(t *testing.T) {
	t.Parallel()
	// A small limit keeps the test fast under the race detector; the
	// order of redaction and cuts does not depend on the size.
	const limit, lookback = 4096, 1024
	lease := "lease-" + strings.Repeat("L", 20)
	github := "gh" + "p_" + strings.Repeat("G", 36)
	filler := strings.Repeat("f", 99) + "\n"
	fill := func(n int) string { return strings.Repeat(filler, n/len(filler)+1) }
	// cutInside puts secret on the first line so that the kept tail starts
	// k bytes into it.
	cutInside := func(secret string, k int) string {
		rest := limit - (len(secret) - k) - 1
		return secret + "\n" + strings.Repeat("f", rest-4) + "\nend"
	}
	key := "-----BEGIN " + "PRIVATE KEY-----\nkeybody-aaaa\nkeybody-bbbb\n-----END " + "PRIVATE KEY-----\n"
	for name, tc := range map[string]struct {
		input  string
		want   string // suffix of the visible output; empty: the marker
		absent []string
	}{
		"final diagnostic survives": {input: fill(limit) + "FAIL: TestX\n", want: "FAIL: TestX"},
		"one huge write":            {input: strings.Repeat("y", 3*limit) + "\nlast line", want: "last line"},
		"no whole line kept":        {input: strings.Repeat("y", 2*limit)},
		// The cut falls inside the token, so its tail would match no rule.
		"token cut at the start": {input: cutInside("x"+github, 8), want: "end", absent: []string{"GGGG"}},
		"lease cut at the start": {input: cutInside(lease, 8), want: "end", absent: []string{"LLLL"}},
		"cut inside a private key": {
			input: key + strings.Repeat("k", limit-40) + "\n" + "keybody-cccc\n-----END " + "PRIVATE KEY-----\ndone\n",
			want:  "done", absent: []string{"keybody"},
		},
		"secret in the tail":     {input: fill(limit) + "token " + github + "\n", want: "[REDACTED", absent: []string{github}},
		"key begins in the tail": {input: fill(limit) + key, want: "[REDACTED:private_key]", absent: []string{"keybody"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := councilOutput{keepTail: true, limit: limit, lookback: lookback}
			for i := 0; i < len(tc.input); i += 32 * 1024 {
				chunk := tc.input[i:min(i+32*1024, len(tc.input))]
				if name == "one huge write" {
					chunk = tc.input[i:]
				}
				if n, err := out.Write([]byte(chunk)); err != nil || n != len(chunk) {
					t.Fatalf("write n=%d err=%v", n, err)
				}
				if out.buf.Len() > 2*(limit+lookback) {
					t.Fatalf("buffer holds %d bytes", out.buf.Len())
				}
				if name == "one huge write" {
					break
				}
			}
			got := out.visible("", lease)
			if !out.overflow || len(got) > limit {
				t.Fatalf("overflow %v, %d bytes", out.overflow, len(got))
			}
			if tc.want == "" {
				if got != outputLimitMarker {
					t.Fatalf("visible = %.80q, want the marker", got)
				}
				return
			}
			if !strings.HasSuffix(strings.TrimSpace(got), tc.want) && !strings.Contains(got, tc.want) {
				t.Fatalf("visible ends %q, want %q", got[max(0, len(got)-80):], tc.want)
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Fatalf("visible holds %q", a)
				}
			}
		})
	}
}

// TestCommandOutputKeepsTailAtFullSize runs one stream past the real limit.
// It redacts about 1 MiB several times, so it is skipped under the race
// detector, where that takes many seconds.
func TestCommandOutputKeepsTailAtFullSize(t *testing.T) {
	if raceEnabled {
		t.Skip("slow under the race detector")
	}
	t.Parallel()
	// The cut falls 4 bytes into the header; the token on the next line is
	// redacted with the header as context.
	secret := "SUPERSECRETVALUE12345"
	head, tail := "Authorization: Bearer\n"+secret+"\n", "\nFAIL: TestX\n"
	input := head + strings.Repeat("f", maxCouncilOutput+4-len(head)-len(tail)) + tail
	out := councilOutput{keepTail: true}
	for i := 0; i < len(input); i += 32 * 1024 {
		_, _ = out.Write([]byte(input[i:min(i+32*1024, len(input))]))
	}
	got := out.visible("", "")
	if !out.overflow || len(got) > maxCouncilOutput || strings.Contains(got, secret) || !strings.HasSuffix(got, "\nFAIL: TestX\n") {
		t.Fatalf("overflow %v, %d bytes, ends %q", out.overflow, len(got), got[max(0, len(got)-40):])
	}
}
