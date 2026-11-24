package coding

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

const processGracePeriod = time.Second

func processGroupRunning(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(-pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, fmt.Errorf("check coding process group: %w", err)
	}
}

func outputClosed(done <-chan struct{}) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func stopProcessGroup(ctx context.Context, pid int, done <-chan struct{}) error {
	if pid <= 0 {
		return nil
	}
	// Once shutdown starts, give each signal its full wait even if the request ends.
	ctx = context.WithoutCancel(ctx)
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := syscall.Kill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signal coding process group: %w", err)
		}
		wait, cancel := context.WithTimeout(ctx, processGracePeriod)
		err := waitForProcessGroup(wait, pid, done)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
	return errors.New("coding process group did not stop")
}

func waitForProcessGroup(ctx context.Context, pid int, done <-chan struct{}) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		running, err := processGroupRunning(pid)
		if err != nil {
			return err
		}
		if !running && outputClosed(done) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
