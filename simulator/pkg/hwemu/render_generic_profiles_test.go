package hwemu

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// The generic Xeon profiles render the zone names, energy ranges and
// constraints of their kernel lines. The H02 goldens pin paths and modes but
// no file content, so a wrong value in one of these profiles fails here: psys
// with its 1 J unit at intel-rapl:1 between the packages, one zone per die
// named package-X-die-Y with the 15300 nJ DRAM unit, and TPMI's peak_power
// with energy-only DRAM children.
func TestGenericXeonProfilesPowercapContents(t *testing.T) {
	t.Parallel()
	type want struct {
		idx  []int
		file string
		want string
	}
	for _, c := range []struct {
		profile string
		files   []want
	}{
		{"intel-xeon-2s-psys", []want{
			{[]int{0}, "name", "package-0\n"},
			{[]int{0}, "max_energy_range_uj", "262143328850\n"},
			{[]int{0}, "constraint_0_power_limit_uw", "270000000\n"},
			{[]int{0, 0}, "name", "dram\n"},
			{[]int{1}, "name", "psys\n"},
			{[]int{1}, "enabled", "0\n"},
			{[]int{1}, "max_energy_range_uj", "4294967295000000\n"},
			{[]int{1}, "constraint_1_name", "short_term\n"},
			{[]int{2}, "name", "package-1\n"},
			{[]int{2, 0}, "name", "dram\n"},
		}},
		{"intel-xeon-2s-multidie", []want{
			{[]int{0}, "name", "package-0-die-0\n"},
			{[]int{1}, "name", "package-0-die-1\n"},
			{[]int{2}, "name", "package-1-die-0\n"},
			{[]int{3}, "name", "package-1-die-1\n"},
			{[]int{3}, "constraint_0_max_power_uw", "200000000\n"},
			{[]int{0, 0}, "name", "dram\n"},
			{[]int{3, 0}, "max_energy_range_uj", "65712999613\n"},
		}},
		{"intel-xeon6-2s-tpmi", []want{
			{[]int{0}, "name", "package-0\n"},
			{[]int{0}, "constraint_2_name", "peak_power\n"},
			{[]int{0}, "constraint_2_max_power_uw", "1750000000\n"},
			{[]int{0, 0}, "name", "dram\n"},
			{[]int{1}, "name", "psys\n"},
			{[]int{1}, "max_energy_range_uj", "262143328850\n"},
			{[]int{2}, "name", "package-1\n"},
			{[]int{2}, "constraint_2_name", "peak_power\n"},
		}},
	} {
		t.Run(c.profile, func(t *testing.T) {
			_, tree := renderSharedBuiltin(t, c.profile)
			for _, f := range c.files {
				if got := renderZoneFile(t, tree, "intel-rapl", f.file, f.idx...); got != f.want {
					t.Errorf("%s/%s = %q, want %q", layout.PowercapZoneDir("intel-rapl", f.idx...), f.file, got, f.want)
				}
			}
		})
	}

	// PL4 has no time window, and a TPMI memory domain flags no limit
	// register, so its DRAM zone has no constraint files at all.
	_, tree := renderSharedBuiltin(t, "intel-xeon6-2s-tpmi")
	for _, rel := range []string{
		layout.PowercapZoneDir("intel-rapl", 0) + "/constraint_2_time_window_us",
		layout.PowercapZoneDir("intel-rapl", 0, 0) + "/constraint_0_name",
		layout.PowercapZoneDir("intel-rapl", 2, 0) + "/constraint_0_power_limit_uw",
	} {
		if _, err := os.Lstat(filepath.Join(tree.SysRoot, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s exists (%v), want it absent", rel, err)
		}
	}

	// Without an MSR match or the TPMI driver there is no powercap at all.
	_, tree = renderSharedBuiltin(t, "intel-xeon6-2s-no-powercap")
	if _, err := os.Lstat(filepath.Join(tree.SysRoot, "class", "powercap")); !os.IsNotExist(err) {
		t.Errorf("class/powercap exists (%v), want none", err)
	}
}

// TPMI registers psys only from the domain root package since commit
// 903eb9fb85e3 (v6.9); on v6.8 every package with a system domain registers
// its own psys. The single psys zone of the TPMI profile therefore needs a
// node that says it runs 6.9 or later.
func TestTPMIProfileKernelHasRootOnlyPsys(t *testing.T) {
	t.Parallel()
	vals := renderBuiltin(t, "intel-xeon6-2s-tpmi").Node.Labels["kernel"].Values
	major, err1 := strconv.Atoi(vals["feature.node.kubernetes.io/kernel-version.major"])
	minor, err2 := strconv.Atoi(vals["feature.node.kubernetes.io/kernel-version.minor"])
	if err1 != nil || err2 != nil {
		t.Fatalf("kernel labels %v: %v %v", vals, err1, err2)
	}
	if major < 6 || major == 6 && minor < 9 {
		t.Errorf("kernel %d.%d renders a single TPMI psys, which needs 6.9 or later", major, minor)
	}
}
