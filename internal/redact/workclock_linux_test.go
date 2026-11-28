//go:build linux

package redact

import (
	"syscall"
	"time"
	"unsafe"
)

// clockThreadCPUTime is CLOCK_THREAD_CPUTIME_ID, which the syscall package
// does not name.
const clockThreadCPUTime = 3

// workClockName says what workClock measures, for failure messages.
const workClockName = "thread CPU time"

// workClock returns the CPU time the calling OS thread has used, from
// clock_gettime with nanosecond resolution (getrusage counts in scheduler
// ticks, too coarse for one call). The caller locks its goroutine to the
// thread. Time the thread spends waiting for a CPU is not counted, so other
// processes and goroutines on a loaded machine do not add to it.
func workClock() time.Duration {
	var ts syscall.Timespec
	if _, _, errno := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, clockThreadCPUTime, uintptr(unsafe.Pointer(&ts)), 0); errno != 0 {
		return wallClock()
	}
	return time.Duration(ts.Nano())
}
