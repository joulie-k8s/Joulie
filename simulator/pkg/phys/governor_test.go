package phys

import (
	"fmt"
	"testing"
)

// governorTargetKHz is the frequency the governor of in sets after one step
// with no previous target.
func governorTargetKHz(in GovernorInput) int64 {
	_, target := GovernorStep(in, 0)
	return target
}

// governorRequestKHz is the request the governor of in keeps after one step
// with no previous target.
func governorRequestKHz(in GovernorInput) int64 {
	request, _ := GovernorStep(in, 0)
	return request
}

// epycTable is a 3-entry acpi-cpufreq table under a boost cpuinfo_max_freq,
// in kHz: the shape of the hwemu profile amd-epyc-2s-energy-only.yaml.
var epycTable = []int64{2400000, 1900000, 1500000}

const epycMaxKHz = 3707000

func epycInput(governor string, util float64) GovernorInput {
	return GovernorInput{
		Governor: governor,
		Util:     util,
		MinKHz:   1500000,
		MaxKHz:   epycMaxKHz,
		TableKHz: epycTable,
		CurKHz:   epycMaxKHz,
	}
}

// continuousInput is a policy without a table from 1.0 to 3.0 GHz.
func continuousInput(governor string, util float64, curKHz int64) GovernorInput {
	return GovernorInput{
		Governor: governor,
		Util:     util,
		MinKHz:   1000000,
		MaxKHz:   3000000,
		CurKHz:   curKHz,
	}
}

func TestGP01SchedutilResolvesUpOnTheThreeEntryTable(t *testing.T) {
	for _, tc := range []struct {
		util float64
		want int64
	}{
		{0.30, 1500000}, // 1.25 x 3707 x 0.30 = 1390 MHz
		{0.35, 1900000}, // 1622 MHz
		{0.50, 2400000}, // 2317 MHz
		{0.00, 1500000},
		{1.00, 2400000},
	} {
		in := epycInput("schedutil", tc.util)
		if got := governorTargetKHz(in); got != tc.want {
			t.Errorf("util %.2f at the maximum: got %d kHz, want %d", tc.util, got, tc.want)
		}
		in.CurKHz = 0
		if got := governorTargetKHz(in); got != tc.want {
			t.Errorf("util %.2f before the first request: got %d kHz, want %d", tc.util, got, tc.want)
		}
	}

	// The same demand, 0.30 of cpuinfo_max_freq, measured at a lower
	// frequency shows as a busier CPU, and schedutil still requests 1500 MHz:
	// its target follows the demand, not the busy share.
	for _, ran := range []int64{1500000, 1900000, 2400000} {
		in := epycInput("schedutil", 0.30*epycMaxKHz/float64(ran))
		in.CurKHz = ran
		if got := governorTargetKHz(in); got != 1500000 {
			t.Errorf("demand 0.30 measured at %d kHz: got %d kHz, want 1500000", ran, got)
		}
		if got := governorRequestKHz(in); got != 1500000 {
			t.Errorf("schedutil request at %d kHz: got %d kHz, want its target 1500000", ran, got)
		}
	}

	// Util was measured at the frequency the policy ran at, also when the
	// limits moved since: half busy at 3.0 GHz is 1.5 GHz of work whatever
	// scaling_max_freq says now, so 1.25 x 1.5 GHz.
	limited := continuousInput("schedutil", 0.5, 3000000)
	limited.MaxKHz = 2000000
	if got := governorTargetKHz(limited); got != 1875000 {
		t.Errorf("scaling_max_freq lowered below the frequency the policy ran at: got %d kHz, want 1875000", got)
	}

	// Without a table the request is continuous, within the policy range.
	if got := governorTargetKHz(continuousInput("schedutil", 0.5, 3000000)); got != 1875000 {
		t.Errorf("continuous schedutil at util 0.5: got %d kHz, want 1875000", got)
	}
	if got := governorTargetKHz(continuousInput("schedutil", 0.1, 3000000)); got != 1000000 {
		t.Errorf("continuous schedutil below the minimum: got %d kHz, want 1000000", got)
	}
}

func TestGP02PerformanceHoldsTheScalingMaximum(t *testing.T) {
	for _, util := range []float64{0, 0.3, 1} {
		if got := governorTargetKHz(continuousInput("performance", util, 1000000)); got != 3000000 {
			t.Errorf("continuous, util %.1f: got %d kHz, want 3000000", util, got)
		}
		lowered := continuousInput("performance", util, 0)
		lowered.MaxKHz = 2200000
		if got := governorTargetKHz(lowered); got != 2200000 {
			t.Errorf("scaling_max_freq 2200000, util %.1f: got %d kHz, want 2200000", util, got)
		}
		// On a table the maximum is the highest entry at or below it.
		if got := governorTargetKHz(epycInput("performance", util)); got != 2400000 {
			t.Errorf("table, util %.1f: got %d kHz, want 2400000", util, got)
		}
		in := epycInput("performance", util)
		in.MaxKHz = 2000000
		if got := governorTargetKHz(in); got != 1900000 {
			t.Errorf("table with scaling_max_freq 2000000, util %.1f: got %d kHz, want 1900000", util, got)
		}
		// HWP performance ignores EPP.
		hwp := continuousInput("performance", util, 0)
		hwp.HWP, hwp.EPP = true, 255
		if got := governorTargetKHz(hwp); got != 3000000 {
			t.Errorf("HWP performance at EPP 255, util %.1f: got %d kHz, want 3000000", util, got)
		}
	}

	// A word Joulie does not model, and none, keep the performance target
	// the legacy engines imply (D9).
	for _, gov := range []string{"", "interactive"} {
		if got := governorTargetKHz(continuousInput(gov, 0.1, 1000000)); got != 3000000 {
			t.Errorf("governor %q: got %d kHz, want 3000000", gov, got)
		}
	}
	// A policy without frequencies gets none.
	if got := governorTargetKHz(GovernorInput{Governor: "performance"}); got != 0 {
		t.Errorf("no frequencies: got %d kHz, want 0", got)
	}
}

func TestGP03GenericPowersaveHoldsTheScalingMinimum(t *testing.T) {
	for _, util := range []float64{0, 0.5, 1} {
		if got := governorTargetKHz(continuousInput("powersave", util, 3000000)); got != 1000000 {
			t.Errorf("continuous, util %.1f: got %d kHz, want 1000000", util, got)
		}
		raised := continuousInput("powersave", util, 3000000)
		raised.MinKHz = 1400000
		if got := governorTargetKHz(raised); got != 1400000 {
			t.Errorf("scaling_min_freq 1400000, util %.1f: got %d kHz, want 1400000", util, got)
		}
		if got := governorTargetKHz(epycInput("powersave", util)); got != 1500000 {
			t.Errorf("table, util %.1f: got %d kHz, want 1500000", util, got)
		}
		in := epycInput("powersave", util)
		in.MinKHz = 1600000
		if got := governorTargetKHz(in); got != 1900000 {
			t.Errorf("table with scaling_min_freq 1600000, util %.1f: got %d kHz, want 1900000", util, got)
		}
	}
}

func TestGP04ConservativeStepsByFreqStepPerPeriodWithinTheRange(t *testing.T) {
	const samplingUS = 10000
	period := func(in GovernorInput, periods int) GovernorInput {
		in.Tunables.OndemandSamplingRateUS = samplingUS
		in.PeriodS = float64(periods) * samplingUS / 1e6
		return in
	}
	// freq_step 5 % of the 3.0 GHz maximum is 150 MHz.
	for _, tc := range []struct {
		name    string
		util    float64
		periods int
		want    int64
	}{
		{"busy, one period", 1, 1, 2150000},
		{"busy, three periods", 1, 3, 2450000},
		{"busy, more periods than the range", 1, 100, 3000000},
		{"idle, one period", 0, 1, 1850000},
		{"idle, three periods", 0, 3, 1550000},
		{"idle, more periods than the range", 0, 100, 1000000},
		{"load between the thresholds", 0.5, 10, 2000000},
		{"no PeriodS is one period", 1, 0, 2150000},
	} {
		in := period(continuousInput("conservative", tc.util, 2000000), tc.periods)
		if tc.periods == 0 {
			in.PeriodS = 0
		}
		if got := governorTargetKHz(in); got != tc.want {
			t.Errorf("%s: got %d kHz, want %d", tc.name, got, tc.want)
		}
	}

	// Within one call the load follows the frequency: 900 MHz of work at
	// 1.0 GHz is a load of 90, above up_threshold 80, and one step to
	// 1.15 GHz makes it 78, between the thresholds, where the governor stays.
	if got := governorTargetKHz(period(continuousInput("conservative", 0.9, 1000000), 50)); got != 1150000 {
		t.Errorf("settling: got %d kHz, want 1150000", got)
	}

	// A step below the table spacing: the request climbs 120 MHz (5 % of
	// the 2400 MHz policy maximum) per period while the policy holds an
	// entry, the highest at or below the request.
	in := period(epycInput("conservative", 1), 1)
	in.CurKHz = 1500000
	var targets, requests []int64
	for range 9 {
		requests = append(requests, governorRequestKHz(in))
		targets = append(targets, governorTargetKHz(in))
		in.CurKHz = requests[len(requests)-1]
	}
	wantRequests := []int64{1620000, 1740000, 1860000, 1980000, 2100000, 2220000, 2340000, 2400000, 2400000}
	wantTargets := []int64{1500000, 1500000, 1500000, 1900000, 1900000, 1900000, 1900000, 2400000, 2400000}
	if fmt.Sprint(requests) != fmt.Sprint(wantRequests) || fmt.Sprint(targets) != fmt.Sprint(wantTargets) {
		t.Errorf("table climb: requests %v targets %v, want %v and %v", requests, targets, wantRequests, wantTargets)
	}
	// Going down, the policy holds the lowest entry at or above the request.
	in = period(epycInput("conservative", 0), 1)
	in.CurKHz = 2400000
	if req, got := governorRequestKHz(in), governorTargetKHz(in); req != 2280000 || got != 2400000 {
		t.Errorf("table descent: request %d target %d, want 2280000 and 2400000", req, got)
	}

	// The two resolutions make a hysteresis, so a step in which nothing
	// moves keeps the entry the policy is at. 400 MHz of work from 2400 MHz
	// descends until the request is 1800 MHz, held at 1900 MHz (load 21,
	// between the thresholds). The next step sees the same load at 1900 MHz
	// and stays there; resolving the request upward instead would drop to
	// 1500 MHz with no governor action.
	const work = 400000.0
	in = period(epycInput("conservative", work/2400000), 100)
	in.CurKHz = 2400000
	prev := int64(2400000)
	for i, want := range []int64{1900000, 1900000, 1900000} {
		req, got := GovernorStep(in, prev)
		if req != 1800000 || got != want {
			t.Errorf("hold, step %d: request %d target %d, want 1800000 and %d", i, req, got, want)
		}
		in.CurKHz, prev = req, got
		in.Util = work / float64(got)
	}
	// Without the previous target, GovernorStep is the pair of the other two.
	in = period(epycInput("conservative", 0.3), 3)
	in.CurKHz = 1800000
	if req, got := GovernorStep(in, 0); req != governorRequestKHz(in) || got != governorTargetKHz(in) {
		t.Errorf("GovernorStep without a previous target: %d, %d, want %d, %d", req, got, governorRequestKHz(in), governorTargetKHz(in))
	}

	// Tunables replace the kernel defaults.
	tuned := period(continuousInput("conservative", 0.5, 2000000), 1)
	tuned.Tunables.ConservativeFreqStep = 10
	tuned.Tunables.ConservativeUpThreshold = 40
	if got := governorTargetKHz(tuned); got != 2300000 {
		t.Errorf("freq_step 10, up_threshold 40, load 50: got %d kHz, want 2300000", got)
	}
	tuned.Tunables.ConservativeUpThreshold = 90
	tuned.Tunables.ConservativeDownThreshold = 60
	if got := governorTargetKHz(tuned); got != 1700000 {
		t.Errorf("down_threshold 60, load 50: got %d kHz, want 1700000", got)
	}

	// A request outside the range restarts from the policy's frequency.
	out := period(continuousInput("conservative", 1, 0), 1)
	out.CurKHz = 3500000
	if got := governorTargetKHz(out); got != 3000000 {
		t.Errorf("request above the maximum: got %d kHz, want 3000000", got)
	}
}

func TestGP05HWPPowersaveFollowsUtilAndEPP(t *testing.T) {
	hwp := func(util float64, epp int) GovernorInput {
		in := continuousInput("powersave", util, 2000000)
		in.HWP = true
		in.EPP = epp
		in.EPPGain = DefaultEPPGain
		in.EPPCeilingDrop = DefaultEPPCeilingDrop
		return in
	}
	epps := []int{0, 32, 64, 128, 192, 255}
	for _, epp := range epps {
		prev := int64(0)
		for u := 0; u <= 20; u++ {
			got := governorTargetKHz(hwp(float64(u)/20, epp))
			if got < prev {
				t.Errorf("EPP %d: target falls from %d to %d kHz at util %.2f", epp, prev, got, float64(u)/20)
			}
			if got < 1000000 || got > 3000000 {
				t.Errorf("EPP %d, util %.2f: %d kHz outside the policy range", epp, float64(u)/20, got)
			}
			prev = got
		}
	}
	for u := 0; u <= 20; u++ {
		prev := int64(1 << 62)
		for _, epp := range epps {
			got := governorTargetKHz(hwp(float64(u)/20, epp))
			if got > prev {
				t.Errorf("util %.2f: target rises from %d to %d kHz at EPP %d", float64(u)/20, prev, got, epp)
			}
			prev = got
		}
	}
	if got := governorTargetKHz(hwp(0.5, 0)); got != 3000000 {
		t.Errorf("EPP 0 at util 0.5: got %d kHz, want the maximum 3000000", got)
	}
	// c(128) = 1 - 0.2 x 128/128 = 0.8 of the range at full load.
	if got := governorTargetKHz(hwp(1, 128)); got != 2600000 {
		t.Errorf("EPP 128 at util 1: got %d kHz, want 2600000", got)
	}
	// c(255) = 1 - 0.2 x 255/128 = 0.6015625 of the range.
	if got := governorTargetKHz(hwp(1, 255)); got != 2203125 {
		t.Errorf("EPP 255 at util 1: got %d kHz, want 2203125", got)
	}
	// The ceiling stops at half the range.
	steep := hwp(1, 255)
	steep.EPPCeilingDrop = 0.5
	if got := governorTargetKHz(steep); got != 2000000 {
		t.Errorf("EPP 255 at util 1, eppCeilingDrop 0.5: got %d kHz, want 2000000", got)
	}
	if got := governorTargetKHz(hwp(0, 0)); got != 1000000 {
		t.Errorf("idle: got %d kHz, want the minimum 1000000", got)
	}
}

func TestGP06OndemandJumpsToTheMaximumAboveUpThreshold(t *testing.T) {
	// The default up_threshold is 95 on a kernel with idle micro accounting.
	for _, tc := range []struct {
		name string
		util float64
		up   int64
		want int64
	}{
		{"default threshold, load 96", 0.96, 0, 3000000},
		{"default threshold, load 95", 0.95, 0, 2900000},
		{"up_threshold 80, load 81", 0.81, 80, 3000000},
		// Proportional from cpuinfo_min_freq: 1.0 + 0.80 x 2.0 GHz.
		{"up_threshold 80, load 80", 0.80, 80, 2600000},
		{"up_threshold 80, load 29", 0.29, 80, 1580000},
		{"idle", 0, 80, 1000000},
	} {
		in := continuousInput("ondemand", tc.util, 1000000)
		in.Tunables.OndemandUpThreshold = tc.up
		if got := governorTargetKHz(in); got != tc.want {
			t.Errorf("%s: got %d kHz, want %d", tc.name, got, tc.want)
		}
	}
	// On a table the proportional frequency goes to the closest entry:
	// 1500 + 0.50 x 2207 = 2603.5 MHz is closest to 2400.
	if got := governorTargetKHz(epycInput("ondemand", 0.5)); got != 2400000 {
		t.Errorf("table, load 50: got %d kHz, want 2400000", got)
	}
	// 1500 + 0.15 x 2207 = 1831 MHz is closest to 1900.
	if got := governorTargetKHz(epycInput("ondemand", 0.15)); got != 1900000 {
		t.Errorf("table, load 15: got %d kHz, want 1900000", got)
	}
	if got := governorTargetKHz(epycInput("ondemand", 0.99)); got != 2400000 {
		t.Errorf("table, load 99: got %d kHz, want 2400000", got)
	}
}

func TestResolveTableKHzMatchesTheKernelRelations(t *testing.T) {
	table := []int64{2400000, 1500000, 1900000}
	for _, tc := range []struct {
		v    int64
		rel  freqRelation
		want int64
	}{
		{1700000, relationLow, 1900000},
		{1700000, relationHigh, 1500000},
		{1700000, relationClosest, 1500000}, // a tie goes to the lower entry
		{1800000, relationClosest, 1900000},
		{3000000, relationLow, 2400000},
		{1000000, relationHigh, 1500000},
		{1900000, relationHigh, 1900000},
	} {
		if got := resolveTableKHz(table, tc.v, tc.rel); got != tc.want {
			t.Errorf("resolve %d with relation %d: got %d, want %d", tc.v, tc.rel, got, tc.want)
		}
	}
	if got := resolveTableKHz(nil, 1234, relationLow); got != 1234 {
		t.Errorf("no table: got %d, want 1234", got)
	}
}
