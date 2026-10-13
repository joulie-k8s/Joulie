package scenario_test

import (
	"math"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/scenario"
)

// A comparator parses to an operator and a value in seconds, joules or
// watts, and holds exactly as its operator says, with == and != tolerant
// of float rounding only.
func TestComparatorParse(t *testing.T) {
	for _, c := range []struct {
		in    scenario.Comparator
		op    string
		value float64
		dim   scenario.Dimension
	}{
		{">= 0.95", ">=", 0.95, scenario.DimensionNone},
		{"==0", "==", 0, scenario.DimensionNone},
		{"!= -1", "!=", -1, scenario.DimensionNone},
		{"< 90s", "<", 90, scenario.DimensionTime},
		{"<= 250ms", "<=", 0.25, scenario.DimensionTime},
		{"> 2min", ">", 120, scenario.DimensionTime},
		{"< 1.5h", "<", 5400, scenario.DimensionTime},
		{"> 0 J", ">", 0, scenario.DimensionEnergy},
		{"<= 50 kJ", "<=", 50e3, scenario.DimensionEnergy},
		{"< 2.5e1 MJ", "<", 25e6, scenario.DimensionEnergy},
		{">= 300 W", ">=", 300, scenario.DimensionPower},
		{"  <= 1.10  ", "<=", 1.1, scenario.DimensionNone},
	} {
		e, err := c.in.Parse()
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if e.Op != c.op || math.Abs(e.Value-c.value) > 1e-9*math.Max(1, c.value) || e.Dim != c.dim {
			t.Errorf("%q parsed as %+v, want %s %g %q", c.in, e, c.op, c.value, c.dim)
		}
	}
	for _, bad := range []scenario.Comparator{"", "0.95", "=> 1", ">= one", ">= 1 kW", ">= 1s s", "> .5", "~ 1", ">= 1 sec"} {
		if e, err := bad.Parse(); err == nil {
			t.Errorf("%q parsed as %+v, want an error", bad, e)
		}
	}
	for _, c := range []struct {
		in       scenario.Comparator
		observed float64
		holds    bool
	}{
		{">= 0.95", 0.95, true}, {">= 0.95", 0.9499, false}, {"> 1", 1, false}, {"> 1", 1.0001, true},
		{"< 90s", 89.9, true}, {"< 90s", 90, false}, {"<= 50 kJ", 50000, true}, {"== 0", 0, true},
		{"== 0.3", 0.1 + 0.2, true}, {"!= 0.3", 0.1 + 0.2, false}, {"!= 0", 1, true}, {"<= 1", math.NaN(), false},
	} {
		e, err := c.in.Parse()
		if err != nil {
			t.Fatal(err)
		}
		if got := e.Holds(c.observed); got != c.holds {
			t.Errorf("%q holds for %g: %v, want %v", c.in, c.observed, got, c.holds)
		}
	}
}
