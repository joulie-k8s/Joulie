package hwemu

// A real driver refuses some values with an errno: amdgpu returns EINVAL or
// EOPNOTSUPP for a performance level its firmware does not take, and
// amd-pstate-epp EBUSY for a non-zero energy-performance preference under
// the performance governor. A file tree cannot fail write(2), so the write
// lands and the emulator reverts it at its next step; the agent then sees a
// readback that differs, where the real host would have seen the errno. The
// event of that step is EventRejected with the errno in Event.Errno, which
// telemetry reports as joulie.sim.write.rejected with error.type the errno
// in lower case.

// Errno names of a rejected write, the values of Event.Errno and of the
// errno a profile gives a refused value, such as gpus.dpm.levels.reject.
const (
	ErrnoEINVAL     = "EINVAL"
	ErrnoEOPNOTSUPP = "EOPNOTSUPP"
	ErrnoEBUSY      = "EBUSY"
)

// Errnos returns every errno name a rejected write may carry.
func Errnos() []string { return []string{ErrnoEINVAL, ErrnoEOPNOTSUPP, ErrnoEBUSY} }

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
