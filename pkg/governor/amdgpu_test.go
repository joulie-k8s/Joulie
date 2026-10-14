package governor

import (
	"reflect"
	"slices"
	"strconv"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

const amdDir = "bus/pci/devices/0000:c1:00.0/"

// amdFacts is a device of the family with neutral clocks.
func amdFacts(family string, od bool) AMDFacts {
	return AMDFacts{BusID: "0000:c1:00.0", Family: family, Level: "auto", OD: od,
		SclkMHz: [2]int{500, 1500}, MclkMHz: [2]int{300, 1200}, FclkMHz: [2]int{400, 1400}}
}

func levelPlan(level string) Plan {
	return Plan{Writes: []Write{{amdDir + "power_dpm_force_performance_level", level}}, State: State{Level: level}, Native: level, Result: joulie.GovernorResultApplied}
}

func odPlan(mhz int) Plan {
	v := strconv.Itoa(mhz)
	return Plan{
		Writes: []Write{
			{amdDir + "power_dpm_force_performance_level", "manual"},
			{amdDir + "pp_od_clk_voltage", "s 1 " + v},
			{amdDir + "pp_od_clk_voltage", "c"},
		},
		State:  State{Level: "manual", SclkMaxMHz: mhz},
		Native: "manual sclk_max=" + v, Result: joulie.GovernorResultApplied,
	}
}

var amdUnsupported = Plan{Result: joulie.GovernorResultUnsupported}

// GA06: every row of the AMD table (the mapping in pkg/governor) at the default
// 80 % ceiling, to files the driver spells that way.
func TestGA06EveryAMDTableRowPlansItsNativeWords(t *testing.T) {
	rows := []struct {
		name  string
		facts AMDFacts
		want  map[string]Plan
	}{
		{"W7900 (navi31)", amdFacts(AMDFamilyNavi31, false), map[string]Plan{
			joulie.GPUGovernorPresetAuto:      levelPlan("auto"),
			joulie.GPUGovernorPresetSustained: levelPlan("high"),
			joulie.GPUGovernorPresetEfficient: amdUnsupported,
			joulie.GPUGovernorPresetLow:       levelPlan("low"),
			joulie.GPUGovernorPresetPeak:      levelPlan("profile_peak"),
		}},
		{"MI210/MI250X (aldebaran)", amdFacts(AMDFamilyAldebaran, true), map[string]Plan{
			joulie.GPUGovernorPresetAuto:      levelPlan("auto"),
			joulie.GPUGovernorPresetSustained: levelPlan("high"),
			joulie.GPUGovernorPresetEfficient: odPlan(1200),
			joulie.GPUGovernorPresetLow:       levelPlan("low"),
			joulie.GPUGovernorPresetPeak:      levelPlan("profile_peak"),
		}},
		{"MI300X (mi300)", amdFacts(AMDFamilyMI300, true), map[string]Plan{
			joulie.GPUGovernorPresetAuto:      levelPlan("auto"),
			joulie.GPUGovernorPresetSustained: amdUnsupported,
			joulie.GPUGovernorPresetEfficient: odPlan(1200),
			joulie.GPUGovernorPresetLow:       amdUnsupported,
			joulie.GPUGovernorPresetPeak:      amdUnsupported,
		}},
		{"aldebaran without overdrive", amdFacts(AMDFamilyAldebaran, false), map[string]Plan{
			joulie.GPUGovernorPresetAuto:      levelPlan("auto"),
			joulie.GPUGovernorPresetSustained: levelPlan("high"),
			joulie.GPUGovernorPresetEfficient: amdUnsupported,
			joulie.GPUGovernorPresetLow:       levelPlan("low"),
			joulie.GPUGovernorPresetPeak:      levelPlan("profile_peak"),
		}},
		{"unknown family", amdFacts("vega20", true), map[string]Plan{
			joulie.GPUGovernorPresetAuto:      amdUnsupported,
			joulie.GPUGovernorPresetSustained: amdUnsupported,
			joulie.GPUGovernorPresetEfficient: amdUnsupported,
			joulie.GPUGovernorPresetLow:       amdUnsupported,
			joulie.GPUGovernorPresetPeak:      amdUnsupported,
		}},
	}
	for _, row := range rows {
		var wantPresets []string
		for _, preset := range joulie.GPUGovernorPresetValues {
			want := row.want[preset]
			if want.Result != joulie.GovernorResultUnsupported {
				wantPresets = append(wantPresets, preset)
			}
			got, err := PlanAMD(row.facts, preset, DefaultCeilingPctOfMax)
			if err != nil {
				t.Fatalf("%s %s: %v", row.name, preset, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s:\n got %+v\nwant %+v", row.name, preset, got, want)
			}
			requireWritesExist(t, got)
		}
		if got := presetsAMD(row.facts); !slices.Equal(got, wantPresets) {
			t.Errorf("%s: presetsAMD = %v, want %v", row.name, got, wantPresets)
		}
	}

	f := amdFacts(AMDFamilyAldebaran, true)
	for pct, mhz := range map[float64]int{100: 1500, 75: 1125, 66.7: 1000, 10: 500} {
		got, err := PlanAMD(f, joulie.GPUGovernorPresetEfficient, pct)
		if err != nil {
			t.Fatal(err)
		}
		if want := odPlan(mhz); !reflect.DeepEqual(got, want) {
			t.Errorf("efficient at %v%%: got %+v, want sclk_max=%d (rounded down, never below the lowest sclk)", pct, got, mhz)
		}
	}
	f.BusID = "0000:C1:00.0"
	if got, _ := PlanAMD(f, joulie.GPUGovernorPresetAuto, 0); !reflect.DeepEqual(got, levelPlan("auto")) {
		t.Errorf("upper-case bus id: got %+v, want the lower-case sysfs path", got)
	}
}

// GA07: MI300X has no sustained, low or peak: unsupported, nothing written.
func TestGA07MI300XRejectsSustainedLowPeak(t *testing.T) {
	f := amdFacts(AMDFamilyMI300, true)
	for _, preset := range []string{joulie.GPUGovernorPresetSustained, joulie.GPUGovernorPresetLow, joulie.GPUGovernorPresetPeak} {
		got, err := PlanAMD(f, preset, DefaultCeilingPctOfMax)
		if err != nil {
			t.Fatal(err)
		}
		if got.Result != joulie.GovernorResultUnsupported || got.Writes != nil || got.Args != nil {
			t.Errorf("MI300X %s = %+v, want unsupported with no writes", preset, got)
		}
	}
	if got := presetsAMD(f); !slices.Equal(got, []string{joulie.GPUGovernorPresetAuto, joulie.GPUGovernorPresetEfficient}) {
		t.Errorf("presetsAMD(MI300X) = %v, want auto and efficient", got)
	}
}

// GA08: W7900 efficient is unsupported, even with overdrive enabled.
func TestGA08W7900EfficientIsUnsupported(t *testing.T) {
	for _, od := range []bool{false, true} {
		got, err := PlanAMD(amdFacts(AMDFamilyNavi31, od), joulie.GPUGovernorPresetEfficient, DefaultCeilingPctOfMax)
		if err != nil {
			t.Fatal(err)
		}
		if got.Result != joulie.GovernorResultUnsupported || got.Writes != nil {
			t.Errorf("W7900 efficient (overdrive %v) = %+v, want unsupported with no writes", od, got)
		}
	}
}
