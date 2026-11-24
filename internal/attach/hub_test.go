package attach

import (
	"bytes"
	"errors"
	"strings"
	"testing"
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
				[]byte("-----BEGIN PRIVATE KEY-----\nMIIE"),
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
