package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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

	for _, role := range []string{"claude", "codex", "agy"} {
		script := councilStubScript(role, mode, tokenPath)
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

	// Move HEAD in ref mode after open to verify all rounds stay pinned to the opening commit.
	if mode == "ref" {
		if err := os.WriteFile(filepath.Join(repo, "committed.txt"), []byte("committed-v2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		councilTestGit(t, repo, "add", "committed.txt")
		councilTestGit(t, repo, "commit", "-qm", "move head after open")
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
		t.Fatalf("mode %s finished with state=%s reason=%q (messages=%d)\ntasks:\n%s\ndaemon log:\n%s",
			mode, detail.Conclave.State, detail.Conclave.WardReason, len(detail.Messages), rawTasks, daemonLog.String())
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

	// Verify execution log contains exactly 6 launches for this council.
	tCode, rawTasks := doDaemonJSON(t, http.MethodGet, baseURL+"/api/v1/tasks", token, "")
	if tCode != http.StatusOK {
		t.Fatalf("GET /api/v1/tasks = %d %s", tCode, rawTasks)
	}
	var tasks []domain.Task
	if err := json.Unmarshal(rawTasks, &tasks); err != nil {
		t.Fatalf("decode tasks: %v", err)
	}
	launches := 0
	for _, task := range tasks {
		if a, ok := store.TaskConclaveAssignment(task); ok && a.ConclaveID == opened.ID {
			launches++
			if task.State != domain.TaskStateCompleted {
				t.Errorf("council task %s ended in state %s", task.ID, task.State)
			}
		}
	}
	if launches != 6 {
		t.Fatalf("mode %s launch count = %d, want 6", mode, launches)
	}
}

func councilStubScript(role, mode, tokenPath string) string {
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

	b.WriteString("round=1\ncase \"$prompt\" in *'### Round 1'*) round=2 ;; esac\n")

	switch mode {
	case "ref":
		b.WriteString("read -r val < committed.txt || exit 20\n")
		b.WriteString("[ \"$val\" = committed-v1 ] || { echo \"unexpected committed.txt: $val\" >&2; exit 21; }\n")
		b.WriteString("if [ -e uncommitted.txt ]; then echo 'saw uncommitted file in ref mode' >&2; exit 22; fi\n")
		b.WriteString("if (echo mutate > committed.txt) 2>/dev/null; then echo 'wrote read-only ref checkout' >&2; exit 23; fi\n")
		b.WriteString("if (echo mutate > probe.tmp) 2>/dev/null; then echo 'created file in read-only ref checkout' >&2; exit 24; fi\n")
	case "live":
		b.WriteString("read -r val < uncommitted.txt || exit 30\n")
		b.WriteString("[ \"$val\" = uncommitted-fixture ] || { echo \"unexpected uncommitted.txt: $val\" >&2; exit 31; }\n")
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
			b.WriteString("read -r val < committed.txt || exit 41\n")
			b.WriteString("[ \"$val\" = committed-v1 ] || { echo \"reviewer saw mutated checkout: $val\" >&2; exit 42; }\n")
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
