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
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
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

	reconcileMu    sync.Mutex
	mu             sync.Mutex
	handles        map[string]scruntime.Handle
	handleAdapters map[string]scruntime.Adapter
	launching      map[string]bool
}

func (r *Reconciler) Reload(bundle *config.Bundle, pol *policy.Engine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bundle = bundle
	r.policy = pol
}

func (r *Reconciler) CancelTask(ctx context.Context, taskID, reason string) error {
	// Serialize cancellation with publication of a newly started handle.
	r.mu.Lock()
	task, err := r.store.GetTask(ctx, taskID)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	if terminalTaskState(task.State) {
		r.mu.Unlock()
		return store.ErrConflict
	}
	if reason == "" {
		reason = "operator_cancelled"
	}
	err = r.store.UpdateTaskState(ctx, taskID, task.State, domain.TaskStateCancelled, reason, `{}`)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	cleanupCtx := context.WithoutCancel(ctx)
	cleanupErr := errors.Join(r.store.RevokeTaskLeases(cleanupCtx, taskID), r.stopTask(cleanupCtx, taskID))
	_ = r.store.AppendAudit(ctx, domain.AuditRecord{
		Category: "controlplane", TaskID: taskID, WorkloadName: task.WorkloadName,
		Action: "task.cancel", Outcome: "cancelled", DetailJSON: fmt.Sprintf(`{"reason":%q}`, reason), CreatedAt: r.now(),
	})
	return cleanupErr
}

func terminalTaskState(state domain.TaskState) bool {
	return state == domain.TaskStateCompleted || state == domain.TaskStateFailed || state == domain.TaskStateCancelled
}

// stopTask retains the handle if stopping fails so a later pass can retry cleanup.
func (r *Reconciler) stopTask(ctx context.Context, taskID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	handle, ok := r.handles[taskID]
	if !ok {
		return nil
	}
	adapter := r.handleAdapters[taskID]
	if adapter == nil {
		return errors.New("runtime adapter unavailable for cleanup")
	}
	if err := adapter.Stop(ctx, handle); err != nil {
		return fmt.Errorf("stop task: %w", err)
	}
	delete(r.handles, taskID)
	delete(r.handleAdapters, taskID)
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
		handleAdapters:  make(map[string]scruntime.Adapter),
		launching:       make(map[string]bool),
	}
}

func (r *Reconciler) SubmitTask(ctx context.Context, workloadName, idempotencyKey, inputJSON string) (domain.Task, error) {
	r.mu.Lock()
	bundle := r.bundle
	r.mu.Unlock()
	if bundle == nil {
		return domain.Task{}, errors.New("no workload bundle loaded")
	}
	if _, ok := bundle.Workloads[workloadName]; !ok {
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
		if errors.Is(err, store.ErrConflict) {
			// The unique key selects the winner even when submissions race.
			tasks, listErr := r.store.ListTasks(ctx)
			if listErr != nil {
				return domain.Task{}, listErr
			}
			for _, existing := range tasks {
				if existing.IdempotencyKey == idempotencyKey && existing.WorkloadName == workloadName && existing.ParentTaskID == "" {
					return existing, nil
				}
			}
		}
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
	prov, mod := step.Provider, step.Model
	if step.Tier != "" && prov == "" && mod == "" {
		prov, mod = contextpack.ResolveTier(ctx, r.store, step.Tier)
	}
	payload := map[string]any{
		"workflow":   workflowName,
		"step":       step.Name,
		"step_index": 0,
		"prompt":     step.Prompt,
		"input":      userInput,
	}
	if step.Role != "" {
		payload["role"] = step.Role
	}
	if step.Tier != "" {
		payload["tier"] = step.Tier
	}
	if step.MaxRetries > 0 {
		payload["max_retries"] = step.MaxRetries
		payload["auto_remediate"] = true
	}
	if prov != "" {
		payload["provider"] = prov
	}
	if mod != "" {
		payload["model"] = mod
	}
	raw, _ := json.Marshal(payload)
	return r.SubmitTask(ctx, step.Workload, "", string(raw))
}

// RetryTaskWithHandoff retries a task, optionally attaching a standardized failure-handoff.md
// containing redacted artifacts, lifecycle events, and failure context.
func (r *Reconciler) RetryTaskWithHandoff(ctx context.Context, origTaskID string, withHandoff bool, reason string) (domain.Task, error) {
	orig, err := r.store.GetTask(ctx, origTaskID)
	if err != nil {
		return domain.Task{}, err
	}
	if !terminalTaskState(orig.State) {
		return domain.Task{}, fmt.Errorf("%w: retry requires a terminal task", store.ErrConflict)
	}
	r.mu.Lock()
	_, hasHandle := r.handles[orig.ID]
	launching := r.launching[orig.ID]
	r.mu.Unlock()
	if hasHandle || launching {
		return domain.Task{}, fmt.Errorf("%w: runtime cleanup is pending", store.ErrConflict)
	}
	payload := make(map[string]any)
	if err := json.Unmarshal([]byte(orig.InputJSON), &payload); err != nil || payload == nil {
		payload = map[string]any{"prompt": orig.InputJSON}
	}
	// Each task can have one successor. Retrying that successor advances the attempt.
	attempt := 1
	var meta struct {
		RetryCount int `json:"retry_count"`
	}
	if err := json.Unmarshal([]byte(orig.InputJSON), &meta); err == nil && meta.RetryCount > 0 {
		if meta.RetryCount == int(^uint(0)>>1) {
			return domain.Task{}, errors.New("retry count exhausted")
		}
		attempt += meta.RetryCount
	}
	payload["retry_count"] = attempt
	payload["retried_from"] = orig.ID
	if withHandoff {
		events, err := r.store.ListTaskEvents(ctx, orig.ID)
		if err != nil {
			return domain.Task{}, err
		}
		msgs, err := r.store.ListTaskMessages(ctx, orig.ID)
		if err != nil {
			return domain.Task{}, err
		}
		payload["failure_handoff"] = contextpack.BuildFailureHandoff(orig, events, msgs, reason)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Task{}, err
	}
	idem := fmt.Sprintf("retry-%s-%d", orig.ID, attempt)
	retried, err := r.SubmitTask(ctx, orig.WorkloadName, idem, string(raw))
	if err != nil {
		return domain.Task{}, err
	}
	return retried, nil
}

// ProposeChildTask enforces strict hub-and-spoke delegation: parent must hold a delegate:<target> grant
// and pass deterministic policy before the control plane dispatches the child task.
func (r *Reconciler) ProposeChildTask(ctx context.Context, parentTaskID, targetWorkload, idempotencyKey, inputJSON, subBudgetJSON string) (domain.Task, error) {
	parent, err := r.store.GetTask(ctx, parentTaskID)
	if err != nil {
		return domain.Task{}, fmt.Errorf("load parent task: %w", err)
	}
	r.mu.Lock()
	bundle, pol := r.bundle, r.policy
	r.mu.Unlock()
	if bundle == nil || pol == nil {
		return domain.Task{}, errors.New("delegation requires a workload bundle and policy")
	}
	parentWL, ok := bundle.Workloads[parent.WorkloadName]
	if !ok {
		return domain.Task{}, fmt.Errorf("parent workload %q not found", parent.WorkloadName)
	}
	if _, ok := bundle.Workloads[targetWorkload]; !ok {
		return domain.Task{}, fmt.Errorf("target workload %q not found", targetWorkload)
	}

	capability := "delegate:" + targetWorkload
	decision := pol.Evaluate(parentWL, capability)
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
	// A second pass must not mistake a launch in progress for a lost runtime.
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	tasks, err := r.store.ListTasks(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	bundle := r.bundle
	r.mu.Unlock()
	activeWorking := 0
	for _, task := range tasks {
		if task.State == domain.TaskStateWorking || task.State == domain.TaskStateInputRequired {
			activeWorking++
		}
	}
	var passErr error
	for _, task := range tasks {
		if terminalTaskState(task.State) {
			passErr = errors.Join(passErr, r.reconcileTerminal(context.WithoutCancel(ctx), task))
			continue
		}
		if task.State != domain.TaskStateSubmitted && task.State != domain.TaskStateWorking && task.State != domain.TaskStateInputRequired {
			continue
		}
		var wl domain.Workload
		var ok bool
		if bundle != nil {
			wl, ok = bundle.Workloads[task.WorkloadName]
		}
		if !ok {
			passErr = errors.Join(passErr, r.failTask(ctx, task, "workload_missing", `{}`))
			continue
		}
		adapter := r.adapters[wl.Spec.Class]
		if adapter == nil {
			passErr = errors.Join(passErr, r.failTask(ctx, task, "adapter_missing", `{}`))
			continue
		}
		if task.State == domain.TaskStateSubmitted {
			if r.maxConcurrent > 0 && activeWorking >= r.maxConcurrent {
				continue
			}
			if err := r.launchTask(ctx, task, wl, adapter); err != nil {
				if !errors.Is(err, store.ErrConflict) {
					passErr = errors.Join(passErr, err)
				}
			} else {
				activeWorking++
			}
			continue
		}
		err := r.observeTask(ctx, task)
		if !errors.Is(err, store.ErrConflict) {
			passErr = errors.Join(passErr, err)
		}
	}
	return passErr
}

func (r *Reconciler) reconcileTerminal(ctx context.Context, task domain.Task) error {
	if err := errors.Join(r.store.RevokeTaskLeases(ctx, task.ID), r.stopTask(ctx, task.ID)); err != nil {
		return err
	}
	if task.State == domain.TaskStateCancelled {
		return nil
	}
	events, err := r.store.ListTaskEvents(ctx, task.ID)
	if err != nil {
		return err
	}
	if len(events) == 0 || events[len(events)-1].Reason != "runtime_exit_observed" {
		return nil
	}
	reason := "runtime_completed"
	if task.State == domain.TaskStateCompleted {
		err = r.advanceWorkflowStep(ctx, task)
	} else {
		reason = "runtime_failed"
		err = r.maybeAutoRemediate(ctx, task, reason)
	}
	if err != nil {
		return err
	}
	if err := r.store.UpdateTaskState(ctx, task.ID, task.State, task.State, reason, events[len(events)-1].FactJSON); err != nil {
		return err
	}
	if task.State == domain.TaskStateFailed && r.onTaskFailed != nil {
		r.onTaskFailed(task, reason)
	}
	return nil
}

func (r *Reconciler) failTask(ctx context.Context, task domain.Task, reason, facts string) error {
	cleanupCtx := context.WithoutCancel(ctx)
	if err := r.store.UpdateTaskState(cleanupCtx, task.ID, task.State, domain.TaskStateFailed, reason, facts); err != nil {
		return err
	}
	cleanupErr := errors.Join(r.store.RevokeTaskLeases(cleanupCtx, task.ID), r.stopTask(cleanupCtx, task.ID))
	if r.onTaskFailed != nil {
		r.onTaskFailed(task, reason)
	}
	return cleanupErr
}

func observationFailures(events []domain.TaskEvent) int {
	count := 0
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Reason != "observe_error" && events[i].Reason != "runtime_unknown" {
			break
		}
		count++
	}
	return count
}

func (r *Reconciler) observeTask(ctx context.Context, task domain.Task) error {
	r.mu.Lock()
	handle, hasHandle := r.handles[task.ID]
	adapter := r.handleAdapters[task.ID]
	r.mu.Unlock()
	if !hasHandle || adapter == nil {
		return r.failTask(ctx, task, "runtime_handle_missing", `{}`)
	}
	obs, observeErr := adapter.Observe(ctx, handle)
	events, err := r.store.ListTaskEvents(ctx, task.ID)
	if err != nil {
		return err
	}
	failures := observationFailures(events)
	reason := ""
	if observeErr != nil {
		reason = "observe_error"
	} else {
		switch obs.State {
		case scruntime.StateRunning, scruntime.StateSuspended, scruntime.StateCompleted, scruntime.StateFailed:
		default:
			reason = "runtime_unknown"
		}
	}
	if reason != "" {
		facts := fmt.Sprintf(`{"consecutive_failures":%d}`, failures+1)
		if failures+1 >= 3 {
			return r.failTask(ctx, task, reason, facts)
		}
		// Failed observations never refresh the last known heartbeat.
		return r.store.UpdateTaskState(ctx, task.ID, task.State, task.State, reason, facts)
	}
	if failures > 0 {
		if err := r.store.UpdateTaskState(ctx, task.ID, task.State, task.State, "observation_recovered", `{}`); err != nil {
			return err
		}
	}
	if err := r.store.RecordHeartbeat(ctx, task.ID, obs.ObservedAt); err != nil {
		return err
	}
	switch obs.State {
	case scruntime.StateCompleted, scruntime.StateFailed:
		r.persistArtifacts(ctx, task.ID, adapter, handle)
		target := domain.TaskStateCompleted
		if obs.State == scruntime.StateFailed {
			target = domain.TaskStateFailed
		}
		if err := r.store.UpdateTaskState(ctx, task.ID, task.State, target, "runtime_exit_observed", fmt.Sprintf(`{"exit_code":%d}`, obs.ExitCode)); err != nil {
			return err
		}
		r.mu.Lock()
		delete(r.handles, task.ID)
		delete(r.handleAdapters, task.ID)
		r.mu.Unlock()
		task.State = target
		return r.reconcileTerminal(context.WithoutCancel(ctx), task)
	case scruntime.StateRunning:
		if task.State == domain.TaskStateInputRequired {
			approvals, err := r.store.ListApprovals(ctx, "")
			if err != nil {
				return err
			}
			decided, pending := false, false
			for _, approval := range approvals {
				if approval.TaskID != task.ID {
					continue
				}
				switch approval.Status {
				case domain.ApprovalPending:
					if r.now().Before(approval.ExpiresAt) {
						pending = true
						continue
					}
					if err := r.store.DecideApproval(ctx, approval.ID, approval.PayloadSHA256, domain.ApprovalExpired, "system", `{"reason":"expired"}`); err != nil {
						return err
					}
					decided = true
				case domain.ApprovalApproved, domain.ApprovalDenied, domain.ApprovalExpired:
					decided = true
				default:
					return nil
				}
			}
			if decided && !pending {
				return r.store.UpdateTaskState(ctx, task.ID, task.State, domain.TaskStateWorking, "approval_decided", `{}`)
			}
		}
	}
	return nil
}

func (r *Reconciler) launchTask(ctx context.Context, task domain.Task, wl domain.Workload, adapter scruntime.Adapter) (launchErr error) {
	// Claim durably before issuing any runtime side effects.
	r.mu.Lock()
	err := r.store.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, domain.TaskStateWorking, "dispatch_claimed", `{}`)
	if err == nil {
		r.launching[task.ID] = true
	}
	r.mu.Unlock()
	if err != nil {
		return err
	}
	defer func() {
		if launchErr != nil {
			task.State = domain.TaskStateWorking
			cleanupErr := r.failTask(context.WithoutCancel(ctx), task, "launch_failed", `{}`)
			if !errors.Is(cleanupErr, store.ErrConflict) {
				launchErr = errors.Join(launchErr, cleanupErr)
			}
		}
		r.mu.Lock()
		delete(r.launching, task.ID)
		r.mu.Unlock()
	}()
	token, err := model.MintTaskToken(ctx, r.store, task.ID, wl.Metadata.Name, time.Hour)
	if err != nil {
		return err
	}

	inputJSON := task.InputJSON
	// Assemble bounded context-packet.md and resolve optional model tier for non-deterministic workloads.
	if wl.Spec.Class != domain.ClassDeterministicController {
		pkt := contextpack.Assemble(ctx, r.store, task, wl)
		payload := make(map[string]any)
		if err := json.Unmarshal([]byte(inputJSON), &payload); err != nil || payload == nil {
			payload = map[string]any{"prompt": inputJSON}
		}
		if tier, _ := payload["tier"].(string); tier != "" {
			prov, _ := payload["provider"].(string)
			mod, _ := payload["model"].(string)
			if prov == "" && mod == "" {
				if tp, tm := contextpack.ResolveTier(ctx, r.store, tier); tp != "" || tm != "" {
					if tp != "" {
						payload["provider"] = tp
					}
					if tm != "" {
						payload["model"] = tm
					}
				}
			}
		}
		if pkt.Markdown != "" {
			payload["context_packet"] = pkt.Markdown
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		inputJSON = string(raw)
	}

	spec := scruntime.LaunchSpec{
		TaskID:      task.ID,
		Workload:    wl,
		InputJSON:   inputJSON,
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
		return err
	}

	r.mu.Lock()
	r.handles[task.ID] = handle
	r.handleAdapters[task.ID] = adapter
	current, err := r.store.GetTask(ctx, task.ID)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	// The gate may already have requested approval while Start was returning.
	if terminalTaskState(current.State) {
		if err := r.reconcileTerminal(context.WithoutCancel(ctx), current); err != nil {
			return err
		}
		return store.ErrConflict
	}
	if current.State != domain.TaskStateWorking && current.State != domain.TaskStateInputRequired {
		return store.ErrConflict
	}
	if err := r.store.RecordHeartbeat(ctx, task.ID, r.now()); err != nil {
		return err
	}
	return nil
}

func (r *Reconciler) advanceWorkflowStep(ctx context.Context, completedTask domain.Task) error {
	// Reload completedTask so ArtifactsJSON populated by persistArtifacts is visible.
	fresh, err := r.store.GetTask(ctx, completedTask.ID)
	if err != nil {
		return err
	}
	completedTask = fresh
	var meta struct {
		Workflow        string `json:"workflow"`
		StepIndex       int    `json:"step_index"`
		Input           string `json:"input"`
		Role            string `json:"role"`
		MaxRetries      int    `json:"max_retries"`
		RetryCount      int    `json:"retry_count"`
		AuthorTaskID    string `json:"author_task_id"`
		AuthorStepIndex int    `json:"author_step_index"`
	}
	if err := json.Unmarshal([]byte(completedTask.InputJSON), &meta); err != nil || meta.Workflow == "" {
		return nil
	}
	r.mu.Lock()
	b := r.bundle
	r.mu.Unlock()
	if b == nil {
		return nil
	}
	wf, ok := b.Workflows[meta.Workflow]
	if !ok {
		return nil
	}

	// If a verifier step rejected the author's output, trigger opt-in remediation or halt advancement.
	if meta.Role == "verifier" {
		msgs, err := r.store.ListTaskMessages(ctx, completedTask.ID)
		if err != nil {
			return err
		}
		verdict, findings := contextpack.ParseVerdict(completedTask.ArtifactsJSON, msgs)
		if verdict == contextpack.VerdictChangesRequested {
			_ = r.store.AppendTaskMessage(ctx, domain.TaskMessage{
				TaskID:    completedTask.ID,
				Role:      "system",
				Content:   "Verifier returned VERDICT: CHANGES_REQUESTED.",
				CreatedAt: r.now(),
			})
			_ = r.store.AppendAudit(ctx, domain.AuditRecord{
				Category:     "workflow",
				TaskID:       completedTask.ID,
				WorkloadName: completedTask.WorkloadName,
				Action:       "workflow.verify",
				Outcome:      "changes_requested",
				DetailJSON:   fmt.Sprintf(`{"workflow":%q,"step_index":%d,"retry_count":%d,"max_retries":%d}`, meta.Workflow, meta.StepIndex, meta.RetryCount, meta.MaxRetries),
				CreatedAt:    r.now(),
			})
			if meta.MaxRetries > 0 && meta.RetryCount < meta.MaxRetries && meta.AuthorStepIndex >= 0 && meta.AuthorStepIndex < len(wf.Spec.Steps) {
				authorStep := wf.Spec.Steps[meta.AuthorStepIndex]
				prov, mod := authorStep.Provider, authorStep.Model
				if authorStep.Tier != "" && prov == "" && mod == "" {
					prov, mod = contextpack.ResolveTier(ctx, r.store, authorStep.Tier)
				}
				handoff := fmt.Sprintf("# Verifier Feedback (CHANGES_REQUESTED)\n\nVerifier task: `%s`\n\n%s\n", completedTask.ID, findings)
				payload := map[string]any{
					"workflow":        meta.Workflow,
					"step":            authorStep.Name,
					"step_index":      meta.AuthorStepIndex,
					"prompt":          authorStep.Prompt,
					"input":           meta.Input,
					"role":            authorStep.Role,
					"tier":            authorStep.Tier,
					"max_retries":     meta.MaxRetries,
					"retry_count":     meta.RetryCount + 1,
					"auto_remediate":  true,
					"failure_handoff": handoff,
				}
				if prov != "" {
					payload["provider"] = prov
				}
				if mod != "" {
					payload["model"] = mod
				}
				raw, _ := json.Marshal(payload)
				idem := fmt.Sprintf("wf:%s:remediate:%s:%d", meta.Workflow, completedTask.ID, meta.RetryCount+1)
				remTask, err := r.SubmitTask(ctx, authorStep.Workload, idem, string(raw))
				if err != nil {
					return err
				}

				_ = r.store.AppendTaskMessage(ctx, domain.TaskMessage{
					TaskID:    remTask.ID,
					Role:      "system",
					Content:   fmt.Sprintf("Closed-loop remediation attempt %d/%d triggered by verifier %s.", meta.RetryCount+1, meta.MaxRetries, completedTask.ID),
					CreatedAt: r.now(),
				})
			}
			return nil
		}
	}

	nextIdx := meta.StepIndex + 1
	if nextIdx >= len(wf.Spec.Steps) {
		return nil
	}
	nextStep := wf.Spec.Steps[nextIdx]
	prov, mod := nextStep.Provider, nextStep.Model
	if nextStep.Tier != "" && prov == "" && mod == "" {
		prov, mod = contextpack.ResolveTier(ctx, r.store, nextStep.Tier)
	}
	prompt := nextStep.Prompt
	if nextStep.Role == "verifier" {
		prompt = contextpack.BuildVerifierPrompt(completedTask, nextStep.Prompt)
	}
	payload := map[string]any{
		"workflow":   meta.Workflow,
		"step":       nextStep.Name,
		"step_index": nextIdx,
		"prompt":     prompt,
		"input":      meta.Input,
	}
	if nextStep.Role != "" {
		payload["role"] = nextStep.Role
	}
	if nextStep.Role == "verifier" {
		payload["author_task_id"] = completedTask.ID
		payload["author_step_index"] = meta.StepIndex
		maxR := nextStep.MaxRetries
		if maxR == 0 {
			maxR = meta.MaxRetries
		}
		if maxR > 0 {
			payload["max_retries"] = maxR
			payload["retry_count"] = meta.RetryCount
		}
	} else if nextStep.MaxRetries > 0 {
		payload["max_retries"] = nextStep.MaxRetries
		payload["auto_remediate"] = true
	}
	if nextStep.Tier != "" {
		payload["tier"] = nextStep.Tier
	}
	if prov != "" {
		payload["provider"] = prov
	}
	if mod != "" {
		payload["model"] = mod
	}
	raw, _ := json.Marshal(payload)
	nextID, err := newTaskID()
	if err != nil {
		return err
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
	if err := r.store.CreateTask(ctx, child); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil
		}
		return err
	}
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
	return nil
}

func (r *Reconciler) maybeAutoRemediate(ctx context.Context, failed domain.Task, reason string) error {
	var meta struct {
		AutoRemediate bool `json:"auto_remediate"`
		MaxRetries    int  `json:"max_retries"`
		RetryCount    int  `json:"retry_count"`
	}
	if err := json.Unmarshal([]byte(failed.InputJSON), &meta); err != nil {
		return nil
	}
	maxR := meta.MaxRetries
	if meta.AutoRemediate && maxR == 0 {
		maxR = 1
	}
	if maxR <= 0 || meta.RetryCount >= maxR {
		return nil
	}
	rem, err := r.RetryTaskWithHandoff(ctx, failed.ID, true, reason)
	if err != nil {
		return err
	}
	_ = r.store.AppendAudit(ctx, domain.AuditRecord{
		Category:     "controlplane",
		TaskID:       rem.ID,
		WorkloadName: rem.WorkloadName,
		Action:       "task.auto_remediate",
		Outcome:      "submitted",
		DetailJSON:   fmt.Sprintf(`{"failed_task_id":%q,"attempt":%d,"max_retries":%d}`, failed.ID, meta.RetryCount+1, maxR),
		CreatedAt:    r.now(),
	})
	return nil
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
