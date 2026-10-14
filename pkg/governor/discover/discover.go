// Package discover reads the facts pkg/governor plans with from a host: the
// cpufreq policies and the amdgpu devices under a sysfs root, and the NVIDIA
// GPUs through nvidia-smi. The agent reads its node with it and the
// simulator's governor sweep reads an emulated tree with it, so a plan the
// sweep measures is the plan the agent writes on that node. It only reads,
// except for WriteAttr, the one way both write an attribute.
package discover

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
)

// Files, words and flags only discovery reads.
const (
	policyDriverFile        = "scaling_driver"
	policyAvailableGovsFile = "scaling_available_governors"
	policyAvailableEPPsFile = "energy_performance_available_preferences"
	policyMinFreqFile       = "cpuinfo_min_freq"
	policyMaxFreqFile       = "cpuinfo_max_freq"
	policyBaseFreqFile      = "base_frequency"
	pstateStatusFile        = "status"
	amdVendorID             = "0x1002"
	amdgpuDriver            = "amdgpu"
	pciVendorFile           = "vendor"
	pciDeviceFile           = "device"
	pciDriverLink           = "driver"
	amdDPMSclkFile          = "pp_dpm_sclk"
	amdDPMMclkFile          = "pp_dpm_mclk"
	amdDPMFclkFile          = "pp_dpm_fclk"
	cpuinfoFlagsKey         = "flags"
	cpuinfoHWPFlag          = "hwp"
	accessWriteOK           = 0x2 // W_OK of access(2); syscall has no name for it
)

// ReadWord is the trimmed content of a sysfs attribute; ok is false when the
// file cannot be read.
func ReadWord(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// word is ReadWord without the ok.
func word(path string) string {
	s, _ := ReadWord(path)
	return s
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// WriteAttr writes value to a sysfs attribute in place: open without
// create, truncate, write. A missing attribute is an error rather than a new
// file, and the attribute is never replaced, which an emulated tree, read
// back in place, would not see.
func WriteAttr(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(value); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Writable reports whether every path passes an access(2) W_OK probe, which
// writes nothing. It catches a read-only /sys mount and a non-root reader;
// root passes it for every file, so a refusal of one attribute shows only
// when it is written. No path is not writable.
func Writable(paths []string) bool {
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

// PolicyIDs lists the numbers N of the cpufreq policyN directories under
// sys, sorted.
func PolicyIDs(sys string) []int {
	matches, _ := filepath.Glob(filepath.Join(sys, governor.CPUFreqRelDir, "policy*"))
	var ids []int
	for _, m := range matches {
		if n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(m), "policy")); err == nil {
			ids = append(ids, n)
		}
	}
	slices.Sort(ids)
	return ids
}

// Policy is the governor state of one cpufreq policy. EPP and Boost (a
// pkg/api CPUGovernorBoost* word) are empty where the policy has none.
type Policy struct {
	ID                   int
	Governor, EPP, Boost string
}

// CPU is the cpufreq governor state of a node.
type CPU struct {
	// Facts is what pkg/governor plans with.
	Facts governor.CPUFacts
	// Policies is the state of every policy in Facts.Policies, in order.
	Policies []Policy
	// HasEPP is true when the policies have an energy_performance_preference
	// file.
	HasEPP bool
	// Files are the attributes a governor write touches, under sys.
	Files []string
}

// IsIntelPstate reports whether driver is intel_pstate in either mode.
func IsIntelPstate(driver string) bool {
	return driver == "intel_pstate" || driver == "intel_cpufreq"
}

// hasHWP reports whether the cpuinfo file lists the hwp flag (hardware
// P-states).
func hasHWP(cpuinfo string) bool {
	b, err := os.ReadFile(cpuinfo)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == cpuinfoFlagsKey {
			return slices.Contains(strings.Fields(value), cpuinfoHWPFlag)
		}
	}
	return false
}

// driverMode is the intel_pstate or amd_pstate status word of driver, or
// empty for a driver without modes or a word Joulie does not know.
func driverMode(sys, driver string) string {
	var status string
	switch {
	case IsIntelPstate(driver):
		status = word(filepath.Join(sys, governor.IntelPstateRelDir, pstateStatusFile))
	case strings.HasPrefix(driver, "amd-pstate"):
		status = word(filepath.Join(sys, governor.AMDPstateRelDir, pstateStatusFile))
	}
	if slices.Contains(joulie.CPUDriverModeValues, status) {
		return status
	}
	return ""
}

// boostScope is where boost is set on driver: the inverted no_turbo on
// intel_pstate, each policy's boost on amd-pstate, the global cpufreq/boost
// elsewhere; empty without a boost file.
func boostScope(sys, driver string, firstPolicy int) string {
	perCPU := exists(filepath.Join(sys, governor.PolicyRelPath(firstPolicy, governor.PolicyBoostFile)))
	switch {
	case IsIntelPstate(driver) && exists(filepath.Join(sys, governor.NoTurboRelPath)):
		return joulie.CPUBoostScopeNoTurbo
	case strings.HasPrefix(driver, "amd-pstate") && perCPU:
		return joulie.CPUBoostScopePerCPU
	case exists(filepath.Join(sys, governor.GlobalBoostRelPath)):
		return joulie.CPUBoostScopeGlobal
	case perCPU:
		return joulie.CPUBoostScopePerCPU
	}
	return ""
}

// boostWord maps a boost attribute (1 on, or 1 off when inverted) to the
// pkg/api boost word; empty for any other content.
func boostWord(v string, inverted bool) string {
	if v != "0" && v != "1" {
		return ""
	}
	if on := v == "1"; on != inverted {
		return joulie.CPUGovernorBoostOn
	}
	return joulie.CPUGovernorBoostOff
}

// ReadCPU reads the cpufreq governor state of every policy under sys with a
// scaling_governor file; cpuinfo is the node's /proc/cpuinfo, for the hwp
// flag. It is nil when no policy has one, as on a VM without cpufreq or a
// capture that recorded the driver only.
func ReadCPU(sys, cpuinfo string) *CPU {
	pol := func(id int, file string) string { return filepath.Join(sys, governor.PolicyRelPath(id, file)) }
	var states []Policy
	for _, id := range PolicyIDs(sys) {
		if gov := word(pol(id, governor.PolicyGovernorFile)); gov != "" {
			states = append(states, Policy{ID: id, Governor: gov})
		}
	}
	if len(states) == 0 {
		return nil
	}
	first := states[0].ID
	driver := word(pol(first, policyDriverFile))
	f := governor.CPUFacts{
		Driver:     driver,
		DriverMode: driverMode(sys, driver),
		HWP:        hasHWP(cpuinfo),
		Governors:  strings.Fields(word(pol(first, policyAvailableGovsFile))),
	}
	hasEPP := exists(pol(first, governor.PolicyEPPFile))
	// intel_pstate creates energy_performance_preference only with hardware
	// P-states (hwp_cpufreq_attrs), so the file says HWP is on where cpuinfo
	// lists no hwp flag.
	f.HWP = f.HWP || (IsIntelPstate(driver) && hasEPP)
	if hasEPP {
		f.EPPs = governor.EPPSupport(driver, states[0].Governor, strings.Fields(word(pol(first, policyAvailableEPPsFile))))
	}
	f.BoostScope = boostScope(sys, driver, first)

	var globalBoost string
	switch f.BoostScope {
	case joulie.CPUBoostScopeGlobal:
		globalBoost = boostWord(word(filepath.Join(sys, governor.GlobalBoostRelPath)), false)
	case joulie.CPUBoostScopeNoTurbo:
		globalBoost = boostWord(word(filepath.Join(sys, governor.NoTurboRelPath)), true)
	}
	c := &CPU{HasEPP: hasEPP}
	for _, s := range states {
		f.Policies = append(f.Policies, s.ID)
		c.Files = append(c.Files, pol(s.ID, governor.PolicyGovernorFile))
		if hasEPP {
			s.EPP = word(pol(s.ID, governor.PolicyEPPFile))
			c.Files = append(c.Files, pol(s.ID, governor.PolicyEPPFile))
		}
		s.Boost = globalBoost
		if f.BoostScope == joulie.CPUBoostScopePerCPU {
			s.Boost = boostWord(word(pol(s.ID, governor.PolicyBoostFile)), false)
			c.Files = append(c.Files, pol(s.ID, governor.PolicyBoostFile))
		}
		c.Policies = append(c.Policies, s)
	}
	switch f.BoostScope {
	case joulie.CPUBoostScopeGlobal:
		c.Files = append(c.Files, filepath.Join(sys, governor.GlobalBoostRelPath))
	case joulie.CPUBoostScopeNoTurbo:
		c.Files = append(c.Files, filepath.Join(sys, governor.NoTurboRelPath))
	}
	c.Facts = f
	return c
}

// Landmarks are the frequency landmarks of cpufreq in MHz: the lowest
// cpuinfo_min_freq, the highest cpuinfo_max_freq (the maximum boost) and the
// highest base_frequency (the nominal frequency, intel_pstate only) over the
// policies. Zero where no policy has the file.
type Landmarks struct {
	MinMHz, MaxBoostMHz, NominalMHz float64
}

// readMHz reads a frequency attribute in kHz as MHz; ok is false when the
// file is missing or holds no positive number.
func readMHz(path string) (float64, bool) {
	s, ok := ReadWord(path)
	if !ok {
		return 0, false
	}
	khz, err := strconv.ParseFloat(s, 64)
	if err != nil || khz <= 0 {
		return 0, false
	}
	return khz / 1000, true
}

// ReadLandmarks reads the cpufreq frequency landmarks under sys.
func ReadLandmarks(sys string) Landmarks {
	var lm Landmarks
	for _, id := range PolicyIDs(sys) {
		pol := func(file string) string { return filepath.Join(sys, governor.PolicyRelPath(id, file)) }
		if mhz, ok := readMHz(pol(policyMinFreqFile)); ok && (lm.MinMHz == 0 || mhz < lm.MinMHz) {
			lm.MinMHz = mhz
		}
		if mhz, ok := readMHz(pol(policyMaxFreqFile)); ok && mhz > lm.MaxBoostMHz {
			lm.MaxBoostMHz = mhz
		}
		if mhz, ok := readMHz(pol(policyBaseFreqFile)); ok && mhz > lm.NominalMHz {
			lm.NominalMHz = mhz
		}
	}
	return lm
}
