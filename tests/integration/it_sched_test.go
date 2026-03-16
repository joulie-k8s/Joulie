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

	t.Run("eco node rejected for performance pod", func(t *testing.T) {
		result := shouldFilterNode_IT("eco-node",
			map[string]*twinStateIT{"eco-node": {SchedulableClass: "eco"}},
			nil, true)
		if result == "" {
			t.Error("expected eco node to be filtered for performance pod")
		}
	})

	t.Run("draining node rejected for performance pod", func(t *testing.T) {
		result := shouldFilterNode_IT("drain-node",
			map[string]*twinStateIT{"drain-node": {SchedulableClass: "draining"}},
			nil, true)
		if result == "" {
			t.Error("expected draining node to be filtered for performance pod")
		}
	})

	t.Run("eco node accepted for standard pod", func(t *testing.T) {
		result := shouldFilterNode_IT("eco-node",
			map[string]*twinStateIT{"eco-node": {SchedulableClass: "eco"}},
			nil, false)
		if result != "" {
			t.Errorf("expected eco node to be accepted for standard pod: %s", result)
		}
	})

	t.Run("draining node accepted for standard pod", func(t *testing.T) {
		result := shouldFilterNode_IT("drain-node",
			map[string]*twinStateIT{"drain-node": {SchedulableClass: "draining"}},
			nil, false)
		if result != "" {
			t.Errorf("expected draining node to be accepted for standard pod: %s", result)
		}
	})

	t.Run("unknown node accepted for any pod", func(t *testing.T) {
		result := shouldFilterNode_IT("new-node",
			map[string]*twinStateIT{},
			nil, true)
		if result != "" {
			t.Errorf("expected unknown node to be accepted: %s", result)
		}
	})
}

// IT-SCHED-02: Scheduler scoring uses NodeTwin values with adaptive pressure.
func TestIT_SCHED_02_ScoringDeterminism(t *testing.T) {
	now := time.Now()
	states := map[string]*twinStateIT{
		"high-headroom": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 90,
			PredictedCoolingStressScore: 10,
			PredictedPsuStressScore:     5,
			LastUpdated:                 now,
		},
		"low-headroom": {
			SchedulableClass:            "performance",
			PredictedPowerHeadroomScore: 20,
			PredictedCoolingStressScore: 80,
			PredictedPsuStressScore:     75,
			LastUpdated:                 now,
		},
	}
	hwInfo := map[string]nodeHWInfoIT{}

	s1 := scoreNode_IT("high-headroom", states, hwInfo, "standard", 0, false)
	s2 := scoreNode_IT("low-headroom", states, hwInfo, "standard", 0, false)
	if s1 <= s2 {
		t.Errorf("high-headroom node should score higher than stressed node: %d vs %d", s1, s2)
	}

	// Changing NodeTwin values should change the score deterministically
	states["high-headroom"].PredictedCoolingStressScore = 90
	s1b := scoreNode_IT("high-headroom", states, hwInfo, "standard", 0, false)
	if s1b >= s1 {
		t.Errorf("increasing cooling stress should decrease score: before=%d after=%d", s1, s1b)
	}
}

// IT-SCHED-03: Adaptive pressure relief steers standard pods away from congested perf nodes.
func TestIT_SCHED_03_AdaptivePressureRelief(t *testing.T) {
	now := time.Now()
	states := map[string]*twinStateIT{
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
	hwInfo := map[string]nodeHWInfoIT{}

	// With high perf pressure, standard pods should prefer eco
	highPressure := 80.0
	sPerf := scoreNode_IT("perf-node", states, hwInfo, "standard", highPressure, false)
	sEco := scoreNode_IT("eco-node", states, hwInfo, "standard", highPressure, false)
	if sEco <= sPerf {
		t.Errorf("standard pod under high pressure should prefer eco: eco=%d perf=%d", sEco, sPerf)
	}

	// With zero pressure, both should score the same
	sPerfNoPressure := scoreNode_IT("perf-node", states, hwInfo, "standard", 0, false)
	sEcoNoPressure := scoreNode_IT("eco-node", states, hwInfo, "standard", 0, false)
	if sPerfNoPressure != sEcoNoPressure {
		t.Errorf("with zero pressure, same-stats nodes should score equally: perf=%d eco=%d", sPerfNoPressure, sEcoNoPressure)
	}
}

// IT-SCHED-04: CPU-only pods penalized on GPU nodes.
func TestIT_SCHED_04_CPUOnlyGPUPenalty(t *testing.T) {
	now := time.Now()
	states := map[string]*twinStateIT{
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
	hwInfo := map[string]nodeHWInfoIT{
		"gpu-node": {GPUPresent: true, GPUCount: 8},
		"cpu-node": {GPUPresent: false, GPUCount: 0},
	}

	// CPU-only pod should prefer CPU-only node
	sGPU := scoreNode_IT("gpu-node", states, hwInfo, "standard", 0, false)
	sCPU := scoreNode_IT("cpu-node", states, hwInfo, "standard", 0, false)
	if sGPU >= sCPU {
		t.Errorf("CPU-only pod should prefer CPU node over GPU node: gpu=%d cpu=%d", sGPU, sCPU)
	}

	// GPU pod should NOT get the penalty
	sGPUPod := scoreNode_IT("gpu-node", states, hwInfo, "standard", 0, true)
	if sGPUPod != sCPU {
		t.Errorf("GPU pod on GPU node should score same as CPU pod on CPU node: gpuPod=%d cpuOnCpu=%d", sGPUPod, sCPU)
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
	LastUpdated                 time.Time
}

type nodeHWInfoIT struct {
	GPUPresent bool
	GPUCount   int
}

// shouldFilterNode_IT mirrors the scheduler's shouldFilterNode logic.
// Performance pods are rejected from eco and draining nodes.
// Standard pods can land anywhere.
func shouldFilterNode_IT(nodeName string, states map[string]*twinStateIT, labels map[string]string, isPerformance bool) string {
	state, hasState := states[nodeName]
	if !hasState {
		return "" // unknown node: allow
	}
	if !isPerformance {
		return "" // standard pods can land anywhere
	}
	// Performance pods cannot land on eco or draining nodes
	if state.SchedulableClass == "eco" {
		return "joulie: performance pod rejected from eco node"
	}
	if state.SchedulableClass == "draining" {
		return "joulie: performance pod rejected from draining node"
	}
	return ""
}

// scoreNode_IT mirrors the scheduler's scoreNode logic with adaptive pressure and GPU penalty.
func scoreNode_IT(nodeName string, states map[string]*twinStateIT, hwInfo map[string]nodeHWInfoIT, wpClass string, perfPressure float64, gpuRequested bool) int64 {
	state, ok := states[nodeName]
	if !ok {
		return 50 // neutral for unknown nodes
	}

	// Stale twin data falls back to neutral
	staleness := 5 * time.Minute
	if !state.LastUpdated.IsZero() && time.Since(state.LastUpdated) > staleness {
		return 50
	}

	score := state.PredictedPowerHeadroomScore*0.4 +
		(100-state.PredictedCoolingStressScore)*0.3 +
		(100-state.PredictedPsuStressScore)*0.3

	// Adaptive pressure relief: standard pods on performance nodes get penalized
	// when performance nodes are congested, steering standard work to eco nodes.
	if wpClass == "standard" && state.SchedulableClass == "performance" {
		score -= perfPressure * 0.3
	}

	// CPU-only pod GPU penalty: discourage CPU-only pods from landing on GPU nodes
	if hw, ok := hwInfo[nodeName]; ok && hw.GPUPresent && !gpuRequested {
		score -= 30
	}

	if score > 100 {
		score = 100
	}
	if score < 0 {
		score = 0
	}
	return int64(score)
}
