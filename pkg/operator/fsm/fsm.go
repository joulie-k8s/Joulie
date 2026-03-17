// Package fsm implements the node power-profile finite state machine.
//
// States:
//
//	ActivePerformance  - node running at full power
//	DrainingPerformance - node transitioning to eco, waiting for performance pods to drain
//	ActiveEco          - node running with power caps applied
//	Unknown            - node profile not yet determined
//
// The FSM enforces downgrade guards: a node cannot transition from performance
// to eco while performance-sensitive pods are still running on it.
package fsm

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/matbun/joulie/pkg/operator/policy"
	corev1 "k8s.io/api/core/v1"
)

const (
	ProfilePerformance = "performance"
	ProfileEco         = "eco"

	StateActivePerformance  = "ActivePerformance"
	StateDrainingPerformance = "DrainingPerformance"
	StateActiveEco          = "ActiveEco"
	StateUnknown            = "Unknown"

	PowerProfileLabelKey    = "joulie.io/power-profile"
	WorkloadClassAnnotation = "joulie.io/workload-class"

	WorkloadClassPerfOnly = "performance-only"
	WorkloadClassEcoOnly  = "eco-only"
	WorkloadClassGeneral  = "general"
)

// NodeOps abstracts Kubernetes operations needed by the FSM.
type NodeOps interface {
	// RunningPerformanceSensitivePodCount returns the number of running
	// performance-sensitive pods on the given node.
	RunningPerformanceSensitivePodCount(ctx context.Context, nodeName string) (int, error)
}

// AssignmentState maps a profile and draining flag to the FSM state name.
func AssignmentState(profile string, draining bool) string {
	if draining {
		return StateDrainingPerformance
	}
	switch profile {
	case ProfilePerformance:
		return StateActivePerformance
	case ProfileEco:
		return StateActiveEco
	default:
		return StateUnknown
	}
}

// CurrentProfileOrDefault normalises a profile string to one of the two known
// profiles or "unknown".
func CurrentProfileOrDefault(in string) string {
	if in == ProfilePerformance || in == ProfileEco {
		return in
	}
	return "unknown"
}

// ComputeDesiredLabels maps desired profile + running performance-sensitive pods
// to the effective profile and draining state.
//
// Rules:
//   - desired performance => (performance, draining=false)
//   - desired eco + perfPods>0 => (eco, draining=true)
//   - desired eco + perfPods=0 => (eco, draining=false)
func ComputeDesiredLabels(desiredProfile string, perfPods int) (string, bool) {
	switch CurrentProfileOrDefault(desiredProfile) {
	case ProfilePerformance:
		return ProfilePerformance, false
	case ProfileEco:
		return ProfileEco, perfPods > 0
	default:
		return "unknown", false
	}
}

// ApplyDowngradeGuards enforces the constraint that nodes with running
// performance-sensitive pods cannot immediately transition to eco. Instead,
// they enter the DrainingPerformance state until all such pods complete.
func ApplyDowngradeGuards(
	ctx context.Context,
	ops NodeOps,
	plan []policy.NodeAssignment,
	currentProfiles map[string]string,
) {
	for i := range plan {
		a := &plan[i]
		if a.Profile != ProfileEco {
			a.Profile, a.Draining = ComputeDesiredLabels(a.Profile, 0)
			a.State = AssignmentState(a.Profile, a.Draining)
			continue
		}
		count, err := ops.RunningPerformanceSensitivePodCount(ctx, a.NodeName)
		if err != nil {
			log.Printf("warning: cannot classify running pods on node=%s: %v", a.NodeName, err)
			continue
		}
		a.Profile, a.Draining = ComputeDesiredLabels(a.Profile, count)
		a.State = AssignmentState(a.Profile, a.Draining)
		if a.Draining {
			log.Printf("transition guarded node=%s desired=eco draining=true reason=running-performance-sensitive-pods count=%d", a.NodeName, count)
		}
	}
}

// IsPerformanceSensitivePod returns true if the pod's scheduling constraints
// indicate it requires a performance (uncapped) node.
func IsPerformanceSensitivePod(p *corev1.Pod) bool {
	return PodExcludesEco(p)
}

// ClassifyPodByScheduling classifies a pod based on its node selector and
// affinity expressions into one of: performance-only, eco-only, or general.
func ClassifyPodByScheduling(p *corev1.Pod) string {
	if PodExcludesEco(p) {
		return WorkloadClassPerfOnly
	}
	if PodIsEcoOnly(p) {
		return WorkloadClassEcoOnly
	}
	return WorkloadClassGeneral
}

// PodExcludesEco returns true if the pod explicitly targets performance nodes
// via the workload-class annotation, nodeSelector, or node affinity.
func PodExcludesEco(p *corev1.Pod) bool {
	if strings.TrimSpace(p.Annotations[WorkloadClassAnnotation]) == ProfilePerformance {
		return true
	}
	if strings.TrimSpace(p.Spec.NodeSelector[PowerProfileLabelKey]) == ProfilePerformance {
		return true
	}
	required := p.Spec.Affinity
	if required == nil || required.NodeAffinity == nil || required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key != PowerProfileLabelKey {
				continue
			}
			switch expr.Operator {
			case corev1.NodeSelectorOpNotIn:
				if containsString(expr.Values, ProfileEco) {
					return true
				}
			case corev1.NodeSelectorOpIn:
				if len(expr.Values) > 0 && !containsString(expr.Values, ProfileEco) {
					return true
				}
			}
		}
	}
	return false
}

// PodIsEcoOnly returns true if the pod explicitly targets only eco nodes.
func PodIsEcoOnly(p *corev1.Pod) bool {
	if strings.TrimSpace(p.Spec.NodeSelector[PowerProfileLabelKey]) == ProfileEco {
		return true
	}
	required := p.Spec.Affinity
	if required == nil || required.NodeAffinity == nil || required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	for _, term := range required.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key != PowerProfileLabelKey {
				continue
			}
			if expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 && !containsString(expr.Values, ProfilePerformance) && containsString(expr.Values, ProfileEco) {
				return true
			}
		}
	}
	return false
}

// CountPerformanceSensitivePods counts running, non-terminated pods that
// require performance nodes from a pod list.
func CountPerformanceSensitivePods(pods []corev1.Pod) int {
	count := 0
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodPending {
			continue
		}
		if !IsPerformanceSensitivePod(p) {
			continue
		}
		count++
	}
	return count
}

// GuardedTransitionInfo returns a human-readable summary of why a node
// transition was guarded (for logging/debugging).
func GuardedTransitionInfo(nodeName string, perfPodCount int) string {
	return fmt.Sprintf("node=%s desired=eco draining=true reason=running-performance-sensitive-pods count=%d", nodeName, perfPodCount)
}

func containsString(in []string, v string) bool {
	for _, x := range in {
		if strings.TrimSpace(x) == v {
			return true
		}
	}
	return false
}
