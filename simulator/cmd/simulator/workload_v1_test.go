package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/scenario"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// v1TestStart is when the runs of these tests start, on the run's clock.
var v1TestStart = time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)

// fakeTP is a node whose controls cost what the test says: cpu for every
// set of CPUs, gpu[i] for GPU i (1 when unset).
type fakeTP struct {
	cpu      float64
	gpu      map[int]float64
	idleW    float64
	exposure string
	units    int
}

func (f *fakeTP) CPUThroughput([]int, string, float64, float64) float64 {
	if f.cpu == 0 {
		return 1
	}
	return f.cpu
}

func (f *fakeTP) GPUThroughput(i int) float64 {
	if v, ok := f.gpu[i]; ok {
		return v
	}
	return 1
}

func (f *fakeTP) IdleCPUPowerW(int) float64  { return f.idleW }
func (f *fakeTP) GPUExposure() (string, int) { return f.exposure, f.units }

// fakeNodes are emulated nodes by name.
type fakeNodes map[string]emuNode

func (f fakeNodes) emuNode(name string) (emuNode, bool) {
	n, ok := f[name]
	return n, ok
}

// fakeCPUNode is a 16-CPU node; fakeGPUNode an 8-GPU node with 64 CPUs.
func fakeCPUNode() emuNode {
	return emuNode{Profile: "fake-cpu", CPUs: 16, Sockets: 1, AllocatableCPU: 16, TP: &fakeTP{}}
}

func fakeGPUNode(tp *fakeTP) emuNode {
	if tp.exposure == "" {
		tp.exposure, tp.units = "whole", 1
	}
	return emuNode{Profile: "fake-gpu", CPUs: 64, Sockets: 2, GPUs: 8, AllocatableCPU: 64, GPUIdleW: 50, TP: tp}
}

// v1Harness is a v1 run over a fake API server. The test plays the Job
// controller, the scheduler and the kubelet with the helpers below. It
// ticks either over fake nodes (tick) or over a real emulator (et).
type v1Harness struct {
	t     *testing.T
	s     *simulator
	run   *workloadRun
	kube  *k8sfake.Clientset
	nodes fakeNodes
	et    *emulateTest
	start time.Time
	prev  time.Time
	// pods maps a trace jobId to its pod's namespace/name.
	pods map[string]string
}

// traceJob is one v3 job record: one CPU, 100 work units, util 1, at
// offset 0, standard, then whatever set changes.
func traceJob(id string, set func(map[string]any)) string {
	rec := map[string]any{
		"type": "job", "schemaVersion": workload.TraceSchemaV3, "jobId": id,
		"submitTimeOffsetSec": 0.0, "intentClass": workload.ClassStandard,
		"podTemplate":     map[string]any{"requests": map[string]any{"cpu": "1"}},
		"work":            map[string]any{"cpuUnits": 100.0},
		"workloadProfile": map[string]any{"cpuUtilization": 1.0},
	}
	if set != nil {
		set(rec)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// v1Options are the simulator's v1 options from env, with the runs under
// a temporary directory.
func v1Options(t *testing.T, env map[string]string) workloadOptions {
	t.Helper()
	get := func(k string) string {
		if k == workload.EnvRunsDir {
			if v, ok := env[k]; ok {
				return v
			}
			return t.TempDir()
		}
		return env[k]
	}
	o, _, err := loadWorkloadOptions(get, true)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// newV1Harness starts run "r1" of the trace lines on s, in cluster c1,
// with ro's overrides, over a fake API server. Ticks go to nodes, or to
// the emulator of et when it is set.
func newV1Harness(t *testing.T, s *simulator, nodes fakeNodes, et *emulateTest, lines []string, ro workload.RunOptions) *v1Harness {
	t.Helper()
	opts := v1Options(t, nil)
	trace := writeTrace(t, lines...)
	engine, err := loadRunTrace(trace, opts)
	if err != nil {
		t.Fatalf("loadRunTrace: %v", err)
	}
	cluster := "c1"
	if et != nil {
		cluster = et.rt.cluster
	}
	set, err := newRunSettings(opts, ro, cluster, engine.baseSpeedCore)
	if err != nil {
		t.Fatal(err)
	}
	run := newWorkloadRun(s, "r1", cluster, engine, set)
	run.dir = filepath.Join(opts.RunsDir, "r1")
	res := telemetry.Resource{ServiceName: telemetry.DefaultServiceName, Instance: "sim-test"}
	if run.rec, err = newRunRecorder(run.dir, res, cluster, "r1", set.fileInterval); err != nil {
		t.Fatal(err)
	}
	fake := k8sfake.NewSimpleClientset()
	run.kube = newRunKube(fake, workload.RunLabelValue("r1"))
	s.run.Store(run)
	h := &v1Harness{t: t, s: s, run: run, kube: fake, nodes: nodes, et: et, start: v1TestStart, pods: map[string]string{}}
	if et != nil {
		h.start = emulateTestStart
	}
	t.Cleanup(func() {
		if run.active() {
			run.finish(false)
		}
	})
	return h
}

// newV1FakeHarness is newV1Harness over fake nodes.
func newV1FakeHarness(t *testing.T, nodes fakeNodes, lines []string, ro workload.RunOptions) *v1Harness {
	t.Helper()
	s := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 1000, prometheus.NewRegistry())
	return newV1Harness(t, s, nodes, nil, lines, ro)
}

// tick runs the run's tick at start+sec: over the fake nodes with an
// empty snapshot, or through the emulator.
func (h *v1Harness) tick(sec float64) *emulate.Snapshot {
	h.t.Helper()
	now := h.start.Add(time.Duration(sec * float64(time.Second)))
	if h.et != nil {
		return h.et.tick(h.t, now)
	}
	var dt time.Duration
	if !h.prev.IsZero() {
		dt = now.Sub(h.prev)
	}
	h.prev = now
	snap := &emulate.Snapshot{Time: now}
	for name := range h.nodes {
		snap.Nodes = append(snap.Nodes, emulate.NodeSnapshot{Name: name})
	}
	sort.Slice(snap.Nodes, func(i, j int) bool { return snap.Nodes[i].Name < snap.Nodes[j].Name })
	h.run.tick(now, dt, snap, h.nodes, nil)
	return snap
}

// step makes the queued Kubernetes calls, plays the Job controller, and
// ticks at start+sec.
func (h *v1Harness) step(sec float64) *emulate.Snapshot {
	h.t.Helper()
	h.run.kube.process(context.Background())
	h.jobController()
	return h.tick(sec)
}

// jobController makes the pods of every Job object as the Job controller
// does: named <job>-<index>, with the job-name label, the completion index
// of an Indexed Job and an owner reference. A bare pod of the run is
// observed as it is.
func (h *v1Harness) jobController() {
	h.t.Helper()
	ctx := context.Background()
	jobs, err := h.kube.BatchV1().Jobs("").List(ctx, metav1.ListOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, job := range jobs.Items {
		ids := strings.Split(job.Annotations[workload.AnnotationJobIDs], ",")
		for i, id := range ids {
			if _, ok := h.pods[id]; ok {
				continue
			}
			tpl := job.Spec.Template.DeepCopy()
			pod := &corev1.Pod{ObjectMeta: tpl.ObjectMeta, Spec: tpl.Spec}
			pod.Name = fmt.Sprintf("%s-%d", job.Name, i)
			pod.Namespace = job.Namespace
			pod.UID = types.UID("uid-" + pod.Name)
			if pod.Labels == nil {
				pod.Labels = map[string]string{}
			}
			pod.Labels[batchv1.JobNameLabel] = job.Name
			if job.Spec.CompletionMode != nil && *job.Spec.CompletionMode == batchv1.IndexedCompletion {
				pod.Annotations[batchv1.JobCompletionIndexAnnotation] = strconv.Itoa(i)
			}
			pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID}}
			pod.Status.Phase = corev1.PodPending
			if _, err := h.kube.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
				h.t.Fatal(err)
			}
			h.pods[id] = pod.Namespace + "/" + pod.Name
			h.run.kube.observePod(pod, false)
		}
	}
	pods, err := h.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if id := p.Annotations[workload.AnnotationJobID]; id != "" {
			if _, ok := h.pods[id]; !ok {
				h.pods[id] = p.Namespace + "/" + p.Name
				h.run.kube.observePod(p, false)
			}
		}
	}
}

// pod returns the pod of trace job id from the fake API server.
func (h *v1Harness) pod(id string) *corev1.Pod {
	h.t.Helper()
	key, ok := h.pods[id]
	if !ok {
		h.t.Fatalf("job %s has no pod", id)
	}
	ns, name, _ := strings.Cut(key, "/")
	p, err := h.kube.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		h.t.Fatalf("pod of job %s: %v", id, err)
	}
	return p
}

// setPod changes the pod of job id on the fake API server and lets the
// run observe it.
func (h *v1Harness) setPod(id string, change func(*corev1.Pod)) {
	h.t.Helper()
	p := h.pod(id)
	change(p)
	if _, err := h.kube.CoreV1().Pods(p.Namespace).UpdateStatus(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.kube.CoreV1().Pods(p.Namespace).Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		h.t.Fatal(err)
	}
	h.run.kube.observePod(p, false)
}

// bind binds the pod of job id to node; runOn also makes it Running.
func (h *v1Harness) bind(id, node string) {
	h.setPod(id, func(p *corev1.Pod) { p.Spec.NodeName = node })
}

func (h *v1Harness) runOn(id, node string) {
	h.setPod(id, func(p *corev1.Pod) {
		p.Spec.NodeName = node
		p.Status.Phase = corev1.PodRunning
	})
}

// unschedulable marks the pod of job id Pending and unschedulable.
func (h *v1Harness) unschedulable(id string) {
	h.setPod(id, func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
	})
}

// job is the run's state of trace job id.
func (h *v1Harness) job(id string) *v1Job {
	h.t.Helper()
	h.run.mu.Lock()
	defer h.run.mu.Unlock()
	j := h.run.byID[id]
	if j == nil {
		h.t.Fatalf("no job %s in the run", id)
	}
	return j
}

// events decodes the run's logs file strictly and returns its records.
func (h *v1Harness) events() []scenario.Event {
	h.t.Helper()
	return readRunLogs(h.t, h.run.dir)
}

func readRunLogs(t *testing.T, dir string) []scenario.Event {
	t.Helper()
	var out []scenario.Event
	readRunLines(t, filepath.Join(dir, workload.OTLPDir, workload.SimulatorLogsFile), func(b []byte) {
		ld, err := otlpjson.DecodeLogsStrict(b)
		if err != nil {
			t.Fatalf("logs line does not decode strictly: %v\n%s", err, b)
		}
		out = append(out, scenario.FlattenLogs(ld, workload.SimulatorLogsFile)...)
	})
	return out
}

// readRunMetrics decodes the run's metrics file strictly and returns the
// points of its last line.
func readRunMetrics(t *testing.T, dir string) (lines int, last []scenario.Point) {
	t.Helper()
	readRunLines(t, filepath.Join(dir, workload.OTLPDir, workload.SimulatorMetricsFile), func(b []byte) {
		md, err := otlpjson.DecodeMetricsStrict(b)
		if err != nil {
			t.Fatalf("metrics line does not decode strictly: %v\n%s", err, b)
		}
		lines++
		last = scenario.FlattenMetrics(md, workload.SimulatorMetricsFile)
	})
	return lines, last
}

func readRunLines(t *testing.T, path string, fn func([]byte)) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) > 0 {
			fn(append([]byte(nil), sc.Bytes()...))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}

// named returns the events called name, optionally of job id.
func named(evs []scenario.Event, name, id string) []scenario.Event {
	var out []scenario.Event
	for _, e := range evs {
		if e.Name != name {
			continue
		}
		if id != "" {
			if got, _ := e.Attributes.String(semconv.AttrSimJobID); got != id {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// createdJobs lists the Job objects on the fake API server.
func (h *v1Harness) createdJobs() []batchv1.Job {
	h.t.Helper()
	list, err := h.kube.BatchV1().Jobs("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items
}

// countActions counts the fake API server's calls of verb on resource.
func (h *v1Harness) countActions(verb, resource string) int {
	n := 0
	for _, a := range h.kube.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

// TestV1JobsOwnPods: a v1 run creates one batch/v1 Job per group of trace
// jobs (Indexed with one pod per job above one, no retries, the run
// label) and no bare pod; the pods the Job controller makes are mapped
// back to their trace jobs by the job-name label and completion index.
func TestV1JobsOwnPods(t *testing.T) {
	nodes := fakeNodes{"n1": fakeCPUNode()}
	worker := func(id string) string {
		return traceJob(id, func(r map[string]any) {
			r["workloadId"], r["podRole"], r["workloadType"] = "train", "worker", "distributed"
		})
	}
	h := newV1FakeHarness(t, nodes, []string{worker("w0"), worker("w1"), worker("w2"), traceJob("solo", nil)}, workload.RunOptions{})
	h.tick(0)
	h.run.kube.process(context.Background())
	if n := h.countActions("create", "pods"); n != 0 {
		t.Errorf("the run created %d bare pods; with owner job every pod belongs to a Job object", n)
	}
	h.jobController()
	h.tick(1)

	jobs := h.createdJobs()
	if len(jobs) != 2 {
		t.Fatalf("%d Job objects, want 2 (one per group)", len(jobs))
	}
	var indexed *batchv1.Job
	for i := range jobs {
		j := &jobs[i]
		if j.Labels[workload.LabelRunID] != workload.RunLabelValue("r1") || j.Spec.Template.Labels[workload.LabelRunID] == "" {
			t.Errorf("Job %s carries no run label: %v", j.Name, j.Labels)
		}
		if j.Spec.BackoffLimit == nil || *j.Spec.BackoffLimit != 0 {
			t.Errorf("Job %s backoffLimit %v, want 0", j.Name, j.Spec.BackoffLimit)
		}
		if *j.Spec.Completions == 3 {
			indexed = j
		}
	}
	if indexed == nil || *indexed.Spec.CompletionMode != batchv1.IndexedCompletion || indexed.Annotations[workload.AnnotationJobIDs] != "w0,w1,w2" {
		t.Fatalf("no Indexed Job of 3 for workload train: %+v", jobs)
	}
	for _, id := range []string{"w0", "w1", "w2", "solo"} {
		h.runOn(id, "n1")
	}
	h.step(2)
	evs := h.events()
	for i, id := range []string{"w0", "w1", "w2"} {
		started := named(evs, semconv.EventSimJobStarted.Name, id)
		if len(started) != 1 {
			t.Fatalf("job %s: %d started events, want 1", id, len(started))
		}
		pod, _ := started[0].Attributes.String(semconv.AttrK8sPodName)
		job, _ := started[0].Attributes.String(semconv.AttrK8sJobName)
		if want := fmt.Sprintf("%s-%d", indexed.Name, i); pod != want || job != indexed.Name {
			t.Errorf("job %s started as pod %s of Job %s, want pod %s of Job %s", id, pod, job, want, indexed.Name)
		}
	}
	if st := h.run.status(); st.Jobs.Running != 4 || !st.AllSubmitted {
		t.Errorf("status %+v, want 4 running, all submitted", st)
	}
}

// TestV1PodsCarryWorkloadClass: every pod template declares its trace
// job's class with joulie.io/workload-class, the annotation the extender
// and the controller read, and the default intent adds no affinity.
func TestV1PodsCarryWorkloadClass(t *testing.T) {
	perf := traceJob("p", func(r map[string]any) { r["intentClass"] = workload.ClassPerformance })
	h := newV1FakeHarness(t, fakeNodes{}, []string{perf, traceJob("s", nil)}, workload.RunOptions{})
	h.tick(0)
	h.step(1)
	got := map[string]string{}
	for _, j := range h.createdJobs() {
		got[j.Annotations[workload.AnnotationJobIDs]] = j.Spec.Template.Annotations[workload.AnnotationWorkloadClass]
		if j.Spec.Template.Spec.Affinity != nil {
			t.Errorf("Job %s has an affinity under the annotation intent: %+v", j.Name, j.Spec.Template.Spec.Affinity)
		}
	}
	if got["p"] != workload.ClassPerformance || got["s"] != workload.ClassStandard {
		t.Errorf("workload classes by job %v, want p performance and s standard", got)
	}
}

// TestV1PodKeepsTraceAffinity: a v3 trace's placement stays on the pod,
// and under the affinity intent a performance pod's every required term
// also excludes eco nodes.
func TestV1PodKeepsTraceAffinity(t *testing.T) {
	perf := traceJob("p", func(r map[string]any) {
		r["intentClass"] = workload.ClassPerformance
		r["podTemplate"] = map[string]any{
			"requests":     map[string]any{"cpu": "1"},
			"nodeSelector": map[string]any{"zone": "a"},
			"affinity": map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
				"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{
					map[string]any{"key": "kubernetes.io/hostname", "operator": "In", "values": []any{"n7"}},
				}}},
			}}},
		}
	})
	h := newV1FakeHarness(t, fakeNodes{}, []string{perf}, workload.RunOptions{Intent: workload.IntentAffinity})
	h.tick(0)
	h.step(1)
	jobs := h.createdJobs()
	if len(jobs) != 1 {
		t.Fatalf("%d Job objects, want 1", len(jobs))
	}
	spec := jobs[0].Spec.Template.Spec
	if spec.NodeSelector["zone"] != "a" {
		t.Errorf("nodeSelector %v lost the trace's zone=a", spec.NodeSelector)
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil || spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("affinity lost: %+v", spec.Affinity)
	}
	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 2 {
		t.Fatalf("required terms %+v, want the trace's term with NotIn eco ANDed in", terms)
	}
	host, eco := terms[0].MatchExpressions[0], terms[0].MatchExpressions[1]
	if host.Key != "kubernetes.io/hostname" || host.Values[0] != "n7" || eco.Key != workload.LabelPowerProfile || eco.Operator != corev1.NodeSelectorOpNotIn {
		t.Errorf("term %+v, want hostname In [n7] and %s NotIn [eco]", terms[0], workload.LabelPowerProfile)
	}
}

// TestV1DefaultSelectorPinsSpecNodes (X11): unset, the node selector pins
// every pod to the spec's nodes, next to the trace's own keys, and the
// pods tolerate the KWOK taint.
func TestV1DefaultSelectorPinsSpecNodes(t *testing.T) {
	job := traceJob("a", func(r map[string]any) {
		r["podTemplate"] = map[string]any{"requests": map[string]any{"cpu": "1"}, "nodeSelector": map[string]any{"zone": "a"}}
	})
	h := newV1FakeHarness(t, fakeNodes{}, []string{job}, workload.RunOptions{})
	h.tick(0)
	h.step(1)
	jobs := h.createdJobs()
	if len(jobs) != 1 {
		t.Fatalf("%d Job objects, want 1", len(jobs))
	}
	spec := jobs[0].Spec.Template.Spec
	if spec.NodeSelector[clusterspec.LabelCluster] != "c1" || spec.NodeSelector["zone"] != "a" {
		t.Errorf("nodeSelector %v, want %s=c1 and the trace's zone=a", spec.NodeSelector, clusterspec.LabelCluster)
	}
	kwok := workload.KWOKToleration()
	found := false
	for _, tol := range spec.Tolerations {
		found = found || (tol.Key == kwok.Key && tol.Value == kwok.Value && tol.Effect == kwok.Effect)
	}
	if !found {
		t.Errorf("tolerations %v lack the KWOK taint's", spec.Tolerations)
	}
}

// TestV1CompletionAnnotatesPodInsteadOfDeleting: a job whose work is done
// gets the completion annotation on its pod, which the KWOK Stage
// pod-complete selects; its pod is never deleted. job.completed follows
// the patch, with the run times, and the run then finishes.
func TestV1CompletionAnnotatesPodInsteadOfDeleting(t *testing.T) {
	short := traceJob("a", func(r map[string]any) { r["work"] = map[string]any{"cpuUnits": 5.0} })
	h := newV1FakeHarness(t, fakeNodes{"n1": fakeCPUNode()}, []string{short}, workload.RunOptions{})
	h.tick(0)
	h.step(1)
	h.runOn("a", "n1")
	for sec := 2.0; sec <= 10; sec++ {
		h.step(sec)
	}
	pod := h.pod("a")
	if pod.Annotations[workload.AnnotationComplete] != workload.CompleteValue || pod.Annotations[workload.AnnotationCompletedAt] == "" {
		t.Errorf("pod annotations %v, want %s=%s and %s", pod.Annotations, workload.AnnotationComplete, workload.CompleteValue, workload.AnnotationCompletedAt)
	}
	if n := h.countActions("delete", "pods"); n != 0 {
		t.Errorf("%d pod deletes; completion must annotate, not delete", n)
	}
	if n := h.countActions("patch", "pods"); n != 1 {
		t.Errorf("%d pod patches, want 1", n)
	}
	evs := h.events()
	done := named(evs, semconv.EventSimJobCompleted.Name, "a")
	if len(done) != 1 {
		t.Fatalf("%d completed events, want 1", len(done))
	}
	// Running from t=2; 5 units at 1 unit/s are done at t=7.
	if rt, _ := done[0].Attributes.Float(semconv.AttrSimJobRunTime); math.Abs(rt-5) > 1e-6 {
		t.Errorf("run time %v s, want 5", rt)
	}
	if ideal, _ := done[0].Attributes.Float(semconv.AttrSimJobIdealRunTime); math.Abs(ideal-5) > 1e-6 {
		t.Errorf("ideal run time %v s, want 5", ideal)
	}
	if st := h.run.status(); st.Phase != workload.PhaseFinished || st.Jobs.Completed != 1 {
		t.Errorf("status %+v, want finished with 1 completed", st)
	}
	if len(named(evs, semconv.EventSimRunFinished.Name, "")) != 1 {
		t.Error("no run.finished event")
	}
}

// TestV1GangDeadlockReportedNotCompleted: a gang of two 8-GPU pods on a
// cluster with one 8-GPU node never runs whole. Over 10 simulated minutes
// the running member is reported stalled (gang_incomplete) and so is the
// run (all_blocked), and nothing is completed. The legacy engine
// completed both by force after 120 s.
func TestV1GangDeadlockReportedNotCompleted(t *testing.T) {
	gang := func(id string) string {
		return traceJob(id, func(r map[string]any) {
			r["workloadId"], r["podRole"], r["gang"] = "g", "worker", true
			r["podTemplate"] = map[string]any{"requests": map[string]any{"cpu": "4", "nvidia.com/gpu": "8"}}
			r["work"] = map[string]any{"cpuUnits": 1000.0, "gpuUnits": 1000.0}
		})
	}
	h := newV1FakeHarness(t, fakeNodes{"gpu1": fakeGPUNode(&fakeTP{})}, []string{gang("g0"), gang("g1")}, workload.RunOptions{})
	h.tick(0)
	h.step(1)
	h.runOn("g0", "gpu1")
	h.unschedulable("g1")
	for sec := 10.0; sec <= 610; sec += 10 {
		h.step(sec)
	}
	evs := h.events()
	for _, name := range []string{semconv.EventSimJobCompleted.Name, semconv.EventSimJobFailed.Name} {
		if n := len(named(evs, name, "")); n != 0 {
			t.Errorf("%d %s events in a deadlock; nothing may be completed or failed", n, name)
		}
	}
	stalled := named(evs, semconv.EventSimJobStalled.Name, "g0")
	if len(stalled) != 1 {
		t.Fatalf("%d job.stalled events of g0, want 1", len(stalled))
	}
	if reason, _ := stalled[0].Attributes.String(semconv.AttrSimStallReason); reason != semconv.StallReasonGangIncomplete {
		t.Errorf("g0 stalled for %q, want %s", reason, semconv.StallReasonGangIncomplete)
	}
	if age, _ := stalled[0].Attributes.Float(semconv.AttrSimStallAge); age < workload.DefaultStallReport.Seconds() {
		t.Errorf("g0 stalled after %v s, want at least the %v report interval", age, workload.DefaultStallReport)
	}
	run := named(evs, semconv.EventSimRunStalled.Name, "")
	if len(run) != 1 {
		t.Fatalf("%d run.stalled events, want 1", len(run))
	}
	if reason, _ := run[0].Attributes.String(semconv.AttrSimStallReason); reason != semconv.StallReasonAllBlocked {
		t.Errorf("run stalled for %q, want %s", reason, semconv.StallReasonAllBlocked)
	}
	if j := h.job("g0"); j.sim.CPUUnitsRemaining != 1000 || j.sim.GPUUnitsRemaining != 1000 {
		t.Errorf("g0 progressed without its gang: %v CPU and %v GPU units left of 1000", j.sim.CPUUnitsRemaining, j.sim.GPUUnitsRemaining)
	}
	st := h.run.status()
	if st.Phase != workload.PhaseRunning || !st.RunStalled || st.Jobs.Stalled != 1 || st.Jobs.Running != 1 || st.Jobs.Pending != 1 {
		t.Errorf("status %+v, want running, stalled, 1 running and stalled, 1 pending", st)
	}
}

// TestV1RunStalledIgnoresNotDueJobs: while the only submitted job is done
// and the next is not due for 20 minutes, the run is idle, not stalled.
func TestV1RunStalledIgnoresNotDueJobs(t *testing.T) {
	first := traceJob("a", func(r map[string]any) { r["work"] = map[string]any{"cpuUnits": 5.0} })
	later := traceJob("b", func(r map[string]any) { r["submitTimeOffsetSec"] = 1200.0 })
	h := newV1FakeHarness(t, fakeNodes{"n1": fakeCPUNode()}, []string{first, later}, workload.RunOptions{})
	h.tick(0)
	h.step(1)
	h.runOn("a", "n1")
	for sec := 10.0; sec <= 1100; sec += 10 {
		h.step(sec)
	}
	evs := h.events()
	if len(named(evs, semconv.EventSimJobCompleted.Name, "a")) != 1 {
		t.Fatal("job a did not complete")
	}
	if n := len(named(evs, semconv.EventSimRunStalled.Name, "")); n != 0 {
		t.Errorf("%d run.stalled events while the remaining job is not due", n)
	}
	if st := h.run.status(); st.Jobs.NotDue != 1 || st.RunStalled || st.Phase != workload.PhaseRunning {
		t.Errorf("status %+v, want 1 not due, not stalled, running", st)
	}
}

// TestV1InjectHasNoPerTickCap: 50 jobs due at once are 50 creates in one
// tick; the legacy engine created 20 per tick.
func TestV1InjectHasNoPerTickCap(t *testing.T) {
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, traceJob(fmt.Sprintf("j%02d", i), nil))
	}
	h := newV1FakeHarness(t, fakeNodes{}, lines, workload.RunOptions{})
	h.tick(0)
	h.run.kube.process(context.Background())
	if n := h.countActions("create", "jobs"); n != 50 {
		t.Errorf("%d creates after the first tick, want 50", n)
	}
	h.tick(1)
	if st := h.run.status(); !st.AllSubmitted || st.Jobs.Pending != 50 {
		t.Errorf("status %+v, want all 50 submitted and pending", st)
	}
}

// TestV1ProgressUsesThroughputOfAssignedGPU: two 1-GPU jobs hold GPUs 0
// and 1; only GPU 1's controls cost throughput, so only the job on it
// slows. TestV1ProgressUsesEnforcedLimitOfAssignedGPU drives the same
// through a real limit.
func TestV1ProgressUsesThroughputOfAssignedGPU(t *testing.T) {
	gpuJob := func(id string) string {
		return traceJob(id, func(r map[string]any) {
			r["podTemplate"] = map[string]any{"requests": map[string]any{"cpu": "1", "nvidia.com/gpu": "1"}}
			r["work"] = map[string]any{"cpuUnits": 1000.0, "gpuUnits": 1000.0}
			r["workloadProfile"] = map[string]any{"cpuUtilization": 1.0, "gpuUtilization": 1.0, "cpuFeedIntensityGpu": 0.0}
		})
	}
	tp := &fakeTP{gpu: map[int]float64{1: 0.5}}
	h := newV1FakeHarness(t, fakeNodes{"gpu1": fakeGPUNode(tp)}, []string{gpuJob("a"), gpuJob("b")}, workload.RunOptions{})
	h.tick(0)
	h.step(1)
	h.runOn("a", "gpu1")
	h.step(2)
	h.runOn("b", "gpu1")
	for sec := 3.0; sec <= 12; sec++ {
		h.step(sec)
	}
	a, b := h.job("a"), h.job("b")
	// a started at 2 on GPU 0, b at 3 on GPU 1: 10 and 9 s of work.
	if math.Abs(a.workGPU-10) > 1e-6 {
		t.Errorf("job a on uncapped GPU 0 did %v GPU units in 10 s, want 10", a.workGPU)
	}
	if math.Abs(b.workGPU-4.5) > 1e-6 {
		t.Errorf("job b on GPU 1 at throughput 0.5 did %v GPU units in 9 s, want 4.5", b.workGPU)
	}
}

// TestV1PendingWithNodeDoesNotProgress: a pod bound to a node but not yet
// Running holds nothing and does no work.
func TestV1PendingWithNodeDoesNotProgress(t *testing.T) {
	h := newV1FakeHarness(t, fakeNodes{"n1": fakeCPUNode()}, []string{traceJob("a", nil)}, workload.RunOptions{})
	h.tick(0)
	h.step(1)
	h.bind("a", "n1")
	for sec := 2.0; sec <= 30; sec++ {
		h.step(sec)
	}
	j := h.job("a")
	if j.state != v1Scheduled || j.sim.CPUUnitsRemaining != 100 || j.workCPU != 0 {
		t.Errorf("bound, Pending job: state %d, %v units left, %v done; want scheduled with all 100 left", j.state, j.sim.CPUUnitsRemaining, j.workCPU)
	}
	h.run.mu.Lock()
	_, loaded := h.run.loads["n1"]
	h.run.mu.Unlock()
	if loaded {
		t.Error("a Pending pod loads its node")
	}
	if st := h.run.status(); st.Jobs.Scheduled != 1 || st.Jobs.Running != 0 {
		t.Errorf("status %+v, want 1 scheduled", st)
	}
}

// TestV1ScheduledCarriesClassOfNodeTwinByNodeName: a NodeTwin's object
// name is pkg/api.ObjectNameForNode of its node, which differs from the
// node name for a name with dots or capitals; spec.nodeName is the node.
// job.scheduled carries the class of the twin whose spec.nodeName is the
// pod's node. Before the fix the class was keyed by the object name, so a
// job on such a node got none.
func TestV1ScheduledCarriesClassOfNodeTwinByNodeName(t *testing.T) {
	const node = "Worker.example.org"
	h := newV1FakeHarness(t, fakeNodes{node: fakeCPUNode()}, []string{traceJob("a", nil)}, workload.RunOptions{})
	twin := v1alpha1.NodeTwin{ObjectMeta: metav1.ObjectMeta{Name: api.ObjectNameForNode(node)}}
	twin.Spec.NodeName = node
	twin.Status.SchedulableClass = "eco"
	if twin.Name == node {
		t.Fatalf("object name %q equals the node name; the test needs a node whose names differ", twin.Name)
	}
	h.run.kube.listTwins = func(context.Context) ([]v1alpha1.NodeTwin, error) { return []v1alpha1.NodeTwin{twin}, nil }
	h.run.kube.pollTwins(context.Background())
	h.tick(0)
	h.step(1)
	h.bind("a", node)
	h.step(2)
	sched := named(h.events(), semconv.EventSimJobScheduled.Name, "a")
	if len(sched) != 1 {
		t.Fatalf("%d job.scheduled events, want 1", len(sched))
	}
	if class, _ := sched[0].Attributes.String(semconv.AttrJoulieNodeSchedulableClass); class != "eco" {
		t.Errorf("job.scheduled on %s carries schedulable class %q, want eco from the NodeTwin %s", node, class, twin.Name)
	}
}
