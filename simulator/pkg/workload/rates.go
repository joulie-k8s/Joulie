package workload

import "math"

// RateInputs are what one running job's progress rates depend on during a
// tick. Every multiplier and share is in 0..1 and set by the caller: a zero
// share means the job gets no CPU or GPU time, not "unset".
type RateInputs struct {
	// BaseSpeed is the work units one core, or one GPU, does per second at
	// full speed (SIM_BASE_SPEED_PER_CORE).
	BaseSpeed float64
	// CPURequest and GPURequest are the job's requested cores and GPUs.
	CPURequest float64
	GPURequest float64
	// CPUUtil, MemoryIntensity, IOIntensity and CPUFeedIntensity are the
	// job's profile; CPUFeedIntensity is how much its GPU work waits on the
	// CPU.
	CPUUtil          float64
	MemoryIntensity  float64
	IOIntensity      float64
	CPUFeedIntensity float64
	// SensitivityCPU and SensitivityGPU are the trace's sensitivity, 1 when
	// the trace omits it.
	SensitivityCPU float64
	SensitivityGPU float64
	// CPUMul and GPUMul are the throughput of the job's CPUs and GPUs
	// relative to full speed: 1 while no cap binds. GPUMul holds all a GPU
	// cap costs; no other input charges the cap.
	CPUMul float64
	GPUMul float64
	// CPUShare and GPUShare are the job's share of the node's CPUs and of
	// its GPUs when they are oversubscribed or time-sliced: in ModelV1,
	// CPUSlots.Share and GPUSlots.Share.
	CPUShare float64
	GPUShare float64
	// PeerTransferIntensity is the share p of the job's GPU time spent
	// moving data between its GPUs on one node (the trace's
	// workloadProfile.peerTransferIntensity), and PeerMul the rate those
	// transfers get over the rate they demand: 1 while nothing slows them.
	// A p of 0, the zero value, means no transfers, and PeerMul is then
	// not read. ModelLegacy reads neither.
	PeerTransferIntensity float64
	PeerMul               float64
}

// Rates returns a running job's CPU and GPU progress in work units per
// second. The caller applies a rate only while the job has work of that
// kind left, and gates gang members.
//
// CPU, both models:
//
//	req x base x clamp01(1 - S_cpu x impact x (1 - CPUMul)) x CPUShare
//
// with impact = clamp01(0.6 cpuUtil + 0.25 (1 - mem) + 0.15 (1 - io)): a
// compute-bound job loses more of a CPU cap than a memory- or I/O-bound one.
//
// GPU, ModelLegacy:
//
//	max(0.1, G) x base x GPUMul x feed x GPUShare
//
// GPUMul is the whole cost of a GPU cap, so a cap the job's power never
// reaches costs nothing, and S_gpu does not enter the legacy GPU rate.
//
// GPU, ModelV1 (and any model other than ModelLegacy):
//
//	max(0.1, G) x base x (1 - S_gpu x (1 - GPUMul)) x feed x GPUShare
//
// so the cap costs only what it takes from throughput. In both models
// feed = clamp01(1 - (1 - CPUMul) x CPUFeedIntensity x S_cpu), or 1 for a
// job with no CPU feed. Negative rates are 0.
//
// A ModelV1 job that moves data between its GPUs spends the share
// p = PeerTransferIntensity of its GPU time on the transfers, which run at
// g = PeerMul of their demand whatever the cap, and the rest on work at
// thr = 1 - S_gpu x (1 - GPUMul), so the throughput factor above becomes
//
//	1 / ((1 - p) / thr + p / g)
//
// It is thr when p is 0, bit for bit, and 0 when thr or g is 0.
//
// The CPU rate keeps the operand order the legacy experiments ran with, so
// their numbers stay bit-identical; reordering it moves them in the last
// bits.
func Rates(model Model, in RateInputs) (cpuRate, gpuRate float64) {
	cpuRate = in.CPURequest * in.BaseSpeed * cpuThrottleFactor(in) * in.CPUShare
	if cpuRate < 0 {
		cpuRate = 0
	}
	gpuBase := math.Max(0.1, in.GPURequest) * in.BaseSpeed
	feed := cpuFeedFactor(in)
	if model == ModelLegacy {
		gpuRate = gpuBase * in.GPUMul * feed * in.GPUShare
	} else {
		gpuRate = gpuBase * peerFactor(1.0-in.SensitivityGPU*(1.0-clamp01(in.GPUMul)), in) * feed * in.GPUShare
	}
	if gpuRate < 0 {
		gpuRate = 0
	}
	return cpuRate, gpuRate
}

// peerFactor is the GPU throughput factor thr of a job with its peer
// transfers counted (Rates): thr itself while the job makes none.
func peerFactor(thr float64, in RateInputs) float64 {
	p := clamp01(in.PeerTransferIntensity)
	if p == 0 {
		return thr
	}
	g := clamp01(in.PeerMul)
	if thr <= 0 || g == 0 {
		return 0
	}
	return 1.0 / ((1.0-p)/thr + p/g)
}

// cpuThrottleFactor is the CPU throughput factor of Rates' CPU rate:
// clamp01(1 - S_cpu x impact x (1 - CPUMul)).
func cpuThrottleFactor(in RateInputs) float64 {
	baseImpact := clamp01(0.60*clamp01(in.CPUUtil) + 0.25*(1.0-clamp01(in.MemoryIntensity)) + 0.15*(1.0-clamp01(in.IOIntensity)))
	effective := 1.0 - baseImpact*(1.0-clamp01(in.CPUMul))
	effective = 1.0 - in.SensitivityCPU*(1.0-effective)
	return clamp01(effective)
}

// cpuFeedFactor is the feed factor of Rates' GPU rate, 1 for a job with no
// CPU feed.
func cpuFeedFactor(in RateInputs) float64 {
	feed := clamp01(in.CPUFeedIntensity)
	if feed <= 0 {
		return 1
	}
	return clamp01(1.0 - (1.0-clamp01(in.CPUMul))*feed*in.SensitivityCPU)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
