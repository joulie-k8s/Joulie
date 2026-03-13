package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
		simModel{BaseIdleW: 100, PodW: 100, DvfsDropW: 1, RaplHeadW: 5, DefaultCapW: 5000},
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
	if cpu["capWatts"].(float64) != 120 {
		t.Fatalf("expected capWatts=120 got %v", cpu["capWatts"])
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
				Class:             "general",
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
				Class:             "general",
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
				Class:             "general",
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
