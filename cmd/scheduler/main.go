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
	"net/http"
	"os"
	"sync"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	nodeTwinGVR     = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodetwins"}
	nodeHardwareGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodehardwares"}

	twinStateCache    map[string]*joulie.NodeTwinStatus
	twinStateMu       sync.RWMutex
	twinStateCacheTTL = 30 * time.Second
	lastCacheRefresh  time.Time

	// NodeHardware cache: tracks GPU presence per node.
	nodeHWCache        map[string]nodeHWInfo
	nodeHWMu           sync.RWMutex
	lastNodeHWRefresh  time.Time

	// twinStalenessThreshold: if a NodeTwin.status.lastUpdated is older than
	// this, the scheduler treats it as stale and falls back to a neutral score.
	// Default 5 minutes. Override via TWIN_STALENESS_THRESHOLD env var.
	twinStalenessThreshold = durationEnvOrDefault("TWIN_STALENESS_THRESHOLD", 5*time.Minute)
)

// nodeHWInfo holds minimal hardware facts needed for scoring.
type nodeHWInfo struct {
	GPUPresent bool
	GPUCount   int
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
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
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
var k8sClient *kubernetes.Clientset

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
		k8sClient, err = kubernetes.NewForConfig(cfg)
		if err != nil {
			log.Fatalf("failed to create k8s client: %v", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/filter", handleFilter)
	mux.HandleFunc("/prioritize", handlePrioritize)
	mux.HandleFunc("/healthz", handleHealthz)

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
	var args ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, fmt.Sprintf("decode error: %v", err), http.StatusBadRequest)
		return
	}

	result := filterNodes(r.Context(), args)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("warning: failed to encode filter response: %v", err)
	}
}

func handlePrioritize(w http.ResponseWriter, r *http.Request) {
	var args ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, fmt.Sprintf("decode error: %v", err), http.StatusBadRequest)
		return
	}

	result := prioritizeNodes(r.Context(), args)
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

// podRequestsGPU returns true if the pod requests any GPU resources.
func podRequestsGPU(pod PodSpec) bool {
	for _, c := range pod.Spec.Containers {
		for k := range c.Resources.Requests {
			if k == "nvidia.com/gpu" || k == "amd.com/gpu" || k == "gpu.intel.com/i915" {
				return true
			}
		}
		for k := range c.Resources.Limits {
			if k == "nvidia.com/gpu" || k == "amd.com/gpu" || k == "gpu.intel.com/i915" {
				return true
			}
		}
	}
	return false
}

// prioritizeNodes scores candidate nodes using NodeTwin status.
//
// Scoring formula:
//
//	base = powerHeadroom * 0.4 + (100 - coolingStress) * 0.3 + (100 - psuStress) * 0.3
//
// Adjustments:
//   - Adaptive pressure relief: when performance nodes are congested, standard pods
//     get a penalty on performance nodes (up to -30), steering them toward eco nodes.
//   - CPU-only pod on GPU node: -30 (avoid wasting GPU idle power on CPU-only work).
func prioritizeNodes(ctx context.Context, args ExtenderArgs) ExtenderPriorityList {
	states := getNodeTwinStates(ctx)
	hwInfo := getNodeHardwareInfo(ctx)

	// Parse pod for workload class
	podBytes, err := json.Marshal(args.Pod)
	if err != nil {
		log.Printf("warning: cannot marshal pod for scoring: %v", err)
	}
	var pod PodSpec
	if err := json.Unmarshal(podBytes, &pod); err != nil {
		log.Printf("warning: cannot unmarshal pod for scoring: %v", err)
	}
	wpClass := podWorkloadClass(pod)
	gpuRequested := podRequestsGPU(pod)

	perfPressure := computePerfPressure(states)

	var result ExtenderPriorityList
	if args.Nodes != nil {
		for _, node := range args.Nodes.Items {
			score := scoreNode(node.Metadata.Name, states, hwInfo, wpClass, perfPressure, gpuRequested)
			result = append(result, HostPriority{Host: node.Metadata.Name, Score: score})
		}
	} else if args.NodeNames != nil {
		for _, nodeName := range *args.NodeNames {
			score := scoreNode(nodeName, states, hwInfo, wpClass, perfPressure, gpuRequested)
			result = append(result, HostPriority{Host: nodeName, Score: score})
		}
	}
	return result
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

func scoreNode(nodeName string, states map[string]*joulie.NodeTwinStatus, hwInfo map[string]nodeHWInfo, wpClass string, perfPressure float64, gpuRequested bool) int64 {
	state, ok := states[nodeName]
	if !ok {
		return 50 // neutral score if no twin state
	}
	if isTwinStale(state) {
		log.Printf("warning: stale twin data for node %s (lastUpdated=%s); using neutral score", nodeName, state.LastUpdated.Format(time.RFC3339))
		return 50
	}

	headroom := state.PredictedPowerHeadroomScore
	coolStress := state.PredictedCoolingStressScore
	psuStress := state.PredictedPsuStressScore

	score := headroom*0.4 + (100-coolStress)*0.3 + (100-psuStress)*0.3

	// Adaptive pressure relief: when performance nodes are congested,
	// steer standard pods away from them to preserve capacity for performance pods.
	if wpClass == "standard" && state.SchedulableClass == "performance" {
		score -= perfPressure * 0.3 // up to -30 at full saturation
	}

	// CPU-only pod on GPU node: strongly discourage to avoid wasting GPU idle power.
	if hw, ok := hwInfo[nodeName]; ok && hw.GPUPresent && !gpuRequested {
		score -= 30
	}

	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return int64(score)
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

// parseNodeHardware extracts minimal hardware info from a NodeHardware unstructured object.
func parseNodeHardware(u unstructured.Unstructured) (string, nodeHWInfo) {
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	var info nodeHWInfo

	var nodeName string
	if spec != nil {
		if v, ok := spec["nodeName"].(string); ok {
			nodeName = v
		}
	}

	// GPU info is in spec.gpu
	if spec != nil {
		if gpu, ok := spec["gpu"].(map[string]interface{}); ok {
			if v, ok := gpu["present"].(bool); ok {
				info.GPUPresent = v
			}
			if v, ok := gpu["count"].(int64); ok {
				info.GPUCount = int(v)
			} else if v, ok := gpu["count"].(float64); ok {
				info.GPUCount = int(v)
			}
			if info.GPUCount > 0 {
				info.GPUPresent = true
			}
		}
	}
	return nodeName, info
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
