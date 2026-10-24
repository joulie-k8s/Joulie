package inputs

import (
	"math"
	"strings"
	"testing"
)

// TestCarbonUnitFactorConvertsToGramsPerKWh: every mass over every energy
// converts to gCO2eq/kWh, whatever the case, the spaces and the gas suffix,
// and a unit outside that grammar is an error that names the accepted form.
func TestCarbonUnitFactorConvertsToGramsPerKWh(t *testing.T) {
	cases := []struct {
		unit string
		want float64
	}{
		{"", 1},
		{"gCO2eq/kWh", 1},
		{"g/kWh", 1},
		{"gCO2e/kWh", 1},
		{" g CO2eq / kWh ", 1},
		{"GCO2EQ/KWH", 1},
		{"kgCO2eq/MWh", 1},
		{"kgCO2/kWh", 1000},
		{"tCO2eq/MWh", 1000},
		{"gCO2eq/MWh", 0.001},
		// WattTime's co2_moer.
		{"lbsCO2/MWh", 0.45359237},
		{"lb/MWh", 0.45359237},
		{"lbs/kWh", 453.59237},
	}
	for _, c := range cases {
		got, err := CarbonUnitFactor(c.unit)
		if err != nil {
			t.Errorf("%q: %v", c.unit, err)
			continue
		}
		if math.Abs(got-c.want) > 1e-12*c.want {
			t.Errorf("%q: factor %v, want %v", c.unit, got, c.want)
		}
	}
	for _, unit := range []string{"gCO2eq", "kWh", "gCO2eq/MJ", "mg/kWh", "CO2/kWh", "g/kWh/h", "gCH4/kWh", "percent", "1"} {
		if f, err := CarbonUnitFactor(unit); err == nil {
			t.Errorf("%q: factor %v, want an error", unit, f)
		} else if !strings.Contains(err.Error(), CarbonUnitGramsPerKWh) {
			t.Errorf("%q: error %q does not name the accepted form", unit, err)
		}
	}
}

// TestParseCarbonSignal: marginal and average in any case; nothing else,
// so no value reaches a policy without saying which signal it is.
func TestParseCarbonSignal(t *testing.T) {
	for in, want := range map[string]CarbonSignal{
		"marginal": CarbonSignalMarginal, " Marginal ": CarbonSignalMarginal,
		"average": CarbonSignalAverage, "AVERAGE": CarbonSignalAverage,
	} {
		if got, err := ParseCarbonSignal(in); err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "moer", "forecast", "avg"} {
		if got, err := ParseCarbonSignal(in); err == nil {
			t.Errorf("%q: %q, want an error", in, got)
		}
	}
}

// TestCarbonIntensitySpecRead: the value comes out in gCO2eq/kWh with its
// signal; a WattTime MOER of 1000 lbs CO2/MWh is 453.59 g/kWh, and 0 is a
// reading. A spec without a signal, with an unknown unit, a value that is
// not finite and a negative value read nothing.
func TestCarbonIntensitySpecRead(t *testing.T) {
	got, err := CarbonIntensitySpec{Unit: "lbsCO2/MWh", Signal: "Marginal"}.Read(1000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.GramsPerKWh-453.59237) > 1e-9 || got.Signal != CarbonSignalMarginal {
		t.Errorf("1000 lbsCO2/MWh marginal read as %+v, want 453.59237 g/kWh marginal", got)
	}
	got, err = CarbonIntensitySpec{Signal: CarbonSignalAverage}.Read(212)
	if err != nil || got != (CarbonIntensity{GramsPerKWh: 212, Signal: CarbonSignalAverage}) {
		t.Errorf("212 in the default unit: %+v, %v; want 212 g/kWh average", got, err)
	}
	got, err = CarbonIntensitySpec{Signal: CarbonSignalMarginal}.Read(0)
	if err != nil || got != (CarbonIntensity{GramsPerKWh: 0, Signal: CarbonSignalMarginal}) {
		t.Errorf("0 marginal: %+v, %v; want a reading of 0 g/kWh marginal", got, err)
	}

	for _, c := range []struct {
		spec CarbonIntensitySpec
		v    float64
	}{
		{CarbonIntensitySpec{}, 100},
		{CarbonIntensitySpec{Unit: "gCO2eq/MJ", Signal: CarbonSignalAverage}, 100},
		{CarbonIntensitySpec{Signal: CarbonSignalAverage}, math.NaN()},
		{CarbonIntensitySpec{Signal: CarbonSignalAverage}, math.Inf(1)},
		{CarbonIntensitySpec{Signal: CarbonSignalAverage}, -250},
		{CarbonIntensitySpec{Unit: "lbsCO2/MWh", Signal: CarbonSignalMarginal}, -1e-9},
	} {
		if got, err := c.spec.Read(c.v); err == nil {
			t.Errorf("%+v read %v as %+v, want an error", c.spec, c.v, got)
		}
	}
}
