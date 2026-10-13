package governor

import (
	"reflect"
	"slices"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

const nvBus = "00000000:18:00.0"

// Neutral supported clocks, out of order as nvidia-smi may list them.
var (
	nvSM  = []int{1500, 1400, 1200, 1100, 900, 600, 300, 1500}
	nvMem = []int{1400, 400, 900}
)

func gddr(arch string) NVFacts {
	return NVFacts{Arch: arch, SupportedSMMHz: nvSM, SupportedMemMHz: nvMem, MemLockRuntime: true}
}

func hbm(arch string) NVFacts {
	return NVFacts{Arch: arch, SupportedSMMHz: nvSM, SupportedMemMHz: []int{1400}}
}

func smi(a ...string) []string { return append([]string{"-i", nvBus}, a...) }

// The rows of the NVIDIA table (design section 1.5) at the default ceiling,
// without and with locks in the agent's record.
func TestNvidiaTableRows(t *testing.T) {
	applied, equivalent := joulie.GovernorResultApplied, joulie.GovernorResultEquivalent
	unsupported := Plan{Result: joulie.GovernorResultUnsupported}
	locked := func(f NVFacts) NVFacts { f.LockedGPU, f.LockedMem = true, true; return f }
	rows := []struct {
		name  string
		facts NVFacts
		want  map[string]Plan
	}{
		{"GDDR (ada), no record", gddr(NVArchAda), map[string]Plan{
			joulie.GPUGovernorPresetAuto:      {Native: "unknown", Result: equivalent},
			joulie.GPUGovernorPresetSustained: {Args: [][]string{smi("-lmc", "1400,1400")}, Native: "-lmc 1400,1400", Result: applied},
			joulie.GPUGovernorPresetEfficient: {Args: [][]string{smi("-lgc", "300,1200")}, Native: "-lgc 300,1200", Result: applied},
			joulie.GPUGovernorPresetLow:       {Args: [][]string{smi("-lgc", "300,300"), smi("-lmc", "400,400")}, Native: "-lgc 300,300 -lmc 400,400", Result: applied},
			joulie.GPUGovernorPresetPeak:      {Args: [][]string{smi("-lgc", "1500,1500"), smi("-lmc", "1400,1400")}, Native: "-lgc 1500,1500 -lmc 1400,1400", Result: applied},
		}},
		{"GDDR (ampere), both locks in the record", locked(gddr(NVArchAmpere)), map[string]Plan{
			joulie.GPUGovernorPresetAuto:      {Args: [][]string{smi("-rgc"), smi("-rmc")}, Native: "unlocked", Result: applied},
			joulie.GPUGovernorPresetSustained: {Args: [][]string{smi("-rgc"), smi("-lmc", "1400,1400")}, Native: "-lmc 1400,1400", Result: applied},
			joulie.GPUGovernorPresetEfficient: {Args: [][]string{smi("-lgc", "300,1200"), smi("-rmc")}, Native: "-lgc 300,1200", Result: applied},
			joulie.GPUGovernorPresetLow:       {Args: [][]string{smi("-lgc", "300,300"), smi("-lmc", "400,400")}, Native: "-lgc 300,300 -lmc 400,400", Result: applied},
			joulie.GPUGovernorPresetPeak:      {Args: [][]string{smi("-lgc", "1500,1500"), smi("-lmc", "1400,1400")}, Native: "-lgc 1500,1500 -lmc 1400,1400", Result: applied},
		}},
		{"Hopper, no record", hbm(NVArchHopper), map[string]Plan{
			joulie.GPUGovernorPresetAuto:      {Native: "unknown", Result: equivalent},
			joulie.GPUGovernorPresetSustained: {Native: "unknown", Result: equivalent},
			joulie.GPUGovernorPresetEfficient: {Args: [][]string{smi("-lgc", "300,1200")}, Native: "-lgc 300,1200", Result: applied},
			joulie.GPUGovernorPresetLow:       {Args: [][]string{smi("-lgc", "300,300")}, Native: "-lgc 300,300", Result: applied},
			joulie.GPUGovernorPresetPeak:      {Args: [][]string{smi("-lgc", "1500,1500")}, Native: "-lgc 1500,1500", Result: applied},
		}},
		{"Hopper, graphics lock in the record (and a stray memory lock)", locked(hbm(NVArchHopper)), map[string]Plan{
			joulie.GPUGovernorPresetAuto:      {Args: [][]string{smi("-rgc")}, Native: "unlocked", Result: applied},
			joulie.GPUGovernorPresetSustained: {Args: [][]string{smi("-rgc")}, Native: "unlocked", Result: equivalent},
			joulie.GPUGovernorPresetEfficient: {Args: [][]string{smi("-lgc", "300,1200")}, Native: "-lgc 300,1200", Result: applied},
			joulie.GPUGovernorPresetLow:       {Args: [][]string{smi("-lgc", "300,300")}, Native: "-lgc 300,300", Result: applied},
			joulie.GPUGovernorPresetPeak:      {Args: [][]string{smi("-lgc", "1500,1500")}, Native: "-lgc 1500,1500", Result: applied},
		}},
		{"Blackwell, as Hopper even with a runtime memory lock", gddr(NVArchBlackwell), map[string]Plan{
			joulie.GPUGovernorPresetAuto:      {Native: "unknown", Result: equivalent},
			joulie.GPUGovernorPresetSustained: {Native: "unknown", Result: equivalent},
			joulie.GPUGovernorPresetEfficient: {Args: [][]string{smi("-lgc", "300,1200")}, Native: "-lgc 300,1200", Result: applied},
			joulie.GPUGovernorPresetLow:       {Args: [][]string{smi("-lgc", "300,300")}, Native: "-lgc 300,300", Result: applied},
			joulie.GPUGovernorPresetPeak:      {Args: [][]string{smi("-lgc", "1500,1500")}, Native: "-lgc 1500,1500", Result: applied},
		}},
		{"Ampere HBM, one memory clock", NVFacts{Arch: NVArchAmpere, SupportedSMMHz: nvSM, SupportedMemMHz: []int{1400}, MemLockRuntime: true}, map[string]Plan{
			joulie.GPUGovernorPresetAuto:      {Native: "unknown", Result: equivalent},
			joulie.GPUGovernorPresetSustained: {Native: "unknown", Result: equivalent},
			joulie.GPUGovernorPresetEfficient: {Args: [][]string{smi("-lgc", "300,1200")}, Native: "-lgc 300,1200", Result: applied},
			joulie.GPUGovernorPresetLow:       {Args: [][]string{smi("-lgc", "300,300")}, Native: "-lgc 300,300", Result: applied},
			joulie.GPUGovernorPresetPeak:      {Args: [][]string{smi("-lgc", "1500,1500")}, Native: "-lgc 1500,1500", Result: applied},
		}},
		{"GDDR without a supported clocks table", NVFacts{Arch: NVArchAda, MemLockRuntime: true}, map[string]Plan{
			joulie.GPUGovernorPresetAuto:      {Native: "unknown", Result: equivalent},
			joulie.GPUGovernorPresetSustained: unsupported,
			joulie.GPUGovernorPresetEfficient: unsupported,
			joulie.GPUGovernorPresetLow:       unsupported,
			joulie.GPUGovernorPresetPeak:      unsupported,
		}},
		{"unknown architecture", NVFacts{Arch: "turing", SupportedSMMHz: nvSM, SupportedMemMHz: nvMem, MemLockRuntime: true}, map[string]Plan{
			joulie.GPUGovernorPresetAuto:      unsupported,
			joulie.GPUGovernorPresetSustained: unsupported,
			joulie.GPUGovernorPresetEfficient: unsupported,
			joulie.GPUGovernorPresetLow:       unsupported,
			joulie.GPUGovernorPresetPeak:      unsupported,
		}},
	}
	for _, row := range rows {
		var wantPresets []string
		for _, preset := range joulie.GPUGovernorPresetValues {
			want := row.want[preset]
			if want.Result != joulie.GovernorResultUnsupported {
				wantPresets = append(wantPresets, preset)
			}
			got, err := PlanNvidia(row.facts, nvBus, preset, DefaultCeilingPctOfMax)
			if err != nil {
				t.Fatalf("%s %s: %v", row.name, preset, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s:\n got %+v\nwant %+v", row.name, preset, got, want)
			}
		}
		if got := PresetsNvidia(row.facts); !slices.Equal(got, wantPresets) {
			t.Errorf("%s: PresetsNvidia = %v, want %v", row.name, got, wantPresets)
		}
	}
}

// GA09: Hopper sustained is equivalent and plans no -lmc, with or without
// locks in the agent's record.
func TestGA09HopperSustainedIsEquivalentWithoutLmc(t *testing.T) {
	for _, f := range []NVFacts{hbm(NVArchHopper), {Arch: NVArchHopper, SupportedSMMHz: nvSM, SupportedMemMHz: nvMem, LockedGPU: true, LockedMem: true}} {
		got, err := PlanNvidia(f, nvBus, joulie.GPUGovernorPresetSustained, DefaultCeilingPctOfMax)
		if err != nil {
			t.Fatal(err)
		}
		if got.Result != joulie.GovernorResultEquivalent {
			t.Errorf("Hopper sustained (record %v/%v) result %q, want equivalent", f.LockedGPU, f.LockedMem, got.Result)
		}
		for _, a := range got.Args {
			if slices.Contains(a, "-lmc") || slices.Contains(a, "-rmc") {
				t.Errorf("Hopper sustained (record %v/%v) plans %v", f.LockedGPU, f.LockedMem, a)
			}
		}
	}
}

// GA10: efficient locks the graphics clock from the lowest supported clock
// to the highest supported one at or below the ceiling times the maximum.
func TestGA10NvidiaEfficientLocksToTheSupportedClockBelowTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		pct  float64
		want string
	}{
		{75, "300,1100"}, // 0.75 x 1500 = 1125, not supported: 1100
		{80, "300,1200"}, // 1200 is supported: at or below includes it
		{100, "300,1500"},
		{10, "300,300"}, // nothing at or below 150: the lowest clock
	} {
		got, err := PlanNvidia(hbm(NVArchHopper), nvBus, joulie.GPUGovernorPresetEfficient, tc.pct)
		if err != nil {
			t.Fatal(err)
		}
		want := Plan{Args: [][]string{smi("-lgc", tc.want)}, Native: "-lgc " + tc.want, Result: joulie.GovernorResultApplied}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("efficient at %v%%:\n got %+v\nwant %+v", tc.pct, got, want)
		}
	}
}
