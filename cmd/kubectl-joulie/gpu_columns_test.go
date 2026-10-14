package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
)

// gpuNode is a node advertising the given GPU resources.
func gpuNode(name string, res map[string]string) *corev1.Node {
	alloc := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("96")}
	for k, v := range res {
		alloc[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Allocatable: alloc}}
}

// gpuPodOn is a running pod on node requesting the given GPU resources.
func gpuPodOn(name, node string, res map[string]string) *corev1.Pod {
	req := corev1.ResourceList{}
	for k, v := range res {
		req[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{
			Name: "c", Resources: corev1.ResourceRequirements{Requests: req, Limits: req},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// The GPU columns show the GPUs of every node that advertises any GPU
// resource pkg/api classifies, and the share of them pods requested, in
// physical GPUs. A DRA node advertises no GPU resource: its GPU CAP column
// shows the cap its twin's spec carries, and its GPU% column has nothing to
// count.
func TestStatusGPUColumnsShowSharedAndPartitionedGPUs(t *testing.T) {
	kube := fake.NewSimpleClientset(
		gpuNode("whole", map[string]string{"nvidia.com/gpu": "8"}),
		gpuNode("mig", map[string]string{"nvidia.com/mig-1g.12gb": "56"}),
		gpuNode("shared", map[string]string{"nvidia.com/gpu.shared": "32"}),
		gpuNode("amd-cpx", map[string]string{"amd.com/cpx_nps4": "64"}),
		gpuNode("cpu", nil),
		gpuNode("dra", nil),
		gpuPodOn("a", "whole", map[string]string{"nvidia.com/gpu": "2"}),
		gpuPodOn("b", "mig", map[string]string{"nvidia.com/mig-1g.12gb": "7"}),
		gpuPodOn("c", "shared", map[string]string{"nvidia.com/gpu.shared": "8"}),
		gpuPodOn("d", "amd-cpx", map[string]string{"amd.com/cpx_nps4": "32"}),
	)
	var states []nodeState
	for _, n := range []string{"amd-cpx", "cpu", "mig", "shared", "whole"} {
		states = append(states, nodeState{name: n, class: "eco", gpuCapPct: 60})
	}
	states = append(states, parseNodeState(unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "dra"},
		"spec": map[string]interface{}{"gpu": map[string]interface{}{
			"powerCap": map[string]interface{}{"scope": "perGpu", "capPctOfMax": int64(60)},
		}},
		"status": map[string]interface{}{
			"schedulableClass":  "eco",
			"effectiveCapState": map[string]interface{}{"cpuPct": int64(100), "gpuPct": int64(60)},
		},
	}}))
	enrichWithResources(states, fetchNodeResources(kube))

	out, err := os.Create(filepath.Join(t.TempDir(), "status.txt"))
	if err != nil {
		t.Fatal(err)
	}
	renderStatus(out, states)
	out.Close()
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	// GPU% per node; GPU CAP is 60% on every GPU node, "-" on the CPU-only one.
	want := map[string]string{"whole": "25%", "mig": "12%", "shared": "25%", "amd-cpx": "50%", "dra": "-", "cpu": "-"}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		w, ok := want[f[0]]
		if !ok {
			continue
		}
		delete(want, f[0])
		gpuCap := "60%"
		if f[0] == "cpu" {
			gpuCap = "-"
		}
		if f[6] != w || f[9] != gpuCap {
			t.Errorf("node %s: GPU%% %s and GPU CAP %s, want %s and %s\n%s", f[0], f[6], f[9], w, gpuCap, line)
		}
	}
	if len(want) > 0 {
		t.Errorf("no status row for %v in:\n%s", want, b)
	}
}
