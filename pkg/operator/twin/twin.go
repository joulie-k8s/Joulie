// Package twin implements the Joulie digital twin.
//
// The digital twin is a lightweight O(1) parametric model that predicts the
// impact of scheduling and power-cap decisions on:
//   - Power headroom (available budget before thermal/PSU limit)
//   - Cooling stress (fraction of cooling capacity in use)
//   - PSU stress (fraction of rack PDU capacity in use)
//
// Runs every ~1 minute in the operator, writes NodeTwin CRs,
// read by the scheduler extender for placement decisions.
//
// CoolingModel is pluggable. Default: LinearCoolingModel (algebraic proxy).
// Future: reduced-order openModelica thermal simulation via subprocess/HTTP.
package twin

import (
	"math"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
)

// CoolingModel predicts cooling stress given node power and ambient temperature.
// Implement this interface to swap in a higher-fidelity thermal model.
type CoolingModel interface {
	// CoolingStress returns 0-100. 100 = cooling at capacity.
	// nodePowerW = total draw of this node; ambientTempC = outside air temperature.
	CoolingStress(nodePowerW, ambientTempC float64) float64
}

// LinearCoolingModel is the default algebraic proxy.
//
//	coolingStress = (nodePower / referenceNodePower) × 80 + max(0, temp-20) × 0.5
//
// referenceNodePower defaults to 4000 W (2S EPYC + 8×H100).
// Replace with a thermal RC or openModelica model for higher accuracy.
type LinearCoolingModel struct {
	ReferenceNodePowerW float64
}

func (m LinearCoolingModel) CoolingStress(nodePowerW, ambientTempC float64) float64 {
	ref := m.ReferenceNodePowerW
	if ref <= 0 {
		ref = 4000.0
	}
	stress := (nodePowerW / ref) * 80.0
	if ambientTempC > 20 {
		stress += (ambientTempC - 20) * 0.5
	}
	return math.Min(100, math.Max(0, stress))
}

var defaultCoolingModel CoolingModel = LinearCoolingModel{}

// Input holds all the inputs needed to compute twin state for one node.
type Input struct {
	NodeName string
	Hardware joulie.NodeHardware
	Profile  string  // "eco" or "performance"
	CPUCapPct float64
	GPUCapPct float64
	Draining  bool
	// Workloads on this node
	Workloads []joulie.WorkloadProfileStatus
	// Facility signals
	ClusterTotalPowerW float64
	OutsideTempC       float64 // 0 if unknown
}

// Output is the computed NodeTwinState fields.
type Output struct {
	SchedulableClass            string
	PredictedPowerHeadroomScore float64
	PredictedCoolingStressScore float64
	PredictedPsuStressScore     float64
	EffectiveCapState           joulie.CapState
	HardwareDensityScore        float64
	RescheduleRecommendations   []joulie.RescheduleRecommendation
	GPUSlicingRecommendation    *joulie.GPUSlicingRecommendation
	LastUpdated                 time.Time
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

	// Effective cap state
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
	// Based on cores*sockets and GPU count
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

	// Power headroom score: how much power budget is left
	// Higher score = more headroom = better for new workloads
	out.PredictedPowerHeadroomScore = computePowerHeadroom(in)

	// Cooling stress score: proxy based on power density and outside temp
	out.PredictedCoolingStressScore = computeCoolingStress(in)

	// PSU stress score: proxy based on cluster power load
	out.PredictedPsuStressScore = computePSUStress(in)

	// Reschedule recommendations
	out.RescheduleRecommendations = computeRescheduleRecommendations(in)

	// GPU slicing recommendation
	out.GPUSlicingRecommendation = computeGPUSlicingRecommendation(in)

	return out
}

// computePowerHeadroom returns a 0-100 score where 100 = maximum available headroom.
// Lower cap = less headroom.
func computePowerHeadroom(in Input) float64 {
	// Headroom is inversely proportional to how constrained the cap is
	// and whether the node is already under stress
	capFactor := (in.CPUCapPct + in.GPUCapPct) / 200.0 // 0=fully capped, 1=uncapped

	// Adjust by cooling stress (high cooling stress = low headroom)
	coolingFactor := 1.0 - computeCoolingStress(in)/100.0

	headroom := capFactor * coolingFactor * 100.0
	return math.Min(100, math.Max(0, headroom))
}

// computeCoolingStress returns a 0-100 stress score for cooling via CoolingModel.
func computeCoolingStress(in Input) float64 {
	var nodePowerW float64
	if in.Hardware.CPU.CapRange.MaxWattsPerSocket > 0 {
		nodePowerW = in.Hardware.CPU.CapRange.MaxWattsPerSocket * float64(in.Hardware.CPU.Sockets) * (in.CPUCapPct / 100.0)
	}
	if in.Hardware.GPU.Present && in.Hardware.GPU.CapRange.MaxWatts > 0 {
		nodePowerW += in.Hardware.GPU.CapRange.MaxWatts * float64(in.Hardware.GPU.Count) * (in.GPUCapPct / 100.0)
	}
	return defaultCoolingModel.CoolingStress(nodePowerW, in.OutsideTempC)
}

// computePSUStress returns a 0-100 stress score for PSU (rack power supply units).
func computePSUStress(in Input) float64 {
	if in.ClusterTotalPowerW <= 0 {
		return 0
	}
	// Rough proxy: cluster power relative to some reference rack capacity
	// In a real deployment this would use actual PDU/PSU readings
	const referenceRackCapacityW = 50000.0 // 50kW rack
	stress := (in.ClusterTotalPowerW / referenceRackCapacityW) * 100.0
	return math.Min(100, math.Max(0, stress))
}

// computeRescheduleRecommendations identifies workloads that should be rescheduled.
func computeRescheduleRecommendations(in Input) []joulie.RescheduleRecommendation {
	var recs []joulie.RescheduleRecommendation

	// Only recommend rescheduling when node is under stress
	if in.Draining {
		return recs // already draining, eviction handles it
	}

	coolingStress := computeCoolingStress(in)
	psuStress := computePSUStress(in)
	highStress := coolingStress > 70 || psuStress > 70

	if !highStress {
		return recs
	}

	// Find reschedulable best-effort workloads
	for _, w := range in.Workloads {
		if w.Migratability.Reschedulable && w.Criticality.Class == "best-effort" {
			recs = append(recs, joulie.RescheduleRecommendation{
				Reason: "node under thermal/power stress, workload is reschedulable best-effort",
			})
		}
	}
	return recs
}

// computeGPUSlicingRecommendation analyzes workload profiles running on a node
// and recommends the optimal GPU slicing configuration (MIG or time-slicing).
//
// The recommendation is advisory: cluster admins review and apply it manually,
// since MIG reconfiguration requires GPU reset and pod eviction.
//
// Decision logic:
//  1. No GPU or slicing not supported → nil (no recommendation)
//  2. Analyze workload GPU intensity distribution:
//     - If most workloads use <30% GPU → recommend MIG small slices (1g.10gb)
//     - If most workloads use 30-70% GPU → recommend MIG medium slices (3g.40gb)
//     - If most workloads use >70% GPU → recommend whole GPU (no slicing)
//  3. If no workloads have GPU usage data → recommend time-slicing as a safe default
func computeGPUSlicingRecommendation(in Input) *joulie.GPUSlicingRecommendation {
	if !in.Hardware.GPU.Present || in.Hardware.GPU.Count == 0 {
		return nil
	}
	if !in.Hardware.GPU.Slicing.Supported {
		return nil
	}

	// Count workloads by GPU intensity bucket
	var lowGPU, medGPU, highGPU, noGPU int
	for _, w := range in.Workloads {
		switch {
		case w.GPU.Intensity == "" || w.GPU.Intensity == "none":
			noGPU++
		case w.GPU.Intensity == "low":
			lowGPU++
		case w.GPU.Intensity == "medium":
			medGPU++
		case w.GPU.Intensity == "high":
			highGPU++
		}
	}

	gpuWorkloads := lowGPU + medGPU + highGPU
	gpuCount := in.Hardware.GPU.Count

	// No GPU workloads observed → time-slicing as safe default
	if gpuWorkloads == 0 {
		return &joulie.GPUSlicingRecommendation{
			Mode:                     "time-slicing",
			SlicesPerGPU:             4,
			TotalSlices:              4 * gpuCount,
			Reason:                   "no GPU workloads observed; time-slicing is a safe default that requires no GPU reset",
			EstimatedUtilizationGain: 10,
			Confidence:               0.3,
		}
	}

	confidence := math.Min(1.0, float64(gpuWorkloads)/10.0) // more data → higher confidence

	// Dominant pattern: most workloads are low-intensity → small MIG slices
	if lowGPU > medGPU && lowGPU > highGPU {
		return &joulie.GPUSlicingRecommendation{
			Mode:                     "mig",
			SliceType:                "1g.10gb",
			SlicesPerGPU:             7,
			TotalSlices:              7 * gpuCount,
			Reason:                   "majority of GPU workloads are low-intensity; small MIG slices maximize GPU sharing and power efficiency",
			EstimatedUtilizationGain: 40,
			Confidence:               confidence,
		}
	}

	// Dominant pattern: most workloads are medium-intensity → medium MIG slices
	if medGPU >= highGPU {
		return &joulie.GPUSlicingRecommendation{
			Mode:                     "mig",
			SliceType:                "3g.40gb",
			SlicesPerGPU:             2,
			TotalSlices:              2 * gpuCount,
			Reason:                   "majority of GPU workloads are medium-intensity; medium MIG slices balance throughput and sharing",
			EstimatedUtilizationGain: 25,
			Confidence:               confidence,
		}
	}

	// Dominant pattern: most workloads are high-intensity → whole GPU
	return &joulie.GPUSlicingRecommendation{
		Mode:                     "none",
		SlicesPerGPU:             1,
		TotalSlices:              gpuCount,
		Reason:                   "majority of GPU workloads are high-intensity; whole-GPU allocation avoids MIG overhead",
		EstimatedUtilizationGain: 0,
		Confidence:               confidence,
	}
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
