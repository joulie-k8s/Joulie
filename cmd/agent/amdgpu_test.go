package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// amdgpuTestDevice is the files of one PCI function with an amdgpu hwmon
// device, the cap attributes in microwatts.
func amdgpuTestDevice(files map[string]string, bus, hwmon, capUW, minUW, maxUW, defUW string) {
	dev := "bus/pci/devices/" + bus + "/"
	h := dev + "hwmon/hwmon" + hwmon + "/"
	files[dev+"vendor"] = "0x1002\n"
	files[h+"name"] = "amdgpu\n"
	files[h+"power1_cap"] = capUW + "\n"
	files[h+"power1_cap_min"] = minUW + "\n"
	files[h+"power1_cap_max"] = maxUW + "\n"
	files[h+"power1_cap_default"] = defUW + "\n"
}

// The amdgpu sysfs inventory and cap write on a hand-made tree:
//   - the GPU's HD audio function (AMD vendor id, no hwmon) is not a GPU,
//     and a hwmon whose name is not amdgpu is not either;
//   - a secondary die (power1_cap reads 0) is listed with no cap of its own
//     and never written;
//   - an empty line, which the driver prints when the firmware query fails,
//     reads as unknown;
//   - the range is power1_cap_min to power1_cap_max, whatever
//     power1_cap_default reads;
//   - a write that fails on the first device is reported by its bus id
//     while the devices after it are still capped.
func TestAMDGPUSysfsInventoryAndWrite(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	amdgpuTestDevice(files, "0000:30:00.0", "4", "500000000", "100000000", "560000000", "500000000")
	amdgpuTestDevice(files, "0000:31:00.0", "5", "0", "0", "560000000", "0")
	amdgpuTestDevice(files, "0000:32:00.0", "6", "500000000", "", "560000000", "560000000")
	files["bus/pci/devices/0000:30:00.1/vendor"] = "0x1002\n"
	files["bus/pci/devices/0000:40:00.0/vendor"] = "0x1002\n"
	files["bus/pci/devices/0000:40:00.0/hwmon/hwmon9/name"] = "k10temp\n"
	files["bus/pci/devices/0000:50:00.0/vendor"] = "0x10de\n"
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})

	devs, err := listAmdDevices()
	if err != nil {
		t.Fatal(err)
	}
	var buses []string
	for _, d := range devs {
		buses = append(buses, d.BusID)
	}
	if got := strings.Join(buses, " "); got != "0000:30:00.0 0000:31:00.0 0000:32:00.0" {
		t.Fatalf("listed %s, want the three amdgpu functions in bus id order", got)
	}
	if d := devs[0]; d.Index != 0 || d.CurrentCapWatts != 500 || d.MinCapWatts != 100 || d.MaxCapWatts != 560 {
		t.Errorf("device 0 %+v, want cap 500 W and range 100 to 560 W, power1_cap_max, although power1_cap_default reads 500 W", d)
	}
	if d := devs[1]; amdgpuOwnCap(d) || d.MaxCapWatts != 560 {
		t.Errorf("secondary die %+v, want no cap of its own and the 560 W maximum", d)
	}
	if d := devs[2]; d.MinCapWatts != 0 || d.MaxCapWatts != 560 {
		t.Errorf("device 2 %+v, want an unknown minimum (empty line) and the 560 W maximum", d)
	}
	if got := amdgpuCapDevices(devs); len(got) != 2 || got[0].BusID != "0000:30:00.0" || got[1].BusID != "0000:32:00.0" {
		t.Errorf("devices with a cap of their own %+v, want 0000:30:00.0 and 0000:32:00.0", got)
	}

	// Device 0's power1_cap cannot be written.
	bad := filepath.Join(root, "bus/pci/devices/0000:30:00.0/hwmon/hwmon4/power1_cap")
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	err = applyAmdGPUCap(devs, 399.6)
	if err == nil || !strings.Contains(err.Error(), "gpu 0000:30:00.0: write power1_cap") || strings.Contains(err.Error(), "0000:31:00.0") || strings.Contains(err.Error(), "0000:32:00.0") {
		t.Fatalf("applyAmdGPUCap: %v, want the write error of 0000:30:00.0 only", err)
	}
	if got := readTrimmed(t, filepath.Join(root, "bus/pci/devices/0000:32:00.0/hwmon/hwmon6/power1_cap")); got != "400000000" {
		t.Errorf("device 2 power1_cap = %s, want 400000000 (399.6 W in whole watts) although device 0 failed", got)
	}
	if got := readTrimmed(t, filepath.Join(root, "bus/pci/devices/0000:31:00.0/hwmon/hwmon5/power1_cap")); got != "0" {
		t.Errorf("secondary die power1_cap = %s, want it untouched at 0", got)
	}
}

// The driver sets power1_cap_default from the cap in force each time it
// initialises the GPU, so after a resume or a reset it reads the agent's own
// last cap. The published maximum stays power1_cap_max, and a 100 % cap
// after a 50 % one raises the GPU back to it.
func TestAMDGPURangeIgnoresTheDefaultTheDriverResets(t *testing.T) {
	node := &corev1.Node{}
	node.Name = "amdgpu-resumed"
	node.Status.Allocatable = corev1.ResourceList{"amd.com/gpu": resource.MustParse("1")}
	root := t.TempDir()
	files := map[string]string{}
	amdgpuTestDevice(files, "0000:30:00.0", "2", "750000000", "0", "750000000", "750000000")
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})
	hwmon := filepath.Join(root, "bus/pci/devices/0000:30:00.0/hwmon/hwmon2")
	capAt := func(pct float64) (string, string) {
		t.Helper()
		hw := discoverHardware(context.Background(), node)
		_, result, msg, _, err := applyGPUIntentFrom(context.Background(), node, &GPUPowerCap{CapPctOfMax: float64Ptr(pct)}, nil,
			&gpuInventory{vendor: hw.GPUVendor, devices: hw.GPUDevices, err: hw.GPUListErr})
		if err != nil {
			t.Fatal(err)
		}
		return result, msg
	}

	if result, msg := capAt(50); result != "applied" || readTrimmed(t, filepath.Join(hwmon, "power1_cap")) != "375000000" {
		t.Fatalf("50 %% cap: %s %q, power1_cap %s; want 375000000 applied", result, msg, readTrimmed(t, filepath.Join(hwmon, "power1_cap")))
	}
	sysfsFiles(t, root, map[string]string{"bus/pci/devices/0000:30:00.0/hwmon/hwmon2/power1_cap_default": "375000000\n"})

	status := publishedStatusMap(t, node.Name, discoverHardware(context.Background(), node))
	gpu, _ := status["gpu"].(map[string]any)
	if capRange, _ := gpu["capRangePerGpu"].(map[string]any); capRange["maxWatts"] != float64(750) {
		t.Fatalf("NodeHardware gpu %v after the default reads 375 W, want capRangePerGpu.maxWatts 750, power1_cap_max", gpu)
	}
	if result, msg := capAt(100); result != "applied" || msg != "applied gpu cap 750.0W on 1 devices" {
		t.Fatalf("100 %% cap: %s %q, want applied gpu cap 750.0W on 1 devices", result, msg)
	}
	if got := readTrimmed(t, filepath.Join(hwmon, "power1_cap")); got != "750000000" {
		t.Errorf("power1_cap = %s after a 100 %% cap, want 750000000", got)
	}
}

// amdgpuSleeper makes amdgpu hwmon devices refuse reads, as the driver does
// while a GPU is runtime suspended or in reset (EPERM), and wraps
// writeSysfsAttr as the driver's power1_cap store, whose write wakes the
// GPU. An attribute that refuses reads is a directory, which exists and does
// not read, also as root.
type amdgpuSleeper struct {
	asleep map[string]map[string]string // by hwmon directory, the attributes' contents
}

func newAmdgpuSleeper(t *testing.T) *amdgpuSleeper {
	t.Helper()
	s := &amdgpuSleeper{asleep: map[string]map[string]string{}}
	old := writeSysfsAttr
	writeSysfsAttr = func(path, value string) error {
		if err := s.wake(filepath.Dir(path)); err != nil {
			return err
		}
		return old(path, value)
	}
	t.Cleanup(func() { writeSysfsAttr = old })
	return s
}

// sleep puts the GPU of the hwmon directory hw to sleep.
func (s *amdgpuSleeper) sleep(t *testing.T, hw string) {
	t.Helper()
	saved := map[string]string{}
	for _, f := range []string{"power1_cap", "power1_cap_min", "power1_cap_max", "power1_cap_default"} {
		p := filepath.Join(hw, f)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		saved[f] = string(b)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s.asleep[hw] = saved
}

func (s *amdgpuSleeper) wake(hw string) error {
	saved, ok := s.asleep[hw]
	if !ok {
		return nil
	}
	for f, content := range saved {
		p := filepath.Join(hw, f)
		if err := os.Remove(p); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	delete(s.asleep, hw)
	return nil
}

// A GPU asleep when the agent lists it is still capped: one asleep since the
// agent started takes the cap resolved from the GPU awake, and the write
// wakes it; once both sleep, NodeHardware keeps the range and the cap last
// read or written, a cap already in force is not written again (which would
// wake them), and a new one is.
func TestAMDGPUAsleepGPUIsStillCapped(t *testing.T) {
	node := &corev1.Node{}
	node.Name = "amdgpu-asleep"
	node.Status.Allocatable = corev1.ResourceList{"amd.com/gpu": resource.MustParse("2")}
	root := t.TempDir()
	files := map[string]string{}
	amdgpuTestDevice(files, "0000:30:00.0", "2", "500000000", "100000000", "560000000", "560000000")
	amdgpuTestDevice(files, "0000:31:00.0", "3", "500000000", "100000000", "560000000", "560000000")
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})
	sleeper := newAmdgpuSleeper(t)
	hw0 := filepath.Join(root, "bus/pci/devices/0000:30:00.0/hwmon/hwmon2")
	hw1 := filepath.Join(root, "bus/pci/devices/0000:31:00.0/hwmon/hwmon3")
	capWith := func(intent *GPUPowerCap) (HardwareInfo, string, string) {
		t.Helper()
		hw := discoverHardware(context.Background(), node)
		_, result, msg, _, err := applyGPUIntentFrom(context.Background(), node, intent, nil,
			&gpuInventory{vendor: hw.GPUVendor, devices: hw.GPUDevices, err: hw.GPUListErr})
		if err != nil {
			t.Fatal(err)
		}
		return hw, result, msg
	}
	caps := func() string {
		t.Helper()
		return readTrimmed(t, filepath.Join(hw0, "power1_cap")) + " " + readTrimmed(t, filepath.Join(hw1, "power1_cap"))
	}

	sleeper.sleep(t, hw0)
	hw, result, msg := capWith(&GPUPowerCap{CapPctOfMax: float64Ptr(60)})
	if hw.GPUCount != 2 || !hw.GPUControl || hw.GPUCapMinWatts != 100 || hw.GPUCapMaxWatts != 560 || hw.GPUCurrentCapW != 500 {
		t.Fatalf("count %d control %v range %v to %v W cap %v W, want 2 GPUs and the 100 to 560 W range and 500 W cap of the one awake",
			hw.GPUCount, hw.GPUControl, hw.GPUCapMinWatts, hw.GPUCapMaxWatts, hw.GPUCurrentCapW)
	}
	if result != "applied" || msg != "applied gpu cap 336.0W on 2 devices" {
		t.Fatalf("60 %% cap with one GPU asleep: %s %q, want applied gpu cap 336.0W on 2 devices", result, msg)
	}
	if got := caps(); got != "336000000 336000000" {
		t.Fatalf("power1_cap %s, want 336000000 on both GPUs", got)
	}

	sleeper.sleep(t, hw0)
	sleeper.sleep(t, hw1)
	writes := watchWrites(t, root)
	hw, result, msg = capWith(&GPUPowerCap{CapPctOfMax: float64Ptr(60)})
	if !hw.GPUControl || !hw.GPUCapKnown || hw.GPUCapMinWatts != 100 || hw.GPUCapMaxWatts != 560 || hw.GPUCurrentCapW != 336 {
		t.Fatalf("both asleep: control %v range known %v %v to %v W cap %v W, want control and the 100 to 560 W range and 336 W cap last read or written",
			hw.GPUControl, hw.GPUCapKnown, hw.GPUCapMinWatts, hw.GPUCapMaxWatts, hw.GPUCurrentCapW)
	}
	if result != "applied" || msg != "applied gpu cap 336.0W on 2 devices" {
		t.Fatalf("60 %% cap with both asleep: %s %q, want applied gpu cap 336.0W on 2 devices", result, msg)
	}
	if w := writes.take(); len(w) != 0 {
		t.Fatalf("wrote %v; the 336 W cap is in force on both, and a write wakes a GPU", w)
	}
	if _, result, msg = capWith(&GPUPowerCap{CapWattsPerGPU: float64Ptr(300)}); result != "applied" || msg != "applied gpu cap 300.0W on 2 devices" {
		t.Fatalf("300 W cap with both asleep: %s %q, want applied gpu cap 300.0W on 2 devices", result, msg)
	}
	if got := caps(); got != "300000000 300000000" {
		t.Fatalf("power1_cap %s, want 300000000 on both GPUs", got)
	}
}

// A node whose only GPU has slept since the agent started publishes no range
// and cannot resolve a percent cap; a cap in watts is written, which wakes
// the GPU, and its range is known from then on, asleep or not.
func TestAMDGPUAsleepSinceStartLearnsItsRangeFromAWrite(t *testing.T) {
	node := &corev1.Node{}
	node.Name = "amdgpu-asleep-since-start"
	node.Status.Allocatable = corev1.ResourceList{"amd.com/gpu": resource.MustParse("1")}
	root := t.TempDir()
	files := map[string]string{}
	amdgpuTestDevice(files, "0000:30:00.0", "2", "500000000", "100000000", "560000000", "560000000")
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})
	sleeper := newAmdgpuSleeper(t)
	hwmon := filepath.Join(root, "bus/pci/devices/0000:30:00.0/hwmon/hwmon2")
	sleeper.sleep(t, hwmon)

	capWith := func(intent *GPUPowerCap) (HardwareInfo, string, string) {
		t.Helper()
		hw := discoverHardware(context.Background(), node)
		_, result, msg, _, err := applyGPUIntentFrom(context.Background(), node, intent, nil,
			&gpuInventory{vendor: hw.GPUVendor, devices: hw.GPUDevices, err: hw.GPUListErr})
		if err != nil {
			t.Fatal(err)
		}
		return hw, result, msg
	}
	hw, result, msg := capWith(&GPUPowerCap{CapPctOfMax: float64Ptr(60)})
	if hw.GPUVendor != "amd" || hw.GPUCount != 1 || hw.GPUCapKnown || hw.GPUControl {
		t.Fatalf("vendor %q count %d range known %v control %v, want amd, 1 GPU, no range and no control", hw.GPUVendor, hw.GPUCount, hw.GPUCapKnown, hw.GPUControl)
	}
	if result != "blocked" || msg != "cannot resolve capPctOfMax without device max power limits" {
		t.Fatalf("60 %% cap: %s %q, want blocked: cannot resolve capPctOfMax without device max power limits", result, msg)
	}
	if _, result, msg = capWith(&GPUPowerCap{CapWattsPerGPU: float64Ptr(300)}); result != "applied" || msg != "applied gpu cap 300.0W on 1 devices" {
		t.Fatalf("300 W cap: %s %q, want applied gpu cap 300.0W on 1 devices", result, msg)
	}
	if got := readTrimmed(t, filepath.Join(hwmon, "power1_cap")); got != "300000000" {
		t.Fatalf("power1_cap %s, want 300000000", got)
	}
	sleeper.sleep(t, hwmon)
	if hw := discoverHardware(context.Background(), node); !hw.GPUControl || hw.GPUCapMinWatts != 100 || hw.GPUCapMaxWatts != 560 || hw.GPUCurrentCapW != 300 {
		t.Fatalf("asleep after the write: control %v range %v to %v W cap %v W, want control, 100 to 560 W and 300 W", hw.GPUControl, hw.GPUCapMinWatts, hw.GPUCapMaxWatts, hw.GPUCurrentCapW)
	}
}

// The integrated GPU of an APU has an amdgpu hwmon without power1_cap, which
// the driver hides there: it is not a GPU. A CPU-only node with one
// publishes no GPU, and beside a discrete GPU it is not counted.
func TestAMDGPUIntegratedGPUIsNotAGPU(t *testing.T) {
	node := &corev1.Node{}
	node.Name = "amd-apu"
	root := t.TempDir()
	files := map[string]string{
		"bus/pci/devices/0000:05:00.0/vendor":                    "0x1002\n",
		"bus/pci/devices/0000:05:00.0/hwmon/hwmon1/name":         "amdgpu\n",
		"bus/pci/devices/0000:05:00.0/hwmon/hwmon1/power1_input": "15000000\n",
		"bus/pci/devices/0000:05:00.1/vendor":                    "0x1002\n",
	}
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})
	status := publishedStatusMap(t, node.Name, discoverHardware(context.Background(), node))
	if gpu, _ := status["gpu"].(map[string]any); gpu["vendor"] != "none" || gpu["count"] != nil && gpu["count"] != float64(0) {
		t.Fatalf("NodeHardware gpu %v on a CPU-only node with an APU, want vendor none and no GPU", gpu)
	}

	node.Status.Allocatable = corev1.ResourceList{"amd.com/gpu": resource.MustParse("1")}
	amdgpuTestDevice(files, "0000:30:00.0", "2", "500000000", "100000000", "560000000", "560000000")
	sysfsFiles(t, root, files)
	hw := discoverHardware(context.Background(), node)
	if hw.GPUVendor != "amd" || hw.GPUCount != 1 || !hw.GPUControl || hw.GPUCapMinWatts != 100 || hw.GPUCapMaxWatts != 560 {
		t.Fatalf("vendor %q count %d control %v range %v to %v W, want amd, the discrete GPU only, with control and 100 to 560 W",
			hw.GPUVendor, hw.GPUCount, hw.GPUControl, hw.GPUCapMinWatts, hw.GPUCapMaxWatts)
	}
}

// A device already at the cap is not written; a write the driver drops is
// an error, caught by the readback.
func TestAMDGPUCapSkipsTheCurrentCapAndReadsBack(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	amdgpuTestDevice(files, "0000:30:00.0", "2", "300000000", "0", "750000000", "750000000")
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})
	writes := watchWrites(t, root)

	devs, err := listAmdDevices()
	if err != nil {
		t.Fatal(err)
	}
	if err := applyAmdGPUCap(devs, 300.2); err != nil {
		t.Fatal(err)
	}
	if w := writes.take(); len(w) != 0 {
		t.Fatalf("wrote %v for the cap already in force", w)
	}

	old := writeSysfsAttr
	writeSysfsAttr = func(path, value string) error { return nil }
	t.Cleanup(func() { writeSysfsAttr = old })
	err = applyAmdGPUCap(devs, 500)
	if err == nil || !strings.Contains(err.Error(), `power1_cap reads "300000000" after writing 500000000`) {
		t.Fatalf("applyAmdGPUCap with a dropped write: %v, want the readback error", err)
	}
}

// With no AMD tool, the vendor comes from amdgpu sysfs, after every tool
// probe; without an amdgpu hwmon device it stays none.
func TestDetectGPUVendorFromAMDGPUSysfs(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	amdgpuTestDevice(files, "0000:30:00.0", "2", "300000000", "0", "750000000", "750000000")
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})
	if got := detectGPUVendor(context.Background(), nil); got != "amd" {
		t.Fatalf("vendor %q with an amdgpu hwmon device and no tool, want amd", got)
	}

	pointAgentAtSysfs(t, t.TempDir(), "", fakeCommandRunner{})
	if got := detectGPUVendor(context.Background(), nil); got != "none" {
		t.Fatalf("vendor %q with no amdgpu hwmon device and no tool, want none", got)
	}
}

// The range, the cap and control come from the first device with a cap of
// its own, here behind a device whose power1_cap and power1_cap_max read 0
// (a die with its package power limit off), and a percent cap is resolved
// against that device and written to it alone; a node whose amdgpu devices
// have no cap of their own publishes no range and no control, and a cap on
// it is blocked rather than written anywhere.
func TestAMDGPURangeComesFromADeviceWithItsOwnCap(t *testing.T) {
	node := &corev1.Node{}
	node.Name = "amdgpu-range"
	node.Status.Allocatable = corev1.ResourceList{"amd.com/gpu": resource.MustParse("2")}

	root := t.TempDir()
	files := map[string]string{}
	amdgpuTestDevice(files, "0000:30:00.0", "2", "0", "0", "0", "0")
	amdgpuTestDevice(files, "0000:31:00.0", "3", "500000000", "100000000", "560000000", "560000000")
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})
	hw := discoverHardware(context.Background(), node)
	if hw.GPUVendor != "amd" || hw.GPUCount != 2 || !hw.GPUControl || hw.GPUCurrentCapW != 500 || hw.GPUCapMinWatts != 100 || hw.GPUCapMaxWatts != 560 {
		t.Fatalf("vendor %q count %d control %v cap %v range %v to %v W, want amd, 2, control, 500 W and 100 to 560 W from 0000:31:00.0",
			hw.GPUVendor, hw.GPUCount, hw.GPUControl, hw.GPUCurrentCapW, hw.GPUCapMinWatts, hw.GPUCapMaxWatts)
	}
	_, result, msg, _, err := applyGPUIntentFrom(context.Background(), node, &GPUPowerCap{CapPctOfMax: float64Ptr(60)}, nil,
		&gpuInventory{vendor: hw.GPUVendor, devices: hw.GPUDevices, err: hw.GPUListErr})
	if err != nil || result != "applied" || msg != "applied gpu cap 336.0W on 1 devices" {
		t.Fatalf("60 %% cap: %q %q (%v), want applied gpu cap 336.0W on 1 devices, resolved against 0000:31:00.0", result, msg, err)
	}
	if got := readTrimmed(t, filepath.Join(root, "bus/pci/devices/0000:31:00.0/hwmon/hwmon3/power1_cap")); got != "336000000" {
		t.Errorf("0000:31:00.0 power1_cap = %s, want 336000000", got)
	}
	if got := readTrimmed(t, filepath.Join(root, "bus/pci/devices/0000:30:00.0/hwmon/hwmon2/power1_cap")); got != "0" {
		t.Errorf("0000:30:00.0 power1_cap = %s, want it untouched at 0", got)
	}

	root = t.TempDir()
	files = map[string]string{}
	amdgpuTestDevice(files, "0000:30:00.0", "2", "0", "0", "560000000", "0")
	sysfsFiles(t, root, files)
	pointAgentAtSysfs(t, root, "", fakeCommandRunner{})
	writes := watchWrites(t, root)
	hw = discoverHardware(context.Background(), node)
	if hw.GPUVendor != "amd" || hw.GPUCount != 1 || hw.GPUControl || hw.GPUCapKnown {
		t.Fatalf("vendor %q count %d control %v range known %v, want amd, 1, and no range or control", hw.GPUVendor, hw.GPUCount, hw.GPUControl, hw.GPUCapKnown)
	}
	_, result, msg, _, err = applyGPUIntentFrom(context.Background(), node, &GPUPowerCap{CapWattsPerGPU: float64Ptr(300)}, nil,
		&gpuInventory{vendor: hw.GPUVendor, devices: hw.GPUDevices, err: hw.GPUListErr})
	if err != nil || result != "blocked" || msg != "no amdgpu device has a power cap of its own" {
		t.Fatalf("cap result %q %q (%v), want blocked: no amdgpu device has a power cap of its own", result, msg, err)
	}
	if w := writes.take(); len(w) != 0 {
		t.Fatalf("wrote %v", w)
	}
}
