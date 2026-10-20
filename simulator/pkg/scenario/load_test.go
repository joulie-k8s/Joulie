package scenario_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/scenario"
)

// The planner's pools hold the Nodes clusterspec.Expand renders: the same
// labels (rack, cooling zone and host name included), taints and
// allocatable resources, grouped by pool in the spec's order.
func TestReadSpecPoolsHoldsTheExpandedNodes(t *testing.T) {
	const spec = "tests/simcluster/specs/mini.yaml"
	name, pools, err := scenario.ReadSpecPools(repoRoot, spec)
	if err != nil {
		t.Fatal(err)
	}
	s, err := clusterspec.LoadFile(filepath.Join(repoRoot, spec))
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.Expand(clusterspec.Profiles(""))
	if err != nil {
		t.Fatal(err)
	}
	if name != s.Metadata.Name || len(pools) != len(s.Spec.Pools) {
		t.Fatalf("spec %s with %d pools, want %s with %d", name, len(pools), s.Metadata.Name, len(s.Spec.Pools))
	}
	want := map[string]*corev1.Node{}
	for _, m := range members {
		want[m.Name] = m.Node
	}
	nodes := 0
	for i, p := range pools {
		if p.Name != s.Spec.Pools[i].Name || len(p.Nodes) != s.Spec.Pools[i].Count {
			t.Errorf("pool %d is %s with %d Nodes, want %s with %d", i, p.Name, len(p.Nodes), s.Spec.Pools[i].Name, s.Spec.Pools[i].Count)
		}
		for _, n := range p.Nodes {
			nodes++
			w := want[n.Name]
			if w == nil {
				t.Errorf("pool %s: Node %s is not one Expand renders", p.Name, n.Name)
				continue
			}
			if !equality.Semantic.DeepEqual(n.Labels, w.Labels) || !equality.Semantic.DeepEqual(n.Spec.Taints, w.Spec.Taints) ||
				!equality.Semantic.DeepEqual(n.Status.Allocatable, w.Status.Allocatable) {
				t.Errorf("Node %s differs from Expand's:\n%+v\n%+v", n.Name, n, w)
			}
			if rack := s.Spec.Pools[i].Rack; n.Labels[clusterspec.LabelRack] != rack || n.Labels[corev1.LabelHostname] != n.Name {
				t.Errorf("Node %s: rack %q, host name %q; want rack %q and the Node's name", n.Name, n.Labels[clusterspec.LabelRack], n.Labels[corev1.LabelHostname], rack)
			}
		}
	}
	if nodes != len(members) {
		t.Errorf("%d Nodes in the pools, Expand renders %d", nodes, len(members))
	}
}

// A placement that selects Nodes by rack or by host name plans over the
// capacity of those Nodes: 2 x 95 CPUs in rack r1, 95 on intel-0. A load
// within it plans, one above it is refused.
func TestPlanCountsNodesSelectedByRackOrHostName(t *testing.T) {
	plan := func(doc string) error {
		t.Helper()
		s := fxDecode(t, doc)
		if err := scenario.Validate(s, scenario.ValidateOptions{RepoRoot: repoRoot}); err != nil {
			t.Fatalf("invalid: %v", err)
		}
		_, err := scenario.MakePlan(context.Background(), s, scenario.PlanOptions{RepoRoot: repoRoot, RunDir: filepath.Join(t.TempDir(), "run"), GitSHA: "abc"})
		return err
	}
	head := "schemaVersion: 1\nname: topo\ncluster: {spec: tests/simcluster/specs/mini.yaml}\nduration: 10m\nworkload:\n  jobs:\n"
	for selector, cores := range map[string]int{
		"{" + clusterspec.LabelRack + ": r1}":     190,
		"{" + corev1.LabelHostname + ": intel-0}": 95,
	} {
		// 10 jobs x cores/2 x 60 s over 600 s: a load of 0.5.
		jobs := "  - {name: j, class: standard, count: 10, every: 50s, requests: {cpu: \"" + itoa(cores/2) + "\"}, runtime: 60s}\n  placement: {nodeSelector: " + selector + "}\n"
		if err := plan(head + jobs); err != nil {
			t.Errorf("placement %s: %v; want a plan over %d cores", selector, err, cores)
		}
		over := "  - {name: j, class: standard, count: 10, every: 50s, requests: {cpu: \"" + itoa(cores) + "\"}, runtime: 60s}\n  placement: {nodeSelector: " + selector + "}\n"
		if err := plan(head + over); err == nil {
			t.Errorf("placement %s: a load of 1.0 planned; want it refused", selector)
		}
	}
}

func itoa(n int) string {
	const digits = "0123456789"
	if n < 10 {
		return digits[n : n+1]
	}
	return itoa(n/10) + digits[n%10:n%10+1]
}

// plan reads the timing with the harness's own reader, so a timing file the
// harness refuses is refused here too: a negative interval and an unknown
// key each fail the plan, naming the key.
func TestPlanRefusesATimingTheHarnessRefuses(t *testing.T) {
	repo := t.TempDir()
	spec := filepath.Join("tests", "simcluster", "specs", "mini.yaml")
	b, err := os.ReadFile(filepath.Join(repoRoot, spec))
	if err != nil {
		t.Fatal(err)
	}
	timing := "CONTROLLER_RECONCILE_INTERVAL=-5s\nAGENT_RECONCILE_INTERVAL=5s\nAGENT_SPEC_READ_TIMEOUT=60s\nPROM_SCRAPE_INTERVAL=5s\n" +
		"OTLP_EXPORT_INTERVAL_MS=5000\nRATE_WINDOW=30s\nFACILITY_POLL_INTERVAL=10s\nSCRAPE_EVERY=5s\n"
	for rel, content := range map[string]string{spec: string(b), "scripts/simcluster/timing/dev.env": timing} {
		path := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := fxDecode(t, "schemaVersion: 1\nname: timing\ncluster: {spec: tests/simcluster/specs/mini.yaml, timing: dev}\nduration: 10m\nworkload:\n  jobs:\n"+
		"  - {name: j, class: standard, count: 1, every: 60s, requests: {cpu: \"1\"}, runtime: 60s}\n")
	_, err = scenario.MakePlan(context.Background(), s, scenario.PlanOptions{RepoRoot: repo, RunDir: filepath.Join(t.TempDir(), "run"), GitSHA: "abc"})
	for _, want := range []string{"CONTROLLER_RECONCILE_INTERVAL=-5s is not a positive duration", "unknown key SCRAPE_EVERY"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("plan with an invalid timing file: error %v, want one containing %q", err, want)
		}
	}
}

// EarliestEnd is the latest of each job's due time plus its runtime and,
// per resource, of each due time plus the work due from then on over the
// capacity: on 10 cores, three jobs of 10 cores and 100 s due at 0, 50 and
// 50 s end at 300 s at the earliest, and a fourth due at 400 s at 500 s.
func TestEarliestEndCountsTheWorkDueFromEachArrival(t *testing.T) {
	job := func(at float64) scenario.TraceJob {
		return scenario.TraceJob{SubmitTimeOffsetSec: at, PodTemplate: scenario.TracePodTemplate{Requests: map[string]string{"cpu": "10"}},
			Work: scenario.TraceWork{CPUUnits: 1000}}
	}
	capacity := map[string]float64{"cpu": 10, "memory": 0}
	jobs := []scenario.TraceJob{job(50), job(0), job(50)}
	if got := scenario.EarliestEnd(jobs, capacity, 1); got != 300*time.Second {
		t.Errorf("earliest end %s, want 5m0s: 3000 core-seconds from 0 s on 10 cores", got)
	}
	if got := scenario.EarliestEnd(append(jobs, job(400)), capacity, 1); got != 500*time.Second {
		t.Errorf("earliest end %s, want 8m20s: the job due at 400 s runs 100 s", got)
	}
}

// plan refuses work that cannot end within 0.9 of the deadline even when
// its load over the window is below the limit: 10 jobs of 95 cores and
// 100 s due from 500 s on, on the mini spec's 190 untainted cores, offer
// 0.83 of the 10 minutes but cannot end before 1000 s. Ten minutes of drain
// make room for them.
func TestPlanRefusesWorkThatCannotEndByTheDeadline(t *testing.T) {
	plan := func(drain string) (*scenario.Plan, error) {
		t.Helper()
		s := fxDecode(t, "schemaVersion: 1\nname: burst\ncluster: {spec: tests/simcluster/specs/mini.yaml}\nduration: 10m\n"+drain+
			"workload: {trace: traces/late-burst.jsonl}\n")
		if err := scenario.Validate(s, scenario.ValidateOptions{RepoRoot: repoRoot}); err != nil {
			t.Fatalf("invalid: %v", err)
		}
		return scenario.MakePlan(context.Background(), s, scenario.PlanOptions{RepoRoot: repoRoot, RunDir: filepath.Join(t.TempDir(), "run"), GitSHA: "abc"})
	}
	p, err := plan("")
	var over *scenario.OverloadError
	if !errors.As(err, &over) || len(over.Over) != 0 || over.EarliestEnd != 1000*time.Second || over.Deadline != 10*time.Minute {
		t.Fatalf("error %v, want the work's earliest end, 16m40s, past 0.9 of the 10m deadline and no resource overloaded", err)
	}
	if !strings.Contains(err.Error(), "the work cannot end before 16m40s, past 0.90 of the 10m0s deadline") {
		t.Errorf("error %q does not say when the work can end", err)
	}
	for _, l := range p.Load {
		if l.Resource == "cpu" && (l.Load < 0.83 || l.Load > 0.84) {
			t.Errorf("cpu load %.3f, want 95000 over 190 x 600 s", l.Load)
		}
	}
	if p, err = plan("drain: 10m\n"); err != nil {
		t.Fatalf("with 10 minutes of drain: %v", err)
	}
	if p.EarliestEnd != 1000*time.Second || p.Deadline != 20*time.Minute {
		t.Errorf("earliest end %s of %s, want 16m40s of 20m0s", p.EarliestEnd, p.Deadline)
	}
}

// Under static_partition the performance jobs run only on the Nodes it
// keeps in performance, so plan checks their load there. On the mini spec
// at STATIC_HP_FRAC 0.5 one Node of each family is in performance, intel-0
// and nvl-0, whose GPU taint keeps CPU jobs off, so 10 performance jobs of
// 60 cores and 100 s, 0.53 of the 190 untainted cores, offer 1.05 of the 95
// left to them. Without Joulie, under queue_aware_v1, which grows its
// performance set with the demand, or with every Node in performance, the
// class is not confined that far and the plan passes.
func TestPlanChecksThePerformanceClassOnThePerformanceNodes(t *testing.T) {
	plan := func(joulie string) (*scenario.Plan, error) {
		t.Helper()
		s := fxDecode(t, "schemaVersion: 1\nname: confined\ncluster: {spec: tests/simcluster/specs/mini.yaml}\nduration: 10m\ndrain: 1m\nworkload:\n  jobs:\n"+
			"  - {name: p, class: performance, count: 10, every: 50s, requests: {cpu: \"60\"}, runtime: 100s}\n"+joulie)
		if err := scenario.Validate(s, scenario.ValidateOptions{RepoRoot: repoRoot}); err != nil {
			t.Fatalf("invalid: %v", err)
		}
		return scenario.MakePlan(context.Background(), s, scenario.PlanOptions{RepoRoot: repoRoot, RunDir: filepath.Join(t.TempDir(), "run"), GitSHA: "abc"})
	}
	_, err := plan("")
	var over *scenario.OverloadError
	if !errors.As(err, &over) || len(over.Over) != 0 || over.Deadline != 0 || len(over.Classes) != 1 {
		t.Fatalf("error %v, want the performance class refused and nothing else", err)
	}
	c := over.Classes[0]
	load := map[string]float64{}
	for _, l := range c.Load {
		load[l.Resource] = l.Load
	}
	if c.Class != "performance" || c.Nodes != 2 || c.Policy != "static_partition at STATIC_HP_FRAC 0.5" || load["cpu"] < 1.05 || load["cpu"] > 1.06 {
		t.Errorf("class %+v, want performance on intel-0 and nvl-0 at 1.05 of 95 cores", c)
	}
	if !strings.Contains(err.Error(), "performance jobs on the 2 Nodes static_partition at STATIC_HP_FRAC 0.5 leaves them: offered load above 0.90: cpu 1.05") {
		t.Errorf("error %q does not name the class and its load", err)
	}
	for name, joulie := range map[string]string{
		"joulie off":         "joulie: {enabled: false}\n",
		"queue_aware_v1":     "joulie: {set: {controllerManager.env.POLICY_TYPE: queue_aware_v1}}\n",
		"all performance":    "joulie: {set: {controllerManager.env.STATIC_HP_FRAC: \"1\"}}\n",
		"variant joulie off": "variants: [{name: nojoulie, joulie: {enabled: false}}]\n",
	} {
		p, err := plan(joulie)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if name == "all performance" {
			if len(p.Classes) != 1 || p.Classes[0].Over() {
				t.Errorf("%s: classes %+v, want one within the limits", name, p.Classes)
			}
		} else if len(p.Classes) != 0 {
			t.Errorf("%s: classes %+v, want none confined", name, p.Classes)
		}
	}
}
