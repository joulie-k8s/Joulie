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

	"github.com/matbun/joulie/simulator/pkg/hw"
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
	CapWatts          float64        `json:"capWatts"`
	ThrottlePct       int            `json:"throttlePct"`
	TargetThrottlePct int            `json:"targetThrottlePct"`
	FreqScale         float64        `json:"freqScale"`
	CPUUtil           float64        `json:"cpuUtil"`
	CapSaturated      bool           `json:"capSaturated"`
	LastAction        string         `json:"lastAction"`
	LastResult        string         `json:"lastResult"`
	LastUpdate        time.Time      `json:"lastUpdate"`
	PodsRunning       int            `json:"podsRunning"`
	ByIntentClass     map[string]int `json:"byIntentClass,omitempty"`
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
	mu         sync.RWMutex
	state      map[string]*nodeState
	model      simModel
	nodeModels map[string]simModel
	nodeClass  map[string]string
	nodeSeen   map[string]bool
	selector   labels.Selector
	classes    []simNodeClass
	events     []simEvent
	eventMax   int

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

	s := newSimulator(baseModel, selector, classes, int(floatEnv("SIM_EVENT_BUFFER", 300)))

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
		state:      map[string]*nodeState{},
		model:      model,
		nodeModels: map[string]simModel{},
		nodeClass:  map[string]string{},
		nodeSeen:   map[string]bool{},
		selector:   selector,
		classes:    classes,
		eventMax:   eventMax,
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
			CapWatts:          model.DefaultCapW,
			ThrottlePct:       0,
			TargetThrottlePct: 0,
			FreqScale:         1.0,
			CPUUtil:           0,
			LastAction:        "none",
			LastResult:        "none",
			LastUpdate:        time.Now().UTC(),
			ByIntentClass:     map[string]int{},
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
	s.updateNodeDynamics(node, st)
	util := clamp01(st.CPUUtil)
	freq := clamp01(st.FreqScale)
	alpha := model.AlphaUtil
	if alpha <= 0 {
		alpha = 1
	}
	beta := model.BetaFreq
	if beta <= 0 {
		beta = 1
	}
	p := model.BaseIdleW + (model.PMaxW-model.BaseIdleW)*math.Pow(util, alpha)*math.Pow(freq, beta)
	p = math.Max(20, p)

	capW := st.CapWatts
	if capW < model.RaplCapMinW {
		capW = model.RaplCapMinW
	}
	if model.RaplCapMaxW > 0 && capW > model.RaplCapMaxW {
		capW = model.RaplCapMaxW
	}
	minFreq := minFreqScale(model)
	minPower := model.BaseIdleW + (model.PMaxW-model.BaseIdleW)*math.Pow(util, alpha)*math.Pow(minFreq, beta)
	st.CapSaturated = false
	if p > capW {
		st.CapSaturated = minPower > capW
		targetFreq := solveFreqScaleForCap(model, util, capW)
		st.FreqScale = math.Max(minFreq, math.Min(st.FreqScale, targetFreq))
		freq = clamp01(st.FreqScale)
		p = model.BaseIdleW + (model.PMaxW-model.BaseIdleW)*math.Pow(util, alpha)*math.Pow(freq, beta)
	}
	p = math.Min(p, capW+model.RaplHeadW)
	return math.Round(p*100) / 100
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
	s.recordEvent("telemetry", node, map[string]any{
		"packagePowerWatts": power,
		"capWatts":          st.CapWatts,
		"throttlePct":       st.ThrottlePct,
		"freqScale":         st.FreqScale,
		"cpuUtil":           st.CPUUtil,
		"capSaturated":      st.CapSaturated,
		"podsRunning":       st.PodsRunning,
	})
	log.Printf("telemetry node=%s powerW=%.2f capW=%.2f throttlePct=%d pods=%d", node, power, st.CapWatts, st.ThrottlePct, st.PodsRunning)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"node":              node,
		"packagePowerWatts": power,
		"cpu": map[string]any{
			"packagePowerWatts": power,
			"utilization":       st.CPUUtil,
			"freqScale":         st.FreqScale,
			"throttlePct":       st.ThrottlePct,
			"capWatts":          st.CapWatts,
			"raplCapWatts":      st.CapWatts,
			"capSaturated":      st.CapSaturated,
		},
		"gpu": map[string]any{
			"present":     false,
			"powerWatts":  0,
			"utilization": 0,
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
		Action      string  `json:"action"`
		CapWatts    float64 `json:"capWatts"`
		ThrottlePct int     `json:"throttlePct"`
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
	case "gpu.set_power_cap_watts", "gpu.set_clock_policy":
		result = "blocked"
		message = "gpu control not implemented"
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
		"action":      payload.Action,
		"capWatts":    st.CapWatts,
		"throttlePct": st.ThrottlePct,
		"powerWatts":  power,
		"podsRunning": st.PodsRunning,
	})
	log.Printf("control node=%s action=%s capW=%.2f throttlePct=%d powerW=%.2f pods=%d", node, payload.Action, st.CapWatts, st.ThrottlePct, power, st.PodsRunning)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      result == "applied",
		"result":  result,
		"node":    node,
		"message": message,
		"state": map[string]any{
			"capWatts":          st.CapWatts,
			"throttlePct":       st.ThrottlePct,
			"targetThrottlePct": st.TargetThrottlePct,
			"freqScale":         st.FreqScale,
			"cpuUtil":           st.CPUUtil,
			"lastAction":        st.LastAction,
			"lastResult":        st.LastResult,
			"lastUpdate":        st.LastUpdate.Format(time.RFC3339),
			"podsRunning":       st.PodsRunning,
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

func (s *simulator) observeRequest(route, method string, status *int, start time.Time) {
	s.requestsTotal.WithLabelValues(route, method, strconv.Itoa(*status)).Inc()
	s.requestDuration.WithLabelValues(route, method).Observe(time.Since(start).Seconds())
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
				CapWatts:          model.DefaultCapW,
				FreqScale:         1,
				LastAction:        "none",
				LastResult:        "none",
				LastUpdate:        time.Now().UTC(),
				ByIntentClass:     map[string]int{},
				TargetThrottlePct: 0,
			}
			s.state[node] = st
		}
	}

	for node, st := range s.state {
		if !selected[node] {
			st.PodsRunning = 0
			st.ByIntentClass = map[string]int{}
			st.CPUUtil = 0
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
	return model, className
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
			IntentClass: p.Labels["joulie.io/workload-intent-class"],
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

func minFreqScale(model simModel) float64 {
	if model.FMaxMHz <= 0 || model.FMinMHz <= 0 {
		return 0.35
	}
	return clamp01(model.FMinMHz / model.FMaxMHz)
}

func solveFreqScaleForCap(model simModel, util, capW float64) float64 {
	util = clamp01(util)
	if util <= 0 || model.PMaxW <= model.BaseIdleW {
		return 1
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
		class := "flex"
		if podTpl, ok := rec["podTemplate"].(map[string]any); ok {
			if labelsRaw, ok := podTpl["labels"].(map[string]any); ok {
				if v, ok := labelsRaw["joulie.io/workload-intent-class"].(string); ok && strings.TrimSpace(v) != "" {
					class = v
				}
			}
		}
		requestedCPU := 1.0
		if podTpl, ok := rec["podTemplate"].(map[string]any); ok {
			if reqRaw, ok := podTpl["requests"].(map[string]any); ok {
				if cpuRaw, ok := reqRaw["cpu"].(string); ok {
					requestedCPU = parseCPURequestOrDefault(cpuRaw, 1.0)
				}
			}
		}
		cpuUnits := 1000.0
		if workRaw, ok := rec["work"].(map[string]any); ok {
			if v, ok := workRaw["cpuUnits"].(float64); ok && v > 0 {
				cpuUnits = v
			}
		}
		sensCPU := 1.0
		if sensRaw, ok := rec["sensitivity"].(map[string]any); ok {
			if v, ok := sensRaw["cpu"].(float64); ok {
				sensCPU = clamp01(v)
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
					"joulie.io/workload-intent-class": j.Class,
					"app.kubernetes.io/part-of":       "joulie-sim-workload",
				},
				Annotations: map[string]string{
					"sim.joulie.io/jobId": j.JobID,
				},
			},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
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
		freqScale := 1.0
		if st != nil {
			freqScale = st.FreqScale
		}
		s.mu.RUnlock()
		for _, j := range jobs {
			if j.CPUUnitsRemaining <= 0 {
				continue
			}
			speed := j.RequestedCPUCores * s.workload.baseSpeedCore * (1.0 - (1.0-freqScale)*j.SensitivityCPU)
			if speed < 0 {
				speed = 0
			}
			j.CPUUnitsRemaining -= speed * dt / float64(maxIntInt(1, len(jobs)))
			if j.CPUUnitsRemaining > 0 {
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

func (w *workloadEngine) overrideNodeUtil(st *nodeState, node string) bool {
	if w == nil {
		return false
	}
	totalCores := 0.0
	active := 0
	for _, j := range w.jobs {
		if j.Completed || !j.Submitted || j.NodeName != node {
			continue
		}
		totalCores += j.RequestedCPUCores
		active++
	}
	if active == 0 {
		return false
	}
	st.CPUUtil = clamp01(totalCores / 16.0)
	return true
}

func maxIntInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func defaultBaseModelFromEnv() simModel {
	return simModel{
		BaseIdleW:   floatEnv("SIM_BASE_IDLE_W", 80),
		PodW:        floatEnv("SIM_POD_W", 120),
		DvfsDropW:   floatEnv("SIM_DVFS_DROP_W_PER_PCT", 1.8),
		RaplHeadW:   floatEnv("SIM_RAPL_HEADROOM_W", 5),
		DefaultCapW: floatEnv("SIM_DEFAULT_CAP_W", 5000),
		PMaxW:       floatEnv("SIM_P_MAX_W", 420),
		AlphaUtil:   floatEnv("SIM_ALPHA_UTIL", 1.15),
		BetaFreq:    floatEnv("SIM_BETA_FREQ", 1.35),
		FMinMHz:     floatEnv("SIM_F_MIN_MHZ", 1200),
		FMaxMHz:     floatEnv("SIM_F_MAX_MHZ", 3200),
		RaplCapMinW: floatEnv("SIM_RAPL_CAP_MIN_W", 80),
		RaplCapMaxW: floatEnv("SIM_RAPL_CAP_MAX_W", 5000),
		DvfsRampMS:  int(floatEnv("SIM_DVFS_RAMP_MS", 500)),
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
