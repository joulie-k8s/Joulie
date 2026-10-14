package hwemu

import (
	"hash/fnv"

	"github.com/matbun/joulie/simulator/pkg/phys"
)

// The receiver gate of a node's peer transfers (phys.ReceiverGate): on when
// the profile's gpus.transfer.receiverGate.enabled says so, or as
// Options.ReceiverGate forces it, with the profile's fractions. A fraction
// the profile leaves unset lets the whole link through (midPairFraction:
// no pair at the mid rate), so forcing the gate on a profile without
// fractions slows nothing. The pair hash is seeded with the node name, so
// each node keeps its own fast and slow pairs from run to run.

// profileReceiverGate is the receiver gate of n, off without gpus.transfer.
func (n *Node) profileReceiverGate() phys.ReceiverGate {
	gs := n.profile.GPUs
	if gs == nil || gs.Transfer == nil {
		return phys.ReceiverGate{}
	}
	rg := gs.Transfer.ReceiverGate
	on := rg.Enabled.V
	switch n.receiverGate {
	case ReceiverGateOff:
		on = false
	case ReceiverGateOn:
		on = true
	}
	if !on {
		return phys.ReceiverGate{}
	}
	fraction := func(f Fact[float64], unset float64) float64 {
		if f.Src == "" {
			return unset
		}
		return f.V
	}
	h := fnv.New64a()
	h.Write([]byte(n.tree.NodeName))
	return phys.ReceiverGate{
		Enabled:         true,
		SlowFraction:    fraction(rg.SlowFraction, 1),
		MidFraction:     fraction(rg.MidFraction, 1),
		MidPairFraction: fraction(rg.MidPairFraction, 0),
		Seed:            h.Sum64(),
	}
}
