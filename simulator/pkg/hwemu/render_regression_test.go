package hwemu_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
)

// RR01: a freshly rendered tree already shows what the emulated kernel and
// devices read back, so the first step of a new node emits no event for any
// builtin profile. A table cpufreq driver with boost (acpi-cpufreq on the
// energy-only EPYC hosts) rendered with scaling_max_freq at
// cpuinfo_max_freq, above the table, would be resolved to the top entry by
// the first readback with one setting.applied event per policy, as if an
// agent had written it: 384 events per node at simulator
// start, and a value no real node shows (cpufreq.c resolves policy->max to
// the table on every policy update, the first one included).
func TestRR01FirstStepOfAFreshTreeEmitsNoEvent(t *testing.T) {
	t.Parallel()
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n, _, _ := hwemutest.NewNode(t, name, nil)
			if err := n.Step(0); err != nil {
				t.Fatalf("first step: %v", err)
			}
			if ev := n.State().Events; len(ev) > 0 {
				t.Errorf("first step of a fresh tree: %d events, want none; first %+v", len(ev), ev[0])
			}
		})
	}
}

// RR02: on the energy-only EPYC host (acpi-cpufreq, table 2.4/1.9/1.5 GHz,
// cpuinfo_max_freq 3.7 GHz with boost), every policy is rendered with
// scaling_max_freq at the top table entry and keeps it through the first
// step, while cpuinfo_max_freq still shows the boost frequency.
func TestRR02ACPIScalingMaxFreqRendersResolved(t *testing.T) {
	t.Parallel()
	n, tree, _ := hwemutest.NewNode(t, "amd-epyc-2s-energy-only", nil)
	files, err := filepath.Glob(filepath.Join(tree.SysRoot, "devices", "system", "cpu", "cpufreq", "policy*", "scaling_max_freq"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no scaling_max_freq under %s: %v", tree.SysRoot, err)
	}
	check := func(when string) {
		t.Helper()
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != "2400000\n" {
				t.Fatalf("%s: %s = %q, want 2400000, the top table entry", when, f, b)
			}
			info, err := os.ReadFile(filepath.Join(filepath.Dir(f), "cpuinfo_max_freq"))
			if err != nil {
				t.Fatal(err)
			}
			if string(info) != "3707812\n" {
				t.Fatalf("%s: cpuinfo_max_freq = %q, want the boost frequency 3707812", when, info)
			}
		}
	}
	check("rendered")
	if err := n.Step(0); err != nil {
		t.Fatal(err)
	}
	check("after the first step")
}
