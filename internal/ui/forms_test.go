package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/approval"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/providers"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// failingStore makes one named store call fail so handlers show their error path.
type failingStore struct {
	store.Store
	fail string
}

var errStoreDown = errors.New("store down")

func (s failingStore) ListWorkloads(ctx context.Context) ([]domain.WorkloadRecord, error) {
	if s.fail == "workloads" {
		return nil, errStoreDown
	}
	return s.Store.ListWorkloads(ctx)
}

func (s failingStore) ListApprovals(ctx context.Context, status domain.ApprovalStatus) ([]domain.Approval, error) {
	if s.fail == "approvals" {
		return nil, errStoreDown
	}
	return s.Store.ListApprovals(ctx, status)
}

func (s failingStore) ListTasks(ctx context.Context) ([]domain.Task, error) {
	if s.fail == "tasks" {
		return nil, errStoreDown
	}
	return s.Store.ListTasks(ctx)
}

func (s failingStore) ListAudit(ctx context.Context, limit int) ([]domain.AuditRecord, error) {
	if s.fail == "audit" {
		return nil, errStoreDown
	}
	return s.Store.ListAudit(ctx, limit)
}

func (s failingStore) TotalBudgetTotals(ctx context.Context) (domain.BudgetTotals, error) {
	if s.fail == "budget" {
		return domain.BudgetTotals{}, errStoreDown
	}
	return s.Store.TotalBudgetTotals(ctx)
}

func (s failingStore) VerifyAuditChain(ctx context.Context, anchor *domain.AuditAnchor) (domain.AuditChainHead, error) {
	if s.fail == "chain" {
		return domain.AuditChainHead{Rows: 3}, errors.New("row 4 hash mismatch")
	}
	return s.Store.VerifyAuditChain(ctx, anchor)
}

func (s failingStore) PutMemory(ctx context.Context, e domain.MemoryEntry) error {
	if s.fail == "putmemory" {
		return errStoreDown
	}
	return s.Store.PutMemory(ctx, e)
}

func (s failingStore) DeleteMemory(ctx context.Context, ns, key string) error {
	if s.fail == "deletememory" {
		return errStoreDown
	}
	return s.Store.DeleteMemory(ctx, ns, key)
}

// seedWorkload stores the workload record that tasks reference.
func seedWorkload(t *testing.T, st store.Store, name string) {
	t.Helper()
	if err := st.UpsertWorkload(context.Background(), domain.WorkloadRecord{Name: name, Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatalf("UpsertWorkload: %v", err)
	}
}

func newFormMux(t *testing.T, opts Options) *http.ServeMux {
	t.Helper()
	dash, err := NewDashboard(opts)
	if err != nil {
		t.Fatalf("NewDashboard: %v", err)
	}
	mux := http.NewServeMux()
	dash.RegisterRoutes(mux)
	return mux
}

func postForm(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getPage(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// redirectTarget returns the path and the notice and err query values of a 303 response.
func redirectTarget(t *testing.T, rec *httptest.ResponseRecorder) (string, string, string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body %q", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return u.Path, u.Query().Get("notice"), u.Query().Get("err")
}

func TestDashboardStoreErrorsReturn500(t *testing.T) {
	for _, tc := range []struct{ fail, want string }{
		{"workloads", "load workloads: store down"},
		{"approvals", "load approvals: store down"},
		{"tasks", "load tasks: store down"},
		{"audit", "load audit: store down"},
		{"budget", "load budget totals: store down"},
	} {
		t.Run(tc.fail, func(t *testing.T) {
			mux := newFormMux(t, Options{Store: failingStore{Store: newUITestStore(t), fail: tc.fail}})
			rec := getPage(t, mux, "/")
			if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("got %d %q, want 500 with %q", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

func TestDashboardFiltersTasksAndShowsFacts(t *testing.T) {
	ctx := context.Background()
	st := newUITestStore(t)
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	seedWorkload(t, st, "coder")
	tasks := []domain.Task{
		{ID: "task_working_stale", State: domain.TaskStateWorking},
		{ID: "task_working_fresh", State: domain.TaskStateWorking},
		{ID: "task_done", State: domain.TaskStateCompleted},
		{ID: "task_broken", State: domain.TaskStateFailed},
		{ID: "task_paused", State: domain.TaskStateSuspended},
	}
	for i, task := range tasks {
		task.WorkloadName = "coder"
		task.IdempotencyKey = task.ID
		task.InputJSON = "{}"
		task.CreatedAt = now.Add(time.Duration(i) * time.Second)
		task.UpdatedAt = task.CreatedAt
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask %s: %v", task.ID, err)
		}
	}
	// CreateTask does not keep heartbeats; record them as the reconciler does.
	for id, at := range map[string]time.Time{"task_working_stale": old, "task_working_fresh": now} {
		if err := st.RecordHeartbeat(ctx, id, at); err != nil {
			t.Fatal(err)
		}
	}
	long := strings.Repeat("r", 400)
	for _, m := range []domain.TaskMessage{
		{TaskID: "task_done", Role: "agent", Content: long, CreatedAt: now},
		{TaskID: "task_done", Role: "user", Content: "user follow-up after reply", CreatedAt: now.Add(time.Second)},
	} {
		if err := st.AppendTaskMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordBudgetUsage(ctx, domain.BudgetEntry{TaskID: "task_done", WorkloadName: "coder", InputTokens: 100, OutputTokens: 23, CostMicroUSD: 1_500_000, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	mux := newFormMux(t, Options{Store: st, StalenessWindow: time.Minute})

	cases := []struct {
		query     string
		present   []string
		absent    []string
		activeTab string
	}{
		{"/", []string{"task_working_stale", "task_done", "task_broken", "task_paused"}, nil, "all"},
		{"/?state=active", []string{"task_working_stale", "task_working_fresh"}, []string{"task_done", "task_broken", "task_paused"}, "active"},
		{"/?state=completed", []string{"task_done"}, []string{"task_working_fresh", "task_broken"}, "completed"},
		{"/?state=failed", []string{"task_broken"}, []string{"task_done", "task_working_fresh"}, "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			rec := getPage(t, mux, tc.query)
			body := rec.Body.String()
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			for _, s := range tc.present {
				if !strings.Contains(body, `href="/tasks/`+s+`"`) {
					t.Errorf("missing %s", s)
				}
			}
			for _, s := range tc.absent {
				if strings.Contains(body, `href="/tasks/`+s+`"`) {
					t.Errorf("unexpected %s", s)
				}
			}
			if !strings.Contains(body, `sc-tab active" href="/?state=`+tc.activeTab+`"`) {
				t.Errorf("tab %s not marked active", tc.activeTab)
			}
		})
	}

	body := getPage(t, mux, "/?notice=saved+ok&err=went+wrong").Body.String()
	for _, want := range []string{
		`sc-banner-ok">saved ok`,
		`sc-banner-err">went wrong`,
		"$1.5000",
		"123 tokens metered",
		strings.Repeat("r", 360) + "...",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
	if strings.Contains(body, strings.Repeat("r", 361)) {
		t.Error("last reply not truncated at 360 characters")
	}
	if strings.Count(body, "sc-glyph-stale\"></i>stale") != 1 {
		t.Errorf("want exactly one stale badge (the old heartbeat), body has %d", strings.Count(body, "sc-glyph-stale\"></i>stale"))
	}
}

func TestSubmitTaskForm(t *testing.T) {
	st := newUITestStore(t)
	_, _, bundle, _, _ := seedStudioManifests(t, st)
	rec := controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})

	t.Run("no reconciler", func(t *testing.T) {
		mux := newFormMux(t, Options{Store: st})
		path, _, errMsg := redirectTarget(t, postForm(t, mux, "/ui/tasks", url.Values{"workload": {"claude-worker"}}))
		if path != "/" || errMsg != "reconciler not configured" {
			t.Fatalf("got %s err=%q", path, errMsg)
		}
	})

	mux := newFormMux(t, Options{Store: st, Reconciler: rec})
	t.Run("unknown workload refused", func(t *testing.T) {
		_, notice, errMsg := redirectTarget(t, postForm(t, mux, "/ui/tasks", url.Values{"workload": {"ghost"}}))
		if notice != "" || errMsg != `unknown workload "ghost"` {
			t.Fatalf("notice=%q err=%q", notice, errMsg)
		}
	})

	for _, tc := range []struct {
		name  string
		form  url.Values
		input map[string]any
	}{
		{"json input with provider and model", url.Values{"workload": {"claude-worker"}, "input_json": {`{"prompt":"fix it","n":2}`}, "provider": {" anthropic "}, "model": {" small "}},
			map[string]any{"prompt": "fix it", "n": float64(2), "provider": "anthropic", "model": "small"}},
		{"plain text becomes prompt", url.Values{"workload": {"codex-worker"}, "input_json": {"just words"}},
			map[string]any{"prompt": "just words"}},
		{"empty input", url.Values{"workload": {"agy-worker"}}, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, notice, errMsg := redirectTarget(t, postForm(t, mux, "/ui/tasks", tc.form))
			if errMsg != "" || !strings.HasPrefix(notice, "Submitted task task_") || !strings.HasSuffix(notice, " for "+tc.form.Get("workload")) {
				t.Fatalf("notice=%q err=%q", notice, errMsg)
			}
			id := strings.Fields(notice)[2]
			task, err := st.GetTask(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(task.InputJSON), &got); err != nil {
				t.Fatalf("stored input %q: %v", task.InputJSON, err)
			}
			if len(got) != len(tc.input) {
				t.Fatalf("stored input %v, want %v", got, tc.input)
			}
			for k, v := range tc.input {
				if got[k] != v {
					t.Fatalf("stored input %v, want %v", got, tc.input)
				}
			}
			if task.WorkloadName != tc.form.Get("workload") || task.State != domain.TaskStateSubmitted {
				t.Fatalf("task %+v", task)
			}
		})
	}
}

type fakeApprover struct {
	digest string
	err    error
}

func (f fakeApprover) Confirm(_ context.Context, req approval.ConfirmationRequest) (approval.Proof, error) {
	if f.err != nil {
		return approval.Proof{}, f.err
	}
	d := f.digest
	if d == "" {
		d = req.PayloadSHA256
	}
	return approval.Proof{Method: "test", ConfirmedAt: time.Now().UTC(), Digest: d}, nil
}

func TestApproveAndDenyForms(t *testing.T) {
	ctx := context.Background()
	newPending := func(t *testing.T, st store.Store, q *approval.Queue) domain.Approval {
		t.Helper()
		seedWorkload(t, st, "coder")
		now := time.Now().UTC()
		if err := st.CreateTask(ctx, domain.Task{ID: "task_apr", IdempotencyKey: "task_apr", WorkloadName: "coder", State: domain.TaskStateWorking, InputJSON: "{}", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		apr, err := q.Request(ctx, "task_apr", "coder", "git.push", map[string]string{"branch": "main"}, time.Hour)
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		return apr
	}

	t.Run("no queue", func(t *testing.T) {
		mux := newFormMux(t, Options{Store: newUITestStore(t)})
		for _, p := range []string{"/ui/approvals/apr_1/approve", "/ui/approvals/apr_1/deny"} {
			_, _, errMsg := redirectTarget(t, postForm(t, mux, p, nil))
			if errMsg != "approval queue unavailable" {
				t.Fatalf("%s err=%q", p, errMsg)
			}
		}
	})

	for _, tc := range []struct {
		name       string
		approver   approval.Approver
		expected   func(apr domain.Approval) string
		wantErr    string
		wantStatus domain.ApprovalStatus
	}{
		{"no approver refused", nil, func(domain.Approval) string { return "" }, "Approval failed: no out-of-band approver configured", domain.ApprovalPending},
		{"digest mismatch refused", fakeApprover{}, func(domain.Approval) string { return strings.Repeat("0", 64) }, "Approval failed: " + approval.ErrDigestMismatch.Error(), domain.ApprovalPending},
		{"confirmation declined", fakeApprover{err: errors.New("cancelled")}, func(a domain.Approval) string { return a.PayloadSHA256 }, "Approval failed: " + approval.ErrNotConfirmed.Error() + ": cancelled", domain.ApprovalPending},
		{"approved with pinned digest", fakeApprover{}, func(a domain.Approval) string { return a.PayloadSHA256 }, "", domain.ApprovalApproved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newUITestStore(t)
			q := approval.NewQueue(st, tc.approver)
			apr := newPending(t, st, q)
			mux := newFormMux(t, Options{Store: st, Approvals: q})
			_, notice, errMsg := redirectTarget(t, postForm(t, mux, "/ui/approvals/"+apr.ID+"/approve", url.Values{"expected_sha256": {tc.expected(apr)}}))
			if errMsg != tc.wantErr {
				t.Fatalf("err=%q, want %q", errMsg, tc.wantErr)
			}
			if tc.wantErr == "" && notice != "Approved "+apr.ID+" via out-of-band confirmation" {
				t.Fatalf("notice=%q", notice)
			}
			got, err := st.GetApproval(ctx, apr.ID)
			if err != nil || got.Status != tc.wantStatus {
				t.Fatalf("status=%q err=%v, want %q", got.Status, err, tc.wantStatus)
			}
			if tc.wantStatus == domain.ApprovalApproved && got.DecidedBy != "operator-ui" {
				t.Fatalf("decided by %q", got.DecidedBy)
			}
		})
	}

	t.Run("deny", func(t *testing.T) {
		st := newUITestStore(t)
		q := approval.NewQueue(st, nil)
		apr := newPending(t, st, q)
		mux := newFormMux(t, Options{Store: st, Approvals: q})
		_, notice, _ := redirectTarget(t, postForm(t, mux, "/ui/approvals/"+apr.ID+"/deny", nil))
		if notice != "Denied "+apr.ID {
			t.Fatalf("notice=%q", notice)
		}
		got, _ := st.GetApproval(ctx, apr.ID)
		if got.Status != domain.ApprovalDenied {
			t.Fatalf("status %q", got.Status)
		}
		_, _, errMsg := redirectTarget(t, postForm(t, mux, "/ui/approvals/apr_missing/deny", nil))
		if !strings.HasPrefix(errMsg, "Deny failed: ") {
			t.Fatalf("err=%q", errMsg)
		}
	})
}

func TestTaskControlsWithoutReconcilerOrTask(t *testing.T) {
	st := newUITestStore(t)
	bare := newFormMux(t, Options{Store: st})
	withRec := newFormMux(t, Options{Store: st, Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st})})
	for _, action := range []string{"cancel", "suspend", "resume"} {
		t.Run(action, func(t *testing.T) {
			_, _, errMsg := redirectTarget(t, postForm(t, bare, "/ui/tasks/task_x/"+action, nil))
			if errMsg != "reconciler not configured" {
				t.Fatalf("bare err=%q", errMsg)
			}
			_, notice, errMsg := redirectTarget(t, postForm(t, withRec, "/ui/tasks/task_missing/"+action, nil))
			title := strings.ToUpper(action[:1]) + action[1:]
			if notice != "" || !strings.HasPrefix(errMsg, title+" failed: ") {
				t.Fatalf("missing task notice=%q err=%q", notice, errMsg)
			}
		})
	}
	t.Run("run workflow", func(t *testing.T) {
		path, _, errMsg := redirectTarget(t, postForm(t, bare, "/ui/workflows/wf/run", nil))
		if path != "/studio" || errMsg != "reconciler unavailable" {
			t.Fatalf("%s err=%q", path, errMsg)
		}
		path, _, errMsg = redirectTarget(t, postForm(t, withRec, "/ui/workflows/wf/run", nil))
		if path != "/studio" || errMsg != "no manifest bundle loaded" {
			t.Fatalf("%s err=%q", path, errMsg)
		}
	})
}

func TestProviderFormsWithoutRegistry(t *testing.T) {
	mux := newFormMux(t, Options{Store: newUITestStore(t)})
	for _, p := range []string{"connect", "disconnect", "sync", "models"} {
		t.Run(p, func(t *testing.T) {
			path, _, errMsg := redirectTarget(t, postForm(t, mux, "/ui/providers/openai/"+p, url.Values{"api_key": {"x"}}))
			if path != "/providers" || errMsg != "provider registry unavailable" {
				t.Fatalf("%s err=%q", path, errMsg)
			}
		})
	}
}

func TestTaskDetailPage(t *testing.T) {
	ctx := context.Background()
	st := newUITestStore(t)
	mux := newFormMux(t, Options{Store: st})
	if rec := getPage(t, mux, "/tasks/task_missing"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "task not found") {
		t.Fatalf("missing task: %d %q", rec.Code, rec.Body.String())
	}
	now := time.Now().UTC()
	seedWorkload(t, st, "coder")
	if err := st.CreateTask(ctx, domain.Task{ID: "task_detail", IdempotencyKey: "k", WorkloadName: "coder", State: domain.TaskStateSubmitted, InputJSON: "{}", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendTaskMessage(ctx, domain.TaskMessage{TaskID: "task_detail", Role: "agent", Content: "agent reply visible here", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordBudgetUsage(ctx, domain.BudgetEntry{TaskID: "task_detail", WorkloadName: "coder", CostMicroUSD: 250_000, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	rec := getPage(t, mux, "/tasks/task_detail")
	body := rec.Body.String()
	for _, want := range []string{"task_detail", "agent reply visible here", "0.2500", "/ui/tasks/task_detail/cancel"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page lacks %q", want)
		}
	}
}

func TestAuditPageFiltersAndChainState(t *testing.T) {
	ctx := context.Background()
	st := newUITestStore(t)
	now := time.Now().UTC()
	for _, r := range []domain.AuditRecord{
		{Category: "policy", WorkloadName: "coder", Action: "git.push", Outcome: "denied", DetailJSON: `{"m":"one"}`, CreatedAt: now},
		{Category: "policy", WorkloadName: "triage", Action: "net.fetch", Outcome: "allowed", DetailJSON: `{"m":"two"}`, CreatedAt: now},
		{Category: "approval", WorkloadName: "coder", Action: "deploy.run", Outcome: "paused", DetailJSON: `{"m":"three"}`, CreatedAt: now},
	} {
		if err := st.AppendAudit(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	head, err := st.VerifyAuditChain(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := newFormMux(t, Options{Store: st})
	for _, tc := range []struct {
		query   string
		present []string
		absent  []string
	}{
		{"", []string{"git.push", "net.fetch", "deploy.run", "Audit Tribunal (3)"}, nil},
		{"?category=policy", []string{"git.push", "net.fetch", "Audit Tribunal (2)"}, []string{"deploy.run"}},
		{"?workload=coder", []string{"git.push", "deploy.run"}, []string{"net.fetch"}},
		{"?outcome=denied", []string{"git.push", "out-deny", "Audit Tribunal (1)"}, []string{"net.fetch", "deploy.run"}},
		{"?category=approval&workload=triage", []string{"Audit Tribunal (0)"}, []string{"git.push", "net.fetch", "deploy.run"}},
	} {
		t.Run("filter"+tc.query, func(t *testing.T) {
			body := getPage(t, mux, "/audit"+tc.query).Body.String()
			for _, s := range tc.present {
				if !strings.Contains(body, s) {
					t.Errorf("missing %q", s)
				}
			}
			for _, s := range tc.absent {
				if strings.Contains(body, s) {
					t.Errorf("unexpected %q", s)
				}
			}
			if !strings.Contains(body, "SHA-256 chain verified (3 rows)") {
				t.Error("chain not shown as verified")
			}
			if !strings.Contains(body, "head 3:"+head.Hash) {
				t.Error("chain head not shown")
			}
		})
	}
	t.Run("broken chain", func(t *testing.T) {
		body := getPage(t, newFormMux(t, Options{Store: failingStore{Store: st, fail: "chain"}}), "/audit").Body.String()
		if !strings.Contains(body, "SHA-256 chain broken: row 4 hash mismatch") || strings.Contains(body, "chain verified") || strings.Contains(body, "Record this head") {
			t.Fatalf("broken chain not shown")
		}
	})
}

func TestMemoryForms(t *testing.T) {
	ctx := context.Background()
	secret := "sk-" + strings.Repeat("a1B2c3D4", 6)
	for _, tc := range []struct {
		name       string
		path       string
		form       url.Values
		fail       string
		wantCode   int
		wantErr    string
		wantNotice string
	}{
		{"set learning namespace forbidden", "/ui/memory/set", url.Values{"scope": {" Learning:global "}, "key": {"k"}, "value": {"v"}}, "", http.StatusForbidden, "", ""},
		{"set missing value", "/ui/memory/set", url.Values{"scope": {"global"}, "key": {"k"}}, "", http.StatusSeeOther, "key and value are required", ""},
		{"set missing key", "/ui/memory/set", url.Values{"value": {"v"}}, "", http.StatusSeeOther, "key and value are required", ""},
		{"set store error", "/ui/memory/set", url.Values{"key": {"k"}, "value": {"v"}}, "putmemory", http.StatusSeeOther, "store down", ""},
		{"set default scope", "/ui/memory/set", url.Values{"key": {" note "}, "value": {"token " + secret}}, "", http.StatusSeeOther, "", "Saved memory global/note"},
		{"delete learning namespace forbidden", "/ui/memory/delete", url.Values{"scope": {"learning:repo:x"}, "key": {"k"}}, "", http.StatusForbidden, "", ""},
		{"delete missing scope", "/ui/memory/delete", url.Values{"key": {"k"}}, "", http.StatusSeeOther, "scope and key are required", ""},
		{"delete store error", "/ui/memory/delete", url.Values{"scope": {"global"}, "key": {"k"}}, "deletememory", http.StatusSeeOther, "store down", ""},
		{"delete ok", "/ui/memory/delete", url.Values{"scope": {"global"}, "key": {"k"}}, "", http.StatusSeeOther, "", "Deleted memory global/k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newUITestStore(t)
			if err := st.PutMemory(ctx, domain.MemoryEntry{Namespace: "learning:global", Key: "k", Value: "kept", Author: "operator", UpdatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if err := st.PutMemory(ctx, domain.MemoryEntry{Namespace: "learning:repo:x", Key: "k", Value: "kept", Author: "operator", UpdatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if err := st.PutMemory(ctx, domain.MemoryEntry{Namespace: "global", Key: "k", Value: "old", Author: "operator", UpdatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			mux := newFormMux(t, Options{Store: failingStore{Store: st, fail: tc.fail}})
			rec := postForm(t, mux, tc.path, tc.form)
			if rec.Code != tc.wantCode {
				t.Fatalf("status %d, want %d", rec.Code, tc.wantCode)
			}
			if tc.wantCode == http.StatusForbidden {
				if !strings.Contains(rec.Body.String(), "learning namespace is reserved") {
					t.Fatalf("body %q", rec.Body.String())
				}
				for _, ns := range []string{"learning:global", "learning:repo:x"} {
					if e, err := st.GetMemory(ctx, ns, "k"); err != nil || e.Value != "kept" {
						t.Fatalf("learning entry %s changed: %+v %v", ns, e, err)
					}
				}
				return
			}
			path, notice, errMsg := redirectTarget(t, rec)
			if path != "/memory" || errMsg != tc.wantErr || notice != tc.wantNotice {
				t.Fatalf("got %s notice=%q err=%q", path, notice, errMsg)
			}
			switch tc.name {
			case "set default scope":
				e, err := st.GetMemory(ctx, "global", "note")
				if err != nil || strings.Contains(e.Value, secret) || !strings.HasPrefix(e.Value, "token ") || e.Author != "operator" {
					t.Fatalf("stored %+v %v, want redacted value", e, err)
				}
			case "delete ok":
				if _, err := st.GetMemory(ctx, "global", "k"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("entry still present: %v", err)
				}
			}
		})
	}
}

const studioBudgetYAML = `apiVersion: sablecourt.io/v1alpha1
kind: Budget
metadata:
  name: studio-daily
spec:
  perTask:
    maxInputTokens: 1000
    maxOutputTokens: 1000
    maxCostUSD: 1.00
  daily:
    maxCostUSD: 5.00
`

func TestStudioCreateAgentForm(t *testing.T) {
	cases := []struct {
		name    string
		form    url.Values
		noPath  bool
		budget  bool
		wantErr string
		check   func(t *testing.T, wl domain.Workload)
	}{
		{name: "missing description", form: url.Values{"name": {"a1"}}, wantErr: "name and description are required"},
		{name: "missing name", form: url.Values{"description": {"d"}}, wantErr: "name and description are required"},
		{name: "no manifests path", form: url.Values{"name": {"a2"}, "description": {"d"}}, noPath: true, wantErr: "sablecourtd was started without --manifests; cannot persist YAML"},
		{name: "coding agent unknown harness falls back to claude", budget: true,
			form: url.Values{"name": {"coder-x"}, "description": {"writes code"}, "class": {string(domain.ClassCodingAgent)}, "runtime_target": {"rm -rf"}, "providers": {"anthropic, ,openai"}},
			check: func(t *testing.T, wl domain.Workload) {
				if wl.Spec.Coding == nil || wl.Spec.Coding.Harness != "claude" || wl.Spec.Coding.Sandbox != "container" || wl.Spec.Runtime.Type != "worktree-cli" {
					t.Fatalf("coding spec %+v runtime %+v", wl.Spec.Coding, wl.Spec.Runtime)
				}
				g := wl.Spec.Grants.ModelGate
				if !g.Allowed || g.BudgetRef != "studio-daily" || strings.Join(g.Providers, ",") != "anthropic,openai" {
					t.Fatalf("model gate grant %+v", g)
				}
			}},
		{name: "coding agent keeps known harness", budget: true,
			form: url.Values{"name": {"coder-y"}, "description": {"d"}, "class": {string(domain.ClassCodingAgent)}, "runtime_target": {"codex"}},
			check: func(t *testing.T, wl domain.Workload) {
				if wl.Spec.Coding == nil || wl.Spec.Coding.Harness != "codex" {
					t.Fatalf("coding spec %+v", wl.Spec.Coding)
				}
			}},
		{name: "controller gets docker default and no model grant", budget: true,
			form: url.Values{"name": {"ctl"}, "description": {"d"}, "class": {string(domain.ClassDeterministicController)}},
			check: func(t *testing.T, wl domain.Workload) {
				if wl.Spec.Runtime.Type != "docker" || wl.Spec.Runtime.Image != "alpine:3.21" || !wl.Spec.Runtime.ReadOnlyRootfs {
					t.Fatalf("runtime %+v", wl.Spec.Runtime)
				}
				if wl.Spec.Grants.ModelGate.Allowed || wl.Metadata.Zone != domain.ZoneSandbox {
					t.Fatalf("grant %+v zone %q", wl.Spec.Grants.ModelGate, wl.Metadata.Zone)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newUITestStore(t)
			dir, _, _, getBundle, reload := seedStudioManifests(t, st)
			if tc.budget {
				if err := os.WriteFile(filepath.Join(dir, "budget.yaml"), []byte(studioBudgetYAML), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := reload(); err != nil {
					t.Fatalf("reload with budget: %v", err)
				}
			}
			opts := Options{Store: st, GetBundle: getBundle, Reload: reload, ManifestsPath: dir}
			if tc.noPath {
				opts.ManifestsPath = ""
			}
			mux := newFormMux(t, opts)
			path, notice, errMsg := redirectTarget(t, postForm(t, mux, "/ui/studio/agent", tc.form))
			if path != "/studio" || errMsg != tc.wantErr {
				t.Fatalf("%s err=%q, want %q", path, errMsg, tc.wantErr)
			}
			saved, _ := filepath.Glob(filepath.Join(dir, "studio-*.yaml"))
			if tc.wantErr != "" {
				if len(saved) != 0 || notice != "" {
					t.Fatalf("refused form wrote %v, notice %q", saved, notice)
				}
				return
			}
			name := tc.form.Get("name")
			if notice != "Saved sandbox agent "+name+" and reloaded manifests" || len(saved) != 1 {
				t.Fatalf("notice %q files %v", notice, saved)
			}
			wl, ok := getBundle().Workloads[name]
			if !ok {
				t.Fatalf("workload %s not loaded after save", name)
			}
			tc.check(t, wl)
		})
	}
}

func TestStudioCreateWorkflowForm(t *testing.T) {
	cases := []struct {
		name      string
		form      url.Values
		wantErr   string
		wantSteps []string
	}{
		{"missing name", url.Values{"step1_name": {"s"}, "step1_workload": {"claude-worker"}}, "workflow name is required", nil},
		{"no complete step", url.Values{"name": {"wf"}, "step1_name": {"s"}, "step2_workload": {"codex-worker"}}, "at least one workflow step is required", nil},
		{"one step", url.Values{"name": {"wf-one"}, "description": {"d"}, "step1_name": {"draft"}, "step1_workload": {"claude-worker"}, "step1_prompt": {" write "}}, "", []string{"draft:claude-worker:write"}},
		{"two steps", url.Values{"name": {"wf-two"}, "step1_name": {"draft"}, "step1_workload": {"claude-worker"}, "step2_name": {"review"}, "step2_workload": {"codex-worker"}}, "", []string{"draft:claude-worker:", "review:codex-worker:"}},
		{"reload refusal removes the saved file", url.Values{"name": {"wf-bad"}, "step1_name": {"review"}, "step1_workload": {"codex-worker"}, "step1_model": {"m"}}, `workflow "wf-bad" step "review": deterministic-controller cannot specify provider or model`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newUITestStore(t)
			dir, _, _, getBundle, reload := seedStudioManifests(t, st)
			mux := newFormMux(t, Options{Store: st, GetBundle: getBundle, Reload: reload, ManifestsPath: dir})
			path, notice, errMsg := redirectTarget(t, postForm(t, mux, "/ui/studio/workflow", tc.form))
			if path != "/studio" || errMsg != tc.wantErr {
				t.Fatalf("%s err=%q, want %q", path, errMsg, tc.wantErr)
			}
			saved, _ := filepath.Glob(filepath.Join(dir, "studio-*.yaml"))
			if tc.wantErr != "" {
				if len(saved) != 0 {
					t.Fatalf("refused form wrote %v", saved)
				}
				return
			}
			name := tc.form.Get("name")
			if notice != "Saved sandbox workflow "+name+" and reloaded manifests" {
				t.Fatalf("notice %q", notice)
			}
			wf, ok := getBundle().Workflows[name]
			if !ok {
				t.Fatalf("workflow not loaded")
			}
			var got []string
			for _, s := range wf.Spec.Steps {
				got = append(got, s.Name+":"+s.Workload+":"+s.Prompt)
			}
			if strings.Join(got, "|") != strings.Join(tc.wantSteps, "|") || wf.Metadata.Zone != domain.ZoneSandbox {
				t.Fatalf("steps %v, want %v", got, tc.wantSteps)
			}
		})
	}
}

func TestProviderFormsWithRegistry(t *testing.T) {
	st := newUITestStore(t)
	reg := providers.NewRegistry(providers.Options{})
	mux := newFormMux(t, Options{Store: st, Providers: reg})
	for _, tc := range []struct {
		name, path string
		form       url.Values
		wantErr    string
		wantNotice string
	}{
		{"connect unknown provider", "/ui/providers/nope/connect", url.Values{"api_key": {"k"}}, `unknown API provider "nope"`, ""},
		{"disconnect unknown provider", "/ui/providers/nope/disconnect", nil, `unknown API provider "nope"`, ""},
		{"save models unknown provider", "/ui/providers/nope/models", url.Values{"default_model": {"m"}}, `unknown provider "nope"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, notice, errMsg := redirectTarget(t, postForm(t, mux, tc.path, tc.form))
			if path != "/providers" || errMsg != tc.wantErr || notice != tc.wantNotice {
				t.Fatalf("%s notice=%q err=%q", path, notice, errMsg)
			}
		})
	}
	t.Run("save models", func(t *testing.T) {
		path, notice, errMsg := redirectTarget(t, postForm(t, mux, "/ui/providers/local/models", url.Values{
			"default_model":  {" big "},
			"enabled_models": {"small"},
			"custom_model":   {"x1, ,x2"},
		}))
		if path != "/providers" || errMsg != "" || notice != "Saved model selection for local (default: big)" {
			t.Fatalf("%s notice=%q err=%q", path, notice, errMsg)
		}
		found := false
		for _, p := range reg.List(context.Background()) {
			if p.ID != "local" {
				continue
			}
			found = true
			var enabled []string
			for _, m := range p.Models {
				if m.Enabled {
					enabled = append(enabled, m.ID)
				}
			}
			if p.DefaultModel != "big" || strings.Join(enabled, ",") != "big,small,x1,x2" {
				t.Fatalf("local prefs default=%q enabled=%v", p.DefaultModel, enabled)
			}
		}
		if !found {
			t.Fatal("local provider not listed")
		}
	})
}

func TestProviderTiersFormAndPage(t *testing.T) {
	st := newUITestStore(t)
	mux := newFormMux(t, Options{Store: st})
	path, notice, errMsg := redirectTarget(t, postForm(t, mux, "/ui/providers/tiers", url.Values{
		"tier_fast_provider": {" cli:codex "}, "tier_fast_model": {"tier-fast-model-x"},
		"tier_coding_provider": {"cli:claude"}, "tier_coding_model": {"tier-coding-model-y"},
	}))
	if path != "/providers" || errMsg != "" || notice == "" {
		t.Fatalf("%s notice=%q err=%q", path, notice, errMsg)
	}
	tiers := contextpack.GetModelTiers(context.Background(), st)
	got := map[string]string{}
	for _, r := range tiers {
		got[r.Tier] = r.Provider + "/" + r.Model
	}
	if got["fast"] != "cli:codex/tier-fast-model-x" || got["coding"] != "cli:claude/tier-coding-model-y" {
		t.Fatalf("tiers %v", got)
	}
	body := getPage(t, mux, "/providers?err=bad+key").Body.String()
	if !strings.Contains(body, "tier-fast-model-x") || !strings.Contains(body, "bad key") {
		t.Fatalf("providers page lacks saved tier or error banner")
	}
}

func TestRetryTaskRefusesUnknownTask(t *testing.T) {
	st := newUITestStore(t)
	mux := newFormMux(t, Options{Store: st, Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st})})
	path, notice, errMsg := redirectTarget(t, postForm(t, mux, "/ui/tasks/task_missing/retry", url.Values{"handoff": {"1"}}))
	if path != "/tasks/task_missing" || notice != "" || errMsg == "" {
		t.Fatalf("%s notice=%q err=%q", path, notice, errMsg)
	}
}

func TestRetryTaskWritesAuditRow(t *testing.T) {
	for _, handoff := range []bool{false, true} {
		t.Run(fmt.Sprintf("handoff %v", handoff), func(t *testing.T) {
			ctx := context.Background()
			st := newUITestStore(t)
			now := time.Now().UTC()
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "coder", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateTask(ctx, domain.Task{ID: "task_failed", IdempotencyKey: "task_failed", WorkloadName: "coder", State: domain.TaskStateFailed, InputJSON: `{"prompt":"p"}`, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			bundle := &config.Bundle{Workloads: map[string]domain.Workload{"coder": {
				Metadata: domain.ObjectMeta{Name: "coder", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent},
			}}}
			mux := newFormMux(t, Options{Store: st, Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})})
			form := url.Values{}
			if handoff {
				form.Set("handoff", "1")
			}
			path, notice, errMsg := redirectTarget(t, postForm(t, mux, "/ui/tasks/task_failed/retry", form))
			if errMsg != "" || notice == "" {
				t.Fatalf("%s notice=%q err=%q", path, notice, errMsg)
			}
			newID := strings.TrimPrefix(path, "/tasks/")
			audit, _ := st.ListAudit(ctx, 20)
			want := fmt.Sprintf(`{"retried_from":"task_failed","handoff":%v}`, handoff)
			for _, a := range audit {
				if a.Action == "task.retry" && a.TaskID == newID && a.Category == "task" && a.WorkloadName == "coder" && a.Outcome == "submitted" && a.DetailJSON == want {
					return
				}
			}
			t.Fatalf("task.retry audit row missing for %s: %+v", newID, audit)
		})
	}
}

func TestRetryTaskReplayWritesOneAuditRow(t *testing.T) {
	ctx := context.Background()
	st := newUITestStore(t)
	now := time.Now().UTC()
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "coder", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, domain.Task{ID: "task_failed", IdempotencyKey: "task_failed", WorkloadName: "coder", State: domain.TaskStateFailed, InputJSON: `{"prompt":"p"}`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	bundle := &config.Bundle{Workloads: map[string]domain.Workload{"coder": {
		Metadata: domain.ObjectMeta{Name: "coder", Zone: domain.ZoneRuntime},
		Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent},
	}}}
	mux := newFormMux(t, Options{Store: st, Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})})
	var ids []string
	for _, handoff := range []string{"", "1"} {
		form := url.Values{}
		if handoff != "" {
			form.Set("handoff", handoff)
		}
		path, notice, errMsg := redirectTarget(t, postForm(t, mux, "/ui/tasks/task_failed/retry", form))
		if errMsg != "" || notice == "" {
			t.Fatalf("%s notice=%q err=%q", path, notice, errMsg)
		}
		ids = append(ids, strings.TrimPrefix(path, "/tasks/"))
	}
	if ids[0] != ids[1] {
		t.Fatalf("replay created another task: %v", ids)
	}
	audit, _ := st.ListAudit(ctx, 50)
	count := 0
	for _, a := range audit {
		if a.Action == "task.retry" {
			count++
			if a.TaskID != ids[0] || a.DetailJSON != `{"retried_from":"task_failed","handoff":false}` {
				t.Fatalf("audit row = %+v", a)
			}
		}
	}
	if count != 1 {
		t.Fatalf("task.retry rows = %d, want 1: %+v", count, audit)
	}
}

func TestRunScheduleRefusals(t *testing.T) {
	st := newUITestStore(t)
	rec := controlplane.NewReconciler(controlplane.Options{Store: st})
	for _, tc := range []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{"no scheduler", Options{Store: st}, "scheduler unavailable"},
		{"no bundle", Options{Store: st, Scheduler: controlplane.NewScheduler(rec), GetBundle: func() *config.Bundle { return nil }}, "no manifest bundle loaded"},
		{"unknown schedule", Options{Store: st, Scheduler: controlplane.NewScheduler(rec), GetBundle: func() *config.Bundle { return &config.Bundle{} }}, "schedule not found: nightly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, _, errMsg := redirectTarget(t, postForm(t, newFormMux(t, tc.opts), "/ui/schedules/nightly/run", nil))
			if path != "/schedules" || errMsg != tc.wantErr {
				t.Fatalf("%s err=%q", path, errMsg)
			}
		})
	}
}
