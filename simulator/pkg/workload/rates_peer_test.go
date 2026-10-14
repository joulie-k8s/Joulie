package workload_test

import (
	"math"
	"math/rand"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// XF11 (rates): a job with no peer transfers, PeerTransferIntensity 0, the
// zero value of every existing caller, gets the rates it got before the
// field existed, bit for bit, in both models and whatever PeerMul holds:
// the rates below are the formulas of Rates without the peer term.
func TestXF11ZeroPeerTransferIntensityKeepsTheRates(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for k := 0; k < 2000; k++ {
		in := workload.RateInputs{
			BaseSpeed: 0.5 + 2*rng.Float64(), CPURequest: 8 * rng.Float64(), GPURequest: 4 * rng.Float64(),
			CPUUtil: rng.Float64(), MemoryIntensity: rng.Float64(), IOIntensity: rng.Float64(), CPUFeedIntensity: rng.Float64(),
			SensitivityCPU: rng.Float64(), SensitivityGPU: rng.Float64(),
			CPUMul: rng.Float64(), GPUMul: rng.Float64(),
			CPUShare: rng.Float64(), GPUShare: rng.Float64(),
		}
		for _, model := range []workload.Model{workload.ModelV1, workload.ModelLegacy} {
			cpu0, gpu0 := workload.Rates(model, in)
			feed := 1.0
			if in.CPUFeedIntensity > 0 {
				feed = math.Max(0, math.Min(1, 1.0-(1.0-in.CPUMul)*in.CPUFeedIntensity*in.SensitivityCPU))
			}
			want := math.Max(0.1, in.GPURequest) * in.BaseSpeed * in.GPUMul * feed * in.GPUShare
			if model != workload.ModelLegacy {
				want = math.Max(0.1, in.GPURequest) * in.BaseSpeed * (1.0 - in.SensitivityGPU*(1.0-in.GPUMul)) * feed * in.GPUShare
			}
			if gpu0 != math.Max(0, want) {
				t.Fatalf("%s %+v: GPU rate %v, want %v", model, in, gpu0, want)
			}
			with := in
			with.PeerMul = rng.Float64()
			if cpu, gpu := workload.Rates(model, with); cpu != cpu0 || gpu != gpu0 {
				t.Fatalf("%s %+v: PeerMul %v with no transfers moved the rates from (%v, %v) to (%v, %v)", model, in, with.PeerMul, cpu0, gpu0, cpu, gpu)
			}
		}
	}
}

// The peer term of Rates: a v1 job that spends p of its GPU time on
// transfers delivered at g of their demand runs at
// 1 / ((1 - p) / thr + p / g) of its throughput factor thr; a cap slows
// only the share spent on work, transfers at their demand cost nothing,
// and ModelLegacy ignores the term. The CPU rate never moves.
func TestRatesPeerTransfersFollowTheJobProgressFormula(t *testing.T) {
	in := fullSpeed()
	in.CPUFeedIntensity = 0
	cpu1, full := workload.Rates(workload.ModelV1, in)
	for _, tc := range []struct{ p, g, gpuMul, want float64 }{
		{0.6, 1, 1, 1},
		{0.6, 0.5 / 0.6, 1, 1 / (0.4 + 0.6*0.6/0.5)},
		{0.3, 0.5, 0.8, 1 / (0.7/0.8 + 0.3/0.5)},
		{0.3, 1, 0.8, 1 / (0.7/0.8 + 0.3)},
		{1, 0.25, 0.8, 0.25},
		{0.5, 0, 1, 0},
		{0.5, 1, 0, 0},
	} {
		in := in
		in.PeerTransferIntensity, in.PeerMul, in.GPUMul = tc.p, tc.g, tc.gpuMul
		cpu, gpu := workload.Rates(workload.ModelV1, in)
		if math.Abs(gpu-full*tc.want) > 1e-12 {
			t.Errorf("p %v, g %v, GPUMul %v: v1 GPU rate %v, want %v x %v", tc.p, tc.g, tc.gpuMul, gpu, full, tc.want)
		}
		if cpu != cpu1 {
			t.Errorf("p %v, g %v: CPU rate %v, want %v", tc.p, tc.g, cpu, cpu1)
		}
		without := in
		without.PeerTransferIntensity = 0
		_, legacy0 := workload.Rates(workload.ModelLegacy, without)
		if _, legacy := workload.Rates(workload.ModelLegacy, in); legacy != legacy0 {
			t.Errorf("p %v, g %v: legacy GPU rate %v, want %v without the peer term", tc.p, tc.g, legacy, legacy0)
		}
	}
}
