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
	"regexp"
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
// tolerations, which cover the GPU vendor taints: all but the unmanaged
// pool and the nvl-tainted pool, whose own taint is not tolerated.
const (
	spMediumNodes  = 50
	spMediumAgents = 46
)

// spChartValues is the Joulie chart's values file. Its agent.tolerations
// are the tolerations the agent DaemonSet carries by default.
var spChartValues = filepath.Join("..", "..", "charts", "joulie", "values.yaml")

// spNameRE is the rule for pool names and Node names: a DNS-1123 label of at
// most 63 characters, so that api.ObjectNameForNode keeps a Node's name and
// a pool name is a valid label value.
var spNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// spSmokeProfiles are the profiles the smoke spec covers, one untainted pool
// each: one Node per hardware family. The list is pinned, not read from the
// builtins, so smoke keeps its size as profiles are added; the agent's
// TestHwemuA02 (cmd/agent) runs every builtin profile.
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

// spSkipWithoutSpec skips while the spec file name is not in the tree.
// smoke.yaml comes with the loader of package clusterspec; until it lands,
// the smoke checks skip, and this helper goes when it does. A spec of the
// same metadata.name under another file name fails instead, so a rename
// never leaves the checks skipping.
func spSkipWithoutSpec(t *testing.T, name string) {
	t.Helper()
	path := filepath.Join(spSpecsDir, name)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return
	}
	others, err := filepath.Glob(filepath.Join(spSpecsDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range others {
		if s := spDecodeSpec(t, other); s.Metadata.Name == strings.TrimSuffix(name, ".yaml") {
			t.Fatalf("spec %s is in %s, where these tests do not look; move it to %s", s.Metadata.Name, other, path)
		}
	}
	t.Skipf("%s does not exist yet", path)
}

// spLoadSpec stands in for clusterspec's Load and Validate until they exist.
// It decodes the spec file name strictly, checks the spec rules (spCheckSpec)
// and checks each pool against the builtin profiles: the profile exists, and
// with the pool's overrides applied it still validates and sets only MIG
// profiles its GPU holds. It returns the spec and each pool's profile,
// overrides applied, by pool name.
func spLoadSpec(t *testing.T, name string) (*clusterspec.Spec, map[string]*hwemu.Profile) {
	t.Helper()
	s := spDecodeSpec(t, filepath.Join(spSpecsDir, name))
	for _, err := range spCheckSpec(s) {
		t.Errorf("%s: %v", name, err)
	}
	profiles := map[string]*hwemu.Profile{}
	for _, pool := range s.Spec.Pools {
		// BuiltinProfiles parses on every call, so one pool's overrides
		// never reach another pool's profile.
		builtins, err := hwemu.BuiltinProfiles()
		if err != nil {
			t.Fatal(err)
		}
		p, ok := builtins[pool.Profile]
		if !ok {
			t.Errorf("%s: pool %s: profile %q is not builtin; builtins are %s", name, pool.Name, pool.Profile, strings.Join(spSortedKeys(builtins), ", "))
			continue
		}
		if err := spApplyOverrides(p, s.Metadata.Name, pool); err != nil {
			t.Errorf("%s: pool %s: %v", name, pool.Name, err)
			continue
		}
		if err := spCheckMIG(p); err != nil {
			t.Errorf("%s: pool %s: %v", name, pool.Name, err)
			continue
		}
		profiles[pool.Name] = p
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

// spCheckSpec returns every violation of the rules package clusterspec
// validates beyond the profiles: pool and Node names follow spNameRE and are
// unique (a Node is named <prefix>-<startIndex+i>, the prefix defaulting to
// the pool name); labels set neither kubernetes.io/hostname nor a
// sim.joulie.io/ key, which the spec itself owns; each cooling zone is
// declared under facility.coolingZones; each baseLoad value is in 0..1.
func spCheckSpec(s *clusterspec.Spec) []error {
	var errs []error
	checkLabels := func(at string, labels map[string]string) {
		for _, k := range spSortedKeys(labels) {
			if k == corev1.LabelHostname || strings.HasPrefix(k, "sim.joulie.io/") {
				errs = append(errs, fmt.Errorf("%s: label %s is set by the spec itself", at, k))
			}
		}
	}
	checkLoad := func(at string, l *clusterspec.Load) {
		if l == nil {
			return
		}
		for _, v := range []struct {
			key string
			v   *float64
		}{{"cpuUtil", l.CPUUtil}, {"gpuUtil", l.GPUUtil}, {"memoryIntensity", l.MemoryIntensity}} {
			if v.v != nil && (*v.v < 0 || *v.v > 1) {
				errs = append(errs, fmt.Errorf("%s: baseLoad.%s %g is not in 0..1", at, v.key, *v.v))
			}
		}
	}
	checkName := func(at, name string) {
		if len(name) > 63 || !spNameRE.MatchString(name) {
			errs = append(errs, fmt.Errorf("%s: name %q is not a DNS-1123 label of at most 63 characters", at, name))
		}
	}
	checkLabels("defaults", s.Spec.Defaults.Labels)
	checkLoad("defaults", s.Spec.Defaults.BaseLoad)
	zones := map[string]bool{}
	for _, z := range s.Spec.Facility.CoolingZones {
		zones[z.Name] = true
	}
	pools, nodes := map[string]bool{}, map[string]string{}
	for _, pool := range s.Spec.Pools {
		at := "pool " + pool.Name
		checkName(at, pool.Name)
		if pools[pool.Name] {
			errs = append(errs, fmt.Errorf("%s: a second pool has this name", at))
		}
		pools[pool.Name] = true
		checkLabels(at, pool.Labels)
		checkLoad(at, pool.BaseLoad)
		if pool.CoolingZone != "" && !zones[pool.CoolingZone] {
			errs = append(errs, fmt.Errorf("%s: cooling zone %q is not declared under facility.coolingZones", at, pool.CoolingZone))
		}
		prefix := pool.Prefix
		if prefix == "" {
			prefix = pool.Name
		}
		for i := 0; i < pool.Count; i++ {
			node := fmt.Sprintf("%s-%d", prefix, pool.StartIndex+i)
			checkName(at, node)
			if other, ok := nodes[node]; ok {
				errs = append(errs, fmt.Errorf("%s: Node %s is also in pool %s", at, node, other))
			}
			nodes[node] = pool.Name
		}
	}
	return errs
}

// spApplyOverrides sets a pool's overrides on its profile with the
// provenance package clusterspec gives them, then validates the result.
func spApplyOverrides(p *hwemu.Profile, spec string, pool clusterspec.Pool) error {
	o := pool.Overrides
	if o == nil {
		return nil
	}
	src := fmt.Sprintf("assumed:cluster spec %s pool %s override", spec, pool.Name)
	if o.NFD != "" {
		p.Node.NFD = o.NFD
	}
	if g := o.GPUs; g != nil {
		if p.GPUs == nil {
			return fmt.Errorf("overrides gpus, but profile %s has no GPUs", p.Name)
		}
		if g.PersistenceMode != "" {
			p.GPUs.PersistenceMode = hwemu.Fact[string]{V: g.PersistenceMode, Src: src}
		}
		if g.Exposure != "" {
			p.GPUs.Exposure = hwemu.Fact[string]{V: g.Exposure, Src: src}
		}
		if g.MIGProfile != "" {
			p.GPUs.MIGProfile = g.MIGProfile
		}
		if g.MIGDevicesPerGPU != nil {
			p.GPUs.MIGDevicesPerGPU = *g.MIGDevicesPerGPU
		}
		if g.TimesliceReplicas != nil {
			p.GPUs.TimesliceReplicas = *g.TimesliceReplicas
		}
	}
	return p.Validate()
}

// spCheckMIG checks a profile that exposes MIG devices against
// spMIGInstances: its GPU product supports the MIG profile, and one GPU
// holds that many instances of it. Profile.Validate does not know which MIG
// profiles a GPU has.
func spCheckMIG(p *hwemu.Profile) error {
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

// TestSP01SpecsLoadWithBuiltinProfiles checks that smoke.yaml and medium.yaml
// load and expand with clusterspec and the builtin profiles, decode strictly
// and follow the spec rules, that every pool's profile is
// builtin and validates with the pool's overrides, and that the harness
// section parses, names only pools of the spec and leaves every other pool
// on the mounted tool delivery (U7). The harness subtest checks the harness
// parser on sections the committed specs do not have.
func TestSP01SpecsLoadWithBuiltinProfiles(t *testing.T) {
	for _, name := range []string{"smoke.yaml", "medium.yaml"} {
		t.Run(name, func(t *testing.T) {
			spSkipWithoutSpec(t, name)
			s, profiles := spLoadSpec(t, name)
			if len(s.Spec.Pools) == 0 {
				t.Fatalf("%s has no pools", name)
			}
			// The loader the simulator and the harness run also checks
			// what spLoadSpec does not, such as a pool taint that repeats
			// the key and effect of its profile's taint.
			loaded, err := clusterspec.LoadFile(filepath.Join(spSpecsDir, name))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loaded.Expand(clusterspec.Profiles("")); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			if len(profiles) != len(s.Spec.Pools) {
				t.Errorf("%s: %d of %d pools have a valid builtin profile", name, len(profiles), len(s.Spec.Pools))
			}
			h := spParseHarness(t, s)
			for _, pool := range s.Spec.Pools {
				if _, listed := h.Pools[pool.Name]; !listed && h.ToolDelivery(pool.Name) != simcluster.ToolDeliveryMounted {
					t.Errorf("%s: pool %s is not under spec.harness.pools, and its tool delivery is %q, want %s", name, pool.Name, h.ToolDelivery(pool.Name), simcluster.ToolDeliveryMounted)
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
		for _, section := range []string{"", "null", "{}", "{pools: {}}", "{pools: {a: {}}}", "{pools: {a: {toolDelivery: mounted}}}"} {
			h, err := parse(section)
			if err != nil {
				t.Errorf("harness %q: %v", section, err)
				continue
			}
			for _, pool := range []string{"a", "unlisted"} {
				if got := h.ToolDelivery(pool); got != simcluster.ToolDeliveryMounted {
					t.Errorf("harness %q: pool %s has tool delivery %q, want %s", section, pool, got, simcluster.ToolDeliveryMounted)
				}
			}
		}
		if h, err := parse("{pools: {a: {toolDelivery: none}}}"); err != nil || h.ToolDelivery("a") != simcluster.ToolDeliveryNone {
			t.Errorf("harness toolDelivery none: tool delivery %q, error %v; want %s", h.ToolDelivery("a"), err, simcluster.ToolDeliveryNone)
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
// DaemonSet reaches, with the chart's default tolerations, use each profile
// of spSmokeProfiles exactly once and no other profile, so the smoke run
// exercises each hardware family with an agent, GPU Nodes with their vendor
// taint included (D15). It also checks that one managed pool has a
// NoSchedule taint the agent does not tolerate: a Node Joulie plans for and
// no agent reaches.
func TestSP02SmokeCoversEachFamilyOnce(t *testing.T) {
	spSkipWithoutSpec(t, "smoke.yaml")
	s, profiles := spLoadSpec(t, "smoke.yaml")
	tolerations := spAgentTolerations(t)
	builtins, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	reached := map[string][]string{}
	var unreached []string
	for _, pool := range s.Spec.Pools {
		p := profiles[pool.Name]
		if p == nil || !spManaged(s, pool) {
			continue
		}
		taints := spUntolerated(t, pool, p, tolerations)
		if len(taints) == 0 {
			reached[pool.Profile] = append(reached[pool.Profile], pool.Name)
		}
		for _, taint := range taints {
			if taint.Effect == corev1.TaintEffectNoSchedule {
				unreached = append(unreached, pool.Name)
				break
			}
		}
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
	if len(unreached) == 0 {
		t.Error("no managed smoke pool has a NoSchedule taint the agent DaemonSet does not tolerate")
	}
}

// TestSP03MediumCoversTheHarnessCases checks the medium spec: every pool,
// and so every hardware family, has at least 2 Nodes; there is a pool for
// each case the harness checks beyond smoke (MIG, renamed time-slicing, no
// NFD labels, no GPU tool delivery, a tainted managed pool, an unmanaged
// pool); and the agent DaemonSet, with the chart's default tolerations,
// reaches 46 of its 50 Nodes. The MIG pool's profile and device count are
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
		if p.GPUs != nil && h.ToolDelivery(pool.Name) == simcluster.ToolDeliveryNone {
			cases["toolDelivery: none"] = append(cases["toolDelivery: none"], pool.Name)
		}
		taints := spUntolerated(t, pool, p, tolerations)
		switch {
		case !spManaged(s, pool):
			cases["unmanaged"] = append(cases["unmanaged"], pool.Name)
		case len(taints) > 0:
			cases["tainted"] = append(cases["tainted"], pool.Name)
		default:
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
