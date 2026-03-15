package fsm

import (
	"context"
	"fmt"
	"testing"

	"github.com/matbun/joulie/pkg/operator/policy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestPodExcludesEco(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "nodeSelector performance",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{PowerProfileLabelKey: ProfilePerformance},
				},
			},
			want: true,
		},
		{
			name: "nodeSelector eco",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{PowerProfileLabelKey: ProfileEco},
				},
			},
			want: false,
		},
		{
			name: "affinity NotIn eco",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{{
									MatchExpressions: []corev1.NodeSelectorRequirement{{
										Key:      PowerProfileLabelKey,
										Operator: corev1.NodeSelectorOpNotIn,
										Values:   []string{ProfileEco},
									}},
								}},
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "affinity In performance only",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{{
									MatchExpressions: []corev1.NodeSelectorRequirement{{
										Key:      PowerProfileLabelKey,
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{ProfilePerformance},
									}},
								}},
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "no constraints",
			pod:  &corev1.Pod{},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PodExcludesEco(tt.pod)
			if got != tt.want {
				t.Errorf("PodExcludesEco() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodIsEcoOnly(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "nodeSelector eco",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{PowerProfileLabelKey: ProfileEco},
				},
			},
			want: true,
		},
		{
			name: "nodeSelector performance",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{PowerProfileLabelKey: ProfilePerformance},
				},
			},
			want: false,
		},
		{
			name: "affinity In eco only",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{{
									MatchExpressions: []corev1.NodeSelectorRequirement{{
										Key:      PowerProfileLabelKey,
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{ProfileEco},
									}},
								}},
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "no constraints",
			pod:  &corev1.Pod{},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PodIsEcoOnly(tt.pod)
			if got != tt.want {
				t.Errorf("PodIsEcoOnly() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyPodByScheduling(t *testing.T) {
	perfPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{PowerProfileLabelKey: ProfilePerformance},
		},
	}
	ecoPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{PowerProfileLabelKey: ProfileEco},
		},
	}
	generalPod := &corev1.Pod{}

	if got := ClassifyPodByScheduling(perfPod); got != WorkloadClassPerfOnly {
		t.Errorf("expected %s, got %s", WorkloadClassPerfOnly, got)
	}
	if got := ClassifyPodByScheduling(ecoPod); got != WorkloadClassEcoOnly {
		t.Errorf("expected %s, got %s", WorkloadClassEcoOnly, got)
	}
	if got := ClassifyPodByScheduling(generalPod); got != WorkloadClassGeneral {
		t.Errorf("expected %s, got %s", WorkloadClassGeneral, got)
	}
}

func TestCountPerformanceSensitivePods(t *testing.T) {
	pods := []corev1.Pod{
		// Running perf pod
		{
			Spec: corev1.PodSpec{
				NodeSelector: map[string]string{PowerProfileLabelKey: ProfilePerformance},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		// Terminated perf pod (should not count)
		{
			ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &metav1.Time{}},
			Spec: corev1.PodSpec{
				NodeSelector: map[string]string{PowerProfileLabelKey: ProfilePerformance},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		// Succeeded perf pod (should not count)
		{
			Spec: corev1.PodSpec{
				NodeSelector: map[string]string{PowerProfileLabelKey: ProfilePerformance},
			},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
		},
		// Running general pod (should not count)
		{
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}

	got := CountPerformanceSensitivePods(pods)
	if got != 1 {
		t.Errorf("CountPerformanceSensitivePods() = %d, want 1", got)
	}
}
