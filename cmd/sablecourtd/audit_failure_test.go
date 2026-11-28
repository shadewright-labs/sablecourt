package main

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// Taking over a task and writing to its stdin are operator actions, not
// evidence that the task's process is alive, so neither records a heartbeat
// (issue 107 row 10). A task whose process is gone still goes stale.
func TestOperatorInputRecordsNoHeartbeat(t *testing.T) {
	steps := []struct{ name, path, body string }{
		{"takeover", "/api/v1/tasks/task_tty/takeover", `{"operator":"alice"}`},
		{"stdin", "/api/v1/tasks/task_tty/stdin", `{"operator":"alice","data":"ls\n"}`},
	}
	env := newAPIEnv(t, apiEnvConfig{})
	env.seedTask(t, "task_tty", domain.TaskStateWorking)
	env.hub.Register("task_tty", &stdinBuffer{})
	last := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := env.st.RecordHeartbeat(context.Background(), "task_tty", last); err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if rec := apiCall(env.h, http.MethodPost, step.path, step.body); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step.name, rec.Code, rec.Body.String())
		}
		task, err := env.st.GetTask(context.Background(), "task_tty")
		if err != nil {
			t.Fatal(err)
		}
		if task.LastHeartbeatAt == nil || !task.LastHeartbeatAt.Equal(last) {
			t.Fatalf("%s moved the heartbeat from %v to %v", step.name, last, task.LastHeartbeatAt)
		}
	}
}

// The daemon's operator routes by class of audit write (issue 107 row 3):
// a route that starts or grants something is refused when its record cannot
// be written; a route that stops something goes ahead and reports the failed
// record; a read logs it. A 500 lists exactly the effects the request
// recorded (issue 179); in wantEffects {id} stands for the approval and
// {new} for a task the request created.
func TestOperatorRoutesAuditWriteFailures(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		prepare     func(t *testing.T, env apiEnv) string // returns a path fragment the request uses
		fail        func(domain.AuditRecord) bool
		method      string
		path        string
		body        string
		wantCode    int
		wantBody    string // for a code other than 500
		what        string
		wantEffects []string
		check       func(t *testing.T, env apiEnv, faults *audittest.Store, arg string)
	}{
		{
			name: "task submission is refused", fail: audittest.Action("task.submit"),
			method: http.MethodPost, path: "/api/v1/tasks", body: `{"workload":"coder","idempotencyKey":"k-new"}`,
			wantCode: 500, what: "task submission",
			check: func(t *testing.T, env apiEnv, _ *audittest.Store, _ string) {
				if _, err := env.st.GetTaskByIdempotencyKey(ctx, "k-new"); err == nil {
					t.Fatal("task created without its record")
				}
			},
		},
		{
			name: "approval is refused and stays pending",
			prepare: func(t *testing.T, env apiEnv) string {
				apr, err := env.approvals.Request(ctx, "task_tty", "coder", "action:git.open_pr", map[string]string{"t": "x"}, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				return apr.ID + "|" + apr.PayloadSHA256
			},
			method: http.MethodPost, path: "/api/v1/approvals/{id}/approve", body: `{"expectedSha256":"{sha}"}`,
			wantCode: 500, what: "approval",
			check: func(t *testing.T, env apiEnv, _ *audittest.Store, arg string) {
				apr, err := env.st.GetApproval(ctx, strings.Split(arg, "|")[0])
				if err != nil {
					t.Fatal(err)
				}
				if apr.Status != domain.ApprovalPending {
					t.Fatalf("status %s, want pending", apr.Status)
				}
			},
		},
		{
			name: "takeover is refused and handed back", fail: audittest.Action("task.takeover"),
			method: http.MethodPost, path: "/api/v1/tasks/task_tty/takeover", body: `{"operator":"alice"}`,
			wantCode: 500, what: "takeover",
			check: func(t *testing.T, env apiEnv, faults *audittest.Store, _ string) {
				faults.Heal()
				if rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_tty/takeover", `{"operator":"bob"}`); rec.Code != http.StatusOK {
					t.Fatalf("takeover still held: %d %s", rec.Code, rec.Body.String())
				}
			},
		},
		{
			name: "detach goes ahead and reports the record",
			prepare: func(t *testing.T, env apiEnv) string {
				if rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_tty/takeover", `{"operator":"alice"}`); rec.Code != http.StatusOK {
					t.Fatalf("takeover: %d %s", rec.Code, rec.Body.String())
				}
				return ""
			},
			fail:   audittest.Action("task.detach"),
			method: http.MethodPost, path: "/api/v1/tasks/task_tty/detach", body: `{"operator":"alice"}`,
			wantCode: 500, what: "detach", wantEffects: []string{"the takeover of task task_tty by alice was released"},
			check: func(t *testing.T, env apiEnv, faults *audittest.Store, _ string) {
				faults.Heal()
				if rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_tty/takeover", `{"operator":"bob"}`); rec.Code != http.StatusOK {
					t.Fatalf("takeover still held: %d %s", rec.Code, rec.Body.String())
				}
			},
		},
		{
			name: "cancel goes ahead and reports the record", fail: audittest.Action("task.cancel"),
			method: http.MethodPost, path: "/api/v1/tasks/task_tty/cancel",
			wantCode: 500, what: "cancel", wantEffects: []string{"recovery of task task_tty was claimed", "task task_tty moved from working to cancelled"},
			check: func(t *testing.T, env apiEnv, _ *audittest.Store, _ string) {
				task, err := env.st.GetTask(ctx, "task_tty")
				if err != nil {
					t.Fatal(err)
				}
				if task.State != domain.TaskStateCancelled {
					t.Fatalf("state %s, want cancelled", task.State)
				}
			},
		},
		{
			name: "suspend goes ahead and reports the record", fail: audittest.Action("task.suspend"),
			method: http.MethodPost, path: "/api/v1/tasks/task_tty/suspend",
			wantCode: 500, what: "suspend", wantEffects: []string{"a checkpoint of task task_tty was saved", "task task_tty moved from working to suspended"},
		},
		{
			name: "resume is refused",
			prepare: func(t *testing.T, env apiEnv) string {
				env.seedTask(t, "task_susp", domain.TaskStateSuspended)
				return ""
			},
			fail:   audittest.Action("task.resume"),
			method: http.MethodPost, path: "/api/v1/tasks/task_susp/resume",
			wantCode: 500, what: "resume",
		},
		{
			name: "retry is refused and creates nothing",
			prepare: func(t *testing.T, env apiEnv) string {
				env.seedTask(t, "task_old", domain.TaskStateFailed)
				return ""
			},
			fail:   audittest.Action("task.retry"),
			method: http.MethodPost, path: "/api/v1/tasks/task_old/retry",
			wantCode: 500, what: "retry",
			check: func(t *testing.T, env apiEnv, _ *audittest.Store, _ string) {
				if _, err := env.st.GetTaskByIdempotencyKey(ctx, "retry-task_old-1"); err == nil {
					t.Fatal("retried task created without its task.retry row")
				}
			},
		},
		{
			name: "denial is refused and stays pending",
			prepare: func(t *testing.T, env apiEnv) string {
				apr, err := env.approvals.Request(ctx, "task_tty", "coder", "action:git.open_pr", map[string]string{"t": "y"}, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				return apr.ID + "|" + apr.PayloadSHA256
			},
			fail:   func(rec domain.AuditRecord) bool { return rec.Category == "approval" && rec.Outcome == "denied" },
			method: http.MethodPost, path: "/api/v1/approvals/{id}/deny", body: `{"reason":"no"}`,
			wantCode: 500, what: "denial",
		},
		{
			name: "repeated takeover by the holder keeps the takeover",
			prepare: func(t *testing.T, env apiEnv) string {
				if rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_tty/takeover", `{"operator":"alice"}`); rec.Code != http.StatusOK {
					t.Fatalf("takeover: %d %s", rec.Code, rec.Body.String())
				}
				return ""
			},
			fail:   audittest.Action("task.takeover"),
			method: http.MethodPost, path: "/api/v1/tasks/task_tty/takeover", body: `{"operator":"alice"}`,
			wantCode: 500, what: "takeover",
			check: func(t *testing.T, env apiEnv, _ *audittest.Store, _ string) {
				if rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_tty/stdin", `{"operator":"alice","data":"pwd\n"}`); rec.Code != http.StatusOK {
					t.Fatalf("confirmed takeover lost after a refused repeat: %d %s", rec.Code, rec.Body.String())
				}
			},
		},
		{
			name: "scry is a read and streams with the record logged", fail: audittest.Action("task.scry"),
			prepare: func(t *testing.T, env apiEnv) string {
				env.hub.Close("task_tty")
				return ""
			},
			method: http.MethodGet, path: "/api/v1/tasks/task_tty/scry",
			wantCode: 200, wantBody: "event: closed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var faults *audittest.Store
			env := newAPIEnv(t, apiEnvConfig{approver: daemonApprover{}, wrap: func(in store.Store) store.Store {
				faults = audittest.Wrap(in)
				return faults
			}})
			env.seedTask(t, "task_tty", domain.TaskStateWorking)
			env.hub.Register("task_tty", &stdinBuffer{})
			var arg string
			if tc.prepare != nil {
				arg = tc.prepare(t, env)
			}
			path, body := tc.path, tc.body
			if id, sha, ok := strings.Cut(arg, "|"); ok {
				path = strings.ReplaceAll(path, "{id}", id)
				body = strings.ReplaceAll(body, "{sha}", sha)
			}
			faults.Fail(tc.fail)
			before := taskIDs(t, env.st)
			rec, effects := apiCallTracked(env.h, tc.method, path, body)
			if tc.wantCode == 500 {
				id, _, _ := strings.Cut(arg, "|")
				requireAuditAnswer(t, rec, tc.what, effects, tc.wantEffects, map[string]string{"{id}": id, "{new}": newTaskID(t, env.st, before)})
			} else if rec.Code != tc.wantCode || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("got %d %s, want %d containing %q", rec.Code, rec.Body.String(), tc.wantCode, tc.wantBody)
			}
			if len(faults.Refused()) == 0 {
				t.Fatal("no audit write was attempted")
			}
			if tc.check != nil {
				tc.check(t, env, faults, arg)
			}
		})
	}
}

// A promotion whose record cannot be written creates no branch.
func TestPromotionBranchNotCreatedWhenAuditWriteFails(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	t.Chdir(repo)
	var faults *audittest.Store
	env := newAPIEnv(t, apiEnvConfig{wrap: func(in store.Store) store.Store {
		faults = audittest.Wrap(in)
		return faults
	}})
	faults.Fail(audittest.Action("sandbox.propose_promotion"))
	rec, effects := apiCallTracked(env.h, http.MethodPost, "/api/v1/promote/trial", "")
	requireAuditAnswer(t, rec, "promotion", effects, nil, nil)
	out, err := exec.Command("git", "-C", repo, "branch", "--list", "sablecourt/promote-trial").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("branch left behind: %s", out)
	}
}
