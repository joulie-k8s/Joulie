// Package simcluster_test holds the repository-level tests of the simulated
// cluster harness: the cluster specs and values committed here, and the
// manifests and scripts under scripts/simcluster. None of them needs a
// cluster.
//
// Several files share this package, so the package-level names of each file
// carry its prefix: sp for specs_test.go.
package simcluster_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/simcluster"
)

// spSpecsDir holds the committed cluster specs.
const spSpecsDir = "specs"

// The agent DaemonSet's node selector (agent.daemonset.nodeSelector in
// charts/joulie/values.yaml).
const (
	spManagedLabel = "joulie.io/managed"
	spManagedValue = "true"
)

// spMediumNodes and spMediumAgents are the size of medium.yaml: every Node,
// and those the agent DaemonSet reaches with the chart's default
// tolerations, which take every taint: all but the unmanaged pool.
const (
	spMediumNodes  = 50
	spMediumAgents = 48
)

// spSimLib is the simulated cluster's shared script, which names the one
// architecture the images are built for (SIM_IMAGE_ARCH).
var spSimLib = filepath.Join("..", "..", "scripts", "simcluster", "lib.sh")

// spChartValues is the Joulie chart's values file. Its agent.tolerations
// are the tolerations the agent DaemonSet carries by default.
var spChartValues = filepath.Join("..", "..", "charts", "joulie", "values.yaml")

// spSmokeProfiles are the profiles the smoke spec covers, one untainted pool
// each: one Node per hardware family. The list is pinned, not read from the
// builtins, so smoke keeps its size as profiles are added; profiles.yaml
// runs every builtin profile (TestSP05ProfilesCoversEveryBuiltin).
var spSmokeProfiles = []string{
	"amd-epyc-2s-energy-only",
	"amd-epyc-2s-hsmp",
	"amd-instinct-mi300x-8gpu",
	"intel-xeon-2s-rapl",
	"nvidia-h100-nvl-8gpu",
	"nvidia-h100-sxm-8gpu",
	"vm-no-powercap",
}

// spMIGInstances is, per GPU product, how many instances of each MIG profile
// one GPU holds: NVIDIA MIG User Guide, supported-mig-profiles.html, the
// tables "H100 80GB product (PCIe and SXM5)" and "H100 94GB product (PCIe
// and SXM5)", column "Number of Instances Available". A spec that sets a MIG
// profile on another product fails until its table is added here.
var spMIGInstances = map[string]map[string]int{
	"NVIDIA H100 80GB HBM3": {"1g.10gb": 7, "1g.10gb+me": 1, "1g.20gb": 4, "2g.20gb": 3, "3g.40gb": 2, "4g.40gb": 1, "7g.80gb": 1},
	"NVIDIA H100 NVL":       {"1g.12gb": 7, "1g.12gb+me": 1, "1g.24gb": 4, "2g.24gb": 3, "3g.47gb": 2, "4g.47gb": 1, "7g.94gb": 1},
}

// spLoadSpec loads the committed spec name with the loader the simulator
// and the harness run, clusterspec.LoadFile then Expand with the builtin
// profiles, and checks each pool's MIG profile against its GPU
// (spCheckMIG), which the loader does not know. It returns the spec and each
// pool's profile, overrides applied, by pool name.
func spLoadSpec(t *testing.T, name string) (*clusterspec.Spec, map[string]*hwemu.Profile) {
	t.Helper()
	s, err := clusterspec.LoadFile(filepath.Join(spSpecsDir, name))
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.Expand(clusterspec.Profiles(""))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	profiles := map[string]*hwemu.Profile{}
	for _, m := range members {
		profiles[m.Pool] = m.Profile
	}
	for _, pool := range s.Spec.Pools {
		if err := spCheckMIG(profiles[pool.Name]); err != nil {
			t.Errorf("%s: pool %s: %v", name, pool.Name, err)
		}
	}
	return s, profiles
}

// spDecodeSpec decodes one spec file strictly: an unknown key fails, except
// under spec.harness, which stays raw.
func spDecodeSpec(t *testing.T, path string) *clusterspec.Spec {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s clusterspec.Spec
	if err := yaml.UnmarshalStrict(b, &s); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if s.APIVersion != clusterspec.APIVersion || s.Kind != clusterspec.Kind {
		t.Fatalf("%s: apiVersion %q kind %q, want %s %s", path, s.APIVersion, s.Kind, clusterspec.APIVersion, clusterspec.Kind)
	}
	return &s
}

// spCheckMIG checks a profile that exposes MIG devices against
// spMIGInstances: its GPU product supports the MIG profile, and one GPU
// holds that many instances of it. Profile.Validate does not know which MIG
// profiles a GPU has.
func spCheckMIG(p *hwemu.Profile) error {
	if p == nil {
		return nil
	}
	g := p.GPUs
	if g == nil || (g.Exposure.V != "mig-single" && g.Exposure.V != "mig-mixed") {
		return nil
	}
	table, ok := spMIGInstances[g.Product.V]
	if !ok {
		return fmt.Errorf("exposure %s on %q, a product with no MIG table in spMIGInstances", g.Exposure.V, g.Product.V)
	}
	most, ok := table[g.MIGProfile]
	if !ok {
		return fmt.Errorf("MIG profile %s is not supported on %q; it supports %s", g.MIGProfile, g.Product.V, strings.Join(spSortedKeys(table), ", "))
	}
	if g.MIGDevicesPerGPU > most {
		return fmt.Errorf("%d MIG devices of %s per GPU, but %q holds at most %d", g.MIGDevicesPerGPU, g.MIGProfile, g.Product.V, most)
	}
	return nil
}

// spParseHarness decodes the spec's harness section and checks that it names
// only pools of the spec.
func spParseHarness(t *testing.T, s *clusterspec.Spec) simcluster.Harness {
	t.Helper()
	h, err := simcluster.ParseHarness(s.Spec.Harness)
	if err != nil {
		t.Fatalf("spec %s: %v", s.Metadata.Name, err)
	}
	pools := map[string]bool{}
	for _, pool := range s.Spec.Pools {
		pools[pool.Name] = true
	}
	for name := range h.Pools {
		if !pools[name] {
			t.Errorf("spec %s: spec.harness.pools.%s names no pool", s.Metadata.Name, name)
		}
	}
	return h
}

// spManaged reports whether a pool's Nodes carry the agent DaemonSet's node
// selector: the defaults' labels, then the pool's. Profiles never set it.
func spManaged(s *clusterspec.Spec, pool clusterspec.Pool) bool {
	v := s.Spec.Defaults.Labels[spManagedLabel]
	if pv, ok := pool.Labels[spManagedLabel]; ok {
		v = pv
	}
	return v == spManagedValue
}

// spAgentTolerations returns the agent DaemonSet's default tolerations,
// agent.tolerations in the chart's values file.
func spAgentTolerations(t *testing.T) []corev1.Toleration {
	t.Helper()
	b, err := os.ReadFile(spChartValues)
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Agent struct {
			Tolerations []corev1.Toleration `json:"tolerations"`
		} `json:"agent"`
	}
	if err := yaml.Unmarshal(b, &values); err != nil {
		t.Fatalf("%s: %v", spChartValues, err)
	}
	return values.Agent.Tolerations
}

// spImageArch returns SIM_IMAGE_ARCH of scripts/simcluster/lib.sh: the
// agent image of the simulated cluster is built for it alone, so a Node of
// another architecture gets no agent.
func spImageArch(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(spSimLib)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "SIM_IMAGE_ARCH="); ok {
			return v
		}
	}
	t.Fatalf("%s sets no SIM_IMAGE_ARCH", spSimLib)
	return ""
}

// spUntolerated returns the taints of a pool, profile's then pool's, that
// keep the agent DaemonSet off its Nodes: every NoSchedule and NoExecute
// taint none of tolerations tolerates. The KWOK taint is not among them:
// clusterspec adds it when expanding, and placement ignores it. The
// node.kubernetes.io taints the DaemonSet controller tolerates need the real
// placement check, so a spec that sets one fails here.
func spUntolerated(t *testing.T, pool clusterspec.Pool, p *hwemu.Profile, tolerations []corev1.Toleration) []corev1.Taint {
	t.Helper()
	var out []corev1.Taint
	for _, taint := range append(append([]corev1.Taint(nil), p.Node.Taints...), pool.Taints...) {
		if strings.HasPrefix(taint.Key, "node.kubernetes.io/") {
			t.Errorf("pool %s: taint %s: the DaemonSet controller may tolerate it; these tests do not model that", pool.Name, taint.ToString())
		}
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for i := range tolerations {
			if tolerations[i].ToleratesTaint(&taint) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			out = append(out, taint)
		}
	}
	return out
}

func spSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestSP01SpecsLoadWithBuiltinProfiles checks that every committed spec
// loads and expands with clusterspec and the builtin profiles, decodes
// strictly, and that its harness section parses, names only pools of the
// spec and sets the tool delivery of every GPU pool: a pool left out
// follows the install, which delivers no GPU tools by default, so a spec
// says which of its GPU pools have them. The harness subtest checks the
// harness parser on sections the committed specs do not have.
func TestSP01SpecsLoadWithBuiltinProfiles(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(spSpecsDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			s, profiles := spLoadSpec(t, name)
			if len(s.Spec.Pools) == 0 {
				t.Fatalf("%s has no pools", name)
			}
			if len(profiles) != len(s.Spec.Pools) {
				t.Errorf("%s: %d of %d pools have a valid builtin profile", name, len(profiles), len(s.Spec.Pools))
			}
			h := spParseHarness(t, s)
			for _, pool := range s.Spec.Pools {
				if p := profiles[pool.Name]; p != nil && p.GPUs != nil && h.Pools[pool.Name].ToolDelivery == "" {
					t.Errorf("%s: GPU pool %s sets no spec.harness.pools.%s.toolDelivery, so it follows the install, which delivers no GPU tools by default", name, pool.Name, pool.Name)
				}
			}
		})
	}
	t.Run("harness", func(t *testing.T) {
		parse := func(section string) (simcluster.Harness, error) {
			doc := "apiVersion: " + clusterspec.APIVersion + "\nkind: " + clusterspec.Kind + "\nmetadata: {name: h}\nspec:\n  pools: [{name: a, profile: vm-no-powercap, count: 1}]\n"
			if section != "" {
				doc += "  harness: " + section + "\n"
			}
			var s clusterspec.Spec
			if err := yaml.UnmarshalStrict([]byte(doc), &s); err != nil {
				t.Fatalf("harness %s: %v", section, err)
			}
			return simcluster.ParseHarness(s.Spec.Harness)
		}
		for _, section := range []string{"", "null", "{}", "{pools: {}}", "{pools: {a: {}}}"} {
			h, err := parse(section)
			if err != nil {
				t.Errorf("harness %q: %v", section, err)
				continue
			}
			for _, pool := range []string{"a", "unlisted"} {
				for _, install := range []string{simcluster.ToolDeliveryMounted, simcluster.ToolDeliveryNone} {
					if got := h.ToolDelivery(pool, install); got != install {
						t.Errorf("harness %q: pool %s has tool delivery %q under an install that delivers %s, want the install's", section, pool, got, install)
					}
				}
			}
		}
		for _, d := range []string{simcluster.ToolDeliveryMounted, simcluster.ToolDeliveryNone} {
			h, err := parse("{pools: {a: {toolDelivery: " + d + "}}}")
			if err != nil || h.ToolDelivery("a", "") != d {
				t.Errorf("harness toolDelivery %s: tool delivery %q, error %v", d, h.ToolDelivery("a", ""), err)
			}
		}
		for section, want := range map[string]string{
			"{pools: {a: {toolDelivery: bogus}}}":          `spec.harness.pools.a.toolDelivery "bogus"`,
			"{pools: {a: {toolDelivery: none, extra: 1}}}": `"extra"`,
			"{pool: {a: {}}}":                              `"pool"`,
		} {
			_, err := parse(section)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("harness %s: error %v, want one naming %s", section, err, want)
			}
		}
	})
}

// TestSP02SmokeCoversEachFamilyOnce checks that the smoke pools the agent
// DaemonSet reaches with the chart's default tolerations, those with a
// taint of their own left out, use each profile of spSmokeProfiles exactly
// once and no other profile, so the smoke run exercises each hardware family
// with an agent, GPU Nodes with their vendor taint included. It also checks
// the two Nodes E12 needs: a managed pool with a NoSchedule taint of its
// own, which the agent still reaches (D15), and a managed pool of an
// architecture the images are not built for, which no agent reaches.
func TestSP02SmokeCoversEachFamilyOnce(t *testing.T) {
	s, profiles := spLoadSpec(t, "smoke.yaml")
	tolerations := spAgentTolerations(t)
	imageArch := spImageArch(t)
	builtins, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.Expand(clusterspec.Profiles(""))
	if err != nil {
		t.Fatal(err)
	}
	arch := map[string]string{}
	for _, m := range members {
		arch[m.Pool] = m.Node.Labels[corev1.LabelArchStable]
	}
	reached := map[string][]string{}
	var tainted, otherArch []string
	for _, pool := range s.Spec.Pools {
		p := profiles[pool.Name]
		if p == nil || !spManaged(s, pool) {
			continue
		}
		if arch[pool.Name] != imageArch {
			otherArch = append(otherArch, pool.Name)
			continue
		}
		if taints := spUntolerated(t, pool, p, tolerations); len(taints) > 0 {
			t.Errorf("smoke pool %s: the agent DaemonSet does not tolerate %v, while its default takes every taint (D15)", pool.Name, taints)
			continue
		}
		if slices.ContainsFunc(pool.Taints, func(taint corev1.Taint) bool { return taint.Effect == corev1.TaintEffectNoSchedule }) {
			tainted = append(tainted, pool.Name)
			continue
		}
		reached[pool.Profile] = append(reached[pool.Profile], pool.Name)
	}
	covered := map[string]bool{}
	for _, name := range spSmokeProfiles {
		covered[name] = true
		if _, ok := builtins[name]; !ok {
			t.Errorf("smoke profile %s is not builtin", name)
		}
		if pools := reached[name]; len(pools) != 1 {
			t.Errorf("smoke profile %s is used by smoke pools %v that the agent reaches, want exactly one", name, pools)
		}
	}
	for _, name := range spSortedKeys(reached) {
		if !covered[name] {
			t.Errorf("smoke pools %v that the agent reaches use profile %s, which is not in spSmokeProfiles", reached[name], name)
		}
	}
	if len(tainted) == 0 {
		t.Error("no managed smoke pool has a NoSchedule taint of its own, so the run never shows the agent on a tainted Node (D15)")
	}
	if len(otherArch) == 0 {
		t.Errorf("no managed smoke pool has an architecture other than %s, the images', so no Node reports AgentReporting False in E12", imageArch)
	}
}

// TestSP03MediumCoversTheHarnessCases checks the medium spec: every pool,
// and so every hardware family, has at least 2 Nodes; there is a pool for
// each case the harness checks beyond smoke (MIG, renamed time-slicing, no
// NFD labels, no GPU tool delivery, a pool with a taint of its own, an
// unmanaged pool); and the agent DaemonSet, with the chart's default
// tolerations, reaches 48 of its 50 Nodes. The MIG pool's profile and device count are
// checked against its GPU by spLoadSpec.
func TestSP03MediumCoversTheHarnessCases(t *testing.T) {
	s, profiles := spLoadSpec(t, "medium.yaml")
	h := spParseHarness(t, s)
	tolerations := spAgentTolerations(t)
	cases := map[string][]string{}
	nodes, agents := 0, 0
	for _, pool := range s.Spec.Pools {
		nodes += pool.Count
		if pool.Count < 2 {
			t.Errorf("pool %s has %d Nodes, want at least 2", pool.Name, pool.Count)
		}
		p := profiles[pool.Name]
		if p == nil {
			continue
		}
		if o := pool.Overrides; o != nil {
			if o.NFD == "absent" {
				cases["nfd: absent"] = append(cases["nfd: absent"], pool.Name)
			}
			if o.GPUs != nil && (o.GPUs.Exposure == "mig-mixed" || o.GPUs.Exposure == "timeslice-rename") {
				cases[o.GPUs.Exposure] = append(cases[o.GPUs.Exposure], pool.Name)
			}
		}
		if p.GPUs != nil && h.Pools[pool.Name].ToolDelivery == simcluster.ToolDeliveryNone {
			cases["toolDelivery: none"] = append(cases["toolDelivery: none"], pool.Name)
		}
		if len(pool.Taints) > 0 {
			cases["tainted"] = append(cases["tainted"], pool.Name)
		}
		switch {
		case !spManaged(s, pool):
			cases["unmanaged"] = append(cases["unmanaged"], pool.Name)
		case len(spUntolerated(t, pool, p, tolerations)) == 0:
			agents += pool.Count
		}
	}
	for _, c := range []string{"mig-mixed", "timeslice-rename", "nfd: absent", "toolDelivery: none", "tainted", "unmanaged"} {
		if len(cases[c]) == 0 {
			t.Errorf("no medium pool covers %s", c)
		}
	}
	if nodes != spMediumNodes {
		t.Errorf("medium has %d Nodes, want %d", nodes, spMediumNodes)
	}
	if agents != spMediumAgents {
		t.Errorf("the agent DaemonSet reaches %d medium Nodes, want %d", agents, spMediumAgents)
	}
}

// TestSP04SpecsUseNoLocalProfileOrMeasurement checks every committed spec:
// each pool's profile is builtin, never one from SIM_PROFILES_DIR, and no
// measured: provenance appears anywhere. Local profiles and measurements
// describe one site's machines and stay outside the repository.
func TestSP04SpecsUseNoLocalProfileOrMeasurement(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(spSpecsDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no spec under %s", spSpecsDir)
	}
	builtins, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range bytes.Split(b, []byte("\n")) {
			if bytes.Contains(line, []byte("measured:")) {
				t.Errorf("%s:%d: measured: provenance belongs only in local profiles", path, i+1)
			}
		}
		s := spDecodeSpec(t, path)
		for _, pool := range s.Spec.Pools {
			if _, ok := builtins[pool.Profile]; !ok {
				t.Errorf("%s: pool %s uses profile %q, which is not builtin; a profile from SIM_PROFILES_DIR stays local", path, pool.Name, pool.Profile)
			}
		}
	}
}

// TestSP05ProfilesCoversEveryBuiltin checks profiles.yaml: one managed Node
// of every builtin profile and of nothing else, each one the agent
// DaemonSet reaches with the chart's default tolerations, so a run of it
// takes every builtin profile end to end; a new builtin profile fails here
// until it is added.
func TestSP05ProfilesCoversEveryBuiltin(t *testing.T) {
	s, profiles := spLoadSpec(t, "profiles.yaml")
	builtins, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	tolerations := spAgentTolerations(t)
	pools := map[string][]string{}
	for _, pool := range s.Spec.Pools {
		pools[pool.Profile] = append(pools[pool.Profile], pool.Name)
		if pool.Count != 1 {
			t.Errorf("pool %s has %d Nodes, want 1", pool.Name, pool.Count)
		}
		if !spManaged(s, pool) {
			t.Errorf("pool %s is not managed", pool.Name)
		}
		if p := profiles[pool.Name]; p != nil {
			if taints := spUntolerated(t, pool, p, tolerations); len(taints) > 0 {
				t.Errorf("pool %s: the agent DaemonSet does not tolerate %v", pool.Name, taints)
			}
		}
	}
	for _, name := range spSortedKeys(builtins) {
		if len(pools[name]) != 1 {
			t.Errorf("builtin profile %s is used by profiles.yaml pools %v, want exactly one", name, pools[name])
		}
	}
	for _, name := range spSortedKeys(pools) {
		if _, ok := builtins[name]; !ok {
			t.Errorf("profiles.yaml pools %v use %s, which is not builtin", pools[name], name)
		}
	}
}
