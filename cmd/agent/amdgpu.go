package main

// AMD GPU power caps through the amdgpu hwmon device of each GPU, under the
// host /sys the agent mounts at /host-sys. The amdgpu driver registers one
// hwmon device named amdgpu under each PCI function it drives; its
// power1_cap (read-write, root only), power1_cap_min and power1_cap_max are
// in microwatts, and the driver keeps whole watts (Linux
// drivers/gpu/drm/amd/pm/amdgpu_pm.c, DOC: hwmon). rocm-smi and amd-smi read
// and write the same files, so the agent runs no AMD tool.
//
// The range is power1_cap_min to power1_cap_max, the bound the driver
// enforces on a write. power1_cap_default is not read: the driver sets it
// from the limit in force each time it initialises the GPU, so after a
// resume or a reset it holds the last cap written.
//
// The driver refuses to read these files while the GPU is runtime suspended
// or in reset, and sets the cap in force again when the GPU resumes. A
// device that refuses the read is listed with the values the agent last read
// or wrote; one it has neither read nor written is listed with an unknown
// cap and written whatever the target, since a write wakes the GPU.

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/matbun/joulie/pkg/governor/discover"
)

const (
	amdgpuName          = "amdgpu"
	amdgpuCapFile       = "power1_cap"
	amdgpuCapMinFile    = "power1_cap_min"
	amdgpuCapMaxFile    = "power1_cap_max"
	amdgpuPCIDevicesDir = "bus/pci/devices"
	amdgpuPCIVendor     = "0x1002"
	microwattsPerWatt   = 1000000
)

// errNoAMDGPUHwmon is a node where no AMD PCI function has an amdgpu hwmon
// device with a power cap: no amdgpu driver, power management off, a guest
// the driver gives no hwmon, or only an APU's integrated GPU.
var errNoAMDGPUHwmon = errors.New("no amdgpu hwmon device under " + amdgpuPCIDevicesDir)

// amdgpuHwmonDir is the amdgpu hwmon directory of the PCI function dev, or ""
// when dev is not an AMD function the amdgpu driver drives with a power cap.
// The GPU's HD audio function has AMD's vendor id and no hwmon; a driver
// link that names another driver rules a function out; the driver gives an
// APU's integrated GPU, other than the few it caps, a hwmon without
// power1_cap, and such a function is not a GPU the agent caps or counts.
func amdgpuHwmonDir(dev string) string {
	if w, _ := discover.ReadWord(filepath.Join(dev, "vendor")); w != amdgpuPCIVendor {
		return ""
	}
	if target, err := os.Readlink(filepath.Join(dev, "driver")); err == nil && filepath.Base(target) != amdgpuName {
		return ""
	}
	dirs, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*"))
	sort.Strings(dirs)
	for _, d := range dirs {
		if w, _ := discover.ReadWord(filepath.Join(d, "name")); w != amdgpuName {
			continue
		}
		if _, err := os.Stat(filepath.Join(d, amdgpuCapFile)); err != nil {
			return ""
		}
		return d
	}
	return ""
}

// readHwmonWatts reads a hwmon power attribute in microwatts as watts. A
// read the driver refuses is an error; an empty line (the driver prints one
// when the firmware query fails) or a value that is not a number reads 0.
func readHwmonWatts(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || v < 0 {
		return 0, nil
	}
	return float64(v) / microwattsPerWatt, nil
}

// amdgpuCaps is the cap and the cap range of one amdgpu hwmon device, in
// watts.
type amdgpuCaps struct{ cap, min, max float64 }

// amdgpuLastCaps holds, per amdgpu hwmon directory, the caps the agent last
// read or wrote, for a device that refuses the read.
var amdgpuLastCaps = struct {
	sync.Mutex
	byDir map[string]amdgpuCaps
}{byDir: map[string]amdgpuCaps{}}

func rememberAmdgpuCaps(dir string, c amdgpuCaps) {
	amdgpuLastCaps.Lock()
	defer amdgpuLastCaps.Unlock()
	amdgpuLastCaps.byDir[dir] = c
}

func lastAmdgpuCaps(dir string) (amdgpuCaps, bool) {
	amdgpuLastCaps.Lock()
	defer amdgpuLastCaps.Unlock()
	c, ok := amdgpuLastCaps.byDir[dir]
	return c, ok
}

// readAmdgpuRange reads power1_cap_min and power1_cap_max of the hwmon
// directory dir, 0 for one that does not read.
func readAmdgpuRange(dir string) (minW, maxW float64) {
	minW, _ = readHwmonWatts(filepath.Join(dir, amdgpuCapMinFile))
	maxW, _ = readHwmonWatts(filepath.Join(dir, amdgpuCapMaxFile))
	return minW, maxW
}

// listAmdDevices lists every amdgpu PCI function with a power cap in bus id
// order, with the cap range and the current cap of its hwmon device. A
// function whose power1_cap reads 0 W, such as a secondary die whose primary
// holds the package cap, is listed with no cap of its own.
func listAmdDevices() ([]GPUDevice, error) {
	root := hostSysPath(amdgpuPCIDevicesDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoAMDGPUHwmon, err)
	}
	var devices []GPUDevice
	for _, e := range entries {
		hw := amdgpuHwmonDir(filepath.Join(root, e.Name()))
		if hw == "" {
			continue
		}
		d := GPUDevice{
			Index:    len(devices),
			Vendor:   "amd",
			BusID:    strings.ToLower(e.Name()),
			HwmonDir: hw,
		}
		if capW, err := readHwmonWatts(filepath.Join(hw, amdgpuCapFile)); err == nil {
			c := amdgpuCaps{cap: capW}
			c.min, c.max = readAmdgpuRange(hw)
			rememberAmdgpuCaps(hw, c)
			d.CurrentCapWatts, d.MinCapWatts, d.MaxCapWatts = c.cap, c.min, c.max
		} else if c, ok := lastAmdgpuCaps(hw); ok {
			d.CurrentCapWatts, d.MinCapWatts, d.MaxCapWatts = c.cap, c.min, c.max
		} else {
			d.CapUnknown = true
		}
		devices = append(devices, d)
	}
	if len(devices) == 0 {
		return nil, errNoAMDGPUHwmon
	}
	return devices, nil
}

// amdgpuSysfsPresent reports whether the node has an amdgpu hwmon device
// with a power cap.
func amdgpuSysfsPresent() bool {
	_, err := listAmdDevices()
	return err == nil
}

// amdgpuOwnCap reports whether d has a power cap of its own to set.
func amdgpuOwnCap(d GPUDevice) bool {
	return d.HwmonDir != "" && (d.CurrentCapWatts > 0 || d.CapUnknown)
}

// amdgpuCapDevices is the devices that have a power cap of their own.
func amdgpuCapDevices(devices []GPUDevice) []GPUDevice {
	var out []GPUDevice
	for _, d := range devices {
		if amdgpuOwnCap(d) {
			out = append(out, d)
		}
	}
	return out
}

// amdgpuCapSets splits an inventory for a cap: capped is every device with
// a cap of its own, which the cap is written to, and ranged is those of them
// whose cap and range are known, which a cap is resolved against and the
// published range comes from. ranged is capped when no range is known.
func amdgpuCapSets(devices []GPUDevice) (capped, ranged []GPUDevice) {
	capped = amdgpuCapDevices(devices)
	for _, d := range capped {
		if !d.CapUnknown {
			ranged = append(ranged, d)
		}
	}
	if len(ranged) == 0 {
		ranged = capped
	}
	return capped, ranged
}

// applyAmdGPUCap writes capWatts, rounded to the whole watts the driver
// keeps, to power1_cap of every device with a cap of its own, and reads it
// back. A device known to be at that cap is left alone: the listing is this
// reconcile's, so a cap the driver put back after a reset shows there and is
// set again. A failed write or a readback that differs is an error naming
// the bus id; the other devices are still capped.
func applyAmdGPUCap(devices []GPUDevice, capWatts float64) error {
	target := int64(math.Round(capWatts))
	want := strconv.FormatInt(target*microwattsPerWatt, 10)
	var errs []error
	for _, d := range devices {
		if !amdgpuOwnCap(d) || (!d.CapUnknown && int64(math.Round(d.CurrentCapWatts)) == target) {
			continue
		}
		f := filepath.Join(d.HwmonDir, amdgpuCapFile)
		if err := writeSysfsAttr(f, want); err != nil {
			errs = append(errs, fmt.Errorf("gpu %s: write %s: %w", d.BusID, amdgpuCapFile, err))
			continue
		}
		if got, ok := discover.ReadWord(f); !ok || got != want {
			errs = append(errs, fmt.Errorf("gpu %s: %s reads %q after writing %s", d.BusID, amdgpuCapFile, got, want))
			continue
		}
		c := amdgpuCaps{cap: float64(target), min: d.MinCapWatts, max: d.MaxCapWatts}
		if d.CapUnknown {
			c.min, c.max = readAmdgpuRange(d.HwmonDir)
		}
		rememberAmdgpuCaps(d.HwmonDir, c)
	}
	return errors.Join(errs...)
}
