package calib

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// param returns a fitted parameter or fails.
func param(t *testing.T, r *Result, path string) Param {
	t.Helper()
	p, ok := r.Param(path)
	if !ok {
		t.Fatalf("the fit has no %s", path)
	}
	return p
}

// within checks a fitted value against want within a relative tolerance.
func within(t *testing.T, r *Result, path string, want, rel float64) {
	t.Helper()
	p := param(t, r, path)
	if !p.Fitted {
		t.Errorf("%s not fitted: %s", path, p.Reason)
		return
	}
	if math.Abs(p.Value-want) > rel*math.Abs(want) {
		t.Errorf("%s = %.6g, want %.6g within %.1f %%", path, p.Value, want, rel*100)
	}
}

// near checks a fitted value against want within an absolute tolerance.
func near(t *testing.T, r *Result, path string, want, abs float64) {
	t.Helper()
	p := param(t, r, path)
	if !p.Fitted {
		t.Errorf("%s not fitted: %s", path, p.Reason)
		return
	}
	if math.Abs(p.Value-want) > abs {
		t.Errorf("%s = %.6g, want %.6g +- %g", path, p.Value, want, abs)
	}
}

func fit(t *testing.T, sw sweep, o Options) *Result {
	t.Helper()
	r, err := Fit(sw.data(t), sw.profile, o)
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	return r
}

// CB01: a sweep of amd-epyc-2s-energy-only under the performance governor,
// with three frequency ceilings and six loads, gives back the profile: idle
// 40 W and maximum 360 W within 1 %, alpha 1.15 +- 0.02 and beta
// 1.35 +- 0.03.
func TestCB01EPYCPerformanceSweepGivesBackTheProfile(t *testing.T) {
	t.Parallel()
	sw := epycPerformanceSweep(t)
	cpu := sw.profile.Physics.CPU
	if cpu.IdlePkgW.V != 40 || cpu.MaxPkgW.V != 360 || cpu.AlphaUtil.V != 1.15 || cpu.BetaFreq.V != 1.35 {
		t.Fatalf("the profile changed (idle %v, max %v, alpha %v, beta %v); update the expected values", cpu.IdlePkgW.V, cpu.MaxPkgW.V, cpu.AlphaUtil.V, cpu.BetaFreq.V)
	}
	r := fit(t, sw, Options{})
	within(t, r, pathCPUIdle, 40, 0.01)
	within(t, r, pathCPUMax, 360, 0.01)
	near(t, r, pathCPUAlpha, 1.15, 0.02)
	near(t, r, pathCPUBeta, 1.35, 0.03)
	if r.Segments != 18 || r.Steady != 18 {
		t.Errorf("%d segments, %d steady, want 18 and 18", r.Segments, r.Steady)
	}
	// The platform draws nothing on this profile; host power minus the
	// packages leaves the unmeasured DRAM, which the base value covers.
	near(t, r, pathPlatform, 0, 0.2)
	if p := param(t, r, pathDRAM); p.Fitted {
		t.Errorf("DRAM fitted to %v on a profile with no DRAM zone", p.Value)
	}
	if p := param(t, r, pathGPUMax); p.Fitted {
		t.Errorf("GPU fitted on a node without GPUs")
	}
}

// CB04: beta is not fitted when frequency cannot be told from load, and is
// fitted when the governor holds frequency while load varies. The sweep
// reports the node's own utilization, which counts work in cycles (design
// D10), so a governor below the maximum raises it.
//   - intel-xeon-2s-rapl without its governor facts: frequency scales
//     linearly with load, as before governors were modelled, and the fit
//     cannot tell the exponents apart.
//   - amd-epyc-2s-energy-only under acpi-cpufreq powersave: frequency stays
//     at the minimum.
//   - intel-xeon-2s-rapl as shipped, intel_pstate powersave with EPP 128:
//     frequency rises with load up to the EPP ceiling, 0.85 of the maximum
//     from a load of 0.6, and stays there while utilization climbs from 0.7
//     to 1, so the fit gives the profile's beta back.
//
// Alpha and the maximum are fitted in every case, with the base beta when
// beta is not.
func TestCB04BetaIsNotFittedWhenFrequencyFollowsLoadOrStands(t *testing.T) {
	t.Parallel()
	loads := []float64{0, 0.2, 0.4, 0.6, 0.8, 1}
	steps := func() []sweepStep {
		var out []sweepStep
		for _, u := range loads {
			out = append(out, sweepStep{knobs: map[string]float64{semconv.KnobCPUUtilization: u}, samples: 11})
		}
		return out
	}
	withoutGovernorFacts := func(p *hwemu.Profile) {
		f := p.CPUFreq
		f.DriverMode, f.HWP, f.BoostScope = hwemu.Fact[string]{}, hwemu.Fact[bool]{}, hwemu.Fact[string]{}
		f.EPP, f.Tunables, f.Uncore = nil, nil, nil
	}
	for _, tc := range []struct {
		name, profile string
		mutate        func(*hwemu.Profile)
		// reason is why beta is not fitted; empty when it is.
		reason string
	}{
		{"intel_pstate powersave without governor facts", "intel-xeon-2s-rapl", withoutGovernorFacts, "frequency followed load"},
		{"acpi-cpufreq powersave", "amd-epyc-2s-energy-only", func(p *hwemu.Profile) { p.CPUFreq.Governor.V = "powersave" }, "one frequency"},
		// A base beta of 0 is 1 to the analytic model, and to the profile
		// the fit writes, which inherits it. Fitting with 0 left
		// ln(F_min) in the intercept: a maximum of 169 W, not 360 W.
		{"acpi-cpufreq powersave, base beta 0", "amd-epyc-2s-energy-only", func(p *hwemu.Profile) {
			p.CPUFreq.Governor.V = "powersave"
			p.Physics.CPU.BetaFreq.V = 0
		}, "one frequency"},
		{"intel_pstate powersave with EPP 128", "intel-xeon-2s-rapl", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sw := generateSweep(t, sweepSpec{profile: tc.profile, mutate: tc.mutate, period: 2 * time.Second, steps: steps()})
			r := fit(t, sw, Options{})
			cpu := sw.profile.Physics.CPU
			if tc.reason == "" {
				near(t, r, pathCPUBeta, cpu.BetaFreq.V, 0.03)
			} else if beta := param(t, r, pathCPUBeta); beta.Fitted || !strings.Contains(beta.Reason, tc.reason) {
				t.Errorf("beta fitted %v (%.4g), reason %q; want not fitted because %s", beta.Fitted, beta.Value, beta.Reason, tc.reason)
			}
			within(t, r, pathCPUIdle, cpu.IdlePkgW.V, 0.01)
			within(t, r, pathCPUMax, cpu.MaxPkgW.V, 0.01)
			near(t, r, pathCPUAlpha, cpu.AlphaUtil.V, 0.02)
		})
	}
}

// nvlGPUSweep is nvidia-h100-nvl-8gpu running gpu.compute_bound work at
// memory intensity 0, from idle to full load, with the CPUs idle.
func nvlGPUSweep(t *testing.T) sweep {
	t.Helper()
	var steps []sweepStep
	for _, u := range []float64{0, 0.25, 0.5, 0.75, 1} {
		steps = append(steps, sweepStep{
			knobs:    map[string]float64{semconv.KnobCPUUtilization: 0, semconv.KnobGPUUtilization: u},
			gpuClass: "gpu.compute_bound", samples: 11,
		})
	}
	return generateSweep(t, sweepSpec{profile: "nvidia-h100-nvl-8gpu", period: 2 * time.Second, steps: steps})
}

// CB07: the GPU board model is inverted at the recorded class. With
// gpu.compute_bound at memory intensity 0 the weights are 0.88 compute and
// 0.12 memory, so a full-load GPU draws idle + 0.958 of the range: 385.7 W
// on the NVL profile (idle 58.6 W, measured; natural maximum 400 W). The
// inversion gives back 58.6 W and 400 W within 1 %; the raw full-load mean
// fails the same check.
func TestCB07GPUNaturalMaximumInvertsTheBoardModel(t *testing.T) {
	t.Parallel()
	sw := nvlGPUSweep(t)
	gpu := sw.profile.Physics.GPU
	r := fit(t, sw, Options{})
	within(t, r, pathGPUIdle, gpu.IdleW.V, 0.01)
	within(t, r, pathGPUMax, gpu.NaturalMaxW.V, 0.01)

	d := sw.data(t)
	segs, err := Segments(d)
	if err != nil {
		t.Fatal(err)
	}
	full := segs[len(segs)-1]
	if full.Setpoints[semconv.KnobGPUUtilization] != 1 {
		t.Fatalf("last segment %+v is not full load", full.Setpoints)
	}
	a, b, _ := full.Steady(10 * time.Second)
	raw, n := d.Select(semconv.HwPower.Name, map[string]string{semconv.AttrHwID: "gpu0"})[0].mean(a, b)
	if n == 0 {
		t.Fatal("no gpu0 power at full load")
	}
	if want := gpu.IdleW.V + (gpu.NaturalMaxW.V-gpu.IdleW.V)*(0.88+0.12*0.65); math.Abs(raw-want) > 0.01 {
		t.Errorf("raw full-load power %.3f W, want %.3f W", raw, want)
	}
	if math.Abs(raw-gpu.NaturalMaxW.V) <= 0.01*gpu.NaturalMaxW.V {
		t.Errorf("the raw mean %.1f W passes the 1 %% check against %.0f W; the test cannot tell the inversion from no inversion", raw, gpu.NaturalMaxW.V)
	}
}

// CB08: settling times come from setpoint steps sampled at the substep, and
// a source with a fixed substep needs the correction
// tau = h / (1 - exp(-h/tau_eff)): the NVL GPU limit, 150 ms in the profile,
// decays like exp(-t/91 ms) under the simulator's 100 ms Euler step. The
// HSMP cap and the frequency ramp are fitted the same way.
func TestCB08SettlingNeedsTheSubstepCorrection(t *testing.T) {
	t.Parallel()
	h := 100 * time.Millisecond
	t.Run("gpu limit", func(t *testing.T) {
		t.Parallel()
		step := func(limit float64) sweepStep {
			return sweepStep{knobs: map[string]float64{semconv.KnobGPUUtilization: 1, semconv.KnobGPUPowerLimit: limit}, gpuClass: "gpu.compute_bound", samples: 30}
		}
		sw := generateSweep(t, sweepSpec{profile: "nvidia-h100-nvl-8gpu", period: h, steps: []sweepStep{step(400), step(250), step(400)}})
		want := float64(sw.profile.Physics.GPU.CapSettleTauMS.V)
		if want != 150 {
			t.Fatalf("the profile's capSettleTauMS is %v; update the test", want)
		}
		within(t, fit(t, sw, Options{Substep: h}), pathGPUCapSettle, want, 0.05)
		raw := fit(t, sw, Options{})
		within(t, raw, pathGPUCapSettle, 91, 0.05)
		if p := param(t, raw, pathGPUCapSettle); math.Abs(p.Value-want) <= 0.05*want {
			t.Errorf("without the correction %s = %.1f ms, within 5 %% of %.0f ms: the test cannot tell the correction", pathGPUCapSettle, p.Value, want)
		}
	})
	t.Run("hsmp cap", func(t *testing.T) {
		t.Parallel()
		step := func(limit float64, n int) sweepStep {
			return sweepStep{knobs: map[string]float64{semconv.KnobCPUUtilization: 1, semconv.KnobCPUPowerLimit: limit}, samples: n}
		}
		sw := generateSweep(t, sweepSpec{profile: "amd-epyc-2s-hsmp", period: h, steps: []sweepStep{step(400, 10), step(250, 100), step(400, 100)}})
		within(t, fit(t, sw, Options{Substep: h}), pathHSMPSettle, float64(sw.profile.Physics.HSMP.SettleTauMS.V), 0.05)
	})
	t.Run("frequency ramp", func(t *testing.T) {
		t.Parallel()
		step := func(hz float64, n int) sweepStep {
			return sweepStep{knobs: map[string]float64{semconv.KnobCPUUtilization: 0.5, semconv.KnobCPUFrequencyMax: hz}, samples: n}
		}
		sw := generateSweep(t, sweepSpec{
			profile: "amd-epyc-2s-energy-only", mutate: func(p *hwemu.Profile) { p.CPUFreq.Governor.V = "performance" },
			period: h, steps: []sweepStep{step(2.4e9, 10), step(1.5e9, 50), step(2.4e9, 50)},
		})
		within(t, fit(t, sw, Options{Substep: h}), pathCPUFreqRamp, float64(sw.profile.Physics.CPU.FreqRampMS.V), 0.05)
	})
	t.Run("too slow sampling", func(t *testing.T) {
		t.Parallel()
		r := fit(t, epycPerformanceSweep(t), Options{Substep: h})
		if p := param(t, r, pathCPUFreqRamp); p.Fitted || !strings.Contains(p.Reason, "sample faster") {
			t.Errorf("a 2 s sampled ramp of 500 ms: fitted %v (%.4g), reason %q; want not fitted, asking for faster sampling", p.Fitted, p.Value, p.Reason)
		}
	})
}

// A reading that is not a number never becomes a fitted value: NaN fails
// every comparison, so the exponent bounds once let a NaN alpha, beta and
// maximum through as fitted, and the profile could not be written. A NaN
// in an idle window once made idlePkgW and platform.otherW fitted NaNs.
func TestFitLeavesNaNReadingsUnfitted(t *testing.T) {
	t.Parallel()
	nanFit := func(t *testing.T, idle bool) *Result {
		t.Helper()
		sw := epycPerformanceSweep(t)
		d := sw.data(t)
		energy := d.Select(semconv.HwEnergy.Name, map[string]string{semconv.AttrHwID: "cpu0"})
		host := d.Select(semconv.HwHostEnergy.Name, map[string]string{semconv.AttrHwID: semconv.HwIDHost})
		if len(energy) != 1 || len(host) != 1 {
			t.Fatalf("cpu0 energy: %d series, host energy: %d; want 1 and 1", len(energy), len(host))
		}
		// The last sample sits in the steady window of the full-load
		// segment, the last of the first segment in that of an idle one.
		at := func(s *Series) int { return len(s.Points) - 1 }
		if idle {
			segs, err := Segments(d)
			if err != nil {
				t.Fatal(err)
			}
			end := segs[0].End()
			if segs[0].Setpoints[semconv.KnobCPUUtilization] != 0 {
				t.Fatalf("the first segment %v is not idle", segs[0].Setpoints)
			}
			at = func(s *Series) int {
				for i, p := range s.Points {
					if p.Time.Equal(end) {
						return i
					}
				}
				t.Fatalf("%s has no point at %v", s.Metric, end)
				return 0
			}
		}
		for _, s := range append(energy, host...) {
			s.Points[at(s)].Value = math.NaN()
		}
		// A NaN idle power leaves nothing to fit, and Fit says so, but
		// still returns the result with the reasons.
		r, err := Fit(d, sw.profile, Options{})
		if r == nil || err != nil && !idle {
			t.Fatalf("fit: %v", err)
		}
		for _, p := range r.Params {
			if p.Fitted && (math.IsNaN(p.Value) || math.IsInf(p.Value, 0)) {
				t.Errorf("%s fitted to %v", p.Path, p.Value)
			}
		}
		if p := param(t, r, pathCPUAlpha); p.Fitted || !strings.Contains(p.Reason, "NaN") {
			t.Errorf("alpha fitted %v (%v), reason %q; want not fitted, naming the NaN", p.Fitted, p.Value, p.Reason)
		}
		return r
	}
	t.Run("loaded window", func(t *testing.T) {
		t.Parallel()
		r := nanFit(t, false)
		within(t, r, pathCPUIdle, 40, 0.01)
		if _, err := LocalProfile(r, "nan-free", "ref", ""); err != nil {
			t.Errorf("the profile of what was fitted: %v", err)
		}
	})
	t.Run("idle window", func(t *testing.T) {
		t.Parallel()
		r := nanFit(t, true)
		for _, path := range []string{pathCPUIdle, pathPlatform} {
			if p := param(t, r, path); p.Fitted || !strings.Contains(p.Reason, "not a finite number") {
				t.Errorf("%s fitted %v (%v), reason %q; want not fitted", path, p.Fitted, p.Value, p.Reason)
			}
		}
	})
}
