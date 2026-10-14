package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/matbun/joulie/pkg/controller/policy"
)

// chartGPUProductLabelKeys is GPU_PRODUCT_LABEL_KEYS as the chart sets it,
// the value a deployed controller manager reads.
func chartGPUProductLabelKeys(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "joulie", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s+GPU_PRODUCT_LABEL_KEYS:\s*"([^"]*)"`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("charts/joulie/values.yaml sets no GPU_PRODUCT_LABEL_KEYS")
	}
	return string(m[1])
}

// Regression for the AMD product label: the AMD node labeller names the board
// in amd.com/gpu.product-name (and beta.amd.com/gpu.product-name), while the
// chart's GPU_PRODUCT_LABEL_KEYS read amd.com/gpu.product, so a node with no
// NodeHardware fell through to amd.com/gpu.family and its hardware family
// was the family code (gpu:GC_11_0_0, gpu:AI), which names no board and puts
// different boards of one code in one family.
func TestGPULabelRegressionChartKeysAMDFamilyByProductName(t *testing.T) {
	keys := parseCSVList(chartGPUProductLabelKeys(t))
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"w7900", map[string]string{
			"amd.com/gpu.family":       "GC_11_0_0",
			"amd.com/gpu.product-name": "AMD_Radeon_PRO_W7900",
		}, "gpu:AMD_RADEON_PRO_W7900"},
		{"mi300x", map[string]string{
			"amd.com/gpu.family":            "AI",
			"amd.com/gpu.product-name":      "AMD_Instinct_MI300X_OAM",
			"beta.amd.com/gpu.family":       "AI",
			"beta.amd.com/gpu.product-name": "AMD_Instinct_MI300X_OAM",
		}, "gpu:AMD_INSTINCT_MI300X"},
		{"beta prefix only", map[string]string{
			"beta.amd.com/gpu.family":       "AI",
			"amd.com/gpu.family":            "AI",
			"beta.amd.com/gpu.product-name": "AMD_Instinct_MI300X_OAM",
		}, "gpu:AMD_INSTINCT_MI300X"},
		{"older amd.com/gpu.product", map[string]string{
			"amd.com/gpu.family":  "AI",
			"amd.com/gpu.product": "AMD_Instinct_MI300X",
		}, "gpu:AMD_INSTINCT_MI300X"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "n0", Labels: tc.labels},
				Status: corev1.NodeStatus{
					Allocatable: corev1.ResourceList{"amd.com/gpu": resource.MustParse("4")},
				},
			}
			nh := nodeHardwareFromLabels(node, loadHardwareCatalog(), keys)
			got := policy.NodeFamily("n0", toHardwareInfoMap(map[string]NodeHardware{"n0": nh}))
			if got != tc.want {
				t.Fatalf("hardware family %q (raw model %q) with the chart's GPU_PRODUCT_LABEL_KEYS %q, want %q",
					got, nh.GPURawModel, keys, tc.want)
			}
		})
	}
}
