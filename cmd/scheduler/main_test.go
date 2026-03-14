package main

import (
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

func TestScoreNodeNoState(t *testing.T) {
	states := map[string]*joulie.NodeTwinState{}
	score := scoreNode("node1", states, "", "", "")
	if score != 50 {
		t.Errorf("expected neutral score 50 when no state, got %d", score)
	}
}

func TestScoreNodeEcoHighHeadroom(t *testing.T) {
	states := map[string]*joulie.NodeTwinState{
		"node1": {
			NodeName:                    "node1",
			SchedulableClass:            "eco",
			PredictedPowerHeadroomScore: 80,
			PredictedCoolingStressScore: 10,
			PredictedPsuStressScore:     10,
			EffectiveCapState:           joulie.CapState{CPUPct: 60, GPUPct: 60},
		},
		"node2": {
			NodeName:                    "node2",
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
	states := map[string]*joulie.NodeTwinState{
		"draining-node": {
			NodeName:                    "draining-node",
			SchedulableClass:            "draining",
			PredictedPowerHeadroomScore: 80,
			PredictedCoolingStressScore: 10,
			PredictedPsuStressScore:     10,
		},
		"normal-node": {
			NodeName:                    "normal-node",
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
	states := map[string]*joulie.NodeTwinState{
		"eco-node": {NodeName: "eco-node", SchedulableClass: "eco"},
	}
	// Performance pod requires non-eco
	reason := shouldFilterNode("eco-node", states, nil, true)
	if reason == "" {
		t.Error("expected eco node to be filtered for performance pod")
	}
}

func TestFilterNodesStandardAccepted(t *testing.T) {
	states := map[string]*joulie.NodeTwinState{
		"eco-node": {NodeName: "eco-node", SchedulableClass: "eco"},
	}
	// Standard pod can go anywhere
	reason := shouldFilterNode("eco-node", states, nil, false)
	if reason != "" {
		t.Errorf("expected eco node to be accepted for standard pod, got: %s", reason)
	}
}
