package hwemu

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// WS01: where the Node has leases, an emulator-owned (S) file that nobody
// holds open is rewritten in place: same inode, same mode, the new value
// whole. Before the fix every changed S file went through a new temp file
// and a rename, an inode created and one freed per file per step: about
// 20000 of each per tick for 200 two socket Intel nodes (96 scaling_cur_freq
// and 4 energy_uj each), which put the BenchmarkEM01Tick intel-200 median at
// 1.4 s as root and 2.6 s as another user on a disk-backed (xfs) root.
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
