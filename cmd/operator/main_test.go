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

func TestRunningIntentPodCountOnNodeFiltersCorrectly(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p1",
				Namespace: "ns1",
				Labels:    map[string]string{"joulie.io/workload-intent-class": "performance"},
			},
			Spec:   corev1.PodSpec{NodeName: "node-a"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p2",
				Namespace: "ns1",
				Labels:    map[string]string{"joulie.io/workload-intent-class": "eco"},
			},
			Spec:   corev1.PodSpec{NodeName: "node-a"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p3",
				Namespace: "ns1",
				Labels:    map[string]string{"joulie.io/workload-intent-class": "performance"},
			},
			Spec:   corev1.PodSpec{NodeName: "node-a"},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
		},
	)

	count, err := runningIntentPodCountOnNode(context.Background(), client, "node-a", "joulie.io/workload-intent-class", "performance")
	if err != nil {
		t.Fatalf("runningIntentPodCountOnNode error: %v", err)
	}
	if count != 1 {
		t.Fatalf("count=%d want=1", count)
	}
}

func TestApplyDowngradeGuardsDefersPerformanceToEcoWhenPerfPodsExist(t *testing.T) {
	t.Parallel()
	client := k8sfake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "perf",
				Namespace: "ns1",
				Labels:    map[string]string{"joulie.io/workload-intent-class": "performance"},
			},
			Spec:   corev1.PodSpec{NodeName: "node-a"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	)
	plan := []NodeAssignment{{
		NodeName:  "node-a",
		Profile:   "eco",
		CapWatts:  120,
		ManagedBy: "rule-swap-v1",
	}}
	current := map[string]string{"node-a": "performance"}

	applyDowngradeGuards(context.Background(), client, plan, current, "joulie.io/workload-intent-class", "performance", 5000, 120)

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

	applyDowngradeGuards(context.Background(), client, plan, current, "joulie.io/workload-intent-class", "performance", 5000, 120)

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
		"joulie.io/workload-intent-class",
		"performance",
		time.Minute,
		5000,
		120,
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
