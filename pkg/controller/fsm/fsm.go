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
//
// It reads jobs as pkg/job describes them and imports nothing from
// Kubernetes; pkg/job/kubejob builds those jobs from pods.
package fsm

import (
	"context"
	"fmt"
	"log"

	"github.com/matbun/joulie/pkg/controller/policy"
	"github.com/matbun/joulie/pkg/job"
)

const (
	ProfilePerformance = "performance"
	ProfileEco         = "eco"

	StateActivePerformance   = "ActivePerformance"
	StateDrainingPerformance = "DrainingPerformance"
	StateActiveEco           = "ActiveEco"
	StateUnknown             = "Unknown"

	PowerProfileLabelKey    = "joulie.io/power-profile"
	WorkloadClassAnnotation = "joulie.io/workload-class"
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

// IsPerformanceSensitive returns true if the job requires a performance
// (uncapped) node: its class excludes eco.
func IsPerformanceSensitive(j job.Job) bool {
	return j.Class == job.ClassPerformanceOnly
}

// CountPerformanceSensitiveJobs counts the performance-sensitive jobs that
// hold their node: not being deleted, and neither pending nor finished.
func CountPerformanceSensitiveJobs(jobs []job.Job) int {
	count := 0
	for _, j := range jobs {
		if j.Deleting {
			continue
		}
		if j.Phase == job.PhaseFinished || j.Phase == job.PhasePending {
			continue
		}
		if !IsPerformanceSensitive(j) {
			continue
		}
		count++
	}
	return count
}

// CountPerformanceDemand counts the performance-sensitive jobs that need a
// performance node, cluster-wide: running ones and pending ones. It is the
// demand queue_aware_v1 sizes the performance set from, so a queue of
// performance jobs waiting for a node raises it. CountPerformanceSensitiveJobs
// leaves pending jobs out because it answers a different question: whether a
// node still runs performance work and must keep draining.
func CountPerformanceDemand(jobs []job.Job) int {
	count := 0
	for _, j := range jobs {
		if j.Deleting {
			continue
		}
		if j.Phase == job.PhaseFinished {
			continue
		}
		if !IsPerformanceSensitive(j) {
			continue
		}
		count++
	}
	return count
}

// MultiGPUJobMinGPUs is the GPU units from which a job counts as a
// multi-GPU job (HasRunningMultiGPUJob).
const MultiGPUJobMinGPUs = 2

// HasRunningMultiGPUJob reports whether one of jobs runs, is not being
// deleted, and requests MultiGPUJobMinGPUs or more GPU units.
func HasRunningMultiGPUJob(jobs []job.Job) bool {
	for _, j := range jobs {
		if j.Deleting || j.Phase != job.PhaseRunning {
			continue
		}
		if j.GPUs >= MultiGPUJobMinGPUs {
			return true
		}
	}
	return false
}

// GuardedTransitionInfo returns a human-readable summary of why a node
// transition was guarded (for logging/debugging).
func GuardedTransitionInfo(nodeName string, perfPodCount int) string {
	return fmt.Sprintf("node=%s desired=eco draining=true reason=running-performance-sensitive-pods count=%d", nodeName, perfPodCount)
}
