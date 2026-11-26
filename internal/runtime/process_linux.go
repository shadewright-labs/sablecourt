//go:build linux

package runtime

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	linuxClockTicksPerSec = 100
	maxProcStatBytes      = 64 * 1024
)

func processStartTime(pid int) (time.Time, error) {
	if pid <= 0 {
		return time.Time{}, ErrProcessNotFound
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return time.Time{}, ErrProcessNotFound
	}
	if err != nil {
		return time.Time{}, err
	}
	if len(data) > maxProcStatBytes {
		return time.Time{}, errors.New("oversized proc stat record")
	}
	closeParen := bytes.LastIndexByte(data, ')')
	if closeParen < 0 || closeParen+2 >= len(data) {
		return time.Time{}, errors.New("malformed proc stat record")
	}
	fields := strings.Fields(string(data[closeParen+2:]))
	if len(fields) < 20 {
		return time.Time{}, errors.New("incomplete proc stat record")
	}
	switch fields[0] {
	case "Z", "X", "x":
		return time.Time{}, ErrProcessNotFound
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse proc start ticks: %w", err)
	}
	btime, err := linuxBootTime()
	if err != nil {
		return time.Time{}, err
	}
	secs := btime + int64(startTicks/linuxClockTicksPerSec)
	nanos := int64(startTicks%linuxClockTicksPerSec) * (int64(time.Second) / linuxClockTicksPerSec)
	return time.Unix(secs, nanos).UTC(), nil
}

func linuxBootTime() (int64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, err
	}
	defer f.Close() //nolint:errcheck // read-only file; its close error cannot lose data
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			return strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("btime missing from /proc/stat")
}
