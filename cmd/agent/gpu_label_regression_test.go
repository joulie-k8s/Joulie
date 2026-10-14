package main

import (
	"context"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
)

// Regression for D8: the AMD node labeller names the board in
// amd.com/gpu.product-name (and beta.amd.com/gpu.product-name), while the
// agent read amd.com/gpu.product, so with no working GPU inventory an AMD
// node took its raw model from amd.com/gpu.family (GC_11_0_0, AI), a code
// that names no board. The emulated W7900 and MI300X nodes carry the
// labeller's label set and fail the inventory call (D1), so the raw model
// comes from the labels alone.
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
