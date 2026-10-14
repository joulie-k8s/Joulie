//go:build linux

package hwemu

import (
	"io/fs"
	"syscall"
	"time"
)

// nodeStampMargin is how much older than the read a file's change time must
// be before the readback trusts an unchanged stamp to mean unchanged
// content: the kernel stamps files with a coarse clock, so a write landing
// right after a read can leave the change time the read saw, and a margin
// far above that clock's tick rules it out (the rule git uses for its
// index).
const nodeStampMargin = 100 * time.Millisecond

// nodeFileStamp is what fstat showed of an A file just before the readback
// last read it, and readAt a time no later than that read. Every write
// moves the change time, so a file whose stamp is the same, and whose
// change time is nodeStampMargin older than readAt, holds what that read
// found.
type nodeFileStamp struct {
	ok           bool
	dev, ino     uint64
	size         int64
	mtime, ctime syscall.Timespec
	readAt       time.Time
}

// unchanged reports whether path still has the stamp s recorded, so the
// readback can skip reading it: one lstat in place of an open, a read and a
// close.
func (s *nodeFileStamp) unchanged(path string) bool {
	if !s.ok {
		return false
	}
	var st syscall.Stat_t
	if syscall.Lstat(path, &st) != nil {
		return false
	}
	ct := time.Unix(st.Ctim.Unix())
	return uint64(st.Dev) == s.dev && st.Ino == s.ino && st.Size == s.size &&
		st.Mtim == s.mtime && st.Ctim == s.ctime && s.readAt.Sub(ct) > nodeStampMargin
}

// nodeReadA reads the A file at path into *buf, which it grows as needed,
// with system calls only: open (O_NOFOLLOW, O_NONBLOCK, so the runtime's
// poller is never tried on it), fstat, pread until a short read, close. It
// returns the content, which aliases *buf until the next call, and the
// modification time fstat showed before the read, and records the stamp in
// s with readAt, a time the caller took before the read. A missing file is
// an error that matches fs.ErrNotExist.
func nodeReadA(path string, buf *[]byte, s *nodeFileStamp, readAt time.Time) ([]byte, time.Time, error) {
	s.ok = false
	var fd int
	var err error
	for {
		fd, err = syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return nil, time.Time{}, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, time.Time{}, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	if need := int(st.Size) + 1; cap(*buf) < need {
		*buf = make([]byte, max(need, 512))
	}
	b := (*buf)[:cap(*buf)]
	n := 0
	for {
		k, err := syscall.Pread(fd, b[n:], int64(n))
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, time.Time{}, &fs.PathError{Op: "read", Path: path, Err: err}
		}
		n += k
		if k == 0 || n < len(b) {
			break
		}
		grown := make([]byte, 2*len(b))
		copy(grown, b)
		*buf, b = grown, grown
	}
	*s = nodeFileStamp{ok: true, dev: uint64(st.Dev), ino: st.Ino, size: st.Size, mtime: st.Mtim, ctime: st.Ctim, readAt: readAt}
	return b[:n], time.Unix(st.Mtim.Unix()), nil
}
