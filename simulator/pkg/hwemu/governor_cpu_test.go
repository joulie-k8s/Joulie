package hwemu

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// cgNode renders p and starts a Node over it.
func cgNode(t *testing.T, p *Profile) (*Node, *Tree) {
	t.Helper()
	tr := renderTestTree(t, p)
	return nodeNewNode(t, p, tr), tr
}

// cgPolicies lists the policy numbers of the Node.
func cgPolicies(n *Node) []int {
	var out []int
	for _, p := range n.policies {
		out = append(out, p.index)
	}
	return out
}

// cgWriteStep writes v to rel as the agent does and steps the Node once.
func cgWriteStep(t *testing.T, n *Node, tr *Tree, rel, v string) {
	t.Helper()
	nodeWrite(t, tr, rel, v)
	nodeStep(t, n, time.Second)
}

// cgOneEvent returns the single event of the last step for rel.
func cgOneEvent(t *testing.T, n *Node, rel string) Event {
	t.Helper()
	ev := nodeEvents(n, rel)
	if len(ev) != 1 {
		t.Fatalf("%s: events %+v, want one", rel, ev)
	}
	return ev[0]
}

// cgIntelPstateFile is a global intel_pstate attribute, relative to the root.
func cgIntelPstateFile(name string) string {
	return path.Join(layout.SysDir, layout.IntelPstateDir(), name)
}

// CG01: the EPP pair is rendered for intel_pstate with hwp true, in both
// modes, listing every preference as intel_pstate prints them, and not
// without HWP.
func TestCG01EPPFilesFollowHWP(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		mutate func(*CPUFreqSpec)
		want   bool
	}{
		{"intel-xeon-2s-rapl", nil, true},
		{"intel-xeon-2s-rapl", func(f *CPUFreqSpec) { f.HWP.V, f.EPP = false, nil }, false},
		{"intel-xeon-6530-2s-rapl", nil, false},
		{"intel-xeon-6530-2s-rapl", func(f *CPUFreqSpec) {
			f.HWP = Fact[bool]{V: true, Src: "assumed:test"}
			f.EPP = &EPPSpec{Available: Fact[string]{V: "default performance balance_performance balance_power power", Src: "assumed:test"}, Default: Fact[string]{V: "balance_power", Src: "assumed:test"}}
		}, true},
	} {
		p := ensureBuiltin(t, c.name)
		if c.mutate != nil {
			c.mutate(p.CPUFreq)
		}
		tr := renderTestTree(t, p)
		for _, k := range []int{0, 7} {
			pref := filepath.Join(tr.Root, nodePolicyFile(k, "energy_performance_preference"))
			_, err := os.Stat(pref)
			if got := err == nil; got != c.want {
				t.Errorf("%s hwp %v: policy%d energy_performance_preference exists %v, want %v", c.name, p.CPUFreq.HWP.V, k, got, c.want)
			}
			if !c.want {
				continue
			}
			if got := nodeRead(t, tr, nodePolicyFile(k, "energy_performance_available_preferences")); got != "default performance balance_performance balance_power power \n" {
				t.Errorf("%s: available preferences %q", c.name, got)
			}
			if got, want := nodeRead(t, tr, nodePolicyFile(k, "energy_performance_preference")), p.CPUFreq.EPP.Default.V+"\n"; got != want {
				t.Errorf("%s: policy%d preference %q, want the default %q", c.name, k, got, want)
			}
		}
	}
}

// CG02: under the performance policy of a setpolicy driver, the preference
// reads performance, a non-zero EPP is reverted at the next step with an
// EventRejected carrying EBUSY (joulie.sim.write.rejected, error.type
// ebusy), and writing performance itself is taken. In passive mode the
// generic performance governor refuses nothing (intel_pstate.c:864-886).
func TestCG02EPPUnderPerformanceIsRejectedWithEBUSY(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"intel-xeon-2s-rapl", "amd-epyc-2s-hsmp"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n, tr := cgNode(t, ensureBuiltin(t, name))
			gov, pref := nodePolicyFile(0, "scaling_governor"), nodePolicyFile(0, "energy_performance_preference")
			if nodeRead(t, tr, gov) != "performance\n" {
				cgWriteStep(t, n, tr, gov, "performance")
			}
			if got := nodeRead(t, tr, pref); got != "performance\n" {
				t.Fatalf("preference under performance %q, want performance", got)
			}
			cgWriteStep(t, n, tr, pref, "balance_power")
			e := cgOneEvent(t, n, pref)
			if e.Kind != EventRejected || e.Errno != ErrnoEBUSY || e.Written != "balance_power" || e.Effective != "performance" {
				t.Fatalf("event %+v, want rejected with EBUSY, written balance_power, effective performance", e)
			}
			if got := nodeRead(t, tr, pref); got != "performance\n" {
				t.Fatalf("preference after the refused write %q, want performance", got)
			}
			cgWriteStep(t, n, tr, pref, "performance")
			if e := cgOneEvent(t, n, pref); e.Kind != EventAccepted {
				t.Fatalf("writing performance under performance: %+v, want accepted", e)
			}
		})
	}

	p := ensureBuiltin(t, "intel-xeon-6530-2s-rapl")
	p.CPUFreq.HWP = Fact[bool]{V: true, Src: "assumed:test"}
	n, tr := cgNode(t, p)
	cgWriteStep(t, n, tr, nodePolicyFile(0, "scaling_governor"), "performance")
	pref := nodePolicyFile(0, "energy_performance_preference")
	cgWriteStep(t, n, tr, pref, "balance_power")
	if e := cgOneEvent(t, n, pref); e.Kind != EventAccepted || nodeRead(t, tr, pref) != "balance_power\n" {
		t.Fatalf("intel_cpufreq under performance: %+v, file %q; passive mode refuses no EPP", e, nodeRead(t, tr, pref))
	}
}

// CG03: no_turbo=1 caps every policy's maximum at the top non-turbo
// frequency, the 2.40 GHz base of the 8260; 0 restores it; a
// scaling_max_freq written in between is kept, below the cap as written and
// above it as the request turbo restores.
func TestCG03NoTurboCapsAndRestoresTheMaximum(t *testing.T) {
	t.Parallel()
	n, tr := cgNode(t, ensureBuiltin(t, "intel-xeon-2s-rapl"))
	const turbo, base = "3900000\n", "2400000\n"
	noTurbo := cgIntelPstateFile("no_turbo")
	if got := nodeRead(t, tr, noTurbo); got != "0\n" {
		t.Fatalf("no_turbo at boot %q, want 0", got)
	}
	cgWriteStep(t, n, tr, noTurbo, "1")
	for _, k := range cgPolicies(n) {
		if got := nodeRead(t, tr, nodePolicyFile(k, "scaling_max_freq")); got != base {
			t.Errorf("no_turbo 1: policy%d scaling_max_freq %q, want %q", k, got, base)
		}
	}
	cgWriteStep(t, n, tr, nodePolicyFile(0, "scaling_max_freq"), "2000000")
	cgWriteStep(t, n, tr, nodePolicyFile(1, "scaling_max_freq"), "3500000")
	if e := cgOneEvent(t, n, nodePolicyFile(1, "scaling_max_freq")); e.Kind != EventClamped || e.Effective != "2400000" {
		t.Errorf("3.5 GHz under no_turbo: %+v, want clamped to 2400000", e)
	}
	n.SetLoad(Load{CPUUtil: []float64{1}})
	for i := 0; i < 20; i++ {
		nodeStep(t, n, time.Second)
	}
	for _, c := range n.State().Components {
		if c.Type == "cpu" && c.SpeedLimitHz > 2.4e9+1 {
			t.Errorf("%s: SpeedLimitHz %.0f above the base under no_turbo", c.ID, c.SpeedLimitHz)
		}
	}

	cgWriteStep(t, n, tr, noTurbo, "0")
	for _, k := range cgPolicies(n) {
		want := turbo
		switch k {
		case 0:
			want = "2000000\n"
		case 1:
			want = "3500000\n"
		}
		if got := nodeRead(t, tr, nodePolicyFile(k, "scaling_max_freq")); got != want {
			t.Errorf("no_turbo 0: policy%d scaling_max_freq %q, want %q", k, got, want)
		}
	}
	cgWriteStep(t, n, tr, noTurbo, "7")
	if e := cgOneEvent(t, n, noTurbo); e.Kind != EventClamped || nodeRead(t, tr, noTurbo) != "1\n" {
		t.Errorf("no_turbo 7: %+v, file %q, want clamped to 1", e, nodeRead(t, tr, noTurbo))
	}
}

// CG04: amd-pstate keeps boost in each policy, reachable as
// cpuX/cpufreq/boost, and, having set_boost, also the global cpufreq/boost
// (v6.12 cpufreq.c:1085-1086, 2944-2945; amd-pstate.c:1753); turning a
// policy's boost off caps its maximum at the base frequency, 2.6 GHz on the
// 9655. CK01 covers how the two copies agree.
func TestCG04AMDPstateBoostIsPerPolicy(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "amd-epyc-2s-hsmp")
	n, tr := cgNode(t, p)
	if got := nodeRead(t, tr, path.Join(layout.SysDir, layout.CPUFreqDir(), "boost")); got != "1\n" {
		t.Fatalf("amd-pstate-epp global cpufreq/boost %q, want 1", got)
	}
	cpus := len(renderCPUs(p))
	for c := 0; c < cpus; c++ {
		rel := path.Join(layout.SysDir, "devices/system/cpu", "cpu"+nodeItoa(int64(c)), "cpufreq", "boost")
		if got := nodeRead(t, tr, rel); got != "1\n" {
			t.Errorf("cpu%d/cpufreq/boost %q, want 1", c, got)
		}
	}
	for _, f := range tr.Files {
		if strings.HasSuffix(f.Rel, "/boost") && f.Owner != layout.OwnerAgent {
			t.Errorf("%s is not agent-writable", f.Rel)
		}
	}
	cgWriteStep(t, n, tr, nodePolicyFile(3, "boost"), "off")
	if got := nodeRead(t, tr, nodePolicyFile(3, "boost")); got != "0\n" {
		t.Fatalf("boost after off %q, want 0", got)
	}
	if got := nodeRead(t, tr, nodePolicyFile(3, "scaling_max_freq")); got != "2600000\n" {
		t.Errorf("boost off: policy3 scaling_max_freq %q, want the 2.6 GHz base", got)
	}
	if got := nodeRead(t, tr, nodePolicyFile(2, "scaling_max_freq")); got != "4500000\n" {
		t.Errorf("policy2 scaling_max_freq %q, want 4500000: boost is per policy", got)
	}
	cgWriteStep(t, n, tr, nodePolicyFile(3, "boost"), "1")
	if got := nodeRead(t, tr, nodePolicyFile(3, "scaling_max_freq")); got != "4500000\n" {
		t.Errorf("boost back on: policy3 scaling_max_freq %q, want 4500000", got)
	}
}

// CG05: a governor word not in scaling_available_governors is still
// reverted, as nodeEnumHandler does without the governor facts.
func TestCG05UnavailableGovernorIsReverted(t *testing.T) {
	t.Parallel()
	n, tr := cgNode(t, ensureBuiltin(t, "intel-xeon-2s-rapl"))
	gov := nodePolicyFile(2, "scaling_governor")
	cgWriteStep(t, n, tr, gov, "schedutil")
	if e := cgOneEvent(t, n, gov); e.Kind != EventRevertedOutOfRange || nodeRead(t, tr, gov) != "powersave\n" {
		t.Fatalf("schedutil on intel_pstate: %+v, file %q, want reverted to powersave", e, nodeRead(t, tr, gov))
	}
}

// CG06: with userspace, scaling_setspeed starts at the current frequency,
// takes a frequency and shows it clamped to the policy's limits, follows a
// later limit, and reads <unsupported> again under any other governor,
// where a write is reverted as before.
func TestCG06UserspaceTakesSetspeed(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "amd-epyc-2s-energy-only")
	n, tr := cgNode(t, p)
	set, gov := nodePolicyFile(1, "scaling_setspeed"), nodePolicyFile(1, "scaling_governor")
	cgWriteStep(t, n, tr, set, "1900000")
	if e := cgOneEvent(t, n, set); e.Kind != EventRevertedOutOfRange || nodeRead(t, tr, set) != "<unsupported>\n" {
		t.Fatalf("setspeed under schedutil: %+v, file %q, want reverted to <unsupported>", e, nodeRead(t, tr, set))
	}
	cgWriteStep(t, n, tr, gov, "userspace")
	if got := nodeRead(t, tr, set); got != "1500000\n" {
		t.Fatalf("setspeed on entering userspace %q, want the idle 1500000", got)
	}
	for _, c := range []struct{ write, want, kind string }{
		{"1900000", "1900000", EventAccepted},
		{"9000000", "2400000", EventClamped},
		{"100", "1500000", EventClamped},
		{"2000000", "2400000", EventClamped}, // CPUFREQ_RELATION_L: the entry at or above
		{"fast", "2400000", EventParseError},
	} {
		cgWriteStep(t, n, tr, set, c.write)
		if e := cgOneEvent(t, n, set); e.Kind != c.kind || e.Effective != c.want {
			t.Errorf("setspeed %s: %+v, want %s with %s", c.write, e, c.kind, c.want)
		}
	}
	cgWriteStep(t, n, tr, nodePolicyFile(1, "scaling_max_freq"), "1900000")
	if got := nodeRead(t, tr, set); got != "1900000\n" {
		t.Errorf("setspeed after scaling_max_freq 1900000 %q, want 1900000", got)
	}
	cgWriteStep(t, n, tr, gov, "performance")
	if got := nodeRead(t, tr, set); got != "<unsupported>\n" {
		t.Errorf("setspeed after leaving userspace %q, want <unsupported>", got)
	}

	q := ensureBuiltin(t, "amd-epyc-2s-energy-only")
	q.CPUFreq.BoostScope = Fact[string]{}
	q.CPUFreq.Tunables = nil
	tr = renderTestTree(t, q)
	for _, f := range tr.Files {
		if strings.HasSuffix(f.Rel, "/scaling_setspeed") && f.Owner != layout.OwnerRender {
			t.Fatalf("%s is agent-writable on a profile without governor facts", f.Rel)
		}
	}
}

// CG07: tunables render for the available governors only, take valid
// writes, refuse what the kernel refuses, and keep a written value across
// Ensure.
func TestCG07TunablesFollowTheAvailableGovernors(t *testing.T) {
	t.Parallel()
	tunables := func(tr *Tree) map[string]bool {
		out := map[string]bool{}
		for _, f := range tr.Files {
			for _, g := range []string{"schedutil", "ondemand", "conservative"} {
				if strings.HasPrefix(f.Rel, path.Join(layout.SysDir, layout.GovernorTunablesDir(g))+"/") {
					out[g] = true
					if f.Owner != layout.OwnerAgent {
						t.Errorf("%s is not agent-writable", f.Rel)
					}
				}
			}
		}
		return out
	}
	if got := tunables(renderTestTree(t, ensureBuiltin(t, "amd-epyc-2s-hsmp"))); len(got) != 0 {
		t.Errorf("amd-pstate-epp offers no generic governor, yet renders tunables %v", got)
	}
	p := ensureBuiltin(t, "intel-xeon-6530-2s-rapl")
	p.CPUFreq.AvailableGovernors.V = "userspace powersave performance schedutil"
	if got := tunables(renderTestTree(t, p)); len(got) != 1 || !got["schedutil"] {
		t.Errorf("tunables %v, want schedutil only", got)
	}

	p = ensureBuiltin(t, "intel-xeon-6530-2s-rapl")
	root := filepath.Join(t.TempDir(), "node")
	tr, _, err := Ensure(p, root, RenderOptions{NodeName: "hwemu-test-node"})
	if err != nil {
		t.Fatal(err)
	}
	if got := tunables(tr); len(got) != 3 {
		t.Fatalf("tunables %v, want schedutil, ondemand and conservative", got)
	}
	n := nodeNewNode(t, p, tr)
	dir := path.Join(layout.SysDir, layout.GovernorTunablesDir("conservative"))
	for _, c := range []struct{ file, write, want, kind string }{
		{"up_threshold", "90", "90", EventAccepted},
		{"down_threshold", "95", "20", EventRevertedOutOfRange},
		{"freq_step", "150", "100", EventClamped},
		{"down_threshold", "0", "20", EventRevertedOutOfRange},
	} {
		rel := path.Join(dir, c.file)
		cgWriteStep(t, n, tr, rel, c.write)
		if e := cgOneEvent(t, n, rel); e.Kind != c.kind || e.Effective != c.want {
			t.Errorf("%s %s: %+v, want %s with %s", c.file, c.write, e, c.kind, c.want)
		}
	}
	rate := path.Join(layout.SysDir, layout.GovernorTunablesDir("schedutil"), "rate_limit_us")
	if got := nodeRead(t, tr, rate); got != "500\n" {
		t.Errorf("schedutil rate_limit_us %q, want the 500 us passive-mode delay", got)
	}
	cgWriteStep(t, n, tr, rate, "2500")

	tr, _, err = Ensure(p, root, RenderOptions{NodeName: "hwemu-test-node"})
	if err != nil {
		t.Fatal(err)
	}
	nodeNewNode(t, p, tr)
	for rel, want := range map[string]string{path.Join(dir, "up_threshold"): "90\n", path.Join(dir, "freq_step"): "100\n", rate: "2500\n"} {
		if got := nodeRead(t, tr, rel); got != want {
			t.Errorf("after Ensure %s = %q, want %q", rel, got, want)
		}
	}
}

// CG08: uncore domains have R initial limits, A limits clamped to the
// initial range in whole 100 MHz ratios, and an S current frequency that
// follows the package's load; TPMI domains add their identities.
func TestCG08UncoreLimits(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "intel-xeon-2s-multidie")
	n, tr := cgNode(t, p)
	owners := map[string]layout.Owner{}
	for _, f := range tr.Files {
		owners[f.Rel] = f.Owner
	}
	dir := path.Join(layout.SysDir, layout.IntelUncoreDieDir(1, 1))
	for name, want := range map[string]layout.Owner{
		"initial_min_freq_khz": layout.OwnerRender, "initial_max_freq_khz": layout.OwnerRender,
		"min_freq_khz": layout.OwnerAgent, "max_freq_khz": layout.OwnerAgent, "current_freq_khz": layout.OwnerEmulator,
	} {
		if got, ok := owners[path.Join(dir, name)]; !ok || got != want {
			t.Errorf("%s: owner %v (present %v), want %v", name, got, ok, want)
		}
	}
	maxF, minF, cur := path.Join(dir, "max_freq_khz"), path.Join(dir, "min_freq_khz"), path.Join(dir, "current_freq_khz")
	for _, c := range []struct{ file, write, want, kind string }{
		{maxF, "3000000", "2400000", EventClamped},
		{minF, "1000000", "1200000", EventClamped},
		{maxF, "1850000", "1800000", EventQuantized},
		{minF, "50000", "1200000", EventRevertedOutOfRange},
	} {
		cgWriteStep(t, n, tr, c.file, c.write)
		if e := cgOneEvent(t, n, c.file); e.Kind != c.kind || e.Effective != c.want {
			t.Errorf("%s %s: %+v, want %s with %s", path.Base(c.file), c.write, e, c.kind, c.want)
		}
	}
	if got := nodeRead(t, tr, cur); got != "1200000\n" {
		t.Errorf("idle current_freq_khz %q, want the minimum", got)
	}
	n.SetLoad(Load{CPUUtil: []float64{1}})
	nodeStep(t, n, time.Second)
	if got := nodeRead(t, tr, cur); got != "1800000\n" {
		t.Errorf("loaded current_freq_khz %q, want the written maximum", got)
	}

	q := ensureBuiltin(t, "intel-xeon6-2s-tpmi")
	tr = renderTestTree(t, q)
	d := path.Join(layout.SysDir, layout.IntelUncoreClusterDir(1))
	for name, want := range map[string]string{"package_id": "1\n", "domain_id": "0\n", "fabric_cluster_id": "0\n", "elc_floor_freq_khz": "800000\n", "initial_max_freq_khz": "2200000\n"} {
		if got := nodeRead(t, tr, path.Join(d, name)); got != want {
			t.Errorf("tpmi uncore01/%s %q, want %q", name, got, want)
		}
	}
}

// CG09: intel_pstate/status is R: Joulie never switches the driver mode,
// and a write is put back with a restored event.
func TestCG09IntelPstateStatusIsReverted(t *testing.T) {
	t.Parallel()
	n, tr := cgNode(t, ensureBuiltin(t, "intel-xeon-2s-rapl"))
	status := cgIntelPstateFile("status")
	cgWriteStep(t, n, tr, status, "passive")
	if e := cgOneEvent(t, n, status); e.Kind != EventRestored || nodeRead(t, tr, status) != "active\n" {
		t.Fatalf("status write: %+v, file %q, want restored to active", e, nodeRead(t, tr, status))
	}
	q := ensureBuiltin(t, "amd-epyc-2s-hsmp")
	if got := nodeRead(t, renderTestTree(t, q), path.Join(layout.SysDir, layout.AMDPstateDir(), "status")); got != "active\n" {
		t.Errorf("amd_pstate/status %q, want active", got)
	}
}

// CG10: a package's GovernorNative is its policies' governor, with
// " epp=" and the preference where they have one (pkg/governor's Plan.Native
// spelling; none under the performance policy of a setpolicy driver, which
// holds EPP at 0), follows writes to both, and is empty while the policies
// disagree. Leaving performance, intel_pstate restores the preference it
// saved and amd-pstate-epp keeps EPP 0.
func TestCG10GovernorNativeFollowsGovernorAndEPP(t *testing.T) {
	t.Parallel()
	native := func(n *Node) []string {
		var out []string
		for _, c := range n.State().Components {
			if c.Type == "cpu" {
				out = append(out, c.GovernorNative)
			}
		}
		return out
	}
	all := func(t *testing.T, n *Node, tr *Tree, file, v string) {
		t.Helper()
		for _, k := range cgPolicies(n) {
			nodeWrite(t, tr, nodePolicyFile(k, file), v)
		}
		nodeStep(t, n, time.Second)
	}
	want := func(t *testing.T, n *Node, w ...string) {
		t.Helper()
		if got := native(n); strings.Join(got, ",") != strings.Join(w, ",") {
			t.Fatalf("GovernorNative %q, want %q", got, w)
		}
	}

	n, tr := cgNode(t, ensureBuiltin(t, "intel-xeon-2s-rapl"))
	nodeStep(t, n, time.Second)
	want(t, n, "powersave epp=balance_performance", "powersave epp=balance_performance")
	all(t, n, tr, "energy_performance_preference", "balance_power")
	want(t, n, "powersave epp=balance_power", "powersave epp=balance_power")
	all(t, n, tr, "scaling_governor", "performance")
	want(t, n, "performance", "performance")
	cgWriteStep(t, n, tr, nodePolicyFile(0, "scaling_governor"), "powersave")
	want(t, n, "", "performance")
	if got := nodeRead(t, tr, nodePolicyFile(0, "energy_performance_preference")); got != "balance_power\n" {
		t.Errorf("intel_pstate leaving performance shows %q, want the saved balance_power", got)
	}
	for k, want := range map[int]string{0: "200\n", 1: "performance\n"} {
		cgWriteStep(t, n, tr, nodePolicyFile(k, "energy_performance_preference"), "200")
		if got := nodeRead(t, tr, nodePolicyFile(k, "energy_performance_preference")); got != want {
			t.Errorf("policy%d: raw EPP 200 reads %q, want %q (taken under powersave, refused under performance)", k, got, want)
		}
	}
	cgWriteStep(t, n, tr, nodePolicyFile(0, "energy_performance_preference"), "192")
	if got := nodeRead(t, tr, nodePolicyFile(0, "energy_performance_preference")); got != "balance_power\n" {
		t.Errorf("raw EPP 192 reads %q, want its word balance_power", got)
	}

	n, tr = cgNode(t, ensureBuiltin(t, "amd-epyc-2s-hsmp"))
	nodeStep(t, n, time.Second)
	want(t, n, "performance", "performance")
	all(t, n, tr, "scaling_governor", "powersave")
	want(t, n, "powersave epp=performance", "powersave epp=performance")

	n, _ = cgNode(t, ensureBuiltin(t, "amd-epyc-2s-energy-only"))
	nodeStep(t, n, time.Second)
	want(t, n, "schedutil", "schedutil")

	// intel_cpufreq runs the generic performance governor, which leaves
	// EPP where it was, so the preference stays in the name.
	p := ensureBuiltin(t, "intel-xeon-6530-2s-rapl")
	p.CPUFreq.HWP = Fact[bool]{V: true, Src: "assumed:test"}
	n, tr = cgNode(t, p)
	all(t, n, tr, "scaling_governor", "performance")
	want(t, n, "performance epp=balance_performance", "performance epp=balance_performance")
}

// CG11: validation refuses an EPP default the list does not offer, hwp on a
// driver that is not intel_pstate, and the other governor facts the kernel
// could not have produced.
func TestCG11GovernorFactValidation(t *testing.T) {
	t.Parallel()
	a := func(v string) Fact[string] { return Fact[string]{V: v, Src: "assumed:test"} }
	for _, c := range []struct {
		name, profile string
		mutate        func(*CPUFreqSpec)
		want          string
	}{
		{"epp default outside available", "intel-xeon-2s-rapl", func(f *CPUFreqSpec) { f.EPP.Default = a("turbo") }, "epp.default"},
		{"epp default not offered", "intel-xeon-2s-rapl", func(f *CPUFreqSpec) {
			f.EPP.Available = a("performance power")
			f.EPP.Default = a("balance_performance")
		}, "epp.default"},
		{"hwp on acpi-cpufreq", "amd-epyc-2s-energy-only", func(f *CPUFreqSpec) { f.HWP = Fact[bool]{V: true, Src: "assumed:test"} }, "hwp"},
		{"hwp on amd-pstate-epp", "amd-epyc-2s-hsmp", func(f *CPUFreqSpec) { f.HWP = Fact[bool]{V: true, Src: "assumed:test"} }, "hwp"},
		{"epp without hwp", "intel-xeon-6530-2s-rapl", func(f *CPUFreqSpec) {
			f.EPP = &EPPSpec{Available: a("performance power"), Default: a("power")}
		}, "epp is set"},
		{"driver mode", "intel-xeon-2s-rapl", func(f *CPUFreqSpec) { f.DriverMode = a("passive") }, "driverMode"},
		{"boost scope", "intel-xeon-2s-rapl", func(f *CPUFreqSpec) { f.BoostScope = a(cpuGovBoostGlobal) }, "boostScope"},
		{"tunable", "amd-epyc-2s-energy-only", func(f *CPUFreqSpec) { f.Tunables.Ondemand.UpThreshold.V = 0 }, "tunables.ondemand.up_threshold"},
		{"uncore off Intel", "amd-epyc-2s-energy-only", func(f *CPUFreqSpec) {
			f.Uncore = &UncoreSpec{Layout: a(cpuGovUncorePerDie), InitialMinKHz: Fact[int64]{V: 800000, Src: "assumed:test"}, InitialMaxKHz: Fact[int64]{V: 2000000, Src: "assumed:test"}}
		}, "uncore"},
	} {
		p := renderBuiltin(t, c.profile)
		c.mutate(p.CPUFreq)
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one naming %s", c.name, err, c.want)
		}
	}
}

// CG12: the layout golden of every CPU profile lists the governor files
// with their classes.
func TestCG12LayoutGoldensListTheGovernorFiles(t *testing.T) {
	t.Parallel()
	pol := "sys/devices/system/cpu/cpufreq/policy{N}/"
	intelActive := []string{
		pol + "energy_performance_available_preferences R file 0444",
		pol + "energy_performance_preference A file 0644",
		"sys/devices/system/cpu/intel_pstate/hwp_dynamic_boost A file 0644",
		"sys/devices/system/cpu/intel_pstate/max_perf_pct A file 0644",
		"sys/devices/system/cpu/intel_pstate/min_perf_pct A file 0644",
		"sys/devices/system/cpu/intel_pstate/no_turbo A file 0644",
		"sys/devices/system/cpu/intel_pstate/status R file 0644",
		pol + "scaling_setspeed R file 0644",
		"sys/devices/system/cpu/intel_pstate/num_pstates R file 0444",
		"sys/devices/system/cpu/intel_pstate/turbo_pct R file 0444",
	}
	acpi := []string{
		"sys/devices/system/cpu/cpufreq/boost A file 0644",
		pol + "boost A file 0644",
		"sys/devices/system/cpu/cpufreq/conservative/down_threshold A file 0644",
		"sys/devices/system/cpu/cpufreq/conservative/freq_step A file 0644",
		"sys/devices/system/cpu/cpufreq/conservative/up_threshold A file 0644",
		"sys/devices/system/cpu/cpufreq/ondemand/sampling_rate A file 0644",
		"sys/devices/system/cpu/cpufreq/ondemand/up_threshold A file 0644",
		"sys/devices/system/cpu/cpufreq/schedutil/rate_limit_us A file 0644",
		pol + "scaling_setspeed A file 0644",
	}
	want := map[string][]string{
		"amd-epyc-2s-energy-only":      acpi,
		"amd-epyc-9534-2s-energy-only": acpi,
		"amd-epyc-2s-hsmp": {
			"sys/devices/system/cpu/amd_pstate/status R file 0644",
			"sys/devices/system/cpu/cpufreq/boost A file 0644",
			pol + "boost A file 0644",
			pol + "energy_performance_available_preferences S file 0444",
			pol + "energy_performance_preference A file 0644",
		},
		"intel-xeon-2s-rapl":         intelActive,
		"intel-xeon-2s-psys":         intelActive,
		"intel-xeon6-2s-no-powercap": intelActive,
		"intel-xeon-2s-multidie": append(append([]string(nil), intelActive...),
			"sys/devices/system/cpu/intel_uncore_frequency/package_01_die_01/current_freq_khz S file 0400",
			"sys/devices/system/cpu/intel_uncore_frequency/package_01_die_01/initial_max_freq_khz R file 0444",
			"sys/devices/system/cpu/intel_uncore_frequency/package_01_die_01/max_freq_khz A file 0644",
		),
		"intel-xeon6-2s-tpmi": append(append([]string(nil), intelActive...),
			"sys/devices/system/cpu/intel_uncore_frequency/uncore01/current_freq_khz S file 0400",
			"sys/devices/system/cpu/intel_uncore_frequency/uncore01/elc_floor_freq_khz A file 0644",
			"sys/devices/system/cpu/intel_uncore_frequency/uncore01/package_id R file 0400",
			"sys/devices/system/cpu/intel_uncore_frequency/package_01_die_00/max_freq_khz A file 0644",
			"sys/devices/system/cpu/intel_uncore_frequency/package_01_die_00/initial_min_freq_khz R file 0444",
		),
		"intel-xeon-6530-2s-rapl": {
			"sys/devices/system/cpu/intel_pstate/no_turbo A file 0644",
			"sys/devices/system/cpu/intel_pstate/status R file 0644",
			"sys/devices/system/cpu/intel_pstate/num_pstates R file 0444",
			"sys/devices/system/cpu/intel_pstate/turbo_pct R file 0444",
			"sys/devices/system/cpu/cpufreq/schedutil/rate_limit_us A file 0644",
			pol + "scaling_setspeed A file 0644",
		},
	}
	absent := map[string][]string{
		"intel-xeon-6530-2s-rapl": {"energy_performance_preference", "hwp_dynamic_boost", "/boost "},
		"amd-epyc-2s-hsmp":        {"schedutil"},
		"intel-xeon-2s-rapl":      {"/boost "},
		"intel-xeon6-2s-tpmi":     {"package_00_die_00/current_freq_khz", "package_00_die_00/elc", "package_00_die_00/domain_id"},
	}
	for name, lines := range want {
		b, err := os.ReadFile(filepath.Join("testdata", "layout", name+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		golden := string(b)
		for _, l := range lines {
			if !strings.Contains(golden, l) {
				t.Errorf("%s: golden lacks %q", name, l)
			}
		}
		for _, s := range absent[name] {
			if strings.Contains(golden, s) {
				t.Errorf("%s: golden has %q", name, s)
			}
		}
	}
}

// ckUncoreFile is file name of uncore domain dir, relative to the root.
func ckUncoreFile(dir, name string) string { return path.Join(layout.SysDir, dir, name) }

// CK02: TPMI with one die per package adds a root domain per package,
// package_PP_die_00, after its clusters (v6.12 uncore-frequency-tpmi.c:584-592).
// It has the four limit files only (uncore-frequency-common.c:175-207),
// reads the lowest minimum and the highest maximum of its clusters
// (uncore-frequency-tpmi.c:150-181), sets every cluster of its package on a
// write (300-317), and a cluster can then not go above the root's maximum
// or below its minimum: the write is refused with EINVAL (319-325). With
// more than one die per package there is no root domain.
func TestCK02TPMIRootDomainSpansItsClusters(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "intel-xeon6-2s-tpmi")
	p.CPUFreq.Uncore.ClustersPerDie = Fact[int]{V: 2, Src: "assumed:test"}
	n, tr := cgNode(t, p)
	root, c0, c1 := layout.IntelUncoreDieDir(0, 0), layout.IntelUncoreClusterDir(0), layout.IntelUncoreClusterDir(1)
	other := layout.IntelUncoreClusterDir(2) // package 1
	for _, name := range []string{"current_freq_khz", "domain_id", "package_id", "fabric_cluster_id", "elc_floor_freq_khz"} {
		if _, err := os.Lstat(filepath.Join(tr.Root, ckUncoreFile(root, name))); err == nil {
			t.Errorf("root domain has %s", name)
		}
	}
	for name, want := range map[string]string{"initial_min_freq_khz": "800000\n", "initial_max_freq_khz": "2200000\n", "min_freq_khz": "800000\n", "max_freq_khz": "2200000\n"} {
		if got := nodeRead(t, tr, ckUncoreFile(root, name)); got != want {
			t.Errorf("root %s %q, want %q", name, got, want)
		}
	}
	if got := nodeRead(t, tr, ckUncoreFile(layout.IntelUncoreDieDir(1, 0), "max_freq_khz")); got != "2200000\n" {
		t.Errorf("package 1 root max_freq_khz %q, want 2200000", got)
	}

	rootMax, rootMin := ckUncoreFile(root, "max_freq_khz"), ckUncoreFile(root, "min_freq_khz")
	cgWriteStep(t, n, tr, ckUncoreFile(c0, "max_freq_khz"), "1800000")
	cgWriteStep(t, n, tr, ckUncoreFile(c1, "max_freq_khz"), "1600000")
	cgWriteStep(t, n, tr, ckUncoreFile(c1, "min_freq_khz"), "1200000")
	if got := nodeRead(t, tr, rootMax); got != "1800000\n" {
		t.Errorf("root max_freq_khz %q, want 1800000, the highest of its clusters", got)
	}
	if got := nodeRead(t, tr, rootMin); got != "800000\n" {
		t.Errorf("root min_freq_khz %q, want 800000, the lowest of its clusters", got)
	}

	cgWriteStep(t, n, tr, rootMax, "2050000")
	if e := cgOneEvent(t, n, rootMax); e.Kind != EventQuantized || e.Effective != "2000000" {
		t.Errorf("root max 2050000: %+v, want quantized to 2000000", e)
	}
	for _, d := range []string{c0, c1} {
		if got := nodeRead(t, tr, ckUncoreFile(d, "max_freq_khz")); got != "2000000\n" {
			t.Errorf("after the root write %s max_freq_khz %q, want 2000000", path.Base(d), got)
		}
	}
	if got := nodeRead(t, tr, ckUncoreFile(other, "max_freq_khz")); got != "2200000\n" {
		t.Errorf("package 1 cluster max_freq_khz %q, want 2200000: the root sets its own package only", got)
	}
	cgWriteStep(t, n, tr, ckUncoreFile(c0, "max_freq_khz"), "2100000")
	if e := cgOneEvent(t, n, ckUncoreFile(c0, "max_freq_khz")); e.Kind != EventRejected || e.Errno != ErrnoEINVAL || e.Effective != "2000000" {
		t.Errorf("cluster max above the root's: %+v, want rejected with EINVAL at 2000000", e)
	}
	cgWriteStep(t, n, tr, ckUncoreFile(c0, "max_freq_khz"), "1500000")
	if e := cgOneEvent(t, n, ckUncoreFile(c0, "max_freq_khz")); e.Kind != EventAccepted {
		t.Errorf("cluster max below the root's: %+v, want accepted", e)
	}
	cgWriteStep(t, n, tr, rootMin, "1000000")
	cgWriteStep(t, n, tr, ckUncoreFile(c1, "min_freq_khz"), "900000")
	if e := cgOneEvent(t, n, ckUncoreFile(c1, "min_freq_khz")); e.Kind != EventRejected || e.Errno != ErrnoEINVAL || e.Effective != "1000000" {
		t.Errorf("cluster min below the root's: %+v, want rejected with EINVAL at 1000000", e)
	}
	cgWriteStep(t, n, tr, rootMax, "0")
	if e := cgOneEvent(t, n, rootMax); e.Kind != EventRevertedOutOfRange {
		t.Errorf("root max 0: %+v, want reverted (ratio 0 is EINVAL)", e)
	}

	q := ensureBuiltin(t, "intel-xeon6-2s-tpmi")
	q.CPU.DiesPerPackage = Fact[int]{V: 2, Src: "assumed:test"}
	tr = renderTestTree(t, q)
	if _, err := os.Lstat(filepath.Join(tr.Root, ckUncoreFile(root, "max_freq_khz"))); err == nil {
		t.Error("TPMI with two dies per package renders a root domain")
	}
}

// CK04: intel_pstate/turbo_pct and intel_pstate/num_pstates are R files of
// every CPU that is not hybrid, in both modes (v6.12
// intel_pstate.c:1729-1730, 1764-1770), with the values show_turbo_pct and
// show_num_pstates compute from the lowest, base and turbo P-states in
// 8-bit fixed point (1445-1490): on the 8260, P-states 10, 24 and 39 give
// 30 P-states, 50 % of them turbo.
func TestCK04IntelPstateTurboPctAndNumPstates(t *testing.T) {
	t.Parallel()
	for name, want := range map[string][2]string{
		"intel-xeon-2s-rapl":         {"50\n", "30\n"},
		"intel-xeon-2s-multidie":     {"42\n", "29\n"},
		"intel-xeon-2s-psys":         {"58\n", "33\n"},
		"intel-xeon6-2s-no-powercap": {"59\n", "31\n"},
		"intel-xeon-6530-2s-rapl":    {"58\n", "33\n"},
	} {
		tr := renderTestTree(t, ensureBuiltin(t, name))
		for i, f := range []string{"turbo_pct", "num_pstates"} {
			if got := nodeRead(t, tr, cgIntelPstateFile(f)); got != want[i] {
				t.Errorf("%s: %s %q, want %q", name, f, got, want[i])
			}
		}
		for _, f := range tr.Files {
			if strings.HasPrefix(f.Rel, cgIntelPstateFile("turbo_pct")) || strings.HasPrefix(f.Rel, cgIntelPstateFile("num_pstates")) {
				if f.Owner != layout.OwnerRender || f.Mode != 0o444 {
					t.Errorf("%s: %s is %v %o, want R 0444", name, f.Rel, f.Owner, f.Mode)
				}
			}
		}
	}
	if _, err := os.Lstat(filepath.Join(renderTestTree(t, ensureBuiltin(t, "amd-epyc-2s-hsmp")).SysRoot, layout.IntelPstateDir())); err == nil {
		t.Error("amd-pstate renders intel_pstate/")
	}
}

// CK05: the base frequency is the cpufreq.baseKHz fact, with its source,
// on every builtin profile with policies, and it wins over the frequency
// table and the brand string. Turbo off caps the 6530, whose model name
// carries no frequency and whose driver has no table, at its 2.1 GHz base;
// without the fact its base is unknown and turbo off lowers nothing.
// Validation refuses a base outside the cpuinfo range.
func TestCK05BaseFrequencyIsAProfileFact(t *testing.T) {
	t.Parallel()
	profiles, err := BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range profiles {
		f := p.CPUFreq
		if f == nil || f.Minimal {
			continue
		}
		if b := f.BaseKHz; b.Src == "" || b.V < f.CPUInfoMinKHz.V || b.V > f.CPUInfoMaxKHz.V {
			t.Errorf("%s: cpufreq.baseKHz %+v, want a sourced value within [%d, %d]", name, b, f.CPUInfoMinKHz.V, f.CPUInfoMaxKHz.V)
		}
	}

	p := ensureBuiltin(t, "intel-xeon-2s-rapl")
	p.CPUFreq.BaseKHz = Fact[int64]{V: 2000000, Src: "assumed:test"}
	if got := cpuGovBaseKHz(p); got != 2000000 {
		t.Errorf("base %d with the fact at 2000000 and the brand string at 2.40 GHz, want the fact", got)
	}
	p.CPUFreq.BaseKHz = Fact[int64]{}
	if got := cpuGovBaseKHz(p); got != 2400000 {
		t.Errorf("base %d without the fact, want the brand string's 2400000", got)
	}
	q := ensureBuiltin(t, "amd-epyc-2s-energy-only")
	q.CPUFreq.BaseKHz = Fact[int64]{V: 2300000, Src: "assumed:test"}
	if got := cpuGovBaseKHz(q); got != 2300000 {
		t.Errorf("base %d with the fact at 2300000 and the table topping at 2400000, want the fact", got)
	}

	r := ensureBuiltin(t, "intel-xeon-6530-2s-rapl")
	n, tr := cgNode(t, r)
	cgWriteStep(t, n, tr, cgIntelPstateFile("no_turbo"), "1")
	if got := nodeRead(t, tr, nodePolicyFile(0, "scaling_max_freq")); got != "2100000\n" {
		t.Errorf("6530 no_turbo 1: scaling_max_freq %q, want the 2.1 GHz base", got)
	}
	r = ensureBuiltin(t, "intel-xeon-6530-2s-rapl")
	r.CPUFreq.BaseKHz = Fact[int64]{}
	if got := cpuGovBaseKHz(r); got != 0 {
		t.Errorf("6530 base without the fact %d, want 0 (unknown)", got)
	}

	v := renderBuiltin(t, "intel-xeon-2s-rapl")
	v.CPUFreq.BaseKHz.V = 4000000
	if err := v.Validate(); err == nil || !strings.Contains(err.Error(), "baseKHz") {
		t.Errorf("baseKHz above cpuinfoMaxKHz: err = %v, want one naming baseKHz", err)
	}
}
