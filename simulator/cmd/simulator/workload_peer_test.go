package main

import (
	"bytes"
	"log"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// peerTP is a fake node that also gives the rate of each peer transfer of
// its load: rates, whatever the load.
type peerTP struct {
	*fakeTP
	rates []float64
}

func (p *peerTP) PeerTransferRates() []float64 { return p.rates }

// peerJob is a v3 job record asking for gpus GPUs, with 1 CPU unit, 1e6 GPU
// units at util 1 and no CPU feed, and peerTransferIntensity p when p is
// not nil.
func peerJob(id string, gpus string, p *float64) string {
	return traceJob(id, func(r map[string]any) {
		r["podTemplate"] = map[string]any{"requests": map[string]any{"cpu": "1", "nvidia.com/gpu": gpus}}
		r["work"] = map[string]any{"cpuUnits": 1.0, "gpuUnits": 1e6}
		prof := map[string]any{"cpuUtilization": 0.2, "gpuUtilization": 1.0, "cpuFeedIntensityGpu": 0.0}
		if p != nil {
			prof["peerTransferIntensity"] = *p
		}
		r["workloadProfile"] = prof
	})
}

// XF12: a 4-GPU job with peerTransferIntensity 0.3 on a node with a 20 GB/s
// peer link loads it with a ring of 4 transfers, GPU k to k+1 and the last
// to GPU 0, each at 0.3 x 20 GB/s; with the node giving those transfers
// half their demand, its GPU work runs at 1 / (0.7 + 0.3 / 0.5) of the full
// rate. A 2-GPU job sends both ways, a 1-GPU job none.
func TestXF12MultiGPUJobSendsARingAtItsIntensity(t *testing.T) {
	const link = 20e9
	p := 0.3
	tp := &peerTP{fakeTP: &fakeTP{}, rates: []float64{3e9, 3e9, 3e9, 3e9}}
	node := fakeGPUNode(tp.fakeTP)
	node.TP, node.PeerLinkBps = tp, link
	lines := []string{peerJob("a", "4", &p), peerJob("b", "2", &p), peerJob("c", "1", &p)}
	h := newV1FakeHarness(t, fakeNodes{"gpu1": node}, lines, workload.RunOptions{})
	h.tick(0)
	h.step(1)
	h.runOn("a", "gpu1")
	h.step(2)

	h.run.mu.Lock()
	got := h.run.loads["gpu1"].PeerTransfers
	h.run.mu.Unlock()
	want := []hwemu.PeerTransfer{{Src: 0, Dst: 1, BytesPerS: p * link}, {Src: 1, Dst: 2, BytesPerS: p * link}, {Src: 2, Dst: 3, BytesPerS: p * link}, {Src: 3, Dst: 0, BytesPerS: p * link}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("load of the node running the 4-GPU job: peer transfers %+v, want %+v", got, want)
	}

	for sec := 3.0; sec <= 12; sec++ {
		h.step(sec)
	}
	if w, full := h.job("a").workGPU, 10*4.0; math.Abs(w-full/(0.7+0.3/0.5)) > 1e-6 {
		t.Errorf("4-GPU job with its transfers at half their demand did %v GPU units in 10 s, want %v", w, full/(0.7+0.3/0.5))
	}

	two, _ := h.run.peerRing([]workload.JobLoad{{ID: "b", GPUs: []workload.DeviceUse{{Device: 5, Units: 1}, {Device: 2, Units: 1}}}}, link)
	if want := []hwemu.PeerTransfer{{Src: 2, Dst: 5, BytesPerS: p * link}, {Src: 5, Dst: 2, BytesPerS: p * link}}; !reflect.DeepEqual(two, want) {
		t.Errorf("2-GPU job: transfers %+v, want %+v", two, want)
	}
	if one, _ := h.run.peerRing([]workload.JobLoad{{ID: "c", GPUs: []workload.DeviceUse{{Device: 3, Units: 1}}}}, link); one != nil {
		t.Errorf("1-GPU job: transfers %+v, want none", one)
	}
	if none, _ := h.run.peerRing([]workload.JobLoad{{ID: "b", GPUs: []workload.DeviceUse{{Device: 0, Units: 1}, {Device: 1, Units: 1}}}}, 0); none != nil {
		t.Errorf("node without a peer link: transfers %+v, want none", none)
	}
}

// XF11 (v1 engine): a 4-GPU job whose record leaves peerTransferIntensity
// out, or gives 0, loads its node with no peer transfer and runs at the
// full rate, even on a node whose transfers would get half their demand.
func TestXF11ZeroPeerTransferIntensityLoadsNoTransfers(t *testing.T) {
	zero := 0.0
	for name, p := range map[string]*float64{"absent": nil, "zero": &zero} {
		t.Run(name, func(t *testing.T) {
			tp := &peerTP{fakeTP: &fakeTP{}, rates: []float64{3e9, 3e9, 3e9, 3e9}}
			node := fakeGPUNode(tp.fakeTP)
			node.TP, node.PeerLinkBps = tp, 20e9
			h := newV1FakeHarness(t, fakeNodes{"gpu1": node}, []string{peerJob("a", "4", p)}, workload.RunOptions{})
			h.tick(0)
			h.step(1)
			h.runOn("a", "gpu1")
			for sec := 2.0; sec <= 12; sec++ {
				h.step(sec)
			}
			h.run.mu.Lock()
			l, loaded := h.run.loads["gpu1"]
			h.run.mu.Unlock()
			if !loaded || l.PeerTransfers != nil {
				t.Errorf("load %+v (loaded %v), want one with no peer transfers", l, loaded)
			}
			if w := h.job("a").workGPU; math.Abs(w-40) > 1e-6 {
				t.Errorf("did %v GPU units in 10 s, want the full 40", w)
			}
		})
	}
}

// syncLog is a log destination tests read while other goroutines may log.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// XF14 (parse): SIM_GPU_RECEIVER_GATE off, on and profile are read as
// they are, unset or empty is profile, and any other value warns, naming
// the variable, and is profile; it never stops the start.
// TestXF14ReceiverGateActsOnTheRun checks that each value reaches the
// emulated nodes.
func TestXF14ReceiverGateParses(t *testing.T) {
	s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 10, prometheus.NewRegistry())
	logs := &syncLog{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	for _, tc := range []struct {
		set        bool
		value      string
		want, warn string
	}{
		{false, "", hwemu.ReceiverGateProfile, ""},
		{true, "", hwemu.ReceiverGateProfile, ""},
		{true, "off", hwemu.ReceiverGateOff, ""},
		{true, " on ", hwemu.ReceiverGateOn, ""},
		{true, "profile", hwemu.ReceiverGateProfile, ""},
		{true, "ON", hwemu.ReceiverGateProfile, `SIM_GPU_RECEIVER_GATE="ON" is not off, on or profile; using profile`},
		{true, "auto", hwemu.ReceiverGateProfile, `SIM_GPU_RECEIVER_GATE="auto" is not off, on or profile; using profile`},
	} {
		before := len(logs.String())
		env := map[string]string{envClusterSpec: "/s.yaml"}
		if tc.set {
			env[envGPUReceiverGate] = tc.value
		}
		o, err := s.emulateOptionsFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
		if err != nil {
			t.Fatalf("%q: %v", tc.value, err)
		}
		if o.ReceiverGate != tc.want {
			t.Errorf("%s=%q: gate %q, want %q", envGPUReceiverGate, tc.value, o.ReceiverGate, tc.want)
		}
		logged := logs.String()[before:]
		if tc.warn == "" && strings.Contains(logged, envGPUReceiverGate) {
			t.Errorf("%s=%q: logged %q, want no warning", envGPUReceiverGate, tc.value, logged)
		}
		if tc.warn != "" && !strings.Contains(logged, "warning: "+tc.warn) {
			t.Errorf("%s=%q: logged %q, want the warning %q", envGPUReceiverGate, tc.value, logged, tc.warn)
		}
	}
}
