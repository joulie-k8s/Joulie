package main

import (
	"context"
	"fmt"
	"math"
	"math/rand"
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

// A node whose last job ends runs no load: its GPU and CPU utilization go
// to 0, as on a node that never ran a job. The job loops visit only nodes
// with jobs, so the node used to keep its last job's utilization, and draw
// that job's power, until a new job arrived: on 2026-10-27, GPU nodes with
// no pod at the end of experiment 03 showed a GPU utilization of 0.98 to 1.
// All three copies of the legacy progress loop are covered.
func TestFinishedJobLeavesItsNodeIdle(t *testing.T) {
	check := func(t *testing.T, s *simulator, jobs []*simJob) {
		t.Helper()
		for _, j := range jobs {
			if !j.Completed {
				t.Fatalf("precondition: %s did not complete", j.JobID)
			}
			st := s.state[j.NodeName]
			if st.GPUUtil != 0 || st.CPUUtil != 0 {
				t.Fatalf("%s after its last job: GPU utilization %g, CPU %g, want 0 and 0", j.NodeName, st.GPUUtil, st.CPUUtil)
			}
		}
	}
	almostDone := func(jobs []*simJob) {
		for _, j := range jobs {
			j.GPUUnitsRemaining = 1e-9
		}
	}
	now := time.Now().UTC()

	t.Run("standaloneAdvanceJobProgressFast", func(t *testing.T) {
		s, jobs := gpuCapSimulator(t, []string{"gpu-0"})
		almostDone(jobs)
		tracker := map[string]*standaloneNodeTracker{"gpu-0": {usedCPU: 1, usedGPU: 8, jobs: jobs}}
		standaloneAdvanceJobProgressFast(s, tracker, 1, now, map[string]bool{"gpu-0": true})
		check(t, s, jobs)
	})

	t.Run("standaloneTickParallel", func(t *testing.T) {
		names := make([]string, 32)
		for i := range names {
			names[i] = fmt.Sprintf("gpu-%02d", i)
		}
		s, jobs := gpuCapSimulator(t, names)
		almostDone(jobs)
		tracker := map[string]*standaloneNodeTracker{}
		active := map[string]bool{}
		for i, name := range names {
			tracker[name] = &standaloneNodeTracker{usedCPU: 1, usedGPU: 8, jobs: []*simJob{jobs[i]}}
			active[name] = true
		}
		standaloneTickParallel(s, tracker, names, nil, 1, time.Second, now, active, 2)
		check(t, s, jobs)
	})

	t.Run("advanceJobProgress", func(t *testing.T) {
		s, jobs := gpuCapSimulator(t, []string{"gpu-0"})
		almostDone(jobs)
		podDetailFunc = func(_ context.Context, _ kubernetes.Interface) ([]runningPodInfo, error) {
			return []runningPodInfo{{Namespace: "default", Name: "p", Node: "gpu-0", JobID: jobs[0].JobID}}, nil
		}
		defer func() { podDetailFunc = listRunningPodsDetailed }()
		// A pod poll counted the job's pod. The job ends in this tick and
		// its pod is deleted with it: the node is idle at once, CPU
		// included, not from the next tick or the next pod poll.
		s.state["gpu-0"].PodsRunning = 1
		s.advanceJobProgress(context.Background(), k8sfake.NewSimpleClientset(), 1, now)
		check(t, s, jobs)
	})
}

// The pod poll leaves the load of a node with jobs to the workload tick,
// which counts a job's CPU only while it has CPU work left and its GPU only
// while it has GPU work left. The poll used to set the load from every
// running job, its finished CPU or GPU work included, every 8 s, and the
// physics stepped on that load until the next tick: at the end of 03's
// baseline B (seed 1, 2026-10-27), 40 of 126 running GPU jobs had their CPU
// work done.
func TestPodPollLeavesTheLoadOfANodeWithJobsToTheTick(t *testing.T) {
	labels := map[string]map[string]string{"gpu-0": {"joulie.io/hw.gpu-count": "8", "joulie.io/hw.cpu-cores": "64"}}
	for _, tc := range []struct {
		name             string
		cpuLeft, gpuLeft float64
		wantCPU, wantGPU float64
	}{
		{"CPU work done", 0, gpuCapWork, 0, 1},
		{"GPU work done", gpuCapWork, 0, 0.5 / 64, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, jobs := gpuCapSimulator(t, []string{"gpu-0"})
			j := jobs[0]
			j.CPUUtilTarget, j.CPUUnitsRemaining, j.GPUUnitsRemaining = 0.5, tc.cpuLeft, tc.gpuLeft
			pod := runningPodInfo{Namespace: "default", Name: "p", Node: "gpu-0", JobID: j.JobID, Running: true}
			podDetailFunc = func(_ context.Context, _ kubernetes.Interface) ([]runningPodInfo, error) {
				return []runningPodInfo{pod}, nil
			}
			defer func() { podDetailFunc = listRunningPodsDetailed }()
			s.advanceJobProgress(context.Background(), k8sfake.NewSimpleClientset(), 1, time.Now().UTC())
			s.refreshNodeStateFromKubeData(map[string]int{"gpu-0": 1}, []runningPodInfo{pod}, labels)
			st := s.state["gpu-0"]
			if math.Abs(st.CPUUtil-tc.wantCPU) > 1e-12 || st.GPUUtil != tc.wantGPU {
				t.Fatalf("after a tick and a pod poll: CPU utilization %g, GPU %g, want the tick's %g and %g", st.CPUUtil, st.GPUUtil, tc.wantCPU, tc.wantGPU)
			}
		})
	}
}

// The pod poll and the workload tick run on two goroutines in Kubernetes
// mode, and the tick writes its jobs without s.mu: the poll reads only the
// node state, under s.mu. Run with -race; the poll used to read the jobs
// (overrideNodeUtil, then hasActiveJob), a data race.
func TestPodPollAndWorkloadTickRunTogether(t *testing.T) {
	labels := map[string]map[string]string{"gpu-0": {"joulie.io/hw.gpu-count": "8", "joulie.io/hw.cpu-cores": "64"}}
	s, jobs := gpuCapSimulator(t, []string{"gpu-0"})
	pod := runningPodInfo{Namespace: "default", Name: "p", Node: "gpu-0", JobID: jobs[0].JobID, Running: true}
	podDetailFunc = func(_ context.Context, _ kubernetes.Interface) ([]runningPodInfo, error) {
		return []runningPodInfo{pod}, nil
	}
	defer func() { podDetailFunc = listRunningPodsDetailed }()
	kube := k8sfake.NewSimpleClientset()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			s.advanceJobProgress(context.Background(), kube, 0.001, time.Now().UTC())
		}
		close(done)
	}()
	for i := 0; i < 50; i++ {
		s.refreshNodeStateFromKubeData(map[string]int{"gpu-0": 1}, []runningPodInfo{pod}, labels)
	}
	<-done
}

// legacyInlineCPURate is the legacy CPU progress formula, spelled out on
// simJob's fields in the operand order the legacy experiments ran with: the
// CPU throttle impact composed with the request, the base speed and the
// share, and clamped at 0.
func legacyInlineCPURate(j *simJob, baseSpeed, cpuMul, cpuShare float64) float64 {
	baseImpact := clamp01(0.60*clamp01(j.CPUUtilTarget) + 0.25*(1.0-clamp01(j.MemoryIntensity)) + 0.15*(1.0-clamp01(j.IOIntensity)))
	effective := 1.0 - baseImpact*(1.0-clamp01(cpuMul))
	effective = 1.0 - j.SensitivityCPU*(1.0-effective)
	speed := j.RequestedCPUCores * baseSpeed * clamp01(effective) * cpuShare
	if speed < 0 {
		speed = 0
	}
	return speed
}

// The CPU rate of legacyJobRates is the legacy inline formula bit for bit
// over the inputs the legacy loops pass: CPU throughputs from
// cpuThroughputMultiplier on the analytic and a curve model, every CPU work
// class, the trace's profile and sensitivity, base speeds and CPU shares,
// plus random inputs, out-of-range ones included.
func TestLegacyJobRatesCPUEqualsTheInlineFormula(t *testing.T) {
	analytic := gpuCapModel()
	curve := gpuCapModel()
	curve.CPUPowerCurve = []hw.PowerPoint{{LoadPct: 0, PowerW: 120}, {LoadPct: 50, PowerW: 260}, {LoadPct: 100, PowerW: 400}}
	curve.CPULowestNonlinearFreqMHz, curve.FMaxMHz = 1800, 3600
	check := func(j *simJob, base, cpuMul, share float64) {
		t.Helper()
		got, _ := legacyJobRates(j, analytic, base, cpuMul, share, 1, gpuCapW, 0)
		if want := legacyInlineCPURate(j, base, cpuMul, share); got != want && !(math.IsNaN(got) && math.IsNaN(want)) {
			t.Fatalf("job %+v, base %v, cpuMul %v, share %v: CPU rate %v, want the inline %v", *j, base, cpuMul, share, got, want)
		}
	}
	n := 0
	for _, model := range []simModel{analytic, curve} {
		for _, class := range []string{"cpu.compute_bound", "cpu.memory_bound", "cpu.io_bound", "cpu.mixed", ""} {
			for _, freq := range []float64{0.35, 0.5, 0.7, 0.85, 1} {
				for _, mem := range []float64{0, 0.3, 0.9} {
					for _, io := range []float64{0, 0.4, 1} {
						for _, thermal := range []float64{0, 0.25} {
							cpuMul := cpuThroughputMultiplier(freq, class, model, mem, io, thermal)
							for _, util := range []float64{0, 0.5, 1} {
								for _, sens := range []float64{0, 0.6, 1} {
									for _, share := range []float64{1, 0.4} {
										j := &simJob{RequestedCPUCores: 4, CPUUtilTarget: util, MemoryIntensity: mem, IOIntensity: io, SensitivityCPU: sens, CPUWorkClass: class}
										check(j, 1, cpuMul, share)
										check(j, 2.5, cpuMul, share)
										n++
									}
								}
							}
						}
					}
				}
			}
		}
	}
	rng := rand.New(rand.NewSource(19))
	r := func(lo, hi float64) float64 { return lo + (hi-lo)*rng.Float64() }
	for i := 0; i < 20000; i++ {
		j := &simJob{
			RequestedCPUCores: r(0, 64), CPUUtilTarget: r(-0.2, 1.2), MemoryIntensity: r(-0.2, 1.2), IOIntensity: r(-0.2, 1.2),
			SensitivityCPU: r(0, 1), CPUFeedIntensity: r(-0.2, 1.2), RequestedGPUs: math.Floor(r(0, 9)), GPUUnitsRemaining: r(-1, 1),
		}
		check(j, r(0, 3), r(-0.1, 1.1), r(0, 1))
		n++
	}
	t.Logf("%d inputs", n)
}
