package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/waitart"
)

// sharedTerm stands for one terminal that stdout and stderr both write to.
type sharedTerm struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *sharedTerm) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(b)
}

func (s *sharedTerm) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func fakeTerminalArt() waitart.Config {
	return waitart.Config{
		LookupEnv: func(string) (string, bool) { return "", false },
		Terminal:  func(io.Writer) (int, bool) { return 100, true },
	}
}

// councilDaemon answers like a daemon running a one-seat council: a round in
// progress for the first polls, then a completed council with a digest.
func councilDaemon(t *testing.T, pollsBeforeDone int) http.Handler {
	t.Helper()
	var (
		mu    sync.Mutex
		polls int
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/councils":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ccl-art","title":"q","state":"active"}`))
		case r.URL.Path == "/api/v1/conclaves/ccl-art/stream":
			http.Error(w, "no stream", http.StatusServiceUnavailable)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-art":
			mu.Lock()
			polls++
			n := polls
			mu.Unlock()
			if n <= pollsBeforeDone {
				_, _ = w.Write([]byte(`{"conclave":{"ID":"ccl-art","State":"active"},
					"messages":[{"id":1,"fromRole":"claude","kind":"proposal","body":"First answer.","round":1}],
					"turns":[{"round":1,"role":"claude","status":"completed"},{"round":1,"role":"codex","status":"running"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"conclave":{"ID":"ccl-art","State":"completed"},
				"messages":[{"id":1,"fromRole":"claude","kind":"proposal","body":"First answer.","round":1},
					{"id":2,"fromRole":"codex","kind":"review","body":"Second answer.","round":1}],
				"relics":[{"Name":"digest","AuthorRole":"system","Body":"stop reason: nothing_to_add"}],
				"turns":[{"round":1,"role":"claude","status":"completed"},{"round":1,"role":"codex","status":"completed"}]}`))
		default:
			http.NotFound(w, r)
		}
	})
}

// TestCouncilWaitArt runs a council with stdout and stderr on one fake
// terminal: the scene plays with the round state as its status, every
// council line arrives whole, and the scene is gone before the digest.
func TestCouncilWaitArt(t *testing.T) {
	repoDir := t.TempDir()
	var term sharedTerm
	err := runCouncilSession(context.Background(), newTestClientFactory(councilDaemon(t, 6)), &term, councilRunParams{
		Question:  "q",
		Repo:      repoDir,
		Rounds:    2,
		Harnesses: []string{"claude"},
		PollEvery: 20 * time.Millisecond,
		ErrOut:    &term,
		Art:       fakeTerminalArt(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := term.String()
	for _, want := range []string{
		"\n--- Round 1 ---",
		"@claude", "(proposal, #1)", "\nFirst answer.\n",
		"@codex", "(review, #2)", "\nSecond answer.\n",
		"Council in session",
		"round 1 of 2, 1 of 2 seats answered",
	} {
		if !strings.Contains(stream, want) {
			t.Errorf("terminal output lacks %q", want)
		}
	}
	shown := strings.LastIndex(stream, "\x1b[?25h")
	digest := strings.Index(stream, "=== Digest ===")
	if shown < 0 || digest < 0 || shown > digest {
		t.Fatalf("the scene was not cleared before the digest (cursor shown at %d, digest at %d)", shown, digest)
	}
	if strings.Contains(stream[digest:], "\x1b[7A") {
		t.Fatal("a frame was drawn after the digest started")
	}
}

// TestCouncilWithoutTerminalDrawsNothing keeps piped output clean.
func TestCouncilWithoutTerminalDrawsNothing(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runCouncilSession(context.Background(), newTestClientFactory(councilDaemon(t, 2)), &out, councilRunParams{
		Question:  "q",
		Repo:      t.TempDir(),
		Rounds:    2,
		Harnesses: []string{"claude"},
		PollEvery: 5 * time.Millisecond,
		ErrOut:    &errOut,
	})
	if err != nil {
		t.Fatal(err)
	}
	if errOut.Len() != 0 || strings.Contains(out.String(), "\x1b[?25") {
		t.Fatalf("art drawn without a terminal: stderr %q", errOut.String())
	}
}

func TestNoArtFlagReachesSubcommands(t *testing.T) {
	root := newRootCmd()
	for _, path := range [][]string{{"council"}, {"harness", "build"}, {"approval", "approve"}, {"auth", "github"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.ParseFlags([]string{"--no-art"}); err != nil {
			t.Fatalf("%v: %v", path, err)
		}
		if !artConfig(cmd).NoArt {
			t.Errorf("%v: --no-art did not reach the command", path)
		}
	}
}

func TestCouncilStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    councilDetailResponse
		want string
	}{
		{"nothing yet", councilDetailResponse{}, "Council in session: waiting for the first answers"},
		{"messages only", councilDetailResponse{Messages: []councilMessageView{{Round: 2}}}, "Council in session: round 2 of 3"},
		{"turns", councilDetailResponse{Turns: []councilTurnView{
			{Round: 1, Status: "completed"}, {Round: 2, Status: "completed"}, {Round: 2, State: "working"}, {Round: 2, Status: "running"},
		}}, "Council in session: round 2 of 3, 1 of 3 seats answered"},
	} {
		if got := councilStatus(3, tc.d); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
