package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
)

// intelFlexProfile is the emulated node of four Data Center GPU Flex 170
// under the i915 driver.
const intelFlexProfile = "intel-flex-170-4gpu"

// intelHwmonDirs lists the i915 or xe hwmon directory of every GPU of the
// emulated node, in bus id order, resolved through the bus/pci/devices
// links the agent lists.
func intelHwmonDirs(t *testing.T, e *emulatedNode) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(e.tree.SysRoot, "bus", "pci", "devices", "*", "hwmon", "hwmon*", "name"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range names {
		if b, err := os.ReadFile(n); err == nil && (strings.TrimSpace(string(b)) == intelDriverI915 || strings.TrimSpace(string(b)) == intelDriverXe) {
			out = append(out, filepath.Dir(n))
		}
	}
	sort.Strings(out)
	return out
}

// withIntelXe drives the Flex 170 node's GPUs with xe instead of i915.
func withIntelXe(p *hwemu.Profile) {
	p.GPUs.Intel.Driver = hwemu.Fact[string]{V: hwemu.IntelDriverXe, Src: "assumed:the xe layout of the same GPUs"}
}

// A39: under xe the Flex 170's limit is the package one, power2_max: the
// agent lists the GPUs with the 0 to 150 W range of power2_rated_max and
// writes a 60 % cap there, never to a card channel the driver does not
// show.
func TestHwemuA39IntelXeCapsThePackageLimit(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, intelFlexProfile, withIntelXe)
	e := useEmulatedNode(t, tree, nil)
	node := e.nodeObject(p)
	dirs := intelHwmonDirs(t, e)
	if len(dirs) != p.GPUs.Count.V {
		t.Fatalf("%d xe hwmon devices, want %d", len(dirs), p.GPUs.Count.V)
	}
	hw := discoverHardware(context.Background(), node)
	if hw.GPUVendor != "intel" || hw.GPUCount != 4 || !hw.GPUControl || hw.GPUCapMaxWatts != 150 || hw.GPUModel != "INTEL_DATA_CENTER_GPU_FLEX_170" {
		t.Fatalf("vendor %q count %d control %v max %v W model %q (list error %v), want intel, 4, control, 150 W and the Flex 170",
			hw.GPUVendor, hw.GPUCount, hw.GPUControl, hw.GPUCapMaxWatts, hw.GPUModel, hw.GPUListErr)
	}
	dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithGPUCap(e.name, nil, float64Ptr(60)))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-intel" || s["result"] != "applied" || s["message"] != "applied gpu cap 90.0W on 4 devices" {
		t.Fatalf("gpu control status %v, want host-intel applied gpu cap 90.0W on 4 devices", s)
	}
	for i, dir := range dirs {
		if got := readTrimmed(t, filepath.Join(dir, "power2_max")); got != "90000000" {
			t.Errorf("device %d power2_max = %s, want 90000000", i, got)
		}
		if _, err := os.Stat(filepath.Join(dir, "power1_max")); err == nil {
			t.Errorf("device %d has power1_max; the Flex 170 under xe has the package limit only", i)
		}
	}
}

// A35: on the Flex 170 node NodeHardware lists the four GPUs from their
// i915 hwmon devices, with the 0 to 150 W range of power1_rated_max, the
// 150 W limit in force and control, the model from the PCI device id, which
// the catalogue knows, and no tool runs (D19).
func TestHwemuA35IntelFlexInventoryThroughSysfs(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, intelFlexProfile, nil)
	e := useEmulatedNode(t, tree, nil)
	node := e.nodeObject(p)
	if len(intelHwmonDirs(t, e)) != p.GPUs.Count.V {
		t.Fatalf("%d i915 hwmon devices, want %d", len(intelHwmonDirs(t, e)), p.GPUs.Count.V)
	}
	status := publishedStatusMap(t, node.Name, discoverHardware(context.Background(), node))
	gpu, _ := status["gpu"].(map[string]any)
	capRange, _ := gpu["capRangePerGpu"].(map[string]any)
	if gpu["vendor"] != "intel" || gpu["count"] != float64(4) || gpu["controlAvailable"] != true || gpu["currentCapWatts"] != float64(150) ||
		capRange["minWatts"] != float64(0) || capRange["maxWatts"] != float64(150) || gpu["rawModel"] != "Intel Data Center GPU Flex 170" ||
		gpu["model"] != "INTEL_DATA_CENTER_GPU_FLEX_170" || containsAny(gpu["warnings"], "gpu model not recognized") {
		t.Fatalf("NodeHardware gpu %v; want vendor intel, count 4, capRangePerGpu 0 to 150 W, currentCapWatts 150, control, "+
			"and the Flex 170 the catalogue knows (D19)", gpu)
	}
	if calls := e.runner.commands(); len(calls) > 0 {
		t.Errorf("the agent ran %v; it runs no tool on an Intel GPU node", calls)
	}
}

// A36: at full load a 60 % cap is host-intel applied, writes 90 W to every
// power1_max, and three seconds later each GPU draws at most 91 W, where
// uncapped it drew more (D19).
func TestHwemuA36IntelPercentCapLowersPower(t *testing.T) {
	n, tree, p := hwemutest.NewNode(t, intelFlexProfile, nil)
	n.SetLoad(hwemu.Load{GPUUtil: []float64{1}})
	if err := n.Step(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	for _, g := range n.State().GPUs {
		if g.PowerW <= 91 {
			t.Fatalf("gpu %d draws %.1f W uncapped at full load; the test needs it above 91 W to mean anything", g.Index, g.PowerW)
		}
	}
	e := useEmulatedNode(t, tree, nil)
	dyn, err := reconcileEmulated(t, e.controller(t), e.nodeObject(p), nodeTwinWithGPUCap(e.name, nil, float64Ptr(60)))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-intel" || s["result"] != "applied" || s["message"] != "applied gpu cap 90.0W on 4 devices" {
		t.Fatalf("gpu control status %v, want host-intel applied gpu cap 90.0W on 4 devices (D19)", s)
	}
	for i, dir := range intelHwmonDirs(t, e) {
		if got := readTrimmed(t, filepath.Join(dir, "power1_max")); got != "90000000" {
			t.Errorf("device %d power1_max = %s, want 90000000 (60 %% of 150 W)", i, got)
		}
	}
	if err := n.Step(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	for _, g := range n.State().GPUs {
		if g.PowerW > 91 {
			t.Errorf("gpu %d draws %.1f W three seconds after a 90 W cap, want at most 91 W", g.Index, g.PowerW)
		}
	}
}

// readsLikeTheDriver sends every agent write through the emulated driver
// at once (checkWritesThroughNode) and then has the node read the files
// back, so the agent's next read shows the limit in effect, as i915 shows
// the limit the hardware clamped (i915_hwmon.c:403-441).
func readsLikeTheDriver(t *testing.T, n *hwemu.Node, tree *hwemu.Tree) {
	t.Helper()
	checkWritesThroughNode(t, n, tree)
	through := writeSysfsAttr
	writeSysfsAttr = func(path, value string) error {
		if err := through(path, value); err != nil {
			return err
		}
		return n.Step(0)
	}
	t.Cleanup(func() { writeSysfsAttr = through })
}

// A37: with every write answered and read back as the driver does it, an
// absolute 100 W cap is applied, and a 30 W cap, below the floor the
// hardware clamps to and sysfs does not show, reads back 40 W on every GPU:
// host-intel error naming each device and the value it reads, never
// applied.
func TestHwemuA37IntelClampedCapIsAnError(t *testing.T) {
	n, tree, p := hwemutest.NewNode(t, intelFlexProfile, nil)
	e := useEmulatedNode(t, tree, nil)
	readsLikeTheDriver(t, n, tree)
	node := e.nodeObject(p)

	dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithGPUCap(e.name, float64Ptr(100), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if s := controlStatus(t, dyn, e.name, "gpu"); s["backend"] != "host-intel" || s["result"] != "applied" || s["message"] != "applied gpu cap 100.0W on 4 devices" {
		t.Fatalf("gpu control status %v, want host-intel applied gpu cap 100.0W on 4 devices", s)
	}
	dirs := intelHwmonDirs(t, e)
	for i, dir := range dirs {
		if got := readTrimmed(t, filepath.Join(dir, "power1_max")); got != "100000000" {
			t.Errorf("device %d power1_max = %s, want 100000000", i, got)
		}
	}

	dyn, err = reconcileEmulated(t, e.controller(t), node, nodeTwinWithGPUCap(e.name, float64Ptr(30), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	s := controlStatus(t, dyn, e.name, "gpu")
	msg, _ := s["message"].(string)
	if s["backend"] != "host-intel" || s["result"] != "error" || !strings.Contains(msg, `power1_max reads "40000000" after writing 30000000`) {
		t.Fatalf("gpu control status %v, want host-intel error with the clamped readback", s)
	}
	if got := strings.Count(msg, "after writing"); got != len(dirs) {
		t.Errorf("the message names %d devices, want all %d: %s", got, len(dirs), msg)
	}
}

// A38: a write that fails on one GPU is host-intel error naming its bus id
// alone, and the other GPUs are still capped; a write that does not take
// is an error caught by the readback.
func TestHwemuA38IntelFailedWriteNamesTheDeviceAndCapsTheOthers(t *testing.T) {
	_, tree, p := hwemutest.NewNode(t, intelFlexProfile, nil)
	e := useEmulatedNode(t, tree, nil)
	node := e.nodeObject(p)
	dirs := intelHwmonDirs(t, e)
	bad := filepath.Join(dirs[1], "power1_max")
	badBus := filepath.Base(filepath.Dir(filepath.Dir(dirs[1])))
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	// The listing reads power1_max; a directory does not read, so the cap
	// of that GPU is unknown and it is written whatever the target.
	dyn, err := reconcileEmulated(t, e.controller(t), node, nodeTwinWithGPUCap(e.name, float64Ptr(120), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	s := controlStatus(t, dyn, e.name, "gpu")
	msg, _ := s["message"].(string)
	if s["backend"] != "host-intel" || s["result"] != "error" || !strings.Contains(msg, "gpu "+badBus+": write power1_max") || strings.Count(msg, "gpu ") != 1 {
		t.Fatalf("gpu control status %v, want host-intel error naming %s alone", s, badBus)
	}
	for i, dir := range dirs {
		if i == 1 {
			continue
		}
		if got := readTrimmed(t, filepath.Join(dir, "power1_max")); got != "120000000" {
			t.Errorf("device %d power1_max = %s, want 120000000 although device 1 failed", i, got)
		}
	}

	_, tree, p = hwemutest.NewNode(t, intelFlexProfile, nil)
	e = useEmulatedNode(t, tree, nil)
	old := writeSysfsAttr
	writeSysfsAttr = func(path, value string) error { return nil }
	t.Cleanup(func() { writeSysfsAttr = old })
	dyn, err = reconcileEmulated(t, e.controller(t), e.nodeObject(p), nodeTwinWithGPUCap(e.name, float64Ptr(100), nil))
	if err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	s = controlStatus(t, dyn, e.name, "gpu")
	msg, _ = s["message"].(string)
	if s["result"] != "error" || strings.Count(msg, `power1_max reads "150000000" after writing 100000000`) != p.GPUs.Count.V {
		t.Fatalf("gpu control status %v, want an error with the readback of every device", s)
	}
}
