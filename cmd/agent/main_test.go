package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestNormalizeCPUVendor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
	}{
		{"AMD", "AuthenticAMD"},
		{"AuthenticAMD", "AuthenticAMD"},
		{"intel", "GenuineIntel"},
		{"GenuineIntel", "GenuineIntel"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeCPUVendor(tt.in); got != tt.want {
			t.Fatalf("normalizeCPUVendor(%q) got=%q want=%q", tt.in, got, tt.want)
		}
	}
}

func TestDiscoverCPUVendorPrefersNFDVendorLabel(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"feature.node.kubernetes.io/cpu-vendor":            "AMD",
		"feature.node.kubernetes.io/cpu-model.vendor_id":   "GenuineIntel",
		"feature.node.kubernetes.io/pci-0300_10de.present": "true",
	}
	if got := discoverCPUVendor(labels); got != "AuthenticAMD" {
		t.Fatalf("discoverCPUVendor got=%q", got)
	}
}

func TestDiscoverHardwareGPUVendors(t *testing.T) {
	t.Parallel()
	labels := map[string]string{
		"feature.node.kubernetes.io/cpu-model.vendor_id":   "GenuineIntel",
		"feature.node.kubernetes.io/pci-0300_10de.present": "true",
		"feature.node.kubernetes.io/pci-0302_1002.present": "true",
	}
	hw := discoverHardware(labels)
	if hw.CPUVendor != "GenuineIntel" {
		t.Fatalf("cpu vendor got=%q", hw.CPUVendor)
	}
	if len(hw.GPUVendors) != 2 {
		t.Fatalf("gpu vendors len got=%d want=2", len(hw.GPUVendors))
	}
}

func TestCPUIndexFromPath(t *testing.T) {
	t.Parallel()
	if got, ok := cpuIndexFromPath("/host-sys/devices/system/cpu/cpufreq/policy11"); !ok || got != 11 {
		t.Fatalf("policy path parse failed: got=%d ok=%v", got, ok)
	}
	if got, ok := cpuIndexFromPath("/host-sys/devices/system/cpu/cpu7/cpufreq"); !ok || got != 7 {
		t.Fatalf("cpu path parse failed: got=%d ok=%v", got, ok)
	}
	if _, ok := cpuIndexFromPath("/not/a/cpu/path"); ok {
		t.Fatalf("invalid path should not parse")
	}
}

func TestParseNodePowerProfileWithIntAndFloatCaps(t *testing.T) {
	t.Parallel()
	intObj := unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "joulie.io/v1alpha1",
			"kind":       "NodePowerProfile",
			"metadata": map[string]any{
				"name": "node-a",
			},
			"spec": map[string]any{
				"nodeName": "node-a",
				"profile":  "eco",
				"cpu": map[string]any{
					"packagePowerCapWatts": int64(120),
				},
			},
		},
	}
	intObj.SetGroupVersionKind(metav1.SchemeGroupVersion.WithKind("NodePowerProfile"))
	npInt := parseNodePowerProfile(intObj)
	if npInt.PowerWatts == nil || *npInt.PowerWatts != 120 {
		t.Fatalf("int cap parse failed: %#v", npInt.PowerWatts)
	}

	floatObj := unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "joulie.io/v1alpha1",
			"kind":       "NodePowerProfile",
			"metadata": map[string]any{
				"name": "node-b",
			},
			"spec": map[string]any{
				"nodeName": "node-b",
				"profile":  "performance",
				"cpu": map[string]any{
					"packagePowerCapWatts": 5000.0,
				},
			},
		},
	}
	npFloat := parseNodePowerProfile(floatObj)
	if npFloat.PowerWatts == nil || *npFloat.PowerWatts != 5000 {
		t.Fatalf("float cap parse failed: %#v", npFloat.PowerWatts)
	}
}
