package hw

import "testing"

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
			PowerModel: GPUPowerModel{
				AlphaUtil: 1.0,
				BetaCap:   1.0,
			},
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
