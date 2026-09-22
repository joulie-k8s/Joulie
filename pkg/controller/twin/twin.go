// Package twin implements the Joulie digital twin.
//
// The digital twin is a lightweight O(1) parametric model that predicts the
// impact of scheduling and power-cap decisions on:
//   - Power headroom (available budget before the operator's power cap)
//   - Cooling stress (fraction of physical cooling capacity in use)
//   - PSU stress (fraction of rack PDU capacity in use) [reserved for future use]
//
// Headroom is computed from measured node power (provided by the operator via
// a 3-tier fallback: Kepler direct measurement, utilization-based estimation,
// or static marginal estimation) against the per-node capped power budget.
//
// Cooling stress is normalized against uncapped TDP (not the operator's cap)
// because the cooling system is sized for the hardware, not the cap. An
// ambient temperature multiplier penalizes hot environments.
//
// Runs every ~1 minute in the operator, writes NodeTwin CRs,
// read by the scheduler extender for placement decisions.
package twin

import (
	"math"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
)

// Input holds all the inputs needed to compute twin state for one node.
type Input struct {
	NodeName  string
	Hardware  joulie.NodeHardware
	Profile   string  // "eco" or "performance"
	CPUCapPct float64 // operator cap percentage for CPU (0 means 100)
	GPUCapPct float64 // operator cap percentage for GPU (0 means 100)
	Draining  bool

	// Measured power from the operator (3-tier fallback: kepler, utilization, static).
	MeasuredNodePowerW float64
	// Power trend in watts per minute (positive = rising, negative = falling).
	PowerTrendWPerMin float64

	// Facility signals
	ClusterTotalPowerW float64
	OutsideTempC       float64 // 0 if unknown
	// Topology: optional rack/cooling-zone labels from the node.
	// When set, PSU stress is computed per-rack and cooling stress uses
	// per-zone ambient temperature instead of the global facility value.
	Rack            string  // joulie.io/rack label value
	CoolingZone     string  // joulie.io/cooling-zone label value
	RackTotalPowerW float64 // sum of estimated power for all nodes in this rack; 0 = use ClusterTotalPowerW
}

// PowerMeasurementOutput holds the power measurement and budget breakdown
// written to the NodeTwin CRD status for consumption by the scheduler.
type PowerMeasurementOutput struct {
	Source             string  `json:"source,omitempty"`
	MeasuredNodePowerW float64 `json:"measuredNodePowerW,omitempty"`
	CpuCappedPowerW    float64 `json:"cpuCappedPowerW,omitempty"`
	GpuCappedPowerW    float64 `json:"gpuCappedPowerW,omitempty"`
	NodeCappedPowerW   float64 `json:"nodeCappedPowerW,omitempty"`
	CpuTdpW            float64 `json:"cpuTdpW,omitempty"`
	GpuTdpW            float64 `json:"gpuTdpW,omitempty"`
	NodeTdpW           float64 `json:"nodeTdpW,omitempty"`
	PowerTrendWPerMin  float64 `json:"powerTrendWPerMin,omitempty"`
}

// Output is the computed NodeTwinState fields.
type Output struct {
	SchedulableClass            string
	PredictedPowerHeadroomScore float64
	PredictedCoolingStressScore float64
	// PredictedPsuStressScore is reserved for future rack-topology-aware extensions.
	// Not used in scoring.
	PredictedPsuStressScore float64
	EffectiveCapState       joulie.CapState
	HardwareDensityScore    float64
	// EstimatedPUE is reserved for future extensions. Not used in scoring.
	EstimatedPUE     float64
	PowerMeasurement PowerMeasurementOutput
	LastUpdated      time.Time
}

// Compute derives NodeTwinState output from the given input.
func Compute(in Input) Output {
	out := Output{
		LastUpdated: time.Now().UTC(),
	}

	// Schedulable class
	if in.Draining {
		out.SchedulableClass = "draining"
	} else if in.Profile == "eco" {
		out.SchedulableClass = "eco"
	} else if in.Profile == "performance" {
		out.SchedulableClass = "performance"
	} else {
		out.SchedulableClass = "unknown"
	}

	// Effective cap state (default 100% if unset)
	cpuPct := in.CPUCapPct
	if cpuPct <= 0 {
		cpuPct = 100
	}
	gpuPct := in.GPUCapPct
	if gpuPct <= 0 {
		gpuPct = 100
	}
	out.EffectiveCapState = joulie.CapState{CPUPct: cpuPct, GPUPct: gpuPct}

	// Hardware density score: normalized compute density proxy
	cpuScore := float64(in.Hardware.CPU.TotalCores) / 192.0 * 100.0 // normalize to 192-core reference
	gpuScore := float64(in.Hardware.GPU.Count) / 8.0 * 100.0        // normalize to 8-GPU reference
	if in.Hardware.GPU.Present {
		out.HardwareDensityScore = (cpuScore + gpuScore) / 2.0
	} else {
		out.HardwareDensityScore = cpuScore
	}
	out.HardwareDensityScore = math.Min(100, math.Max(0, out.HardwareDensityScore))

	// Apply defaulted cap values so downstream helpers see consistent inputs.
	in.CPUCapPct = cpuPct
	in.GPUCapPct = gpuPct

	// Compute hardware TDP and capped power budgets.
	cpuTDP := in.Hardware.CPU.CapRange.MaxWattsPerSocket * float64(in.Hardware.CPU.Sockets)
	var gpuTDP float64
	if in.Hardware.GPU.Present {
		gpuTDP = in.Hardware.GPU.CapRange.MaxWatts * float64(in.Hardware.GPU.Count)
	}
	nodeTDP := cpuTDP + gpuTDP

	cpuCappedPower := cpuTDP * in.CPUCapPct / 100.0
	gpuCappedPower := gpuTDP * in.GPUCapPct / 100.0
	nodeCappedPower := cpuCappedPower + gpuCappedPower

	// Populate power measurement output.
	out.PowerMeasurement = PowerMeasurementOutput{
		MeasuredNodePowerW: in.MeasuredNodePowerW,
		CpuCappedPowerW:    cpuCappedPower,
		GpuCappedPowerW:    gpuCappedPower,
		NodeCappedPowerW:   nodeCappedPower,
		CpuTdpW:            cpuTDP,
		GpuTdpW:            gpuTDP,
		NodeTdpW:           nodeTDP,
		PowerTrendWPerMin:  in.PowerTrendWPerMin,
	}

	// Power headroom score: how much of the node's power budget remains.
	// Higher score = more headroom = better for new workloads.
	out.PredictedPowerHeadroomScore = computePowerHeadroom(in.MeasuredNodePowerW, nodeCappedPower)

	// Cooling stress score: fraction of physical cooling capacity in use,
	// adjusted for ambient temperature.
	out.PredictedCoolingStressScore = computeCoolingStress(in.MeasuredNodePowerW, nodeTDP, in.OutsideTempC)

	// PSU stress score: proxy based on cluster/rack power load.
	// Reserved for future rack-topology-aware extensions.
	out.PredictedPsuStressScore = computePSUStress(in)

	// Estimated PUE: derived from cooling stress.
	// At 0 stress: PUE = 1.05 (best-case datacenter overhead).
	// At 100 stress: PUE = 1.40 (stressed cooling, high overhead).
	// Reserved for future extensions; not used in scoring.
	out.EstimatedPUE = 1.0 + 0.05 + (out.PredictedCoolingStressScore/100.0)*0.35

	return out
}

// computePowerHeadroom returns a score (clamped to max 100) representing
// the fraction of the node's capped power budget that remains unused.
//
//	headroom = (nodeCappedPower - measuredPower) / nodeCappedPower * 100
//
// Headroom can go negative (node drawing more than its budget) but is clamped
// to 100 at the top. If nodeCappedPower is 0 (unknown hardware), returns 100
// (neutral — don't penalize nodes we know nothing about).
func computePowerHeadroom(measuredPowerW, nodeCappedPowerW float64) float64 {
	if nodeCappedPowerW <= 0 {
		return 100
	}
	headroom := (nodeCappedPowerW - measuredPowerW) / nodeCappedPowerW * 100.0
	return math.Min(100, headroom)
}

// computeCoolingStress returns a 0-100 stress score for cooling.
//
//	tempMultiplier = 1 / max(0.5, 1 - (ambientTemp - 20) * 0.02)
//	coolingStress  = clamp((measuredPower / nodeTDP) * tempMultiplier * 100, 0, 100)
//
// If nodeTDP is 0, returns 0 (no hardware info → no stress signal).
// If ambientTemp <= 0 (unknown), tempMultiplier defaults to 1.0.
func computeCoolingStress(measuredPowerW, nodeTDPW, ambientTempC float64) float64 {
	if nodeTDPW <= 0 {
		return 0
	}

	// Temperature multiplier: hotter ambient → harder to cool → higher stress.
	var tempMultiplier float64
	if ambientTempC <= 0 {
		tempMultiplier = 1.0
	} else {
		denominator := 1.0 - (ambientTempC-20.0)*0.02
		tempMultiplier = 1.0 / math.Max(0.5, denominator)
	}

	stress := (measuredPowerW / nodeTDPW) * tempMultiplier * 100.0
	return math.Min(100, math.Max(0, stress))
}

// computePSUStress returns a 0-100 stress score for PSU (rack power supply units).
// When topology is enabled and RackTotalPowerW is set, stress is computed
// per-rack instead of cluster-wide, giving a more accurate picture of PDU
// headroom for the node's physical rack.
//
// Reserved for future rack-topology-aware extensions. Not used in scoring.
func computePSUStress(in Input) float64 {
	powerW := in.RackTotalPowerW
	if powerW <= 0 {
		powerW = in.ClusterTotalPowerW
	}
	if powerW <= 0 {
		return 0
	}
	// Reference rack capacity: 50kW. In a real deployment this could be
	// per-rack from facility data; for now it is a shared constant.
	const referenceRackCapacityW = 50000.0 // 50kW rack
	stress := (powerW / referenceRackCapacityW) * 100.0
	return math.Min(100, math.Max(0, stress))
}

// ComputeHardwareDensityScore is exported for use in tests and operator.
func ComputeHardwareDensityScore(hw joulie.NodeHardware) float64 {
	cpuScore := float64(hw.CPU.TotalCores) / 192.0 * 100.0
	gpuScore := float64(hw.GPU.Count) / 8.0 * 100.0
	if hw.GPU.Present {
		return math.Min(100, (cpuScore+gpuScore)/2.0)
	}
	return math.Min(100, cpuScore)
}
