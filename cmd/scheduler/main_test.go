package main

import (
	"testing"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/scheduler/powerest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// --- Twin-based scoring tests ---

func TestScoreNodeNoState(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{}
	hwInfo := map[string]nodeHWInfo{}
	score := scoreNode("node1", states, hwInfo, "standard", 0, 0, nil)
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
			LastUpdated:                 now,
		},
		"node2": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 30,
			PredictedCoolingStressScore: 70,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{}
	s1 := scoreNode("node1", states, hwInfo, "standard", 0, 0, nil)
	s2 := scoreNode("node2", states, hwInfo, "standard", 0, 0, nil)
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
			LastUpdated:                 time.Now().Add(-10 * time.Minute),
		},
		"fresh-node": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 90,
			PredictedCoolingStressScore: 5,
			LastUpdated:                 time.Now(),
		},
	}
	hwInfo := map[string]nodeHWInfo{}
	sStale := scoreNode("stale-node", states, hwInfo, "standard", 0, 0, nil)
	sFresh := scoreNode("fresh-node", states, hwInfo, "standard", 0, 0, nil)
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
			LastUpdated:                 time.Now(),
		},
	}
	hwInfo := map[string]nodeHWInfo{}
	score := scoreNode("ideal", states, hwInfo, "standard", 0, 0, nil)
	// score = 100*0.7 + (100-0)*0.15 = 70 + 15 = 85
	if score != 85 {
		t.Errorf("expected score 85 for ideal node (headroom*0.7 + cooling*0.15), got %d", score)
	}
}

func TestScoreNodeAllMaxStress(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"stressed": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 0,
			PredictedCoolingStressScore: 100,
			LastUpdated:                 time.Now(),
		},
	}
	hwInfo := map[string]nodeHWInfo{}
	score := scoreNode("stressed", states, hwInfo, "standard", 0, 0, nil)
	// score = 0*0.7 + (100-100)*0.15 = 0
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
			LastUpdated:                 now,
		},
		"eco-node": {
			SchedulableClass:            "eco",
			PredictedPowerHeadroomScore: 50,
			PredictedCoolingStressScore: 30,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{}

	highPressure := 80.0
	sPerfNode := scoreNode("perf-node", states, hwInfo, "standard", highPressure, 0, nil)
	sEcoNode := scoreNode("eco-node", states, hwInfo, "standard", highPressure, 0, nil)
	if sEcoNode <= sPerfNode {
		t.Errorf("standard pod under high pressure should prefer eco node: eco=%d perf=%d", sEcoNode, sPerfNode)
	}

	sPerfNoPressure := scoreNode("perf-node", states, hwInfo, "standard", 0, 0, nil)
	sEcoNoPressure := scoreNode("eco-node", states, hwInfo, "standard", 0, 0, nil)
	if sEcoNoPressure <= sPerfNoPressure {
		t.Errorf("with zero pressure, standard pod should prefer eco node (profile bonus): eco=%d perf=%d", sEcoNoPressure, sPerfNoPressure)
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

// --- Scoring with powerMeasurement ---

func TestScoreNodeWithPowerMeasurement(t *testing.T) {
	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"node1": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 50, // fallback, should be overridden
			PredictedCoolingStressScore: 20,
			LastUpdated:                 now,
			PowerMeasurement: &joulie.PowerMeasurement{
				MeasuredNodePowerW: 300,
				NodeCappedPowerW:   1000,
				NodeTdpW:           1200,
				PowerTrendWPerMin:  0,
			},
		},
	}
	hwInfo := map[string]nodeHWInfo{}
	demand := &powerest.PodDemand{CPUCores: 4, WorkloadClass: "standard"}

	// Without hardware info, no marginal power, so projectedPower = measuredPower = 300
	// headroomScore = (1000-300)/1000*100 = 70
	// score = 70*0.7 + (100-20)*0.15 + 0 = 49 + 12 = 61
	score := scoreNode("node1", states, hwInfo, "standard", 0, 0, demand)
	if score != 61 {
		t.Errorf("expected score 61, got %d", score)
	}
}

func TestScoreNodeWithPowerMeasurementAndMarginal(t *testing.T) {
	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"node1": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 50,
			PredictedCoolingStressScore: 0,
			LastUpdated:                 now,
			PowerMeasurement: &joulie.PowerMeasurement{
				MeasuredNodePowerW: 200,
				NodeCappedPowerW:   1000,
				NodeTdpW:           1200,
				PowerTrendWPerMin:  0,
			},
		},
	}
	hwInfo := map[string]nodeHWInfo{
		"node1": {CPUTotalCores: 96, CPUSockets: 2, CPUMaxWattsTotal: 720},
	}
	demand := &powerest.PodDemand{CPUCores: 8, WorkloadClass: "standard"}

	score := scoreNode("node1", states, hwInfo, "standard", 0, 0, demand)
	// Marginal power subtracts from headroom, so score should be less than without marginal.
	scoreNoMarginal := scoreNode("node1", states, hwInfo, "standard", 0, 0, nil)
	if score >= scoreNoMarginal {
		t.Errorf("marginal power should reduce score: with=%d without=%d", score, scoreNoMarginal)
	}
}

func TestScoreNodeWithTrendBonus(t *testing.T) {
	now := time.Now()
	risingState := &joulie.NodeTwinStatus{
		SchedulableClass:            "performance",
		PredictedPowerHeadroomScore: 50,
		PredictedCoolingStressScore: 20,
		LastUpdated:                 now,
		PowerMeasurement: &joulie.PowerMeasurement{
			MeasuredNodePowerW: 500,
			NodeCappedPowerW:   1000,
			NodeTdpW:           1200,
			PowerTrendWPerMin:  120, // rising fast: 120/6.0 = 20 → trendBonus = -20
		},
	}
	fallingState := &joulie.NodeTwinStatus{
		SchedulableClass:            "performance",
		PredictedPowerHeadroomScore: 50,
		PredictedCoolingStressScore: 20,
		LastUpdated:                 now,
		PowerMeasurement: &joulie.PowerMeasurement{
			MeasuredNodePowerW: 500,
			NodeCappedPowerW:   1000,
			NodeTdpW:           1200,
			PowerTrendWPerMin:  -120, // falling fast: -120/6.0 = -20 → trendBonus = +20
		},
	}

	states := map[string]*joulie.NodeTwinStatus{
		"rising":  risingState,
		"falling": fallingState,
	}
	hwInfo := map[string]nodeHWInfo{}

	sRising := scoreNode("rising", states, hwInfo, "standard", 0, 0, nil)
	sFalling := scoreNode("falling", states, hwInfo, "standard", 0, 0, nil)

	if sFalling <= sRising {
		t.Errorf("falling power trend should score higher: falling=%d rising=%d", sFalling, sRising)
	}
}

// --- Marginal power scoring ---

func TestScoreNodeMarginalCPUOnlyDeltaScaling(t *testing.T) {
	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"small-cpu": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 70,
			PredictedCoolingStressScore: 20,
			LastUpdated:                 now,
		},
		"big-gpu": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 70,
			PredictedCoolingStressScore: 20,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{
		"small-cpu": {CPUTotalCores: 32, CPUSockets: 1, CPUMaxWattsTotal: 270},
		"big-gpu":   {CPUTotalCores: 96, CPUSockets: 2, CPUMaxWattsTotal: 720, GPUPresent: true, GPUCount: 8, GPUMaxWattsPerGPU: 700},
	}
	demand := &powerest.PodDemand{CPUCores: 4, WorkloadClass: "standard"}

	sSmall := scoreNode("small-cpu", states, hwInfo, "standard", 0, 0, demand)
	sBig := scoreNode("big-gpu", states, hwInfo, "standard", 0, 0, demand)
	// Without powerMeasurement, headroom comes from twin. Both have same headroom=70.
	// Marginal delta only adjusts if powerMeasurement is present.
	// Without it, both should score similarly.
	if sSmall < sBig-5 || sSmall > sBig+5 {
		t.Errorf("without powerMeasurement, scores should be similar: small=%d big=%d", sSmall, sBig)
	}
}

func TestScoreNodeMarginalGPUPodPrefersLowerImpact(t *testing.T) {
	now := time.Now()
	// With powerMeasurement, marginal is subtracted from headroom.
	states := map[string]*joulie.NodeTwinStatus{
		"dense-gpu": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 70,
			PredictedCoolingStressScore: 20,
			LastUpdated:                 now,
			PowerMeasurement: &joulie.PowerMeasurement{
				MeasuredNodePowerW: 1000,
				NodeCappedPowerW:   5000,
				NodeTdpW:           6000,
			},
		},
		"small-gpu": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 70,
			PredictedCoolingStressScore: 20,
			LastUpdated:                 now,
			PowerMeasurement: &joulie.PowerMeasurement{
				MeasuredNodePowerW: 200,
				NodeCappedPowerW:   1000,
				NodeTdpW:           1200,
			},
		},
	}
	hwInfo := map[string]nodeHWInfo{
		"dense-gpu": {CPUTotalCores: 96, CPUSockets: 2, CPUMaxWattsTotal: 720, GPUPresent: true, GPUCount: 8, GPUMaxWattsPerGPU: 700},
		"small-gpu": {CPUTotalCores: 32, CPUSockets: 1, CPUMaxWattsTotal: 270, GPUPresent: true, GPUCount: 2, GPUMaxWattsPerGPU: 350},
	}
	demand := &powerest.PodDemand{CPUCores: 4, GPUCount: 1, GPUVendor: "nvidia", WorkloadClass: "standard"}

	sSmall := scoreNode("small-gpu", states, hwInfo, "standard", 0, 0, demand)
	sDense := scoreNode("dense-gpu", states, hwInfo, "standard", 0, 0, demand)
	// The dense node has more headroom (4000W remaining vs 800W), so it should score higher.
	if sDense <= sSmall {
		t.Errorf("dense GPU node with more absolute headroom should score higher: dense=%d small=%d", sDense, sSmall)
	}
}

func TestScoreNodeNoHardwarePreservesBase(t *testing.T) {
	now := time.Now()
	states := map[string]*joulie.NodeTwinStatus{
		"unknown-hw": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 80,
			PredictedCoolingStressScore: 10,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfo{} // no hardware data
	demand := &powerest.PodDemand{CPUCores: 4, WorkloadClass: "standard"}

	score := scoreNode("unknown-hw", states, hwInfo, "standard", 0, 0, demand)
	// No hw data = no marginal. headroom=80, coolingStress=10, no trend.
	// score = 80*0.7 + (100-10)*0.15 = 56 + 13.5 = 69.5 → 69
	if score != 69 {
		t.Errorf("no hardware data should give base score 69, got %d", score)
	}
}

// --- parseTwinState powerMeasurement ---

func TestParseTwinStatePowerMeasurement(t *testing.T) {
	obj := unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"nodeName": "test-node",
			},
			"status": map[string]interface{}{
				"schedulableClass":            "performance",
				"predictedPowerHeadroomScore": float64(75),
				"predictedCoolingStressScore": float64(20),
				"lastUpdated":                time.Now().Format(time.RFC3339),
				"powerMeasurement": map[string]interface{}{
					"source":             "kepler",
					"measuredNodePowerW": float64(500),
					"cpuCappedPowerW":    float64(600),
					"gpuCappedPowerW":    float64(2800),
					"nodeCappedPowerW":   float64(3400),
					"cpuTdpW":            float64(720),
					"gpuTdpW":            float64(5600),
					"nodeTdpW":           float64(6320),
					"powerTrendWPerMin":  float64(-50),
				},
			},
		},
	}

	name, ts := parseTwinState(obj)
	if name != "test-node" {
		t.Errorf("expected node name 'test-node', got %q", name)
	}
	if ts.PowerMeasurement == nil {
		t.Fatal("expected powerMeasurement to be parsed")
	}
	pm := ts.PowerMeasurement
	if pm.Source != "kepler" {
		t.Errorf("expected source 'kepler', got %q", pm.Source)
	}
	if pm.MeasuredNodePowerW != 500 {
		t.Errorf("expected measuredNodePowerW 500, got %.0f", pm.MeasuredNodePowerW)
	}
	if pm.NodeCappedPowerW != 3400 {
		t.Errorf("expected nodeCappedPowerW 3400, got %.0f", pm.NodeCappedPowerW)
	}
	if pm.NodeTdpW != 6320 {
		t.Errorf("expected nodeTdpW 6320, got %.0f", pm.NodeTdpW)
	}
	if pm.PowerTrendWPerMin != -50 {
		t.Errorf("expected powerTrendWPerMin -50, got %.0f", pm.PowerTrendWPerMin)
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
