package facility

import (
	"math"
	"testing"
	"time"
)

func facilityNear(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.12f, want %.12f", what, got, want)
	}
}

// FA01: the model the legacy simulator computes in updateFacilityMetrics. At
// 25 C and 800 W with the peak at 800 W, the PUE is 1.08 + 0.06 + 0.04 + 0.01
// = 1.19 and cooling 800 x 0.19 = 152 W, as
// TestUpdateFacilityMetricsComputesITAndCoolingPower in simulator/cmd/simulator
// expects. The peak only grows, and a lower load reads as a lower fraction of
// it.
func TestFA01ComputeMatchesTheLegacyModel(t *testing.T) {
	p := Params{BaseAmbientC: 25, AmplitudeC: 0, PeriodH: 24}

	v, peak := Compute(p, time.Hour, 800, 0)
	facilityNear(t, "ambient", v.AmbientC, 25)
	facilityNear(t, "IT power", v.ITPowerW, 800)
	facilityNear(t, "PUE", v.PUE, 1.19)
	facilityNear(t, "cooling", v.CoolingPowerW, 152)
	facilityNear(t, "peak", peak, 800)

	// Half the peak: load fraction 0.5, PUE 1.08 + 0.06 + 0.02 + 0.005.
	v, peak = Compute(p, time.Hour, 400, peak)
	facilityNear(t, "peak after a lower load", peak, 800)
	facilityNear(t, "PUE at half load", v.PUE, 1.165)
	facilityNear(t, "cooling at half load", v.CoolingPowerW, 400*0.165)

	_, peak = Compute(p, time.Hour, 1000, peak)
	facilityNear(t, "peak after a higher load", peak, 1000)

	// No load: no cooling, PUE from temperature alone.
	v, _ = Compute(p, 0, 0, 0)
	facilityNear(t, "PUE without load", v.PUE, 1.14)
	facilityNear(t, "cooling without load", v.CoolingPowerW, 0)
}

// The ambient temperature is a sinusoid that starts at the base, peaks a
// quarter period in, and uses 24 hours when no period is set; the PUE stays
// within 1 to 3.
func TestFA01AmbientSinusoidAndPUEBounds(t *testing.T) {
	v, _ := Compute(Params{BaseAmbientC: 20, AmplitudeC: 5, PeriodH: 24}, 6*time.Hour, 0, 0)
	facilityNear(t, "ambient a quarter period in", v.AmbientC, 25)
	v, _ = Compute(Params{BaseAmbientC: 20, AmplitudeC: 5, PeriodH: 0}, 18*time.Hour, 0, 0)
	facilityNear(t, "ambient three quarters into the default period", v.AmbientC, 15)
	v, _ = Compute(Params{BaseAmbientC: 20, AmplitudeC: 5, PeriodH: 2}, 30*time.Minute, 0, 0)
	facilityNear(t, "ambient a quarter into a 2 h period", v.AmbientC, 25)

	v, _ = Compute(Params{BaseAmbientC: -100}, 0, 100, 100)
	facilityNear(t, "PUE floor", v.PUE, 1)
	facilityNear(t, "cooling at the floor", v.CoolingPowerW, 0)
	v, _ = Compute(Params{BaseAmbientC: 500}, 0, 100, 100)
	facilityNear(t, "PUE ceiling", v.PUE, 3)
	facilityNear(t, "cooling at the ceiling", v.CoolingPowerW, 200)
}
