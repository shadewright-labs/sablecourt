package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

const (
	sessionMarkerFile  = ".sablecourt-session.json"
	sessionContextFile = ".sablecourt-context.md"
	stateSuffix        = ".state.json"
)

type worktreeState struct {
	TaskID     string
	Repo       string
	CommonDir  string
	Branch     string
	Base       string
	Tip        string
	FinishedAt time.Time
	Artifacts  []scruntime.Artifact
}

func (a *Adapter) prepareWorktree(ctx context.Context, repo, taskID string) (string, *worktreeState, error) {
	if a.WorktreeRoot == "" || repo == "" || taskID == "" {
		return "", nil, errors.New("worktree root, repository and task ID are required")
	}
	if err := os.MkdirAll(a.WorktreeRoot, 0o700); err != nil {
		return "", nil, fmt.Errorf("mkdir worktree root: %w", err)
	}
	dir, err := resolvedPath(a.WorktreeRoot)
	if err != nil {
		return "", nil, err
	}
	dir = filepath.Join(dir, sanitizeSegment(taskID))
	for _, path := range []string{dir, dir + stateSuffix, dir + ".log"} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				err = os.ErrExist
			}
			return "", nil, fmt.Errorf("worktree path unavailable: %w", err)
		}
	}
	repo, err = resolvedPath(repo)
	if err != nil {
		return "", nil, err
	}
	common, err := git(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", nil, err
	}
	base, err := git(ctx, repo, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", nil, err
	}
	state := &worktreeState{
		TaskID: taskID, Repo: repo, CommonDir: strings.TrimSuffix(common, "\n"),
		Branch: "sablecourt/" + sanitizeSegment(taskID), Base: strings.TrimSpace(base),
	}
	a.pruneFinishedBranches(ctx, filepath.Dir(dir), state)

	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	// Let Git finish registration and checkout so a canceled request can roll them back.
	setupCtx := context.WithoutCancel(ctx)
	if _, err := git(setupCtx, repo, "branch", "--no-track", state.Branch, state.Base); err != nil {
		return "", nil, err
	}
	if _, err := git(setupCtx, repo, "worktree", "add", "--", dir, state.Branch); err != nil {
		return "", nil, errors.Join(err, rollbackWorktree(setupCtx, dir, state))
	}
	if err := writeWorktreeState(dir, state); err != nil {
		return "", nil, errors.Join(err, rollbackWorktree(setupCtx, dir, state))
	}
	return dir, state, nil
}

func resolvedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{
		"-C", dir, "-c", "core.hooksPath=" + os.DevNull,
	}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var exit *exec.ExitError
		message := ""
		if errors.As(err, &exit) {
			message = redact.String(strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w (%s)", args[0], err, message)
	}
	return string(out), nil
}

func gitExitCode(err error, code int) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == code
}

func gitConfig(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := git(ctx, dir, append([]string{"config"}, args...)...)
	if gitExitCode(err, 1) {
		return "", nil
	}
	return strings.TrimSuffix(out, "\n"), err
}

func excludeSessionFiles(ctx context.Context, dir string) error {
	files := []string{sessionMarkerFile, sessionContextFile}
	out, err := git(ctx, dir, append([]string{"ls-files", "--"}, files...)...)
	if err != nil {
		return err
	}
	if out != "" {
		return errors.New("repository tracks a reserved session file")
	}
	oldExclude, err := gitConfig(ctx, dir, "--path", "--get", "core.excludesFile")
	if err != nil {
		return err
	}
	if oldExclude == "" {
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			if home, err := os.UserHomeDir(); err == nil {
				configHome = filepath.Join(home, ".config")
			}
		}
		if configHome != "" {
			oldExclude = filepath.Join(configHome, "git", "ignore")
		}
	}
	var patterns []byte
	if oldExclude != "" {
		if !filepath.IsAbs(oldExclude) {
			oldExclude = filepath.Join(dir, oldExclude)
		}
		patterns, err = os.ReadFile(oldExclude)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read existing excludes: %w", err)
		}
	}
	gitDir, err := git(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	infoDir := filepath.Join(strings.TrimSuffix(gitDir, "\n"), "info")
	if err := os.MkdirAll(infoDir, 0o700); err != nil {
		return err
	}
	exclude := filepath.Join(infoDir, "exclude")
	for _, file := range files {
		patterns = append(patterns, "\n/"+file+"\n"...)
	}
	if err := os.WriteFile(exclude, patterns, 0o600); err != nil {
		return err
	}

	// Linked worktrees share info/exclude unless a worktree config points to a private file.
	enabled, err := gitConfig(ctx, dir, "--local", "--type=bool", "--get", "extensions.worktreeConfig")
	if err != nil {
		return err
	}
	if enabled != "true" {
		bare, err := gitConfig(ctx, dir, "--local", "--type=bool", "--get", "core.bare")
		if err != nil {
			return err
		}
		worktree, err := gitConfig(ctx, dir, "--local", "--get", "core.worktree")
		if err != nil {
			return err
		}
		if bare == "true" || worktree != "" {
			return errors.New("repository must migrate its Git worktree config before starting")
		}
		if _, err := git(ctx, dir, "config", "--local", "extensions.worktreeConfig", "true"); err != nil {
			return err
		}
	}
	if _, err := git(ctx, dir, "config", "--worktree", "core.excludesFile", exclude); err != nil {
		return err
	}
	ignored, err := git(ctx, dir, append([]string{"check-ignore", "--no-index", "--"}, files...)...)
	if err != nil && !gitExitCode(err, 1) {
		return err
	}
	if ignored != strings.Join(files, "\n")+"\n" {
		return errors.New("repository ignore rules override the session excludes")
	}
	return nil
}

func (a *Adapter) worktreeState(handle scruntime.Handle) (*worktreeState, error) {
	root, err := resolvedPath(a.WorktreeRoot)
	if err != nil {
		return nil, err
	}
	segment := sanitizeSegment(handle.ExternalID)
	if handle.ExternalID == "" || handle.WorkDir != filepath.Join(root, segment) || handle.Branch != "sablecourt/"+segment {
		return nil, errors.New("invalid coding worktree handle")
	}
	state, err := readWorktreeState(handle.WorkDir + stateSuffix)
	if err != nil {
		return nil, err
	}
	if state.TaskID != handle.ExternalID || state.Branch != handle.Branch {
		return nil, errors.New("coding worktree state does not match handle")
	}
	return state, nil
}

func readWorktreeState(path string) (*worktreeState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read coding worktree state: %w", err)
	}
	var state worktreeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode coding worktree state: %w", err)
	}
	return &state, nil
}

func writeWorktreeState(dir string, state *worktreeState) error {
	file, err := os.CreateTemp(filepath.Dir(dir), ".worktree-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(state)
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), dir+stateSuffix)
}

func (s *worktreeState) validateCheckout(ctx context.Context, dir string) error {
	common, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if strings.TrimSuffix(common, "\n") != s.CommonDir {
		return errors.New("coding worktree belongs to another repository")
	}
	head, err := git(ctx, dir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != "refs/heads/"+s.Branch {
		return errors.New("coding worktree has another branch checked out")
	}
	return nil
}

func snapshotWorktree(ctx context.Context, dir string) error {
	if err := commitWorktreeIndex(ctx, dir, "chore(coding): preserve staged task edits"); err != nil {
		return err
	}
	if _, err := git(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	return commitWorktreeIndex(ctx, dir, "chore(coding): preserve task edits")
}

func commitWorktreeIndex(ctx context.Context, dir, message string) error {
	_, err := git(ctx, dir, "diff", "--cached", "--quiet", "--no-ext-diff", "--ignore-submodules=none", "--")
	if err == nil {
		return nil
	}
	if !gitExitCode(err, 1) {
		return err
	}
	_, err = git(ctx, dir, "-c", "user.name=Sablecourt", "-c", "user.email=sablecourt",
		"-c", "commit.gpgSign=false", "commit", "-m", message)
	return err
}

func (a *Adapter) finishWorktree(ctx context.Context, dir string, state *worktreeState) error {
	if state.FinishedAt.IsZero() {
		if err := state.validateCheckout(ctx, dir); err != nil {
			return err
		}
		if err := removeSessionFiles(ctx, dir); err != nil {
			return err
		}
		status, err := git(ctx, dir, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
		if err != nil {
			return err
		}
		if status != "" {
			if err := snapshotWorktree(ctx, dir); err != nil {
				return err
			}
		}
		tip, err := git(ctx, dir, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return err
		}
		state.Tip = strings.TrimSpace(tip)
		state.Artifacts, err = worktreeArtifacts(ctx, dir, state, status)
		if err != nil {
			return err
		}
		state.FinishedAt = time.Now().UTC()
		if err := writeWorktreeState(dir, state); err != nil {
			return err
		}
	}
	if err := removeWorktree(ctx, dir, state); err != nil {
		return err
	}
	if err := removeFile(dir + ".log"); err != nil {
		return err
	}
	delete(a.running, dir)
	return nil
}

func removeSessionFiles(ctx context.Context, dir string) error {
	tracked, err := git(ctx, dir, "ls-files", "--", sessionMarkerFile, sessionContextFile)
	if err != nil {
		return err
	}
	if tracked != "" {
		return errors.New("coding worktree tracks a reserved session file")
	}
	return errors.Join(removeFile(filepath.Join(dir, sessionMarkerFile)), removeFile(filepath.Join(dir, sessionContextFile)))
}

func worktreeArtifacts(ctx context.Context, dir string, state *worktreeState, status string) ([]scruntime.Artifact, error) {
	artifacts := []scruntime.Artifact{{Name: "git-status", ContentType: "text/plain", Text: redact.String(strings.TrimSpace(status))}}
	diff, err := git(ctx, dir, "diff", "--no-ext-diff", "--no-textconv", "--no-color", state.Base, state.Tip, "--")
	if err != nil {
		return nil, err
	}
	if diff != "" {
		diff = redact.String(diff)
		if len(diff) > 32*1024 {
			diff = diff[:32*1024] + "\n...[truncated]"
		}
		artifacts = append(artifacts, scruntime.Artifact{Name: "git-diff", ContentType: "text/x-diff", Text: diff})
	}
	log, err := os.ReadFile(dir + ".log")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(log) > 0 {
		clean := redact.String(string(log))
		if len(clean) > 16*1024 {
			clean = "...[truncated]\n" + clean[len(clean)-16*1024:]
		}
		artifacts = append(artifacts, scruntime.Artifact{Name: "session-log", ContentType: "text/plain", Text: strings.TrimSpace(clean)})
	}
	return artifacts, nil
}

func removeWorktree(ctx context.Context, dir string, state *worktreeState) error {
	list, err := git(ctx, state.CommonDir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return err
	}
	for _, field := range strings.Split(list, "\x00") {
		if field == "worktree "+dir {
			if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				if err != nil {
					return err
				}
				if err := state.validateCheckout(ctx, dir); err != nil {
					return err
				}
			}
			_, err := git(ctx, state.CommonDir, "worktree", "remove", "--", dir)
			return err
		}
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		return errors.New("coding worktree path is no longer registered with Git")
	}
	return nil
}

func rollbackWorktree(ctx context.Context, dir string, state *worktreeState) error {
	if err := removeWorktree(ctx, dir, state); err != nil {
		return err
	}
	tip, err := git(ctx, state.CommonDir, "rev-parse", "--verify", "refs/heads/"+state.Branch)
	if err != nil {
		return err
	}
	if strings.TrimSpace(tip) == state.Base {
		if _, err := git(ctx, state.Repo, "branch", "-d", "--", state.Branch); err != nil {
			return err
		}
	}
	return removeFile(dir + stateSuffix)
}

func removeFile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (a *Adapter) pruneFinishedBranches(ctx context.Context, root string, current *worktreeState) {
	if a.BranchRetention <= 0 {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-a.BranchRetention)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), stateSuffix) || entry.Type() != 0 {
			continue
		}
		path := filepath.Join(root, entry.Name())
		state, err := readWorktreeState(path)
		if err != nil || state.CommonDir != current.CommonDir ||
			state.FinishedAt.IsZero() || state.FinishedAt.After(cutoff) ||
			state.Branch != "sablecourt/"+sanitizeSegment(state.TaskID) {
			continue
		}
		tip, err := git(ctx, current.Repo, "rev-parse", "--verify", "refs/heads/"+state.Branch)
		if err != nil || strings.TrimSpace(tip) != state.Tip {
			continue
		}
		if _, err := git(ctx, current.Repo, "merge-base", "--is-ancestor", state.Tip, "HEAD"); err != nil {
			continue
		}
		if _, err := git(ctx, current.Repo, "branch", "-d", "--", state.Branch); err == nil {
			_ = removeFile(path)
		}
	}
}
