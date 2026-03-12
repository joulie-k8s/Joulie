package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

var profileGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodepowerprofiles"}

const (
	powerProfileLabelKey  = "joulie.io/power-profile"
	drainingLabelKey      = "joulie.io/draining"
	profilePerformance    = "performance"
	profileEco            = "eco"
	workloadClassUnknown  = "unknown"
	workloadClassGeneral  = "general"
	workloadClassEcoOnly  = "eco-only"
	workloadClassPerfOnly = "performance-only"
)

type NodeAssignment struct {
	NodeName       string
	Profile        string
	CapWatts       float64
	CPUCapPctOfMax *float64
	GPU            *GPUCapIntent
	ManagedBy      string
	SourceProfile  string
	SourceDrain    bool
	Draining       bool
	State          string
}

type GPUCapIntent struct {
	Scope          string
	CapWattsPerGPU *float64
	CapPctOfMax    *float64
}

type GPUModelCaps struct {
	MinCapWatts float64 `json:"minCapWatts"`
	MaxCapWatts float64 `json:"maxCapWatts"`
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
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[joulie-operator] ")

	reconcileEvery := durationEnv("RECONCILE_INTERVAL", time.Minute)
	metricsAddr := envOrDefault("METRICS_ADDR", ":8081")
	selector := envOrDefault("NODE_SELECTOR", "node-role.kubernetes.io/worker")
	reservedLabel := envOrDefault("RESERVED_LABEL_KEY", "joulie.io/reserved")
	profileLabel := envOrDefault("POWER_PROFILE_LABEL", "joulie.io/power-profile")
	drainingLabel := envOrDefault("DRAINING_LABEL", drainingLabelKey)
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

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := reconcile(
			ctx, kube, dyn, parsedSelector, reservedLabel, profileLabel, drainingLabel, reconcileEvery,
			perfCap, ecoCap, policyType, staticHPFrac, queueHPBaseFrac, queueHPMin, queueHPMax, queuePerfPerHPNode,
			cpuPerfCapPct, cpuEcoCapPct, cpuWriteAbsolute,
			gpuPerfCapPct, gpuEcoCapPct, gpuWriteAbsolute, gpuModelCaps, gpuProductLabelKeys,
		); err != nil {
			log.Printf("reconcile failed: %v", err)
		}
		cancel()
		time.Sleep(reconcileEvery)
	}
}

func startMetricsServer(addr string) {
	prometheus.MustRegister(operatorNodeState, operatorNodeProfileLabel, operatorStateTransitions)
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
	drainingLabel string,
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
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}

	eligible := make([]string, 0)
	nodesByName := make(map[string]string, len(nodes.Items))
	nodeObjs := make(map[string]*corev1.Node, len(nodes.Items))
	drainingByName := make(map[string]bool, len(nodes.Items))
	for _, n := range nodes.Items {
		node := n
		nodeObjs[n.Name] = &node
		prof, draining := normalizeNodeLabels(n.Labels[profileLabel], n.Labels[drainingLabel])
		nodesByName[n.Name] = prof
		drainingByName[n.Name] = draining
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

	plan := buildPlanByPolicy(ctx, kube, policyType, eligible, interval, perfCap, ecoCap, staticHPFrac, queueHPBaseFrac, queueHPMin, queueHPMax, queuePerfPerHPNode)
	for i := range plan {
		plan[i].SourceProfile = currentProfileOrDefault(nodesByName[plan[i].NodeName])
		plan[i].SourceDrain = drainingByName[plan[i].NodeName]
		plan[i].Draining = false
		plan[i].State = assignmentState(plan[i].Profile, plan[i].Draining)
		if cpuWriteAbsolute {
			plan[i].CPUCapPctOfMax = nil
		} else {
			pct := cpuEcoCapPct
			if plan[i].Profile == profilePerformance {
				pct = cpuPerfCapPct
			}
			plan[i].CPUCapPctOfMax = floatPtr(pct)
		}
		if n := nodeObjs[plan[i].NodeName]; n != nil {
			plan[i].GPU = computeGPUIntentForNode(*n, plan[i].Profile, gpuPerfCapPct, gpuEcoCapPct, gpuWriteAbsolute, gpuModelCaps, gpuProductLabelKeys)
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
	for _, a := range plan {
		if err := upsertNodeProfile(ctx, dyn, a); err != nil {
			return err
		}
		if err := upsertNodeLabels(ctx, kube, profileLabel, drainingLabel, a); err != nil {
			return err
		}
		recordNodeStateMetrics(a.NodeName, a.State)
		recordNodeProfileLabelMetrics(a.NodeName, a.Profile)
		recordTransitionMetrics(a)
	}

	log.Printf("assigned profiles policy=%s interval=%s nodes=%d plan=%s", policyType, interval, len(eligible), summarizePlan(plan))
	return nil
}

func currentProfileOrDefault(in string) string {
	if in == profilePerformance || in == profileEco {
		return in
	}
	return "unknown"
}

func assignmentState(profile string, draining bool) string {
	if draining {
		return "DrainingPerformance"
	}
	switch profile {
	case profilePerformance:
		return "ActivePerformance"
	case profileEco:
		return "ActiveEco"
	default:
		return "Unknown"
	}
}

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
	for i := range plan {
		a := &plan[i]
		if a.Profile != profileEco {
			a.Profile, a.Draining = computeDesiredLabels(a.Profile, 0)
			a.State = assignmentState(a.Profile, a.Draining)
			continue
		}
		current := currentProfiles[a.NodeName]
		if current != profilePerformance && current != profileEco {
			// New/unknown node state; mark draining based on current pod mix.
		}
		count, err := runningPerformanceSensitivePodCountOnNode(ctx, kube, a.NodeName)
		if err != nil {
			log.Printf("warning: cannot classify running pods on node=%s: %v", a.NodeName, err)
			continue
		}
		a.Profile, a.Draining = computeDesiredLabels(a.Profile, count)
		a.State = assignmentState(a.Profile, a.Draining)
		if a.Draining {
			operatorStateTransitions.WithLabelValues(a.NodeName, "ActivePerformance", "ActiveEco", "deferred").Inc()
			log.Printf("transition guarded node=%s desired=eco draining=true reason=running-performance-sensitive-pods count=%d", a.NodeName, count)
		}
	}
}

// computeDesiredLabels maps desired profile + running performance-sensitive pods
// to node labels.
// Rules:
// - desired performance => (performance, draining=false)
// - desired eco + perfPods>0 => (eco, draining=true)
// - desired eco + perfPods=0 => (eco, draining=false)
func computeDesiredLabels(desiredProfile string, perfPods int) (string, bool) {
	switch currentProfileOrDefault(desiredProfile) {
	case profilePerformance:
		return profilePerformance, false
	case profileEco:
		return profileEco, perfPods > 0
	default:
		return "unknown", false
	}
}

func runningPerformanceSensitivePodCountOnNode(
	ctx context.Context,
	kube kubernetes.Interface,
	nodeName string,
) (int, error) {
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		if !isPerformanceSensitivePod(&p) {
			continue
		}
		count++
	}
	return count, nil
}

func buildPlan(nodes []string, interval time.Duration, perfCap, ecoCap float64) []NodeAssignment {
	return buildPlanAt(nodes, interval, perfCap, ecoCap, time.Now())
}

func buildPlanByPolicy(
	ctx context.Context,
	kube kubernetes.Interface,
	policyType string,
	nodes []string,
	interval time.Duration,
	perfCap, ecoCap, staticHPFrac, queueHPBaseFrac float64,
	queueHPMin, queueHPMax, queuePerfPerHPNode int,
) []NodeAssignment {
	switch policyType {
	case "static_partition", "":
		return buildStaticPlan(nodes, perfCap, ecoCap, staticHPFrac)
	case "queue_aware_v1":
		perfIntentPods, err := runningPerformanceSensitivePodCountAllNodes(ctx, kube)
		if err != nil {
			log.Printf("warning: cannot classify running pods for queue_aware_v1: %v; falling back to static fraction", err)
			return buildStaticPlan(nodes, perfCap, ecoCap, queueHPBaseFrac)
		}
		return buildQueueAwarePlan(nodes, perfCap, ecoCap, queueHPBaseFrac, queueHPMin, queueHPMax, queuePerfPerHPNode, perfIntentPods)
	case "rule_swap_v1":
		return buildPlan(nodes, interval, perfCap, ecoCap)
	default:
		log.Printf("warning: unknown POLICY_TYPE=%q, falling back to static_partition", policyType)
		return buildStaticPlan(nodes, perfCap, ecoCap, staticHPFrac)
	}
}

func buildPlanAt(nodes []string, interval time.Duration, perfCap, ecoCap float64, now time.Time) []NodeAssignment {
	phase := int((now.Unix() / int64(interval.Seconds())) % 2)
	plan := make([]NodeAssignment, 0, len(nodes))
	for i, n := range nodes {
		profile := "performance"
		cap := perfCap
		if i == 0 && (phase%2 == 0) {
			profile = "eco"
			cap = ecoCap
		}
		if i == 1 && (phase%2 == 1) {
			profile = "eco"
			cap = ecoCap
		}
		plan = append(plan, NodeAssignment{NodeName: n, Profile: profile, CapWatts: cap, ManagedBy: "rule-swap-v1"})
	}
	if len(nodes) == 1 {
		if phase%2 == 0 {
			plan[0].Profile = "eco"
			plan[0].CapWatts = ecoCap
		} else {
			plan[0].Profile = "performance"
			plan[0].CapWatts = perfCap
		}
	}
	return plan
}

func buildStaticPlan(nodes []string, perfCap, ecoCap, hpFrac float64) []NodeAssignment {
	n := len(nodes)
	if n == 0 {
		return nil
	}
	if hpFrac < 0 {
		hpFrac = 0
	}
	if hpFrac > 1 {
		hpFrac = 1
	}
	hpCount := int(math.Round(float64(n) * hpFrac))
	if hpCount < 0 {
		hpCount = 0
	}
	if hpCount > n {
		hpCount = n
	}

	plan := make([]NodeAssignment, 0, n)
	for i, node := range nodes {
		profile := "eco"
		cap := ecoCap
		if i < hpCount {
			profile = "performance"
			cap = perfCap
		}
		plan = append(plan, NodeAssignment{
			NodeName:  node,
			Profile:   profile,
			CapWatts:  cap,
			ManagedBy: "static-partition-v1",
		})
	}
	return plan
}

func buildQueueAwarePlan(nodes []string, perfCap, ecoCap, hpBaseFrac float64, hpMin, hpMax, perfPerHPNode, perfIntentPods int) []NodeAssignment {
	n := len(nodes)
	if n == 0 {
		return nil
	}
	if hpBaseFrac < 0 {
		hpBaseFrac = 0
	}
	if hpBaseFrac > 1 {
		hpBaseFrac = 1
	}
	if hpMin < 0 {
		hpMin = 0
	}
	if hpMax <= 0 {
		hpMax = n
	}
	if hpMax < hpMin {
		hpMax = hpMin
	}
	if perfPerHPNode <= 0 {
		perfPerHPNode = 1
	}
	baseCount := int(math.Round(float64(n) * hpBaseFrac))
	queueNeed := int(math.Ceil(float64(perfIntentPods) / float64(perfPerHPNode)))
	hpCount := baseCount
	if queueNeed > hpCount {
		hpCount = queueNeed
	}
	if hpCount < hpMin {
		hpCount = hpMin
	}
	if hpCount > hpMax {
		hpCount = hpMax
	}
	if hpCount > n {
		hpCount = n
	}
	if hpCount < 0 {
		hpCount = 0
	}

	plan := make([]NodeAssignment, 0, n)
	for i, node := range nodes {
		profile := "eco"
		cap := ecoCap
		if i < hpCount {
			profile = "performance"
			cap = perfCap
		}
		plan = append(plan, NodeAssignment{
			NodeName:  node,
			Profile:   profile,
			CapWatts:  cap,
			ManagedBy: "queue-aware-v1",
		})
	}
	return plan
}

func runningPerformanceSensitivePodCountAllNodes(
	ctx context.Context,
	kube kubernetes.Interface,
) (int, error) {
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		if !isPerformanceSensitivePod(&p) {
			continue
		}
		count++
	}
	return count, nil
}

func isPerformanceSensitivePod(p *corev1.Pod) bool {
	return podExcludesEco(p)
}

func classifyPodByScheduling(p *corev1.Pod) string {
	if podExcludesEco(p) {
		return workloadClassPerfOnly
	}
	if podIsEcoOnly(p) {
		return workloadClassEcoOnly
	}
	return workloadClassGeneral
}

func podExcludesEco(p *corev1.Pod) bool {
	// Compatibility path: explicit selector on profile=performance.
	if strings.TrimSpace(p.Spec.NodeSelector[powerProfileLabelKey]) == profilePerformance {
		return true
	}
	required := p.Spec.Affinity
	if required == nil || required.NodeAffinity == nil || required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key != powerProfileLabelKey {
				continue
			}
			switch expr.Operator {
			case corev1.NodeSelectorOpNotIn:
				if containsString(expr.Values, profileEco) {
					return true
				}
			case corev1.NodeSelectorOpIn:
				if len(expr.Values) > 0 && !containsString(expr.Values, profileEco) {
					return true
				}
			}
		}
	}
	return false
}

func podIsEcoOnly(p *corev1.Pod) bool {
	if strings.TrimSpace(p.Spec.NodeSelector[powerProfileLabelKey]) == profileEco {
		return true
	}
	required := p.Spec.Affinity
	if required == nil || required.NodeAffinity == nil || required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key != powerProfileLabelKey {
				continue
			}
			if expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 && !containsString(expr.Values, profilePerformance) && containsString(expr.Values, profileEco) {
				return true
			}
		}
	}
	return false
}

func containsString(in []string, v string) bool {
	for _, x := range in {
		if strings.TrimSpace(x) == v {
			return true
		}
	}
	return false
}

func normalizeNodeLabels(profileVal, drainingVal string) (string, bool) {
	prof := currentProfileOrDefault(profileVal)
	draining := strings.EqualFold(strings.TrimSpace(drainingVal), "true")
	if prof == "unknown" {
		return prof, false
	}
	return prof, draining
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

func computeGPUIntentForNode(
	node corev1.Node,
	profile string,
	gpuPerfCapPct float64,
	gpuEcoCapPct float64,
	gpuWriteAbsolute bool,
	gpuModelCaps map[string]GPUModelCaps,
	gpuProductLabelKeys []string,
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
	product := resolveGPUProduct(node.Labels, gpuProductLabelKeys)
	if product == "" {
		return intent
	}
	caps, ok := gpuModelCaps[product]
	if !ok || caps.MaxCapWatts <= 0 {
		return intent
	}
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
	return intent
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

func upsertNodeProfile(ctx context.Context, dyn dynamic.Interface, a NodeAssignment) error {
	name := fmt.Sprintf("node-%s", sanitizeName(a.NodeName))
	spec := map[string]any{
		"nodeName": a.NodeName,
		"profile":  a.Profile,
		"policy": map[string]any{
			"name": a.ManagedBy,
		},
	}
	cpu := map[string]any{}
	if a.CPUCapPctOfMax != nil {
		cpu["packagePowerCapPctOfMax"] = *a.CPUCapPctOfMax
	} else {
		cpu["packagePowerCapWatts"] = a.CapWatts
	}
	spec["cpu"] = cpu
	if a.GPU != nil {
		powerCap := map[string]any{
			"scope": "perGpu",
		}
		if a.GPU.CapWattsPerGPU != nil {
			powerCap["capWattsPerGpu"] = *a.GPU.CapWattsPerGPU
		}
		if a.GPU.CapPctOfMax != nil {
			powerCap["capPctOfMax"] = *a.GPU.CapPctOfMax
		}
		spec["gpu"] = map[string]any{
			"powerCap": powerCap,
		}
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "joulie.io/v1alpha1",
		"kind":       "NodePowerProfile",
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]any{
				"joulie.io/managed-by": "operator",
			},
		},
		"spec": spec,
	}}

	res := dyn.Resource(profileGVR)
	existing, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get NodePowerProfile %s: %w", name, err)
		}
		_, err := res.Create(ctx, obj, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create NodePowerProfile %s: %w", name, err)
		}
		return nil
	}

	existing.Object["spec"] = obj.Object["spec"]
	if _, err := res.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update NodePowerProfile %s: %w", name, err)
	}
	return nil
}

func upsertNodeLabels(ctx context.Context, kube kubernetes.Interface, profileLabel, drainingLabel string, a NodeAssignment) error {
	labelValue := a.Profile
	drainingValue := "false"
	if a.Draining {
		drainingValue = "true"
	}
	node, err := kube.CoreV1().Nodes().Get(ctx, a.NodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s before patch: %w", a.NodeName, err)
	}
	currentProfile := strings.TrimSpace(node.Labels[profileLabel])
	currentDraining := strings.TrimSpace(node.Labels[drainingLabel])
	if currentProfile == labelValue && currentDraining == drainingValue {
		return nil
	}
	patch := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				profileLabel:  labelValue,
				drainingLabel: drainingValue,
			},
		},
	}
	rawPatch, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal node label patch for %s: %w", a.NodeName, err)
	}
	if _, err := kube.CoreV1().Nodes().Patch(ctx, a.NodeName, types.MergePatchType, rawPatch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch node %s labels %s=%s %s=%s: %w", a.NodeName, profileLabel, labelValue, drainingLabel, drainingValue, err)
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
