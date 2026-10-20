package scenario

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/matbun/joulie/pkg/controller/policy"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
)

// MaxOfferedLoad is the highest offered load plan accepts for any
// resource, and the highest share of the run's deadline (duration plus
// drain) the earliest end of its work may take. Above it queues grow for
// the whole window, or the last jobs end past the deadline, so a
// completion assertion fails by construction rather than by Joulie's doing.
const MaxOfferedLoad = 0.9

// SpecPool is one pool of a cluster spec as the planner sees it: its Nodes
// as clusterspec.Expand renders them, with their labels (the topology and
// host name labels included), taints and allocatable resources, and the
// hardware the agent reports for each, in the same order.
type SpecPool struct {
	Name     string
	Nodes    []*corev1.Node
	Hardware []policy.NodeHardwareInfo
}

// ReadSpecPools loads the cluster spec file spec (relative to repoRoot) as
// the simulator and the harness do, clusterspec.LoadFile then Expand, and
// returns its name and its Nodes grouped by pool, in the spec's pool order.
// The profiles are the builtin ones: the in-cluster simulator reads no
// other, and up.sh refuses a spec that needs a local profile.
func ReadSpecPools(repoRoot, spec string) (string, []SpecPool, error) {
	s, err := clusterspec.LoadFile(filepath.Join(repoRoot, spec))
	if err != nil {
		return "", nil, fmt.Errorf("cluster spec: %w", err)
	}
	members, err := s.Expand(clusterspec.Profiles(""))
	if err != nil {
		return "", nil, fmt.Errorf("cluster spec %s: %w", spec, err)
	}
	byPool := map[string]*SpecPool{}
	for _, m := range members {
		p := byPool[m.Pool]
		if p == nil {
			p = &SpecPool{Name: m.Pool}
			byPool[m.Pool] = p
		}
		p.Nodes = append(p.Nodes, m.Node)
		p.Hardware = append(p.Hardware, nodeHardware(m))
	}
	out := make([]SpecPool, 0, len(s.Spec.Pools))
	for _, pool := range s.Spec.Pools {
		if p := byPool[pool.Name]; p != nil {
			out = append(out, *p)
		} else {
			out = append(out, SpecPool{Name: pool.Name})
		}
	}
	return s.Metadata.Name, out, nil
}

// nodeHardware is the hardware of a spec member as the policy groups it into
// families: the CPU model, and the GPU product and count.
func nodeHardware(m clusterspec.Member) policy.NodeHardwareInfo {
	var info policy.NodeHardwareInfo
	if m.Profile == nil {
		return info
	}
	info.CPUModel = m.Profile.CPU.ModelName.V
	if g := m.Profile.GPUs; g != nil {
		info.GPUModel, info.GPUCount = g.Product.V, g.Count.V
	}
	return info
}

// runTolerations are the tolerations of every pod of a run: the scenario's
// placement, or the simulator's default, the KWOK taint.
func runTolerations(s Scenario) []corev1.Toleration {
	if p := s.Workload.Placement; p != nil && p.Tolerations != nil {
		return p.Tolerations
	}
	return []corev1.Toleration{{Key: clusterspec.KWOKTaintKey, Operator: corev1.TolerationOpEqual, Value: clusterspec.KWOKValue, Effect: corev1.TaintEffectNoSchedule}}
}

// Capacity sums the allocatable resources of the Nodes some of the jobs'
// pods can use: every NoSchedule and NoExecute taint is tolerated, and the
// placement's node selector, when it sets one, matches the Node's labels.
// A pod tolerates the run's tolerations plus, for each extended resource
// it requests, the taint keyed by that resource, as Kubernetes'
// ExtendedResourceToleration admission plugin adds: a GPU Node carrying
// its vendor's taint (nvidia.com/gpu, amd.com/gpu) counts only when some
// job requests its GPUs. The simulator's default selector is the spec's
// own label, which every Node matches. Values are in cores for cpu, bytes
// for memory and units otherwise.
func Capacity(pools []SpecPool, jobs []TraceJob, nodeSelector map[string]string, tolerations []corev1.Toleration) map[string]float64 {
	// Each distinct set of extended resources the jobs request is one set
	// of tolerations; the empty set is a pod requesting none.
	kinds := map[string][]string{"": nil}
	for _, j := range jobs {
		var ext []string
		for name, v := range jobRequests(j) {
			if name != string(corev1.ResourceCPU) && name != string(corev1.ResourceMemory) && v > 0 {
				ext = append(ext, name)
			}
		}
		sort.Strings(ext)
		kinds[strings.Join(ext, ",")] = ext
	}
	out := map[string]float64{}
	for _, p := range pools {
		for _, n := range p.Nodes {
			usable := false
			for _, ext := range kinds {
				if nodeUsable(n, nodeSelector, withResourceTolerations(tolerations, ext)) {
					usable = true
					break
				}
			}
			if !usable {
				continue
			}
			for name, q := range n.Status.Allocatable {
				if name == corev1.ResourcePods {
					continue
				}
				out[string(name)] += q.AsApproximateFloat64()
			}
		}
	}
	return out
}

// withResourceTolerations returns base plus, per extended resource, the
// toleration of the NoSchedule taint keyed by it.
func withResourceTolerations(base []corev1.Toleration, resources []string) []corev1.Toleration {
	out := append([]corev1.Toleration(nil), base...)
	for _, r := range resources {
		out = append(out, corev1.Toleration{Key: r, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
	}
	return out
}

func nodeUsable(n *corev1.Node, nodeSelector map[string]string, tolerations []corev1.Toleration) bool {
	for k, v := range nodeSelector {
		if n.Labels[k] != v {
			return false
		}
	}
	for i := range n.Spec.Taints {
		t := &n.Spec.Taints[i]
		if t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for j := range tolerations {
			if tolerations[j].ToleratesTaint(t) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

// OfferedLoad is the demand of one resource over a window: Demand is the
// sum over due jobs of request x ideal runtime (resource-seconds), and Load
// is Demand over Capacity x Window.
type OfferedLoad struct {
	Resource string
	Demand   float64
	Capacity float64
	Window   time.Duration
	Load     float64
}

// OverloadError is what plan refuses: the resources whose offered load
// exceeds MaxOfferedLoad on every usable Node (Over), work whose earliest
// end there takes more than MaxOfferedLoad of the deadline (EarliestEnd
// and Deadline set), and the classes a variant's policy confines whose load
// on the Nodes left to them breaks either limit (Classes).
type OverloadError struct {
	Over                  []OfferedLoad
	EarliestEnd, Deadline time.Duration
	Classes               []ClassLoad
}

func (e *OverloadError) Error() string {
	var parts []string
	if over := overLimit(e.Over); over != "" {
		parts = append(parts, fmt.Sprintf("offered load above %.2f: %s", MaxOfferedLoad, over))
	}
	if e.Deadline > 0 {
		parts = append(parts, fmt.Sprintf("the work cannot end before %s, past %.2f of the %s deadline", e.EarliestEnd.Round(time.Second), MaxOfferedLoad, e.Deadline))
	}
	for _, c := range e.Classes {
		line := fmt.Sprintf("%s jobs on the %d Nodes %s leaves them:", c.Class, c.Nodes, c.Policy)
		if over := overLimit(c.Load); over != "" {
			line += fmt.Sprintf(" offered load above %.2f: %s;", MaxOfferedLoad, over)
		}
		parts = append(parts, fmt.Sprintf("%s earliest end %s of the %s deadline", line, c.EarliestEnd.Round(time.Second), c.Deadline))
	}
	return strings.Join(parts, "; ") + "; lower the job count or runtimes, or lengthen the duration or the drain"
}

// overLimit lists the loads above MaxOfferedLoad, "cpu 1.05, amd.com/gpu 1.23".
func overLimit(loads []OfferedLoad) string {
	var parts []string
	for _, l := range loads {
		if l.Load > MaxOfferedLoad || math.IsInf(l.Load, 1) {
			parts = append(parts, fmt.Sprintf("%s %.2f", l.Resource, l.Load))
		}
	}
	return strings.Join(parts, ", ")
}

// ClassLoad is the load of the jobs of one class on the Nodes a variant's
// policy leaves them, and the earliest end of their work there.
type ClassLoad struct {
	Class string
	// Policy describes the policy and its setting, "static_partition at
	// STATIC_HP_FRAC 0.5"; Variants are the variants that run with it.
	Policy   string
	Variants []string
	// Nodes counts the spec's Nodes the class may use.
	Nodes       int
	Load        []OfferedLoad
	EarliestEnd time.Duration
	Deadline    time.Duration
}

// Over reports whether the class breaks a limit: an offered load above
// MaxOfferedLoad, or an earliest end past MaxOfferedLoad of the deadline.
func (c ClassLoad) Over() bool {
	return overLimit(c.Load) != "" || pastDeadline(c.EarliestEnd, c.Deadline)
}

// pastDeadline reports whether end takes more than MaxOfferedLoad of a
// deadline; a scenario with no deadline has none to pass.
func pastDeadline(end, deadline time.Duration) bool {
	return deadline > 0 && end.Seconds() > MaxOfferedLoad*deadline.Seconds()
}

// EarliestEnd is the earliest time, from the start of the run, at which every
// job can have ended with the capacity free at the start and each job at its
// ideal runtime from the moment it is due: the latest, over the jobs, of the
// due time plus the runtime, and over the resources and the due times T, of
// T plus the work due from T on over the capacity. How the requests pack
// onto Nodes is ignored, so a run ends later, never earlier. A resource with
// no capacity is skipped; checkTraceResources reports a job that needs it.
func EarliestEnd(jobs []TraceJob, capacity map[string]float64, base float64) time.Duration {
	sorted := append([]TraceJob(nil), jobs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].SubmitTimeOffsetSec < sorted[j].SubmitTimeOffsetSec })
	runtimes := make([]float64, len(sorted))
	requests := make([]map[string]float64, len(sorted))
	end := 0.0
	for i, j := range sorted {
		runtimes[i] = IdealRuntime(j, base).Seconds()
		requests[i] = jobRequests(j)
		end = math.Max(end, j.SubmitTimeOffsetSec+runtimes[i])
	}
	for name, c := range capacity {
		if c <= 0 {
			continue
		}
		work := 0.0
		for i := len(sorted) - 1; i >= 0; i-- {
			work += requests[i][name] * runtimes[i]
			end = math.Max(end, sorted[i].SubmitTimeOffsetSec+work/c)
		}
	}
	return time.Duration(end * float64(time.Second))
}

// deadline is the end of the run: its duration plus its drain.
func (s Scenario) deadline() time.Duration {
	d := s.Duration.Duration
	if s.Drain != nil {
		d += s.Drain.Duration
	}
	return d
}

// The policy settings ConfinedLoads reads from a variant's joulie.set, with
// the controller manager's defaults and the chart's Node selector.
const (
	confinedClass         = "performance"
	policyStaticPartition = "static_partition"
	defaultStaticHPFrac   = 0.5
	defaultNodeSelector   = "joulie.io/managed=true"
)

// ConfinedLoads returns the load of the performance jobs on the Nodes each
// policy of the scenario's variants leaves them: under static_partition,
// which keeps STATIC_HP_FRAC of the managed Nodes of every hardware family
// in performance (policy.BuildStaticPlan, before any Node is labelled), the
// extender keeps performance pods off the others, the eco Nodes. A variant
// without Joulie, or with a policy that grows its performance set with the
// demand, confines no class. The policy, STATIC_HP_FRAC and NODE_SELECTOR
// are read from joulie.set (controllerManager.env.*); a values file is not
// read. Variants with the same setting share one entry.
func ConfinedLoads(s Scenario, pools []SpecPool, jobs []TraceJob, nodeSelector map[string]string, tolerations []corev1.Toleration) ([]ClassLoad, error) {
	var class []TraceJob
	for _, j := range jobs {
		if j.IntentClass == confinedClass {
			class = append(class, j)
		}
	}
	if len(class) == 0 {
		return nil, nil
	}
	var out []ClassLoad
	byPolicy := map[string]int{}
	for _, name := range s.RunVariants() {
		v, err := s.ForVariant(name)
		if err != nil {
			return nil, err
		}
		if v.Joulie.Enabled != nil && !*v.Joulie.Enabled {
			continue
		}
		set := v.Joulie.Set
		policyType := strings.ToLower(string(set[helmControllerEnv+"POLICY_TYPE"]))
		if policyType == "" {
			policyType = policyStaticPartition
		}
		if policyType != policyStaticPartition {
			continue
		}
		frac := defaultStaticHPFrac
		if raw := string(set[helmControllerEnv+"STATIC_HP_FRAC"]); raw != "" {
			if frac, err = strconv.ParseFloat(raw, 64); err != nil {
				return nil, fmt.Errorf("variant %q: %sSTATIC_HP_FRAC %q: %w", name, helmControllerEnv, raw, err)
			}
		}
		selector := defaultNodeSelector
		if raw, ok := set[helmControllerEnv+"NODE_SELECTOR"]; ok {
			selector = string(raw)
		}
		managed, err := labels.Parse(selector)
		if err != nil {
			return nil, fmt.Errorf("variant %q: %sNODE_SELECTOR %q: %w", name, helmControllerEnv, selector, err)
		}
		desc := fmt.Sprintf("%s at STATIC_HP_FRAC %g", policyStaticPartition, frac)
		if selector != defaultNodeSelector {
			desc += fmt.Sprintf(" over %s", selector)
		}
		if i, ok := byPolicy[desc]; ok {
			out[i].Variants = append(out[i].Variants, name)
			continue
		}
		left := staticPartitionLeaves(pools, managed, frac)
		capacity := Capacity(left, class, nodeSelector, tolerations)
		c := ClassLoad{
			Class: confinedClass, Policy: desc, Variants: []string{name},
			Load:        ComputeOfferedLoad(class, capacity, s.Duration.Duration, s.baseSpeed()),
			EarliestEnd: EarliestEnd(class, capacity, s.baseSpeed()),
			Deadline:    s.deadline(),
		}
		for _, p := range left {
			c.Nodes += len(p.Nodes)
		}
		byPolicy[desc] = len(out)
		out = append(out, c)
	}
	return out, nil
}

// staticPartitionLeaves returns the pools without the Nodes static_partition
// puts in eco at hpFrac: the managed Nodes outside policy.BuildStaticPlan's
// performance set. Unmanaged Nodes carry no profile and stay.
func staticPartitionLeaves(pools []SpecPool, managed labels.Selector, hpFrac float64) []SpecPool {
	var names []string
	hw := map[string]policy.NodeHardwareInfo{}
	for _, p := range pools {
		for i, n := range p.Nodes {
			if managed.Matches(labels.Set(n.Labels)) {
				names = append(names, n.Name)
				hw[n.Name] = p.Hardware[i]
			}
		}
	}
	eco := map[string]bool{}
	for _, a := range policy.BuildStaticPlan(names, hw, nil, 0, 0, hpFrac) {
		if a.Profile != "performance" {
			eco[a.NodeName] = true
		}
	}
	out := make([]SpecPool, 0, len(pools))
	for _, p := range pools {
		kept := SpecPool{Name: p.Name}
		for i, n := range p.Nodes {
			if !eco[n.Name] {
				kept.Nodes = append(kept.Nodes, n)
				kept.Hardware = append(kept.Hardware, p.Hardware[i])
			}
		}
		out = append(out, kept)
	}
	return out
}

// jobRequests returns a trace job's requests as numbers, with the
// simulator loader's defaults: one core when cpu is missing.
func jobRequests(j TraceJob) map[string]float64 {
	out := map[string]float64{string(corev1.ResourceCPU): 1}
	for name, raw := range j.PodTemplate.Requests {
		q, err := resource.ParseQuantity(raw)
		if err != nil {
			continue
		}
		out[name] = q.AsApproximateFloat64()
	}
	return out
}

// gpuRequest returns the job's extended resource and its count; "" when it
// requests none.
func gpuRequest(req map[string]float64) (string, float64) {
	for name, v := range req {
		if name != string(corev1.ResourceCPU) && name != string(corev1.ResourceMemory) && v > 0 {
			return name, v
		}
	}
	return "", 0
}

// IdealRuntime is a trace job's runtime at full speed: its CPU work over
// cpu request x base, or its GPU work over GPU count x base when longer.
// Missing CPU work is the simulator loader's default of 1000 units.
func IdealRuntime(j TraceJob, base float64) time.Duration {
	req := jobRequests(j)
	cpuUnits := j.Work.CPUUnits
	if cpuUnits <= 0 {
		cpuUnits = 1000
	}
	sec := cpuUnits / (req[string(corev1.ResourceCPU)] * base)
	if _, gpus := gpuRequest(req); gpus > 0 && j.Work.GPUUnits > 0 {
		sec = math.Max(sec, j.Work.GPUUnits/(gpus*base))
	}
	return time.Duration(sec * float64(time.Second))
}

// ComputeOfferedLoad returns the offered load of every resource the jobs
// request or the capacity holds, sorted by resource. A resource requested
// with no capacity has an infinite load.
func ComputeOfferedLoad(jobs []TraceJob, capacity map[string]float64, window time.Duration, base float64) []OfferedLoad {
	demand := map[string]float64{}
	for name := range capacity {
		demand[name] = 0
	}
	for _, j := range jobs {
		rt := IdealRuntime(j, base).Seconds()
		for name, v := range jobRequests(j) {
			demand[name] += v * rt
		}
	}
	out := make([]OfferedLoad, 0, len(demand))
	for name, d := range demand {
		l := OfferedLoad{Resource: name, Demand: d, Capacity: capacity[name], Window: window}
		switch denom := l.Capacity * window.Seconds(); {
		case denom > 0:
			l.Load = d / denom
		case d > 0:
			l.Load = math.Inf(1)
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	return out
}

// checkTraceResources fails when a job requests an extended resource no
// usable pool has.
func checkTraceResources(jobs []TraceJob, capacity map[string]float64) error {
	if parts := missingResources(jobs, func(name string) bool { return capacity[name] > 0 }); len(parts) > 0 {
		return fmt.Errorf("no usable pool has the requested resource %s", strings.Join(parts, "; "))
	}
	return nil
}

// missingResources lists, sorted, each resource beyond cpu and memory that
// some job requests and has reports missing, with up to three of the jobs:
// "nvidia.com/gpu (jobs a, b, c and 2 more)".
func missingResources(jobs []TraceJob, has func(name string) bool) []string {
	missing := map[string][]string{}
	for _, j := range jobs {
		for name, v := range jobRequests(j) {
			if name != string(corev1.ResourceCPU) && name != string(corev1.ResourceMemory) && v > 0 && !has(name) {
				missing[name] = append(missing[name], j.JobID)
			}
		}
	}
	var parts []string
	for name, ids := range missing {
		more := ""
		if len(ids) > 3 {
			more = fmt.Sprintf(" and %d more", len(ids)-3)
			ids = ids[:3]
		}
		parts = append(parts, fmt.Sprintf("%s (jobs %s%s)", name, strings.Join(ids, ", "), more))
	}
	sort.Strings(parts)
	return parts
}
