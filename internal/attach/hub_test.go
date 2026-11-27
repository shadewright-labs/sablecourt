package attach

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
)

type nopWriteCloser struct {
	bytes.Buffer
}

func (n *nopWriteCloser) Close() error { return nil }

func TestHubScryRedactionAndTakeoverExclusivity(t *testing.T) {
	hub := NewHub()
	stdin := &nopWriteCloser{}
	hub.Register("task-1", stdin)

	// 1. Write output containing a secret; verify redaction in history and live channel.
	hub.WriteOutput("task-1", []byte("booting agent with key sk-1234567890abcdefghij123456\n"))

	hist, ch, closed, unsub, err := hub.Subscribe("task-1")
	if err != nil || closed {
		t.Fatalf("Subscribe failed: err=%v closed=%v", err, closed)
	}
	defer unsub()

	if strings.Contains(hist, "sk-1234567890") {
		t.Fatalf("expected secret redaction in history, got %q", hist)
	}
	if !strings.Contains(hist, "booting agent") {
		t.Fatalf("expected history text, got %q", hist)
	}

	hub.WriteOutput("task-1", []byte("step 2 running\n"))
	chunk := <-ch
	if chunk != "step 2 running\n" {
		t.Fatalf("unexpected live chunk: %q", chunk)
	}

	// 2. Exclusive Takeover: alice acquires, bob is blocked, alice writes stdin, then detaches.
	if err := hub.AcquireTakeover("task-1", "alice"); err != nil {
		t.Fatalf("alice AcquireTakeover: %v", err)
	}
	if err := hub.AcquireTakeover("task-1", "bob"); !errors.Is(err, ErrTakeoverHeld) {
		t.Fatalf("expected ErrTakeoverHeld for bob, got %v", err)
	}
	if _, err := hub.SendInput("task-1", "alice", []byte("continue\n")); err != nil {
		t.Fatalf("alice SendInput: %v", err)
	}
	if stdin.String() != "continue\n" {
		t.Fatalf("expected stdin to receive 'continue\\n', got %q", stdin.String())
	}

	hub.ReleaseTakeover("task-1", "alice")
	if err := hub.AcquireTakeover("task-1", "bob"); err != nil {
		t.Fatalf("bob AcquireTakeover after release: %v", err)
	}

	// 3. Close terminates subscribers cleanly.
	hub.Close("task-1")
	if _, ok := <-ch; ok {
		t.Fatalf("expected subscriber channel to close on Hub.Close")
	}
}

func TestHubSplitChunkRedactionAndFlushOnClose(t *testing.T) {
	cases := []struct {
		name           string
		chunks         [][]byte
		wantContains   []string
		wantNotContain []string
		wantExact      string
	}{
		{
			name:           "openai key split across chunks with newline",
			chunks:         [][]byte{[]byte("prefix sk-123456"), []byte("7890abcdefghij123456\n")},
			wantContains:   []string{"prefix [REDACTED:openai_key]\n"},
			wantNotContain: []string{"sk-123456", "7890abcdefghij123456"},
			wantExact:      "prefix [REDACTED:openai_key]\n",
		},
		{
			name:           "anthropic key split across three chunks flushed on close",
			chunks:         [][]byte{[]byte("tok sk-ant-"), []byte("1234567890"), []byte("abcdef12")},
			wantContains:   []string{"tok [REDACTED:anthropic_key]"},
			wantNotContain: []string{"sk-ant-", "1234567890", "abcdef12"},
			wantExact:      "tok [REDACTED:anthropic_key]",
		},
		{
			name:           "github token split with trailing partial line",
			chunks:         [][]byte{[]byte("ghp_12345"), []byte("67890abcdefghijklmno\nnext")},
			wantContains:   []string{"[REDACTED:github_token]\nnext"},
			wantNotContain: []string{"ghp_12345", "67890abcdefghijklmno"},
			wantExact:      "[REDACTED:github_token]\nnext",
		},
		{
			name: "multiline pem private key split across chunks",
			chunks: [][]byte{
				[]byte("-----BEGIN" + " PRIVATE KEY-----\nMIIE"),
				[]byte("owIBAAK\n"),
				[]byte("-----END PRIVATE KEY-----\n"),
			},
			wantContains:   []string{"[REDACTED:private_key]\n"},
			wantNotContain: []string{"MIIE", "owIBAAK", "BEGIN PRIVATE KEY"},
			wantExact:      "[REDACTED:private_key]\n",
		},
		{
			name:         "plain text without newline flushed on close",
			chunks:       [][]byte{[]byte("hello "), []byte("world")},
			wantContains: []string{"hello world"},
			wantExact:    "hello world",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := NewHub()
			hub.Register("task-split", nil)

			_, ch, closed, unsub, err := hub.Subscribe("task-split")
			if err != nil || closed {
				t.Fatalf("Subscribe before writes: err=%v closed=%v", err, closed)
			}
			defer unsub()

			for _, chunk := range tc.chunks {
				hub.WriteOutput("task-split", chunk)
			}
			hub.Close("task-split")

			// Writes after Close must be ignored.
			hub.WriteOutput("task-split", []byte("after-close\n"))

			var streamed strings.Builder
			for s := range ch {
				streamed.WriteString(s)
			}
			gotStream := streamed.String()

			hist, postCh, postClosed, postUnsub, err := hub.Subscribe("task-split")
			if err != nil {
				t.Fatalf("Subscribe after close: %v", err)
			}
			defer postUnsub()
			if !postClosed {
				t.Fatalf("expected closed=true after Close")
			}
			if _, ok := <-postCh; ok {
				t.Fatalf("expected closed channel for post-close Subscribe")
			}

			for _, target := range []struct {
				label string
				val   string
			}{
				{label: "stream", val: gotStream},
				{label: "history", val: hist},
			} {
				if target.val != tc.wantExact {
					t.Errorf("%s = %q, want %q", target.label, target.val, tc.wantExact)
				}
				for _, want := range tc.wantContains {
					if !strings.Contains(target.val, want) {
						t.Errorf("%s missing %q in %q", target.label, want, target.val)
					}
				}
				for _, forbid := range tc.wantNotContain {
					if strings.Contains(target.val, forbid) {
						t.Errorf("%s leaked %q in %q", target.label, forbid, target.val)
					}
				}
			}
		})
	}
}

func TestHubStreamsRedactedOutput(t *testing.T) {
	const (
		limit     = 16 * 1024
		pemHeader = "-----BEGIN" + " RSA PRIVATE KEY-----\n"
		pemFooter = "-----END RSA PRIVATE KEY-----"
		pemMarker = "[REDACTED:private_key]"
	)
	longBody := strings.Repeat("synthetic body\n", limit/14+1)
	longToken := strings.Repeat("x", limit)
	cases := []struct {
		name      string
		chunks    []string
		wantLive  []string
		wantClose string
	}{
		{
			name:     "complete PEM at EOF",
			chunks:   []string{pemHeader + "synthetic body\n-----END RSA ", "PRIVATE KEY-----"},
			wantLive: []string{"", pemMarker},
		},
		{
			name:      "unfinished PEM at EOF",
			chunks:    []string{pemHeader + "synthetic body\n"},
			wantLive:  []string{""},
			wantClose: pemMarker,
		},
		{
			name:     "PEM over limit resumes after footer",
			chunks:   []string{pemHeader + longBody, "continued body\n" + pemFooter[:8], pemFooter[8:] + "\nready> "},
			wantLive: []string{"", "", pemMarker + "\nready> "},
		},
		{
			name:      "PEM over limit at EOF",
			chunks:    []string{pemHeader + longBody},
			wantLive:  []string{""},
			wantClose: pemMarker,
		},
		{
			name:     "PEM resumes only at its own footer",
			chunks:   []string{pemHeader + "synthetic body\n-----END EC PRIVATE KEY-----\n", "continued body\n" + pemFooter + "\n"},
			wantLive: []string{"", pemMarker + "\n"},
		},
		{
			name:     "token crossing old size cut",
			chunks:   []string{strings.Repeat(".", limit-512) + "sk-" + strings.Repeat("x", 600), "suffix!"},
			wantLive: []string{strings.Repeat(".", limit-512), "[REDACTED:openai_key]!"},
		},
		{
			name:     "token over limit continues across writes",
			chunks:   []string{"sk-" + longToken, "continued-token", "\nready> "},
			wantLive: []string{"", "", "[REDACTED:openai_key]\nready> "},
		},
		{
			name:     "prompt without newline",
			chunks:   []string{"Continue? ", "[y/N] "},
			wantLive: []string{"Continue? ", "[y/N] "},
		},
		{
			name:     "assignment with split label and oversized value",
			chunks:   []string{"db_pass", "word  \n", "= \"" + longToken, "suffix\" ready> "},
			wantLive: []string{"db_pass", "word  \n", "= \"", "[REDACTED:secret]\" ready> "},
		},
		{
			name:     "bearer header split across lines and writes",
			chunks:   []string{"Authori", "zation:\nBea", "rer \t", "synthetic", "token!"},
			wantLive: []string{"Authori", "zation:\nBea", "rer \t", "", "[REDACTED:bearer_token]!"},
		},
		{
			name:     "bearer label overlaps an assignment value",
			chunks:   []string{"password=Authorization:Bearer ", "syntheticbearer!"},
			wantLive: []string{"password=[REDACTED:secret] ", "[REDACTED:bearer_token]!"},
		},
		{
			name:      "unfinished token at EOF",
			chunks:    []string{"token sk-ant-123"},
			wantLive:  []string{"token "},
			wantClose: "[REDACTED:anthropic_key]",
		},
		{
			name:     "public PEM stays visible",
			chunks:   []string{"-----BE", "GIN CERTIFICATE-----\npublic body\n-----END CERTIFICATE-----\n"},
			wantLive: []string{"", "-----BEGIN CERTIFICATE-----\npublic body\n-----END CERTIFICATE-----\n"},
		},
		{
			name:      "ambiguous fragment becomes visible on close",
			chunks:    []string{"hello ", "sk"},
			wantLive:  []string{"hello ", ""},
			wantClose: "sk",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := NewHub()
			const task = "task-stream"
			hub.Register(task, nil)
			_, ch, _, unsubscribe, err := hub.Subscribe(task)
			if err != nil {
				t.Fatal(err)
			}
			defer unsubscribe()
			var expected strings.Builder
			for i, chunk := range tc.chunks {
				hub.WriteOutput(task, []byte(chunk))
				var live strings.Builder
				for len(ch) > 0 {
					live.WriteString(<-ch)
				}
				want := tc.wantLive[i]
				if live.String() != want {
					t.Errorf("write %d live output differs from expected (%d bytes, want %d)", i+1, live.Len(), len(want))
				}
				expected.WriteString(want)
				history, _, _, unsub, err := hub.Subscribe(task)
				if err != nil {
					t.Fatal(err)
				}
				unsub()
				if history != expected.String() {
					t.Errorf("write %d history differs from expected (%d bytes, want %d)", i+1, len(history), expected.Len())
				}
			}
			hub.Close(task)
			var final strings.Builder
			for chunk := range ch {
				final.WriteString(chunk)
			}
			if final.String() != tc.wantClose {
				t.Errorf("close output differs from expected (%d bytes, want %d)", final.Len(), len(tc.wantClose))
			}
			expected.WriteString(tc.wantClose)
			history, _, _, unsub, err := hub.Subscribe(task)
			if err != nil {
				t.Fatal(err)
			}
			defer unsub()
			if history != expected.String() {
				t.Errorf("closed history differs from expected (%d bytes, want %d)", len(history), expected.Len())
			}
		})
	}
}

func TestStreamRedactorPreservesCredentialRulesAcrossWrites(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"task token", "task=sc_task_0123456789abcdef!", "task=[REDACTED:sablecourt_token]!"},
		{"operator token", "op=op_" + strings.Repeat("0123456789abcdef", 4) + "!", "op=[REDACTED:sablecourt_token]!"},
		{"operator_token assignment", "operator_token=synthetic", "operator_token=[REDACTED:secret]"},
		{"Anthropic", "key sk-ant-synthetic1234567!", "key [REDACTED:anthropic_key]!"},
		{"OpenAI project", "key sk-proj-synthetic1234567890!", "key [REDACTED:openai_key]!"},
		{"GitHub PAT", "key github_pat_synthetic1234567890x!", "key [REDACTED:github_token]!"},
		{"Google", "key Aiza AIzaSynthetic1234567890x!", "key Aiza [REDACTED:google_api_key]!"},
		{"AWS", "key ASIASYNTHETIC1234567!", "key [REDACTED:aws_access_key]!"},
		{"bearer", "AUTHORIZATION \n: bEaReR\t synthetic.token!", "AUTHORIZATION \n: bEaReR\t [REDACTED:bearer_token]!"},
		{"assignment", "test_api-key\n= 'synthetic'", "test_api-key\n= '[REDACTED:secret]'"},
		{"PEM", "-----BEGIN" + " EC PRIVATE KEY-----\nsynthetic\n-----END EC PRIVATE KEY-----!", "[REDACTED:private_key]!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range len(tc.input) + 1 {
				var stream streamRedactor
				got := stream.write([]byte(tc.input[:i]), false) + stream.write([]byte(tc.input[i:]), false) + stream.write(nil, true)
				if got != tc.want {
					t.Fatalf("split at byte %d: got %q, want %q", i, got, tc.want)
				}
			}
		})
	}
}

func TestHubRedactsExactKnownSecretsAcrossChunksAndCRLF(t *testing.T) {
	reg := redact.NewRegistry()
	secret := strings.Join([]string{"hub", "_opaque_", "exact_778899"}, "")
	reg.Register(secret)

	for _, tc := range []struct {
		name   string
		chunks []string
		forbid []string
	}{
		{
			name:   "raw split across chunks",
			chunks: []string{"pre " + secret[:7], secret[7:] + " post\n"},
			forbid: []string{secret, secret[:7], secret[7:]},
		},
		{
			name:   "split by CRLF across chunks",
			chunks: []string{"pre " + secret[:9] + "\r\n", secret[9:] + " post\n"},
			forbid: []string{secret, secret[:9], secret[9:]},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := NewHubWithRegistry(reg)
			hub.Register("task-exact", nil)
			_, ch, _, unsub, err := hub.Subscribe("task-exact")
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer unsub()
			for _, c := range tc.chunks {
				hub.WriteOutput("task-exact", []byte(c))
			}
			hub.Close("task-exact")
			var live strings.Builder
			for s := range ch {
				live.WriteString(s)
			}
			hist, _, _, unsub2, err := hub.Subscribe("task-exact")
			if err != nil {
				t.Fatalf("Subscribe after close: %v", err)
			}
			unsub2()
			for _, got := range []string{live.String(), hist} {
				if !strings.Contains(got, redact.KnownSecretMarker) {
					t.Fatalf("expected marker in %q", got)
				}
				for _, f := range tc.forbid {
					if len(f) >= redact.MinSecretLength && strings.Contains(got, f) {
						t.Fatalf("leaked %q in %q", f, got)
					}
				}
			}
		})
	}
}

// Third review: Subscribe returned the ring without rechecking it, so a value
// registered after its output was buffered was replayed, and a slow
// subscriber lost chunks silently, so the viewer joined chunks the redactor
// never saw side by side.
func TestScryReplayRechecksRingAndMarksGaps(t *testing.T) {
	reg := redact.NewRegistry()
	hub := NewHubWithRegistry(reg)
	hub.Register("task", nil)
	later := "scry_" + "later_" + strings.Repeat("h4", 10)
	hub.WriteOutput("task", []byte("before "+later+" after\n"))
	reg.Register(later)
	history, ch, closed, unsub, err := hub.Subscribe("task")
	if err != nil || closed {
		t.Fatalf("subscribe: %v %v", err, closed)
	}
	if strings.Contains(history, later) || !strings.Contains(history, "before ") {
		t.Fatalf("replay = %q", history)
	}

	// Fill the subscriber's channel, then write more: the missed chunks are
	// marked before the next chunk it receives.
	for i := range cap(ch) + 5 {
		hub.WriteOutput("task", []byte(fmt.Sprintf("line %d\n", i)))
	}
	var got []string
	for range cap(ch) {
		got = append(got, <-ch)
	}
	hub.WriteOutput("task", []byte("resumed\n"))
	for range 2 {
		select {
		case c := <-ch:
			got = append(got, c)
		case <-time.After(2 * time.Second):
			t.Fatalf("expected the gap marker and the next chunk, got %q", got[len(got)-2:])
		}
	}
	unsub()
	if got[len(got)-2] != GapMarker || got[len(got)-1] != "resumed\n" {
		t.Fatalf("missed chunks were not marked: last chunks %q", got[len(got)-3:])
	}
}

// Fix round: Scry output ran pattern rules after the exact stream and did not
// check their output again, so a pattern marker next to other text could
// rebuild a held value. Every split is tried.
func TestScryPatternMarkerNeverRebuildsHeldValue(t *testing.T) {
	reg := redact.NewRegistry()
	held := "bearer_token]]tail_value"
	reg.Register(held)
	doc := "Authorization: Bearer abcdefghij]tail_value\n"
	for cut := 0; cut <= len(doc); cut++ {
		r := newStreamRedactor(reg)
		out := r.write([]byte(doc[:cut]), false) + r.write([]byte(doc[cut:]), true)
		if strings.Contains(out, held) {
			t.Fatalf("split at %d emitted %q", cut, out)
		}
	}
}
