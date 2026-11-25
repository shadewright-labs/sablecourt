package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A launch commits by deleting its start journal and then releasing the lock.
// A reconciler that opened the journal before the delete can take that lock
// afterwards. It must not treat the finished launch as interrupted.
func TestReconcileIgnoresJournalCommittedAfterOpen(t *testing.T) {
	root := t.TempDir()
	const (
		key    = "ccl-reconcile-0123456789abcdef"
		taskID = "writer-r2-a1"
	)
	commit := strings.Repeat("a", 40)
	if err := os.MkdirAll(councilDirPath(root, key), 0o700); err != nil {
		t.Fatal(err)
	}
	taskBase := filepath.Join(root, taskID)
	// A dry-run launch leaves a state file without a worker or run directory.
	if err := writeWorktreeState(taskBase, &worktreeState{TaskID: taskID, Base: commit}); err != nil {
		t.Fatal(err)
	}
	cs := &councilState{
		ConclaveID:         "ccl-reconcile",
		Key:                key,
		Mode:               checkoutModeWriter,
		Repo:               filepath.Join(root, "repo"),
		CommonDir:          filepath.Join(root, "repo", ".git"),
		BaseCommit:         commit,
		WriterRole:         "claude",
		ActiveWriterTaskID: taskID,
		ActiveTasks:        []string{taskID},
	}
	if err := writeCouncilState(root, key, cs); err != nil {
		t.Fatal(err)
	}
	if err := acquireWriterLock(root, key, taskID, "claude"); err != nil {
		t.Fatal(err)
	}
	journal, err := openStartJournal(root, key, councilStartJournal{
		TaskID: taskID, TaskBase: taskBase, Mode: checkoutModeWriter, IsWriter: true, CreatedWriterLock: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.discard() })

	reconcileAfterOpen = func(string) {
		if err := journal.discard(); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { reconcileAfterOpen = nil })

	if err := reconcileCouncilJournals(context.Background(), root, key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(taskBase + stateSuffix); err != nil {
		t.Errorf("reconcile removed the state of a committed launch: %v", err)
	}
	if _, err := os.Stat(writerLockPath(root, key)); err != nil {
		t.Errorf("reconcile released the writer lock of a committed launch: %v", err)
	}
	got, err := readCouncilState(councilStatePath(root, key))
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveWriterTaskID != taskID {
		t.Errorf("ActiveWriterTaskID = %q, want %q", got.ActiveWriterTaskID, taskID)
	}
}
