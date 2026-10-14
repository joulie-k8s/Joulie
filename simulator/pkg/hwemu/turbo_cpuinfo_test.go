package hwemu

import (
	"path"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// intel_pstate with HWP shows the guaranteed performance as
// base_frequency, which the agent reads as the nominal frequency, from
// cpufreq.baseKHz; acpi-cpufreq has no such file.
func TestHWPPoliciesShowBaseFrequency(t *testing.T) {
	t.Parallel()
	intel := renderTestTree(t, ensureBuiltin(t, "intel-xeon-2s-rapl"))
	if got := nodeRead(t, intel, nodePolicyFile(3, "base_frequency")); got != "2400000\n" {
		t.Errorf("intel_pstate base_frequency %q, want 2400000", got)
	}
	for _, f := range intel.Files {
		if f.Rel == nodePolicyFile(3, "base_frequency") && (f.Owner != layout.OwnerRender || f.Mode != 0o444) {
			t.Errorf("base_frequency is %+v, want render-owned 0444", f)
		}
	}
	epyc := renderTestTree(t, ensureBuiltin(t, "amd-epyc-2s-energy-only"))
	for _, f := range epyc.Files {
		if f.Rel == nodePolicyFile(3, "base_frequency") {
			t.Errorf("acpi-cpufreq renders %s", f.Rel)
		}
	}
}

// cpuinfo_max_freq follows turbo where the kernel lowers it: to the top
// non-turbo frequency while intel_pstate/no_turbo is set or an amd-pstate
// policy's boost is off, and back to the turbo maximum when turbo comes
// on. The physics keeps the turbo maximum as its scale, also on a Node
// started over a tree with turbo off.
func TestCPUInfoMaxFreqFollowsTurbo(t *testing.T) {
	t.Parallel()
	t.Run("intel_pstate no_turbo", func(t *testing.T) {
		t.Parallel()
		p := ensureBuiltin(t, "intel-xeon-2s-rapl")
		n, tr := cgNode(t, p)
		max0 := nodePolicyFile(0, "cpuinfo_max_freq")
		if got := nodeRead(t, tr, max0); got != "3900000\n" {
			t.Fatalf("cpuinfo_max_freq at boot %q, want 3900000", got)
		}
		cgWriteStep(t, n, tr, cgIntelPstateFile("no_turbo"), "1")
		for _, pol := range []int{0, 5} {
			if got := nodeRead(t, tr, nodePolicyFile(pol, "cpuinfo_max_freq")); got != "2400000\n" {
				t.Errorf("policy%d cpuinfo_max_freq with no_turbo %q, want the base 2400000", pol, got)
			}
		}
		if got := stateComponent(t, n.State(), "cpu0").SpeedMaxHz; got != 3.9e9 {
			t.Errorf("SpeedMaxHz %v with no_turbo, want the hardware maximum 3.9e9", got)
		}
		m := nodeNewNode(t, p, tr)
		if got := m.policies[0].maxKHz; got != 3900000 {
			t.Errorf("a Node over the tree with turbo off scales by %d kHz, want the turbo maximum 3900000", got)
		}
		cgWriteStep(t, m, tr, cgIntelPstateFile("no_turbo"), "0")
		if got := nodeRead(t, tr, max0); got != "3900000\n" {
			t.Errorf("cpuinfo_max_freq with turbo back on %q, want 3900000", got)
		}
	})
	t.Run("amd-pstate policy boost", func(t *testing.T) {
		t.Parallel()
		n, tr := cgNode(t, ensureBuiltin(t, "amd-epyc-2s-hsmp"))
		cgWriteStep(t, n, tr, nodePolicyFile(2, "boost"), "0")
		if got := nodeRead(t, tr, nodePolicyFile(2, "cpuinfo_max_freq")); got != "2600000\n" {
			t.Errorf("policy2 cpuinfo_max_freq with its boost off %q, want the base 2600000", got)
		}
		if got := nodeRead(t, tr, nodePolicyFile(3, "cpuinfo_max_freq")); got != "4500000\n" {
			t.Errorf("policy3 cpuinfo_max_freq %q, want 4500000: its boost is on", got)
		}
		cgWriteStep(t, n, tr, nodePolicyFile(2, "boost"), "1")
		if got := nodeRead(t, tr, nodePolicyFile(2, "cpuinfo_max_freq")); got != "4500000\n" {
			t.Errorf("policy2 cpuinfo_max_freq with its boost back on %q, want 4500000", got)
		}
	})
	t.Run("acpi-cpufreq stays as rendered", func(t *testing.T) {
		t.Parallel()
		n, tr := cgNode(t, ensureBuiltin(t, "amd-epyc-2s-energy-only"))
		cgWriteStep(t, n, tr, path.Join(layout.SysDir, layout.CPUFreqDir(), "boost"), "0")
		if got := nodeRead(t, tr, nodePolicyFile(1, "cpuinfo_max_freq")); got != "3707812\n" {
			t.Errorf("acpi-cpufreq cpuinfo_max_freq with boost off %q, want 3707812 as rendered", got)
		}
	})
}
