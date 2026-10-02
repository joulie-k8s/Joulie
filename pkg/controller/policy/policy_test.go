package policy

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestBuildStaticPlanEmpty(t *testing.T) {
	plan := BuildStaticPlan(nil, nil, nil, 5000, 120, 0.5)
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
	plan := BuildStaticPlan(nodes, hw, nil, 5000, 120, 0.5)
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
	plan := BuildStaticPlan(nodes, hw, nil, 5000, 120, 0.25)
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
	plan := BuildQueueAwarePlan(nodes, hw, nil, 5000, 120, 0.60, 1, 100, 10, 20)
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
	plan2 := BuildQueueAwarePlan(nodes, hw, nil, 5000, 120, 0.60, 1, 100, 10, 50)
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

// Edge case tests

func TestBuildStaticPlanSingleNode(t *testing.T) {
	nodes := []string{"only"}
	hw := map[string]NodeHardwareInfo{"only": {CPUModel: "x"}}
	plan := BuildStaticPlan(nodes, hw, nil, 5000, 120, 0.0)
	// With hpFrac=0, family floor forces 1 performance node
	if len(plan) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(plan))
	}
	if plan[0].Profile != "performance" {
		t.Errorf("expected performance (family floor), got %s", plan[0].Profile)
	}
}

func TestBuildStaticPlanAllPerformance(t *testing.T) {
	nodes := []string{"a", "b"}
	hw := map[string]NodeHardwareInfo{"a": {CPUModel: "x"}, "b": {CPUModel: "x"}}
	plan := BuildStaticPlan(nodes, hw, nil, 5000, 120, 1.0)
	for _, a := range plan {
		if a.Profile != "performance" {
			t.Errorf("expected all performance at hpFrac=1.0, got %s for %s", a.Profile, a.NodeName)
		}
	}
}

func TestBuildStaticPlanFractionClamped(t *testing.T) {
	nodes := []string{"a", "b"}
	hw := map[string]NodeHardwareInfo{"a": {CPUModel: "x"}, "b": {CPUModel: "x"}}
	// Negative fraction should be clamped to 0
	plan := BuildStaticPlan(nodes, hw, nil, 5000, 120, -0.5)
	perf := 0
	for _, a := range plan {
		if a.Profile == "performance" {
			perf++
		}
	}
	// Family floor forces at least 1 perf node per family
	if perf < 1 {
		t.Errorf("expected at least 1 performance node (family floor), got %d", perf)
	}
}

func TestBuildStaticPlanFractionAboveOne(t *testing.T) {
	nodes := []string{"a", "b"}
	hw := map[string]NodeHardwareInfo{"a": {CPUModel: "x"}, "b": {CPUModel: "x"}}
	plan := BuildStaticPlan(nodes, hw, nil, 5000, 120, 1.5)
	for _, a := range plan {
		if a.Profile != "performance" {
			t.Errorf("expected all performance at hpFrac>1.0, got %s for %s", a.Profile, a.NodeName)
		}
	}
}

func TestBuildQueueAwarePlanEmptyNodes(t *testing.T) {
	plan := BuildQueueAwarePlan(nil, nil, nil, 5000, 120, 0.5, 0, 10, 10, 0)
	if len(plan) != 0 {
		t.Errorf("expected empty plan for nil nodes, got %d", len(plan))
	}
}

func TestBuildQueueAwarePlanNegativeParams(t *testing.T) {
	nodes := []string{"a", "b"}
	hw := map[string]NodeHardwareInfo{"a": {CPUModel: "x"}, "b": {CPUModel: "x"}}
	// Negative hpMin, perfPerHPNode=0 should be corrected
	plan := BuildQueueAwarePlan(nodes, hw, nil, 5000, 120, 0.5, -1, -1, 0, 0)
	if len(plan) != 2 {
		t.Fatalf("expected 2 assignments, got %d", len(plan))
	}
}

func TestBuildRuleSwapPlanSingleNode(t *testing.T) {
	nodes := []string{"solo"}
	plan := BuildRuleSwapPlanAt(nodes, time.Minute, 5000, 120, time.Unix(0, 0))
	if len(plan) != 1 {
		t.Fatalf("expected 1 assignment")
	}
	// At t=0, phase=0, single node should be eco
	if plan[0].Profile != "eco" {
		t.Errorf("expected eco at phase 0, got %s", plan[0].Profile)
	}
}

func TestBuildRuleSwapPlanZeroInterval(t *testing.T) {
	nodes := []string{"a", "b"}
	// Zero interval should not panic (division by zero guard)
	plan := BuildRuleSwapPlanAt(nodes, 0, 5000, 120, time.Now())
	if len(plan) != 2 {
		t.Fatalf("expected 2 assignments with zero interval, got %d", len(plan))
	}
}

func TestNodeFamilyFallbackToRawModel(t *testing.T) {
	hw := map[string]NodeHardwareInfo{
		"n": {GPURawModel: "NVIDIA-L40S", GPUCount: 4},
	}
	f := NodeFamily("n", hw)
	if f != "gpu:NVIDIA-L40S" {
		t.Errorf("expected gpu:NVIDIA-L40S from raw model fallback, got %s", f)
	}
}

func TestNodeFamilyCPUFallbackToRawModel(t *testing.T) {
	hw := map[string]NodeHardwareInfo{
		"n": {CPURawModel: "AMD-EPYC-9654"},
	}
	f := NodeFamily("n", hw)
	if f != "cpu:AMD-EPYC-9654" {
		t.Errorf("expected cpu:AMD-EPYC-9654 from raw model fallback, got %s", f)
	}
}

func TestNodeFamilyEmptyHardware(t *testing.T) {
	hw := map[string]NodeHardwareInfo{"n": {}}
	f := NodeFamily("n", hw)
	if f != "cpu:unknown-cpu" {
		t.Errorf("expected cpu:unknown-cpu for empty hw, got %s", f)
	}
}

// fleet builds n nodes per family, named <family>-<i>, with hardware that
// NodeFamily keys as gpu:<family> or cpu:<family>.
func fleet(sizes map[string]int) ([]string, map[string]NodeHardwareInfo) {
	var nodes []string
	hw := map[string]NodeHardwareInfo{}
	for family, n := range sizes {
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("%s-%04d", family, i)
			nodes = append(nodes, name)
			if strings.HasPrefix(family, "gpu") {
				hw[name] = NodeHardwareInfo{GPUModel: family, GPUCount: 4}
			} else {
				hw[name] = NodeHardwareInfo{CPUModel: family}
			}
		}
	}
	sort.Strings(nodes)
	return nodes, hw
}

func perfByFamily(set map[string]bool, hw map[string]NodeHardwareInfo) map[string]int {
	out := map[string]int{}
	for node := range set {
		out[NodeFamily(node, hw)]++
	}
	return out
}

// A fraction of performance nodes is a share of every family. In name order
// the CPU family, whose names sort first, took 4 of the 5 slots.
func TestPerformanceSetSplitsSlotsByFamilySize(t *testing.T) {
	nodes, hw := fleet(map[string]int{"a-cpu": 5, "gpu-b": 20})
	got := perfByFamily(PerformanceSet(nodes, hw, nil, 5), hw)
	if got["cpu:a-cpu"] != 1 || got["gpu:gpu-b"] != 4 {
		t.Fatalf("performance nodes per family = %v, want cpu 1 of 5 and gpu 4 of 20", got)
	}
}

// The quotas documented for the exp02 5k inventory at 1000 slots, quoted in
// website/content/en/docs/architecture/policies.md and controller-manager.md.
func TestPerformanceSetMatchesDocumentedExp02Split(t *testing.T) {
	nodes, hw := fleet(map[string]int{
		"gpu-h100-nvl": 1450, "gpu-h100-sxm": 730, "gpu-l40s": 850, "gpu-mi300x": 240, "gpu-w7900": 730,
		"cpu-highcore": 250, "cpu-highfreq": 250, "cpu-intensive": 500,
	})
	got := perfByFamily(PerformanceSet(nodes, hw, nil, 1000), hw)
	want := map[string]int{
		"gpu:gpu-h100-nvl": 290, "gpu:gpu-h100-sxm": 146, "gpu:gpu-l40s": 170, "gpu:gpu-mi300x": 48, "gpu:gpu-w7900": 146,
		"cpu:cpu-highcore": 50, "cpu:cpu-highfreq": 50, "cpu:cpu-intensive": 100,
	}
	for family, n := range want {
		if got[family] != n {
			t.Fatalf("family %s has %d performance nodes, want %d (all: %v)", family, got[family], n, got)
		}
	}
}

func TestPerformanceSetIgnoresInputOrder(t *testing.T) {
	nodes, hw := fleet(map[string]int{"cpu-x": 7, "gpu-y": 11, "cpu-z": 3})
	want := PerformanceSet(nodes, hw, nil, 9)
	reversed := append([]string(nil), nodes...)
	sort.Sort(sort.Reverse(sort.StringSlice(reversed)))
	got := PerformanceSet(reversed, hw, nil, 9)
	if len(got) != len(want) {
		t.Fatalf("got %d nodes, want %d", len(got), len(want))
	}
	for node := range want {
		if !got[node] {
			t.Fatalf("reversed input dropped %s: got %v want %v", node, got, want)
		}
	}
}

// queue_aware_v1 moves hpCount with demand. Each step must change exactly one
// node, or a one-pod change in demand reshuffles the cluster.
func TestPerformanceSetChangesOneNodePerSlot(t *testing.T) {
	nodes, hw := fleet(map[string]int{"gpu-a": 29, "gpu-b": 15, "gpu-c": 17, "gpu-d": 5, "cpu-e": 5, "cpu-f": 10})
	prev := PerformanceSet(nodes, hw, nil, 6)
	for k := 7; k <= len(nodes); k++ {
		next := PerformanceSet(nodes, hw, nil, k)
		for node := range prev {
			if !next[node] {
				t.Fatalf("k=%d drops %s, which was performance at k=%d", k, node, k-1)
			}
		}
		if len(next) != len(prev)+1 {
			t.Fatalf("k=%d has %d performance nodes, want %d", k, len(next), len(prev)+1)
		}
		prev = next
	}
}

func TestPerformanceSetKeepsCurrentPerformanceNodes(t *testing.T) {
	nodes, hw := fleet(map[string]int{"cpu-a": 4})
	current := map[string]string{"cpu-a-0002": "performance", "cpu-a-0003": "performance", "cpu-a-0000": "eco"}
	got := PerformanceSet(nodes, hw, current, 2)
	if !got["cpu-a-0002"] || !got["cpu-a-0003"] || len(got) != 2 {
		t.Fatalf("got %v, want the two nodes already in performance", got)
	}
}

func TestPerformanceSetGivesEveryFamilyOneSlot(t *testing.T) {
	nodes, hw := fleet(map[string]int{"cpu-a": 3, "gpu-b": 3, "gpu-c": 3})
	got := perfByFamily(PerformanceSet(nodes, hw, nil, 0), hw)
	if len(got) != 3 || got["cpu:cpu-a"] != 1 || got["gpu:gpu-b"] != 1 || got["gpu:gpu-c"] != 1 {
		t.Fatalf("performance nodes per family = %v, want one in each", got)
	}
}

// Pins the divisor method and its tie rule, which the docs state. For sizes
// 5, 2 and 2 with 7 slots, Sainte-Lague gives 4/2/1, D'Hondt 5/1/1 and Adams
// 3/2/2, and the 2/1 between the two equal families is a tie that goes to the
// smaller key; ties to the larger key would give 4/1/2.
func TestPerformanceSetUsesSainteLagueWithTiesToTheSmallerKey(t *testing.T) {
	nodes, hw := fleet(map[string]int{"cpu-a": 5, "cpu-b": 2, "cpu-c": 2})
	got := perfByFamily(PerformanceSet(nodes, hw, nil, 7), hw)
	if got["cpu:cpu-a"] != 4 || got["cpu:cpu-b"] != 2 || got["cpu:cpu-c"] != 1 {
		t.Fatalf("performance nodes per family = %v, want cpu-a 4, cpu-b 2, cpu-c 1", got)
	}
}
