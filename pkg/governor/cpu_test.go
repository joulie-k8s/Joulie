package governor

import (
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

// sysfsTree is a synthetic host /sys holding every attribute a plan in these
// tests writes: the union of the CPU and GPU rows, not one real node. A
// planned path missing from it is misspelt.
const sysfsTree = "testdata/sysfs"

func requireWritesExist(t *testing.T, p Plan) {
	t.Helper()
	for _, w := range p.Writes {
		if _, err := os.Stat(filepath.Join(sysfsTree, filepath.FromSlash(w.RelPath))); err != nil {
			t.Errorf("planned write to %s, which the synthetic sysfs tree does not hold: %v", w.RelPath, err)
		}
	}
}

var (
	genericGovernors = []string{"conservative", "ondemand", "userspace", "powersave", "performance", "schedutil"}
	activeGovernors  = []string{"performance", "powersave"}
	allEPPs          = []string{"default", "performance", "balance_performance", "balance_power", "power"}
)

// cpuRow is one row of the CPU mapping table (design section 1.4): a node
// with two policies, the native state each preset must map to, and the
// boost writes per boost word (nil when the node has no boost file).
type cpuRow struct {
	name  string
	facts CPUFacts
	want  map[string]cpuChoice
	boost map[string][]Write
}

func cpuRows() []cpuRow {
	facts := func(driver, mode string, hwp bool, govs, epps []string, scope string) CPUFacts {
		// Policies out of order on purpose: plans sort them.
		return CPUFacts{Driver: driver, DriverMode: mode, HWP: hwp, Governors: govs, EPPs: epps, BoostScope: scope, Policies: []int{1, 0}}
	}
	ok := func(gov, epp string) cpuChoice { return cpuChoice{gov, epp, joulie.GovernorResultApplied} }
	eq := func(gov, epp string) cpuChoice { return cpuChoice{gov, epp, joulie.GovernorResultEquivalent} }
	presets := func(perf, bal, eff cpuChoice) map[string]cpuChoice {
		return map[string]cpuChoice{
			joulie.CPUGovernorPresetPerformance: perf,
			joulie.CPUGovernorPresetBalanced:    bal,
			joulie.CPUGovernorPresetEfficiency:  eff,
		}
	}
	generic := presets(ok("performance", ""), ok("schedutil", ""), ok("conservative", ""))
	none := cpuChoice{result: joulie.GovernorResultUnsupported}
	global := map[string][]Write{
		"on":  {{"devices/system/cpu/cpufreq/boost", "1"}},
		"off": {{"devices/system/cpu/cpufreq/boost", "0"}},
	}
	noTurbo := map[string][]Write{
		"on":  {{"devices/system/cpu/intel_pstate/no_turbo", "0"}},
		"off": {{"devices/system/cpu/intel_pstate/no_turbo", "1"}},
	}
	perCPU := map[string][]Write{
		"on":  {{"devices/system/cpu/cpufreq/policy0/boost", "1"}, {"devices/system/cpu/cpufreq/policy1/boost", "1"}},
		"off": {{"devices/system/cpu/cpufreq/policy0/boost", "0"}, {"devices/system/cpu/cpufreq/policy1/boost", "0"}},
	}
	return []cpuRow{
		{"acpi-cpufreq", facts("acpi-cpufreq", "", false, genericGovernors, nil, joulie.CPUBoostScopeGlobal), generic, global},
		{"intel_cpufreq without HWP", facts("intel_cpufreq", joulie.CPUDriverModePassive, false, genericGovernors, nil, joulie.CPUBoostScopeNoTurbo), generic, noTurbo},
		{"intel_cpufreq with HWP", facts("intel_cpufreq", joulie.CPUDriverModePassive, true, genericGovernors, allEPPs, joulie.CPUBoostScopeNoTurbo),
			presets(ok("performance", ""), ok("schedutil", "balance_performance"), ok("conservative", "balance_power")), noTurbo},
		{"intel_pstate active with HWP", facts("intel_pstate", joulie.CPUDriverModeActive, true, activeGovernors, allEPPs, joulie.CPUBoostScopeNoTurbo),
			presets(ok("performance", ""), ok("powersave", "balance_performance"), ok("powersave", "balance_power")), noTurbo},
		{"intel_pstate active without HWP", facts("intel_pstate", joulie.CPUDriverModeActive, false, activeGovernors, nil, joulie.CPUBoostScopeNoTurbo),
			presets(ok("performance", ""), ok("powersave", ""), eq("powersave", "")), noTurbo},
		{"amd-pstate-epp", facts("amd-pstate-epp", joulie.CPUDriverModeActive, false, activeGovernors, allEPPs, joulie.CPUBoostScopePerCPU),
			presets(ok("performance", ""), ok("powersave", "balance_performance"), ok("powersave", "balance_power")), perCPU},
		{"amd-pstate passive", facts("amd-pstate", joulie.CPUDriverModePassive, false, genericGovernors, nil, joulie.CPUBoostScopePerCPU), generic, perCPU},
		{"amd-pstate guided", facts("amd-pstate", joulie.CPUDriverModeGuided, false, genericGovernors, nil, joulie.CPUBoostScopePerCPU), generic, perCPU},
		{"cppc_cpufreq with a boost file", facts("cppc_cpufreq", "", false, genericGovernors, nil, joulie.CPUBoostScopeGlobal), generic, global},
		{"cppc_cpufreq without a boost file", facts("cppc_cpufreq", "", false, genericGovernors, nil, ""), generic, nil},
		{"no cpufreq (VM)", CPUFacts{}, presets(none, none, none), nil},
	}
}

// wantCPUPlan is the plan a row's expectation spells: the governor on
// policy0 and policy1, then the preference on both, then the boost writes.
func wantCPUPlan(row cpuRow, preset, boost string) Plan {
	c := row.want[preset]
	if c.result == joulie.GovernorResultUnsupported || (boost != "" && row.boost == nil) {
		return Plan{Result: joulie.GovernorResultUnsupported}
	}
	p := Plan{Native: c.governor, Result: c.result}
	for _, n := range []string{"0", "1"} {
		p.Writes = append(p.Writes, Write{"devices/system/cpu/cpufreq/policy" + n + "/scaling_governor", c.governor})
	}
	if c.epp != "" {
		for _, n := range []string{"0", "1"} {
			p.Writes = append(p.Writes, Write{"devices/system/cpu/cpufreq/policy" + n + "/energy_performance_preference", c.epp})
		}
		p.Native += " epp=" + c.epp
	}
	if boost != "" {
		bw := row.boost[boost]
		p.Writes = append(p.Writes, bw...)
		p.Native += " " + path.Base(bw[0].RelPath) + "=" + bw[0].Value
	}
	return p
}

// GA01: every row of the CPU table plans its native words, with and without
// a boost word, to files the kernel spells that way; PresetsCPU lists the
// presets the row expresses.
func TestGA01EveryCPUTableRowPlansItsNativeWords(t *testing.T) {
	for _, row := range cpuRows() {
		var wantPresets []string
		for _, preset := range joulie.CPUGovernorPresetValues {
			if row.want[preset].result != joulie.GovernorResultUnsupported {
				wantPresets = append(wantPresets, preset)
			}
			for _, boost := range []string{"", joulie.CPUGovernorBoostOn, joulie.CPUGovernorBoostOff} {
				got, err := PlanCPU(row.facts, preset, boost)
				if err != nil {
					t.Fatalf("%s %s boost=%q: %v", row.name, preset, boost, err)
				}
				if want := wantCPUPlan(row, preset, boost); !reflect.DeepEqual(got, want) {
					t.Errorf("%s %s boost=%q:\n got %+v\nwant %+v", row.name, preset, boost, got, want)
				}
				requireWritesExist(t, got)
			}
		}
		if got := PresetsCPU(row.facts); !slices.Equal(got, wantPresets) {
			t.Errorf("%s: PresetsCPU = %v, want %v", row.name, got, wantPresets)
		}
	}
}

// GA02: efficiency without conservative falls back to schedutil and is
// equivalent; balanced without schedutil falls back to ondemand the same
// way; with no word of the chain left the preset is unsupported.
func TestGA02FallbackWordsAreEquivalent(t *testing.T) {
	f := CPUFacts{Driver: "acpi-cpufreq", Governors: []string{"performance", "powersave", "schedutil"}, Policies: []int{0}}
	got, err := PlanCPU(f, joulie.CPUGovernorPresetEfficiency, "")
	if err != nil {
		t.Fatal(err)
	}
	want := Plan{
		Writes: []Write{{"devices/system/cpu/cpufreq/policy0/scaling_governor", "schedutil"}},
		Native: "schedutil", Result: joulie.GovernorResultEquivalent,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("efficiency without conservative:\n got %+v\nwant %+v", got, want)
	}
	if got, _ := PlanCPU(f, joulie.CPUGovernorPresetBalanced, ""); got.Result != joulie.GovernorResultApplied || got.Native != "schedutil" {
		t.Errorf("balanced with schedutil = %s %q, want applied schedutil", got.Result, got.Native)
	}

	f.Governors = []string{"performance", "powersave", "ondemand"}
	if got, _ := PlanCPU(f, joulie.CPUGovernorPresetBalanced, ""); got.Result != joulie.GovernorResultEquivalent || got.Native != "ondemand" {
		t.Errorf("balanced without schedutil = %s %q, want equivalent ondemand", got.Result, got.Native)
	}
	got, _ = PlanCPU(f, joulie.CPUGovernorPresetEfficiency, "")
	if got.Result != joulie.GovernorResultUnsupported || got.Writes != nil {
		t.Errorf("efficiency with neither conservative nor schedutil = %+v, want unsupported with no writes", got)
	}
	if got := PresetsCPU(f); !slices.Equal(got, []string{joulie.CPUGovernorPresetPerformance, joulie.CPUGovernorPresetBalanced}) {
		t.Errorf("PresetsCPU = %v, want performance and balanced", got)
	}
}

// GA03: under performance no energy-performance preference is written, on
// every row, with or without boost: the kernel rejects a non-zero one.
func TestGA03PerformancePlansNoEPPWrite(t *testing.T) {
	for _, row := range cpuRows() {
		for _, boost := range []string{"", joulie.CPUGovernorBoostOn, joulie.CPUGovernorBoostOff} {
			p, err := PlanCPU(row.facts, joulie.CPUGovernorPresetPerformance, boost)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range p.Writes {
				if strings.HasSuffix(w.RelPath, "/energy_performance_preference") {
					t.Errorf("%s boost=%q: performance writes %s=%s", row.name, boost, w.RelPath, w.Value)
				}
			}
			if strings.Contains(p.Native, "epp=") {
				t.Errorf("%s boost=%q: performance native %q names a preference", row.name, boost, p.Native)
			}
		}
	}
}

// writeKind ranks a write: 0 governor, 1 preference, 2 boost.
func writeKind(t *testing.T, w Write) int {
	t.Helper()
	switch path.Base(w.RelPath) {
	case "scaling_governor":
		return 0
	case "energy_performance_preference":
		return 1
	case "boost", "no_turbo":
		return 2
	}
	t.Fatalf("unexpected write %s", w.RelPath)
	return -1
}

// GA04: writes are ordered governor, then preference, then boost, on every
// policy.
func TestGA04WritesAreOrderedGovernorEPPBoost(t *testing.T) {
	for _, row := range cpuRows() {
		if row.boost == nil || len(row.facts.EPPs) == 0 {
			continue
		}
		for _, preset := range []string{joulie.CPUGovernorPresetBalanced, joulie.CPUGovernorPresetEfficiency} {
			for _, boost := range []string{joulie.CPUGovernorBoostOn, joulie.CPUGovernorBoostOff} {
				p, err := PlanCPU(row.facts, preset, boost)
				if err != nil {
					t.Fatal(err)
				}
				var kinds []int
				for _, w := range p.Writes {
					kinds = append(kinds, writeKind(t, w))
				}
				if !slices.IsSorted(kinds) || !slices.Contains(kinds, 0) || !slices.Contains(kinds, 1) || !slices.Contains(kinds, 2) {
					t.Errorf("%s %s boost=%s: write kinds %v, want every governor, then every preference, then boost", row.name, preset, boost, kinds)
				}
			}
		}
	}
}

// GA05: on intel_pstate boost is the inverted no_turbo: on writes 0, off
// writes 1, and no boost file is touched.
func TestGA05IntelPstateBoostWritesInvertedNoTurbo(t *testing.T) {
	for _, driver := range []string{"intel_pstate", "intel_cpufreq"} {
		f := CPUFacts{Driver: driver, HWP: true, Governors: []string{"performance", "powersave", "schedutil", "conservative"}, EPPs: allEPPs,
			BoostScope: joulie.CPUBoostScopeNoTurbo, Policies: []int{0, 1}}
		for boost, want := range map[string]string{joulie.CPUGovernorBoostOn: "0", joulie.CPUGovernorBoostOff: "1"} {
			p, err := PlanCPU(f, joulie.CPUGovernorPresetPerformance, boost)
			if err != nil {
				t.Fatal(err)
			}
			last := p.Writes[len(p.Writes)-1]
			if last != (Write{"devices/system/cpu/intel_pstate/no_turbo", want}) {
				t.Errorf("%s boost=%s: last write %+v, want no_turbo=%s", driver, boost, last, want)
			}
			for _, w := range p.Writes {
				if path.Base(w.RelPath) == "boost" {
					t.Errorf("%s boost=%s: writes %s", driver, boost, w.RelPath)
				}
			}
			if !strings.HasSuffix(p.Native, " no_turbo="+want) {
				t.Errorf("%s boost=%s: native %q, want the no_turbo=%s suffix", driver, boost, p.Native, want)
			}
		}
	}
}

// GA12: ReverseCPU inverts PlanCPU on every row where the native state
// (governor and preference) belongs to one preset only, and reports no
// preset where two share it or none maps to it.
func TestGA12ReverseCPUInvertsPlanCPU(t *testing.T) {
	type state struct{ governor, epp string }
	for _, row := range cpuRows() {
		owners := map[state][]string{}
		for _, preset := range joulie.CPUGovernorPresetValues {
			p, err := PlanCPU(row.facts, preset, "")
			if err != nil {
				t.Fatal(err)
			}
			if p.Result == joulie.GovernorResultUnsupported {
				continue
			}
			var s state
			for _, w := range p.Writes {
				switch w.RelPath {
				case "devices/system/cpu/cpufreq/policy0/scaling_governor":
					s.governor = w.Value
				case "devices/system/cpu/cpufreq/policy0/energy_performance_preference":
					s.epp = w.Value
				}
			}
			owners[s] = append(owners[s], preset)
		}
		for s, presets := range owners {
			got, ok := ReverseCPU(row.facts, s.governor, s.epp)
			if len(presets) == 1 && (got != presets[0] || !ok) {
				t.Errorf("%s: ReverseCPU(%q, %q) = %q, %v, want %q, true", row.name, s.governor, s.epp, got, ok, presets[0])
			}
			if len(presets) > 1 && (got != "" || ok) {
				t.Errorf("%s: ReverseCPU(%q, %q) = %q, %v, want no preset: %v share it", row.name, s.governor, s.epp, got, ok, presets)
			}
		}
		if got, ok := ReverseCPU(row.facts, "userspace", ""); ok {
			t.Errorf("%s: ReverseCPU(userspace) = %q, want no preset", row.name, got)
		}
		if len(row.facts.EPPs) > 0 {
			// Under performance the kernel sets the preference itself.
			if got, ok := ReverseCPU(row.facts, "performance", "performance"); got != joulie.CPUGovernorPresetPerformance || !ok {
				t.Errorf("%s: ReverseCPU(performance, performance) = %q, %v, want performance", row.name, got, ok)
			}
			if got, ok := ReverseCPU(row.facts, row.want[joulie.CPUGovernorPresetBalanced].governor, "power"); ok {
				t.Errorf("%s: a preference no preset sets reverses to %q", row.name, got)
			}
		}
	}
}

// Unknown words are errors, never a plan.
func TestPlansRejectUnknownWords(t *testing.T) {
	f := cpuRows()[0].facts
	if _, err := PlanCPU(f, "powersave", ""); err == nil {
		t.Error("PlanCPU accepted the native word powersave as a preset")
	}
	if _, err := PlanCPU(f, joulie.CPUGovernorPresetBalanced, "1"); err == nil {
		t.Error("PlanCPU accepted boost word 1")
	}
	amd := AMDFacts{BusID: "0000:c1:00.0", Family: AMDFamilyAldebaran, OD: true, SclkMHz: [2]int{500, 1500}}
	nv := NVFacts{Arch: NVArchAda, SupportedSMMHz: []int{300, 1500}}
	for _, pct := range []float64{0, -5, 100.5} {
		if _, err := PlanAMD(amd, joulie.GPUGovernorPresetEfficient, pct); err == nil {
			t.Errorf("PlanAMD efficient accepted ceiling %v", pct)
		}
		if _, err := PlanNvidia(nv, "00000000:18:00.0", joulie.GPUGovernorPresetEfficient, pct); err == nil {
			t.Errorf("PlanNvidia efficient accepted ceiling %v", pct)
		}
		if _, err := PlanAMD(amd, joulie.GPUGovernorPresetAuto, pct); err != nil {
			t.Errorf("PlanAMD auto rejected ceiling %v, which it ignores: %v", pct, err)
		}
	}
	if _, err := PlanAMD(amd, "high", 0); err == nil {
		t.Error("PlanAMD accepted the native level high as a preset")
	}
	if _, err := PlanNvidia(nv, "00000000:18:00.0", "unlocked", 0); err == nil {
		t.Error("PlanNvidia accepted unlocked as a preset")
	}
	if _, err := PlanAMD(AMDFacts{Family: AMDFamilyAldebaran}, joulie.GPUGovernorPresetAuto, 0); err == nil {
		t.Error("PlanAMD planned a device without a bus id")
	}
	if _, err := PlanNvidia(nv, "", joulie.GPUGovernorPresetAuto, 0); err == nil {
		t.Error("PlanNvidia planned without a bus id: nvidia-smi without -i acts on every GPU")
	}
}
