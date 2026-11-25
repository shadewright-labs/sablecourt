package attach

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/redact/redacttest"
)

func TestStreamRedactorRedactsGeneratedCorpusAtEverySplit(t *testing.T) {
	for _, tc := range redacttest.Corpus() {
		t.Run(tc.Name, func(t *testing.T) {
			for i := range len(tc.Text) + 1 {
				var stream streamRedactor
				out := stream.write([]byte(tc.Text[:i]), false) + stream.write([]byte(tc.Text[i:]), false) + stream.write(nil, true)
				if frag, ok := redacttest.Leak(out, tc.Secrets); ok {
					t.Fatalf("split at byte %d: secret fragment %q streamed: %q", i, frag, out)
				}
			}
		})
	}
}

func TestHubRedactsJSONQuotedPasswordLine(t *testing.T) {
	hub := NewHub()
	hub.Register("task-json", nil)
	hub.WriteOutput("task-json", []byte(`{"password":"hunter2hunter2"}`+"\n"))
	history, _, _, unsub, err := hub.Subscribe("task-json")
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	if frag, ok := redacttest.Leak(history, []string{"hunter2hunter2"}); ok {
		t.Fatalf("password fragment %q streamed: %q", frag, history)
	}
}

func TestStreamRedactorKeyAndEscapeCasesAtEverySplit(t *testing.T) {
	for _, tc := range append(redacttest.KeyCases(), redacttest.StreamCases()...) {
		t.Run(tc.Name, func(t *testing.T) {
			for i := range len(tc.Text) + 1 {
				var stream streamRedactor
				out := stream.write([]byte(tc.Text[:i]), false) + stream.write([]byte(tc.Text[i:]), false) + stream.write(nil, true)
				if frag, ok := redacttest.Leak(out, tc.Secrets); ok {
					t.Fatalf("split at byte %d: secret fragment %q streamed: %q", i, frag, out)
				}
			}
		})
	}
}

// TestStreamRedactorKeepsNonSecretEscapesAndANSI checks that normalising
// ANSI sequences and unicode escapes for matching leaves ordinary output byte
// for byte as it was, at every split.
func TestStreamRedactorKeepsNonSecretEscapesAndANSI(t *testing.T) {
	for _, text := range []string{
		"\x1b[1;32mok\x1b[0m done\n",
		`{"note":"café ABC \\ \u12"}` + "\n",
		"prompt> \x1b[",
		"trailing backslash \\",
		"sk\x1b[0m-short and gh\\u0070_ too",
		"Authorization: required for\n",
		"\x1b[" + strings.Repeat("1;", 40) + "m long sequence",
	} {
		for i := range len(text) + 1 {
			var stream streamRedactor
			got := stream.write([]byte(text[:i]), false) + stream.write([]byte(text[i:]), false) + stream.write(nil, true)
			if got != text {
				t.Fatalf("split at byte %d: got %q, want %q", i, got, text)
			}
		}
	}
}
