package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/api/v1alpha1"
	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// storedAgentCondition reads the AgentReporting condition the controller
// manager wrote to the NodeTwin of node, through the typed status, as the
// scheduler extender and kubectl read it; nil when there is none.
func storedAgentCondition(t *testing.T, dyn *dynamicfake.FakeDynamicClient, node string) *metav1.Condition {
	t.Helper()
	u, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), joulie.ObjectNameForNode(node), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get NodeTwin %s: %v", node, err)
	}
	var nt v1alpha1.NodeTwin
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &nt); err != nil {
		t.Fatalf("decode NodeTwin %s: %v", node, err)
	}
	return meta.FindStatusCondition(nt.Status.Conditions, joulie.ConditionAgentReporting)
}

// nodeHardwareUpdatedAt is a NodeHardware of node whose last agent write was
// at updatedAt.
func nodeHardwareUpdatedAt(t *testing.T, node string, updatedAt time.Time) *v1alpha1.NodeHardware {
	t.Helper()
	nh := nodeHardwareFromJSON(t, strings.ReplaceAll(nodeHardware165W, "n2-atos", node))
	nh.Status.UpdatedAt = updatedAt.UTC().Format(time.RFC3339)
	return nh
}

// Regression for D15: a managed node that no agent reaches (a taint the
// agent does not tolerate, an image without the node's platform) is planned
// like any other, and its NodeTwin said nothing about the caps going
// unenforced: it only lacked a controlStatus. Its twin now carries
// AgentReporting False with reason NoNodeHardware, True once an agent
// publishes NodeHardware, and False with NodeHardwareStale when that agent
// stops writing. The plan itself is unchanged: the spec keeps its cap.
func TestD15ManagedNodeWithoutAnAgentIsReported(t *testing.T) {
	ctx := context.Background()
	const node = "n-no-agent"
	reader, _, dyn := fakeClients(t)
	capPct := 80.0
	if err := upsertNodeTwinSpec(ctx, dyn, NodeAssignment{NodeName: node, Profile: profilePerformance, CPUCapPctOfMax: &capPct}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileNodeTwin(ctx, reader, dyn, node, profilePerformance, capPct, 100, false, nil); err != nil {
		t.Fatalf("reconcileNodeTwin: %v", err)
	}
	c := storedAgentCondition(t, dyn, node)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != joulie.ReasonNoNodeHardware || !strings.Contains(c.Message, "not enforced") {
		t.Fatalf("AgentReporting %+v on a managed node without NodeHardware, want False, reason %s, a message that the caps are not enforced (D15)", c, joulie.ReasonNoNodeHardware)
	}
	if c.LastTransitionTime.IsZero() {
		t.Errorf("AgentReporting has no lastTransitionTime, which the CRD requires")
	}
	u, err := dyn.Resource(nodeTwinGVR).Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var nt v1alpha1.NodeTwin
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &nt); err != nil {
		t.Fatal(err)
	}
	if nt.Spec.CPU == nil || nt.Spec.CPU.PackagePowerCapPctOfMax == nil || *nt.Spec.CPU.PackagePowerCapPctOfMax != capPct {
		t.Errorf("spec.cpu %+v, want the planned %v %% cap kept: the condition reports, it does not change the plan", nt.Spec.CPU, capPct)
	}

	reader = fakeReader(t, nodeHardwareUpdatedAt(t, node, time.Now().Add(-time.Minute)))
	if _, err := reconcileNodeTwin(ctx, reader, dyn, node, profilePerformance, capPct, 100, false, nil); err != nil {
		t.Fatalf("reconcileNodeTwin: %v", err)
	}
	if c := storedAgentCondition(t, dyn, node); c == nil || c.Status != metav1.ConditionTrue || c.Reason != joulie.ReasonNodeHardwareCurrent {
		t.Fatalf("AgentReporting %+v once the agent wrote NodeHardware a minute ago, want True, reason %s", c, joulie.ReasonNodeHardwareCurrent)
	}

	stale := time.Now().Add(-joulie.AgentReportingStaleAfter - time.Minute)
	reader = fakeReader(t, nodeHardwareUpdatedAt(t, node, stale))
	if _, err := reconcileNodeTwin(ctx, reader, dyn, node, profilePerformance, capPct, 100, false, nil); err != nil {
		t.Fatalf("reconcileNodeTwin: %v", err)
	}
	if c := storedAgentCondition(t, dyn, node); c == nil || c.Status != metav1.ConditionFalse || c.Reason != joulie.ReasonNodeHardwareStale ||
		!strings.Contains(c.Message, stale.UTC().Format(time.RFC3339)) {
		t.Fatalf("AgentReporting %+v with NodeHardware last written at %s, want False, reason %s, naming that time", c, stale.UTC().Format(time.RFC3339), joulie.ReasonNodeHardwareStale)
	}
}

// The condition's lastTransitionTime is when its status last changed: a
// write with the same status keeps it, a new status moves it, and a
// condition the write does not carry stays.
func TestAgentReportingConditionKeepsItsTransitionTime(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	other := metav1.Condition{Type: "Other", Status: metav1.ConditionTrue, Reason: "R", LastTransitionTime: metav1.NewTime(t0)}
	absent := agentReportingCondition(nil, t0)
	out, changed := mergeConditions([]metav1.Condition{other}, []metav1.Condition{absent}, t0)
	if len(changed) != 1 || len(out) != 2 {
		t.Fatalf("first write: changed %v, conditions %v; want the new AgentReporting changed and Other kept", changed, out)
	}
	t1 := t0.Add(10 * time.Minute)
	out, changed = mergeConditions(out, []metav1.Condition{agentReportingCondition(nil, t1)}, t1)
	if c := meta.FindStatusCondition(out, joulie.ConditionAgentReporting); len(changed) != 0 || c == nil || !c.LastTransitionTime.Time.Equal(t0) {
		t.Fatalf("same status again: changed %v, condition %+v; want no change and lastTransitionTime %s", changed, c, t0)
	}
	t2 := t1.Add(time.Minute)
	reporting := agentReportingCondition(&v1alpha1.NodeHardware{Status: v1alpha1.NodeHardwareStatus{UpdatedAt: t2.Format(time.RFC3339)}}, t2)
	out, changed = mergeConditions(out, []metav1.Condition{reporting}, t2)
	if c := meta.FindStatusCondition(out, joulie.ConditionAgentReporting); len(changed) != 1 || c == nil || c.Status != metav1.ConditionTrue || !c.LastTransitionTime.Time.Equal(t2) {
		t.Fatalf("status True: changed %v, condition %+v; want a change at %s", changed, c, t2)
	}
	if meta.FindStatusCondition(out, "Other") == nil {
		t.Fatalf("conditions %v lost Other", out)
	}
}

// One reconcile of three managed nodes, one whose agent writes NodeHardware,
// one with none and one whose agent stopped writing, sets
// joulie_twin_nodes_agent_reporting to one node per status and reason; a
// reconcile with no managed node sets every series to 0 and keeps it, so an
// alert on the false series always has a value.
func TestAgentReportingGaugeCountsTheManagedNodes(t *testing.T) {
	managed := func(name string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"joulie.io/managed": "true"}}}
	}
	reconcile := func(objs ...runtime.Object) {
		t.Helper()
		reader, client, dyn := fakeClients(t, objs...)
		selector, err := labels.Parse("joulie.io/managed=true")
		if err != nil {
			t.Fatal(err)
		}
		if err := reconcileWithCatalogAndFacility(
			context.Background(), reader, client, dyn, selector,
			"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
			5000, 120, "static_partition", 0.5, 0.5, 1, 5, 10,
			100, 60, false, 100, 60, false,
			map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"}, loadHardwareCatalog(),
			&facilityMetrics{},
		); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	want := func(when string, current, none, stale float64) {
		t.Helper()
		for _, c := range []struct {
			status, reason string
			want           float64
		}{
			{"true", joulie.ReasonNodeHardwareCurrent, current},
			{"false", joulie.ReasonNoNodeHardware, none},
			{"false", joulie.ReasonNodeHardwareStale, stale},
		} {
			if got := testutil.ToFloat64(twinNodesAgentReporting.WithLabelValues(c.status, c.reason)); got != c.want {
				t.Errorf("%s: joulie_twin_nodes_agent_reporting{status=%q,reason=%q} = %v, want %v", when, c.status, c.reason, got, c.want)
			}
		}
		if n := testutil.CollectAndCount(twinNodesAgentReporting); n != 3 {
			t.Errorf("%s: %d series, want the 3 status and reason pairs", when, n)
		}
	}

	reconcile(
		managed("with-agent"), nodeHardwareUpdatedAt(t, "with-agent", time.Now().Add(-time.Minute)),
		managed("no-agent"),
		managed("stale-agent"), nodeHardwareUpdatedAt(t, "stale-agent", time.Now().Add(-joulie.AgentReportingStaleAfter-time.Minute)),
	)
	want("three managed nodes", 1, 1, 1)
	reconcile()
	want("no managed node", 0, 0, 0)
}
