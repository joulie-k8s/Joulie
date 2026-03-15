package policy

import (
	"testing"
	"time"
)

func TestBuildStaticPlanEmpty(t *testing.T) {
	plan := BuildStaticPlan(nil, nil, 5000, 120, 0.5)
	if len(plan) != 0 {
		t.Errorf("expected empty plan for no nodes")
	}
}

func TestBuildStaticPlanFraction(t *testing.T) {
	nodes := []string{"a", "b", "c", "d"}
	hw := map[string]NodeHardwareInfo{
		"a": {CPUModel: "epyc-1"},
		"b": {CPUModel: "epyc-1"},
		"c": {CPUModel: "epyc-1"},
		"d": {CPUModel: "epyc-1"},
	}
	plan := BuildStaticPlan(nodes, hw, 5000, 120, 0.5)
	if len(plan) != 4 {
		t.Fatalf("expected 4 assignments, got %d", len(plan))
	}
	perf := 0
	for _, a := range plan {
		if a.Profile == "performance" {
			perf++
		}
	}
	if perf != 2 {
		t.Errorf("expected 2 performance nodes at 50%%, got %d", perf)
	}
}

func TestBuildStaticPlanDiversity(t *testing.T) {
	nodes := []string{"gpu1", "gpu2", "cpu1", "cpu2"}
	hw := map[string]NodeHardwareInfo{
		"gpu1": {GPUModel: "A100", GPUCount: 8},
		"gpu2": {GPUModel: "A100", GPUCount: 8},
		"cpu1": {CPUModel: "EPYC"},
		"cpu2": {CPUModel: "EPYC"},
	}
	// Even at 25% (1 node), family floor forces 2 (one gpu, one cpu)
	plan := BuildStaticPlan(nodes, hw, 5000, 120, 0.25)
	perf := map[string]bool{}
	for _, a := range plan {
		if a.Profile == "performance" {
			perf[a.NodeName] = true
		}
	}
	// Must have at least one from each family
	hasGPU, hasCPU := false, false
	for name := range perf {
		if hw[name].GPUCount > 0 {
			hasGPU = true
		} else {
			hasCPU = true
		}
	}
	if !hasGPU || !hasCPU {
		t.Errorf("expected diversity: hasGPU=%v hasCPU=%v perfNodes=%v", hasGPU, hasCPU, perf)
	}
}

func TestBuildQueueAwarePlan(t *testing.T) {
	nodes := []string{"a", "b", "c", "d", "e"}
	hw := map[string]NodeHardwareInfo{
		"a": {CPUModel: "x"}, "b": {CPUModel: "x"},
		"c": {CPUModel: "x"}, "d": {CPUModel: "x"},
		"e": {CPUModel: "x"},
	}
	// 20 perf pods, 10 per node → need 2, but base is 60% of 5 = 3
	plan := BuildQueueAwarePlan(nodes, hw, 5000, 120, 0.60, 1, 100, 10, 20)
	perf := 0
	for _, a := range plan {
		if a.Profile == "performance" {
			perf++
		}
	}
	if perf != 3 {
		t.Errorf("expected 3 performance nodes (base 60%%), got %d", perf)
	}

	// 50 perf pods, 10 per node → need 5 > base 3
	plan2 := BuildQueueAwarePlan(nodes, hw, 5000, 120, 0.60, 1, 100, 10, 50)
	perf2 := 0
	for _, a := range plan2 {
		if a.Profile == "performance" {
			perf2++
		}
	}
	if perf2 != 5 {
		t.Errorf("expected 5 performance nodes (queue demand), got %d", perf2)
	}
}

func TestBuildRuleSwapPlan(t *testing.T) {
	nodes := []string{"a", "b"}
	t0 := time.Unix(0, 0)
	plan := BuildRuleSwapPlanAt(nodes, time.Minute, 5000, 120, t0)
	if len(plan) != 2 {
		t.Fatalf("expected 2 assignments, got %d", len(plan))
	}
	// At t=0, phase=0, node[0] should be eco
	if plan[0].Profile != "eco" {
		t.Errorf("expected node a to be eco at phase 0, got %s", plan[0].Profile)
	}
}

func TestNodeFamily(t *testing.T) {
	hw := map[string]NodeHardwareInfo{
		"gpu1": {GPUModel: "A100", GPUCount: 8},
		"cpu1": {CPUModel: "EPYC"},
		"x":    {},
	}
	if f := NodeFamily("gpu1", hw); f != "gpu:A100" {
		t.Errorf("expected gpu:A100, got %s", f)
	}
	if f := NodeFamily("cpu1", hw); f != "cpu:EPYC" {
		t.Errorf("expected cpu:EPYC, got %s", f)
	}
	if f := NodeFamily("missing", hw); f != "unknown:missing" {
		t.Errorf("expected unknown:missing, got %s", f)
	}
}
