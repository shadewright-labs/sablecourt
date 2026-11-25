package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestCouncilEndToEndMacOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("skipping real confined council test on %s: requires macOS Seatbelt (sandbox-exec)", runtime.GOOS)
	}

	// Build the real sablecourtd binary from this tree.
	binDir := t.TempDir()
	daemonBin := filepath.Join(binDir, "sablecourtd")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", daemonBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build sablecourtd: %v\n%s", err, out)
	}

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("locate git: %v", err)
	}

	for _, tc := range []struct {
		name string
		mode string // "ref", "live", "writer"
	}{
		{name: "ref", mode: "ref"},
		{name: "live", mode: "live"},
		{name: "writer", mode: "writer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runRealCouncilE2E(t, daemonBin, filepath.Dir(gitPath), tc.mode)
		})
	}
}

func runRealCouncilE2E(t *testing.T, daemonBin, gitDir, mode string) {
	t.Helper()

	repo := t.TempDir()
	councilTestGit(t, repo, "init", "--template=", "-q")
	if err := os.WriteFile(filepath.Join(repo, "committed.txt"), []byte("committed-v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	councilTestGit(t, repo, "add", "committed.txt")
	councilTestGit(t, repo, "commit", "-qm", "initial")
	if err := os.WriteFile(filepath.Join(repo, "uncommitted.txt"), []byte("uncommitted-fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(t.TempDir(), "state")
	dbPath := filepath.Join(t.TempDir(), "sablecourt.db")
	tokenPath := filepath.Join(stateDir, "operator.token")
	stubDir := t.TempDir()

	// Gate ref-mode checkout reads until after HEAD moves, record every real
	// harness launch, and verify the running task's active lease scope in SQLite.
	headMoved := make(chan struct{})
	var headMovedOnce sync.Once
	releaseHead := func() { headMovedOnce.Do(func() { close(headMoved) }) }
	defer releaseHead()
	if mode != "ref" {
		releaseHead()
	}

	var (
		launchMu     sync.Mutex
		launchCounts = make(map[string]int)
		totalStarts  int
		leaseErrors  []string
		checkStore   *sqlite.SQLiteStore
	)
	t.Cleanup(func() {
		launchMu.Lock()
		st := checkStore
		checkStore = nil
		launchMu.Unlock()
		if st != nil {
			_ = st.Close()
		}
	})

	gateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/launch" {
			http.NotFound(w, r)
			return
		}
		taskID := strings.TrimSpace(r.Header.Get("X-Task-ID"))
		workload := strings.TrimSpace(r.Header.Get("X-Workload"))
		rawTaskToken := strings.TrimSpace(r.Header.Get("X-Task-Token"))
		tokenHash := ""
		if rawTaskToken != "" {
			tokenHash = model.HashToken(rawTaskToken)
		}

		launchMu.Lock()
		totalStarts++
		launchCounts[taskID]++
		st := checkStore
		if st == nil {
			openedStore, err := sqlite.Open(dbPath)
			if err != nil {
				leaseErrors = append(leaseErrors, fmt.Sprintf("open store for task %s: %v", taskID, err))
			} else {
				checkStore = openedStore
				st = openedStore
			}
		}
		launchMu.Unlock()

		if st == nil || taskID == "" || workload == "" || tokenHash == "" {
			launchMu.Lock()
			leaseErrors = append(leaseErrors, fmt.Sprintf("invalid launch callback metadata for task=%q workload=%q hasToken=%t", taskID, workload, tokenHash != ""))
			launchMu.Unlock()
			http.Error(w, "invalid launch metadata", http.StatusBadRequest)
			return
		}
		lookupCtx, cancelLookup := context.WithTimeout(t.Context(), 5*time.Second)
		lease, err := st.GetActiveLeaseByTokenHash(lookupCtx, tokenHash, time.Now())
		cancelLookup()
		if err != nil || lease.TaskID != taskID || lease.WorkloadName != workload {
			launchMu.Lock()
			leaseErrors = append(leaseErrors, fmt.Sprintf("lease scope mismatch for task=%q workload=%q: got task=%q workload=%q err=%v",
				taskID, workload, lease.TaskID, lease.WorkloadName, err))
			launchMu.Unlock()
			http.Error(w, "invalid lease scope", http.StatusForbidden)
			return
		}

		select {
		case <-headMoved:
		case <-t.Context().Done():
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		releaseHead()
		gateSrv.Close()
	})

	gateHost, gatePort, err := net.SplitHostPort(gateSrv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("parse gate addr: %v", err)
	}
	for _, role := range []string{"claude", "codex", "agy"} {
		script := councilStubScript(role, mode, tokenPath, gateHost, gatePort)
		if err := os.WriteFile(filepath.Join(stubDir, role), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	addr := freeLocalAddr(t)
	daemonCtx, stopDaemon := context.WithCancel(t.Context())
	defer stopDaemon()

	var daemonLog lockedBuffer
	cmd := exec.CommandContext(daemonCtx, daemonBin, "--db", dbPath, "--listen", addr)
	cmd.Dir = t.TempDir()
	cmd.Stdout = &daemonLog
	cmd.Stderr = &daemonLog
	cmd.Env = []string{
		"PATH=" + stubDir + string(os.PathListSeparator) + gitDir + ":/usr/bin:/bin",
		"HOME=" + t.TempDir(),
		"SABLECOURT_STATE_DIR=" + stateDir,
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sablecourtd: %v", err)
	}
	t.Cleanup(func() {
		stopDaemon()
		_ = cmd.Wait()
	})

	baseURL := "http://" + addr
	waitForDaemonHealth(t, baseURL, &daemonLog)

	rawToken, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read operator token: %v\ndaemon log:\n%s", err, daemonLog.String())
	}
	token := strings.TrimSpace(string(rawToken))

	// Unauthenticated POST must fail closed.
	unauthStatus, unauthBody := doDaemonJSON(t, http.MethodPost, baseURL+"/api/v1/councils", "", `{"question":"Check","repo":`+strconv.Quote(repo)+`}`)
	if unauthStatus != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST = %d %s, want 401", unauthStatus, unauthBody)
	}

	payload := map[string]any{
		"question": "Evaluate checkout isolation and review peer findings.",
		"repo":     repo,
	}
	switch mode {
	case "ref":
		payload["ref"] = "HEAD"
	case "live":
		payload["live"] = true
	case "writer":
		payload["ref"] = "HEAD"
		payload["writer"] = "claude"
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	status, body := doDaemonJSON(t, http.MethodPost, baseURL+"/api/v1/councils", token, string(rawPayload))
	if status != http.StatusCreated {
		t.Fatalf("POST /api/v1/councils (%s) = %d %s\ndaemon log:\n%s", mode, status, body, daemonLog.String())
	}
	var opened domain.ConclaveSession
	if err := json.Unmarshal(body, &opened); err != nil || opened.ID == "" {
		t.Fatalf("decode opened council: %v (%s)", err, body)
	}

	// Move HEAD in ref mode after open, then release the checkout-read gate so
	// every participant reads its checkout after HEAD has moved to committed-v2.
	if mode == "ref" {
		if err := os.WriteFile(filepath.Join(repo, "committed.txt"), []byte("committed-v2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		councilTestGit(t, repo, "add", "committed.txt")
		councilTestGit(t, repo, "commit", "-qm", "move head after open")
		releaseHead()
	}

	type detailMessage struct {
		domain.ConclaveMessage
		Round int `json:"round"`
	}
	type detailResponse struct {
		Conclave domain.ConclaveSession `json:"conclave"`
		Messages []detailMessage        `json:"messages"`
		Relics   []domain.ConclaveRelic `json:"relics"`
	}

	var detail detailResponse
	deadline := time.Now().Add(45 * time.Second)
	for {
		stCode, raw := doDaemonJSON(t, http.MethodGet, baseURL+"/api/v1/conclaves/"+opened.ID, token, "")
		if stCode != http.StatusOK {
			t.Fatalf("GET detail = %d %s\ndaemon log:\n%s", stCode, raw, daemonLog.String())
		}
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("decode detail: %v (%s)", err, raw)
		}
		if detail.Conclave.State == domain.ConclaveStateCompleted ||
			detail.Conclave.State == domain.ConclaveStateCancelled ||
			detail.Conclave.State == domain.ConclaveStateInputRequired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("council %s (%s) timed out in state %s (messages=%d)\ndaemon log:\n%s",
				opened.ID, mode, detail.Conclave.State, len(detail.Messages), daemonLog.String())
		}
		time.Sleep(150 * time.Millisecond)
	}

	if detail.Conclave.State != domain.ConclaveStateCompleted || detail.Conclave.WardReason != "round_cap" {
		_, rawTasks := doDaemonJSON(t, http.MethodGet, baseURL+"/api/v1/tasks", token, "")
		launchMu.Lock()
		errsCopy := append([]string(nil), leaseErrors...)
		launchMu.Unlock()
		t.Fatalf("mode %s finished with state=%s reason=%q (messages=%d)\nlease errors:\n%s\ntasks:\n%s\ndaemon log:\n%s",
			mode, detail.Conclave.State, detail.Conclave.WardReason, len(detail.Messages), strings.Join(errsCopy, "\n"), rawTasks, daemonLog.String())
	}
	if len(detail.Messages) != 6 {
		t.Fatalf("mode %s messages = %d, want 6: %+v", mode, len(detail.Messages), detail.Messages)
	}

	var digest *domain.ConclaveRelic
	var writerDiffs []domain.ConclaveRelic
	for i := range detail.Relics {
		r := &detail.Relics[i]
		if r.Name == "digest" && r.AuthorRole == "system" {
			digest = r
		}
		if strings.HasPrefix(r.Name, "git-diff-") {
			writerDiffs = append(writerDiffs, *r)
		}
	}
	if digest == nil || digest.SHA256 == "" {
		t.Fatalf("mode %s missing system digest Relic: %+v", mode, detail.Relics)
	}

	seen := make(map[string]detailMessage)
	for _, msg := range detail.Messages {
		if msg.Round != 1 && msg.Round != 2 {
			t.Errorf("unexpected round %d for %s", msg.Round, msg.FromRole)
		}
		for line := range strings.SplitSeq(msg.Body, "\n") {
			if !strings.Contains(digest.Body, line) {
				t.Errorf("digest missing line %q from %s round %d", line, msg.FromRole, msg.Round)
			}
		}
		seen[fmt.Sprintf("%d/%s", msg.Round, msg.FromRole)] = msg
	}
	for _, role := range []string{"claude", "codex", "agy"} {
		for _, round := range []int{1, 2} {
			msg, ok := seen[fmt.Sprintf("%d/%s", round, role)]
			if !ok {
				t.Errorf("missing %s answer in round %d", role, round)
				continue
			}
			wantTag := fmt.Sprintf("role=%s mode=%s round=%d", role, mode, round)
			if !strings.Contains(msg.Body, wantTag) {
				t.Errorf("answer %d/%s missing %q: %q", round, role, wantTag, msg.Body)
			}
			if mode == "writer" && round == 2 && role != "claude" {
				if !strings.Contains(msg.Body, "verified-writer-diff-r2") {
					t.Errorf("reviewer %s round 2 did not verify redacted writer diff: %q", role, msg.Body)
				}
			}
		}
	}

	if mode == "writer" {
		if len(writerDiffs) != 1 {
			t.Fatalf("writer mode expected 1 git-diff Relic, got %d: %+v", len(writerDiffs), writerDiffs)
		}
		diff := writerDiffs[0]
		if diff.AuthorRole != "claude" || !strings.Contains(diff.Body, "+writer-change-r1") ||
			!strings.Contains(diff.Body, "[REDACTED:secret]") || strings.Contains(diff.Body, "private-fixture-secret") {
			t.Fatalf("unexpected writer diff Relic: author=%q body=%q", diff.AuthorRole, diff.Body)
		}
	} else if len(detail.Relics) != 1 {
		t.Fatalf("mode %s expected only digest Relic, got %d", mode, len(detail.Relics))
	}

	// Verify execution log and actual stub launches both record exactly 6 starts.
	tCode, rawTasks := doDaemonJSON(t, http.MethodGet, baseURL+"/api/v1/tasks", token, "")
	if tCode != http.StatusOK {
		t.Fatalf("GET /api/v1/tasks = %d %s", tCode, rawTasks)
	}
	var tasks []domain.Task
	if err := json.Unmarshal(rawTasks, &tasks); err != nil {
		t.Fatalf("decode tasks: %v", err)
	}
	councilTaskIDs := make([]string, 0, 6)
	for _, task := range tasks {
		if a, ok := store.TaskConclaveAssignment(task); ok && a.ConclaveID == opened.ID {
			councilTaskIDs = append(councilTaskIDs, task.ID)
			if task.State != domain.TaskStateCompleted {
				t.Errorf("council task %s ended in state %s", task.ID, task.State)
			}
		}
	}
	if len(councilTaskIDs) != 6 {
		t.Fatalf("mode %s task count = %d, want 6", mode, len(councilTaskIDs))
	}
	launchMu.Lock()
	gotTotalStarts := totalStarts
	gotLaunchCounts := make(map[string]int, len(launchCounts))
	for id, n := range launchCounts {
		gotLaunchCounts[id] = n
	}
	gotLeaseErrors := append([]string(nil), leaseErrors...)
	launchMu.Unlock()
	if len(gotLeaseErrors) > 0 {
		t.Fatalf("mode %s task lease verification errors:\n%s", mode, strings.Join(gotLeaseErrors, "\n"))
	}
	if gotTotalStarts != 6 || len(gotLaunchCounts) != 6 {
		t.Fatalf("mode %s actual stub starts = %d (unique tasks = %d), want 6", mode, gotTotalStarts, len(gotLaunchCounts))
	}
	for _, taskID := range councilTaskIDs {
		if gotLaunchCounts[taskID] != 1 {
			t.Errorf("mode %s task %s launched %d times, want 1", mode, taskID, gotLaunchCounts[taskID])
		}
	}

	// Verify shared council worktrees (ref / writer) are cleaned up after closure.
	if mode == "ref" || mode == "writer" {
		cleanDeadline := time.Now().Add(10 * time.Second)
		for {
			wtList := councilTestGit(t, repo, "worktree", "list", "--porcelain")
			if strings.Count(wtList, "worktree ") == 1 {
				break
			}
			if time.Now().After(cleanDeadline) {
				t.Fatalf("mode %s left council worktrees registered after close:\n%s\ndaemon log:\n%s", mode, wtList, daemonLog.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func councilStubScript(role, mode, tokenPath, gateHost, gatePort string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -u\n")
	// Extract the prompt (final positional argument).
	b.WriteString("prompt=\"\"\nfor arg in \"$@\"; do prompt=\"$arg\"; done\n")
	// Common confinement and lease contract checks.
	b.WriteString("case \"${SABLECOURT_TASK_TOKEN:-}\" in " + model.TokenPrefix + "*) ;; *) echo 'missing task lease' >&2; exit 10 ;; esac\n")
	b.WriteString("if [ \"${SABLECOURT_WORKLOAD:-}\" != " + shellSingleQuote(role) + " ]; then echo 'wrong workload scope' >&2; exit 14; fi\n")
	b.WriteString("if [ -z \"${SABLECOURT_TASK_ID:-}\" ]; then echo 'missing task id' >&2; exit 15; fi\n")
	b.WriteString("if [ -n \"${SABLECOURT_MODEL_GATE_URL:-}\" ]; then echo 'unexpected model gate url' >&2; exit 11; fi\n")
	b.WriteString("if [ -z \"${SABLECOURT_DATA_GATE_URL:-}\" ]; then echo 'missing data gate url' >&2; exit 12; fi\n")
	b.WriteString("if cat " + shellSingleQuote(tokenPath) + " >/dev/null 2>&1; then echo 'read operator token' >&2; exit 13; fi\n")
	// Report this launch to the test callback server for start counting, live lease verification, and ref-mode HEAD gating.
	b.WriteString("resp=$(printf 'POST /launch HTTP/1.1\\r\\nHost: 127.0.0.1\\r\\nConnection: close\\r\\nX-Task-ID: %s\\r\\nX-Workload: %s\\r\\nX-Task-Token: %s\\r\\nContent-Length: 0\\r\\n\\r\\n' \"$SABLECOURT_TASK_ID\" \"$SABLECOURT_WORKLOAD\" \"$SABLECOURT_TASK_TOKEN\" | /usr/bin/nc " + shellSingleQuote(gateHost) + " " + shellSingleQuote(gatePort) + ") || exit 16\n")
	b.WriteString("case \"$resp\" in 'HTTP/1.1 200 '*) ;; *) echo \"launch callback failed: $resp\" >&2; exit 17 ;; esac\n")

	b.WriteString("round=1\ncase \"$prompt\" in *'### Round 1'*) round=2 ;; esac\n")

	switch mode {
	case "ref":
		b.WriteString("printf 'committed-v1\\n' | /usr/bin/cmp -s - committed.txt || { echo 'unexpected committed.txt' >&2; exit 21; }\n")
		b.WriteString("if [ -e uncommitted.txt ]; then echo 'saw uncommitted file in ref mode' >&2; exit 22; fi\n")
		b.WriteString("if (echo mutate > committed.txt) 2>/dev/null; then echo 'wrote read-only ref checkout' >&2; exit 23; fi\n")
		b.WriteString("if (echo mutate > probe.tmp) 2>/dev/null; then echo 'created file in read-only ref checkout' >&2; exit 24; fi\n")
	case "live":
		b.WriteString("printf 'uncommitted-fixture\\n' | /usr/bin/cmp -s - uncommitted.txt || { echo 'unexpected uncommitted.txt' >&2; exit 31; }\n")
		b.WriteString("if (echo mutate > uncommitted.txt) 2>/dev/null; then echo 'wrote read-only live checkout' >&2; exit 32; fi\n")
		b.WriteString("if (echo mutate > probe.tmp) 2>/dev/null; then echo 'created file in read-only live checkout' >&2; exit 33; fi\n")
	case "writer":
		if role == "claude" {
			b.WriteString("if [ \"$round\" = 1 ]; then\n")
			b.WriteString("  {\n")
			b.WriteString("    echo 'committed-v1'\n")
			b.WriteString("    echo 'writer-change-r1'\n")
			b.WriteString("    echo 'password=private-fixture-secret'\n")
			b.WriteString("    i=0\n")
			b.WriteString("    while [ \"$i\" -lt 450 ]; do\n")
			b.WriteString("      echo 'padding-line-0123456789abcdef0123456789'\n")
			b.WriteString("      i=$((i + 1))\n")
			b.WriteString("    done\n")
			b.WriteString("    echo 'tail-beyond-cap'\n")
			b.WriteString("  } > committed.txt || exit 40\n")
			b.WriteString("fi\n")
		} else {
			b.WriteString("printf 'committed-v1\\n' | /usr/bin/cmp -s - committed.txt || { echo 'reviewer saw mutated checkout' >&2; exit 42; }\n")
			b.WriteString("if (echo mutate > committed.txt) 2>/dev/null; then echo 'reviewer wrote checkout' >&2; exit 43; fi\n")
			b.WriteString("if [ \"$round\" = 2 ]; then\n")
			b.WriteString("  case \"$prompt\" in *'Writer diff'*) ;; *) echo 'missing Writer diff header' >&2; exit 44 ;; esac\n")
			b.WriteString("  case \"$prompt\" in *'+writer-change-r1'*) ;; *) echo 'missing writer edit' >&2; exit 45 ;; esac\n")
			b.WriteString("  case \"$prompt\" in *'[REDACTED:secret]'*) ;; *) echo 'missing secret redaction marker' >&2; exit 46 ;; esac\n")
			b.WriteString("  case \"$prompt\" in *'[peer text omitted]'*) ;; *) echo 'missing cap omission marker' >&2; exit 47 ;; esac\n")
			b.WriteString("  case \"$prompt\" in *'private-fixture-secret'*) echo 'secret leaked to reviewer' >&2; exit 48 ;; esac\n")
			b.WriteString("  case \"$prompt\" in *'tail-beyond-cap'*) echo 'diff exceeded peer cap' >&2; exit 49 ;; esac\n")
			b.WriteString("  echo 'verified-writer-diff-r2'\n")
			b.WriteString("fi\n")
		}
	}

	fmt.Fprintf(&b, "echo \"role=%s mode=%s round=$round ok\"\n", role, mode)
	return b.String()
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func freeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForDaemonHealth(t *testing.T, baseURL string, logBuf *lockedBuffer) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := client.Get(baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not become healthy at %s\nlog:\n%s", baseURL, logBuf.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func doDaemonJSON(t *testing.T, method, url, token, body string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response %s %s: %v", method, url, err)
	}
	return resp.StatusCode, out
}

func TestReloadBuiltinOnlyAndReservedPanelManifests(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("requires macOS Seatbelt to register built-in council panel")
	}

	// Build sablecourtd binary.
	binDir := t.TempDir()
	daemonBin := filepath.Join(binDir, "sablecourtd")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", daemonBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build sablecourtd: %v\n%s", err, out)
	}

	// Case 1 (Finding 8): built-in-only bundle (no --manifests) reloaded via POST /api/v1/reload must not panic.
	t.Run("reload builtin-only bundle", func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		dbPath := filepath.Join(t.TempDir(), "sablecourt.db")
		tokenPath := filepath.Join(stateDir, "operator.token")
		addr := freeLocalAddr(t)

		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		var logBuf lockedBuffer
		cmd := exec.CommandContext(ctx, daemonBin, "--db", dbPath, "--listen", addr)
		cmd.Dir = t.TempDir()
		cmd.Stdout = &logBuf
		cmd.Stderr = &logBuf
		cmd.Env = []string{
			"PATH=/usr/bin:/bin",
			"HOME=" + t.TempDir(),
			"SABLECOURT_STATE_DIR=" + stateDir,
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start sablecourtd: %v", err)
		}
		t.Cleanup(func() {
			stop()
			_ = cmd.Wait()
		})

		baseURL := "http://" + addr
		waitForDaemonHealth(t, baseURL, &logBuf)

		rawToken, err := os.ReadFile(tokenPath)
		if err != nil {
			t.Fatalf("read token: %v\nlog:\n%s", err, logBuf.String())
		}
		token := strings.TrimSpace(string(rawToken))

		status, body := doDaemonJSON(t, http.MethodPost, baseURL+"/api/v1/reload", token, "{}")
		if status != http.StatusOK {
			t.Fatalf("POST /api/v1/reload = %d %s\nlog:\n%s", status, body, logBuf.String())
		}
	})

	// Case 2 (Finding 3): manifest defining reserved panel name refused at reload and does not replace built-in.
	t.Run("reload refuses reserved panel name", func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		dbPath := filepath.Join(t.TempDir(), "sablecourt.db")
		tokenPath := filepath.Join(stateDir, "operator.token")
		manifestsDir := t.TempDir()
		initialManifest := `apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: helper
  zone: runtime
spec:
  class: deterministic-controller
  runtime:
    type: docker
    image: helper:0.1.0
`
		manifestFile := filepath.Join(manifestsDir, "workloads.yaml")
		if err := os.WriteFile(manifestFile, []byte(initialManifest), 0o600); err != nil {
			t.Fatal(err)
		}
		addr := freeLocalAddr(t)

		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		var logBuf lockedBuffer
		cmd := exec.CommandContext(ctx, daemonBin, "--db", dbPath, "--listen", addr, "--manifests", manifestsDir)
		cmd.Dir = t.TempDir()
		cmd.Stdout = &logBuf
		cmd.Stderr = &logBuf
		cmd.Env = []string{
			"PATH=/usr/bin:/bin",
			"HOME=" + t.TempDir(),
			"SABLECOURT_STATE_DIR=" + stateDir,
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start sablecourtd: %v", err)
		}
		t.Cleanup(func() {
			stop()
			_ = cmd.Wait()
		})

		baseURL := "http://" + addr
		waitForDaemonHealth(t, baseURL, &logBuf)

		rawToken, err := os.ReadFile(tokenPath)
		if err != nil {
			t.Fatalf("read token: %v", err)
		}
		token := strings.TrimSpace(string(rawToken))

		reservedManifest := initialManifest + `---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: claude
  zone: sandbox
spec:
  class: coding-agent
  coding:
    harness: claude
    repoPath: /tmp/repo
    sandbox: seatbelt
  runtime:
    type: worktree-cli
  grants:
    mcpTools:
      - conclave.read
`
		if err := os.WriteFile(manifestFile, []byte(reservedManifest), 0o600); err != nil {
			t.Fatal(err)
		}
		status, body := doDaemonJSON(t, http.MethodPost, baseURL+"/api/v1/reload", token, "{}")
		if status != http.StatusBadRequest || !strings.Contains(string(body), "reserved") {
			t.Fatalf("POST /api/v1/reload with reserved name = %d %s, want 400 containing reserved", status, body)
		}
	})
}
