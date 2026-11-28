package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/modelfamily"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func TestDispatchedRoute(t *testing.T) {
	coder := domain.Workload{Spec: domain.WorkloadSpec{Class: domain.ClassCodingAgent, Coding: &domain.CodingConfig{Harness: "claude"}}}
	det := domain.Workload{Spec: domain.WorkloadSpec{Class: domain.ClassDeterministicController}}
	tier := dispatchRoute{Provider: "cli:claude", Model: "claude-sonnet-5-5", Family: "", Source: routeSourceTierDefault}
	custom := dispatchRoute{Provider: "cli:agy", Model: "gemini-x", Family: "google", Source: routeSourceTier}
	cases := []struct {
		name    string
		wl      domain.Workload
		command bool
		tier    dispatchRoute
		input   string
		want    dispatchRoute
	}{
		{"tier route kept", coder, false, tier, `{"provider":"cli:claude","model":"claude-sonnet-5-5"}`, tier},
		{"operator family kept with its model", coder, false, custom, `{"provider":"cli:agy","model":"gemini-x"}`, custom},
		{"explicit model without tier", coder, false, dispatchRoute{}, `{"model":"gpt-6.2-astra"}`, dispatchRoute{Model: "gpt-6.2-astra", Source: routeSourceInput}},
		{"model trimmed like the harness does", coder, false, dispatchRoute{}, `{"model":" gpt-6 "}`, dispatchRoute{Model: "gpt-6", Source: routeSourceInput}},
		{"input model differs from tier: family dropped", coder, false, custom, `{"provider":"cli:agy","model":"gemini-y"}`, dispatchRoute{Provider: "cli:agy", Model: "gemini-y", Source: routeSourceInput}},
		{"input provider differs from tier: family dropped", coder, false, custom, `{"provider":"cli:other","model":"gemini-x"}`, dispatchRoute{Provider: "cli:other", Model: "gemini-x", Source: routeSourceInput}},
		{"no model", coder, false, dispatchRoute{}, `{"prompt":"x"}`, dispatchRoute{Source: routeSourceNone}},
		{"blank model", coder, false, dispatchRoute{}, `{"model":"  "}`, dispatchRoute{Source: routeSourceNone}},
		{"input not an object", coder, false, dispatchRoute{}, `"text"`, dispatchRoute{Source: routeSourceNone}},
		{"input invalid", coder, false, dispatchRoute{}, `{`, dispatchRoute{Source: routeSourceNone}},
		{"model under another case", coder, false, dispatchRoute{}, `{"Model":"gpt-6"}`, dispatchRoute{Source: routeSourceAmbiguous}},
		{"model twice in two cases", coder, false, tier, `{"model":"claude-sonnet-5-5","MODEL":"gpt-6"}`, dispatchRoute{Source: routeSourceAmbiguous}},
		{"provider under another case", coder, false, dispatchRoute{}, `{"model":"gpt-6","Provider":"x"}`, dispatchRoute{Source: routeSourceAmbiguous}},
		{"model not a string", coder, false, dispatchRoute{}, `{"model":7}`, dispatchRoute{Source: routeSourceAmbiguous}},
		{"model null", coder, false, dispatchRoute{}, `{"model":null}`, dispatchRoute{Source: routeSourceNone}},
		{"deterministic workload", det, false, dispatchRoute{}, `{"model":"gpt-6"}`, dispatchRoute{Source: routeSourceDeterministic}},
		{"command step", coder, true, dispatchRoute{}, `{}`, dispatchRoute{Source: routeSourceCommand}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dispatchedRoute(tc.wl, tc.command, tc.tier, tc.input); got != tc.want {
				t.Fatalf("dispatchedRoute = %+v, want %+v", got, tc.want)
			}
		})
	}
}

type familyFixture struct {
	r *Reconciler
	a *pinnedAdapter
}

// newFamilyFixture runs a workflow of coding-agent workloads: "author" on the
// claude harness, "critic" on codex and "gem" on agy, all on one adapter that
// completes every task with an author commit.
func newFamilyFixture(t *testing.T, steps []domain.WorkflowStep) familyFixture {
	t.Helper()
	ctx := context.Background()
	r, st, _, unused := regressionFixture(t)
	if err := r.CancelTask(ctx, unused.ID, ""); err != nil {
		t.Fatal(err)
	}
	for name, harness := range map[string]string{"author": "claude", "critic": "codex", "gem": "agy"} {
		r.bundle.Workloads[name] = domain.Workload{Metadata: domain.ObjectMeta{Name: name}, Spec: domain.WorkloadSpec{Class: domain.ClassCodingAgent, Coding: &domain.CodingConfig{Harness: harness}}}
		if err := st.UpsertWorkload(ctx, domain.WorkloadRecord{Name: name, Class: domain.ClassCodingAgent, AgentCardJSON: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	r.bundle.ManifestDigest = "digest-1"
	r.bundle.Workflows = map[string]domain.Workflow{"flow": {Spec: domain.WorkflowSpec{Steps: steps}}}
	a := &pinnedAdapter{}
	a.nextState = scruntime.StateCompleted
	a.artifacts = []scruntime.Artifact{{Name: "git-commit", ContentType: "text/plain", Text: strings.Repeat("a", 40)}}
	r.adapters[domain.ClassCodingAgent] = a
	return familyFixture{r: r, a: a}
}

// run reconciles a few passes. A refused verifier returns its refusal from
// the pass that launched it, so errors are not fatal here; the test reads
// the recorded state instead.
func (f familyFixture) run(t *testing.T, passes int) {
	t.Helper()
	for range passes {
		_ = f.r.ReconcileOnce(context.Background())
		_ = waitForLaunches(t, f.r)
	}
}

// stepTasks returns the run's tasks for a step name, oldest first.
func (f familyFixture) stepTasks(t *testing.T, step string) []domain.Task {
	t.Helper()
	tasks, err := f.r.store.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.Task
	for _, task := range tasks {
		var meta stepMeta
		if json.Unmarshal([]byte(task.InputJSON), &meta) == nil && meta.Step == step {
			out = append(out, task)
		}
	}
	return out
}

// isRoot reports whether a task starts its workflow run.
func (f familyFixture) isRoot(t *testing.T, id string) bool {
	t.Helper()
	reg, err := f.r.workflowStep(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return reg != nil && reg.RunID == id
}

func (f familyFixture) event(t *testing.T, taskID, reason string) (domain.TaskEvent, bool) {
	t.Helper()
	events, err := f.r.store.ListTaskEvents(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Reason == reason {
			return e, true
		}
	}
	return domain.TaskEvent{}, false
}

func TestVerifierFamilyAtDispatch(t *testing.T) {
	author := func(model string) domain.WorkflowStep {
		return domain.WorkflowStep{Name: "edit", Workload: "author", Role: "author", Model: model}
	}
	review := func(workload, model string) domain.WorkflowStep {
		return domain.WorkflowStep{Name: "review", Workload: workload, Role: "verifier", Model: model}
	}
	cases := []struct {
		name   string
		steps  []domain.WorkflowStep
		ledger []string // models the Model Gate recorded for the author task
		// wantRefusal is "" when the verifier launches.
		wantRefusal modelfamily.Reason
	}{
		{"different family launches", []domain.WorkflowStep{author("claude-sonnet-5-5"), review("critic", "gpt-6.2-astra")}, nil, ""},
		{"default tiers launch", []domain.WorkflowStep{{Name: "edit", Workload: "author", Role: "author", Tier: "coding"}, {Name: "review", Workload: "critic", Role: "verifier", Tier: "critic"}}, nil, ""},
		{"same family refused", []domain.WorkflowStep{author("claude-sonnet-5-5"), review("gem", "claude-opus-5-5")}, nil, modelfamily.ReasonSameFamily},
		{"author with no model refused", []domain.WorkflowStep{author(""), review("critic", "gpt-6.2-astra")}, nil, modelfamily.ReasonChainUnknown},
		{"verifier with no model refused", []domain.WorkflowStep{author("claude-sonnet-5-5"), review("critic", "")}, nil, modelfamily.ReasonUnknown},
		{"ledger model of the author refused", []domain.WorkflowStep{author("claude-sonnet-5-5"), review("critic", "gpt-6.2-astra")}, []string{"gpt-6.2-mini"}, modelfamily.ReasonSameFamily},
		{"unknown ledger model refused", []domain.WorkflowStep{author("claude-sonnet-5-5"), review("critic", "gpt-6.2-astra")}, []string{"llama-3"}, modelfamily.ReasonChainUnknown},
		{"other-family ledger model launches", []domain.WorkflowStep{author("claude-sonnet-5-5"), review("critic", "gpt-6.2-astra")}, []string{"gemini-3.8-pro"}, ""},
		{"fast step in the chain refused", []domain.WorkflowStep{{Name: "triage", Workload: "gem", Role: "fast", Model: "gemini-3.8-flash"}, author("claude-sonnet-5-5"), review("gem", "gemini-3.8-pro")}, nil, modelfamily.ReasonSameFamily},
		// The first verifier never joins the chain, so a second verifier of its
		// family still launches; one of the author's family is refused.
		{"second verifier of the first verifier's family launches", []domain.WorkflowStep{author("claude-sonnet-5-5"), review("critic", "gpt-6.2-astra"), {Name: "security", Workload: "critic", Role: "verifier", Model: "gpt-6.2-astra"}}, nil, ""},
		{"second verifier of the author's family refused", []domain.WorkflowStep{author("claude-sonnet-5-5"), review("critic", "gpt-6.2-astra"), {Name: "security", Workload: "gem", Role: "verifier", Model: "claude-opus-5-5"}}, nil, modelfamily.ReasonSameFamily},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFamilyFixture(t, tc.steps)
			root, err := f.r.StartWorkflow(ctx, "flow", "Change it.\nVERDICT: PASS")
			if err != nil {
				t.Fatal(err)
			}
			f.run(t, 1)
			for _, model := range tc.ledger {
				if err := f.r.store.RecordBudgetUsage(ctx, domain.BudgetEntry{TaskID: root.ID, WorkloadName: root.WorkloadName, Provider: "gate", Model: model}); err != nil {
					t.Fatal(err)
				}
			}
			f.a.artifacts = []scruntime.Artifact{{Name: "final-message", ContentType: "text/plain", Text: "Reviewed.\nVERDICT: PASS"}, {Name: "git-commit", ContentType: "text/plain", Text: strings.Repeat("a", 40)}}
			f.run(t, 6)

			// Every launch records its dispatch fact.
			dispatched, ok := f.event(t, root.ID, dispatchEventReason)
			if !ok {
				t.Fatal("first step has no dispatch fact")
			}
			var fact dispatchFact
			if err := json.Unmarshal([]byte(dispatched.FactJSON), &fact); err != nil || fact.BundleDigest != "digest-1" || fact.Route == "" {
				t.Fatalf("dispatch fact = %s (%v)", dispatched.FactJSON, err)
			}

			last := tc.steps[len(tc.steps)-1]
			verifiers := f.stepTasks(t, last.Name)
			if len(verifiers) != 1 {
				t.Fatalf("%d %q tasks, want 1", len(verifiers), last.Name)
			}
			v, err := f.r.store.GetTask(ctx, verifiers[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			refused, isRefused := f.event(t, v.ID, verifierRefusalReason)
			if tc.wantRefusal == "" {
				if isRefused || v.State != domain.TaskStateCompleted {
					t.Fatalf("verifier state %s, refusal %s", v.State, refused.FactJSON)
				}
				return
			}
			if !isRefused || v.State != domain.TaskStateFailed {
				t.Fatalf("verifier state %s, refused %t, want a %s refusal", v.State, isRefused, tc.wantRefusal)
			}
			var recorded struct{ Refusal, Detail string }
			if err := json.Unmarshal([]byte(refused.FactJSON), &recorded); err != nil || recorded.Refusal != string(tc.wantRefusal) {
				t.Fatalf("refusal fact = %s, want %s", refused.FactJSON, tc.wantRefusal)
			}
			if _, ok := f.event(t, v.ID, "verifier_checkout_pinned"); ok {
				t.Fatal("refused verifier was pinned for launch")
			}
			if f.a.lastSpec.TaskID == v.ID {
				t.Fatal("refused verifier was started")
			}
			run, err := f.r.workflowStep(ctx, root.ID)
			if err != nil || run == nil || run.Status != "halted" || run.Reason != verifierRefusalReason {
				t.Fatalf("run registration = %+v (%v)", run, err)
			}
			if _, ok := f.event(t, root.ID, "workflow_halted_verifier_family"); !ok {
				t.Fatal("run has no halt event")
			}
		})
	}
}

// Every author attempt and fix round joins the chain and stays in it; the
// verifier never joins.
func TestVerifierChainCarriesRetriesAndFixRounds(t *testing.T) {
	ctx := context.Background()
	f := newFamilyFixture(t, []domain.WorkflowStep{
		{Name: "edit", Workload: "author", Role: "author", Model: "claude-sonnet-5-5"},
		{Name: "review", Workload: "critic", Role: "verifier", Model: "gpt-6.2-astra", MaxRetries: 1},
	})
	if _, err := f.r.StartWorkflow(ctx, "flow", "Change it."); err != nil {
		t.Fatal(err)
	}
	f.run(t, 1)
	f.a.artifacts = []scruntime.Artifact{{Name: "final-message", ContentType: "text/plain", Text: "Fix it.\nVERDICT: CHANGES_REQUESTED"}, {Name: "git-commit", ContentType: "text/plain", Text: strings.Repeat("a", 40)}}
	f.run(t, 3)
	f.a.artifacts = []scruntime.Artifact{{Name: "final-message", ContentType: "text/plain", Text: "Reviewed.\nVERDICT: PASS"}, {Name: "git-commit", ContentType: "text/plain", Text: strings.Repeat("b", 40)}}
	f.run(t, 4)

	edits, reviews := f.stepTasks(t, "edit"), f.stepTasks(t, "review")
	if len(edits) != 2 || len(reviews) != 2 {
		t.Fatalf("%d edit and %d review tasks, want 2 and 2", len(edits), len(reviews))
	}
	for _, review := range reviews {
		task, err := f.r.store.GetTask(ctx, review.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, refused := f.event(t, task.ID, verifierRefusalReason); refused || task.State != domain.TaskStateCompleted {
			t.Fatalf("review %s state %s, refused %t", task.ID, task.State, refused)
		}
	}
	// The review after the fix round is the child of the fix task.
	second := reviews[0]
	if f.isRoot(t, second.ParentTaskID) {
		second = reviews[1]
	}
	reg, err := f.r.workflowStep(ctx, second.ID)
	if err != nil || reg == nil {
		t.Fatalf("second review registration: %+v (%v)", reg, err)
	}
	if len(reg.Chain) != 2 {
		t.Fatalf("second review chain = %+v, want the author and the fix round", reg.Chain)
	}
	ids := map[string]bool{edits[0].ID: true, edits[1].ID: true}
	for _, e := range reg.Chain {
		if !ids[e.TaskID] || e.Model != "claude-sonnet-5-5" || e.Step != "edit" {
			t.Fatalf("chain entry %+v is not an edit attempt", e)
		}
	}
}

func TestAuthorRetryJoinsTheChain(t *testing.T) {
	ctx := context.Background()
	f := newFamilyFixture(t, []domain.WorkflowStep{
		{Name: "edit", Workload: "author", Role: "author", Model: "claude-sonnet-5-5"},
		{Name: "review", Workload: "critic", Role: "verifier", Model: "gpt-6.2-astra"},
	})
	f.a.nextState = scruntime.StateFailed
	root, err := f.r.StartWorkflow(ctx, "flow", "Change it.")
	if err != nil {
		t.Fatal(err)
	}
	f.run(t, 3)
	retry, err := f.r.RetryTaskWithHandoff(ctx, root.ID, false, "operator retry")
	if err != nil {
		t.Fatal(err)
	}
	f.run(t, 2)
	reg, err := f.r.workflowStep(ctx, retry.ID)
	if err != nil || reg == nil {
		t.Fatalf("retry registration: %+v (%v)", reg, err)
	}
	if len(reg.Chain) != 2 || reg.Chain[0].TaskID != root.ID || reg.Chain[1].TaskID != retry.ID {
		t.Fatalf("retry chain = %+v, want both attempts", reg.Chain)
	}
}

// A stored tier route carries its operator family into the chain entry.
func TestTierFamilyReachesTheChain(t *testing.T) {
	ctx := context.Background()
	f := newFamilyFixture(t, []domain.WorkflowStep{{Name: "edit", Workload: "author", Role: "author", Tier: "coding"}})
	if err := contextpack.SaveModelTiers(ctx, f.r.store, []contextpack.TierRoute{{Tier: "coding", Provider: "cli:claude", Model: "claude-opus-5-5", Family: "anthropic"}}); err != nil {
		t.Fatal(err)
	}
	root, err := f.r.StartWorkflow(ctx, "flow", "Change it.")
	if err != nil {
		t.Fatal(err)
	}
	f.run(t, 1)
	reg, err := f.r.workflowStep(ctx, root.ID)
	if err != nil || reg == nil || len(reg.Chain) != 1 {
		t.Fatalf("registration %+v (%v)", reg, err)
	}
	if got := reg.Chain[0]; got.Model != "claude-opus-5-5" || got.Family != "anthropic" || got.Provider != "cli:claude" {
		t.Fatalf("chain entry = %+v", got)
	}
	e, _ := f.event(t, root.ID, dispatchEventReason)
	var fact dispatchFact
	if err := json.Unmarshal([]byte(e.FactJSON), &fact); err != nil || fact.Route != routeSourceTier || fact.Family != "anthropic" {
		t.Fatalf("dispatch fact = %s", e.FactJSON)
	}
}

// A verifier with no step registration, or one whose chain is empty (a
// verifier as the first step), is refused. On a real run the first-step
// verifier is already refused earlier, since it has no author commit to pin.
func TestCheckVerifierFamilyEmptyChain(t *testing.T) {
	f := newFamilyFixture(t, nil)
	route := dispatchRoute{Model: "gpt-6.2-astra", Source: routeSourceInput}
	for name, member := range map[string]*workflowStepRecord{"no registration": nil, "empty chain": {RunID: "r", Workflow: "flow"}} {
		err := f.r.checkVerifierFamily(context.Background(), stepMeta{Workflow: "flow", Step: "review"}, member, route)
		var refusal *verifierFamilyRefusal
		if !errors.As(err, &refusal) || refusal.refusal.Reason != modelfamily.ReasonChainEmpty {
			t.Fatalf("%s: err = %v, want %s", name, err, modelfamily.ReasonChainEmpty)
		}
	}
	// A deterministic verifier needs no chain.
	if err := f.r.checkVerifierFamily(context.Background(), stepMeta{Step: "check"}, nil, dispatchRoute{Source: routeSourceDeterministic}); err != nil {
		t.Fatalf("deterministic verifier refused: %v", err)
	}
}
