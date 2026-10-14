package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/simcluster"
)

var (
	testRepo      = filepath.Join("..", "..", "..")
	testSmokeSpec = filepath.Join(testRepo, "tests", "simcluster", "specs", "smoke.yaml")
	testTestdata  = filepath.Join("..", "..", "pkg", "simcluster", "testdata")
	testRender    = filepath.Join(testTestdata, "daemonset-render.yaml")
)

const testRoot = "/var/lib/joulie-hwemu"

// testRun runs the command and returns its exit status, stdout and stderr.
func testRun(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func testSmoke(t *testing.T) (*clusterspec.Spec, []clusterspec.Member) {
	t.Helper()
	s, err := clusterspec.LoadFile(testSmokeSpec)
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.Expand(clusterspec.Profiles(""))
	if err != nil {
		t.Fatal(err)
	}
	return s, members
}

// CLI01: each subcommand prints what the library computes.
func TestCLI01SubcommandsEqualTheLibrary(t *testing.T) {
	s, members := testSmoke(t)

	t.Run("validate", func(t *testing.T) {
		code, out, errOut := testRun("validate", "--spec", testSmokeSpec)
		if code != 0 || !strings.Contains(out, "harness section of cluster spec smoke is valid: 8 Nodes, 7 with measured CPU packages, tool delivery set for pools [nvl: mounted, sxm-nopm: mounted, mi300x: mounted], the install's for the others") {
			t.Errorf("validate: exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})

	t.Run("agents", func(t *testing.T) {
		code, out, errOut := testRun("agents", "--spec", testSmokeSpec, "--daemonset", testRender, "--image-arch", "amd64", "--root", testRoot)
		if code != 0 {
			t.Fatalf("agents: exit %d, stderr %q", code, errOut)
		}
		render, err := os.ReadFile(testRender)
		if err != nil {
			t.Fatal(err)
		}
		p, err := simcluster.PlacementFromRender(render, "amd64")
		if err != nil {
			t.Fatal(err)
		}
		h, err := simcluster.SpecHarness(s)
		if err != nil {
			t.Fatal(err)
		}
		agents, excluded, err := simcluster.Agents(members, p, h, testRoot)
		if err != nil {
			t.Fatal(err)
		}
		want, err := simcluster.AgentsValues(s.Metadata.Name, agents)
		if err != nil {
			t.Fatal(err)
		}
		if out != string(want) {
			t.Errorf("agents stdout:\n%s\nwant:\n%s", out, want)
		}
		golden, err := os.ReadFile(filepath.Join(testTestdata, "agents-smoke.golden.yaml"))
		if err != nil || out != string(golden) {
			t.Errorf("agents stdout differs from the G04 golden (%v)", err)
		}
		var lines []string
		for _, e := range excluded {
			lines = append(lines, e.Node+": "+e.Reason+"\n")
		}
		if errOut != strings.Join(lines, "") || len(lines) == 0 {
			t.Errorf("agents stderr %q, want %q", errOut, strings.Join(lines, ""))
		}
	})

	t.Run("telemetry", func(t *testing.T) {
		timing, err := simcluster.ReadTiming(testRepo, simcluster.TimingDev)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range simcluster.Modes() {
			parts, err := simcluster.Telemetry(m, simcluster.ScopeComponents, timing)
			if err != nil {
				t.Fatal(err)
			}
			parts.Wait.Nodes = simcluster.WaitNodes(members)
			for _, part := range simcluster.Parts() {
				want, wantErr := parts.Part(part)
				code, out, errOut := testRun("telemetry", "--mode", string(m), "--part", part, "--spec", testSmokeSpec)
				switch {
				case wantErr != nil:
					if code != 1 || !strings.Contains(errOut, wantErr.Error()) {
						t.Errorf("mode %s part %s: exit %d, stderr %q; want 1 and %q", m, part, code, errOut, wantErr)
					}
				case code != 0 || out != string(want):
					t.Errorf("mode %s part %s: exit %d, stderr %q, stdout:\n%s\nwant:\n%s", m, part, code, errOut, out, want)
				}
			}
		}
		// The defaults are mode otel, scope components and timing dev.
		parts, err := simcluster.Telemetry(simcluster.ModeOTel, simcluster.ScopeComponents, timing)
		if err != nil {
			t.Fatal(err)
		}
		if code, out, _ := testRun("telemetry", "--part", "joulie-values"); code != 0 || out != string(parts.JoulieValues) {
			t.Errorf("default telemetry: exit %d, stdout %q", code, out)
		}
		if code, _, errOut := testRun("telemetry", "--part", "checks", "--mode", "kepler", "--scope", "host"); code != 1 || !strings.Contains(errOut, "needs an otel mode") {
			t.Errorf("kepler with scope host: exit %d, stderr %q", code, errOut)
		}
		if code, _, errOut := testRun("telemetry", "--part", "checks", "--timing", "nightly"); code != 1 || !strings.Contains(errOut, "nightly.env") {
			t.Errorf("an unknown timing: exit %d, stderr %q", code, errOut)
		}
	})

	t.Run("scale-spec", func(t *testing.T) {
		want, err := simcluster.ScaleSpec(3, "amd-epyc-2s-hsmp")
		if err != nil {
			t.Fatal(err)
		}
		if code, out, errOut := testRun("scale-spec", "--nodes", "3", "--profile", "amd-epyc-2s-hsmp"); code != 0 || out != string(want) {
			t.Errorf("scale-spec: exit %d, stderr %q, stdout %q", code, errOut, out)
		}
		want, err = simcluster.ScaleSpec(2, "intel-xeon-2s-rapl")
		if err != nil {
			t.Fatal(err)
		}
		if code, out, _ := testRun("scale-spec", "--nodes", "2"); code != 0 || out != string(want) {
			t.Errorf("scale-spec without --profile: exit %d, stdout %q", code, out)
		}
		if code, _, errOut := testRun("scale-spec", "--nodes", "2", "--profile", "no-such-profile"); code != 1 || !strings.Contains(errOut, `profile "no-such-profile" is not known`) {
			t.Errorf("scale-spec of an unknown profile: exit %d, stderr %q", code, errOut)
		}
	})

	t.Run("failures", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.yaml")
		if err := os.WriteFile(bad, []byte("apiVersion: sim.joulie.io/v1alpha1\nkind: ClusterSpec\nmetadata: {name: bad}\nspec:\n  pools: [{name: a, profile: vm-no-powercap, count: 1}]\n  harness: {pools: {b: {}}}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"validate", "--spec", bad},
			{"validate", "--spec", filepath.Join(t.TempDir(), "missing.yaml")},
			{"agents", "--spec", testSmokeSpec, "--daemonset", testSmokeSpec, "--image-arch", "amd64", "--root", testRoot},
			{"agents", "--spec", testSmokeSpec, "--daemonset", testRender, "--image-arch", "amd64", "--root", "relative"},
		} {
			if code, _, errOut := testRun(args...); code != 1 || errOut == "" {
				t.Errorf("%v: exit %d, stderr %q; want 1 with a message", args, code, errOut)
			}
		}
	})
}

// CLI02: unknown commands and flags, missing or bad flag values and
// positional arguments exit 2 with the usage.
func TestCLI02UsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"render-nodes"},
		{"validate", "--spec", testSmokeSpec, "--bogus"},
		{"validate", "-profiles", "x", "--spec", testSmokeSpec},
		{"validate"},
		{"validate", "--spec", testSmokeSpec, "extra"},
		{"agents", "--spec", testSmokeSpec, "--daemonset", testRender, "--image-arch", "amd64"},
		{"agents", "--spec", testSmokeSpec, "--daemonset", testRender, "--root", testRoot},
		{"agents", "--spec", testSmokeSpec, "--daemonset", testRender, "--image-arch", "amd64", "--root", testRoot, "--mode", "otel"},
		{"telemetry"},
		{"telemetry", "--part", "nodes"},
		{"telemetry", "--part", "checks", "--mode", "kepler-0.7"},
		{"telemetry", "--part", "checks", "--scope", "node"},
		{"telemetry", "--part", "wait"},
		{"telemetry", "--part", "checks", "--nodes", "3"},
		{"scale-spec"},
		{"scale-spec", "--nodes", "0"},
		{"scale-spec", "--nodes", "two"},
		{"scale-spec", "--nodes", "2", "--spec", testSmokeSpec},
	} {
		code, out, errOut := testRun(args...)
		if code != 2 || !strings.Contains(errOut, "usage:") || out != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want 2 with the usage on stderr", args, code, out, errOut)
		}
	}
	for _, args := range [][]string{{"help"}, {"--help"}, {"telemetry", "-h"}} {
		if code, _, _ := testRun(args...); code != 0 {
			t.Errorf("%v: exit %d, want 0", args, code)
		}
	}
}

// CLI03: --profiles-dir defaults to SIM_PROFILES_DIR, as clusterspec's
// -profiles-dir does, and an empty --profiles-dir means the builtins only.
func TestCLI03ProfilesDirDefaultsToTheSimulatorsVariable(t *testing.T) {
	spec := filepath.Join("..", "..", "pkg", "clusterspec", "testdata", "local.yaml")
	b, err := os.ReadFile(filepath.Join("..", "..", "pkg", "clusterspec", "testdata", "lab-vm-4cpu.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lab-vm-4cpu.yaml"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envProfilesDir, dir)
	if code, _, errOut := testRun("validate", "--spec", spec); code != 0 {
		t.Errorf("local.yaml with %s set: exit %d, stderr %q", envProfilesDir, code, errOut)
	}
	if code, _, errOut := testRun("validate", "--spec", spec, "--profiles-dir", ""); code != 1 || !strings.Contains(errOut, `profile "lab-vm-4cpu" is not known`) {
		t.Errorf("local.yaml with --profiles-dir \"\": exit %d, stderr %q; want 1, the profile unknown", code, errOut)
	}
}

// CLI04: a GPU pool the spec's harness section does not list gets the GPU
// tools only when the install delivers them: under a DaemonSet render with
// neither a runtime class nor an extra volume, the chart's default, its
// agent mounts no driver stub, so its GPU inventory fails as on a real
// node of such an install; with an extra volume it mounts the stub.
func TestCLI04ToolDeliveryFollowsTheInstall(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(spec, []byte(`apiVersion: sim.joulie.io/v1alpha1
kind: ClusterSpec
metadata: {name: gpu}
spec:
  defaults: {labels: {joulie.io/managed: "true"}}
  pools: [{name: nvl, profile: nvidia-h100-nvl-8gpu, count: 1}]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	render := func(extra string) string {
		path := filepath.Join(dir, "render"+fmtLen(extra)+".yaml")
		doc := `apiVersion: apps/v1
kind: DaemonSet
metadata: {name: joulie-agent}
spec:
  template:
    spec:
      nodeSelector: {joulie.io/managed: "true"}
      tolerations: [{key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}]
      volumes: [{name: host-sys, hostPath: {path: /sys}}` + extra + `]
`
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tc := range []struct {
		extra string
		stub  bool
	}{
		{"", false},
		{", {name: nvidia-driver, hostPath: {path: /usr/lib/nvidia}}", true},
	} {
		code, out, errOut := testRun("agents", "--spec", spec, "--daemonset", render(tc.extra), "--image-arch", "amd64", "--root", testRoot)
		if code != 0 {
			t.Fatalf("agents: exit %d, stderr %q", code, errOut)
		}
		if !strings.Contains(out, "name: nvl-0") || strings.Contains(out, simcluster.DriverStubVolume) != tc.stub {
			t.Errorf("extra volumes %q: the agent of nvl-0 mounts the driver stub: %v, want %v:\n%s", tc.extra, strings.Contains(out, simcluster.DriverStubVolume), tc.stub, out)
		}
	}
}

func fmtLen(s string) string {
	if s == "" {
		return "-default"
	}
	return "-extra"
}
