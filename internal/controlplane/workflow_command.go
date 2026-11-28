package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

// Workflow command steps (issue 134). A command step runs one entry of its
// coding workload's coding.commands in a harness container, on a read-only
// checkout pinned to the commit of the step before it. The argv comes only
// from the loaded manifest, found through the step registration the control
// plane wrote; a task's input can name a command but never supplies argv, and
// a task that names one without a matching registration is refused.

type commandStepMeta struct {
	Workflow        string `json:"workflow"`
	StepIndex       int    `json:"step_index"`
	Command         string `json:"command"`
	AuthorTaskID    string `json:"author_task_id"`
	AuthorStepIndex int    `json:"author_step_index"`
}

// namesCommand reports whether the task input has a command field in any
// case, which a typed reader could match.
func namesCommand(inputJSON string) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(inputJSON), &payload) != nil {
		return false
	}
	for key := range payload {
		if strings.EqualFold(key, "command") {
			return true
		}
	}
	return false
}

// workflowCommand resolves the command a task runs, or nil for a task that
// is not a command step. Every mismatch is a refusal before any container starts.
func (r *Reconciler) workflowCommand(ctx context.Context, task domain.Task, wl domain.Workload) (*scruntime.CommandSpec, error) {
	var meta commandStepMeta
	_ = json.Unmarshal([]byte(task.InputJSON), &meta)
	r.mu.Lock()
	bundle := r.bundle
	r.mu.Unlock()
	var step *domain.WorkflowStep
	if bundle != nil && meta.Workflow != "" {
		steps := bundle.Workflows[meta.Workflow].Spec.Steps
		if meta.StepIndex >= 0 && meta.StepIndex < len(steps) {
			step = &steps[meta.StepIndex]
		}
	}
	stepCommand := step != nil && step.Command != "" && step.Workload == task.WorkloadName
	if !stepCommand && !namesCommand(task.InputJSON) {
		return nil, nil
	}
	refuse := &scruntime.CommandRefusal{Command: meta.Command, Reason: scruntime.CommandNotAllowed}
	if !stepCommand || meta.Command != step.Command {
		return nil, refuse
	}
	member, err := r.workflowStep(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if member == nil || member.Workflow != meta.Workflow || member.Index != meta.StepIndex || member.AuthorCommit == "" {
		return nil, refuse
	}
	if wl.Spec.Class != domain.ClassCodingAgent || wl.Spec.Coding == nil {
		return nil, refuse
	}
	cmd, ok := wl.Spec.Coding.Command(step.Command)
	if !ok || domain.ValidCommandArgv(cmd.Argv) != nil {
		return nil, refuse
	}
	// No recorded author base, or one that cannot name a commit of the same
	// object format as the pinned commit, fails the step: no comparison is
	// skipped (issue 214).
	if len(member.AuthorBase) != len(member.AuthorCommit) || !validCommitID(member.AuthorBase) {
		return nil, &scruntime.CommandRefusal{Command: meta.Command, Reason: scruntime.CommandAuthorBaseUnavailable}
	}
	return &scruntime.CommandSpec{
		Name:           cmd.Name,
		Argv:           slices.Clone(cmd.Argv),
		Timeout:        cmd.Timeout(),
		MaxOutputBytes: cmd.OutputCap(),
		AuthorBase:     member.AuthorBase,
	}, nil
}

// commandStepOf returns the command step the loaded bundle declares at this
// workflow index for this workload, with the workload, from one snapshot of the bundle.
func (r *Reconciler) commandStepOf(workflow string, index int, workload string) (domain.WorkflowStep, domain.Workload, bool) {
	r.mu.Lock()
	bundle := r.bundle
	r.mu.Unlock()
	if bundle == nil {
		return domain.WorkflowStep{}, domain.Workload{}, false
	}
	steps := bundle.Workflows[workflow].Spec.Steps
	if index < 0 || index >= len(steps) || steps[index].Command == "" || steps[index].Workload != workload {
		return domain.WorkflowStep{}, domain.Workload{}, false
	}
	return steps[index], bundle.Workloads[workload], true
}

// isCommandStep reports whether the bundle declares this workflow step as a command step.
func (r *Reconciler) isCommandStep(workflow string, index int, workload string) bool {
	_, _, ok := r.commandStepOf(workflow, index, workload)
	return ok
}

// commandRefusalFacts is the fixed-text fact of a refused command step.
func commandRefusalFacts(refusal *scruntime.CommandRefusal) string {
	detail := map[string]string{
		scruntime.CommandNotAllowed:            "the command is not in the allowed set of the step's workload, or the task is not a registered command step",
		scruntime.CommandNoContainerRuntime:    "commands run only in harness containers and no container runtime is configured; there is no host fallback",
		scruntime.CommandContainerRefused:      "the command's harness container could not be built or started; there is no host fallback",
		scruntime.CommandAuthorBaseUnavailable: "the run has no recorded author base, or it is not an ancestor of the commit the step checks",
	}[refusal.Reason]
	raw, _ := json.Marshal(struct {
		Reason  string `json:"reason"`
		Refusal string `json:"refusal"`
		Command string `json:"command,omitempty"`
		Detail  string `json:"detail,omitempty"`
	}{"command_refused", refusal.Reason, refusal.Command, detail})
	return string(raw)
}

// CommandStepResult reads the command result a command step task recorded.
// It is the durable fact a later step or another workflow rule reads.
func CommandStepResult(task domain.Task) (scruntime.CommandResult, bool) {
	var artifacts []scruntime.Artifact
	if json.Unmarshal([]byte(task.ArtifactsJSON), &artifacts) != nil {
		return scruntime.CommandResult{}, false
	}
	var found *scruntime.CommandResult
	for _, a := range artifacts {
		if a.Name != scruntime.CommandResultArtifact {
			continue
		}
		var res scruntime.CommandResult
		if found != nil || a.ContentType != "application/json" || json.Unmarshal([]byte(a.Text), &res) != nil || res.Command == "" {
			return scruntime.CommandResult{}, false
		}
		found = &res
	}
	if found == nil {
		return scruntime.CommandResult{}, false
	}
	return *found, true
}

// commandPassed reports whether a completed command step recorded a passed result.
func commandPassed(task domain.Task) bool {
	res, ok := CommandStepResult(task)
	return ok && res.Outcome == scruntime.CommandPassed && res.ExitCode == 0
}

// commandStepAuthor returns the task a command step checked, its step index
// and its retry budget, so the step after a command step sees the author as
// if it had followed it directly.
func (r *Reconciler) commandStepAuthor(ctx context.Context, member *workflowStepRecord, authorTaskID string, authorIndex int) (domain.Task, int, int, int, error) {
	author, err := r.store.GetTask(ctx, authorTaskID)
	if err != nil {
		return domain.Task{}, 0, 0, 0, err
	}
	reg, err := r.workflowStep(ctx, author.ID)
	if err != nil {
		return domain.Task{}, 0, 0, 0, err
	}
	if reg == nil || reg.RunID != member.RunID || reg.Workflow != member.Workflow || reg.Index != authorIndex || authorIndex >= member.Index {
		return domain.Task{}, 0, 0, 0, errors.New("command step author does not match its registration")
	}
	var budget struct {
		MaxRetries int `json:"max_retries"`
		RetryCount int `json:"retry_count"`
	}
	_ = json.Unmarshal([]byte(author.InputJSON), &budget)
	return author, authorIndex, budget.MaxRetries, budget.RetryCount, nil
}

// commandStepPayload is the input of a command step task: names only, no argv.
func commandStepPayload(workflow string, step domain.WorkflowStep, index int, input string, author domain.Task, authorIndex int) map[string]any {
	return map[string]any{
		"workflow":          workflow,
		"step":              step.Name,
		"step_index":        index,
		"command":           step.Command,
		"input":             input,
		"author_task_id":    author.ID,
		"author_step_index": authorIndex,
	}
}

// haltCommandStep records on the workflow run that a command step failed or
// was refused, with the operator's failure reason, and stops the run. It is a
// no-op for any other task and for a run already halted.
func (r *Reconciler) haltCommandStep(ctx context.Context, task domain.Task, refusal *scruntime.CommandRefusal) error {
	var meta commandStepMeta
	if json.Unmarshal([]byte(task.InputJSON), &meta) != nil || meta.Workflow == "" {
		return nil
	}
	step, wl, ok := r.commandStepOf(meta.Workflow, meta.StepIndex, task.WorkloadName)
	if !ok {
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
	fact := struct {
		CommandTaskID string `json:"command_task_id"`
		Step          string `json:"step"`
		Command       string `json:"command"`
		Outcome       string `json:"outcome,omitempty"`
		ExitCode      *int   `json:"exit_code,omitempty"`
		Refusal       string `json:"refusal,omitempty"`
		FailureReason string `json:"failure_reason,omitempty"`
	}{CommandTaskID: task.ID, Step: step.Name, Command: meta.Command}
	if cmd, ok := wl.Spec.Coding.Command(step.Command); ok {
		fact.FailureReason = cmd.FailureReason
	}
	event, status := "workflow_halted_command_failed", "command_failed"
	if refusal != nil {
		event, status = "workflow_halted_command_refused", "command_refused"
		fact.Refusal = refusal.Reason
	} else {
		fresh, err := r.store.GetTask(ctx, task.ID)
		if err != nil {
			return err
		}
		fact.Outcome = scruntime.CommandFailed
		if res, ok := CommandStepResult(fresh); ok {
			fact.Outcome = res.Outcome
			fact.ExitCode = &res.ExitCode
		}
	}
	raw, err := json.Marshal(fact)
	if err != nil {
		return err
	}
	return r.haltRun(ctx, member.RunID, rootStep, event, string(raw), status, "command_task_id", task.ID)
}
