package main

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
	"github.com/shadewright-labs/sablecourt/internal/store/audittest"
)

// A refused takeover leaves no owner, so stdin cannot take over implicitly
// and write without a task.takeover row (issue 107 row 3).
func TestStdinDoesNotBypassFailedTakeoverAudit(t *testing.T) {
	var faults *audittest.Store
	env := newAPIEnv(t, apiEnvConfig{wrap: func(in store.Store) store.Store {
		faults = audittest.Wrap(in)
		return faults
	}})
	env.seedTask(t, "task_tty", domain.TaskStateWorking)
	buf := &stdinBuffer{}
	env.hub.Register("task_tty", buf)
	faults.Fail(audittest.Action("task.takeover"))
	if rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_tty/takeover", `{"operator":"alice"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("takeover: want 500, got %d", rec.Code)
	}
	rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_tty/stdin", `{"operator":"alice","data":"ls\n"}`)
	if rec.Code == http.StatusOK || buf.String() != "" {
		t.Fatalf("stdin bypassed refused takeover: code=%d written=%q", rec.Code, buf.String())
	}
}

// hookStore runs onAppend before each audit write.
type hookStore struct {
	store.Store
	mu       sync.Mutex
	onAppend func(domain.AuditRecord) error
}

func (h *hookStore) AppendAudit(ctx context.Context, rec domain.AuditRecord) error {
	h.mu.Lock()
	fn := h.onAppend
	h.mu.Unlock()
	if fn != nil {
		if err := fn(rec); err != nil {
			return err
		}
	}
	return h.Store.AppendAudit(ctx, rec)
}

// While the task.takeover row is being written, the operator does not hold
// the takeover yet, so a concurrent stdin write is refused.
func TestStdinRefusedWhileTakeoverRowIsWritten(t *testing.T) {
	hooks := &hookStore{}
	env := newAPIEnv(t, apiEnvConfig{wrap: func(in store.Store) store.Store {
		hooks.Store = in
		return hooks
	}})
	env.seedTask(t, "task_tty", domain.TaskStateWorking)
	buf := &stdinBuffer{}
	env.hub.Register("task_tty", buf)
	var during int
	hooks.onAppend = func(rec domain.AuditRecord) error {
		if rec.Action == "task.takeover" {
			during = apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_tty/stdin", `{"operator":"alice","data":"early\n"}`).Code
		}
		return nil
	}
	if rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_tty/takeover", `{"operator":"alice"}`); rec.Code != http.StatusOK {
		t.Fatalf("takeover: %d %s", rec.Code, rec.Body.String())
	}
	if during != http.StatusConflict || buf.String() != "" {
		t.Fatalf("stdin during the takeover row: code=%d written=%q", during, buf.String())
	}
	if rec := apiCall(env.h, http.MethodPost, "/api/v1/tasks/task_tty/stdin", `{"operator":"alice","data":"ls\n"}`); rec.Code != http.StatusOK || buf.String() != "ls\n" {
		t.Fatalf("stdin after takeover: %d %q", rec.Code, buf.String())
	}
}

// The promotion row is written before the branch exists, so a crash between
// the two never leaves an unrecorded branch; a branch that then cannot be
// created gets an error row.
func TestPromotionRecordedBeforeBranch(t *testing.T) {
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
	hooks := &hookStore{}
	env := newAPIEnv(t, apiEnvConfig{wrap: func(in store.Store) store.Store {
		hooks.Store = in
		return hooks
	}})
	branchExists := func() bool {
		out, err := exec.Command("git", "-C", repo, "branch", "--list", "sablecourt/promote-trial").CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out)) != ""
	}
	var existedAtRow, sawRow bool
	hooks.onAppend = func(rec domain.AuditRecord) error {
		if rec.Action == "sandbox.propose_promotion" && rec.Outcome != "error" {
			sawRow, existedAtRow = true, branchExists()
		}
		return nil
	}
	if rec := apiCall(env.h, http.MethodPost, "/api/v1/promote/trial", ""); rec.Code != http.StatusOK {
		t.Fatalf("promote: %d %s", rec.Code, rec.Body.String())
	}
	if !sawRow || existedAtRow {
		t.Fatalf("row written=%v, branch existed when it was written=%v", sawRow, existedAtRow)
	}
	// The branch exists now, so a second promotion fails after its row.
	if rec := apiCall(env.h, http.MethodPost, "/api/v1/promote/trial", ""); rec.Code != http.StatusConflict {
		t.Fatalf("second promote: %d %s", rec.Code, rec.Body.String())
	}
	rows, err := env.st.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || rows[0].Action != "sandbox.propose_promotion" || rows[0].Outcome != "error" {
		t.Fatalf("no error row after the failed branch: %+v", rows)
	}
}

// Every operator and gate route that hits a failed audit write answers 500
// (issue 107 row 3), and says what already happened when something did.
func TestAuditWriteFailuresAnswer500(t *testing.T) {
	ctx := context.Background()
	withConclave := func(t *testing.T, env apiEnv) string {
		env.bundle.Conclaves = map[string]domain.Conclave{"council": {Spec: domain.ConclaveSpec{Vassals: []domain.ConclaveVassal{
			{Role: "coder", Workload: "coder", Activation: domain.ActivationOnMention},
		}}}}
		rec := apiCall(env.h, http.MethodPost, "/api/v1/conclaves", `{"template":"council","title":"t"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("open conclave: %d %s", rec.Code, rec.Body.String())
		}
		_, after, _ := strings.Cut(rec.Body.String(), `"ID":"`)
		id, _, _ := strings.Cut(after, `"`)
		return id
	}
	cases := []struct {
		name     string
		prepare  func(t *testing.T, env apiEnv) string
		fail     string
		path     string
		body     string
		wantBody string
	}{
		{
			name: "retry", fail: "task.submit",
			prepare: func(t *testing.T, env apiEnv) string {
				env.seedTask(t, "task_old", domain.TaskStateFailed)
				return ""
			},
			path: "/api/v1/tasks/task_old/retry", wantBody: "nothing was changed",
		},
		{
			name: "schedule run", fail: "task.submit",
			prepare: func(t *testing.T, env apiEnv) string {
				env.bundle.Schedules = map[string]domain.Schedule{"nightly": {
					Metadata: domain.ObjectMeta{Name: "nightly"},
					Spec:     domain.ScheduleSpec{Cron: "0 1 * * *", TargetKind: "workload", TargetName: "coder", Enabled: true},
				}}
				return ""
			},
			path: "/api/v1/schedules/nightly/run", wantBody: "nothing was changed",
		},
		{
			name: "open conclave", fail: "conclave.open",
			prepare: func(t *testing.T, env apiEnv) string {
				env.bundle.Conclaves = map[string]domain.Conclave{"council": {}}
				return ""
			},
			path: "/api/v1/conclaves", body: `{"template":"council","title":"t"}`, wantBody: "nothing was changed",
		},
		{
			name: "store relic", fail: "conclave.relic_put", prepare: withConclave,
			path: "/api/v1/conclaves/{id}/relics", body: `{"name":"r","body":"x"}`, wantBody: "nothing was changed",
		},
		{
			name: "decree whose wake is refused", fail: "task.submit", prepare: withConclave,
			path: "/api/v1/conclaves/{id}/decree", body: `{"to":"all","body":"go"}`, wantBody: "was stored, but",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var faults *audittest.Store
			env := newAPIEnv(t, apiEnvConfig{wrap: func(in store.Store) store.Store {
				faults = audittest.Wrap(in)
				return faults
			}})
			path := tc.path
			if tc.prepare != nil {
				path = strings.ReplaceAll(path, "{id}", tc.prepare(t, env))
			}
			faults.Fail(audittest.Action(tc.fail))
			rec := apiCall(env.h, http.MethodPost, path, tc.body)
			if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("got %d %s, want 500 containing %q", rec.Code, rec.Body.String(), tc.wantBody)
			}
			if len(faults.Refused()) == 0 {
				t.Fatal("no audit write was attempted")
			}
			_ = ctx
		})
	}
}
