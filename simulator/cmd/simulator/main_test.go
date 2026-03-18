package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestListRunningPodsByNode(t *testing.T) {
	client := k8sfake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "n1"},
			Spec:       corev1.PodSpec{NodeName: "node-a"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "n1"},
			Spec:       corev1.PodSpec{NodeName: "node-a"},
			Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p3", Namespace: "n1"},
			Spec:       corev1.PodSpec{NodeName: "node-b"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	)
	counts, err := listRunningPodsByNode(context.Background(), client)
	if err != nil {
		t.Fatalf("listRunningPodsByNode error: %v", err)
	}
	if counts["node-a"] != 1 || counts["node-b"] != 1 {
		t.Fatalf("unexpected counts: %#v", counts)
	}
}

func TestHandleControlAndTelemetry(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 100, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000, RaplCapMinW: 80, RaplCapMaxW: 5000, CPUCapApplyTauMS: 2000},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)

	body := `{"action":"rapl.set_power_cap_watts","capWatts":120}`
	req := httptest.NewRequest(http.MethodPost, "/control/node-a", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleControl(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("control status=%d", w.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/telemetry/node-a", nil)
	w2 := httptest.NewRecorder()
	s.handleTelemetry(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("telemetry status=%d", w2.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode telemetry: %v", err)
	}
	cpu, ok := payload["cpu"].(map[string]any)
	if !ok {
		t.Fatalf("missing cpu payload")
	}
	if cpu["capWatts"].(float64) <= 120 {
		t.Fatalf("expected applied cap to be settling above target, got %v", cpu["capWatts"])
	}
	if cpu["targetCapWatts"].(float64) != 120 {
		t.Fatalf("expected targetCapWatts=120 got %v", cpu["targetCapWatts"])
	}
}

func TestCPUCapSettlingIsNotInstant(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{
			BaseIdleW:        100,
			PodW:             100,
			DvfsDropW:        1,
			RaplHeadW:        5,
			DefaultCapW:      500,
			RaplCapMinW:      100,
			RaplCapMaxW:      500,
			CPUCapApplyTauMS: 2000,
			CPUSockets:       2,
			CPUSocketCapMaxW: 250,
		},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)

	body := `{"action":"rapl.set_power_cap_watts","capWatts":200}`
	req := httptest.NewRequest(http.MethodPost, "/control/node-a", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleControl(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("control status=%d", w.Code)
	}
	st := s.getNode("node-a")
	if st.TargetCapWatts != 200 {
		t.Fatalf("target cap=%v want=200", st.TargetCapWatts)
	}
	if st.CapWatts <= 200 || st.CapWatts >= 500 {
		t.Fatalf("expected applied cap to settle gradually, got=%v", st.CapWatts)
	}
}

func TestCPUCapSettlesTowardTargetOverTime(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{
			BaseIdleW:        100,
			PodW:             100,
			DvfsDropW:        1,
			RaplHeadW:        5,
			DefaultCapW:      500,
			RaplCapMinW:      100,
			RaplCapMaxW:      500,
			CPUCapApplyTauMS: 100,
			CPUSockets:       2,
			CPUSocketCapMaxW: 250,
		},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)
	st := s.getNode("node-a")
	st.TargetCapWatts = 200
	st.CapWatts = 500
	st.LastUpdate = time.Now().Add(-time.Second)

	s.updateNodeDynamicsWithModel(st, s.model)

	if math.Abs(st.CapWatts-200) > 1 {
		t.Fatalf("expected cap to converge near target, got=%v", st.CapWatts)
	}
	if len(st.CPUSockets) != 2 {
		t.Fatalf("unexpected socket count=%d", len(st.CPUSockets))
	}
	if math.Abs(st.CPUSockets[0].CapWatts-100) > 1 || math.Abs(st.CPUSockets[1].CapWatts-100) > 1 {
		t.Fatalf("unexpected per-socket cap distribution: %#v", st.CPUSockets)
	}
}

func TestThermalTelemetryTracksHeatingAndAveraging(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{
			BaseIdleW:                100,
			PodW:                     100,
			DvfsDropW:                1,
			RaplHeadW:                5,
			DefaultCapW:              500,
			PMaxW:                    700,
			RaplCapMinW:              100,
			RaplCapMaxW:              700,
			CPUCapApplyTauMS:         100,
			CPUTelemetryWindowMS:     4000,
			CPUAmbientTempC:          24,
			CPUThermalTauMS:          500,
			CPUWattsPerDeltaC:        4,
			CPUThermalThrottleStartC: 60,
			CPUThermalThrottleFullC:  90,
			CPUSockets:               2,
		},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)

	st := s.getNode("node-hot")
	st.CPUUtil = 1.0
	st.MemoryIntensity = 0.05
	st.IOIntensity = 0.02
	st.LastUpdate = time.Now().Add(-2 * time.Second)

	model := s.modelForNode("node-hot")
	power := s.nodePowerWithModel(st, model)
	if power <= 0 {
		t.Fatalf("expected positive power")
	}
	if st.CPUTemperatureC <= 24 {
		t.Fatalf("expected cpu temperature to rise, got=%f", st.CPUTemperatureC)
	}
	if st.CPUAvgPowerWatts <= 0 || st.CPUAvgPowerWatts >= st.CPUInstantPowerWatts {
		t.Fatalf("expected averaged CPU power to lag instant power: avg=%f instant=%f", st.CPUAvgPowerWatts, st.CPUInstantPowerWatts)
	}
}

func TestWorkloadAggregationCarriesIntensityIntoNodeState(t *testing.T) {
	now := time.Now().UTC()
	s := newSimulatorWithRegisterer(
		simModel{
			BaseIdleW:   80,
			PodW:        100,
			DvfsDropW:   1,
			RaplHeadW:   5,
			DefaultCapW: 5000,
			GPU: hw.GPUProfile{
				Vendor:                "nvidia",
				Product:               "L40S",
				Count:                 1,
				IdleWattsPerGPU:       30,
				MaxWattsPerGPU:        350,
				MinCapWattsPerGPU:     200,
				CapApplyTauMS:         150,
				TelemetryWindowMS:     1000,
				AmbientTempC:          24,
				ThermalTauMS:          500,
				WattsPerDeltaC:        8,
				ThermalThrottleStartC: 80,
				ThermalThrottleFullC:  90,
				ComputeGamma:          1.0,
				MemoryEpsilon:         0.2,
				MemoryGamma:           1.2,
				PowerModel:            hw.GPUPowerModel{AlphaUtil: 1.0, BetaCap: 1.0},
			},
		},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)
	s.workload = &workloadEngine{
		startTime:     now,
		baseSpeedCore: 1.0,
		jobByID:       map[string]*simJob{},
	}
	job := &simJob{
		JobID:             "job-1",
		NodeName:          "node-x",
		RequestedCPUCores: 8,
		CPUUnitsRemaining: 100,
		CPUWorkClass:      "cpu.memory_bound",
		CPUUtilTarget:     0.7,
		RequestedGPUs:     1,
		GPUUnitsRemaining: 100,
		GPUWorkClass:      "gpu.memory_bound",
		GPUUtilTarget:     0.8,
		MemoryIntensity:   0.9,
		IOIntensity:       0.1,
		CPUFeedIntensity:  0.6,
	}
	s.workload.jobs = []*simJob{job}
	s.workload.jobByID[job.JobID] = job

	podDetailFunc = func(_ context.Context, _ kubernetes.Interface) ([]runningPodInfo, error) {
		return []runningPodInfo{{Namespace: "default", Name: "p", Node: "node-x", JobID: "job-1"}}, nil
	}
	defer func() { podDetailFunc = listRunningPodsDetailed }()

	s.advanceJobProgress(context.Background(), k8sfake.NewSimpleClientset(), 1.0, now.Add(time.Second))

	st := s.getNode("node-x")
	if st.MemoryIntensity < 0.85 {
		t.Fatalf("expected memory intensity to be aggregated, got=%f", st.MemoryIntensity)
	}
	if st.CPUFeedIntensity < 0.55 {
		t.Fatalf("expected cpu feed intensity to be aggregated, got=%f", st.CPUFeedIntensity)
	}
}

func TestWorkloadProgressUsesNodeCapacityInsteadOfJobCount(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 40, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil,
		nil,
		10,
		prometheus.NewRegistry(),
	)
	s.state["node-big"] = &nodeState{
		FreqScale:        1,
		CPUCapacityCores: 128,
	}
	s.workload = &workloadEngine{
		startTime:     time.Now().UTC(),
		baseSpeedCore: 1.0,
		jobByID:       map[string]*simJob{},
	}
	j1 := &simJob{JobID: "j1", RequestedCPUCores: 2, CPUUnitsRemaining: 100, CPUWorkClass: "cpu.compute_bound", CPUUtilTarget: 0.9}
	j2 := &simJob{JobID: "j2", RequestedCPUCores: 2, CPUUnitsRemaining: 100, CPUWorkClass: "cpu.compute_bound", CPUUtilTarget: 0.9}
	s.workload.jobs = []*simJob{j1, j2}
	s.workload.jobByID[j1.JobID] = j1
	s.workload.jobByID[j2.JobID] = j2

	podDetailFunc = func(_ context.Context, _ kubernetes.Interface) ([]runningPodInfo, error) {
		return []runningPodInfo{
			{Namespace: "default", Name: "p1", Node: "node-big", JobID: "j1"},
			{Namespace: "default", Name: "p2", Node: "node-big", JobID: "j2"},
		}, nil
	}
	defer func() { podDetailFunc = listRunningPodsDetailed }()

	s.advanceJobProgress(context.Background(), k8sfake.NewSimpleClientset(), 1.0, time.Now().UTC())
	if got := j1.CPUUnitsRemaining; got > 98.1 || got < 97.9 {
		t.Fatalf("j1 CPUUnitsRemaining=%f want about 98", got)
	}
	if got := j2.CPUUnitsRemaining; got > 98.1 || got < 97.9 {
		t.Fatalf("j2 CPUUnitsRemaining=%f want about 98", got)
	}
}

func TestGangJobCanFinishAfterPeerCompletes(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 40, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil,
		nil,
		10,
		prometheus.NewRegistry(),
	)
	s.state["node-gang"] = &nodeState{
		FreqScale:        1,
		CPUCapacityCores: 64,
	}
	s.workload = &workloadEngine{
		startTime:      time.Now().UTC(),
		baseSpeedCore:  1.0,
		jobByID:        map[string]*simJob{},
		jobsByWorkload: map[string][]*simJob{},
	}
	done := &simJob{
		JobID:             "done",
		WorkloadID:        "w1",
		Gang:              true,
		RequestedCPUCores: 1,
		CPUUnitsRemaining: 0,
		Completed:         true,
		Submitted:         true,
	}
	active := &simJob{
		JobID:             "active",
		WorkloadID:        "w1",
		Gang:              true,
		RequestedCPUCores: 1,
		CPUUnitsRemaining: 1,
		CPUWorkClass:      "cpu.compute_bound",
		CPUUtilTarget:     0.9,
		Submitted:         true,
		SubmittedAt:       time.Now().UTC().Add(-time.Second),
	}
	s.workload.jobs = []*simJob{done, active}
	s.workload.jobByID[done.JobID] = done
	s.workload.jobByID[active.JobID] = active
	s.workload.jobsByWorkload["w1"] = []*simJob{done, active}

	podDetailFunc = func(_ context.Context, _ kubernetes.Interface) ([]runningPodInfo, error) {
		return []runningPodInfo{{Namespace: "default", Name: "p-active", Node: "node-gang", JobID: "active"}}, nil
	}
	defer func() { podDetailFunc = listRunningPodsDetailed }()

	s.advanceJobProgress(context.Background(), k8sfake.NewSimpleClientset(), 2.0, time.Now().UTC())
	if !active.Completed {
		t.Fatalf("expected active gang member to complete once the other member is already completed")
	}
}

func TestCompletedJobDeletionIsRetriedUntilPodDisappears(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 40, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil,
		nil,
		10,
		prometheus.NewRegistry(),
	)
	s.workload = &workloadEngine{
		startTime: time.Now().UTC(),
		jobs:      []*simJob{},
		jobByID:   map[string]*simJob{},
	}
	job := &simJob{
		JobID:       "done",
		Namespace:   "default",
		PodName:     "sim-done",
		Submitted:   true,
		Completed:   true,
		CompletedAt: time.Now().UTC().Add(-time.Second),
	}
	s.workload.jobs = []*simJob{job}
	s.workload.jobByID[job.JobID] = job

	kube := k8sfake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sim-done",
			Namespace: "default",
			Annotations: map[string]string{
				"sim.joulie.io/jobId": "done",
			},
		},
		Spec:   corev1.PodSpec{NodeName: "node-cleanup"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	})

	podDetailFunc = func(_ context.Context, _ kubernetes.Interface) ([]runningPodInfo, error) {
		return []runningPodInfo{{
			Namespace: "default",
			Name:      "sim-done",
			Node:      "node-cleanup",
			JobID:     "done",
		}}, nil
	}
	defer func() { podDetailFunc = listRunningPodsDetailed }()

	s.advanceJobProgress(context.Background(), kube, 1.0, time.Now().UTC())
	if job.DeleteRequestedAt.IsZero() {
		t.Fatalf("expected deletion request to be recorded")
	}
	if _, err := kube.CoreV1().Pods("default").Get(context.Background(), "sim-done", metav1.GetOptions{}); err == nil {
		t.Fatalf("expected completed workload pod to be deleted")
	}
}

func TestHandleGPUControlAndTelemetry(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{
			BaseIdleW:   90,
			PodW:        80,
			DvfsDropW:   1,
			RaplHeadW:   5,
			DefaultCapW: 5000,
			GPU: hw.GPUProfile{
				Vendor:            "nvidia",
				Product:           "L40S",
				Count:             4,
				IdleWattsPerGPU:   30,
				MaxWattsPerGPU:    350,
				MinCapWattsPerGPU: 200,
				CapApplyTauMS:     300,
				ComputeGamma:      1.0,
				MemoryEpsilon:     0.2,
				MemoryGamma:       1.2,
				PowerModel: hw.GPUPowerModel{
					AlphaUtil: 1.0,
					BetaCap:   1.0,
				},
			},
		},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)

	body := `{"action":"gpu.set_power_cap_watts","capWattsPerGpu":220}`
	req := httptest.NewRequest(http.MethodPost, "/control/node-gpu", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleControl(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("control status=%d", w.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/telemetry/node-gpu", nil)
	w2 := httptest.NewRecorder()
	s.handleTelemetry(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("telemetry status=%d", w2.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode telemetry: %v", err)
	}
	gpu, ok := payload["gpu"].(map[string]any)
	if !ok {
		t.Fatalf("missing gpu payload")
	}
	if !gpu["present"].(bool) {
		t.Fatalf("expected gpu.present=true")
	}
	if gpu["capWattsPerGpuTarget"].(float64) != 220 {
		t.Fatalf("unexpected gpu target cap: %v", gpu["capWattsPerGpuTarget"])
	}
	applied := gpu["capWattsPerGpuApplied"].(float64)
	if applied < 220 || applied > 350 {
		t.Fatalf("unexpected gpu applied cap after settling start: %v", applied)
	}
}

func TestGPUCapSettlingIsNotInstant(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{
			BaseIdleW:   90,
			PodW:        80,
			DvfsDropW:   1,
			RaplHeadW:   5,
			DefaultCapW: 5000,
			GPU: hw.GPUProfile{
				Vendor:            "nvidia",
				Product:           "L40S",
				Count:             1,
				IdleWattsPerGPU:   30,
				MaxWattsPerGPU:    350,
				MinCapWattsPerGPU: 200,
				CapApplyTauMS:     2000,
				ComputeGamma:      1.0,
				MemoryEpsilon:     0.2,
				MemoryGamma:       1.2,
				PowerModel:        hw.GPUPowerModel{AlphaUtil: 1.0, BetaCap: 1.0},
			},
		},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)

	body := `{"action":"gpu.set_power_cap_watts","capWattsPerGpu":220}`
	req := httptest.NewRequest(http.MethodPost, "/control/node-gpu", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleControl(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("control status=%d", w.Code)
	}
	st := s.getNode("node-gpu")
	if st.GPUTargetCapWattsPerGpu != 220 {
		t.Fatalf("target cap=%v", st.GPUTargetCapWattsPerGpu)
	}
	if st.GPUCapWattsPerGpu <= 220 {
		t.Fatalf("expected applied cap to settle gradually, got=%v", st.GPUCapWattsPerGpu)
	}
}

func TestInjectTraceJobsSetsExtendedResourceLimits(t *testing.T) {
	now := time.Now()
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)
	s.workload = &workloadEngine{
		startTime: now.Add(-2 * time.Second),
		jobs: []*simJob{
			{
				JobID:             "gpu-1",
				Class:             "standard",
				Namespace:         "default",
				SubmitOffsetSec:   0,
				RequestedCPUCores: 2,
				RequestedGPUs:     1,
				PodName:           "sim-gpu-1",
			},
		},
	}

	kube := k8sfake.NewSimpleClientset()
	s.injectTraceJobs(context.Background(), kube, now)
	pod, err := kube.CoreV1().Pods("default").Get(context.Background(), "sim-gpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get injected pod: %v", err)
	}
	req := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]
	lim := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
	if req.Cmp(resource.MustParse("1")) != 0 {
		t.Fatalf("gpu request=%s want=1", req.String())
	}
	if lim.Cmp(resource.MustParse("1")) != 0 {
		t.Fatalf("gpu limit=%s want=1", lim.String())
	}
}

func TestInjectTraceJobsUsesAMDResourceAndSelector(t *testing.T) {
	now := time.Now()
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)
	s.workload = &workloadEngine{
		startTime: now.Add(-2 * time.Second),
		jobs: []*simJob{
			{
				JobID:             "gpu-amd-1",
				Class:             "standard",
				Namespace:         "default",
				SubmitOffsetSec:   0,
				RequestedCPUCores: 2,
				RequestedGPUs:     2,
				GPUResourceName:   "amd.com/gpu",
				PodName:           "sim-gpu-amd-1",
			},
		},
	}

	kube := k8sfake.NewSimpleClientset()
	s.injectTraceJobs(context.Background(), kube, now)
	pod, err := kube.CoreV1().Pods("default").Get(context.Background(), "sim-gpu-amd-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get injected pod: %v", err)
	}
	req := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceName("amd.com/gpu")]
	lim := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceName("amd.com/gpu")]
	if req.Cmp(resource.MustParse("2")) != 0 {
		t.Fatalf("amd gpu request=%s want=2", req.String())
	}
	if lim.Cmp(resource.MustParse("2")) != 0 {
		t.Fatalf("amd gpu limit=%s want=2", lim.String())
	}
	if got := pod.Spec.NodeSelector["feature.node.kubernetes.io/pci-1002.present"]; got != "true" {
		t.Fatalf("expected AMD selector, got=%q selectors=%v", got, pod.Spec.NodeSelector)
	}
}

func TestInjectTraceJobsAddsKWOKToleration(t *testing.T) {
	now := time.Now()
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)
	s.workload = &workloadEngine{
		startTime: now.Add(-2 * time.Second),
		jobs: []*simJob{
			{
				JobID:             "cpu-1",
				Class:             "standard",
				Namespace:         "default",
				SubmitOffsetSec:   0,
				RequestedCPUCores: 1,
				PodName:           "sim-cpu-1",
			},
		},
	}

	kube := k8sfake.NewSimpleClientset()
	s.injectTraceJobs(context.Background(), kube, now)
	pod, err := kube.CoreV1().Pods("default").Get(context.Background(), "sim-cpu-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get injected pod: %v", err)
	}
	found := false
	for _, tol := range pod.Spec.Tolerations {
		if tol.Key == "kwok.x-k8s.io/node" && tol.Value == "fake" && tol.Effect == corev1.TaintEffectNoSchedule {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected kwok toleration in injected pod: %#v", pod.Spec.Tolerations)
	}
}

func TestInitWorkloadEngineFromTraceParsesGPUResourceKey(t *testing.T) {
	now := time.Now()
	trace := strings.Join([]string{
		`{"type":"job","jobId":"nvidia-job","submitTimeOffsetSec":0,"podTemplate":{"requests":{"cpu":"1","nvidia.com/gpu":"2"}}}`,
		`{"type":"job","jobId":"amd-job","submitTimeOffsetSec":0,"podTemplate":{"requests":{"cpu":"1","amd.com/gpu":"1"}}}`,
		`{"type":"job","jobId":"generic-job","submitTimeOffsetSec":0,"podTemplate":{"requests":{"cpu":"1","gpu":"3"}}}`,
	}, "\n")
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, []byte(trace), 0o644); err != nil {
		t.Fatalf("write trace: %v", err)
	}

	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)
	if err := s.initWorkloadEngineFromTrace(path); err != nil {
		t.Fatalf("initWorkloadEngineFromTrace: %v", err)
	}
	if len(s.workload.jobs) != 3 {
		t.Fatalf("jobs=%d want=3", len(s.workload.jobs))
	}

	byID := map[string]*simJob{}
	for _, j := range s.workload.jobs {
		byID[j.JobID] = j
	}
	if byID["nvidia-job"].GPUResourceName != "nvidia.com/gpu" || byID["nvidia-job"].RequestedGPUs != 2 {
		t.Fatalf("unexpected nvidia job: %+v", byID["nvidia-job"])
	}
	if byID["amd-job"].GPUResourceName != "amd.com/gpu" || byID["amd-job"].RequestedGPUs != 1 {
		t.Fatalf("unexpected amd job: %+v", byID["amd-job"])
	}
	if byID["generic-job"].GPUResourceName != "nvidia.com/gpu" || byID["generic-job"].RequestedGPUs != 3 {
		t.Fatalf("unexpected generic job: %+v", byID["generic-job"])
	}
	if s.workload.startTime.Before(now.Add(-2 * time.Second)) {
		t.Fatalf("unexpected start time too old: %v", s.workload.startTime)
	}
}

func TestInitWorkloadEngineFromTraceReadsDirectoryOfParts(t *testing.T) {
	dir := t.TempDir()
	partA := filepath.Join(dir, "part-000.jsonl")
	partB := filepath.Join(dir, "part-001.jsonl")
	if err := os.WriteFile(partA, []byte(`{"type":"job","jobId":"job-a","submitTimeOffsetSec":2}`+"\n"), 0o644); err != nil {
		t.Fatalf("write partA: %v", err)
	}
	if err := os.WriteFile(partB, []byte(`{"type":"job","jobId":"job-b","submitTimeOffsetSec":1}`+"\n"), 0o644); err != nil {
		t.Fatalf("write partB: %v", err)
	}

	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)
	if err := s.initWorkloadEngineFromTrace(dir); err != nil {
		t.Fatalf("initWorkloadEngineFromTrace: %v", err)
	}
	if len(s.workload.jobs) != 2 {
		t.Fatalf("jobs=%d want=2", len(s.workload.jobs))
	}
	if s.workload.jobs[0].JobID != "job-b" || s.workload.jobs[1].JobID != "job-a" {
		t.Fatalf("unexpected job order from trace parts: %+v", s.workload.jobs)
	}
}

func TestInitWorkloadEngineFromTraceRejectsNonIntegerGPURequest(t *testing.T) {
	trace := `{"type":"job","jobId":"bad-gpu","submitTimeOffsetSec":0,"podTemplate":{"requests":{"cpu":"1","nvidia.com/gpu":"0.5"}}}`
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, []byte(trace), 0o644); err != nil {
		t.Fatalf("write trace: %v", err)
	}

	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)
	if err := s.initWorkloadEngineFromTrace(path); err != nil {
		t.Fatalf("initWorkloadEngineFromTrace: %v", err)
	}
	if len(s.workload.jobs) != 1 {
		t.Fatalf("jobs=%d want=1", len(s.workload.jobs))
	}
	job := s.workload.jobs[0]
	if job.RequestedGPUs != 0 {
		t.Fatalf("RequestedGPUs=%v want=0", job.RequestedGPUs)
	}
	if job.GPUResourceName != "" {
		t.Fatalf("GPUResourceName=%q want empty", job.GPUResourceName)
	}
}

func TestInitWorkloadEngineFromTraceParsesWorkloadProfile(t *testing.T) {
	trace := `{"type":"job","jobId":"profiled","submitTimeOffsetSec":0,"podTemplate":{"requests":{"cpu":"2","nvidia.com/gpu":"1"}},"workloadClass":{"cpu":"cpu.io_bound","gpu":"gpu.memory_bound"},"workloadProfile":{"cpuUtilization":0.25,"gpuUtilization":0.7,"memoryIntensity":0.8,"ioIntensity":0.9,"cpuFeedIntensityGpu":0.6}}`
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, []byte(trace), 0o644); err != nil {
		t.Fatalf("write trace: %v", err)
	}
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil,
		nil,
		200,
		prometheus.NewRegistry(),
	)
	if err := s.initWorkloadEngineFromTrace(path); err != nil {
		t.Fatalf("initWorkloadEngineFromTrace: %v", err)
	}
	job := s.workload.jobs[0]
	if job.CPUUtilTarget != 0.25 || job.GPUUtilTarget != 0.7 || job.MemoryIntensity != 0.8 || job.IOIntensity != 0.9 || job.CPUFeedIntensity != 0.6 {
		t.Fatalf("unexpected job profile: %+v", job)
	}
	if job.CPUWorkClass != "cpu.io_bound" || job.GPUWorkClass != "gpu.memory_bound" {
		t.Fatalf("unexpected job classes: %+v", job)
	}
}

func TestThrottleImpactHelpers(t *testing.T) {
	compute := &simJob{CPUUtilTarget: 0.95, MemoryIntensity: 0.1, IOIntensity: 0.05, SensitivityCPU: 1.0}
	ioBound := &simJob{CPUUtilTarget: 0.2, MemoryIntensity: 0.2, IOIntensity: 0.95, SensitivityCPU: 1.0}
	computeFactor := cpuThrottleImpactFactor(0.5, compute)
	ioFactor := cpuThrottleImpactFactor(0.5, ioBound)
	if ioFactor <= computeFactor {
		t.Fatalf("expected io-bound helper to degrade less: io=%f compute=%f", ioFactor, computeFactor)
	}

	gpuFed := &simJob{CPUFeedIntensity: 0.8, SensitivityCPU: 1.0}
	gpuLoose := &simJob{CPUFeedIntensity: 0.2, SensitivityCPU: 1.0}
	fedFactor := cpuFeedThrottleFactor(0.5, gpuFed)
	looseFactor := cpuFeedThrottleFactor(0.5, gpuLoose)
	if fedFactor >= looseFactor {
		t.Fatalf("expected stronger cpu feed dependence to degrade more: fed=%f loose=%f", fedFactor, looseFactor)
	}
}

func TestEnvHelpers(t *testing.T) {
	t.Setenv("SIM_BOOL", "true")
	t.Setenv("SIM_FLOAT", "12.5")
	t.Setenv("SIM_DUR", "7s")
	if !boolEnv("SIM_BOOL", false) {
		t.Fatalf("boolEnv failed")
	}
	if got := floatEnv("SIM_FLOAT", 0); got != 12.5 {
		t.Fatalf("floatEnv got=%v", got)
	}
	if got := durationEnv("SIM_DUR", time.Second); got != 7*time.Second {
		t.Fatalf("durationEnv got=%v", got)
	}
}

func TestRefreshNodeStateRespectsSelectorAndClass(t *testing.T) {
	base := simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000}
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	classCap := 900.0
	classIdle := 50.0
	classes := []simNodeClass{
		{
			Name: "intel-eco",
			MatchLabels: map[string]string{
				"feature.node.kubernetes.io/cpu-model.vendor_id": "Intel",
			},
			Model: simModelOverrides{
				DefaultCapW: &classCap,
				BaseIdleW:   &classIdle,
			},
		},
	}

	s := newSimulatorWithRegisterer(base, selector, classes, 200, prometheus.NewRegistry())
	s.refreshNodeStateFromKubeData(
		map[string]int{"node-a": 3, "node-b": 7},
		nil,
		map[string]map[string]string{
			"node-a": {
				"joulie.io/managed": "true",
				"feature.node.kubernetes.io/cpu-model.vendor_id": "Intel",
			},
			"node-b": {
				"joulie.io/managed": "false",
			},
		},
	)

	a := s.getNode("node-a")
	if a.PodsRunning != 3 {
		t.Fatalf("node-a pods=%d", a.PodsRunning)
	}
	if a.CapWatts != classCap {
		t.Fatalf("node-a cap=%v want=%v", a.CapWatts, classCap)
	}
	b := s.getNode("node-b")
	if b.PodsRunning != 0 {
		t.Fatalf("node-b should be filtered by selector, pods=%d", b.PodsRunning)
	}
}

func TestDebugEventsRingBuffer(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 100, PodW: 10, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil,
		nil,
		2,
		prometheus.NewRegistry(),
	)
	s.recordEvent("control", "n1", map[string]any{"x": 1})
	s.recordEvent("telemetry", "n1", map[string]any{"x": 2})
	s.recordEvent("control", "n1", map[string]any{"x": 3})

	req := httptest.NewRequest(http.MethodGet, "/debug/events", nil)
	w := httptest.NewRecorder()
	s.handleDebugEvents(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := int(payload["count"].(float64)); got != 2 {
		t.Fatalf("count=%d", got)
	}
}

func TestHandleDebugNodesIncludesClassAndSelection(t *testing.T) {
	base := simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000}
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	classCap := 900.0
	classes := []simNodeClass{
		{
			Name: "intel-eco",
			MatchLabels: map[string]string{
				"feature.node.kubernetes.io/cpu-model.vendor_id": "Intel",
			},
			Model: simModelOverrides{
				DefaultCapW: &classCap,
			},
		},
	}

	s := newSimulatorWithRegisterer(base, selector, classes, 50, prometheus.NewRegistry())
	s.refreshNodeStateFromKubeData(
		map[string]int{"node-a": 2, "node-b": 5},
		nil,
		map[string]map[string]string{
			"node-a": {
				"joulie.io/managed": "true",
				"feature.node.kubernetes.io/cpu-model.vendor_id": "Intel",
			},
			"node-b": {
				"joulie.io/managed": "false",
			},
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/debug/nodes", nil)
	w := httptest.NewRecorder()
	s.handleDebugNodes(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}

	var payload struct {
		Count int `json:"count"`
		Nodes []struct {
			Node     string `json:"node"`
			Selected bool   `json:"selected"`
			Class    string `json:"class"`
			Known    bool   `json:"known"`
			State    *struct {
				CapWatts    float64 `json:"capWatts"`
				PodsRunning int     `json:"podsRunning"`
			} `json:"state"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Count != 2 {
		t.Fatalf("count=%d", payload.Count)
	}
	seen := map[string]struct {
		selected bool
		class    string
		known    bool
		pods     int
		cap      float64
	}{}
	for _, n := range payload.Nodes {
		entry := struct {
			selected bool
			class    string
			known    bool
			pods     int
			cap      float64
		}{
			selected: n.Selected,
			class:    n.Class,
			known:    n.Known,
		}
		if n.State != nil {
			entry.pods = n.State.PodsRunning
			entry.cap = n.State.CapWatts
		}
		seen[n.Node] = entry
	}

	if !seen["node-a"].selected || seen["node-a"].class != "intel-eco" || !seen["node-a"].known {
		t.Fatalf("unexpected node-a: %#v", seen["node-a"])
	}
	if seen["node-a"].pods != 2 || seen["node-a"].cap != classCap {
		t.Fatalf("unexpected node-a state: %#v", seen["node-a"])
	}
	if seen["node-b"].selected || seen["node-b"].known {
		t.Fatalf("unexpected node-b: %#v", seen["node-b"])
	}
}

func TestDebugEndpointsMethodNotAllowed(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 100, PodW: 10, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil,
		nil,
		10,
		prometheus.NewRegistry(),
	)

	reqNodes := httptest.NewRequest(http.MethodPost, "/debug/nodes", nil)
	wNodes := httptest.NewRecorder()
	s.handleDebugNodes(wNodes, reqNodes)
	if wNodes.Code != http.StatusMethodNotAllowed {
		t.Fatalf("debug/nodes status=%d", wNodes.Code)
	}

	reqEvents := httptest.NewRequest(http.MethodPost, "/debug/events", nil)
	wEvents := httptest.NewRecorder()
	s.handleDebugEvents(wEvents, reqEvents)
	if wEvents.Code != http.StatusMethodNotAllowed {
		t.Fatalf("debug/events status=%d", wEvents.Code)
	}
}

func TestLoadNodeClasses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node-classes.yaml")
	content := `
classes:
  - name: amd-fast
    matchLabels:
      feature.node.kubernetes.io/cpu-model.vendor_id: AMD
    model:
      podW: 200
      defaultCapW: 4500
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	classes, err := loadNodeClasses(path)
	if err != nil {
		t.Fatalf("loadNodeClasses: %v", err)
	}
	if len(classes) != 1 || classes[0].Name != "amd-fast" {
		t.Fatalf("unexpected classes: %#v", classes)
	}
	if classes[0].Model.PodW == nil || *classes[0].Model.PodW != 200 {
		t.Fatalf("podW override missing: %#v", classes[0].Model.PodW)
	}
	if classes[0].Model.DefaultCapW == nil || *classes[0].Model.DefaultCapW != 4500 {
		t.Fatalf("defaultCapW override missing: %#v", classes[0].Model.DefaultCapW)
	}
}

// ---------------------------------------------------------------------------
// Intent-class classification tests
// ---------------------------------------------------------------------------

func TestClassifyClassFromPodSpecPerformance(t *testing.T) {
	spec := &corev1.PodSpec{
		Affinity: &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "joulie.io/power-profile",
									Operator: corev1.NodeSelectorOpNotIn,
									Values:   []string{"eco"},
								},
							},
						},
					},
				},
			},
		},
	}
	if got := classifyClassFromPodSpec(spec); got != "performance" {
		t.Fatalf("expected 'performance', got %q", got)
	}
}

func TestClassifyClassFromPodSpecStandard(t *testing.T) {
	spec := &corev1.PodSpec{}
	if got := classifyClassFromPodSpec(spec); got != "standard" {
		t.Fatalf("expected 'standard', got %q", got)
	}
}

func TestClassifyClassFromPodSpecNilSpec(t *testing.T) {
	if got := classifyClassFromPodSpec(nil); got != "standard" {
		t.Fatalf("expected 'standard', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// affinityForIntentClass tests
// ---------------------------------------------------------------------------

func TestAffinityForIntentClassPerformance(t *testing.T) {
	aff := affinityForIntentClass("performance")
	if aff == nil || aff.NodeAffinity == nil {
		t.Fatalf("expected non-nil affinity for performance")
	}
	required := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil || len(required.NodeSelectorTerms) == 0 {
		t.Fatalf("expected NodeSelectorTerms")
	}
	expr := required.NodeSelectorTerms[0].MatchExpressions[0]
	if expr.Key != "joulie.io/power-profile" || expr.Operator != corev1.NodeSelectorOpNotIn {
		t.Fatalf("unexpected expression: %+v", expr)
	}
	found := false
	for _, v := range expr.Values {
		if v == "eco" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'eco' in NotIn values, got %v", expr.Values)
	}
}

func TestAffinityForIntentClassStandard(t *testing.T) {
	if aff := affinityForIntentClass("standard"); aff != nil {
		t.Fatalf("expected nil affinity for standard, got %+v", aff)
	}
}

func TestAffinityForIntentClassRoundTrip(t *testing.T) {
	aff := affinityForIntentClass("performance")
	spec := &corev1.PodSpec{Affinity: aff}
	if got := classifyClassFromPodSpec(spec); got != "performance" {
		t.Fatalf("round-trip: expected 'performance', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Trace loading tests
// ---------------------------------------------------------------------------

func TestLoadTraceReadsExplicitIntentClass(t *testing.T) {
	trace := `{"type":"job","jobId":"j1","submitTimeOffsetSec":0,"intentClass":"performance","podTemplate":{"requests":{"cpu":"1"}}}` + "\n"
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, []byte(trace), 0o644); err != nil {
		t.Fatalf("write trace: %v", err)
	}
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil, nil, 200, prometheus.NewRegistry(),
	)
	if err := s.initWorkloadEngineFromTrace(path); err != nil {
		t.Fatalf("initWorkloadEngineFromTrace: %v", err)
	}
	if len(s.workload.jobs) != 1 {
		t.Fatalf("jobs=%d want=1", len(s.workload.jobs))
	}
	if got := s.workload.jobs[0].Class; got != "performance" {
		t.Fatalf("expected Class='performance', got %q", got)
	}
}

func TestLoadTraceInfersIntentFromAffinityWhenNoExplicitField(t *testing.T) {
	// Old-style trace: no intentClass field, but has affinity NotIn eco.
	trace := `{"type":"job","jobId":"j1","submitTimeOffsetSec":0,"podTemplate":{"requests":{"cpu":"1"},"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"joulie.io/power-profile","operator":"NotIn","values":["eco"]}]}]}}}}}` + "\n"
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, []byte(trace), 0o644); err != nil {
		t.Fatalf("write trace: %v", err)
	}
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil, nil, 200, prometheus.NewRegistry(),
	)
	if err := s.initWorkloadEngineFromTrace(path); err != nil {
		t.Fatalf("initWorkloadEngineFromTrace: %v", err)
	}
	if got := s.workload.jobs[0].Class; got != "performance" {
		t.Fatalf("expected Class='performance' inferred from affinity, got %q", got)
	}
}

func TestLoadTraceDefaultsToStandardWithNoAffinityNoField(t *testing.T) {
	trace := `{"type":"job","jobId":"j1","submitTimeOffsetSec":0,"podTemplate":{"requests":{"cpu":"2"}}}` + "\n"
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, []byte(trace), 0o644); err != nil {
		t.Fatalf("write trace: %v", err)
	}
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil, nil, 200, prometheus.NewRegistry(),
	)
	if err := s.initWorkloadEngineFromTrace(path); err != nil {
		t.Fatalf("initWorkloadEngineFromTrace: %v", err)
	}
	if got := s.workload.jobs[0].Class; got != "standard" {
		t.Fatalf("expected Class='standard', got %q", got)
	}
}

func TestLoadTraceNeverProducesStaleClassNames(t *testing.T) {
	staleNames := []string{"general", "eco", "batch"}
	lines := []string{
		`{"type":"job","jobId":"j1","submitTimeOffsetSec":0,"podTemplate":{"requests":{"cpu":"1"}}}`,
		`{"type":"job","jobId":"j2","submitTimeOffsetSec":1,"intentClass":"performance","podTemplate":{"requests":{"cpu":"1"}}}`,
		`{"type":"job","jobId":"j3","submitTimeOffsetSec":2,"intentClass":"standard","podTemplate":{"requests":{"cpu":"1"}}}`,
	}
	trace := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, []byte(trace), 0o644); err != nil {
		t.Fatalf("write trace: %v", err)
	}
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil, nil, 200, prometheus.NewRegistry(),
	)
	if err := s.initWorkloadEngineFromTrace(path); err != nil {
		t.Fatalf("initWorkloadEngineFromTrace: %v", err)
	}
	for _, j := range s.workload.jobs {
		for _, stale := range staleNames {
			if j.Class == stale {
				t.Fatalf("job %s has stale class name %q", j.JobID, j.Class)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Pod injection tests
// ---------------------------------------------------------------------------

func newTestSimulatorWithJobs(jobs []*simJob) (*simulator, kubernetes.Interface) {
	now := time.Now()
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 80, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
		nil, nil, 200, prometheus.NewRegistry(),
	)
	s.workload = &workloadEngine{
		startTime: now.Add(-2 * time.Second),
		jobs:      jobs,
	}
	kube := k8sfake.NewSimpleClientset()
	s.injectTraceJobs(context.Background(), kube, now)
	return s, kube
}

func TestInjectTraceJobsSetsSimUtilAnnotations(t *testing.T) {
	job := &simJob{
		JobID:             "j-compute",
		Class:             "performance",
		Namespace:         "default",
		SubmitOffsetSec:   0,
		RequestedCPUCores: 4,
		CPUUtilTarget:     0.85,
		GPUUtilTarget:     0.0,
		MemoryIntensity:   0.20,
		IOIntensity:       0.10,
		PodName:           "sim-j-compute",
	}
	_, kube := newTestSimulatorWithJobs([]*simJob{job})
	pod, err := kube.CoreV1().Pods("default").Get(context.Background(), job.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	// Should NOT have joulie.io/workload-class (no more cheating).
	if got := pod.Annotations["joulie.io/workload-class"]; got != "" {
		t.Fatalf("expected no joulie.io/workload-class annotation, got %q", got)
	}
	// Should have sim utilization annotations.
	expected := map[string]string{
		"sim.joulie.io/cpu-util-pct":        "85.0",
		"sim.joulie.io/gpu-util-pct":        "0.0",
		"sim.joulie.io/memory-pressure-pct": "20.0",
		"sim.joulie.io/io-intensity":        "0.10",
	}
	for key, want := range expected {
		if got := pod.Annotations[key]; got != want {
			t.Errorf("annotation %s: got %q, want %q", key, got, want)
		}
	}
}

func TestInjectTraceJobsPerformancePodHasAffinityNotInEco(t *testing.T) {
	job := &simJob{
		JobID:             "perf-1",
		Class:             "performance",
		Namespace:         "default",
		SubmitOffsetSec:   0,
		RequestedCPUCores: 1,
		PodName:           "sim-perf-1",
	}
	_, kube := newTestSimulatorWithJobs([]*simJob{job})
	pod, err := kube.CoreV1().Pods("default").Get(context.Background(), "sim-perf-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	aff := pod.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil {
		t.Fatalf("expected affinity on performance pod")
	}
	required := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil || len(required.NodeSelectorTerms) == 0 {
		t.Fatalf("expected NodeSelectorTerms on performance pod")
	}
	foundNotInEco := false
	for _, term := range required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == "joulie.io/power-profile" && expr.Operator == corev1.NodeSelectorOpNotIn {
				for _, v := range expr.Values {
					if v == "eco" {
						foundNotInEco = true
					}
				}
			}
		}
	}
	if !foundNotInEco {
		t.Fatalf("expected NotIn eco affinity on performance pod, got %+v", aff)
	}
}

func TestInjectTraceJobsStandardPodHasNoAffinity(t *testing.T) {
	job := &simJob{
		JobID:             "std-1",
		Class:             "standard",
		Namespace:         "default",
		SubmitOffsetSec:   0,
		RequestedCPUCores: 1,
		PodName:           "sim-std-1",
	}
	_, kube := newTestSimulatorWithJobs([]*simJob{job})
	pod, err := kube.CoreV1().Pods("default").Get(context.Background(), "sim-std-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	// Standard pods should have nil affinity (no power-profile constraint).
	if pod.Spec.Affinity != nil {
		t.Fatalf("expected nil affinity for standard pod, got %+v", pod.Spec.Affinity)
	}
}

func TestInjectTraceJobsNeverSetsNodeName(t *testing.T) {
	jobs := []*simJob{
		{
			JobID:             "perf-nn",
			Class:             "performance",
			Namespace:         "default",
			SubmitOffsetSec:   0,
			RequestedCPUCores: 1,
			PodName:           "sim-perf-nn",
		},
		{
			JobID:             "std-nn",
			Class:             "standard",
			Namespace:         "default",
			SubmitOffsetSec:   0,
			RequestedCPUCores: 1,
			PodName:           "sim-std-nn",
		},
	}
	_, kube := newTestSimulatorWithJobs(jobs)
	for _, j := range jobs {
		pod, err := kube.CoreV1().Pods("default").Get(context.Background(), j.PodName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get pod %s: %v", j.PodName, err)
		}
		if pod.Spec.NodeName != "" {
			t.Fatalf("pod %s has NodeName=%q; injected pods must not set NodeName (bypasses scheduler)", j.PodName, pod.Spec.NodeName)
		}
	}
}

func TestInjectTraceJobsHasSimAnnotations(t *testing.T) {
	job := &simJob{
		JobID:             "annotated-1",
		WorkloadID:        "wl-42",
		WorkloadType:      "training",
		PodRole:           "worker",
		Class:             "standard",
		Namespace:         "default",
		SubmitOffsetSec:   0,
		RequestedCPUCores: 1,
		PodName:           "sim-annotated-1",
	}
	_, kube := newTestSimulatorWithJobs([]*simJob{job})
	pod, err := kube.CoreV1().Pods("default").Get(context.Background(), "sim-annotated-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	expected := map[string]string{
		"sim.joulie.io/jobId":        "annotated-1",
		"sim.joulie.io/workloadId":   "wl-42",
		"sim.joulie.io/workloadType": "training",
		"sim.joulie.io/podRole":      "worker",
	}
	for key, want := range expected {
		if got := pod.Annotations[key]; got != want {
			t.Fatalf("annotation %s: got %q, want %q", key, got, want)
		}
	}
}

// --- Fake Prometheus query endpoint tests ---

func TestHandleFakePrometheusQueryAmbientTemp(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 100, DefaultCapW: 5000},
		nil, nil, 10, prometheus.NewRegistry(),
	)
	s.facilityAmbientTemp.Set(22.5)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=datacenter_ambient_temperature_celsius", nil)
	w := httptest.NewRecorder()
	s.handleFakePrometheusQuery(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}

	val := parsePrometheusScalarResponse(t, w.Body.Bytes())
	if math.Abs(val-22.5) > 0.01 {
		t.Errorf("expected 22.5, got %f", val)
	}
}

func TestHandleFakePrometheusQueryITPower(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 100, DefaultCapW: 5000},
		nil, nil, 10, prometheus.NewRegistry(),
	)
	s.facilityITPowerW.Set(4200.0)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=datacenter_total_it_power_watts", nil)
	w := httptest.NewRecorder()
	s.handleFakePrometheusQuery(w, req)

	val := parsePrometheusScalarResponse(t, w.Body.Bytes())
	if math.Abs(val-4200.0) > 0.01 {
		t.Errorf("expected 4200, got %f", val)
	}
}

func TestHandleFakePrometheusQueryCoolingPower(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 100, DefaultCapW: 5000},
		nil, nil, 10, prometheus.NewRegistry(),
	)
	s.facilityCoolingPowerW.Set(800.0)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=datacenter_cooling_power_watts", nil)
	w := httptest.NewRecorder()
	s.handleFakePrometheusQuery(w, req)

	val := parsePrometheusScalarResponse(t, w.Body.Bytes())
	if math.Abs(val-800.0) > 0.01 {
		t.Errorf("expected 800, got %f", val)
	}
}

func TestHandleFakePrometheusQueryPUE(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 100, DefaultCapW: 5000},
		nil, nil, 10, prometheus.NewRegistry(),
	)
	s.facilityPUE.Set(1.25)

	for _, query := range []string{"joulie_sim_facility_pue", "some_pue_metric"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/query?query="+query, nil)
		w := httptest.NewRecorder()
		s.handleFakePrometheusQuery(w, req)

		val := parsePrometheusScalarResponse(t, w.Body.Bytes())
		if math.Abs(val-1.25) > 0.01 {
			t.Errorf("query=%s: expected 1.25, got %f", query, val)
		}
	}
}

func TestHandleFakePrometheusQueryUnknownMetric(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 100, DefaultCapW: 5000},
		nil, nil, 10, prometheus.NewRegistry(),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=unknown_metric_xyz", nil)
	w := httptest.NewRecorder()
	s.handleFakePrometheusQuery(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, expected 200 with empty result", w.Code)
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []any `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("expected success, got %s", resp.Status)
	}
	if len(resp.Data.Result) != 0 {
		t.Errorf("expected empty result for unknown metric, got %d", len(resp.Data.Result))
	}
}

func TestHandleFakePrometheusQueryMissingParam(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 100, DefaultCapW: 5000},
		nil, nil, 10, prometheus.NewRegistry(),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
	w := httptest.NewRecorder()
	s.handleFakePrometheusQuery(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleFakePrometheusQuerySimMetricNames(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 100, DefaultCapW: 5000},
		nil, nil, 10, prometheus.NewRegistry(),
	)
	s.facilityAmbientTemp.Set(18.0)
	s.facilityITPowerW.Set(3000.0)
	s.facilityCoolingPowerW.Set(500.0)

	tests := []struct {
		query string
		want  float64
	}{
		{"joulie_sim_facility_ambient_temp_celsius", 18.0},
		{"joulie_sim_facility_it_power_watts", 3000.0},
		{"joulie_sim_facility_cooling_power_watts", 500.0},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/query?query="+tc.query, nil)
		w := httptest.NewRecorder()
		s.handleFakePrometheusQuery(w, req)

		val := parsePrometheusScalarResponse(t, w.Body.Bytes())
		if math.Abs(val-tc.want) > 0.01 {
			t.Errorf("query=%s: expected %f, got %f", tc.query, tc.want, val)
		}
	}
}

func TestUpdateFacilityMetricsComputesITAndCoolingPower(t *testing.T) {
	s := newSimulatorWithRegisterer(
		simModel{BaseIdleW: 200, DefaultCapW: 5000},
		nil, nil, 10, prometheus.NewRegistry(),
	)
	s.facilityBaseAmbientTempC = 25.0
	s.facilityTempAmplitudeC = 0.0 // no oscillation for determinism
	s.facilityTempPeriodH = 24.0
	s.workload = &workloadEngine{startTime: time.Now()}

	// Create two nodes with known power.
	nodeA := s.getNode("node-a")
	nodeA.TotalAvgPowerWatts = 300.0
	nodeB := s.getNode("node-b")
	nodeB.TotalAvgPowerWatts = 500.0

	s.updateFacilityMetrics(time.Now())

	itPower := gaugeValue(s.facilityITPowerW)
	if math.Abs(itPower-800.0) > 0.01 {
		t.Errorf("expected IT power 800W, got %.2f", itPower)
	}

	ambientTemp := gaugeValue(s.facilityAmbientTemp)
	if math.Abs(ambientTemp-25.0) > 0.5 {
		t.Errorf("expected ambient ~25C, got %.2f", ambientTemp)
	}

	coolingPower := gaugeValue(s.facilityCoolingPowerW)
	// PUE at 25C = 1.1 + 0.008*(25-15) = 1.18. Cooling = 800 * 0.18 = 144W.
	expectedCooling := 800.0 * (1.18 - 1.0)
	if math.Abs(coolingPower-expectedCooling) > 1.0 {
		t.Errorf("expected cooling ~%.0fW, got %.2f", expectedCooling, coolingPower)
	}
}

// parsePrometheusScalarResponse parses a Prometheus instant query JSON response
// and returns the scalar value. Mirrors the format expected by the operator's
// queryPrometheusScalar function.
func parsePrometheusScalarResponse(t *testing.T, body []byte) float64 {
	t.Helper()
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode prometheus response: %v", err)
	}
	if resp.Status != "success" {
		t.Fatalf("expected status=success, got %s", resp.Status)
	}
	if len(resp.Data.Result) == 0 {
		t.Fatal("empty result set")
	}
	if len(resp.Data.Result[0].Value) < 2 {
		t.Fatal("value array too short")
	}
	valStr, ok := resp.Data.Result[0].Value[1].(string)
	if !ok {
		t.Fatalf("value[1] is not a string: %T", resp.Data.Result[0].Value[1])
	}
	v, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		t.Fatalf("parse float %q: %v", valStr, err)
	}
	return v
}
