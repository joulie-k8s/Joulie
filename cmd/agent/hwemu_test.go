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
//   - D1 (fixed): the AMD inventory ran rocm-smi --showpowercap, which no
//     rocm-smi release has, so every AMD GPU cap ended in an error. The
//     agent lists AMD GPUs and their cap range from the amdgpu hwmon files
//     (listAmdDevices in amdgpu.go) and runs no AMD tool to cap; A02, A06,
//     A06b and A19 guard it.
//   - D2 (fixed): rocm-smi's exit 0 was taken as applied, while rocm-smi
//     exits 0 when it rejects a set. applyAmdGPUCap reads every write back
//     and never writes a device whose power1_cap reads 0 (a secondary die);
//     A06c, A34, A34b and TestAMDGPUSysfsInventoryAndWrite guard it. hwemu
//     renders a secondary die as the kernel shows it (power1_cap and
//     power1_cap_default 0, the package's range, every write refused with
//     EINVAL), and A34 and A34b run the agent over the four MI250X of the
//     test profile in testdata/hwemu-profiles.
//   - D3 (fixed): rocm-smi --setpoweroverdrive asked for confirmation above
//     max(current, default) and read EOF. No tool runs, so a cap above
//     power1_cap_default is written like any other; A05d guards it.
//   - D4 (fixed): the model came from Card SKU, a VBIOS segment, and the
//     maximum from --showmaxpower, which reports the current cap. The model
//     comes from the labeller's labels and the range from power1_cap_min and
//     power1_cap_max; A02, A05b and TestAMDGPURangeIgnoresTheDefaultTheDriverResets
//     guard it.
//   - D5 (fixed): dvfs.EnergyFiles deduped by path string, so it listed every
//     package twice, through class/powercap and devices/virtual/powercap. It
//     now dedupes by resolved path; A04 and the dvfs D03 test guard it.
//   - D6 (fixed): dvfs.CPUFreqList listed every policy twice, through cpuN/cpufreq and
//     as policyN. An odd entry count throttles one policy fewer than
//     intended; with policies shared per core, the later entry of each
//     sibling writes the maximum back.
//   - D7: one CPU cap has two meanings: applyRAPLPackageCap writes it to each
//     package, while the DVFS fallback (dvfs.Controller.Reconcile) compares
//     it with the node total.
//   - D8: hasNFDGPUVendor ignores the NFD accelerator class pci-1200_<vendor>.
//     Its other half is fixed: the GPU model was read from amd.com/gpu.product
//     while the AMD labeller publishes amd.com/gpu.product-name, so an AMD
//     node took the family code as its model; gpu_label_regression_test.go
//     guards it.
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
//   - D13: the simulator's curve-model cap solver never throttled, because
//     the curve's power was clamped to the cap. Fixed by F2; its regression
//     tests are in simulator/cmd/simulator/cpupower_regression_test.go.
//   - D14: some hardware catalog curves and minimum power limits matched no
//     public source. Fixed in pkg/hwinv/assets/hardware.yaml, which now names
//     the source of each value or says that none is published; its regression
//     tests are in pkg/hwinv/catalog_test.go.
//   - D15: the agent DaemonSet has no tolerations and an amd64-only image, so
//     a tainted or arm64 node runs no agent.
//   - D16: listNvidiaDevices parses nvidia-smi's [N/A] limits as 0 W, so the
//     cap range is unknown and a percent cap cannot be resolved.
//   - D18 (fixed): allocatableGPUCount counted only <vendor>/gpu resources,
//     so a node exposing its GPUs as nvidia.com/mig-<profile> or
//     nvidia.com/gpu.shared blocked every GPU cap ("node has no allocatable
//     GPU resources") and, without GPU tools, published no GPUs. Resources
//     are classified by pkg/api.GPUResourceKind and the count is read from
//     the labels first; A12, A12b and A12c guard it.
//
// The fixes that flip them:
//
//   - F1: document that a CPU cap is per socket (D7). EnergyFiles and
//     CPUFreqList are deduplicated by resolved path (D5, D6).
//   - F2: the simulator uses phys.AnalyticCPUModel, SolveFreqScaleForCap and
//     FirstOrderToward instead of its own copies (D13).
//   - F3: AMD caps through the amdgpu hwmon files, each write read back
//     (D1 to D4, fixed), and nvidia-smi's [N/A] read as unknown (D16).
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
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/agent/dvfs"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
)

// hwemuGoldenDir holds the published NodeHardware status of each generic
// profile and of the MI250X test profile (A34), generated with -update and
// never hand edited.
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
	// energyZones names the powercap zones whose counters dvfs.EnergyFiles
	// sums as CPU energy, and capZones the zones a CPU cap writes through
	// dvfs.RAPLCapFiles, each in the order the agent lists them; nil when
	// there are none. Today psys counts as package energy and every
	// package-X-die-Y zone takes the full socket cap (D11).
	energyZones []string
	capZones    []string
}

const (
	nvidiaPCILabel     = "feature.node.kubernetes.io/pci-0302_10de.present"
	rocmProductProbe   = "rocm-smi --showproductname"
	nvidiaListProbe    = "nvidia-smi -L"
	nvidiaQueryAgent   = "nvidia-smi " + nvidiaInventoryQuery + " --format=csv,noheader,nounits"
	amdAcceleratorNFD  = "feature.node.kubernetes.io/pci-1200_1002.present"
	amdLabellerProduct = "amd.com/gpu.product-name"
	amdDisplayNFD      = "feature.node.kubernetes.io/pci-0300_1002.present"
)

// The powercap zone lists of a02Paths, in the order the agent lists them.
var (
	a02Packages        = []string{"package-0", "package-1"}
	a02PackagesAndPsys = []string{"package-0", "psys", "package-1"}
	a02Dies            = []string{"package-0-die-0", "package-0-die-1", "package-1-die-0", "package-1-die-1"}
)

// a02Paths is the road each generic profile takes. Each GPU profile runs on
// one of the CPU hosts, so it publishes that host's CPU block next to its
// GPU block: the 8-GPU H100 profiles on the Xeon 8260 RAPL host, the 4-GPU
// SXM profile on the Xeon 6530 RAPL host, the L40S and the W7900 on the
// energy-only EPYC 9534 host and the MI300X on the energy-only EPYC 9654
// host.
var a02Paths = map[string]a02Path{
	"intel-xeon-2s-rapl": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 165, cpuDriverFamily: "intel_pstate", cpuControl: true, cpuModel: "INTEL_XEON_PLATINUM_8260",
		gpuVendor:   "none",
		energyZones: a02Packages, capZones: a02Packages,
	},
	"intel-xeon-6530-2s-rapl": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 270, cpuDriverFamily: "intel_cpufreq", cpuControl: true, cpuModel: "INTEL_XEON_GOLD_6530",
		gpuVendor:   "none",
		energyZones: a02Packages, capZones: a02Packages,
	},
	"intel-xeon-2s-psys": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 270, cpuDriverFamily: "intel_pstate", cpuControl: true, cpuModel: "INTEL_XEON_GOLD_6530",
		gpuVendor:   "none",
		energyZones: a02PackagesAndPsys, capZones: a02Packages,
	},
	"intel-xeon-2s-multidie": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 200, cpuDriverFamily: "intel_pstate", cpuControl: true,
		gpuVendor:   "none",
		energyZones: a02Dies, capZones: a02Dies,
	},
	"intel-xeon6-2s-no-powercap": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuDriverFamily: "intel_pstate", cpuControl: true,
		gpuVendor: "none",
	},
	"intel-xeon6-2s-tpmi": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 350, cpuDriverFamily: "intel_pstate", cpuControl: true,
		gpuVendor:   "none",
		energyZones: a02PackagesAndPsys, capZones: a02Packages,
	},
	"amd-epyc-2s-energy-only": {
		cpuVendor: "AuthenticAMD", cpuSockets: 2, cpuDriverFamily: "acpi-cpufreq", cpuControl: true, cpuModel: "AMD_EPYC_9654",
		gpuVendor:   "none",
		energyZones: a02Packages,
	},
	"amd-epyc-9534-2s-energy-only": {
		cpuVendor: "AuthenticAMD", cpuSockets: 2, cpuDriverFamily: "acpi-cpufreq", cpuControl: true, cpuModel: "AMD_EPYC_9534",
		gpuVendor:   "none",
		energyZones: a02Packages,
	},
	"amd-epyc-2s-hsmp": {
		cpuVendor: "AuthenticAMD", cpuSockets: 2, cpuDriverFamily: "amd-pstate-epp", cpuControl: true, cpuModel: "AMD_EPYC_9655",
		gpuVendor:   "none",
		energyZones: a02Packages,
	},
	"nvidia-h100-nvl-8gpu": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 165, cpuDriverFamily: "intel_pstate", cpuControl: true, cpuModel: "INTEL_XEON_PLATINUM_8260",
		gpuVendor: "nvidia", gpuCount: 8, gpuCapMinW: 200, gpuCapMaxW: 400, gpuControl: true, gpuModel: "NVIDIA_H100_NVL", gpuVendorFrom: nvidiaPCILabel,
		energyZones: a02Packages, capZones: a02Packages,
	},
	"nvidia-h100-sxm-8gpu": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 165, cpuDriverFamily: "intel_pstate", cpuControl: true, cpuModel: "INTEL_XEON_PLATINUM_8260",
		gpuVendor: "nvidia", gpuCount: 8, gpuCapMinW: 200, gpuCapMaxW: 700, gpuControl: true, gpuModel: "NVIDIA_H100_SXM", gpuVendorFrom: nvidiaPCILabel,
		energyZones: a02Packages, capZones: a02Packages,
	},
	"nvidia-h100-sxm-4gpu": {
		cpuVendor: "GenuineIntel", cpuSockets: 2, cpuCapMaxW: 270, cpuDriverFamily: "intel_cpufreq", cpuControl: true, cpuModel: "INTEL_XEON_GOLD_6530",
		gpuVendor: "nvidia", gpuCount: 4, gpuCapMinW: 200, gpuCapMaxW: 700, gpuControl: true, gpuModel: "NVIDIA_H100_SXM", gpuVendorFrom: nvidiaPCILabel,
		energyZones: a02Packages, capZones: a02Packages,
	},
	"nvidia-l40s-4gpu": {
		cpuVendor: "AuthenticAMD", cpuSockets: 2, cpuDriverFamily: "acpi-cpufreq", cpuControl: true, cpuModel: "AMD_EPYC_9534",
		gpuVendor: "nvidia", gpuCount: 4, gpuCapMinW: 100, gpuCapMaxW: 350, gpuControl: true, gpuModel: "NVIDIA_L40S", gpuVendorFrom: nvidiaPCILabel,
		energyZones: a02Packages,
	},
	"amd-instinct-mi300x-8gpu": {
		cpuVendor: "AuthenticAMD", cpuSockets: 2, cpuDriverFamily: "acpi-cpufreq", cpuControl: true, cpuModel: "AMD_EPYC_9654",
		gpuVendor: "amd", gpuCount: 8, gpuCapMinW: 0, gpuCapMaxW: 750, gpuControl: true, gpuModel: "AMD_INSTINCT_MI300X", gpuVendorFrom: rocmProductProbe,
		energyZones: a02Packages,
	},
	"amd-radeon-pro-w7900-4gpu": {
		cpuVendor: "AuthenticAMD", cpuSockets: 2, cpuDriverFamily: "acpi-cpufreq", cpuControl: true, cpuModel: "AMD_EPYC_9534",
		gpuVendor: "amd", gpuCount: 4, gpuCapMinW: 241, gpuCapMaxW: 241, gpuControl: true, gpuModel: "AMD_RADEON_PRO_W7900", gpuVendorFrom: amdDisplayNFD,
		energyZones: a02Packages,
	},
	"vm-no-powercap": {
		cpuVendor: "GenuineIntel", cpuSockets: 8,
		gpuVendor: "none",
	},
}

// A02: each generic profile publishes the NodeHardware status pinned in
// testdata/hwemu/<profile>.json, and takes its family's road: Intel RAPL
// gives a capRange of the package TDP (165 W on the 8260, 270 W on the 6530
// with or without psys, 350 W on the Xeon 6787P through TPMI; min 0, since
// RAPL has no min file) and controls through intel_pstate or intel_cpufreq;
// psys, where present, is summed as package energy (D11); the two-die 9282
// publishes its 200 W per-die PL1 maximum as the per-socket maximum, and a
// cap writes all of it to each die (D11); the 6787P without powercap gives
// no capRange and controls through cpufreq (DVFS); the energy-only and HSMP
// EPYC hosts give no capRange and control through the cpufreq driver; H100
// NVL and SXM and L40S give their per-GPU range, with the vendor from the
// NFD 0302 label; MI300X and W7900 count their GPUs from the amdgpu hwmon
// devices and publish their cap range from them with control, 0 to 750 W
// and the pinned 241 to 241 W, and run no rocm-smi cap command; the MI300X
// gets its vendor from rocm-smi --showproductname (D8), the W7900 from the
// NFD display-class label pci-0300_1002; the VM reads one socket per vCPU
// and has no control. Every CPU and GPU model string matches its hardware
// catalog entry, except the VM's CPU, the 9282 and the 6787P, which the
// catalog does not list. amdgpu sysfs names no product, so the AMD GPU model
// comes from amd.com/gpu.product-name, which names the MI300X
// AMD_Instinct_MI300X_OAM, a catalog alias of AMD_INSTINCT_MI300X.
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
	// The D11 notes below apply only to the rows D11 shapes: a psys zone
	// among the energy zones, die zones among the cap zones.
	var psysNote, diesNote string
	if slices.Contains(want.energyZones, "psys") {
		psysNote = " (today a top-level psys zone counts as package energy, D11)"
	}
	if slices.ContainsFunc(want.capZones, func(z string) bool { return strings.Contains(z, "-die-") }) {
		diesNote = " (today every package-X-die-Y zone takes the full per-socket cap and publishes its per-die maximum as the per-socket one, D11)"
	}
	switch {
	case want.cpuCapMaxW == 0 && hw.CPUCapKnown:
		t.Errorf("cpu capRange %v to %v W, want none", hw.CPUCapMinWatts, hw.CPUCapMaxWatts)
	case want.cpuCapMaxW > 0 && (!hw.CPUCapKnown || hw.CPUCapMaxWatts != want.cpuCapMaxW || hw.CPUCapMinWatts != 0):
		t.Errorf("cpu capRange known=%v %v to %v W, want 0 to %v W%s", hw.CPUCapKnown, hw.CPUCapMinWatts, hw.CPUCapMaxWatts, want.cpuCapMaxW, diesNote)
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

	energy, err := dvfs.EnergyFiles()
	if err != nil {
		t.Fatal(err)
	}
	caps, err := dvfs.RAPLCapFiles()
	if err != nil {
		t.Fatal(err)
	}
	if got := zoneNames(t, energy); !slices.Equal(got, want.energyZones) {
		t.Errorf("dvfs.EnergyFiles sums zones %v, want %v%s", got, want.energyZones, psysNote)
	}
	if got := zoneNames(t, caps); !slices.Equal(got, want.capZones) {
		t.Errorf("a CPU cap writes zones %v, want %v%s", got, want.capZones, diesNote)
	}
	// Each of those zones takes the whole per-socket cap: on two dies per
	// package that is twice the cap per socket (D11). The golden is built
	// from hw, so the write does not change it.
	if len(caps) > 0 {
		const capW = 120.0
		if _, n, err := applyRAPLPackageCap(hw, capW); err != nil || n != len(caps) {
			t.Errorf("a %v W cap wrote %d of the %d zones: %v", capW, n, len(caps), err)
		}
		for _, f := range caps {
			if got := readTrimmed(t, f); got != "120000000" {
				t.Errorf("%s PL1 = %s after a %v W cap, want 120000000%s", filepath.Base(filepath.Dir(f)), got, capW, diesNote)
			}
		}
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
	case amdDisplayNFD:
		// The display-class label answers detectGPUVendor before any probe
		// runs.
		if node.Labels[amdDisplayNFD] != "true" {
			t.Errorf("node has no %s label: %v", amdDisplayNFD, node.Labels)
		}
		for _, probe := range []string{nvidiaListProbe, rocmProductProbe} {
			if ran, _ := e.runner.ran(probe); ran {
				t.Errorf("%s ran, so the vendor did not come from %s; commands: %v", probe, amdDisplayNFD, e.runner.commands())
			}
		}
		checkA02AMDInventory(t, e, hw, want)
	case rocmProductProbe:
		ranList, listOK := e.runner.ran(nvidiaListProbe)
		ranProbe, probeOK := e.runner.ran(rocmProductProbe)
		if !ranList || listOK || !ranProbe || !probeOK {
			t.Errorf("want %s to fail and %s to succeed; commands: %v", nvidiaListProbe, rocmProductProbe, e.runner.commands())
		}
		checkA02AMDInventory(t, e, hw, want)
	}
}

// checkA02AMDInventory checks that the AMD GPUs were listed from the amdgpu
// hwmon devices, one per GPU of the profile, and that no rocm-smi command
// but the vendor probe ran.
func checkA02AMDInventory(t *testing.T, e *emulatedNode, hw HardwareInfo, want a02Path) {
	t.Helper()
	if hw.GPUListErr != nil || len(hw.GPUDevices) != want.gpuCount || len(amdgpuHwmonDirs(t, e)) != want.gpuCount {
		t.Errorf("listed %d AMD GPUs (%v) on a node with %d amdgpu hwmon devices, want %d", len(hw.GPUDevices), hw.GPUListErr,
			len(amdgpuHwmonDirs(t, e)), want.gpuCount)
	}
	if calls := e.amdToolCalls(); len(calls) > 0 {
		t.Errorf("the agent ran %v; it runs no AMD tool but the vendor probe %s", calls, rocmProductProbe)
	}
}

// zoneNames reads the name file of the powercap zone each of files lies in;
// nil for no files.
func zoneNames(t *testing.T, files []string) []string {
	t.Helper()
	var names []string
	for _, f := range files {
		names = append(names, readTrimmed(t, filepath.Join(filepath.Dir(f), "name")))
	}
	return names
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
	// A write to a DRAM limit is refused and reverted, so check what the
	// agent aims at, the package PL1 files and nothing else, and that the
	// emulator saw no write to a DRAM limit.
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
		// The DRAM sub-zone's limit is disabled, as on the machine the
		// profile comes from: it reads 0 and refuses writes.
		if got := readTrimmed(t, filepath.Join(zoneDir(e, zone+":0"), "constraint_0_power_limit_uw")); got != "0" {
			t.Errorf("%s:0 DRAM limit reads %s, want 0 untouched", zone, got)
		}
	}
	if s := controlStatus(t, dyn, e.name, "cpu"); s["backend"] != "rapl" || s["result"] != "applied" {
		t.Fatalf("cpu control status %v, want backend rapl, result applied", s)
	}

	if err := n.Step(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	for _, ev := range n.State().Events {
		if zone := path.Base(path.Dir(ev.File)); strings.Count(zone, ":") == 2 {
			t.Errorf("the agent wrote a limit of the sub-zone %s: %+v", zone, ev)
		}
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
// It skips if dvfs.EnergyFiles lists a package twice again (D5): the agent
// would then read about twice the true power and throttle to the floor.
//
// The loop runs on the node's simulated clock, one reading every 25 ms, and
// never on the wall clock: the host reader turns energy_uj deltas into watts
// over its own time.Now interval (dvfs readPowerWatts), and the cooldown
// compares time.Now with LastAction, so a test that slept until each
// simulated moment would let the agent read late by however long its
// reconcile took before the energy files. That lag varies with host load,
// so each reading would be off by the change in lag over its 25 ms
// interval; a loaded run averaged 552.6 W under a 572.1 W cap. A test
// cannot set that clock, so after one host reading, which shows the agent
// reads every package counter, the agent reads the same counters through
// its telemetry endpoint, as an OTLP hw.energy export stamped with
// simulated time, which its reader turns into watts over those stamps
// (otlpjson.PowerReader). The
// cooldown is kept in simulated time too: before each reconcile LastAction
// is set to the zero time when the cooldown has run out since the last
// action, and far ahead when it has not.
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
		t.Skip("EnergyFiles counts a package twice (D5 regressed)")
	}

	n.SetLoad(load)
	reader, dyn := newTestClients(t, e.nodeObject(p), nodeTwinWithCPUCap(e.name, capW))
	nc := e.controller(t)
	ctx := context.Background()
	start := n.State().Time

	// One reading on the host path: DVFS keeps a sample of every package
	// counter, and has no interval yet to act on.
	if err := reconcileOnce(ctx, reader, dyn, nc); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	for _, f := range files {
		if _, ok := nc.dvfs.Samples[f]; !ok {
			t.Fatalf("after a host reading DVFS has samples of %d counters, none of %s; want all of %v", len(nc.dvfs.Samples), f, files)
		}
	}

	// The counters as the agent's telemetry endpoint serves them. The
	// path is unique per run, because the HTTP reader keeps its energy
	// points per endpoint for the life of the process, and every run
	// starts at the same simulated time: a kept point of an earlier run on
	// a reused port would stand for the first readings of this one.
	energy := newPackageEnergy(t, files)
	var body atomic.Pointer[[]byte]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(*body.Load())
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TELEMETRY_CPU_SOURCE", "http")
	t.Setenv("TELEMETRY_CPU_HTTP_ENDPOINT", fmt.Sprintf("%s/a04/%d/{node}", srv.URL, a04Runs.Add(1)))
	t.Setenv("TELEMETRY_HTTP_TIMEOUT_SECONDS", "60")
	resource := otlpjson.Resource{Attributes: []otlpjson.KeyValue{otlpjson.String(semconv.AttrK8sNodeName, e.name)}}

	const (
		tick               = 25 * time.Millisecond
		settleFor, holdFor = 3 * time.Second, time.Second
	)
	farAhead := time.Now().Add(24 * time.Hour)
	var acted, holdStart time.Time
	var held []float64
	for at := start.Add(tick); ; at = at.Add(tick) {
		if err := n.AdvanceTo(at); err != nil {
			t.Fatal(err)
		}
		points := make([]otlpjson.NumberDataPoint, len(files))
		for i, j := range energy.joules(t) {
			points[i] = otlpjson.DoublePoint(start, at, j,
				otlpjson.String(semconv.AttrHwType, semconv.HwTypeCPU),
				otlpjson.String(semconv.AttrHwID, filepath.Base(filepath.Dir(files[i]))))
		}
		doc, err := json.Marshal(otlpjson.NewMetricsData(resource, otlpjson.Scope{Name: "a04"}, otlpjson.SumMetric(semconv.HwEnergy, points...)))
		if err != nil {
			t.Fatal(err)
		}
		body.Store(&doc)

		last := time.Time{}
		if !acted.IsZero() && at.Sub(acted) < nc.dvfs.Cooldown {
			last = farAhead
		}
		nc.dvfs.LastAction = last
		// The agent's whole reconcile, so the cap reaches DVFS the way it
		// does in a deployed agent.
		if err := reconcileOnce(ctx, reader, dyn, nc); err != nil {
			t.Fatalf("reconcileOnce: %v", err)
		}
		if !nc.dvfs.LastAction.Equal(last) {
			acted = at
		}

		elapsed := at.Sub(start)
		if holdStart.IsZero() {
			if elapsed < settleFor {
				continue
			}
			holdStart = at
		}
		if pct := nc.dvfs.ThrottlePct; pct != 20 && pct != 30 && pct != 40 {
			t.Fatalf("throttle %d%% after %v, want 20, 30 or 40 once settled (cap %.1f W, true power %.1f W)",
				pct, elapsed, capW, totalPackagePower(n))
		}
		held = append(held, totalPackagePower(n))
		if at.Sub(holdStart) >= holdFor {
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
	t.Logf("throttle %d%%, true package power averages %.3f W over the hold window (cap %.3f W)", nc.dvfs.ThrottlePct, mean, capW)

	// RAPL has no limit to write on this host, so the cap went to DVFS.
	if s := controlStatus(t, dyn, e.name, "cpu"); s["backend"] != "dvfs" {
		t.Fatalf("cpu control status %v, want backend dvfs", s)
	}
}

// a04Runs numbers the runs of A04 in this process, for its endpoint path.
var a04Runs atomic.Int64

// packageEnergy reads the package energy counters the agent's DVFS reads and
// keeps their running totals in joules across counter wraps, so an export of
// them never decreases.
type packageEnergy struct {
	files   []string
	rangeUJ []int64
	lastUJ  []int64
	totalJ  []float64
}

func newPackageEnergy(t *testing.T, files []string) *packageEnergy {
	t.Helper()
	pe := &packageEnergy{files: files, rangeUJ: make([]int64, len(files)), lastUJ: make([]int64, len(files)), totalJ: make([]float64, len(files))}
	for i, f := range files {
		pe.rangeUJ[i] = readInt64File(t, filepath.Join(filepath.Dir(f), "max_energy_range_uj"))
		pe.lastUJ[i] = readInt64File(t, f)
		pe.totalJ[i] = float64(pe.lastUJ[i]) / 1e6
	}
	return pe
}

// joules reads every counter and returns the running totals.
func (pe *packageEnergy) joules(t *testing.T) []float64 {
	t.Helper()
	for i, f := range pe.files {
		uj := readInt64File(t, f)
		d := uj - pe.lastUJ[i]
		if d < 0 {
			d += pe.rangeUJ[i]
		}
		pe.lastUJ[i] = uj
		pe.totalJ[i] += float64(d) / 1e6
	}
	return append([]float64(nil), pe.totalJ...)
}

func readInt64File(t *testing.T, path string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(readTrimmed(t, path), 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

func totalPackagePower(n *hwemu.Node) float64 {
	total := 0.0
	for _, pk := range n.State().Packages {
		total += pk.PowerW
	}
	return total
}

// A04b: a throttle percentage reaches exactly that share of the policies.
// Regression for D6: dvfs.CPUFreqList listed every policy twice, through each
// cpuN/cpufreq link and as policyN. On the Intel host, one policy per CPU, a
// 15 % throttle then reached 14 policies where 15 were intended; with
// policies shared per core, a 30 % throttle reached none, because each
// sibling's later entry wrote the maximum back.
func TestHwemuA04bThrottleReachesTheIntendedPolicies(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mutate        func(*hwemu.Profile)
		pct           int
		wantThrottled int
	}{
		{"per-cpu", nil, 15, 15},
		{"per-core", withPerCorePolicies, 30, 15},
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
					"want %d: one entry per policy (D6)",
					tc.pct, throttled, policies, intended, len(ctl.Cpus), policies, tc.wantThrottled)
			}
			t.Logf("%d%% throttle: %d of %d policies throttled, %d intended; CPUFreqList returned %d entries",
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

// A05b: on the MI300X node, the agent finds the vendor only through
// rocm-smi --showproductname, because it ignores the NFD accelerator-class
// label pci-1200_1002 (D8). The model comes from the labeller's
// amd.com/gpu.product-name, not from a VBIOS segment, and a 500 W cap runs
// no rocm-smi command but the probe.
func TestHwemuA05bAMDVendorComesFromRocmSMI(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "amd-instinct-mi300x-8gpu", nil)
	e := useEmulatedNode(t, tree, nil)
	node := e.nodeObject(p)
	if node.Labels[amdAcceleratorNFD] != "true" {
		t.Fatalf("the node has no %s label, so this test proves nothing about it: %v", amdAcceleratorNFD, node.Labels)
	}
	if hasNFDGPUVendor(node.Labels, "1002") {
		t.Fatalf("the agent now reads an AMD vendor from the node labels; it ignored %s before (D8). Update this test with the fix", amdAcceleratorNFD)
	}

	hw := discoverHardware(context.Background(), node)
	if _, ok := e.runner.ran(rocmProductProbe); !ok || hw.GPUVendor != "amd" {
		t.Fatalf("gpu vendor %q; %s did not decide it; commands: %v (D8)", hw.GPUVendor, rocmProductProbe, e.runner.commands())
	}
	if product := node.Labels[amdLabellerProduct]; hw.GPURawModel != product || strings.Contains(p.GPUs.VBIOSVersion.V, hw.GPURawModel) {
		t.Fatalf("gpu rawModel %q, want the labeller's %s %q, never a segment of the VBIOS version %q (D4)",
			hw.GPURawModel, amdLabellerProduct, product, p.GPUs.VBIOSVersion.V)
	}

	e.runner.reset()
	dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithGPUCap(e.name, float64Ptr(500), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-amd" || s["result"] != "applied" {
		t.Fatalf("gpu control status %v, want backend host-amd, result applied", s)
	}
	if calls := e.amdToolCalls(); len(calls) > 0 {
		t.Fatalf("the agent ran %v to cap; it caps through amdgpu sysfs", calls)
	}
}

// A05d: on the MI300X node with a 600 W default below the 750 W maximum,
// NodeHardware publishes the driver's range, 0 to 750 W, and a 700 W cap
// above the default is host-amd applied: the emulated driver takes it on
// every power1_cap. No tool runs, so nothing can prompt (D3).
func TestHwemuA05dCapAboveTheDefaultLimitIsWritten(t *testing.T) {
	n, tree, p := hwemutest.NewNode(t, "amd-instinct-mi300x-8gpu", withDefaultLimit(600))
	e := useEmulatedNode(t, tree, nil)
	checkWritesThroughNode(t, n, tree)
	hwmons := amdgpuHwmonDirs(t, e)
	for i, dir := range hwmons {
		if got := readTrimmed(t, filepath.Join(dir, "power1_cap")); got != "600000000" {
			t.Fatalf("device %d starts at power1_cap %s, want the 600 W default", i, got)
		}
		if got := readTrimmed(t, filepath.Join(dir, "power1_cap_max")); got != "750000000" {
			t.Fatalf("device %d power1_cap_max %s, want 750 W above the default, or this test proves nothing", i, got)
		}
	}
	node := e.nodeObject(p)
	if hw := discoverHardware(context.Background(), node); !hw.GPUCapKnown || hw.GPUCapMinWatts != 0 || hw.GPUCapMaxWatts != 750 {
		t.Fatalf("gpu capRangePerGpu known=%v %v to %v W, want 0 to 750 W, power1_cap_min to power1_cap_max", hw.GPUCapKnown, hw.GPUCapMinWatts, hw.GPUCapMaxWatts)
	}

	dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithGPUCap(e.name, float64Ptr(700), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-amd" || s["result"] != "applied" || s["message"] != "applied gpu cap 700.0W on 8 devices" {
		t.Fatalf("gpu control status %v, want a 700 W cap applied above the 600 W default", s)
	}
	for i, dir := range hwmons {
		if got := readTrimmed(t, filepath.Join(dir, "power1_cap")); got != "700000000" {
			t.Errorf("device %d power1_cap = %s, want 700000000", i, got)
		}
	}
	if calls := e.amdToolCalls(); len(calls) > 0 {
		t.Errorf("the agent ran %v", calls)
	}
}

// A06: on the MI300X node with the current rocm-smi, which has no
// --showpowercap, NodeHardware lists the 8 GPUs from the amdgpu hwmon
// devices with their 0 to 750 W range, the 750 W cap in force and control,
// and takes the raw model from the labeller's amd.com/gpu.product-name,
// AMD_Instinct_MI300X_OAM, which the catalog lists as an alias of
// AMD_INSTINCT_MI300X. At full load a 500 W cap is host-amd applied, sets
// every power1_cap to 500000000, and three seconds later each GPU draws at
// most 505 W; no rocm-smi cap command runs (D1).
func TestHwemuA06AMDCapsThroughSysfs(t *testing.T) {
	n, tree, p := hwemutest.NewNode(t, "amd-instinct-mi300x-8gpu", nil)
	n.SetLoad(hwemu.Load{GPUUtil: []float64{1}})
	if err := n.Step(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	e := useEmulatedNode(t, tree, nil)
	hwmons := amdgpuHwmonDirs(t, e)
	if w := float64(readFileInt(t, filepath.Join(hwmons[0], "power1_input"))) / 1e6; w <= 505 {
		t.Fatalf("device 0 draws %.1f W uncapped at full load; the test needs it above 505 W to mean anything", w)
	}
	node := e.nodeObject(p)
	if node.Labels[amdLabellerProduct] == "" {
		t.Fatalf("the node has no %s label, so this test proves nothing about it", amdLabellerProduct)
	}
	status := publishedStatusMap(t, node.Name, discoverHardware(context.Background(), node))
	gpu, _ := status["gpu"].(map[string]any)
	product := node.Labels[amdLabellerProduct]
	capRange, _ := gpu["capRangePerGpu"].(map[string]any)
	if gpu["count"] != float64(8) || gpu["controlAvailable"] != true || gpu["currentCapWatts"] != float64(750) ||
		capRange["minWatts"] != float64(0) || capRange["maxWatts"] != float64(750) || gpu["rawModel"] != product ||
		gpu["model"] != "AMD_INSTINCT_MI300X" || containsAny(gpu["warnings"], "gpu model not recognized") {
		t.Fatalf("NodeHardware gpu %v; want count 8, capRangePerGpu 0 to 750 W, currentCapWatts 750 and control from amdgpu sysfs (D1), "+
			"and rawModel the %s value %q, which the catalog matches as AMD_INSTINCT_MI300X", gpu, amdLabellerProduct, product)
	}

	e.runner.reset()
	dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithGPUCap(e.name, float64Ptr(500), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-amd" || s["result"] != "applied" {
		t.Fatalf("gpu control status %v, want backend host-amd, result applied (D1)", s)
	}
	if calls := e.amdToolCalls(); len(calls) > 0 {
		t.Errorf("the agent ran %v to cap", calls)
	}
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
// emulated package power over the agent's own interval. The agent's total
// equals the sum of both emulated packages: each package is counted once,
// although the tree reaches it through class/powercap and
// devices/virtual/powercap (D5).
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

	var firsts []int64
	for _, pkg := range n.State().Packages {
		firsts = append(firsts, pkg.EnergyUJ)
	}
	first := firsts[0]
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
	total, ok, err := nc.dvfs.ReadPowerWatts()
	if err != nil || !ok {
		t.Fatalf("second ReadPowerWatts: %v (ok %v)", err, ok)
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

	var wantTotal float64
	for i, pkg := range n.State().Packages {
		wantTotal += float64(pkg.EnergyUJ-firsts[i]) / 1e6 / interval
	}
	if math.Abs(total-wantTotal) > 0.01*wantTotal {
		t.Fatalf("agent total %.3f W, want the emulated %.3f W of both packages within 1 %% (each counted once, D5)", total, wantTotal)
	}
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

// checkWritesThroughNode routes the agent's attribute writes
// (writeSysfsAttr) and its RAPL limit writes (writeRAPLLimit) under tree
// through n's CheckWrite before they land, so a value the driver refuses
// fails with its errno at the write, as on the real host, instead of
// landing in the tree until the emulator's next step reverts it. A write
// the driver takes is in effect at once and then lands.
func checkWritesThroughNode(t *testing.T, n *hwemu.Node, tree *hwemu.Tree) {
	t.Helper()
	check := func(path, value string) error {
		if rel, err := filepath.Rel(tree.Root, path); err == nil && !strings.HasPrefix(rel, "..") {
			return n.CheckWrite(filepath.ToSlash(rel), value)
		}
		return nil
	}
	oldAttr, oldRAPL := writeSysfsAttr, writeRAPLLimit
	writeSysfsAttr = func(path, value string) error {
		if err := check(path, value); err != nil {
			return err
		}
		return oldAttr(path, value)
	}
	writeRAPLLimit = func(path string, data []byte) error {
		if err := check(path, string(data)); err != nil {
			return err
		}
		return oldRAPL(path, data)
	}
	t.Cleanup(func() { writeSysfsAttr, writeRAPLLimit = oldAttr, oldRAPL })
}

// A10: on the Intel host at full load with both package PL1 limits locked by
// the BIOS, every RAPL write fails with the kernel's ENODATA (the writes go
// through the node's CheckWrite), reconcileOnce returns the error, no DVFS
// fallback touches scaling_max_freq, and NodeTwin gets no CPU control status:
// the cap is never enforced and nothing says so (D10).
//
// The DVFS controller needs two energy readings before it acts, so one
// reconcile would pass even with a fallback in place. The test reconciles
// three times, a second of node time apart, with no cooldown and a trip count
// of one: a fallback would throttle from the second reconcile on.
func TestHwemuA10LockedRAPLLeavesTheCapUnenforced(t *testing.T) {
	n, tree, p := hwemutest.NewNode(t, "intel-xeon-2s-rapl", withLockedPL1)
	checkWritesThroughNode(t, n, tree)
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
		if err == nil || !strings.Contains(err.Error(), "write rapl cap") || !strings.Contains(err.Error(), syscall.ENODATA.Error()) {
			t.Fatalf("reconcile %d returned %v; today a locked PL1 fails every write with the kernel's ENODATA and the error ends the reconcile (D10)", i+1, err)
		}
	}
	if err := n.Step(0); err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, ev := range n.State().Events {
		if ev.Kind == hwemu.EventRejected && ev.Errno == hwemu.ErrnoENODATA {
			refused++
		}
	}
	if refused != 2 {
		t.Errorf("%d writes refused with ENODATA at the last reconcile, want one per locked package limit", refused)
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
// renamed time-sliced replicas, the GPUs are GPUs (pkg/api.GPUResourceKind):
// a 250 W cap runs nvidia-smi -i <k> -pl 250 on each of the 8 physical GPUs,
// whose limit holds every MIG device and replica of that GPU, and the
// control status is host-nvidia applied.
func TestHwemuA12SharedGPUExposureIsCapped(t *testing.T) {
	for _, mode := range []string{"mig-mixed", "timeslice-rename"} {
		t.Run(mode, func(t *testing.T) {
			_, tree, p := hwemutest.NewNode(t, "nvidia-h100-nvl-8gpu", withExposure(mode))
			e := useEmulatedNode(t, tree, nil)
			dyn, err := reconcileEmulated(t, e.controller(t), e.nodeObject(p), nodeTwinWithGPUCap(e.name, float64Ptr(250), nil))
			if err != nil {
				t.Fatalf("reconcileOnce: %v", err)
			}
			if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-nvidia" || s["result"] != "applied" {
				t.Fatalf("gpu control status %v, want backend host-nvidia, result applied (D18)", s)
			}
			for k := 0; k < p.GPUs.Count.V; k++ {
				cmd := "nvidia-smi -i " + strconv.Itoa(k) + " -pl 250"
				if _, ok := e.runner.ran(cmd); !ok {
					t.Errorf("%q did not run successfully; commands: %v", cmd, e.runner.commands())
				}
				if got := readTrimmed(t, nvmlFile(e, k, "power_limit_mw")); got != "250000" {
					t.Errorf("gpu %d power_limit_mw = %s, want 250000", k, got)
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

// A19: on the Radeon PRO W7900 node, whose driver pins every board's cap at
// 241 W (minimum, maximum, default and current), the agent takes the AMD
// vendor from the NFD display-class label pci-0300_1002 and runs no probe.
// NodeHardware lists the 4 GPUs from the amdgpu hwmon devices with the
// pinned range, 241 to 241 W, and control, and takes the raw model from the
// labeller's amd.com/gpu.product-name, the catalog's AMD_RADEON_PRO_W7900. A
// 200 W cap is blocked below the minimum; a 60 % cap resolves to the 241 W
// minimum, already in force, so it is applied with nothing written. The
// writes go through the node's CheckWrite, which refuses any other value.
func TestHwemuA19W7900PinnedCapIsNeverWritten(t *testing.T) {
	n, tree, p := hwemutest.NewNode(t, "amd-radeon-pro-w7900-4gpu", nil)
	e := useEmulatedNode(t, tree, nil)
	checkWritesThroughNode(t, n, tree)
	writes := watchWrites(t, tree.SysRoot)
	node := e.nodeObject(p)
	if node.Labels[amdDisplayNFD] != "true" || node.Labels[amdAcceleratorNFD] != "" {
		t.Fatalf("labels %v: want %s and not %s, or this test proves nothing about the display class", node.Labels, amdDisplayNFD, amdAcceleratorNFD)
	}
	hw := discoverHardware(context.Background(), node)
	for _, probe := range []string{nvidiaListProbe, rocmProductProbe} {
		if ran, _ := e.runner.ran(probe); ran {
			t.Errorf("%s ran, so the vendor did not come from %s; commands: %v", probe, amdDisplayNFD, e.runner.commands())
		}
	}
	status := publishedStatusMap(t, node.Name, hw)
	gpu, _ := status["gpu"].(map[string]any)
	capRange, _ := gpu["capRangePerGpu"].(map[string]any)
	product := node.Labels[amdLabellerProduct]
	if gpu["vendor"] != "amd" || gpu["count"] != float64(4) || gpu["controlAvailable"] != true || gpu["currentCapWatts"] != float64(241) ||
		capRange["minWatts"] != float64(241) || capRange["maxWatts"] != float64(241) ||
		gpu["rawModel"] != product || gpu["model"] != "AMD_RADEON_PRO_W7900" || containsAny(gpu["warnings"], "gpu model not recognized") {
		t.Fatalf("NodeHardware gpu %v; want 4 GPUs pinned at 241 to 241 W with control, from amdgpu sysfs (D1), "+
			"and rawModel the %s value %q, the catalog's AMD_RADEON_PRO_W7900", gpu, amdLabellerProduct, product)
	}

	nc := e.controller(t)
	dyn, err := reconcileEmulated(t, nc, node, nodeTwinWithGPUCap(e.name, float64Ptr(200), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	s := controlStatus(t, dyn, e.name, "gpu")
	if msg, _ := s["message"].(string); s["result"] != "blocked" || !strings.Contains(msg, "requested cap 200.0W below min 241.0W") {
		t.Fatalf("gpu control status %v, want a 200 W cap blocked below the 241 W minimum", s)
	}
	dyn, err = reconcileEmulated(t, nc, node, nodeTwinWithGPUCap(e.name, nil, float64Ptr(60)))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-amd" || s["result"] != "applied" || s["message"] != "applied gpu cap 241.0W on 4 devices" {
		t.Fatalf("gpu control status %v, want a 60 %% cap applied at the pinned 241 W", s)
	}
	if w := writes.take(); len(w) != 0 {
		t.Errorf("wrote %v on a pinned cap", w)
	}
	if calls := e.amdToolCalls(); len(calls) > 0 {
		t.Errorf("the agent ran %v", calls)
	}
	for i, dir := range amdgpuHwmonDirs(t, e) {
		if got := readTrimmed(t, filepath.Join(dir, "power1_cap")); got != "241000000" {
			t.Errorf("device %d power1_cap = %s, want it pinned at 241000000", i, got)
		}
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

// Governor rows. Each renders a generic profile, points the agent at it
// with GOVERNOR_WRITES true and drives its real discovery and control: the
// emulated cpufreq and amdgpu attributes, the fake nvidia-smi, and the
// emulator's step, which takes a write or reverts it as the driver would.

// govEmu is an emulated node for the governor rows.
type govEmu struct {
	n *hwemu.Node
	e *emulatedNode
	p *hwemu.Profile
}

func emulatedGovernorNode(t *testing.T, profile string, mutate func(*hwemu.Profile)) *govEmu {
	t.Helper()
	n, tree, p := hwemutest.NewNode(t, profile, mutate)
	return &govEmu{n: n, e: useEmulatedNode(t, tree, nil), p: p}
}

// rig is the governor rig of the node, with GOVERNOR_WRITES true.
func (g *govEmu) rig(t *testing.T, cpu *v1alpha1.CPUGovernorIntent, gpu *v1alpha1.GPUGovernorIntent) *govRig {
	t.Helper()
	return newGovRig(t, g.e.nodeObject(g.p), governorOn, cpu, gpu)
}

// step runs one emulator step, which takes or reverts what the agent wrote.
func (g *govEmu) step(t *testing.T) {
	t.Helper()
	if err := g.n.Step(time.Second); err != nil {
		t.Fatal(err)
	}
}

// stepInBackground steps the node every few milliseconds until stop, as the
// emulator of a deployed simulator ticks on its own: the overdrive sequence
// waits for the table it prints.
func (g *govEmu) stepInBackground(t *testing.T) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				finished <- nil
				return
			case <-tick.C:
				if err := g.n.Step(5 * time.Millisecond); err != nil {
					finished <- err
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
}

// discoveredGovernors is the governor discovery of the node.
func (g *govEmu) discoveredGovernors() governorDiscovery {
	return discoverHardware(context.Background(), g.e.nodeObject(g.p)).Governors
}

// allPolicyFiles is one attribute of every cpufreq policy.
func allPolicyFiles(t *testing.T, g *govEmu, name string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(g.e.tree.SysRoot, "devices", "system", "cpu", "cpufreq", "policy*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no cpufreq policies: %v", err)
	}
	var out []string
	for _, d := range dirs {
		out = append(out, readTrimmed(t, filepath.Join(d, name)))
	}
	return out
}

// amdLevelFiles is the performance level file of every amdgpu device the
// agent discovered, by bus id.
func amdLevelFiles(t *testing.T, g *govEmu) map[string]string {
	t.Helper()
	d := g.discoveredGovernors().GPU
	if d == nil || len(d.AMD) == 0 {
		t.Fatal("no amdgpu device discovered")
	}
	out := map[string]string{}
	for _, f := range d.AMD {
		out[f.BusID] = filepath.Join(g.e.tree.SysRoot, "bus", "pci", "devices", f.BusID, "power_dpm_force_performance_level")
	}
	return out
}

func presetNames(ps []v1alpha1.GovernorPresetSupport) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Preset)
	}
	return out
}

// A20: on the EPYC 9534 energy-only host the CPU runs schedutil, and the
// efficiency preset is realised as conservative.
func TestHwemuA20EpycEfficiencyIsConservative(t *testing.T) {
	g := emulatedGovernorNode(t, "amd-epyc-9534-2s-energy-only", nil)
	c := g.discoveredGovernors().CPU
	if c == nil {
		t.Fatal("no CPU governor discovered")
	}
	if c.Inventory.Current != "schedutil" {
		t.Errorf("current %q, want schedutil", c.Inventory.Current)
	}
	var native string
	for _, p := range c.Inventory.Presets {
		if p.Preset == joulie.CPUGovernorPresetEfficiency {
			native = p.Native
		}
	}
	if native != "conservative" {
		t.Errorf("efficiency maps to %q, want conservative", native)
	}
}

// A21: the W7900 node publishes mechanism amdgpu-sysfs, current auto and
// the presets auto, sustained, low and peak (no overdrive ceiling).
func TestHwemuA21W7900GovernorDiscovery(t *testing.T) {
	g := emulatedGovernorNode(t, "amd-radeon-pro-w7900-4gpu", nil)
	d := g.discoveredGovernors().GPU
	if d == nil {
		t.Fatal("no GPU governor discovered")
	}
	inv := d.Inventory
	want := []string{joulie.GPUGovernorPresetAuto, joulie.GPUGovernorPresetSustained, joulie.GPUGovernorPresetLow, joulie.GPUGovernorPresetPeak}
	if inv.Mechanism != joulie.GovernorBackendAMDGPUSysfs || inv.Current != "auto" || !slices.Equal(presetNames(inv.Presets), want) {
		t.Errorf("mechanism %q current %q presets %v, want amdgpu-sysfs, auto, %v", inv.Mechanism, inv.Current, presetNames(inv.Presets), want)
	}
}

// A22: the MI300X node accepts auto and efficient only.
func TestHwemuA22MI300XGovernorPresets(t *testing.T) {
	g := emulatedGovernorNode(t, "amd-instinct-mi300x-8gpu", nil)
	d := g.discoveredGovernors().GPU
	if d == nil {
		t.Fatal("no GPU governor discovered")
	}
	if got, want := presetNames(d.Inventory.Presets), []string{joulie.GPUGovernorPresetAuto, joulie.GPUGovernorPresetEfficient}; !slices.Equal(got, want) {
		t.Errorf("presets %v, want %v", got, want)
	}
}

// A23: on the H100 SXM node with nvidia-smi in reach, the mechanism is
// nvidia-smi and the current state unknown: a lock has no getter.
func TestHwemuA23H100GovernorDiscovery(t *testing.T) {
	g := emulatedGovernorNode(t, "nvidia-h100-sxm-4gpu", nil)
	d := g.discoveredGovernors().GPU
	if d == nil {
		t.Fatal("no GPU governor discovered")
	}
	if d.Inventory.Mechanism != joulie.GovernorBackendNvidiaSMI || d.Inventory.Current != joulie.GovernorStateUnknown {
		t.Errorf("mechanism %q current %q, want nvidia-smi, unknown", d.Inventory.Mechanism, d.Inventory.Current)
	}
}

// A24: on the acpi-cpufreq EPYC host, efficiency writes conservative to
// every policy and leaves boost alone; without the field every policy goes
// back to schedutil.
func TestHwemuA24EpycEfficiencyAndRestore(t *testing.T) {
	g := emulatedGovernorNode(t, "amd-epyc-2s-energy-only", nil)
	writes := watchWrites(t, g.e.tree.SysRoot)
	r := g.rig(t, efficiency, nil)
	r.reconcile()
	g.step(t)
	r.reconcile()
	if got := allPolicyFiles(t, g, "scaling_governor"); !allEqual(got, "conservative") {
		t.Fatalf("scaling_governor %v, want conservative on every policy", got)
	}
	for _, w := range writes.take() {
		if strings.Contains(w, "boost") {
			t.Errorf("efficiency without a boost field wrote %s", w)
		}
	}
	if s := r.status(componentCPUGovernor); s == nil || s.Result != joulie.GovernorResultApplied || s.Applied != "conservative" {
		t.Fatalf("cpuGovernor status %+v after a step, want applied conservative", s)
	}
	r.setIntents(nil, nil)
	r.reconcile()
	g.step(t)
	if got := allPolicyFiles(t, g, "scaling_governor"); !allEqual(got, "schedutil") {
		t.Fatalf("scaling_governor %v without the field, want schedutil", got)
	}
	if s := r.status(componentCPUGovernor); s == nil || s.Result != joulie.GovernorResultRestored {
		t.Errorf("cpuGovernor status %+v, want restored", s)
	}
}

// A25: on the Intel HWP host, balanced is powersave with the preference
// balance_performance on every policy; performance writes no preference,
// which the kernel rejects under performance.
func TestHwemuA25IntelHWPBalancedAndPerformance(t *testing.T) {
	g := emulatedGovernorNode(t, "intel-xeon-2s-rapl", nil)
	writes := watchWrites(t, g.e.tree.SysRoot)
	r := g.rig(t, &v1alpha1.CPUGovernorIntent{Preset: joulie.CPUGovernorPresetBalanced}, nil)
	r.reconcile()
	g.step(t)
	if got := allPolicyFiles(t, g, "scaling_governor"); !allEqual(got, "powersave") {
		t.Errorf("scaling_governor %v, want powersave", got)
	}
	if got := allPolicyFiles(t, g, "energy_performance_preference"); !allEqual(got, "balance_performance") {
		t.Errorf("energy_performance_preference %v, want balance_performance", got)
	}
	eppWrites := 0
	for _, w := range writes.take() {
		if strings.HasSuffix(w, "/energy_performance_preference=balance_performance") {
			eppWrites++
		}
	}
	if n := len(allPolicyFiles(t, g, "scaling_governor")); eppWrites != n {
		t.Errorf("%d preference writes, want one per policy (%d)", eppWrites, n)
	}

	r.setIntents(&v1alpha1.CPUGovernorIntent{Preset: joulie.CPUGovernorPresetPerformance}, nil)
	r.reconcile()
	g.step(t)
	if got := allPolicyFiles(t, g, "scaling_governor"); !allEqual(got, "performance") {
		t.Errorf("scaling_governor %v, want performance", got)
	}
	for _, w := range writes.take() {
		if strings.Contains(w, "energy_performance_preference") {
			t.Errorf("performance wrote %s", w)
		}
	}
}

// A26: on the W7900 node, sustained writes high to all four level files and
// is applied; efficient is unsupported and writes nothing.
func TestHwemuA26W7900Sustained(t *testing.T) {
	g := emulatedGovernorNode(t, "amd-radeon-pro-w7900-4gpu", nil)
	levels := amdLevelFiles(t, g)
	writes := watchWrites(t, g.e.tree.SysRoot)
	r := g.rig(t, nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetSustained})
	r.reconcile()
	g.step(t)
	r.reconcile()
	if len(levels) != 4 {
		t.Fatalf("%d devices, want 4", len(levels))
	}
	for bus, f := range levels {
		if got := readTrimmed(t, f); got != "high" {
			t.Errorf("%s level %q, want high", bus, got)
		}
	}
	if s := r.status(componentGPUGovernor); s == nil || s.Result != joulie.GovernorResultApplied || s.Applied != "high" {
		t.Fatalf("gpuGovernor status %+v, want applied high", s)
	}
	writes.take()
	r.setIntents(nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetEfficient})
	r.reconcile()
	if s := r.status(componentGPUGovernor); s == nil || s.Result != joulie.GovernorResultUnsupported {
		t.Errorf("efficient: status %+v, want unsupported", s)
	}
	if w := writes.take(); len(w) != 0 {
		t.Errorf("an unsupported preset wrote %v", w)
	}
}

// A27: on the MI300X node, sustained is unsupported and writes nothing;
// efficient at the default 80 % sets manual, then "s 1" at 80 % of the
// sclk maximum, then "c" once the edit reads back, and the ceiling reads
// back in OD_SCLK.
func TestHwemuA27MI300XEfficientOverdrive(t *testing.T) {
	g := emulatedGovernorNode(t, "amd-instinct-mi300x-8gpu", nil)
	levels := amdLevelFiles(t, g)
	writes := watchWrites(t, g.e.tree.SysRoot)
	r := g.rig(t, nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetSustained})
	r.reconcile()
	if s := r.status(componentGPUGovernor); s == nil || s.Result != joulie.GovernorResultUnsupported {
		t.Fatalf("sustained: status %+v, want unsupported", s)
	}
	if w := writes.take(); len(w) != 0 {
		t.Fatalf("sustained wrote %v", w)
	}

	sclkMax := int(g.p.GPUs.DPM.Tables.Sclk.MaxMHz.V)
	ceiling := sclkMax * 80 / 100
	r.setIntents(nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetEfficient})
	stop := g.stepInBackground(t)
	r.reconcile()
	stop()
	s := r.status(componentGPUGovernor)
	if want := fmt.Sprintf("manual sclk_max=%d", ceiling); s == nil || s.Result != joulie.GovernorResultApplied || s.Applied != want {
		t.Fatalf("efficient: status %+v, want applied %q", s, want)
	}
	for bus, f := range levels {
		if got := readTrimmed(t, f); got != "manual" {
			t.Errorf("%s level %q, want manual", bus, got)
		}
		od := readTrimmed(t, filepath.Join(filepath.Dir(f), "pp_od_clk_voltage"))
		if got, ok := odSclkMax(od); !ok || got != ceiling {
			t.Errorf("%s OD_SCLK maximum %d (%v), want %d", bus, got, ok, ceiling)
		}
	}
	// Per device: manual, then the edit, then the commit, in that order.
	order := map[string][]string{}
	for _, w := range writes.take() {
		path, value, _ := strings.Cut(w, "=")
		dir := filepath.Dir(path)
		order[dir] = append(order[dir], filepath.Base(path)+"="+value)
	}
	want := []string{"power_dpm_force_performance_level=manual", fmt.Sprintf("pp_od_clk_voltage=s 1 %d", ceiling), "pp_od_clk_voltage=c"}
	for dir, got := range order {
		if !slices.Equal(got, want) {
			t.Errorf("%s writes %v, want %v", dir, got, want)
		}
	}
	if len(order) != len(levels) {
		t.Errorf("writes reached %d devices, want %d", len(order), len(levels))
	}
}

// A28: on the H100 SXM node, sustained is equivalent and runs no -lmc;
// efficient runs -lgc and is applied-unverified until busy samples within
// the lock make it applied.
func TestHwemuA28H100EfficientIsVerifiedUnderLoad(t *testing.T) {
	g := emulatedGovernorNode(t, "nvidia-h100-sxm-4gpu", nil)
	r := g.rig(t, nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetSustained})
	r.reconcile()
	if s := r.status(componentGPUGovernor); s == nil || s.Result != joulie.GovernorResultEquivalent {
		t.Fatalf("sustained: status %+v, want equivalent", s)
	}
	if c := lockCommands(g.e, "-lmc", "-lgc"); len(c) != 0 {
		t.Fatalf("sustained ran %v on Hopper", c)
	}

	r.setIntents(nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetEfficient})
	r.reconcile()
	if c := lockCommands(g.e, "-lgc"); len(c) != g.p.GPUs.Count.V {
		t.Fatalf("efficient ran %v, want one -lgc per GPU", c)
	}
	if s := r.status(componentGPUGovernor); s == nil || s.Result != joulie.GovernorResultAppliedUnverified {
		t.Fatalf("efficient: status %+v, want applied-unverified (a lock has no getter)", s)
	}
	busy := make([]float64, g.p.GPUs.Count.V)
	for i := range busy {
		busy[i] = 1
	}
	g.n.SetLoad(hwemu.Load{GPUUtil: busy})
	for i := 0; i < nvidiaVerifySamples; i++ {
		g.step(t)
		r.reconcile()
	}
	if s := r.status(componentGPUGovernor); s == nil || s.Result != joulie.GovernorResultApplied {
		t.Fatalf("efficient after %d busy samples: status %+v, want applied", nvidiaVerifySamples, s)
	}
}

// A29: on the L40S node, sustained runs -lmc <max>,<max> per bus id; with
// persistence mode Disabled, the profile's own, the lock is issued again at
// every reconcile, and with it Enabled it is not.
func TestHwemuA29L40SSustainedReissuedWithoutPersistence(t *testing.T) {
	g := emulatedGovernorNode(t, "nvidia-l40s-4gpu", nil)
	d := g.discoveredGovernors().GPU
	if d == nil || len(d.NVIDIA) == 0 {
		t.Fatal("no NVIDIA GPU discovered")
	}
	memMax := slices.Max(d.NVIDIA[0].Facts.SupportedMemMHz)
	r := g.rig(t, nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetSustained})
	want := map[string]bool{}
	for _, n := range d.NVIDIA {
		want[fmt.Sprintf("nvidia-smi -i %s -lmc %d,%d", n.BusID, memMax, memMax)] = true
	}
	for i := 0; i < 2; i++ {
		r.reconcile()
		got := lockCommands(g.e, "-lmc")
		if len(got) != len(want) {
			t.Fatalf("reconcile %d ran %v, want %d -lmc commands", i, got, len(want))
		}
		for _, c := range got {
			if !want[c] {
				t.Errorf("reconcile %d ran %q, want one of %v", i, c, want)
			}
		}
	}
	if _, err := g.e.runner.Run(context.Background(), "nvidia-smi", "-pm", "1"); err != nil {
		t.Fatal(err)
	}
	g.e.runner.reset()
	r.reconcile()
	if got := lockCommands(g.e, "-lmc"); len(got) != 0 {
		t.Errorf("with persistence mode Enabled the lock was issued again: %v", got)
	}
}

// checkWritesThroughNode routes the agent's writes through the node's
// CheckWrite (the function of that name).
func (g *govEmu) checkWritesThroughNode(t *testing.T) {
	t.Helper()
	checkWritesThroughNode(t, g.n, g.e.tree)
}

// A30: on a W7900 whose driver refuses high, the first write of the level
// fails with the driver's EINVAL, which the agent's errno classification
// turns into unsupported; no device reads high, the emulator records the
// refusal, and the next reconciles write nothing.
func TestHwemuA30W7900RefusedLevelIsUnsupported(t *testing.T) {
	g := emulatedGovernorNode(t, "amd-radeon-pro-w7900-4gpu", func(p *hwemu.Profile) {
		var accept []string
		for _, l := range p.GPUs.DPM.Levels.Accept.V {
			if l != "high" {
				accept = append(accept, l)
			}
		}
		p.GPUs.DPM.Levels.Accept = hwemu.Fact[[]string]{V: accept, Src: variantSrc}
	})
	g.checkWritesThroughNode(t)
	writes := watchWrites(t, g.e.tree.SysRoot)
	r := g.rig(t, nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetSustained})
	r.reconcile()
	s := r.status(componentGPUGovernor)
	if s == nil || s.Result != joulie.GovernorResultUnsupported || !strings.Contains(s.Message, "EINVAL") {
		t.Fatalf("status %+v right after the write, want unsupported with the driver's EINVAL", s)
	}
	for bus, f := range amdLevelFiles(t, g) {
		if got := readTrimmed(t, f); got == "high" {
			t.Errorf("%s reads high, a level the driver refused", bus)
		}
	}
	if w := writes.take(); len(w) != 1 || !strings.HasSuffix(w[0], "=high") {
		t.Errorf("writes %v, want the one refused write of high", w)
	}
	g.step(t)
	var refused []hwemu.Event
	for _, e := range g.n.State().Events {
		if e.Kind == hwemu.EventRejected {
			refused = append(refused, e)
		}
	}
	if len(refused) != 1 || refused[0].Errno != hwemu.ErrnoEINVAL || refused[0].Written != "high" {
		t.Errorf("rejected events %+v, want the one refused write of high with EINVAL", refused)
	}
	r.reconcile()
	r.reconcile()
	if w := writes.take(); len(w) != 0 {
		t.Errorf("a level the driver refused was written again at once: %v", w)
	}
}

// A31: with the DRM card order, which the tools number GPUs by, reversed
// against the bus order, each GPU keeps its own baseline: the operator's
// low on one bus comes back there, auto on the others.
func TestHwemuA31AMDGPUsKeyedByBusID(t *testing.T) {
	g := emulatedGovernorNode(t, "amd-radeon-pro-w7900-4gpu", nil)
	drm := filepath.Join(g.e.tree.SysRoot, "class", "drm")
	entries, err := os.ReadDir(drm)
	if err != nil {
		t.Fatal(err)
	}
	var cards []string
	targets := map[string]string{}
	for _, en := range entries {
		if n, ok := strings.CutPrefix(en.Name(), "card"); ok && !strings.Contains(n, "-") {
			if dst, err := os.Readlink(filepath.Join(drm, en.Name())); err == nil {
				cards = append(cards, en.Name())
				targets[en.Name()] = dst
			}
		}
	}
	sort.Slice(cards, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(cards[i], "card"))
		b, _ := strconv.Atoi(strings.TrimPrefix(cards[j], "card"))
		return a < b
	})
	if len(cards) < 2 {
		t.Fatalf("cards %v, want several", cards)
	}
	for i, c := range cards {
		link := filepath.Join(drm, c)
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(targets[cards[len(cards)-1-i]], link); err != nil {
			t.Fatal(err)
		}
	}
	levels := amdLevelFiles(t, g)
	buses := make([]string, 0, len(levels))
	for bus := range levels {
		buses = append(buses, bus)
	}
	sort.Strings(buses)
	firstCardBus, err := filepath.EvalSymlinks(filepath.Join(drm, cards[0], "device"))
	if err != nil || filepath.Base(firstCardBus) != buses[len(buses)-1] {
		t.Fatalf("%s is the device at %q, want the last bus %s: the reversal did not take", cards[0], firstCardBus, buses[len(buses)-1])
	}
	operator := buses[len(buses)-1]
	if err := os.WriteFile(levels[operator], []byte("low\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := g.rig(t, nil, &v1alpha1.GPUGovernorIntent{Preset: joulie.GPUGovernorPresetSustained})
	r.reconcile()
	for bus, f := range levels {
		if got := readTrimmed(t, f); got != "high" {
			t.Fatalf("%s level %q, want high", bus, got)
		}
	}
	r.setIntents(nil, nil)
	r.reconcile()
	for bus, f := range levels {
		want := "auto"
		if bus == operator {
			want = "low"
		}
		if got := readTrimmed(t, f); got != want {
			t.Errorf("%s level %q after the restore, want its own baseline %q", bus, got, want)
		}
	}
}
