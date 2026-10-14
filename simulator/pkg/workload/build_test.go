package workload_test

import (
	"reflect"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

func gpuTemplate() workload.Template {
	return workload.Template{
		Namespace: "sim", WorkloadID: "wl-7", WorkloadType: "Distributed_Training", PodRole: "worker",
		Class: workload.ClassPerformance, Gang: true, SubmitOffsetSec: 30,
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("8"),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
			"nvidia.com/gpu":      resource.MustParse("2"),
		},
		Placement: workload.Placement{NodeSelector: map[string]string{"pool": "gpu"}},
		Profile:   workload.Profile{CPUUtil: 0.95, GPUUtil: 0.8, MemoryIntensity: 0.45, IOIntensity: 0.1},
	}
}

// A group of three becomes one Indexed Job object of three pods that never
// retries, named and labelled for the run, listing its jobIds in completion
// index order, with a pod template that carries the class, the group key and
// the profile, requests with GPU limits, and the merged placement.
func TestBuildJobShape(t *testing.T) {
	g := workload.Group{Key: "wl-7/worker@30", Template: gpuTemplate(), JobIDs: []string{"wl-7-worker-01", "wl-7-worker-02", "wl-7-worker-03"}}
	o := workload.BuildOptions{
		RunID: "run-a", Image: "example.org/pause:1", JobTTL: 10 * time.Minute,
		NodeSelector: workload.DefaultNodeSelector("smoke"),
		Tolerations:  []corev1.Toleration{workload.KWOKToleration()},
	}
	job := workload.BuildJob(g, o)

	if job.Name != workload.ObjectName("run-a", g.Key) || job.Namespace != "sim" {
		t.Errorf("name %s/%s", job.Namespace, job.Name)
	}
	wantLabels := map[string]string{
		workload.LabelPartOf: workload.PartOfWorkload, workload.LabelRunID: workload.RunLabelValue("run-a"),
		workload.LabelWorkloadType: "distributed-training", workload.LabelPodRole: "worker",
	}
	if !reflect.DeepEqual(job.Labels, wantLabels) || !reflect.DeepEqual(job.Spec.Template.Labels, wantLabels) {
		t.Errorf("labels %v and template labels %v, want %v", job.Labels, job.Spec.Template.Labels, wantLabels)
	}
	if job.Annotations[workload.AnnotationRun] != "run-a" || job.Annotations[workload.AnnotationJobIDs] != "wl-7-worker-01,wl-7-worker-02,wl-7-worker-03" {
		t.Errorf("annotations %v", job.Annotations)
	}
	s := job.Spec
	if *s.Completions != 3 || *s.Parallelism != 3 || *s.CompletionMode != batchv1.IndexedCompletion || *s.BackoffLimit != 0 {
		t.Errorf("completions %d parallelism %d mode %s backoff %d", *s.Completions, *s.Parallelism, *s.CompletionMode, *s.BackoffLimit)
	}
	if s.TTLSecondsAfterFinished == nil || *s.TTLSecondsAfterFinished != 600 {
		t.Errorf("ttlSecondsAfterFinished %v, want 600", s.TTLSecondsAfterFinished)
	}

	ann := s.Template.Annotations
	wantAnn := map[string]string{
		workload.AnnotationGroup: "wl-7/worker@30", workload.AnnotationWorkloadClass: workload.ClassPerformance,
		workload.AnnotationCPUUtilPct: "95.0", workload.AnnotationGPUUtilPct: "80.0",
		workload.AnnotationMemoryPressurePct: "45.0", workload.AnnotationIOIntensity: "0.10",
	}
	if !reflect.DeepEqual(ann, wantAnn) {
		t.Errorf("pod annotations %v, want %v", ann, wantAnn)
	}
	ps := s.Template.Spec
	if ps.RestartPolicy != corev1.RestartPolicyNever || len(ps.Containers) != 1 {
		t.Fatalf("restartPolicy %s, %d containers", ps.RestartPolicy, len(ps.Containers))
	}
	c := ps.Containers[0]
	if c.Name != workload.ContainerName || c.Image != "example.org/pause:1" {
		t.Errorf("container %s image %s", c.Name, c.Image)
	}
	if q := c.Resources.Requests[corev1.ResourceCPU]; q.Cmp(resource.MustParse("8")) != 0 || len(c.Resources.Requests) != 3 {
		t.Errorf("requests %v", c.Resources.Requests)
	}
	if q := c.Resources.Limits["nvidia.com/gpu"]; len(c.Resources.Limits) != 1 || q.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("limits %v, want only the GPU at its request", c.Resources.Limits)
	}
	if !reflect.DeepEqual(ps.NodeSelector, map[string]string{"pool": "gpu", "sim.joulie.io/cluster": "smoke"}) {
		t.Errorf("nodeSelector %v", ps.NodeSelector)
	}
	if len(ps.Tolerations) != 1 || ps.Tolerations[0] != workload.KWOKToleration() {
		t.Errorf("tolerations %v", ps.Tolerations)
	}
	if ps.Affinity != nil {
		t.Errorf("annotation intent rendered an affinity %+v", ps.Affinity)
	}
}

// A group of one is a non-indexed Job object; without a TTL, an image or a
// namespace the defaults apply, and an empty workload type or role leaves
// its label out, as legacy does.
func TestBuildJobDefaults(t *testing.T) {
	tpl := gpuTemplate()
	tpl.Namespace, tpl.WorkloadType, tpl.PodRole = "", "", ""
	job := workload.BuildJob(workload.Group{Key: "solo", Template: tpl, JobIDs: []string{"solo"}}, workload.BuildOptions{RunID: "r"})
	if *job.Spec.CompletionMode != batchv1.NonIndexedCompletion || *job.Spec.Completions != 1 {
		t.Errorf("mode %s completions %d", *job.Spec.CompletionMode, *job.Spec.Completions)
	}
	if job.Spec.TTLSecondsAfterFinished != nil || job.Namespace != "default" || job.Spec.Template.Spec.Containers[0].Image != workload.DefaultImage {
		t.Errorf("ttl %v namespace %q image %q", job.Spec.TTLSecondsAfterFinished, job.Namespace, job.Spec.Template.Spec.Containers[0].Image)
	}
	if _, ok := job.Labels[workload.LabelWorkloadType]; ok {
		t.Errorf("labels %v carry an empty workload type", job.Labels)
	}
	if _, ok := job.Labels[workload.LabelPodRole]; ok {
		t.Errorf("labels %v carry an empty pod role", job.Labels)
	}
	o := workload.BuildOptions{RunID: "r", Namespace: "override"}
	if job := workload.BuildJob(workload.Group{Key: "solo", Template: gpuTemplate(), JobIDs: []string{"solo"}}, o); job.Namespace != "override" {
		t.Errorf("namespace option ignored: %q", job.Namespace)
	}
}

// Each intent renders what it says: the class annotation for annotation and
// both (and for the zero intent, which is the default), the NotIn-eco
// affinity for a performance pod under affinity and both, and nothing under
// none.
func TestBuildIntentRendering(t *testing.T) {
	cases := []struct {
		intent       workload.Intent
		annotation   bool
		perfAffinity bool
	}{
		{"", true, false},
		{workload.IntentAnnotation, true, false},
		{workload.IntentAffinity, false, true},
		{workload.IntentBoth, true, true},
		{workload.IntentNone, false, false},
	}
	for _, c := range cases {
		for _, class := range []string{workload.ClassPerformance, workload.ClassStandard} {
			tpl := gpuTemplate()
			tpl.Class = class
			job := workload.BuildJob(workload.Group{Key: "k", Template: tpl, JobIDs: []string{"j"}}, workload.BuildOptions{RunID: "r", Intent: c.intent})
			ps := job.Spec.Template
			v, ok := ps.Annotations[workload.AnnotationWorkloadClass]
			if ok != c.annotation || (ok && v != class) {
				t.Errorf("intent %q class %s: class annotation %q present=%v, want present=%v", c.intent, class, v, ok, c.annotation)
			}
			wantAff := c.perfAffinity && class == workload.ClassPerformance
			if gotAff := ps.Spec.Affinity != nil; gotAff != wantAff {
				t.Errorf("intent %q class %s: affinity %+v, want present=%v", c.intent, class, ps.Spec.Affinity, wantAff)
			}
		}
	}
}

// The trace's placement is kept: its selector value wins over an option for
// the same key, and an option's toleration the trace already has is not
// added twice.
func TestBuildKeepsTracePlacement(t *testing.T) {
	tpl := gpuTemplate()
	tpl.Placement = workload.Placement{
		Affinity:     traceAffinity(),
		NodeSelector: map[string]string{"sim.joulie.io/cluster": "trace-choice", "pool": "gpu"},
		Tolerations:  []corev1.Toleration{workload.KWOKToleration(), {Key: "dedicated", Operator: corev1.TolerationOpExists}},
	}
	tpl.Class = workload.ClassStandard
	o := workload.BuildOptions{RunID: "r", NodeSelector: map[string]string{"sim.joulie.io/cluster": "smoke", "extra": "1"}, Tolerations: []corev1.Toleration{workload.KWOKToleration()}}
	ps := workload.BuildJob(workload.Group{Key: "k", Template: tpl, JobIDs: []string{"j"}}, o).Spec.Template.Spec
	if want := map[string]string{"sim.joulie.io/cluster": "trace-choice", "pool": "gpu", "extra": "1"}; !reflect.DeepEqual(ps.NodeSelector, want) {
		t.Errorf("nodeSelector %v, want %v", ps.NodeSelector, want)
	}
	if len(ps.Tolerations) != 2 {
		t.Errorf("tolerations %v, want the trace's two", ps.Tolerations)
	}
	if !reflect.DeepEqual(ps.Affinity, traceAffinity()) {
		t.Errorf("affinity changed for a standard pod: %+v", ps.Affinity)
	}
}

// Under OwnerPod each job is a bare pod named by its jobId, carrying its
// jobId and the run id, otherwise identical to the pod a Job object of that
// job alone would create.
func TestBuildPodOwnerPod(t *testing.T) {
	j := workload.JobSpec{JobID: "wl-7-worker-02", Template: gpuTemplate()}
	o := workload.BuildOptions{RunID: "run-a"}
	pod := workload.BuildPod(j, o)
	if pod.Name != workload.ObjectName("run-a", "wl-7-worker-02") || pod.Namespace != "sim" {
		t.Errorf("pod %s/%s", pod.Namespace, pod.Name)
	}
	if pod.Annotations[workload.AnnotationJobID] != "wl-7-worker-02" || pod.Annotations[workload.AnnotationRun] != "run-a" ||
		pod.Annotations[workload.AnnotationGroup] != "wl-7/worker@30" {
		t.Errorf("annotations %v", pod.Annotations)
	}
	tpl := workload.BuildJob(workload.Group{Key: "wl-7/worker@30", Template: j.Template, JobIDs: []string{j.JobID}}, o).Spec.Template
	if !reflect.DeepEqual(pod.Spec, tpl.Spec) || !reflect.DeepEqual(pod.Labels, tpl.Labels) {
		t.Errorf("bare pod differs from the Job object's pod:\n%+v\n%+v", pod.Spec, tpl.Spec)
	}
}

// Jobs of one workload, role and offset share a Job object only while their
// pods are the same: a different request splits them into <key>#2, reported
// once; a quantity written another way ("1" and "1000m") is the same; a
// different offset is simply another group; order is kept throughout.
func TestGroupJobsSplitsDifferingPods(t *testing.T) {
	mk := func(id string, offset float64, cpu string) workload.JobSpec {
		tpl := gpuTemplate()
		tpl.SubmitOffsetSec = offset
		tpl.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}
		return workload.JobSpec{JobID: id, Template: tpl}
	}
	jobs := []workload.JobSpec{
		mk("a1", 30, "1"), mk("a2", 30, "1000m"), mk("a3", 30, "2"), mk("a4", 30, "1"),
		mk("b1", 60, "1"),
		{JobID: "solo", Template: workload.Template{Class: workload.ClassStandard}},
	}
	groups, split := workload.GroupJobs(jobs)
	type g struct {
		Key string
		IDs []string
	}
	var got []g
	for _, gr := range groups {
		got = append(got, g{gr.Key, gr.JobIDs})
	}
	want := []g{
		{"wl-7/worker@30", []string{"a1", "a2", "a4"}},
		{"wl-7/worker@30#2", []string{"a3"}},
		{"wl-7/worker@60", []string{"b1"}},
		{"solo", []string{"solo"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groups %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(split, []string{"wl-7/worker@30"}) {
		t.Errorf("split %v", split)
	}
	if name1, name2 := workload.ObjectName("r", groups[0].Key), workload.ObjectName("r", groups[1].Key); name1 == name2 {
		t.Errorf("split groups share the name %s", name1)
	}
}
