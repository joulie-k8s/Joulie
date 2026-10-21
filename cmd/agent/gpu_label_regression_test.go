package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
)

// Regression for D8: the AMD node labeller names the board in
// amd.com/gpu.product-name (and beta.amd.com/gpu.product-name), while the
// agent read amd.com/gpu.product, so with no working GPU inventory an AMD
// node took its raw model from amd.com/gpu.family (GC_11_0_0, AI), a code
// that names no board. The emulated W7900 and MI300X nodes carry the
// labeller's label set, and amdgpu sysfs, which the inventory reads, names
// no product, so the raw model comes from the labels alone.
func TestGPULabelRegressionAMDModelFromLabellerProductName(t *testing.T) {
	for _, tc := range []struct {
		profile   string
		dropLabel string // a label removed to read the other prefix
		wantRaw   string
		wantModel string // catalog key, empty when the catalog has none
	}{
		{profile: "amd-radeon-pro-w7900-4gpu", wantRaw: "AMD_Radeon_PRO_W7900", wantModel: "AMD_RADEON_PRO_W7900"},
		{profile: "amd-instinct-mi300x-8gpu", wantRaw: "AMD_Instinct_MI300X_OAM", wantModel: "AMD_INSTINCT_MI300X"},
		{profile: "amd-instinct-mi300x-8gpu", dropLabel: "amd.com/gpu.product-name", wantRaw: "AMD_Instinct_MI300X_OAM", wantModel: "AMD_INSTINCT_MI300X"},
	} {
		name := tc.profile
		if tc.dropLabel != "" {
			name += " without " + tc.dropLabel
		}
		t.Run(name, func(t *testing.T) {
			_, tree, p := hwemutest.NewNode(t, tc.profile, nil)
			e := useEmulatedNode(t, tree, nil)
			node := e.nodeObject(p)
			if node.Labels["amd.com/gpu.family"] == "" {
				t.Fatalf("the node has no amd.com/gpu.family label, so this test proves nothing about it: %v", node.Labels)
			}
			if tc.dropLabel != "" {
				if node.Labels[tc.dropLabel] == "" {
					t.Fatalf("the node has no %s label to drop", tc.dropLabel)
				}
				delete(node.Labels, tc.dropLabel)
			}
			hw := discoverHardware(context.Background(), node)
			if hw.GPURawModel != tc.wantRaw {
				t.Fatalf("GPU raw model %q (gpu.family %q), want the labeller's product name %q", hw.GPURawModel, node.Labels["amd.com/gpu.family"], tc.wantRaw)
			}
			if hw.GPUModel != tc.wantModel {
				t.Errorf("GPU model %q, want catalog key %q", hw.GPUModel, tc.wantModel)
			}
		})
	}
}

// Regression for D8: hasNFDGPUVendor read only the display classes 0300 and
// 0302, while an AMD Instinct MI300X enumerates as a processing accelerator
// (class 0x120000) and NFD labels it pci-1200_1002, so on a node without
// the GPU tools and amdgpu sysfs the agent found no GPU vendor. The node is
// a CPU-only emulated node, whose BMC VGA gives the one label
// pci-0300_102b, so the labels a case adds are the only evidence of a GPU:
// the vendor must come from them, with no probe run. Every class a GPU
// driver binds counts for every vendor (nfdGPUClasses).
func TestD8GPUVendorFromEveryNFDGPUClass(t *testing.T) {
	const nfd = "feature.node.kubernetes.io/"
	for _, tc := range []struct {
		name        string
		labels      map[string]string
		wantVendor  string // detectGPUVendor, which knows nvidia and amd only
		wantVendors []string
	}{
		{name: "MI300X processing accelerator", labels: map[string]string{
			nfd + "pci-1200_1002.present": "true", amdLabellerProduct: "AMD_Instinct_MI300X_OAM",
		}, wantVendor: "amd", wantVendors: []string{"amd"}},
		{name: "AMD other display controller", labels: map[string]string{nfd + "pci-0380_1002.present": "true"}, wantVendor: "amd", wantVendors: []string{"amd"}},
		{name: "NVIDIA processing accelerator", labels: map[string]string{nfd + "pci-1200_10de.present": "true"}, wantVendor: "nvidia", wantVendors: []string{"nvidia"}},
		{name: "Intel processing accelerator", labels: map[string]string{nfd + "pci-1200_8086.present": "true"}, wantVendor: "none", wantVendors: []string{"intel"}},
		{name: "Intel other display controller", labels: map[string]string{nfd + "pci-0380_8086.present": "true"}, wantVendor: "none", wantVendors: []string{"intel"}},
		{name: "a BMC VGA only", wantVendor: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, tree, p := hwemutest.NewNode(t, "intel-xeon-2s-rapl", nil)
			e := useEmulatedNode(t, tree, nil)
			node := e.nodeObject(p)
			for k := range node.Labels {
				if strings.HasPrefix(k, nfd+"pci-") && k != nfd+"pci-0300_102b.present" {
					t.Fatalf("the CPU-only node carries %s, so the case does not decide the vendor alone: %v", k, node.Labels)
				}
			}
			for k, v := range tc.labels {
				node.Labels[k] = v
			}
			hw := discoverHardware(context.Background(), node)
			if hw.GPUVendor != tc.wantVendor || !slices.Equal(hw.GPUVendors, tc.wantVendors) {
				t.Errorf("gpu vendor %q, vendors %v; want %q and %v from the labels %v (D8)", hw.GPUVendor, hw.GPUVendors, tc.wantVendor, tc.wantVendors, tc.labels)
			}
			if tc.wantVendor != "none" {
				for _, probe := range []string{nvidiaListProbe, rocmProductProbe} {
					if ran, _ := e.runner.ran(probe); ran {
						t.Errorf("%s ran, so the vendor did not come from the labels %v (D8); commands: %v", probe, tc.labels, e.runner.commands())
					}
				}
			}
		})
	}
}
