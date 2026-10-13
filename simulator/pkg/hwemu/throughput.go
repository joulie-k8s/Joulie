package hwemu

import (
	"math"

	"github.com/matbun/joulie/simulator/pkg/phys"
)

// The throughput methods let a workload model read what the node's controls
// cost its jobs: how much slower a job runs on some CPUs or on a GPU than it
// would with no power control and the performance governor, the rate its
// peer transfers get, the idle power of a package, and how the GPUs are
// shared. They read the state after the last step, as State does, and
// change nothing. Like every other method of a Node they take its lock; the
// simulator calls them only on the emulator goroutine, after a step.

// CPUThroughput is the throughput of a job on the logical CPUs cpus, of
// workload class class with memory intensity mem and IO intensity io,
// relative to a governor-independent reference (decision D10): the mean,
// over cpus, of TM(controlled scale) / TM(reference scale), where TM is the
// CPU model's ThroughputMultiplier.
//
//   - The controlled scale is the frequency scaling_cur_freq shows, the
//     governor's choice, boost and a package cap included, over
//     cpuinfo_max_freq.
//   - The reference scale is what performance runs the CPU at with
//     scaling_max_freq at cpuinfo_max_freq and boost as the profile sets
//     it, over cpuinfo_max_freq (refKHz), whatever the governor, the limits
//     or a boost write say now.
//
// An uncapped node under performance with its profile's boost therefore
// gives 1 once frequencies have settled. A governor that holds a lower
// frequency, generic powersave or schedutil at light load, gives less, as
// do a package cap, a lowered scaling_max_freq and boost turned off. While a
// frequency ramps after a change the ratio follows the ramp. A CPU without
// cpufreq runs at scale 1, or at its package's capped scale. CPUs outside
// the node are skipped; with none left the result is 1, the throughput of a
// job no control slows.
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
	maxKHz := float64(pol.maxKHz)
	return pol.shownKHz / maxKHz, float64(n.refKHz(pol)) / maxKHz
}

// refKHz is the reference frequency of pol: the ceiling performance holds
// with scaling_max_freq at cpuinfo_max_freq and the profile's cpufreq.boost,
// which on a table driver with boost on is cpuinfo_max_freq and with boost
// off the top table entry. A driver without a table reaches
// cpuinfo_max_freq, whatever no_turbo or a per-policy boost now say.
func (n *Node) refKHz(pol *nodePolicy) int64 {
	free := *pol
	free.reqMaxKHz = pol.maxKHz
	boost := 0
	if cf := n.profile.CPUFreq; cf != nil {
		boost = cf.Boost.V
	}
	free.boost = &boost
	return free.ceilingKHz()
}

// GPUThroughput is the throughput of GPU i relative to the GPU uncapped at
// its maximum clock: the GPU model's ThroughputMultiplier at the device
// utilization, Load.GPUClass and Load.MemoryIntensity the power derivation
// uses, with CapWatts the enforced limit, through the clock model when the
// GPU has one (phys.GPUThroughputWithClock): a compute ceiling below the
// clock the cap allows slows compute-bound work, and a data path forced to
// its minimum slows memory-bound work. A data path that follows demand rises
// for the GPU's own work, so under auto only the cap counts. It is 1 while
// nothing binds, and a GPU the node does not have gives 1.
func (n *Node) GPUThroughput(i int) float64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	g := n.gpuByIndex(i)
	if g == nil {
		return 1
	}
	s := n.gpuDeviceState(g)
	m, ok := n.gpuModel.(phys.CappedBoardGPUModel)
	clk := n.gpuPhysClock(g, true)
	if !ok || clk.ComputeMaxMHz <= 0 {
		return n.gpuModel.ThroughputMultiplier(s, n.load.GPUClass)
	}
	return phys.GPUThroughputWithClock(m, s, clk, n.gpuClockFacts(), n.load.GPUClass)
}

// PeerTransferRates is the rate, in bytes per second, each transfer of the
// load's PeerTransfers gets after the last step, in the order SetLoad was
// given them: a copy between two GPUs of the node shares the receiver's
// peer link (gpus.transfer.peerLinkGBps) and, with the receiver gate on,
// gets the fraction the receiver's data path lets through; a transfer with
// a host or NIC end (a negative Src or Dst) is never gated and gets its
// demand, as does every transfer on a profile without gpus.transfer. The
// ratio of a rate to its demand is how much the copy slows the job that
// makes it.
func (n *Node) PeerTransferRates() []float64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.peerRates()
}

// IdleCPUPowerW is the power, in W, CPU package pkg draws at zero
// utilization at its present frequency scale, with the load's CPU class and
// memory intensity, physics.cpu.idleGovernorDeltaW included while its
// governor holds the top frequency. A package cap never lowers it:
// derivePackage holds a capped package at or above its power at the lowest
// frequency, which at zero utilization is this power. A package the node
// does not have gives 0.
func (n *Node) IdleCPUPowerW(pkg int) float64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if pkg < 0 || pkg >= len(n.pkgs) {
		return 0
	}
	pk := n.pkgs[pkg]
	return n.cpuModel.Power(phys.DeviceState{
		FreqScale:       pk.effScale,
		MemoryIntensity: n.load.MemoryIntensity,
		Class:           n.load.CPUClass,
	}) + n.idleGovernorW(pk)
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
