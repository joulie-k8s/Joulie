package policy

import "github.com/matbun/joulie/pkg/controller/inputs"

// Facility is what the facility input read in its last poll before a
// reconcile: the readings a policy may plan with. The controller manager
// takes it from the same snapshot the twins of that reconcile read, before
// it plans, so the plan and the twins never see two polls. A reading the
// poll could not get is unknown (its Known flag is false, or its key is
// missing from a map), never the value of an earlier poll. Every reading is
// unknown with ENABLE_FACILITY_METRICS off.
//
// No policy reads it yet; a carbon-aware policy will. The maps belong to the
// caller and must not be written.
type Facility struct {
	// CarbonIntensity is the grid carbon intensity in gCO2eq/kWh with its
	// signal, marginal or average; a policy compares it only with values of
	// the same signal.
	CarbonIntensity      inputs.CarbonIntensity
	CarbonIntensityKnown bool

	// AmbientC is the outdoor ambient temperature in degrees Celsius.
	AmbientC     float64
	AmbientKnown bool
	// ITPowerW and CoolingPowerW are the power of the IT equipment and of
	// the cooling, in watts.
	ITPowerW          float64
	ITPowerKnown      bool
	CoolingPowerW     float64
	CoolingPowerKnown bool

	// ZoneAmbientC is the ambient temperature of each cooling zone, keyed by
	// the joulie.io/cooling-zone node label value; RackPowerW the power of
	// each rack in watts, keyed by the joulie.io/rack value; NodeInletC the
	// inlet temperature of each node, keyed by node name.
	ZoneAmbientC map[string]float64
	RackPowerW   map[string]float64
	NodeInletC   map[string]float64
}
