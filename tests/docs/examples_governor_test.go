package docs

// The governor example (examples/12-governor-presets) is what an adopter
// copies to turn the governors on. Its files spell the frozen preset words,
// the two gates and a cluster spec for the simulated cluster, so a renamed
// preset, a spec the harness refuses or a values file that no longer turns
// both gates on fails here instead of in a reader's cluster.

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
)

// geExampleDir is the example, relative to the repository root.
const geExampleDir = "examples/12-governor-presets"

// geChartValues holds the chart defaults the example spells out.
const geChartValues = "charts/joulie/values.yaml"

// geNameWords is the allowlist of the words a pool name of the example may
// use: vendors, product lines and models of the builtin profiles, and the
// drivers that tell CPU pools apart. A Node name is a pool name plus an
// index. An allowlist, because a denylist of real cluster, pool or node names
// would itself put those names in the repository. It is the list GX05 holds
// the Tier B spec to (gvNameWords in geGX05File), word for word, so the
// example and the experiment name pools from one list;
// TestGENameWordsAreGX05s keeps the two equal.
var geNameWords = []string{
	"amd", "intel", "nvidia", "epyc", "xeon", "xeon6", "9534", "6530", "hsmp", "rapl", "tpmi",
	"radeon", "pro", "w7900", "instinct", "mi300x", "h100", "nvl", "sxm", "l40s", "vm", "cpu", "gpu",
}

// geGX05File holds gvNameWords, the allowlist of GX05.
const geGX05File = "tests/simcluster/governor_test.go"

var geNameWord = `(` + strings.Join(geNameWords, "|") + `)`

var (
	gePoolNameRE = regexp.MustCompile(`^` + geNameWord + `(-` + geNameWord + `)*$`)
	geNodeNameRE = regexp.MustCompile(`^` + geNameWord + `(-` + geNameWord + `)*-[0-9]+$`)
)

// geGates are the two gates, one per component: the controller manager
// writes presets only with GOVERNOR_CONTROL on, the agent applies them only
// with GOVERNOR_WRITES true.
const (
	geControlKey = "GOVERNOR_CONTROL"
	geWritesKey  = "GOVERNOR_WRITES"
)

// geGovernorKey reports whether an environment variable of the chart is a
// governor setting the example spells out.
func geGovernorKey(name string) bool {
	return strings.HasPrefix(name, "GOVERNOR_") || strings.HasPrefix(name, "CPU_GOVERNOR_") || strings.HasPrefix(name, "GPU_GOVERNOR_")
}

// geValues is the part of a Joulie values file the example sets.
type geValues struct {
	ControllerManager struct {
		Env map[string]string `json:"env"`
	} `json:"controllerManager"`
	Agent struct {
		Env map[string]string `json:"env"`
	} `json:"agent"`
}

func geReadFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func geReadValues(t *testing.T, rel string) geValues {
	t.Helper()
	var v geValues
	if err := yaml.Unmarshal(geReadFile(t, rel), &v); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return v
}

// The example's allowlist is GX05's: the words of gvNameWords, in its order.
func TestGENameWordsAreGX05s(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), geGX05File, geReadFile(t, geGX05File), 0)
	if err != nil {
		t.Fatal(err)
	}
	var gx05 []string
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "gvNameWords" || len(vs.Values) != 1 {
			return true
		}
		found = true
		lit, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			t.Fatalf("%s: gvNameWords is not a composite literal", geGX05File)
		}
		for _, el := range lit.Elts {
			bl, ok := el.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				t.Fatalf("%s: gvNameWords holds %T, not a string literal", geGX05File, el)
			}
			w, err := strconv.Unquote(bl.Value)
			if err != nil {
				t.Fatal(err)
			}
			gx05 = append(gx05, w)
		}
		return false
	})
	if !found {
		t.Fatalf("%s declares no gvNameWords", geGX05File)
	}
	if !slices.Equal(geNameWords, gx05) {
		t.Fatalf("geNameWords %v, GX05's gvNameWords %v: keep one list", geNameWords, gx05)
	}
}

// GE01: nodetwin-governor.yaml decodes into the typed NodeTwin, with no
// field the CRD does not have, and its presets are words of
// CPUGovernorPresetValues and GPUGovernorPresetValues. make test-examples
// also dry-runs it, since it has apiVersion and kind.
func TestGE01NodeTwinExampleDecodesWithKnownPresets(t *testing.T) {
	rel := filepath.Join(geExampleDir, "nodetwin-governor.yaml")
	raw := geReadFile(t, rel)
	if !regexp.MustCompile(`(?m)^apiVersion:`).Match(raw) || !regexp.MustCompile(`(?m)^kind:`).Match(raw) {
		t.Errorf("%s has no top level apiVersion or kind, so make test-examples skips it", rel)
	}
	js, err := yaml.YAMLToJSONStrict(raw)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	// sigs.k8s.io/json matches keys case-sensitively, as the API server
	// does; yaml.UnmarshalStrict would take Preset for preset.
	var nt v1alpha1.NodeTwin
	strict, err := sigsjson.UnmarshalStrict(js, &nt, sigsjson.DisallowDuplicateFields, sigsjson.DisallowUnknownFields)
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	if len(strict) > 0 {
		t.Fatalf("%s: %v", rel, errors.Join(strict...))
	}

	if nt.APIVersion != v1alpha1.GroupVersion.String() || nt.Kind != "NodeTwin" {
		t.Errorf("%s: apiVersion %q kind %q, want %s NodeTwin", rel, nt.APIVersion, nt.Kind, v1alpha1.GroupVersion)
	}
	if want := joulie.ObjectNameForNode(nt.Spec.NodeName); nt.Name != want {
		t.Errorf("%s: metadata.name %q, want %q, the object name of spec.nodeName %q", rel, nt.Name, want, nt.Spec.NodeName)
	}

	if nt.Spec.CPU == nil || nt.Spec.CPU.Governor == nil {
		t.Errorf("%s shows no spec.cpu.governor", rel)
	} else {
		g := nt.Spec.CPU.Governor
		if !slices.Contains(joulie.CPUGovernorPresetValues, g.Preset) {
			t.Errorf("%s: spec.cpu.governor.preset %q is not one of %v", rel, g.Preset, joulie.CPUGovernorPresetValues)
		}
		if g.Boost != "" && !slices.Contains(joulie.CPUGovernorBoostValues, g.Boost) {
			t.Errorf("%s: spec.cpu.governor.boost %q is not one of %v", rel, g.Boost, joulie.CPUGovernorBoostValues)
		}
	}

	if nt.Spec.GPU == nil || nt.Spec.GPU.Governor == nil {
		t.Errorf("%s shows no spec.gpu.governor", rel)
	} else {
		g := nt.Spec.GPU.Governor
		if !slices.Contains(joulie.GPUGovernorPresetValues, g.Preset) {
			t.Errorf("%s: spec.gpu.governor.preset %q is not one of %v", rel, g.Preset, joulie.GPUGovernorPresetValues)
		}
		if c := g.ComputeClockCeilingPctOfMax; c != nil {
			if g.Preset != joulie.GPUGovernorPresetEfficient {
				t.Errorf("%s: computeClockCeilingPctOfMax is set with preset %q; it is read only with %s", rel, g.Preset, joulie.GPUGovernorPresetEfficient)
			}
			if *c <= 0 || *c > 100 {
				t.Errorf("%s: computeClockCeilingPctOfMax %g is not in (0, 100]", rel, *c)
			}
		}
	}
}

// GE02: cluster-spec.yaml validates and expands with clusterspec, as
// make simcluster-up does, and its pool and Node names pass the allowlist;
// values.yaml sets both gates explicitly and on, spells every other
// governor setting at the chart default the README describes, and renders
// with helm template on its own.
func TestGE02GovernorExampleSpecAndValues(t *testing.T) {
	t.Run("cluster spec", func(t *testing.T) {
		rel := filepath.Join(geExampleDir, "cluster-spec.yaml")
		s, err := clusterspec.LoadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		members, err := s.Expand(clusterspec.Profiles(""))
		if err != nil {
			t.Fatalf("%s does not expand, so make simcluster-up refuses it: %v", rel, err)
		}
		for _, pool := range s.Spec.Pools {
			if !gePoolNameRE.MatchString(pool.Name) {
				t.Errorf("%s: pool name %q is not vendor or model words joined by hyphens (%s)", rel, pool.Name, gePoolNameRE)
			}
		}
		for _, m := range members {
			if !geNodeNameRE.MatchString(m.Name) {
				t.Errorf("%s: Node name %q is not vendor or model words plus an index (%s)", rel, m.Name, geNodeNameRE)
			}
		}
	})

	t.Run("values", func(t *testing.T) {
		rel := filepath.Join(geExampleDir, "values.yaml")
		v := geReadValues(t, rel)
		control, ok := v.ControllerManager.Env[geControlKey]
		if !ok || !slices.Contains([]string{"cpu", "gpu", "all"}, control) {
			t.Errorf("%s: controllerManager.env.%s is %q (set %t), want cpu, gpu or all, set explicitly", rel, geControlKey, control, ok)
		}
		if writes, ok := v.Agent.Env[geWritesKey]; !ok || writes != "true" {
			t.Errorf("%s: agent.env.%s is %q (set %t), want \"true\", set explicitly", rel, geWritesKey, writes, ok)
		}

		chart := geReadValues(t, geChartValues)
		for _, c := range []struct {
			component      string
			example, chart map[string]string
		}{
			{"controllerManager", v.ControllerManager.Env, chart.ControllerManager.Env},
			{"agent", v.Agent.Env, chart.Agent.Env},
		} {
			for _, k := range sortedKeys(c.chart) {
				if !geGovernorKey(k) || k == geControlKey || k == geWritesKey {
					continue
				}
				got, ok := c.example[k]
				if !ok {
					t.Errorf("%s: %s.env.%s is not set; the example spells out every governor setting (chart default %q)", rel, c.component, k, c.chart[k])
					continue
				}
				if got != c.chart[k] {
					t.Errorf("%s: %s.env.%s is %q, but the chart default is %q; the example shows the default presets, so update one of them and the README", rel, c.component, k, got, c.chart[k])
				}
			}
			for _, k := range sortedKeys(c.example) {
				if _, ok := c.chart[k]; geGovernorKey(k) && !ok {
					t.Errorf("%s: %s.env.%s is not a setting of %s", rel, c.component, k, geChartValues)
				}
			}
		}

		helm, err := exec.LookPath("helm")
		if err != nil {
			t.Skip("helm is not on PATH")
		}
		cmd := exec.Command(helm, "template", "joulie", "charts/joulie", "-f", rel)
		cmd.Dir = repoRoot
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		render, err := cmd.Output()
		if err != nil {
			t.Fatalf("helm template joulie charts/joulie -f %s: %v\n%s", rel, err, stderr.String())
		}
		env := geRenderedEnv(t, render)
		for _, c := range []struct{ container, key, want string }{
			{"controller-manager", geControlKey, control},
			{"agent", geWritesKey, "true"},
		} {
			got, ok := env[c.container][c.key]
			if !ok || got != c.want {
				t.Errorf("rendered %s container: %s=%q (set %t), want %q", c.container, c.key, got, ok, c.want)
			}
		}
	})
}

// geRenderedEnv returns the literal environment of every container of the
// workloads in a helm render, by container name.
func geRenderedEnv(t *testing.T, render []byte) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(render), 4096)
	for {
		var doc struct {
			Spec struct {
				Template corev1.PodTemplateSpec `json:"template"`
			} `json:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode helm render: %v", err)
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			if out[c.Name] == nil {
				out[c.Name] = map[string]string{}
			}
			for _, e := range c.Env {
				if e.ValueFrom == nil {
					out[c.Name][e.Name] = e.Value
				}
			}
		}
	}
	return out
}
