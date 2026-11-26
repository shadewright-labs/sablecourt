package mcpgate

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// recordingApprover approves every request and keeps the last one, so a test
// can see exactly what the dialog was built from.
type recordingApprover struct {
	mu   sync.Mutex
	last approval.ConfirmationRequest
}

func (r *recordingApprover) Confirm(_ context.Context, req approval.ConfirmationRequest) (approval.Proof, error) {
	r.mu.Lock()
	r.last = req
	r.mu.Unlock()
	return approval.Proof{Method: "test_oob", ConfirmedAt: time.Now().UTC(), Digest: req.PayloadSHA256}, nil
}

func (r *recordingApprover) lastRequest() approval.ConfirmationRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// recordingPROpener keeps every request it is asked to send and can be told to fail.
type recordingPROpener struct {
	mu   sync.Mutex
	sent []OpenPRInput
	fail atomic.Bool
}

func (p *recordingPROpener) OpenPR(_ context.Context, in OpenPRInput) (string, error) {
	p.mu.Lock()
	p.sent = append(p.sent, in)
	p.mu.Unlock()
	if p.fail.Load() {
		return "", errors.New("github unavailable")
	}
	return "https://github.com/" + in.Owner + "/" + in.Repo + "/pull/5", nil
}

func (p *recordingPROpener) calls() []OpenPRInput {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]OpenPRInput(nil), p.sent...)
}

type typedGateOptions struct {
	effect    domain.PolicyEffect
	workloads []string
	wrapStore func(store.Store) store.Store
	clock     *atomic.Pointer[time.Time]
	opener    PROpener // replaces the recording opener when set
}

type typedGate struct {
	gate     *Gate
	st       *sqlite.SQLiteStore
	dbPath   string
	queue    *approval.Queue
	approver *recordingApprover
	pr       *recordingPROpener
	repo     string
	tokens   map[string]string
}

// setupTypedGate wires the real GitOps catalog against a SQLite store, a repo
// root whose origin is github.com/acme/widgets, and one policy rule giving
// every workload the chosen effect on every action.
func setupTypedGate(t *testing.T, opts typedGateOptions) *typedGate {
	t.Helper()
	ctx := context.Background()
	if len(opts.workloads) == 0 {
		opts.workloads = []string{"ops-agent"}
	}
	repo := initGitRepo(t)
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "https://github.com/acme/widgets.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v (%s)", err, out)
	}
	dbPath := filepath.Join(t.TempDir(), "sablecourt.db")
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pol := domain.Policy{
		Metadata: domain.ObjectMeta{Name: "default"},
		Spec: domain.PolicySpec{DefaultEffect: domain.EffectDeny, Rules: []domain.PolicyRule{
			{Workload: "*", Capability: "action:*", Effect: opts.effect},
		}},
	}
	bundle := &config.Bundle{
		Policies: map[string]domain.Policy{"default": pol},
		Workloads: map[string]domain.Workload{
			"exp-sandbox": {Metadata: domain.ObjectMeta{Name: "exp-sandbox", Zone: domain.ZoneSandbox}},
		},
	}
	tokens := map[string]string{}
	for _, name := range opts.workloads {
		bundle.Workloads[name] = domain.Workload{
			Metadata: domain.ObjectMeta{Name: name, Zone: domain.ZoneRuntime},
			Spec: domain.WorkloadSpec{Class: domain.ClassLongLivedAgent, Grants: domain.GrantSet{
				Actions: []string{"git.create_branch", "git.open_pr", "sandbox.propose_promotion"},
			}},
		}
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: name, Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateTask(ctx, domain.Task{ID: "task-" + name, IdempotencyKey: "k-" + name, WorkloadName: name, State: domain.TaskStateWorking}); err != nil {
			t.Fatal(err)
		}
		tok, err := model.MintTaskToken(ctx, st, "task-"+name, name, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		tokens[name] = tok
	}
	approver := &recordingApprover{}
	queue := approval.NewQueue(st, approver)
	var gateStore store.Store = st
	if opts.wrapStore != nil {
		gateStore = opts.wrapStore(st)
	}
	var now func() time.Time
	if opts.clock != nil {
		now = func() time.Time { return *opts.clock.Load() }
	}
	gate := New(Options{Store: gateStore, Bundle: bundle, Policy: policy.NewEngine([]domain.Policy{pol}, nil), Approvals: queue, Now: now})
	pr := &recordingPROpener{}
	var opener PROpener = pr
	if opts.opener != nil {
		opener = opts.opener
	}
	RegisterBuiltinGitOps(gate, repo, opener)
	return &typedGate{gate: gate, st: st, dbPath: dbPath, queue: queue, approver: approver, pr: pr, repo: repo, tokens: tokens}
}

func (tg *typedGate) post(workload, action, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://sablecourt.local/"+action, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tg.tokens[workload])
	rec := httptest.NewRecorder()
	tg.gate.ActionHandler().ServeHTTP(rec, req)
	return rec
}

func (tg *typedGate) approvals(t *testing.T) []domain.Approval {
	t.Helper()
	list, err := tg.st.ListApprovals(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func (tg *typedGate) rawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", tg.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		t.Fatal(err)
	}
	return db
}

func (tg *typedGate) setOrigin(t *testing.T, url string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", tg.repo, "remote", "set-url", "origin", url).CombinedOutput(); err != nil {
		t.Fatalf("git remote set-url: %v (%s)", err, out)
	}
}
