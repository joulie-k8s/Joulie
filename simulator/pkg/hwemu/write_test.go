package hwemu

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"testing"
)

// nodeBumpLastDigit returns a value of the same length as v, a decimal with a
// trailing newline, that differs from it in the last digit.
func nodeBumpLastDigit(v string) string {
	d := v[len(v)-2] - '0'
	return v[:len(v)-2] + strconv.Itoa(int(d+1)%10) + "\n"
}

// nodeSFileOf returns the S file rel of node n's tree with its path.
func nodeSFileOf(t *testing.T, n *Node, tr *Tree, rel string) (*nodeSFile, string) {
	t.Helper()
	abs := filepath.Join(tr.Root, filepath.FromSlash(rel))
	f := n.sfile(abs)
	if f == nil {
		t.Fatalf("%s is not an S file of the tree", rel)
	}
	return f, abs
}

// WS02: an S file that a reader holds open is replaced by rename, never
// written in place: the reader's open file keeps the old value whole, and a
// new open reads the new one. The write lease is refused while the file is
// open elsewhere, which is how the Node knows.
func TestWS02SFileOpenElsewhereIsReplacedByRename(t *testing.T) {
	nodeLeaseModes(t, func(t *testing.T, leases bool) {
		p := nodeIntelProfile()
		tr := nodeRender(t, p, nil)
		n := nodeNewNode(t, p, tr)
		f, abs := nodeSFileOf(t, n, tr, nodePolicyFile(0, "scaling_cur_freq"))
		old, inode := f.last, nodeInode(t, abs)
		r, err := os.Open(abs)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()

		want := nodeBumpLastDigit(old)
		if err := n.writeS(f, want); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		k, _ := r.ReadAt(buf, 0)
		if got := string(buf[:k]); got != old {
			t.Errorf("the open file reads %q after the write, want the old %q whole", got, old)
		}
		if got := nodeRead(t, tr, f.rel); got != want {
			t.Errorf("a new open reads %q, want %q", got, want)
		}
		if got := nodeInode(t, abs); got == inode {
			t.Errorf("inode %d kept, want a rename while the file is open", got)
		}
	})
}

// WS03: a value shorter than the file is replaced by rename, never written in
// place: an in-place write would need a truncate after it, and a process
// stopped between the two would leave the end of the old value behind.
// Without leases every S write goes by rename. The in-place cases need a
// file system that grants leases (nodeNeedLeases).
func TestWS03ShorterValueOrNoLeaseReplacesByRename(t *testing.T) {
	nodeLeaseModes(t, func(t *testing.T, leases bool) {
		if leases {
			nodeNeedLeases(t)
		}
		p := nodeIntelProfile()
		tr := nodeRender(t, p, nil)
		n := nodeNewNode(t, p, tr)
		f, abs := nodeSFileOf(t, n, tr, nodeZoneFile("energy_uj", 0))
		if err := n.writeS(f, "1000\n"); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			content string
			inPlace bool
		}{
			{"123456789012\n", leases},
			{"123456789099\n", leases},
			{"1234\n", false},
			{"98765\n", leases},
		} {
			inode, size := nodeInode(t, abs), len(f.last)
			if err := n.writeS(f, c.content); err != nil {
				t.Fatal(err)
			}
			if got := nodeRead(t, tr, f.rel); got != c.content {
				t.Errorf("after writing %q the file reads %q", c.content, got)
			}
			if kept := nodeInode(t, abs) == inode; kept != c.inPlace {
				t.Errorf("writing %q over %d bytes: inode kept %v, want %v", c.content, size, kept, c.inPlace)
			}
		}
	})
}

// WS04: an S file another writer changed, to a length the Node did not write,
// is replaced whole, never overwritten from offset 0, which would leave the
// rest of the other writer's bytes behind.
func TestWS04SFileChangedByAnotherWriterIsReplacedWhole(t *testing.T) {
	nodeLeaseModes(t, func(t *testing.T, leases bool) {
		p := nodeIntelProfile()
		tr := nodeRender(t, p, nil)
		n := nodeNewNode(t, p, tr)
		f, abs := nodeSFileOf(t, n, tr, nodePolicyFile(1, "scaling_cur_freq"))
		fi, err := os.Stat(abs)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(abs, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("99999999999999999\n"), 0); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(abs, fi.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
		want := nodeBumpLastDigit(f.last)
		if err := n.writeS(f, want); err != nil {
			t.Fatal(err)
		}
		if got := nodeRead(t, tr, f.rel); got != want {
			t.Errorf("the file reads %q, want %q whole", got, want)
		}
	})
}

// WS05: readers polling S files while the Node rewrites them read only values
// it wrote, whole: never a mix of two values, nor the end of an older one.
// The values grow and shrink in length, so both the in-place and the rename
// path run while the readers read.
func TestWS05ReadersSeeOnlyWholeValues(t *testing.T) {
	nodeLeaseModes(t, func(t *testing.T, leases bool) {
		p := nodeIntelProfile()
		tr := nodeRender(t, p, nil)
		n := nodeNewNode(t, p, tr)
		var files []*nodeSFile
		for _, rel := range []string{nodeZoneFile("energy_uj", 0), nodePolicyFile(0, "scaling_cur_freq"), nodePolicyFile(3, "scaling_cur_freq")} {
			f, _ := nodeSFileOf(t, n, tr, rel)
			files = append(files, f)
		}
		written := map[string]bool{}
		for _, f := range files {
			written[f.last] = true
		}
		values := make([]string, 0, 2000)
		for i := 0; i < 2000; i++ {
			// 1 to 4 leading zeros, then a counter: lengths go up and down.
			v := fmt.Sprintf("%0*d%d\n", 1+i%4, 0, 1000000+i)
			values = append(values, v)
			written[v] = true
		}

		whole := regexp.MustCompile(`^[0-9]+\n$`)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var problems []string
		reads := 0
		for r := 0; r < 4; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					for _, f := range files {
						b, err := os.ReadFile(f.path)
						s := string(b)
						mu.Lock()
						reads++
						if err != nil || !whole.MatchString(s) || !written[s] {
							problems = append(problems, fmt.Sprintf("%s read %q, %v", f.rel, s, err))
						}
						mu.Unlock()
					}
				}
			}()
		}
		for _, v := range values {
			for _, f := range files {
				if err := n.writeS(f, v); err != nil {
					t.Error(err)
				}
			}
		}
		close(stop)
		wg.Wait()
		if reads == 0 {
			t.Fatal("the readers never ran")
		}
		for i, pr := range problems {
			if i == 10 {
				t.Errorf("... %d more", len(problems)-10)
				break
			}
			t.Error(pr)
		}
	})
}
