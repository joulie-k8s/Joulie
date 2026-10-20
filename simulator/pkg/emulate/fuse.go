package emulate

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/hwfuse"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// FUSEDir is the directory of the root under which FUSE mode serves each
// member's view.
const FUSEDir = "fuse"

// ViewDir is where FUSE mode serves the view of member node:
// <root>/FUSEDir/<node>, holding sys and state as the node's tree lays them
// out, the paths an emulated agent mounts for /host-sys and /emu/state.
// cpuinfo, bin and dev/nvidiactl stay in the tree itself.
func ViewDir(root, node string) string { return filepath.Join(root, FUSEDir, node) }

// viewDirs are the entries of a tree a view serves, as hwemu.Tree.LiveFiles
// lists them.
var viewDirs = []string{layout.SysDir, layout.StateDir}

// views is the FUSE mount of FUSE mode. As root, which the simulator's
// container is, the file system is mounted in a fresh directory of the
// process (base), and each view is a bind mount of its node's directory at
// ViewDir, never unmounted: a new process binds its views over the old
// ones, which reaches the agent containers that mount a view with mount
// propagation HostToContainer, so they never need a restart. As another
// user, which has no bind mounts, the file system is mounted at
// <root>/FUSEDir itself, which serves the same paths, and unmounted on
// close.
type views struct {
	srv   *hwfuse.Server
	base  string // the private mount point, as root
	root  string
	nodes int
}

// mountViews mounts the views of members, whose runners must serve a live
// view (*hwemu.Node started with Options.Live), on clock.
func mountViews(root string, members []*member, clock func() time.Time) (*views, error) {
	nodes := make([]hwfuse.Node, 0, len(members))
	for _, m := range members {
		b, ok := m.runner.(hwfuse.Backend)
		if !ok || m.tree == nil {
			return nil, fmt.Errorf("emulate: FUSE mode: node %s does not serve a live view", m.spec.Name)
		}
		uid, gid := viewOwner(m.tree.Root)
		nodes = append(nodes, hwfuse.Node{Name: m.spec.Name, Files: m.tree.LiveFiles(), Backend: b, UID: uid, GID: gid})
	}
	v := &views{root: root, nodes: len(nodes)}
	opts := hwfuse.Options{Clock: clock}
	if os.Geteuid() != 0 {
		dir := filepath.Join(root, FUSEDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("emulate: FUSE mode: %w", err)
		}
		srv, err := hwfuse.Mount(dir, nodes, opts)
		if err != nil {
			return nil, fmt.Errorf("emulate: FUSE mode: %w", err)
		}
		v.srv = srv
		return v, nil
	}
	base, err := os.MkdirTemp("", "joulie-hwemu-fuse-")
	if err != nil {
		return nil, fmt.Errorf("emulate: FUSE mode: %w", err)
	}
	srv, err := hwfuse.Mount(base, nodes, opts)
	if err != nil {
		_ = os.Remove(base)
		return nil, fmt.Errorf("emulate: FUSE mode: %w", err)
	}
	v.srv, v.base = srv, base
	for _, n := range nodes {
		for _, d := range viewDirs {
			if err := hwfuse.Bind(filepath.Join(base, n.Name, d), filepath.Join(ViewDir(root, n.Name), d)); err != nil {
				return nil, fmt.Errorf("emulate: FUSE mode: %w", err)
			}
		}
	}
	return v, nil
}

// viewOwner is the owner of a tree's root, the owner every entry of its view
// shows: root in the simulator's container.
func viewOwner(dir string) (uid, gid uint32) {
	uid, gid = uint32(os.Geteuid()), uint32(os.Getegid())
	if fi, err := os.Stat(dir); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = st.Uid, st.Gid
		}
	}
	return uid, gid
}

func (v *views) String() string {
	if v.base != "" {
		return fmt.Sprintf("serving %d nodes through FUSE: %s, bound at %s/<node>/{sys,state}", v.nodes, v.base, filepath.Join(v.root, FUSEDir))
	}
	return fmt.Sprintf("serving %d nodes through FUSE at %s/<node>/{sys,state}", v.nodes, filepath.Join(v.root, FUSEDir))
}

// close unmounts the views a user other than root mounted. The bind mounts
// of root stay, served until the process ends, so the views of the next
// process go over them (views).
func (v *views) close() error {
	if v == nil || v.base != "" {
		return nil
	}
	return v.srv.Unmount()
}
