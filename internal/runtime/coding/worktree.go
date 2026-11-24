package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	GitDir     string
	GitFile    *gitFileIdentity
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
	owner := gitRepo{dir: repo}
	common, err := owner.run(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", nil, err
	}
	base, err := owner.commitID(ctx, "HEAD")
	if err != nil {
		return "", nil, err
	}
	state := &worktreeState{
		TaskID: taskID, Repo: repo, CommonDir: strings.TrimSuffix(common, "\n"),
		Branch: "sablecourt/" + sanitizeSegment(taskID), Base: base,
	}
	a.pruneFinishedBranches(ctx, filepath.Dir(dir), state)

	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	// Let Git finish registration and checkout so a canceled request can roll them back.
	setupCtx := context.WithoutCancel(ctx)
	if _, err := owner.run(setupCtx, "branch", "--no-track", "--end-of-options", state.Branch, state.Base); err != nil {
		return "", nil, err
	}
	if err := state.createCheckout(setupCtx, dir); err != nil {
		return "", nil, errors.Join(err, rollbackWorktree(setupCtx, dir, state))
	}
	if err := writeWorktreeState(dir, state); err != nil {
		return "", nil, errors.Join(err, rollbackWorktree(setupCtx, dir, state))
	}
	return dir, state, nil
}

func (s *worktreeState) createCheckout(ctx context.Context, dir string) error {
	if _, err := s.commonGit().run(ctx, "worktree", "add", "--no-checkout", "--end-of-options", dir, s.Branch); err != nil {
		return err
	}
	// Discover the registration once, before the workload can modify the checkout.
	gitDir, err := (gitRepo{dir: dir}).run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	s.GitDir = strings.TrimSuffix(gitDir, "\n")
	s.GitFile, err = s.readGitFile(dir)
	if err != nil {
		return err
	}
	checkout, err := s.checkoutGit(dir).withoutFilters(ctx)
	if err != nil {
		return err
	}
	_, err = checkout.run(ctx, "read-tree", "--reset", "-u", "--end-of-options", "HEAD")
	return err
}

func resolvedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func (s *worktreeState) commonGit() gitRepo {
	return gitRepo{dir: s.CommonDir, gitDir: s.CommonDir}
}

func (s *worktreeState) checkoutGit(dir string) gitRepo {
	return gitRepo{dir: s.CommonDir, gitDir: s.GitDir, workTree: dir}
}

func excludeSessionFiles(ctx context.Context, dir string, state *worktreeState) error {
	checkout := state.checkoutGit(dir)
	files := []string{sessionMarkerFile, sessionContextFile}
	out, err := checkout.run(ctx, append([]string{"ls-files", "--"}, files...)...)
	if err != nil {
		return err
	}
	if out != "" {
		return errors.New("repository tracks a reserved session file")
	}
	oldExclude, err := checkout.config(ctx, "--path", "--get", "core.excludesFile")
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
	infoDir := filepath.Join(state.GitDir, "info")
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
	enabled, err := checkout.config(ctx, "--local", "--type=bool", "--get", "extensions.worktreeConfig")
	if err != nil {
		return err
	}
	if enabled != "true" {
		bare, err := checkout.config(ctx, "--local", "--type=bool", "--get", "core.bare")
		if err != nil {
			return err
		}
		worktree, err := checkout.config(ctx, "--local", "--get", "core.worktree")
		if err != nil {
			return err
		}
		if bare == "true" || worktree != "" {
			return errors.New("repository must migrate its Git worktree config before starting")
		}
		if _, err := checkout.run(ctx, "config", "--local", "extensions.worktreeConfig", "true"); err != nil {
			return err
		}
	}
	if _, err := checkout.run(ctx, "config", "--worktree", "core.excludesFile", exclude); err != nil {
		return err
	}
	ignored, err := checkout.run(ctx, append([]string{"check-ignore", "--no-index", "--"}, files...)...)
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
	if !isCommitID(state.Base) {
		return nil, errors.New("invalid base commit ID in coding worktree state")
	}
	if (state.Tip != "" || !state.FinishedAt.IsZero()) &&
		(!isCommitID(state.Tip) || len(state.Base) != len(state.Tip)) {
		return nil, errors.New("invalid tip commit ID in coding worktree state")
	}
	state.Base = strings.ToLower(state.Base)
	state.Tip = strings.ToLower(state.Tip)
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

type gitFileIdentity struct {
	Device uint64
	Inode  uint64
}

func (s *worktreeState) readGitFile(dir string) (*gitFileIdentity, error) {
	if !filepath.IsAbs(s.CommonDir) || filepath.Dir(s.GitDir) != filepath.Join(s.CommonDir, "worktrees") ||
		filepath.Clean(s.GitDir) != s.GitDir {
		return nil, errors.New("coding worktree .git entry has no valid recorded gitdir")
	}
	checkout, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("check coding worktree .git entry: %w", err)
	}
	if !checkout.IsDir() {
		return nil, errors.New("coding worktree .git entry is outside a regular checkout directory")
	}
	path := filepath.Join(dir, ".git")
	entry, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("check coding worktree .git entry: %w", err)
	}
	if !entry.Mode().IsRegular() {
		return nil, errors.New("coding worktree .git entry must be a regular file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open coding worktree .git entry: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat coding worktree .git entry: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !os.SameFile(entry, info) {
		return nil, errors.New("coding worktree .git entry changed while opening it")
	}
	expected := "gitdir: " + s.GitDir + "\n"
	data, err := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
	if err != nil {
		return nil, fmt.Errorf("read coding worktree .git entry: %w", err)
	}
	if string(data) != expected {
		return nil, errors.New("coding worktree .git entry does not point to the recorded gitdir")
	}
	return &gitFileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func (s *worktreeState) validateCheckout(ctx context.Context, dir string) error {
	if s.GitFile == nil {
		return errors.New("coding worktree .git entry has no recorded identity")
	}
	identity, err := s.readGitFile(dir)
	if err != nil {
		return err
	}
	if *identity != *s.GitFile {
		return errors.New("coding worktree .git entry was replaced")
	}
	checkout := s.checkoutGit(dir)
	common, err := checkout.run(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if strings.TrimSuffix(common, "\n") != s.CommonDir {
		return errors.New("coding worktree belongs to another repository")
	}
	head, err := checkout.run(ctx, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != "refs/heads/"+s.Branch {
		return errors.New("coding worktree has another branch checked out")
	}
	return nil
}

func snapshotWorktree(ctx context.Context, checkout gitRepo) error {
	untracked, err := checkout.run(ctx, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return err
	}
	add := []string{"add", "-A", "--"}
	for _, path := range strings.Split(untracked, "\x00") {
		// Without --directory, Git marks embedded, untracked repositories with a trailing slash.
		if nested, ok := strings.CutSuffix(path, "/"); ok {
			if !emptyNestedRepo(checkout.workTree, nested) {
				return fmt.Errorf("cannot safely remove nested Git repository %q", redact.String(nested))
			}
			add = append(add, ":(exclude,literal)"+nested)
		}
	}
	if err := commitWorktreeIndex(ctx, checkout, "chore(coding): preserve staged task edits"); err != nil {
		return err
	}
	if _, err := checkout.run(ctx, add...); err != nil {
		return err
	}
	return commitWorktreeIndex(ctx, checkout, "chore(coding): preserve task edits")
}

func emptyNestedRepo(dir, nested string) bool {
	checkout, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer checkout.Close()
	path := filepath.Join(nested, ".git")
	info, err := checkout.Lstat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	repo, err := checkout.OpenRoot(path)
	if err != nil {
		return false
	}
	defer repo.Close()

	// Inspect the stores without loading config or following a nested gitdir.
	for _, file := range []string{"commondir", "packed-refs", "reftable"} {
		if _, err := repo.Lstat(file); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	for _, store := range []string{"refs", "objects"} {
		err := fs.WalkDir(repo.FS(), store, func(_ string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() {
				return errors.New("nested Git repository has data")
			}
			return err
		})
		if err != nil {
			return false
		}
	}
	return true
}

func commitWorktreeIndex(ctx context.Context, checkout gitRepo, message string) error {
	_, err := checkout.run(ctx, "diff", "--cached", "--quiet", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none", "--")
	if err == nil {
		return nil
	}
	if !gitExitCode(err, 1) {
		return err
	}
	_, err = checkout.run(ctx, "-c", "user.name=Sablecourt", "-c", "user.email=sablecourt",
		"-c", "commit.gpgSign=false", "commit", "--quiet", "--no-status", "-m", message)
	return err
}

func (a *Adapter) finishWorktree(ctx context.Context, dir string, state *worktreeState) error {
	if state.FinishedAt.IsZero() {
		if err := state.validateCheckout(ctx, dir); err != nil {
			return err
		}
		base, err := state.commonGit().commitID(ctx, state.Base)
		if err != nil {
			return err
		}
		if base != state.Base {
			return errors.New("recorded coding worktree base does not identify a commit")
		}
		checkout, err := state.checkoutGit(dir).withoutFilters(ctx)
		if err != nil {
			return err
		}
		if err := removeSessionFiles(ctx, checkout, dir); err != nil {
			return err
		}
		status, err := checkout.run(ctx, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=dirty")
		if err != nil {
			return err
		}
		if status != "" {
			if err := snapshotWorktree(ctx, checkout); err != nil {
				return err
			}
		}
		tip, err := checkout.commitID(ctx, "HEAD")
		if err != nil {
			return err
		}
		state.Tip = tip
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

func removeSessionFiles(ctx context.Context, checkout gitRepo, dir string) error {
	tracked, err := checkout.run(ctx, "ls-files", "--", sessionMarkerFile, sessionContextFile)
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
	diff, err := state.commonGit().run(ctx, "diff", "--no-ext-diff", "--no-textconv", "--no-color",
		"--submodule=short", "--end-of-options", state.Base, state.Tip, "--")
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
	_, statErr := os.Lstat(dir)
	if !errors.Is(statErr, os.ErrNotExist) {
		if statErr != nil {
			return statErr
		}
		if err := state.validateCheckout(ctx, dir); err != nil {
			return err
		}
	}
	common := state.commonGit()
	list, err := common.run(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return err
	}
	for _, field := range strings.Split(list, "\x00") {
		if field == "worktree "+dir {
			_, err := common.run(ctx, "worktree", "remove", "--force", "--", dir)
			return err
		}
	}
	if statErr == nil {
		return errors.New("coding worktree path is no longer registered with Git")
	}
	return nil
}

func rollbackWorktree(ctx context.Context, dir string, state *worktreeState) error {
	if err := removeWorktree(ctx, dir, state); err != nil {
		return err
	}
	common := state.commonGit()
	ref := "refs/heads/" + state.Branch
	tip, err := common.run(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", ref)
	if err != nil && !gitExitCode(err, 1) {
		return err
	}
	if err == nil && strings.TrimSpace(tip) == state.Base {
		if _, err := common.run(ctx, "update-ref", "--no-deref", "-d", "--end-of-options", ref, state.Base); err != nil {
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
		owner := gitRepo{dir: current.Repo}
		tip, err := owner.commitID(ctx, "refs/heads/"+state.Branch)
		if err != nil || strings.TrimSpace(tip) != state.Tip {
			continue
		}
		if _, err := owner.run(ctx, "merge-base", "--is-ancestor", "--end-of-options", state.Tip, "HEAD"); err != nil {
			continue
		}
		if _, err := owner.run(ctx, "branch", "-d", "--", state.Branch); err == nil {
			_ = removeFile(path)
		}
	}
}
