package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

type Options struct {
	Store           store.Store
	Bundle          *config.Bundle
	Policy          *policy.Engine
	Adapters        map[domain.WorkloadClass]scruntime.Adapter
	ModelGateURL    string
	DataGateURL     string
	StalenessWindow time.Duration
	MaxConcurrent   int
	OnTaskFailed    func(task domain.Task, reason string)
	Now             func() time.Time
}

type Reconciler struct {
	store           store.Store
	bundle          *config.Bundle
	policy          *policy.Engine
	adapters        map[domain.WorkloadClass]scruntime.Adapter
	modelGateURL    string
	dataGateURL     string
	stalenessWindow time.Duration
	maxConcurrent   int
	onTaskFailed    func(task domain.Task, reason string)
	now             func() time.Time

	mu      sync.Mutex
	handles map[string]scruntime.Handle
}

func (r *Reconciler) Reload(bundle *config.Bundle, pol *policy.Engine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bundle = bundle
	r.policy = pol
}

func (r *Reconciler) CancelTask(ctx context.Context, taskID, reason string) error {
	task, err := r.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.State == domain.TaskStateCompleted || task.State == domain.TaskStateFailed || task.State == domain.TaskStateCancelled {
		return store.ErrConflict
	}

	r.mu.Lock()
	handle, hasHandle := r.handles[taskID]
	delete(r.handles, taskID)
	var wl domain.Workload
	if r.bundle != nil {
		wl = r.bundle.Workloads[task.WorkloadName]
	}
	r.mu.Unlock()

	if hasHandle {
		if adapter := r.adapters[wl.Spec.Class]; adapter != nil {
			_ = adapter.Stop(ctx, handle)
		}
	}
	_ = r.store.RevokeTaskLeases(ctx, taskID)
	if reason == "" {
		reason = "operator_cancelled"
	}
	if err := r.store.UpdateTaskState(ctx, taskID, task.State, domain.TaskStateCancelled, reason, `{}`); err != nil {
		return err
	}
	_ = r.store.AppendAudit(ctx, domain.AuditRecord{
		Category:     "controlplane",
		TaskID:       taskID,
		WorkloadName: task.WorkloadName,
		Action:       "task.cancel",
		Outcome:      "cancelled",
		DetailJSON:   fmt.Sprintf(`{"reason":%q}`, reason),
		CreatedAt:    r.now(),
	})
	return nil
}

// SuspendTask checkpoints a running task, revokes its active gate leases, and transitions it to suspended.
func (r *Reconciler) SuspendTask(ctx context.Context, taskID, reason string) error {
	task, err := r.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.State != domain.TaskStateWorking {
		return fmt.Errorf("%w: task %s is in state %s (must be working)", store.ErrConflict, taskID, task.State)
	}

	r.mu.Lock()
	handle, hasHandle := r.handles[taskID]
	var wl domain.Workload
	if r.bundle != nil {
		wl = r.bundle.Workloads[task.WorkloadName]
	}
	r.mu.Unlock()

	cp := scruntime.CheckpointRef{ID: "cp-" + taskID, CreatedAt: r.now()}
	if hasHandle {
		if adapter := r.adapters[wl.Spec.Class]; adapter != nil {
			if c, err := adapter.Suspend(ctx, handle); err == nil {
				cp = c
			}
			_ = adapter.Stop(ctx, handle)
		}
	}
	if raw, err := json.Marshal(cp); err == nil {
		_ = r.store.SaveCheckpoint(ctx, taskID, task.WorkloadName, string(raw))
	}
	_ = r.store.RevokeTaskLeases(ctx, taskID)

	if reason == "" {
		reason = "operator_suspended"
	}
	if err := r.store.UpdateTaskState(ctx, taskID, domain.TaskStateWorking, domain.TaskStateSuspended, reason, fmt.Sprintf(`{"checkpoint_id":%q}`, cp.ID)); err != nil {
		return err
	}
	_ = r.store.AppendAudit(ctx, domain.AuditRecord{
		Category:     "controlplane",
		TaskID:       taskID,
		WorkloadName: task.WorkloadName,
		Action:       "task.suspend",
		Outcome:      "suspended",
		DetailJSON:   fmt.Sprintf(`{"checkpoint_id":%q,"reason":%q}`, cp.ID, reason),
		CreatedAt:    r.now(),
	})
	return nil
}

// ResumeTask restores the latest checkpoint, mints a fresh gate lease, and moves a suspended task back to working.
func (r *Reconciler) ResumeTask(ctx context.Context, taskID string) error {
	task, err := r.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.State != domain.TaskStateSuspended {
		return fmt.Errorf("%w: task %s is in state %s (must be suspended)", store.ErrConflict, taskID, task.State)
	}

	var cp scruntime.CheckpointRef
	if refJSON, err := r.store.LatestCheckpoint(ctx, taskID); err == nil {
		_ = json.Unmarshal([]byte(refJSON), &cp)
	}

	r.mu.Lock()
	handle, hasHandle := r.handles[taskID]
	var wl domain.Workload
	if r.bundle != nil {
		wl = r.bundle.Workloads[task.WorkloadName]
	}
	r.mu.Unlock()

	if _, err := model.MintTaskToken(ctx, r.store, taskID, task.WorkloadName, time.Hour); err != nil {
		return err
	}
	if hasHandle {
		if adapter := r.adapters[wl.Spec.Class]; adapter != nil {
			if err := adapter.Resume(ctx, handle, cp); err != nil {
				return fmt.Errorf("resume adapter: %w", err)
			}
		}
	}

	if err := r.store.UpdateTaskState(ctx, taskID, domain.TaskStateSuspended, domain.TaskStateWorking, "operator_resumed", fmt.Sprintf(`{"checkpoint_id":%q}`, cp.ID)); err != nil {
		return err
	}
	_ = r.store.RecordHeartbeat(ctx, taskID, r.now())
	_ = r.store.AppendAudit(ctx, domain.AuditRecord{
		Category:     "controlplane",
		TaskID:       taskID,
		WorkloadName: task.WorkloadName,
		Action:       "task.resume",
		Outcome:      "working",
		DetailJSON:   fmt.Sprintf(`{"checkpoint_id":%q}`, cp.ID),
		CreatedAt:    r.now(),
	})
	return nil
}

func NewReconciler(opts Options) *Reconciler {
	win := opts.StalenessWindow
	if win <= 0 {
		win = 2 * time.Minute
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	return &Reconciler{
		store:           opts.Store,
		bundle:          opts.Bundle,
		policy:          opts.Policy,
		adapters:        opts.Adapters,
		modelGateURL:    opts.ModelGateURL,
		dataGateURL:     opts.DataGateURL,
		stalenessWindow: win,
		maxConcurrent:   opts.MaxConcurrent,
		onTaskFailed:    opts.OnTaskFailed,
		now:             nowFn,
		handles:         make(map[string]scruntime.Handle),
	}
}

func (r *Reconciler) SubmitTask(ctx context.Context, workloadName, idempotencyKey, inputJSON string) (domain.Task, error) {
	if r.bundle == nil {
		return domain.Task{}, errors.New("no workload bundle loaded")
	}
	if _, ok := r.bundle.Workloads[workloadName]; !ok {
		return domain.Task{}, fmt.Errorf("unknown workload %q", workloadName)
	}
	id, err := newTaskID()
	if err != nil {
		return domain.Task{}, err
	}
	if idempotencyKey == "" {
		idempotencyKey = id
	}
	now := r.now()
	task := domain.Task{
		ID:             id,
		IdempotencyKey: idempotencyKey,
		WorkloadName:   workloadName,
		State:          domain.TaskStateSubmitted,
		InputJSON:      inputJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := r.store.CreateTask(ctx, task); err != nil {
		return domain.Task{}, err
	}
	if inputJSON != "" && inputJSON != "{}" {
		_ = r.store.AppendTaskMessage(ctx, domain.TaskMessage{
			TaskID:    task.ID,
			Role:      "user",
			Content:   inputJSON,
			CreatedAt: now,
		})
	}
	_ = r.store.AppendAudit(ctx, domain.AuditRecord{
		Category:     "controlplane",
		TaskID:       task.ID,
		WorkloadName: workloadName,
		Action:       "task.submit",
		Outcome:      "submitted",
		CreatedAt:    now,
	})
	return task, nil
}

// StartWorkflow dispatches the first step of a declared Workflow and records workflow metadata
// so ReconcileOnce can advance subsequent steps upon completion.
func (r *Reconciler) StartWorkflow(ctx context.Context, workflowName, userInput string) (domain.Task, error) {
	r.mu.Lock()
	b := r.bundle
	r.mu.Unlock()
	if b == nil {
		return domain.Task{}, errors.New("no manifest bundle loaded")
	}
	wf, ok := b.Workflows[workflowName]
	if !ok {
		return domain.Task{}, fmt.Errorf("unknown workflow %q", workflowName)
	}
	if len(wf.Spec.Steps) == 0 {
		return domain.Task{}, fmt.Errorf("workflow %q has no steps", workflowName)
	}

	step := wf.Spec.Steps[0]
	payload := map[string]any{
		"workflow":   workflowName,
		"step":       step.Name,
		"step_index": 0,
		"prompt":     step.Prompt,
		"input":      userInput,
	}
	if step.Provider != "" {
		payload["provider"] = step.Provider
	}
	if step.Model != "" {
		payload["model"] = step.Model
	}
	raw, _ := json.Marshal(payload)
	return r.SubmitTask(ctx, step.Workload, "", string(raw))
}

// ProposeChildTask enforces strict hub-and-spoke delegation: parent must hold a delegate:<target> grant
// and pass deterministic policy before the control plane dispatches the child task.
func (r *Reconciler) ProposeChildTask(ctx context.Context, parentTaskID, targetWorkload, idempotencyKey, inputJSON, subBudgetJSON string) (domain.Task, error) {
	parent, err := r.store.GetTask(ctx, parentTaskID)
	if err != nil {
		return domain.Task{}, fmt.Errorf("load parent task: %w", err)
	}
	parentWL, ok := r.bundle.Workloads[parent.WorkloadName]
	if !ok {
		return domain.Task{}, fmt.Errorf("parent workload %q not found", parent.WorkloadName)
	}
	if _, ok := r.bundle.Workloads[targetWorkload]; !ok {
		return domain.Task{}, fmt.Errorf("target workload %q not found", targetWorkload)
	}

	capability := "delegate:" + targetWorkload
	decision := r.policy.Evaluate(parentWL, capability)
	if decision.Effect != domain.EffectAllow {
		_ = r.store.AppendAudit(ctx, domain.AuditRecord{
			Category:     "controlplane",
			TaskID:       parentTaskID,
			WorkloadName: parent.WorkloadName,
			Action:       capability,
			Outcome:      "denied",
			DetailJSON:   fmt.Sprintf(`{"reason":%q}`, decision.Reason),
			CreatedAt:    r.now(),
		})
		return domain.Task{}, fmt.Errorf("delegation denied: %s", decision.Reason)
	}

	id, err := newTaskID()
	if err != nil {
		return domain.Task{}, err
	}
	if idempotencyKey == "" {
		idempotencyKey = parentTaskID + ":" + targetWorkload + ":" + id
	}
	now := r.now()
	child := domain.Task{
		ID:                id,
		ParentTaskID:      parentTaskID,
		IdempotencyKey:    idempotencyKey,
		WorkloadName:      targetWorkload,
		State:             domain.TaskStateSubmitted,
		InputJSON:         inputJSON,
		BudgetCeilingJSON: subBudgetJSON,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := r.store.CreateTask(ctx, child); err != nil {
		return domain.Task{}, err
	}
	_ = r.store.AppendAudit(ctx, domain.AuditRecord{
		Category:     "controlplane",
		TaskID:       child.ID,
		WorkloadName: parent.WorkloadName,
		Action:       capability,
		Outcome:      "allowed",
		DetailJSON:   fmt.Sprintf(`{"parent_task_id":%q,"child_task_id":%q}`, parentTaskID, child.ID),
		CreatedAt:    now,
	})
	return child, nil
}

// ReconcileOnce runs one pass of OBSERVE -> UPDATE -> DERIVE/ACT over all active tasks.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	tasks, err := r.store.ListTasks(ctx)
	if err != nil {
		return err
	}

	activeWorking := 0
	for _, t := range tasks {
		if t.State == domain.TaskStateWorking {
			activeWorking++
		}
	}

	for _, task := range tasks {
		wl, ok := r.bundle.Workloads[task.WorkloadName]
		if !ok {
			continue
		}
		adapter := r.adapters[wl.Spec.Class]
		if adapter == nil {
			continue
		}

		switch task.State {
		case domain.TaskStateSubmitted:
			if r.maxConcurrent > 0 && activeWorking >= r.maxConcurrent {
				continue
			}
			if err := r.launchTask(ctx, task, wl, adapter); err != nil {
				_ = r.store.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, domain.TaskStateFailed, "launch_failed", fmt.Sprintf(`{"error":%q}`, err.Error()))
				if r.onTaskFailed != nil {
					r.onTaskFailed(task, "launch_failed: "+err.Error())
				}
			} else {
				activeWorking++
			}

		case domain.TaskStateWorking:
			r.mu.Lock()
			handle, hasHandle := r.handles[task.ID]
			r.mu.Unlock()
			if !hasHandle {
				continue
			}

			// OBSERVE
			obs, err := adapter.Observe(ctx, handle)
			if err != nil {
				continue
			}
			// UPDATE
			_ = r.store.RecordHeartbeat(ctx, task.ID, obs.ObservedAt)

			// DERIVE / ACT
			switch obs.State {
			case scruntime.StateCompleted:
				r.persistArtifacts(ctx, task.ID, adapter, handle)
				_ = r.store.UpdateTaskState(ctx, task.ID, domain.TaskStateWorking, domain.TaskStateCompleted, "runtime_completed", `{}`)
				_ = r.store.RevokeTaskLeases(ctx, task.ID)
				r.mu.Lock()
				delete(r.handles, task.ID)
				r.mu.Unlock()
				r.advanceWorkflowStep(ctx, task)
			case scruntime.StateFailed:
				r.persistArtifacts(ctx, task.ID, adapter, handle)
				_ = r.store.UpdateTaskState(ctx, task.ID, domain.TaskStateWorking, domain.TaskStateFailed, "runtime_failed", fmt.Sprintf(`{"exit_code":%d}`, obs.ExitCode))
				_ = r.store.RevokeTaskLeases(ctx, task.ID)
				r.mu.Lock()
				delete(r.handles, task.ID)
				r.mu.Unlock()
				if r.onTaskFailed != nil {
					r.onTaskFailed(task, fmt.Sprintf("runtime_failed (exit_code=%d)", obs.ExitCode))
				}
			}
		}
	}
	return nil
}

func (r *Reconciler) launchTask(ctx context.Context, task domain.Task, wl domain.Workload, adapter scruntime.Adapter) error {
	token, err := model.MintTaskToken(ctx, r.store, task.ID, wl.Metadata.Name, time.Hour)
	if err != nil {
		return err
	}

	spec := scruntime.LaunchSpec{
		TaskID:      task.ID,
		Workload:    wl,
		InputJSON:   task.InputJSON,
		DataGateURL: r.dataGateURL,
		TaskToken:   token,
	}
	// Only non-deterministic workloads with explicit Model Gate grants receive Model Gate coordinates.
	if wl.Spec.Class != domain.ClassDeterministicController && wl.Spec.Grants.ModelGate.Allowed {
		spec.ModelGateURL = r.modelGateURL
		spec.ModelGateToken = token
	}

	handle, err := adapter.Start(ctx, spec)
	if err != nil {
		_ = r.store.RevokeTaskLeases(ctx, task.ID)
		return err
	}

	r.mu.Lock()
	r.handles[task.ID] = handle
	r.mu.Unlock()

	if err := r.store.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, domain.TaskStateWorking, "dispatched", fmt.Sprintf(`{"adapter":%q}`, handle.AdapterName)); err != nil {
		return err
	}
	_ = r.store.RecordHeartbeat(ctx, task.ID, r.now())
	return nil
}

func (r *Reconciler) advanceWorkflowStep(ctx context.Context, completedTask domain.Task) {
	var meta struct {
		Workflow  string `json:"workflow"`
		StepIndex int    `json:"step_index"`
		Input     string `json:"input"`
	}
	if err := json.Unmarshal([]byte(completedTask.InputJSON), &meta); err != nil || meta.Workflow == "" {
		return
	}
	r.mu.Lock()
	b := r.bundle
	r.mu.Unlock()
	if b == nil {
		return
	}
	wf, ok := b.Workflows[meta.Workflow]
	if !ok {
		return
	}
	nextIdx := meta.StepIndex + 1
	if nextIdx >= len(wf.Spec.Steps) {
		return
	}
	nextStep := wf.Spec.Steps[nextIdx]
	payload := map[string]any{
		"workflow":   meta.Workflow,
		"step":       nextStep.Name,
		"step_index": nextIdx,
		"prompt":     nextStep.Prompt,
		"input":      meta.Input,
	}
	if nextStep.Provider != "" {
		payload["provider"] = nextStep.Provider
	}
	if nextStep.Model != "" {
		payload["model"] = nextStep.Model
	}
	raw, _ := json.Marshal(payload)
	nextID, err := newTaskID()
	if err != nil {
		return
	}
	now := r.now()
	child := domain.Task{
		ID:             nextID,
		ParentTaskID:   completedTask.ID,
		IdempotencyKey: fmt.Sprintf("wf:%s:%s:step:%d", meta.Workflow, completedTask.ID, nextIdx),
		WorkloadName:   nextStep.Workload,
		State:          domain.TaskStateSubmitted,
		InputJSON:      string(raw),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := r.store.CreateTask(ctx, child); err == nil {
		_ = r.store.AppendTaskMessage(ctx, domain.TaskMessage{
			TaskID:    child.ID,
			Role:      "system",
			Content:   fmt.Sprintf("Workflow %s advanced to step %d (%s): %s", meta.Workflow, nextIdx+1, nextStep.Name, nextStep.Prompt),
			CreatedAt: now,
		})
		_ = r.store.AppendAudit(ctx, domain.AuditRecord{
			Category:     "workflow",
			TaskID:       child.ID,
			WorkloadName: nextStep.Workload,
			Action:       "workflow.advance",
			Outcome:      "submitted",
			DetailJSON:   fmt.Sprintf(`{"workflow":%q,"step":%q,"step_index":%d,"parent_task_id":%q}`, meta.Workflow, nextStep.Name, nextIdx, completedTask.ID),
			CreatedAt:    now,
		})
	}
}

func (r *Reconciler) persistArtifacts(ctx context.Context, taskID string, adapter scruntime.Adapter, handle scruntime.Handle) {
	arts, err := adapter.CollectArtifacts(ctx, handle)
	if err != nil || len(arts) == 0 {
		return
	}
	if raw, err := json.Marshal(arts); err == nil {
		_ = r.store.UpdateTaskArtifacts(ctx, taskID, string(raw))
	}
}

func newTaskID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "task_" + hex.EncodeToString(b[:]), nil
}
