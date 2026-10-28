package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// legacyOrderTrace has 64 jobs in a scrambled id order, half of them due at
// 0 s, so the 20-per-tick cap splits them over two ticks and the unstable
// sort decides which go first.
func legacyOrderTrace() []string {
	offsets := []float64{0, 0, 2, 5}
	lines := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		ns := "default"
		if i%3 == 0 {
			ns = "team-a"
		}
		lines = append(lines, fmt.Sprintf(
			`{"type":"job","jobId":"job-%02d","submitTimeOffsetSec":%g,"namespace":%q,"podTemplate":{"requests":{"cpu":"1"}},"work":{"cpuUnits":100}}`,
			(i*37)%64, offsets[(i*7)%4], ns))
	}
	return lines
}

// injectionOrder runs the legacy injection for ticks one-second ticks and
// returns one line per created pod: the tick, the namespace and name, and
// the jobId annotation.
func injectionOrder(t *testing.T, s *simulator, ticks int) []string {
	t.Helper()
	kube := k8sfake.NewSimpleClientset()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.workload.startTime = start
	var out []string
	for k := 0; k < ticks; k++ {
		kube.ClearActions()
		s.injectTraceJobs(context.Background(), kube, start.Add(time.Duration(k)*time.Second))
		for _, a := range kube.Actions() {
			ca, ok := a.(k8stesting.CreateAction)
			if !ok || a.GetResource().Resource != "pods" {
				continue
			}
			pod := ca.GetObject().(*corev1.Pod)
			out = append(out, fmt.Sprintf("%d %s/%s %s", k, pod.Namespace, pod.Name, pod.Annotations["sim.joulie.io/jobId"]))
		}
	}
	return out
}

// TestLegacyInjectionOrderUnchanged pins the order and the per-tick split in
// which the legacy engine creates pods. Legacy keeps sort.Slice, whose order
// for jobs due at the same offset depends on the Go release, so the expected
// order is sort.Slice applied to the trace on the running toolchain. The
// per-tick counts were captured from the code before the per-tick caps became
// settings.
func TestLegacyInjectionOrderUnchanged(t *testing.T) {
	trace := legacyOrderTrace()
	s := newLoaderTestSimulator()
	if err := s.initWorkloadEngineWithOptions(writeTrace(t, trace...), workloadOptions{}); err != nil {
		t.Fatalf("initWorkloadEngineWithOptions: %v", err)
	}
	got := injectionOrder(t, s, 7)

	type rec struct {
		JobID     string  `json:"jobId"`
		Namespace string  `json:"namespace"`
		Offset    float64 `json:"submitTimeOffsetSec"`
	}
	recs := make([]rec, len(trace))
	for i, line := range trace {
		if err := json.Unmarshal([]byte(line), &recs[i]); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Offset < recs[j].Offset })
	ticks := []struct{ tick, pods int }{{0, 20}, {1, 12}, {2, 16}, {5, 16}}
	var want []string
	for _, tk := range ticks {
		for _, r := range recs[len(want) : len(want)+tk.pods] {
			want = append(want, fmt.Sprintf("%d %s/sim-%s %s", tk.tick, r.Namespace, r.JobID, r.JobID))
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("legacy injection order moved:\n got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestLoadTraceReadsV3Fields: a v3 record carries its podTemplate placement,
// its schemaVersion, whether it gave a sensitivity, and the shared and MIG
// GPU resources. A v2 record with the same keys reads as before.
func TestLoadTraceReadsV3Fields(t *testing.T) {
	podTemplate := `"podTemplate":{"requests":{"cpu":"1",%s},` +
		`"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"zone","operator":"In","values":["a"]}]}]}}},` +
		`"nodeSelector":{"pool":"gpu"},"tolerations":[{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"}]}`
	rec := func(id, schema, gpu, extra string) string {
		return fmt.Sprintf(`{"type":"job","schemaVersion":%q,"jobId":%q,`+podTemplate+`%s}`, schema, id, gpu, extra)
	}
	path := writeTrace(t,
		rec("v3-mig", workload.TraceSchemaV3, `"nvidia.com/mig-3g.40gb":"2","nvidia.com/mig-1g.10gb":"1"`, `,"sensitivity":{"gpu":0.5}`),
		rec("v3-shared", workload.TraceSchemaV3, `"nvidia.com/gpu.shared":"1"`, ""),
		rec("v3-whole", workload.TraceSchemaV3, `"nvidia.com/gpu":"1","nvidia.com/mig-1g.10gb":"1"`, ""),
		rec("v2-mig", workload.TraceSchemaV2, `"nvidia.com/mig-1g.10gb":"1"`, `,"sensitivity":{"cpu":0.4,"gpu":0.5}`),
	)
	s := newLoaderTestSimulator()
	if err := s.initWorkloadEngineWithOptions(path, workloadOptions{}); err != nil {
		t.Fatalf("initWorkloadEngineWithOptions: %v", err)
	}
	byID := map[string]*simJob{}
	for _, j := range s.workload.jobs {
		byID[j.JobID] = j
	}
	for _, tc := range []struct {
		id, resource string
		gpus         float64
		sensSet      bool
	}{
		{"v3-mig", "nvidia.com/mig-1g.10gb", 1, true},
		{"v3-shared", "nvidia.com/gpu.shared", 1, false},
		{"v3-whole", "nvidia.com/gpu", 1, false},
		{"v2-mig", "", 0, true},
	} {
		j := byID[tc.id]
		if j.GPUResourceName != tc.resource || j.RequestedGPUs != tc.gpus || j.SensitivitySet != tc.sensSet {
			t.Errorf("%s: gpu %q x %g, sensitivity set %v; want %q x %g, %v", tc.id, j.GPUResourceName, j.RequestedGPUs, j.SensitivitySet, tc.resource, tc.gpus, tc.sensSet)
		}
	}
	v3 := byID["v3-mig"]
	if v3.SchemaVersion != workload.TraceSchemaV3 || v3.SensitivityGPU != 0.5 || v3.SensitivityCPU != 1 {
		t.Errorf("v3-mig: schema %q, sensitivity cpu %g gpu %g; want v3, 1, 0.5", v3.SchemaVersion, v3.SensitivityCPU, v3.SensitivityGPU)
	}
	if v3.Affinity == nil || v3.Affinity.NodeAffinity == nil || v3.NodeSelector["pool"] != "gpu" ||
		len(v3.Tolerations) != 1 || v3.Tolerations[0].Key != "nvidia.com/gpu" {
		t.Errorf("v3-mig placement = %+v %v %v, want the trace's affinity, nodeSelector and toleration", v3.Affinity, v3.NodeSelector, v3.Tolerations)
	}
	// A legacy record keeps the trace's affinity only (legacyAffinity).
	if v2 := byID["v2-mig"]; v2.Affinity == nil || v2.NodeSelector != nil || v2.Tolerations != nil || v2.SchemaVersion != workload.TraceSchemaV2 {
		t.Errorf("v2-mig: placement %+v %v %v, schema %q; want the affinity only, v2", v2.Affinity, v2.NodeSelector, v2.Tolerations, v2.SchemaVersion)
	}

	t.Run("a misspelled placement field is an error", func(t *testing.T) {
		path := writeTrace(t, `{"type":"job","schemaVersion":"v3","jobId":"bad","podTemplate":{"requests":{"cpu":"1"},"nodeSelector":{"a":"b"},"tolerations":[{"keey":"x"}]}}`)
		err := newLoaderTestSimulator().initWorkloadEngineWithOptions(path, workloadOptions{})
		if err == nil || !strings.Contains(err.Error(), "trace line 1") || !strings.Contains(err.Error(), "keey") {
			t.Fatalf("error = %v, want one naming trace line 1 and the field", err)
		}
	})
}

// TestV1LoaderKeepsTraceOrder: the v1 loader, loadRunTrace, sorts stably,
// so jobs due at the same offset stay in trace order.
func TestV1LoaderKeepsTraceOrder(t *testing.T) {
	path := writeTrace(t, legacyOrderTrace()...)
	engine, err := loadRunTrace(path, workloadOptions{})
	if err != nil {
		t.Fatalf("loadRunTrace: %v", err)
	}
	var want []string
	for _, off := range []float64{0, 2, 5} {
		for i := range legacyOrderTrace() {
			if []float64{0, 0, 2, 5}[(i*7)%4] == off {
				want = append(want, fmt.Sprintf("job-%02d", (i*37)%64))
			}
		}
	}
	for i, j := range engine.jobs {
		if j.JobID != want[i] {
			t.Fatalf("job %d is %s, want %s: v1 must keep trace order at equal offsets", i, j.JobID, want[i])
		}
	}
	if engine.opts.model() != workload.ModelV1 {
		t.Fatalf("engine model = %s, want v1", engine.opts.model())
	}
}

// TestLegacyPodKeysUnchanged: a legacy pod carries the label, annotation,
// container, image, toleration, node selector and affinity keys the legacy
// experiments ran with, byte for byte. They are spelled out here, not taken
// from simulator/pkg/workload, so a change to those constants fails here.
func TestLegacyPodKeysUnchanged(t *testing.T) {
	t.Setenv(workload.EnvImage, "")
	path := writeTrace(t, `{"type":"job","jobId":"j-1","namespace":"team-a","workloadId":"w-1","workloadType":"Training Run","podRole":"Worker_0",`+
		`"intentClass":"performance","podTemplate":{"requests":{"cpu":"2","memory":"512Mi","nvidia.com/gpu":"1"}},`+
		`"workloadProfile":{"cpuUtilization":0.25,"gpuUtilization":0.875,"memoryIntensity":0.5,"ioIntensity":0.125},"work":{"cpuUnits":100,"gpuUnits":100}}`)
	s := newLoaderTestSimulator()
	if err := s.initWorkloadEngineWithOptions(path, workloadOptions{}); err != nil {
		t.Fatalf("initWorkloadEngineWithOptions: %v", err)
	}
	kube := k8sfake.NewSimpleClientset()
	s.injectTraceJobs(context.Background(), kube, time.Now().Add(time.Minute))
	pod, err := kube.CoreV1().Pods("team-a").Get(context.Background(), "sim-j-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("legacy pod: %v", err)
	}
	wantLabels := map[string]string{
		"app.kubernetes.io/part-of":   "joulie-sim-workload",
		"sim.joulie.io/workload-type": "training-run",
		"sim.joulie.io/pod-role":      "worker-0",
	}
	wantAnnotations := map[string]string{
		"sim.joulie.io/jobId":               "j-1",
		"sim.joulie.io/workloadId":          "w-1",
		"sim.joulie.io/workloadType":        "Training Run",
		"sim.joulie.io/podRole":             "Worker_0",
		"sim.joulie.io/cpu-util-pct":        "25.0",
		"sim.joulie.io/gpu-util-pct":        "87.5",
		"sim.joulie.io/memory-pressure-pct": "50.0",
		"sim.joulie.io/io-intensity":        "0.12",
	}
	if !reflect.DeepEqual(pod.Labels, wantLabels) {
		t.Errorf("labels %v, want %v", pod.Labels, wantLabels)
	}
	if !reflect.DeepEqual(pod.Annotations, wantAnnotations) {
		t.Errorf("annotations %v, want %v", pod.Annotations, wantAnnotations)
	}
	if c := pod.Spec.Containers; len(c) != 1 || c[0].Name != "work" || c[0].Image != "registry.k8s.io/pause:3.9" {
		t.Errorf("containers %+v, want one named work running registry.k8s.io/pause:3.9", c)
	}
	wantTol := []corev1.Toleration{{Key: "kwok.x-k8s.io/node", Operator: corev1.TolerationOpEqual, Value: "fake", Effect: corev1.TaintEffectNoSchedule}}
	if !reflect.DeepEqual(pod.Spec.Tolerations, wantTol) {
		t.Errorf("tolerations %+v, want %+v", pod.Spec.Tolerations, wantTol)
	}
	wantSel := map[string]string{"type": "kwok", "feature.node.kubernetes.io/pci-10de.present": "true"}
	if !reflect.DeepEqual(pod.Spec.NodeSelector, wantSel) {
		t.Errorf("node selector %v, want %v", pod.Spec.NodeSelector, wantSel)
	}
	wantAff := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "joulie.io/power-profile", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"eco"}},
		}}},
	}}}
	if !reflect.DeepEqual(pod.Spec.Affinity, wantAff) {
		t.Errorf("affinity %+v, want %+v", pod.Spec.Affinity, wantAff)
	}
	if got := classifyClassFromPodSpec(&pod.Spec); got != "performance" {
		t.Errorf("the pod reads back as class %q, want performance", got)
	}

	t.Setenv(workload.EnvImage, "example.org/work:1")
	s = newLoaderTestSimulator()
	if err := s.initWorkloadEngineWithOptions(path, workloadOptions{}); err != nil {
		t.Fatal(err)
	}
	kube = k8sfake.NewSimpleClientset()
	s.injectTraceJobs(context.Background(), kube, time.Now().Add(time.Minute))
	if pod, err := kube.CoreV1().Pods("team-a").Get(context.Background(), "sim-j-1", metav1.GetOptions{}); err != nil || pod.Spec.Containers[0].Image != "example.org/work:1" {
		t.Errorf("with SIM_WORKLOAD_IMAGE set: pod %v, err %v; want the image example.org/work:1", pod, err)
	}
}
