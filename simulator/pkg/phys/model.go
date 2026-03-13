package phys

import (
	"math"

	"github.com/matbun/joulie/simulator/pkg/hw"
)

type DeviceState struct {
	Utilization    float64
	FreqScale      float64
	CapWatts       float64
	MaxCapWatts    float64
	IdlePowerWatts float64
	Class          string
}

type PowerModel interface {
	Power(state DeviceState) float64
	ThroughputMultiplier(state DeviceState, workloadClass string) float64
}

type MeasuredCurveCPUModel struct {
	Points []hw.PowerPoint
	Knee   float64
}

func (m MeasuredCurveCPUModel) Power(state DeviceState) float64 {
	if len(m.Points) == 0 {
		return 0
	}
	u := clamp01(state.Utilization) * 100.0
	base := interpolateCurve(m.Points, u)
	if base <= 0 {
		return 0
	}
	scale := m.ThroughputMultiplier(state, state.Class)
	if scale <= 0 {
		scale = 0.01
	}
	adjLoad := clamp01(clamp01(state.Utilization) * scale * 100.0)
	p := interpolateCurve(m.Points, adjLoad)
	if state.CapWatts > 0 && p > state.CapWatts {
		p = state.CapWatts
	}
	return p
}

func (m MeasuredCurveCPUModel) ThroughputMultiplier(state DeviceState, workloadClass string) float64 {
	f := clamp01(state.FreqScale)
	knee := m.Knee
	if knee <= 0 {
		knee = 0.7
	}
	switch workloadClass {
	case "cpu.compute_bound":
		return f
	case "cpu.memory_bound":
		if f >= knee {
			// Memory-bound workloads degrade slowly above the non-linear knee.
			return 1.0 - 0.2*(1.0-f)
		}
		return 0.8 * (f / math.Max(0.1, knee))
	default:
		mem := 1.0
		if f >= knee {
			mem = 1.0 - 0.2*(1.0-f)
		} else {
			mem = 0.8 * (f / math.Max(0.1, knee))
		}
		return 0.5*f + 0.5*mem
	}
}

type ProxyCurveCPUModel struct {
	Base              MeasuredCurveCPUModel
	DynamicScale      float64
	HighLoadSteepness float64
}

func (m ProxyCurveCPUModel) Power(state DeviceState) float64 {
	if len(m.Base.Points) == 0 {
		return 0
	}
	u := clamp01(state.Utilization) * 100.0
	p := interpolateCurve(m.Base.Points, u)
	idle := m.Base.Points[0].PowerW
	dyn := math.Max(0, p-idle) * m.DynamicScale
	if m.HighLoadSteepness > 0 && u > 70 {
		f := 1.0 + ((u-70.0)/30.0)*m.HighLoadSteepness
		dyn *= f
	}
	out := idle + dyn
	if state.CapWatts > 0 && out > state.CapWatts {
		out = state.CapWatts
	}
	return out
}

func (m ProxyCurveCPUModel) ThroughputMultiplier(state DeviceState, workloadClass string) float64 {
	return m.Base.ThroughputMultiplier(state, workloadClass)
}

type CappedBoardGPUModel struct {
	IdleW         float64
	MaxW          float64
	ComputeGamma  float64
	MemoryEpsilon float64
	MemoryGamma   float64
}

func (m CappedBoardGPUModel) Power(state DeviceState) float64 {
	util := clamp01(state.Utilization)
	capW := state.CapWatts
	if capW <= 0 {
		capW = state.MaxCapWatts
	}
	if capW <= 0 {
		capW = m.MaxW
	}
	if capW <= 0 {
		capW = 1
	}
	pNat := m.naturalPower(util, state.Class)
	if pNat > capW {
		pNat = capW
	}
	if pNat < m.IdleW {
		pNat = m.IdleW
	}
	return pNat
}

func (m CappedBoardGPUModel) ThroughputMultiplier(state DeviceState, workloadClass string) float64 {
	util := clamp01(state.Utilization)
	pNat := m.naturalPower(util, workloadClass)
	capW := state.CapWatts
	if capW <= 0 {
		capW = state.MaxCapWatts
	}
	if capW <= 0 {
		capW = m.MaxW
	}
	if capW <= 0 || pNat <= 0 {
		return 1
	}
	if capW >= pNat {
		return 1
	}
	ratio := clamp01(capW / pNat)
	computeGamma := m.ComputeGamma
	if computeGamma <= 0 {
		computeGamma = 1.0
	}
	memEps := m.MemoryEpsilon
	if memEps <= 0 {
		memEps = 0.2
	}
	memGamma := m.MemoryGamma
	if memGamma <= 0 {
		memGamma = 1.1
	}
	switch workloadClass {
	case "gpu.compute_bound":
		return math.Pow(ratio, computeGamma)
	case "gpu.memory_bound":
		return 1.0 - memEps*math.Pow(1.0-ratio, memGamma)
	default:
		c := math.Pow(ratio, computeGamma)
		mm := 1.0 - memEps*math.Pow(1.0-ratio, memGamma)
		return 0.5*c + 0.5*mm
	}
}

func (m CappedBoardGPUModel) naturalPower(util float64, class string) float64 {
	util = clamp01(util)
	dyn := math.Max(0, m.MaxW-m.IdleW)
	switch class {
	case "gpu.compute_bound":
		return m.IdleW + dyn*util
	case "gpu.memory_bound":
		return m.IdleW + dyn*0.65*math.Sqrt(util)
	default:
		c := m.IdleW + dyn*util
		mem := m.IdleW + dyn*0.65*math.Sqrt(util)
		return 0.5*c + 0.5*mem
	}
}

func interpolateCurve(points []hw.PowerPoint, load float64) float64 {
	if len(points) == 0 {
		return 0
	}
	load = math.Max(points[0].LoadPct, math.Min(load, points[len(points)-1].LoadPct))
	for i := 1; i < len(points); i++ {
		a := points[i-1]
		b := points[i]
		if load <= b.LoadPct {
			span := b.LoadPct - a.LoadPct
			if span <= 0 {
				return b.PowerW
			}
			t := (load - a.LoadPct) / span
			return a.PowerW + t*(b.PowerW-a.PowerW)
		}
	}
	return points[len(points)-1].PowerW
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
