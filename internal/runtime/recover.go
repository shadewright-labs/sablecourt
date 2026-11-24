package runtime

import (
	"context"
	"errors"
	"time"
)

// ErrProcessNotFound reports that no live process exists for a PID.
var ErrProcessNotFound = errors.New("process not found")

// HandleLivenessProber is an optional Adapter capability that reports whether a
// recorded runtime handle still refers to the same live workload process.
// Implementations must fail closed when process identity cannot be verified.
type HandleLivenessProber interface {
	HandleAlive(ctx context.Context, handle Handle) (bool, error)
}

// ProcessStartTime returns the OS start timestamp for a live, non-zombie PID.
// It returns ErrProcessNotFound when the PID does not exist or is a zombie.
func ProcessStartTime(pid int) (time.Time, error) {
	if pid <= 0 {
		return time.Time{}, ErrProcessNotFound
	}
	return processStartTime(pid)
}

// ProcessAliveWithStartTime reports whether pid is currently alive (non-zombie)
// and started at expectedStart. It fails closed when pid <= 0 or expectedStart is zero.
func ProcessAliveWithStartTime(pid int, expectedStart time.Time) (bool, error) {
	if pid <= 0 || expectedStart.IsZero() {
		return false, nil
	}
	actual, err := processStartTime(pid)
	if errors.Is(err, ErrProcessNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return actual.UTC().Equal(expectedStart.UTC()), nil
}
