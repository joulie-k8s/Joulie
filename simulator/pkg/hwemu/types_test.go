package hwemu

import "testing"

// L04: the kernel converts a RAPL limit to whole power units without rounding
// up (intel_rapl_common.c:558-563, 744), so a limit one microwatt above a unit
// boundary reads back on the boundary.
func TestL04QuantizeUWFloorsToThePowerUnit(t *testing.T) {
	t.Parallel()
	cases := []struct{ v, unitUW, want int64 }{
		{120000001, 125000, 120000000},
		{120000000, 125000, 120000000},
		{120124999, 125000, 120000000},
		{124999, 125000, 0},
	}
	for _, c := range cases {
		if got := QuantizeUW(c.v, c.unitUW); got != c.want {
			t.Errorf("QuantizeUW(%d, %d) = %d, want %d", c.v, c.unitUW, got, c.want)
		}
	}
}

// H05: max_energy_range_uj is the 32-bit energy counter span in uJ
// (intel_rapl_common.c:33, 50, 215, 741). The agent adds it back on a wrap, so
// a wrong range shows up as a power spike. 61035 and 15300 are the package and
// DRAM units behind the values in the xeon-4socket-nfd-labels capture; 15258
// is ESU 16.
func TestH05MaxEnergyRangeUJMatchesTheKernel(t *testing.T) {
	t.Parallel()
	cases := []struct{ unitNJ, want int64 }{
		{61035, 262143328850},
		{15300, 65712999613},
		{15258, 65532610987},
	}
	for _, c := range cases {
		if got := MaxEnergyRangeUJ(c.unitNJ); got != c.want {
			t.Errorf("MaxEnergyRangeUJ(%d) = %d, want %d", c.unitNJ, got, c.want)
		}
	}
}
