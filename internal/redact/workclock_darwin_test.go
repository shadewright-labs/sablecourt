//go:build darwin

package redact

import (
	"syscall"
	"time"
)

// workClockName says what workClock measures, for failure messages.
const workClockName = "process CPU time"

// workClock returns the CPU time the test process has used. Darwin has no
// per-thread rusage; process CPU time still leaves out time spent waiting for
// a CPU behind other processes.
func workClock() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return wallClock()
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
