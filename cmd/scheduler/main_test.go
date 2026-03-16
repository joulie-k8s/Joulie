package main

import (
	"testing"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
)

func TestScoreNodeNoState(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{}
	hwInfo := map[string]nodeHWInfo{}
	score := scoreNode("node1", states, hwInfo, "standard", 0, false)
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
	// Standard workload on eco node with high headroom should outscore stressed perf node
	s1 := scoreNode("node1", states, hwInfo, "standard", 0, false)
	s2 := scoreNode("node2", states, hwInfo, "standard", 0, false)
	if s1 <= s2 {
		t.Errorf("eco node with high headroom should outscore stressed performance node for standard: %d vs %d", s1, s2)
	}
}

func TestScoreNodeDrainingFiltered(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"draining-node": {SchedulableClass: "draining"},
	}
	// Draining nodes ARE filtered for performance pods (same as eco).
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
	sStale := scoreNode("stale-node", states, hwInfo, "standard", 0, false)
	sFresh := scoreNode("fresh-node", states, hwInfo, "standard", 0, false)
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
	// Performance pods are rejected from draining nodes.
	reason := shouldFilterNode("draining-node", states, nil, true)
	if reason == "" {
		t.Error("draining node should be filtered for performance pod")
	}
	// Standard pods can still land on draining nodes.
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
	score := scoreNode("ideal", states, hwInfo, "standard", 0, false)
	// headroom*0.4 + (100-0)*0.3 + (100-0)*0.3 = 40+30+30 = 100
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
	score := scoreNode("stressed", states, hwInfo, "standard", 0, false)
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

	// With high perf pressure, standard pods should prefer eco over performance
	highPressure := 80.0
	sPerfNode := scoreNode("perf-node", states, hwInfo, "standard", highPressure, false)
	sEcoNode := scoreNode("eco-node", states, hwInfo, "standard", highPressure, false)
	if sEcoNode <= sPerfNode {
		t.Errorf("standard pod under high pressure should prefer eco node: eco=%d perf=%d", sEcoNode, sPerfNode)
	}

	// With zero pressure, both should score the same
	sPerfNoPressure := scoreNode("perf-node", states, hwInfo, "standard", 0, false)
	sEcoNoPressure := scoreNode("eco-node", states, hwInfo, "standard", 0, false)
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

	// CPU-only pod should prefer CPU-only node over GPU node
	sGPU := scoreNode("gpu-node", states, hwInfo, "standard", 0, false)
	sCPU := scoreNode("cpu-node", states, hwInfo, "standard", 0, false)
	if sGPU >= sCPU {
		t.Errorf("CPU-only pod should prefer CPU node over GPU node: gpu=%d cpu=%d", sGPU, sCPU)
	}

	// GPU pod should NOT get the penalty
	sGPUPod := scoreNode("gpu-node", states, hwInfo, "standard", 0, true)
	if sGPUPod != sCPU {
		t.Errorf("GPU pod on GPU node should score same as CPU pod on CPU node: gpuPod=%d cpuOnCpu=%d", sGPUPod, sCPU)
	}
}

func TestComputePerfPressure(t *testing.T) {
	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"perf1": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 40, // pressure = 60
			LastUpdated:                 now,
		},
		"perf2": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 20, // pressure = 80
			LastUpdated:                 now,
		},
		"eco1": {
			SchedulableClass:            "eco",
			PredictedPowerHeadroomScore: 10, // should be ignored
			LastUpdated:                 now,
		},
	}
	pressure := computePerfPressure(states)
	expected := 70.0 // (60+80)/2
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
	// Default
	pod := PodSpec{}
	if podWorkloadClass(pod) != "standard" {
		t.Errorf("expected default 'standard', got %s", podWorkloadClass(pod))
	}

	// Annotated
	pod.Metadata.Annotations = map[string]string{"joulie.io/workload-class": "performance"}
	if podWorkloadClass(pod) != "performance" {
		t.Errorf("expected 'performance', got %s", podWorkloadClass(pod))
	}
}

func TestPodRequestsGPU(t *testing.T) {
	// No GPU
	pod := PodSpec{Spec: PodBody{Containers: []ContainerSpec{{}}}}
	if podRequestsGPU(pod) {
		t.Error("expected no GPU request")
	}

	// GPU in requests
	pod.Spec.Containers[0].Resources.Requests = map[string]interface{}{"nvidia.com/gpu": "1"}
	if !podRequestsGPU(pod) {
		t.Error("expected GPU request detected")
	}
}
