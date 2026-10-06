package main

// Emulated-node tests. Each renders a node of a hardware profile with
// simulator/pkg/hwemu, points the agent at it (useEmulatedNode) and drives the
// agent's real discovery and control code over it: the sysfs tree, cpuinfo,
// the fake nvidia-smi and rocm-smi, and the physics that turns the limits the
// agent writes into power and energy readings.
//
// Several tests pin today's behaviour of a known defect. Their failure
// messages name the defect, so the fix that changes the behaviour turns the
// test into its regression test. The defects, with the code involved:
//
//   - D1: listAmdDevices runs rocm-smi --showpowercap --showproductname
//     --json. No rocm-smi release has --showpowercap, so the inventory fails
//     and every AMD GPU cap ends in an error.
//   - D2: applyAmdGPUCap takes rocm-smi's exit 0 as applied, but rocm-smi
//     exits 0 when it rejects a set, for example on a secondary die.
//   - D3: applyAmdGPUCap passes no --autorespond. Above max(current, default)
//     rocm-smi --setpoweroverdrive asks for confirmation, reads EOF from the
//     empty stdin and exits 1.
//   - D4: listAmdDevices takes the model from Card SKU, a VBIOS segment, and
//     the maximum from --showmaxpower, which reports the current cap.
//   - D5: dvfs.EnergyFiles dedupes by path string, so it lists every package
//     twice, through class/powercap and through devices/virtual/powercap.
//   - D6: dvfs.CPUFreqList lists every policy twice, through cpuN/cpufreq and
//     as policyN. An odd entry count throttles one policy fewer than
//     intended; with policies shared per core, the later entry of each
//     sibling writes the maximum back.
//   - D7: one CPU cap has two meanings: applyRAPLPackageCap writes it to each
//     package, while the DVFS fallback (dvfs.Controller.Reconcile) compares
//     it with the node total.
//   - D8: hasNFDGPUVendor ignores the NFD accelerator class pci-1200_<vendor>,
//     and the GPU model is read from amd.com/gpu.product while the AMD
//     labeller publishes amd.com/gpu.product-name.
//   - D9: discoverCPUVendor reads NFD labels only. Without NFD the vendor is
//     empty, so applyRAPLPackageCap skips RAPL, while NodeHardware still
//     publishes the RAPL capRange and controlAvailable.
//   - D10: when the BIOS locks RAPL, every write of applyRAPLPackageCap fails
//     and reconcileOnce returns: no DVFS fallback runs and no CPU control
//     status is written.
//   - D11: zone selection in package dvfs: EnergyFiles sums the platform zone
//     psys as package energy, and RAPLPackageZones takes intel-rapl-mmio's
//     package-0 as a second socket 0 and gives every package-X-die-Y zone the
//     full socket cap.
//   - D12: the controller manager's node power query keeps the first series
//     only, while Kepler returns several per node.
//   - D13: the simulator's curve-model cap solver never throttles, because
//     the curve's power is clamped to the cap.
//   - D14: some hardware catalog curves and minimum power limits match no
//     public source.
//   - D15: the agent DaemonSet has no tolerations and an amd64-only image, so
//     a tainted or arm64 node runs no agent.
//   - D16: listNvidiaDevices parses nvidia-smi's [N/A] limits as 0 W, so the
//     cap range is unknown and a percent cap cannot be resolved.
//
// The fixes that flip them:
//
//   - F1: dedupe EnergyFiles and CPUFreqList by resolved path, and document
//     that a CPU cap is per socket (D5, D6, D7).
//   - F2: the simulator uses phys.AnalyticCPUModel, SolveFreqScaleForCap and
//     FirstOrderToward instead of its own copies (D13).
//   - F3: AMD and NVIDIA tool handling: real rocm-smi flags, output checks
//     instead of exit codes, --autorespond, the model from Card Series, and
//     [N/A] as unknown (D1 to D4, D16).
//   - F4: labels, vendor and zones: the NFD accelerator class, the labeller's
//     keys, the vendor from cpuinfo without NFD, a fallback when RAPL is
//     locked, and zone selection that leaves out psys and MMIO and splits a
//     socket cap between dies (D8 to D11).
//   - F5: the controller manager's node power query (D12).
//
// Regenerate the goldens of the generic profiles after reviewing a change:
//
//	go test ./cmd/agent/ -run Hwemu -update

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/agent/dvfs"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
)

// hwemuGoldenDir holds the published NodeHardware status of each generic
// profile, generated with -update and never hand edited.
const hwemuGoldenDir = "testdata/hwemu"

// A01: every corpus machine with a golden, mapped by hwemu.FromCorpus and
// rendered as a real node lays it out (nested devices tree, class symlinks,
// cpufreq policies, NVML state and the fake nvidia-smi), publishes exactly
// the golden the flat capture publishes. The emulated tree is
// indistinguishable from the capture for discovery, and an NVIDIA machine
// takes the same /dev/nvidiactl path as in the corpus test.
func TestHwemuA01CorpusReplaysThroughTheEmulatedTree(t *testing.T) {
	for _, machine := range corpusMachines(t) {
		t.Run(machine, func(t *testing.T) {
			dir := filepath.Join(corpusRoot(), machine)
			want, err := os.ReadFile(filepath.Join(dir, "expected.json"))
			if errors.Is(err, os.ErrNotExist) {
				t.Skipf("%s has no expected.json, so there is no golden to compare the emulated node with", machine)
			}
			if err != nil {
				t.Fatal(err)
			}
			p, err := hwemu.FromCorpus(dir)
			if err != nil {
				t.Fatal(err)
			}
			tree, err := hwemu.Render(p, filepath.Join(t.TempDir(), "node"), hwemu.RenderOptions{NodeName: "corpus-" + machine})
			if err != nil {
				t.Fatal(err)
			}
			e := useEmulatedNode(t, tree, nil)
			node := e.nodeObject(p)

			got := publishedNodeHardwareStatus(t, node.Name, discoverHardware(context.Background(), node))
			if string(got) != string(want) {
				t.Fatalf("the emulated %s publishes a NodeHardware status that differs from the corpus golden.\ngot:\n%s\nwant:\n%s",
					machine, got, want)
			}

			// The corpus test answers the NVIDIA probe of a machine with GPU
			// output through a /dev/nvidiactl marker (hideHostGPUProbe). The
			// emulated node must take that road too: the rendered device
			// decides the vendor, and nvidia-smi -L never runs. Without the
			// device the fake nvidia-smi would answer -L, and the bytes
			// above would still match.
			if _, err := os.Stat(filepath.Join(dir, "nvidia-smi.txt")); err == nil {
				if _, err := os.Stat(filepath.Join(tree.Root, filepath.FromSlash(layout.DevNvidiactl))); err != nil {
					t.Errorf("%s has nvidia-smi.txt but the emulated node has no %s: %v", machine, layout.DevNvidiactl, err)
				}
				if ran, _ := e.runner.ran(nvidiaListProbe); ran {
					t.Errorf("%s ran, so the GPU vendor did not come from %s as in the corpus test; commands: %v",
						nvidiaListProbe, layout.DevNvidiactl, e.runner.commands())
				}
			}
		})
	}
}

// a02Path is what A02 expects of one generic profile: the road each family
// takes through discovery, as the profile was written to exercise it.
type a02Path struct {
	cpuVendor       string
	cpuSockets      int
	cpuCapMaxW      float64 // 0: no capRange
	cpuDriverFamily string
	cpuControl      bool
	// cpuModel and gpuModel are the hardware catalog keys the raw model
	// strings match; "" when none does.
	cpuModel   string
	gpuVendor  string
	gpuCount   int
	gpuCapMinW float64 // with gpuCapMaxW 0: no capRangePerGpu
	gpuCapMaxW float64
	gpuControl bool
	gpuModel   string
	// gpuVendorFrom names what decided the GPU vendor: an NFD label, or the
	// command whose success did.
	gpuVendorFrom string
}

const (
	nvidiaPCILabel     = "feature.node.kubernetes.io/pci-0302_10de.present"
	rocmProductProbe   = "rocm-smi --showproductname"
	nvidiaListProbe    = "nvidia-smi -L"
	amdInventoryAgent  = "rocm-smi --showpowercap --showproductname --json"
	nvidiaQueryAgent   = "nvidia-smi --query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name --format=csv,noheader,nounits"
	amdAcceleratorNFD  = "feature.node.kubernetes.io/pci-1200_1002.present"
	amdLabellerProduct = "amd.com/gpu.product-name"
)

// a02Paths is the road each generic profile takes. Each GPU profile runs on
// one of the CPU hosts, so it publishes that host's CPU block next to its
// GPU block: the 8-GPU H100 profiles on the Xeon 8260 RAPL host, the 4-GPU
// SXM profile on the Xeon 6530 RAPL host, the L40S on the energy-only EPYC
// 9534 host and the MI300X on the energy-only EPYC 9654 host.
var a02Paths = map[string]a02Path{
	"intel-xeon-2s-rapl": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 165, cpuDriverFamily: "intel_pstate", cpuControl: true, cpuModel: "INTEL_XEON_PLATINUM_8260",
		gpuVendor: "none",
	},
	"intel-xeon-6530-2s-rapl": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 270, cpuDriverFamily: "intel_cpufreq", cpuControl: true, cpuModel: "INTEL_XEON_GOLD_6530",
		gpuVendor: "none",
	},
	"amd-epyc-2s-energy-only": {
		cpuVendor: "AuthenticAMD", cpuSockets: 2, cpuDriverFamily: "acpi-cpufreq", cpuControl: true, cpuModel: "AMD_EPYC_9654",
		gpuVendor: "none",
	},
	"amd-epyc-9534-2s-energy-only": {
		cpuVendor: "AuthenticAMD", cpuSockets: 2, cpuDriverFamily: "acpi-cpufreq", cpuControl: true, cpuModel: "AMD_EPYC_9534",
		gpuVendor: "none",
	},
	"amd-epyc-2s-hsmp": {
		cpuVendor: "AuthenticAMD", cpuSockets: 2, cpuDriverFamily: "amd-pstate-epp", cpuControl: true, cpuModel: "AMD_EPYC_9655",
		gpuVendor: "none",
	},
	"nvidia-h100-nvl-8gpu": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 165, cpuDriverFamily: "intel_pstate", cpuControl: true, cpuModel: "INTEL_XEON_PLATINUM_8260",
		gpuVendor: "nvidia", gpuCount: 8, gpuCapMinW: 200, gpuCapMaxW: 400, gpuControl: true, gpuModel: "NVIDIA_H100_NVL", gpuVendorFrom: nvidiaPCILabel,
	},
	"nvidia-h100-sxm-8gpu": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 165, cpuDriverFamily: "intel_pstate", cpuControl: true, cpuModel: "INTEL_XEON_PLATINUM_8260",
		gpuVendor: "nvidia", gpuCount: 8, gpuCapMinW: 200, gpuCapMaxW: 700, gpuControl: true, gpuModel: "NVIDIA_H100_SXM", gpuVendorFrom: nvidiaPCILabel,
	},
	"nvidia-h100-sxm-4gpu": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 270, cpuDriverFamily: "intel_cpufreq", cpuControl: true, cpuModel: "INTEL_XEON_GOLD_6530",
		gpuVendor: "nvidia", gpuCount: 4, gpuCapMinW: 200, gpuCapMaxW: 700, gpuControl: true, gpuModel: "NVIDIA_H100_SXM", gpuVendorFrom: nvidiaPCILabel,
	},
	"nvidia-l40s-4gpu": {
		cpuVendor: "AuthenticAMD", cpuSockets: 2, cpuDriverFamily: "acpi-cpufreq", cpuControl: true, cpuModel: "AMD_EPYC_9534",
		gpuVendor: "nvidia", gpuCount: 4, gpuCapMinW: 100, gpuCapMaxW: 350, gpuControl: true, gpuModel: "NVIDIA_L40S", gpuVendorFrom: nvidiaPCILabel,
	},
	"amd-instinct-mi300x-8gpu": {
		cpuVendor: "AuthenticAMD", cpuSockets: 2, cpuDriverFamily: "acpi-cpufreq", cpuControl: true, cpuModel: "AMD_EPYC_9654",
		gpuVendor: "amd", gpuCount: 8, gpuVendorFrom: rocmProductProbe,
	},
	"vm-no-powercap": {
		cpuVendor: "GenuineIntel", cpuSockets: 8,
		gpuVendor: "none",
	},
}

// A02: each generic profile publishes the NodeHardware status pinned in
// testdata/hwemu/<profile>.json, and takes its family's road: Intel RAPL
// gives a capRange of the package TDP (165 W on the 8260, 270 W on the 6530;
// min 0, since RAPL has no min file) and controls through intel_pstate; the
// energy-only and HSMP EPYC hosts give no capRange and control through the
// cpufreq driver; H100 NVL and SXM and L40S give their per-GPU range, with
// the vendor from the NFD 0302 label; MI300X counts its GPUs from
// allocatable, has no GPU control and gets its vendor from rocm-smi
// --showproductname; the VM reads one socket per vCPU and has no control.
// Every CPU and GPU model string matches its hardware catalog entry, except
// the VM's CPU and the MI300X, whose inventory fails (D1).
func TestHwemuA02GenericProfilesTakeTheirFamilyPath(t *testing.T) {
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
		t.Run(name, func(t *testing.T) {
			want, ok := a02Paths[name]
			if !ok {
				t.Fatalf("profile %s has no row in a02Paths; add the road its family should take", name)
			}
			_, tree, p := hwemutest.NewNode(t, name, nil)
			e := useEmulatedNode(t, tree, nil)
			node := e.nodeObject(p)
			hw := discoverHardware(context.Background(), node)
			checkA02Path(t, e, node, hw, want)

			got := publishedNodeHardwareStatus(t, node.Name, hw)
			golden := filepath.Join(hwemuGoldenDir, name+".json")
			if *updateCorpusGolden {
				if err := os.MkdirAll(hwemuGoldenDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("wrote %s", golden)
				return
			}
			wantJSON, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v. Generate it with: go test ./cmd/agent/ -run Hwemu -update", err)
			}
			if string(got) != string(wantJSON) {
				t.Fatalf("NodeHardware status for %s does not match %s.\ngot:\n%s\nwant:\n%s\n"+
					"If the change is intended, review it and run: go test ./cmd/agent/ -run Hwemu -update",
					name, golden, got, wantJSON)
			}
		})
	}
}

func checkA02Path(t *testing.T, e *emulatedNode, node *corev1.Node, hw HardwareInfo, want a02Path) {
	t.Helper()
	if hw.CPUVendor != want.cpuVendor || hw.CPUSockets != want.cpuSockets || hw.CPUDriverFamily != want.cpuDriverFamily || hw.CPUControl != want.cpuControl {
		t.Errorf("cpu vendor=%q sockets=%d driverFamily=%q control=%v, want %q %d %q %v",
			hw.CPUVendor, hw.CPUSockets, hw.CPUDriverFamily, hw.CPUControl, want.cpuVendor, want.cpuSockets, want.cpuDriverFamily, want.cpuControl)
	}
	switch {
	case want.cpuCapMaxW == 0 && hw.CPUCapKnown:
		t.Errorf("cpu capRange %v to %v W, want none", hw.CPUCapMinWatts, hw.CPUCapMaxWatts)
	case want.cpuCapMaxW > 0 && (!hw.CPUCapKnown || hw.CPUCapMaxWatts != want.cpuCapMaxW || hw.CPUCapMinWatts != 0):
		t.Errorf("cpu capRange known=%v %v to %v W, want 0 to %v W", hw.CPUCapKnown, hw.CPUCapMinWatts, hw.CPUCapMaxWatts, want.cpuCapMaxW)
	}
	if hw.CPUModel != want.cpuModel || hw.GPUModel != want.gpuModel {
		t.Errorf("catalog keys cpu=%q (raw %q) gpu=%q (raw %q), want %q and %q",
			hw.CPUModel, hw.CPURawModel, hw.GPUModel, hw.GPURawModel, want.cpuModel, want.gpuModel)
	}
	if hw.GPUVendor != want.gpuVendor || hw.GPUCount != want.gpuCount || hw.GPUControl != want.gpuControl {
		t.Errorf("gpu vendor=%q count=%d control=%v, want %q %d %v", hw.GPUVendor, hw.GPUCount, hw.GPUControl, want.gpuVendor, want.gpuCount, want.gpuControl)
	}
	switch {
	case want.gpuCapMaxW == 0 && hw.GPUCapKnown:
		t.Errorf("gpu capRangePerGpu %v to %v W, want none", hw.GPUCapMinWatts, hw.GPUCapMaxWatts)
	case want.gpuCapMaxW > 0 && (!hw.GPUCapKnown || hw.GPUCapMinWatts != want.gpuCapMinW || hw.GPUCapMaxWatts != want.gpuCapMaxW):
		t.Errorf("gpu capRangePerGpu known=%v %v to %v W, want %v to %v W", hw.GPUCapKnown, hw.GPUCapMinWatts, hw.GPUCapMaxWatts, want.gpuCapMinW, want.gpuCapMaxW)
	}

	switch want.gpuVendorFrom {
	case nvidiaPCILabel:
		// The label answers detectGPUVendor before any probe runs.
		if node.Labels[nvidiaPCILabel] != "true" {
			t.Errorf("node has no %s label: %v", nvidiaPCILabel, node.Labels)
		}
		if ran, _ := e.runner.ran(nvidiaListProbe); ran {
			t.Errorf("%s ran, so the vendor did not come from the NFD label; commands: %v", nvidiaListProbe, e.runner.commands())
		}
		if _, ok := e.runner.ran(nvidiaQueryAgent); !ok {
			t.Errorf("the nvidia-smi inventory query did not succeed; commands: %v", e.runner.commands())
		}
		// The node also has dev/nvidiactl, which detectGPUVendor checks
		// right after the labels, so the checks above hold even when the
		// label is ignored. Hide the device and discover again: the vendor
		// must still be nvidia, with no probe run, so the label alone
		// decided it.
		saved := nvidiaControlDevicePath
		nvidiaControlDevicePath = filepath.Join(t.TempDir(), "absent-nvidiactl")
		defer func() { nvidiaControlDevicePath = saved }()
		e.runner.reset()
		if again := discoverHardware(context.Background(), node); again.GPUVendor != "nvidia" {
			t.Errorf("without dev/nvidiactl the gpu vendor is %q, want nvidia from %s; commands: %v",
				again.GPUVendor, nvidiaPCILabel, e.runner.commands())
		}
		if ran, _ := e.runner.ran(nvidiaListProbe); ran {
			t.Errorf("without dev/nvidiactl %s ran, so the agent did not take the vendor from %s; commands: %v",
				nvidiaListProbe, nvidiaPCILabel, e.runner.commands())
		}
	case rocmProductProbe:
		ranList, listOK := e.runner.ran(nvidiaListProbe)
		ranProbe, probeOK := e.runner.ran(rocmProductProbe)
		if !ranList || listOK || !ranProbe || !probeOK {
			t.Errorf("want %s to fail and %s to succeed; commands: %v", nvidiaListProbe, rocmProductProbe, e.runner.commands())
		}
		// Count from allocatable: inventory failed, so no device was listed.
		if _, ok := e.runner.ran(amdInventoryAgent); ok {
			t.Errorf("%s succeeded, so the count may not come from allocatable", amdInventoryAgent)
		}
		if got := allocatableGPUCount(node); got != int64(want.gpuCount) {
			t.Errorf("allocatable GPUs %d, want %d", got, want.gpuCount)
		}
	}
}

// A03: on the two socket Intel host at full load, a 120 W CPU cap writes
// 120000000 to both package PL1 files and touches neither disabled DRAM
// sub-zone, the status backend is rapl, and five seconds later each package
// draws at most 121.2 W by the emulator's account and by its energy_uj
// counter. RAPL applies the cap per socket, so the node as a whole draws
// about twice the cap (D7).
func TestHwemuA03RAPLCapsEachPackage(t *testing.T) {
	const capW, limit = 120.0, 121.2
	n, tree, p := hwemutest.NewNode(t, "intel-xeon-2s-rapl", nil)
	n.SetLoad(hwemu.Load{CPUUtil: []float64{1}})
	if err := n.Step(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	for _, pk := range n.State().Packages {
		if pk.PowerW <= limit {
			t.Fatalf("package %d draws %.1f W uncapped at full load; it must start above %.1f W for the cap to show", pk.Socket, pk.PowerW, limit)
		}
	}
	e := useEmulatedNode(t, tree, nil)
	dyn, err := reconcileEmulated(t, e.controller(t), e.nodeObject(p), nodeTwinWithCPUCap(e.name, capW))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}

	packages := []string{"intel-rapl:0", "intel-rapl:1"}
	// A write to a DRAM limit fails on its directory and leaves no trace,
	// so check what the agent aims at: the package PL1 files, nothing else.
	targets, err := dvfs.RAPLCapFiles()
	if err != nil {
		t.Fatal(err)
	}
	wantTargets := make([]string, len(packages))
	for i, zone := range packages {
		wantTargets[i] = filepath.Join(zoneDir(e, zone), "constraint_0_power_limit_uw")
	}
	if !slices.Equal(targets, wantTargets) {
		t.Errorf("the agent caps %v, want only the package PL1 files %v", targets, wantTargets)
	}
	for _, zone := range packages {
		if got := readTrimmed(t, filepath.Join(zoneDir(e, zone), "constraint_0_power_limit_uw")); got != "120000000" {
			t.Errorf("%s PL1 = %s, want 120000000", zone, got)
		}
		// The DRAM sub-zone's limit is a directory, as on the machine where
		// it is disabled; writing it would have failed.
		dram := filepath.Join(zoneDir(e, zone+":0"), "constraint_0_power_limit_uw")
		fi, err := os.Stat(dram)
		if err != nil || !fi.IsDir() {
			t.Errorf("%s is no longer the write-rejecting directory: %v", dram, err)
		} else if entries, _ := os.ReadDir(dram); len(entries) > 0 {
			t.Errorf("%s gained entries %v", dram, entries)
		}
	}
	if s := controlStatus(t, dyn, e.name, "cpu"); s["backend"] != "rapl" || s["result"] != "applied" {
		t.Fatalf("cpu control status %v, want backend rapl, result applied", s)
	}

	if err := n.Step(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, pk := range n.State().Packages {
		total += pk.PowerW
		if pk.PowerW > limit {
			t.Errorf("package %d draws %.3f W five seconds after a %.0f W cap, want <= %.1f W", pk.Socket, pk.PowerW, capW, limit)
		}
	}
	before := make([]int64, len(packages))
	for i, zone := range packages {
		before[i] = readFileInt(t, filepath.Join(zoneDir(e, zone), "energy_uj"))
	}
	if err := n.Step(time.Second); err != nil {
		t.Fatal(err)
	}
	for i, zone := range packages {
		after := readFileInt(t, filepath.Join(zoneDir(e, zone), "energy_uj"))
		delta := after - before[i]
		if delta < 0 {
			delta += readFileInt(t, filepath.Join(zoneDir(e, zone), "max_energy_range_uj"))
		}
		if w := float64(delta) / 1e6; w > limit {
			t.Errorf("%s energy_uj grew by %d uJ in one second, %.3f W, want <= %.1f W", zone, delta, w, limit)
		}
	}
	if total <= 1.5*capW {
		t.Fatalf("the node draws %.1f W in total under a %.0f W cap; RAPL applies the cap to each of its %d packages, "+
			"so it should draw about %.0f W (D7). If the cap now means the node total, update this test with the fix", total, capW, len(packages), 2*capW)
	}
}

// A04: on the energy-only EPYC host, which has no RAPL limit, the DVFS
// fallback holds the node at its cap. With a 250 ms cooldown, a 50 ms
// frequency ramp and a cap equal to the steady power at 30 % throttle, the
// throttle settles in {20, 30, 40} within 3 s, and over the next second the
// true package power averages within [cap-15, cap+10] W. Every reading goes
// through the agent's whole reconcileOnce, which hands the cap to DVFS, and
// DVFS compares it with the node total (D7).
//
// It skips while dvfs.EnergyFiles lists every package twice (D5): the agent
// then reads about twice the true power and throttles to the floor. F1 fixes
// that and this test runs from then on.
//
// The agent turns energy_uj deltas into watts over its own wall-clock
// interval, so the files must hold the energy of the instant it reads them.
// A step of this 384-CPU node takes tens of milliseconds, because it reads
// back every agent-writable file, so the loop runs in lockstep: it advances
// the node to a moment just ahead, waits for that moment, and only then lets
// the agent reconcile, which reads the energy files last. The loop asks for a
// reading every 25 ms and gets one as often as the node keeps up; a slow
// machine gets as many readings, later.
func TestHwemuA04DVFSHoldsTheEnergyOnlyHostAtItsCap(t *testing.T) {
	n, tree, p := hwemutest.NewNode(t, "amd-epyc-2s-energy-only", func(p *hwemu.Profile) {
		p.Physics.CPU.FreqRampMS = intFact(50)
	})
	load := hwemu.Load{CPUUtil: []float64{1}}
	capW := hwemutest.SteadyPackagePower(t, p, load, 30)
	e := useEmulatedNode(t, tree, map[string]string{"DVFS_COOLDOWN": "250ms"})

	files, err := dvfs.EnergyFiles()
	if err != nil {
		t.Fatal(err)
	}
	zones, err := dvfs.RAPLPackageZones()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(zones) {
		t.Logf("dvfs.EnergyFiles lists %d counters for %d package zones: %v", len(files), len(zones), files)
		t.Skip("EnergyFiles counts every package twice (D5); passes with F1")
	}

	n.SetLoad(load)
	reader, dyn := newTestClients(t, e.nodeObject(p), nodeTwinWithCPUCap(e.name, capW))
	nc := e.controller(t)
	ctx := context.Background()
	const (
		tick                   = 25 * time.Millisecond
		settleFor, holdFor     = 3 * time.Second, time.Second
		settleReads, holdReads = 30, 10
		maxLead, giveUp        = 2 * time.Second, time.Minute
	)
	lead := tick
	start := time.Now()
	next := start
	reads := 0
	var held []float64
	var holdStart time.Time
	for {
		next = next.Add(tick)
		if earliest := time.Now().Add(lead); next.Before(earliest) {
			next = earliest
		}
		if next.Sub(start) > giveUp {
			t.Fatalf("the controller read %d times in %v; the node cannot keep up", reads, giveUp)
		}
		if err := n.AdvanceTo(next); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(next) {
			// The step ended after the moment it stands for: give the
			// next one more time instead of letting the agent read late.
			lead = min(2*lead, maxLead)
			continue
		}
		time.Sleep(time.Until(next))
		// The agent's whole reconcile, so the cap reaches DVFS the way it
		// does in a deployed agent. It reads the energy files last.
		if err := reconcileOnce(ctx, reader, dyn, nc); err != nil {
			t.Fatalf("reconcileOnce: %v", err)
		}
		reads++
		elapsed := next.Sub(start)
		if holdStart.IsZero() {
			if elapsed < settleFor || reads < settleReads {
				continue
			}
			holdStart = next
		}
		if pct := nc.dvfs.ThrottlePct; pct != 20 && pct != 30 && pct != 40 {
			t.Fatalf("throttle %d%% after %v and %d readings, want 20, 30 or 40 once settled (cap %.1f W, true power %.1f W)",
				pct, elapsed.Round(time.Millisecond), reads, capW, totalPackagePower(n))
		}
		held = append(held, totalPackagePower(n))
		if next.Sub(holdStart) >= holdFor && len(held) >= holdReads {
			break
		}
	}
	mean := 0.0
	for _, w := range held {
		mean += w
	}
	mean /= float64(len(held))
	if mean < capW-15 || mean > capW+10 {
		t.Fatalf("true package power averages %.1f W over the hold window, want within [%.1f, %.1f] W", mean, capW-15, capW+10)
	}

	// RAPL has no limit to write on this host, so the cap went to DVFS.
	if s := controlStatus(t, dyn, e.name, "cpu"); s["backend"] != "dvfs" {
		t.Fatalf("cpu control status %v, want backend dvfs", s)
	}
}

func totalPackagePower(n *hwemu.Node) float64 {
	total := 0.0
	for _, pk := range n.State().Packages {
		total += pk.PowerW
	}
	return total
}

// A04b: dvfs.CPUFreqList lists every policy twice, once through each
// cpuN/cpufreq link and once as policyN (D6). On the Intel host, one policy
// per CPU, a 15 % throttle reaches 14 policies where 15 were intended: the
// odd entry count splits one policy's pair, and its second entry writes the
// maximum back. With policies shared per core, a 30 % throttle reaches none:
// the throttled entries all come first, and each sibling's later entry writes
// the maximum back. F1 makes both what was intended.
func TestHwemuA04bCPUFreqListCountsEveryPolicyTwice(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mutate        func(*hwemu.Profile)
		pct           int
		wantThrottled int
	}{
		{"per-cpu", nil, 15, 14},
		{"per-core", withPerCorePolicies, 30, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, tree, _ := hwemutest.NewNode(t, "intel-xeon-2s-rapl", tc.mutate)
			e := useEmulatedNode(t, tree, nil)
			const minFreqKHz = 1500000
			ctl, err := dvfs.New(dvfs.Config{MinFreqKHz: minFreqKHz}, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, cpuinfoMax := policyMaxFreqs(t, e)
			// Start every policy at its cpuinfo_min_freq, a value the agent
			// never writes here, so a policy it left alone cannot pass for
			// one whose maximum it wrote back.
			cpuinfoMin := setScalingMaxToCPUInfoMin(t, e)
			policies := len(cpuinfoMax)
			intended := int(math.Ceil(float64(policies) * float64(tc.pct) / 100))
			if _, err := ctl.ApplyThrottlePct(tc.pct, nil, 0); err != nil {
				t.Fatal(err)
			}
			scalingMax, _ := policyMaxFreqs(t, e)
			throttled := 0
			var unwritten []string
			for name, khz := range scalingMax {
				target := max(cpuinfoMin[name], min(cpuinfoMax[name], minFreqKHz))
				if khz != cpuinfoMax[name] && khz != target {
					unwritten = append(unwritten, name)
				}
				if khz < cpuinfoMax[name] {
					throttled++
				}
			}
			if len(unwritten) > 0 {
				sort.Strings(unwritten)
				t.Fatalf("%d of %d policies hold neither cpuinfo_max_freq nor the %d kHz throttle target, so the agent never wrote them: %v",
					len(unwritten), policies, minFreqKHz, unwritten)
			}
			if throttled != tc.wantThrottled {
				t.Fatalf("a %d%% throttle left %d of %d policies throttled (%d intended); CPUFreqList returned %d entries for %d policies. "+
					"Today it throttles %d, because CPUFreqList lists every policy twice (D6); if F1 landed, expect %d",
					tc.pct, throttled, policies, intended, len(ctl.Cpus), policies, tc.wantThrottled, intended)
			}
			t.Logf("%d%% throttle: %d of %d policies throttled, %d intended; CPUFreqList returned %d entries (D6)",
				tc.pct, throttled, policies, intended, len(ctl.Cpus))
		})
	}
}

// A05: on the H100 NVL node at full GPU load, a 250 W per-GPU cap runs
// nvidia-smi -i k -pl 250 for every GPU, sets every power_limit_mw to 250000
// and records host-nvidia applied; six seconds later every GPU's
// power_draw_mw is at most 252500 and below what it drew uncapped.
func TestHwemuA05NvidiaCapReachesEveryGPU(t *testing.T) {
	n, tree, p := hwemutest.NewNode(t, "nvidia-h100-nvl-8gpu", nil)
	n.SetLoad(hwemu.Load{GPUUtil: []float64{1}})
	if err := n.Step(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	e := useEmulatedNode(t, tree, nil)
	count := p.GPUs.Count.V
	uncapped := make([]int64, count)
	for k := range uncapped {
		uncapped[k] = readFileInt(t, nvmlFile(e, k, "power_draw_mw"))
	}
	nc := e.controller(t)
	e.runner.reset()
	dyn, err := reconcileEmulated(t, nc, e.nodeObject(p), nodeTwinWithGPUCap(e.name, float64Ptr(250), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	for k := 0; k < count; k++ {
		cmd := "nvidia-smi -i " + strconv.Itoa(k) + " -pl 250"
		if _, ok := e.runner.ran(cmd); !ok {
			t.Errorf("%q did not run successfully; commands: %v", cmd, e.runner.commands())
		}
		if got := readTrimmed(t, nvmlFile(e, k, "power_limit_mw")); got != "250000" {
			t.Errorf("gpu %d power_limit_mw = %s, want 250000", k, got)
		}
	}
	if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-nvidia" || s["result"] != "applied" {
		t.Fatalf("gpu control status %v, want backend host-nvidia, result applied", s)
	}

	if err := n.Step(6 * time.Second); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < count; k++ {
		draw := readFileInt(t, nvmlFile(e, k, "power_draw_mw"))
		if draw > 252500 || draw >= uncapped[k] {
			t.Errorf("gpu %d power_draw_mw = %d six seconds after a 250 W cap, want <= 252500 and below the uncapped %d", k, draw, uncapped[k])
		}
	}
}

// A05b: on the MI300X node with the legacy rocm-smi, the agent finds the
// vendor only through rocm-smi --showproductname, because it ignores the NFD
// accelerator-class label pci-1200_1002 (D8). A 500 W cap then sets every
// power1_cap to 500000000, and power settles at most at 505 W. The product the
// agent reads is the Card SKU, a VBIOS segment, not the product name (D4).
func TestHwemuA05bAMDVendorComesFromRocmSMI(t *testing.T) {
	n, tree, p := hwemutest.NewNode(t, "amd-instinct-mi300x-8gpu", withLegacyRocmSMI)
	n.SetLoad(hwemu.Load{GPUUtil: []float64{1}})
	if err := n.Step(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	e := useEmulatedNode(t, tree, nil)
	node := e.nodeObject(p)
	if node.Labels[amdAcceleratorNFD] != "true" {
		t.Fatalf("the node has no %s label, so this test proves nothing about it: %v", amdAcceleratorNFD, node.Labels)
	}
	if hasNFDGPUVendor(node.Labels, "1002") {
		t.Fatalf("the agent now reads an AMD vendor from the node labels; it ignored %s before (D8). Update this test with the fix", amdAcceleratorNFD)
	}

	nc := e.controller(t)
	e.runner.reset()
	dyn, err := reconcileEmulated(t, nc, node, nodeTwinWithGPUCap(e.name, float64Ptr(500), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if _, ok := e.runner.ran(rocmProductProbe); !ok {
		t.Fatalf("%s did not decide the vendor; commands: %v (D8)", rocmProductProbe, e.runner.commands())
	}
	if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-amd" || s["result"] != "applied" {
		t.Fatalf("gpu control status %v, want backend host-amd, result applied", s)
	}
	hwmons := amdgpuHwmonDirs(t, e)
	if len(hwmons) != p.GPUs.Count.V {
		t.Fatalf("%d amdgpu devices, want %d", len(hwmons), p.GPUs.Count.V)
	}
	for i, dir := range hwmons {
		if got := readTrimmed(t, filepath.Join(dir, "power1_cap")); got != "500000000" {
			t.Errorf("device %d power1_cap = %s, want 500000000", i, got)
		}
	}
	if err := n.Step(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	for i, dir := range hwmons {
		if w := float64(readFileInt(t, filepath.Join(dir, "power1_input"))) / 1e6; w > 505 {
			t.Errorf("device %d power1_input = %.3f W three seconds after a 500 W cap, want <= 505 W", i, w)
		}
	}

	hw := discoverHardware(context.Background(), node)
	sku := strings.Split(p.GPUs.VBIOSVersion.V, "-")[1]
	if hw.GPURawModel != sku {
		t.Fatalf("gpu rawModel %q; today the agent reads Card SKU, the VBIOS segment %q, not the product %q (D4). "+
			"If the agent now reads the product, update this test with the fix", hw.GPURawModel, sku, p.GPUs.Product.V)
	}
}

// A05c: on the MI300X node with the legacy rocm-smi and device 1 a secondary
// die, a 500 W cap is recorded as host-amd applied while device 1's
// power1_cap is unchanged: rocm-smi skips a secondary die and still exits 0
// (python_smi_tools/rocm_smi.py:1687-1689 of rocm_smi_lib at 323ab1105dce),
// and the agent takes exit 0 as success (D2).
func TestHwemuA05cSecondaryDieCapIsAnExitZeroNoOp(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "amd-instinct-mi300x-8gpu", variant(withLegacyRocmSMI, withSecondaryDie(1)))
	e := useEmulatedNode(t, tree, nil)
	hwmons := amdgpuHwmonDirs(t, e)
	before := readTrimmed(t, filepath.Join(hwmons[1], "power1_cap"))

	dyn, err := reconcileEmulated(t, e.controller(t), e.nodeObject(p), nodeTwinWithGPUCap(e.name, float64Ptr(500), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-amd" || s["result"] != "applied" {
		t.Fatalf("gpu control status %v; today the agent records host-amd applied, because rocm-smi exits 0 on a secondary die "+
			"and the agent takes exit 0 as success (D2). F3 checks the output instead: update this test with it", s)
	}
	if got := readTrimmed(t, filepath.Join(hwmons[1], "power1_cap")); got != before {
		t.Fatalf("secondary die power1_cap = %s, want it unchanged at %s: rocm-smi does not cap a secondary die", got, before)
	}
	for i, dir := range hwmons {
		if i == 1 {
			continue
		}
		if got := readTrimmed(t, filepath.Join(dir, "power1_cap")); got != "500000000" {
			t.Errorf("device %d power1_cap = %s, want 500000000", i, got)
		}
	}
}

// A05d: on the MI300X node with the legacy rocm-smi and a 600 W default below
// the 750 W maximum, a 700 W cap makes rocm-smi ask for confirmation. The
// agent passes no --autorespond and exec gives the tool an empty stdin, so it
// reads EOF and exits 1 (D3). The agent records host-amd error and no
// power1_cap changes.
func TestHwemuA05dRocmSMIPromptReadsEOF(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "amd-instinct-mi300x-8gpu", variant(withLegacyRocmSMI, withDefaultLimit(600)))
	e := useEmulatedNode(t, tree, nil)
	hwmons := amdgpuHwmonDirs(t, e)
	for i, dir := range hwmons {
		if got := readTrimmed(t, filepath.Join(dir, "power1_cap")); got != "600000000" {
			t.Fatalf("device %d starts at power1_cap %s, want the 600 W default", i, got)
		}
	}

	dyn, err := reconcileEmulated(t, e.controller(t), e.nodeObject(p), nodeTwinWithGPUCap(e.name, float64Ptr(700), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	s := controlStatus(t, dyn, e.name, "gpu")
	msg, _ := s["message"].(string)
	if s["backend"] != "host-amd" || s["result"] != "error" || !strings.Contains(msg, "EOF") {
		t.Fatalf("gpu control status %v; today rocm-smi prompts, reads EOF and exits 1, so the agent records host-amd error (D3)", s)
	}
	for i, dir := range hwmons {
		if got := readTrimmed(t, filepath.Join(dir, "power1_cap")); got != "600000000" {
			t.Errorf("device %d power1_cap = %s, want it unchanged at 600000000", i, got)
		}
	}
}

// A06: on the MI300X node with the current rocm-smi, the agent's inventory
// call fails, because no rocm-smi release has --showpowercap (D1). NodeHardware
// then counts 8 GPUs from allocatable, has no GPU control, and takes the model
// from the amd.com/gpu.family label, because the agent reads
// amd.com/gpu.product while the labeller publishes amd.com/gpu.product-name
// (D8); that family code matches no catalog entry. A GPU cap ends in backend
// none, result error, with the argparse message.
func TestHwemuA06AMDInventoryFailsOnCurrentRocmSMI(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "amd-instinct-mi300x-8gpu", nil)
	e := useEmulatedNode(t, tree, nil)
	node := e.nodeObject(p)
	if node.Labels[amdLabellerProduct] == "" {
		t.Fatalf("the node has no %s label, so this test proves nothing about it", amdLabellerProduct)
	}
	status := publishedStatusMap(t, node.Name, discoverHardware(context.Background(), node))
	gpu, _ := status["gpu"].(map[string]any)
	family := node.Labels["amd.com/gpu.family"]
	if gpu["count"] != float64(8) || gpu["controlAvailable"] != false || gpu["rawModel"] != family ||
		!containsAny(gpu["warnings"], "gpu model not recognized") {
		t.Fatalf("NodeHardware gpu %v; today inventory fails (D1), so count 8 comes from allocatable, controlAvailable is false, "+
			"and rawModel is the amd.com/gpu.family value %q with a gpu model not recognized warning (D8)", gpu, family)
	}

	dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithGPUCap(e.name, float64Ptr(500), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	s := controlStatus(t, dyn, e.name, "gpu")
	msg, _ := s["message"].(string)
	if s["backend"] != "none" || s["result"] != "error" || !strings.Contains(msg, "unrecognized arguments: --showpowercap") {
		t.Fatalf("gpu control status %v; today the inventory call fails argparse on every rocm-smi release (D1)", s)
	}
}

// A07: on the HSMP EPYC host the agent publishes no capRange and caps through
// DVFS, and the HSMP socket cap power1_cap stays where it was. There is no
// HSMP backend yet; this guards the one that comes.
func TestHwemuA07HSMPCapIsNotUsedYet(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "amd-epyc-2s-hsmp", nil)
	e := useEmulatedNode(t, tree, nil)
	caps := make([]string, p.CPU.Sockets.V)
	before := make([]string, len(caps))
	for s := range caps {
		caps[s] = filepath.Join(tree.SysRoot, filepath.FromSlash(layout.HSMPHwmonDir(s)), "power1_cap")
		before[s] = readTrimmed(t, caps[s])
	}
	node := e.nodeObject(p)
	if hw := discoverHardware(context.Background(), node); hw.CPUCapKnown {
		t.Fatalf("cpu capRange %v to %v W, want none: RAPL here only counts energy", hw.CPUCapMinWatts, hw.CPUCapMaxWatts)
	}
	dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithCPUCap(e.name, 600))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if s := controlStatus(t, dyn, e.name, "cpu"); s["backend"] != "dvfs" {
		t.Fatalf("cpu control status %v, want backend dvfs", s)
	}
	for s, f := range caps {
		if got := readTrimmed(t, f); got != before[s] {
			t.Errorf("socket %d HSMP power1_cap = %s, want it unchanged at %s: the agent has no HSMP backend", s, got, before[s])
		}
	}
}

// A08: with intel-rapl:0's energy_uj 1 J below its range, the counter wraps
// between the agent's two readings, and the per-zone
// joulie_rapl_estimated_power_watts for intel-rapl:0 is within 1 % of the
// emulated package power over the agent's own interval. The total is not
// asserted: it counts every package twice until F1 (D5).
func TestHwemuA08RAPLPowerSurvivesTheCounterWrap(t *testing.T) {
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	p := profiles["intel-xeon-2s-rapl"]
	const zone = "intel-rapl:0"
	rangeUJ := hwemu.MaxEnergyRangeUJ(p.Powercap.ControlTypes[0].Zones[0].EnergyUnitNJ.V)
	startUJ := rangeUJ - 1_000_000
	tree, err := hwemu.Render(p, filepath.Join(t.TempDir(), "node"), hwemu.RenderOptions{
		NodeName:      "hwemu-a08-rapl-wrap",
		EnergyStartUJ: map[string]int64{zone: startUJ},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := useEmulatedNode(t, tree, nil)
	nc := e.controller(t)
	// The node's clock starts where the agent first reads: the files then
	// hold what Render wrote, and the node is advanced by exactly the wall
	// time that passed before the second reading.
	origin := time.Unix(0, 0)
	n, err := hwemu.NewNode(p, tree, hwemu.Options{Start: origin})
	if err != nil {
		t.Fatal(err)
	}
	n.SetLoad(hwemu.Load{CPUUtil: []float64{1}})
	energyFile := filepath.Join(zoneDir(e, zone), "energy_uj")

	first := n.State().Packages[0].EnergyUJ
	readAt := time.Now()
	if _, _, err := nc.dvfs.ReadPowerWatts(); err != nil {
		t.Fatal(err)
	}
	sampled, ok := nc.dvfs.Samples[energyFile]
	if !ok || sampled.LastUJ != startUJ {
		t.Fatalf("the agent's first reading of %s is %+v (found %v), want %d", energyFile, sampled, ok, startUJ)
	}
	time.Sleep(500 * time.Millisecond)
	if err := n.AdvanceTo(origin.Add(time.Since(readAt))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := nc.dvfs.ReadPowerWatts(); err != nil {
		t.Fatal(err)
	}
	if after := readFileInt(t, energyFile); after >= startUJ {
		t.Fatalf("%s reads %d after the interval, not below the %d it started at: the counter did not wrap", energyFile, after, startUJ)
	}

	interval := nc.dvfs.Samples[energyFile].LastTime.Sub(sampled.LastTime).Seconds()
	want := float64(n.State().Packages[0].EnergyUJ-first) / 1e6 / interval
	got := testutil.ToFloat64(raplPowerMetric.WithLabelValues(e.name, zone))
	if math.Abs(got-want) > 0.01*want {
		t.Fatalf("joulie_rapl_estimated_power_watts{zone=%q} = %.3f W across the wrap, want %.3f W within 1 %%", zone, got, want)
	}
	t.Logf("%s across the wrap: agent %.3f W, emulated %.3f W over %.3f s", zone, got, want, interval)
}

// A09: on the VM, which has neither powercap nor cpufreq, a CPU cap is
// recorded as blocked.
func TestHwemuA09VMCapIsBlocked(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "vm-no-powercap", nil)
	e := useEmulatedNode(t, tree, nil)
	dyn, err := reconcileEmulated(t, e.controller(t), e.nodeObject(p), nodeTwinWithCPUCap(e.name, 120))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	const want = "RAPL unavailable, no cpufreq host control, and no HTTP control backend"
	if s := controlStatus(t, dyn, e.name, "cpu"); s["backend"] != "none" || s["result"] != "blocked" || s["message"] != want {
		t.Fatalf("cpu control status %v, want backend none, result blocked, message %q", s, want)
	}
}

// A10: on the Intel host at full load with both package PL1 limits locked by
// the BIOS, every RAPL write fails, reconcileOnce returns the error, no DVFS
// fallback touches scaling_max_freq, and NodeTwin gets no CPU control status:
// the cap is never enforced and nothing says so (D10).
//
// The DVFS controller needs two energy readings before it acts, so one
// reconcile would pass even with a fallback in place. The test reconciles
// three times, a second of node time apart, with no cooldown and a trip count
// of one: a fallback would throttle from the second reconcile on.
func TestHwemuA10LockedRAPLLeavesTheCapUnenforced(t *testing.T) {
	n, tree, p := hwemutest.NewNode(t, "intel-xeon-2s-rapl", withLockedPL1)
	n.SetLoad(hwemu.Load{CPUUtil: []float64{1}})
	e := useEmulatedNode(t, tree, map[string]string{"DVFS_COOLDOWN": "0s", "DVFS_TRIP_COUNT": "1"})
	before, _ := policyMaxFreqs(t, e)

	nc := e.controller(t)
	reader, dyn := newTestClients(t, e.nodeObject(p), nodeTwinWithCPUCap(e.name, 120))
	for i := 0; i < 3; i++ {
		if err := n.Step(time.Second); err != nil {
			t.Fatal(err)
		}
		err := reconcileOnce(context.Background(), reader, dyn, nc)
		if err == nil || !strings.Contains(err.Error(), "write rapl cap") {
			t.Fatalf("reconcile %d returned %v; today a locked PL1 fails every write and the error ends the reconcile (D10)", i+1, err)
		}
	}
	after, _ := policyMaxFreqs(t, e)
	for policy, khz := range before {
		if after[policy] != khz {
			t.Errorf("%s scaling_max_freq %d, was %d: today no DVFS fallback runs after a RAPL write failure (D10)", policy, after[policy], khz)
		}
	}
	if s := controlStatus(t, dyn, e.name, "cpu"); s != nil {
		t.Fatalf("cpu control status %v; today the agent writes none when RAPL is locked (D10)", s)
	}
}

// A11: on the Intel host without NFD, the CPU vendor comes out empty, because
// it is read only from NFD labels (D9). NodeHardware still publishes the
// 165 W capRange and controlAvailable true, but a 120 W cap skips RAPL, which
// needs a known vendor: the PL1 files keep 165000000 and the status backend is
// dvfs.
func TestHwemuA11WithoutNFDRAPLIsSkipped(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "intel-xeon-2s-rapl", withoutNFD)
	e := useEmulatedNode(t, tree, nil)
	node := e.nodeObject(p)
	for k := range node.Labels {
		if strings.HasPrefix(k, "feature.node.kubernetes.io/") {
			t.Fatalf("node without NFD has label %s", k)
		}
	}
	status := publishedStatusMap(t, node.Name, discoverHardware(context.Background(), node))
	cpu, _ := status["cpu"].(map[string]any)
	capRange, _ := cpu["capRange"].(map[string]any)
	if cpu["vendor"] != "" || capRange["maxWattsPerSocket"] != float64(165) || cpu["controlAvailable"] != true {
		t.Fatalf("NodeHardware cpu %v; today the vendor is empty without NFD (D9) while capRange 165 and controlAvailable true are published", cpu)
	}

	dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithCPUCap(e.name, 120))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	for _, zone := range []string{"intel-rapl:0", "intel-rapl:1"} {
		if got := readTrimmed(t, filepath.Join(zoneDir(e, zone), "constraint_0_power_limit_uw")); got != "165000000" {
			t.Errorf("%s PL1 = %s; today RAPL is skipped without a vendor and the limit stays 165000000 (D9)", zone, got)
		}
	}
	if s := controlStatus(t, dyn, e.name, "cpu"); s["backend"] != "dvfs" {
		t.Fatalf("cpu control status %v; today the cap falls to DVFS without a vendor (D9)", s)
	}
}

// A12: on the H100 NVL node exposed as MIG devices of mixed strategy, or as
// renamed time-sliced replicas, the node has no resource the agent counts as
// a GPU, so a GPU cap is blocked with node has no allocatable GPU resources
// while nvidia-smi works, and no power_limit_mw changes.
func TestHwemuA12SharedGPUExposureBlocksTheCap(t *testing.T) {
	for _, mode := range []string{"mig-mixed", "timeslice-rename"} {
		t.Run(mode, func(t *testing.T) {
			_, tree, p := hwemutest.NewNode(t, "nvidia-h100-nvl-8gpu", withExposure(mode))
			e := useEmulatedNode(t, tree, nil)
			before := make([]string, p.GPUs.Count.V)
			for k := range before {
				before[k] = readTrimmed(t, nvmlFile(e, k, "power_limit_mw"))
			}
			dyn, err := reconcileEmulated(t, e.controller(t), e.nodeObject(p), nodeTwinWithGPUCap(e.name, float64Ptr(250), nil))
			if err != nil {
				t.Fatalf("reconcileOnce: %v", err)
			}
			if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "none" || s["result"] != "blocked" || s["message"] != "node has no allocatable GPU resources" {
				t.Fatalf("gpu control status %v, want backend none, result blocked, node has no allocatable GPU resources", s)
			}
			if out, err := e.runner.Run(context.Background(), "nvidia-smi", "-L"); err != nil {
				t.Fatalf("nvidia-smi -L on the emulated node: %v (%s)", err, out)
			}
			for k := range before {
				if got := readTrimmed(t, nvmlFile(e, k, "power_limit_mw")); got != before[k] {
					t.Errorf("gpu %d power_limit_mw = %s, want it unchanged at %s", k, got, before[k])
				}
			}
		})
	}
}

// A13: with the platform zone psys next to the packages, dvfs.EnergyFiles
// lists psys's energy_uj, because it selects zones by their one colon, so the
// DVFS fallback sums platform energy as package power (D11).
func TestHwemuA13PsysEnergyCountsAsPackageEnergy(t *testing.T) {
	_, tree, _ := hwemutest.NewNode(t, "intel-xeon-2s-rapl", withPsys)
	useEmulatedNode(t, tree, nil)
	files, err := dvfs.EnergyFiles()
	if err != nil {
		t.Fatal(err)
	}
	var psys []string
	for _, f := range files {
		if readTrimmed(t, filepath.Join(filepath.Dir(f), "name")) == "psys" {
			psys = append(psys, f)
		}
	}
	if len(psys) == 0 {
		t.Fatalf("dvfs.EnergyFiles %v has no psys counter; today it includes psys, a one-colon zone, as package energy (D11)", files)
	}
}

// A14: with an intel-rapl-mmio zone named package-0 next to the MSR zones,
// dvfs.RAPLCapFiles includes it, and a 120 W cap writes it as well: socket 0
// is capped a second time through MMIO (D11).
func TestHwemuA14MMIOZoneIsCappedTwice(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "intel-xeon-2s-rapl", withMMIO)
	e := useEmulatedNode(t, tree, nil)
	mmio := filepath.Join(zoneDir(e, "intel-rapl-mmio:0"), "constraint_0_power_limit_uw")
	files, err := dvfs.RAPLCapFiles()
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(files, mmio) {
		t.Fatalf("dvfs.RAPLCapFiles %v lacks %s; today it selects every zone named package-*, the MMIO one included (D11)", files, mmio)
	}
	if _, err := reconcileEmulated(t, e.controller(t), e.nodeObject(p), nodeTwinWithCPUCap(e.name, 120)); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if got := readTrimmed(t, mmio); got != "120000000" {
		t.Fatalf("intel-rapl-mmio:0 PL1 = %s; today the cap is written there too (D11)", got)
	}
}

// A15: on two packages of two dies each, dvfs.RAPLCapFiles returns the four
// package-X-die-Y zones, and a 120 W cap puts the full 120 W on each die
// rather than sharing it between the dies of a socket (D11).
func TestHwemuA15EveryDieTakesTheFullSocketCap(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "intel-xeon-2s-rapl", withDies(2))
	e := useEmulatedNode(t, tree, nil)
	files, err := dvfs.RAPLCapFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("dvfs.RAPLCapFiles %v, want the 4 die zones", files)
	}
	if _, err := reconcileEmulated(t, e.controller(t), e.nodeObject(p), nodeTwinWithCPUCap(e.name, 120)); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	for _, f := range files {
		if got := readTrimmed(t, f); got != "120000000" {
			t.Errorf("%s = %s; today every die zone takes the full per-socket cap, 120000000 (D11)", f, got)
		}
	}
}

// A16: on the H100 NVL node whose minimum and maximum power limits read
// [N/A], nvidia-smi prints the token and the agent parses it as 0 W (D16):
// NodeHardware has no capRangePerGpu and controlAvailable false, and a
// percent cap is blocked because it cannot be resolved without a maximum.
func TestHwemuA16UnsupportedNvidiaLimitsBlockAPercentCap(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "nvidia-h100-nvl-8gpu", withUnsupportedLimits)
	e := useEmulatedNode(t, tree, nil)
	node := e.nodeObject(p)

	// The symptoms below look the same whether the tool prints [N/A] or 0,
	// so pin both ends: the tool prints the token, and the agent records it
	// as a 0 W limit.
	args := strings.Fields(nvidiaQueryAgent)
	out, err := e.runner.Run(context.Background(), args[0], args[1:]...)
	if err != nil {
		t.Fatalf("%s: %v (%s)", nvidiaQueryAgent, err, out)
	}
	rows := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(rows) != p.GPUs.Count.V {
		t.Fatalf("the inventory query printed %d rows, want %d:\n%s", len(rows), p.GPUs.Count.V, out)
	}
	for _, row := range rows {
		if f := splitCSVLine(row); len(f) < 3 || strings.TrimSpace(f[1]) != "[N/A]" || strings.TrimSpace(f[2]) != "[N/A]" {
			t.Fatalf("inventory row %q, want [N/A] for power.min_limit and power.max_limit", row)
		}
	}
	devices, err := listNvidiaDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		if d.MinCapWatts != 0 || d.MaxCapWatts != 0 {
			t.Fatalf("gpu %d limits %v to %v W; today [N/A] parses as 0 W (D16). F3 handles the token: update this test with it",
				d.Index, d.MinCapWatts, d.MaxCapWatts)
		}
	}

	status := publishedStatusMap(t, node.Name, discoverHardware(context.Background(), node))
	gpu, _ := status["gpu"].(map[string]any)
	if _, ok := gpu["capRangePerGpu"]; ok || gpu["controlAvailable"] != false {
		t.Fatalf("NodeHardware gpu %v; today [N/A] limits parse as 0, so there is no capRangePerGpu and no control (D16)", gpu)
	}
	dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithGPUCap(e.name, nil, float64Ptr(80)))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	const want = "cannot resolve capPctOfMax without device max power limits"
	if s := controlStatus(t, dyn, e.name, "gpu"); s["result"] != "blocked" || s["message"] != want {
		t.Fatalf("gpu control status %v; today a percent cap is blocked with %q (D16)", s, want)
	}
}

// publishedStatusMap is publishedNodeHardwareStatus decoded.
func publishedStatusMap(t *testing.T, nodeName string, hw HardwareInfo) map[string]any {
	t.Helper()
	var status map[string]any
	if err := json.Unmarshal(publishedNodeHardwareStatus(t, nodeName, hw), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

// containsAny reports whether list, a decoded JSON array, holds want.
func containsAny(list any, want string) bool {
	items, _ := list.([]any)
	for _, v := range items {
		if v == want {
			return true
		}
	}
	return false
}
