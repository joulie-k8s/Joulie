package phys

// The receiver gate: inbound peer copies bounded by the receiving GPU's
// data-path state. A demand-driven governor sees only the GPU's own work,
// never inbound DMA, so a receiver that runs nothing of its own stays at a
// low data-path level while a copy lands in it. The rate a cold receiver
// gives is a property of the (source, receiver) pair, not of the node.

func init() { receiverGateFraction = gatedFraction }

// gatedFraction is the fraction of the link recv lets through on the pair
// (src, dst) when the gate is on:
//
//   - 1 at DataPathMax, forced or reached by the auto governor while the
//     receiver runs its own memory-active work;
//   - SlowFraction when the data path is forced to DataPathMin, MidFraction
//     when forced to DataPathMid;
//   - an idle receiver under auto (DataPathAuto, below DataPathMax) gives
//     MidFraction on a MidPairFraction share of the pairs and SlowFraction on
//     the rest, the share chosen by gatePairUnit.
func gatedFraction(recv GPUClock, gate ReceiverGate, src, dst int) float64 {
	if recv.DataPath == DataPathMax {
		return 1
	}
	if recv.DataPathAuto {
		if gatePairUnit(gate.Seed, src, dst) < clamp01(gate.MidPairFraction) {
			return gate.MidFraction
		}
		return gate.SlowFraction
	}
	if recv.DataPath == DataPathMid {
		return gate.MidFraction
	}
	return gate.SlowFraction
}

// gatePairUnit maps the ordered pair (src, dst) and seed to a number in
// [0, 1) with the splitmix64 finalizer, so each pair keeps its rate for a
// seed and pairs are spread evenly over [0, 1).
func gatePairUnit(seed uint64, src, dst int) float64 {
	x := seed ^ (uint64(uint32(src))<<32 | uint64(uint32(dst)))
	x += 0x9e3779b97f4a7c15
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return float64(x>>11) / (1 << 53)
}
