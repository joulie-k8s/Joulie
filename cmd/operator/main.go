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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

type NodeAssignment struct {
	NodeName      string
	Profile       string
	CapWatts      float64
	ManagedBy     string
	SourceProfile string
	LabelProfile  string
	State         string
}

var (
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
	intentLabel := envOrDefault("WORKLOAD_INTENT_LABEL", "joulie.io/workload-intent-class")
	perfIntentValue := envOrDefault("PERFORMANCE_INTENT_VALUE", "performance")
	perfCap := floatEnv("PERFORMANCE_CAP_WATTS", 5000)
	ecoCap := floatEnv("ECO_CAP_WATTS", 120)

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
		if err := reconcile(ctx, kube, dyn, parsedSelector, reservedLabel, profileLabel, intentLabel, perfIntentValue, reconcileEvery, perfCap, ecoCap); err != nil {
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
	kube *kubernetes.Clientset,
	dyn dynamic.Interface,
	selector labels.Selector,
	reservedLabel string,
	profileLabel string,
	intentLabel string,
	perfIntentValue string,
	interval time.Duration,
	perfCap float64,
	ecoCap float64,
) error {
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}

	eligible := make([]string, 0)
	nodesByName := make(map[string]string, len(nodes.Items))
	for _, n := range nodes.Items {
		nodesByName[n.Name] = n.Labels[profileLabel]
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

	plan := buildPlan(eligible, interval, perfCap, ecoCap)
	for i := range plan {
		plan[i].SourceProfile = currentProfileOrDefault(nodesByName[plan[i].NodeName])
		plan[i].LabelProfile = plan[i].Profile
		plan[i].State = profileToState(plan[i].Profile)
	}
	applyDowngradeGuards(ctx, kube, plan, nodesByName, intentLabel, perfIntentValue, perfCap)
	for _, a := range plan {
		if err := upsertNodeProfile(ctx, dyn, a); err != nil {
			return err
		}
		if err := upsertNodeProfileLabel(ctx, kube, profileLabel, a); err != nil {
			return err
		}
		recordNodeStateMetrics(a.NodeName, a.State)
		recordNodeProfileLabelMetrics(a.NodeName, a.LabelProfile)
		recordTransitionMetrics(a)
	}

	log.Printf("assigned profiles interval=%s nodes=%d plan=%s", interval, len(eligible), summarizePlan(plan))
	return nil
}

func currentProfileOrDefault(in string) string {
	if in == "performance" || in == "eco" || in == "draining-performance" {
		return in
	}
	return "unknown"
}

func profileToState(profile string) string {
	switch profile {
	case "performance":
		return "ActivePerformance"
	case "eco":
		return "ActiveEco"
	case "draining-performance":
		return "DrainingPerformance"
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
	draining := 0.0
	if profile == "performance" {
		perf = 1
	}
	if profile == "eco" {
		eco = 1
	}
	if profile == "draining-performance" {
		draining = 1
	}
	operatorNodeProfileLabel.WithLabelValues(nodeName, "performance").Set(perf)
	operatorNodeProfileLabel.WithLabelValues(nodeName, "eco").Set(eco)
	operatorNodeProfileLabel.WithLabelValues(nodeName, "draining-performance").Set(draining)
}

func recordTransitionMetrics(a NodeAssignment) {
	fromState := profileToState(a.SourceProfile)
	toState := a.State
	if toState == "" {
		toState = profileToState(a.Profile)
	}
	if fromState == "Unknown" || toState == "Unknown" || fromState == toState {
		return
	}
	operatorStateTransitions.WithLabelValues(a.NodeName, fromState, toState, "applied").Inc()
}

func applyDowngradeGuards(
	ctx context.Context,
	kube *kubernetes.Clientset,
	plan []NodeAssignment,
	currentProfiles map[string]string,
	intentLabel string,
	perfIntentValue string,
	perfCap float64,
) {
	for i := range plan {
		a := &plan[i]
		if a.Profile != "eco" {
			continue
		}
		if currentProfiles[a.NodeName] != "performance" {
			continue
		}
		count, err := runningIntentPodCountOnNode(ctx, kube, a.NodeName, intentLabel, perfIntentValue)
		if err != nil {
			log.Printf("warning: cannot check workload intents on node=%s: %v", a.NodeName, err)
			continue
		}
		if count == 0 {
			continue
		}

		// Keep node at performance until performance-intent workloads drain.
		a.Profile = "performance"
		a.CapWatts = perfCap
		a.ManagedBy = "rule-swap-v1-draining"
		a.State = "DrainingPerformance"
		a.LabelProfile = "draining-performance"
		operatorStateTransitions.WithLabelValues(a.NodeName, "ActivePerformance", "ActiveEco", "deferred").Inc()
		log.Printf("transition deferred node=%s from=performance to=eco reason=running-intent-pods label=%s value=%s count=%d", a.NodeName, intentLabel, perfIntentValue, count)
	}
}

func runningIntentPodCountOnNode(
	ctx context.Context,
	kube *kubernetes.Clientset,
	nodeName string,
	intentLabel string,
	intentValue string,
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
		if p.Labels[intentLabel] != intentValue {
			continue
		}
		count++
	}
	return count, nil
}

func buildPlan(nodes []string, interval time.Duration, perfCap, ecoCap float64) []NodeAssignment {
	phase := int((time.Now().Unix() / int64(interval.Seconds())) % 2)
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

func summarizePlan(plan []NodeAssignment) string {
	parts := make([]string, 0, len(plan))
	for _, p := range plan {
		parts = append(parts, fmt.Sprintf("%s=%s(%.1fW)", p.NodeName, p.Profile, p.CapWatts))
	}
	return strings.Join(parts, ",")
}

func upsertNodeProfile(ctx context.Context, dyn dynamic.Interface, a NodeAssignment) error {
	name := fmt.Sprintf("node-%s", sanitizeName(a.NodeName))
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "joulie.io/v1alpha1",
		"kind":       "NodePowerProfile",
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]any{
				"joulie.io/managed-by": "operator",
			},
		},
		"spec": map[string]any{
			"nodeName": a.NodeName,
			"profile":  a.Profile,
			"cpu": map[string]any{
				"packagePowerCapWatts": a.CapWatts,
			},
			"policy": map[string]any{
				"name": a.ManagedBy,
			},
		},
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

func upsertNodeProfileLabel(ctx context.Context, kube *kubernetes.Clientset, profileLabel string, a NodeAssignment) error {
	labelValue := a.Profile
	if a.LabelProfile != "" {
		labelValue = a.LabelProfile
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

func envOrDefault(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}
