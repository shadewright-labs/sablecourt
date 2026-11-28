package controlplane

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
	sqlitedriver "modernc.org/sqlite"
)

// writeFaults holds the armed keys of sc_test_write_fault and how many more
// matching writes each fails.
var writeFaults sync.Map

func init() {
	// sc_test_write_fault(key, matches) is true for the next n matching rows
	// of an armed key, then false. The counter lives outside the database,
	// so a rollback does not re-arm it.
	sqlitedriver.MustRegisterScalarFunction("sc_test_write_fault", 2, func(_ *sqlitedriver.FunctionContext, args []driver.Value) (driver.Value, error) {
		key, _ := args[0].(string)
		matches, _ := args[1].(int64)
		v, ok := writeFaults.Load(key)
		if !ok || matches == 0 {
			return int64(0), nil
		}
		if v.(*atomic.Int32).Add(-1) >= 0 {
			return int64(1), nil
		}
		return int64(0), nil
	})
}

// armWriteFault installs a trigger that fails the next n inserts into table
// whose row matches cond (an SQL expression over NEW). The returned counter
// is at or below zero once all n failures fired.
func armWriteFault(t *testing.T, f *commandFixture, table, cond string, n int32) *atomic.Int32 {
	t.Helper()
	path, err := f.st.MainFile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	key := "fault_" + strings.ToLower(rand.Text())
	counter := &atomic.Int32{}
	counter.Store(n)
	writeFaults.Store(key, counter)
	t.Cleanup(func() { writeFaults.Delete(key) })
	ddl := fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON %s
		WHEN sc_test_write_fault('%s', %s)
		BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`, key, table, key, cond)
	if _, err := db.ExecContext(t.Context(), ddl); err != nil {
		t.Fatal(err)
	}
	return counter
}

// haltWriteCond selects one halt write: its audit row or its halted run
// registration.
func haltWriteCond(write string) (table, cond string) {
	if write == "audit" {
		return "audit_log", "NEW.action = 'workflow.command'"
	}
	return "court_memory", fmt.Sprintf(`NEW.namespace = '%s' AND instr(NEW.value, '"Status":"halted"') > 0`, workflowStepsNamespace)
}

func (f *commandFixture) pendingHalt(taskID string) bool {
	f.t.Helper()
	_, err := f.st.GetMemory(f.t.Context(), pendingHaltsNamespace, taskID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		f.t.Fatal(err)
	}
	return err == nil
}

// TestCommandStepHaltRetriedByLaterPass covers every path that fails a
// command step outside a runtime exit (issues 226 and 253): a launch that
// fails, a command the control plane refuses, and a step lost on restart
// with no handle left. When every halt attempt of that path fails, the step
// is failed and visible, and a later reconcile pass still halts the run with
// one halt event, one audit row and a halted registration, then clears the
// step's pending halt and writes nothing more.
func TestCommandStepHaltRetriedByLaterPass(t *testing.T) {
	t.Parallel()
	type haltCase struct {
		reason string
		faults int32 // halt attempts the path makes, all failed
		fail   func(t *testing.T, f *commandFixture)
	}
	cases := map[string]haltCase{
		"launch failed": {reason: "command_failed", faults: 1, fail: func(t *testing.T, f *commandFixture) {
			// The command step's launch fails on its lease write, before any start.
			armWriteFault(t, f, "leases", "1", 1)
		}},
		"command refused": {reason: "command_refused", faults: 2, fail: func(_ *testing.T, f *commandFixture) {
			f.adapter.CouncilRunner = refusingRunner{f.runner}
		}},
	}
	for name, tc := range cases {
		for _, write := range []string{"audit", "registration"} {
			t.Run(name+"/"+write, func(t *testing.T) {
				t.Parallel()
				f := newCommandFixture(t, checkCommand(0, 0), func(context.Context, scruntime.CouncilLaunch) (string, string, int, error) {
					t.Error("the command ran")
					return "", "", 0, nil
				})
				root := f.start()
				f.runUntil("the author step", func() bool { _, ok := f.step(1); return terminal(f.step(0)) && ok })
				table, cond := haltWriteCond(write)
				halts := armWriteFault(t, f, table, cond, tc.faults)
				tc.fail(t, f)
				f.runUntil("the failed command step", func() bool { return terminal(f.step(1)) && halts.Load() <= 0 })
				check, _ := f.step(1)
				if check.State != domain.TaskStateFailed {
					t.Fatalf("command step state = %s, want failed", check.State)
				}
				f.runUntil("the halted run", func() bool { return f.haltState(root.ID).allOrNone(t, tc.reason) })
				for range 3 {
					_ = errors.Join(f.r.ReconcileOnce(t.Context()), waitForLaunches(t, f.r))
					if !f.haltState(root.ID).allOrNone(t, tc.reason) {
						t.Fatal("the halt was undone")
					}
				}
				if f.pendingHalt(check.ID) {
					t.Fatal("the pending halt is still recorded after the halt")
				}
				if _, ok := f.step(2); ok {
					t.Fatal("the review step started after the run halted")
				}
			})
		}
	}
}

// TestCommandStepLostWithoutHandleHaltRetried covers a command step failed
// as lost on restart when no handle of it is left to keep it in recovery:
// its halt fails once and a later pass still halts the run.
func TestCommandStepLostWithoutHandleHaltRetried(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	f := newCommandFixture(t, checkCommand(0, 0), func(ctx context.Context, _ scruntime.CouncilLaunch) (string, string, int, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "", "", 0, nil
	})
	defer close(release)
	root := f.start()
	f.runUntil("the running command", func() bool {
		check, ok := f.step(1)
		return ok && check.State == domain.TaskStateWorking && len(f.commandLaunches()) == 1
	})
	check, _ := f.step(1)
	// Drop every handle of the step, as when the daemon restarted before
	// the handle was saved.
	f.r.mu.Lock()
	delete(f.r.handles, check.ID)
	delete(f.r.handleAdapters, check.ID)
	f.r.mu.Unlock()
	if err := f.r.deletePersistedHandle(t.Context(), check.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	table, cond := haltWriteCond("audit")
	halts := armWriteFault(t, f, table, cond, 1)
	if err := f.r.failLostTask(t.Context(), check, `{"reason":"container_not_alive"}`); err == nil {
		t.Fatal("failLostTask reported success although its halt failed")
	}
	if halts.Load() > 0 {
		t.Fatal("the injected halt failure never fired")
	}
	f.runUntil("the halted run", func() bool { return f.haltState(root.ID).allOrNone(t, "command_failed") })
	if f.pendingHalt(check.ID) {
		t.Fatal("the pending halt is still recorded after the halt")
	}
}
