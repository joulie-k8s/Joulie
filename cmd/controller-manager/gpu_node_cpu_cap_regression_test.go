package main

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

// A GPU node gets no eco CPU cap: capping its CPU saves little and slows the
// GPU data feed by more (exp-03), so the CPU of a GPU node runs as on a
// performance node whatever the node's profile. The old code skipped the
// percent cap on GPU nodes but left CapWatts at ECO_CAP_WATTS, which the spec
// then wrote as spec.cpu.packagePowerCapWatts, and the agent applied it: an
// eco GPU node had its CPU capped at 120 W.
func TestEcoGPUNodeCPUIsNotCapped(t *testing.T) {
	const perfW, ecoW = 5000.0, 120.0
	gpus := corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")}
	// Both GPU nodes are in performance and the fraction is 0, so the family
	// keeps one: g0 by name, and g1 goes to eco with nothing running on it.
	g0, g1 := managedNode("g0", "performance"), managedNode("g1", "performance")
	g0.Status.Allocatable, g1.Status.Allocatable = gpus, gpus
	reader, client, dyn := fakeClients(t, g0, g1)
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	if err := reconcile(
		context.Background(), reader, client, dyn, selector,
		"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
		perfW, ecoW, "static_partition", 0, 0, 1, 5, 10,
		100, 60, false, 100, 60, false,
		map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"},
	); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	cpuSpec := func(name string) (profile string, cpu map[string]any) {
		t.Helper()
		twin, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get NodeTwin %s: %v", name, err)
		}
		profile, _, _ = unstructured.NestedString(twin.Object, "spec", "profile")
		cpu, _, _ = unstructured.NestedMap(twin.Object, "spec", "cpu")
		if draining, _, _ := unstructured.NestedBool(twin.Object, "spec", "scheduling", "draining"); draining {
			t.Fatalf("%s runs no performance pod and must not be draining: spec=%v", name, twin.Object["spec"])
		}
		return profile, cpu
	}
	perfProfile, perfCPU := cpuSpec("g0")
	ecoProfile, ecoCPU := cpuSpec("g1")
	if perfProfile != profilePerformance || ecoProfile != profileEco {
		t.Fatalf("profiles g0=%q g1=%q, want performance and eco", perfProfile, ecoProfile)
	}
	if !reflect.DeepEqual(ecoCPU, perfCPU) {
		t.Fatalf("eco GPU node spec.cpu = %v, want what a performance GPU node gets (%v): no eco CPU cap on a GPU node", ecoCPU, perfCPU)
	}
}

// gpuNodeHardware is the NodeHardware of a node with four GPUs, decoded the
// way the API server's JSON is; control is status.gpu.controlAvailable.
func gpuNodeHardware(t *testing.T, node string, control bool) *v1alpha1.NodeHardware {
	t.Helper()
	return nodeHardwareFromJSON(t, fmt.Sprintf(`{"apiVersion": "joulie.io/v1alpha1", "kind": "NodeHardware",
		"metadata": {"name": %q}, "spec": {"nodeName": %q},
		"status": {"gpu": {"present": true, "rawModel": "Test GPU", "count": 4, "controlAvailable": %t}}}`, node, node, control))
}

// With CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES on, a GPU node whose agent
// reports no GPU control (Intel GPUs, NVIDIA boards whose limits read
// [N/A]) takes the CPU caps of its profile, as a CPU-only node does, instead
// of running uncapped in eco. A node whose GPUs the agent can cap, or one
// known only from its labels, keeps the GPU node rule; off, nothing changes.
func TestCPUEcoCapOnUncappableGPUNodes(t *testing.T) {
	const gpuNodeCPU = "map[packagePowerCapWatts:5000]"
	for _, tc := range []struct {
		name     string
		option   string
		hardware string // "uncappable", "cappable" or "labels"
		perf     string // spec.cpu of the performance node g0
		eco      string // spec.cpu of the eco node g1
	}{
		{"off by default", "", "uncappable", gpuNodeCPU, gpuNodeCPU},
		{"off", "false", "uncappable", gpuNodeCPU, gpuNodeCPU},
		{"on, GPUs without control", "true", "uncappable", "map[packagePowerCapPctOfMax:100]", "map[packagePowerCapPctOfMax:60]"},
		{"on, GPUs with control", "true", "cappable", gpuNodeCPU, gpuNodeCPU},
		{"on, no NodeHardware", "true", "labels", gpuNodeCPU, gpuNodeCPU},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CPU_ECO_CAP_ON_UNCAPPABLE_GPU_NODES", tc.option)
			loadCapOptions().install()
			t.Cleanup(func() { capOptions{}.install() })

			gpus := corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")}
			g0, g1 := managedNode("g0", "performance"), managedNode("g1", "performance")
			g0.Status.Allocatable, g1.Status.Allocatable = gpus, gpus
			objs := []runtime.Object{g0, g1}
			if tc.hardware != "labels" {
				objs = append(objs, gpuNodeHardware(t, "g0", tc.hardware == "cappable"), gpuNodeHardware(t, "g1", tc.hardware == "cappable"))
			}
			reader, client, dyn := fakeClients(t, objs...)
			selector, err := labels.Parse("joulie.io/managed=true")
			if err != nil {
				t.Fatalf("parse selector: %v", err)
			}
			if err := reconcile(
				context.Background(), reader, client, dyn, selector,
				"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
				5000, 120, "static_partition", 0, 0, 1, 5, 10,
				100, 60, false, 100, 60, false,
				map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"},
			); err != nil {
				t.Fatalf("reconcile error: %v", err)
			}
			for node, want := range map[string]string{"g0": tc.perf, "g1": tc.eco} {
				s := twinSpec(t, dyn, node)
				if wantProfile := map[string]string{"g0": profilePerformance, "g1": profileEco}[node]; s.Profile != wantProfile {
					t.Fatalf("%s profile %q, want %q", node, s.Profile, wantProfile)
				}
				twin, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), node, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("get NodeTwin %s: %v", node, err)
				}
				cpu, _, _ := unstructured.NestedMap(twin.Object, "spec", "cpu")
				if got := fmt.Sprint(cpu); got != want {
					t.Errorf("%s spec.cpu = %s, want %s", node, got, want)
				}
				if gpu, _, _ := unstructured.NestedMap(twin.Object, "spec", "gpu", "powerCap"); gpu == nil {
					t.Errorf("%s lost its GPU cap intent: spec=%v", node, twin.Object["spec"])
				}
			}
		})
	}
}
