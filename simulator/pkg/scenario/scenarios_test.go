package scenario_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/matbun/joulie/pkg/controller/policy"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/scenario"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// The scenario library (tests/simcluster/scenarios) and the cluster specs it
// adds (tests/simcluster/specs), checked without a cluster: each scenario is
// valid, plans under the load limit, can place every job somewhere, and
// names as eco only Nodes the policy does put in eco.

const (
	libScenarioDir = "tests/simcluster/scenarios"
	libSpecDir     = "tests/simcluster/specs"
	libDocsPage    = "website/content/en/docs/simulator/scenarios.md"
	libConfigRef   = "website/content/en/docs/getting-started/05-configuration-reference.md"
)

// libScenarios is the library: a file added or removed must change this
// list, the docs page and the nightly workflow together.
var libScenarios = []string{"cap-cost", "deadlock-visible", "nightly-mixed", "perf-isolation", "smoke", "weekly-compare"}

func libRead(t *testing.T, name string) scenario.Scenario {
	t.Helper()
	repo := moduleRoot(t)
	path := filepath.Join(repo, libScenarioDir, name+".yaml")
	s, err := scenario.ReadScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := scenario.Validate(s, scenario.ValidateOptions{File: path, RepoRoot: repo}); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return s
}

var (
	libWorkloadgenOnce sync.Once
	libWorkloadgenBin  string
	libWorkloadgenErr  error
)

// libWorkloadgen builds workloadgen once for the tests that plan a
// generated trace.
func libWorkloadgen(t *testing.T) string {
	t.Helper()
	repo := moduleRoot(t)
	libWorkloadgenOnce.Do(func() {
		dir, err := os.MkdirTemp("", "scenario-lib-")
		if err != nil {
			libWorkloadgenErr = err
			return
		}
		libWorkloadgenBin = filepath.Join(dir, "workloadgen")
		build := exec.Command(goTool(), "build", "-o", libWorkloadgenBin, "./simulator/cmd/workloadgen")
		build.Dir = repo
		if out, err := build.CombinedOutput(); err != nil {
			libWorkloadgenErr = errors.New(err.Error() + "\n" + string(out))
		}
	})
	if libWorkloadgenErr != nil {
		t.Fatalf("build workloadgen: %v", libWorkloadgenErr)
	}
	return libWorkloadgenBin
}

// TestMain removes the directory of the workloadgen binary libWorkloadgen
// built, which outlives every test that uses it.
func TestMain(m *testing.M) {
	code := m.Run()
	if libWorkloadgenBin != "" {
		os.RemoveAll(filepath.Dir(libWorkloadgenBin))
	}
	os.Exit(code)
}

// libPlan plans scenario name into a temporary run directory and returns the
// plan and the trace's job records.
func libPlan(t *testing.T, name string) (*scenario.Plan, []scenario.TraceJob) {
	t.Helper()
	s := libRead(t, name)
	dir := filepath.Join(t.TempDir(), "run")
	p, err := scenario.MakePlan(context.Background(), s, scenario.PlanOptions{
		RepoRoot: moduleRoot(t), RunDir: dir, Workloadgen: libWorkloadgen(t),
	})
	if err != nil {
		t.Fatalf("%s: plan: %v", name, err)
	}
	first := scenario.VariantDir(dir, p.Effective.RunVariants()[0])
	jobs, err := scenario.ReadTrace(filepath.Join(first, workload.TraceFile))
	if err != nil {
		t.Fatal(err)
	}
	return p, jobs
}

// libSpec loads and expands a cluster spec of the repository.
func libSpec(t *testing.T, rel string) (*clusterspec.Spec, []clusterspec.Member) {
	t.Helper()
	s, err := clusterspec.LoadFile(filepath.Join(moduleRoot(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.Expand(clusterspec.Profiles(""))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return s, members
}

// The library holds exactly the scenarios above, each valid against the
// repository: its spec, its values files and its trace exist, and every
// rule of simscenario validate holds.
func TestScenarioLibraryValidates(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(moduleRoot(t), libScenarioDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, p := range paths {
		have = append(have, strings.TrimSuffix(filepath.Base(p), ".yaml"))
	}
	sort.Strings(have)
	if strings.Join(have, " ") != strings.Join(libScenarios, " ") {
		t.Errorf("%s holds %v, want %v", libScenarioDir, have, libScenarios)
	}
	for _, name := range have {
		t.Run(name, func(t *testing.T) { libRead(t, name) })
	}
}

// Every scenario plans without exceeding the offered load limit. The smoke
// scenario runs 9 jobs, and the two nightly ones offer between 0.5 and 0.8
// of each resource a job can saturate (the CPUs and both GPU kinds), so the
// cluster is busy without queueing past the drain.
func TestScenarioLibraryPlansUnderTheLoadLimit(t *testing.T) {
	for _, name := range libScenarios {
		t.Run(name, func(t *testing.T) {
			p, jobs := libPlan(t, name)
			if len(jobs) == 0 || p.Jobs != len(jobs) {
				t.Fatalf("plan kept %d jobs, the trace holds %d", p.Jobs, len(jobs))
			}
			if name == "smoke" && p.Jobs != 9 {
				t.Errorf("smoke plans %d jobs, want 9", p.Jobs)
			}
			if name != "nightly-mixed" && name != "weekly-compare" {
				return
			}
			load := map[string]float64{}
			for _, l := range p.Load {
				load[l.Resource] = l.Load
			}
			for _, r := range []string{"cpu", "nvidia.com/gpu", "amd.com/gpu"} {
				if l, ok := load[r]; !ok || l < 0.5 || l > 0.8 {
					t.Errorf("offered load of %s is %.3f (present %t), want 0.5 to 0.8", r, l, ok)
				}
			}
		})
	}
}

// The specs the library adds load with builtin profiles and carry no
// baseLoad: a v1 run's Nodes idle until its jobs load them.
func TestScenarioSpecsLoadWithoutBaseLoad(t *testing.T) {
	for _, name := range []string{"policy.yaml", "nightly.yaml"} {
		t.Run(name, func(t *testing.T) {
			s, members := libSpec(t, filepath.Join(libSpecDir, name))
			if s.Spec.Defaults.BaseLoad != nil {
				t.Errorf("spec.defaults sets baseLoad")
			}
			for _, p := range s.Spec.Pools {
				if p.BaseLoad != nil {
					t.Errorf("pool %s sets baseLoad", p.Name)
				}
			}
			if len(members) == 0 {
				t.Fatal("no Nodes")
			}
		})
	}
}

// libTolerates reports whether the tolerations tolerate every NoSchedule
// and NoExecute taint.
func libTolerates(tols []corev1.Toleration, taints []corev1.Taint) bool {
	for i := range taints {
		if taints[i].Effect != corev1.TaintEffectNoSchedule && taints[i].Effect != corev1.TaintEffectNoExecute {
			continue
		}
		ok := false
		for j := range tols {
			if tols[j].ToleratesTaint(&taints[i]) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// Every job of every scenario fits some Node of its spec on its own: the
// Node has the resources, matches the job's node selector and the run's
// default one, and the job tolerates its taints. k3s adds no toleration for
// an extended resource (no ExtendedResourceToleration admission), so a GPU
// job that does not tolerate its vendor's taint would stay Pending forever.
func TestEveryScenarioJobFitsSomeNode(t *testing.T) {
	for _, name := range libScenarios {
		t.Run(name, func(t *testing.T) {
			p, jobs := libPlan(t, name)
			_, members := libSpec(t, p.Effective.Cluster.Spec)
			runTols := []corev1.Toleration{workload.KWOKToleration()}
			if pl := p.Effective.Workload.Placement; pl != nil && pl.Tolerations != nil {
				runTols = pl.Tolerations
			}
			var cluster string
			if len(p.Variants) > 0 {
				cluster = p.Variants[0].ClusterName
			}
			selector := workload.DefaultNodeSelector(cluster)
			if pl := p.Effective.Workload.Placement; pl != nil && pl.NodeSelector != nil {
				selector = pl.NodeSelector
			}
			for _, j := range jobs {
				tols := append(append([]corev1.Toleration(nil), j.PodTemplate.Tolerations...), runTols...)
				fits := false
				for _, m := range members {
					if libFits(m.Node, j, selector, tols) {
						fits = true
						break
					}
				}
				if !fits {
					t.Errorf("job %s (requests %v, node selector %v) fits no Node of %s", j.JobID, j.PodTemplate.Requests, j.PodTemplate.NodeSelector, p.Effective.Cluster.Spec)
				}
			}
		})
	}
}

func libFits(n *corev1.Node, j scenario.TraceJob, runSelector map[string]string, tols []corev1.Toleration) bool {
	for _, sel := range []map[string]string{runSelector, j.PodTemplate.NodeSelector} {
		for k, v := range sel {
			if n.Labels[k] != v {
				return false
			}
		}
	}
	for r, q := range j.PodTemplate.Requests {
		want, err := resource.ParseQuantity(q)
		if err != nil {
			return false
		}
		have, ok := n.Status.Allocatable[corev1.ResourceName(r)]
		if !ok || have.Cmp(want) < 0 {
			return false
		}
	}
	return libTolerates(tols, n.Spec.Taints)
}

// The scenarios that pin work to a Node by name, all to an eco Node, or read
// a Node's energy, name a Node that the policy they set does put in eco at
// the first reconcile: policy.BuildStaticPlan over the spec's Nodes with their
// hardware families, the scenario's STATIC_HP_FRAC and no labels yet. Each
// family must also keep a performance Node, which cap-cost's reference
// needs.
func TestEcoNodesTheScenariosNameAreEco(t *testing.T) {
	for _, name := range []string{"perf-isolation", "cap-cost"} {
		t.Run(name, func(t *testing.T) {
			s := libRead(t, name)
			if got := s.Joulie.Set["controllerManager.env.POLICY_TYPE"]; got != "static_partition" {
				t.Fatalf("POLICY_TYPE %q: the scenario must pin static_partition, whose eco set is fixed", got)
			}
			frac, err := strconv.ParseFloat(string(s.Joulie.Set["controllerManager.env.STATIC_HP_FRAC"]), 64)
			if err != nil {
				t.Fatalf("STATIC_HP_FRAC: %v; the scenario must pin it", err)
			}
			_, members := libSpec(t, s.Cluster.Spec)
			hw := map[string]policy.NodeHardwareInfo{}
			var nodes []string
			for _, m := range members {
				nodes = append(nodes, m.Name)
				info := policy.NodeHardwareInfo{CPUModel: m.Profile.CPU.ModelName.V}
				if g := m.Profile.GPUs; g != nil {
					info.GPUModel, info.GPUCount = g.Product.V, g.Count.V
				}
				hw[m.Name] = info
			}
			plan := policy.BuildStaticPlan(nodes, hw, nil, 0, 0, frac)
			profile := map[string]string{}
			families := map[string]map[string]bool{}
			for _, a := range plan {
				profile[a.NodeName] = a.Profile
				f := policy.NodeFamily(a.NodeName, hw)
				if families[f] == nil {
					families[f] = map[string]bool{}
				}
				families[f][a.Profile] = true
			}
			for f, profiles := range families {
				if !profiles["eco"] || !profiles["performance"] {
					t.Errorf("family %s has profiles %v, want both eco and performance", f, profiles)
				}
			}
			var named []string
			for _, j := range s.Workload.Jobs {
				if n := j.NodeSelector[corev1.LabelHostname]; n != "" {
					named = append(named, n)
				}
			}
			for _, a := range s.Assertions {
				if a.Energy != nil {
					if kind, n, err := scenario.ParseEnergyScope(a.Energy.Scope); err == nil && kind == scenario.EnergyScopeNode {
						named = append(named, n)
					}
				}
			}
			for _, n := range named {
				if profile[n] != "eco" {
					t.Errorf("the scenario names %s as an eco Node, the plan gives it %q", n, profile[n])
				}
			}
		})
	}
}

// deadlock-visible's gang cannot run whole: the Node its pods are pinned to
// fits one pod's GPUs but not the gang's.
func TestDeadlockGangCannotFitItsNode(t *testing.T) {
	s := libRead(t, "deadlock-visible")
	if len(s.Workload.Jobs) != 1 {
		t.Fatalf("%d job kinds, want the one gang", len(s.Workload.Jobs))
	}
	j := s.Workload.Jobs[0]
	if !j.Gang || j.Pods < 2 || j.GPUs == nil {
		t.Fatalf("job %s: gang %t, pods %d, gpus %v; want a GPU gang of at least 2 pods", j.Name, j.Gang, j.Pods, j.GPUs)
	}
	host := j.NodeSelector[corev1.LabelHostname]
	_, members := libSpec(t, s.Cluster.Spec)
	for _, m := range members {
		if m.Name != host {
			continue
		}
		q := m.Node.Status.Allocatable[corev1.ResourceName(j.GPUs.Resource)]
		have := q.Value()
		if have < int64(j.GPUs.Count) || have >= int64(j.GPUs.Count*j.Pods) {
			t.Errorf("%s has %d %s: one pod needs %d, the gang %d; want room for one pod and not the gang", host, have, j.GPUs.Resource, j.GPUs.Count, j.GPUs.Count*j.Pods)
		}
		return
	}
	t.Fatalf("the gang is pinned to %q, which %s does not have", host, s.Cluster.Spec)
}

// The scenarios page names every scenario of the library.
func TestScenarioLibraryIsDocumented(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), libDocsPage))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range libScenarios {
		if !strings.Contains(string(b), "`"+name+"`") {
			t.Errorf("%s does not name the scenario `%s`", libDocsPage, name)
		}
	}
}

// The configuration reference documents every variable of the workload
// engine (workload.EnvNames).
func TestWorkloadEnvVarsAreDocumented(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), libConfigRef))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range workload.EnvNames() {
		if !strings.Contains(string(b), "`"+name+"`") {
			t.Errorf("%s does not document %s", libConfigRef, name)
		}
	}
}
