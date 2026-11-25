package coding

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

const (
	sessionMarkerFile   = ".sablecourt-session.json"
	sessionContextFile  = ".sablecourt-context.md"
	stateSuffix         = ".state.json"
	excludeConfigSuffix = ".exclude-config"
)

type worktreeState struct {
	beforeGit    func() error
	configLock   func(context.Context) (func(), error)
	TaskID       string
	Mode         string `json:",omitempty"`
	ReadOnly     bool   `json:",omitempty"`
	CouncilKey   string `json:",omitempty"`
	CheckoutDir  string `json:",omitempty"`
	Repo         string
	CommonDir    string
	GitDir       string
	GitFile      *gitFileIdentity
	Registration map[string]fileStamp
	CleanupError string `json:",omitempty"`
	Branch       string
	Base         string
	Tip          string
	FinishedAt   time.Time
	Artifacts    []scruntime.Artifact
	Worker       *workerIdentity `json:",omitempty"`
	Result       *processResult  `json:",omitempty"`
}

func (s *worktreeState) checkoutDir(taskBase string) string {
	if s.CheckoutDir != "" {
		return s.CheckoutDir
	}
	return taskBase
}

func (a *Adapter) taskBase(taskID string) (string, error) {
	if a == nil || a.WorktreeRoot == "" || taskID == "" {
		return "", errors.New("worktree root and task ID are required")
	}
	root, err := resolvedPath(a.WorktreeRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, sanitizeSegment(taskID)), nil
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
	for _, path := range []string{dir, dir + stateSuffix, dir + ".log", dir + excludeConfigSuffix} {
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
	owner, err := sourceGit(repo)
	if err != nil {
		return "", nil, err
	}
	list, err := owner.run(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", nil, err
	}
	primary, _, _ := strings.Cut(list, "\x00\x00")
	fields := strings.Split(primary, "\x00")
	if fields[0] != "worktree "+repo || len(fields) < 2 || !strings.HasPrefix(fields[1], "HEAD ") {
		return "", nil, errors.New("configured repository is not the registered primary worktree")
	}
	base, err := owner.commitID(ctx, "HEAD")
	if err != nil {
		return "", nil, err
	}
	state := &worktreeState{
		beforeGit: owner.beforeRun,
		TaskID:    taskID, Repo: repo, CommonDir: owner.gitDir,
		Branch: "sablecourt/" + sanitizeSegment(taskID), Base: base,
	}
	a.bindConfigLock(state)
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
	if err := excludeSessionFiles(setupCtx, dir, state); err != nil {
		return "", nil, errors.Join(err, rollbackWorktree(setupCtx, dir, state))
	}
	if err := writeWorktreeState(dir, state); err != nil {
		return "", nil, errors.Join(err, rollbackWorktree(setupCtx, dir, state))
	}
	state.beforeGit = nil
	return dir, state, nil
}

func (s *worktreeState) createCheckout(ctx context.Context, dir string) error {
	// Registration shares the repository's metadata with every other launch
	// and with the config writers that take this same gate.
	release := func() {}
	if s.configLock != nil {
		rel, err := s.configLock(ctx)
		if err != nil {
			return err
		}
		release = rel
	}
	_, err := s.commonGit().run(ctx, "worktree", "add", "--no-checkout", "--end-of-options", dir, s.Branch)
	release()
	if err != nil {
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
	return gitRepo{beforeRun: s.beforeGit, dir: s.CommonDir, gitDir: s.CommonDir}
}

func (s *worktreeState) checkoutGit(dir string) gitRepo {
	return gitRepo{beforeRun: s.beforeGit, dir: s.CommonDir, gitDir: s.GitDir, workTree: dir}
}

func excludeSessionFiles(ctx context.Context, dir string, state *worktreeState) error {
	checkout := state.checkoutGit(dir)
	out, err := checkout.run(ctx, "ls-files", "--", sessionMarkerFile, sessionContextFile)
	if err != nil {
		return err
	}
	if out != "" {
		return errors.New("repository tracks a reserved session file")
	}
	infoDir := filepath.Join(state.GitDir, "info")
	if err := os.Mkdir(infoDir, 0o700); err != nil {
		return err
	}
	exclude := filepath.Join(infoDir, "exclude")
	if err := os.WriteFile(exclude, []byte("/"+sessionMarkerFile+"\n/"+sessionContextFile+"\n"), 0o600); err != nil {
		return err
	}
	// Plain Git looks in the common info directory. Select the private exclude
	// file only for this registration, using config outside the workload.
	config := dir + excludeConfigSuffix
	if _, err := checkout.run(ctx, "config", "--file", config, "core.excludesFile", exclude); err != nil {
		return err
	}
	state.Registration = make(map[string]fileStamp)
	for _, path := range state.registrationPaths(dir) {
		_, stamp, err := readMetadata(path)
		if err != nil {
			return err
		}
		state.Registration[path] = stamp
	}
	if err := state.validateRegistration(dir); err != nil {
		return err
	}
	if err := state.updateCommonConfig(ctx, "--add", state.excludeIncludeKey(), config); err != nil {
		return err
	}
	return state.checkSessionExcludes(ctx, dir)
}

func (s *worktreeState) checkSessionExcludes(ctx context.Context, dir string) error {
	exclude := filepath.Join(s.GitDir, "info", "exclude")
	ignored, err := s.checkoutGit(dir).run(ctx, "-c", "core.excludesFile="+exclude,
		"check-ignore", "--no-index", "--", sessionMarkerFile, sessionContextFile)
	if err != nil && !gitExitCode(err, 1) {
		return err
	}
	if ignored != sessionMarkerFile+"\n"+sessionContextFile+"\n" {
		return errors.New("repository ignore rules override the session excludes")
	}
	return nil
}

func (s *worktreeState) excludeIncludeKey() string {
	escape := strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`)
	return "includeIf.gitdir:" + escape.Replace(s.GitDir) + ".path"
}

func (s *worktreeState) registrationPaths(dir string) []string {
	if s.Mode != "" {
		return []string{filepath.Join(s.GitDir, "commondir"), filepath.Join(s.GitDir, "gitdir")}
	}
	return []string{filepath.Join(s.GitDir, "commondir"), filepath.Join(s.GitDir, "gitdir"),
		filepath.Join(s.GitDir, "info", "exclude"), dir + excludeConfigSuffix}
}

type fileStamp struct {
	gitFileIdentity
	Digest   [sha256.Size]byte
	Mode     os.FileMode
	Modified int64
}

// Registration pointers and the exclude files are short, regular files.
func readMetadata(path string) ([]byte, fileStamp, error) {
	var stamp fileStamp
	actual, err := filepath.EvalSymlinks(path)
	if err != nil || actual != path {
		return nil, stamp, fmt.Errorf("invalid Git metadata path %s", path)
	}
	entry, err := os.Lstat(path)
	if err != nil {
		return nil, stamp, err
	}
	if !entry.Mode().IsRegular() {
		return nil, stamp, fmt.Errorf("invalid Git metadata file %s", path)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, stamp, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, stamp, err
	}
	const limit = 64 * 1024
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !os.SameFile(entry, info) || info.Size() > limit {
		return nil, stamp, fmt.Errorf("invalid Git metadata file %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, stamp, err
	}
	if len(data) > limit {
		return nil, stamp, fmt.Errorf("oversized Git metadata file %s", path)
	}
	stamp.gitFileIdentity = gitFileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
	stamp.Digest = sha256.Sum256(data)
	stamp.Mode = info.Mode()
	stamp.Modified = info.ModTime().UnixNano()
	return data, stamp, nil
}

func (s *worktreeState) validateRegistration(dir string) error {
	if _, err := os.Lstat(filepath.Join(s.GitDir, "config.worktree")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("coding worktree gitdir has an unexpected or inaccessible config.worktree")
	}
	for _, path := range s.registrationPaths(dir) {
		_, stamp, err := readMetadata(path)
		if err != nil {
			return err
		}
		if stamp != s.Registration[path] {
			return fmt.Errorf("coding worktree registration file %q changed", path)
		}
	}
	return nil
}

func (a *Adapter) worktreeState(handle scruntime.Handle) (*worktreeState, error) {
	root, err := resolvedPath(a.WorktreeRoot)
	if err != nil {
		return nil, err
	}
	if handle.ExternalID == "" {
		return nil, errors.New("invalid coding worktree handle")
	}
	segment := sanitizeSegment(handle.ExternalID)
	taskBase := filepath.Join(root, segment)
	if handle.LogPath != "" && handle.LogPath != taskBase+".log" {
		return nil, errors.New("invalid coding worktree handle")
	}
	state, err := readWorktreeState(taskBase + stateSuffix)
	if err != nil {
		return nil, err
	}
	if state.Mode == "" {
		if handle.WorkDir != taskBase || handle.Branch != "sablecourt/"+segment {
			return nil, errors.New("invalid coding worktree handle")
		}
	} else {
		if handle.WorkDir != state.checkoutDir(taskBase) || handle.Branch != state.Branch {
			return nil, errors.New("invalid coding worktree handle")
		}
	}
	if state.Worker != nil && (state.Worker.PID != handle.PID || state.Worker.Start.IsZero() || !state.Worker.Start.Equal(handle.ProcessStartTime)) {
		return nil, errors.New("coding worker identity does not match handle")
	}
	if state.TaskID != handle.ExternalID || state.Branch != handle.Branch {
		return nil, errors.New("coding worktree state does not match handle")
	}
	a.bindConfigLock(state)
	return state, nil
}

func (a *Adapter) bindConfigLock(state *worktreeState) {
	state.configLock = func(ctx context.Context) (func(), error) {
		return a.lockScope(ctx, "config:"+state.CommonDir, false)
	}
}

func (s *worktreeState) updateCommonConfig(ctx context.Context, args ...string) error {
	if s.configLock != nil {
		release, err := s.configLock(ctx)
		if err != nil {
			return err
		}
		defer release()
	}
	_, err := s.commonGit().run(ctx, append([]string{"config", "--local"}, args...)...)
	return err
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
	data, stamp, err := readMetadata(filepath.Join(dir, ".git"))
	if err != nil {
		return nil, fmt.Errorf("check coding worktree .git entry: %w", err)
	}
	if string(data) != "gitdir: "+s.GitDir+"\n" {
		return nil, errors.New("coding worktree .git entry does not point to the recorded gitdir")
	}
	return &stamp.gitFileIdentity, nil
}

func (s *worktreeState) validateCheckout(ctx context.Context, dir string, launched bool) error {
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
	if launched {
		s.beforeGit = func() error { return s.validateRegistration(dir) }
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

func snapshotWorktree(ctx context.Context, checkout gitRepo, exclude []string) error {
	add := append([]string{"add", "-A", "--"}, exclude...)
	if err := commitWorktreeIndex(ctx, checkout, "chore(coding): preserve staged task edits"); err != nil {
		return err
	}
	if _, err := checkout.run(ctx, add...); err != nil {
		return err
	}
	return commitWorktreeIndex(ctx, checkout, "chore(coding): preserve task edits")
}

func commitWorktreeIndex(ctx context.Context, checkout gitRepo, message string) error {
	_, err := checkout.run(ctx, "diff", "--cached", "--quiet", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none", "--")
	if err == nil {
		return nil
	}
	if !gitExitCode(err, 1) {
		return err
	}
	// commit refreshes the whole index and reads initialized submodule
	// configuration even with --no-status. Commit the verified index directly.
	parent, err := checkout.commitID(ctx, "HEAD")
	if err != nil {
		return err
	}
	tree, err := checkout.run(ctx, "write-tree")
	if err != nil {
		return err
	}
	tree = strings.TrimSuffix(tree, "\n")
	if !isCommitID(tree) {
		return errors.New("Git returned an invalid tree ID")
	}
	result, err := checkout.run(ctx, "-c", "user.name=Sablecourt", "-c", "user.email=sablecourt", "-c", "commit.gpgSign=false", "commit-tree", tree, "-p", parent, "-m", message)
	if err != nil {
		return err
	}
	commit := strings.TrimSuffix(result, "\n")
	if !isCommitID(commit) {
		return errors.New("Git returned an invalid snapshot commit ID")
	}
	_, err = checkout.run(ctx, "update-ref", "--end-of-options", "HEAD", commit, parent)
	return err
}

func (a *Adapter) finishWorktree(ctx context.Context, dir string, state *worktreeState) (finishErr error) {
	defer func() {
		if finishErr != nil {
			state.CleanupError = redact.String(finishErr.Error())
			finishErr = errors.Join(fmt.Errorf("coding worktree cleanup incomplete: %w", finishErr), writeWorktreeState(dir, state))
		}
	}()
	if state.FinishedAt.IsZero() {
		if err := state.validateCheckout(ctx, dir, true); err != nil {
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
		exclude, err := inspectNested(ctx, checkout, "HEAD", state.Base, filepath.Join(state.GitDir, "modules"))
		if err != nil {
			return err
		}
		if err := removeSessionFiles(ctx, checkout, dir); err != nil {
			return err
		}
		status, err := checkout.run(ctx, append([]string{"status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=all", "--"}, exclude...)...)
		if err != nil {
			return err
		}
		if status != "" {
			if err := snapshotWorktree(ctx, checkout, exclude); err != nil {
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
		state.CleanupError = ""
		state.FinishedAt = time.Now().UTC()
		if err := writeWorktreeState(dir, state); err != nil {
			return err
		}
	}
	if err := removeWorktree(ctx, dir, state, true); err != nil {
		return err
	}
	if err := removeFile(dir + ".log"); err != nil {
		return err
	}
	if state.Worker != nil {
		if err := os.RemoveAll(dir + ".run"); err != nil {
			return err
		}
	}
	a.running.Delete(dir)
	if state.CleanupError != "" {
		state.CleanupError = ""
		return writeWorktreeState(dir, state)
	}
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
			diff = truncateUTF8Prefix(diff, 32*1024) + "\n...[truncated]"
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
	if state.Result != nil && state.Result.Response != "" {
		artifacts = append(artifacts, scruntime.Artifact{Name: "final-message", ContentType: "text/plain", Text: state.Result.Response})
	}
	return artifacts, nil
}

func verifySavedWorktree(ctx context.Context, dir string, state *worktreeState) error {
	checkout, err := state.checkoutGit(dir).withoutFilters(ctx)
	if err != nil {
		return err
	}
	exclude, err := inspectNested(ctx, checkout, state.Tip, state.Base, filepath.Join(state.GitDir, "modules"))
	if err != nil {
		return err
	}
	// Git's snapshot skips ignored files. List individual files before force removal.
	args := append([]string{"-c", "core.untrackedCache=false", "status", "--porcelain=v1", "-z",
		"--untracked-files=all", "--ignored=traditional", "--ignore-submodules=all", "--"}, exclude...)
	status, err := checkout.run(ctx, args...)
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("coding worktree has unsaved files")
	}
	return nil
}

func removeWorktree(ctx context.Context, dir string, state *worktreeState, launched bool) error {
	_, statErr := os.Lstat(dir)
	if !errors.Is(statErr, os.ErrNotExist) {
		if statErr != nil {
			return statErr
		}
		if err := state.validateCheckout(ctx, dir, launched); err != nil {
			return err
		}
	}
	if errors.Is(statErr, os.ErrNotExist) && launched {
		if _, err := os.Lstat(state.GitDir); !errors.Is(err, os.ErrNotExist) {
			return errors.New("coding worktree is missing but its gitdir remains")
		}
	}
	common := state.commonGit()
	list, err := common.run(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return err
	}
	for _, field := range strings.Split(list, "\x00") {
		if field == "worktree "+dir {
			if launched && statErr == nil {
				if err := verifySavedWorktree(ctx, dir, state); err != nil {
					return err
				}
			}
			if _, err := common.run(ctx, "worktree", "remove", "--force", "--", dir); err != nil {
				return err
			}
			statErr = os.ErrNotExist
			common.beforeRun = nil
			state.beforeGit = nil
			break
		}
	}
	if statErr == nil {
		return errors.New("coding worktree path is no longer registered with Git")
	}
	config := dir + excludeConfigSuffix
	if _, err := os.Lstat(config); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		if err := state.updateCommonConfig(ctx, "--fixed-value", "--unset-all", state.excludeIncludeKey(), config); err != nil && !gitExitCode(err, 5) {
			return err
		}
		return removeFile(config)
	}
	return nil
}

func rollbackWorktree(ctx context.Context, dir string, state *worktreeState) error {
	if err := removeWorktree(ctx, dir, state, false); err != nil {
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
		id := strings.TrimSuffix(entry.Name(), stateSuffix)
		release, err := a.lockTask(ctx, id, true)
		if err != nil {
			continue
		}
		pruneFinishedBranch(ctx, filepath.Join(root, entry.Name()), id, current, cutoff)
		release()
	}
}

func pruneFinishedBranch(ctx context.Context, path, id string, current *worktreeState, cutoff time.Time) {
	state, err := readWorktreeState(path)
	if err != nil || state.CommonDir != current.CommonDir ||
		state.CleanupError != "" || state.FinishedAt.IsZero() || state.FinishedAt.After(cutoff) ||
		id != sanitizeSegment(state.TaskID) || state.Branch != "sablecourt/"+id {
		return
	}
	owner := current.commonGit()
	tip, err := owner.commitID(ctx, "refs/heads/"+state.Branch)
	if err != nil || strings.TrimSpace(tip) != state.Tip {
		return
	}
	if _, err := owner.run(ctx, "merge-base", "--is-ancestor", "--end-of-options", state.Tip, "HEAD"); err != nil {
		return
	}
	if _, err := owner.run(ctx, "branch", "-d", "--", state.Branch); err == nil {
		_ = removeFile(path)
	}
}
