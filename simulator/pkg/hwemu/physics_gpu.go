package hwemu

import (
	"math"

	"github.com/matbun/joulie/simulator/pkg/phys"
)

// The GPU clocks and the peer link of the physics: a GPU's clock state as
// the phys clock model takes it, its board power through that model, and
// the rates of the peer transfers of Load.PeerTransfers.

// gpuPhysClock is g's clock state as the phys clock model takes it: the
// governor's compute ceiling (none when it is at or above the highest clock
// the GPU sustains), the data-path level and the gating flag. own asks for
// the view of the GPU's own work: a data path that follows demand rises for
// the GPU's own kernels, so its own work always meets it at its maximum;
// only inbound peer transfers, which the governor does not see, meet the
// level it holds. A GPU without a clock model gives the zero GPUClock,
// which the phys functions read as no model.
func (n *Node) gpuPhysClock(g *nodeGPU, own bool) phys.GPUClock {
	c := &g.clock
	if !c.modelled || c.computeMaxMHz <= 0 {
		return phys.GPUClock{}
	}
	top := c.computeMaxMHz
	if v := g.nvclock.spec; v != nil {
		if s := float64(v.SM.SustainedMaxMHz.V); s > 0 && s < top {
			top = s
		}
	}
	out := phys.GPUClock{ComputeMaxMHz: top, DataPath: c.dataPath, DataPathAuto: c.dataPathAuto, GatingOff: c.gatingOff}
	if ceil := c.computeCeilMHz; ceil > 0 && ceil < top {
		out.ComputeMHz = ceil
	}
	if own && c.dataPathAuto {
		out.DataPath = phys.DataPathMax
	}
	return out
}

// gpuClockFacts are the profile's physics.gpu clock parameters.
func (n *Node) gpuClockFacts() phys.GPUClockFacts {
	g := &n.profile.Physics.GPU
	return phys.GPUClockFacts{
		ClockBeta:       g.ClockBeta.V,
		MemoryKneeFrac:  g.MemoryKneeFrac.V,
		MemoryKneeGamma: g.MemoryKneeGamma.V,
		DataPathHoldW:   g.DataPathHoldW.V,
		GatingOffIdleW:  g.GatingOffIdleW.V,
		DataPathGamma:   g.DataPathGamma.V,
	}
}

// gpuPowerW is g's board power running s: the GPU model's, through the
// clock model when g has one (phys.GPUPowerWithClock), which at the maximum
// compute clock, the data path at its maximum and idle gating kept is the
// same.
func (n *Node) gpuPowerW(g *nodeGPU, s phys.DeviceState) float64 {
	m, ok := n.gpuModel.(phys.CappedBoardGPUModel)
	clk := n.gpuPhysClock(g, false)
	if !ok || clk.ComputeMaxMHz <= 0 {
		return n.gpuModel.Power(s)
	}
	return phys.GPUPowerWithClock(m, s, clk, n.gpuClockFacts())
}

// gpuByIndex is the GPU with index i, nil when the node has none.
func (n *Node) gpuByIndex(i int) *nodeGPU {
	for _, g := range n.gpus {
		if g.index == i {
			return g
		}
	}
	return nil
}

// isPeer reports whether t is a peer transfer: a copy between two distinct
// GPUs of the node, over the peer link.
func (n *Node) isPeer(t PeerTransfer) bool {
	return t.Src != t.Dst && n.gpuByIndex(t.Src) != nil && n.gpuByIndex(t.Dst) != nil
}

// peerRates is the rate, in bytes per second, each transfer of
// Load.PeerTransfers gets, in order. A peer transfer alone gets phys.PeerRate
// over gpus.transfer.peerLinkGBps (GB/s, 1e9 bytes) into a receiver at its
// clock state, through the receiver gate: at most link x g, the fraction g
// the receiver's data path lets through from that source. The copies into
// one GPU share its link by time: each needs its demand over link x g of
// the link's time, and when together they need more than all of it, each
// gets its demand over that total, so a gated receiver takes in at most
// link x g whatever the number of sources. Any other transfer, with a host
// or NIC end (a negative Src or Dst, phys.HostSource) or naming no two GPUs
// of the node, is not on the peer link and gets its demand, as does every
// transfer on a profile without gpus.transfer, which does not model the
// link.
func (n *Node) peerRates() []float64 {
	ts := n.load.PeerTransfers
	if len(ts) == 0 {
		return nil
	}
	out := make([]float64, len(ts))
	var link float64
	if gs := n.profile.GPUs; gs != nil && gs.Transfer != nil {
		link = gs.Transfer.PeerLinkGBps.V * 1e9
	}
	gate := n.profileReceiverGate()
	busy := map[int]float64{} // the share of each receiver's link time the copies into it need
	for i, t := range ts {
		if !n.isPeer(t) || link <= 0 {
			out[i] = phys.PeerRate(t.BytesPerS, 0, phys.GPUClock{}, phys.ReceiverGate{}, phys.HostSource, phys.HostSource)
			continue
		}
		clk := n.gpuPhysClock(n.gpuByIndex(t.Dst), false)
		out[i] = phys.PeerRate(t.BytesPerS, link, clk, gate, t.Src, t.Dst)
		if pair := phys.PeerRate(math.Inf(1), link, clk, gate, t.Src, t.Dst); pair > 0 && t.BytesPerS > 0 {
			busy[t.Dst] += t.BytesPerS / pair
		}
	}
	for i, t := range ts {
		if b := busy[t.Dst]; b > 1 && link > 0 && t.BytesPerS > 0 && n.isPeer(t) {
			out[i] = t.BytesPerS / b
		}
	}
	return out
}

// derivePeers sets each GPU's next peer receive and send rates from
// peerRates. Only peer transfers count, and only on a profile with
// gpus.transfer: State's PeerRxBytes and PeerTxBytes count the peer link.
func (n *Node) derivePeers() {
	for _, g := range n.gpus {
		g.nextRx, g.nextTx = 0, 0
	}
	if gs := n.profile.GPUs; gs == nil || gs.Transfer == nil {
		return
	}
	for i, r := range n.peerRates() {
		if t := n.load.PeerTransfers[i]; n.isPeer(t) {
			n.gpuByIndex(t.Dst).nextRx += r
			n.gpuByIndex(t.Src).nextTx += r
		}
	}
}
