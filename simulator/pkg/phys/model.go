package phys

import (
	"math"

	"github.com/matbun/joulie/simulator/pkg/hw"
)

type DeviceState struct {
	Utilization      float64
	FreqScale        float64
	CapWatts         float64
	MaxCapWatts      float64
	IdlePowerWatts   float64
	MemoryIntensity  float64
	IOIntensity      float64
	CPUFeedIntensity float64
	TemperatureC     float64
	ThermalThrottle  float64
	Class            string
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
	util := clamp01(state.Utilization)
	activity := cpuActivityFactor(state)
	u := clamp01(util*activity) * 100.0
	base := interpolateCurve(m.Points, u)
	if base <= 0 {
		return 0
	}
	scale := m.ThroughputMultiplier(state, state.Class)
	if scale <= 0 {
		scale = 0.01
	}
	adjLoad := clamp01(clamp01(state.Utilization)*scale) * 100.0
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
	computeScale := f
	memoryScale := 1.0
	if f >= knee {
		memoryScale = 1.0 - 0.12*(1.0-f)
	} else {
		memoryScale = 1.0 - 0.12*(1.0-knee) - 0.45*((knee-f)/math.Max(0.1, knee))
	}
	ioScale := 1.0 - 0.05*(1.0-f)
	wc, wm, wi := cpuBoundnessWeights(state, workloadClass)
	out := wc*computeScale + wm*memoryScale + wi*ioScale
	return applyThermalPenalty(clamp01(out), state)
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
	pNat := m.naturalPower(util, state)
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
	pNat := m.naturalPower(util, state)
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
	computeScale := math.Pow(ratio, computeGamma)
	memScale := 1.0 - memEps*math.Pow(1.0-ratio, memGamma)
	wc, wm := gpuBoundnessWeights(state, workloadClass)
	out := wc*computeScale + wm*memScale
	return applyThermalPenalty(clamp01(out), state)
}

func (m CappedBoardGPUModel) naturalPower(util float64, state DeviceState) float64 {
	util = clamp01(util)
	dyn := math.Max(0, m.MaxW-m.IdleW)
	computeCurve := m.IdleW + dyn*math.Pow(util, 1.02)
	memoryCurve := m.IdleW + dyn*(0.35*math.Sqrt(util)+0.30*util)
	wc, wm := gpuBoundnessWeights(state, state.Class)
	return wc*computeCurve + wm*memoryCurve
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

func cpuActivityFactor(state DeviceState) float64 {
	mem := clamp01(state.MemoryIntensity)
	io := clamp01(state.IOIntensity)
	activity := 1.0 - 0.30*mem - 0.45*io
	if stringsHasPrefixFast(state.Class, "cpu.compute_bound") {
		activity += 0.08
	}
	if stringsHasPrefixFast(state.Class, "cpu.io_bound") {
		activity -= 0.12
	}
	return clampRange(activity, 0.35, 1.0)
}

func cpuBoundnessWeights(state DeviceState, workloadClass string) (compute, memory, ioWeight float64) {
	mem := clamp01(state.MemoryIntensity)
	ioIntensity := clamp01(state.IOIntensity)
	switch workloadClass {
	case "cpu.compute_bound":
		compute = 0.75
		memory = 0.20
		ioWeight = 0.05
	case "cpu.memory_bound":
		compute = 0.20
		memory = 0.75
		ioWeight = 0.05
	case "cpu.io_bound":
		compute = 0.10
		memory = 0.10
		ioWeight = 0.80
	default:
		compute = 0.45
		memory = 0.35
		ioWeight = 0.20
	}
	compute = clamp01(0.55*compute + 0.45*(1.0-math.Max(mem, ioIntensity)))
	memory = clamp01(0.55*memory + 0.45*mem)
	ioWeight = clamp01(0.55*ioWeight + 0.45*ioIntensity)
	sum := compute + memory + ioWeight
	if sum <= 0 {
		return 0.45, 0.35, 0.20
	}
	return compute / sum, memory / sum, ioWeight / sum
}

func gpuBoundnessWeights(state DeviceState, workloadClass string) (compute, memory float64) {
	mem := clamp01(state.MemoryIntensity)
	feed := clamp01(state.CPUFeedIntensity)
	switch workloadClass {
	case "gpu.compute_bound":
		compute = 0.80
		memory = 0.20
	case "gpu.memory_bound", "gpu.bandwidth_bound":
		compute = 0.20
		memory = 0.80
	default:
		compute = 0.50
		memory = 0.50
	}
	compute = clamp01(compute + 0.10*feed)
	compute = clamp01(0.60*compute + 0.40*(1.0-mem))
	memory = clamp01(0.60*memory + 0.40*mem)
	sum := compute + memory
	if sum <= 0 {
		return 0.5, 0.5
	}
	return compute / sum, memory / sum
}

func applyThermalPenalty(v float64, state DeviceState) float64 {
	return clamp01(v * (1.0 - clamp01(state.ThermalThrottle)))
}

func clampRange(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func stringsHasPrefixFast(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return s[:len(prefix)] == prefix
}
