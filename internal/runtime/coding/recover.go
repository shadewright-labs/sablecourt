package coding

import (
	"context"
	"errors"
	"os"

	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

var _ scruntime.HandleLivenessProber = (*Adapter)(nil)

// HandleAlive reports whether a recorded coding handle still refers to the same
// live process and valid worktree state. It fails closed when identity cannot be verified.
func (a *Adapter) HandleAlive(ctx context.Context, handle scruntime.Handle) (bool, error) {
	return a.probe(ctx, handle, false)
}

func (a *Adapter) HandleExited(ctx context.Context, handle scruntime.Handle) (bool, error) {
	return a.probe(ctx, handle, true)
}

func (a *Adapter) probe(ctx context.Context, handle scruntime.Handle, exit bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if a == nil || a.WorktreeRoot == "" {
		return false, nil
	}
	if isContainerHandle(handle) {
		return a.probeContainer(ctx, handle, exit)
	}
	if handle.PID <= 0 || handle.ProcessStartTime.IsZero() {
		return false, nil
	}
	taskBase, err := a.taskBase(handle.ExternalID)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		return false, nil
	}
	if handle.LogPath != "" && handle.LogPath != taskBase+".log" {
		return false, nil
	}
	release, err := a.lockTask(ctx, handle.ExternalID, false)
	if err != nil {
		return false, err
	}
	defer release()
	state, err := a.worktreeState(handle)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		return false, nil
	}
	if exit {
		if state.Result != nil && state.Result.PID == handle.PID && state.Result.Start.Equal(handle.ProcessStartTime) {
			return true, nil
		}
		result, err := recordedExitAt(taskBase, handle)
		return result != nil, err
	}
	if !state.FinishedAt.IsZero() {
		return false, nil
	}
	return scruntime.ProcessAliveWithStartTime(handle.PID, handle.ProcessStartTime)
}
