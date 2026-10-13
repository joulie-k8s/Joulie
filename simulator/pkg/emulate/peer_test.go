package emulate

import (
	"reflect"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// XF12 (emulator): a job load's peer transfers reach the member's runner as
// they are, in order, with or without the base load, and a change of the
// transfers alone sets the load again, so the rates the node gives back
// follow the transfers of the step. The base load makes none, and a member
// without GPUs gets none.
func TestXF12EmulatorPassesPeerTransfersToTheRunner(t *testing.T) {
	f := newEmuFakes()
	gpu := emuMember("gpu", "", "", 1, 4)
	gpu.BaseLoad = clusterspec.Load{GPUUtil: emuFloat(0.2)}
	cpu := emuMember("cpu", "", "", 1, 0)
	e := emuNew(t, emuConfig(t.TempDir(), f, &emuLog{}, gpu, cpu))

	ring := []hwemu.PeerTransfer{{Src: 0, Dst: 1, BytesPerS: 6e9}, {Src: 1, Dst: 2, BytesPerS: 6e9}, {Src: 2, Dst: 3, BytesPerS: 6e9}, {Src: 3, Dst: 0, BytesPerS: 6e9}}
	job := hwemu.Load{GPUUtil: []float64{1, 1, 1, 1}, PeerTransfers: ring}
	for i, applyBase := range []bool{false, true} {
		emuTick(t, e, time.Duration(i+1)*time.Second, map[string]hwemu.Load{"gpu": job, "cpu": job}, applyBase, nil)
		if got := f.runner(t, "gpu").lastLoad(t).PeerTransfers; !reflect.DeepEqual(got, ring) {
			t.Errorf("base load %v: GPU member got transfers %+v, want %+v", applyBase, got, ring)
		}
		if got := f.runner(t, "cpu").lastLoad(t).PeerTransfers; got != nil {
			t.Errorf("base load %v: member without GPUs got transfers %+v, want none", applyBase, got)
		}
	}
	emuTick(t, e, 3*time.Second, nil, true, nil)
	if got := f.runner(t, "gpu").lastLoad(t).PeerTransfers; got != nil {
		t.Errorf("base load alone: transfers %+v, want none", got)
	}

	r := f.runner(t, "gpu")
	r.mu.Lock()
	before := len(r.loads)
	r.mu.Unlock()
	slower := job
	slower.PeerTransfers = append([]hwemu.PeerTransfer(nil), ring...)
	slower.PeerTransfers[2].BytesPerS = 3e9
	emuTick(t, e, 4*time.Second, map[string]hwemu.Load{"gpu": job}, false, nil)
	emuTick(t, e, 5*time.Second, map[string]hwemu.Load{"gpu": job}, false, nil)
	emuTick(t, e, 6*time.Second, map[string]hwemu.Load{"gpu": slower}, false, nil)
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := len(r.loads) - before; n != 2 {
		t.Errorf("%d SetLoad calls over a new load, the same again and one with one transfer changed, want 2", n)
	}
	if got := r.loads[len(r.loads)-1].PeerTransfers; !reflect.DeepEqual(got, slower.PeerTransfers) {
		t.Errorf("last transfers %+v, want %+v", got, slower.PeerTransfers)
	}
}

// XF14 (emulator): Config.ReceiverGate is every member's
// hwemu.Options.ReceiverGate, empty when unset.
func TestXF14ConfigReceiverGateReachesEveryRunner(t *testing.T) {
	for _, gate := range []string{"", hwemu.ReceiverGateOff, hwemu.ReceiverGateOn, hwemu.ReceiverGateProfile} {
		f := newEmuFakes()
		c := emuConfig(t.TempDir(), f, &emuLog{}, emuMember("a", "", "", 1, 2), emuMember("b", "", "", 1, 0))
		c.ReceiverGate = gate
		emuNew(t, c)
		for _, name := range []string{"a", "b"} {
			if got := f.runner(t, name).opts.ReceiverGate; got != gate {
				t.Errorf("Config.ReceiverGate %q: runner %s got %q", gate, name, got)
			}
		}
	}
}
