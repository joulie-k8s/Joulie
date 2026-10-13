package hwemu

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// EN00: Ensure on an absent or empty root renders the node, names it on the
// Tree, and reports a fresh tree with nothing added. A root hwemu did not
// render, a directory without state/hwemu.json or a regular file, is
// refused and left as it was. A root Ensure rendered is resumed (EN02).
func TestEN00EnsureRendersAnEmptyRoot(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "intel-xeon-2s-rapl")
	absent := filepath.Join(t.TempDir(), "node")
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, root := range map[string]string{"absent": absent, "empty": empty} {
		tree, rep, err := Ensure(p, root, RenderOptions{NodeName: "node-a"})
		if err != nil {
			t.Fatalf("%s root: %v", name, err)
		}
		if !rep.Fresh || len(rep.Added) != 0 {
			t.Errorf("%s root: report %+v, want Fresh and nothing added", name, rep)
		}
		if tree.NodeName != "node-a" || tree.Root != root || len(tree.Files) == 0 {
			t.Errorf("%s root: tree %q at %q with %d files, want node-a at %q with files", name, tree.NodeName, tree.Root, len(tree.Files), root)
		}
		if _, err := os.Stat(filepath.Join(root, layout.StateDir, "hwemu.json")); err != nil {
			t.Errorf("%s root: %v", name, err)
		}
	}

	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "notes.txt"), []byte("not a node\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(file, []byte("not a node\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, root := range map[string]string{"foreign": foreign, "file": file} {
		before := nodeSnapshot(t, root)
		tree, rep, err := Ensure(p, root, RenderOptions{NodeName: "node-a"})
		if err == nil || tree != nil || !reflect.DeepEqual(rep, EnsureReport{}) {
			t.Errorf("%s root: Ensure gave tree %v, report %+v, error %v; want only an error", name, tree, rep, err)
		}
		if !errors.Is(err, ErrTreeConflict) {
			t.Errorf("%s root: error %v, want ErrTreeConflict", name, err)
		}
		if after := nodeSnapshot(t, root); !reflect.DeepEqual(after, before) {
			t.Errorf("%s root: a refused Ensure changed the root", name)
		}
	}
}

// ensureBuiltin is the builtin profile name cut to two cores per socket,
// which keeps the trees the Ensure, state and persistence tests render many
// times small. Everything else is the profile's.
func ensureBuiltin(tb testing.TB, name string) *Profile {
	tb.Helper()
	p := renderBuiltin(tb, name)
	if c := p.CPU; c.CoresPerSocket.V > 2 && c.CPUInfoVerbatim == "" {
		p.CPU.CoresPerSocket = Fact[int]{V: 2, Src: nodeSmallTopology}
		p.Node.Capacity.CPU = Fact[int64]{V: int64(c.Sockets.V * 2 * c.ThreadsPerCore.V), Src: nodeSmallTopology}
	}
	return p
}

// ensureEntry is one entry of a tree as ensureStat records it.
type ensureEntry struct {
	Mode    fs.FileMode
	Content string
	Inode   uint64
	MTime   time.Time
}

// ensureStat records every entry under root: mode, content or link target,
// inode and modification time. state/.tmp and what it holds are left out:
// it is the scratch directory both the Node and Ensure write temporary
// files to, so its own modification time moves with every step.
func ensureStat(tb testing.TB, root string) map[string]ensureEntry {
	tb.Helper()
	scratch := path.Join(layout.StateDir, layout.TmpDir)
	out := map[string]ensureEntry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == scratch || strings.HasPrefix(rel, scratch+"/") {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		e := ensureEntry{Mode: fi.Mode(), Inode: fi.Sys().(*syscall.Stat_t).Ino, MTime: fi.ModTime()}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			e.Content, err = os.Readlink(p)
		case fi.Mode().IsRegular():
			var b []byte
			b, err = os.ReadFile(p)
			e.Content = string(b)
		}
		out[rel] = e
		return err
	})
	if err != nil {
		tb.Fatal(err)
	}
	return out
}

// ensureDiff lists the entries that differ between two ensureStat records.
func ensureDiff(before, after map[string]ensureEntry) []string {
	var diffs []string
	for rel, b := range before {
		a, ok := after[rel]
		switch {
		case !ok:
			diffs = append(diffs, rel+": removed")
		case a != b:
			diffs = append(diffs, fmt.Sprintf("%s: %+v became %+v", rel, b, a))
		}
	}
	for rel := range after {
		if _, ok := before[rel]; !ok {
			diffs = append(diffs, rel+": added")
		}
	}
	sort.Strings(diffs)
	return diffs
}

// ensureNoScratch fails when a resume left its scratch render behind.
func ensureNoScratch(tb testing.TB, root string) {
	tb.Helper()
	left, err := filepath.Glob(filepath.Join(root, layout.StateDir, layout.TmpDir, ensureScratchPrefix+"*"))
	if err != nil || len(left) > 0 {
		tb.Errorf("scratch renders left in state/.tmp: %v %v", left, err)
	}
}

// EN01: Ensure on an empty root gives the same entries, bytes, modes, link
// targets and Tree as Render.
func TestEN01EnsureOnAnEmptyRootEqualsRender(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"intel-xeon-2s-rapl", "nvidia-h100-nvl-8gpu", "amd-instinct-mi300x-8gpu", "vm-no-powercap"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := ensureBuiltin(t, name)
			o := RenderOptions{NodeName: "node-a", Seed: 7}
			rendered, err := Render(p, filepath.Join(t.TempDir(), "node"), o)
			if err != nil {
				t.Fatal(err)
			}
			ensured, rep, err := Ensure(p, filepath.Join(t.TempDir(), "node"), o)
			if err != nil {
				t.Fatal(err)
			}
			if !rep.Fresh || rep.Added != nil {
				t.Errorf("report %+v, want Fresh and nothing added", rep)
			}
			if !reflect.DeepEqual(ensured.Files, rendered.Files) || ensured.NodeName != "node-a" || rendered.NodeName != "node-a" {
				t.Errorf("Ensure and Render return different trees: %d and %d files, nodes %q and %q", len(ensured.Files), len(rendered.Files), ensured.NodeName, rendered.NodeName)
			}
			want, got := ensureStat(t, rendered.Root), ensureStat(t, ensured.Root)
			if len(want) != len(got) {
				t.Fatalf("Render made %d entries, Ensure %d", len(want), len(got))
			}
			for rel, w := range want {
				g, ok := got[rel]
				if !ok || g.Mode != w.Mode || g.Content != w.Content {
					t.Errorf("%s: Ensure gave %v %q, Render %v %q", rel, g.Mode, g.Content, w.Mode, w.Content)
				}
			}
		})
	}
}

// EN02: Ensure over a live tree, after a Node stepped it and the agent wrote
// a PL1 limit the Node has not read back yet, changes no byte, inode or
// modification time, adds nothing and returns the tree Render returned.
func TestEN02EnsureLeavesALiveTreeUntouched(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "nvidia-h100-nvl-8gpu")
	root := filepath.Join(t.TempDir(), "node")
	o := RenderOptions{NodeName: "node-a"}
	tree, _, err := Ensure(p, root, o)
	if err != nil {
		t.Fatal(err)
	}
	n := nodeNewNode(t, p, tree)
	n.SetLoad(Load{CPUUtil: []float64{0.8}, GPUUtil: []float64{0.6}})
	nodeStep(t, n, 10*time.Second)
	nodeWrite(t, tree, nodeZoneFile("constraint_0_power_limit_uw", 0), "120000000")

	before := ensureStat(t, root)
	again, rep, err := Ensure(p, root, o)
	if err != nil {
		t.Fatalf("Ensure over a live tree: %v", err)
	}
	if rep.Fresh || len(rep.Added) != 0 {
		t.Errorf("report %+v, want a resume that adds nothing", rep)
	}
	if !reflect.DeepEqual(again, tree) {
		t.Errorf("the resumed Tree differs from the rendered one")
	}
	if d := ensureDiff(before, ensureStat(t, root)); len(d) > 0 {
		t.Errorf("Ensure changed the live tree:\n%s", strings.Join(d, "\n"))
	}
	if got := nodeRead(t, tree, nodeZoneFile("constraint_0_power_limit_uw", 0)); got != "120000000" {
		t.Errorf("the agent's PL1 write reads %q after Ensure, want it kept", got)
	}
	ensureNoScratch(t, root)
}

// EN03: entries deleted from a live tree come back from a fresh render, and
// only those: an R file and a class link, each listed in Added; a whole
// policy directory, listed with every entry in it; state/.tmp, created
// before the scratch render goes in it. A scratch render a crashed resume
// left behind is removed, and nothing else in state/.tmp is.
func TestEN03EnsurePutsBackOnlyMissingEntries(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "intel-xeon-2s-rapl")
	o := RenderOptions{NodeName: "node-a"}
	ref, err := Render(p, filepath.Join(t.TempDir(), "node"), o)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "node")
	tree, _, err := Ensure(p, root, o)
	if err != nil {
		t.Fatal(err)
	}
	n := nodeNewNode(t, p, tree)
	nodeStep(t, n, 2*time.Second)

	rfile := nodeZoneFile("constraint_0_name", 1)
	link, _ := layout.PowercapClassLink("intel-rapl", 1)
	link = path.Join(layout.SysDir, link)
	for _, rel := range []string{rfile, link} {
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	before := ensureStat(t, root)
	_, rep, err := Ensure(p, root, o)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{link, rfile}
	sort.Strings(want)
	if rep.Fresh || !reflect.DeepEqual(rep.Added, want) {
		t.Errorf("report %+v, want Added %v", rep, want)
	}
	after := ensureStat(t, root)
	for _, rel := range want {
		if a, w := after[rel], ensureStat(t, ref.Root)[rel]; a.Content != w.Content || a.Mode != w.Mode {
			t.Errorf("%s came back as %v %q, want %v %q", rel, a.Mode, a.Content, w.Mode, w.Content)
		}
		delete(after, rel)
	}
	// The directories the two entries came back to change their time; no
	// entry of their own changes.
	for _, dir := range []string{path.Dir(rfile), path.Dir(link)} {
		delete(before, dir)
		delete(after, dir)
	}
	if d := ensureDiff(before, after); len(d) > 0 {
		t.Errorf("Ensure changed entries it did not add:\n%s", strings.Join(d, "\n"))
	}

	policy := path.Join(layout.SysDir, layout.PolicyDir(5))
	if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(policy))); err != nil {
		t.Fatal(err)
	}
	_, rep, err = Ensure(p, root, o)
	if err != nil {
		t.Fatal(err)
	}
	var wantDir []string
	for _, f := range ref.Files {
		if f.Rel == policy || strings.HasPrefix(f.Rel, policy+"/") {
			wantDir = append(wantDir, f.Rel)
		}
	}
	if len(wantDir) < 2 || !reflect.DeepEqual(rep.Added, wantDir) {
		t.Errorf("after removing %s: Added %v, want %v", policy, rep.Added, wantDir)
	}
	if got, want := nodeRead(t, tree, nodePolicyFile(5, "scaling_driver")), nodeRead(t, ref, nodePolicyFile(5, "scaling_driver")); got != want {
		t.Errorf("policy5 scaling_driver = %q, want %q", got, want)
	}
	ensureNoScratch(t, root)

	// A scratch render a crashed resume left in state/.tmp is removed; a
	// temporary file of the Node next to it is not.
	tmp := filepath.Join(root, layout.StateDir, layout.TmpDir)
	crashed := filepath.Join(tmp, ensureScratchPrefix+"123")
	nodeTemp := filepath.Join(tmp, "s-456")
	if err := os.MkdirAll(filepath.Join(crashed, layout.SysDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(crashed, layout.SysDir, "x"), nodeTemp} {
		if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, rep, err = Ensure(p, root, o); err != nil || rep.Added != nil {
		t.Fatalf("resume with a crashed scratch render: %+v %v, want nothing added", rep, err)
	}
	if _, err := os.Lstat(crashed); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the crashed scratch render is still there: %v", err)
	}
	if _, err := os.Lstat(nodeTemp); err != nil {
		t.Errorf("the Node's temporary file was removed: %v", err)
	}

	// A missing state/.tmp is created first and listed in Added.
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if _, rep, err = Ensure(p, root, o); err != nil || !reflect.DeepEqual(rep.Added, []string{path.Join(layout.StateDir, layout.TmpDir)}) {
		t.Errorf("resume without state/.tmp: %+v %v, want Added [state/.tmp]", rep, err)
	}
	if fi, err := os.Lstat(tmp); err != nil || !fi.IsDir() {
		t.Errorf("state/.tmp after the resume: %v", err)
	}
	ensureNoScratch(t, root)
}

// EN04: a tree rendered from another profile, for another node or from
// other values, or with no profileDigest, is ErrProfileChanged naming the
// value found and the value wanted, and is left as it was. Provenance is no
// value: a changed src alone resumes. Neither is a zero value, so a field
// added to the schema later leaves the digest of a profile without it.
func TestEN04EnsureRefusesAnotherProfileNodeOrValue(t *testing.T) {
	t.Parallel()
	o := RenderOptions{NodeName: "node-a"}
	root := filepath.Join(t.TempDir(), "node")
	if _, _, err := Ensure(ensureBuiltin(t, "intel-xeon-2s-rapl"), root, o); err != nil {
		t.Fatal(err)
	}
	digest, err := ensureProfileDigest(ensureBuiltin(t, "intel-xeon-2s-rapl"))
	if err != nil {
		t.Fatal(err)
	}
	limit := func() *Profile {
		p := ensureBuiltin(t, "intel-xeon-2s-rapl")
		p.Powercap.ControlTypes[0].Zones[0].Constraints[0].PowerLimitUW.V = 150000000
		return p
	}
	otherDigest, err := ensureProfileDigest(limit())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		p    *Profile
		node string
		want []string
	}{
		{"profile", ensureBuiltin(t, "nvidia-h100-nvl-8gpu"), "node-a", []string{`profile "intel-xeon-2s-rapl", want "nvidia-h100-nvl-8gpu"`}},
		{"node", ensureBuiltin(t, "intel-xeon-2s-rapl"), "node-b", []string{`node "node-a", want "node-b"`}},
		{"limit value", limit(), "node-a", []string{fmt.Sprintf("profileDigest %q, want %q", digest, otherDigest)}},
	} {
		before := nodeSnapshot(t, root)
		tree, _, err := Ensure(c.p, root, RenderOptions{NodeName: c.node})
		if !errors.Is(err, ErrProfileChanged) || tree != nil {
			t.Errorf("%s: Ensure gave %v, want ErrProfileChanged", c.name, err)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q does not name %s", c.name, err, w)
			}
		}
		if !reflect.DeepEqual(nodeSnapshot(t, root), before) {
			t.Errorf("%s: a refused Ensure changed the root", c.name)
		}
	}

	// A field the schema gains later is no value while a profile leaves it
	// unset: the profile's JSON with new keys holding zero values, at the top
	// and nested in objects and arrays, has the same digest. A new key with
	// a value changes it.
	b, err := json.Marshal(ensureBuiltin(t, "intel-xeon-2s-rapl"))
	if err != nil {
		t.Fatal(err)
	}
	extended := func(v any) string {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		m["accelerators"] = nil
		m["cpu"].(map[string]any)["smtControl"] = ""
		m["node"].(map[string]any)["taints"] = []any{}
		ct := m["powercap"].(map[string]any)["controlTypes"].([]any)[0].(map[string]any)
		ct["mmio"] = false
		ct["zones"].([]any)[0].(map[string]any)["floorUW"] = map[string]any{"v": v, "src": "assumed:test fixture"}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		d, err := ensureDigestJSON(out)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if got := extended(0); got != digest {
		t.Errorf("new keys with zero values: digest %s, want the profile's %s", got, digest)
	}
	if got := extended(5000000); got == digest {
		t.Errorf("a new key with a value kept the digest %s", got)
	}

	src := ensureBuiltin(t, "intel-xeon-2s-rapl")
	src.Powercap.ControlTypes[0].Zones[0].Constraints[0].PowerLimitUW.Src = "assumed:another reason for the same value"
	src.Sources["extra"] = Source{Kind: "published", Ref: "https://example.org/extra"}
	if _, _, err := Ensure(src, root, o); err != nil {
		t.Errorf("a changed src alone: %v, want a resume", err)
	}

	// A tree from before the digest records none.
	meta := filepath.Join(root, filepath.FromSlash(ensureMetaFile))
	b, err = json.Marshal(map[string]any{"schemaVersion": 1, "profile": "intel-xeon-2s-rapl", "node": "node-a", "substep": "100ms"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = Ensure(ensureBuiltin(t, "intel-xeon-2s-rapl"), root, o)
	if !errors.Is(err, ErrProfileChanged) || !strings.Contains(err.Error(), fmt.Sprintf(`profileDigest "", want %q`, digest)) {
		t.Errorf("no profileDigest: %v, want ErrProfileChanged naming the digest", err)
	}
}

// EN05: an entry of another kind than the render's, or a root without
// state/hwemu.json, is ErrTreeConflict naming the entry, found before
// anything is moved: the root is left as it was.
func TestEN05EnsureRefusesAConflictingTree(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "intel-xeon-2s-rapl")
	o := RenderOptions{NodeName: "node-a"}
	rfile := nodeZoneFile("constraint_0_name", 0)
	link, _ := layout.PowercapClassLink("intel-rapl", 0)
	link = path.Join(layout.SysDir, link)
	missing := nodeZoneFile("constraint_1_name", 0)
	for _, c := range []struct {
		name   string
		change func(root string) error
		want   error
		names  string
	}{
		{"directory for a file", func(root string) error {
			abs := filepath.Join(root, filepath.FromSlash(rfile))
			return errors.Join(os.Remove(abs), os.Mkdir(abs, 0o755))
		}, ErrTreeConflict, rfile + ": want a file, found a directory"},
		{"file for a link", func(root string) error {
			abs := filepath.Join(root, filepath.FromSlash(link))
			return errors.Join(os.Remove(abs), os.WriteFile(abs, []byte("x\n"), 0o644))
		}, ErrTreeConflict, link + ": want a link, found a file"},
		{"link to another target", func(root string) error {
			abs := filepath.Join(root, filepath.FromSlash(link))
			return errors.Join(os.Remove(abs), os.Symlink("../../devices/virtual/powercap/intel-rapl/intel-rapl:1", abs))
		}, ErrTreeConflict, link + ": want a link to"},
		{"no hwemu.json", func(root string) error {
			return os.Remove(filepath.Join(root, filepath.FromSlash(ensureMetaFile)))
		}, ErrTreeConflict, "has no readable state/hwemu.json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "node")
			if _, _, err := Ensure(p, root, o); err != nil {
				t.Fatal(err)
			}
			// A missing entry next to the conflict must not be moved in.
			if err := c.change(root); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(missing))); err != nil {
				t.Fatal(err)
			}
			before := nodeSnapshot(t, root)
			tree, rep, err := Ensure(p, root, o)
			if !errors.Is(err, c.want) || tree != nil || rep.Added != nil {
				t.Fatalf("Ensure gave tree %v, report %+v, error %v; want %v", tree, rep, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("error %q does not say %q", err, c.names)
			}
			if !reflect.DeepEqual(nodeSnapshot(t, root), before) {
				t.Errorf("a refused Ensure changed the root")
			}
		})
	}
}

// EN06: a new Node over a tree Ensure resumed reads every energy_uj and
// total_energy_mj at or above the value the last Node left, so no reader
// sees a counter go back, which it would take for a wrap. The counters start
// at 0, far from max_energy_range_uj, so none can wrap in the test.
func TestEN06CountersContinueAcrossEnsure(t *testing.T) {
	t.Parallel()
	p := ensureBuiltin(t, "nvidia-h100-nvl-8gpu")
	root := filepath.Join(t.TempDir(), "node")
	o := RenderOptions{NodeName: "node-a"}
	tree, _, err := Ensure(p, root, o)
	if err != nil {
		t.Fatal(err)
	}
	load := Load{CPUUtil: []float64{0.9}, GPUUtil: []float64{0.7}}
	n := nodeNewNode(t, p, tree)
	n.SetLoad(load)
	for i := 0; i < 7; i++ {
		nodeStep(t, n, 1300*time.Millisecond)
	}
	counters := map[string]int64{}
	for _, f := range tree.Files {
		if base := path.Base(f.Rel); base == "energy_uj" || base == "total_energy_mj" {
			counters[f.Rel] = nodeReadInt64(t, tree, f.Rel)
		}
	}
	if len(counters) < 8+4 {
		t.Fatalf("found %d energy counters, want the RAPL zones and 8 GPUs", len(counters))
	}

	resumed, rep, err := Ensure(p, root, o)
	if err != nil || rep.Fresh {
		t.Fatalf("resume: %+v %v", rep, err)
	}
	m, err := NewNode(p, resumed, Options{Start: nodeStart.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLoad(load)
	for step := 0; step < 3; step++ {
		nodeStep(t, m, 700*time.Millisecond)
		for rel, last := range counters {
			got := nodeReadInt64(t, resumed, rel)
			if got < last {
				t.Fatalf("step %d: %s = %d after the resume, below the %d it had", step, rel, got, last)
			}
			if rng := path.Join(path.Dir(rel), "max_energy_range_uj"); path.Base(rel) == "energy_uj" && got > nodeReadInt64(t, resumed, rng)/2 {
				t.Fatalf("%s = %d is near its wrap; the test needs counters far from it", rel, got)
			}
			counters[rel] = got
		}
	}
	if gpu := nodeNVMLFile(0, "total_energy_mj"); counters[gpu] == 0 {
		t.Errorf("%s did not move under load", gpu)
	}
}
