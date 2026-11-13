package main

// A capped GPU's throughput parameters had three defaults that drifted
// apart: SIM_GPU_MEMORY_GAMMA defaulted to 1.2, the model's own fallback for
// an unset memoryGamma was 1.1, and the node class generator wrote 1.10 for a
// product it has no row for. A GPU's throughput under a cap then depended on
// which of the three filled the value.

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/phys"
)

const generatorScript = "../../../scripts/generate_heterogeneous_assets.py"

var generatorDefaultsBlock = regexp.MustCompile(`(?s)\n_GPU_PHYSICS_DEFAULTS[^=]*=\s*\{(.*?)\}`)

// generatorDefault is the value of key in the generator's
// _GPU_PHYSICS_DEFAULTS.
func generatorDefault(t *testing.T, src, key string) float64 {
	t.Helper()
	block := generatorDefaultsBlock.FindStringSubmatch(src)
	if block == nil {
		t.Fatalf("%s has no _GPU_PHYSICS_DEFAULTS", generatorScript)
	}
	m := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `":\s*([0-9.]+)`).FindStringSubmatch(block[1])
	if m == nil {
		t.Fatalf("_GPU_PHYSICS_DEFAULTS has no %s", key)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("_GPU_PHYSICS_DEFAULTS %s: %v", key, err)
	}
	return v
}

// TestGPUThroughputDefaultsAgree checks, for each throughput parameter, that
// the simulator's environment default, the generator's default and the
// model's fallback for an unset value are one value.
func TestGPUThroughputDefaultsAgree(t *testing.T) {
	clearSimEnv(t)
	raw, err := os.ReadFile(generatorScript)
	if err != nil {
		t.Fatal(err)
	}
	env := defaultBaseModelFromEnv().GPU
	// Capped well below the natural draw, on a mixed workload, so that both
	// the compute and the memory scale count.
	state := phys.DeviceState{Utilization: 1, CapWatts: 150, MaxCapWatts: 400, MemoryIntensity: 0.5}
	base := phys.CappedBoardGPUModel{IdleW: 60, MaxW: 400, ComputeGamma: 0.5, MemoryEpsilon: 0.3, MemoryGamma: 0.7}
	cases := []struct {
		key string
		env float64
		set func(*phys.CappedBoardGPUModel, float64)
	}{
		{"computeGamma", env.ComputeGamma, func(m *phys.CappedBoardGPUModel, v float64) { m.ComputeGamma = v }},
		{"memoryEpsilon", env.MemoryEpsilon, func(m *phys.CappedBoardGPUModel, v float64) { m.MemoryEpsilon = v }},
		{"memoryGamma", env.MemoryGamma, func(m *phys.CappedBoardGPUModel, v float64) { m.MemoryGamma = v }},
	}
	throughput := func(set func(*phys.CappedBoardGPUModel, float64), v float64) float64 {
		m := base
		set(&m, v)
		return m.ThroughputMultiplier(state, "")
	}
	for _, c := range cases {
		if g := generatorDefault(t, string(raw), c.key); g != c.env {
			t.Errorf("%s: the generator's default is %g, the simulator's environment default %g", c.key, g, c.env)
		}
		if throughput(c.set, c.env) == throughput(c.set, c.env+0.1) {
			t.Fatalf("%s: the test state does not depend on it", c.key)
		}
		if unset, set := throughput(c.set, 0), throughput(c.set, c.env); unset != set {
			t.Errorf("%s: the model's fallback gives throughput %g, the environment default %g gives %g", c.key, unset, c.env, set)
		}
	}
}
