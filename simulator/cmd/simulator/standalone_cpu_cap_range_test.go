package main

import (
	"sort"
	"testing"

	"github.com/matbun/joulie/pkg/hwinv"
	"github.com/matbun/joulie/simulator/pkg/hw"
)

// legacyStandalonePolicy is the nodes of a legacy inventory, built as
// runStandalone builds them with the embedded catalog, ready for
// applyPowerPolicy.
func legacyStandalonePolicy(t *testing.T, inventory string) *standalonePolicy {
	t.Helper()
	nodes, err := loadInventory(writeStandaloneFile(t, "inventory.yaml", inventory))
	if err != nil {
		t.Fatal(err)
	}
	s := newStandaloneTestSimulator(t)
	if s.catalog, err = hw.LoadCatalog(""); err != nil {
		t.Fatal(err)
	}
	if err := initStandaloneNodes(s, nodes); err != nil {
		t.Fatal(err)
	}
	p := &standalonePolicy{s: s, tracker: map[string]*standaloneNodeTracker{}, nodeByName: map[string]*expandedNode{}}
	for i, n := range nodes {
		p.tracker[n.Name] = &standaloneNodeTracker{}
		p.nodeByName[n.Name] = &nodes[i]
		p.names = append(p.names, n.Name)
	}
	sort.Strings(p.names)
	return p
}

// A legacy CPU-only node is a KWOK node that a pool agent caps over HTTP.
// That agent resolves a percent cap against the catalog range of the node's
// CPU, sockets x the socket range, and sends watts (cmd/agent
// resolveCPUCapFromPct), so standalone mode applies the same watts: eco at
// 60 % on 2x EPYC 9654 is 640 W, the cTDP low end, at full frequency, and
// performance under B is the 720 W top of the range. Standalone used to scale
// the frequency of an eco node to 0.6 and leave performance at the 5000 W
// default cap, while the agent it models now sends watts. Baseline A runs
// without Joulie and keeps the default cap; a CPU the catalog does not know
// keeps the frequency scale, as the agent keeps its DVFS throttle.
func TestStandaloneLegacyCPUNodeTakesTheAgentsCatalogWattCap(t *testing.T) {
	clearSimEnv(t)
	const epyc = `nodes:
  - node_name_prefix: p
    replicas: 2
    vendor: none
    cpu: AMD EPYC 9654 96-Core Processor
    cpu_sockets: 2
    cpu_cores: 192
    memory_gib: 1536
    gpu_count: 0
`
	p := legacyStandalonePolicy(t, epyc)
	n := p.nodeByName["p-1"]
	model, sockets := hwinv.SimulatedNodeCPU(n.Labels)
	agent, err := p.s.catalog.NodeCPUCapRangeW(model, sockets)
	if err != nil {
		t.Fatalf("test setup: the agent's catalog range for %q x %d: %v", model, sockets, err)
	}
	if agent.MinW() != 640 || agent.MaxW() != 720 {
		t.Fatalf("test setup: the agent's range is %v to %v W, want 640 to 720 W", agent.MinW(), agent.MaxW())
	}

	p.apply("B", 0)
	perf, eco := p.s.state["p-0"], p.s.state["p-1"]
	if p.tracker["p-0"].isEco || !p.tracker["p-1"].isEco {
		t.Fatalf("eco = p-0 %v, p-1 %v; want p-1 only", p.tracker["p-0"].isEco, p.tracker["p-1"].isEco)
	}
	if want := agent.CapForPctW(60); eco.TargetCapWatts != want || want != 640 || eco.FreqScale != 1 || eco.TargetThrottlePct != 0 {
		t.Errorf("eco: CPU cap %v W at scale %v, throttle %d%%; want the agent's %v W at 1 and 0", eco.TargetCapWatts, eco.FreqScale, eco.TargetThrottlePct, want)
	}
	if perf.TargetCapWatts != agent.MaxW() || perf.FreqScale != 1 || perf.TargetThrottlePct != 0 {
		t.Errorf("performance: CPU cap %v W at scale %v, throttle %d%%; want the agent's %v W for 100 %% at 1 and 0", perf.TargetCapWatts, perf.FreqScale, perf.TargetThrottlePct, agent.MaxW())
	}

	p.apply("A", 0)
	if def := p.s.modelForNode("p-1").DefaultCapW; eco.TargetCapWatts != def || def != 5000 {
		t.Errorf("baseline A: CPU cap %v W, want the 5000 W default (no Joulie)", eco.TargetCapWatts)
	}

	unknown := legacyStandalonePolicy(t, `nodes:
  - node_name_prefix: p
    replicas: 2
    vendor: none
    cpu: Some Future CPU 9999
    cpu_sockets: 2
    cpu_cores: 64
    memory_gib: 256
    gpu_count: 0
`)
	unknown.apply("B", 0)
	if st := unknown.s.state["p-1"]; st.FreqScale != 0.6 || st.TargetThrottlePct != 40 {
		t.Errorf("unknown CPU in eco: scale %v, throttle %d%%; want 0.6 and 40 %%, the agent's DVFS throttle", st.FreqScale, st.TargetThrottlePct)
	}
}
