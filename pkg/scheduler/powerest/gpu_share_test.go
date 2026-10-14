package powerest

import (
	"math"
	"testing"
)

// gpuPod is a pod whose containers request the given resources, one map per
// container, as the extender receives it.
func gpuPod(containers ...map[string]interface{}) map[string]interface{} {
	cs := make([]interface{}, 0, len(containers))
	for _, req := range containers {
		cs = append(cs, map[string]interface{}{
			"resources": map[string]interface{}{"requests": req, "limits": req},
		})
	}
	return map[string]interface{}{"spec": map[string]interface{}{"containers": cs}}
}

// A pod's GPU demand counts every GPU resource pkg/api classifies, each unit
// weighted by the share of a GPU it stands for: a MIG device by its compute
// slices out of seven, a renamed replica as the device it shares, an AMD
// partition by its compute mode, an Intel xe GPU as one GPU.
func TestExtractPodDemandCountsEveryGPUResourceByItsShare(t *testing.T) {
	cases := []struct {
		name       string
		containers []map[string]interface{}
		want       float64
		vendor     string
	}{
		{"whole", []map[string]interface{}{{"nvidia.com/gpu": "2"}}, 2, "nvidia"},
		{"MIG 1g", []map[string]interface{}{{"nvidia.com/mig-1g.12gb": "1"}}, 1.0 / 7, "nvidia"},
		{"MIG 3g x2", []map[string]interface{}{{"nvidia.com/mig-3g.47gb": "2"}}, 6.0 / 7, "nvidia"},
		{"time-sliced replica", []map[string]interface{}{{"nvidia.com/gpu.shared": "1"}}, 1, "nvidia"},
		{"shared MIG device", []map[string]interface{}{{"nvidia.com/mig-1g.12gb.shared": "1"}}, 1.0 / 7, "nvidia"},
		{"AMD CPX partition", []map[string]interface{}{{"amd.com/cpx_nps4": "2"}}, 2.0 / 8, "amd"},
		{"AMD SPX", []map[string]interface{}{{"amd.com/spx_nps1": "1"}}, 1, "amd"},
		{"Intel xe", []map[string]interface{}{{"gpu.intel.com/xe": "1"}}, 1, "intel"},
		{"two containers", []map[string]interface{}{{"nvidia.com/gpu": "1"}, {"nvidia.com/mig-1g.12gb": "1"}}, 1 + 1.0/7, "nvidia"},
		{"monitoring only", []map[string]interface{}{{"gpu.intel.com/i915_monitoring": "1"}}, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := ExtractPodDemand(gpuPod(c.containers...), "standard")
			if got := float64(d.GPUCount); math.Abs(got-c.want) > 1e-9 || d.GPUVendor != c.vendor {
				t.Errorf("GPU demand %v %q, want %v %q", got, d.GPUVendor, c.want, c.vendor)
			}
		})
	}
}

// A MIG pod on a GPU node adds the watts of its slice of a GPU: one seventh
// of what a whole GPU adds.
func TestEstimateMarginalImpactOfAMIGPod(t *testing.T) {
	coeff := DefaultCoefficients()
	whole := EstimateMarginalImpact(ExtractPodDemand(gpuPod(map[string]interface{}{"nvidia.com/gpu": "1"}), "standard"), gpuNode(), coeff)
	mig := EstimateMarginalImpact(ExtractPodDemand(gpuPod(map[string]interface{}{"nvidia.com/mig-1g.12gb": "1"}), "standard"), gpuNode(), coeff)
	if whole.DeltaGPUWatts <= 0 || math.Abs(mig.DeltaGPUWatts-whole.DeltaGPUWatts/7) > 1e-9 {
		t.Errorf("GPU delta of a 1g MIG pod %.2f W, want a seventh of a whole GPU's %.2f W", mig.DeltaGPUWatts, whole.DeltaGPUWatts)
	}
}
