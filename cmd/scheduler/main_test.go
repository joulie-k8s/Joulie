package main

import (
	"testing"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
)

func TestScoreNodeNoState(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{}
	score := scoreNode("node1", states, "", "", "")
	if score != 50 {
		t.Errorf("expected neutral score 50 when no state, got %d", score)
	}
}

func TestScoreNodeEcoHighHeadroom(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"node1": {
			SchedulableClass:            "eco",
			PredictedPowerHeadroomScore: 80,
			PredictedCoolingStressScore: 10,
			PredictedPsuStressScore:     10,
			EffectiveCapState:           joulie.CapState{CPUPct: 60, GPUPct: 60},
		},
		"node2": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 30,
			PredictedCoolingStressScore: 70,
			PredictedPsuStressScore:     70,
			EffectiveCapState:           joulie.CapState{CPUPct: 100, GPUPct: 100},
		},
	}
	// best-effort workload: eco preferred when headroom is better
	s1 := scoreNode("node1", states, "best-effort", "", "")
	s2 := scoreNode("node2", states, "best-effort", "", "")
	if s1 <= s2 {
		t.Errorf("eco node with high headroom should outscore stressed performance node for best-effort: %d vs %d", s1, s2)
	}
}

func TestScoreNodeDrainingPenalty(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"draining-node": {
			SchedulableClass:            "draining",
			PredictedPowerHeadroomScore: 80,
			PredictedCoolingStressScore: 10,
			PredictedPsuStressScore:     10,
		},
		"normal-node": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 80,
			PredictedCoolingStressScore: 10,
			PredictedPsuStressScore:     10,
		},
	}
	// draining node should score lower than identical non-draining node
	sDraining := scoreNode("draining-node", states, "standard", "", "")
	sNormal := scoreNode("normal-node", states, "standard", "", "")
	if sDraining >= sNormal {
		t.Errorf("draining node should score lower: draining=%d normal=%d", sDraining, sNormal)
	}
}

func TestFilterNodesEcoRejected(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"eco-node": {SchedulableClass: "eco"},
	}
	// Performance pod requires non-eco
	reason := shouldFilterNode("eco-node", states, nil, true)
	if reason == "" {
		t.Error("expected eco node to be filtered for performance pod")
	}
}

func TestFilterNodesStandardAccepted(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"eco-node": {SchedulableClass: "eco"},
	}
	// Standard pod can go anywhere
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
			LastUpdated:                 time.Now().Add(-10 * time.Minute), // 10 min ago = stale
		},
		"fresh-node": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 90,
			PredictedCoolingStressScore: 5,
			PredictedPsuStressScore:     5,
			LastUpdated:                 time.Now(), // just updated
		},
	}
	sStale := scoreNode("stale-node", states, "standard", "", "")
	sFresh := scoreNode("fresh-node", states, "standard", "", "")
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
	// Zero timestamp = operator hasn't set it yet; should not be treated as stale
	ts := &joulie.NodeTwinStatus{}
	if isTwinStale(ts) {
		t.Error("zero LastUpdated should not be treated as stale")
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

func TestFilterNodesDrainingNotFiltered(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"draining-node": {SchedulableClass: "draining"},
	}
	// Draining nodes are NOT filtered (they're penalized in scoring instead).
	// The node is transitioning to eco but hasn't completed - existing perf pods
	// need to finish, and filtering would prevent that.
	reason := shouldFilterNode("draining-node", states, nil, true)
	if reason != "" {
		t.Errorf("draining node should not be filtered (only penalized in scoring), got: %s", reason)
	}
}

func TestFilterNodesNoStateAccepted(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{}
	// No twin state = allow scheduling (don't block if operator hasn't run yet)
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
	score := scoreNode("ideal", states, "standard", "", "")
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
	score := scoreNode("stressed", states, "standard", "", "")
	if score != 0 {
		t.Errorf("expected minimum score 0 for max stress, got %d", score)
	}
}

func TestScoreNodeBestEffortEcoBonus(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"eco": {
			SchedulableClass:            "eco",
			PredictedPowerHeadroomScore: 50,
			PredictedCoolingStressScore: 30,
			PredictedPsuStressScore:     30,
			LastUpdated:                 time.Now(),
		},
		"perf": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 50,
			PredictedCoolingStressScore: 30,
			PredictedPsuStressScore:     30,
			LastUpdated:                 time.Now(),
		},
	}
	sEco := scoreNode("eco", states, "best-effort", "", "")
	sPerf := scoreNode("perf", states, "best-effort", "", "")
	if sEco <= sPerf {
		t.Errorf("eco node should get bonus for best-effort: eco=%d perf=%d", sEco, sPerf)
	}
}

func TestScoreNodeCapSensitivityWithFreshData(t *testing.T) {
	states := map[string]*joulie.NodeTwinStatus{
		"node1": {
			SchedulableClass:            "eco",
			PredictedPowerHeadroomScore: 60,
			PredictedCoolingStressScore: 20,
			PredictedPsuStressScore:     20,
			EffectiveCapState:           joulie.CapState{CPUPct: 100, GPUPct: 100},
			LastUpdated:                 time.Now(),
		},
	}
	// High CPU sensitivity should blend cap headroom into score
	sNormal := scoreNode("node1", states, "standard", "", "")
	sCPUSensitive := scoreNode("node1", states, "standard", "high", "")
	// With CPUPct=100, blending should increase score
	if sCPUSensitive < sNormal {
		t.Errorf("high cpu sensitivity with 100%% cap should increase score: normal=%d sensitive=%d", sNormal, sCPUSensitive)
	}
}
