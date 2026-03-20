package powerest

import (
	"math"
	"testing"
)

// --- Pod demand extraction ---

func TestExtractPodDemand_CPURequestsPreferred(t *testing.T) {
	pod := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{"cpu": "2"},
						"limits":   map[string]interface{}{"cpu": "4"},
					},
				},
			},
		},
	}
	d := ExtractPodDemand(pod, "standard")
	if d.CPUCores != 2 {
		t.Errorf("expected 2 cores from requests, got %.2f", d.CPUCores)
	}
}

func TestExtractPodDemand_CPULimitsFallback(t *testing.T) {
	pod := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{
					"resources": map[string]interface{}{
						"limits": map[string]interface{}{"cpu": "4"},
					},
				},
			},
		},
	}
	d := ExtractPodDemand(pod, "standard")
	if d.CPUCores != 4 {
		t.Errorf("expected 4 cores from limits, got %.2f", d.CPUCores)
	}
}

func TestExtractPodDemand_MillicoreParsing(t *testing.T) {
	pod := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{"cpu": "500m"},
					},
				},
			},
		},
	}
	d := ExtractPodDemand(pod, "standard")
	if math.Abs(d.CPUCores-0.5) > 0.001 {
		t.Errorf("expected 0.5 cores from 500m, got %.4f", d.CPUCores)
	}
}

func TestExtractPodDemand_MultipleContainers(t *testing.T) {
	pod := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{"cpu": "1"},
					},
				},
				map[string]interface{}{
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{"cpu": "2"},
					},
				},
			},
		},
	}
	d := ExtractPodDemand(pod, "standard")
	if d.CPUCores != 3 {
		t.Errorf("expected 3 cores from two containers, got %.2f", d.CPUCores)
	}
}

func TestExtractPodDemand_FallbackPerformance(t *testing.T) {
	pod := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{},
			},
		},
	}
	d := ExtractPodDemand(pod, "performance")
	if d.CPUCores != 0.5 {
		t.Errorf("expected 0.5 fallback for performance, got %.2f", d.CPUCores)
	}
}

func TestExtractPodDemand_FallbackStandard(t *testing.T) {
	pod := map[string]interface{}{}
	d := ExtractPodDemand(pod, "standard")
	if d.CPUCores != 0.25 {
		t.Errorf("expected 0.25 fallback for standard, got %.2f", d.CPUCores)
	}
}

func TestExtractPodDemand_GPUNvidia(t *testing.T) {
	pod := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{"nvidia.com/gpu": "2", "cpu": "4"},
						"limits":   map[string]interface{}{"nvidia.com/gpu": "2"},
					},
				},
			},
		},
	}
	d := ExtractPodDemand(pod, "standard")
	if d.GPUCount != 2 {
		t.Errorf("expected 2 GPUs, got %d", d.GPUCount)
	}
	if d.GPUVendor != "nvidia" {
		t.Errorf("expected nvidia vendor, got %s", d.GPUVendor)
	}
}

func TestExtractPodDemand_GPUAMD(t *testing.T) {
	pod := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{
					"resources": map[string]interface{}{
						"limits": map[string]interface{}{"amd.com/gpu": "1"},
					},
				},
			},
		},
	}
	d := ExtractPodDemand(pod, "standard")
	if d.GPUCount != 1 || d.GPUVendor != "amd" {
		t.Errorf("expected 1 AMD GPU, got %d %s", d.GPUCount, d.GPUVendor)
	}
}

func TestExtractPodDemand_GPUIntel(t *testing.T) {
	pod := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{
					"resources": map[string]interface{}{
						"limits": map[string]interface{}{"gpu.intel.com/i915": "1"},
					},
				},
			},
		},
	}
	d := ExtractPodDemand(pod, "standard")
	if d.GPUCount != 1 || d.GPUVendor != "intel" {
		t.Errorf("expected 1 Intel GPU, got %d %s", d.GPUCount, d.GPUVendor)
	}
}

func TestExtractPodDemand_MemoryParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
	}{
		{"128Mi", 128 * 1024 * 1024},
		{"1Gi", 1024 * 1024 * 1024},
		{"256M", 256e6},
		{"1073741824", 1073741824},
	}
	for _, tc := range cases {
		pod := map[string]interface{}{
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"resources": map[string]interface{}{
							"requests": map[string]interface{}{"memory": tc.raw, "cpu": "1"},
						},
					},
				},
			},
		}
		d := ExtractPodDemand(pod, "standard")
		if math.Abs(d.MemoryBytes-tc.want) > 1 {
			t.Errorf("memory %q: expected %.0f, got %.0f", tc.raw, tc.want, d.MemoryBytes)
		}
	}
}

func TestExtractPodDemand_NumericCPU(t *testing.T) {
	// CPU as float64 (JSON numbers are float64 after unmarshal)
	pod := map[string]interface{}{
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{"cpu": float64(4)},
					},
				},
			},
		},
	}
	d := ExtractPodDemand(pod, "standard")
	if d.CPUCores != 4 {
		t.Errorf("expected 4 cores from float64, got %.2f", d.CPUCores)
	}
}

// --- Marginal estimation ---

func cpuOnlyNode() *NodePowerProfile {
	return &NodePowerProfile{
		NodeName:         "cpu-node",
		CPUModel:         "EPYC-9654",
		CPUTotalCores:    96,
		CPUSockets:       2,
		CPUMaxWattsTotal: 720, // 360W per socket
		HasGPU:           false,
	}
}

func gpuNode() *NodePowerProfile {
	return &NodePowerProfile{
		NodeName:          "gpu-node",
		CPUModel:          "EPYC-9654",
		GPUModel:          "H100-SXM",
		CPUTotalCores:     96,
		CPUSockets:        2,
		CPUMaxWattsTotal:  720,
		GPUCount:          8,
		GPUMaxWattsPerGPU: 700,
		HasGPU:            true,
	}
}

func smallGPUNode() *NodePowerProfile {
	return &NodePowerProfile{
		NodeName:          "small-gpu",
		CPUModel:          "Xeon-6530",
		GPUModel:          "L40S",
		CPUTotalCores:     32,
		CPUSockets:        1,
		CPUMaxWattsTotal:  270,
		GPUCount:          2,
		GPUMaxWattsPerGPU: 350,
		HasGPU:            true,
	}
}

func TestEstimateMarginalImpact_NilNode(t *testing.T) {
	demand := PodDemand{CPUCores: 4}
	est := EstimateMarginalImpact(demand, nil, DefaultCoefficients())
	if est.DeltaTotalWatts != 0 {
		t.Errorf("nil node should return zero delta, got %.2f", est.DeltaTotalWatts)
	}
}

func TestEstimateMarginalImpact_CPUOnly(t *testing.T) {
	demand := PodDemand{CPUCores: 8, WorkloadClass: "standard"}
	node := cpuOnlyNode()
	coeff := DefaultCoefficients()
	est := EstimateMarginalImpact(demand, node, coeff)

	// utilShare = 8/96 = 0.0833
	// deltaCPU = 720 * 0.7 * 0.0833 = ~41.98
	expectedCPU := 720 * 0.7 * (8.0 / 96.0)
	if math.Abs(est.DeltaCPUWatts-expectedCPU) > 1 {
		t.Errorf("expected ~%.1fW CPU delta, got %.1f", expectedCPU, est.DeltaCPUWatts)
	}
	if est.DeltaGPUWatts != 0 {
		t.Errorf("CPU-only pod on CPU-only node should have 0 GPU delta, got %.1f", est.DeltaGPUWatts)
	}
}

func TestEstimateMarginalImpact_GPUPod(t *testing.T) {
	demand := PodDemand{CPUCores: 4, GPUCount: 2, GPUVendor: "nvidia", WorkloadClass: "standard"}
	node := gpuNode()
	coeff := DefaultCoefficients()
	est := EstimateMarginalImpact(demand, node, coeff)

	// deltaGPU = 2 * 700 * 0.65 = 910
	expectedGPU := 2.0 * 700 * 0.65
	if math.Abs(est.DeltaGPUWatts-expectedGPU) > 1 {
		t.Errorf("expected ~%.1fW GPU delta, got %.1f", expectedGPU, est.DeltaGPUWatts)
	}
}

func TestEstimateMarginalImpact_PerformanceGPUHigherUtil(t *testing.T) {
	demandStd := PodDemand{CPUCores: 4, GPUCount: 1, GPUVendor: "nvidia", WorkloadClass: "standard"}
	demandPerf := PodDemand{CPUCores: 4, GPUCount: 1, GPUVendor: "nvidia", WorkloadClass: "performance"}
	node := gpuNode()
	coeff := DefaultCoefficients()

	estStd := EstimateMarginalImpact(demandStd, node, coeff)
	estPerf := EstimateMarginalImpact(demandPerf, node, coeff)

	if estPerf.DeltaGPUWatts <= estStd.DeltaGPUWatts {
		t.Errorf("performance pod should have higher GPU delta: perf=%.1f std=%.1f",
			estPerf.DeltaGPUWatts, estStd.DeltaGPUWatts)
	}
}

func TestEstimateMarginalImpact_SamePodPrefersSmallerNode(t *testing.T) {
	demand := PodDemand{CPUCores: 4, GPUCount: 1, GPUVendor: "nvidia", WorkloadClass: "standard"}
	coeff := DefaultCoefficients()
	estLarge := EstimateMarginalImpact(demand, gpuNode(), coeff)
	estSmall := EstimateMarginalImpact(demand, smallGPUNode(), coeff)

	// On a small GPU node with lower GPU max watts, delta should be lower.
	if estSmall.DeltaGPUWatts >= estLarge.DeltaGPUWatts {
		t.Errorf("smaller GPU node should have lower GPU delta: small=%.1f large=%.1f",
			estSmall.DeltaGPUWatts, estLarge.DeltaGPUWatts)
	}
}

func TestEstimateMarginalImpact_MissingCPUWattsFallback(t *testing.T) {
	demand := PodDemand{CPUCores: 4, WorkloadClass: "standard"}
	node := &NodePowerProfile{
		NodeName:      "no-watts",
		CPUTotalCores: 32,
		// CPUMaxWattsTotal = 0 (unknown)
	}
	coeff := DefaultCoefficients()
	est := EstimateMarginalImpact(demand, node, coeff)

	// Fallback: max(150, 32*3.5) = max(150, 112) = 150
	expectedMax := math.Max(150, 32*3.5)
	expectedCPU := expectedMax * 0.7 * (4.0 / 32.0)
	if math.Abs(est.DeltaCPUWatts-expectedCPU) > 1 {
		t.Errorf("expected fallback CPU delta ~%.1f, got %.1f", expectedCPU, est.DeltaCPUWatts)
	}
}

func TestEstimateMarginalImpact_MissingGPUWattsFallback(t *testing.T) {
	demand := PodDemand{CPUCores: 2, GPUCount: 1, GPUVendor: "nvidia", WorkloadClass: "standard"}
	node := &NodePowerProfile{
		NodeName:      "no-gpu-watts",
		CPUTotalCores: 32,
		GPUCount:      4,
		HasGPU:        true,
		// GPUMaxWattsPerGPU = 0 (unknown)
	}
	coeff := DefaultCoefficients()
	est := EstimateMarginalImpact(demand, node, coeff)

	// Fallback for nvidia = 350W
	expectedGPU := 1 * 350.0 * 0.65
	if math.Abs(est.DeltaGPUWatts-expectedGPU) > 1 {
		t.Errorf("expected fallback GPU delta ~%.1f, got %.1f", expectedGPU, est.DeltaGPUWatts)
	}
}

func TestEstimateMarginalImpact_AMDGPUFallback(t *testing.T) {
	demand := PodDemand{CPUCores: 2, GPUCount: 1, GPUVendor: "amd", WorkloadClass: "standard"}
	node := &NodePowerProfile{
		NodeName:      "amd-gpu",
		CPUTotalCores: 32,
		GPUCount:      4,
		HasGPU:        true,
	}
	coeff := DefaultCoefficients()
	est := EstimateMarginalImpact(demand, node, coeff)
	expectedGPU := 1 * 400.0 * 0.65
	if math.Abs(est.DeltaGPUWatts-expectedGPU) > 1 {
		t.Errorf("expected AMD fallback GPU delta ~%.1f, got %.1f", expectedGPU, est.DeltaGPUWatts)
	}
}

func TestEstimateMarginalImpact_DeltaMonotonic(t *testing.T) {
	coeff := DefaultCoefficients()
	node := cpuOnlyNode()
	var prevDelta float64
	for _, watts := range []float64{10, 50, 100, 200} {
		demand := PodDemand{CPUCores: watts / (720 * 0.7) * 96, WorkloadClass: "standard"}
		est := EstimateMarginalImpact(demand, node, coeff)
		if est.DeltaTotalWatts < prevDelta {
			t.Errorf("delta should be monotonically increasing, got %.1f after %.1f", est.DeltaTotalWatts, prevDelta)
		}
		prevDelta = est.DeltaTotalWatts
	}
}

func TestEstimateMarginalImpact_MemoryModifier(t *testing.T) {
	coeff := DefaultCoefficients()
	node := cpuOnlyNode()

	demandLow := PodDemand{CPUCores: 8, MemoryBytes: 1 * 1024 * 1024 * 1024, WorkloadClass: "standard"}
	demandHigh := PodDemand{CPUCores: 8, MemoryBytes: 128 * 1024 * 1024 * 1024, WorkloadClass: "standard"}

	estLow := EstimateMarginalImpact(demandLow, node, coeff)
	estHigh := EstimateMarginalImpact(demandHigh, node, coeff)

	if estHigh.DeltaCPUWatts <= estLow.DeltaCPUWatts {
		t.Errorf("memory-heavy pod should have higher CPU delta: high=%.1f low=%.1f",
			estHigh.DeltaCPUWatts, estLow.DeltaCPUWatts)
	}
}

// --- Edge cases ---

func TestEstimateMarginalImpact_ZeroCores(t *testing.T) {
	demand := PodDemand{CPUCores: 0.1, WorkloadClass: "standard"}
	node := &NodePowerProfile{CPUTotalCores: 0} // degenerate
	coeff := DefaultCoefficients()
	est := EstimateMarginalImpact(demand, node, coeff)
	// Should not panic, should produce some estimate
	if est.DeltaCPUWatts < 0 {
		t.Errorf("CPU delta should be non-negative, got %.2f", est.DeltaCPUWatts)
	}
}

func TestEstimateMarginalImpact_GPUPodOnCPUNode(t *testing.T) {
	demand := PodDemand{CPUCores: 4, GPUCount: 2, GPUVendor: "nvidia", WorkloadClass: "standard"}
	node := cpuOnlyNode()
	coeff := DefaultCoefficients()
	est := EstimateMarginalImpact(demand, node, coeff)
	// GPU pod on CPU-only node: no GPU delta
	if est.DeltaGPUWatts != 0 {
		t.Errorf("GPU pod on CPU node should have 0 GPU delta, got %.1f", est.DeltaGPUWatts)
	}
}

// --- Quantity parsing ---

func TestParseQuantityString(t *testing.T) {
	cases := []struct {
		input string
		want  float64
	}{
		{"500m", 0.5},
		{"1000m", 1.0},
		{"2", 2.0},
		{"1.5", 1.5},
		{"250m", 0.25},
		{"", 0},
	}
	for _, tc := range cases {
		got := parseQuantityString(tc.input)
		if math.Abs(got-tc.want) > 0.001 {
			t.Errorf("parseQuantityString(%q) = %.4f, want %.4f", tc.input, got, tc.want)
		}
	}
}

func TestParseMemoryString(t *testing.T) {
	cases := []struct {
		input string
		want  float64
	}{
		{"128Mi", 128 * 1024 * 1024},
		{"1Gi", 1024 * 1024 * 1024},
		{"256M", 256e6},
		{"2Ki", 2048},
		{"1073741824", 1073741824},
		{"", 0},
	}
	for _, tc := range cases {
		got := parseMemoryString(tc.input)
		if math.Abs(got-tc.want) > 1 {
			t.Errorf("parseMemoryString(%q) = %.0f, want %.0f", tc.input, got, tc.want)
		}
	}
}
