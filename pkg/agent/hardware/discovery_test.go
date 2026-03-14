package hardware

import (
	"testing"
)

func TestClassifyLinkSpeed(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"400000Mb/s", "400G"},
		{"100000Mb/s", "100G"},
		{"25000Mb/s", "25G"},
		{"10000Mb/s", "10G"},
		{"1000Mb/s", "unknown"},
	}
	for _, tc := range tests {
		got := classifyLinkSpeed(tc.input)
		if got != tc.want {
			t.Errorf("classifyLinkSpeed(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestSanitizeGPUModel(t *testing.T) {
	if got := sanitizeGPUModel("NVIDIA H100 NVL"); got != "NVIDIA_H100_NVL" {
		t.Errorf("got %q", got)
	}
	if got := sanitizeGPUModel("nvidia a100-sxm4"); got != "NVIDIA_A100_SXM4" {
		t.Errorf("got %q", got)
	}
}

func TestDiscoverSimulated(t *testing.T) {
	info, err := Discover(DiscoverOptions{
		NodeName:     "test-node",
		SimulateMode: true,
	})
	if err != nil {
		t.Fatalf("simulated discovery failed: %v", err)
	}
	if info.NodeName != "test-node" {
		t.Errorf("expected NodeName=test-node, got %s", info.NodeName)
	}
	if info.CPU.Vendor != "simulator" {
		t.Errorf("expected simulator vendor, got %s", info.CPU.Vendor)
	}
	if !info.GPU.Present {
		t.Error("expected GPU present in simulated mode")
	}
	if info.Memory.TotalBytes == 0 {
		t.Error("expected non-zero memory")
	}
}
