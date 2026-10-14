package workload_test

import (
	"math"
	"math/rand"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/phys"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// fullSpeed is a GPU job on an uncapped node with no sharing.
func fullSpeed() workload.RateInputs {
	return workload.RateInputs{
		BaseSpeed: 2, CPURequest: 4, GPURequest: 1,
		CPUUtil: 0.6, MemoryIntensity: 0.4, IOIntensity: 0.1, CPUFeedIntensity: 0.45,
		SensitivityCPU: 1, SensitivityGPU: 1,
		CPUMul: 1, GPUMul: 1, CPUShare: 1, GPUShare: 1,
	}
}

// A GPU cap that the job's power never reaches costs nothing in either
// model: util 0.6 with a 490 W cap on a 700 W board runs at full speed, not
// at 0.7x, the cap's fraction of the board maximum. Once the cap binds, v1
// charges exactly 1 - S_gpu x (1 - GPUMul) and legacy GPUMul, whatever S_gpu
// is.
func TestRatesV1GPUCapPenaltyOnlyWhenBinding(t *testing.T) {
	board := phys.CappedBoardGPUModel{IdleW: 70, MaxW: 700, ComputeGamma: 1, MemoryEpsilon: 0.2, MemoryGamma: 1.1}
	mul := func(util, capW float64) float64 {
		return board.ThroughputMultiplier(phys.DeviceState{
			Utilization: util, CapWatts: capW, MaxCapWatts: 700, MemoryIntensity: 0.4, Class: "gpu.compute_bound",
		}, "gpu.compute_bound")
	}

	in := fullSpeed()
	_, unthrottled := workload.Rates(workload.ModelV1, in)
	in.GPUMul = mul(0.6, 490)
	if in.GPUMul != 1 {
		t.Fatalf("precondition: util 0.6 under a 490 W cap gives GPUMul %v; the cap should not bind", in.GPUMul)
	}
	if _, v1 := workload.Rates(workload.ModelV1, in); v1 != unthrottled {
		t.Errorf("v1 GPU rate %v under a non-binding cap, want the unthrottled %v", v1, unthrottled)
	}
	if _, legacy := workload.Rates(workload.ModelLegacy, in); legacy != unthrottled {
		t.Errorf("legacy GPU rate %v under a non-binding cap, want the unthrottled %v (0.7x is the cap counted twice)", legacy, unthrottled)
	}

	in.GPUMul = mul(0.95, 300)
	if in.GPUMul >= 1 {
		t.Fatalf("precondition: util 0.95 under a 300 W cap gives GPUMul %v; the cap should bind", in.GPUMul)
	}
	for _, s := range []float64{1, 0.5, 0} {
		in.SensitivityGPU = s
		_, v1 := workload.Rates(workload.ModelV1, in)
		if want := unthrottled * (1 - s*(1-in.GPUMul)); math.Abs(v1-want) > 1e-12 {
			t.Errorf("S_gpu %v: v1 GPU rate %v, want %v", s, v1, want)
		}
		_, legacy := workload.Rates(workload.ModelLegacy, in)
		if want := unthrottled * in.GPUMul; math.Abs(legacy-want) > 1e-12 {
			t.Errorf("S_gpu %v: legacy GPU rate %v, want unthrottled x GPUMul = %v", s, legacy, want)
		}
	}
}

// At full speed a job's rates are its request times the base speed in both
// models, whatever its sensitivity, which is what makes a v3 trace's ideal
// runtime equal its durationSec. A CPU-only job still gets the legacy 0.1
// GPU floor; the caller applies the GPU rate only to GPU work.
func TestRatesFullSpeedIsRequestTimesBase(t *testing.T) {
	for _, m := range []workload.Model{workload.ModelLegacy, workload.ModelV1} {
		in := fullSpeed()
		in.SensitivityCPU, in.SensitivityGPU = 0.3, 0.7
		cpu, gpu := workload.Rates(m, in)
		if cpu != 8 || gpu != 2 {
			t.Errorf("%s: rates %v, %v; want 8, 2", m, cpu, gpu)
		}
		in.GPURequest = 0
		if _, gpu := workload.Rates(m, in); math.Abs(gpu-0.2) > 1e-12 {
			t.Errorf("%s: GPU rate without a GPU request %v, want the 0.1 x base floor", m, gpu)
		}
		in.CPUShare, in.GPUShare = 0, 0
		if cpu, gpu := workload.Rates(m, in); cpu != 0 || gpu != 0 {
			t.Errorf("%s: zero shares give %v, %v; want no progress", m, cpu, gpu)
		}
	}
}

// legacyCase is an input of legacyReference, the legacy progress formula
// with the GPU cap charged twice, and the rates it gives: in, plus
// capFactor, the GPU cap over the board's maximum power, which that formula
// multiplied in as 1 - (1 - capFactor) x S_gpu on top of GPUMul.
type legacyCase struct {
	in        workload.RateInputs
	capFactor float64
	cpu, gpu  float64
}

// legacyGolden pins legacyReference: the rates the simulator's legacy
// progress loop gave for these inputs, every float printed so it parses
// back exactly.
var legacyGolden = []legacyCase{
	{workload.RateInputs{BaseSpeed: 2, CPURequest: 4, GPURequest: 1, CPUUtil: 0.6, MemoryIntensity: 0.4, IOIntensity: 0.1, CPUFeedIntensity: 0.45, SensitivityCPU: 1, SensitivityGPU: 1, CPUMul: 1, GPUMul: 1, CPUShare: 1, GPUShare: 1}, 1, 8, 2},
	{workload.RateInputs{BaseSpeed: 2, CPURequest: 4, GPURequest: 1, CPUUtil: 0.6, MemoryIntensity: 0.4, IOIntensity: 0.1, CPUFeedIntensity: 0.45, SensitivityCPU: 1, SensitivityGPU: 1, CPUMul: 0.7, GPUMul: 0.85, CPUShare: 1, GPUShare: 1}, 0.7, 6.452, 1.02935},
	{workload.RateInputs{BaseSpeed: 1, CPURequest: 8, GPURequest: 2, CPUUtil: 0.9, MemoryIntensity: 0.2, IOIntensity: 0.05, CPUFeedIntensity: 0.3, SensitivityCPU: 0.8, SensitivityGPU: 0.6, CPUMul: 0.55, GPUMul: 0.62, CPUShare: 0.5, GPUShare: 0.75}, 0.42857142857142855, 2.7292, 0.5451394285714286},
	{workload.RateInputs{BaseSpeed: 1.5, CPURequest: 0.5, GPURequest: 0, CPUUtil: 0.3, MemoryIntensity: 0.7, IOIntensity: 0.4, CPUFeedIntensity: 0, SensitivityCPU: 1, SensitivityGPU: 1, CPUMul: 0.8, GPUMul: 1, CPUShare: 1, GPUShare: 1}, 0.9, 0.69825, 0.13500000000000004},
	{workload.RateInputs{BaseSpeed: 3, CPURequest: 16, GPURequest: 4, CPUUtil: 1.2, MemoryIntensity: -0.2, IOIntensity: 1.3, CPUFeedIntensity: 1.1, SensitivityCPU: 0.5, SensitivityGPU: 0.25, CPUMul: -0.1, GPUMul: 1.1, CPUShare: 0.8, GPUShare: 0.3}, 0.2, 22.08, 1.5840000000000003},
	{workload.RateInputs{BaseSpeed: 0.7, CPURequest: 32, GPURequest: 8, CPUUtil: -0.1, MemoryIntensity: 1.2, IOIntensity: -0.3, CPUFeedIntensity: -0.2, SensitivityCPU: 0.3, SensitivityGPU: 0.9, CPUMul: 1.05, GPUMul: 0.4, CPUShare: 0.6, GPUShare: 0.9}, 0.55, 13.44, 1.19952},
	{workload.RateInputs{BaseSpeed: 2, CPURequest: 1, GPURequest: 1, CPUUtil: 0.5, MemoryIntensity: 0.5, IOIntensity: 0.5, CPUFeedIntensity: 0.5, SensitivityCPU: 0, SensitivityGPU: 0, CPUMul: 0.3, GPUMul: 0.3, CPUShare: 1, GPUShare: 1}, 0.3, 2, 0.6},
	{workload.RateInputs{BaseSpeed: 2, CPURequest: 64, GPURequest: 0.5, CPUUtil: 0.75, MemoryIntensity: 0.15, IOIntensity: 0.2, CPUFeedIntensity: 0.95, SensitivityCPU: 0.95, SensitivityGPU: 0.85, CPUMul: 0.4, GPUMul: 0.5, CPUShare: 0.25, GPUShare: 0.5}, 0.45, 17.7272, 0.0610378125},
}

// ModelLegacy reproduces the legacy progress formula bit for bit, over
// random inputs including out-of-range ones, except that it charges the GPU
// cap once: its GPU rate is legacyReference's with the cap factor at 1,
// which equals the twice-charged one whenever S_gpu is 0 or the GPU is
// uncapped. ModelV1 changes only the GPU rate. legacyReference is the
// simulator's formula copied verbatim apart from field names, and
// legacyGolden pins it to what the simulator's own functions returned, so a
// slip made in both the copy and Rates still fails.
func TestRatesLegacyMatchesSimulatorFormula(t *testing.T) {
	for i, g := range legacyGolden {
		if cpu, gpu := legacyReference(g.in, g.capFactor); cpu != g.cpu || gpu != g.gpu {
			t.Errorf("golden %d: the in-test reference gives %v, %v; the simulator's functions gave %v, %v", i, cpu, gpu, g.cpu, g.gpu)
		}
		cpu, gpu := workload.Rates(workload.ModelLegacy, g.in)
		if _, want := legacyReference(g.in, 1); cpu != g.cpu || gpu != want {
			t.Errorf("golden %d: legacy rates %v, %v; want the CPU rate %v and the GPU rate with the cap counted once %v", i, cpu, gpu, g.cpu, want)
		}
		if (g.in.SensitivityGPU == 0 || g.capFactor == 1) && gpu != g.gpu {
			t.Errorf("golden %d: legacy GPU rate %v; with S_gpu 0 or no cap the formula gives %v", i, gpu, g.gpu)
		}
	}

	rng := rand.New(rand.NewSource(7))
	r := func(lo, hi float64) float64 { return lo + (hi-lo)*rng.Float64() }
	for i := 0; i < 20000; i++ {
		in := workload.RateInputs{
			BaseSpeed: r(0, 3), CPURequest: r(0, 64), GPURequest: math.Floor(r(0, 9)),
			CPUUtil: r(-0.2, 1.2), MemoryIntensity: r(-0.2, 1.2), IOIntensity: r(-0.2, 1.2), CPUFeedIntensity: r(-0.2, 1.2),
			SensitivityCPU: r(0, 1), SensitivityGPU: r(0, 1),
			CPUMul: r(-0.1, 1.1), GPUMul: r(0, 1.1),
		}
		capFactor := r(0, 1)
		in.CPUShare, in.GPUShare = r(0, 1), r(0, 1)
		wantCPU, _ := legacyReference(in, capFactor)
		_, wantGPU := legacyReference(in, 1)
		cpu, gpu := workload.Rates(workload.ModelLegacy, in)
		if cpu != wantCPU || gpu != wantGPU {
			t.Fatalf("case %d %+v: legacy rates %v, %v; want the CPU rate %v and the GPU rate with the cap counted once %v", i, in, cpu, gpu, wantCPU, wantGPU)
		}
		if v1cpu, _ := workload.Rates(workload.ModelV1, in); v1cpu != wantCPU {
			t.Fatalf("case %d %+v: v1 CPU rate %v differs from legacy %v", i, in, v1cpu, wantCPU)
		}
	}
}

// legacyReference is the legacy progress formula with the GPU cap charged
// twice, through capFactor; a capFactor of 1 removes the second charge
// exactly.
func legacyReference(in workload.RateInputs, capFactor float64) (float64, float64) {
	clamp01 := func(v float64) float64 {
		if v < 0 {
			return 0
		}
		if v > 1 {
			return 1
		}
		return v
	}
	cpuThrottleImpactFactor := func(cpuMul float64) float64 {
		baseImpact := clamp01(0.60*clamp01(in.CPUUtil) + 0.25*(1.0-clamp01(in.MemoryIntensity)) + 0.15*(1.0-clamp01(in.IOIntensity)))
		effective := 1.0 - baseImpact*(1.0-clamp01(cpuMul))
		effective = 1.0 - in.SensitivityCPU*(1.0-effective)
		return clamp01(effective)
	}
	cpuFeedThrottleFactor := func(cpuMul float64) float64 {
		feed := clamp01(in.CPUFeedIntensity)
		if feed <= 0 {
			return 1
		}
		return clamp01(1.0 - (1.0-clamp01(cpuMul))*feed*in.SensitivityCPU)
	}
	jobCPUMul := in.CPUMul
	cpuThrottleFactor := cpuThrottleImpactFactor(jobCPUMul)
	speed := in.CPURequest * in.BaseSpeed * cpuThrottleFactor * in.CPUShare
	if speed < 0 {
		speed = 0
	}
	gpuBase := math.Max(0.1, in.GPURequest) * in.BaseSpeed
	gpuMul := in.GPUMul
	cpuFeedFactor := cpuFeedThrottleFactor(jobCPUMul)
	gpuCapImpact := 1.0 - (1.0-capFactor)*in.SensitivityGPU
	gpuSpeed := gpuBase * gpuMul * cpuFeedFactor * gpuCapImpact * in.GPUShare
	if gpuSpeed < 0 {
		gpuSpeed = 0
	}
	return speed, gpuSpeed
}
