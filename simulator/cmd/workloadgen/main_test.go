package main

import (
	"math/rand"
	"testing"
)

func TestSampleLogicalWorkloadDistributedHasMultiplePods(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	cfg := generatorConfig{
		Namespace:        "default",
		GPURatio:         1,
		CPURatePerCore:   1,
		GPURatePerGPU:    1,
		MeanInterArrival: 1,
	}
	found := false
	for i := 0; i < 100; i++ {
		wl := sampleLogicalWorkload(rng, i+1, float64(i), cfg)
		if wl.Type != "distributed_training" && wl.Type != "parameter_server_training" && wl.Type != "hpo_experiment" {
			continue
		}
		found = true
		if len(wl.Pods) < 2 {
			t.Fatalf("expected multi-pod workload for %s, got %d pods", wl.Type, len(wl.Pods))
		}
		if wl.Type == "distributed_training" && !wl.Gang {
			t.Fatalf("expected distributed training workload to be gang-scheduled")
		}
		break
	}
	if !found {
		t.Fatalf("did not sample a multi-pod workload in 100 attempts")
	}
}

func TestExpandLogicalWorkloadAddsWorkloadMetadata(t *testing.T) {
	wl := logicalWorkload{
		ID:        "workload-1",
		Type:      "distributed_training",
		Namespace: "default",
		Gang:      true,
		CPUClass:  "cpu.mixed",
		GPUClass:  "gpu.compute_bound",
		Profile: workloadProfileRec{
			CPUUtilization: 0.3,
			GPUUtilization: 0.5,
		},
		Pods: []logicalPod{{
			Role:        "worker",
			Replicas:    2,
			CPURequest:  4,
			MemoryGiB:   8,
			GPURequest:  1,
			GPUResource: "nvidia.com/gpu",
			CPUUnits:    100,
			GPUUnits:    200,
			IntentClass: "performance",
		}},
	}
	cfg := generatorConfig{}
	ord := 0
	jobs := expandLogicalWorkload(wl, cfg, &ord)
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
	for _, j := range jobs {
		if j.WorkloadID != wl.ID || j.WorkloadType != wl.Type || j.PodRole != "worker" {
			t.Fatalf("missing workload metadata in expanded job: %+v", j)
		}
		if !j.Gang {
			t.Fatalf("expected gang metadata on expanded job")
		}
	}
}

func TestHourlyArrivalMultiplierHasNightDip(t *testing.T) {
	if !(hourlyArrivalMultiplier(2) < hourlyArrivalMultiplier(10)) {
		t.Fatalf("expected night arrival multiplier to be lower than daytime")
	}
	if !(hourlyArrivalMultiplier(12) < hourlyArrivalMultiplier(10)) {
		t.Fatalf("expected lunch dip to be lower than daytime")
	}
}
