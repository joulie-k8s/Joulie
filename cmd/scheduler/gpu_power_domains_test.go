package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/scheduler/powerest"
)

// A pod requests counted GPUs. On four MI250X the agent counts eight and
// publishes powerDomains 4 with the package's 560 W maximum (its golden
// cmd/agent/testdata/hwemu/amd-instinct-mi250x-4oam.json), so a counted GPU
// stands for 280 W, and a pod of two amd.com/gpu, one package, adds
// 2 x 280 W times the utilization coefficient, not twice that. Without
// powerDomains, as from an older agent, a counted GPU is the whole maximum.
func TestGPUPowerDomainsSetThePodGPUEstimate(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "hwemu", "amd-instinct-mi250x-4oam.json"))
	if err != nil {
		t.Fatal(err)
	}
	coeff := powerest.DefaultCoefficients()
	for _, tc := range []struct {
		name     string
		drop     bool
		wantPerW float64
	}{
		{"powerDomains published", false, 280},
		{"no powerDomains, the count", true, 560},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var status map[string]any
			if err := json.Unmarshal(b, &status); err != nil {
				t.Fatal(err)
			}
			gpu, _ := status["gpu"].(map[string]any)
			if gpu["count"] != float64(8) || gpu["powerDomains"] != float64(4) {
				t.Fatalf("the agent's MI250X status has gpu %v; want count 8 and powerDomains 4", gpu)
			}
			if tc.drop {
				delete(gpu, "powerDomains")
			}
			raw, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			obj := &v1alpha1.NodeHardware{Spec: v1alpha1.NodeHardwareSpec{NodeName: "mi250x"}}
			if err := json.Unmarshal(raw, &obj.Status); err != nil {
				t.Fatal(err)
			}
			name, info := hwInfoFromObject(obj)
			if info.GPUMaxWattsPerGPU != tc.wantPerW {
				t.Fatalf("GPU maximum per counted GPU %v W, want %v W", info.GPUMaxWattsPerGPU, tc.wantPerW)
			}
			profile := hwInfoToProfile(name, info)
			est := powerest.EstimateMarginalImpact(powerest.PodDemand{CPUCores: 1, GPUCount: 2, GPUVendor: "amd"}, &profile, coeff)
			if want := 2 * tc.wantPerW * coeff.GPUUtilCoeffStandard; math.Abs(est.DeltaGPUWatts-want) > 1e-9 {
				t.Fatalf("a pod of two amd.com/gpu adds %v W of GPU power, want %v W", est.DeltaGPUWatts, want)
			}
		})
	}
}
