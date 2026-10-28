package hwemu

import (
	"math"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/phys"
)

// nodeFullLoad runs every CPU and GPU flat out.
var nodeFullLoad = Load{CPUUtil: []float64{1}, GPUUtil: []float64{1}}

// nodeSettled is where a cap that starts at from stands after k substeps of
// 100 ms toward to, each a clamped Euler step of 0.1/tau (P02).
func nodeSettled(from, to, tauSec float64, k int) float64 {
	return to + (from-to)*math.Pow(1-math.Min(0.1/tauSec, 1), float64(k))
}

// H06: a PL1 write settles package power to the cap within a few time
// windows, on that package only. The cap settles with the constraint's time
// window as read back from constraint_0_time_window_us, so the drop is not
// instant and a longer window written there slows the next change.
func TestH06PL1WriteCapsOnlyItsPackage(t *testing.T) {
	t.Parallel()
	p := nodeIntelProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(nodeFullLoad)
	nodeStep(t, n, time.Second)
	before := n.State().Packages
	if math.Abs(before[0].PowerW-165) > 0.01 {
		t.Fatalf("uncapped package 0 at full load = %.2f W, want 165 W", before[0].PowerW)
	}

	// One time window of 999424 us is ten substeps: 165 W moves toward 120 W
	// by a factor 1 - 0.1/0.999424 per substep, about 135.7 W. A window five
	// times shorter gives 120.0 W, an exponential 136.5 W.
	nodeWrite(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 0), "120000000")
	nodeStep(t, n, time.Second)
	if got, want := n.State().Packages[0].PowerW, nodeSettled(165, 120, 0.999424, 10); math.Abs(got-want) > 0.05 {
		t.Errorf("package 0 one time window after the write = %.2f W, want %.2f W (tau = constraint_0_time_window_us)", got, want)
	}
	nodeStep(t, n, 5*999424*time.Microsecond-time.Second)
	after := n.State().Packages
	if after[0].PowerW > 121.2 || after[0].PowerW < 119 {
		t.Errorf("package 0 after 5 time windows = %.2f W, want 120 W within 1%%", after[0].PowerW)
	}
	if after[1].PowerW != before[1].PowerW {
		t.Errorf("package 1 = %.2f W, want unchanged %.2f W", after[1].PowerW, before[1].PowerW)
	}

	// A 3 s window written to the file sets tau for the way back up.
	capW := after[0].CapW
	nodeWrite(t, tr, nodeZoneFile("constraint_0_time_window_us", 0), "3000000")
	nodeWrite(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 0), "165000000")
	nodeStep(t, n, time.Second)
	if got, want := n.State().Packages[0].PowerW, nodeSettled(capW, 165, 3, 10); math.Abs(got-want) > 0.05 {
		t.Errorf("package 0 one second after raising PL1 with a 3 s window = %.2f W, want %.2f W", got, want)
	}

	// The same through the measured-curve model, which the simulator's solver
	// never throttles (D13): power follows the cap and the frequency drops.
	t.Run("catalog-curve", func(t *testing.T) {
		p := nodeIntelProfile()
		p.Physics.CPU.Model = "catalog-curve"
		p.CPU.ModelName = nodeF("AMD EPYC 9654 96-Core Processor", "assumed:test fixture, a catalog alias with a node2S curve")
		cs := p.Powercap.ControlTypes[0].Zones[0].Constraints
		cs[0].PowerLimitUW, cs[0].MaxPowerUW = nodeF[int64](500000000, "assumed:above the curve"), nodeF[int64](500000000, "assumed:above the curve")
		cs[1].PowerLimitUW, cs[1].MaxPowerUW = nodeF[int64](600000000, "assumed:above the curve"), nodeF[int64](800000000, "assumed:above the curve")
		tr := nodeRender(t, p, nil)
		n := nodeNewNode(t, p, tr)
		n.SetLoad(nodeFullLoad)
		nodeStep(t, n, time.Second)
		natural := n.State().Packages[0].PowerW
		if natural <= 300 {
			t.Fatalf("natural package power = %.2f W, want above the 300 W cap", natural)
		}
		nodeWrite(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 0), "300000000")
		nodeStep(t, n, 10*time.Second)
		pk := n.State().Packages[0]
		if pk.PowerW > 303 || pk.PowerW < 297 {
			t.Errorf("capped package 0 = %.2f W, want 300 W within 1%%", pk.PowerW)
		}
		if pk.FreqScale >= 0.999 {
			t.Errorf("capped package 0 frequency scale = %.4f, want below 1", pk.FreqScale)
		}
		if cur := nodeReadInt64(t, tr, nodePolicyFile(0, "scaling_cur_freq")); cur >= 3900000 {
			t.Errorf("scaling_cur_freq = %d kHz, want the solved frequency below 3900000", cur)
		}
	})
}

// H20: a PL1 below what the package reaches at its lowest frequency reads back
// as written, and power stays at max(floorW, power at minScale), above the
// limit.
func TestH20CapBelowTheFloorShowsAsPowerAboveTheLimit(t *testing.T) {
	t.Parallel()
	// 40 + 125 * (1000000/3900000)^1.35, by hand
	const powerAtMinScale = 59.905486367040034
	for _, c := range []struct {
		floorW, want float64
	}{
		{0, powerAtMinScale},
		{70, 70},
	} {
		p := nodeIntelProfile()
		p.Physics.RAPL.FloorW = nodeF(c.floorW, "assumed:test value")
		tr := nodeRender(t, p, nil)
		n := nodeNewNode(t, p, tr)
		n.SetLoad(nodeFullLoad)
		nodeWrite(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 0), "30000000")
		nodeStep(t, n, 10*time.Second)
		if got := nodeRead(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 0)); got != "30000000\n" {
			t.Errorf("floorW %v: PL1 reads back %q, want 30000000", c.floorW, got)
		}
		pk := n.State().Packages[0]
		if math.Abs(pk.PowerW-c.want) > 0.01 || pk.PowerW <= 30 {
			t.Errorf("floorW %v: steady power = %.4f W, want %.4f W, above the 30 W limit", c.floorW, pk.PowerW, c.want)
		}
		if math.Abs(pk.FreqScale-1000000.0/3900000) > 1e-9 {
			t.Errorf("floorW %v: frequency scale = %v, want the lowest, %v", c.floorW, pk.FreqScale, 1000000.0/3900000)
		}
	}
}

// H24: with enabled=0 a package's PL1 is not enforced; the other package,
// still enabled, takes the same write.
func TestH24DisabledZoneIgnoresPL1(t *testing.T) {
	t.Parallel()
	p := nodeIntelProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(nodeFullLoad)
	nodeWrite(t, tr, nodeZoneFile("enabled", 0), "0")
	nodeStep(t, n, 0)
	if got := nodeRead(t, tr, nodeZoneFile("enabled", 0)); got != "0\n" {
		t.Fatalf("enabled reads back %q, want 0", got)
	}
	nodeWrite(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 0), "120000000")
	nodeWrite(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 1), "120000000")
	nodeStep(t, n, 10*time.Second)
	pk := n.State().Packages
	if math.Abs(pk[0].PowerW-165) > 0.01 || pk[0].CapW != 0 || pk[0].RequestedCapW != 120 {
		t.Errorf("disabled package 0: %.2f W, cap %v W, requested %v W; want 165 W, no cap, 120 W requested", pk[0].PowerW, pk[0].CapW, pk[0].RequestedCapW)
	}
	if pk[1].PowerW > 121.2 {
		t.Errorf("enabled package 1 = %.2f W, want capped at 120 W", pk[1].PowerW)
	}
}

// H27: acpi-cpufreq under powersave holds scaling_min_freq at any load
// (cpufreq.rst:372-389); intel_pstate powersave follows load.
func TestH27GovernorsSetTheRunningFrequency(t *testing.T) {
	t.Parallel()
	p := nodeEPYCProfile()
	p.CPUFreq.Governor = nodeF("powersave", "assumed:test value")
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(Load{CPUUtil: []float64{1}})
	nodeStep(t, n, 10*time.Second)
	for k := 0; k < 8; k++ {
		if got := nodeRead(t, tr, nodePolicyFile(k, "scaling_cur_freq")); got != "1500000\n" {
			t.Errorf("acpi powersave at full load: policy%d scaling_cur_freq = %q, want scaling_min_freq 1500000", k, got)
		}
	}
	nodeWrite(t, tr, nodePolicyFile(0, "scaling_governor"), "performance")
	nodeStep(t, n, 10*time.Second)
	if got := nodeRead(t, tr, nodePolicyFile(0, "scaling_cur_freq")); got != "2400000\n" {
		t.Errorf("acpi performance: policy0 scaling_cur_freq = %q, want 2400000", got)
	}

	ip := nodeIntelProfile()
	itr := nodeRender(t, ip, nil)
	in := nodeNewNode(t, ip, itr)
	var cur []int64
	for _, u := range []float64{0, 0.5, 1} {
		in.SetLoad(Load{CPUUtil: []float64{u}})
		nodeStep(t, in, 10*time.Second)
		cur = append(cur, nodeReadInt64(t, itr, nodePolicyFile(0, "scaling_cur_freq")))
	}
	if cur[0] != 1000000 || cur[2] != 3900000 || !(cur[0] < cur[1] && cur[1] < cur[2]) {
		t.Errorf("intel_pstate powersave at load 0, 0.5, 1: scaling_cur_freq = %v, want 1000000, between, 3900000", cur)
	}
	if pk := in.State().Packages[0]; math.Abs(pk.PowerW-165) > 0.01 {
		t.Errorf("intel_pstate powersave at full load = %.2f W, want 165 W at full frequency", pk.PowerW)
	}
}

// H27: a policy's frequency counts for the package its CPUs sit on, located
// by numbering. Under socket-major-smt-last with two sockets of two cores and
// two threads, CPUs 2 and 3 are cores of socket 1, so pinning policy2 and
// policy3 to cpuinfo_min_freq, as dvfs throttles the first CPUs by index,
// slows package 1 only. Loads cannot show a wrong mapping: they go through
// the same lookup.
func TestH27PolicyFrequencyReachesThePackageOfItsCPUs(t *testing.T) {
	t.Parallel()
	p := nodeIntelProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(nodeFullLoad)
	for _, k := range []int{2, 3} {
		nodeWrite(t, tr, nodePolicyFile(k, "scaling_max_freq"), "1000000")
	}
	nodeStep(t, n, 10*time.Second)
	pk := n.State().Packages

	// Package 1 runs CPUs 2, 3, 6 and 7; two of them at 1000000 of 3900000 kHz.
	scale := (2*1000000.0/3900000 + 2) / 4
	if math.Abs(pk[1].FreqScale-scale) > 1e-6 {
		t.Errorf("package 1 frequency scale = %.6f, want %.6f from CPUs 2 and 3 at the minimum", pk[1].FreqScale, scale)
	}
	if want := 40 + 125*math.Pow(scale, 1.35); math.Abs(pk[1].PowerW-want) > 0.01 {
		t.Errorf("package 1 = %.2f W, want %.2f W", pk[1].PowerW, want)
	}
	if pk[0].FreqScale != 1 || math.Abs(pk[0].PowerW-165) > 0.01 {
		t.Errorf("package 0: frequency scale %.6f, %.2f W; want 1 and 165 W, untouched", pk[0].FreqScale, pk[0].PowerW)
	}
}

// H11: an NVML limit settles power_draw_mw to it under full load, on that GPU
// only. power_draw_mw is the mean power of the last second (NVML: "averaged
// over 1 sec interval"), not a first-order lag: read on a substep boundary it
// is exactly the mean of the ten substeps before, and it reaches the capped
// power one second after the power itself settled.
func TestH11NVMLLimitSettlesPowerDraw(t *testing.T) {
	t.Parallel()
	p := nodeNVLProfile()
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(Load{GPUUtil: []float64{1}})
	nodeStep(t, n, 10*time.Second)
	uncapped := nodeReadInt64(t, tr, nodeNVMLFile(1, "power_draw_mw"))
	if got := nodeReadInt64(t, tr, nodeNVMLFile(0, "power_draw_mw")); got < 300000 {
		t.Fatalf("uncapped power_draw_mw = %d, want well above the 250 W limit", got)
	}

	// The power of each substep is the one State shows at its start.
	cur := n.State().GPUs[0].PowerW
	window := []float64{cur, cur, cur, cur, cur, cur, cur, cur, cur, cur}
	nodeWrite(t, tr, nodeNVMLFile(0, "power_limit_mw"), "250000")
	settledAt := -1
	for k := 1; k <= 30; k++ {
		nodeStep(t, n, 100*time.Millisecond)
		window = append(window[1:], cur)
		cur = n.State().GPUs[0].PowerW
		if settledAt < 0 && math.Abs(cur-250) < 0.25 {
			settledAt = k
		}
		mean := 0.0
		for _, w := range window {
			mean += w / 10
		}
		if got, want := nodeReadInt64(t, tr, nodeNVMLFile(0, "power_draw_mw")), int64(math.Floor(mean*1000)); got < want-1 || got > want+1 {
			t.Errorf("%d ms after the write: power_draw_mw = %d, want the mean of the last second, %d", 100*k, got, want)
		}
		if settledAt >= 0 && k == settledAt+10 {
			if got := nodeReadInt64(t, tr, nodeNVMLFile(0, "power_draw_mw")); got > 252500 || got < 247500 {
				t.Errorf("1 s after the power settled: power_draw_mw = %d, want 250000 within 1%%", got)
			}
		}
	}
	if settledAt < 0 || settledAt > 10 {
		t.Fatalf("GPU 0 power settled to 250 W after %d substeps, want within one second", settledAt)
	}
	if got := nodeRead(t, tr, nodeNVMLFile(0, "enforced_power_limit_mw")); strings.TrimSpace(got) != "250000" {
		t.Errorf("GPU 0 enforced_power_limit_mw = %q, want 250000", got)
	}
	if got := nodeReadInt64(t, tr, nodeNVMLFile(1, "power_draw_mw")); got < uncapped-100 {
		t.Errorf("GPU 1 power_draw_mw = %d, want unchanged at %d", got, uncapped)
	}
	if got := nodeRead(t, tr, nodeNVMLFile(0, "power_limit_mw")); got != "250000\n" {
		t.Errorf("GPU 0 power_limit_mw = %q, want 250000 kept", got)
	}
}

// A table policy with boost on runs above its top entry up to
// cpuinfo_max_freq at full load; with boost off, or with scaling_max_freq
// below the top entry, it stays on the table. Without this a fully loaded
// EPYC host whose cpuinfo_max_freq includes boost never reaches its rated
// package power.
func TestNodePolicyBoostRunsAboveTheTopTableEntry(t *testing.T) {
	on, off := 1, 0
	p := &nodePolicy{minKHz: 1500000, maxKHz: 3707812, table: []int64{2400000, 1900000, 1500000}, governor: "schedutil", reqMinKHz: 1500000, reqMaxKHz: 3707812, boost: &on}
	if got := p.target(1); got != 3707812 {
		t.Errorf("boost on, full load: target %v kHz, want cpuinfo_max_freq 3707812", got)
	}
	if got := p.target(0); got != 1500000 {
		t.Errorf("boost on, idle: target %v kHz, want the 1500000 minimum", got)
	}
	p.boost = &off
	if got := p.target(1); got != 2400000 {
		t.Errorf("boost off, full load: target %v kHz, want the 2400000 top entry", got)
	}
	p.boost = &on
	p.reqMaxKHz = 1900000
	if got := p.target(1); got != 1900000 {
		t.Errorf("scaling_max_freq 1900000, full load: target %v kHz, want 1900000 with no boost", got)
	}
}

// With the governor facts the governor sets a table entry, and the policy
// runs above the top entry only when that entry is set, boost is on and
// scaling_max_freq allows the entry.
func TestNodePolicyBoostsOnlyFromTheTopEntryTheGovernorSets(t *testing.T) {
	on, off := 1, 0
	p := &nodePolicy{minKHz: 1500000, maxKHz: 3707812, table: []int64{2400000, 1900000, 1500000}, governor: "schedutil", reqMinKHz: 1500000, reqMaxKHz: 3707812, boost: &on}
	if got := p.boostedKHz(2400000); got != 3707812 {
		t.Errorf("boost on, top entry set: runs at %v kHz, want cpuinfo_max_freq 3707812", got)
	}
	if got := p.boostedKHz(1900000); got != 1900000 {
		t.Errorf("boost on, middle entry set: runs at %v kHz, want 1900000", got)
	}
	p.boost = &off
	if got := p.boostedKHz(2400000); got != 2400000 {
		t.Errorf("boost off, top entry set: runs at %v kHz, want 2400000", got)
	}
	p.boost = &on
	p.reqMaxKHz = 1900000
	if got := p.boostedKHz(1900000); got != 1900000 {
		t.Errorf("scaling_max_freq 1900000, its entry set: runs at %v kHz, want 1900000 with no boost", got)
	}
}

// physicsNode renders the builtin profile name, cut to two cores per
// socket, after mutate changed it, and starts its node.
func physicsNode(t *testing.T, name string, mutate func(*Profile)) (*Node, *Tree, *Profile) {
	t.Helper()
	p := ensureBuiltin(t, name)
	if mutate != nil {
		mutate(p)
	}
	tr := renderTestTree(t, p)
	return nodeNewNode(t, p, tr), tr, p
}

// physicsGovernor sets the profile's governor.
func physicsGovernor(gov string) func(*Profile) {
	return func(p *Profile) { p.CPUFreq.Governor = Fact[string]{V: gov, Src: "assumed:test fixture"} }
}

// physicsWriteGovernor writes gov to every policy's scaling_governor.
func physicsWriteGovernor(t *testing.T, n *Node, tr *Tree, gov string) {
	t.Helper()
	for _, pol := range n.policies {
		nodeWrite(t, tr, nodePolicyFile(pol.index, "scaling_governor"), gov)
	}
}

// CG15: schedutil on the acpi-cpufreq EPYC, a three-entry table, at a
// demand of 0.3 asks 1.25 x 0.3 x cpuinfo_max_freq, below the lowest entry,
// and sits there. Work is counted in cycles: the same work at 1.5 GHz keeps
// the CPUs busy 0.3 x cpuinfo_max_freq / 1500000 of the time, which the
// package reports as its utilization and the governor reads back, so the
// request stays where it is.
func TestCG15SchedutilAtLightLoadSitsAtTheLowestEntryAndRunsBusier(t *testing.T) {
	t.Parallel()
	n, tr, p := physicsNode(t, "amd-epyc-2s-energy-only", nil)
	cf := p.CPUFreq
	if cf.Governor.V != "schedutil" || len(cf.AvailableKHz.V) != 3 {
		t.Fatalf("%s: governor %s, table %v; the case needs schedutil on a three-entry table", p.Name, cf.Governor.V, cf.AvailableKHz.V)
	}
	lowest := cf.AvailableKHz.V[0]
	for _, f := range cf.AvailableKHz.V {
		lowest = min(lowest, f)
	}
	const demand = 0.3
	if ask := 1.25 * demand * float64(cf.CPUInfoMaxKHz.V); ask >= float64(lowest) {
		t.Fatalf("schedutil asks %.0f kHz at %v, not below the lowest entry %d", ask, demand, lowest)
	}
	n.SetLoad(Load{CPUUtil: []float64{demand}})
	nodeStep(t, n, 10*time.Second)
	for _, pol := range n.policies {
		if got := nodeReadInt64(t, tr, nodePolicyFile(pol.index, "scaling_cur_freq")); got != lowest {
			t.Errorf("policy%d scaling_cur_freq = %d, want the lowest entry %d", pol.index, got, lowest)
		}
	}
	want := demand * float64(cf.CPUInfoMaxKHz.V) / float64(lowest)
	for s := 0; s < p.CPU.Sockets.V; s++ {
		if got := stateComponent(t, n.State(), stateID(semconv.HwTypeCPU, s)).Utilization; math.Abs(got-want) > 1e-9 || got <= demand {
			t.Errorf("package %d utilization %v, want %v, above the demand %v", s, got, want, demand)
		}
	}
}

// CG16: a fixed compute job on the HWP profile intel-xeon-2s-rapl, under
// performance and under powersave with EPP 255 (intel_pstate in active mode
// has no generic powersave). Its energy is the node's power once settled
// times the work over the throughput. Race to idle wins when the idle share
// of the power is high; with the shipped parameters (idle 40 of 165 W,
// betaFreq 1.43) the job uses 3.4 % less energy at EPP 255, which this test pins,
// and with the package idle raised to 140 W performance uses less.
func TestCG16FixedComputeJobEnergyUnderPerformanceAndEPP255(t *testing.T) {
	t.Parallel()
	const class, work = "cpu.compute_bound", 100.0 // seconds at the reference speed
	energy := func(t *testing.T, mutate func(*Profile)) float64 {
		t.Helper()
		n, _, p := physicsNode(t, "intel-xeon-2s-rapl", mutate)
		n.SetLoad(Load{CPUUtil: []float64{1}, CPUClass: class})
		nodeStep(t, n, 20*time.Second)
		var cpus []int
		for c := range n.cpuUtil {
			cpus = append(cpus, c)
		}
		thr := n.CPUThroughput(cpus, class, 0, 0)
		w := n.State().Host.PowerW
		t.Logf("%s, governor %s: %.2f W at throughput %.4f, %.0f J", p.Name, p.CPUFreq.Governor.V, w, thr, w*work/thr)
		return w * work / thr
	}
	performance := physicsGovernor("performance")
	epp255 := func(p *Profile) {
		if p.CPUFreq.Governor.V != "powersave" || !p.CPUFreq.HWP.V || p.CPUFreq.EPP == nil {
			t.Fatalf("%s: governor %s, hwp %v; the case needs powersave with HWP and EPP", p.Name, p.CPUFreq.Governor.V, p.CPUFreq.HWP.V)
		}
		p.CPUFreq.EPP.Default = Fact[string]{V: "power", Src: "assumed:test fixture, EPP 255"}
	}
	if perf, low := energy(t, performance), energy(t, epp255); perf <= low {
		t.Errorf("shipped profile: %.0f J under performance, %.0f J at EPP 255; want EPP 255 lower, the sign the shipped parameters give", perf, low)
	}
	idle := func(p *Profile) {
		p.Physics.CPU.IdlePkgW = Fact[float64]{V: 140, Src: "assumed:test fixture, a high idle share"}
	}
	perf := energy(t, func(p *Profile) { performance(p); idle(p) })
	low := energy(t, func(p *Profile) { epp255(p); idle(p) })
	if perf >= low {
		t.Errorf("idle 140 of 165 W: %.0f J under performance, %.0f J at EPP 255; want performance lower (race to idle)", perf, low)
	}
}

// CG17: an idle package draws the same power under every governor while
// idleGovernorDeltaW is 0; with 5 W it draws 5 W more under performance
// only, the one governor that holds the top frequency at idle.
func TestCG17IdleGovernorDeltaOnlyUnderPerformance(t *testing.T) {
	t.Parallel()
	for _, delta := range []float64{0, 5} {
		for _, gov := range []string{"performance", "powersave", "schedutil", "ondemand", "conservative"} {
			t.Run(gov+"/"+strconv.FormatFloat(delta, 'f', -1, 64), func(t *testing.T) {
				t.Parallel()
				n, _, p := physicsNode(t, "amd-epyc-2s-energy-only", func(p *Profile) {
					p.Physics.CPU.Model = "analytic" // idlePkgW, in place of the catalog curve's idle
					physicsGovernor(gov)(p)
					p.Physics.CPU.IdleGovernorDeltaW = Fact[float64]{V: delta, Src: "assumed:test fixture"}
				})
				nodeStep(t, n, 10*time.Second)
				want := p.Physics.CPU.IdlePkgW.V
				if gov == "performance" {
					want += delta
				}
				for _, pk := range n.State().Packages {
					if math.Abs(pk.PowerW-want) > 1e-9 {
						t.Errorf("package %d idle under %s with idleGovernorDeltaW %v: %v W, want %v W", pk.Socket, gov, delta, pk.PowerW, want)
					}
					if got := n.IdleCPUPowerW(pk.Socket); math.Abs(got-want) > 1e-9 {
						t.Errorf("package %d: IdleCPUPowerW %v, want %v", pk.Socket, got, want)
					}
				}
			})
		}
	}
}

// CG18: a binding PL1 wins over every governor's target. At a demand of 0.6
// on the 6530 every governor that runs the package above 120 W (performance,
// schedutil, ondemand, conservative) settles at a 120 W cap on package 0,
// with scaling_cur_freq below the frequency the governor asks, while
// package 1, uncapped, keeps its natural power.
func TestCG18BindingRAPLCapWinsOverEveryGovernorTarget(t *testing.T) {
	t.Parallel()
	const capW = 120
	for _, gov := range []string{"performance", "schedutil", "ondemand", "conservative"} {
		t.Run(gov, func(t *testing.T) {
			t.Parallel()
			n, tr, _ := physicsNode(t, "intel-xeon-6530-2s-rapl", physicsGovernor(gov))
			n.SetLoad(Load{CPUUtil: []float64{0.6}})
			nodeStep(t, n, 10*time.Second)
			if pk := n.State().Packages[0]; pk.PowerW <= capW+5 {
				t.Fatalf("package 0 under %s at 0.6 draws %v W uncapped; the case needs more than %d W", gov, pk.PowerW, capW)
			}
			nodeWrite(t, tr, nodeZoneFile("constraint_0_power_limit_uw", 0), strconv.Itoa(capW*1000000))
			nodeStep(t, n, 30*time.Second)
			pk := n.State().Packages
			if math.Abs(pk[0].PowerW-capW) > 0.05 {
				t.Errorf("package 0 under %s with a %d W PL1: %v W, want the cap", gov, capW, pk[0].PowerW)
			}
			if pk[1].PowerW <= capW+5 {
				t.Errorf("package 1, uncapped, under %s: %v W, want its natural power above %d W", gov, pk[1].PowerW, capW)
			}
			for _, pol := range n.policies {
				if pol.socket != 0 {
					continue
				}
				if got := float64(nodeReadInt64(t, tr, nodePolicyFile(pol.index, "scaling_cur_freq"))); got >= pol.targetKHz {
					t.Errorf("policy%d under %s, capped: scaling_cur_freq %v kHz, want below the governor's %v kHz", pol.index, gov, got, pol.targetKHz)
				}
			}
		})
	}
}

// CG19: State's GovernorPreset is the CPU preset whose plan gives the
// governor the policies run, through governor.ReverseCPU, and only when
// exactly one preset does: on acpi-cpufreq schedutil is balanced and
// conservative efficiency, while a hand-written ondemand, which no preset
// writes, leaves it empty with the governor still in GovernorNative.
func TestCG19GovernorPresetOnlyWhenTheReverseMappingIsUnique(t *testing.T) {
	t.Parallel()
	n, tr, p := physicsNode(t, "amd-epyc-2s-energy-only", nil)
	if p.CPUFreq.Driver.V != "acpi-cpufreq" || p.CPUFreq.Governor.V != "schedutil" {
		t.Fatalf("%s: driver %s, governor %s; the case needs acpi-cpufreq under schedutil", p.Name, p.CPUFreq.Driver.V, p.CPUFreq.Governor.V)
	}
	for _, step := range []struct{ write, native, preset string }{
		{"", "schedutil", "balanced"},
		{"conservative", "conservative", "efficiency"},
		{"ondemand", "ondemand", ""},
		{"performance", "performance", "performance"},
	} {
		if step.write != "" {
			physicsWriteGovernor(t, n, tr, step.write)
		}
		nodeStep(t, n, time.Second)
		for s := 0; s < p.CPU.Sockets.V; s++ {
			if c := stateComponent(t, n.State(), stateID(semconv.HwTypeCPU, s)); c.GovernorNative != step.native || c.GovernorPreset != step.preset {
				t.Errorf("after writing %q: package %d governor %q, preset %q; want %q and %q", step.write, s, c.GovernorNative, c.GovernorPreset, step.native, step.preset)
			}
		}
	}
}

// XF10: an idle GPU draws dataPathHoldW more with its data path held at its
// maximum, and gatingOffIdleW more with idle gating off: on the W7900,
// sustained (level high) adds the first to auto and peak (profile_peak)
// adds the second on top; on the L40S, sustained (a memory lock at its
// maximum) adds the first.
func TestXF10IdlePowerOfHeldDataPathAndGatingOff(t *testing.T) {
	t.Parallel()
	t.Run("w7900", func(t *testing.T) {
		t.Parallel()
		n, tr, p := physicsNode(t, w7900Profile, nil)
		ph := p.Physics.GPU
		if ph.DataPathHoldW.V <= 0 || ph.GatingOffIdleW.V <= 0 {
			t.Fatalf("%s: dataPathHoldW %v, gatingOffIdleW %v; the case needs both above 0", p.Name, ph.DataPathHoldW.V, ph.GatingOffIdleW.V)
		}
		nodeStep(t, n, 2*time.Second)
		auto := n.State().GPUs[1].PowerW
		if math.Abs(auto-ph.IdleW.V) > 1e-9 {
			t.Fatalf("idle GPU 1 under auto: %v W, want idleW %v", auto, ph.IdleW.V)
		}
		for _, step := range []struct {
			level string
			want  float64
		}{
			{"high", auto + ph.DataPathHoldW.V},
			{"profile_peak", auto + ph.DataPathHoldW.V + ph.GatingOffIdleW.V},
			{"auto", auto},
		} {
			dpmSetLevel(t, tr, n, 1, step.level)
			if got := n.State().GPUs[1].PowerW; math.Abs(got-step.want) > 1e-9 {
				t.Errorf("idle GPU 1 at %s: %v W, want %v W", step.level, got, step.want)
			}
			if got := n.State().GPUs[0].PowerW; math.Abs(got-auto) > 1e-9 {
				t.Errorf("idle GPU 0, untouched, while GPU 1 is at %s: %v W, want %v W", step.level, got, auto)
			}
		}
	})
	t.Run("l40s", func(t *testing.T) {
		t.Parallel()
		n, tr, p := physicsNode(t, "nvidia-l40s-4gpu", nil)
		ph, mem := p.Physics.GPU, p.GPUs.Clocks.Mem.MaxMHz.V
		nodeStep(t, n, time.Second)
		auto := n.State().GPUs[1].PowerW
		lock := path.Join(layout.StateDir, layout.NVMLGPUDir(1), layout.NVMLLockedMemClocksFile)
		nodeWrite(t, tr, lock, strconv.FormatInt(mem, 10)+","+strconv.FormatInt(mem, 10))
		nodeStep(t, n, time.Second)
		if got, want := n.State().GPUs[1].PowerW, auto+ph.DataPathHoldW.V; ph.DataPathHoldW.V <= 0 || math.Abs(got-want) > 1e-9 {
			t.Errorf("idle GPU 1 with its memory locked at %d MHz: %v W, want %v W (dataPathHoldW %v)", mem, got, want, ph.DataPathHoldW.V)
		}
	})
}

// A graphics clock lock below the maximum is the compute ceiling the GPU
// runs under: a busy L40S locked at 75 % of its clock draws and works as the
// phys clock model gives for that ceiling, below its uncapped power and
// throughput, while an unlocked GPU of the same node runs as the power
// model alone says.
func TestGPUClockLockLowersPowerAndThroughputThroughTheClockModel(t *testing.T) {
	t.Parallel()
	n, tr, p := physicsNode(t, "nvidia-l40s-4gpu", nil)
	maxMHz := p.GPUs.Clocks.SM.MaxMHz.V
	ceil := maxMHz * 3 / 4
	lock := path.Join(layout.StateDir, layout.NVMLGPUDir(0), layout.NVMLLockedGPUClocksFile)
	nodeWrite(t, tr, lock, strconv.FormatInt(p.GPUs.Clocks.SM.MinMHz.V, 10)+","+strconv.FormatInt(ceil, 10))
	n.SetLoad(Load{GPUUtil: []float64{1}, GPUClass: "gpu.compute_bound", MemoryIntensity: 0.2})
	nodeStep(t, n, 5*time.Second)

	m := n.gpuModel.(phys.CappedBoardGPUModel)
	f := n.gpuClockFacts()
	locked, free := n.gpus[0], n.gpus[1]
	clk := phys.GPUClock{ComputeMHz: float64(ceil), ComputeMaxMHz: float64(maxMHz), DataPath: phys.DataPathMax, DataPathAuto: true}
	s := n.gpuDeviceState(locked)
	if got, want := locked.pwr.W, phys.GPUPowerWithClock(m, s, clk, f); math.Abs(got-want) > 1e-9 || got >= free.pwr.W {
		t.Errorf("GPU 0 locked at %d MHz: %v W, want %v W, below the unlocked GPU's %v W", ceil, got, want, free.pwr.W)
	}
	if got, want := n.GPUThroughput(0), phys.GPUThroughputWithClock(m, s, clk, f, "gpu.compute_bound"); math.Abs(got-want) > 1e-9 || got >= 1 {
		t.Errorf("GPU 0 locked at %d MHz: throughput %v, want %v, below 1", ceil, got, want)
	}
	if got, want := free.pwr.W, m.Power(n.gpuDeviceState(free)); math.Abs(got-want) > 1e-9 {
		t.Errorf("GPU 1 unlocked: %v W, want the power model's %v W", got, want)
	}
	if got := n.GPUThroughput(1); math.Abs(got-1) > 1e-9 {
		t.Errorf("GPU 1 unlocked: throughput %v, want 1", got)
	}
}

// The EPYC 9654 hosts run the SPECpower curve of the catalog's
// AMD_EPYC_9654 entry (SPECpower_ssj2008 result 01203), split over the two
// packages: under performance the packages draw the curve's 128 W idle,
// 257 W at 10 % and 817 W at full load, where the analytic model of the
// same facts drew 80, 125.3 and 720 W, adding 7.1 % of the range at 10 %
// load against the curve's 18.7 %.
func TestEPYC9654RunsItsSPECpowerCurve(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"amd-epyc-2s-energy-only", "amd-instinct-mi300x-8gpu"} {
		n, _, _ := physicsNode(t, name, physicsGovernor("performance"))
		for _, c := range []struct{ util, want float64 }{{0, 128}, {0.1, 257}, {1, 817}} {
			n.SetLoad(Load{CPUUtil: []float64{c.util}})
			nodeStep(t, n, time.Minute)
			got := 0.0
			for _, pk := range n.State().Packages {
				got += pk.PowerW
			}
			if math.Abs(got-c.want) > 0.5 {
				t.Errorf("%s at load %.1f: packages draw %.1f W, want the curve's %.0f W", name, c.util, got, c.want)
			}
		}
	}
}
