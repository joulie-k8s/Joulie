// Experiment: heterogeneous_cluster_control_loop
//
// This experiment validates the full Joulie next-architecture by simulating
// a heterogeneous cluster with different power-management scenarios.
//
// The experiment does NOT connect to a real Kubernetes cluster.
// It uses internal simulation of the operator, scheduler, and agent logic.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sort"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/operator/twin"
	"github.com/matbun/joulie/simulator/pkg/facility"
)

// ScenarioConfig controls which Joulie features are enabled.
type ScenarioConfig struct {
	Name             string
	Label            string
	CapsEnabled      bool
	SchedulerEnabled bool
	CPUCapPct        float64
	GPUCapPct        float64
}

// Job represents a simulated workload.
type Job struct {
	ID            string
	CPUIntensity  float64 // 0-1
	GPUIntensity  float64 // 0-1
	Bound         string  // "compute", "memory", "io", "mixed"
	DurationS     float64 // nominal duration in seconds (uncapped)
	Criticality   string  // "performance", "standard", "best-effort"
	Reschedulable bool
	GPUSensitive  bool // true = penalized by GPU cap
	CPUSensitive  bool // true = penalized by CPU cap
	Rescheduled   bool // set when job was migrated during execution
}

// reschedulingOverheadS is the overhead added to job makespan when a pod is
// rescheduled: includes eviction, restart, checkpoint restore time.
// Real overhead depends on job type (stateless ≈ 30s, stateful ≈ 5-10min).
const reschedulingOverheadS = 120.0 // 2 minutes: conservative stateless restart

// SimNode represents a simulated cluster node.
type SimNode struct {
	Name          string
	CPUCores      int
	GPUCount      int
	HasGPU        bool
	MaxCPUPowerW  float64
	MaxGPUPowerW  float64 // total for all GPUs
	TwinState     joulie.NodeTwinStatus
	CurrentPowerW float64
	AssignedJobs  []Job
}

// RunResult holds the outcome of one scenario run.
type RunResult struct {
	Scenario           string
	TotalEnergyKWh     float64
	MakespanS          float64
	P50CompletionS     float64
	P95CompletionS     float64
	AvgPendingS        float64
	PeakRackPowerW     float64
	AvgRackPowerW      float64
	P95RackPowerW      float64
	PeakCoolingStress  float64
	AvgCoolingStress   float64
	PeakPSUStress      float64
	EcoPct             float64 // fraction of work placed on eco nodes
	EnergyDelayProduct float64
	NodeProfileChurn   int
	// Rescheduling metrics (only non-zero in scenarios with scheduler + migration)
	RescheduledJobs      int
	TotalReschedulingOverheadS float64
}

func main() {
	outDir := "results"
	if err := os.MkdirAll(outDir, 0755); err != nil {
		log.Fatalf("mkdir results: %v", err)
	}

	scenarios := []ScenarioConfig{
		{Name: "A", Label: "Baseline (no Joulie)", CapsEnabled: false, SchedulerEnabled: false, CPUCapPct: 100, GPUCapPct: 100},
		{Name: "B", Label: "Caps only", CapsEnabled: true, SchedulerEnabled: false, CPUCapPct: 65, GPUCapPct: 65},
		{Name: "C", Label: "Caps + Scheduler", CapsEnabled: true, SchedulerEnabled: true, CPUCapPct: 65, GPUCapPct: 65},
	}

	jobs := generateWorkloadMix(200)
	nodes := buildCluster()
	facilityModel := facility.NewModel(facility.DefaultConfig())

	var results []RunResult
	for _, sc := range scenarios {
		log.Printf("Running scenario %s: %s", sc.Name, sc.Label)
		result := runScenario(sc, jobs, nodes, facilityModel)
		results = append(results, result)

		// Write per-scenario results
		data, _ := json.MarshalIndent(result, "", "  ")
		path := fmt.Sprintf("%s/scenario_%s_metrics.json", outDir, sc.Name)
		os.WriteFile(path, data, 0644)
		log.Printf("  Energy: %.2f kWh, Makespan: %.0fs, Peak cooling: %.1f%%",
			result.TotalEnergyKWh, result.MakespanS, result.PeakCoolingStress)
	}

	// Write comparison
	comp := buildComparison(results)
	compData, _ := json.MarshalIndent(comp, "", "  ")
	os.WriteFile(fmt.Sprintf("%s/comparison.json", outDir), compData, 0644)

	// Print summary table
	printSummaryTable(results)
}

// generateWorkloadMix creates a realistic mix of HPC/AI workloads.
func generateWorkloadMix(n int) []Job {
	rng := rand.New(rand.NewSource(42))
	jobs := make([]Job, n)

	types := []struct {
		cpu        float64
		gpu        float64
		bound      string
		dur        float64
		crit       string
		reschedule bool
		gpuSens    bool
		cpuSens    bool
		weight     float64
	}{
		// GPU compute-bound (LLM training): 30%
		{0.5, 0.95, "compute", 3600, "standard", true, true, false, 0.30},
		// GPU memory-bound (inference): 20%
		{0.3, 0.85, "memory", 600, "performance", false, true, false, 0.20},
		// CPU compute-bound (data prep): 25%
		{0.90, 0.0, "compute", 1800, "standard", true, false, true, 0.25},
		// Best-effort low-util: 15%
		{0.20, 0.10, "mixed", 900, "best-effort", true, false, false, 0.15},
		// Short checkpointable: 10%
		{0.60, 0.50, "mixed", 300, "standard", true, true, true, 0.10},
	}

	cumulativeWeights := make([]float64, len(types))
	cumWeight := 0.0
	for i, t := range types {
		cumWeight += t.weight
		cumulativeWeights[i] = cumWeight
	}

	for i := range jobs {
		r := rng.Float64()
		tidx := 0
		for j, w := range cumulativeWeights {
			if r <= w {
				tidx = j
				break
			}
		}
		t := types[tidx]
		// Add ±20% jitter
		jitter := 0.8 + rng.Float64()*0.4
		jobs[i] = Job{
			ID:            fmt.Sprintf("job-%04d", i),
			CPUIntensity:  t.cpu * jitter,
			GPUIntensity:  t.gpu * jitter,
			Bound:         t.bound,
			DurationS:     t.dur * jitter,
			Criticality:   t.crit,
			Reschedulable: t.reschedule,
			GPUSensitive:  t.gpuSens,
			CPUSensitive:  t.cpuSens,
		}
		// Clamp intensities
		if jobs[i].CPUIntensity > 1 {
			jobs[i].CPUIntensity = 1
		}
		if jobs[i].GPUIntensity > 1 {
			jobs[i].GPUIntensity = 1
		}
	}
	return jobs
}

// buildCluster creates a heterogeneous simulated cluster.
func buildCluster() []SimNode {
	return []SimNode{
		// GPU nodes (4x H100-class)
		{Name: "gpu-node-1", CPUCores: 192, GPUCount: 8, HasGPU: true, MaxCPUPowerW: 720, MaxGPUPowerW: 3200},
		{Name: "gpu-node-2", CPUCores: 192, GPUCount: 8, HasGPU: true, MaxCPUPowerW: 720, MaxGPUPowerW: 3200},
		{Name: "gpu-node-3", CPUCores: 192, GPUCount: 4, HasGPU: true, MaxCPUPowerW: 720, MaxGPUPowerW: 1600},
		{Name: "gpu-node-4", CPUCores: 128, GPUCount: 4, HasGPU: true, MaxCPUPowerW: 480, MaxGPUPowerW: 1600},
		// CPU-only nodes (4x)
		{Name: "cpu-node-1", CPUCores: 192, GPUCount: 0, HasGPU: false, MaxCPUPowerW: 720, MaxGPUPowerW: 0},
		{Name: "cpu-node-2", CPUCores: 192, GPUCount: 0, HasGPU: false, MaxCPUPowerW: 720, MaxGPUPowerW: 0},
		{Name: "cpu-node-3", CPUCores: 128, GPUCount: 0, HasGPU: false, MaxCPUPowerW: 480, MaxGPUPowerW: 0},
		{Name: "cpu-node-4", CPUCores: 64, GPUCount: 0, HasGPU: false, MaxCPUPowerW: 250, MaxGPUPowerW: 0},
	}
}

// runScenario simulates one scenario and returns its metrics.
func runScenario(sc ScenarioConfig, jobs []Job, clusterTemplate []SimNode, fm *facility.Model) RunResult {
	// Deep copy cluster
	nodes := make([]SimNode, len(clusterTemplate))
	copy(nodes, clusterTemplate)

	// Build NodeTwinState for each node
	for i := range nodes {
		nodes[i].TwinState = buildNodeTwinState(nodes[i], sc)
	}

	completionTimes := make([]float64, 0, len(jobs))
	pendingTimes := make([]float64, 0, len(jobs))
	var rackPowerSamples [][2]float64
	var coolingStressSamples []float64
	var psuStressSamples []float64
	var totalEnergyJ float64
	ecoPlacements := 0
	profileChurns := 0
	lastProfiles := make(map[string]string)
	rescheduledJobs := 0
	totalReschedOverhead := 0.0

	// Simulate job queue
	simTime := 0.0
	remaining := make([]Job, len(jobs))
	copy(remaining, jobs)
	nodeSlots := make(map[string]float64) // nodeID -> next available time

	for len(remaining) > 0 || simTime < 10 {
		if len(remaining) == 0 {
			break
		}

		job := remaining[0]
		remaining = remaining[1:]

		// Schedule this job
		pendingStart := simTime
		chosenNode, waitTime := scheduleJob(job, nodes, nodeSlots, simTime, sc)
		pendingTimes = append(pendingTimes, waitTime)

		// Track eco placements
		if nodes[chosenNode].TwinState.SchedulableClass == "eco" {
			ecoPlacements++
		}

		// Profile churn tracking
		newProfile := nodes[chosenNode].TwinState.SchedulableClass
		if lastProfiles[nodes[chosenNode].Name] != "" && lastProfiles[nodes[chosenNode].Name] != newProfile {
			profileChurns++
		}
		lastProfiles[nodes[chosenNode].Name] = newProfile

		// Compute effective duration (slowdown under caps)
		node := nodes[chosenNode]
		startTime := simTime + waitTime
		effectiveDur := computeEffectiveDuration(job, node, sc)

		// Compute power draw during this job
		cpuPowerW := node.MaxCPUPowerW * job.CPUIntensity * (sc.CPUCapPct / 100.0)
		gpuPowerW := 0.0
		if node.HasGPU {
			gpuPowerW = node.MaxGPUPowerW * job.GPUIntensity * (sc.GPUCapPct / 100.0)
		}
		totalNodePowerW := cpuPowerW + gpuPowerW
		jobEnergyJ := totalNodePowerW * effectiveDur
		totalEnergyJ += jobEnergyJ

		// Simulate rescheduling overhead: in scenario B (caps without scheduler),
		// performance jobs may land on eco nodes. When thermal/PSU stress is
		// high (>70%), reschedulable jobs are migrated, adding restart overhead.
		reschedOverhead := 0.0
		if sc.CapsEnabled && job.Reschedulable {
			nodeClass := nodes[chosenNode].TwinState.SchedulableClass
			coolStress := nodes[chosenNode].TwinState.PredictedCoolingStressScore
			psuStress := nodes[chosenNode].TwinState.PredictedPsuStressScore
			// Reschedule if: performance on eco, or node under high stress
			shouldReschedule := (job.Criticality == "performance" && nodeClass == "eco" && !sc.SchedulerEnabled) ||
				(coolStress > 70 || psuStress > 70)
			if shouldReschedule {
				reschedOverhead = reschedulingOverheadS
				rescheduledJobs++
				totalReschedOverhead += reschedOverhead
			}
		}

		endTime := startTime + effectiveDur + reschedOverhead
		completionTimes = append(completionTimes, endTime-pendingStart)
		nodeSlots[nodes[chosenNode].Name] = endTime

		// Sample facility state
		nodePowers := collectNodePowers(nodes, nodeSlots, simTime)
		fState := fm.Compute(nodePowers, 22.0)
		rackPowerSamples = append(rackPowerSamples, [2]float64{fState.TotalRackPowerW, effectiveDur})
		coolingStressSamples = append(coolingStressSamples, fState.CoolingStressScore)
		psuStressSamples = append(psuStressSamples, fState.PSUStressScore)

		simTime += 0.1 // advance time
		_ = pendingStart
	}

	stats := facility.ComputeStats(rackPowerSamples, nil)
	makespan := computeMakespan(nodeSlots)

	sort.Float64s(completionTimes)
	p50 := percentile(completionTimes, 50)
	p95 := percentile(completionTimes, 95)

	ecoPct := 0.0
	if len(jobs) > 0 {
		ecoPct = float64(ecoPlacements) / float64(len(jobs)) * 100
	}

	return RunResult{
		Scenario:                   fmt.Sprintf("%s: %s", sc.Name, sc.Label),
		TotalEnergyKWh:             totalEnergyJ / 3600000,
		MakespanS:                  makespan,
		P50CompletionS:             p50,
		P95CompletionS:             p95,
		AvgPendingS:                mean(pendingTimes),
		PeakRackPowerW:             stats.PeakPowerW,
		AvgRackPowerW:              stats.AvgPowerW,
		P95RackPowerW:              stats.P95PowerW,
		PeakCoolingStress:          max64(coolingStressSamples),
		AvgCoolingStress:           mean(coolingStressSamples),
		PeakPSUStress:              max64(psuStressSamples),
		EcoPct:                     ecoPct,
		EnergyDelayProduct:         totalEnergyJ * makespan,
		NodeProfileChurn:           profileChurns,
		RescheduledJobs:            rescheduledJobs,
		TotalReschedulingOverheadS: totalReschedOverhead,
	}
}

func buildNodeTwinState(n SimNode, sc ScenarioConfig) joulie.NodeTwinStatus {
	profile := "performance"
	if sc.CapsEnabled {
		// Eco profile for GPU nodes under caps
		profile = "eco"
	}

	hw := joulie.NodeHardware{
		CPU: joulie.NodeHardwareCPU{TotalCores: n.CPUCores, Sockets: 2,
			CapRange: joulie.CPUCapRange{MaxWattsPerSocket: n.MaxCPUPowerW / 2}},
		GPU: joulie.NodeHardwareGPU{Present: n.HasGPU, Count: n.GPUCount,
			CapRange: joulie.GPUCapRange{MaxWatts: n.MaxGPUPowerW / float64(maxInt(n.GPUCount, 1))},
			},
	}

	in := twin.Input{
		NodeName:  n.Name,
		Hardware:  hw,
		Profile:   profile,
		CPUCapPct: sc.CPUCapPct,
		GPUCapPct: sc.GPUCapPct,
	}
	out := twin.Compute(in)

	return joulie.NodeTwinStatus{
		SchedulableClass:            out.SchedulableClass,
		PredictedPowerHeadroomScore: out.PredictedPowerHeadroomScore,
		PredictedCoolingStressScore: out.PredictedCoolingStressScore,
		PredictedPsuStressScore:     out.PredictedPsuStressScore,
		EffectiveCapState:           out.EffectiveCapState,
		HardwareDensityScore:        out.HardwareDensityScore,
	}
}

// scheduleJob picks a node for a job and returns (node index, wait time).
func scheduleJob(job Job, nodes []SimNode, nodeSlots map[string]float64, currentTime float64, sc ScenarioConfig) (int, float64) {
	bestNode := -1
	bestScore := -1.0

	for i, node := range nodes {
		// Filter: GPU jobs need GPU nodes
		if job.GPUIntensity > 0.3 && !node.HasGPU {
			continue
		}

		// Filter (scheduler): eco nodes reject performance workloads if scheduler enabled
		if sc.SchedulerEnabled && node.TwinState.SchedulableClass == "eco" && job.Criticality == "performance" {
			continue
		}

		score := scoreNode(node, job, sc)
		if score > bestScore {
			bestScore = score
			bestNode = i
		}
	}

	if bestNode == -1 {
		// Fallback: pick first available GPU node for GPU jobs, or first node
		for i, n := range nodes {
			if job.GPUIntensity > 0.3 && n.HasGPU {
				bestNode = i
				break
			}
		}
		if bestNode == -1 {
			bestNode = 0
		}
	}

	waitTime := 0.0
	if slot, ok := nodeSlots[nodes[bestNode].Name]; ok && slot > currentTime {
		waitTime = slot - currentTime
	}
	return bestNode, waitTime
}

func scoreNode(node SimNode, job Job, sc ScenarioConfig) float64 {
	if !sc.SchedulerEnabled {
		// Round-robin-like: prefer least loaded (lower next available time)
		return float64(node.CPUCores) * 0.01
	}

	ts := node.TwinState
	// Scheduler extender scoring logic (mirrors cmd/scheduler/main.go)
	score := ts.PredictedPowerHeadroomScore*0.4 +
		(100-ts.PredictedCoolingStressScore)*0.3 +
		(100-ts.PredictedPsuStressScore)*0.3

	if job.Criticality == "performance" && ts.SchedulableClass == "eco" {
		score *= 0.5
	}
	if job.Criticality == "best-effort" && ts.SchedulableClass == "eco" {
		score += 5
	}
	if job.CPUSensitive {
		score = score*0.7 + ts.EffectiveCapState.CPUPct/100*100*0.3
	}
	if job.GPUSensitive {
		score = score*0.7 + ts.EffectiveCapState.GPUPct/100*100*0.3
	}
	return score
}

func computeEffectiveDuration(job Job, node SimNode, sc ScenarioConfig) float64 {
	slowdown := 1.0
	if sc.CapsEnabled {
		cpuCap := sc.CPUCapPct / 100.0
		gpuCap := sc.GPUCapPct / 100.0

		// CPU-sensitive jobs slow down under CPU cap
		if job.CPUSensitive && job.Bound == "compute" {
			slowdown *= 1.0 + (1.0-cpuCap)*0.8
		}
		// GPU-sensitive jobs slow down under GPU cap
		if job.GPUSensitive && node.HasGPU {
			slowdown *= 1.0 + (1.0-gpuCap)*0.6
		}
	}

	return job.DurationS * slowdown
}

func collectNodePowers(nodes []SimNode, nodeSlots map[string]float64, currentTime float64) []facility.NodePower {
	powers := make([]facility.NodePower, len(nodes))
	for i, n := range nodes {
		w := 0.0
		if slot, ok := nodeSlots[n.Name]; ok && slot > currentTime {
			// Node is busy
			w = n.MaxCPUPowerW*0.7 + n.MaxGPUPowerW*0.8
		} else {
			// Idle
			w = n.MaxCPUPowerW * 0.1
		}
		powers[i] = facility.NodePower{NodeName: n.Name, PowerWatts: w}
	}
	return powers
}

func computeMakespan(nodeSlots map[string]float64) float64 {
	makespan := 0.0
	for _, t := range nodeSlots {
		if t > makespan {
			makespan = t
		}
	}
	return makespan
}

type comparison struct {
	Scenarios         []RunResult
	BaselineEnergy    float64
	EnergySavingsPct  map[string]float64
	MakespanRatioPct  map[string]float64
	CoolingSavingsPct map[string]float64
}

func buildComparison(results []RunResult) comparison {
	if len(results) == 0 {
		return comparison{}
	}
	baseline := results[0]
	comp := comparison{
		Scenarios:         results,
		BaselineEnergy:    baseline.TotalEnergyKWh,
		EnergySavingsPct:  make(map[string]float64),
		MakespanRatioPct:  make(map[string]float64),
		CoolingSavingsPct: make(map[string]float64),
	}
	for _, r := range results[1:] {
		if baseline.TotalEnergyKWh > 0 {
			comp.EnergySavingsPct[r.Scenario] = (baseline.TotalEnergyKWh - r.TotalEnergyKWh) / baseline.TotalEnergyKWh * 100
		}
		if baseline.MakespanS > 0 {
			comp.MakespanRatioPct[r.Scenario] = (r.MakespanS - baseline.MakespanS) / baseline.MakespanS * 100
		}
		if baseline.PeakCoolingStress > 0 {
			comp.CoolingSavingsPct[r.Scenario] = (baseline.PeakCoolingStress - r.PeakCoolingStress) / baseline.PeakCoolingStress * 100
		}
	}
	return comp
}

func printSummaryTable(results []RunResult) {
	fmt.Printf("\n%-40s %10s %10s %12s %12s %10s\n",
		"Scenario", "Energy(kWh)", "Makespan(s)", "PeakCool(%)", "EcoPlace(%)", "EDP")
	fmt.Printf("%s\n", "-----------------------------------------------------------------------------------------")
	for _, r := range results {
		fmt.Printf("%-40s %10.2f %10.0f %12.1f %12.1f %10.2e\n",
			r.Scenario, r.TotalEnergyKWh, r.MakespanS,
			r.PeakCoolingStress, r.EcoPct, r.EnergyDelayProduct)
	}
	fmt.Println()
}

// Helpers
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)) * p / 100)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func max64(vals []float64) float64 {
	m := 0.0
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Ensure time is used (imported for potential future use in trace logging).
var _ = time.Now
