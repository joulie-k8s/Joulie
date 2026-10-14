package scenario_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
