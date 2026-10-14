//go:build linux

package calib

import (
	"syscall"
	"unsafe"
)

func init() { sweepPin = sweepPinLinux }

// sweepPinLinux pins the calling thread to cpu with sched_setaffinity(2),
// pid 0 being the calling thread. The mask is a bitmap of unsigned longs,
// one bit per CPU.
func sweepPinLinux(cpu int) error {
	mask := make([]uint64, cpu/64+1)
	mask[cpu/64] = 1 << (uint(cpu) % 64)
	_, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY, 0, uintptr(len(mask)*8), uintptr(unsafe.Pointer(&mask[0])))
	if errno != 0 {
		return errno
	}
	return nil
}
