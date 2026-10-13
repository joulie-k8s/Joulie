package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/matbun/joulie/api/v1alpha1"
)

// GV01 (pkg/api side): the governor mirrors carry the CRD's JSON names, so a
// value written through one reads back through the other unchanged.
func TestGV01GovernorMirrorsMatchTheCRDTypes(t *testing.T) {
	ceiling := 75.0
	pairs := []struct {
		name     string
		crd, api any
	}{
		{"CPUGovernorIntent", &v1alpha1.CPUGovernorIntent{Preset: CPUGovernorPresetEfficiency, Boost: CPUGovernorBoostOff}, &CPUGovernorIntent{}},
		{"GPUGovernorIntent", &v1alpha1.GPUGovernorIntent{Preset: GPUGovernorPresetEfficient, ComputeClockCeilingPctOfMax: &ceiling}, &GPUGovernorIntent{}},
		{"ControlStatus", &v1alpha1.ControlStatus{
			CPUGovernor: &v1alpha1.ControlResult{Backend: GovernorBackendCPUFreq, Result: GovernorResultEquivalent, Applied: "conservative", UpdatedAt: "2026-01-01T00:00:00Z"},
			GPUGovernor: &v1alpha1.ControlResult{Backend: GovernorBackendNvidiaSMI, Result: GovernorResultAppliedUnverified, Applied: "-lgc 300,1500"},
		}, &ControlStatus{}},
		{"CPUGovernorInventory", &v1alpha1.CPUGovernorInventory{
			Driver: "amd-pstate-epp", DriverMode: CPUDriverModeActive, Current: "powersave", Available: []string{"performance", "powersave"},
			EPP:      &v1alpha1.CPUEPPInventory{Current: "balance_performance", Available: []string{"performance", "balance_power"}},
			Boost:    &v1alpha1.CPUBoostInventory{State: CPUGovernorBoostOn, Scope: CPUBoostScopePerCPU},
			Presets:  []v1alpha1.GovernorPresetSupport{{Preset: CPUGovernorPresetBalanced, Result: GovernorResultApplied, Native: "powersave"}},
			Baseline: &v1alpha1.CPUGovernorBaseline{Groups: []v1alpha1.CPUGovernorState{{Policies: "0-7", Governor: "powersave", EPP: "balance_performance", Boost: CPUGovernorBoostOn}}, RecordedAt: "2026-01-01T00:00:00Z"},
		}, &CPUGovernorInventory{}},
		{"GPUGovernorInventory", &v1alpha1.GPUGovernorInventory{
			Mechanism: GovernorBackendNvidiaSMI, Current: GovernorStateUnknown,
			Presets:        []v1alpha1.GovernorPresetSupport{{Preset: GPUGovernorPresetSustained, Result: GovernorResultEquivalent}},
			ClockRangesMHz: &v1alpha1.GPUClockRanges{Graphics: &v1alpha1.ClockRangeMHz{Min: 300, Max: 1980}, Memory: &v1alpha1.ClockRangeMHz{Min: 1500, Max: 1500}},
			Baseline:       &v1alpha1.GPUGovernorBaseline{Devices: []v1alpha1.GPUGovernorDeviceState{{BusID: "00000000:18:00.0", Level: GovernorStateUnknown}}},
			Writable:       true,
		}, &GPUGovernorInventory{}},
	}
	for _, p := range pairs {
		want, err := json.Marshal(p.crd)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(want, p.api); err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		got, err := json.Marshal(p.api)
		if err != nil {
			t.Fatal(err)
		}
		var a, b map[string]any
		_ = json.Unmarshal(want, &a)
		_ = json.Unmarshal(got, &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: the pkg/api mirror changes the JSON\n crd: %s\n api: %s", p.name, want, got)
		}
	}

	hw := NodeHardware{CPU: NodeHardwareCPU{Governor: &CPUGovernorInventory{Driver: "acpi-cpufreq"}}, GPU: NodeHardwareGPU{Governor: &GPUGovernorInventory{Mechanism: GovernorBackendAMDGPUSysfs}}}
	spec := NodeTwinSpec{CPU: &NodeTwinCPU{Governor: &CPUGovernorIntent{Preset: CPUGovernorPresetPerformance}}, GPU: &NodeTwinGPU{Governor: &GPUGovernorIntent{Preset: GPUGovernorPresetAuto}}}
	for name, v := range map[string]any{"NodeHardware": hw, "NodeTwinSpec": spec} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, c := range []string{"cpu", "gpu"} {
			if _, ok := m[c].(map[string]any)["governor"]; !ok {
				t.Errorf("%s: %s.governor missing from %s", name, c, b)
			}
		}
	}
}

// The value lists hold their constants once each; a duplicate would hide a
// missing value from the schema guard in tests/contracts.
func TestGovernorValueListsHaveNoDuplicates(t *testing.T) {
	for name, list := range map[string][]string{
		"CPUGovernorPresetValues":           CPUGovernorPresetValues,
		"CPUGovernorBoostValues":            CPUGovernorBoostValues,
		"GPUGovernorPresetValues":           GPUGovernorPresetValues,
		"GovernorResultValues":              GovernorResultValues,
		"GovernorPresetSupportResultValues": GovernorPresetSupportResultValues,
		"GovernorBackendValues":             GovernorBackendValues,
		"GPUGovernorMechanismValues":        GPUGovernorMechanismValues,
		"CPUDriverModeValues":               CPUDriverModeValues,
		"CPUBoostScopeValues":               CPUBoostScopeValues,
	} {
		seen := map[string]bool{}
		for _, v := range list {
			if v == "" || seen[v] {
				t.Errorf("%s: empty or duplicate value %q", name, v)
			}
			seen[v] = true
		}
	}
	// Presets are not native words: a CPU preset that equals a governor
	// name other than performance, or a GPU preset that equals an amdgpu
	// level other than auto and low, would let a native word pass for one.
	for _, native := range []string{"powersave", "schedutil", "ondemand", "conservative", "userspace", "high", "manual", "profile_peak", "profile_standard"} {
		for _, list := range [][]string{CPUGovernorPresetValues, GPUGovernorPresetValues} {
			for _, v := range list {
				if v == native {
					t.Errorf("preset %q is a native word", v)
				}
			}
		}
	}
}
