// Package facility is the simulator's model of the data centre around the
// nodes: an outdoor ambient temperature that follows a daily sinusoid, and a
// PUE that rises with that temperature and with the IT load, from which the
// cooling power follows. Both the legacy simulator and emulate mode compute
// the facility through Compute, so the two report the same model.
package facility

import (
	"math"
	"time"
)

// Params is the ambient temperature model: BaseAmbientC plus a sinusoid of
// AmplitudeC with a period of PeriodH hours, zero at the start. A PeriodH of
// zero or less means 24 hours.
type Params struct {
	BaseAmbientC float64
	AmplitudeC   float64
	PeriodH      float64
}

// Values is the facility at one instant. Nothing is rounded; the legacy
// gauges round for display.
type Values struct {
	AmbientC      float64
	ITPowerW      float64
	PUE           float64
	CoolingPowerW float64
}

// PUE model terms: a base PUE of 1.08 at 15 C, plus 0.006 per degree above
// 15 C, plus 0.04 at full load, plus a temperature and load interaction of
// 0.015 at full load and 30 C; the result is kept within 1 to 3. The values
// are assumptions of the simulator, not fitted to a facility.
const (
	facilityBasePUE        = 1.08
	facilityRefTempC       = 15.0
	facilityTempCoeff      = 0.006
	facilityLoadCoeff      = 0.04
	facilityInterCoeff     = 0.015
	facilityInterSpanC     = 15.0
	facilityMinPUE         = 1.0
	facilityMaxPUE         = 3.0
	facilityDefaultPeriodH = 24.0
)

// Compute returns the facility at elapsed time since the start, with
// itPowerW of IT load, and the new peak IT power. The load fraction that
// drives the PUE is itPowerW over the peak IT power seen so far, standing in
// for the design capacity; the peak only grows. Cooling power is
// itPowerW x (PUE - 1).
func Compute(p Params, elapsed time.Duration, itPowerW, peakITPowerW float64) (Values, float64) {
	periodSec := p.PeriodH * 3600.0
	if periodSec <= 0 {
		periodSec = facilityDefaultPeriodH * 3600.0
	}
	ambientC := p.BaseAmbientC + p.AmplitudeC*math.Sin(2*math.Pi*elapsed.Seconds()/periodSec)

	if itPowerW > peakITPowerW {
		peakITPowerW = itPowerW
	}
	loadFrac := 0.0
	if peakITPowerW > 0 {
		loadFrac = itPowerW / peakITPowerW
	}
	pue := facilityBasePUE +
		facilityTempCoeff*(ambientC-facilityRefTempC) +
		facilityLoadCoeff*loadFrac +
		facilityInterCoeff*loadFrac*(ambientC-facilityRefTempC)/facilityInterSpanC
	if pue < facilityMinPUE {
		pue = facilityMinPUE
	} else if pue > facilityMaxPUE {
		pue = facilityMaxPUE
	}
	return Values{
		AmbientC:      ambientC,
		ITPowerW:      itPowerW,
		PUE:           pue,
		CoolingPowerW: itPowerW * (pue - 1.0),
	}, peakITPowerW
}
