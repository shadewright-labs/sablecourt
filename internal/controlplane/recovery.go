package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

const (
	// Only the control plane writes runtime handle records in the reserved system namespace.
	runtimeHandlesNamespace = "system:runtime-handles"
	maxPersistedHandleBytes = 16 * 1024
	reasonLostOnRestart     = "lost_on_restart"
)

type persistedHandle struct {
	TaskID           string               `json:"task_id"`
	WorkloadClass    domain.WorkloadClass `json:"workload_class"`
	AdapterName      string               `json:"adapter_name"`
	ExternalID       string               `json:"external_id"`
	WorkDir          string               `json:"work_dir,omitempty"`
	Branch           string               `json:"branch,omitempty"`
	PID              int                  `json:"pid,omitempty"`
	ProcessStartTime string               `json:"process_start_time,omitempty"`
	LogPath          string               `json:"log_path,omitempty"`
	ContainerName    string               `json:"container_name,omitempty"`
	CreateAttempt    string               `json:"create_attempt,omitempty"`
	ContainerID      string               `json:"container_id,omitempty"`
	ProxyName        string               `json:"proxy_name,omitempty"`
	NetworkName      string               `json:"network_name,omitempty"`
}

func isContainerHandle(h scruntime.Handle) bool {
	return h.ContainerName != "" || h.ContainerID != ""
}

// Recover inspects active tasks after daemon startup, reattaches handles whose
// recorded PID and OS start time match a live process, and fails lost tasks
// with lost_on_restart while revoking their leases.
func (r *Reconciler) Recover(ctx context.Context) error {
	r.mu.Lock()
	ids := make([]string, 0, len(r.handles))
	for id := range r.handles {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	tasks, err := r.store.ListReconcileTasks(ctx, ids)
	if err != nil {
		return err
	}
	var passErr error
	for _, task := range tasks {
		if ctx.Err() != nil {
			return errors.Join(passErr, ctx.Err())
		}
		if terminalTaskState(task.State) || task.State == domain.TaskStateWorking || task.State == domain.TaskStateInputRequired {
			err := r.recoverOne(ctx, task)
			if !errors.Is(err, store.ErrConflict) {
				passErr = errors.Join(passErr, err)
			}
		}
	}
	return passErr
}

func (r *Reconciler) recoverOne(ctx context.Context, task domain.Task) error {
	r.reconcileMu.Lock()
	r.mu.Lock()
	terminal := terminalTaskState(task.State)
	_, attached := r.handles[task.ID]
	busy := r.recovering[task.ID] || r.launching[task.ID] != nil || (!terminal && (attached || r.partialStarts[task.ID]))
	if !busy {
		r.recovering[task.ID] = true
	}
	bundle := r.bundle
	r.mu.Unlock()
	r.reconcileMu.Unlock()
	if busy {
		return nil
	}
	defer func() {
		r.mu.Lock()
		delete(r.recovering, task.ID)
		r.mu.Unlock()
	}()
	if err := r.claimRecovery(ctx, task.ID); err != nil {
		return err
	}
	current, err := r.store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if terminalTaskState(current.State) {
		return r.reconcileTerminal(ctx, current)
	}
	if current.State != domain.TaskStateWorking && current.State != domain.TaskStateInputRequired {
		return nil
	}
	return r.recoverTask(ctx, current, bundle)
}

type recoveryOwner struct {
	Node     string
	PID      int
	Start    time.Time
	Instance string
}

// PID-backed workers and their owners are local to the recorded host.
func (r *Reconciler) claimRecovery(ctx context.Context, id string) error {
	entry, err := r.store.GetMemory(ctx, store.RecoveryOwners, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	node := fmt.Sprintf("%x", sha256.Sum256([]byte(host)))
	if entry.Value != "" {
		var old recoveryOwner
		if entry.Author != "controlplane" || json.Unmarshal([]byte(entry.Value), &old) != nil || old.Node != node || old.PID <= 0 || old.Start.IsZero() {
			return store.ErrConflict
		}
		if old.Instance == r.instance {
			return nil
		}
		alive, err := scruntime.ProcessAliveWithStartTime(old.PID, old.Start)
		if err != nil {
			return err
		}
		if alive {
			return store.ErrConflict
		}
	}
	start, err := scruntime.ProcessStartTime(os.Getpid())
	if err != nil {
		return err
	}
	owner, err := json.Marshal(recoveryOwner{node, os.Getpid(), start, r.instance})
	if err != nil {
		return err
	}
	won, err := r.store.ClaimRecovery(ctx, id, entry.Value, string(owner))
	if err != nil {
		return err
	}
	if !won {
		return store.ErrConflict
	}
	return nil
}

func (r *Reconciler) saveHandle(ctx context.Context, taskID string, class domain.WorkloadClass, handle scruntime.Handle) error {
	container := isContainerHandle(handle)
	if container {
		if handle.ContainerID == "" && (handle.ContainerName == "" || handle.CreateAttempt == "") {
			return errors.New("runtime handle has no durable container identity")
		}
	} else {
		if handle.PID <= 0 {
			return nil
		}
		if handle.ProcessStartTime.IsZero() {
			return errors.New("runtime handle has no launch-time process identity")
		}
	}
	rec := persistedHandle{
		TaskID:        taskID,
		WorkloadClass: class,
		AdapterName:   handle.AdapterName,
		ExternalID:    handle.ExternalID,
		WorkDir:       handle.WorkDir,
		Branch:        handle.Branch,
		PID:           handle.PID,
		LogPath:       handle.LogPath,
		ContainerName: handle.ContainerName,
		CreateAttempt: handle.CreateAttempt,
		ContainerID:   handle.ContainerID,
		ProxyName:     handle.ProxyName,
		NetworkName:   handle.NetworkName,
	}
	if !handle.ProcessStartTime.IsZero() {
		rec.ProcessStartTime = handle.ProcessStartTime.UTC().Format(time.RFC3339Nano)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if len(raw) > maxPersistedHandleBytes {
		return errors.New("persisted runtime handle is oversized")
	}
	return r.store.PutMemory(ctx, domain.MemoryEntry{
		Namespace: runtimeHandlesNamespace,
		Key:       taskID,
		Value:     string(raw),
		Author:    "controlplane",
		UpdatedAt: r.now(),
	})
}

func (r *Reconciler) deletePersistedHandle(ctx context.Context, taskID string) error {
	err := r.store.DeleteMemory(ctx, runtimeHandlesNamespace, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

func (r *Reconciler) loadPersistedHandle(ctx context.Context, taskID string) (*persistedHandle, scruntime.Handle, bool, error) {
	entry, err := r.store.GetMemory(ctx, runtimeHandlesNamespace, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, scruntime.Handle{}, false, nil
	}
	if err != nil {
		return nil, scruntime.Handle{}, false, err
	}
	if entry.Author != "controlplane" {
		return nil, scruntime.Handle{}, false, nil
	}
	trimmed := bytes.TrimSpace([]byte(entry.Value))
	if len(trimmed) == 0 || len(entry.Value) > maxPersistedHandleBytes || trimmed[0] != '{' {
		return nil, scruntime.Handle{}, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var rec persistedHandle
	if err := dec.Decode(&rec); err != nil {
		return nil, scruntime.Handle{}, false, nil
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, scruntime.Handle{}, false, nil
	}
	if rec.TaskID != taskID || rec.WorkloadClass == "" || rec.AdapterName == "" || rec.ExternalID == "" {
		return nil, scruntime.Handle{}, false, nil
	}
	container := rec.ContainerName != "" || rec.ContainerID != ""
	if container {
		if rec.ContainerID == "" && (rec.ContainerName == "" || rec.CreateAttempt == "") {
			return nil, scruntime.Handle{}, false, nil
		}
	} else if rec.PID <= 0 || rec.ProcessStartTime == "" {
		return nil, scruntime.Handle{}, false, nil
	}
	if (rec.WorkloadClass == domain.ClassCodingAgent || strings.HasPrefix(rec.AdapterName, "coding:")) && rec.ExternalID != taskID {
		return nil, scruntime.Handle{}, false, nil
	}
	for _, field := range []string{rec.AdapterName, rec.ExternalID, rec.WorkDir, rec.Branch, rec.LogPath, rec.ContainerName, rec.CreateAttempt, rec.ContainerID, rec.ProxyName, rec.NetworkName} {
		if strings.ContainsRune(field, 0) {
			return nil, scruntime.Handle{}, false, nil
		}
	}
	var startTime time.Time
	if rec.ProcessStartTime != "" {
		var err error
		startTime, err = time.Parse(time.RFC3339Nano, rec.ProcessStartTime)
		if err != nil || startTime.IsZero() {
			return nil, scruntime.Handle{}, false, nil
		}
		startTime = startTime.UTC()
	}
	handle := scruntime.Handle{
		AdapterName:      rec.AdapterName,
		ExternalID:       rec.ExternalID,
		WorkDir:          rec.WorkDir,
		Branch:           rec.Branch,
		PID:              rec.PID,
		ProcessStartTime: startTime,
		LogPath:          rec.LogPath,
		ContainerName:    rec.ContainerName,
		CreateAttempt:    rec.CreateAttempt,
		ContainerID:      rec.ContainerID,
		ProxyName:        rec.ProxyName,
		NetworkName:      rec.NetworkName,
	}
	return &rec, handle, true, nil
}

func (r *Reconciler) resolveRecoveryAdapter(task domain.Task, rec *persistedHandle, bundle *config.Bundle) scruntime.Adapter {
	var class domain.WorkloadClass
	if bundle != nil {
		if wl, ok := bundle.Workloads[task.WorkloadName]; ok {
			if rec != nil && rec.WorkloadClass != "" && rec.WorkloadClass != wl.Spec.Class {
				return nil
			}
			class = wl.Spec.Class
		}
	}
	if class == "" && rec != nil {
		class = rec.WorkloadClass
	}
	if class == "" {
		return nil
	}
	return r.adapters[class]
}

func (r *Reconciler) canRecoverHandle(ctx context.Context, adapter scruntime.Adapter, handle scruntime.Handle) (bool, error) {
	if adapter == nil {
		return false, nil
	}
	prober, ok := adapter.(scruntime.HandleLivenessProber)
	if !ok {
		return false, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, adapterCallTimeout)
	alive, err := prober.HandleAlive(callCtx, handle)
	if err == nil && !alive {
		if results, ok := adapter.(scruntime.HandleExitProber); ok {
			alive, err = results.HandleExited(callCtx, handle)
		}
	}
	if callCtx.Err() != nil {
		err = callCtx.Err()
	}
	cancel()
	return alive, err
}

func (r *Reconciler) isTaskStale(task domain.Task, now time.Time) bool {
	if task.State != domain.TaskStateWorking || r.stalenessWindow <= 0 {
		return false
	}
	var ref time.Time
	if task.LastHeartbeatAt != nil && !task.LastHeartbeatAt.IsZero() {
		ref = *task.LastHeartbeatAt
	} else if !task.UpdatedAt.IsZero() {
		ref = task.UpdatedAt
	} else if !task.CreatedAt.IsZero() {
		ref = task.CreatedAt
	} else {
		return true
	}
	return now.Sub(ref) > r.stalenessWindow
}

func (r *Reconciler) staleWithoutExit(ctx context.Context, task domain.Task, adapter scruntime.Adapter, h scruntime.Handle) (bool, error) {
	if !r.isTaskStale(task, r.now()) {
		return false, nil
	}
	results, ok := adapter.(scruntime.HandleExitProber)
	if !ok {
		return true, nil
	}
	exited, err := results.HandleExited(ctx, h)
	if err != nil {
		return false, errors.Join(err, r.store.RevokeTaskLeases(context.WithoutCancel(ctx), task.ID))
	}
	return !exited, nil
}

// persistedClassAdapter returns the adapter registered for the class stored with a handle.
func (r *Reconciler) persistedClassAdapter(rec *persistedHandle) scruntime.Adapter {
	if rec == nil || rec.WorkloadClass == "" {
		return nil
	}
	return r.adapters[rec.WorkloadClass]
}

// cleanupContainerHandle revokes the task lease and then stops the container handle.
func (r *Reconciler) cleanupContainerHandle(ctx context.Context, taskID string, adapter scruntime.Adapter, handle scruntime.Handle) error {
	cleanupCtx := context.WithoutCancel(ctx)
	if err := r.store.RevokeTaskLeases(cleanupCtx, taskID); err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(cleanupCtx, adapterCallTimeout)
	stopErr := adapter.Stop(callCtx, handle)
	if callCtx.Err() != nil {
		stopErr = callCtx.Err()
	}
	cancel()
	store.NoteEffectIf(ctx, stopErr, "the runtime of task %s was stopped", taskID)
	if stopErr != nil {
		return errors.New("runtime container cleanup failed")
	}
	return nil
}

// recoverTask attempts to reattach a task without an in-memory handle.
// Probe errors retain the record for a later pass. A definitive loss revokes
// access and fails the task without signaling the recorded PID.
func (r *Reconciler) recoverTask(ctx context.Context, task domain.Task, bundle *config.Bundle) error {
	rec, handle, valid, loadErr := r.loadPersistedHandle(ctx, task.ID)
	if loadErr != nil {
		return errors.Join(loadErr, r.store.RevokeTaskLeases(context.WithoutCancel(ctx), task.ID))
	}
	if !valid {
		return r.failLostTask(ctx, task, `{"reason":"handle_missing_or_invalid"}`)
	}
	adapter := r.resolveRecoveryAdapter(task, rec, bundle)
	if adapter == nil && isContainerHandle(handle) {
		// The manifest class changed, so the runtime cannot resume the turn. The
		// adapter that created the container still removes it and its proxy.
		if cleaner := r.persistedClassAdapter(rec); cleaner != nil {
			if err := r.cleanupContainerHandle(ctx, task.ID, cleaner, handle); err != nil {
				return err
			}
			return r.failLostTask(ctx, task, `{"reason":"workload_class_changed"}`)
		}
	}
	if adapter == nil {
		if isContainerHandle(handle) {
			return errors.Join(errors.New("council runtime unavailable for cleanup"), r.store.RevokeTaskLeases(context.WithoutCancel(ctx), task.ID))
		}
		return r.failLostTask(ctx, task, `{"reason":"adapter_unavailable"}`)
	}
	alive, probeErr := r.canRecoverHandle(ctx, adapter, handle)
	if probeErr != nil {
		return errors.Join(probeErr, r.store.RevokeTaskLeases(context.WithoutCancel(ctx), task.ID))
	}
	if isContainerHandle(handle) {
		// Per-launch listeners belong to the previous daemon. Revoke the lease
		// and remove its resources even when the final message is already durable.
		if err := r.cleanupContainerHandle(ctx, task.ID, adapter, handle); err != nil {
			return err
		}
		if !alive {
			return r.failLostTask(ctx, task, `{"reason":"container_not_alive"}`)
		}
	} else if !alive {
		return r.failLostTask(ctx, task, `{"reason":"process_not_alive"}`)
	}

	r.mu.Lock()
	if _, exists := r.handles[task.ID]; !exists {
		r.handles[task.ID] = handle
		r.handleAdapters[task.ID] = adapter
	}
	r.mu.Unlock()

	// Cancellation can finish while the startup probe is outside the lock.
	current, err := r.store.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if terminalTaskState(current.State) {
		return r.reconcileTerminal(ctx, current)
	}
	stale, err := r.staleWithoutExit(ctx, current, adapter, handle)
	if err != nil {
		return err
	}
	if stale {
		return r.failTask(ctx, current, reasonLostOnRestart, `{"reason":"stale_observation"}`)
	}
	return nil
}

// failLostTask fails a task with lost_on_restart without calling adapter.Stop,
// used when no verified live handle can be matched to the task.
func (r *Reconciler) failLostTask(ctx context.Context, task domain.Task, facts string) error {
	cleanupCtx := context.WithoutCancel(ctx)
	if err := r.failStep(cleanupCtx, task, reasonLostOnRestart, facts); err != nil {
		return err
	}
	// A command step lost on restart halts its run. Until the halt is
	// recorded its pending halt entry (failStep) and the persisted handle
	// stay, so recovery retries it.
	if err := r.haltCommandStep(cleanupCtx, task, nil); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.handles, task.ID)
	delete(r.handleAdapters, task.ID)
	r.mu.Unlock()
	cleanupErr := r.deletePersistedHandle(cleanupCtx, task.ID)
	if r.onTaskFailed != nil {
		r.onTaskFailed(task, reasonLostOnRestart)
	}
	return cleanupErr
}
