package governor

import (
	"strconv"
	"strings"
)

// Directories and files of the governor attributes, relative to the host's
// /sys (the agent's /host-sys). Plans, restores and discovery spell a path
// only through these.
const (
	// CPUFreqRelDir holds the cpufreq policies and the global boost file.
	CPUFreqRelDir = "devices/system/cpu/cpufreq"
	// IntelPstateRelDir and AMDPstateRelDir hold the driver status and,
	// for intel_pstate, no_turbo.
	IntelPstateRelDir = "devices/system/cpu/intel_pstate"
	AMDPstateRelDir   = "devices/system/cpu/amd_pstate"
	// PCIDevicesRelDir holds one directory per PCI device, named by bus id.
	PCIDevicesRelDir = "bus/pci/devices"

	// GlobalBoostRelPath is the global cpufreq boost file (1 on);
	// NoTurboRelPath is intel_pstate's inverted one (1 off).
	GlobalBoostRelPath = CPUFreqRelDir + "/boost"
	NoTurboRelPath     = IntelPstateRelDir + "/no_turbo"
)

// Attribute files of a cpufreq policy.
const (
	PolicyGovernorFile = "scaling_governor"
	PolicyEPPFile      = "energy_performance_preference"
	PolicyBoostFile    = "boost"
)

// Attribute files of an amdgpu device.
const (
	AMDLevelFile = "power_dpm_force_performance_level"
	AMDODFile    = "pp_od_clk_voltage"
)

// AMDLevelAuto is the amdgpu performance level of the vendor default.
const AMDLevelAuto = "auto"

// PolicyRelPath is the attribute file of cpufreq policy n.
func PolicyRelPath(n int, file string) string {
	return CPUFreqRelDir + "/policy" + strconv.Itoa(n) + "/" + file
}

// AMDDeviceRelPath is the attribute file of the amdgpu device at busID, the
// PCI bus id as sysfs spells it.
func AMDDeviceRelPath(busID, file string) string {
	return PCIDevicesRelDir + "/" + strings.ToLower(busID) + "/" + file
}

// IsODWrite reports whether w edits an amdgpu overdrive table. The file
// prints the table after an edit, so such a write is never read back as
// written.
func IsODWrite(w Write) bool {
	return strings.HasSuffix(w.RelPath, "/"+AMDODFile)
}
