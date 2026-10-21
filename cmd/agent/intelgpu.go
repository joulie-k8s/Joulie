package main

// Intel GPU power caps through the hwmon device the i915 or the xe driver
// registers under each discrete GPU, under the host /sys the agent mounts at
// /host-sys. The agent runs no Intel tool: xpu-smi and Level Zero Sysman
// read and write the same files.
//
// The device-level hwmon is named after its driver, i915 or xe; i915 also
// names per-tile devices i915_gt<N>, which carry energy only (linux
// v7.3-rc5 drivers/gpu/drm/i915/i915_hwmon.c:930-946). Both drivers register
// it for discrete GPUs alone, and xe not for an SR-IOV virtual function
// (i915_hwmon.c:916-918, drivers/gpu/drm/xe/xe_hwmon.c:1549-1555), so an
// integrated GPU has no hwmon and is neither capped nor counted.
//
// The cap is the sustained limit PL1, power<N>_max, in microwatts, read and
// written as root; the default limit (TDP) is power<N>_rated_max, read only
// (Documentation/ABI/testing/sysfs-driver-intel-i915-hwmon:9-28,
// sysfs-driver-intel-xe-hwmon:1-21,41-60). i915 has one channel, power1. xe
// has power1 for the card and power2 for the package, each present only
// where the platform has that limit: from 6.10 DG2 and Ponte Vecchio have
// the package limit alone, power2 (6.8 and 6.9 showed it as power1), and the
// platforms whose limits go through the firmware mailbox (Battlemage,
// Crescent Island) show each limit the firmware enables and no
// power<N>_rated_max (xe_hwmon.c:283-291,1081-1112; drivers/gpu/drm/xe/
// xe_pci.c:362,381,419,477). The agent caps the first of power1 and power2
// the driver exposes. power<N>_cap on xe is the burst limit PL2
// (sysfs-driver-intel-xe-hwmon:269-281) and is never written.
//
// The drivers take any value and the hardware clamps it, on its own, to a
// minimum and a maximum the sysfs files do not show; a read shows the
// clamped limit (i915_hwmon.c:403-441, xe_hwmon.c:335-383). The ABI bounds a
// write to the TDP, so the range the agent publishes and writes within is
// power<N>_rated_max at most, with no known minimum, as Level Zero Sysman
// reports it (compute-runtime level_zero/sysman/source/api/power/linux/
// sysman_os_power_imp.cpp: maxLimit is the default limit, minLimit -1). A
// device without power<N>_rated_max has no known range and is never
// written; Intel's out-of-tree i915 gives DG2 none (intel-gpu-i915-backports
// backport/main drivers/gpu/drm/i915/i915_hwmon.c:857-862). A limit the
// hardware clamped reads back different from the write and is an error. A
// read wakes a runtime suspended GPU (i915_hwmon.c:417,430,
// xe_hwmon.c:1388), so the limits always read.

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/matbun/joulie/pkg/governor/discover"
)

const (
	intelPCIVendor  = "0x8086"
	intelDriverI915 = "i915"
	intelDriverXe   = "xe"
	// intelBackend is the control backend name of an Intel GPU cap.
	intelBackend = "host-intel"
)

// intelPCIClasses are the PCI base classes of an Intel GPU: display (0x03,
// VGA 0x0300 for Arc and integrated GPUs, other 0x0380 for Flex and Max)
// and processing accelerator (0x12). Intel's NFD rules select GPUs by these
// classes (intel-device-plugins-for-kubernetes deployments/nfd/overlays/
// node-feature-rules/node-feature-rules.yaml).
var intelPCIClasses = map[int64]bool{0x03: true, 0x12: true}

// intelGPUModels names the Intel data center GPUs by PCI device id, as
// pci.ids does (https://pci-ids.ucw.cz/read/PC/8086, 2026-10), with the
// vendor prefix the catalogue aliases carry. Another Intel GPU takes its
// model from the gpu.intel.com/product label.
var intelGPUModels = map[string]string{
	"0x56c0": "Intel Data Center GPU Flex 170",
	"0x56c1": "Intel Data Center GPU Flex 140",
	"0x56c2": "Intel Data Center GPU Flex 170V",
	"0x0b69": "Intel Data Center GPU Max 1450",
	"0x0b6e": "Intel Data Center GPU Max 1100C",
	"0x0bd4": "Intel Data Center GPU Max 1550VG",
	"0x0bd5": "Intel Data Center GPU Max 1550",
	"0x0bd6": "Intel Data Center GPU Max 1550",
	"0x0bd7": "Intel Data Center GPU Max 1350",
	"0x0bd8": "Intel Data Center GPU Max 1350",
	"0x0bd9": "Intel Data Center GPU Max 1100",
	"0x0bda": "Intel Data Center GPU Max 1100",
	"0x0bdb": "Intel Data Center GPU Max 1100",
}

// errNoIntelGPUHwmon is a node where no Intel PCI function has an i915 or
// xe hwmon device with a power limit: no driver, only integrated GPUs, a
// virtual function, or a platform whose firmware gives no limit.
var errNoIntelGPUHwmon = errors.New("no i915 or xe hwmon device with a power limit under " + amdgpuPCIDevicesDir)

// intelGPUCapFile is the sustained limit the agent caps in the i915 or xe
// hwmon directory dir, power1_max or power2_max, or "" when there is none.
// name is the hwmon's name: i915 has power1 only.
func intelGPUCapFile(dir, name string) string {
	channels := []string{"power1"}
	if name == intelDriverXe {
		channels = append(channels, "power2")
	}
	for _, ch := range channels {
		if _, err := os.Stat(filepath.Join(dir, ch+"_max")); err == nil {
			return ch + "_max"
		}
	}
	return ""
}

// intelRatedFile is the default limit file of the cap file capFile:
// power1_rated_max for power1_max.
func intelRatedFile(capFile string) string {
	return strings.TrimSuffix(capFile, "_max") + "_rated_max"
}

// intelGPUHwmon is the device-level i915 or xe hwmon directory of the PCI
// function dev, with its name and cap file, or "" when dev is not an Intel
// GPU with a power limit: another vendor, a class that is neither display
// nor accelerator (the Intel NPU and QAT are accelerators of other classes
// or have no hwmon), a driver link naming another driver, or no hwmon with
// power1_max or power2_max.
func intelGPUHwmon(dev string) (dir, name, capFile string) {
	if w, _ := discover.ReadWord(filepath.Join(dev, "vendor")); w != intelPCIVendor {
		return "", "", ""
	}
	class, _ := discover.ReadWord(filepath.Join(dev, "class"))
	c, err := strconv.ParseInt(strings.TrimPrefix(class, "0x"), 16, 64)
	if err != nil || !intelPCIClasses[c>>16] {
		return "", "", ""
	}
	if target, err := os.Readlink(filepath.Join(dev, "driver")); err == nil {
		if d := filepath.Base(target); d != intelDriverI915 && d != intelDriverXe {
			return "", "", ""
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*"))
	sort.Strings(dirs)
	for _, d := range dirs {
		n, _ := discover.ReadWord(filepath.Join(d, "name"))
		if n != intelDriverI915 && n != intelDriverXe {
			continue
		}
		if f := intelGPUCapFile(d, n); f != "" {
			return d, n, f
		}
		return "", "", ""
	}
	return "", "", ""
}

// intelGPUCap is the cap file of the listed Intel device d, from its hwmon
// directory, or "" when it has none.
func intelGPUCap(d GPUDevice) string {
	if d.HwmonDir == "" {
		return ""
	}
	name, _ := discover.ReadWord(filepath.Join(d.HwmonDir, "name"))
	return intelGPUCapFile(d.HwmonDir, name)
}

// listIntelDevices lists every Intel discrete GPU with a power limit in bus
// id order: its model from the device id, its cap and its maximum,
// power<N>_rated_max, 0 when the driver has none. The minimum is unknown
// and stays 0. A cap that does not read is unknown, and is written
// whatever the target.
func listIntelDevices() ([]GPUDevice, error) {
	root := hostSysPath(amdgpuPCIDevicesDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoIntelGPUHwmon, err)
	}
	var devices []GPUDevice
	for _, e := range entries {
		dev := filepath.Join(root, e.Name())
		hw, _, capFile := intelGPUHwmon(dev)
		if hw == "" {
			continue
		}
		id, _ := discover.ReadWord(filepath.Join(dev, "device"))
		d := GPUDevice{
			Index:    len(devices),
			Vendor:   "intel",
			Product:  intelGPUModels[strings.ToLower(id)],
			BusID:    strings.ToLower(e.Name()),
			HwmonDir: hw,
		}
		if w, err := readHwmonWatts(filepath.Join(hw, capFile)); err == nil {
			d.CurrentCapWatts = w
		} else {
			d.CapUnknown = true
		}
		d.MaxCapWatts, _ = readHwmonWatts(filepath.Join(hw, intelRatedFile(capFile)))
		devices = append(devices, d)
	}
	if len(devices) == 0 {
		return nil, errNoIntelGPUHwmon
	}
	return devices, nil
}

// intelGPUSysfsPresent reports whether the node has an Intel discrete GPU
// with a power limit.
func intelGPUSysfsPresent() bool {
	_, err := listIntelDevices()
	return err == nil
}

// intelDataCenterFamilies are the gpu.intel.com/family values Intel's NFD
// platform rules give its data center GPUs, matched by PCI device id
// (intel-device-plugins-for-kubernetes deployments/nfd/overlays/
// node-feature-rules/platform-labeling-rules.yaml). Its A_Series and
// B_Series also cover integrated GPUs.
var intelDataCenterFamilies = map[string]bool{"Flex_Series": true, "Max_Series": true}

// intelGPULabelled reports whether the node labels show an Intel discrete
// GPU: a data center family label, or an Intel display or accelerator PCI
// label (NFD's pci-<class>_8086 and pci-8086, or Intel's
// intel.feature.node.kubernetes.io/gpu) that an i915 or xe hwmon with a
// power limit confirms. An integrated GPU, an Intel NPU and QAT carry the
// PCI labels too, and have no such hwmon.
func intelGPULabelled(labels map[string]string) bool {
	if intelDataCenterFamilies[labels["gpu.intel.com/family"]] {
		return true
	}
	pci := labels["intel.feature.node.kubernetes.io/gpu"] == "true"
	for _, k := range []string{"pci-8086", "pci-0300_8086", "pci-0380_8086", "pci-1200_8086"} {
		pci = pci || labels["feature.node.kubernetes.io/"+k+".present"] == "true"
	}
	return pci && intelGPUSysfsPresent()
}

// intelGPURanged is the devices whose cap range is known, the ones a cap is
// resolved against and written to.
func intelGPURanged(devices []GPUDevice) []GPUDevice {
	var out []GPUDevice
	for _, d := range devices {
		if d.MaxCapWatts > 0 {
			out = append(out, d)
		}
	}
	return out
}

// intelGPUUnknownRange is why no Intel GPU of devices is capped: none
// publishes its default limit, so the range is unknown.
func intelGPUUnknownRange(devices []GPUDevice) string {
	if len(devices) == 0 {
		return "no Intel GPU with a power limit"
	}
	d := devices[0]
	return fmt.Sprintf("the power limit range of the Intel GPUs is unknown: gpu %s has no %s, so no cap is written",
		d.BusID, intelRatedFile(intelGPUCap(d)))
}

// applyIntelGPUCap writes capWatts, in whole watts and never above the
// device's power<N>_rated_max, to the cap file of every device, and reads it
// back. A device known to be at that cap is left alone: the listing is this
// reconcile's, so a limit reset by a driver reload shows there and is set
// again. A device with no known range is not written. A device that fails,
// to write or to read back what was written, is an error naming its bus id;
// the other devices are still capped.
func applyIntelGPUCap(devices []GPUDevice, capWatts float64) error {
	var errs []error
	for _, d := range devices {
		f := intelGPUCap(d)
		if f == "" {
			errs = append(errs, fmt.Errorf("gpu %s: no power1_max or power2_max", d.BusID))
			continue
		}
		if d.MaxCapWatts <= 0 {
			errs = append(errs, fmt.Errorf("gpu %s: no %s, the power limit range is unknown, not written", d.BusID, intelRatedFile(f)))
			continue
		}
		target := math.Min(math.Round(capWatts), math.Floor(d.MaxCapWatts))
		if !d.CapUnknown && math.Round(d.CurrentCapWatts) == target {
			continue
		}
		want := strconv.FormatInt(int64(target)*microwattsPerWatt, 10)
		path := filepath.Join(d.HwmonDir, f)
		if err := writeSysfsAttr(path, want); err != nil {
			errs = append(errs, fmt.Errorf("gpu %s: write %s: %w", d.BusID, f, err))
			continue
		}
		if got, ok := discover.ReadWord(path); !ok || got != want {
			errs = append(errs, fmt.Errorf("gpu %s: %s reads %q after writing %s", d.BusID, f, got, want))
		}
	}
	return errors.Join(errs...)
}
