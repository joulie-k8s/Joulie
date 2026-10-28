package hw

import (
	"strings"
	"testing"
)

func TestValidateProfile(t *testing.T) {
	base := Profile{
		BaseIdleW:                70,
		DefaultCapW:              500,
		PMaxW:                    420,
		AlphaUtil:                1.1,
		BetaFreq:                 1.2,
		FMinMHz:                  1200,
		FMaxMHz:                  3200,
		RaplCapMinW:              70,
		RaplCapMaxW:              600,
		DvfsRampMS:               500,
		CPUCapApplyTauMS:         250,
		CPUTelemetryWindowMS:     250,
		CPUAmbientTempC:          24,
		CPUThermalTauMS:          4000,
		CPUWattsPerDeltaC:        8,
		CPUThermalThrottleStartC: 85,
		CPUThermalThrottleFullC:  97,
		GPU: GPUProfile{
			Count:                 2,
			Vendor:                "nvidia",
			Product:               "L40S",
			IdleWattsPerGPU:       30,
			MaxWattsPerGPU:        350,
			MinCapWattsPerGPU:     200,
			CapApplyTauMS:         150,
			TelemetryWindowMS:     1000,
			AmbientTempC:          24,
			ThermalTauMS:          2500,
			WattsPerDeltaC:        12,
			ThermalThrottleStartC: 82,
			ThermalThrottleFullC:  92,
			ComputeGamma:          1.0,
			MemoryEpsilon:         0.2,
			MemoryGamma:           1.2,
		},
	}
	if err := ValidateProfile(base); err != nil {
		t.Fatalf("base profile should validate: %v", err)
	}

	bad := base
	bad.FMaxMHz = 1000
	if err := ValidateProfile(bad); err == nil {
		t.Fatalf("expected validation error when fMaxMHz < fMinMHz")
	}
}

func TestApplyOverrides(t *testing.T) {
	base := Profile{
		BaseIdleW:                80,
		PMaxW:                    400,
		AlphaUtil:                1,
		BetaFreq:                 1,
		FMinMHz:                  1200,
		FMaxMHz:                  3000,
		RaplCapMinW:              80,
		RaplCapMaxW:              500,
		DefaultCapW:              300,
		CPUWattsPerDeltaC:        8,
		CPUThermalThrottleStartC: 85,
		CPUThermalThrottleFullC:  97,
		GPU: GPUProfile{
			TelemetryWindowMS:     1000,
			AmbientTempC:          24,
			ThermalTauMS:          2500,
			WattsPerDeltaC:        12,
			ThermalThrottleStartC: 82,
			ThermalThrottleFullC:  92,
		},
	}
	newIdle := 60.0
	newRamp := 200
	gpuCount := 4
	out := ApplyOverrides(base, Overrides{BaseIdleW: &newIdle, DvfsRampMS: &newRamp, GPU: &GPUOverrides{Count: &gpuCount}})
	if out.BaseIdleW != 60 {
		t.Fatalf("BaseIdleW override failed")
	}
	if out.DvfsRampMS != 200 {
		t.Fatalf("DvfsRampMS override failed")
	}
	if out.GPU.Count != 4 {
		t.Fatalf("GPU count override failed")
	}
}

// A field a class sets is no longer the built-in constant, so the catalogue
// must not replace it; the fields the class leaves alone keep their mark.
func TestApplyOverridesClearsDefaultedMarks(t *testing.T) {
	all := Defaulted{PMaxW: true, CPUSocketCapMaxW: true, GPUMaxWattsPerGPU: true, GPUMinCapWattsPerGPU: true}
	w := 250.0
	for _, tc := range []struct {
		key   string
		o     Overrides
		clear func(*Defaulted)
		got   func(Profile) float64
	}{
		{"pMaxW", Overrides{PMaxW: &w}, func(d *Defaulted) { d.PMaxW = false }, func(p Profile) float64 { return p.PMaxW }},
		{"cpuSocketCapMaxW", Overrides{CPUSocketCapMaxW: &w}, func(d *Defaulted) { d.CPUSocketCapMaxW = false }, func(p Profile) float64 { return p.CPUSocketCapMaxW }},
		{"gpu.maxWattsPerGpu", Overrides{GPU: &GPUOverrides{MaxWattsPerGPU: &w}}, func(d *Defaulted) { d.GPUMaxWattsPerGPU = false }, func(p Profile) float64 { return p.GPU.MaxWattsPerGPU }},
		{"gpu.minCapWattsPerGpu", Overrides{GPU: &GPUOverrides{MinCapWattsPerGPU: &w}}, func(d *Defaulted) { d.GPUMinCapWattsPerGPU = false }, func(p Profile) float64 { return p.GPU.MinCapWattsPerGPU }},
	} {
		out := ApplyOverrides(Profile{Defaulted: all}, tc.o)
		want := all
		tc.clear(&want)
		if out.Defaulted != want {
			t.Errorf("Defaulted after a class sets %s = %+v, want %+v", tc.key, out.Defaulted, want)
		}
		if got := tc.got(out); got != w {
			t.Errorf("%s override not applied: got %v, want %v", tc.key, got, w)
		}
	}
	idle := 90.0
	if out := ApplyOverrides(Profile{Defaulted: all}, Overrides{BaseIdleW: &idle, GPU: &GPUOverrides{}}); out.Defaulted != all {
		t.Errorf("Defaulted after a class sets only baseIdleW = %+v, want every mark kept", out.Defaulted)
	}
}

// The removed knobs are found in any class and in any case, each once, and
// a class file that carries them still loads.
func TestRemovedClassKeys(t *testing.T) {
	file := []byte(`
classes:
  - name: a
    model:
      podW: 110
      dvfsDropWPerPct: 1.6
      gpu:
        count: 2
        powerModel:
          alphaUtil: 1.0
  - name: b
    model:
      PODW: 120
      noiseStddevW: 3
      baseIdleW: 90
`)
	got := RemovedClassKeys(file)
	want := []string{"model.podW", "model.dvfsDropWPerPct", "model.gpu.powerModel", "model.noiseStddevW"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("RemovedClassKeys = %v, want %v", got, want)
	}
	base := Profile{
		BaseIdleW: 80, PMaxW: 420, AlphaUtil: 1, BetaFreq: 1, FMinMHz: 1200, FMaxMHz: 3200,
		RaplCapMinW: 80, RaplCapMaxW: 5000, DefaultCapW: 5000, CPUWattsPerDeltaC: 8,
		GPU: GPUProfile{MaxWattsPerGPU: 300, MinCapWattsPerGPU: 80, WattsPerDeltaC: 12, ComputeGamma: 1, MemoryGamma: 1},
	}
	classes, err := ParseClasses(file, base)
	if err != nil || len(classes) != 2 {
		t.Fatalf("ParseClasses with removed keys: got %d classes, err %v; want 2 and no error", len(classes), err)
	}
	if got := RemovedClassKeys([]byte("classes:\n  - name: c\n    model:\n      baseIdleW: 90\n")); len(got) != 0 {
		t.Fatalf("RemovedClassKeys on a clean file = %v, want none", got)
	}
}
