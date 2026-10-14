package phys

import (
	"math"
	"testing"
)

// XF02: with the gate on, a receiver forced to its lowest data-path level
// gets SlowFraction of the link, one forced to mid MidFraction, and one at
// its highest level, forced or busy under auto, the whole link.
func TestXF02GateOnBoundsByTheReceiverLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		recv GPUClock
		frac float64
	}{
		{"forced min", GPUClock{DataPath: DataPathMin}, xfGateOn.SlowFraction},
		{"forced mid", GPUClock{DataPath: DataPathMid}, xfGateOn.MidFraction},
		{"forced max", GPUClock{DataPath: DataPathMax}, 1},
		{"auto at max", GPUClock{DataPath: DataPathMax, DataPathAuto: true}, 1},
	} {
		for src := 0; src < 8; src++ {
			if got, want := PeerRate(3*xfLink, xfLink, tc.recv, xfGateOn, src, 8), tc.frac*xfLink; got != want {
				t.Errorf("%s, src %d: rate %.0f, want %.2f x link = %.0f", tc.name, src, got, tc.frac, want)
			}
		}
	}
	// A demand below the gated rate passes whole.
	if got := PeerRate(1e9, xfLink, GPUClock{DataPath: DataPathMin}, xfGateOn, 0, 1); got != 1e9 {
		t.Errorf("forced min, demand 1e9: rate %.0f, want the demand", got)
	}
}

// XF03: the receiver decides and the source never does. Every ordered pair
// of four GPUs, each in a different state, gets the rate its receiver's
// state gives, whatever the other three are set to; swapping the two ends of
// a pair swaps which state counts.
func TestXF03SourceStateNeverChangesTheRate(t *testing.T) {
	states := []GPUClock{
		{DataPath: DataPathMin},
		{DataPath: DataPathMax},
		{DataPath: DataPathMid},
		{DataPath: DataPathMin, DataPathAuto: true},
	}
	rateInto := func(clocks []GPUClock, src, dst int) float64 {
		return PeerRate(3*xfLink, xfLink, clocks[dst], xfGateOn, src, dst)
	}
	for src := range states {
		for dst := range states {
			if src == dst {
				continue
			}
			want := rateInto(states, src, dst)
			for _, other := range states {
				clocks := make([]GPUClock, len(states))
				for i := range clocks {
					clocks[i] = other
				}
				clocks[dst] = states[dst]
				if got := rateInto(clocks, src, dst); got != want {
					t.Errorf("pair %d -> %d: rate %.0f with every other GPU at %+v, want %.0f", src, dst, got, other, want)
				}
			}
		}
	}
	if into1, into0 := rateInto(states, 0, 1), rateInto(states, 1, 0); into1 != xfLink || into0 != xfGateOn.SlowFraction*xfLink {
		t.Errorf("min GPU 0 to max GPU 1: %.0f, want the link; back: %.0f, want the slow fraction of it", into1, into0)
	}
}

// XF04: a receiver under auto that runs its own memory-active work has its
// data path raised to the maximum by its governor and gets the whole link
// from every source; idle under auto, the same receiver gets less from
// every source.
func TestXF04BusyReceiverUnderAutoGetsTheFullRate(t *testing.T) {
	busy := GPUClock{DataPath: DataPathMax, DataPathAuto: true}
	idle := GPUClock{DataPath: DataPathMin, DataPathAuto: true}
	for src := 0; src < 64; src++ {
		if got := PeerRate(3*xfLink, xfLink, busy, xfGateOn, src, 64); got != xfLink {
			t.Errorf("busy receiver, src %d: rate %.0f, want the link", src, got)
		}
		if got := PeerRate(3*xfLink, xfLink, idle, xfGateOn, src, 64); got >= xfLink {
			t.Errorf("idle receiver, src %d: rate %.0f, want below the link", src, got)
		}
	}
}

// XF05: an idle receiver under auto gets MidFraction or SlowFraction per
// pair, from a hash of (src, dst) and the gate's seed: the same pair always
// gets the same rate, and over 10000 pairs the share at MidFraction is
// within 2 points of MidPairFraction. Another seed draws other pairs.
func TestXF05MidPairsComeFromASeededHash(t *testing.T) {
	idle := GPUClock{DataPath: DataPathMin, DataPathAuto: true}
	for _, seed := range []uint64{0, 1, 42} {
		for _, share := range []float64{0.1, 0.5, 0.8} {
			gate := xfGateOn
			gate.Seed, gate.MidPairFraction = seed, share
			mid := 0
			for src := 0; src < 100; src++ {
				for dst := 0; dst < 100; dst++ {
					r := PeerRate(3*xfLink, xfLink, idle, gate, src, dst)
					if again := PeerRate(3*xfLink, xfLink, idle, gate, src, dst); again != r {
						t.Fatalf("seed %d pair %d -> %d: rate %.0f then %.0f", seed, src, dst, r, again)
					}
					switch r {
					case gate.MidFraction * xfLink:
						mid++
					case gate.SlowFraction * xfLink:
					default:
						t.Fatalf("seed %d pair %d -> %d: rate %.0f is neither the mid nor the slow fraction of the link", seed, src, dst, r)
					}
				}
			}
			if got := float64(mid) / 10000; math.Abs(got-share) > 0.02 {
				t.Errorf("seed %d: %.4f of the pairs at the mid fraction, want within 0.02 of %.2f", seed, got, share)
			}
		}
	}
	differ := 0
	for src := 0; src < 100; src++ {
		a, b := xfGateOn, xfGateOn
		b.Seed = a.Seed + 1
		if PeerRate(3*xfLink, xfLink, idle, a, src, 0) != PeerRate(3*xfLink, xfLink, idle, b, src, 0) {
			differ++
		}
	}
	if differ == 0 {
		t.Errorf("seeds %d and %d give every pair the same rate", xfGateOn.Seed, xfGateOn.Seed+1)
	}
}
