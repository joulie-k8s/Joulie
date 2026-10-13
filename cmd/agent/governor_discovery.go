package main

// Governor discovery: what the agent reads about the frequency governors of
// its node, without writing anything. cpufreq gives the CPU governor, the
// energy-performance preference, boost and the frequency landmarks; amdgpu
// sysfs gives the performance level and DPM tables of every AMD GPU, keyed
// by PCI bus id; nvidia-smi gives the clocks of every NVIDIA GPU. pkg/governor
// turns these facts into the presets the node accepts, which NodeHardware
// publishes with the baseline the agent restores.

import (
	"cmp"
	"context"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/agent/dvfs"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// Directories the governor discovery reads, relative to dvfs.HostSysRoot,
// the host's /sys as mounted into the agent container.
const (
	cpufreqRelDir     = "devices/system/cpu/cpufreq"
	intelPstateRelDir = "devices/system/cpu/intel_pstate"
	amdPstateRelDir   = "devices/system/cpu/amd_pstate"
	pciDevicesRelDir  = "bus/pci/devices"
)

// accessWriteOK is W_OK of access(2). syscall has no name for it.
const accessWriteOK = 0x2

// nvidiaClockQueryArgs is the nvidia-smi clock query: the readback the agent
// also uses after a clock lock. Locks themselves have no getter.
var nvidiaClockQueryArgs = []string{
	"--query-gpu=pci.bus_id,clocks.sm,clocks.mem,clocks.max.sm,clocks.max.mem,pstate,clocks_event_reasons.active",
	"--format=csv,noheader,nounits",
}

// nvidiaSupportedClocksArgs lists the graphics and memory clocks every GPU
// accepts in a lock.
var nvidiaSupportedClocksArgs = []string{"-q", "-d", "SUPPORTED_CLOCKS"}

// Warnings in NodeHardware.status.gpu.warnings when the GPU governor block is
// left out because a tool failed. They name no tool output, so the status
// does not change with the wording of an error.
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
	// CPUBaseline and GPUBaseline are the baselines the governor control
	// holds (governor_control.go), published when the NodeHardware object
	// stores none: the state the agent first saw, never one it may have set.
	// nil before the control has seen the node.
	CPUBaseline *v1alpha1.CPUGovernorBaseline
	GPUBaseline *v1alpha1.GPUGovernorBaseline
}

// cpuGovernorDiscovery is the cpufreq governor state of the node.
type cpuGovernorDiscovery struct {
	// Facts is what pkg/governor plans with.
	Facts governor.CPUFacts
	// Policies is the state of every policy in Facts.Policies, in order.
	Policies []cpuPolicyState
	// Inventory is the published block, without the baseline.
	Inventory v1alpha1.CPUGovernorInventory
}

// cpuPolicyState is the governor state of one cpufreq policy. EPP and Boost
// are empty where the policy has none.
type cpuPolicyState struct {
	ID                   int
	Governor, EPP, Boost string
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
// settled on, the GPU governor. nvidiaAnswered is true when the nvidia-smi
// inventory query listed GPUs: the clock queries run only then, as the cap
// path runs only then.
func discoverGovernors(ctx context.Context, nodeLabels map[string]string, hw HardwareInfo, nvidiaAnswered bool) governorDiscovery {
	d := governorDiscovery{CPU: discoverCPUGovernor()}
	switch hw.GPUVendor {
	case "amd":
		d.GPU = discoverAMDGovernor()
	case "nvidia":
		if nvidiaAnswered {
			var warning string
			d.GPU, warning = discoverNvidiaGovernor(ctx, nodeLabels, hw.GPURawModel)
			if warning != "" {
				d.GPUWarnings = append(d.GPUWarnings, warning)
			}
		}
	}
	return d
}

// hostSysPath is rel under the host's /sys.
func hostSysPath(rel ...string) string {
	return filepath.Join(append([]string{dvfs.HostSysRoot}, rel...)...)
}

// readSysfsWord is the trimmed content of a sysfs attribute; ok is false
// when the file cannot be read.
func readSysfsWord(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// readSysfsMHz reads a frequency attribute in kHz as MHz; ok is false when
// the file is missing or holds no positive number.
func readSysfsMHz(path string) (float64, bool) {
	s, ok := readSysfsWord(path)
	if !ok {
		return 0, false
	}
	khz, err := strconv.ParseFloat(s, 64)
	if err != nil || khz <= 0 {
		return 0, false
	}
	return khz / 1000, true
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writable reports whether every path passes an access(2) W_OK probe, which
// writes nothing. It catches a read-only /sys mount and a non-root agent; a
// root agent passes it for every file, so a refusal of one attribute shows
// only when the agent writes it.
func writable(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if syscall.Access(p, accessWriteOK) != nil {
			return false
		}
	}
	return true
}

// cpufreqPolicyIDs lists the numbers N of the cpufreq policyN directories,
// sorted.
func cpufreqPolicyIDs() []int {
	matches, _ := filepath.Glob(hostSysPath(cpufreqRelDir, "policy*"))
	var ids []int
	for _, m := range matches {
		if n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(m), "policy")); err == nil {
			ids = append(ids, n)
		}
	}
	slices.Sort(ids)
	return ids
}

func policyFile(id int, name string) string {
	return hostSysPath(cpufreqRelDir, "policy"+strconv.Itoa(id), name)
}

// discoverCPULandmarks reads the frequency landmarks of cpufreq: the lowest
// cpuinfo_min_freq, the highest cpuinfo_max_freq (the maximum boost) and the
// highest base_frequency (the nominal frequency, intel_pstate only) over the
// policies. nil when no policy has any of them.
func discoverCPULandmarks() *v1alpha1.CPULandmarks {
	var lm v1alpha1.CPULandmarks
	for _, id := range cpufreqPolicyIDs() {
		if mhz, ok := readSysfsMHz(policyFile(id, "cpuinfo_min_freq")); ok && (lm.MinFreqMHz == 0 || mhz < lm.MinFreqMHz) {
			lm.MinFreqMHz = mhz
		}
		if mhz, ok := readSysfsMHz(policyFile(id, "cpuinfo_max_freq")); ok && mhz > lm.MaxBoostMHz {
			lm.MaxBoostMHz = mhz
		}
		if mhz, ok := readSysfsMHz(policyFile(id, "base_frequency")); ok && mhz > lm.NominalFreqMHz {
			lm.NominalFreqMHz = mhz
		}
	}
	if lm == (v1alpha1.CPULandmarks{}) {
		return nil
	}
	return &lm
}

// cpuInfoHasHWP reports whether /proc/cpuinfo lists the hwp flag (hardware
// P-states).
func cpuInfoHasHWP() bool {
	b, err := os.ReadFile(procCPUInfoPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == "flags" {
			return slices.Contains(strings.Fields(value), "hwp")
		}
	}
	return false
}

func isIntelPstateDriver(driver string) bool {
	return driver == "intel_pstate" || driver == "intel_cpufreq"
}

// cpuDriverMode is the intel_pstate or amd_pstate status word of driver, or
// empty for a driver without modes or a word Joulie does not know.
func cpuDriverMode(driver string) string {
	var status string
	switch {
	case isIntelPstateDriver(driver):
		status, _ = readSysfsWord(hostSysPath(intelPstateRelDir, "status"))
	case strings.HasPrefix(driver, "amd-pstate"):
		status, _ = readSysfsWord(hostSysPath(amdPstateRelDir, "status"))
	}
	if slices.Contains(joulie.CPUDriverModeValues, status) {
		return status
	}
	return ""
}

// boostWord maps a boost attribute (1 on) to the pkg/api boost word.
func boostWord(v string, inverted bool) string {
	if v != "0" && v != "1" {
		return ""
	}
	if on := v == "1"; on != inverted {
		return joulie.CPUGovernorBoostOn
	}
	return joulie.CPUGovernorBoostOff
}

// cpuBoostScope is where boost is set on driver, following the mapping of
// pkg/governor: the inverted no_turbo on intel_pstate, each policy's boost on
// amd-pstate, the global cpufreq/boost elsewhere; empty without a boost file.
func cpuBoostScope(driver string, firstPolicy int) string {
	global := fileExists(hostSysPath(cpufreqRelDir, "boost"))
	perCPU := fileExists(policyFile(firstPolicy, "boost"))
	switch {
	case isIntelPstateDriver(driver) && fileExists(hostSysPath(intelPstateRelDir, "no_turbo")):
		return joulie.CPUBoostScopeNoTurbo
	case strings.HasPrefix(driver, "amd-pstate") && perCPU:
		return joulie.CPUBoostScopePerCPU
	case global:
		return joulie.CPUBoostScopeGlobal
	case perCPU:
		return joulie.CPUBoostScopePerCPU
	}
	return ""
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
	var states []cpuPolicyState
	for _, id := range cpufreqPolicyIDs() {
		if gov, ok := readSysfsWord(policyFile(id, "scaling_governor")); ok && gov != "" {
			states = append(states, cpuPolicyState{ID: id, Governor: gov})
		}
	}
	if len(states) == 0 {
		return nil
	}
	first := states[0].ID
	driver, _ := readSysfsWord(policyFile(first, "scaling_driver"))
	available, _ := readSysfsWord(policyFile(first, "scaling_available_governors"))
	f := governor.CPUFacts{
		Driver:     driver,
		DriverMode: cpuDriverMode(driver),
		HWP:        cpuInfoHasHWP(),
		Governors:  strings.Fields(available),
	}
	hasEPP := fileExists(policyFile(first, "energy_performance_preference"))
	// intel_pstate creates energy_performance_preference only with hardware
	// P-states (hwp_cpufreq_attrs), so the file says HWP is on where
	// /proc/cpuinfo lists no hwp flag.
	f.HWP = f.HWP || (isIntelPstateDriver(driver) && hasEPP)
	if hasEPP {
		epps, _ := readSysfsWord(policyFile(first, "energy_performance_available_preferences"))
		f.EPPs = governor.EPPSupport(driver, states[0].Governor, strings.Fields(epps))
	}
	f.BoostScope = cpuBoostScope(driver, first)

	var globalBoost string
	switch f.BoostScope {
	case joulie.CPUBoostScopeGlobal:
		v, _ := readSysfsWord(hostSysPath(cpufreqRelDir, "boost"))
		globalBoost = boostWord(v, false)
	case joulie.CPUBoostScopeNoTurbo:
		v, _ := readSysfsWord(hostSysPath(intelPstateRelDir, "no_turbo"))
		globalBoost = boostWord(v, true)
	}

	files := []string{}
	var govs, epps, boosts []string
	for i := range states {
		s := &states[i]
		f.Policies = append(f.Policies, s.ID)
		files = append(files, policyFile(s.ID, "scaling_governor"))
		govs = append(govs, s.Governor)
		if hasEPP {
			s.EPP, _ = readSysfsWord(policyFile(s.ID, "energy_performance_preference"))
			epps = append(epps, s.EPP)
			files = append(files, policyFile(s.ID, "energy_performance_preference"))
		}
		s.Boost = globalBoost
		if f.BoostScope == joulie.CPUBoostScopePerCPU {
			v, _ := readSysfsWord(policyFile(s.ID, "boost"))
			s.Boost = boostWord(v, false)
			files = append(files, policyFile(s.ID, "boost"))
		}
		boosts = append(boosts, s.Boost)
	}
	switch f.BoostScope {
	case joulie.CPUBoostScopeGlobal:
		files = append(files, hostSysPath(cpufreqRelDir, "boost"))
	case joulie.CPUBoostScopeNoTurbo:
		files = append(files, hostSysPath(intelPstateRelDir, "no_turbo"))
	}

	inv := v1alpha1.CPUGovernorInventory{
		Driver:     f.Driver,
		DriverMode: f.DriverMode,
		Current:    uniformOrMixed(govs),
		Available:  f.Governors,
		Presets:    cpuPresetSupport(f),
		Writable:   writable(files),
	}
	if hasEPP {
		inv.EPP = &v1alpha1.CPUEPPInventory{Current: uniformOrMixed(epps), Available: f.EPPs}
	}
	if f.BoostScope != "" {
		state := uniformOrMixed(boosts)
		if state == joulie.GovernorCurrentMixed {
			state = "" // the enum has on and off only; the baseline keeps each group's
		}
		inv.Boost = &v1alpha1.CPUBoostInventory{State: state, Scope: f.BoostScope}
	}
	return &cpuGovernorDiscovery{Facts: f, Policies: states, Inventory: inv}
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

// amdFamilies maps the PCI device ids of the amdgpu devices pkg/governor has
// a mapping table for to their family (pci.ids, vendor 1002). Any other
// device publishes its level and clock ranges with no preset.
var amdFamilies = map[string]string{
	"0x7448": governor.AMDFamilyNavi31,    // Radeon Pro W7900
	"0x744c": governor.AMDFamilyNavi31,    // Radeon RX 7900 XT/XTX
	"0x745e": governor.AMDFamilyNavi31,    // Radeon Pro W7800
	"0x7408": governor.AMDFamilyAldebaran, // Instinct MI250X
	"0x740c": governor.AMDFamilyAldebaran, // Instinct MI250X/MI250
	"0x740f": governor.AMDFamilyAldebaran, // Instinct MI210
	"0x74a0": governor.AMDFamilyMI300,     // Instinct MI300A
	"0x74a1": governor.AMDFamilyMI300,     // Instinct MI300X
	"0x74a5": governor.AMDFamilyMI300,     // Instinct MI325X
}

// dpmRangeMHz is the lowest and highest numbered level of a pp_dpm_* table,
// lines such as "1: 1500Mhz *". The deep-sleep line "S: <MHz>" is a sampled
// clock, not a level, and is skipped. Zero when the file is absent.
func dpmRangeMHz(path string) [2]int {
	s, ok := readSysfsWord(path)
	if !ok {
		return [2]int{}
	}
	var r [2]int
	for _, line := range strings.Split(s, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSpace(key)); err != nil {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		mhz, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(fields[0], "Mhz"), "MHz"))
		if err != nil || mhz <= 0 {
			continue
		}
		if r[0] == 0 || mhz < r[0] {
			r[0] = mhz
		}
		if mhz > r[1] {
			r[1] = mhz
		}
	}
	return r
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

// discoverAMDGovernor reads every amdgpu device under bus/pci/devices: AMD's
// vendor id, a performance level file (only amdgpu creates it), and a driver
// link naming amdgpu where the tree has one. Devices are keyed by the bus id
// sysfs names them by, never by a tool's index: rocm-smi and HIP enumerate
// in opposite orders.
func discoverAMDGovernor() *gpuGovernorDiscovery {
	entries, err := os.ReadDir(hostSysPath(pciDevicesRelDir))
	if err != nil {
		return nil
	}
	d := &gpuGovernorDiscovery{}
	var ranges v1alpha1.GPUClockRanges
	var files, levels []string
	for _, e := range entries {
		dev := hostSysPath(pciDevicesRelDir, e.Name())
		if vendor, _ := readSysfsWord(filepath.Join(dev, "vendor")); vendor != "0x1002" {
			continue
		}
		levelFile := filepath.Join(dev, "power_dpm_force_performance_level")
		level, ok := readSysfsWord(levelFile)
		if !ok || level == "" {
			continue
		}
		if target, err := os.Readlink(filepath.Join(dev, "driver")); err == nil && filepath.Base(target) != "amdgpu" {
			continue
		}
		deviceID, _ := readSysfsWord(filepath.Join(dev, "device"))
		deviceID = strings.ToLower(deviceID)
		odFile := filepath.Join(dev, "pp_od_clk_voltage")
		f := governor.AMDFacts{
			BusID:    strings.ToLower(e.Name()),
			DeviceID: deviceID,
			Family:   amdFamilies[deviceID],
			Level:    level,
			OD:       fileExists(odFile),
			SclkMHz:  dpmRangeMHz(filepath.Join(dev, "pp_dpm_sclk")),
			MclkMHz:  dpmRangeMHz(filepath.Join(dev, "pp_dpm_mclk")),
			FclkMHz:  dpmRangeMHz(filepath.Join(dev, "pp_dpm_fclk")),
		}
		d.AMD = append(d.AMD, f)
		d.Devices = append(d.Devices, v1alpha1.GPUGovernorDeviceState{BusID: f.BusID, Level: level})
		levels = append(levels, level)
		files = append(files, levelFile)
		if f.OD {
			files = append(files, odFile)
		}
		ranges.Graphics = widenRange(ranges.Graphics, float64(f.SclkMHz[0]), float64(f.SclkMHz[1]))
		ranges.Memory = widenRange(ranges.Memory, float64(f.MclkMHz[0]), float64(f.MclkMHz[1]))
		ranges.Fabric = widenRange(ranges.Fabric, float64(f.FclkMHz[0]), float64(f.FclkMHz[1]))
	}
	if len(d.AMD) == 0 {
		return nil
	}
	current := uniformOrMixed(levels)
	d.Inventory = v1alpha1.GPUGovernorInventory{
		Mechanism: joulie.GovernorBackendAMDGPUSysfs,
		Current:   current,
		Presets: gpuPresetSupport(len(d.AMD), func(i int, p string) (governor.Plan, error) {
			return governor.PlanAMD(d.AMD[i], p, governor.DefaultCeilingPctOfMax)
		}),
		ClockRangesMHz: gpuClockRanges(ranges),
		Writable:       writable(files),
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

// nvidiaArch is the pkg/governor architecture of the node's NVIDIA GPUs,
// empty when it has no mapping table. The compute capability labels of GPU
// feature discovery come first: its gpu.family label names compute 8.9 (Ada)
// ampere on some releases. The product name comes next, the family label
// last.
func nvidiaArch(nodeLabels map[string]string, product string) string {
	if major, err := strconv.Atoi(nodeLabels["nvidia.com/gpu.compute.major"]); err == nil {
		minor, _ := strconv.Atoi(nodeLabels["nvidia.com/gpu.compute.minor"])
		switch {
		case major == 8 && minor == 9:
			return governor.NVArchAda
		case major == 8:
			return governor.NVArchAmpere
		case major == 9:
			return governor.NVArchHopper
		case major == 10 || major == 12:
			return governor.NVArchBlackwell
		}
		return ""
	}
	for _, tok := range strings.FieldsFunc(strings.ToUpper(product), func(r rune) bool { return r == ' ' || r == '-' }) {
		switch tok {
		case "A100", "A800", "A40", "A30", "A10", "A16", "A2":
			return governor.NVArchAmpere
		case "L4", "L40", "L40S", "L20", "ADA":
			return governor.NVArchAda
		case "H100", "H200", "H800", "H20", "GH200":
			return governor.NVArchHopper
		case "B100", "B200", "B300", "GB200", "GB300", "BLACKWELL":
			return governor.NVArchBlackwell
		}
	}
	switch strings.ToLower(nodeLabels["nvidia.com/gpu.family"]) {
	case "ampere":
		return governor.NVArchAmpere
	case "ada", "ada-lovelace":
		return governor.NVArchAda
	case "hopper":
		return governor.NVArchHopper
	case "blackwell":
		return governor.NVArchBlackwell
	}
	return ""
}

// nvidiaSupported is one GPU's SUPPORTED_CLOCKS: graphics and memory in MHz.
type nvidiaSupported struct {
	sm, mem []int
}

// parseNvidiaMHz reads "1980 MHz" (or a bare number) as MHz.
func parseNvidiaMHz(s string) (int, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return int(v), true
}

// parseNvidiaSupportedClocks reads nvidia-smi -q -d SUPPORTED_CLOCKS: a
// "GPU <bus id>" header per GPU, then "Memory : <MHz> MHz" lines, each
// followed by its "Graphics : <MHz> MHz" lines. A GPU whose table is N/A has
// no clocks. Keys are bus ids in lower case.
func parseNvidiaSupportedClocks(out string) map[string]*nvidiaSupported {
	res := map[string]*nvidiaSupported{}
	var cur *nvidiaSupported
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(t, "GPU "); ok && strings.Contains(rest, ":") && !strings.ContainsAny(strings.TrimSpace(rest), " \t") {
			cur = &nvidiaSupported{}
			res[strings.ToLower(strings.TrimSpace(rest))] = cur
			continue
		}
		key, value, ok := strings.Cut(t, ":")
		if !ok || cur == nil {
			continue
		}
		mhz, ok := parseNvidiaMHz(value)
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Memory":
			cur.mem = append(cur.mem, mhz)
		case "Graphics":
			cur.sm = append(cur.sm, mhz)
		}
	}
	return res
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
func discoverNvidiaGovernor(ctx context.Context, nodeLabels map[string]string, product string) (*gpuGovernorDiscovery, string) {
	arch := nvidiaArch(nodeLabels, product)
	if arch == "" {
		return nil, ""
	}
	out, err := runCommand(ctx, "nvidia-smi", nvidiaClockQueryArgs...)
	if err != nil {
		logGovernorWarningOnce(warnNvidiaClockQuery, err)
		return nil, warnNvidiaClockQuery
	}
	type row struct {
		bus           string
		maxSM, maxMem float64
	}
	var rows []row
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := splitCSVLine(ln)
		if len(parts) < 5 || strings.TrimSpace(parts[0]) == "" {
			continue
		}
		r := row{bus: strings.TrimSpace(parts[0])}
		if v, ok := parseNvidiaMHz(parts[3]); ok {
			r.maxSM = float64(v)
		}
		if v, ok := parseNvidiaMHz(parts[4]); ok {
			r.maxMem = float64(v)
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		logGovernorWarningOnce(warnNvidiaClockQuery, nil)
		return nil, warnNvidiaClockQuery
	}
	sup, err := runCommand(ctx, "nvidia-smi", nvidiaSupportedClocksArgs...)
	if err != nil {
		logGovernorWarningOnce(warnNvidiaSupportedClocks, err)
		return nil, warnNvidiaSupportedClocks
	}
	supported := parseNvidiaSupportedClocks(string(sup))

	d := &gpuGovernorDiscovery{}
	var ranges v1alpha1.GPUClockRanges
	for _, r := range rows {
		s := supported[strings.ToLower(r.bus)]
		if s == nil {
			s = &nvidiaSupported{}
		}
		d.NVIDIA = append(d.NVIDIA, nvidiaGovernorFacts{BusID: r.bus, Facts: governor.NVFacts{
			Arch:            arch,
			SupportedSMMHz:  s.sm,
			SupportedMemMHz: s.mem,
			// -lmc "does not work on GPUs based on NVIDIA Hopper".
			MemLockRuntime: arch != governor.NVArchHopper,
		}})
		d.Devices = append(d.Devices, v1alpha1.GPUGovernorDeviceState{BusID: r.bus, Level: joulie.GovernorStateUnknown})
		smLo, smHi := minMaxMHz(s.sm)
		memLo, memHi := minMaxMHz(s.mem)
		ranges.Graphics = widenRange(ranges.Graphics, smLo, max(smHi, r.maxSM))
		ranges.Memory = widenRange(ranges.Memory, memLo, max(memHi, r.maxMem))
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

// storedGovernorBaselines reads the baselines a NodeHardware object already
// holds, through the typed status. Either is nil when absent.
func storedGovernorBaselines(obj *unstructured.Unstructured) (*v1alpha1.CPUGovernorBaseline, *v1alpha1.GPUGovernorBaseline) {
	if obj == nil {
		return nil, nil
	}
	var nh v1alpha1.NodeHardware
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &nh); err != nil {
		return nil, nil
	}
	var cpu *v1alpha1.CPUGovernorBaseline
	var gpu *v1alpha1.GPUGovernorBaseline
	if c := nh.Status.CPU; c != nil && c.Governor != nil && c.Governor.Baseline != nil && len(c.Governor.Baseline.Groups) > 0 {
		cpu = c.Governor.Baseline
	}
	if g := nh.Status.GPU; g != nil && g.Governor != nil && g.Governor.Baseline != nil && len(g.Governor.Baseline.Devices) > 0 {
		gpu = g.Governor.Baseline
	}
	return cpu, gpu
}

// addGovernorStatus adds the cpufreq landmarks, the governor blocks and the
// GPU governor warnings of hw to the NodeHardware status map being built.
// The baseline is the one obj already stores when it has one, else the one
// the governor control holds: the agent records it once, at the first
// discovery and so before any write, and never records over it a state it
// may have set itself. A GPU discovery that comes back empty for one
// reconcile (a tool that did not answer) keeps the stored baseline in a
// block of its own, so the next discovery does not record a new one.
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
		inv.Baseline = storedCPU
		if inv.Baseline == nil {
			inv.Baseline = hw.Governors.CPUBaseline
		}
		if inv.Baseline == nil {
			inv.Baseline = c.baseline()
		}
		if err := put(cpuMap, "governor", &inv); err != nil {
			return err
		}
	}
	if gpuMap == nil {
		return nil
	}
	if g := hw.Governors.GPU; g != nil {
		inv := g.Inventory
		inv.Baseline = storedGPU
		if inv.Baseline == nil {
			inv.Baseline = hw.Governors.GPUBaseline
		}
		if inv.Baseline == nil {
			inv.Baseline = &v1alpha1.GPUGovernorBaseline{Devices: g.Devices}
		}
		if err := put(gpuMap, "governor", &inv); err != nil {
			return err
		}
	} else if kept := cmp.Or(storedGPU, hw.Governors.GPUBaseline); kept != nil {
		if err := put(gpuMap, "governor", &v1alpha1.GPUGovernorInventory{Baseline: kept}); err != nil {
			return err
		}
	}
	if len(hw.Governors.GPUWarnings) > 0 {
		warnings, _ := gpuMap["warnings"].([]any)
		gpuMap["warnings"] = append(warnings, stringAnySlice(hw.Governors.GPUWarnings)...)
	}
	return nil
}
