package hwemu

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

func nodeInode(t *testing.T, abs string) uint64 {
	t.Helper()
	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// H14: A files keep their inode however often they are normalized, and a
// reader polling S files never sees one empty or missing, nor a temp file
// next to an attribute, while the emulator rewrites them.
//
// It does not call t.Parallel, because it toggles nodeLeases, which every
// NewNode reads; Go runs it before any parallel test starts.
func TestH14WritesNeverExposeAnEmptyFile(t *testing.T) {
	p := nodeNVLProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	abs := func(rel string) string { return filepath.Join(tr.Root, filepath.FromSlash(rel)) }

	sfiles := []string{
		nodeZoneFile("energy_uj", 0),
		nodeZoneFile("energy_uj", 1),
		nodeZoneFile("energy_uj", 0, 0),
		nodePolicyFile(0, "scaling_cur_freq"),
		nodeNVMLFile(0, "power_draw_mw"),
		nodeNVMLFile(0, "total_energy_mj"),
	}
	afiles := []string{
		nodeZoneFile("constraint_0_power_limit_uw", 0),
		nodePolicyFile(0, "scaling_max_freq"),
		nodeNVMLFile(0, "power_limit_mw"),
	}
	inodes := map[string]uint64{}
	for _, rel := range afiles {
		inodes[rel] = nodeInode(t, abs(rel))
	}
	dirs := []string{
		path.Join(layout.SysDir, layout.PowercapZoneDir("intel-rapl", 0)),
		path.Join(layout.SysDir, layout.PolicyDir(0)),
		path.Join(layout.StateDir, layout.NVMLGPUDir(0)),
	}
	listing := map[string]map[string]bool{}
	for _, d := range dirs {
		entries, err := os.ReadDir(abs(d))
		if err != nil {
			t.Fatal(err)
		}
		listing[d] = map[string]bool{}
		for _, e := range entries {
			listing[d][e.Name()] = true
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var problems []string
	reads := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, rel := range sfiles {
				b, err := os.ReadFile(abs(rel))
				if err != nil || len(b) == 0 {
					mu.Lock()
					problems = append(problems, fmt.Sprintf("%s read %q, %v", rel, b, err))
					mu.Unlock()
				}
			}
			for _, d := range dirs {
				entries, _ := os.ReadDir(abs(d))
				for _, e := range entries {
					if !listing[d][e.Name()] {
						mu.Lock()
						problems = append(problems, fmt.Sprintf("%s showed %s", d, e.Name()))
						mu.Unlock()
					}
				}
			}
			mu.Lock()
			reads++
			mu.Unlock()
		}
	}()

	for i := 0; i < 800; i++ {
		n.SetLoad(Load{CPUUtil: []float64{0.3 + 0.7*float64(i%2)}, GPUUtil: []float64{0.5 + 0.5*float64(i%2)}})
		if i%50 == 0 {
			nodeWrite(t, tr, afiles[0], fmt.Sprint(120000001+i))
			nodeWrite(t, tr, afiles[1], fmt.Sprint(2000000+i))
			nodeWrite(t, tr, afiles[2], fmt.Sprint(250000+i))
		}
		nodeStep(t, n, 10*time.Millisecond)
	}
	close(stop)
	wg.Wait()

	if reads == 0 {
		t.Fatal("the reader never ran")
	}
	for i, pr := range problems {
		if i == 10 {
			t.Errorf("... %d more", len(problems)-10)
			break
		}
		t.Error(pr)
	}
	for _, rel := range afiles {
		if got := nodeInode(t, abs(rel)); got != inodes[rel] {
			t.Errorf("%s changed inode from %d to %d", rel, inodes[rel], got)
		}
		if b := nodeRead(t, tr, rel); len(b) == 0 || b[len(b)-1] != '\n' {
			t.Errorf("%s = %q, want a normalized value", rel, b)
		}
	}
}

// nodeAFileOf returns the readback record of the agent-writable file rel.
func nodeAFileOf(t *testing.T, n *Node, rel string) *nodeAFile {
	t.Helper()
	for _, f := range n.afiles {
		if f.rel == rel {
			return f
		}
	}
	t.Fatalf("%s is not an agent-writable file of the node", rel)
	return nil
}

// nodeLeaseFileSystems are the Linux file systems that grant leases through
// generic_setlease (fs/locks.c), having no setlease of their own: ext4, xfs,
// btrfs and tmpfs (include/uapi/linux/magic.h).
var nodeLeaseFileSystems = []int64{0xef53, 0x58465342, 0x9123683e, 0x01021994}

// nodeNeedLeases skips a test that relies on leases where the platform or the
// test's file system has none: there the rewrite keeps the window readAFile
// describes. On a Linux file system that grants leases, with leases enabled,
// a failed probe is a bug and fails the test.
func nodeNeedLeases(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if nodeLeasesWork(dir) {
		return
	}
	var st syscall.Statfs_t
	enabled, _ := os.ReadFile("/proc/sys/fs/leases-enable")
	if runtime.GOOS == "linux" && string(enabled) == "1\n" && syscall.Statfs(dir, &st) == nil && slices.Contains(nodeLeaseFileSystems, int64(st.Type)) {
		t.Fatalf("no write lease on a file system of type %#x, which grants them", st.Type)
	}
	t.Skip("no file leases on this platform or file system")
}

// nodeLeaseModes runs fn with leases, as on Linux, and without, the
// unguarded path a Node takes where the platform or file system has none. It
// sets nodeLeases, which every NewNode reads, so a test that calls it never
// calls t.Parallel.
func nodeLeaseModes(t *testing.T, fn func(t *testing.T, leases bool)) {
	for _, leases := range []bool{true, false} {
		t.Run(fmt.Sprintf("leases %v", leases), func(t *testing.T) {
			nodeLeases = leases
			defer func() { nodeLeases = true }()
			fn(t, leases)
		})
	}
}

// H14: an agent write that lands while a Step is reading a file back is
// never lost. The Node rewrites an A file in place only while it still holds
// what the readback handled, so the agent's value stays for the next
// readback, as real sysfs applies every write. Each case lands the write at
// the one point it can: inside the handler of the file's own readback, and
// between deciding and writing a value another file moved.
func TestH14AgentWriteDuringAReadbackIsKept(t *testing.T) {
	nodeLeaseModes(t, func(t *testing.T, leases bool) {
		t.Run("normalized readback", func(t *testing.T) {
			p := nodeIntelProfile()
			tr := nodeRender(t, p, nil)
			n := nodeNewNode(t, p, tr)
			n.SetLoad(nodeFullLoad)
			pl1 := nodeZoneFile("constraint_0_power_limit_uw", 0)
			f := nodeAFileOf(t, n, pl1)
			handle, armed := f.handle, true
			f.handle = func(v string) (string, string) {
				kind, eff := handle(v)
				if armed {
					armed = false
					nodeWrite(t, tr, pl1, "100000000")
				}
				return kind, eff
			}
			nodeWrite(t, tr, pl1, "120000001")
			nodeStep(t, n, 0)
			nodeStep(t, n, 0)
			if got := nodeRead(t, tr, pl1); got != "100000000\n" {
				t.Errorf("PL1 reads back %q, want the later write 100000000", got)
			}
			if got := n.State().Packages[0].RequestedCapW; got != 100 {
				t.Errorf("requested cap = %v W, want 100 W from the later write", got)
			}
		})

		// The agent's write can even equal the readback the Node was about
		// to write, and still be a different request: 1500000 is not 2400000
		// once the maximum rises.
		t.Run("same bytes as the readback", func(t *testing.T) {
			p := nodeEPYCProfile()
			tr := nodeRender(t, p, nil)
			n := nodeNewNode(t, p, tr)
			maxf, minf := nodePolicyFile(0, "scaling_max_freq"), nodePolicyFile(0, "scaling_min_freq")
			nodeWrite(t, tr, maxf, "1500000")
			nodeStep(t, n, 0)
			f := nodeAFileOf(t, n, minf)
			handle, armed := f.handle, true
			f.handle = func(v string) (string, string) {
				kind, eff := handle(v)
				if armed {
					armed = false
					nodeWrite(t, tr, minf, "1500000\n")
				}
				return kind, eff
			}
			nodeWrite(t, tr, minf, "2400000")
			nodeStep(t, n, 0)
			nodeStep(t, n, 0)
			nodeWrite(t, tr, maxf, "2400000")
			nodeStep(t, n, 0)
			if got := nodeRead(t, tr, minf); got != "1500000\n" {
				t.Errorf("scaling_min_freq after raising the maximum = %q, want the agent's later 1500000", got)
			}
		})

		t.Run("moved by another file", func(t *testing.T) {
			p := nodeEPYCProfile()
			tr := nodeRender(t, p, nil)
			n := nodeNewNode(t, p, tr)
			maxf, minf := nodePolicyFile(0, "scaling_max_freq"), nodePolicyFile(0, "scaling_min_freq")
			nodeWrite(t, tr, minf, "1900000")
			nodeStep(t, n, 0)

			// A maximum below the minimum moves the minimum's readback; the
			// agent writes the minimum just as that readback is about to be
			// written.
			f := nodeAFileOf(t, n, minf)
			current, armed := f.current, true
			f.current = func() string {
				if armed {
					armed = false
					nodeWrite(t, tr, minf, "2400000")
				}
				return current()
			}
			nodeWrite(t, tr, maxf, "1600000")
			nodeStep(t, n, 0)
			nodeStep(t, n, 0)
			if got := nodeRead(t, tr, minf); got != "1500000\n" {
				t.Errorf("scaling_min_freq = %q, want 1500000, the agent's 2400000 under the 1500000 maximum", got)
			}
			// Raising the maximum shows which minimum the policy kept.
			nodeWrite(t, tr, maxf, "2400000")
			nodeStep(t, n, 0)
			if got := nodeRead(t, tr, minf); got != "2400000\n" {
				t.Errorf("scaling_min_freq after raising the maximum = %q, want the agent's 2400000", got)
			}
		})

		// With a lease, a file open elsewhere may be a writer between
		// truncate and write, so the Node waits for it to close. The retry
		// rewrites the readback and never reads its own output back as a
		// request.
		if !leases {
			return
		}
		t.Run("open elsewhere", func(t *testing.T) {
			nodeNeedLeases(t)
			p := nodeEPYCProfile()
			tr := nodeRender(t, p, nil)
			n := nodeNewNode(t, p, tr)
			maxf, minf := nodePolicyFile(0, "scaling_max_freq"), nodePolicyFile(0, "scaling_min_freq")
			nodeWrite(t, tr, maxf, "1500000")
			nodeWrite(t, tr, minf, "2400000")
			nodeStep(t, n, 0)
			if got := nodeRead(t, tr, minf); got != "1500000\n" {
				t.Fatalf("scaling_min_freq = %q, want 1500000 under the 1500000 maximum", got)
			}
			r, err := os.Open(filepath.Join(tr.Root, minf))
			if err != nil {
				t.Fatal(err)
			}
			nodeWrite(t, tr, maxf, "1900000")
			nodeStep(t, n, 0)
			if got := nodeRead(t, tr, minf); got != "1500000\n" {
				t.Errorf("scaling_min_freq while open elsewhere = %q, want it left alone", got)
			}
			r.Close()
			nodeStep(t, n, 0)
			if got := nodeRead(t, tr, minf); got != "1900000\n" {
				t.Errorf("scaling_min_freq once closed = %q, want 1900000, the 2400000 request under the new maximum", got)
			}
			nodeWrite(t, tr, maxf, "2400000")
			nodeStep(t, n, 0)
			if got := nodeRead(t, tr, minf); got != "2400000\n" {
				t.Errorf("scaling_min_freq after raising the maximum = %q, want the 2400000 request", got)
			}
		})
	})
}

// H14: the agent writing while another goroutine steps the Node, as a
// simulator ticking next to real agent pods does. Whatever the interleaving,
// the last write is the one in effect, with or without the newline, for the
// normalized readback and for content caught between truncate and write
// alike.
func TestH14ConcurrentAgentWritesLastOneWins(t *testing.T) {
	t.Parallel()
	nodeNeedLeases(t)
	const trials, writes = 30, 10
	for _, newline := range []bool{false, true} {
		lost := 0
		for trial := 0; trial < trials; trial++ {
			p := nodeIntelProfile()
			tr := nodeRender(t, p, nil)
			n := nodeNewNode(t, p, tr)
			pl1 := nodeZoneFile("constraint_0_power_limit_uw", 0)
			stop, done := make(chan struct{}), make(chan error, 1)
			go func() {
				for {
					select {
					case <-stop:
						done <- nil
						return
					default:
					}
					if err := n.Step(0); err != nil {
						done <- err
						return
					}
				}
			}()
			var last int64
			for i := 0; i < writes; i++ {
				last = 100000000 + int64(i)*125000
				v := nodeItoa(last)
				if newline {
					v += "\n"
				}
				nodeWrite(t, tr, pl1, v)
			}
			close(stop)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			nodeStep(t, n, 0)
			got, gotW := nodeRead(t, tr, pl1), n.State().Packages[0].RequestedCapW
			if got != nodeItoa(last)+"\n" || gotW != float64(last)/1e6 {
				if lost < 3 {
					t.Errorf("newline %v, trial %d: last write %d, file now %q, requested %v W", newline, trial, last, got, gotW)
				}
				lost++
			}
		}
		if lost > 0 {
			t.Errorf("newline %v: %d of %d trials lost the agent's last write", newline, lost, trials)
		}
	}
}
