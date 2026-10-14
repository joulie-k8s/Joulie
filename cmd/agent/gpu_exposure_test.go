package main

import (
	"context"
	"strings"
	"testing"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// GPU exposures beside whole devices: MIG, time-slicing and DRA. A12 in
// hwemu_test.go covers the caps on MIG devices and shared replicas; these
// cover the count the agent publishes and the nodes that advertise no GPU
// resource.

// publishedHardware is the NodeHardware status the agent wrote, decoded
// through the typed API.
func publishedHardware(t *testing.T, dyn *dynamicfake.FakeDynamicClient, node string) v1alpha1.NodeHardwareStatus {
	t.Helper()
	u, err := dyn.Resource(nodeHardwareGVR).Get(context.Background(), sanitizeNodeObjectName(node), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get NodeHardware: %v", err)
	}
	var nh v1alpha1.NodeHardware
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &nh); err != nil {
		t.Fatalf("decode NodeHardware: %v", err)
	}
	return nh.Status
}

// A12b: without GPU tools (no nvidia-smi on the node) the agent publishes
// the physical GPU count GPU feature discovery labels, 8, for every
// exposure but mig-single, where no label states it and the 56 MIG devices
// stand in; and the catalog model of the product label without its
// -MIG-<profile> or -SHARED suffix. The single MIG strategy label alone,
// which the GPU Operator sets on every GPU node by default, does not hide
// the physical count. A GPU cap is then attempted and fails on the
// inventory, as on whole GPUs without tools, instead of being blocked for
// want of a GPU resource.
func TestHwemuA12bWithoutToolsTheGPUCountComesFromTheLabels(t *testing.T) {
	wantModel, _, ok := loadHardwareCatalog().MatchGPU("NVIDIA-H100-NVL")
	if !ok {
		t.Fatal("the catalog has no NVIDIA-H100-NVL")
	}
	cases := []struct {
		name  string
		mode  string
		count int
	}{
		{"whole", "whole", 8},
		{"mig-single", "mig-single", 56},
		{"mig-mixed", "mig-mixed", 8},
		{"timeslice-rename", "timeslice-rename", 8},
		{"timeslice-norename", "timeslice-norename", 8},
		{"dra", "dra", 8},
		{"timeslice-norename, mig.strategy=single", "timeslice-norename", 8},
		{"dra, mig.strategy=single", "dra", 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			noTools := func(p *hwemu.Profile) { p.GPUs.Tools = nil }
			_, tree, p := hwemutest.NewNode(t, "nvidia-h100-nvl-8gpu", variant(withExposure(c.mode), noTools))
			e := useEmulatedNode(t, tree, nil)
			node := e.nodeObject(p)
			if strings.HasSuffix(c.name, "mig.strategy=single") {
				node.Labels["nvidia.com/mig.strategy"] = "single"
			}
			dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithGPUCap(e.name, float64Ptr(250), nil))
			if err != nil {
				t.Fatalf("reconcileOnce: %v", err)
			}
			gpu := publishedHardware(t, dyn, e.name).GPU
			if gpu == nil {
				t.Fatal("NodeHardware has no status.gpu")
			}
			if gpu.Count != c.count || gpu.RawModel != "NVIDIA-H100-NVL" || gpu.Model != wantModel {
				t.Errorf("NodeHardware gpu count %d rawModel %q model %q, want %d, NVIDIA-H100-NVL and %q (D18)",
					gpu.Count, gpu.RawModel, gpu.Model, c.count, wantModel)
			}
			s := controlStatus(t, dyn, e.name, "gpu")
			if s["backend"] != "none" || s["result"] != "error" || !strings.HasPrefix(s["message"].(string), "gpu inventory failed") {
				t.Errorf("gpu control status %v, want none/error/gpu inventory failed..., as on whole GPUs without tools", s)
			}
		})
	}
}

// A12c: a DRA node advertises no GPU resource. The agent still caps its
// GPUs when GPU feature discovery labels them or when nvidia-smi lists
// them; with neither, as on a node without GPUs, the cap stays blocked.
func TestHwemuA12cDRANodeIsCappedWhenItsGPUsAreKnown(t *testing.T) {
	cases := []struct {
		name   string
		labels bool
		tools  bool
		want   string
	}{
		{"labels and tools", true, true, "host-nvidia/applied"},
		{"tools only", false, true, "host-nvidia/applied"},
		{"neither", false, false, "none/blocked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutate := withExposure("dra")
			if !c.tools {
				mutate = variant(mutate, func(p *hwemu.Profile) { p.GPUs.Tools = nil })
			}
			_, tree, p := hwemutest.NewNode(t, "nvidia-h100-nvl-8gpu", mutate)
			e := useEmulatedNode(t, tree, nil)
			node := e.nodeObject(p)
			if !c.labels {
				for k := range node.Labels {
					if strings.HasPrefix(k, "nvidia.com/") {
						delete(node.Labels, k)
					}
				}
			}
			dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithGPUCap(e.name, float64Ptr(250), nil))
			if err != nil {
				t.Fatalf("reconcileOnce: %v", err)
			}
			s := controlStatus(t, dyn, e.name, "gpu")
			if got := s["backend"].(string) + "/" + s["result"].(string); got != c.want {
				t.Fatalf("gpu control status %v, want %s", s, c.want)
			}
			if c.want == "none/blocked" && s["message"] != "node has no allocatable GPU resources" {
				t.Errorf("gpu control message %q, want node has no allocatable GPU resources", s["message"])
			}
		})
	}
}
