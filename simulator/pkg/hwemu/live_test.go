package hwemu

import (
	"errors"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// liveNode renders p and starts a Live node over it at nodeStart.
func liveNode(t *testing.T, p *Profile) (*Node, *Tree) {
	t.Helper()
	tr := renderTestTree(t, p)
	n, err := NewNode(p, tr, Options{Start: nodeStart, Live: true})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return n, tr
}

// liveAt is nodeStart plus d, the time of a live call.
func liveAt(d time.Duration) time.Time { return nodeStart.Add(d) }

func liveRead(t *testing.T, n *Node, rel string, at time.Duration) string {
	t.Helper()
	b, err := n.LiveRead(rel, liveAt(at))
	if err != nil {
		t.Fatalf("LiveRead(%s): %v", rel, err)
	}
	return string(b)
}

func liveReadInt(t *testing.T, n *Node, rel string, at time.Duration) int64 {
	t.Helper()
	v, err := strconv.ParseInt(strings.TrimSpace(liveRead(t, n, rel, at)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func liveAdvance(t *testing.T, n *Node, at time.Duration) {
	t.Helper()
	if err := n.AdvanceTo(liveAt(at)); err != nil {
		t.Fatalf("AdvanceTo(%v): %v", at, err)
	}
}

// LV01: a write the driver refuses fails at the write with the kernel's
// errno and changes nothing; one it takes is in effect at once, so the next
// write is checked against it, reads back normalized at once, is on disk
// before the write returns, and every write is an event of the next step.
func TestLV01WritesAreAnsweredAtTheWrite(t *testing.T) {
	t.Parallel()
	n, tr := liveNode(t, ensureBuiltin(t, "amd-epyc-2s-hsmp"))
	gov, epp := nodePolicyFile(0, "scaling_governor"), nodePolicyFile(0, "energy_performance_preference")
	if got := liveRead(t, n, epp, 0); got != "performance\n" {
		t.Fatalf("EPP reads %q under performance, want performance", got)
	}

	_, err := n.LiveWrite(epp, []byte("balance_power"), liveAt(100*time.Millisecond))
	var pe *fs.PathError
	if !errors.Is(err, syscall.EBUSY) || !errors.As(err, &pe) || pe.Path != epp {
		t.Fatalf("EPP under performance: %v, want a *fs.PathError on %s with EBUSY", err, epp)
	}
	if got := liveRead(t, n, epp, 150*time.Millisecond); got != "performance\n" {
		t.Errorf("EPP reads %q after the refused write, want performance", got)
	}
	if k, err := n.LiveWrite(gov, []byte("powersave\n"), liveAt(200*time.Millisecond)); err != nil || k != len("powersave\n") {
		t.Fatalf("powersave: %d, %v; want every byte taken", k, err)
	}
	if got := liveRead(t, n, gov, 250*time.Millisecond); got != "powersave\n" {
		t.Errorf("scaling_governor reads %q right after the write, want powersave", got)
	}
	if _, err := n.LiveWrite(epp, []byte("balance_power"), liveAt(300*time.Millisecond)); err != nil {
		t.Fatalf("EPP after powersave: %v, want it checked against the state the governor write left", err)
	}
	if got := nodeRead(t, tr, epp); got != "balance_power\n" {
		t.Errorf("the file on disk holds %q right after the write, want balance_power", got)
	}
	if _, err := n.LiveWrite(gov, []byte("ondemand"), liveAt(400*time.Millisecond)); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("a governor the policy lacks: %v, want EINVAL", err)
	}

	liveAdvance(t, n, time.Second)
	var kinds []string
	for _, e := range n.State().Events {
		if e.File == gov || e.File == epp {
			kinds = append(kinds, path.Base(e.File)+"="+e.Kind+":"+e.Errno)
		}
	}
	want := "energy_performance_preference=rejected:EBUSY scaling_governor=accepted: energy_performance_preference=accepted: scaling_governor=rejected:EINVAL"
	if got := strings.Join(kinds, " "); got != want {
		t.Errorf("events of the next step: %s, want %s", got, want)
	}
}

// LV02: an energy counter read twice within a tick moves, at the power in
// effect; a read never steps the physics differently from a node nobody
// reads, so the counter at a step's time is the one a node stepped without
// reads shows, and it never goes back, across a cap write too.
func TestLV02EnergyMovesBetweenTicksAndMatchesTheSteps(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "intel-xeon-2s-rapl")
	n, _ := liveNode(t, p)
	ref := nodeNewNode(t, p, renderTestTree(t, p))
	full := Load{CPUUtil: []float64{1}}
	n.SetLoad(full)
	ref.SetLoad(full)
	energy := nodeZoneFile("energy_uj", 0)

	last := liveReadInt(t, n, energy, 0)
	for tick := 1; tick <= 4; tick++ {
		base := time.Duration(tick-1) * time.Second
		for _, at := range []time.Duration{150 * time.Millisecond, 450 * time.Millisecond, 870 * time.Millisecond} {
			v := liveReadInt(t, n, energy, base+at)
			if v <= last {
				t.Fatalf("tick %d: energy_uj read %d at +%v after %d, want it to move within the tick", tick, v, at, last)
			}
			last = v
		}
		if tick == 3 {
			// A lower PL1 between two reads: the counter slows, never goes back.
			if _, err := n.LiveWrite(nodeZoneFile("constraint_0_power_limit_uw", 0), []byte("60000000"), liveAt(base+900*time.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
		end := time.Duration(tick) * time.Second
		liveAdvance(t, n, end)
		if err := ref.AdvanceTo(liveAt(end)); err != nil {
			t.Fatal(err)
		}
		at := liveReadInt(t, n, energy, end)
		if at < last {
			t.Fatalf("tick %d: energy_uj %d at the step, below the %d read before it", tick, at, last)
		}
		last = at
		// Until the cap write, the node nobody reads shows the same counter.
		if want := nodeReadInt64(t, ref.tree, energy); tick < 3 && at != want {
			t.Errorf("tick %d: energy_uj %d, a node stepped without reads shows %d", tick, at, want)
		}
	}
}

// LV03: a counter a reader saw is on disk before the read returns, and a
// limit written through the view is on disk once the write returns, so a
// Node resumed over the tree never shows a counter below what a reader saw
// and keeps the limit.
func TestLV03ResumeKeepsWhatReadersSawAndWrote(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "intel-xeon-2s-rapl")
	n, tr := liveNode(t, p)
	n.SetLoad(Load{CPUUtil: []float64{1}})
	energy, limit := nodeZoneFile("energy_uj", 0), nodeZoneFile("constraint_0_power_limit_uw", 0)
	liveAdvance(t, n, time.Second)
	if _, err := n.LiveWrite(limit, []byte("90000000"), liveAt(1200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	seen := liveReadInt(t, n, energy, 1700*time.Millisecond)
	if onDisk := nodeReadInt64(t, tr, energy); onDisk != seen {
		t.Errorf("energy_uj on disk %d after a read saw %d", onDisk, seen)
	}

	resumed, err := NewNode(p, tr, Options{Start: liveAt(1800 * time.Millisecond), Live: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := liveReadInt(t, resumed, energy, 1800*time.Millisecond); got < seen {
		t.Errorf("the resumed node reads energy_uj %d, below the %d a reader saw", got, seen)
	}
	if got := liveRead(t, resumed, limit, 1800*time.Millisecond); got != "90000000\n" {
		t.Errorf("the resumed node reads PL1 %q, want the 90000000 written through the view", got)
	}
}

// LV04: files fail as sysfs fails them: a read-only one, energy_uj and
// cpuinfo_max_freq among them, refuses a write with EACCES, a missing one is
// ENOENT and a directory EISDIR; a writable file the emulator does not model
// takes the write and keeps its content, with a restored event; a write of
// no bytes reaches nothing; a write takes at most a page.
func TestLV04FilesFailAsSysfsDoes(t *testing.T) {
	t.Parallel()
	n, _ := liveNode(t, ensureBuiltin(t, "intel-xeon-2s-rapl"))
	for _, c := range []struct {
		rel   string
		errno syscall.Errno
	}{
		{nodePolicyFile(0, "cpuinfo_max_freq"), syscall.EACCES},
		{nodeZoneFile("energy_uj", 0), syscall.EACCES},
		{nodePolicyFile(0, "no_such_attribute"), syscall.ENOENT},
		{path.Join(layout.SysDir, layout.PolicyDir(0)), syscall.EISDIR},
	} {
		if _, err := n.LiveWrite(c.rel, []byte("1"), liveAt(0)); !errors.Is(err, c.errno) {
			t.Errorf("LiveWrite(%s) = %v, want %v", c.rel, err, c.errno)
		}
	}
	if _, err := n.LiveRead(nodePolicyFile(0, "no_such_attribute"), liveAt(0)); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("LiveRead of a missing file: %v, want ENOENT", err)
	}

	status := cgIntelPstateFile("status")
	before := liveRead(t, n, status, 0)
	if k, err := n.LiveWrite(status, []byte("passive"), liveAt(10*time.Millisecond)); err != nil || k != len("passive") {
		t.Fatalf("intel_pstate/status: %d, %v; want the write taken", k, err)
	}
	if got := liveRead(t, n, status, 20*time.Millisecond); got != before {
		t.Errorf("intel_pstate/status reads %q after the write, want %q: the emulator does not model it", got, before)
	}
	limit := nodeZoneFile("constraint_0_power_limit_uw", 0)
	if k, err := n.LiveWrite(limit, nil, liveAt(30*time.Millisecond)); k != 0 || err != nil {
		t.Errorf("a write of no bytes: %d, %v; want 0, nil", k, err)
	}
	liveAdvance(t, n, time.Second)
	if e := cgOneEvent(t, n, status); e.Kind != EventRestored || e.Written != "passive" {
		t.Errorf("the write to intel_pstate/status reports %+v, want restored", e)
	}
	long := []byte(strings.Repeat("p", LiveAttrSize+10))
	if k, err := n.LiveWrite(status, long, liveAt(1100*time.Millisecond)); k != LiveAttrSize || err != nil {
		t.Errorf("a write of %d bytes took %d, %v; want one page, %d", len(long), k, err, LiveAttrSize)
	}
}

// LV05: a read that stepped the node past a tick's time is no error for the
// tick: the step ends at the node's time, and the events the read's step
// made are the tick's.
func TestLV05TickBehindAReadIsNoError(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "nvidia-h100-sxm-8gpu")
	p.GPUs.PersistenceMode = Fact[string]{V: "disabled", Src: "assumed:test, persistence off"}
	n, _ := liveNode(t, p)
	limit := nodeNVMLFile(0, "power_limit_mw")
	liveAdvance(t, n, 50*time.Millisecond)
	if _, err := n.LiveWrite(limit, []byte("300000\n"), liveAt(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	// Idle with persistence off: the read's own step puts the limit back.
	if got := liveRead(t, n, limit, 1500*time.Millisecond); got == "300000\n" {
		t.Errorf("power_limit_mw reads the written 300000 on an idle GPU with persistence off, want the default back")
	}
	liveAdvance(t, n, time.Second)
	if got := n.State().Time; !got.Equal(liveAt(1500 * time.Millisecond)) {
		t.Errorf("node time %v after the tick, want the read's %v", got, liveAt(1500*time.Millisecond))
	}
	var kinds []string
	for _, e := range nodeEvents(n, limit) {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, " "); got != EventAccepted+" "+EventResetPersistenceOff {
		t.Errorf("events of the tick for power_limit_mw: %s, want accepted then reset-persistence-off", got)
	}
}

// LV06: a live view serves sys/ and state/ without the temp directory, and
// only a Live node answers it.
func TestLV06LiveFilesAndPlainNodes(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "nvidia-h100-nvl-8gpu")
	tr := renderTestTree(t, p)
	var sys, state int
	for _, f := range tr.LiveFiles() {
		switch top, _, _ := strings.Cut(f.Rel, "/"); {
		case strings.HasPrefix(f.Rel, path.Join(layout.StateDir, layout.TmpDir)):
			t.Errorf("LiveFiles lists %s, in the temp directory", f.Rel)
		case top == layout.SysDir:
			sys++
		case top == layout.StateDir:
			state++
		default:
			t.Errorf("LiveFiles lists %s, outside sys/ and state/", f.Rel)
		}
	}
	if sys == 0 || state == 0 {
		t.Errorf("LiveFiles lists %d entries under sys/ and %d under state/, want both", sys, state)
	}
	n := nodeNewNode(t, p, tr)
	if _, err := n.LiveRead(nodeNVMLFile(0, "power_limit_mw"), liveAt(0)); !errors.Is(err, errNotLive) {
		t.Errorf("LiveRead on a node without Options.Live: %v, want errNotLive", err)
	}
	if _, err := NewNode(p, &Tree{Root: tr.Root, SysRoot: tr.SysRoot, StateRoot: tr.StateRoot}, Options{Live: true}); err == nil {
		t.Errorf("a Live node over a tree without its layout started")
	}
}
