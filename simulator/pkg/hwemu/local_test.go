package hwemu

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// renderLocalDir writes files, file name to content, into a new directory
// and returns its path.
func renderLocalDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// renderLocalXeon is a local profile on top of the builtin Intel host: its
// own idle figure and nothing else.
const renderLocalXeon = `schemaVersion: 1
name: lab-xeon
extends: intel-xeon-2s-rapl
description: the builtin Intel host with a local idle figure
physics:
  cpu:
    idlePkgW: {v: 55, src: "assumed:local test override"}
`

// renderMeasuredMinimalProfile is renderMinimalProfile with a measured
// source described by ref and one fact citing it.
func renderMeasuredMinimalProfile(t *testing.T, ref string) string {
	t.Helper()
	body := strings.Replace(renderMinimalProfile, `  cap: {kind: corpus`, `  bench: {kind: measured, ref: "`+ref+`"}
  cap: {kind: corpus`, 1)
	body = strings.Replace(body, `maxPkgW: {v: 100, src: published:doc}`, `maxPkgW: {v: 100, src: "measured:bench#run 3"}`, 1)
	if !strings.Contains(body, "measured:bench") {
		t.Fatal("renderMinimalProfile no longer has the fact the measured variant replaces")
	}
	return body
}

// H38: a local profile extends a builtin one by the same merge rules the
// builtins use, and a local profile may extend another local one. The
// builtins come back as BuiltinProfiles returns them, files that are not
// *.yaml are ignored, the local names are reported sorted, and a profile
// linked into place, as a mounted ConfigMap is, loads like a file.
func TestH38LocalProfileExtendsBuiltin(t *testing.T) {
	t.Parallel()
	dir := renderLocalDir(t, map[string]string{
		"lab-xeon.yaml": renderLocalXeon,
		"lab-xeon-small.yaml": `schemaVersion: 1
name: lab-xeon-small
extends: lab-xeon
description: the local Intel host with less memory
node:
  capacity:
    memoryKi: {v: 16777216, src: "assumed:local test override"}
`,
		"README.md": "not a profile",
	})
	profiles, local, err := LoadProfileSet(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(local, ","), "lab-xeon,lab-xeon-small"; got != want {
		t.Fatalf("local = %s, want %s", got, want)
	}
	builtins, err := BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != len(builtins)+2 {
		t.Errorf("got %d profiles, want the %d builtins plus 2 local", len(profiles), len(builtins))
	}
	for name, want := range builtins {
		if !reflect.DeepEqual(profiles[name], want) {
			t.Errorf("builtin %s differs when loaded beside local profiles", name)
		}
	}

	parent := builtins["intel-xeon-2s-rapl"]
	x := profiles["lab-xeon"]
	if x == nil {
		t.Fatal("lab-xeon is missing")
	}
	if x.Physics.CPU.IdlePkgW != (Fact[float64]{V: 55, Src: "assumed:local test override"}) {
		t.Errorf("lab-xeon idlePkgW = %+v, want the local value and source", x.Physics.CPU.IdlePkgW)
	}
	if x.Physics.CPU.MaxPkgW != parent.Physics.CPU.MaxPkgW || !reflect.DeepEqual(x.CPU, parent.CPU) ||
		!reflect.DeepEqual(x.Powercap, parent.Powercap) || !reflect.DeepEqual(x.Sources, parent.Sources) {
		t.Errorf("lab-xeon: want every fact it does not write, and the sources, from intel-xeon-2s-rapl")
	}
	if x.Extends != "intel-xeon-2s-rapl" || x.Description == parent.Description {
		t.Errorf("lab-xeon extends %q, description %q: want its own header", x.Extends, x.Description)
	}
	small := profiles["lab-xeon-small"]
	if small == nil {
		t.Fatal("lab-xeon-small is missing")
	}
	if small.Node.Capacity.MemoryKi.V != 16777216 || small.Physics.CPU.IdlePkgW.V != 55 || !reflect.DeepEqual(small.CPU, parent.CPU) {
		t.Errorf("lab-xeon-small: want its own memory on lab-xeon's idle figure on the builtin's cpu")
	}

	// A mounted ConfigMap links each key through ..data into a hidden
	// directory that holds the files. The key's link is a profile; the
	// hidden directory and the ..data link are not.
	mounted := t.TempDir()
	if err := os.Mkdir(filepath.Join(mounted, "..2026_10_05"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mounted, "..2026_10_05", "lab-xeon.yaml"), []byte(renderLocalXeon), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, link := range [][2]string{{"..2026_10_05", "..data"}, {"..data/lab-xeon.yaml", "lab-xeon.yaml"}} {
		if err := os.Symlink(link[0], filepath.Join(mounted, link[1])); err != nil {
			t.Fatal(err)
		}
	}
	profiles, local, err = LoadProfileSet(mounted)
	if err != nil {
		t.Fatalf("mounted ConfigMap: %v", err)
	}
	if strings.Join(local, ",") != "lab-xeon" || profiles["lab-xeon"] == nil || profiles["lab-xeon"].Physics.CPU.IdlePkgW.V != 55 {
		t.Errorf("mounted ConfigMap: local = %v, want [lab-xeon] at 55 W idle", local)
	}

	// An extends that names no profile fails before anything is merged,
	// naming the file on disk and both places that were searched.
	typo := strings.Replace(renderLocalXeon, "name: lab-xeon\nextends: intel-xeon-2s-rapl", "name: lab-typo\nextends: intel-xeon-2s-rap1", 1)
	bad := renderLocalDir(t, map[string]string{"lab-typo.yaml": typo})
	_, _, err = LoadProfileSet(bad)
	want := filepath.Join(bad, "lab-typo.yaml") + ` extends "intel-xeon-2s-rap1", which is neither a builtin profile nor a profile in ` + bad
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want one containing %q", err, want)
	}
}

// H39: a local profile may not take a builtin's name, because one of the two
// would be shadowed without a word. The error names both files and nothing
// is loaded. The other per-file errors name the file on disk too, not the
// in-memory copy the set is loaded from.
func TestH39LocalNameCollidesWithBuiltin(t *testing.T) {
	t.Parallel()
	// The realistic case: a copy of the builtin, edited in place.
	copied, err := fs.ReadFile(renderProfilesFS, "profiles/intel-xeon-2s-rapl.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := renderLocalDir(t, map[string]string{
		"intel-xeon-2s-rapl.yaml": strings.Replace(string(copied), "idlePkgW: {v: 40,", "idlePkgW: {v: 55,", 1),
		"lab-xeon.yaml":           renderLocalXeon,
	})
	profiles, local, err := LoadProfileSet(dir)
	if err == nil {
		t.Fatal("a local profile named like a builtin loaded")
	}
	for _, want := range []string{filepath.Join(dir, "intel-xeon-2s-rapl.yaml"), "builtin profile profiles/intel-xeon-2s-rapl.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
	if profiles != nil || local != nil {
		t.Errorf("got %d profiles and local %v with the error, want none", len(profiles), local)
	}

	cases := []struct {
		name, file, body, after string
	}{
		{"name differs from the file name", "lab-xeon-2.yaml", renderLocalXeon, `: name is "lab-xeon", want "lab-xeon-2", the file name without .yaml`},
		{"duplicate key", "lab-xeon.yaml", renderLocalXeon + "physics: {}\n", `: yaml: unmarshal errors`},
		// Read past, a .yml file or a suffix in another case would surface
		// much later as an unknown profile, as a mistyped directory would
		// (H42).
		{".yml extension", "lab-xeon.yml", renderLocalXeon, `: a local profile file ends in .yaml; rename it to lab-xeon.yaml`},
		{".YAML extension", "lab-xeon.YAML", renderLocalXeon, `: a local profile file ends in .yaml; rename it to lab-xeon.yaml`},
		{".Yml extension", "lab-xeon.Yml", renderLocalXeon, `: a local profile file ends in .yaml; rename it to lab-xeon.yaml`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := renderLocalDir(t, map[string]string{c.file: c.body})
			_, _, err := LoadProfileSet(dir)
			if want := filepath.Join(dir, c.file) + c.after; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want one containing %q", err, want)
			}
		})
	}
}

// H40: a measured source names the machine it was measured on only in a
// local profile. A local profile may cite a measured source on top of a
// builtin, naming the machine or not, and keeps it; the builtin it extends
// gets nothing from it; and the builtins are still held to the repository
// rule when they are loaded beside local profiles.
func TestH40MeasuredStaysLocal(t *testing.T) {
	t.Parallel()
	const measured = `schemaVersion: 1
name: NAME
extends: intel-xeon-2s-rapl
description: the builtin Intel host with a measured idle figure
sources:
  bench: {kind: measured, ref: "REF"}
physics:
  cpu:
    idlePkgW: {v: 52, src: "measured:bench#idle"}
`
	dir := renderLocalDir(t, map[string]string{
		"lab-xeon-measured.yaml": strings.NewReplacer("NAME", "lab-xeon-measured", "REF", "local idle run").Replace(measured),
		"lab-xeon-node07.yaml":   strings.NewReplacer("NAME", "lab-xeon-node07", "REF", "idle run on node-07").Replace(measured),
	})
	profiles, local, err := LoadProfileSet(dir)
	if err != nil {
		t.Fatalf("a local profile may cite a measured source and name its machine: %v", err)
	}
	if got, want := strings.Join(local, ","), "lab-xeon-measured,lab-xeon-node07"; got != want {
		t.Errorf("local = %s, want %s", got, want)
	}
	parent := profiles["intel-xeon-2s-rapl"]
	if parent == nil {
		t.Fatalf("intel-xeon-2s-rapl is missing from %d profiles", len(profiles))
	}
	for name, ref := range map[string]string{"lab-xeon-measured": "local idle run", "lab-xeon-node07": "idle run on node-07"} {
		m := profiles[name]
		if m == nil {
			t.Fatalf("%s is missing from %d profiles", name, len(profiles))
		}
		if m.Physics.CPU.IdlePkgW != (Fact[float64]{V: 52, Src: "measured:bench#idle"}) || m.Sources["bench"] != (Source{Kind: "measured", Ref: ref}) {
			t.Errorf("%s idlePkgW = %+v, sources %v: want the measured value and its source", name, m.Physics.CPU.IdlePkgW, m.Sources)
		}
	}
	if _, ok := parent.Sources["bench"]; ok || parent.Physics.CPU.IdlePkgW.Src == "measured:bench#idle" {
		t.Errorf("the measured source reached the builtin it extends: %+v", parent.Physics.CPU.IdlePkgW)
	}

	// A builtin may cite a measured source that describes the machine by
	// its hardware, and may not name the machine, however the set is
	// loaded: with a local directory, an empty one or none. The local child
	// cites nothing measured, so only the builtin rule can reject the set.
	child := `schemaVersion: 1
name: lab
extends: base
description: a local child of a measured builtin
`
	neutral := renderMapFS(map[string]string{"base": renderMeasuredMinimalProfile(t, "idle run on one 2-socket host")})
	named := renderMapFS(map[string]string{"base": renderMeasuredMinimalProfile(t, "node-07 bench")})
	for _, dir := range []string{renderLocalDir(t, map[string]string{"lab.yaml": child}), t.TempDir(), ""} {
		if _, _, err := renderLoadProfileSet(neutral, "p", dir); err != nil {
			t.Errorf("dir %q: a builtin measured source that names only the hardware must load: %v", dir, err)
		}
		_, _, err := renderLoadProfileSet(named, "p", dir)
		if want := `base: measured source "bench" names a machine ("node-0")`; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("dir %q: err = %v, want one containing %q", dir, err, want)
		}
	}
}

// H41: no directory means the builtins only, and so does a directory with no
// *.yaml in it, such as a mounted ConfigMap that holds no profile yet. Hidden
// entries are not profiles, whatever their suffix: an editor's lock link
// that points nowhere, a hidden file and a ConfigMap's ..data link.
func TestH41EmptyDirMeansBuiltins(t *testing.T) {
	t.Parallel()
	builtins, err := BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	hidden := renderLocalDir(t, map[string]string{".lab-xeon.yaml": renderLocalXeon, ".lab-xeon.yml": renderLocalXeon})
	for _, link := range [][2]string{{"user@host.1234:1700000000", ".#lab-xeon.yaml"}, {"..2026_10_05", "..data"}} {
		if err := os.Symlink(link[0], filepath.Join(hidden, link[1])); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"", t.TempDir(), renderLocalDir(t, map[string]string{"README.md": "no profiles here"}), hidden} {
		profiles, local, err := LoadProfileSet(dir)
		if err != nil {
			t.Fatalf("dir %q: %v", dir, err)
		}
		if local != nil {
			t.Errorf("dir %q: local = %#v, want nil", dir, local)
		}
		if !reflect.DeepEqual(profiles, builtins) {
			t.Errorf("dir %q: want exactly the builtin profiles", dir)
		}
	}
}

// H42: a directory that does not exist, or a path that is not a directory,
// is an error naming the path. Falling back to the builtins would turn a
// mistyped SIM_PROFILES_DIR into an unknown profile much later.
func TestH42MissingDirIsAnError(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	profiles, local, err := LoadProfileSet(missing)
	if err == nil || !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("err = %v, want a not-exist error naming %s", err, missing)
	}
	if profiles != nil || local != nil {
		t.Errorf("got %d profiles and local %v with the error, want none", len(profiles), local)
	}

	file := filepath.Join(renderLocalDir(t, map[string]string{"lab-xeon.yaml": renderLocalXeon}), "lab-xeon.yaml")
	if _, _, err := LoadProfileSet(file); err == nil || !strings.Contains(err.Error(), file+" is not a directory") {
		t.Fatalf("err = %v, want one saying %s is not a directory", err, file)
	}
}
