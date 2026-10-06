package hwemu

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"path"
	"sort"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderDriverVersion is state/nvml/driver_version. No profile field holds
// it and the agent never reads it: the version a read-only nvidia-smi -q of
// H100 and L40S hosts reported (2026-10).
const renderDriverVersion = "590.48.01"

// renderNVMLFields maps each nvidia-smi query field the emulated tool
// answers to the NVML state file behind it. gpus.fieldSupport names fields
// from this map.
var renderNVMLFields = map[string]string{
	"name":                 "name",
	"uuid":                 "uuid",
	"pci.bus_id":           "pci_bus_id",
	"power.min_limit":      "power_min_limit_mw",
	"power.max_limit":      "power_max_limit_mw",
	"power.default_limit":  "power_default_limit_mw",
	"power.limit":          "power_limit_mw",
	"enforced.power.limit": "enforced_power_limit_mw",
	"power.draw":           "power_draw_mw",
	"utilization.gpu":      "utilization_gpu_pct",
}

// renderNVML writes the NVML state the fake nvidia-smi reads, under
// state/nvml: driver_version (R) and per GPU, in mW and mJ as NVML reports
// them, name, uuid, pci_bus_id and the min, max and default limits (R),
// power_limit_mw (A), and enforced_power_limit_mw, power_draw_mw,
// total_energy_mj and utilization_gpu_pct (S). A GPU starts at its default
// limit, at idle power and with no energy.
//
// A field that gpus.fieldSupport marks unsupported holds the token instead
// of a value, such as [N/A], and the tool prints it as it is (D16).
func renderNVML(b *renderBuilder, p *Profile, o RenderOptions) error {
	g := p.GPUs
	if g == nil || g.Vendor != "nvidia" {
		return nil
	}
	nvml := path.Join(layout.StateDir, layout.NVMLDir)
	if err := b.file(path.Join(nvml, "driver_version"), renderDriverVersion+"\n", layout.OwnerRender, 0o444); err != nil {
		return err
	}
	mw := func(w float64) string { return renderUint(int64(math.Round(w * 1000))) }
	current := g.Limits.DefaultW.V
	if g.Limits.CurrentW.Src != "" {
		current = g.Limits.CurrentW.V
	}
	for i := 0; i < g.Count.V; i++ {
		entries := []renderEntry{
			{name: "name", content: g.Product.V + "\n", owner: layout.OwnerRender, mode: 0o444},
			{name: "uuid", content: renderGPUUUID(o.NodeName, i) + "\n", owner: layout.OwnerRender, mode: 0o444},
			{name: "pci_bus_id", content: fmt.Sprintf("%08X:%02X:00.0\n", 0, renderGPUBus+i), owner: layout.OwnerRender, mode: 0o444},
			{name: "power_min_limit_mw", content: mw(g.Limits.MinW.V), owner: layout.OwnerRender, mode: 0o444},
			{name: "power_max_limit_mw", content: mw(g.Limits.MaxW.V), owner: layout.OwnerRender, mode: 0o444},
			{name: "power_default_limit_mw", content: mw(g.Limits.DefaultW.V), owner: layout.OwnerRender, mode: 0o444},
			{name: "power_limit_mw", content: mw(current), owner: layout.OwnerAgent, mode: 0o644},
			{name: "enforced_power_limit_mw", content: mw(current), owner: layout.OwnerEmulator, mode: 0o444},
			{name: "power_draw_mw", content: mw(p.Physics.GPU.IdleW.V), owner: layout.OwnerEmulator, mode: 0o444},
			{name: "total_energy_mj", content: "0\n", owner: layout.OwnerEmulator, mode: 0o444},
			{name: "utilization_gpu_pct", content: "0\n", owner: layout.OwnerEmulator, mode: 0o444},
		}
		fields := make([]string, 0, len(g.FieldSupport))
		for f := range g.FieldSupport {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		for _, f := range fields {
			for j := range entries {
				if entries[j].name == renderNVMLFields[f] {
					entries[j].content = g.FieldSupport[f] + "\n"
				}
			}
		}
		if err := b.writeEntries(path.Join(layout.StateDir, layout.NVMLGPUDir(i)), entries); err != nil {
			return err
		}
	}
	return nil
}

// renderGPUUUID is GPU-<sha256 of node and index>, in the 8-4-4-4-12 form
// nvidia-smi prints, so every emulated GPU has a stable unique id.
func renderGPUUUID(node string, i int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", node, i)))
	h := hex.EncodeToString(sum[:16])
	return "GPU-" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
