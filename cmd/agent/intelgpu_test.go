package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// intelGPUTestDevice adds the files of one Intel PCI function: vendor, class
// and device id, and, when hwmon is not empty, the hwmon device hwmon<hwmon>
// with the name and attributes given (limits in microwatts).
func intelGPUTestDevice(files map[string]string, bus, class, device, hwmon, name string, attrs map[string]string) {
	dev := "bus/pci/devices/" + bus + "/"
	files[dev+"vendor"] = "0x8086\n"
	files[dev+"class"] = class + "\n"
	files[dev+"device"] = device + "\n"
	if hwmon == "" {
		return
	}
	h := dev + "hwmon/hwmon" + hwmon + "/"
	files[h+"name"] = name + "\n"
	for k, v := range attrs {
		files[h+k] = v + "\n"
	}
}

// intelDriverLink links the PCI function bus to the driver, as sysfs does.
func intelDriverLink(t *testing.T, root, bus, driver string) {
	t.Helper()
	drv := filepath.Join(root, "bus", "pci", "drivers", driver)
	if err := os.MkdirAll(drv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(drv, filepath.Join(root, "bus", "pci", "devices", bus, "driver")); err != nil {
		t.Fatal(err)
	}
}

// intelFlex170 adds a Data Center GPU Flex 170 the i915 driver drives:
// power1_max and power1_rated_max at the 150 W TBP, and a per-tile hwmon
// with energy only.
func intelFlex170(t *testing.T, root string, files map[string]string, bus, hwmon string) {
	t.Helper()
	intelGPUTestDevice(files, bus, "0x038000", "0x56c0", hwmon, "i915", map[string]string{
		"power1_max": "150000000", "power1_rated_max": "150000000", "power1_max_interval": "1000", "energy1_input": "123456",
	})
	files["bus/pci/devices/"+bus+"/hwmon/hwmon9"+hwmon+"/name"] = "i915_gt0\n"
	files["bus/pci/devices/"+bus+"/hwmon/hwmon9"+hwmon+"/energy1_input"] = "654321\n"
	sysfsFiles(t, root, files)
	intelDriverLink(t, root, bus, "i915")
}

// intelTestTree is a node with one Intel GPU of every layout the agent
// reads, and the Intel and other functions it must leave out:
//   - 0000:00:02.0, an integrated GPU (i915, no hwmon: the driver registers
//     one for discrete GPUs alone), and 0000:00:0b.0, an NPU (accelerator
//     class, another driver, no hwmon), are not GPUs;
//   - 0000:03:00.0, a Flex 170 under i915: power1_max, power1_rated_max;
//   - 0000:04:00.0, a Max 1550 under xe from 6.10: the package limit,
//     power2_max and power2_rated_max, beside the burst limit power2_cap;
//   - 0000:05:00.0, a Battlemage card under xe with mailbox limits:
//     power1_max and power2_max, no rated maximum;
//   - 0000:09:00.0, a Flex 170 under xe 6.8: power1_max, power1_rated_max;
//   - 0000:6b:00.0 (QAT) and 0000:08:00.0 (NVIDIA) are not Intel GPUs.
func intelTestTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{}
	intelGPUTestDevice(files, "0000:00:02.0", "0x030000", "0x46a6", "", "", nil)
	intelGPUTestDevice(files, "0000:00:0b.0", "0x120000", "0x7d1d", "", "", nil)
	intelGPUTestDevice(files, "0000:04:00.0", "0x038000", "0x0bd5", "4", "xe", map[string]string{
		"power2_max": "600000000", "power2_rated_max": "600000000", "power2_cap": "0", "energy2_input": "1",
	})
	intelGPUTestDevice(files, "0000:05:00.0", "0x030000", "0xe211", "5", "xe", map[string]string{
		"power1_max": "120000000", "power1_cap": "160000000", "power2_max": "110000000",
	})
	intelGPUTestDevice(files, "0000:09:00.0", "0x038000", "0x56c0", "7", "xe", map[string]string{
		"power1_max": "150000000", "power1_rated_max": "150000000",
	})
	intelGPUTestDevice(files, "0000:6b:00.0", "0x0b4000", "0x4940", "", "", nil)
	files["bus/pci/devices/0000:08:00.0/vendor"] = "0x10de\n"
	intelFlex170(t, root, files, "0000:03:00.0", "2")
	intelDriverLink(t, root, "0000:00:02.0", "i915")
	intelDriverLink(t, root, "0000:00:0b.0", "intel_vpu")
	intelDriverLink(t, root, "0000:04:00.0", "xe")
	intelDriverLink(t, root, "0000:05:00.0", "xe")
	intelDriverLink(t, root, "0000:09:00.0", "xe")
	return root
}

// The Intel sysfs inventory lists the discrete GPUs in bus id order with
// the cap file of their driver and channel, the model of a known device id
// and the maximum from power<N>_rated_max, unknown where there is none.
func TestIntelGPUSysfsInventory(t *testing.T) {
	root := intelTestTree(t)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})

	devs, err := listIntelDevices()
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ bus, capFile, product string }
	want := []row{
		{"0000:03:00.0", "power1_max", "Intel Data Center GPU Flex 170"},
		{"0000:04:00.0", "power2_max", "Intel Data Center GPU Max 1550"},
		{"0000:05:00.0", "power1_max", ""},
		{"0000:09:00.0", "power1_max", "Intel Data Center GPU Flex 170"},
	}
	if len(devs) != len(want) {
		t.Fatalf("listed %+v, want the four discrete GPUs %v", devs, want)
	}
	for i, d := range devs {
		if got := (row{d.BusID, intelGPUCap(d), d.Product}); got != want[i] || d.Index != i || d.Vendor != "intel" {
			t.Errorf("device %d: %+v (cap file %s), want %+v", i, d, intelGPUCap(d), want[i])
		}
	}
	if d := devs[0]; d.CurrentCapWatts != 150 || d.MinCapWatts != 0 || d.MaxCapWatts != 150 {
		t.Errorf("Flex 170 %+v, want cap 150 W, maximum 150 W from power1_rated_max and no known minimum", d)
	}
	if d := devs[1]; d.CurrentCapWatts != 600 || d.MaxCapWatts != 600 {
		t.Errorf("Max 1550 %+v, want the package limit power2_max 600 W, never the burst limit power2_cap", d)
	}
	if d := devs[2]; d.CurrentCapWatts != 120 || d.MaxCapWatts != 0 {
		t.Errorf("Battlemage %+v, want the card limit 120 W and an unknown maximum (no rated maximum)", d)
	}
	if got := intelGPURanged(devs); len(got) != 3 || got[0].BusID != "0000:03:00.0" || got[1].BusID != "0000:04:00.0" || got[2].BusID != "0000:09:00.0" {
		t.Errorf("devices with a known range %+v, want all but 0000:05:00.0", got)
	}
}

// A cap is written in whole watts to every device whose range is known and
// read back; a device without a rated maximum is never written, the burst
// limit never, and a device whose write fails is reported by its bus id
// while the others are still capped.
func TestIntelGPUCapWrite(t *testing.T) {
	root := intelTestTree(t)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})
	devs, err := listIntelDevices()
	if err != nil {
		t.Fatal(err)
	}
	// The Max 1550's power2_max cannot be written.
	bad := filepath.Join(root, "bus/pci/devices/0000:04:00.0/hwmon/hwmon4/power2_max")
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bad, 0o755); err != nil {
		t.Fatal(err)
	}

	err = applyIntelGPUCap(devs, 99.6)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if !strings.Contains(msg, "gpu 0000:04:00.0: write power2_max") || !strings.Contains(msg, "gpu 0000:05:00.0: no power1_rated_max") ||
		strings.Contains(msg, "0000:03:00.0") || strings.Contains(msg, "0000:09:00.0") {
		t.Fatalf("applyIntelGPUCap: %v, want the write error of 0000:04:00.0 and the unknown range of 0000:05:00.0 only", err)
	}
	for file, want := range map[string]string{
		"0000:03:00.0/hwmon/hwmon2/power1_max": "100000000",
		"0000:09:00.0/hwmon/hwmon7/power1_max": "100000000",
		"0000:04:00.0/hwmon/hwmon4/power2_cap": "0",
		"0000:05:00.0/hwmon/hwmon5/power1_max": "120000000",
		"0000:05:00.0/hwmon/hwmon5/power2_max": "110000000",
	} {
		if got := readTrimmed(t, filepath.Join(root, "bus/pci/devices", file)); got != want {
			t.Errorf("%s = %s, want %s", file, got, want)
		}
	}
}

// A device at the target is not written; a cap above the rated maximum,
// once rounded, is written at the whole watts below it; a write that does
// not take, or that the hardware clamps, is an error caught by the readback.
func TestIntelGPUCapSkipsTheCurrentCapStaysWithinTheRatedMaxAndReadsBack(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	intelGPUTestDevice(files, "0000:03:00.0", "0x038000", "0x56c0", "2", "i915", map[string]string{
		"power1_max": "120000000", "power1_rated_max": "150500000",
	})
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})
	writes := watchWrites(t, root)
	capFile := filepath.Join(root, "bus/pci/devices/0000:03:00.0/hwmon/hwmon2/power1_max")
	list := func() []GPUDevice {
		t.Helper()
		devs, err := listIntelDevices()
		if err != nil {
			t.Fatal(err)
		}
		return devs
	}

	if err := applyIntelGPUCap(list(), 120.3); err != nil {
		t.Fatal(err)
	}
	if w := writes.take(); len(w) != 0 {
		t.Fatalf("wrote %v for the cap already in force", w)
	}
	if err := applyIntelGPUCap(list(), 150.6); err != nil {
		t.Fatal(err)
	}
	if got := readTrimmed(t, capFile); got != "150000000" {
		t.Errorf("power1_max = %s after a 150.6 W cap, want 150000000, the whole watts at or below the 150.5 W rated maximum", got)
	}

	old := writeSysfsAttr
	t.Cleanup(func() { writeSysfsAttr = old })
	writeSysfsAttr = func(path, value string) error { return nil }
	err := applyIntelGPUCap(list(), 100)
	if err == nil || !strings.Contains(err.Error(), `gpu 0000:03:00.0: power1_max reads "150000000" after writing 100000000`) {
		t.Fatalf("applyIntelGPUCap with a dropped write: %v, want the readback error", err)
	}
	// The hardware clamps a limit below its own minimum, which sysfs does
	// not show, and the driver reads back the clamped value.
	writeSysfsAttr = func(path, value string) error { return old(path, "40000000") }
	err = applyIntelGPUCap(list(), 30)
	if err == nil || !strings.Contains(err.Error(), `power1_max reads "40000000" after writing 30000000`) {
		t.Fatalf("applyIntelGPUCap with a clamped write: %v, want the readback error naming the clamped value", err)
	}
}

// D19 (fixed): the agent had no Intel GPU backend, so an Intel GPU node
// published vendor none without control and its GPU cap ended blocked with
// no supported GPU backend detected: it ran uncapped. The agent now lists
// the GPUs from i915 sysfs, publishes them with their range and control,
// and writes a percent cap to every power1_max; a cap above the rated
// maximum is blocked and writes nothing.
func TestD19IntelGPUNodeIsCappedThroughSysfs(t *testing.T) {
	node := &corev1.Node{}
	node.Name = "intel-flex"
	node.Labels = map[string]string{
		"feature.node.kubernetes.io/pci-0380_8086.present": "true",
		"gpu.intel.com/family":                             "Flex_Series",
		"gpu.intel.com/product":                            "Flex_170",
		"gpu.intel.com/device.count":                       "2",
	}
	node.Status.Allocatable = corev1.ResourceList{"gpu.intel.com/i915": resource.MustParse("2")}
	root := t.TempDir()
	files := map[string]string{}
	intelFlex170(t, root, files, "0000:03:00.0", "2")
	files = map[string]string{}
	intelFlex170(t, root, files, "0000:04:00.0", "3")
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})

	hw := discoverHardware(context.Background(), node)
	if hw.GPUVendor != "intel" || hw.GPUCount != 2 || !hw.GPUControl || !hw.GPUCapKnown || hw.GPUCapMinWatts != 0 || hw.GPUCapMaxWatts != 150 ||
		hw.GPUCurrentCapW != 150 || hw.GPURawModel != "Intel Data Center GPU Flex 170" || hw.GPUModel != "INTEL_DATA_CENTER_GPU_FLEX_170" {
		t.Fatalf("vendor %q count %d control %v range %v to %v W cap %v W model %q (%q), list error %v; "+
			"want intel, 2, control, 0 to 150 W, 150 W and the Flex 170 the catalogue knows (D19)",
			hw.GPUVendor, hw.GPUCount, hw.GPUControl, hw.GPUCapMinWatts, hw.GPUCapMaxWatts, hw.GPUCurrentCapW, hw.GPURawModel, hw.GPUModel, hw.GPUListErr)
	}
	status := publishedStatusMap(t, node.Name, hw)
	gpu, _ := status["gpu"].(map[string]any)
	capRange, _ := gpu["capRangePerGpu"].(map[string]any)
	if gpu["vendor"] != "intel" || gpu["count"] != float64(2) || gpu["controlAvailable"] != true || capRange["maxWatts"] != float64(150) ||
		gpu["model"] != "INTEL_DATA_CENTER_GPU_FLEX_170" || containsAny(gpu["warnings"], "gpu model not recognized") {
		t.Fatalf("NodeHardware gpu %v, want vendor intel, count 2, control and capRangePerGpu.maxWatts 150 (D19)", gpu)
	}

	inv := &gpuInventory{vendor: hw.GPUVendor, devices: hw.GPUDevices, err: hw.GPUListErr}
	backend, result, msg, _, err := applyGPUIntentFrom(context.Background(), node, &GPUPowerCap{CapPctOfMax: float64Ptr(60)}, nil, inv)
	if err != nil || backend != "host-intel" || result != "applied" || msg != "applied gpu cap 90.0W on 2 devices" {
		t.Fatalf("60 %% cap: %s %s %q (%v), want host-intel applied gpu cap 90.0W on 2 devices (D19)", backend, result, msg, err)
	}
	for _, f := range []string{"0000:03:00.0/hwmon/hwmon2/power1_max", "0000:04:00.0/hwmon/hwmon3/power1_max"} {
		if got := readTrimmed(t, filepath.Join(root, "bus/pci/devices", f)); got != "90000000" {
			t.Errorf("%s = %s, want 90000000 (60 %% of 150 W)", f, got)
		}
	}

	writes := watchWrites(t, root)
	_, result, msg, _, err = applyGPUIntentFrom(context.Background(), node, &GPUPowerCap{CapWattsPerGPU: float64Ptr(200)}, nil, inv)
	if err != nil || result != "blocked" || msg != "requested cap 200.0W above max 150.0W on gpu 0" {
		t.Fatalf("200 W cap: %s %q (%v), want blocked above the 150 W rated maximum", result, msg, err)
	}
	if w := writes.take(); len(w) != 0 {
		t.Fatalf("wrote %v for a cap above the rated maximum", w)
	}
}

// A node whose Intel GPUs publish no rated maximum (xe with mailbox limits)
// lists them without a range or control, takes the model from Intel's
// product label, and blocks both percent and absolute caps with a message
// naming the missing file, writing nothing.
func TestIntelGPUUnknownRangeIsBlocked(t *testing.T) {
	node := &corev1.Node{}
	node.Name = "intel-bmg"
	node.Labels = map[string]string{"gpu.intel.com/product": "Arc_Pro_B60", "gpu.intel.com/family": "B_Series"}
	node.Status.Allocatable = corev1.ResourceList{"gpu.intel.com/xe": resource.MustParse("1")}
	root := t.TempDir()
	files := map[string]string{}
	intelGPUTestDevice(files, "0000:05:00.0", "0x030000", "0xe211", "5", "xe", map[string]string{
		"power1_max": "120000000", "power2_max": "110000000",
	})
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})
	writes := watchWrites(t, root)

	hw := discoverHardware(context.Background(), node)
	if hw.GPUVendor != "intel" || hw.GPUCount != 1 || hw.GPUControl || hw.GPUCapKnown || hw.GPURawModel != "Arc_Pro_B60" {
		t.Fatalf("vendor %q count %d control %v range known %v model %q, want intel, 1, no control, no range and the product label",
			hw.GPUVendor, hw.GPUCount, hw.GPUControl, hw.GPUCapKnown, hw.GPURawModel)
	}
	inv := &gpuInventory{vendor: hw.GPUVendor, devices: hw.GPUDevices, err: hw.GPUListErr}
	want := "the power limit range of the Intel GPUs is unknown: gpu 0000:05:00.0 has no power1_rated_max, so no cap is written"
	for _, intent := range []*GPUPowerCap{{CapPctOfMax: float64Ptr(60)}, {CapWattsPerGPU: float64Ptr(100)}} {
		backend, result, msg, _, err := applyGPUIntentFrom(context.Background(), node, intent, nil, inv)
		if err != nil || backend != "none" || result != "blocked" || msg != want {
			t.Errorf("cap %+v: %s %s %q (%v), want none blocked %q", intent, backend, result, msg, err, want)
		}
	}
	if w := writes.take(); len(w) != 0 {
		t.Fatalf("wrote %v", w)
	}
}

// An Intel integrated GPU, an NPU and QAT carry Intel PCI labels but no
// i915 or xe hwmon: a CPU-only node with them has GPU vendor none and no
// GPU, and no GPU file is written.
func TestIntelIntegratedGPUIsNotAGPU(t *testing.T) {
	node := &corev1.Node{}
	node.Name = "intel-igpu"
	node.Labels = map[string]string{
		"feature.node.kubernetes.io/pci-0300_8086.present": "true",
		"feature.node.kubernetes.io/pci-8086.present":      "true",
		"intel.feature.node.kubernetes.io/gpu":             "true",
		"gpu.intel.com/device-id.0300-46a6.present":        "true",
	}
	root := t.TempDir()
	files := map[string]string{}
	intelGPUTestDevice(files, "0000:00:02.0", "0x030000", "0x46a6", "", "", nil)
	intelGPUTestDevice(files, "0000:00:0b.0", "0x120000", "0x7d1d", "", "", nil)
	intelGPUTestDevice(files, "0000:6b:00.0", "0x0b4000", "0x4940", "", "", nil)
	sysfsFiles(t, root, files)
	intelDriverLink(t, root, "0000:00:02.0", "i915")
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})

	if got := detectGPUVendor(context.Background(), node.Labels); got != "none" {
		t.Fatalf("vendor %q on a node with an integrated GPU only, want none", got)
	}
	hw := discoverHardware(context.Background(), node)
	if hw.GPUVendor != "none" || hw.GPUCount != 0 || hw.GPUControl || len(hw.GPUDevices) != 0 {
		t.Fatalf("vendor %q count %d control %v devices %v, want no GPU", hw.GPUVendor, hw.GPUCount, hw.GPUControl, hw.GPUDevices)
	}
}

// callRecorder is a command runner that fails every command and records it.
type callRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *callRecorder) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return fakeCommandRunner{}.Run(context.Background(), name, args...)
}

// The Intel vendor comes from Intel's data center family label alone, from
// an Intel display PCI label that an i915 or xe hwmon confirms, before any
// GPU tool runs, and without labels from sysfs after the tool probes; the
// NVIDIA and AMD labels still come first.
func TestDetectGPUVendorIntel(t *testing.T) {
	ctx := context.Background()
	empty := t.TempDir()

	pointAgentAtSysfs(t, empty, "", fakeCommandRunner{})
	if got := detectGPUVendor(ctx, map[string]string{"gpu.intel.com/family": "Max_Series"}); got != "intel" {
		t.Errorf("vendor %q with gpu.intel.com/family=Max_Series, want intel", got)
	}
	if got := detectGPUVendor(ctx, map[string]string{"feature.node.kubernetes.io/pci-0380_8086.present": "true"}); got != "none" {
		t.Errorf("vendor %q with pci-0380_8086 and no Intel hwmon (an integrated GPU as a secondary display), want none", got)
	}

	root := t.TempDir()
	intelFlex170(t, root, map[string]string{}, "0000:03:00.0", "2")
	rec := &callRecorder{}
	pointAgentAtSysfs(t, root, "", rec)
	if got := detectGPUVendor(ctx, map[string]string{"feature.node.kubernetes.io/pci-0380_8086.present": "true"}); got != "intel" || len(rec.calls) != 0 {
		t.Errorf("vendor %q, commands %v with pci-0380_8086 and a Flex 170, want intel before any tool runs", got, rec.calls)
	}
	if got := detectGPUVendor(ctx, nil); got != "intel" || len(rec.calls) == 0 {
		t.Errorf("vendor %q, commands %v with a Flex 170 and no labels, want intel from sysfs after the tool probes", got, rec.calls)
	}
	if got := detectGPUVendor(ctx, map[string]string{"feature.node.kubernetes.io/pci-10de.present": "true", "gpu.intel.com/family": "Flex_Series"}); got != "nvidia" {
		t.Errorf("vendor %q with the NVIDIA label, want nvidia", got)
	}
}

// When the Intel inventory fails (no i915 hwmon readable here), the GPU
// count is the four GPUs Intel's gpu.intel.com/device.count states, not
// the eight gpu.intel.com/i915 units the plugin advertises with
// shared-dev-num 2, and a cap reports the failed inventory.
func TestIntelGPUCountFromTheDeviceCountLabelWithSharedUnits(t *testing.T) {
	node := &corev1.Node{}
	node.Name = "intel-shared"
	node.Labels = map[string]string{
		"gpu.intel.com/family":       "Flex_Series",
		"gpu.intel.com/product":      "Flex_170",
		"gpu.intel.com/device.count": "4",
	}
	node.Status.Allocatable = corev1.ResourceList{"gpu.intel.com/i915": resource.MustParse("8")}
	pointAgentAtSysfs(t, t.TempDir(), "", fakeCommandRunner{})

	hw := discoverHardware(context.Background(), node)
	if hw.GPUVendor != "intel" || hw.GPUCount != 4 || hw.GPUControl || hw.GPUListErr == nil || hw.GPUModel != "INTEL_DATA_CENTER_GPU_FLEX_170" {
		t.Fatalf("vendor %q count %d control %v list error %v model %q, want intel, 4 GPUs from the label, no control, "+
			"the inventory error and the Flex 170 from gpu.intel.com/product", hw.GPUVendor, hw.GPUCount, hw.GPUControl, hw.GPUListErr, hw.GPUModel)
	}
	_, result, msg, _, err := applyGPUIntentFrom(context.Background(), node, &GPUPowerCap{CapPctOfMax: float64Ptr(60)}, nil,
		&gpuInventory{vendor: hw.GPUVendor, devices: hw.GPUDevices, err: hw.GPUListErr})
	if err != nil || result != "error" || !strings.HasPrefix(msg, "gpu inventory failed: no i915 or xe hwmon device with a power limit") {
		t.Fatalf("cap: %s %q (%v), want error gpu inventory failed: no i915 or xe hwmon device with a power limit", result, msg, err)
	}
}

// The model names the agent reads, from the device id and from Intel's
// product label, are the catalogue's entries for the data center GPUs it
// lists.
func TestIntelGPUModelNamesMatchTheCatalog(t *testing.T) {
	cat := loadHardwareCatalog()
	for raw, key := range map[string]string{
		intelGPUModels["0x56c0"]: "INTEL_DATA_CENTER_GPU_FLEX_170",
		intelGPUModels["0x0bd5"]: "INTEL_DATA_CENTER_GPU_MAX_1550",
		intelGPUModels["0x0bd6"]: "INTEL_DATA_CENTER_GPU_MAX_1550",
		intelGPUModels["0x0bda"]: "INTEL_DATA_CENTER_GPU_MAX_1100",
		"Flex_170":               "INTEL_DATA_CENTER_GPU_FLEX_170",
		"Max_1550":               "INTEL_DATA_CENTER_GPU_MAX_1550",
		"Max_1100":               "INTEL_DATA_CENTER_GPU_MAX_1100",
	} {
		if got, _, ok := cat.MatchGPU(raw); !ok || got != key {
			t.Errorf("MatchGPU(%q) = %q, %v; want %s", raw, got, ok, key)
		}
	}
}
