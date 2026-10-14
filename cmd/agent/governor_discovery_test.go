package main

// Tests of the governor discovery over synthetic sysfs trees and tool
// outputs. The clock values are neutral, not those of any machine.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor/discover"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// acpiCPUFreqTree is four acpi-cpufreq policies under schedutil with the six
// generic governors and the global boost on.
func acpiCPUFreqTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{"devices/system/cpu/cpufreq/boost": "1\n"}
	for p := 0; p < 4; p++ {
		dir := fmt.Sprintf("devices/system/cpu/cpufreq/policy%d/", p)
		files[dir+"scaling_driver"] = "acpi-cpufreq\n"
		files[dir+"scaling_governor"] = "schedutil\n"
		files[dir+"scaling_available_governors"] = "conservative ondemand userspace powersave performance schedutil\n"
		files[dir+"cpuinfo_min_freq"] = "300000\n"
		files[dir+"cpuinfo_max_freq"] = "1500000\n"
	}
	sysfsFiles(t, root, files)
	return root
}

// GA14: an acpi-cpufreq node publishes its governor block: schedutil, the
// six available governors, no EPP, the global boost, all three presets, and
// one baseline group.
func TestGA14ACPICPUFreqGovernorBlock(t *testing.T) {
	pointAgentAtSysfs(t, acpiCPUFreqTree(t), "", nil)

	status := publishedStatus(t, "ga14-acpi", nil)
	if status.CPU == nil || status.CPU.Governor == nil {
		t.Fatalf("no cpu.governor block; cpu status: %+v", status.CPU)
	}
	want := v1alpha1.CPUGovernorInventory{
		Driver:    "acpi-cpufreq",
		Current:   "schedutil",
		Available: []string{"conservative", "ondemand", "userspace", "powersave", "performance", "schedutil"},
		Boost:     &v1alpha1.CPUBoostInventory{State: joulie.CPUGovernorBoostOn, Scope: joulie.CPUBoostScopeGlobal},
		Presets: []v1alpha1.GovernorPresetSupport{
			{Preset: joulie.CPUGovernorPresetPerformance, Result: joulie.GovernorResultApplied, Native: "performance"},
			{Preset: joulie.CPUGovernorPresetBalanced, Result: joulie.GovernorResultApplied, Native: "schedutil"},
			{Preset: joulie.CPUGovernorPresetEfficiency, Result: joulie.GovernorResultApplied, Native: "conservative"},
		},
		Baseline: &v1alpha1.CPUGovernorBaseline{Groups: []v1alpha1.CPUGovernorState{
			{Policies: "0-3", Governor: "schedutil", Boost: joulie.CPUGovernorBoostOn},
		}},
		Writable: true,
	}
	if got := *status.CPU.Governor; !reflect.DeepEqual(got, want) {
		t.Fatalf("cpu.governor\n got %+v\nwant %+v", got, want)
	}
}

// GA15: an intel_pstate node in active mode with hardware P-states publishes
// the current energy-performance preference and the available ones, and the
// presets that set them.
func TestGA15IntelPstateHWPPublishesEPP(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"devices/system/cpu/intel_pstate/status":   "active\n",
		"devices/system/cpu/intel_pstate/no_turbo": "0\n",
	}
	for p := 0; p < 2; p++ {
		dir := fmt.Sprintf("devices/system/cpu/cpufreq/policy%d/", p)
		files[dir+"scaling_driver"] = "intel_pstate\n"
		files[dir+"scaling_governor"] = "powersave\n"
		files[dir+"scaling_available_governors"] = "performance powersave\n"
		files[dir+"energy_performance_preference"] = "balance_performance\n"
		files[dir+"energy_performance_available_preferences"] = "default performance balance_performance balance_power power\n"
	}
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "processor\t: 0\nflags\t\t: fpu tsc hwp hwp_epp\n\n", nil)

	status := publishedStatus(t, "ga15-hwp", nil)
	if status.CPU == nil || status.CPU.Governor == nil {
		t.Fatalf("no cpu.governor block; cpu status: %+v", status.CPU)
	}
	g := status.CPU.Governor
	wantEPP := &v1alpha1.CPUEPPInventory{Current: "balance_performance", Available: []string{"default", "performance", "balance_performance", "balance_power", "power"}}
	if !reflect.DeepEqual(g.EPP, wantEPP) {
		t.Errorf("cpu.governor.epp = %+v, want %+v", g.EPP, wantEPP)
	}
	if g.Driver != "intel_pstate" || g.DriverMode != joulie.CPUDriverModeActive || g.Current != "powersave" {
		t.Errorf("driver %q mode %q current %q, want intel_pstate active powersave", g.Driver, g.DriverMode, g.Current)
	}
	if want := (&v1alpha1.CPUBoostInventory{State: joulie.CPUGovernorBoostOn, Scope: joulie.CPUBoostScopeNoTurbo}); !reflect.DeepEqual(g.Boost, want) {
		t.Errorf("cpu.governor.boost = %+v, want %+v (no_turbo 0 is boost on)", g.Boost, want)
	}
	wantPresets := []v1alpha1.GovernorPresetSupport{
		{Preset: joulie.CPUGovernorPresetPerformance, Result: joulie.GovernorResultApplied, Native: "performance"},
		{Preset: joulie.CPUGovernorPresetBalanced, Result: joulie.GovernorResultApplied, Native: "powersave epp=balance_performance"},
		{Preset: joulie.CPUGovernorPresetEfficiency, Result: joulie.GovernorResultApplied, Native: "powersave epp=balance_power"},
	}
	if !reflect.DeepEqual(g.Presets, wantPresets) {
		t.Errorf("cpu.governor.presets\n got %+v\nwant %+v", g.Presets, wantPresets)
	}
	wantBaseline := &v1alpha1.CPUGovernorBaseline{Groups: []v1alpha1.CPUGovernorState{
		{Policies: "0-1", Governor: "powersave", EPP: "balance_performance", Boost: joulie.CPUGovernorBoostOn},
	}}
	if !reflect.DeepEqual(g.Baseline, wantBaseline) {
		t.Errorf("cpu.governor.baseline = %+v, want %+v", g.Baseline, wantBaseline)
	}
}

// amdDevice writes one amdgpu device of the Radeon Pro W7900 family at bus
// under bus/pci/devices, at performance level level.
func amdDevice(files map[string]string, bus, level string) {
	dir := "bus/pci/devices/" + bus + "/"
	files[dir+"vendor"] = "0x1002\n"
	files[dir+"device"] = "0x7448\n"
	files[dir+"power_dpm_force_performance_level"] = level + "\n"
	files[dir+"pp_dpm_sclk"] = "0: 500Mhz\n1: 1500Mhz *\nS: 20Mhz\n"
	files[dir+"pp_dpm_mclk"] = "0: 300Mhz *\n1: 1200Mhz\n"
	files[dir+"pp_dpm_fclk"] = "0: 400Mhz *\n1: 1000Mhz\n"
}

// GA16: two AMD GPUs are published by bus id, in bus order, each with its
// own level; the node is not uniform.
func TestGA16AMDGPUsArePublishedByBusID(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	amdDevice(files, "0000:20:00.0", "auto")
	amdDevice(files, "0000:10:00.0", "high")
	// The HD audio function of a GPU has AMD's vendor id and no level file.
	files["bus/pci/devices/0000:10:00.1/vendor"] = "0x1002\n"
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", nil)

	status := publishedStatus(t, "ga16-amd", map[string]string{"feature.node.kubernetes.io/pci-1002.present": "true"})
	if status.GPU == nil || status.GPU.Governor == nil {
		t.Fatalf("no gpu.governor block; gpu status: %+v", status.GPU)
	}
	g := status.GPU.Governor
	wantDevices := []v1alpha1.GPUGovernorDeviceState{{BusID: "0000:10:00.0", Level: "high"}, {BusID: "0000:20:00.0", Level: "auto"}}
	if g.Baseline == nil || !reflect.DeepEqual(g.Baseline.Devices, wantDevices) {
		t.Fatalf("gpu.governor.baseline = %+v, want devices %+v", g.Baseline, wantDevices)
	}
	if g.Mechanism != joulie.GovernorBackendAMDGPUSysfs || g.Current != joulie.GovernorCurrentMixed || g.Uniform {
		t.Errorf("mechanism %q current %q uniform %v, want amdgpu-sysfs, mixed, false", g.Mechanism, g.Current, g.Uniform)
	}
	var presets []string
	for _, p := range g.Presets {
		presets = append(presets, p.Preset+"="+p.Native)
	}
	if want := []string{"auto=auto", "sustained=high", "low=low", "peak=profile_peak"}; !slices.Equal(presets, want) {
		t.Errorf("gpu.governor.presets %v, want %v (Navi 31 has no overdrive ceiling for efficient)", presets, want)
	}
	wantRanges := &v1alpha1.GPUClockRanges{
		Graphics: &v1alpha1.ClockRangeMHz{Min: 500, Max: 1500},
		Memory:   &v1alpha1.ClockRangeMHz{Min: 300, Max: 1200},
		Fabric:   &v1alpha1.ClockRangeMHz{Min: 400, Max: 1000},
	}
	if !reflect.DeepEqual(g.ClockRangesMHz, wantRanges) {
		t.Errorf("gpu.governor.clockRangesMHz = %+v, want %+v (the deep-sleep line is not a level)", g.ClockRangesMHz, wantRanges)
	}
}

// Command keys of the fake runner for the NVIDIA tests.
var (
	nvidiaInventoryKey = "nvidia-smi --query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name --format=csv,noheader,nounits"
	nvidiaClockKey     = "nvidia-smi " + strings.Join(discover.NvidiaClockQueryArgs, " ")
	nvidiaSupportedKey = "nvidia-smi " + strings.Join(discover.NvidiaSupportedClocksArgs, " ")
)

// nvidiaSupportedClocksOutput is nvidia-smi -q -d SUPPORTED_CLOCKS of two
// GPUs with one memory clock each.
const nvidiaSupportedClocksOutput = `
==============NVSMI LOG==============

Timestamp                                 : Thu Jan  1 00:00:00 2026
Driver Version                            : 0.0
CUDA Version                              : 0.0

Attached GPUs                             : 2
GPU 00000000:18:00.0
    Supported Clocks
        Memory                            : 1500 MHz
            Graphics                      : 1500 MHz
            Graphics                      : 1200 MHz
            Graphics                      : 900 MHz
            Graphics                      : 600 MHz
            Graphics                      : 300 MHz

GPU 00000000:2A:00.0
    Supported Clocks
        Memory                            : 1500 MHz
            Graphics                      : 1500 MHz
            Graphics                      : 1200 MHz
            Graphics                      : 900 MHz
            Graphics                      : 600 MHz
            Graphics                      : 300 MHz
`

// nvidiaRunner answers the inventory query, the clock query and the
// supported clocks of two Hopper GPUs; failing names a command that fails.
func nvidiaRunner(failing string) fakeCommandRunner {
	r := fakeCommandRunner{responses: map[string]fakeCommandResult{
		nvidiaInventoryKey: {out: "0, 200.00, 700.00, 700.00, 90.00, NVIDIA H100 80GB HBM3\n1, 200.00, 700.00, 700.00, 90.00, NVIDIA H100 80GB HBM3\n"},
		nvidiaClockKey:     {out: "00000000:18:00.0, 345, 1500, 1500, 1500, P0, 0x0000000000000001\n00000000:2A:00.0, 345, 1500, 1500, 1500, P0, 0x0000000000000001\n"},
		nvidiaSupportedKey: {out: nvidiaSupportedClocksOutput},
	}}
	if failing != "" {
		r.responses[failing] = fakeCommandResult{err: errors.New("exit status 2")}
	}
	return r
}

var hopperLabels = map[string]string{
	"feature.node.kubernetes.io/pci-10de.present": "true",
	"nvidia.com/gpu.compute.major":                "9",
	"nvidia.com/gpu.compute.minor":                "0",
}

// GA17: the NVIDIA clock query and the supported clocks parse into
// clockRangesMHz and the presets; a failing tool leaves the governor block
// out with a warning.
func TestGA17NvidiaClockQueryParsesAndAToolFailureWarns(t *testing.T) {
	t.Run("parsed", func(t *testing.T) {
		pointAgentAtSysfs(t, t.TempDir(), "", nvidiaRunner(""))
		status := publishedStatus(t, "ga17-nvidia", hopperLabels)
		if status.GPU == nil || status.GPU.Governor == nil {
			t.Fatalf("no gpu.governor block; gpu status: %+v", status.GPU)
		}
		g := status.GPU.Governor
		wantRanges := &v1alpha1.GPUClockRanges{
			Graphics: &v1alpha1.ClockRangeMHz{Min: 300, Max: 1500},
			Memory:   &v1alpha1.ClockRangeMHz{Min: 1500, Max: 1500},
		}
		if !reflect.DeepEqual(g.ClockRangesMHz, wantRanges) {
			t.Errorf("gpu.governor.clockRangesMHz = %+v, want %+v", g.ClockRangesMHz, wantRanges)
		}
		if g.Mechanism != joulie.GovernorBackendNvidiaSMI || g.Current != joulie.GovernorStateUnknown {
			t.Errorf("mechanism %q current %q, want nvidia-smi, unknown (a lock cannot be read back)", g.Mechanism, g.Current)
		}
		wantDevices := []v1alpha1.GPUGovernorDeviceState{
			{BusID: "00000000:18:00.0", Level: joulie.GovernorStateUnknown},
			{BusID: "00000000:2A:00.0", Level: joulie.GovernorStateUnknown},
		}
		if g.Baseline == nil || !reflect.DeepEqual(g.Baseline.Devices, wantDevices) {
			t.Errorf("gpu.governor.baseline = %+v, want devices %+v", g.Baseline, wantDevices)
		}
		wantPresets := []v1alpha1.GovernorPresetSupport{
			{Preset: joulie.GPUGovernorPresetAuto, Result: joulie.GovernorResultEquivalent, Native: joulie.GovernorStateUnknown},
			{Preset: joulie.GPUGovernorPresetSustained, Result: joulie.GovernorResultEquivalent, Native: joulie.GovernorStateUnknown},
			{Preset: joulie.GPUGovernorPresetEfficient, Result: joulie.GovernorResultApplied, Native: "-lgc 300,1200"},
			{Preset: joulie.GPUGovernorPresetLow, Result: joulie.GovernorResultApplied, Native: "-lgc 300,300"},
			{Preset: joulie.GPUGovernorPresetPeak, Result: joulie.GovernorResultApplied, Native: "-lgc 1500,1500"},
		}
		if !reflect.DeepEqual(g.Presets, wantPresets) {
			t.Errorf("gpu.governor.presets\n got %+v\nwant %+v", g.Presets, wantPresets)
		}
		if slices.Contains(status.GPU.Warnings, warnNvidiaClockQuery) {
			t.Errorf("gpu.warnings %v names a failure that did not happen", status.GPU.Warnings)
		}
	})
	for _, tc := range []struct{ name, failing, warning string }{
		{"clock query fails", nvidiaClockKey, warnNvidiaClockQuery},
		{"supported clocks fail", nvidiaSupportedKey, warnNvidiaSupportedClocks},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pointAgentAtSysfs(t, t.TempDir(), "", nvidiaRunner(tc.failing))
			status := publishedStatus(t, "ga17-nvidia-failing", hopperLabels)
			if status.GPU == nil {
				t.Fatal("no gpu status")
			}
			if status.GPU.Governor != nil {
				t.Errorf("gpu.governor = %+v after %q failed, want no block", status.GPU.Governor, tc.failing)
			}
			if !slices.Contains(status.GPU.Warnings, tc.warning) {
				t.Errorf("gpu.warnings = %v, want %q", status.GPU.Warnings, tc.warning)
			}
			if !status.GPU.ControlAvailable {
				t.Errorf("the cap path must not depend on the governor queries: controlAvailable false")
			}
		})
	}
}

// GA18: the NodeHardware write is skipped when nothing changed, the governor
// block included; a governor change is written, and, with no governor
// control holding a baseline, the stored one is kept rather than recorded
// again over the new state.
func TestGA18GovernorWriteSkippedWhenUnchanged(t *testing.T) {
	root := acpiCPUFreqTree(t)
	pointAgentAtSysfs(t, root, "", nil)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodeHardwareGVR: "NodeHardwareList"})
	const nodeName = "ga18-governor"
	lastWrites.Delete("nodeHardware|" + sanitizeNodeObjectName(nodeName))
	node := &corev1.Node{}
	node.Name = nodeName
	statusWrites := func() int {
		n := 0
		for _, a := range dyn.Actions() {
			if a.GetVerb() == "update" && a.GetSubresource() == "status" {
				n++
			}
		}
		return n
	}
	publish := func() {
		t.Helper()
		if err := upsertNodeHardwareStatus(context.Background(), dyn, nodeName, discoverHardware(context.Background(), node)); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 3; i++ {
		publish()
	}
	if got := statusWrites(); got != 1 {
		t.Fatalf("status writes=%d want=1 (an unchanged governor must not be rewritten each tick)", got)
	}

	sysfsFiles(t, root, map[string]string{"devices/system/cpu/cpufreq/policy1/scaling_governor": "performance\n"})
	publish()
	if got := statusWrites(); got != 2 {
		t.Fatalf("status writes=%d want=2 (a governor change must be written)", got)
	}
	obj, err := dyn.Resource(nodeHardwareGVR).Get(context.Background(), sanitizeNodeObjectName(nodeName), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var nh v1alpha1.NodeHardware
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &nh); err != nil {
		t.Fatal(err)
	}
	g := nh.Status.CPU.Governor
	if g == nil || g.Current != joulie.GovernorCurrentMixed {
		t.Fatalf("cpu.governor %+v, want current mixed after one policy changed", g)
	}
	want := []v1alpha1.CPUGovernorState{{Policies: "0-3", Governor: "schedutil", Boost: joulie.CPUGovernorBoostOn}}
	if g.Baseline == nil || !reflect.DeepEqual(g.Baseline.Groups, want) {
		t.Fatalf("cpu.governor.baseline %+v, want the first discovery %+v kept", g.Baseline, want)
	}
}

// A cpufreq tree with the driver alone, as a capture that recorded only
// scaling_driver, publishes no governor block and no landmarks: the corpus
// replays through the emulated tree unchanged.
func TestGovernorDiscoveryNeedsAScalingGovernor(t *testing.T) {
	root := t.TempDir()
	sysfsFiles(t, root, map[string]string{"devices/system/cpu/cpufreq/policy0/scaling_driver": "intel_pstate\n"})
	pointAgentAtSysfs(t, root, "", nil)
	status := publishedStatus(t, "governor-driver-only", nil)
	if status.CPU == nil || status.CPU.Governor != nil || status.CPU.Landmarks != nil {
		t.Fatalf("cpu status %+v, want no governor block and no landmarks", status.CPU)
	}
}

// writable comes from an access(2) probe: a governor file the agent cannot
// write makes the block not writable, without any write.
func TestGovernorDiscoveryWritableComesFromAnAccessProbe(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root passes access(2) W_OK for every file")
	}
	root := acpiCPUFreqTree(t)
	file := filepath.Join(root, "devices", "system", "cpu", "cpufreq", "policy2", "scaling_governor")
	if err := os.Chmod(file, 0o444); err != nil {
		t.Fatal(err)
	}
	pointAgentAtSysfs(t, root, "", nil)
	status := publishedStatus(t, "governor-read-only", nil)
	if status.CPU == nil || status.CPU.Governor == nil || status.CPU.Governor.Writable {
		t.Fatalf("cpu.governor %+v, want writable false with a read-only scaling_governor", status.CPU.Governor)
	}
	if b, _ := os.ReadFile(file); string(b) != "schedutil\n" {
		t.Fatalf("discovery changed %s to %q", file, b)
	}
}

func TestFormatCPUList(t *testing.T) {
	for _, tc := range []struct {
		in   []int
		want string
	}{
		{nil, ""},
		{[]int{0}, "0"},
		{[]int{0, 1, 2, 3}, "0-3"},
		{[]int{0, 2, 4}, "0,2,4"},
		{[]int{0, 1, 2, 5, 7, 8}, "0-2,5,7-8"},
	} {
		if got := formatCPUList(tc.in); got != tc.want {
			t.Errorf("formatCPUList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
