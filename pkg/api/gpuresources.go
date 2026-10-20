package api

import (
	"strconv"
	"strings"
)

// Kinds of GPU extended resources, as device plugins advertise them. Every
// component that tells a GPU resource from another resource does it through
// GPUResourceKind, so a node, a pod and a printed column agree on what a GPU
// is.
const (
	// GPUResourceWhole is one unit per device: <domain>/gpu (nvidia.com/gpu,
	// amd.com/gpu, any vendor's), gpu.intel.com/i915 or gpu.intel.com/xe.
	// Under the NVIDIA single MIG strategy, time-slicing without renaming or
	// the Intel shared-dev-num option, a unit is a MIG device or a replica,
	// not a physical GPU.
	GPUResourceWhole = "whole"
	// GPUResourcePartition is one part of a GPU: a MIG device of the NVIDIA
	// mixed strategy, nvidia.com/mig-<profile>, or an AMD compute partition
	// of the mixed naming strategy, amd.com/<compute>_<memory> such as
	// amd.com/cpx_nps4.
	GPUResourcePartition = "partition"
	// GPUResourceShared is a time-sliced or MPS replica the NVIDIA device
	// plugin renamed <resource>.shared (renameByDefault): nvidia.com/gpu.shared
	// or nvidia.com/mig-<profile>.shared.
	GPUResourceShared = "shared"
)

// migSlicesPerGPU is the number of compute slices of the MIG-capable GPUs
// a MIG profile name counts against: A100, H100, H200 and B200 class GPUs
// have seven.
const migSlicesPerGPU = 7

// amdComputePartitions maps an amdgpu compute partition mode to the number
// of partitions it splits a GPU into. CPX gives one partition per XCD, eight
// on MI300X, MI325X and the MI350 series.
var amdComputePartitions = map[string]int{"spx": 1, "dpx": 2, "tpx": 3, "qpx": 4, "cpx": 8}

// GPUResourceKind classifies an extended resource name: GPUResourceWhole,
// GPUResourcePartition, GPUResourceShared, or "" when it is not a GPU
// resource. Monitoring and share-accounting resources of the Intel plugin
// (gpu.intel.com/i915_monitoring, gpu.intel.com/millicores) are not GPUs.
func GPUResourceKind(name string) string {
	base, renamed := strings.CutSuffix(strings.ToLower(strings.TrimSpace(name)), ".shared")
	kind := ""
	switch {
	case base == "gpu.intel.com/i915", base == "gpu.intel.com/xe":
		kind = GPUResourceWhole
	case strings.HasSuffix(base, "/gpu") && strings.Index(base, "/") > 0:
		kind = GPUResourceWhole
	case strings.HasPrefix(base, "nvidia.com/mig-") && len(base) > len("nvidia.com/mig-"):
		kind = GPUResourcePartition
	case amdPartitionsPerGPU(base) > 0:
		kind = GPUResourcePartition
	}
	if kind != "" && renamed {
		return GPUResourceShared
	}
	return kind
}

// GPUResourceShare is the fraction of one physical GPU that one unit of
// resource name stands for, 0 when it is not a GPU resource:
//
//   - a whole unit is one GPU;
//   - a MIG device is its compute slices out of seven, read from the profile
//     (1g.12gb is 1/7, 3g.47gb 3/7, 1c.3g.40gb 1/7), one slice when the
//     profile names none;
//   - an AMD partition is one GPU divided by the partitions of its compute
//     mode (spx 1, dpx 2, tpx 3, qpx 4, cpx 8);
//   - a shared replica is the device it shares, whole or a MIG device,
//     because a time-sliced or MPS replica can drive that device alone
//     while the other replicas idle.
//
// The share is an estimate of a pod's GPU demand, not a capacity: on a GPU
// with four MIG slices (A30) a MIG device counts less than it is, and on an
// MI300A, with six XCDs, a CPX partition counts 1/8.
func GPUResourceShare(name string) float64 {
	base, _ := strings.CutSuffix(strings.ToLower(strings.TrimSpace(name)), ".shared")
	switch GPUResourceKind(base) {
	case GPUResourceWhole:
		return 1
	case GPUResourcePartition:
		if n := amdPartitionsPerGPU(base); n > 0 {
			return 1 / float64(n)
		}
		slices := migComputeSlices(strings.TrimPrefix(base, "nvidia.com/mig-"))
		if slices > migSlicesPerGPU {
			slices = migSlicesPerGPU
		}
		return float64(slices) / migSlicesPerGPU
	}
	return 0
}

// amdPartitionsPerGPU is the number of partitions per GPU of an AMD mixed
// strategy resource, amd.com/<compute>_nps<n> (the device plugin's name is
// the lower-cased current_compute_partition and current_memory_partition of
// amdgpu sysfs joined by "_"), or 0 when name is not one.
func amdPartitionsPerGPU(name string) int {
	rest, ok := strings.CutPrefix(name, "amd.com/")
	if !ok {
		return 0
	}
	compute, memory, ok := strings.Cut(rest, "_")
	if !ok {
		return 0
	}
	nps, ok := strings.CutPrefix(memory, "nps")
	if !ok || nps == "" || strings.Trim(nps, "0123456789") != "" {
		return 0
	}
	return amdComputePartitions[compute]
}

// migComputeSlices is the number of compute slices a MIG profile name
// holds: <c>c.<g>g.<memory> is c slices of a g-slice GPU instance,
// <g>g.<memory> is g slices; 1 when the name says neither.
func migComputeSlices(profile string) int {
	first, rest, _ := strings.Cut(profile, ".")
	if c, ok := strings.CutSuffix(first, "c"); ok {
		if v, err := strconv.Atoi(c); err == nil && v > 0 {
			return v
		}
		first, _, _ = strings.Cut(rest, ".")
	}
	if g, ok := strings.CutSuffix(first, "g"); ok {
		if v, err := strconv.Atoi(g); err == nil && v > 0 {
			return v
		}
	}
	return 1
}

// Node labels that state a node's physical GPU count.
const (
	// LabelHWGPUCount is Joulie's own hardware label, set by an operator or
	// by the simulator.
	LabelHWGPUCount = "joulie.io/hw.gpu-count"
	// LabelGFDGPUCount is the count GPU feature discovery publishes: the
	// physical GPUs, except when the single MIG strategy exposes MIG devices
	// (product suffixed -MIG-), where it counts the MIG devices, and 0 on an
	// invalid MIG configuration.
	LabelGFDGPUCount = "nvidia.com/gpu.count"
	// LabelGFDMIGStrategy is the MIG strategy GPU feature discovery runs
	// with: none, single or mixed. The GPU Operator runs it with single by
	// default, so the label is on nodes without any MIG-enabled GPU too.
	LabelGFDMIGStrategy = "nvidia.com/mig.strategy"
	// LabelGFDGPUProduct is the GPU product GPU feature discovery publishes,
	// with the suffixes GPUProductBase strips.
	LabelGFDGPUProduct = "nvidia.com/gpu.product"
)

// GPUCountFromLabels is the number of physical GPUs the node labels state:
// LabelHWGPUCount, else LabelGFDGPUCount except when the single MIG
// strategy exposes MIG devices (product suffixed -MIG-). ok is false when no
// label states a positive count. nvidia.com/gpu.replicas is never read: it
// is 0 on a node whose GPUs are all MIG enabled.
func GPUCountFromLabels(labels map[string]string) (n int, ok bool) {
	if v, err := strconv.Atoi(strings.TrimSpace(labels[LabelHWGPUCount])); err == nil && v > 0 {
		return v, true
	}
	if strings.EqualFold(strings.TrimSpace(labels[LabelGFDMIGStrategy]), "single") &&
		strings.Contains(labels[LabelGFDGPUProduct], "-MIG-") {
		return 0, false
	}
	if v, err := strconv.Atoi(strings.TrimSpace(labels[LabelGFDGPUCount])); err == nil && v > 0 {
		return v, true
	}
	return 0, false
}

// GPUPowerDomains is the number of a node's GPUs that hold a power limit of
// their own, the factor a per-GPU power maximum or cap is multiplied by for
// the node: powerDomains as the agent publishes it in NodeHardware
// (status.gpu.powerDomains), when it is below count, and count otherwise.
// The agent publishes none when every counted GPU holds its own limit, and
// an older agent never does. On a two-die AMD package such as an MI250X
// both dies are counted GPUs, and only the primary holds a limit, which
// bounds the whole package.
func GPUPowerDomains(count, powerDomains int) int {
	if powerDomains > 0 && powerDomains < count {
		return powerDomains
	}
	return count
}

// GPUProductBase is the GPU model a product label names, without the
// suffixes GPU feature discovery appends to nvidia.com/gpu.product: -SHARED
// for time-slicing or MPS without renaming, -MIG-<profile> under the single
// MIG strategy, and -MIG-INVALID for an invalid MIG configuration
// (A100-SXM4-40GB-MIG-1g.5gb-SHARED is A100-SXM4-40GB). A label value
// without them is returned trimmed.
func GPUProductBase(product string) string {
	p := strings.TrimSuffix(strings.TrimSpace(product), "-SHARED")
	if i := strings.LastIndex(p, "-MIG-"); i > 0 {
		p = p[:i]
	}
	return p
}
