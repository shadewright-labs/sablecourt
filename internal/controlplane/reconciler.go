package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/gate/model"
	"github.com/shadewright-labs/sablecourt/internal/policy"
	"github.com/shadewright-labs/sablecourt/internal/redact"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const (
	taskLeaseTTL        = time.Hour
	maxParallelLaunches = 4
	adapterCallTimeout  = 15 * time.Second
	adapterStartTimeout = 30 * time.Second
)

var errWorkflowKeyReserved = errors.New("wf: keys are reserved for workflows")

type Options struct {
	Store        store.Store
	Bundle       *config.Bundle
	Policy       *policy.Engine
	Adapters     map[domain.WorkloadClass]scruntime.Adapter
	ModelGateURL string
	DataGateURL  string
	// HostDataGateURL is the data gate address for coding agents that run on the host.
	HostDataGateURL string
	StalenessWindow time.Duration
	MaxConcurrent   int
	OnTaskFailed    func(task domain.Task, reason string)
	Now             func() time.Time
}

type launchReservation struct {
	done    chan struct{}
	tokenMu sync.Mutex
}

type Reconciler struct {
	store           store.Store
	bundle          *config.Bundle
	policy          *policy.Engine
	adapters        map[domain.WorkloadClass]scruntime.Adapter
	modelGateURL    string
	dataGateURL     string
	hostDataGateURL string
	stalenessWindow time.Duration
	maxConcurrent   int
	onTaskFailed    func(task domain.Task, reason string)
	now             func() time.Time

	instance          string
	recovering        map[string]bool
	reconcileMu       sync.Mutex
	mu                sync.Mutex
	handles           map[string]scruntime.Handle
	handleAdapters    map[string]scruntime.Adapter
	partialStarts     map[string]bool
	partialStartFacts map[string]string
	launching         map[string]*launchReservation
	launchErrors      error
	stopping          map[string]chan struct{}
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
	if terminalTaskState(task.State) {
		return store.ErrConflict
	}
	r.mu.Lock()
	_, local := r.handles[taskID]
	launching := r.launching[taskID] != nil
	r.mu.Unlock()
	if !local && !launching && task.State != domain.TaskStateSubmitted {
		if err := r.claimRecovery(ctx, taskID); err != nil {
			return err
		}
	}
	if reason == "" {
		reason = "operator_cancelled"
	}
	if err := r.store.UpdateTaskState(ctx, taskID, task.State, domain.TaskStateCancelled, reason, `{}`); err != nil {
		return err
	}
	cleanupCtx := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		cleanupCtx, cancel = context.WithDeadline(cleanupCtx, deadline)
		defer cancel()
	}
	r.mu.Lock()
	reservation := r.launching[taskID]
	r.mu.Unlock()
	if reservation != nil {
		reservation.tokenMu.Lock()
	}
	leaseErr := r.store.RevokeTaskLeases(cleanupCtx, taskID)
	if reservation != nil {
		reservation.tokenMu.Unlock()
	}
	cleanupErr := errors.Join(leaseErr, r.stopTask(cleanupCtx, taskID))
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
	for r.stopping[taskID] != nil {
		done := r.stopping[taskID]
		r.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		r.mu.Lock()
	}
	handle, ok := r.handles[taskID]
	adapter := r.handleAdapters[taskID]
	if !ok {
		done := make(chan struct{})
		r.stopping[taskID] = done
		bundle := r.bundle
		r.mu.Unlock()

		defer func() {
			r.mu.Lock()
			delete(r.stopping, taskID)
			close(done)
			r.mu.Unlock()
		}()

		rec, persisted, valid, loadErr := r.loadPersistedHandle(ctx, taskID)
		if loadErr != nil {
			return loadErr
		}
		if !valid {
			return r.deletePersistedHandle(ctx, taskID)
		}
		task, err := r.store.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		persistedAdapter := r.resolveRecoveryAdapter(task, rec, bundle)
		if persistedAdapter == nil && isContainerHandle(persisted) {
			persistedAdapter = r.persistedClassAdapter(rec)
		}
		if persistedAdapter == nil {
			return errors.New("runtime adapter unavailable for cleanup")
		}
		alive, probeErr := r.canRecoverHandle(ctx, persistedAdapter, persisted)
		if probeErr != nil {
			return probeErr
		}
		if !alive && !isContainerHandle(persisted) {
			// Process is gone or PID was reused; never signal an unverified PID.
			return r.deletePersistedHandle(ctx, taskID)
		}
		callCtx, cancel := context.WithTimeout(ctx, adapterCallTimeout)
		stopErr := persistedAdapter.Stop(callCtx, persisted)
		if callCtx.Err() != nil {
			stopErr = callCtx.Err()
		}
		cancel()
		if stopErr != nil {
			// Retain the verified handle in memory so the next reconcile pass retries cleanup
			// and RetryTaskWithHandoff waits until the live process stops.
			r.mu.Lock()
			r.handles[taskID] = persisted
			r.handleAdapters[taskID] = persistedAdapter
			r.mu.Unlock()
			return errors.New("runtime stop failed")
		}
		return r.deletePersistedHandle(ctx, taskID)
	}
	if adapter == nil {
		r.mu.Unlock()
		return errors.New("runtime adapter unavailable for cleanup")
	}
	done := make(chan struct{})
	r.stopping[taskID] = done
	r.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, adapterCallTimeout)
	err := adapter.Stop(callCtx, handle)
	cancel()
	r.mu.Lock()
	delete(r.stopping, taskID)
	close(done)
	if err == nil {
		delete(r.handles, taskID)
		delete(r.handleAdapters, taskID)
		delete(r.partialStarts, taskID)
		delete(r.partialStartFacts, taskID)
	}
	r.mu.Unlock()
	if err != nil {
		return errors.New("runtime stop failed")
	}
	return r.deletePersistedHandle(ctx, taskID)
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
	if r.launching[taskID] != nil {
		r.mu.Unlock()
		return fmt.Errorf("%w: task launch is in progress", store.ErrConflict)
	}
	handle, hasHandle := r.handles[taskID]
	adapter := r.handleAdapters[taskID]
	r.mu.Unlock()

	cp := scruntime.CheckpointRef{ID: "cp-" + taskID, CreatedAt: r.now()}
	if hasHandle && adapter != nil {
		callCtx, cancel := context.WithTimeout(ctx, adapterCallTimeout)
		c, err := adapter.Suspend(callCtx, handle)
		cancel()
		if err == nil {
			cp = c
		}
		callCtx, cancel = context.WithTimeout(ctx, adapterCallTimeout)
		_ = adapter.Stop(callCtx, handle)
		cancel()
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
func (r *Reconciler) ResumeTask(ctx context.Context, taskID string) (resumeErr error) {
	r.mu.Lock()
	if r.launching[taskID] != nil {
		r.mu.Unlock()
		return store.ErrConflict
	}
	attempt := &launchReservation{done: make(chan struct{})}
	r.launching[taskID] = attempt
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.launching, taskID)
		close(attempt.done)
		r.mu.Unlock()
	}()

	attempt.tokenMu.Lock()
	task, err := r.store.GetTask(ctx, taskID)
	if err == nil && task.State != domain.TaskStateSuspended {
		err = fmt.Errorf("%w: task %s is in state %s (must be suspended)", store.ErrConflict, taskID, task.State)
	}
	if err == nil {
		_, err = model.MintTaskToken(ctx, r.store, taskID, task.WorkloadName, taskLeaseTTL)
	}
	attempt.tokenMu.Unlock()
	if err != nil {
		return err
	}

	var cp scruntime.CheckpointRef
	if refJSON, err := r.store.LatestCheckpoint(ctx, taskID); err == nil {
		_ = json.Unmarshal([]byte(refJSON), &cp)
	}

	r.mu.Lock()
	handle, hasHandle := r.handles[taskID]
	adapter := r.handleAdapters[taskID]
	r.mu.Unlock()

	defer func() {
		if resumeErr == nil {
			return
		}
		cleanupCtx := context.WithoutCancel(ctx)
		resumeErr = errors.Join(resumeErr, r.store.RevokeTaskLeases(cleanupCtx, taskID))
		if hasHandle && adapter != nil {
			// Cancellation may already have removed the handle before Resume returned.
			stopCtx, cancel := context.WithTimeout(cleanupCtx, adapterCallTimeout)
			defer cancel()
			if err := adapter.Stop(stopCtx, handle); err != nil {
				resumeErr = errors.Join(resumeErr, errors.New("runtime stop failed"))
			}
		}
	}()
	if hasHandle && adapter != nil {
		callCtx, cancel := context.WithTimeout(ctx, adapterCallTimeout)
		err := adapter.Resume(callCtx, handle, cp)
		if callCtx.Err() != nil {
			err = callCtx.Err()
		}
		cancel()
		if err != nil {
			return errors.New("runtime resume failed")
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
		store:             opts.Store,
		instance:          rand.Text(),
		recovering:        make(map[string]bool),
		bundle:            opts.Bundle,
		policy:            opts.Policy,
		adapters:          opts.Adapters,
		modelGateURL:      opts.ModelGateURL,
		dataGateURL:       opts.DataGateURL,
		hostDataGateURL:   opts.HostDataGateURL,
		stalenessWindow:   win,
		maxConcurrent:     opts.MaxConcurrent,
		onTaskFailed:      opts.OnTaskFailed,
		now:               nowFn,
		handles:           make(map[string]scruntime.Handle),
		handleAdapters:    make(map[string]scruntime.Adapter),
		partialStarts:     make(map[string]bool),
		partialStartFacts: make(map[string]string),
		launching:         make(map[string]*launchReservation),
		stopping:          make(map[string]chan struct{}),
	}
}

func (r *Reconciler) SubmitTask(ctx context.Context, workloadName, idempotencyKey, inputJSON string) (domain.Task, error) {
	return r.submitTask(ctx, workloadName, idempotencyKey, inputJSON, "", nil)
}

func (r *Reconciler) submitTask(ctx context.Context, workloadName, idempotencyKey, inputJSON, parentID string, step *workflowStepRecord) (domain.Task, error) {
	// Every submit path (API, A2A, UI, webhooks, schedules, workflow steps,
	// retries, council turns) passes here, so the stored input, its task
	// message and what readers get back hold no known secret form.
	inputJSON = redact.JSON(inputJSON)
	if step == nil && strings.HasPrefix(idempotencyKey, "wf:") {
		return domain.Task{}, errWorkflowKeyReserved
	}
	if strings.HasPrefix(idempotencyKey, "conclave-") && !store.HasConclaveReservation(ctx, idempotencyKey) {
		return domain.Task{}, errors.New("conclave submission requires a run reservation")
	}
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
	if step != nil {
		if idempotencyKey != "" {
			digest := sha256.Sum256([]byte(idempotencyKey))
			id = "task_" + hex.EncodeToString(digest[:16])
		}
		if step.RunID == "" {
			step.RunID = id
		}
	}
	if idempotencyKey == "" {
		idempotencyKey = id
	}
	now := r.now()
	task := domain.Task{
		ID:             id,
		ParentTaskID:   parentID,
		IdempotencyKey: idempotencyKey,
		WorkloadName:   workloadName,
		State:          domain.TaskStateSubmitted,
		InputJSON:      inputJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if step != nil {
		entry, encodeErr := r.workflowStepEntry(id, *step)
		if encodeErr != nil {
			return domain.Task{}, encodeErr
		}
		err = store.CreateTaskWithMemory(ctx, r.store, task, entry)
	} else {
		err = r.store.CreateTask(ctx, task)
	}
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			if step != nil {
				existing, getErr := r.store.GetTask(ctx, id)
				if getErr == nil && existing.IdempotencyKey == idempotencyKey && existing.WorkloadName == workloadName && existing.ParentTaskID == parentID {
					return existing, nil
				}
				return domain.Task{}, err
			}
			// The unique key selects the winner even when submissions race.
			existing, lookupErr := r.store.GetTaskByIdempotencyKey(ctx, idempotencyKey)
			if lookupErr == nil && existing.WorkloadName == workloadName && existing.ParentTaskID == parentID {
				return existing, nil
			}
			if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
				return domain.Task{}, lookupErr
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
	return r.startWorkflow(ctx, workflowName, "", userInput)
}

func (r *Reconciler) startWorkflow(ctx context.Context, workflowName, idempotencyKey, userInput string) (domain.Task, error) {
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

	for _, step := range wf.Spec.Steps {
		if step.Tier == "" {
			continue
		}
		wl, ok := b.Workloads[step.Workload]
		if !ok {
			return domain.Task{}, fmt.Errorf("workflow step %q has no configured workload", step.Name)
		}
		if _, _, err := resolveTierRoute(ctx, r.store, wl, step.Tier, step.Provider, step.Model); err != nil {
			return domain.Task{}, fmt.Errorf("workflow step %q: %w", step.Name, err)
		}
	}

	step := wf.Spec.Steps[0]
	prov, mod := step.Provider, step.Model
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
	return r.submitTask(ctx, step.Workload, idempotencyKey, string(raw), "", &workflowStepRecord{Workflow: workflowName})
}

type councilCleaner interface {
	CleanupCouncil(ctx context.Context, conclaveID string) error
}

// CleanupCouncil removes shared council runtime checkout state after all attempts stop.
func (r *Reconciler) CleanupCouncil(ctx context.Context, conclaveID string) error {
	var errs []error
	for _, adapter := range r.adapters {
		cleaner, ok := adapter.(councilCleaner)
		if !ok {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, adapterCallTimeout)
		err := cleaner.CleanupCouncil(callCtx, conclaveID)
		if callCtx.Err() != nil {
			err = callCtx.Err()
		}
		cancel()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CouncilRetryReady checks for unfinished cleanup, including handles and leases
// left by the previous daemon. Recover owns the cleanup itself.
func (r *Reconciler) CouncilRetryReady(ctx context.Context, id string) (bool, error) {
	r.mu.Lock()
	_, attached := r.handles[id]
	busy := attached || r.launching[id] != nil || r.recovering[id] || r.stopping[id] != nil
	r.mu.Unlock()
	if busy {
		return false, nil
	}
	pending, err := r.store.ListReconcileTasks(ctx, nil)
	if err != nil {
		return false, err
	}
	return !slices.ContainsFunc(pending, func(task domain.Task) bool { return task.ID == id }), nil
}

// RetryTaskWithHandoff retries a task, optionally attaching a standardized failure-handoff.md
// containing redacted artifacts, lifecycle events, and failure context.
func (r *Reconciler) RetryTaskWithHandoff(ctx context.Context, origTaskID string, withHandoff bool, reason string) (domain.Task, error) {
	orig, err := r.store.GetTask(ctx, origTaskID)
	if err != nil {
		return domain.Task{}, err
	}
	if a, ok := store.TaskConclaveAssignment(orig); ok && a.Round > 0 {
		return domain.Task{}, fmt.Errorf("%w: council retries require a planned reservation", store.ErrConflict)
	}
	if !terminalTaskState(orig.State) {
		return domain.Task{}, fmt.Errorf("%w: retry requires a terminal task", store.ErrConflict)
	}
	r.mu.Lock()
	_, hasHandle := r.handles[orig.ID]
	launching := r.launching[orig.ID]
	r.mu.Unlock()
	if hasHandle || launching != nil {
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
	if !strings.HasPrefix(orig.IdempotencyKey, "conclave-") {
		idem := fmt.Sprintf("retry-%s-%d", orig.ID, attempt)
		member, err := r.workflowStep(ctx, orig.ID)
		if err != nil {
			return domain.Task{}, err
		}
		if member != nil && payload["role"] != "verifier" {
			if commit := authorCommit(orig.ArtifactsJSON); commit != "" {
				member.AuthorCommit = commit
			}
		}
		return r.submitTask(ctx, orig.WorkloadName, idem, string(raw), orig.ParentTaskID, member)
	}
	assignment, ok := store.TaskConclaveAssignment(orig)
	if !ok {
		return domain.Task{}, errors.New("invalid conclave task assignment")
	}
	room, err := r.store.GetConclave(ctx, assignment.ConclaveID)
	if err != nil {
		return domain.Task{}, err
	}
	r.mu.Lock()
	bundle := r.bundle
	r.mu.Unlock()
	if bundle == nil {
		return domain.Task{}, errors.New("no workload bundle loaded")
	}
	tpl, ok := bundle.Conclaves[room.TemplateName]
	if !ok {
		return domain.Task{}, errors.New("conclave template not found")
	}
	key := assignment.RetryKey(orig.ID, attempt)
	limits := store.LimitsForConclave(tpl, true, bundle.Budgets, r.now)
	return store.SubmitConclaveTurn(ctx, room.ID, key, limits, func(ctx context.Context) (domain.Task, error) {
		return r.SubmitTask(ctx, orig.WorkloadName, key, string(raw))
	})
}

// ProposeChildTask enforces strict hub-and-spoke delegation: parent must hold a delegate:<target> grant
// and pass deterministic policy before the control plane dispatches the child task.
func (r *Reconciler) ProposeChildTask(ctx context.Context, parentTaskID, targetWorkload, idempotencyKey, inputJSON, subBudgetJSON string) (domain.Task, error) {
	inputJSON = redact.JSON(inputJSON)
	if strings.HasPrefix(idempotencyKey, "wf:") {
		return domain.Task{}, errWorkflowKeyReserved
	}
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

// ReconcileOnce observes tasks and queues bounded launches.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	recoveryErr := r.Recover(ctx)
	// Only one pass at a time can observe and reserve task slots.
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	r.mu.Lock()
	launching := maps.Clone(r.launching)
	handleIDs := slices.Collect(maps.Keys(r.handles))
	bundle := r.bundle
	passErr := errors.Join(recoveryErr, r.launchErrors)
	r.launchErrors = nil
	r.mu.Unlock()

	tasks, err := r.store.ListReconcileTasks(ctx, handleIDs)
	if err != nil {
		return errors.Join(passErr, err)
	}
	// First reconcile terminal and active tasks so lost, stale, or finished tasks
	// release concurrency slots before submitted tasks check maxConcurrent.
	activeWorking := 0
	var submitted []domain.Task
	for _, task := range tasks {
		if launching[task.ID] != nil {
			activeWorking++
			continue
		}
		if terminalTaskState(task.State) {
			activeWorking++
			continue
		}
		if task.State == domain.TaskStateSubmitted {
			submitted = append(submitted, task)
			continue
		}
		if task.State != domain.TaskStateWorking && task.State != domain.TaskStateInputRequired {
			continue
		}

		r.mu.Lock()
		_, hasHandle := r.handles[task.ID]
		partialStart := r.partialStarts[task.ID]
		recovering := r.recovering[task.ID]
		r.mu.Unlock()
		if (!hasHandle && !partialStart) || recovering {
			activeWorking++
			continue
		}

		var wl domain.Workload
		var ok bool
		if bundle != nil {
			wl, ok = bundle.Workloads[task.WorkloadName]
		}
		reason := ""
		if !ok {
			reason = "workload_missing"
		} else if r.adapters[wl.Spec.Class] == nil {
			reason = "adapter_missing"
		}
		if reason != "" {
			if err := r.failTask(ctx, task, reason, `{}`); err != nil {
				activeWorking++
				passErr = errors.Join(passErr, err)
			}
			continue
		}

		stillActive, obsErr := r.observeTask(ctx, task)
		if !errors.Is(obsErr, store.ErrConflict) {
			passErr = errors.Join(passErr, obsErr)
		}
		if stillActive || obsErr != nil {
			activeWorking++
		}
	}
	for _, task := range submitted {
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
		if r.maxConcurrent > 0 && activeWorking >= r.maxConcurrent {
			continue
		}
		if r.claimLaunch(task.ID) {
			activeWorking++
			go r.launchTask(ctx, task, wl, adapter)
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
	cleanupErr := r.stopTask(cleanupCtx, task.ID)
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

func (r *Reconciler) observeTask(ctx context.Context, task domain.Task) (bool, error) {
	r.mu.Lock()
	handle, hasHandle := r.handles[task.ID]
	adapter := r.handleAdapters[task.ID]
	partialStart := r.partialStarts[task.ID]
	partialFacts := r.partialStartFacts[task.ID]
	r.mu.Unlock()
	if partialStart {
		if partialFacts == "" {
			partialFacts = `{}`
		}
		return false, r.failTask(ctx, task, "launch_failed", partialFacts)
	}
	if !hasHandle || adapter == nil {
		return false, r.failLostTask(ctx, task, `{"reason":"handle_missing"}`)
	}
	assignment, assigned := store.TaskConclaveAssignment(task)
	council := assigned && assignment.Round > 0
	stale, err := r.staleWithoutExit(ctx, task, adapter, handle)
	if err != nil {
		return true, err
	}
	if stale {
		if !council {
			return false, r.failTask(ctx, task, reasonLostOnRestart, `{"reason":"stale_observation"}`)
		}
		if err := r.store.RevokeTaskLeases(context.WithoutCancel(ctx), task.ID); err != nil {
			return true, err
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, adapterCallTimeout)
	obs, observeErr := adapter.Observe(callCtx, handle)
	if callCtx.Err() != nil {
		observeErr = callCtx.Err()
	}
	cancel()
	if errors.Is(observeErr, scruntime.ErrProcessUnverified) {
		return true, errors.Join(observeErr, r.store.RevokeTaskLeases(context.WithoutCancel(ctx), task.ID))
	}
	var earlyRevokeErr error
	if errors.Is(observeErr, scruntime.ErrProcessNotFound) ||
		(observeErr == nil && (obs.State == scruntime.StateCompleted || obs.State == scruntime.StateFailed)) {
		earlyRevokeErr = r.store.RevokeTaskLeases(context.WithoutCancel(ctx), task.ID)
		if earlyRevokeErr != nil && council {
			return true, earlyRevokeErr
		}
	}
	events, err := r.store.ListTaskEvents(ctx, task.ID)
	if err != nil {
		return true, errors.Join(earlyRevokeErr, err)
	}
	failures := observationFailures(events)
	reason := ""
	if errors.Is(observeErr, scruntime.ErrProcessNotFound) {
		if isContainerHandle(handle) {
			return false, r.failTask(ctx, task, reasonLostOnRestart, `{"reason":"container_not_alive"}`)
		}
		return false, r.failLostTask(ctx, task, `{"reason":"process_not_alive"}`)
	}
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
		if council {
			// Keep the reservation until the runtime supplies a definite observation.
			cleanupCtx := context.WithoutCancel(ctx)
			err := r.store.RevokeTaskLeases(cleanupCtx, task.ID)
			if failures == 0 {
				err = errors.Join(err, r.store.UpdateTaskState(ctx, task.ID, task.State, task.State, reason, `{"consecutive_failures":1}`))
			}
			return true, err
		}
		if r.isTaskStale(task, r.now()) {
			return false, r.failTask(ctx, task, reasonLostOnRestart, `{"reason":"stale_observation"}`)
		}
		facts := fmt.Sprintf(`{"consecutive_failures":%d}`, failures+1)
		if failures+1 >= 3 {
			return false, r.failTask(ctx, task, reason, facts)
		}
		// Failed observations never refresh the last known heartbeat.
		return true, r.store.UpdateTaskState(ctx, task.ID, task.State, task.State, reason, facts)
	}
	if stale && (obs.State == scruntime.StateRunning || obs.State == scruntime.StateSuspended) {
		return false, r.failTask(ctx, task, reasonLostOnRestart, `{"reason":"stale_observation"}`)
	}
	if failures > 0 {
		if err := r.store.UpdateTaskState(ctx, task.ID, task.State, task.State, "observation_recovered", `{}`); err != nil {
			return true, err
		}
	}
	now := r.now()
	hasTimestamp := !obs.ObservedAt.IsZero() && !obs.ObservedAt.After(now)
	if hasTimestamp && (task.LastHeartbeatAt == nil || !obs.ObservedAt.Before(*task.LastHeartbeatAt)) {
		if err := r.store.RecordHeartbeat(ctx, task.ID, obs.ObservedAt); err != nil {
			return true, errors.Join(earlyRevokeErr, err)
		}
		ts := obs.ObservedAt
		task.LastHeartbeatAt = &ts
	}
	switch obs.State {
	case scruntime.StateCompleted, scruntime.StateFailed:
		target := domain.TaskStateCompleted
		if obs.State == scruntime.StateFailed || (council && obs.ExitCode != 0) {
			target = domain.TaskStateFailed
		}
		if err := r.persistArtifacts(ctx, task.ID, adapter, handle, assignment, target); err != nil {
			return true, errors.Join(earlyRevokeErr, err, r.store.RevokeTaskLeases(context.WithoutCancel(ctx), task.ID))
		}
		if err := r.store.UpdateTaskState(ctx, task.ID, task.State, target, "runtime_exit_observed", fmt.Sprintf(`{"exit_code":%d}`, obs.ExitCode)); err != nil {
			return true, errors.Join(earlyRevokeErr, err)
		}
		if handle.AdapterName != "docker" {
			r.mu.Lock()
			delete(r.handles, task.ID)
			delete(r.handleAdapters, task.ID)
			r.mu.Unlock()
			_ = r.deletePersistedHandle(context.WithoutCancel(ctx), task.ID)
		}
		task.State = target
		return false, errors.Join(earlyRevokeErr, r.reconcileTerminal(context.WithoutCancel(ctx), task))
	case scruntime.StateRunning:
		now := r.now()
		age := now.Sub(obs.ObservedAt)
		if hasTimestamp && age >= 0 && age <= r.stalenessWindow {
			expiresAt := obs.ObservedAt.Add(taskLeaseTTL)
			if council {
				deadline, err := r.councilDeadline(ctx, assignment.ConclaveID)
				if err != nil {
					return true, err
				}
				if !now.Before(deadline) {
					return true, r.store.RevokeTaskLeases(context.WithoutCancel(ctx), task.ID)
				}
				if expiresAt.After(deadline) {
					expiresAt = deadline
				}
			}
			if err := r.store.RenewTaskLeases(ctx, task.ID, now, expiresAt); err != nil {
				return true, err
			}
		} else if r.isTaskStale(task, now) {
			return false, r.failTask(ctx, task, reasonLostOnRestart, `{"reason":"stale_observation"}`)
		}
		if task.State == domain.TaskStateInputRequired {
			approvals, err := r.store.ListApprovals(ctx, "")
			if err != nil {
				return true, err
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
						return true, err
					}
					decided = true
				case domain.ApprovalApproved, domain.ApprovalDenied, domain.ApprovalExpired:
					decided = true
				default:
					return true, nil
				}
			}
			if decided && !pending {
				return true, r.store.UpdateTaskState(ctx, task.ID, task.State, domain.TaskStateWorking, "approval_decided", `{}`)
			}
		}
	}
	return true, nil
}

func (r *Reconciler) claimLaunch(taskID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.launching) >= maxParallelLaunches || r.launching[taskID] != nil {
		return false
	}
	r.launching[taskID] = &launchReservation{done: make(chan struct{})}
	return true
}

func launchFailureFacts(err error) string {
	var missing *scruntime.MissingBinaryError
	if errors.As(err, &missing) && missing.Path != "" {
		raw, marshalErr := json.Marshal(struct {
			Reason string `json:"reason"`
			Path   string `json:"path"`
		}{
			Reason: "missing_binary",
			Path:   redact.String(missing.Path),
		})
		if marshalErr == nil {
			return string(raw)
		}
	}
	return `{}`
}

func (r *Reconciler) launchTask(ctx context.Context, task domain.Task, wl domain.Workload, adapter scruntime.Adapter) (launchErr error) {
	claimed := false
	startFailed := false
	launchFacts := `{}`
	defer func() {
		if launchErr != nil && claimed {
			cleanupCtx := context.WithoutCancel(ctx)
			reason := "launch_failed"
			if errors.Is(launchErr, errAuthorCheckoutUnavailable) {
				reason = "author_checkout_unavailable"
			}
			cleanupErr := r.failTask(cleanupCtx, task, reason, launchFacts)
			if startFailed && cleanupErr != nil {
				launchErr = errors.Join(launchErr, r.store.RevokeTaskLeases(cleanupCtx, task.ID))
			}
			if cleanupErr != nil && !errors.Is(cleanupErr, store.ErrConflict) {
				launchErr = errors.Join(launchErr, cleanupErr)
			}
		}
		r.mu.Lock()
		if reservation := r.launching[task.ID]; reservation != nil {
			if launchErr != nil && launchErr != store.ErrConflict {
				r.launchErrors = errors.Join(r.launchErrors, launchErr)
			}
			delete(r.launching, task.ID)
			close(reservation.done)
		}
		r.mu.Unlock()
	}()

	assignment, assigned := store.TaskConclaveAssignment(task)
	if strings.HasPrefix(task.IdempotencyKey, "conclave-") && !assigned {
		return store.ErrConflict
	}
	var err error
	if assigned {
		err = store.ClaimConclaveTask(ctx, r.store, task.ID, assignment.ConclaveID, r.now)
	} else {
		err = r.store.UpdateTaskState(ctx, task.ID, domain.TaskStateSubmitted, domain.TaskStateWorking, "dispatch_claimed", `{}`)
	}
	if err != nil {
		return err
	}
	claimed = true
	task.State = domain.TaskStateWorking

	var commit string
	var readOnly bool
	if assignment.Round == 0 {
		commit, readOnly, err = r.workflowCheckout(ctx, task, wl)
		if err != nil {
			return err
		}
	}
	starter, supportsPin := adapter.(pinnedStarter)
	if commit != "" && !supportsPin {
		return errAuthorCheckoutUnavailable
	}
	wl, inputJSON, err := r.prepareLaunchInput(ctx, task, wl)
	if err != nil {
		return err
	}
	if readOnly {
		var payload struct {
			Packet string `json:"context_packet"`
		}
		if err := json.Unmarshal([]byte(inputJSON), &payload); err != nil || payload.Packet == "" {
			return errors.New("missing verifier prompt")
		}
		facts, _ := json.Marshal(verifierLaunch{Commit: commit, PromptDigest: promptDigest(payload.Packet)})
		if err := r.store.UpdateTaskState(ctx, task.ID, task.State, task.State, "verifier_checkout_pinned", string(facts)); err != nil {
			return err
		}
	}
	var maxLeaseExpiry time.Time
	if assigned && assignment.Round > 0 {
		maxLeaseExpiry, err = r.councilDeadline(ctx, assignment.ConclaveID)
		if err != nil {
			return err
		}
	}
	token, err := r.issueLaunchToken(ctx, task.ID, maxLeaseExpiry)
	if err != nil {
		return err
	}

	spec := scruntime.LaunchSpec{
		TaskID:      task.ID,
		Workload:    wl,
		InputJSON:   inputJSON,
		DataGateURL: r.dataGateURL,
		TaskToken:   token,
	}
	if assigned && assignment.Round > 0 && wl.Spec.Class == domain.ClassCodingAgent && wl.Spec.Coding != nil {
		if cap, ok := adapter.(councilContainerCapable); ok && cap.SupportsCouncilContainers() {
			spec.CouncilContainer = true
			spec.CouncilDeadline = maxLeaseExpiry
			spec.CreateAttempt = rand.Text()
			preStart := scruntime.Handle{
				AdapterName:   "coding:" + wl.Spec.Coding.Harness,
				ExternalID:    task.ID,
				ContainerName: "sablecourt-council-" + task.ID,
				CreateAttempt: spec.CreateAttempt,
			}
			if err := r.saveHandle(ctx, task.ID, wl.Spec.Class, preStart); err != nil {
				return err
			}
		}
	}
	// Host coding agents reach the gate over loopback. A council container has no host loopback.
	if wl.Spec.Class == domain.ClassCodingAgent && !spec.CouncilContainer && r.hostDataGateURL != "" {
		spec.DataGateURL = r.hostDataGateURL
	}
	// Only non-deterministic workloads with explicit Model Gate grants receive Model Gate coordinates.
	if wl.Spec.Class != domain.ClassDeterministicController && wl.Spec.Grants.ModelGate.Allowed {
		spec.ModelGateURL = r.modelGateURL
		spec.ModelGateToken = token
	}

	callCtx, cancel := context.WithTimeout(ctx, adapterStartTimeout)
	var handle scruntime.Handle
	var startErr error
	if commit != "" {
		handle, startErr = starter.StartAtCommit(callCtx, spec, commit, readOnly)
	} else {
		handle, startErr = adapter.Start(callCtx, spec)
	}
	if callCtx.Err() != nil {
		startErr = callCtx.Err()
	}
	cancel()
	startFailed = startErr != nil
	if startFailed {
		launchFacts = launchFailureFacts(startErr)
	} else {
		if handle.LogPath == "" && handle.WorkDir != "" && strings.HasPrefix(handle.AdapterName, "coding:") {
			handle.LogPath = handle.WorkDir + ".log"
		}
	}

	if !startFailed || handle != (scruntime.Handle{}) {
		r.mu.Lock()
		r.handles[task.ID] = handle
		r.handleAdapters[task.ID] = adapter
		if startFailed {
			r.partialStarts[task.ID] = true
			if launchFacts != `{}` {
				r.partialStartFacts[task.ID] = launchFacts
			}
		}
		r.mu.Unlock()
	}

	current, err := r.store.GetTask(context.WithoutCancel(ctx), task.ID)
	if err != nil {
		return err
	}
	if startFailed {
		// Adapter errors can contain launch input or bearer tokens.
		startFailure := errors.New("runtime start failed")
		if terminalTaskState(current.State) {
			return errors.Join(startFailure, r.reconcileTerminal(context.WithoutCancel(ctx), current))
		}
		if current.State == domain.TaskStateInputRequired {
			task.State = current.State
		}
		return startFailure
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
	if err := r.saveHandle(ctx, task.ID, wl.Spec.Class, handle); err != nil {
		return err
	}
	if err := r.store.RecordHeartbeat(ctx, task.ID, r.now()); err != nil {
		return err
	}
	return nil
}

func (r *Reconciler) councilDeadline(ctx context.Context, conclaveID string) (time.Time, error) {
	councils, err := store.Councils(r.store)
	if err != nil {
		return time.Time{}, err
	}
	snap, err := councils.CouncilSnapshot(ctx, conclaveID)
	if err != nil {
		return time.Time{}, err
	}
	return snap.Deadline, nil
}

func (r *Reconciler) issueLaunchToken(ctx context.Context, taskID string, maxExpiry time.Time) (string, error) {
	r.mu.Lock()
	reservation := r.launching[taskID]
	r.mu.Unlock()
	if reservation == nil {
		return "", store.ErrConflict
	}
	// Cancellation transitions the task first and then revokes any lease minted here.
	reservation.tokenMu.Lock()
	defer reservation.tokenMu.Unlock()
	current, err := r.store.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if current.State != domain.TaskStateWorking {
		return "", store.ErrConflict
	}
	now := r.now()
	expiresAt := now.Add(taskLeaseTTL)
	if !maxExpiry.IsZero() {
		if !now.Before(maxExpiry) {
			return "", errors.New("council deadline reached")
		}
		if expiresAt.After(maxExpiry) {
			expiresAt = maxExpiry
		}
	}
	return model.MintTaskTokenUntil(ctx, r.store, current.ID, current.WorkloadName, now, expiresAt)
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
		Prompt          string `json:"prompt"`
		Role            string `json:"role"`
		MaxRetries      int    `json:"max_retries"`
		RetryCount      int    `json:"retry_count"`
		AuthorTaskID    string `json:"author_task_id"`
		AuthorStepIndex int    `json:"author_step_index"`
	}
	if err := json.Unmarshal([]byte(completedTask.InputJSON), &meta); err != nil || meta.Workflow == "" {
		return nil
	}
	member, err := r.workflowStep(ctx, completedTask.ID)
	if err != nil || member == nil {
		return err
	}
	if member.Workflow != meta.Workflow || member.Index != meta.StepIndex {
		return errors.New("workflow step does not match its registration")
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
	r.mu.Lock()
	b := r.bundle
	r.mu.Unlock()
	if b == nil {
		return nil
	}
	wf, ok := b.Workflows[meta.Workflow]
	if !ok || meta.StepIndex < 0 || meta.StepIndex >= len(wf.Spec.Steps) {
		return nil
	}
	step := wf.Spec.Steps[meta.StepIndex]
	if step.Workload != completedTask.WorkloadName {
		return nil
	}

	if step.Role == "verifier" || meta.Role == "verifier" {
		digest, err := r.verifierDigest(ctx, completedTask.ID, member.AuthorCommit)
		if err != nil {
			return err
		}
		verdict, findings := finalVerifierVerdict(completedTask.ArtifactsJSON, meta.Prompt, digest)
		if verdict == "" {
			_ = r.store.AppendAudit(ctx, domain.AuditRecord{
				Category: "workflow", TaskID: completedTask.ID, WorkloadName: completedTask.WorkloadName,
				Action: "workflow.verify", Outcome: "verdict_missing", CreatedAt: r.now(),
			})
			root, err := r.store.GetTask(ctx, member.RunID)
			if err != nil {
				return err
			}
			if err := r.store.UpdateTaskState(ctx, root.ID, root.State, root.State, "workflow_halted_verdict_missing", fmt.Sprintf(`{"verifier_task_id":%q}`, completedTask.ID)); err != nil {
				return err
			}
			rootStep.Status, rootStep.Reason = "halted", "verdict_missing"
			entry, err := r.workflowStepEntry(root.ID, *rootStep)
			if err != nil {
				return err
			}
			return r.store.PutMemory(ctx, entry)
		}
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
				handoff := fmt.Sprintf("# Verifier Feedback (CHANGES_REQUESTED)\n\nVerifier task: `%s`\n\n%s\n", completedTask.ID, findings)
				payload := map[string]any{
					"workflow":        meta.Workflow,
					"step":            authorStep.Name,
					"step_index":      meta.AuthorStepIndex,
					"prompt":          authorStep.Prompt,
					"input":           meta.Input,
					"role":            authorStep.Role,
					"max_retries":     meta.MaxRetries,
					"retry_count":     meta.RetryCount + 1,
					"auto_remediate":  true,
					"failure_handoff": handoff,
				}
				if authorStep.Tier != "" {
					payload["tier"] = authorStep.Tier
				}
				if prov != "" {
					payload["provider"] = prov
				}
				if mod != "" {
					payload["model"] = mod
				}
				raw, _ := json.Marshal(payload)
				idem := fmt.Sprintf("wf:%s:remediate:%s:%d", meta.Workflow, completedTask.ID, meta.RetryCount+1)
				remTask, err := r.submitTask(ctx, authorStep.Workload, idem, string(raw), completedTask.ID, &workflowStepRecord{RunID: member.RunID, Workflow: member.Workflow, Index: meta.AuthorStepIndex, AuthorCommit: member.AuthorCommit})
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
	prompt := nextStep.Prompt
	if nextStep.Role == "verifier" {
		prompt = contextpack.BuildVerifierPrompt(completedTask, nextStep.Prompt) +
			"Write your findings before the final verdict line.\n"
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
	next := workflowStepRecord{RunID: member.RunID, Workflow: member.Workflow, Index: nextIdx}
	if nextStep.Role == "verifier" {
		next.AuthorCommit = authorCommit(completedTask.ArtifactsJSON)
	}
	idem := fmt.Sprintf("wf:%s:%s:step:%d", meta.Workflow, completedTask.ID, nextIdx)
	child, err := r.submitTask(ctx, nextStep.Workload, idem, string(raw), completedTask.ID, &next)
	if err != nil {
		return err
	}
	now := r.now()
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

func (r *Reconciler) persistArtifacts(ctx context.Context, taskID string, adapter scruntime.Adapter, handle scruntime.Handle, assignment store.ConclaveAssignment, target domain.TaskState) error {
	callCtx, cancel := context.WithTimeout(ctx, adapterCallTimeout)
	arts, err := adapter.CollectArtifacts(callCtx, handle)
	if callCtx.Err() != nil {
		err = callCtx.Err()
	}
	cancel()
	if err != nil {
		return errors.New("runtime artifact collection failed")
	}
	if len(arts) == 0 {
		return nil
	}
	council := assignment.Round > 0
	var writerStore store.CouncilStore
	if council && slices.ContainsFunc(arts, func(a scruntime.Artifact) bool { return a.Name == "git-diff" }) {
		cs, err := store.Councils(r.store)
		if err != nil {
			return err
		}
		snap, err := cs.CouncilSnapshot(ctx, assignment.ConclaveID)
		if err != nil {
			return err
		}
		if snap.Session.ID != assignment.ConclaveID {
			return store.ErrConflict
		}
		if snap.Checkout.WriterRole == assignment.Role {
			writerStore = cs
		}
	}
	clean := make([]*scruntime.Artifact, 0, len(arts))
	for _, artifact := range arts {
		if writerStore != nil && artifact.Name == "git-diff" {
			if target != domain.TaskStateCompleted {
				continue
			}
			if !store.ValidCouncilText(artifact.Text) {
				artifact.Text = ""
			}
		}
		if artifact.Name == "final-message" && len(artifact.Text) > maxFinalMessage {
			artifact.Text = ""
		}
		if council {
			artifact.Text = store.RedactCouncilAnswer(artifact.Text)
		}
		clean = append(clean, &scruntime.Artifact{
			Name:        redact.String(artifact.Name),
			ContentType: redact.String(artifact.ContentType),
			URI:         redact.String(artifact.URI),
			Text:        redact.String(artifact.Text),
		})
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	if err := r.store.UpdateTaskArtifacts(ctx, taskID, string(raw)); err != nil || writerStore == nil || target != domain.TaskStateCompleted {
		return err
	}
	// Coding has durably recorded the zero exit. Stage the Relic while the
	// attempt accepts writes. Peer visibility still requires its linked answer.
	relic, valid := CouncilWriterDiff(assignment, taskID, clean)
	if !valid || relic == nil {
		return nil // The broker refuses to link an answer with malformed evidence.
	}
	relic.CreatedAt = r.now()
	return writerStore.PutCouncilRelic(ctx, taskID, *relic)
}

func newTaskID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "task_" + hex.EncodeToString(b[:]), nil
}
