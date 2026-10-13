package phys

import "math"

// The GPU clock model: board power and throughput of a GPU whose compute
// clock a governor may lock or ceil, whose data-path clocks may be held at
// their maximum or forced to their minimum, and whose idle gating may be off.
// Every value here is assumed (a simulator parameter to calibrate); the
// shapes follow published sweeps of the compute clock (Karimi et al., SC24-W,
// arXiv 2408.01552, Table III(a): compute-bound runtime grows as the clock
// falls, a memory-bandwidth benchmark stays flat).
//
// The cap and the clock act through one clock scale. The cap-implied clock
// is the compute scale of CappedBoardGPUModel, (cap / natural)^ComputeGamma,
// and the GPU runs at the lower of it and the governor's clock, never at
// their product. At the maximum governor clock both functions return exactly
// what the capped board model gives (GP07). The formulation, as one rule:
// the cap and the governor are two limits, and lowering either never raises
// the power or the throughput of any workload class (GP13).
//
//   - Throughput: the compute share is the run clock; the memory share is
//     the lower of the capped board's memory scale and the memory knee at
//     the governor's clock, each a limit of its own.
//   - Power: at or above the cap-implied clock the capped board power; below
//     it, that power with its dynamic part scaled by (governor clock /
//     cap-implied clock)^ClockBeta. Either way never more than the governor's
//     clock draws without a cap, idle + (P_natural - idle) x r^ClockBeta.
//
// The cap-implied clock comes from ComputeGamma and the power at a clock
// from ClockBeta, and the two describe one clock when
// ComputeGamma x ClockBeta x (1 - IdleW / MaxW) <= 1. Then the uncapped
// bound never binds: a lock at the cap-implied clock draws what the cap
// allows, the cap itself, a lock above it changes nothing (above the memory
// knee, for the throughput too) and a lock below it draws less (GP09). Beyond
// that bound the board's cap response draws more at a clock than the clock
// curve does, and the uncapped bound decides: a lock at or above the
// cap-implied clock draws the lower of the cap and its own uncapped power, at
// the cap's compute share, since a cap that raised a locked GPU's power would
// make the two limits contradict each other (GP14).

// gpuIdleUtil is the utilization below which a GPU counts as idle: only then
// does holding the data path at its maximum or turning idle gating off cost
// power, since a busy GPU's natural power already includes its data path and
// a busy GPU does not gate. The same threshold as the CPU governor's idle
// rule (assumed).
const gpuIdleUtil = 0.05

// GPUPowerWithClock is the board power, in W, of a GPU running s at clock c.
// Without a clock model (c.ComputeMaxMHz <= 0) it is m.Power(s). With one:
//
//   - while the governor's clock is at or above the cap-implied clock, the
//     power is m.Power(s), the capped board power;
//   - below it, the dynamic part of that power scales with
//     (clock / cap-implied clock)^ClockBeta, so with no binding cap
//     P = idle + (P_natural - idle) x (f_c / f_c,max)^ClockBeta;
//   - either way it is at most that uncapped power at the governor's clock,
//     so neither a lower cap nor a lower clock raises it;
//   - at idle (utilization below gpuIdleUtil) the GPU adds DataPathHoldW
//     while its data path is at DataPathMax and GatingOffIdleW while its idle
//     gating is off. Under auto the data path falls when the GPU idles, so
//     the caller passes the level the auto governor has reached.
func GPUPowerWithClock(m CappedBoardGPUModel, s DeviceState, c GPUClock, f GPUClockFacts) float64 {
	p := m.Power(s)
	if c.ComputeMaxMHz <= 0 {
		return p
	}
	capClock, _, _ := gpuCapScales(m, s)
	clock := gpuGovernorScale(c)
	if clock < capClock {
		p = m.IdleW + (p-m.IdleW)*math.Pow(clock/capClock, f.clockBeta())
	}
	pNat := math.Max(m.naturalPower(clamp01(s.Utilization), s), m.IdleW)
	if uncapped := m.IdleW + (pNat-m.IdleW)*math.Pow(clock, f.clockBeta()); uncapped < p {
		p = uncapped
	}
	if clamp01(s.Utilization) < gpuIdleUtil {
		if c.DataPath == DataPathMax {
			p += f.DataPathHoldW
		}
		if c.GatingOff {
			p += f.GatingOffIdleW
		}
	}
	return p
}

// GPUThroughputWithClock is the throughput multiplier of workloadClass on a
// GPU running s at clock c, relative to the GPU uncapped at its maximum
// clock. Without a clock model (c.ComputeMaxMHz <= 0) it is
// m.ThroughputMultiplier(s, workloadClass). With one, the compute and memory
// shares of the class (the weights of ThroughputMultiplier) scale as
// gpuClockScales says, and, as in ThroughputMultiplier, the thermal penalty
// applies only while the cap binds.
func GPUThroughputWithClock(m CappedBoardGPUModel, s DeviceState, c GPUClock, f GPUClockFacts, workloadClass string) float64 {
	if c.ComputeMaxMHz <= 0 {
		return m.ThroughputMultiplier(s, workloadClass)
	}
	compute, memory, capBinds := gpuClockScales(m, s, c, f)
	wc, wm := gpuBoundnessWeights(s, workloadClass)
	out := clamp01(wc*compute + wm*memory)
	if capBinds {
		out = applyThermalPenalty(out, s)
	}
	return out
}

// gpuClockScales returns the compute and memory throughput scales of a GPU
// running s at clock c, before the class weights, and whether the cap binds.
//
// The compute scale is the run clock as a fraction of the maximum: the lower
// of the governor's clock and the cap-implied clock. The memory scale is the
// lower of two limits: the capped board model's memory scale, and the memory
// knee at the governor's clock, 1 at or above MemoryKneeFrac of the maximum
// clock and (clock / knee)^MemoryKneeGamma below. With the data path at
// DataPathMin it is further multiplied by 1 - DataPathGamma: memory-bound
// work loses that fraction, compute-bound work nothing. Both scales are
// non-increasing in the cap and in the clock, each limit on its own.
func gpuClockScales(m CappedBoardGPUModel, s DeviceState, c GPUClock, f GPUClockFacts) (compute, memory float64, capBinds bool) {
	capClock, capMemory, capBinds := gpuCapScales(m, s)
	clock := gpuGovernorScale(c)
	compute = math.Min(clock, capClock)
	memory = math.Min(capMemory, f.memoryKnee(clock))
	if c.DataPath == DataPathMin {
		memory *= 1 - clamp01(f.DataPathGamma)
	}
	return compute, memory, capBinds
}

// gpuCapScales is the compute and memory scale ThroughputMultiplier gives a
// capped board, and whether its cap binds; the compute scale is the
// cap-implied clock. It repeats ThroughputMultiplier's arithmetic, which
// GP07 checks.
func gpuCapScales(m CappedBoardGPUModel, s DeviceState) (compute, memory float64, binds bool) {
	pNat := m.naturalPower(clamp01(s.Utilization), s)
	capW := s.CapWatts
	if capW <= 0 {
		capW = s.MaxCapWatts
	}
	if capW <= 0 {
		capW = m.MaxW
	}
	if capW <= 0 || pNat <= 0 || capW >= pNat {
		return 1, 1, false
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
	return math.Pow(ratio, computeGamma), 1.0 - memEps*math.Pow(1.0-ratio, memGamma), true
}

// gpuGovernorScale is the compute clock the governor allows as a fraction of
// the maximum, within [0, 1]; a zero ComputeMHz allows the maximum.
func gpuGovernorScale(c GPUClock) float64 {
	if c.ComputeMHz <= 0 {
		return 1
	}
	return clamp01(c.ComputeMHz / c.ComputeMaxMHz)
}

// clockBeta is ClockBeta, with zero meaning 1, as for the exponents of
// AnalyticCPUModel.
func (f GPUClockFacts) clockBeta() float64 {
	if f.ClockBeta <= 0 {
		return 1
	}
	return f.ClockBeta
}

// memoryKnee is the memory scale at clock scale r with no cap: 1 at or above
// MemoryKneeFrac, (r / MemoryKneeFrac)^MemoryKneeGamma below. A zero
// MemoryKneeFrac means no knee, and a zero MemoryKneeGamma means 1.
func (f GPUClockFacts) memoryKnee(r float64) float64 {
	knee := clamp01(f.MemoryKneeFrac)
	if knee <= 0 || r >= knee {
		return 1
	}
	gamma := f.MemoryKneeGamma
	if gamma <= 0 {
		gamma = 1
	}
	return math.Pow(clamp01(r)/knee, gamma)
}
