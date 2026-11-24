package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func installStubHarnesses(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		p := filepath.Join(dir, name)
		body := "#!/bin/sh\necho \"stub " + name + "\"\n"
		if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

func TestCouncilValidationFailsClosed(t *testing.T) {
	validRepo := t.TempDir()
	called := 0
	rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		http.Error(w, "daemon should not be called", http.StatusInternalServerError)
	})}

	cases := []struct {
		name      string
		harnesses []string
		args      []string
		stdin     string
	}{
		{
			name:      "empty_question_arg",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "   ", "--repo", validRepo},
		},
		{
			name:      "missing_question_empty_stdin",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "--repo", validRepo},
			stdin:     "",
		},
		{
			name:      "oversized_question",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", strings.Repeat("x", (64<<10)+1), "--repo", validRepo},
		},
		{
			name:      "missing_default_harness_agy",
			harnesses: []string{"claude", "codex"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo},
		},
		{
			name:      "unknown_harness",
			harnesses: []string{"claude", "codex", "agy", "bash"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--with", "claude,bash"},
		},
		{
			name:      "empty_with",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--with", ""},
		},
		{
			name:      "duplicate_with",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--with", "claude,claude"},
		},
		{
			name:      "zero_rounds",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--rounds", "0"},
		},
		{
			name:      "negative_rounds",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--rounds", "-2"},
		},
		{
			name:      "ref_and_live_mutually_exclusive",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--ref", "HEAD", "--live"},
		},
		{
			name:      "empty_ref",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--ref", "  "},
		},
		{
			name:      "dash_ref_rejected",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--ref", "--exec=evil"},
		},
		{
			name:      "nonexistent_repo",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "How does auth work?", "--repo", filepath.Join(validRepo, "does-not-exist")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = 0
			installStubHarnesses(t, tc.harnesses...)
			root := newRootCmdWithTransport(rt)
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetIn(strings.NewReader(tc.stdin))
			root.SetArgs(append([]string{"--api", "http://unix"}, tc.args...))

			err := root.ExecuteContext(context.Background())
			if err == nil {
				t.Fatalf("expected validation error for %s, got nil (stdout=%q)", tc.name, stdout.String())
			}
			if strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("command missing: %v", err)
			}
			if called != 0 {
				t.Fatalf("expected 0 HTTP calls when validation fails, got %d", called)
			}
		})
	}
}

func TestCouncilWithStubCLIsPrintsRoundsAndDigest(t *testing.T) {
	installStubHarnesses(t, "claude", "codex", "agy")
	repoDir := t.TempDir()

	type msg struct {
		ID         int64  `json:"id"`
		ConclaveID string `json:"conclaveId"`
		FromRole   string `json:"fromRole"`
		ToRole     string `json:"toRole"`
		Kind       string `json:"kind"`
		Body       string `json:"body"`
		Round      int    `json:"round,omitempty"`
	}
	type relic struct {
		Name        string `json:"name"`
		AuthorRole  string `json:"authorRole"`
		ContentType string `json:"contentType"`
		Body        string `json:"body"`
		SHA256      string `json:"sha256"`
	}

	var (
		mu       sync.Mutex
		openBody map[string]any
		getCount int
	)

	rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/conclaves":
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&openBody)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ccl-council-1","templateName":"council","title":"How is auth tested?","state":"active"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-council-1/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "event: state\ndata: active:4:4:0\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(20 * time.Millisecond)
			_, _ = fmt.Fprint(w, "event: state\ndata: completed:7:7:1\n\n")
			if fl != nil {
				fl.Flush()
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-council-1":
			mu.Lock()
			getCount++
			n := getCount
			mu.Unlock()

			msgs := []msg{
				{ID: 1, ConclaveID: "ccl-council-1", FromRole: "operator", ToRole: "all", Kind: "decree", Body: "How is auth tested?"},
				{ID: 2, ConclaveID: "ccl-council-1", FromRole: "claude", ToRole: "all", Kind: "proposal", Body: "Round 1 from claude", Round: 1},
				{ID: 3, ConclaveID: "ccl-council-1", FromRole: "codex", ToRole: "all", Kind: "proposal", Body: "Round 1 from codex", Round: 1},
				{ID: 4, ConclaveID: "ccl-council-1", FromRole: "agy", ToRole: "all", Kind: "proposal", Body: "Round 1 from agy", Round: 1},
			}
			state := "active"
			var relics []relic
			if n >= 2 {
				state = "completed"
				msgs = append(msgs,
					msg{ID: 5, ConclaveID: "ccl-council-1", FromRole: "claude", ToRole: "all", Kind: "review", Body: "Round 2 review from claude", Round: 2},
					msg{ID: 6, ConclaveID: "ccl-council-1", FromRole: "codex", ToRole: "all", Kind: "review", Body: "Round 2 review from codex", Round: 2},
					msg{ID: 7, ConclaveID: "ccl-council-1", FromRole: "agy", ToRole: "all", Kind: "review", Body: "Round 2 review from agy", Round: 2},
				)
				relics = append(relics, relic{
					Name:        "digest",
					AuthorRole:  "system",
					ContentType: "text/markdown",
					Body:        "Consensus: auth tests cover bearer and session checks across both rounds.",
					SHA256:      "abcdef0123456789",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"conclave": map[string]any{
					"id":           "ccl-council-1",
					"templateName": "council",
					"title":        "How is auth tested?",
					"state":        state,
				},
				"messages": msgs,
				"relics":   relics,
			})
		default:
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	})}

	root := newRootCmdWithTransport(rt)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"--api", "http://unix",
		"council", "How is auth tested?",
		"--repo", repoDir,
		"--rounds", "2",
		"--with", "claude,codex,agy",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("sablectl council failed: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}

	mu.Lock()
	opened := openBody
	mu.Unlock()
	if opened["template"] != "council" || opened["question"] != "How is auth tested?" || opened["repo"] != repoDir {
		t.Fatalf("unexpected open payload: %+v", opened)
	}

	out := stdout.String()
	for _, want := range []string{
		"Round 1",
		"@claude", "Round 1 from claude",
		"@codex", "Round 1 from codex",
		"@agy", "Round 1 from agy",
		"Round 2",
		"Round 2 review from claude",
		"Round 2 review from codex",
		"Round 2 review from agy",
		"Consensus: auth tests cover bearer and session checks across both rounds.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCouncilExitsNonZeroWhenNoParticipantAnswered(t *testing.T) {
	installStubHarnesses(t, "claude", "codex", "agy")
	repoDir := t.TempDir()

	for _, state := range []string{"completed", "cancelled", "input-required"} {
		t.Run(state, func(t *testing.T) {
			var opened bool
			rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/conclaves":
					opened = true
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"id":"ccl-empty","templateName":"council","title":"q","state":"active"}`))
				case r.URL.Path == "/api/v1/conclaves/ccl-empty/stream":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "event: state\ndata: %s:1:1:0\n\n", state)
				case r.URL.Path == "/api/v1/conclaves/ccl-empty":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"conclave": map[string]any{"id": "ccl-empty", "state": state, "wardReason": "wall_time_reached"},
						"messages": []map[string]any{
							{"id": 1, "fromRole": "operator", "toRole": "all", "kind": "decree", "body": "q"},
						},
						"relics": []any{},
					})
				default:
					http.NotFound(w, r)
				}
			})}

			root := newRootCmdWithTransport(rt)
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"--api", "http://unix", "council", "q", "--repo", repoDir})
			err := root.ExecuteContext(context.Background())
			if err == nil {
				t.Fatalf("expected non-zero error when no participant answered in state %s", state)
			}
			if !opened {
				t.Fatalf("expected council open request before failing on empty answers (err=%v)", err)
			}
			if !strings.Contains(err.Error(), "no participant") {
				t.Fatalf("expected 'no participant' error, got %v", err)
			}
		})
	}
}

func TestCouncilCtrlCStopsCouncilAndTasks(t *testing.T) {
	installStubHarnesses(t, "claude", "codex", "agy")
	repoDir := t.TempDir()

	var (
		mu             sync.Mutex
		closedState    string
		cancelledTasks []string
		streamStarted  = make(chan struct{})
		once           sync.Once
	)

	rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/conclaves":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ccl-int","templateName":"council","title":"q","state":"active"}`))
		case r.URL.Path == "/api/v1/conclaves/ccl-int/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			once.Do(func() { close(streamStarted) })
			<-r.Context().Done()
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-int":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"conclave": map[string]any{"id": "ccl-int", "state": "active"},
				"messages": []any{},
				"relics":   []any{},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/conclaves/ccl-int/close":
			var body struct {
				State string `json:"state"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			closedState = body.State
			mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"closed"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks":
			_, _ = w.Write([]byte(`[
				{"id":"task_1","idempotencyKey":"conclave-ccl-int-init-claude","state":"working","inputJson":"{\"conclaveId\":\"ccl-int\",\"role\":\"claude\",\"activation\":\"always-on\"}"},
				{"id":"task_2","idempotencyKey":"conclave-ccl-int-init-codex","state":"submitted","inputJson":"{\"conclaveId\":\"ccl-int\",\"role\":\"codex\",\"activation\":\"always-on\"}"},
				{"id":"task_done","idempotencyKey":"conclave-ccl-int-init-agy","state":"completed","inputJson":"{\"conclaveId\":\"ccl-int\",\"role\":\"agy\",\"activation\":\"always-on\"}"},
				{"id":"task_other","idempotencyKey":"conclave-other-init-claude","state":"working","inputJson":"{\"conclaveId\":\"other\",\"role\":\"claude\",\"activation\":\"always-on\"}"}
			]`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/tasks/") && strings.HasSuffix(r.URL.Path, "/cancel"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/"), "/cancel")
			mu.Lock()
			cancelledTasks = append(cancelledTasks, id)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"cancelled"}`))
		default:
			http.NotFound(w, r)
		}
	})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := newRootCmdWithTransport(rt)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--api", "http://unix", "council", "q", "--repo", repoDir})

	errCh := make(chan error, 1)
	go func() {
		errCh <- root.ExecuteContext(ctx)
	}()

	select {
	case <-streamStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for council stream to start")
	}
	cancel()

	var execErr error
	select {
	case execErr = <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for council command to exit after Ctrl-C")
	}
	if execErr == nil {
		t.Fatal("expected non-zero error after Ctrl-C cancellation")
	}

	mu.Lock()
	gotClosed := closedState
	gotCancelled := append([]string(nil), cancelledTasks...)
	mu.Unlock()

	if gotClosed != "cancelled" {
		t.Fatalf("conclave close state = %q, want %q", gotClosed, "cancelled")
	}
	if len(gotCancelled) != 2 || gotCancelled[0] != "task_1" || gotCancelled[1] != "task_2" {
		t.Fatalf("cancelled tasks = %v, want [task_1 task_2]", gotCancelled)
	}
}

func TestCouncilStdinPromptInferredRoundsAndFallbackDigest(t *testing.T) {
	installStubHarnesses(t, "claude", "codex", "agy")
	repoDir := t.TempDir()

	rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/conclaves":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ccl-inferred","templateName":"council","title":"stdin question","state":"active"}`))
		case r.URL.Path == "/api/v1/conclaves/ccl-inferred/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: state\ndata: completed:5:5:0\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-inferred":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"conclave": map[string]any{"id": "ccl-inferred", "state": "completed"},
				"messages": []map[string]any{
					{"id": 1, "fromRole": "operator", "toRole": "all", "kind": "decree", "body": "stdin question"},
					{"id": 2, "fromRole": "claude", "toRole": "all", "kind": "proposal", "body": "Claude R1"},
					{"id": 3, "fromRole": "codex", "toRole": "all", "kind": "proposal", "body": "Codex R1"},
					{"id": 4, "fromRole": "claude", "toRole": "all", "kind": "review", "body": "Claude R2"},
				},
				"relics": []any{},
			})
		default:
			http.NotFound(w, r)
		}
	})}

	root := newRootCmdWithTransport(rt)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader("stdin question\n"))
	root.SetArgs([]string{"--api", "http://unix", "council", "--repo", repoDir, "--with", "claude,codex", "--live"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("council failed: %v (stderr=%s)", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Round 1", "Round 2", "Claude R1", "Codex R1", "Claude R2", "=== Digest ===", "[missing turn]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}
