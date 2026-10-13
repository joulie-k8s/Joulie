package scenario

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// MaxOfferedLoad is the highest offered load plan accepts for any
// resource. Above it queues grow for the whole window, so a completion
// assertion fails by construction rather than by Joulie's doing.
const MaxOfferedLoad = 0.9

// SpecPool is one pool of a cluster spec as the planner sees it: its Node
// count, the labels and taints of each Node, and each Node's allocatable
// resources, with the pool's overrides applied to its profile.
type SpecPool struct {
	Name        string
	Nodes       int
	Labels      map[string]string
	Taints      []corev1.Taint
	Allocatable corev1.ResourceList
}

// ReadSpecPools reads the cluster spec file spec (relative to repoRoot)
// strictly and returns its name and pools, each pool's profile taken from
// the builtin profiles with its overrides applied.
//
// It stands in for clusterspec's Load and Expand, which arrive in the same
// wave; it computes only what planning needs and leaves validation of the
// spec to them.
func ReadSpecPools(repoRoot, spec string) (string, []SpecPool, error) {
	path := filepath.Join(repoRoot, spec)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("cluster spec: %w", err)
	}
	var s clusterspec.Spec
	if err := yaml.UnmarshalStrict(b, &s); err != nil {
		return "", nil, fmt.Errorf("cluster spec %s: %w", spec, err)
	}
	var out []SpecPool
	for _, pool := range s.Spec.Pools {
		// BuiltinProfiles parses on every call, so one pool's overrides
		// never reach another pool's profile.
		builtins, err := hwemu.BuiltinProfiles()
		if err != nil {
			return "", nil, err
		}
		p, ok := builtins[pool.Profile]
		if !ok {
			return "", nil, fmt.Errorf("cluster spec %s: pool %s: profile %q is not builtin", spec, pool.Name, pool.Profile)
		}
		if err := applyPoolOverrides(p, s.Metadata.Name, pool); err != nil {
			return "", nil, fmt.Errorf("cluster spec %s: pool %s: %w", spec, pool.Name, err)
		}
		node := p.NodeObject(pool.Name)
		labels := map[string]string{}
		for _, m := range []map[string]string{node.Labels, s.Spec.Defaults.Labels, pool.Labels} {
			for k, v := range m {
				labels[k] = v
			}
		}
		labels[clusterspec.LabelCluster] = s.Metadata.Name
		labels[clusterspec.LabelPool] = pool.Name
		taints := append(append([]corev1.Taint(nil), node.Spec.Taints...), pool.Taints...)
		if s.Spec.Defaults.Kwok == nil || *s.Spec.Defaults.Kwok {
			taints = append(taints, corev1.Taint{Key: clusterspec.KWOKTaintKey, Value: clusterspec.KWOKValue, Effect: corev1.TaintEffectNoSchedule})
		}
		out = append(out, SpecPool{Name: pool.Name, Nodes: pool.Count, Labels: labels, Taints: taints, Allocatable: node.Status.Allocatable})
	}
	return s.Metadata.Name, out, nil
}

// applyPoolOverrides sets a pool's overrides on its profile with the
// provenance package clusterspec gives them, then validates the result.
func applyPoolOverrides(p *hwemu.Profile, spec string, pool clusterspec.Pool) error {
	o := pool.Overrides
	if o == nil {
		return nil
	}
	src := fmt.Sprintf("assumed:cluster spec %s pool %s override", spec, pool.Name)
	if o.NFD != "" {
		p.Node.NFD = o.NFD
	}
	if g := o.GPUs; g != nil {
		if p.GPUs == nil {
			return fmt.Errorf("overrides gpus, but profile %s has no GPUs", p.Name)
		}
		if g.PersistenceMode != "" {
			p.GPUs.PersistenceMode = hwemu.Fact[string]{V: g.PersistenceMode, Src: src}
		}
		if g.Exposure != "" {
			p.GPUs.Exposure = hwemu.Fact[string]{V: g.Exposure, Src: src}
		}
		if g.MIGProfile != "" {
			p.GPUs.MIGProfile = g.MIGProfile
		}
		if g.MIGDevicesPerGPU != nil {
			p.GPUs.MIGDevicesPerGPU = *g.MIGDevicesPerGPU
		}
		if g.TimesliceReplicas != nil {
			p.GPUs.TimesliceReplicas = *g.TimesliceReplicas
		}
	}
	return p.Validate()
}

// runTolerations are the tolerations of every pod of a run: the scenario's
// placement, or the simulator's default, the KWOK taint.
func runTolerations(s Scenario) []corev1.Toleration {
	if p := s.Workload.Placement; p != nil && p.Tolerations != nil {
		return p.Tolerations
	}
	return []corev1.Toleration{{Key: clusterspec.KWOKTaintKey, Operator: corev1.TolerationOpEqual, Value: clusterspec.KWOKValue, Effect: corev1.TaintEffectNoSchedule}}
}

// Capacity sums the allocatable resources of the pools whose Nodes some of
// the jobs' pods can use: every NoSchedule and NoExecute taint is
// tolerated, and the placement's node selector, when it sets one, matches.
// A pod tolerates the run's tolerations plus, for each extended resource
// it requests, the taint keyed by that resource, as Kubernetes'
// ExtendedResourceToleration admission plugin adds: a GPU pool carrying
// its vendor's taint (nvidia.com/gpu, amd.com/gpu) counts only when some
// job requests its GPUs. The simulator's default selector is the spec's
// own label, which every pool matches. Values are in cores for cpu, bytes
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
		usable := false
		for _, ext := range kinds {
			if poolUsable(p, nodeSelector, withResourceTolerations(tolerations, ext)) {
				usable = true
				break
			}
		}
		if !usable {
			continue
		}
		for name, q := range p.Allocatable {
			if name == corev1.ResourcePods {
				continue
			}
			out[string(name)] += float64(p.Nodes) * q.AsApproximateFloat64()
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

func poolUsable(p SpecPool, nodeSelector map[string]string, tolerations []corev1.Toleration) bool {
	for k, v := range nodeSelector {
		if p.Labels[k] != v {
			return false
		}
	}
	for i := range p.Taints {
		t := &p.Taints[i]
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

// OverloadError is the offered load of the resources above MaxOfferedLoad.
type OverloadError struct {
	Over []OfferedLoad
}

func (e *OverloadError) Error() string {
	parts := make([]string, len(e.Over))
	for i, l := range e.Over {
		parts[i] = fmt.Sprintf("%s %.2f", l.Resource, l.Load)
	}
	return fmt.Sprintf("offered load above %.2f: %s; lower the job count or runtimes, or lengthen the duration", MaxOfferedLoad, strings.Join(parts, ", "))
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
