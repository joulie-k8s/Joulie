package main

// The CPU frequency exponent BetaFreq (SIM_BETA_FREQ) is fitted to published
// frequency and power caps, and this test refits it from the committed rows
// and job mix and checks the default against the fit. The rule:
//
//  1. A row enters when one setting (a frequency or a power limit) changes
//     for the same application on the same nodes, the source gives the
//     throughput ratio and the drawn power ratio, and the job spans at most
//     8 nodes. The threshold is a judgment: the simulator has no
//     synchronization across nodes, which would rule out the 2 to 8 node
//     runs as much as the 64-node ones. Applied to one node only, the rule
//     keeps imagick alone, and TestBetaFreqFit logs that beta too.
//  2. The target is the median g = ln T / ln r of the rows in. No source
//     classifies their applications, so the target applies to experiment
//     01's job mix, not to one workload class.
//  3. At experiment 01's eco cap (cpu_eco_pct_of_max of its benchmark-40n.yaml,
//     resolved against the catalog range as the agent does), each of 01's
//     node types (configs/cluster-nodes.yaml) at full load solves its frequency
//     (cpuPower) and each job runs at the throughput of its class and
//     intensities (cpuThroughputMultiplier); g_job = ln S / ln(drawn power /
//     uncapped power). beta is the value whose mean g, weighted by the jobs'
//     CPU units and the nodes of each type, equals the target.
//
// Every value of beta in the band of the rows gives each of 01's workload
// types a mean g below 1, so a cap saving energy per job on average is an
// input of the model, not a finding of the experiments. Single jobs can
// exceed 1 at the low end of the band: at beta 0.86 the most compute-bound
// cpu_analytics jobs reach about 1.1.

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/matbun/joulie/pkg/hwinv"
	"sigs.k8s.io/yaml"
)

// cpuCapRows are the verified rows of published caps: drawn power ratio r and
// throughput ratio T between a capped and an uncapped run.
var cpuCapRows = []struct {
	source     string
	nodes      int
	oneSetting bool
	r, t       float64
}{
	// Jackson, Simpson, Turner, arXiv 2309.05440, Table 4: 2.0 GHz against
	// 2.25 GHz+turbo; node power ratio = energy ratio x performance ratio.
	{"ARCHER2 LAMMPS", 4, true, 0.92 * 0.74, 0.74},
	{"ARCHER2 GROMACS", 3, true, 0.92 * 0.83, 0.83},
	{"ARCHER2 Nektar++", 2, true, 0.80 * 0.80, 0.80},
	{"ARCHER2 CP2K", 4, true, 0.93 * 0.91, 0.91},
	{"ARCHER2 CASTEP", 4, true, 0.88 * 0.93, 0.93},
	{"ARCHER2 ONETEP", 4, true, 0.82 * 0.92, 0.92},
	{"ARCHER2 VASP", 8, true, 0.88 * 0.95, 0.95},
	// Jelvani, Martin, Nagarakatte, arXiv 2506.16046: 638.imagick_s at a
	// 120 W RAPL limit against 150 W, 7 % slower for a 9 % efficiency gain.
	{"Xeon Gold 6242 imagick", 1, true, 0.93 / 1.09, 0.93},
	// Patki et al., ICS '25, Table 6: 64 Hawk nodes, uncapped against static
	// and dynamic node caps (drawn kW, GFlop/s).
	{"Hawk HPL static", 64, true, 36.8 / 44.5, 1.27 / 2.05},
	{"Hawk HPL dynamic", 64, true, 38.4 / 44.5, 1.56 / 2.05},
	{"Hawk HPCG static", 64, true, 35.1 / 40.6, 2401.0 / 2515},
	{"Hawk HPCG dynamic", 64, true, 33.6 / 40.6, 2402.0 / 2515},
	// SPEC CPU2017 44852 against 44951 (EPYC 9965): several settings change.
	{"EPYC 9965 SPEC x264", 1, false, 1170.0 / 1230, 0.918},
}

type exp01Job struct {
	workloadType, class string
	mem, io, units      float64
}

// exp01Dir is experiment 01, whose job mix, inventory and eco cap the fit
// uses. The fixture testdata/exp01-cpu-jobmix.json holds its jobs of seeds 3 and
// 4, which the sweep does not measure (it runs seeds 1 and 2), as the policies
// are sized on them; its
// "source" field gives the command that regenerates it.
var exp01Dir = filepath.Join("..", "..", "..", "experiments", "01-cpu-only-benchmark")

func loadExp01JobMix(t *testing.T) []exp01Job {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "exp01-cpu-jobmix.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Jobs [][]any `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	jobs := make([]exp01Job, 0, len(doc.Jobs))
	for _, j := range doc.Jobs {
		jobs = append(jobs, exp01Job{j[0].(string), j[1].(string), j[2].(float64), j[3].(float64), j[4].(float64)})
	}
	return jobs
}

type exp01Node struct {
	labels map[string]string
	count  int
}

// loadExp01Setup reads experiment 01's node types (configs/cluster-nodes.yaml)
// and its eco CPU cap in percent (policy.caps.cpu_eco_pct_of_max of
// configs/benchmark-40n.yaml).
func loadExp01Setup(t *testing.T) ([]exp01Node, float64) {
	t.Helper()
	var inv struct {
		Nodes []struct {
			CPU      string `json:"cpu"`
			Sockets  int    `json:"cpu_sockets"`
			Replicas int    `json:"replicas"`
			GPUs     int    `json:"gpu_count"`
		} `json:"nodes"`
	}
	var cfg struct {
		Policy struct {
			Caps struct {
				CPUEcoPct float64 `json:"cpu_eco_pct_of_max"`
			} `json:"caps"`
		} `json:"policy"`
	}
	for path, into := range map[string]any{"cluster-nodes.yaml": &inv, "benchmark-40n.yaml": &cfg} {
		raw, err := os.ReadFile(filepath.Join(exp01Dir, "configs", path))
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	var nodes []exp01Node
	for _, n := range inv.Nodes {
		if n.GPUs != 0 {
			t.Fatalf("experiment 01 has a GPU node (%s); the fit assumes CPU-only nodes", n.CPU)
		}
		nodes = append(nodes, exp01Node{map[string]string{"joulie.io/hw.cpu-model": n.CPU, "joulie.io/hw.cpu-sockets": strconv.Itoa(n.Sockets)}, n.Replicas})
	}
	if len(nodes) == 0 || cfg.Policy.Caps.CPUEcoPct <= 0 {
		t.Fatalf("experiment 01's setup not read: %d node types, eco cap %v %%", len(nodes), cfg.Policy.Caps.CPUEcoPct)
	}
	return nodes, cfg.Policy.Caps.CPUEcoPct
}

// exp01MeanG is the mean g over jobs at experiment 01's eco cap with BetaFreq
// beta, weighted by CPU units and node counts; keep selects the jobs.
func exp01MeanG(t *testing.T, beta float64, jobs []exp01Job, keep func(exp01Job) bool) (mean, maxG float64) {
	t.Helper()
	s, _ := mrSimulator(t, nil)
	nodes, ecoPct := loadExp01Setup(t)
	var sum, weight float64
	for _, n := range nodes {
		model, _ := s.resolveModelForNode(n.labels)
		model.BetaFreq = beta
		r, err := s.catalog.NodeCPUCapRangeW(hwinv.SimulatedNodeCPU(n.labels))
		if err != nil {
			t.Fatal(err)
		}
		capW := r.CapForPctW(ecoPct)
		for _, j := range jobs {
			if !keep(j) {
				continue
			}
			st := &nodeState{CPUUtil: 1, CPUWorkClass: j.class, MemoryIntensity: j.mem, IOIntensity: j.io}
			st.CapWatts = 1e9
			free, _, _ := cpuPower(st, model, 1)
			st.CapWatts = capW
			capped, f, _ := cpuPower(st, model, 1)
			if capped >= free {
				t.Fatalf("%s: the %.0f W cap does not bind at full load (%.0f W uncapped)", n.labels["joulie.io/hw.cpu-model"], capW, free)
			}
			g := math.Log(cpuThroughputMultiplier(f, j.class, model, j.mem, j.io, 0)) / math.Log(capped/free)
			w := j.units * float64(n.count)
			sum += w * g
			weight += w
			maxG = math.Max(maxG, g)
		}
	}
	return sum / weight, maxG
}

// solveBetaFreq is the beta whose mean g is target; the mean g falls as beta
// rises.
func solveBetaFreq(t *testing.T, target float64, jobs []exp01Job) float64 {
	lo, hi := 0.3, 8.0
	for i := 0; i < 40; i++ {
		mid := (lo + hi) / 2
		if g, _ := exp01MeanG(t, mid, jobs, func(exp01Job) bool { return true }); g > target {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// TestBetaFreqFit refits BetaFreq by the rule above and checks that the
// default is the fit, and that every class saves power faster than it loses
// throughput at experiment 01's cap. Before the fit, a curve node read its
// power at utilization x throughput, and the 9655 gave g 1.4 to 2.0 for
// every class.
func TestBetaFreqFit(t *testing.T) {
	clearSimEnv(t)
	jobs := loadExp01JobMix(t)
	var in, oneNode []float64
	for _, row := range cpuCapRows {
		if row.oneSetting && row.nodes <= 8 {
			in = append(in, math.Log(row.t)/math.Log(row.r))
			if row.nodes == 1 {
				oneNode = append(oneNode, math.Log(row.t)/math.Log(row.r))
			}
		}
	}
	sort.Float64s(in)
	target := median(in)
	beta := solveBetaFreq(t, target, jobs)
	if def := defaultBaseModelFromEnv().BetaFreq; math.Abs(beta-def) > 0.01 {
		t.Fatalf("SIM_BETA_FREQ default %.3f, the fit gives %.3f (target g %.3f over %d rows)", def, beta, target, len(in))
	}
	lowBeta, highBeta := solveBetaFreq(t, in[len(in)-1], jobs), solveBetaFreq(t, in[0], jobs)
	t.Logf("target g %.3f over %d rows, band %.2f to %.2f: beta %.3f; band ends beta %.3f and %.3f; one-node rule (g %.3f): beta %.3f",
		target, len(in), in[0], in[len(in)-1], beta, lowBeta, highBeta, median(oneNode), solveBetaFreq(t, median(oneNode), jobs))
	for _, b := range []float64{lowBeta, highBeta} {
		for _, wt := range []string{"cpu_analytics", "cpu_preprocess"} {
			g, maxG := exp01MeanG(t, b, jobs, func(j exp01Job) bool { return j.workloadType == wt })
			t.Logf("band end beta %.3f, %s: mean g %.3f, highest %.3f", b, wt, g, maxG)
			if g >= 1 {
				t.Errorf("band end beta %.3f, %s: mean g %.3f; the docs say a cap saves energy per job on average across the band", b, wt, g)
			}
		}
	}
	// The delivered g of each of 01's workload types at the fitted beta, as
	// the docs quote them (wiki CPU power model).
	for wt, want := range map[string]float64{"cpu_analytics": 0.59, "cpu_preprocess": 0.22} {
		g, maxG := exp01MeanG(t, beta, jobs, func(j exp01Job) bool { return j.workloadType == wt })
		t.Logf("%s: mean g %.3f, highest %.3f", wt, g, maxG)
		if math.Abs(g-want) > 0.02 {
			t.Errorf("%s: mean g %.3f at experiment 01's cap, want %.2f +- 0.02", wt, g, want)
		}
		if maxG >= 1 {
			t.Errorf("%s: a job at experiment 01's cap loses throughput faster than power (g %.3f)", wt, maxG)
		}
	}
}
