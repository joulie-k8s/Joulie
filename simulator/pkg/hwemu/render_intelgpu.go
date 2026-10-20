package hwemu

import (
	"path"
	"strconv"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderIntelGPUHwmonBase is the hwmon number of the first Intel GPU, so
// GPU i is hwmon i+2, as the AMD GPUs are numbered.
const renderIntelGPUHwmonBase = renderAMDGPUHwmonBase

// renderIntelGPUs writes every Intel discrete GPU of p: the PCI function
// with vendor, device and class, its DRM card and bus/pci/devices entry
// (renderPCIDevice), the driver binding the kernel shows (the function's
// driver link to bus/pci/drivers/<driver>, and the driver's link back), and
// the driver's hwmon device (renderIntelGPUHwmon).
func renderIntelGPUs(b *renderBuilder, p *Profile) error {
	g := p.GPUs
	driver := IntelDriverI915
	if g.Intel != nil {
		driver = g.Intel.Driver.V
	}
	drvDir := renderSys(path.Join("bus", "pci", "drivers", driver))
	if err := b.mkdir(drvDir); err != nil {
		return err
	}
	for i := 0; i < g.Count.V; i++ {
		bus, card := renderGPUCard(p, i)
		if err := renderPCIDevice(b, bus, card, g.PCI, nil); err != nil {
			return err
		}
		link, target := layout.PCIBusDeviceLink(bus)
		if err := b.link(renderSys(link), target); err != nil {
			return err
		}
		dev := layout.PCIDevDir(bus)
		// devices/pci0000:BB/0000:BB:00.0/driver points three levels up to
		// sys/, then down to the driver; the driver's entry points back.
		if err := b.link(renderSys(path.Join(dev, "driver")), path.Join("..", "..", "..", "bus", "pci", "drivers", driver)); err != nil {
			return err
		}
		if err := b.link(path.Join(drvDir, path.Base(dev)), path.Join("..", "..", "..", "..", dev)); err != nil {
			return err
		}
		if err := renderIntelGPUHwmon(b, p, driver, bus, renderIntelGPUHwmonBase+i); err != nil {
			return err
		}
	}
	return nil
}

// renderIntelGPUHwmon writes the hwmon device of the Intel GPU on bus, on
// the power channel of driver (IntelPowerChannel): name (R), the
// sustained limit power<N>_max in uW (A, mode 0664 as the drivers register
// it), at limits.currentW when set and else at the TDP, with
// power<N>_rated_max, the TDP in uW, and power<N>_max_interval, the PL1
// window in ms (R), and the energy counter energy<N>_input in uJ (S), from
// 0 (linux v7.3-rc5 i915_hwmon.c:382-398, 581-597, 930; xe_hwmon.c:757,
// 1081-1135, 1574).
// xe also labels the channel pkg (xe_hwmon.c:1445-1453). Neither driver
// has a power sensor (power<N>_input), and no per-tile device is rendered:
// mainline i915 registers one only for a per-tile energy register, which
// DG2 lacks (i915_hwmon.c:852-860).
func renderIntelGPUHwmon(b *renderBuilder, p *Profile, driver string, bus, h int) error {
	g := p.GPUs
	uw := func(w float64) string { return renderUint(int64(w * 1e6)) }
	current := g.Limits.DefaultW.V
	if g.Limits.CurrentW.Src != "" {
		current = g.Limits.CurrentW.V
	}
	interval := int64(0)
	if g.Intel != nil {
		interval = g.Intel.MaxIntervalMS.V
	}
	entries := []renderEntry{
		{name: "name", content: driver + "\n", owner: layout.OwnerRender, mode: 0o444},
		{name: intelHwmonFile("power", driver, "max"), content: uw(current), owner: layout.OwnerAgent, mode: 0o664},
		{name: intelHwmonFile("power", driver, "rated_max"), content: uw(g.Limits.DefaultW.V), owner: layout.OwnerRender, mode: 0o444},
		{name: intelHwmonFile("power", driver, "max_interval"), content: strconv.FormatInt(interval, 10) + "\n", owner: layout.OwnerRender, mode: 0o664},
		{name: intelHwmonFile("energy", driver, "input"), content: "0\n", owner: layout.OwnerEmulator, mode: 0o444},
	}
	if driver == IntelDriverXe {
		entries = append(entries, renderEntry{name: intelHwmonFile("power", driver, "label"), content: "pkg\n", owner: layout.OwnerRender, mode: 0o444})
	}
	link, target := layout.HwmonClassLink(h, layout.PCIDevDir(bus))
	if err := b.writeEntries(renderSys(path.Join(layout.PCIDevDir(bus), "hwmon", "hwmon"+strconv.Itoa(h))), entries); err != nil {
		return err
	}
	return b.link(renderSys(link), target)
}
