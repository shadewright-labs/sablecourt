package store_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/postgres"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
	sqlitedriver "modernc.org/sqlite"
)

// effectFaults holds the armed keys of sc_test_effect_fault: each fires once.
var effectFaults sync.Map

func init() {
	// sc_test_effect_fault(key) is true once per armed key. The counter
	// lives outside the database, so the rollback it causes does not re-arm it.
	sqlitedriver.MustRegisterScalarFunction("sc_test_effect_fault", 1, func(_ *sqlitedriver.FunctionContext, args []driver.Value) (driver.Value, error) {
		key, _ := args[0].(string)
		if v, ok := effectFaults.Load(key); ok && v.(*atomic.Int32).Add(-1) == 0 {
			return int64(1), nil
		}
		return int64(0), nil
	})
}

const haltNamespace = "system:workflow-steps"

// effectRig is one backend with a raw connection for fault triggers.
type effectRig struct {
	backend string
	st      store.Store
	db      *sql.DB
}

func openEffectRig(t *testing.T, backend string) effectRig {
	t.Helper()
	var st store.Store
	var err error
	var driverName, dsn string
	if backend == "sqlite" {
		driverName, dsn = "sqlite", filepath.Join(t.TempDir(), "state.db")
		st, err = sqlite.Open(dsn)
	} else {
		dsn = os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("SABLECOURT_TEST_POSTGRES_DSN is unset")
		}
		driverName, dsn = "pgx", isolatedPostgresDSN(t, dsn)
		st, err = postgres.Open(dsn)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorkload(t.Context(), domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return effectRig{backend: backend, st: st, db: db}
}

// arm makes the next write to table fail once, inside whatever transaction
// it runs in.
func (r effectRig) arm(t *testing.T, table string) {
	t.Helper()
	key := "effect_" + rand.Text()
	var stmts []string
	if r.backend == "sqlite" {
		counter := &atomic.Int32{}
		counter.Store(1)
		effectFaults.Store(key, counter)
		t.Cleanup(func() { effectFaults.Delete(key) })
		stmts = []string{fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON %s WHEN sc_test_effect_fault('%s')
			BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`, key, table, key)}
	} else {
		// A sequence is not transactional, so the rollback keeps it advanced.
		stmts = []string{
			"CREATE SEQUENCE " + key,
			fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN
				IF nextval('%s') = 1 THEN RAISE EXCEPTION 'injected write failure'; END IF;
				RETURN NEW; END $$ LANGUAGE plpgsql`, key, key),
			fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION %s()", key, table, key),
		}
	}
	for _, stmt := range stmts {
		if _, err := r.db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// halt is one call that halts run root for the step task stepID.
func halt(ctx context.Context, st store.Store, root, stepID, reason string) error {
	fact, _ := json.Marshal(map[string]string{"command_task_id": stepID})
	task, err := st.GetTask(ctx, root)
	if err != nil {
		return err
	}
	reg, _ := json.Marshal(map[string]string{"Status": "halted", "Reason": reason, "Step": stepID})
	return store.UpdateTaskStateWithEffects(ctx, st, root, task.State, task.State, "workflow_halted_"+reason, string(fact), store.TransitionEffects{
		Recorded: func(r, f string) bool {
			var named map[string]string
			return r == "workflow_halted_"+reason && json.Unmarshal([]byte(f), &named) == nil && named["command_task_id"] == stepID
		},
		Audit: &domain.AuditRecord{Category: "workflow", TaskID: stepID, WorkloadName: "worker", Action: "workflow.command", Outcome: reason, CreatedAt: time.Now()},
		Memory: &domain.MemoryEntry{Namespace: haltNamespace, Key: root, Value: string(reg), Author: "controlplane",
			UpdatedAt: time.Now()},
	})
}

type haltCounts struct {
	events, audits int
	halted         bool
}

func countHalt(t *testing.T, st store.Store, root, stepID, reason string) haltCounts {
	t.Helper()
	var c haltCounts
	events, err := st.ListTaskEvents(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Reason == "workflow_halted_"+reason {
			c.events++
		}
	}
	audits, err := st.ListAudit(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range audits {
		if a.Action == "workflow.command" && a.TaskID == stepID {
			c.audits++
		}
	}
	entry, err := st.GetMemory(t.Context(), haltNamespace, root)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	var reg map[string]string
	c.halted = err == nil && json.Unmarshal([]byte(entry.Value), &reg) == nil && reg["Status"] == "halted" && reg["Reason"] == reason && reg["Step"] == stepID
	return c
}

// allOrNone fails unless the halt is complete (one event, one audit row and
// the halted registration with the event's reason) or absent, and reports
// whether it is complete.
func allOrNone(t *testing.T, c haltCounts) bool {
	t.Helper()
	switch c {
	case haltCounts{}:
		return false
	case haltCounts{events: 1, audits: 1, halted: true}:
		return true
	}
	t.Fatalf("partial halt: %+v", c)
	return false
}

// TestTransitionEffectsAllOrNothing is the property of the decision of
// 2026-11-28 on issue 214, in both stores: after any number of failed and
// replayed halts, the step has exactly one halt event, one audit row and a
// halted registration with the event's reason, or none of the three.
func TestTransitionEffectsAllOrNothing(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			r := openEffectRig(t, backend)
			ctx := t.Context()
			for _, fault := range []string{"", "task_events", "audit_log", "court_memory"} {
				t.Run("fault="+fault, func(t *testing.T) {
					root, step := "run-"+rand.Text(), "check-"+rand.Text()
					createTask(t, r.st, root, domain.TaskStateCompleted)
					if fault != "" {
						r.arm(t, fault)
						if err := halt(ctx, r.st, root, step, "command_failed"); err == nil {
							t.Fatalf("halt committed although the %s write failed", fault)
						}
						if allOrNone(t, countHalt(t, r.st, root, step, "command_failed")) {
							t.Fatal("halt written although its transaction failed")
						}
					}
					if err := halt(ctx, r.st, root, step, "command_failed"); err != nil {
						t.Fatal(err)
					}
					if !allOrNone(t, countHalt(t, r.st, root, step, "command_failed")) {
						t.Fatal("halt missing after a successful call")
					}
					// Replays find the event and write nothing.
					for range 2 {
						if err := halt(ctx, r.st, root, step, "command_failed"); !errors.Is(err, store.ErrAlreadyRecorded) {
							t.Fatalf("replay = %v, want ErrAlreadyRecorded", err)
						}
						allOrNone(t, countHalt(t, r.st, root, step, "command_failed"))
					}
				})
			}
			t.Run("concurrent", func(t *testing.T) {
				root, step := "run-"+rand.Text(), "check-"+rand.Text()
				createTask(t, r.st, root, domain.TaskStateCompleted)
				var wg sync.WaitGroup
				var wrote atomic.Int32
				for range 6 {
					wg.Go(func() {
						// A busy SQLite database can refuse a call; retry a bounded number of times.
						for range 50 {
							err := halt(ctx, r.st, root, step, "command_failed")
							if err == nil {
								wrote.Add(1)
							}
							if err == nil || errors.Is(err, store.ErrAlreadyRecorded) {
								return
							}
						}
					})
				}
				wg.Wait()
				if wrote.Load() != 1 || !allOrNone(t, countHalt(t, r.st, root, step, "command_failed")) {
					t.Fatalf("concurrent halts wrote %d times", wrote.Load())
				}
			})
			t.Run("other step", func(t *testing.T) {
				// An event of another step is not this step's halt.
				root := "run-" + rand.Text()
				createTask(t, r.st, root, domain.TaskStateCompleted)
				if err := halt(ctx, r.st, root, "check-a", "command_failed"); err != nil {
					t.Fatal(err)
				}
				if err := halt(ctx, r.st, root, "check-b", "command_refused"); err != nil {
					t.Fatalf("halt of another step = %v", err)
				}
			})
		})
	}
}

// ignoringStore drops the context of UpdateTaskState, as a store that does
// not know about transition effects would.
type ignoringStore struct{ store.Store }

func (s ignoringStore) UpdateTaskState(_ context.Context, taskID string, from, to domain.TaskState, reason, factJSON string) error {
	return s.Store.UpdateTaskState(context.Background(), taskID, from, to, reason, factJSON)
}

func TestTransitionEffectsFailClosedWhenNotApplied(t *testing.T) {
	r := openEffectRig(t, "sqlite")
	createTask(t, r.st, "run", domain.TaskStateCompleted)
	if err := halt(t.Context(), ignoringStore{r.st}, "run", "check", "command_failed"); err == nil {
		t.Fatal("a store that dropped the effects reported success")
	}
}
