package hwemu

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// The sustained (PL1) limit of an Intel discrete GPU, as i915 and xe keep it
// (linux v7.3-rc5 drivers/gpu/drm/i915/i915_hwmon.c:400-498,
// drivers/gpu/drm/xe/xe_hwmon.c:333-458). A write of power<N>_max takes any
// value: 0 disables the limit, which then reads 0; a value above 0 is stored
// in the 15 bit PL1 field in units of 1/2^PKG_PWR_UNIT W, rounded to the
// nearest unit, i915 keeping the low 15 bits of a value too large for the
// field and xe the field's maximum. The hardware clamps the limit to
// PKG_MIN_PWR and PKG_MAX_PWR of its SKU register, and a read shows the
// clamped value when both are set, so a limit below the floor reads back
// as the floor. The SKU register is not in sysfs.

// intelPL1FieldMax is the largest value of the PL1 field, PKG_PWR_LIM_1 and
// PWR_LIM_VAL, bits 14 to 0.
const intelPL1FieldMax = 1<<15 - 1

// intelMaxWriteUW bounds a written value before it is shifted, far above any
// limit the field holds, so the arithmetic stays in int64.
const intelMaxWriteUW = int64(1) << 40

// intelHwmonNames are the names of the device-level hwmon devices of the
// Intel drivers (i915_hwmon.c:930, xe_hwmon.c:1574).
var intelHwmonNames = map[string]bool{IntelDriverI915: true, IntelDriverXe: true}

// intelGPUHwmonDirs lists the hwmon device of every Intel GPU in DRM card
// order: vendor 0x8086, hwmon named i915 or xe.
func (n *Node) intelGPUHwmonDirs() ([]string, error) {
	return n.gpuHwmonDirs("0x8086", intelHwmonNames)
}

// buildIntelGPU seeds g from its hwmon directory dir: the PL1 limit file of
// its driver's channel, an A file, and the energy counter, an S file. The
// range is the profile's limits, PKG_MIN_PWR to PKG_MAX_PWR, which sysfs
// does not show. Without dir the GPU keeps its profile limit and no file.
func (n *Node) buildIntelGPU(g *nodeGPU, dir string) {
	gs := n.profile.GPUs
	g.intel = IntelDriverI915
	if gs.Intel != nil {
		g.unitShift = gs.Intel.PowerUnitShift.V
		g.intel = gs.Intel.Driver.V
	}
	g.limitRaw = g.intelStoredUW(int64(math.Round(gs.Limits.DefaultW.V * 1e6)))
	if dir != "" {
		file := func(kind, attr string) string { return filepath.Join(dir, intelHwmonFile(kind, g.intel, attr)) }
		// hwmon/hwmon<N> sits in the PCI device's directory.
		dev := filepath.Dir(filepath.Dir(dir))
		g.pciBusID = filepath.Base(dev)
		g.sysfsPath = strings.TrimPrefix(n.relPath(dev), layout.SysDir+"/")
		h, cur := n.intelPL1Handler(g, file("power", "max"))
		n.addAFile(file("power", "max"), g.id(), h, cur)
		if g.energyUJ = n.sfile(file("energy", "input")); g.energyUJ != nil {
			g.seedUJ, _ = nodeParseInt(strings.TrimSpace(g.energyUJ.last))
		}
	}
	g.settledW = g.targetW()
}

// intelStoredUW is the limit the PL1 field holds after a write of uw > 0,
// in uW: uw in units of 1/2^unitShift W, rounded to nearest, kept within
// the field as the GPU's driver keeps it, and read back as the driver
// scales it (units times 1e6, shifted right).
func (g *nodeGPU) intelStoredUW(uw int64) int64 {
	uw = min(max(uw, 0), intelMaxWriteUW)
	units := ((uw << g.unitShift) + 500000) / 1000000
	if g.intel == IntelDriverXe {
		units = min(units, intelPL1FieldMax)
	} else {
		units &= intelPL1FieldMax
	}
	return (units * 1000000) >> g.unitShift
}

// intelShownUW is what power<N>_max reads: 0 while the limit is disabled,
// else the stored limit clamped to [minW, maxW] when both are above 0.
func (g *nodeGPU) intelShownUW() int64 {
	v := g.limitRaw
	if v == 0 {
		return 0
	}
	lo, hi := int64(math.Round(g.minW*1e6)), int64(math.Round(g.maxW*1e6))
	if lo > 0 && hi > 0 {
		v = min(max(v, lo), hi)
	}
	return v
}

// intelEnforcedW is the limit the GPU enforces: the clamped limit, or with
// the limit disabled PKG_MAX_PWR, the highest the SKU register allows
// (assumed: the burst and critical limits are not modelled).
func (g *nodeGPU) intelEnforcedW() float64 {
	if g.limitRaw == 0 {
		return g.maxW
	}
	return float64(g.intelShownUW()) / 1e6
}

// intelPL1Handler is power<N>_max at abs: a decimal number parsed as kstrtol
// parses it, so a value that does not parse is EINVAL (hwmon_attr_store), 0
// disables the limit, and a value above 0 is stored and read back as
// intelStoredUW and intelShownUW give. A read that differs from the write
// is clamped when the SKU range moved it and quantized when the unit did. A
// negative value is refused with EINVAL (assumed: the drivers store an
// undefined limit for one).
func (n *Node) intelPL1Handler(g *nodeGPU, abs string) (func(string) (string, string), func() string) {
	handle := func(v string) (string, string) {
		x, err := strconv.ParseInt(strings.TrimPrefix(v, "+"), 10, 64)
		if err != nil || x < 0 {
			return EventParseError, nodeItoa(g.intelShownUW())
		}
		g.limitRaw = 0
		if x > 0 {
			g.limitRaw = g.intelStoredUW(x)
		}
		shown := g.intelShownUW()
		switch {
		case shown != g.limitRaw:
			return EventClamped, nodeItoa(shown)
		case shown != x:
			return EventQuantized, nodeItoa(shown)
		}
		return EventAccepted, nodeItoa(shown)
	}
	return handle, func() string { return nodeItoa(g.intelShownUW()) + "\n" }
}

// intelHwmonFile names attribute attr (such as "max" or "rated_max") of the
// power channel of driver, or of the energy channel with kind "energy":
// power1_max under i915, power2_max under xe.
func intelHwmonFile(kind, driver, attr string) string {
	name := fmt.Sprintf("%s%d", kind, IntelPowerChannel(driver))
	if attr != "" {
		name += "_" + attr
	}
	return name
}
