package scenario_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/kube"
	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/scenario"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// A snapshot taken through the typed client writes and reads back: the
// workload's objects at the end only, and only those of the workload.
func TestKubeSnapshotRoundTrip(t *testing.T) {
	scheme, err := kube.NewScheme(v1alpha1.AddToScheme, batchv1.AddToScheme)
	if err != nil {
		t.Fatal(err)
	}
	fx := fxSnapshot(scenario.SnapshotEnd)
	objs := []client.Object{
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "not-workload", Namespace: "kube-system"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "default"}, Reason: "Scheduled"},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e2", Namespace: "kube-system"}, Reason: "Other"},
	}
	for i := range fx.Nodes {
		objs = append(objs, &fx.Nodes[i], &fx.NodeHardwares[i])
	}
	for i := range fx.NodeTwins {
		objs = append(objs, &fx.NodeTwins[i])
	}
	for i := range fx.Jobs {
		objs = append(objs, &fx.Jobs[i])
	}
	for i := range fx.Pods {
		objs = append(objs, &fx.Pods[i])
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	dir := t.TempDir()
	for _, phase := range []string{scenario.SnapshotStart, scenario.SnapshotEnd} {
		snap, err := scenario.TakeKubeSnapshot(context.Background(), c, phase)
		if err != nil {
			t.Fatal(err)
		}
		if err := scenario.WriteKubeSnapshot(dir, phase, snap); err != nil {
			t.Fatal(err)
		}
	}
	start, err := scenario.ReadKubeSnapshot(dir, scenario.SnapshotStart)
	if err != nil {
		t.Fatal(err)
	}
	end, err := scenario.ReadKubeSnapshot(dir, scenario.SnapshotEnd)
	if err != nil {
		t.Fatal(err)
	}
	if len(start.Nodes) != 2 || len(start.NodeTwins) != 2 || len(start.Pods) != 0 || start.Time.IsZero() {
		t.Errorf("start %d nodes %d twins %d pods at %s", len(start.Nodes), len(start.NodeTwins), len(start.Pods), start.Time)
	}
	if len(end.Pods) != 2 || len(end.Jobs) != 2 || len(end.Events) != 1 || end.Events[0].Reason != "Scheduled" {
		t.Errorf("end %d pods %d jobs %d events", len(end.Pods), len(end.Jobs), len(end.Events))
	}
	if end.NodeTwins[1].Status.ControlStatus.GPU.Result != "blocked" || end.Pods[0].Labels[workload.LabelPartOf] != workload.PartOfWorkload {
		t.Errorf("typed fields lost: %+v", end.NodeTwins[1].Status)
	}
	if empty, err := scenario.ReadKubeSnapshot(t.TempDir(), scenario.SnapshotEnd); err != nil || !empty.Time.IsZero() {
		t.Errorf("no files: %+v %v", empty, err)
	}
}

// Each capture records the twins' measured power, their schedulable class
// and each component's control result, keyed by node.
func TestCaptureNodeTwins(t *testing.T) {
	twins := []v1alpha1.NodeTwin{
		{Spec: v1alpha1.NodeTwinSpec{NodeName: "b"}, Status: v1alpha1.NodeTwinStatus{SchedulableClass: "eco",
			PowerMeasurement: &v1alpha1.PowerMeasurement{Source: "prometheus", MeasuredNodePowerW: 420},
			ControlStatus:    &v1alpha1.ControlStatus{CPU: &v1alpha1.ControlResult{Backend: "rapl", Result: "applied"}, GPU: &v1alpha1.ControlResult{Backend: "none", Result: "blocked"}}}},
		{Spec: v1alpha1.NodeTwinSpec{NodeName: "a"}, Status: v1alpha1.NodeTwinStatus{SchedulableClass: "performance"}},
	}
	at := time.Date(2026, 10, 4, 20, 1, 0, 0, time.UTC)
	ms := scenario.CaptureNodeTwins(twins, at, at.Add(-time.Minute))
	points := scenario.FlattenMetrics(otlpjson.MetricsData{ResourceMetrics: []otlpjson.ResourceMetrics{{ScopeMetrics: []otlpjson.ScopeMetrics{{Metrics: ms}}}}}, "h")
	var got []string
	for _, p := range points {
		node, _ := p.Attributes.String(semconv.AttrK8sNodeName)
		got = append(got, p.Metric+" "+node+" "+p.Kind.String())
		if p.Kind == semconv.UpDownCounter && (p.Value != 1 || !p.StartTime.Equal(at.Add(-time.Minute))) {
			t.Errorf("state point %+v", p)
		}
	}
	want := []string{
		"joulie.capture.nodetwin.measured_power b gauge",
		"joulie.capture.nodetwin.schedulable_class a updowncounter",
		"joulie.capture.nodetwin.schedulable_class b updowncounter",
		"joulie.capture.nodetwin.control_result b updowncounter",
		"joulie.capture.nodetwin.control_result b updowncounter",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("points\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r, _ := points[4].Attributes.String(semconv.AttrJoulieNodeTwinControlResult); r != "blocked" {
		t.Errorf("control point %v", points[4].Attributes)
	}
}

// The preflight refuses a pod-complete Stage that does not select the
// completion annotation or still selects by owner.
func TestCheckPodCompleteStage(t *testing.T) {
	good := `{"spec":{"selector":{"matchExpressions":[{"key":".status.phase","operator":"In","values":["Running"]},
{"key":".metadata.annotations[\"sim.joulie.io/complete\"]","operator":"In","values":["true"]}]}}}`
	if err := scenario.CheckPodCompleteStage([]byte(good)); err != nil {
		t.Errorf("good stage: %v", err)
	}
	upstream := `{"spec":{"selector":{"matchExpressions":[{"key":".metadata.ownerReferences.[].kind","operator":"In","values":["Job"]}]}}}`
	if err := scenario.CheckPodCompleteStage([]byte(upstream)); err == nil || !strings.Contains(err.Error(), "does not select the annotation") {
		t.Errorf("upstream stage: %v", err)
	}
	both := strings.Replace(good, `]}}}`, `,{"key":".metadata.ownerReferences.[].kind","operator":"In","values":["Job"]}]}}}`, 1)
	if err := scenario.CheckPodCompleteStage([]byte(both)); err == nil || !strings.Contains(err.Error(), "ownerReferences") {
		t.Errorf("owner selector: %v", err)
	}
}

// Every emulated agent pod runs on the infra Node, and the Node it stands
// in for is the pod annotation sim.joulie.io/node, which the chart sets:
// CollectLogs writes one agent log per stood-in Node, read from the
// annotation first, then the label, then spec.nodeName.
func TestCollectLogsNamesEachAgentByItsNode(t *testing.T) {
	const infra = "joulie-sim-infra"
	agent := func(name string, annotations, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: scenario.NamespaceJoulie, Annotations: annotations, Labels: labels},
			Spec:       corev1.PodSpec{NodeName: infra},
		}
	}
	cs := k8sfake.NewSimpleClientset(
		agent("joulie-agent-emu-0-a", map[string]string{"sim.joulie.io/node": "intel-0"}, nil),
		agent("joulie-agent-emu-1-b", map[string]string{"sim.joulie.io/node": "nvl-0"}, nil),
		agent("joulie-agent-emu-2-c", nil, map[string]string{"sim.joulie.io/node": "epyc-0"}),
		agent("joulie-agent-x", nil, nil),
	)
	dir := t.TempDir()
	if err := scenario.CollectLogs(context.Background(), cs, dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, scenario.LogsDir))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	want := []string{"agent-epyc-0.log", "agent-intel-0.log", "agent-" + infra + ".log", "agent-nvl-0.log"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("log files %v, want %v: one per Node an agent stands in for", got, want)
	}
}
