//go:build linux

package hwemu

import (
	"errors"
	"os"
	"syscall"
)

// nodeLease takes a write lease on f (fcntl(2) F_SETLEASE). Until release,
// any other open of the file, by the agent in its pod or by anyone else,
// waits, so a check and a write made under it cannot lose a concurrent write.
// The kernel refuses the lease while the file is open elsewhere: busy reports
// that, and the caller leaves the file for the next readback. Without a lease
// for another reason, such as a file system without leases or a caller that
// neither owns the file nor holds CAP_LEASE, ok is false and the caller
// writes unguarded.
func nodeLease(f *os.File) (release func(), busy, ok bool) {
	err := nodeSetLease(f, syscall.F_WRLCK)
	if err != nil {
		return func() {}, errors.Is(err, syscall.EAGAIN), false
	}
	return func() { _ = nodeSetLease(f, syscall.F_UNLCK) }, false, true
}

func nodeSetLease(f *os.File, typ int) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETLEASE, uintptr(typ))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}
