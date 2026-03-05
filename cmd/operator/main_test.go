package main

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

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
	if got := profileToState("draining-performance"); got != "DrainingPerformance" {
		t.Fatalf("profileToState draining: got=%q", got)
	}
}

func TestBuildPlanAtTwoNodesAlternatesEcoNode(t *testing.T) {
	t.Parallel()
	nodes := []string{"node-a", "node-b"}
	interval := time.Minute
	perfCap := 5000.0
	ecoCap := 120.0

	planEven := buildPlanAt(nodes, interval, perfCap, ecoCap, time.Unix(120, 0)) // phase=0
	if len(planEven) != 2 {
		t.Fatalf("planEven len=%d", len(planEven))
	}
	if planEven[0].Profile != "eco" || planEven[1].Profile != "performance" {
		t.Fatalf("unexpected even plan profiles: %#v", planEven)
	}

	planOdd := buildPlanAt(nodes, interval, perfCap, ecoCap, time.Unix(180, 0)) // phase=1
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

	planEven := buildPlanAt(nodes, interval, perfCap, ecoCap, time.Unix(120, 0))
	if planEven[0].Profile != "eco" || planEven[0].CapWatts != ecoCap {
		t.Fatalf("unexpected single-node even plan: %#v", planEven[0])
	}

	planOdd := buildPlanAt(nodes, interval, perfCap, ecoCap, time.Unix(180, 0))
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
		{name: "node selector draining treated as performance-only", pod: corev1.Pod{Spec: corev1.PodSpec{NodeSelector: map[string]string{"joulie.io/power-profile": "draining-performance"}}}, want: workloadClassPerfOnly},
		{name: "required affinity in performance", pod: mkRequired(corev1.NodeSelectorOpIn, "performance"), want: workloadClassPerfOnly},
		{name: "required affinity in eco", pod: mkRequired(corev1.NodeSelectorOpIn, "eco"), want: workloadClassEcoOnly},
		{name: "required affinity in both is general", pod: mkRequired(corev1.NodeSelectorOpIn, "eco", "performance"), want: workloadClassGeneral},
		{name: "or terms perf or eco is general", pod: mkRequiredOr([]string{"performance"}, []string{"eco"}), want: workloadClassGeneral},
		{name: "notin eco means performance only", pod: mkRequired(corev1.NodeSelectorOpNotIn, "eco"), want: workloadClassPerfOnly},
		{name: "notin performance means eco only", pod: mkRequired(corev1.NodeSelectorOpNotIn, "performance"), want: workloadClassEcoOnly},
		{name: "does-not-exist on power-profile is unknown", pod: mkRequired(corev1.NodeSelectorOpDoesNotExist), want: workloadClassUnknown},
		{name: "gt operator on power-profile is unknown", pod: mkRequired(corev1.NodeSelectorOpGt, "1"), want: workloadClassUnknown},
		{name: "unknown node selector value is unknown", pod: corev1.Pod{Spec: corev1.PodSpec{NodeSelector: map[string]string{"joulie.io/power-profile": "ultra"}}}, want: workloadClassUnknown},
		{
			name: "contradicting node selector and affinity is unknown",
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
			want: workloadClassUnknown,
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
	client := k8sfake.NewSimpleClientset(
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
	)

	count, err := runningPerformanceSensitivePodCountOnNode(context.Background(), client, "node-a")
	if err != nil {
		t.Fatalf("runningPerformanceSensitivePodCountOnNode error: %v", err)
	}
	if count != 1 {
		t.Fatalf("count=%d want=1", count)
	}
}

func TestApplyDowngradeGuardsDefersPerformanceToEcoWhenPerfPodsExist(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewSimpleClientset(
		podWithRequiredPowerProfile("perf", "node-a", "performance"),
	)
	plan := []NodeAssignment{{
		NodeName:  "node-a",
		Profile:   "eco",
		CapWatts:  120,
		ManagedBy: "rule-swap-v1",
	}}
	current := map[string]string{"node-a": "performance"}

	applyDowngradeGuards(context.Background(), client, plan, current, 5000, 120)

	if plan[0].Profile != "performance" || plan[0].State != "DrainingPerformance" || plan[0].LabelProfile != "draining-performance" {
		t.Fatalf("unexpected plan after defer: %#v", plan[0])
	}
}

func TestApplyDowngradeGuardsDrainCompleteTransitionsToEco(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewSimpleClientset()
	plan := []NodeAssignment{{
		NodeName:     "node-a",
		Profile:      "performance",
		CapWatts:     5000,
		ManagedBy:    "rule-swap-v1",
		LabelProfile: "draining-performance",
		State:        "DrainingPerformance",
	}}
	current := map[string]string{"node-a": "draining-performance"}

	applyDowngradeGuards(context.Background(), client, plan, current, 5000, 120)

	if plan[0].Profile != "eco" || plan[0].State != "ActiveEco" || plan[0].LabelProfile != "eco" || plan[0].CapWatts != 120 {
		t.Fatalf("unexpected plan after drain completion: %#v", plan[0])
	}
}

func TestUpsertNodeProfileCreateAndUpdate(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		profileGVR: "NodePowerProfileList",
	})
	ctx := context.Background()

	a := NodeAssignment{
		NodeName:  "node-a",
		Profile:   "eco",
		CapWatts:  120,
		ManagedBy: "rule-swap-v1",
	}
	if err := upsertNodeProfile(ctx, dyn, a); err != nil {
		t.Fatalf("upsert create failed: %v", err)
	}

	got, err := dyn.Resource(profileGVR).Get(ctx, "node-node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created profile: %v", err)
	}
	profile, _, _ := unstructured.NestedString(got.Object, "spec", "profile")
	if profile != "eco" {
		t.Fatalf("profile=%s want=eco", profile)
	}

	a.Profile = "performance"
	a.CapWatts = 5000
	if err := upsertNodeProfile(ctx, dyn, a); err != nil {
		t.Fatalf("upsert update failed: %v", err)
	}
	got, err = dyn.Resource(profileGVR).Get(ctx, "node-node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated profile: %v", err)
	}
	profile, _, _ = unstructured.NestedString(got.Object, "spec", "profile")
	if profile != "performance" {
		t.Fatalf("profile=%s want=performance", profile)
	}
}

func TestUpsertNodeProfileLabel(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{}}},
	)
	a := NodeAssignment{NodeName: "node-a", Profile: "performance", LabelProfile: "draining-performance"}
	if err := upsertNodeProfileLabel(context.Background(), client, "joulie.io/power-profile", a); err != nil {
		t.Fatalf("upsertNodeProfileLabel failed: %v", err)
	}
	n, err := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got := n.Labels["joulie.io/power-profile"]; got != "draining-performance" {
		t.Fatalf("label=%s want=draining-performance", got)
	}
}

func TestReconcileCreatesProfilesAndLabels(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"joulie.io/managed": "true"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b", Labels: map[string]string{"joulie.io/managed": "true"}}},
	)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		profileGVR: "NodePowerProfileList",
	})
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	if err := reconcile(
		context.Background(),
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
	); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	ul, err := dyn.Resource(profileGVR).List(context.Background(), metav1.ListOptions{})
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
	}
	if perf != 1 || eco != 1 {
		t.Fatalf("unexpected node labels perf=%d eco=%d", perf, eco)
	}
}

func TestBuildStaticPlan(t *testing.T) {
	t.Parallel()
	nodes := []string{"node-a", "node-b", "node-c", "node-d", "node-e"}
	plan := buildStaticPlan(nodes, 5000, 120, 0.6)
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

	// Base 60% of 5 => 3 performance nodes at idle.
	plan := buildQueueAwarePlan(nodes, 5000, 120, 0.6, 1, 5, 10, 0)
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
	plan = buildQueueAwarePlan(nodes, 5000, 120, 0.6, 1, 5, 10, 40)
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
	client := k8sfake.NewSimpleClientset(
		podWithRequiredPowerProfile("perf-1", "node-a", "performance"),
		podWithRequiredPowerProfile("perf-2", "node-b", "performance"),
	)
	nodes := []string{"node-a", "node-b", "node-c"}
	plan := buildPlanByPolicy(
		context.Background(),
		client,
		"queue_aware_v1",
		nodes,
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
	client := k8sfake.NewSimpleClientset()
	nodes := []string{"node-a", "node-b", "node-c", "node-d"}
	plan := buildPlanByPolicy(
		context.Background(),
		client,
		"unknown_policy",
		nodes,
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
