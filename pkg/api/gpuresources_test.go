package api

import (
	"math"
	"testing"
)

// Every resource name a device plugin advertises for a GPU, whole, as a
// partition or as a renamed replica, is a GPU resource of its kind; the
// look-alikes are not. The NVIDIA names are the closed set of the device
// plugin (README.md, "Shared Access to GPUs" and the MIG strategies); the
// AMD partition names are the plugin's mixed naming strategy
// (ROCm/k8s-device-plugin, getResourceList and UniquePartitionConfigCount);
// the Intel names are the plugin's device types (deviceTypeI915,
// deviceTypeXe) beside its monitoring and GAS resources.
func TestGPUResourceKind(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"nvidia.com/gpu":                GPUResourceWhole,
		"amd.com/gpu":                   GPUResourceWhole,
		"gpu.intel.com/i915":            GPUResourceWhole,
		"gpu.intel.com/xe":              GPUResourceWhole,
		"vendor.example/gpu":            GPUResourceWhole,
		"NVIDIA.com/GPU":                GPUResourceWhole,
		"nvidia.com/gpu.shared":         GPUResourceShared,
		"nvidia.com/mig-1g.12gb":        GPUResourcePartition,
		"nvidia.com/mig-1c.3g.40gb":     GPUResourcePartition,
		"nvidia.com/mig-3g.40gb.shared": GPUResourceShared,
		"amd.com/cpx_nps4":              GPUResourcePartition,
		"amd.com/spx_nps1":              GPUResourcePartition,
		"amd.com/dpx_nps2":              GPUResourcePartition,
		"amd.com/tpx_nps1":              GPUResourcePartition,
		"amd.com/qpx_nps4":              GPUResourcePartition,
		"amd.com/cpx_nps1":              GPUResourcePartition,
		"amd.com/xpx_nps1":              "",
		"amd.com/cpx_nps":               "",
		"amd.com/cpx":                   "",
		"amd.com/cpx_nps4x":             "",
		"gpu.intel.com/i915_monitoring": "",
		"gpu.intel.com/xe_monitoring":   "",
		"gpu.intel.com/monitoring":      "",
		"gpu.intel.com/millicores":      "",
		"gpu.intel.com/memory.max":      "",
		"gpu.intel.com/tiles":           "",
		"nvidia.com/mig-":               "",
		"example.com/gpus":              "",
		"nvidia.com/gpu-foo":            "",
		"/gpu":                          "",
		"gpu":                           "",
		"cpu":                           "",
		"memory":                        "",
		"":                              "",
	}
	for name, want := range cases {
		if got := GPUResourceKind(name); got != want {
			t.Errorf("GPUResourceKind(%q) = %q, want %q", name, got, want)
		}
	}
}

// One unit of each GPU resource is the documented fraction of a physical
// GPU; anything else is none.
func TestGPUResourceShare(t *testing.T) {
	t.Parallel()
	cases := map[string]float64{
		"nvidia.com/gpu":                1,
		"amd.com/gpu":                   1,
		"gpu.intel.com/xe":              1,
		"nvidia.com/gpu.shared":         1,
		"nvidia.com/mig-1g.12gb":        1.0 / 7,
		"nvidia.com/mig-3g.47gb":        3.0 / 7,
		"nvidia.com/mig-7g.94gb":        1,
		"nvidia.com/mig-1c.3g.40gb":     1.0 / 7,
		"nvidia.com/mig-2c.4g.40gb":     2.0 / 7,
		"nvidia.com/mig-1g.10gb+me":     1.0 / 7,
		"nvidia.com/mig-3g.40gb.shared": 3.0 / 7,
		"nvidia.com/mig-9g.200gb":       1,
		"nvidia.com/mig-custom":         1.0 / 7,
		"amd.com/spx_nps1":              1,
		"amd.com/dpx_nps2":              1.0 / 2,
		"amd.com/tpx_nps1":              1.0 / 3,
		"amd.com/qpx_nps4":              1.0 / 4,
		"amd.com/cpx_nps4":              1.0 / 8,
		"gpu.intel.com/millicores":      0,
		"cpu":                           0,
	}
	for name, want := range cases {
		if got := GPUResourceShare(name); math.Abs(got-want) > 1e-12 {
			t.Errorf("GPUResourceShare(%q) = %v, want %v", name, got, want)
		}
	}
}

// The physical count comes from Joulie's label, then GPU feature
// discovery's count, which counts MIG devices when the single strategy
// exposes them (product suffixed -MIG-) and is 0 on an invalid MIG
// configuration; the strategy label alone, which the GPU Operator sets on
// every GPU node by default, does not change it; replicas are never a count.
func TestGPUCountFromLabels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		labels map[string]string
		want   int
		ok     bool
	}{
		{"no labels", nil, 0, false},
		{"mig none", map[string]string{"nvidia.com/gpu.count": "8", "nvidia.com/mig.strategy": "none"}, 8, true},
		{"mig mixed", map[string]string{"nvidia.com/gpu.count": "8", "nvidia.com/mig.strategy": "mixed", "nvidia.com/gpu.replicas": "0"}, 8, true},
		{"time-slicing", map[string]string{"nvidia.com/gpu.count": "8", "nvidia.com/gpu.replicas": "4"}, 8, true},
		{"mig single counts MIG devices", map[string]string{"nvidia.com/gpu.count": "56", "nvidia.com/mig.strategy": "single", "nvidia.com/gpu.product": "NVIDIA-H100-NVL-MIG-1g.12gb"}, 0, false},
		{"mig single, Joulie label", map[string]string{"nvidia.com/gpu.count": "56", "nvidia.com/mig.strategy": "single", "nvidia.com/gpu.product": "NVIDIA-H100-NVL-MIG-1g.12gb", "joulie.io/hw.gpu-count": "8"}, 8, true},
		{"mig single without MIG devices", map[string]string{"nvidia.com/gpu.count": "8", "nvidia.com/mig.strategy": "single", "nvidia.com/gpu.product": "NVIDIA-H100-NVL"}, 8, true},
		{"mig single without MIG devices, time-slicing", map[string]string{"nvidia.com/gpu.count": "8", "nvidia.com/mig.strategy": "single", "nvidia.com/gpu.replicas": "4", "nvidia.com/gpu.product": "NVIDIA-H100-NVL-SHARED"}, 8, true},
		{"Joulie label first", map[string]string{"nvidia.com/gpu.count": "4", "joulie.io/hw.gpu-count": " 8 "}, 8, true},
		{"mig invalid", map[string]string{"nvidia.com/gpu.count": "0", "nvidia.com/gpu.product": "NVIDIA-H100-NVL-MIG-INVALID"}, 0, false},
		{"not a number", map[string]string{"nvidia.com/gpu.count": "eight"}, 0, false},
		{"replicas only", map[string]string{"nvidia.com/gpu.replicas": "4"}, 0, false},
		// Intel's NFD rules count the data center GPUs, whatever units the
		// plugin advertises with shared-dev-num.
		{"intel device count", map[string]string{"gpu.intel.com/device.count": "4", "gpu.intel.com/product": "Flex_170"}, 4, true},
		{"intel, Joulie label first", map[string]string{"gpu.intel.com/device.count": "4", "joulie.io/hw.gpu-count": "2"}, 2, true},
		{"intel device count zero", map[string]string{"gpu.intel.com/device.count": "0"}, 0, false},
	}
	for _, c := range cases {
		got, ok := GPUCountFromLabels(c.labels)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: GPUCountFromLabels = %d, %v, want %d, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// GPU feature discovery's product suffixes come off, in the order it
// appends them (internal/lm/resource.go getProductName, mig-strategy.go
// newInvalidMigStrategyLabeler); a plain product is unchanged.
func TestGPUProductBase(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"NVIDIA-H100-NVL":                  "NVIDIA-H100-NVL",
		"NVIDIA-H100-NVL-MIG-1g.12gb":      "NVIDIA-H100-NVL",
		"NVIDIA-H100-NVL-SHARED":           "NVIDIA-H100-NVL",
		"NVIDIA-H100-NVL-MIG-INVALID":      "NVIDIA-H100-NVL",
		"A100-SXM4-40GB-MIG-1g.5gb-SHARED": "A100-SXM4-40GB",
		" NVIDIA-L40S ":                    "NVIDIA-L40S",
		"AMD Instinct MI300X":              "AMD Instinct MI300X",
		"":                                 "",
	}
	for in, want := range cases {
		if got := GPUProductBase(in); got != want {
			t.Errorf("GPUProductBase(%q) = %q, want %q", in, got, want)
		}
	}
}
