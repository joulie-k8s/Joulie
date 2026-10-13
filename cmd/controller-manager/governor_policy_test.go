package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/kube"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const testGPUProduct = "TEST-GPU"

// governorNode is a managed node at profile; gpus > 0 makes it a GPU node
// with the product label testGPUProduct.
func governorNode(name, profile string, gpus int) *corev1.Node {
	n := managedNode(name, profile)
	if gpus > 0 {
		n.Labels["joulie.io/gpu.product"] = testGPUProduct
		n.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(int64(gpus), resource.DecimalSI)}
	}
	return n
}

// governorHardware is the NodeHardware an agent with governor discovery
// publishes, decoded the way the API server's JSON is. nil presets publish
// no governor at all, as an agent from before governor discovery does.
func governorHardware(t *testing.T, node string, cpuPresets, gpuPresets []string, gpus int) *v1alpha1.NodeHardware {
	t.Helper()
	support := func(presets []string) []map[string]any {
		out := []map[string]any{}
		for _, p := range presets {
			out = append(out, map[string]any{"preset": p, "result": "applied", "native": p})
		}
		return out
	}
	cpu := map[string]any{"vendor": "AuthenticAMD", "rawModel": "Test CPU", "sockets": 1, "totalCores": 8}
	if cpuPresets != nil {
		cpu["governor"] = map[string]any{
			"driver": "acpi-cpufreq", "current": "schedutil", "writable": true,
			"boost":   map[string]any{"state": "on", "scope": "global"},
			"presets": support(cpuPresets),
		}
	}
	status := map[string]any{"cpu": cpu}
	if gpus > 0 {
		gpu := map[string]any{"present": true, "rawModel": "Test GPU", "count": gpus}
		if gpuPresets != nil {
			gpu["governor"] = map[string]any{
				"mechanism": "nvidia-smi", "current": "unknown", "writable": true, "uniform": true,
				"presets": support(gpuPresets),
			}
		}
		status["gpu"] = gpu
	}
	raw, err := json.Marshal(map[string]any{
		"apiVersion": "joulie.io/v1alpha1", "kind": "NodeHardware",
		"metadata": map[string]any{"name": node}, "spec": map[string]any{"nodeName": node},
		"status": status,
	})
	if err != nil {
		t.Fatalf("encode NodeHardware: %v", err)
	}
	return nodeHardwareFromJSON(t, string(raw))
}

// governorCluster is two CPU-only nodes and two GPU nodes that accept every
// preset. The fraction is 0, so each family keeps one performance node: c0
// and g0 stay in performance, c1 and g1 go to eco.
func governorCluster(t *testing.T, extra ...runtime.Object) []runtime.Object {
	t.Helper()
	cpus, gpus := joulie.CPUGovernorPresetValues, joulie.GPUGovernorPresetValues
	return append([]runtime.Object{
		governorNode("c0", "performance", 0), governorNode("c1", "performance", 0),
		governorNode("g0", "performance", 4), governorNode("g1", "performance", 4),
		governorHardware(t, "c0", cpus, nil, 0), governorHardware(t, "c1", cpus, nil, 0),
		governorHardware(t, "g0", cpus, gpus, 4), governorHardware(t, "g1", cpus, gpus, 4),
	}, extra...)
}

// performancePodOn is a running performance pod, which keeps a node the
// policy moves to eco draining.
func performancePodOn(node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "train-" + node, Namespace: "default",
			Annotations: map[string]string{"joulie.io/workload-class": "performance"}},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func reconcileGovernors(t *testing.T, policyType string, reader kube.Reader, client kubernetes.Interface, dyn dynamic.Interface) {
	t.Helper()
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	if err := reconcile(
		context.Background(), reader, client, dyn, selector,
		"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
		5000, 120, policyType, 0, 0, 1, 5, 10,
		100, 60, false, 100, 60, false,
		map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"},
	); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
}

// twinSpec reads a NodeTwin spec through the typed API, as the agent does.
func twinSpec(t *testing.T, dyn dynamic.Interface, node string) v1alpha1.NodeTwinSpec {
	t.Helper()
	u, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), node, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get NodeTwin %s: %v", node, err)
	}
	var nt v1alpha1.NodeTwin
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &nt); err != nil {
		t.Fatalf("decode NodeTwin %s: %v", node, err)
	}
	return nt.Spec
}

func cpuGovernorOf(s v1alpha1.NodeTwinSpec) *v1alpha1.CPUGovernorIntent {
	if s.CPU == nil {
		return nil
	}
	return s.CPU.Governor
}

func gpuGovernorOf(s v1alpha1.NodeTwinSpec) *v1alpha1.GPUGovernorIntent {
	if s.GPU == nil {
		return nil
	}
	return s.GPU.Governor
}

func governorString(s v1alpha1.NodeTwinSpec) string {
	out := "cpu="
	if g := cpuGovernorOf(s); g != nil {
		out += g.Preset
		if g.Boost != "" {
			out += "+boost-" + g.Boost
		}
	} else {
		out += "none"
	}
	out += " gpu="
	if g := gpuGovernorOf(s); g != nil {
		out += g.Preset
		if g.ComputeClockCeilingPctOfMax != nil {
			out += fmt.Sprintf("@%g", *g.ComputeClockCeilingPctOfMax)
		}
	} else {
		out += "none"
	}
	return out
}

// wantGovernors checks the governors of each node, spelled as
// governorString spells them.
func wantGovernors(t *testing.T, dyn dynamic.Interface, want map[string]string) {
	t.Helper()
	for node, w := range want {
		if got := governorString(twinSpec(t, dyn, node)); got != w {
			t.Errorf("node %s: %s, want %s", node, got, w)
		}
	}
}

// GC01: with every governor variable unset, no NodeTwin spec carries a
// governor, whatever the node type, its profile (draining included) or the
// policy, even on nodes that accept every preset.
func TestGC01DefaultsWriteNoGovernor(t *testing.T) {
	useGovernorEnv(t, nil)
	for _, policyType := range []string{"static_partition", "queue_aware_v1", "rule_swap_v1"} {
		t.Run(policyType, func(t *testing.T) {
			reader, client, dyn := fakeClients(t, governorCluster(t, performancePodOn("c1"), performancePodOn("g1"))...)
			reconcileGovernors(t, policyType, reader, client, dyn)
			list, err := dyn.Resource(nodeTwinGVR).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatalf("list NodeTwins: %v", err)
			}
			if len(list.Items) != 4 {
				t.Fatalf("%d NodeTwins, want 4", len(list.Items))
			}
			for _, u := range list.Items {
				for _, part := range []string{"cpu", "gpu"} {
					if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", part, "governor"); found {
						t.Errorf("NodeTwin %s carries spec.%s.governor with GOVERNOR_CONTROL unset: %v", u.GetName(), part, u.Object["spec"])
					}
				}
			}
		})
	}
}

// GC02: with all, a CPU-only performance node gets performance and an eco
// node efficiency; CPU_GOVERNOR_ECO_BOOST goes with the eco preset only.
func TestGC02AllWritesTheProfilePresets(t *testing.T) {
	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "all"})
	reader, client, dyn := fakeClients(t, governorCluster(t)...)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	wantGovernors(t, dyn, map[string]string{"c0": "cpu=performance gpu=none", "c1": "cpu=efficiency gpu=none"})
	if p := twinSpec(t, dyn, "c1").Profile; p != profileEco {
		t.Fatalf("c1 profile %q, want eco", p)
	}

	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "all", "CPU_GOVERNOR_ECO_BOOST": "off"})
	reader, client, dyn = fakeClients(t, governorCluster(t)...)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	wantGovernors(t, dyn, map[string]string{"c0": "cpu=performance gpu=none", "c1": "cpu=efficiency+boost-off gpu=none"})
}

// GC03: a GPU node gets no spec.cpu.governor, the rule the CPU cap follows,
// unless CPU_GOVERNOR_ON_GPU_NODES is true.
func TestGC03GPUNodeGetsNoCPUGovernorByDefault(t *testing.T) {
	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "all"})
	reader, client, dyn := fakeClients(t, governorCluster(t)...)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	wantGovernors(t, dyn, map[string]string{"g0": "cpu=none gpu=none", "g1": "cpu=none gpu=efficient@80"})

	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "all", "CPU_GOVERNOR_ON_GPU_NODES": "true"})
	reader, client, dyn = fakeClients(t, governorCluster(t)...)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	wantGovernors(t, dyn, map[string]string{"g0": "cpu=performance gpu=none", "g1": "cpu=efficiency gpu=efficient@80"})
}

// GC04: a node the policy moves to eco while performance pods still run on it
// keeps the performance presets until they finish, as it keeps the
// performance caps (TestDrainingNodeKeepsPerformanceCaps).
func TestGC04DrainingNodeKeepsPerformancePresets(t *testing.T) {
	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "all", "GPU_GOVERNOR_PERFORMANCE_PRESET": "sustained"})
	reader, client, dyn := fakeClients(t, governorCluster(t, performancePodOn("c1"), performancePodOn("g1"))...)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	for _, node := range []string{"c1", "g1"} {
		s := twinSpec(t, dyn, node)
		if s.Scheduling == nil || !s.Scheduling.Draining || s.Profile != profileEco {
			t.Fatalf("%s should be draining toward eco: %+v", node, s)
		}
	}
	wantGovernors(t, dyn, map[string]string{"c1": "cpu=performance gpu=none", "g1": "cpu=none gpu=sustained"})
}

// GC05: a preset the node does not list is not written, for the CPU and the
// GPU alike, and nothing else is written in its place (auto included); the
// warning comes once per node, not on every reconcile.
func TestGC05UnlistedPresetIsNotWritten(t *testing.T) {
	buf := captureLog(t)
	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "all"})
	cpuNoEfficiency := []string{"performance", "balanced"}
	gpuNoEfficient := []string{"auto", "sustained", "low", "peak"}
	objs := []runtime.Object{
		governorNode("c0", "performance", 0), governorNode("c1", "performance", 0),
		governorNode("g0", "performance", 4), governorNode("g1", "performance", 4),
		governorHardware(t, "c0", cpuNoEfficiency, nil, 0), governorHardware(t, "c1", cpuNoEfficiency, nil, 0),
		governorHardware(t, "g0", nil, gpuNoEfficient, 4), governorHardware(t, "g1", nil, gpuNoEfficient, 4),
		// An agent from before governor discovery lists nothing.
		governorNode("o0", "performance", 0), governorHardware(t, "o0", nil, nil, 0),
	}
	reader, client, dyn := fakeClients(t, objs...)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	wantGovernors(t, dyn, map[string]string{
		"c0": "cpu=performance gpu=none",
		"c1": "cpu=none gpu=none",
		"g0": "cpu=none gpu=none",
		"g1": "cpu=none gpu=none",
		"o0": "cpu=none gpu=none",
	})
	for _, w := range []struct{ node, what string }{
		{"c1", `CPU governor preset "efficiency" is not in NodeHardware`},
		{"g1", `GPU governor preset "efficient" is not in NodeHardware`},
		{"o0", `is not in NodeHardware status.cpu.governor.presets`},
	} {
		n := 0
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, "node="+w.node+" ") && strings.Contains(line, w.what) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("node %s: %d warnings %q over two reconciles, want 1:\n%s", w.node, n, w.what, buf)
		}
	}
}

// GC08: cpu writes only CPU governors, gpu only GPU governors.
func TestGC08ControlScopesTheComponent(t *testing.T) {
	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "cpu", "CPU_GOVERNOR_ON_GPU_NODES": "true", "GPU_GOVERNOR_PERFORMANCE_PRESET": "sustained"})
	reader, client, dyn := fakeClients(t, governorCluster(t)...)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	wantGovernors(t, dyn, map[string]string{
		"c0": "cpu=performance gpu=none", "c1": "cpu=efficiency gpu=none",
		"g0": "cpu=performance gpu=none", "g1": "cpu=efficiency gpu=none",
	})

	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "gpu", "CPU_GOVERNOR_ON_GPU_NODES": "true", "GPU_GOVERNOR_PERFORMANCE_PRESET": "sustained"})
	reader, client, dyn = fakeClients(t, governorCluster(t)...)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	wantGovernors(t, dyn, map[string]string{
		"c0": "cpu=none gpu=none", "c1": "cpu=none gpu=none",
		"g0": "cpu=none gpu=sustained", "g1": "cpu=none gpu=efficient@80",
	})
}

// GC09: turning control from all to off removes both governors in the next
// spec write, and leaves the caps as they were.
func TestGC09SwitchingOffRemovesTheGovernors(t *testing.T) {
	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "all", "CPU_GOVERNOR_ON_GPU_NODES": "true"})
	reader, client, dyn := fakeClients(t, governorCluster(t)...)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	wantGovernors(t, dyn, map[string]string{"c1": "cpu=efficiency gpu=none", "g1": "cpu=efficiency gpu=efficient@80"})
	before := map[string]v1alpha1.NodeTwinSpec{"c1": twinSpec(t, dyn, "c1"), "g1": twinSpec(t, dyn, "g1")}

	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "off"})
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	wantGovernors(t, dyn, map[string]string{
		"c0": "cpu=none gpu=none", "c1": "cpu=none gpu=none",
		"g0": "cpu=none gpu=none", "g1": "cpu=none gpu=none",
	})
	for node, b := range before {
		after := twinSpec(t, dyn, node)
		b.CPU.Governor = nil
		if b.GPU != nil {
			b.GPU.Governor = nil
		}
		if jb, ja := jsonOf(t, b), jsonOf(t, after); jb != ja {
			t.Errorf("node %s: spec after off = %s, want the spec before without its governors %s", node, ja, jb)
		}
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// writeRecorder wraps a dynamic client and records the field manager of every
// write.
type writeRecorder struct {
	dynamic.Interface
	mu     sync.Mutex
	writes []recordedWrite
}

type recordedWrite struct {
	verb, resource, subresource, manager string
	governor                             bool // the written spec carries a governor
}

func (r *writeRecorder) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &recordingResource{NamespaceableResourceInterface: r.Interface.Resource(gvr), rec: r, resource: gvr.Resource}
}

func (r *writeRecorder) add(verb, resource, manager string, obj *unstructured.Unstructured, sub []string) {
	w := recordedWrite{verb: verb, resource: resource, subresource: strings.Join(sub, "/"), manager: manager}
	if obj != nil {
		for _, part := range []string{"cpu", "gpu"} {
			if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", part, "governor"); found {
				w.governor = true
			}
		}
	}
	r.mu.Lock()
	r.writes = append(r.writes, w)
	r.mu.Unlock()
}

type recordingResource struct {
	dynamic.NamespaceableResourceInterface
	rec      *writeRecorder
	resource string
}

func (r *recordingResource) Create(ctx context.Context, obj *unstructured.Unstructured, opts metav1.CreateOptions, sub ...string) (*unstructured.Unstructured, error) {
	r.rec.add("create", r.resource, opts.FieldManager, obj, sub)
	return r.NamespaceableResourceInterface.Create(ctx, obj, opts, sub...)
}

func (r *recordingResource) Update(ctx context.Context, obj *unstructured.Unstructured, opts metav1.UpdateOptions, sub ...string) (*unstructured.Unstructured, error) {
	r.rec.add("update", r.resource, opts.FieldManager, obj, sub)
	return r.NamespaceableResourceInterface.Update(ctx, obj, opts, sub...)
}

func (r *recordingResource) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, sub ...string) (*unstructured.Unstructured, error) {
	r.rec.add("patch", r.resource, opts.FieldManager, nil, sub)
	return r.NamespaceableResourceInterface.Patch(ctx, name, pt, data, opts, sub...)
}

// GC10: the spec writes that carry governors, the create and the later
// update alike, carry the controller manager's field manager.
func TestGC10GovernorSpecWritesCarryTheControllerManagerFieldManager(t *testing.T) {
	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "all"})
	reader, client, fakeDyn := fakeClients(t, governorCluster(t)...)
	rec := &writeRecorder{Interface: fakeDyn}
	reconcileGovernors(t, "static_partition", reader, client, rec)
	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "all", "CPU_GOVERNOR_ECO_PRESET": "balanced"})
	reconcileGovernors(t, "static_partition", reader, client, rec)
	wantGovernors(t, fakeDyn, map[string]string{"c1": "cpu=balanced gpu=none"})

	verbs := map[string]bool{}
	for _, w := range rec.writes {
		if w.resource != nodeTwinGVR.Resource {
			continue
		}
		if w.manager != joulie.FieldManagerControllerManager {
			t.Errorf("%s nodetwins/%s carries field manager %q, want %q", w.verb, w.subresource, w.manager, joulie.FieldManagerControllerManager)
		}
		if w.governor && w.subresource == "" {
			verbs[w.verb] = true
		}
	}
	if !verbs["create"] || !verbs["update"] {
		t.Fatalf("recorded governor spec writes %v, want a create and an update", verbs)
	}
}

// GC17: with all and the default presets, a performance GPU node carries no
// GPU governor (no data path is held unless the operator measured a gate on
// the model); a model mapped to sustained gets it; an eco GPU node gets
// efficient with the ceiling at 80.
func TestGC17DefaultGPUPresetsAndTheModelMap(t *testing.T) {
	useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "all"})
	reader, client, dyn := fakeClients(t, governorCluster(t)...)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	wantGovernors(t, dyn, map[string]string{"g0": "cpu=none gpu=none", "g1": "cpu=none gpu=efficient@80"})

	useGovernorEnv(t, map[string]string{
		"GOVERNOR_CONTROL":                "all",
		"GPU_GOVERNOR_MODEL_PRESETS_JSON": `{"` + testGPUProduct + `": {"performance": "sustained"}}`,
	})
	reader, client, dyn = fakeClients(t, governorCluster(t)...)
	reconcileGovernors(t, "static_partition", reader, client, dyn)
	wantGovernors(t, dyn, map[string]string{"g0": "cpu=none gpu=sustained", "g1": "cpu=none gpu=efficient@80"})
}
