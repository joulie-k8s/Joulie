package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/prometheus/client_golang/prometheus"
)

// writeStandaloneFile writes content to name in a fresh directory.
func writeStandaloneFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// newStandaloneTestSimulator is a simulator on the code defaults, with no
// catalogue, and its log silenced.
func newStandaloneTestSimulator(t *testing.T) *simulator {
	t.Helper()
	prev := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prev) })
	return newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 0, prometheus.NewRegistry())
}

// A legacy inventory entry's gpu_min_cap_watts and gpu_max_cap_watts used to
// be parsed and dropped: a GPU the catalogue does not know kept the default
// 80 to 300 W range. The entry describes its node, so it sets the range. It
// goes through runStandalone, so it also runs on the old code, where it
// fails with 300 W.
func TestStandaloneHonoursInventoryGPUCapRange(t *testing.T) {
	clearSimEnv(t)
	inventory := writeStandaloneFile(t, "inventory.yaml", `nodes:
  - node_name: gpu-unknown
    vendor: nvidia
    product: Generic PCIe Accelerator
    cpu_sockets: 2
    cpu_cores: 64
    gpu_count: 4
    gpu_min_cap_watts: 150
    gpu_max_cap_watts: 400
`)
	trace := writeStandaloneFile(t, "trace.jsonl", `{"type":"job","jobId":"j1","submitTimeOffsetSec":0,"intentClass":"standard","podTemplate":{"requests":{"cpu":"4","memory":"8Gi"}},"work":{"cpuUnits":20},"workloadProfile":{"cpuUtilization":0.5}}
`)
	for k, v := range map[string]string{
		"SIM_STANDALONE_INVENTORY": inventory,
		"SIM_STANDALONE_BASELINE":  "A",
		"SIM_SA_WORKERS":           "1",
		"SIM_SA_DT":                "5",
		"SIM_SA_TIMEOUT":           "10",
		// The catalogue does not know the GPU: the inventory's gpu_count,
		// carried as the joulie.io/hw.gpu-count label, sets the count, and
		// the base model the 80 to 300 W default range.
		"SIM_GPU_COUNT": "4",
	} {
		t.Setenv(k, v)
	}
	prev := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prev)
	s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 0, prometheus.NewRegistry())
	if err := s.initWorkloadEngineFromTrace(trace); err != nil {
		t.Fatal(err)
	}
	runStandalone(s)

	model := s.modelForNode("gpu-unknown")
	if model.GPU.MaxWattsPerGPU != 400 || model.GPU.MinCapWattsPerGPU != 150 {
		t.Errorf("model GPU range = %g to %g W, want 150 to 400 W from the inventory", model.GPU.MinCapWattsPerGPU, model.GPU.MaxWattsPerGPU)
	}
	st := s.state["gpu-unknown"]
	if st == nil {
		t.Fatal("no state for gpu-unknown")
	}
	if st.GPUCapWattsPerGpu != 400 || st.GPUTargetCapWattsPerGpu != 400 {
		t.Errorf("GPU cap = %g W, target %g W, want 400 W", st.GPUCapWattsPerGpu, st.GPUTargetCapWattsPerGpu)
	}
	if len(st.GPUDevices) != 4 {
		t.Fatalf("GPU devices = %d, want 4", len(st.GPUDevices))
	}
	for _, d := range st.GPUDevices {
		if d.CapWatts != 400 {
			t.Errorf("GPU %d cap = %g W, want 400 W", d.Index, d.CapWatts)
		}
	}
}

// An inventory entry with only gpu_max_cap_watts below the model's minimum
// lowers the minimum to it, and a minimum above the maximum is an error.
func TestInventoryGPUCapRangeEdges(t *testing.T) {
	clearSimEnv(t)
	t.Setenv("SIM_GPU_COUNT", "2")
	cases := []struct {
		name             string
		minW, maxW       float64
		wantMin, wantMax float64
		wantErr          bool
	}{
		{name: "max below the default minimum", maxW: 60, wantMin: 60, wantMax: 60},
		{name: "minimum only", minW: 100, wantMin: 100, wantMax: 300},
		{name: "minimum above maximum", minW: 500, maxW: 400, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStandaloneTestSimulator(t)
			nodes := []expandedNode{{
				Name: "g", CPUCores: 16, GPUCount: 2, GPUMinCapWatts: tc.minW, GPUMaxCapWatts: tc.maxW,
				Labels: map[string]string{"joulie.io/managed": "true", "joulie.io/hw.cpu-cores": "16", "joulie.io/hw.gpu-count": "2"},
			}}
			err := initStandaloneNodes(s, nodes)
			if tc.wantErr {
				if err == nil {
					t.Fatal("initStandaloneNodes succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			m := s.modelForNode("g")
			if m.GPU.MinCapWattsPerGPU != tc.wantMin || m.GPU.MaxWattsPerGPU != tc.wantMax {
				t.Errorf("GPU range = %g to %g W, want %g to %g W", m.GPU.MinCapWattsPerGPU, m.GPU.MaxWattsPerGPU, tc.wantMin, tc.wantMax)
			}
		})
	}
}

// profileModelFacts pins, per builtin profile, the values simModelFromProfile
// derives rather than copies: the CPU cap range and default (the sum of the
// PL1 limits, else of the HSMP caps, else the peak), the cap settling time,
// whether eco writes a CPU cap, and the GPU range.
var profileModelFacts = map[string]struct {
	capMaxW, capDefaultW float64
	capTauMS             int
	ecoPowerCap          bool
	gpuMinW, gpuMaxW     float64
}{
	// 2 x 165 W PL1 over a 999424 us window.
	"intel-xeon-2s-rapl": {capMaxW: 330, capDefaultW: 330, capTauMS: 999, ecoPowerCap: true},
	// package-0 and package-1 at 270 W; psys limits no package.
	"intel-xeon-2s-psys":      {capMaxW: 540, capDefaultW: 540, capTauMS: 999, ecoPowerCap: true},
	"intel-xeon-6530-2s-rapl": {capMaxW: 540, capDefaultW: 540, capTauMS: 999, ecoPowerCap: true},
	// 2 packages x 2 dies x 200 W: a die zone's limit counts once per die.
	"intel-xeon-2s-multidie": {capMaxW: 800, capDefaultW: 800, capTauMS: 999, ecoPowerCap: true},
	"intel-xeon6-2s-tpmi":    {capMaxW: 700, capDefaultW: 700, capTauMS: 999, ecoPowerCap: true},
	// 2 x 400 W HSMP settling over hsmp.settleTauMS; the agent has no HSMP
	// path, so eco scales the frequency.
	"amd-epyc-2s-hsmp": {capMaxW: 800, capDefaultW: 800, capTauMS: 1000},
	// Energy only, or no powercap: capped at the peak, 2 x maxPkgW. The tau
	// stays the base model's 250 ms.
	"amd-epyc-2s-energy-only":      {capMaxW: 720, capDefaultW: 720, capTauMS: 250},
	"amd-epyc-9534-2s-energy-only": {capMaxW: 560, capDefaultW: 560, capTauMS: 250},
	"intel-xeon6-2s-no-powercap":   {capMaxW: 700, capDefaultW: 700, capTauMS: 250},
	"amd-instinct-mi300x-8gpu":     {capMaxW: 720, capDefaultW: 720, capTauMS: 250, gpuMinW: 1, gpuMaxW: 750},
	"nvidia-l40s-4gpu":             {capMaxW: 560, capDefaultW: 560, capTauMS: 250, gpuMinW: 100, gpuMaxW: 350},
	"nvidia-h100-nvl-8gpu":         {capMaxW: 330, capDefaultW: 330, capTauMS: 999, ecoPowerCap: true, gpuMinW: 200, gpuMaxW: 400},
	"nvidia-h100-sxm-8gpu":         {capMaxW: 330, capDefaultW: 330, capTauMS: 999, ecoPowerCap: true, gpuMinW: 200, gpuMaxW: 700},
	"nvidia-h100-sxm-4gpu":         {capMaxW: 540, capDefaultW: 540, capTauMS: 999, ecoPowerCap: true, gpuMinW: 200, gpuMaxW: 700},
	// The W7900's limit is pinned: minimum and maximum are both 241 W.
	"amd-radeon-pro-w7900-4gpu": {capMaxW: 560, capDefaultW: 560, capTauMS: 250, gpuMinW: 241, gpuMaxW: 241},
}

// profilesNotMappable are the builtin profiles simModelFromProfile rejects.
var profilesNotMappable = map[string]string{
	"vm-no-powercap": "maxPkgW",
}

func TestProfileModelMatchesProfileFacts(t *testing.T) {
	clearSimEnv(t)
	base := defaultBaseModelFromEnv()
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := profiles[name]
		t.Run(name, func(t *testing.T) {
			m, err := simModelFromProfile(p, base)
			if want, rejected := profilesNotMappable[name]; rejected {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want one naming %s", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			facts, ok := profileModelFacts[name]
			if !ok {
				t.Fatalf("no pinned facts for builtin profile %s: add a row to profileModelFacts", name)
			}
			cpu := p.Physics.CPU
			sockets := float64(p.CPU.Sockets.V)
			checks := []struct {
				what      string
				got, want float64
			}{
				{"sockets", float64(m.CPUSockets), sockets},
				{"idle W", m.BaseIdleW, sockets * cpu.IdlePkgW.V},
				{"peak W", m.PMaxW, sockets * cpu.MaxPkgW.V},
				{"alpha", m.AlphaUtil, cpu.AlphaUtil.V},
				{"beta", m.BetaFreq, cpu.BetaFreq.V},
				{"fMin MHz", m.FMinMHz, float64(p.CPUFreq.CPUInfoMinKHz.V) / 1000},
				{"fMax MHz", m.FMaxMHz, float64(p.CPUFreq.CPUInfoMaxKHz.V) / 1000},
				{"knee MHz", m.CPULowestNonlinearFreqMHz, cpu.Knee.V * float64(p.CPUFreq.CPUInfoMaxKHz.V) / 1000},
				{"knee", cpuKneeFromModel(m), cpu.Knee.V},
				{"freq ramp ms", float64(m.DvfsRampMS), float64(cpu.FreqRampMS.V)},
				{"cap max W", m.RaplCapMaxW, facts.capMaxW},
				{"cap default W", m.DefaultCapW, facts.capDefaultW},
				{"cap min W", m.RaplCapMinW, 1},
				{"socket cap max W", m.CPUSocketCapMaxW, facts.capMaxW / sockets},
				{"cap tau ms", float64(m.CPUCapApplyTauMS), float64(facts.capTauMS)},
				{"cpu throttle start C", m.CPUThermalThrottleStartC, 0},
				{"cpu throttle full C", m.CPUThermalThrottleFullC, 0},
				{"gpu throttle start C", m.GPU.ThermalThrottleStartC, 0},
				{"gpu throttle full C", m.GPU.ThermalThrottleFullC, 0},
				{"gpu min cap W", m.GPU.MinCapWattsPerGPU, facts.gpuMinW},
				{"gpu max W", m.GPU.MaxWattsPerGPU, facts.gpuMaxW},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %g, want %g", c.what, c.got, c.want)
				}
			}
			if m.CPUWattsPerDeltaC <= 0 || m.GPU.WattsPerDeltaC <= 0 {
				t.Errorf("watts per degree = %g (cpu), %g (gpu), want > 0 so the model validates", m.CPUWattsPerDeltaC, m.GPU.WattsPerDeltaC)
			}
			if m.CPUModel != p.CPU.ModelName.V || m.CPUDriverFamily != p.CPUFreq.Driver.V {
				t.Errorf("cpu = %q on %q, want %q on %q", m.CPUModel, m.CPUDriverFamily, p.CPU.ModelName.V, p.CPUFreq.Driver.V)
			}
			if m.Defaulted != (hw.Defaulted{}) {
				t.Errorf("Defaulted = %+v, want none: the profile sets every field", m.Defaulted)
			}
			if len(m.CPUPowerCurve) != 0 {
				t.Errorf("an analytic profile has a curve of %d points", len(m.CPUPowerCurve))
			}
			if got := profileHasWritablePL1(p); got != facts.ecoPowerCap {
				t.Errorf("writable PL1 = %v, want %v", got, facts.ecoPowerCap)
			}
			if g := p.GPUs; g != nil {
				if m.GPU.Count != g.Count.V || m.GPU.Product != g.Product.V || m.GPU.Vendor != g.Vendor {
					t.Errorf("gpu = %d x %q (%s), want %d x %q (%s)", m.GPU.Count, m.GPU.Product, m.GPU.Vendor, g.Count.V, g.Product.V, g.Vendor)
				}
				ph := p.Physics.GPU
				if m.GPU.IdleWattsPerGPU != ph.IdleW.V || m.GPU.CapApplyTauMS != ph.CapSettleTauMS.V || m.GPU.ComputeGamma != ph.ComputeGamma.V || m.GPU.MemoryEpsilon != ph.MemoryEpsilon.V || m.GPU.MemoryGamma != ph.MemoryGamma.V {
					t.Errorf("gpu physics = idle %g, tau %d, gammas %g %g %g; want %g, %d, %g %g %g", m.GPU.IdleWattsPerGPU, m.GPU.CapApplyTauMS, m.GPU.ComputeGamma, m.GPU.MemoryEpsilon, m.GPU.MemoryGamma, ph.IdleW.V, ph.CapSettleTauMS.V, ph.ComputeGamma.V, ph.MemoryEpsilon.V, ph.MemoryGamma.V)
				}
			} else if m.GPU.Count != 0 {
				t.Errorf("gpu count = %d, want 0", m.GPU.Count)
			}
		})
	}
	for name := range profileModelFacts {
		if profiles[name] == nil {
			t.Errorf("profileModelFacts has a row for %s, which is no builtin profile", name)
		}
	}
}

// builtinProfile is a fresh copy of a builtin profile, safe to change.
func builtinProfile(t *testing.T, name string) *hwemu.Profile {
	t.Helper()
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	p := profiles[name]
	if p == nil {
		t.Fatalf("no builtin profile %s", name)
	}
	return p
}

func TestProfileModelRejectsWhatItCannotRepresent(t *testing.T) {
	clearSimEnv(t)
	base := defaultBaseModelFromEnv()

	mmio := builtinProfile(t, "intel-xeon-2s-rapl")
	mmio.Powercap.ControlTypes = append(mmio.Powercap.ControlTypes, hwemu.ControlTypeSpec{Name: "intel-rapl-mmio", Zones: mmio.Powercap.ControlTypes[0].Zones})
	if _, err := simModelFromProfile(mmio, base); err == nil || !strings.Contains(err.Error(), "intel-rapl-mmio") {
		t.Errorf("intel-rapl-mmio: err = %v, want one naming the control type", err)
	}

	gpu := builtinProfile(t, "nvidia-h100-nvl-8gpu")
	gpu.Physics.GPU.NaturalMaxW.V = 450
	if _, err := simModelFromProfile(gpu, base); err == nil || !strings.Contains(err.Error(), "naturalMaxW") {
		t.Errorf("naturalMaxW 450 against limits.maxW 400: err = %v, want one naming naturalMaxW", err)
	}
}

// A catalog-curve profile runs the node2S curve of its catalogue CPU scaled
// by sockets/2, as hwemu runs half of it on each package.
func TestProfileModelCatalogCurveScalesBySockets(t *testing.T) {
	clearSimEnv(t)
	cat, err := hw.LoadCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	p := builtinProfile(t, "amd-epyc-2s-energy-only") // 2x EPYC 9654, which has a node2S curve
	_, spec, ok := cat.MatchCPU(p.CPU.ModelName.V)
	if !ok || spec.MeasuredCurves == nil || spec.MeasuredCurves.Node2S == nil {
		t.Fatalf("catalogue has no node2S curve for %q", p.CPU.ModelName.V)
	}
	node2S := spec.MeasuredCurves.Node2S.Points
	p.Physics.CPU.Model = "catalog-curve"
	for _, sockets := range []int{1, 2} {
		p.CPU.Sockets.V = sockets
		m, err := simModelFromProfile(p, defaultBaseModelFromEnv())
		if err != nil {
			t.Fatal(err)
		}
		if len(m.CPUPowerCurve) != len(node2S) {
			t.Fatalf("%d sockets: curve of %d points, want %d", sockets, len(m.CPUPowerCurve), len(node2S))
		}
		scale := float64(sockets) / 2
		for i, pt := range m.CPUPowerCurve {
			if pt.LoadPct != node2S[i].LoadPct || pt.PowerW != node2S[i].PowerW*scale {
				t.Errorf("%d sockets: point %d = %+v, want %g%% at %g W", sockets, i, pt, node2S[i].LoadPct, node2S[i].PowerW*scale)
			}
		}
		// Energy only: the cap is the curve's peak, which never binds.
		if last := m.CPUPowerCurve[len(m.CPUPowerCurve)-1].PowerW; m.RaplCapMaxW < last {
			t.Errorf("%d sockets: cap %g W below the curve peak %g W", sockets, m.RaplCapMaxW, last)
		}
	}
}

// standaloneProfileNodes loads an inventory and builds its nodes.
func standaloneProfileNodes(t *testing.T, inventory string) (*simulator, []expandedNode) {
	t.Helper()
	nodes, err := loadInventory(writeStandaloneFile(t, "inventory.yaml", inventory))
	if err != nil {
		t.Fatal(err)
	}
	s := newStandaloneTestSimulator(t)
	if err := initStandaloneNodes(s, nodes); err != nil {
		t.Fatal(err)
	}
	return s, nodes
}

func TestStandaloneInventoryProfileKey(t *testing.T) {
	clearSimEnv(t)
	s, nodes := standaloneProfileNodes(t, `nodes:
  - node_name: nvl
    profile: nvidia-h100-nvl-8gpu
`)
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(nodes))
	}
	n := nodes[0]
	p := builtinProfile(t, "nvidia-h100-nvl-8gpu")
	if n.Profile == nil || n.Profile.Name != p.Name {
		t.Fatalf("profile = %v, want %s", n.Profile, p.Name)
	}
	if n.CPUCores != 96 || n.GPUCount != 8 || n.CPUSockets != 2 || n.CPUModel != p.CPU.ModelName.V || n.Product != "NVIDIA H100 NVL" || n.Vendor != "nvidia" {
		t.Errorf("node = %d CPUs, %d sockets of %q, %d x %q (%s); want 96, 2 of %q, 8 x NVIDIA H100 NVL (nvidia)", n.CPUCores, n.CPUSockets, n.CPUModel, n.GPUCount, n.Product, n.Vendor, p.CPU.ModelName.V)
	}
	for k, v := range p.NodeObject("nvl").Labels {
		if n.Labels[k] != v {
			t.Errorf("label %s = %q, want %q from NodeObject", k, n.Labels[k], v)
		}
	}
	for k, v := range map[string]string{
		"type":                   "kwok",
		"joulie.io/managed":      "true",
		"joulie.io/node-name":    "nvl",
		"joulie.io/hw.cpu-cores": "96",
		"joulie.io/hw.gpu-count": "8",
	} {
		if n.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, n.Labels[k], v)
		}
	}

	st := s.state["nvl"]
	if st == nil {
		t.Fatal("no state for nvl")
	}
	if st.CPUCapacityCores != 96 || st.GPUCapacityDevices != 8 {
		t.Errorf("capacity = %g CPUs, %g GPUs, want 96 and 8", st.CPUCapacityCores, st.GPUCapacityDevices)
	}
	want, err := simModelFromProfile(p, s.model)
	if err != nil {
		t.Fatal(err)
	}
	m := s.modelForNode("nvl")
	if m.RaplCapMaxW != want.RaplCapMaxW || m.PMaxW != want.PMaxW || m.GPU.MaxWattsPerGPU != want.GPU.MaxWattsPerGPU || m.GPU.MinCapWattsPerGPU != want.GPU.MinCapWattsPerGPU {
		t.Errorf("node model = cap %g W, peak %g W, GPU %g to %g W; want the profile's %g, %g, %g to %g", m.RaplCapMaxW, m.PMaxW, m.GPU.MinCapWattsPerGPU, m.GPU.MaxWattsPerGPU, want.RaplCapMaxW, want.PMaxW, want.GPU.MinCapWattsPerGPU, want.GPU.MaxWattsPerGPU)
	}
	if len(st.GPUDevices) != 8 || st.GPUCapWattsPerGpu != 400 || st.CapWatts != 330 {
		t.Errorf("state = %d GPUs at %g W, CPU cap %g W; want 8 at 400 W, 330 W", len(st.GPUDevices), st.GPUCapWattsPerGpu, st.CapWatts)
	}
}

// standalonePolicy is two nodes of one profile, p-0 and p-1, ready for
// applyPowerPolicy.
type standalonePolicy struct {
	s          *simulator
	tracker    map[string]*standaloneNodeTracker
	names      []string
	nodeByName map[string]*expandedNode
}

func newStandalonePolicy(t *testing.T, profile string) *standalonePolicy {
	t.Helper()
	s, nodes := standaloneProfileNodes(t, "nodes:\n  - node_name_prefix: p\n    replicas: 2\n    profile: "+profile+"\n")
	p := &standalonePolicy{s: s, tracker: map[string]*standaloneNodeTracker{}, nodeByName: map[string]*expandedNode{}}
	for i, n := range nodes {
		p.tracker[n.Name] = &standaloneNodeTracker{}
		p.nodeByName[n.Name] = &nodes[i]
		p.names = append(p.names, n.Name)
	}
	sort.Strings(p.names)
	return p
}

// apply runs the policy at SIM_SA_CPU_ECO_PCT 60 and SIM_SA_GPU_ECO_PCT 70.
// Under baseline B with hpFrac 0 it puts p-1 in eco: every family keeps one
// performance node.
func (p *standalonePolicy) apply(baseline string, hpFrac float64) {
	applyPowerPolicy(p.s, p.tracker, p.names, p.nodeByName, baseline, hpFrac, 0.6, 0.7, 0, 10, 0.3, 1, 25)
}

// standaloneEco puts the second of two nodes of profile in eco under
// baseline B and returns the two node states and the eco node's model.
func standaloneEco(t *testing.T, profile string) (perf, eco *nodeState, ecoModel simModel) {
	t.Helper()
	p := newStandalonePolicy(t, profile)
	p.apply("B", 0)
	if p.tracker["p-0"].isEco || !p.tracker["p-1"].isEco {
		t.Fatalf("eco = p-0 %v, p-1 %v; want p-1 only", p.tracker["p-0"].isEco, p.tracker["p-1"].isEco)
	}
	return p.s.state["p-0"], p.s.state["p-1"], p.s.modelForNode("p-1")
}

// Eco on a node whose PL1 the agent can write is what the agent does: a CPU
// power cap at SIM_SA_CPU_ECO_PCT of the PL1 maximum, 0.6 x 2 x 165 = 198 W,
// with the frequency left to the cap.
func TestStandaloneEcoOnRAPLProfileIsAPowerCap(t *testing.T) {
	clearSimEnv(t)
	perf, eco, model := standaloneEco(t, "intel-xeon-2s-rapl")
	if eco.TargetCapWatts != 198 {
		t.Errorf("eco CPU cap = %g W, want 198 W", eco.TargetCapWatts)
	}
	if eco.FreqScale != 1 || eco.TargetThrottlePct != 0 {
		t.Errorf("eco frequency = scale %g, throttle %d%%, want 1 and 0: the cap sets it", eco.FreqScale, eco.TargetThrottlePct)
	}
	if perf.TargetCapWatts != 330 || perf.FreqScale != 1 {
		t.Errorf("performance CPU cap = %g W at scale %g, want 330 W at 1", perf.TargetCapWatts, perf.FreqScale)
	}

	// At full load the cap holds the package: the frequency is solved down
	// and the power stays at the cap.
	eco.CPUUtil = 1
	stepNode(eco, model, 30, physMaxSubstep)
	if eco.CPUInstantPowerWatts > 198+model.RaplHeadW || eco.CPUInstantPowerWatts < 190 {
		t.Errorf("eco power at full load = %.1f W, want about 198 W", eco.CPUInstantPowerWatts)
	}
	if eco.FreqScale >= 1 {
		t.Errorf("eco frequency scale at full load = %g, want below 1", eco.FreqScale)
	}
}

// The cap eco writes lasts only while the node is in eco. Leaving eco, for
// performance under B, for baseline A, or for DrainingPerformance when a
// performance job lands on it, the node gets its PL1 limit back, as the
// agent restores it.
func TestStandaloneEcoPowerCapLiftsOutOfEco(t *testing.T) {
	clearSimEnv(t)
	cases := []struct {
		name     string
		baseline string
		hpFrac   float64
		perfPods int
		draining bool
	}{
		{name: "performance under B", baseline: "B", hpFrac: 1},
		{name: "baseline A", baseline: "A"},
		{name: "draining", baseline: "B", perfPods: 1, draining: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newStandalonePolicy(t, "intel-xeon-2s-rapl")
			p.apply("B", 0)
			st := p.s.state["p-1"]
			if !p.tracker["p-1"].isEco || st.TargetCapWatts != 198 {
				t.Fatalf("eco = %v at %g W, want eco at 198 W", p.tracker["p-1"].isEco, st.TargetCapWatts)
			}
			p.tracker["p-1"].perfPodCount = tc.perfPods
			p.apply(tc.baseline, tc.hpFrac)
			if tr := p.tracker["p-1"]; tr.isEco || tr.isDraining != tc.draining {
				t.Fatalf("eco = %v, draining = %v; want false, %v", tr.isEco, tr.isDraining, tc.draining)
			}
			if st.TargetCapWatts != 330 || st.FreqScale != 1 || st.TargetThrottlePct != 0 {
				t.Errorf("CPU cap = %g W at scale %g, throttle %d%%; want the 330 W PL1 at 1 and 0", st.TargetCapWatts, st.FreqScale, st.TargetThrottlePct)
			}
		})
	}
}

// The agent has no HSMP path, so eco on an HSMP node keeps the frequency
// scale of a legacy node and leaves the CPU cap where it started.
func TestStandaloneEcoOnHSMPProfileIsAFrequencyScale(t *testing.T) {
	clearSimEnv(t)
	perf, eco, model := standaloneEco(t, "amd-epyc-2s-hsmp")
	if eco.FreqScale != 0.6 || eco.TargetThrottlePct != 40 {
		t.Errorf("eco frequency = scale %g, throttle %d%%, want 0.6 and 40%%", eco.FreqScale, eco.TargetThrottlePct)
	}
	if eco.TargetCapWatts != model.DefaultCapW || model.DefaultCapW != 800 {
		t.Errorf("eco CPU cap = %g W (default %g W), want the 800 W HSMP default", eco.TargetCapWatts, model.DefaultCapW)
	}
	if perf.FreqScale != 1 || perf.TargetThrottlePct != 0 {
		t.Errorf("performance frequency = scale %g, throttle %d%%, want 1 and 0", perf.FreqScale, perf.TargetThrottlePct)
	}
}

func TestUnknownProfileIsAnError(t *testing.T) {
	clearSimEnv(t)
	cases := []struct {
		name, inventory, want string
	}{
		{
			name:      "unknown profile",
			inventory: "nodes:\n  - node_name: x\n    profile: no-such-profile\n",
			want:      `unknown profile "no-such-profile"`,
		},
		{
			name:      "profile with hardware keys",
			inventory: "nodes:\n  - node_name: x\n    profile: intel-xeon-2s-rapl\n    cpu_cores: 64\n    gpu_max_cap_watts: 300\n",
			want:      "cpu_cores, gpu_max_cap_watts must stay unset",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadInventory(writeStandaloneFile(t, "inventory.yaml", tc.inventory))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}

	// A profile directory that does not exist is an error too, never a
	// fallback to the builtins.
	t.Setenv(envProfilesDir, filepath.Join(t.TempDir(), "missing"))
	_, err := loadInventory(writeStandaloneFile(t, "inventory.yaml", "nodes:\n  - node_name: x\n    profile: intel-xeon-2s-rapl\n"))
	if err == nil || !strings.Contains(err.Error(), envProfilesDir) {
		t.Fatalf("missing %s: err = %v, want one naming it", envProfilesDir, err)
	}
}
