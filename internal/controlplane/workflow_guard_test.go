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
			if err != nil || reg == nil || reg.AuthorBase != base || reg.AuthorBaseState != authorBaseRecorded || reg.AuthorCommit != authorCommit(author.ArtifactsJSON) {
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
	orig := failed("task_first", first, commitA, workflowStepRecord{RunID: "task_first", Workflow: commandWorkflow, AuthorBaseState: authorBasePending})
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

// TestSetRunAuthorBase checks which step sets the run's author base and its
// state: only the run's first author attempt, only once, and only on a run
// whose registrations carry the state (written with the issue 214 rule). The
// step's kind comes from its own input, never from the loaded bundle.
func TestSetRunAuthorBase(t *testing.T) {
	t.Parallel()
	base, other := strings.Repeat("1", 40), strings.Repeat("2", 40)
	// arts is an author's artifacts: its commit and these bases.
	arts := func(texts ...string) string {
		items := []scruntime.Artifact{{Name: "git-commit", ContentType: "text/plain", Text: strings.Repeat("3", 40)}}
		for _, text := range texts {
			items = append(items, scruntime.Artifact{Name: scruntime.GitBaseArtifact, ContentType: "text/plain", Text: text})
		}
		raw, _ := json.Marshal(items)
		return string(raw)
	}
	baseOnly := `[{"name":"git-base","contentType":"text/plain","text":"` + base + `"}]`
	const (
		author   = `{"workflow":"w","step_index":0,"role":"author"}`
		unnamed  = `{"workflow":"w","step_index":0}`
		verifier = `{"workflow":"w","step_index":1,"role":"verifier"}`
		command  = `{"workflow":"w","step_index":1,"command":"check"}`
	)
	const (
		pending  = authorBasePending
		recorded = authorBaseRecorded
		unknown  = authorBaseUnknown
	)
	done, failed, cancelled := domain.TaskStateCompleted, domain.TaskStateFailed, domain.TaskStateCancelled
	for name, tc := range map[string]struct {
		state, base, pinned string
		input               string
		taskState           domain.TaskState
		artifacts           string
		want, wantState     string
	}{
		"first author records its base":               {state: pending, input: author, taskState: done, artifacts: arts(base), want: base, wantState: recorded},
		"an author without a role records its base":   {state: pending, input: unnamed, taskState: done, artifacts: arts(base), want: base, wantState: recorded},
		"a failed first author with a commit records": {state: pending, input: author, taskState: failed, artifacts: arts(base), want: base, wantState: recorded},
		"a failed attempt with only a base records":   {state: pending, input: author, taskState: failed, artifacts: baseOnly, want: base, wantState: recorded},
		"a failed attempt with nothing stays pending": {state: pending, input: author, taskState: failed, artifacts: "", wantState: pending},
		"a failed attempt with other artifacts":       {state: pending, input: author, taskState: failed, artifacts: `[{"name":"git-status","contentType":"text/plain","text":""}]`, wantState: pending},
		"a cancelled attempt with nothing":            {state: pending, input: author, taskState: cancelled, artifacts: "[]", wantState: pending},
		"a completed author with nothing is unknown":  {state: pending, input: author, taskState: done, artifacts: "", wantState: unknown},
		"a failed attempt with unreadable artifacts":  {state: pending, input: author, taskState: failed, artifacts: "{", wantState: unknown},
		"a failed attempt with an invalid base":       {state: pending, input: author, taskState: failed, artifacts: `[{"name":"git-base","contentType":"text/plain","text":"HEAD"}]`, wantState: unknown},
		"a pinned author while pending is unknown":    {state: pending, pinned: other, input: author, taskState: done, artifacts: arts(other), wantState: unknown},
		"a later author keeps the first":              {state: recorded, base: base, input: author, taskState: done, artifacts: arts(other), want: base, wantState: recorded},
		"a verifier passes the base on":               {state: recorded, base: base, input: verifier, taskState: done, artifacts: arts(other), want: base, wantState: recorded},
		"a recorded state without a base is unknown":  {state: recorded, input: author, taskState: done, artifacts: arts(base), wantState: unknown},
		"a recorded state with a bad base is unknown": {state: recorded, base: "HEAD", input: author, taskState: done, artifacts: arts(base), wantState: unknown},
		"a verifier before any author stays pending":  {state: pending, input: verifier, taskState: done, artifacts: arts(base), wantState: pending},
		"a command before any author stays pending":   {state: pending, input: command, taskState: done, artifacts: arts(base), wantState: pending},
		"unreadable input is unknown":                 {state: pending, input: "edit the file", taskState: done, artifacts: arts(base), wantState: unknown},
		"unknown stays unknown after a later author":  {state: unknown, input: author, taskState: done, artifacts: arts(other), wantState: unknown},
		"unknown stays unknown after a verifier":      {state: unknown, input: verifier, taskState: done, artifacts: arts(other), wantState: unknown},
		"an unrecognised state is unknown":            {state: "set", base: base, input: author, taskState: done, artifacts: arts(base), wantState: unknown},
		"pre-guard, no base, is unknown":              {input: author, taskState: done, artifacts: arts(base), wantState: unknown},
		"pre-guard with a later author's base":        {base: other, input: author, taskState: done, artifacts: arts(base), wantState: unknown},
		"pre-guard before a verifier":                 {base: other, input: verifier, taskState: done, artifacts: arts(base), wantState: unknown},
		"two base artifacts":                          {state: pending, input: author, taskState: done, artifacts: arts(base, other), wantState: unknown},
		"base not a commit ID":                        {state: pending, input: author, taskState: done, artifacts: arts("HEAD"), wantState: unknown},
		"a commit and no base":                        {state: pending, input: author, taskState: done, artifacts: arts(), wantState: unknown},
		"a failed commit and no base":                 {state: pending, input: author, taskState: failed, artifacts: arts(), wantState: unknown},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			completed := &workflowStepRecord{AuthorBase: tc.base, AuthorBaseState: tc.state, AuthorCommit: tc.pinned}
			task := domain.Task{InputJSON: tc.input, State: tc.taskState, ArtifactsJSON: tc.artifacts}
			var next workflowStepRecord
			setRunAuthorBase(&next, completed, task)
			if next.AuthorBase != tc.want || next.AuthorBaseState != tc.wantState {
				t.Fatalf("next = base %q, state %q; want %q, %q", next.AuthorBase, next.AuthorBaseState, tc.want, tc.wantState)
			}
			// The same record as both (a retried attempt) gives the same answer.
			setRunAuthorBase(completed, completed, task)
			if completed.AuthorBase != tc.want || completed.AuthorBaseState != tc.wantState {
				t.Fatalf("in place = base %q, state %q; want %q, %q", completed.AuthorBase, completed.AuthorBaseState, tc.want, tc.wantState)
			}
		})
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
	if err != nil || reg == nil || reg.AuthorBase != "" || reg.AuthorBaseState != authorBaseUnknown || reg.AuthorCommit != commitA {
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
	if err != nil || reg == nil || reg.AuthorBase != "" || reg.AuthorBaseState != authorBaseUnknown {
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
			entry, err := f.r.workflowStepEntry(first.ID, workflowStepRecord{RunID: first.ID, Workflow: workflow, AuthorBaseState: authorBasePending})
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
			if err != nil || reg == nil || reg.AuthorBase != "" || reg.AuthorBaseState != authorBaseUnknown {
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
			if err != nil || reg == nil || reg.AuthorBase != "" || reg.AuthorBaseState != authorBaseUnknown || reg.AuthorCommit != commitB {
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

// putFailedStep stores a failed workflow step task with its registration and
// artifacts, as a daemon would have left them.
func putFailedStep(t *testing.T, f *commandFixture, id, input, artifacts string, rec workflowStepRecord) domain.Task {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	task := domain.Task{ID: id, IdempotencyKey: id, WorkloadName: "coder", State: domain.TaskStateSubmitted, InputJSON: input, CreatedAt: now, UpdatedAt: now}
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
	if err := f.st.UpdateTaskArtifacts(ctx, id, artifacts); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestPreGuardRegistrationWithBaseFailsClosed checks a command step
// registration written by a daemon that recorded author bases but not their
// provenance (before the first author rule of issue 214): it may name a later
// author's base, so the step is refused even when the base would pass the
// ancestry check, and so is every step after a pre-guard registration.
func TestPreGuardRegistrationWithBaseFailsClosed(t *testing.T) {
	t.Parallel()
	f := newCommandFixture(t, checkCommand(0, 0), nil)
	ctx := t.Context()
	commit, laterBase := strings.Repeat("a", 40), strings.Repeat("c", 40)
	entry, err := f.r.workflowStepEntry("task_pre_guard", workflowStepRecord{RunID: "task_root", Workflow: commandWorkflow, Index: 1, AuthorCommit: commit, AuthorBase: laterBase})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutMemory(ctx, entry); err != nil {
		t.Fatal(err)
	}
	task := domain.Task{ID: "task_pre_guard", WorkloadName: "coder", InputJSON: `{"workflow":"` + commandWorkflow + `","step_index":1,"command":"check"}`}
	_, err = f.r.workflowCommand(ctx, task, f.bundle.Workloads["coder"])
	var refusal *scruntime.CommandRefusal
	if !errors.As(err, &refusal) || refusal.Reason != scruntime.CommandAuthorBaseUnavailable {
		t.Fatalf("pre-guard registration launch = %v, want the author base refusal", err)
	}
}

// TestFailedFirstAuthorBaseWithoutCommitIsKept checks a failed first author
// that recorded where its checkout started but handed on no commit: its base
// is the run's, and a retry checked out from a later base does not replace
// it. (The coding runtime emits git-base only together with git-commit, so
// this guards the rule, not a path seen today.)
func TestFailedFirstAuthorBaseWithoutCommitIsKept(t *testing.T) {
	t.Parallel()
	f := newCommandFixture(t, checkCommand(0, 0), nil)
	first := strings.Repeat("1", 40)
	arts, _ := json.Marshal([]scruntime.Artifact{{Name: scruntime.GitBaseArtifact, ContentType: "text/plain", Text: first}})
	orig := putFailedStep(t, f, "task_base_only", `{"workflow":"`+commandWorkflow+`","step_index":0,"role":"author"}`, string(arts),
		workflowStepRecord{RunID: "task_base_only", Workflow: commandWorkflow, AuthorBaseState: authorBasePending})
	retry, err := f.r.RetryTaskWithHandoff(t.Context(), orig.ID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := f.r.workflowStep(t.Context(), retry.ID)
	if err != nil || reg == nil || reg.AuthorBase != first || reg.AuthorBaseState != authorBaseRecorded || reg.AuthorCommit != "" {
		t.Fatalf("retry registration = %+v, %v; want recorded base %s and no pinned commit", reg, err, first)
	}
}

// TestFirstAuthorRetryAcrossReload checks that which author is first comes
// from the run's recorded history, not the loaded bundle: a failed first
// author retried while its workflow is not loaded still records its base, and
// the command step after the retry, once the workflow is back, compares from
// it.
func TestFirstAuthorRetryAcrossReload(t *testing.T) {
	t.Parallel()
	f := newCommandFixture(t, checkCommand(0, 0), nil)
	ctx := t.Context()
	first, commitA, commitB := strings.Repeat("1", 40), strings.Repeat("a", 40), strings.Repeat("b", 40)
	arts := func(commit, base string) string {
		raw, _ := json.Marshal([]scruntime.Artifact{
			{Name: "git-commit", ContentType: "text/plain", Text: commit},
			{Name: scruntime.GitBaseArtifact, ContentType: "text/plain", Text: base},
		})
		return string(raw)
	}
	orig := putFailedStep(t, f, "task_reload", `{"workflow":"`+commandWorkflow+`","step_index":0,"role":"author"}`, arts(commitA, first),
		workflowStepRecord{RunID: "task_reload", Workflow: commandWorkflow, AuthorBaseState: authorBasePending})
	f.r.Reload(&config.Bundle{Workloads: f.bundle.Workloads, Workflows: map[string]domain.Workflow{}}, policy.NewEngine(nil, nil))
	retry, err := f.r.RetryTaskWithHandoff(ctx, orig.ID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	f.r.Reload(f.bundle, policy.NewEngine(nil, nil))
	reg, err := f.r.workflowStep(ctx, retry.ID)
	if err != nil || reg == nil || reg.AuthorBase != first || reg.AuthorBaseState != authorBaseRecorded || reg.AuthorCommit != commitA {
		t.Fatalf("retry registration = %+v, %v; want recorded base %s, commit %s", reg, err, first, commitA)
	}
	if err := f.st.UpdateTaskState(ctx, retry.ID, retry.State, domain.TaskStateCompleted, "", `{}`); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateTaskArtifacts(ctx, retry.ID, arts(commitB, commitA)); err != nil {
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
		t.Fatal("no command step after the retry")
	}
	spec, err := f.r.workflowCommand(ctx, *guard, f.bundle.Workloads["coder"])
	if err != nil || spec == nil || spec.AuthorBase != first {
		t.Fatalf("command launch = %+v, %v; want base %s", spec, err, first)
	}
}
