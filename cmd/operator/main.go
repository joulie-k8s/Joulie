package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matbun/joulie/pkg/hwinv"
	"github.com/matbun/joulie/pkg/operator/fsm"
	"github.com/matbun/joulie/pkg/operator/policy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var nodeHardwareGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodehardwares"}

const (
	powerProfileLabelKey  = "joulie.io/power-profile"

	profilePerformance    = "performance"
	profileEco            = "eco"
	workloadClassUnknown  = "unknown"
	workloadClassGeneral  = "general"
	workloadClassEcoOnly  = "eco-only"
	workloadClassPerfOnly = "performance-only"
)

// NodeAssignment and GPUCapIntent are defined in pkg/operator/policy.
type NodeAssignment = policy.NodeAssignment
type GPUCapIntent = policy.GPUCapIntent

// kubeNodeOps adapts kubernetes.Interface to the fsm.NodeOps interface.
type kubeNodeOps struct {
	kube kubernetes.Interface
}

func (k *kubeNodeOps) RunningPerformanceSensitivePodCount(ctx context.Context, nodeName string) (int, error) {
	pods, err := k.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return 0, err
	}
	return fsm.CountPerformanceSensitivePods(pods.Items), nil
}

type GPUModelCaps struct {
	MinCapWatts float64 `json:"minCapWatts"`
	MaxCapWatts float64 `json:"maxCapWatts"`
}

type NodeHardware struct {
	Name                string
	NodeName            string
	CPUModel            string
	CPURawModel         string
	CPUSockets          int
	CPUTotalCores       int
	GPUModel            string
	GPURawModel         string
	GPUCount            int
	CPUCapMinWatts      float64
	CPUCapMaxWatts      float64
	CPUCapKnown         bool
	GPUCapMinWatts      float64
	GPUCapMaxWatts      float64
	GPUCapKnown         bool
	CPUControlAvailable bool
	GPUControlAvailable bool
	CPUComputeDensity   float64
	GPUComputeDensity   float64
	Warnings            []string
}

var (
	gpuIntentWarningMu   sync.Mutex
	gpuIntentWarningSeen = map[string]struct{}{}

	operatorNodeState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "joulie_operator_node_state",
			Help: "Current operator view of node state (1 for active state, 0 otherwise).",
		},
		[]string{"node", "state"},
	)
	operatorNodeProfileLabel = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "joulie_operator_node_profile_label",
			Help: "Current node power-profile label as seen/applied by operator (1 for active profile, 0 otherwise).",
		},
		[]string{"node", "profile"},
	)
	operatorStateTransitions = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "joulie_operator_state_transitions_total",
			Help: "Total number of state-transition events handled by operator.",
		},
		[]string{"node", "from_state", "to_state", "result"},
	)

	operatorNodeDensity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "joulie_operator_node_compute_density",
			Help: "Normalized compute density score used by the operator for heterogeneous planning.",
		},
		[]string{"node", "component"},
	)
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[joulie-operator] ")

	reconcileEvery := durationEnv("RECONCILE_INTERVAL", time.Minute)
	metricsAddr := envOrDefault("METRICS_ADDR", ":8081")
	selector := envOrDefault("NODE_SELECTOR", "node-role.kubernetes.io/worker")
	reservedLabel := envOrDefault("RESERVED_LABEL_KEY", "joulie.io/reserved")
	profileLabel := envOrDefault("POWER_PROFILE_LABEL", "joulie.io/power-profile")

	perfCap := floatEnv("PERFORMANCE_CAP_WATTS", 5000)
	ecoCap := floatEnv("ECO_CAP_WATTS", 120)
	cpuPerfCapPct := floatEnv("CPU_PERFORMANCE_CAP_PCT_OF_MAX", 100)
	cpuEcoCapPct := floatEnv("CPU_ECO_CAP_PCT_OF_MAX", 60)
	cpuWriteAbsolute := boolEnv("CPU_WRITE_ABSOLUTE_CAPS", false)
	policyType := strings.ToLower(envOrDefault("POLICY_TYPE", "static_partition"))
	staticHPFrac := floatEnv("STATIC_HP_FRAC", 0.50)
	queueHPBaseFrac := floatEnv("QUEUE_HP_BASE_FRAC", 0.60)
	queueHPMin := intEnv("QUEUE_HP_MIN", 1)
	queueHPMax := intEnv("QUEUE_HP_MAX", 1000000)
	queuePerfPerHPNode := intEnv("QUEUE_PERF_PER_HP_NODE", 10)
	gpuPerfCapPct := floatEnv("GPU_PERFORMANCE_CAP_PCT_OF_MAX", 100)
	gpuEcoCapPct := floatEnv("GPU_ECO_CAP_PCT_OF_MAX", 60)
	gpuWriteAbsolute := boolEnv("GPU_WRITE_ABSOLUTE_CAPS", false)
	gpuModelCaps := parseGPUModelCaps(envOrDefault("GPU_MODEL_CAPS_JSON", "{}"))
	gpuProductLabelKeys := parseCSVList(envOrDefault(
		"GPU_PRODUCT_LABEL_KEYS",
		"joulie.io/gpu.product,nvidia.com/gpu.product,amd.com/gpu.product,amd.com/gpu.family",
	))
	hardwareCatalog := loadHardwareCatalog()

	parsedSelector, err := labels.Parse(selector)
	if err != nil {
		log.Fatalf("invalid NODE_SELECTOR %q: %v", selector, err)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("in-cluster config: %v", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("dynamic client: %v", err)
	}
	startMetricsServer(metricsAddr)

	// --- Facility metrics (data-center level) ---
	fm := &facilityMetrics{}
	facCfg := facilityConfig{
		enabled:            boolEnv("ENABLE_FACILITY_METRICS", false),
		prometheusAddress:  envOrDefault("FACILITY_PROMETHEUS_ADDRESS", "http://prometheus-operated.monitoring:9090"),
		pollInterval:       durationEnv("FACILITY_POLL_INTERVAL", 30*time.Second),
		ambientTempMetric:  envOrDefault("FACILITY_AMBIENT_TEMP_METRIC", "datacenter_ambient_temperature_celsius"),
		itPowerMetric:      envOrDefault("FACILITY_IT_POWER_METRIC", "datacenter_total_it_power_watts"),
		coolingPowerMetric: envOrDefault("FACILITY_COOLING_POWER_METRIC", "datacenter_cooling_power_watts"),
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	go facilityMetricsLoop(bgCtx, fm, facCfg)

	// --- Workload classifier ---
	if boolEnv("ENABLE_CLASSIFIER", true) {
		clsCfg := classifierConfig{
			classifyInterval:   durationEnv("CLASSIFY_INTERVAL", 30*time.Second),
			reclassifyInterval: durationEnv("RECLASSIFY_INTERVAL", 15*time.Minute),
			metricsWindow:      durationEnv("CLASSIFY_METRICS_WINDOW", 10*time.Minute),
			prometheusAddress:  envOrDefault("PROMETHEUS_ADDRESS", "http://prometheus-operated.monitoring:9090"),
			keplerAvailable:    boolEnv("KEPLER_AVAILABLE", true),
			minConfidence:      floatEnv("CLASSIFY_MIN_CONFIDENCE", 0.5),
			nodeSelector:       selector,
		}
		go classifierLoop(bgCtx, kube, dyn, clsCfg)
	}

	// --- Active rescheduler ---
	reschCfg := reschedulerConfig{
		enabled:             boolEnv("ENABLE_ACTIVE_RESCHEDULING", false),
		interval:            durationEnv("RESCHEDULE_INTERVAL", 60*time.Second),
		maxEvictionsPerNode: intEnv("RESCHEDULE_MAX_EVICTIONS_PER_NODE", 1),
		dryRun:              boolEnv("RESCHEDULE_DRY_RUN", false),
	}
	go reschedulerLoop(bgCtx, kube, dyn, reschCfg)

	// --- Periodic WorkloadProfile cleanup ---
	go func() {
		cleanupTicker := time.NewTicker(5 * time.Minute)
		defer cleanupTicker.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-cleanupTicker.C:
				ctx, cancel := context.WithTimeout(bgCtx, 10*time.Second)
				if err := cleanupOrphanedWorkloadProfiles(ctx, kube, dyn); err != nil {
					log.Printf("[cleanup] WorkloadProfile cleanup: %v", err)
				}
				cancel()
			}
		}
	}()

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := reconcileWithCatalogAndFacility(
			ctx, kube, dyn, parsedSelector, reservedLabel, profileLabel, reconcileEvery,
			perfCap, ecoCap, policyType, staticHPFrac, queueHPBaseFrac, queueHPMin, queueHPMax, queuePerfPerHPNode,
			cpuPerfCapPct, cpuEcoCapPct, cpuWriteAbsolute,
			gpuPerfCapPct, gpuEcoCapPct, gpuWriteAbsolute, gpuModelCaps, gpuProductLabelKeys, hardwareCatalog,
			fm,
		); err != nil {
			log.Printf("reconcile failed: %v", err)
		}
		cancel()
		time.Sleep(reconcileEvery)
	}
}

func startMetricsServer(addr string) {
	prometheus.MustRegister(operatorNodeState, operatorNodeProfileLabel, operatorStateTransitions, operatorNodeDensity)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Printf("metrics server listening on %s", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("metrics server stopped: %v", err)
		}
	}()
}

func reconcile(
	ctx context.Context,
	kube kubernetes.Interface,
	dyn dynamic.Interface,
	selector labels.Selector,
	reservedLabel string,
	profileLabel string,
	interval time.Duration,
	perfCap float64,
	ecoCap float64,
	policyType string,
	staticHPFrac float64,
	queueHPBaseFrac float64,
	queueHPMin int,
	queueHPMax int,
	queuePerfPerHPNode int,
	cpuPerfCapPct float64,
	cpuEcoCapPct float64,
	cpuWriteAbsolute bool,
	gpuPerfCapPct float64,
	gpuEcoCapPct float64,
	gpuWriteAbsolute bool,
	gpuModelCaps map[string]GPUModelCaps,
	gpuProductLabelKeys []string,
) error {
	return reconcileWithCatalog(
		ctx, kube, dyn, selector, reservedLabel, profileLabel, interval,
		perfCap, ecoCap, policyType, staticHPFrac, queueHPBaseFrac, queueHPMin, queueHPMax, queuePerfPerHPNode,
		cpuPerfCapPct, cpuEcoCapPct, cpuWriteAbsolute,
		gpuPerfCapPct, gpuEcoCapPct, gpuWriteAbsolute, gpuModelCaps, gpuProductLabelKeys, loadHardwareCatalog(),
	)
}

func reconcileWithCatalog(
	ctx context.Context,
	kube kubernetes.Interface,
	dyn dynamic.Interface,
	selector labels.Selector,
	reservedLabel string,
	profileLabel string,
	interval time.Duration,
	perfCap float64,
	ecoCap float64,
	policyType string,
	staticHPFrac float64,
	queueHPBaseFrac float64,
	queueHPMin int,
	queueHPMax int,
	queuePerfPerHPNode int,
	cpuPerfCapPct float64,
	cpuEcoCapPct float64,
	cpuWriteAbsolute bool,
	gpuPerfCapPct float64,
	gpuEcoCapPct float64,
	gpuWriteAbsolute bool,
	gpuModelCaps map[string]GPUModelCaps,
	gpuProductLabelKeys []string,
	hardwareCatalog *hwinv.Catalog,
) error {
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}

	eligible := make([]string, 0)
	nodesByName := make(map[string]string, len(nodes.Items))
	nodeObjs := make(map[string]*corev1.Node, len(nodes.Items))
	for _, n := range nodes.Items {
		node := n
		nodeObjs[n.Name] = &node
		nodesByName[n.Name] = currentProfileOrDefault(n.Labels[profileLabel])
		if n.Spec.Unschedulable {
			continue
		}
		if n.Labels[reservedLabel] == "true" {
			continue
		}
		if !selector.Matches(labels.Set(n.Labels)) {
			continue
		}
		eligible = append(eligible, n.Name)
	}
	sort.Strings(eligible)
	if len(eligible) == 0 {
		log.Printf("no eligible nodes matched selector=%q", selector.String())
		return nil
	}

	nodeHardwareByName, err := listNodeHardware(ctx, dyn, hardwareCatalog)
	if err != nil {
		return fmt.Errorf("list node hardware: %w", err)
	}
	for _, nodeName := range eligible {
		if _, ok := nodeHardwareByName[nodeName]; ok {
			continue
		}
		if n := nodeObjs[nodeName]; n != nil {
			nodeHardwareByName[nodeName] = nodeHardwareFromLabels(*n, hardwareCatalog, gpuProductLabelKeys)
		}
	}
	sortNodesByDensity(eligible, nodeHardwareByName)
	for _, nodeName := range eligible {
		nh, ok := nodeHardwareByName[nodeName]
		if !ok {
			operatorNodeDensity.WithLabelValues(nodeName, "cpu").Set(0)
			operatorNodeDensity.WithLabelValues(nodeName, "gpu").Set(0)
			continue
		}
		operatorNodeDensity.WithLabelValues(nodeName, "cpu").Set(nh.CPUComputeDensity)
		operatorNodeDensity.WithLabelValues(nodeName, "gpu").Set(nh.GPUComputeDensity)
	}

	plan := buildPlanByPolicy(ctx, kube, policyType, eligible, nodeHardwareByName, interval, perfCap, ecoCap, staticHPFrac, queueHPBaseFrac, queueHPMin, queueHPMax, queuePerfPerHPNode)
	for i := range plan {
		plan[i].SourceProfile = currentProfileOrDefault(nodesByName[plan[i].NodeName])
		plan[i].Draining = false
		plan[i].State = assignmentState(plan[i].Profile, plan[i].Draining)
		// Skip CPU cap on GPU nodes: CPU is ~6% of GPU node power; capping it
		// saves ~1.2% but slows GPU data feed by ~4.5%, costing 3.8x more energy
		// than saved (exp-03 finding). GPU nodes get only GPU caps.
		hasGPU := false
		if n := nodeObjs[plan[i].NodeName]; n != nil {
			hasGPU = discoverGPUCount(*n) > 0
		}
		if !hasGPU {
			if cpuWriteAbsolute {
				plan[i].CPUCapPctOfMax = nil
				if nh, ok := nodeHardwareByName[plan[i].NodeName]; ok {
					if abs, ok := computeAbsoluteCPUCap(plan[i].Profile, nh, hardwareCatalog, perfCap, ecoCap); ok {
						plan[i].CapWatts = abs
					}
				}
			} else {
				pct := cpuEcoCapPct
				if plan[i].Profile == profilePerformance {
					pct = cpuPerfCapPct
				}
				plan[i].CPUCapPctOfMax = floatPtr(pct)
			}
		}
		if n := nodeObjs[plan[i].NodeName]; n != nil {
			plan[i].GPU = computeGPUIntentForNodeWithHardware(*n, plan[i].Profile, gpuPerfCapPct, gpuEcoCapPct, gpuWriteAbsolute, gpuModelCaps, gpuProductLabelKeys, nodeHardwareByName[plan[i].NodeName], hardwareCatalog)
			if discoverGPUCount(*n) > 0 && plan[i].GPU == nil {
				reason := "no GPU cap configured"
				if plan[i].Profile == profilePerformance && gpuPerfCapPct <= 0 {
					reason = "GPU_PERFORMANCE_CAP_PCT_OF_MAX <= 0"
				} else if plan[i].Profile != profilePerformance && gpuEcoCapPct <= 0 {
					reason = "GPU_ECO_CAP_PCT_OF_MAX <= 0"
				}
				warnNoGPUIntentOnce(plan[i].NodeName, plan[i].Profile, reason)
			}
		}
	}
	applyDowngradeGuards(ctx, kube, plan, nodesByName)

	// Build per-rack estimated power for topology-aware PSU stress.
	// Sum each node's estimated power (CPU + GPU at current cap %) per rack.
	rackPowerEstimates := make(map[string]float64)
	nodeRack := make(map[string]string)
	nodeZone := make(map[string]string)
	for _, a := range plan {
		if n := nodeObjs[a.NodeName]; n != nil {
			rack := n.Labels["joulie.io/rack"]
			zone := n.Labels["joulie.io/cooling-zone"]
			nodeRack[a.NodeName] = rack
			nodeZone[a.NodeName] = zone
			if rack != "" {
				cpuPct := 100.0
				if a.CPUCapPctOfMax != nil {
					cpuPct = *a.CPUCapPctOfMax
				}
				gpuPct := 100.0
				if a.GPU != nil && a.GPU.CapPctOfMax != nil {
					gpuPct = *a.GPU.CapPctOfMax
				}
				nodePowerW := estimateNodePowerW(nodeHardwareByName[a.NodeName], cpuPct, gpuPct)
				rackPowerEstimates[rack] += nodePowerW
			}
		}
	}

	for _, a := range plan {
		if err := upsertNodeTwinSpec(ctx, dyn, a); err != nil {
			return err
		}
		if err := upsertNodeLabels(ctx, kube, profileLabel, a); err != nil {
			return err
		}
		cpuPct := 100.0
		if a.CPUCapPctOfMax != nil {
			cpuPct = *a.CPUCapPctOfMax
		}
		gpuPct := 100.0
		if a.GPU != nil && a.GPU.CapPctOfMax != nil {
			gpuPct = *a.GPU.CapPctOfMax
		}
		var topo *nodeTopology
		rack := nodeRack[a.NodeName]
		zone := nodeZone[a.NodeName]
		if rack != "" || zone != "" {
			topo = &nodeTopology{
				rack:            rack,
				coolingZone:     zone,
				rackTotalPowerW: rackPowerEstimates[rack],
			}
		}
		if err := reconcileNodeTwin(ctx, dyn, a.NodeName, a.Profile, cpuPct, gpuPct, a.Draining, topo); err != nil {
			log.Printf("warning: reconcileNodeTwin %s: %v", a.NodeName, err)
		}
		recordNodeStateMetrics(a.NodeName, a.State)
		recordNodeProfileLabelMetrics(a.NodeName, a.Profile)
		recordTransitionMetrics(a)
	}

	log.Printf("assigned profiles policy=%s interval=%s nodes=%d plan=%s", policyType, interval, len(eligible), summarizePlan(plan))
	return nil
}

// reconcileWithCatalogAndFacility wraps reconcileWithCatalog and passes facility metrics
// to the twin computation via reconcileNodeTwin. This enables PUE estimation from
// real data-center metrics (ambient temperature, IT power, cooling power).
func reconcileWithCatalogAndFacility(
	ctx context.Context,
	kube kubernetes.Interface,
	dyn dynamic.Interface,
	selector labels.Selector,
	reservedLabel string,
	profileLabel string,
	interval time.Duration,
	perfCap float64,
	ecoCap float64,
	policyType string,
	staticHPFrac float64,
	queueHPBaseFrac float64,
	queueHPMin int,
	queueHPMax int,
	queuePerfPerHPNode int,
	cpuPerfCapPct float64,
	cpuEcoCapPct float64,
	cpuWriteAbsolute bool,
	gpuPerfCapPct float64,
	gpuEcoCapPct float64,
	gpuWriteAbsolute bool,
	gpuModelCaps map[string]GPUModelCaps,
	gpuProductLabelKeys []string,
	hardwareCatalog *hwinv.Catalog,
	fm *facilityMetrics,
) error {
	// Store facility metrics in package-level vars for use by reconcileNodeTwin.
	if fm != nil {
		ambientC, itPowerW, _ := fm.get()
		facilityAmbientTempC = ambientC
		facilityClusterPowerW = itPowerW
	}
	return reconcileWithCatalog(
		ctx, kube, dyn, selector, reservedLabel, profileLabel, interval,
		perfCap, ecoCap, policyType, staticHPFrac, queueHPBaseFrac, queueHPMin, queueHPMax, queuePerfPerHPNode,
		cpuPerfCapPct, cpuEcoCapPct, cpuWriteAbsolute,
		gpuPerfCapPct, gpuEcoCapPct, gpuWriteAbsolute, gpuModelCaps, gpuProductLabelKeys, hardwareCatalog,
	)
}

// facilityAmbientTempC and facilityClusterPowerW are set by reconcileWithCatalogAndFacility
// and read by reconcileNodeTwin to pass to the twin computation.
var (
	facilityAmbientTempC  float64
	facilityClusterPowerW float64
)

// Delegate to fsm package.
var (
	currentProfileOrDefault = fsm.CurrentProfileOrDefault
	assignmentState         = fsm.AssignmentState
)

func recordNodeStateMetrics(nodeName, state string) {
	activePerf := 0.0
	draining := 0.0
	activeEco := 0.0
	if state == "ActivePerformance" {
		activePerf = 1
	}
	if state == "DrainingPerformance" {
		draining = 1
	}
	if state == "ActiveEco" {
		activeEco = 1
	}
	operatorNodeState.WithLabelValues(nodeName, "ActivePerformance").Set(activePerf)
	operatorNodeState.WithLabelValues(nodeName, "DrainingPerformance").Set(draining)
	operatorNodeState.WithLabelValues(nodeName, "ActiveEco").Set(activeEco)
}

func recordNodeProfileLabelMetrics(nodeName, profile string) {
	perf := 0.0
	eco := 0.0
	if profile == profilePerformance {
		perf = 1
	}
	if profile == profileEco {
		eco = 1
	}
	operatorNodeProfileLabel.WithLabelValues(nodeName, profilePerformance).Set(perf)
	operatorNodeProfileLabel.WithLabelValues(nodeName, profileEco).Set(eco)
}

func recordTransitionMetrics(a NodeAssignment) {
	fromState := assignmentState(a.SourceProfile, a.SourceDrain)
	toState := a.State
	if toState == "" {
		toState = assignmentState(a.Profile, a.Draining)
	}
	if fromState == "Unknown" || toState == "Unknown" || fromState == toState {
		return
	}
	operatorStateTransitions.WithLabelValues(a.NodeName, fromState, toState, "applied").Inc()
}

func applyDowngradeGuards(
	ctx context.Context,
	kube kubernetes.Interface,
	plan []NodeAssignment,
	currentProfiles map[string]string,
) {
	ops := &kubeNodeOps{kube: kube}
	fsm.ApplyDowngradeGuards(ctx, ops, plan, currentProfiles)
	// Record metrics for guarded transitions.
	for _, a := range plan {
		if a.Draining {
			operatorStateTransitions.WithLabelValues(a.NodeName, "ActivePerformance", "ActiveEco", "deferred").Inc()
		}
	}
}

// computeDesiredLabels delegates to fsm.ComputeDesiredLabels.
var computeDesiredLabels = fsm.ComputeDesiredLabels

// toHardwareInfoMap converts the operator's NodeHardware map to the minimal
// policy.NodeHardwareInfo map needed by the policy algorithms.
func toHardwareInfoMap(hw map[string]NodeHardware) map[string]policy.NodeHardwareInfo {
	out := make(map[string]policy.NodeHardwareInfo, len(hw))
	for k, v := range hw {
		out[k] = policy.NodeHardwareInfo{
			CPUModel:    v.CPUModel,
			CPURawModel: v.CPURawModel,
			GPUModel:    v.GPUModel,
			GPURawModel: v.GPURawModel,
			GPUCount:    v.GPUCount,
		}
	}
	return out
}

func buildPlanByPolicy(
	ctx context.Context,
	kube kubernetes.Interface,
	policyType string,
	nodes []string,
	nodeHardwareByName map[string]NodeHardware,
	interval time.Duration,
	perfCap, ecoCap, staticHPFrac, queueHPBaseFrac float64,
	queueHPMin, queueHPMax, queuePerfPerHPNode int,
) []NodeAssignment {
	hw := toHardwareInfoMap(nodeHardwareByName)
	switch policyType {
	case "static_partition", "":
		return policy.BuildStaticPlan(nodes, hw, perfCap, ecoCap, staticHPFrac)
	case "queue_aware_v1":
		perfIntentPods, err := runningPerformanceSensitivePodCountAllNodes(ctx, kube)
		if err != nil {
			log.Printf("warning: cannot classify running pods for queue_aware_v1: %v; falling back to static fraction", err)
			return policy.BuildStaticPlan(nodes, hw, perfCap, ecoCap, queueHPBaseFrac)
		}
		return policy.BuildQueueAwarePlan(nodes, hw, perfCap, ecoCap, queueHPBaseFrac, queueHPMin, queueHPMax, queuePerfPerHPNode, perfIntentPods)
	case "rule_swap_v1":
		return policy.BuildRuleSwapPlan(nodes, interval, perfCap, ecoCap)
	default:
		log.Printf("warning: unknown POLICY_TYPE=%q, falling back to static_partition", policyType)
		return policy.BuildStaticPlan(nodes, hw, perfCap, ecoCap, staticHPFrac)
	}
}

func runningPerformanceSensitivePodCountAllNodes(
	ctx context.Context,
	kube kubernetes.Interface,
) (int, error) {
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	return fsm.CountPerformanceSensitivePods(pods.Items), nil
}

// Delegate pod classification to fsm package.
var (
	isPerformanceSensitivePod = fsm.IsPerformanceSensitivePod
	classifyPodByScheduling   = fsm.ClassifyPodByScheduling
	podExcludesEco            = fsm.PodExcludesEco
	podIsEcoOnly              = fsm.PodIsEcoOnly
)

func runningPerformanceSensitivePodCountOnNode(
	ctx context.Context,
	kube kubernetes.Interface,
	nodeName string,
) (int, error) {
	ops := &kubeNodeOps{kube: kube}
	return ops.RunningPerformanceSensitivePodCount(ctx, nodeName)
}

func summarizePlan(plan []NodeAssignment) string {
	parts := make([]string, 0, len(plan))
	for _, p := range plan {
		gpuPart := ""
		if p.GPU != nil {
			if p.GPU.CapWattsPerGPU != nil {
				gpuPart = fmt.Sprintf(",gpu=%.1fW", *p.GPU.CapWattsPerGPU)
			} else if p.GPU.CapPctOfMax != nil {
				gpuPart = fmt.Sprintf(",gpu=%.1f%%", *p.GPU.CapPctOfMax)
			}
		}
		cpuPart := fmt.Sprintf("%.1fW", p.CapWatts)
		if p.CPUCapPctOfMax != nil {
			cpuPart = fmt.Sprintf("%.1f%%", *p.CPUCapPctOfMax)
		}
		parts = append(parts, fmt.Sprintf("%s=%s(cpu=%s%s)", p.NodeName, p.Profile, cpuPart, gpuPart))
	}
	return strings.Join(parts, ",")
}

func sortNodesByDensity(nodes []string, hardwareByName map[string]NodeHardware) {
	sort.SliceStable(nodes, func(i, j int) bool {
		di := nodeDensityScore(hardwareByName[nodes[i]])
		dj := nodeDensityScore(hardwareByName[nodes[j]])
		if di == dj {
			return nodes[i] < nodes[j]
		}
		return di > dj
	})
}

func computeAbsoluteCPUCap(profile string, nh NodeHardware, catalog *hwinv.Catalog, perfCap, ecoCap float64) (float64, bool) {
	if nh.CPUCapKnown && nh.CPUCapMaxWatts > 0 {
		target := ecoCap
		if profile == profilePerformance {
			target = perfCap
		}
		if target < nh.CPUCapMinWatts {
			target = nh.CPUCapMinWatts
		}
		if target > nh.CPUCapMaxWatts {
			target = nh.CPUCapMaxWatts
		}
		return target, true
	}
	if catalog == nil {
		return 0, false
	}
	match := catalog.MatchNode(hwinv.NodeDescriptor{
		CPUModelRaw: nh.CPURawModel,
		CPUSockets:  nh.CPUSockets,
		CPUCores:    nh.CPUTotalCores,
	})
	if match.CPUSpec == nil {
		return 0, false
	}
	maxW := match.CPUSpec.Official.TDPW
	if nh.CPUSockets > 0 {
		maxW *= float64(nh.CPUSockets)
	}
	if maxW <= 0 {
		return 0, false
	}
	target := ecoCap
	if profile == profilePerformance {
		target = perfCap
	}
	if target > maxW {
		target = maxW
	}
	minW := maxW * 0.55
	if target < minW {
		target = minW
	}
	return target, true
}

func computeGPUIntentForNode(
	node corev1.Node,
	profile string,
	gpuPerfCapPct float64,
	gpuEcoCapPct float64,
	gpuWriteAbsolute bool,
	gpuModelCaps map[string]GPUModelCaps,
	gpuProductLabelKeys []string,
) *GPUCapIntent {
	return computeGPUIntentForNodeWithHardware(node, profile, gpuPerfCapPct, gpuEcoCapPct, gpuWriteAbsolute, gpuModelCaps, gpuProductLabelKeys, NodeHardware{}, loadHardwareCatalog())
}

func computeGPUIntentForNodeWithHardware(
	node corev1.Node,
	profile string,
	gpuPerfCapPct float64,
	gpuEcoCapPct float64,
	gpuWriteAbsolute bool,
	gpuModelCaps map[string]GPUModelCaps,
	gpuProductLabelKeys []string,
	nh NodeHardware,
	catalog *hwinv.Catalog,
) *GPUCapIntent {
	if discoverGPUCount(node) <= 0 {
		return nil
	}
	capPct := gpuEcoCapPct
	if profile == profilePerformance {
		capPct = gpuPerfCapPct
	}
	if capPct <= 0 {
		return nil
	}
	intent := &GPUCapIntent{
		Scope:       "perGpu",
		CapPctOfMax: floatPtr(capPct),
	}
	if !gpuWriteAbsolute {
		return intent
	}
	if abs, ok := computeInventoryGPUCap(profile, nh, catalog, gpuPerfCapPct, gpuEcoCapPct); ok {
		intent.CapWattsPerGPU = floatPtr(abs)
		return intent
	}
	product := resolveGPUProduct(node.Labels, gpuProductLabelKeys)
	if product == "" {
		return intent
	}
	if caps, ok := gpuModelCaps[product]; ok && caps.MaxCapWatts > 0 {
		minW := caps.MinCapWatts
		if minW <= 0 {
			minW = 1
		}
		abs := (capPct / 100.0) * caps.MaxCapWatts
		if abs < minW {
			abs = minW
		}
		if abs > caps.MaxCapWatts {
			abs = caps.MaxCapWatts
		}
		intent.CapWattsPerGPU = floatPtr(abs)
	}
	return intent
}

func computeInventoryGPUCap(profile string, nh NodeHardware, catalog *hwinv.Catalog, gpuPerfCapPct, gpuEcoCapPct float64) (float64, bool) {
	capPct := gpuEcoCapPct
	if profile == profilePerformance {
		capPct = gpuPerfCapPct
	}
	if capPct <= 0 {
		return 0, false
	}
	if nh.GPUCapKnown && nh.GPUCapMaxWatts > 0 {
		abs := (capPct / 100.0) * nh.GPUCapMaxWatts
		if abs < nh.GPUCapMinWatts && nh.GPUCapMinWatts > 0 {
			abs = nh.GPUCapMinWatts
		}
		if abs > nh.GPUCapMaxWatts {
			abs = nh.GPUCapMaxWatts
		}
		return abs, true
	}
	if catalog == nil {
		return 0, false
	}
	match := catalog.MatchNode(hwinv.NodeDescriptor{GPUModelRaw: nh.GPURawModel, GPUCount: nh.GPUCount})
	if match.GPUSpec == nil || match.GPUSpec.Official.MaxBoardPowerW <= 0 {
		return 0, false
	}
	maxW := match.GPUSpec.Official.MaxBoardPowerW
	minW := match.GPUSpec.Official.MinBoardPowerW
	if minW <= 0 {
		minW = maxW * 0.5
	}
	abs := (capPct / 100.0) * maxW
	if abs < minW {
		abs = minW
	}
	if abs > maxW {
		abs = maxW
	}
	return abs, true
}

func discoverGPUCount(node corev1.Node) int64 {
	var total int64
	for k, q := range node.Status.Allocatable {
		key := strings.ToLower(string(k))
		if key == "nvidia.com/gpu" || key == "amd.com/gpu" || key == "gpu.intel.com/i915" || strings.HasSuffix(key, "/gpu") {
			total += q.Value()
		}
	}
	return total
}

func resolveGPUProduct(nodeLabels map[string]string, keys []string) string {
	for _, k := range keys {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		if v := strings.TrimSpace(nodeLabels[key]); v != "" {
			return v
		}
	}
	return ""
}

func parseGPUModelCaps(raw string) map[string]GPUModelCaps {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]GPUModelCaps{}
	}
	out := map[string]GPUModelCaps{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		log.Printf("warning: invalid GPU_MODEL_CAPS_JSON: %v", err)
		return map[string]GPUModelCaps{}
	}
	return out
}

func parseCSVList(in string) []string {
	parts := strings.Split(in, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func loadHardwareCatalog() *hwinv.Catalog {
	path := strings.TrimSpace(os.Getenv("HARDWARE_CATALOG_PATH"))
	if path == "" {
		path = "simulator/catalog/hardware.yaml"
	}
	cat, err := hwinv.LoadCatalog(path)
	if err != nil {
		log.Printf("warning: failed to load hardware catalog path=%s err=%v", path, err)
		return nil
	}
	return cat
}

func listNodeHardware(ctx context.Context, dyn dynamic.Interface, catalog *hwinv.Catalog) (out map[string]NodeHardware, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("warning: nodehardware resource unavailable in dynamic client: %v", r)
			out = map[string]NodeHardware{}
			err = nil
		}
	}()
	ul, err := dyn.Resource(nodeHardwareGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]NodeHardware{}, nil
		}
		return nil, err
	}
	out = make(map[string]NodeHardware, len(ul.Items))
	for _, item := range ul.Items {
		nh := parseNodeHardware(item)
		if catalog != nil {
			match := catalog.MatchNode(hwinv.NodeDescriptor{
				CPUModelRaw: nh.CPURawModel,
				CPUSockets:  nh.CPUSockets,
				CPUCores:    nh.CPUTotalCores,
				GPUModelRaw: nh.GPURawModel,
				GPUCount:    nh.GPUCount,
			})
			if match.CPUSpec != nil {
				nh.CPUModel = match.CPUKey
				nh.CPUComputeDensity = computeCPUNodeDensity(*match.CPUSpec, nh.CPUSockets, nh.CPUTotalCores)
			}
			if match.GPUSpec != nil {
				nh.GPUModel = match.GPUKey
				nh.GPUComputeDensity = computeGPUNodeDensity(*match.GPUSpec, nh.GPUCount)
			}
			nh.Warnings = append(nh.Warnings, match.Warnings...)
		}
		out[nh.NodeName] = nh
	}
	return out, nil
}

func parseNodeHardware(u unstructured.Unstructured) NodeHardware {
	nh := NodeHardware{Name: u.GetName()}
	nh.NodeName, _, _ = unstructured.NestedString(u.Object, "spec", "nodeName")
	nh.CPUModel, _, _ = unstructured.NestedString(u.Object, "status", "cpu", "model")
	nh.CPURawModel, _, _ = unstructured.NestedString(u.Object, "status", "cpu", "rawModel")
	if v, ok, _ := unstructured.NestedInt64(u.Object, "status", "cpu", "sockets"); ok {
		nh.CPUSockets = int(v)
	}
	if v, ok, _ := unstructured.NestedInt64(u.Object, "status", "cpu", "totalCores"); ok {
		nh.CPUTotalCores = int(v)
	}
	if v, ok, _ := unstructured.NestedFloat64(u.Object, "status", "cpu", "capMinWatts"); ok {
		nh.CPUCapMinWatts = v
		nh.CPUCapKnown = true
	}
	if v, ok, _ := unstructured.NestedFloat64(u.Object, "status", "cpu", "capMaxWatts"); ok {
		nh.CPUCapMaxWatts = v
		nh.CPUCapKnown = true
	}
	if v, ok, _ := unstructured.NestedBool(u.Object, "status", "cpu", "controlAvailable"); ok {
		nh.CPUControlAvailable = v
	}
	nh.GPUModel, _, _ = unstructured.NestedString(u.Object, "status", "gpu", "model")
	nh.GPURawModel, _, _ = unstructured.NestedString(u.Object, "status", "gpu", "rawModel")
	if v, ok, _ := unstructured.NestedInt64(u.Object, "status", "gpu", "count"); ok {
		nh.GPUCount = int(v)
	}
	if v, ok, _ := unstructured.NestedFloat64(u.Object, "status", "gpu", "capMinWatts"); ok {
		nh.GPUCapMinWatts = v
		nh.GPUCapKnown = true
	}
	if v, ok, _ := unstructured.NestedFloat64(u.Object, "status", "gpu", "capMaxWatts"); ok {
		nh.GPUCapMaxWatts = v
		nh.GPUCapKnown = true
	}
	if v, ok, _ := unstructured.NestedBool(u.Object, "status", "gpu", "controlAvailable"); ok {
		nh.GPUControlAvailable = v
	}
	if warnings, ok, _ := unstructured.NestedStringSlice(u.Object, "status", "quality", "warnings"); ok {
		nh.Warnings = append(nh.Warnings, warnings...)
	}
	return nh
}

func nodeHardwareFromLabels(node corev1.Node, catalog *hwinv.Catalog, gpuProductLabelKeys []string) NodeHardware {
	nh := NodeHardware{
		Name:        sanitizeName(node.Name),
		NodeName:    node.Name,
		CPURawModel: firstNonEmpty(node.Labels["joulie.io/hw.cpu-model"], node.Labels["feature.node.kubernetes.io/cpu-model.name"]),
		CPUSockets:  hwinv.ParseIntString(firstNonEmpty(node.Labels["joulie.io/hw.cpu-sockets"], node.Labels["feature.node.kubernetes.io/cpu-sockets"])),
		CPUTotalCores: func() int {
			if qty, ok := node.Status.Capacity[corev1.ResourceCPU]; ok {
				return int(qty.Value())
			}
			return 0
		}(),
		GPURawModel: resolveGPUProduct(node.Labels, append([]string{"joulie.io/hw.gpu-model"}, gpuProductLabelKeys...)),
		GPUCount: func() int {
			if v := hwinv.ParseIntString(node.Labels["joulie.io/hw.gpu-count"]); v > 0 {
				return v
			}
			return int(discoverGPUCount(node))
		}(),
		CPUControlAvailable: false,
		GPUControlAvailable: false,
	}
	if catalog != nil {
		match := catalog.MatchNode(hwinv.NodeDescriptor{
			CPUModelRaw: nh.CPURawModel,
			CPUSockets:  nh.CPUSockets,
			CPUCores:    nh.CPUTotalCores,
			GPUModelRaw: nh.GPURawModel,
			GPUCount:    nh.GPUCount,
		})
		if match.CPUSpec != nil {
			nh.CPUModel = match.CPUKey
			nh.CPUComputeDensity = computeCPUNodeDensity(*match.CPUSpec, nh.CPUSockets, nh.CPUTotalCores)
		}
		if match.GPUSpec != nil {
			nh.GPUModel = match.GPUKey
			nh.GPUComputeDensity = computeGPUNodeDensity(*match.GPUSpec, nh.GPUCount)
		}
		nh.Warnings = append(nh.Warnings, match.Warnings...)
	}
	return nh
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func computeCPUNodeDensity(spec hwinv.CPUModelSpec, sockets, totalCores int) float64 {
	base := spec.ComputeDensity
	if base <= 0 {
		base = spec.Official.BoostGHz * spec.Official.TDPW
	}
	multiplier := float64(totalCores)
	if multiplier <= 0 && sockets > 0 {
		multiplier = float64(sockets)
	}
	if multiplier <= 0 {
		multiplier = 1
	}
	return base * multiplier
}

func computeGPUNodeDensity(spec hwinv.GPUModelSpec, count int) float64 {
	base := spec.ComputeDensity
	if base <= 0 {
		base = spec.Official.MaxBoardPowerW
	}
	if count <= 0 {
		count = 1
	}
	return base * float64(count)
}

func nodeDensityScore(nh NodeHardware) float64 {
	return nh.CPUComputeDensity + nh.GPUComputeDensity
}

// estimateNodePowerW estimates the total power draw of a node from its hardware
// specs and current cap percentages. Used for per-rack PSU stress aggregation.
func estimateNodePowerW(nh NodeHardware, cpuCapPct, gpuCapPct float64) float64 {
	var powerW float64
	if nh.CPUCapMaxWatts > 0 {
		powerW += nh.CPUCapMaxWatts * float64(nh.CPUSockets) * (cpuCapPct / 100.0)
	}
	if nh.GPUCapMaxWatts > 0 {
		powerW += nh.GPUCapMaxWatts * float64(nh.GPUCount) * (gpuCapPct / 100.0)
	}
	return powerW
}

func floatPtr(v float64) *float64 {
	vv := v
	return &vv
}

func warnNoGPUIntentOnce(nodeName, profile, reason string) {
	key := nodeName + "|" + profile + "|" + reason
	gpuIntentWarningMu.Lock()
	if _, ok := gpuIntentWarningSeen[key]; ok {
		gpuIntentWarningMu.Unlock()
		return
	}
	gpuIntentWarningSeen[key] = struct{}{}
	gpuIntentWarningMu.Unlock()
	log.Printf("warning: node=%s profile=%s has allocatable GPUs but no gpu.powerCap intent (%s); continuing without GPU control", nodeName, profile, reason)
}

// upsertNodeProfile is replaced by upsertNodeTwinSpec in twinstate.go

func upsertNodeLabels(ctx context.Context, kube kubernetes.Interface, profileLabel string, a NodeAssignment) error {
	labelValue := a.Profile
	node, err := kube.CoreV1().Nodes().Get(ctx, a.NodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s before patch: %w", a.NodeName, err)
	}
	currentProfile := strings.TrimSpace(node.Labels[profileLabel])
	if currentProfile == labelValue {
		return nil
	}
	patch := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				profileLabel: labelValue,
			},
		},
	}
	rawPatch, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal node label patch for %s: %w", a.NodeName, err)
	}
	if _, err := kube.CoreV1().Nodes().Patch(ctx, a.NodeName, types.MergePatchType, rawPatch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch node %s label %s=%s: %w", a.NodeName, profileLabel, labelValue, err)
	}
	return nil
}

func sanitizeName(in string) string {
	in = strings.ToLower(in)
	var b strings.Builder
	for _, r := range in {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteRune('-')
	}
	return strings.Trim(b.String(), "-")
}

func durationEnv(key string, def time.Duration) time.Duration {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return def
}

func floatEnv(key string, def float64) float64 {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v
		}
	}
	return def
}

func intEnv(key string, def int) int {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return def
}

func boolEnv(key string, def bool) bool {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		return strings.EqualFold(s, "true") || s == "1" || strings.EqualFold(s, "yes")
	}
	return def
}

func envOrDefault(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}
