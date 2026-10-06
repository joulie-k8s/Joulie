package hwemu

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// PCI buses of rendered devices: listed devices with a DRM card from
// renderDRMBus on, GPUs from renderGPUBus on. The flat PCI path without
// bridges is assumed (layout.PCIDevDir).
const (
	renderDRMBus = 0x02
	renderGPUBus = 0x10
)

// renderAMDGPUHwmonBase is the hwmon number of the first amdgpu device, so
// GPU i is hwmon i+2, after the HSMP sockets when there are more.
const renderAMDGPUHwmonBase = 2

// renderHSMP writes one HSMP hwmon device per socket under the amd_hsmp
// platform device, the layout the kernel registers when firmware exposes no
// ACPI HSMP device (hsmp/plat.c:30, 198, 286, 335-341): name (R),
// power1_input (S), power1_cap (A) and power1_cap_max (R), in microwatts
// (hsmp/hwmon.c:20, 34-38, 74, 79-96), plus its class/hwmon link.
func renderHSMP(b *renderBuilder, p *Profile, o RenderOptions) error {
	h := p.HSMP
	if h == nil {
		return nil
	}
	// power1_input reports milliwatts times 1000 (hsmp/hwmon.c:74).
	idleUW := int64(p.Physics.CPU.IdlePkgW.V*1000) * 1000
	for s := 0; s < p.CPU.Sockets.V; s++ {
		entries := []renderEntry{
			{name: "name", content: "amd_hsmp_hwmon\n", owner: layout.OwnerRender, mode: 0o444},
			{name: "power1_input", content: renderUint(idleUW), owner: layout.OwnerEmulator, mode: 0o444},
			{name: "power1_cap", content: renderUint(h.CapDefaultUW.V), owner: layout.OwnerAgent, mode: 0o644},
			{name: "power1_cap_max", content: renderUint(h.CapMaxUW.V), owner: layout.OwnerRender, mode: 0o444},
		}
		if err := b.writeEntries(renderSys(layout.HSMPHwmonDir(s)), entries); err != nil {
			return err
		}
		link, target := layout.HwmonClassLink(s, path.Dir(path.Dir(layout.HSMPHwmonDir(s))))
		if err := b.link(renderSys(link), target); err != nil {
			return err
		}
	}
	return nil
}

// renderPCIDevices writes the PCI devices that tools find through sysfs:
// each listed device with drm set, then each AMD GPU. Every one gets vendor,
// and device and class when known (R), and a DRM card with its device link
// and class/drm link, cards numbered in that order. An AMD GPU also gets
// product_name, vbios_version, an amdgpu hwmon device (renderAMDGPUHwmon)
// and the state the AMD tools read besides sysfs (renderAMDGPUState). NVIDIA
// GPUs have no sysfs power files; their state is NVML's.
func renderPCIDevices(b *renderBuilder, p *Profile, o RenderOptions) error {
	card := 0
	bus := renderDRMBus
	for _, d := range p.PCI {
		if !d.DRM {
			continue
		}
		if err := renderPCIDevice(b, bus, card, d, nil); err != nil {
			return err
		}
		bus++
		card++
	}
	g := p.GPUs
	if g == nil || g.Vendor != "amd" {
		return nil
	}
	hwmonBase := renderAMDGPUHwmonBase
	if p.HSMP != nil {
		hwmonBase = max(hwmonBase, p.CPU.Sockets.V)
	}
	for i := 0; i < g.Count.V; i++ {
		var extra []renderEntry
		if g.Product.V != "" {
			extra = append(extra, renderEntry{name: "product_name", content: g.Product.V + "\n", owner: layout.OwnerRender, mode: 0o444})
		}
		if g.VBIOSVersion.V != "" {
			extra = append(extra, renderEntry{name: "vbios_version", content: g.VBIOSVersion.V + "\n", owner: layout.OwnerRender, mode: 0o444})
		}
		gbus := renderGPUBus + i
		if err := renderPCIDevice(b, gbus, card+i, g.PCI, extra); err != nil {
			return err
		}
		if err := renderAMDGPUHwmon(b, p, gbus, hwmonBase+i); err != nil {
			return err
		}
		if err := renderAMDGPUState(b, g, card+i, slices.Contains(g.SecondaryDies, i)); err != nil {
			return err
		}
	}
	return nil
}

// renderAMDGPUState writes what rocm-smi and amd-smi learn from KFD or from
// the binary gpu_metrics file, which have no text attribute, under
// state/amdgpu/card<N>/ where package smi reads them (R): gfx_version, the
// KFD target, when the profile names one, and energy_count 0 on a secondary
// die, whose energy accumulator never moves. A primary has no energy_count,
// which the tools read as a primary.
func renderAMDGPUState(b *renderBuilder, g *GPUSpec, card int, secondary bool) error {
	dir := path.Join(layout.StateDir, "amdgpu", fmt.Sprintf("card%d", card))
	var entries []renderEntry
	if g.GFXVersion.V != "" {
		entries = append(entries, renderEntry{name: "gfx_version", content: g.GFXVersion.V + "\n", owner: layout.OwnerRender, mode: 0o444})
	}
	if secondary {
		entries = append(entries, renderEntry{name: "energy_count", content: renderUint(0), owner: layout.OwnerRender, mode: 0o444})
	}
	if len(entries) == 0 {
		return nil
	}
	return b.writeEntries(dir, entries)
}

func renderPCIDevice(b *renderBuilder, bus, card int, d PCIDevice, extra []renderEntry) error {
	dev := renderSys(layout.PCIDevDir(bus))
	entries := []renderEntry{{name: "vendor", content: strings.ToLower(d.Vendor.V) + "\n", owner: layout.OwnerRender, mode: 0o444}}
	if d.Class.V != "" {
		entries = append(entries, renderEntry{name: "class", content: strings.ToLower(d.Class.V) + "\n", owner: layout.OwnerRender, mode: 0o444})
	}
	if d.Device.V != "" {
		entries = append(entries, renderEntry{name: "device", content: strings.ToLower(d.Device.V) + "\n", owner: layout.OwnerRender, mode: 0o444})
	}
	entries = append(entries, extra...)
	if err := b.writeEntries(dev, entries); err != nil {
		return err
	}
	cardDir := renderSys(layout.DRMCardDir(bus, card))
	if err := b.mkdir(cardDir); err != nil {
		return err
	}
	// drm/cardC/device points back at the PCI function, written the way
	// sysfs writes it: up to the bus directory, then down by name.
	if err := b.link(path.Join(cardDir, "device"), "../../../"+path.Base(layout.PCIDevDir(bus))); err != nil {
		return err
	}
	name := fmt.Sprintf("card%d", card)
	return b.link(renderSys(path.Join("class", "drm", name)), path.Join("..", "..", layout.DRMCardDir(bus, card)))
}

// renderAMDGPUHwmon writes the amdgpu hwmon device of the GPU on bus: name
// (R), power1_cap (A) with power1_cap_min, power1_cap_max and
// power1_cap_default (R), and the power sensor the product has, power1_input
// or power1_average or both (S), all in microwatts (amdgpu_pm.c:3591-3600,
// 3664-3669, 3827-3834). A product with ppt1 present also gets the power2_cap
// set, with the same limits (assumed).
func renderAMDGPUHwmon(b *renderBuilder, p *Profile, bus, h int) error {
	g := p.GPUs
	uw := func(w float64) string { return renderUint(int64(w * 1e6)) }
	entries := []renderEntry{
		{name: "name", content: "amdgpu\n", owner: layout.OwnerRender, mode: 0o444},
		{name: "power1_cap", content: uw(g.Limits.DefaultW.V), owner: layout.OwnerAgent, mode: 0o644},
		{name: "power1_cap_min", content: uw(g.Limits.MinW.V), owner: layout.OwnerRender, mode: 0o444},
		{name: "power1_cap_max", content: uw(g.Limits.MaxW.V), owner: layout.OwnerRender, mode: 0o444},
		{name: "power1_cap_default", content: uw(g.Limits.DefaultW.V), owner: layout.OwnerRender, mode: 0o444},
	}
	idle := uw(p.Physics.GPU.IdleW.V)
	switch g.PowerSensor.V {
	case "input":
		entries = append(entries, renderEntry{name: "power1_input", content: idle, owner: layout.OwnerEmulator, mode: 0o444})
	case "both":
		entries = append(entries,
			renderEntry{name: "power1_average", content: idle, owner: layout.OwnerEmulator, mode: 0o444},
			renderEntry{name: "power1_input", content: idle, owner: layout.OwnerEmulator, mode: 0o444})
	default:
		entries = append(entries, renderEntry{name: "power1_average", content: idle, owner: layout.OwnerEmulator, mode: 0o444})
	}
	if g.PPT1.V == "present" {
		entries = append(entries,
			renderEntry{name: "power2_cap", content: uw(g.Limits.DefaultW.V), owner: layout.OwnerAgent, mode: 0o644},
			renderEntry{name: "power2_cap_min", content: uw(g.Limits.MinW.V), owner: layout.OwnerRender, mode: 0o444},
			renderEntry{name: "power2_cap_max", content: uw(g.Limits.MaxW.V), owner: layout.OwnerRender, mode: 0o444},
			renderEntry{name: "power2_cap_default", content: uw(g.Limits.DefaultW.V), owner: layout.OwnerRender, mode: 0o444})
	}
	link, target := layout.HwmonClassLink(h, layout.PCIDevDir(bus))
	if err := b.writeEntries(renderSys(strings.TrimPrefix(target, "../../")), entries); err != nil {
		return err
	}
	return b.link(renderSys(link), target)
}
