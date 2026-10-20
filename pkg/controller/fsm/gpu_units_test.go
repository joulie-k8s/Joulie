package fsm

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
func TestPodGPUUnits(t *testing.T) {
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
			if got := PodGPUUnits(p); got != tc.want {
				t.Fatalf("PodGPUUnits = %d, want %d", got, tc.want)
			}
		})
	}
}

// Only a running pod that is not being deleted and requests two or more GPU
// units makes a node run a multi-GPU pod; GPUs spread over several pods do
// not.
func TestHasRunningMultiGPUPod(t *testing.T) {
	pod := func(phase corev1.PodPhase, gpus string, deleting bool) corev1.Pod {
		p := corev1.Pod{
			Spec:   corev1.PodSpec{Containers: []corev1.Container{gpuContainer(nil, map[string]string{"nvidia.com/gpu": gpus})}},
			Status: corev1.PodStatus{Phase: phase},
		}
		if deleting {
			now := metav1.Now()
			p.DeletionTimestamp = &now
		}
		return p
	}
	for _, tc := range []struct {
		name string
		pods []corev1.Pod
		want bool
	}{
		{"no pod", nil, false},
		{"a running pod with 2 GPUs", []corev1.Pod{pod(corev1.PodRunning, "2", false)}, true},
		{"a running pod with 8 GPUs", []corev1.Pod{pod(corev1.PodRunning, "8", false)}, true},
		{"a running pod with 1 GPU", []corev1.Pod{pod(corev1.PodRunning, "1", false)}, false},
		{"two running pods with 1 GPU each", []corev1.Pod{pod(corev1.PodRunning, "1", false), pod(corev1.PodRunning, "1", false)}, false},
		{"a pending pod with 2 GPUs", []corev1.Pod{pod(corev1.PodPending, "2", false)}, false},
		{"a finished pod with 2 GPUs", []corev1.Pod{pod(corev1.PodSucceeded, "2", false), pod(corev1.PodFailed, "2", false)}, false},
		{"a running pod with 2 GPUs being deleted", []corev1.Pod{pod(corev1.PodRunning, "2", true)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasRunningMultiGPUPod(tc.pods); got != tc.want {
				t.Fatalf("HasRunningMultiGPUPod = %v, want %v", got, tc.want)
			}
		})
	}
}
