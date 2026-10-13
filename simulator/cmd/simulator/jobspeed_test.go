package main

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/matbun/joulie/simulator/pkg/phys"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// The GPU cap fixture: eight 400 W GPUs capped at 240 W each, and a
// compute-bound job that keeps all eight busy with no CPU work and no CPU
// feed, so only the cap can slow it.
const (
	gpuCapMaxW     = 400.0
	gpuCapW        = 240.0
	gpuCapBase     = 2.0
	gpuCapWork     = 1e6
	gpuCapWorkload = "gpu.compute_bound"
)

func gpuCapModel() simModel {
	return simModel{
		BaseIdleW: 80, RaplHeadW: 5, DefaultCapW: 5000,
		GPU: hw.GPUProfile{
			Vendor: "nvidia", Product: "test-gpu", Count: 8,
			IdleWattsPerGPU: 100, MaxWattsPerGPU: gpuCapMaxW, MinCapWattsPerGPU: 100,
			ComputeGamma: 1, MemoryEpsilon: 0.2, MemoryGamma: 1.1,
		},
	}
}

// gpuCapSimulator returns a legacy simulator whose nodes run one fixture job
// each under the 240 W cap.
func gpuCapSimulator(t *testing.T, nodes []string) (*simulator, []*simJob) {
	t.Helper()
	s := newSimulatorWithRegisterer(gpuCapModel(), nil, nil, 10, prometheus.NewRegistry())
	s.workload = &workloadEngine{startTime: time.Now().UTC(), baseSpeedCore: gpuCapBase, jobByID: map[string]*simJob{}}
	jobs := make([]*simJob, 0, len(nodes))
	for _, node := range nodes {
		st := s.getNode(node)
		st.CPUCapacityCores = 64
		st.GPUCapWattsPerGpu = gpuCapW
		st.GPUTargetCapWattsPerGpu = gpuCapW
		j := &simJob{
			JobID: "job-" + node, NodeName: node, Submitted: true,
			RequestedCPUCores: 1, CPUWorkClass: "cpu.mixed",
			RequestedGPUs: 8, GPUUnitsRemaining: gpuCapWork, GPUWorkClass: gpuCapWorkload, GPUUtilTarget: 1,
			SensitivityCPU: 1, SensitivityGPU: 1,
		}
		jobs = append(jobs, j)
		s.workload.jobs = append(s.workload.jobs, j)
		s.workload.jobByID[j.JobID] = j
	}
	return s, jobs
}

// A GPU cap slows a GPU job by what the capped board model says it costs,
// and by nothing more. The old progress formula multiplied that by the cap
// over the board maximum as well (gpuCapImpact, 0.6 here), so a 60 % cap cost
// a job with S_gpu 1 the throughput loss twice. All three copies of the
// legacy progress loop are covered: standaloneAdvanceJobProgressFast, the
// parallel standalone tick and advanceJobProgress.
func TestGPUCapSlowsAGPUJobOnce(t *testing.T) {
	model := gpuCapModel()
	board := phys.CappedBoardGPUModel{
		IdleW: model.GPU.IdleWattsPerGPU, MaxW: model.GPU.MaxWattsPerGPU,
		ComputeGamma: model.GPU.ComputeGamma, MemoryEpsilon: model.GPU.MemoryEpsilon, MemoryGamma: model.GPU.MemoryGamma,
	}
	mul := board.ThroughputMultiplier(phys.DeviceState{
		Utilization: 1, CapWatts: gpuCapW, MaxCapWatts: gpuCapMaxW, Class: gpuCapWorkload,
	}, gpuCapWorkload)
	if math.Abs(mul-0.660) > 0.005 {
		t.Fatalf("precondition: a 240 W cap on a 400 W board gives ThroughputMultiplier %v, want about 0.660", mul)
	}
	want := 8 * gpuCapBase * mul
	check := func(t *testing.T, jobs []*simJob) {
		t.Helper()
		for _, j := range jobs {
			got := gpuCapWork - j.GPUUnitsRemaining
			if math.Abs(got-want) > 1e-9*want {
				t.Fatalf("%s: GPU progress in 1 s is %v, want 8 x base x ThroughputMultiplier = %v (ratio %.4f; 0.6 is the cap counted twice)", j.JobID, got, want, got/want)
			}
		}
	}
	now := time.Now().UTC()

	t.Run("standaloneAdvanceJobProgressFast", func(t *testing.T) {
		s, jobs := gpuCapSimulator(t, []string{"gpu-0"})
		tracker := map[string]*standaloneNodeTracker{"gpu-0": {usedCPU: 1, usedGPU: 8, jobs: jobs}}
		standaloneAdvanceJobProgressFast(s, tracker, 1, now, map[string]bool{"gpu-0": true})
		check(t, jobs)
	})

	t.Run("standaloneTickParallel", func(t *testing.T) {
		// The parallel path runs only from 32 active nodes and 2 workers.
		names := make([]string, 32)
		for i := range names {
			names[i] = fmt.Sprintf("gpu-%02d", i)
		}
		s, jobs := gpuCapSimulator(t, names)
		tracker := map[string]*standaloneNodeTracker{}
		active := map[string]bool{}
		for i, name := range names {
			tracker[name] = &standaloneNodeTracker{usedCPU: 1, usedGPU: 8, jobs: []*simJob{jobs[i]}}
			active[name] = true
		}
		standaloneTickParallel(s, tracker, names, nil, 1, time.Second, now, active, 2)
		check(t, jobs)
	})

	t.Run("advanceJobProgress", func(t *testing.T) {
		s, jobs := gpuCapSimulator(t, []string{"gpu-0"})
		podDetailFunc = func(_ context.Context, _ kubernetes.Interface) ([]runningPodInfo, error) {
			return []runningPodInfo{{Namespace: "default", Name: "p", Node: "gpu-0", JobID: jobs[0].JobID}}, nil
		}
		defer func() { podDetailFunc = listRunningPodsDetailed }()
		s.advanceJobProgress(context.Background(), k8sfake.NewSimpleClientset(), 1, now)
		check(t, jobs)
	})
}
