package fsm

import (
	"testing"

	"github.com/matbun/joulie/pkg/job"
)

// Only a running job that is not being deleted and requests two or more GPU
// units makes a node run a multi-GPU job; GPUs spread over several jobs do
// not.
func TestHasRunningMultiGPUJob(t *testing.T) {
	j := func(phase job.Phase, gpus int64, deleting bool) job.Job {
		return job.Job{Class: job.ClassGeneral, GPUs: gpus, Phase: phase, Deleting: deleting}
	}
	for _, tc := range []struct {
		name string
		jobs []job.Job
		want bool
	}{
		{"no job", nil, false},
		{"a running job with 2 GPUs", []job.Job{j(job.PhaseRunning, 2, false)}, true},
		{"a running job with 8 GPUs", []job.Job{j(job.PhaseRunning, 8, false)}, true},
		{"a running job with 1 GPU", []job.Job{j(job.PhaseRunning, 1, false)}, false},
		{"two running jobs with 1 GPU each", []job.Job{j(job.PhaseRunning, 1, false), j(job.PhaseRunning, 1, false)}, false},
		{"a pending job with 2 GPUs", []job.Job{j(job.PhasePending, 2, false)}, false},
		{"a finished job with 2 GPUs", []job.Job{j(job.PhaseFinished, 2, false)}, false},
		{"a job in an unknown phase with 2 GPUs", []job.Job{j(job.PhaseUnknown, 2, false)}, false},
		{"a running job with 2 GPUs being deleted", []job.Job{j(job.PhaseRunning, 2, true)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasRunningMultiGPUJob(tc.jobs); got != tc.want {
				t.Fatalf("HasRunningMultiGPUJob = %v, want %v", got, tc.want)
			}
		})
	}
}
