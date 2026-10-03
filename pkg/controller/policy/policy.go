// Package policy implements node power profile assignment algorithms.
//
// Each policy takes a list of node names + hardware info and returns a plan
// assigning each node to "performance" or "eco" with the corresponding power cap.
//
// Available policies:
//   - static_partition: a fixed fraction of every hardware family is performance
//   - queue_aware_v1: adjusts performance count based on running perf-sensitive pods
//   - rule_swap_v1: time-phased round-robin (legacy, for benchmarking)
//
// static_partition and queue_aware_v1 decide how many nodes stay in
// performance; PerformanceSet decides which ones.
package policy

import (
	"math"
	"sort"
	"strings"
	"time"
)

// profilePerformance is the value of the power-profile label on a node that
// runs uncapped. It mirrors fsm.ProfilePerformance, which imports this
// package and so cannot be imported here.
const profilePerformance = "performance"

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

// BuildStaticPlan keeps a fixed fraction of the nodes in performance, split
// across hardware families by PerformanceSet. current maps a node to its
// present power-profile label; nodes absent from it are treated as not in
// performance.
func BuildStaticPlan(nodes []string, hw map[string]NodeHardwareInfo, current map[string]string, perfCap, ecoCap, hpFrac float64) []NodeAssignment {
	n := len(nodes)
	if n == 0 {
		return nil
	}
	hpFrac = clamp01(hpFrac)
	hpCount := int(math.Round(float64(n) * hpFrac))
	perfNodes := PerformanceSet(nodes, hw, current, hpCount)

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
// The nodes are chosen by PerformanceSet, as in BuildStaticPlan.
func BuildQueueAwarePlan(nodes []string, hw map[string]NodeHardwareInfo, current map[string]string, perfCap, ecoCap, hpBaseFrac float64, hpMin, hpMax, perfPerHPNode, perfIntentPods int) []NodeAssignment {
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
	perfNodes := PerformanceSet(nodes, hw, current, hpCount)
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
	intervalSec := int64(interval.Seconds())
	if intervalSec <= 0 {
		intervalSec = 60 // default to 1 minute if invalid
	}
	phase := int((now.Unix() / intervalSec) % 2)
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

// PerformanceSet returns the nodes to keep in the performance profile when
// hpCount of them may run uncapped.
//
// The slots are split across hardware families (NodeFamily) in proportion to
// family size, with at least one per family, so STATIC_HP_FRAC=0.2 keeps about
// a fifth of every family uncapped instead of giving every slot to whichever
// family a ranking puts first. hpCount is raised to the number of families
// and capped at the number of nodes.
//
// The split is the Sainte-Lague divisor method started from one slot per
// family: each further slot goes to the family with the largest
// size/(2*slots+1), ties to the smaller family key. A divisor method is house
// monotone: the split for k slots is contained in the split for k+1, so when
// queue pressure moves hpCount by one, exactly one node changes profile.
//
// Within a family, nodes whose current label is performance are kept first,
// so a reconcile at constant demand moves nothing; the node name orders the
// rest. The result does not depend on the order of nodes.
func PerformanceSet(nodes []string, hw map[string]NodeHardwareInfo, current map[string]string, hpCount int) map[string]bool {
	members := make(map[string][]string)
	for _, node := range nodes {
		family := NodeFamily(node, hw)
		members[family] = append(members[family], node)
	}
	families := make([]string, 0, len(members))
	for family, m := range members {
		families = append(families, family)
		sort.Slice(m, func(i, j int) bool {
			pi, pj := current[m[i]] == profilePerformance, current[m[j]] == profilePerformance
			if pi != pj {
				return pi
			}
			return m[i] < m[j]
		})
	}
	sort.Strings(families)

	hpCount = clampInt(hpCount, len(families), len(nodes))
	slots := make(map[string]int, len(families))
	for _, family := range families {
		slots[family] = 1
	}
	for left := hpCount - len(families); left > 0; left-- {
		next := ""
		for _, family := range families {
			if slots[family] == len(members[family]) {
				continue
			}
			if next == "" || deservesNextSlot(len(members[family]), slots[family], len(members[next]), slots[next]) {
				next = family
			}
		}
		slots[next]++
	}

	perf := make(map[string]bool, hpCount)
	for _, family := range families {
		for _, node := range members[family][:slots[family]] {
			perf[node] = true
		}
	}
	return perf
}

// deservesNextSlot reports whether a family of size na holding qa slots has a
// strictly larger Sainte-Lague quotient na/(2*qa+1) than one of size nb
// holding qb. Cross multiplication keeps the comparison exact, so ties fall to
// the family met first, which is the smaller key.
func deservesNextSlot(na, qa, nb, qb int) bool {
	return na*(2*qb+1) > nb*(2*qa+1)
}

// NodeFamily classifies a node by its hardware family, the unit PerformanceSet
// splits performance slots across. Returns "gpu:<model>" or "cpu:<model>".
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
