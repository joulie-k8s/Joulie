package semconv

// Names of the carbon stage: the carbon intensity of the electricity grid
// that supplies the cluster. The controller manager reads it from a query
// the operator configures (FACILITY_CARBON_INTENSITY_QUERY) and exports what
// it read under this name. It is a cluster metric, never a NodeTwin field.
// The semantic conventions v1.44.0 define no carbon or grid names, so these
// are Joulie's own.

// Attributes of the carbon stage.
const (
	// AttrJoulieGridCarbonIntensitySignal says what a carbon intensity
	// measures: CarbonIntensitySignalMarginal or
	// CarbonIntensitySignalAverage. Values of different signals are never
	// compared.
	AttrJoulieGridCarbonIntensitySignal = "joulie.grid.carbon_intensity.signal"
)

// Attribute values of the carbon stage.
const (
	// CarbonIntensitySignalMarginal is the emission rate of the generators
	// that follow a change in demand: what one more kilowatt-hour emits
	// now, such as WattTime's co2_moer.
	CarbonIntensitySignalMarginal = "marginal"
	// CarbonIntensitySignalAverage is the emission rate of the whole
	// generation mix, such as Electricity Maps' carbon intensity.
	CarbonIntensitySignalAverage = "average"
)

func init() {
	registerAttributes(AttrJoulieGridCarbonIntensitySignal)
}

// Metrics of the carbon stage. The unit is grams per kilowatt-hour, written
// g/(kW.h): UCUM reads the period and the solidus left to right with equal
// precedence, so g/kW.h would be (g/kW).h. The mass is CO2 equivalent, or
// CO2 alone for a provider that reports only CO2.
var (
	JoulieGridCarbonIntensity = Register(Metric{
		Name: "joulie.grid.carbon_intensity", Unit: "g/(kW.h)", Owner: OwnerJoulie,
		Kind: Gauge, Value: Double, Attributes: []string{AttrJoulieGridCarbonIntensitySignal},
		Brief: "Carbon intensity of the electricity grid that supplies the cluster, in grams of CO2 equivalent, or of CO2 alone when the provider reports only CO2, per kilowatt-hour.",
	})
)
