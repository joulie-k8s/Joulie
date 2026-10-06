// Package hwemutest builds emulated nodes for tests outside package hwemu. A
// test names a builtin profile, changes it if the case needs a variant, and
// gets a rendered tree in its own temporary directory with the physics running
// over it, so the code under test reads files and runs tools exactly as it
// would on the hardware the profile describes.
package hwemutest

import (
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderSeed seeds the energy counters every node starts from: not zero, so a
// counter starts somewhere inside its range as on a running machine, and
// fixed, so a run is reproducible.
const renderSeed = 1

// NewNode renders the builtin profile named profile into t.TempDir and starts
// its physics at the current time. It returns the node, its tree and the
// profile it was rendered from.
//
// mutate, when not nil, changes a fresh copy of the profile before it is
// rendered. That is how a test builds a variant: psys or mmio zones, dies,
// per-core policies, a locked limit, nfd absent, an exposure mode,
// unsupported fields, a secondary die, a default limit below the maximum.
// Every fact the mutation sets needs a source, as in a profile file.
//
// The node is named after the test, so two tests never share a node name.
func NewNode(t testing.TB, profile string, mutate func(*hwemu.Profile)) (*hwemu.Node, *hwemu.Tree, *hwemu.Profile) {
	t.Helper()
	p := builtin(t, profile)
	if mutate != nil {
		mutate(p)
	}
	tree := render(t, p)
	n, err := hwemu.NewNode(p, tree, hwemu.Options{})
	if err != nil {
		t.Fatalf("hwemutest: start node %s: %v", profile, err)
	}
	return n, tree, p
}

// SteadyPackagePower is the total power, in W, of every CPU package of p
// once it has settled under load with throttlePct percent of its cpufreq
// policies held at cpuinfo_min_freq. The policies are taken lowest numbered
// first, rounding up, which is how a cpufreq controller that throttles that
// share picks them. It is the cap such a controller should settle at
// throttlePct for. The power comes from a node of its own, so the caller's
// node is not touched.
func SteadyPackagePower(t testing.TB, p *hwemu.Profile, load hwemu.Load, throttlePct int) float64 {
	t.Helper()
	if throttlePct < 0 || throttlePct > 100 {
		t.Fatalf("hwemutest: throttle %d%% is outside [0, 100]", throttlePct)
	}
	tree := render(t, p)
	n, err := hwemu.NewNode(p, tree, hwemu.Options{Start: time.Unix(0, 0)})
	if err != nil {
		t.Fatalf("hwemutest: start node %s: %v", p.Name, err)
	}
	n.SetLoad(load)

	policies := policyDirs(t, tree)
	throttled := int(math.Ceil(float64(len(policies)) * float64(throttlePct) / 100))
	for _, dir := range policies[:throttled] {
		lowest, err := os.ReadFile(filepath.Join(dir, "cpuinfo_min_freq"))
		if err != nil {
			t.Fatalf("hwemutest: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scaling_max_freq"), lowest, 0); err != nil {
			t.Fatalf("hwemutest: %v", err)
		}
	}

	// Frequencies ramp and PL1 limits settle over a few time constants; a
	// minute of node time is far beyond any profile's.
	if err := n.Step(time.Minute); err != nil {
		t.Fatalf("hwemutest: step %s: %v", p.Name, err)
	}
	settled := packagePower(n)
	if err := n.Step(time.Second); err != nil {
		t.Fatalf("hwemutest: step %s: %v", p.Name, err)
	}
	if after := packagePower(n); math.Abs(after-settled) > 0.01 {
		t.Fatalf("hwemutest: %s has not settled after a minute: %.3f W, then %.3f W a second later", p.Name, settled, after)
	}
	return settled
}

func packagePower(n *hwemu.Node) float64 {
	total := 0.0
	for _, pk := range n.State().Packages {
		total += pk.PowerW
	}
	return total
}

// builtin returns a fresh copy of a builtin profile, so a mutation never
// reaches another test.
func builtin(t testing.TB, name string) *hwemu.Profile {
	t.Helper()
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatalf("hwemutest: builtin profiles: %v", err)
	}
	p, ok := profiles[name]
	if !ok {
		names := make([]string, 0, len(profiles))
		for n := range profiles {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("hwemutest: no builtin profile %q; have %s", name, strings.Join(names, ", "))
	}
	return p
}

// render renders p into a fresh directory of the test.
func render(t testing.TB, p *hwemu.Profile) *hwemu.Tree {
	t.Helper()
	tree, err := hwemu.Render(p, filepath.Join(t.TempDir(), "node"), hwemu.RenderOptions{NodeName: nodeName(t), Seed: renderSeed})
	if err != nil {
		t.Fatalf("hwemutest: %v", err)
	}
	return tree
}

// policyDirs lists the cpufreq policy directories of the tree, lowest index
// first.
func policyDirs(t testing.TB, tree *hwemu.Tree) []string {
	t.Helper()
	root := filepath.Join(tree.SysRoot, filepath.FromSlash(path.Dir(layout.PolicyDir(0))))
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("hwemutest: %s has no cpufreq policies: %v", tree.Root, err)
	}
	var indices []int
	for _, e := range entries {
		if k, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "policy")); err == nil && strings.HasPrefix(e.Name(), "policy") {
			indices = append(indices, k)
		}
	}
	sort.Ints(indices)
	out := make([]string, 0, len(indices))
	for _, k := range indices {
		out = append(out, filepath.Join(tree.SysRoot, filepath.FromSlash(layout.PolicyDir(k))))
	}
	return out
}

// nodeName is a node name made from the test's name: lower case, every run of
// other characters a dash, at most 63 characters with a hash of the full name
// when it is cut, so it is a valid object name and unique per test.
func nodeName(t testing.TB) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(t.Name()) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "node"
	}
	if len(name) > 63 {
		h := fnv.New32a()
		h.Write([]byte(t.Name()))
		name = strings.TrimRight(name[:54], "-") + "-" + fmt.Sprintf("%08x", h.Sum32())
	}
	return name
}
