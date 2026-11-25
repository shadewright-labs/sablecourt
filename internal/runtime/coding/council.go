package coding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

const (
	councilsDirName            = ".councils"
	defaultCouncilSetupTimeout = 30 * time.Second
	writerLockFileName         = "writer.lock"
	journalPrefix              = "pending-"
	journalSuffix              = ".json"
)

type sharedWorktree struct {
	Dir          string
	Commit       string
	GitDir       string
	GitFile      *gitFileIdentity
	Registration map[string]fileStamp
}

type isolatedCheckout struct {
	Dir       string
	Commit    string
	GitDir    string
	HeadStamp fileStamp
}

type councilState struct {
	ConclaveID         string
	Key                string
	Mode               string
	Repo               string
	CommonDir          string
	BaseRef            string
	BaseCommit         string
	CreatorTaskID      string            `json:",omitempty"`
	RetainPin          bool              `json:",omitempty"`
	RefSetupTaskID     string            `json:",omitempty"`
	RefWorktree        *sharedWorktree   `json:",omitempty"`
	WriterRole         string            `json:",omitempty"`
	WriterBranch       string            `json:",omitempty"`
	WriterTip          string            `json:",omitempty"`
	WriterDirty        bool              `json:",omitempty"`
	WriterWorktree     *sharedWorktree   `json:",omitempty"`
	ReviewSetupTaskID  string            `json:",omitempty"`
	ReviewCheckout     *isolatedCheckout `json:",omitempty"`
	ActiveWriterTaskID string            `json:",omitempty"`
	ActiveTasks        []string          `json:",omitempty"`
}

type writerLockRecord struct {
	TaskID     string    `json:"task_id"`
	WriterRole string    `json:"writer_role"`
	CreatedAt  time.Time `json:"created_at"`
}

type councilStartJournal struct {
	TaskID            string    `json:"task_id"`
	TaskBase          string    `json:"task_base"`
	CommonDir         string    `json:"common_dir,omitempty"`
	Mode              string    `json:"mode,omitempty"`
	IsWriter          bool      `json:"is_writer,omitempty"`
	WriterBranch      string    `json:"writer_branch,omitempty"`
	BaseCommit        string    `json:"base_commit,omitempty"`
	CreatedCouncilDir bool      `json:"created_council_dir,omitempty"`
	CreatedRefWT      bool      `json:"created_ref_wt,omitempty"`
	CreatedWriterWT   bool      `json:"created_writer_wt,omitempty"`
	CreatedReviewDir  bool      `json:"created_review_dir,omitempty"`
	CreatedWriterLock bool      `json:"created_writer_lock,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

type startJournalHandle struct {
	path   string
	file   *os.File
	rec    councilStartJournal
	closed bool
}

func (a *Adapter) councilSetupTimeout() time.Duration {
	if a != nil && a.setupTimeout > 0 {
		return a.setupTimeout
	}
	return defaultCouncilSetupTimeout
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

func writerLockPath(root, key string) string {
	return filepath.Join(councilDirPath(root, key), writerLockFileName)
}

func councilJournalPath(root, key, taskID string) string {
	return filepath.Join(councilDirPath(root, key), journalPrefix+sanitizeSegment(taskID)+journalSuffix)
}

func (a *Adapter) lockCouncil(ctx context.Context, root, key string) (func(), error) {
	relMem, err := a.lockScope(ctx, "council:"+key, false)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, councilsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		relMem()
		return nil, err
	}
	lockPath := filepath.Join(dir, key+".lock")
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			relMem()
			return nil, err
		}
		for {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
				_ = f.Close()
				relMem()
				return nil, err
			}
			select {
			case <-ctx.Done():
				_ = f.Close()
				relMem()
				return nil, ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
		fi, err1 := f.Stat()
		pi, err2 := os.Stat(lockPath)
		if err1 == nil && err2 == nil && os.SameFile(fi, pi) {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
				relMem()
			}, nil
		}
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

func acquireWriterLock(root, key, taskID, writerRole string) error {
	path := writerLockPath(root, key)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("council writer worktree is already active")
		}
		return err
	}
	encErr := json.NewEncoder(f).Encode(writerLockRecord{
		TaskID:     taskID,
		WriterRole: writerRole,
		CreatedAt:  time.Now().UTC(),
	})
	closeErr := f.Close()
	if err := errors.Join(encErr, closeErr); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func releaseWriterLock(root, key, taskID string) error {
	path := writerLockPath(root, key)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var rec writerLockRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.TaskID != taskID {
		return nil
	}
	return removeFile(path)
}

func openStartJournal(root, key string, rec councilStartJournal) (*startJournalHandle, error) {
	path := councilJournalPath(root, key, rec.TaskID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	rec.CreatedAt = time.Now().UTC()
	h := &startJournalHandle{path: path, file: f, rec: rec}
	if err := h.sync(); err != nil {
		_ = h.discard()
		return nil, err
	}
	return h, nil
}

func (h *startJournalHandle) sync() error {
	if h == nil || h.closed {
		return nil
	}
	raw, err := json.Marshal(h.rec)
	if err != nil {
		return err
	}
	if _, err := h.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := h.file.Truncate(0); err != nil {
		return err
	}
	if _, err := h.file.Write(raw); err != nil {
		return err
	}
	return h.file.Sync()
}

func (h *startJournalHandle) discard() error {
	if h == nil || h.closed {
		return nil
	}
	h.closed = true
	rmErr := removeFile(h.path)
	unlockErr := syscall.Flock(int(h.file.Fd()), syscall.LOCK_UN)
	closeErr := h.file.Close()
	return errors.Join(rmErr, unlockErr, closeErr)
}

func reconcileCouncilJournals(ctx context.Context, root, key string) error {
	councilDir := councilDirPath(root, key)
	entries, err := os.ReadDir(councilDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	statePath := councilStatePath(root, key)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, journalPrefix) || !strings.HasSuffix(name, journalSuffix) {
			continue
		}
		jPath := filepath.Join(councilDir, name)
		f, err := os.OpenFile(jPath, os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr != nil {
			_ = f.Close()
			if errors.Is(lockErr, syscall.EWOULDBLOCK) || errors.Is(lockErr, syscall.EAGAIN) {
				// Another launch actively holds this start journal.
				continue
			}
			return lockErr
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, 64*1024))
		if readErr != nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			return readErr
		}
		var rec councilStartJournal
		if err := json.Unmarshal(raw, &rec); err != nil || rec.TaskID == "" {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			return fmt.Errorf("invalid council start journal %q", name)
		}

		// Determine if this task actually launched or was interrupted before launch completed.
		incomplete := false
		if rec.TaskBase != "" {
			st, stErr := readWorktreeState(rec.TaskBase + stateSuffix)
			switch {
			case errors.Is(stErr, os.ErrNotExist):
				incomplete = true
			case stErr != nil:
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
				return stErr
			case st.Worker == nil:
				if _, runErr := os.Lstat(rec.TaskBase + ".run"); errors.Is(runErr, os.ErrNotExist) {
					incomplete = true
					_ = removeFile(rec.TaskBase + stateSuffix)
					_ = removeFile(rec.TaskBase + ".log")
				}
			}
		} else {
			incomplete = true
		}

		if incomplete {
			if rec.CreatedWriterLock {
				_ = releaseWriterLock(root, key, rec.TaskID)
			}
			cs, csErr := readCouncilState(statePath)
			if csErr != nil && !errors.Is(csErr, os.ErrNotExist) {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
				return csErr
			}
			if csErr == nil {
				cs.ActiveTasks = removeString(cs.ActiveTasks, rec.TaskID)
				if rec.IsWriter && cs.ActiveWriterTaskID == rec.TaskID {
					cs.ActiveWriterTaskID = ""
				}
				if cs.RefSetupTaskID == rec.TaskID {
					cs.RefSetupTaskID = ""
					if rec.CreatedRefWT && cs.RefWorktree == nil {
						_ = removeOrphanedWorktree(ctx, cs.CommonDir, filepath.Join(councilDir, "ref"))
					}
				}
				if cs.ReviewSetupTaskID == rec.TaskID {
					cs.ReviewSetupTaskID = ""
					if rec.CreatedReviewDir && cs.ReviewCheckout == nil {
						_ = os.RemoveAll(filepath.Join(councilDir, "review"))
					}
				}
				if rec.CreatedWriterWT && cs.WriterWorktree == nil {
					_ = removeOrphanedWorktree(ctx, cs.CommonDir, filepath.Join(councilDir, "writer"))
					_ = deleteBranchIfAt(ctx, cs.CommonDir, cs.WriterBranch, cs.BaseCommit)
				}
				if rec.CreatedCouncilDir && !cs.RetainPin && len(cs.ActiveTasks) == 0 &&
					cs.RefWorktree == nil && cs.WriterWorktree == nil && cs.ReviewCheckout == nil {
					_ = removeFile(statePath)
				} else if err := writeCouncilState(root, key, cs); err != nil {
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
					_ = f.Close()
					return err
				}
			} else {
				if rec.CreatedRefWT && rec.CommonDir != "" {
					_ = removeOrphanedWorktree(ctx, rec.CommonDir, filepath.Join(councilDir, "ref"))
				}
				if rec.CreatedWriterWT && rec.CommonDir != "" {
					_ = removeOrphanedWorktree(ctx, rec.CommonDir, filepath.Join(councilDir, "writer"))
					_ = deleteBranchIfAt(ctx, rec.CommonDir, rec.WriterBranch, rec.BaseCommit)
				}
				if rec.CreatedReviewDir {
					_ = os.RemoveAll(filepath.Join(councilDir, "review"))
				}
			}
		}

		_ = removeFile(jPath)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()

		if incomplete && rec.CreatedCouncilDir {
			if _, err := os.Lstat(statePath); errors.Is(err, os.ErrNotExist) {
				_ = os.Remove(councilDir)
			}
		}
	}
	return nil
}

func removeOrphanedWorktree(ctx context.Context, commonDir, wtDir string) error {
	if commonDir == "" || wtDir == "" {
		return nil
	}
	common := gitRepo{dir: commonDir, gitDir: commonDir}
	list, err := common.run(ctx, "worktree", "list", "--porcelain", "-z")
	if err == nil {
		for _, field := range strings.Split(list, "\x00") {
			if field == "worktree "+wtDir {
				_, _ = common.run(ctx, "worktree", "remove", "--force", "--", wtDir)
				return nil
			}
		}
	}
	return os.RemoveAll(wtDir)
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

func resolveCheckoutRef(ctx context.Context, owner gitRepo, ref string) (string, error) {
	if !validCheckoutRef(ref) {
		return "", errors.New("invalid coding checkoutRef")
	}
	if isCommitID(ref) {
		id, err := owner.commitID(ctx, ref)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(id, ref) {
			return "", errors.New("checkout commit ID mismatch")
		}
		return strings.ToLower(id), nil
	}
	if ref == "HEAD" {
		return owner.commitID(ctx, "HEAD")
	}
	if strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/") {
		if _, err := owner.run(ctx, "show-ref", "--verify", "--quiet", "--", ref); err != nil {
			return "", fmt.Errorf("checkout ref %q not found: %w", ref, err)
		}
		return owner.commitID(ctx, ref)
	}
	headRef := "refs/heads/" + ref
	tagRef := "refs/tags/" + ref
	var headID, tagID string
	if _, err := owner.run(ctx, "show-ref", "--verify", "--quiet", "--", headRef); err == nil {
		headID, err = owner.commitID(ctx, headRef)
		if err != nil {
			return "", err
		}
	}
	if _, err := owner.run(ctx, "show-ref", "--verify", "--quiet", "--", tagRef); err == nil {
		tagID, err = owner.commitID(ctx, tagRef)
		if err != nil {
			return "", err
		}
	}
	switch {
	case headID != "" && tagID != "" && headID != tagID:
		return "", fmt.Errorf("checkout ref %q is ambiguous between branch and tag", ref)
	case headID != "":
		return headID, nil
	case tagID != "":
		return tagID, nil
	default:
		return "", fmt.Errorf("checkout ref %q is not a branch, tag, or full commit ID", ref)
	}
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
	for _, path := range []string{base, base + stateSuffix, base + ".log", base + ".run", base + ".git", base + excludeConfigSuffix} {
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
	setupCtx, cancel := context.WithTimeout(ctx, a.councilSetupTimeout())
	defer cancel()

	_, taskBase, err := a.reserveTaskBase(taskID)
	if err != nil {
		return "", "", nil, nil, err
	}
	resolvedRepo, owner, err := a.verifyPrimaryRepo(setupCtx, repo, true)
	if err != nil {
		return "", "", nil, nil, err
	}
	head, err := owner.commitID(setupCtx, "HEAD")
	if err != nil {
		return "", "", nil, nil, err
	}
	liveGitDir := taskBase + ".git"
	if err := createLiveGitDir(setupCtx, owner, liveGitDir, head); err != nil {
		return "", "", nil, nil, err
	}
	state := &worktreeState{
		TaskID:      taskID,
		Mode:        checkoutModeLive,
		ReadOnly:    true,
		CheckoutDir: resolvedRepo,
		Repo:        resolvedRepo,
		CommonDir:   liveGitDir,
		GitDir:      liveGitDir,
		Base:        head,
	}
	if err := writeWorktreeState(taskBase, state); err != nil {
		_ = os.RemoveAll(liveGitDir)
		return "", "", nil, nil, err
	}
	rollback := func(_ context.Context) error {
		return errors.Join(
			removeFile(taskBase+stateSuffix),
			removeFile(taskBase+".log"),
			os.RemoveAll(taskBase+".run"),
			os.RemoveAll(liveGitDir),
		)
	}
	return resolvedRepo, taskBase, state, rollback, nil
}

func (a *Adapter) prepareRefCheckout(ctx context.Context, repo, taskID string, input launchInput) (string, string, *worktreeState, func(context.Context) error, func() error, error) {
	if repo == "" {
		return "", "", nil, nil, nil, errors.New("worktree root, repository and task ID are required")
	}
	setupCtx, cancel := context.WithTimeout(ctx, a.councilSetupTimeout())
	defer cancel()

	root, taskBase, err := a.reserveTaskBase(taskID)
	if err != nil {
		return "", "", nil, nil, nil, err
	}
	resolvedRepo, owner, err := a.verifyPrimaryRepo(setupCtx, repo, false)
	if err != nil {
		return "", "", nil, nil, nil, err
	}

	key := councilKey(input.conclaveID)
	statePath := councilStatePath(root, key)
	councilDir := councilDirPath(root, key)
	wtDir := filepath.Join(councilDir, "ref")

	// Resolve ref outside the council lock so delayed Git commands never hold the lock.
	resolvedCommit, resolveErr := resolveCheckoutRef(setupCtx, owner, input.checkoutRef)

	var (
		createdCouncilDir bool
		createdRefWT      bool
		needCreateRef     bool
		journal           *startJournalHandle
		commit            string
		commonDir         string
		existingWT        *sharedWorktree
	)

	// Short lock loop for reservation only.
	for {
		releaseCouncil, err := a.lockCouncil(setupCtx, root, key)
		if err != nil {
			return "", "", nil, nil, nil, err
		}
		if err := reconcileCouncilJournals(setupCtx, root, key); err != nil {
			releaseCouncil()
			return "", "", nil, nil, nil, err
		}
		existing, readErr := readCouncilState(statePath)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			releaseCouncil()
			return "", "", nil, nil, nil, readErr
		}
		if errors.Is(readErr, os.ErrNotExist) {
			if resolveErr != nil {
				releaseCouncil()
				return "", "", nil, nil, nil, fmt.Errorf("resolve checkout ref: %w", resolveErr)
			}
			if err := os.MkdirAll(filepath.Join(root, councilsDirName), 0o700); err != nil {
				releaseCouncil()
				return "", "", nil, nil, nil, err
			}
			if err := os.Mkdir(councilDir, 0o700); err != nil {
				releaseCouncil()
				return "", "", nil, nil, nil, fmt.Errorf("create council directory: %w", err)
			}
			createdCouncilDir = true
			journal, err = openStartJournal(root, key, councilStartJournal{
				TaskID:            taskID,
				TaskBase:          taskBase,
				CommonDir:         owner.gitDir,
				Mode:              checkoutModeRef,
				BaseCommit:        resolvedCommit,
				CreatedCouncilDir: true,
				CreatedRefWT:      true,
			})
			if err != nil {
				_ = os.Remove(councilDir)
				releaseCouncil()
				return "", "", nil, nil, nil, err
			}
			cs := &councilState{
				ConclaveID:     input.conclaveID,
				Key:            key,
				Mode:           checkoutModeRef,
				Repo:           resolvedRepo,
				CommonDir:      owner.gitDir,
				BaseRef:        input.checkoutRef,
				BaseCommit:     resolvedCommit,
				CreatorTaskID:  taskID,
				RefSetupTaskID: taskID,
				ActiveTasks:    []string{taskID},
			}
			if err := writeCouncilState(root, key, cs); err != nil {
				_ = journal.discard()
				_ = os.Remove(councilDir)
				releaseCouncil()
				return "", "", nil, nil, nil, err
			}
			needCreateRef = true
			commit = resolvedCommit
			commonDir = owner.gitDir
			releaseCouncil()
			break
		}

		// Existing council state.
		if existing.Mode != checkoutModeRef || existing.Repo != resolvedRepo || existing.ConclaveID != input.conclaveID || existing.BaseRef != input.checkoutRef {
			releaseCouncil()
			return "", "", nil, nil, nil, errors.New("council checkout parameters do not match existing council")
		}
		if existing.RefWorktree == nil && existing.RefSetupTaskID != "" {
			// Another participant is setting up the ref worktree outside the lock; wait briefly without holding lock.
			releaseCouncil()
			select {
			case <-setupCtx.Done():
				return "", "", nil, nil, nil, setupCtx.Err()
			case <-time.After(15 * time.Millisecond):
			}
			continue
		}
		jRec := councilStartJournal{
			TaskID:     taskID,
			TaskBase:   taskBase,
			CommonDir:  existing.CommonDir,
			Mode:       checkoutModeRef,
			BaseCommit: existing.BaseCommit,
		}
		if existing.RefWorktree == nil {
			needCreateRef = true
			jRec.CreatedRefWT = true
			existing.RefSetupTaskID = taskID
		}
		journal, err = openStartJournal(root, key, jRec)
		if err != nil {
			releaseCouncil()
			return "", "", nil, nil, nil, err
		}
		if existing.CreatorTaskID != taskID {
			existing.RetainPin = true
		}
		if !slices.Contains(existing.ActiveTasks, taskID) {
			existing.ActiveTasks = append(existing.ActiveTasks, taskID)
		}
		if err := writeCouncilState(root, key, existing); err != nil {
			_ = journal.discard()
			releaseCouncil()
			return "", "", nil, nil, nil, err
		}
		commit = existing.BaseCommit
		commonDir = existing.CommonDir
		existingWT = existing.RefWorktree
		releaseCouncil()
		break
	}

	rollback := func(rbCtx context.Context) error {
		var rbErr error
		rel, lockErr := a.lockCouncil(rbCtx, root, key)
		if lockErr == nil {
			if cur, err := readCouncilState(statePath); err == nil {
				cur.ActiveTasks = removeString(cur.ActiveTasks, taskID)
				if cur.RefSetupTaskID == taskID {
					cur.RefSetupTaskID = ""
				}
				if createdRefWT && !cur.RetainPin && len(cur.ActiveTasks) == 0 && cur.RefWorktree != nil {
					rbErr = errors.Join(rbErr, removeSharedWorktree(rbCtx, cur.CommonDir, cur.RefWorktree, false))
					cur.RefWorktree = nil
				}
				if createdCouncilDir && !cur.RetainPin && len(cur.ActiveTasks) == 0 && cur.RefWorktree == nil {
					rbErr = errors.Join(rbErr, removeFile(statePath))
				} else {
					rbErr = errors.Join(rbErr, writeCouncilState(root, key, cur))
				}
			}
			rbErr = errors.Join(rbErr, journal.discard())
			if createdCouncilDir {
				if _, err := os.Lstat(statePath); errors.Is(err, os.ErrNotExist) {
					_ = os.Remove(councilDir)
				}
			}
			rel()
		} else {
			rbErr = errors.Join(rbErr, lockErr, journal.discard())
		}
		return errors.Join(rbErr,
			removeFile(taskBase+stateSuffix),
			removeFile(taskBase+".log"),
			os.RemoveAll(taskBase+".run"),
		)
	}

	// Run Git setup/validation outside the council lock, bounded by setupCtx.
	var wt *sharedWorktree
	if needCreateRef {
		if _, err := os.Lstat(wtDir); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				err = os.ErrExist
			}
			rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
			defer rbCancel()
			return "", "", nil, nil, nil, errors.Join(fmt.Errorf("council ref path unavailable: %w", err), rollback(rbCtx))
		}
		created, err := createDetachedWorktree(setupCtx, owner, wtDir, commit)
		if err != nil {
			rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
			defer rbCancel()
			return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
		}
		createdRefWT = true
		rel, err := a.lockCouncil(setupCtx, root, key)
		if err != nil {
			rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
			defer rbCancel()
			_ = removeSharedWorktree(rbCtx, commonDir, created, false)
			createdRefWT = false
			return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
		}
		cur, readErr := readCouncilState(statePath)
		if readErr != nil {
			rel()
			rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
			defer rbCancel()
			_ = removeSharedWorktree(rbCtx, commonDir, created, false)
			createdRefWT = false
			return "", "", nil, nil, nil, errors.Join(readErr, rollback(rbCtx))
		}
		cur.RefWorktree = created
		if cur.RefSetupTaskID == taskID {
			cur.RefSetupTaskID = ""
		}
		if err := writeCouncilState(root, key, cur); err != nil {
			rel()
			rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
			defer rbCancel()
			_ = removeSharedWorktree(rbCtx, commonDir, created, false)
			createdRefWT = false
			return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
		}
		rel()
		wt = created
	} else {
		if err := validateDetachedWorktree(setupCtx, commonDir, existingWT); err != nil {
			rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
			defer rbCancel()
			return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
		}
		wt = existingWT
	}

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
	if a.beforeTaskState != nil {
		a.beforeTaskState(taskID)
	}
	if err := writeWorktreeState(taskBase, state); err != nil {
		rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
		defer rbCancel()
		return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
	}
	return wt.Dir, taskBase, state, rollback, journal.discard, nil
}

func (a *Adapter) prepareWriterCheckout(ctx context.Context, repo, taskID string, input launchInput) (string, string, *worktreeState, func(context.Context) error, func() error, error) {
	if repo == "" {
		return "", "", nil, nil, nil, errors.New("worktree root, repository and task ID are required")
	}
	setupCtx, cancel := context.WithTimeout(ctx, a.councilSetupTimeout())
	defer cancel()

	root, taskBase, err := a.reserveTaskBase(taskID)
	if err != nil {
		return "", "", nil, nil, nil, err
	}
	resolvedRepo, owner, err := a.verifyPrimaryRepo(setupCtx, repo, false)
	if err != nil {
		return "", "", nil, nil, nil, err
	}

	isWriter := input.role == input.checkoutWriter
	key := councilKey(input.conclaveID)
	statePath := councilStatePath(root, key)
	councilDir := councilDirPath(root, key)
	writerWTDir := filepath.Join(councilDir, "writer")
	reviewDir := filepath.Join(councilDir, "review")

	ref := input.checkoutRef
	if ref == "" {
		ref = "HEAD"
	}
	resolvedBaseCommit, resolveErr := resolveCheckoutRef(setupCtx, owner, ref)

	var (
		createdCouncilDir bool
		createdWriterLock bool
		createdWriterWT   bool
		createdReviewDir  bool
		needCreateWriter  bool
		needCreateReview  bool
		journal           *startJournalHandle
		baseCommit        string
		commonDir         string
		writerBranch      string
		writerTip         string
		existingWriterWT  *sharedWorktree
		existingReview    *isolatedCheckout
	)

	// Short lock loop for reservation only.
	for {
		releaseCouncil, err := a.lockCouncil(setupCtx, root, key)
		if err != nil {
			return "", "", nil, nil, nil, err
		}
		if err := reconcileCouncilJournals(setupCtx, root, key); err != nil {
			releaseCouncil()
			return "", "", nil, nil, nil, err
		}
		existing, readErr := readCouncilState(statePath)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			releaseCouncil()
			return "", "", nil, nil, nil, readErr
		}

		var cs *councilState
		localCreatedCouncilDir := false
		if errors.Is(readErr, os.ErrNotExist) {
			if resolveErr != nil {
				releaseCouncil()
				return "", "", nil, nil, nil, fmt.Errorf("resolve checkout ref: %w", resolveErr)
			}
			if err := os.MkdirAll(filepath.Join(root, councilsDirName), 0o700); err != nil {
				releaseCouncil()
				return "", "", nil, nil, nil, err
			}
			if err := os.Mkdir(councilDir, 0o700); err != nil {
				releaseCouncil()
				return "", "", nil, nil, nil, fmt.Errorf("create council directory: %w", err)
			}
			localCreatedCouncilDir = true
			cs = &councilState{
				ConclaveID:    input.conclaveID,
				Key:           key,
				Mode:          checkoutModeWriter,
				Repo:          resolvedRepo,
				CommonDir:     owner.gitDir,
				BaseRef:       input.checkoutRef,
				BaseCommit:    resolvedBaseCommit,
				CreatorTaskID: taskID,
				WriterRole:    input.checkoutWriter,
				WriterBranch:  councilWriterBranch(key),
			}
		} else {
			cs = existing
			if cs.Mode != checkoutModeWriter || cs.Repo != resolvedRepo || cs.ConclaveID != input.conclaveID {
				releaseCouncil()
				return "", "", nil, nil, nil, errors.New("council checkout parameters do not match existing council")
			}
			if cs.WriterRole != input.checkoutWriter {
				releaseCouncil()
				return "", "", nil, nil, nil, errors.New("council already has a different writer role")
			}
			if input.checkoutRef != "" && input.checkoutRef != cs.BaseRef {
				releaseCouncil()
				return "", "", nil, nil, nil, errors.New("council checkout ref does not match existing council")
			}
			if !isWriter && cs.ReviewCheckout == nil && cs.ReviewSetupTaskID != "" {
				releaseCouncil()
				select {
				case <-setupCtx.Done():
					return "", "", nil, nil, nil, setupCtx.Err()
				case <-time.After(15 * time.Millisecond):
				}
				continue
			}
		}

		jRec := councilStartJournal{
			TaskID:            taskID,
			TaskBase:          taskBase,
			CommonDir:         cs.CommonDir,
			Mode:              checkoutModeWriter,
			IsWriter:          isWriter,
			WriterBranch:      cs.WriterBranch,
			BaseCommit:        cs.BaseCommit,
			CreatedCouncilDir: localCreatedCouncilDir,
		}
		journal, err = openStartJournal(root, key, jRec)
		if err != nil {
			if localCreatedCouncilDir {
				_ = os.Remove(councilDir)
			}
			releaseCouncil()
			return "", "", nil, nil, nil, err
		}

		if isWriter {
			if cs.ActiveWriterTaskID != "" {
				_ = journal.discard()
				if localCreatedCouncilDir {
					_ = os.Remove(councilDir)
				}
				releaseCouncil()
				return "", "", nil, nil, nil, errors.New("council writer worktree is already active")
			}
			if err := acquireWriterLock(root, key, taskID, input.checkoutWriter); err != nil {
				_ = journal.discard()
				if localCreatedCouncilDir {
					_ = os.Remove(councilDir)
				}
				releaseCouncil()
				return "", "", nil, nil, nil, err
			}
			createdWriterLock = true
			journal.rec.CreatedWriterLock = true
			cs.ActiveWriterTaskID = taskID
			if cs.WriterWorktree == nil {
				needCreateWriter = true
				journal.rec.CreatedWriterWT = true
			}
		} else {
			if cs.ReviewCheckout == nil {
				needCreateReview = true
				journal.rec.CreatedReviewDir = true
				cs.ReviewSetupTaskID = taskID
			}
		}

		if err := journal.sync(); err != nil {
			if createdWriterLock {
				_ = releaseWriterLock(root, key, taskID)
			}
			_ = journal.discard()
			if localCreatedCouncilDir {
				_ = os.Remove(councilDir)
			}
			releaseCouncil()
			return "", "", nil, nil, nil, err
		}

		if !localCreatedCouncilDir && cs.CreatorTaskID != taskID {
			cs.RetainPin = true
		}
		if !slices.Contains(cs.ActiveTasks, taskID) {
			cs.ActiveTasks = append(cs.ActiveTasks, taskID)
		}
		if err := writeCouncilState(root, key, cs); err != nil {
			if createdWriterLock {
				_ = releaseWriterLock(root, key, taskID)
			}
			_ = journal.discard()
			if localCreatedCouncilDir {
				_ = os.Remove(councilDir)
			}
			releaseCouncil()
			return "", "", nil, nil, nil, err
		}
		createdCouncilDir = localCreatedCouncilDir
		baseCommit = cs.BaseCommit
		commonDir = cs.CommonDir
		writerBranch = cs.WriterBranch
		writerTip = cs.WriterTip
		existingWriterWT = cs.WriterWorktree
		existingReview = cs.ReviewCheckout
		releaseCouncil()
		break
	}

	rollback := func(rbCtx context.Context) error {
		var rbErr error
		rel, lockErr := a.lockCouncil(rbCtx, root, key)
		if lockErr == nil {
			if cur, err := readCouncilState(statePath); err == nil {
				cur.ActiveTasks = removeString(cur.ActiveTasks, taskID)
				if isWriter && cur.ActiveWriterTaskID == taskID {
					cur.ActiveWriterTaskID = ""
				}
				if cur.ReviewSetupTaskID == taskID {
					cur.ReviewSetupTaskID = ""
				}
				if createdWriterWT && cur.WriterTip == "" && cur.WriterWorktree != nil {
					rbErr = errors.Join(rbErr, removeWriterWorktreeAndBranch(rbCtx, cur.CommonDir, cur.WriterBranch, cur.BaseCommit, cur.WriterWorktree))
					cur.WriterWorktree = nil
				}
				if createdReviewDir && !cur.RetainPin && len(cur.ActiveTasks) == 0 && cur.ReviewCheckout != nil {
					rbErr = errors.Join(rbErr, os.RemoveAll(cur.ReviewCheckout.Dir))
					cur.ReviewCheckout = nil
				}
				if createdCouncilDir && !cur.RetainPin && len(cur.ActiveTasks) == 0 && cur.WriterWorktree == nil && cur.ReviewCheckout == nil {
					rbErr = errors.Join(rbErr, removeFile(statePath))
				} else {
					rbErr = errors.Join(rbErr, writeCouncilState(root, key, cur))
				}
			}
			if createdWriterLock {
				rbErr = errors.Join(rbErr, releaseWriterLock(root, key, taskID))
			}
			rbErr = errors.Join(rbErr, journal.discard())
			if createdCouncilDir {
				if _, err := os.Lstat(statePath); errors.Is(err, os.ErrNotExist) {
					_ = os.Remove(councilDir)
				}
			}
			rel()
		} else {
			if createdWriterLock {
				rbErr = errors.Join(rbErr, releaseWriterLock(root, key, taskID))
			}
			rbErr = errors.Join(rbErr, lockErr, journal.discard())
		}
		return errors.Join(rbErr,
			removeFile(taskBase+stateSuffix),
			removeFile(taskBase+".log"),
			os.RemoveAll(taskBase+".run"),
		)
	}

	// Perform Git setup/validation outside the council lock, bounded by setupCtx.
	var (
		checkoutDir string
		state       *worktreeState
	)
	if isWriter {
		var (
			wt       *sharedWorktree
			turnBase string
		)
		if needCreateWriter {
			if _, err := os.Lstat(writerWTDir); !errors.Is(err, os.ErrNotExist) {
				if err == nil {
					err = os.ErrExist
				}
				rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
				defer rbCancel()
				return "", "", nil, nil, nil, errors.Join(fmt.Errorf("council writer path unavailable: %w", err), rollback(rbCtx))
			}
			created, err := createWriterWorktree(setupCtx, owner, writerWTDir, writerBranch, baseCommit)
			if err != nil {
				rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
				defer rbCancel()
				return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
			}
			createdWriterWT = true
			rel, err := a.lockCouncil(setupCtx, root, key)
			if err != nil {
				rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
				defer rbCancel()
				_ = removeWriterWorktreeAndBranch(rbCtx, commonDir, writerBranch, baseCommit, created)
				createdWriterWT = false
				return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
			}
			cur, readErr := readCouncilState(statePath)
			if readErr != nil {
				rel()
				rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
				defer rbCancel()
				_ = removeWriterWorktreeAndBranch(rbCtx, commonDir, writerBranch, baseCommit, created)
				createdWriterWT = false
				return "", "", nil, nil, nil, errors.Join(readErr, rollback(rbCtx))
			}
			cur.WriterWorktree = created
			if err := writeCouncilState(root, key, cur); err != nil {
				rel()
				rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
				defer rbCancel()
				_ = removeWriterWorktreeAndBranch(rbCtx, commonDir, writerBranch, baseCommit, created)
				createdWriterWT = false
				return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
			}
			rel()
			wt = created
			turnBase = baseCommit
		} else {
			head, err := validateWriterWorktree(setupCtx, commonDir, writerBranch, existingWriterWT)
			if err != nil {
				rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
				defer rbCancel()
				return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
			}
			wt = existingWriterWT
			if writerTip != "" {
				turnBase = writerTip
			} else {
				turnBase = baseCommit
			}
			if head != "" && writerTip == head {
				turnBase = head
			}
		}
		checkoutDir = wt.Dir
		state = &worktreeState{
			TaskID:       taskID,
			Mode:         checkoutModeWriter,
			ReadOnly:     false,
			CouncilKey:   key,
			CheckoutDir:  wt.Dir,
			Repo:         resolvedRepo,
			CommonDir:    commonDir,
			GitDir:       wt.GitDir,
			GitFile:      wt.GitFile,
			Registration: wt.Registration,
			Branch:       writerBranch,
			Base:         turnBase,
		}
	} else {
		// Reviewers always stay on the pinned base commit in an isolated standalone checkout.
		var rc *isolatedCheckout
		if needCreateReview {
			created, err := createIsolatedReviewCheckout(setupCtx, owner, reviewDir, baseCommit)
			if err != nil {
				rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
				defer rbCancel()
				return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
			}
			createdReviewDir = true
			rel, err := a.lockCouncil(setupCtx, root, key)
			if err != nil {
				rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
				defer rbCancel()
				_ = os.RemoveAll(created.Dir)
				createdReviewDir = false
				return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
			}
			cur, readErr := readCouncilState(statePath)
			if readErr != nil {
				rel()
				rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
				defer rbCancel()
				_ = os.RemoveAll(created.Dir)
				createdReviewDir = false
				return "", "", nil, nil, nil, errors.Join(readErr, rollback(rbCtx))
			}
			cur.ReviewCheckout = created
			if cur.ReviewSetupTaskID == taskID {
				cur.ReviewSetupTaskID = ""
			}
			if err := writeCouncilState(root, key, cur); err != nil {
				rel()
				rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
				defer rbCancel()
				_ = os.RemoveAll(created.Dir)
				createdReviewDir = false
				return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
			}
			rel()
			rc = created
		} else {
			if err := validateIsolatedReviewCheckout(setupCtx, existingReview); err != nil {
				rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
				defer rbCancel()
				return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
			}
			rc = existingReview
		}
		checkoutDir = rc.Dir
		state = &worktreeState{
			TaskID:      taskID,
			Mode:        checkoutModeWriter,
			ReadOnly:    true,
			CouncilKey:  key,
			CheckoutDir: rc.Dir,
			Repo:        resolvedRepo,
			CommonDir:   rc.GitDir,
			GitDir:      rc.GitDir,
			Base:        baseCommit,
		}
	}

	if a.beforeTaskState != nil {
		a.beforeTaskState(taskID)
	}
	if err := writeWorktreeState(taskBase, state); err != nil {
		rbCtx, rbCancel := context.WithTimeout(context.WithoutCancel(ctx), a.councilSetupTimeout())
		defer rbCancel()
		return "", "", nil, nil, nil, errors.Join(err, rollback(rbCtx))
	}
	return checkoutDir, taskBase, state, rollback, journal.discard, nil
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
	failedWriter := state.Mode == checkoutModeWriter && !state.ReadOnly && state.Result != nil && state.Result.ExitCode != 0
	if justFinished {
		if state.Mode == checkoutModeWriter && !state.ReadOnly && !failedWriter {
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
			state.Artifacts, err = writerWorktreeArtifacts(ctx, taskBase, checkoutDir, state, status)
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
		releaseCouncil, err := a.lockCouncil(ctx, root, state.CouncilKey)
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
			cs.RetainPin = true
			if state.Mode == checkoutModeWriter && !state.ReadOnly {
				if failedWriter {
					cs.WriterDirty = true
				} else if (justFinished || cs.ActiveWriterTaskID == state.TaskID) && state.Tip != "" {
					cs.WriterTip = state.Tip
					cs.WriterDirty = false
				}
				if cs.ActiveWriterTaskID == state.TaskID {
					cs.ActiveWriterTaskID = ""
				}
				if err := releaseWriterLock(root, state.CouncilKey, state.TaskID); err != nil {
					releaseCouncil()
					return err
				}
			}
			if err := writeCouncilState(root, state.CouncilKey, cs); err != nil {
				releaseCouncil()
				return err
			}
		}
		releaseCouncil()
	}

	if state.Mode == checkoutModeLive {
		if err := os.RemoveAll(taskBase + ".git"); err != nil {
			return err
		}
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

func writerWorktreeArtifacts(ctx context.Context, taskBase, checkoutDir string, state *worktreeState, status string) ([]scruntime.Artifact, error) {
	allowed, excluded, err := visibleWriterDiffPaths(ctx, checkoutDir, state)
	if err != nil {
		return nil, err
	}
	filteredStatus := filterStatusLines(status, excluded)
	artifacts := []scruntime.Artifact{{Name: "git-status", ContentType: "text/plain", Text: redact.String(strings.TrimSpace(filteredStatus))}}
	if len(allowed) > 0 {
		args := []string{
			"diff", "--no-ext-diff", "--no-textconv", "--no-color",
			"--submodule=short", "--end-of-options", state.Base, state.Tip, "--",
		}
		for _, p := range allowed {
			args = append(args, ":(literal)"+p)
		}
		diff, err := state.commonGit().run(ctx, args...)
		if err != nil {
			return nil, err
		}
		if diff != "" {
			diff = redactDiffText(diff)
			if len(diff) > 32*1024 {
				diff = diff[:32*1024] + "\n...[truncated]"
			}
			artifacts = append(artifacts, scruntime.Artifact{Name: "git-diff", ContentType: "text/x-diff", Text: diff})
		}
	}
	roArts, err := readOnlyArtifacts(taskBase, state)
	if err != nil {
		return nil, err
	}
	return append(artifacts, roArts...), nil
}

func visibleWriterDiffPaths(ctx context.Context, checkoutDir string, state *worktreeState) ([]string, map[string]bool, error) {
	raw, err := state.commonGit().run(ctx, "diff-tree", "--no-commit-id", "-r", "-z", "--raw", "--no-renames", "--end-of-options", state.Base, state.Tip, "--")
	if err != nil {
		return nil, nil, err
	}
	var allowed []string
	excluded := make(map[string]bool)
	parts := strings.Split(raw, "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		meta := parts[i]
		path := parts[i+1]
		if meta == "" || path == "" {
			continue
		}
		if path == sessionMarkerFile || path == sessionContextFile || !filepath.IsLocal(path) {
			excluded[path] = true
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(meta, ":"))
		if len(fields) < 5 {
			excluded[path] = true
			continue
		}
		newMode := fields[1]
		newOID := fields[3]
		if newMode == "120000" {
			target, err := state.commonGit().run(ctx, "cat-file", "blob", "--end-of-options", newOID)
			if err != nil {
				return nil, nil, err
			}
			if !symlinkTargetWithinCheckout(path, target) {
				excluded[path] = true
				continue
			}
		} else if newMode != "000000" && newMode != "160000" {
			if info, err := os.Lstat(filepath.Join(checkoutDir, path)); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o400 == 0 {
				excluded[path] = true
				continue
			}
		}
		allowed = append(allowed, path)
	}
	return allowed, excluded, nil
}

func symlinkTargetWithinCheckout(relPath, target string) bool {
	if target == "" || filepath.IsAbs(target) || strings.ContainsRune(target, 0) {
		return false
	}
	joined := filepath.Clean(filepath.Join(filepath.Dir(relPath), target))
	return filepath.IsLocal(joined)
}

func filterStatusLines(status string, excluded map[string]bool) string {
	if len(excluded) == 0 || status == "" {
		return status
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(status, "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		if excluded[path] {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func redactDiffText(text string) string {
	clean := redact.String(text)
	if hasUnfinishedPrivateKey(clean) {
		starts := privateKeyStart.FindAllStringIndex(clean, -1)
		if len(starts) > 0 {
			clean = clean[:starts[len(starts)-1][0]] + "[REDACTED:private_key]"
		}
	}
	return clean
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
	releaseCouncil, err := a.lockCouncil(ctx, root, key)
	if err != nil {
		return err
	}
	defer releaseCouncil()

	if err := reconcileCouncilJournals(ctx, root, key); err != nil {
		return err
	}

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
	if cs.ReviewCheckout != nil {
		if err := os.RemoveAll(cs.ReviewCheckout.Dir); err != nil {
			if changed {
				_ = writeCouncilState(root, key, cs)
			}
			return err
		}
		cs.ReviewCheckout = nil
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
		verifySaved := cs.WriterTip != "" && !cs.WriterDirty
		if err := removeWorktree(ctx, cs.WriterWorktree.Dir, wtState, verifySaved); err != nil {
			if changed {
				_ = writeCouncilState(root, key, cs)
			}
			return err
		}
		if cs.WriterTip == "" {
			_ = deleteBranchIfAt(ctx, cs.CommonDir, cs.WriterBranch, cs.BaseCommit)
		}
		cs.WriterWorktree = nil
	}
	if err := os.RemoveAll(councilDirPath(root, key)); err != nil {
		return err
	}
	return removeFile(statePath)
}

func objectFormatForCommit(commit string) string {
	if len(commit) == 64 {
		return "sha256"
	}
	return "sha1"
}

func copyReachableObjects(ctx context.Context, owner gitRepo, destGitDir string, commits []string, extraOIDs []string) error {
	seen := make(map[string]bool)
	var oids strings.Builder
	addOID := func(oid string) {
		oid = strings.ToLower(strings.TrimSpace(oid))
		if !isCommitID(oid) || seen[oid] {
			return
		}
		seen[oid] = true
		oids.WriteString(oid)
		oids.WriteByte('\n')
	}
	for _, c := range commits {
		out, err := owner.run(ctx, "rev-list", "--objects", "--end-of-options", c)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(out, "\n") {
			if line == "" {
				continue
			}
			oid, _, _ := strings.Cut(line, " ")
			addOID(oid)
		}
	}
	for _, oid := range extraOIDs {
		addOID(oid)
	}
	if len(seen) == 0 {
		return errors.New("no Git objects found for isolated checkout")
	}

	if owner.beforeRun != nil {
		if err := owner.beforeRun(); err != nil {
			return err
		}
	}
	packFlags := []string{
		"-C", owner.gitDir, "--no-pager", "--no-replace-objects",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull,
		"--git-dir=" + owner.gitDir,
		"pack-objects", "--stdout", "-q",
	}
	packCmd := exec.CommandContext(ctx, "git", packFlags...)
	packCmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=",
	}
	packCmd.Stdin = strings.NewReader(oids.String())
	var packErr bytes.Buffer
	packCmd.Stderr = &packErr

	packOut, err := packCmd.StdoutPipe()
	if err != nil {
		return err
	}
	packPath := filepath.Join(destGitDir, "objects", "pack", "snapshot.pack")
	indexFlags := []string{
		"-C", destGitDir, "--no-pager", "--no-replace-objects",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull,
		"--git-dir=" + destGitDir,
		"index-pack", "--stdin", packPath,
	}
	indexCmd := exec.CommandContext(ctx, "git", indexFlags...)
	indexCmd.Env = packCmd.Env
	indexCmd.Stdin = packOut
	var indexErr bytes.Buffer
	indexCmd.Stderr = &indexErr

	if err := packCmd.Start(); err != nil {
		return err
	}
	if err := indexCmd.Start(); err != nil {
		_ = packCmd.Process.Kill()
		_ = packCmd.Wait()
		return err
	}
	idxWaitErr := indexCmd.Wait()
	packWaitErr := packCmd.Wait()
	if packWaitErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("git pack-objects: %w (%s)", packWaitErr, redact.String(strings.TrimSpace(packErr.String())))
	}
	if idxWaitErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("git index-pack: %w (%s)", idxWaitErr, redact.String(strings.TrimSpace(indexErr.String())))
	}
	return nil
}

func createIsolatedReviewCheckout(ctx context.Context, owner gitRepo, dir, commit string) (_ *isolatedCheckout, retErr error) {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create isolated review checkout: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(dir)
		}
	}()

	initRepo := gitRepo{dir: dir}
	if _, err := initRepo.run(ctx, "init", "--quiet", "--template=", "--object-format="+objectFormatForCommit(commit)); err != nil {
		return nil, err
	}
	gitDir := filepath.Join(dir, ".git")
	if err := copyReachableObjects(ctx, owner, gitDir, []string{commit}, nil); err != nil {
		return nil, err
	}
	headPath := filepath.Join(gitDir, "HEAD")
	if err := os.WriteFile(headPath, []byte(commit+"\n"), 0o600); err != nil {
		return nil, err
	}
	checkout, err := (gitRepo{dir: dir, gitDir: gitDir, workTree: dir}).withoutFilters(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := checkout.run(ctx, "read-tree", "--reset", "-u", "--end-of-options", commit); err != nil {
		return nil, err
	}
	_, stamp, err := readMetadata(headPath)
	if err != nil {
		return nil, err
	}
	return &isolatedCheckout{
		Dir:       dir,
		Commit:    commit,
		GitDir:    gitDir,
		HeadStamp: stamp,
	}, nil
}

func validateIsolatedReviewCheckout(ctx context.Context, rc *isolatedCheckout) error {
	if rc == nil || rc.Dir == "" || rc.GitDir == "" || !isCommitID(rc.Commit) {
		return errors.New("council review checkout has no recorded identity")
	}
	expectedGit := filepath.Join(rc.Dir, ".git")
	if rc.GitDir != expectedGit {
		return errors.New("council review checkout gitdir is invalid")
	}
	info, err := os.Lstat(rc.GitDir)
	if err != nil || !info.IsDir() {
		return errors.New("council review checkout .git directory is missing or redirected")
	}
	_, stamp, err := readMetadata(filepath.Join(rc.GitDir, "HEAD"))
	if err != nil {
		return err
	}
	if stamp != rc.HeadStamp {
		return errors.New("council review checkout HEAD was modified")
	}
	checkout := gitRepo{dir: rc.Dir, gitDir: rc.GitDir, workTree: rc.Dir, readOnly: true}
	if _, err := checkout.run(ctx, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		return errors.New("read-only council review checkout is no longer detached")
	}
	head, err := checkout.commitID(ctx, "HEAD")
	if err != nil {
		return err
	}
	if head != rc.Commit {
		return errors.New("read-only council review checkout HEAD moved")
	}
	return nil
}

func createLiveGitDir(ctx context.Context, owner gitRepo, liveGitDir, headCommit string) (retErr error) {
	if err := os.Mkdir(liveGitDir, 0o700); err != nil {
		return fmt.Errorf("create live git dir: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(liveGitDir)
		}
	}()

	initRepo := gitRepo{dir: liveGitDir, gitDir: liveGitDir}
	if _, err := initRepo.run(ctx, "init", "--bare", "--quiet", "--template=", "--object-format="+objectFormatForCommit(headCommit)); err != nil {
		return err
	}
	if _, err := initRepo.run(ctx, "config", "--local", "core.bare", "false"); err != nil {
		return err
	}

	// Collect only blobs staged in the selected primary checkout's own index.
	var extraOIDs []string
	staged, err := owner.run(ctx, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	for _, record := range strings.Split(staged, "\x00") {
		if record == "" {
			continue
		}
		header, _, _ := strings.Cut(record, "\t")
		fields := strings.Fields(header)
		if len(fields) >= 2 && fields[0] != "160000" && isCommitID(fields[1]) {
			extraOIDs = append(extraOIDs, fields[1])
		}
	}
	if err := copyReachableObjects(ctx, owner, liveGitDir, []string{headCommit}, extraOIDs); err != nil {
		return err
	}

	headBytes, _, err := readMetadata(filepath.Join(owner.gitDir, "HEAD"))
	if err != nil {
		return err
	}
	headStr := strings.TrimSpace(string(headBytes))
	if refName, ok := strings.CutPrefix(headStr, "ref: "); ok && strings.HasPrefix(refName, "refs/heads/") && validCheckoutRef(refName) {
		refPath := filepath.Join(liveGitDir, filepath.FromSlash(refName))
		if err := os.MkdirAll(filepath.Dir(refPath), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(refPath, []byte(headCommit+"\n"), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(liveGitDir, "HEAD"), []byte("ref: "+refName+"\n"), 0o600); err != nil {
			return err
		}
	} else {
		if err := os.WriteFile(filepath.Join(liveGitDir, "HEAD"), []byte(headCommit+"\n"), 0o600); err != nil {
			return err
		}
	}

	// Copy primary checkout's own index if present.
	srcIndex := filepath.Join(owner.gitDir, "index")
	if info, err := os.Lstat(srcIndex); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("primary repository index is not a regular file")
		}
		if err := copyRegularFile(srcIndex, filepath.Join(liveGitDir, "index")); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Copy primary checkout's own info/exclude if present.
	srcExclude := filepath.Join(owner.gitDir, "info", "exclude")
	if data, _, err := readMetadata(srcExclude); err == nil {
		if err := os.MkdirAll(filepath.Join(liveGitDir, "info"), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(liveGitDir, "info", "exclude"), data, 0o600); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func copyRegularFile(src, dst string) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("expected regular file")
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
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
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultCouncilSetupTimeout)
		defer cancel()
		_ = removeSharedWorktree(rbCtx, owner.gitDir, cleanupWT, false)
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
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultCouncilSetupTimeout)
		defer cancel()
		_ = deleteBranchIfAt(rbCtx, owner.gitDir, branch, commit)
		return nil, err
	}
	wt, err := registerSharedWorktree(ctx, common, dir, commit)
	if err != nil {
		cleanupWT := wt
		if cleanupWT == nil {
			cleanupWT = &sharedWorktree{Dir: dir}
		}
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultCouncilSetupTimeout)
		defer cancel()
		_ = removeWriterWorktreeAndBranch(rbCtx, owner.gitDir, branch, commit, cleanupWT)
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
