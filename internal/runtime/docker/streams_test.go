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
	for _, tc := range []struct{ name, input string }{
		{"split prompt", "reply to me"}, {"split lease", "lease-value"},
		{"credential", "password=sample-secret"}, {"unfinished private key", "-----BEGIN PRIVATE KEY-----\nprivate bytes"},
		{"large line", strings.Repeat("x", maxCouncilOutput+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf councilOutput
			for i := 0; i < len(tc.input); i += 3 {
				_, _ = io.WriteString(&buf, tc.input[i:min(i+3, len(tc.input))])
			}
			got := buf.visible("reply to me", "lease-value")
			if len(got) > maxCouncilOutput || strings.Contains(got, tc.input) || strings.Contains(got, "private bytes") || !strings.Contains(got, "[REDACTED") {
				t.Fatalf("unsafe output, length %d", len(got))
			}
		})
	}
}
