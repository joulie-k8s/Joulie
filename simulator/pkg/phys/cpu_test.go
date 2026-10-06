package phys

import (
	"math"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hw"
)

// simulatorAnalyticPower is the analytic branch of cpuPowerWithModel in
// simulator/cmd/simulator, with the model and node fields as arguments, kept
// here as the reference for the move.
func simulatorAnalyticPower(baseIdleW, pMaxW, alphaUtil, betaFreq, cpuUtil, freqScale, memoryIntensity, ioIntensity, thermalThrottle float64) float64 {
	util := clamp01(cpuUtil)
	freq := clamp01(freqScale)
	alpha := alphaUtil
	if alpha <= 0 {
		alpha = 1
	}
	beta := betaFreq
	if beta <= 0 {
		beta = 1
	}
	activity := 1.0 - 0.30*clamp01(memoryIntensity) - 0.45*clamp01(ioIntensity)
	activity = math.Max(0.35, math.Min(1.0, activity))
	p := baseIdleW + (pMaxW-baseIdleW)*math.Pow(clamp01(util*activity), alpha)*math.Pow(freq, beta)
	p *= 1.0 - 0.35*clamp01(thermalThrottle)
	return p
}

// simulatorSolveFreqScaleForCap is the analytic branch of
// solveFreqScaleForCap in simulator/cmd/simulator, with minFreqScale(model)
// passed in.
func simulatorSolveFreqScaleForCap(baseIdleW, pMaxW, alphaUtil, betaFreq, util, capW, minFreq float64) float64 {
	util = clamp01(util)
	if util <= 0 || pMaxW <= baseIdleW {
		return 1
	}
	alpha := alphaUtil
	if alpha <= 0 {
		alpha = 1
	}
	beta := betaFreq
	if beta <= 0 {
		beta = 1
	}
	den := (pMaxW - baseIdleW) * math.Pow(util, alpha)
	if den <= 0 {
		return 1
	}
	x := (capW - baseIdleW) / den
	if x <= 0 {
		return minFreq
	}
	return math.Pow(x, 1.0/beta)
}

// P01: AnalyticCPUModel.Power is the simulator formula moved verbatim, activity
// and thermal factors included. A few points are evaluated by hand, the rest
// of the grid against the formula as main.go writes it.
func TestP01AnalyticPowerIsTheSimulatorFormula(t *testing.T) {
	m := AnalyticCPUModel{IdleW: 40, MaxW: 165, AlphaUtil: 1.15, BetaFreq: 1.35, Knee: 0.7}
	byHand := []struct {
		name string
		s    DeviceState
		want float64
	}{
		{"full load, full frequency", DeviceState{Utilization: 1, FreqScale: 1}, 165},
		{"half load", DeviceState{Utilization: 0.5, FreqScale: 1}, 96.32815391317689},
		{"memory bound, half frequency, hot", DeviceState{Utilization: 1, FreqScale: 0.5, MemoryIntensity: 1, ThermalThrottle: 0.5}, 59.84331905885863},
		{"activity floor 0.35", DeviceState{Utilization: 1, FreqScale: 1, MemoryIntensity: 1, IOIntensity: 1}, 77.37560781281391},
		{"idle", DeviceState{Utilization: 0, FreqScale: 1}, 40},
	}
	for _, c := range byHand {
		if got := m.Power(c.s); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: Power = %v, want %v", c.name, got, c.want)
		}
	}

	models := []AnalyticCPUModel{m, {IdleW: 80, MaxW: 420}, {IdleW: 45, MaxW: 400, AlphaUtil: 0.9, BetaFreq: 2}}
	for _, mm := range models {
		for _, util := range []float64{-0.1, 0, 0.3, 0.7, 1, 1.2} {
			for _, freq := range []float64{0.3, 0.8, 1, 1.1} {
				for _, mem := range []float64{0, 0.5, 1} {
					for _, io := range []float64{0, 0.6} {
						for _, th := range []float64{0, 0.5, 1} {
							s := DeviceState{Utilization: util, FreqScale: freq, MemoryIntensity: mem, IOIntensity: io, ThermalThrottle: th, CapWatts: 50}
							want := simulatorAnalyticPower(mm.IdleW, mm.MaxW, mm.AlphaUtil, mm.BetaFreq, util, freq, mem, io, th)
							if got := mm.Power(s); got != want {
								t.Fatalf("%+v at %+v: Power = %v, want %v", mm, s, got, want)
							}
						}
					}
				}
			}
		}
	}
}

// P02: FirstOrderToward is the clamped Euler step of the simulator. An
// exponential would give 10*(1-e^-0.5) = 3.93 for half a time constant and never
// reach the target.
func TestP02FirstOrderTowardIsAClampedEulerStep(t *testing.T) {
	cases := []struct {
		name                     string
		current, target, dt, tau float64
		want                     float64
	}{
		{"half a time constant moves half way", 0, 10, 0.5, 1, 5},
		{"one tenth", 165, 120, 0.1, 1, 160.5},
		{"a full time constant lands on target", 0, 10, 1, 1, 10},
		{"a longer step does not overshoot", 0, 10, 3, 1, 10},
		{"no time constant jumps", 4, 10, 0.01, 0, 10},
		{"no time passes", 4, 10, 0, 1, 4},
	}
	for _, c := range cases {
		if got := FirstOrderToward(c.current, c.target, c.dt, c.tau); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("%s: FirstOrderToward(%v, %v, %v, %v) = %v, want %v", c.name, c.current, c.target, c.dt, c.tau, got, c.want)
		}
	}
}

// H15: the solver throttles a curve model (D13 in cmd/agent/hwemu_test.go:
// the simulator's bisection passes CapWatts, the curve clamps to it, so p >
// capW never holds and the scale stays at 1), and the analytic closed form is
// the simulator's.
func TestH15SolveFreqScaleForCapThrottlesCurveModels(t *testing.T) {
	curve := MeasuredCurveCPUModel{
		Points: []hw.PowerPoint{{LoadPct: 0, PowerW: 100}, {LoadPct: 50, PowerW: 300}, {LoadPct: 100, PowerW: 700}},
		Knee:   0.7,
	}
	const minScale = 0.3
	checked := 0
	for _, class := range []string{"cpu.compute_bound", "cpu.mixed"} {
		for _, util := range []float64{0.5, 0.8, 1} {
			base := DeviceState{Utilization: util, FreqScale: 1, Class: class}
			natural := curve.Power(base)
			low := base
			low.FreqScale = minScale
			floor := curve.Power(low)
			for _, frac := range []float64{0.2, 0.5, 0.8} {
				capW := floor + frac*(natural-floor)
				in := base
				in.CapWatts = capW // the trap of D13: a solver that keeps it never throttles
				scale := SolveFreqScaleForCap(curve, in, capW, minScale)
				if scale >= 1 {
					t.Errorf("%s util %.1f cap %.1f W (natural %.1f W): scale = %v, want < 1", class, util, capW, natural, scale)
					continue
				}
				at := base
				at.FreqScale = scale
				if p := curve.Power(at); p > capW*1.01 {
					t.Errorf("%s util %.1f: power at scale %.4f = %.2f W, want <= cap %.2f W + 1%%", class, util, scale, p, capW)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no reachable cap was checked")
	}
	if got := SolveFreqScaleForCap(curve, DeviceState{Utilization: 1, FreqScale: 1}, 1000, minScale); got != 1 {
		t.Errorf("cap above natural power: scale = %v, want 1", got)
	}

	for _, m := range []AnalyticCPUModel{{IdleW: 40, MaxW: 165, AlphaUtil: 1.15, BetaFreq: 1.35}, {IdleW: 80, MaxW: 420}, {IdleW: 50, MaxW: 50}} {
		for _, util := range []float64{0, 0.25, 0.6, 1} {
			for _, capW := range []float64{20, 60, 100, 150, 400} {
				want := simulatorSolveFreqScaleForCap(m.IdleW, m.MaxW, m.AlphaUtil, m.BetaFreq, util, capW, 0.375)
				s := DeviceState{Utilization: util, FreqScale: 1, MemoryIntensity: 0.4}
				if got := SolveFreqScaleForCap(m, s, capW, 0.375); got != want {
					t.Errorf("%+v util %v cap %v: closed form = %v, want main.go's %v", m, util, capW, got, want)
				}
				if got := SolveFreqScaleForCap(&m, s, capW, 0.375); got != want {
					t.Errorf("pointer %+v util %v cap %v: closed form = %v, want %v", m, util, capW, got, want)
				}
			}
		}
	}
}
