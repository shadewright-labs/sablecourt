package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// Adapter runs local coding CLIs (claude or codex) inside dedicated Git worktrees
// with stripped environment variables, detached process groups, and OS sandboxing.
type Adapter struct {
	WorktreeRoot string
	DryRun       bool
}

var _ scruntime.Adapter = (*Adapter)(nil)

func NewAdapter(worktreeRoot string) *Adapter {
	return &Adapter{WorktreeRoot: worktreeRoot}
}

func (a *Adapter) Start(ctx context.Context, spec scruntime.LaunchSpec) (scruntime.Handle, error) {
	if spec.Workload.Spec.Coding == nil {
		return scruntime.Handle{}, errors.New("coding config is required")
	}
	cfg := spec.Workload.Spec.Coding
	if cfg.Harness != "claude" && cfg.Harness != "codex" {
		return scruntime.Handle{}, fmt.Errorf("unsupported coding harness %q", cfg.Harness)
	}

	branch := "sablecourt/" + sanitizeSegment(spec.TaskID)
	worktreeDir := filepath.Join(a.WorktreeRoot, sanitizeSegment(spec.TaskID))
	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0o700); err != nil {
		return scruntime.Handle{}, fmt.Errorf("mkdir worktree root: %w", err)
	}

	if err := createWorktree(ctx, cfg.RepoPath, worktreeDir, branch); err != nil {
		return scruntime.Handle{}, err
	}

	handle := scruntime.Handle{
		AdapterName: "coding:" + cfg.Harness,
		ExternalID:  spec.TaskID,
		WorkDir:     worktreeDir,
		Branch:      branch,
	}
	if a.DryRun {
		return handle, nil
	}

	bin, argv, err := buildCommand(cfg.Harness, cfg.Sandbox, worktreeDir, spec.InputJSON)
	if err != nil {
		return scruntime.Handle{}, err
	}

	cmd := exec.Command(bin, argv...)
	cmd.Dir = worktreeDir
	cmd.Env = BuildStrippedEnv(spec)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return scruntime.Handle{}, fmt.Errorf("start coding harness %q: %w", cfg.Harness, err)
	}
	handle.PID = cmd.Process.Pid
	_ = cmd.Process.Release()
	return handle, nil
}

func (a *Adapter) Observe(_ context.Context, handle scruntime.Handle) (scruntime.Observation, error) {
	now := time.Now().UTC()
	if handle.PID <= 0 {
		return scruntime.Observation{State: scruntime.StateCompleted, ObservedAt: now}, nil
	}
	err := syscall.Kill(handle.PID, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return scruntime.Observation{State: scruntime.StateRunning, ObservedAt: now}, nil
	}
	return scruntime.Observation{State: scruntime.StateCompleted, ObservedAt: now}, nil
}

func (a *Adapter) Suspend(_ context.Context, _ scruntime.Handle) (scruntime.CheckpointRef, error) {
	return scruntime.CheckpointRef{}, errors.New("coding CLI adapter does not support live suspend")
}

func (a *Adapter) Resume(_ context.Context, _ scruntime.Handle, _ scruntime.CheckpointRef) error {
	return errors.New("coding CLI adapter does not support live resume")
}

func (a *Adapter) Stop(_ context.Context, handle scruntime.Handle) error {
	if handle.PID > 0 {
		_ = syscall.Kill(-handle.PID, syscall.SIGTERM)
	}
	return nil
}

func (a *Adapter) CollectArtifacts(ctx context.Context, handle scruntime.Handle) ([]scruntime.Artifact, error) {
	if handle.WorkDir == "" {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", handle.WorkDir, "status", "--short")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return []scruntime.Artifact{
		{
			Name:        "git-status",
			ContentType: "text/plain",
			Text:        strings.TrimSpace(string(out)),
		},
	}, nil
}

// BuildStrippedEnv constructs a minimal allowlisted environment with zero raw provider keys
// and sets read-only starter metadata so Nightloom can identify Sablecourt-managed sessions.
func BuildStrippedEnv(spec scruntime.LaunchSpec) []string {
	env := []string{
		"PATH=/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin",
		"LANG=en_US.UTF-8",
		"SABLECOURT_MANAGED=1",
		"SABLECOURT_TASK_ID=" + spec.TaskID,
		"SABLECOURT_WORKLOAD=" + spec.Workload.Metadata.Name,
	}
	if spec.ModelGateURL != "" && spec.ModelGateToken != "" {
		env = append(env,
			"SABLECOURT_MODEL_GATE_URL="+spec.ModelGateURL,
			"SABLECOURT_TASK_TOKEN="+spec.ModelGateToken,
		)
	}
	if spec.DataGateURL != "" && spec.TaskToken != "" {
		env = append(env, "SABLECOURT_DATA_GATE_URL="+spec.DataGateURL)
	}
	return env
}

// SeatbeltProfile returns a macOS sandbox-exec profile restricting writes to workDir and /tmp.
func SeatbeltProfile(workDir string) string {
	clean := filepath.Clean(workDir)
	return fmt.Sprintf(`(version 1)
(allow default)
(deny file-write*)
(allow file-write* (subpath %q) (subpath "/tmp") (subpath "/private/tmp"))
`, clean)
}

func buildCommand(harness, sandbox, workDir, prompt string) (string, []string, error) {
	var baseBin string
	var baseArgs []string
	switch harness {
	case "claude":
		baseBin = "claude"
		baseArgs = []string{"-p", prompt}
	case "codex":
		baseBin = "codex"
		baseArgs = []string{"exec", prompt}
	default:
		return "", nil, fmt.Errorf("unsupported harness %q", harness)
	}

	switch sandbox {
	case "seatbelt":
		if goruntime.GOOS == "darwin" {
			args := append([]string{"-p", SeatbeltProfile(workDir), baseBin}, baseArgs...)
			return "/usr/bin/sandbox-exec", args, nil
		}
	case "bubblewrap":
		if goruntime.GOOS == "linux" {
			args := append([]string{"--ro-bind", "/", "/", "--bind", workDir, workDir, "--tmpfs", "/tmp", "--", baseBin}, baseArgs...)
			return "bwrap", args, nil
		}
	}
	return baseBin, baseArgs, nil
}

func createWorktree(ctx context.Context, repoPath, worktreeDir, branch string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "add", "-b", branch, worktreeDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func sanitizeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "task"
	}
	return b.String()
}
