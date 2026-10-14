package discover

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/matbun/joulie/pkg/governor"
)

// amdFamilies maps the PCI device ids of the amdgpu devices pkg/governor has
// a mapping table for to their family (pci.ids, vendor 1002).
var amdFamilies = map[string]string{
	"0x7448": governor.AMDFamilyNavi31,    // Radeon Pro W7900
	"0x744c": governor.AMDFamilyNavi31,    // Radeon RX 7900 XT/XTX
	"0x745e": governor.AMDFamilyNavi31,    // Radeon Pro W7800
	"0x7408": governor.AMDFamilyAldebaran, // Instinct MI250X
	"0x740c": governor.AMDFamilyAldebaran, // Instinct MI250X/MI250
	"0x740f": governor.AMDFamilyAldebaran, // Instinct MI210
	"0x74a0": governor.AMDFamilyMI300,     // Instinct MI300A
	"0x74a1": governor.AMDFamilyMI300,     // Instinct MI300X
	"0x74a5": governor.AMDFamilyMI300,     // Instinct MI325X
}

// AMDFamily is the pkg/governor family of a PCI device id such as 0x74a1, in
// any case; empty for a device without a mapping table, which then
// publishes its level and clock ranges with no preset.
func AMDFamily(deviceID string) string {
	return amdFamilies[strings.ToLower(deviceID)]
}

// DPMRangeMHz is the lowest and highest numbered level of a pp_dpm_* table,
// lines such as "1: 1500Mhz *". The deep-sleep line "S: <MHz>" is a sampled
// clock, not a level, and is skipped. Zero when the file is absent.
func DPMRangeMHz(path string) [2]int {
	s, ok := ReadWord(path)
	if !ok {
		return [2]int{}
	}
	var r [2]int
	for _, line := range strings.Split(s, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSpace(key)); err != nil {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		mhz, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(fields[0], "Mhz"), "MHz"))
		if err != nil || mhz <= 0 {
			continue
		}
		if r[0] == 0 || mhz < r[0] {
			r[0] = mhz
		}
		r[1] = max(r[1], mhz)
	}
	return r
}

// AMDDevice is one amdgpu device: what pkg/governor plans with and the
// attributes a governor write touches.
type AMDDevice struct {
	Facts governor.AMDFacts
	Files []string
}

// ReadAMD reads every amdgpu device under sys's bus/pci/devices: AMD's vendor
// id, a performance level file (only amdgpu creates it), and a driver link
// naming amdgpu where the tree has one. Devices are keyed by the bus id
// sysfs names them by, never by a tool's index (rocm-smi and HIP enumerate
// in opposite orders), and come sorted by it.
func ReadAMD(sys string) []AMDDevice {
	root := filepath.Join(sys, governor.PCIDevicesRelDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []AMDDevice
	for _, e := range entries {
		dev := filepath.Join(root, e.Name())
		if word(filepath.Join(dev, pciVendorFile)) != amdVendorID {
			continue
		}
		levelFile := filepath.Join(dev, governor.AMDLevelFile)
		level := word(levelFile)
		if level == "" {
			continue
		}
		if target, err := os.Readlink(filepath.Join(dev, pciDriverLink)); err == nil && filepath.Base(target) != amdgpuDriver {
			continue
		}
		deviceID := strings.ToLower(word(filepath.Join(dev, pciDeviceFile)))
		odFile := filepath.Join(dev, governor.AMDODFile)
		d := AMDDevice{
			Facts: governor.AMDFacts{
				BusID:    strings.ToLower(e.Name()),
				DeviceID: deviceID,
				Family:   AMDFamily(deviceID),
				Level:    level,
				OD:       exists(odFile),
				SclkMHz:  DPMRangeMHz(filepath.Join(dev, amdDPMSclkFile)),
				MclkMHz:  DPMRangeMHz(filepath.Join(dev, amdDPMMclkFile)),
				FclkMHz:  DPMRangeMHz(filepath.Join(dev, amdDPMFclkFile)),
			},
			Files: []string{levelFile},
		}
		if d.Facts.OD {
			d.Files = append(d.Files, odFile)
		}
		out = append(out, d)
	}
	return out
}
