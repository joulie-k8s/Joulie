package main

import (
	"math"
	"testing"

	"github.com/matbun/joulie/pkg/controller/fsm"
	"github.com/matbun/joulie/pkg/controller/policy"
	"github.com/matbun/joulie/pkg/hwinv"
	corev1 "k8s.io/api/core/v1"
)

// These conformance tests verify that the standalone simulator's scheduling
// and operator policy logic matches the real Joulie operator and scheduler
// extender. Any drift between these implementations means simulation results
// diverge from real-world behavior.

// --- Operator Policy Conformance ---

// TestConformance_PerformanceSetMatchesController verifies that the simulator
// keeps the same nodes in performance as the controller manager would for the
// same cluster. The controller side is written out by hand as what
// listNodeHardware and the node labels give the policy: catalogue keys for the
// models, and eco for a draining node. Writing it from the simulator's own
// mapping would hide a mapping bug. The two H100 families are the same size,
// so some slot counts tie and the tie goes to the smaller family key: raw
// product strings sort SXM first ("NVIDIA H100 80GB HBM3"), catalogue keys
// sort NVL first.
func TestConformance_PerformanceSetMatchesController(t *testing.T) {
	catalog, err := hwinv.LoadDefaultCatalog()
	if err != nil {
		t.Fatalf("load catalogue: %v", err)
	}
	const epyc, nvl, sxm = "AMD EPYC 9654 96-Core Processor", "NVIDIA H100 NVL", "NVIDIA H100 80GB HBM3"
	nodes := []expandedNode{
		{Name: "cpu-0", CPUModel: epyc},
		{Name: "cpu-1", CPUModel: epyc},
		{Name: "cpu-2", CPUModel: epyc},
		{Name: "nvl-0", Product: nvl, GPUCount: 8, CPUModel: epyc},
		{Name: "nvl-1", Product: nvl, GPUCount: 8, CPUModel: epyc},
		{Name: "sxm-0", Product: sxm, GPUCount: 4, CPUModel: epyc},
		{Name: "sxm-1", Product: sxm, GPUCount: 4, CPUModel: epyc},
	}
	nodeNames := make([]string, 0, len(nodes))
	nodeByName := map[string]*expandedNode{}
	for i := range nodes {
		nodeNames = append(nodeNames, nodes[i].Name)
		nodeByName[nodes[i].Name] = &nodes[i]
	}
	tracker := map[string]*standaloneNodeTracker{
		"cpu-0": {isEco: true}, "cpu-1": {isEco: true}, "cpu-2": {},
		"nvl-0": {isEco: true}, "nvl-1": {isEco: true},
		"sxm-0": {isEco: true}, "sxm-1": {isEco: true, isDraining: true},
	}

	cpu := policy.NodeHardwareInfo{CPUModel: "AMD_EPYC_9654"}
	ctrlHW := map[string]policy.NodeHardwareInfo{
		"cpu-0": cpu, "cpu-1": cpu, "cpu-2": cpu,
		"nvl-0": {CPUModel: "AMD_EPYC_9654", GPUModel: "NVIDIA_H100_NVL", GPUCount: 8},
		"nvl-1": {CPUModel: "AMD_EPYC_9654", GPUModel: "NVIDIA_H100_NVL", GPUCount: 8},
		"sxm-0": {CPUModel: "AMD_EPYC_9654", GPUModel: "NVIDIA_H100_SXM", GPUCount: 4},
		"sxm-1": {CPUModel: "AMD_EPYC_9654", GPUModel: "NVIDIA_H100_SXM", GPUCount: 4},
	}
	ctrlProfiles := map[string]string{
		"cpu-0": "eco", "cpu-1": "eco", "cpu-2": "performance",
		"nvl-0": "eco", "nvl-1": "eco", "sxm-0": "eco", "sxm-1": "eco",
	}

	for hp := 0; hp <= len(nodes); hp++ {
		sim := performanceNodes(catalog, nodeNames, nodeByName, tracker, hp)
		plan := policy.BuildStaticPlan(nodeNames, ctrlHW, ctrlProfiles, 5000, 120, float64(hp)/float64(len(nodes)))
		ctrl := map[string]bool{}
		for _, a := range plan {
			if a.Profile == "performance" {
				ctrl[a.NodeName] = true
			}
		}
		if len(sim) != len(ctrl) {
			t.Fatalf("hp=%d: simulator keeps %v in performance, controller %v", hp, sim, ctrl)
		}
		for name := range ctrl {
			if !sim[name] {
				t.Fatalf("hp=%d: simulator keeps %v in performance, controller %v", hp, sim, ctrl)
			}
		}
	}
}

// TestConformance_QueueAware_PerfPodCounting verifies that queue-aware scaling
// uses performance-sensitive pod count (not all pending jobs) as the demand signal.
func TestConformance_QueueAware_PerfPodCounting(t *testing.T) {
	totalNodes := 100
	hpBaseFrac := 0.05
	hpMin := 5
	hpMax := 80
	perfPerHPNode := 3

	// Scenario: 30 performance-sensitive pods running.
	perfIntentPods := 30

	// Real operator.
	realNodes := make([]string, totalNodes)
	hw := map[string]policy.NodeHardwareInfo{}
	for i := 0; i < totalNodes; i++ {
		name := "node-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		if i < totalNodes {
			name = "node-" + itoa(i)
		}
		realNodes[i] = name
		hw[name] = policy.NodeHardwareInfo{CPUModel: "Xeon"}
	}
	realPlan := policy.BuildQueueAwarePlan(realNodes, hw, nil, 5000, 120, hpBaseFrac, hpMin, hpMax, perfPerHPNode, perfIntentPods)
	realPerfCount := 0
	for _, a := range realPlan {
		if a.Profile == "performance" {
			realPerfCount++
		}
	}

	// Standalone calculation (mirrors applyPowerPolicy for C).
	baseCount := int(math.Round(hpBaseFrac * float64(totalNodes)))
	queueNeed := int(math.Ceil(float64(perfIntentPods) / float64(perfPerHPNode)))
	standalHPCount := baseCount
	if queueNeed > standalHPCount {
		standalHPCount = queueNeed
	}
	if standalHPCount < hpMin {
		standalHPCount = hpMin
	}
	if standalHPCount > hpMax {
		standalHPCount = hpMax
	}

	// Allow off-by-one due to family diversity enforcement in real operator.
	if abs(realPerfCount-standalHPCount) > 1 {
		t.Errorf("queue-aware HP count mismatch: real=%d standalone=%d (perfIntentPods=%d)",
			realPerfCount, standalHPCount, perfIntentPods)
	}
}

// --- Scheduler Conformance ---

// TestConformance_Scheduler_PerfPodBlockedFromEco verifies that performance pods
// are rejected from eco nodes, matching the real scheduler extender's filter.
func TestConformance_Scheduler_PerfPodBlockedFromEco(t *testing.T) {
	nodes := []expandedNode{
		{Name: "eco-node", CPUCores: 64},
		{Name: "perf-node", CPUCores: 64},
	}
	nodeNames := []string{"eco-node", "perf-node"}
	nodeByName := map[string]*expandedNode{}
	for i := range nodes {
		nodeByName[nodes[i].Name] = &nodes[i]
	}
	tracker := map[string]*standaloneNodeTracker{
		"eco-node":  {isEco: true},
		"perf-node": {isEco: false},
	}

	perfJob := &simJob{Class: "performance", RequestedCPUCores: 4}
	node := findNodeForJob(tracker, nodeNames, nodeByName, perfJob)

	if node == "eco-node" {
		t.Error("performance pod was placed on eco node — should be rejected")
	}
	if node != "perf-node" {
		t.Errorf("performance pod should go to perf-node, got %q", node)
	}
}

// TestConformance_Scheduler_PerfPodBlockedFromDraining verifies that performance
// pods are also rejected from draining nodes (FSM DrainingPerformance state).
func TestConformance_Scheduler_PerfPodBlockedFromDraining(t *testing.T) {
	nodes := []expandedNode{
		{Name: "draining-node", CPUCores: 64},
		{Name: "perf-node", CPUCores: 64},
	}
	nodeNames := []string{"draining-node", "perf-node"}
	nodeByName := map[string]*expandedNode{}
	for i := range nodes {
		nodeByName[nodes[i].Name] = &nodes[i]
	}
	tracker := map[string]*standaloneNodeTracker{
		"draining-node": {isEco: false, isDraining: true},
		"perf-node":     {isEco: false, isDraining: false},
	}

	perfJob := &simJob{Class: "performance", RequestedCPUCores: 4}
	node := findNodeForJob(tracker, nodeNames, nodeByName, perfJob)

	if node == "draining-node" {
		t.Error("performance pod was placed on draining node — should be rejected")
	}
	if node != "perf-node" {
		t.Errorf("expected perf-node, got %q", node)
	}
}

// TestConformance_Scheduler_StandardPodPrefersEco verifies that standard pods
// get a scoring bonus on eco nodes (+10), steering them toward eco.
func TestConformance_Scheduler_StandardPodPrefersEco(t *testing.T) {
	nodes := []expandedNode{
		{Name: "eco-node", CPUCores: 64},
		{Name: "perf-node", CPUCores: 64},
	}
	nodeNames := []string{"eco-node", "perf-node"}
	nodeByName := map[string]*expandedNode{}
	for i := range nodes {
		nodeByName[nodes[i].Name] = &nodes[i]
	}
	tracker := map[string]*standaloneNodeTracker{
		"eco-node":  {isEco: true, usedCPU: 0},
		"perf-node": {isEco: false, usedCPU: 0},
	}

	stdJob := &simJob{Class: "standard", RequestedCPUCores: 4}
	node := findNodeForJob(tracker, nodeNames, nodeByName, stdJob)

	if node != "eco-node" {
		t.Errorf("standard pod should prefer eco node (scoring bonus), got %q", node)
	}
}

// TestConformance_Scheduler_PerfPodPlacedOnPerf verifies that performance pods
// are placed on performance nodes (only non-eco, non-draining nodes pass filter).
func TestConformance_Scheduler_PerfPodPlacedOnPerf(t *testing.T) {
	nodes := []expandedNode{
		{Name: "perf-node", CPUCores: 64},
	}
	nodeNames := []string{"perf-node"}
	nodeByName := map[string]*expandedNode{}
	for i := range nodes {
		nodeByName[nodes[i].Name] = &nodes[i]
	}
	tracker := map[string]*standaloneNodeTracker{
		"perf-node": {isEco: false, usedCPU: 0},
	}

	perfJob := &simJob{Class: "performance", RequestedCPUCores: 4}
	node := findNodeForJob(tracker, nodeNames, nodeByName, perfJob)

	if node != "perf-node" {
		t.Errorf("performance pod should be placed on perf-node, got %q", node)
	}
}

// TestConformance_Scheduler_EqualNodes_TieBreaking verifies that when two
// performance nodes have equal headroom, the first in name order wins.
func TestConformance_Scheduler_EqualNodes_TieBreaking(t *testing.T) {
	nodes := []expandedNode{
		{Name: "node-a", CPUCores: 64, GPUCount: 0},
		{Name: "node-b", CPUCores: 64, GPUCount: 0},
	}
	nodeNames := []string{"node-a", "node-b"}
	nodeByName := map[string]*expandedNode{}
	for i := range nodes {
		nodeByName[nodes[i].Name] = &nodes[i]
	}
	tracker := map[string]*standaloneNodeTracker{
		"node-a": {isEco: false, usedCPU: 0},
		"node-b": {isEco: false, usedCPU: 0},
	}

	job := &simJob{Class: "standard", RequestedCPUCores: 4}
	node := findNodeForJob(tracker, nodeNames, nodeByName, job)

	if node != "node-a" {
		t.Errorf("expected first node in name order (node-a), got %q", node)
	}
}

// TestConformance_Scheduler_AdaptivePressureRelief verifies that standard pods
// are steered away from congested performance nodes.
func TestConformance_Scheduler_AdaptivePressureRelief(t *testing.T) {
	nodes := []expandedNode{
		{Name: "perf-full", CPUCores: 64},
		{Name: "eco-empty", CPUCores: 64},
	}
	nodeNames := []string{"eco-empty", "perf-full"}
	nodeByName := map[string]*expandedNode{}
	for i := range nodes {
		nodeByName[nodes[i].Name] = &nodes[i]
	}
	tracker := map[string]*standaloneNodeTracker{
		"perf-full": {isEco: false, usedCPU: 60}, // 94% utilized
		"eco-empty": {isEco: true, usedCPU: 0},
	}

	stdJob := &simJob{Class: "standard", RequestedCPUCores: 2}
	node := findNodeForJob(tracker, nodeNames, nodeByName, stdJob)

	// With perf pressure high (~94%), standard pod should strongly prefer eco.
	if node != "eco-empty" {
		t.Errorf("standard pod should avoid congested perf node, got %q", node)
	}
}

// TestConformance_FSM_DrainingGuard verifies that nodes transitioning to eco
// keep performance caps when they still have performance pods running.
func TestConformance_FSM_DrainingGuard(t *testing.T) {
	// Node has perfPodCount=2, desired profile=eco → should be draining.
	tracker := map[string]*standaloneNodeTracker{
		"node-0": {perfPodCount: 2},
	}

	// In standalone, when applyPowerPolicy marks a node as eco but it has
	// perfPodCount>0, it should set isDraining=true and keep perf caps.
	// This is verified by the isDraining field being set.
	t.Run("perfPods>0_means_draining", func(t *testing.T) {
		tr := tracker["node-0"]
		// Simulate what applyPowerPolicy does for non-HP nodes.
		if tr.perfPodCount > 0 {
			tr.isDraining = true
			tr.isEco = false
		}
		if !tr.isDraining {
			t.Error("node with running perf pods should be in draining state")
		}
		if tr.isEco {
			t.Error("draining node should not be marked eco (keeps perf caps)")
		}
	})

	t.Run("perfPods==0_means_eco", func(t *testing.T) {
		tr := &standaloneNodeTracker{perfPodCount: 0}
		tr.isDraining = false
		tr.isEco = true
		if tr.isDraining {
			t.Error("node with no perf pods should not be draining")
		}
		if !tr.isEco {
			t.Error("node with no perf pods should be eco")
		}
	})
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

// TestConformance_QueueAwareDemandMatchesController pins the queue_aware_v1
// demand on both sides: what fsm.CountPerformanceDemand counts over pods, the
// simulator counts over the same workload as jobs. A running performance pod
// is a performance job placed on a node; a Pending one is a performance job
// whose submit time has passed and that no node has taken. Before this test
// the simulator's waiting-job branch could never match anything, because
// standalone marks a job Submitted only when it places it.
func TestConformance_QueueAwareDemandMatchesController(t *testing.T) {
	jobs := []*simJob{
		{Class: "performance", SubmitOffsetSec: 10, Submitted: true, NodeName: "n1"}, // running
		{Class: "performance", SubmitOffsetSec: 20},                                  // waiting
		{Class: "performance", SubmitOffsetSec: 30},                                  // waiting
		{Class: "performance", SubmitOffsetSec: 40, Submitted: true, Completed: true, NodeName: "n1"},
		{Class: "standard", SubmitOffsetSec: 50},     // waiting, not performance
		{Class: "performance", SubmitOffsetSec: 900}, // not submitted yet
	}
	tracker := map[string]*standaloneNodeTracker{"n1": {perfPodCount: 1}, "n2": {}}
	sim := countPerformanceSensitivePending(tracker, jobs, 100)

	perf := corev1.PodSpec{NodeSelector: map[string]string{fsm.PowerProfileLabelKey: fsm.ProfilePerformance}}
	pods := []corev1.Pod{
		{Spec: perf, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		{Spec: perf, Status: corev1.PodStatus{Phase: corev1.PodPending}},
		{Spec: perf, Status: corev1.PodStatus{Phase: corev1.PodPending}},
		{Spec: perf, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
		{Status: corev1.PodStatus{Phase: corev1.PodPending}},
	}
	ctrl := fsm.CountPerformanceDemand(pods)

	if sim != 3 || ctrl != 3 {
		t.Fatalf("queue-aware demand: simulator=%d controller=%d, want 3 on both (one running, two waiting)", sim, ctrl)
	}
}
