package powerest

import (
	"fmt"
	"math"
)

// EstimateMarginalImpact computes the predicted incremental power cost of
// placing pod (described by demand) on the given node.
//
// If node is nil (hardware data unavailable), it returns a zero estimate so
// the caller preserves legacy twin-only scoring.
func EstimateMarginalImpact(
	demand PodDemand,
	node *NodePowerProfile,
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

	est.DeltaTotalWatts = est.DeltaCPUWatts + est.DeltaGPUWatts

	est.Explanation = fmt.Sprintf(
		"cpu=%.1fW(%.2f cores/%.0f, coeff=%.2f) gpu=%.1fW(%dx%.0fW) total=%.1fW",
		est.DeltaCPUWatts, demand.CPUCores, totalCores, coeff.CPUUtilCoeff,
		est.DeltaGPUWatts, demand.GPUCount, node.GPUMaxWattsPerGPU,
		est.DeltaTotalWatts,
	)

	return est
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
