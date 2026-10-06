package hwemu

import "math"

// nodeCounterSpan is the span of a RAPL energy status register: the kernel
// reads 32 bits of it (intel_rapl_common.c:33, 50), so the raw count wraps at
// 2^32 and energy_uj wraps at MaxEnergyRangeUJ of the unit.
const nodeCounterSpan = 1 << 32

// nodeCounter is the raw counter behind one energy_uj file. The file shows
// floor(raw*unitNJ/1000) (intel_rapl_common.c:215, 741), and raw is the seed
// plus the whole counts of the energy since the Node started. Counting the
// cumulative energy, rather than rounding each step's share, keeps the count
// independent of how the time was cut into steps.
type nodeCounter struct {
	unitNJ int64
	seed   uint64
}

// nodeNewCounter resumes the counter of a file that shows energyUJ. The seed
// is the smallest raw count that shows at least energyUJ, so a Node started
// over a live tree never moves the file backwards, which the agent would read
// as a wrap and a power spike (dvfs.Controller.readPowerWatts). It moves
// forward by less than one count.
func nodeNewCounter(unitNJ, energyUJ int64) nodeCounter {
	if energyUJ < 0 {
		energyUJ = 0
	}
	// A value past the range is not one the kernel prints; folding it keeps
	// the arithmetic below in int64.
	energyUJ %= nodeCounterSpan * unitNJ / 1000
	seed := (energyUJ*1000 + unitNJ - 1) / unitNJ
	return nodeCounter{unitNJ: unitNJ, seed: uint64(seed) % nodeCounterSpan}
}

// energyUJ is the file value after joules more than at the seed.
func (c nodeCounter) energyUJ(joules float64) int64 {
	counts := uint64(0)
	if joules > 0 {
		counts = uint64(math.Floor(joules * 1e9 / float64(c.unitNJ)))
	}
	raw := (c.seed + counts) % nodeCounterSpan
	return int64(raw) * c.unitNJ / 1000
}

// nodeUnitFromRange recovers the energy unit of a zone from its
// max_energy_range_uj, the inverse of MaxEnergyRangeUJ: the range is the
// 32-bit span floored to whole uJ, so rounding gives the unit back exactly for
// every unit the kernel derives (1e9 >> ESU nJ).
func nodeUnitFromRange(rangeUJ int64) int64 {
	return int64(math.Round(float64(rangeUJ) * 1000 / float64(nodeCounterSpan-1)))
}

// nodeMilliJoules is an NVML-style energy counter: whole mJ since the seed.
func nodeMilliJoules(seedMJ int64, joules float64) int64 {
	if joules <= 0 {
		return seedMJ
	}
	return seedMJ + int64(math.Floor(joules*1000))
}
