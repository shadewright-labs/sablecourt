package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/operatortoken"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func mustMarshalInput(t *testing.T, fields map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestCheckoutModeInputValidation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		input         string
		sandbox       string
		pin           string
		dryRun        bool
		wantParseFail bool
		wantFail      bool
	}{
		{name: "unknown mode", input: `{"checkoutMode":"bogus","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "empty mode string", input: `{"checkoutMode":"","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "wrong mode type", input: `{"checkoutMode":42,"prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "null mode", input: `{"checkoutMode":null,"prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "wrong ref type", input: `{"checkoutMode":"ref","checkoutRef":123,"conclaveId":"c1","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "null ref", input: `{"checkoutMode":"ref","checkoutRef":null,"conclaveId":"c1","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "empty ref", input: `{"checkoutMode":"ref","checkoutRef":"  ","conclaveId":"c1","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "missing ref in ref mode", input: `{"checkoutMode":"ref","conclaveId":"c1","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "missing conclaveId in ref mode", input: `{"checkoutMode":"ref","checkoutRef":"HEAD","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "option prefix ref", input: `{"checkoutMode":"ref","checkoutRef":"--all","conclaveId":"c1","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "range ref", input: `{"checkoutMode":"ref","checkoutRef":"HEAD~1..HEAD","conclaveId":"c1","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "whitespace ref", input: `{"checkoutMode":"ref","checkoutRef":"main branch","conclaveId":"c1","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "oversized ref", input: `{"checkoutMode":"ref","checkoutRef":"` + strings.Repeat("a", 300) + `","conclaveId":"c1","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "ref forbidden in live mode", input: `{"checkoutMode":"live","checkoutRef":"HEAD","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "ref forbidden in default mode", input: `{"checkoutRef":"HEAD","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "writer forbidden in live mode", input: `{"checkoutMode":"live","checkoutWriter":"claude","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "writer forbidden in ref mode", input: `{"checkoutMode":"ref","checkoutRef":"HEAD","checkoutWriter":"claude","conclaveId":"c1","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "writer forbidden in default mode", input: `{"checkoutWriter":"claude","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "missing writer in writer mode", input: `{"checkoutMode":"writer","conclaveId":"c1","role":"claude","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "wrong writer type", input: `{"checkoutMode":"writer","checkoutWriter":true,"conclaveId":"c1","role":"claude","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "null writer", input: `{"checkoutMode":"writer","checkoutWriter":null,"conclaveId":"c1","role":"claude","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "invalid writer role", input: `{"checkoutMode":"writer","checkoutWriter":"@claude","conclaveId":"c1","role":"claude","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "operator as writer", input: `{"checkoutMode":"writer","checkoutWriter":"operator","conclaveId":"c1","role":"claude","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "missing role in writer mode", input: `{"checkoutMode":"writer","checkoutWriter":"claude","conclaveId":"c1","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "missing conclaveId in writer mode", input: `{"checkoutMode":"writer","checkoutWriter":"claude","role":"claude","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "oversized conclaveId", input: `{"checkoutMode":"ref","checkoutRef":"HEAD","conclaveId":"` + strings.Repeat("c", 200) + `","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "duplicate checkoutMode", input: `{"checkoutMode":"live","checkoutMode":"live","prompt":"check"}`, dryRun: true, wantParseFail: true, wantFail: true},
		{name: "live mode rejects unconfined sandbox", input: `{"checkoutMode":"live","prompt":"check"}`, sandbox: "none", dryRun: false, wantFail: true},
		{name: "ref mode rejects unconfined sandbox", input: `{"checkoutMode":"ref","checkoutRef":"HEAD","conclaveId":"c1","prompt":"check"}`, sandbox: "none", dryRun: false, wantFail: true},
		{name: "writer reviewer rejects unconfined sandbox", input: `{"checkoutMode":"writer","checkoutWriter":"claude","conclaveId":"c1","role":"codex","prompt":"check"}`, sandbox: "none", dryRun: false, wantFail: true},
		{name: "startAtCommit rejects checkoutMode", input: `{"checkoutMode":"live","prompt":"check"}`, pin: "HEAD", dryRun: true, wantFail: true},
		{name: "valid live dryrun", input: `{"checkoutMode":"live","prompt":"check"}`, dryRun: true, wantFail: false},
		{name: "valid ref dryrun", input: `{"checkoutMode":"ref","checkoutRef":"HEAD","conclaveId":"c1","prompt":"check"}`, dryRun: true, wantFail: false},
		{name: "valid writer dryrun", input: `{"checkoutMode":"writer","checkoutWriter":"claude","conclaveId":"c1","role":"claude","prompt":"check"}`, dryRun: true, wantFail: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, parseErr := parseLaunchInput(tc.input); (parseErr != nil) != tc.wantParseFail {
				t.Fatalf("parseLaunchInput err = %v, wantParseFail = %t", parseErr, tc.wantParseFail)
			}
			a, spec := codingFixture(t)
			a.DryRun = tc.dryRun
			if tc.sandbox != "" {
				spec.Workload.Spec.Coding.Sandbox = tc.sandbox
			}
			spec.InputJSON = tc.input
			var (
				h   scruntime.Handle
				err error
			)
			if tc.pin != "" {
				commit := strings.TrimSpace(testGit(t, spec.Workload.Spec.Coding.RepoPath, "rev-parse", tc.pin))
				h, err = a.StartAtCommit(ctx, spec, commit, true)
			} else {
				h, err = a.Start(ctx, spec)
			}
			if (err != nil) != tc.wantFail {
				t.Fatalf("Start err = %v, wantFail = %t", err, tc.wantFail)
			}
			if err == nil {
				t.Cleanup(func() { _ = a.Stop(ctx, h) })
			}
		})
	}
}

func TestReadOnlyHarnessFlags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		harness  string
		model    string
		readOnly bool
		want     []string
	}{
		{
			name: "claude writable", harness: "claude", readOnly: false,
			want: []string{"-p", "inspect"},
		},
		{
			name: "claude read-only", harness: "claude", readOnly: true,
			want: []string{"--permission-mode", "plan", "-p", "inspect"},
		},
		{
			name: "claude read-only with model", harness: "claude", model: "opus", readOnly: true,
			want: []string{"--permission-mode", "plan", "--model", "opus", "-p", "inspect"},
		},
		{
			name: "codex writable", harness: "codex", readOnly: false,
			want: []string{"exec", "inspect"},
		},
		{
			name: "codex read-only", harness: "codex", readOnly: true,
			want: []string{"exec", "--sandbox", "read-only", "inspect"},
		},
		{
			name: "codex read-only with model", harness: "codex", model: "o3", readOnly: true,
			want: []string{"exec", "--sandbox", "read-only", "-m", "o3", "inspect"},
		},
		{
			name: "agy writable", harness: "agy", readOnly: false,
			want: []string{"-p", "inspect"},
		},
		{
			name: "agy read-only", harness: "agy", readOnly: true,
			want: []string{"--mode", "plan", "-p", "inspect"},
		},
		{
			name: "agy read-only with model", harness: "agy", model: "gemini", readOnly: true,
			want: []string{"--mode", "plan", "--model", "gemini", "-p", "inspect"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			spec.Workload.Spec.Coding.Harness = tc.harness
			spec.Workload.Spec.Coding.Sandbox = "none"
			h := startTestTask(t, a, spec)
			state, err := a.worktreeState(h)
			if err != nil {
				t.Fatal(err)
			}
			binDir := t.TempDir()
			testWrite(t, binDir, tc.harness, "#!/bin/sh\nexit 0\n", 0o700)
			a.HarnessPaths = map[string]string{tc.harness: filepath.Join(binDir, tc.harness)}
			cmd, _, err := a.buildCommand(spec, launchInput{prompt: "inspect", model: tc.model}, h.WorkDir, t.TempDir(), state, tc.readOnly)
			if err != nil {
				t.Fatal(err)
			}
			got := cmd.Args[1:]
			if !slices.Equal(got, tc.want) {
				t.Fatalf("args = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLiveModeReadOnlySandbox(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("native Seatbelt live mode test requires macOS (running on %s)", runtime.GOOS)
	}

	for _, tc := range []struct {
		harness  string
		wantFlag string
	}{
		{harness: "claude", wantFlag: "--permission-mode plan"},
		{harness: "codex", wantFlag: "exec --sandbox read-only"},
		{harness: "agy", wantFlag: "--mode plan"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			ctx := context.Background()
			tempState := filepath.Join(t.TempDir(), "state")
			t.Setenv("SABLECOURT_STATE_DIR", tempState)
			token, err := operatortoken.LoadOrCreate(tempState)
			if err != nil {
				t.Fatal(err)
			}
			tokenPath := filepath.Join(tempState, operatortoken.Filename)

			repo := initTempGitRepo(t)
			resolvedRepo, err := resolvedPath(repo)
			if err != nil {
				t.Fatal(err)
			}
			// Create uncommitted untracked, unstaged tracked, and staged fixture changes.
			const uncommittedPayload = "live-uncommitted-fixture-content\n"
			const modifiedReadme = "# test\nlive-unstaged-edit\n"
			const stagedPayload = "live-staged-fixture-content\n"
			testWrite(t, repo, "uncommitted.txt", uncommittedPayload, 0o600)
			testWrite(t, repo, "README.md", modifiedReadme, 0o600)
			testWrite(t, repo, "staged.txt", stagedPayload, 0o600)
			testGit(t, repo, "add", "staged.txt")

			harnessDir := t.TempDir()
			harnessPath := filepath.Join(harnessDir, tc.harness)
			script := fmt.Sprintf(`#!/bin/sh
set -u

args="$*"
case " $args " in
    *" %s "*) ;;
    *)
        printf 'MISSING_RO_FLAG:%%s\n' "$args" >&2
        exit 10
        ;;
esac

if [ "$(/bin/cat uncommitted.txt)" != "live-uncommitted-fixture-content" ]; then
    printf 'MISSING_UNCOMMITTED\n' >&2
    exit 11
fi
if ! /usr/bin/grep -q "live-unstaged-edit" README.md; then
    printf 'MISSING_README_EDIT\n' >&2
    exit 12
fi
if [ "$(/bin/cat staged.txt)" != "live-staged-fixture-content" ]; then
    printf 'MISSING_STAGED\n' >&2
    exit 13
fi
git status --short >/dev/null || exit 14

printf 'scratch-ok\n' > "$HOME/scratch.txt" || exit 15
printf 'tmp-ok\n' > "$TMPDIR/tmp.txt" || exit 16

check_denied() {
    label="$1"
    shift
    err=$("$@" 2>&1 >/dev/null)
    status=$?
    if [ "$status" -eq 0 ]; then
        printf 'LEAK:%%s\n' "$label" >&2
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

check_denied "overwrite-tracked" /bin/sh -c 'printf bad > README.md'
check_denied "create-file" /bin/sh -c 'printf bad > mutated.txt'
check_denied "overwrite-uncommitted" /bin/sh -c 'printf bad > uncommitted.txt'
check_denied "write-git-config" /bin/sh -c 'printf bad >> .git/config'
check_denied "read-operator-token" /bin/cat %q

printf 'live-ro-verified\n'
`, tc.wantFlag, tokenPath)
			testWrite(t, harnessDir, tc.harness, script, 0o700)

			worktreeRoot := filepath.Join(tempState, "worktrees")
			adapter := NewAdapter(worktreeRoot)
			adapter.HarnessPaths = map[string]string{tc.harness: harnessPath}
			spec := scruntime.LaunchSpec{
				TaskID: "live-" + tc.harness,
				Workload: domain.Workload{
					Metadata: domain.ObjectMeta{Name: "council-" + tc.harness, Zone: domain.ZoneRuntime},
					Spec: domain.WorkloadSpec{
						Class: domain.ClassCodingAgent,
						Coding: &domain.CodingConfig{
							RepoPath: repo,
							Harness:  tc.harness,
							Sandbox:  "seatbelt",
						},
					},
				},
				InputJSON: mustMarshalInput(t, map[string]any{
					"checkoutMode": "live",
					"conclaveId":   "ccl-live",
					"role":         tc.harness,
					"prompt":       "review live changes",
				}),
			}

			handle, err := adapter.Start(ctx, spec)
			if err != nil {
				t.Fatalf("Start live mode: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Stop(context.Background(), handle) })

			if handle.WorkDir != resolvedRepo {
				t.Fatalf("handle.WorkDir = %q, want %q", handle.WorkDir, resolvedRepo)
			}
			if handle.Branch != "" {
				t.Fatalf("handle.Branch = %q, want empty in live mode", handle.Branch)
			}
			if handle.LogPath == "" || strings.HasPrefix(handle.LogPath, resolvedRepo) {
				t.Fatalf("handle.LogPath = %q, want per-task path under worktree root", handle.LogPath)
			}

			waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(handle).done) })
			logData, _ := os.ReadFile(handle.LogPath)

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
				if strings.Contains(art.Text, token) {
					t.Fatalf("artifact %q leaked operator token", art.Name)
				}
				if art.Name == "final-message" {
					finalMsg = strings.TrimSpace(art.Text)
				}
			}
			if finalMsg != "live-ro-verified" {
				t.Fatalf("final-message = %q, want %q\nlog:\n%s", finalMsg, "live-ro-verified", string(logData))
			}

			// Verify owner checkout is completely untouched.
			if got, err := os.ReadFile(filepath.Join(repo, "uncommitted.txt")); err != nil || string(got) != uncommittedPayload {
				t.Fatalf("uncommitted.txt changed: %q, %v", got, err)
			}
			if got, err := os.ReadFile(filepath.Join(repo, "README.md")); err != nil || string(got) != modifiedReadme {
				t.Fatalf("README.md changed: %q, %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(repo, "mutated.txt")); !os.IsNotExist(err) {
				t.Fatalf("mutated.txt created in live checkout: %v", err)
			}
			for _, forbidden := range []string{sessionMarkerFile, sessionContextFile} {
				if _, err := os.Stat(filepath.Join(repo, forbidden)); !os.IsNotExist(err) {
					t.Fatalf("live mode wrote %s into owner checkout", forbidden)
				}
			}
			if wtList := testGit(t, repo, "worktree", "list", "--porcelain"); strings.Count(wtList, "worktree ") != 1 {
				t.Fatalf("live mode created unexpected worktree:\n%s", wtList)
			}
		})
	}
}

func TestRefModeSharedReadOnlyWorktree(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("native Seatbelt ref mode test requires macOS (running on %s)", runtime.GOOS)
	}

	ctx := context.Background()
	repo := initTempGitRepo(t)
	testGit(t, repo, "checkout", "-b", "feature")
	testWrite(t, repo, "feature.txt", "pinned-v1\n", 0o600)
	testGit(t, repo, "add", "feature.txt")
	testGit(t, repo, "commit", "-m", "feature v1")
	commitV1 := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	testGit(t, repo, "checkout", "-")
	// Owner working tree has an uncommitted file that ref participants must NOT see.
	testWrite(t, repo, "owner-dirty.txt", "owner-only\n", 0o600)

	harnessDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
set -u
if [ "$(/bin/cat feature.txt)" != "pinned-v1" ]; then
    printf 'WRONG_CONTENT\n' >&2
    exit 10
fi
if [ -e owner-dirty.txt ]; then
    printf 'SAW_OWNER_DIRTY\n' >&2
    exit 11
fi
if [ "$(git rev-parse HEAD)" != %q ]; then
    printf 'WRONG_HEAD:%%s\n' "$(git rev-parse HEAD)" >&2
    exit 12
fi
err=$(/bin/sh -c 'printf bad > feature.txt' 2>&1)
if [ $? -eq 0 ]; then
    printf 'LEAK:ref-write\n' >&2
    exit 13
fi
case "$err" in
    *"Operation not permitted"*) ;;
    *)
        printf 'UNEXPECTED_WRITE_ERR:%%s\n' "$err" >&2
        exit 14
        ;;
esac
printf 'ref-verified:%s\n' "$(git rev-parse HEAD)"
`, commitV1, "%s")
	for _, h := range []string{"claude", "codex", "agy"} {
		testWrite(t, harnessDir, h, script, 0o700)
	}

	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	adapter := NewAdapter(worktreeRoot)
	adapter.HarnessPaths = map[string]string{
		"claude": filepath.Join(harnessDir, "claude"),
		"codex":  filepath.Join(harnessDir, "codex"),
		"agy":    filepath.Join(harnessDir, "agy"),
	}

	makeSpec := func(taskID, harness, ref string) scruntime.LaunchSpec {
		return scruntime.LaunchSpec{
			TaskID: taskID,
			Workload: domain.Workload{
				Metadata: domain.ObjectMeta{Name: "council-" + harness, Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassCodingAgent,
					Coding: &domain.CodingConfig{
						RepoPath: repo,
						Harness:  harness,
						Sandbox:  "seatbelt",
					},
				},
			},
			InputJSON: mustMarshalInput(t, map[string]any{
				"checkoutMode": "ref",
				"checkoutRef":  ref,
				"conclaveId":   "ccl-ref-1",
				"role":         harness,
				"prompt":       "review feature branch",
			}),
		}
	}

	// Invalid ref fails closed and leaves no worktree behind.
	if _, err := adapter.Start(ctx, makeSpec("ref-invalid", "claude", "nonexistent-branch")); err == nil {
		t.Fatal("accepted nonexistent ref")
	}
	if wtList := testGit(t, repo, "worktree", "list", "--porcelain"); strings.Count(wtList, "worktree ") != 1 {
		t.Fatalf("invalid ref left worktree behind:\n%s", wtList)
	}

	// Start participant 1 on "feature" (pinned to commitV1).
	h1, err := adapter.Start(ctx, makeSpec("ref-p1", "claude", "feature"))
	if err != nil {
		t.Fatalf("Start p1: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Stop(context.Background(), h1) })

	// Move the "feature" branch forward AFTER p1 started.
	testGit(t, repo, "checkout", "feature")
	testWrite(t, repo, "feature.txt", "moved-v2\n", 0o600)
	testGit(t, repo, "add", "feature.txt")
	testGit(t, repo, "commit", "-m", "feature v2")
	testGit(t, repo, "checkout", "-")

	// Mismatched ref for the same council must fail closed.
	if _, err := adapter.Start(ctx, makeSpec("ref-mismatch", "codex", "HEAD")); err == nil {
		t.Fatal("accepted mismatched ref for existing council")
	}

	// Start participants 2 and 3 concurrently; both must reuse p1's pinned commitV1 worktree.
	var (
		wg     sync.WaitGroup
		h2, h3 scruntime.Handle
		err2   error
		err3   error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		h2, err2 = adapter.Start(ctx, makeSpec("ref-p2", "codex", "feature"))
	}()
	go func() {
		defer wg.Done()
		h3, err3 = adapter.Start(ctx, makeSpec("ref-p3", "agy", "feature"))
	}()
	wg.Wait()
	if err2 != nil {
		t.Fatalf("Start p2: %v", err2)
	}
	if err3 != nil {
		t.Fatalf("Start p3: %v", err3)
	}
	t.Cleanup(func() {
		_ = adapter.Stop(context.Background(), h2)
		_ = adapter.Stop(context.Background(), h3)
	})

	if h1.WorkDir != h2.WorkDir || h1.WorkDir != h3.WorkDir {
		t.Fatalf("participants did not share one worktree: %q, %q, %q", h1.WorkDir, h2.WorkDir, h3.WorkDir)
	}

	// CleanupCouncil must refuse while a task is still active.
	if err := adapter.CleanupCouncil(ctx, "ccl-ref-1"); err == nil {
		t.Fatal("CleanupCouncil succeeded while ref tasks were active")
	}

	for _, h := range []scruntime.Handle{h1, h2, h3} {
		waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(h).done) })
		fresh := NewAdapter(worktreeRoot)
		if exited, err := fresh.HandleExited(ctx, h); err != nil || !exited {
			t.Fatalf("HandleExited(%s) = %t, %v", h.ExternalID, exited, err)
		}
		obs, err := adapter.Observe(ctx, h)
		if err != nil || obs.State != scruntime.StateCompleted {
			logData, _ := os.ReadFile(h.LogPath)
			t.Fatalf("Observe %s = %+v, err = %v\nlog:\n%s", h.ExternalID, obs, err, string(logData))
		}
		arts, err := adapter.CollectArtifacts(ctx, h)
		if err != nil {
			t.Fatalf("CollectArtifacts %s: %v", h.ExternalID, err)
		}
		var finalMsg string
		for _, art := range arts {
			if art.Name == "final-message" {
				finalMsg = strings.TrimSpace(art.Text)
			}
		}
		wantMsg := "ref-verified:" + commitV1
		if finalMsg != wantMsg {
			t.Fatalf("%s final-message = %q, want %q", h.ExternalID, finalMsg, wantMsg)
		}
	}

	if err := adapter.CleanupCouncil(ctx, "ccl-ref-1"); err != nil {
		t.Fatalf("CleanupCouncil: %v", err)
	}
	if wtList := testGit(t, repo, "worktree", "list", "--porcelain"); strings.Count(wtList, "worktree ") != 1 {
		t.Fatalf("CleanupCouncil left worktree behind:\n%s", wtList)
	}

	// Verify ref mode also accepts a raw SHA commit ID.
	shaHandle, err := adapter.Start(ctx, scruntime.LaunchSpec{
		TaskID: "ref-sha",
		Workload: domain.Workload{
			Metadata: domain.ObjectMeta{Name: "council-claude", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{
				Class: domain.ClassCodingAgent,
				Coding: &domain.CodingConfig{
					RepoPath: repo,
					Harness:  "claude",
					Sandbox:  "seatbelt",
				},
			},
		},
		InputJSON: mustMarshalInput(t, map[string]any{
			"checkoutMode": "ref",
			"checkoutRef":  commitV1,
			"conclaveId":   "ccl-ref-sha",
			"role":         "claude",
			"prompt":       "review raw sha",
		}),
	})
	if err != nil {
		t.Fatalf("Start ref raw SHA: %v", err)
	}
	t.Cleanup(func() {
		_ = adapter.Stop(context.Background(), shaHandle)
		_ = adapter.CleanupCouncil(context.Background(), "ccl-ref-sha")
	})
	waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(shaHandle).done) })
	if obs, err := adapter.Observe(ctx, shaHandle); err != nil || obs.State != scruntime.StateCompleted {
		t.Fatalf("Observe ref raw SHA = %+v, err = %v", obs, err)
	}
	if err := adapter.CleanupCouncil(ctx, "ccl-ref-sha"); err != nil {
		t.Fatalf("CleanupCouncil raw SHA: %v", err)
	}
}

func TestWriterModeSingleWriterAndReviewers(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("native Seatbelt writer mode test requires macOS (running on %s)", runtime.GOOS)
	}

	ctx := context.Background()
	repo := initTempGitRepo(t)
	baseCommit := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	harnessDir := t.TempDir()

	// Writer commits one file, stages another, edits tracked README unstaged, and leaves an untracked file.
	writerScript := `#!/bin/sh
set -e
printf 'committed-change\n' > committed.txt
git add -- committed.txt
git commit --no-gpg-sign -m 'writer commit'
printf 'staged-change\n' > staged.txt
git add -- staged.txt
printf 'unstaged-readme\n' >> README.md
printf 'untracked-change\n' > untracked.txt
printf 'writer-round1-done\n'
`
	reviewerScript := fmt.Sprintf(`#!/bin/sh
set -u
if [ "$(git rev-parse HEAD)" != %q ]; then
    printf 'WRONG_BASE_HEAD:%%s\n' "$(git rev-parse HEAD)" >&2
    exit 10
fi
for f in committed.txt staged.txt untracked.txt; do
    if [ -e "$f" ]; then
        printf 'LEAK_WRITER_FILE:%%s\n' "$f" >&2
        exit 11
    fi
done
if /usr/bin/grep -q "unstaged-readme" README.md; then
    printf 'LEAK_README_EDIT\n' >&2
    exit 12
fi
err=$(/bin/sh -c 'printf bad > README.md' 2>&1)
if [ $? -eq 0 ]; then
    printf 'LEAK:reviewer-write\n' >&2
    exit 14
fi
case "$err" in
    *"Operation not permitted"*) ;;
    *)
        printf 'UNEXPECTED_WRITE_ERR:%%s\n' "$err" >&2
        exit 15
        ;;
esac
printf 'reviewer-verified\n'
`, baseCommit)
	testWrite(t, harnessDir, "claude", writerScript, 0o700)
	testWrite(t, harnessDir, "codex", reviewerScript, 0o700)
	testWrite(t, harnessDir, "agy", reviewerScript, 0o700)

	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	adapter := NewAdapter(worktreeRoot)
	adapter.HarnessPaths = map[string]string{
		"claude": filepath.Join(harnessDir, "claude"),
		"codex":  filepath.Join(harnessDir, "codex"),
		"agy":    filepath.Join(harnessDir, "agy"),
	}

	makeSpec := func(taskID, harness, role, writer string) scruntime.LaunchSpec {
		return scruntime.LaunchSpec{
			TaskID: taskID,
			Workload: domain.Workload{
				Metadata: domain.ObjectMeta{Name: "council-" + harness, Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassCodingAgent,
					Coding: &domain.CodingConfig{
						RepoPath: repo,
						Harness:  harness,
						Sandbox:  "seatbelt",
					},
				},
			},
			InputJSON: mustMarshalInput(t, map[string]any{
				"checkoutMode":   "writer",
				"checkoutWriter": writer,
				"conclaveId":     "ccl-write-1",
				"role":           role,
				"prompt":         "implement and review",
			}),
		}
	}

	// Start writer (claude).
	wHandle, err := adapter.Start(ctx, makeSpec("write-r1-claude", "claude", "claude", "claude"))
	if err != nil {
		t.Fatalf("Start writer: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Stop(context.Background(), wHandle) })
	if wHandle.Branch == "" {
		t.Fatal("writer handle has empty branch")
	}

	// Second writer role must fail closed.
	if _, err := adapter.Start(ctx, makeSpec("write-second-role", "codex", "codex", "codex")); err == nil {
		t.Fatal("accepted a second writer role for the same council")
	}
	// Concurrent second start for the same writer must fail closed.
	if _, err := adapter.Start(ctx, makeSpec("write-concurrent", "claude", "claude", "claude")); err == nil {
		t.Fatal("accepted concurrent second writer run")
	}

	waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(wHandle).done) })
	obs, err := adapter.Observe(ctx, wHandle)
	if err != nil || obs.State != scruntime.StateCompleted {
		logData, _ := os.ReadFile(wHandle.LogPath)
		t.Fatalf("Observe writer = %+v, err = %v\nlog:\n%s", obs, err, string(logData))
	}

	wArts, err := adapter.CollectArtifacts(ctx, wHandle)
	if err != nil {
		t.Fatalf("CollectArtifacts writer: %v", err)
	}
	var wDiff string
	for _, art := range wArts {
		if art.Name == "git-diff" {
			wDiff = art.Text
		}
	}
	for _, want := range []string{"+committed-change", "+staged-change", "+untracked-change", "+unstaged-readme"} {
		if !strings.Contains(wDiff, want) {
			t.Fatalf("writer git-diff missing %q:\n%s", want, wDiff)
		}
	}

	// Launch both reviewers (codex and agy); both must see committed + uncommitted changes and fail writes.
	r1, err := adapter.Start(ctx, makeSpec("write-r1-codex", "codex", "codex", "claude"))
	if err != nil {
		t.Fatalf("Start reviewer codex: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Stop(context.Background(), r1) })
	r2, err := adapter.Start(ctx, makeSpec("write-r1-agy", "agy", "agy", "claude"))
	if err != nil {
		t.Fatalf("Start reviewer agy: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Stop(context.Background(), r2) })

	if r1.WorkDir != r2.WorkDir {
		t.Fatalf("reviewers did not share review worktree: %q vs %q", r1.WorkDir, r2.WorkDir)
	}

	for _, rh := range []scruntime.Handle{r1, r2} {
		waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(rh).done) })
		robs, err := adapter.Observe(ctx, rh)
		if err != nil || robs.State != scruntime.StateCompleted {
			logData, _ := os.ReadFile(rh.LogPath)
			t.Fatalf("Observe reviewer %s = %+v, err = %v\nlog:\n%s", rh.ExternalID, robs, err, string(logData))
		}
		arts, err := adapter.CollectArtifacts(ctx, rh)
		if err != nil {
			t.Fatalf("CollectArtifacts reviewer %s: %v", rh.ExternalID, err)
		}
		var msg string
		for _, art := range arts {
			if art.Name == "final-message" {
				msg = strings.TrimSpace(art.Text)
			}
		}
		if msg != "reviewer-verified" {
			t.Fatalf("reviewer %s final-message = %q, want reviewer-verified", rh.ExternalID, msg)
		}
	}

	// Round 2: writer reuses council worktree and adds another uncommitted edit;
	// writer diff artifact carries round 2 edit while round 2 reviewer stays on pinned base.
	testWrite(t, harnessDir, "claude", "#!/bin/sh\nset -e\nprintf 'round2-edit\\n' > round2.txt\nprintf 'writer-r2-done\\n'\n", 0o700)
	testWrite(t, harnessDir, "codex", fmt.Sprintf("#!/bin/sh\nset -e\ntest \"$(git rev-parse HEAD)\" = %q\ntest ! -e round2.txt\ntest ! -e committed.txt\nprintf 'r2-review-ok\\n'\n", baseCommit), 0o700)

	w2, err := adapter.Start(ctx, makeSpec("write-r2-claude", "claude", "claude", "claude"))
	if err != nil {
		t.Fatalf("Start writer round 2: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Stop(context.Background(), w2) })
	if w2.WorkDir != wHandle.WorkDir || w2.Branch != wHandle.Branch {
		t.Fatalf("writer round 2 did not reuse council worktree/branch: (%q,%q) vs (%q,%q)", w2.WorkDir, w2.Branch, wHandle.WorkDir, wHandle.Branch)
	}
	waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(w2).done) })
	if obs, err := adapter.Observe(ctx, w2); err != nil || obs.State != scruntime.StateCompleted {
		t.Fatalf("Observe writer round 2 = %+v, err = %v", obs, err)
	}
	w2Arts, err := adapter.CollectArtifacts(ctx, w2)
	if err != nil {
		t.Fatalf("CollectArtifacts writer round 2: %v", err)
	}
	var w2Diff string
	for _, art := range w2Arts {
		if art.Name == "git-diff" {
			w2Diff = art.Text
		}
	}
	if !strings.Contains(w2Diff, "+round2-edit") {
		t.Fatalf("writer round 2 git-diff missing +round2-edit:\n%s", w2Diff)
	}

	// Re-collecting artifacts from round-1 writer must NOT regress WriterTip back to round 1.
	if _, err := adapter.CollectArtifacts(ctx, wHandle); err != nil {
		t.Fatalf("re-collect round 1 writer artifacts: %v", err)
	}

	r3, err := adapter.Start(ctx, makeSpec("write-r2-codex", "codex", "codex", "claude"))
	if err != nil {
		t.Fatalf("Start reviewer round 2: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Stop(context.Background(), r3) })
	waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(r3).done) })
	if obs, err := adapter.Observe(ctx, r3); err != nil || obs.State != scruntime.StateCompleted {
		logData, _ := os.ReadFile(r3.LogPath)
		t.Fatalf("Observe reviewer round 2 = %+v, err = %v\nlog:\n%s", obs, err, string(logData))
	}

	// Halfway failure on writer round 3 must preserve existing council worktree and allow retry.
	savedClaude := adapter.HarnessPaths["claude"]
	adapter.HarnessPaths["claude"] = filepath.Join(t.TempDir(), "missing-claude")
	if _, err := adapter.Start(ctx, makeSpec("write-r3-claude", "claude", "claude", "claude")); err == nil {
		t.Fatal("expected round 3 writer failure with missing harness")
	}
	adapter.HarnessPaths["claude"] = savedClaude
	testWrite(t, harnessDir, "claude", "#!/bin/sh\nset -e\ntest \"$(/bin/cat round2.txt)\" = 'round2-edit'\nprintf 'writer-r3-ok\\n'\n", 0o700)
	w3, err := adapter.Start(ctx, makeSpec("write-r3-claude", "claude", "claude", "claude"))
	if err != nil {
		t.Fatalf("retry writer round 3 failed: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Stop(context.Background(), w3) })
	waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(w3).done) })
	if obs, err := adapter.Observe(ctx, w3); err != nil || obs.State != scruntime.StateCompleted {
		t.Fatalf("Observe writer round 3 = %+v, err = %v", obs, err)
	}

	// CleanupCouncil removes worktrees while preserving the writer's branch.
	if err := adapter.CleanupCouncil(ctx, "ccl-write-1"); err != nil {
		t.Fatalf("CleanupCouncil: %v", err)
	}
	if wtList := testGit(t, repo, "worktree", "list", "--porcelain"); strings.Count(wtList, "worktree ") != 1 {
		t.Fatalf("CleanupCouncil left worktree behind:\n%s", wtList)
	}
	if got := testGit(t, repo, "show", wHandle.Branch+":round2.txt"); got != "round2-edit\n" {
		t.Fatalf("writer branch missing round2.txt: %q", got)
	}
}

func TestCheckoutModeRollbackAndRetry(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("native Seatbelt rollback test requires macOS (running on %s)", runtime.GOOS)
	}

	for _, tc := range []struct {
		name  string
		input map[string]any
	}{
		{
			name:  "live mode",
			input: map[string]any{"checkoutMode": "live", "prompt": "check"},
		},
		{
			name:  "ref mode",
			input: map[string]any{"checkoutMode": "ref", "checkoutRef": "HEAD", "conclaveId": "ccl-rb-ref", "prompt": "check"},
		},
		{
			name:  "writer mode",
			input: map[string]any{"checkoutMode": "writer", "checkoutWriter": "claude", "conclaveId": "ccl-rb-writer", "role": "claude", "prompt": "check"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo := initTempGitRepo(t)
			worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
			adapter := NewAdapter(worktreeRoot)

			badDir := t.TempDir()
			badBin := filepath.Join(badDir, "missing-claude")
			adapter.HarnessPaths = map[string]string{"claude": badBin}

			spec := scruntime.LaunchSpec{
				TaskID: "retry-task",
				Workload: domain.Workload{
					Metadata: domain.ObjectMeta{Name: "council-claude", Zone: domain.ZoneRuntime},
					Spec: domain.WorkloadSpec{
						Class: domain.ClassCodingAgent,
						Coding: &domain.CodingConfig{
							RepoPath: repo,
							Harness:  "claude",
							Sandbox:  "seatbelt",
						},
					},
				},
				InputJSON: mustMarshalInput(t, tc.input),
			}

			if _, err := adapter.Start(ctx, spec); err == nil {
				t.Fatal("expected start failure with broken interpreter")
			}

			// Verify clean rollback: no leftover worktrees or branches.
			if wtList := testGit(t, repo, "worktree", "list", "--porcelain"); strings.Count(wtList, "worktree ") != 1 {
				t.Fatalf("failed start left worktree behind:\n%s", wtList)
			}
			if branches := testGit(t, repo, "branch", "--list", "sablecourt/*"); branches != "" {
				t.Fatalf("failed start left branch behind: %q", branches)
			}

			// Fix harness and retry with the exact same TaskID.
			goodDir := t.TempDir()
			goodBin := filepath.Join(goodDir, "claude")
			testWrite(t, goodDir, "claude", "#!/bin/sh\nprintf 'retry-ok\\n'\n", 0o700)
			adapter.HarnessPaths = map[string]string{"claude": goodBin}

			h, err := adapter.Start(ctx, spec)
			if err != nil {
				t.Fatalf("retry Start failed: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Stop(context.Background(), h) })
			waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(h).done) })
			if obs, err := adapter.Observe(ctx, h); err != nil || obs.State != scruntime.StateCompleted {
				t.Fatalf("retry Observe = %+v, err = %v", obs, err)
			}
		})
	}
}

func TestParseCheckoutRefValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ref      string
		wantFail bool
	}{
		{name: "option prefix", ref: "--all", wantFail: true},
		{name: "leading dash", ref: "-main", wantFail: true},
		{name: "range", ref: "HEAD~1..HEAD", wantFail: true},
		{name: "whitespace", ref: "main branch", wantFail: true},
		{name: "oversized", ref: strings.Repeat("a", 300), wantFail: true},
		{name: "reflog expression", ref: "HEAD@{0}", wantFail: true},
		{name: "upstream reflog", ref: "@{upstream}", wantFail: true},
		{name: "bare at", ref: "@", wantFail: true},
		{name: "tilde zero", ref: "HEAD~0", wantFail: true},
		{name: "tilde ancestor", ref: "HEAD~1", wantFail: true},
		{name: "caret parent", ref: "HEAD^", wantFail: true},
		{name: "caret second parent", ref: "main^2", wantFail: true},
		{name: "colon tree path", ref: "HEAD:README.md", wantFail: true},
		{name: "relative dot slash path", ref: "./HEAD", wantFail: true},
		{name: "parent dotdot path", ref: "../HEAD", wantFail: true},
		{name: "absolute path", ref: "/etc/passwd", wantFail: true},
		{name: "inner dot segment", ref: "feat/./x", wantFail: true},
		{name: "double slash", ref: "feat//x", wantFail: true},
		{name: "trailing slash", ref: "feat/", wantFail: true},
		{name: "trailing dot", ref: "feat.", wantFail: true},
		{name: "dot component", ref: "feat/.hidden", wantFail: true},
		{name: "lock suffix", ref: "feat.lock", wantFail: true},
		{name: "backslash", ref: `feat\x`, wantFail: true},
		{name: "glob star", ref: "feat/*", wantFail: true},
		{name: "glob question", ref: "feat/?", wantFail: true},
		{name: "bracket", ref: "feat/[a]", wantFail: true},
		{name: "non branch or tag ref namespace", ref: "refs/stash", wantFail: true},
		{name: "HEAD valid", ref: "HEAD", wantFail: false},
		{name: "branch valid", ref: "feature/checkout-31", wantFail: false},
		{name: "tag valid", ref: "v1.2.3", wantFail: false},
		{name: "full refs heads valid", ref: "refs/heads/main", wantFail: false},
		{name: "full refs tags valid", ref: "refs/tags/v1.0.0", wantFail: false},
		{name: "full sha1 valid", ref: strings.Repeat("a1", 20), wantFail: false},
		{name: "full sha256 valid", ref: strings.Repeat("b2", 32), wantFail: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := mustMarshalInput(t, map[string]any{
				"checkoutMode": "ref",
				"checkoutRef":  tc.ref,
				"conclaveId":   "c1",
				"prompt":       "check",
			})
			_, err := parseLaunchInput(raw)
			if (err != nil) != tc.wantFail {
				t.Fatalf("parseLaunchInput(%q) err = %v, wantFail = %t", tc.ref, err, tc.wantFail)
			}
			if tc.wantFail && !strings.Contains(err.Error(), "invalid coding checkoutRef") {
				t.Fatalf("parseLaunchInput(%q) err = %v, want invalid coding checkoutRef", tc.ref, err)
			}
		})
	}
}

func TestRefModeRejectsShortCommitPrefix(t *testing.T) {
	ctx := context.Background()
	a, spec := codingFixture(t)
	a.DryRun = true
	full := strings.TrimSpace(testGit(t, spec.Workload.Spec.Coding.RepoPath, "rev-parse", "HEAD"))
	spec.InputJSON = mustMarshalInput(t, map[string]any{
		"checkoutMode": "ref",
		"checkoutRef":  full[:7],
		"conclaveId":   "ccl-short-sha",
		"prompt":       "check",
	})
	if _, err := a.Start(ctx, spec); err == nil {
		t.Fatalf("accepted abbreviated commit prefix %q", full[:7])
	}
}

func TestCouncilInputWithoutCheckoutModeRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    string
		wantFail bool
	}{
		{
			name:     "round one council task without checkoutMode",
			input:    `{"conclaveId":"ccl-1","role":"claude","round":1,"attempt":1,"prompt":"review"}`,
			wantFail: true,
		},
		{
			name:     "council task without activation or checkoutMode",
			input:    `{"conclaveId":"ccl-1","role":"claude","prompt":"review"}`,
			wantFail: true,
		},
		{
			name:     "legacy conclave always-on task allowed",
			input:    `{"conclaveId":"room-1","role":"coder","activation":"always-on","prompt":"review"}`,
			wantFail: false,
		},
		{
			name:     "ordinary coding task without conclaveId allowed",
			input:    `{"prompt":"fix bug","round":1}`,
			wantFail: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, parseErr := parseLaunchInput(tc.input)
			if (parseErr != nil) != tc.wantFail {
				t.Fatalf("parseLaunchInput err = %v, wantFail = %t", parseErr, tc.wantFail)
			}
			a, spec := codingFixture(t)
			a.DryRun = true
			spec.InputJSON = tc.input
			h, err := a.Start(context.Background(), spec)
			if (err != nil) != tc.wantFail {
				t.Fatalf("Start err = %v, wantFail = %t", err, tc.wantFail)
			}
			if err == nil {
				t.Cleanup(func() { _ = a.Stop(context.Background(), h) })
			}
		})
	}
}

func TestWriterModeReviewersIsolatedOnPinnedBaseAndDiffArtifact(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("native Seatbelt test requires macOS (running on %s)", runtime.GOOS)
	}
	ctx := context.Background()
	repo := initTempGitRepo(t)
	baseCommit := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	outsideSecret := filepath.Join(t.TempDir(), "outside-secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("outside-secret-data\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	harnessDir := t.TempDir()
	// Writer commits one file, writes a 42 KiB uncommitted file containing a synthetic secret,
	// and creates a symlink pointing outside its checkout.
	writerScript := fmt.Sprintf(`#!/bin/sh
set -e
printf 'committed-line\n' > committed.txt
git add -- committed.txt
git commit --no-gpg-sign -m 'writer commit'
printf 'uncommitted-line\n' > small.txt
/usr/bin/awk 'BEGIN { for (i=0; i<420; i++) printf "012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678\n"; print "password=fixture-secret-value-9999" }' > big-secret.txt
ln -s %q aaa-escape-link
printf 'writer-done\n'
`, outsideSecret)
	testWrite(t, harnessDir, "claude", writerScript, 0o700)

	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	adapter := NewAdapter(worktreeRoot)
	adapter.HarnessPaths = map[string]string{
		"claude": filepath.Join(harnessDir, "claude"),
	}

	makeSpec := func(taskID, harness, role string) scruntime.LaunchSpec {
		return scruntime.LaunchSpec{
			TaskID: taskID,
			Workload: domain.Workload{
				Metadata: domain.ObjectMeta{Name: "council-" + harness, Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassCodingAgent,
					Coding: &domain.CodingConfig{
						RepoPath: repo,
						Harness:  harness,
						Sandbox:  "seatbelt",
					},
				},
			},
			InputJSON: mustMarshalInput(t, map[string]any{
				"checkoutMode":   "writer",
				"checkoutWriter": "claude",
				"conclaveId":     "ccl-writer-iso",
				"role":           role,
				"prompt":         "implement and review",
			}),
		}
	}

	wHandle, err := adapter.Start(ctx, makeSpec("w-r1", "claude", "claude"))
	if err != nil {
		t.Fatalf("Start writer: %v", err)
	}
	t.Cleanup(func() {
		_ = adapter.Stop(context.Background(), wHandle)
		_ = adapter.CleanupCouncil(context.Background(), "ccl-writer-iso")
	})
	waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(wHandle).done) })
	obs, err := adapter.Observe(ctx, wHandle)
	if err != nil || obs.State != scruntime.StateCompleted {
		t.Fatalf("Observe writer = %+v, err = %v", obs, err)
	}
	arts, err := adapter.CollectArtifacts(ctx, wHandle)
	if err != nil {
		t.Fatalf("CollectArtifacts writer: %v", err)
	}
	var diffText, writerTip string
	for _, art := range arts {
		if art.Name == "git-diff" {
			diffText = art.Text
		}
		if art.Name == "git-commit" {
			writerTip = art.Text
		}
	}
	if diffText == "" || writerTip == "" || writerTip == baseCommit {
		t.Fatalf("missing writer diff or tip: len(diff)=%d tip=%q base=%q", len(diffText), writerTip, baseCommit)
	}
	if len(diffText) > 32*1024+len("\n...[truncated]") {
		t.Fatalf("writer git-diff exceeded cap: %d bytes", len(diffText))
	}
	if strings.Contains(diffText, "fixture-secret-value-9999") {
		t.Fatalf("writer git-diff leaked unredacted secret")
	}
	if strings.Contains(diffText, "aaa-escape-link") || strings.Contains(diffText, outsideSecret) {
		t.Fatalf("writer git-diff included escaping symlink the writer could not read:\n%s", diffText)
	}

	// Reviewer must stay on pinned baseCommit with isolated Git objects (no writer tree, no writer Git objects).
	reviewerScript := fmt.Sprintf(`#!/bin/sh
set -u
if [ "$(git rev-parse HEAD)" != %q ]; then
    printf 'WRONG_BASE:%%s\n' "$(git rev-parse HEAD)" >&2
    exit 10
fi
for f in committed.txt small.txt big-secret.txt aaa-escape-link; do
    if [ -e "$f" ]; then
        printf 'LEAK_FILE:%%s\n' "$f" >&2
        exit 11
    fi
done
if git cat-file -e %q 2>/dev/null; then
    printf 'LEAK_WRITER_COMMIT\n' >&2
    exit 12
fi
if git show %q:committed.txt >/dev/null 2>&1; then
    printf 'LEAK_WRITER_BRANCH\n' >&2
    exit 13
fi
err=$(/bin/sh -c 'printf bad > README.md' 2>&1)
if [ $? -eq 0 ]; then
    printf 'LEAK_WRITE\n' >&2
    exit 14
fi
case "$err" in
    *"Operation not permitted"*) ;;
    *)
        printf 'UNEXPECTED_WRITE_ERR:%%s\n' "$err" >&2
        exit 15
        ;;
esac
printf 'reviewer-isolated-ok\n'
`, baseCommit, writerTip, wHandle.Branch)
	testWrite(t, harnessDir, "codex", reviewerScript, 0o700)
	adapter.HarnessPaths["codex"] = filepath.Join(harnessDir, "codex")

	rHandle, err := adapter.Start(ctx, makeSpec("r-r1", "codex", "codex"))
	if err != nil {
		t.Fatalf("Start reviewer: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Stop(context.Background(), rHandle) })
	waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(rHandle).done) })
	logData, _ := os.ReadFile(rHandle.LogPath)
	rObs, err := adapter.Observe(ctx, rHandle)
	if err != nil || rObs.State != scruntime.StateCompleted {
		t.Fatalf("Observe reviewer = %+v, err = %v\nlog:\n%s", rObs, err, string(logData))
	}
}

func TestWriterModeFailedTurnPublishesNothing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("native Seatbelt test requires macOS (running on %s)", runtime.GOOS)
	}
	ctx := context.Background()
	repo := initTempGitRepo(t)
	baseCommit := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	harnessDir := t.TempDir()

	// Writer makes both a committed edit and a partial uncommitted edit, then exits 1.
	testWrite(t, harnessDir, "claude", `#!/bin/sh
set -e
printf 'failed-commit\n' > failed-commit.txt
git add -- failed-commit.txt
git commit --no-gpg-sign -m 'failed attempt commit'
printf 'partial-edit\n' > partial.txt
exit 1
`, 0o700)
	testWrite(t, harnessDir, "codex", fmt.Sprintf(`#!/bin/sh
set -u
if [ "$(git rev-parse HEAD)" != %q ]; then
    printf 'WRONG_HEAD:%%s\n' "$(git rev-parse HEAD)" >&2
    exit 10
fi
if [ -e partial.txt ] || [ -e failed-commit.txt ]; then
    printf 'LEAK_FAILED_EDIT\n' >&2
    exit 11
fi
printf 'reviewer-clean\n'
`, baseCommit), 0o700)

	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	adapter := NewAdapter(worktreeRoot)
	adapter.HarnessPaths = map[string]string{
		"claude": filepath.Join(harnessDir, "claude"),
		"codex":  filepath.Join(harnessDir, "codex"),
	}
	makeSpec := func(taskID, harness, role string) scruntime.LaunchSpec {
		return scruntime.LaunchSpec{
			TaskID: taskID,
			Workload: domain.Workload{
				Metadata: domain.ObjectMeta{Name: "council-" + harness, Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassCodingAgent,
					Coding: &domain.CodingConfig{
						RepoPath: repo,
						Harness:  harness,
						Sandbox:  "seatbelt",
					},
				},
			},
			InputJSON: mustMarshalInput(t, map[string]any{
				"checkoutMode":   "writer",
				"checkoutWriter": "claude",
				"conclaveId":     "ccl-writer-fail",
				"role":           role,
				"prompt":         "implement",
			}),
		}
	}

	wHandle, err := adapter.Start(ctx, makeSpec("wf-1", "claude", "claude"))
	if err != nil {
		t.Fatalf("Start failed writer: %v", err)
	}
	t.Cleanup(func() {
		_ = adapter.Stop(context.Background(), wHandle)
		_ = adapter.CleanupCouncil(context.Background(), "ccl-writer-fail")
	})
	waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(wHandle).done) })
	obs, err := adapter.Observe(ctx, wHandle)
	if err != nil || obs.State != scruntime.StateFailed || obs.ExitCode != 1 {
		t.Fatalf("Observe failed writer = %+v, err = %v", obs, err)
	}
	arts, err := adapter.CollectArtifacts(ctx, wHandle)
	if err != nil {
		t.Fatalf("CollectArtifacts failed writer: %v", err)
	}
	for _, art := range arts {
		if art.Name == "git-diff" || art.Name == "git-status" || art.Name == "git-commit" {
			t.Fatalf("failed writer turn published artifact %q: %q", art.Name, art.Text)
		}
	}
	resolvedRoot, err := resolvedPath(worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := readCouncilState(councilStatePath(resolvedRoot, councilKey("ccl-writer-fail")))
	if err != nil {
		t.Fatal(err)
	}
	if cs.WriterTip != "" {
		t.Fatalf("failed writer turn advanced WriterTip to %q", cs.WriterTip)
	}

	rHandle, err := adapter.Start(ctx, makeSpec("rf-1", "codex", "codex"))
	if err != nil {
		t.Fatalf("Start reviewer after failed writer: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Stop(context.Background(), rHandle) })
	waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(rHandle).done) })
	logData, _ := os.ReadFile(rHandle.LogPath)
	rObs, err := adapter.Observe(ctx, rHandle)
	if err != nil || rObs.State != scruntime.StateCompleted {
		t.Fatalf("Observe reviewer = %+v, err = %v\nlog:\n%s", rObs, err, string(logData))
	}
	if err := adapter.CleanupCouncil(ctx, "ccl-writer-fail"); err != nil {
		t.Fatalf("CleanupCouncil after failed writer: %v", err)
	}
}

func TestWriterModeSingleWriterAcrossAdapterInstances(t *testing.T) {
	ctx := context.Background()
	repo := initTempGitRepo(t)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	a1 := NewAdapter(worktreeRoot)
	a1.DryRun = true
	a2 := NewAdapter(worktreeRoot)
	a2.DryRun = true

	makeSpec := func(taskID string) scruntime.LaunchSpec {
		return scruntime.LaunchSpec{
			TaskID: taskID,
			Workload: domain.Workload{
				Metadata: domain.ObjectMeta{Name: "council-claude", Zone: domain.ZoneRuntime},
				Spec: domain.WorkloadSpec{
					Class: domain.ClassCodingAgent,
					Coding: &domain.CodingConfig{
						RepoPath: repo,
						Harness:  "claude",
						Sandbox:  "seatbelt",
					},
				},
			},
			InputJSON: mustMarshalInput(t, map[string]any{
				"checkoutMode":   "writer",
				"checkoutWriter": "claude",
				"conclaveId":     "ccl-cross-instance",
				"role":           "claude",
				"prompt":         "write",
			}),
		}
	}

	// Complete round 1 so the council writer worktree already exists, then race two adapter instances on round 2.
	h0, err := a1.Start(ctx, makeSpec("writer-r1"))
	if err != nil {
		t.Fatalf("Start round 1: %v", err)
	}
	if _, err := a1.Observe(ctx, h0); err != nil {
		t.Fatalf("Observe round 1: %v", err)
	}
	t.Cleanup(func() { _ = a1.CleanupCouncil(context.Background(), "ccl-cross-instance") })

	for round := range 5 {
		startCh := make(chan struct{})
		type result struct {
			h   scruntime.Handle
			err error
		}
		resCh := make(chan result, 2)
		id1 := fmt.Sprintf("writer-r%d-a1", round+2)
		id2 := fmt.Sprintf("writer-r%d-a2", round+2)
		go func() {
			<-startCh
			h, err := a1.Start(ctx, makeSpec(id1))
			resCh <- result{h, err}
		}()
		go func() {
			<-startCh
			h, err := a2.Start(ctx, makeSpec(id2))
			resCh <- result{h, err}
		}()
		close(startCh)
		r1, r2 := <-resCh, <-resCh
		succeeded := 0
		for _, r := range []result{r1, r2} {
			if r.err == nil {
				succeeded++
				if err := a1.Stop(ctx, r.h); err != nil {
					t.Fatalf("Stop winner: %v", err)
				}
			}
		}
		if succeeded != 1 {
			t.Fatalf("round %d concurrent cross-instance writer launches succeeded = %d (err1=%v, err2=%v), want exactly 1", round+2, succeeded, r1.err, r2.err)
		}
	}
}

func TestCouncilOccupiedDestinationRefusedUntouched(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		conclaveID string
		input      map[string]any
		subdir     string
	}{
		{
			name:       "occupied ref council dir",
			conclaveID: "ccl-occ-ref",
			input:      map[string]any{"checkoutMode": "ref", "checkoutRef": "HEAD", "conclaveId": "ccl-occ-ref", "prompt": "check"},
			subdir:     "ref",
		},
		{
			name:       "occupied writer council dir",
			conclaveID: "ccl-occ-writer",
			input:      map[string]any{"checkoutMode": "writer", "checkoutWriter": "claude", "conclaveId": "ccl-occ-writer", "role": "claude", "prompt": "check"},
			subdir:     "writer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, spec := codingFixture(t)
			a.DryRun = true
			if err := os.MkdirAll(a.WorktreeRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			resolvedRoot, err := resolvedPath(a.WorktreeRoot)
			if err != nil {
				t.Fatal(err)
			}
			key := councilKey(tc.conclaveID)
			occupied := filepath.Join(councilDirPath(resolvedRoot, key), tc.subdir)
			if err := os.MkdirAll(occupied, 0o700); err != nil {
				t.Fatal(err)
			}
			draftPath := filepath.Join(occupied, "draft.txt")
			const draftContent = "precious-preexisting-draft\n"
			if err := os.WriteFile(draftPath, []byte(draftContent), 0o600); err != nil {
				t.Fatal(err)
			}
			spec.InputJSON = mustMarshalInput(t, tc.input)
			if _, err := a.Start(ctx, spec); err == nil {
				t.Fatal("accepted launch into occupied council directory")
			}
			got, err := os.ReadFile(draftPath)
			if err != nil || string(got) != draftContent {
				t.Fatalf("pre-existing draft altered or deleted: got=%q, err=%v", got, err)
			}
		})
	}
}

func TestRefModePinSurvivesFirstLaunchRollbackAfterSecondFinishes(t *testing.T) {
	ctx := context.Background()
	a, spec := codingFixture(t)
	a.DryRun = true
	repo := spec.Workload.Spec.Coding.RepoPath
	testGit(t, repo, "checkout", "-b", "feature")
	testWrite(t, repo, "feature.txt", "v1\n", 0o600)
	testGit(t, repo, "add", "feature.txt")
	testGit(t, repo, "commit", "-m", "v1")
	commitV1 := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	testGit(t, repo, "checkout", "-")

	rawInput := mustMarshalInput(t, map[string]any{
		"checkoutMode": "ref",
		"checkoutRef":  "feature",
		"conclaveId":   "ccl-pin-rollback",
		"role":         "claude",
		"prompt":       "review",
	})
	input, err := parseLaunchInput(rawInput)
	if err != nil {
		t.Fatal(err)
	}
	// Participant 1 prepares council ref checkout (pinning commitV1), before failing/rolling back later.
	_, _, _, rb1, _, err := a.prepareRefCheckout(ctx, repo, "pin-p1", input)
	if err != nil {
		t.Fatalf("prepareRefCheckout p1: %v", err)
	}

	// Move branch "feature" forward to v2.
	testGit(t, repo, "checkout", "feature")
	testWrite(t, repo, "feature.txt", "v2\n", 0o600)
	testGit(t, repo, "add", "feature.txt")
	testGit(t, repo, "commit", "-m", "v2")
	testGit(t, repo, "checkout", "-")

	// Participant 2 starts and finishes on the pinned commitV1.
	spec2 := spec
	spec2.TaskID = "pin-p2"
	spec2.InputJSON = rawInput
	h2, err := a.Start(ctx, spec2)
	if err != nil {
		t.Fatalf("Start p2: %v", err)
	}
	if obs, err := a.Observe(ctx, h2); err != nil || obs.State != scruntime.StateCompleted {
		t.Fatalf("Observe p2 = %+v, err = %v", obs, err)
	}

	// Now participant 1 rolls back! The pin MUST remain until CleanupCouncil.
	if err := rb1(ctx); err != nil {
		t.Fatalf("rollback p1: %v", err)
	}

	// Participant 3 starts and must still see commitV1, not v2.
	spec3 := spec
	spec3.TaskID = "pin-p3"
	spec3.InputJSON = rawInput
	h3, err := a.Start(ctx, spec3)
	if err != nil {
		t.Fatalf("Start p3: %v", err)
	}
	t.Cleanup(func() {
		_ = a.Stop(context.Background(), h3)
		_ = a.CleanupCouncil(context.Background(), "ccl-pin-rollback")
	})
	gotHead := strings.TrimSpace(testGit(t, h3.WorkDir, "rev-parse", "HEAD"))
	if gotHead != commitV1 {
		t.Fatalf("p3 saw commit %s after p1 rollback, want pinned %s", gotHead, commitV1)
	}
}

func TestLiveModeNarrowsGitExposure(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("native Seatbelt live mode test requires macOS (running on %s)", runtime.GOOS)
	}
	ctx := context.Background()
	repo := initTempGitRepo(t)
	testWrite(t, repo, "uncommitted.txt", "owner-draft\n", 0o600)
	testWrite(t, repo, "staged.txt", "owner-staged\n", 0o600)
	testGit(t, repo, "add", "staged.txt")

	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	siblingAdapter := NewAdapter(worktreeRoot)
	siblingAdapter.DryRun = true
	siblingHandle := startTestTask(t, siblingAdapter, scruntime.LaunchSpec{
		TaskID: "sibling-live-exposure",
		Workload: domain.Workload{
			Metadata: domain.ObjectMeta{Name: "sibling", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{
				Class: domain.ClassCodingAgent,
				Coding: &domain.CodingConfig{
					RepoPath: repo,
					Harness:  "claude",
					Sandbox:  "seatbelt",
				},
			},
		},
	})
	t.Cleanup(func() { _ = siblingAdapter.Stop(context.Background(), siblingHandle) })

	// Sibling worktree stages a secret file (creating a loose object in primary .git/objects
	// and recording it in primary .git/worktrees/<sibling>/index) and commits another.
	testWrite(t, siblingHandle.WorkDir, "sibling-committed.txt", "sibling-committed-secret\n", 0o600)
	testGit(t, siblingHandle.WorkDir, "add", "sibling-committed.txt")
	testGit(t, siblingHandle.WorkDir, "commit", "-m", "sibling commit")
	testWrite(t, siblingHandle.WorkDir, "sibling-staged.txt", "sibling-staged-secret\n", 0o600)
	testGit(t, siblingHandle.WorkDir, "add", "sibling-staged.txt")

	siblingStagedOID := strings.TrimSpace(testGit(t, siblingHandle.WorkDir, "hash-object", filepath.Join(siblingHandle.WorkDir, "sibling-staged.txt")))
	resolvedRepo, err := resolvedPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	siblingState, err := siblingAdapter.worktreeState(siblingHandle)
	if err != nil {
		t.Fatal(err)
	}
	siblingIndexPath := filepath.Join(siblingState.GitDir, "index")
	siblingLooseObjPath := filepath.Join(resolvedRepo, ".git", "objects", siblingStagedOID[:2], siblingStagedOID[2:])
	siblingRefPath := filepath.Join(resolvedRepo, ".git", "refs", "heads", siblingHandle.Branch)
	logsHeadPath := filepath.Join(resolvedRepo, ".git", "logs", "HEAD")

	harnessDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
set -u

# Selected checkout files and Git queries must still work.
if [ "$(/bin/cat uncommitted.txt)" != "owner-draft" ]; then
    printf 'MISSING_DRAFT\n' >&2
    exit 10
fi
if [ "$(/bin/cat staged.txt)" != "owner-staged" ]; then
    printf 'MISSING_STAGED\n' >&2
    exit 11
fi
status=$(git status --short) || exit 12
case "$status" in
    *"staged.txt"*"uncommitted.txt"*|*"uncommitted.txt"*"staged.txt"*) ;;
    *)
        printf 'BAD_STATUS:%%s\n' "$status" >&2
        exit 13
        ;;
esac
git diff --cached -- staged.txt | /usr/bin/grep -q "owner-staged" || exit 14
git rev-parse HEAD >/dev/null || exit 15

check_denied() {
    label="$1"
    shift
    err=$("$@" 2>&1 >/dev/null)
    status=$?
    if [ "$status" -eq 0 ]; then
        printf 'LEAK:%%s\n' "$label" >&2
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

check_denied "sibling-index" /bin/cat %q
check_denied "sibling-loose-object" /bin/cat %q
check_denied "sibling-branch-ref" /bin/cat %q
check_denied "primary-logs-head" /bin/cat %q

if git cat-file -p %q >/dev/null 2>&1; then
    printf 'LEAK:git-cat-file-sibling-staged\n' >&2
    exit 22
fi
if git show %q:sibling-committed.txt >/dev/null 2>&1; then
    printf 'LEAK:git-show-sibling-branch\n' >&2
    exit 23
fi

printf 'live-git-narrowed-ok\n'
`, siblingIndexPath, siblingLooseObjPath, siblingRefPath, logsHeadPath, siblingStagedOID, siblingHandle.Branch)
	testWrite(t, harnessDir, "claude", script, 0o700)

	adapter := NewAdapter(worktreeRoot)
	adapter.HarnessPaths = map[string]string{"claude": filepath.Join(harnessDir, "claude")}
	h, err := adapter.Start(ctx, scruntime.LaunchSpec{
		TaskID: "live-narrow-git",
		Workload: domain.Workload{
			Metadata: domain.ObjectMeta{Name: "council-claude", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{
				Class: domain.ClassCodingAgent,
				Coding: &domain.CodingConfig{
					RepoPath: repo,
					Harness:  "claude",
					Sandbox:  "seatbelt",
				},
			},
		},
		InputJSON: mustMarshalInput(t, map[string]any{
			"checkoutMode": "live",
			"conclaveId":   "ccl-live-narrow",
			"role":         "claude",
			"prompt":       "review live checkout",
		}),
	})
	if err != nil {
		t.Fatalf("Start live: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Stop(context.Background(), h) })
	waitUntil(t, func() bool { return outputClosed(adapter.processForHandle(h).done) })
	logData, _ := os.ReadFile(h.LogPath)
	obs, err := adapter.Observe(ctx, h)
	if err != nil || obs.State != scruntime.StateCompleted {
		t.Fatalf("Observe live narrow git = %+v, err = %v\nlog:\n%s", obs, err, string(logData))
	}
}

func TestCouncilSetupBoundedAndDoesNotHoldCouncilLock(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	repo := initTempGitRepo(t)
	markerDir := t.TempDir()
	enteredMarker := filepath.Join(markerDir, "entered")
	releaseMarker := filepath.Join(markerDir, "release")

	// Wrap git so only "worktree add" pauses until releaseMarker appears or it is killed.
	binDir := t.TempDir()
	wrapper := fmt.Sprintf(`#!/bin/sh
case " $* " in
    *" worktree add "*)
        printf 'entered\n' > %q
        while [ ! -f %q ]; do
            sleep 0.05
        done
        ;;
esac
exec %q "$@"
`, enteredMarker, releaseMarker, realGit)
	testWrite(t, binDir, "git", wrapper, 0o700)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { _ = os.WriteFile(releaseMarker, []byte("go"), 0o600) })

	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	adapter := NewAdapter(worktreeRoot)
	adapter.DryRun = true
	spec := scruntime.LaunchSpec{
		TaskID: "slow-ref-1",
		Workload: domain.Workload{
			Metadata: domain.ObjectMeta{Name: "council-claude", Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{
				Class: domain.ClassCodingAgent,
				Coding: &domain.CodingConfig{
					RepoPath: repo,
					Harness:  "claude",
					Sandbox:  "seatbelt",
				},
			},
		},
		InputJSON: mustMarshalInput(t, map[string]any{
			"checkoutMode": "ref",
			"checkoutRef":  "HEAD",
			"conclaveId":   "ccl-slow-setup",
			"role":         "claude",
			"prompt":       "review",
		}),
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	done1 := make(chan error, 1)
	go func() {
		_, err := adapter.Start(ctx1, spec)
		done1 <- err
	}()

	// Wait until git worktree add is blocked.
	waitUntil(t, func() bool {
		_, err := os.Stat(enteredMarker)
		return err == nil
	})

	// While git worktree add is blocked, the council lock must NOT be held.
	lockCtx, lockCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer lockCancel()
	rel, lockErr := adapter.lockScope(lockCtx, "council:"+councilKey("ccl-slow-setup"), false)
	if lockErr != nil {
		t.Fatalf("council lock was held across delayed git checkout: %v", lockErr)
	}
	rel()

	// Cancelling ctx1 must abort setup promptly (not wait for releaseMarker).
	cancel1()
	select {
	case err := <-done1:
		if err == nil {
			t.Fatal("cancelled slow setup unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delayed git checkout did not stop promptly on context cancellation")
	}

	// Setup must also be bounded by adapter setupTimeout even with context.Background().
	adapter.setupTimeout = 300 * time.Millisecond
	spec.TaskID = "slow-ref-timeout"
	spec.InputJSON = mustMarshalInput(t, map[string]any{
		"checkoutMode": "ref",
		"checkoutRef":  "HEAD",
		"conclaveId":   "ccl-slow-timeout",
		"role":         "claude",
		"prompt":       "review",
	})
	start := time.Now()
	if _, err := adapter.Start(context.Background(), spec); err == nil {
		t.Fatal("expected setupTimeout error on slow git checkout")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("setupTimeout did not bound slow git checkout: took %s", elapsed)
	}
}

func TestCouncilInterruptedReservationReconciled(t *testing.T) {
	const crashEnv = "SABLECOURT_TEST_CRASH_RESERVATION"
	if cfgPath := os.Getenv(crashEnv); cfgPath != "" {
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Root string               `json:"root"`
			Spec scruntime.LaunchSpec `json:"spec"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		a := NewAdapter(payload.Root)
		a.DryRun = true
		a.beforeTaskState = func(_ string) {
			// Simulate abrupt process crash between council reservation and writing task state.
			os.Exit(2)
		}
		if _, err := a.Start(context.Background(), payload.Spec); err != nil {
			fmt.Fprintf(os.Stderr, "crash helper Start error: %v\n", err)
		}
		os.Exit(3)
	}

	for _, tc := range []struct {
		name       string
		conclaveID string
		input      map[string]any
	}{
		{
			name:       "writer reservation interrupted",
			conclaveID: "ccl-crash-writer",
			input: map[string]any{
				"checkoutMode":   "writer",
				"checkoutWriter": "claude",
				"conclaveId":     "ccl-crash-writer",
				"role":           "claude",
				"prompt":         "write",
			},
		},
		{
			name:       "ref reservation interrupted",
			conclaveID: "ccl-crash-ref",
			input: map[string]any{
				"checkoutMode": "ref",
				"checkoutRef":  "HEAD",
				"conclaveId":   "ccl-crash-ref",
				"role":         "claude",
				"prompt":       "review",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo := initTempGitRepo(t)
			worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
			spec := scruntime.LaunchSpec{
				TaskID: "crash-task-1",
				Workload: domain.Workload{
					Metadata: domain.ObjectMeta{Name: "council-claude", Zone: domain.ZoneRuntime},
					Spec: domain.WorkloadSpec{
						Class: domain.ClassCodingAgent,
						Coding: &domain.CodingConfig{
							RepoPath: repo,
							Harness:  "claude",
							Sandbox:  "seatbelt",
						},
					},
				},
				InputJSON: mustMarshalInput(t, tc.input),
			}
			cfgPath := filepath.Join(t.TempDir(), "crash.json")
			runCrash := func(taskID, conclaveID string) {
				crashSpec := spec
				crashSpec.TaskID = taskID
				inp := make(map[string]any, len(tc.input))
				for k, v := range tc.input {
					inp[k] = v
				}
				inp["conclaveId"] = conclaveID
				crashSpec.InputJSON = mustMarshalInput(t, inp)
				raw, err := json.Marshal(struct {
					Root string               `json:"root"`
					Spec scruntime.LaunchSpec `json:"spec"`
				}{Root: worktreeRoot, Spec: crashSpec})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestCouncilInterruptedReservationReconciled$")
				cmd.Env = append(os.Environ(), crashEnv+"="+cfgPath, "GORACE=atexit_sleep_ms=0")
				out, err := cmd.CombinedOutput()
				var exitErr *exec.ExitError
				if err == nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
					t.Fatalf("crash subprocess err = %v, want exit status 2\noutput:\n%s", err, string(out))
				}
			}
			// First crash: verify retry from a fresh adapter reconciles the journal and succeeds.
			runCrash("crash-task-1", tc.conclaveID)

			fresh := NewAdapter(worktreeRoot)
			fresh.DryRun = true
			h, err := fresh.Start(ctx, spec)
			if err != nil {
				t.Fatalf("retry after interrupted reservation failed: %v", err)
			}
			if _, err := fresh.Observe(ctx, h); err != nil {
				t.Fatalf("Observe after retry: %v", err)
			}
			if err := fresh.CleanupCouncil(ctx, tc.conclaveID); err != nil {
				t.Fatalf("CleanupCouncil after retry: %v", err)
			}

			// Second crash on a separate council: verify CleanupCouncil alone reconciles the journal.
			cleanupID := tc.conclaveID + "-cleanup"
			runCrash("crash-task-2", cleanupID)
			fresh2 := NewAdapter(worktreeRoot)
			if err := fresh2.CleanupCouncil(ctx, cleanupID); err != nil {
				t.Fatalf("CleanupCouncil after interrupted reservation failed: %v", err)
			}
		})
	}
}

func TestLiveModeWithoutGitInfoExclude(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	testGit(t, repo, "init", "--template=", "-q")
	testWrite(t, repo, "README.md", "base\n", 0o600)
	testGit(t, repo, "add", "README.md")
	testGit(t, repo, "commit", "-qm", "init")
	if _, err := os.Lstat(filepath.Join(repo, ".git", "info", "exclude")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no .git/info/exclude with --template=, got err=%v", err)
	}

	a := NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
	a.DryRun = true
	spec := scruntime.LaunchSpec{
		TaskID: "live-no-exclude",
		Workload: domain.Workload{
			Metadata: domain.ObjectMeta{Name: "claude", Zone: domain.ZoneSandbox},
			Spec: domain.WorkloadSpec{
				Class:  domain.ClassCodingAgent,
				Coding: &domain.CodingConfig{RepoPath: repo, Harness: "claude", Sandbox: "seatbelt"},
			},
		},
		InputJSON: mustMarshalInput(t, map[string]any{
			"checkoutMode": "live",
			"conclaveId":   "ccl-live-no-exclude",
			"role":         "claude",
			"prompt":       "review",
		}),
	}
	h, err := a.Start(ctx, spec)
	if err != nil {
		t.Fatalf("Start live mode without .git/info/exclude failed: %v", err)
	}
	if err := a.Stop(ctx, h); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// A symlinked .git/info directory must still be rejected.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, ".git", "info")); err != nil {
		t.Fatal(err)
	}
	spec.TaskID = "live-symlink-info"
	if _, err := a.Start(ctx, spec); err == nil {
		t.Fatal("expected symlinked .git/info to be rejected")
	}
}
