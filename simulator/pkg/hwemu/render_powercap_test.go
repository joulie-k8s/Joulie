package hwemu

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderAssumed is the provenance of a test fixture value.
const renderAssumed = "assumed:test fixture"

func renderFactI64(v int64) Fact[int64] { return Fact[int64]{V: v, Src: renderAssumed} }

// renderZoneFile reads a file of the zone at idx under ct.
func renderZoneFile(t testing.TB, tree *Tree, ct string, file string, idx ...int) string {
	t.Helper()
	return renderReadFile(t, tree, "sys/"+layout.PowercapZoneDir(ct, idx...)+"/"+file)
}

// The powercap files of each family carry the values of its profile: zone
// names, the max_energy_range_uj of the energy unit (H05), the corpus limits
// and windows, enabled 1 on limited packages and 0 on DRAM and AMD zones, and
// no constraint on an energy-only zone.
func TestH19PowercapFileContents(t *testing.T) {
	t.Parallel()
	intel := renderTestTree(t, renderBuiltin(t, "intel-xeon-2s-rapl"))
	for _, c := range []struct {
		file string
		idx  []int
		want string
	}{
		{"name", []int{1}, "package-1\n"},
		{"enabled", []int{1}, "1\n"},
		{"max_energy_range_uj", []int{1}, "262143328850\n"},
		{"constraint_0_name", []int{1}, "long_term\n"},
		{"constraint_0_power_limit_uw", []int{1}, "165000000\n"},
		{"constraint_0_time_window_us", []int{1}, "999424\n"},
		{"constraint_0_max_power_uw", []int{1}, "165000000\n"},
		{"constraint_1_name", []int{1}, "short_term\n"},
		{"constraint_1_power_limit_uw", []int{1}, "198000000\n"},
		{"constraint_1_max_power_uw", []int{1}, "413000000\n"},
		{"energy_uj", []int{1}, "0\n"},
		{"name", []int{1, 0}, "dram\n"},
		{"enabled", []int{1, 0}, "0\n"},
		{"max_energy_range_uj", []int{1, 0}, "65712999613\n"},
		{"constraint_0_max_power_uw", []int{1, 0}, "47250000\n"},
		{"constraint_0_time_window_us", []int{1, 0}, "976\n"},
	} {
		if got := renderZoneFile(t, intel, "intel-rapl", c.file, c.idx...); got != c.want {
			t.Errorf("intel %v %s = %q, want %q", c.idx, c.file, got, c.want)
		}
	}
	if got := renderReadFile(t, intel, "sys/devices/virtual/powercap/intel-rapl/enabled"); got != "1\n" {
		t.Errorf("control type enabled = %q", got)
	}
	// The DRAM limit rejects writes the way the corpus replays it.
	if !renderIsEISDIR(filepath.Join(intel.SysRoot, layout.PowercapZoneDir("intel-rapl", 0, 0), "constraint_0_power_limit_uw")) {
		t.Errorf("the DRAM limit accepts writes, want EISDIR")
	}

	amd := renderTestTree(t, renderBuiltin(t, "amd-epyc-2s-energy-only"))
	for _, c := range []struct {
		file string
		idx  []int
		want string
	}{
		{"name", []int{0}, "package-0\n"},
		{"enabled", []int{0}, "0\n"},
		{"max_energy_range_uj", []int{0}, "65532610987\n"},
		{"name", []int{0, 0}, "core\n"},
		{"enabled", []int{0, 0}, "0\n"},
	} {
		if got := renderZoneFile(t, amd, "intel-rapl", c.file, c.idx...); got != c.want {
			t.Errorf("amd %v %s = %q, want %q", c.idx, c.file, got, c.want)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(amd.SysRoot, "devices/virtual/powercap/intel-rapl/*/constraint_*")); len(m) != 0 {
		t.Errorf("energy-only zones have constraint files: %v", m)
	}
	// RAPL has no get_min_power op, so no zone anywhere has min_power_uw.
	for _, tree := range []*Tree{intel, amd} {
		if m, _ := filepath.Glob(filepath.Join(tree.SysRoot, "devices/virtual/powercap/*/*/constraint_*_min_power_uw")); len(m) != 0 {
			t.Errorf("min_power_uw rendered: %v", m)
		}
	}
}

// H19: the psys, intel-rapl-mmio, per-die and peak_power fixtures render
// where the kernel puts them: psys as the next intel-rapl index after the
// packages, mmio in its own control type with its own class links, per-die
// zones as package-X-die-Y at index X*D+Y, and a third constraint as
// constraint_2_* named peak_power.
func TestH19ZoneFixturesRenderWhereTheKernelPutsThem(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, "intel-xeon-2s-rapl")
	rapl := &p.Powercap.ControlTypes[0]
	pkg := &rapl.Zones[0]
	pkg.Constraints = append(pkg.Constraints, ConstraintSpec{
		Name: "peak_power", PowerLimitUW: renderFactI64(300000000), MaxPowerUW: renderFactI64(300000000), TimeWindowUS: renderFactI64(976),
	})
	rapl.Zones = append(rapl.Zones, ZoneSpec{
		Repeat: "once", NameFormat: "psys",
		Enabled:      Fact[int]{V: 0, Src: renderAssumed},
		EnergyUnitNJ: renderFactI64(61035),
	})
	p.Powercap.ControlTypes = append(p.Powercap.ControlTypes, ControlTypeSpec{
		Name: "intel-rapl-mmio",
		Zones: []ZoneSpec{{
			Repeat: "once", NameFormat: "package-0",
			Enabled:      Fact[int]{V: 1, Src: renderAssumed},
			EnergyUnitNJ: renderFactI64(61035),
			PowerUnitUW:  renderFactI64(125000),
			Constraints:  []ConstraintSpec{{Name: "long_term", PowerLimitUW: renderFactI64(165000000), MaxPowerUW: renderFactI64(165000000), TimeWindowUS: renderFactI64(999424)}},
		}},
	})
	tree := renderTestTree(t, p)

	if got := renderZoneFile(t, tree, "intel-rapl", "name", 2); got != "psys\n" {
		t.Errorf("intel-rapl:2 name = %q, want psys after the two packages", got)
	}
	if got := renderZoneFile(t, tree, "intel-rapl", "constraint_2_name", 0); got != "peak_power\n" {
		t.Errorf("constraint_2_name = %q", got)
	}
	if got := renderZoneFile(t, tree, "intel-rapl", "constraint_2_power_limit_uw", 1); got != "300000000\n" {
		t.Errorf("constraint_2_power_limit_uw = %q", got)
	}
	if got := renderZoneFile(t, tree, "intel-rapl-mmio", "name", 0); got != "package-0\n" {
		t.Errorf("intel-rapl-mmio:0 name = %q", got)
	}
	for _, c := range []struct {
		ct  string
		idx []int
	}{{"intel-rapl", []int{2}}, {"intel-rapl-mmio", nil}, {"intel-rapl-mmio", []int{0}}} {
		link, target := layout.PowercapClassLink(c.ct, c.idx...)
		got, err := os.Readlink(filepath.Join(tree.SysRoot, link))
		if err != nil || got != target {
			t.Errorf("%s -> %q (%v), want %q", link, got, err, target)
		}
	}
	if got := renderReadFile(t, tree, "sys/"+layout.PowercapControlDir("intel-rapl-mmio")+"/enabled"); got != "1\n" {
		t.Errorf("mmio control type enabled = %q", got)
	}

	die := renderBuiltin(t, "intel-xeon-2s-rapl")
	die.CPU.DiesPerPackage = Fact[int]{V: 2, Src: renderAssumed}
	die.Powercap.ControlTypes[0].Zones[0].Repeat = "per-die"
	die.Powercap.ControlTypes[0].Zones[0].NameFormat = "package-%d-die-%d"
	tree = renderTestTree(t, die)
	for i, want := range []string{"package-0-die-0", "package-0-die-1", "package-1-die-0", "package-1-die-1"} {
		if got := renderZoneFile(t, tree, "intel-rapl", "name", i); got != want+"\n" {
			t.Errorf("intel-rapl:%d name = %q, want %s", i, got, want)
		}
		if got := renderZoneFile(t, tree, "intel-rapl", "name", i, 0); got != "dram\n" {
			t.Errorf("intel-rapl:%d:0 name = %q, want dram", i, got)
		}
	}
	if _, err := os.Stat(filepath.Join(tree.SysRoot, layout.PowercapZoneDir("intel-rapl", 4))); err == nil {
		t.Errorf("a fifth zone exists for 2 packages of 2 dies")
	}

	// A name format the repeat cannot fill is rejected.
	bad := renderBuiltin(t, "intel-xeon-2s-rapl")
	bad.Powercap.ControlTypes[0].Zones[0].NameFormat = "package-%d-die-%d"
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "verbs") {
		t.Errorf("err = %v, want a per-package zone with two verbs rejected", err)
	}
}

// RenderOptions.EnergyStartUJ sets a zone's starting energy_uj by its class
// name, a seed gives every other zone a start below the counter span, and a
// start outside the span is rejected.
func TestH19EnergyStart(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, "intel-xeon-2s-rapl")
	tree, err := Render(p, filepath.Join(t.TempDir(), "n"), RenderOptions{
		NodeName: "n", Seed: 42, EnergyStartUJ: map[string]int64{"intel-rapl:0": 262143328849},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := renderZoneFile(t, tree, "intel-rapl", "energy_uj", 0); got != "262143328849\n" {
		t.Errorf("intel-rapl:0 energy_uj = %q", got)
	}
	seeded := renderZoneFile(t, tree, "intel-rapl", "energy_uj", 1)
	if seeded == "0\n" {
		t.Errorf("seeded intel-rapl:1 energy_uj = %q, want a seeded start", seeded)
	}
	again, err := Render(p, filepath.Join(t.TempDir(), "n"), RenderOptions{NodeName: "n", Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	if got := renderZoneFile(t, again, "intel-rapl", "energy_uj", 1); got != seeded {
		t.Errorf("same seed gave %q then %q", seeded, got)
	}
	if _, err := Render(p, filepath.Join(t.TempDir(), "n"), RenderOptions{NodeName: "n", EnergyStartUJ: map[string]int64{"intel-rapl:0": 262143328851}}); err == nil {
		t.Errorf("a start above max_energy_range_uj was accepted")
	}
}
