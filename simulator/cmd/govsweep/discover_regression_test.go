package main

// The sweep plans with the facts the agent discovers on the same node: it
// reads them through pkg/governor/discover, the agent's own discovery, so a
// plan the sweep measures is the plan the agent writes there.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/pkg/governor/discover"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// On an amd-pstate-epp host booted under performance, the sweep plans the
// balanced and efficiency presets with the preference the agent writes:
// amd-pstate-epp shrinks energy_performance_available_preferences to
// "performance" while the governor is performance.
func TestSweepPlansTheAgentsEPPOnAnAMDPstateEPPHost(t *testing.T) {
	_, tree, _ := hwemutest.NewNode(t, "amd-epyc-2s-hsmp", nil)
	f := cpuFacts(tree.SysRoot, tree.CPUInfoPath)
	for preset, want := range map[string]string{
		joulie.CPUGovernorPresetBalanced:   "powersave epp=balance_performance",
		joulie.CPUGovernorPresetEfficiency: "powersave epp=balance_power",
	} {
		plan, err := governor.PlanCPU(f, preset, "")
		if err != nil {
			t.Fatalf("PlanCPU(%s): %v", preset, err)
		}
		if plan.Native != want || plan.Result != joulie.GovernorResultApplied {
			t.Errorf("%s plans %q (%s), want %q (applied), as the agent does", preset, plan.Native, plan.Result, want)
		}
	}
}

// The sweep names an NVIDIA GPU's architecture as the agent does, from the
// node's labels and product name: the L40S is Ada (compute 8.9), whatever
// GPU feature discovery's gpu.family label says.
func TestSweepNamesTheNvidiaArchitectureAsTheAgentDoes(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "nvidia-l40s-4gpu", nil)
	r := smi.Runner{Env: smi.Env{SysRoot: tree.SysRoot, StateRoot: tree.StateRoot}}
	gpus, err := nvFacts(context.Background(), r, p)
	if err != nil || len(gpus) != p.GPUs.Count.V {
		t.Fatalf("nvFacts: %d GPUs, %v; want %d", len(gpus), err, p.GPUs.Count.V)
	}
	if want := discover.NvidiaArch(p.NodeObject("n").Labels, p.GPUs.Product.V); want != governor.NVArchAda {
		t.Fatalf("the agent names the L40S %q, want ada", want)
	}
	for _, g := range gpus {
		if g.Facts.Arch != governor.NVArchAda {
			t.Errorf("GPU %s: architecture %q, want ada as the agent names it", g.BusID, g.Facts.Arch)
		}
	}
}

// The sweep plans only for devices bound to amdgpu, as the agent does: a
// device whose driver link names another driver (passed through to a VM)
// is not one Joulie controls.
func TestSweepSkipsAMDDevicesNotBoundToAmdgpu(t *testing.T) {
	_, tree, _ := hwemutest.NewNode(t, "amd-radeon-pro-w7900-4gpu", nil)
	all := amdFacts(tree.SysRoot)
	if len(all) < 2 {
		t.Fatalf("amdFacts found %d devices, want the profile's GPUs", len(all))
	}
	dev := filepath.Join(tree.SysRoot, governor.PCIDevicesRelDir, all[0].BusID)
	if err := os.Symlink("../../../bus/pci/drivers/vfio-pci", filepath.Join(dev, "driver")); err != nil {
		t.Fatal(err)
	}
	got := amdFacts(tree.SysRoot)
	if len(got) != len(all)-1 {
		t.Fatalf("amdFacts found %d devices with %s bound to vfio-pci, want %d", len(got), all[0].BusID, len(all)-1)
	}
	for _, f := range got {
		if f.BusID == all[0].BusID {
			t.Errorf("amdFacts lists %s, which is bound to vfio-pci", f.BusID)
		}
	}
}
