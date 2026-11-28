package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// dashAuditCase is one dashboard form whose audit write fails.
type dashAuditCase struct {
	name    string
	fail    string
	post    func(t *testing.T, st store.Store) (string, url.Values)
	wantErr string
	check   func(t *testing.T, st store.Store, path string)
}

// The dashboard's audit writes by class (issue 107 row 3): a form that starts
// work is recorded first and refused with "nothing was changed" when the
// record cannot be written; a form whose effect already happened (a retry
// task created, a task cancelled or suspended) names that effect.
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
			wantErr: "proposal refused: its audit record could not be written",
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
			name: "retry reports the record of a task it created",
			fail: "task.retry",
			post: func(t *testing.T, st store.Store) (string, url.Values) {
				now := time.Now().UTC()
				if err := st.CreateTask(ctx, domain.Task{ID: "task_failed", IdempotencyKey: "task_failed", WorkloadName: "coder", State: domain.TaskStateFailed, InputJSON: `{"prompt":"p"}`, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
				return "/ui/tasks/task_failed/retry", url.Values{}
			},
			wantErr: "was submitted, but an audit record could not be written",
			check: func(t *testing.T, st store.Store, path string) {
				if _, err := st.GetTask(ctx, strings.TrimPrefix(path, "/tasks/")); err != nil {
					t.Fatalf("retry task missing: %v", err)
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
			post:    task("task_c", domain.TaskStateWorking, "/ui/tasks/task_c/cancel"),
			wantErr: "task task_c was cancelled, but an audit record could not be written"},
		{name: "suspend reports the task it suspended", fail: "task.suspend",
			post:    task("task_s", domain.TaskStateWorking, "/ui/tasks/task_s/suspend"),
			wantErr: "task task_s was suspended, but an audit record could not be written"},
		{name: "resume is refused", fail: "task.resume",
			post:    task("task_r", domain.TaskStateSuspended, "/ui/tasks/task_r/resume"),
			wantErr: "resume refused: its audit record could not be written, nothing was changed"},
		{name: "retry is refused before its task", fail: "task.submit",
			post:    task("task_f", domain.TaskStateFailed, "/ui/tasks/task_f/retry"),
			wantErr: "retry refused: its audit record could not be written, nothing was changed"},
		{name: "submission is refused", fail: "task.submit",
			post: func(*testing.T, store.Store) (string, url.Values) {
				return "/ui/tasks", url.Values{"workload": {"coder"}, "input_json": {"hi"}}
			},
			wantErr: "task submission refused: its audit record could not be written, nothing was changed"},
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
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			target, _, errMsg := redirectTarget(t, rec)
			if !strings.Contains(errMsg, tc.wantErr) {
				t.Fatalf("redirect %s err=%q, want %q", target, errMsg, tc.wantErr)
			}
			if strings.Contains(tc.wantErr, ", but ") && strings.Contains(errMsg, "nothing was changed") {
				t.Fatalf("err=%q says nothing changed after an effect", errMsg)
			}
			if len(faults.Refused()) == 0 {
				t.Fatal("no audit write was attempted")
			}
			if tc.check != nil {
				tc.check(t, st, target)
			}
		})
	}
}

func seedUITask(t *testing.T, st store.Store, id string, state domain.TaskState) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.CreateTask(context.Background(), domain.Task{ID: id, IdempotencyKey: id, WorkloadName: "coder", State: state, InputJSON: `{"prompt":"p"}`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}
