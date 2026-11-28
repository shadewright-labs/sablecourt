package docker

import (
	"context"
	"strings"
	"testing"

	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// TestCommandOutputMarkerTextIsNotOverflow: output that equals the limit
// marker but was never cut is not an overflow. Overflow comes only from a
// recorded cut.
func TestCommandOutputMarkerTextIsNotOverflow(t *testing.T) {
	for name, out := range map[string][]byte{
		"stdout": frame(1, outputLimitMarker),
		"stderr": frame(2, outputLimitMarker),
	} {
		t.Run(name, func(t *testing.T) {
			a, f, s := fixtureRun(t)
			s.Prompt, s.Model, s.Command = "", "", []string{"make", "check"}
			f.output = out
			var got rt.CouncilResult
			if _, err := a.RunCouncil(t.Context(), s, func(_ context.Context, r rt.CouncilResult) error { got = r; return nil }); err != nil {
				t.Fatal(err)
			}
			if got.OutputOverflow {
				t.Fatalf("OutputOverflow = true for uncut output %q %q", got.Stdout, got.Stderr)
			}
		})
	}
}

// TestCommandOutputCompactionKeepsUnfinishedKeyHidden: a private key BEGIN
// line that compaction drops before redaction still hides the key body
// after it, up to its END line.
func TestCommandOutputCompactionKeepsUnfinishedKeyHidden(t *testing.T) {
	t.Parallel()
	begin := "-----BEGIN " + "PRIVATE KEY-----\n"
	end := "-----END " + "PRIVATE KEY-----\n"
	body := strings.Repeat("keybody-secret-line\n", 1000) // 20000 bytes
	const limit, lookback = 4096, 1024
	for name, tc := range map[string]struct {
		input string
		chunk int
		after string // must be shown
	}{
		"one write past the window":  {input: begin + body, chunk: 1 << 20},
		"small writes, compaction":   {input: begin + body, chunk: 100},
		"BEGIN split across writes":  {input: begin + body, chunk: 7},
		"END after the compaction":   {input: begin + body + end + "visible-after\n", chunk: 333, after: "visible-after"},
		"END and BEGIN both dropped": {input: begin + "x\n" + end + begin + body, chunk: 512},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := councilOutput{keepTail: true, limit: limit, lookback: lookback}
			for j := 0; j < len(tc.input); j += tc.chunk {
				if _, err := out.Write([]byte(tc.input[j:min(j+tc.chunk, len(tc.input))])); err != nil {
					t.Fatal(err)
				}
			}
			got := out.visible("", "")
			if strings.Contains(got, "keybody") || strings.Contains(got, "secret-line") {
				t.Fatalf("unfinished key body shown after compaction: %.80q", got)
			}
			if !out.overflow || len(got) > limit || !strings.Contains(got, tc.after) {
				t.Fatalf("visible = %.80q (overflow %v), want within %d bytes with %q", got, out.overflow, limit, tc.after)
			}
		})
	}
}
