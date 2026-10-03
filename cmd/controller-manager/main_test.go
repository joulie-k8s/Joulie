package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/controller/policy"
	"github.com/matbun/joulie/pkg/controller/twin"
	"github.com/matbun/joulie/pkg/hwinv"
	"github.com/matbun/joulie/pkg/kube"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s, err := kube.NewScheme(v1alpha1.AddToScheme)
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

// fakeReader stands in for the manager's cache: a controller-runtime fake
// client seeded with typed objects and carrying the same spec.nodeName index
// main registers on the real cache.
func fakeReader(t *testing.T, objs ...runtime.Object) kube.Reader {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithRuntimeObjects(objs...).
		WithIndex(&corev1.Pod{}, podNodeNameField, podNodeNameIndex).
		Build()
}

// fakeClients seeds the reader and both write clients from one object list,
// so a test's reads and writes see the same cluster. Core objects go to the
// clientset, joulie.io objects to the dynamic client, everything to the reader.
func fakeClients(t *testing.T, objs ...runtime.Object) (kube.Reader, *k8sfake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	var core, joulieObjs []runtime.Object
	for _, o := range objs {
		switch o.(type) {
		case *v1alpha1.NodeHardware, *v1alpha1.NodeTwin:
			joulieObjs = append(joulieObjs, o)
		default:
			core = append(core, o)
		}
	}
	return fakeReader(t, objs...),
		k8sfake.NewSimpleClientset(core...),
		dynamicfake.NewSimpleDynamicClient(testScheme(t), joulieObjs...)
}

func podWithRequiredPowerProfile(name, nodeName, profile string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns1",
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      "joulie.io/power-profile",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{profile},
									},
								},
							},
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestSanitizeName(t *testing.T) {
	t.Parallel()
	got := sanitizeName("Node_A/1")
	if got != "node-a-1" {
		t.Fatalf("sanitizeName mismatch: got=%q want=%q", got, "node-a-1")
	}
}

func TestProfileMapping(t *testing.T) {
	t.Parallel()
	if got := currentProfileOrDefault("performance"); got != "performance" {
		t.Fatalf("currentProfileOrDefault performance: got=%q", got)
	}
	if got := currentProfileOrDefault("weird"); got != "unknown" {
		t.Fatalf("currentProfileOrDefault unknown: got=%q", got)
	}
	if got := assignmentState("eco", true); got != "DrainingPerformance" {
		t.Fatalf("assignmentState draining: got=%q", got)
	}
}

func TestBuildStaticPlanPreservesOnePerformanceNodePerHardwareFamily(t *testing.T) {
	t.Parallel()
	nodes := []string{"h100-a", "h100-b", "mi300x-a", "cpu-a"}
	hw := map[string]policy.NodeHardwareInfo{
		"h100-a":   {GPUModel: "NVIDIA-H100", GPUCount: 8},
		"h100-b":   {GPUModel: "NVIDIA-H100", GPUCount: 8},
		"mi300x-a": {GPUModel: "AMD-Instinct-MI300X", GPUCount: 8},
		"cpu-a":    {CPUModel: "AMD-EPYC-9654"},
	}

	plan := policy.BuildStaticPlan(nodes, hw, nil, 5000, 120, 0.25)
	perfByNode := map[string]bool{}
	for _, a := range plan {
		perfByNode[a.NodeName] = a.Profile == profilePerformance
	}
	if !perfByNode["h100-a"] {
		t.Fatalf("expected one H100 node to remain performance")
	}
	if !perfByNode["mi300x-a"] {
		t.Fatalf("expected one MI300X node to remain performance")
	}
	if !perfByNode["cpu-a"] {
		t.Fatalf("expected one CPU-only family node to remain performance")
	}
}

func TestBuildQueueAwarePlanPreservesOnePerformanceNodePerHardwareFamily(t *testing.T) {
	t.Parallel()
	nodes := []string{"w7900-a", "w7900-b", "l40s-a", "cpu-a"}
	hw := map[string]policy.NodeHardwareInfo{
		"w7900-a": {GPUModel: "AMD-Radeon-PRO-W7900", GPUCount: 4},
		"w7900-b": {GPUModel: "AMD-Radeon-PRO-W7900", GPUCount: 4},
		"l40s-a":  {GPUModel: "NVIDIA-L40S", GPUCount: 8},
		"cpu-a":   {CPUModel: "Intel-Xeon-Gold-6530"},
	}

	plan := policy.BuildQueueAwarePlan(nodes, hw, nil, 5000, 120, 0.10, 0, 2, 10, 0)
	perfFamilies := map[string]bool{}
	for _, a := range plan {
		if a.Profile != profilePerformance {
			continue
		}
		perfFamilies[policy.NodeFamily(a.NodeName, hw)] = true
	}
	for _, family := range []string{"gpu:AMD-Radeon-PRO-W7900", "gpu:NVIDIA-L40S", "cpu:Intel-Xeon-Gold-6530"} {
		if !perfFamilies[family] {
			t.Fatalf("missing performance family %s in plan %#v", family, plan)
		}
	}
}

func TestComputeInventoryGPUCapUsesCatalogFallback(t *testing.T) {
	t.Parallel()
	cat, err := hwinv.LoadDefaultCatalog()
	if err != nil {
		t.Fatalf("LoadDefaultCatalog: %v", err)
	}
	abs, ok := computeInventoryGPUCap(profileEco, NodeHardware{
		GPURawModel: "NVIDIA-L40S",
		GPUCount:    4,
	}, cat, 100, 60)
	if !ok {
		t.Fatalf("expected catalog-based cap resolution")
	}
	if abs < 200 || abs > 350 {
		t.Fatalf("unexpected absolute cap: %v", abs)
	}
}

func TestBuildPlanAtTwoNodesAlternatesEcoNode(t *testing.T) {
	t.Parallel()
	nodes := []string{"node-a", "node-b"}
	interval := time.Minute
	perfCap := 5000.0
	ecoCap := 120.0

	planEven := policy.BuildRuleSwapPlanAt(nodes, interval, perfCap, ecoCap, time.Unix(120, 0)) // phase=0
	if len(planEven) != 2 {
		t.Fatalf("planEven len=%d", len(planEven))
	}
	if planEven[0].Profile != "eco" || planEven[1].Profile != "performance" {
		t.Fatalf("unexpected even plan profiles: %#v", planEven)
	}

	planOdd := policy.BuildRuleSwapPlanAt(nodes, interval, perfCap, ecoCap, time.Unix(180, 0)) // phase=1
	if len(planOdd) != 2 {
		t.Fatalf("planOdd len=%d", len(planOdd))
	}
	if planOdd[0].Profile != "performance" || planOdd[1].Profile != "eco" {
		t.Fatalf("unexpected odd plan profiles: %#v", planOdd)
	}
}

func TestBuildPlanAtSingleNodeAlternatesProfile(t *testing.T) {
	t.Parallel()
	nodes := []string{"node-a"}
	interval := time.Minute
	perfCap := 5000.0
	ecoCap := 120.0

	planEven := policy.BuildRuleSwapPlanAt(nodes, interval, perfCap, ecoCap, time.Unix(120, 0))
	if planEven[0].Profile != "eco" || planEven[0].CapWatts != ecoCap {
		t.Fatalf("unexpected single-node even plan: %#v", planEven[0])
	}

	planOdd := policy.BuildRuleSwapPlanAt(nodes, interval, perfCap, ecoCap, time.Unix(180, 0))
	if planOdd[0].Profile != "performance" || planOdd[0].CapWatts != perfCap {
		t.Fatalf("unexpected single-node odd plan: %#v", planOdd[0])
	}
}

func TestClassifyPodBySchedulingCornerCases(t *testing.T) {
	t.Parallel()
	mkRequired := func(op corev1.NodeSelectorOperator, values ...string) corev1.Pod {
		return corev1.Pod{
			Spec: corev1.PodSpec{
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{Key: "joulie.io/power-profile", Operator: op, Values: values},
									},
								},
							},
						},
					},
				},
			},
		}
	}
	mkRequiredOr := func(lhs, rhs []string) corev1.Pod {
		return corev1.Pod{
			Spec: corev1.PodSpec{
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "joulie.io/power-profile", Operator: corev1.NodeSelectorOpIn, Values: lhs}}},
								{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "joulie.io/power-profile", Operator: corev1.NodeSelectorOpIn, Values: rhs}}},
							},
						},
					},
				},
			},
		}
	}

	tests := []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{name: "no constraints is general", pod: corev1.Pod{Spec: corev1.PodSpec{}}, want: workloadClassGeneral},
		{name: "node selector performance only", pod: corev1.Pod{Spec: corev1.PodSpec{NodeSelector: map[string]string{"joulie.io/power-profile": "performance"}}}, want: workloadClassPerfOnly},
		{name: "node selector eco only", pod: corev1.Pod{Spec: corev1.PodSpec{NodeSelector: map[string]string{"joulie.io/power-profile": "eco"}}}, want: workloadClassEcoOnly},
		{name: "required affinity in performance", pod: mkRequired(corev1.NodeSelectorOpIn, "performance"), want: workloadClassPerfOnly},
		{name: "required affinity in eco", pod: mkRequired(corev1.NodeSelectorOpIn, "eco"), want: workloadClassEcoOnly},
		{name: "required affinity in both is general", pod: mkRequired(corev1.NodeSelectorOpIn, "eco", "performance"), want: workloadClassGeneral},
		{name: "or terms perf or eco treated as performance-only by conservative rule", pod: mkRequiredOr([]string{"performance"}, []string{"eco"}), want: workloadClassPerfOnly},
		{name: "notin eco means performance only", pod: mkRequired(corev1.NodeSelectorOpNotIn, "eco"), want: workloadClassPerfOnly},
		{name: "notin performance is general", pod: mkRequired(corev1.NodeSelectorOpNotIn, "performance"), want: workloadClassGeneral},
		{name: "does-not-exist on power-profile is general", pod: mkRequired(corev1.NodeSelectorOpDoesNotExist), want: workloadClassGeneral},
		{name: "gt operator on power-profile is general", pod: mkRequired(corev1.NodeSelectorOpGt, "1"), want: workloadClassGeneral},
		{name: "unknown node selector value is general", pod: corev1.Pod{Spec: corev1.PodSpec{NodeSelector: map[string]string{"joulie.io/power-profile": "ultra"}}}, want: workloadClassGeneral},
		{
			name: "contradicting node selector and affinity is performance-only per compat selector",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				NodeSelector: map[string]string{"joulie.io/power-profile": "performance"},
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "joulie.io/power-profile", Operator: corev1.NodeSelectorOpIn, Values: []string{"eco"}}}},
							},
						},
					},
				},
			}},
			want: workloadClassPerfOnly,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyPodByScheduling(&tt.pod); got != tt.want {
				t.Fatalf("classifyPodByScheduling=%q want=%q", got, tt.want)
			}
		})
	}
}

func TestRunningPerformanceSensitivePodCountOnNodeFiltersCorrectly(t *testing.T) {
	t.Parallel()
	reader := fakeReader(t,
		podWithRequiredPowerProfile("p1", "node-a", "performance"),
		podWithRequiredPowerProfile("p2", "node-a", "eco"),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p3", Namespace: "ns1"},
			Spec:       corev1.PodSpec{NodeName: "node-a"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}, // general, should not be counted
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p4", Namespace: "ns1"},
			Spec:       podWithRequiredPowerProfile("x", "node-a", "performance").Spec,
			Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
		}, // terminal, ignored
		podWithRequiredPowerProfile("p5", "node-b", "performance"), // other node, filtered by the index
	)

	count, err := runningPerformanceSensitivePodCountOnNode(context.Background(), reader, "node-a")
	if err != nil {
		t.Fatalf("runningPerformanceSensitivePodCountOnNode error: %v", err)
	}
	if count != 1 {
		t.Fatalf("count=%d want=1", count)
	}
}

func TestApplyDowngradeGuardsSetsDrainingWhenPerfPodsExist(t *testing.T) {
	t.Parallel()
	client := fakeReader(t,
		podWithRequiredPowerProfile("perf", "node-a", "performance"),
	)
	plan := []NodeAssignment{{
		NodeName:  "node-a",
		Profile:   "eco",
		CapWatts:  120,
		ManagedBy: "rule-swap-v1",
	}}
	current := map[string]string{"node-a": "performance"}

	applyDowngradeGuards(context.Background(), client, plan, current)

	if plan[0].Profile != "eco" || plan[0].State != "DrainingPerformance" || !plan[0].Draining {
		t.Fatalf("unexpected plan after guard: %#v", plan[0])
	}
}

func TestApplyDowngradeGuardsClearsDrainingWhenNoPerfPods(t *testing.T) {
	t.Parallel()
	client := fakeReader(t)
	plan := []NodeAssignment{{
		NodeName:  "node-a",
		Profile:   "eco",
		CapWatts:  120,
		ManagedBy: "rule-swap-v1",
		Draining:  true,
		State:     "DrainingPerformance",
	}}
	current := map[string]string{"node-a": "eco"}

	applyDowngradeGuards(context.Background(), client, plan, current)

	if plan[0].Profile != "eco" || plan[0].State != "ActiveEco" || plan[0].Draining {
		t.Fatalf("unexpected plan after guard clear: %#v", plan[0])
	}
}

func TestComputeDesiredLabelsMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		desired      string
		perfPods     int
		wantProfile  string
		wantDraining bool
	}{
		{name: "eco no perf pods", desired: "eco", perfPods: 0, wantProfile: "eco", wantDraining: false},
		{name: "eco with perf pods", desired: "eco", perfPods: 1, wantProfile: "eco", wantDraining: true},
		{name: "performance no perf pods", desired: "performance", perfPods: 0, wantProfile: "performance", wantDraining: false},
		{name: "performance with perf pods", desired: "performance", perfPods: 3, wantProfile: "performance", wantDraining: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotProfile, gotDraining := computeDesiredLabels(tt.desired, tt.perfPods)
			if gotProfile != tt.wantProfile || gotDraining != tt.wantDraining {
				t.Fatalf("computeDesiredLabels(%q,%d)=(%q,%v) want=(%q,%v)", tt.desired, tt.perfPods, gotProfile, gotDraining, tt.wantProfile, tt.wantDraining)
			}
		})
	}
}

func TestComputeDesiredLabelsIdempotent(t *testing.T) {
	t.Parallel()
	firstProfile, firstDraining := computeDesiredLabels("eco", 2)
	secondProfile, secondDraining := computeDesiredLabels(firstProfile, 2)
	if firstProfile != secondProfile || firstDraining != secondDraining {
		t.Fatalf("idempotency failed first=(%q,%v) second=(%q,%v)", firstProfile, firstDraining, secondProfile, secondDraining)
	}
}

func TestUpsertNodeTwinSpecCreateAndUpdate(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		nodeTwinGVR: "NodeTwinList",
	})
	ctx := context.Background()

	a := NodeAssignment{
		NodeName:  "node-a",
		Profile:   "eco",
		CapWatts:  120,
		ManagedBy: "rule-swap-v1",
	}
	if err := upsertNodeTwinSpec(ctx, dyn, a); err != nil {
		t.Fatalf("upsert create failed: %v", err)
	}

	got, err := dyn.Resource(nodeTwinGVR).Get(ctx, "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created NodeTwin: %v", err)
	}
	profile, _, _ := unstructured.NestedString(got.Object, "spec", "profile")
	if profile != "eco" {
		t.Fatalf("profile=%s want=eco", profile)
	}

	a.Profile = "performance"
	a.CapWatts = 5000
	if err := upsertNodeTwinSpec(ctx, dyn, a); err != nil {
		t.Fatalf("upsert update failed: %v", err)
	}
	got, err = dyn.Resource(nodeTwinGVR).Get(ctx, "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated NodeTwin: %v", err)
	}
	profile, _, _ = unstructured.NestedString(got.Object, "spec", "profile")
	if profile != "performance" {
		t.Fatalf("profile=%s want=performance", profile)
	}
}

func TestUpsertNodeLabels(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{}}},
	)
	a := NodeAssignment{NodeName: "node-a", Profile: "eco", Draining: true}
	if err := upsertNodeLabels(context.Background(), client, "joulie.io/power-profile", a); err != nil {
		t.Fatalf("upsertNodeLabels failed: %v", err)
	}
	n, err := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got := n.Labels["joulie.io/power-profile"]; got != "eco" {
		t.Fatalf("label=%s want=eco", got)
	}
	// Draining is NOT set as a node label; it is tracked in NodeTwinState.schedulableClass only.
	if _, hasDraining := n.Labels["joulie.io/draining"]; hasDraining {
		t.Fatalf("joulie.io/draining label should not be set on node")
	}
}

func TestUpsertNodeLabelsIsIdempotent(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
			Labels: map[string]string{
				"joulie.io/power-profile": "eco",
			},
		}},
	)
	a := NodeAssignment{NodeName: "node-a", Profile: "eco", Draining: false}
	before := len(client.Actions())
	if err := upsertNodeLabels(context.Background(), client, "joulie.io/power-profile", a); err != nil {
		t.Fatalf("upsertNodeLabels failed: %v", err)
	}
	after := len(client.Actions())
	// Idempotent call should do only a GET and skip PATCH.
	if got := after - before; got != 1 {
		t.Fatalf("action delta=%d want=1 (get only)", got)
	}
}

func TestReconcileCreatesProfilesAndLabels(t *testing.T) {
	t.Parallel()
	reader, client, dyn := fakeClients(t,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"joulie.io/managed": "true"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b", Labels: map[string]string{"joulie.io/managed": "true"}}},
	)
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	if err := reconcile(
		context.Background(),
		reader,
		client,
		dyn,
		selector,
		"joulie.io/reserved",
		"joulie.io/power-profile",
		time.Minute,
		5000,
		120,
		"rule_swap_v1",
		0.6,
		0.6,
		1,
		5,
		10,
		100,
		60,
		false,
		100,
		60,
		false,
		map[string]GPUModelCaps{},
		[]string{"joulie.io/gpu.product"},
	); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	ul, err := dyn.Resource(nodeTwinGVR).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list profiles: %v", err)
	}
	if len(ul.Items) != 2 {
		t.Fatalf("profiles len=%d want=2", len(ul.Items))
	}
	nodes, err := client.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	perf, eco := 0, 0
	for _, n := range nodes.Items {
		switch n.Labels["joulie.io/power-profile"] {
		case "performance":
			perf++
		case "eco":
			eco++
		}
		// Draining is NOT a node label; it is tracked in NodeTwinState.schedulableClass only.
		if _, hasDraining := n.Labels["joulie.io/draining"]; hasDraining {
			t.Fatalf("unexpected joulie.io/draining label on node %s", n.Name)
		}
	}
	if perf != 1 || eco != 1 {
		t.Fatalf("unexpected node labels perf=%d eco=%d", perf, eco)
	}
}

// The twin's TDP comes from the NodeHardware the reader serves, parsed typed.
func TestReconcileReadsNodeHardwareFromReader(t *testing.T) {
	t.Parallel()
	reader, client, dyn := fakeClients(t,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2-atos", Labels: map[string]string{"joulie.io/managed": "true"}}},
		nodeHardwareFromJSON(t, nodeHardware165W),
	)
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	if err := reconcile(
		context.Background(), reader, client, dyn, selector,
		"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
		5000, 120, "static_partition", 0.6, 0.6, 1, 5, 10,
		100, 60, false, 100, 60, false,
		map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"},
	); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	got, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), "n2-atos", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get NodeTwin: %v", err)
	}
	// The fake stores whole numbers as int64, like the API server does.
	v, found, err := unstructured.NestedFieldNoCopy(got.Object, "status", "powerMeasurement", "cpuTdpW")
	if err != nil || !found {
		t.Fatalf("cpuTdpW missing from status: found=%v err=%v obj=%v", found, err, got.Object["status"])
	}
	var tdp float64
	switch n := v.(type) {
	case int64:
		tdp = float64(n)
	case float64:
		tdp = n
	default:
		t.Fatalf("cpuTdpW has type %T", v)
	}
	if tdp != 660 {
		t.Fatalf("cpuTdpW=%v want=660 (4 sockets x 165 W from the cached NodeHardware)", tdp)
	}
}

func TestReconcileLoopStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	runs := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconcileLoop(ctx, time.Millisecond, func(context.Context) error {
			runs++
			if runs == 3 {
				cancel()
			}
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reconcileLoop did not return after cancel")
	}
	if runs != 3 {
		t.Fatalf("runs=%d want=3", runs)
	}
}

func TestPodTransformKeepsOnlyFSMFields(t *testing.T) {
	t.Parallel()
	now := metav1.Now()
	full := podWithRequiredPowerProfile("p1", "node-a", "performance")
	full.UID = "uid-1"
	full.ResourceVersion = "42"
	full.Labels = map[string]string{"app": "train"}
	full.Annotations = map[string]string{"joulie.io/workload-class": "performance"}
	full.DeletionTimestamp = &now
	full.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "train"}}
	full.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubelet"}}
	full.Finalizers = []string{"example.com/keep"}
	full.Spec.NodeSelector = map[string]string{"joulie.io/power-profile": "performance"}
	full.Spec.Containers = []corev1.Container{{Name: "main", Image: "img"}}
	full.Spec.Volumes = []corev1.Volume{{Name: "data"}}
	full.Spec.Tolerations = []corev1.Toleration{{Key: "gpu"}}
	full.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main"}}
	full.Status.PodIP = "10.0.0.1"
	full.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady}}

	out, err := podTransform(full)
	if err != nil {
		t.Fatalf("podTransform: %v", err)
	}
	pod, ok := out.(*corev1.Pod)
	if !ok {
		t.Fatalf("podTransform returned %T, want *corev1.Pod", out)
	}
	// Kept: identity, the fields fsm reads, and the index key.
	if pod.Name != "p1" || pod.Namespace != "ns1" || pod.UID != "uid-1" || pod.ResourceVersion != "42" {
		t.Fatalf("identity metadata lost: %#v", pod.ObjectMeta)
	}
	if pod.Labels["app"] != "train" || pod.Annotations["joulie.io/workload-class"] != "performance" {
		t.Fatalf("labels or annotations lost: %#v", pod.ObjectMeta)
	}
	if pod.DeletionTimestamp == nil || len(pod.OwnerReferences) != 1 {
		t.Fatalf("deletionTimestamp or ownerReferences lost: %#v", pod.ObjectMeta)
	}
	if pod.Spec.NodeName != "node-a" || pod.Spec.NodeSelector["joulie.io/power-profile"] != "performance" || pod.Spec.Affinity == nil {
		t.Fatalf("scheduling fields lost: %#v", pod.Spec)
	}
	if pod.Status.Phase != corev1.PodRunning {
		t.Fatalf("phase lost: %#v", pod.Status)
	}
	// Dropped: everything that makes a Pod large.
	if pod.Spec.Containers != nil || pod.Spec.Volumes != nil || pod.Spec.Tolerations != nil {
		t.Fatalf("spec not stripped: %#v", pod.Spec)
	}
	if pod.Status.ContainerStatuses != nil || pod.Status.PodIP != "" || pod.Status.Conditions != nil {
		t.Fatalf("status not stripped: %#v", pod.Status)
	}
	if pod.ManagedFields != nil || pod.Finalizers != nil {
		t.Fatalf("metadata not stripped: %#v", pod.ObjectMeta)
	}
	// The stripped Pod still classifies the way the FSM expects.
	if !isPerformanceSensitivePod(pod) {
		t.Fatal("stripped pod no longer classifies as performance-sensitive")
	}

	// Tombstones pass through untouched.
	tomb := toolscache.DeletedFinalStateUnknown{Key: "ns1/p1", Obj: pod}
	out, err = podTransform(tomb)
	if err != nil {
		t.Fatalf("podTransform(tombstone): %v", err)
	}
	if _, ok := out.(toolscache.DeletedFinalStateUnknown); !ok {
		t.Fatalf("tombstone was replaced by %T", out)
	}
}

func TestManagerCacheOptionsTransformPods(t *testing.T) {
	t.Parallel()
	opts := managerCacheOptions()
	if opts.DefaultTransform == nil {
		t.Fatal("DefaultTransform not set: managedFields would be cached for every kind")
	}
	found := false
	for obj, byObj := range opts.ByObject {
		if _, ok := obj.(*corev1.Pod); !ok {
			continue
		}
		found = true
		if byObj.Transform == nil {
			t.Fatal("Pod transform not registered")
		}
	}
	if !found {
		t.Fatal("no ByObject entry for Pods")
	}
}

func TestBuildStaticPlan(t *testing.T) {
	t.Parallel()
	nodes := []string{"node-a", "node-b", "node-c", "node-d", "node-e"}
	hw := map[string]policy.NodeHardwareInfo{
		"node-a": {CPUModel: "same-cpu"},
		"node-b": {CPUModel: "same-cpu"},
		"node-c": {CPUModel: "same-cpu"},
		"node-d": {CPUModel: "same-cpu"},
		"node-e": {CPUModel: "same-cpu"},
	}
	plan := policy.BuildStaticPlan(nodes, hw, nil, 5000, 120, 0.6)
	if len(plan) != len(nodes) {
		t.Fatalf("len(plan)=%d", len(plan))
	}
	perf, eco := 0, 0
	for _, a := range plan {
		if a.Profile == "performance" {
			perf++
		}
		if a.Profile == "eco" {
			eco++
		}
	}
	if perf != 3 || eco != 2 {
		t.Fatalf("unexpected mix perf=%d eco=%d", perf, eco)
	}
}

func TestBuildQueueAwarePlan(t *testing.T) {
	t.Parallel()
	nodes := []string{"node-a", "node-b", "node-c", "node-d", "node-e"}
	hw := map[string]policy.NodeHardwareInfo{
		"node-a": {CPUModel: "same-cpu"},
		"node-b": {CPUModel: "same-cpu"},
		"node-c": {CPUModel: "same-cpu"},
		"node-d": {CPUModel: "same-cpu"},
		"node-e": {CPUModel: "same-cpu"},
	}

	// Base 60% of 5 => 3 performance nodes at idle.
	plan := policy.BuildQueueAwarePlan(nodes, hw, nil, 5000, 120, 0.6, 1, 5, 10, 0)
	perf := 0
	for _, a := range plan {
		if a.Profile == "performance" {
			perf++
		}
	}
	if perf != 3 {
		t.Fatalf("idle perf nodes=%d want=3", perf)
	}

	// 40 performance-intent pods with perfPerHPNode=10 => 4 performance nodes.
	plan = policy.BuildQueueAwarePlan(nodes, hw, nil, 5000, 120, 0.6, 1, 5, 10, 40)
	perf = 0
	for _, a := range plan {
		if a.Profile == "performance" {
			perf++
		}
	}
	if perf != 4 {
		t.Fatalf("loaded perf nodes=%d want=4", perf)
	}
}

func TestBuildPlanByPolicyQueueAware(t *testing.T) {
	t.Parallel()
	client := fakeReader(t,
		podWithRequiredPowerProfile("perf-1", "node-a", "performance"),
		podWithRequiredPowerProfile("perf-2", "node-b", "performance"),
	)
	nodes := []string{"node-a", "node-b", "node-c"}
	hw := map[string]NodeHardware{
		"node-a": {CPUModel: "same-cpu"},
		"node-b": {CPUModel: "same-cpu"},
		"node-c": {CPUModel: "same-cpu"},
	}
	plan := buildPlanByPolicy(
		context.Background(),
		client,
		"queue_aware_v1",
		nodes,
		hw,
		nil,
		time.Minute,
		5000,
		120,
		0.6,
		0.34,
		1,
		3,
		2,
	)

	perf := 0
	for _, a := range plan {
		if a.Profile == "performance" {
			perf++
		}
	}
	// queueNeed=ceil(2/2)=1, base=round(3*0.34)=1 => 1 perf node.
	if perf != 1 {
		t.Fatalf("perf nodes=%d want=1 plan=%#v", perf, plan)
	}
}

func TestBuildPlanByPolicyUnknownFallsBackToStatic(t *testing.T) {
	t.Parallel()
	client := fakeReader(t)
	nodes := []string{"node-a", "node-b", "node-c", "node-d"}
	hw := map[string]NodeHardware{
		"node-a": {CPUModel: "same-cpu"},
		"node-b": {CPUModel: "same-cpu"},
		"node-c": {CPUModel: "same-cpu"},
		"node-d": {CPUModel: "same-cpu"},
	}
	plan := buildPlanByPolicy(
		context.Background(),
		client,
		"unknown_policy",
		nodes,
		hw,
		nil,
		time.Minute,
		5000,
		120,
		0.5,
		0.5,
		1,
		4,
		10,
	)

	perf := 0
	for _, a := range plan {
		if a.Profile == "performance" {
			perf++
		}
	}
	if perf != 2 {
		t.Fatalf("perf nodes=%d want=2 (50/50 static fallback)", perf)
	}
}

func TestComputeGPUIntentPctOnly(t *testing.T) {
	t.Parallel()
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n1",
			Labels: map[string]string{
				"joulie.io/gpu.product": "NVIDIA-L40S",
			},
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceName("nvidia.com/gpu"): resourceMustParse("4"),
			},
		},
	}
	intent := computeGPUIntentForNode(node, profileEco, 100, 60, false, nil, []string{"joulie.io/gpu.product"})
	if intent == nil || intent.CapPctOfMax == nil || *intent.CapPctOfMax != 60 {
		t.Fatalf("unexpected intent: %#v", intent)
	}
	if intent.CapWattsPerGPU != nil {
		t.Fatalf("absolute cap should be unset when writeAbsolute=false")
	}
}

func TestComputeGPUIntentAbsoluteWhenModelExists(t *testing.T) {
	t.Parallel()
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n1",
			Labels: map[string]string{
				"joulie.io/gpu.product": "NVIDIA-L40S",
			},
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceName("nvidia.com/gpu"): resourceMustParse("8"),
			},
		},
	}
	intent := computeGPUIntentForNode(
		node,
		profileEco,
		100,
		60,
		true,
		map[string]GPUModelCaps{"NVIDIA-L40S": {MinCapWatts: 200, MaxCapWatts: 350}},
		[]string{"joulie.io/gpu.product"},
	)
	if intent == nil || intent.CapWattsPerGPU == nil {
		t.Fatalf("expected absolute gpu cap intent, got %#v", intent)
	}
	if got := *intent.CapWattsPerGPU; got != 210 {
		t.Fatalf("absolute cap got=%v want=210", got)
	}
}

func TestComputeGPUIntentFallsBackWhenModelMissing(t *testing.T) {
	t.Parallel()
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n1",
			Labels: map[string]string{
				"joulie.io/gpu.product": "Unknown",
			},
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceName("amd.com/gpu"): resourceMustParse("2"),
			},
		},
	}
	intent := computeGPUIntentForNode(node, profilePerformance, 100, 60, true, map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"})
	if intent == nil || intent.CapPctOfMax == nil || *intent.CapPctOfMax != 100 {
		t.Fatalf("expected pct fallback, got %#v", intent)
	}
	if intent.CapWattsPerGPU != nil {
		t.Fatalf("did not expect absolute cap when model is missing")
	}
}

func TestComputeGPUIntentReturnsNilWithoutAllocatableGPU(t *testing.T) {
	t.Parallel()
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cpu-only"},
		Status:     corev1.NodeStatus{Allocatable: corev1.ResourceList{}},
	}
	intent := computeGPUIntentForNode(node, profilePerformance, 100, 60, false, nil, []string{"joulie.io/gpu.product"})
	if intent != nil {
		t.Fatalf("expected nil intent for non-GPU node, got %#v", intent)
	}
}

func TestComputeGPUIntentReturnsNilWhenPctDisabled(t *testing.T) {
	t.Parallel()
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceName("nvidia.com/gpu"): resourceMustParse("1"),
			},
		},
	}
	intent := computeGPUIntentForNode(node, profileEco, 100, 0, false, nil, []string{"joulie.io/gpu.product"})
	if intent != nil {
		t.Fatalf("expected nil intent when eco gpu pct is disabled, got %#v", intent)
	}
}

func TestParseGPUModelCapsInvalidJSON(t *testing.T) {
	t.Parallel()
	got := parseGPUModelCaps("{")
	if len(got) != 0 {
		t.Fatalf("expected empty map on invalid json, got=%#v", got)
	}
}

func TestDiscoverGPUCountCustomSuffix(t *testing.T) {
	t.Parallel()
	node := corev1.Node{
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceName("vendor.example/gpu"): resourceMustParse("3"),
			},
		},
	}
	if got := discoverGPUCount(node); got != 3 {
		t.Fatalf("discoverGPUCount=%d want=3", got)
	}
}

func resourceMustParse(v string) resource.Quantity {
	return resource.MustParse(v)
}

// nodeHardwareFromJSON decodes a NodeHardware exactly as the API server sends
// it (whole numbers arrive as int64) and converts it to the typed struct the
// cache hands out, the same path a real informer takes.
func nodeHardwareFromJSON(t *testing.T, raw string) *v1alpha1.NodeHardware {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON([]byte(raw)); err != nil {
		t.Fatalf("decode NodeHardware: %v", err)
	}
	nh := &v1alpha1.NodeHardware{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, nh); err != nil {
		t.Fatalf("convert NodeHardware: %v", err)
	}
	return nh
}

// n2-atos: the agent reports 165 W per socket as a whole number.
const nodeHardware165W = `{
  "apiVersion": "joulie.io/v1alpha1",
  "kind": "NodeHardware",
  "metadata": {"name": "n2-atos"},
  "spec": {"nodeName": "n2-atos"},
  "status": {
    "cpu": {"vendor": "GenuineIntel", "sockets": 4, "totalCores": 192,
            "capRange": {"type": "package", "maxWattsPerSocket": 165, "minWattsPerSocket": 90}},
    "gpu": {"present": true, "count": 8, "capRangePerGpu": {"maxWatts": 700}}
  }
}`

func TestFetchNodeHardwareKeepsWholeNumberWatts(t *testing.T) {
	t.Parallel()
	reader := fakeReader(t, nodeHardwareFromJSON(t, nodeHardware165W))

	hw := fetchNodeHardware(context.Background(), reader, "n2-atos")
	if hw.CPU.CapRange.MaxWattsPerSocket != 165 {
		t.Fatalf("maxWattsPerSocket=%v want=165 (int64 from the API must be accepted)", hw.CPU.CapRange.MaxWattsPerSocket)
	}
	if hw.CPU.CapRange.MinWattsPerSocket != 90 {
		t.Fatalf("minWattsPerSocket=%v want=90", hw.CPU.CapRange.MinWattsPerSocket)
	}
	if hw.GPU.CapRange.MaxWatts != 700 {
		t.Fatalf("gpu maxWatts=%v want=700", hw.GPU.CapRange.MaxWatts)
	}
}

func TestFetchNodeHardwareGivesTwinRealTDP(t *testing.T) {
	t.Parallel()
	reader := fakeReader(t, nodeHardwareFromJSON(t, nodeHardware165W))

	hw := fetchNodeHardware(context.Background(), reader, "n2-atos")
	out := twin.Compute(twin.Input{
		NodeName:           "n2-atos",
		Hardware:           hw,
		Profile:            "performance",
		MeasuredNodePowerW: 330,
	})
	if out.PowerMeasurement.CpuTdpW != 660 {
		t.Fatalf("cpuTdpW=%v want=660 (4 sockets x 165 W)", out.PowerMeasurement.CpuTdpW)
	}
}

func TestFetchNodeHardwareUsesSanitizedObjectName(t *testing.T) {
	t.Parallel()
	raw := strings.ReplaceAll(nodeHardware165W, `"nodeName": "n2-atos"`, `"nodeName": "n2.atos"`)
	reader := fakeReader(t, nodeHardwareFromJSON(t, raw))

	hw := fetchNodeHardware(context.Background(), reader, "n2.atos")
	if hw.CPU.CapRange.MaxWattsPerSocket != 165 {
		t.Fatalf("maxWattsPerSocket=%v want=165 (object is stored under the sanitized name)", hw.CPU.CapRange.MaxWattsPerSocket)
	}
}

func TestUpsertNodeTwinStatusUsesSanitizedObjectName(t *testing.T) {
	t.Parallel()
	_, _, dyn := fakeClients(t)

	if err := upsertNodeTwinSpec(context.Background(), dyn, NodeAssignment{NodeName: "n2.atos", Profile: "performance"}); err != nil {
		t.Fatalf("upsert spec: %v", err)
	}
	if err := upsertNodeTwinStatus(context.Background(), dyn, "n2.atos", joulieNodeTwinStatusForTest()); err != nil {
		t.Fatalf("upsert status: %v", err)
	}

	got, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), "n2-atos", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("spec and status must land on the same object: %v", err)
	}
	class, _, _ := unstructured.NestedString(got.Object, "status", "schedulableClass")
	if class != "performance" {
		t.Fatalf("schedulableClass=%q want=performance", class)
	}
	if list, err := dyn.Resource(nodeTwinGVR).List(context.Background(), metav1.ListOptions{}); err != nil {
		t.Fatalf("list: %v", err)
	} else if len(list.Items) != 1 {
		t.Fatalf("got %d NodeTwins, want 1 (no duplicate under the raw node name)", len(list.Items))
	}
}

func TestParseNodeHardwareReadsCapRange(t *testing.T) {
	t.Parallel()
	nh := parseNodeHardware(nodeHardwareFromJSON(t, nodeHardware165W))

	if !nh.CPUCapKnown || nh.CPUCapMaxWatts != 165 || nh.CPUCapMinWatts != 90 {
		t.Fatalf("cpu cap known=%v max=%v min=%v want true/165/90", nh.CPUCapKnown, nh.CPUCapMaxWatts, nh.CPUCapMinWatts)
	}
	if !nh.GPUCapKnown || nh.GPUCapMaxWatts != 700 {
		t.Fatalf("gpu cap known=%v max=%v want true/700", nh.GPUCapKnown, nh.GPUCapMaxWatts)
	}
}

func joulieNodeTwinStatusForTest() joulie.NodeTwinStatus {
	return joulie.NodeTwinStatus{
		SchedulableClass:            "performance",
		PredictedPowerHeadroomScore: 50,
		LastUpdated:                 time.Now().UTC(),
	}
}

func TestImplausibleNodePowerCatchesJoulesCounter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		measuredW, tdpW float64
		want            bool
	}{
		{"joules counter read as watts", 6_540_020, 660, true},
		{"normal load", 330, 660, false},
		{"brief overshoot above TDP", 700, 660, false},
		{"unknown TDP", 6_540_020, 0, false},
		{"no reading", 0, 660, false},
	}
	for _, tc := range tests {
		if got := implausibleNodePower(tc.measuredW, tc.tdpW); got != tc.want {
			t.Fatalf("%s: implausibleNodePower(%v, %v)=%v want=%v", tc.name, tc.measuredW, tc.tdpW, got, tc.want)
		}
	}
}

func TestNodeTwinStatusMapNeverContainsControlStatus(t *testing.T) {
	t.Parallel()
	status := joulieNodeTwinStatusForTest()
	// Even if a ControlStatus is present in memory, the controller manager must never
	// write it: that subtree belongs to the agent.
	status.ControlStatus = &joulie.ControlStatus{CPU: &joulie.ControlResult{Backend: "rapl", Result: "applied"}}
	m := nodeTwinStatusToMap(status)
	if _, ok := m["controlStatus"]; ok {
		t.Fatalf("controller manager status payload contains controlStatus: %v", m)
	}
}

func TestUpsertNodeTwinSpecSkipsUpdateWhenUnchanged(t *testing.T) {
	t.Parallel()
	_, _, dyn := fakeClients(t)
	a := NodeAssignment{NodeName: "node-same", Profile: "eco", CapWatts: 120, ManagedBy: "static-partition-v1"}
	updates := func() int {
		n := 0
		for _, act := range dyn.Actions() {
			if act.GetVerb() == "update" {
				n++
			}
		}
		return n
	}
	for i := 0; i < 3; i++ {
		if err := upsertNodeTwinSpec(context.Background(), dyn, a); err != nil {
			t.Fatal(err)
		}
	}
	if got := updates(); got != 0 {
		t.Fatalf("updates=%d want=0 (unchanged spec must not be rewritten)", got)
	}
	a.Profile = "performance"
	if err := upsertNodeTwinSpec(context.Background(), dyn, a); err != nil {
		t.Fatal(err)
	}
	if got := updates(); got != 1 {
		t.Fatalf("updates=%d want=1 (changed spec must be written)", got)
	}
}

// reconcileStaticPartition runs one static_partition reconcile with the given
// fraction of performance nodes and returns the profile label of every node.
func reconcileStaticPartition(t *testing.T, hpFrac float64, objs ...runtime.Object) map[string]string {
	t.Helper()
	reader, client, dyn := fakeClients(t, objs...)
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	if err := reconcile(
		context.Background(), reader, client, dyn, selector,
		"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
		5000, 120, "static_partition", hpFrac, hpFrac, 1, 5, 10,
		100, 60, false, 100, 60, false,
		map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"},
	); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	nodes, err := client.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	out := make(map[string]string, len(nodes.Items))
	for _, n := range nodes.Items {
		out[n.Name] = n.Labels["joulie.io/power-profile"]
	}
	return out
}

func managedNode(name, profile string) *corev1.Node {
	l := map[string]string{"joulie.io/managed": "true"}
	if profile != "" {
		l["joulie.io/power-profile"] = profile
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
}

// nodeHardwareFor builds the NodeHardware the agent publishes for a node with
// the given CPU and, if gpuCount > 0, GPU model strings as discovery reports
// them.
func nodeHardwareFor(t *testing.T, node, cpuRaw string, totalCores int, gpuRaw string, gpuCount int) *v1alpha1.NodeHardware {
	t.Helper()
	return nodeHardwareFromJSON(t, fmt.Sprintf(`{
  "apiVersion": "joulie.io/v1alpha1", "kind": "NodeHardware",
  "metadata": {"name": %q}, "spec": {"nodeName": %q},
  "status": {
    "cpu": {"vendor": "AuthenticAMD", "rawModel": %q, "sockets": 2, "totalCores": %d},
    "gpu": {"present": %t, "rawModel": %q, "count": %d}
  }
}`, node, node, cpuRaw, totalCores, gpuCount > 0, gpuRaw, gpuCount))
}

// STATIC_HP_FRAC is a share of every hardware family, not of the cluster in
// some order. The old density order ranked a CPU-only 2x EPYC 9965 node
// (1776 x 768) far above an 8x H100 NVL node, so the CPU family took both of
// its nodes into performance and the GPU family got what was left: 2 of 2
// against 3 of 8. Half of each family is 1 of 2 and 4 of 8.
func TestReconcileSplitsPerformanceSlotsPerHardwareFamily(t *testing.T) {
	t.Parallel()
	objs := []runtime.Object{}
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("cpu-9965-%d", i)
		objs = append(objs, managedNode(name, ""),
			nodeHardwareFor(t, name, "AMD EPYC 9965 192-Core Processor", 768, "", 0))
	}
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("gpu-h100-%d", i)
		objs = append(objs, managedNode(name, ""),
			nodeHardwareFor(t, name, "AMD EPYC 9654 96-Core Processor", 384, "NVIDIA H100 NVL", 8))
	}
	profiles := reconcileStaticPartition(t, 0.5, objs...)

	perf := map[string]int{}
	for name, p := range profiles {
		if p == "performance" {
			perf[strings.SplitN(name, "-", 2)[0]]++
		}
	}
	if perf["cpu"] != 1 || perf["gpu"] != 4 {
		t.Fatalf("performance nodes per family cpu=%d gpu=%d, want 1 of 2 and 4 of 8: %v", perf["cpu"], perf["gpu"], profiles)
	}
}

// At constant demand a reconcile must not move the performance profile from
// one node to an identical one: every move throttles whatever runs on the
// node that leaves. The old code picked the first names, so n0 and n1 took
// over from n2 and n3 on every reconcile after a relabel.
func TestReconcileKeepsCurrentPerformanceNodes(t *testing.T) {
	t.Parallel()
	profiles := reconcileStaticPartition(t, 0.5,
		managedNode("n0", "eco"), managedNode("n1", "eco"),
		managedNode("n2", "performance"), managedNode("n3", "performance"),
	)
	want := map[string]string{"n0": "eco", "n1": "eco", "n2": "performance", "n3": "performance"}
	for name, p := range want {
		if profiles[name] != p {
			t.Fatalf("node %s profile=%q want=%q (all: %v)", name, profiles[name], p, profiles)
		}
	}
}

// hardwareDensityScore left the CRD. Helm never upgrades crds/, so a cluster
// can keep the old schema and the number the previous release stored; the
// merge patch must carry null to delete it rather than leave a stale value.
func TestNodeTwinStatusMapClearsHardwareDensityScore(t *testing.T) {
	m := nodeTwinStatusToMap(joulie.NodeTwinStatus{})
	v, ok := m["hardwareDensityScore"]
	if !ok || v != nil {
		t.Fatalf("hardwareDensityScore = %v (present=%v), want an explicit null", v, ok)
	}
}

// The family gauges describe the latest plan only. A family that left the
// cluster, or a selector that now matches nothing, must not keep its old
// series, or a sum over families double counts.
func TestRecordFamilySplitReplacesThePreviousSplit(t *testing.T) {
	hw := map[string]NodeHardware{
		"c0": {CPUModel: "AMD_EPYC_9654"}, "c1": {CPUModel: "AMD_EPYC_9654"},
		"g0": {GPUModel: "NVIDIA_H100_NVL", GPUCount: 8},
	}
	recordFamilySplit([]NodeAssignment{
		{NodeName: "c0", Profile: profilePerformance},
		{NodeName: "c1", Profile: profileEco},
		{NodeName: "g0", Profile: profilePerformance},
	}, hw)
	if got := testutil.ToFloat64(policyFamilyNodes.WithLabelValues("cpu:AMD_EPYC_9654")); got != 2 {
		t.Fatalf("cpu family nodes = %v, want 2", got)
	}
	if got := testutil.ToFloat64(policyFamilyPerformanceNodes.WithLabelValues("cpu:AMD_EPYC_9654")); got != 1 {
		t.Fatalf("cpu family performance nodes = %v, want 1", got)
	}

	recordFamilySplit([]NodeAssignment{{NodeName: "c0", Profile: profilePerformance}}, hw)
	if n := testutil.CollectAndCount(policyFamilyNodes); n != 1 {
		t.Fatalf("family gauge has %d series after the GPU family left, want 1", n)
	}
	recordFamilySplit(nil, nil)
	if n := testutil.CollectAndCount(policyFamilyNodes) + testutil.CollectAndCount(policyFamilyPerformanceNodes); n != 0 {
		t.Fatalf("family gauges keep %d series with no eligible node, want 0", n)
	}
}

// A node the policy moves out of performance while performance pods still run
// on it is draining: the scheduler sends it no new performance pod, and the
// running ones keep performance caps until they finish (the FSM's
// DrainingPerformance, and what the simulator does). The old code computed the
// caps from the planned eco profile before the guard ran, so the running pods
// were capped at once. Every cap the spec can carry is checked: CPU percent,
// GPU percent, and the CPU watts written on GPU nodes and in absolute mode.
func TestDrainingNodeKeepsPerformanceCaps(t *testing.T) {
	const perfW, ecoW = 5000.0, 120.0
	gpus := corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")}
	type check struct {
		path []string
		want float64
	}
	for _, tc := range []struct {
		name        string
		withGPU     bool
		absolute    bool
		capRangeMax float64 // NodeHardware maxWattsPerSocket; 0 publishes none
		checks      []check
	}{
		{name: "cpu node", checks: []check{{[]string{"spec", "cpu", "packagePowerCapPctOfMax"}, 100}}},
		{name: "gpu node", withGPU: true, checks: []check{
			{[]string{"spec", "gpu", "powerCap", "capPctOfMax"}, 100},
			{[]string{"spec", "cpu", "packagePowerCapWatts"}, perfW},
		}},
		{name: "cpu node, absolute, known cap range", absolute: true, capRangeMax: 300,
			checks: []check{{[]string{"spec", "cpu", "packagePowerCapWatts"}, 300}}},
		{name: "cpu node, absolute, unknown hardware", absolute: true,
			checks: []check{{[]string{"spec", "cpu", "packagePowerCapWatts"}, perfW}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both nodes are in performance and the fraction is 0, so the
			// family keeps one: n0 by name, and n1 must go to eco.
			n0, n1 := managedNode("n0", "performance"), managedNode("n1", "performance")
			if tc.withGPU {
				n0.Status.Allocatable, n1.Status.Allocatable = gpus, gpus
			}
			perfPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "train", Namespace: "default",
					Annotations: map[string]string{"joulie.io/workload-class": "performance"}},
				Spec:   corev1.PodSpec{NodeName: "n1"},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}
			objs := []runtime.Object{n0, n1, perfPod}
			if tc.capRangeMax > 0 {
				for _, n := range []string{"n0", "n1"} {
					objs = append(objs, nodeHardwareFromJSON(t, fmt.Sprintf(`{
  "apiVersion": "joulie.io/v1alpha1", "kind": "NodeHardware",
  "metadata": {"name": %q}, "spec": {"nodeName": %q},
  "status": {"cpu": {"sockets": 1, "totalCores": 8, "controlAvailable": true,
    "capRange": {"type": "package", "minWattsPerSocket": 50, "maxWattsPerSocket": %v}}}
}`, n, n, tc.capRangeMax)))
				}
			}
			reader, client, dyn := fakeClients(t, objs...)
			selector, err := labels.Parse("joulie.io/managed=true")
			if err != nil {
				t.Fatalf("parse selector: %v", err)
			}
			if err := reconcile(
				context.Background(), reader, client, dyn, selector,
				"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
				perfW, ecoW, "static_partition", 0, 0, 1, 5, 10,
				100, 60, tc.absolute, 100, 60, false,
				map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"},
			); err != nil {
				t.Fatalf("reconcile error: %v", err)
			}

			twin, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), "n1", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get NodeTwin n1: %v", err)
			}
			draining, _, _ := unstructured.NestedBool(twin.Object, "spec", "scheduling", "draining")
			if !draining {
				t.Fatalf("n1 runs a performance pod and should be draining: spec=%v", twin.Object["spec"])
			}
			for _, c := range tc.checks {
				v, found, _ := unstructured.NestedFieldNoCopy(twin.Object, c.path...)
				got, ok := v.(float64)
				if i, isInt := v.(int64); isInt {
					got, ok = float64(i), true
				}
				if !found || !ok || got != c.want {
					t.Fatalf("%v = %v while draining, want the performance cap %v (spec=%v)", c.path, v, c.want, twin.Object["spec"])
				}
			}
		})
	}
}
