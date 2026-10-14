package hwemu

import (
	"math"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/phys"
)

// gpuClockState is the governor and clock state of one GPU, shared between
// the vendor lanes and the physics. The AMD lane (node_dpm.go) sets it from
// power_dpm_force_performance_level, the overdrive range and the pp_dpm_*
// tables; the NVIDIA lane (node_nvclock.go) from the locks the fake
// nvidia-smi writes. The physics turns it into power and throughput and
// keeps the peer transfer ledgers. State reports it. The zero value is a GPU
// without a clock model: no clocks, no governor, nothing moved, and the
// physics ignores it.
type gpuClockState struct {
	// modelled is set by a lane that maintains this state; until then State
	// reports no clocks.
	modelled bool

	// native is the governor state in the driver's own words: the amdgpu
	// performance level, or on NVIDIA the locks as nvidia-smi was told
	// them. preset is the vendor-neutral preset it maps to, empty when no
	// single one does.
	native, preset string

	// computeFloorMHz and computeCeilMHz bound the compute clock: a level's
	// soft minimum, the overdrive range, an -lgc lock. Zero leaves that side
	// unbounded. computeMaxMHz is the hardware maximum the physics scales
	// against.
	computeFloorMHz, computeCeilMHz, computeMaxMHz float64
	// memFloorMHz and memCeilMHz bound the memory clock (an -lmc lock).
	memFloorMHz, memCeilMHz float64

	// dataPath is the data-path level the native state requests. Under a
	// demand-driven level (dataPathAuto) the lane raises it while the GPU
	// runs its own memory-active work and holds it holdS seconds after.
	dataPath     phys.DataPathLevel
	dataPathAuto bool
	holdS        float64
	// gatingOff is true when idle gating (deep sleep) is disabled, as under
	// the AMD profile_* levels.
	gatingOff bool

	// mhz is the current clock of each domain, keyed by the
	// semconv.ClockDomain* values, as the readback files show it after the
	// last step. maxMHz is each domain's hardware maximum and limitMHz the
	// maximum in effect: the clock the domain reaches at full load under
	// the native state, with the data path raised.
	mhz, maxMHz, limitMHz map[string]float64

	// peerRxBytes and peerTxBytes integrate the peer transfer bytes the GPU
	// received and sent since the Node started.
	peerRxBytes, peerTxBytes float64
}

// stateClockHz is ClockHz of State: the current clock of each domain in Hz,
// nil for a GPU without a clock model.
func (c *gpuClockState) stateClockHz() map[string]int64 { return c.stateHz(c.mhz) }

// stateClockMaxHz and stateClockLimitHz are ClockMaxHz and ClockLimitHz of
// State: each domain's hardware maximum and the maximum in effect, in Hz.
func (c *gpuClockState) stateClockMaxHz() map[string]int64   { return c.stateHz(c.maxMHz) }
func (c *gpuClockState) stateClockLimitHz() map[string]int64 { return c.stateHz(c.limitMHz) }

// stateHz converts clocks per domain in MHz to a new map in whole Hz, nil
// for a GPU without a clock model.
func (c *gpuClockState) stateHz(mhz map[string]float64) map[string]int64 {
	if !c.modelled || len(mhz) == 0 {
		return nil
	}
	out := make(map[string]int64, len(mhz))
	for _, d := range semconv.ClockDomains() {
		if v, ok := mhz[d]; ok {
			out[d] = int64(math.Round(v * 1e6))
		}
	}
	return out
}

// setDomain sets the current clock, the hardware maximum and the maximum in
// effect of domain d, in MHz.
func (c *gpuClockState) setDomain(d string, cur, maxMHz, limit float64) {
	if c.mhz == nil {
		c.mhz = map[string]float64{}
	}
	if c.maxMHz == nil {
		c.maxMHz, c.limitMHz = map[string]float64{}, map[string]float64{}
	}
	c.mhz[d], c.maxMHz[d], c.limitMHz[d] = cur, maxMHz, limit
}

// stateBytes converts a ledger to the whole bytes State reports.
func stateBytes(b float64) uint64 {
	if b <= 0 {
		return 0
	}
	return uint64(math.Floor(b))
}
