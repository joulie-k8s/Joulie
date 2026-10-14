package phys

import "math"

// HostSource is the src of a transfer that does not come from a GPU of the
// node: host memory or a NIC (RDMA). Any negative src or dst means the same.
// Such a transfer is never gated: the gate bounds only copies between the
// node's GPUs (assumed).
const HostSource = -1

// PeerRate is the rate, in bytes per second, a copy from GPU src to GPU dst
// gets when it demands demandBps over a link of linkBps into a receiver at
// clock recv: min(demand, link x g), where g is the fraction of the link the
// receiver's data path lets through. g is 1 when the gate is off or when
// either end is not a GPU of the node (HostSource), and it never depends on
// the source's state, which this function does not see. A link of zero or
// less is a link the profile does not bound, and the copy gets its demand.
func PeerRate(demandBps, linkBps float64, recv GPUClock, gate ReceiverGate, src, dst int) float64 {
	if demandBps <= 0 {
		return 0
	}
	if linkBps <= 0 {
		return demandBps
	}
	g := 1.0
	if gate.Enabled && src >= 0 && dst >= 0 {
		g = clamp01(gatedFraction(recv, gate, src, dst))
	}
	return math.Min(demandBps, linkBps*g)
}
