package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/matbun/joulie/simulator/pkg/phys"
	"sigs.k8s.io/yaml"
)

// ---------------------------------------------------------------------------
// Standalone simulation mode — bypasses K8s entirely, runs at CPU speed.
// Activated by SIM_STANDALONE=true.
//
// Required env vars:
//   SIM_STANDALONE_INVENTORY  — path to cluster-nodes.yaml
//   SIM_WORKLOAD_TRACE_PATH   — path to workload trace JSONL
//   SIM_STANDALONE_BASELINE   — A, B, or C
//   SIM_DEBUG_PERSIST_DIR     — output directory (timeseries.csv written here)
//
// Optional env vars (same as normal mode):
//   SIM_BASE_SPEED_PER_CORE, SIM_FACILITY_AMBIENT_TEMP_C,
//   SIM_FACILITY_TEMP_AMPLITUDE_C, SIM_FACILITY_TEMP_PERIOD_H
//
// Policy env vars (for baselines B and C):
//   SIM_SA_CPU_ECO_PCT          — eco CPU frequency scale (0-100), default 60
//   SIM_SA_GPU_ECO_PCT          — eco GPU cap as % of max, default 70
//   SIM_SA_HP_FRAC              — static HP fraction for baseline B, default 0.30
//   SIM_SA_HP_BASE_FRAC         — base HP fraction for baseline C, default 0.30
//   SIM_SA_HP_MIN               — min HP nodes for baseline C, default 1
//   SIM_SA_HP_MAX               — max HP nodes for baseline C, default 25
//   SIM_SA_PERF_PER_HP_NODE     — queue depth per HP node for baseline C, default 10
//   SIM_SA_RECONCILE_INTERVAL   — operator reconcile sim-seconds for C, default 300
// ---------------------------------------------------------------------------

// inventoryFile is the top-level YAML structure for cluster-nodes.yaml.
type inventoryFile struct {
	Nodes []inventoryNode `json:"nodes"`
}

type inventoryNode struct {
	NodeNamePrefix string  `json:"node_name_prefix"`
	NodeName       string  `json:"node_name"`
	Replicas       int     `json:"replicas"`
	Vendor         string  `json:"vendor"`
	Product        string  `json:"product"`
	CPU            string  `json:"cpu"`
	CPUSockets     int     `json:"cpu_sockets"`
	CPUCores       int     `json:"cpu_cores"`
	MemoryGiB      int     `json:"memory_gib"`
	GPUCount       int     `json:"gpu_count"`
	GPUMinCapWatts float64 `json:"gpu_min_cap_watts"`
	GPUMaxCapWatts float64 `json:"gpu_max_cap_watts"`
}

// expandedNode is a single concrete node after expanding replicas.
type expandedNode struct {
	Name           string
	Vendor         string
	Product        string
	CPUModel       string
	CPUSockets     int
	CPUCores       int
	MemoryGiB      int
	GPUCount       int
	GPUMinCapWatts float64
	GPUMaxCapWatts float64
	Labels         map[string]string
}

// standaloneNodeTracker tracks per-node resource usage for bin-packing.
type standaloneNodeTracker struct {
	usedCPU      float64
	usedGPU      float64
	jobs         []*simJob
	isEco        bool
	isDraining   bool // FSM: transitioning perf→eco, still has perf pods
	perfPodCount int  // running performance-sensitive pods on this node
}

func runStandalone(s *simulator) {
	inventoryPath := strings.TrimSpace(os.Getenv("SIM_STANDALONE_INVENTORY"))
	if inventoryPath == "" {
		log.Fatal("standalone mode requires SIM_STANDALONE_INVENTORY")
	}
	baseline := strings.ToUpper(strings.TrimSpace(envOrDefault("SIM_STANDALONE_BASELINE", "A")))
	if baseline != "A" && baseline != "B" && baseline != "C" {
		log.Fatalf("invalid SIM_STANDALONE_BASELINE=%q (must be A, B, or C)", baseline)
	}
	if s.workload == nil {
		log.Fatal("standalone mode requires SIM_WORKLOAD_TRACE_PATH")
	}

	// Parse inventory and initialize nodes.
	nodes, err := loadInventory(inventoryPath)
	if err != nil {
		log.Fatalf("failed to load inventory: %v", err)
	}
	log.Printf("standalone: loaded inventory nodes=%d baseline=%s", len(nodes), baseline)

	// Initialize node state from inventory labels (same path as refreshNodeStateFromKubeData).
	nodeLabels := map[string]map[string]string{}
	for _, n := range nodes {
		nodeLabels[n.Name] = n.Labels
	}
	counts := map[string]int{} // no pods yet
	s.refreshNodeStateFromKubeData(counts, nil, nodeLabels)

	// Build resource tracker.
	tracker := map[string]*standaloneNodeTracker{}
	for _, n := range nodes {
		tracker[n.Name] = &standaloneNodeTracker{
			usedCPU: 0,
			usedGPU: 0,
		}
	}

	// Policy configuration.
	cpuEcoPct := floatEnv("SIM_SA_CPU_ECO_PCT", 60) / 100.0
	gpuEcoPct := floatEnv("SIM_SA_GPU_ECO_PCT", 70) / 100.0
	hpFrac := floatEnv("SIM_SA_HP_FRAC", 0.30)
	hpBaseFrac := floatEnv("SIM_SA_HP_BASE_FRAC", 0.30)
	hpMin := int(floatEnv("SIM_SA_HP_MIN", 1))
	hpMax := int(floatEnv("SIM_SA_HP_MAX", 25))
	perfPerHP := floatEnv("SIM_SA_PERF_PER_HP_NODE", 10)
	reconcileInterval := floatEnv("SIM_SA_RECONCILE_INTERVAL", 300)

	// Build lookup maps.
	nodeNames := make([]string, 0, len(nodes))
	nodeByName := map[string]*expandedNode{}
	for i, n := range nodes {
		nodeNames = append(nodeNames, n.Name)
		nodeByName[n.Name] = &nodes[i]
	}
	sort.Strings(nodeNames) // deterministic ordering

	// Apply initial power policy.
	applyPowerPolicy(s, tracker, nodeNames, nodeByName, baseline, hpFrac, cpuEcoPct, gpuEcoPct, 0, perfPerHP, hpBaseFrac, hpMin, hpMax)

	// --- Performance tuning knobs ---
	// SIM_SA_DT: seconds per tick (default 5). Larger = fewer ticks = faster.
	// Physics is stable at 5-10s steps for this workload model.
	dt := floatEnv("SIM_SA_DT", 5.0)
	// SIM_SA_SAMPLE_EVERY: emit one timeseries row every N ticks (default 1).
	// Reduces CSV I/O. With dt=5 and sample_every=1, one row per 5 sim-seconds.
	sampleEvery := int(floatEnv("SIM_SA_SAMPLE_EVERY", 1))
	if sampleEvery < 1 {
		sampleEvery = 1
	}

	// Parallel workers for physics computation.
	numWorkers := runtime.GOMAXPROCS(0)
	if v := int(floatEnv("SIM_SA_WORKERS", 0)); v > 0 {
		numWorkers = v
	}
	if numWorkers < 1 {
		numWorkers = 1
	}
	log.Printf("standalone: using %d parallel workers", numWorkers)

	s.workload.startTime = time.Now().UTC()
	virtualNow := s.workload.startTime
	tickCount := 0
	lastReconcileSec := 0.0
	totalJobs := len(s.workload.jobs)

	// Incremental counters — avoid O(totalJobs) scans per tick.
	submittedCount := 0
	completedCount := 0
	pendingCount := 0 // submitted but not completed

	// Job injection cursor — jobs are sorted by SubmitOffsetSec, so we
	// never need to re-scan already-submitted jobs.
	nextInjectIdx := 0

	// Active node set — only run physics on nodes that have jobs.
	activeNodes := map[string]bool{}

	// Timeout: truncate at steady state, not after drain.
	// SIM_SA_TIMEOUT is in wall-seconds (same time domain as trace submitOffsetSec).
	// Default: stop 10% after last job injection to capture steady-state, not cool-down.
	lastSubmitOffset := s.workload.jobs[totalJobs-1].SubmitOffsetSec
	defaultTimeout := lastSubmitOffset * 1.1
	if defaultTimeout < 600 {
		defaultTimeout = lastSubmitOffset + 300
	}
	timeout := floatEnv("SIM_SA_TIMEOUT", defaultTimeout)
	estTicks := int(timeout / dt)
	log.Printf("standalone: starting simulation jobs=%d dt=%.1fs timeout=%.0fs est_ticks=%d (last_inject=%.0fs)",
		totalJobs, dt, timeout, estTicks, lastSubmitOffset)

	for {
		tickCount++
		virtualNow = virtualNow.Add(time.Duration(dt * float64(time.Second)))
		elapsed := virtualNow.Sub(s.workload.startTime).Seconds()

		// 1. Inject ready jobs (bin-packing scheduler) — cursor-based, no cap.
		injected := standaloneInjectJobsFast(s, tracker, nodeNames, nodeByName, virtualNow, &nextInjectIdx, activeNodes)
		submittedCount += injected
		pendingCount += injected

		// 2. Advance job progress + accumulate energy (parallel across nodes).
		completed := standaloneTickParallel(s, tracker, nodeNames, nodeByName, dt, virtualNow, activeNodes, numWorkers)
		completedCount += completed
		pendingCount -= completed

		// 3. Update facility metrics (lightweight, uses precomputed power).
		s.updateFacilityMetrics(virtualNow)
		if tickCount%sampleEvery == 0 {
			s.appendTimeseriesRow(virtualNow)
		}

		// 4. Periodic operator reconcile for baseline C.
		if baseline == "C" && elapsed-lastReconcileSec >= reconcileInterval {
			perfIntentPods := countPerformanceSensitivePending(tracker, s.workload.jobs)
			applyPowerPolicy(s, tracker, nodeNames, nodeByName, baseline, hpFrac, cpuEcoPct, gpuEcoPct, perfIntentPods, perfPerHP, hpBaseFrac, hpMin, hpMax)
			lastReconcileSec = elapsed
		}

		// 5. Progress logging.
		if tickCount <= 5 || tickCount%100 == 0 {
			log.Printf("standalone: tick=%d elapsed=%.0fs submitted=%d completed=%d/%d pending=%d active_nodes=%d",
				tickCount, elapsed, submittedCount, completedCount, totalJobs, pendingCount, len(activeNodes))
		}

		// Truncate at timeout — captures steady state, avoids cool-down drain.
		if elapsed >= timeout {
			log.Printf("standalone: timeout reached ticks=%d elapsed=%.0fs submitted=%d completed=%d/%d (steady-state truncation)",
				tickCount, elapsed, submittedCount, completedCount, totalJobs)
			break
		}

		// All jobs submitted and completed → done early.
		if submittedCount >= totalJobs && completedCount >= totalJobs {
			log.Printf("standalone: all jobs complete ticks=%d elapsed=%.0fs completed=%d",
				tickCount, elapsed, completedCount)
			break
		}
	}

	// Flush timeseries.
	s.flushTimeseries()
	log.Printf("standalone: done, timeseries written (%d ticks, dt=%.1fs)", tickCount, dt)
}

func loadInventory(path string) ([]expandedNode, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read inventory: %w", err)
	}
	var inv inventoryFile
	if err := yaml.Unmarshal(data, &inv); err != nil {
		return nil, fmt.Errorf("parse inventory: %w", err)
	}

	var result []expandedNode
	for _, n := range inv.Nodes {
		replicas := n.Replicas
		if replicas <= 0 {
			replicas = 1
		}
		prefix := n.NodeNamePrefix
		if prefix == "" {
			prefix = n.NodeName
		}
		if prefix == "" {
			prefix = "kwok-node"
		}
		sockets := n.CPUSockets
		if sockets <= 0 {
			sockets = 2
		}
		cores := n.CPUCores
		if cores <= 0 {
			cores = 16
		}

		for idx := 0; idx < replicas; idx++ {
			name := fmt.Sprintf("%s-%d", prefix, idx)
			if replicas == 1 && n.NodeName != "" {
				name = n.NodeName
			}

			vendor := strings.ToLower(n.Vendor)
			if vendor == "" {
				if n.Product != "" {
					if strings.Contains(strings.ToLower(n.Product), "amd") {
						vendor = "amd"
					} else {
						vendor = "nvidia"
					}
				} else {
					vendor = "none"
				}
			}

			hasGPU := n.GPUCount > 0
			lbls := map[string]string{
				"type":                     "kwok",
				"joulie.io/managed":        "true",
				"joulie.io/node-name":      name,
				"joulie.io/hw.cpu-cores":   fmt.Sprintf("%d", cores),
				"joulie.io/hw.cpu-sockets": fmt.Sprintf("%d", sockets),
				"joulie.io/hw.gpu-count":   fmt.Sprintf("%d", n.GPUCount),
			}
			if hasGPU {
				lbls["joulie.io/hw.kind"] = "gpu"
			} else {
				lbls["joulie.io/hw.kind"] = "cpu-only"
			}
			if n.CPU != "" {
				lbls["joulie.io/hw.cpu-model"] = labelSafe(n.CPU)
			}
			if n.Product != "" {
				lbls["joulie.io/hw.gpu-model"] = labelSafe(n.Product)
				lbls["joulie.io/gpu.product"] = labelSafe(n.Product)
			}
			if vendor == "nvidia" && hasGPU {
				lbls["feature.node.kubernetes.io/pci-10de.present"] = "true"
			} else if vendor == "amd" && hasGPU {
				lbls["feature.node.kubernetes.io/pci-1002.present"] = "true"
			}

			result = append(result, expandedNode{
				Name:           name,
				Vendor:         vendor,
				Product:        n.Product,
				CPUModel:       n.CPU,
				CPUSockets:     sockets,
				CPUCores:       cores,
				MemoryGiB:      n.MemoryGiB,
				GPUCount:       n.GPUCount,
				GPUMinCapWatts: n.GPUMinCapWatts,
				GPUMaxCapWatts: n.GPUMaxCapWatts,
				Labels:         lbls,
			})
		}
	}
	return result, nil
}

// labelSafe sanitizes a string for use as a K8s label value.
func labelSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

// standaloneInjectJobsFast assigns ready jobs to nodes using scoring-based placement.
// Uses a cursor (nextIdx) to skip already-submitted jobs. Scans ALL ready jobs
// each tick so that unplaceable jobs (e.g. perf jobs when perf nodes are full)
// don't block placement of other job classes.
// Returns the number of jobs injected this tick.
func standaloneInjectJobsFast(s *simulator, tracker map[string]*standaloneNodeTracker, nodeNames []string, nodeByName map[string]*expandedNode, now time.Time, nextIdx *int, activeNodes map[string]bool) int {
	injected := 0
	jobs := s.workload.jobs
	deadline := now.Sub(s.workload.startTime).Seconds()

	// Advance cursor past already-submitted jobs.
	for *nextIdx < len(jobs) && jobs[*nextIdx].Submitted {
		*nextIdx++
	}

	// Scan all ready jobs from cursor position. Track placement failures
	// per job class to avoid O(n*m) scanning when a class is saturated,
	// while still allowing other classes to inject.
	type classKey struct {
		intent string
		gpu    bool
	}
	classFails := map[classKey]int{}
	const maxFailsPerClass = 100 // skip class after this many failures

	for i := *nextIdx; i < len(jobs); i++ {
		j := jobs[i]
		if j.SubmitOffsetSec > deadline {
			break // remaining jobs are in the future (sorted order)
		}
		if j.Submitted {
			continue
		}

		ck := classKey{intent: j.Class, gpu: j.RequestedGPUs > 0}
		if classFails[ck] >= maxFailsPerClass {
			continue // this class is saturated, skip
		}

		node := findNodeForJob(tracker, nodeNames, nodeByName, j)
		if node == "" {
			classFails[ck]++
			continue
		}

		placeJob(s, tracker, j, node, now, activeNodes)
		injected++
		classFails[ck] = 0 // reset on success
	}

	// Advance cursor past submitted jobs.
	for *nextIdx < len(jobs) && jobs[*nextIdx].Submitted {
		*nextIdx++
	}

	return injected
}

// placeJob assigns a job to a node and updates all tracking state.
func placeJob(s *simulator, tracker map[string]*standaloneNodeTracker, j *simJob, node string, now time.Time, activeNodes map[string]bool) {
	j.Submitted = true
	j.SubmittedAt = now
	j.LastProgressAt = now
	j.NodeName = node
	j.Namespace = "joulie-sim-demo"
	j.PodName = fmt.Sprintf("sim-%s", j.JobID)

	t := tracker[node]
	t.usedCPU += j.RequestedCPUCores
	t.usedGPU += j.RequestedGPUs
	t.jobs = append(t.jobs, j)
	activeNodes[node] = true

	// Track performance pod count for FSM draining and queue-aware scaling.
	if j.Class == "performance" {
		t.perfPodCount++
	}

	s.mu.Lock()
	if st := s.state[node]; st != nil {
		st.PodsRunning++
	}
	s.mu.Unlock()

	s.jobSubmitted.WithLabelValues(j.Class).Inc()
}

// findNodeForJob selects the best node for a job using the same logic as the
// real Joulie scheduler extender:
//  1. Filter: performance pods blocked from eco and draining nodes.
//  2. Score: headroom*0.7 + (100-coolingStress)*0.15 + adaptiveTrend(±25) + profileBonus + pressureRelief
//  3. Return highest-scoring node (ties broken by name order).
//
// The simulator approximates power headroom via CPU utilization (no real power
// telemetry). Pod marginal power is approximated by projecting the pod's CPU
// request onto the node's capacity. Cooling stress and trend are not simulated.
// In production, trend uses adaptive scaling: trendScale = 2.0 during cluster-wide
// bursts (|clusterTrend| > 500 W/min), 6.0 at steady state, capped at ±25 points.
func findNodeForJob(tracker map[string]*standaloneNodeTracker, nodeNames []string, nodeByName map[string]*expandedNode, j *simJob) string {
	needGPU := j.RequestedGPUs > 0
	isPerf := j.Class == "performance"

	// Compute performance node pressure (average utilization of perf nodes).
	var perfUtilSum, perfCount float64
	for _, name := range nodeNames {
		t := tracker[name]
		n := nodeByName[name]
		if t == nil || n == nil || t.isEco {
			continue
		}
		cpuCap := float64(n.CPUCores)
		if cpuCap > 0 {
			perfUtilSum += t.usedCPU / cpuCap * 100
		}
		perfCount++
	}
	perfPressure := 0.0
	if perfCount > 0 {
		perfPressure = perfUtilSum / perfCount
	}

	bestNode := ""
	bestScore := -1.0

	for _, name := range nodeNames {
		t := tracker[name]
		n := nodeByName[name]
		if n == nil || t == nil {
			continue
		}

		// --- FILTER STAGE (matches real scheduler extender) ---

		// Performance pods blocked from eco AND draining nodes.
		if isPerf && (t.isEco || t.isDraining) {
			continue
		}

		cpuCap := float64(n.CPUCores)
		gpuCap := float64(n.GPUCount)

		// Resource capacity check.
		if t.usedCPU+j.RequestedCPUCores > cpuCap {
			continue
		}
		if needGPU {
			if gpuCap <= 0 {
				continue
			}
			if t.usedGPU+j.RequestedGPUs > gpuCap {
				continue
			}
		}

		// --- SCORE STAGE (matches real scheduler extender) ---
		// Scoring formula:
		//   headroomScore = (cappedPower - projectedPower) / cappedPower * 100
		//   score = headroomScore * 0.7 + (100 - coolingStress) * 0.15 + adaptiveTrend(±25) + profileBonus + pressureRelief
		//
		// In the simulator, we approximate power via CPU utilization:
		//   measuredPower ≈ usedCPU / cpuCap (utilization fraction)
		//   podMarginal   ≈ requestedCPU / cpuCap
		//   cappedPower   = 1.0 (normalized)

		// Projected headroom: subtract pod's marginal from remaining capacity.
		projectedUtil := 0.0
		if cpuCap > 0 {
			projectedUtil = (t.usedCPU + j.RequestedCPUCores) / cpuCap
		}
		headroomScore := (1.0 - projectedUtil) * 100.0
		if headroomScore > 100 {
			headroomScore = 100
		}

		// Cooling stress not simulated — use 0 (no stress).
		coolingStress := 0.0

		// Trend not simulated — use 0. In production, uses adaptive scaling:
		// trendScale = 2.0 if |clusterTrend| > 500 W/min, else 6.0; capped at ±25.
		trendBonus := 0.0

		score := headroomScore*0.7 + (100-coolingStress)*0.15 + trendBonus

		// Profile-aware bonus: standard pods prefer eco nodes.
		if !isPerf && t.isEco {
			score += 10
		}

		// Adaptive pressure relief: steer standard pods away from congested perf nodes.
		if !isPerf && !t.isEco {
			score -= perfPressure * 0.3 // up to -30 at full saturation
		}

		// Clamp to [0, 100].
		if score < 0 {
			score = 0
		}
		if score > 100 {
			score = 100
		}

		if score > bestScore {
			bestScore = score
			bestNode = name
		}
	}
	return bestNode
}

// standaloneAdvanceJobProgressFast is the standalone equivalent of advanceJobProgress.
// It only processes active nodes (those with running jobs) and returns the number
// of jobs completed this tick.
func standaloneAdvanceJobProgressFast(s *simulator, tracker map[string]*standaloneNodeTracker, dt float64, now time.Time, activeNodes map[string]bool) int {
	if s.workload == nil {
		return 0
	}
	completedThisTick := 0

	// Only iterate nodes that have active jobs.
	for nodeName := range activeNodes {
		t := tracker[nodeName]
		if t == nil || len(t.jobs) == 0 {
			delete(activeNodes, nodeName)
			continue
		}

		s.mu.RLock()
		st := s.state[nodeName]
		model := s.model
		if m, ok := s.nodeModels[nodeName]; ok {
			model = m
		}
		freqScale := 1.0
		gpuCapFactor := 1.0
		gpuCount := 0.0
		if st != nil {
			freqScale = st.FreqScale
			if model.GPU.Count > 0 && model.GPU.MaxWattsPerGPU > 0 {
				gpuCount = float64(model.GPU.Count)
				cap := st.GPUCapWattsPerGpu
				if cap <= 0 {
					cap = model.GPU.MaxWattsPerGPU
				}
				gpuCapFactor = clamp01(cap / model.GPU.MaxWattsPerGPU)
			}
		}
		s.mu.RUnlock()

		if st == nil {
			continue
		}

		// Compute aggregate resource demand.
		activeJobs := make([]*simJob, 0, len(t.jobs))
		totalCPUReq := 0.0
		totalGPUReq := 0.0
		totalCPUUtilDemand := 0.0
		totalGPUUtilDemand := 0.0
		totalMemoryWeight := 0.0
		totalIOWeight := 0.0
		totalCPUFeedWeight := 0.0
		memoryWeighted := 0.0
		ioWeighted := 0.0
		cpuFeedWeighted := 0.0
		cpuClassWeights := map[string]float64{}
		gpuClassWeights := map[string]float64{}

		for _, j := range t.jobs {
			if j.Completed {
				continue
			}
			activeJobs = append(activeJobs, j)
			if j.CPUUnitsRemaining > 0 {
				weight := math.Max(0.1, j.RequestedCPUCores)
				totalCPUReq += math.Max(0.1, j.RequestedCPUCores)
				cpuClassWeights[j.CPUWorkClass] += weight
				totalCPUUtilDemand += j.RequestedCPUCores * clamp01(j.CPUUtilTarget)
				memoryWeighted += weight * clamp01(j.MemoryIntensity)
				ioWeighted += weight * clamp01(j.IOIntensity)
				totalMemoryWeight += weight
				totalIOWeight += weight
			}
			if j.GPUUnitsRemaining > 0 {
				weight := math.Max(0.1, j.RequestedGPUs)
				totalGPUReq += j.RequestedGPUs
				totalGPUUtilDemand += j.RequestedGPUs * clamp01(j.GPUUtilTarget)
				gpuClassWeights[j.GPUWorkClass] += weight
				memoryWeighted += weight * clamp01(j.MemoryIntensity)
				cpuFeedWeighted += weight * clamp01(j.CPUFeedIntensity)
				totalMemoryWeight += weight
				totalCPUFeedWeight += weight
			}
		}

		// Update node utilization state.
		cpuCapacity := st.CPUCapacityCores
		if cpuCapacity <= 0 {
			cpuCapacity = 16
		}
		s.mu.Lock()
		st.CPUUtil = clamp01(totalCPUUtilDemand / cpuCapacity)
		if gpuCount > 0 {
			if totalGPUReq > 0 {
				st.GPUUtil = clamp01(totalGPUUtilDemand / gpuCount)
			} else {
				st.GPUUtil = 0
			}
		} else {
			st.GPUUtil = 0
		}
		st.CPUWorkClass = dominantWorkClass(cpuClassWeights, "cpu.mixed")
		st.GPUWorkClass = dominantWorkClass(gpuClassWeights, "gpu.mixed")
		if totalMemoryWeight > 0 {
			st.MemoryIntensity = clamp01(memoryWeighted / totalMemoryWeight)
		} else {
			st.MemoryIntensity = 0
		}
		if totalIOWeight > 0 {
			st.IOIntensity = clamp01(ioWeighted / totalIOWeight)
		} else {
			st.IOIntensity = 0
		}
		if totalCPUFeedWeight > 0 {
			st.CPUFeedIntensity = clamp01(cpuFeedWeighted / totalCPUFeedWeight)
		} else {
			st.CPUFeedIntensity = 0
		}
		snapCPUCapacity := st.CPUCapacityCores
		snapThermalThrottle := st.CPUThermalThrottle
		snapGPUCapPerGpu := st.GPUCapWattsPerGpu
		s.mu.Unlock()

		cpuCapacityF := 16.0
		if snapCPUCapacity > 0 {
			cpuCapacityF = snapCPUCapacity
		}
		cpuShareFactor := 1.0
		if totalCPUReq > cpuCapacityF && cpuCapacityF > 0 {
			cpuShareFactor = clamp01(cpuCapacityF / totalCPUReq)
		}
		gpuShareFactor := 1.0
		if gpuCount > 0 && totalGPUReq > gpuCount {
			gpuShareFactor = clamp01(gpuCount / totalGPUReq)
		}

		// Advance each job.
		for _, j := range activeJobs {
			jobThermalThrottle := snapThermalThrottle
			jobCPUMul := cpuThroughputMultiplier(freqScale, j.CPUWorkClass, model, j.MemoryIntensity, j.IOIntensity, jobThermalThrottle)

			if j.CPUUnitsRemaining > 0 {
				cpuThrottle := cpuThrottleImpactFactor(jobCPUMul, j)
				speed := j.RequestedCPUCores * s.workload.baseSpeedCore * cpuThrottle * cpuShareFactor
				if speed < 0 {
					speed = 0
				}
				prev := j.CPUUnitsRemaining
				j.CPUUnitsRemaining -= speed * dt
				if j.CPUUnitsRemaining < prev {
					j.LastProgressAt = now
				}
			}

			if j.GPUUnitsRemaining > 0 {
				gpuBase := math.Max(0.1, j.RequestedGPUs) * s.workload.baseSpeedCore
				gpuPhys := phys.CappedBoardGPUModel{
					IdleW:         model.GPU.IdleWattsPerGPU,
					MaxW:          model.GPU.MaxWattsPerGPU,
					ComputeGamma:  model.GPU.ComputeGamma,
					MemoryEpsilon: model.GPU.MemoryEpsilon,
					MemoryGamma:   model.GPU.MemoryGamma,
				}
				gpuMul := gpuPhys.ThroughputMultiplier(phys.DeviceState{
					Utilization:      clamp01(j.GPUUtilTarget),
					CapWatts:         snapGPUCapPerGpu,
					MaxCapWatts:      model.GPU.MaxWattsPerGPU,
					MemoryIntensity:  j.MemoryIntensity,
					CPUFeedIntensity: j.CPUFeedIntensity,
					ThermalThrottle:  snapThermalThrottle,
					Class:            j.GPUWorkClass,
				}, j.GPUWorkClass)
				cpuFeedFac := cpuFeedThrottleFactor(jobCPUMul, j)
				gpuCapImpact := 1.0 - (1.0-gpuCapFactor)*j.SensitivityGPU
				gpuSpeed := gpuBase * gpuMul * cpuFeedFac * gpuCapImpact * gpuShareFactor
				if gpuSpeed < 0 {
					gpuSpeed = 0
				}
				prev := j.GPUUnitsRemaining
				j.GPUUnitsRemaining -= gpuSpeed * dt
				if j.GPUUnitsRemaining < prev {
					j.LastProgressAt = now
				}
			}

			if j.CPUUnitsRemaining <= 0 && j.GPUUnitsRemaining <= 0 {
				j.CPUUnitsRemaining = 0
				j.GPUUnitsRemaining = 0
				j.Completed = true
				j.CompletedAt = now
				completedThisTick++

				// Release resources from tracker.
				t.usedCPU -= j.RequestedCPUCores
				t.usedGPU -= j.RequestedGPUs
				if t.usedCPU < 0 {
					t.usedCPU = 0
				}
				if t.usedGPU < 0 {
					t.usedGPU = 0
				}
				if j.Class == "performance" {
					t.perfPodCount--
					if t.perfPodCount < 0 {
						t.perfPodCount = 0
					}
				}

				s.mu.Lock()
				if st != nil {
					st.PodsRunning--
					if st.PodsRunning < 0 {
						st.PodsRunning = 0
					}
				}
				s.mu.Unlock()

				s.jobCompleted.WithLabelValues(j.Class, nodeName).Inc()
				if !j.SubmittedAt.IsZero() {
					s.jobCompletion.Observe(j.CompletedAt.Sub(j.SubmittedAt).Seconds())
				}
			}
		}

		// Compact completed jobs from tracker every tick (fast path).
		alive := t.jobs[:0] // reuse backing array
		for _, j := range t.jobs {
			if !j.Completed {
				alive = append(alive, j)
			}
		}
		t.jobs = alive
		if len(t.jobs) == 0 {
			delete(activeNodes, nodeName)
		}
	}
	return completedThisTick
}

// standaloneTickParallel runs job progress + energy accumulation in parallel
// across numWorkers goroutines. Each worker handles a disjoint set of active nodes,
// so no locking is needed for per-node state.
func standaloneTickParallel(s *simulator, tracker map[string]*standaloneNodeTracker, allNodeNames []string, nodeByName map[string]*expandedNode, dt float64, now time.Time, activeNodes map[string]bool, numWorkers int) int {
	// Build list of active node names for partitioning.
	activeList := make([]string, 0, len(activeNodes))
	for name := range activeNodes {
		if t := tracker[name]; t != nil && len(t.jobs) > 0 {
			activeList = append(activeList, name)
		}
	}

	// For small workloads, skip goroutine overhead.
	if numWorkers <= 1 || len(activeList) < 32 {
		completed := standaloneAdvanceJobProgressFast(s, tracker, dt, now, activeNodes)
		s.accumulateEnergy(dt)
		return completed
	}

	// --- Parallel job progress ---
	var completedTotal int64
	var wg sync.WaitGroup

	type completionRec struct {
		class    string
		node     string
		duration float64
	}
	// Per-worker buffers to avoid contention on prometheus metrics and maps.
	type workerResult struct {
		completed   int64
		completions []completionRec
		emptyNodes  []string
		energyByNode map[string]float64
		energyTotal  float64
	}
	results := make([]workerResult, numWorkers)

	chunkSize := (len(activeList) + numWorkers - 1) / numWorkers
	baseSpeedCore := s.workload.baseSpeedCore

	for w := 0; w < numWorkers; w++ {
		start := w * chunkSize
		if start >= len(activeList) {
			break
		}
		end := start + chunkSize
		if end > len(activeList) {
			end = len(activeList)
		}

		wg.Add(1)
		go func(workerID int, nodes []string) {
			defer wg.Done()
			res := workerResult{energyByNode: make(map[string]float64, len(nodes))}

			for _, nodeName := range nodes {
				t := tracker[nodeName]
				if t == nil || len(t.jobs) == 0 {
					res.emptyNodes = append(res.emptyNodes, nodeName)
					continue
				}

				st := s.state[nodeName]
				model := s.model
				if m, ok := s.nodeModels[nodeName]; ok {
					model = m
				}
				if st == nil {
					continue
				}

				freqScale := st.FreqScale
				gpuCapFactor := 1.0
				gpuCount := 0.0
				if model.GPU.Count > 0 && model.GPU.MaxWattsPerGPU > 0 {
					gpuCount = float64(model.GPU.Count)
					cap := st.GPUCapWattsPerGpu
					if cap <= 0 {
						cap = model.GPU.MaxWattsPerGPU
					}
					gpuCapFactor = clamp01(cap / model.GPU.MaxWattsPerGPU)
				}

				// Compute aggregate resource demand.
				activeJobs := make([]*simJob, 0, len(t.jobs))
				totalCPUReq := 0.0
				totalGPUReq := 0.0
				totalCPUUtilDemand := 0.0
				totalGPUUtilDemand := 0.0
				totalMemoryWeight := 0.0
				totalIOWeight := 0.0
				totalCPUFeedWeight := 0.0
				memoryWeighted := 0.0
				ioWeighted := 0.0
				cpuFeedWeighted := 0.0
				cpuClassWeights := map[string]float64{}
				gpuClassWeights := map[string]float64{}

				for _, j := range t.jobs {
					if j.Completed {
						continue
					}
					activeJobs = append(activeJobs, j)
					if j.CPUUnitsRemaining > 0 {
						weight := math.Max(0.1, j.RequestedCPUCores)
						totalCPUReq += math.Max(0.1, j.RequestedCPUCores)
						cpuClassWeights[j.CPUWorkClass] += weight
						totalCPUUtilDemand += j.RequestedCPUCores * clamp01(j.CPUUtilTarget)
						memoryWeighted += weight * clamp01(j.MemoryIntensity)
						ioWeighted += weight * clamp01(j.IOIntensity)
						totalMemoryWeight += weight
						totalIOWeight += weight
					}
					if j.GPUUnitsRemaining > 0 {
						weight := math.Max(0.1, j.RequestedGPUs)
						totalGPUReq += j.RequestedGPUs
						totalGPUUtilDemand += j.RequestedGPUs * clamp01(j.GPUUtilTarget)
						gpuClassWeights[j.GPUWorkClass] += weight
						memoryWeighted += weight * clamp01(j.MemoryIntensity)
						cpuFeedWeighted += weight * clamp01(j.CPUFeedIntensity)
						totalMemoryWeight += weight
						totalCPUFeedWeight += weight
					}
				}

				// Update node utilization (no lock needed — disjoint partition).
				cpuCapacity := st.CPUCapacityCores
				if cpuCapacity <= 0 {
					cpuCapacity = 16
				}
				st.CPUUtil = clamp01(totalCPUUtilDemand / cpuCapacity)
				if gpuCount > 0 && totalGPUReq > 0 {
					st.GPUUtil = clamp01(totalGPUUtilDemand / gpuCount)
				} else {
					st.GPUUtil = 0
				}
				st.CPUWorkClass = dominantWorkClass(cpuClassWeights, "cpu.mixed")
				st.GPUWorkClass = dominantWorkClass(gpuClassWeights, "gpu.mixed")
				if totalMemoryWeight > 0 {
					st.MemoryIntensity = clamp01(memoryWeighted / totalMemoryWeight)
				} else {
					st.MemoryIntensity = 0
				}
				if totalIOWeight > 0 {
					st.IOIntensity = clamp01(ioWeighted / totalIOWeight)
				} else {
					st.IOIntensity = 0
				}
				if totalCPUFeedWeight > 0 {
					st.CPUFeedIntensity = clamp01(cpuFeedWeighted / totalCPUFeedWeight)
				} else {
					st.CPUFeedIntensity = 0
				}

				snapCPUCapacity := st.CPUCapacityCores
				snapThermalThrottle := st.CPUThermalThrottle
				snapGPUCapPerGpu := st.GPUCapWattsPerGpu

				cpuCapacityF := 16.0
				if snapCPUCapacity > 0 {
					cpuCapacityF = snapCPUCapacity
				}
				cpuShareFactor := 1.0
				if totalCPUReq > cpuCapacityF && cpuCapacityF > 0 {
					cpuShareFactor = clamp01(cpuCapacityF / totalCPUReq)
				}
				gpuShareFactor := 1.0
				if gpuCount > 0 && totalGPUReq > gpuCount {
					gpuShareFactor = clamp01(gpuCount / totalGPUReq)
				}

				// Advance each job.
				for _, j := range activeJobs {
					jobThermalThrottle := snapThermalThrottle
					jobCPUMul := cpuThroughputMultiplier(freqScale, j.CPUWorkClass, model, j.MemoryIntensity, j.IOIntensity, jobThermalThrottle)

					if j.CPUUnitsRemaining > 0 {
						cpuThrottle := cpuThrottleImpactFactor(jobCPUMul, j)
						speed := j.RequestedCPUCores * baseSpeedCore * cpuThrottle * cpuShareFactor
						if speed < 0 {
							speed = 0
						}
						prev := j.CPUUnitsRemaining
						j.CPUUnitsRemaining -= speed * dt
						if j.CPUUnitsRemaining < prev {
							j.LastProgressAt = now
						}
					}

					if j.GPUUnitsRemaining > 0 {
						gpuBase := math.Max(0.1, j.RequestedGPUs) * baseSpeedCore
						gpuPhys := phys.CappedBoardGPUModel{
							IdleW:         model.GPU.IdleWattsPerGPU,
							MaxW:          model.GPU.MaxWattsPerGPU,
							ComputeGamma:  model.GPU.ComputeGamma,
							MemoryEpsilon: model.GPU.MemoryEpsilon,
							MemoryGamma:   model.GPU.MemoryGamma,
						}
						gpuMul := gpuPhys.ThroughputMultiplier(phys.DeviceState{
							Utilization:      clamp01(j.GPUUtilTarget),
							CapWatts:         snapGPUCapPerGpu,
							MaxCapWatts:      model.GPU.MaxWattsPerGPU,
							MemoryIntensity:  j.MemoryIntensity,
							CPUFeedIntensity: j.CPUFeedIntensity,
							ThermalThrottle:  snapThermalThrottle,
							Class:            j.GPUWorkClass,
						}, j.GPUWorkClass)
						cpuFeedFac := cpuFeedThrottleFactor(jobCPUMul, j)
						gpuCapImpact := 1.0 - (1.0-gpuCapFactor)*j.SensitivityGPU
						gpuSpeed := gpuBase * gpuMul * cpuFeedFac * gpuCapImpact * gpuShareFactor
						if gpuSpeed < 0 {
							gpuSpeed = 0
						}
						prev := j.GPUUnitsRemaining
						j.GPUUnitsRemaining -= gpuSpeed * dt
						if j.GPUUnitsRemaining < prev {
							j.LastProgressAt = now
						}
					}

					if j.CPUUnitsRemaining <= 0 && j.GPUUnitsRemaining <= 0 {
						j.CPUUnitsRemaining = 0
						j.GPUUnitsRemaining = 0
						j.Completed = true
						j.CompletedAt = now
						res.completed++

						t.usedCPU -= j.RequestedCPUCores
						t.usedGPU -= j.RequestedGPUs
						if t.usedCPU < 0 {
							t.usedCPU = 0
						}
						if t.usedGPU < 0 {
							t.usedGPU = 0
						}
						if j.Class == "performance" {
							t.perfPodCount--
							if t.perfPodCount < 0 {
								t.perfPodCount = 0
							}
						}
						st.PodsRunning--
						if st.PodsRunning < 0 {
							st.PodsRunning = 0
						}

						dur := 0.0
						if !j.SubmittedAt.IsZero() {
							dur = j.CompletedAt.Sub(j.SubmittedAt).Seconds()
						}
						res.completions = append(res.completions, completionRec{
							class: j.Class, node: nodeName, duration: dur,
						})
					}
				}

				// Compact completed jobs.
				alive := t.jobs[:0]
				for _, j := range t.jobs {
					if !j.Completed {
						alive = append(alive, j)
					}
				}
				t.jobs = alive
				if len(t.jobs) == 0 {
					res.emptyNodes = append(res.emptyNodes, nodeName)
				}

				// Compute energy for this node (local accumulation, merged after).
				p := s.nodePowerWithModel(st, model)
				e := p * dt
				if e > 0 {
					res.energyByNode[nodeName] += e
					res.energyTotal += e
				}
			}

			atomic.AddInt64(&completedTotal, res.completed)
			results[workerID] = res
		}(w, activeList[start:end])
	}
	wg.Wait()

	// --- Parallel energy for IDLE nodes (not in activeList) ---
	activeSet := make(map[string]bool, len(activeList))
	for _, name := range activeList {
		activeSet[name] = true
	}
	idleList := make([]string, 0, len(allNodeNames)-len(activeList))
	for _, name := range allNodeNames {
		if !activeSet[name] {
			idleList = append(idleList, name)
		}
	}
	if len(idleList) > 0 {
		idleChunk := (len(idleList) + numWorkers - 1) / numWorkers
		idleEnergy := make([]float64, numWorkers)
		idleEnergyMaps := make([]map[string]float64, numWorkers)
		var wg2 sync.WaitGroup
		for w := 0; w < numWorkers; w++ {
			start := w * idleChunk
			if start >= len(idleList) {
				break
			}
			end := start + idleChunk
			if end > len(idleList) {
				end = len(idleList)
			}
			wg2.Add(1)
			go func(wid int, nodes []string) {
				defer wg2.Done()
				localE := make(map[string]float64, len(nodes))
				var total float64
				for _, name := range nodes {
					st := s.state[name]
					if st == nil {
						continue
					}
					model := s.model
					if m, ok := s.nodeModels[name]; ok {
						model = m
					}
					p := s.nodePowerWithModel(st, model)
					e := p * dt
					if e > 0 {
						localE[name] = e
						total += e
					}
				}
				idleEnergy[wid] = total
				idleEnergyMaps[wid] = localE
			}(w, idleList[start:end])
		}
		wg2.Wait()
		// Merge idle energy.
		for wid := range idleEnergyMaps {
			s.energyTotalJ += idleEnergy[wid]
			for name, e := range idleEnergyMaps[wid] {
				s.energyJByNode[name] += e
			}
		}
	}

	// Flush completion metrics, energy, and remove empty nodes (single-threaded).
	for _, res := range results {
		for _, c := range res.completions {
			s.jobCompleted.WithLabelValues(c.class, c.node).Inc()
			if c.duration > 0 {
				s.jobCompletion.Observe(c.duration)
			}
		}
		for _, name := range res.emptyNodes {
			delete(activeNodes, name)
		}
		// Merge energy from active nodes.
		s.energyTotalJ += res.energyTotal
		for name, e := range res.energyByNode {
			s.energyJByNode[name] += e
		}
	}

	return int(completedTotal)
}

// nodeFamily returns the hardware family key for a node (matches operator's NodeFamily).
func nodeFamily(n *expandedNode) string {
	if n.GPUCount > 0 {
		model := n.Product
		if model == "" {
			model = "unknown-gpu"
		}
		return "gpu:" + model
	}
	model := n.CPUModel
	if model == "" {
		model = "unknown-cpu"
	}
	return "cpu:" + model
}

// selectPerformanceNodes picks hpCount nodes for performance, prioritizing
// one node from each hardware family before filling remaining slots.
// Matches the real operator's selectPerformanceNodes in policy.go.
func selectPerformanceNodes(nodeNames []string, nodeByName map[string]*expandedNode, hpCount int) map[string]bool {
	perfNodes := make(map[string]bool, hpCount)
	seenFamilies := make(map[string]struct{}, len(nodeNames))
	// First pass: one node per family.
	for _, name := range nodeNames {
		n := nodeByName[name]
		if n == nil {
			continue
		}
		family := nodeFamily(n)
		if _, ok := seenFamilies[family]; ok {
			continue
		}
		perfNodes[name] = true
		seenFamilies[family] = struct{}{}
		if len(perfNodes) >= hpCount {
			return perfNodes
		}
	}
	// Second pass: fill remaining slots.
	for _, name := range nodeNames {
		if perfNodes[name] {
			continue
		}
		perfNodes[name] = true
		if len(perfNodes) >= hpCount {
			break
		}
	}
	return perfNodes
}

// countPerformanceSensitivePending counts pending+running performance-class pods
// across all nodes. This matches the real operator's queue-aware metric.
func countPerformanceSensitivePending(tracker map[string]*standaloneNodeTracker, jobs []*simJob) int {
	count := 0
	// Running performance pods on all nodes.
	for _, t := range tracker {
		count += t.perfPodCount
	}
	// Pending (submitted but not placed yet) performance jobs — these are still
	// in the job list but haven't been assigned a node.
	for _, j := range jobs {
		if j.Submitted && !j.Completed && j.NodeName == "" && j.Class == "performance" {
			count++
		}
	}
	return count
}

// applyPowerPolicy sets eco/performance labels and corresponding power caps on nodes.
// Matches the real operator logic:
//   - Family diversity: at least 1 perf node per hardware family
//   - FSM draining: nodes with running perf pods can't instantly transition to eco
//   - Queue-aware (C): counts performance-sensitive pods, not all pending
func applyPowerPolicy(s *simulator, tracker map[string]*standaloneNodeTracker, nodeNames []string, nodeByName map[string]*expandedNode, baseline string, hpFrac, cpuEcoPct, gpuEcoPct float64, perfIntentPods int, perfPerHP, hpBaseFrac float64, hpMin, hpMax int) {
	if baseline == "A" {
		// Baseline A: all nodes at full power, no eco.
		s.mu.Lock()
		for _, name := range nodeNames {
			if st := s.state[name]; st != nil {
				st.FreqScale = 1.0
				st.TargetThrottlePct = 0
				model := s.model
				if m, ok := s.nodeModels[name]; ok {
					model = m
				}
				st.GPUCapWattsPerGpu = model.GPU.MaxWattsPerGPU
				st.GPUTargetCapWattsPerGpu = model.GPU.MaxWattsPerGPU
			}
			if t := tracker[name]; t != nil {
				t.isEco = false
				t.isDraining = false
			}
		}
		s.mu.Unlock()
		return
	}

	// Determine number of HP (performance) nodes.
	totalNodes := len(nodeNames)
	var hpCount int
	if baseline == "B" {
		hpCount = int(math.Round(hpFrac * float64(totalNodes)))
	} else {
		// Baseline C: queue-aware dynamic HP count based on performance-sensitive pods.
		baseCount := int(math.Round(hpBaseFrac * float64(totalNodes)))
		queueNeed := int(math.Ceil(float64(perfIntentPods) / perfPerHP))
		hpCount = baseCount
		if queueNeed > hpCount {
			hpCount = queueNeed
		}
		if hpCount < hpMin {
			hpCount = hpMin
		}
		if hpCount > hpMax {
			hpCount = hpMax
		}
	}
	if hpCount > totalNodes {
		hpCount = totalNodes
	}
	if hpCount < 0 {
		hpCount = 0
	}

	// Enforce family diversity: at least 1 perf node per HW family.
	families := map[string]struct{}{}
	for _, name := range nodeNames {
		if n := nodeByName[name]; n != nil {
			families[nodeFamily(n)] = struct{}{}
		}
	}
	if len(families) > hpCount {
		hpCount = len(families)
	}
	if hpCount > totalNodes {
		hpCount = totalNodes
	}

	// Select performance nodes with family diversity.
	perfNodes := selectPerformanceNodes(nodeNames, nodeByName, hpCount)

	s.mu.Lock()
	for _, name := range nodeNames {
		st := s.state[name]
		t := tracker[name]
		if st == nil || t == nil {
			continue
		}
		model := s.model
		if m, ok := s.nodeModels[name]; ok {
			model = m
		}

		isHP := perfNodes[name]
		if isHP {
			// Performance node: full power, no throttle.
			st.FreqScale = 1.0
			st.TargetThrottlePct = 0
			st.GPUCapWattsPerGpu = model.GPU.MaxWattsPerGPU
			st.GPUTargetCapWattsPerGpu = model.GPU.MaxWattsPerGPU
			t.isEco = false
			t.isDraining = false
		} else {
			// FSM draining guard: if node still has perf pods, enter draining
			// state instead of immediately applying eco caps. Draining nodes
			// keep performance caps but reject new performance pods.
			if t.perfPodCount > 0 {
				// DrainingPerformance: keep perf caps, mark as draining.
				st.FreqScale = 1.0
				st.TargetThrottlePct = 0
				st.GPUCapWattsPerGpu = model.GPU.MaxWattsPerGPU
				st.GPUTargetCapWattsPerGpu = model.GPU.MaxWattsPerGPU
				t.isEco = false
				t.isDraining = true
			} else {
				// ActiveEco: apply eco caps.
				throttlePct := int(math.Round((1.0 - cpuEcoPct) * 100))
				st.FreqScale = cpuEcoPct
				st.TargetThrottlePct = throttlePct
				if model.GPU.MaxWattsPerGPU > 0 {
					ecoCap := gpuEcoPct * model.GPU.MaxWattsPerGPU
					st.GPUCapWattsPerGpu = ecoCap
					st.GPUTargetCapWattsPerGpu = ecoCap
				}
				t.isEco = true
				t.isDraining = false
			}
		}
	}
	s.mu.Unlock()
}

// countPendingJobs returns the number of submitted but not-yet-completed jobs.
// Only used by the non-standalone code path; standalone tracks this incrementally.
func countPendingJobs(s *simulator) int {
	count := 0
	for _, j := range s.workload.jobs {
		if j.Submitted && !j.Completed {
			count++
		}
	}
	return count
}
