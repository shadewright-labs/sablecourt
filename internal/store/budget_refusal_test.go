package store_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestExceedsBudgetLimit(t *testing.T) {
	for _, tc := range []struct {
		used, reserve, maximum int64
		want                   bool
	}{
		{0, 0, 10, false},
		{4, 6, 10, false},
		{4, 7, 10, true},
		{10, 0, 10, true},
		{11, 0, 10, true},
		{0, math.MaxInt64, 10, true},
		{math.MaxInt64 - 1, math.MaxInt64, math.MaxInt64, true},
		{1, 0, 0, true},
	} {
		if got := store.ExceedsBudgetLimit(tc.used, tc.reserve, tc.maximum); got != tc.want {
			t.Errorf("ExceedsBudgetLimit(%d, %d, %d) = %v, want %v", tc.used, tc.reserve, tc.maximum, got, tc.want)
		}
	}
}

func TestCheckChildBudgetCeiling(t *testing.T) {
	used := domain.BudgetTotals{InputTokens: 5, OutputTokens: 5, CostMicroUSD: 500_000}
	reserve := domain.BudgetTotals{InputTokens: 5, OutputTokens: 5, CostMicroUSD: 500_000}
	for _, tc := range []struct {
		name, raw, reason string
		invalid           bool
	}{
		{name: "all within", raw: `{"maxInputTokens":11,"maxOutputTokens":11,"maxCostUSD":1.5}`},
		{name: "input exactly at ceiling", raw: `{"maxInputTokens":10}`, reason: ""},
		{name: "input over", raw: `{"maxInputTokens":9}`, reason: "child_input_tokens"},
		{name: "output reached", raw: `{"maxOutputTokens":9}`, reason: "child_output_tokens"},
		{name: "cost over", raw: `{"maxCostUSD":0.999999}`, reason: "child_cost_usd"},
		{name: "first reason wins", raw: `{"maxInputTokens":1,"maxOutputTokens":1,"maxCostUSD":0.1}`, reason: "child_input_tokens"},
		{name: "zero ceiling refuses", raw: `{"maxOutputTokens":0}`, reason: "child_output_tokens"},
		{name: "later limit still validated", raw: `{"maxInputTokens":1,"maxCostUSD":"1"}`, invalid: true},

		{name: "empty", raw: ``, invalid: true},
		{name: "empty object", raw: `{}`, invalid: true},
		{name: "null", raw: `null`, invalid: true},
		{name: "array", raw: `[1]`, invalid: true},
		{name: "unknown member", raw: `{"maxInputTokens":10,"maxTotal":1}`, invalid: true},
		{name: "duplicate member", raw: `{"maxInputTokens":100,"maxInputTokens":1}`, invalid: true},
		{name: "string tokens", raw: `{"maxInputTokens":"10"}`, invalid: true},
		{name: "null tokens", raw: `{"maxInputTokens":null}`, invalid: true},
		{name: "negative tokens", raw: `{"maxInputTokens":-1}`, invalid: true},
		{name: "fractional tokens", raw: `{"maxOutputTokens":1.5}`, invalid: true},
		{name: "exponent tokens", raw: `{"maxOutputTokens":1e3}`, invalid: true},
		{name: "tokens overflow", raw: `{"maxInputTokens":9223372036854775808}`, invalid: true},
		{name: "negative cost", raw: `{"maxCostUSD":-0.5}`, invalid: true},
		{name: "cost overflow", raw: `{"maxCostUSD":1e300}`, invalid: true},
		{name: "boolean cost", raw: `{"maxCostUSD":true}`, invalid: true},
		{name: "trailing data", raw: `{"maxInputTokens":10} {}`, invalid: true},
		{name: "oversize", raw: `{"maxInputTokens":10` + strings.Repeat(" ", 4<<10) + `}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, err := store.CheckChildBudgetCeiling(tc.raw, used, reserve)
			if tc.invalid {
				if !errors.Is(err, store.ErrInvalidChildBudgetCeiling) || reason != "" {
					t.Fatalf("CheckChildBudgetCeiling(%q) = %q, %v; want ErrInvalidChildBudgetCeiling", tc.raw, reason, err)
				}
				return
			}
			if err != nil || reason != tc.reason {
				t.Fatalf("CheckChildBudgetCeiling(%q) = %q, %v; want %q", tc.raw, reason, err, tc.reason)
			}
		})
	}
}

func openBudgetStore(t *testing.T) (*sqlite.SQLiteStore, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "budget.db")
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := st.UpsertWorkload(t.Context(), domain.WorkloadRecord{Name: "worker"}); err != nil {
		t.Fatal(err)
	}
	return st, db
}

func countReservations(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM budget_reservations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReserveBudgetRefusals(t *testing.T) {
	st, db := openBudgetStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	if err := st.CreateTask(ctx, domain.Task{ID: "task", IdempotencyKey: "task", WorkloadName: "worker", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordBudgetUsage(ctx, domain.BudgetEntry{
		TaskID: "task", WorkloadName: "worker", InputTokens: 50, OutputTokens: 40, CostMicroUSD: 300_000, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	perTask := func(in, out int64, cost float64) domain.Budget {
		return domain.Budget{Spec: domain.BudgetSpec{PerTask: domain.PerTaskBudget{MaxInputTokens: in, MaxOutputTokens: out, MaxCostUSD: cost}}}
	}
	for _, tc := range []struct {
		name     string
		taskID   string
		workload string
		budget   domain.Budget
		reserve  domain.BudgetTotals
		reason   string
		err      error
		anyErr   bool
	}{
		{name: "per task input", budget: perTask(59, 0, 0), reserve: domain.BudgetTotals{InputTokens: 10}, reason: "per_task_input_tokens"},
		{name: "per task output", budget: perTask(100, 44, 0), reserve: domain.BudgetTotals{OutputTokens: 5}, reason: "per_task_output_tokens"},
		{name: "per task cost", budget: perTask(0, 0, 0.39), reserve: domain.BudgetTotals{CostMicroUSD: 100_000}, reason: "per_task_cost_usd"},
		{name: "daily cost", budget: domain.Budget{Spec: domain.BudgetSpec{Daily: domain.DailyBudget{MaxCostUSD: 0.3}}}, reason: "daily_cost_usd"},
		{name: "unknown task", taskID: "missing", err: store.ErrNotFound},
		{name: "task of another workload", workload: "other", err: store.ErrNotFound},
		{name: "negative input", reserve: domain.BudgetTotals{InputTokens: -1}, anyErr: true},
		{name: "negative output", reserve: domain.BudgetTotals{OutputTokens: -1}, anyErr: true},
		{name: "negative cost", reserve: domain.BudgetTotals{CostMicroUSD: -1}, anyErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := store.BudgetReservationRequest{TaskID: "task", WorkloadName: "worker", Budget: tc.budget, Reserve: tc.reserve, Now: now}
			if tc.taskID != "" {
				req.TaskID = tc.taskID
			}
			if tc.workload != "" {
				req.WorkloadName = tc.workload
			}
			res, err := st.ReserveBudget(ctx, req)
			switch {
			case tc.err != nil:
				if !errors.Is(err, tc.err) {
					t.Fatalf("ReserveBudget = %+v, %v; want %v", res, err, tc.err)
				}
			case tc.anyErr:
				if err == nil {
					t.Fatalf("ReserveBudget = %+v, want an error", res)
				}
			default:
				if err != nil || !res.Exceeded || res.Reason != tc.reason || res.ReservationID != "" {
					t.Fatalf("ReserveBudget = %+v, %v; want refusal %q", res, err, tc.reason)
				}
			}
			if n := countReservations(t, db); n != 0 {
				t.Fatalf("refused reservation left %d rows", n)
			}
		})
	}

	// Within every limit the reservation is granted and expires with its default TTL.
	res, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{
		TaskID: "task", WorkloadName: "worker", Budget: perTask(100, 100, 1), Reserve: domain.BudgetTotals{InputTokens: 49}, Now: now,
	})
	if err != nil || res.Exceeded || !strings.HasPrefix(res.ReservationID, "bres-") {
		t.Fatalf("grant = %+v, %v", res, err)
	}
	var expires string
	if err := db.QueryRowContext(ctx, "SELECT expires_at FROM budget_reservations WHERE id = $1", res.ReservationID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if want := now.Add(15 * time.Minute).Format(time.RFC3339Nano); expires != want {
		t.Fatalf("expires_at = %s, want %s", expires, want)
	}
	// The live reservation counts: 50 used plus 49 reserved leaves room for one token, not two.
	if again, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{
		TaskID: "task", WorkloadName: "worker", Budget: perTask(100, 100, 1), Reserve: domain.BudgetTotals{InputTokens: 2}, Now: now,
	}); err != nil || !again.Exceeded || again.Reason != "per_task_input_tokens" {
		t.Fatalf("reservation over a live reservation = %+v, %v", again, err)
	}
}

func TestSettleBudgetReservationIdempotent(t *testing.T) {
	st, db := openBudgetStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	if err := st.CreateTask(ctx, domain.Task{ID: "task", IdempotencyKey: "task", WorkloadName: "worker", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	res, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{TaskID: "task", WorkloadName: "worker", Reserve: domain.BudgetTotals{InputTokens: 7}, Now: now})
	if err != nil || res.Exceeded {
		t.Fatal(res, err)
	}
	actual := &domain.BudgetEntry{TaskID: "task", WorkloadName: "worker", Provider: "p", Model: "m", InputTokens: -3, OutputTokens: 4, CostMicroUSD: 9}
	if err := st.SettleBudgetReservation(ctx, res.ReservationID, actual); err != nil {
		t.Fatal(err)
	}
	totals, err := st.TaskBudgetTotals(ctx, "task")
	if err != nil {
		t.Fatal(err)
	}
	// Negative actual usage is clamped to zero, never credited back.
	if totals != (domain.BudgetTotals{InputTokens: 0, OutputTokens: 4, CostMicroUSD: 9}) {
		t.Fatalf("totals after settle = %+v", totals)
	}
	if err := st.SettleBudgetReservation(ctx, res.ReservationID, actual); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second settle with usage = %v, want ErrNotFound", err)
	}
	if err := st.SettleBudgetReservation(ctx, res.ReservationID, nil); err != nil {
		t.Fatalf("release of a settled reservation = %v, want nil", err)
	}
	if again, _ := st.TaskBudgetTotals(ctx, "task"); again != totals {
		t.Fatalf("totals changed on a repeated settle: %+v", again)
	}
	if n := countReservations(t, db); n != 0 {
		t.Fatalf("reservations left = %d", n)
	}
}

// The row-locking path is what PostgreSQL uses. SQLite runs it with a test locker.
func TestBudgetTxWithRowLocker(t *testing.T) {
	st, db := openBudgetStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	if err := st.CreateTask(ctx, domain.Task{ID: "task", IdempotencyKey: "task", WorkloadName: "worker", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	var locked [][2]string
	locker := func(_ context.Context, tx *sql.Tx, taskID, workload string) error {
		if tx == nil {
			return errors.New("no transaction")
		}
		locked = append(locked, [2]string{taskID, workload})
		return nil
	}
	errLock := errors.New("lock refused")
	failing := func(context.Context, *sql.Tx, string, string) error { return errLock }

	req := store.BudgetReservationRequest{TaskID: "task", WorkloadName: "worker", Reserve: domain.BudgetTotals{InputTokens: 2}, Now: now, TTL: time.Minute}
	if _, err := store.ReserveBudgetTx(ctx, db, failing, req); !errors.Is(err, errLock) {
		t.Fatalf("reserve with a failing locker = %v", err)
	}
	if n := countReservations(t, db); n != 0 {
		t.Fatalf("failed lock left %d reservations", n)
	}
	res, err := store.ReserveBudgetTx(ctx, db, locker, req)
	if err != nil || res.Exceeded {
		t.Fatal(res, err)
	}
	if len(locked) != 1 || locked[0] != [2]string{"task", "worker"} {
		t.Fatalf("locked rows = %v", locked)
	}

	if err := store.SettleBudgetReservationTx(ctx, db, failing, res.ReservationID, nil); !errors.Is(err, errLock) {
		t.Fatalf("settle with a failing locker = %v", err)
	}
	if n := countReservations(t, db); n != 1 {
		t.Fatalf("failed settle removed the reservation: %d rows", n)
	}
	if err := store.SettleBudgetReservationTx(ctx, db, locker, "bres-unknown", nil); err != nil {
		t.Fatalf("release of an unknown reservation = %v", err)
	}
	if err := store.SettleBudgetReservationTx(ctx, db, locker, "bres-unknown", &domain.BudgetEntry{TaskID: "task", WorkloadName: "worker"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("settle of an unknown reservation = %v", err)
	}
	if err := store.SettleBudgetReservationTx(ctx, db, locker, res.ReservationID, &domain.BudgetEntry{TaskID: "task", WorkloadName: "worker", InputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if len(locked) != 2 {
		t.Fatalf("settle did not lock the reservation rows: %v", locked)
	}
	if totals, err := st.TaskBudgetTotals(ctx, "task"); err != nil || totals.InputTokens != 1 {
		t.Fatalf("totals = %+v, %v", totals, err)
	}
}
