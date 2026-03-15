//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/matbun/joulie/tests/integration/helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IT-SCHED-01: Scheduler filter respects eco/draining nodes.
// This test uses the scheduler extender logic directly (unit-level integration).
func TestIT_SCHED_01_FilterEcoDraining(t *testing.T) {
	// These tests run inline (no cluster needed) to test filter logic correctness.
	// Full cluster-level filter tests require the extender deployed and scheduler configured.

	t.Run("eco node rejected for non-eco pod", func(t *testing.T) {
		// This mirrors the unit test in cmd/scheduler/main_test.go
		// Running it here as part of integration suite for completeness.
		result := shouldFilterNode_IT("eco-node",
			map[string]*twinStateIT{"eco-node": {SchedulableClass: "eco"}},
			nil, true, false)
		if result == "" {
			t.Error("expected eco node to be filtered for non-eco pod")
		}
	})

	t.Run("draining node rejected", func(t *testing.T) {
		result := shouldFilterNode_IT("drain-node",
			map[string]*twinStateIT{"drain-node": {SchedulableClass: "draining"}},
			nil, false, false)
		if result == "" {
			t.Error("expected draining node to be filtered")
		}
	})

	t.Run("eco node accepted for eco-tolerating pod", func(t *testing.T) {
		result := shouldFilterNode_IT("eco-node",
			map[string]*twinStateIT{"eco-node": {SchedulableClass: "eco"}},
			nil, false, false)
		if result != "" {
			t.Errorf("expected eco node to be accepted: %s", result)
		}
	})
}

// IT-SCHED-02: Scheduler scoring uses NodeTwin values.
func TestIT_SCHED_02_ScoringDeterminism(t *testing.T) {
	states := map[string]*twinStateIT{
		"high-headroom": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 90,
			PredictedCoolingStressScore: 10,
			PredictedPsuStressScore:     5,
		},
		"low-headroom": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 20,
			PredictedCoolingStressScore: 80,
			PredictedPsuStressScore:     75,
		},
	}

	s1 := scoreNode_IT("high-headroom", states, "standard", "", "")
	s2 := scoreNode_IT("low-headroom", states, "standard", "", "")
	if s1 <= s2 {
		t.Errorf("high-headroom node should score higher than stressed node: %d vs %d", s1, s2)
	}

	// Changing NodeTwin values should change the score deterministically
	states["high-headroom"].PredictedCoolingStressScore = 90
	s1b := scoreNode_IT("high-headroom", states, "standard", "", "")
	if s1b >= s1 {
		t.Errorf("increasing cooling stress should decrease score: before=%d after=%d", s1, s1b)
	}
}

// IT-FSM-01: Existing FSM transitions still work with new architecture.
func TestIT_FSM_01_ExistingFSMWithNodeTwin(t *testing.T) {
	kubeconfig := os.Getenv("KUBECONFIG")
	clients, err := helpers.NewClients(kubeconfig)
	if err != nil {
		t.Skipf("no kubeconfig available: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Find a managed node
	nodes, err := clients.K8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "joulie.io/managed=true",
	})
	if err != nil || len(nodes.Items) == 0 {
		t.Skip("no managed nodes found")
	}
	nodeName := nodes.Items[0].Name

	// Create a NodeTwin with eco profile
	nt := map[string]interface{}{
		"apiVersion": "joulie.io/v1alpha1",
		"kind":       "NodeTwin",
		"metadata":   map[string]interface{}{"name": nodeName},
		"spec": map[string]interface{}{
			"nodeName": nodeName,
			"profile":  "eco",
			"cpu":      map[string]interface{}{"packagePowerCapPctOfMax": float64(60)},
		},
	}
	ntBytes, _ := json.Marshal(nt)
	ntObj := helpers.MustParseCR(t, string(ntBytes))
	if err := helpers.ApplyUnstructured(ctx, clients.Dynamic, helpers.JoulieGVRs["nodetwins"], "", ntObj); err != nil {
		t.Logf("apply NodeTwin: %v", err)
	}

	// Wait for node label to be set to eco
	helpers.AssertNodeLabel(ctx, t, clients.K8s, nodeName, "joulie.io/power-profile", "eco")
}

// These types/functions mirror the scheduler extender for inline integration testing.
// We duplicate minimal logic here rather than importing cmd/scheduler (which is package main).

type twinStateIT struct {
	SchedulableClass            string
	PredictedPowerHeadroomScore float64
	PredictedCoolingStressScore float64
	PredictedPsuStressScore     float64
	EffectiveCapStateCPUPct     float64
	EffectiveCapStateGPUPct     float64
}

func shouldFilterNode_IT(nodeName string, states map[string]*twinStateIT, labels map[string]string, podRequiresNonEco, podTolerateDraining bool) string {
	state, hasState := states[nodeName]
	if hasState && state.SchedulableClass == "draining" && !podTolerateDraining {
		return "joulie: node is draining"
	}
	if hasState && state.SchedulableClass == "eco" && podRequiresNonEco {
		return "joulie: pod requires non-eco but node is eco"
	}
	if labels != nil {
		if labels["joulie.io/power-profile"] == "eco" && podRequiresNonEco {
			return "joulie: eco via label"
		}
	}
	return ""
}

func scoreNode_IT(nodeName string, states map[string]*twinStateIT, wpClass, cpuSens, gpuSens string) int64 {
	state, ok := states[nodeName]
	if !ok {
		return 50
	}
	score := state.PredictedPowerHeadroomScore*0.4 +
		(100-state.PredictedCoolingStressScore)*0.3 +
		(100-state.PredictedPsuStressScore)*0.3
	if wpClass == "performance" && state.SchedulableClass == "eco" {
		score *= 0.5
	}
	if wpClass == "best-effort" && state.SchedulableClass == "eco" {
		score += 5
	}
	if score > 100 {
		score = 100
	}
	if score < 0 {
		score = 0
	}
	return int64(score)
}
