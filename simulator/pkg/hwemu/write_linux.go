//go:build linux

package hwemu

import (
	"errors"
	"io"
	"syscall"
)

// nodeWriteFlags opens an S file for an in-place write. O_NOFOLLOW refuses a
// link put in the file's place, which the rename then replaces, and
// O_NONBLOCK an open that would wait, such as on a FIFO.
const nodeWriteFlags = syscall.O_WRONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC

// nodeNoFollow is added to every in-place open of a file the agent can
// write (an A file): the agent could put a link or a FIFO in its place, and
// the emulator must neither write through the link nor wait on the FIFO.
const nodeNoFollow = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// nodeWriteLeased writes content over the file at path, in one write at
// offset 0, and reports whether it did. It writes only under a write lease
// (F_SETLEASE, as nodeLease takes) and only while the file is a regular file
// of exactly len(last) bytes, as the Node last wrote it. Together these make
// a torn read impossible:
//
//   - the kernel grants the lease only while nobody else has the file open,
//     so no reader is part way through it and no mapping of it exists;
//   - while the lease is held, any other open waits until it is released, so
//     no reader starts during the write;
//   - content is at least as long as the file, so the one write covers every
//     old byte and needs no truncate. A value of a few bytes is copied in one
//     piece, so even a process killed during the write leaves the old value
//     or the new one, never an empty or a shortened file.
//
// A write that fails part way puts last back before the lease ends. Any
// failure, including a lease refused because a reader has the file open,
// reports false, and the caller replaces the file by rename instead.
//
// It uses system calls directly: os.OpenFile would add two nonblocking
// toggles and a netpoller registration per file, which a regular file never
// uses and which serialize parallel workers on the runtime's poll cache.
func nodeWriteLeased(path, last, content string) bool {
	fd, err := nodeOpenForWrite(path)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	if nodeSetLeaseFD(fd, syscall.F_WRLCK) != nil {
		return false
	}
	// Runs before the Close above: the lease ends, then the file closes.
	defer nodeSetLeaseFD(fd, syscall.F_UNLCK)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Size != int64(len(last)) {
		return false
	}
	if nodePwriteAll(fd, []byte(content)) != nil {
		if nodePwriteAll(fd, []byte(last)) == nil {
			_ = syscall.Ftruncate(fd, int64(len(last)))
		}
		return false
	}
	return true
}

// nodeOpenForWrite opens path with nodeWriteFlags. A file the caller may not
// write, such as a 0444 attribute for a caller that is not root, is made
// writable for the open when the caller owns it, as nodeOpenOwned does for A
// files; its mode is back before the write, which the open file descriptor
// keeps the right to make.
func nodeOpenForWrite(path string) (int, error) {
	fd, err := nodeOpenRetry(path)
	if !errors.Is(err, syscall.EACCES) {
		return fd, err
	}
	var st syscall.Stat_t
	if syscall.Lstat(path, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return -1, err
	}
	perm := st.Mode & 0o7777
	if syscall.Chmod(path, perm|0o200) != nil {
		return -1, err
	}
	fd, err = nodeOpenRetry(path)
	_ = syscall.Chmod(path, perm)
	return fd, err
}

func nodeOpenRetry(path string) (int, error) {
	for {
		fd, err := syscall.Open(path, nodeWriteFlags, 0)
		if err != syscall.EINTR {
			return fd, err
		}
	}
}

func nodePwriteAll(fd int, b []byte) error {
	for off := 0; off < len(b); {
		k, err := syscall.Pwrite(fd, b[off:], int64(off))
		switch {
		case err == syscall.EINTR:
			continue
		case err != nil:
			return err
		case k == 0:
			return io.ErrShortWrite
		}
		off += k
	}
	return nil
}

// nodeSetLeaseFD is nodeSetLease on a raw file descriptor.
func nodeSetLeaseFD(fd, typ int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETLEASE, uintptr(typ))
	if errno != 0 {
		return errno
	}
	return nil
}
