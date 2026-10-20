package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/matbun/joulie/pkg/controller/policy"
)

// An Intel GPU node with no NodeHardware is planned as a GPU node from its
// labels: its gpu.intel.com/i915 units make it one, the chart's
// GPU_PRODUCT_LABEL_KEYS read the model from Intel's gpu.intel.com/product,
// the catalogue knows it, so the node gets its own hardware family and, with
// absolute caps, an eco cap of 60 % of the published 150 W.
func TestIntelGPUNodeFromLabelsIsPlannedWithItsModel(t *testing.T) {
	keys := parseCSVList(chartGPUProductLabelKeys(t))
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "flex-0", Labels: map[string]string{
			"gpu.intel.com/family":  "Flex_Series",
			"gpu.intel.com/product": "Flex_170",
		}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{"gpu.intel.com/i915": resource.MustParse("4")},
		},
	}
	nh := nodeHardwareFromLabels(node, loadHardwareCatalog(), keys)
	if nh.GPURawModel != "Flex_170" || nh.GPUModel != "INTEL_DATA_CENTER_GPU_FLEX_170" || nh.GPUCount != 4 {
		t.Fatalf("NodeHardware from labels %+v, want raw model Flex_170, the catalogue's INTEL_DATA_CENTER_GPU_FLEX_170 and 4 GPUs", nh)
	}
	if got := policy.NodeFamily("flex-0", toHardwareInfoMap(map[string]NodeHardware{"flex-0": nh})); got != "gpu:INTEL_DATA_CENTER_GPU_FLEX_170" {
		t.Fatalf("hardware family %q, want gpu:INTEL_DATA_CENTER_GPU_FLEX_170", got)
	}
	intent := computeGPUIntentForNodeWithHardware(node, "eco", 100, 60, true, map[string]GPUModelCaps{}, keys, nh, loadHardwareCatalog())
	if intent == nil || intent.CapPctOfMax == nil || *intent.CapPctOfMax != 60 || intent.CapWattsPerGPU == nil || *intent.CapWattsPerGPU != 90 {
		t.Fatalf("eco GPU intent %+v, want 60 %% and 90 W per GPU (60 %% of the catalogue's 150 W)", intent)
	}
}

// With the Intel device plugin's shared-dev-num 2, four Flex 170 advertise
// eight gpu.intel.com/i915 units; the GPU count of a node with no
// NodeHardware is the four GPUs Intel's gpu.intel.com/device.count label
// states, not the units, in the controller manager and in the agent.
func TestIntelGPUCountComesFromTheDeviceCountLabel(t *testing.T) {
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "flex-shared", Labels: map[string]string{
			"gpu.intel.com/product":      "Flex_170",
			"gpu.intel.com/device.count": "4",
		}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{"gpu.intel.com/i915": resource.MustParse("8")},
		},
	}
	if got := discoverGPUCount(node); got != 4 {
		t.Fatalf("discoverGPUCount = %d, want the 4 GPUs of gpu.intel.com/device.count, not the 8 shared units", got)
	}
	nh := nodeHardwareFromLabels(node, loadHardwareCatalog(), parseCSVList(chartGPUProductLabelKeys(t)))
	if nh.GPUCount != 4 {
		t.Fatalf("NodeHardware from labels counts %d GPUs, want 4", nh.GPUCount)
	}
}
