package store_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/postgres"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

func TestReconcileStore(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var st store.Store
			var err error
			var driver, dsn string
			if backend == "sqlite" {
				driver, dsn = "sqlite", filepath.Join(t.TempDir(), "state.db")
				st, err = sqlite.Open(dsn)
			} else {
				dsn = os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("SABLECOURT_TEST_POSTGRES_DSN is unset")
				}
				driver, dsn = "pgx", isolatedPostgresDSN(t, dsn)
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
			t.Run("candidates", func(t *testing.T) { testCandidates(t, st) })
			db, err := sql.Open(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			t.Run("renewal", func(t *testing.T) { testRenewal(t, st, db) })
			if backend == "sqlite" {
				t.Run("atomic_failure", func(t *testing.T) {
					createTask(t, st, "atomic", domain.TaskStateWorking)
					createLease(t, st, "atomic", time.Now().Add(time.Hour))
					if _, err := db.Exec("CREATE TRIGGER reject_revoke BEFORE UPDATE ON leases BEGIN SELECT RAISE(FAIL, 'unavailable'); END"); err != nil {
						t.Fatal(err)
					}
					err := st.UpdateTaskState(t.Context(), "atomic", domain.TaskStateWorking, domain.TaskStateFailed, "lost_on_restart", "{}")
					if err == nil {
						t.Fatal("committed failure without revocation")
					}
					task, err := st.GetTask(t.Context(), "atomic")
					if err != nil || task.State != domain.TaskStateWorking {
						t.Fatal(task.State, err)
					}
					events, err := st.ListTaskEvents(t.Context(), "atomic")
					if err != nil {
						t.Fatal(err)
					}
					for _, event := range events {
						if event.Reason == "lost_on_restart" {
							t.Fatal("failure event survived rollback")
						}
					}
					if _, err := db.Exec("DROP TRIGGER reject_revoke"); err != nil {
						t.Fatal(err)
					}
					if err := st.UpdateTaskState(t.Context(), "atomic", domain.TaskStateWorking, domain.TaskStateFailed, "lost_on_restart", "{}"); err != nil {
						t.Fatal(err)
					}
					if _, err := st.GetActiveLeaseByTokenHash(t.Context(), "atomic", time.Now()); !errors.Is(err, store.ErrNotFound) {
						t.Fatal("lease survived retry", err)
					}
				})
			}
		})
	}
}

func isolatedPostgresDSN(t *testing.T, dsn string) string {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	schema := pgx.Identifier{"reconcile_" + rand.Text()}.Sanitize()
	if _, err := db.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg.RuntimeParams["search_path"] = schema
	key := stdlib.RegisterConnConfig(cfg)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(key) })
	return key
}

func createTask(t *testing.T, st store.Store, id string, state domain.TaskState) {
	t.Helper()
	if err := st.CreateTask(t.Context(), domain.Task{ID: id, IdempotencyKey: id, WorkloadName: "worker", State: state}); err != nil {
		t.Fatal(err)
	}
}

func createLease(t *testing.T, st store.Store, taskID string, expiry time.Time) {
	t.Helper()
	if err := st.CreateLease(t.Context(), domain.Lease{
		TokenSHA256: taskID, TaskID: taskID, WorkloadName: "worker", ExpiresAt: expiry,
	}); err != nil {
		t.Fatal(err)
	}
}

func testCandidates(t *testing.T, st store.Store) {
	ctx := t.Context()
	for _, tc := range []struct {
		id     string
		state  domain.TaskState
		reason string
	}{
		{"queued", domain.TaskStateSubmitted, ""},
		{"running", domain.TaskStateWorking, ""},
		{"waiting", domain.TaskStateInputRequired, ""},
		{"suspended", domain.TaskStateSuspended, ""},
		{"advance", domain.TaskStateCompleted, "runtime_exit_observed"},
		{"remediate", domain.TaskStateFailed, "runtime_exit_observed"},
		{"cleanup", domain.TaskStateCancelled, "cancelled"},
		{"stop", domain.TaskStateFailed, "launch_failed"},
		{"history", domain.TaskStateCompleted, "runtime_completed"},
	} {
		createTask(t, st, tc.id, tc.state)
		if tc.reason != "" {
			if err := st.UpdateTaskState(ctx, tc.id, tc.state, tc.state, tc.reason, "{}"); err != nil {
				t.Fatal(err)
			}
		}
	}
	createLease(t, st, "cleanup", time.Now())
	check := func(handles, want []string) {
		t.Helper()
		tasks, err := st.ListReconcileTasks(ctx, handles)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, task := range tasks {
			ids = append(ids, task.ID)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, want) {
			t.Errorf("reconcile candidates = %v, want %v", ids, want)
		}
	}
	check([]string{"running", "stop"}, []string{"advance", "cleanup", "queued", "remediate", "running", "stop", "waiting"})
	if err := st.RevokeTaskLeases(ctx, "cleanup"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTaskState(ctx, "advance", domain.TaskStateCompleted, domain.TaskStateCompleted, "runtime_completed", "{}"); err != nil {
		t.Fatal(err)
	}
	check(nil, []string{"queued", "remediate", "running", "waiting"})
	if err := st.PutMemory(ctx, domain.MemoryEntry{Namespace: "system:runtime-handles", Key: "cleanup", Value: "{}", Author: "controlplane"}); err != nil {
		t.Fatal(err)
	}
	check(nil, []string{"cleanup", "queued", "remediate", "running", "waiting"})
}

func testRenewal(t *testing.T, st store.Store, db *sql.DB) {
	ctx := t.Context()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		id              string
		state           domain.TaskState
		old, next, want time.Duration
		revoked         bool
	}{
		{"extend", domain.TaskStateWorking, time.Nanosecond, time.Hour, time.Hour, false},
		{"await-input", domain.TaskStateInputRequired, time.Minute, time.Hour, time.Hour, false},
		{"keep-longer", domain.TaskStateWorking, 2 * time.Hour, time.Hour, 2 * time.Hour, false},
		{"at-expiry", domain.TaskStateWorking, 0, time.Hour, 0, false},
		{"already-expired", domain.TaskStateWorking, -time.Nanosecond, time.Hour, -time.Nanosecond, false},
		{"revoked", domain.TaskStateWorking, time.Minute, time.Hour, time.Minute, true},
		{"not-launched", domain.TaskStateSubmitted, time.Minute, time.Hour, time.Minute, false},
		{"paused", domain.TaskStateSuspended, time.Minute, time.Hour, time.Minute, false},
		{"finished", domain.TaskStateCompleted, time.Minute, time.Hour, time.Minute, false},
		{"cancelled", domain.TaskStateCancelled, time.Minute, time.Hour, time.Minute, false},
		{"failed", domain.TaskStateFailed, time.Minute, time.Hour, time.Minute, false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			createTask(t, st, tc.id, tc.state)
			createLease(t, st, tc.id, now.Add(tc.old))
			if tc.revoked {
				if err := st.RevokeTaskLeases(ctx, tc.id); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.RenewTaskLeases(ctx, tc.id, now, now.Add(tc.next)); err != nil {
				t.Fatal(err)
			}
			lease, err := st.GetActiveLeaseByTokenHash(ctx, tc.id, now.Add(-time.Minute))
			if tc.revoked {
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("revoked token lookup = %v", err)
				}
				var expiry string
				if err := db.QueryRowContext(ctx, "SELECT expires_at FROM leases WHERE token_sha256 = $1", tc.id).Scan(&expiry); err != nil {
					t.Fatal(err)
				}
				if want := now.Add(tc.want).Format(time.RFC3339Nano); expiry != want {
					t.Errorf("revoked lease expiry = %s, want %s", expiry, want)
				}
				return
			}
			if err != nil || !lease.ExpiresAt.Equal(now.Add(tc.want)) {
				t.Errorf("expiry = %v, err = %v, want %v", lease.ExpiresAt, err, now.Add(tc.want))
			}
		})
	}
}
