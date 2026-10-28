package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/matbun/joulie/simulator/pkg/compat"
	"github.com/matbun/joulie/simulator/pkg/facility"
	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/matbun/joulie/simulator/pkg/phys"
	"github.com/matbun/joulie/simulator/pkg/workload"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// The Kubernetes reads of the loops; tests replace them. podPollFunc and
// nodeListFunc are the pod poll's one list of pods and one list of Nodes;
// podDetailFunc is the legacy workload loop's list of pods.
var (
	podPollFunc   = listPodsForPoll
	podDetailFunc = listRunningPodsDetailed
	nodeListFunc  = listNodeMeta
)

type nodeState struct {
	CapWatts          float64   `json:"capWatts"`
	TargetCapWatts    float64   `json:"targetCapWatts"`
	ThrottlePct       int       `json:"throttlePct"`
	TargetThrottlePct int       `json:"targetThrottlePct"`
	FreqScale         float64   `json:"freqScale"`
	CPUUtil           float64   `json:"cpuUtil"`
	CPUWorkClass      string    `json:"cpuWorkClass"`
	CapSaturated      bool      `json:"capSaturated"`
	LastAction        string    `json:"lastAction"`
	LastResult        string    `json:"lastResult"`
	LastUpdate        time.Time `json:"lastUpdate"`
	PodsRunning       int       `json:"podsRunning"`
	// OtherPods counts the running pods on the node outside the workload
	// engine's jobs, the load of a node without jobs (setNoJobLoad).
	OtherPods int `json:"otherPods"`
	// HasJobs tells whether the workload tick last saw jobs on the node:
	// the tick then sets its load, and the pod poll leaves it alone.
	HasJobs                 bool             `json:"hasJobs"`
	ByIntentClass           map[string]int   `json:"byIntentClass,omitempty"`
	GPUCapWattsPerGpu       float64          `json:"gpuCapWattsPerGpu"`
	GPUTargetCapWattsPerGpu float64          `json:"gpuTargetCapWattsPerGpu"`
	GPUUtil                 float64          `json:"gpuUtil"`
	GPUWorkClass            string           `json:"gpuWorkClass"`
	GPUPowerWatts           float64          `json:"gpuPowerWatts"`
	GPUPerfMultiplier       float64          `json:"gpuPerfMultiplier"`
	MemoryIntensity         float64          `json:"memoryIntensity"`
	IOIntensity             float64          `json:"ioIntensity"`
	CPUFeedIntensity        float64          `json:"cpuFeedIntensity"`
	CPUInstantPowerWatts    float64          `json:"cpuInstantPowerWatts"`
	CPUAvgPowerWatts        float64          `json:"cpuAvgPowerWatts"`
	TotalAvgPowerWatts      float64          `json:"totalAvgPowerWatts"`
	CPUTemperatureC         float64          `json:"cpuTemperatureC"`
	CPUThermalThrottle      float64          `json:"cpuThermalThrottle"`
	CPUCapacityCores        float64          `json:"cpuCapacityCores"`
	GPUCapacityDevices      float64          `json:"gpuCapacityDevices"`
	CPUSockets              []cpuSocketState `json:"cpuSockets,omitempty"`
	GPUDevices              []gpuDeviceState `json:"gpuDevices,omitempty"`
	// SimTime is the simulated time the node's physics has reached: only
	// stepNode moves it, by the time it steps.
	SimTime time.Time `json:"simTime"`
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
	AvgPowerWatts      float64 `json:"avgPowerWatts"`
	PerfMultiplier     float64 `json:"perfMultiplier"`
	TemperatureC       float64 `json:"temperatureC"`
	ThermalThrottle    float64 `json:"thermalThrottle"`
	SettledAtTimestamp string  `json:"settledAt,omitempty"`
}

// clone is a copy of st that shares nothing with it, for a reader that
// uses it after releasing s.mu while the physics tick writes st.
func (st *nodeState) clone() nodeState {
	cp := *st
	cp.CPUSockets = append([]cpuSocketState(nil), st.CPUSockets...)
	cp.GPUDevices = append([]gpuDeviceState(nil), st.GPUDevices...)
	if st.ByIntentClass != nil {
		cp.ByIntentClass = make(map[string]int, len(st.ByIntentClass))
		for k, v := range st.ByIntentClass {
			cp.ByIntentClass[k] = v
		}
	}
	return cp
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
	Running     bool
}

type simJob struct {
	JobID              string
	WorkloadID         string
	WorkloadType       string
	PodRole            string
	Gang               bool
	Class              string
	Namespace          string
	SubmitOffsetSec    float64
	RequestedCPUCores  float64
	RequestedMemoryMiB float64 // 0 means use default (64Mi)
	CPUUnitsTotal      float64
	CPUUnitsRemaining  float64
	SensitivityCPU     float64
	CPUWorkClass       string
	CPUUtilTarget      float64
	RequestedGPUs      float64
	GPUResourceName    string
	GPUUnitsTotal      float64
	GPUUnitsRemaining  float64
	SensitivityGPU     float64
	GPUWorkClass       string
	GPUUtilTarget      float64
	MemoryIntensity    float64
	IOIntensity        float64
	CPUFeedIntensity   float64
	Submitted          bool
	Completed          bool
	SubmitAt           time.Time
	SubmittedAt        time.Time
	CompletedAt        time.Time
	LastProgressAt     time.Time
	DeleteRequestedAt  time.Time
	PodName            string
	NodeName           string

	// Read from the trace. SchemaVersion is the record's schemaVersion.
	// Affinity, NodeSelector and Tolerations are the podTemplate's
	// placement; a legacy record keeps only its affinity, which its pod
	// carries with the intent ANDed in (legacyAffinity). SensitivitySet is whether the record gave sensitivity.cpu
	// or sensitivity.gpu; without either both are 1.
	SchemaVersion  string
	Affinity       *corev1.Affinity
	NodeSelector   map[string]string
	Tolerations    []corev1.Toleration
	SensitivitySet bool

	// PeerTransferIntensity is workloadProfile.peerTransferIntensity, the
	// share of the job's GPU time spent moving data between its GPUs on
	// one node: 0, no transfers, unless the record gives it. Only the v1
	// engine reads it.
	PeerTransferIntensity float64
}

type workloadEngine struct {
	startTime     time.Time
	baseSpeedCore float64
	// jobsMu guards the jobs' fields: injectTraceJobs and
	// advanceJobProgress hold it, as standalone does for each step, and
	// the debug snapshot loop and /debug/jobs take it to read them
	// (debugJobsPayload). The job maps are written only at load.
	jobsMu         sync.Mutex
	jobs           []*simJob
	jobByID        map[string]*simJob
	jobsByWorkload map[string][]*simJob
	// opts are the engine's settings; the zero value is the legacy engine.
	opts workloadOptions
}

type simulator struct {
	mu            sync.RWMutex
	startTime     time.Time
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
	persistDir    string
	persistMu     sync.Mutex
	// journal is events.ndjson under persistDir, open from
	// configurePersistence until closePersistence; nil without one.
	journal  *os.File
	tsFile   *os.File
	tsWriter *bufio.Writer
	tsPath   string
	// energyJByHwType is the modelled nodes' energy by hw.type (cpu, gpu),
	// as stepNode returns it; an emulated node's energy is its host's.
	// addEnergyLocked creates it.
	energyJByHwType map[string]float64

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
	// run is the v1 workload run that is active, or the last one; nil
	// before the first. The run API stores it, the emulator's callbacks
	// read it.
	run atomic.Pointer[workloadRun]
	// emu is emulate mode, nil without SIM_EMULATE. It is set before the
	// server starts and never changes.
	emu *emulateRuntime

	// Facility ambient temperature parameters.
	facilityBaseAmbientTempC float64
	facilityTempAmplitudeC   float64
	facilityTempPeriodH      float64
	// facilityMu guards facilityPeakITPowerW, which the workload loop and
	// the emulator's facility callback both update.
	facilityMu           sync.Mutex
	facilityPeakITPowerW float64

	// Facility Prometheus gauges.
	facilityAmbientTemp   prometheus.Gauge
	facilityPUE           prometheus.Gauge
	facilityITPowerW      prometheus.Gauge
	facilityCoolingPowerW prometheus.Gauge
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[joulie-simulator] ")
	if err := checkModes(os.Getenv); err != nil {
		log.Fatal(err)
	}
	workloadOpts, warnings, err := loadWorkloadOptions(os.Getenv, boolEnv(envEmulate, false))
	if err != nil {
		log.Fatal(err)
	}
	for _, w := range warnings {
		log.Printf("warning: %s", w)
	}

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
	// Empty means the catalog embedded in pkg/hwinv. The experiments mount a
	// generated catalog over it with SIM_HARDWARE_CATALOG_PATH.
	catalogPath := strings.TrimSpace(envOrDefault("SIM_HARDWARE_CATALOG_PATH", ""))
	catalog, err := hw.LoadCatalog(catalogPath)
	if err != nil {
		log.Printf("warning: failed to load hardware catalog path=%s err=%v", catalogPath, err)
	}

	s := newSimulator(baseModel, selector, classes, int(floatEnv("SIM_EVENT_BUFFER", 300)))
	s.catalog = catalog
	s.configurePersistence(strings.TrimSpace(envOrDefault("SIM_DEBUG_PERSIST_DIR", "/tmp/joulie-simulator-debug")))

	// A legacy trace loads into the legacy engine here; a v1 trace starts
	// an implicit run once emulate mode is up.
	tracePath := strings.TrimSpace(os.Getenv(workload.EnvTracePath))
	if tracePath != "" && workloadOpts.model() == workload.ModelLegacy {
		if err := s.initWorkloadEngineWithOptions(tracePath, workloadOpts); err != nil {
			log.Printf("warning: workload trace disabled: %v", err)
		}
	}

	// Facility ambient temperature configuration.
	s.facilityBaseAmbientTempC = floatEnv("SIM_FACILITY_AMBIENT_TEMP_C", 20.0)
	s.facilityTempAmplitudeC = floatEnv("SIM_FACILITY_TEMP_AMPLITUDE_C", 5.0)
	s.facilityTempPeriodH = floatEnv("SIM_FACILITY_TEMP_PERIOD_H", 24.0)
	log.Printf("facility model base_temp=%.1fC amplitude=%.1fC period=%.1fh",
		s.facilityBaseAmbientTempC, s.facilityTempAmplitudeC, s.facilityTempPeriodH)

	// Standalone mode: bypass K8s entirely, run simulation at CPU speed.
	if boolEnv("SIM_STANDALONE", false) {
		runStandalone(s)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if boolEnv(envEmulate, false) {
		if err := s.startEmulate(ctx); err != nil {
			log.Fatalf("emulate mode: %v", err)
		}
	} else {
		// Without emulate mode the physics tick steps the modelled nodes;
		// in emulate mode the emulator's tick does (stepLegacyNodes).
		go s.runPhysicsLoop(ctx, durationEnv(envPhysicsTick, defaultPhysicsTick))
	}

	if boolEnv("SIM_K8S_POD_WATCH", true) {
		go s.startPodPolling(durationEnv("SIM_POLL_INTERVAL", defaultPollInterval))
	}
	if s.workload != nil {
		go s.startWorkloadLoop(durationEnv(workload.EnvTick, defaultWorkloadTick))
	}
	go s.startDebugSnapshotLoop(durationEnv("SIM_DEBUG_SNAPSHOT_INTERVAL", 5*time.Second))

	mux := s.newMux()
	runs := s.registerRunHandlers(mux, workloadOpts)
	if tracePath != "" && workloadOpts.model() == workload.ModelV1 {
		runs.startImplicitRun(tracePath)
	}

	addr := envOrDefault("SIM_ADDR", defaultSimAddr())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
	}
	s.stopEmulate()
	// A run still active when the simulator stops ends aborted, so its
	// files close with their last records.
	if r := s.run.Load(); r != nil && r.active() {
		r.finish(false)
	}
	s.closePersistence()
}

// newMux routes the simulator's HTTP API. Emulate mode adds the
// compatibility exporters, /debug/emulate and /readyz, and has no fake
// Prometheus: its facility goes out as OpenTelemetry metrics.
func (s *simulator) newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/telemetry/", s.handleTelemetry)
	mux.HandleFunc("/otlp/telemetry/", s.handleOTLPTelemetry)
	mux.HandleFunc("/control/", s.handleControl)
	mux.HandleFunc("/state/", s.handleState)
	mux.HandleFunc("/debug/nodes", s.handleDebugNodes)
	mux.HandleFunc("/debug/jobs", s.handleDebugJobs)
	mux.HandleFunc("/debug/events", s.handleDebugEvents)
	mux.HandleFunc("/debug/energy", s.handleDebugEnergy)
	mux.HandleFunc("/debug/timeseries", s.handleDebugTimeseries)
	if s.emu != nil {
		mux.Handle(compat.PathPrefix, s.emu.compat.Handler())
		mux.HandleFunc("/debug/emulate", s.handleDebugEmulate)
		mux.HandleFunc("/readyz", s.handleReadyz)
	} else {
		mux.HandleFunc("/api/v1/query", s.handleFakePrometheusQuery)
	}
	return mux
}

func newSimulator(model simModel, selector labels.Selector, classes []simNodeClass, eventMax int) *simulator {
	return newSimulatorWithRegisterer(model, selector, classes, eventMax, prometheus.DefaultRegisterer)
}

func newSimulatorWithRegisterer(model simModel, selector labels.Selector, classes []simNodeClass, eventMax int, reg prometheus.Registerer) *simulator {
	if eventMax < 0 {
		eventMax = 0
	}
	s := &simulator{
		startTime:     time.Now().UTC(),
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
		facilityAmbientTemp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "joulie_sim_facility_ambient_temp_celsius",
			Help: "Simulated facility ambient temperature in Celsius",
		}),
		facilityPUE: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "joulie_sim_facility_pue",
			Help: "Simulated facility Power Usage Effectiveness",
		}),
		facilityITPowerW: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "joulie_sim_facility_it_power_watts",
			Help: "Simulated total IT power draw in watts",
		}),
		facilityCoolingPowerW: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "joulie_sim_facility_cooling_power_watts",
			Help: "Simulated cooling infrastructure power in watts",
		}),
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
		s.facilityAmbientTemp,
		s.facilityPUE,
		s.facilityITPowerW,
		s.facilityCoolingPowerW,
	)
	return s
}

func (s *simulator) configurePersistence(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("warning: debug persistence disabled mkdir path=%s err=%v", dir, err)
		return
	}
	s.persistDir = dir
	s.snapshotDebugState()
	journalPath := filepath.Join(dir, eventJournalFile)
	if f, err := os.OpenFile(journalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
		log.Printf("warning: events journal disabled path=%s err=%v", journalPath, err)
	} else {
		s.journal = f
	}
	// Open time-series CSV for append.
	tsPath := filepath.Join(dir, "timeseries.csv")
	f, err := os.OpenFile(tsPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		log.Printf("warning: timeseries CSV disabled path=%s err=%v", tsPath, err)
	} else {
		s.tsFile = f
		s.tsWriter = bufio.NewWriter(f)
		s.tsPath = tsPath
		_, _ = s.tsWriter.WriteString("timestamp_utc,elapsed_sec,it_power_w,cpu_power_w,gpu_power_w,pue,cooling_power_w,facility_power_w,ambient_temp_c,cluster_cpu_util,cluster_gpu_util,nodes_active,pods_running,energy_cumulative_j\n")
		log.Printf("timeseries CSV enabled path=%s", tsPath)
	}
	log.Printf("debug persistence enabled dir=%s", dir)
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
			TargetCapWatts:          model.DefaultCapW,
			ThrottlePct:             0,
			TargetThrottlePct:       0,
			FreqScale:               1.0,
			CPUUtil:                 0,
			CPUWorkClass:            "cpu.mixed",
			LastAction:              "none",
			LastResult:              "none",
			LastUpdate:              time.Now().UTC(),
			SimTime:                 time.Now().UTC(),
			ByIntentClass:           map[string]int{},
			GPUCapWattsPerGpu:       model.GPU.MaxWattsPerGPU,
			GPUTargetCapWattsPerGpu: model.GPU.MaxWattsPerGPU,
			GPUWorkClass:            "gpu.mixed",
			GPUPerfMultiplier:       1.0,
			CPUAvgPowerWatts:        model.BaseIdleW,
			TotalAvgPowerWatts:      model.BaseIdleW,
			CPUTemperatureC:         model.CPUAmbientTempC,
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
					AvgPowerWatts:  model.GPU.IdleWattsPerGPU,
					TemperatureC:   model.GPU.AmbientTempC,
				})
			}
		}
		// A step of 0 s brings the readings up to date with the
		// targets; it advances nothing.
		stepNode(st, model, 0, physMaxSubstep)
		s.state[node] = st
	}
	return st
}

func (s *simulator) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleTelemetry serves /telemetry/{node}: by default the OTLP JSON of
// /otlp/telemetry/{node}, and with ?format=legacy the legacy JSON,
// deprecated until otlpjson.LegacyRemovedIn (telemetry_route.go).
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
	format, err := parseTelemetryFormat(r)
	if err != nil {
		status = http.StatusBadRequest
		http.Error(w, err.Error(), status)
		return
	}
	if format == telemetryFormatOTLP {
		s.serveOTLPTelemetry(w, node, &status)
		return
	}
	warnLegacyTelemetry()
	if s.emulated(node) {
		s.handleEmulatedTelemetry(w, node, &status)
		return
	}
	snap := s.telemetrySample(node)
	modelForTelemetry := snap.model
	resp := map[string]any{
		"node":                     node,
		"packagePowerWatts":        snap.power,
		"instantPackagePowerWatts": math.Round((snap.cpuInstantPower+snap.gpuPowerW)*100) / 100,
		"cpu": map[string]any{
			"packagePowerWatts":  snap.cpuAvgPower,
			"instantPowerWatts":  snap.cpuInstantPower,
			"utilization":        snap.cpuUtil,
			"memoryIntensity":    snap.memIntensity,
			"ioIntensity":        snap.ioIntensity,
			"workClass":          snap.cpuWorkClass,
			"freqScale":          snap.freqScale,
			"throttlePct":        snap.throttlePct,
			"thermalThrottlePct": snap.cpuThermalThrottle * 100.0,
			"temperatureC":       snap.cpuTempC,
			"capWatts":           snap.capW,
			"targetCapWatts":     snap.targetCapW,
			"raplCapWatts":       snap.capW,
			"capSaturated":       snap.capSaturated,
		},
		"gpu": map[string]any{
			"present":               modelForTelemetry.GPU.Count > 0,
			"vendor":                modelForTelemetry.GPU.Vendor,
			"product":               modelForTelemetry.GPU.Product,
			"count":                 modelForTelemetry.GPU.Count,
			"powerWattsTotal":       snap.gpuPowerW,
			"avgPowerWattsTotal":    snap.gpuAvgPower,
			"capWattsPerGpuApplied": snap.gpuCapPerGpu,
			"capWattsPerGpuTarget":  snap.gpuTargetCapPerGpu,
			"utilization":           snap.gpuUtil,
			"memoryIntensity":       snap.memIntensity,
			"cpuFeedIntensity":      snap.cpuFeedIntensity,
			"workClass":             snap.gpuWorkClass,
			"perfMultiplier":        snap.gpuPerfMul,
		},
		"hardwareModel": map[string]any{
			"cpuModel":       modelForTelemetry.CPUModel,
			"cpuProvenance":  modelForTelemetry.CPUProvenance,
			"cpuCurveSource": modelForTelemetry.CPUCurveSource,
			"cpuProxyFrom":   modelForTelemetry.CPUProxyFrom,
			"gpuProvenance":  modelForTelemetry.GPU.Provenance,
		},
		"pods": map[string]any{
			"running":       snap.podsRunning,
			"byIntentClass": snap.byIntentClass,
		},
		"ts": time.Now().UTC().Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
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
	if s.emulated(node) {
		// An emulated node is capped as real hardware is: through the
		// files of its tree, which the next tick reads back.
		status = http.StatusConflict
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "result": "conflict", "message": node + " is emulated; write its files"})
		return
	}

	var payload struct {
		Action         string  `json:"action"`
		CapWatts       float64 `json:"capWatts"`
		ThrottlePct    int     `json:"throttlePct"`
		CapWattsPerGpu float64 `json:"capWattsPerGpu"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&payload); err != nil {
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
			// A cap below the lowest one the CPUs accept is refused, never
			// applied: a saving measured under it could not happen.
			if floorW := cpuCapFloorW(model); payload.CapWatts < floorW {
				result = "error"
				message = fmt.Sprintf("capWatts %g is below the lowest cap this node's CPUs accept, %g W", payload.CapWatts, floorW)
				break
			}
			capW := payload.CapWatts
			if model.RaplCapMaxW > 0 && capW > model.RaplCapMaxW {
				capW = model.RaplCapMaxW
			}
			st.TargetCapWatts = capW
			if st.CapWatts <= 0 {
				st.CapWatts = capW
			}
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
		if payload.CapWattsPerGpu < model.GPU.MinCapWattsPerGPU {
			result = "error"
			message = fmt.Sprintf("capWattsPerGpu %g is below the lowest cap this node's GPUs accept, %g W", payload.CapWattsPerGpu, model.GPU.MinCapWattsPerGPU)
			break
		}
		cap := payload.CapWattsPerGpu
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

	// The control sets targets only; the next physics step moves toward
	// them. Snapshot the state, then release the lock quickly.
	power := observe(st).powerW
	ctrlSnap := struct {
		capW, targetCapW, freqScale, cpuUtil float64
		throttlePct, targetThrottlePct       int
		podsRunning                          int
		gpuCapPerGpu, gpuTargetCapPerGpu     float64
		lastAction, lastResult               string
		lastUpdate                           string
	}{
		capW: st.CapWatts, targetCapW: st.TargetCapWatts,
		freqScale: st.FreqScale, cpuUtil: st.CPUUtil,
		throttlePct: st.ThrottlePct, targetThrottlePct: st.TargetThrottlePct,
		podsRunning:  st.PodsRunning,
		gpuCapPerGpu: st.GPUCapWattsPerGpu, gpuTargetCapPerGpu: st.GPUTargetCapWattsPerGpu,
		lastAction: st.LastAction, lastResult: st.LastResult,
		lastUpdate: st.LastUpdate.Format(time.RFC3339),
	}
	s.mu.Unlock()

	// Prometheus updates and response building outside lock.
	s.nodeCapW.WithLabelValues(node).Set(ctrlSnap.capW)
	s.nodeThrottlePct.WithLabelValues(node).Set(float64(ctrlSnap.throttlePct))
	s.nodePowerW.WithLabelValues(node).Set(power)
	s.nodePods.WithLabelValues(node).Set(float64(ctrlSnap.podsRunning))
	s.nodeUtilCPU.WithLabelValues(node).Set(ctrlSnap.cpuUtil)
	s.nodeFreqScale.WithLabelValues(node).Set(ctrlSnap.freqScale)
	s.nodeRaplCapW.WithLabelValues(node).Set(ctrlSnap.capW)
	eventPayload := map[string]any{
		"action":            payload.Action,
		"capWatts":          ctrlSnap.capW,
		"targetCapWatts":    ctrlSnap.targetCapW,
		"throttlePct":       ctrlSnap.throttlePct,
		"powerWatts":        power,
		"podsRunning":       ctrlSnap.podsRunning,
		"gpuCapWattsPerGpu": ctrlSnap.gpuCapPerGpu,
	}
	resp := map[string]any{
		"ok":      result == "applied",
		"result":  result,
		"node":    node,
		"message": message,
		"state": map[string]any{
			"capWatts":                 ctrlSnap.capW,
			"throttlePct":              ctrlSnap.throttlePct,
			"targetThrottlePct":        ctrlSnap.targetThrottlePct,
			"freqScale":                ctrlSnap.freqScale,
			"cpuUtil":                  ctrlSnap.cpuUtil,
			"lastAction":               ctrlSnap.lastAction,
			"lastResult":               ctrlSnap.lastResult,
			"lastUpdate":               ctrlSnap.lastUpdate,
			"podsRunning":              ctrlSnap.podsRunning,
			"gpuCapWattsPerGpuApplied": ctrlSnap.gpuCapPerGpu,
			"gpuCapWattsPerGpuTarget":  ctrlSnap.gpuTargetCapPerGpu,
		},
		"simulatedPackagePowerWatts": power,
	}
	logMsg := fmt.Sprintf("control node=%s action=%s capW=%.2f throttlePct=%d powerW=%.2f pods=%d", node, payload.Action, ctrlSnap.capW, ctrlSnap.throttlePct, power, ctrlSnap.podsRunning)

	s.controlsTotal.WithLabelValues(node, payload.Action).Inc()
	s.controlResult.WithLabelValues(node, payload.Action, result).Inc()
	s.recordEvent("control", node, eventPayload)
	log.Print(logMsg)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
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
	if s.emulated(node) {
		s.handleEmulatedState(w, node)
		return
	}
	st := s.getNode(node)
	s.mu.Lock()
	power := observe(st).powerW
	s.nodeCapW.WithLabelValues(node).Set(st.CapWatts)
	s.nodeThrottlePct.WithLabelValues(node).Set(float64(st.ThrottlePct))
	s.nodePowerW.WithLabelValues(node).Set(power)
	s.nodePods.WithLabelValues(node).Set(float64(st.PodsRunning))
	s.nodeUtilCPU.WithLabelValues(node).Set(st.CPUUtil)
	s.nodeFreqScale.WithLabelValues(node).Set(st.FreqScale)
	s.nodeRaplCapW.WithLabelValues(node).Set(st.CapWatts)
	// Encoded under the lock: the physics tick writes st.
	body, err := json.Marshal(map[string]any{"node": node, "state": st})
	s.mu.Unlock()
	if err != nil {
		status = http.StatusInternalServerError
		http.Error(w, err.Error(), status)
		return
	}
	_, _ = w.Write(append(body, '\n'))
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.debugNodesPayload())
}

func (s *simulator) handleDebugJobs(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	defer s.observeRequest("debug_jobs", r.Method, &status, start)
	if r.Method != http.MethodGet {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	if s.workload == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"count": 0, "jobs": []any{}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.debugJobsPayload())
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.debugEventsPayload(int(floatEnv("SIM_DEBUG_EVENT_LIMIT", 200))))
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.debugEnergyPayload())
}

func (s *simulator) handleDebugTimeseries(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	defer s.observeRequest("debug_timeseries", r.Method, &status, start)
	if r.Method != http.MethodGet {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	if s.tsPath == "" {
		status = http.StatusNotFound
		http.Error(w, "timeseries CSV not enabled", status)
		return
	}
	// Flush writer before reading the file.
	s.mu.Lock()
	if s.tsWriter != nil {
		_ = s.tsWriter.Flush()
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/csv")
	http.ServeFile(w, r, s.tsPath)
}

// defaultSimAddr is the listen address without SIM_ADDR: every interface in
// a pod, where a Service reaches it, and the loopback interface elsewhere,
// since the HTTP API has no authentication.
func defaultSimAddr() string {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return ":18080"
	}
	return "127.0.0.1:18080"
}

// observeRequest counts a request. Methods outside the standard set share
// one label value, so a client cannot grow the series without bound.
func (s *simulator) observeRequest(route, method string, status *int, start time.Time) {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		method = "other"
	}
	s.requestsTotal.WithLabelValues(route, method, strconv.Itoa(*status)).Inc()
	s.requestDuration.WithLabelValues(route, method).Observe(time.Since(start).Seconds())
}

// updateFacilityMetrics computes the facility (package facility: a
// sinusoidal ambient temperature, and a PUE that grows with it and with the
// IT load) over every node's power and sets the facility gauges.
func (s *simulator) updateFacilityMetrics(now time.Time) {
	var itPowerW float64
	s.mu.RLock()
	for _, st := range s.state {
		if st.TotalAvgPowerWatts > 0 {
			itPowerW += st.TotalAvgPowerWatts
		}
	}
	s.mu.RUnlock()
	params := facility.Params{
		BaseAmbientC: s.facilityBaseAmbientTempC,
		AmplitudeC:   s.facilityTempAmplitudeC,
		PeriodH:      s.facilityTempPeriodH,
	}
	s.facilityMu.Lock()
	v, peak := facility.Compute(params, now.Sub(s.facilityStart()), itPowerW, s.facilityPeakITPowerW)
	s.facilityPeakITPowerW = peak
	s.facilityMu.Unlock()
	s.setFacilityGauges(v)
}

// facilityStart is the start of the ambient temperature cycle: the
// workload start with a trace, else the simulator start.
func (s *simulator) facilityStart() time.Time {
	if s.workload != nil {
		return s.workload.startTime
	}
	return s.startTime
}

// setFacilityGauges sets the facility gauges from v, rounded for display:
// powers to 0.01 W and the PUE to 0.001.
func (s *simulator) setFacilityGauges(v facility.Values) {
	s.facilityAmbientTemp.Set(v.AmbientC)
	s.facilityITPowerW.Set(math.Round(v.ITPowerW*100) / 100)
	s.facilityPUE.Set(math.Round(v.PUE*1000) / 1000)
	s.facilityCoolingPowerW.Set(math.Round(v.CoolingPowerW*100) / 100)
}

// clusterUtilization is the cluster's CPU and GPU utilization: each node's
// utilization weighted by its CPU cores (CPUCapacityCores, 1 when unknown)
// and by its GPUs, so a node without GPUs does not count toward the GPU
// figure and a 384-core node counts six times a 64-core one.
func clusterUtilization(states []*nodeState) (cpu, gpu float64) {
	var cpuW, cpuSum, gpuW, gpuSum float64
	for _, st := range states {
		cores := st.CPUCapacityCores
		if cores <= 0 {
			cores = 1
		}
		cpuW += cores
		cpuSum += cores * st.CPUUtil
		if n := float64(len(st.GPUDevices)); n > 0 {
			gpuW += n
			gpuSum += n * st.GPUUtil
		}
	}
	if cpuW > 0 {
		cpu = cpuSum / cpuW
	}
	if gpuW > 0 {
		gpu = gpuSum / gpuW
	}
	return cpu, gpu
}

// appendTimeseriesRow writes one CSV row to the time-series file with the
// current cluster power state. Called once per workload tick from the
// workload loop, after updateFacilityMetrics.
func (s *simulator) appendTimeseriesRow(now time.Time) {
	if s.tsWriter == nil {
		return
	}
	s.mu.RLock()
	var itPowerW, cpuPowerW, gpuPowerW float64
	var nodesActive, podsRunning int
	states := make([]*nodeState, 0, len(s.state))
	for _, st := range s.state {
		if st == nil {
			continue
		}
		nodesActive++
		podsRunning += st.PodsRunning
		cpuPowerW += st.CPUInstantPowerWatts
		gpuPowerW += st.GPUPowerWatts
		if st.TotalAvgPowerWatts > 0 {
			itPowerW += st.TotalAvgPowerWatts
		} else {
			itPowerW += st.CPUInstantPowerWatts + st.GPUPowerWatts
		}
		states = append(states, st)
	}
	clusterCPUUtil, clusterGPUUtil := clusterUtilization(states)
	energyJ := s.energyTotalJ
	s.mu.RUnlock()

	// Use facility gauges for PUE/cooling/ambient (set by updateFacilityMetrics).
	pue := readGaugeValue(s.facilityPUE)
	ambientTemp := readGaugeValue(s.facilityAmbientTemp)
	coolingPowerW := readGaugeValue(s.facilityCoolingPowerW)
	facilityPowerW := itPowerW + coolingPowerW

	// Since the workload start with a trace, else since the simulator
	// start, which emulate mode writes rows from.
	elapsed := now.Sub(s.facilityStart()).Seconds()

	s.persistMu.Lock()
	_, _ = fmt.Fprintf(s.tsWriter,
		"%s,%.1f,%.1f,%.1f,%.1f,%.4f,%.1f,%.1f,%.2f,%.4f,%.4f,%d,%d,%.1f\n",
		now.UTC().Format(time.RFC3339),
		elapsed,
		itPowerW, cpuPowerW, gpuPowerW,
		pue, coolingPowerW, facilityPowerW,
		ambientTemp,
		clusterCPUUtil, clusterGPUUtil,
		nodesActive, podsRunning,
		energyJ,
	)
	s.persistMu.Unlock()
}

// closePersistence flushes and closes the time-series CSV file and the
// events journal, on a clean stop. Events recorded after it are kept in
// memory only.
func (s *simulator) closePersistence() {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if s.tsWriter != nil {
		_ = s.tsWriter.Flush()
	}
	if s.tsFile != nil {
		_ = s.tsFile.Close()
		s.tsFile = nil
	}
	if s.journal != nil {
		if err := s.journal.Close(); err != nil {
			log.Printf("warning: events journal close failed: %v", err)
		}
		s.journal = nil
	}
}

// readGaugeValue extracts the current float64 from a prometheus.Gauge.
func readGaugeValue(g prometheus.Gauge) float64 {
	m := &dto.Metric{}
	if err := g.Write(m); err != nil {
		return 0
	}
	if m.Gauge != nil && m.Gauge.Value != nil {
		return *m.Gauge.Value
	}
	return 0
}

// gaugeValue reads the current value from a Prometheus gauge.
func gaugeValue(g prometheus.Gauge) float64 {
	m := &dto.Metric{}
	if err := g.Write(m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
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

func (s *simulator) debugNodesPayload() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
			cp := st.clone()
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
	return map[string]any{
		"count": len(out),
		"nodes": out,
	}
}

func (s *simulator) debugJobsPayload() map[string]any {
	if s.workload == nil {
		return map[string]any{"count": 0, "jobs": []any{}}
	}
	now := time.Now().UTC()
	out := make([]map[string]any, 0, len(s.workload.jobs))
	s.workload.jobsMu.Lock()
	defer s.workload.jobsMu.Unlock()
	for _, j := range s.workload.jobs {
		stalledForSec := 0.0
		if !j.LastProgressAt.IsZero() && !j.Completed {
			stalledForSec = now.Sub(j.LastProgressAt).Seconds()
		}
		out = append(out, map[string]any{
			"jobId":             j.JobID,
			"workloadId":        j.WorkloadID,
			"workloadType":      j.WorkloadType,
			"podRole":           j.PodRole,
			"gang":              j.Gang,
			"class":             j.Class,
			"submitted":         j.Submitted,
			"completed":         j.Completed,
			"node":              j.NodeName,
			"requestedCpuCores": j.RequestedCPUCores,
			"requestedGpus":     j.RequestedGPUs,
			"cpuUnitsTotal":     j.CPUUnitsTotal,
			"cpuUnitsRemaining": j.CPUUnitsRemaining,
			"gpuUnitsTotal":     j.GPUUnitsTotal,
			"gpuUnitsRemaining": j.GPUUnitsRemaining,
			"lastProgressAt":    j.LastProgressAt,
			"deleteRequestedAt": j.DeleteRequestedAt,
			"stalledForSec":     stalledForSec,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		ic := out[i]["completed"].(bool)
		jc := out[j]["completed"].(bool)
		if ic != jc {
			return !ic
		}
		return fmt.Sprint(out[i]["jobId"]) < fmt.Sprint(out[j]["jobId"])
	})
	return map[string]any{
		"count": len(out),
		"jobs":  out,
	}
}

func (s *simulator) debugEventsPayload(limit int) map[string]any {
	s.mu.RLock()
	events := append([]simEvent(nil), s.events...)
	s.mu.RUnlock()
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return map[string]any{
		"count":  len(events),
		"events": events,
	}
}

func (s *simulator) debugEnergyPayload() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	perNode := map[string]float64{}
	for node, v := range s.energyJByNode {
		perNode[node] = v
	}
	return map[string]any{
		"totalJoules":  s.energyTotalJ,
		"byNodeJoules": perNode,
		"lastUpdated":  s.energyLastTs.Format(time.RFC3339Nano),
	}
}

func (s *simulator) startDebugSnapshotLoop(interval time.Duration) {
	if strings.TrimSpace(s.persistDir) == "" {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	s.snapshotDebugState()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		s.snapshotDebugState()
	}
}

func (s *simulator) snapshotDebugState() {
	if strings.TrimSpace(s.persistDir) == "" {
		return
	}
	s.persistJSONAtomic("nodes.json", s.debugNodesPayload())
	s.persistJSONAtomic("jobs.json", s.debugJobsPayload())
	s.persistJSONAtomic("events.json", s.debugEventsPayload(0))
	s.persistJSONAtomic("energy.json", s.debugEnergyPayload())
	// Flush timeseries CSV so partial data is available for collection.
	if s.tsWriter != nil {
		s.persistMu.Lock()
		_ = s.tsWriter.Flush()
		s.persistMu.Unlock()
	}
}

func (s *simulator) persistJSONAtomic(name string, payload any) {
	if strings.TrimSpace(s.persistDir) == "" {
		return
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("warning: persist %s marshal failed: %v", name, err)
		return
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	target := filepath.Join(s.persistDir, name)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		log.Printf("warning: persist %s write failed: %v", name, err)
		return
	}
	if err := os.Rename(tmp, target); err != nil {
		log.Printf("warning: persist %s rename failed: %v", name, err)
	}
}

// eventJournalFile is the journal of every recorded event, one JSON object
// per line, under SIM_DEBUG_PERSIST_DIR.
const eventJournalFile = "events.ndjson"

// persistEventJournal appends evs to the journal, in order, with one write.
func (s *simulator) persistEventJournal(evs []simEvent) {
	if len(evs) == 0 {
		return
	}
	var buf bytes.Buffer
	for _, e := range evs {
		line, err := json.Marshal(e)
		if err != nil {
			log.Printf("warning: persist events journal marshal failed: %v", err)
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if s.journal == nil {
		return
	}
	if _, err := s.journal.Write(buf.Bytes()); err != nil {
		log.Printf("warning: persist events journal write failed: %v", err)
	}
}

// defaultPollInterval is the period of the pod poll when SIM_POLL_INTERVAL is
// unset or not positive.
const defaultPollInterval = 15 * time.Second

// startPodPolling lists pods and nodes every interval. An interval of 0 or
// less, which time.NewTicker rejects with a panic, takes the default.
func (s *simulator) startPodPolling(interval time.Duration) {
	if interval <= 0 {
		log.Printf("warning: SIM_POLL_INTERVAL=%s is not positive, using %s", interval, defaultPollInterval)
		interval = defaultPollInterval
	}
	kube, err := kubeClientFactory(20, 40, 8*time.Second)
	if err != nil {
		log.Printf("warning: pod polling disabled: %v", err)
		return
	}
	log.Printf("pod polling enabled interval=%s", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.pollOnce(kube)
		<-ticker.C
	}
}

// pollOnce is one pod poll: one list of the pods and one of the Nodes,
// from which the node state, the per-node gauges and, in emulate mode, the
// check of the spec nodes' Nodes are all taken.
func (s *simulator) pollOnce(kube kubernetes.Interface) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	counts, podsDetailed, err := podPollFunc(ctx, kube)
	var nodes, nodeAnnotations map[string]map[string]string
	var nodeErr error
	if err == nil {
		nodes, nodeAnnotations, nodeErr = nodeListFunc(ctx, kube)
	}
	cancel()
	if err != nil {
		log.Printf("warning: pod polling list failed: %v", err)
		return
	}
	if nodeErr != nil {
		log.Printf("warning: node polling list failed: %v", nodeErr)
		return
	}

	s.refreshNodeStateFromKubeData(counts, podsDetailed, nodes)

	// Compute power for all selected nodes in a single lock hold to
	// reduce lock acquisition overhead.
	type pollSnap struct {
		node                 string
		power, capW, cpuUtil float64
		throttlePct          int
		podsRunning          int
		freqScale            float64
	}
	var pollSnaps []pollSnap
	s.mu.Lock()
	for node, labels := range nodes {
		// An emulated node's power comes from its snapshot.
		if !s.nodeSelected(labels) || s.emulated(node) {
			continue
		}
		st := s.state[node]
		if st == nil {
			continue
		}
		power := observe(st).powerW
		pollSnaps = append(pollSnaps, pollSnap{
			node: node, power: power, capW: st.CapWatts,
			cpuUtil: st.CPUUtil, throttlePct: st.ThrottlePct,
			podsRunning: st.PodsRunning, freqScale: st.FreqScale,
		})
	}
	s.mu.Unlock()
	for _, ps := range pollSnaps {
		s.nodeCapW.WithLabelValues(ps.node).Set(ps.capW)
		s.nodeThrottlePct.WithLabelValues(ps.node).Set(float64(ps.throttlePct))
		s.nodePowerW.WithLabelValues(ps.node).Set(ps.power)
		s.nodePods.WithLabelValues(ps.node).Set(float64(ps.podsRunning))
		s.nodeUtilCPU.WithLabelValues(ps.node).Set(ps.cpuUtil)
		s.nodeFreqScale.WithLabelValues(ps.node).Set(ps.freqScale)
		s.nodeRaplCapW.WithLabelValues(ps.node).Set(ps.capW)
	}
	if s.emu != nil {
		s.checkEmulatedNodes(nodeAnnotations)
	}
}

func (s *simulator) refreshNodeStateFromKubeData(counts map[string]int, pods []runningPodInfo, nodeLabels map[string]map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	selected := map[string]bool{}
	intentByNode := map[string]map[string]int{}
	others := map[string]int{}
	for node := range nodeLabels {
		s.nodeSeen[node] = true
	}
	for _, p := range pods {
		if p.Node == "" {
			continue
		}
		if p.Running && (s.workload == nil || s.workload.jobByID[p.JobID] == nil) {
			others[p.Node]++
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
		// A spec node is emulated whatever SIM_NODE_SELECTOR and its labels
		// say, with its profile's capacity.
		if s.emulated(node) {
			selected[node] = true
			m, _ := s.emu.emu.Member(node)
			st, ok := s.state[node]
			if !ok {
				st = newEmulatedNodeState(m, s.emu.simNow())
				s.state[node] = st
			}
			st.CPUCapacityCores, st.GPUCapacityDevices = emulatedCapacity(m)
			continue
		}
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
		cpuCapacity := parsePositiveFloatString(labels["joulie.io/hw.cpu-cores"])
		if cpuCapacity <= 0 {
			cpuCapacity = 16
		}
		gpuCapacity := parsePositiveFloatString(labels["joulie.io/hw.gpu-count"])
		if gpuCapacity <= 0 {
			gpuCapacity = float64(model.GPU.Count)
		}
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
				TargetCapWatts:          model.DefaultCapW,
				FreqScale:               1,
				CPUWorkClass:            "cpu.mixed",
				LastAction:              "none",
				LastResult:              "none",
				LastUpdate:              time.Now().UTC(),
				SimTime:                 time.Now().UTC(),
				ByIntentClass:           map[string]int{},
				TargetThrottlePct:       0,
				GPUCapWattsPerGpu:       model.GPU.MaxWattsPerGPU,
				GPUTargetCapWattsPerGpu: model.GPU.MaxWattsPerGPU,
				GPUWorkClass:            "gpu.mixed",
				GPUPerfMultiplier:       1.0,
				CPUCapacityCores:        cpuCapacity,
				GPUCapacityDevices:      gpuCapacity,
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
			stepNode(st, model, 0, physMaxSubstep)
			s.state[node] = st
		}
		st.CPUCapacityCores = cpuCapacity
		st.GPUCapacityDevices = gpuCapacity
	}

	for node, st := range s.state {
		if s.emulated(node) {
			// The pods do not set an emulated node's load; its
			// utilization is its snapshot's (applyEmulateSnapshot), 0
			// before the first.
			st.PodsRunning = counts[node]
			st.ByIntentClass = intentByNode[node]
			if st.ByIntentClass == nil {
				st.ByIntentClass = map[string]int{}
			}
			continue
		}
		if !selected[node] {
			st.PodsRunning = 0
			st.OtherPods = 0
			st.ByIntentClass = map[string]int{}
			st.CPUUtil = 0
			st.CPUWorkClass = "cpu.mixed"
			st.GPUUtil = 0
			st.GPUWorkClass = "gpu.mixed"
			continue
		}
		st.PodsRunning = counts[node]
		st.OtherPods = others[node]
		st.ByIntentClass = intentByNode[node]
		if st.ByIntentClass == nil {
			st.ByIntentClass = map[string]int{}
		}
		// The workload tick sets the load of a node with jobs, every second
		// and counting a job's CPU and GPU only while it has that work
		// left; the poll set it too, from every running job, and the
		// physics stepped on that load until the next tick. The tick marks
		// such a node under s.mu (HasJobs); the jobs themselves are the
		// tick's, unlocked.
		if s.workload == nil || !st.HasJobs {
			setNoJobLoad(st)
		}
	}
}

// setNoJobLoad sets the load of a node that runs no job of the workload:
// what the job loop computes for an empty job list, with the CPU at the
// estimate of 0.12 per running pod outside the workload (OtherPods). A
// workload pod carries its job's load while the job runs and none after,
// though the pod poll counts it until its deletion. The job loops visit
// only nodes with jobs, so a node whose last job ended used to keep that
// job's load, and draw its power, until a new job arrived.
func setNoJobLoad(st *nodeState) {
	st.HasJobs = false
	st.CPUUtil = math.Min(1, float64(st.OtherPods)*0.12)
	st.GPUUtil = 0
	st.CPUWorkClass = "cpu.mixed"
	st.GPUWorkClass = "gpu.mixed"
	st.MemoryIntensity = 0
	st.IOIntensity = 0
	st.CPUFeedIntensity = 0
}

func (s *simulator) nodeSelected(nodeLabels map[string]string) bool {
	if s.selector == nil || s.selector.Empty() {
		return true
	}
	return s.selector.Matches(labels.Set(nodeLabels))
}

// listPodsForPoll lists every pod once and returns the Running pods per
// node and the Running or Pending pods bound to a node.
func listPodsForPoll(ctx context.Context, kube kubernetes.Interface) (map[string]int, []runningPodInfo, error) {
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	return runningPodsByNode(pods.Items), runningPodDetails(pods.Items), nil
}

// runningPodsByNode counts the Running pods of each node, deleted ones
// aside.
func runningPodsByNode(pods []corev1.Pod) map[string]int {
	counts := map[string]int{}
	for _, p := range pods {
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
	return counts
}

func listRunningPodsDetailed(ctx context.Context, kube kubernetes.Interface) ([]runningPodInfo, error) {
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return runningPodDetails(pods.Items), nil
}

// runningPodDetails is every Running or Pending pod bound to a node,
// deleted ones aside.
func runningPodDetails(pods []corev1.Pod) []runningPodInfo {
	out := make([]runningPodInfo, 0, len(pods))
	for _, p := range pods {
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
			JobID:       p.Annotations[workload.AnnotationJobID],
			Running:     p.Status.Phase == "Running",
		})
	}
	return out
}

// listNodeMeta lists the Nodes once and returns the labels and the
// annotations of each, by name.
func listNodeMeta(ctx context.Context, kube kubernetes.Interface) (labels, annotations map[string]map[string]string, err error) {
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	labels = make(map[string]map[string]string, len(nodes.Items))
	annotations = make(map[string]map[string]string, len(nodes.Items))
	for _, n := range nodes.Items {
		l := make(map[string]string, len(n.Labels))
		for k, v := range n.Labels {
			l[k] = v
		}
		a := make(map[string]string, len(n.Annotations))
		for k, v := range n.Annotations {
			a[k] = v
		}
		labels[n.Name], annotations[n.Name] = l, a
	}
	return labels, annotations, nil
}

// newSimEvent is an event of kind on node, stamped now.
func newSimEvent(kind, node string, payload map[string]any) simEvent {
	return simEvent{Timestamp: time.Now().UTC(), Kind: kind, Node: node, Payload: payload}
}

func (s *simulator) recordEvent(kind, node string, payload map[string]any) {
	s.recordEvents([]simEvent{newSimEvent(kind, node, payload)})
}

// recordEvents keeps evs, in order, for /debug/events and appends them to
// the events journal with one write.
func (s *simulator) recordEvents(evs []simEvent) {
	s.persistEventJournal(evs)
	if s.eventMax <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evs...)
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

func parsePositiveFloatString(raw string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// initWorkloadEngineWithOptions loads the trace at path, a file or a
// directory of .jsonl parts, into the simulator's legacy engine with opts.
// A v1 trace loads per run instead (loadRunTrace).
func (s *simulator) initWorkloadEngineWithOptions(path string, opts workloadOptions) error {
	engine, parts, err := newWorkloadEngineFromTrace(path, opts)
	if err != nil {
		return err
	}
	s.workload = engine
	log.Printf("workload trace loaded jobs=%d parts=%d path=%s model=%s", len(engine.jobs), parts, path, opts.model())
	return nil
}

// newWorkloadEngine returns an engine with no jobs, started now, with opts
// and the base speed of SIM_BASE_SPEED_PER_CORE.
func newWorkloadEngine(opts workloadOptions) *workloadEngine {
	return &workloadEngine{
		startTime:      time.Now().UTC(),
		baseSpeedCore:  floatEnv(workload.EnvBaseSpeedPerCore, 1.0),
		jobs:           []*simJob{},
		jobByID:        map[string]*simJob{},
		jobsByWorkload: map[string][]*simJob{},
		opts:           opts,
	}
}

// newWorkloadEngineFromTrace returns a legacy engine holding the jobs of the
// trace at path in submit order, and the number of files read. It sorts with
// sort.Slice, as legacy always has, so the order of jobs due at the same
// offset stays what the experiments ran. Each pod is named after its jobId,
// and a name two jobs share in one namespace gets a suffix
// (suffixCollidingPodNames).
func newWorkloadEngineFromTrace(path string, opts workloadOptions) (*workloadEngine, int, error) {
	engine := newWorkloadEngine(opts)
	tracePaths, err := resolveTracePaths(path)
	if err != nil {
		return nil, 0, err
	}
	lineNum := 0
	for _, tracePath := range tracePaths {
		if err := loadTraceFileIntoEngine(tracePath, engine, &lineNum); err != nil {
			return nil, 0, err
		}
	}
	suffixCollidingPodNames(engine.jobs)
	sort.Slice(engine.jobs, func(i, j int) bool { return engine.jobs[i].SubmitOffsetSec < engine.jobs[j].SubmitOffsetSec })
	return engine, len(tracePaths), nil
}

// suffixCollidingPodNames gives each legacy job its own pod name. Pods are
// named sim-<sanitized jobId>, cut to 50 characters, so distinct jobIds such
// as a_b and a.b, or two long ids with a common prefix, can share a name in
// one namespace. Before this fix the second create failed with
// AlreadyExists, the job counted as submitted with no pod of its own, and it
// was completed by force after the stall timeout. In trace order, the first
// job keeps the name and each later one gets the lowest -<n> suffix, from 2,
// that no other job's name uses.
func suffixCollidingPodNames(jobs []*simJob) {
	key := func(ns, name string) string { return ns + "/" + name }
	natural := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		natural[key(j.Namespace, j.PodName)] = true
	}
	taken := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		k := key(j.Namespace, j.PodName)
		if !taken[k] {
			taken[k] = true
			continue
		}
		for n := 2; ; n++ {
			name := fmt.Sprintf("%s-%d", j.PodName, n)
			if c := key(j.Namespace, name); !taken[c] && !natural[c] {
				log.Printf("warning: trace job %s shares pod name %s/%s with an earlier job; its pod is %s", j.JobID, j.Namespace, j.PodName, name)
				j.PodName = name
				taken[c] = true
				break
			}
		}
	}
}

func resolveTracePaths(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		paths = append(paths, filepath.Join(path, entry.Name()))
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("no .jsonl trace parts found in %s", path)
	}
	return paths, nil
}

// maxTraceLineBytes is the longest trace line the loader reads. bufio's
// default of 64 KiB failed whole traces on one job with a long podTemplate.
const maxTraceLineBytes = 16 << 20

// traceGPUKey is a requests key the loader reads as a GPU request, and the
// extended resource the pod then requests.
type traceGPUKey struct {
	key         string
	resourceKey string
}

// legacyTraceGPUKeys are the GPU requests keys of every trace, in order of
// precedence: the first one present is the job's GPU request.
var legacyTraceGPUKeys = []traceGPUKey{
	{key: "nvidia.com/gpu", resourceKey: "nvidia.com/gpu"},
	{key: "amd.com/gpu", resourceKey: "amd.com/gpu"},
	{key: "gpu", resourceKey: "nvidia.com/gpu"},
}

// traceGPUKeys are the GPU requests keys of one record of requests req. A v3
// record adds the other resources a device plugin advertises for a shared
// GPU (nvidia.com/gpu.shared) and for MIG slices (nvidia.com/mig-<profile>,
// in name order), after the legacy keys, so a v2 record reads as before.
func traceGPUKeys(req map[string]any, v3 bool) []traceGPUKey {
	if !v3 {
		return legacyTraceGPUKeys
	}
	keys := append([]traceGPUKey{}, legacyTraceGPUKeys...)
	keys = append(keys, traceGPUKey{key: "nvidia.com/gpu.shared", resourceKey: "nvidia.com/gpu.shared"})
	var mig []string
	for k := range req {
		if strings.HasPrefix(k, "nvidia.com/mig-") {
			mig = append(mig, k)
		}
	}
	sort.Strings(mig)
	for _, k := range mig {
		keys = append(keys, traceGPUKey{key: k, resourceKey: k})
	}
	return keys
}

func loadTraceFileIntoEngine(path string, engine *workloadEngine, lineNum *int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxTraceLineBytes)
	for sc.Scan() {
		*lineNum++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return fmt.Errorf("trace parse line %d file=%s: %w", *lineNum, path, err)
		}
		tp, _ := rec["type"].(string)
		if tp != "job" {
			continue
		}
		jobID, _ := rec["jobId"].(string)
		if jobID == "" {
			continue
		}
		schemaVersion, _ := rec["schemaVersion"].(string)
		v3 := schemaVersion == workload.TraceSchemaV3
		workloadID, _ := rec["workloadId"].(string)
		workloadType, _ := rec["workloadType"].(string)
		podRole, _ := rec["podRole"].(string)
		gang, _ := rec["gang"].(bool)
		class := "standard"
		if ic, ok := rec["intentClass"].(string); ok && ic != "" {
			class = ic
		} else if podTpl, ok := rec["podTemplate"].(map[string]any); ok {
			if affinityRaw, ok := podTpl["affinity"].(map[string]any); ok {
				class = classifyClassFromAffinityMap(affinityRaw)
			}
		}
		requestedCPU := 1.0
		requestedMemoryMiB := 0.0
		requestedGPUs := 0.0
		gpuResourceName := ""
		if podTpl, ok := rec["podTemplate"].(map[string]any); ok {
			if reqRaw, ok := podTpl["requests"].(map[string]any); ok {
				if cpuRaw, ok := reqRaw["cpu"].(string); ok {
					requestedCPU = parseCPURequestOrDefault(cpuRaw, 1.0)
				}
				if memRaw, ok := reqRaw["memory"].(string); ok {
					requestedMemoryMiB = parseMemoryRequestMiB(memRaw)
				}
				for _, gpuReq := range traceGPUKeys(reqRaw, v3) {
					raw, ok := reqRaw[gpuReq.key]
					if !ok {
						continue
					}
					rawStr, ok := raw.(string)
					if !ok {
						log.Printf("warning: trace line %d file=%s job=%s has non-string %s request; skipping GPU request", *lineNum, path, jobID, gpuReq.key)
						break
					}
					gpuQty, ok := parseIntegerResourceRequest(rawStr)
					if !ok {
						log.Printf("warning: trace line %d file=%s job=%s has non-integer %s request=%q; skipping GPU request", *lineNum, path, jobID, gpuReq.key, rawStr)
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
		peerTransferIntensity := 0.0
		sensitivitySet := false
		if sensRaw, ok := rec["sensitivity"].(map[string]any); ok {
			if v, ok := sensRaw["cpu"].(float64); ok {
				sensCPU = clamp01(v)
				sensitivitySet = true
			}
			if v, ok := sensRaw["gpu"].(float64); ok {
				sensGPU = clamp01(v)
				sensitivitySet = true
			}
		}
		var placement workload.Placement
		if podTpl, ok := rec["podTemplate"].(map[string]any); ok {
			p, err := workload.ParsePlacement(podTpl)
			if err != nil {
				return fmt.Errorf("trace line %d file=%s job=%s: %w", *lineNum, path, jobID, err)
			}
			placement = p
			if !v3 {
				// A legacy pod keeps the trace's affinity (legacyAffinity);
				// its node selector and tolerations are the simulator's.
				placement = workload.Placement{Affinity: p.Affinity}
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
			if v, ok := profRaw["peerTransferIntensity"].(float64); ok {
				peerTransferIntensity = clamp01(v)
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
			JobID:              jobID,
			WorkloadID:         workloadID,
			WorkloadType:       workloadType,
			PodRole:            podRole,
			Gang:               gang,
			Class:              class,
			Namespace:          namespace,
			SubmitOffsetSec:    submitOffset,
			RequestedCPUCores:  requestedCPU,
			RequestedMemoryMiB: requestedMemoryMiB,
			CPUUnitsTotal:      cpuUnits,
			CPUUnitsRemaining:  cpuUnits,
			SensitivityCPU:     sensCPU,
			CPUWorkClass:       cpuWorkClass,
			CPUUtilTarget:      cpuUtilTarget,
			RequestedGPUs:      requestedGPUs,
			GPUResourceName:    gpuResourceName,
			GPUUnitsTotal:      gpuUnits,
			GPUUnitsRemaining:  gpuUnits,
			SensitivityGPU:     sensGPU,
			GPUWorkClass:       gpuWorkClass,
			GPUUtilTarget:      gpuUtilTarget,
			MemoryIntensity:    memoryIntensity,
			IOIntensity:        ioIntensity,
			CPUFeedIntensity:   cpuFeedIntensity,
			PodName:            fmt.Sprintf("sim-%s", sanitizeK8sName(jobID)),
			SchemaVersion:      schemaVersion,
			Affinity:           placement.Affinity,
			NodeSelector:       placement.NodeSelector,
			Tolerations:        placement.Tolerations,
			SensitivitySet:     sensitivitySet,
		}
		job.PeerTransferIntensity = peerTransferIntensity
		engine.jobs = append(engine.jobs, job)
		engine.jobByID[jobID] = job
		if workloadID != "" {
			engine.jobsByWorkload[workloadID] = append(engine.jobsByWorkload[workloadID], job)
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("trace line %d file=%s is longer than %d bytes: %w", *lineNum+1, path, maxTraceLineBytes, err)
		}
		return err
	}
	return nil
}

// defaultWorkloadTick is the period of the legacy workload loop when
// SIM_WORKLOAD_TICK is unset or not positive.
const defaultWorkloadTick = time.Second

// startWorkloadLoop runs the legacy workload engine every interval of wall
// time. An interval of 0 or less, which time.NewTicker rejects with a panic,
// takes the default, as SIM_PHYSICS_TICK does (runPhysicsLoop).
func (s *simulator) startWorkloadLoop(interval time.Duration) {
	if interval <= 0 {
		log.Printf("warning: %s=%s is not positive, using %s", workload.EnvTick, interval, defaultWorkloadTick)
		interval = defaultWorkloadTick
	}
	kube, err := kubeClientFactory(s.workload.opts.kubeQPS(), s.workload.opts.kubeBurst(), 8*time.Second)
	if err != nil {
		log.Printf("warning: workload loop disabled: %v", err)
		return
	}
	log.Printf("workload loop enabled interval=%s", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	last := time.Now().UTC()
	tickCount := 0
	for {
		now := time.Now().UTC()
		dt := now.Sub(last).Seconds()
		if dt <= 0 {
			dt = interval.Seconds()
		}
		last = now
		tickCount++
		if tickCount <= 5 || tickCount%30 == 0 {
			log.Printf("workload tick %d dt=%.2fs jobs=%d", tickCount, dt, len(s.workload.jobs))
		}
		// In emulate mode the emulator's publish callback does these,
		// once per tick. The physics tick books the energy.
		if s.emu == nil {
			s.updateFacilityMetrics(now)
			s.appendTimeseriesRow(now)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		s.injectTraceJobs(ctx, kube, now)
		if ctx.Err() == nil {
			s.advanceJobProgress(ctx, kube, dt, now)
		}
		cancel()
		<-ticker.C
	}
}

func (s *simulator) injectTraceJobs(ctx context.Context, kube kubernetes.Interface, now time.Time) {
	if s.workload == nil {
		return
	}
	s.workload.jobsMu.Lock()
	defer s.workload.jobsMu.Unlock()
	// Rate-limit pod injection to avoid overwhelming the K8s API server: at
	// most SIM_WORKLOAD_MAX_CREATES_PER_TICK pods per tick (20 by default);
	// the remaining jobs are injected on the next tick.
	maxInjectPerTick := s.workload.opts.maxCreatesPerTick()
	injected := 0
	for _, j := range s.workload.jobs {
		if !underCap(injected, maxInjectPerTick) {
			break
		}
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
					workload.LabelPartOf: workload.PartOfWorkload,
				},
				Annotations: map[string]string{
					workload.AnnotationJobID:             j.JobID,
					workload.AnnotationWorkloadID:        j.WorkloadID,
					workload.AnnotationWorkloadType:      j.WorkloadType,
					workload.AnnotationPodRole:           j.PodRole,
					workload.AnnotationCPUUtilPct:        strconv.FormatFloat(j.CPUUtilTarget*100, 'f', 1, 64),
					workload.AnnotationGPUUtilPct:        strconv.FormatFloat(j.GPUUtilTarget*100, 'f', 1, 64),
					workload.AnnotationMemoryPressurePct: strconv.FormatFloat(j.MemoryIntensity*100, 'f', 1, 64),
					workload.AnnotationIOIntensity:       strconv.FormatFloat(j.IOIntensity, 'f', 2, 64),
				},
			},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Affinity:      legacyAffinity(j),
				Containers: []corev1.Container{
					{
						Name:  workload.ContainerName,
						Image: envOrDefault(workload.EnvImage, workload.DefaultImage),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(strconv.FormatFloat(j.RequestedCPUCores, 'f', -1, 64)),
								corev1.ResourceMemory: resource.MustParse(formatMemoryMiB(j.RequestedMemoryMiB)),
							},
						},
					},
				},
				Tolerations: []corev1.Toleration{workload.KWOKToleration()},
				NodeSelector: map[string]string{
					"type": "kwok",
				},
			},
		}
		if j.WorkloadType != "" {
			pod.ObjectMeta.Labels[workload.LabelWorkloadType] = sanitizeK8sName(j.WorkloadType)
		}
		if j.PodRole != "" {
			pod.ObjectMeta.Labels[workload.LabelPodRole] = sanitizeK8sName(j.PodRole)
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
		createCtx, createCancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := kube.CoreV1().Pods(j.Namespace).Create(createCtx, pod, metav1.CreateOptions{})
		createCancel()
		if err != nil {
			if !apierrors.IsAlreadyExists(err) {
				log.Printf("warning: create workload pod job=%s: %v", j.JobID, err)
				continue
			}
		}
		j.Submitted = true
		j.SubmittedAt = now
		injected++
		s.jobSubmitted.WithLabelValues(j.Class).Inc()
	}
}

// legacyAffinity is the affinity of a legacy pod: the trace's own, with the
// NotIn-eco requirement of a performance pod ANDed into each required term
// (workload.MergeIntent). A trace without affinity gives what the intent
// class alone gave. The trace's affinity used to be dropped, so a CPU-only
// job of experiments 02 and 03, which their sweeps restrict to
// joulie.io/hw.kind=cpu-only nodes, could run on a GPU node.
func legacyAffinity(j *simJob) *corev1.Affinity {
	return workload.MergeIntent(j.Affinity, j.Class, workload.IntentAffinity)
}

func classifyClassFromPodSpec(spec *corev1.PodSpec) string {
	// No power-profile scheduling constraint means standard class.
	if spec == nil || spec.Affinity == nil || spec.Affinity.NodeAffinity == nil || spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return "standard"
	}
	required := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	perfAllowed := false
	ecoAllowed := false
	for _, term := range required.NodeSelectorTerms {
		termPerf := true
		termEco := true
		for _, expr := range term.MatchExpressions {
			if expr.Key != workload.LabelPowerProfile {
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
	if perfAllowed && !ecoAllowed {
		return "performance"
	}
	// Legacy "eco" intent and anything else maps to "standard".
	return "standard"
}

func classifyClassFromAffinityMap(affinityRaw map[string]any) string {
	b, err := json.Marshal(map[string]any{"affinity": affinityRaw})
	if err != nil {
		return "standard"
	}
	var wrapper struct {
		Affinity *corev1.Affinity `json:"affinity"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		return "standard"
	}
	spec := &corev1.PodSpec{Affinity: wrapper.Affinity}
	return classifyClassFromPodSpec(spec)
}

func (s *simulator) advanceJobProgress(ctx context.Context, kube kubernetes.Interface, dt float64, now time.Time) {
	if s.workload == nil {
		return
	}
	// The jobs' lock covers the stall detection too: deferred calls run
	// last in, first out.
	s.workload.jobsMu.Lock()
	defer s.workload.jobsMu.Unlock()
	// Stall detection runs unconditionally via defer, even if pod listing fails.
	defer s.detectStalledJobs(kube, now)

	pods, err := podDetailFunc(ctx, kube)
	if err != nil {
		log.Printf("warning: workload progress list pods: %v", err)
		return
	}
	byNode := map[string][]*simJob{}
	podByJobID := map[string]runningPodInfo{}
	runningByWorkload := map[string]int{}
	completedByWorkload := map[string]int{}
	submittedByWorkload := map[string]int{}
	for _, j := range s.workload.jobs {
		if j.WorkloadID == "" {
			continue
		}
		if j.Submitted {
			submittedByWorkload[j.WorkloadID]++
		}
		if j.Completed {
			completedByWorkload[j.WorkloadID]++
		}
	}
	for _, p := range pods {
		if p.JobID == "" {
			continue
		}
		podByJobID[p.JobID] = p
		j := s.workload.jobByID[p.JobID]
		if j == nil || j.Completed {
			continue
		}
		j.NodeName = p.Node
		byNode[p.Node] = append(byNode[p.Node], j)
		if j.WorkloadID != "" {
			runningByWorkload[j.WorkloadID]++
		}
	}
	// Clean up pods for completed jobs, rate-limited to avoid blocking the
	// loop: at most SIM_WORKLOAD_MAX_DELETES_PER_TICK (10 by default).
	deletesThisTick := 0
	maxDeletesPerTick := s.workload.opts.maxDeletesPerTick()
	for _, j := range s.workload.jobs {
		if !underCap(deletesThisTick, maxDeletesPerTick) || ctx.Err() != nil {
			break
		}
		if !j.Completed {
			continue
		}
		p, ok := podByJobID[j.JobID]
		if !ok {
			continue
		}
		j.NodeName = p.Node
		s.ensureWorkloadPodDeleted(kube, j)
		deletesThisTick++
	}
	s.mu.Lock()
	for node, st := range s.state {
		if st != nil && len(byNode[node]) == 0 && !s.emulated(node) {
			setNoJobLoad(st)
		}
	}
	s.mu.Unlock()
	for node, jobs := range byNode {
		if ctx.Err() != nil {
			break
		}
		s.mu.RLock()
		st := s.state[node]
		// Access nodeModels directly to avoid recursive RLock (which deadlocks
		// when a writer is waiting, due to Go RWMutex writer-priority semantics).
		model := s.model
		if m, ok := s.nodeModels[node]; ok {
			model = m
		}
		freqScale := 1.0
		gpuCount := 0.0
		if st != nil {
			freqScale = st.FreqScale
			if model.GPU.Count > 0 && model.GPU.MaxWattsPerGPU > 0 {
				gpuCount = float64(model.GPU.Count)
			}
		}
		s.mu.RUnlock()
		if st == nil {
			st = s.getNode(node)
		}
		gpuJobs := 0
		totalCPUReq := 0.0
		totalGPUReq := 0.0
		totalCPUUtilDemand := 0.0
		totalGPUUtilDemand := 0.0
		totalMemoryWeight := 0.0
		totalIOWeight := 0.0
		totalCPUFeedWeight := 0.0
		memoryWeighted := 0.0
		ioWeighted := 0.0
		cpuFeedWeighted := 0.0
		cpuClassWeights := map[string]float64{}
		gpuClassWeights := map[string]float64{}
		for _, j := range jobs {
			if j.CPUUnitsRemaining > 0 {
				weight := math.Max(0.1, j.RequestedCPUCores)
				totalCPUReq += math.Max(0.1, j.RequestedCPUCores)
				cpuClassWeights[j.CPUWorkClass] += weight
				totalCPUUtilDemand += j.RequestedCPUCores * clamp01(j.CPUUtilTarget)
				memoryWeighted += weight * clamp01(j.MemoryIntensity)
				ioWeighted += weight * clamp01(j.IOIntensity)
				totalMemoryWeight += weight
				totalIOWeight += weight
			}
			if j.GPUUnitsRemaining > 0 {
				gpuJobs++
				weight := math.Max(0.1, j.RequestedGPUs)
				totalGPUReq += j.RequestedGPUs
				totalGPUUtilDemand += j.RequestedGPUs * clamp01(j.GPUUtilTarget)
				gpuClassWeights[j.GPUWorkClass] += weight
				memoryWeighted += weight * clamp01(j.MemoryIntensity)
				cpuFeedWeighted += weight * clamp01(j.CPUFeedIntensity)
				totalMemoryWeight += weight
				totalCPUFeedWeight += weight
			}
		}
		// Snapshot values we need after releasing the lock.
		var snapCPUCapacity float64
		var snapThermalThrottle float64
		var snapGPUCapPerGpu float64
		if st != nil {
			// The pod poll writes the capacity under s.mu; read it there.
			s.mu.Lock()
			cpuCapacity := st.CPUCapacityCores
			if cpuCapacity <= 0 {
				cpuCapacity = 16
			}
			st.HasJobs = true
			st.CPUUtil = clamp01(totalCPUUtilDemand / cpuCapacity)
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
			if totalMemoryWeight > 0 {
				st.MemoryIntensity = clamp01(memoryWeighted / totalMemoryWeight)
			} else {
				st.MemoryIntensity = 0
			}
			if totalIOWeight > 0 {
				st.IOIntensity = clamp01(ioWeighted / totalIOWeight)
			} else {
				st.IOIntensity = 0
			}
			if totalCPUFeedWeight > 0 {
				st.CPUFeedIntensity = clamp01(cpuFeedWeighted / totalCPUFeedWeight)
			} else {
				st.CPUFeedIntensity = 0
			}
			snapCPUCapacity = st.CPUCapacityCores
			snapThermalThrottle = st.CPUThermalThrottle
			snapGPUCapPerGpu = st.GPUCapWattsPerGpu
			s.mu.Unlock()
		}
		cpuCapacity := 16.0
		if snapCPUCapacity > 0 {
			cpuCapacity = snapCPUCapacity
		}
		cpuShareFactor := 1.0
		if totalCPUReq > cpuCapacity && cpuCapacity > 0 {
			cpuShareFactor = clamp01(cpuCapacity / totalCPUReq)
		}
		gpuShareFactor := 1.0
		if gpuCount > 0 && totalGPUReq > gpuCount {
			gpuShareFactor = clamp01(gpuCount / totalGPUReq)
		}
		var finished []*simJob
		for _, j := range jobs {
			if j.Gang && j.WorkloadID != "" {
				total := len(s.workload.jobsByWorkload[j.WorkloadID])
				if total > 0 && submittedByWorkload[j.WorkloadID] < total {
					continue
				}
				if total > 0 && runningByWorkload[j.WorkloadID]+completedByWorkload[j.WorkloadID] < total {
					continue
				}
			}
			jobThermalThrottle := snapThermalThrottle
			jobCPUMul := cpuThroughputMultiplier(freqScale, j.CPUWorkClass, model, j.MemoryIntensity, j.IOIntensity, jobThermalThrottle)
			cpuSpeed, gpuSpeed := legacyJobRates(j, model, s.workload.baseSpeedCore, jobCPUMul, cpuShareFactor, gpuShareFactor, snapGPUCapPerGpu, snapThermalThrottle)
			if j.CPUUnitsRemaining > 0 {
				prev := j.CPUUnitsRemaining
				j.CPUUnitsRemaining -= cpuSpeed * dt
				if j.CPUUnitsRemaining < prev {
					j.LastProgressAt = now
				}
			}
			if j.GPUUnitsRemaining > 0 {
				prev := j.GPUUnitsRemaining
				j.GPUUnitsRemaining -= gpuSpeed * dt
				if j.GPUUnitsRemaining < prev {
					j.LastProgressAt = now
				}
			}
			if j.CPUUnitsRemaining > 0 || j.GPUUnitsRemaining > 0 {
				continue
			}
			j.Completed = true
			j.CompletedAt = now
			finished = append(finished, j)
			s.jobCompleted.WithLabelValues(j.Class, node).Inc()
			if !j.SubmittedAt.IsZero() {
				s.jobCompletion.Observe(j.CompletedAt.Sub(j.SubmittedAt).Seconds())
			}
			log.Printf("job completed id=%s node=%s class=%s elapsed=%.1fs", j.JobID, node, j.Class, j.CompletedAt.Sub(j.SubmittedAt).Seconds())
		}
		// A node whose last job ended in this tick runs no load from now
		// on, not from the next tick, and before the jobs' pods are deleted
		// through the API, whose calls take time: a pod poll after the
		// deletion counts no pod on the node.
		active := false
		for _, j := range jobs {
			if !j.Completed {
				active = true
				break
			}
		}
		if !active {
			s.mu.Lock()
			setNoJobLoad(st)
			s.mu.Unlock()
		}
		for _, j := range finished {
			s.ensureWorkloadPodDeleted(kube, j)
		}
	}

}

// legacyJobRates is legacy job j's CPU and GPU progress, in work units per
// second, on a node of model whose CPUs run at cpuMul of full speed and whose
// GPUs are capped at gpuCapPerGPU watts each, with the node's CPU and GPU
// shares and thermal throttle: workload.Rates of ModelLegacy. The GPU
// throughput comes from the capped board model only while j has GPU work
// left; the GPU rate is 0 otherwise and must not be applied.
func legacyJobRates(j *simJob, model simModel, baseSpeed, cpuMul, cpuShare, gpuShare, gpuCapPerGPU, thermalThrottle float64) (cpuRate, gpuRate float64) {
	gpuMul := 0.0
	if j.GPUUnitsRemaining > 0 {
		board := phys.CappedBoardGPUModel{
			IdleW:         model.GPU.IdleWattsPerGPU,
			MaxW:          model.GPU.MaxWattsPerGPU,
			ComputeGamma:  model.GPU.ComputeGamma,
			MemoryEpsilon: model.GPU.MemoryEpsilon,
			MemoryGamma:   model.GPU.MemoryGamma,
		}
		gpuMul = board.ThroughputMultiplier(phys.DeviceState{
			Utilization:      clamp01(j.GPUUtilTarget),
			CapWatts:         gpuCapPerGPU,
			MaxCapWatts:      model.GPU.MaxWattsPerGPU,
			MemoryIntensity:  j.MemoryIntensity,
			CPUFeedIntensity: j.CPUFeedIntensity,
			ThermalThrottle:  thermalThrottle,
			Class:            j.GPUWorkClass,
		}, j.GPUWorkClass)
	}
	return workload.Rates(workload.ModelLegacy, legacyRateInputs(j, baseSpeed, cpuMul, gpuMul, cpuShare, gpuShare))
}

// legacyRateInputs are the workload.RateInputs of legacy job j on a node
// whose CPUs and GPUs run at cpuMul and gpuMul of full speed, for
// workload.Rates(workload.ModelLegacy, ...).
func legacyRateInputs(j *simJob, baseSpeed, cpuMul, gpuMul, cpuShare, gpuShare float64) workload.RateInputs {
	return workload.RateInputs{
		BaseSpeed: baseSpeed, CPURequest: j.RequestedCPUCores, GPURequest: j.RequestedGPUs,
		CPUUtil: j.CPUUtilTarget, MemoryIntensity: j.MemoryIntensity, IOIntensity: j.IOIntensity,
		CPUFeedIntensity: j.CPUFeedIntensity, SensitivityCPU: j.SensitivityCPU, SensitivityGPU: j.SensitivityGPU,
		CPUMul: cpuMul, GPUMul: gpuMul,
		CPUShare: cpuShare, GPUShare: gpuShare,
	}
}

// detectStalledJobs force-completes jobs that haven't made progress for
// SIM_WORKLOAD_STALL_FORCE_COMPLETE of wall-clock time (120 s by default; -1
// disables it). This prevents individual stuck jobs (e.g., gang scheduling
// deadlocks, unschedulable pods) from blocking the entire benchmark run.
// Called via defer so it runs even if pod listing fails.
func (s *simulator) detectStalledJobs(kube kubernetes.Interface, now time.Time) {
	if s.workload == nil {
		return
	}
	stallTimeout, enabled := s.workload.opts.stallForceComplete()
	if !enabled {
		return
	}
	stallTimeoutSec := stallTimeout.Seconds()
	for _, j := range s.workload.jobs {
		if j.Completed || !j.Submitted {
			continue
		}
		if j.LastProgressAt.IsZero() {
			if !j.SubmittedAt.IsZero() && now.Sub(j.SubmittedAt).Seconds() > stallTimeoutSec {
				log.Printf("stall detected: force-completing job id=%s (no progress since submission %.0fs ago)", j.JobID, now.Sub(j.SubmittedAt).Seconds())
				j.CPUUnitsRemaining = 0
				j.GPUUnitsRemaining = 0
				j.Completed = true
				j.CompletedAt = now
				s.ensureWorkloadPodDeleted(kube, j)
			}
			continue
		}
		if now.Sub(j.LastProgressAt).Seconds() > stallTimeoutSec {
			log.Printf("stall detected: force-completing job id=%s (no progress for %.0fs)", j.JobID, now.Sub(j.LastProgressAt).Seconds())
			j.CPUUnitsRemaining = 0
			j.GPUUnitsRemaining = 0
			j.Completed = true
			j.CompletedAt = now
			s.ensureWorkloadPodDeleted(kube, j)
		}
	}
}

func (s *simulator) ensureWorkloadPodDeleted(kube kubernetes.Interface, j *simJob) {
	if j == nil || j.Namespace == "" || j.PodName == "" {
		return
	}
	if !j.DeleteRequestedAt.IsZero() && time.Since(j.DeleteRequestedAt) < 2*time.Second {
		return
	}
	grace := int64(0)
	propagation := metav1.DeletePropagationBackground
	deleteCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := kube.CoreV1().Pods(j.Namespace).Delete(deleteCtx, j.PodName, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
		PropagationPolicy:  &propagation,
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		j.DeleteRequestedAt = time.Now().UTC()
		log.Printf("warning: delete completed workload pod job=%s namespace=%s pod=%s: %v", j.JobID, j.Namespace, j.PodName, err)
		return
	}
	j.DeleteRequestedAt = time.Now().UTC()
	log.Printf("requested delete for completed workload pod job=%s namespace=%s pod=%s", j.JobID, j.Namespace, j.PodName)
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

// parseMemoryRequestMiB parses a Kubernetes memory quantity string (e.g. "4Gi", "512Mi")
// and returns the value in MiB. Returns 0 on failure (caller should use default).
func parseMemoryRequestMiB(v string) float64 {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return 0
	}
	// Value() returns bytes
	bytes := q.Value()
	if bytes <= 0 {
		return 0
	}
	return float64(bytes) / (1024 * 1024)
}

// formatMemoryMiB returns a Kubernetes memory quantity string.
// Uses the parsed MiB value, falling back to 64Mi if unset.
func formatMemoryMiB(mib float64) string {
	if mib <= 0 {
		return "64Mi"
	}
	if mib >= 1024 && int(mib)%1024 == 0 {
		return fmt.Sprintf("%dGi", int(mib)/1024)
	}
	return fmt.Sprintf("%dMi", int(mib))
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

// boolEnvWords are the spellings a boolean variable takes, in any case.
var boolEnvWords = map[string]bool{
	"true": true, "1": true, "yes": true, "on": true,
	"false": false, "0": false, "no": false, "off": false,
}

// boolEnvWarned holds the values already warned about, by variable.
var boolEnvWarned sync.Map

// boolEnv reads a boolean variable: true, 1, yes or on, false, 0, no or off,
// in any case. Unset or empty gives def; any other value gives def with one
// warning naming the variable, so a typo never flips a switch.
func boolEnv(key string, def bool) bool {
	return boolEnvFrom(os.Getenv, key, def)
}

// boolEnvFrom is boolEnv over get instead of the process environment.
func boolEnvFrom(get func(string) string, key string, def bool) bool {
	raw := strings.TrimSpace(get(key))
	if raw == "" {
		return def
	}
	if v, ok := boolEnvWords[strings.ToLower(raw)]; ok {
		return v
	}
	if _, seen := boolEnvWarned.LoadOrStore(key+"="+raw, true); !seen {
		log.Printf("warning: %s=%q is not true, 1, yes, on, false, 0, no or off; using %t", key, raw, def)
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
