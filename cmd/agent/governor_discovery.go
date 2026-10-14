package main

// Governor discovery: what the agent reads about the frequency governors of
// its node, without writing anything. pkg/governor/discover reads the facts
// (cpufreq for the CPU governor, the energy-performance preference, boost
// and the frequency landmarks; amdgpu sysfs for the performance level and
// DPM tables of every AMD GPU, keyed by PCI bus id; nvidia-smi for the
// clocks of every NVIDIA GPU), and pkg/governor turns them into the presets
// the node accepts, which NodeHardware publishes with the baseline the agent
// restores.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/agent/dvfs"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/pkg/governor/discover"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// Warnings in NodeHardware.status.gpu.warnings when a GPU governor discovery
// failed because a tool did. They name no tool output, so the status does
// not change with the wording of an error.
const (
	warnNvidiaClockQuery      = "gpu governor not discovered: the nvidia-smi clock query failed"
	warnNvidiaSupportedClocks = "gpu governor not discovered: nvidia-smi -q -d SUPPORTED_CLOCKS failed"
)

// governorDiscovery is what discoverHardware read about the governors of the
// node.
type governorDiscovery struct {
	// CPU is nil when no cpufreq policy has a scaling_governor file, as on a
	// VM without cpufreq or a capture that recorded the driver only.
	CPU *cpuGovernorDiscovery
	// GPU is nil when no GPU governor was discovered.
	GPU *gpuGovernorDiscovery
	// GPUWarnings say why GPU is nil when a tool failed.
	GPUWarnings []string
	// GPUKept is the last GPU discovery of this run that was not empty, set
	// by the governor control while GPU is nil: NodeHardware publishes it
	// in place of an empty one.
	GPUKept *gpuGovernorDiscovery
	// CPUBaseline and GPUBaseline are the baselines the governor control
	// holds (prepare), which NodeHardware publishes: the state discovery
	// found while the agent had nothing of its own written, never one it
	// may have set. nil before the control has read the stored ones.
	CPUBaseline *v1alpha1.CPUGovernorBaseline
	GPUBaseline *v1alpha1.GPUGovernorBaseline
}

// cpuGovernorDiscovery is the cpufreq governor state of the node.
type cpuGovernorDiscovery struct {
	// Facts is what pkg/governor plans with.
	Facts governor.CPUFacts
	// Policies is the state of every policy in Facts.Policies, in order.
	Policies []discover.Policy
	// Inventory is the published block, without the baseline.
	Inventory v1alpha1.CPUGovernorInventory
}

// gpuGovernorDiscovery is the GPU governor state of the node: amdgpu devices
// or NVIDIA GPUs, never both.
type gpuGovernorDiscovery struct {
	// AMD holds one entry per amdgpu device, sorted by bus id.
	AMD []governor.AMDFacts
	// NVIDIA holds one entry per NVIDIA GPU, in the order nvidia-smi lists
	// them.
	NVIDIA []nvidiaGovernorFacts
	// Devices is the state of every GPU now, keyed by bus id: the baseline
	// when none is stored yet.
	Devices []v1alpha1.GPUGovernorDeviceState
	// Inventory is the published block, without the baseline.
	Inventory v1alpha1.GPUGovernorInventory
}

// nvidiaGovernorFacts is one NVIDIA GPU: the bus id nvidia-smi -i takes and
// what pkg/governor plans with.
type nvidiaGovernorFacts struct {
	BusID string
	Facts governor.NVFacts
}

// discoverGovernors reads the CPU governor and, for the GPU vendor discovery
// settled on, the GPU governor. The NVIDIA clock queries run only when the
// nvidia-smi inventory listed GPUs, as the cap path runs only then; facts,
// when not nil, keeps their static answers between reconciles.
func discoverGovernors(ctx context.Context, nodeLabels map[string]string, hw HardwareInfo, facts *nvidiaFactsCache) governorDiscovery {
	d := governorDiscovery{CPU: discoverCPUGovernor()}
	if hw.GPUVendor != "nvidia" || len(hw.GPUDevices) == 0 {
		facts.forget()
	}
	switch hw.GPUVendor {
	case "amd":
		d.GPU = discoverAMDGovernor()
	case "nvidia":
		if len(hw.GPUDevices) > 0 {
			var warning string
			d.GPU, warning = discoverNvidiaGovernor(ctx, nodeLabels, hw.GPURawModel, hw.GPUDevices, facts)
			if warning != "" {
				d.GPUWarnings = append(d.GPUWarnings, warning)
			}
		}
	}
	return d
}

// nvidiaFactsTTL bounds how long the static NVIDIA governor facts are
// reused, so a driver reload that changes them without a failure or a
// change of the GPU list shows within it.
const nvidiaFactsTTL = 10 * time.Minute

// nvidiaFactsCache keeps the static governor facts of the node's NVIDIA
// GPUs (bus ids, supported clocks, maximum clocks), which take two
// nvidia-smi runs to read, between reconciles. They are read again when
// the architecture or the GPU list of the inventory changes (index, name and
// power limit range of every GPU), after any failed nvidia-smi run of the
// agent, and once they are nvidiaFactsTTL old. A nil cache reads them every
// time.
type nvidiaFactsCache struct {
	// now is the clock, time.Now when nil.
	now func() time.Time

	key      string
	failures int64
	at       time.Time
	gpus     []discover.NvidiaGPU
}

// nvidiaToolFailures counts the failed nvidia-smi runs of the agent; a
// cache filled before the latest one is stale.
var nvidiaToolFailures atomic.Int64

// nvidiaFactsKey is the architecture and the GPU list the facts belong to.
func nvidiaFactsKey(arch string, devices []GPUDevice) string {
	var b strings.Builder
	b.WriteString(arch)
	for _, d := range devices {
		fmt.Fprintf(&b, "|%d,%s,%g,%g", d.Index, d.Product, d.MinCapWatts, d.MaxCapWatts)
	}
	return b.String()
}

// read returns the facts of the GPUs in devices, from the cache when it
// holds them for this architecture and GPU list, else from nvidia-smi.
func (c *nvidiaFactsCache) read(ctx context.Context, arch string, devices []GPUDevice) ([]discover.NvidiaGPU, error) {
	if c == nil {
		return discover.ReadNvidia(ctx, runTool, arch)
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	key := nvidiaFactsKey(arch, devices)
	if c.gpus != nil && c.key == key && c.failures == nvidiaToolFailures.Load() && now().Sub(c.at) < nvidiaFactsTTL {
		return c.gpus, nil
	}
	c.forget()
	failures := nvidiaToolFailures.Load()
	gpus, err := discover.ReadNvidia(ctx, runTool, arch)
	if err != nil {
		return nil, err
	}
	c.key, c.failures, c.at, c.gpus = key, failures, now(), gpus
	return gpus, nil
}

// forget drops the cached facts.
func (c *nvidiaFactsCache) forget() {
	if c != nil {
		c.key, c.gpus = "", nil
	}
}

// hostSysPath is rel under the host's /sys.
func hostSysPath(rel ...string) string {
	return filepath.Join(append([]string{dvfs.HostSysRoot}, rel...)...)
}

// discoverCPULandmarks reads the frequency landmarks of cpufreq; nil when no
// policy has any of them.
func discoverCPULandmarks() *v1alpha1.CPULandmarks {
	lm := discover.ReadLandmarks(dvfs.HostSysRoot)
	if lm == (discover.Landmarks{}) {
		return nil
	}
	return &v1alpha1.CPULandmarks{MinFreqMHz: lm.MinMHz, MaxBoostMHz: lm.MaxBoostMHz, NominalFreqMHz: lm.NominalMHz}
}

// uniformOrMixed is the one value every entry has, mixed when they differ,
// empty when there is none.
func uniformOrMixed(values []string) string {
	if len(values) == 0 {
		return ""
	}
	for _, v := range values[1:] {
		if v != values[0] {
			return joulie.GovernorCurrentMixed
		}
	}
	return values[0]
}

// discoverCPUGovernor reads the cpufreq governor state of every policy with
// a scaling_governor file.
func discoverCPUGovernor() *cpuGovernorDiscovery {
	c := discover.ReadCPU(dvfs.HostSysRoot, procCPUInfoPath)
	if c == nil {
		return nil
	}
	f := c.Facts
	var govs, epps, boosts []string
	for _, s := range c.Policies {
		govs = append(govs, s.Governor)
		epps = append(epps, s.EPP)
		boosts = append(boosts, s.Boost)
	}
	inv := v1alpha1.CPUGovernorInventory{
		Driver:     f.Driver,
		DriverMode: f.DriverMode,
		Current:    uniformOrMixed(govs),
		Available:  f.Governors,
		Presets:    cpuPresetSupport(f),
		Writable:   discover.Writable(c.Files),
	}
	if c.HasEPP {
		inv.EPP = &v1alpha1.CPUEPPInventory{Current: uniformOrMixed(epps), Available: f.EPPs}
	}
	if f.BoostScope != "" {
		state := uniformOrMixed(boosts)
		if state == joulie.GovernorCurrentMixed {
			state = "" // the enum has on and off only; the baseline keeps each group's
		}
		inv.Boost = &v1alpha1.CPUBoostInventory{State: state, Scope: f.BoostScope}
	}
	return &cpuGovernorDiscovery{Facts: f, Policies: c.Policies, Inventory: inv}
}

// cpuPresetSupport lists the CPU presets f accepts, with the result and the
// native state pkg/governor plans for each.
func cpuPresetSupport(f governor.CPUFacts) []v1alpha1.GovernorPresetSupport {
	var out []v1alpha1.GovernorPresetSupport
	for _, p := range governor.PresetsCPU(f) {
		plan, err := governor.PlanCPU(f, p, "")
		if err != nil || plan.Result == joulie.GovernorResultUnsupported {
			continue
		}
		out = append(out, v1alpha1.GovernorPresetSupport{Preset: p, Result: plan.Result, Native: plan.Native})
	}
	return out
}

// baseline groups the policies that share a governor, preference and boost
// state, in the order the groups first appear.
func (c *cpuGovernorDiscovery) baseline() *v1alpha1.CPUGovernorBaseline {
	type key struct{ governor, epp, boost string }
	var order []key
	members := map[key][]int{}
	for _, s := range c.Policies {
		k := key{s.Governor, s.EPP, s.Boost}
		if _, ok := members[k]; !ok {
			order = append(order, k)
		}
		members[k] = append(members[k], s.ID)
	}
	b := &v1alpha1.CPUGovernorBaseline{}
	for _, k := range order {
		b.Groups = append(b.Groups, v1alpha1.CPUGovernorState{
			Policies: formatCPUList(members[k]), Governor: k.governor, EPP: k.epp, Boost: k.boost,
		})
	}
	return b
}

// formatCPUList writes sorted numbers in the kernel's list form, such as
// 0-63 or 0,2,4.
func formatCPUList(ids []int) string {
	var parts []string
	for i := 0; i < len(ids); {
		j := i
		for j+1 < len(ids) && ids[j+1] == ids[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, strconv.Itoa(ids[i])+"-"+strconv.Itoa(ids[j]))
		} else {
			parts = append(parts, strconv.Itoa(ids[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// widenRange merges [lo, hi] into r, a nil r meaning no range yet; zero
// bounds are ignored.
func widenRange(r *v1alpha1.ClockRangeMHz, lo, hi float64) *v1alpha1.ClockRangeMHz {
	if lo <= 0 && hi <= 0 {
		return r
	}
	if r == nil {
		r = &v1alpha1.ClockRangeMHz{}
	}
	if lo > 0 && (r.Min == 0 || lo < r.Min) {
		r.Min = lo
	}
	if hi > r.Max {
		r.Max = hi
	}
	return r
}

// gpuClockRanges is nil when every domain is empty.
func gpuClockRanges(r v1alpha1.GPUClockRanges) *v1alpha1.GPUClockRanges {
	if r.Graphics == nil && r.Memory == nil && r.Fabric == nil {
		return nil
	}
	return &r
}

// discoverAMDGovernor reads every amdgpu device of the node, keyed by bus
// id; nil when there is none.
func discoverAMDGovernor() *gpuGovernorDiscovery {
	devices := discover.ReadAMD(dvfs.HostSysRoot)
	if len(devices) == 0 {
		return nil
	}
	d := &gpuGovernorDiscovery{}
	var ranges v1alpha1.GPUClockRanges
	var files, levels []string
	for _, dev := range devices {
		f := dev.Facts
		d.AMD = append(d.AMD, f)
		d.Devices = append(d.Devices, v1alpha1.GPUGovernorDeviceState{BusID: f.BusID, Level: f.Level})
		levels = append(levels, f.Level)
		files = append(files, dev.Files...)
		ranges.Graphics = widenRange(ranges.Graphics, float64(f.SclkMHz[0]), float64(f.SclkMHz[1]))
		ranges.Memory = widenRange(ranges.Memory, float64(f.MclkMHz[0]), float64(f.MclkMHz[1]))
		ranges.Fabric = widenRange(ranges.Fabric, float64(f.FclkMHz[0]), float64(f.FclkMHz[1]))
	}
	current := uniformOrMixed(levels)
	d.Inventory = v1alpha1.GPUGovernorInventory{
		Mechanism: joulie.GovernorBackendAMDGPUSysfs,
		Current:   current,
		Presets: gpuPresetSupport(len(d.AMD), func(i int, p string) (governor.Plan, error) {
			return governor.PlanAMD(d.AMD[i], p, governor.DefaultCeilingPctOfMax)
		}),
		ClockRangesMHz: gpuClockRanges(ranges),
		Writable:       discover.Writable(files),
		Uniform:        current != joulie.GovernorCurrentMixed,
	}
	return d
}

// gpuPresetSupport lists the GPU presets every one of n GPUs accepts, plan
// giving the plan of preset p on GPU i, judged at the default ceiling. A
// preset is applied when it is applied on every GPU, else equivalent; its
// native state is the first GPU's.
func gpuPresetSupport(n int, plan func(i int, preset string) (governor.Plan, error)) []v1alpha1.GovernorPresetSupport {
	var out []v1alpha1.GovernorPresetSupport
	for _, p := range joulie.GPUGovernorPresetValues {
		s := v1alpha1.GovernorPresetSupport{Preset: p, Result: joulie.GovernorResultApplied}
		ok := n > 0
		for i := 0; i < n && ok; i++ {
			pl, err := plan(i, p)
			switch {
			case err != nil || pl.Result == joulie.GovernorResultUnsupported:
				ok = false
			case pl.Result == joulie.GovernorResultEquivalent:
				s.Result = joulie.GovernorResultEquivalent
			}
			if i == 0 {
				s.Native = pl.Native
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// runTool is runCommand as a pkg/governor/discover Runner.
func runTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	return runCommand(ctx, name, args...)
}

// minMaxMHz is the lowest and highest of xs, zero for none.
func minMaxMHz(xs []int) (lo, hi float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	return float64(slices.Min(xs)), float64(slices.Max(xs))
}

// discoverNvidiaGovernor reads the clocks of every NVIDIA GPU through
// nvidia-smi. Without a mapping table for the architecture it runs nothing
// and publishes nothing. A failed or empty query leaves the block out and
// returns the warning that says so. A clock lock cannot be read back, so the
// current state and the baseline are unknown until the agent sets a lock of
// its own.
func discoverNvidiaGovernor(ctx context.Context, nodeLabels map[string]string, product string, devices []GPUDevice, facts *nvidiaFactsCache) (*gpuGovernorDiscovery, string) {
	arch := discover.NvidiaArch(nodeLabels, product)
	if arch == "" {
		return nil, ""
	}
	gpus, err := facts.read(ctx, arch, devices)
	if err != nil {
		warning := warnNvidiaClockQuery
		if errors.Is(err, discover.ErrNvidiaSupportedClocks) {
			warning = warnNvidiaSupportedClocks
		}
		logGovernorWarningOnce(warning, err)
		return nil, warning
	}
	d := &gpuGovernorDiscovery{}
	var ranges v1alpha1.GPUClockRanges
	for _, g := range gpus {
		d.NVIDIA = append(d.NVIDIA, nvidiaGovernorFacts{BusID: g.BusID, Facts: g.Facts})
		d.Devices = append(d.Devices, v1alpha1.GPUGovernorDeviceState{BusID: g.BusID, Level: joulie.GovernorStateUnknown})
		smLo, smHi := minMaxMHz(g.Facts.SupportedSMMHz)
		memLo, memHi := minMaxMHz(g.Facts.SupportedMemMHz)
		ranges.Graphics = widenRange(ranges.Graphics, smLo, max(smHi, g.MaxSMMHz))
		ranges.Memory = widenRange(ranges.Memory, memLo, max(memHi, g.MaxMemMHz))
	}
	d.Inventory = v1alpha1.GPUGovernorInventory{
		Mechanism: joulie.GovernorBackendNvidiaSMI,
		Current:   joulie.GovernorStateUnknown,
		Presets: gpuPresetSupport(len(d.NVIDIA), func(i int, p string) (governor.Plan, error) {
			return governor.PlanNvidia(d.NVIDIA[i].Facts, d.NVIDIA[i].BusID, p, governor.DefaultCeilingPctOfMax)
		}),
		ClockRangesMHz: gpuClockRanges(ranges),
		// No file to probe: nvidia-smi answered, and a refused lock shows
		// only when the agent sets one.
		Writable: true,
		// A lock cannot be read, so no two GPUs are known to share a state.
		Uniform: false,
	}
	return d, ""
}

// governorWarningsLogged holds the warnings already logged, so a tool that
// fails on every reconcile logs once.
var governorWarningsLogged sync.Map

func logGovernorWarningOnce(warning string, err error) {
	if _, seen := governorWarningsLogged.LoadOrStore(warning, true); !seen {
		log.Printf("warning: %s: %v", warning, err)
	}
}

// storedGovernors reads the governor blocks a NodeHardware object already
// holds, through the typed status. Either is nil when absent.
func storedGovernors(obj *unstructured.Unstructured) (*v1alpha1.CPUGovernorInventory, *v1alpha1.GPUGovernorInventory) {
	if obj == nil {
		return nil, nil
	}
	var nh v1alpha1.NodeHardware
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &nh); err != nil {
		return nil, nil
	}
	var cpu *v1alpha1.CPUGovernorInventory
	var gpu *v1alpha1.GPUGovernorInventory
	if c := nh.Status.CPU; c != nil {
		cpu = c.Governor
	}
	if g := nh.Status.GPU; g != nil {
		gpu = g.Governor
	}
	return cpu, gpu
}

// storedGovernorBaselines reads the baselines a NodeHardware object already
// holds. Either is nil when absent.
func storedGovernorBaselines(obj *unstructured.Unstructured) (*v1alpha1.CPUGovernorBaseline, *v1alpha1.GPUGovernorBaseline) {
	c, g := storedGovernors(obj)
	var cpu *v1alpha1.CPUGovernorBaseline
	var gpu *v1alpha1.GPUGovernorBaseline
	if c != nil && c.Baseline != nil && len(c.Baseline.Groups) > 0 {
		cpu = c.Baseline
	}
	if g != nil && g.Baseline != nil && len(g.Baseline.Devices) > 0 {
		gpu = g.Baseline
	}
	return cpu, gpu
}

// addGovernorStatus adds the cpufreq landmarks, the governor blocks and the
// GPU governor warnings of hw to the NodeHardware status map being built.
// The baseline is the one the governor control holds (prepare), else the
// one obj already stores, else the state discovered now. A GPU discovery
// that comes back empty publishes the last one of this run that did not
// (GPUKept) or, before there is one (a tool that did not answer at start),
// the block obj stores, presets and baseline included, so neither the
// presets nor the baseline drop out for a reconcile.
func addGovernorStatus(status map[string]any, hw HardwareInfo, obj *unstructured.Unstructured) error {
	cpuMap, _ := status["cpu"].(map[string]any)
	gpuMap, _ := status["gpu"].(map[string]any)
	storedCPU, storedGPU := storedGovernorBaselines(obj)
	put := func(m map[string]any, key string, v any) error {
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(v)
		if err != nil {
			return err
		}
		m[key] = u
		return nil
	}
	if cpuMap != nil && hw.CPULandmarks != nil {
		if err := put(cpuMap, "landmarks", hw.CPULandmarks); err != nil {
			return err
		}
	}
	if c := hw.Governors.CPU; cpuMap != nil && c != nil {
		inv := c.Inventory
		inv.Baseline = cmp.Or(hw.Governors.CPUBaseline, storedCPU, c.baseline())
		if err := put(cpuMap, "governor", &inv); err != nil {
			return err
		}
	}
	if gpuMap == nil {
		return nil
	}
	if g := cmp.Or(hw.Governors.GPU, hw.Governors.GPUKept); g != nil {
		inv := g.Inventory
		inv.Baseline = cmp.Or(hw.Governors.GPUBaseline, storedGPU, &v1alpha1.GPUGovernorBaseline{Devices: g.Devices})
		if err := put(gpuMap, "governor", &inv); err != nil {
			return err
		}
	} else if kept := cmp.Or(hw.Governors.GPUBaseline, storedGPU); kept != nil {
		inv := v1alpha1.GPUGovernorInventory{}
		if _, stored := storedGovernors(obj); stored != nil {
			inv = *stored
		}
		inv.Baseline = kept
		if err := put(gpuMap, "governor", &inv); err != nil {
			return err
		}
	}
	if len(hw.Governors.GPUWarnings) > 0 {
		warnings, _ := gpuMap["warnings"].([]any)
		gpuMap["warnings"] = append(warnings, stringAnySlice(hw.Governors.GPUWarnings)...)
	}
	return nil
}
