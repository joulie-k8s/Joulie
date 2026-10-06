package hwemu

import (
	"math"
	"strings"
	"testing"
	"time"
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
