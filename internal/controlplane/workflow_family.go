package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/modelfamily"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// Where a dispatched task's model came from (the dispatch fact's route).
const (
	routeSourceStep          = "step"          // the step's explicit provider and model
	routeSourceTier          = "tier"          // the stored tier route
	routeSourceTierDefault   = "tier_default"  // the shipped default tier route
	routeSourceInput         = "input"         // a model in the task input with no tier
	routeSourceNone          = "none"          // a model-running task that names no model
	routeSourceAmbiguous     = "ambiguous"     // the input names its model more than one way
	routeSourceDeterministic = "deterministic" // a deterministic-controller workload: no model
	routeSourceCommand       = "command"       // a workflow command step: no model
)

const (
	dispatchEventReason   = "model_dispatched"
	verifierRefusalReason = "verifier_family_refused"
)

// dispatchRoute is the provider and model the daemon asks a task to run, the
// operator family of its tier route if any, and where the route came from.
type dispatchRoute struct {
	Provider string
	Model    string
	Family   string
	Source   string
}

// runsModel reports whether the route belongs to a task that runs a model.
// A model-running task that names no model still runs one, of unknown family.
func (d dispatchRoute) runsModel() bool {
	return d.Source != routeSourceDeterministic && d.Source != routeSourceCommand
}

// chainEntry is one model in a workflow run's chain, as the step registration
// stores it. Family is the operator family of the tier route, not the
// classified family; the classifier runs at every verifier check.
type chainEntry struct {
	Step     string `json:"step"`
	TaskID   string `json:"task_id"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Family   string `json:"family,omitempty"`
}

// dispatchFact is the fact of the model_dispatched event every task launch
// writes (ADR-0010, amendment of 2026-11-27). It records the model the
// daemon asked for, not the one the provider ran.
type dispatchFact struct {
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	Family         string `json:"family"`
	FamilyOverride string `json:"family_override,omitempty"`
	FamilyRefusal  string `json:"family_refusal,omitempty"`
	Route          string `json:"route"`
	BundleDigest   string `json:"bundle_digest,omitempty"`
}

// verifierFamilyRefusal is a verifier dispatch the family rule refused. The
// task fails with verifierRefusalReason and its workflow run halts.
type verifierFamilyRefusal struct {
	refusal *modelfamily.Refusal
}

func (e *verifierFamilyRefusal) Error() string {
	return "verifier dispatch refused: " + e.refusal.Error()
}

func (e *verifierFamilyRefusal) facts() string {
	raw, _ := json.Marshal(map[string]string{"refusal": string(e.refusal.Reason), "detail": e.refusal.Detail})
	return string(raw)
}

// dispatchedRoute is the route a task launches with, read from the input the
// workload receives. tier is what prepareLaunchInput resolved from the step's
// tier; its family and source apply only when the input still names its
// model. An input that names its model under more than one key, in another
// case or as a non-string records an unknown model.
func dispatchedRoute(wl domain.Workload, command bool, tier dispatchRoute, inputJSON string) dispatchRoute {
	switch {
	case command:
		return dispatchRoute{Source: routeSourceCommand}
	case wl.Spec.Class == domain.ClassDeterministicController:
		return dispatchRoute{Source: routeSourceDeterministic}
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inputJSON), &payload); err != nil || payload == nil {
		return dispatchRoute{Source: routeSourceNone}
	}
	var model, provider string
	for key, raw := range payload {
		var target *string
		switch {
		case strings.EqualFold(key, "model"):
			target = &model
		case strings.EqualFold(key, "provider"):
			target = &provider
		default:
			continue
		}
		var value string
		if key != strings.ToLower(key) || json.Unmarshal(raw, &value) != nil {
			return dispatchRoute{Source: routeSourceAmbiguous}
		}
		*target = strings.TrimSpace(value)
	}
	switch {
	case model == "":
		return dispatchRoute{Provider: provider, Source: routeSourceNone}
	case tier.Model != "" && tier.Model == model && tier.Provider == provider:
		return tier
	default:
		return dispatchRoute{Provider: provider, Model: model, Source: routeSourceInput}
	}
}

// stepMeta is the part of a workflow step task's input the dispatch record
// and the family check read.
type stepMeta struct {
	Workflow  string `json:"workflow"`
	Step      string `json:"step"`
	StepIndex int    `json:"step_index"`
	Role      string `json:"role"`
}

// isVerifierStep reports whether a workflow step task is a verifier: its
// input or its declared step says so. Either one is enough.
func (r *Reconciler) isVerifierStep(meta stepMeta, workload string) bool {
	if meta.Role == "verifier" {
		return true
	}
	r.mu.Lock()
	bundle := r.bundle
	r.mu.Unlock()
	if bundle == nil {
		return false
	}
	steps := bundle.Workflows[meta.Workflow].Spec.Steps
	return meta.StepIndex >= 0 && meta.StepIndex < len(steps) && steps[meta.StepIndex].Workload == workload && steps[meta.StepIndex].Role == "verifier"
}

// recordDispatch writes the task's dispatch fact and, for a non-verifier
// workflow step that runs a model, adds the step's entry to the run's chain
// in its step registration, in one transaction. For a verifier step it then
// checks the verifier's family against the run's chain and returns a
// *verifierFamilyRefusal when the rule refuses it (invariant 4). A replayed
// dispatch (the event is already on the task) writes nothing again.
func (r *Reconciler) recordDispatch(ctx context.Context, task domain.Task, route dispatchRoute) error {
	r.mu.Lock()
	var digest string
	if r.bundle != nil {
		digest = r.bundle.ManifestDigest
	}
	r.mu.Unlock()
	fact := dispatchFact{Provider: route.Provider, Model: route.Model, FamilyOverride: route.Family, Route: route.Source, BundleDigest: digest}
	if route.runsModel() {
		family, err := modelfamily.Classify(route.Model, route.Family)
		fact.Family = string(family)
		fact.FamilyRefusal = string(modelfamily.ReasonOf(err))
	}
	if fact.Family == "" {
		fact.Family = "unknown"
	}
	if !route.runsModel() {
		fact.Family = "none"
	}
	rawFact, err := json.Marshal(fact)
	if err != nil {
		return err
	}

	var meta stepMeta
	var member *workflowStepRecord
	verifier := false
	if json.Unmarshal([]byte(task.InputJSON), &meta) == nil && meta.Workflow != "" {
		member, err = r.workflowStep(ctx, task.ID)
		if err != nil {
			return err
		}
		if member != nil && (member.Workflow != meta.Workflow || member.Index != meta.StepIndex) {
			return errors.New("workflow step does not match its registration")
		}
		verifier = r.isVerifierStep(meta, task.WorkloadName)
	}

	effects := store.TransitionEffects{Recorded: func(reason, _ string) bool { return reason == dispatchEventReason }}
	if member != nil && !verifier && route.runsModel() {
		next := *member
		next.Chain = append(append([]chainEntry(nil), member.Chain...), chainEntry{
			Step: meta.Step, TaskID: task.ID, Provider: route.Provider, Model: route.Model, Family: route.Family,
		})
		entry, err := r.workflowStepEntry(task.ID, next)
		if err != nil {
			return err
		}
		effects.Memory = &entry
	}
	err = store.UpdateTaskStateWithEffects(ctx, r.store, task.ID, task.State, task.State, dispatchEventReason, string(rawFact), effects)
	if err != nil && !errors.Is(err, store.ErrAlreadyRecorded) {
		return err
	}
	if !verifier {
		return nil
	}
	return r.checkVerifierFamily(ctx, meta, member, route)
}

// checkVerifierFamily checks a verifier step against every model of its run's
// chain: the entries its registration carries and every model budget_ledger
// recorded for those tasks. A verifier with no registration has an empty
// chain and is refused.
func (r *Reconciler) checkVerifierFamily(ctx context.Context, meta stepMeta, member *workflowStepRecord, route dispatchRoute) error {
	v := modelfamily.Verifier{Step: meta.Step, Deterministic: !route.runsModel(), Model: route.Model, Family: route.Family}
	var chain []modelfamily.ChainEntry
	if member != nil {
		seen := make(map[string]bool)
		for _, e := range member.Chain {
			chain = append(chain, modelfamily.ChainEntry{Step: e.Step, TaskID: e.TaskID, Model: e.Model, Family: e.Family})
		}
		for _, e := range member.Chain {
			if e.TaskID == "" || seen[e.TaskID] {
				continue
			}
			seen[e.TaskID] = true
			models, err := r.store.TaskLedgerModels(ctx, e.TaskID)
			if err != nil {
				return fmt.Errorf("read the models of chain task %s: %w", e.TaskID, err)
			}
			for _, model := range models {
				chain = append(chain, modelfamily.ChainEntry{Step: e.Step, TaskID: e.TaskID, Model: model})
			}
		}
	}
	err := modelfamily.CheckVerifier(chain, v)
	if err == nil {
		return nil
	}
	var refusal *modelfamily.Refusal
	if !errors.As(err, &refusal) {
		return err
	}
	return &verifierFamilyRefusal{refusal: refusal}
}

// haltVerifierFamily halts the workflow run of a verifier step that the
// family rule refused. It is a no-op for any other task and for a run already
// halted, so the failure paths call it for every failed task, and a halt that
// failed once is written by a later pass.
func (r *Reconciler) haltVerifierFamily(ctx context.Context, task domain.Task) error {
	var meta stepMeta
	if json.Unmarshal([]byte(task.InputJSON), &meta) != nil || meta.Workflow == "" {
		return nil
	}
	events, err := r.store.ListTaskEvents(ctx, task.ID)
	if err != nil {
		return err
	}
	var refused *domain.TaskEvent
	for i := range events {
		if events[i].Reason == verifierRefusalReason && events[i].ToState == domain.TaskStateFailed {
			refused = &events[i]
		}
	}
	if refused == nil {
		return nil
	}
	member, err := r.workflowStep(ctx, task.ID)
	if err != nil || member == nil || member.Workflow != meta.Workflow || member.Index != meta.StepIndex {
		return err
	}
	rootStep, err := r.workflowStep(ctx, member.RunID)
	if err != nil {
		return err
	}
	if rootStep == nil || rootStep.Index != 0 || rootStep.RunID != member.RunID || rootStep.Workflow != member.Workflow {
		return errors.New("missing workflow run registration")
	}
	if rootStep.Status == "halted" {
		return nil
	}
	var recorded struct {
		Refusal string `json:"refusal"`
	}
	_ = json.Unmarshal([]byte(refused.FactJSON), &recorded)
	fact, err := json.Marshal(map[string]string{"verifier_task_id": task.ID, "step": meta.Step, "refusal": recorded.Refusal})
	if err != nil {
		return err
	}
	detail, err := json.Marshal(map[string]any{"workflow": meta.Workflow, "step": meta.Step, "step_index": meta.StepIndex, "refusal": recorded.Refusal})
	if err != nil {
		return err
	}
	return r.haltRun(ctx, member.RunID, rootStep, "workflow_halted_verifier_family", string(fact), verifierRefusalReason, "verifier_task_id", task.ID, domain.AuditRecord{
		Category: "workflow", TaskID: task.ID, WorkloadName: task.WorkloadName,
		Action: "workflow.verify", Outcome: verifierRefusalReason, DetailJSON: string(detail), CreatedAt: r.now(),
	})
}
