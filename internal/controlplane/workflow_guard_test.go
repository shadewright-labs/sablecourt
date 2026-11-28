package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// Issue 214: the run's first author base is recorded by the daemon and named
// to every command step, and the example guard compares the author's changes
// from it.

// guardCommand is the example coding court's invariant path guard with its
// own path list.
func guardCommand(t *testing.T, paths ...string) domain.CodingCommand {
	t.Helper()
	bundle, err := config.LoadPath(filepath.Join("..", "..", "examples", "coding-court", "coding-court.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wl := bundle.Workloads["developer-go"]
	cmd, ok := wl.Spec.Coding.Command("invariant-paths")
	if !ok || len(cmd.Argv) < 4 {
		t.Fatal("the example has no invariant-paths guard")
	}
	cmd.Argv = append(slices.Clone(cmd.Argv[:4]), paths...)
	return cmd
}

// runInCheckout runs a command launch's argv on the host against the
// launch's checkout and Git metadata, as the container would, and records
// the author base ref the launch's Git metadata names.
type guardRunner struct {
	mu    sync.Mutex
	bases []string
}

func (g *guardRunner) run(ctx context.Context, l scruntime.CouncilLaunch) (string, string, int, error) {
	env := append(os.Environ(), "GIT_DIR="+l.Checkout.GitDir, "GIT_WORK_TREE="+l.Checkout.Path, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	ref := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", scruntime.AuthorBaseRef)
	ref.Env = env
	out, _ := ref.Output()
	g.mu.Lock()
	g.bases = append(g.bases, strings.TrimSpace(string(out)))
	g.mu.Unlock()
	cmd := exec.CommandContext(ctx, l.Command[0], l.Command[1:]...)
	cmd.Dir, cmd.Env = l.Checkout.Path, env
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return stdout.String(), stderr.String(), exit.ExitCode(), nil
	}
	return stdout.String(), stderr.String(), 0, err
}

func (g *guardRunner) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.bases)
}

func repoHead(t *testing.T, repo string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// TestInvariantGuardStopsTheRunBeforeReview is the Property of the decision
// of 2026-11-28: a change on a listed path between the run's first author
// base and the pinned author commit never reaches the reviewer step, and an
// unlisted change does.
func TestInvariantGuardStopsTheRunBeforeReview(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		paths  []string
		halted bool
	}{
		"listed path halts the run":    {paths: []string{"feature.txt"}, halted: true},
		"unlisted path reaches review": {paths: []string{"internal/redact/"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := &guardRunner{}
			f := newCommandFixture(t, guardCommand(t, tc.paths...), g.run)
			base := repoHead(t, f.bundle.Workloads["coder"].Spec.Coding.RepoPath)
			root := f.start()
			if tc.halted {
				f.runUntil("the halt", func() bool {
					_, halted := f.event(root.ID, "workflow_halted_command_failed")
					return terminal(f.step(1)) && halted
				})
				if _, ok := f.step(2); ok {
					t.Fatal("the review step started after the guard matched")
				}
				ev, _ := f.event(root.ID, "workflow_halted_command_failed")
				if !strings.Contains(ev.FactJSON, "coding-invariant") || !strings.Contains(ev.FactJSON, `"exit_code":1`) {
					t.Fatalf("halt fact = %s", ev.FactJSON)
				}
				check, _ := f.step(1)
				if res, ok := CommandStepResult(check); !ok || !strings.Contains(res.Output, "feature.txt") {
					t.Fatalf("guard result = %+v", res)
				}
			} else {
				f.runUntil("the review step", func() bool { return terminal(f.step(2)) })
				review, _ := f.step(2)
				if review.State != domain.TaskStateCompleted {
					t.Fatalf("review state = %s", review.State)
				}
			}
			author, _ := f.step(0)
			check, _ := f.step(1)
			if got := commitArtifact(author.ArtifactsJSON, scruntime.GitBaseArtifact); got != base {
				t.Fatalf("author git-base = %q, want the repository head %q it started from", got, base)
			}
			if got := g.seen(); len(got) != 1 || got[0] != base {
				t.Fatalf("author base refs seen in the container = %q, want [%s]", got, base)
			}
			// The base is durable: a reconciler built on the same store reads it.
			fresh := NewReconciler(Options{Store: f.st, Bundle: f.bundle, Policy: policy.NewEngine(nil, nil)})
			reg, err := fresh.workflowStep(t.Context(), check.ID)
			if err != nil || reg == nil || reg.AuthorBase != base || reg.AuthorCommit != authorCommit(author.ArtifactsJSON) {
				t.Fatalf("command registration after restart = %+v, %v", reg, err)
			}
		})
	}
}

// TestAuthorRetryKeepsFirstAuthorBase checks that a fix round after a
// verifier's CHANGES_REQUESTED, pinned to the first attempt's commit, keeps
// the run's first author base, so the guard after it still sees the first
// attempt's changes.
func TestAuthorRetryKeepsFirstAuthorBase(t *testing.T) {
	t.Parallel()
	g := &guardRunner{}
	f := newCommandFixture(t, guardCommand(t, "internal/redact/"), g.run)
	steps := f.bundle.Workflows[commandWorkflow].Spec.Steps
	steps[0].MaxRetries = 1
	base := repoHead(t, f.bundle.Workloads["coder"].Spec.Coding.RepoPath)
	var mu sync.Mutex
	authors, reviews := 0, 0
	f.runner.onRun = func(ctx context.Context, s scruntime.CouncilLaunch) (string, string, int, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case s.Command != nil:
			return g.run(ctx, s)
		case s.Checkout.Kind == scruntime.CouncilTask:
			authors++
			name := "feature.txt"
			if authors > 1 {
				name = "fix.txt"
			}
			return "done", "", 0, os.WriteFile(filepath.Join(s.Checkout.Path, name), []byte("x\n"), 0o600)
		default:
			reviews++
			if reviews == 1 {
				return "add a fix\nVERDICT: CHANGES_REQUESTED", "", 0, nil
			}
			return "ok\nVERDICT: PASS", "", 0, nil
		}
	}
	f.start()
	var commands []domain.Task
	f.runUntil("the second guard", func() bool {
		commands = commands[:0]
		for _, task := range f.tasks() {
			if f.r.isCommandStep(commandWorkflow, 1, task.WorkloadName) && strings.Contains(task.InputJSON, `"command"`) && terminalTaskState(task.State) {
				commands = append(commands, task)
			}
		}
		return len(commands) == 2
	})
	seen := g.seen()
	if len(seen) != 2 || seen[0] != base || seen[1] != base {
		t.Fatalf("author base refs seen = %q, want the first base %s twice", seen, base)
	}
	var retryID, retryCommit string
	for _, task := range f.tasks() {
		if strings.Contains(task.InputJSON, `"retry_count":1`) && strings.Contains(task.InputJSON, `"role":"author"`) {
			retryID, retryCommit = task.ID, authorCommit(task.ArtifactsJSON)
			if got := commitArtifact(task.ArtifactsJSON, scruntime.GitBaseArtifact); got == base || got == "" {
				t.Fatalf("the retry's own git-base = %q; the test needs it to differ from the first base", got)
			}
		}
	}
	if retryID == "" || retryCommit == "" {
		t.Fatalf("no retried author with a commit (id %q, commit %q)", retryID, retryCommit)
	}
	// The second guard is the one that checks the retried author; it is
	// pinned to the retry's commit, with the first author base.
	retryGuards := 0
	for _, c := range commands {
		reg, err := f.r.workflowStep(t.Context(), c.ID)
		if err != nil || reg == nil || reg.AuthorBase != base {
			t.Fatalf("guard registration = %+v, %v; want author base %s", reg, err, base)
		}
		var meta struct {
			AuthorTaskID string `json:"author_task_id"`
		}
		if err := json.Unmarshal([]byte(c.InputJSON), &meta); err != nil || meta.AuthorTaskID == "" {
			t.Fatalf("guard input %s names no author task: %v", c.InputJSON, err)
		}
		if meta.AuthorTaskID != retryID {
			continue
		}
		retryGuards++
		if reg.AuthorCommit != retryCommit {
			t.Fatalf("second guard pinned to %s, want the retry's commit %s", reg.AuthorCommit, retryCommit)
		}
	}
	if retryGuards != 1 {
		t.Fatalf("guards of the retried author = %d, want 1", retryGuards)
	}
}

// TestRetryTaskWithHandoffAuthorBase checks a retry by hand: a failed first
// author records its own base on the retry, and a retry of that retry keeps
// it, whatever base the retry reported.
func TestRetryTaskWithHandoffAuthorBase(t *testing.T) {
	t.Parallel()
	f := newCommandFixture(t, checkCommand(0, 0), nil)
	ctx := t.Context()
	first, second := strings.Repeat("1", 40), strings.Repeat("2", 40)
	commitA, commitB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	arts := func(commit, base string) string {
		raw, _ := json.Marshal([]scruntime.Artifact{
			{Name: "git-commit", ContentType: "text/plain", Text: commit},
			{Name: scruntime.GitBaseArtifact, ContentType: "text/plain", Text: base},
		})
		return string(raw)
	}
	failed := func(id, base, commit string, rec workflowStepRecord) domain.Task {
		t.Helper()
		now := time.Now().UTC()
		task := domain.Task{ID: id, IdempotencyKey: id, WorkloadName: "coder", State: domain.TaskStateSubmitted,
			InputJSON: `{"workflow":"` + commandWorkflow + `","step_index":0,"role":"author"}`, CreatedAt: now, UpdatedAt: now}
		entry, err := f.r.workflowStepEntry(id, rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := f.st.PutMemory(ctx, entry); err != nil {
			t.Fatal(err)
		}
		if err := f.st.UpdateTaskState(ctx, id, domain.TaskStateSubmitted, domain.TaskStateFailed, "runtime_failed", `{}`); err != nil {
			t.Fatal(err)
		}
		if err := f.st.UpdateTaskArtifacts(ctx, id, arts(commit, base)); err != nil {
			t.Fatal(err)
		}
		got, err := f.st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	orig := failed("task_first", first, commitA, workflowStepRecord{RunID: "task_first", Workflow: commandWorkflow})
	retry, err := f.r.RetryTaskWithHandoff(ctx, orig.ID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := f.r.workflowStep(ctx, retry.ID)
	if err != nil || reg == nil || reg.AuthorBase != first || reg.AuthorCommit != commitA {
		t.Fatalf("retry registration = %+v, %v; want base %s, commit %s", reg, err, first, commitA)
	}
	// The retry was pinned to commitA, so it reports that as its base.
	if err := f.st.UpdateTaskState(ctx, retry.ID, domain.TaskStateSubmitted, domain.TaskStateFailed, "runtime_failed", `{}`); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateTaskArtifacts(ctx, retry.ID, arts(commitB, second)); err != nil {
		t.Fatal(err)
	}
	again, err := f.r.RetryTaskWithHandoff(ctx, retry.ID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	reg, err = f.r.workflowStep(ctx, again.ID)
	if err != nil || reg == nil || reg.AuthorBase != first || reg.AuthorCommit != commitB {
		t.Fatalf("second retry registration = %+v, %v; want base %s, commit %s", reg, err, first, commitB)
	}
}

// TestSetRunAuthorBase checks which step sets the run's author base or its
// unknown state: only the run's first author attempt, only once.
func TestSetRunAuthorBase(t *testing.T) {
	t.Parallel()
	base, other := strings.Repeat("1", 40), strings.Repeat("2", 40)
	// arts is a completed author's artifacts: its commit and these bases.
	arts := func(texts ...string) string {
		items := []scruntime.Artifact{{Name: "git-commit", ContentType: "text/plain", Text: strings.Repeat("3", 40)}}
		for _, text := range texts {
			items = append(items, scruntime.Artifact{Name: scruntime.GitBaseArtifact, ContentType: "text/plain", Text: text})
		}
		raw, _ := json.Marshal(items)
		return string(raw)
	}
	for name, tc := range map[string]struct {
		recorded      string
		unknown       bool
		pinned        string
		author        bool
		earlierAuthor bool
		artifacts     string
		want          string
		wantUnknown   bool
	}{
		"first author records its base":                 {author: true, artifacts: arts(base), want: base},
		"a pinned author with no base sets unknown":     {pinned: other, author: true, artifacts: arts(other), wantUnknown: true},
		"a later step's author with no base is unknown": {author: true, earlierAuthor: true, artifacts: arts(other), wantUnknown: true},
		"a later author keeps the first":                {recorded: base, author: true, artifacts: arts(other), want: base},
		"unknown stays unknown after a later author":    {unknown: true, author: true, artifacts: arts(other), wantUnknown: true},
		"unknown stays unknown after a verifier":        {unknown: true, author: false, artifacts: arts(other), wantUnknown: true},
		"unknown wins over a recorded base":             {recorded: base, unknown: true, author: true, artifacts: arts(base), wantUnknown: true},
		"a verifier or command records none":            {author: false, artifacts: arts(base)},
		"a verifier passes the base on":                 {recorded: base, author: false, artifacts: arts(other), want: base},
		"two base artifacts":                            {author: true, artifacts: arts(base, other), wantUnknown: true},
		"base not a commit ID":                          {author: true, artifacts: arts("HEAD"), wantUnknown: true},
		"a commit and no base":                          {author: true, artifacts: arts(), wantUnknown: true},
		"a failed attempt with no commit sets neither":  {author: true, artifacts: `[{"name":"git-base","contentType":"text/plain","text":"` + base + `"}]`},
		"no artifacts":                                  {author: true, artifacts: ""},
		"no artifacts after a pinned attempt":           {pinned: other, author: true, artifacts: "", wantUnknown: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			completed := &workflowStepRecord{AuthorBase: tc.recorded, AuthorBaseUnknown: tc.unknown, AuthorCommit: tc.pinned}
			var next workflowStepRecord
			setRunAuthorBase(&next, completed, tc.author, tc.earlierAuthor, tc.artifacts)
			if next.AuthorBase != tc.want || next.AuthorBaseUnknown != tc.wantUnknown {
				t.Fatalf("next = base %q, unknown %v; want %q, %v", next.AuthorBase, next.AuthorBaseUnknown, tc.want, tc.wantUnknown)
			}
			// The same record as both (a retried attempt) gives the same answer.
			setRunAuthorBase(completed, completed, tc.author, tc.earlierAuthor, tc.artifacts)
			if completed.AuthorBase != tc.want || completed.AuthorBaseUnknown != tc.wantUnknown {
				t.Fatalf("in place = base %q, unknown %v; want %q, %v", completed.AuthorBase, completed.AuthorBaseUnknown, tc.want, tc.wantUnknown)
			}
		})
	}
}

// TestEarlierAuthorStep checks which workflow steps count as an earlier
// author step.
func TestEarlierAuthorStep(t *testing.T) {
	t.Parallel()
	steps := []domain.WorkflowStep{
		{Name: "check", Command: "check"},
		{Name: "review", Role: "verifier"},
		{Name: "author", Role: "author"},
		{Name: "polish"},
	}
	for index, want := range map[int]bool{-1: true, 0: false, 1: false, 2: false, 3: true, 4: true, 5: true} {
		if got := earlierAuthorStep(steps, index); got != want {
			t.Errorf("earlierAuthorStep(%d) = %v, want %v", index, got, want)
		}
	}
}

// TestLegacyFailedAuthorRetryFailsClosed checks a failed first author from
// before issue 214, with a git-commit but no git-base: the run's first author
// base is unknown, so neither its retry nor any later step may record one,
// and the guard after the retry is refused instead of comparing from the
// retry's own base (which would leave out the failed attempt's changes).
func TestLegacyFailedAuthorRetryFailsClosed(t *testing.T) {
	t.Parallel()
	f := newCommandFixture(t, checkCommand(0, 0), nil)
	ctx := t.Context()
	commitA, commitB, retryBase := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	now := time.Now().UTC()
	orig := domain.Task{ID: "task_legacy", IdempotencyKey: "task_legacy", WorkloadName: "coder", State: domain.TaskStateSubmitted,
		InputJSON: `{"workflow":"` + commandWorkflow + `","step_index":0,"role":"author"}`, CreatedAt: now, UpdatedAt: now}
	entry, err := f.r.workflowStepEntry(orig.ID, workflowStepRecord{RunID: orig.ID, Workflow: commandWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal([]scruntime.Artifact{{Name: "git-commit", ContentType: "text/plain", Text: commitA}})
	if err := f.st.CreateTask(ctx, orig); err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutMemory(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateTaskState(ctx, orig.ID, domain.TaskStateSubmitted, domain.TaskStateFailed, "runtime_failed", `{}`); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateTaskArtifacts(ctx, orig.ID, string(legacy)); err != nil {
		t.Fatal(err)
	}
	retry, err := f.r.RetryTaskWithHandoff(ctx, orig.ID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := f.r.workflowStep(ctx, retry.ID)
	if err != nil || reg == nil || reg.AuthorBase != "" || !reg.AuthorBaseUnknown || reg.AuthorCommit != commitA {
		t.Fatalf("retry registration = %+v, %v; want no base, the unknown state, commit %s", reg, err, commitA)
	}
	// The retry, pinned to commitA, completes and reports its own checkout base.
	retryArts, _ := json.Marshal([]scruntime.Artifact{
		{Name: "git-commit", ContentType: "text/plain", Text: commitB},
		{Name: scruntime.GitBaseArtifact, ContentType: "text/plain", Text: retryBase},
	})
	if err := f.st.UpdateTaskState(ctx, retry.ID, retry.State, domain.TaskStateCompleted, "", `{}`); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateTaskArtifacts(ctx, retry.ID, string(retryArts)); err != nil {
		t.Fatal(err)
	}
	if err := f.r.advanceWorkflowStep(ctx, retry); err != nil {
		t.Fatal(err)
	}
	var guard *domain.Task
	for _, task := range f.tasks() {
		if task.ParentTaskID == retry.ID {
			guard = &task
		}
	}
	if guard == nil {
		t.Fatal("no guard step after the retry")
	}
	reg, err = f.r.workflowStep(ctx, guard.ID)
	if err != nil || reg == nil || reg.AuthorBase != "" || !reg.AuthorBaseUnknown {
		t.Fatalf("guard registration = %+v, %v; want no author base and the unknown state", reg, err)
	}
	_, err = f.r.workflowCommand(ctx, *guard, f.bundle.Workloads["coder"])
	var refusal *scruntime.CommandRefusal
	if !errors.As(err, &refusal) || refusal.Reason != scruntime.CommandAuthorBaseUnavailable {
		t.Fatalf("guard launch = %v, want the author base refusal", err)
	}
}

// TestLegacyFirstAuthorBeforeSecondAuthorFailsClosed checks a run of author,
// author, command whose first author has a git-commit but no git-base (it
// started before issue 214). The run's first author base is unknown, so the
// second author's own checkout base must not become it: the command step is
// refused instead of comparing from a later base, which would leave out
// changes that reached the repository between the two author checkouts. The
// "registered before 214" case has the second author's registration written
// by a daemon that did not record the unknown state.
func TestLegacyFirstAuthorBeforeSecondAuthorFailsClosed(t *testing.T) {
	t.Parallel()
	for name, preUpgradeRegistration := range map[string]bool{
		"registered after 214":  false,
		"registered before 214": true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const workflow = "two-authors"
			f := newCommandFixture(t, checkCommand(0, 0), nil)
			f.bundle.Workflows[workflow] = domain.Workflow{Metadata: domain.ObjectMeta{Name: workflow}, Spec: domain.WorkflowSpec{Steps: []domain.WorkflowStep{
				{Name: "author", Workload: "coder", Role: "author", Prompt: "implement the change"},
				{Name: "polish", Workload: "coder", Role: "author", Prompt: "polish the change"},
				{Name: "check", Workload: "coder", Command: checkCommand(0, 0).Name},
			}}}
			ctx := t.Context()
			commitA, commitB, laterBase := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
			now := time.Now().UTC()
			first := domain.Task{ID: "task_legacy_first", IdempotencyKey: "task_legacy_first", WorkloadName: "coder", State: domain.TaskStateSubmitted,
				InputJSON: `{"workflow":"` + workflow + `","step_index":0,"role":"author"}`, CreatedAt: now, UpdatedAt: now}
			entry, err := f.r.workflowStepEntry(first.ID, workflowStepRecord{RunID: first.ID, Workflow: workflow})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.st.CreateTask(ctx, first); err != nil {
				t.Fatal(err)
			}
			if err := f.st.PutMemory(ctx, entry); err != nil {
				t.Fatal(err)
			}
			legacy, _ := json.Marshal([]scruntime.Artifact{{Name: "git-commit", ContentType: "text/plain", Text: commitA}})
			if err := f.st.UpdateTaskState(ctx, first.ID, domain.TaskStateSubmitted, domain.TaskStateCompleted, "", `{}`); err != nil {
				t.Fatal(err)
			}
			if err := f.st.UpdateTaskArtifacts(ctx, first.ID, string(legacy)); err != nil {
				t.Fatal(err)
			}
			if err := f.r.advanceWorkflowStep(ctx, first); err != nil {
				t.Fatal(err)
			}
			child := func(parent string) domain.Task {
				t.Helper()
				for _, task := range f.tasks() {
					if task.ParentTaskID == parent {
						return task
					}
				}
				t.Fatalf("no step after %s", parent)
				return domain.Task{}
			}
			second := child(first.ID)
			reg, err := f.r.workflowStep(ctx, second.ID)
			if err != nil || reg == nil || reg.AuthorBase != "" || !reg.AuthorBaseUnknown {
				t.Fatalf("second author registration = %+v, %v; want the first author base recorded as unknown", reg, err)
			}
			if preUpgradeRegistration {
				old, err := f.r.workflowStepEntry(second.ID, workflowStepRecord{RunID: first.ID, Workflow: workflow, Index: 1})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.st.PutMemory(ctx, old); err != nil {
					t.Fatal(err)
				}
			}
			// The second author completes and reports its own, later checkout base.
			arts, _ := json.Marshal([]scruntime.Artifact{
				{Name: "git-commit", ContentType: "text/plain", Text: commitB},
				{Name: scruntime.GitBaseArtifact, ContentType: "text/plain", Text: laterBase},
			})
			if err := f.st.UpdateTaskState(ctx, second.ID, second.State, domain.TaskStateCompleted, "", `{}`); err != nil {
				t.Fatal(err)
			}
			if err := f.st.UpdateTaskArtifacts(ctx, second.ID, string(arts)); err != nil {
				t.Fatal(err)
			}
			if err := f.r.advanceWorkflowStep(ctx, second); err != nil {
				t.Fatal(err)
			}
			guard := child(second.ID)
			reg, err = f.r.workflowStep(ctx, guard.ID)
			if err != nil || reg == nil || reg.AuthorBase != "" || !reg.AuthorBaseUnknown || reg.AuthorCommit != commitB {
				t.Fatalf("guard registration = %+v, %v; want no author base, the unknown state and commit %s", reg, err, commitB)
			}
			_, err = f.r.workflowCommand(ctx, guard, f.bundle.Workloads["coder"])
			var refusal *scruntime.CommandRefusal
			if !errors.As(err, &refusal) || refusal.Reason != scruntime.CommandAuthorBaseUnavailable {
				t.Fatalf("guard launch = %v, want the author base refusal", err)
			}
		})
	}
}
