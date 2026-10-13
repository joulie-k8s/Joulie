package main

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

// governorEnvVars is every variable loadGovernorConfig reads.
var governorEnvVars = []string{
	"GOVERNOR_CONTROL",
	"CPU_GOVERNOR_PERFORMANCE_PRESET", "CPU_GOVERNOR_ECO_PRESET", "CPU_GOVERNOR_ECO_BOOST", "CPU_GOVERNOR_ON_GPU_NODES",
	"GPU_GOVERNOR_PERFORMANCE_PRESET", "GPU_GOVERNOR_ECO_PRESET", "GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX",
	"GPU_GOVERNOR_MODEL_PRESETS_JSON",
}

// setGovernorEnv unsets every governor variable, then sets env, for the
// length of the test. An empty GPU preset is a value, so unset is not "".
func setGovernorEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, k := range governorEnvVars {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// useGovernorEnv loads the governor configuration from env and installs it
// for the length of the test, with a fresh set of once-only warnings.
func useGovernorEnv(t *testing.T, env map[string]string) governorConfig {
	t.Helper()
	setGovernorEnv(t, env)
	resetGovernorWarnings()
	cfg := loadGovernorConfig()
	cfg.install()
	t.Cleanup(func() {
		defaultGovernorConfig().install()
		resetGovernorWarnings()
	})
	return cfg
}

func resetGovernorWarnings() {
	governorWarningMu.Lock()
	governorWarningSeen = map[string]struct{}{}
	governorWarningMu.Unlock()
}

// allPresetsHardware is a node that accepts every GPU preset and every CPU preset,
// with a boost control.
func allPresetsHardware() NodeHardware {
	return NodeHardware{
		CPUGovernorPresets: append([]string(nil), joulie.CPUGovernorPresetValues...),
		GPUGovernorPresets: append([]string(nil), joulie.GPUGovernorPresetValues...),
		CPUBoostAvailable:  true,
	}
}

func TestGovernorConfigDefaults(t *testing.T) {
	buf := captureLog(t)
	cfg := useGovernorEnv(t, nil)
	if !reflect.DeepEqual(cfg, defaultGovernorConfig()) {
		t.Fatalf("config with no variable set = %+v, want %+v", cfg, defaultGovernorConfig())
	}
	if cfg.control != governorControlOff || cfg.cpuEnabled() || cfg.gpuEnabled() {
		t.Fatalf("control = %q, want off", cfg.control)
	}
	if cfg.cpuPerformancePreset != "performance" || cfg.cpuEcoPreset != "efficiency" ||
		cfg.gpuPerformancePreset != "" || cfg.gpuEcoPreset != "efficient" || cfg.gpuCeilingPctOfMax != 80 ||
		cfg.cpuEcoBoost != "" || cfg.cpuOnGPUNodes {
		t.Fatalf("defaults = %+v, want the documented ones", cfg)
	}
	if strings.Contains(buf.String(), "warning") {
		t.Fatalf("defaults logged a warning:\n%s", buf)
	}
}

// GC11: a value the controller manager cannot use logs a warning naming the
// variable and keeps the default; an invalid GOVERNOR_CONTROL means off.
func TestGC11InvalidGovernorEnvWarnsAndUsesTheDefault(t *testing.T) {
	for _, tc := range []struct {
		invalid string // the variable the warning names
		env     map[string]string
		want    func(governorConfig) bool
	}{
		{"GOVERNOR_CONTROL", map[string]string{"GOVERNOR_CONTROL": "on"},
			func(c governorConfig) bool { return c.control == governorControlOff }},
		// A native governor word is not a preset.
		{"CPU_GOVERNOR_PERFORMANCE_PRESET", map[string]string{"GOVERNOR_CONTROL": "all", "CPU_GOVERNOR_PERFORMANCE_PRESET": "schedutil"},
			func(c governorConfig) bool {
				return c.control == governorControlAll && c.cpuPerformancePreset == "performance"
			}},
		{"CPU_GOVERNOR_ECO_PRESET", map[string]string{"CPU_GOVERNOR_ECO_PRESET": "powersave"},
			func(c governorConfig) bool { return c.cpuEcoPreset == "efficiency" }},
		{"CPU_GOVERNOR_ECO_BOOST", map[string]string{"CPU_GOVERNOR_ECO_BOOST": "maybe"},
			func(c governorConfig) bool { return c.cpuEcoBoost == "" }},
		{"CPU_GOVERNOR_ON_GPU_NODES", map[string]string{"CPU_GOVERNOR_ON_GPU_NODES": "ture"},
			func(c governorConfig) bool { return !c.cpuOnGPUNodes }},
		{"GPU_GOVERNOR_PERFORMANCE_PRESET", map[string]string{"GPU_GOVERNOR_PERFORMANCE_PRESET": "high"},
			func(c governorConfig) bool { return c.gpuPerformancePreset == "" }},
		{"GPU_GOVERNOR_ECO_PRESET", map[string]string{"GPU_GOVERNOR_ECO_PRESET": "profile_peak"},
			func(c governorConfig) bool { return c.gpuEcoPreset == "efficient" }},
		{"GPU_GOVERNOR_MODEL_PRESETS_JSON", map[string]string{"GPU_GOVERNOR_MODEL_PRESETS_JSON": `{"M":{"performance":"manual","eco":"low"}}`},
			func(c governorConfig) bool {
				m := c.gpuModelPresets["M"]
				return m.Performance == nil && m.Eco != nil && *m.Eco == "low"
			}},
		{"GPU_GOVERNOR_MODEL_PRESETS_JSON", map[string]string{"GPU_GOVERNOR_MODEL_PRESETS_JSON": `{"M":`},
			func(c governorConfig) bool { return len(c.gpuModelPresets) == 0 }},
	} {
		buf := captureLog(t)
		cfg := useGovernorEnv(t, tc.env)
		if !tc.want(cfg) {
			t.Errorf("env %v gave %+v, want the default for the invalid value", tc.env, cfg)
		}
		if !strings.Contains(buf.String(), "warning: ") || !strings.Contains(buf.String(), tc.invalid) {
			t.Errorf("env %v logged no warning naming %s:\n%s", tc.env, tc.invalid, buf)
		}
	}

	// Case and spaces do not make a valid name invalid, and "" is a valid
	// GPU preset: write none.
	cfg := useGovernorEnv(t, map[string]string{
		"GOVERNOR_CONTROL": "ALL", "CPU_GOVERNOR_ECO_PRESET": " Balanced ", "GPU_GOVERNOR_ECO_PRESET": "",
	})
	if cfg.control != governorControlAll || cfg.cpuEcoPreset != "balanced" || cfg.gpuEcoPreset != "" {
		t.Fatalf("valid spellings gave %+v", cfg)
	}
}

// GC06: GPU_GOVERNOR_MODEL_PRESETS_JSON overrides the environment defaults
// for the models it names, keyed like GPU_MODEL_CAPS_JSON (the product label)
// and also matched on the model the agent reports; other models take the
// defaults. A field the entry leaves out keeps the default.
func TestGC06ModelPresetsOverrideTheDefaultsPerModel(t *testing.T) {
	cfg := useGovernorEnv(t, map[string]string{
		"GOVERNOR_CONTROL": "gpu",
		"GPU_GOVERNOR_MODEL_PRESETS_JSON": `{
			"MODEL-A": {"performance": "sustained", "ecoCeilingPctOfMax": 60},
			"MODEL-B": {"eco": ""},
			"Raw Model C": {"performance": "peak"}
		}`,
	})
	nh := allPresetsHardware()
	for _, tc := range []struct {
		name, profile, product, raw string
		want                        *joulie.GPUGovernorIntent
	}{
		{"model A performance", profilePerformance, "MODEL-A", "", &joulie.GPUGovernorIntent{Preset: "sustained"}},
		{"model A eco keeps the eco default, with its own ceiling", profileEco, "MODEL-A", "",
			&joulie.GPUGovernorIntent{Preset: "efficient", ComputeClockCeilingPctOfMax: floatPtr(60)}},
		{"model B eco is none", profileEco, "MODEL-B", "", nil},
		{"model B performance keeps the default (none)", profilePerformance, "MODEL-B", "", nil},
		{"model matched on the raw model", profilePerformance, "", "Raw Model C", &joulie.GPUGovernorIntent{Preset: "peak"}},
		{"the label wins over the raw model", profilePerformance, "MODEL-A", "Raw Model C", &joulie.GPUGovernorIntent{Preset: "sustained"}},
		{"unknown model, performance", profilePerformance, "MODEL-Z", "", nil},
		{"unknown model, eco", profileEco, "MODEL-Z", "",
			&joulie.GPUGovernorIntent{Preset: "efficient", ComputeClockCeilingPctOfMax: floatPtr(80)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := nh
			n.GPURawModel = tc.raw
			_, gpu := cfg.governorIntents("g", tc.profile, true, n, tc.product)
			if !reflect.DeepEqual(gpu, tc.want) {
				t.Fatalf("GPU governor = %s, want %s", intentString(gpu), intentString(tc.want))
			}
		})
	}
}

// GC07: the compute clock ceiling goes with the efficient preset only, and a
// ceiling outside (0, 100] falls back, with a warning, to 80 for the
// variable and to the variable's value for a model entry.
func TestGC07CeilingOnlyWithEfficientAndOutOfRangeFallsBack(t *testing.T) {
	nh := allPresetsHardware()
	for _, v := range []string{"0", "-5", "100.5", "150", "abc"} {
		buf := captureLog(t)
		cfg := useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "all", "GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX": v})
		_, gpu := cfg.governorIntents("g", profileEco, true, nh, "")
		if gpu == nil || gpu.ComputeClockCeilingPctOfMax == nil || *gpu.ComputeClockCeilingPctOfMax != 80 {
			t.Errorf("ceiling %q: GPU governor = %s, want efficient at 80", v, intentString(gpu))
		}
		if !strings.Contains(buf.String(), "warning: GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX") {
			t.Errorf("ceiling %q logged no warning:\n%s", v, buf)
		}
	}

	buf := captureLog(t)
	cfg := useGovernorEnv(t, map[string]string{
		"GOVERNOR_CONTROL":                    "all",
		"GPU_GOVERNOR_ECO_CEILING_PCT_OF_MAX": "100",
		"GPU_GOVERNOR_PERFORMANCE_PRESET":     "low",
		"GPU_GOVERNOR_MODEL_PRESETS_JSON": `{
			"LOW-ECO": {"eco": "low", "ecoCeilingPctOfMax": 60},
			"BAD-CEILING": {"ecoCeilingPctOfMax": 120}
		}`,
	})
	if !strings.Contains(buf.String(), `warning: GPU_GOVERNOR_MODEL_PRESETS_JSON["BAD-CEILING"].ecoCeilingPctOfMax`) {
		t.Errorf("an out-of-range model ceiling logged no warning:\n%s", buf)
	}
	for _, tc := range []struct {
		profile, product string
		want             *joulie.GPUGovernorIntent
	}{
		{profileEco, "", &joulie.GPUGovernorIntent{Preset: "efficient", ComputeClockCeilingPctOfMax: floatPtr(100)}},
		{profilePerformance, "", &joulie.GPUGovernorIntent{Preset: "low"}},
		{profileEco, "LOW-ECO", &joulie.GPUGovernorIntent{Preset: "low"}},
		{profileEco, "BAD-CEILING", &joulie.GPUGovernorIntent{Preset: "efficient", ComputeClockCeilingPctOfMax: floatPtr(100)}},
	} {
		_, gpu := cfg.governorIntents("g", tc.profile, true, nh, tc.product)
		if !reflect.DeepEqual(gpu, tc.want) {
			t.Errorf("%s %q: GPU governor = %s, want %s", tc.profile, tc.product, intentString(gpu), intentString(tc.want))
		}
	}
}

// The eco boost is written with the eco CPU preset only, and only to a node
// whose agent found a boost control.
func TestGovernorEcoBoostNeedsABoostControl(t *testing.T) {
	cfg := useGovernorEnv(t, map[string]string{"GOVERNOR_CONTROL": "cpu", "CPU_GOVERNOR_ECO_BOOST": "off"})
	nh := allPresetsHardware()
	if cpu, _ := cfg.governorIntents("c", profileEco, false, nh, ""); cpu == nil || cpu.Preset != "efficiency" || cpu.Boost != "off" {
		t.Fatalf("eco CPU governor = %+v, want efficiency with boost off", cpu)
	}
	if cpu, _ := cfg.governorIntents("c", profilePerformance, false, nh, ""); cpu == nil || cpu.Boost != "" {
		t.Fatalf("performance CPU governor = %+v, want no boost", cpu)
	}
	buf := captureLog(t)
	nh.CPUBoostAvailable = false
	if cpu, _ := cfg.governorIntents("c", profileEco, false, nh, ""); cpu == nil || cpu.Preset != "efficiency" || cpu.Boost != "" {
		t.Fatalf("eco CPU governor without a boost control = %+v, want efficiency and boost left alone", cpu)
	}
	if !strings.Contains(buf.String(), "no boost control") {
		t.Fatalf("no warning for a boost the node cannot set:\n%s", buf)
	}
}

func intentString(v any) string {
	switch g := v.(type) {
	case *joulie.GPUGovernorIntent:
		if g == nil {
			return "none"
		}
		if g.ComputeClockCeilingPctOfMax != nil {
			return fmt.Sprintf("%s ceiling %g", g.Preset, *g.ComputeClockCeilingPctOfMax)
		}
		return g.Preset
	case *joulie.CPUGovernorIntent:
		if g == nil {
			return "none"
		}
		return fmt.Sprintf("%s boost %q", g.Preset, g.Boost)
	}
	return fmt.Sprint(v)
}
