package api

// Governor presets are vendor-neutral words. Only pkg/governor turns them
// into native governor, energy-performance preference, performance level or
// clock lock settings, for the agent and the simulator alike; every other
// component speaks presets. The CRD enums must accept every value listed
// here; tests/contracts enforces it.

// Values of NodeTwin.spec.cpu.governor.preset.
const (
	// CPUGovernorPresetPerformance holds the highest frequency the policy
	// allows at any load (race to idle).
	CPUGovernorPresetPerformance = "performance"
	// CPUGovernorPresetBalanced is demand-driven with a quick ramp.
	CPUGovernorPresetBalanced = "balanced"
	// CPUGovernorPresetEfficiency is demand-driven with an energy bias.
	CPUGovernorPresetEfficiency = "efficiency"
)

// CPUGovernorPresetValues lists every value of spec.cpu.governor.preset.
var CPUGovernorPresetValues = []string{
	CPUGovernorPresetPerformance, CPUGovernorPresetBalanced, CPUGovernorPresetEfficiency,
}

// Values of NodeTwin.spec.cpu.governor.boost and of the boost state in
// NodeHardware.status.cpu.governor.boost.state and its baseline.
const (
	CPUGovernorBoostOn  = "on"
	CPUGovernorBoostOff = "off"
)

// CPUGovernorBoostValues lists every value of spec.cpu.governor.boost.
var CPUGovernorBoostValues = []string{CPUGovernorBoostOn, CPUGovernorBoostOff}

// Values of NodeTwin.spec.gpu.governor.preset.
const (
	// GPUGovernorPresetAuto is the vendor default, and the restore target.
	GPUGovernorPresetAuto = "auto"
	// GPUGovernorPresetSustained holds the data-path clocks at their
	// maximum and keeps idle gating; the compute clock stays free to idle.
	GPUGovernorPresetSustained = "sustained"
	// GPUGovernorPresetEfficient puts the compute clock under a ceiling,
	// spec.gpu.governor.computeClockCeilingPctOfMax of its maximum.
	GPUGovernorPresetEfficient = "efficient"
	// GPUGovernorPresetLow holds every clock at its minimum. Experiments
	// only.
	GPUGovernorPresetLow = "low"
	// GPUGovernorPresetPeak holds every clock at its maximum and turns idle
	// gating off. Experiments only: the built-in policy never selects it.
	GPUGovernorPresetPeak = "peak"
)

// GPUGovernorPresetValues lists every value of spec.gpu.governor.preset.
var GPUGovernorPresetValues = []string{
	GPUGovernorPresetAuto, GPUGovernorPresetSustained, GPUGovernorPresetEfficient,
	GPUGovernorPresetLow, GPUGovernorPresetPeak,
}

// Values of status.controlStatus.cpuGovernor.result and gpuGovernor.result.
// ControlResult is shared with the cap results, so the CRD has no enum here;
// the agent writes only these.
const (
	// GovernorResultApplied: written and read back equal.
	GovernorResultApplied = "applied"
	// GovernorResultAppliedUnverified: written, but the setting cannot be
	// read back (an NVIDIA clock lock) and no loaded sample confirmed it yet.
	GovernorResultAppliedUnverified = "applied-unverified"
	// GovernorResultEquivalent: the preset maps to the state the node is
	// already in; the native word is in applied.
	GovernorResultEquivalent = "equivalent"
	// GovernorResultUnsupported: the family cannot express the preset, or
	// the driver rejected the value (EINVAL, EOPNOTSUPP, EBUSY).
	GovernorResultUnsupported = "unsupported"
	// GovernorResultNotApplied: the write succeeded but the readback
	// differs, as with a silent no-op.
	GovernorResultNotApplied = "not-applied"
	// GovernorResultRefused: no permission (EACCES, EPERM, EROFS).
	GovernorResultRefused = "refused"
	// GovernorResultDrift: something else changed the setting after the
	// agent wrote it.
	GovernorResultDrift = "drift"
	// GovernorResultContended: repeated drift; the agent stopped writing the
	// component.
	GovernorResultContended = "contended"
	// GovernorResultDisabled: governor writes are turned off on the agent.
	GovernorResultDisabled = "disabled"
	// GovernorResultRestored: the agent put the baseline back.
	GovernorResultRestored = "restored"
	// GovernorResultError: the mechanism could not run, such as a missing
	// tool.
	GovernorResultError = "error"
)

// GovernorResultValues lists every value of a governor result.
var GovernorResultValues = []string{
	GovernorResultApplied, GovernorResultAppliedUnverified, GovernorResultEquivalent,
	GovernorResultUnsupported, GovernorResultNotApplied, GovernorResultRefused,
	GovernorResultDrift, GovernorResultContended, GovernorResultDisabled,
	GovernorResultRestored, GovernorResultError,
}

// Values of the result of a preset in NodeHardware.status.{cpu,gpu}.governor.presets:
// a preset the node cannot express is left out of the list.
var GovernorPresetSupportResultValues = []string{GovernorResultApplied, GovernorResultEquivalent}

// Backends of a governor result, and the mechanism of
// NodeHardware.status.gpu.governor.mechanism.
const (
	// GovernorBackendCPUFreq writes the cpufreq sysfs attributes.
	GovernorBackendCPUFreq = "cpufreq"
	// GovernorBackendAMDGPUSysfs writes the amdgpu sysfs attributes as root.
	GovernorBackendAMDGPUSysfs = "amdgpu-sysfs"
	// GovernorBackendNvidiaSMI runs nvidia-smi -i <pci bus id>.
	GovernorBackendNvidiaSMI = "nvidia-smi"
)

// GovernorBackendValues lists every governor backend.
var GovernorBackendValues = []string{GovernorBackendCPUFreq, GovernorBackendAMDGPUSysfs, GovernorBackendNvidiaSMI}

// GPUGovernorMechanismValues lists every value of
// NodeHardware.status.gpu.governor.mechanism.
var GPUGovernorMechanismValues = []string{GovernorBackendAMDGPUSysfs, GovernorBackendNvidiaSMI}

// Values of NodeHardware.status.cpu.governor.driverMode: the intel_pstate
// and amd_pstate status words.
const (
	CPUDriverModeActive  = "active"
	CPUDriverModePassive = "passive"
	CPUDriverModeGuided  = "guided"
)

// CPUDriverModeValues lists every value of cpu.governor.driverMode.
var CPUDriverModeValues = []string{CPUDriverModeActive, CPUDriverModePassive, CPUDriverModeGuided}

// Values of NodeHardware.status.cpu.governor.boost.scope: where boost is
// set.
const (
	// CPUBoostScopeGlobal is /sys/devices/system/cpu/cpufreq/boost.
	CPUBoostScopeGlobal = "global"
	// CPUBoostScopePerCPU is each policy's boost (amd-pstate).
	CPUBoostScopePerCPU = "per-cpu"
	// CPUBoostScopeNoTurbo is the inverted intel_pstate/no_turbo.
	CPUBoostScopeNoTurbo = "no-turbo"
)

// CPUBoostScopeValues lists every value of cpu.governor.boost.scope.
var CPUBoostScopeValues = []string{CPUBoostScopeGlobal, CPUBoostScopePerCPU, CPUBoostScopeNoTurbo}

// Special values of the current state and the baseline.
const (
	// GovernorCurrentMixed is cpu.governor.current when the policies of a
	// node run different governors.
	GovernorCurrentMixed = "mixed"
	// GovernorStateUnknown is the NVIDIA current state and baseline level
	// without an agent record: a clock lock has no getter.
	GovernorStateUnknown = "unknown"
	// GovernorStateUnlocked is the NVIDIA baseline level when the agent
	// knows no lock is set.
	GovernorStateUnlocked = "unlocked"
)

// CPUGovernorIntent mirrors api/v1alpha1.CPUGovernorIntent.
type CPUGovernorIntent struct {
	Preset string `json:"preset"`
	Boost  string `json:"boost,omitempty"`
}

// GPUGovernorIntent mirrors api/v1alpha1.GPUGovernorIntent.
type GPUGovernorIntent struct {
	Preset                      string   `json:"preset"`
	ComputeClockCeilingPctOfMax *float64 `json:"computeClockCeilingPctOfMax,omitempty"`
}
