package main

import (
	"context"

	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/pkg/governor/discover"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// The sweep reads an emulated tree with the agent's own discovery
// (pkg/governor/discover), so a plan the sweep writes is the plan the agent
// would write on that node.

// readWord is the trimmed content of a file, empty when it cannot be read.
func readWord(path string) string {
	s, _ := discover.ReadWord(path)
	return s
}

// cpuFacts is the agent's CPU discovery over sys, the tree's sys/, and
// cpuinfo, its /proc/cpuinfo. A tree without cpufreq policies gives the
// zero facts, which plan every preset as unsupported.
func cpuFacts(sys, cpuinfo string) governor.CPUFacts {
	if c := discover.ReadCPU(sys, cpuinfo); c != nil {
		return c.Facts
	}
	return governor.CPUFacts{}
}

// amdFacts is the agent's amdgpu discovery over sys: every device bound to
// amdgpu with a performance level file, keyed by bus id, sorted.
func amdFacts(sys string) []governor.AMDFacts {
	var out []governor.AMDFacts
	for _, d := range discover.ReadAMD(sys) {
		out = append(out, d.Facts)
	}
	return out
}

// nvFacts is the agent's NVIDIA discovery on the node of profile p: the
// architecture from the labels the node registers with and the product
// name, then the GPUs through the node's fake nvidia-smi. A node without
// NVIDIA GPUs, or an architecture pkg/governor has no table for, gives none.
func nvFacts(ctx context.Context, r smi.Runner, p *hwemu.Profile) ([]discover.NvidiaGPU, error) {
	if p.GPUs == nil || p.GPUs.Vendor != "nvidia" {
		return nil, nil
	}
	arch := discover.NvidiaArch(p.NodeObject(p.Name).Labels, p.GPUs.Product.V)
	if arch == "" {
		return nil, nil
	}
	return discover.ReadNvidia(ctx, r.Run, arch)
}
