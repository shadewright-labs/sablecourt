//go:build !linux && !darwin

package redact

import "time"

// workClockName says what workClock measures, for failure messages.
const workClockName = "wall time"

// workClock falls back to wall time where no CPU clock is wired up.
func workClock() time.Duration { return wallClock() }
