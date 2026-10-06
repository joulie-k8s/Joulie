package hwemu

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderPowercap writes each control type as the kernel lays it out
// (powercap.rst:34-100): devices/virtual/powercap/<ct> with its zones nested
// by index, and a flat class/powercap entry per control type and per zone
// linking into it, so the same zone is reachable by two paths (D5).
//
// Top-level zones take indices in listing order: a per-package zone one per
// socket, a per-die zone one per die in the order package X die Y at X*D+Y
// (assumed), a once zone one. Children nest under each instance of their
// parent, indexed in listing order.
func renderPowercap(b *renderBuilder, p *Profile, o RenderOptions) error {
	if p.Powercap == nil {
		return nil
	}
	sockets := p.CPU.Sockets.V
	dies := max(1, p.CPU.DiesPerPackage.V)
	for _, ct := range p.Powercap.ControlTypes {
		if err := b.mkdir(renderSys(layout.PowercapControlDir(ct.Name))); err != nil {
			return err
		}
		link, target := layout.PowercapClassLink(ct.Name)
		if err := b.link(renderSys(link), target); err != nil {
			return err
		}
		// The control type's enabled attribute (powercap_sys.c).
		if err := b.file(renderSys(path.Join(layout.PowercapControlDir(ct.Name), "enabled")), "1\n", layout.OwnerAgent, 0o644); err != nil {
			return err
		}
		idx := 0
		for _, z := range ct.Zones {
			for _, args := range renderZoneInstances(z.Repeat, sockets, dies) {
				if err := renderZone(b, ct.Name, []int{idx}, z, renderZoneName(z.NameFormat, args...), o); err != nil {
					return err
				}
				idx++
			}
		}
	}
	return nil
}

// renderZoneInstances lists the name arguments of each instance of a zone.
func renderZoneInstances(repeat string, sockets, dies int) [][]int {
	var out [][]int
	switch repeat {
	case "per-package":
		for s := 0; s < sockets; s++ {
			out = append(out, []int{s})
		}
	case "per-die":
		for x := 0; x < sockets; x++ {
			for y := 0; y < dies; y++ {
				out = append(out, []int{x, y})
			}
		}
	default:
		out = append(out, nil)
	}
	return out
}

// renderZoneName fills each %d of format with the next argument; %% is a
// literal percent. Validate has checked the verbs.
func renderZoneName(format string, args ...int) string {
	var sb strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] == '%' && i+1 < len(format) {
			switch format[i+1] {
			case 'd':
				if len(args) > 0 {
					sb.WriteString(strconv.Itoa(args[0]))
					args = args[1:]
				}
				i++
				continue
			case '%':
				sb.WriteByte('%')
				i++
				continue
			}
		}
		sb.WriteByte(format[i])
	}
	return sb.String()
}

// renderZone writes one zone and its children. Each zone has name (R),
// enabled (A when a constraint accepts writes, else R; 0644 either way),
// energy_uj (S, 0400) and max_energy_range_uj (R). Each constraint i has
// constraint_i_name (R), constraint_i_power_limit_uw (A, or D when the
// constraint rejects writes), constraint_i_time_window_us (A, or R 0644 when
// the constraint rejects writes) and constraint_i_max_power_uw (R). No
// constraint_i_min_power_uw is written: RAPL has no get_min_power op
// (intel_rapl_common.c:479-486), so the kernel creates none
// (powercap_sys.c:313).
func renderZone(b *renderBuilder, ct string, idx []int, z ZoneSpec, name string, o RenderOptions) error {
	dir := renderSys(layout.PowercapZoneDir(ct, idx...))
	link, target := layout.PowercapClassLink(ct, idx...)
	key := path.Base(link)

	writable := false
	for _, c := range z.Constraints {
		if c.RejectWrites.V == "" {
			writable = true
		}
	}
	rangeUJ := MaxEnergyRangeUJ(z.EnergyUnitNJ.V)
	if z.MaxEnergyRangeUJ != nil {
		rangeUJ = z.MaxEnergyRangeUJ.V
	}

	entries := []renderEntry{{name: "name", content: name + "\n", owner: layout.OwnerRender, mode: 0o444}}
	if z.Enabled.Src != "" {
		owner := layout.OwnerRender
		if writable {
			owner = layout.OwnerAgent
		}
		entries = append(entries, renderEntry{name: "enabled", content: renderUint(z.Enabled.V), owner: owner, mode: 0o644})
	}
	entries = append(entries,
		renderEntry{name: "energy_uj", content: renderUint(renderSeedEnergy(o.Seed, key, z.EnergyUnitNJ.V)), owner: layout.OwnerEmulator, mode: 0o400},
		renderEntry{name: "max_energy_range_uj", content: renderUint(rangeUJ), owner: layout.OwnerRender, mode: 0o444},
	)
	for i, c := range z.Constraints {
		prefix := fmt.Sprintf("constraint_%d_", i)
		rejects := c.RejectWrites.V != ""
		entries = append(entries, renderEntry{name: prefix + "name", content: c.Name + "\n", owner: layout.OwnerRender, mode: 0o444})
		if rejects {
			entries = append(entries, renderEntry{name: prefix + "power_limit_uw", rejecting: true})
		} else if c.PowerLimitUW.Src != "" {
			entries = append(entries, renderEntry{name: prefix + "power_limit_uw", content: renderUint(c.PowerLimitUW.V), owner: layout.OwnerAgent, mode: 0o644})
		}
		if c.TimeWindowUS.Src != "" {
			owner := layout.OwnerAgent
			if rejects {
				owner = layout.OwnerRender
			}
			entries = append(entries, renderEntry{name: prefix + "time_window_us", content: renderUint(c.TimeWindowUS.V), owner: owner, mode: 0o644})
		}
		if c.MaxPowerUW.Src != "" {
			entries = append(entries, renderEntry{name: prefix + "max_power_uw", content: renderUint(c.MaxPowerUW.V), owner: layout.OwnerRender, mode: 0o444})
		}
	}

	// Extra files replace the content of an attribute of the same name,
	// keeping its owner and mode, or add a read-only file. A corpus zone
	// carries every captured file this way, its energy_uj included.
	extra := make([]string, 0, len(z.ExtraFiles))
	for n := range z.ExtraFiles {
		extra = append(extra, n)
	}
	sort.Strings(extra)
	for _, n := range extra {
		found := false
		for i := range entries {
			if entries[i].name != n {
				continue
			}
			if entries[i].rejecting {
				return fmt.Errorf("%s/%s rejects writes and cannot take extraFiles content", key, n)
			}
			entries[i].content = z.ExtraFiles[n]
			found = true
		}
		if !found {
			entries = append(entries, renderEntry{name: n, content: z.ExtraFiles[n], owner: layout.OwnerRender, mode: 0o444})
		}
	}
	if v, ok := o.EnergyStartUJ[key]; ok {
		if v < 0 || v > rangeUJ {
			return fmt.Errorf("EnergyStartUJ[%s] = %d is outside [0, %d]", key, v, rangeUJ)
		}
		for i := range entries {
			if entries[i].name == "energy_uj" {
				entries[i].content = renderUint(v)
			}
		}
	}

	if err := b.writeEntries(dir, entries); err != nil {
		return err
	}
	if err := b.link(renderSys(link), target); err != nil {
		return err
	}
	for j, c := range z.Children {
		if err := renderZone(b, ct, append(append([]int(nil), idx...), j), c, renderZoneName(c.NameFormat), o); err != nil {
			return err
		}
	}
	return nil
}
