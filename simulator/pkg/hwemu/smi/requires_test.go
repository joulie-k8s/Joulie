package smi

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// The requirements of the builtin profiles (nvidia-h100-nvl-8gpu and
// amd-instinct-mi300x-8gpu), which TestRenderToolsRequiresNeedSrc in
// package hwemu checks reach tools.json. The NVML message is a test string;
// fakesmi prints whatever the entry says.
var (
	reqLoader = layout.ToolRequirement{Paths: []string{"/lib64/ld-linux-x86-64.so.2"}, Exit: 127}
	reqNVML   = layout.ToolRequirement{Paths: []string{"/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1", "/usr/lib64/libnvidia-ml.so.1"}, Exit: 12, Stderr: "test: NVML Shared Library couldn't be found"}
	reqEnv    = layout.ToolRequirement{Paths: []string{"/usr/bin/env"}, Exit: 127}
	reqPython = layout.ToolRequirement{Paths: []string{"/usr/bin/python3", "/usr/local/bin/python3"}, Exit: 127, Stderr: "/usr/bin/env: 'python3': No such file or directory"}
)

// requireTools rewrites tools.json with entries, requirements included.
func (n *testNode) requireTools(entries map[string]layout.ToolEntry) {
	n.t.Helper()
	b, err := json.Marshal(layout.ToolsManifest{SchemaVersion: 1, Tools: entries})
	if err != nil {
		n.t.Fatal(err)
	}
	n.write(path.Join(layout.StateDir, layout.ToolsFile), string(b), 0o444)
}

// newRootFS is a temporary root file system holding the absolute paths files
// as empty files, never the host's /.
func newRootFS(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// newRequiringNVLNode is newNVLNode with nvidia-smi requiring the loader,
// then NVML, as the H100 NVL profile lists them.
func newRequiringNVLNode(t *testing.T) *testNode {
	n := newNVLNode(t)
	n.requireTools(map[string]layout.ToolEntry{"nvidia-smi": {Variant: "default", Requires: []layout.ToolRequirement{reqLoader, reqNVML}}})
	return n
}

// The loader is present and NVML is not: nvidia-smi fails with its return
// code 12 and the entry's message, and writes nothing to stdout. Either NVML
// path satisfies the requirement, and with one present the call runs.
func TestS17NvidiaSMIWithoutNVMLExits12(t *testing.T) {
	n := newRequiringNVLNode(t)
	n.env.RootFS = newRootFS(t, reqLoader.Paths[0])
	code, stdout, stderr := n.main("", agentNvidiaQuery...)
	if code != 12 || stdout != "" || stderr != reqNVML.Stderr+"\n" {
		t.Errorf("query without NVML = %d, stdout %q, stderr %q; want 12, nothing and %q", code, stdout, stderr, reqNVML.Stderr)
	}
	out, err := n.run(agentNvidiaQuery...)
	if exitCode(t, err) != 12 || string(out) != reqNVML.Stderr+"\n" {
		t.Errorf("Runner query without NVML = %v, output %q; want exit 12 and the message", err, out)
	}
	if got := n.read(nvmlRel(0, "power_limit_mw")); got != "400000" {
		t.Fatalf("power_limit_mw = %s before the set", got)
	}
	if code, _, _ := n.main("", agentNvidiaSet(0, 300)...); code != 12 {
		t.Errorf("-pl without NVML = %d, want 12", code)
	}
	if got := n.read(nvmlRel(0, "power_limit_mw")); got != "400000" {
		t.Errorf("-pl without NVML wrote power_limit_mw = %s, want it untouched", got)
	}

	for _, lib := range reqNVML.Paths {
		n.env.RootFS = newRootFS(t, reqLoader.Paths[0], lib)
		code, stdout, stderr := n.main("", agentNvidiaQuery...)
		if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "0, 200.00, 400.00, 400.00, 50.00, NVIDIA H100 NVL\n") {
			t.Errorf("query with %s = %d, stdout %q, stderr %q; want the S1 answer", lib, code, stdout, stderr)
		}
	}
}

// Without the ELF loader the kernel cannot exec nvidia-smi at all, so the
// loader is checked first: 127 and no message, whether NVML is present or
// not, and a dangling loader link counts as absent.
func TestS18NvidiaSMIWithoutLoaderExits127First(t *testing.T) {
	n := newRequiringNVLNode(t)
	for _, files := range [][]string{{reqNVML.Paths[0]}, nil} {
		n.env.RootFS = newRootFS(t, files...)
		code, stdout, stderr := n.main("", agentNvidiaList...)
		if code != 127 || stdout != "" || stderr != "" {
			t.Errorf("-L with %v = %d, stdout %q, stderr %q; want 127 and nothing", files, code, stdout, stderr)
		}
	}
	n.env.RootFS = newRootFS(t, reqNVML.Paths[0])
	loader := filepath.Join(n.env.RootFS, filepath.FromSlash(reqLoader.Paths[0]))
	if err := os.MkdirAll(filepath.Dir(loader), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ld-linux-x86-64.so.2.absent", loader); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := n.main("", agentNvidiaList...); code != 127 || stderr != "" {
		t.Errorf("-L with a dangling loader link = %d, stderr %q; want 127", code, stderr)
	}
	// Called as fakesmi nvidia-smi, the same checks apply.
	if code, _, _ := n.main("", append([]string{"fakesmi"}, agentNvidiaList...)...); code != 127 {
		t.Errorf("fakesmi nvidia-smi -L without the loader = %d, want 127", code)
	}
}

// rocm-smi and amd-smi are scripts run through /usr/bin/env python3. With
// env but no python3, env's message and 127; without env, 127 and nothing;
// with either python3 path, the call runs.
func TestS19RocmSMIWithoutPythonExits127(t *testing.T) {
	n := newMI300XNode(t, "current", "7.2")
	py := []layout.ToolRequirement{reqEnv, reqPython}
	n.requireTools(map[string]layout.ToolEntry{"rocm-smi": {Variant: "current", Requires: py}, "amd-smi": {Variant: "7.2", Requires: py}})
	calls := [][]string{agentRocmProduct, {"amd-smi", "static", "-g", "0", "--limit", "--json"}}

	n.env.RootFS = newRootFS(t, reqEnv.Paths[0])
	for _, argv := range calls {
		code, stdout, stderr := n.main("", argv...)
		if code != 127 || stdout != "" || stderr != reqPython.Stderr+"\n" {
			t.Errorf("%v without python3 = %d, stdout %q, stderr %q; want 127 and %q", argv, code, stdout, stderr, reqPython.Stderr)
		}
	}
	n.env.RootFS = newRootFS(t, reqPython.Paths...)
	for _, argv := range calls {
		if code, stdout, stderr := n.main("", argv...); code != 127 || stdout != "" || stderr != "" {
			t.Errorf("%v without /usr/bin/env = %d, stdout %q, stderr %q; want 127 and nothing", argv, code, stdout, stderr)
		}
	}
	for _, python := range reqPython.Paths {
		n.env.RootFS = newRootFS(t, reqEnv.Paths[0], python)
		for _, argv := range calls {
			if code, stdout, stderr := n.main("", argv...); code != 0 || stdout == "" || stderr != "" {
				t.Errorf("%v with %s = %d, stdout %q, stderr %q; want it to run", argv, python, code, stdout, stderr)
			}
		}
	}
}

// An empty RootFS checks nothing, so the in-process Runner the agent tests
// use (cmd/agent hwemu_helpers_test.go) answers a manifest whose
// requirements exist nowhere as it answers one without requirements. EnvFromOS gives
// / unless HWEMU_ROOTFS says otherwise, so fakesmi in a pod checks the pod's
// own files.
func TestS20EmptyRootFSBehavesAsS1(t *testing.T) {
	absent := layout.ToolRequirement{Paths: []string{"/nonexistent/hwemu-s20/loader"}, Exit: 99, Stderr: "must not print"}
	nvidiaCalls := [][]string{agentNvidiaList, agentNvidiaQuery, {"nvidia-smi", "--bogus"}}
	amdCalls := [][]string{agentRocmProduct, agentRocmInventory}

	plain, requiring := newNVLNode(t), newNVLNode(t)
	requiring.requireTools(map[string]layout.ToolEntry{"nvidia-smi": {Variant: "default", Requires: []layout.ToolRequirement{absent, reqLoader, reqNVML}}})
	s20Compare(t, plain, requiring, nvidiaCalls)

	plain, requiring = newMI300XNode(t, "current", "7.2"), newMI300XNode(t, "current", "7.2")
	requiring.requireTools(map[string]layout.ToolEntry{
		"rocm-smi": {Variant: "current", Requires: []layout.ToolRequirement{absent, reqEnv, reqPython}},
		"amd-smi":  {Variant: "7.2", Requires: []layout.ToolRequirement{absent}},
	})
	s20Compare(t, plain, requiring, amdCalls)

	t.Setenv("HWEMU_ROOTFS", "")
	if got := EnvFromOS().RootFS; got != "/" {
		t.Errorf("EnvFromOS().RootFS with HWEMU_ROOTFS empty = %q, want /", got)
	}
	t.Setenv("HWEMU_ROOTFS", "/rootfs")
	if got := EnvFromOS().RootFS; got != "/rootfs" {
		t.Errorf("EnvFromOS().RootFS = %q, want HWEMU_ROOTFS /rootfs", got)
	}
}

// s20Compare runs each call through Main and through Runner on both nodes,
// whose RootFS is empty, and wants the same exit code and bytes.
func s20Compare(t *testing.T, plain, requiring *testNode, calls [][]string) {
	t.Helper()
	if requiring.env.RootFS != "" {
		t.Fatalf("RootFS = %q, want empty", requiring.env.RootFS)
	}
	for _, argv := range calls {
		pc, po, pe := plain.main("", argv...)
		rc, ro, re := requiring.main("", argv...)
		if pc != rc || po != ro || pe != re {
			t.Errorf("Main %v with requirements = %d %q %q, without = %d %q %q", argv, rc, ro, re, pc, po, pe)
		}
		pout, perr := plain.run(argv...)
		rout, rerr := requiring.run(argv...)
		if exitCode(t, perr) != exitCode(t, rerr) || string(pout) != string(rout) {
			t.Errorf("Runner %v with requirements = %v %q, without = %v %q", argv, rerr, rout, perr, pout)
		}
	}
}
