package hwemu

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// L06: requirements are an optional key of a tools.json entry. An entry
// without them marshals byte for byte as in L03, so a manifest written
// before requirements existed reads the same; one with them carries the
// requires array in order, and both round trip.
func TestL06ToolsManifestRequiresKeyOnlyWhenSet(t *testing.T) {
	cases := []struct {
		name string
		m    layout.ToolsManifest
		want string
	}{
		{"no tools", layout.ToolsManifest{SchemaVersion: 1, Tools: map[string]layout.ToolEntry{}},
			`{"schemaVersion":1,"tools":{}}`},
		{"no requirements", layout.ToolsManifest{SchemaVersion: 1, Tools: map[string]layout.ToolEntry{"rocm-smi": {Variant: "current"}, "amd-smi": {Variant: "7.2"}}},
			`{"schemaVersion":1,"tools":{"amd-smi":{"variant":"7.2"},"rocm-smi":{"variant":"current"}}}`},
		{"requirements", layout.ToolsManifest{SchemaVersion: 1, Tools: map[string]layout.ToolEntry{"nvidia-smi": {Variant: "default", Requires: []layout.ToolRequirement{
			{Paths: []string{"/lib64/ld-linux-x86-64.so.2"}, Exit: 127},
			{Paths: []string{"/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1", "/usr/lib64/libnvidia-ml.so.1"}, Exit: 12, Stderr: "no NVML"},
		}}}},
			`{"schemaVersion":1,"tools":{"nvidia-smi":{"variant":"default","requires":[` +
				`{"paths":["/lib64/ld-linux-x86-64.so.2"],"exit":127},` +
				`{"paths":["/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1","/usr/lib64/libnvidia-ml.so.1"],"exit":12,"stderr":"no NVML"}]}}}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.m)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(b) != c.want {
			t.Errorf("%s: marshals to %s, want %s", c.name, b, c.want)
		}
		var back layout.ToolsManifest
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(back, c.m) {
			t.Errorf("%s: round trip gives %+v, want %+v", c.name, back, c.m)
		}
	}
}

// The requirements of the two builtin profiles that carry them reach
// tools.json in profile order without their sources, fakesmi acts on them
// over the rendered tree, and a requirement without a source, or with a
// path or exit code a real tool could not have, fails validation. Every
// root file system here is a temporary directory, never the host's /.
func TestRenderToolsRequiresNeedSrc(t *testing.T) {
	t.Parallel()
	loader := layout.ToolRequirement{Paths: []string{"/lib64/ld-linux-x86-64.so.2"}, Exit: 127}
	nvmlPaths := []string{"/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1", "/usr/lib64/libnvidia-ml.so.1"}
	env := layout.ToolRequirement{Paths: []string{"/usr/bin/env"}, Exit: 127}
	python := layout.ToolRequirement{Paths: []string{"/usr/bin/python3", "/usr/local/bin/python3"}, Exit: 127, Stderr: "/usr/bin/env: 'python3': No such file or directory"}
	want := map[string]map[string][]layout.ToolRequirement{
		"nvidia-h100-nvl-8gpu":     {"nvidia-smi": {loader, {Paths: nvmlPaths, Exit: 12}}},
		"amd-instinct-mi300x-8gpu": {"rocm-smi": {env, python}, "amd-smi": {env, python}},
	}
	for name, tools := range want {
		p, tree := renderSharedBuiltin(t, name)
		raw := renderReadFile(t, tree, "state/tools.json")
		if strings.Contains(raw, `"src"`) {
			t.Errorf("%s: tools.json = %s carries a src; requirements reach it without their sources", name, raw)
		}
		var m layout.ToolsManifest
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, pt := range p.GPUs.Tools {
			if len(pt.Requires) == 0 {
				t.Errorf("%s: %s has no requirements", name, pt.Name)
			}
			for j, r := range pt.Requires {
				if r.Src == "" {
					t.Errorf("%s: %s requirement %d lost its src on load", name, pt.Name, j)
				}
			}
			got := m.Tools[pt.Name].Requires
			if len(got) != len(pt.Requires) {
				t.Fatalf("%s: tools.json %s has %d requirements, the profile %d", name, pt.Name, len(got), len(pt.Requires))
			}
			for j, r := range pt.Requires {
				if g := got[j]; !reflect.DeepEqual(g.Paths, r.Paths) || g.Exit != r.Exit || g.Stderr != r.Stderr {
					t.Errorf("%s: tools.json %s requirement %d = %+v, want the profile's %+v", name, pt.Name, j, g, r)
				}
			}
		}
		for tool, reqs := range tools {
			got := m.Tools[tool].Requires
			if len(got) != len(reqs) {
				t.Fatalf("%s: tools.json %s requirements = %+v, want %+v", name, tool, got, reqs)
			}
			for j, r := range reqs {
				g := got[j]
				if !reflect.DeepEqual(g.Paths, r.Paths) || g.Exit != r.Exit || (r.Stderr != "" && g.Stderr != r.Stderr) {
					t.Errorf("%s: tools.json %s requirement %d = %+v, want %+v", name, tool, j, g, r)
				}
			}
		}
	}

	// fakesmi over the rendered NVL tree: the loader alone gives the NVML
	// exit code with the profile's message; the loader and the second NVML
	// path let the agent's listing run.
	p, tree := renderSharedBuiltin(t, "nvidia-h100-nvl-8gpu")
	rootFS := t.TempDir()
	renderTouchUnder(t, rootFS, "/lib64/ld-linux-x86-64.so.2")
	e := smi.Env{SysRoot: tree.SysRoot, StateRoot: tree.StateRoot, RootFS: rootFS}
	var out, errOut bytes.Buffer
	if code := smi.Main([]string{"nvidia-smi", "-L"}, e, strings.NewReader(""), &out, &errOut); code != 12 || out.Len() != 0 ||
		errOut.String() != p.GPUs.Tools[0].Requires[1].Stderr+"\n" {
		t.Errorf("nvidia-smi -L without NVML = %d, stdout %q, stderr %q; want 12 and the profile's message", code, out.String(), errOut.String())
	}
	renderTouchUnder(t, rootFS, "/usr/lib64/libnvidia-ml.so.1")
	out.Reset()
	errOut.Reset()
	if code := smi.Main([]string{"nvidia-smi", "-L"}, e, strings.NewReader(""), &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "GPU 0: ") {
		t.Errorf("nvidia-smi -L with the loader and NVML = %d, stdout %q, stderr %q; want 0 and the GPU list", code, out.String(), errOut.String())
	}

	bad := []struct {
		name    string
		mutate  func(r *GPUToolRequirement)
		wantErr string
	}{
		{"no src", func(r *GPUToolRequirement) { r.Src = "" }, "gpus.tools[0].requires[1] has no src"},
		{"unknown source", func(r *GPUToolRequirement) { r.Src = "published:nosuchsource" }, "gpus.tools[0].requires[1]: provenance"},
		{"no paths", func(r *GPUToolRequirement) { r.Paths = nil }, "gpus.tools[0].requires[1] has no paths"},
		{"relative path", func(r *GPUToolRequirement) { r.Paths = []string{"usr/lib64/libnvidia-ml.so.1"} }, "is not a clean absolute path"},
		{"unclean path", func(r *GPUToolRequirement) { r.Paths = []string{"/usr/lib64/../lib64/libnvidia-ml.so.1"} }, "is not a clean absolute path"},
		{"exit 0", func(r *GPUToolRequirement) { r.Exit = 0 }, "exit 0 is not in [1, 255]"},
		{"exit 256", func(r *GPUToolRequirement) { r.Exit = 256 }, "exit 256 is not in [1, 255]"},
	}
	for _, c := range bad {
		profiles, err := BuiltinProfiles()
		if err != nil {
			t.Fatal(err)
		}
		q := profiles["nvidia-h100-nvl-8gpu"]
		if err := q.Validate(); err != nil {
			t.Fatalf("%s: the builtin profile does not validate: %v", c.name, err)
		}
		reqs := append([]GPUToolRequirement(nil), q.GPUs.Tools[0].Requires...)
		c.mutate(&reqs[1])
		q.GPUs.Tools[0].Requires = reqs
		if err := q.Validate(); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: Validate = %v, want an error containing %q", c.name, err, c.wantErr)
		}
	}
}

// renderTouchUnder creates the absolute path abs as an empty file under
// root.
func renderTouchUnder(t *testing.T, root, abs string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(abs))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}
