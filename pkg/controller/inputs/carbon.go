package inputs

import (
	"fmt"
	"math"
	"strings"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// CarbonSignal is what a grid carbon intensity measures. A marginal and an
// average value of the same grid differ by design, so the two are never
// compared.
type CarbonSignal string

const (
	// CarbonSignalMarginal is the emission rate of the generators that
	// follow a change in demand, such as WattTime's co2_moer.
	CarbonSignalMarginal CarbonSignal = semconv.CarbonIntensitySignalMarginal
	// CarbonSignalAverage is the emission rate of the whole generation
	// mix, such as Electricity Maps' carbon intensity.
	CarbonSignalAverage CarbonSignal = semconv.CarbonIntensitySignalAverage
)

// ParseCarbonSignal reads marginal or average, ignoring case and
// surrounding space. Anything else, the empty string included, is an error.
func ParseCarbonSignal(s string) (CarbonSignal, error) {
	switch v := CarbonSignal(strings.ToLower(strings.TrimSpace(s))); v {
	case CarbonSignalMarginal, CarbonSignalAverage:
		return v, nil
	}
	return "", fmt.Errorf("carbon intensity signal %q, want %s or %s", s, CarbonSignalMarginal, CarbonSignalAverage)
}

// CarbonUnitGramsPerKWh is the unit Joulie reads carbon intensity in, and
// the default unit of a carbon intensity query.
const CarbonUnitGramsPerKWh = "gCO2eq/kWh"

// Masses and energies a carbon intensity unit may name, in grams and
// kilowatt-hours. A pound is the international avoirdupois pound.
var (
	carbonUnitMassGrams = map[string]float64{"g": 1, "kg": 1e3, "t": 1e6, "lb": 453.59237, "lbs": 453.59237}
	carbonUnitEnergyKWh = map[string]float64{"kwh": 1, "mwh": 1e3}
)

// CarbonUnitFactor returns the factor that converts a value in unit to
// gCO2eq/kWh. A unit is a mass over an energy: the mass g, kg, t (metric
// tonne), lb or lbs, optionally followed by CO2, CO2e or CO2eq; the energy
// kWh or MWh. Case and spaces do not matter, so lbs CO2/MWh (WattTime's
// co2_moer) gives 0.45359237 and kgCO2eq/MWh gives 1. Empty is gCO2eq/kWh.
// Any other unit is an error.
func CarbonUnitFactor(unit string) (float64, error) {
	u := strings.ToLower(strings.Join(strings.Fields(unit), ""))
	if u == "" {
		return 1, nil
	}
	mass, energy, ok := strings.Cut(u, "/")
	if ok {
		for _, gas := range []string{"co2eq", "co2e", "co2"} {
			if m, found := strings.CutSuffix(mass, gas); found {
				mass = m
				break
			}
		}
		g, okMass := carbonUnitMassGrams[mass]
		kwh, okEnergy := carbonUnitEnergyKWh[energy]
		if okMass && okEnergy {
			return g / kwh, nil
		}
	}
	return 0, fmt.Errorf("carbon intensity unit %q, want a mass of g, kg, t, lb or lbs (optionally followed by CO2, CO2e or CO2eq) over kWh or MWh, such as %s or lbsCO2/MWh", unit, CarbonUnitGramsPerKWh)
}

// CarbonIntensitySpec is how the value of the carbon intensity query is
// read: its unit and its signal.
type CarbonIntensitySpec struct {
	// Unit is the unit the query returns (CarbonUnitFactor); empty is
	// gCO2eq/kWh.
	Unit string
	// Signal is what the value measures. It has no default.
	Signal CarbonSignal
}

// Check reports a unit CarbonUnitFactor does not know and a signal that is
// neither marginal nor average.
func (s CarbonIntensitySpec) Check() error {
	if _, err := CarbonUnitFactor(s.Unit); err != nil {
		return err
	}
	if _, err := ParseCarbonSignal(string(s.Signal)); err != nil {
		return err
	}
	return nil
}

// CarbonIntensity is one grid carbon intensity reading.
type CarbonIntensity struct {
	// GramsPerKWh is the value in gCO2eq/kWh.
	GramsPerKWh float64
	// Signal is what the value measures.
	Signal CarbonSignal
}

// Read converts v, a value of the query in s.Unit, to a reading in
// gCO2eq/kWh of signal s.Signal. A spec that fails Check, a value that is
// not finite and a negative value are errors: no grid emits less than
// nothing, by either signal, so a negative value comes from a broken
// exporter or query. Zero, a grid of curtailed renewables at the margin,
// is a reading.
func (s CarbonIntensitySpec) Read(v float64) (CarbonIntensity, error) {
	if err := s.Check(); err != nil {
		return CarbonIntensity{}, err
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return CarbonIntensity{}, fmt.Errorf("carbon intensity %v is not finite", v)
	}
	if v < 0 {
		return CarbonIntensity{}, fmt.Errorf("carbon intensity %v is negative", v)
	}
	factor, _ := CarbonUnitFactor(s.Unit)
	signal, _ := ParseCarbonSignal(string(s.Signal))
	return CarbonIntensity{GramsPerKWh: v * factor, Signal: signal}, nil
}
