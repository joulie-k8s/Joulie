package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression tests for the trace workloadgen writes without -schema-version.
// They use only identifiers that exist at a94f345, so they also build against
// the generator from before trace schema v3, where both fail.

// regressionConfigs are both modes as a config with no schema version, which
// is what workloadgen run without -schema-version builds.
func regressionConfigs() map[string]generatorConfig {
	def := defaultTestConfig()
	def.TimeScale = 1
	diurnal := def
	diurnal.Mode = "diurnal"
	diurnal.SimHours = 2
	diurnal.DiurnalPeakRate = 4
	diurnal.DiurnalBurstMin = 2
	diurnal.DiurnalBurstMax = 5
	return map[string]generatorConfig{"default": def, "diurnal": diurnal}
}

// regressionJobs writes cfg's trace in its mode and returns its job records.
func regressionJobs(t *testing.T, cfg generatorConfig) []map[string]any {
	t.Helper()
	out := filepath.Join(t.TempDir(), "trace.jsonl")
	setOutputPath(out)
	gen := generateTrace
	if cfg.Mode == "diurnal" {
		gen = generateDiurnalTrace
	}
	if err := gen(cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var jobs []map[string]any
	for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if rec["type"] == "job" {
			jobs = append(jobs, rec)
		}
	}
	if len(jobs) == 0 {
		t.Fatal("no job records")
	}
	return jobs
}

// The simulator reads a missing sensitivity as 1: a cap slows the job in
// full. Before v3 every default-mode job carried {"cpu":0,"gpu":0}, so a CPU
// cap never slowed it (cpuThrottleImpactFactor in
// simulator/cmd/simulator/main.go), and diurnal standard jobs carried the
// same.
func TestDefaultTraceLeavesSensitivityToTheSimulator(t *testing.T) {
	for mode, cfg := range regressionConfigs() {
		for _, j := range regressionJobs(t, cfg) {
			if s, ok := j["sensitivity"]; ok {
				t.Fatalf("%s mode: job %v has sensitivity %v; want the key absent", mode, j["jobId"], s)
			}
		}
	}
}

// The physics model knows gpu.compute_bound (gpuBoundnessWeights in
// simulator/pkg/phys/model.go). Before v3 diurnal mode wrote "gpu.compute",
// which it treats as gpu.mixed.
func TestDefaultDiurnalTraceWritesKnownGPUClasses(t *testing.T) {
	known := map[string]bool{"gpu.compute_bound": true, "gpu.mixed": true, "gpu.memory_bound": true}
	seen := map[string]bool{}
	for _, j := range regressionJobs(t, regressionConfigs()["diurnal"]) {
		wc, _ := j["workloadClass"].(map[string]any)
		if cls, ok := wc["gpu"].(string); ok {
			if !known[cls] {
				t.Fatalf("job %v: GPU class %q", j["jobId"], cls)
			}
			seen[cls] = true
		}
	}
	if !seen["gpu.compute_bound"] {
		t.Fatal("no gpu.compute_bound job")
	}
}
