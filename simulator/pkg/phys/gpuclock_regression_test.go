package phys

import (
	"math"
	"testing"
)

// GP14: lowering the cap never raises what a locked GPU gets. Two models
// break it. One that scales a locked GPU's power from the cap at the
// cap-implied clock: where ComputeGamma x ClockBeta x (1 - IdleW / MaxW)
// exceeds 1 (here 1.2 x 2 x 0.875 = 2.1) a cap raises the power of a GPU
// locked below the cap-implied clock above what the same lock draws without
// a cap. And one that takes the memory share of a locked GPU relative to the
// memory knee at the cap-implied clock: below the knee a lower cap moves that
// reference down and raises the memory-bound throughput of a locked GPU.
func TestGP14LoweringTheCapNeverRaisesALockedGPU(t *testing.T) {
	lock := gpClock(0.5 * gpClockMaxMHz)
	for _, class := range gpClockClasses {
		s := gpBusy(1, 0, class)
		free := GPUPowerWithClock(gpClockBoard, s, lock, gpClockFacts)
		s.CapWatts = gpCapAt(s, 0.55)
		if capped := GPUPowerWithClock(gpClockBoard, s, lock, gpClockFacts); capped > free+gpClockTol {
			t.Errorf("class %q locked at 0.50 of the maximum clock: %.3f W under a %.3f W cap (cap-implied clock 0.55), %.3f W without a cap; a cap must never raise the power",
				class, capped, s.CapWatts, free)
		}
	}

	// Locked at 0.3 of the maximum clock, below the 0.6 knee, while the cap
	// comes down until its implied clock nears the lock.
	lock = gpClock(0.3 * gpClockMaxMHz)
	for _, class := range []string{"gpu.memory_bound", "gpu.bandwidth_bound"} {
		s := gpBusy(1, 0, class)
		prev, prevClock := math.Inf(1), 0.0
		for _, capClock := range []float64{1, 0.8, 0.6, 0.5, 0.4, 0.31} {
			s.CapWatts = 0
			if capClock < 1 {
				s.CapWatts = gpCapAt(s, capClock)
			}
			got := GPUThroughputWithClock(gpClockBoard, s, lock, gpClockFacts, class)
			if got > prev+gpClockTol {
				t.Errorf("class %q locked at 0.30: lowering the cap from implied clock %.2f to %.2f raised the throughput %.6f -> %.6f",
					class, prevClock, capClock, prev, got)
			}
			prev, prevClock = got, capClock
		}
	}
}
