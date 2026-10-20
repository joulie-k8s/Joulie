// Package hwfusetest mounts the FUSE view of an emulated node for a test.
// A test that needs FUSE calls Require or Mount, which skip it, with the
// reason, on a host that cannot mount FUSE, unless JOULIE_REQUIRE_FUSE is
// 1, as CI sets it, which fails it instead.
package hwfusetest

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwfuse"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// EnvRequire set to 1 turns a skip for want of FUSE into a failure.
const EnvRequire = "JOULIE_REQUIRE_FUSE"

var (
	probeOnce sync.Once
	probeErr  error
)

// probe mounts an empty view once, the only reliable answer to whether this
// process can mount FUSE: /dev/fuse, fusermount3 for a user that is not
// root, CAP_SYS_ADMIN for root.
func probe() error {
	probeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hwfusetest-probe-")
		if err != nil {
			probeErr = err
			return
		}
		defer os.Remove(dir)
		s, err := hwfuse.Mount(dir, nil, hwfuse.Options{})
		if err != nil {
			probeErr = err
			return
		}
		probeErr = s.Unmount()
	})
	return probeErr
}

// Require skips t, or fails it under JOULIE_REQUIRE_FUSE=1, when this host
// cannot mount FUSE.
func Require(t testing.TB) {
	t.Helper()
	err := probe()
	if err == nil {
		return
	}
	if os.Getenv(EnvRequire) == "1" {
		t.Fatalf("FUSE is required (%s=1) and cannot be mounted here: %v", EnvRequire, err)
	}
	t.Skipf("FUSE cannot be mounted here, so this test of the FUSE view is skipped (%s=1 makes it fail): %v", EnvRequire, err)
}

// Mount mounts the view of node n, started with hwemu.Options.Live over
// tree, at a fresh directory of t, unmounted when t ends, with clock as the
// time of every call (time.Now when nil). It returns tree as an agent pod
// of the simulated cluster sees it: SysRoot and StateRoot in the view, the
// rest, cpuinfo, bin and dev/nvidiactl, on disk.
func Mount(t testing.TB, n *hwemu.Node, tree *hwemu.Tree, clock func() time.Time) *hwemu.Tree {
	t.Helper()
	Require(t)
	dir := t.TempDir()
	s, err := hwfuse.Mount(dir, []hwfuse.Node{{
		Name:    "node",
		Files:   tree.LiveFiles(),
		Backend: n,
		UID:     uint32(os.Geteuid()),
		GID:     uint32(os.Getegid()),
	}}, hwfuse.Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Unmount(); err != nil {
			t.Errorf("unmount the view: %v", err)
		}
	})
	view := *tree
	view.SysRoot = filepath.Join(dir, "node", layout.SysDir)
	view.StateRoot = filepath.Join(dir, "node", layout.StateDir)
	return &view
}
