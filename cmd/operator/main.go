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
	powerProfileLabelKey   = "joulie.io/power-profile"
	profilePerformance     = "performance"
	profileEco             = "eco"
	profileDrainingPerf    = "draining-performance"
	profileMaskPerformance = 1
	profileMaskEco         = 2
	profileMaskBoth        = profileMaskPerformance | profileMaskEco
	workloadClassUnknown   = "unknown"
	workloadClassGeneral   = "general"
	workloadClassEcoOnly   = "eco-only"
	workloadClassPerfOnly  = "performance-only"
)

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
	perfCap := floatEnv("PERFORMANCE_CAP_WATTS", 5000)
	ecoCap := floatEnv("ECO_CAP_WATTS", 120)
	policyType := strings.ToLower(envOrDefault("POLICY_TYPE", "static_partition"))
	staticHPFrac := floatEnv("STATIC_HP_FRAC", 0.50)
	queueHPBaseFrac := floatEnv("QUEUE_HP_BASE_FRAC", 0.60)
	queueHPMin := intEnv("QUEUE_HP_MIN", 1)
	queueHPMax := intEnv("QUEUE_HP_MAX", 1000000)
	queuePerfPerHPNode := intEnv("QUEUE_PERF_PER_HP_NODE", 10)

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
		if err := reconcile(ctx, kube, dyn, parsedSelector, reservedLabel, profileLabel, reconcileEvery, perfCap, ecoCap, policyType, staticHPFrac, queueHPBaseFrac, queueHPMin, queueHPMax, queuePerfPerHPNode); err != nil {
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
	interval time.Duration,
	perfCap float64,
	ecoCap float64,
	policyType string,
	staticHPFrac float64,
	queueHPBaseFrac float64,
	queueHPMin int,
	queueHPMax int,
	queuePerfPerHPNode int,
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

	plan := buildPlanByPolicy(ctx, kube, policyType, eligible, interval, perfCap, ecoCap, staticHPFrac, queueHPBaseFrac, queueHPMin, queueHPMax, queuePerfPerHPNode)
	for i := range plan {
		plan[i].SourceProfile = currentProfileOrDefault(nodesByName[plan[i].NodeName])
		plan[i].LabelProfile = plan[i].Profile
		plan[i].State = profileToState(plan[i].Profile)
	}
	applyDowngradeGuards(ctx, kube, plan, nodesByName, perfCap, ecoCap)
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

	log.Printf("assigned profiles policy=%s interval=%s nodes=%d plan=%s", policyType, interval, len(eligible), summarizePlan(plan))
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
	kube kubernetes.Interface,
	plan []NodeAssignment,
	currentProfiles map[string]string,
	perfCap float64,
	ecoCap float64,
) {
	for i := range plan {
		a := &plan[i]

		current := currentProfiles[a.NodeName]
		// If a node is already in draining, prioritize drain completion:
		// as soon as performance-only workloads on that node are gone, move to eco.
		if current == "draining-performance" {
			count, err := runningPerformanceSensitivePodCountOnNode(ctx, kube, a.NodeName)
			if err != nil {
				log.Printf("warning: cannot classify running pods on node=%s: %v", a.NodeName, err)
				continue
			}
			if count == 0 {
				a.Profile = "eco"
				a.CapWatts = ecoCap
				a.ManagedBy = "rule-swap-v1-drain-complete"
				a.State = "ActiveEco"
				a.LabelProfile = "eco"
				log.Printf("drain completed node=%s transition=DrainingPerformance->ActiveEco", a.NodeName)
				continue
			}

			// Still draining: keep high cap and keep node unattractive for new performance pods.
			a.Profile = "performance"
			a.CapWatts = perfCap
			a.ManagedBy = "rule-swap-v1-draining"
			a.State = "DrainingPerformance"
			a.LabelProfile = "draining-performance"
			operatorStateTransitions.WithLabelValues(a.NodeName, "DrainingPerformance", "ActiveEco", "deferred").Inc()
			log.Printf("transition deferred node=%s from=DrainingPerformance to=ActiveEco reason=running-performance-sensitive-pods count=%d", a.NodeName, count)
			continue
		}

		if a.Profile != "eco" {
			continue
		}
		if current != "performance" {
			continue
		}
		count, err := runningPerformanceSensitivePodCountOnNode(ctx, kube, a.NodeName)
		if err != nil {
			log.Printf("warning: cannot classify running pods on node=%s: %v", a.NodeName, err)
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
		log.Printf("transition deferred node=%s from=performance to=eco reason=running-performance-sensitive-pods count=%d", a.NodeName, count)
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
	class := classifyPodByScheduling(p)
	return class == workloadClassPerfOnly || class == workloadClassUnknown
}

func classifyPodByScheduling(p *corev1.Pod) string {
	mask, ok := resolvePodPowerProfileMask(&p.Spec)
	if !ok || mask == 0 {
		return workloadClassUnknown
	}
	// No explicit power-profile constraint resolves to both masks,
	// which is treated as implicit flexible/general demand.
	switch mask {
	case profileMaskPerformance:
		return workloadClassPerfOnly
	case profileMaskEco:
		return workloadClassEcoOnly
	case profileMaskBoth:
		return workloadClassGeneral
	default:
		return workloadClassUnknown
	}
}

func resolvePodPowerProfileMask(spec *corev1.PodSpec) (int, bool) {
	baseMask, ok := maskFromNodeSelector(spec.NodeSelector)
	if !ok {
		return 0, false
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil || spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return baseMask, true
	}
	required := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(required.NodeSelectorTerms) == 0 {
		return 0, false
	}

	overallMask := 0
	for _, term := range required.NodeSelectorTerms {
		termMask, ok := maskForNodeSelectorTerm(term, baseMask)
		if !ok {
			return 0, false
		}
		overallMask |= termMask
	}
	return overallMask, true
}

func maskFromNodeSelector(selector map[string]string) (int, bool) {
	mask := profileMaskBoth
	v, ok := selector[powerProfileLabelKey]
	if !ok {
		return mask, true
	}
	valMask, ok := maskFromProfileValue(v)
	if !ok {
		return 0, false
	}
	return mask & valMask, true
}

func maskForNodeSelectorTerm(term corev1.NodeSelectorTerm, baseMask int) (int, bool) {
	mask := baseMask
	for _, expr := range term.MatchExpressions {
		if expr.Key != powerProfileLabelKey {
			continue
		}
		switch expr.Operator {
		case corev1.NodeSelectorOpIn:
			inMask, ok := maskFromValues(expr.Values)
			if !ok {
				return 0, false
			}
			mask &= inMask
		case corev1.NodeSelectorOpNotIn:
			notInMask, ok := maskFromValues(expr.Values)
			if !ok {
				return 0, false
			}
			mask &^= notInMask
		case corev1.NodeSelectorOpExists:
			// no change to profile domain
		case corev1.NodeSelectorOpDoesNotExist:
			mask = 0
		case corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
			return 0, false
		default:
			return 0, false
		}
	}
	return mask, true
}

func maskFromValues(values []string) (int, bool) {
	mask := 0
	for _, v := range values {
		profileMask, ok := maskFromProfileValue(v)
		if !ok {
			continue
		}
		mask |= profileMask
	}
	// values were specified but none are recognized power-profile classes.
	if len(values) > 0 && mask == 0 {
		return 0, true
	}
	return mask, true
}

func maskFromProfileValue(v string) (int, bool) {
	switch strings.TrimSpace(v) {
	case profilePerformance, profileDrainingPerf:
		return profileMaskPerformance, true
	case profileEco:
		return profileMaskEco, true
	default:
		return 0, false
	}
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

func upsertNodeProfileLabel(ctx context.Context, kube kubernetes.Interface, profileLabel string, a NodeAssignment) error {
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

func intEnv(key string, def int) int {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
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
