package hwemu

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// A real driver refuses some values with an errno: amdgpu returns EINVAL or
// EOPNOTSUPP for a performance level its firmware does not take,
// amd-pstate-epp EBUSY for a non-zero energy-performance preference under
// the performance governor, and a powercap constraint ENODATA for a limit
// its op refuses (powercap_sys.c, store_constraint_*). A file tree cannot
// fail write(2), so the write lands and the emulator reverts it at its next
// step; until then the file reads back what was written, where the real
// host would have failed the write. The event of that step is
// EventRejected with the errno in Event.Errno, which telemetry reports as
// joulie.sim.write.rejected with error.type the errno in lower case.
//
// CheckWrite closes the gap for a writer that can call it: it answers each
// write with the errno, or applies it, at the moment of the write. The live
// view of a Live node (LiveWrite) closes it for every writer of the view.

// Errno names of a rejected write, the values of Event.Errno and of the
// errno a profile gives a refused value, such as gpus.dpm.levels.reject.
const (
	ErrnoEINVAL     = "EINVAL"
	ErrnoEOPNOTSUPP = "EOPNOTSUPP"
	ErrnoEBUSY      = "EBUSY"
	ErrnoENODATA    = "ENODATA"
)

// Errnos returns every errno name a rejected write may carry.
func Errnos() []string { return []string{ErrnoEINVAL, ErrnoEOPNOTSUPP, ErrnoEBUSY, ErrnoENODATA} }

// rejectErrnos are the system errnos of the Errno* names.
var rejectErrnos = map[string]syscall.Errno{
	ErrnoEINVAL:     syscall.EINVAL,
	ErrnoEOPNOTSUPP: syscall.EOPNOTSUPP,
	ErrnoEBUSY:      syscall.EBUSY,
	ErrnoENODATA:    syscall.ENODATA,
}

// rejectWrite is what the handler of an agent-writable file returns as its
// event kind when the real driver would fail the write to file (relative to
// the node root, as nodeAFile.rel) with errno: it records errno for file
// and returns EventRejected. The handler leaves the model unchanged, so the
// readback reverts the file, and the step's EventRejected for file gets
// errno (rejectAttach).
func (n *Node) rejectWrite(file, errno string) string {
	if n.rejectErrno == nil {
		n.rejectErrno = map[string]string{}
	}
	n.rejectErrno[file] = errno
	return EventRejected
}

// rejectAttach gives every EventRejected of the step the errno its handler
// recorded with rejectWrite, then forgets the records, so an errno recorded
// while a Node seeds its model from the tree (addAFile) never reaches a
// later event.
func (n *Node) rejectAttach() {
	for i := range n.events {
		if e := &n.events[i]; e.Kind == EventRejected && e.Errno == "" {
			e.Errno = n.rejectErrno[e.File]
		}
	}
	clear(n.rejectErrno)
}

// CheckWrite is write(2) of value to the file rel of the tree, a path
// relative to the node root that may pass through the tree's links, such
// as sys/bus/pci/devices/<bus>/power_dpm_force_performance_level, answered
// as the kernel answers it at this moment.
//
// An agent-writable file hands value, less the one trailing newline the
// kernel accepts, to the handler the readback runs. A value the driver
// refuses returns a *fs.PathError wrapping its errno and changes nothing:
// the errno its handler gives (rejectWrite), or EINVAL for a value that
// does not parse or is out of range, the kstrto* and range checks of the
// store functions (assumed: an overflow, which kstrto* fails with ERANGE,
// is EINVAL too). The refusal is an event of the next step, as the readback
// would have reported it. Any other value takes effect in the model at
// once, so the next CheckWrite is checked against it, as the kernel checks
// each write against the state the writes before it left. The file is the
// caller's to write: the next readback finds the value in effect and
// normalizes the file.
//
// A read-only file, such as cpuinfo_max_freq or energy_uj, fails with
// EACCES, as a read-only attribute does even for root (kernfs_fop_open); a
// directory with EISDIR and a missing file with ENOENT. A writable file the
// emulator does not model, such as intel_pstate/status, takes the write,
// which the next render check restores. An empty value is a write of no
// bytes, which sysfs takes without calling the driver (sysfs_kf_write).
func (n *Node) CheckWrite(rel, value string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	abs := n.rootPath(rel)
	fail := func(errno syscall.Errno) error { return &fs.PathError{Op: "write", Path: abs, Err: errno} }
	real := abs
	if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		real = filepath.Join(dir, filepath.Base(abs))
	}
	fi, err := os.Lstat(real)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fail(syscall.ENOENT)
	case err != nil:
		return &fs.PathError{Op: "write", Path: abs, Err: err}
	case fi.IsDir():
		return fail(syscall.EISDIR)
	}
	f := n.afileAt(n.relPath(real))
	if f == nil {
		if fi.Mode().Perm()&0o222 == 0 {
			return fail(syscall.EACCES)
		}
		return nil
	}
	if value == "" {
		return nil
	}
	if _, _, errno := n.applyWrite(f, value); errno != 0 {
		return fail(errno)
	}
	return nil
}

// applyWrite is a non-empty write of value to the A file f, with n.mu held:
// value, less the one trailing newline the kernel accepts, goes to the
// handler the readback runs. A value the driver refuses changes nothing and
// returns the errno CheckWrite documents, with the event the next step
// reports. Any other value takes effect in the model at once and returns
// the handler's event kind and the value in effect.
func (n *Node) applyWrite(f *nodeAFile, value string) (kind, eff string, errno syscall.Errno) {
	kind, eff = f.handle(strings.TrimSuffix(value, "\n"))
	n.afilesDirty = true
	switch kind {
	case EventRejected:
		name := n.rejectErrno[f.rel]
		delete(n.rejectErrno, f.rel)
		if errno = rejectErrnos[name]; errno == 0 {
			errno = syscall.EIO
		}
		n.pending = append(n.pending, Event{Time: n.now, File: f.rel, Kind: kind, Written: nodeEventValue(value), Effective: nodeEventValue(eff), Component: f.component, Errno: name})
		return kind, eff, errno
	case EventParseError, EventRevertedOutOfRange:
		n.pending = append(n.pending, Event{Time: n.now, File: f.rel, Kind: kind, Written: nodeEventValue(value), Effective: nodeEventValue(eff), Component: f.component})
		return kind, eff, syscall.EINVAL
	}
	n.derive()
	n.adopt(n.now)
	return kind, eff, 0
}

// afileAt is the A file at rel, relative to the node root, or nil.
func (n *Node) afileAt(rel string) *nodeAFile {
	for _, f := range n.afiles {
		if f.rel == rel {
			return f
		}
	}
	return nil
}
