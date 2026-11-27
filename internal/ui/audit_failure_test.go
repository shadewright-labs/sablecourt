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

// The dashboard's audit writes by class (issue 107 row 3): a learning
// proposal is recorded first and refused when the record cannot be written;
// a retry has created its task by the time its row is written, so the failed
// row is reported with the new task.
func TestDashboardAuditWriteFailures(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		fail    string
		post    func(t *testing.T, st store.Store) (string, url.Values)
		wantErr string
		check   func(t *testing.T, st store.Store, path string)
	}{
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
			wantErr: "was submitted, but its audit record could not be written",
			check: func(t *testing.T, st store.Store, path string) {
				if _, err := st.GetTask(ctx, strings.TrimPrefix(path, "/tasks/")); err != nil {
					t.Fatalf("retry task missing: %v", err)
				}
			},
		},
	}
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
			dash, err := NewDashboard(Options{Store: faults, Reconciler: controlplane.NewReconciler(controlplane.Options{Store: st, Bundle: bundle})})
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
			tc.check(t, st, target)
		})
	}
}
