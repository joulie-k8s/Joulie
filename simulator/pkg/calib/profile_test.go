package calib

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// CB05: the fitted profile loads through hwemu.LoadProfileSet as a local
// profile beside the builtins: it extends its base, cites the sweep as a
// measured source on every fitted fact, and inherits the rest.
func TestCB05OutputLoadsThroughLoadProfileSet(t *testing.T) {
	t.Parallel()
	sw := epycPerformanceSweep(t)
	// The base is the builtin profile, which runs its catalog curve: the fit
	// selects the analytic model its facts describe.
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	r, err := Fit(sw.data(t), profiles[sw.profile.Name], Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := LocalProfile(r, "epyc-calibrated", "hwcalib fit of a test sweep", "18 segments")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path, err := WriteProfile(dir, "epyc-calibrated", b)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "epyc-calibrated.yaml") {
		t.Errorf("wrote %s", path)
	}
	profiles, local, err := hwemu.LoadProfileSet(dir)
	if err != nil {
		t.Fatalf("load:\n%s\n%v", b, err)
	}
	if len(local) != 1 || local[0] != "epyc-calibrated" {
		t.Errorf("local profiles %v", local)
	}
	p := profiles["epyc-calibrated"]
	if src, ok := p.Sources[SourceID]; !ok || src.Kind != "measured" || src.Ref == "" {
		t.Errorf("sources.%s = %+v, want a measured source", SourceID, src)
	}
	cpu := p.Physics.CPU
	for _, f := range []struct {
		path string
		got  float64
		src  string
		want float64
	}{
		{pathCPUIdle, cpu.IdlePkgW.V, cpu.IdlePkgW.Src, 40},
		{pathCPUMax, cpu.MaxPkgW.V, cpu.MaxPkgW.Src, 360},
		{pathCPUAlpha, cpu.AlphaUtil.V, cpu.AlphaUtil.Src, 1.15},
		{pathCPUBeta, cpu.BetaFreq.V, cpu.BetaFreq.Src, 1.35},
	} {
		fp := param(t, r, f.path)
		if f.got != f.want || f.src != "measured:"+SourceID+"#"+fp.Fit {
			t.Errorf("%s = %v (%s), want %v (measured:%s#%s)", f.path, f.got, f.src, f.want, SourceID, fp.Fit)
		}
	}
	if p.Physics.DRAM.WattsPerSocket.Src == "measured:"+SourceID+"#"+fitDRAM {
		t.Error("DRAM cites the sweep, which did not measure it")
	}
	base := sw.profile
	if p.CPU.Sockets.V != base.CPU.Sockets.V || p.CPUFreq == nil || p.CPUFreq.Driver.V != base.CPUFreq.Driver.V || p.Physics.CPU.Model != "analytic" {
		t.Error("the profile did not inherit the base's topology, cpufreq and model")
	}
	// The written profile renders, so the simulator can run it.
	if _, err := hwemu.Render(p, filepath.Join(t.TempDir(), "node"), hwemu.RenderOptions{NodeName: fixtureNode, Seed: 1}); err != nil {
		t.Errorf("render the fitted profile: %v", err)
	}
	if _, err := WriteProfile(dir, "epyc-calibrated", b); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("a second write: err %v, want a refusal to replace the file", err)
	}
	if _, err := LocalProfile(r, "Bad_Name", "ref", ""); err == nil {
		t.Error("a name the loader rejects was accepted")
	}
}

// repoRoot is the repository this test file is in.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// CB06: a measured profile is never written into the repository: not into
// its root, not into a directory still to be created there, not through a
// link that resolves into it. There is no override.
func TestCB06RepositoryPathsAreRefused(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s is not the repository root: %v", root, err)
	}
	link := filepath.Join(t.TempDir(), "into-repo")
	if err := os.Symlink(filepath.Join(root, "simulator"), link); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "simulator", "pkg", "calib", "testdata", "cb06-never-created", "profiles")
	for _, dir := range []string{
		root,
		filepath.Join(root, "simulator", "pkg", "hwemu", "profiles"),
		missing,
		filepath.Join(link, "profiles"),
		link,
	} {
		got, in, err := InRepository(dir)
		if err != nil || !in {
			t.Errorf("InRepository(%s) = %q, %v, %v; want inside", dir, got, in, err)
		}
		path, err := WriteProfile(dir, "cb06", []byte("name: cb06\n"))
		if err == nil {
			// A regression must not leave its file in the checkout:
			// remove it and the directory it may have created, which
			// os.Remove leaves alone unless it is empty.
			os.Remove(path)
			os.Remove(filepath.Dir(path))
		}
		if err == nil || !strings.Contains(err.Error(), "inside the repository") {
			t.Errorf("WriteProfile(%s): err %v, want a refusal", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Errorf("a refused write created %s", filepath.Dir(missing))
	}
	outside := filepath.Join(t.TempDir(), "profiles")
	if _, in, err := InRepository(outside); err != nil || in {
		t.Errorf("InRepository(%s) = %v, %v; want outside", outside, in, err)
	}
	if _, err := WriteProfile(outside, "cb06", []byte("name: cb06\n")); err != nil {
		t.Errorf("WriteProfile outside the repository: %v", err)
	}
}
