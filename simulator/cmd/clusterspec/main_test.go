package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
)

var (
	testSmokeSpec = filepath.Join("..", "..", "..", "tests", "simcluster", "specs", "smoke.yaml")
	testdata      = filepath.Join("..", "..", "pkg", "clusterspec", "testdata")
)

// testProfiles returns a new profiles directory holding the local profile
// lab-vm-4cpu, a copy of the clusterspec test fixture.
func testProfiles(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdata, "lab-vm-4cpu.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lab-vm-4cpu.yaml"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testRun runs the command and returns its exit status, stdout and stderr.
func testRun(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// testExpand expands the spec at path with the builtin profiles plus
// profilesDir.
func testExpand(t *testing.T, path, profilesDir string) []clusterspec.Member {
	t.Helper()
	s, err := clusterspec.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.Expand(clusterspec.Profiles(profilesDir))
	if err != nil {
		t.Fatal(err)
	}
	return members
}

// CS09: render-nodes prints a v1 List whose items decode into the Nodes
// Expand returns, status included, in the same order.
func TestCS09RenderNodesDecodesIntoExpandedNodes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ spec, profiles string }{
		{testSmokeSpec, ""},
		{filepath.Join(testdata, "local.yaml"), testProfiles(t)},
	} {
		args := []string{"render-nodes", "-spec", c.spec}
		if c.profiles != "" {
			args = append(args, "-profiles-dir", c.profiles)
		}
		code, stdout, stderr := testRun(args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, stderr)
		}
		var list struct {
			APIVersion string        `json:"apiVersion"`
			Kind       string        `json:"kind"`
			Items      []corev1.Node `json:"items"`
		}
		if err := yaml.UnmarshalStrict([]byte(stdout), &list); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if list.APIVersion != "v1" || list.Kind != "List" {
			t.Errorf("%v: apiVersion %q kind %q, want v1 List", args, list.APIVersion, list.Kind)
		}
		members := testExpand(t, c.spec, c.profiles)
		if len(list.Items) != len(members) {
			t.Fatalf("%v: %d items, want %d", args, len(list.Items), len(members))
		}
		for i, m := range members {
			got := list.Items[i]
			if !equality.Semantic.DeepEqual(&got, m.Node) {
				t.Errorf("%v: item %d (%s) differs from Expand's Node %s:\n%+v\n%+v", args, i, got.Name, m.Name, got, *m.Node)
			}
			if len(got.Status.Allocatable) == 0 || got.Status.NodeInfo.Architecture == "" {
				t.Errorf("%v: %s has no status: %+v", args, got.Name, got.Status)
			}
		}
	}
}

// validate exits 0 on a valid spec, 1 on an invalid one with every problem
// on stderr, and 2 on a usage error.
func TestValidateExitStatus(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := testRun("validate", "-spec", testSmokeSpec)
	if code != 0 || !strings.Contains(stdout, "cluster spec smoke is valid: 8 pools, 8 Nodes") {
		t.Errorf("smoke: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, _, stderr = testRun("validate", "-spec", filepath.Join(testdata, "invalid.yaml"))
	if code != 1 || !strings.Contains(stderr, `pool a: cooling zone "nowhere"`) || !strings.Contains(stderr, "pool b: baseLoad.cpuUtil 2") {
		t.Errorf("invalid.yaml: exit %d, stderr %q; want 1 and both problems", code, stderr)
	}
	code, _, stderr = testRun("validate", "-spec", filepath.Join(testdata, "local.yaml"))
	if code != 1 || !strings.Contains(stderr, `profile "lab-vm-4cpu" is not known`) {
		t.Errorf("local.yaml without -profiles-dir: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := testRun("validate", "-spec", filepath.Join(testdata, "local.yaml"), "-profiles-dir", testProfiles(t)); code != 0 {
		t.Errorf("local.yaml with -profiles-dir: exit %d, stderr %q", code, stderr)
	}
	if code, _, _ := testRun("validate", "-spec", filepath.Join(testdata, "missing.yaml")); code != 1 {
		t.Errorf("missing spec file: exit %d, want 1", code)
	}
	for _, args := range [][]string{
		nil,
		{"bogus"},
		{"validate"},
		{"validate", "-spec", testSmokeSpec, "extra"},
		{"list", "-bogus", "x"},
		{"validate", "-spec", testSmokeSpec, "-profiles", "x"},
	} {
		if code, _, stderr := testRun(args...); code != 2 || stderr == "" {
			t.Errorf("%v: exit %d, stderr %q; want 2 and a message", args, code, stderr)
		}
	}
}

// list prints name, pool, profile, rack and zone per Node, sorted by name,
// with "-" for an empty field.
func TestListPrintsOneLinePerNode(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := testRun("list", "-spec", testSmokeSpec)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		got = append(got, strings.Join(strings.Fields(line), " "))
	}
	want := []string{
		"epyc-0 epyc amd-epyc-2s-energy-only rack-1 zone-a",
		"hsmp-0 hsmp amd-epyc-2s-hsmp rack-1 zone-a",
		"intel-0 intel intel-xeon-2s-rapl rack-1 zone-a",
		"intel-tainted-0 intel-tainted intel-xeon-2s-rapl rack-1 zone-a",
		"mi300x-0 mi300x amd-instinct-mi300x-8gpu rack-2 zone-b",
		"nvl-0 nvl nvidia-h100-nvl-8gpu rack-2 zone-b",
		"sxm-nopm-0 sxm-nopm nvidia-h100-sxm-8gpu rack-2 zone-b",
		"vm-0 vm vm-no-powercap - -",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("list:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// -profiles-dir defaults to SIM_PROFILES_DIR, the directory the simulator
// reads, and an empty -profiles-dir means the builtins only.
func TestProfilesDirDefaultsToTheSimulatorsVariable(t *testing.T) {
	local := filepath.Join(testdata, "local.yaml")
	t.Setenv(envProfilesDir, testProfiles(t))
	if code, _, stderr := testRun("validate", "-spec", local); code != 0 {
		t.Errorf("local.yaml with %s set: exit %d, stderr %q", envProfilesDir, code, stderr)
	}
	if code, _, stderr := testRun("validate", "-spec", local, "-profiles-dir", ""); code != 1 || !strings.Contains(stderr, `profile "lab-vm-4cpu" is not known`) {
		t.Errorf("local.yaml with -profiles-dir \"\": exit %d, stderr %q; want 1, the profile unknown", code, stderr)
	}
}
