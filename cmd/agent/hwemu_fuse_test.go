package main

// The agent over the FUSE view of an emulated node (simulator/pkg/hwemu/
// hwfuse), as an emulated agent pod of the simulated cluster sees it in FUSE
// mode: its real file calls reach the node, so a value the driver refuses
// fails at the write with the kernel's errno and a value it takes is in
// effect before the write returns, with no CheckWrite seam. The tests skip
// where FUSE cannot be mounted (hwfusetest.Require).

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwfuse/hwfusetest"
)

// HV01: A10 through the view. Both package PL1 limits are locked: every
// RAPL write the agent makes with os.WriteFile fails at the write with
// ENODATA, the reconcile returns the error, no DVFS fallback runs, and each
// refusal is a rejected event of the node's next step.
func TestHwemuHV01LockedRAPLFailsAtTheWriteThroughTheView(t *testing.T) {
	n, tree, p := hwemutest.NewNodeWith(t, "intel-xeon-2s-rapl", withLockedPL1, hwemu.Options{Live: true})
	// The view reads at the node's time, which the test steps.
	view := hwfusetest.Mount(t, n, tree, func() time.Time { return n.State().Time })
	n.SetLoad(hwemu.Load{CPUUtil: []float64{1}})
	e := useEmulatedNode(t, view, map[string]string{"DVFS_COOLDOWN": "0s", "DVFS_TRIP_COUNT": "1"})
	before, _ := policyMaxFreqs(t, e)

	nc := e.controller(t)
	reader, dyn := newTestClients(t, e.nodeObject(p), nodeTwinWithCPUCap(e.name, 120))
	for i := 0; i < 3; i++ {
		if err := n.Step(time.Second); err != nil {
			t.Fatal(err)
		}
		err := reconcileOnce(context.Background(), reader, dyn, nc)
		if err == nil || !strings.Contains(err.Error(), "write rapl cap") || !strings.Contains(err.Error(), syscall.ENODATA.Error()) {
			t.Fatalf("reconcile %d returned %v; want the locked PL1 to fail the agent's own write with ENODATA (D10)", i+1, err)
		}
	}
	if err := n.Step(0); err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, ev := range n.State().Events {
		if ev.Kind == hwemu.EventRejected && ev.Errno == hwemu.ErrnoENODATA {
			refused++
		}
	}
	if refused != 2 {
		t.Errorf("%d writes refused with ENODATA at the last reconcile, want one per locked package limit", refused)
	}
	after, _ := policyMaxFreqs(t, e)
	for policy, khz := range before {
		if after[policy] != khz {
			t.Errorf("%s scaling_max_freq %d, was %d: no DVFS fallback runs after a RAPL write failure (D10)", policy, after[policy], khz)
		}
	}
}

// HV02: GPU caps through the views, by amdgpu sysfs on the MI300X and by
// nvidia-smi on the NVIDIA state on the H100: the agent reports the cap
// applied, and the node holds it before any step.
func TestHwemuHV02GPUCapsThroughTheViews(t *testing.T) {
	for _, c := range []struct {
		profile, backend string
		capW             float64
	}{
		{"amd-instinct-mi300x-8gpu", "host-amd", 500},
		{"nvidia-h100-nvl-8gpu", "host-nvidia", 300},
	} {
		t.Run(c.profile, func(t *testing.T) {
			n, tree, p := hwemutest.NewNodeWith(t, c.profile, nil, hwemu.Options{Live: true})
			view := hwfusetest.Mount(t, n, tree, nil)
			e := useEmulatedNode(t, view, nil)
			dyn, err := reconcileEmulated(t, e.controller(t), e.nodeObject(p), nodeTwinWithGPUCap(e.name, float64Ptr(c.capW), nil))
			if err != nil {
				t.Fatalf("reconcileOnce: %v", err)
			}
			if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != c.backend || s["result"] != "applied" {
				t.Fatalf("gpu control status %v, want %s applied", s, c.backend)
			}
			for _, g := range n.State().GPUs {
				if g.LimitW != c.capW {
					t.Errorf("GPU %d holds a %v W limit right after the reconcile, want %v W, before any step", g.Index, g.LimitW, c.capW)
				}
			}
		})
	}
}
