package main

import (
	"context"
	"testing"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/kube"
	"github.com/matbun/joulie/pkg/scheduler/powerest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

// --- twinStatusFromObject powerMeasurement ---

func TestTwinStatusFromObjectPowerMeasurement(t *testing.T) {
	obj := &v1alpha1.NodeTwin{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Spec:       v1alpha1.NodeTwinSpec{NodeName: "test-node"},
		Status: v1alpha1.NodeTwinStatus{
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 75,
			PredictedCoolingStressScore: 20,
			LastUpdated:                 time.Now().Format(time.RFC3339),
			PowerMeasurement: &v1alpha1.PowerMeasurement{
				Source:             "kepler",
				MeasuredNodePowerW: 500,
				CPUCappedPowerW:    600,
				GPUCappedPowerW:    2800,
				NodeCappedPowerW:   3400,
				CPUTdpW:            720,
				GPUTdpW:            5600,
				NodeTdpW:           6320,
				PowerTrendWPerMin:  -50,
			},
		},
	}

	name, ts := twinStatusFromObject(obj)
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
	if ts.LastUpdated.IsZero() {
		t.Error("expected lastUpdated to be parsed from the RFC3339 string")
	}
}

func TestTwinStatusFromObjectNoPowerMeasurement(t *testing.T) {
	obj := &v1alpha1.NodeTwin{
		Spec:   v1alpha1.NodeTwinSpec{NodeName: "bare"},
		Status: v1alpha1.NodeTwinStatus{SchedulableClass: "eco"},
	}
	name, ts := twinStatusFromObject(obj)
	if name != "bare" {
		t.Errorf("expected 'bare', got %q", name)
	}
	if ts.PowerMeasurement != nil {
		t.Error("expected nil powerMeasurement when the status has none")
	}
	if !ts.LastUpdated.IsZero() {
		t.Error("expected zero lastUpdated when the status has none")
	}
}

// --- hwInfoFromObject ---

func TestHWInfoFromObjectFromStatus(t *testing.T) {
	obj := &v1alpha1.NodeHardware{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		Spec:       v1alpha1.NodeHardwareSpec{NodeName: "test-node"},
		Status: v1alpha1.NodeHardwareStatus{
			CPU: &v1alpha1.NodeHardwareCPU{
				Model:      "EPYC-9654",
				TotalCores: 96,
				Sockets:    2,
				CapRange:   &v1alpha1.CPUCapRange{MaxWattsPerSocket: 360},
			},
			GPU: &v1alpha1.NodeHardwareGPU{
				Present:        true,
				Model:          "H100-SXM",
				Vendor:         "nvidia",
				Count:          8,
				CapRangePerGPU: &v1alpha1.GPUCapRange{MaxWatts: 700},
			},
			Memory: &v1alpha1.NodeHardwareMemory{TotalBytes: 1099511627776},
		},
	}
	name, info := hwInfoFromObject(obj)
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

func TestHWInfoFromObjectNoStatus(t *testing.T) {
	obj := &v1alpha1.NodeHardware{
		Spec: v1alpha1.NodeHardwareSpec{NodeName: "empty-node"},
	}
	name, info := hwInfoFromObject(obj)
	if name != "empty-node" {
		t.Errorf("expected 'empty-node', got %q", name)
	}
	if info.GPUPresent || info.CPUTotalCores != 0 {
		t.Error("expected empty hw info for node with no status")
	}
}

// nodeTwinJSON is a NodeTwin as the API server returns it for a healthy
// 4-socket node: every value that happens to be whole is decoded as int64.
func nodeTwinJSON(lastUpdated time.Time) string {
	return `{
  "apiVersion": "joulie.io/v1alpha1",
  "kind": "NodeTwin",
  "metadata": {"name": "n2-atos"},
  "spec": {"nodeName": "n2-atos", "profile": "performance"},
  "status": {
    "schedulableClass": "performance",
    "predictedPowerHeadroomScore": 50,
    "predictedCoolingStressScore": 0,
    "hardwareDensityScore": 100,
    "estimatedPUE": 1.4,
    "effectiveCapState": {"cpuPct": 100, "gpuPct": 100},
    "powerMeasurement": {
      "source": "prometheus",
      "measuredNodePowerW": 330,
      "nodeCappedPowerW": 660,
      "cpuCappedPowerW": 660,
      "cpuTdpW": 660,
      "nodeTdpW": 660,
      "powerTrendWPerMin": 0
    },
    "lastUpdated": "` + lastUpdated.Format(time.RFC3339) + `"
  }
}`
}

// decodeNodeTwinJSON takes the JSON the API server would return and decodes
// it the way the informer does: JSON to an unstructured map (whole numbers
// stay int64 here) and then through the unstructured converter into the
// typed NodeTwin, which is where int64 must become float64.
func decodeNodeTwinJSON(t *testing.T, raw string) *v1alpha1.NodeTwin {
	t.Helper()
	u := unstructured.Unstructured{}
	if err := u.UnmarshalJSON([]byte(raw)); err != nil {
		t.Fatalf("decode NodeTwin JSON: %v", err)
	}
	nt := &v1alpha1.NodeTwin{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, nt); err != nil {
		t.Fatalf("convert NodeTwin to typed: %v", err)
	}
	return nt
}

func TestTwinStatusFromObjectAcceptsWholeNumbers(t *testing.T) {
	nt := decodeNodeTwinJSON(t, nodeTwinJSON(time.Now().UTC()))

	nodeName, ts := twinStatusFromObject(nt)
	if nodeName != "n2-atos" {
		t.Fatalf("nodeName=%q want=n2-atos", nodeName)
	}
	if ts.PredictedPowerHeadroomScore != 50 {
		t.Fatalf("headroom=%v want=50 (int64 from the API must be accepted)", ts.PredictedPowerHeadroomScore)
	}
	if ts.EffectiveCapState.CPUPct != 100 || ts.EffectiveCapState.GPUPct != 100 {
		t.Fatalf("capState=%+v want cpu/gpu 100", ts.EffectiveCapState)
	}
	if ts.HardwareDensityScore != 100 {
		t.Fatalf("density=%v want=100", ts.HardwareDensityScore)
	}
	if ts.PowerMeasurement == nil {
		t.Fatal("powerMeasurement missing")
	}
	if ts.PowerMeasurement.MeasuredNodePowerW != 330 {
		t.Fatalf("measured=%v want=330", ts.PowerMeasurement.MeasuredNodePowerW)
	}
	if ts.PowerMeasurement.NodeCappedPowerW != 660 || ts.PowerMeasurement.NodeTdpW != 660 {
		t.Fatalf("capped=%v tdp=%v want 660/660", ts.PowerMeasurement.NodeCappedPowerW, ts.PowerMeasurement.NodeTdpW)
	}
}

func TestScoreNodeUsesWholeNumberTwinFields(t *testing.T) {
	nt := decodeNodeTwinJSON(t, nodeTwinJSON(time.Now().UTC()))
	nodeName, ts := twinStatusFromObject(nt)
	states := map[string]*joulie.NodeTwinStatus{nodeName: ts}

	// 330 W drawn against a 660 W budget is half the budget left:
	// 50 * 0.7 + (100 - 0) * 0.15 = 50.
	got := scoreNode(nodeName, states, map[string]nodeHWInfo{}, "standard", 0, 0, nil)
	if got != 50 {
		t.Fatalf("score=%d want=50 (dropped twin fields collapse this to 15)", got)
	}
}

// --- reader-backed maps ---

// resetReaderState points the package-level reader at r and clears the
// memoized maps, restoring everything when the test ends. The state is
// package-level, so tests using this must not call t.Parallel().
func resetReaderState(t *testing.T, r kube.Reader) {
	t.Helper()
	prevReader := reader
	twinStateMu.Lock()
	prevTwin, prevTwinAt := twinStateCache, lastCacheRefresh
	twinStateCache, lastCacheRefresh = nil, time.Time{}
	twinStateMu.Unlock()
	nodeHWMu.Lock()
	prevHW, prevHWAt := nodeHWCache, lastNodeHWRefresh
	nodeHWCache, lastNodeHWRefresh = nil, time.Time{}
	nodeHWMu.Unlock()
	setReader(r)
	t.Cleanup(func() {
		setReader(prevReader)
		twinStateMu.Lock()
		twinStateCache, lastCacheRefresh = prevTwin, prevTwinAt
		twinStateMu.Unlock()
		nodeHWMu.Lock()
		nodeHWCache, lastNodeHWRefresh = prevHW, prevHWAt
		nodeHWMu.Unlock()
	})
}

func TestReaderBackedMaps(t *testing.T) {
	s, err := kube.NewScheme(v1alpha1.AddToScheme)
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	twinA := &v1alpha1.NodeTwin{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Spec:       v1alpha1.NodeTwinSpec{NodeName: "node-a", Profile: "performance"},
		Status: v1alpha1.NodeTwinStatus{
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 60,
			LastUpdated:                 now,
			PowerMeasurement:            &v1alpha1.PowerMeasurement{MeasuredNodePowerW: 400, NodeCappedPowerW: 1000},
		},
	}
	twinB := &v1alpha1.NodeTwin{
		ObjectMeta: metav1.ObjectMeta{Name: "node-b"},
		Spec:       v1alpha1.NodeTwinSpec{NodeName: "node-b", Profile: "eco"},
		Status:     v1alpha1.NodeTwinStatus{SchedulableClass: "eco", LastUpdated: now},
	}
	hwA := &v1alpha1.NodeHardware{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Spec:       v1alpha1.NodeHardwareSpec{NodeName: "node-a"},
		Status: v1alpha1.NodeHardwareStatus{
			CPU: &v1alpha1.NodeHardwareCPU{TotalCores: 64, Sockets: 2, CapRange: &v1alpha1.CPUCapRange{MaxWattsPerSocket: 300}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(twinA, twinB, hwA).Build()
	resetReaderState(t, c)

	ctx := context.Background()
	states := getNodeTwinStates(ctx)
	if len(states) != 2 {
		t.Fatalf("twin states=%d want=2: %v", len(states), states)
	}
	if st := states["node-a"]; st == nil || st.SchedulableClass != "performance" || st.PowerMeasurement == nil || st.PowerMeasurement.NodeCappedPowerW != 1000 {
		t.Fatalf("node-a state=%+v", st)
	}
	if st := states["node-b"]; st == nil || st.SchedulableClass != "eco" || st.PowerMeasurement != nil {
		t.Fatalf("node-b state=%+v", st)
	}

	hw := getNodeHardwareInfo(ctx)
	if len(hw) != 1 {
		t.Fatalf("hw infos=%d want=1: %v", len(hw), hw)
	}
	if got := hw["node-a"]; got.CPUTotalCores != 64 || got.CPUMaxWattsTotal != 600 {
		t.Fatalf("node-a hw=%+v want cores=64 cpuMaxW=600", got)
	}

	// Within CACHE_TTL the memoized map is returned as is: a new object in
	// the reader is not visible until the next rebuild.
	if err := c.Create(ctx, &v1alpha1.NodeTwin{
		ObjectMeta: metav1.ObjectMeta{Name: "node-c"},
		Spec:       v1alpha1.NodeTwinSpec{NodeName: "node-c", Profile: "eco"},
	}); err != nil {
		t.Fatalf("create node-c: %v", err)
	}
	if again := getNodeTwinStates(ctx); len(again) != 2 {
		t.Fatalf("memoized twin states=%d want=2 within CACHE_TTL", len(again))
	}
}

func TestReaderNilReturnsEmptyMaps(t *testing.T) {
	resetReaderState(t, nil)
	ctx := context.Background()
	if got := getNodeTwinStates(ctx); len(got) != 0 {
		t.Fatalf("twin states=%d want=0 without Kubernetes", len(got))
	}
	if got := getNodeHardwareInfo(ctx); len(got) != 0 {
		t.Fatalf("hw infos=%d want=0 without Kubernetes", len(got))
	}
}
