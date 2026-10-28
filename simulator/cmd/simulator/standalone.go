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

	"github.com/matbun/joulie/pkg/controller/policy"
	"github.com/matbun/joulie/pkg/hwinv"
	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"sigs.k8s.io/yaml"
)

// ---------------------------------------------------------------------------
// Standalone simulation mode: bypasses K8s entirely, runs at CPU speed.
// Activated by SIM_STANDALONE=true.
//
// Required env vars:
//   SIM_STANDALONE_INVENTORY  path to cluster-nodes.yaml
//   SIM_WORKLOAD_TRACE_PATH   path to workload trace JSONL
//   SIM_STANDALONE_BASELINE   A, B, or C
//   SIM_DEBUG_PERSIST_DIR     output directory (timeseries.csv written here)
//
// Optional env vars (same as normal mode):
//   SIM_BASE_SPEED_PER_CORE, SIM_FACILITY_AMBIENT_TEMP_C,
//   SIM_FACILITY_TEMP_AMPLITUDE_C, SIM_FACILITY_TEMP_PERIOD_H,
//   SIM_PROFILES_DIR (local hwemu profiles an inventory entry may name)
//
// Policy env vars (for baselines B and C):
//   SIM_SA_CPU_ECO_PCT          eco CPU frequency scale (0-100), default 60
//   SIM_SA_GPU_ECO_PCT          eco GPU cap as % of max, default 70
//   SIM_SA_HP_FRAC              static HP fraction for baseline B, default 0.30
//   SIM_SA_HP_BASE_FRAC         base HP fraction for baseline C, default 0.30
//   SIM_SA_HP_MIN               min HP nodes for baseline C, default 1
//   SIM_SA_HP_MAX               max HP nodes for baseline C, default 25
//   SIM_SA_PERF_PER_HP_NODE     queue depth per HP node for baseline C, default 10
//   SIM_SA_RECONCILE_INTERVAL   controller manager reconcile sim-seconds for C, default 300
//
// Physics env vars:
//   SIM_SA_SUBSTEP              longest physics substep in sim-seconds, default SIM_SA_DT
// ---------------------------------------------------------------------------

// inventoryFile is the top-level YAML structure for cluster-nodes.yaml.
type inventoryFile struct {
	Nodes []inventoryNode `json:"nodes"`
}

// inventoryNode is one entry of the inventory. Profile names an hwemu
// profile (builtin, or local in SIM_PROFILES_DIR) that sets the node's
// hardware; the hardware keys below it then stay unset.
type inventoryNode struct {
	NodeNamePrefix string  `json:"node_name_prefix"`
	NodeName       string  `json:"node_name"`
	Replicas       int     `json:"replicas"`
	Profile        string  `json:"profile"`
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
	// Profile is the hwemu profile of a profile entry, nil for a legacy one.
	Profile *hwemu.Profile
	// EcoPowerCap marks a profile node with a writable PL1: eco caps its CPU
	// power, as the agent does, instead of scaling its frequency.
	EcoPowerCap bool
	// AgentCPUCapRange is the catalog CPU cap range a pool agent resolves a
	// percent cap against on this legacy node, which it caps over HTTP
	// without RAPL files (cmd/agent resolveCPUCapFromPct); nil for a
	// profile node, a GPU node or a CPU the catalog does not know, which
	// that agent throttles instead. initStandaloneNodes sets it.
	AgentCPUCapRange *hwinv.NodeCPUCapRange
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

	if err := initStandaloneNodes(s, nodes); err != nil {
		log.Fatalf("standalone: %v", err)
	}

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
	// SIM_SA_SUBSTEP: the longest physics substep in sim-seconds (default
	// SIM_SA_DT, one substep per tick). stepNode splits each tick into
	// equal substeps no longer than this.
	substep := time.Duration(floatEnv("SIM_SA_SUBSTEP", dt) * float64(time.Second))

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
	// The nodes' physics runs on the virtual clock from its start.
	s.mu.Lock()
	for _, name := range nodeNames {
		if st := s.state[name]; st != nil {
			st.SimTime = virtualNow
		}
	}
	s.mu.Unlock()
	tickCount := 0
	lastReconcileSec := 0.0
	totalJobs := len(s.workload.jobs)

	// Incremental counters: avoid O(totalJobs) scans per tick.
	submittedCount := 0
	completedCount := 0
	pendingCount := 0 // submitted but not completed

	// Job injection cursor: jobs are sorted by SubmitOffsetSec, so we
	// never need to re-scan already-submitted jobs.
	nextInjectIdx := 0

	// Active node set: only nodes that have jobs run job progress; the
	// physics steps every node.
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

		// 1. Inject ready jobs (bin-packing scheduler), cursor-based, no cap.
		s.workload.jobsMu.Lock()
		injected := standaloneInjectJobsFast(s, tracker, nodeNames, nodeByName, virtualNow, &nextInjectIdx, activeNodes)
		submittedCount += injected
		pendingCount += injected

		// 2. Advance job progress, then step every node's physics by dt
		// and book its energy (parallel across nodes).
		completed := standaloneTickParallel(s, tracker, nodeNames, nodeByName, dt, substep, virtualNow, activeNodes, numWorkers)
		s.workload.jobsMu.Unlock()
		completedCount += completed
		pendingCount -= completed

		// 3. Update facility metrics (lightweight, uses precomputed power).
		s.updateFacilityMetrics(virtualNow)
		if tickCount%sampleEvery == 0 {
			s.appendTimeseriesRow(virtualNow)
		}

		// 4. Periodic controller manager reconcile for baseline C.
		if baseline == "C" && elapsed-lastReconcileSec >= reconcileInterval {
			perfIntentPods := countPerformanceSensitivePending(tracker, s.workload.jobs, virtualNow.Sub(s.workload.startTime).Seconds())
			applyPowerPolicy(s, tracker, nodeNames, nodeByName, baseline, hpFrac, cpuEcoPct, gpuEcoPct, perfIntentPods, perfPerHP, hpBaseFrac, hpMin, hpMax)
			lastReconcileSec = elapsed
		}

		// 5. Progress logging.
		if tickCount <= 5 || tickCount%100 == 0 {
			log.Printf("standalone: tick=%d elapsed=%.0fs submitted=%d completed=%d/%d pending=%d active_nodes=%d",
				tickCount, elapsed, submittedCount, completedCount, totalJobs, pendingCount, len(activeNodes))
		}

		// Truncate at timeout: captures steady state, avoids cool-down drain.
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

	// Flush the timeseries and close the journal.
	s.closePersistence()
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

	var profiles map[string]*hwemu.Profile
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
		var profile *hwemu.Profile
		if n.Profile != "" {
			if keys := inventoryHardwareKeys(n); len(keys) > 0 {
				return nil, fmt.Errorf("inventory entry %s: profile %s sets the hardware, so %s must stay unset", prefix, n.Profile, strings.Join(keys, ", "))
			}
			if profiles == nil {
				// Loaded once, and only for an inventory that names a profile.
				if profiles, err = loadProfileSetFromEnv(); err != nil {
					return nil, fmt.Errorf("inventory entry %s: %w", prefix, err)
				}
			}
			if profile = profiles[n.Profile]; profile == nil {
				known := make([]string, 0, len(profiles))
				for name := range profiles {
					known = append(known, name)
				}
				sort.Strings(known)
				return nil, fmt.Errorf("inventory entry %s: unknown profile %q (known: %s)", prefix, n.Profile, strings.Join(known, ", "))
			}
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
			if profile != nil {
				result = append(result, profileInventoryNode(profile, name))
				continue
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

// inventoryHardwareKeys lists the hardware keys an inventory entry sets.
func inventoryHardwareKeys(n inventoryNode) []string {
	var keys []string
	for _, k := range []struct {
		name string
		set  bool
	}{
		{"vendor", n.Vendor != ""},
		{"product", n.Product != ""},
		{"cpu", n.CPU != ""},
		{"cpu_sockets", n.CPUSockets != 0},
		{"cpu_cores", n.CPUCores != 0},
		{"memory_gib", n.MemoryGiB != 0},
		{"gpu_count", n.GPUCount != 0},
		{"gpu_min_cap_watts", n.GPUMinCapWatts != 0},
		{"gpu_max_cap_watts", n.GPUMaxCapWatts != 0},
	} {
		if k.set {
			keys = append(keys, k.name)
		}
	}
	return keys
}

// profileInventoryNode is the inventory node called name, of profile p. Its
// labels are those of the Node a real node of the profile registers as
// (hwemu.Profile.NodeObject), plus the ones of a legacy entry that the
// simulator reads: type, joulie.io/managed, joulie.io/node-name and the CPU
// and GPU counts (refreshNodeStateFromKubeData).
func profileInventoryNode(p *hwemu.Profile, name string) expandedNode {
	cpus := int(p.Node.Capacity.CPU.V)
	gpus, vendor, product := 0, "none", ""
	if g := p.GPUs; g != nil && g.Count.V > 0 {
		gpus, vendor, product = g.Count.V, g.Vendor, g.Product.V
	}
	lbls := map[string]string{}
	for k, v := range p.NodeObject(name).Labels {
		lbls[k] = v
	}
	lbls["type"] = "kwok"
	lbls["joulie.io/managed"] = "true"
	lbls["joulie.io/node-name"] = name
	lbls["joulie.io/hw.cpu-cores"] = fmt.Sprintf("%d", cpus)
	lbls["joulie.io/hw.gpu-count"] = fmt.Sprintf("%d", gpus)
	return expandedNode{
		Name:        name,
		Vendor:      vendor,
		Product:     product,
		CPUModel:    p.CPU.ModelName.V,
		CPUSockets:  max(1, p.CPU.Sockets.V),
		CPUCores:    cpus,
		MemoryGiB:   int(p.Node.Capacity.MemoryKi.V / (1024 * 1024)),
		GPUCount:    gpus,
		Labels:      lbls,
		Profile:     p,
		EcoPowerCap: profileHasWritablePL1(p),
	}
}

// initStandaloneNodes builds the model and the state of every inventory
// node. A legacy entry resolves its model from its labels, as a Kubernetes
// node does (refreshNodeStateFromKubeData), and then takes the GPU board
// power range the entry sets (withInventoryGPUCaps). A profile entry takes
// simModelFromProfile and its profile's capacity, whatever SIM_NODE_SELECTOR
// says, as an emulated node does.
func initStandaloneNodes(s *simulator, nodes []expandedNode) error {
	legacy := map[string]map[string]string{}
	for _, n := range nodes {
		if n.Profile == nil {
			legacy[n.Name] = n.Labels
		}
	}
	s.refreshNodeStateFromKubeData(map[string]int{}, nil, legacy)

	for i, n := range nodes {
		if n.Profile == nil {
			if err := withInventoryGPUCaps(s, n); err != nil {
				return fmt.Errorf("inventory node %s: %w", n.Name, err)
			}
			if n.GPUCount == 0 {
				if r, err := s.catalog.NodeCPUCapRangeW(hwinv.SimulatedNodeCPU(n.Labels)); err == nil {
					nodes[i].AgentCPUCapRange = &r
				}
			}
			continue
		}
		model, err := simModelFromProfile(n.Profile, s.model)
		if err != nil {
			return fmt.Errorf("inventory node %s: %w", n.Name, err)
		}
		class := profileProvenance(n.Profile)
		s.mu.Lock()
		s.nodeSeen[n.Name] = true
		s.nodeModels[n.Name] = model
		if prev := s.nodeClass[n.Name]; prev != "" && prev != class {
			s.nodeClassInfo.WithLabelValues(n.Name, prev).Set(0)
		}
		s.nodeClass[n.Name] = class
		s.nodeClassInfo.WithLabelValues(n.Name, class).Set(1)
		delete(s.state, n.Name)
		s.mu.Unlock()
		st := s.getNode(n.Name)
		s.mu.Lock()
		st.CPUCapacityCores = float64(n.CPUCores)
		st.GPUCapacityDevices = float64(n.GPUCount)
		s.mu.Unlock()
	}
	return nil
}

// withInventoryGPUCaps sets the GPU board power range of a legacy inventory
// entry, gpu_min_cap_watts and gpu_max_cap_watts, on its node's model: the
// entry describes that node, so it overrides the environment, the node
// classes and the catalogue. A value of 0 keeps the model's. The node's GPUs
// restart at the new maximum, as a new node's do.
func withInventoryGPUCaps(s *simulator, n expandedNode) error {
	if n.GPUMinCapWatts <= 0 && n.GPUMaxCapWatts <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	model, selected := s.nodeModels[n.Name]
	st := s.state[n.Name]
	if !selected || st == nil {
		return nil
	}
	if n.GPUMaxCapWatts > 0 {
		model.GPU.MaxWattsPerGPU = n.GPUMaxCapWatts
		model.Defaulted.GPUMaxWattsPerGPU = false
	}
	if n.GPUMinCapWatts > 0 {
		model.GPU.MinCapWattsPerGPU = n.GPUMinCapWatts
		model.Defaulted.GPUMinCapWattsPerGPU = false
	} else {
		model.GPU.MinCapWattsPerGPU = capMinAtMax(model.GPU.MinCapWattsPerGPU, model.GPU.MaxWattsPerGPU)
	}
	if err := hw.ValidateProfile(model); err != nil {
		return fmt.Errorf("gpu_min_cap_watts %g, gpu_max_cap_watts %g: %w", n.GPUMinCapWatts, n.GPUMaxCapWatts, err)
	}
	s.nodeModels[n.Name] = model
	maxW := model.GPU.MaxWattsPerGPU
	st.GPUCapWattsPerGpu, st.GPUTargetCapWattsPerGpu = maxW, maxW
	for i := range st.GPUDevices {
		st.GPUDevices[i].CapWatts, st.GPUDevices[i].TargetCapWatts = maxW, maxW
	}
	stepNode(st, model, 0, physMaxSubstep)
	return nil
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

		// Cooling stress not simulated: use 0 (no stress).
		coolingStress := 0.0

		// Trend not simulated: use 0. In production, uses adaptive scaling:
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
		gpuCount := 0.0
		if st != nil {
			freqScale = st.FreqScale
			if model.GPU.Count > 0 && model.GPU.MaxWattsPerGPU > 0 {
				gpuCount = float64(model.GPU.Count)
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
		st.HasJobs = true
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
			cpuSpeed, gpuSpeed := legacyJobRates(j, model, s.workload.baseSpeedCore, jobCPUMul, cpuShareFactor, gpuShareFactor, snapGPUCapPerGpu, snapThermalThrottle)

			if j.CPUUnitsRemaining > 0 {
				prev := j.CPUUnitsRemaining
				j.CPUUnitsRemaining -= cpuSpeed * dt
				if j.CPUUnitsRemaining < prev {
					j.LastProgressAt = now
				}
			}

			if j.GPUUnitsRemaining > 0 {
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
			s.mu.Lock()
			setNoJobLoad(st)
			s.mu.Unlock()
		}
	}
	return completedThisTick
}

// standaloneTickParallel runs job progress, then steps every node's physics
// by dt (stepNode, in substeps of at most substep) and books its energy, in
// parallel across numWorkers goroutines. Each worker handles a disjoint set
// of nodes, so no locking is needed for per-node state.
func standaloneTickParallel(s *simulator, tracker map[string]*standaloneNodeTracker, allNodeNames []string, nodeByName map[string]*expandedNode, dt float64, substep time.Duration, now time.Time, activeNodes map[string]bool, numWorkers int) int {
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
		s.mu.Lock()
		for _, name := range allNodeNames {
			if st := s.state[name]; st != nil {
				cpuJ, gpuJ := stepNode(st, s.modelLocked(name), dt, substep)
				s.addEnergyLocked(name, cpuJ, gpuJ)
			}
		}
		s.mu.Unlock()
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
		energy      []standaloneNodeEnergy
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
			res := workerResult{energy: make([]standaloneNodeEnergy, 0, len(nodes))}

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
				gpuCount := 0.0
				if model.GPU.Count > 0 && model.GPU.MaxWattsPerGPU > 0 {
					gpuCount = float64(model.GPU.Count)
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

				// Update node utilization (no lock needed: disjoint partition).
				cpuCapacity := st.CPUCapacityCores
				if cpuCapacity <= 0 {
					cpuCapacity = 16
				}
				st.HasJobs = true
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
					cpuSpeed, gpuSpeed := legacyJobRates(j, model, baseSpeedCore, jobCPUMul, cpuShareFactor, gpuShareFactor, snapGPUCapPerGpu, snapThermalThrottle)

					if j.CPUUnitsRemaining > 0 {
						prev := j.CPUUnitsRemaining
						j.CPUUnitsRemaining -= cpuSpeed * dt
						if j.CPUUnitsRemaining < prev {
							j.LastProgressAt = now
						}
					}

					if j.GPUUnitsRemaining > 0 {
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

				// Step this node's physics (local accumulation, merged after).
				cpuJ, gpuJ := stepNode(st, model, dt, substep)
				res.energy = append(res.energy, standaloneNodeEnergy{node: nodeName, cpuJ: cpuJ, gpuJ: gpuJ})
			}

			atomic.AddInt64(&completedTotal, res.completed)
			results[workerID] = res
		}(w, activeList[start:end])
	}
	wg.Wait()

	// --- Parallel physics for IDLE nodes (not in activeList) ---
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
		idleEnergy := make([][]standaloneNodeEnergy, numWorkers)
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
				local := make([]standaloneNodeEnergy, 0, len(nodes))
				for _, name := range nodes {
					st := s.state[name]
					if st == nil {
						continue
					}
					cpuJ, gpuJ := stepNode(st, s.modelLocked(name), dt, substep)
					local = append(local, standaloneNodeEnergy{node: name, cpuJ: cpuJ, gpuJ: gpuJ})
				}
				idleEnergy[wid] = local
			}(w, idleList[start:end])
		}
		wg2.Wait()
		// Merge idle energy.
		s.mu.Lock()
		for _, local := range idleEnergy {
			for _, e := range local {
				s.addEnergyLocked(e.node, e.cpuJ, e.gpuJ)
			}
		}
		s.mu.Unlock()
	}

	// Flush completion metrics, energy, and remove empty nodes (single-threaded).
	for _, res := range results {
		for _, c := range res.completions {
			s.jobCompleted.WithLabelValues(c.class, c.node).Inc()
			if c.duration > 0 {
				s.jobCompletion.Observe(c.duration)
			}
		}
		s.mu.Lock()
		for _, name := range res.emptyNodes {
			delete(activeNodes, name)
			if st := s.state[name]; st != nil {
				setNoJobLoad(st)
			}
		}
		s.mu.Unlock()
		// Merge energy from active nodes.
		s.mu.Lock()
		for _, e := range res.energy {
			s.addEnergyLocked(e.node, e.cpuJ, e.gpuJ)
		}
		s.mu.Unlock()
	}

	return int(completedTotal)
}

// standaloneNodeEnergy is one node's energy over a tick, as a worker of
// standaloneTickParallel collects it before the merge.
type standaloneNodeEnergy struct {
	node       string
	cpuJ, gpuJ float64
}

// performanceNodes is the simulator's whole decision of which nodes stay in
// performance. It describes the simulated nodes the way the controller manager
// hands them to pkg/controller/policy and calls the same PerformanceSet:
//   - a model the catalogue knows is keyed by its catalogue key, otherwise by
//     its raw string, as listNodeHardware does in the controller manager. The
//     key matters: PerformanceSet gives a tied slot to the smaller key;
//   - a node is currently in performance when it is neither eco nor draining.
//     The controller labels a draining node eco, so it is not kept first there
//     either.
func performanceNodes(catalog *hw.Catalog, nodeNames []string, nodeByName map[string]*expandedNode, tracker map[string]*standaloneNodeTracker, hpCount int) map[string]bool {
	hwInfo := make(map[string]policy.NodeHardwareInfo, len(nodeNames))
	current := make(map[string]string, len(nodeNames))
	for _, name := range nodeNames {
		if n := nodeByName[name]; n != nil {
			info := policy.NodeHardwareInfo{CPURawModel: n.CPUModel, GPURawModel: n.Product, GPUCount: n.GPUCount}
			if catalog != nil {
				match := catalog.MatchNode(hwinv.NodeDescriptor{CPUModelRaw: n.CPUModel, GPUModelRaw: n.Product, GPUCount: n.GPUCount})
				if match.CPUSpec != nil {
					info.CPUModel = match.CPUKey
				}
				if match.GPUSpec != nil {
					info.GPUModel = match.GPUKey
				}
			}
			hwInfo[name] = info
		}
		if t := tracker[name]; t != nil && !t.isEco && !t.isDraining {
			current[name] = "performance"
		}
	}
	return policy.PerformanceSet(nodeNames, hwInfo, current, hpCount)
}

// countPerformanceSensitivePending is the queue-aware demand: performance jobs
// running on a node plus performance jobs waiting for one, the counterpart of
// fsm.CountPerformanceDemand over Running and Pending pods. A job is waiting
// when its submit time has passed (readySec, seconds since the workload start)
// and no node has taken it: standalone marks a job Submitted only when it
// places it, so a waiting job is one that is ready but not Submitted.
func countPerformanceSensitivePending(tracker map[string]*standaloneNodeTracker, jobs []*simJob, readySec float64) int {
	count := 0
	for _, t := range tracker {
		count += t.perfPodCount
	}
	for _, j := range jobs {
		if j.SubmitOffsetSec > readySec {
			break // jobs are sorted by submit time; the rest are in the future
		}
		if !j.Submitted && !j.Completed && j.Class == "performance" {
			count++
		}
	}
	return count
}

// applyPowerPolicy sets eco/performance labels and corresponding power caps on nodes.
// Matches the real controller manager logic:
//   - Per-family split: policy.PerformanceSet, at least 1 perf node per hardware family
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
				st.TargetCapWatts = model.DefaultCapW
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

	// Split the slots across hardware families exactly as the controller
	// manager does, at least one per family.
	perfNodes := performanceNodes(s.catalog, nodeNames, nodeByName, tracker, hpCount)

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
			st.TargetCapWatts = perfCPUCapW(nodeByName[name], model)
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
				st.TargetCapWatts = perfCPUCapW(nodeByName[name], model)
				st.GPUCapWattsPerGpu = model.GPU.MaxWattsPerGPU
				st.GPUTargetCapWattsPerGpu = model.GPU.MaxWattsPerGPU
				t.isEco = false
				t.isDraining = true
			} else {
				// ActiveEco: apply eco caps. A GPU node gets GPU caps
				// only, as the controller manager plans it: its CPU runs
				// as on a performance node, since capping it saves little
				// and slows the GPU data feed by more (exp-03). A CPU-only
				// profile node with a writable PL1 gets a CPU power cap,
				// as the agent writes one. A legacy CPU-only node whose
				// CPU the catalog knows gets the watt cap its pool agent
				// resolves from the catalog range and sends over HTTP.
				// Any other CPU-only node, HSMP included since the agent
				// has no HSMP path, gets a frequency scale.
				n := nodeByName[name]
				switch {
				case n != nil && n.GPUCount > 0:
					st.FreqScale = 1.0
					st.TargetThrottlePct = 0
					st.TargetCapWatts = model.DefaultCapW
				case n != nil && n.EcoPowerCap:
					st.FreqScale = 1.0
					st.TargetThrottlePct = 0
					st.TargetCapWatts = cpuEcoPct * model.RaplCapMaxW
				case n != nil && n.AgentCPUCapRange != nil:
					st.FreqScale = 1.0
					st.TargetThrottlePct = 0
					st.TargetCapWatts = n.AgentCPUCapRange.CapForPctW(cpuEcoPct * 100)
				default:
					throttlePct := int(math.Round((1.0 - cpuEcoPct) * 100))
					st.FreqScale = cpuEcoPct
					st.TargetThrottlePct = throttlePct
				}
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

// perfCPUCapW is the CPU cap of a performance or draining node under
// baseline B or C, the cap the agent applies for a 100 % CPU cap: on a legacy
// node with AgentCPUCapRange its pool agent resolves 100 % to the top of the
// catalog range and sends it as watts, which binds where the node's power
// curve peaks above its TDP; any other node keeps its default cap.
func perfCPUCapW(n *expandedNode, model simModel) float64 {
	if n != nil && n.AgentCPUCapRange != nil {
		return n.AgentCPUCapRange.MaxW()
	}
	return model.DefaultCapW
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
