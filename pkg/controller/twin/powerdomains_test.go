package twin

import (
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

// The GPU TDP counts the per-GPU maximum once per GPU that holds a power
// limit of its own: four MI250X are eight counted GPUs and four power
// domains of 560 W, 2240 W. Without PowerDomains, or with one that is not
// below the count, every counted GPU holds its own.
func TestGPUTDPCountsPowerDomains(t *testing.T) {
	for _, tc := range []struct {
		count, domains int
		want           float64
	}{
		{8, 4, 2240},
		{8, 0, 4480},
		{8, 8, 4480},
		{8, 9, 4480},
	} {
		hw := joulie.NodeHardware{GPU: joulie.NodeHardwareGPU{Present: true, Count: tc.count, PowerDomains: tc.domains,
			CapRange: joulie.GPUCapRange{MaxWatts: 560}}}
		pm := Compute(Input{Hardware: hw, GPUCapPct: 60}).PowerMeasurement
		if pm.GpuTdpW != tc.want || pm.GpuCappedPowerW != 0.6*tc.want {
			t.Errorf("count %d, powerDomains %d: gpuTdpW %v, gpuCappedPowerW %v; want %v and %v",
				tc.count, tc.domains, pm.GpuTdpW, pm.GpuCappedPowerW, tc.want, 0.6*tc.want)
		}
	}
}
