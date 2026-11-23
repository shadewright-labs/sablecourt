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
