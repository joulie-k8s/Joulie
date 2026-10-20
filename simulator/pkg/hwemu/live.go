package hwemu

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// A Live node (Options.Live) is the content of a live view of its tree, such
// as the FUSE mount of package hwfuse: the view hands every read and write of
// a file to LiveRead and LiveWrite, so a value is computed when it is read
// and a write is answered when it is made, as sysfs answers them.
//
//   - A read of an emulator-owned (S) or agent-writable (A) file first steps
//     the node to the time of the read, in substeps on the grid every Step
//     uses, so a counter such as energy_uj moves between two reads within a
//     tick, never goes back, and reads at a step's time what the step
//     computes. A read within LiveQuantum of the node's time is served at
//     the node's time.
//   - A write to an A file goes through the handler the readback runs
//     (applyWrite): a value the driver refuses fails with its errno and
//     changes nothing; any other takes effect at once, at the time of the
//     write. Both are events of the next step.
//   - A write to a writable file the emulator does not model, such as
//     intel_pstate/status or pp_dpm_sclk, is taken and changes nothing: the
//     file keeps the content the emulator gives it, with a restored event.
//   - Render-owned (R) files read as rendered; any other file of the layout,
//     such as state/hwemu.json, reads as it is on disk.
//
// The files on disk follow the node: a write is written through in place
// (normalizeAFiles), every S file a reader saw holds what it saw, and each
// Step writes the rest. A Node resumed over the tree after a restart then
// starts from limits the agent wrote and counters no lower than any reader
// saw.

// LiveQuantum is the resolution of a live read in node time: a read within
// it of the node's time does not step the node. RAPL's own counter moves
// about once a millisecond.
const LiveQuantum = time.Millisecond

// LiveAttrSize is the size a live view gives every regular file, the size
// sysfs gives an attribute whatever its content (kernfs, PAGE_SIZE). A
// write takes at most this many bytes, as a sysfs store does.
const LiveAttrSize = 4096

// liveMaxAhead bounds the step a live read may take. A view whose clock is
// further ahead of the node than this runs on another clock than the
// node's, such as a node started at a fixed time in a test, and would step
// the node through every substep in between.
const liveMaxAhead = 24 * time.Hour

// errNotLive is the error of a live call on a Node without Options.Live.
var errNotLive = errors.New("hwemu: the node was not started with Options.Live")

// liveIndex indexes the files of a Live node by path, once NewNode built
// the model. Each A file holds, as its last handled content, what the file
// on disk holds, so the first normalizeAFiles brings a file the model reads
// differently, such as a limit quantized to the power unit, to the model's
// readback.
func (n *Node) liveIndex() {
	n.liveA = make(map[string]*nodeAFile, len(n.afiles))
	for _, f := range n.afiles {
		n.liveA[f.rel] = f
		if b, err := n.readFile(f.path); err == nil && string(b) != f.held {
			f.held = string(b)
			n.afilesDirty = true
		}
	}
	n.liveR = make(map[string]*nodeRFile, len(n.rfiles))
	for _, r := range n.rfiles {
		n.liveR[r.rel] = r
	}
	n.liveComp = n.rfileComponents()
}

// LiveRead is read(2) of the file rel of the tree, a path relative to the
// node root such as sys/class/powercap/intel-rapl:0/energy_uj, through a
// live view at time at: its whole content, as a read from offset 0 returns
// it. rel names the file itself, not a link to it; the view resolves links.
// A path the layout does not list fails with ENOENT, a directory with
// EISDIR.
func (n *Node) LiveRead(rel string, at time.Time) ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.live {
		return nil, errNotLive
	}
	if r := n.liveR[rel]; r != nil {
		return append([]byte(nil), r.content...), nil
	}
	f, s := n.liveA[rel], n.liveS[rel]
	if f == nil && s == nil {
		if err := n.liveFile(rel, "read"); err != nil {
			return nil, err
		}
		return os.ReadFile(n.rootPath(rel))
	}
	if err := n.advanceLive(at); err != nil {
		return nil, err
	}
	if f != nil {
		return []byte(f.current()), nil
	}
	c, held := n.pendingS[s]
	if !held {
		return []byte(s.last), nil
	}
	// What a reader saw is on disk before the read returns, so a restart
	// never resumes a counter below it.
	if err := n.writeS(s, c); err != nil {
		return nil, &fs.PathError{Op: "read", Path: rel, Err: err}
	}
	return []byte(c), nil
}

// LiveWrite is one write(2) of data to the file rel of the tree through a
// live view at time at, as sysfs takes it: one store call per write,
// whatever the offset, of at most LiveAttrSize bytes, and it returns how
// many bytes it took. A refused value fails with a *fs.PathError that wraps
// the errno CheckWrite documents. A file without write permission fails
// with EACCES whoever writes, as a sysfs attribute does (kernfs_fop_open).
// A write of no bytes reaches no handler.
func (n *Node) LiveWrite(rel string, data []byte, at time.Time) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.live {
		return 0, errNotLive
	}
	if err := n.liveFile(rel, "write"); err != nil {
		return 0, err
	}
	if spec, ok := n.owners[rel]; ok && spec.Mode.Perm()&0o222 == 0 {
		return 0, &fs.PathError{Op: "write", Path: rel, Err: syscall.EACCES}
	}
	if len(data) > LiveAttrSize {
		data = data[:LiveAttrSize]
	}
	if len(data) == 0 {
		return 0, nil
	}
	if err := n.advanceLive(at); err != nil {
		return 0, err
	}
	value := string(data)
	if f := n.liveA[rel]; f != nil {
		kind, eff, errno := n.applyWrite(f, value)
		if errno != 0 {
			return 0, &fs.PathError{Op: "write", Path: rel, Err: errno}
		}
		n.pending = append(n.pending, Event{Time: n.now, File: f.rel, Kind: kind, Written: nodeEventValue(value), Effective: nodeEventValue(eff), Component: f.component})
		// An error stays in afilesDirty and is the next step's.
		_ = n.normalizeAFiles()
		return len(data), nil
	}
	shown := ""
	switch s, r := n.liveS[rel], n.liveR[rel]; {
	case s != nil:
		shown = s.last
		if c, ok := n.pendingS[s]; ok {
			shown = c
		}
	case r != nil:
		shown = string(r.content)
	default:
		// An agent-writable file no handler models keeps what was written.
		if err := nodeWriteInPlace(n.rootPath(rel), data, n.owners[rel].Mode.Perm()); err != nil {
			return 0, &fs.PathError{Op: "write", Path: rel, Err: errnoOf(err)}
		}
		return len(data), nil
	}
	n.pending = append(n.pending, Event{Time: n.now, File: rel, Kind: EventRestored, Written: nodeEventValue(value), Effective: nodeEventValue(shown), Component: n.liveComp[path.Dir(rel)]})
	return len(data), nil
}

// liveFile checks that rel is a regular file of the layout: ENOENT for a
// path it does not list, EISDIR for a directory.
func (n *Node) liveFile(rel, op string) error {
	spec, ok := n.owners[rel]
	switch {
	case !ok:
		return &fs.PathError{Op: op, Path: rel, Err: syscall.ENOENT}
	case spec.Kind == layout.KindDir:
		return &fs.PathError{Op: op, Path: rel, Err: syscall.EISDIR}
	case spec.Kind != layout.KindFile:
		return &fs.PathError{Op: op, Path: rel, Err: syscall.EINVAL}
	}
	return nil
}

// advanceLive steps the node to at for a live read or write, as Step does,
// but holds the S files it computes for the view to serve (pendingS) and
// leaves the events of the last Step to State: its own events wait for the
// next Step, which reports them.
func (n *Node) advanceLive(at time.Time) error {
	ahead := at.Sub(n.now)
	if ahead < LiveQuantum {
		return nil
	}
	if ahead > liveMaxAhead {
		return fmt.Errorf("hwemu: a live read at %v is %v ahead of the node's time %v: the view and the node run on different clocks: %w", at, ahead, n.now, syscall.EIO)
	}
	saved := n.events
	n.unobserved = true
	err := n.step(at)
	n.unobserved = false
	n.pending, n.events = n.events, saved
	return err
}

// errnoOf is the errno err wraps, EIO for an error that wraps none.
func errnoOf(err error) syscall.Errno {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	return syscall.EIO
}

// LiveFiles is the layout a live view of the node serves, as Render laid it
// out: every entry under sys/ and state/, except state's temp directory
// (layout.TmpDir), whose files come and go.
func (t *Tree) LiveFiles() []layout.FileSpec {
	tmp := path.Join(layout.StateDir, layout.TmpDir)
	var out []layout.FileSpec
	for _, f := range t.Files {
		top, _, _ := strings.Cut(f.Rel, "/")
		if top != layout.SysDir && top != layout.StateDir || f.Rel == tmp || strings.HasPrefix(f.Rel, tmp+"/") {
			continue
		}
		out = append(out, f)
	}
	return out
}
