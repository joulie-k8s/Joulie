package main

// Regression test of the sweep's CPU discovery: govsweep read the raw
// energy_performance_available_preferences file, which amd-pstate-epp shrinks
// to "performance" while the governor is performance. The balanced and
// efficiency arms then planned plain powersave, while the agent plans
// powersave with a preference, so the sweep did not measure what Joulie
// writes on those hosts.

import (
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
)

// On an amd-pstate-epp host booted under performance, the sweep plans the
// balanced and efficiency presets with the preference the agent writes.
func TestSweepPlansTheAgentsEPPOnAnAMDPstateEPPHost(t *testing.T) {
	_, tree, _ := hwemutest.NewNode(t, "amd-epyc-2s-hsmp", nil)
	f := cpuFacts(tree.SysRoot, tree.CPUInfoPath)
	for preset, want := range map[string]string{
		joulie.CPUGovernorPresetBalanced:   "powersave epp=balance_performance",
		joulie.CPUGovernorPresetEfficiency: "powersave epp=balance_power",
	} {
		plan, err := governor.PlanCPU(f, preset, "")
		if err != nil {
			t.Fatalf("PlanCPU(%s): %v", preset, err)
		}
		if plan.Native != want || plan.Result != joulie.GovernorResultApplied {
			t.Errorf("%s plans %q (%s), want %q (applied), as the agent does", preset, plan.Native, plan.Result, want)
		}
	}
}
