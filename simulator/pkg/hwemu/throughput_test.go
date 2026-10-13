package hwemu_test

import (
	"context"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
	"github.com/matbun/joulie/simulator/pkg/phys"
)

// These tests drive the throughput methods the way the simulator's workload
// model reads them: the agent's controls go in through files and tools, and
// the methods report what those controls cost a job.

// throughputProfile is the powersave intel_pstate machine the CPU tests use:
// one policy per CPU, 1.0 to 3.9 GHz, an analytic CPU model, RAPL zones.
const throughputProfile = "intel-xeon-2s-rapl"

// throughputTol is how close two throughputs must be to count as equal: far
// below any effect of a control, far above float rounding.
const throughputTol = 1e-9

// TestGPUThroughputOneUntilCapBinds: GPU 0 of an H100 NVL node at full load
// keeps throughput 1 while its enforced limit, set with nvidia-smi -pl, is at
// or above the power it draws uncapped, and falls below 1, lower the lower
// the limit, once the limit binds. GPU 1, which nobody capped, stays at 1.
func TestGPUThroughputOneUntilCapBinds(t *testing.T) {
	t.Parallel()
	n, tree, _ := hwemutest.NewNode(t, "nvidia-h100-nvl-8gpu", nil)
	n.SetLoad(hwemu.Load{GPUUtil: []float64{1}})
	throughputStep(t, n, 5*time.Second)
	natural := n.State().GPUs[0].PowerW
	if lim := n.State().GPUs[0].EnforcedLimitW; natural >= lim-1 {
		t.Fatalf("GPU 0 draws %.3f W at full load under a %.0f W limit; the test needs headroom to set a limit that does not bind", natural, lim)
	}
	if got := n.GPUThroughput(0); got != 1 {
		t.Fatalf("GPU 0 throughput uncapped = %v, want 1", got)
	}

	runner := smi.Runner{Env: smi.Env{SysRoot: tree.SysRoot, StateRoot: tree.StateRoot}}
	prev := 1.0
	bound := 0
	for _, w := range []int{400, int(math.Ceil(natural)) + 1, int(math.Floor(natural)) - 1, 300, 200} {
		if out, err := runner.Run(context.Background(), "nvidia-smi", "-i", "0", "-pl", strconv.Itoa(w)); err != nil {
			t.Fatalf("nvidia-smi -i 0 -pl %d: %v (%q)", w, err, out)
		}
		throughputStep(t, n, 5*time.Second)
		g := n.State().GPUs[0]
		if math.Abs(g.EnforcedLimitW-float64(w)) > 1e-3 {
			t.Fatalf("enforced limit %.6f W five seconds after nvidia-smi -pl %d", g.EnforcedLimitW, w)
		}
		got := n.GPUThroughput(0)
		switch {
		case float64(w) >= natural:
			if got != 1 {
				t.Errorf("limit %d W at or above the %.3f W natural power: throughput %v, want 1", w, natural, got)
			}
		default:
			bound++
			if got <= 0 || got >= 1 || got > prev {
				t.Errorf("limit %d W below the %.3f W natural power: throughput %v, want in (0, 1) and at most %v from the higher limit", w, natural, got, prev)
			}
		}
		prev = got
		if other := n.GPUThroughput(1); other != 1 {
			t.Errorf("limit %d W on GPU 0: GPU 1 throughput %v, want 1", w, other)
		}
	}
	if bound < 3 {
		t.Fatalf("only %d limits bound; the test needs several to mean anything", bound)
	}
	if got := n.GPUThroughput(99); got != 1 {
		t.Errorf("throughput of a GPU the node does not have = %v, want 1", got)
	}
}

// TestGPUThroughputFollowsTheEnforcedLimitAndTheLoad: GPUThroughput reads
// the GPU model at the inputs the power derivation uses: each GPU's own
// utilization, the load's GPU class and memory intensity, and the limit the
// GPU enforces, which settles toward a new nvidia-smi -pl over a few hundred
// ms. Each input is checked to change the expected value, so a method that
// took the requested limit, utilization 1 or the default class would fail.
func TestGPUThroughputFollowsTheEnforcedLimitAndTheLoad(t *testing.T) {
	t.Parallel()
	n, tree, p := hwemutest.NewNode(t, "nvidia-h100-nvl-8gpu", nil)
	const class, mem, limW = "gpu.memory_bound", 0.4, 200
	utils := []float64{1, 0.85}
	n.SetLoad(hwemu.Load{GPUUtil: utils, GPUClass: class, MemoryIntensity: mem})
	throughputStep(t, n, 5*time.Second)
	runner := smi.Runner{Env: smi.Env{SysRoot: tree.SysRoot, StateRoot: tree.StateRoot}}
	if out, err := runner.Run(context.Background(), "nvidia-smi", "-pl", strconv.Itoa(limW)); err != nil {
		t.Fatalf("nvidia-smi -pl %d: %v (%q)", limW, err, out)
	}
	ph := p.Physics.GPU
	m := phys.CappedBoardGPUModel{IdleW: ph.IdleW.V, MaxW: ph.NaturalMaxW.V, ComputeGamma: ph.ComputeGamma.V, MemoryEpsilon: ph.MemoryEpsilon.V, MemoryGamma: ph.MemoryGamma.V}
	tm := func(util, capW float64, class string, mem float64) float64 {
		return m.ThroughputMultiplier(phys.DeviceState{Utilization: util, CapWatts: capW, MemoryIntensity: mem, Class: class}, class)
	}

	for _, step := range []struct {
		when     string
		d        time.Duration
		settling bool
	}{
		{"100 ms after the limit", 100 * time.Millisecond, true},
		{"5 s after the limit", 5 * time.Second, false},
	} {
		throughputStep(t, n, step.d)
		for i, u := range utils {
			enforced := n.State().GPUs[i].EnforcedLimitW
			if settled := math.Abs(enforced-limW) < 1e-3; settled == step.settling {
				t.Fatalf("%s: GPU %d enforces %.3f W under a %d W limit; want it settled %v", step.when, i, enforced, limW, !step.settling)
			}
			want := tm(u, enforced, class, mem)
			if want >= 1 {
				t.Fatalf("%s: GPU %d expected throughput %v; the case needs a limit that binds", step.when, i, want)
			}
			alts := map[string]float64{"the default class and no memory intensity": tm(u, enforced, "", 0)}
			if step.settling {
				alts["the requested limit"] = tm(u, limW, class, mem)
			}
			if u != 1 {
				alts["utilization 1"] = tm(1, enforced, class, mem)
			}
			for what, v := range alts {
				if math.Abs(v-want) < 1e-6 {
					t.Fatalf("%s: GPU %d gives throughput %v at %s too; the case cannot tell them apart", step.when, i, want, what)
				}
			}
			if got := n.GPUThroughput(i); math.Abs(got-want) > throughputTol {
				t.Errorf("%s: GPU %d at util %v, class %s, memory %v, enforced limit %.3f W: throughput %v, want %v", step.when, i, u, class, mem, enforced, got, want)
			}
		}
	}
}

// TestCPUThroughputFollowsPackageCap: a PL1 limit on package 0 below its
// natural power lowers the throughput of a job on socket 0 to TM at the
// package's capped frequency scale over TM at full frequency, and lower for a
// lower limit, while socket 1, uncapped, stays at 1.
func TestCPUThroughputFollowsPackageCap(t *testing.T) {
	t.Parallel()
	n, tree, p := hwemutest.NewNode(t, throughputProfile, nil)
	throughputAnalytic(t, p)
	const class, mem, io = "cpu.compute_bound", 0.2, 0.1
	socket0, socket1 := throughputSocketCPUs(p, 0), throughputSocketCPUs(p, 1)
	n.SetLoad(hwemu.Load{CPUUtil: []float64{1}})
	throughputStep(t, n, 20*time.Second)
	for s, cpus := range [][]int{socket0, socket1} {
		if got := n.CPUThroughput(cpus, class, mem, io); math.Abs(got-1) > throughputTol {
			t.Fatalf("socket %d uncapped at full load: throughput %v, want 1", s, got)
		}
	}

	pl1 := path.Join(layout.PowercapZoneDir("intel-rapl", 0), "constraint_0_power_limit_uw")
	prev := 1.0
	for _, w := range []int{120, 90} {
		throughputWrite(t, tree, pl1, strconv.Itoa(w*1000000))
		throughputStep(t, n, 30*time.Second)
		pk := n.State().Packages[0]
		if math.Abs(pk.CapW-float64(w)) > 1e-6 || pk.FreqScale >= 1 {
			t.Fatalf("package 0 under a %d W PL1: cap %v W, frequency scale %v; want the cap in effect and a scale below 1", w, pk.CapW, pk.FreqScale)
		}
		want := throughputTM(p, pk.FreqScale, class, mem, io) / throughputTM(p, 1, class, mem, io)
		got := n.CPUThroughput(socket0, class, mem, io)
		if math.Abs(got-want) > throughputTol || got >= prev {
			t.Errorf("socket 0 under a %d W PL1: throughput %v, want TM(%.6f)/TM(1) = %v, below %v", w, got, pk.FreqScale, want, prev)
		}
		prev = got
		if other := n.CPUThroughput(socket1, class, mem, io); math.Abs(other-1) > throughputTol {
			t.Errorf("socket 1, uncapped, while socket 0 is under %d W: throughput %v, want 1", w, other)
		}
	}
}

// TestCPUThroughputIsOneUncappedUnderPowersave: with no control, a job runs
// as fast as the governor lets it, so its throughput is 1 at any load, though
// powersave holds a lightly loaded intel_pstate CPU far below its maximum,
// and acpi-cpufreq powersave holds every CPU at its minimum. TM at the
// package's own scale, the plain implementation, is well below 1 there.
func TestCPUThroughputIsOneUncappedUnderPowersave(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, profile string
		mutate        func(*hwemu.Profile)
	}{
		{name: "intel_pstate", profile: throughputProfile},
		{name: "acpi-cpufreq", profile: "amd-epyc-2s-energy-only", mutate: func(p *hwemu.Profile) {
			p.CPUFreq.Governor = hwemu.Fact[string]{V: "powersave", Src: "assumed:test fixture"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, _, p := hwemutest.NewNode(t, tc.profile, tc.mutate)
			throughputAnalytic(t, p)
			if g := p.CPUFreq.Governor.V; g != "powersave" {
				t.Fatalf("%s runs governor %s, want powersave", tc.profile, g)
			}
			all := append(throughputSocketCPUs(p, 0), throughputSocketCPUs(p, 1)...)
			for _, u := range []float64{0, 0.038, 0.5, 1} {
				n.SetLoad(hwemu.Load{CPUUtil: []float64{u}})
				throughputStep(t, n, 20*time.Second)
				for _, class := range []string{"", "cpu.compute_bound", "cpu.memory_bound", "cpu.io_bound"} {
					if got := n.CPUThroughput(all, class, 0, 0); math.Abs(got-1) > throughputTol {
						t.Errorf("util %v, class %q, uncapped: throughput %v, want 1", u, class, got)
					}
				}
				if u != 0.038 {
					continue
				}
				scale := n.State().Packages[0].FreqScale
				if plain := throughputTM(p, scale, "", 0, 0); plain > 0.6 {
					t.Fatalf("util %v: TM at the package scale %.4f is %.4f; the case needs it well below 1 to tell a ratio from a plain TM", u, scale, plain)
				}
			}
		})
	}
}

// TestCPUThroughputFollowsScalingMaxFreq: scaling_max_freq written at 2.0
// GHz on CPUs 0 to 3 lowers their throughput at full load to TM(2.0/3.9)
// over TM(1) and leaves CPU 4 at 1. At light load intel_pstate powersave
// scales between cpuinfo_min_freq and the limit, so the limit still costs
// throughput there, less: the reference is the governor's target at the same
// load without the limit, not cpuinfo_max_freq.
func TestCPUThroughputFollowsScalingMaxFreq(t *testing.T) {
	t.Parallel()
	n, tree, p := hwemutest.NewNode(t, throughputProfile, nil)
	throughputAnalytic(t, p)
	const limKHz = 2000000
	maxKHz := p.CPUFreq.CPUInfoMaxKHz.V
	for c := 0; c < 4; c++ {
		throughputWrite(t, tree, path.Join("devices/system/cpu", "cpu"+strconv.Itoa(c), "cpufreq", "scaling_max_freq"), strconv.Itoa(limKHz))
	}

	n.SetLoad(hwemu.Load{CPUUtil: []float64{1}})
	throughputStep(t, n, 20*time.Second)
	want := throughputTM(p, float64(limKHz)/float64(maxKHz), "", 0, 0) / throughputTM(p, 1, "", 0, 0)
	if want >= 1 {
		t.Fatalf("TM(%d/%d)/TM(1) = %v; the case needs a limit that costs throughput", limKHz, maxKHz, want)
	}
	for c := 0; c < 4; c++ {
		if got := n.CPUThroughput([]int{c}, "", 0, 0); math.Abs(got-want) > throughputTol {
			t.Errorf("CPU %d at %d kHz scaling_max_freq, full load: throughput %v, want %v", c, limKHz, got, want)
		}
	}
	if got := n.CPUThroughput([]int{4}, "", 0, 0); math.Abs(got-1) > throughputTol {
		t.Errorf("CPU 4, not limited: throughput %v, want 1", got)
	}
	if got, mean := n.CPUThroughput([]int{0, 4}, "", 0, 0), (want+1)/2; math.Abs(got-mean) > throughputTol {
		t.Errorf("CPUs 0 and 4: throughput %v, want their mean %v", got, mean)
	}
	if got := n.CPUThroughput(nil, "", 0, 0); got != 1 {
		t.Errorf("no CPUs: throughput %v, want 1", got)
	}

	// At 3.8 % load powersave asks for min + u*(max - min): about 1.04 GHz
	// under the limit, 1.11 GHz without it.
	const u = 0.038
	n.SetLoad(hwemu.Load{CPUUtil: []float64{u}})
	throughputStep(t, n, 20*time.Second)
	minKHz := float64(p.CPUFreq.CPUInfoMinKHz.V)
	limited := (minKHz + u*(limKHz-minKHz)) / float64(maxKHz)
	free := (minKHz + u*(float64(maxKHz)-minKHz)) / float64(maxKHz)
	light := throughputTM(p, limited, "", 0, 0) / throughputTM(p, free, "", 0, 0)
	if got := n.CPUThroughput([]int{0, 1, 2, 3}, "", 0, 0); math.Abs(got-light) > throughputTol || got <= want || got >= 1 {
		t.Errorf("CPUs 0 to 3 at %v load: throughput %v, want TM(%.6f)/TM(%.6f) = %v, between the full-load %v and 1", u, got, limited, free, light, want)
	}
	if got := n.CPUThroughput([]int{4}, "", 0, 0); math.Abs(got-1) > throughputTol {
		t.Errorf("CPU 4, not limited, at %v load: throughput %v, want 1", u, got)
	}
}

// TestIdleCPUPowerWIsThePackagePowerAtZeroLoad: IdleCPUPowerW is what a
// package draws once its load is gone, and it stays that value under load
// and under a package cap. A package the node does not have gives 0.
func TestIdleCPUPowerWIsThePackagePowerAtZeroLoad(t *testing.T) {
	t.Parallel()
	n, tree, p := hwemutest.NewNode(t, throughputProfile, nil)
	throughputAnalytic(t, p)
	idle := p.Physics.CPU.IdlePkgW.V
	throughputStep(t, n, 5*time.Second)
	for _, pk := range n.State().Packages {
		if got := n.IdleCPUPowerW(pk.Socket); math.Abs(got-pk.PowerW) > throughputTol || math.Abs(got-idle) > throughputTol {
			t.Errorf("package %d at zero load draws %v W; IdleCPUPowerW = %v, want both %v", pk.Socket, pk.PowerW, got, idle)
		}
	}
	n.SetLoad(hwemu.Load{CPUUtil: []float64{1}})
	throughputWrite(t, tree, path.Join(layout.PowercapZoneDir("intel-rapl", 0), "constraint_0_power_limit_uw"), "90000000")
	throughputStep(t, n, 30*time.Second)
	if pk := n.State().Packages[0]; pk.PowerW <= idle || math.Abs(pk.CapW-90) > 1e-6 {
		t.Fatalf("package 0 at full load under 90 W draws %v W with cap %v W; want above idle and capped", pk.PowerW, pk.CapW)
	}
	for s := 0; s < 2; s++ {
		if got := n.IdleCPUPowerW(s); math.Abs(got-idle) > throughputTol {
			t.Errorf("package %d at full load: IdleCPUPowerW = %v, want %v", s, got, idle)
		}
	}
	if got := n.IdleCPUPowerW(2); got != 0 {
		t.Errorf("IdleCPUPowerW of a package the node does not have = %v, want 0", got)
	}
}

// TestGPUExposureFollowsTheProfile: GPUExposure gives the profile's exposure
// and the units each GPU advertises under it, and nothing for a node without
// GPUs.
func TestGPUExposureFollowsTheProfile(t *testing.T) {
	t.Parallel()
	src := "assumed:test fixture"
	cases := []struct {
		name, profile string
		mutate        func(*hwemu.Profile)
		exposure      string
		units         int
	}{
		{name: "whole", profile: "nvidia-h100-nvl-8gpu", exposure: "whole", units: 1},
		{name: "mig-mixed", profile: "nvidia-h100-nvl-8gpu", mutate: func(p *hwemu.Profile) {
			p.GPUs.Exposure = hwemu.Fact[string]{V: "mig-mixed", Src: src}
			p.GPUs.MIGProfile, p.GPUs.MIGDevicesPerGPU = "1g.12gb", 7
		}, exposure: "mig-mixed", units: 7},
		{name: "timeslice-rename", profile: "nvidia-h100-sxm-8gpu", mutate: func(p *hwemu.Profile) {
			p.GPUs.Exposure = hwemu.Fact[string]{V: "timeslice-rename", Src: src}
			p.GPUs.TimesliceReplicas = 4
		}, exposure: "timeslice-rename", units: 4},
		{name: "dra", profile: "nvidia-l40s-4gpu", mutate: func(p *hwemu.Profile) {
			p.GPUs.Exposure = hwemu.Fact[string]{V: "dra", Src: src}
		}, exposure: "dra", units: 0},
		{name: "no GPUs", profile: throughputProfile, exposure: "", units: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, _, _ := hwemutest.NewNode(t, tc.profile, tc.mutate)
			if exposure, units := n.GPUExposure(); exposure != tc.exposure || units != tc.units {
				t.Errorf("GPUExposure() = %q, %d; want %q, %d", exposure, units, tc.exposure, tc.units)
			}
		})
	}
}

// throughputSocketCPUs lists the logical CPUs of socket s in hwemu's
// numbering, socket-major-smt-last: CPU k sits on socket (k mod S*C)/C.
func throughputSocketCPUs(p *hwemu.Profile, s int) []int {
	sockets, cores := p.CPU.Sockets.V, p.CPU.CoresPerSocket.V
	var cpus []int
	for k := 0; k < sockets*cores*p.CPU.ThreadsPerCore.V; k++ {
		if (k%(sockets*cores))/cores == s {
			cpus = append(cpus, k)
		}
	}
	return cpus
}

// throughputAnalytic fails unless p uses the analytic CPU model, the one
// throughputTM computes TM for.
func throughputAnalytic(t *testing.T, p *hwemu.Profile) {
	t.Helper()
	if m := p.Physics.CPU.Model; m != "" && m != "analytic" {
		t.Fatalf("%s uses cpu model %q; the test computes TM for the analytic one", p.Name, m)
	}
}

// throughputTM is the analytic CPU model's ThroughputMultiplier at frequency
// scale scale.
func throughputTM(p *hwemu.Profile, scale float64, class string, mem, io float64) float64 {
	m := phys.AnalyticCPUModel{Knee: p.Physics.CPU.Knee.V}
	return m.ThroughputMultiplier(phys.DeviceState{FreqScale: scale, MemoryIntensity: mem, IOIntensity: io, Class: class}, class)
}

func throughputStep(t *testing.T, n *hwemu.Node, d time.Duration) {
	t.Helper()
	if err := n.Step(d); err != nil {
		t.Fatal(err)
	}
}

// throughputWrite writes v to rel under the tree's sys directory, in place,
// as the agent writes a control file.
func throughputWrite(t *testing.T, tree *hwemu.Tree, rel, v string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(tree.SysRoot, filepath.FromSlash(rel)), []byte(v), 0); err != nil {
		t.Fatal(err)
	}
}
