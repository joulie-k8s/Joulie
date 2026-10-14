package governor

import (
	"reflect"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

// gpuCeilings are the ceilings the reverse tests plan the efficient preset
// at: the default, both ends of (0, 100] and points between.
var gpuCeilings = []float64{1, 10, 50, 66.7, 75, DefaultCeilingPctOfMax, 99, 100}

// ReverseNvidia names, for the locks of every plan PlanNvidia makes at any
// ceiling, a preset whose plan sets those locks, and the planned preset
// itself wherever that preset sets them as its own.
func TestReverseNvidiaInvertsPlanNvidia(t *testing.T) {
	for name, f := range map[string]NVFacts{
		"ada":       gddr(NVArchAda),
		"ampere":    gddr(NVArchAmpere),
		"hopper":    hbm(NVArchHopper),
		"blackwell": gddr(NVArchBlackwell),
	} {
		for _, preset := range joulie.GPUGovernorPresetValues {
			for _, pct := range gpuCeilings {
				p, err := PlanNvidia(f, nvBus, preset, pct)
				if err != nil {
					t.Fatal(err)
				}
				if p.Result == joulie.GovernorResultUnsupported {
					continue
				}
				got, ok := ReverseNvidia(f, p.GPULockMHz, p.MemLockMHz)
				if !ok {
					t.Errorf("%s: %s at %v%% plans %q, which reverses to no preset", name, preset, pct, p.Native)
					continue
				}
				q, err := PlanNvidia(f, nvBus, got, pct)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(q.State, p.State) {
					t.Errorf("%s: %q reverses to %s, whose plan at %v%% sets %q", name, p.Native, got, pct, q.Native)
				}
			}
		}
	}
	lock := func(lo, hi int) *[2]int { return &[2]int{lo, hi} }
	for _, tc := range []struct {
		name     string
		f        NVFacts
		gpu, mem *[2]int
		want     string
	}{
		{"no lock is auto", gddr(NVArchAda), nil, nil, joulie.GPUGovernorPresetAuto},
		{"no lock is auto where sustained changes nothing", hbm(NVArchHopper), nil, nil, joulie.GPUGovernorPresetAuto},
		{"efficient at 100 %", gddr(NVArchAda), lock(300, 1500), nil, joulie.GPUGovernorPresetEfficient},
		{"efficient at the default ceiling", gddr(NVArchAda), lock(300, 1200), nil, joulie.GPUGovernorPresetEfficient},
		{"the lowest clock alone is low where memory has no lock", hbm(NVArchHopper), lock(300, 300), nil, joulie.GPUGovernorPresetLow},
		{"memory at its top is sustained", gddr(NVArchAda), nil, lock(1400, 1400), joulie.GPUGovernorPresetSustained},
		{"a floor above the lowest clock is no preset", gddr(NVArchAda), lock(600, 1200), nil, ""},
		{"a ceiling that is not a supported clock is no preset", gddr(NVArchAda), lock(300, 1250), nil, ""},
		{"a graphics lock with a memory lock is no efficient", gddr(NVArchAda), lock(300, 1200), lock(400, 400), ""},
		{"an architecture without a table", NVFacts{Arch: "turing", SupportedSMMHz: nvSM}, nil, nil, ""},
	} {
		got, ok := ReverseNvidia(tc.f, tc.gpu, tc.mem)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: ReverseNvidia = %q, %v, want %q", tc.name, got, ok, tc.want)
		}
	}
}

// ReverseAMD names the preset of every plan PlanAMD makes at any ceiling,
// given the level and the overdrive range the plan leaves.
func TestReverseAMDInvertsPlanAMD(t *testing.T) {
	for _, family := range []string{AMDFamilyNavi31, AMDFamilyAldebaran, AMDFamilyMI300} {
		for _, od := range []bool{false, true} {
			f := amdFacts(family, od)
			for _, preset := range joulie.GPUGovernorPresetValues {
				for _, pct := range gpuCeilings {
					p, err := PlanAMD(f, preset, pct)
					if err != nil {
						t.Fatal(err)
					}
					if p.Result == joulie.GovernorResultUnsupported {
						continue
					}
					odRange := f.SclkMHz
					if p.SclkMaxMHz > 0 {
						odRange[1] = p.SclkMaxMHz
					}
					if got, ok := ReverseAMD(f, p.Level, odRange); got != preset || !ok {
						t.Errorf("%s (overdrive %v): %s at %v%% plans %q, which reverses to %q, %v", family, od, preset, pct, p.Native, got, ok)
					}
				}
			}
		}
	}
	mi300 := amdFacts(AMDFamilyMI300, true)
	for _, tc := range []struct {
		name  string
		f     AMDFacts
		level string
		od    [2]int
		want  string
	}{
		{"manual over the whole range is efficient at 100 %", mi300, "manual", [2]int{500, 1500}, joulie.GPUGovernorPresetEfficient},
		{"a raised sclk minimum is no preset", mi300, "manual", [2]int{700, 1200}, ""},
		{"perf_determinism is no preset", mi300, "perf_determinism", [2]int{500, 1200}, ""},
		{"manual on a family without the overdrive ceiling", amdFacts(AMDFamilyNavi31, true), "manual", [2]int{500, 1200}, ""},
		{"high on MI300X", mi300, "high", [2]int{500, 1500}, ""},
		{"an unknown family", amdFacts("vega20", true), "auto", [2]int{500, 1500}, ""},
	} {
		got, ok := ReverseAMD(tc.f, tc.level, tc.od)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: ReverseAMD = %q, %v, want %q", tc.name, got, ok, tc.want)
		}
	}
}

// ParseNative reads back the State of every plan from its Native spelling,
// for every row of every table.
func TestParseNativeReadsBackThePlannedState(t *testing.T) {
	check := func(backend string, p Plan) {
		t.Helper()
		if p.Result == joulie.GovernorResultUnsupported {
			return
		}
		if got := ParseNative(backend, p.Native); !reflect.DeepEqual(got, p.State) {
			t.Errorf("ParseNative(%s, %q) = %+v, want %+v", backend, p.Native, got, p.State)
		}
	}
	for _, row := range cpuRows() {
		for _, preset := range joulie.CPUGovernorPresetValues {
			for _, boost := range []string{"", joulie.CPUGovernorBoostOn, joulie.CPUGovernorBoostOff} {
				p, err := PlanCPU(row.facts, preset, boost)
				if err != nil {
					t.Fatal(err)
				}
				check(joulie.GovernorBackendCPUFreq, p)
			}
		}
	}
	for _, family := range []string{AMDFamilyNavi31, AMDFamilyAldebaran, AMDFamilyMI300} {
		for _, preset := range joulie.GPUGovernorPresetValues {
			p, err := PlanAMD(amdFacts(family, true), preset, 75)
			if err != nil {
				t.Fatal(err)
			}
			check(joulie.GovernorBackendAMDGPUSysfs, p)
		}
	}
	for _, f := range []NVFacts{gddr(NVArchAda), hbm(NVArchHopper), {Arch: NVArchAmpere, SupportedSMMHz: nvSM, SupportedMemMHz: nvMem, MemLockRuntime: true, LockedGPU: true, LockedMem: true}} {
		for _, preset := range joulie.GPUGovernorPresetValues {
			p, err := PlanNvidia(f, nvBus, preset, 75)
			if err != nil {
				t.Fatal(err)
			}
			check(joulie.GovernorBackendNvidiaSMI, p)
		}
	}
}

// PlanCPUGroups writes every governor, then every preference, then boost;
// boost is per policy on a per-policy node and one file elsewhere.
func TestPlanCPUGroups(t *testing.T) {
	groups := []CPUGroup{
		{Policies: []int{1, 0}, Governor: "schedutil", Boost: joulie.CPUGovernorBoostOn},
		{Policies: []int{2}, Governor: "powersave", EPP: "balance_power", Boost: joulie.CPUGovernorBoostOff},
	}
	pol := func(n, file, v string) Write { return Write{"devices/system/cpu/cpufreq/policy" + n + "/" + file, v} }
	for _, tc := range []struct {
		scope string
		boost []Write
	}{
		{joulie.CPUBoostScopeGlobal, []Write{{"devices/system/cpu/cpufreq/boost", "1"}}},
		{joulie.CPUBoostScopeNoTurbo, []Write{{"devices/system/cpu/intel_pstate/no_turbo", "0"}}},
		{joulie.CPUBoostScopePerCPU, []Write{pol("0", "boost", "1"), pol("1", "boost", "1"), pol("2", "boost", "0")}},
		{"", nil},
	} {
		govs, epps, boost := PlanCPUGroups(CPUFacts{BoostScope: tc.scope}, groups)
		if want := []Write{pol("0", "scaling_governor", "schedutil"), pol("1", "scaling_governor", "schedutil"), pol("2", "scaling_governor", "powersave")}; !reflect.DeepEqual(govs, want) {
			t.Errorf("scope %q: governors %v, want %v", tc.scope, govs, want)
		}
		if want := []Write{pol("2", "energy_performance_preference", "balance_power")}; !reflect.DeepEqual(epps, want) {
			t.Errorf("scope %q: preferences %v, want %v", tc.scope, epps, want)
		}
		if !reflect.DeepEqual(boost, tc.boost) {
			t.Errorf("scope %q: boost %v, want %v", tc.scope, boost, tc.boost)
		}
	}
}

// The restore writes of an amdgpu device go to the files PlanAMD writes.
func TestAMDRestoreWritesTargetThePlannedFiles(t *testing.T) {
	if got := PlanAMDLevel("0000:C1:00.0", AMDLevelAuto); got != (Write{amdDir + "power_dpm_force_performance_level", "auto"}) {
		t.Errorf("PlanAMDLevel = %+v", got)
	}
	reset := PlanAMDODReset("0000:c1:00.0")
	if want := []Write{{amdDir + "pp_od_clk_voltage", "r"}, {amdDir + "pp_od_clk_voltage", "c"}}; !reflect.DeepEqual(reset, want) {
		t.Errorf("PlanAMDODReset = %v, want %v", reset, want)
	}
	for _, w := range reset {
		if !IsODWrite(w) {
			t.Errorf("IsODWrite(%+v) = false", w)
		}
	}
	if IsODWrite(PlanAMDLevel("0000:c1:00.0", "manual")) {
		t.Error("IsODWrite took the level file for an overdrive table")
	}
	requireWritesExist(t, Plan{Writes: append(reset, PlanAMDLevel("0000:c1:00.0", "auto"))})
}
