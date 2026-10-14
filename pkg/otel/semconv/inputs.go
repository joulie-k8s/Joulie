package semconv

// The controller manager's inputs: the metrics of this table it reads back
// from Prometheus with its default presets. The simulator and any collector
// that follows the table write them; the controller manager spells them in
// PromQL through package promname, so a rename here moves the writer and the
// reader together. This file declares no new name.

// NodeInputs returns the metrics the node power input reads: component
// energy and power, summed over the cpu and gpu components under the default
// scope (components), then whole-host energy and power under the host scope.
// In each pair the energy counter is read first, as a rate, and the power
// gauge is the fallback for a component or host without one. The result is
// a copy.
func NodeInputs() []Metric {
	return inputMetricCopies(HwEnergy, HwPower, HwHostEnergy, HwHostPower)
}

// FacilityInputs returns the metrics the facility inputs read: the outdoor
// ambient temperature, IT and cooling energy and power, the ambient
// temperature of each cooling zone, rack energy and power, and the inlet
// temperature of each node, which is the host's ambient temperature. The PUE
// gauge is not an input: the controller manager derives PUE from IT and
// cooling power. The result is a copy.
func FacilityInputs() []Metric {
	return inputMetricCopies(
		JoulieFacilityAmbientTemperature,
		JoulieFacilityITEnergy, JoulieFacilityITPower,
		JoulieFacilityCoolingEnergy, JoulieFacilityCoolingPower,
		JoulieCoolingZoneAmbientTemperature,
		JoulieRackEnergy, JoulieRackPower,
		HwHostAmbientTemperature,
	)
}

// inputMetricCopies returns ms with their own Attributes, so a caller's write
// never reaches the package variables.
func inputMetricCopies(ms ...Metric) []Metric {
	out := make([]Metric, len(ms))
	for i, m := range ms {
		m.Attributes = append([]string(nil), m.Attributes...)
		out[i] = m
	}
	return out
}
