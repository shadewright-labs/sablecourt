package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func installStubHarnesses(t *testing.T, names ...string) string {
	t.Helper()
	return installStubHarnessesWithBody(t, func(name string) string {
		return fmt.Sprintf("#!/bin/sh\nprintf 'stub-out %s round=%%s question=%%s\\n' \"$1\" \"$2\"\n", name)
	}, names...)
}

func installStubHarnessesWithBody(t *testing.T, bodyFn func(name string) string, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(bodyFn(name)), 0o700); err != nil {
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
			name:      "oversized_question_over_daemon_budget",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", strings.Repeat("x", (30<<10)+1), "--repo", validRepo},
		},
		{
			name:      "oversized_stdin_with_cr_suffix",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "--repo", validRepo},
			stdin:     strings.Repeat("x", 30<<10) + "\rkeep this suffix\n",
		},
		{
			name:      "escaped_question_over_launch_budget",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", strings.Repeat("<", 12<<10), "--repo", validRepo},
		},
		{
			name:      "escaped_question_exceeds_round2_budget",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", strings.Repeat("<", 10240), "--repo", validRepo},
		},
		{
			name:      "partial_panel_two",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--with", "claude,codex"},
		},
		{
			name:      "partial_panel_one",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--with", "claude"},
		},
		{
			name:      "missing_default_harness_agy",
			harnesses: []string{"claude", "codex"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo},
		},
		{
			name:      "unknown_harness",
			harnesses: []string{"claude", "codex", "agy", "bash"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--with", "claude,codex,bash"},
		},
		{
			name:      "empty_with",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--with", ""},
		},
		{
			name:      "duplicate_with",
			harnesses: []string{"claude", "codex", "agy"},
			args:      []string{"council", "How does auth work?", "--repo", validRepo, "--with", "claude,claude,agy"},
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

type stubCouncilDaemon struct {
	mu       sync.Mutex
	openBody map[string]any
	state    string
	msgs     []councilMessageView
	turns    []councilTurnView
	relics   []map[string]any
	notify   chan struct{}
	done     chan struct{}
}

func newStubExecutingCouncilDaemon(t *testing.T) (*stubCouncilDaemon, *handlerRoundTripper) {
	t.Helper()
	d := &stubCouncilDaemon{
		state:  "active",
		notify: make(chan struct{}, 16),
		done:   make(chan struct{}),
	}

	signalUpdate := func() {
		select {
		case d.notify <- struct{}{}:
		default:
		}
	}

	rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/councils":
			var rawBody map[string]any
			if err := json.NewDecoder(r.Body).Decode(&rawBody); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			question, _ := rawBody["question"].(string)
			roundsF, _ := rawBody["rounds"].(float64)
			rounds := int(roundsF)
			if rounds <= 0 {
				rounds = 2
			}
			var roles []string
			if rawWith, ok := rawBody["with"].([]any); ok {
				for _, item := range rawWith {
					if s, ok := item.(string); ok {
						roles = append(roles, s)
					}
				}
			}

			d.mu.Lock()
			d.openBody = rawBody
			d.msgs = []councilMessageView{
				{ID: 1, ConclaveID: "ccl-council-1", FromRole: "operator", ToRole: "all", Kind: "decree", Body: question},
			}
			d.mu.Unlock()

			go func() {
				defer close(d.done)
				var nextID int64 = 2
				var digestLines []string
				for round := 1; round <= rounds; round++ {
					kind := "proposal"
					if round > 1 {
						kind = "review"
					}
					for _, role := range roles {
						bin, err := exec.LookPath(role)
						if err != nil {
							d.mu.Lock()
							d.turns = append(d.turns, councilTurnView{Round: round, Role: role, Status: "failed"})
							d.state = "cancelled"
							d.mu.Unlock()
							signalUpdate()
							return
						}
						outBytes, err := exec.Command(bin, fmt.Sprintf("%d", round), question).CombinedOutput()
						outText := strings.TrimSpace(string(outBytes))
						if err != nil || outText == "" {
							d.mu.Lock()
							d.turns = append(d.turns, councilTurnView{Round: round, Role: role, Status: "failed"})
							d.state = "cancelled"
							d.mu.Unlock()
							signalUpdate()
							return
						}
						d.mu.Lock()
						d.msgs = append(d.msgs, councilMessageView{
							ID:         nextID,
							ConclaveID: "ccl-council-1",
							FromRole:   role,
							ToRole:     "all",
							Kind:       kind,
							Body:       outText,
							Round:      round,
						})
						d.turns = append(d.turns, councilTurnView{Round: round, Role: role, Status: "completed"})
						nextID++
						d.mu.Unlock()
						digestLines = append(digestLines, fmt.Sprintf("r%d @%s: %s", round, role, outText))
					}
					signalUpdate()
				}
				d.mu.Lock()
				d.relics = []map[string]any{
					{
						"name":        "digest",
						"authorRole":  "system",
						"contentType": "text/markdown",
						"body":        "Daemon digest:\n" + strings.Join(digestLines, "\n"),
						"sha256":      "abcdef0123456789",
					},
				}
				d.state = "completed"
				d.mu.Unlock()
				signalUpdate()
			}()

			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ccl-council-1","title":"How is auth tested?","state":"active"}`))

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-council-1/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			for {
				select {
				case <-r.Context().Done():
					return
				case <-d.notify:
					d.mu.Lock()
					st := d.state
					n := len(d.msgs)
					rn := len(d.relics)
					d.mu.Unlock()
					_, _ = fmt.Fprintf(w, "event: state\ndata: %s:%d:%d:%d\n\n", st, n, n, rn)
					if fl != nil {
						fl.Flush()
					}
					if st != "active" {
						return
					}
				case <-d.done:
					d.mu.Lock()
					st := d.state
					n := len(d.msgs)
					rn := len(d.relics)
					d.mu.Unlock()
					_, _ = fmt.Fprintf(w, "event: state\ndata: %s:%d:%d:%d\n\n", st, n, n, rn)
					if fl != nil {
						fl.Flush()
					}
					return
				}
			}

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-council-1":
			d.mu.Lock()
			st := d.state
			msgs := append([]councilMessageView(nil), d.msgs...)
			turns := append([]councilTurnView(nil), d.turns...)
			relics := append([]map[string]any(nil), d.relics...)
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"conclave": map[string]any{
					"id":    "ccl-council-1",
					"title": "How is auth tested?",
					"state": st,
				},
				"messages": msgs,
				"turns":    turns,
				"relics":   relics,
			})
		default:
			http.Error(w, "unexpected request: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	})}
	return d, rt
}

func TestCouncilWithStubCLIsPrintsRoundsAndDigest(t *testing.T) {
	installStubHarnesses(t, "claude", "codex", "agy")
	repoDir := t.TempDir()

	daemon, rt := newStubExecutingCouncilDaemon(t)

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

	daemon.mu.Lock()
	opened := daemon.openBody
	daemon.mu.Unlock()
	if _, hasTemplate := opened["template"]; hasTemplate {
		t.Fatalf("unexpected legacy template in open payload: %+v", opened)
	}
	if opened["question"] != "How is auth tested?" || opened["repo"] != repoDir {
		t.Fatalf("unexpected open payload: %+v", opened)
	}

	out := stdout.String()
	for _, want := range []string{
		"Round 1",
		"@claude", "stub-out claude round=1 question=How is auth tested?",
		"@codex", "stub-out codex round=1 question=How is auth tested?",
		"@agy", "stub-out agy round=1 question=How is auth tested?",
		"Round 2",
		"stub-out claude round=2 question=How is auth tested?",
		"stub-out codex round=2 question=How is auth tested?",
		"stub-out agy round=2 question=How is auth tested?",
		"=== Digest ===",
		"Daemon digest:",
		"r2 @agy: stub-out agy round=2 question=How is auth tested?",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCouncilStubCLIFailureFailsRun(t *testing.T) {
	installStubHarnessesWithBody(t, func(string) string {
		return "#!/bin/sh\nexit 93\n"
	}, "claude", "codex", "agy")
	repoDir := t.TempDir()

	_, rt := newStubExecutingCouncilDaemon(t)

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
	err := root.ExecuteContext(ctx)
	if err == nil {
		t.Fatalf("expected failure when stub CLIs exit 93, got nil\nstdout=%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "no digest recorded") {
		t.Fatalf("expected 'no digest recorded' on failed run:\n%s", stdout.String())
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
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/councils":
					opened = true
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"id":"ccl-empty","title":"q","state":"active"}`))
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
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/councils":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ccl-int","title":"q","state":"active"}`))
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
			mu.Lock()
			done := make(map[string]bool)
			for _, id := range cancelledTasks {
				done[id] = true
			}
			mu.Unlock()
			s1 := "working"
			if done["task_1"] {
				s1 = "cancelled"
			}
			s2 := "submitted"
			if done["task_2"] {
				s2 = "cancelled"
			}
			_, _ = fmt.Fprintf(w, `[
				{"id":"task_1","idempotencyKey":"conclave-ccl-int-init-claude","state":%q,"inputJson":"{\"conclaveId\":\"ccl-int\",\"role\":\"claude\",\"activation\":\"always-on\"}"},
				{"id":"task_2","idempotencyKey":"conclave-ccl-int-init-codex","state":%q,"inputJson":"{\"conclaveId\":\"ccl-int\",\"role\":\"codex\",\"activation\":\"always-on\"}"},
				{"id":"task_done","idempotencyKey":"conclave-ccl-int-init-agy","state":"completed","inputJson":"{\"conclaveId\":\"ccl-int\",\"role\":\"agy\",\"activation\":\"always-on\"}"},
				{"id":"task_other","idempotencyKey":"conclave-other-init-claude","state":"working","inputJson":"{\"conclaveId\":\"other\",\"role\":\"claude\",\"activation\":\"always-on\"}"}
			]`, s1, s2)
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

func TestCouncilStdinPromptUsesRecordedRoundsAndPrintsNoDigestRecorded(t *testing.T) {
	installStubHarnesses(t, "claude", "codex", "agy")
	repoDir := t.TempDir()

	rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/councils":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ccl-inferred","title":"stdin question","state":"active"}`))
		case r.URL.Path == "/api/v1/conclaves/ccl-inferred/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: state\ndata: completed:5:5:0\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-inferred":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"conclave": map[string]any{"id": "ccl-inferred", "state": "completed"},
				"messages": []map[string]any{
					{"id": 1, "fromRole": "operator", "toRole": "all", "kind": "decree", "body": "stdin question"},
					{"id": 2, "fromRole": "claude", "toRole": "all", "kind": "proposal", "body": "Claude R1", "round": 1},
					{"id": 3, "fromRole": "codex", "toRole": "all", "kind": "proposal", "body": "Codex R1", "round": 1},
					{"id": 4, "fromRole": "claude", "toRole": "all", "kind": "review", "body": "Claude R2", "round": 2},
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
	root.SetArgs([]string{"--api", "http://unix", "council", "--repo", repoDir, "--with", "claude,codex,agy", "--live"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("council failed: %v (stderr=%s)", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Round 1", "Round 2", "Claude R1", "Codex R1", "Claude R2", "=== Digest ===", "no digest recorded"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[missing turn]") {
		t.Fatalf("output must not synthesize a fallback digest:\n%s", out)
	}
}

func TestCouncilOpenUsesDedicatedEndpointAndFailsClosedOn404Or405(t *testing.T) {
	installStubHarnesses(t, "claude", "codex", "agy")
	repoDir := t.TempDir()

	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(fmt.Sprintf("http_%d", status), func(t *testing.T) {
			var (
				mu    sync.Mutex
				paths []string
			)
			rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.Method+" "+r.URL.Path)
				mu.Unlock()
				if r.URL.Path == "/api/v1/conclaves" {
					t.Errorf("client fell back to legacy endpoint %s", r.URL.Path)
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"id":"ccl-legacy","templateName":"council","state":"active"}`))
					return
				}
				http.Error(w, "not supported", status)
			})}

			root := newRootCmdWithTransport(rt)
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"--api", "http://unix", "council", "Check auth", "--repo", repoDir})

			err := root.ExecuteContext(context.Background())
			if err == nil {
				t.Fatalf("expected error on HTTP %d, got nil", status)
			}
			if !strings.Contains(err.Error(), "/api/v1/councils") {
				t.Fatalf("expected clear error mentioning /api/v1/councils, got: %v", err)
			}

			mu.Lock()
			gotPaths := append([]string(nil), paths...)
			mu.Unlock()
			if len(gotPaths) != 1 || gotPaths[0] != "POST /api/v1/councils" {
				t.Fatalf("requests = %v, want [POST /api/v1/councils]", gotPaths)
			}
		})
	}
}

func TestCouncilCloseCancelsOnlyExactAssignmentAndFailsIfRunning(t *testing.T) {
	t.Run("close_failed_cancels_only_exact_assignment", func(t *testing.T) {
		var (
			mu        sync.Mutex
			cancelled []string
		)
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/conclaves/ccl-room/close":
				http.Error(w, "close failed", http.StatusInternalServerError)
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks":
				mu.Lock()
				done := make(map[string]bool)
				for _, id := range cancelled {
					done[id] = true
				}
				mu.Unlock()
				ownedState := "working"
				if done["task_owned"] {
					ownedState = "cancelled"
				}
				_, _ = fmt.Fprintf(w, `[
					{"id":"task_owned","idempotencyKey":"conclave-ccl-room-init-claude","state":%q,"inputJson":"{\"conclaveId\":\"ccl-room\",\"role\":\"claude\",\"activation\":\"always-on\"}"},
					{"id":"task_input_mention","idempotencyKey":"manual-job-1","state":"working","inputJson":"{\"conclaveId\":\"ccl-room\",\"prompt\":\"inspect ccl-room\"}"},
					{"id":"task_prefix_only","idempotencyKey":"conclave-ccl-room-custom","state":"working","inputJson":"{\"prompt\":\"unrelated\"}"}
				]`, ownedState)
			case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/tasks/") && strings.HasSuffix(r.URL.Path, "/cancel"):
				id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/"), "/cancel")
				mu.Lock()
				cancelled = append(cancelled, id)
				mu.Unlock()
				_, _ = w.Write([]byte(`{"status":"cancelled"}`))
			default:
				http.NotFound(w, r)
			}
		})

		err := stopCouncilAndTasks(newTestClientFactory(handler), "", "ccl-room")
		if err == nil {
			t.Fatal("expected error when close failed")
		}
		mu.Lock()
		got := append([]string(nil), cancelled...)
		mu.Unlock()
		if len(got) != 1 || got[0] != "task_owned" {
			t.Fatalf("cancelled tasks = %v, want [task_owned]", got)
		}
	})

	t.Run("close_fails_when_owned_task_stays_working", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/conclaves/ccl-room/close":
				_, _ = w.Write([]byte(`{"status":"closed"}`))
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks":
				_, _ = w.Write([]byte(`[
					{"id":"task_stuck","idempotencyKey":"conclave-ccl-room-init-claude","state":"working","inputJson":"{\"conclaveId\":\"ccl-room\",\"role\":\"claude\",\"activation\":\"always-on\"}"}
				]`))
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/task_stuck/cancel":
				// Daemon answers 200 but leaves task working.
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			default:
				http.NotFound(w, r)
			}
		})

		err := stopCouncilAndTasks(newTestClientFactory(handler), "", "ccl-room")
		if err == nil {
			t.Fatal("expected close to fail when owned task remains working")
		}
		if !strings.Contains(err.Error(), "task_stuck") {
			t.Fatalf("expected error naming still-running task_stuck, got: %v", err)
		}
	})
}

func TestCouncilTitleRedactsBeforeTruncation(t *testing.T) {
	prefix := strings.Repeat("a", 98)
	cases := []struct {
		name     string
		secret   string
		fragment string
	}{
		{
			name:     "openai_key_across_cutoff",
			secret:   "sk-proj-SYNTHETIC1234567890abcdefghij",
			fragment: "sk-proj-SYN",
		},
		{
			name:     "github_token_across_cutoff",
			secret:   "ghp_SYNTHETIC1234567890abcdefghij",
			fragment: "ghp_SYN",
		},
		{
			name:     "generic_kv_across_cutoff",
			secret:   "api_key=SYNTHETICVALUE123456789",
			fragment: "SYNTHETICVALUE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := prefix + " " + tc.secret + " trailing context"
			payload := buildCouncilOpenPayload(q, "/repo", "", false, 2, []string{"claude"}, false)
			if _, hasTemplate := payload["template"]; hasTemplate {
				t.Fatalf("council open payload must not include legacy template field: %+v", payload)
			}
			for _, field := range []string{"title", "question", "decree"} {
				val, _ := payload[field].(string)
				if strings.Contains(val, tc.fragment) || strings.Contains(val, tc.secret) {
					t.Fatalf("payload[%q] leaked synthetic credential fragment %q: %q", field, tc.fragment, val)
				}
				if !strings.Contains(val, "[REDACT") {
					t.Fatalf("payload[%q] missing redaction marker: %q", field, val)
				}
			}
		})
	}
}

func TestCouncilDigestRejectsParticipantRelicAndPrintsNoDigestRecorded(t *testing.T) {
	installStubHarnesses(t, "claude", "codex", "agy")
	repoDir := t.TempDir()

	rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/councils":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ccl-spoof","title":"q","state":"active"}`))
		case r.URL.Path == "/api/v1/conclaves/ccl-spoof/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: state\ndata: completed:2:2:1\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-spoof":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"conclave": map[string]any{"id": "ccl-spoof", "state": "completed"},
				"messages": []map[string]any{
					{"id": 1, "fromRole": "operator", "toRole": "all", "kind": "decree", "body": "q"},
					{"id": 2, "fromRole": "claude", "toRole": "all", "kind": "proposal", "body": "Claude answer", "round": 1},
				},
				"relics": []map[string]any{
					{
						"name":        "digest",
						"authorRole":  "claude",
						"contentType": "text/markdown",
						"body":        "SPOOFED PARTICIPANT DIGEST",
						"sha256":      "0123456789abcdef",
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	})}

	root := newRootCmdWithTransport(rt)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--api", "http://unix", "council", "q", "--repo", repoDir, "--rounds", "1", "--with", "claude,codex,agy"})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("council failed: %v (stderr=%s)", err, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "SPOOFED PARTICIPANT DIGEST") {
		t.Fatalf("participant-authored Relic was presented as digest:\n%s", out)
	}
	if !strings.Contains(out, "no digest recorded") {
		t.Fatalf("expected 'no digest recorded' when no daemon digest exists:\n%s", out)
	}
}

func TestCouncilClarificationQuestionsFailClosedWithoutInferredRoundOrFallbackDigest(t *testing.T) {
	installStubHarnesses(t, "claude", "codex", "agy")
	repoDir := t.TempDir()

	rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/councils":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ccl-qonly","title":"q","state":"active"}`))
		case r.URL.Path == "/api/v1/conclaves/ccl-qonly/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: state\ndata: completed:3:3:0\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-qonly":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"conclave": map[string]any{"id": "ccl-qonly", "state": "completed"},
				"messages": []map[string]any{
					{"id": 1, "fromRole": "operator", "toRole": "all", "kind": "decree", "body": "q"},
					{"id": 2, "fromRole": "claude", "toRole": "operator", "kind": "question", "body": "Which file do you mean?"},
					{"id": 3, "fromRole": "claude", "toRole": "operator", "kind": "question", "body": "Also which branch?"},
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
	root.SetArgs([]string{"--api", "http://unix", "council", "q", "--repo", repoDir, "--rounds", "1", "--with", "claude,codex,agy"})

	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatalf("expected non-zero exit when only clarification questions were posted, got nil\nstdout=%s", stdout.String())
	}
	out := stdout.String()
	if strings.Contains(out, "Round 2") {
		t.Fatalf("output fabricated Round 2 under a one-round cap:\n%s", out)
	}
	if strings.Contains(out, "[missing turn]") {
		t.Fatalf("output synthesized a client-side fallback digest:\n%s", out)
	}
	if !strings.Contains(out, "no digest recorded") {
		t.Fatalf("expected 'no digest recorded' in output:\n%s", out)
	}
}

func TestCouncilPollFailureAttemptsBoundedCleanup(t *testing.T) {
	installStubHarnesses(t, "claude", "codex", "agy")
	repoDir := t.TempDir()

	var (
		mu             sync.Mutex
		closedState    string
		cancelledTasks []string
	)
	rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/councils":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ccl-pollfail","title":"q","state":"active"}`))
		case r.URL.Path == "/api/v1/conclaves/ccl-pollfail/stream":
			http.Error(w, "stream unavailable", http.StatusServiceUnavailable)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-pollfail":
			http.Error(w, "backend down", http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/conclaves/ccl-pollfail/close":
			var body struct {
				State string `json:"state"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			closedState = body.State
			mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"closed"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks":
			mu.Lock()
			done := len(cancelledTasks) > 0
			mu.Unlock()
			st := "working"
			if done {
				st = "cancelled"
			}
			_, _ = fmt.Fprintf(w, `[{"id":"task_poll","idempotencyKey":"conclave-ccl-pollfail-init-claude","state":%q,"inputJson":"{\"conclaveId\":\"ccl-pollfail\",\"role\":\"claude\",\"activation\":\"always-on\"}"}]`, st)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/task_poll/cancel":
			mu.Lock()
			cancelledTasks = append(cancelledTasks, "task_poll")
			mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"cancelled"}`))
		default:
			http.NotFound(w, r)
		}
	})}

	var out bytes.Buffer
	err := runCouncilSession(context.Background(), newTestClientFactory(rt.handler), &out, councilRunParams{
		Question:  "q",
		Repo:      repoDir,
		Rounds:    2,
		Harnesses: []string{"claude"},
		PollEvery: 5 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected error after consecutive poll failures")
	}

	mu.Lock()
	gotClosed := closedState
	gotCancelled := append([]string(nil), cancelledTasks...)
	mu.Unlock()
	if gotClosed != "cancelled" {
		t.Fatalf("expected cleanup close state=cancelled after poll failures, got %q", gotClosed)
	}
	if len(gotCancelled) != 1 || gotCancelled[0] != "task_poll" {
		t.Fatalf("expected cleanup to cancel task_poll, got %v", gotCancelled)
	}
}

func TestCouncilStdinPromptCancelsOnContextDone(t *testing.T) {
	installStubHarnesses(t, "claude", "codex", "agy")
	repoDir := t.TempDir()

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("daemon should not be called when prompt is cancelled: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := newRootCmdWithTransport(rt)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(pr)
	root.SetArgs([]string{"--api", "http://unix", "council", "--repo", repoDir})

	errCh := make(chan error, 1)
	go func() {
		errCh <- root.ExecuteContext(ctx)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case execErr := <-errCh:
		if execErr == nil {
			t.Fatal("expected cancellation error, got nil")
		}
		if !strings.Contains(execErr.Error(), "context canceled") {
			t.Fatalf("expected context canceled error, got: %v", execErr)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("council question prompt remained blocked after context cancellation")
	}
}

func TestCouncilClientSendsIdempotencyKey(t *testing.T) {
	installStubHarnesses(t, "claude", "codex", "agy")
	repoDir := t.TempDir()

	var gotKey string
	rt := &handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/councils":
			gotKey = r.Header.Get("Idempotency-Key")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"ccl-idem-1","title":"Review","state":"active"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-idem-1/stream":
			w.Header().Set("Content-Type", "text/event-stream")
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/conclaves/ccl-idem-1":
			_, _ = w.Write([]byte(`{"conclave":{"id":"ccl-idem-1","state":"completed"},"messages":[{"id":1,"conclaveId":"ccl-idem-1","fromRole":"claude","toRole":"all","kind":"proposal","body":"ok","round":1}],"relics":[{"name":"digest","authorRole":"system","body":"ok"}],"turns":[{"round":1,"role":"claude","state":"completed"}]}`))
		default:
			http.NotFound(w, r)
		}
	})}

	root := newRootCmdWithTransport(rt)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--api", "http://unix", "council", "Review this", "--repo", repoDir})

	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("council command failed: %v (stderr=%s)", err, stderr.String())
	}
	if strings.TrimSpace(gotKey) == "" {
		t.Fatal("expected sablectl council to send a non-empty Idempotency-Key header")
	}
}

func TestCouncilStdinMaxQuestionPreserved(t *testing.T) {
	exact := strings.Repeat("x", maxCouncilQuestionBytes)
	for _, tc := range []struct {
		name  string
		stdin string
	}{
		{name: "exact_eof", stdin: exact},
		{name: "exact_lf", stdin: exact + "\n"},
		{name: "exact_crlf", stdin: exact + "\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, err := readLineContext(context.Background(), strings.NewReader(tc.stdin), maxCouncilQuestionBytes)
			if err != nil {
				t.Fatalf("readLineContext(%s) err = %v", tc.name, err)
			}
			got, err := validateCouncilQuestion(line)
			if err != nil {
				t.Fatalf("validateCouncilQuestion(%s) err = %v", tc.name, err)
			}
			if got != exact {
				t.Fatalf("got len=%d, want %d", len(got), len(exact))
			}
		})
	}
}
