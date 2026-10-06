package phys

import "math"

// AnalyticCPUModel is the simulator's analytic package power model (the
// analytic branch of cpuPowerWithModel in simulator/cmd/simulator): idle
// power plus a dynamic part that grows with utilization^AlphaUtil and
// frequency scale^BetaFreq. Knee only shapes throughput, exactly as in
// MeasuredCurveCPUModel. A zero AlphaUtil or BetaFreq means 1.
type AnalyticCPUModel struct {
	IdleW     float64
	MaxW      float64
	AlphaUtil float64
	BetaFreq  float64
	Knee      float64
}

// Power is the natural power for s. Memory and IO intensity lower the activity
// factor and thermal throttling scales the result. CapWatts is ignored: the
// caller enforces its own cap, so a solver can always see the natural power.
func (m AnalyticCPUModel) Power(s DeviceState) float64 {
	util := clamp01(s.Utilization)
	freq := clamp01(s.FreqScale)
	alpha, beta := m.exponents()
	activity := 1.0 - 0.30*clamp01(s.MemoryIntensity) - 0.45*clamp01(s.IOIntensity)
	activity = math.Max(0.35, math.Min(1.0, activity))
	p := m.IdleW + (m.MaxW-m.IdleW)*math.Pow(clamp01(util*activity), alpha)*math.Pow(freq, beta)
	p *= 1.0 - 0.35*clamp01(s.ThermalThrottle)
	return p
}

// ThroughputMultiplier uses the knee formula of MeasuredCurveCPUModel, which
// depends on the frequency scale and the workload class, never on the curve.
func (m AnalyticCPUModel) ThroughputMultiplier(s DeviceState, workloadClass string) float64 {
	return MeasuredCurveCPUModel{Knee: m.Knee}.ThroughputMultiplier(s, workloadClass)
}

func (m AnalyticCPUModel) exponents() (alpha, beta float64) {
	alpha, beta = m.AlphaUtil, m.BetaFreq
	if alpha <= 0 {
		alpha = 1
	}
	if beta <= 0 {
		beta = 1
	}
	return alpha, beta
}

// solveFreqScaleForCap is the closed form of the analytic branch of the
// simulator's solveFreqScaleForCap. It inverts the formula without the
// activity and thermal factors, and the result is not clamped: callers clamp
// it to [minScale, current scale].
func (m AnalyticCPUModel) solveFreqScaleForCap(util, capW, minScale float64) float64 {
	if m.MaxW <= m.IdleW {
		return 1
	}
	alpha, beta := m.exponents()
	den := (m.MaxW - m.IdleW) * math.Pow(util, alpha)
	if den <= 0 {
		return 1
	}
	x := (capW - m.IdleW) / den
	if x <= 0 {
		return minScale
	}
	return math.Pow(x, 1.0/beta)
}

// SolveFreqScaleForCap returns the frequency scale at which m, running s,
// draws capW. Zero utilization needs no throttling and gives 1. An
// AnalyticCPUModel uses its closed form; any other model is bisected between
// minScale and 1 with CapWatts cleared, because a model that clamps to its
// cap never reports power above it and would never be throttled (D13 in
// cmd/agent/hwemu_test.go: the curve branch of the simulator's
// solveFreqScaleForCap). When even minScale draws more than capW, the result
// is minScale.
func SolveFreqScaleForCap(m PowerModel, s DeviceState, capW, minScale float64) float64 {
	util := clamp01(s.Utilization)
	if util <= 0 {
		return 1
	}
	minScale = clamp01(minScale)
	switch a := m.(type) {
	case AnalyticCPUModel:
		return a.solveFreqScaleForCap(util, capW, minScale)
	case *AnalyticCPUModel:
		if a != nil {
			return a.solveFreqScaleForCap(util, capW, minScale)
		}
	}
	probe := s
	probe.CapWatts = 0
	probe.FreqScale = 1
	if m.Power(probe) <= capW {
		return 1
	}
	lo, hi := minScale, 1.0
	for i := 0; i < 22; i++ {
		mid := (lo + hi) / 2.0
		probe.FreqScale = mid
		if m.Power(probe) > capW {
			hi = mid
		} else {
			lo = mid
		}
	}
	return lo
}

// FirstOrderToward moves current toward target by one clamped Euler step,
// current + (target-current)*min(dt/tauSec, 1), as the simulator does for
// caps, frequency and averages (its firstOrderToward). It is not an
// exponential: a step of dt >= tauSec lands on target. tauSec <= 0 gives
// target, dt <= 0 current.
func FirstOrderToward(current, target, dt, tauSec float64) float64 {
	if tauSec <= 0 {
		return target
	}
	if dt <= 0 {
		return current
	}
	step := dt / tauSec
	if step > 1 {
		step = 1
	}
	return current + (target-current)*step
}
