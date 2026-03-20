// Package main implements the Joulie scheduler extender.
//
// The scheduler extender is deployed as an HTTP server and integrated with
// kube-scheduler via the KubeSchedulerConfiguration extenderConfig. It
// implements the Filter and Prioritize endpoints of the scheduler extender
// protocol.
//
// The extender reads NodeTwin CRs for placement decisions. It uses a single
// pod annotation (joulie.io/workload-class) and adaptive scoring.
//
// Scoring formula:
//
//	projectedPower = measuredPower + podMarginalPower
//	headroomScore  = (cappedPower - projectedPower) / cappedPower * 100
//	coolingStress  = clamp((measuredPower / nodeTDP) * tempMultiplier * 100, 0, 100)
//	clusterTrend   = sum of all per-node powerTrend (W/min)
//	trendScale     = 2.0 if |clusterTrend| > 500 else 6.0
//	trendBonus     = -clamp(powerTrend / trendScale, -25, 25)
//	score = headroomScore * 0.7 + (100 - coolingStress) * 0.15 + trendBonus + profileBonus + pressureRelief
//
// Marginal pod power is subtracted from headroom before scoring, making the
// score pod-specific. headroomScore can go negative (pod would exceed budget).
//
// Resilience: twin data older than TWIN_STALENESS_THRESHOLD (default 5m) is
// treated as stale and the node receives a neutral score instead of potentially
// misleading values from an operator that may have crashed.
//
// Scheduler extender protocol reference:
//
//	https://kubernetes.io/docs/reference/scheduling/config/#extension-points
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/scheduler/powerest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var (
	nodeTwinGVR     = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodetwins"}
	nodeHardwareGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodehardwares"}

	twinStateCache    map[string]*joulie.NodeTwinStatus
	twinStateMu       sync.RWMutex
	twinStateCacheTTL = envDurationOrDefault("CACHE_TTL", 30*time.Second)
	lastCacheRefresh  time.Time

	// NodeHardware cache: tracks hardware power envelope per node.
	nodeHWCache       map[string]nodeHWInfo
	nodeHWMu          sync.RWMutex
	lastNodeHWRefresh time.Time

	// twinStalenessThreshold: if a NodeTwin.status.lastUpdated is older than
	// this, the scheduler treats it as stale and falls back to a neutral score.
	// Default 5 minutes. Override via TWIN_STALENESS_THRESHOLD env var.
	twinStalenessThreshold = durationEnvOrDefault("TWIN_STALENESS_THRESHOLD", 5*time.Minute)

	// marginalCoeff holds the tunable coefficients for power estimation.
	marginalCoeff = loadCoefficients()

	// --- Prometheus metrics ---

	schedulerFilterRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "joulie_scheduler_filter_requests_total",
			Help: "Total filter requests processed by workload class.",
		},
		[]string{"workload_class"},
	)
	schedulerPrioritizeRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "joulie_scheduler_prioritize_requests_total",
			Help: "Total prioritize requests processed by workload class.",
		},
		[]string{"workload_class"},
	)
	schedulerFilterDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "joulie_scheduler_filter_duration_seconds",
			Help:    "Time to process a filter request.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"workload_class"},
	)
	schedulerPrioritizeDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "joulie_scheduler_prioritize_duration_seconds",
			Help:    "Time to process a prioritize request.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"workload_class"},
	)
	schedulerFinalNodeScore = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "joulie_scheduler_final_node_score",
			Help: "Final scheduling score per node and workload class (0-100).",
		},
		[]string{"node", "workload_class"},
	)
	schedulerNodeHeadroomScore = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "joulie_scheduler_node_headroom_score",
			Help: "Power headroom score per node (0-100, can be negative if pod exceeds budget).",
		},
		[]string{"node"},
	)
	schedulerStaleTwinNodes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "joulie_scheduler_stale_twin_data",
			Help: "Set to 1 if twin data is stale for a node, 0 otherwise.",
		},
		[]string{"node"},
	)
)

// nodeHWInfo holds hardware facts needed for scoring and marginal estimation.
type nodeHWInfo struct {
	GPUPresent        bool
	GPUCount          int
	CPUTotalCores     int
	CPUSockets        int
	CPUMaxWattsTotal  float64 // maxWattsPerSocket * sockets
	GPUMaxWattsPerGPU float64
	CPUModel          string
	GPUModel          string
	GPUVendor         string
	MemoryBytes       int64
}

// ExtenderArgs is the request body for filter/prioritize calls.
type ExtenderArgs struct {
	Pod       interface{} `json:"pod"`
	Nodes     *NodeList   `json:"nodes,omitempty"`
	NodeNames *[]string   `json:"nodenames,omitempty"`
}

// NodeList wraps a list of node names for the extender protocol.
type NodeList struct {
	Items []NodeItem `json:"items"`
}

// NodeItem represents a node in the extender protocol.
type NodeItem struct {
	Metadata NodeMeta `json:"metadata"`
	Spec     NodeSpec `json:"spec,omitempty"`
}

type NodeMeta struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

type NodeSpec struct {
	// intentionally minimal
}

// ExtenderFilterResult is the response for filter calls.
type ExtenderFilterResult struct {
	Nodes       *NodeList         `json:"nodes,omitempty"`
	NodeNames   *[]string         `json:"nodenames,omitempty"`
	FailedNodes map[string]string `json:"failedNodes,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// ExtenderPriorityList is the response for prioritize calls.
type ExtenderPriorityList []HostPriority

// HostPriority pairs a node name with a score.
type HostPriority struct {
	Host  string `json:"host"`
	Score int64  `json:"score"`
}

// PodSpec holds the minimal pod fields needed for scheduling decisions.
type PodSpec struct {
	Metadata PodMeta `json:"metadata"`
	Spec     PodBody `json:"spec"`
}

type PodMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	OwnerReferences []OwnerRef        `json:"ownerReferences,omitempty"`
}

// OwnerRef captures the minimal owner reference fields.
type OwnerRef struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

type PodBody struct {
	NodeSelector map[string]string  `json:"nodeSelector,omitempty"`
	Affinity     *AffinitySpec      `json:"affinity,omitempty"`
	Containers   []ContainerSpec    `json:"containers,omitempty"`
}

type AffinitySpec struct {
	NodeAffinity *NodeAffinitySpec `json:"nodeAffinity,omitempty"`
}

type NodeAffinitySpec struct {
	RequiredDuringSchedulingIgnoredDuringExecution *NodeSelectorSpec `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

type NodeSelectorSpec struct {
	NodeSelectorTerms []NodeSelectorTerm `json:"nodeSelectorTerms,omitempty"`
}

type NodeSelectorTerm struct {
	MatchExpressions []NodeSelectorRequirement `json:"matchExpressions,omitempty"`
}

type NodeSelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitempty"`
}

// ContainerSpec holds minimal container fields for GPU detection.
type ContainerSpec struct {
	Resources ResourceSpec `json:"resources,omitempty"`
}

type ResourceSpec struct {
	Requests map[string]interface{} `json:"requests,omitempty"`
	Limits   map[string]interface{} `json:"limits,omitempty"`
}

var dynClient dynamic.Interface

func envDurationOrDefault(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("warning: invalid %s=%q, using default %s", key, v, def)
		return def
	}
	return d
}

func main() {
	addr := os.Getenv("EXTENDER_ADDR")
	if addr == "" {
		addr = ":9876"
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("Warning: in-cluster config failed: %v. Running without Kubernetes.", err)
	} else {
		dynClient, err = dynamic.NewForConfig(cfg)
		if err != nil {
			log.Fatalf("failed to create dynamic client: %v", err)
		}
	}

	log.Printf("coefficients: cpuUtil=%.2f gpuUtilStd=%.2f gpuUtilPerf=%.2f",
		marginalCoeff.CPUUtilCoeff, marginalCoeff.GPUUtilCoeffStandard,
		marginalCoeff.GPUUtilCoeffPerformance)

	prometheus.MustRegister(
		schedulerFilterRequests,
		schedulerPrioritizeRequests,
		schedulerFilterDuration,
		schedulerPrioritizeDuration,
		schedulerFinalNodeScore,
		schedulerNodeHeadroomScore,
		schedulerStaleTwinNodes,
	)

	metricsAddr := os.Getenv("METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":9877"
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Printf("metrics server listening on %s", metricsAddr)
		if err := http.ListenAndServe(metricsAddr, metricsMux); err != nil {
			log.Printf("metrics server failed: %v", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/filter", handleFilter)
	mux.HandleFunc("/prioritize", handlePrioritize)
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/debug/scoring", handleDebugScoring)

	log.Printf("Joulie scheduler extender starting on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("extender server failed: %v", err)
	}
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

func handleFilter(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var args ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, fmt.Sprintf("decode error: %v", err), http.StatusBadRequest)
		return
	}

	// Parse pod to extract workload class for metrics.
	wpClass := extractWorkloadClass(args)

	result := filterNodes(r.Context(), args)
	schedulerFilterRequests.WithLabelValues(wpClass).Inc()
	schedulerFilterDuration.WithLabelValues(wpClass).Observe(time.Since(start).Seconds())

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("warning: failed to encode filter response: %v", err)
	}
}

func handlePrioritize(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var args ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, fmt.Sprintf("decode error: %v", err), http.StatusBadRequest)
		return
	}

	wpClass := extractWorkloadClass(args)

	result := prioritizeNodes(r.Context(), args)
	schedulerPrioritizeRequests.WithLabelValues(wpClass).Inc()
	schedulerPrioritizeDuration.WithLabelValues(wpClass).Observe(time.Since(start).Seconds())

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("warning: failed to encode prioritize response: %v", err)
	}
}

// filterNodes applies Joulie filter logic to candidate nodes.
//
// Filter rules:
//  1. Performance pods are rejected from eco and draining nodes.
//  2. Standard pods pass all nodes.
func filterNodes(ctx context.Context, args ExtenderArgs) ExtenderFilterResult {
	result := ExtenderFilterResult{
		FailedNodes: make(map[string]string),
	}

	// Parse pod
	podBytes, err := json.Marshal(args.Pod)
	if err != nil {
		log.Printf("warning: cannot marshal pod for classification: %v", err)
	}
	var pod PodSpec
	if err := json.Unmarshal(podBytes, &pod); err != nil {
		log.Printf("warning: cannot unmarshal pod for classification: %v", err)
	}

	isPerformance := podWorkloadClass(pod) == "performance"
	states := getNodeTwinStates(ctx)

	var passedNodes []NodeItem
	var passedNames []string
	if args.Nodes != nil {
		for _, node := range args.Nodes.Items {
			nodeName := node.Metadata.Name
			reason := shouldFilterNode(nodeName, states, node.Metadata.Labels, isPerformance)
			if reason != "" {
				result.FailedNodes[nodeName] = reason
			} else {
				passedNodes = append(passedNodes, node)
			}
		}
		result.Nodes = &NodeList{Items: passedNodes}
	} else if args.NodeNames != nil {
		for _, nodeName := range *args.NodeNames {
			reason := shouldFilterNode(nodeName, states, nil, isPerformance)
			if reason != "" {
				result.FailedNodes[nodeName] = reason
			} else {
				passedNames = append(passedNames, nodeName)
			}
		}
		result.NodeNames = &passedNames
	}

	return result
}

// shouldFilterNode returns a non-empty rejection reason if this node should be
// excluded for this pod. Performance pods are rejected from eco and draining nodes.
func shouldFilterNode(nodeName string, states map[string]*joulie.NodeTwinStatus, labels map[string]string, isPerformance bool) string {
	if !isPerformance {
		return "" // standard pods can go anywhere
	}

	state, hasState := states[nodeName]
	if hasState && (state.SchedulableClass == "eco" || state.SchedulableClass == "draining") {
		return "joulie: performance pod rejected from eco/draining node"
	}

	// Label fallback (works without NodeTwin status)
	if labels != nil && labels["joulie.io/power-profile"] == "eco" {
		return "joulie: performance pod rejected from eco node (label fallback)"
	}
	return ""
}

// extractWorkloadClass parses the pod from ExtenderArgs and returns the workload class.
func extractWorkloadClass(args ExtenderArgs) string {
	podBytes, err := json.Marshal(args.Pod)
	if err != nil {
		return "standard"
	}
	var pod PodSpec
	if err := json.Unmarshal(podBytes, &pod); err != nil {
		return "standard"
	}
	return podWorkloadClass(pod)
}

// podWorkloadClass returns the workload class from pod annotations,
// defaulting to "standard".
func podWorkloadClass(pod PodSpec) string {
	if pod.Metadata.Annotations != nil {
		if v := pod.Metadata.Annotations["joulie.io/workload-class"]; v != "" {
			return v
		}
	}
	return "standard"
}

// prioritizeNodes scores candidate nodes using NodeTwin status and marginal
// power estimation from pod resource requests.
//
// Scoring formula:
//
//	score = headroomScore * 0.7 + (100 - coolingStress) * 0.15 + trendBonus + profileBonus + pressureRelief
//
// where headroomScore = (cappedPower - projectedPower) / cappedPower * 100
// and projectedPower = measuredPower + podMarginalPower.
//
// Trend uses adaptive scaling: trendScale = 2.0 during cluster-wide bursts
// (|clusterTrend| > 500 W/min), 6.0 at steady state. Capped at ±25 points.
func prioritizeNodes(ctx context.Context, args ExtenderArgs) ExtenderPriorityList {
	states := getNodeTwinStates(ctx)
	hwInfo := getNodeHardwareInfo(ctx)

	// Parse pod for workload class and resource demand.
	podBytes, err := json.Marshal(args.Pod)
	if err != nil {
		log.Printf("warning: cannot marshal pod for scoring: %v", err)
	}
	var pod PodSpec
	if err := json.Unmarshal(podBytes, &pod); err != nil {
		log.Printf("warning: cannot unmarshal pod for scoring: %v", err)
	}
	wpClass := podWorkloadClass(pod)

	// Build pod demand for marginal estimation.
	var podDemand *powerest.PodDemand
	podRaw, _ := args.Pod.(map[string]interface{})
	if podRaw != nil {
		d := powerest.ExtractPodDemand(podRaw, wpClass)
		podDemand = &d
	}

	perfPressure := computePerfPressure(states)
	clusterTrend := computeClusterTrend(states)

	var result ExtenderPriorityList
	if args.Nodes != nil {
		for _, node := range args.Nodes.Items {
			score := scoreNode(node.Metadata.Name, states, hwInfo, wpClass, perfPressure, clusterTrend, podDemand)
			result = append(result, HostPriority{Host: node.Metadata.Name, Score: score})
		}
	} else if args.NodeNames != nil {
		for _, nodeName := range *args.NodeNames {
			score := scoreNode(nodeName, states, hwInfo, wpClass, perfPressure, clusterTrend, podDemand)
			result = append(result, HostPriority{Host: nodeName, Score: score})
		}
	}
	return result
}

// computeClusterTrend returns the average power trend (W/min) across all
// non-stale nodes. Used to detect cluster-wide power bursts for adaptive
// trend scaling.
func computeClusterTrend(states map[string]*joulie.NodeTwinStatus) float64 {
	var total, count float64
	for _, s := range states {
		if isTwinStale(s) {
			continue
		}
		if pm := s.PowerMeasurement; pm != nil {
			total += pm.PowerTrendWPerMin
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return total // sum of per-node trends ≈ cluster-wide power ramp rate
}

// computePerfPressure returns the average congestion (0-100) across performance nodes.
// 0 = perf nodes idle, 100 = perf nodes saturated.
func computePerfPressure(states map[string]*joulie.NodeTwinStatus) float64 {
	var total, count float64
	for _, s := range states {
		if s.SchedulableClass == "performance" && !isTwinStale(s) {
			total += 100 - s.PredictedPowerHeadroomScore
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return total / count
}

func scoreNode(nodeName string, states map[string]*joulie.NodeTwinStatus, hwInfo map[string]nodeHWInfo, wpClass string, perfPressure float64, clusterTrend float64, podDemand *powerest.PodDemand) int64 {
	state, ok := states[nodeName]
	if !ok {
		schedulerFinalNodeScore.WithLabelValues(nodeName, wpClass).Set(50)
		schedulerStaleTwinNodes.WithLabelValues(nodeName).Set(1)
		return 50 // neutral score if no twin state
	}
	if isTwinStale(state) {
		log.Printf("warning: stale twin data for node %s (lastUpdated=%s); using neutral score", nodeName, state.LastUpdated.Format(time.RFC3339))
		schedulerFinalNodeScore.WithLabelValues(nodeName, wpClass).Set(50)
		schedulerStaleTwinNodes.WithLabelValues(nodeName).Set(1)
		return 50
	}
	schedulerStaleTwinNodes.WithLabelValues(nodeName).Set(0)

	// Extract power measurement data from twin.
	var measuredPowerW, cappedPowerW, powerTrendWPerMin float64
	if pm := state.PowerMeasurement; pm != nil {
		measuredPowerW = pm.MeasuredNodePowerW
		cappedPowerW = pm.NodeCappedPowerW
		powerTrendWPerMin = pm.PowerTrendWPerMin
	}

	// Estimate pod marginal power and subtract from headroom.
	var podMarginalW float64
	hw, hasHW := hwInfo[nodeName]
	if podDemand != nil && hasHW {
		nodeProfile := hwInfoToProfile(nodeName, hw)
		est := powerest.EstimateMarginalImpact(*podDemand, &nodeProfile, marginalCoeff)
		podMarginalW = est.DeltaTotalWatts
		log.Printf("marginal: node=%s pod_cpu=%.2f pod_gpu=%d delta=%.1fW [%s]",
			nodeName, podDemand.CPUCores, podDemand.GPUCount, est.DeltaTotalWatts, est.Explanation)
	}

	// 1. Projected headroom score: how much capped budget remains after this pod.
	//    headroomScore = (cappedPower - projectedPower) / cappedPower * 100
	//    Can go negative (pod would exceed budget), clamped at 100 max.
	headroomScore := state.PredictedPowerHeadroomScore // fallback: twin-computed headroom
	if cappedPowerW > 0 {
		projectedPower := measuredPowerW + podMarginalW
		headroomScore = (cappedPowerW - projectedPower) / cappedPowerW * 100.0
		headroomScore = math.Min(100, headroomScore) // no upper clamp; can go negative
	}

	// 2. Cooling stress (already computed by twin, 0-100).
	coolingStress := state.PredictedCoolingStressScore

	// 3. Trend bonus: reward nodes with falling power, penalize rising power.
	//    Uses adaptive scaling: trendScale = 2.0 during cluster-wide bursts
	//    (|clusterTrend| > 500 W/min), 6.0 at steady state. Capped at ±25 points.
	var trendBonus float64
	if powerTrendWPerMin != 0 {
		trendScale := 6.0
		if math.Abs(clusterTrend) > 500.0 {
			trendScale = 2.0
		}
		trendBonus = -math.Max(-25, math.Min(25, powerTrendWPerMin/trendScale))
	}

	// Combined score.
	score := headroomScore*0.7 + (100-coolingStress)*0.15 + trendBonus

	// Profile-aware bonus: steer standard pods toward eco nodes.
	if wpClass == "standard" && state.SchedulableClass == "eco" {
		score += 10
	}
	// Adaptive pressure relief: when performance nodes are congested,
	// steer standard pods away from them to preserve capacity for performance pods.
	if wpClass == "standard" && state.SchedulableClass == "performance" {
		score -= perfPressure * 0.3 // up to -30 at full saturation
	}

	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}

	schedulerNodeHeadroomScore.WithLabelValues(nodeName).Set(headroomScore)
	schedulerFinalNodeScore.WithLabelValues(nodeName, wpClass).Set(score)

	return int64(score)
}

// hwInfoToProfile converts the cached nodeHWInfo into a powerest.NodePowerProfile.
func hwInfoToProfile(nodeName string, hw nodeHWInfo) powerest.NodePowerProfile {
	return powerest.NodePowerProfile{
		NodeName:          nodeName,
		CPUModel:          hw.CPUModel,
		GPUModel:          hw.GPUModel,
		CPUTotalCores:     hw.CPUTotalCores,
		CPUSockets:        hw.CPUSockets,
		CPUMaxWattsTotal:  hw.CPUMaxWattsTotal,
		GPUCount:          hw.GPUCount,
		GPUMaxWattsPerGPU: hw.GPUMaxWattsPerGPU,
		HasGPU:            hw.GPUPresent,
		MemoryBytes:       hw.MemoryBytes,
	}
}

// --- NodeTwin cache ---

// getNodeTwinStates retrieves NodeTwin status objects, using a short-lived cache.
func getNodeTwinStates(ctx context.Context) map[string]*joulie.NodeTwinStatus {
	twinStateMu.RLock()
	if time.Since(lastCacheRefresh) < twinStateCacheTTL && twinStateCache != nil {
		defer twinStateMu.RUnlock()
		return twinStateCache
	}
	twinStateMu.RUnlock()

	twinStateMu.Lock()
	defer twinStateMu.Unlock()

	// Double-check after acquiring write lock: another goroutine may have refreshed.
	if time.Since(lastCacheRefresh) < twinStateCacheTTL && twinStateCache != nil {
		return twinStateCache
	}

	if dynClient == nil {
		return make(map[string]*joulie.NodeTwinStatus)
	}

	list, err := dynClient.Resource(nodeTwinGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Printf("failed to list NodeTwin: %v", err)
		}
		return make(map[string]*joulie.NodeTwinStatus)
	}

	states := make(map[string]*joulie.NodeTwinStatus, len(list.Items))
	for _, item := range list.Items {
		nodeName, ts := parseTwinState(item)
		if nodeName != "" {
			states[nodeName] = ts
		}
	}
	twinStateCache = states
	lastCacheRefresh = time.Now()
	return states
}

// parseTwinState returns (nodeName, status) from a NodeTwin unstructured object.
func parseTwinState(u unstructured.Unstructured) (string, *joulie.NodeTwinStatus) {
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	status, _, _ := unstructured.NestedMap(u.Object, "status")

	var nodeName string
	if spec != nil {
		if v, ok := spec["nodeName"].(string); ok {
			nodeName = v
		}
	}
	ts := &joulie.NodeTwinStatus{}
	if status != nil {
		if v, ok := status["schedulableClass"].(string); ok {
			ts.SchedulableClass = v
		}
		if v, ok := status["predictedPowerHeadroomScore"].(float64); ok {
			ts.PredictedPowerHeadroomScore = v
		}
		if v, ok := status["predictedCoolingStressScore"].(float64); ok {
			ts.PredictedCoolingStressScore = v
		}
		if v, ok := status["predictedPsuStressScore"].(float64); ok {
			ts.PredictedPsuStressScore = v
		}
		if cap, ok := status["effectiveCapState"].(map[string]interface{}); ok {
			if v, ok := cap["cpuPct"].(float64); ok {
				ts.EffectiveCapState.CPUPct = v
			}
			if v, ok := cap["gpuPct"].(float64); ok {
				ts.EffectiveCapState.GPUPct = v
			}
		}
		if v, ok := status["hardwareDensityScore"].(float64); ok {
			ts.HardwareDensityScore = v
		}
		if v, ok := status["estimatedPUE"].(float64); ok {
			ts.EstimatedPUE = v
		}
		if pm, ok := status["powerMeasurement"].(map[string]interface{}); ok {
			ts.PowerMeasurement = &joulie.PowerMeasurement{}
			if v, ok := pm["source"].(string); ok {
				ts.PowerMeasurement.Source = v
			}
			if v, ok := pm["measuredNodePowerW"].(float64); ok {
				ts.PowerMeasurement.MeasuredNodePowerW = v
			}
			if v, ok := pm["cpuCappedPowerW"].(float64); ok {
				ts.PowerMeasurement.CpuCappedPowerW = v
			}
			if v, ok := pm["gpuCappedPowerW"].(float64); ok {
				ts.PowerMeasurement.GpuCappedPowerW = v
			}
			if v, ok := pm["nodeCappedPowerW"].(float64); ok {
				ts.PowerMeasurement.NodeCappedPowerW = v
			}
			if v, ok := pm["cpuTdpW"].(float64); ok {
				ts.PowerMeasurement.CpuTdpW = v
			}
			if v, ok := pm["gpuTdpW"].(float64); ok {
				ts.PowerMeasurement.GpuTdpW = v
			}
			if v, ok := pm["nodeTdpW"].(float64); ok {
				ts.PowerMeasurement.NodeTdpW = v
			}
			if v, ok := pm["powerTrendWPerMin"].(float64); ok {
				ts.PowerMeasurement.PowerTrendWPerMin = v
			}
		}
		if v, ok := status["lastUpdated"].(string); ok {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				ts.LastUpdated = t
			}
		}
	}
	return nodeName, ts
}

// --- NodeHardware cache ---

// getNodeHardwareInfo retrieves NodeHardware objects, using a short-lived cache.
func getNodeHardwareInfo(ctx context.Context) map[string]nodeHWInfo {
	nodeHWMu.RLock()
	if time.Since(lastNodeHWRefresh) < twinStateCacheTTL && nodeHWCache != nil {
		defer nodeHWMu.RUnlock()
		return nodeHWCache
	}
	nodeHWMu.RUnlock()

	nodeHWMu.Lock()
	defer nodeHWMu.Unlock()

	if time.Since(lastNodeHWRefresh) < twinStateCacheTTL && nodeHWCache != nil {
		return nodeHWCache
	}

	if dynClient == nil {
		return make(map[string]nodeHWInfo)
	}

	list, err := dynClient.Resource(nodeHardwareGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Printf("failed to list NodeHardware: %v", err)
		}
		return make(map[string]nodeHWInfo)
	}

	hw := make(map[string]nodeHWInfo, len(list.Items))
	for _, item := range list.Items {
		nodeName, info := parseNodeHardware(item)
		if nodeName != "" {
			hw[nodeName] = info
		}
	}
	nodeHWCache = hw
	lastNodeHWRefresh = time.Now()
	return hw
}

// parseNodeHardware extracts hardware info from a NodeHardware unstructured object.
// The CRD stores hardware data in status (written by the agent as a subresource),
// while spec only contains nodeName.
func parseNodeHardware(u unstructured.Unstructured) (string, nodeHWInfo) {
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	status, _, _ := unstructured.NestedMap(u.Object, "status")
	var info nodeHWInfo

	var nodeName string
	if spec != nil {
		if v, ok := spec["nodeName"].(string); ok {
			nodeName = v
		}
	}

	if status == nil {
		return nodeName, info
	}

	// CPU info from status.cpu
	if cpu, ok := status["cpu"].(map[string]interface{}); ok {
		info.CPUTotalCores = intFromMap(cpu, "totalCores")
		info.CPUSockets = intFromMap(cpu, "sockets")
		if v, ok := cpu["model"].(string); ok {
			info.CPUModel = v
		}
		if capRange, ok := cpu["capRange"].(map[string]interface{}); ok {
			maxPerSocket := floatFromMap(capRange, "maxWattsPerSocket")
			sockets := info.CPUSockets
			if sockets <= 0 {
				sockets = 1
			}
			info.CPUMaxWattsTotal = maxPerSocket * float64(sockets)
		}
	}

	// GPU info from status.gpu
	if gpu, ok := status["gpu"].(map[string]interface{}); ok {
		if v, ok := gpu["present"].(bool); ok {
			info.GPUPresent = v
		}
		info.GPUCount = intFromMap(gpu, "count")
		if info.GPUCount > 0 {
			info.GPUPresent = true
		}
		if v, ok := gpu["model"].(string); ok {
			info.GPUModel = v
		}
		if v, ok := gpu["vendor"].(string); ok {
			info.GPUVendor = v
		}
		if capRange, ok := gpu["capRangePerGpu"].(map[string]interface{}); ok {
			info.GPUMaxWattsPerGPU = floatFromMap(capRange, "maxWatts")
		}
	}

	// Memory from status.memory
	if mem, ok := status["memory"].(map[string]interface{}); ok {
		info.MemoryBytes = int64(floatFromMap(mem, "totalBytes"))
	}

	return nodeName, info
}

// intFromMap extracts an integer from a map, handling both int64 and float64.
func intFromMap(m map[string]interface{}, key string) int {
	if v, ok := m[key].(int64); ok {
		return int(v)
	}
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

// floatFromMap extracts a float64 from a map.
func floatFromMap(m map[string]interface{}, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	if v, ok := m[key].(int64); ok {
		return float64(v)
	}
	return 0
}

// isTwinStale returns true if the twin data is too old to trust for scheduling.
func isTwinStale(ts *joulie.NodeTwinStatus) bool {
	if ts.LastUpdated.IsZero() {
		return true // no timestamp = operator hasn't populated status yet; treat as stale
	}
	return time.Since(ts.LastUpdated) > twinStalenessThreshold
}

func durationEnvOrDefault(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("warning: invalid %s=%q, using default %s", key, v, fallback)
		return fallback
	}
	return d
}

func floatEnvOrDefault(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("warning: invalid %s=%q, using default %.2f", key, v, fallback)
		return fallback
	}
	return f
}

// loadCoefficients builds powerest.Coefficients from environment variables,
// falling back to production defaults.
func loadCoefficients() powerest.Coefficients {
	c := powerest.DefaultCoefficients()
	c.CPUUtilCoeff = floatEnvOrDefault("MARGINAL_CPU_UTIL_COEFF", c.CPUUtilCoeff)
	c.GPUUtilCoeffStandard = floatEnvOrDefault("MARGINAL_GPU_UTIL_COEFF_STANDARD", c.GPUUtilCoeffStandard)
	c.GPUUtilCoeffPerformance = floatEnvOrDefault("MARGINAL_GPU_UTIL_COEFF_PERFORMANCE", c.GPUUtilCoeffPerformance)
	return c
}

// --- Debug endpoint (Phase 4: observability) ---

// debugScoringResponse is the JSON output of /debug/scoring.
type debugScoringResponse struct {
	Coefficients powerest.Coefficients  `json:"coefficients"`
	Nodes        []debugNodeScoringEntry `json:"nodes"`
}

type debugNodeScoringEntry struct {
	NodeName          string  `json:"nodeName"`
	SchedulableClass  string  `json:"schedulableClass"`
	Headroom          float64 `json:"headroom"`
	CoolingStress     float64 `json:"coolingStress"`
	MeasuredPowerW    float64 `json:"measuredPowerW"`
	CappedPowerW      float64 `json:"cappedPowerW"`
	NodeTDPW          float64 `json:"nodeTdpW"`
	PowerTrendWPerMin float64 `json:"powerTrendWPerMin"`
	BaseScore         float64 `json:"baseScore"`
	CPUTotalCores     int     `json:"cpuTotalCores"`
	CPUMaxWattsTotal  float64 `json:"cpuMaxWattsTotal"`
	GPUCount          int     `json:"gpuCount"`
	GPUMaxWattsPerGPU float64 `json:"gpuMaxWattsPerGPU"`
	HasGPU            bool    `json:"hasGpu"`
	Stale             bool    `json:"stale"`
}

func handleDebugScoring(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	states := getNodeTwinStates(ctx)
	hwInfo := getNodeHardwareInfo(ctx)

	resp := debugScoringResponse{
		Coefficients: marginalCoeff,
	}

	for nodeName, state := range states {
		entry := debugNodeScoringEntry{
			NodeName:         nodeName,
			SchedulableClass: state.SchedulableClass,
			Headroom:         state.PredictedPowerHeadroomScore,
			CoolingStress:    state.PredictedCoolingStressScore,
			Stale:            isTwinStale(state),
		}
		if pm := state.PowerMeasurement; pm != nil {
			entry.MeasuredPowerW = pm.MeasuredNodePowerW
			entry.CappedPowerW = pm.NodeCappedPowerW
			entry.NodeTDPW = pm.NodeTdpW
			entry.PowerTrendWPerMin = pm.PowerTrendWPerMin
		}
		// Compute base score (without pod marginal or bonuses).
		entry.BaseScore = state.PredictedPowerHeadroomScore*0.7 + (100-state.PredictedCoolingStressScore)*0.15
		if hw, ok := hwInfo[nodeName]; ok {
			entry.CPUTotalCores = hw.CPUTotalCores
			entry.CPUMaxWattsTotal = hw.CPUMaxWattsTotal
			entry.GPUCount = hw.GPUCount
			entry.GPUMaxWattsPerGPU = hw.GPUMaxWattsPerGPU
			entry.HasGPU = hw.GPUPresent
		}
		resp.Nodes = append(resp.Nodes, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("warning: failed to encode debug response: %v", err)
	}
}

