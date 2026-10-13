package phys

import (
	"fmt"
	"math"
	"testing"
)

// gpClockMaxMHz is the maximum compute clock of the test GPU, a neutral
// value.
const gpClockMaxMHz = 1500

// gpClockTol is how close two values must be to count as equal: far below
// any effect of the model, far above float rounding.
const gpClockTol = 1e-9

// gpClockBoard is the capped board model of the tests.
var gpClockBoard = CappedBoardGPUModel{IdleW: 50, MaxW: 400, ComputeGamma: 1.2, MemoryEpsilon: 0.25, MemoryGamma: 1.3}

// gpClockFacts sets every clock fact to a value with a visible effect.
var gpClockFacts = GPUClockFacts{
	ClockBeta:       2,
	MemoryKneeFrac:  0.6,
	MemoryKneeGamma: 1.5,
	DataPathHoldW:   12,
	GatingOffIdleW:  7,
	DataPathGamma:   0.3,
}

// gpClockClasses are the GPU workload classes gpuBoundnessWeights tells
// apart, and the default.
var gpClockClasses = []string{"gpu.compute_bound", "gpu.memory_bound", "gpu.bandwidth_bound", ""}

// gpClock is a GPU whose governor allows mhz, data path at its maximum and
// idle gating kept.
func gpClock(mhz float64) GPUClock {
	return GPUClock{ComputeMHz: mhz, ComputeMaxMHz: gpClockMaxMHz}
}

// gpBusy is a busy GPU running class under capW, 0 for no cap.
func gpBusy(util, capW float64, class string) DeviceState {
	return DeviceState{Utilization: util, CapWatts: capW, MaxCapWatts: gpClockBoard.MaxW, Class: class}
}

// gpCapAt is the cap at which the board, running s, has the cap-implied
// clock scale r: (cap / natural)^ComputeGamma = r.
func gpCapAt(s DeviceState, r float64) float64 {
	return gpClockBoard.naturalPower(s.Utilization, s) * math.Pow(r, 1/gpClockBoard.ComputeGamma)
}

func gpNear(a, b float64) bool { return math.Abs(a-b) <= gpClockTol }

// GP07: at the maximum compute clock, the data path at its maximum and idle
// gating kept, power and throughput equal CappedBoardGPUModel for every
// class, with and without a binding cap, whatever the clock facts. A
// governor that allows nothing (ComputeMHz 0) is the same, and a GPU without
// a clock model (ComputeMaxMHz 0) is the board model even with every idle
// cost on. At idle the anchor is the auto governor, whose data path has
// fallen: then the power is the board's idle power too.
func TestGP07ClockAtMaxEqualsTheCappedBoardModel(t *testing.T) {
	for _, class := range gpClockClasses {
		for _, util := range []float64{0.05, 0.3, 0.7, 1} {
			for _, mem := range []float64{0, 0.6} {
				for _, throttle := range []float64{0, 0.2} {
					base := gpBusy(util, 0, class)
					base.MemoryIntensity, base.CPUFeedIntensity, base.ThermalThrottle = mem, 0.5, throttle
					pNat := gpClockBoard.naturalPower(util, base)
					for _, capW := range []float64{0, pNat * 1.1, pNat * 0.9, pNat * 0.5, gpClockBoard.IdleW * 0.5} {
						s := base
						s.CapWatts = capW
						wantP, wantT := gpClockBoard.Power(s), gpClockBoard.ThroughputMultiplier(s, class)
						for _, c := range []GPUClock{gpClock(gpClockMaxMHz), gpClock(0)} {
							if got := GPUPowerWithClock(gpClockBoard, s, c, gpClockFacts); !gpNear(got, wantP) {
								t.Errorf("class %q util %.2f mem %.1f cap %.1f W governor %v MHz: power %.6f, want the board's %.6f", class, util, mem, capW, c.ComputeMHz, got, wantP)
							}
							if got := GPUThroughputWithClock(gpClockBoard, s, c, gpClockFacts, class); !gpNear(got, wantT) {
								t.Errorf("class %q util %.2f mem %.1f throttle %.1f cap %.1f W governor %v MHz: throughput %.6f, want the board's %.6f", class, util, mem, throttle, capW, c.ComputeMHz, got, wantT)
							}
						}
						none := GPUClock{ComputeMHz: 300, DataPath: DataPathMin, GatingOff: true}
						if got := GPUPowerWithClock(gpClockBoard, s, none, gpClockFacts); !gpNear(got, wantP) {
							t.Errorf("no clock model, class %q util %.2f cap %.1f W: power %.6f, want the board's %.6f", class, util, capW, got, wantP)
						}
						if got := GPUThroughputWithClock(gpClockBoard, s, none, gpClockFacts, class); !gpNear(got, wantT) {
							t.Errorf("no clock model, class %q util %.2f cap %.1f W: throughput %.6f, want the board's %.6f", class, util, capW, got, wantT)
						}
					}
				}
			}
		}
		for _, util := range []float64{0, 0.02} {
			s := gpBusy(util, 0, class)
			auto := GPUClock{ComputeMHz: gpClockMaxMHz, ComputeMaxMHz: gpClockMaxMHz, DataPath: DataPathMin, DataPathAuto: true}
			if got, want := GPUPowerWithClock(gpClockBoard, s, auto, gpClockFacts), gpClockBoard.Power(s); !gpNear(got, want) {
				t.Errorf("class %q idle at util %.2f under auto: power %.6f, want the board's %.6f", class, util, got, want)
			}
		}
	}
}

// GP08: with no binding cap, the compute share of the throughput is the
// compute clock as a fraction of its maximum, linear in f_c, and the memory
// share is 1 at or above MemoryKneeFrac of the maximum and
// (f_c / knee)^MemoryKneeGamma below. The class throughput is the weighted
// sum of the two, so a memory-bound class above the knee loses only its
// compute weight.
func TestGP08ComputeLinearInClockMemoryFlatAboveTheKnee(t *testing.T) {
	knee := gpClockFacts.MemoryKneeFrac
	for _, class := range gpClockClasses {
		s := gpBusy(1, 0, class)
		wc, wm := gpuBoundnessWeights(s, class)
		for mhz := 150.0; mhz <= gpClockMaxMHz; mhz += 75 {
			r := mhz / gpClockMaxMHz
			compute, memory, binds := gpuClockScales(gpClockBoard, s, gpClock(mhz), gpClockFacts)
			if binds {
				t.Fatalf("class %q: the uncapped board reports a binding cap", class)
			}
			wantMem := 1.0
			if r < knee {
				wantMem = math.Pow(r/knee, gpClockFacts.MemoryKneeGamma)
			}
			if !gpNear(compute, r) || !gpNear(memory, wantMem) {
				t.Errorf("class %q at %v MHz: compute %.6f memory %.6f, want %.6f and %.6f", class, mhz, compute, memory, r, wantMem)
			}
			if got, want := GPUThroughputWithClock(gpClockBoard, s, gpClock(mhz), gpClockFacts, class), wc*r+wm*wantMem; !gpNear(got, want) {
				t.Errorf("class %q at %v MHz: throughput %.6f, want %.4f x %.4f + %.4f x %.4f = %.6f", class, mhz, got, wc, r, wm, wantMem, want)
			}
		}
	}
	// Above the knee the memory share is flat: two clocks there differ only
	// by the compute weight times the clock difference.
	s := gpBusy(1, 0, "gpu.memory_bound")
	wc, _ := gpuBoundnessWeights(s, "gpu.memory_bound")
	hi := GPUThroughputWithClock(gpClockBoard, s, gpClock(gpClockMaxMHz), gpClockFacts, "gpu.memory_bound")
	lo := GPUThroughputWithClock(gpClockBoard, s, gpClock(0.75*gpClockMaxMHz), gpClockFacts, "gpu.memory_bound")
	if !gpNear(hi-lo, wc*0.25) {
		t.Errorf("memory-bound above the knee: 100 %% minus 75 %% of the clock is %.6f, want the compute weight x 0.25 = %.6f", hi-lo, wc*0.25)
	}
}

// GP09: the cap and the governor act through one clock. Below the
// cap-implied clock the compute share is the governor's own clock, never the
// product of the two scales (the cap counted twice), the GPU draws less than
// its cap and at least its idle power, lowering the governor's clock never
// raises power or throughput, and the hand-over at the cap-implied clock is
// continuous. With ComputeGamma x ClockBeta x (1 - IdleW / MaxW) <= 1 a lock
// at or above the cap-implied clock (here above the memory knee) changes
// nothing: it draws exactly the cap, at the cap's throughput. Beyond that
// bound it still runs at the cap's throughput, and draws the lower of the
// cap and what its clock draws without a cap, since a cap never raises the
// power (GP14).
func TestGP09LockAndCapActThroughOneClock(t *testing.T) {
	within := gpClockBoard
	within.ComputeGamma = 1 / (gpClockFacts.ClockBeta * (1 - within.IdleW/within.MaxW))
	for _, tc := range []struct {
		name     string
		board    CappedBoardGPUModel
		capClock float64
		within   bool
	}{
		{"within the bound", within, 0.7, true},
		{"beyond the bound", gpClockBoard, 0.65, false},
	} {
		board := tc.board
		for _, class := range gpClockClasses {
			s := gpBusy(1, 0, class)
			s.CapWatts = board.naturalPower(1, s) * math.Pow(tc.capClock, 1/board.ComputeGamma)
			power := func(r float64) float64 { return GPUPowerWithClock(board, s, gpClock(r*gpClockMaxMHz), gpClockFacts) }
			throughput := func(r float64) float64 {
				return GPUThroughputWithClock(board, s, gpClock(r*gpClockMaxMHz), gpClockFacts, class)
			}
			free := s
			free.CapWatts = 0
			uncapped := func(r float64) float64 {
				return GPUPowerWithClock(board, free, gpClock(r*gpClockMaxMHz), gpClockFacts)
			}
			maxP, maxT := power(1), throughput(1)
			if !gpNear(maxP, s.CapWatts) || maxT >= 1 {
				t.Fatalf("%s, class %q: setup: power %.3f W under a %.3f W cap, throughput %.3f; the cap must bind", tc.name, class, maxP, s.CapWatts, maxT)
			}
			if beyond := uncapped(tc.capClock) < s.CapWatts; beyond == tc.within {
				t.Fatalf("%s, class %q: setup: the lock at the cap-implied clock draws %.3f W without a cap, the cap is %.3f W", tc.name, class, uncapped(tc.capClock), s.CapWatts)
			}
			for _, r := range []float64{tc.capClock, (tc.capClock + 1) / 2, 0.95, 1} {
				wantP := s.CapWatts
				if !tc.within {
					wantP = math.Min(s.CapWatts, uncapped(r))
				}
				if p, tp := power(r), throughput(r); !gpNear(p, wantP) || !gpNear(tp, maxT) {
					t.Errorf("%s, class %q governor at %.3f, at or above the %.2f cap clock: power %.6f throughput %.6f, want %.6f and the cap's %.6f",
						tc.name, class, r, tc.capClock, p, tp, wantP, maxT)
				}
			}
			for _, r := range []float64{tc.capClock - 0.05, 0.4, 0.2} {
				c := gpClock(r * gpClockMaxMHz)
				compute, _, _ := gpuClockScales(board, s, c, gpClockFacts)
				if !gpNear(compute, r) {
					t.Errorf("%s, class %q governor at %.2f below the %.2f cap clock: compute share %.6f, want the governor's %.2f (the product would be %.4f)", tc.name, class, r, tc.capClock, compute, r, r*tc.capClock)
				}
				if p := power(r); p >= s.CapWatts || p < board.IdleW || p > uncapped(r)+gpClockTol {
					t.Errorf("%s, class %q governor at %.2f: power %.3f W, want below the %.3f W cap, at most the %.3f W it draws without one and at least the %.0f W idle power", tc.name, class, r, p, s.CapWatts, uncapped(r), board.IdleW)
				}
			}
			prevP, prevT := math.Inf(1), math.Inf(1)
			for mhz := float64(gpClockMaxMHz); mhz >= 30; mhz -= 15 {
				p, tp := power(mhz/gpClockMaxMHz), throughput(mhz/gpClockMaxMHz)
				if p > prevP+gpClockTol || tp > prevT+gpClockTol {
					t.Fatalf("%s, class %q: lowering the governor to %v MHz raised power %.6f -> %.6f or throughput %.6f -> %.6f", tc.name, class, mhz, prevP, p, prevT, tp)
				}
				prevP, prevT = p, tp
			}
			atP, atT := power(tc.capClock), throughput(tc.capClock)
			if p, tp := power(tc.capClock-1e-7), throughput(tc.capClock-1e-7); math.Abs(p-atP) > 1e-4 || math.Abs(tp-atT) > 1e-6 {
				t.Errorf("%s, class %q just below the cap clock: power %.6f throughput %.6f, want about %.6f and %.6f (no jump at the hand-over)", tc.name, class, p, tp, atP, atT)
			}
		}
	}
}

// GP13: lowering the cap or the governor's clock never raises the power or
// the throughput, over a grid of caps and clock locks, for compute-, memory-
// and transfer-bound work: within and beyond the ComputeGamma x ClockBeta
// bound, above and below the memory knee, busy and idle, with the data path
// at its maximum and at its minimum. Transfer-bound work is the
// bandwidth-bound class, and a job that spends a share p of its work on peer
// copies progresses at 1 / ((1 - p) / throughput + p / g), g the share of the
// link PeerRate gives the copy (no receiver gate here, so the clock leaves it
// alone).
func TestGP13LoweringTheCapOrTheClockNeverRaisesPowerOrThroughput(t *testing.T) {
	var boards []CappedBoardGPUModel
	for _, gamma := range []float64{0.5, 1, 1.2, 2} {
		for _, idle := range []float64{0, 100} {
			boards = append(boards, CappedBoardGPUModel{IdleW: idle, MaxW: gpClockBoard.MaxW, ComputeGamma: gamma, MemoryEpsilon: 0.25, MemoryGamma: 1.3})
		}
	}
	facts := []GPUClockFacts{
		gpClockFacts,
		{},
		{ClockBeta: 1, MemoryKneeFrac: 0.4, MemoryKneeGamma: 1, DataPathGamma: 0.1},
		{ClockBeta: 3, MemoryKneeFrac: 0.8, MemoryKneeGamma: 2, DataPathGamma: 0.5},
	}
	classes := []string{"gpu.compute_bound", "gpu.memory_bound", "gpu.bandwidth_bound", ""}
	// caps lists the cap as a fraction of the natural power, highest first;
	// 0 is no cap and comes first.
	caps := []float64{0}
	for frac := 1.3; frac > 0.01; frac -= 0.05 {
		caps = append(caps, frac)
	}
	var locks []float64
	for r := 1.0; r > 0; r -= 0.04 {
		locks = append(locks, r)
	}
	const link, peerShare = 1e9, 0.6
	jobRate := func(thr float64, recv GPUClock) float64 {
		g := PeerRate(link, link, recv, ReceiverGate{}, 0, 1) / link
		if thr <= 0 || g <= 0 {
			return 0
		}
		return 1 / ((1-peerShare)/thr + peerShare/g)
	}
	raised := func(prev, v float64) bool { return v > prev+1e-9*math.Max(1, math.Abs(prev)) }
	checked := 0
	for _, board := range boards {
		for fi, f := range facts {
			for _, class := range classes {
				for _, util := range []float64{0.02, 0.4, 1} {
					for _, mem := range []float64{0, 0.7} {
						for _, dp := range []DataPathLevel{DataPathMax, DataPathMin} {
							s := gpBusy(util, 0, class)
							s.MemoryIntensity, s.ThermalThrottle = mem, 0.2
							pNat := board.naturalPower(util, s)
							type point struct{ p, tp, job float64 }
							grid := make([][]point, len(caps))
							for i, frac := range caps {
								s.CapWatts = frac * pNat
								grid[i] = make([]point, len(locks))
								for j, r := range locks {
									c := gpClock(r * gpClockMaxMHz)
									c.DataPath = dp
									tp := GPUThroughputWithClock(board, s, c, f, class)
									grid[i][j] = point{GPUPowerWithClock(board, s, c, f), tp, jobRate(tp, c)}
									checked++
								}
							}
							for i := range caps {
								for j := range locks {
									at := grid[i][j]
									what := fmt.Sprintf("gamma %.1f idle %.0f W facts %d class %q util %.2f mem %.1f data path %v", board.ComputeGamma, board.IdleW, fi, class, util, mem, dp)
									if i > 0 {
										prev := grid[i-1][j]
										if raised(prev.p, at.p) || raised(prev.tp, at.tp) || raised(prev.job, at.job) {
											t.Fatalf("%s, lock %.2f: lowering the cap to %.3f of natural raised power %.6f -> %.6f, throughput %.6f -> %.6f or the transfer-bound job rate %.6f -> %.6f",
												what, locks[j], caps[i], prev.p, at.p, prev.tp, at.tp, prev.job, at.job)
										}
									}
									if j > 0 {
										prev := grid[i][j-1]
										if raised(prev.p, at.p) || raised(prev.tp, at.tp) || raised(prev.job, at.job) {
											t.Fatalf("%s, cap %.3f of natural: lowering the lock to %.2f raised power %.6f -> %.6f, throughput %.6f -> %.6f or the transfer-bound job rate %.6f -> %.6f",
												what, caps[i], locks[j], prev.p, at.p, prev.tp, at.tp, prev.job, at.job)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d points", checked)
}

// GP10: DataPathHoldW is drawn only at idle with the data path at its
// maximum, forced or held by the auto governor, and GatingOffIdleW only at
// idle with gating off; a busy GPU draws neither, since its natural power
// already includes its data path and it does not gate.
func TestGP10IdleCostsOnlyAtIdle(t *testing.T) {
	hold, gating := gpClockFacts.DataPathHoldW, gpClockFacts.GatingOffIdleW
	for _, tc := range []struct {
		name  string
		util  float64
		dp    DataPathLevel
		auto  bool
		gOff  bool
		extra float64
	}{
		{"idle, data path forced to max", 0, DataPathMax, false, false, hold},
		{"idle, data path held at max by auto", 0, DataPathMax, true, false, hold},
		{"idle, data path at mid", 0, DataPathMid, true, false, 0},
		{"idle, data path at min", 0, DataPathMin, false, false, 0},
		{"idle, gating off, data path at min", 0, DataPathMin, false, true, gating},
		{"idle, gating off, data path at max", 0, DataPathMax, false, true, hold + gating},
		{"nearly idle, gating off, data path at max", 0.04, DataPathMax, false, true, hold + gating},
		{"busy, gating off, data path at max", 0.05, DataPathMax, false, true, 0},
		{"fully busy, gating off, data path at max", 1, DataPathMax, false, true, 0},
	} {
		s := gpBusy(tc.util, 0, "gpu.memory_bound")
		c := GPUClock{ComputeMHz: gpClockMaxMHz, ComputeMaxMHz: gpClockMaxMHz, DataPath: tc.dp, DataPathAuto: tc.auto, GatingOff: tc.gOff}
		if got, want := GPUPowerWithClock(gpClockBoard, s, c, gpClockFacts), gpClockBoard.Power(s)+tc.extra; !gpNear(got, want) {
			t.Errorf("%s: power %.6f, want the board's %.6f plus %.0f W = %.6f", tc.name, got, gpClockBoard.Power(s), tc.extra, want)
		}
	}
}

// GP11: with the data path at its minimum the memory share of the throughput
// loses DataPathGamma of itself and the compute share nothing, so a class
// loses its memory weight times that; the data path at mid or max costs
// nothing. It holds under a binding cap too.
func TestGP11DataPathAtMinimumSlowsMemoryOnly(t *testing.T) {
	for _, class := range gpClockClasses {
		for _, capClock := range []float64{1, 0.7} {
			s := gpBusy(1, 0, class)
			if capClock < 1 {
				s.CapWatts = gpCapAt(s, capClock)
			}
			wc, wm := gpuBoundnessWeights(s, class)
			for _, mhz := range []float64{gpClockMaxMHz, 600} {
				at := func(d DataPathLevel) (compute, memory, throughput float64) {
					c := gpClock(mhz)
					c.DataPath = d
					compute, memory, _ = gpuClockScales(gpClockBoard, s, c, gpClockFacts)
					return compute, memory, GPUThroughputWithClock(gpClockBoard, s, c, gpClockFacts, class)
				}
				cMax, mMax, tMax := at(DataPathMax)
				cMin, mMin, tMin := at(DataPathMin)
				if !gpNear(cMin, cMax) {
					t.Errorf("class %q cap clock %.1f at %v MHz: compute share %.6f at min, %.6f at max; want no change", class, capClock, mhz, cMin, cMax)
				}
				if !gpNear(mMin, mMax*(1-gpClockFacts.DataPathGamma)) {
					t.Errorf("class %q cap clock %.1f at %v MHz: memory share %.6f at min, want %.6f x (1 - %.1f)", class, capClock, mhz, mMin, mMax, gpClockFacts.DataPathGamma)
				}
				if capClock == 1 && !gpNear(tMax-tMin, wm*mMax*gpClockFacts.DataPathGamma) {
					t.Errorf("class %q at %v MHz: the data path at min costs %.6f of throughput, want %.4f x %.4f x %.1f (compute weight %.4f untouched)", class, mhz, tMax-tMin, wm, mMax, gpClockFacts.DataPathGamma, wc)
				}
				if _, _, tMid := at(DataPathMid); !gpNear(tMid, tMax) {
					t.Errorf("class %q cap clock %.1f at %v MHz: data path at mid gives %.6f, want %.6f as at max", class, capClock, mhz, tMid, tMax)
				}
			}
		}
	}
}

// GP12: fitted to the power column of a published compute-clock sweep on a
// compute-bound benchmark (Karimi et al., SC24-W, arXiv 2408.01552, Table
// III(a): 83.7, 68.2 and 53.3 % of the power at 1500, 1300 and 900 of 1700
// MHz), ClockBeta and the idle share make the model reproduce the energy
// column (94.4, 88.6, 97.3 %) within 5 points and its ordering: 1300 MHz uses
// less energy than 1700 and 900. The fit is what makes the ordering: with
// arbitrary values, ClockBeta 1.5 and an idle share of 0.1, 900 MHz wins.
func TestGP12FitToAPublishedClockSweepReproducesItsEnergy(t *testing.T) {
	const (
		maxMHz = 1700
		maxW   = 500
		class  = "gpu.compute_bound"
	)
	mhz := []float64{1500, 1300, 900}
	powerPct := []float64{83.7, 68.2, 53.3}
	energyPct := []float64{94.4, 88.6, 97.3}
	s := DeviceState{Utilization: 1, Class: class}
	clock := func(f float64) GPUClock { return GPUClock{ComputeMHz: f, ComputeMaxMHz: maxMHz} }
	// energy is the energy to solution at each clock of mhz, in percent of
	// the energy at the maximum clock, and the power in percent.
	energy := func(m CappedBoardGPUModel, f GPUClockFacts) (e, p []float64) {
		p0 := GPUPowerWithClock(m, s, clock(maxMHz), f)
		t0 := GPUThroughputWithClock(m, s, clock(maxMHz), f, class)
		for _, v := range mhz {
			pr := GPUPowerWithClock(m, s, clock(v), f) / p0
			tr := GPUThroughputWithClock(m, s, clock(v), f, class) / t0
			p = append(p, 100*pr)
			e = append(e, 100*pr/tr)
		}
		return e, p
	}
	facts := GPUClockFacts{MemoryKneeFrac: 0.65, MemoryKneeGamma: 1}

	bestErr, bestBeta, bestIdle := math.Inf(1), 0.0, 0.0
	for beta := 1.0; beta <= 4; beta += 0.02 {
		for idle := 0.0; idle <= 0.9*maxW; idle += 2 {
			f := facts
			f.ClockBeta = beta
			_, p := energy(CappedBoardGPUModel{IdleW: idle, MaxW: maxW}, f)
			var sq float64
			for i := range p {
				sq += (p[i] - powerPct[i]) * (p[i] - powerPct[i])
			}
			if sq < bestErr {
				bestErr, bestBeta, bestIdle = sq, beta, idle
			}
		}
	}
	f := facts
	f.ClockBeta = bestBeta
	m := CappedBoardGPUModel{IdleW: bestIdle, MaxW: maxW}
	e, p := energy(m, f)
	share := bestIdle / m.naturalPower(1, s)
	t.Logf("fit: ClockBeta %.2f, idle share %.3f; power %.1f %%, energy %.1f %%", bestBeta, share, p, e)
	for i := range mhz {
		if math.Abs(p[i]-powerPct[i]) > 2 {
			t.Errorf("%v MHz: fitted power %.1f %%, want within 2 points of %.1f %%", mhz[i], p[i], powerPct[i])
		}
		if math.Abs(e[i]-energyPct[i]) > 5 {
			t.Errorf("%v MHz: energy %.1f %%, want within 5 points of the published %.1f %%", mhz[i], e[i], energyPct[i])
		}
	}
	if !(e[1] < 100 && e[1] < e[2]) {
		t.Errorf("energy %.1f %% at 1300 MHz, %.1f %% at 900; want 1300 the lowest of 1700 (100 %%), 1300 and 900", e[1], e[2])
	}

	// The control: ClockBeta 1.5 and an idle share of 0.1 put 900 MHz first.
	f.ClockBeta = 1.5
	k := CappedBoardGPUModel{MaxW: maxW}.naturalPower(1, s) / maxW
	idle := 0.1 * maxW * k / (0.9 + 0.1*k)
	m = CappedBoardGPUModel{IdleW: idle, MaxW: maxW}
	if got := idle / m.naturalPower(1, s); math.Abs(got-0.1) > 1e-9 {
		t.Fatalf("control: idle share %.6f, want 0.1", got)
	}
	if e, _ := energy(m, f); !(e[2] < e[1]) {
		t.Errorf("control: ClockBeta 1.5, idle share 0.1: energy %.1f %% at 1300 MHz, %.1f %% at 900; want 900 lower, as without a fit", e[1], e[2])
	}
}
