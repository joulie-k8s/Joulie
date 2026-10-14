package main

// The v1 workload engine's view of the emulated nodes. hwemu.Node is not
// safe for concurrent use and the emulator steps it on its own goroutine,
// so the engine reads a node's throughput, idle power and GPU exposure only
// inside the emulator's publish callback, through the Runner the emulator
// hands out. The engine never sets a node's load itself: the
// emulator's load callback returns what the engine computed, and the
// emulator's Tick is the one caller of SetLoad.

import (
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// throughputer is what the engine reads of an emulated node: what the
// node's power controls cost a job (hwemu.Node's throughput methods). The
// emulator's Runner of a real node is a *hwemu.Node; tests use fakes.
type throughputer interface {
	CPUThroughput(cpus []int, class string, mem, io float64) float64
	GPUThroughput(i int) float64
	IdleCPUPowerW(pkg int) float64
	GPUExposure() (string, int)
}

var _ throughputer = (*hwemu.Node)(nil)

// emuNode is one emulated node as the engine sees it. CPUs is the number of
// logical CPUs in hwemu's numbering, the length of the per-CPU load;
// AllocatableCPU is the Node's allocatable CPUs, the denominator of the
// idle energy split; GPUIdleW is one GPU's power at zero utilization.
// PeerLinkBps is the peer link into each GPU, gpus.transfer.peerLinkGBps in
// bytes per second: 0 on a profile without it, whose GPUs move no data
// between them.
type emuNode struct {
	Profile        string
	CPUs           int
	Sockets        int
	GPUs           int
	AllocatableCPU float64
	GPUIdleW       float64
	PeerLinkBps    float64
	TP             throughputer
}

// emuNodes gives the engine the emulated node of a name, false for a node
// that is not emulated. It is called only from the emulator's goroutine.
type emuNodes interface {
	emuNode(name string) (emuNode, bool)
}

// runtimeNodes is emuNodes over a running emulator.
type runtimeNodes struct{ rt *emulateRuntime }

func (n runtimeNodes) emuNode(name string) (emuNode, bool) {
	m, ok := n.rt.emu.Member(name)
	if !ok || m.Profile == nil {
		return emuNode{}, false
	}
	r, ok := n.rt.emu.RunnerFor(name)
	if !ok {
		return emuNode{}, false
	}
	tp, ok := r.(throughputer)
	if !ok {
		return emuNode{}, false
	}
	return emuNodeOf(m, tp), true
}

// emuNodeOf is spec member m with tp as its throughput.
func emuNodeOf(m clusterspec.Member, tp throughputer) emuNode {
	p := m.Profile
	n := emuNode{Profile: p.Name, Sockets: p.CPU.Sockets.V, GPUIdleW: p.Physics.GPU.IdleW.V, TP: tp}
	n.CPUs = p.CPU.Sockets.V * p.CPU.CoresPerSocket.V * p.CPU.ThreadsPerCore.V
	if n.CPUs <= 0 {
		n.CPUs = int(p.Node.Capacity.CPU.V)
	}
	if p.GPUs != nil {
		n.GPUs = p.GPUs.Count.V
		if tr := p.GPUs.Transfer; tr != nil {
			n.PeerLinkBps = tr.PeerLinkGBps.V * 1e9
		}
	}
	n.AllocatableCPU = float64(n.CPUs)
	if m.Node != nil {
		if q, ok := m.Node.Status.Allocatable[corev1.ResourceCPU]; ok {
			n.AllocatableCPU = q.AsApproximateFloat64()
		}
	}
	return n
}

// snapshotEnergy is node n's energy readings at time at, the input of
// workload.Attribute: its CPU package and GPU ledgers from the snapshot,
// and the packages' idle power from the node itself. unitsPerDevice is the
// units each GPU advertises (workload.GPUSlots.UnitsPerDevice).
func snapshotEnergy(n *emulate.NodeSnapshot, info emuNode, at time.Time, unitsPerDevice int) workload.NodeEnergy {
	e := workload.NodeEnergy{
		Time:              at,
		AllocatableCPU:    info.AllocatableCPU,
		GPUIdleW:          info.GPUIdleW,
		GPUUnitsPerDevice: unitsPerDevice,
	}
	for _, c := range n.Components {
		switch c.Type {
		case semconv.HwTypeCPU:
			e.CPUEnergyJ += c.EnergyJ
		case semconv.HwTypeGPU:
			if c.Index < 0 {
				continue
			}
			for len(e.GPUEnergyJ) <= c.Index {
				e.GPUEnergyJ = append(e.GPUEnergyJ, 0)
			}
			e.GPUEnergyJ[c.Index] = c.EnergyJ
		}
	}
	if info.TP != nil {
		for pkg := 0; pkg < info.Sockets; pkg++ {
			e.CPUIdleW += info.TP.IdleCPUPowerW(pkg)
		}
	}
	return e
}
