package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matbun/joulie/pkg/hwinv"
	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/matbun/joulie/simulator/pkg/phys"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var podListFunc = listRunningPodsByNode
var podDetailFunc = listRunningPodsDetailed
var nodeListFunc = listNodeLabels

type nodeState struct {
	CapWatts                float64          `json:"capWatts"`
	ThrottlePct             int              `json:"throttlePct"`
	TargetThrottlePct       int              `json:"targetThrottlePct"`
	FreqScale               float64          `json:"freqScale"`
	CPUUtil                 float64          `json:"cpuUtil"`
	CPUWorkClass            string           `json:"cpuWorkClass"`
	CapSaturated            bool             `json:"capSaturated"`
	LastAction              string           `json:"lastAction"`
	LastResult              string           `json:"lastResult"`
	LastUpdate              time.Time        `json:"lastUpdate"`
	PodsRunning             int              `json:"podsRunning"`
	ByIntentClass           map[string]int   `json:"byIntentClass,omitempty"`
	GPUCapWattsPerGpu       float64          `json:"gpuCapWattsPerGpu"`
	GPUTargetCapWattsPerGpu float64          `json:"gpuTargetCapWattsPerGpu"`
	GPUUtil                 float64          `json:"gpuUtil"`
	GPUWorkClass            string           `json:"gpuWorkClass"`
	GPUPowerWatts           float64          `json:"gpuPowerWatts"`
	GPUPerfMultiplier       float64          `json:"gpuPerfMultiplier"`
	CPUSockets              []cpuSocketState `json:"cpuSockets,omitempty"`
	GPUDevices              []gpuDeviceState `json:"gpuDevices,omitempty"`
}

type cpuSocketState struct {
	Index          int     `json:"index"`
	CapWatts       float64 `json:"capWatts"`
	Utilization    float64 `json:"utilization"`
	PerfMultiplier float64 `json:"perfMultiplier"`
}

type gpuDeviceState struct {
	Index              int     `json:"index"`
	CapWatts           float64 `json:"capWatts"`
	TargetCapWatts     float64 `json:"targetCapWatts"`
	PowerWatts         float64 `json:"powerWatts"`
	PerfMultiplier     float64 `json:"perfMultiplier"`
	SettledAtTimestamp string  `json:"settledAt,omitempty"`
}

type simModel = hw.Profile
type simNodeClass = hw.Class
type simModelOverrides = hw.Overrides

type simEvent struct {
	Timestamp time.Time      `json:"timestamp"`
	Kind      string         `json:"kind"`
	Node      string         `json:"node"`
	Payload   map[string]any `json:"payload,omitempty"`
}

type runningPodInfo struct {
	Namespace   string
	Name        string
	Node        string
	IntentClass string
	JobID       string
}

type simJob struct {
	JobID             string
	Class             string
	Namespace         string
	SubmitOffsetSec   float64
	RequestedCPUCores float64
	CPUUnitsTotal     float64
	CPUUnitsRemaining float64
	SensitivityCPU    float64
	CPUWorkClass      string
	CPUUtilTarget     float64
	RequestedGPUs     float64
	GPUResourceName   string
	GPUUnitsTotal     float64
	GPUUnitsRemaining float64
	SensitivityGPU    float64
	GPUWorkClass      string
	GPUUtilTarget     float64
	MemoryIntensity   float64
	IOIntensity       float64
	CPUFeedIntensity  float64
	Submitted         bool
	Completed         bool
	SubmitAt          time.Time
	SubmittedAt       time.Time
	CompletedAt       time.Time
	PodName           string
	NodeName          string
}

type workloadEngine struct {
	startTime     time.Time
	baseSpeedCore float64
	jobs          []*simJob
	jobByID       map[string]*simJob
}

type simulator struct {
	mu            sync.RWMutex
	state         map[string]*nodeState
	model         simModel
	nodeModels    map[string]simModel
	nodeClass     map[string]string
	nodeSeen      map[string]bool
	selector      labels.Selector
	classes       []simNodeClass
	catalog       *hw.Catalog
	events        []simEvent
	eventMax      int
	energyJByNode map[string]float64
	energyTotalJ  float64
	energyLastTs  time.Time

	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	controlsTotal   *prometheus.CounterVec
	nodeCapW        *prometheus.GaugeVec
	nodeThrottlePct *prometheus.GaugeVec
	nodePowerW      *prometheus.GaugeVec
	nodePods        *prometheus.GaugeVec
	nodeClassInfo   *prometheus.GaugeVec
	nodeUtilCPU     *prometheus.GaugeVec
	nodeFreqScale   *prometheus.GaugeVec
	nodeRaplCapW    *prometheus.GaugeVec
	jobSubmitted    *prometheus.CounterVec
	jobCompleted    *prometheus.CounterVec
	jobCompletion   prometheus.Histogram
	controlResult   *prometheus.CounterVec

	workload *workloadEngine
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[joulie-simulator] ")

	baseModel := defaultBaseModelFromEnv()
	if err := hw.ValidateProfile(baseModel); err != nil {
		log.Fatalf("invalid base hardware profile: %v", err)
	}

	var selector labels.Selector
	selectorExpr := strings.TrimSpace(os.Getenv("SIM_NODE_SELECTOR"))
	if selectorExpr != "" {
		parsed, err := labels.Parse(selectorExpr)
		if err != nil {
			log.Printf("warning: invalid SIM_NODE_SELECTOR=%q: %v", selectorExpr, err)
		} else {
			selector = parsed
			log.Printf("node selector enabled: %s", selectorExpr)
		}
	}

	classes, err := loadNodeClasses(strings.TrimSpace(os.Getenv("SIM_NODE_CLASS_CONFIG")))
	if err != nil {
		log.Printf("warning: failed to load node class config: %v", err)
	}
	if len(classes) > 0 {
		log.Printf("node class config loaded classes=%d", len(classes))
	}
	catalogPath := strings.TrimSpace(envOrDefault("SIM_HARDWARE_CATALOG_PATH", "simulator/catalog/hardware.yaml"))
	catalog, err := hw.LoadCatalog(catalogPath)
	if err != nil {
		log.Printf("warning: failed to load hardware catalog path=%s err=%v", catalogPath, err)
	}

	s := newSimulator(baseModel, selector, classes, int(floatEnv("SIM_EVENT_BUFFER", 300)))
	s.catalog = catalog

	tracePath := strings.TrimSpace(os.Getenv("SIM_WORKLOAD_TRACE_PATH"))
	if tracePath != "" {
		if err := s.initWorkloadEngineFromTrace(tracePath); err != nil {
			log.Printf("warning: workload trace disabled: %v", err)
		}
	}

	if boolEnv("SIM_K8S_POD_WATCH", true) {
		go s.startPodPolling(durationEnv("SIM_POLL_INTERVAL", 15*time.Second))
	}
	if s.workload != nil {
		go s.startWorkloadLoop(durationEnv("SIM_WORKLOAD_TICK", time.Second))
	}

	addr := envOrDefault("SIM_ADDR", ":18080")
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/telemetry/", s.handleTelemetry)
	mux.HandleFunc("/control/", s.handleControl)
	mux.HandleFunc("/state/", s.handleState)
	mux.HandleFunc("/debug/nodes", s.handleDebugNodes)
	mux.HandleFunc("/debug/events", s.handleDebugEvents)
	mux.HandleFunc("/debug/energy", s.handleDebugEnergy)

	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func newSimulator(model simModel, selector labels.Selector, classes []simNodeClass, eventMax int) *simulator {
	return newSimulatorWithRegisterer(model, selector, classes, eventMax, prometheus.DefaultRegisterer)
}

func newSimulatorWithRegisterer(model simModel, selector labels.Selector, classes []simNodeClass, eventMax int, reg prometheus.Registerer) *simulator {
	if eventMax < 0 {
		eventMax = 0
	}
	s := &simulator{
		state:         map[string]*nodeState{},
		model:         model,
		nodeModels:    map[string]simModel{},
		nodeClass:     map[string]string{},
		nodeSeen:      map[string]bool{},
		selector:      selector,
		classes:       classes,
		eventMax:      eventMax,
		energyJByNode: map[string]float64{},
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_sim_requests_total",
			Help: "Total simulator HTTP requests",
		}, []string{"route", "method", "status"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "joulie_sim_request_duration_seconds",
			Help:    "Simulator request latency",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		controlsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_sim_controls_total",
			Help: "Total control actions received by simulator",
		}, []string{"node", "action"}),
		nodeCapW: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_sim_node_cap_watts",
			Help: "Current simulated cap watts by node",
		}, []string{"node"}),
		nodeThrottlePct: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_sim_node_throttle_pct",
			Help: "Current simulated DVFS throttle percentage by node",
		}, []string{"node"}),
		nodePowerW: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_sim_node_power_watts",
			Help: "Current simulated node power by node",
		}, []string{"node"}),
		nodePods: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_sim_node_running_pods",
			Help: "Running pod count by node, as observed from Kubernetes API",
		}, []string{"node"}),
		nodeClassInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_sim_node_class_info",
			Help: "Node class assignment (1 for active class, 0 for previous class labels)",
		}, []string{"node", "class"}),
		nodeUtilCPU: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_sim_node_cpu_util",
			Help: "Current simulated CPU utilization by node",
		}, []string{"node"}),
		nodeFreqScale: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_sim_node_freq_scale",
			Help: "Current simulated CPU frequency scale by node",
		}, []string{"node"}),
		nodeRaplCapW: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_sim_node_rapl_cap_watts",
			Help: "Current simulated RAPL cap watts by node",
		}, []string{"node"}),
		jobSubmitted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_sim_job_submitted_total",
			Help: "Total workload jobs submitted by class",
		}, []string{"class"}),
		jobCompleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_sim_job_completed_total",
			Help: "Total workload jobs completed by class and node",
		}, []string{"class", "node"}),
		jobCompletion: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "joulie_sim_job_completion_seconds",
			Help:    "Completion latency of simulated jobs",
			Buckets: prometheus.DefBuckets,
		}),
		controlResult: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_sim_control_actions_total",
			Help: "Control actions by node/action/result",
		}, []string{"node", "action", "result"}),
	}

	reg.MustRegister(
		s.requestsTotal,
		s.requestDuration,
		s.controlsTotal,
		s.nodeCapW,
		s.nodeThrottlePct,
		s.nodePowerW,
		s.nodePods,
		s.nodeClassInfo,
		s.nodeUtilCPU,
		s.nodeFreqScale,
		s.nodeRaplCapW,
		s.jobSubmitted,
		s.jobCompleted,
		s.jobCompletion,
		s.controlResult,
	)
	return s
}

func (s *simulator) getNode(node string) *nodeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.state[node]
	if !ok {
		model := s.model
		if m, ok := s.nodeModels[node]; ok {
			model = m
		}
		st = &nodeState{
			CapWatts:                model.DefaultCapW,
			ThrottlePct:             0,
			TargetThrottlePct:       0,
			FreqScale:               1.0,
			CPUUtil:                 0,
			CPUWorkClass:            "cpu.mixed",
			LastAction:              "none",
			LastResult:              "none",
			LastUpdate:              time.Now().UTC(),
			ByIntentClass:           map[string]int{},
			GPUCapWattsPerGpu:       model.GPU.MaxWattsPerGPU,
			GPUTargetCapWattsPerGpu: model.GPU.MaxWattsPerGPU,
			GPUWorkClass:            "gpu.mixed",
			GPUPerfMultiplier:       1.0,
		}
		cpuSockets := model.CPUSockets
		if cpuSockets <= 0 {
			cpuSockets = 2
		}
		st.CPUSockets = make([]cpuSocketState, 0, cpuSockets)
		for i := 0; i < cpuSockets; i++ {
			st.CPUSockets = append(st.CPUSockets, cpuSocketState{
				Index:          i,
				CapWatts:       model.CPUSocketCapMaxW,
				Utilization:    0,
				PerfMultiplier: 1.0,
			})
		}
		if model.GPU.Count > 0 {
			st.GPUDevices = make([]gpuDeviceState, 0, model.GPU.Count)
			for i := 0; i < model.GPU.Count; i++ {
				cap := model.GPU.MaxWattsPerGPU
				st.GPUDevices = append(st.GPUDevices, gpuDeviceState{
					Index:          i,
					CapWatts:       cap,
					TargetCapWatts: cap,
				})
			}
		}
		s.state[node] = st
	}
	return st
}

func (s *simulator) modelForNode(node string) simModel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.nodeModels[node]; ok {
		return m
	}
	return s.model
}

func (s *simulator) nodePower(node string, st *nodeState) float64 {
	model := s.modelForNode(node)
	return s.nodePowerWithModel(st, model)
}

func (s *simulator) nodePowerWithModel(st *nodeState, model simModel) float64 {
	s.updateNodeDynamicsWithModel(st, model)
	util := clamp01(st.CPUUtil)
	freq := clamp01(st.FreqScale)
	cpuClass := strings.TrimSpace(st.CPUWorkClass)
	if cpuClass == "" {
		cpuClass = "cpu.mixed"
	}
	p := 0.0
	if len(model.CPUPowerCurve) > 0 {
		cpuModel := phys.MeasuredCurveCPUModel{
			Points: model.CPUPowerCurve,
			Knee:   cpuKneeFromModel(model),
		}
		p = cpuModel.Power(phys.DeviceState{
			Utilization: util,
			FreqScale:   freq,
			CapWatts:    st.CapWatts,
			Class:       cpuClass,
		})
	} else {
		alpha := model.AlphaUtil
		if alpha <= 0 {
			alpha = 1
		}
		beta := model.BetaFreq
		if beta <= 0 {
			beta = 1
		}
		p = model.BaseIdleW + (model.PMaxW-model.BaseIdleW)*math.Pow(util, alpha)*math.Pow(freq, beta)
	}
	p = math.Max(20, p)

	capW := st.CapWatts
	if capW < model.RaplCapMinW {
		capW = model.RaplCapMinW
	}
	if model.RaplCapMaxW > 0 && capW > model.RaplCapMaxW {
		capW = model.RaplCapMaxW
	}
	minFreq := minFreqScale(model)
	alpha := model.AlphaUtil
	if alpha <= 0 {
		alpha = 1
	}
	beta := model.BetaFreq
	if beta <= 0 {
		beta = 1
	}
	minPower := model.BaseIdleW + (model.PMaxW-model.BaseIdleW)*math.Pow(util, alpha)*math.Pow(minFreq, beta)
	st.CapSaturated = false
	if p > capW {
		st.CapSaturated = minPower > capW
		targetFreq := solveFreqScaleForCap(model, util, capW, cpuClass)
		st.FreqScale = math.Max(minFreq, math.Min(st.FreqScale, targetFreq))
		freq = clamp01(st.FreqScale)
		if len(model.CPUPowerCurve) > 0 {
			cpuModel := phys.MeasuredCurveCPUModel{
				Points: model.CPUPowerCurve,
				Knee:   cpuKneeFromModel(model),
			}
			p = cpuModel.Power(phys.DeviceState{
				Utilization: util,
				FreqScale:   freq,
				CapWatts:    capW,
				Class:       cpuClass,
			})
		} else {
			alpha := model.AlphaUtil
			if alpha <= 0 {
				alpha = 1
			}
			beta := model.BetaFreq
			if beta <= 0 {
				beta = 1
			}
			p = model.BaseIdleW + (model.PMaxW-model.BaseIdleW)*math.Pow(util, alpha)*math.Pow(freq, beta)
		}
	}
	p = math.Min(p, capW+model.RaplHeadW)
	gpu := gpuPowerWithModel(st, model)
	st.GPUPowerWatts = gpu
	return math.Round((p+gpu)*100) / 100
}

func gpuPowerWithModel(st *nodeState, model simModel) float64 {
	if model.GPU.Count <= 0 || model.GPU.MaxWattsPerGPU <= 0 {
		return 0
	}
	if len(st.GPUDevices) == 0 {
		for i := 0; i < model.GPU.Count; i++ {
			st.GPUDevices = append(st.GPUDevices, gpuDeviceState{
				Index:          i,
				CapWatts:       model.GPU.MaxWattsPerGPU,
				TargetCapWatts: model.GPU.MaxWattsPerGPU,
			})
		}
	}
	gpuModel := phys.CappedBoardGPUModel{
		IdleW:         model.GPU.IdleWattsPerGPU,
		MaxW:          model.GPU.MaxWattsPerGPU,
		ComputeGamma:  model.GPU.ComputeGamma,
		MemoryEpsilon: model.GPU.MemoryEpsilon,
		MemoryGamma:   model.GPU.MemoryGamma,
	}
	total := 0.0
	perfMul := 0.0
	class := strings.TrimSpace(st.GPUWorkClass)
	if class == "" {
		class = "gpu.mixed"
	}
	for i := range st.GPUDevices {
		d := &st.GPUDevices[i]
		p := gpuModel.Power(phys.DeviceState{
			Utilization:    clamp01(st.GPUUtil),
			CapWatts:       d.CapWatts,
			MaxCapWatts:    model.GPU.MaxWattsPerGPU,
			IdlePowerWatts: model.GPU.IdleWattsPerGPU,
			Class:          class,
		})
		t := gpuModel.ThroughputMultiplier(phys.DeviceState{
			Utilization:    clamp01(st.GPUUtil),
			CapWatts:       d.CapWatts,
			MaxCapWatts:    model.GPU.MaxWattsPerGPU,
			IdlePowerWatts: model.GPU.IdleWattsPerGPU,
			Class:          class,
		}, class)
		d.PowerWatts = p
		d.PerfMultiplier = t
		total += p
		perfMul += t
	}
	if len(st.GPUDevices) > 0 {
		st.GPUPerfMultiplier = perfMul / float64(len(st.GPUDevices))
	} else {
		st.GPUPerfMultiplier = 1.0
	}
	return math.Round(total*100) / 100
}

func (s *simulator) updateNodeMetrics(node string, st *nodeState) {
	power := s.nodePower(node, st)
	s.nodeCapW.WithLabelValues(node).Set(st.CapWatts)
	s.nodeThrottlePct.WithLabelValues(node).Set(float64(st.ThrottlePct))
	s.nodePowerW.WithLabelValues(node).Set(power)
	s.nodePods.WithLabelValues(node).Set(float64(st.PodsRunning))
	s.nodeUtilCPU.WithLabelValues(node).Set(st.CPUUtil)
	s.nodeFreqScale.WithLabelValues(node).Set(st.FreqScale)
	s.nodeRaplCapW.WithLabelValues(node).Set(st.CapWatts)
}

func (s *simulator) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *simulator) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	defer s.observeRequest("telemetry", r.Method, &status, start)
	if r.Method != http.MethodGet {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	node := strings.TrimPrefix(r.URL.Path, "/telemetry/")
	if node == "" {
		status = http.StatusBadRequest
		http.Error(w, "missing node", status)
		return
	}
	st := s.getNode(node)
	s.updateNodeMetrics(node, st)
	power := s.nodePower(node, st)
	modelForTelemetry := s.modelForNode(node)
	s.recordEvent("telemetry", node, map[string]any{
		"packagePowerWatts": power,
		"capWatts":          st.CapWatts,
		"throttlePct":       st.ThrottlePct,
		"freqScale":         st.FreqScale,
		"cpuUtil":           st.CPUUtil,
		"cpuWorkClass":      st.CPUWorkClass,
		"capSaturated":      st.CapSaturated,
		"podsRunning":       st.PodsRunning,
		"gpuPowerWatts":     st.GPUPowerWatts,
		"gpuCapWattsPerGpu": st.GPUCapWattsPerGpu,
		"gpuUtil":           st.GPUUtil,
		"gpuWorkClass":      st.GPUWorkClass,
		"gpuPerfMultiplier": st.GPUPerfMultiplier,
	})
	log.Printf("telemetry node=%s powerW=%.2f capW=%.2f throttlePct=%d pods=%d", node, power, st.CapWatts, st.ThrottlePct, st.PodsRunning)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"node":              node,
		"packagePowerWatts": power,
		"cpu": map[string]any{
			"packagePowerWatts": power,
			"utilization":       st.CPUUtil,
			"workClass":         st.CPUWorkClass,
			"freqScale":         st.FreqScale,
			"throttlePct":       st.ThrottlePct,
			"capWatts":          st.CapWatts,
			"raplCapWatts":      st.CapWatts,
			"capSaturated":      st.CapSaturated,
		},
		"gpu": map[string]any{
			"present":               modelForTelemetry.GPU.Count > 0,
			"vendor":                modelForTelemetry.GPU.Vendor,
			"product":               modelForTelemetry.GPU.Product,
			"count":                 modelForTelemetry.GPU.Count,
			"powerWattsTotal":       st.GPUPowerWatts,
			"capWattsPerGpuApplied": st.GPUCapWattsPerGpu,
			"capWattsPerGpuTarget":  st.GPUTargetCapWattsPerGpu,
			"utilization":           st.GPUUtil,
			"workClass":             st.GPUWorkClass,
			"perfMultiplier":        st.GPUPerfMultiplier,
			"devices":               st.GPUDevices,
		},
		"hardwareModel": map[string]any{
			"cpuModel":       modelForTelemetry.CPUModel,
			"cpuProvenance":  modelForTelemetry.CPUProvenance,
			"cpuCurveSource": modelForTelemetry.CPUCurveSource,
			"cpuProxyFrom":   modelForTelemetry.CPUProxyFrom,
			"gpuProvenance":  modelForTelemetry.GPU.Provenance,
		},
		"pods": map[string]any{
			"running":       st.PodsRunning,
			"byIntentClass": st.ByIntentClass,
		},
		"ts": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *simulator) handleControl(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	defer s.observeRequest("control", r.Method, &status, start)
	if r.Method != http.MethodPost {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	node := strings.TrimPrefix(r.URL.Path, "/control/")
	if node == "" {
		status = http.StatusBadRequest
		http.Error(w, "missing node", status)
		return
	}

	var payload struct {
		Action         string  `json:"action"`
		CapWatts       float64 `json:"capWatts"`
		ThrottlePct    int     `json:"throttlePct"`
		CapWattsPerGpu float64 `json:"capWattsPerGpu"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		status = http.StatusBadRequest
		http.Error(w, "invalid json", status)
		return
	}

	st := s.getNode(node)
	result := "applied"
	message := "ok"
	model := s.modelForNode(node)
	s.mu.Lock()
	switch payload.Action {
	case "rapl.set_power_cap_watts":
		if payload.CapWatts > 0 {
			capW := payload.CapWatts
			if capW < model.RaplCapMinW {
				capW = model.RaplCapMinW
			}
			if model.RaplCapMaxW > 0 && capW > model.RaplCapMaxW {
				capW = model.RaplCapMaxW
			}
			st.CapWatts = capW
		}
	case "dvfs.set_throttle_pct":
		if payload.ThrottlePct < 0 {
			payload.ThrottlePct = 0
		}
		if payload.ThrottlePct > 100 {
			payload.ThrottlePct = 100
		}
		st.TargetThrottlePct = payload.ThrottlePct
	case "gpu.set_power_cap_watts":
		if model.GPU.Count <= 0 {
			result = "blocked"
			message = "gpu not present on node model"
			break
		}
		if payload.CapWattsPerGpu <= 0 {
			result = "error"
			message = "capWattsPerGpu must be > 0"
			break
		}
		cap := payload.CapWattsPerGpu
		if cap < model.GPU.MinCapWattsPerGPU {
			cap = model.GPU.MinCapWattsPerGPU
		}
		if cap > model.GPU.MaxWattsPerGPU {
			cap = model.GPU.MaxWattsPerGPU
		}
		st.GPUTargetCapWattsPerGpu = cap
		if len(st.GPUDevices) == 0 {
			for i := 0; i < model.GPU.Count; i++ {
				st.GPUDevices = append(st.GPUDevices, gpuDeviceState{
					Index:          i,
					CapWatts:       model.GPU.MaxWattsPerGPU,
					TargetCapWatts: cap,
				})
			}
		}
		for i := range st.GPUDevices {
			st.GPUDevices[i].TargetCapWatts = cap
		}
	default:
		result = "error"
		message = "unsupported action"
	}
	st.LastAction = payload.Action
	st.LastResult = result
	st.LastUpdate = time.Now().UTC()
	s.mu.Unlock()

	s.controlsTotal.WithLabelValues(node, payload.Action).Inc()
	s.controlResult.WithLabelValues(node, payload.Action, result).Inc()
	s.updateNodeMetrics(node, st)
	power := s.nodePower(node, st)
	s.recordEvent("control", node, map[string]any{
		"action":            payload.Action,
		"capWatts":          st.CapWatts,
		"throttlePct":       st.ThrottlePct,
		"powerWatts":        power,
		"podsRunning":       st.PodsRunning,
		"gpuCapWattsPerGpu": st.GPUCapWattsPerGpu,
	})
	log.Printf("control node=%s action=%s capW=%.2f throttlePct=%d powerW=%.2f pods=%d", node, payload.Action, st.CapWatts, st.ThrottlePct, power, st.PodsRunning)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      result == "applied",
		"result":  result,
		"node":    node,
		"message": message,
		"state": map[string]any{
			"capWatts":                 st.CapWatts,
			"throttlePct":              st.ThrottlePct,
			"targetThrottlePct":        st.TargetThrottlePct,
			"freqScale":                st.FreqScale,
			"cpuUtil":                  st.CPUUtil,
			"lastAction":               st.LastAction,
			"lastResult":               st.LastResult,
			"lastUpdate":               st.LastUpdate.Format(time.RFC3339),
			"podsRunning":              st.PodsRunning,
			"gpuCapWattsPerGpuApplied": st.GPUCapWattsPerGpu,
			"gpuCapWattsPerGpuTarget":  st.GPUTargetCapWattsPerGpu,
			"gpuDevices":               st.GPUDevices,
		},
		"simulatedPackagePowerWatts": power,
	})
}

func (s *simulator) handleState(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	defer s.observeRequest("state", r.Method, &status, start)
	if r.Method != http.MethodGet {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	node := strings.TrimPrefix(r.URL.Path, "/state/")
	if node == "" {
		status = http.StatusBadRequest
		http.Error(w, "missing node", status)
		return
	}
	st := s.getNode(node)
	s.updateNodeMetrics(node, st)
	_ = json.NewEncoder(w).Encode(map[string]any{"node": node, "state": st})
}

func (s *simulator) handleDebugNodes(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	defer s.observeRequest("debug_nodes", r.Method, &status, start)
	if r.Method != http.MethodGet {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	type debugNode struct {
		Node      string     `json:"node"`
		Selected  bool       `json:"selected"`
		Class     string     `json:"class"`
		Model     simModel   `json:"model"`
		State     *nodeState `json:"state,omitempty"`
		Known     bool       `json:"known"`
		SeenByAPI bool       `json:"seenByApi"`
	}

	s.mu.RLock()
	nodes := make([]string, 0, len(s.nodeSeen))
	for n := range s.nodeSeen {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	out := make([]debugNode, 0, len(nodes))
	for _, n := range nodes {
		_, selected := s.nodeModels[n]
		class := s.nodeClass[n]
		if class == "" {
			class = "default"
		}
		model := s.model
		if m, ok := s.nodeModels[n]; ok {
			model = m
		}
		var stCopy *nodeState
		if st, ok := s.state[n]; ok {
			cp := *st
			stCopy = &cp
		}
		out = append(out, debugNode{
			Node:      n,
			Selected:  selected,
			Class:     class,
			Model:     model,
			State:     stCopy,
			Known:     stCopy != nil,
			SeenByAPI: s.nodeSeen[n],
		})
	}
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"count": len(out),
		"nodes": out,
	})
}

func (s *simulator) handleDebugEvents(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	defer s.observeRequest("debug_events", r.Method, &status, start)
	if r.Method != http.MethodGet {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	s.mu.RLock()
	events := append([]simEvent(nil), s.events...)
	s.mu.RUnlock()
	limit := int(floatEnv("SIM_DEBUG_EVENT_LIMIT", 200))
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"count":  len(events),
		"events": events,
	})
}

func (s *simulator) handleDebugEnergy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	defer s.observeRequest("debug_energy", r.Method, &status, start)
	if r.Method != http.MethodGet {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	s.mu.RLock()
	perNode := map[string]float64{}
	for node, v := range s.energyJByNode {
		perNode[node] = v
	}
	total := s.energyTotalJ
	last := s.energyLastTs
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"totalJoules":  total,
		"byNodeJoules": perNode,
		"lastUpdated":  last.Format(time.RFC3339Nano),
	})
}

func (s *simulator) observeRequest(route, method string, status *int, start time.Time) {
	s.requestsTotal.WithLabelValues(route, method, strconv.Itoa(*status)).Inc()
	s.requestDuration.WithLabelValues(route, method).Observe(time.Since(start).Seconds())
}

func (s *simulator) accumulateEnergy(dt float64) {
	if dt <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for node, st := range s.state {
		if st == nil {
			continue
		}
		model := s.model
		if m, ok := s.nodeModels[node]; ok {
			model = m
		}
		p := s.nodePowerWithModel(st, model)
		e := p * dt
		if e <= 0 {
			continue
		}
		s.energyJByNode[node] += e
		s.energyTotalJ += e
	}
	s.energyLastTs = time.Now().UTC()
}

func (s *simulator) startPodPolling(interval time.Duration) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("warning: pod polling disabled (no in-cluster config): %v", err)
		return
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Printf("warning: pod polling disabled (kube client): %v", err)
		return
	}
	log.Printf("pod polling enabled interval=%s", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		counts, err := podListFunc(ctx, kube)
		podsDetailed, detailErr := podDetailFunc(ctx, kube)
		nodes, nodeErr := nodeListFunc(ctx, kube)
		cancel()
		if err != nil {
			log.Printf("warning: pod polling list failed: %v", err)
			<-ticker.C
			continue
		}
		if detailErr != nil {
			log.Printf("warning: detailed pod list failed: %v", detailErr)
			<-ticker.C
			continue
		}
		if nodeErr != nil {
			log.Printf("warning: node polling list failed: %v", nodeErr)
			<-ticker.C
			continue
		}

		s.refreshNodeStateFromKubeData(counts, podsDetailed, nodes)

		for node, labels := range nodes {
			if !s.nodeSelected(labels) {
				continue
			}
			st := s.getNode(node)
			s.updateNodeMetrics(node, st)
		}
		<-ticker.C
	}
}

func (s *simulator) refreshNodeStateFromKubeData(counts map[string]int, pods []runningPodInfo, nodeLabels map[string]map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	selected := map[string]bool{}
	intentByNode := map[string]map[string]int{}
	for node := range nodeLabels {
		s.nodeSeen[node] = true
	}
	for _, p := range pods {
		if p.Node == "" {
			continue
		}
		if _, ok := intentByNode[p.Node]; !ok {
			intentByNode[p.Node] = map[string]int{}
		}
		intent := p.IntentClass
		if intent == "" {
			intent = "unknown"
		}
		intentByNode[p.Node][intent]++
	}
	for node, labels := range nodeLabels {
		if !s.nodeSelected(labels) {
			if prevClass := s.nodeClass[node]; prevClass != "" {
				s.nodeClassInfo.WithLabelValues(node, prevClass).Set(0)
			}
			delete(s.nodeModels, node)
			delete(s.nodeClass, node)
			continue
		}
		selected[node] = true
		model, className := s.resolveModelForNode(labels)
		prevClass := s.nodeClass[node]
		if prevClass != "" && prevClass != className {
			s.nodeClassInfo.WithLabelValues(node, prevClass).Set(0)
		}
		s.nodeModels[node] = model
		s.nodeClass[node] = className
		s.nodeClassInfo.WithLabelValues(node, className).Set(1)
		st, ok := s.state[node]
		if !ok {
			st = &nodeState{
				CapWatts:                model.DefaultCapW,
				FreqScale:               1,
				CPUWorkClass:            "cpu.mixed",
				LastAction:              "none",
				LastResult:              "none",
				LastUpdate:              time.Now().UTC(),
				ByIntentClass:           map[string]int{},
				TargetThrottlePct:       0,
				GPUCapWattsPerGpu:       model.GPU.MaxWattsPerGPU,
				GPUTargetCapWattsPerGpu: model.GPU.MaxWattsPerGPU,
				GPUWorkClass:            "gpu.mixed",
				GPUPerfMultiplier:       1.0,
			}
			cpuSockets := model.CPUSockets
			if cpuSockets <= 0 {
				cpuSockets = 2
			}
			st.CPUSockets = make([]cpuSocketState, 0, cpuSockets)
			for i := 0; i < cpuSockets; i++ {
				st.CPUSockets = append(st.CPUSockets, cpuSocketState{Index: i, CapWatts: model.CPUSocketCapMaxW, PerfMultiplier: 1.0})
			}
			if model.GPU.Count > 0 {
				st.GPUDevices = make([]gpuDeviceState, 0, model.GPU.Count)
				for i := 0; i < model.GPU.Count; i++ {
					st.GPUDevices = append(st.GPUDevices, gpuDeviceState{
						Index:          i,
						CapWatts:       model.GPU.MaxWattsPerGPU,
						TargetCapWatts: model.GPU.MaxWattsPerGPU,
					})
				}
			}
			s.state[node] = st
		}
	}

	for node, st := range s.state {
		if !selected[node] {
			st.PodsRunning = 0
			st.ByIntentClass = map[string]int{}
			st.CPUUtil = 0
			st.CPUWorkClass = "cpu.mixed"
			st.GPUUtil = 0
			st.GPUWorkClass = "gpu.mixed"
			continue
		}
		st.PodsRunning = counts[node]
		st.ByIntentClass = intentByNode[node]
		if st.ByIntentClass == nil {
			st.ByIntentClass = map[string]int{}
		}
		if s.workload == nil || !s.workload.overrideNodeUtil(st, node) {
			st.CPUUtil = math.Min(1, float64(st.PodsRunning)*0.12)
		}
		if s.workload == nil {
			st.GPUUtil = 0
		}
	}
}

func (s *simulator) nodeSelected(nodeLabels map[string]string) bool {
	if s.selector == nil || s.selector.Empty() {
		return true
	}
	return s.selector.Matches(labels.Set(nodeLabels))
}

func (s *simulator) resolveModelForNode(nodeLabels map[string]string) (simModel, string) {
	model := s.model
	className := "default"
	for _, c := range s.classes {
		if !matchLabels(nodeLabels, c.MatchLabels) {
			continue
		}
		className = c.Name
		model = applyModelOverrides(model, c.Model)
		break
	}
	model, catalogClass := s.applyCatalogModelDefaults(model, nodeLabels)
	if catalogClass != "" {
		return model, catalogClass
	}
	return model, className
}

func (s *simulator) applyCatalogModelDefaults(model simModel, nodeLabels map[string]string) (simModel, string) {
	if s.catalog == nil {
		return model, ""
	}
	desc := hwinv.NodeDescriptor{
		CPUModelRaw: firstNonEmpty(
			nodeLabels["joulie.io/hw.cpu-model"],
			nodeLabels["feature.node.kubernetes.io/cpu-model.name"],
		),
		CPUSockets: hwinv.ParseIntString(nodeLabels["joulie.io/hw.cpu-sockets"]),
		GPUModelRaw: firstNonEmpty(
			nodeLabels["joulie.io/hw.gpu-model"],
			nodeLabels["joulie.io/gpu.product"],
			nodeLabels["nvidia.com/gpu.product"],
			nodeLabels["amd.com/gpu.product"],
			nodeLabels["amd.com/gpu.family"],
		),
		GPUCount: hwinv.ParseIntString(nodeLabels["joulie.io/hw.gpu-count"]),
	}
	match := s.catalog.MatchNode(desc)
	if match.CPUSpec != nil {
		cpuSpec := *match.CPUSpec
		model.CPUModel = match.CPUKey
		model.CPUProvenance = cpuSpec.Provenance
		if desc.CPUSockets > 0 {
			model.CPUSockets = desc.CPUSockets
		}
		model.CPUDriverFamily = cpuSpec.Official.DriverFamily
		if cpuSpec.MeasuredCurves != nil && cpuSpec.MeasuredCurves.Node2S != nil && len(model.CPUPowerCurve) == 0 {
			points := make([]hw.PowerPoint, 0, len(cpuSpec.MeasuredCurves.Node2S.Points))
			for _, p := range cpuSpec.MeasuredCurves.Node2S.Points {
				points = append(points, hw.PowerPoint{LoadPct: p.LoadPct, PowerW: p.PowerW})
			}
			model.CPUPowerCurve = append([]hw.PowerPoint(nil), points...)
			model.CPUCurveSource = cpuSpec.MeasuredCurves.Node2S.Source
		}
		if cpuSpec.ProxyFrom != nil {
			model.CPUProxyFrom = cpuSpec.ProxyFrom.Family
		}
		if model.CPUSocketCapMaxW <= 0 && cpuSpec.Official.TDPW > 0 {
			model.CPUSocketCapMaxW = cpuSpec.Official.TDPW
		}
		if model.CPUSocketCapMinW <= 0 && cpuSpec.Official.TDPW > 0 {
			model.CPUSocketCapMinW = cpuSpec.Official.TDPW * 0.55
		}
	}
	if match.GPUSpec != nil {
		gpuSpec := *match.GPUSpec
		model.GPU.Provenance = gpuSpec.Provenance
		model.GPU.Vendor = gpuSpec.Official.Vendor
		model.GPU.Product = match.GPUKey
		if desc.GPUCount > 0 {
			model.GPU.Count = desc.GPUCount
		}
		if model.GPU.MaxWattsPerGPU <= 0 {
			model.GPU.MaxWattsPerGPU = gpuSpec.Official.MaxBoardPowerW
		}
		if model.GPU.MinCapWattsPerGPU <= 0 {
			model.GPU.MinCapWattsPerGPU = gpuSpec.Official.MinBoardPowerW
			if model.GPU.MinCapWattsPerGPU <= 0 {
				model.GPU.MinCapWattsPerGPU = model.GPU.MaxWattsPerGPU * 0.5
			}
		}
	}
	nameParts := []string{"default"}
	if match.CPUKey != "" {
		nameParts = append(nameParts, strings.ToLower(strings.ReplaceAll(match.CPUKey, "_", "-")))
	}
	if match.GPUKey != "" {
		nameParts = append(nameParts, strings.ToLower(strings.ReplaceAll(match.GPUKey, "_", "-")))
	}
	return model, strings.Join(nameParts, "+")
}

func listRunningPodsByNode(ctx context.Context, kube kubernetes.Interface) (map[string]int, error) {
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, p := range pods.Items {
		if p.Spec.NodeName == "" {
			continue
		}
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase != "Running" {
			continue
		}
		counts[p.Spec.NodeName]++
	}
	return counts, nil
}

func listRunningPodsDetailed(ctx context.Context, kube kubernetes.Interface) ([]runningPodInfo, error) {
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]runningPodInfo, 0, len(pods.Items))
	for _, p := range pods.Items {
		if p.Spec.NodeName == "" {
			continue
		}
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase != "Running" && p.Status.Phase != "Pending" {
			continue
		}
		out = append(out, runningPodInfo{
			Namespace:   p.Namespace,
			Name:        p.Name,
			Node:        p.Spec.NodeName,
			IntentClass: classifyClassFromPodSpec(&p.Spec),
			JobID:       p.Annotations["sim.joulie.io/jobId"],
		})
	}
	return out, nil
}

func listNodeLabels(ctx context.Context, kube kubernetes.Interface) (map[string]map[string]string, error) {
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]string, len(nodes.Items))
	for _, n := range nodes.Items {
		labelsCopy := map[string]string{}
		for k, v := range n.GetLabels() {
			labelsCopy[k] = v
		}
		out[n.GetName()] = labelsCopy
	}
	return out, nil
}

func loadNodeClasses(path string) ([]simNodeClass, error) {
	base := defaultBaseModelFromEnv()
	return hw.LoadClasses(path, base)
}

func matchLabels(nodeLabels, required map[string]string) bool {
	for k, v := range required {
		if nodeLabels[k] != v {
			return false
		}
	}
	return true
}

func applyModelOverrides(base simModel, o simModelOverrides) simModel {
	return hw.ApplyOverrides(base, o)
}

func (s *simulator) recordEvent(kind, node string, payload map[string]any) {
	if s.eventMax <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, simEvent{
		Timestamp: time.Now().UTC(),
		Kind:      kind,
		Node:      node,
		Payload:   payload,
	})
	if len(s.events) > s.eventMax {
		s.events = s.events[len(s.events)-s.eventMax:]
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func cpuKneeFromModel(model simModel) float64 {
	if model.CPULowestNonlinearFreqMHz > 0 && model.FMaxMHz > 0 {
		return clamp01(model.CPULowestNonlinearFreqMHz / model.FMaxMHz)
	}
	return 0.7
}

func cpuThroughputMultiplier(freqScale float64, workloadClass string, model simModel) float64 {
	m := phys.MeasuredCurveCPUModel{
		Points: model.CPUPowerCurve,
		Knee:   cpuKneeFromModel(model),
	}
	return m.ThroughputMultiplier(phys.DeviceState{
		FreqScale: freqScale,
		Class:     workloadClass,
	}, workloadClass)
}

func minFreqScale(model simModel) float64 {
	if model.FMaxMHz <= 0 || model.FMinMHz <= 0 {
		return 0.35
	}
	return clamp01(model.FMinMHz / model.FMaxMHz)
}

func solveFreqScaleForCap(model simModel, util, capW float64, workloadClass string) float64 {
	util = clamp01(util)
	if util <= 0 || model.PMaxW <= model.BaseIdleW {
		return 1
	}
	if len(model.CPUPowerCurve) > 0 {
		lo := minFreqScale(model)
		hi := 1.0
		curve := phys.MeasuredCurveCPUModel{
			Points: model.CPUPowerCurve,
			Knee:   cpuKneeFromModel(model),
		}
		for i := 0; i < 22; i++ {
			mid := (lo + hi) / 2.0
			p := curve.Power(phys.DeviceState{
				Utilization: util,
				FreqScale:   mid,
				CapWatts:    capW,
				Class:       workloadClass,
			})
			if p > capW {
				hi = mid
			} else {
				lo = mid
			}
		}
		return clamp01(lo)
	}
	alpha := model.AlphaUtil
	if alpha <= 0 {
		alpha = 1
	}
	beta := model.BetaFreq
	if beta <= 0 {
		beta = 1
	}
	den := (model.PMaxW - model.BaseIdleW) * math.Pow(util, alpha)
	if den <= 0 {
		return 1
	}
	x := (capW - model.BaseIdleW) / den
	if x <= 0 {
		return minFreqScale(model)
	}
	return math.Pow(x, 1.0/beta)
}

func (s *simulator) updateNodeDynamics(node string, st *nodeState) {
	model := s.modelForNode(node)
	s.updateNodeDynamicsWithModel(st, model)
}

func (s *simulator) updateNodeDynamicsWithModel(st *nodeState, model simModel) {
	targetScale := 1.0 - clamp01(float64(st.TargetThrottlePct)/100.0)
	now := time.Now().UTC()
	last := st.LastUpdate
	if last.IsZero() {
		st.LastUpdate = now
		last = now
	}
	dt := now.Sub(last).Seconds()
	if dt < 0 {
		dt = 0
	}
	rampSec := math.Max(0.05, float64(model.DvfsRampMS)/1000.0)
	if st.FreqScale == 0 {
		st.FreqScale = 1
	}
	maxStep := dt / rampSec
	if maxStep > 1 {
		maxStep = 1
	}
	st.FreqScale = st.FreqScale + (targetScale-st.FreqScale)*maxStep
	st.FreqScale = math.Max(minFreqScale(model), clamp01(st.FreqScale))
	st.ThrottlePct = int(math.Round((1.0 - st.FreqScale) * 100.0))

	if len(st.CPUSockets) == 0 {
		sockets := model.CPUSockets
		if sockets <= 0 {
			sockets = 2
		}
		st.CPUSockets = make([]cpuSocketState, 0, sockets)
		for i := 0; i < sockets; i++ {
			st.CPUSockets = append(st.CPUSockets, cpuSocketState{Index: i})
		}
	}
	perSocketUtil := st.CPUUtil / float64(maxIntInt(1, len(st.CPUSockets)))
	cpuClass := strings.TrimSpace(st.CPUWorkClass)
	if cpuClass == "" {
		cpuClass = "cpu.mixed"
	}
	for i := range st.CPUSockets {
		st.CPUSockets[i].Utilization = clamp01(perSocketUtil)
		st.CPUSockets[i].PerfMultiplier = cpuThroughputMultiplier(st.FreqScale, cpuClass, model)
		if model.CPUSocketCapMaxW > 0 {
			st.CPUSockets[i].CapWatts = model.CPUSocketCapMaxW
		}
	}

	tauMs := model.GPU.CapApplyTauMS
	if tauMs <= 0 {
		tauMs = 150
		if strings.EqualFold(model.GPU.Product, "AMD_Instinct_MI300X") || strings.Contains(strings.ToLower(model.GPU.Product), "mi300x") {
			tauMs = 350
		}
	}
	if model.GPU.Count > 0 && len(st.GPUDevices) == 0 {
		st.GPUDevices = make([]gpuDeviceState, 0, model.GPU.Count)
		for i := 0; i < model.GPU.Count; i++ {
			c := model.GPU.MaxWattsPerGPU
			st.GPUDevices = append(st.GPUDevices, gpuDeviceState{Index: i, CapWatts: c, TargetCapWatts: c})
		}
	}
	if st.GPUTargetCapWattsPerGpu <= 0 {
		st.GPUTargetCapWattsPerGpu = st.GPUCapWattsPerGpu
	}
	tauSec := math.Max(0.01, float64(tauMs)/1000.0)
	gpuStep := dt / tauSec
	if gpuStep > 1 {
		gpuStep = 1
	}
	for i := range st.GPUDevices {
		d := &st.GPUDevices[i]
		target := st.GPUTargetCapWattsPerGpu
		if target <= 0 {
			target = model.GPU.MaxWattsPerGPU
		}
		if model.GPU.MinCapWattsPerGPU > 0 && target < model.GPU.MinCapWattsPerGPU {
			target = model.GPU.MinCapWattsPerGPU
		}
		if model.GPU.MaxWattsPerGPU > 0 && target > model.GPU.MaxWattsPerGPU {
			target = model.GPU.MaxWattsPerGPU
		}
		d.TargetCapWatts = target
		if d.CapWatts <= 0 {
			d.CapWatts = target
		}
		d.CapWatts = d.CapWatts + (target-d.CapWatts)*gpuStep
		st.GPUCapWattsPerGpu = d.CapWatts
		d.SettledAtTimestamp = now.Format(time.RFC3339Nano)
	}
}

func (s *simulator) initWorkloadEngineFromTrace(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	engine := &workloadEngine{
		startTime:     time.Now().UTC(),
		baseSpeedCore: floatEnv("SIM_BASE_SPEED_PER_CORE", 1.0),
		jobs:          []*simJob{},
		jobByID:       map[string]*simJob{},
	}
	sc := bufio.NewScanner(f)
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return fmt.Errorf("trace parse line %d: %w", lineNum, err)
		}
		tp, _ := rec["type"].(string)
		if tp != "job" {
			continue
		}
		jobID, _ := rec["jobId"].(string)
		if jobID == "" {
			continue
		}
		class := "general"
		if podTpl, ok := rec["podTemplate"].(map[string]any); ok {
			if affinityRaw, ok := podTpl["affinity"].(map[string]any); ok {
				class = classifyClassFromAffinityMap(affinityRaw)
			}
		}
		requestedCPU := 1.0
		requestedGPUs := 0.0
		gpuResourceName := ""
		if podTpl, ok := rec["podTemplate"].(map[string]any); ok {
			if reqRaw, ok := podTpl["requests"].(map[string]any); ok {
				if cpuRaw, ok := reqRaw["cpu"].(string); ok {
					requestedCPU = parseCPURequestOrDefault(cpuRaw, 1.0)
				}
				gpuReqOrder := []struct {
					key         string
					resourceKey string
				}{
					{key: "nvidia.com/gpu", resourceKey: "nvidia.com/gpu"},
					{key: "amd.com/gpu", resourceKey: "amd.com/gpu"},
					{key: "gpu", resourceKey: "nvidia.com/gpu"},
				}
				for _, gpuReq := range gpuReqOrder {
					raw, ok := reqRaw[gpuReq.key]
					if !ok {
						continue
					}
					rawStr, ok := raw.(string)
					if !ok {
						log.Printf("warning: trace line %d job=%s has non-string %s request; skipping GPU request", lineNum, jobID, gpuReq.key)
						break
					}
					gpuQty, ok := parseIntegerResourceRequest(rawStr)
					if !ok {
						log.Printf("warning: trace line %d job=%s has non-integer %s request=%q; skipping GPU request", lineNum, jobID, gpuReq.key, rawStr)
						break
					}
					requestedGPUs = gpuQty
					gpuResourceName = gpuReq.resourceKey
					break
				}
			}
		}
		cpuUnits := 1000.0
		gpuUnits := 0.0
		if workRaw, ok := rec["work"].(map[string]any); ok {
			if v, ok := workRaw["cpuUnits"].(float64); ok && v > 0 {
				cpuUnits = v
			}
			if v, ok := workRaw["gpuUnits"].(float64); ok && v > 0 {
				gpuUnits = v
			}
		}
		sensCPU := 1.0
		sensGPU := 1.0
		cpuWorkClass := "cpu.mixed"
		gpuWorkClass := "gpu.mixed"
		cpuUtilTarget := defaultCPUUtilTarget(cpuWorkClass)
		gpuUtilTarget := 0.0
		memoryIntensity := defaultMemoryIntensity(cpuWorkClass, gpuWorkClass)
		ioIntensity := defaultIOIntensity(cpuWorkClass)
		cpuFeedIntensity := 0.0
		if sensRaw, ok := rec["sensitivity"].(map[string]any); ok {
			if v, ok := sensRaw["cpu"].(float64); ok {
				sensCPU = clamp01(v)
			}
			if v, ok := sensRaw["gpu"].(float64); ok {
				sensGPU = clamp01(v)
			}
		}
		if wcRaw, ok := rec["workloadClass"].(map[string]any); ok {
			if v, ok := wcRaw["cpu"].(string); ok && strings.TrimSpace(v) != "" {
				cpuWorkClass = strings.TrimSpace(v)
			}
			if v, ok := wcRaw["gpu"].(string); ok && strings.TrimSpace(v) != "" {
				gpuWorkClass = strings.TrimSpace(v)
			}
		}
		cpuUtilTarget = defaultCPUUtilTarget(cpuWorkClass)
		gpuUtilTarget = defaultGPUUtilTarget(gpuWorkClass, requestedGPUs > 0)
		memoryIntensity = defaultMemoryIntensity(cpuWorkClass, gpuWorkClass)
		ioIntensity = defaultIOIntensity(cpuWorkClass)
		cpuFeedIntensity = defaultCPUFeedIntensity(requestedGPUs > 0)
		if profRaw, ok := rec["workloadProfile"].(map[string]any); ok {
			if v, ok := profRaw["cpuUtilization"].(float64); ok {
				cpuUtilTarget = clamp01(v)
			}
			if v, ok := profRaw["gpuUtilization"].(float64); ok {
				gpuUtilTarget = clamp01(v)
			}
			if v, ok := profRaw["memoryIntensity"].(float64); ok {
				memoryIntensity = clamp01(v)
			}
			if v, ok := profRaw["ioIntensity"].(float64); ok {
				ioIntensity = clamp01(v)
			}
			if v, ok := profRaw["cpuFeedIntensityGpu"].(float64); ok {
				cpuFeedIntensity = clamp01(v)
			}
		}
		submitOffset := 0.0
		if v, ok := rec["submitTimeOffsetSec"].(float64); ok {
			submitOffset = math.Max(0, v)
		}
		namespace := "default"
		if ns, ok := rec["namespace"].(string); ok && strings.TrimSpace(ns) != "" {
			namespace = ns
		}
		job := &simJob{
			JobID:             jobID,
			Class:             class,
			Namespace:         namespace,
			SubmitOffsetSec:   submitOffset,
			RequestedCPUCores: requestedCPU,
			CPUUnitsTotal:     cpuUnits,
			CPUUnitsRemaining: cpuUnits,
			SensitivityCPU:    sensCPU,
			CPUWorkClass:      cpuWorkClass,
			CPUUtilTarget:     cpuUtilTarget,
			RequestedGPUs:     requestedGPUs,
			GPUResourceName:   gpuResourceName,
			GPUUnitsTotal:     gpuUnits,
			GPUUnitsRemaining: gpuUnits,
			SensitivityGPU:    sensGPU,
			GPUWorkClass:      gpuWorkClass,
			GPUUtilTarget:     gpuUtilTarget,
			MemoryIntensity:   memoryIntensity,
			IOIntensity:       ioIntensity,
			CPUFeedIntensity:  cpuFeedIntensity,
			PodName:           fmt.Sprintf("sim-%s", sanitizeK8sName(jobID)),
		}
		engine.jobs = append(engine.jobs, job)
		engine.jobByID[jobID] = job
	}
	if err := sc.Err(); err != nil {
		return err
	}
	sort.Slice(engine.jobs, func(i, j int) bool { return engine.jobs[i].SubmitOffsetSec < engine.jobs[j].SubmitOffsetSec })
	s.workload = engine
	log.Printf("workload trace loaded jobs=%d path=%s", len(engine.jobs), path)
	return nil
}

func (s *simulator) startWorkloadLoop(interval time.Duration) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("warning: workload loop disabled (no in-cluster config): %v", err)
		return
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Printf("warning: workload loop disabled (kube client): %v", err)
		return
	}
	log.Printf("workload loop enabled interval=%s", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	last := time.Now().UTC()
	for {
		now := time.Now().UTC()
		dt := now.Sub(last).Seconds()
		if dt <= 0 {
			dt = interval.Seconds()
		}
		last = now
		s.accumulateEnergy(dt)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		s.injectTraceJobs(ctx, kube, now)
		s.advanceJobProgress(ctx, kube, dt, now)
		cancel()
		<-ticker.C
	}
}

func (s *simulator) injectTraceJobs(ctx context.Context, kube kubernetes.Interface, now time.Time) {
	if s.workload == nil {
		return
	}
	for _, j := range s.workload.jobs {
		if j.Submitted {
			continue
		}
		if now.Before(s.workload.startTime.Add(time.Duration(j.SubmitOffsetSec * float64(time.Second)))) {
			continue
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      j.PodName,
				Namespace: j.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/part-of": "joulie-sim-workload",
				},
				Annotations: map[string]string{
					"sim.joulie.io/jobId": j.JobID,
				},
			},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Affinity:      affinityForIntentClass(j.Class),
				Containers: []corev1.Container{
					{
						Name:  "work",
						Image: envOrDefault("SIM_WORKLOAD_IMAGE", "registry.k8s.io/pause:3.9"),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(strconv.FormatFloat(j.RequestedCPUCores, 'f', -1, 64)),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
					},
				},
				Tolerations: []corev1.Toleration{
					{
						Key:      "kwok.x-k8s.io/node",
						Operator: corev1.TolerationOpEqual,
						Value:    "fake",
						Effect:   corev1.TaintEffectNoSchedule,
					},
				},
				NodeSelector: map[string]string{
					"type": "kwok",
				},
			},
		}
		if j.RequestedGPUs > 0 {
			gpuResourceName := j.GPUResourceName
			if gpuResourceName == "" {
				gpuResourceName = "nvidia.com/gpu"
			}
			gpuQty := resource.MustParse(strconv.FormatFloat(j.RequestedGPUs, 'f', -1, 64))
			pod.Spec.Containers[0].Resources.Requests[corev1.ResourceName(gpuResourceName)] = gpuQty
			if pod.Spec.Containers[0].Resources.Limits == nil {
				pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{}
			}
			// Extended resources must be set in limits; request-only is rejected.
			pod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(gpuResourceName)] = gpuQty
			switch gpuResourceName {
			case "nvidia.com/gpu":
				pod.Spec.NodeSelector["feature.node.kubernetes.io/pci-10de.present"] = "true"
			case "amd.com/gpu":
				pod.Spec.NodeSelector["feature.node.kubernetes.io/pci-1002.present"] = "true"
			}
		}
		if _, err := kube.CoreV1().Pods(j.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				log.Printf("warning: create workload pod job=%s: %v", j.JobID, err)
				continue
			}
		}
		j.Submitted = true
		j.SubmittedAt = now
		s.jobSubmitted.WithLabelValues(j.Class).Inc()
	}
}

func affinityForIntentClass(intent string) *corev1.Affinity {
	switch intent {
	case "performance":
		return &corev1.Affinity{
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
		}
	case "eco":
		return &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "joulie.io/power-profile",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"eco"},
								},
								{
									Key:      "joulie.io/draining",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"false"},
								},
							},
						},
					},
				},
			},
		}
	default:
		// General/flexible class: no explicit power-profile affinity constraint.
		return nil
	}
}

func classifyClassFromPodSpec(spec *corev1.PodSpec) string {
	// No power-profile scheduling constraint means implicit flexible/general class.
	if spec == nil || spec.Affinity == nil || spec.Affinity.NodeAffinity == nil || spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return "general"
	}
	required := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	perfAllowed := false
	ecoAllowed := false
	for _, term := range required.NodeSelectorTerms {
		termPerf := true
		termEco := true
		for _, expr := range term.MatchExpressions {
			if expr.Key != "joulie.io/power-profile" {
				continue
			}
			switch expr.Operator {
			case corev1.NodeSelectorOpIn:
				termPerf = false
				termEco = false
				for _, v := range expr.Values {
					if v == "performance" {
						termPerf = true
					}
					if v == "eco" {
						termEco = true
					}
				}
			case corev1.NodeSelectorOpNotIn:
				for _, v := range expr.Values {
					if v == "performance" {
						termPerf = false
					}
					if v == "eco" {
						termEco = false
					}
				}
			case corev1.NodeSelectorOpDoesNotExist:
				termPerf = false
				termEco = false
			}
		}
		perfAllowed = perfAllowed || termPerf
		ecoAllowed = ecoAllowed || termEco
	}
	switch {
	case perfAllowed && !ecoAllowed:
		return "performance"
	case ecoAllowed && !perfAllowed:
		return "eco"
	default:
		return "general"
	}
}

func classifyClassFromAffinityMap(affinityRaw map[string]any) string {
	b, err := json.Marshal(map[string]any{"affinity": affinityRaw})
	if err != nil {
		return "general"
	}
	var wrapper struct {
		Affinity *corev1.Affinity `json:"affinity"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		return "general"
	}
	spec := &corev1.PodSpec{Affinity: wrapper.Affinity}
	return classifyClassFromPodSpec(spec)
}

func (s *simulator) advanceJobProgress(ctx context.Context, kube kubernetes.Interface, dt float64, now time.Time) {
	if s.workload == nil {
		return
	}
	pods, err := listRunningPodsDetailed(ctx, kube)
	if err != nil {
		log.Printf("warning: workload progress list pods: %v", err)
		return
	}
	byNode := map[string][]*simJob{}
	for _, p := range pods {
		if p.JobID == "" {
			continue
		}
		j := s.workload.jobByID[p.JobID]
		if j == nil || j.Completed {
			continue
		}
		j.NodeName = p.Node
		byNode[p.Node] = append(byNode[p.Node], j)
	}
	for node, jobs := range byNode {
		s.mu.RLock()
		st := s.state[node]
		model := s.modelForNode(node)
		freqScale := 1.0
		gpuCapFactor := 1.0
		gpuCount := 0.0
		if st != nil {
			freqScale = st.FreqScale
			if model.GPU.Count > 0 && model.GPU.MaxWattsPerGPU > 0 {
				gpuCount = float64(model.GPU.Count)
				cap := st.GPUCapWattsPerGpu
				if cap <= 0 {
					cap = model.GPU.MaxWattsPerGPU
				}
				gpuCapFactor = clamp01(cap / model.GPU.MaxWattsPerGPU)
			}
		}
		s.mu.RUnlock()
		if st == nil {
			st = s.getNode(node)
		}
		gpuJobs := 0
		totalGPUReq := 0.0
		totalCPUUtilDemand := 0.0
		totalGPUUtilDemand := 0.0
		cpuClassWeights := map[string]float64{}
		gpuClassWeights := map[string]float64{}
		for _, j := range jobs {
			if j.CPUUnitsRemaining > 0 {
				cpuClassWeights[j.CPUWorkClass] += math.Max(0.1, j.RequestedCPUCores)
				totalCPUUtilDemand += j.RequestedCPUCores * clamp01(j.CPUUtilTarget)
			}
			if j.GPUUnitsRemaining > 0 {
				gpuJobs++
				totalGPUReq += j.RequestedGPUs
				totalGPUUtilDemand += j.RequestedGPUs * clamp01(j.GPUUtilTarget)
				gpuClassWeights[j.GPUWorkClass] += math.Max(0.1, j.RequestedGPUs)
			}
		}
		if st != nil {
			s.mu.Lock()
			st.CPUUtil = clamp01(totalCPUUtilDemand / 16.0)
			if gpuCount > 0 {
				if totalGPUReq > 0 {
					st.GPUUtil = clamp01(totalGPUUtilDemand / gpuCount)
				} else {
					st.GPUUtil = 0
				}
			} else {
				st.GPUUtil = 0
			}
			st.CPUWorkClass = dominantWorkClass(cpuClassWeights, "cpu.mixed")
			st.GPUWorkClass = dominantWorkClass(gpuClassWeights, "gpu.mixed")
			s.mu.Unlock()
		}
		for _, j := range jobs {
			jobCPUMul := cpuThroughputMultiplier(freqScale, j.CPUWorkClass, model)
			if j.CPUUnitsRemaining > 0 {
				cpuThrottleFactor := cpuThrottleImpactFactor(jobCPUMul, j)
				speed := j.RequestedCPUCores * s.workload.baseSpeedCore * cpuThrottleFactor
				if speed < 0 {
					speed = 0
				}
				j.CPUUnitsRemaining -= speed * dt / float64(maxIntInt(1, len(jobs)))
			}
			if j.GPUUnitsRemaining > 0 {
				share := float64(maxIntInt(1, gpuJobs))
				gpuBase := math.Max(0.1, j.RequestedGPUs) * s.workload.baseSpeedCore
				gpuModel := phys.CappedBoardGPUModel{
					IdleW:         model.GPU.IdleWattsPerGPU,
					MaxW:          model.GPU.MaxWattsPerGPU,
					ComputeGamma:  model.GPU.ComputeGamma,
					MemoryEpsilon: model.GPU.MemoryEpsilon,
					MemoryGamma:   model.GPU.MemoryGamma,
				}
				gpuMul := gpuModel.ThroughputMultiplier(phys.DeviceState{
					Utilization: clamp01(j.GPUUtilTarget),
					CapWatts:    st.GPUCapWattsPerGpu,
					MaxCapWatts: model.GPU.MaxWattsPerGPU,
					Class:       j.GPUWorkClass,
				}, j.GPUWorkClass)
				cpuFeedFactor := cpuFeedThrottleFactor(jobCPUMul, j)
				gpuCapImpact := 1.0 - (1.0-gpuCapFactor)*j.SensitivityGPU
				gpuSpeed := gpuBase * gpuMul * cpuFeedFactor * gpuCapImpact
				if gpuSpeed < 0 {
					gpuSpeed = 0
				}
				j.GPUUnitsRemaining -= gpuSpeed * dt / share
			}
			if j.CPUUnitsRemaining > 0 || j.GPUUnitsRemaining > 0 {
				continue
			}
			j.Completed = true
			j.CompletedAt = now
			_ = kube.CoreV1().Pods(j.Namespace).Delete(ctx, j.PodName, metav1.DeleteOptions{})
			s.jobCompleted.WithLabelValues(j.Class, node).Inc()
			if !j.SubmittedAt.IsZero() {
				s.jobCompletion.Observe(j.CompletedAt.Sub(j.SubmittedAt).Seconds())
			}
			log.Printf("job completed id=%s node=%s class=%s elapsed=%.1fs", j.JobID, node, j.Class, j.CompletedAt.Sub(j.SubmittedAt).Seconds())
		}
	}
}

func parseCPURequestOrDefault(v string, def float64) float64 {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return def
	}
	f := q.AsApproximateFloat64()
	if f <= 0 {
		return def
	}
	return f
}

func parseIntegerResourceRequest(v string) (float64, bool) {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return 0, false
	}
	i, ok := q.AsInt64()
	if !ok || i <= 0 {
		return 0, false
	}
	return float64(i), true
}

func sanitizeK8sName(in string) string {
	in = strings.ToLower(strings.TrimSpace(in))
	if in == "" {
		return "job"
	}
	var b strings.Builder
	for _, r := range in {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "job"
	}
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}

func defaultCPUUtilTarget(class string) float64 {
	switch strings.TrimSpace(class) {
	case "cpu.compute_bound":
		return 0.95
	case "cpu.memory_bound":
		return 0.60
	case "cpu.io_bound":
		return 0.20
	default:
		return 0.65
	}
}

func defaultGPUUtilTarget(class string, hasGPU bool) float64 {
	if !hasGPU {
		return 0
	}
	switch strings.TrimSpace(class) {
	case "gpu.compute_bound":
		return 0.95
	case "gpu.memory_bound", "gpu.bandwidth_bound":
		return 0.70
	default:
		return 0.80
	}
}

func defaultMemoryIntensity(cpuClass, gpuClass string) float64 {
	switch {
	case cpuClass == "cpu.memory_bound" || gpuClass == "gpu.memory_bound" || gpuClass == "gpu.bandwidth_bound":
		return 0.85
	case cpuClass == "cpu.io_bound":
		return 0.20
	default:
		return 0.45
	}
}

func defaultIOIntensity(cpuClass string) float64 {
	if cpuClass == "cpu.io_bound" {
		return 0.85
	}
	return 0.10
}

func defaultCPUFeedIntensity(hasGPU bool) float64 {
	if !hasGPU {
		return 0
	}
	return 0.45
}

func (w *workloadEngine) overrideNodeUtil(st *nodeState, node string) bool {
	if w == nil {
		return false
	}
	totalCPUUtil := 0.0
	totalGPUUtil := 0.0
	gpuReq := 0.0
	active := 0
	for _, j := range w.jobs {
		if j.Completed || !j.Submitted || j.NodeName != node {
			continue
		}
		totalCPUUtil += j.RequestedCPUCores * clamp01(j.CPUUtilTarget)
		if j.RequestedGPUs > 0 {
			totalGPUUtil += j.RequestedGPUs * clamp01(j.GPUUtilTarget)
			gpuReq += j.RequestedGPUs
		}
		active++
	}
	if active == 0 {
		return false
	}
	st.CPUUtil = clamp01(totalCPUUtil / 16.0)
	if gpuReq > 0 {
		st.GPUUtil = clamp01(totalGPUUtil / gpuReq)
	}
	return true
}

func maxIntInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func dominantWorkClass(weights map[string]float64, def string) string {
	total := 0.0
	bestClass := ""
	bestWeight := 0.0
	for cls, w := range weights {
		class := strings.TrimSpace(cls)
		if class == "" || w <= 0 {
			continue
		}
		total += w
		if w > bestWeight {
			bestWeight = w
			bestClass = class
		}
	}
	if bestClass == "" || total <= 0 {
		return def
	}
	// If workload classes are mixed on the same node, keep the blended class.
	if bestWeight/total < 0.6 {
		return def
	}
	return bestClass
}

func cpuThrottleImpactFactor(cpuMul float64, j *simJob) float64 {
	baseImpact := clamp01(0.60*clamp01(j.CPUUtilTarget) + 0.25*(1.0-clamp01(j.MemoryIntensity)) + 0.15*(1.0-clamp01(j.IOIntensity)))
	effective := 1.0 - baseImpact*(1.0-clamp01(cpuMul))
	effective = 1.0 - j.SensitivityCPU*(1.0-effective)
	return clamp01(effective)
}

func cpuFeedThrottleFactor(cpuMul float64, j *simJob) float64 {
	feed := clamp01(j.CPUFeedIntensity)
	if feed <= 0 {
		return 1
	}
	return clamp01(1.0 - (1.0-clamp01(cpuMul))*feed*j.SensitivityCPU)
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func defaultBaseModelFromEnv() simModel {
	return simModel{
		BaseIdleW:                 floatEnv("SIM_BASE_IDLE_W", 80),
		PodW:                      floatEnv("SIM_POD_W", 120),
		DvfsDropW:                 floatEnv("SIM_DVFS_DROP_W_PER_PCT", 1.8),
		RaplHeadW:                 floatEnv("SIM_RAPL_HEADROOM_W", 5),
		DefaultCapW:               floatEnv("SIM_DEFAULT_CAP_W", 5000),
		PMaxW:                     floatEnv("SIM_P_MAX_W", 420),
		AlphaUtil:                 floatEnv("SIM_ALPHA_UTIL", 1.15),
		BetaFreq:                  floatEnv("SIM_BETA_FREQ", 1.35),
		FMinMHz:                   floatEnv("SIM_F_MIN_MHZ", 1200),
		FMaxMHz:                   floatEnv("SIM_F_MAX_MHZ", 3200),
		RaplCapMinW:               floatEnv("SIM_RAPL_CAP_MIN_W", 80),
		RaplCapMaxW:               floatEnv("SIM_RAPL_CAP_MAX_W", 5000),
		DvfsRampMS:                int(floatEnv("SIM_DVFS_RAMP_MS", 500)),
		CPUDriverFamily:           strings.TrimSpace(envOrDefault("SIM_CPU_DRIVER_FAMILY", "amd-pstate")),
		CPULowestNonlinearFreqMHz: floatEnv("SIM_CPU_LOWEST_NONLINEAR_FREQ_MHZ", 1800),
		CPUSockets:                int(floatEnv("SIM_CPU_SOCKETS", 2)),
		CPUSocketCapMinW:          floatEnv("SIM_CPU_SOCKET_CAP_MIN_W", 120),
		CPUSocketCapMaxW:          floatEnv("SIM_CPU_SOCKET_CAP_MAX_W", 400),
		GPU: hw.GPUProfile{
			Vendor:            strings.TrimSpace(envOrDefault("SIM_GPU_VENDOR", "")),
			Product:           strings.TrimSpace(envOrDefault("SIM_GPU_PRODUCT", "")),
			Count:             int(floatEnv("SIM_GPU_COUNT", 0)),
			IdleWattsPerGPU:   floatEnv("SIM_GPU_IDLE_WATTS_PER_GPU", 30),
			MaxWattsPerGPU:    floatEnv("SIM_GPU_MAX_WATTS_PER_GPU", 300),
			MinCapWattsPerGPU: floatEnv("SIM_GPU_MIN_CAP_WATTS_PER_GPU", 80),
			CapApplyTauMS:     int(floatEnv("SIM_GPU_CAP_APPLY_TAU_MS", 150)),
			ComputeGamma:      floatEnv("SIM_GPU_COMPUTE_GAMMA", 1.0),
			MemoryEpsilon:     floatEnv("SIM_GPU_MEMORY_EPSILON", 0.2),
			MemoryGamma:       floatEnv("SIM_GPU_MEMORY_GAMMA", 1.2),
			PowerModel: hw.GPUPowerModel{
				AlphaUtil: floatEnv("SIM_GPU_ALPHA_UTIL", 1.0),
				BetaCap:   floatEnv("SIM_GPU_BETA_CAP", 1.0),
			},
		},
	}
}

func floatEnv(key string, def float64) float64 {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v
		}
	}
	return def
}

func boolEnv(key string, def bool) bool {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		return strings.EqualFold(s, "true") || s == "1" || strings.EqualFold(s, "yes")
	}
	return def
}

func durationEnv(key string, def time.Duration) time.Duration {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
		log.Printf("warning: invalid duration %s=%q, using default %s", key, s, def)
	}
	return def
}
