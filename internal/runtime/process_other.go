//go:build !darwin && !linux

package runtime

import (
	"errors"
	"time"
)

func processStartTime(int) (time.Time, error) {
	return time.Time{}, errors.New("process start time lookup is unsupported on this platform")
}
