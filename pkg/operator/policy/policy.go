// Package policy implements node power profile assignment algorithms.
//
// Each policy takes a list of node names + hardware info and returns a plan
// assigning each node to "performance" or "eco" with the corresponding power cap.
//
// Available policies:
//   - static_partition: fixed fraction of nodes are performance
//   - queue_aware_v1: adjusts performance count based on running perf-sensitive pods
//   - rule_swap_v1: time-phased round-robin (legacy, for benchmarking)
package policy

import (
	"math"
	"strings"
	"time"
)

// NodeAssignment is the output of a policy: one entry per managed node.
type NodeAssignment struct {
	NodeName       string
	Profile        string  // "performance" or "eco"
	CapWatts       float64 // absolute power cap
	CPUCapPctOfMax *float64
	GPU            *GPUCapIntent
	ManagedBy      string // policy name that produced this assignment
	SourceProfile  string
	SourceDrain    bool
	Draining       bool
	State          string
}

// GPUCapIntent encodes GPU power cap intent.
type GPUCapIntent struct {
	Scope          string
	CapWattsPerGPU *float64
	CapPctOfMax    *float64
}

// NodeHardwareInfo is the minimal hardware info needed by policy algorithms.
type NodeHardwareInfo struct {
	CPUModel    string
	CPURawModel string
	GPUModel    string
	GPURawModel string
	GPUCount    int
}

// BuildStaticPlan allocates a fixed fraction of nodes to performance.
// Nodes are selected to maximize hardware family diversity.
func BuildStaticPlan(nodes []string, hw map[string]NodeHardwareInfo, perfCap, ecoCap, hpFrac float64) []NodeAssignment {
	n := len(nodes)
	if n == 0 {
		return nil
	}
	hpFrac = clamp01(hpFrac)
	hpCount := int(math.Round(float64(n) * hpFrac))
	hpCount = clampInt(hpCount, 0, n)
	hpCount = enforceFamilyPerformanceFloor(nodes, hw, hpCount)
	perfNodes := selectPerformanceNodes(nodes, hw, hpCount)

	plan := make([]NodeAssignment, 0, n)
	for _, node := range nodes {
		profile := "eco"
		cap := ecoCap
		if perfNodes[node] {
			profile = "performance"
			cap = perfCap
		}
		plan = append(plan, NodeAssignment{
			NodeName:  node,
			Profile:   profile,
			CapWatts:  cap,
			ManagedBy: "static-partition-v1",
		})
	}
	return plan
}

// BuildQueueAwarePlan adjusts performance node count based on running
// performance-sensitive pods. More perf pods → more perf nodes (up to max).
func BuildQueueAwarePlan(nodes []string, hw map[string]NodeHardwareInfo, perfCap, ecoCap, hpBaseFrac float64, hpMin, hpMax, perfPerHPNode, perfIntentPods int) []NodeAssignment {
	n := len(nodes)
	if n == 0 {
		return nil
	}
	hpBaseFrac = clamp01(hpBaseFrac)
	if hpMin < 0 {
		hpMin = 0
	}
	if hpMax <= 0 {
		hpMax = n
	}
	if hpMax < hpMin {
		hpMax = hpMin
	}
	if perfPerHPNode <= 0 {
		perfPerHPNode = 1
	}
	baseCount := int(math.Round(float64(n) * hpBaseFrac))
	queueNeed := int(math.Ceil(float64(perfIntentPods) / float64(perfPerHPNode)))
	hpCount := baseCount
	if queueNeed > hpCount {
		hpCount = queueNeed
	}
	hpCount = clampInt(hpCount, hpMin, hpMax)
	hpCount = clampInt(hpCount, 0, n)
	hpCount = enforceFamilyPerformanceFloor(nodes, hw, hpCount)

	perfNodes := selectPerformanceNodes(nodes, hw, hpCount)
	plan := make([]NodeAssignment, 0, n)
	for _, node := range nodes {
		profile := "eco"
		cap := ecoCap
		if perfNodes[node] {
			profile = "performance"
			cap = perfCap
		}
		plan = append(plan, NodeAssignment{
			NodeName:  node,
			Profile:   profile,
			CapWatts:  cap,
			ManagedBy: "queue-aware-v1",
		})
	}
	return plan
}

// BuildRuleSwapPlan alternates which node is eco on a time-phased schedule.
// Legacy algorithm, primarily used for benchmarking.
func BuildRuleSwapPlan(nodes []string, interval time.Duration, perfCap, ecoCap float64) []NodeAssignment {
	return BuildRuleSwapPlanAt(nodes, interval, perfCap, ecoCap, time.Now())
}

// BuildRuleSwapPlanAt is the time-parameterized version of BuildRuleSwapPlan.
func BuildRuleSwapPlanAt(nodes []string, interval time.Duration, perfCap, ecoCap float64, now time.Time) []NodeAssignment {
	phase := int((now.Unix() / int64(interval.Seconds())) % 2)
	plan := make([]NodeAssignment, 0, len(nodes))
	for i, n := range nodes {
		profile := "performance"
		cap := perfCap
		if i == 0 && (phase%2 == 0) {
			profile = "eco"
			cap = ecoCap
		}
		if i == 1 && (phase%2 == 1) {
			profile = "eco"
			cap = ecoCap
		}
		plan = append(plan, NodeAssignment{NodeName: n, Profile: profile, CapWatts: cap, ManagedBy: "rule-swap-v1"})
	}
	if len(nodes) == 1 {
		if phase%2 == 0 {
			plan[0].Profile = "eco"
			plan[0].CapWatts = ecoCap
		} else {
			plan[0].Profile = "performance"
			plan[0].CapWatts = perfCap
		}
	}
	return plan
}

// enforceFamilyPerformanceFloor ensures at least one performance node per
// hardware family (GPU model or CPU model), so each family stays represented.
func enforceFamilyPerformanceFloor(nodes []string, hw map[string]NodeHardwareInfo, hpCount int) int {
	families := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		families[NodeFamily(node, hw)] = struct{}{}
	}
	if len(families) > hpCount {
		hpCount = len(families)
	}
	if hpCount > len(nodes) {
		hpCount = len(nodes)
	}
	return hpCount
}

// selectPerformanceNodes greedily picks hpCount nodes for performance,
// prioritizing one node from each hardware family before filling remaining slots.
func selectPerformanceNodes(nodes []string, hw map[string]NodeHardwareInfo, hpCount int) map[string]bool {
	perfNodes := make(map[string]bool, hpCount)
	seenFamilies := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		family := NodeFamily(node, hw)
		if _, ok := seenFamilies[family]; ok {
			continue
		}
		perfNodes[node] = true
		seenFamilies[family] = struct{}{}
		if len(perfNodes) >= hpCount {
			return perfNodes
		}
	}
	for _, node := range nodes {
		if perfNodes[node] {
			continue
		}
		perfNodes[node] = true
		if len(perfNodes) >= hpCount {
			break
		}
	}
	return perfNodes
}

// NodeFamily classifies a node by its hardware family for diversity in
// performance node selection. Returns "gpu:<model>" or "cpu:<model>".
func NodeFamily(node string, hw map[string]NodeHardwareInfo) string {
	nh, ok := hw[node]
	if !ok {
		return "unknown:" + node
	}
	if nh.GPUCount > 0 {
		model := firstNonEmpty(nh.GPUModel, nh.GPURawModel, "unknown-gpu")
		return "gpu:" + model
	}
	model := firstNonEmpty(nh.CPUModel, nh.CPURawModel, "unknown-cpu")
	return "cpu:" + model
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
