//go:build darwin

package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"syscall"
	"time"
	"unsafe"
)

const (
	darwinCtlKern      = 1
	darwinKernProc     = 14
	darwinKernProcPID  = 1
	darwinProcZombie   = 5
	darwinKinfoBufSize = 648
	darwinKinfoMinSize = 44
)

func processStartTime(pid int) (time.Time, error) {
	if pid <= 0 || pid > math.MaxInt32 {
		return time.Time{}, ErrProcessNotFound
	}
	mib := [4]int32{darwinCtlKern, darwinKernProc, darwinKernProcPID, int32(pid)}
	var buf [darwinKinfoBufSize]byte
	n := uintptr(len(buf))
	_, _, errno := syscall.Syscall6(
		syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])),
		uintptr(len(mib)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&n)),
		0,
		0,
	)
	if errno == syscall.ESRCH {
		return time.Time{}, ErrProcessNotFound
	}
	if errno != 0 {
		return time.Time{}, fmt.Errorf("query darwin process %d: %w", pid, errno)
	}
	if n == 0 {
		return time.Time{}, ErrProcessNotFound
	}
	if n < darwinKinfoMinSize {
		return time.Time{}, errors.New("incomplete darwin process record")
	}
	sec := int64(binary.LittleEndian.Uint64(buf[0:8]))
	usec := int32(binary.LittleEndian.Uint32(buf[8:12]))
	stat := buf[36]
	gotPID := int32(binary.LittleEndian.Uint32(buf[40:44]))
	if int(gotPID) != pid || stat == darwinProcZombie {
		return time.Time{}, ErrProcessNotFound
	}
	if sec <= 0 || usec < 0 || usec >= 1_000_000 {
		return time.Time{}, errors.New("invalid darwin process start time")
	}
	return time.Unix(sec, int64(usec)*1000).UTC(), nil
}
