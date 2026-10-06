package hwemu

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderUpdateGolden rewrites testdata/layout/*.txt:
//
//	go test ./simulator/pkg/hwemu/ -run TestH02 -update
var renderUpdateGolden = flag.Bool("update", false, "rewrite simulator/pkg/hwemu/testdata/layout/*.txt from the rendered trees")

// renderTestTree renders p into a fresh directory.
func renderTestTree(t testing.TB, p *Profile) *Tree {
	t.Helper()
	tree, err := Render(p, filepath.Join(t.TempDir(), "node"), RenderOptions{NodeName: "hwemu-test-node"})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// renderShared holds one rendering of each builtin profile for the tests that
// only read a tree, so the three H02 tests that walk every profile render
// each one once per test binary instead of once per test. Rendering is
// syscall bound and was most of their time. TestMain removes the directory.
var renderShared struct {
	mu      sync.Mutex
	dir     string
	entries map[string]*renderSharedEntry
}

type renderSharedEntry struct {
	once sync.Once
	p    *Profile
	tree *Tree
	err  error
}

// renderSharedBuiltin returns the shared rendering of builtin profile name,
// rendered as renderTestTree renders it. Callers must not write to the tree
// or change the profile.
func renderSharedBuiltin(t testing.TB, name string) (*Profile, *Tree) {
	t.Helper()
	renderShared.mu.Lock()
	if renderShared.entries == nil {
		renderShared.entries = map[string]*renderSharedEntry{}
	}
	e, ok := renderShared.entries[name]
	if !ok {
		e = &renderSharedEntry{}
		renderShared.entries[name] = e
	}
	if renderShared.dir == "" {
		renderShared.dir, e.err = os.MkdirTemp("", "hwemu-shared-render-")
	}
	dir := renderShared.dir
	renderShared.mu.Unlock()
	e.once.Do(func() {
		if e.err != nil {
			return
		}
		profiles, err := BuiltinProfiles()
		if err != nil {
			e.err = err
			return
		}
		if e.p = profiles[name]; e.p == nil {
			e.err = fmt.Errorf("no builtin profile %q", name)
			return
		}
		e.tree, e.err = Render(e.p, filepath.Join(dir, name), RenderOptions{NodeName: "hwemu-test-node"})
	})
	if e.err != nil {
		t.Fatal(e.err)
	}
	return e.p, e.tree
}

func TestMain(m *testing.M) {
	code := m.Run()
	if renderShared.dir != "" {
		if err := os.RemoveAll(renderShared.dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	os.Exit(code)
}

// renderReadFile returns the content of rel under the node root.
func renderReadFile(t testing.TB, tree *Tree, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(tree.Root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var renderOwnerLetters = map[layout.Owner]string{layout.OwnerRender: "R", layout.OwnerEmulator: "S", layout.OwnerAgent: "A"}
var renderKindWords = map[layout.Kind]string{layout.KindFile: "file", layout.KindDir: "dir", layout.KindSymlink: "link", layout.KindRejectingDir: "rejectdir"}

// renderLayoutListing lists a rendered tree one entry per line, sorted: path,
// owner (R, S or A), kind, mode and link target. Kind, mode and target are read from disk and
// must agree with Tree.Files, which must list exactly the entries on disk.
// Lines that differ only in one CPU or policy number fold into one line with
// the numbers as ranges, so a golden stays readable at 384 CPUs and still
// changes when one CPU gains or loses a file.
func renderLayoutListing(t testing.TB, tree *Tree) string {
	t.Helper()
	specs := map[string]layout.FileSpec{}
	for _, f := range tree.Files {
		if _, dup := specs[f.Rel]; dup {
			t.Errorf("Tree.Files lists %s twice", f.Rel)
		}
		specs[f.Rel] = f
	}
	var lines []string
	err := filepath.WalkDir(tree.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == tree.Root {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, tree.Root+string(filepath.Separator)))
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		spec, ok := specs[rel]
		if !ok {
			t.Errorf("%s is on disk but not in Tree.Files", rel)
			return nil
		}
		delete(specs, rel)
		kind := layout.KindFile
		target := ""
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			kind = layout.KindSymlink
			target, err = os.Readlink(p)
			if err != nil {
				return err
			}
		case fi.IsDir():
			kind = layout.KindDir
			if spec.Kind == layout.KindRejectingDir {
				kind = layout.KindRejectingDir
			}
		}
		mode := fi.Mode().Perm()
		if spec.Kind != kind || spec.Mode != mode || spec.Target != target {
			t.Errorf("%s: Tree.Files says kind %d mode %04o target %q, disk has kind %d mode %04o target %q",
				rel, spec.Kind, spec.Mode, spec.Target, kind, mode, target)
		}
		line := fmt.Sprintf("%s %s %s %04o", rel, renderOwnerLetters[spec.Owner], renderKindWords[kind], mode)
		if target != "" {
			line += " -> " + target
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel := range specs {
		t.Errorf("Tree.Files lists %s, which is not on disk", rel)
	}
	return renderFoldCPUNumbers(lines)
}

var renderCPUNumber = regexp.MustCompile(`(cpu|policy)(\d+)`)

func renderFoldCPUNumbers(lines []string) string {
	folded := map[string][]int{}
	var out []string
	for _, line := range lines {
		ms := renderCPUNumber.FindAllStringSubmatch(line, -1)
		if len(ms) == 0 {
			out = append(out, line)
			continue
		}
		n := ms[0][2]
		same := true
		for _, m := range ms {
			same = same && m[2] == n
		}
		if !same {
			out = append(out, line)
			continue
		}
		v, _ := strconv.Atoi(n)
		key := renderCPUNumber.ReplaceAllString(line, "${1}{N}")
		folded[key] = append(folded[key], v)
	}
	for key, ns := range folded {
		out = append(out, key+" [N="+renderRanges(ns)+"]")
	}
	sort.Strings(out)
	return strings.Join(out, "\n") + "\n"
}

// renderRanges writes sorted numbers as ranges, such as 0-3,8.
func renderRanges(ns []int) string {
	sort.Ints(ns)
	var parts []string
	for i := 0; i < len(ns); {
		j := i
		for j+1 < len(ns) && ns[j+1] == ns[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(ns[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ns[i], ns[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// H02: each builtin profile renders exactly the tree in its golden: every
// path, kind, mode, owner and link target. A change to the layout shows up as
// a golden diff in review, never silently.
func TestH02RenderedTreesMatchGoldens(t *testing.T) {
	t.Parallel()
	for _, name := range renderBuiltinNames {
		t.Run(name, func(t *testing.T) {
			_, tree := renderSharedBuiltin(t, name)
			got := renderLayoutListing(t, tree)
			golden := filepath.Join("testdata", "layout", name+".txt")
			if *renderUpdateGolden {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (create it with: go test ./simulator/pkg/hwemu/ -run TestH02 -update)", err)
			}
			if got != string(want) {
				t.Fatalf("rendered tree differs from %s. If the change is intended, review it and run: go test ./simulator/pkg/hwemu/ -run TestH02 -update\n%s",
					golden, renderLineDiff(string(want), got))
			}
		})
	}
}

// renderLineDiff lists the lines only one side has.
func renderLineDiff(want, got string) string {
	in := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, l := range strings.Split(s, "\n") {
			m[l] = true
		}
		return m
	}
	w, g := in(want), in(got)
	var sb strings.Builder
	for _, l := range strings.Split(want, "\n") {
		if !g[l] {
			sb.WriteString("- " + l + "\n")
		}
	}
	for _, l := range strings.Split(got, "\n") {
		if !w[l] {
			sb.WriteString("+ " + l + "\n")
		}
	}
	return sb.String()
}

// H03: class/powercap/*, class/hwmon/*, class/drm/* and cpuN/cpufreq are
// relative symlinks that resolve inside SysRoot into devices/..., so the real
// double-path layout exists (D5, D6) and resolves the same way at /host-sys
// in a pod.
func TestH03ClassEntriesAreRelativeLinksIntoDevices(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"intel-xeon-2s-rapl", "amd-epyc-2s-hsmp", "amd-instinct-mi300x-8gpu"} {
		t.Run(name, func(t *testing.T) {
			tree := renderTestTree(t, renderBuiltin(t, name))
			sysReal, err := filepath.EvalSymlinks(tree.SysRoot)
			if err != nil {
				t.Fatal(err)
			}
			var links []string
			for _, glob := range []string{"class/powercap/*", "class/hwmon/*", "class/drm/*", "devices/system/cpu/cpu*/cpufreq"} {
				m, err := filepath.Glob(filepath.Join(tree.SysRoot, glob))
				if err != nil {
					t.Fatal(err)
				}
				links = append(links, m...)
			}
			if len(links) == 0 {
				t.Fatal("no class entries rendered")
			}
			for _, l := range links {
				fi, err := os.Lstat(l)
				if err != nil {
					t.Fatal(err)
				}
				if fi.Mode()&fs.ModeSymlink == 0 {
					t.Errorf("%s is not a symlink", l)
					continue
				}
				target, _ := os.Readlink(l)
				if filepath.IsAbs(target) {
					t.Errorf("%s -> %s is absolute; it would leave the mount in a pod", l, target)
				}
				resolved, err := filepath.EvalSymlinks(l)
				if err != nil {
					t.Errorf("%s does not resolve: %v", l, err)
					continue
				}
				if !strings.HasPrefix(resolved, filepath.Join(sysReal, "devices")+string(filepath.Separator)) {
					t.Errorf("%s resolves to %s, outside %s/devices", l, resolved, sysReal)
				}
			}
		})
	}

	// The same package energy counter is reachable through the class link
	// and through the devices path, which is what the agent's globs see.
	tree := renderTestTree(t, renderBuiltin(t, "intel-xeon-2s-rapl"))
	viaClass, _ := filepath.Glob(filepath.Join(tree.SysRoot, "class/powercap/*:*/energy_uj"))
	viaDevices, _ := filepath.Glob(filepath.Join(tree.SysRoot, "devices/virtual/powercap/intel-rapl/*/energy_uj"))
	if len(viaClass) != 4 || len(viaDevices) != 2 {
		t.Fatalf("energy_uj via class: %d (want 2 packages + 2 dram), via devices: %d (want 2 packages)", len(viaClass), len(viaDevices))
	}
	for _, d := range viaDevices {
		zone := filepath.Base(filepath.Dir(d))
		c := filepath.Join(tree.SysRoot, "class/powercap", zone, "energy_uj")
		a, _ := os.Stat(c)
		b, _ := os.Stat(d)
		if a == nil || b == nil || !os.SameFile(a, b) {
			t.Errorf("%s and %s are not the same file", c, d)
		}
	}
}

// H23: Render refuses a root that is not empty and leaves it as it was, so it
// can never clobber a live tree. An absent or empty root is fine.
func TestH23RenderRefusesNonEmptyRoot(t *testing.T) {
	t.Parallel()
	p := renderBuiltin(t, "intel-xeon-2s-rapl")
	root := t.TempDir()
	marker := filepath.Join(root, "energy_uj")
	if err := os.WriteFile(marker, []byte("123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(p, root, RenderOptions{NodeName: "n"}); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("err = %v, want a refusal of the non-empty root", err)
	}
	entries, _ := os.ReadDir(root)
	if b, _ := os.ReadFile(marker); len(entries) != 1 || string(b) != "123\n" {
		t.Fatalf("the refused root changed: %d entries, marker %q", len(entries), b)
	}

	file := filepath.Join(t.TempDir(), "plainfile")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(p, file, RenderOptions{NodeName: "n"}); err == nil {
		t.Fatal("Render accepted a regular file as root")
	}

	if _, err := Render(p, t.TempDir(), RenderOptions{NodeName: "n"}); err != nil {
		t.Fatalf("empty root: %v", err)
	}
	if _, err := Render(p, filepath.Join(t.TempDir(), "absent"), RenderOptions{NodeName: "n"}); err != nil {
		t.Fatalf("absent root: %v", err)
	}
	if _, err := Render(p, filepath.Join(t.TempDir(), "absent"), RenderOptions{}); err == nil {
		t.Fatal("Render accepted an empty NodeName")
	}
}

// The common entries of every family: cpuinfo, state/hwemu.json, the L03
// tools envelope with one bin link per tool, and dev/nvidiactl exactly on
// NVIDIA profiles.
func TestH02CommonEntriesOfEveryFamily(t *testing.T) {
	t.Parallel()
	for _, name := range renderBuiltinNames {
		t.Run(name, func(t *testing.T) {
			p, tree := renderSharedBuiltin(t, name)
			if got := renderReadFile(t, tree, "state/hwemu.json"); got != `{"schemaVersion":1,"profile":"`+name+`","node":"hwemu-test-node","substep":"100ms"}`+"\n" {
				t.Errorf("hwemu.json = %s", got)
			}
			tools := renderReadFile(t, tree, "state/tools.json")
			if !strings.HasPrefix(tools, `{"schemaVersion":1,"tools":{`) {
				t.Errorf("tools.json = %s, want the L03 envelope", tools)
			}
			var want []string
			if p.GPUs != nil {
				for _, tool := range p.GPUs.Tools {
					want = append(want, tool.Name)
					if !strings.Contains(tools, `"`+tool.Name+`":{"variant":"`+tool.Variant+`"}`) {
						t.Errorf("tools.json = %s lacks %s %s", tools, tool.Name, tool.Variant)
					}
				}
			}
			bins, _ := os.ReadDir(filepath.Join(tree.Root, layout.BinDir))
			var got []string
			for _, b := range bins {
				target, err := os.Readlink(filepath.Join(tree.Root, layout.BinDir, b.Name()))
				if err != nil || target != layout.FakeSMIInContainer {
					t.Errorf("bin/%s -> %q, %v; want a link to %s", b.Name(), target, err, layout.FakeSMIInContainer)
				}
				got = append(got, b.Name())
			}
			if strings.Join(renderSortedStrings(got), ",") != strings.Join(renderSortedStrings(want), ",") {
				t.Errorf("bin = %v, want %v", got, want)
			}
			_, err := os.Stat(filepath.Join(tree.Root, layout.DevNvidiactl))
			isNvidia := p.GPUs != nil && p.GPUs.Vendor == "nvidia"
			if isNvidia != (err == nil) {
				t.Errorf("dev/nvidiactl exists = %v, want %v", err == nil, isNvidia)
			}
			if fi, err := os.Stat(tree.CPUInfoPath); err != nil || fi.Mode().Perm() != 0o444 {
				t.Errorf("cpuinfo: %v", err)
			}
		})
	}
}

// renderIsEISDIR reports whether writing path fails the way a rejected sysfs
// write does on an emulated node: EISDIR, because a directory stands in for
// the attribute.
func renderIsEISDIR(path string) bool {
	err := os.WriteFile(path, []byte("1"), 0o644)
	return errors.Is(err, syscall.EISDIR)
}
