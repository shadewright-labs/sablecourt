package coding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

const councilsDirName = ".councils"

type sharedWorktree struct {
	Dir          string
	Commit       string
	GitDir       string
	GitFile      *gitFileIdentity
	Registration map[string]fileStamp
}

type councilState struct {
	ConclaveID         string
	Key                string
	Mode               string
	Repo               string
	CommonDir          string
	BaseRef            string
	BaseCommit         string
	RefWorktree        *sharedWorktree            `json:",omitempty"`
	WriterRole         string                     `json:",omitempty"`
	WriterBranch       string                     `json:",omitempty"`
	WriterTip          string                     `json:",omitempty"`
	WriterWorktree     *sharedWorktree            `json:",omitempty"`
	ReviewWorktrees    map[string]*sharedWorktree `json:",omitempty"`
	ActiveWriterTaskID string                     `json:",omitempty"`
	ActiveTasks        []string                   `json:",omitempty"`
}

func councilKey(conclaveID string) string {
	sum := sha256.Sum256([]byte(conclaveID))
	slug := sanitizeSegment(conclaveID)
	if len(slug) > 24 {
		slug = slug[:24]
	}
	return slug + "-" + hex.EncodeToString(sum[:8])
}

func councilWriterBranch(key string) string {
	return "sablecourt/council-" + key
}

func councilStatePath(root, key string) string {
	return filepath.Join(root, councilsDirName, key+".json")
}

func councilDirPath(root, key string) string {
	return filepath.Join(root, councilsDirName, key)
}

func readCouncilState(path string) (*councilState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cs councilState
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("decode council checkout state: %w", err)
	}
	if cs.ConclaveID == "" || cs.Key == "" || cs.Repo == "" || cs.CommonDir == "" || !isCommitID(cs.BaseCommit) {
		return nil, errors.New("invalid council checkout state")
	}
	if cs.Mode != checkoutModeRef && cs.Mode != checkoutModeWriter {
		return nil, errors.New("invalid council checkout mode in state")
	}
	cs.BaseCommit = strings.ToLower(cs.BaseCommit)
	if cs.WriterTip != "" {
		if !isCommitID(cs.WriterTip) || len(cs.WriterTip) != len(cs.BaseCommit) {
			return nil, errors.New("invalid writer tip commit ID in council state")
		}
		cs.WriterTip = strings.ToLower(cs.WriterTip)
	}
	return &cs, nil
}

func writeCouncilState(root, key string, cs *councilState) error {
	dir := filepath.Join(root, councilsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".council-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(cs)
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), councilStatePath(root, key))
}

func (a *Adapter) verifyPrimaryRepo(ctx context.Context, repo string, readOnly bool) (string, gitRepo, error) {
	resolved, err := resolvedPath(repo)
	if err != nil {
		return "", gitRepo{}, err
	}
	owner, err := sourceGit(resolved)
	if err != nil {
		return "", gitRepo{}, err
	}
	owner.readOnly = readOnly
	list, err := owner.run(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", gitRepo{}, err
	}
	primary, _, _ := strings.Cut(list, "\x00\x00")
	fields := strings.Split(primary, "\x00")
	if fields[0] != "worktree "+resolved || len(fields) < 2 || !strings.HasPrefix(fields[1], "HEAD ") {
		return "", gitRepo{}, errors.New("configured repository is not the registered primary worktree")
	}
	return resolved, owner, nil
}

func (a *Adapter) reserveTaskBase(taskID string) (string, string, error) {
	if a.WorktreeRoot == "" || taskID == "" {
		return "", "", errors.New("worktree root and task ID are required")
	}
	if err := os.MkdirAll(a.WorktreeRoot, 0o700); err != nil {
		return "", "", fmt.Errorf("mkdir worktree root: %w", err)
	}
	root, err := resolvedPath(a.WorktreeRoot)
	if err != nil {
		return "", "", err
	}
	base := filepath.Join(root, sanitizeSegment(taskID))
	for _, path := range []string{base, base + stateSuffix, base + ".log", base + ".run", base + excludeConfigSuffix} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				err = os.ErrExist
			}
			return "", "", fmt.Errorf("worktree path unavailable: %w", err)
		}
	}
	return root, base, nil
}

func (a *Adapter) prepareLiveCheckout(ctx context.Context, repo, taskID string) (string, string, *worktreeState, func(context.Context) error, error) {
	if repo == "" {
		return "", "", nil, nil, errors.New("worktree root, repository and task ID are required")
	}
	_, taskBase, err := a.reserveTaskBase(taskID)
	if err != nil {
		return "", "", nil, nil, err
	}
	resolvedRepo, owner, err := a.verifyPrimaryRepo(ctx, repo, true)
	if err != nil {
		return "", "", nil, nil, err
	}
	head, err := owner.commitID(ctx, "HEAD")
	if err != nil {
		return "", "", nil, nil, err
	}
	state := &worktreeState{
		TaskID:      taskID,
		Mode:        checkoutModeLive,
		ReadOnly:    true,
		CheckoutDir: resolvedRepo,
		Repo:        resolvedRepo,
		CommonDir:   owner.gitDir,
		GitDir:      owner.gitDir,
		Base:        head,
	}
	if err := writeWorktreeState(taskBase, state); err != nil {
		return "", "", nil, nil, err
	}
	rollback := func(_ context.Context) error {
		return errors.Join(
			removeFile(taskBase+stateSuffix),
			removeFile(taskBase+".log"),
			os.RemoveAll(taskBase+".run"),
		)
	}
	return resolvedRepo, taskBase, state, rollback, nil
}

func (a *Adapter) prepareRefCheckout(ctx context.Context, repo, taskID string, input launchInput) (string, string, *worktreeState, func(context.Context) error, error) {
	if repo == "" {
		return "", "", nil, nil, errors.New("worktree root, repository and task ID are required")
	}
	root, taskBase, err := a.reserveTaskBase(taskID)
	if err != nil {
		return "", "", nil, nil, err
	}
	resolvedRepo, owner, err := a.verifyPrimaryRepo(ctx, repo, false)
	if err != nil {
		return "", "", nil, nil, err
	}

	key := councilKey(input.conclaveID)
	releaseCouncil, err := a.lockScope(ctx, "council:"+key, false)
	if err != nil {
		return "", "", nil, nil, err
	}

	var (
		cs             *councilState
		createdCouncil bool
	)
	statePath := councilStatePath(root, key)
	existing, readErr := readCouncilState(statePath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		releaseCouncil()
		return "", "", nil, nil, readErr
	}
	if errors.Is(readErr, os.ErrNotExist) {
		commit, err := owner.commitID(ctx, input.checkoutRef)
		if err != nil {
			releaseCouncil()
			return "", "", nil, nil, fmt.Errorf("resolve checkout ref: %w", err)
		}
		setupCtx := context.WithoutCancel(ctx)
		wtDir := filepath.Join(councilDirPath(root, key), "ref")
		if err := os.MkdirAll(filepath.Dir(wtDir), 0o700); err != nil {
			releaseCouncil()
			return "", "", nil, nil, err
		}
		wt, err := createDetachedWorktree(setupCtx, owner, wtDir, commit)
		if err != nil {
			_ = os.RemoveAll(councilDirPath(root, key))
			releaseCouncil()
			return "", "", nil, nil, err
		}
		cs = &councilState{
			ConclaveID:  input.conclaveID,
			Key:         key,
			Mode:        checkoutModeRef,
			Repo:        resolvedRepo,
			CommonDir:   owner.gitDir,
			BaseRef:     input.checkoutRef,
			BaseCommit:  commit,
			RefWorktree: wt,
			ActiveTasks: []string{taskID},
		}
		if err := writeCouncilState(root, key, cs); err != nil {
			_ = removeSharedWorktree(setupCtx, owner.gitDir, wt, false)
			_ = os.RemoveAll(councilDirPath(root, key))
			releaseCouncil()
			return "", "", nil, nil, err
		}
		createdCouncil = true
	} else {
		cs = existing
		if cs.Mode != checkoutModeRef || cs.Repo != resolvedRepo || cs.ConclaveID != input.conclaveID || cs.BaseRef != input.checkoutRef || cs.RefWorktree == nil {
			releaseCouncil()
			return "", "", nil, nil, errors.New("council checkout parameters do not match existing council")
		}
		if err := validateDetachedWorktree(ctx, cs.CommonDir, cs.RefWorktree); err != nil {
			releaseCouncil()
			return "", "", nil, nil, err
		}
		if !slices.Contains(cs.ActiveTasks, taskID) {
			cs.ActiveTasks = append(cs.ActiveTasks, taskID)
		}
		if err := writeCouncilState(root, key, cs); err != nil {
			releaseCouncil()
			return "", "", nil, nil, err
		}
	}
	wt := cs.RefWorktree
	commit := cs.BaseCommit
	commonDir := cs.CommonDir
	releaseCouncil()

	state := &worktreeState{
		TaskID:       taskID,
		Mode:         checkoutModeRef,
		ReadOnly:     true,
		CouncilKey:   key,
		CheckoutDir:  wt.Dir,
		Repo:         resolvedRepo,
		CommonDir:    commonDir,
		GitDir:       wt.GitDir,
		GitFile:      wt.GitFile,
		Registration: wt.Registration,
		Base:         commit,
	}
	rollback := func(rbCtx context.Context) error {
		var rbErr error
		rel, lockErr := a.lockScope(rbCtx, "council:"+key, false)
		if lockErr == nil {
			if cur, err := readCouncilState(statePath); err == nil {
				cur.ActiveTasks = removeString(cur.ActiveTasks, taskID)
				if createdCouncil && len(cur.ActiveTasks) == 0 && cur.RefWorktree != nil {
					rbErr = errors.Join(rbErr, removeSharedWorktree(rbCtx, cur.CommonDir, cur.RefWorktree, false))
					rbErr = errors.Join(rbErr, os.RemoveAll(councilDirPath(root, key)))
					rbErr = errors.Join(rbErr, removeFile(statePath))
				} else {
					rbErr = errors.Join(rbErr, writeCouncilState(root, key, cur))
				}
			}
			rel()
		} else {
			rbErr = errors.Join(rbErr, lockErr)
		}
		return errors.Join(rbErr,
			removeFile(taskBase+stateSuffix),
			removeFile(taskBase+".log"),
			os.RemoveAll(taskBase+".run"),
		)
	}
	if err := writeWorktreeState(taskBase, state); err != nil {
		return "", "", nil, nil, errors.Join(err, rollback(context.WithoutCancel(ctx)))
	}
	return wt.Dir, taskBase, state, rollback, nil
}

func (a *Adapter) prepareWriterCheckout(ctx context.Context, repo, taskID string, input launchInput) (string, string, *worktreeState, func(context.Context) error, error) {
	if repo == "" {
		return "", "", nil, nil, errors.New("worktree root, repository and task ID are required")
	}
	root, taskBase, err := a.reserveTaskBase(taskID)
	if err != nil {
		return "", "", nil, nil, err
	}
	resolvedRepo, owner, err := a.verifyPrimaryRepo(ctx, repo, false)
	if err != nil {
		return "", "", nil, nil, err
	}

	isWriter := input.role == input.checkoutWriter
	key := councilKey(input.conclaveID)
	releaseCouncil, err := a.lockScope(ctx, "council:"+key, false)
	if err != nil {
		return "", "", nil, nil, err
	}

	var (
		cs              *councilState
		createdCouncil  bool
		createdWriter   bool
		createdReviewAt string
		chosenWT        *sharedWorktree
		turnBase        string
		branch          string
	)
	statePath := councilStatePath(root, key)
	existing, readErr := readCouncilState(statePath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		releaseCouncil()
		return "", "", nil, nil, readErr
	}
	setupCtx := context.WithoutCancel(ctx)
	if errors.Is(readErr, os.ErrNotExist) {
		ref := input.checkoutRef
		if ref == "" {
			ref = "HEAD"
		}
		baseCommit, err := owner.commitID(ctx, ref)
		if err != nil {
			releaseCouncil()
			return "", "", nil, nil, fmt.Errorf("resolve checkout ref: %w", err)
		}
		cs = &councilState{
			ConclaveID:      input.conclaveID,
			Key:             key,
			Mode:            checkoutModeWriter,
			Repo:            resolvedRepo,
			CommonDir:       owner.gitDir,
			BaseRef:         input.checkoutRef,
			BaseCommit:      baseCommit,
			WriterRole:      input.checkoutWriter,
			WriterBranch:    councilWriterBranch(key),
			ReviewWorktrees: make(map[string]*sharedWorktree),
		}
		createdCouncil = true
	} else {
		cs = existing
		if cs.Mode != checkoutModeWriter || cs.Repo != resolvedRepo || cs.ConclaveID != input.conclaveID {
			releaseCouncil()
			return "", "", nil, nil, errors.New("council checkout parameters do not match existing council")
		}
		if cs.WriterRole != input.checkoutWriter {
			releaseCouncil()
			return "", "", nil, nil, errors.New("council already has a different writer role")
		}
		if input.checkoutRef != "" && input.checkoutRef != cs.BaseRef {
			releaseCouncil()
			return "", "", nil, nil, errors.New("council checkout ref does not match existing council")
		}
		if cs.ReviewWorktrees == nil {
			cs.ReviewWorktrees = make(map[string]*sharedWorktree)
		}
	}

	if isWriter {
		if cs.ActiveWriterTaskID != "" {
			releaseCouncil()
			return "", "", nil, nil, errors.New("council writer worktree is already active")
		}
		if cs.WriterWorktree == nil {
			wtDir := filepath.Join(councilDirPath(root, key), "writer")
			if err := os.MkdirAll(filepath.Dir(wtDir), 0o700); err != nil {
				releaseCouncil()
				return "", "", nil, nil, err
			}
			wt, err := createWriterWorktree(setupCtx, owner, wtDir, cs.WriterBranch, cs.BaseCommit)
			if err != nil {
				if createdCouncil {
					_ = os.RemoveAll(councilDirPath(root, key))
				}
				releaseCouncil()
				return "", "", nil, nil, err
			}
			cs.WriterWorktree = wt
			createdWriter = true
			turnBase = cs.BaseCommit
		} else {
			head, err := validateWriterWorktree(ctx, cs.CommonDir, cs.WriterBranch, cs.WriterWorktree)
			if err != nil {
				releaseCouncil()
				return "", "", nil, nil, err
			}
			turnBase = head
		}
		cs.ActiveWriterTaskID = taskID
		chosenWT = cs.WriterWorktree
		branch = cs.WriterBranch
	} else {
		target := cs.WriterTip
		if target == "" {
			target = cs.BaseCommit
		}
		wt := cs.ReviewWorktrees[target]
		if wt == nil {
			wtDir := filepath.Join(councilDirPath(root, key), "review-"+target[:12])
			if err := os.MkdirAll(filepath.Dir(wtDir), 0o700); err != nil {
				releaseCouncil()
				return "", "", nil, nil, err
			}
			created, err := createDetachedWorktree(setupCtx, owner, wtDir, target)
			if err != nil {
				if createdCouncil {
					_ = os.RemoveAll(councilDirPath(root, key))
				}
				releaseCouncil()
				return "", "", nil, nil, err
			}
			cs.ReviewWorktrees[target] = created
			createdReviewAt = target
			wt = created
		} else if err := validateDetachedWorktree(ctx, cs.CommonDir, wt); err != nil {
			releaseCouncil()
			return "", "", nil, nil, err
		}
		chosenWT = wt
		turnBase = target
	}

	if !slices.Contains(cs.ActiveTasks, taskID) {
		cs.ActiveTasks = append(cs.ActiveTasks, taskID)
	}
	if err := writeCouncilState(root, key, cs); err != nil {
		if createdWriter && cs.WriterWorktree != nil {
			_ = removeWriterWorktreeAndBranch(setupCtx, cs.CommonDir, cs.WriterBranch, cs.BaseCommit, cs.WriterWorktree)
		}
		if createdReviewAt != "" {
			_ = removeSharedWorktree(setupCtx, cs.CommonDir, cs.ReviewWorktrees[createdReviewAt], false)
		}
		if createdCouncil {
			_ = os.RemoveAll(councilDirPath(root, key))
		}
		releaseCouncil()
		return "", "", nil, nil, err
	}
	commonDir := cs.CommonDir
	releaseCouncil()

	state := &worktreeState{
		TaskID:       taskID,
		Mode:         checkoutModeWriter,
		ReadOnly:     !isWriter,
		CouncilKey:   key,
		CheckoutDir:  chosenWT.Dir,
		Repo:         resolvedRepo,
		CommonDir:    commonDir,
		GitDir:       chosenWT.GitDir,
		GitFile:      chosenWT.GitFile,
		Registration: chosenWT.Registration,
		Branch:       branch,
		Base:         turnBase,
	}
	rollback := func(rbCtx context.Context) error {
		var rbErr error
		rel, lockErr := a.lockScope(rbCtx, "council:"+key, false)
		if lockErr == nil {
			if cur, err := readCouncilState(statePath); err == nil {
				cur.ActiveTasks = removeString(cur.ActiveTasks, taskID)
				if isWriter && cur.ActiveWriterTaskID == taskID {
					cur.ActiveWriterTaskID = ""
				}
				if createdWriter && cur.WriterTip == "" && cur.WriterWorktree != nil {
					rbErr = errors.Join(rbErr, removeWriterWorktreeAndBranch(rbCtx, cur.CommonDir, cur.WriterBranch, cur.BaseCommit, cur.WriterWorktree))
					cur.WriterWorktree = nil
				}
				if createdReviewAt != "" && len(cur.ActiveTasks) == 0 {
					if rwt := cur.ReviewWorktrees[createdReviewAt]; rwt != nil {
						rbErr = errors.Join(rbErr, removeSharedWorktree(rbCtx, cur.CommonDir, rwt, false))
						delete(cur.ReviewWorktrees, createdReviewAt)
					}
				}
				if createdCouncil && len(cur.ActiveTasks) == 0 && cur.WriterWorktree == nil && len(cur.ReviewWorktrees) == 0 {
					rbErr = errors.Join(rbErr, os.RemoveAll(councilDirPath(root, key)))
					rbErr = errors.Join(rbErr, removeFile(statePath))
				} else {
					rbErr = errors.Join(rbErr, writeCouncilState(root, key, cur))
				}
			}
			rel()
		} else {
			rbErr = errors.Join(rbErr, lockErr)
		}
		return errors.Join(rbErr,
			removeFile(taskBase+stateSuffix),
			removeFile(taskBase+".log"),
			os.RemoveAll(taskBase+".run"),
		)
	}
	if err := writeWorktreeState(taskBase, state); err != nil {
		return "", "", nil, nil, errors.Join(err, rollback(context.WithoutCancel(ctx)))
	}
	return chosenWT.Dir, taskBase, state, rollback, nil
}

func (a *Adapter) finishCouncilTask(ctx context.Context, taskBase string, state *worktreeState) (finishErr error) {
	defer func() {
		if finishErr != nil {
			state.CleanupError = redact.String(finishErr.Error())
			finishErr = errors.Join(fmt.Errorf("coding council checkout cleanup incomplete: %w", finishErr), writeWorktreeState(taskBase, state))
		}
	}()

	checkoutDir := state.checkoutDir(taskBase)
	justFinished := state.FinishedAt.IsZero()
	if justFinished {
		if state.Mode == checkoutModeWriter && !state.ReadOnly {
			if err := state.validateCheckout(ctx, checkoutDir, true); err != nil {
				return err
			}
			base, err := state.commonGit().commitID(ctx, state.Base)
			if err != nil {
				return err
			}
			if base != state.Base {
				return errors.New("recorded coding worktree base does not identify a commit")
			}
			checkout, err := state.checkoutGit(checkoutDir).withoutFilters(ctx)
			if err != nil {
				return err
			}
			exclude, err := inspectNested(ctx, checkout, "HEAD", state.Base, filepath.Join(state.GitDir, "modules"))
			if err != nil {
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
			state.Artifacts, err = worktreeArtifacts(ctx, taskBase, state, status)
			if err != nil {
				return err
			}
		} else {
			state.Tip = state.Base
			var err error
			state.Artifacts, err = readOnlyArtifacts(taskBase, state)
			if err != nil {
				return err
			}
		}
		state.CleanupError = ""
		state.FinishedAt = time.Now().UTC()
		if err := writeWorktreeState(taskBase, state); err != nil {
			return err
		}
	}

	if state.CouncilKey != "" {
		root, err := resolvedPath(a.WorktreeRoot)
		if err != nil {
			return err
		}
		releaseCouncil, err := a.lockScope(ctx, "council:"+state.CouncilKey, false)
		if err != nil {
			return err
		}
		statePath := councilStatePath(root, state.CouncilKey)
		cs, err := readCouncilState(statePath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			releaseCouncil()
			return err
		}
		if err == nil {
			cs.ActiveTasks = removeString(cs.ActiveTasks, state.TaskID)
			if state.Mode == checkoutModeWriter && !state.ReadOnly {
				if (justFinished || cs.ActiveWriterTaskID == state.TaskID) && state.Tip != "" {
					cs.WriterTip = state.Tip
				}
				if cs.ActiveWriterTaskID == state.TaskID {
					cs.ActiveWriterTaskID = ""
				}
			}
			if err := writeCouncilState(root, state.CouncilKey, cs); err != nil {
				releaseCouncil()
				return err
			}
		}
		releaseCouncil()
	}

	if err := removeFile(taskBase + ".log"); err != nil {
		return err
	}
	if state.Worker != nil {
		if err := os.RemoveAll(taskBase + ".run"); err != nil {
			return err
		}
	}
	a.running.Delete(taskBase)
	if state.CleanupError != "" {
		state.CleanupError = ""
		return writeWorktreeState(taskBase, state)
	}
	return nil
}

func readOnlyArtifacts(taskBase string, state *worktreeState) ([]scruntime.Artifact, error) {
	var artifacts []scruntime.Artifact
	log, err := os.ReadFile(taskBase + ".log")
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

// CleanupCouncil removes shared council worktrees once no tasks for the council remain active.
// The writer branch (if any) is preserved in Git for review and merge.
func (a *Adapter) CleanupCouncil(ctx context.Context, conclaveID string) error {
	if a.WorktreeRoot == "" || strings.TrimSpace(conclaveID) == "" {
		return errors.New("worktree root and conclave ID are required")
	}
	root, err := resolvedPath(a.WorktreeRoot)
	if err != nil {
		return err
	}
	key := councilKey(conclaveID)
	releaseCouncil, err := a.lockScope(ctx, "council:"+key, false)
	if err != nil {
		return err
	}
	defer releaseCouncil()

	statePath := councilStatePath(root, key)
	cs, err := readCouncilState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(cs.ActiveTasks) > 0 || cs.ActiveWriterTaskID != "" {
		return errors.New("council checkout still has active tasks")
	}

	changed := false
	if cs.RefWorktree != nil {
		if err := removeSharedWorktree(ctx, cs.CommonDir, cs.RefWorktree, false); err != nil {
			return err
		}
		cs.RefWorktree = nil
		changed = true
	}
	for commit, wt := range cs.ReviewWorktrees {
		if wt == nil {
			delete(cs.ReviewWorktrees, commit)
			continue
		}
		if err := removeSharedWorktree(ctx, cs.CommonDir, wt, false); err != nil {
			if changed {
				_ = writeCouncilState(root, key, cs)
			}
			return err
		}
		delete(cs.ReviewWorktrees, commit)
		changed = true
	}
	if cs.WriterWorktree != nil {
		wtState := &worktreeState{
			Mode:         checkoutModeWriter,
			CommonDir:    cs.CommonDir,
			GitDir:       cs.WriterWorktree.GitDir,
			GitFile:      cs.WriterWorktree.GitFile,
			Registration: cs.WriterWorktree.Registration,
			Branch:       cs.WriterBranch,
			Base:         cs.BaseCommit,
			Tip:          cs.WriterTip,
		}
		if wtState.Tip == "" {
			wtState.Tip = cs.BaseCommit
		}
		if err := removeWorktree(ctx, cs.WriterWorktree.Dir, wtState, cs.WriterTip != ""); err != nil {
			if changed {
				_ = writeCouncilState(root, key, cs)
			}
			return err
		}
		cs.WriterWorktree = nil
	}
	if err := os.RemoveAll(councilDirPath(root, key)); err != nil {
		return err
	}
	return removeFile(statePath)
}

func createDetachedWorktree(ctx context.Context, owner gitRepo, dir, commit string) (*sharedWorktree, error) {
	common := gitRepo{beforeRun: owner.beforeRun, dir: owner.gitDir, gitDir: owner.gitDir}
	if _, err := common.run(ctx, "worktree", "add", "--detach", "--no-checkout", "--end-of-options", dir, commit); err != nil {
		return nil, err
	}
	wt, err := registerSharedWorktree(ctx, common, dir, commit)
	if err != nil {
		cleanupWT := wt
		if cleanupWT == nil {
			cleanupWT = &sharedWorktree{Dir: dir}
		}
		_ = removeSharedWorktree(ctx, owner.gitDir, cleanupWT, false)
		return nil, err
	}
	return wt, nil
}

func createWriterWorktree(ctx context.Context, owner gitRepo, dir, branch, commit string) (*sharedWorktree, error) {
	if _, err := owner.run(ctx, "branch", "--no-track", "--end-of-options", branch, commit); err != nil {
		return nil, err
	}
	common := gitRepo{beforeRun: owner.beforeRun, dir: owner.gitDir, gitDir: owner.gitDir}
	if _, err := common.run(ctx, "worktree", "add", "--no-checkout", "--end-of-options", dir, branch); err != nil {
		_ = deleteBranchIfAt(ctx, owner.gitDir, branch, commit)
		return nil, err
	}
	wt, err := registerSharedWorktree(ctx, common, dir, commit)
	if err != nil {
		cleanupWT := wt
		if cleanupWT == nil {
			cleanupWT = &sharedWorktree{Dir: dir}
		}
		_ = removeWriterWorktreeAndBranch(ctx, owner.gitDir, branch, commit, cleanupWT)
		return nil, err
	}
	return wt, nil
}

func registerSharedWorktree(ctx context.Context, common gitRepo, dir, commit string) (*sharedWorktree, error) {
	gitDirOut, err := (gitRepo{dir: dir}).run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	gitDir := strings.TrimSuffix(gitDirOut, "\n")
	probe := &worktreeState{
		Mode:      checkoutModeRef,
		CommonDir: common.gitDir,
		GitDir:    gitDir,
	}
	gitFile, err := probe.readGitFile(dir)
	if err != nil {
		return nil, err
	}
	checkout, err := probe.checkoutGit(dir).withoutFilters(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := checkout.run(ctx, "read-tree", "--reset", "-u", "--end-of-options", commit); err != nil {
		return nil, err
	}
	reg := make(map[string]fileStamp)
	for _, path := range probe.registrationPaths(dir) {
		_, stamp, err := readMetadata(path)
		if err != nil {
			return nil, err
		}
		reg[path] = stamp
	}
	probe.Registration = reg
	if err := probe.validateRegistration(dir); err != nil {
		return nil, err
	}
	return &sharedWorktree{
		Dir:          dir,
		Commit:       commit,
		GitDir:       gitDir,
		GitFile:      gitFile,
		Registration: reg,
	}, nil
}

func validateDetachedWorktree(ctx context.Context, commonDir string, wt *sharedWorktree) error {
	if wt == nil || wt.GitFile == nil {
		return errors.New("council checkout has no recorded git identity")
	}
	s := &worktreeState{
		Mode:         checkoutModeRef,
		CommonDir:    commonDir,
		GitDir:       wt.GitDir,
		GitFile:      wt.GitFile,
		Registration: wt.Registration,
	}
	id, err := s.readGitFile(wt.Dir)
	if err != nil {
		return err
	}
	if *id != *wt.GitFile {
		return errors.New("council checkout .git entry was replaced")
	}
	s.beforeGit = func() error { return s.validateRegistration(wt.Dir) }
	checkout := s.checkoutGit(wt.Dir)
	checkout.readOnly = true
	common, err := checkout.run(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if strings.TrimSuffix(common, "\n") != commonDir {
		return errors.New("council checkout belongs to another repository")
	}
	if _, err := checkout.run(ctx, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		return errors.New("read-only council checkout is no longer detached")
	}
	head, err := checkout.commitID(ctx, "HEAD")
	if err != nil {
		return err
	}
	if head != wt.Commit {
		return errors.New("read-only council checkout HEAD moved")
	}
	return nil
}

func validateWriterWorktree(ctx context.Context, commonDir, branch string, wt *sharedWorktree) (string, error) {
	if wt == nil || wt.GitFile == nil {
		return "", errors.New("council writer checkout has no recorded git identity")
	}
	s := &worktreeState{
		Mode:         checkoutModeWriter,
		CommonDir:    commonDir,
		GitDir:       wt.GitDir,
		GitFile:      wt.GitFile,
		Registration: wt.Registration,
		Branch:       branch,
	}
	if err := s.validateCheckout(ctx, wt.Dir, true); err != nil {
		return "", err
	}
	return s.checkoutGit(wt.Dir).commitID(ctx, "HEAD")
}

func removeSharedWorktree(ctx context.Context, commonDir string, wt *sharedWorktree, verifySaved bool) error {
	if wt == nil {
		return nil
	}
	_, statErr := os.Lstat(wt.Dir)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	common := gitRepo{dir: commonDir, gitDir: commonDir}
	list, err := common.run(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return err
	}
	for _, field := range strings.Split(list, "\x00") {
		if field == "worktree "+wt.Dir {
			if verifySaved && statErr == nil {
				s := &worktreeState{
					Mode:         checkoutModeRef,
					CommonDir:    commonDir,
					GitDir:       wt.GitDir,
					GitFile:      wt.GitFile,
					Registration: wt.Registration,
					Base:         wt.Commit,
					Tip:          wt.Commit,
				}
				if err := verifySavedWorktree(ctx, wt.Dir, s); err != nil {
					return err
				}
			}
			if _, err := common.run(ctx, "worktree", "remove", "--force", "--", wt.Dir); err != nil {
				return err
			}
			statErr = os.ErrNotExist
			break
		}
	}
	if statErr == nil {
		return errors.New("council worktree path is no longer registered with Git")
	}
	return nil
}

func removeWriterWorktreeAndBranch(ctx context.Context, commonDir, branch, baseCommit string, wt *sharedWorktree) error {
	var err error
	if wt != nil {
		err = removeSharedWorktree(ctx, commonDir, wt, false)
	}
	return errors.Join(err, deleteBranchIfAt(ctx, commonDir, branch, baseCommit))
}

func deleteBranchIfAt(ctx context.Context, commonDir, branch, expectedCommit string) error {
	if branch == "" || !isCommitID(expectedCommit) {
		return nil
	}
	common := gitRepo{dir: commonDir, gitDir: commonDir}
	ref := "refs/heads/" + branch
	tip, err := common.run(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", ref)
	if err != nil {
		if gitExitCode(err, 1) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(tip) == expectedCommit {
		_, err = common.run(ctx, "update-ref", "--no-deref", "-d", "--end-of-options", ref, expectedCommit)
		return err
	}
	return nil
}

func removeString(items []string, target string) []string {
	out := items[:0]
	for _, item := range items {
		if item != target {
			out = append(out, item)
		}
	}
	return out
}
