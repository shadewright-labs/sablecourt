package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// newHostModeAdapter returns an adapter with the host launcher opted in. Tests
// of the Seatbelt host path use it; production sets HostModeUnsafe only from
// the daemon's --unsafe-host-harnesses flag.
func newHostModeAdapter(root string) *Adapter {
	a := NewAdapter(root)
	a.HostModeUnsafe = true
	return a
}

// TestHostLaunchRefusedByDefault checks that with default configuration no
// coding task or council seat starts on the host, and that host mode never
// replaces a configured container runner.
func TestHostLaunchRefusedByDefault(t *testing.T) {
	repo := initTempGitRepo(t)
	for _, tc := range []struct {
		name      string
		hostOptIn bool
		runner    bool
		input     string
		want      error
	}{
		{name: "standalone task, default configuration", input: `{"prompt":"edit"}`, want: ErrHostModeOff},
		{name: "council seat, default configuration", input: `{"conclaveId":"ccl_1","role":"claude","checkoutMode":"live","context_packet":"review"}`, want: ErrHostModeOff},
		{name: "standalone task, opt-in but containers configured", hostOptIn: true, runner: true, input: `{"prompt":"edit"}`, want: errHostWithContainers},
		{name: "council seat, opt-in but containers configured", hostOptIn: true, runner: true, input: `{"conclaveId":"ccl_1","role":"claude","checkoutMode":"live","context_packet":"review"}`, want: errHostWithContainers},
		{name: "containers configured without opt-in", runner: true, input: `{"prompt":"edit"}`, want: errHostWithContainers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "worktrees")
			a := NewAdapter(root)
			a.HostModeUnsafe = tc.hostOptIn
			if tc.runner {
				a.CouncilRunner = newFakeCouncilRunner()
			}
			spec := scruntime.LaunchSpec{
				TaskID:    "task-host-refusal",
				TaskToken: "sc_task_" + "hostrefusal",
				InputJSON: tc.input,
				Workload: domain.Workload{Metadata: domain.ObjectMeta{Name: "claude"}, Spec: domain.WorkloadSpec{
					Class: domain.ClassCodingAgent, Coding: &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "seatbelt"},
				}},
			}
			h, err := a.Start(t.Context(), spec)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Start error = %v, want %v", err, tc.want)
			}
			if h.PID != 0 || h.WorkDir != "" {
				t.Fatalf("refused launch returned a running handle: %+v", h)
			}
			if entries, _ := filepath.Glob(filepath.Join(root, "*")); len(entries) != 0 {
				t.Fatalf("refused launch left worktree state: %v", entries)
			}
		})
	}
	if !strings.Contains(ErrHostModeOff.Error(), "harness containers only") {
		t.Fatal("refusal must name the container-only rule")
	}
}

// TestStandaloneTaskRunsInHarnessContainer checks that a standalone coding
// task reaches the same container runner as a council seat, with the task
// checkout kind, the per-launch proxy endpoint and its own lease only.
func TestStandaloneTaskRunsInHarnessContainer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readOnly bool
		want     scruntime.CouncilCheckoutKind
	}{
		{name: "writable task", want: scruntime.CouncilTask},
		{name: "pinned verifier", readOnly: true, want: scruntime.CouncilReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initTempGitRepo(t)
			base := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
			a := NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
			runner := newFakeCouncilRunner()
			proxy := newFakeProxyDocker()
			a.CouncilRunner = runner
			a.CouncilProxy = proxy
			a.CouncilProxyImage = "sha256:" + strings.Repeat("a", 64)
			a.DaemonListen = "127.0.0.1:7432"
			token := "sc_task_" + "standalonelease"
			spec := scruntime.LaunchSpec{
				TaskID: "task-standalone", TaskToken: token, CouncilContainer: true,
				InputJSON: `{"prompt":"implement the change"}`,
				Workload: domain.Workload{Metadata: domain.ObjectMeta{Name: "coder"}, Spec: domain.WorkloadSpec{
					Class: domain.ClassCodingAgent, Coding: &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "container"},
				}},
			}
			var h scruntime.Handle
			var err error
			if tc.readOnly {
				h, err = a.StartAtCommit(t.Context(), spec, base, true)
			} else {
				h, err = a.Start(t.Context(), spec)
			}
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = a.Stop(t.Context(), h) })
			if !isContainerHandle(h) || h.PID != 0 || h.ProxyName == "" || h.NetworkName == "" {
				t.Fatalf("standalone handle = %+v, want a container handle with proxy and network", h)
			}
			runner.mu.Lock()
			if len(runner.launches) != 1 {
				runner.mu.Unlock()
				t.Fatalf("runner launches = %d, want 1", len(runner.launches))
			}
			l := runner.launches[0]
			runner.mu.Unlock()
			if l.Checkout.Kind != tc.want || l.Checkout.Path != h.WorkDir || l.TaskToken != token || l.Prompt != "implement the change" ||
				l.Network != h.NetworkName || l.ProxyURL == "" || l.DataGateURL != l.ProxyURL+"/gate/mcp" || l.Limits.WallTime <= 0 || l.Limits.WallTime > defaultWallTime+time.Second {
				t.Fatalf("standalone launch = %+v", l)
			}
			// Once the task finishes and its cleanup succeeds, the container
			// entry and its proxy session leave the adapter.
			deadline := time.Now().Add(5 * time.Second)
			for {
				obs, err := a.Observe(t.Context(), h)
				if err != nil {
					t.Fatalf("Observe: %v", err)
				}
				if obs.State == scruntime.StateCompleted || obs.State == scruntime.StateFailed {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("task state = %s, want terminal", obs.State)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := a.CollectArtifacts(t.Context(), h); err != nil {
				t.Fatalf("CollectArtifacts: %v", err)
			}
			if a.containerProc(h.WorkDir) != nil {
				t.Fatal("finished standalone task left its container entry and proxy session in the adapter")
			}
		})
	}
}

// TestRecoveryOnlyAdapterObservesAndStopsHostWorker checks the upgrade path:
// a host worker started before the upgrade is still observed and stopped by
// the daemon's default adapter (no runner, no host opt-in), which refuses to
// launch anything new.
func TestRecoveryOnlyAdapterObservesAndStopsHostWorker(t *testing.T) {
	old, spec := codingFixture(t)
	shellHarness(t, "sleep 30\n")
	h := startTestTask(t, old, spec)

	upgraded := NewAdapter(old.WorktreeRoot)
	obs, err := upgraded.Observe(t.Context(), h)
	if err != nil || obs.State != scruntime.StateRunning {
		t.Fatalf("Observe after upgrade = %+v, %v, want running", obs, err)
	}
	if err := upgraded.Stop(t.Context(), h); err != nil {
		t.Fatalf("Stop after upgrade: %v", err)
	}
	waitUntil(t, func() bool { exited, err := upgraded.HandleExited(t.Context(), h); return err == nil && exited })

	next := spec
	next.TaskID = "task-after-upgrade"
	if _, err := upgraded.Start(t.Context(), next); !errors.Is(err, ErrHostModeOff) {
		t.Fatalf("Start after upgrade = %v, want ErrHostModeOff", err)
	}
}

// TestHarnessContainerGetsOwnGitMetadata checks the Git layout a harness
// container receives for a linked-worktree checkout. The task worktree's .git
// file names its registration in the shared repository, which the container
// never mounts, so the launch carries a self-contained metadata directory:
// Git resolves HEAD, status and a commit through it alone, it never points
// back at the shared registration or object store, and it is gone after the
// task finishes while the host still saves the edit from the checkout files.
func TestHarnessContainerGetsOwnGitMetadata(t *testing.T) {
	repo := initTempGitRepo(t)
	base := strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD"))
	common, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	a := NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
	runner := newFakeCouncilRunner()
	a.CouncilRunner = runner
	a.CouncilProxy = newFakeProxyDocker()
	a.CouncilProxyImage = "sha256:" + strings.Repeat("a", 64)
	a.DaemonListen = "127.0.0.1:7432"
	var inContainer error
	runner.onRun = func(ctx context.Context, s scruntime.CouncilLaunch) (string, string, int, error) {
		inContainer = func() error {
			gitDir := s.Checkout.GitDir
			if gitDir == "" || strings.HasPrefix(gitDir, common+string(filepath.Separator)) || strings.HasPrefix(gitDir, s.Checkout.Path+string(filepath.Separator)) {
				return fmt.Errorf("launch GitDir = %q, want a directory outside the shared repository and the checkout", gitDir)
			}
			for _, name := range []string{"commondir", "worktrees", filepath.Join("objects", "info", "alternates")} {
				if _, err := os.Lstat(filepath.Join(gitDir, name)); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("GitDir has %s, so it reaches shared metadata", name)
				}
			}
			// The container's view: GIT_DIR and GIT_WORK_TREE, as the spec sets them.
			run := func(args ...string) (string, error) {
				cmd := exec.CommandContext(ctx, "git", args...)
				cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir, "GIT_WORK_TREE="+s.Checkout.Path, "GIT_CONFIG_GLOBAL="+os.DevNull)
				cmd.Dir = s.Checkout.Path
				out, err := cmd.CombinedOutput()
				return strings.TrimSpace(string(out)), err
			}
			if out, err := run("rev-parse", "--path-format=absolute", "--git-common-dir"); err != nil || out != gitDir {
				return fmt.Errorf("common dir = %q, %v, want the launch's own directory", out, err)
			}
			if out, err := run("symbolic-ref", "HEAD"); err != nil || out != "refs/heads/sablecourt/task-gitdir" {
				return fmt.Errorf("HEAD = %q, %v", out, err)
			}
			if out, err := run("rev-parse", "HEAD"); err != nil || out != base {
				return fmt.Errorf("HEAD commit = %q, %v, want %s", out, err, base)
			}
			if out, err := run("status", "--porcelain"); err != nil || out != "" {
				return fmt.Errorf("status = %q, %v, want a clean checkout", out, err)
			}
			if err := os.WriteFile(filepath.Join(s.Checkout.Path, "edit.txt"), []byte("container edit\n"), 0o644); err != nil {
				return err
			}
			if out, err := run("add", "edit.txt"); err != nil {
				return fmt.Errorf("add: %v (%s)", err, out)
			}
			if out, err := run("-c", "user.name=t", "-c", "user.email=t", "commit", "-q", "-m", "in container"); err != nil {
				return fmt.Errorf("commit: %v (%s)", err, out)
			}
			return nil
		}()
		return "done\n", "", 0, nil
	}
	spec := scruntime.LaunchSpec{
		TaskID: "task-gitdir", TaskToken: "sc_task_" + "gitdirlease", CouncilContainer: true,
		InputJSON: `{"prompt":"implement the change"}`,
		Workload: domain.Workload{Metadata: domain.ObjectMeta{Name: "coder"}, Spec: domain.WorkloadSpec{
			Class: domain.ClassCodingAgent, Coding: &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "container"},
		}},
	}
	h, err := a.Start(t.Context(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(t.Context(), h) })
	waitUntil(t, func() bool {
		obs, err := a.Observe(t.Context(), h)
		return err == nil && (obs.State == scruntime.StateCompleted || obs.State == scruntime.StateFailed)
	})
	if inContainer != nil {
		t.Fatal(inContainer)
	}
	arts, err := a.CollectArtifacts(t.Context(), h)
	if err != nil {
		t.Fatalf("CollectArtifacts: %v", err)
	}
	runner.mu.Lock()
	gitDir := runner.launches[0].Checkout.GitDir
	runner.mu.Unlock()
	if _, err := os.Lstat(gitDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("harness Git metadata left after finish: %v", err)
	}
	var commit string
	for _, art := range arts {
		if art.Name == "git-commit" {
			commit = strings.TrimSpace(art.Text)
		}
	}
	if got := strings.TrimSpace(testGit(t, repo, "show", commit+":edit.txt")); got != "container edit" {
		t.Fatalf("host branch edit.txt = %q, want the container's edit", got)
	}
}
