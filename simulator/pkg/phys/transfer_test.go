package phys

import "testing"

// xfLink is the peer link of the tests, in bytes per second, a neutral value.
const xfLink = 20e9

// xfReceivers are receivers in every data-path state.
var xfReceivers = map[string]GPUClock{
	"forced max":    {DataPath: DataPathMax},
	"forced mid":    {DataPath: DataPathMid},
	"forced min":    {DataPath: DataPathMin},
	"auto, busy":    {DataPath: DataPathMax, DataPathAuto: true},
	"auto, idle":    {DataPath: DataPathMin, DataPathAuto: true},
	"auto, cooling": {DataPath: DataPathMid, DataPathAuto: true},
}

// xfGateOn is an enabled gate with neutral fractions.
var xfGateOn = ReceiverGate{Enabled: true, SlowFraction: 0.4, MidFraction: 0.7, MidPairFraction: 0.5, Seed: 7}

// XF01: with the gate off, a copy gets min(demand, link) whatever the
// receiver's state, even when the gate's fractions are set. No demand gets
// nothing, and a link of zero (one the profile does not bound) passes the
// demand.
func TestXF01GateOffRateIsDemandBoundedByTheLink(t *testing.T) {
	off := xfGateOn
	off.Enabled = false
	for name, recv := range xfReceivers {
		for _, gate := range []ReceiverGate{{}, off} {
			for _, demand := range []float64{1e9, xfLink, 3 * xfLink} {
				want := min(demand, xfLink)
				if got := PeerRate(demand, xfLink, recv, gate, 0, 1); got != want {
					t.Errorf("receiver %s, gate %+v, demand %.0f: rate %.0f, want %.0f", name, gate, demand, got, want)
				}
			}
			if got := PeerRate(0, xfLink, recv, gate, 0, 1); got != 0 {
				t.Errorf("receiver %s: no demand gets %.0f, want 0", name, got)
			}
			if got := PeerRate(3*xfLink, 0, recv, gate, 0, 1); got != 3*xfLink {
				t.Errorf("receiver %s: unbounded link gives %.0f, want the demand %.0f", name, got, 3*xfLink)
			}
		}
	}
}

// XF06: a copy from or to a NIC or the host (HostSource, any negative end)
// is never gated: with the gate on it gets min(demand, link) into a receiver
// in every state.
func TestXF06HostAndNICTransfersAreNeverGated(t *testing.T) {
	for name, recv := range xfReceivers {
		for _, ends := range [][2]int{{HostSource, 0}, {HostSource, 3}, {-2, 1}, {2, HostSource}} {
			if got := PeerRate(3*xfLink, xfLink, recv, xfGateOn, ends[0], ends[1]); got != xfLink {
				t.Errorf("receiver %s, src %d dst %d: rate %.0f, want the link %.0f", name, ends[0], ends[1], got, float64(xfLink))
			}
		}
	}
}
