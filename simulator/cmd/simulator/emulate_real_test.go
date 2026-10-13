package main

import (
	"math"
	"testing"
	"time"
)

// EM11: over ten one-second ticks of real hwemu nodes, the host ledger of
// each node grows by the integral of the host power the snapshots report,
// so the energy the simulator exports and the power it exports describe the
// same machine. The integral is the trapezoid rule over the tick powers,
// from the power of the first tick. The VM reports no host power and its
// ledger stays where it is: no collector could measure either.
func TestEM11HostLedgerIsTheIntegralOfHostPower(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{})
	const ticks = 10
	first := et.tick(t, emulateTestStart.Add(time.Second))
	prevW, startJ := map[string]float64{}, map[string]float64{}
	integral := map[string]float64{}
	for _, n := range first.Nodes {
		prevW[n.Name], startJ[n.Name] = n.Host.PowerW, n.Host.EnergyJ
	}
	last := first
	for k := 2; k <= ticks+1; k++ {
		last = et.tick(t, emulateTestStart.Add(time.Duration(k)*time.Second))
		for _, n := range last.Nodes {
			integral[n.Name] += (prevW[n.Name] + n.Host.PowerW) / 2
			prevW[n.Name] = n.Host.PowerW
		}
	}
	for _, pool := range []string{"intel", "nvl", "vm"} {
		node := et.node(t, pool)
		var endJ float64
		measured := false
		for _, n := range last.Nodes {
			if n.Name == node {
				endJ, measured = n.Host.EnergyJ, n.Host.Measured
			}
		}
		delta, want := endJ-startJ[node], integral[node]
		t.Logf("%s: ledger +%.3f J, power integral %.3f J", node, delta, want)
		if pool == "vm" {
			if measured || delta != 0 || want != 0 {
				t.Errorf("%s: measured %v, ledger +%v J, power integral %v J; want unmeasured and both 0", node, measured, delta, want)
			}
			continue
		}
		if want <= 0 {
			t.Errorf("%s: host power integral %.3f J over %d s, want above 0 under load", node, want, ticks)
			continue
		}
		if math.Abs(delta-want) > 0.001*want {
			t.Errorf("%s: host ledger grew %.3f J over %d s, power integral %.3f J; want within 0.1 %%", node, delta, ticks, want)
		}
	}
}
