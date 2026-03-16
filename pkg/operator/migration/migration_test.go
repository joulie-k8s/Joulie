package migration

import (
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
)

func TestEvaluateNodeNoStress(t *testing.T) {
	ts := joulie.NodeTwinStatus{
		SchedulableClass:            "performance",
		PredictedCoolingStressScore: 30,
		PredictedPsuStressScore:     20,
	}
	workloads := []WorkloadOnNode{
		{
			Ref: joulie.WorkloadRef{Kind: "Job", Namespace: "default", Name: "job1"},
			Profile: joulie.WorkloadProfileStatus{
				Migratability: joulie.WorkloadMigratability{Reschedulable: true},
				Criticality:   joulie.WorkloadCriticality{Class: "standard"},
			},
		},
	}
	recs := EvaluateNode(ts, workloads, DefaultPolicy())
	if len(recs) != 0 {
		t.Errorf("expected no recommendations under low stress, got %d", len(recs))
	}
}

func TestEvaluateNodeHighStress(t *testing.T) {
	ts := joulie.NodeTwinStatus{
		SchedulableClass:            "eco",
		PredictedCoolingStressScore: 80,
		PredictedPsuStressScore:     75,
	}
	workloads := []WorkloadOnNode{
		{
			Ref: joulie.WorkloadRef{Kind: "Job", Namespace: "default", Name: "reschedulable-job"},
			Profile: joulie.WorkloadProfileStatus{
				Migratability: joulie.WorkloadMigratability{Reschedulable: true},
				Criticality:   joulie.WorkloadCriticality{Class: "standard"},
			},
		},
		{
			Ref: joulie.WorkloadRef{Kind: "Job", Namespace: "default", Name: "non-reschedulable"},
			Profile: joulie.WorkloadProfileStatus{
				Migratability: joulie.WorkloadMigratability{Reschedulable: false},
				Criticality:   joulie.WorkloadCriticality{Class: "performance"},
			},
		},
	}
	recs := EvaluateNode(ts, workloads, DefaultPolicy())
	if len(recs) != 1 {
		t.Errorf("expected 1 recommendation (only reschedulable), got %d", len(recs))
	}
	if recs[0].WorkloadRef.Name != "reschedulable-job" {
		t.Errorf("expected reschedulable-job, got %s", recs[0].WorkloadRef.Name)
	}
}

func TestEvaluateNodeDraining(t *testing.T) {
	ts := joulie.NodeTwinStatus{
		SchedulableClass:            "draining",
		PredictedCoolingStressScore: 10,
		PredictedPsuStressScore:     10,
	}
	workloads := []WorkloadOnNode{
		{
			Ref: joulie.WorkloadRef{Kind: "Pod", Name: "any-pod"},
			Profile: joulie.WorkloadProfileStatus{
				Migratability: joulie.WorkloadMigratability{Reschedulable: true},
			},
		},
	}
	recs := EvaluateNode(ts, workloads, DefaultPolicy())
	if len(recs) != 1 {
		t.Errorf("expected recommendation on draining node, got %d", len(recs))
	}
}
