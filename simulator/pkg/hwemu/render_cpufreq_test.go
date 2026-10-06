package hwemu

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderPolicyDirs lists the policy numbers rendered under cpufreq/.
func renderPolicyDirs(t testing.TB, tree *Tree) map[int]bool {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(tree.SysRoot, "devices/system/cpu/cpufreq/policy*"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]bool{}
	for _, d := range m {
		n, err := strconv.Atoi(filepath.Base(d)[len("policy"):])
		if err != nil {
			t.Fatal(err)
		}
		out[n] = true
	}
	return out
}

// H18: per-cpu renders one policy per CPU. per-core renders one policy per
// core, named after the core's lowest CPU, with related_cpus and
// affected_cpus from the numbering and both siblings' cpuN/cpufreq links on
// it.
func TestH18PolicyGrouping(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, "intel-xeon-2s-rapl")
	tree := renderTestTree(t, p)
	policies := renderPolicyDirs(t, tree)
	if len(policies) != 96 {
		t.Fatalf("per-cpu: %d policies, want 96", len(policies))
	}
	for _, k := range []int{0, 47, 48, 95} {
		dir := "sys/" + layout.PolicyDir(k)
		if got := renderReadFile(t, tree, dir+"/related_cpus"); got != strconv.Itoa(k)+"\n" {
			t.Errorf("per-cpu policy%d related_cpus = %q", k, got)
		}
		link, _ := layout.CPUFreqLink(k, k)
		if got, _ := filepath.EvalSymlinks(filepath.Join(tree.SysRoot, link)); filepath.Base(got) != "policy"+strconv.Itoa(k) {
			t.Errorf("per-cpu cpu%d/cpufreq resolves to %s", k, got)
		}
	}

	p.CPUFreq.PolicyGrouping.V = "per-core"
	tree = renderTestTree(t, p)
	policies = renderPolicyDirs(t, tree)
	if len(policies) != 48 {
		t.Fatalf("per-core: %d policies, want 48 (2 sockets x 24 cores)", len(policies))
	}
	// socket-major-smt-last with S*C = 48: CPU k and k+48 are siblings.
	for _, c := range []struct{ policy, sibling int }{{0, 48}, {23, 71}, {24, 72}, {47, 95}} {
		if !policies[c.policy] || policies[c.sibling] {
			t.Errorf("per-core: want policy%d and no policy%d", c.policy, c.sibling)
		}
		dir := "sys/" + layout.PolicyDir(c.policy)
		want := strconv.Itoa(c.policy) + " " + strconv.Itoa(c.sibling) + "\n"
		for _, f := range []string{"related_cpus", "affected_cpus"} {
			if got := renderReadFile(t, tree, dir+"/"+f); got != want {
				t.Errorf("per-core policy%d %s = %q, want %q", c.policy, f, got, want)
			}
		}
		for _, cpu := range []int{c.policy, c.sibling} {
			link, target := layout.CPUFreqLink(cpu, c.policy)
			got, err := os.Readlink(filepath.Join(tree.SysRoot, link))
			if err != nil || got != target {
				t.Errorf("per-core cpu%d/cpufreq -> %q (%v), want %q", cpu, got, err, target)
			}
		}
	}
}

// The attribute set and formats of each driver: a table
// driver lists its frequencies each followed by a space (freq_table.c:253-255),
// a target driver lists governors the same way while a setpolicy driver has
// no trailing space (cpufreq.c:847-860), scaling_setspeed reads
// <unsupported> (cpufreq.c:919-925), and amd-pstate-epp has no frequency
// table and adds its EPP pair. cpuinfo_transition_latency is 0 for
// amd-pstate-epp, whose init never sets it (amd-pstate.c:1875-1972), and the
// 20000 ns fallback only for the passive amd-pstate (amd-pstate.c:968-976,
// 1086). The EPYC 9655 server boots amd-pstate-epp under performance
// (amd-pstate.c:1941-1944), where EPP reads and offers performance only.
func TestH18CPUFreqAttributesPerDriver(t *testing.T) {
	t.Parallel()
	type file struct{ rel, want string }
	cases := []struct {
		profile string
		want    []file
		absent  []string
	}{
		{"intel-xeon-2s-rapl", []file{
			{"policy7/scaling_driver", "intel_pstate\n"},
			{"policy7/scaling_available_governors", "performance powersave\n"},
			{"policy7/scaling_governor", "powersave\n"},
			{"policy7/cpuinfo_min_freq", "1000000\n"},
			{"policy7/cpuinfo_max_freq", "3900000\n"},
			{"policy7/scaling_max_freq", "3900000\n"},
			{"policy7/scaling_min_freq", "1000000\n"},
			{"policy7/scaling_cur_freq", "1000000\n"},
			{"policy7/scaling_setspeed", "<unsupported>\n"},
			{"policy7/cpuinfo_transition_latency", "0\n"},
		}, []string{"policy7/scaling_available_frequencies", "policy7/energy_performance_preference", "boost"}},
		{"amd-epyc-2s-energy-only", []file{
			{"policy7/scaling_driver", "acpi-cpufreq\n"},
			{"policy7/scaling_available_governors", "performance powersave ondemand schedutil \n"},
			{"policy7/scaling_available_frequencies", "2400000 1900000 1500000 \n"},
			{"policy7/scaling_cur_freq", "2400000\n"},
			{"policy7/scaling_setspeed", "<unsupported>\n"},
			{"policy7/cpuinfo_transition_latency", "10000\n"},
			{"boost", "1\n"},
		}, []string{"policy7/energy_performance_preference"}},
		{"amd-epyc-2s-hsmp", []file{
			{"policy7/scaling_driver", "amd-pstate-epp\n"},
			{"policy7/scaling_available_governors", "performance powersave\n"},
			{"policy7/scaling_setspeed", "<unsupported>\n"},
			{"policy7/cpuinfo_max_freq", "4500000\n"},
			{"policy7/cpuinfo_transition_latency", "0\n"},
			{"policy7/scaling_governor", "performance\n"},
			{"policy7/scaling_cur_freq", "4500000\n"},
			{"policy7/energy_performance_preference", "performance\n"},
			{"policy7/energy_performance_available_preferences", "performance\n"},
		}, []string{"policy7/scaling_available_frequencies"}},
	}
	for _, c := range cases {
		t.Run(c.profile, func(t *testing.T) {
			tree := renderTestTree(t, renderBuiltin(t, c.profile))
			base := "sys/devices/system/cpu/cpufreq/"
			for _, f := range c.want {
				if got := renderReadFile(t, tree, base+f.rel); got != f.want {
					t.Errorf("%s = %q, want %q", f.rel, got, f.want)
				}
			}
			for _, rel := range c.absent {
				if _, err := os.Stat(filepath.Join(tree.Root, base+rel)); err == nil {
					t.Errorf("%s exists, want it absent", rel)
				}
			}
		})
	}

	// Under powersave amd-pstate-epp offers every preference, each followed
	// by a space (amd-pstate.c:1402-1405), and starts at balance_performance.
	p := renderBuiltin(t, "amd-epyc-2s-hsmp")
	p.CPUFreq.Governor.V = "powersave"
	tree := renderTestTree(t, p)
	for rel, want := range map[string]string{
		"energy_performance_preference":            "balance_performance\n",
		"energy_performance_available_preferences": "default performance balance_performance balance_power power \n",
		"scaling_cur_freq":                         "1500000\n",
	} {
		if got := renderReadFile(t, tree, "sys/"+layout.PolicyDir(7)+"/"+rel); got != want {
			t.Errorf("amd-pstate-epp powersave %s = %q, want %q", rel, got, want)
		}
	}

	// The passive amd-pstate is a target driver: governors carry a trailing
	// space, there is no EPP pair, and the latency is the CPPC fallback.
	p = renderBuiltin(t, "amd-epyc-2s-hsmp")
	p.CPUFreq.Driver.V = "amd-pstate"
	tree = renderTestTree(t, p)
	dir := "sys/" + layout.PolicyDir(7) + "/"
	if got := renderReadFile(t, tree, dir+"cpuinfo_transition_latency"); got != "20000\n" {
		t.Errorf("amd-pstate cpuinfo_transition_latency = %q, want 20000", got)
	}
	if got := renderReadFile(t, tree, dir+"scaling_available_governors"); got != "performance powersave \n" {
		t.Errorf("amd-pstate scaling_available_governors = %q", got)
	}
	if _, err := os.Stat(filepath.Join(tree.Root, dir+"energy_performance_preference")); err == nil {
		t.Error("amd-pstate has energy_performance_preference, want it absent")
	}

	// A minimal cpufreq, from a capture of the driver alone, has
	// scaling_driver and nothing the agent could try to write.
	p = renderBuiltin(t, "intel-xeon-2s-rapl")
	p.CPUFreq.Minimal = true
	tree = renderTestTree(t, p)
	entries, err := os.ReadDir(filepath.Join(tree.SysRoot, layout.PolicyDir(0)))
	if err != nil || len(entries) != 1 || entries[0].Name() != "scaling_driver" {
		t.Errorf("minimal policy0 = %v (%v), want scaling_driver only", entries, err)
	}
}
