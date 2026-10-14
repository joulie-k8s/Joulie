package hwemu

import (
	"math"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/phys"
)

// gateNode starts a node of testdata/gate-test-gpu.yaml, the W7900 node with
// the receiver gate on (slowFraction 0.5, every idle receiver slow), cut to
// two cores per socket.
func gateNode(t *testing.T) (*Node, *Tree, *Profile) {
	t.Helper()
	profiles, local, err := LoadProfileSet("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != 1 || local[0] != "gate-test-gpu" {
		t.Fatalf("local test profiles %v, want gate-test-gpu", local)
	}
	p := profiles["gate-test-gpu"]
	p.CPU.CoresPerSocket = Fact[int]{V: 2, Src: nodeSmallTopology}
	p.Node.Capacity.CPU = Fact[int64]{V: int64(p.CPU.Sockets.V * 2 * p.CPU.ThreadsPerCore.V), Src: nodeSmallTopology}
	if tr := p.GPUs.Transfer; tr == nil || !tr.ReceiverGate.Enabled.V || tr.ReceiverGate.MidPairFraction.V != 0 {
		t.Fatalf("gate-test-gpu has transfer %+v; the tests need the gate on with no mid pairs", tr)
	}
	tr := renderTestTree(t, p)
	return nodeNewNode(t, p, tr), tr, p
}

// gateRate is the rate of transfer i of the node's load, in bytes per
// second.
func gateRate(t *testing.T, n *Node, i int) float64 {
	t.Helper()
	r := n.PeerTransferRates()
	if i >= len(r) {
		t.Fatalf("%d transfer rates, want at least %d", len(r), i+1)
	}
	return r[i]
}

// XF07 (gate code): with the gate on, a peer copy into an idle receiver
// under auto gets slowFraction of the link; with the receiver at high,
// whose data path is held at its maximum, the whole link; with only the
// source at high, still the slow rate, since the source's state never
// counts; and with the receiver running memory-active work of its own
// under auto, which raises its data path, the whole link.
func TestXF07ReceiverGateFollowsTheReceiverOnly(t *testing.T) {
	t.Parallel()
	n, tr, p := gateNode(t)
	link := p.GPUs.Transfer.PeerLinkGBps.V * 1e9
	slow := link * p.GPUs.Transfer.ReceiverGate.SlowFraction.V
	copy01 := Load{PeerTransfers: []PeerTransfer{{Src: 0, Dst: 1, BytesPerS: 1.5 * link}}}
	n.SetLoad(copy01)
	nodeStep(t, n, 2*time.Second)
	check := func(when string, want float64) {
		t.Helper()
		if got := gateRate(t, n, 0); math.Abs(got-want) > 1e-3 {
			t.Errorf("%s: copy 0 to 1 at %v B/s, want %v B/s", when, got, want)
		}
	}
	check("receiver idle under auto", slow)
	dpmSetLevel(t, tr, n, 1, "high")
	check("receiver at high", link)
	dpmSetLevel(t, tr, n, 1, "auto")
	dpmSetLevel(t, tr, n, 0, "high")
	check("only the source at high", slow)

	busy := copy01
	busy.GPUUtil, busy.MemoryIntensity = []float64{0, 1}, 0.5
	n.SetLoad(busy)
	nodeStep(t, n, time.Second)
	if c := n.gpus[1].clock; c.dataPath != phys.DataPathMax || !c.dataPathAuto {
		t.Fatalf("receiver running memory-active work under auto: clock state %+v, want its data path raised", c)
	}
	check("receiver running its own memory-active work under auto", link)
}

// XF08 (gate code): a transfer from a NIC or host memory into a GPU, or from
// a GPU out to the host, is never gated: it gets its demand whatever the
// receiver's level, while a peer copy into the same GPU slows at auto and at
// low.
func TestXF08NICTransfersKeepTheFullRateAtEveryLevel(t *testing.T) {
	t.Parallel()
	n, tr, p := gateNode(t)
	link := p.GPUs.Transfer.PeerLinkGBps.V * 1e9
	const nic = 15e9
	n.SetLoad(Load{PeerTransfers: []PeerTransfer{
		{Src: phys.HostSource, Dst: 1, BytesPerS: nic},
		{Src: 1, Dst: phys.HostSource, BytesPerS: nic},
		{Src: 0, Dst: 1, BytesPerS: link},
	}})
	nodeStep(t, n, 2*time.Second)
	for _, level := range []string{"auto", "low", "high", "auto"} {
		dpmSetLevel(t, tr, n, 1, level)
		r := n.PeerTransferRates()
		if r[0] != nic || r[1] != nic {
			t.Errorf("GPU 1 at %s: NIC to GPU %v B/s, GPU to host %v B/s, want both %v", level, r[0], r[1], nic)
		}
		if gated := level != "high"; gated != (r[2] < link) {
			t.Errorf("GPU 1 at %s: peer copy into it at %v B/s of a %v B/s link, gated %v", level, r[2], link, gated)
		}
	}
}

// The gate bounds what a receiver takes in, not each copy alone: two copies
// into one idle receiver under auto, each asking half the link, share
// slowFraction of it, and at high, the data path held, they share the whole
// link. Bounding each copy alone let two sources together through a cold
// receiver at the full link.
func TestReceiverGateBoundsTheTotalIntoOneReceiver(t *testing.T) {
	t.Parallel()
	n, tr, p := gateNode(t)
	link := p.GPUs.Transfer.PeerLinkGBps.V * 1e9
	slow := p.GPUs.Transfer.ReceiverGate.SlowFraction.V
	n.SetLoad(Load{PeerTransfers: []PeerTransfer{{Src: 0, Dst: 1, BytesPerS: link / 2}, {Src: 2, Dst: 1, BytesPerS: link / 2}}})
	nodeStep(t, n, 2*time.Second)
	for _, step := range []struct {
		level string
		want  float64
	}{
		{"auto", link * slow / 2},
		{"high", link / 2},
	} {
		dpmSetLevel(t, tr, n, 1, step.level)
		r := n.PeerTransferRates()
		if len(r) != 2 || math.Abs(r[0]-step.want) > 1e-3 || math.Abs(r[1]-step.want) > 1e-3 {
			t.Errorf("GPU 1 at %s, copies from GPUs 0 and 2 at %v B/s each: rates %v, want %v each", step.level, link/2, r, step.want)
		}
	}
}
