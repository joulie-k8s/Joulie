package hwemu

import (
	"math"
	"path"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// H04: energy_uj wraps at max_energy_range_uj, so the agent's correction, new
// - old + range (dvfs.Controller.readPowerWatts), gives emulated power to
// within one or two counts. Package (unit 61035) and DRAM (15300) counters
// both start 1 J below the wrap.
func TestH04EnergyWrapsAtMaxEnergyRange(t *testing.T) {
	t.Parallel()
	p := nodeIntelProfile()
	pkgDir := layout.PowercapZoneDir("intel-rapl", 0)
	dramDir := layout.PowercapZoneDir("intel-rapl", 0, 0)
	start := map[string]int64{
		pkgDir:  MaxEnergyRangeUJ(61035) - 1000000,
		dramDir: MaxEnergyRangeUJ(15300) - 1000000,
	}
	tr := nodeRender(t, p, start)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(nodeFullLoad)
	nodeStep(t, n, time.Second)

	pk := n.State().Packages[0]
	for _, z := range []struct {
		dir    string
		unitNJ int64
		watts  float64
	}{
		{pkgDir, 61035, pk.PowerW},
		{dramDir, 15300, 8},
	} {
		rel := path.Join(layout.SysDir, z.dir)
		rangeUJ := nodeReadInt64(t, tr, path.Join(rel, "max_energy_range_uj"))
		old, cur := start[z.dir], nodeReadInt64(t, tr, path.Join(rel, "energy_uj"))
		if cur >= old || cur >= rangeUJ {
			t.Errorf("%s: energy_uj went from %d to %d, want a wrap below the range %d", z.dir, old, cur, rangeUJ)
			continue
		}
		delta := cur - old + rangeUJ
		want := z.watts * 1e6
		if math.Abs(float64(delta)-want) > 0.001*want {
			t.Errorf("%s: corrected delta %d uJ over 1 s, want %.0f uJ within 0.1%%", z.dir, delta, want)
		}
		if slack := 2*z.unitNJ/1000 + 2; math.Abs(float64(delta)-want) > float64(slack) {
			t.Errorf("%s: corrected delta %d uJ, want %.0f uJ within two counts (%d uJ)", z.dir, delta, want, slack)
		}
	}

	// Every unit the kernel derives (1e9 >> ESU nJ) comes back from its range,
	// which is how a zone without a unit in its profile is counted.
	for esu := 0; esu < 32; esu++ {
		unit := int64(1e9) >> esu
		if unit == 0 {
			break
		}
		if got := nodeUnitFromRange(MaxEnergyRangeUJ(unit)); got != unit {
			t.Errorf("ESU %d: unit from range %d = %d, want %d", esu, MaxEnergyRangeUJ(unit), got, unit)
		}
	}
	for _, unit := range []int64{61035, 15300, 15258} {
		if got := nodeUnitFromRange(MaxEnergyRangeUJ(unit)); got != unit {
			t.Errorf("unit from range of %d = %d", unit, got)
		}
	}
}

// H04: in a per-die layout each package-X-die-Y zone counts an equal share of
// its package (assumed), and its children count shares of that die: a core
// zone never counts more than the die it sits in, and the DRAM zones of a
// socket's dies add up to the socket's DRAM power.
func TestH04PerDieChildZonesCountTheirDieShare(t *testing.T) {
	t.Parallel()
	p := nodeIntelProfile()
	p.CPU.DiesPerPackage = nodeF(2, "assumed:test fixture, a two-die package")
	p.Physics.CPU.CoreFraction = nodeF(0.7, "assumed:test value")
	z := &p.Powercap.ControlTypes[0].Zones[0]
	z.Repeat, z.NameFormat = "per-die", "package-%d-die-%d"
	z.Children = append(z.Children, ZoneSpec{NameFormat: "core", Enabled: nodeF(0, "assumed:no limit MSR"), EnergyUnitNJ: nodeF[int64](61035, "assumed:the package unit")})
	tr := nodeRender(t, p, nil)
	n := nodeNewNode(t, p, tr)
	n.SetLoad(nodeFullLoad)
	nodeStep(t, n, 10*time.Second)

	// Dies are intel-rapl:0 and :1 on socket 0, :2 and :3 on socket 1; each
	// has dram at :0 and core at :1.
	pk := n.State().Packages
	for s := 0; s < 2; s++ {
		pkgUJ := pk[s].PowerW * 10e6
		dramUJ := int64(0)
		for d := 0; d < 2; d++ {
			idx := 2*s + d
			die := nodeReadInt64(t, tr, nodeZoneFile("energy_uj", idx))
			core := nodeReadInt64(t, tr, nodeZoneFile("energy_uj", idx, 1))
			if want := pkgUJ / 2; math.Abs(float64(die)-want) > 61 {
				t.Errorf("socket %d die %d: energy_uj %d, want half the package, %.0f", s, d, die, want)
			}
			if want := 0.7 * pkgUJ / 2; math.Abs(float64(core)-want) > 61 || core > die {
				t.Errorf("socket %d die %d: core energy_uj %d, want 0.7 of its die's %d, %.0f", s, d, core, die, want)
			}
			dramUJ += nodeReadInt64(t, tr, nodeZoneFile("energy_uj", idx, 0))
		}
		if want := int64(8 * 10e6); dramUJ < want-30 || dramUJ > want+30 {
			t.Errorf("socket %d: DRAM energy_uj over its dies = %d, want wattsPerSocket for 10 s, %d", s, dramUJ, want)
		}
	}
}
