// Package powerest estimates the marginal power impact of placing a pod on a
// candidate node. It combines pod resource requests with cached node hardware
// facts to produce a watt-level delta that the scheduler subtracts from the
// node's power headroom before scoring.
//
// The estimation is intentionally heuristic and conservative: it uses declared
// resource requests (not runtime telemetry) and simple linear models with
// tunable coefficients. The goal is directional correctness -- steer pods
// away from nodes where they would cause disproportionate power impact --
// not exact watt-level prediction.
package powerest

// PodDemand captures the resource footprint of a pending pod as seen at
// scheduling time. Extracted from the pod spec by ExtractPodDemand.
type PodDemand struct {
	CPUCores      float64 // effective CPU cores (requests preferred, then limits, then fallback)
	MemoryBytes   float64 // effective memory bytes
	GPUCount      int     // number of GPUs requested (any vendor)
	GPUVendor     string  // "nvidia", "amd", "intel", or ""
	WorkloadClass string  // "performance", "standard", or ""
}

// NodePowerProfile holds the hardware power envelope for one node, extracted
// from the NodeHardware CR. The scheduler caches these and passes them into
// the estimation functions.
type NodePowerProfile struct {
	NodeName          string
	CPUModel          string
	GPUModel          string
	CPUTotalCores     int
	CPUSockets        int
	CPUMaxWattsTotal  float64 // maxWattsPerSocket * sockets (or heuristic fallback)
	GPUCount          int
	GPUMaxWattsPerGPU float64
	HasGPU            bool
	MemoryBytes       int64
}

// MarginalEstimate is the output of EstimateMarginalImpact. It quantifies the
// predicted incremental power cost of placing a specific pod on a specific node.
type MarginalEstimate struct {
	DeltaCPUWatts   float64 // estimated incremental CPU package power
	DeltaGPUWatts   float64 // estimated incremental GPU board power
	DeltaTotalWatts float64 // CPU + GPU combined
	Explanation     string  // human-readable scoring rationale
}

// Coefficients holds all tunable parameters for the estimation model.
// The scheduler reads these from environment variables at startup and passes
// them into estimation functions.
type Coefficients struct {
	// CPUUtilCoeff: fraction of max CPU TDP attributed to marginal pod load.
	CPUUtilCoeff float64
	// MemPowerCoeff: per-GiB memory power modifier on CPU delta.
	MemPowerCoeff float64
	// MemPowerCap: maximum multiplier from memory modifier.
	MemPowerCap float64
	// GPUUtilCoeffStandard: expected GPU utilization fraction for standard pods.
	GPUUtilCoeffStandard float64
	// GPUUtilCoeffPerformance: expected GPU utilization fraction for performance pods.
	GPUUtilCoeffPerformance float64
	// CPUWattsPerCoreFallback: fallback watts-per-core when hardware data is missing.
	CPUWattsPerCoreFallback float64
	// GPUMaxWattsFallbackNVIDIA: fallback per-GPU max watts for NVIDIA.
	GPUMaxWattsFallbackNVIDIA float64
	// GPUMaxWattsFallbackAMD: fallback per-GPU max watts for AMD.
	GPUMaxWattsFallbackAMD float64
	// GPUMaxWattsFallbackGeneric: fallback per-GPU max watts for unknown vendor.
	GPUMaxWattsFallbackGeneric float64
}

// DefaultCoefficients returns production-ready defaults. Override individual
// fields via environment variables at scheduler startup.
func DefaultCoefficients() Coefficients {
	return Coefficients{
		CPUUtilCoeff:               0.7,
		MemPowerCoeff:              0.01,
		MemPowerCap:                1.3,
		GPUUtilCoeffStandard:       0.65,
		GPUUtilCoeffPerformance:    0.85,
		CPUWattsPerCoreFallback:    3.5,
		GPUMaxWattsFallbackNVIDIA:  350,
		GPUMaxWattsFallbackAMD:     400,
		GPUMaxWattsFallbackGeneric: 350,
	}
}
