package main

// The CPU presets a node publishes do not depend on the governor it runs:
// on amd-pstate-epp the kernel lists only performance in
// energy_performance_available_preferences while the governor is
// performance, and the full set under powersave, so balanced and efficiency
// are judged on the driver's own set (governor.EPPSupport).

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
)

// amdPstateEPPTree is two amd-pstate-epp policies in active mode under the
// governor gov, with the preference and the preference list the kernel shows
// under it (show_energy_performance_available_preferences, amd-pstate.c).
func amdPstateEPPTree(t *testing.T, gov string) string {
	t.Helper()
	root := t.TempDir()
	epp, available := "performance\n", "performance\n"
	if gov != "performance" {
		epp, available = "balance_performance\n", "default performance balance_performance balance_power power \n"
	}
	files := map[string]string{"devices/system/cpu/amd_pstate/status": "active\n"}
	for p := 0; p < 2; p++ {
		dir := fmt.Sprintf("devices/system/cpu/cpufreq/policy%d/", p)
		files[dir+"scaling_driver"] = "amd-pstate-epp\n"
		files[dir+"scaling_governor"] = gov + "\n"
		files[dir+"scaling_available_governors"] = "performance powersave\n"
		files[dir+"energy_performance_preference"] = epp
		files[dir+"energy_performance_available_preferences"] = available
		files[dir+"boost"] = "1\n"
	}
	sysfsFiles(t, root, files)
	return root
}

// An amd-pstate-epp node lists the same presets, with the same results and
// native states, and the same preference set, under its boot governor
// performance as under powersave.
func TestCPUPresetSupportDoesNotDependOnTheCurrentGovernor(t *testing.T) {
	wantPresets := []v1alpha1.GovernorPresetSupport{
		{Preset: joulie.CPUGovernorPresetPerformance, Result: joulie.GovernorResultApplied, Native: "performance"},
		{Preset: joulie.CPUGovernorPresetBalanced, Result: joulie.GovernorResultApplied, Native: "powersave epp=balance_performance"},
		{Preset: joulie.CPUGovernorPresetEfficiency, Result: joulie.GovernorResultApplied, Native: "powersave epp=balance_power"},
	}
	wantAvailable := []string{"default", "performance", "balance_performance", "balance_power", "power"}
	for _, gov := range []string{"performance", "powersave"} {
		t.Run(gov, func(t *testing.T) {
			pointAgentAtSysfs(t, amdPstateEPPTree(t, gov), "", nil)
			d := discoverCPUGovernor()
			if d == nil {
				t.Fatal("no CPU governor discovered")
			}
			if got := d.Inventory.Presets; !reflect.DeepEqual(got, wantPresets) {
				t.Errorf("presets under %s\n got %+v\nwant %+v", gov, got, wantPresets)
			}
			if d.Inventory.EPP == nil || !reflect.DeepEqual(d.Inventory.EPP.Available, wantAvailable) {
				t.Errorf("epp under %s = %+v, want available %v", gov, d.Inventory.EPP, wantAvailable)
			}
		})
	}
}
