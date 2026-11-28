package docker

import (
	"context"
	"errors"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

func verifiedExit(st *container.State) (time.Time, error) {
	if st == nil || st.Status != "exited" || st.Running || st.Paused || st.Restarting || st.Dead || st.ExitCode < 0 {
		return time.Time{}, errors.New("missing terminal container state")
	}
	t, err := time.Parse(time.RFC3339Nano, st.FinishedAt)
	if err != nil || t.IsZero() {
		return time.Time{}, errors.New("missing container exit timestamp")
	}
	return t, nil
}

func (a *Adapter) recoveryState(ctx context.Context, h rt.Handle) (rt.ObservedState, error) {
	if h.AdapterName != "docker" || h.ExternalID == "" {
		return rt.StateUnknown, errors.New("invalid Docker recovery handle")
	}
	r, err := a.engineClient().ContainerInspect(ctx, h.ExternalID, client.ContainerInspectOptions{})
	if isMissing(err) {
		return rt.StateUnknown, nil
	}
	if err != nil {
		return rt.StateUnknown, err
	}
	st := r.Container.State
	if r.Container.ID != h.ExternalID || st == nil {
		return rt.StateUnknown, errors.New("unverified container identity or state")
	}
	if st.Running && !st.Restarting && !st.Dead {
		if st.Status == "paused" && st.Paused {
			return rt.StateSuspended, nil
		}
		if st.Status == "running" && !st.Paused {
			return rt.StateRunning, nil
		}
	}
	if _, err := verifiedExit(st); err != nil {
		return rt.StateUnknown, err
	}
	if st.ExitCode != 0 {
		return rt.StateFailed, nil
	}
	return rt.StateCompleted, nil
}

func (a *Adapter) HandleAlive(ctx context.Context, h rt.Handle) (bool, error) {
	state, err := a.recoveryState(ctx, h)
	return state == rt.StateRunning || state == rt.StateSuspended, err
}

func (a *Adapter) HandleExited(ctx context.Context, h rt.Handle) (bool, error) {
	state, err := a.recoveryState(ctx, h)
	return state == rt.StateCompleted || state == rt.StateFailed, err
}
