package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// The discovery here reads an emulated tree the way the agent reads a host
// (cmd/agent/governor_discovery.go), so a plan the sweep writes is the plan
// the agent would write on that node. It reads only what pkg/governor needs.

const (
	cpufreqRel     = "devices/system/cpu/cpufreq"
	intelPstateRel = "devices/system/cpu/intel_pstate"
	amdPstateRel   = "devices/system/cpu/amd_pstate"
	pciDevicesRel  = "bus/pci/devices"
)

func readWord(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// cpuFacts is the agent's CPU discovery over sys, the tree's sys/, and
// cpuinfo, its /proc/cpuinfo. A tree without cpufreq policies gives the
// zero facts, which plan every preset as unsupported.
func cpuFacts(sys, cpuinfo string) governor.CPUFacts {
	matches, _ := filepath.Glob(filepath.Join(sys, cpufreqRel, "policy*"))
	var ids []int
	for _, m := range matches {
		if n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(m), "policy")); err == nil && exists(filepath.Join(m, "scaling_governor")) {
			ids = append(ids, n)
		}
	}
	if len(ids) == 0 {
		return governor.CPUFacts{}
	}
	slices.Sort(ids)
	pol := func(id int, name string) string {
		return filepath.Join(sys, cpufreqRel, "policy"+strconv.Itoa(id), name)
	}
	first := ids[0]
	driver := readWord(pol(first, "scaling_driver"))
	f := governor.CPUFacts{
		Driver:    driver,
		Governors: strings.Fields(readWord(pol(first, "scaling_available_governors"))),
		Policies:  ids,
	}
	intel := driver == "intel_pstate" || driver == "intel_cpufreq"
	var status string
	switch {
	case intel:
		status = readWord(filepath.Join(sys, intelPstateRel, "status"))
	case strings.HasPrefix(driver, "amd-pstate"):
		status = readWord(filepath.Join(sys, amdPstateRel, "status"))
	}
	if slices.Contains(joulie.CPUDriverModeValues, status) {
		f.DriverMode = status
	}
	for _, line := range strings.Split(readWord(cpuinfo), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "flags" {
			f.HWP = slices.Contains(strings.Fields(v), "hwp")
			break
		}
	}
	hasEPP := exists(pol(first, "energy_performance_preference"))
	f.HWP = f.HWP || (intel && hasEPP)
	if hasEPP {
		f.EPPs = governor.EPPSupport(driver, readWord(pol(first, "scaling_governor")),
			strings.Fields(readWord(pol(first, "energy_performance_available_preferences"))))
	}
	perCPU := exists(pol(first, "boost"))
	switch {
	case intel && exists(filepath.Join(sys, intelPstateRel, "no_turbo")):
		f.BoostScope = joulie.CPUBoostScopeNoTurbo
	case strings.HasPrefix(driver, "amd-pstate") && perCPU:
		f.BoostScope = joulie.CPUBoostScopePerCPU
	case exists(filepath.Join(sys, cpufreqRel, "boost")):
		f.BoostScope = joulie.CPUBoostScopeGlobal
	case perCPU:
		f.BoostScope = joulie.CPUBoostScopePerCPU
	}
	return f
}

// amdFamilies maps the amdgpu device ids of the builtin profiles' GPUs to
// their pkg/governor family, as the agent's table does (pci.ids, vendor
// 1002). A device not listed has no family, and every preset is unsupported
// on it.
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

// amdFacts is the agent's amdgpu discovery over sys: every AMD device with
// a performance level file, keyed by bus id, sorted.
func amdFacts(sys string) []governor.AMDFacts {
	entries, err := os.ReadDir(filepath.Join(sys, pciDevicesRel))
	if err != nil {
		return nil
	}
	var out []governor.AMDFacts
	for _, e := range entries {
		dev := filepath.Join(sys, pciDevicesRel, e.Name())
		if readWord(filepath.Join(dev, "vendor")) != "0x1002" {
			continue
		}
		level := readWord(filepath.Join(dev, "power_dpm_force_performance_level"))
		if level == "" {
			continue
		}
		id := strings.ToLower(readWord(filepath.Join(dev, "device")))
		out = append(out, governor.AMDFacts{
			BusID:    strings.ToLower(e.Name()),
			DeviceID: id,
			Family:   amdFamilies[id],
			Level:    level,
			OD:       exists(filepath.Join(dev, "pp_od_clk_voltage")),
			SclkMHz:  dpmRange(filepath.Join(dev, "pp_dpm_sclk")),
			MclkMHz:  dpmRange(filepath.Join(dev, "pp_dpm_mclk")),
			FclkMHz:  dpmRange(filepath.Join(dev, "pp_dpm_fclk")),
		})
	}
	slices.SortFunc(out, func(a, b governor.AMDFacts) int { return strings.Compare(a.BusID, b.BusID) })
	return out
}

// dpmRange is the lowest and highest numbered level of a pp_dpm_* table;
// the deep-sleep line "S: <MHz>" is a sampled clock and is skipped.
func dpmRange(path string) [2]int {
	var r [2]int
	for _, line := range strings.Split(readWord(path), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSpace(k)); err != nil {
			continue
		}
		fields := strings.Fields(v)
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
		r[1] = max(r[1], mhz)
	}
	return r
}

// nvGPU is one NVIDIA GPU: its bus id, as nvidia-smi -i takes it, and its
// facts.
type nvGPU struct {
	BusID string
	Facts governor.NVFacts
}

// nvFacts asks the node's fake nvidia-smi for the bus ids and the supported
// clocks, as the agent does, with the architecture from the profile (the
// agent reads it from the node's labels or product name). A node without
// nvidia-smi, or an architecture pkg/governor has no table for, gives none.
func nvFacts(ctx context.Context, r smi.Runner, p *hwemu.Profile) ([]nvGPU, error) {
	if p.GPUs == nil || p.GPUs.Vendor != "nvidia" {
		return nil, nil
	}
	arch := strings.ToLower(p.GPUs.Architecture.V)
	if !slices.Contains([]string{governor.NVArchAmpere, governor.NVArchAda, governor.NVArchHopper, governor.NVArchBlackwell}, arch) {
		return nil, nil
	}
	out, err := r.Run(ctx, "nvidia-smi", "--query-gpu=pci.bus_id", "--format=csv,noheader")
	if err != nil {
		return nil, err
	}
	sup, err := r.Run(ctx, "nvidia-smi", "-q", "-d", "SUPPORTED_CLOCKS")
	if err != nil {
		return nil, err
	}
	clocks := supportedClocks(string(sup))
	var gpus []nvGPU
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		bus := strings.TrimSpace(ln)
		if bus == "" {
			continue
		}
		s := clocks[strings.ToLower(bus)]
		gpus = append(gpus, nvGPU{BusID: bus, Facts: governor.NVFacts{
			Arch: arch, SupportedSMMHz: s[0], SupportedMemMHz: s[1],
			MemLockRuntime: arch != governor.NVArchHopper,
		}})
	}
	return gpus, nil
}

// supportedClocks reads nvidia-smi -q -d SUPPORTED_CLOCKS: a "GPU <bus id>"
// header per GPU, then Memory lines, each followed by its Graphics lines.
// Each value is [graphics MHz, memory MHz], keyed by bus id in lower case.
func supportedClocks(out string) map[string][2][]int {
	res := map[string][2][]int{}
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(t, "GPU "); ok && strings.Contains(rest, ":") && !strings.ContainsAny(strings.TrimSpace(rest), " \t") {
			cur = strings.ToLower(strings.TrimSpace(rest))
			res[cur] = [2][]int{}
			continue
		}
		k, v, ok := strings.Cut(t, ":")
		if !ok || cur == "" {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		mhz, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || mhz <= 0 {
			continue
		}
		e := res[cur]
		switch strings.TrimSpace(k) {
		case "Graphics":
			e[0] = append(e[0], int(mhz))
		case "Memory":
			e[1] = append(e[1], int(mhz))
		}
		res[cur] = e
	}
	return res
}
