package clusterspec

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// testSmokeSpec is the committed smoke spec.
var testSmokeSpec = filepath.Join("..", "..", "..", "tests", "simcluster", "specs", "smoke.yaml")

// testLabProfile is the local profile lab-vm-4cpu.
var testLabProfile = filepath.Join("testdata", "lab-vm-4cpu.yaml")

// testLocalProfiles returns a new profiles directory holding lab-vm-4cpu
// renamed to name, with each edit pair (old, new) applied: old must occur
// once in the file.
func testLocalProfiles(t *testing.T, name string, edits ...string) string {
	t.Helper()
	b, err := os.ReadFile(testLabProfile)
	if err != nil {
		t.Fatal(err)
	}
	doc := testReplace(t, string(b), "name: lab-vm-4cpu", "name: "+name)
	for i := 0; i+1 < len(edits); i += 2 {
		doc = testReplace(t, doc, edits[i], edits[i+1])
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testOnePool is a spec of one pool of one Node of profile.
func testOnePool(t *testing.T, profile string) *Spec {
	t.Helper()
	return testParse(t, `apiVersion: sim.joulie.io/v1alpha1
kind: ClusterSpec
metadata: {name: one}
spec:
  pools:
  - {name: lab, profile: `+profile+`, count: 1}
`)
}

// testExpand loads the spec at path and expands it with src, or fails the
// test.
func testExpand(t *testing.T, path string, src ProfileSource) []Member {
	t.Helper()
	s, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.Expand(src)
	if err != nil {
		t.Fatalf("%s: Expand: %v", path, err)
	}
	return members
}

// testMember returns the member called name.
func testMember(t *testing.T, members []Member, name string) Member {
	t.Helper()
	for _, m := range members {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no member %s", name)
	return Member{}
}

// CS03: a pool's Nodes are <prefix>-<startIndex+i>, the prefix defaulting to
// the pool name; Expand returns them sorted by name; and each name is its
// own object name.
func TestCS03NodeNamesFromPrefixAndStartIndex(t *testing.T) {
	t.Parallel()
	members := testExpand(t, filepath.Join("testdata", "names.yaml"), Profiles(""))
	var got []string
	for _, m := range members {
		got = append(got, m.Name)
		if m.Node.Name != m.Name || m.Node.Labels[corev1.LabelHostname] != m.Name {
			t.Errorf("member %s: Node %s, hostname %s", m.Name, m.Node.Name, m.Node.Labels[corev1.LabelHostname])
		}
		if obj := api.ObjectNameForNode(m.Name); obj != m.Name {
			t.Errorf("ObjectNameForNode(%s) = %s", m.Name, obj)
		}
	}
	want := []string{"b-0", "b-1", "gpu-10", "gpu-8", "gpu-9"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	if m := testMember(t, members, "gpu-10"); m.Pool != "gpu-pool" {
		t.Errorf("gpu-10 is in pool %s, want gpu-pool", m.Pool)
	}
}

// CS04: labels are the profile's, then the defaults', then the pool's, its
// rack and cooling zone, then the cluster and pool labels, each level
// overriding the one before; annotations are the profile annotation, the
// defaults' and the pool's, then the KWOK annotation.
func TestCS04LabelAndAnnotationPrecedence(t *testing.T) {
	t.Parallel()
	members := testExpand(t, filepath.Join("testdata", "labels.yaml"), Profiles(""))
	m := testMember(t, members, "vm-1")
	wantLabels := map[string]string{
		// profile
		"feature.node.kubernetes.io/cpu-model.vendor_id": "Intel",
		corev1.LabelArchStable:                           "amd64",
		corev1.LabelOSStable:                             "linux",
		corev1.LabelHostname:                             "vm-1",
		// defaults, one over the profile's KVM
		"feature.node.kubernetes.io/cpu-model.hypervisor": "none",
		"example.com/from-defaults":                       "true",
		// pool over defaults, rack, zone
		"example.com/tier": "pool",
		LabelRack:          "rack-7",
		LabelCoolingZone:   "zone-a",
		LabelCluster:       "labels",
		LabelPool:          "vm",
	}
	if !reflect.DeepEqual(m.Node.Labels, wantLabels) {
		t.Errorf("labels = %v\nwant %v", m.Node.Labels, wantLabels)
	}
	wantAnnotations := map[string]string{
		AnnotationProfile:  "vm-no-powercap",
		"example.com/note": "defaults",
		"example.com/who":  "pool",
		KWOKAnnotation:     KWOKValue,
	}
	if !reflect.DeepEqual(m.Node.Annotations, wantAnnotations) {
		t.Errorf("annotations = %v\nwant %v", m.Node.Annotations, wantAnnotations)
	}
	if m.Rack != "rack-7" || m.CoolingZone != "zone-a" || m.ZoneOffsetC != 2.5 {
		t.Errorf("rack %q zone %q offset %g, want rack-7 zone-a 2.5", m.Rack, m.CoolingZone, m.ZoneOffsetC)
	}
	arm := testMember(t, members, "arm-0")
	if _, ok := arm.Node.Labels[LabelRack]; ok || arm.Rack != "" || arm.CoolingZone != "" || arm.ZoneOffsetC != 0 {
		t.Errorf("arm-0 has no rack or zone, but labels %v, member %+v", arm.Node.Labels, arm)
	}
	if arm.Node.Labels["example.com/tier"] != "defaults" {
		t.Errorf("arm-0 tier = %q, want the defaults' value", arm.Node.Labels["example.com/tier"])
	}
}

// CS05: a mig-mixed override replaces nvidia.com/gpu with MIG devices in
// that pool's allocatable only; the next pool of the same profile keeps
// whole GPUs. The source is called once per pool.
func TestCS05MIGOverrideStaysInItsPool(t *testing.T) {
	t.Parallel()
	calls := 0
	src := func() (map[string]*hwemu.Profile, error) {
		calls++
		return hwemu.BuiltinProfiles()
	}
	members := testExpand(t, filepath.Join("testdata", "mig.yaml"), src)
	if calls != 2 {
		t.Errorf("the source was called %d times for 2 pools", calls)
	}
	mig := testMember(t, members, "nvl-mig-0").Node.Status.Allocatable
	if q, ok := mig["nvidia.com/gpu"]; ok {
		t.Errorf("nvl-mig-0 allocatable nvidia.com/gpu = %s, want none", q.String())
	}
	if q := mig["nvidia.com/mig-1g.12gb"]; q.Value() != 56 {
		t.Errorf("nvl-mig-0 allocatable nvidia.com/mig-1g.12gb = %s, want 56 (8 GPUs x 7)", q.String())
	}
	whole := testMember(t, members, "nvl-0")
	if q := whole.Node.Status.Allocatable["nvidia.com/gpu"]; q.Value() != 8 {
		t.Errorf("nvl-0 allocatable nvidia.com/gpu = %s, want 8", q.String())
	}
	if _, ok := whole.Node.Status.Allocatable["nvidia.com/mig-1g.12gb"]; ok || whole.Profile.GPUs.Exposure.V != "whole" {
		t.Errorf("nvl-0 got the MIG override: exposure %q, allocatable %v", whole.Profile.GPUs.Exposure.V, whole.Node.Status.Allocatable)
	}
}

// CS06: a persistenceMode override reaches Member.Profile with the
// override's provenance, and the profile still validates; another pool of
// the same GPU family keeps the profile's value. An override other than
// enabled or disabled, or one on a profile whose GPUs are not NVIDIA's,
// fails and names the pool. An override the profile does not accept fails
// with the profile's own Validate error.
func TestCS06PersistenceOverrideReachesTheProfile(t *testing.T) {
	t.Parallel()
	members := testExpand(t, testSmokeSpec, Profiles(""))
	m := testMember(t, members, "sxm-nopm-0")
	want := hwemu.Fact[string]{V: hwemu.PersistenceDisabled, Src: "assumed:cluster spec smoke pool sxm-nopm override"}
	if m.Profile.GPUs.PersistenceMode != want {
		t.Errorf("sxm-nopm-0 persistenceMode = %+v, want %+v", m.Profile.GPUs.PersistenceMode, want)
	}
	if err := m.Profile.Validate(); err != nil {
		t.Errorf("sxm-nopm-0 profile: %v", err)
	}
	if got := testMember(t, members, "nvl-0").Profile.GPUs.PersistenceMode.V; got == hwemu.PersistenceDisabled {
		t.Errorf("nvl-0 persistenceMode = %q: the override leaked", got)
	}

	spec := func(pool, profile, gpus string) *Spec {
		return testParse(t, fmt.Sprintf(`apiVersion: sim.joulie.io/v1alpha1
kind: ClusterSpec
metadata: {name: cs06}
spec:
  pools:
  - {name: %s, profile: %s, count: 1, overrides: {gpus: %s}}
`, pool, profile, gpus))
	}
	for _, c := range []struct{ pool, profile, mode, want string }{
		{"mi300x", "amd-instinct-mi300x-8gpu", hwemu.PersistenceDisabled, `pool mi300x: overrides.gpus.persistenceMode: profile amd-instinct-mi300x-8gpu has amd GPUs`},
		{"mi300x", "amd-instinct-mi300x-8gpu", hwemu.PersistenceEnabled, `pool mi300x: overrides.gpus.persistenceMode: profile amd-instinct-mi300x-8gpu has amd GPUs`},
		{"nvl", "nvidia-h100-nvl-8gpu", "bogus", `pool nvl: overrides.gpus.persistenceMode "bogus" is not enabled or disabled`},
		{"nvl", "nvidia-h100-nvl-8gpu", "Disabled", `pool nvl: overrides.gpus.persistenceMode "Disabled" is not enabled or disabled`},
	} {
		_, err := spec(c.pool, c.profile, "{persistenceMode: "+c.mode+"}").Expand(Profiles(""))
		testWantErr(t, c.profile+" persistenceMode "+c.mode, err, c.want)
	}
	for _, mode := range []string{hwemu.PersistenceEnabled, hwemu.PersistenceDisabled} {
		members, err := spec("nvl", "nvidia-h100-nvl-8gpu", "{persistenceMode: "+mode+"}").Expand(Profiles(""))
		if err != nil {
			t.Errorf("nvl persistenceMode %s: %v", mode, err)
		} else if got := members[0].Profile.GPUs.PersistenceMode.V; got != mode {
			t.Errorf("nvl persistenceMode = %q, want %q", got, mode)
		}
	}

	// exposure mig-single without migProfile does not validate: Expand
	// returns the profile's error, prefixed with the pool.
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	p := profiles["nvidia-h100-nvl-8gpu"]
	p.GPUs.Exposure = hwemu.Fact[string]{V: "mig-single", Src: "assumed:cluster spec cs06 pool nvl override"}
	invalid := p.Validate()
	if invalid == nil {
		t.Fatal("nvidia-h100-nvl-8gpu with exposure mig-single and no migProfile validates")
	}
	_, err = spec("nvl", "nvidia-h100-nvl-8gpu", "{exposure: mig-single}").Expand(Profiles(""))
	testWantErr(t, "exposure mig-single", err, "pool nvl: profile nvidia-h100-nvl-8gpu with the pool's overrides: "+invalid.Error())
}

// CS07: a local profile loads next to the builtins, whatever characters the
// directory's path has; a local profile with a builtin's name fails, naming
// its file; an empty directory adds nothing; a missing directory, or a file
// in its place, is an error.
func TestCS07LocalProfiles(t *testing.T) {
	t.Parallel()
	members := testExpand(t, filepath.Join("testdata", "local.yaml"), Profiles(testLocalProfiles(t, "lab-vm-4cpu")))
	if len(members) != 3 {
		t.Fatalf("%d members, want 3", len(members))
	}
	lab := testMember(t, members, "lab-0")
	if lab.Profile.Name != "lab-vm-4cpu" || lab.Node.Annotations[AnnotationProfile] != "lab-vm-4cpu" {
		t.Errorf("lab-0 profile %s, annotation %s", lab.Profile.Name, lab.Node.Annotations[AnnotationProfile])
	}
	if q := lab.Node.Status.Capacity[corev1.ResourceCPU]; q.Value() != 4 {
		t.Errorf("lab-0 capacity cpu = %s, want 4", q.String())
	}

	// Glob metacharacters in the path are plain characters of a name.
	// "lab[1]" as a pattern matches lab1, not lab[1], and "lab[" is not a
	// pattern at all.
	src := testLocalProfiles(t, "lab-vm-4cpu")
	for _, name := range []string{"lab[1]", "lab[", "lab*?", `lab\x`} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(src, "lab-vm-4cpu.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "lab-vm-4cpu.yaml"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		profiles, err := Profiles(dir)()
		if err != nil {
			t.Errorf("directory %s: %v", name, err)
		} else if profiles["lab-vm-4cpu"] == nil {
			t.Errorf("directory %s: lab-vm-4cpu did not load", name)
		}
	}

	dir := testLocalProfiles(t, "vm-no-powercap")
	_, err := Profiles(dir)()
	testWantErr(t, "builtin name", err, filepath.Join(dir, "vm-no-powercap.yaml"), "builtin")
	s, err := LoadFile(filepath.Join("testdata", "local.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Expand(Profiles(dir))
	testWantErr(t, "builtin name (Expand)", err, "pool lab:", "builtin")

	empty, err := Profiles(t.TempDir())()
	if err != nil {
		t.Errorf("empty directory: %v", err)
	}
	builtins, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != len(builtins) {
		t.Errorf("empty directory gives %d profiles, want the %d builtins", len(empty), len(builtins))
	}
	_, err = Profiles(filepath.Join(dir, "missing"))()
	testWantErr(t, "missing directory", err, "local profiles: ")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing directory: error %v is not fs.ErrNotExist", err)
	}
	file := filepath.Join(dir, "vm-no-powercap.yaml")
	_, err = Profiles(file)()
	testWantErr(t, "file as directory", err, "local profiles: "+file+" is not a directory")
}

// CS07b: a local profile that extends a builtin one, as every hwcalib fit
// writes it, expands like the builtin with its own facts: Profiles loads the
// local directory by the rules of hwemu.LoadProfileSet.
func TestCS07bLocalProfileExtendsABuiltin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const local = `schemaVersion: 1
name: lab-xeon
extends: intel-xeon-2s-rapl
description: Test fixture, the builtin Intel host with a local idle figure.
physics:
  cpu:
    idlePkgW: {v: 55, src: "assumed:clusterspec test fixture"}
`
	if err := os.WriteFile(filepath.Join(dir, "lab-xeon.yaml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	members, err := testOnePool(t, "lab-xeon").Expand(Profiles(dir))
	if err != nil {
		t.Fatalf("a local profile extending intel-xeon-2s-rapl: %v", err)
	}
	builtins, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	parent := builtins["intel-xeon-2s-rapl"]
	m := testMember(t, members, "lab-0")
	if m.Profile.Name != "lab-xeon" || m.Node.Annotations[AnnotationProfile] != "lab-xeon" {
		t.Errorf("lab-0 profile %s, annotation %s, want lab-xeon", m.Profile.Name, m.Node.Annotations[AnnotationProfile])
	}
	if m.Profile.Physics.CPU.IdlePkgW.V != 55 {
		t.Errorf("lab-0 idlePkgW %v, want the local 55", m.Profile.Physics.CPU.IdlePkgW.V)
	}
	if !reflect.DeepEqual(m.Profile.CPU, parent.CPU) || !reflect.DeepEqual(m.Profile.Powercap, parent.Powercap) {
		t.Error("lab-0: want the CPU and powercap facts of intel-xeon-2s-rapl, which lab-xeon does not write")
	}
}

// CS08: smoke.yaml validates and expands to 9 members, 8 of them bare
// metal; intel-tainted-0, and only it, carries the example.com/dedicated
// taint, and intel-arm64-0, and only it, is arm64, as its label and node
// info say.
func TestCS08SmokeSpecExpands(t *testing.T) {
	t.Parallel()
	s, err := LoadFile(testSmokeSpec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	members, err := s.Expand(Profiles(""))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(members) != 9 {
		t.Fatalf("%d members, want 9", len(members))
	}
	bare := 0
	for _, m := range members {
		if !m.Profile.CPU.Hypervisor {
			bare++
		}
		tainted := false
		for _, taint := range m.Node.Spec.Taints {
			if taint.Key == "example.com/dedicated" {
				tainted = taint.Value == "batch" && taint.Effect == corev1.TaintEffectNoSchedule
				if !tainted {
					t.Errorf("%s: taint %s", m.Name, taint.ToString())
				}
			}
		}
		if tainted != (m.Name == "intel-tainted-0") {
			t.Errorf("%s: example.com/dedicated=batch:NoSchedule taint %v", m.Name, tainted)
		}
		wantArch := "amd64"
		if m.Name == "intel-arm64-0" {
			wantArch = "arm64"
		}
		if arch := m.Node.Labels[corev1.LabelArchStable]; arch != wantArch || m.Node.Status.NodeInfo.Architecture != wantArch {
			t.Errorf("%s: architecture label %q, node info %q, want %s", m.Name, arch, m.Node.Status.NodeInfo.Architecture, wantArch)
		}
	}
	if bare != 8 {
		t.Errorf("%d bare-metal members, want 8", bare)
	}
}

// CS10 (expansion half): a pool's baseLoad overrides the defaults field by
// field, and no two members share a value.
func TestCS10PoolBaseLoadOverridesDefaultsPerField(t *testing.T) {
	t.Parallel()
	members := testExpand(t, filepath.Join("testdata", "baseload.yaml"), Profiles(""))
	f := func(v *float64) float64 {
		if v == nil {
			return -1
		}
		return *v
	}
	cases := []struct {
		node               string
		cpu, gpu, mem      float64
		cpuClass, gpuClass string
	}{
		{"plain-0", 0.8, 0.9, 0.2, "cpu.compute_bound", "gpu.compute_bound"},
		{"tuned-0", 0.3, 0.9, 0.2, "cpu.compute_bound", "gpu.memory_bound"},
	}
	for _, c := range cases {
		l := testMember(t, members, c.node).BaseLoad
		if f(l.CPUUtil) != c.cpu || f(l.GPUUtil) != c.gpu || f(l.MemoryIntensity) != c.mem || l.CPUClass != c.cpuClass || l.GPUClass != c.gpuClass {
			t.Errorf("%s baseLoad = cpu %g gpu %g mem %g %s %s, want %g %g %g %s %s", c.node,
				f(l.CPUUtil), f(l.GPUUtil), f(l.MemoryIntensity), l.CPUClass, l.GPUClass, c.cpu, c.gpu, c.mem, c.cpuClass, c.gpuClass)
		}
	}
	*testMember(t, members, "plain-0").BaseLoad.CPUUtil = 0
	if got := *testMember(t, members, "plain-1").BaseLoad.CPUUtil; got != 0.8 {
		t.Errorf("changing plain-0's cpuUtil changed plain-1's to %g", got)
	}

	none := testExpand(t, filepath.Join("testdata", "names.yaml"), Profiles(""))
	if l := none[0].BaseLoad; l.CPUUtil != nil || l.GPUUtil != nil || l.MemoryIntensity != nil || l.CPUClass != "" || l.GPUClass != "" {
		t.Errorf("no baseLoad anywhere, but %s has %+v", none[0].Name, l)
	}
}

// CS11: status.nodeInfo.architecture equals the kubernetes.io/arch label,
// including a pool that overrides it.
func TestCS11ArchitectureFollowsTheArchLabel(t *testing.T) {
	t.Parallel()
	for _, path := range []string{testSmokeSpec, filepath.Join("testdata", "labels.yaml")} {
		for _, m := range testExpand(t, path, Profiles("")) {
			arch := m.Node.Labels[corev1.LabelArchStable]
			if arch == "" || m.Node.Status.NodeInfo.Architecture != arch {
				t.Errorf("%s: nodeInfo.architecture %q, label %q", m.Name, m.Node.Status.NodeInfo.Architecture, arch)
			}
		}
	}
	m := testMember(t, testExpand(t, filepath.Join("testdata", "labels.yaml"), Profiles("")), "arm-0")
	if m.Node.Status.NodeInfo.Architecture != "arm64" {
		t.Errorf("arm-0 nodeInfo.architecture = %q, want arm64", m.Node.Status.NodeInfo.Architecture)
	}
}

// CS12: no Node carries a joulie.io/hw.* label; with kwok, the default,
// every Node has the KWOK annotation and taint, and with kwok false none
// has. A local profile is checked too: one that carries a joulie.io/hw.*,
// rack, cooling-zone or sim.joulie.io/ label fails, naming the pool, the
// profile's label group and each key; its kubernetes.io/hostname, which a
// corpus capture copies, gives way to the Node name.
func TestCS12KWOKMarksAndNoHwLabels(t *testing.T) {
	t.Parallel()
	kwokTaint := corev1.Taint{Key: KWOKTaintKey, Value: KWOKValue, Effect: corev1.TaintEffectNoSchedule}
	for _, path := range []string{testSmokeSpec, filepath.Join("testdata", "labels.yaml"), filepath.Join("testdata", "no-kwok.yaml")} {
		kwok := !strings.HasSuffix(path, "no-kwok.yaml")
		for _, m := range testExpand(t, path, Profiles("")) {
			for k := range m.Node.Labels {
				if strings.HasPrefix(k, "joulie.io/hw.") {
					t.Errorf("%s: label %s", m.Name, k)
				}
			}
			ann, hasAnn := m.Node.Annotations[KWOKAnnotation]
			taints := 0
			for _, taint := range m.Node.Spec.Taints {
				if taint.Key == KWOKTaintKey {
					taints++
					if !reflect.DeepEqual(taint, kwokTaint) {
						t.Errorf("%s: KWOK taint %s, want %s", m.Name, taint.ToString(), kwokTaint.ToString())
					}
				}
			}
			if kwok != (hasAnn && ann == KWOKValue) || (kwok && taints != 1) || (!kwok && taints != 0) {
				t.Errorf("%s (kwok %v): annotation %q, %d KWOK taints", m.Name, kwok, ann, taints)
			}
		}
	}

	labels := "kubernetes.io/os: linux}"
	leak := testLocalProfiles(t, "lab-leak", labels, "kubernetes.io/os: linux, joulie.io/hw.cpu-model: fake, joulie.io/rack: r9, joulie.io/cooling-zone: z9, sim.joulie.io/extra: x}")
	_, err := testOnePool(t, "lab-leak").Expand(Profiles(leak))
	at := "pool lab: profile lab-leak node.labels.cpu: label "
	testWantErr(t, "local profile labels", err,
		at+"joulie.io/hw.cpu-model: Joulie reads joulie.io/hw.*",
		at+"joulie.io/rack is set from the pool's rack",
		at+"joulie.io/cooling-zone is set from the pool's coolingZone",
		at+"sim.joulie.io/extra is set by the spec itself")

	host := testLocalProfiles(t, "lab-host", labels, "kubernetes.io/os: linux, kubernetes.io/hostname: captured-host, joulie.io/managed: \"true\"}")
	members, err := testOnePool(t, "lab-host").Expand(Profiles(host))
	if err != nil {
		t.Fatalf("a profile with a captured hostname: %v", err)
	}
	if l := members[0].Node.Labels; l[corev1.LabelHostname] != "lab-0" || l["joulie.io/managed"] != "true" {
		t.Errorf("lab-0 labels %v, want hostname lab-0 and the profile's joulie.io/managed", l)
	}
}

// CS13 (expansion half): a Node's taints are the profile's, then the
// pool's, then the KWOK taint; a pool taint with the key and effect of a
// profile taint fails, and so does a local profile's taint on the KWOK key,
// with kwok or without: defaults.kwok owns that key.
func TestCS13TaintOrderIsProfilePoolKWOK(t *testing.T) {
	t.Parallel()
	profileTaint := corev1.Taint{Key: "example.com/profile", Value: "a", Effect: corev1.TaintEffectNoExecute}
	src := func() (map[string]*hwemu.Profile, error) {
		profiles, err := hwemu.BuiltinProfiles()
		if err != nil {
			return nil, err
		}
		vm := profiles["vm-no-powercap"]
		vm.Node.Taints = append(vm.Node.Taints, profileTaint)
		return profiles, nil
	}
	path := filepath.Join("testdata", "taints.yaml")
	m := testExpand(t, path, src)[0]
	var got []string
	for _, taint := range m.Node.Spec.Taints {
		got = append(got, taint.ToString())
	}
	want := []string{"example.com/profile=a:NoExecute", "example.com/pool=b:NoSchedule", "example.com/second=c:PreferNoSchedule", "kwok.x-k8s.io/node=fake:NoSchedule"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("taints = %v, want %v", got, want)
	}

	s, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Spec.Pools[0].Taints[1] = corev1.Taint{Key: "example.com/profile", Value: "other", Effect: corev1.TaintEffectNoExecute}
	_, err = s.Expand(src)
	testWantErr(t, "pool taint over a profile taint", err, "pool tainted: taints[1] repeats the key and effect example.com/profile:NoExecute")

	dir := testLocalProfiles(t, "lab-kwok", "taints: []", "taints: [{key: kwok.x-k8s.io/node, value: fake, effect: NoSchedule}]")
	for _, kwok := range []bool{true, false} {
		s := testOnePool(t, "lab-kwok")
		s.Spec.Defaults.Kwok = &kwok
		_, err := s.Expand(Profiles(dir))
		testWantErr(t, fmt.Sprintf("profile KWOK taint (kwok %v)", kwok), err, "pool lab: profile lab-kwok node.taints[0] kwok.x-k8s.io/node is added by defaults.kwok")
	}
}

// Expand reports the problems of every pool, not only the first, and a
// gpus override on a profile without GPUs.
func TestExpandReportsEveryPool(t *testing.T) {
	t.Parallel()
	s := testParse(t, `apiVersion: sim.joulie.io/v1alpha1
kind: ClusterSpec
metadata: {name: many}
spec:
  pools:
  - {name: a, profile: no-such-profile, count: 1}
  - {name: b, profile: vm-no-powercap, count: 1, overrides: {gpus: {persistenceMode: disabled}}}
  - {name: c, profile: amd-instinct-mi300x-8gpu, count: 1, overrides: {gpus: {exposure: mig-mixed}}}
  - {name: d, profile: vm-no-powercap, count: 1}
`)
	_, err := s.Expand(Profiles(""))
	testWantErr(t, "three bad pools", err,
		`pool a: profile "no-such-profile" is not known`,
		"pool b: overrides.gpus: profile vm-no-powercap has no GPUs",
		"pool c: profile amd-instinct-mi300x-8gpu with the pool's overrides:", `exposure "mig-mixed" is not supported for amd`)
	if err != nil && strings.Contains(err.Error(), "pool d") {
		t.Errorf("pool d is fine, but: %v", err)
	}
}
