package hwemu

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// WS01: where the Node has leases, an emulator-owned (S) file that nobody
// holds open is rewritten in place: same inode, same mode, the new value
// whole. A temp file and a rename for every changed S file would create
// and free an inode per file per step: about 20000 of each per tick for 200
// two socket Intel nodes (96 scaling_cur_freq and 4 energy_uj each), which
// puts the BenchmarkEM01Tick intel-200 median at 1.4 s as root and 2.6 s
// as another user on a disk-backed (xfs) root.
//
// It does not call t.Parallel, because nodeNewNode reads nodeLeases, which
// other tests of the package toggle.
func TestWS01UnopenedSFileIsRewrittenInPlace(t *testing.T) {
	nodeNeedLeases(t)
	p := nodeIntelProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	if !n.leases {
		t.Fatal("the Node has no leases on a tree that grants them")
	}
	for _, rel := range []string{nodeZoneFile("energy_uj", 0), nodePolicyFile(0, "scaling_cur_freq")} {
		abs := filepath.Join(tr.Root, filepath.FromSlash(rel))
		before, err := os.Stat(abs)
		if err != nil {
			t.Fatal(err)
		}
		inode := nodeInode(t, abs)
		f := n.sfile(abs)
		if f == nil {
			t.Fatalf("%s is not an S file of the tree", rel)
		}
		want := nodeBumpLastDigit(f.last)
		if err := n.writeS(f, want); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		if got := nodeRead(t, tr, rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
		if got := nodeInode(t, abs); got != inode {
			t.Errorf("%s changed inode from %d to %d, want it rewritten in place", rel, inode, got)
		}
		after, err := os.Stat(abs)
		if err != nil {
			t.Fatal(err)
		}
		if after.Mode() != before.Mode() {
			t.Errorf("%s mode %v after the write, want %v", rel, after.Mode(), before.Mode())
		}
	}

	// A step that changes the readings keeps every S file's inode too.
	rel := nodeZoneFile("energy_uj", 0)
	abs := filepath.Join(tr.Root, filepath.FromSlash(rel))
	inode, before := nodeInode(t, abs), nodeRead(t, tr, rel)
	n.SetLoad(Load{CPUUtil: []float64{1}})
	nodeStep(t, n, time.Second)
	if got := nodeRead(t, tr, rel); got == before {
		t.Fatalf("%s still %q after a loaded second", rel, got)
	}
	if got := nodeInode(t, abs); got != inode {
		t.Errorf("%s changed inode from %d to %d in a step, want it rewritten in place", rel, inode, got)
	}
}

// TestAFileReplacedByALinkIsNotWrittenThrough: an agent that puts a symbolic
// link where one of its files was cannot make the emulator write into the
// link's target, neither by an in-place write nor by a readback rewrite.
// The old opens followed the link.
func TestAFileReplacedByALinkIsNotWrittenThrough(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "constraint_0_power_limit_uw")
	if err := os.Symlink(sentinel, link); err != nil {
		t.Fatal(err)
	}
	_ = nodeWriteInPlace(link, []byte("1\n"), 0o644)
	_, _ = nodeRewriteInPlace(link, nil, []byte("2\n"), time.Time{}, false)
	if got, _ := os.ReadFile(sentinel); string(got) != "keep\n" {
		t.Fatalf("the link target holds %q, want it untouched", got)
	}
}
