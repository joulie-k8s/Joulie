package kubejob

import (
	"testing"

	"github.com/matbun/joulie/pkg/controller/fsm"
	"github.com/matbun/joulie/pkg/job"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func required(op corev1.NodeSelectorOperator, values ...string) corev1.Pod {
	return corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
			{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: fsm.PowerProfileLabelKey, Operator: op, Values: values}}},
		}},
	}}}}
}

func requiredOr(lhs, rhs []string) corev1.Pod {
	return corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
			{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: fsm.PowerProfileLabelKey, Operator: corev1.NodeSelectorOpIn, Values: lhs}}},
			{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: fsm.PowerProfileLabelKey, Operator: corev1.NodeSelectorOpIn, Values: rhs}}},
		}},
	}}}}
}

func selector(profile string) corev1.Pod {
	return corev1.Pod{Spec: corev1.PodSpec{NodeSelector: map[string]string{fsm.PowerProfileLabelKey: profile}}}
}

// A pod's class comes from the workload-class annotation, the nodeSelector
// and the required node affinity on joulie.io/power-profile.
func TestFromPodClass(t *testing.T) {
	contradicting := selector(fsm.ProfilePerformance)
	contradicting.Spec.Affinity = required(corev1.NodeSelectorOpIn, fsm.ProfileEco).Spec.Affinity
	annotated := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{fsm.WorkloadClassAnnotation: " performance "}}}
	annotatedStandard := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{fsm.WorkloadClassAnnotation: "standard"}}}

	for _, tt := range []struct {
		name string
		pod  corev1.Pod
		want job.Class
	}{
		{"no constraints is general", corev1.Pod{}, job.ClassGeneral},
		{"annotation performance", annotated, job.ClassPerformanceOnly},
		{"annotation standard is general", annotatedStandard, job.ClassGeneral},
		{"node selector performance only", selector(fsm.ProfilePerformance), job.ClassPerformanceOnly},
		{"node selector eco only", selector(fsm.ProfileEco), job.ClassEcoOnly},
		{"unknown node selector value is general", selector("ultra"), job.ClassGeneral},
		{"required affinity in performance", required(corev1.NodeSelectorOpIn, fsm.ProfilePerformance), job.ClassPerformanceOnly},
		{"required affinity in eco", required(corev1.NodeSelectorOpIn, fsm.ProfileEco), job.ClassEcoOnly},
		{"required affinity in both is general", required(corev1.NodeSelectorOpIn, fsm.ProfileEco, fsm.ProfilePerformance), job.ClassGeneral},
		{"or terms perf or eco treated as performance-only by conservative rule", requiredOr([]string{fsm.ProfilePerformance}, []string{fsm.ProfileEco}), job.ClassPerformanceOnly},
		{"notin eco means performance only", required(corev1.NodeSelectorOpNotIn, fsm.ProfileEco), job.ClassPerformanceOnly},
		{"notin performance is general", required(corev1.NodeSelectorOpNotIn, fsm.ProfilePerformance), job.ClassGeneral},
		{"does-not-exist on power-profile is general", required(corev1.NodeSelectorOpDoesNotExist), job.ClassGeneral},
		{"gt operator on power-profile is general", required(corev1.NodeSelectorOpGt, "1"), job.ClassGeneral},
		{"contradicting node selector and affinity is performance-only", contradicting, job.ClassPerformanceOnly},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := FromPod(&tt.pod).Class; got != tt.want {
				t.Fatalf("class = %q, want %q", got, tt.want)
			}
		})
	}
}

// Every pod phase maps to a job phase, and a deletion timestamp to Deleting.
func TestFromPodPhase(t *testing.T) {
	for _, tt := range []struct {
		in   corev1.PodPhase
		want job.Phase
	}{
		{corev1.PodPending, job.PhasePending},
		{corev1.PodRunning, job.PhaseRunning},
		{corev1.PodSucceeded, job.PhaseFinished},
		{corev1.PodFailed, job.PhaseFinished},
		{corev1.PodUnknown, job.PhaseUnknown},
		{"", job.PhaseUnknown},
	} {
		p := corev1.Pod{Status: corev1.PodStatus{Phase: tt.in}}
		if got := FromPod(&p); got.Phase != tt.want || got.Deleting {
			t.Errorf("pod phase %q: job %+v, want phase %q and not deleting", tt.in, got, tt.want)
		}
	}
	now := metav1.Now()
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if got := FromPod(&p); !got.Deleting || got.Phase != job.PhaseRunning {
		t.Fatalf("deleting running pod: job %+v", got)
	}
}

func TestFromPodsKeepsOrder(t *testing.T) {
	pods := []corev1.Pod{selector(fsm.ProfilePerformance), selector(fsm.ProfileEco), {}}
	got := FromPods(pods)
	want := []job.Class{job.ClassPerformanceOnly, job.ClassEcoOnly, job.ClassGeneral}
	if len(got) != len(want) {
		t.Fatalf("FromPods returned %d jobs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Class != want[i] {
			t.Errorf("job %d class %q, want %q", i, got[i].Class, want[i])
		}
	}
}

func gpuContainer(requests, limits map[string]string) corev1.Container {
	list := func(in map[string]string) corev1.ResourceList {
		if in == nil {
			return nil
		}
		out := corev1.ResourceList{}
		for k, v := range in {
			out[corev1.ResourceName(k)] = resource.MustParse(v)
		}
		return out
	}
	return corev1.Container{Name: "c", Resources: corev1.ResourceRequirements{Requests: list(requests), Limits: list(limits)}}
}

func sidecar(c corev1.Container) corev1.Container {
	always := corev1.ContainerRestartPolicyAlways
	c.RestartPolicy = &always
	return c
}

// A pod's GPU units count every GPU resource kind pkg/api classifies, one
// per unit, from the request or else the limit, the way the scheduler sizes
// a pod: containers and sidecars add up, a larger init container counts
// instead.
func TestFromPodGPUs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		init, main []corev1.Container
		want       int64
	}{
		{"no containers", nil, nil, 0},
		{"cpu and memory only", nil, []corev1.Container{gpuContainer(map[string]string{"cpu": "4", "memory": "8Gi"}, nil)}, 0},
		{"whole GPUs requested", nil, []corev1.Container{gpuContainer(map[string]string{"nvidia.com/gpu": "2"}, nil)}, 2},
		{"whole GPUs as a limit only", nil, []corev1.Container{gpuContainer(nil, map[string]string{"amd.com/gpu": "2"})}, 2},
		{"request and limit count once", nil, []corev1.Container{gpuContainer(map[string]string{"nvidia.com/gpu": "2"}, map[string]string{"nvidia.com/gpu": "2"})}, 2},
		{"MIG devices", nil, []corev1.Container{gpuContainer(map[string]string{"nvidia.com/mig-1g.10gb": "3"}, nil)}, 3},
		{"shared replicas", nil, []corev1.Container{gpuContainer(nil, map[string]string{"nvidia.com/gpu.shared": "2"})}, 2},
		{"AMD partitions", nil, []corev1.Container{gpuContainer(nil, map[string]string{"amd.com/cpx_nps4": "2"})}, 2},
		{"two containers add up", nil, []corev1.Container{
			gpuContainer(nil, map[string]string{"gpu.intel.com/xe": "1"}),
			gpuContainer(nil, map[string]string{"nvidia.com/gpu": "1"}),
		}, 2},
		{"Intel share and monitoring resources are not GPUs", nil, []corev1.Container{
			gpuContainer(map[string]string{"gpu.intel.com/millicores": "500", "gpu.intel.com/i915_monitoring": "1"}, nil),
		}, 0},
		{"a larger init container counts instead", []corev1.Container{gpuContainer(nil, map[string]string{"nvidia.com/gpu": "4"})},
			[]corev1.Container{gpuContainer(nil, map[string]string{"nvidia.com/gpu": "1"})}, 4},
		{"a smaller init container does not add", []corev1.Container{gpuContainer(nil, map[string]string{"nvidia.com/gpu": "1"})},
			[]corev1.Container{gpuContainer(nil, map[string]string{"nvidia.com/gpu": "1"})}, 1},
		{"a sidecar adds up", []corev1.Container{sidecar(gpuContainer(nil, map[string]string{"nvidia.com/gpu": "1"}))},
			[]corev1.Container{gpuContainer(nil, map[string]string{"nvidia.com/gpu": "1"})}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &corev1.Pod{Spec: corev1.PodSpec{InitContainers: tc.init, Containers: tc.main}}
			if got := FromPod(p).GPUs; got != tc.want {
				t.Fatalf("GPUs = %d, want %d", got, tc.want)
			}
		})
	}
}

// A running pod with two GPUs makes its node run a multi-GPU job; the same
// pod pending, or being deleted, does not.
func TestFromPodFeedsMultiGPURule(t *testing.T) {
	pod := func(phase corev1.PodPhase, deleting bool) corev1.Pod {
		p := corev1.Pod{
			Spec:   corev1.PodSpec{Containers: []corev1.Container{gpuContainer(nil, map[string]string{"nvidia.com/gpu": "2"})}},
			Status: corev1.PodStatus{Phase: phase},
		}
		if deleting {
			now := metav1.Now()
			p.DeletionTimestamp = &now
		}
		return p
	}
	if !fsm.HasRunningMultiGPUJob(FromPods([]corev1.Pod{pod(corev1.PodRunning, false)})) {
		t.Error("running pod with 2 GPUs: no multi-GPU job")
	}
	if fsm.HasRunningMultiGPUJob(FromPods([]corev1.Pod{pod(corev1.PodPending, false), pod(corev1.PodRunning, true)})) {
		t.Error("pending or deleting pod with 2 GPUs counted as a running multi-GPU job")
	}
}
