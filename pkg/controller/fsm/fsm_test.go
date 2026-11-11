package fsm

import (
	"context"
	"fmt"
	"testing"

	"github.com/matbun/joulie/pkg/controller/policy"
	"github.com/matbun/joulie/pkg/job"
)

// mockNodeOps implements NodeOps for testing.
type mockNodeOps struct {
	counts map[string]int
	err    error
}

func (m *mockNodeOps) RunningPerformanceSensitivePodCount(_ context.Context, nodeName string) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	return m.counts[nodeName], nil
}

func TestAssignmentState(t *testing.T) {
	tests := []struct {
		profile  string
		draining bool
		want     string
	}{
		{ProfilePerformance, false, StateActivePerformance},
		{ProfileEco, false, StateActiveEco},
		{ProfilePerformance, true, StateDrainingPerformance},
		{ProfileEco, true, StateDrainingPerformance},
		{"unknown", false, StateUnknown},
		{"", false, StateUnknown},
	}
	for _, tt := range tests {
		got := AssignmentState(tt.profile, tt.draining)
		if got != tt.want {
			t.Errorf("AssignmentState(%q, %v) = %q, want %q", tt.profile, tt.draining, got, tt.want)
		}
	}
}

func TestCurrentProfileOrDefault(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{ProfilePerformance, ProfilePerformance},
		{ProfileEco, ProfileEco},
		{"unknown", "unknown"},
		{"", "unknown"},
		{"garbage", "unknown"},
	}
	for _, tt := range tests {
		got := CurrentProfileOrDefault(tt.in)
		if got != tt.want {
			t.Errorf("CurrentProfileOrDefault(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestComputeDesiredLabels(t *testing.T) {
	tests := []struct {
		desired     string
		perfPods    int
		wantProfile string
		wantDrain   bool
	}{
		{ProfilePerformance, 0, ProfilePerformance, false},
		{ProfilePerformance, 5, ProfilePerformance, false},
		{ProfileEco, 0, ProfileEco, false},
		{ProfileEco, 3, ProfileEco, true},
		{"unknown", 0, "unknown", false},
	}
	for _, tt := range tests {
		profile, drain := ComputeDesiredLabels(tt.desired, tt.perfPods)
		if profile != tt.wantProfile || drain != tt.wantDrain {
			t.Errorf("ComputeDesiredLabels(%q, %d) = (%q, %v), want (%q, %v)",
				tt.desired, tt.perfPods, profile, drain, tt.wantProfile, tt.wantDrain)
		}
	}
}

func TestApplyDowngradeGuards(t *testing.T) {
	ctx := context.Background()

	t.Run("eco with running perf pods enters draining", func(t *testing.T) {
		ops := &mockNodeOps{counts: map[string]int{"node-a": 2}}
		plan := []policy.NodeAssignment{
			{NodeName: "node-a", Profile: ProfileEco},
		}
		ApplyDowngradeGuards(ctx, ops, plan, map[string]string{"node-a": ProfilePerformance})
		if !plan[0].Draining {
			t.Error("expected node-a to be draining")
		}
		if plan[0].State != StateDrainingPerformance {
			t.Errorf("expected state %s, got %s", StateDrainingPerformance, plan[0].State)
		}
	})

	t.Run("eco with no perf pods transitions cleanly", func(t *testing.T) {
		ops := &mockNodeOps{counts: map[string]int{"node-a": 0}}
		plan := []policy.NodeAssignment{
			{NodeName: "node-a", Profile: ProfileEco},
		}
		ApplyDowngradeGuards(ctx, ops, plan, map[string]string{"node-a": ProfilePerformance})
		if plan[0].Draining {
			t.Error("expected node-a not to be draining")
		}
		if plan[0].State != StateActiveEco {
			t.Errorf("expected state %s, got %s", StateActiveEco, plan[0].State)
		}
	})

	t.Run("performance nodes are not guarded", func(t *testing.T) {
		ops := &mockNodeOps{counts: map[string]int{}}
		plan := []policy.NodeAssignment{
			{NodeName: "node-b", Profile: ProfilePerformance},
		}
		ApplyDowngradeGuards(ctx, ops, plan, map[string]string{"node-b": ProfilePerformance})
		if plan[0].Draining {
			t.Error("expected node-b not to be draining")
		}
		if plan[0].State != StateActivePerformance {
			t.Errorf("expected state %s, got %s", StateActivePerformance, plan[0].State)
		}
	})

	t.Run("error leaves plan entry unchanged", func(t *testing.T) {
		ops := &mockNodeOps{err: fmt.Errorf("api error")}
		plan := []policy.NodeAssignment{
			{NodeName: "node-c", Profile: ProfileEco},
		}
		ApplyDowngradeGuards(ctx, ops, plan, map[string]string{"node-c": ProfilePerformance})
		// On error, the entry should not be modified
		if plan[0].State != "" {
			t.Errorf("expected empty state on error, got %s", plan[0].State)
		}
	})
}

func TestIsPerformanceSensitive(t *testing.T) {
	for _, tt := range []struct {
		class job.Class
		want  bool
	}{
		{job.ClassPerformanceOnly, true},
		{job.ClassEcoOnly, false},
		{job.ClassGeneral, false},
		{"", false},
	} {
		if got := IsPerformanceSensitive(job.Job{Class: tt.class}); got != tt.want {
			t.Errorf("IsPerformanceSensitive(class %q) = %v, want %v", tt.class, got, tt.want)
		}
	}
}

func TestCountPerformanceSensitiveJobs(t *testing.T) {
	jobs := []job.Job{
		// Running perf job
		{Class: job.ClassPerformanceOnly, Phase: job.PhaseRunning},
		// Deleted perf job (should not count)
		{Class: job.ClassPerformanceOnly, Phase: job.PhaseRunning, Deleting: true},
		// Finished perf job (should not count)
		{Class: job.ClassPerformanceOnly, Phase: job.PhaseFinished},
		// Running general job (should not count)
		{Class: job.ClassGeneral, Phase: job.PhaseRunning},
		// Perf job in an unknown phase still holds its node
		{Class: job.ClassPerformanceOnly, Phase: job.PhaseUnknown},
	}

	got := CountPerformanceSensitiveJobs(jobs)
	if got != 2 {
		t.Errorf("CountPerformanceSensitiveJobs() = %d, want 2", got)
	}
}

func TestCountPerformanceSensitiveJobsPendingSkipped(t *testing.T) {
	// Regression: Pending jobs must not block draining.
	jobs := []job.Job{{Class: job.ClassPerformanceOnly, Phase: job.PhasePending}}
	got := CountPerformanceSensitiveJobs(jobs)
	if got != 0 {
		t.Errorf("Pending jobs should not count as performance-sensitive, got %d", got)
	}
}

// The queue-aware demand counts what still needs a performance node: running
// and pending performance jobs, not finished, deleting or general ones.
func TestCountPerformanceDemandIncludesPendingPods(t *testing.T) {
	perf := job.ClassPerformanceOnly
	jobs := []job.Job{
		{Class: perf, Phase: job.PhaseRunning},
		{Class: perf, Phase: job.PhasePending},
		{Class: perf, Phase: job.PhaseFinished},
		{Class: perf, Phase: job.PhasePending, Deleting: true},
		{Class: job.ClassGeneral, Phase: job.PhasePending},
	}
	if got := CountPerformanceDemand(jobs); got != 2 {
		t.Fatalf("CountPerformanceDemand() = %d, want 2 (one running, one pending)", got)
	}
	// The drain check over the same jobs still ignores the pending one.
	if got := CountPerformanceSensitiveJobs(jobs); got != 1 {
		t.Fatalf("CountPerformanceSensitiveJobs() = %d, want 1", got)
	}
}
