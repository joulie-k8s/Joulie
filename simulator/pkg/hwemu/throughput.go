package hwemu

import (
	"math"

	"github.com/matbun/joulie/simulator/pkg/phys"
)

// The throughput methods let a workload model read what the node's controls
// cost its jobs: how much slower a job runs on some CPUs or on a GPU than it
// would with no power control, the idle power of a package, and how the GPUs
// are shared. They read the state after the last step, as State does, and
// change nothing. Like every other method of a Node they take its lock; the
// simulator calls them only on the emulator goroutine, after a step.

// CPUThroughput is the throughput of a job on the logical CPUs cpus, of
// workload class class with memory intensity mem and IO intensity io,
// relative to the throughput the governor alone would give it: the mean, over
// cpus, of TM(controlled scale) / TM(reference scale), where TM is the CPU
// model's ThroughputMultiplier.
//
//   - The controlled scale is the frequency scaling_cur_freq shows, a package
//     cap included, over cpuinfo_max_freq.
//   - The reference scale is the governor's target at the CPU's present
//     utilization with no package cap and scaling_max_freq at
//     cpuinfo_max_freq, over cpuinfo_max_freq. Every other policy setting
//     (governor, scaling_min_freq, boost) stays as it is.
//
// An uncapped node therefore gives 1 once frequencies have settled, under any
// governor, while a package cap or a lowered scaling_max_freq gives less. The
// reference is the governor's target, not a ramp toward it, so while a
// frequency ramps after a load change the ratio departs from 1 for a few ramp
// time constants. A CPU without cpufreq runs at scale 1, or at its package's
// capped scale. CPUs outside the node are skipped; with none left the result
// is 1, the throughput of a job no control slows.
func (n *Node) CPUThroughput(cpus []int, class string, mem, io float64) float64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	sum, k := 0.0, 0
	for _, c := range cpus {
		if c < 0 || c >= len(n.cpuUtil) {
			continue
		}
		controlled, reference := n.throughputScales(c)
		s := phys.DeviceState{Utilization: n.cpuUtil[c], MemoryIntensity: mem, IOIntensity: io, Class: class}
		s.FreqScale = reference
		ref := n.cpuModel.ThroughputMultiplier(s, class)
		s.FreqScale = controlled
		got := n.cpuModel.ThroughputMultiplier(s, class)
		r := 1.0
		if ref > 0 {
			r = got / ref
		}
		sum += r
		k++
	}
	if k == 0 {
		return 1
	}
	return sum / float64(k)
}

// throughputScales is CPU c's controlled and reference frequency scale, as
// CPUThroughput defines them.
func (n *Node) throughputScales(c int) (controlled, reference float64) {
	pk := n.pkgs[n.topo.socketOf(c)]
	pol := n.cpuPolicy[c]
	if pol == nil || pol.maxKHz <= 0 {
		// No frequency to read: the package's scale, as derivePackage
		// takes it for such a CPU.
		controlled = 1
		if pk.capped {
			controlled = math.Min(controlled, pk.effScale)
		}
		return controlled, 1
	}
	u := 0.0
	for _, pc := range pol.cpus {
		u += n.cpuUtil[pc]
	}
	if len(pol.cpus) > 0 {
		u /= float64(len(pol.cpus))
	}
	free := *pol
	free.reqMaxKHz = pol.maxKHz
	maxKHz := float64(pol.maxKHz)
	return pol.shownKHz / maxKHz, free.target(u) / maxKHz
}

// GPUThroughput is the throughput of GPU i relative to the GPU uncapped: the
// GPU model's ThroughputMultiplier at the device utilization, Load.GPUClass
// and Load.MemoryIntensity the power derivation uses, with CapWatts the
// enforced limit. It is 1 while the limit is at or above the power the GPU
// would draw uncapped, and falls below 1 once the limit binds. A GPU the node
// does not have gives 1.
func (n *Node) GPUThroughput(i int) float64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, g := range n.gpus {
		if g.index != i {
			continue
		}
		s := phys.DeviceState{
			Utilization:     n.gpuUtil(g.index),
			CapWatts:        math.Max(g.settledW, nodeMinGPUCapW),
			MemoryIntensity: n.load.MemoryIntensity,
			Class:           n.load.GPUClass,
		}
		return n.gpuModel.ThroughputMultiplier(s, n.load.GPUClass)
	}
	return 1
}

// IdleCPUPowerW is the power, in W, CPU package pkg draws at zero
// utilization at its present frequency scale, with the load's CPU class and
// memory intensity. A package cap never lowers it: derivePackage holds a
// capped package at or above its power at the lowest frequency, which at
// zero utilization is this power. A package the node does not have gives 0.
func (n *Node) IdleCPUPowerW(pkg int) float64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if pkg < 0 || pkg >= len(n.pkgs) {
		return 0
	}
	return n.cpuModel.Power(phys.DeviceState{
		FreqScale:       n.pkgs[pkg].effScale,
		MemoryIntensity: n.load.MemoryIntensity,
		Class:           n.load.CPUClass,
	})
}

// GPUExposure is how the node's GPUs are offered to pods: the profile's
// gpus.exposure and the units each GPU advertises under it. whole gives 1,
// mig-single and mig-mixed migDevicesPerGPU, timeslice-rename and
// timeslice-norename timesliceReplicas, and dra 0, since its ResourceSlices
// are not node resources. A node without GPUs gives "" and 0.
func (n *Node) GPUExposure() (string, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	g := n.profile.GPUs
	if g == nil || g.Count.V <= 0 {
		return "", 0
	}
	switch g.Exposure.V {
	case "whole":
		return g.Exposure.V, 1
	case "mig-single", "mig-mixed":
		return g.Exposure.V, g.MIGDevicesPerGPU
	case "timeslice-rename", "timeslice-norename":
		return g.Exposure.V, g.TimesliceReplicas
	}
	return g.Exposure.V, 0
}
