// Package main implements the Joulie scheduler extender.
//
// The scheduler extender is deployed as an HTTP server and integrated with
// kube-scheduler via the KubeSchedulerConfiguration extenderConfig. It
// implements the Filter and Prioritize endpoints of the scheduler extender
// protocol.
//
// The extender reads NodeTwin and WorkloadProfile CRs to make
// placement decisions. It does NOT run heavy simulation inline.
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
	nodeTwinGVR        = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodetwins"}
	workloadProfileGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "workloadprofiles"}

	twinStateCache    map[string]*joulie.NodeTwinStatus
	twinStateMu       sync.RWMutex
	twinStateCacheTTL = 30 * time.Second
	lastCacheRefresh  time.Time

	// twinStalenessThreshold: if a NodeTwin.status.lastUpdated is older than
	// this, the scheduler treats it as stale and falls back to a neutral score.
	// Default 5 minutes. Override via TWIN_STALENESS_THRESHOLD env var.
	twinStalenessThreshold = durationEnvOrDefault("TWIN_STALENESS_THRESHOLD", 5*time.Minute)
)

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
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Affinity     *AffinitySpec     `json:"affinity,omitempty"`
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
	json.NewEncoder(w).Encode(result)
}

func handlePrioritize(w http.ResponseWriter, r *http.Request) {
	var args ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, fmt.Sprintf("decode error: %v", err), http.StatusBadRequest)
		return
	}

	result := prioritizeNodes(r.Context(), args)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// filterNodes applies Joulie filter logic to candidate nodes.
//
// Filter rules:
//  1. Reject eco nodes for performance pods.
//     performance pods must run on uncapped (performance) nodes.
//  2. All other pods pass all nodes. Eco pods can run on performance nodes.
//
// Draining nodes are NOT filtered out; they receive a score penalty in prioritizeNodes.
// Draining = node is transitioning from performance to eco (still has performance pods).
func filterNodes(ctx context.Context, args ExtenderArgs) ExtenderFilterResult {
	result := ExtenderFilterResult{
		FailedNodes: make(map[string]string),
	}

	// Parse pod
	podBytes, _ := json.Marshal(args.Pod)
	var pod PodSpec
	json.Unmarshal(podBytes, &pod)

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
// excluded for this pod. Only performance-class pods are strictly filtered from eco nodes.
func shouldFilterNode(nodeName string, states map[string]*joulie.NodeTwinStatus, labels map[string]string, isPerformance bool) string {
	if !isPerformance {
		return "" // standard/best-effort pods can go anywhere
	}

	state, hasState := states[nodeName]
	if hasState && (state.SchedulableClass == "eco") {
		return "joulie: performance pod requires performance node, but node is eco"
	}

	// Also check node label directly (works without NodeTwinState)
	if labels != nil && labels["joulie.io/power-profile"] == "eco" {
		return "joulie: performance pod requires performance node, but node is eco (via label)"
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

// prioritizeNodes scores candidate nodes using NodeTwinState.
//
// Scoring formula:
//
//	base = powerHeadroom×0.4 + (100-coolingStress)×0.3 + (100-psuStress)×0.3
//
// Adjustments:
//   - best-effort + eco node: +5 (save performance capacity)
//   - draining node: -20 (operator is trying to drain it, avoid adding load)
//   - cpu/gpu sensitivity: blend base with cap headroom (70/30)
func prioritizeNodes(ctx context.Context, args ExtenderArgs) ExtenderPriorityList {
	states := getNodeTwinStates(ctx)

	// Parse pod for workload profile hints
	podBytes, _ := json.Marshal(args.Pod)
	var pod PodSpec
	json.Unmarshal(podBytes, &pod)
	wpClass := podWorkloadClass(pod)
	cpuSensitivity := pod.Metadata.Annotations["joulie.io/cpu-sensitivity"]
	gpuSensitivity := pod.Metadata.Annotations["joulie.io/gpu-sensitivity"]

	var result ExtenderPriorityList
	if args.Nodes != nil {
		for _, node := range args.Nodes.Items {
			score := scoreNode(node.Metadata.Name, states, wpClass, cpuSensitivity, gpuSensitivity)
			result = append(result, HostPriority{Host: node.Metadata.Name, Score: score})
		}
	} else if args.NodeNames != nil {
		for _, nodeName := range *args.NodeNames {
			score := scoreNode(nodeName, states, wpClass, cpuSensitivity, gpuSensitivity)
			result = append(result, HostPriority{Host: nodeName, Score: score})
		}
	}
	return result
}

func scoreNode(nodeName string, states map[string]*joulie.NodeTwinStatus, wpClass, cpuSensitivity, gpuSensitivity string) int64 {
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

	// Draining: node is transitioning from performance → eco, avoid adding new load.
	if state.SchedulableClass == "draining" {
		score -= 20
	}

	// best-effort workloads: slight preference for eco nodes to preserve performance capacity.
	if wpClass == "best-effort" && state.SchedulableClass == "eco" {
		score += 5
	}

	// Cap sensitivity: blend base score with cap headroom for sensitive workloads.
	if cpuSensitivity == "high" {
		cpuHeadroom := state.EffectiveCapState.CPUPct
		score = score*0.7 + cpuHeadroom*0.3
	}
	if gpuSensitivity == "high" {
		gpuHeadroom := state.EffectiveCapState.GPUPct
		score = score*0.7 + gpuHeadroom*0.3
	}

	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return int64(score)
}

// getNodeTwinStates retrieves NodeTwinState objects, using a short-lived cache.
func getNodeTwinStates(ctx context.Context) map[string]*joulie.NodeTwinStatus {
	twinStateMu.RLock()
	if time.Since(lastCacheRefresh) < twinStateCacheTTL && twinStateCache != nil {
		defer twinStateMu.RUnlock()
		return twinStateCache
	}
	twinStateMu.RUnlock()

	twinStateMu.Lock()
	defer twinStateMu.Unlock()

	if dynClient == nil {
		return make(map[string]*joulie.NodeTwinStatus)
	}

	list, err := dynClient.Resource(nodeTwinGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Printf("failed to list NodeTwinState: %v", err)
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
		if v, ok := status["lastUpdated"].(string); ok {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				ts.LastUpdated = t
			}
		}
	}
	return nodeName, ts
}

// isTwinStale returns true if the twin data is too old to trust for scheduling.
func isTwinStale(ts *joulie.NodeTwinStatus) bool {
	if ts.LastUpdated.IsZero() {
		return false // no timestamp = operator hasn't set it yet; don't penalize
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


