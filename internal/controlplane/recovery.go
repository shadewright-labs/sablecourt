package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
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
	PID              int                  `json:"pid"`
	ProcessStartTime string               `json:"process_start_time,omitempty"`
	LogPath          string               `json:"log_path,omitempty"`
}

// Recover inspects active tasks after daemon startup, reattaches handles whose
// recorded PID and OS start time match a live process, and fails lost tasks
// with lost_on_restart while revoking their leases.
func (r *Reconciler) Recover(ctx context.Context) error {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()

	r.mu.Lock()
	launching := maps.Clone(r.launching)
	handleIDs := slices.Collect(maps.Keys(r.handles))
	bundle := r.bundle
	r.mu.Unlock()

	tasks, err := r.store.ListReconcileTasks(ctx, handleIDs)
	if err != nil {
		return err
	}

	var passErr error
	for _, task := range tasks {
		if ctx.Err() != nil {
			return errors.Join(passErr, ctx.Err())
		}
		if launching[task.ID] != nil {
			continue
		}
		if terminalTaskState(task.State) {
			passErr = errors.Join(passErr, r.reconcileTerminal(context.WithoutCancel(ctx), task))
			continue
		}
		if task.State != domain.TaskStateWorking && task.State != domain.TaskStateInputRequired {
			continue
		}
		r.mu.Lock()
		_, hasHandle := r.handles[task.ID]
		partialStart := r.partialStarts[task.ID]
		r.mu.Unlock()
		if hasHandle || partialStart {
			continue
		}
		if _, recErr := r.recoverTask(ctx, task, bundle); recErr != nil && !errors.Is(recErr, store.ErrConflict) {
			passErr = errors.Join(passErr, recErr)
		}
	}
	return passErr
}

func enrichHandle(handle scruntime.Handle) scruntime.Handle {
	if handle.PID > 0 && handle.ProcessStartTime.IsZero() {
		if start, err := scruntime.ProcessStartTime(handle.PID); err == nil {
			handle.ProcessStartTime = start
		}
	}
	if handle.LogPath == "" && handle.WorkDir != "" && strings.HasPrefix(handle.AdapterName, "coding:") {
		handle.LogPath = handle.WorkDir + ".log"
	}
	return handle
}

func (r *Reconciler) saveHandle(ctx context.Context, taskID string, class domain.WorkloadClass, handle scruntime.Handle) error {
	if handle.PID <= 0 {
		return nil
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
	if rec.TaskID != taskID || rec.WorkloadClass == "" || rec.AdapterName == "" || rec.ExternalID == "" || rec.PID <= 0 || rec.ProcessStartTime == "" {
		return nil, scruntime.Handle{}, false, nil
	}
	for _, field := range []string{rec.AdapterName, rec.ExternalID, rec.WorkDir, rec.Branch, rec.LogPath} {
		if strings.ContainsRune(field, 0) {
			return nil, scruntime.Handle{}, false, nil
		}
	}
	startTime, err := time.Parse(time.RFC3339Nano, rec.ProcessStartTime)
	if err != nil || startTime.IsZero() {
		return nil, scruntime.Handle{}, false, nil
	}
	handle := scruntime.Handle{
		AdapterName:      rec.AdapterName,
		ExternalID:       rec.ExternalID,
		WorkDir:          rec.WorkDir,
		Branch:           rec.Branch,
		PID:              rec.PID,
		ProcessStartTime: startTime.UTC(),
		LogPath:          rec.LogPath,
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

func (r *Reconciler) probeHandleAlive(ctx context.Context, adapter scruntime.Adapter, handle scruntime.Handle) (bool, error) {
	if adapter == nil {
		return false, nil
	}
	prober, ok := adapter.(scruntime.HandleLivenessProber)
	if !ok {
		return false, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, adapterCallTimeout)
	alive, err := prober.HandleAlive(callCtx, handle)
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

// recoverTask attempts to reattach a task without an in-memory handle.
// If identity cannot be verified, it fails the task with lost_on_restart
// without signaling an unverified or reused PID.
func (r *Reconciler) recoverTask(ctx context.Context, task domain.Task, bundle *config.Bundle) (bool, error) {
	rec, handle, valid, loadErr := r.loadPersistedHandle(ctx, task.ID)
	if loadErr != nil {
		return false, loadErr
	}
	if !valid {
		return false, r.failLostTask(ctx, task, `{"reason":"handle_missing_or_invalid"}`)
	}
	adapter := r.resolveRecoveryAdapter(task, rec, bundle)
	if adapter == nil {
		return false, r.failLostTask(ctx, task, `{"reason":"adapter_unavailable"}`)
	}
	alive, probeErr := r.probeHandleAlive(ctx, adapter, handle)
	if probeErr != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	if probeErr != nil || !alive {
		return false, r.failLostTask(ctx, task, `{"reason":"process_not_alive"}`)
	}

	r.mu.Lock()
	if _, exists := r.handles[task.ID]; !exists {
		r.handles[task.ID] = handle
		r.handleAdapters[task.ID] = adapter
	}
	r.mu.Unlock()

	if r.isTaskStale(task, r.now()) {
		return false, r.failTask(ctx, task, reasonLostOnRestart, `{"reason":"stale_observation"}`)
	}
	return true, nil
}

// failLostTask fails a task with lost_on_restart without calling adapter.Stop,
// used when no verified live handle can be matched to the task.
func (r *Reconciler) failLostTask(ctx context.Context, task domain.Task, facts string) error {
	cleanupCtx := context.WithoutCancel(ctx)
	if err := r.store.UpdateTaskState(cleanupCtx, task.ID, task.State, domain.TaskStateFailed, reasonLostOnRestart, facts); err != nil {
		return err
	}
	cleanupErr := errors.Join(
		r.store.RevokeTaskLeases(cleanupCtx, task.ID),
		r.deletePersistedHandle(cleanupCtx, task.ID),
	)
	if r.onTaskFailed != nil {
		r.onTaskFailed(task, reasonLostOnRestart)
	}
	return cleanupErr
}
