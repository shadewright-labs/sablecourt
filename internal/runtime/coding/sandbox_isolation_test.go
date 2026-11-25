package coding

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/operatortoken"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func TestSeatbeltRefusesOperatorStateAndSiblingWorktree(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("native Seatbelt isolation test requires macOS (running on %s)", runtime.GOOS)
	}

	for _, tc := range []struct {
		name     string
		readOnly bool
	}{
		{name: "writable", readOnly: false},
		{name: "verifier", readOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			tempState := filepath.Join(t.TempDir(), "state")
			t.Setenv("SABLECOURT_STATE_DIR", tempState)
			stateDir, err := operatortoken.StateDir()
			if err != nil {
				t.Fatalf("operatortoken.StateDir: %v", err)
			}
			if stateDir != filepath.Clean(tempState) {
				t.Fatalf("operatortoken.StateDir = %q, want %q", stateDir, tempState)
			}
			token, err := operatortoken.LoadOrCreate(stateDir)
			if err != nil {
				t.Fatalf("operatortoken.LoadOrCreate: %v", err)
			}
			canonicalStateDir, err := resolvedPath(stateDir)
			if err != nil {
				t.Fatalf("resolvedPath(stateDir): %v", err)
			}

			const dbSentinel = "sablecourt-db-sentinel-payload"
			dbPath := filepath.Join(stateDir, "sablecourt.db")
			testWrite(t, stateDir, "sablecourt.db", dbSentinel+"\n", 0o600)

			repo := initTempGitRepo(t)
			headCommit := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
			worktreeRoot := filepath.Join(filepath.Dir(dbPath), "worktrees")

			siblingAdapter := NewAdapter(worktreeRoot)
			siblingAdapter.DryRun = true
			siblingSpec := scruntime.LaunchSpec{
				TaskID: "sibling-task",
				Workload: domain.Workload{
					Metadata: domain.ObjectMeta{Name: "sibling-coder", Zone: domain.ZoneRuntime},
					Spec: domain.WorkloadSpec{
						Class: domain.ClassCodingAgent,
						Coding: &domain.CodingConfig{
							RepoPath: repo,
							Harness:  "claude",
							Sandbox:  "seatbelt",
						},
					},
				},
			}
			siblingHandle := startTestTask(t, siblingAdapter, siblingSpec)
			t.Cleanup(func() { _ = siblingAdapter.Stop(context.Background(), siblingHandle) })

			const siblingSentinel = "sibling-worktree-secret-payload"
			testWrite(t, siblingHandle.WorkDir, "sibling-secret.txt", siblingSentinel+"\n", 0o600)

			tokenPath := filepath.Join(stateDir, operatortoken.Filename)
			canonicalTokenPath := filepath.Join(canonicalStateDir, operatortoken.Filename)
			siblingFile := filepath.Join(siblingHandle.WorkDir, "sibling-secret.txt")
			siblingSession := filepath.Join(siblingHandle.WorkDir, sessionMarkerFile)

			writeCheck := `printf 'writable-ok\n' > task-output.txt || exit 30`
			if tc.readOnly {
				writeCheck = `if (printf 'nope\n' > task-output.txt) 2>/dev/null; then printf 'LEAK:verifier-write\n'; exit 31; fi`
			}

			harnessDir := t.TempDir()
			harnessPath := filepath.Join(harnessDir, "claude")
			script := fmt.Sprintf(`#!/bin/sh
set -u

/bin/cat README.md >/dev/null || exit 10
/bin/ls -A . >/dev/null || exit 11
printf 'scratch\n' > "$HOME/scratch.txt" || exit 12
printf 'tmp\n' > "$TMPDIR/tmp.txt" || exit 13

check_denied() {
    label="$1"
    shift
    err=$("$@" 2>&1 >/dev/null)
    status=$?
    if [ "$status" -eq 0 ]; then
        printf 'LEAK:%%s\n' "$label"
        exit 20
    fi
    case "$err" in
        *"Operation not permitted"*) ;;
        *)
            printf 'UNEXPECTED:%%s:%%s\n' "$label" "$err" >&2
            exit 21
            ;;
    esac
}

check_denied "operator-token" /bin/cat %q
check_denied "operator-token-canonical" /bin/cat %q
check_denied "state-dir-list" /bin/ls -A %q
check_denied "state-dir-list-canonical" /bin/ls -A %q
check_denied "daemon-db" /bin/cat %q
check_denied "sibling-worktree-file" /bin/cat %q
check_denied "sibling-worktree-session" /bin/cat %q
check_denied "sibling-worktree-list" /bin/ls -A %q
check_denied "worktree-root-list" /bin/ls -A %q
%s
printf 'isolation-verified\n'
`, tokenPath, canonicalTokenPath, stateDir, canonicalStateDir, dbPath, siblingFile, siblingSession, siblingHandle.WorkDir, worktreeRoot, writeCheck)
			testWrite(t, harnessDir, "claude", script, 0o700)

			adapter := NewAdapter(worktreeRoot)
			adapter.HarnessPaths = map[string]string{"claude": harnessPath}
			spec := scruntime.LaunchSpec{
				TaskID: "subject-" + tc.name,
				Workload: domain.Workload{
					Metadata: domain.ObjectMeta{Name: "subject-coder", Zone: domain.ZoneRuntime},
					Spec: domain.WorkloadSpec{
						Class: domain.ClassCodingAgent,
						Coding: &domain.CodingConfig{
							RepoPath: repo,
							Harness:  "claude",
							Sandbox:  "seatbelt",
						},
					},
				},
			}

			var handle scruntime.Handle
			if tc.readOnly {
				handle, err = adapter.StartAtCommit(ctx, spec, headCommit, true)
			} else {
				handle, err = adapter.Start(ctx, spec)
			}
			if err != nil {
				t.Fatalf("start %s task: %v", tc.name, err)
			}
			t.Cleanup(func() { _ = adapter.Stop(context.Background(), handle) })

			waitUntil(t, func() bool { return outputClosed(adapter.process(handle.WorkDir).done) })
			logData, _ := os.ReadFile(handle.WorkDir + ".log")

			obs, err := adapter.Observe(ctx, handle)
			if err != nil || obs.State != scruntime.StateCompleted {
				t.Fatalf("Observe = %+v, err = %v\nlog:\n%s", obs, err, string(logData))
			}

			arts, err := adapter.CollectArtifacts(ctx, handle)
			if err != nil {
				t.Fatalf("CollectArtifacts: %v", err)
			}
			var finalMsg string
			for _, art := range arts {
				for _, secret := range []string{token, dbSentinel, siblingSentinel} {
					if strings.Contains(art.Text, secret) {
						t.Fatalf("artifact %q leaked secret material", art.Name)
					}
				}
				if art.Name == "final-message" {
					finalMsg = strings.TrimSpace(art.Text)
				}
			}
			for _, secret := range []string{token, dbSentinel, siblingSentinel} {
				if strings.Contains(string(logData), secret) {
					t.Fatalf("session log leaked secret material")
				}
			}
			if finalMsg != "isolation-verified" {
				t.Fatalf("final-message = %q, want %q\nlog:\n%s", finalMsg, "isolation-verified", string(logData))
			}
			if !tc.readOnly {
				if got := testGit(t, repo, "show", handle.Branch+":task-output.txt"); got != "writable-ok\n" {
					t.Fatalf("writable task output = %q, want %q", got, "writable-ok\n")
				}
			}
		})
	}
}

func TestCheckSandbox(t *testing.T) {
	if runtime.GOOS == "darwin" {
		cmd := exec.CommandContext(t.Context(), seatbeltExecutable, "-p", "(version 1)(allow default)", "/usr/bin/true")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("native Seatbelt unavailable: %v: %s", err, out)
		}
	}
	err := CheckSandbox(context.Background())
	if runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatalf("CheckSandbox on darwin = %v, want nil", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("CheckSandbox on %s = nil, want fail-closed error", runtime.GOOS)
	}
}
