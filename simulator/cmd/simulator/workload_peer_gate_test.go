package main

// Gate code: these tests drive the receiver gate of the test profiles in
// testdata/gate-profiles through the v1 engine. They stay with the gate
// files.

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// gateSpec is three W7900 hosts with no base load: two with the receiver
// gate on in their profile (slowFraction 0.5, every idle receiver slow,
// 20 GB/s link), one whose profile leaves the same gate off.
const gateSpec = `apiVersion: sim.joulie.io/v1alpha1
kind: ClusterSpec
metadata: {name: v1gate}
spec:
  facility: {ambientC: 20, amplitudeC: 0, periodH: 24}
  pools:
  - {name: auto, profile: gate-on-test-gpu, count: 1}
  - {name: high, profile: gate-on-test-gpu, count: 1}
  - {name: dormant, profile: gate-off-test-gpu, count: 1}
`

// gatePeer is the peerTransferIntensity of the gate tests' jobs.
const gatePeer = 0.6

// gateFactor is the share of the full GPU rate a 4-GPU job keeps on a
// gate-on host under auto: each GPU receives one ring copy demanding
// 0.6 x link through a receiver that lets 0.5 x link in, so the copies get
// g = 0.5 / 0.6 of their demand, and the job runs at 1 / ((1 - p) + p / g).
var gateFactor = 1 / ((1 - gatePeer) + gatePeer/(0.5/gatePeer))

// newGateEmulateTest is newEmulateTest over gateSpec, with the profiles of
// testdata/gate-profiles and SIM_GPU_RECEIVER_GATE read as the simulator
// reads it.
func newGateEmulateTest(t *testing.T, gateEnv string) *emulateTest {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "gate-profiles"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envProfilesDir, dir)
	path := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(path, []byte(gateSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 1000, reg)
	s.facilityBaseAmbientTempC, s.facilityTempAmplitudeC, s.facilityTempPeriodH = 20, 5, 24
	root, fileDir := filepath.Join(t.TempDir(), "hwemu"), t.TempDir()
	rt, err := s.newEmulateRuntime(emulateOptions{
		SpecPath:     path,
		Root:         root,
		Tick:         time.Second,
		Telemetry:    telemetry.Config{FileDir: fileDir},
		Resource:     telemetry.Resource{ServiceName: telemetry.DefaultServiceName, Instance: "sim-test"},
		Start:        emulateTestStart,
		Registerer:   reg,
		ReceiverGate: receiverGateFromEnv(gateEnv, func(string) {}),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.emu = rt
	et := &emulateTest{s: s, rt: rt, reg: reg, root: root, fileDir: fileDir}
	t.Cleanup(et.close)
	return et
}

// gateJob is a v3 record of a job on all four GPUs of a host, compute
// bound with no memory activity of its own, so under auto its GPUs' data
// paths stay down: 1 CPU unit and gpuUnits GPU units, and
// peerTransferIntensity 0.6.
func gateJob(id string, gpuUnits float64) string {
	return traceJob(id, func(r map[string]any) {
		r["podTemplate"] = map[string]any{"requests": map[string]any{"cpu": "1", "amd.com/gpu": "4"}}
		r["work"] = map[string]any{"cpuUnits": 1.0, "gpuUnits": gpuUnits}
		r["workloadClass"] = map[string]any{"gpu": "gpu.compute_bound"}
		r["workloadProfile"] = map[string]any{
			"cpuUtilization": 0.2, "gpuUtilization": 1.0, "memoryIntensity": 0.0, "cpuFeedIntensityGpu": 0.0,
			"peerTransferIntensity": gatePeer,
		}
	})
}

// gateSetLevel sets every GPU of node to level with rocm-smi, as root.
func gateSetLevel(t *testing.T, et *emulateTest, node, level string) {
	t.Helper()
	runner := smi.Runner{Env: smi.Env{SysRoot: filepath.Join(et.root, node, layout.SysDir), StateRoot: filepath.Join(et.root, node, layout.StateDir)}}
	for d := 0; d < 4; d++ {
		if out, err := runner.Run(context.Background(), "rocm-smi", "-d", fmt.Sprint(d), "--setperflevel", level); err != nil {
			t.Fatalf("%s: rocm-smi -d %d --setperflevel %s: %v (%q)", node, d, level, err, out)
		}
	}
}

// XF13 (gate code): on the gated test profile a 4-GPU job with
// peerTransferIntensity 0.6 runs at gateFactor of the rate of the same job
// on a host whose GPUs are at high, where the data paths are held up and
// the copies get their demand, and so finishes later, by 1 / gateFactor of
// the time the job at high takes.
func TestXF13GatedJobFinishesLaterUnderAutoThanUnderHigh(t *testing.T) {
	et := newGateEmulateTest(t, "")
	auto, high := et.node(t, "auto"), et.node(t, "high")
	const units = 80.0 // 20 s at high: four GPUs at base speed 1
	h := newV1Harness(t, et.s, nil, et, []string{gateJob("a", units), gateJob("h", units)}, workload.RunOptions{})
	gateSetLevel(t, et, high, "high")
	h.step(0)
	h.step(1)
	h.runOn("a", auto)
	h.runOn("h", high)
	h.step(2)
	for sec := 3.0; sec <= 8; sec++ {
		h.step(sec)
	}
	a0, h0 := h.job("a").workGPU, h.job("h").workGPU
	for sec := 9.0; sec <= 13; sec++ {
		h.step(sec)
	}
	da, dh := h.job("a").workGPU-a0, h.job("h").workGPU-h0
	if math.Abs(dh-5*4) > 1e-6 {
		t.Errorf("job at high did %v GPU units in 5 s, want the full 20", dh)
	}
	if math.Abs(da/dh-gateFactor) > 1e-9 {
		t.Errorf("job under auto did %v GPU units in 5 s, the job at high %v: ratio %v, want %v", da, dh, da/dh, gateFactor)
	}
	for sec := 14.0; sec <= 40 && !(h.job("a").state >= v1Completing && h.job("h").state >= v1Completing); sec++ {
		h.step(sec)
	}
	ja, jh := h.job("a"), h.job("h")
	if ja.doneAt.IsZero() || jh.doneAt.IsZero() {
		t.Fatalf("by 40 s the job under auto is done %v, the job at high %v; want both", !ja.doneAt.IsZero(), !jh.doneAt.IsZero())
	}
	ta, th := ja.doneAt.Sub(ja.runningAt).Seconds(), jh.doneAt.Sub(jh.runningAt).Seconds()
	t.Logf("done after %v s under auto, %v s at high (ratio %.3f, formula %.3f)", ta, th, ta/th, 1/gateFactor)
	if !(ta > th) || math.Abs(ta-th/gateFactor) > 1 {
		t.Errorf("done after %v s under auto and %v s at high; want later under auto, by %v s within a tick", ta, th, th/gateFactor)
	}
}

// XF14 (act; gate code): SIM_GPU_RECEIVER_GATE reaches every emulated
// node. With profile, unset and an invalid value, the gate is on only where
// the profile turns it on; with on, also on the host whose profile leaves
// it off; with off, nowhere. A gated job runs at gateFactor, an ungated one
// at the full rate.
func TestXF14ReceiverGateActsOnTheRun(t *testing.T) {
	for _, tc := range []struct {
		env           string
		auto, dormant float64
	}{
		{"", gateFactor, 1},
		{hwemu.ReceiverGateProfile, gateFactor, 1},
		{"bogus", gateFactor, 1},
		{hwemu.ReceiverGateOn, gateFactor, gateFactor},
		{hwemu.ReceiverGateOff, 1, 1},
	} {
		t.Run(fmt.Sprintf("%q", tc.env), func(t *testing.T) {
			et := newGateEmulateTest(t, tc.env)
			auto, dormant := et.node(t, "auto"), et.node(t, "dormant")
			h := newV1Harness(t, et.s, nil, et, []string{gateJob("a", 1e6), gateJob("d", 1e6)}, workload.RunOptions{})
			h.step(0)
			h.step(1)
			h.runOn("a", auto)
			h.runOn("d", dormant)
			for sec := 2.0; sec <= 5; sec++ {
				h.step(sec)
			}
			a0, d0 := h.job("a").workGPU, h.job("d").workGPU
			for sec := 6.0; sec <= 10; sec++ {
				h.step(sec)
			}
			if got := (h.job("a").workGPU - a0) / 20; math.Abs(got-tc.auto) > 1e-9 {
				t.Errorf("gate-on host: job at %v of the full rate, want %v", got, tc.auto)
			}
			if got := (h.job("d").workGPU - d0) / 20; math.Abs(got-tc.dormant) > 1e-9 {
				t.Errorf("gate-off host: job at %v of the full rate, want %v", got, tc.dormant)
			}
		})
	}
}
