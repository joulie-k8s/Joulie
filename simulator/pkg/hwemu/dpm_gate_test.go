package hwemu

import (
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/phys"
)

// GD06 (receiver gate): under auto a GPU that only receives peer copies
// runs no work of its own, so its data path stays at its minimum and the
// fclk star does not move, with the receiver gate on; the sender's own copy
// work is not memory-active work of the receiver.
func TestGD06InboundTransfersDoNotRaiseTheReceiver(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, w7900Profile)
	tr := renderTestTree(t, p)
	n, err := NewNode(p, tr, Options{Start: nodeStart, ReceiverGate: ReceiverGateOn})
	if err != nil {
		t.Fatal(err)
	}
	lo := itoa(p.GPUs.DPM.Tables.Fclk.V[0])
	n.SetLoad(Load{PeerTransfers: []PeerTransfer{{Src: 0, Dst: 1, BytesPerS: 25e9}, {Src: 2, Dst: 1, BytesPerS: 25e9}}})
	for k := 0; k < 5; k++ {
		nodeStep(t, n, time.Second)
		if got := dpmStar(t, tr, 1, "pp_dpm_fclk"); got != lo {
			t.Fatalf("after %d s of inbound copies: fclk star %s, want it at the minimum %s", k+1, got, lo)
		}
		if c := n.gpus[1].clock; c.dataPath != phys.DataPathMin || !c.dataPathAuto {
			t.Fatalf("after %d s of inbound copies: clock state %+v, want the data path at its minimum, following demand", k+1, c)
		}
	}
}
