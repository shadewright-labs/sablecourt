package store_test

import (
	"errors"
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
)

func TestBudgetReservationAndSettlementStore(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var st store.Store
			var err error
			if backend == "sqlite" {
				st, err = sqlite.Open(filepath.Join(t.TempDir(), "budget.db"))
			} else {
				dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("SABLECOURT_TEST_POSTGRES_DSN is unset")
				}
				st, err = postgres.Open(isolatedPostgresDSN(t, dsn))
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			ctx := t.Context()
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}

			t.Run("concurrent_child_reservation", func(t *testing.T) {
				testConcurrentChildReservation(t, st)
			})
			t.Run("settle_release_and_expiry", func(t *testing.T) {
				testSettleReleaseAndExpiry(t, st)
			})
			t.Run("daily_budget_across_children", func(t *testing.T) {
				testDailyBudgetAcrossChildren(t, st)
			})
		})
	}
}

func testConcurrentChildReservation(t *testing.T, st store.Store) {
	ctx := t.Context()
	const wl = "worker-conc"
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: wl}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "parent-conc", IdempotencyKey: "parent-conc", WorkloadName: wl, State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	child := domain.Task{
		ID:                "child-conc",
		ParentTaskID:      "parent-conc",
		IdempotencyKey:    "child-conc",
		WorkloadName:      wl,
		State:             domain.TaskStateWorking,
		BudgetCeilingJSON: `{"maxInputTokens":10,"maxOutputTokens":8,"maxCostUSD":0.0001}`,
	}
	if err := st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}

	budget := domain.Budget{
		Spec: domain.BudgetSpec{
			PerTask: domain.PerTaskBudget{MaxInputTokens: 1000, MaxOutputTokens: 1000, MaxCostUSD: 1},
			Daily:   domain.DailyBudget{MaxCostUSD: 5},
		},
	}

	const workers = 16
	start := make(chan struct{})
	var granted atomic.Int32
	var exceeded atomic.Int32
	resIDs := make(chan string, workers)
	var wg sync.WaitGroup
	now := time.Now().UTC()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{
				TaskID:       child.ID,
				WorkloadName: wl,
				Budget:       budget,
				Reserve:      domain.BudgetTotals{InputTokens: 3, OutputTokens: 2, CostMicroUSD: 10},
				Now:          now,
			})
			if err != nil {
				t.Errorf("ReserveBudget: %v", err)
				return
			}
			if res.Exceeded {
				exceeded.Add(1)
				return
			}
			granted.Add(1)
			resIDs <- res.ReservationID
		}()
	}
	close(start)
	wg.Wait()
	close(resIDs)

	// maxInputTokens=10 with reserve=3 allows at most 3 reservations (9 <= 10, 12 > 10).
	if got := granted.Load(); got != 3 {
		t.Fatalf("granted reservations = %d, want 3 (exceeded=%d)", got, exceeded.Load())
	}
	if got := exceeded.Load(); got != workers-3 {
		t.Fatalf("exceeded reservations = %d, want %d", got, workers-3)
	}

	// Settle all 3 reservations to 1 input token each (3 total used).
	for id := range resIDs {
		if err := st.SettleBudgetReservation(ctx, id, &domain.BudgetEntry{
			TaskID:       child.ID,
			WorkloadName: wl,
			Provider:     "openai",
			Model:        "gpt-5",
			InputTokens:  1,
			OutputTokens: 1,
			CostMicroUSD: 10,
			CreatedAt:    now,
		}); err != nil {
			t.Fatalf("SettleBudgetReservation: %v", err)
		}
	}

	// After settlement freed unused headroom (3 used < 10), another reserve=3 fits.
	res, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{
		TaskID:       child.ID,
		WorkloadName: wl,
		Budget:       budget,
		Reserve:      domain.BudgetTotals{InputTokens: 3, OutputTokens: 1, CostMicroUSD: 10},
		Now:          now,
	})
	if err != nil || res.Exceeded {
		t.Fatalf("post-settle reservation = %+v, err=%v", res, err)
	}
	if err := st.SettleBudgetReservation(ctx, res.ReservationID, nil); err != nil {
		t.Fatalf("cleanup reserve: %v", err)
	}
}

func testSettleReleaseAndExpiry(t *testing.T, st store.Store) {
	ctx := t.Context()
	const wl = "worker-rel"
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: wl}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "parent-rel", IdempotencyKey: "parent-rel", WorkloadName: wl, State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	child := domain.Task{
		ID:                "child-rel",
		ParentTaskID:      "parent-rel",
		IdempotencyKey:    "child-rel",
		WorkloadName:      wl,
		State:             domain.TaskStateWorking,
		BudgetCeilingJSON: `{"maxInputTokens":5}`,
	}
	if err := st.CreateTask(ctx, child); err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	res, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{
		TaskID:       child.ID,
		WorkloadName: wl,
		Reserve:      domain.BudgetTotals{InputTokens: 5, OutputTokens: 1, CostMicroUSD: 1},
		Now:          t0,
		TTL:          time.Minute,
	})
	if err != nil || res.Exceeded {
		t.Fatalf("initial reserve = %+v, err=%v", res, err)
	}

	// Release without committing usage.
	if err := st.SettleBudgetReservation(ctx, res.ReservationID, nil); err != nil {
		t.Fatalf("release reserve: %v", err)
	}
	// Duplicate settle with actual must fail with ErrNotFound so usage cannot double-post.
	if err := st.SettleBudgetReservation(ctx, res.ReservationID, &domain.BudgetEntry{
		TaskID: child.ID, WorkloadName: wl, InputTokens: 5,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("duplicate settle err = %v, want ErrNotFound", err)
	}

	// Reserve again, let it expire, and verify a new reservation succeeds after TTL.
	res2, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{
		TaskID:       child.ID,
		WorkloadName: wl,
		Reserve:      domain.BudgetTotals{InputTokens: 5, OutputTokens: 1, CostMicroUSD: 1},
		Now:          t0,
		TTL:          time.Minute,
	})
	if err != nil || res2.Exceeded {
		t.Fatalf("second reserve = %+v, err=%v", res2, err)
	}
	res3, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{
		TaskID:       child.ID,
		WorkloadName: wl,
		Reserve:      domain.BudgetTotals{InputTokens: 5, OutputTokens: 1, CostMicroUSD: 1},
		Now:          t0.Add(time.Minute),
		TTL:          time.Minute,
	})
	if err != nil || res3.Exceeded {
		t.Fatalf("post-expiry reserve = %+v, err=%v", res3, err)
	}
	if err := st.SettleBudgetReservation(ctx, res3.ReservationID, nil); err != nil {
		t.Fatalf("cleanup reserve: %v", err)
	}

	// Malformed child ceiling must fail closed without inserting a reservation.
	badChild := domain.Task{
		ID:                "child-bad",
		ParentTaskID:      "parent-rel",
		IdempotencyKey:    "child-bad",
		WorkloadName:      wl,
		State:             domain.TaskStateWorking,
		BudgetCeilingJSON: `{}`,
	}
	if err := st.CreateTask(ctx, badChild); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{
		TaskID:       badChild.ID,
		WorkloadName: wl,
		Reserve:      domain.BudgetTotals{InputTokens: 1, OutputTokens: 1, CostMicroUSD: 1},
		Now:          t0,
	}); !errors.Is(err, store.ErrInvalidChildBudgetCeiling) {
		t.Fatalf("bad child ceiling err = %v, want ErrInvalidChildBudgetCeiling", err)
	}
}

func testDailyBudgetAcrossChildren(t *testing.T, st store.Store) {
	ctx := t.Context()
	const wl = "worker-day"
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: wl}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "parent-day", IdempotencyKey: "parent-day", WorkloadName: wl, State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"child-day-1", "child-day-2"} {
		if err := st.CreateTask(ctx, domain.Task{
			ID: id, ParentTaskID: "parent-day", IdempotencyKey: id,
			WorkloadName: wl, State: domain.TaskStateWorking,
			BudgetCeilingJSON: `{"maxCostUSD":10}`,
		}); err != nil {
			t.Fatal(err)
		}
	}

	budget := domain.Budget{
		Spec: domain.BudgetSpec{
			Daily: domain.DailyBudget{MaxCostUSD: 0.000025}, // 25 microUSD
		},
	}
	now := time.Now().UTC()
	r1, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{
		TaskID: "child-day-1", WorkloadName: wl, Budget: budget,
		Reserve: domain.BudgetTotals{InputTokens: 1, OutputTokens: 1, CostMicroUSD: 15},
		Now:     now,
	})
	if err != nil || r1.Exceeded {
		t.Fatalf("child-day-1 reserve = %+v, err=%v", r1, err)
	}
	r2, err := st.ReserveBudget(ctx, store.BudgetReservationRequest{
		TaskID: "child-day-2", WorkloadName: wl, Budget: budget,
		Reserve: domain.BudgetTotals{InputTokens: 1, OutputTokens: 1, CostMicroUSD: 15},
		Now:     now,
	})
	if err != nil || !r2.Exceeded || r2.Reason != "daily_cost_usd" {
		t.Fatalf("child-day-2 reserve = %+v, err=%v, want daily_cost_usd exceeded", r2, err)
	}
	if err := st.SettleBudgetReservation(ctx, r1.ReservationID, nil); err != nil {
		t.Fatalf("release r1: %v", err)
	}
}
