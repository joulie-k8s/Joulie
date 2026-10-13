package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/kube"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// simJobPod is the first pod the Job controller creates from a simulator v1
// Job object: the template's metadata and spec plus the controller's own
// name label and completion index.
func simJobPod(t *testing.T, class string, intent workload.Intent) corev1.Pod {
	t.Helper()
	g := workload.Group{
		Key: "wl-1/worker@0",
		Template: workload.Template{
			Namespace: "sim", WorkloadID: "wl-1", PodRole: "worker", Class: class,
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		},
		JobIDs: []string{"wl-1-worker-01", "wl-1-worker-02"},
	}
	job := workload.BuildJob(g, workload.BuildOptions{RunID: "run-1", Intent: intent, Tolerations: []corev1.Toleration{workload.KWOKToleration()}})
	meta := *job.Spec.Template.ObjectMeta.DeepCopy()
	meta.Name, meta.Namespace = job.Name+"-0-abcde", job.Namespace
	meta.Labels[batchv1.JobNameLabel] = job.Name
	meta.Annotations[batchv1.JobCompletionIndexAnnotation] = "0"
	return corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: meta, Spec: *job.Spec.Template.Spec.DeepCopy()}
}

// A performance pod the simulator builds reaches the extender over the wire
// as the kube-scheduler sends it (nodeCacheCapable false, so full nodes) and
// is filtered off the eco node, while a standard pod, a performance pod
// whose intent declares nothing and one that declares it only through node
// affinity, which the kube-scheduler enforces, pass every node. The class
// travels only in the joulie.io/workload-class annotation, so the builder,
// the JSON encoding and extractWorkloadClass must agree on it.
func TestSimulatorJobPodClassReadByExtender(t *testing.T) {
	s, err := kube.NewScheme(v1alpha1.AddToScheme)
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	twin := func(node, class string) *v1alpha1.NodeTwin {
		return &v1alpha1.NodeTwin{
			ObjectMeta: metav1.ObjectMeta{Name: node},
			Spec:       v1alpha1.NodeTwinSpec{NodeName: node, Profile: class},
			Status:     v1alpha1.NodeTwinStatus{SchedulableClass: class},
		}
	}
	resetReaderState(t, fake.NewClientBuilder().WithScheme(s).WithObjects(twin("eco-node", "eco"), twin("perf-node", "performance")).Build())

	cases := []struct {
		class     string
		intent    workload.Intent
		wantClass string
		rejected  bool
	}{
		{workload.ClassPerformance, workload.IntentAnnotation, "performance", true},
		{workload.ClassPerformance, workload.IntentBoth, "performance", true},
		{workload.ClassStandard, workload.IntentAnnotation, "standard", false},
		{workload.ClassPerformance, workload.IntentNone, "standard", false},
		{workload.ClassPerformance, workload.IntentAffinity, "standard", false},
	}
	for _, c := range cases {
		pod := simJobPod(t, c.class, c.intent)
		body, err := json.Marshal(map[string]any{
			"pod": pod,
			"nodes": map[string]any{"items": []map[string]any{
				{"metadata": map[string]any{"name": "eco-node"}},
				{"metadata": map[string]any{"name": "perf-node"}},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		var args ExtenderArgs
		if err := json.Unmarshal(body, &args); err != nil {
			t.Fatal(err)
		}
		if got := extractWorkloadClass(args); got != c.wantClass {
			t.Errorf("%s under %s: extender reads class %q, want %q", c.class, c.intent, got, c.wantClass)
		}

		rec := httptest.NewRecorder()
		handleFilter(rec, httptest.NewRequest(http.MethodPost, "/filter", bytes.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("filter answered %d: %s", rec.Code, rec.Body)
		}
		var res ExtenderFilterResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		_, ecoRejected := res.FailedNodes["eco-node"]
		_, perfRejected := res.FailedNodes["perf-node"]
		if ecoRejected != c.rejected || perfRejected {
			t.Errorf("%s under %s: failed nodes %v, want eco-node rejected=%v and perf-node passing", c.class, c.intent, res.FailedNodes, c.rejected)
		}
	}
}
