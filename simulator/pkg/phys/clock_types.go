package phys

import "fmt"

// Types of the governor and GPU clock models. The pure functions over them
// live beside them, one file per model, with these signatures:
//
//	// governor.go: the frequency a cpufreq governor requests for one policy.
//	func GovernorTargetKHz(in GovernorInput) int64
//
//	// gpuclock.go: GPU board power and the throughput multiplier of a
//	// workload class at clock c. At the maximum compute clock, the data path
//	// at its maximum and idle gating kept, both equal what m gives today.
//	func GPUPowerWithClock(m CappedBoardGPUModel, s DeviceState, c GPUClock, f GPUClockFacts) float64
//	func GPUThroughputWithClock(m CappedBoardGPUModel, s DeviceState, c GPUClock, f GPUClockFacts, workloadClass string) float64
//
//	// transfer.go: the rate, in bytes per second, a peer copy from GPU src
//	// to GPU dst gets, demanding demandBps over a link of linkBps into a
//	// receiver at clock recv. The source's state never matters.
//	func PeerRate(demandBps, linkBps float64, recv GPUClock, gate ReceiverGate, src, dst int) float64
//
// The cap and the clock act through one clock scale: a cap lowers the
// compute clock, and the clock the GPU runs at is the lower of its lock or
// ceiling and the cap-implied clock, never the product of both.

// GovernorInput is what a cpufreq governor sees for one policy at one step.
// Frequencies are in kHz, as cpufreq writes them.
type GovernorInput struct {
	// Governor is scaling_governor.
	Governor string
	// HWP is true when powersave is the driver's own algorithm (intel_pstate
	// in active mode with HWP, amd-pstate-epp), which follows EPP, rather
	// than the generic governor that sits at the minimum.
	HWP bool
	// EPP is the energy-performance preference as a number, 0 (performance)
	// to 255 (power); read with HWP only.
	EPP int
	// Util is the policy's effective utilization over the previous step,
	// 0..1: the demand scaled by how much slower than the reference the
	// policy ran, so a slower clock shows as a busier CPU.
	Util float64
	// MinKHz is the resolved scaling_min_freq; MaxKHz the highest frequency
	// the policy runs at, scaling_max_freq with boost.
	MinKHz, MaxKHz int64
	// TableKHz is scaling_available_frequencies, empty for a driver without
	// a table; a request resolves to the lowest entry at or above it.
	TableKHz []int64
	// CurKHz is the governor's previous request, from which conservative
	// steps.
	CurKHz int64
	// PeriodS is the time since the governor last sampled, in seconds.
	PeriodS float64
	// Tunables are the governor's sysfs tunables.
	Tunables GovernorTunables
	// EPPGain and EPPCeilingDrop shape the HWP powersave target: how much a
	// lower EPP raises the frequency at a given utilization, and how far EPP
	// 128 lowers the full-load frequency, as a fraction of the range.
	EPPGain, EPPCeilingDrop float64
}

// GovernorTunables are the tunables of the generic governors, under the
// names of their sysfs attributes; times in us, thresholds and the step in
// percent.
type GovernorTunables struct {
	SchedutilRateLimitUS                                                     int64
	OndemandUpThreshold, OndemandSamplingRateUS                              int64
	ConservativeFreqStep, ConservativeUpThreshold, ConservativeDownThreshold int64
}

// DataPathLevel is the level of a GPU's data-path clocks: AMD fclk, socclk
// and uclk, NVIDIA memory. The zero value is DataPathMax, so a GPU without
// a clock model moves data as it always did.
type DataPathLevel int

const (
	DataPathMax DataPathLevel = iota
	DataPathMid
	DataPathMin
)

func (d DataPathLevel) String() string {
	switch d {
	case DataPathMax:
		return "max"
	case DataPathMid:
		return "mid"
	case DataPathMin:
		return "min"
	}
	return fmt.Sprintf("DataPathLevel(%d)", int(d))
}

// GPUClock is the clock state of one GPU at one step. ComputeMHz is the
// compute clock the governor allows (a lock, a ceiling or a level), before
// the cap; ComputeMaxMHz its hardware maximum. DataPath is the data-path
// level and DataPathAuto says it follows the GPU's own demand (the auto
// level) rather than a forced setting. GatingOff is true when idle gating is
// disabled. A zero ComputeMaxMHz means no clock model: the GPU runs as the
// power model alone says.
type GPUClock struct {
	ComputeMHz, ComputeMaxMHz float64
	DataPath                  DataPathLevel
	DataPathAuto              bool
	GatingOff                 bool
}

// GPUClockFacts are the profile parameters of the GPU clock model:
// ClockBeta, the exponent of the compute clock in dynamic power;
// MemoryKneeFrac, the fraction of the maximum compute clock below which
// memory-bound work slows, by the exponent MemoryKneeGamma; DataPathHoldW,
// the idle power of a data path held at its maximum; GatingOffIdleW, the
// idle power with idle gating off; DataPathGamma, how much memory-bound work
// slows with the data path at its minimum.
type GPUClockFacts struct {
	ClockBeta       float64
	MemoryKneeFrac  float64
	MemoryKneeGamma float64
	DataPathHoldW   float64
	GatingOffIdleW  float64
	DataPathGamma   float64
}

// ReceiverGate bounds inbound peer transfers by the receiving GPU's
// data-path state. Off (the zero value), every pair gets the link. On, a
// receiver at DataPathMax, or running its own memory-active work, gets the
// link; one forced to DataPathMin gets SlowFraction of it; an idle receiver
// under auto gets MidFraction from a MidPairFraction share of its sources
// and SlowFraction from the rest, the share chosen by a hash of (src, dst)
// seeded with Seed, so the rate is a property of the pair.
type ReceiverGate struct {
	Enabled                                    bool
	SlowFraction, MidFraction, MidPairFraction float64
	Seed                                       uint64
}
