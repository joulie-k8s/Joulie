package main

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

var podListFunc = listRunningPodsByNode
var nodeListFunc = listNodeLabels

type nodeState struct {
	CapWatts    float64   `json:"capWatts"`
	ThrottlePct int       `json:"throttlePct"`
	LastAction  string    `json:"lastAction"`
	LastUpdate  time.Time `json:"lastUpdate"`
	PodsRunning int       `json:"podsRunning"`
}

type simModel struct {
	BaseIdleW   float64
	PodW        float64
	DvfsDropW   float64
	RaplHeadW   float64
	DefaultCapW float64
}

type simNodeClassFile struct {
	Classes []simNodeClass `yaml:"classes"`
}

type simNodeClass struct {
	Name        string            `yaml:"name"`
	MatchLabels map[string]string `yaml:"matchLabels"`
	Model       simModelOverrides `yaml:"model"`
}

type simModelOverrides struct {
	BaseIdleW   *float64 `yaml:"baseIdleW"`
	PodW        *float64 `yaml:"podW"`
	DvfsDropW   *float64 `yaml:"dvfsDropWPerPct"`
	RaplHeadW   *float64 `yaml:"raplHeadW"`
	DefaultCapW *float64 `yaml:"defaultCapW"`
}

type simEvent struct {
	Timestamp time.Time      `json:"timestamp"`
	Kind      string         `json:"kind"`
	Node      string         `json:"node"`
	Payload   map[string]any `json:"payload,omitempty"`
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
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[joulie-simulator] ")

	baseModel := simModel{
		BaseIdleW:   floatEnv("SIM_BASE_IDLE_W", 80),
		PodW:        floatEnv("SIM_POD_W", 120),
		DvfsDropW:   floatEnv("SIM_DVFS_DROP_W_PER_PCT", 1.8),
		RaplHeadW:   floatEnv("SIM_RAPL_HEADROOM_W", 5),
		DefaultCapW: floatEnv("SIM_DEFAULT_CAP_W", 5000),
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

	if boolEnv("SIM_K8S_POD_WATCH", true) {
		go s.startPodPolling(durationEnv("SIM_POLL_INTERVAL", 15*time.Second))
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
			CapWatts:    model.DefaultCapW,
			ThrottlePct: 0,
			LastAction:  "none",
			LastUpdate:  time.Now().UTC(),
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
	p := model.BaseIdleW + float64(st.PodsRunning)*model.PodW - float64(st.ThrottlePct)*model.DvfsDropW
	p = math.Max(20, p)
	p = math.Min(p, st.CapWatts+model.RaplHeadW)
	return math.Round(p*100) / 100
}

func (s *simulator) updateNodeMetrics(node string, st *nodeState) {
	power := s.nodePower(node, st)
	s.nodeCapW.WithLabelValues(node).Set(st.CapWatts)
	s.nodeThrottlePct.WithLabelValues(node).Set(float64(st.ThrottlePct))
	s.nodePowerW.WithLabelValues(node).Set(power)
	s.nodePods.WithLabelValues(node).Set(float64(st.PodsRunning))
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
		"podsRunning":       st.PodsRunning,
	})
	log.Printf("telemetry node=%s powerW=%.2f capW=%.2f throttlePct=%d pods=%d", node, power, st.CapWatts, st.ThrottlePct, st.PodsRunning)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"node": node,
		"cpu": map[string]any{
			"packagePowerWatts": power,
			"throttlePct":       st.ThrottlePct,
			"capWatts":          st.CapWatts,
		},
		"workload": map[string]any{
			"runningPods": st.PodsRunning,
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
	s.mu.Lock()
	switch payload.Action {
	case "rapl.set_power_cap_watts":
		if payload.CapWatts > 0 {
			st.CapWatts = payload.CapWatts
		}
	case "dvfs.set_throttle_pct":
		if payload.ThrottlePct < 0 {
			payload.ThrottlePct = 0
		}
		if payload.ThrottlePct > 100 {
			payload.ThrottlePct = 100
		}
		st.ThrottlePct = payload.ThrottlePct
	}
	st.LastAction = payload.Action
	st.LastUpdate = time.Now().UTC()
	s.mu.Unlock()

	s.controlsTotal.WithLabelValues(node, payload.Action).Inc()
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
		"ok":   true,
		"node": node,
		"state": map[string]any{
			"capWatts":    st.CapWatts,
			"throttlePct": st.ThrottlePct,
			"lastAction":  st.LastAction,
			"lastUpdate":  st.LastUpdate.Format(time.RFC3339),
			"podsRunning": st.PodsRunning,
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
		nodes, nodeErr := nodeListFunc(ctx, kube)
		cancel()
		if err != nil {
			log.Printf("warning: pod polling list failed: %v", err)
			<-ticker.C
			continue
		}
		if nodeErr != nil {
			log.Printf("warning: node polling list failed: %v", nodeErr)
			<-ticker.C
			continue
		}

		s.refreshNodeStateFromKubeData(counts, nodes)

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

func (s *simulator) refreshNodeStateFromKubeData(counts map[string]int, nodeLabels map[string]map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	selected := map[string]bool{}
	for node := range nodeLabels {
		s.nodeSeen[node] = true
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
			st = &nodeState{CapWatts: model.DefaultCapW, LastAction: "none", LastUpdate: time.Now().UTC()}
			s.state[node] = st
		}
	}

	for node, st := range s.state {
		if !selected[node] {
			st.PodsRunning = 0
			continue
		}
		st.PodsRunning = counts[node]
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
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg simNodeClassFile
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	out := make([]simNodeClass, 0, len(cfg.Classes))
	for _, c := range cfg.Classes {
		if strings.TrimSpace(c.Name) == "" {
			continue
		}
		if c.MatchLabels == nil {
			c.MatchLabels = map[string]string{}
		}
		out = append(out, c)
	}
	return out, nil
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
	out := base
	if o.BaseIdleW != nil {
		out.BaseIdleW = *o.BaseIdleW
	}
	if o.PodW != nil {
		out.PodW = *o.PodW
	}
	if o.DvfsDropW != nil {
		out.DvfsDropW = *o.DvfsDropW
	}
	if o.RaplHeadW != nil {
		out.RaplHeadW = *o.RaplHeadW
	}
	if o.DefaultCapW != nil {
		out.DefaultCapW = *o.DefaultCapW
	}
	return out
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

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
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
