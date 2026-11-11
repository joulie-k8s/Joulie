// Package job describes a job the way Joulie's policies read it: where it
// may run, how many GPU units it asks for, and whether it waits, runs or is
// done. It imports nothing from Kubernetes, so the node state machine
// (pkg/controller/fsm) works on any scheduler that can fill a Job; the
// Kubernetes adapter, which builds one from a Pod, is pkg/job/kubejob.
package job

// Class is where a job may run.
type Class string

const (
	// ClassPerformanceOnly: the job excludes eco nodes, so it holds a node
	// in performance while it runs and counts as performance demand.
	ClassPerformanceOnly Class = "performance-only"
	// ClassEcoOnly: the job targets eco nodes only.
	ClassEcoOnly Class = "eco-only"
	// ClassGeneral: the job runs on either profile.
	ClassGeneral Class = "general"
)

// Phase is where a job is in its life.
type Phase string

const (
	// PhaseUnknown is a job whose state the scheduler does not report, such
	// as a placed job on a node that stopped answering. It counts as holding
	// its node, not as running. It is the zero Phase.
	PhaseUnknown Phase = ""
	// PhasePending: submitted and waiting for a node.
	PhasePending Phase = "Pending"
	// PhaseRunning: placed on a node and running.
	PhaseRunning Phase = "Running"
	// PhaseFinished: succeeded or failed; it holds no node.
	PhaseFinished Phase = "Finished"
)

// Job is one job as the policies read it.
type Job struct {
	Class Class
	// GPUs is the number of GPU units the job requests: whole GPUs, MIG
	// devices, partitions and shared replicas, one each.
	GPUs  int64
	Phase Phase
	// Deleting marks a job that is being removed: whatever its phase, it
	// neither holds a node nor adds demand.
	Deleting bool
}
