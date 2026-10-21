//go:build linux

package hwfuse_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwfuse"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwfuse/hwfusetest"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// hfClock is a clock a test moves by hand, so a read's time is the test's.
type hfClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *hfClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *hfClock) add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// hfNode starts a Live node of profile, cut to two cores per socket, which
// keeps its tree small, and mounts its view, on a clock that starts at the
// node's time.
func hfNode(t *testing.T, profile string, mutate func(*hwemu.Profile)) (*hwemu.Node, *hwemu.Tree, *hwemu.Tree, *hfClock) {
	t.Helper()
	hwfusetest.Require(t)
	small := func(p *hwemu.Profile) {
		if c := p.CPU; c.CoresPerSocket.V > 2 && c.CPUInfoVerbatim == "" {
			const src = "assumed:test fixture, two cores per socket keep the tree small"
			p.CPU.CoresPerSocket = hwemu.Fact[int]{V: 2, Src: src}
			p.Node.Capacity.CPU = hwemu.Fact[int64]{V: int64(c.Sockets.V * 2 * c.ThreadsPerCore.V), Src: src}
		}
		if mutate != nil {
			mutate(p)
		}
	}
	n, tree, _ := hwemutest.NewNodeWith(t, profile, small, hwemu.Options{Live: true})
	c := &hfClock{t: n.State().Time}
	return n, tree, hwfusetest.Mount(t, n, tree, c.now), c
}

func hfSys(view *hwemu.Tree, rel string) string {
	return filepath.Join(view.SysRoot, filepath.FromSlash(rel))
}

func hfPolicy(k int, name string) string { return path.Join(layout.PolicyDir(k), name) }

func hfZone(name string, idx ...int) string {
	return path.Join(layout.PowercapZoneDir("intel-rapl", idx...), name)
}

func hfReadInt(t *testing.T, p string) int64 {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("%s holds %q: %v", p, b, err)
	}
	return v
}

func hfAdvance(t *testing.T, n *hwemu.Node, c *hfClock, d time.Duration) {
	t.Helper()
	if err := n.AdvanceTo(c.add(d)); err != nil {
		t.Fatal(err)
	}
}

// HF01: through the view, os.WriteFile of a value the driver refuses fails
// with the kernel's errno, ENODATA for a locked RAPL limit and EBUSY for an
// EPP under performance, and changes nothing; a value it takes reads back
// at once, normalized, and the next write is checked against it.
func TestHF01WritesFailWithTheKernelsErrno(t *testing.T) {
	_, _, view, _ := hfNode(t, "amd-epyc-2s-hsmp", nil)
	gov, epp := hfSys(view, hfPolicy(0, "scaling_governor")), hfSys(view, hfPolicy(0, "energy_performance_preference"))
	if err := os.WriteFile(epp, []byte("balance_power"), 0); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("EPP under performance: %v, want EBUSY", err)
	}
	if b, _ := os.ReadFile(epp); string(b) != "performance\n" {
		t.Errorf("EPP reads %q after the refused write, want performance", b)
	}
	if err := os.WriteFile(gov, []byte("powersave"), 0); err != nil {
		t.Fatalf("powersave: %v", err)
	}
	if b, _ := os.ReadFile(gov); string(b) != "powersave\n" {
		t.Errorf("scaling_governor reads %q right after the write, want powersave", b)
	}
	if err := os.WriteFile(epp, []byte("balance_power\n"), 0); err != nil {
		t.Errorf("EPP after powersave: %v, want it checked against the governor written before it", err)
	}

	locked := func(p *hwemu.Profile) {
		for i := range p.Powercap.ControlTypes[0].Zones[0].Constraints {
			p.Powercap.ControlTypes[0].Zones[0].Constraints[i].RejectWrites = hwemu.Fact[string]{V: "locked", Src: "assumed:test, a BIOS lock"}
		}
	}
	_, _, iview, _ := hfNode(t, "intel-xeon-2s-rapl", locked)
	pl1 := hfSys(iview, hfZone("constraint_0_power_limit_uw", 0))
	before, _ := os.ReadFile(pl1)
	if err := os.WriteFile(pl1, []byte("120000000"), 0); !errors.Is(err, syscall.ENODATA) {
		t.Fatalf("a locked PL1: %v, want ENODATA", err)
	}
	if after, _ := os.ReadFile(pl1); string(after) != string(before) {
		t.Errorf("a locked PL1 reads %q after the refused write, want %q", after, before)
	}
}

// HF02: energy_uj read twice within a tick moves; a read never runs
// backwards, and a lower cap written between two reads slows it.
func TestHF02EnergyMovesBetweenTicks(t *testing.T) {
	n, _, view, c := hfNode(t, "intel-xeon-2s-rapl", nil)
	n.SetLoad(hwemu.Load{CPUUtil: []float64{1}})
	hfAdvance(t, n, c, time.Second)
	energy := hfSys(view, hfZone("energy_uj", 0))
	a := hfReadInt(t, energy)
	c.add(300 * time.Millisecond)
	b := hfReadInt(t, energy)
	if b <= a {
		t.Fatalf("energy_uj %d, then %d 300 ms later within the tick, want it to move", a, b)
	}
	if err := os.WriteFile(hfSys(view, hfZone("constraint_1_power_limit_uw", 0)), []byte("40000000"), 0); err != nil {
		t.Fatal(err)
	}
	c.add(300 * time.Millisecond)
	d := hfReadInt(t, energy)
	if d <= b || d-b >= b-a {
		t.Errorf("energy_uj moved %d uJ in 300 ms before a 40 W PL2 and %d after, want it slower and never back", b-a, d-b)
	}
	hfAdvance(t, n, c, 400*time.Millisecond)
	if e := hfReadInt(t, energy); e < d {
		t.Errorf("energy_uj %d at the tick, below the %d read before it", e, d)
	}
}

// HF03: entries look as sysfs shows them: attribute files are 4096 bytes
// whatever their content, with the layout's mode, links resolve through the
// view, directories list, and nothing can be created, removed or renamed.
func TestHF03EntriesLookAsSysfs(t *testing.T) {
	_, _, view, _ := hfNode(t, "intel-xeon-2s-rapl", nil)
	energy := hfSys(view, hfZone("energy_uj", 0))
	fi, err := os.Stat(energy)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode() != 0o400 || fi.Size() != hwemu.LiveAttrSize {
		t.Errorf("energy_uj: mode %v size %d, want -r-------- and %d", fi.Mode(), fi.Size(), hwemu.LiveAttrSize)
	}
	classRoot := filepath.Join(view.SysRoot, "class", "powercap")
	entries, err := os.ReadDir(classRoot)
	if err != nil || len(entries) == 0 {
		t.Fatalf("class/powercap lists %v, %v; want its zones", entries, err)
	}
	link := filepath.Join(classRoot, "intel-rapl:0")
	if entries[0].Type() != fs.ModeSymlink {
		t.Errorf("class/powercap/%s is %v, want a link", entries[0].Name(), entries[0].Type())
	}
	target, err := os.Readlink(link)
	if err != nil || !strings.HasPrefix(target, "../../devices/virtual/powercap/") {
		t.Errorf("class/powercap/%s links to %q, %v; want ../../devices/virtual/powercap/...", "intel-rapl:0", target, err)
	}
	if li, err := os.Lstat(link); err != nil || li.Size() != 0 {
		t.Errorf("a link is %v bytes, %v; want 0", li.Size(), err)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(link, "energy_uj"))
	if err != nil || !strings.HasPrefix(real, view.SysRoot) {
		t.Errorf("energy_uj through the class link resolves to %q, %v; want a file of the view", real, err)
	}
	matches, _ := filepath.Glob(filepath.Join(classRoot, "*"))
	if len(matches) != len(entries) {
		t.Errorf("Glob class/powercap/* matches %d, ReadDir lists %d", len(matches), len(entries))
	}
	if di, err := os.Stat(classRoot); err != nil || di.Size() != 0 || !di.IsDir() {
		t.Errorf("a directory: %v, %v; want size 0", di, err)
	}

	maxFreq := hfSys(view, hfPolicy(0, "cpuinfo_max_freq"))
	if _, err := os.OpenFile(maxFreq, os.O_WRONLY, 0); !errors.Is(err, syscall.EACCES) {
		t.Errorf("open a 0444 file for writing: %v, want EACCES", err)
	}
	if err := syscall.Access(maxFreq, 2); !errors.Is(err, syscall.EACCES) {
		t.Errorf("access(W_OK) of a 0444 file: %v, want EACCES", err)
	}
	if err := syscall.Access(hfSys(view, hfPolicy(0, "scaling_max_freq")), 2); err != nil {
		t.Errorf("access(W_OK) of a 0644 file its owner probes: %v, want nil", err)
	}
	dir := hfSys(view, layout.PolicyDir(0))
	if err := os.WriteFile(filepath.Join(dir, "new_attribute"), []byte("1"), 0o644); !errors.Is(err, syscall.EACCES) {
		t.Errorf("create a file: %v, want EACCES", err)
	}
	if err := os.Remove(maxFreq); !errors.Is(err, syscall.EPERM) {
		t.Errorf("remove a file: %v, want EPERM", err)
	}
	if err := os.Rename(maxFreq, filepath.Join(dir, "renamed")); !errors.Is(err, syscall.EPERM) {
		t.Errorf("rename a file: %v, want EPERM", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); !errors.Is(err, syscall.EPERM) {
		t.Errorf("mkdir: %v, want EPERM", err)
	}
}

// HF04: a reader that keeps a file open reads the value of the moment at
// offset 0 on the same inode, which the emulator never replaces by rename;
// meanwhile the tick's write of the file on disk takes its lease and stays
// in place, since no reader holds the file on disk open.
func TestHF04OpenFilesSeeNewValuesOnTheSameInode(t *testing.T) {
	n, tree, view, c := hfNode(t, "intel-xeon-2s-rapl", nil)
	n.SetLoad(hwemu.Load{CPUUtil: []float64{1}})
	energy := hfSys(view, hfZone("energy_uj", 0))
	f, err := os.Open(energy)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	read := func() int64 {
		buf := make([]byte, 64)
		k, err := f.ReadAt(buf, 0)
		if err != nil && k == 0 {
			t.Fatal(err)
		}
		v, err := strconv.ParseInt(strings.TrimSpace(string(buf[:k])), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	ino := func(fi fs.FileInfo) uint64 { return fi.Sys().(*syscall.Stat_t).Ino }
	first, _ := f.Stat()
	disk := filepath.Join(tree.SysRoot, filepath.FromSlash(hfZone("energy_uj", 0)))
	diskBefore, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	a := read()
	for i := 0; i < 3; i++ {
		hfAdvance(t, n, c, time.Second)
	}
	if b := read(); b <= a {
		t.Errorf("an open energy_uj reads %d at offset 0 three ticks after %d, want the new value", b, a)
	}
	again, _ := os.Stat(energy)
	if ino(again) != ino(first) {
		t.Errorf("energy_uj is inode %d after three ticks, was %d", ino(again), ino(first))
	}
	diskAfter, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	if hfLeasesWork(t, filepath.Join(tree.StateRoot, layout.TmpDir)) && ino(diskAfter) != ino(diskBefore) {
		t.Errorf("the file on disk is inode %d after the ticks, was %d: written by rename, not in place under its lease", ino(diskAfter), ino(diskBefore))
	}
	if got, want := hfReadInt(t, disk), read(); got > want {
		t.Errorf("energy_uj on disk %d is ahead of the view's %d", got, want)
	}
}

// hfLeasesWork reports whether a write lease can be taken in dir, which the
// emulator's in-place write of a file on disk needs.
func hfLeasesWork(t *testing.T, dir string) bool {
	t.Helper()
	f, err := os.CreateTemp(dir, "lease-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_SETLEASE, syscall.F_WRLCK); errno != 0 {
		return false
	}
	_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_SETLEASE, syscall.F_UNLCK)
	return true
}

// HF05: each write(2) is one store, as on sysfs: truncation changes
// nothing, and a newline alone, written after a value in the same open,
// fails with EINVAL.
func TestHF05EachWriteIsOneStore(t *testing.T) {
	_, _, view, _ := hfNode(t, "intel-xeon-2s-rapl", nil)
	pl1 := hfSys(view, hfZone("constraint_0_power_limit_uw", 0))
	f, err := os.OpenFile(pl1, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("150000000")); err != nil {
		t.Fatalf("a PL1 in range: %v", err)
	}
	if _, err := f.Write([]byte("\n")); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("a newline alone: %v, want EINVAL", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(pl1); string(b) != "150000000\n" {
		t.Errorf("PL1 reads %q, want 150000000", b)
	}
}

// HF06: the fake GPU tools work on the state view: nvidia-smi -pl writes
// the NVML limit through it, and the query reads it back.
func TestHF06FakeToolsRunOnTheStateView(t *testing.T) {
	_, _, view, _ := hfNode(t, "nvidia-h100-nvl-8gpu", nil)
	r := smi.Runner{Env: smi.Env{SysRoot: view.SysRoot, StateRoot: view.StateRoot}}
	if out, err := r.Run(context.Background(), "nvidia-smi", "-i", "0", "-pl", "300"); err != nil {
		t.Fatalf("nvidia-smi -pl 300: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(view.StateRoot, filepath.FromSlash(layout.NVMLGPUDir(0)), "power_limit_mw")); string(b) != "300000\n" {
		t.Errorf("power_limit_mw reads %q after nvidia-smi -pl 300, want 300000", b)
	}
	out, err := r.Run(context.Background(), "nvidia-smi", "-i", "0", "--query-gpu=power.limit", "--format=csv,noheader")
	if err != nil || !strings.Contains(string(out), "300.00 W") {
		t.Errorf("nvidia-smi --query-gpu=power.limit: %q, %v; want 300.00 W", out, err)
	}
}

// HF07: readers, a writer and the tick at once, for the race detector.
func TestHF07ReadersWritersAndTicksAtOnce(t *testing.T) {
	n, _, view, c := hfNode(t, "intel-xeon-2s-rapl", nil)
	n.SetLoad(hwemu.Load{CPUUtil: []float64{0.7}})
	energy := hfSys(view, hfZone("energy_uj", 0))
	pl1 := hfSys(view, hfZone("constraint_0_power_limit_uw", 1))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			last := int64(-1)
			for {
				select {
				case <-stop:
					return
				default:
				}
				b, err := os.ReadFile(energy)
				if err != nil {
					errs <- err
					return
				}
				v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
				if err != nil || v < last {
					errs <- errors.New("energy_uj read " + string(b) + " after " + strconv.FormatInt(last, 10))
					return
				}
				last = v
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := os.WriteFile(pl1, []byte(strconv.Itoa(100000000+i*1000000)), 0); err != nil {
				errs <- err
				return
			}
		}
	}()
	for i := 0; i < 10; i++ {
		hfAdvance(t, n, c, 200*time.Millisecond)
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// The view must not serve a node twice, or a name that is not a directory.
func TestHF08MountRefusesBadNodes(t *testing.T) {
	hwfusetest.Require(t)
	n, tree, _ := hwemutest.NewNodeWith(t, "vm-no-powercap", nil, hwemu.Options{Live: true})
	node := hwfuse.Node{Name: "a", Files: tree.LiveFiles(), Backend: n}
	for _, nodes := range [][]hwfuse.Node{
		{node, node},
		{{Name: "a/b", Files: node.Files, Backend: n}},
		{{Name: "a", Files: node.Files}},
	} {
		if s, err := hwfuse.Mount(t.TempDir(), nodes, hwfuse.Options{}); err == nil {
			_ = s.Unmount()
			t.Errorf("Mount of %d nodes named %q took them", len(nodes), nodes[0].Name)
		}
	}
}
