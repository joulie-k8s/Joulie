// Package kubejob is the Kubernetes adapter of pkg/job: it builds the
// job.Job the policies read from a Pod. The controller manager's pod cache
// keeps exactly the Pod fields FromPod reads (cmd/controller-manager
// podTransform), so a field read here must be kept there too.
package kubejob

import (
	"strings"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/controller/fsm"
	"github.com/matbun/joulie/pkg/job"
	corev1 "k8s.io/api/core/v1"
)

// FromPod describes a pod as a job: its class from the workload-class
// annotation, nodeSelector and node affinity, its GPU units from the
// container resources, its phase from status.phase, and Deleting from
// deletionTimestamp.
func FromPod(p *corev1.Pod) job.Job {
	return job.Job{
		Class:    classify(p),
		GPUs:     gpuUnits(p),
		Phase:    phase(p.Status.Phase),
		Deleting: p.DeletionTimestamp != nil,
	}
}

// FromPods describes each pod as a job, in order.
func FromPods(pods []corev1.Pod) []job.Job {
	out := make([]job.Job, len(pods))
	for i := range pods {
		out[i] = FromPod(&pods[i])
	}
	return out
}

func phase(p corev1.PodPhase) job.Phase {
	switch p {
	case corev1.PodPending:
		return job.PhasePending
	case corev1.PodRunning:
		return job.PhaseRunning
	case corev1.PodSucceeded, corev1.PodFailed:
		return job.PhaseFinished
	default:
		return job.PhaseUnknown
	}
}

// classify reads a pod's class from its scheduling constraints. A pod that
// excludes eco is performance-only even when it also names eco, the
// conservative reading.
func classify(p *corev1.Pod) job.Class {
	if excludesEco(p) {
		return job.ClassPerformanceOnly
	}
	if ecoOnly(p) {
		return job.ClassEcoOnly
	}
	return job.ClassGeneral
}

// excludesEco returns true if the pod explicitly targets performance nodes
// via the workload-class annotation, nodeSelector, or node affinity.
func excludesEco(p *corev1.Pod) bool {
	if strings.TrimSpace(p.Annotations[fsm.WorkloadClassAnnotation]) == fsm.ProfilePerformance {
		return true
	}
	if strings.TrimSpace(p.Spec.NodeSelector[fsm.PowerProfileLabelKey]) == fsm.ProfilePerformance {
		return true
	}
	required := p.Spec.Affinity
	if required == nil || required.NodeAffinity == nil || required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key != fsm.PowerProfileLabelKey {
				continue
			}
			switch expr.Operator {
			case corev1.NodeSelectorOpNotIn:
				if containsString(expr.Values, fsm.ProfileEco) {
					return true
				}
			case corev1.NodeSelectorOpIn:
				if len(expr.Values) > 0 && !containsString(expr.Values, fsm.ProfileEco) {
					return true
				}
			}
		}
	}
	return false
}

// ecoOnly returns true if the pod explicitly targets only eco nodes.
func ecoOnly(p *corev1.Pod) bool {
	if strings.TrimSpace(p.Spec.NodeSelector[fsm.PowerProfileLabelKey]) == fsm.ProfileEco {
		return true
	}
	required := p.Spec.Affinity
	if required == nil || required.NodeAffinity == nil || required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key != fsm.PowerProfileLabelKey {
				continue
			}
			if expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 && !containsString(expr.Values, fsm.ProfilePerformance) && containsString(expr.Values, fsm.ProfileEco) {
				return true
			}
		}
	}
	return false
}

// gpuUnits is the number of GPU units a pod requests, of every GPU resource
// pkg/api classifies (joulie.GPUResourceKind): whole GPUs, MIG devices,
// partitions and shared replicas, one each. A container's request counts,
// else its limit. As the scheduler sizes a pod, the containers and the
// sidecars (init containers with restartPolicy Always) add up, and an init
// container that asks for more counts instead.
func gpuUnits(p *corev1.Pod) int64 {
	var sum, initMax int64
	for i := range p.Spec.Containers {
		sum += containerGPUUnits(&p.Spec.Containers[i])
	}
	for i := range p.Spec.InitContainers {
		c := &p.Spec.InitContainers[i]
		units := containerGPUUnits(c)
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sum += units
		} else if units > initMax {
			initMax = units
		}
	}
	return max(sum, initMax)
}

func containerGPUUnits(c *corev1.Container) int64 {
	var units int64
	seen := map[corev1.ResourceName]bool{}
	for _, list := range []corev1.ResourceList{c.Resources.Requests, c.Resources.Limits} {
		for name, q := range list {
			if seen[name] || joulie.GPUResourceKind(string(name)) == "" {
				continue
			}
			seen[name] = true
			units += q.Value()
		}
	}
	return units
}

func containsString(in []string, v string) bool {
	for _, x := range in {
		if strings.TrimSpace(x) == v {
			return true
		}
	}
	return false
}
