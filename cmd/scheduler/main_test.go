package main

import (
	"testing"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/scheduler/powerest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// --- Legacy twin-based scoring (nil podDemand = no marginal scoring) ---

func TestScoreNodeNoState(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{}
	hwInfo := map[string]nodeHWInfo{}
	score := scoreNode("node1", states, hwInfo, "standard", 0, false, nil, nil)
	if score != 50 {
		t.Errorf("expected neutral score 50 when no state, got %d", score)
	}
}

func TestScoreNodeEcoHighHeadroom(t *testing.T) {
	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"node1": {
			SchedulableClass:            "eco",
			PredictedPowerHeadroomScore: 80,
			PredictedCoolingStressScore: 10,
			PredictedPsuStressScore:     10,
			EffectiveCapState:           joulie.CapState{CPUPct: 60, GPUPct: 60},
			LastUpdated:                 now,
		},
		"node2": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 30,
			PredictedCoolingStressScore: 70,
			PredictedPsuStressScore:     70,
			EffectiveCapState:           joulie.CapState{CPUPct: 100, GPUPct: 100},
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{}
	s1 := scoreNode("node1", states, hwInfo, "standard", 0, false, nil, nil)
	s2 := scoreNode("node2", states, hwInfo, "standard", 0, false, nil, nil)
	if s1 <= s2 {
		t.Errorf("eco node with high headroom should outscore stressed performance node for standard: %d vs %d", s1, s2)
	}
}

func TestScoreNodeDrainingFiltered(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"draining-node": {SchedulableClass: "draining"},
	}
	reason := shouldFilterNode("draining-node", states, nil, true)
	if reason == "" {
		t.Error("draining node should be filtered for performance pod")
	}
}

func TestFilterNodesEcoRejected(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"eco-node": {SchedulableClass: "eco"},
	}
	reason := shouldFilterNode("eco-node", states, nil, true)
	if reason == "" {
		t.Error("expected eco node to be filtered for performance pod")
	}
}

func TestFilterNodesStandardAccepted(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"eco-node": {SchedulableClass: "eco"},
	}
	reason := shouldFilterNode("eco-node", states, nil, false)
	if reason != "" {
		t.Errorf("expected eco node to be accepted for standard pod, got: %s", reason)
	}
}

func TestScoreNodeStaleTwinFallsBackToNeutral(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"stale-node": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 90,
			PredictedCoolingStressScore: 5,
			PredictedPsuStressScore:     5,
			LastUpdated:                 time.Now().Add(-10 * time.Minute),
		},
		"fresh-node": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 90,
			PredictedCoolingStressScore: 5,
			PredictedPsuStressScore:     5,
			LastUpdated:                 time.Now(),
		},
	}
	hwInfo := map[string]nodeHWInfo{}
	sStale := scoreNode("stale-node", states, hwInfo, "standard", 0, false, nil, nil)
	sFresh := scoreNode("fresh-node", states, hwInfo, "standard", 0, false, nil, nil)
	if sStale != 50 {
		t.Errorf("stale node should get neutral score 50, got %d", sStale)
	}
	if sFresh == 50 {
		t.Errorf("fresh node should get a non-neutral score, got %d", sFresh)
	}
	if sStale >= sFresh {
		t.Errorf("fresh node should outscore stale node: fresh=%d stale=%d", sFresh, sStale)
	}
}

func TestIsTwinStaleWithZeroTimestamp(t *testing.T) {
	ts := &joulie.NodeTwinStatus{}
	if !isTwinStale(ts) {
		t.Error("zero LastUpdated should be treated as stale (unpopulated twin)")
	}
}

func TestIsTwinStaleWithRecentTimestamp(t *testing.T) {
	ts := &joulie.NodeTwinStatus{LastUpdated: time.Now().Add(-1 * time.Minute)}
	if isTwinStale(ts) {
		t.Error("1-minute-old data should not be stale with 5min threshold")
	}
}

func TestIsTwinStaleWithOldTimestamp(t *testing.T) {
	ts := &joulie.NodeTwinStatus{LastUpdated: time.Now().Add(-10 * time.Minute)}
	if !isTwinStale(ts) {
		t.Error("10-minute-old data should be stale with 5min threshold")
	}
}

func TestFilterNodesDrainingFilteredForPerformance(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"draining-node": {SchedulableClass: "draining"},
	}
	reason := shouldFilterNode("draining-node", states, nil, true)
	if reason == "" {
		t.Error("draining node should be filtered for performance pod")
	}
	reason = shouldFilterNode("draining-node", states, nil, false)
	if reason != "" {
		t.Errorf("draining node should accept standard pod, got: %s", reason)
	}
}

func TestFilterNodesNoStateAccepted(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{}
	reason := shouldFilterNode("new-node", states, nil, true)
	if reason != "" {
		t.Errorf("expected no filter for unknown node, got: %s", reason)
	}
}

func TestScoreNodeAllZeroStress(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"ideal": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 100,
			PredictedCoolingStressScore: 0,
			PredictedPsuStressScore:     0,
			LastUpdated:                 time.Now(),
		},
	}
	hwInfo := map[string]nodeHWInfo{}
	score := scoreNode("ideal", states, hwInfo, "standard", 0, false, nil, nil)
	if score != 100 {
		t.Errorf("expected perfect score 100 for zero stress, got %d", score)
	}
}

func TestScoreNodeAllMaxStress(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"stressed": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 0,
			PredictedCoolingStressScore: 100,
			PredictedPsuStressScore:     100,
			LastUpdated:                 time.Now(),
		},
	}
	hwInfo := map[string]nodeHWInfo{}
	score := scoreNode("stressed", states, hwInfo, "standard", 0, false, nil, nil)
	if score != 0 {
		t.Errorf("expected minimum score 0 for max stress, got %d", score)
	}
}

func TestScoreNodeAdaptivePressureRelief(t *testing.T) {
	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"perf-node": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 50,
			PredictedCoolingStressScore: 30,
			PredictedPsuStressScore:     30,
			LastUpdated:                 now,
		},
		"eco-node": {
			SchedulableClass:            "eco",
			PredictedPowerHeadroomScore: 50,
			PredictedCoolingStressScore: 30,
			PredictedPsuStressScore:     30,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{}

	highPressure := 80.0
	sPerfNode := scoreNode("perf-node", states, hwInfo, "standard", highPressure, false, nil, nil)
	sEcoNode := scoreNode("eco-node", states, hwInfo, "standard", highPressure, false, nil, nil)
	if sEcoNode <= sPerfNode {
		t.Errorf("standard pod under high pressure should prefer eco node: eco=%d perf=%d", sEcoNode, sPerfNode)
	}

	sPerfNoPressure := scoreNode("perf-node", states, hwInfo, "standard", 0, false, nil, nil)
	sEcoNoPressure := scoreNode("eco-node", states, hwInfo, "standard", 0, false, nil, nil)
	if sPerfNoPressure != sEcoNoPressure {
		t.Errorf("with zero pressure, same-stats nodes should score equally: perf=%d eco=%d", sPerfNoPressure, sEcoNoPressure)
	}
}

func TestScoreNodeCPUOnlyGPUPenalty(t *testing.T) {
	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"gpu-node": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 80,
			PredictedCoolingStressScore: 10,
			PredictedPsuStressScore:     10,
			LastUpdated:                 now,
		},
		"cpu-node": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 80,
			PredictedCoolingStressScore: 10,
			PredictedPsuStressScore:     10,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{
		"gpu-node": {GPUPresent: true, GPUCount: 8},
		"cpu-node": {GPUPresent: false, GPUCount: 0},
	}

	// CPU-only pod should prefer CPU-only node over GPU node (legacy penalty)
	sGPU := scoreNode("gpu-node", states, hwInfo, "standard", 0, false, nil, nil)
	sCPU := scoreNode("cpu-node", states, hwInfo, "standard", 0, false, nil, nil)
	if sGPU >= sCPU {
		t.Errorf("CPU-only pod should prefer CPU node over GPU node: gpu=%d cpu=%d", sGPU, sCPU)
	}

	// GPU pod should NOT get the penalty
	sGPUPod := scoreNode("gpu-node", states, hwInfo, "standard", 0, true, nil, nil)
	if sGPUPod != sCPU {
		t.Errorf("GPU pod on GPU node should score same as CPU pod on CPU node: gpuPod=%d cpuOnCpu=%d", sGPUPod, sCPU)
	}
}

func TestComputePerfPressure(t *testing.T) {
	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"perf1": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 40,
			LastUpdated:                 now,
		},
		"perf2": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 20,
			LastUpdated:                 now,
		},
		"eco1": {
			SchedulableClass:            "eco",
			PredictedPowerHeadroomScore: 10,
			LastUpdated:                 now,
		},
	}
	pressure := computePerfPressure(states)
	expected := 70.0
	if pressure != expected {
		t.Errorf("expected perfPressure=%.1f, got %.1f", expected, pressure)
	}
}

func TestComputePerfPressureEmpty(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{}
	pressure := computePerfPressure(states)
	if pressure != 0 {
		t.Errorf("expected 0 pressure for empty states, got %.1f", pressure)
	}
}

func TestPodWorkloadClass(t *testing.T) {
	pod := PodSpec{}
	if podWorkloadClass(pod) != "standard" {
		t.Errorf("expected default 'standard', got %s", podWorkloadClass(pod))
	}

	pod.Metadata.Annotations = map[string]string{"joulie.io/workload-class": "performance"}
	if podWorkloadClass(pod) != "performance" {
		t.Errorf("expected 'performance', got %s", podWorkloadClass(pod))
	}
}

func TestPodRequestsGPU(t *testing.T) {
	pod := PodSpec{Spec: PodBody{Containers: []ContainerSpec{{}}}}
	if podRequestsGPU(pod) {
		t.Error("expected no GPU request")
	}

	pod.Spec.Containers[0].Resources.Requests = map[string]interface{}{"nvidia.com/gpu": "1"}
	if !podRequestsGPU(pod) {
		t.Error("expected GPU request detected")
	}
}

// --- Marginal power scoring tests ---

func TestScoreNodeMarginalCPUOnlyPrefersSmaller(t *testing.T) {
	// With marginal scoring, a CPU-only pod should score better on a smaller
	// CPU-only node than a large GPU node (lower marginal impact + idle GPU waste).
	savedEnabled := marginalScoringEnabled
	savedCoeff := marginalCoeff
	marginalScoringEnabled = true
	marginalCoeff = powerest.DefaultCoefficients()
	defer func() {
		marginalScoringEnabled = savedEnabled
		marginalCoeff = savedCoeff
	}()

	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"small-cpu": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 70,
			PredictedCoolingStressScore: 20,
			PredictedPsuStressScore:     15,
			LastUpdated:                 now,
		},
		"big-gpu": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 70,
			PredictedCoolingStressScore: 20,
			PredictedPsuStressScore:     15,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{
		"small-cpu": {CPUTotalCores: 32, CPUSockets: 1, CPUMaxWattsTotal: 270},
		"big-gpu":   {CPUTotalCores: 96, CPUSockets: 2, CPUMaxWattsTotal: 720, GPUPresent: true, GPUCount: 8, GPUMaxWattsPerGPU: 700},
	}
	demand := &powerest.PodDemand{CPUCores: 4, WorkloadClass: "standard"}

	sSmall := scoreNode("small-cpu", states, hwInfo, "standard", 0, false, demand, nil)
	sBig := scoreNode("big-gpu", states, hwInfo, "standard", 0, false, demand, nil)
	if sSmall <= sBig {
		t.Errorf("CPU-only pod should prefer small CPU node over big GPU node with marginal scoring: small=%d big=%d", sSmall, sBig)
	}
}

func TestScoreNodeMarginalGPUPodPrefersLowerImpact(t *testing.T) {
	savedEnabled := marginalScoringEnabled
	savedCoeff := marginalCoeff
	marginalScoringEnabled = true
	marginalCoeff = powerest.DefaultCoefficients()
	defer func() {
		marginalScoringEnabled = savedEnabled
		marginalCoeff = savedCoeff
	}()

	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"dense-gpu": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 70,
			PredictedCoolingStressScore: 20,
			PredictedPsuStressScore:     15,
			LastUpdated:                 now,
		},
		"small-gpu": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 70,
			PredictedCoolingStressScore: 20,
			PredictedPsuStressScore:     15,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{
		"dense-gpu": {CPUTotalCores: 96, CPUSockets: 2, CPUMaxWattsTotal: 720, GPUPresent: true, GPUCount: 8, GPUMaxWattsPerGPU: 700},
		"small-gpu": {CPUTotalCores: 32, CPUSockets: 1, CPUMaxWattsTotal: 270, GPUPresent: true, GPUCount: 2, GPUMaxWattsPerGPU: 350},
	}
	demand := &powerest.PodDemand{CPUCores: 4, GPUCount: 1, GPUVendor: "nvidia", WorkloadClass: "standard"}

	sSmall := scoreNode("small-gpu", states, hwInfo, "standard", 0, false, demand, nil)
	sDense := scoreNode("dense-gpu", states, hwInfo, "standard", 0, false, demand, nil)
	if sSmall <= sDense {
		t.Errorf("1-GPU pod should prefer smaller GPU node: small=%d dense=%d", sSmall, sDense)
	}
}

func TestScoreNodeMarginalDisabledPreservesLegacy(t *testing.T) {
	savedEnabled := marginalScoringEnabled
	marginalScoringEnabled = false
	defer func() { marginalScoringEnabled = savedEnabled }()

	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"gpu-node": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 80,
			PredictedCoolingStressScore: 10,
			PredictedPsuStressScore:     10,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{
		"gpu-node": {GPUPresent: true, GPUCount: 8},
	}
	// Even with podDemand provided, marginal scoring is off: legacy -30 penalty applies.
	demand := &powerest.PodDemand{CPUCores: 4, WorkloadClass: "standard"}
	score := scoreNode("gpu-node", states, hwInfo, "standard", 0, false, demand, nil)
	// base = 80*0.4 + 90*0.3 + 90*0.3 = 32+27+27 = 86, minus 30 = 56
	if score != 56 {
		t.Errorf("with marginal disabled, expected legacy score 56, got %d", score)
	}
}

func TestScoreNodeMarginalNoHardwarePreservesBase(t *testing.T) {
	savedEnabled := marginalScoringEnabled
	savedCoeff := marginalCoeff
	marginalScoringEnabled = true
	marginalCoeff = powerest.DefaultCoefficients()
	defer func() {
		marginalScoringEnabled = savedEnabled
		marginalCoeff = savedCoeff
	}()

	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"unknown-hw": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 80,
			PredictedCoolingStressScore: 10,
			PredictedPsuStressScore:     10,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{} // no hardware data
	demand := &powerest.PodDemand{CPUCores: 4, WorkloadClass: "standard"}

	score := scoreNode("unknown-hw", states, hwInfo, "standard", 0, false, demand, nil)
	// No hw data = no marginal penalty, no legacy penalty. base = 86.
	if score != 86 {
		t.Errorf("no hardware data should preserve base score 86, got %d", score)
	}
}

// --- parseNodeHardware ---

func TestParseNodeHardwareFromStatus(t *testing.T) {
	obj := unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"nodeName": "test-node",
			},
			"status": map[string]interface{}{
				"cpu": map[string]interface{}{
					"model":      "EPYC-9654",
					"totalCores": float64(96),
					"sockets":    float64(2),
					"capRange": map[string]interface{}{
						"maxWattsPerSocket": float64(360),
					},
				},
				"gpu": map[string]interface{}{
					"present": true,
					"model":   "H100-SXM",
					"vendor":  "nvidia",
					"count":   float64(8),
					"capRangePerGpu": map[string]interface{}{
						"maxWatts": float64(700),
					},
				},
				"memory": map[string]interface{}{
					"totalBytes": float64(1099511627776),
				},
			},
		},
	}
	name, info := parseNodeHardware(obj)
	if name != "test-node" {
		t.Errorf("expected node name 'test-node', got %q", name)
	}
	if info.CPUTotalCores != 96 {
		t.Errorf("expected 96 cores, got %d", info.CPUTotalCores)
	}
	if info.CPUSockets != 2 {
		t.Errorf("expected 2 sockets, got %d", info.CPUSockets)
	}
	if info.CPUMaxWattsTotal != 720 {
		t.Errorf("expected 720W CPU total, got %.0f", info.CPUMaxWattsTotal)
	}
	if !info.GPUPresent {
		t.Error("expected GPU present")
	}
	if info.GPUCount != 8 {
		t.Errorf("expected 8 GPUs, got %d", info.GPUCount)
	}
	if info.GPUMaxWattsPerGPU != 700 {
		t.Errorf("expected 700W per GPU, got %.0f", info.GPUMaxWattsPerGPU)
	}
	if info.GPUModel != "H100-SXM" {
		t.Errorf("expected GPU model H100-SXM, got %q", info.GPUModel)
	}
	if info.MemoryBytes != 1099511627776 {
		t.Errorf("expected memory 1TiB, got %d", info.MemoryBytes)
	}
}

func TestParseNodeHardwareNoStatus(t *testing.T) {
	obj := unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"nodeName": "empty-node",
			},
		},
	}
	name, info := parseNodeHardware(obj)
	if name != "empty-node" {
		t.Errorf("expected 'empty-node', got %q", name)
	}
	if info.GPUPresent || info.CPUTotalCores != 0 {
		t.Error("expected empty hw info for node with no status")
	}
}

// --- Eviction history tests ---

func TestScoreNodeEvictionHistoryPenalizesMatchingClass(t *testing.T) {
	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"eco-node": {
			SchedulableClass:            "eco",
			PredictedPowerHeadroomScore: 70,
			PredictedCoolingStressScore: 20,
			PredictedPsuStressScore:     20,
			LastUpdated:                 now,
		},
		"perf-node": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 70,
			PredictedCoolingStressScore: 20,
			PredictedPsuStressScore:     20,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{}

	// Pod was previously evicted from eco class
	eviction := &evictionInfo{fromClass: "eco", reason: "cooling_stress", evictedAt: now}

	sEco := scoreNode("eco-node", states, hwInfo, "standard", 0, false, nil, eviction)
	sPerf := scoreNode("perf-node", states, hwInfo, "standard", 0, false, nil, eviction)

	// eco node should be penalized (-25), performance node should not
	if sEco >= sPerf {
		t.Errorf("eco node should be penalized for pod evicted from eco: eco=%d perf=%d", sEco, sPerf)
	}
}

func TestScoreNodeEvictionHistoryNilHasNoEffect(t *testing.T) {
	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"node": {
			SchedulableClass:            "eco",
			PredictedPowerHeadroomScore: 70,
			PredictedCoolingStressScore: 20,
			PredictedPsuStressScore:     20,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{}

	sWithout := scoreNode("node", states, hwInfo, "standard", 0, false, nil, nil)
	sWithPerf := scoreNode("node", states, hwInfo, "standard", 0, false, nil, &evictionInfo{fromClass: "performance", reason: "test", evictedAt: now})

	// Eviction from "performance" should not affect an "eco" node
	if sWithout != sWithPerf {
		t.Errorf("eviction from different class should not affect score: without=%d with=%d", sWithout, sWithPerf)
	}
}
