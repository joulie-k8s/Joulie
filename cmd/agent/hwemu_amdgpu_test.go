package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
)

// A06b: the stock agent image has no rocm-smi. On the MI300X node the
// vendor comes from the NFD accelerator-class label (D8), and the cap range
// and the cap from amdgpu sysfs: vendor amd, 8 GPUs, 0 to 750 W with
// control, and a 60 % cap writes 450 W to every power1_cap (D1).
func TestHwemuA06bMI300XCapsWithoutAnyAMDTool(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "amd-instinct-mi300x-8gpu", withoutGPUTools)
	e := useEmulatedNode(t, tree, nil)
	node := e.nodeObject(p)
	hw := discoverHardware(context.Background(), node)
	if hw.GPUVendor != "amd" || hw.GPUCount != 8 || !hw.GPUCapKnown || hw.GPUCapMinWatts != 0 || hw.GPUCapMaxWatts != 750 || !hw.GPUControl {
		t.Fatalf("gpu vendor %q count %d range known=%v %v to %v W control %v (list error %v); want amd, 8, 0 to 750 W with control from amdgpu sysfs",
			hw.GPUVendor, hw.GPUCount, hw.GPUCapKnown, hw.GPUCapMinWatts, hw.GPUCapMaxWatts, hw.GPUControl, hw.GPUListErr)
	}
	dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithGPUCap(e.name, nil, float64Ptr(60)))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-amd" || s["result"] != "applied" {
		t.Fatalf("gpu control status %v, want backend host-amd, result applied", s)
	}
	for i, dir := range amdgpuHwmonDirs(t, e) {
		if got := readTrimmed(t, filepath.Join(dir, "power1_cap")); got != "450000000" {
			t.Errorf("device %d power1_cap = %s, want 450000000 (60 %% of 750 W)", i, got)
		}
	}
}

// A06c: a cap write that does not take effect is host-amd error naming the
// value written, never applied: the agent reads every power1_cap back (D2).
func TestHwemuA06cDroppedCapWriteIsAnError(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, "amd-instinct-mi300x-8gpu", nil)
	e := useEmulatedNode(t, tree, nil)
	old := writeSysfsAttr
	writeSysfsAttr = func(path, value string) error { return nil }
	t.Cleanup(func() { writeSysfsAttr = old })

	dyn, err := reconcileEmulated(t, e.controller(t), e.nodeObject(p), nodeTwinWithGPUCap(e.name, float64Ptr(500), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	s := controlStatus(t, dyn, e.name, "gpu")
	msg, _ := s["message"].(string)
	if s["backend"] != "host-amd" || s["result"] != "error" || !strings.Contains(msg, `power1_cap reads "750000000" after writing 500000000`) {
		t.Fatalf("gpu control status %v, want host-amd error with the readback of every device", s)
	}
	if n := strings.Count(msg, "after writing"); n != p.GPUs.Count.V {
		t.Errorf("the message names %d devices, want all %d: %s", n, p.GPUs.Count.V, msg)
	}
}
