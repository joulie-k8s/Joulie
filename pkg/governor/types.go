// Package governor maps the vendor-neutral governor presets of pkg/api to
// the native settings of each CPU frequency driver and GPU family, and back.
// It is the only place a preset becomes a native word: the agent plans its
// writes with it and the simulator names the state it emulates with it, so
// the mapping is never spelled twice.
//
// A plan is pure: it reads facts that discovery gathered and returns the
// writes or commands that would set a preset, with the result they would
// give (applied, equivalent or unsupported, the pkg/api GovernorResult*
// values). It never touches the host.
package governor

// AMD GPU families, the values of AMDFacts.Family. Each has its own mapping
// table: the levels its firmware accepts and whether overdrive is there.
const (
	// AMDFamilyNavi31 is RDNA3 (Radeon Pro W7900, SMU 13.0.0).
	AMDFamilyNavi31 = "navi31"
	// AMDFamilyAldebaran is CDNA2 (Instinct MI210, MI250, MI250X).
	AMDFamilyAldebaran = "aldebaran"
	// AMDFamilyMI300 is CDNA3 (Instinct MI300X, SMU 13.0.6).
	AMDFamilyMI300 = "mi300"
)

// NVIDIA architectures, the values of NVFacts.Arch, as nvidia-smi names
// them in lower case.
const (
	NVArchAmpere    = "ampere"
	NVArchAda       = "ada"
	NVArchHopper    = "hopper"
	NVArchBlackwell = "blackwell"
)

// DefaultCeilingPctOfMax is the compute clock ceiling of the efficient GPU
// preset, in percent of the maximum compute clock, when the intent names
// none. The agent's discovery judges the efficient preset at it, and the
// controller manager writes it unless configured otherwise.
//
// 80 is provisional. Published sweeps put the best energy per job between
// 61 and 76 % of the maximum clock, and the constant power of the rest of
// the node moves the node's optimum up; in the simulator's governor
// exploration (experiment 05, Tier A, judged per GPU model) 75 missed the
// eco p95 slowdown limit of 20 % on every model that can express it (+22.5
// to +22.7 %) and 80 passed on all of them (+17.1 to +17.6 %, energy -11 to
// -17 %). It stands until hardware measures the clock exponent and the
// memory knee the simulator assumes.
const DefaultCeilingPctOfMax = 80.0

// CPUFacts is what discovery reads about the cpufreq policies of a node, all
// of which get the same preset.
type CPUFacts struct {
	// Driver is scaling_driver, such as acpi-cpufreq, intel_cpufreq,
	// intel_pstate, amd-pstate, amd-pstate-epp or cppc_cpufreq. Empty means
	// the node has no cpufreq, as a VM often has.
	Driver string
	// DriverMode is the pkg/api CPUDriverMode* word from intel_pstate/status
	// or amd_pstate/status, empty for a driver without modes.
	DriverMode string
	// HWP is true when the CPU has hardware P-states (the hwp flag of
	// /proc/cpuinfo).
	HWP bool
	// Governors is scaling_available_governors.
	Governors []string
	// EPPs is energy_performance_available_preferences, empty when the
	// policies have no energy_performance_preference file.
	EPPs []string
	// BoostScope is the pkg/api CPUBoostScope* word for where boost is set,
	// empty when the node has no boost file.
	BoostScope string
	// Policies are the numbers N of devices/system/cpu/cpufreq/policyN.
	Policies []int
}

// AMDFacts is what discovery reads about one amdgpu device, keyed by its PCI
// bus id: rocm-smi and HIP enumerate GPUs in opposite orders, so a tool
// index never identifies one.
type AMDFacts struct {
	// BusID is the PCI address as sysfs spells it: domain:bus:device.function
	// in lower case, such as 0000:c1:00.0.
	BusID string
	// DeviceID is the PCI device id, such as 0x7448.
	DeviceID string
	// Family is one of the AMDFamily* values, from DeviceID.
	Family string
	// Level is power_dpm_force_performance_level.
	Level string
	// OD is true when pp_od_clk_voltage exists, that is when overdrive is
	// enabled.
	OD bool
	// SclkMHz, MclkMHz and FclkMHz are the lowest and highest entries of
	// pp_dpm_sclk, pp_dpm_mclk and pp_dpm_fclk; zero when the file is
	// absent.
	SclkMHz, MclkMHz, FclkMHz [2]int
}

// NVFacts is what discovery reads about one NVIDIA GPU through nvidia-smi.
type NVFacts struct {
	// Arch is one of the NVArch* values.
	Arch string
	// SupportedSMMHz and SupportedMemMHz are the graphics and memory clocks
	// of nvidia-smi -q -d SUPPORTED_CLOCKS, in any order.
	SupportedSMMHz, SupportedMemMHz []int
	// MemLockRuntime is true when -lmc takes effect at run time; it does not
	// on Hopper.
	MemLockRuntime bool
	// LockedGPU and LockedMem are the agent's own record of the locks it
	// set. A lock cannot be read back, so a reset is issued only for a lock
	// in this record, never over an operator's own.
	LockedGPU, LockedMem bool
}

// Write is one sysfs write: Value written to RelPath, a path relative to the
// host's /sys (the agent's /host-sys), such as
// devices/system/cpu/cpufreq/policy0/scaling_governor.
type Write struct {
	RelPath, Value string
}

// State is a native governor state in typed fields: what a plan sets, or
// what ParseNative reads back from its Native spelling. Only the fields of
// the plan's mechanism are set.
type State struct {
	// Governor is the cpufreq scaling_governor; EPP the
	// energy_performance_preference, empty when the plan writes none; Boost
	// the pkg/api CPUGovernorBoost* word the plan writes, empty when it
	// leaves boost alone.
	Governor, EPP, Boost string
	// Level is the amdgpu power_dpm_force_performance_level; SclkMaxMHz the
	// overdrive sclk ceiling, zero for none.
	Level      string
	SclkMaxMHz int
	// GPULockMHz and MemLockMHz are the NVIDIA graphics and memory clock
	// locks [min, max] in MHz, nil for a domain the plan does not lock.
	GPULockMHz, MemLockMHz *[2]int
}

// Plan is how to set one preset. Writes are sysfs writes, in order, each to
// be read back before the next; Args are nvidia-smi invocations, each the
// arguments after the binary name, such as
// ["-i", "00000000:18:00.0", "-lgc", "300,1500"]. State is the native state
// the plan sets, typed, for any code that acts on it.
//
// Native spells State for people (the controlStatus applied field and the
// presets NodeHardware lists), and is never parsed outside this package: the
// CPU governor, then " epp=<word>" and " boost=<0|1>" or " no_turbo=<0|1>"
// when the plan writes them, such as "powersave epp=balance_power"; the
// amdgpu performance level, then " sclk_max=<MHz>" for an overdrive
// ceiling, such as "manual sclk_max=1125"; the NVIDIA locks as nvidia-smi is
// told them, such as "-lgc 300,1100", or "unlocked" when the plan only
// resets locks, or "unknown" when it issues nothing. ParseNative reads it
// back.
//
// Result is the pkg/api GovernorResult* value the plan gives when every step
// succeeds: applied, equivalent or unsupported. Equivalent means the preset
// has no native state of its own on this hardware: it sets a fallback word
// (efficiency as schedutil when conservative is missing), the state of
// another preset (efficiency as powersave without an energy-performance
// preference), or the vendor default (sustained on Hopper). Its Writes and
// Args still bring the node to that state, and may be empty. Unsupported has
// no Writes, no Args and no State.
type Plan struct {
	Writes []Write
	Args   [][]string
	State
	Native string
	Result string
}
