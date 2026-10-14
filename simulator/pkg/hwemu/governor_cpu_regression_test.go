package hwemu

import (
	"testing"
	"time"
)

// GR01: amd-pstate-epp offers every energy-performance preference again
// once a policy leaves performance (v6.12 and v6.16 amd-pstate.c,
// show_energy_performance_available_preferences), so an agent that sets
// powersave and then balance_power gets balance_power. The emulated policy
// kept the preference list of the governor it was rendered under: on the
// EPYC 9655 host, which boots performance, it took performance alone and
// reverted balance_power for the Node's whole life.
func TestGR01AMDPstateEPPTakesPreferencesAfterPerformance(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "amd-epyc-2s-hsmp")
	tr := renderTestTree(t, p)
	n := nodeNewNode(t, p, tr)
	gov := nodePolicyFile(0, "scaling_governor")
	pref := nodePolicyFile(0, "energy_performance_preference")
	if got := nodeRead(t, tr, gov); got != "performance\n" {
		t.Fatalf("the EPYC 9655 host boots %q, want performance", got)
	}
	nodeWrite(t, tr, gov, "powersave")
	nodeStep(t, n, time.Second)
	nodeWrite(t, tr, pref, "balance_power")
	nodeStep(t, n, time.Second)
	if got := nodeRead(t, tr, pref); got != "balance_power\n" {
		t.Fatalf("energy_performance_preference after powersave and balance_power = %q (events %+v), want balance_power", got, nodeEvents(n, pref))
	}
	if got, want := nodeRead(t, tr, nodePolicyFile(0, "energy_performance_available_preferences")), "default performance balance_performance balance_power power \n"; got != want {
		t.Fatalf("energy_performance_available_preferences under powersave = %q, want %q", got, want)
	}
}
