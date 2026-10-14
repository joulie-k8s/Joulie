package contracts_test

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/scheduler/powerest"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// GPU resource names are a contract between the device plugins, the
// emulator that renders their resources, and every Joulie component that
// tells a GPU from them: the controller manager (presence and count), the
// agent (the GPU cap gate and the count without tools), the scheduler
// extender (a pod's GPU demand) and kubectl-joulie (the GPU columns). All
// of them classify through pkg/api.GPUResourceKind.

// GR01: every GPU resource the emulator renders, for every exposure of
// every builtin GPU profile, is a GPU resource, and every exposure but DRA
// tells a GPU node from its resources or its labels; a DRA node advertises
// no resource, so it is a GPU node when GPU feature discovery labels its
// count or the agent lists its GPUs. Where GPU feature discovery labels a
// physical count, it is the profile's GPU count; under the single MIG
// strategy it counts MIG devices, so no label states it.
func TestGR01EveryRenderedGPUExposureIsAGPUNode(t *testing.T) {
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for name, base := range profiles {
		if base.GPUs == nil {
			continue
		}
		for _, mode := range hwemu.Exposures(base.GPUs.Vendor) {
			fresh, err := hwemu.BuiltinProfiles()
			if err != nil {
				t.Fatal(err)
			}
			p := fresh[name]
			p.GPUs.Exposure = hwemu.Fact[string]{V: mode, Src: "assumed:tests/contracts GR01 variant"}
			switch mode {
			case "mig-single", "mig-mixed":
				p.GPUs.MIGProfile, p.GPUs.MIGDevicesPerGPU = "1g.12gb", 7
			case "timeslice-rename", "timeslice-norename":
				p.GPUs.TimesliceReplicas = 4
			}
			if err := p.Validate(); err != nil {
				t.Errorf("%s with exposure %s: %v", name, mode, err)
				continue
			}
			n := p.NodeObject("n")
			var units int64
			for res, q := range n.Status.Allocatable {
				if !strings.Contains(string(res), "/") {
					continue
				}
				if joulie.GPUResourceKind(string(res)) == "" {
					t.Errorf("%s %s: resource %s is not a GPU resource for pkg/api.GPUResourceKind", name, mode, res)
				}
				units += q.Value()
			}
			count, labelled := joulie.GPUCountFromLabels(n.Labels)
			if units <= 0 && !labelled && mode != "dra" {
				t.Errorf("%s %s: no GPU resource and no GPU count label, so no component sees a GPU node", name, mode)
			}
			if p.GPUs.Labeller.V == "gfd" && mode != "mig-single" && (!labelled || count != p.GPUs.Count.V) {
				t.Errorf("%s %s: GPU count from labels %d (%v), want the profile's %d", name, mode, count, labelled, p.GPUs.Count.V)
			}
			if mode == "mig-single" && labelled {
				t.Errorf("%s %s: GPU count %d from labels, want none: nvidia.com/gpu.count counts MIG devices", name, mode, count)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no builtin GPU profile")
	}
}

// GR02: every GPU resource a scenario job may request (gpus.resource,
// simulator/pkg/scenario) and every name a device plugin advertises is a
// GPU resource, and a pod requesting one unit of it has a positive GPU
// demand at the scheduler extender.
func TestGR02EveryGPUResourceIsAPodGPUDemand(t *testing.T) {
	for _, res := range []string{
		// The scenario schema's gpus.resource values.
		"nvidia.com/gpu", "amd.com/gpu", "nvidia.com/gpu.shared", "nvidia.com/mig-1g.12gb", "nvidia.com/mig-1g.10gb+me",
		// The other names of the device plugins.
		"nvidia.com/mig-3g.40gb.shared", "amd.com/cpx_nps4", "amd.com/spx_nps1", "gpu.intel.com/i915", "gpu.intel.com/xe",
	} {
		if joulie.GPUResourceKind(res) == "" {
			t.Errorf("%s is not a GPU resource for pkg/api.GPUResourceKind", res)
			continue
		}
		req := map[string]interface{}{res: "1"}
		pod := map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{
			map[string]interface{}{"resources": map[string]interface{}{"requests": req, "limits": req}},
		}}}
		if d := powerest.ExtractPodDemand(pod, "standard"); d.GPUCount <= 0 {
			t.Errorf("a pod requesting 1 %s has GPU demand %v at the extender, want more than 0", res, d.GPUCount)
		}
	}
}

// gpuSuffixMatcher is a GPU resource matcher written by hand: a test of a
// resource name for the /gpu suffix.
var gpuSuffixMatcher = regexp.MustCompile(`HasSuffix\([^)]*"/gpu"\)`)

// gpuResourceLiteral is a GPU resource name spelled in a string literal, a
// list of GPU resources or a lookup of one by name. The components (cmd/
// and pkg/) look GPU resources up through pkg/api; the simulator writes
// these names on the objects it renders and is outside this rule.
var gpuResourceLiteral = regexp.MustCompile(`"(nvidia\.com/gpu|amd\.com/gpu|gpu\.intel\.com/(i915|xe))"`)

// GR03: GPU resource names are classified only in pkg/api: no hand written
// /gpu suffix test anywhere, and no GPU resource name literal in the
// components.
func TestGR03GPUResourcesAreClassifiedOnlyInPkgAPI(t *testing.T) {
	root := repoRoot(t)
	apiDir := filepath.Join(root, "pkg", "api")
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" || name == "node_modules") || path == apiDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		component := strings.HasPrefix(rel, "cmd/") || strings.HasPrefix(rel, "pkg/")
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 1<<20)
		for line := 1; sc.Scan(); line++ {
			if gpuSuffixMatcher.MatchString(sc.Text()) || component && gpuResourceLiteral.MatchString(sc.Text()) {
				found = append(found, rel+":"+intToStr(line)+": "+strings.TrimSpace(sc.Text()))
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Errorf("%s\n\ta GPU resource matcher or name outside pkg/api; use joulie.GPUResourceKind", f)
	}
}
