package ui

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// dashAuditCase is one dashboard form whose audit write fails.
type dashAuditCase struct {
	name string
	fail string
	post func(t *testing.T, st store.Store) (string, url.Values)
	// what names the request in a refusal; wantEffects are the effects the
	// form records before the failed write, {new} standing for the ID of the
	// task the redirect names. The error lists exactly those effects.
	what        string
	wantEffects []string
	check       func(t *testing.T, st store.Store, path string)
}

// The dashboard's audit writes by class (issue 107 row 3): a form that starts
// work is recorded first and refused, with no effect, when the record cannot
// be written; a form whose effect already happened (a retry task created, a
// task cancelled or suspended) lists the effects it recorded (issue 179).
func TestDashboardAuditWriteFailures(t *testing.T) {
	ctx := context.Background()
	cases := []dashAuditCase{
		{
			name: "learning proposal is refused and not stored",
			fail: "learning.propose",
			post: func(t *testing.T, st store.Store) (string, url.Values) {
				now := time.Now().UTC()
				if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "cc-1", Title: "t", State: domain.ConclaveStateActive, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
				msgID, err := st.AppendConclaveMessage(ctx, domain.ConclaveMessage{ConclaveID: "cc-1", FromRole: "coder", ToRole: "all", Kind: "proposal", Body: "b", CreatedAt: now})
				if err != nil {
					t.Fatal(err)
				}
				return "/ui/conclaves/cc-1/learning", url.Values{"scope": {"global"}, "key": {"conclave-msg-" + strconv.FormatInt(msgID, 10)}, "rule": {"Keep PRs small"}}
			},
			what: "proposal",
			check: func(t *testing.T, st store.Store, _ string) {
				entries, err := st.SearchMemory(ctx, "proposed:learning:global", "", 10)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatalf("proposal stored without its record: %+v", entries)
				}
			},
		},
		{
			name: "retry is refused and creates nothing",
			fail: "task.retry",
			post: func(t *testing.T, st store.Store) (string, url.Values) {
				now := time.Now().UTC()
				if err := st.CreateTask(ctx, domain.Task{ID: "task_failed", IdempotencyKey: "task_failed", WorkloadName: "coder", State: domain.TaskStateFailed, InputJSON: `{"prompt":"p"}`, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
				return "/ui/tasks/task_failed/retry", url.Values{}
			},
			what: "retry",
			check: func(t *testing.T, st store.Store, _ string) {
				if _, err := st.GetTaskByIdempotencyKey(ctx, "retry-task_failed-1"); err == nil {
					t.Fatal("retried task created without its task.retry row")
				}
			},
		},
	}
	task := func(id string, state domain.TaskState, path string) func(t *testing.T, st store.Store) (string, url.Values) {
		return func(t *testing.T, st store.Store) (string, url.Values) {
			seedUITask(t, st, id, state)
			return path, url.Values{}
		}
	}
	cases = append(cases, []dashAuditCase{
		{name: "cancel reports the task it cancelled", fail: "task.cancel",
			post: task("task_c", domain.TaskStateWorking, "/ui/tasks/task_c/cancel"),
			what: "cancel", wantEffects: []string{"recovery of task task_c was claimed", "task task_c moved from working to cancelled"}},
		{name: "suspend reports the task it suspended", fail: "task.suspend",
			post: task("task_s", domain.TaskStateWorking, "/ui/tasks/task_s/suspend"),
			what: "suspend", wantEffects: []string{"a checkpoint of task task_s was saved", "task task_s moved from working to suspended"}},
		{name: "resume is refused", fail: "task.resume",
			post: task("task_r", domain.TaskStateSuspended, "/ui/tasks/task_r/resume"),
			what: "resume"},
		{name: "retry is refused before its task", fail: "task.submit",
			post: task("task_f", domain.TaskStateFailed, "/ui/tasks/task_f/retry"),
			what: "retry"},
		// Court Memory changes write their row in the change's transaction
		// (issue 228): a row that cannot be written changes nothing.
		{name: "memory set is refused and not stored", fail: "memory.put",
			post: func(*testing.T, store.Store) (string, url.Values) {
				return "/ui/memory/set", url.Values{"scope": {"notes"}, "key": {"new"}, "value": {"v"}}
			},
			what: "memory write",
			check: func(t *testing.T, st store.Store, _ string) {
				if _, err := st.GetMemory(ctx, "notes", "new"); err == nil {
					t.Fatal("memory stored without its row")
				}
			}},
		{name: "memory delete is refused and keeps the entry", fail: "memory.delete",
			post: func(t *testing.T, st store.Store) (string, url.Values) {
				if err := st.PutMemory(ctx, domain.MemoryEntry{Namespace: "notes", Key: "old", Value: "v"}); err != nil {
					t.Fatal(err)
				}
				return "/ui/memory/delete", url.Values{"scope": {"notes"}, "key": {"old"}}
			},
			what: "memory delete",
			check: func(t *testing.T, st store.Store, _ string) {
				if _, err := st.GetMemory(ctx, "notes", "old"); err != nil {
					t.Fatalf("memory deleted without its row: %v", err)
				}
			}},
		{name: "model tier save is refused and not stored", fail: "model_tiers.save",
			post: func(*testing.T, store.Store) (string, url.Values) {
				return "/ui/providers/tiers", url.Values{"tier_fast_provider": {"local"}, "tier_fast_model": {"llama-3"}}
			},
			what: "model tier save",
			check: func(t *testing.T, st store.Store, _ string) {
				if _, err := st.GetMemory(ctx, contextpack.ModelTiersNamespace, "fast"); err == nil {
					t.Fatal("model tier stored without its row")
				}
			}},
		{name: "submission is refused", fail: "task.submit",
			post: func(*testing.T, store.Store) (string, url.Values) {
				return "/ui/tasks", url.Values{"workload": {"coder"}, "input_json": {"hi"}}
			},
			what: "task submission"},
	}...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newUITestStore(t)
			if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "coder", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
				t.Fatal(err)
			}
			bundle := &config.Bundle{Workloads: map[string]domain.Workload{"coder": {
				Metadata: domain.ObjectMeta{Name: "coder", Zone: domain.ZoneRuntime},
				Spec:     domain.WorkloadSpec{Class: domain.ClassLongLivedAgent},
			}}}
			faults := audittest.Wrap(st)
			dash, err := NewDashboard(Options{Store: faults, Reconciler: controlplane.NewReconciler(controlplane.Options{Store: faults, Bundle: bundle})})
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			dash.RegisterRoutes(mux)
			path, form := tc.post(t, st)
			faults.Fail(audittest.Action(tc.fail))
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			reqCtx, effects := store.TrackEffects(req.Context())
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req.WithContext(reqCtx))
			target, _, errMsg := redirectTarget(t, rec)
			if errMsg != audittest.Answer(tc.what, effects.List()...) {
				t.Fatalf("redirect %s err=%q does not list the recorded effects %q", target, errMsg, effects.List())
			}
			var want []string
			for _, e := range tc.wantEffects {
				want = append(want, strings.ReplaceAll(e, "{new}", strings.TrimPrefix(target, "/tasks/")))
			}
			if !slices.Equal(effects.List(), want) {
				t.Fatalf("effects %q, want %q", effects.List(), want)
			}
			if len(faults.Refused()) == 0 {
				t.Fatal("no audit write was attempted")
			}
			if tc.check != nil {
				tc.check(t, st, target)
			}
			// The page the form redirects to shows the error it carries.
			faults.Heal()
			if strings.HasPrefix(target, "/tasks/") {
				page := getPage(t, mux, rec.Header().Get("Location"))
				if want := `sc-banner-err">` + template.HTMLEscapeString(errMsg) + `<`; page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
					t.Fatalf("task page %s (status %d) does not show the error %q", target, page.Code, errMsg)
				}
			}
		})
	}
}

// The task page shows the notice and the error a form redirect carries, as
// the other pages do.
func TestTaskPageShowsNoticeAndErr(t *testing.T) {
	st := newUITestStore(t)
	if err := st.UpsertWorkload(context.Background(), domain.WorkloadRecord{Name: "coder", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	seedUITask(t, st, "task_n", domain.TaskStateFailed)
	mux := newFormMux(t, Options{Store: st})
	for _, tc := range []struct{ query, want, absent string }{
		{"?notice=Retried+from+t1", `sc-banner-ok">Retried from t1<`, "sc-banner-err"},
		{"?err=went+%3Cwrong%3E", `sc-banner-err">went &lt;wrong&gt;<`, "sc-banner-ok"},
		{"", "", "sc-banner"},
	} {
		body := getPage(t, mux, "/tasks/task_n"+tc.query).Body.String()
		if tc.want != "" && !strings.Contains(body, tc.want) {
			t.Errorf("%q: page lacks %q", tc.query, tc.want)
		}
		if strings.Contains(body, tc.absent) {
			t.Errorf("%q: page has %q", tc.query, tc.absent)
		}
	}
}

// Every page a form redirects to with notice or err shows both. A new
// redirect target that drops them fails here once it is added to the list.
func TestRedirectTargetsShowNoticeAndErr(t *testing.T) {
	ctx := context.Background()
	st := newUITestStore(t)
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "coder", Zone: domain.ZoneRuntime, Class: domain.ClassLongLivedAgent, AgentCardJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	seedUITask(t, st, "task_n", domain.TaskStateFailed)
	now := time.Now().UTC()
	if err := st.CreateConclave(ctx, domain.ConclaveSession{ID: "cc-1", Title: "t", State: domain.ConclaveStateActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	mux := newFormMux(t, Options{Store: st})
	for _, path := range []string{"/", "/memory", "/conclaves", "/conclaves/cc-1", "/providers", "/schedules", "/studio", "/tasks/task_n"} {
		page := getPage(t, mux, path+"?notice=n+ok&err=e+bad")
		body := page.Body.String()
		if page.Code != http.StatusOK || !strings.Contains(body, `banner-ok">n ok<`) || !strings.Contains(body, `banner-err">e bad<`) {
			t.Errorf("%s (status %d) does not show both notice and err", path, page.Code)
		}
	}
}

func seedUITask(t *testing.T, st store.Store, id string, state domain.TaskState) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.CreateTask(context.Background(), domain.Task{ID: id, IdempotencyKey: id, WorkloadName: "coder", State: state, InputJSON: `{"prompt":"p"}`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}
