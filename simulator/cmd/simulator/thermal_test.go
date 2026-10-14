package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/matbun/joulie/simulator/pkg/phys"
	"github.com/prometheus/client_golang/prometheus"
)

// spec01203Curve is the node power of two AMD EPYC 9654 against load from
// SPECpower_ssj2008 result 01203: ASUS RS720A-E12-RS12, 2x AMD EPYC 9654,
// SLES 15 SP4, overall 30,064 ssj_ops/watt, retrieved 2026-10-03 from
// https://www.spec.org/power_ssj2008/results/res2022q4/power_ssj2008-20221204-01203.txt
// The 0% point is Active Idle. The catalogue's AMD_EPYC_9654 node2S curve is
// the same result.
var spec01203Curve = []hw.PowerPoint{
	{LoadPct: 0, PowerW: 128},
	{LoadPct: 10, PowerW: 257},
	{LoadPct: 20, PowerW: 300},
	{LoadPct: 30, PowerW: 340},
	{LoadPct: 40, PowerW: 378},
	{LoadPct: 50, PowerW: 410},
	{LoadPct: 60, PowerW: 442},
	{LoadPct: 70, PowerW: 498},
	{LoadPct: 80, PowerW: 577},
	{LoadPct: 90, PowerW: 697},
	{LoadPct: 100, PowerW: 817},
}

// clearSimEnv blanks every SIM_ variable of the environment running the
// test, so that defaultBaseModelFromEnv returns its code defaults plus what
// the test sets.
func clearSimEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "SIM_") {
			t.Setenv(k, "")
		}
	}
}

// With the old defaults (85 and 97 C) the 2x 9654 node at full load heads for
// 24 + 817/8 = 126.1 C, throttles fully, drops to 141 W with throughput 0,
// cools, and swings back: a limit cycle no real node shows. Throttling is
// therefore opt-in, and explicit thresholds keep the old behaviour.
func TestCurveNodeIsNotThermallyThrottledByDefault(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		throttled bool
	}{
		{name: "defaults"},
		{
			name: "explicit 85 and 97 C",
			env: map[string]string{
				"SIM_CPU_THERMAL_THROTTLE_START_C": "85",
				"SIM_CPU_THERMAL_THROTTLE_FULL_C":  "97",
			},
			throttled: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearSimEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			model := defaultBaseModelFromEnv()
			// main exits at startup on this error, so a default with one
			// threshold alone, which thermalThrottleFraction reads as off,
			// must not pass below.
			if err := hw.ValidateProfile(model); err != nil {
				t.Fatalf("base profile from the environment is invalid, the simulator would exit at startup: %v", err)
			}
			model.CPUPowerCurve = spec01203Curve
			s := newSimulatorWithRegisterer(model, nil, nil, 200, prometheus.NewRegistry())
			st := s.getNode("node-9654")
			st.CPUUtil = 1
			st.CPUWorkClass = "cpu.compute_bound"
			// The first call steps 5 s in one substep, the second 0 s.
			stepSec := 5.0
			for call := 1; call <= 2; call++ {
				power := stepOnce(st, model, stepSec)
				stepSec = 0
				throughput := cpuThroughputMultiplier(st.FreqScale, st.CPUWorkClass, model, st.MemoryIntensity, st.IOIntensity, st.CPUThermalThrottle)
				if tc.throttled {
					if st.CPUThermalThrottle <= 0 || throughput >= 1 || power >= 817 {
						t.Fatalf("call %d: explicit thresholds must still throttle at %.1f C: throttle=%.3f throughput=%.3f power=%.2f W",
							call, st.CPUTemperatureC, st.CPUThermalThrottle, throughput, power)
					}
					// Only the first call is checked: the node then cools and
					// recovers, which is the limit cycle itself.
					break
				}
				if st.CPUThermalThrottle != 0 || math.Abs(power-817) > 0.01 || math.Abs(throughput-1) > 1e-9 {
					t.Fatalf("call %d: default thresholds must not throttle (T0): temp=%.1f C throttle=%.3f power=%.2f W throughput=%.3f, want throttle 0, 817 W, throughput 1",
						call, st.CPUTemperatureC, st.CPUThermalThrottle, power, throughput)
				}
			}
		})
	}
}

// Eight GPUs at the MI300X maximum TBP of 750 W (AMD Instinct MI300X data
// sheet, https://www.amd.com/content/dam/amd/en/documents/instinct-tech-docs/data-sheets/amd-instinct-mi300x-data-sheet.pdf)
// running compute-bound work draw about 720 W each uncapped. The test caps
// them at 500 W, so that the cap binds and the thermal fraction reaches the
// throughput each device reports (perfMultiplier); below its cap a device
// reports 1 whatever its temperature. At the default 12 W/C a 500 W board
// heads for 24 + 500/12 = 65.7 C, below the old 82 C start, so the test runs
// them at 6 W/C: 24 + 500/6 = 107.3 C, above the old 92 C full threshold. The
// outcome then depends on the thresholds alone; board power would have to fall
// below 408 W before the old defaults stopped throttling fully. Job progress
// does not read this fraction (advanceJobProgress passes the node's CPU
// fraction to the GPU throughput model, hardware-modeling.md 6.6), so that
// path is not exercised here.
func TestGPUIsNotThermallyThrottledByDefault(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		throttled bool
	}{
		{name: "defaults"},
		{
			name: "explicit 82 and 92 C",
			env: map[string]string{
				"SIM_GPU_THERMAL_THROTTLE_START_C": "82",
				"SIM_GPU_THERMAL_THROTTLE_FULL_C":  "92",
			},
			throttled: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearSimEnv(t)
			t.Setenv("SIM_GPU_COUNT", "8")
			t.Setenv("SIM_GPU_MAX_WATTS_PER_GPU", "750")
			t.Setenv("SIM_GPU_WATTS_PER_DELTA_C", "6")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			model := defaultBaseModelFromEnv()
			if err := hw.ValidateProfile(model); err != nil {
				t.Fatalf("base profile from the environment is invalid, the simulator would exit at startup: %v", err)
			}
			s := newSimulatorWithRegisterer(model, nil, nil, 200, prometheus.NewRegistry())
			st := s.getNode("node-gpu")
			st.GPUUtil = 1
			st.GPUWorkClass = "gpu.compute_bound"
			st.GPUTargetCapWattsPerGpu = 500

			stepOnce(st, model, 5)
			if len(st.GPUDevices) != 8 {
				t.Fatalf("got %d GPU devices, want 8", len(st.GPUDevices))
			}
			// The throughput of a board capped at 500 W, before the thermal
			// factor.
			capOnly := phys.CappedBoardGPUModel{
				IdleW:         model.GPU.IdleWattsPerGPU,
				MaxW:          model.GPU.MaxWattsPerGPU,
				ComputeGamma:  model.GPU.ComputeGamma,
				MemoryEpsilon: model.GPU.MemoryEpsilon,
				MemoryGamma:   model.GPU.MemoryGamma,
			}.ThroughputMultiplier(phys.DeviceState{
				Utilization:    1,
				CapWatts:       500,
				MaxCapWatts:    model.GPU.MaxWattsPerGPU,
				IdlePowerWatts: model.GPU.IdleWattsPerGPU,
				Class:          "gpu.compute_bound",
			}, "gpu.compute_bound")
			if capOnly <= 0 || capOnly >= 1 {
				t.Fatalf("test setup: a 500 W cap must bind: cap-only throughput %.3f", capOnly)
			}
			for _, d := range st.GPUDevices {
				if math.Abs(d.CapWatts-500) > 1e-9 {
					t.Fatalf("gpu %d: test setup: cap %.1f W, want 500 W", d.Index, d.CapWatts)
				}
				if d.TemperatureC <= 92 {
					t.Fatalf("gpu %d: test setup: %.1f C at %.1f W is not above the old 92 C full threshold; lower SIM_GPU_WATTS_PER_DELTA_C", d.Index, d.TemperatureC, d.PowerWatts)
				}
				if want := capOnly * (1 - d.ThermalThrottle); math.Abs(d.PerfMultiplier-want) > 1e-9 {
					t.Fatalf("gpu %d: reported throughput %.3f, want the cap-only %.3f scaled by 1 - throttle %.3f = %.3f", d.Index, d.PerfMultiplier, capOnly, d.ThermalThrottle, want)
				}
				if tc.throttled && d.ThermalThrottle != 1 {
					t.Fatalf("gpu %d: explicit thresholds must throttle fully at %.1f C: throttle=%.3f", d.Index, d.TemperatureC, d.ThermalThrottle)
				}
				if !tc.throttled && d.ThermalThrottle != 0 {
					t.Fatalf("gpu %d: default thresholds must not throttle (T0): temp=%.1f C throttle=%.3f", d.Index, d.TemperatureC, d.ThermalThrottle)
				}
			}
		})
	}

	// With the stock 12 W/C and no cap, a board heads for 24 + 720/12 = 84 C,
	// above the old 82 C start, where the old defaults throttled it by 0.2.
	t.Run("defaults with the stock thermal constants", func(t *testing.T) {
		clearSimEnv(t)
		t.Setenv("SIM_GPU_COUNT", "8")
		t.Setenv("SIM_GPU_MAX_WATTS_PER_GPU", "750")
		model := defaultBaseModelFromEnv()
		s := newSimulatorWithRegisterer(model, nil, nil, 200, prometheus.NewRegistry())
		st := s.getNode("node-gpu")
		st.GPUUtil = 1
		st.GPUWorkClass = "gpu.compute_bound"

		stepOnce(st, model, 5)
		if len(st.GPUDevices) != 8 {
			t.Fatalf("got %d GPU devices, want 8", len(st.GPUDevices))
		}
		for _, d := range st.GPUDevices {
			if d.TemperatureC <= 82 {
				t.Fatalf("gpu %d: test setup: %.1f C at %.1f W is not above the old 82 C start", d.Index, d.TemperatureC, d.PowerWatts)
			}
			if d.ThermalThrottle != 0 {
				t.Fatalf("gpu %d: default thresholds must not throttle (T0): temp=%.1f C throttle=%.3f", d.Index, d.TemperatureC, d.ThermalThrottle)
			}
		}
	})
}

// Setting one threshold of a pair from the environment turns throttling on
// with the former default for the other, as before throttling became opt-in.
// With the other one left at 0, a start alone failed validation (the
// simulator exited at startup, and a class file adding GPUs lost every class)
// and a full alone throttled linearly from 0 C, already at idle.
func TestThermalThresholdAloneFromEnvKeepsFormerPartner(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want [4]float64 // CPU start, CPU full, GPU start, GPU full
	}{
		{
			name: "cpu start alone",
			env:  map[string]string{"SIM_CPU_THERMAL_THROTTLE_START_C": "85"},
			want: [4]float64{85, 97, 0, 0},
		},
		{
			name: "cpu full alone",
			env:  map[string]string{"SIM_CPU_THERMAL_THROTTLE_FULL_C": "100"},
			want: [4]float64{85, 100, 0, 0},
		},
		{
			// The GPU count is what makes ValidateProfile check the GPU keys.
			name: "gpu start alone with a gpu count",
			env:  map[string]string{"SIM_GPU_COUNT": "8", "SIM_GPU_THERMAL_THROTTLE_START_C": "80"},
			want: [4]float64{0, 0, 80, 92},
		},
		{
			name: "gpu full alone with a gpu count",
			env:  map[string]string{"SIM_GPU_COUNT": "8", "SIM_GPU_THERMAL_THROTTLE_FULL_C": "95"},
			want: [4]float64{0, 0, 82, 95},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearSimEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			model := defaultBaseModelFromEnv()
			// main exits with log.Fatalf on this error.
			if err := hw.ValidateProfile(model); err != nil {
				t.Fatalf("%v: the simulator would exit at startup: %v", tc.env, err)
			}
			got := [4]float64{model.CPUThermalThrottleStartC, model.CPUThermalThrottleFullC, model.GPU.ThermalThrottleStartC, model.GPU.ThermalThrottleFullC}
			if got != tc.want {
				t.Fatalf("%v: thresholds (cpu start, cpu full, gpu start, gpu full) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}

	t.Run("cpu full alone leaves an idle node unthrottled", func(t *testing.T) {
		clearSimEnv(t)
		t.Setenv("SIM_CPU_THERMAL_THROTTLE_FULL_C", "97")
		model := defaultBaseModelFromEnv()
		model.CPUPowerCurve = spec01203Curve
		s := newSimulatorWithRegisterer(model, nil, nil, 200, prometheus.NewRegistry())
		st := s.getNode("node-9654")

		stepOnce(st, model, 5)
		// Idle at 128 W heads for 24 + 128/8 = 40 C. A start of 0 would
		// give 40/97 = 0.41.
		if st.CPUThermalThrottle != 0 {
			t.Fatalf("idle node at %.1f C with only the full threshold set: throttle=%.3f, want 0", st.CPUTemperatureC, st.CPUThermalThrottle)
		}
	})

	t.Run("gpu start alone without a gpu count throttles catalogue gpus", func(t *testing.T) {
		clearSimEnv(t)
		t.Setenv("SIM_GPU_THERMAL_THROTTLE_START_C", "82")
		// 0 lets the catalogue fill in the board power, as on a node with
		// no GPU settings of its own.
		t.Setenv("SIM_GPU_MAX_WATTS_PER_GPU", "0")
		t.Setenv("SIM_GPU_WATTS_PER_DELTA_C", "4")
		base := defaultBaseModelFromEnv()
		if err := hw.ValidateProfile(base); err != nil {
			t.Fatalf("a GPU start threshold alone with no GPU count: %v", err)
		}
		catalog, err := hw.LoadCatalog("")
		if err != nil {
			t.Fatal(err)
		}
		s := newSimulatorWithRegisterer(base, nil, nil, 200, prometheus.NewRegistry())
		s.catalog = catalog
		// The node gets its GPUs from its labels through the catalogue.
		model, _ := s.resolveModelForNode(map[string]string{
			"joulie.io/hw.gpu-model": "NVIDIA H100 NVL",
			"joulie.io/hw.gpu-count": "8",
		})
		if model.GPU.Count != 8 || model.GPU.MaxWattsPerGPU <= 0 {
			t.Fatalf("test setup: catalogue gave %d GPUs at %.0f W, want 8 with a board power", model.GPU.Count, model.GPU.MaxWattsPerGPU)
		}
		st := s.getNode("node-gpu")
		st.GPUUtil = 1
		st.GPUWorkClass = "gpu.compute_bound"

		stepOnce(st, model, 5)
		if len(st.GPUDevices) != 8 {
			t.Fatalf("got %d GPU devices, want 8", len(st.GPUDevices))
		}
		for _, d := range st.GPUDevices {
			want := clamp01((d.TemperatureC - 82) / (92 - 82))
			if d.TemperatureC <= 82 || d.ThermalThrottle <= 0 || math.Abs(d.ThermalThrottle-want) > 1e-9 {
				t.Fatalf("gpu %d with start 82 C and the former 92 C full: temp=%.1f C throttle=%.3f, want above 82 C and throttle %.3f", d.Index, d.TemperatureC, d.ThermalThrottle, want)
			}
		}
	})

	t.Run("gpu start alone keeps a class file that adds gpus", func(t *testing.T) {
		clearSimEnv(t)
		t.Setenv("SIM_GPU_THERMAL_THROTTLE_START_C", "82")
		classes, err := loadNodeClasses(writeThermalClasses(t, "gpu:\n        count: 8"))
		if err != nil || len(classes) != 2 {
			t.Fatalf("class file under a GPU start threshold from the environment: got %d classes, err %v; want 2 and no error", len(classes), err)
		}
	})
}

// A node class that sets one threshold alone still takes the other one from
// the base profile, which is 0 unless the environment sets the pair. This is
// the sharp edge that remains, documented in hardware-modeling.md 5.5 and 6.6
// and in the configuration reference: a start alone fails validation and
// ParseClasses drops every class of the file, and a full alone throttles
// linearly from 0 C, already at idle. A fix in simulator/pkg/hw or in
// thermalThrottleFraction changes this test and those docs together.
func TestThermalThresholdAloneInNodeClass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		model   string
		wantErr string
	}{
		{
			name:    "cpu start alone drops every class",
			model:   "cpuThermalThrottleStartC: 85",
			wantErr: "cpuThermalThrottleFullC must be >= cpuThermalThrottleStartC",
		},
		{
			name:    "gpu count and start alone drops every class",
			model:   "gpu:\n        count: 8\n        thermalThrottleStartC: 82",
			wantErr: "gpu.thermalThrottleFullC must be >= gpu.thermalThrottleStartC",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearSimEnv(t)
			// main logs this error as a warning and runs every node on the
			// base profile, the valid class "plain" included.
			classes, err := loadNodeClasses(writeThermalClasses(t, tc.model))
			if err == nil || len(classes) != 0 || !strings.Contains(err.Error(), `class "hot"`) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("class file with a start threshold alone: got %d classes, err %v; want none and an error naming class \"hot\" and containing %q", len(classes), err, tc.wantErr)
			}
		})
	}

	// ValidateProfile checks the GPU keys only when gpu.count > 0.
	t.Run("gpu start alone without a count is accepted and stays off", func(t *testing.T) {
		clearSimEnv(t)
		classes, err := loadNodeClasses(writeThermalClasses(t, "gpu:\n        thermalThrottleStartC: 82"))
		if err != nil || len(classes) != 2 {
			t.Fatalf("got %d classes, err %v; want 2 and no error", len(classes), err)
		}
		s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, classes, 200, prometheus.NewRegistry())
		model, _ := s.resolveModelForNode(map[string]string{"pool": "hot"})
		if model.GPU.ThermalThrottleStartC != 82 || model.GPU.ThermalThrottleFullC != 0 {
			t.Fatalf("class gpu thresholds start=%.0f full=%.0f, want 82 and 0", model.GPU.ThermalThrottleStartC, model.GPU.ThermalThrottleFullC)
		}
		if f := thermalThrottleFraction(120, model.GPU.ThermalThrottleStartC, model.GPU.ThermalThrottleFullC); f != 0 {
			t.Fatalf("a start of 82 C and no full threshold at 120 C: throttle=%.3f, want 0", f)
		}
	})

	t.Run("cpu full alone throttles from 0 C", func(t *testing.T) {
		clearSimEnv(t)
		classes, err := loadNodeClasses(writeThermalClasses(t, "cpuThermalThrottleFullC: 97"))
		if err != nil || len(classes) != 2 {
			t.Fatalf("got %d classes, err %v; want 2 and no error", len(classes), err)
		}
		s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, classes, 200, prometheus.NewRegistry())
		model, _ := s.resolveModelForNode(map[string]string{"pool": "hot"})
		model.CPUPowerCurve = spec01203Curve
		st := s.getNode("node-9654")

		stepOnce(st, model, 5)
		// Idle at 128 W heads for 24 + 128/8 = 40 C, and 40/97 = 0.412.
		if want := st.CPUTemperatureC / 97; st.CPUThermalThrottle < 0.4 || math.Abs(st.CPUThermalThrottle-want) > 1e-9 {
			t.Fatalf("idle node at %.1f C: throttle=%.3f, want %.3f (linear from 0 C to 97 C)", st.CPUTemperatureC, st.CPUThermalThrottle, want)
		}
	})
}

// writeThermalClasses writes a class file with a valid class "plain" and a
// class "hot" whose model is hotModel, indented as a key under model:.
func writeThermalClasses(t *testing.T, hotModel string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "classes.yaml")
	doc := "classes:\n" +
		"  - name: plain\n    matchLabels: {pool: plain}\n    model:\n      cpuWattsPerDeltaC: 8\n" +
		"  - name: hot\n    matchLabels: {pool: hot}\n    model:\n      " + hotModel + "\n"
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
