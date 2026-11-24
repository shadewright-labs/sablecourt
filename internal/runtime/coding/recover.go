package coding

import (
	"context"

	scruntime "github.com/shadewright-labs/sablecourt/internal/runtime"
)

var _ scruntime.HandleLivenessProber = (*Adapter)(nil)

// HandleAlive reports whether a recorded coding handle still refers to the same
// live process and valid worktree state. It fails closed when identity cannot be verified.
func (a *Adapter) HandleAlive(ctx context.Context, handle scruntime.Handle) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if a == nil || a.WorktreeRoot == "" || handle.PID <= 0 || handle.ProcessStartTime.IsZero() {
		return false, nil
	}
	if handle.LogPath != "" && handle.LogPath != handle.WorkDir+".log" {
		return false, nil
	}
	release, err := a.lockTask(ctx, handle.ExternalID, false)
	if err != nil {
		return false, err
	}
	defer release()
	state, err := a.worktreeState(handle)
	if err != nil {
		return false, nil
	}
	if !state.FinishedAt.IsZero() {
		return false, nil
	}
	return scruntime.ProcessAliveWithStartTime(handle.PID, handle.ProcessStartTime)
}
