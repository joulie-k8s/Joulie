package compat

import "math"

// compatCounter turns a cumulative energy reading into increments. The first
// reading only sets the baseline. A reading below the last one, which a
// ledger never produces, adds nothing and becomes the new baseline, and a
// value that is not finite is ignored.
type compatCounter struct {
	seen bool
	last float64
}

// delta returns the energy added since the last reading, never negative.
func (c *compatCounter) delta(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	d := 0.0
	if c.seen && v > c.last {
		d = v - c.last
	}
	c.seen, c.last = true, v
	return d
}

// k07Split splits one Kepler v0.7.12 series into idle and dynamic energy.
// Upstream keeps, per series, the energy of the interval with the lowest
// node CPU time seen so far as the idle energy of every later interval, and
// the dynamic energy is the rest (pkg/collector/stats/node_stats.go:69-104,
// pkg/collector/stats/stats.go:158-181). A zero increment is skipped, so a
// series that never moved has no idle energy yet.
//
// assumed: the lowest interval is kept as a power, not as an energy, because
// emulator ticks need not be the fixed 3 s Kepler sample period; and the
// idle increment is capped at the interval's energy, so idle plus dynamic
// always equals the energy measured, where upstream reports dynamic 0 and
// the full idle when the interval is below the idle one.
type k07Split struct {
	seen    bool
	minLoad float64
	idleW   float64
	idleJ   float64
	dynJ    float64
}

// add splits deltaJ, measured over dt seconds at the given load, and
// returns the idle and dynamic increments. A new idle baseline needs a load
// and a power at or below the current ones, as upstream requires both the
// resource utilisation and the energy to be at or below the stored minimum
// (node_stats.go:89-90).
func (s *k07Split) add(deltaJ, dt, load float64) (idle, dyn float64) {
	if !(deltaJ > 0) || !(dt > 0) || math.IsInf(deltaJ, 0) {
		return 0, 0
	}
	p := deltaJ / dt
	if !s.seen || (load <= s.minLoad && p <= s.idleW) {
		s.seen, s.minLoad, s.idleW = true, load, p
	}
	idle = math.Min(float64(s.idleW*dt), deltaJ)
	dyn = deltaJ - idle
	s.idleJ += idle
	s.dynJ += dyn
	return idle, dyn
}

// compatActiveIdle accumulates energy split by an active ratio, as Kepler
// v0.12.0 does: active = delta x ratio and idle = delta - active
// (internal/monitor/node.go:131-141 for cpu zones, with the node CPU usage
// ratio; internal/monitor/process.go:264-299 for GPUs, with active power
// over total power). Both totals start at zero with the exporter.
type compatActiveIdle struct {
	activeJ float64
	idleJ   float64
}

// add splits deltaJ with ratio, kept within 0..1.
func (a *compatActiveIdle) add(deltaJ, ratio float64) {
	if !(deltaJ > 0) || math.IsInf(deltaJ, 0) {
		return
	}
	// The conversion keeps the product rounded, so the idle part is the
	// same on platforms that fuse a multiply and a subtraction.
	active := float64(deltaJ * compatClamp01(ratio))
	a.activeJ += active
	a.idleJ += deltaJ - active
}

// compatClamp01 keeps v within 0..1; NaN reads as 0.
func compatClamp01(v float64) float64 {
	switch {
	case !(v > 0):
		return 0
	case v > 1:
		return 1
	}
	return v
}
