package main

// Emulated-node tests through simulator/pkg/emulate, the emulator a
// simulator in emulate mode runs: the node's tree is kept by the emulator
// on its tick, across an emulator restart, rather than stepped by the test,
// and the agent reads it as it reads a real host.

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/agent/dvfs"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// emulatedPool is one node of a one-pool cluster spec, expanded as the
// simulator expands it, with the builtin profiles.
func emulatedPool(t *testing.T, poolYAML string) clusterspec.Member {
	t.Helper()
	spec, err := clusterspec.Parse([]byte("apiVersion: sim.joulie.io/v1alpha1\nkind: ClusterSpec\nmetadata: {name: agent-test}\nspec:\n  pools:\n  - " + poolYAML + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	members, err := spec.Expand(clusterspec.Profiles(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 {
		t.Fatalf("%d members, want 1", len(members))
	}
	return members[0]
}

// emulatorRun is an emulator over one member in root, started at start, and
// the tree it keeps for the member.
type emulatorRun struct {
	e    *emulate.Emulator
	tree *hwemu.Tree
}

func startEmulator(t *testing.T, root string, m clusterspec.Member, start time.Time) *emulatorRun {
	t.Helper()
	run := &emulatorRun{}
	e, err := emulate.New(emulate.Config{
		Root:    root,
		Cluster: "agent-test",
		Members: []clusterspec.Member{m},
		Start:   start,
		Ensure: func(p *hwemu.Profile, r string, o hwemu.RenderOptions) (*hwemu.Tree, hwemu.EnsureReport, error) {
			tree, rep, err := hwemu.Ensure(p, r, o)
			run.tree = tree
			return tree, rep, err
		},
		Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	run.e = e
	t.Cleanup(func() { _ = e.Close() })
	return run
}

// packagePower is the summed power of the snapshot's CPU packages.
func packagePower(snap *emulate.Snapshot) float64 {
	w := 0.0
	for _, n := range snap.Nodes {
		for _, c := range n.Components {
			if c.Type == semconv.HwTypeCPU {
				w += c.PowerW
			}
		}
	}
	return w
}

// A17: the Intel host at a constant 0.6 CPU load, kept by the emulator on
// ticks aligned to wall time as A04 aligns its steps. The agent's DVFS
// power, from energy_uj deltas over wall time, is within 5 % of the package
// power the emulator reports. The emulator then closes and a new one
// resumes the tree: the agent's first reading spans the time nothing ran,
// so it is at most 5 % above the package power (no spike from a counter
// that jumped), and its second is within 5 % again.
//
// The files hold the energy up to each tick, and the agent divides it by
// the time between its own clock reads, which fall somewhere inside each
// call. A read that wakes late, or is preempted, on a loaded host changes
// that time, so the package power is scaled by the ticks' interval over the
// shortest and the longest interval the agent can have used, and the
// reading must lie within 5 % of that range.
func TestHwemuA17DVFSPowerHasNoSpikeAcrossAnEmulatorRestart(t *testing.T) {
	m := emulatedPool(t, "{name: intel, profile: intel-xeon-2s-rapl, count: 1, baseLoad: {cpuUtil: 0.6}}")
	root := filepath.Join(t.TempDir(), "hwemu")
	run := startEmulator(t, root, m, time.Now())
	useEmulatedNode(t, run.tree, nil)
	d, err := dvfs.New(dvfs.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	const (
		// The tick of a deployed emulator. The agent's reading is off by
		// the jitter of its wall-clock read over the interval, so a much
		// shorter tick fails on a loaded host.
		tick            = time.Second
		maxLead, giveUp = 2 * time.Second, time.Minute
	)
	lead := tick
	next := time.Now()
	begin := next
	lastW := 0.0
	// The tick of the last reading, and when its call began and ended.
	var readNext, readBegin, readEnd time.Time
	// read ticks run.e at the next aligned time, waits for it and reads the
	// agent's power. It returns the agent's reading and the package power
	// over the same interval, the mean of its two end points, scaled to the
	// longest (lo) and the shortest (hi) interval the agent can have
	// measured; ok is false while the agent has no previous sample.
	read := func() (agentW, lo, hi float64, ok bool) {
		t.Helper()
		for {
			next = next.Add(tick)
			if earliest := time.Now().Add(lead); next.Before(earliest) {
				next = earliest
			}
			if next.Sub(begin) > giveUp {
				t.Fatalf("no reading in %v; the emulator cannot keep up", giveUp)
			}
			snap, err := run.e.Tick(next, nil, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			nowW := packagePower(snap)
			if time.Now().After(next) {
				// The tick ended after the moment it stands for: give the
				// next one more time instead of letting the agent read late.
				lead = min(2*lead, maxLead)
				lastW = nowW
				continue
			}
			time.Sleep(time.Until(next))
			callBegin := time.Now()
			w, ok, err := d.ReadPowerWatts()
			if err != nil {
				t.Fatal(err)
			}
			callEnd := time.Now()
			lo = (lastW + nowW) / 2
			hi = lo
			if !readBegin.IsZero() {
				ticks := next.Sub(readNext).Seconds()
				lo *= ticks / callEnd.Sub(readBegin).Seconds()
				hi *= ticks / callBegin.Sub(readEnd).Seconds()
			}
			lastW, readNext, readBegin, readEnd = nowW, next, callBegin, callEnd
			return w, lo, hi, ok
		}
	}
	within := func(what string, agentW, lo, hi float64) {
		t.Helper()
		t.Logf("%s: agent %.1f W, package %.1f to %.1f W", what, agentW, lo, hi)
		if agentW < 0.95*lo || agentW > 1.05*hi {
			t.Errorf("%s: the agent reads %.1f W, the package power is %.1f to %.1f W; want within 5 %%", what, agentW, lo, hi)
		}
	}

	var agentW, lo, hi float64
	for i := 0; i < 5; i++ {
		agentW, lo, hi, _ = read()
	}
	if lo <= 0 {
		t.Fatalf("package power %.1f W at 0.6 load, want above 0", lo)
	}
	within("before the restart", agentW, lo, hi)

	if err := run.e.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	run = startEmulator(t, root, m, time.Now())
	next = time.Now()
	agentW, lo, hi, ok := read()
	if !ok {
		t.Fatal("no agent reading after the restart")
	}
	t.Logf("first reading after the restart: agent %.1f W, package %.1f to %.1f W", agentW, lo, hi)
	if agentW > 1.05*hi {
		t.Errorf("first reading after the restart: the agent reads %.1f W, the package power is %.1f to %.1f W; want at most 5 %% above (no spike)", agentW, lo, hi)
	}
	agentW, lo, hi, _ = read()
	within("second reading after the restart", agentW, lo, hi)
}

// A18: the H100 SXM pool with persistence mode off (smoke's sxm-nopm),
// idle, capped at 400 W per GPU. Without a client holding the GPUs, NVML
// puts each limit back at its 700 W default at the next tick, as a real GPU
// does when the driver unloads; the agent's next reconcile writes 400 W
// again.
func TestHwemuA18PersistenceOffResetsTheCapAndTheAgentReappliesIt(t *testing.T) {
	m := emulatedPool(t, "{name: sxm-nopm, profile: nvidia-h100-sxm-8gpu, count: 1, overrides: {gpus: {persistenceMode: disabled}}}")
	start := time.Now()
	run := startEmulator(t, filepath.Join(t.TempDir(), "hwemu"), m, start)
	if _, err := run.e.Tick(start.Add(time.Second), nil, true, nil); err != nil {
		t.Fatal(err)
	}
	e := useEmulatedNode(t, run.tree, nil)
	count := m.Profile.GPUs.Count.V
	limits := func() []string {
		out := make([]string, count)
		for k := range out {
			b, err := os.ReadFile(nvmlFile(e, k, "power_limit_mw"))
			if err != nil {
				t.Fatal(err)
			}
			out[k] = strings.TrimSpace(string(b))
		}
		return out
	}
	all := func(got []string, want string) bool {
		for _, v := range got {
			if v != want {
				return false
			}
		}
		return true
	}
	defaultMW := strconv.Itoa(int(math.Round(m.Profile.GPUs.Limits.DefaultW.V * 1000)))
	if got := limits(); !all(got, defaultMW) {
		t.Fatalf("power_limit_mw %v before any cap, want every GPU at its default %s", got, defaultMW)
	}

	nc := e.controller(t)
	node := m.Profile.NodeObject(e.name)
	twin := nodeTwinWithGPUCap(e.name, float64Ptr(400), nil)
	if _, err := reconcileEmulated(t, nc, node, twin); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if got := limits(); !all(got, "400000") {
		t.Fatalf("power_limit_mw %v after the 400 W cap, want 400000 on every GPU", got)
	}

	if _, err := run.e.Tick(start.Add(2*time.Second), nil, true, nil); err != nil {
		t.Fatal(err)
	}
	if got := limits(); !all(got, defaultMW) {
		t.Fatalf("power_limit_mw %v one idle tick later with persistence off, want every GPU back at %s", got, defaultMW)
	}

	e.runner.reset()
	dyn, err := reconcileEmulated(t, nc, node, twin)
	if err != nil {
		t.Fatalf("second reconcileOnce: %v", err)
	}
	if got := limits(); !all(got, "400000") {
		t.Fatalf("the agent did not write the cap again after NVML reset it: power_limit_mw %v, commands %v, gpu status %v",
			got, e.runner.commands(), controlStatus(t, dyn, e.name, "gpu"))
	}
	// The status payload equals the first one, which the agent does not
	// write twice (lastWrites); the commands show the cap went out again.
	for k := 0; k < count; k++ {
		cmd := "nvidia-smi -i " + strconv.Itoa(k) + " -pl 400"
		if _, ok := e.runner.ran(cmd); !ok {
			t.Errorf("%q did not run successfully after the reset; commands: %v", cmd, e.runner.commands())
		}
	}
}
