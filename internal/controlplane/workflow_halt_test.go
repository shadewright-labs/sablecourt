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

	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	sqlitedriver "modernc.org/sqlite"
)

// haltFaults holds the armed fault keys of sc_test_halt_fault: each fires once.
var haltFaults sync.Map

func init() {
	// sc_test_halt_fault(key, matches) is true once per armed key, and only
	// for a row that matches, so a trigger can fail one chosen write once
	// whichever transaction it is in. The counter lives outside the
	// database, so a rollback does not re-arm it.
	sqlitedriver.MustRegisterScalarFunction("sc_test_halt_fault", 2, func(_ *sqlitedriver.FunctionContext, args []driver.Value) (driver.Value, error) {
		key, _ := args[0].(string)
		matches, _ := args[1].(int64)
		v, ok := haltFaults.Load(key)
		if !ok || matches == 0 {
			return int64(0), nil
		}
		if v.(*atomic.Int32).Add(-1) == 0 {
			return int64(1), nil
		}
		return int64(0), nil
	})
}

// armHaltFault installs a trigger that fails one halt write once: the halt's
// audit row ("audit") or the halted run registration ("registration").
func armHaltFault(t *testing.T, f *commandFixture, write string) *atomic.Int32 {
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
	key := "halt-" + rand.Text()
	counter := &atomic.Int32{}
	counter.Store(1)
	haltFaults.Store(key, counter)
	t.Cleanup(func() { haltFaults.Delete(key) })
	var ddl string
	switch write {
	case "audit":
		ddl = fmt.Sprintf(`CREATE TRIGGER halt_fault BEFORE INSERT ON audit_log
			WHEN sc_test_halt_fault('%s', NEW.action IN ('workflow.verify', 'workflow.command'))
			BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`, key)
	case "registration":
		ddl = fmt.Sprintf(`CREATE TRIGGER halt_fault BEFORE INSERT ON court_memory
			WHEN sc_test_halt_fault('%s', NEW.namespace = '%s' AND instr(NEW.value, '"Status":"halted"') > 0)
			BEGIN SELECT RAISE(ABORT, 'injected registration failure'); END`, key, workflowStepsNamespace)
	default:
		t.Fatalf("unknown write %q", write)
	}
	if _, err := db.ExecContext(t.Context(), ddl); err != nil {
		t.Fatal(err)
	}
	return counter
}

// haltState counts the three halt writes of a run: halt events on its root
// task, halt audit rows, and the run registration's status and reason.
type haltState struct {
	events, audits int
	event          string
	status, reason string
}

func (f *commandFixture) haltState(rootID string) haltState {
	f.t.Helper()
	var h haltState
	for _, ev := range f.events(rootID) {
		if strings.HasPrefix(ev.Reason, "workflow_halted_") {
			h.events++
			h.event = ev.Reason
		}
	}
	audits, err := f.st.ListAudit(f.t.Context(), 1000)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, a := range audits {
		if (a.Action == "workflow.verify" && a.Outcome == "verdict_missing") || a.Action == "workflow.command" {
			h.audits++
		}
	}
	reg := f.runStatus(rootID)
	h.status, h.reason = reg.Status, reg.Reason
	return h
}

// allOrNone fails unless the run has all three halt writes, consistent with
// each other, or none of them (the decision of 2026-11-28 on issue 214).
func (h haltState) allOrNone(t *testing.T, want string) bool {
	t.Helper()
	switch {
	case h.events == 0 && h.audits == 0 && h.status != "halted":
		return false
	case h.events == 1 && h.audits == 1 && h.status == "halted" && h.event == "workflow_halted_"+want && h.reason == want:
		return true
	}
	t.Fatalf("partial halt: %+v, want all three writes for %s or none", h, want)
	return false
}

// TestRunHaltIsAllOrNothing checks each way a run halts against a failure of
// each halt write: the audit row (the round 3 case: event committed, audit
// row lost and never written by the replay) and the halted registration.
// After every reconcile pass the run has one halt event, one audit row and a
// halted registration with the event's reason, or none of the three; replays
// after the halt write nothing more (invariant 6).
func TestRunHaltIsAllOrNothing(t *testing.T) {
	t.Parallel()
	type haltCase struct {
		paths  []string
		reason string
		review string
		refuse bool // refuse the command launch once the author step is done
		steps  int  // a step that must not start, 0 for none
	}
	halts := map[string]haltCase{
		"command failed":  {paths: []string{"feature.txt"}, reason: "command_failed", steps: 2},
		"command refused": {paths: []string{"feature.txt"}, reason: "command_refused", refuse: true, steps: 2},
		"verdict missing": {paths: []string{"internal/redact/"}, reason: "verdict_missing", review: "no verdict line"},
	}
	for name, tc := range halts {
		for _, write := range []string{"audit", "registration"} {
			t.Run(name+"/"+write, func(t *testing.T) {
				t.Parallel()
				g := &guardRunner{}
				f := newCommandFixture(t, guardCommand(t, tc.paths...), g.run)
				if tc.review != "" {
					defaultRun := f.runner.onRun
					f.runner.onRun = func(ctx context.Context, s scruntime.CouncilLaunch) (string, string, int, error) {
						if s.Command == nil && s.Checkout.Kind != scruntime.CouncilTask {
							return tc.review, "", 0, nil
						}
						return defaultRun(ctx, s)
					}
				}
				fault := armHaltFault(t, f, write)
				root := f.start()
				if tc.refuse {
					f.runUntil("the author step", func() bool { _, ok := f.step(1); return terminal(f.step(0)) && ok })
					f.adapter.CouncilRunner = refusingRunner{f.runner}
				}
				f.runUntil("the halted run", func() bool { return f.haltState(root.ID).allOrNone(t, tc.reason) })
				for range 3 {
					_ = errors.Join(f.r.ReconcileOnce(t.Context()), waitForLaunches(t, f.r))
					f.haltState(root.ID).allOrNone(t, tc.reason)
				}
				if fault.Load() > 0 {
					t.Fatalf("the injected %s failure never fired", write)
				}
				if !f.haltState(root.ID).allOrNone(t, tc.reason) {
					t.Fatal("the run never halted")
				}
				if _, ok := f.step(tc.steps); ok && tc.steps > 0 {
					t.Fatalf("step %d started after the run halted", tc.steps)
				}
			})
		}
	}
}
