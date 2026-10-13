package hwemu

import (
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// ckGlobalBoost is the global cpufreq/boost, relative to the node root.
func ckGlobalBoost() string { return path.Join(layout.SysDir, layout.CPUFreqDir(), "boost") }

// ckPolicy returns the policy of the Node with number k.
func ckPolicy(t *testing.T, n *Node, k int) *nodePolicy {
	t.Helper()
	for _, p := range n.policies {
		if p.index == k {
			return p
		}
	}
	t.Fatalf("no policy%d", k)
	return nil
}

// ckAgentFile fails unless rel is an agent-writable file of tr.
func ckAgentFile(t *testing.T, tr *Tree, rel string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(tr.Root, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("%s is not rendered: %v", rel, err)
	}
	for _, f := range tr.Files {
		if f.Rel == rel {
			if f.Owner != layout.OwnerAgent {
				t.Fatalf("%s is %v, want agent-writable", rel, f.Owner)
			}
			return
		}
	}
	t.Fatalf("%s is missing from the tree's file list", rel)
}

// CK01: every driver with set_boost has both the global cpufreq/boost and
// each policy's boost (v6.12 cpufreq.c:1085-1086, 2944-2945), and they
// agree as the kernel keeps them: a global write of a new state sets every
// policy, the state it already has changes none (cpufreq.c:2781-2800), and
// a policy's boost refuses any write with EINVAL while the global one is
// off (cpufreq.c:621-648). amd-pstate-epp (amd-pstate.c:1753) caps a policy
// with boost off at the 2.6 GHz base; acpi-cpufreq (acpi-cpufreq.c:963-969)
// takes away the boost above its 2.4 GHz top entry. The W1 tree rendered
// no global boost for amd-pstate and no policy boost for acpi-cpufreq.
func TestCK01SetBoostDriversHaveGlobalAndPolicyBoost(t *testing.T) {
	t.Parallel()
	t.Run("amd-pstate-epp", func(t *testing.T) {
		t.Parallel()
		p := ensureBuiltin(t, "amd-epyc-2s-hsmp")
		n, tr := cgNode(t, p)
		global := ckGlobalBoost()
		ckAgentFile(t, tr, global)
		ckAgentFile(t, tr, nodePolicyFile(3, "boost"))
		if got := nodeRead(t, tr, global); got != "1\n" {
			t.Fatalf("global boost at boot %q, want 1", got)
		}

		cgWriteStep(t, n, tr, global, "0")
		for _, k := range cgPolicies(n) {
			if got := nodeRead(t, tr, nodePolicyFile(k, "boost")); got != "0\n" {
				t.Fatalf("global boost 0: policy%d boost %q, want 0", k, got)
			}
			if got := nodeRead(t, tr, nodePolicyFile(k, "scaling_max_freq")); got != "2600000\n" {
				t.Fatalf("global boost 0: policy%d scaling_max_freq %q, want the 2.6 GHz base", k, got)
			}
		}
		pol := nodePolicyFile(3, "boost")
		cgWriteStep(t, n, tr, pol, "1")
		if e := cgOneEvent(t, n, pol); e.Kind != EventRejected || e.Errno != ErrnoEINVAL || nodeRead(t, tr, pol) != "0\n" {
			t.Fatalf("policy boost 1 under global 0: %+v, file %q, want rejected with EINVAL and 0", e, nodeRead(t, tr, pol))
		}

		cgWriteStep(t, n, tr, global, "on")
		for _, k := range cgPolicies(n) {
			if got := nodeRead(t, tr, nodePolicyFile(k, "scaling_max_freq")); got != "4500000\n" {
				t.Fatalf("global boost on: policy%d scaling_max_freq %q, want 4500000", k, got)
			}
		}
		cgWriteStep(t, n, tr, pol, "0")
		if e := cgOneEvent(t, n, pol); e.Kind != EventAccepted {
			t.Fatalf("policy boost 0 under global 1: %+v, want accepted", e)
		}
		cgWriteStep(t, n, tr, global, "1")
		if got := nodeRead(t, tr, pol); got != "0\n" {
			t.Fatalf("global boost 1 again: policy3 boost %q, want 0: the state it already has changes no policy", got)
		}
		if got := nodeRead(t, tr, global); got != "1\n" {
			t.Fatalf("global boost %q, want 1", got)
		}
	})
	t.Run("acpi-cpufreq", func(t *testing.T) {
		t.Parallel()
		p := ensureBuiltin(t, "amd-epyc-2s-energy-only")
		n, tr := cgNode(t, p)
		global := ckGlobalBoost()
		ckAgentFile(t, tr, global)
		for _, k := range cgPolicies(n) {
			ckAgentFile(t, tr, nodePolicyFile(k, "boost"))
			if got := nodeRead(t, tr, nodePolicyFile(k, "boost")); got != "1\n" {
				t.Fatalf("policy%d boost at boot %q, want the global 1", k, got)
			}
		}
		cgWriteStep(t, n, tr, nodePolicyFile(1, "boost"), "0")
		if got, want := ckPolicy(t, n, 1).ceilingKHz(), int64(2400000); got != want {
			t.Errorf("policy1 boost 0: ceiling %d kHz, want the top entry %d", got, want)
		}
		if got, want := ckPolicy(t, n, 0).ceilingKHz(), int64(3707812); got != want {
			t.Errorf("policy0 ceiling %d kHz, want %d: boost is per policy", got, want)
		}
		cgWriteStep(t, n, tr, global, "0")
		if got := nodeRead(t, tr, nodePolicyFile(0, "boost")); got != "0\n" {
			t.Fatalf("global boost 0: policy0 boost %q, want 0", got)
		}
		if got := ckPolicy(t, n, 0).ceilingKHz(); got != 2400000 {
			t.Errorf("global boost 0: policy0 ceiling %d kHz, want 2400000", got)
		}
		cgWriteStep(t, n, tr, global, "1")
		for _, k := range []int{0, 1} {
			if got := nodeRead(t, tr, nodePolicyFile(k, "boost")); got != "1\n" {
				t.Fatalf("global boost 1: policy%d boost %q, want 1", k, got)
			}
		}
		if got := ckPolicy(t, n, 1).ceilingKHz(); got != 3707812 {
			t.Errorf("global boost 1: policy1 ceiling %d kHz, want 3707812", got)
		}
	})
}

// CK03: a Node started over a tree with turbo off still restores the
// uncapped maximum when turbo comes back on, where the kernel keeps the
// policy's request under the cap (intel_pstate.c:1351-1361,
// amd-pstate.c:665-702): the W1 Node took the capped scaling_max_freq for
// the request, so after a restart with no_turbo=1 or an amd-pstate policy's
// boost 0 the policies stayed at the base for good. A request written
// below the cap is still kept, and the restart itself reverts nothing.
func TestCK03RestartWithTurboOffRestoresTheUncappedMaximum(t *testing.T) {
	t.Parallel()
	t.Run("intel_pstate no_turbo", func(t *testing.T) {
		t.Parallel()
		p := ensureBuiltin(t, "intel-xeon-2s-rapl")
		n, tr := cgNode(t, p)
		noTurbo := cgIntelPstateFile("no_turbo")
		cgWriteStep(t, n, tr, noTurbo, "1")
		cgWriteStep(t, n, tr, nodePolicyFile(0, "scaling_max_freq"), "2000000")

		n = nodeNewNode(t, p, tr)
		nodeStep(t, n, time.Second)
		for _, k := range []int{0, 1} {
			if ev := nodeEvents(n, nodePolicyFile(k, "scaling_max_freq")); len(ev) != 0 {
				t.Fatalf("restart: policy%d scaling_max_freq events %+v, want none", k, ev)
			}
		}
		cgWriteStep(t, n, tr, noTurbo, "0")
		for _, k := range cgPolicies(n) {
			want := "3900000\n"
			if k == 0 {
				want = "2000000\n"
			}
			if got := nodeRead(t, tr, nodePolicyFile(k, "scaling_max_freq")); got != want {
				t.Fatalf("no_turbo 0 after a restart: policy%d scaling_max_freq %q, want %q", k, got, want)
			}
		}
	})
	t.Run("amd-pstate boost", func(t *testing.T) {
		t.Parallel()
		p := ensureBuiltin(t, "amd-epyc-2s-hsmp")
		n, tr := cgNode(t, p)
		boost, maxFreq := nodePolicyFile(3, "boost"), nodePolicyFile(3, "scaling_max_freq")
		cgWriteStep(t, n, tr, boost, "0")
		if got := nodeRead(t, tr, maxFreq); got != "2600000\n" {
			t.Fatalf("boost 0: scaling_max_freq %q, want 2600000", got)
		}

		n = nodeNewNode(t, p, tr)
		nodeStep(t, n, time.Second)
		if ev := nodeEvents(n, maxFreq); len(ev) != 0 {
			t.Fatalf("restart: scaling_max_freq events %+v, want none", ev)
		}
		cgWriteStep(t, n, tr, boost, "1")
		if got := nodeRead(t, tr, maxFreq); got != "4500000\n" {
			t.Fatalf("boost 1 after a restart: scaling_max_freq %q, want 4500000", got)
		}
	})
}
