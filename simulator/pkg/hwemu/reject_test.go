package hwemu

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// rejectWriteAt writes v to rel as the agent does and gives the file the
// modification time at, so the order of writes within one step is set by
// the test rather than by the file system clock's resolution.
func rejectWriteAt(t *testing.T, tr *Tree, rel, v string, at time.Time) {
	t.Helper()
	nodeWrite(t, tr, rel, v)
	if err := os.Chtimes(filepath.Join(tr.Root, filepath.FromSlash(rel)), at, at); err != nil {
		t.Fatal(err)
	}
}

// Writes that land between two steps are handled in the order they were
// written, as the kernel checks each against the state the earlier ones
// left: from the performance policy of a setpolicy driver, an EPP write
// that comes before the switch to powersave is refused with EBUSY, while
// one that comes after it is taken. Writes with the same time keep the
// order the files were registered in, scaling_governor first.
func TestReadbackHandlesWritesInWriteOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, profile string
		eppFirst      bool
		sameTime      bool
		wantEPP       string
		wantKind      string
	}{
		{"amd-pstate-epp, EPP first", "amd-epyc-2s-hsmp", true, false, "performance", EventRejected},
		{"amd-pstate-epp, governor first", "amd-epyc-2s-hsmp", false, false, "balance_power", EventAccepted},
		{"amd-pstate-epp, same time", "amd-epyc-2s-hsmp", true, true, "balance_power", EventAccepted},
		{"intel_pstate, EPP first", "intel-xeon-2s-rapl", true, false, "balance_performance", EventRejected},
		{"intel_pstate, governor first", "intel-xeon-2s-rapl", false, false, "balance_power", EventAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, tr := cgNode(t, ensureBuiltin(t, tc.profile))
			gov, epp := nodePolicyFile(0, "scaling_governor"), nodePolicyFile(0, "energy_performance_preference")
			if nodeRead(t, tr, gov) != "performance\n" {
				cgWriteStep(t, n, tr, gov, "performance")
			}
			if got := nodeRead(t, tr, epp); got != "performance\n" {
				t.Fatalf("EPP under performance reads %q, want performance", got)
			}
			at := time.Now().Add(-time.Minute)
			eppAt, govAt := at, at.Add(time.Millisecond)
			if !tc.eppFirst {
				eppAt, govAt = govAt, eppAt
			}
			if tc.sameTime {
				govAt = eppAt
			}
			rejectWriteAt(t, tr, epp, "balance_power", eppAt)
			rejectWriteAt(t, tr, gov, "powersave", govAt)
			nodeStep(t, n, time.Second)
			if e := cgOneEvent(t, n, gov); e.Kind != EventAccepted {
				t.Errorf("governor write: %+v, want accepted", e)
			}
			e := cgOneEvent(t, n, epp)
			if e.Kind != tc.wantKind || tc.wantKind == EventRejected && e.Errno != ErrnoEBUSY {
				t.Errorf("EPP write: %+v, want %s (EBUSY when refused)", e, tc.wantKind)
			}
			if got := nodeRead(t, tr, epp); got != tc.wantEPP+"\n" {
				t.Errorf("EPP reads %q, want %s", got, tc.wantEPP)
			}
		})
	}
}

// CheckWrite answers a write as the kernel would at its moment, through
// the handler the readback runs: a refusal returns its errno and changes
// nothing, a write the driver takes is in effect at once, so the next one
// is checked against it, and a read-only, missing or directory path fails
// as sysfs fails it.
func TestCheckWriteAnswersAsTheKernel(t *testing.T) {
	t.Parallel()
	n, tr := cgNode(t, ensureBuiltin(t, "amd-epyc-2s-hsmp"))
	gov, epp := nodePolicyFile(0, "scaling_governor"), nodePolicyFile(0, "energy_performance_preference")

	err := n.CheckWrite(epp, "balance_power")
	var pe *fs.PathError
	if !errors.Is(err, syscall.EBUSY) || !errors.As(err, &pe) || pe.Op != "write" || pe.Path != filepath.Join(tr.Root, filepath.FromSlash(epp)) {
		t.Fatalf("EPP under performance: %v, want a write *fs.PathError on the file with EBUSY", err)
	}
	if err := n.CheckWrite(gov, "powersave\n"); err != nil {
		t.Fatalf("powersave: %v", err)
	}
	if err := n.CheckWrite(epp, "balance_power"); err != nil {
		t.Fatalf("EPP after powersave: %v, want it taken against the state the governor write left", err)
	}
	if err := n.CheckWrite(gov, "ondemand"); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("a governor the policy lacks: %v, want EINVAL", err)
	}
	nodeStep(t, n, 0)
	if e := cgOneEvent(t, n, epp); e.Kind != EventRejected || e.Errno != ErrnoEBUSY || e.Written != "balance_power" {
		t.Errorf("the refused EPP write reports %+v, want rejected with EBUSY", e)
	}
	if e := cgOneEvent(t, n, gov); e.Kind != EventRejected || e.Errno != ErrnoEINVAL {
		t.Errorf("the refused governor write reports %+v, want rejected with EINVAL", e)
	}
	if pol := n.policies[0]; pol.governor != "powersave" || pol.epp != "balance_power" {
		t.Errorf("policy0 runs %s with EPP %s after the two writes, want powersave with balance_power", pol.governor, pol.epp)
	}
	// The caller writes the file; the readback finds the value in effect.
	nodeWrite(t, tr, gov, "powersave")
	nodeWrite(t, tr, epp, "balance_power")
	nodeStep(t, n, time.Second)
	if got := nodeRead(t, tr, epp); got != "balance_power\n" {
		t.Errorf("EPP reads %q after the step, want balance_power", got)
	}
	if pol := n.policies[0]; pol.governor != "powersave" || pol.epp != "balance_power" {
		t.Errorf("policy0 runs %s with EPP %s after the step, want powersave with balance_power", pol.governor, pol.epp)
	}

	cases := []struct {
		rel   string
		errno syscall.Errno
	}{
		{nodePolicyFile(0, "cpuinfo_max_freq"), syscall.EACCES},
		{path.Join(layout.SysDir, layout.PowercapZoneDir("intel-rapl", 0), "energy_uj"), syscall.EACCES},
		{nodePolicyFile(0, "no_such_attribute"), syscall.ENOENT},
		{path.Join(layout.SysDir, layout.PolicyDir(0)), syscall.EISDIR},
	}
	for _, c := range cases {
		if err := n.CheckWrite(c.rel, "1"); !errors.Is(err, c.errno) {
			t.Errorf("CheckWrite(%s) = %v, want %v", c.rel, err, c.errno)
		}
	}
	if err := n.CheckWrite(epp, ""); err != nil {
		t.Errorf("a write of no bytes: %v, want nil", err)
	}
}

// CheckWrite follows the tree's links, as a write through
// /sys/bus/pci/devices does, and a cap it takes binds the physics at once.
func TestCheckWriteFollowsLinksAndAppliesAtOnce(t *testing.T) {
	t.Parallel()
	p := nodeMI300XProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(Load{GPUUtil: []float64{1}})
	capf := nodeMI300XCap(0, "power1_cap")
	if err := n.CheckWrite(capf, "300000000"); err != nil {
		t.Fatal(err)
	}
	if got := n.State().GPUs[0].LimitW; got != 300 {
		t.Errorf("limit %v W right after the write, want 300", got)
	}
	link := path.Join(layout.SysDir, "class", "drm", "card1", "device", "hwmon")
	matches, _ := filepath.Glob(filepath.Join(tr.Root, filepath.FromSlash(link), "hwmon*", "power1_cap"))
	if len(matches) == 0 {
		t.Fatalf("no power1_cap under %s", link)
	}
	rel, _ := filepath.Rel(tr.Root, matches[0])
	if err := n.CheckWrite(filepath.ToSlash(rel), "900000000"); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("an out-of-range cap through the class link: %v, want EINVAL", err)
	}
}

// A RAPL limit is stored in whole power units in a field of the register,
// so a value past the field wraps as the kernel writes it
// (rapl_write_data_raw): 4100 W in the 15-bit PL1 field of 1/8 W units
// reads back 4 W, and the package is held at that limit; psys has a 17-bit
// field and TPMI an 18-bit one.
func TestRAPLLimitWrapsInItsRegisterField(t *testing.T) {
	t.Parallel()
	t.Run("MSR package PL1", func(t *testing.T) {
		t.Parallel()
		p := nodeIntelProfile()
		tr := nodeRender(t, p, nil)
		n := nodeNewNode(t, p, tr)
		n.SetLoad(nodeFullLoad)
		pl1 := nodeZoneFile("constraint_0_power_limit_uw", 0)
		nodeWrite(t, tr, pl1, "4100000000")
		nodeStep(t, n, 30*time.Second)
		if got := nodeRead(t, tr, pl1); got != "4000000\n" {
			t.Errorf("4100 W reads back %q, want 4000000: (4100 W / 125000 uW) & 0x7fff = 32 units", got)
		}
		if e := nodeOneEvent(t, n, pl1); e.Effective != "4000000" {
			t.Errorf("event %+v, want 4000000 in effect", e)
		}
		if pk := n.State().Packages[0]; pk.RequestedCapW != 4 {
			t.Errorf("package 0 requested cap %v W, want 4", pk.RequestedCapW)
		}
		nodeWrite(t, tr, pl1, "4095875000")
		nodeStep(t, n, 0)
		if got := nodeRead(t, tr, pl1); got != "4095875000\n" {
			t.Errorf("the largest value the field holds reads back %q, want it kept", got)
		}
	})
	t.Run("psys and TPMI", func(t *testing.T) {
		t.Parallel()
		for _, c := range []struct {
			profile, zone, write, want string
		}{
			{"intel-xeon-2s-psys", "intel-rapl:1", "16400000000", "16000000\n"},
			{"intel-xeon6-2s-tpmi", "intel-rapl:0", "4100000000", "4100000000\n"},
			{"intel-xeon6-2s-tpmi", "intel-rapl:0", "33000000000", "232000000\n"},
		} {
			p := ensureBuiltin(t, c.profile)
			tr := renderTestTree(t, p)
			n := nodeNewNode(t, p, tr)
			idx := c.zone[len("intel-rapl:"):]
			var rel string
			for _, f := range tr.Files {
				if path.Base(f.Rel) == "constraint_0_power_limit_uw" && path.Base(path.Dir(f.Rel)) == "intel-rapl:"+idx {
					rel = f.Rel
				}
			}
			if rel == "" {
				t.Fatalf("%s has no %s PL1", c.profile, c.zone)
			}
			nodeWrite(t, tr, rel, c.write)
			nodeStep(t, n, 0)
			if got := nodeRead(t, tr, rel); got != c.want {
				t.Errorf("%s %s: %s reads back %q, want %q", c.profile, c.zone, c.write, got, c.want)
			}
		}
	})
}

// The readback skips an A file whose stamp shows no change since its last
// read, and still sees every write: one of the same length right after a
// read, which the file's change time reveals, one landing within the stamp
// margin of the read, which is read again regardless, and a file nobody
// touched, which stays as it is.
func TestReadbackSeesEveryWriteAfterAStampedRead(t *testing.T) {
	t.Parallel()
	p := nodeIntelProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	pl1 := nodeZoneFile("constraint_0_power_limit_uw", 0)
	for _, v := range []string{"120000000", "130000000", "140000000"} {
		nodeWrite(t, tr, pl1, v)
		nodeStep(t, n, time.Second)
		if e := nodeOneEvent(t, n, pl1); e.Kind != EventAccepted || e.Written != v {
			t.Fatalf("write %s: %+v, want it accepted", v, e)
		}
	}
	for k := 0; k < 3; k++ {
		nodeStep(t, n, time.Second)
		if ev := nodeEvents(n, pl1); len(ev) != 0 {
			t.Fatalf("step %d with no write: events %+v", k, ev)
		}
	}
	// Age the stamp past the margin, then write a value of the same length.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(tr.Root, filepath.FromSlash(pl1)), old, old); err != nil {
		t.Fatal(err)
	}
	nodeStep(t, n, time.Second)
	nodeWrite(t, tr, pl1, "150000000")
	nodeStep(t, n, time.Second)
	if e := nodeOneEvent(t, n, pl1); e.Kind != EventAccepted || e.Written != "150000000" {
		t.Errorf("a same-length write after an aged read: %+v, want it accepted", e)
	}
	if got := nodeRead(t, tr, pl1); got != "150000000\n" {
		t.Errorf("PL1 reads %q, want 150000000", got)
	}
}
