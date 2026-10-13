package main

import (
	"sort"
	"testing"
)

// An eco GPU node gets GPU caps only, in standalone mode as in Kubernetes
// mode (TestEcoGPUNodeCPUIsNotCapped in cmd/controller-manager): its CPU runs
// as on a performance node, at full frequency under the performance cap,
// because capping it saves little and slows the GPU data feed by more
// (exp-03). The old code scaled the CPU frequency of an eco GPU node to
// SIM_SA_CPU_ECO_PCT, or capped its PL1 at that share on a profile with a
// writable PL1, so the two modes differed on every eco GPU node.
func TestStandaloneEcoGPUNodeCPURunsAsOnAPerformanceNode(t *testing.T) {
	for _, tc := range []struct {
		name, inventory string
	}{
		{
			name:      "profile with a writable PL1",
			inventory: "nodes:\n  - node_name_prefix: p\n    replicas: 2\n    profile: nvidia-h100-nvl-8gpu\n",
		},
		{
			name:      "profile without a CPU power cap",
			inventory: "nodes:\n  - node_name_prefix: p\n    replicas: 2\n    profile: nvidia-l40s-4gpu\n",
		},
		{
			name: "legacy entry",
			inventory: `nodes:
  - node_name_prefix: p
    replicas: 2
    vendor: nvidia
    product: NVIDIA H100 NVL
    cpu: INTEL(R) XEON(R) GOLD 6530
    cpu_sockets: 2
    cpu_cores: 64
    memory_gib: 1024
    gpu_count: 8
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearSimEnv(t)
			s, nodes := standaloneProfileNodes(t, tc.inventory)
			p := &standalonePolicy{s: s, tracker: map[string]*standaloneNodeTracker{}, nodeByName: map[string]*expandedNode{}}
			for i, n := range nodes {
				if n.GPUCount == 0 {
					t.Fatalf("setup: node %s has no GPU", n.Name)
				}
				p.tracker[n.Name] = &standaloneNodeTracker{}
				p.nodeByName[n.Name] = &nodes[i]
				p.names = append(p.names, n.Name)
			}
			sort.Strings(p.names)
			p.apply("B", 0)
			if p.tracker["p-0"].isEco || !p.tracker["p-1"].isEco {
				t.Fatalf("eco = p-0 %v, p-1 %v; want p-1 only", p.tracker["p-0"].isEco, p.tracker["p-1"].isEco)
			}
			perf, eco, model := s.state["p-0"], s.state["p-1"], s.modelForNode("p-1")
			if eco.FreqScale != 1 || eco.TargetThrottlePct != 0 || eco.TargetCapWatts != model.DefaultCapW {
				t.Errorf("eco GPU node CPU = scale %g, throttle %d%%, cap %g W; want scale 1, throttle 0 and the %g W performance cap, as on a performance node: no eco CPU cap on a GPU node",
					eco.FreqScale, eco.TargetThrottlePct, eco.TargetCapWatts, model.DefaultCapW)
			}
			if eco.FreqScale != perf.FreqScale || eco.TargetThrottlePct != perf.TargetThrottlePct || eco.TargetCapWatts != perf.TargetCapWatts {
				t.Errorf("eco GPU node CPU = scale %g, throttle %d%%, cap %g W; performance node %g, %d%%, %g W; want the same",
					eco.FreqScale, eco.TargetThrottlePct, eco.TargetCapWatts, perf.FreqScale, perf.TargetThrottlePct, perf.TargetCapWatts)
			}
			// The GPUs still take the eco cap: 0.7 of the board maximum.
			if want := 0.7 * model.GPU.MaxWattsPerGPU; want <= 0 || eco.GPUTargetCapWattsPerGpu != want || perf.GPUTargetCapWattsPerGpu != model.GPU.MaxWattsPerGPU {
				t.Errorf("GPU caps = eco %g W, performance %g W; want %g W and the %g W maximum", eco.GPUTargetCapWattsPerGpu, perf.GPUTargetCapWattsPerGpu, want, model.GPU.MaxWattsPerGPU)
			}
		})
	}
}
