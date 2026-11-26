package coding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recoveryFixture is a council root with a real primary repository, laid
// out on resolved paths so Git's worktree listing matches the paths the
// reconciler compares.
type recoveryFixture struct {
	root, key, repo, common, base, councilDir string
}

func newRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	root, err := resolvedPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := resolvedPath(initTempGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	f := recoveryFixture{
		root:   root,
		key:    councilKey("ccl-recovery"),
		repo:   repo,
		common: filepath.Join(repo, ".git"),
		base:   strings.TrimSpace(testGit(t, repo, "rev-parse", "HEAD")),
	}
	f.councilDir = councilDirPath(root, f.key)
	if err := os.MkdirAll(f.councilDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f recoveryFixture) state(tasks ...string) *councilState {
	return &councilState{
		ConclaveID:   "ccl-recovery",
		Key:          f.key,
		Mode:         checkoutModeWriter,
		Repo:         f.repo,
		CommonDir:    f.common,
		BaseCommit:   f.base,
		WriterRole:   "claude",
		WriterBranch: councilWriterBranch(f.key),
		ActiveTasks:  tasks,
	}
}

// crashedJournal leaves a start journal on disk as a launch that died
// before committing would: written, then unlocked by process exit.
func (f recoveryFixture) crashedJournal(t *testing.T, rec councilStartJournal) string {
	t.Helper()
	h, err := openStartJournal(f.root, f.key, rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.file.Close(); err != nil {
		t.Fatal(err)
	}
	return h.path
}

func (f recoveryFixture) worktreeListed(t *testing.T, dir string) bool {
	t.Helper()
	return strings.Contains(testGit(t, f.repo, "worktree", "list", "--porcelain"), "worktree "+dir+"\n")
}

func (f recoveryFixture) branchExists(t *testing.T, branch string) bool {
	t.Helper()
	return strings.TrimSpace(testGit(t, f.repo, "branch", "--list", branch)) != ""
}

func TestReconcileWithoutCouncilDirectory(t *testing.T) {
	if err := reconcileCouncilJournals(context.Background(), t.TempDir(), "absent"); err != nil {
		t.Fatalf("reconcile of a missing council = %v", err)
	}
}

func TestReconcileInterruptedWriterLaunch(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		name := "branch at base"
		if advanced {
			name = "branch has new commits"
		}
		t.Run(name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			const task = "writer-1"
			branch := councilWriterBranch(f.key)
			writerDir := filepath.Join(f.councilDir, "writer")
			testGit(t, f.repo, "worktree", "add", "--quiet", "-b", branch, writerDir, f.base)
			var wantTip string
			if advanced {
				testGit(t, writerDir, "commit", "--allow-empty", "--quiet", "-m", "writer work")
				wantTip = strings.TrimSpace(testGit(t, writerDir, "rev-parse", "HEAD"))
				if wantTip == f.base {
					t.Fatal("writer commit did not advance branch")
				}
			}
			cs := f.state(task, "reviewer-1")
			cs.ActiveWriterTaskID = task
			if err := writeCouncilState(f.root, f.key, cs); err != nil {
				t.Fatal(err)
			}
			if err := acquireWriterLock(f.root, f.key, task, "claude"); err != nil {
				t.Fatal(err)
			}
			journal := f.crashedJournal(t, councilStartJournal{
				TaskID: task, TaskBase: filepath.Join(f.root, task), CommonDir: f.common, Mode: checkoutModeWriter,
				IsWriter: true, WriterBranch: branch, BaseCommit: f.base,
				CreatedWriterWT: true, CreatedWriterLock: true,
			})

			for round := range 2 {
				if err := reconcileCouncilJournals(context.Background(), f.root, f.key); err != nil {
					t.Fatalf("reconcile round %d: %v", round, err)
				}
				if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("round %d: journal remains: %v", round, err)
				}
				if _, err := os.Stat(writerLockPath(f.root, f.key)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("round %d: writer lock remains: %v", round, err)
				}
				if f.worktreeListed(t, writerDir) {
					t.Errorf("round %d: orphaned writer worktree still registered", round)
				}
				if _, err := os.Stat(writerDir); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("round %d: orphaned writer directory remains: %v", round, err)
				}
				// A branch that moved past the base holds work and must survive.
				if got := f.branchExists(t, branch); got != advanced {
					t.Errorf("round %d: writer branch exists = %t, want %t", round, got, advanced)
				}
				if advanced {
					if tip := strings.TrimSpace(testGit(t, f.repo, "rev-parse", "--verify", "refs/heads/"+branch)); tip != wantTip {
						t.Errorf("round %d: writer branch tip = %q, want %q", round, tip, wantTip)
					}
				}
				got, err := readCouncilState(councilStatePath(f.root, f.key))
				if err != nil {
					t.Fatal(err)
				}
				if got.ActiveWriterTaskID != "" || strings.Join(got.ActiveTasks, ",") != "reviewer-1" {
					t.Errorf("round %d: state = writer %q tasks %v", round, got.ActiveWriterTaskID, got.ActiveTasks)
				}
			}
		})
	}
}

func TestReconcileInterruptedSetupClearsReservations(t *testing.T) {
	f := newRecoveryFixture(t)
	const task = "ref-1"
	refDir := filepath.Join(f.councilDir, "ref")
	reviewDir := filepath.Join(f.councilDir, "review")
	testGit(t, f.repo, "worktree", "add", "--quiet", "--detach", refDir, f.base)
	if err := os.MkdirAll(filepath.Join(reviewDir, "partial"), 0o700); err != nil {
		t.Fatal(err)
	}
	cs := f.state(task)
	cs.Mode = checkoutModeRef
	cs.RefSetupTaskID, cs.ReviewSetupTaskID = task, task
	if err := writeCouncilState(f.root, f.key, cs); err != nil {
		t.Fatal(err)
	}
	f.crashedJournal(t, councilStartJournal{
		TaskID: task, CommonDir: f.common, Mode: checkoutModeRef,
		CreatedCouncilDir: true, CreatedRefWT: true, CreatedReviewDir: true,
	})
	if err := reconcileCouncilJournals(context.Background(), f.root, f.key); err != nil {
		t.Fatal(err)
	}
	if f.worktreeListed(t, refDir) {
		t.Error("orphaned ref worktree still registered")
	}
	if _, err := os.Stat(reviewDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial review checkout remains: %v", err)
	}
	// The council existed only for this launch: its state and directory go.
	if _, err := os.Stat(councilStatePath(f.root, f.key)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("council state remains: %v", err)
	}
	if _, err := os.Stat(f.councilDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("council directory remains: %v", err)
	}
}

func TestReconcileKeepsPinnedCouncilState(t *testing.T) {
	f := newRecoveryFixture(t)
	const task = "ref-2"
	// Without the pin, this council would be removed with its last task.
	cs := f.state(task)
	cs.Mode = checkoutModeRef
	cs.RetainPin = true
	cs.RefSetupTaskID = task
	if err := writeCouncilState(f.root, f.key, cs); err != nil {
		t.Fatal(err)
	}
	f.crashedJournal(t, councilStartJournal{TaskID: task, CreatedCouncilDir: true, CreatedRefWT: true})
	if err := reconcileCouncilJournals(context.Background(), f.root, f.key); err != nil {
		t.Fatal(err)
	}
	got, err := readCouncilState(councilStatePath(f.root, f.key))
	if err != nil {
		t.Fatalf("pinned council state was removed: %v", err)
	}
	if got.RefSetupTaskID != "" || !got.RetainPin || len(got.ActiveTasks) != 0 {
		t.Errorf("state after reconcile = %+v", got)
	}
}

func TestReconcileWithoutCouncilStateRemovesCreatedCheckouts(t *testing.T) {
	f := newRecoveryFixture(t)
	const task = "writer-2"
	branch := councilWriterBranch(f.key)
	refDir := filepath.Join(f.councilDir, "ref")
	writerDir := filepath.Join(f.councilDir, "writer")
	reviewDir := filepath.Join(f.councilDir, "review")
	testGit(t, f.repo, "worktree", "add", "--quiet", "--detach", refDir, f.base)
	testGit(t, f.repo, "worktree", "add", "--quiet", "-b", branch, writerDir, f.base)
	if err := os.MkdirAll(reviewDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.crashedJournal(t, councilStartJournal{
		TaskID: task, CommonDir: f.common, WriterBranch: branch, BaseCommit: f.base,
		CreatedCouncilDir: true, CreatedRefWT: true, CreatedWriterWT: true, CreatedReviewDir: true,
	})
	if err := reconcileCouncilJournals(context.Background(), f.root, f.key); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{refDir, writerDir} {
		if f.worktreeListed(t, dir) {
			t.Errorf("orphaned worktree %s still registered", filepath.Base(dir))
		}
	}
	if f.branchExists(t, branch) {
		t.Error("unused writer branch remains")
	}
	if _, err := os.Stat(f.councilDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("council directory remains: %v", err)
	}
}

func TestReconcileLaunchedTaskIsNotUndone(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      *worktreeState
		run        bool
		incomplete bool
	}{
		{name: "worker recorded", state: &worktreeState{Worker: &workerIdentity{}}},
		{name: "run directory present", state: &worktreeState{}, run: true},
		{name: "state without worker or run directory", state: &worktreeState{}, incomplete: true},
		{name: "no state file", incomplete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			const task = "writer-3"
			taskBase := filepath.Join(f.root, task)
			if tc.state != nil {
				tc.state.TaskID, tc.state.Base = task, f.base
				if err := writeWorktreeState(taskBase, tc.state); err != nil {
					t.Fatal(err)
				}
				testWrite(t, f.root, task+".log", "log", 0o600)
			}
			if tc.run {
				if err := os.Mkdir(taskBase+".run", 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cs := f.state(task)
			cs.ActiveWriterTaskID = task
			if err := writeCouncilState(f.root, f.key, cs); err != nil {
				t.Fatal(err)
			}
			if err := acquireWriterLock(f.root, f.key, task, "claude"); err != nil {
				t.Fatal(err)
			}
			journal := f.crashedJournal(t, councilStartJournal{
				TaskID: task, TaskBase: taskBase, IsWriter: true, CreatedWriterLock: true,
			})
			if err := reconcileCouncilJournals(context.Background(), f.root, f.key); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("journal remains: %v", err)
			}
			_, lockErr := os.Stat(writerLockPath(f.root, f.key))
			if released := errors.Is(lockErr, os.ErrNotExist); released != tc.incomplete {
				t.Errorf("writer lock released = %t, want %t", released, tc.incomplete)
			}
			got, err := readCouncilState(councilStatePath(f.root, f.key))
			if err != nil {
				t.Fatal(err)
			}
			if active := got.ActiveWriterTaskID == task; active == tc.incomplete {
				t.Errorf("ActiveWriterTaskID = %q with incomplete = %t", got.ActiveWriterTaskID, tc.incomplete)
			}
			if tc.state != nil {
				_, stErr := os.Stat(taskBase + stateSuffix)
				if removed := errors.Is(stErr, os.ErrNotExist); removed != tc.incomplete {
					t.Errorf("task state removed = %t, want %t", removed, tc.incomplete)
				}
			}
		})
	}
}

func TestReconcileSkipsActiveJournal(t *testing.T) {
	f := newRecoveryFixture(t)
	const task = "writer-4"
	cs := f.state(task)
	if err := writeCouncilState(f.root, f.key, cs); err != nil {
		t.Fatal(err)
	}
	live, err := openStartJournal(f.root, f.key, councilStartJournal{TaskID: task})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.discard() })
	// Entries that are not journals are ignored.
	if err := os.Mkdir(filepath.Join(f.councilDir, journalPrefix+"dir"+journalSuffix), 0o700); err != nil {
		t.Fatal(err)
	}
	testWrite(t, f.councilDir, "notes.txt", "{", 0o600)
	if err := reconcileCouncilJournals(context.Background(), f.root, f.key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live.path); err != nil {
		t.Errorf("reconcile removed a journal held by a running launch: %v", err)
	}
	got, err := readCouncilState(councilStatePath(f.root, f.key))
	if err != nil || strings.Join(got.ActiveTasks, ",") != task {
		t.Errorf("state changed under an active launch: %+v, %v", got, err)
	}
}

func TestReconcileRefusesCorruptRecords(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f recoveryFixture)
		want  string
	}{
		{name: "journal is not JSON", setup: func(t *testing.T, f recoveryFixture) {
			testWrite(t, f.councilDir, journalPrefix+"bad"+journalSuffix, "{", 0o600)
		}, want: "invalid council start journal"},
		{name: "journal without task ID", setup: func(t *testing.T, f recoveryFixture) {
			testWrite(t, f.councilDir, journalPrefix+"empty"+journalSuffix, `{"task_base":"x"}`, 0o600)
		}, want: "invalid council start journal"},
		{name: "task state is corrupt", setup: func(t *testing.T, f recoveryFixture) {
			taskBase := filepath.Join(f.root, "t1")
			testWrite(t, f.root, "t1"+stateSuffix, "{", 0o600)
			f.crashedJournal(t, councilStartJournal{TaskID: "t1", TaskBase: taskBase})
		}, want: "decode coding worktree state"},
		{name: "council state is corrupt", setup: func(t *testing.T, f recoveryFixture) {
			testWrite(t, filepath.Dir(f.councilDir), f.key+".json", "{", 0o600)
			f.crashedJournal(t, councilStartJournal{TaskID: "t2"})
		}, want: "decode council checkout state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			tc.setup(t, f)
			err := reconcileCouncilJournals(context.Background(), f.root, f.key)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("reconcile error = %v, want %q", err, tc.want)
			}
			entries, readErr := os.ReadDir(f.councilDir)
			if readErr != nil || len(entries) == 0 {
				t.Errorf("reconcile discarded the evidence of a refused journal: %v", readErr)
			}
		})
	}
}

func TestRemoveOrphanedWorktree(t *testing.T) {
	f := newRecoveryFixture(t)
	registered := filepath.Join(f.councilDir, "ref")
	testGit(t, f.repo, "worktree", "add", "--quiet", "--detach", registered, f.base)
	plain := filepath.Join(f.councilDir, "plain")
	if err := os.MkdirAll(filepath.Join(plain, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{registered, plain} {
		if err := removeOrphanedWorktree(context.Background(), f.common, dir); err != nil {
			t.Fatalf("remove %s: %v", filepath.Base(dir), err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s remains: %v", filepath.Base(dir), err)
		}
	}
	if f.worktreeListed(t, registered) {
		t.Error("registered worktree still listed")
	}
	// Empty arguments are a no-op, never a removal of the target or working directory.
	keep := filepath.Join(f.councilDir, "keep")
	if err := os.MkdirAll(keep, 0o700); err != nil {
		t.Fatal(err)
	}
	testWrite(t, keep, "sentinel", "keep-me", 0o600)
	if err := removeOrphanedWorktree(context.Background(), "", keep); err != nil {
		t.Error(err)
	}
	if data, err := os.ReadFile(filepath.Join(keep, "sentinel")); err != nil || string(data) != "keep-me" {
		t.Errorf("sentinel after blank commonDir = %q, %v; want untouched", data, err)
	}
	if err := removeOrphanedWorktree(context.Background(), f.common, ""); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(f.repo); err != nil {
		t.Errorf("primary repository was touched: %v", err)
	}
}

func TestLockCouncilContention(t *testing.T) {
	root := t.TempDir()
	first, second := NewAdapter(root), NewAdapter(root)
	release, err := first.lockCouncil(context.Background(), root, "k1")
	if err != nil {
		t.Fatal(err)
	}
	// Another adapter (another daemon process in production) sees the file lock.
	timed, cancelTimed := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelTimed()
	if rel, err := second.lockCouncil(timed, root, "k1"); !errors.Is(err, context.DeadlineExceeded) {
		if rel != nil {
			rel()
		}
		t.Fatalf("contended lock = %v, want context.DeadlineExceeded", err)
	}
	// The same adapter waits on its in-memory gate.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if rel, err := first.lockCouncil(cancelled, root, "k1"); !errors.Is(err, context.Canceled) {
		if rel != nil {
			rel()
		}
		t.Fatalf("re-entrant lock = %v, want context.Canceled", err)
	}
	// A different council key is independent.
	other, err := second.lockCouncil(context.Background(), root, "k2")
	if err != nil {
		t.Fatalf("independent council lock = %v", err)
	}
	other()
	release()
	again, err := second.lockCouncil(context.Background(), root, "k1")
	if err != nil {
		t.Fatalf("lock after release = %v", err)
	}
	again()
}

func TestLockCouncilRefusesUnusableRoot(t *testing.T) {
	root := t.TempDir()
	testWrite(t, root, councilsDirName, "not a directory", 0o600)
	a := NewAdapter(root)
	if _, err := a.lockCouncil(context.Background(), root, "k1"); err == nil {
		t.Fatal("locked a council under a file")
	}
	// The in-memory gate was released on failure.
	if release, err := a.lockScope(context.Background(), "council:k1", true); err != nil {
		t.Fatalf("gate still held after failure: %v", err)
	} else {
		release()
	}
}
