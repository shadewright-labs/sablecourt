package postgres

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// High 2: concurrent consumers of different approvals, with plain audit appends
// in between, leave one unbroken audit chain whatever isolation level the
// connection defaults to. A trigger slows each consumption between the
// approval row lock and the audit append, so a transaction whose snapshot
// predates the audit lock would chain onto a stale head.
func TestLivePostgresConcurrentConsumersKeepOneAuditChainIfConfigured(t *testing.T) {
	dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SABLECOURT_TEST_POSTGRES_DSN not set; skipping live Postgres integration test")
	}
	for _, level := range []string{"read committed", "repeatable read", "serializable"} {
		t.Run(level, func(t *testing.T) {
			u, err := url.Parse(dsn)
			if err != nil {
				t.Fatalf("SABLECOURT_TEST_POSTGRES_DSN must be a URL for this test: %v", err)
			}
			q := u.Query()
			q.Set("default_transaction_isolation", level)
			u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
			st := openFreshPostgres(t, u.String())
			ctx := context.Background()
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := st.db.QueryRowContext(ctx, `SHOW default_transaction_isolation`).Scan(&got); err != nil || got != level {
				t.Fatalf("connection isolation %q (%v), want %q", got, err, level)
			}
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateTask(ctx, domain.Task{ID: "task-chain", IdempotencyKey: "task-chain", WorkloadName: "triage-agent", State: domain.TaskStateWorking}); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendAudit(ctx, domain.AuditRecord{Category: "test", Action: "seed", Outcome: "ok", CreatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			const n = 6
			now := time.Now().UTC()
			for i := 0; i < n; i++ {
				id := "apr-chain-" + strconv.Itoa(i)
				if err := st.CreateApproval(ctx, domain.Approval{ID: id, TaskID: "task-chain", WorkloadName: "triage-agent", Capability: "action:git.open_pr", PayloadCanonicalJSON: `{}`, PayloadSHA256: "d1", ExpiresAt: now.Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
				if err := st.DecideApproval(ctx, id, "d1", domain.ApprovalApproved, "operator", `{}`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.db.ExecContext(ctx, `
				CREATE FUNCTION slow_consume() RETURNS trigger AS $$
				BEGIN PERFORM pg_sleep(0.2); RETURN NEW; END $$ LANGUAGE plpgsql;
				CREATE TRIGGER slow_consume BEFORE UPDATE ON approvals
				FOR EACH ROW WHEN (NEW.status = 'consumed') EXECUTE FUNCTION slow_consume();
			`); err != nil {
				t.Fatal(err)
			}

			var wg sync.WaitGroup
			errs := make(chan error, 2*n)
			for i := 0; i < n; i++ {
				wg.Add(2)
				go func(i int) {
					defer wg.Done()
					errs <- st.ConsumeApproval(ctx, store.ConsumeRequest{
						ID: "apr-chain-" + strconv.Itoa(i), TaskID: "task-chain", Capability: "action:git.open_pr", PayloadSHA256: "d1",
						Now:   func() time.Time { return time.Now().UTC() },
						Audit: domain.AuditRecord{Category: "approval", TaskID: "task-chain", Action: "action:git.open_pr", Outcome: "consumed"},
					})
				}(i)
				go func() {
					defer wg.Done()
					errs <- st.AppendAudit(ctx, domain.AuditRecord{Category: "test", Action: "between", Outcome: "ok", CreatedAt: time.Now().UTC()})
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Errorf("concurrent write: %v", err)
				}
			}
			var rows, heads int
			if err := st.db.QueryRowContext(ctx, `SELECT count(*), count(DISTINCT prev_hash) FROM audit_log`).Scan(&rows, &heads); err != nil {
				t.Fatal(err)
			}
			if rows != heads {
				t.Fatalf("audit chain forked: %d rows share %d previous hashes", rows, heads)
			}
			if _, err := st.VerifyAuditChain(ctx); err != nil {
				t.Fatalf("VerifyAuditChain: %v", err)
			}
		})
	}
}

// useAudit is a complete consumption audit record for store tests.
var useAudit = domain.AuditRecord{Category: "approval", Action: "action:git.open_pr", Outcome: "consumed"}

// Medium 4: PostgreSQL refuses a use without an audit record and leaves the
// approval unused.
func TestLivePostgresConsumeRequiresAuditIfConfigured(t *testing.T) {
	dsn := os.Getenv("SABLECOURT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SABLECOURT_TEST_POSTGRES_DSN not set; skipping live Postgres integration test")
	}
	st := openFreshPostgres(t, dsn)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "triage-agent", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "task-na", IdempotencyKey: "task-na", WorkloadName: "triage-agent", State: domain.TaskStateWorking}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateApproval(ctx, domain.Approval{ID: "apr-na", TaskID: "task-na", WorkloadName: "triage-agent", Capability: "action:git.open_pr", PayloadCanonicalJSON: `{}`, PayloadSHA256: "d1", ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.DecideApproval(ctx, "apr-na", "d1", domain.ApprovalApproved, "operator", `{}`); err != nil {
		t.Fatal(err)
	}
	err := st.ConsumeApproval(ctx, store.ConsumeRequest{ID: "apr-na", TaskID: "task-na", Capability: "action:git.open_pr", PayloadSHA256: "d1", Now: func() time.Time { return time.Now().UTC() }})
	if !errors.Is(err, store.ErrAuditRequired) {
		t.Fatalf("err %v, want ErrAuditRequired", err)
	}
	if got, _ := st.GetApproval(ctx, "apr-na"); got.Status != domain.ApprovalApproved {
		t.Fatalf("status %q, want approved", got.Status)
	}
}
