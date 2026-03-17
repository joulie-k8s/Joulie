package powerest

import (
	"fmt"
	"math"
)

// EstimateMarginalImpact computes the predicted incremental power and stress
// cost of placing pod (described by demand) on the given node. The current
// node energy state is provided via currentCoolingStress and currentPsuStress
// (from NodeTwin.status).
//
// If node is nil (hardware data unavailable), it returns a zero estimate so
// the caller preserves legacy twin-only scoring.
func EstimateMarginalImpact(
	demand PodDemand,
	node *NodePowerProfile,
	currentCoolingStress float64,
	currentPsuStress float64,
	coeff Coefficients,
) MarginalEstimate {
	if node == nil {
		return MarginalEstimate{Explanation: "no hardware data; skipping marginal estimate"}
	}

	est := MarginalEstimate{}

	// --- CPU delta ---
	cpuMaxW := node.CPUMaxWattsTotal
	if cpuMaxW <= 0 {
		cpuMaxW = math.Max(150, float64(node.CPUTotalCores)*coeff.CPUWattsPerCoreFallback)
	}
	totalCores := float64(node.CPUTotalCores)
	if totalCores <= 0 {
		totalCores = 1
	}
	utilShare := clamp(demand.CPUCores/totalCores, 0, 1)
	est.DeltaCPUWatts = cpuMaxW * coeff.CPUUtilCoeff * utilShare

	// Memory modifier: memory-heavy pods slightly increase CPU power.
	memGiB := demand.MemoryBytes / (1024 * 1024 * 1024)
	memMod := clamp(1+memGiB*coeff.MemPowerCoeff, 1, coeff.MemPowerCap)
	est.DeltaCPUWatts *= memMod

	// --- GPU delta ---
	if demand.GPUCount > 0 && node.HasGPU {
		gpuMaxW := node.GPUMaxWattsPerGPU
		if gpuMaxW <= 0 {
			gpuMaxW = gpuFallbackWatts(demand.GPUVendor, coeff)
		}
		utilCoeff := coeff.GPUUtilCoeffStandard
		if demand.WorkloadClass == "performance" {
			utilCoeff = coeff.GPUUtilCoeffPerformance
		}
		est.DeltaGPUWatts = float64(demand.GPUCount) * gpuMaxW * utilCoeff
	}

	// --- Idle GPU waste ---
	if demand.GPUCount == 0 && node.HasGPU && node.GPUCount > 0 {
		est.IdleGPUWastePenalty = math.Min(
			float64(node.GPUCount)*coeff.IdleGPUWattsPerDevice,
			coeff.IdleGPUPenaltyCap,
		)
	}

	est.DeltaTotalWatts = est.DeltaCPUWatts + est.DeltaGPUWatts

	// --- Stress projections ---
	refNodeW := coeff.ReferenceNodePowerW
	if refNodeW <= 0 {
		refNodeW = 4000
	}
	est.CoolingStressDelta = clamp((est.DeltaTotalWatts/refNodeW)*80, 0, 100)
	est.ProjectedCoolingStress = clamp(currentCoolingStress+est.CoolingStressDelta, 0, 100)

	refRackW := coeff.ReferenceRackCapacityW
	if refRackW <= 0 {
		refRackW = 50000
	}
	est.PsuStressDelta = clamp((est.DeltaTotalWatts/refRackW)*100, 0, 100)
	est.ProjectedPsuStress = clamp(currentPsuStress+est.PsuStressDelta, 0, 100)

	est.Explanation = fmt.Sprintf(
		"cpu=%.1fW(%.2f cores/%.0f, coeff=%.2f) gpu=%.1fW(%dx%.0fW) idle_gpu=%.0fW cool=%.1f->%.1f psu=%.1f->%.1f",
		est.DeltaCPUWatts, demand.CPUCores, totalCores, coeff.CPUUtilCoeff,
		est.DeltaGPUWatts, demand.GPUCount, node.GPUMaxWattsPerGPU,
		est.IdleGPUWastePenalty,
		currentCoolingStress, est.ProjectedCoolingStress,
		currentPsuStress, est.ProjectedPsuStress,
	)

	return est
}

// ComputeScoreAdjustment converts a MarginalEstimate into penalty points to
// subtract from the twin-based scheduling score. All components are clamped
// independently to prevent any single factor from dominating.
func ComputeScoreAdjustment(est MarginalEstimate) ScoreAdjustment {
	adj := ScoreAdjustment{}
	adj.MarginalPowerPenalty = clamp(est.DeltaTotalWatts/20, 0, 20)
	adj.CoolingDeltaPenalty = clamp(est.CoolingStressDelta*0.6, 0, 20)
	adj.PsuDeltaPenalty = clamp(est.PsuStressDelta*0.4, 0, 15)
	adj.IdleGPUWastePenalty = clamp(est.IdleGPUWastePenalty/10, 0, 20)
	adj.TotalPenalty = adj.MarginalPowerPenalty + adj.CoolingDeltaPenalty +
		adj.PsuDeltaPenalty + adj.IdleGPUWastePenalty
	adj.Explanation = fmt.Sprintf(
		"power=%.1f cool=%.1f psu=%.1f idle_gpu=%.1f total=%.1f",
		adj.MarginalPowerPenalty, adj.CoolingDeltaPenalty,
		adj.PsuDeltaPenalty, adj.IdleGPUWastePenalty, adj.TotalPenalty,
	)
	return adj
}

// gpuFallbackWatts returns a vendor-appropriate GPU power fallback.
func gpuFallbackWatts(vendor string, coeff Coefficients) float64 {
	switch vendor {
	case "nvidia":
		return coeff.GPUMaxWattsFallbackNVIDIA
	case "amd":
		return coeff.GPUMaxWattsFallbackAMD
	default:
		return coeff.GPUMaxWattsFallbackGeneric
	}
}
