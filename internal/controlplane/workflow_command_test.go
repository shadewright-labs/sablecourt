package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/councilproxy"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/runtime/coding"
	"github.com/shadewright-labs/sablecourt/internal/store/sqlite"
)

// commandProxy records which proxy profile each launch asked for.
type commandProxy struct {
	*cpFakeProxyDocker
	mu       sync.Mutex
	harness  []string
	leaseSet []bool
}

func (p *commandProxy) ProvisionCouncilProxy(ctx context.Context, image string, cfg councilproxy.Config) (councilproxy.LaunchSession, error) {
	p.mu.Lock()
	p.harness = append(p.harness, cfg.Harness)
	p.leaseSet = append(p.leaseSet, cfg.LeaseSHA256 != "")
	p.mu.Unlock()
	return p.cpFakeProxyDocker.ProvisionCouncilProxy(ctx, image, cfg)
}

// refusingRunner fails before any container starts, as a spec that cannot be built does.
type refusingRunner struct{ *cpFakeCouncilRunner }

func (refusingRunner) RunCouncil(context.Context, scruntime.CouncilLaunch, scruntime.CouncilRecorder) (scruntime.CouncilResult, error) {
	return scruntime.CouncilResult{}, errors.New("invalid command: argv must name a program")
}

type commandFixture struct {
	t       *testing.T
	r       *Reconciler
	st      *sqlite.SQLiteStore
	runner  *cpFakeCouncilRunner
	proxy   *commandProxy
	adapter *coding.Adapter
	bundle  *config.Bundle
}

const commandWorkflow = "checked"

var checkArgv = []string{"/bin/sh", "-c", "git diff --name-only HEAD~1 | grep -qx invariant.txt && exit 1; exit 0"}

func newCommandFixture(t *testing.T, cmd domain.CodingCommand, command func(context.Context, scruntime.CouncilLaunch) (string, string, int, error)) *commandFixture {
	t.Helper()
	ctx := t.Context()
	repo, _ := initCPTestRepo(t)
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	wl := domain.Workload{
		Metadata: domain.ObjectMeta{Name: "coder", Zone: domain.ZoneRuntime},
		Spec: domain.WorkloadSpec{
			Class:   domain.ClassCodingAgent,
			Runtime: domain.RuntimeConfig{Type: "worktree-cli"},
			Coding:  &domain.CodingConfig{Harness: "claude", RepoPath: repo, Sandbox: "container", Commands: []domain.CodingCommand{cmd}},
		},
	}
	if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: "coder", Zone: domain.ZoneRuntime, Class: domain.ClassCodingAgent}); err != nil {
		t.Fatal(err)
	}
	bundle := &config.Bundle{
		Workloads: map[string]domain.Workload{"coder": wl},
		Workflows: map[string]domain.Workflow{commandWorkflow: {Metadata: domain.ObjectMeta{Name: commandWorkflow}, Spec: domain.WorkflowSpec{Steps: []domain.WorkflowStep{
			{Name: "author", Workload: "coder", Role: "author", Prompt: "implement the change"},
			{Name: "check", Workload: "coder", Command: cmd.Name},
			{Name: "review", Workload: "coder", Role: "verifier", Prompt: "review the change"},
		}}}},
	}
	runner := newCPFakeCouncilRunner()
	runner.onRun = func(ctx context.Context, s scruntime.CouncilLaunch) (string, string, int, error) {
		switch {
		case s.Command != nil:
			return command(ctx, s)
		case s.Checkout.Kind == scruntime.CouncilTask:
			if err := os.WriteFile(filepath.Join(s.Checkout.Path, "feature.txt"), []byte("change\n"), 0o600); err != nil {
				return "", "", 1, err
			}
			return "implemented", "", 0, nil
		default:
			return "looks right\nVERDICT: PASS", "", 0, nil
		}
	}
	proxy := &commandProxy{cpFakeProxyDocker: newCPFakeProxyDocker()}
	adapter := coding.NewAdapter(filepath.Join(t.TempDir(), "worktrees"))
	adapter.CouncilRunner = runner
	adapter.CouncilProxy = proxy
	adapter.CouncilProxyImage = "sha256:" + strings.Repeat("d", 64)
	adapter.DaemonListen = net.JoinHostPort(net.IPv6loopback.String(), "7432")
	r := NewReconciler(Options{
		Store:    st,
		Bundle:   bundle,
		Policy:   policy.NewEngine(nil, nil),
		Adapters: map[domain.WorkloadClass]scruntime.Adapter{domain.ClassCodingAgent: adapter},
	})
	return &commandFixture{t: t, r: r, st: st, runner: runner, proxy: proxy, adapter: adapter, bundle: bundle}
}

// runUntil drives reconcile passes until done reports true.
func (f *commandFixture) runUntil(what string, done func() bool) {
	f.t.Helper()
	ctx := f.t.Context()
	deadline := time.Now().Add(8 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		_ = errors.Join(f.r.ReconcileOnce(ctx), waitForLaunches(f.t, f.r))
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *commandFixture) tasks() []domain.Task {
	f.t.Helper()
	tasks, err := f.st.ListTasks(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	return tasks
}

// step returns the task that ran the workflow step at index, if any.
func (f *commandFixture) step(index int) (domain.Task, bool) {
	for _, task := range f.tasks() {
		var meta struct {
			Workflow  string `json:"workflow"`
			StepIndex int    `json:"step_index"`
		}
		if json.Unmarshal([]byte(task.InputJSON), &meta) == nil && meta.Workflow == commandWorkflow && meta.StepIndex == index {
			return task, true
		}
	}
	return domain.Task{}, false
}

func terminal(task domain.Task, ok bool) bool { return ok && terminalTaskState(task.State) }

func (f *commandFixture) events(id string) []domain.TaskEvent {
	f.t.Helper()
	events, err := f.st.ListTaskEvents(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return events
}

func (f *commandFixture) event(id, reason string) (domain.TaskEvent, bool) {
	for _, ev := range f.events(id) {
		if ev.Reason == reason {
			return ev, true
		}
	}
	return domain.TaskEvent{}, false
}

func (f *commandFixture) commandLaunches() []scruntime.CouncilLaunch {
	f.runner.mu.Lock()
	defer f.runner.mu.Unlock()
	var out []scruntime.CouncilLaunch
	for _, l := range f.runner.launches {
		if l.Command != nil {
			out = append(out, l)
		}
	}
	return out
}

func (f *commandFixture) start() domain.Task {
	f.t.Helper()
	root, err := f.r.StartWorkflow(f.t.Context(), commandWorkflow, "add the feature")
	if err != nil {
		f.t.Fatal(err)
	}
	return root
}

func (f *commandFixture) runStatus(rootID string) workflowStepRecord {
	f.t.Helper()
	rec, err := f.r.workflowStep(f.t.Context(), rootID)
	if err != nil || rec == nil {
		f.t.Fatalf("run registration: %v", err)
	}
	return *rec
}

func checkCommand(maxOutput, timeoutSeconds int) domain.CodingCommand {
	return domain.CodingCommand{Name: "check", Argv: checkArgv, TimeoutSeconds: timeoutSeconds, MaxOutputBytes: maxOutput, FailureReason: "the diff touches an invariant path: use the coding-invariant workflow"}
}

// TestCommandStepRunsInHarnessContainerAndAdvances checks the allowed case:
// after the author, the command runs in a harness container on a read-only
// checkout of the author's commit, with the manifest's argv, no prompt, no
// model and a proxy that allows no provider; its result is stored as a
// durable artifact, and the reviewer after it is built from the author.
func TestCommandStepRunsInHarnessContainerAndAdvances(t *testing.T) {
	t.Parallel()
	f := newCommandFixture(t, checkCommand(0, 0), func(context.Context, scruntime.CouncilLaunch) (string, string, int, error) {
		return "no invariant path touched", "", 0, nil
	})
	root := f.start()
	f.runUntil("the review step", func() bool { return terminal(f.step(2)) })

	author, _ := f.step(0)
	check, _ := f.step(1)
	review, _ := f.step(2)
	if author.ID != root.ID || check.State != domain.TaskStateCompleted || review.State != domain.TaskStateCompleted {
		t.Fatalf("states: author %s, check %s, review %s", author.State, check.State, review.State)
	}
	launches := f.commandLaunches()
	if len(launches) != 1 {
		t.Fatalf("command launches = %d, want 1", len(launches))
	}
	l := launches[0]
	if !slices.Equal(l.Command, checkArgv) || l.Prompt != "" || l.Model != "" || l.Checkout.Kind != scruntime.CouncilReview || l.TaskToken == "" {
		t.Fatalf("command launch = %+v", l)
	}
	if l.Limits.WallTime <= 0 || l.Limits.WallTime > domain.DefaultCommandTimeout {
		t.Fatalf("command wall time = %s, want the command's limit", l.Limits.WallTime)
	}
	f.proxy.mu.Lock()
	harnesses := slices.Clone(f.proxy.harness)
	f.proxy.mu.Unlock()
	if !slices.Contains(harnesses, councilproxy.CommandHarness) || slices.Index(harnesses, councilproxy.CommandHarness) != 1 {
		t.Fatalf("proxy profiles = %v, want the command profile for the second launch", harnesses)
	}
	res, ok := CommandStepResult(check)
	if !ok || res.Command != "check" || res.Outcome != scruntime.CommandPassed || res.ExitCode != 0 || res.Output != "no invariant path touched" {
		t.Fatalf("command result = %+v, %v", res, ok)
	}
	// The pinned commit is the author's, and the reviewer sees the author.
	commit := authorCommit(author.ArtifactsJSON)
	reg, err := f.r.workflowStep(t.Context(), check.ID)
	if err != nil || reg == nil || commit == "" || reg.AuthorCommit != commit {
		t.Fatalf("command registration = %+v, author commit %q, err %v", reg, commit, err)
	}
	var reviewInput struct {
		AuthorTaskID    string `json:"author_task_id"`
		AuthorStepIndex int    `json:"author_step_index"`
		Prompt          string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(review.InputJSON), &reviewInput); err != nil {
		t.Fatal(err)
	}
	if reviewInput.AuthorTaskID != author.ID || reviewInput.AuthorStepIndex != 0 || !strings.Contains(reviewInput.Prompt, author.ID) {
		t.Fatalf("review input = %+v, want the author task", reviewInput)
	}
	if status := f.runStatus(root.ID); status.Status != "" {
		t.Fatalf("run status = %+v, want running", status)
	}
}

// TestCommandStepFailureHaltsRun covers every outcome that fails a command
// step: a nonzero exit, output over the cap (even with exit 0) and the time
// limit. Each fails the step, records the outcome, stores no more than the
// cap of redacted output, and halts the run with the operator's reason.
func TestCommandStepFailureHaltsRun(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("Q", 36)
	for name, tc := range map[string]struct {
		cmd       domain.CodingCommand
		run       func(context.Context, scruntime.CouncilLaunch) (string, string, int, error)
		outcome   string
		exitCode  int
		maxOutput int
	}{
		"nonzero exit": {
			cmd: checkCommand(0, 0),
			run: func(context.Context, scruntime.CouncilLaunch) (string, string, int, error) {
				return "invariant.txt", "token " + secret, 1, nil
			},
			outcome: scruntime.CommandFailed, exitCode: 1, maxOutput: domain.DefaultCommandOutputBytes,
		},
		"output over cap": {
			cmd: checkCommand(64, 0),
			run: func(context.Context, scruntime.CouncilLaunch) (string, string, int, error) {
				return strings.Repeat("x", 200) + " " + secret, "", 0, nil
			},
			outcome: scruntime.CommandOutputOverCap, exitCode: 0, maxOutput: 64,
		},
		"timeout": {
			cmd: checkCommand(0, 1),
			run: func(ctx context.Context, _ scruntime.CouncilLaunch) (string, string, int, error) {
				<-ctx.Done()
				return "", "", 0, ctx.Err()
			},
			outcome: scruntime.CommandTimedOut, exitCode: -1, maxOutput: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCommandFixture(t, tc.cmd, tc.run)
			root := f.start()
			f.runUntil("the command step", func() bool {
				_, halted := f.event(root.ID, "workflow_halted_command_failed")
				return terminal(f.step(1)) && halted
			})
			check, _ := f.step(1)
			if check.State != domain.TaskStateFailed {
				t.Fatalf("command step state = %s, want failed", check.State)
			}
			if _, ok := f.step(2); ok {
				t.Fatal("the review step started after a failed command step")
			}
			res, ok := CommandStepResult(check)
			if !ok || res.Outcome != tc.outcome || res.ExitCode != tc.exitCode {
				t.Fatalf("command result = %+v, %v; want outcome %s, exit %d", res, ok, tc.outcome, tc.exitCode)
			}
			if len(res.Output) > tc.maxOutput || strings.Contains(check.ArtifactsJSON, secret) {
				t.Fatalf("stored output is over the cap or unredacted: %q", res.Output)
			}
			if tc.outcome == scruntime.CommandOutputOverCap && !res.OutputTruncated {
				t.Fatal("output over the cap was not marked truncated")
			}
			ev, _ := f.event(root.ID, "workflow_halted_command_failed")
			var fact struct {
				CommandTaskID, Step, Command, Outcome string
				ExitCode                              *int   `json:"exit_code"`
				FailureReason                         string `json:"failure_reason"`
			}
			if err := json.Unmarshal([]byte(ev.FactJSON), &fact); err != nil {
				t.Fatal(err)
			}
			if fact.Command != "check" || fact.Step != "check" || fact.Outcome != tc.outcome || fact.ExitCode == nil || *fact.ExitCode != tc.exitCode ||
				!strings.Contains(fact.FailureReason, "coding-invariant") {
				t.Fatalf("halt fact = %s", ev.FactJSON)
			}
			if status := f.runStatus(root.ID); status.Status != "halted" || status.Reason != "command_failed" {
				t.Fatalf("run status = %+v, want halted by command_failed", status)
			}
		})
	}
}

// TestCommandStepRefusedBeforeAnyContainer covers the refusals of the
// decision table that stop a command before it runs: no container runtime
// (no host fallback, whatever host mode says) and a container spec that
// cannot be built. Each fails the step with a fixed reason and halts the run.
func TestCommandStepRefusedBeforeAnyContainer(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		swap    func(f *commandFixture)
		refusal string
	}{
		"no container runtime": {
			swap: func(f *commandFixture) {
				hostOnly := coding.NewAdapter(f.adapter.WorktreeRoot)
				hostOnly.HostModeUnsafe = true
				f.r.adapters[domain.ClassCodingAgent] = hostOnly
			},
			refusal: scruntime.CommandNoContainerRuntime,
		},
		"spec cannot be built": {
			swap:    func(f *commandFixture) { f.adapter.CouncilRunner = refusingRunner{f.runner} },
			refusal: scruntime.CommandContainerRefused,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCommandFixture(t, checkCommand(0, 0), func(context.Context, scruntime.CouncilLaunch) (string, string, int, error) {
				t.Error("the command ran")
				return "", "", 0, nil
			})
			root := f.start()
			f.runUntil("the author step", func() bool { _, ok := f.step(1); return terminal(f.step(0)) && ok })
			tc.swap(f)
			f.runUntil("the refusal", func() bool {
				_, halted := f.event(root.ID, "workflow_halted_command_refused")
				return terminal(f.step(1)) && halted
			})
			check, _ := f.step(1)
			ev, ok := f.event(check.ID, "command_refused")
			if check.State != domain.TaskStateFailed || !ok {
				t.Fatalf("command step state = %s, refusal event %v", check.State, ok)
			}
			var fact struct{ Reason, Refusal, Command string }
			if err := json.Unmarshal([]byte(ev.FactJSON), &fact); err != nil || fact.Reason != "command_refused" || fact.Refusal != tc.refusal || fact.Command != "check" {
				t.Fatalf("refusal fact = %s", ev.FactJSON)
			}
			if len(f.commandLaunches()) != 0 {
				t.Fatal("a refused command reached the container runner")
			}
			halt, _ := f.event(root.ID, "workflow_halted_command_refused")
			if !strings.Contains(halt.FactJSON, `"refusal":"`+tc.refusal+`"`) || !strings.Contains(halt.FactJSON, "coding-invariant") {
				t.Fatalf("halt fact = %s", halt.FactJSON)
			}
			if status := f.runStatus(root.ID); status.Status != "halted" || status.Reason != "command_refused" {
				t.Fatalf("run status = %+v", status)
			}
		})
	}
}

// TestWorkflowCommandResolvesArgvOnlyFromManifest checks the last row of the
// decision table: a command outside the allowed set, or one a task's input
// names without a matching step registration, is refused, and argv in the
// input is never used.
func TestWorkflowCommandResolvesArgvOnlyFromManifest(t *testing.T) {
	t.Parallel()
	f := newCommandFixture(t, checkCommand(0, 0), nil)
	ctx := t.Context()
	wl := f.bundle.Workloads["coder"]
	commit := strings.Repeat("a", 40)
	register := func(id string, rec workflowStepRecord) {
		entry, err := f.r.workflowStepEntry(id, rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.st.PutMemory(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	register("task_registered", workflowStepRecord{RunID: "task_root", Workflow: commandWorkflow, Index: 1, AuthorCommit: commit})
	register("task_no_commit", workflowStepRecord{RunID: "task_root", Workflow: commandWorkflow, Index: 1})
	register("task_wrong_index", workflowStepRecord{RunID: "task_root", Workflow: commandWorkflow, Index: 2, AuthorCommit: commit})
	stepInput := func(extra string) string {
		return `{"workflow":"` + commandWorkflow + `","step_index":1,"command":"check"` + extra + `}`
	}
	missing := *wl.Spec.Coding
	missing.Commands = nil
	noCommands := wl
	noCommands.Spec.Coding = &missing

	for name, tc := range map[string]struct {
		id, input string
		wl        domain.Workload
		want      []string // nil means not a command step
		refused   bool
	}{
		"ordinary task":                 {id: "task_plain", input: `{"prompt":"edit"}`, wl: wl},
		"registered step":               {id: "task_registered", input: stepInput(""), wl: wl, want: checkArgv},
		"argv in input is ignored":      {id: "task_registered", input: stepInput(`,"argv":["/bin/rm","-rf","/"]`), wl: wl, want: checkArgv},
		"input names a command":         {id: "task_plain", input: `{"prompt":"edit","command":"check"}`, wl: wl, refused: true},
		"input names a command in case": {id: "task_plain", input: `{"prompt":"edit","Command":"check"}`, wl: wl, refused: true},
		"input names another command":   {id: "task_registered", input: `{"workflow":"` + commandWorkflow + `","step_index":1,"command":"deploy"}`, wl: wl, refused: true},
		"step without registration":     {id: "task_unregistered", input: stepInput(""), wl: wl, refused: true},
		"registration without commit":   {id: "task_no_commit", input: stepInput(""), wl: wl, refused: true},
		"registration for another step": {id: "task_wrong_index", input: stepInput(""), wl: wl, refused: true},
		"command not in allowed set":    {id: "task_registered", input: stepInput(""), wl: noCommands, refused: true},
		"command step input dropped":    {id: "task_registered", input: `{"workflow":"` + commandWorkflow + `","step_index":1}`, wl: wl, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			task := domain.Task{ID: tc.id, WorkloadName: "coder", InputJSON: tc.input}
			spec, err := f.r.workflowCommand(ctx, task, tc.wl)
			var refusal *scruntime.CommandRefusal
			switch {
			case tc.refused:
				if !errors.As(err, &refusal) || refusal.Reason != scruntime.CommandNotAllowed || spec != nil {
					t.Fatalf("spec %+v, err %v; want a command_not_allowed refusal", spec, err)
				}
			case tc.want == nil:
				if err != nil || spec != nil {
					t.Fatalf("spec %+v, err %v; want no command", spec, err)
				}
			default:
				if err != nil || spec == nil || !slices.Equal(spec.Argv, tc.want) || spec.Timeout != domain.DefaultCommandTimeout || spec.MaxOutputBytes != domain.DefaultCommandOutputBytes {
					t.Fatalf("spec %+v, err %v; want the manifest argv %v", spec, err, tc.want)
				}
			}
		})
	}
}

// TestCommandStepRefusalOnSubmittedTask runs a directly submitted task that
// names a command through the reconciler: it fails with a refusal and never
// reaches the container runner.
func TestCommandStepRefusalOnSubmittedTask(t *testing.T) {
	t.Parallel()
	f := newCommandFixture(t, checkCommand(0, 0), func(context.Context, scruntime.CouncilLaunch) (string, string, int, error) {
		t.Error("the command ran")
		return "", "", 0, nil
	})
	task, err := f.r.SubmitTask(t.Context(), "coder", "direct-1", `{"prompt":"edit","command":"check","argv":["/bin/sh"]}`)
	if err != nil {
		t.Fatal(err)
	}
	f.runUntil("the refusal", func() bool {
		got, err := f.st.GetTask(t.Context(), task.ID)
		return err == nil && terminalTaskState(got.State)
	})
	requireState(t, f.st, task.ID, domain.TaskStateFailed)
	ev, ok := f.event(task.ID, "command_refused")
	if !ok || !strings.Contains(ev.FactJSON, `"refusal":"`+scruntime.CommandNotAllowed+`"`) {
		t.Fatalf("refusal event %v: %s", ok, ev.FactJSON)
	}
	f.runner.mu.Lock()
	defer f.runner.mu.Unlock()
	if len(f.runner.launches) != 0 {
		t.Fatal("a refused task reached the container runner")
	}
}

// TestCommandStepQuotedSecretDoesNotCorruptResultJSON checks that a passed
// command whose output holds a quoted secret shape keeps a valid stored
// result and does not halt the run.
func TestCommandStepQuotedSecretDoesNotCorruptResultJSON(t *testing.T) {
	t.Parallel()
	f := newCommandFixture(t, checkCommand(0, 0), func(context.Context, scruntime.CouncilLaunch) (string, string, int, error) {
		return `err: "password=supersecret"`, "", 0, nil
	})
	root := f.start()
	f.runUntil("the review step", func() bool { return terminal(f.step(2)) })
	check, _ := f.step(1)
	res, ok := CommandStepResult(check)
	if !ok || res.Outcome != scruntime.CommandPassed || res.ExitCode != 0 {
		t.Fatalf("command result corrupted: ok=%v res=%+v artifacts=%s", ok, res, check.ArtifactsJSON)
	}
	if status := f.runStatus(root.ID); status.Status != "" {
		t.Fatalf("passed command step halted run: %+v", status)
	}
}

// TestCommandStepMissingProxyHaltsRun checks that a command step whose
// container launch cannot be set up halts the run.
func TestCommandStepMissingProxyHaltsRun(t *testing.T) {
	t.Parallel()
	f := newCommandFixture(t, checkCommand(0, 0), func(context.Context, scruntime.CouncilLaunch) (string, string, int, error) {
		t.Error("the command ran")
		return "", "", 0, nil
	})
	root := f.start()
	f.runUntil("author step", func() bool { _, ok := f.step(1); return terminal(f.step(0)) && ok })
	f.adapter.CouncilProxy = nil
	f.runUntil("command failure", func() bool { return terminal(f.step(1)) })
	if status := f.runStatus(root.ID); status.Status != "halted" {
		t.Fatalf("run status = %+v, want halted", status)
	}
}

// TestRedactArtifactTextKeepsJSONValid checks that a JSON artifact is
// redacted value by value: the result always decodes, secrets in values and
// sensitive fields are gone, and plain text keeps the plain-text rules.
func TestRedactArtifactTextKeepsJSONValid(t *testing.T) {
	t.Parallel()
	token := "ghp_" + strings.Repeat("T", 36)
	for name, tc := range map[string]struct {
		contentType, in string
		wantGone        []string
		wantKept        []string
	}{
		"quoted kv in a value":     {contentType: "application/json", in: `{"output":"err: \"password=supersecret\""}`, wantGone: []string{"supersecret"}, wantKept: []string{`password=`}},
		"redacted marker in value": {contentType: "application/json", in: `{"output":"err: \"password=[REDACTED:secret]\""}`, wantKept: []string{`password=[REDACTED:secret]`}},
		"escape after short value": {contentType: "application/json", in: `{"output":"password=12345\nnext"}`, wantKept: []string{`password=12345`, "next"}},
		"token next to a kv":       {contentType: "application/json", in: `{"output":"` + token + ` api_key=abcdef123"}`, wantGone: []string{token, "abcdef123"}},
		"sensitive field":          {contentType: "application/json", in: `{"password":"hunter22","exit_code":0}`, wantGone: []string{"hunter22"}, wantKept: []string{`"exit_code":0`}},
		"nested array":             {contentType: "application/json", in: `{"lines":["ok","` + token + `"]}`, wantGone: []string{token}, wantKept: []string{`"ok"`}},
		"invalid json":             {contentType: "application/json", in: `{"output":"` + token, wantGone: []string{token}},
		"plain text":               {contentType: "text/plain", in: "token " + token, wantGone: []string{token}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := redactArtifactText(tc.contentType, tc.in)
			if tc.contentType == "application/json" && json.Valid([]byte(tc.in)) && !json.Valid([]byte(got)) {
				t.Fatalf("redacted JSON does not decode: %s", got)
			}
			for _, s := range tc.wantGone {
				if strings.Contains(got, s) {
					t.Fatalf("%q survived redaction: %s", s, got)
				}
			}
			for _, s := range tc.wantKept {
				if !strings.Contains(got, s) {
					t.Fatalf("%q lost: %s", s, got)
				}
			}
		})
	}
}

// TestCommandStepLostOnRestartHaltsRun checks that a command step failed by
// recovery, as after a daemon restart while the command ran, halts its run.
func TestCommandStepLostOnRestartHaltsRun(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	f := newCommandFixture(t, checkCommand(0, 0), func(ctx context.Context, _ scruntime.CouncilLaunch) (string, string, int, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "", "", 0, nil
	})
	defer close(release)
	root := f.start()
	f.runUntil("the running command", func() bool {
		check, ok := f.step(1)
		return ok && check.State == domain.TaskStateWorking && len(f.commandLaunches()) == 1
	})
	check, _ := f.step(1)
	if err := f.r.failLostTask(t.Context(), check, `{"reason":"container_not_alive"}`); err != nil {
		t.Fatal(err)
	}
	if status := f.runStatus(root.ID); status.Status != "halted" || status.Reason != "command_failed" {
		t.Fatalf("run status = %+v, want halted by command_failed", status)
	}
	if _, ok := f.event(root.ID, "workflow_halted_command_failed"); !ok {
		t.Fatal("no halt event on the run")
	}
}

// TestCommandStepSkipsVerifierContext checks that a command step builds no
// verifier context packet: it runs no host pre-checks and records no
// verifier_checkout_pinned event, since no model reads a prompt.
func TestCommandStepSkipsVerifierContext(t *testing.T) {
	t.Parallel()
	f := newCommandFixture(t, checkCommand(0, 0), func(context.Context, scruntime.CouncilLaunch) (string, string, int, error) {
		return "ok", "", 0, nil
	})
	f.start()
	f.runUntil("the command step", func() bool { return terminal(f.step(1)) })
	check, _ := f.step(1)
	if check.State != domain.TaskStateCompleted {
		t.Fatalf("command step state = %s", check.State)
	}
	if ev, ok := f.event(check.ID, "verifier_checkout_pinned"); ok {
		t.Fatalf("command step recorded a verifier context packet: %s", ev.FactJSON)
	}
}
