package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	profileNodeGVR    = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "nodepowerprofiles"}
	telemetryNodeGVR  = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "telemetryprofiles"}
	defaultHTTPTimout = 2 * time.Second

	registerMetricsOnce sync.Once
	backendModeMetric   *prometheus.GaugeVec
	policyCapMetric     *prometheus.GaugeVec
	raplEnergyMetric    *prometheus.GaugeVec
	raplPowerMetric     *prometheus.GaugeVec
	raplTotalMetric     *prometheus.GaugeVec
	dvfsObservedMetric  *prometheus.GaugeVec
	dvfsEMAMetric       *prometheus.GaugeVec
	dvfsThrottleMetric  *prometheus.GaugeVec
	dvfsTripAboveMetric *prometheus.GaugeVec
	dvfsTripBelowMetric *prometheus.GaugeVec
	dvfsCurFreqMetric   *prometheus.GaugeVec
	dvfsMaxFreqMetric   *prometheus.GaugeVec
	dvfsActionsMetric   *prometheus.CounterVec
	reconcileErrMetric  *prometheus.CounterVec
)

type HardwareInfo struct {
	CPUVendor  string
	GPUVendors []string
}

type DesiredState struct {
	Name          string
	PowerWatts    *float64
	PowerPctOfMax *float64
	GPU           *GPUPowerCap
}

type NodePowerProfile struct {
	Name          string
	NodeName      string
	Profile       string
	PowerWatts    *float64
	PowerPctOfMax *float64
	GPU           *GPUPowerCap
	PolicyName    string
}

type GPUPowerCap struct {
	Scope          string
	CapWattsPerGPU *float64
	CapPctOfMax    *float64
}

type TelemetryConfig struct {
	Name                     string
	NodeName                 string
	TargetScope              string
	CPUSourceType            string
	HTTPEndpoint             string
	TimeoutSeconds           int
	CPUControlType           string
	ControlHTTPEndpoint      string
	ControlTimeoutSeconds    int
	ControlMode              string
	GPUControlType           string
	GPUControlHTTPEndpoint   string
	GPUControlTimeoutSeconds int
	GPUControlMode           string
}

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type OSCommandRunner struct{}

func (OSCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

var commandRunner CommandRunner = OSCommandRunner{}

type GPUDevice struct {
	Index           int
	Vendor          string
	Product         string
	PowerWatts      float64
	CurrentCapWatts float64
	MinCapWatts     float64
	MaxCapWatts     float64
}

type GPUTelemetry struct {
	Vendor                 string
	DeviceCount            int
	PowerWattsTotal        float64
	CapWattsPerGPUObserved float64
}

type DVFSCpu struct {
	Index   int
	MaxFile string
	CurFile string
	MinKHz  int64
	MaxKHz  int64
}

type energySample struct {
	LastUJ   int64
	LastTime time.Time
	RangeUJ  int64
}

type AgentMetrics struct {
	node string

	backendMode          *prometheus.GaugeVec
	policyCapWatts       *prometheus.GaugeVec
	raplEnergyUJ         *prometheus.GaugeVec
	raplPowerWatts       *prometheus.GaugeVec
	raplPackageTotalW    *prometheus.GaugeVec
	dvfsObservedPowerW   *prometheus.GaugeVec
	dvfsEMAPowerW        *prometheus.GaugeVec
	dvfsThrottlePct      *prometheus.GaugeVec
	dvfsTripAbove        *prometheus.GaugeVec
	dvfsTripBelow        *prometheus.GaugeVec
	dvfsCPUCurFreqKHz    *prometheus.GaugeVec
	dvfsCPUMaxFreqKHz    *prometheus.GaugeVec
	dvfsActionsTotal     *prometheus.CounterVec
	reconcileErrorsTotal *prometheus.CounterVec
}

type NodeController struct {
	nodeName     string
	metrics      *AgentMetrics
	dvfs         *DVFSController
	simulateOnly bool
	lastRaplKey  string
}

type DVFSController struct {
	cpus []DVFSCpu

	samples map[string]energySample

	emaAlpha    float64
	emaPowerW   float64
	emaInit     bool
	highMarginW float64
	lowMarginW  float64

	stepPct     int
	throttlePct int
	minFreqKHz  int64

	cooldown   time.Duration
	lastAction time.Time
	aboveCount int
	belowCount int
	tripCount  int

	warned  bool
	metrics *AgentMetrics

	powerReader PowerReader
}

type PowerReader interface {
	ReadPowerWatts() (float64, bool, error)
}

type HTTPPowerReader struct {
	endpoint string
	nodeName string
	client   *http.Client
}

type HTTPControlClient struct {
	endpoint string
	nodeName string
	client   *http.Client
}

func newAgentMetrics(node string) *AgentMetrics {
	registerMetricsOnce.Do(func() {
		backendModeMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_backend_mode",
			Help: "Current backend mode (1 active) per node and mode",
		}, []string{"node", "mode"})
		policyCapMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_policy_cap_watts",
			Help: "Current policy cap watts selected by the agent",
		}, []string{"node", "policy"})
		raplEnergyMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_rapl_energy_uj",
			Help: "Latest RAPL energy reading in microjoules",
		}, []string{"node", "zone"})
		raplPowerMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_rapl_estimated_power_watts",
			Help: "Estimated per-zone RAPL power in watts",
		}, []string{"node", "zone"})
		raplTotalMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_rapl_package_total_power_watts",
			Help: "Estimated total package power watts (sum of package zones)",
		}, []string{"node"})
		dvfsObservedMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_observed_power_watts",
			Help: "Observed package power used by DVFS controller",
		}, []string{"node"})
		dvfsEMAMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_ema_power_watts",
			Help: "EMA package power used by DVFS controller",
		}, []string{"node"})
		dvfsThrottleMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_throttle_pct",
			Help: "Current DVFS throttle percent",
		}, []string{"node"})
		dvfsTripAboveMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_above_trip_count",
			Help: "Consecutive above-threshold samples",
		}, []string{"node"})
		dvfsTripBelowMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_below_trip_count",
			Help: "Consecutive below-threshold samples",
		}, []string{"node"})
		dvfsCurFreqMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_cpu_cur_freq_khz",
			Help: "Current CPU/policy frequency in kHz",
		}, []string{"node", "cpu"})
		dvfsMaxFreqMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_cpu_max_freq_khz",
			Help: "Current CPU/policy max frequency cap in kHz",
		}, []string{"node", "cpu"})
		dvfsActionsMetric = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_dvfs_actions_total",
			Help: "Total number of DVFS control actions",
		}, []string{"node", "action"})
		reconcileErrMetric = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_reconcile_errors_total",
			Help: "Total reconcile errors",
		}, []string{"node"})

		prometheus.MustRegister(
			backendModeMetric, policyCapMetric, raplEnergyMetric, raplPowerMetric, raplTotalMetric,
			dvfsObservedMetric, dvfsEMAMetric, dvfsThrottleMetric, dvfsTripAboveMetric, dvfsTripBelowMetric,
			dvfsCurFreqMetric, dvfsMaxFreqMetric, dvfsActionsMetric, reconcileErrMetric,
		)
	})

	m := &AgentMetrics{
		node:                 node,
		backendMode:          backendModeMetric,
		policyCapWatts:       policyCapMetric,
		raplEnergyUJ:         raplEnergyMetric,
		raplPowerWatts:       raplPowerMetric,
		raplPackageTotalW:    raplTotalMetric,
		dvfsObservedPowerW:   dvfsObservedMetric,
		dvfsEMAPowerW:        dvfsEMAMetric,
		dvfsThrottlePct:      dvfsThrottleMetric,
		dvfsTripAbove:        dvfsTripAboveMetric,
		dvfsTripBelow:        dvfsTripBelowMetric,
		dvfsCPUCurFreqKHz:    dvfsCurFreqMetric,
		dvfsCPUMaxFreqKHz:    dvfsMaxFreqMetric,
		dvfsActionsTotal:     dvfsActionsMetric,
		reconcileErrorsTotal: reconcileErrMetric,
	}
	m.setBackendMode("none")
	return m
}

func (m *AgentMetrics) setBackendMode(mode string) {
	m.backendMode.WithLabelValues(m.node, "none").Set(0)
	m.backendMode.WithLabelValues(m.node, "rapl").Set(0)
	m.backendMode.WithLabelValues(m.node, "dvfs").Set(0)
	m.backendMode.WithLabelValues(m.node, mode).Set(1)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[joulie-agent] ")

	mode := strings.ToLower(strings.TrimSpace(envOrDefault("AGENT_MODE", "daemonset")))
	startMetricsServer()
	switch mode {
	case "daemonset":
		runDaemonsetMode()
	case "pool":
		runPoolMode()
	default:
		log.Fatalf("unsupported AGENT_MODE=%q (expected daemonset|pool)", mode)
	}
}

func runDaemonsetMode() {
	nodeName := strings.TrimSpace(os.Getenv("NODE_NAME"))
	if nodeName == "" {
		log.Fatal("NODE_NAME env var is required in daemonset mode")
	}
	reconcileEvery := durationEnv("RECONCILE_INTERVAL", 20*time.Second)
	simulateOnly := strings.EqualFold(strings.TrimSpace(os.Getenv("SIMULATE_ONLY")), "true")
	controller, err := newNodeController(nodeName, simulateOnly)
	if err != nil {
		log.Fatalf("init controller: %v", err)
	}
	kube, dyn := initKubeClients()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := controller.reconcile(ctx, kube, dyn); err != nil {
			controller.metrics.reconcileErrorsTotal.WithLabelValues(nodeName).Inc()
			log.Printf("reconcile failed node=%s: %v", nodeName, err)
		}
		cancel()
		time.Sleep(reconcileEvery)
	}
}

func runPoolMode() {
	reconcileEvery := durationEnv("RECONCILE_INTERVAL", 20*time.Second)
	simulateOnly := strings.EqualFold(strings.TrimSpace(os.Getenv("SIMULATE_ONLY")), "true")
	selectorExpr := envOrDefault("POOL_NODE_SELECTOR", "joulie.io/managed=true")
	shards := intEnv("POOL_SHARDS", 1)
	shardID := resolvePoolShardID()
	if shards <= 0 {
		log.Fatalf("invalid POOL_SHARDS=%d", shards)
	}
	if shardID < 0 || shardID >= shards {
		log.Fatalf("invalid POOL_SHARD_ID=%d for POOL_SHARDS=%d", shardID, shards)
	}
	selector, err := labels.Parse(selectorExpr)
	if err != nil {
		log.Fatalf("invalid POOL_NODE_SELECTOR=%q: %v", selectorExpr, err)
	}
	kube, dyn := initKubeClients()
	controllers := map[string]*NodeController{}
	log.Printf("pool mode enabled selector=%q shards=%d shardID=%d interval=%s", selectorExpr, shards, shardID, reconcileEvery)

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			cancel()
			log.Printf("pool list nodes failed: %v", err)
			time.Sleep(reconcileEvery)
			continue
		}
		active := map[string]bool{}
		for _, n := range nodes.Items {
			if !selector.Matches(labels.Set(n.Labels)) {
				continue
			}
			if !ownsNodeForShard(n.Name, shards, shardID) {
				continue
			}
			active[n.Name] = true
			c := controllers[n.Name]
			if c == nil {
				c, err = newNodeController(n.Name, simulateOnly)
				if err != nil {
					log.Printf("failed to init controller node=%s: %v", n.Name, err)
					continue
				}
				controllers[n.Name] = c
				log.Printf("controller started node=%s shard=%d/%d", n.Name, shardID, shards)
			}
			if err := c.reconcile(ctx, kube, dyn); err != nil {
				c.metrics.reconcileErrorsTotal.WithLabelValues(c.nodeName).Inc()
				log.Printf("reconcile failed node=%s: %v", c.nodeName, err)
			}
		}
		for node := range controllers {
			if active[node] {
				continue
			}
			delete(controllers, node)
			log.Printf("controller stopped node=%s shard=%d/%d", node, shardID, shards)
		}
		cancel()
		time.Sleep(reconcileEvery)
	}
}

func resolvePoolShardID() int {
	if raw := strings.TrimSpace(os.Getenv("POOL_SHARD_ID")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			return v
		}
	}
	podName := strings.TrimSpace(os.Getenv("POD_NAME"))
	if podName == "" {
		return 0
	}
	lastDash := strings.LastIndex(podName, "-")
	if lastDash < 0 || lastDash+1 >= len(podName) {
		return 0
	}
	v, err := strconv.Atoi(podName[lastDash+1:])
	if err != nil {
		return 0
	}
	return v
}

func initKubeClients() (kubernetes.Interface, dynamic.Interface) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("in-cluster config: %v", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("dynamic client: %v", err)
	}
	return kube, dyn
}

func newNodeController(nodeName string, simulateOnly bool) (*NodeController, error) {
	metrics := newAgentMetrics(nodeName)
	dvfs, err := newDVFSControllerFromEnv(metrics)
	if err != nil {
		log.Printf("warning: DVFS controller disabled node=%s: %v", nodeName, err)
		dvfs = nil
	}
	return &NodeController{
		nodeName:     nodeName,
		metrics:      metrics,
		dvfs:         dvfs,
		simulateOnly: simulateOnly,
	}, nil
}

func (n *NodeController) reconcile(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface) error {
	return reconcileOnce(ctx, kube, dyn, n.nodeName, n.dvfs, n.metrics, n.simulateOnly, &n.lastRaplKey)
}

func ownsNodeForShard(nodeName string, shards, shardID int) bool {
	if shards <= 1 {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(nodeName))
	return int(h.Sum32()%uint32(shards)) == shardID
}

func startMetricsServer() {
	addr := strings.TrimSpace(os.Getenv("METRICS_ADDR"))
	if addr == "" {
		addr = ":8080"
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Printf("metrics server listening on %s", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("warning: metrics server stopped: %v", err)
		}
	}()
}

func reconcileOnce(
	ctx context.Context,
	kube kubernetes.Interface,
	dyn dynamic.Interface,
	nodeName string,
	dvfs *DVFSController,
	metrics *AgentMetrics,
	simulateOnly bool,
	lastRaplKey *string,
) error {
	node, err := kube.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node: %w", err)
	}

	hw := discoverHardware(node.Labels)

	selected, source, err := resolveDesiredStateForNode(ctx, dyn, nodeName)
	if err != nil {
		return err
	}

	telemetry, err := resolveTelemetryConfigForNode(ctx, dyn, nodeName)
	if err != nil {
		return err
	}
	controlClient := controlClientFromTelemetry(telemetry, nodeName)
	gpuControlClient := gpuControlClientFromTelemetry(telemetry, nodeName)
	_ = updateTelemetryHardwareStatus(ctx, dyn, telemetry, nodeName, node.Labels, hw)
	if dvfs != nil {
		dvfs.SetPowerReaderForTelemetry(telemetry, nodeName)
	}

	if selected == nil {
		metrics.setBackendMode("none")
		_ = updateTelemetryControlStatus(ctx, dyn, telemetry, nodeName, "none", "none", "no NodePowerProfile selected")
		_ = updateTelemetryGPUStatus(ctx, dyn, telemetry, nodeName, "none", "none", "no NodePowerProfile selected", nil)
		if *lastRaplKey != "" {
			log.Printf("no NodePowerProfile found for node %s; leaving current settings untouched", nodeName)
			*lastRaplKey = ""
		}
		return nil
	}

	cpuCapWatts := selected.PowerWatts
	if cpuCapWatts == nil && selected.PowerPctOfMax != nil {
		if resolved, msg, ok := resolveCPUCapFromPct(ctx, *selected.PowerPctOfMax); ok {
			cpuCapWatts = &resolved
			log.Printf("resolved cpu cap from pct node=%s pct=%.2f%% cap=%.2fW (%s)", nodeName, *selected.PowerPctOfMax, resolved, msg)
		} else {
			log.Printf("warning: unable to resolve cpu cap from pct node=%s pct=%.2f%% (%s)", nodeName, *selected.PowerPctOfMax, msg)
		}
	}

	if cpuCapWatts != nil {
		log.Printf("desired state source=%s name=%s node=%s cap=%.2fW", source, selected.Name, nodeName, *cpuCapWatts)
		metrics.policyCapWatts.WithLabelValues(nodeName, selected.Name).Set(*cpuCapWatts)
	} else if selected.PowerPctOfMax != nil {
		log.Printf("desired state source=%s name=%s node=%s cpu-cap-pct=%.2f", source, selected.Name, nodeName, *selected.PowerPctOfMax)
	} else {
		log.Printf("desired state source=%s name=%s node=%s cpu-cap=none", source, selected.Name, nodeName)
	}

	if simulateOnly {
		metrics.setBackendMode("none")
		_ = updateTelemetryControlStatus(ctx, dyn, telemetry, nodeName, "none", "applied", "simulate-only mode")
		if selected.GPU != nil {
			_ = updateTelemetryGPUStatus(ctx, dyn, telemetry, nodeName, "none", "applied", "simulate-only mode", nil)
		} else {
			_ = updateTelemetryGPUStatus(ctx, dyn, telemetry, nodeName, "none", "none", "no GPU cap intent", nil)
		}
		if cpuCapWatts != nil {
			log.Printf("simulate-only mode: would apply policy=%s cap=%.2fW on node=%s", selected.Name, *cpuCapWatts, nodeName)
		}
		return nil
	}

	if selected.GPU != nil {
		backend, result, msg, observed, err := applyGPUIntent(ctx, node, selected.GPU, gpuControlClient)
		_ = updateTelemetryGPUStatus(ctx, dyn, telemetry, nodeName, backend, result, msg, observed)
		if err != nil {
			return err
		}
	} else {
		msg := "no GPU cap intent"
		if allocatableGPUCount(node) > 0 {
			msg = "no GPU cap intent for GPU node; leaving current GPU state unchanged"
			log.Printf("warning: node=%s has allocatable GPUs but NodePowerProfile has no gpu.powerCap; leaving GPU state unchanged", nodeName)
		}
		_ = updateTelemetryGPUStatus(ctx, dyn, telemetry, nodeName, "none", "none", msg, nil)
	}

	if cpuCapWatts == nil {
		if selected.PowerPctOfMax != nil {
			backend, result, msg, err := applyCPUPercentIntent(*selected.PowerPctOfMax, controlClient, dvfs, metrics)
			_ = updateTelemetryControlStatus(ctx, dyn, telemetry, nodeName, backend, result, msg)
			if err != nil {
				return err
			}
		}
		return nil
	}

	preferDVFS := telemetry != nil && telemetry.CPUControlType == "http" && telemetry.ControlMode == "dvfs"
	appliedRapl := false
	raplFiles := 0
	if !preferDVFS {
		if controlClient != nil {
			if err := controlClient.ApplyCPUControl("rapl.set_power_cap_watts", *cpuCapWatts, -1); err == nil {
				appliedRapl = true
				raplFiles = 1
			} else {
				log.Printf("warning: HTTP RAPL control failed for node=%s: %v (trying fallback backend)", nodeName, err)
			}
		} else {
			appliedRapl, raplFiles, err = applyRAPLPackageCap(hw, *cpuCapWatts)
			if err != nil {
				return err
			}
		}
	}
	if appliedRapl {
		metrics.setBackendMode("rapl")
		_ = updateTelemetryControlStatus(ctx, dyn, telemetry, nodeName, "rapl", "applied", "RAPL package cap applied")
		key := fmt.Sprintf("%s|rapl|%.2f", selected.Name, *cpuCapWatts)
		if key != *lastRaplKey {
			log.Printf("applied policy=%s backend=rapl cap=%.2fW files=%d cpuVendor=%s", selected.Name, *cpuCapWatts, raplFiles, hw.CPUVendor)
			*lastRaplKey = key
		}
		if dvfs != nil {
			dvfs.warned = false
		}
		if dvfs != nil && dvfs.Active() {
			if restored, rerr := dvfs.RestoreAllMax(); rerr != nil {
				log.Printf("warning: could not fully restore DVFS fallback state after RAPL became available: %v", rerr)
			} else if restored > 0 {
				log.Printf("restored %d cpufreq entries to cpuinfo_max after switching back to RAPL", restored)
			}
		}
		return nil
	}

	*lastRaplKey = ""
	metrics.setBackendMode("dvfs")
	if dvfs == nil {
		_ = updateTelemetryControlStatus(ctx, dyn, telemetry, nodeName, "none", "blocked", "RAPL unavailable and DVFS disabled")
		log.Printf("warning: no enforce backend available for node=%s policy=%s (RAPL unavailable and DVFS disabled); recording desired state only", nodeName, selected.Name)
		return nil
	}
	if controlClient == nil && !dvfs.HasHostControl() {
		_ = updateTelemetryControlStatus(ctx, dyn, telemetry, nodeName, "none", "blocked", "RAPL unavailable, no cpufreq host control, and no HTTP control backend")
		log.Printf("warning: no enforce backend available for node=%s policy=%s (RAPL unavailable, no cpufreq host control, no HTTP control backend); recording desired state only", nodeName, selected.Name)
		return nil
	}
	if raplFiles == 0 && !dvfs.warned {
		log.Printf("warning: RAPL power-limit files not available on node %s (vendor=%s); using DVFS fallback controller", nodeName, hw.CPUVendor)
		dvfs.warned = true
	}
	action, err := dvfs.Reconcile(*cpuCapWatts, controlClient)
	if err != nil {
		_ = updateTelemetryControlStatus(ctx, dyn, telemetry, nodeName, "dvfs", "error", err.Error())
		return fmt.Errorf("dvfs fallback failed: %w", err)
	}
	_ = updateTelemetryControlStatus(ctx, dyn, telemetry, nodeName, "dvfs", "applied", action)
	if action != "" {
		log.Printf("dvfs-control node=%s policy=%s cap=%.2fW %s", nodeName, selected.Name, *cpuCapWatts, action)
	}
	return nil
}

func applyRAPLPackageCap(hw HardwareInfo, watts float64) (bool, int, error) {
	if hw.CPUVendor != "AuthenticAMD" && hw.CPUVendor != "GenuineIntel" {
		return false, 0, nil
	}
	if watts <= 0 {
		return false, 0, fmt.Errorf("power cap watts must be > 0")
	}
	files, err := raplCapFiles()
	if err != nil {
		return false, 0, err
	}
	if len(files) == 0 {
		return false, 0, nil
	}

	uw := int64(watts * 1_000_000)
	payload := []byte(strconv.FormatInt(uw, 10))
	count := 0
	for _, f := range files {
		if err := os.WriteFile(f, payload, 0); err != nil {
			return false, count, fmt.Errorf("write %s: %w", f, err)
		}
		count++
	}
	return true, count, nil
}

func resolveCPUCapFromPct(ctx context.Context, pct float64) (float64, string, bool) {
	if pct <= 0 {
		return 0, "pct must be > 0", false
	}
	maxW, minW, ok := readRAPLPackageCapRangeWatts()
	if !ok || maxW <= 0 {
		return 0, "rapl cap range unavailable", false
	}
	target := (pct / 100.0) * maxW
	if target < minW {
		target = minW
	}
	if target > maxW {
		target = maxW
	}
	_ = ctx
	return target, fmt.Sprintf("min=%.2fW max=%.2fW", minW, maxW), true
}

func applyCPUPercentIntent(
	pct float64,
	controlClient *HTTPControlClient,
	dvfs *DVFSController,
	metrics *AgentMetrics,
) (string, string, string, error) {
	if pct <= 0 {
		return "none", "blocked", "cpu cap pct must be > 0", nil
	}
	if pct > 100 {
		pct = 100
	}
	throttlePct := int(math.Round(100.0 - pct))

	targetWatts := 0.0
	if dvfs != nil {
		if observed, ok, err := dvfs.readPowerWatts(); err != nil {
			log.Printf("warning: unable to read telemetry power for pct intent: %v", err)
		} else if ok {
			targetWatts = (pct / 100.0) * observed
		}
	}

	msg := fmt.Sprintf("applied cpu cap pct=%.2f%% via dvfs throttle=%d%%", pct, throttlePct)
	if targetWatts > 0 {
		msg = fmt.Sprintf("%s target=%.2fW", msg, targetWatts)
	}

	if controlClient != nil {
		if err := controlClient.ApplyCPUControl("dvfs.set_throttle_pct", targetWatts, throttlePct); err != nil {
			log.Printf("warning: HTTP dvfs control failed for pct intent: %v", err)
			if dvfs == nil || !dvfs.HasHostControl() {
				return "none", "error", fmt.Sprintf("dvfs http control failed: %v", err), err
			}
		} else {
			if metrics != nil {
				metrics.setBackendMode("dvfs")
			}
			if dvfs != nil {
				dvfs.throttlePct = throttlePct
				if dvfs.metrics != nil {
					dvfs.metrics.dvfsThrottlePct.WithLabelValues(dvfs.metrics.node).Set(float64(throttlePct))
				}
			}
			return "dvfs", "applied", msg, nil
		}
	}

	if dvfs == nil || !dvfs.HasHostControl() {
		return "none", "blocked", "pct intent unresolved and no dvfs backend available", nil
	}
	written, err := dvfs.applyThrottlePct(throttlePct, nil, targetWatts)
	if err != nil {
		return "dvfs", "error", err.Error(), err
	}
	dvfs.throttlePct = throttlePct
	if dvfs.metrics != nil {
		dvfs.metrics.dvfsThrottlePct.WithLabelValues(dvfs.metrics.node).Set(float64(throttlePct))
	}
	dvfs.updateCPUFreqMetrics()
	if metrics != nil {
		metrics.setBackendMode("dvfs")
	}
	return "dvfs", "applied", fmt.Sprintf("%s cpus=%d", msg, written), nil
}

func readRAPLPackageCapRangeWatts() (maxW float64, minW float64, ok bool) {
	maxPaths, _ := filepath.Glob("/host-sys/class/powercap/intel-rapl:*/constraint_0_max_power_uw")
	minPaths, _ := filepath.Glob("/host-sys/class/powercap/intel-rapl:*/constraint_0_min_power_uw")
	maxVals := []float64{}
	minVals := []float64{}
	for _, p := range maxPaths {
		if v, err := readInt64(p); err == nil && v > 0 {
			maxVals = append(maxVals, float64(v)/1_000_000.0)
		}
	}
	for _, p := range minPaths {
		if v, err := readInt64(p); err == nil && v > 0 {
			minVals = append(minVals, float64(v)/1_000_000.0)
		}
	}
	if len(maxVals) == 0 {
		return 0, 0, false
	}
	maxW = maxVals[0]
	for _, v := range maxVals {
		if v < maxW {
			maxW = v
		}
	}
	if len(minVals) > 0 {
		minW = minVals[0]
		for _, v := range minVals {
			if v > minW {
				minW = v
			}
		}
	}
	return maxW, minW, true
}

func applyGPUIntent(
	ctx context.Context,
	node *corev1.Node,
	intent *GPUPowerCap,
	httpClient *HTTPControlClient,
) (string, string, string, map[string]any, error) {
	if intent == nil {
		return "none", "none", "no GPU cap intent", nil, nil
	}
	nodeName := ""
	nodeLabels := map[string]string{}
	if node != nil {
		nodeName = node.Name
		nodeLabels = node.Labels
	}
	if allocatableGPUCount(node) <= 0 {
		return "none", "blocked", "node has no allocatable GPU resources", map[string]any{
			"node":              nodeName,
			"allocatableGPUs":   0,
			"requestedGPUIntent": true,
		}, nil
	}

	// For HTTP control backends (e.g., simulator), absolute caps can be sent
	// without host GPU inventory/driver tools on the agent container.
	if httpClient != nil && intent.CapWattsPerGPU != nil && *intent.CapWattsPerGPU > 0 {
		capPerGPU := *intent.CapWattsPerGPU
		observed := map[string]any{
			"capWattsPerGpu": capPerGPU,
		}
		if err := httpClient.ApplyGPUControl("gpu.set_power_cap_watts", capPerGPU); err != nil {
			return "http", "error", err.Error(), observed, nil
		}
		return "http", "applied", fmt.Sprintf("applied gpu cap %.1fW", capPerGPU), observed, nil
	}

	vendor := detectGPUVendor(ctx, nodeLabels)
	if vendor == "none" {
		return "none", "blocked", "no supported GPU backend detected", map[string]any{"vendor": "none"}, nil
	}
	devices, err := listGPUDevices(ctx, vendor)
	if err != nil {
		return "none", "error", fmt.Sprintf("gpu inventory failed: %v", err), map[string]any{"vendor": vendor}, nil
	}
	if len(devices) == 0 {
		return "none", "blocked", "no GPU devices discovered", map[string]any{"vendor": vendor}, nil
	}
	capPerGPU, msg, ok := resolveGPUCapPerDevice(intent, devices)
	if !ok {
		return "none", "blocked", msg, map[string]any{"vendor": vendor, "deviceCount": len(devices)}, nil
	}

	observed := map[string]any{
		"vendor":         vendor,
		"deviceCount":    len(devices),
		"capWattsPerGpu": capPerGPU,
	}

	if httpClient != nil {
		if err := httpClient.ApplyGPUControl("gpu.set_power_cap_watts", capPerGPU); err != nil {
			return "http", "error", err.Error(), observed, nil
		}
		return "http", "applied", fmt.Sprintf("applied gpu cap %.1fW on %d devices", capPerGPU, len(devices)), observed, nil
	}

	switch vendor {
	case "nvidia":
		if err := applyNvidiaGPUCap(ctx, devices, capPerGPU); err != nil {
			return "host-nvidia", "error", err.Error(), observed, nil
		}
		return "host-nvidia", "applied", fmt.Sprintf("applied gpu cap %.1fW on %d devices", capPerGPU, len(devices)), observed, nil
	case "amd":
		if err := applyAmdGPUCap(ctx, devices, capPerGPU); err != nil {
			return "host-amd", "error", err.Error(), observed, nil
		}
		return "host-amd", "applied", fmt.Sprintf("applied gpu cap %.1fW on %d devices", capPerGPU, len(devices)), observed, nil
	default:
		return "none", "blocked", fmt.Sprintf("unsupported gpu vendor %q", vendor), observed, nil
	}
}

func allocatableGPUCount(node *corev1.Node) int64 {
	if node == nil {
		return 0
	}
	var total int64
	for k, q := range node.Status.Allocatable {
		key := strings.ToLower(string(k))
		if key == "nvidia.com/gpu" || key == "amd.com/gpu" || key == "gpu.intel.com/i915" || strings.HasSuffix(key, "/gpu") {
			total += q.Value()
		}
	}
	return total
}

func resolveGPUCapPerDevice(intent *GPUPowerCap, devices []GPUDevice) (float64, string, bool) {
	if intent.CapWattsPerGPU != nil && *intent.CapWattsPerGPU > 0 {
		w := *intent.CapWattsPerGPU
		for _, d := range devices {
			if d.MinCapWatts > 0 && w < d.MinCapWatts {
				return 0, fmt.Sprintf("requested cap %.1fW below min %.1fW on gpu %d", w, d.MinCapWatts, d.Index), false
			}
			if d.MaxCapWatts > 0 && w > d.MaxCapWatts {
				return 0, fmt.Sprintf("requested cap %.1fW above max %.1fW on gpu %d", w, d.MaxCapWatts, d.Index), false
			}
		}
		return w, "", true
	}
	if intent.CapPctOfMax == nil || *intent.CapPctOfMax <= 0 {
		return 0, "missing capWattsPerGpu or capPctOfMax", false
	}
	pct := *intent.CapPctOfMax
	minResolved := 0.0
	for _, d := range devices {
		if d.MaxCapWatts <= 0 {
			return 0, "cannot resolve capPctOfMax without device max power limits", false
		}
		w := (pct / 100.0) * d.MaxCapWatts
		if d.MinCapWatts > 0 && w < d.MinCapWatts {
			w = d.MinCapWatts
		}
		if minResolved == 0 || w < minResolved {
			minResolved = w
		}
	}
	if minResolved <= 0 {
		return 0, "failed to resolve cap from percentage", false
	}
	return minResolved, "", true
}

func detectGPUVendor(ctx context.Context, nodeLabels map[string]string) string {
	if hasNFDGPUVendor(nodeLabels, "10de") {
		return "nvidia"
	}
	if hasNFDGPUVendor(nodeLabels, "1002") {
		return "amd"
	}
	if _, err := os.Stat("/dev/nvidiactl"); err == nil {
		return "nvidia"
	}
	if _, err := runCommand(ctx, "nvidia-smi", "-L"); err == nil {
		return "nvidia"
	}
	if _, err := runCommand(ctx, "rocm-smi", "--showproductname"); err == nil {
		return "amd"
	}
	return "none"
}

func listGPUDevices(ctx context.Context, vendor string) ([]GPUDevice, error) {
	switch vendor {
	case "nvidia":
		return listNvidiaDevices(ctx)
	case "amd":
		return listAmdDevices(ctx)
	default:
		return nil, nil
	}
}

func listNvidiaDevices(ctx context.Context) ([]GPUDevice, error) {
	out, err := runCommand(ctx, "nvidia-smi", "--query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	devices := make([]GPUDevice, 0, len(lines))
	for _, ln := range lines {
		parts := splitCSVLine(ln)
		if len(parts) < 6 {
			continue
		}
		idx, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		minW, _ := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		maxW, _ := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
		curCap, _ := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
		pwr, _ := strconv.ParseFloat(strings.TrimSpace(parts[4]), 64)
		name := strings.TrimSpace(parts[5])
		devices = append(devices, GPUDevice{
			Index:           idx,
			Vendor:          "nvidia",
			Product:         name,
			PowerWatts:      pwr,
			CurrentCapWatts: curCap,
			MinCapWatts:     minW,
			MaxCapWatts:     maxW,
		})
	}
	return devices, nil
}

func listAmdDevices(ctx context.Context) ([]GPUDevice, error) {
	out, err := runCommand(ctx, "rocm-smi", "--showpowercap", "--showproductname", "--json")
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		// v1 fallback: no JSON support, rely on command success and unknown limits.
		return []GPUDevice{{Index: 0, Vendor: "amd"}}, nil
	}
	devices := make([]GPUDevice, 0, len(raw))
	for k, v := range raw {
		if !strings.HasPrefix(strings.ToLower(k), "card") {
			continue
		}
		idx, _ := strconv.Atoi(strings.TrimPrefix(strings.ToLower(k), "card"))
		d := GPUDevice{Index: idx, Vendor: "amd"}
		if m, ok := v.(map[string]any); ok {
			if name, ok := extractStringAny(m, "Card SKU"); ok {
				d.Product = name
			}
			if maxW, ok := extractFloatAny(m, "Max Graphics Package Power (W)"); ok {
				d.MaxCapWatts = maxW
			}
			if minW, ok := extractFloatAny(m, "Min Graphics Package Power (W)"); ok {
				d.MinCapWatts = minW
			}
			if curW, ok := extractFloatAny(m, "Current Power Cap (W)"); ok {
				d.CurrentCapWatts = curW
			}
			if pwr, ok := extractFloatAny(m, "Average Graphics Package Power (W)"); ok {
				d.PowerWatts = pwr
			}
		}
		devices = append(devices, d)
	}
	if len(devices) == 0 {
		devices = append(devices, GPUDevice{Index: 0, Vendor: "amd"})
	}
	return devices, nil
}

func applyNvidiaGPUCap(ctx context.Context, devices []GPUDevice, capWatts float64) error {
	for _, d := range devices {
		if _, err := runCommand(ctx, "nvidia-smi", "-i", strconv.Itoa(d.Index), "-pl", strconv.Itoa(int(math.Round(capWatts)))); err != nil {
			return fmt.Errorf("nvidia-smi set cap gpu=%d: %w", d.Index, err)
		}
	}
	return nil
}

func applyAmdGPUCap(ctx context.Context, devices []GPUDevice, capWatts float64) error {
	for _, d := range devices {
		if _, err := runCommand(ctx, "rocm-smi", "-d", strconv.Itoa(d.Index), "--setpoweroverdrive", strconv.Itoa(int(math.Round(capWatts)))); err != nil {
			return fmt.Errorf("rocm-smi set cap gpu=%d: %w", d.Index, err)
		}
	}
	return nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := commandRunner.Run(timeoutCtx, name, args...)
	if err != nil {
		return nil, fmt.Errorf("%s %s failed: %v (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func splitCSVLine(in string) []string {
	parts := strings.Split(in, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(strings.Trim(p, "\"")))
	}
	return out
}

func raplCapFiles() ([]string, error) {
	patterns := []string{
		"/host-sys/class/powercap/*/constraint_0_power_limit_uw",
		"/host-sys/class/powercap/*:*/constraint_0_power_limit_uw",
		"/host-sys/class/powercap/*:*:*/constraint_0_power_limit_uw",
	}
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if _, ok := seen[m]; ok {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out, nil
}

func energyFiles() ([]string, error) {
	patterns := []string{
		"/host-sys/class/powercap/*/energy_uj",
		"/host-sys/class/powercap/*:*/energy_uj",
		"/host-sys/class/powercap/*:*:*/energy_uj",
		"/host-sys/devices/virtual/powercap/intel-rapl/*/energy_uj",
		"/host-sys/devices/virtual/powercap/intel-rapl/*/*/energy_uj",
	}
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if _, ok := seen[m]; ok {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	sort.Strings(out)
	filtered := make([]string, 0, len(out))
	for _, f := range out {
		if isPackageEnergyFile(f) {
			filtered = append(filtered, f)
		}
	}
	return filtered, nil
}

func isPackageEnergyFile(path string) bool {
	zone := filepath.Base(filepath.Dir(path))
	return strings.Count(zone, ":") == 1
}

func cpufreqCPUList() ([]DVFSCpu, error) {
	matches := make([]string, 0)
	cpuMatches, err := filepath.Glob("/host-sys/devices/system/cpu/cpu*/cpufreq/scaling_max_freq")
	if err != nil {
		return nil, err
	}
	policyMatches, err := filepath.Glob("/host-sys/devices/system/cpu/cpufreq/policy*/scaling_max_freq")
	if err != nil {
		return nil, err
	}
	matches = append(matches, cpuMatches...)
	matches = append(matches, policyMatches...)

	cpus := make([]DVFSCpu, 0, len(matches))
	for _, maxf := range matches {
		dir := filepath.Dir(maxf)
		idx, ok := cpuIndexFromPath(dir)
		if !ok {
			continue
		}
		minKHz, err := readInt64(filepath.Join(dir, "cpuinfo_min_freq"))
		if err != nil {
			continue
		}
		maxKHz, err := readInt64(filepath.Join(dir, "cpuinfo_max_freq"))
		if err != nil {
			continue
		}
		cpus = append(cpus, DVFSCpu{Index: idx, MaxFile: maxf, CurFile: filepath.Join(dir, "scaling_cur_freq"), MinKHz: minKHz, MaxKHz: maxKHz})
	}
	sort.Slice(cpus, func(i, j int) bool { return cpus[i].Index < cpus[j].Index })
	return cpus, nil
}

func cpuIndexFromPath(cpufreqDir string) (int, bool) {
	base := filepath.Base(cpufreqDir)
	if strings.HasPrefix(base, "policy") {
		v, err := strconv.Atoi(strings.TrimPrefix(base, "policy"))
		if err != nil {
			return 0, false
		}
		return v, true
	}
	if strings.HasPrefix(base, "cpufreq") {
		base = filepath.Base(filepath.Dir(cpufreqDir))
	}
	if !strings.HasPrefix(base, "cpu") {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimPrefix(base, "cpu"))
	if err != nil {
		return 0, false
	}
	return v, true
}

func newDVFSControllerFromEnv(metrics *AgentMetrics) (*DVFSController, error) {
	cpus, err := cpufreqCPUList()
	if err != nil {
		return nil, err
	}
	if len(cpus) == 0 {
		log.Printf("warning: no cpufreq files found under /host-sys/devices/system/cpu; host DVFS writes disabled, HTTP control can still be used")
	}
	return &DVFSController{
		cpus:        cpus,
		samples:     map[string]energySample{},
		emaAlpha:    floatEnv("DVFS_EMA_ALPHA", 0.30),
		highMarginW: floatEnv("DVFS_HIGH_MARGIN_W", 10.0),
		lowMarginW:  floatEnv("DVFS_LOW_MARGIN_W", 15.0),
		stepPct:     intEnv("DVFS_STEP_PCT", 10),
		minFreqKHz:  int64Env("DVFS_MIN_FREQ_KHZ", 1500000),
		cooldown:    durationEnv("DVFS_COOLDOWN", 20*time.Second),
		tripCount:   intEnv("DVFS_TRIP_COUNT", 2),
		metrics:     metrics,
	}, nil
}

func (d *DVFSController) Active() bool {
	return d.throttlePct > 0
}

func (d *DVFSController) HasHostControl() bool {
	return len(d.cpus) > 0
}

func (d *DVFSController) Reconcile(capWatts float64, controlClient *HTTPControlClient) (string, error) {
	if capWatts <= 0 {
		return "", nil
	}
	powerW, hasPower, err := d.readPowerWatts()
	if err != nil {
		return "", err
	}
	if !hasPower {
		return "", nil
	}
	if !d.emaInit {
		d.emaPowerW = powerW
		d.emaInit = true
	} else {
		d.emaPowerW = d.emaAlpha*powerW + (1.0-d.emaAlpha)*d.emaPowerW
	}
	d.metrics.dvfsObservedPowerW.WithLabelValues(d.metrics.node).Set(powerW)
	d.metrics.dvfsEMAPowerW.WithLabelValues(d.metrics.node).Set(d.emaPowerW)
	d.metrics.dvfsThrottlePct.WithLabelValues(d.metrics.node).Set(float64(d.throttlePct))

	now := time.Now()
	if now.Sub(d.lastAction) < d.cooldown {
		d.metrics.dvfsTripAbove.WithLabelValues(d.metrics.node).Set(float64(d.aboveCount))
		d.metrics.dvfsTripBelow.WithLabelValues(d.metrics.node).Set(float64(d.belowCount))
		d.updateCPUFreqMetrics()
		return fmt.Sprintf("mode=dvfs-fallback observed=%.2fW ema=%.2fW throttlePct=%d action=hold(cooldown)", powerW, d.emaPowerW, d.throttlePct), nil
	}

	upper := capWatts + d.highMarginW
	lower := capWatts - d.lowMarginW
	if d.emaPowerW > upper {
		d.aboveCount++
		d.belowCount = 0
	} else if d.emaPowerW < lower {
		d.belowCount++
		d.aboveCount = 0
	} else {
		d.aboveCount = 0
		d.belowCount = 0
	}
	d.metrics.dvfsTripAbove.WithLabelValues(d.metrics.node).Set(float64(d.aboveCount))
	d.metrics.dvfsTripBelow.WithLabelValues(d.metrics.node).Set(float64(d.belowCount))

	if d.aboveCount >= d.tripCount {
		oldPct := d.throttlePct
		d.throttlePct = minInt(100, d.throttlePct+d.stepPct)
		d.aboveCount = 0
		d.lastAction = now
		written, err := d.applyThrottlePct(d.throttlePct, controlClient, capWatts)
		if err != nil {
			return "", err
		}
		d.metrics.dvfsActionsTotal.WithLabelValues(d.metrics.node, "throttle_up").Inc()
		d.metrics.dvfsThrottlePct.WithLabelValues(d.metrics.node).Set(float64(d.throttlePct))
		d.updateCPUFreqMetrics()
		return fmt.Sprintf("mode=dvfs-fallback observed=%.2fW ema=%.2fW upper=%.2fW action=throttle-up pct=%d->%d cpus=%d", powerW, d.emaPowerW, upper, oldPct, d.throttlePct, written), nil
	}

	if d.belowCount >= d.tripCount {
		oldPct := d.throttlePct
		d.throttlePct = maxInt(0, d.throttlePct-d.stepPct)
		d.belowCount = 0
		d.lastAction = now
		written, err := d.applyThrottlePct(d.throttlePct, controlClient, capWatts)
		if err != nil {
			return "", err
		}
		d.metrics.dvfsActionsTotal.WithLabelValues(d.metrics.node, "throttle_down").Inc()
		d.metrics.dvfsThrottlePct.WithLabelValues(d.metrics.node).Set(float64(d.throttlePct))
		d.updateCPUFreqMetrics()
		return fmt.Sprintf("mode=dvfs-fallback observed=%.2fW ema=%.2fW lower=%.2fW action=throttle-down pct=%d->%d cpus=%d", powerW, d.emaPowerW, lower, oldPct, d.throttlePct, written), nil
	}

	d.updateCPUFreqMetrics()
	return fmt.Sprintf("mode=dvfs-fallback observed=%.2fW ema=%.2fW throttlePct=%d action=hold", powerW, d.emaPowerW, d.throttlePct), nil
}

func (d *DVFSController) SetPowerReaderForTelemetry(cfg *TelemetryConfig, nodeName string) {
	if cfg == nil || cfg.CPUSourceType == "" || cfg.CPUSourceType == "host" {
		d.powerReader = nil
		return
	}
	if cfg.CPUSourceType != "http" {
		d.powerReader = nil
		return
	}
	if cfg.HTTPEndpoint == "" {
		d.powerReader = nil
		return
	}
	timeout := defaultHTTPTimout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	d.powerReader = &HTTPPowerReader{
		endpoint: cfg.HTTPEndpoint,
		nodeName: nodeName,
		client:   &http.Client{Timeout: timeout},
	}
}

func controlClientFromTelemetry(cfg *TelemetryConfig, nodeName string) *HTTPControlClient {
	if cfg == nil || cfg.CPUControlType != "http" || cfg.ControlHTTPEndpoint == "" {
		return nil
	}
	timeout := defaultHTTPTimout
	if cfg.ControlTimeoutSeconds > 0 {
		timeout = time.Duration(cfg.ControlTimeoutSeconds) * time.Second
	}
	return &HTTPControlClient{
		endpoint: cfg.ControlHTTPEndpoint,
		nodeName: nodeName,
		client:   &http.Client{Timeout: timeout},
	}
}

func gpuControlClientFromTelemetry(cfg *TelemetryConfig, nodeName string) *HTTPControlClient {
	if cfg == nil || cfg.GPUControlType != "http" || cfg.GPUControlHTTPEndpoint == "" {
		return nil
	}
	timeout := defaultHTTPTimout
	if cfg.GPUControlTimeoutSeconds > 0 {
		timeout = time.Duration(cfg.GPUControlTimeoutSeconds) * time.Second
	}
	return &HTTPControlClient{
		endpoint: cfg.GPUControlHTTPEndpoint,
		nodeName: nodeName,
		client:   &http.Client{Timeout: timeout},
	}
}

func (h *HTTPPowerReader) ReadPowerWatts() (float64, bool, error) {
	url := strings.ReplaceAll(h.endpoint, "{node}", h.nodeName)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, false, fmt.Errorf("telemetry endpoint status=%d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, false, err
	}
	if v, ok := extractFloat(payload, "packagePowerWatts"); ok {
		return v, true, nil
	}
	if cpuRaw, ok := payload["cpu"].(map[string]any); ok {
		if v, ok := extractFloat(cpuRaw, "packagePowerWatts"); ok {
			return v, true, nil
		}
	}
	return 0, false, nil
}

func (h *HTTPControlClient) ApplyCPUControl(action string, capWatts float64, throttlePct int) error {
	url := strings.ReplaceAll(h.endpoint, "{node}", h.nodeName)
	reqBody := map[string]any{
		"node":        h.nodeName,
		"action":      action,
		"capWatts":    capWatts,
		"throttlePct": throttlePct,
		"ts":          time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("control endpoint status=%d", resp.StatusCode)
	}
	return nil
}

func (h *HTTPControlClient) ApplyGPUControl(action string, capWattsPerGPU float64) error {
	url := strings.ReplaceAll(h.endpoint, "{node}", h.nodeName)
	reqBody := map[string]any{
		"node":           h.nodeName,
		"action":         action,
		"capWattsPerGpu": capWattsPerGPU,
		"ts":             time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("control endpoint status=%d", resp.StatusCode)
	}
	return nil
}

func (d *DVFSController) RestoreAllMax() (int, error) {
	written, err := d.applyThrottlePct(0, nil, 0)
	if err == nil {
		d.throttlePct = 0
		d.metrics.dvfsThrottlePct.WithLabelValues(d.metrics.node).Set(0)
		d.updateCPUFreqMetrics()
	}
	return written, err
}

func (d *DVFSController) applyThrottlePct(pct int, controlClient *HTTPControlClient, capWatts float64) (int, error) {
	if pct < 0 || pct > 100 {
		return 0, fmt.Errorf("invalid throttle pct %d", pct)
	}
	if controlClient != nil {
		if err := controlClient.ApplyCPUControl("dvfs.set_throttle_pct", capWatts, pct); err != nil {
			return 0, err
		}
		return 1, nil
	}
	count := len(d.cpus)
	if count == 0 {
		return 0, fmt.Errorf("no cpufreq scaling_max_freq files found under /host-sys/devices/system/cpu")
	}
	throttleCount := int(math.Ceil(float64(count) * float64(pct) / 100.0))
	written := 0
	for i, c := range d.cpus {
		target := c.MaxKHz
		if i < throttleCount {
			target = maxInt64(c.MinKHz, minInt64(c.MaxKHz, d.minFreqKHz))
		}
		if err := os.WriteFile(c.MaxFile, []byte(strconv.FormatInt(target, 10)), 0); err != nil {
			return written, fmt.Errorf("write %s: %w", c.MaxFile, err)
		}
		written++
	}
	return written, nil
}

func (d *DVFSController) updateCPUFreqMetrics() {
	for _, c := range d.cpus {
		cpuLabel := strconv.Itoa(c.Index)
		if maxKHz, err := readInt64(c.MaxFile); err == nil {
			d.metrics.dvfsCPUMaxFreqKHz.WithLabelValues(d.metrics.node, cpuLabel).Set(float64(maxKHz))
		}
		if curKHz, err := readInt64(c.CurFile); err == nil {
			d.metrics.dvfsCPUCurFreqKHz.WithLabelValues(d.metrics.node, cpuLabel).Set(float64(curKHz))
		}
	}
}

func (d *DVFSController) readPowerWatts() (float64, bool, error) {
	if d.powerReader != nil {
		return d.powerReader.ReadPowerWatts()
	}
	files, err := energyFiles()
	if err != nil {
		return 0, false, err
	}
	if len(files) == 0 {
		return 0, false, nil
	}

	totalW := 0.0
	count := 0
	now := time.Now()
	for _, f := range files {
		zone := filepath.Base(filepath.Dir(f))
		currentUJ, err := readInt64(f)
		if err != nil {
			continue
		}
		d.metrics.raplEnergyUJ.WithLabelValues(d.metrics.node, zone).Set(float64(currentUJ))
		s, ok := d.samples[f]
		if !ok {
			rangeUJ, _ := readInt64(filepath.Join(filepath.Dir(f), "max_energy_range_uj"))
			d.samples[f] = energySample{LastUJ: currentUJ, LastTime: now, RangeUJ: rangeUJ}
			continue
		}

		deltaUJ := currentUJ - s.LastUJ
		if deltaUJ < 0 && s.RangeUJ > 0 {
			deltaUJ += s.RangeUJ
		}
		dt := now.Sub(s.LastTime).Seconds()
		s.LastUJ = currentUJ
		s.LastTime = now
		d.samples[f] = s
		if dt <= 0 || deltaUJ < 0 {
			continue
		}
		w := (float64(deltaUJ) / 1_000_000.0) / dt
		d.metrics.raplPowerWatts.WithLabelValues(d.metrics.node, zone).Set(w)
		totalW += w
		count++
	}
	if count == 0 {
		return 0, false, nil
	}
	d.metrics.raplPackageTotalW.WithLabelValues(d.metrics.node).Set(totalW)
	return totalW, true, nil
}

func discoverHardware(nodeLabels map[string]string) HardwareInfo {
	hw := HardwareInfo{CPUVendor: discoverCPUVendor(nodeLabels)}

	if hasNFDGPUVendor(nodeLabels, "10de") {
		hw.GPUVendors = append(hw.GPUVendors, "nvidia")
	}
	if hasNFDGPUVendor(nodeLabels, "1002") {
		hw.GPUVendors = append(hw.GPUVendors, "amd")
	}
	if hasNFDGPUVendor(nodeLabels, "8086") {
		hw.GPUVendors = append(hw.GPUVendors, "intel")
	}
	if len(hw.GPUVendors) == 0 {
		switch detectGPUVendor(context.Background(), nodeLabels) {
		case "nvidia":
			hw.GPUVendors = append(hw.GPUVendors, "nvidia")
		case "amd":
			hw.GPUVendors = append(hw.GPUVendors, "amd")
		}
	}

	return hw
}

func discoverCPUVendor(nodeLabels map[string]string) string {
	if v := normalizeCPUVendor(nodeLabels["feature.node.kubernetes.io/cpu-vendor"]); v != "" {
		return v
	}
	if v := normalizeCPUVendor(nodeLabels["feature.node.kubernetes.io/cpu-model.vendor_id"]); v != "" {
		return v
	}
	return ""
}

func normalizeCPUVendor(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	switch strings.ToUpper(v) {
	case "AUTHENTICAMD", "AMD":
		return "AuthenticAMD"
	case "GENUINEINTEL", "INTEL":
		return "GenuineIntel"
	default:
		return v
	}
}

func hasNFDGPUVendor(nodeLabels map[string]string, vendorHex string) bool {
	keys := []string{
		fmt.Sprintf("feature.node.kubernetes.io/pci-%s.present", vendorHex),
		fmt.Sprintf("feature.node.kubernetes.io/pci-0300_%s.present", vendorHex),
		fmt.Sprintf("feature.node.kubernetes.io/pci-0302_%s.present", vendorHex),
	}
	for _, k := range keys {
		if nodeLabels[k] == "true" {
			return true
		}
	}
	return false
}

func resolveDesiredStateForNode(ctx context.Context, dyn dynamic.Interface, nodeName string) (*DesiredState, string, error) {
	np, err := getNodePowerProfile(ctx, dyn, nodeName)
	if err != nil {
		return nil, "", fmt.Errorf("get NodePowerProfile: %w", err)
	}
	if np != nil {
		return &DesiredState{
			Name:          np.Name,
			PowerWatts:    np.PowerWatts,
			PowerPctOfMax: np.PowerPctOfMax,
			GPU:           np.GPU,
		}, "nodepowerprofile", nil
	}
	return nil, "", nil
}

func resolveTelemetryConfigForNode(ctx context.Context, dyn dynamic.Interface, nodeName string) (*TelemetryConfig, error) {
	cfg, err := getTelemetryProfileForNode(ctx, dyn, nodeName)
	if err != nil {
		return nil, fmt.Errorf("get TelemetryProfile: %w", err)
	}
	return cfg, nil
}

func getNodePowerProfile(ctx context.Context, dyn dynamic.Interface, nodeName string) (*NodePowerProfile, error) {
	ul, err := dyn.Resource(profileNodeGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, item := range ul.Items {
		np := parseNodePowerProfile(item)
		if np.NodeName == nodeName {
			return &np, nil
		}
	}
	return nil, nil
}

func getTelemetryProfileForNode(ctx context.Context, dyn dynamic.Interface, nodeName string) (*TelemetryConfig, error) {
	ul, err := dyn.Resource(telemetryNodeGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, item := range ul.Items {
		cfg := parseTelemetryProfile(item)
		if cfg.TargetScope == "node" && cfg.NodeName == nodeName {
			return &cfg, nil
		}
	}
	return nil, nil
}

func parseNodePowerProfile(u unstructured.Unstructured) NodePowerProfile {
	np := NodePowerProfile{Name: u.GetName()}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "nodeName"); ok {
		np.NodeName = v
	}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "profile"); ok {
		np.Profile = v
	}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "policy", "name"); ok {
		np.PolicyName = v
	}
	if w, ok, _ := unstructured.NestedFloat64(u.Object, "spec", "cpu", "packagePowerCapWatts"); ok {
		np.PowerWatts = &w
	} else if wi, ok, _ := unstructured.NestedInt64(u.Object, "spec", "cpu", "packagePowerCapWatts"); ok {
		w := float64(wi)
		np.PowerWatts = &w
	}
	if p, ok, _ := unstructured.NestedFloat64(u.Object, "spec", "cpu", "packagePowerCapPctOfMax"); ok {
		np.PowerPctOfMax = &p
	} else if pi, ok, _ := unstructured.NestedInt64(u.Object, "spec", "cpu", "packagePowerCapPctOfMax"); ok {
		p := float64(pi)
		np.PowerPctOfMax = &p
	}
	gpu := GPUPowerCap{}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "gpu", "powerCap", "scope"); ok {
		gpu.Scope = strings.TrimSpace(v)
	}
	if w, ok, _ := unstructured.NestedFloat64(u.Object, "spec", "gpu", "powerCap", "capWattsPerGpu"); ok {
		gpu.CapWattsPerGPU = &w
	} else if wi, ok, _ := unstructured.NestedInt64(u.Object, "spec", "gpu", "powerCap", "capWattsPerGpu"); ok {
		w := float64(wi)
		gpu.CapWattsPerGPU = &w
	}
	if p, ok, _ := unstructured.NestedFloat64(u.Object, "spec", "gpu", "powerCap", "capPctOfMax"); ok {
		gpu.CapPctOfMax = &p
	} else if pi, ok, _ := unstructured.NestedInt64(u.Object, "spec", "gpu", "powerCap", "capPctOfMax"); ok {
		p := float64(pi)
		gpu.CapPctOfMax = &p
	}
	if gpu.CapWattsPerGPU != nil || gpu.CapPctOfMax != nil {
		if gpu.Scope == "" {
			gpu.Scope = "perGpu"
		}
		np.GPU = &gpu
	}
	return np
}

func parseTelemetryProfile(u unstructured.Unstructured) TelemetryConfig {
	out := TelemetryConfig{Name: u.GetName()}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "target", "scope"); ok {
		out.TargetScope = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "target", "nodeName"); ok {
		out.NodeName = v
	}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "sources", "cpu", "type"); ok {
		out.CPUSourceType = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "sources", "cpu", "http", "endpoint"); ok {
		out.HTTPEndpoint = strings.TrimSpace(v)
	}
	if v, ok, _ := unstructured.NestedInt64(u.Object, "spec", "sources", "cpu", "http", "timeoutSeconds"); ok {
		out.TimeoutSeconds = int(v)
	}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "controls", "cpu", "type"); ok {
		out.CPUControlType = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "controls", "cpu", "http", "endpoint"); ok {
		out.ControlHTTPEndpoint = strings.TrimSpace(v)
	}
	if v, ok, _ := unstructured.NestedInt64(u.Object, "spec", "controls", "cpu", "http", "timeoutSeconds"); ok {
		out.ControlTimeoutSeconds = int(v)
	}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "controls", "cpu", "http", "mode"); ok {
		out.ControlMode = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "controls", "gpu", "type"); ok {
		out.GPUControlType = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "controls", "gpu", "http", "endpoint"); ok {
		out.GPUControlHTTPEndpoint = strings.TrimSpace(v)
	}
	if v, ok, _ := unstructured.NestedInt64(u.Object, "spec", "controls", "gpu", "http", "timeoutSeconds"); ok {
		out.GPUControlTimeoutSeconds = int(v)
	}
	if v, ok, _ := unstructured.NestedString(u.Object, "spec", "controls", "gpu", "http", "mode"); ok {
		out.GPUControlMode = strings.ToLower(strings.TrimSpace(v))
	}
	return out
}

func updateTelemetryControlStatus(ctx context.Context, dyn dynamic.Interface, cfg *TelemetryConfig, nodeName, backend, result, message string) error {
	if cfg == nil || cfg.Name == "" {
		return nil
	}
	res := dyn.Resource(telemetryNodeGVR)
	obj, err := res.Get(ctx, cfg.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if sc, ok, _ := unstructured.NestedString(obj.Object, "spec", "target", "scope"); ok && sc != "node" {
		return nil
	}
	if nn, ok, _ := unstructured.NestedString(obj.Object, "spec", "target", "nodeName"); ok && nn != nodeName {
		return nil
	}
	_ = unstructured.SetNestedField(obj.Object, backend, "status", "control", "cpu", "backend")
	_ = unstructured.SetNestedField(obj.Object, result, "status", "control", "cpu", "result")
	_ = unstructured.SetNestedField(obj.Object, message, "status", "control", "cpu", "message")
	_ = unstructured.SetNestedField(obj.Object, time.Now().UTC().Format(time.RFC3339), "status", "control", "cpu", "updatedAt")
	_, err = res.UpdateStatus(ctx, obj, metav1.UpdateOptions{})
	if err == nil {
		return nil
	}
	_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

func updateTelemetryGPUStatus(ctx context.Context, dyn dynamic.Interface, cfg *TelemetryConfig, nodeName, backend, result, message string, observed map[string]any) error {
	if cfg == nil || cfg.Name == "" {
		return nil
	}
	res := dyn.Resource(telemetryNodeGVR)
	obj, err := res.Get(ctx, cfg.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if sc, ok, _ := unstructured.NestedString(obj.Object, "spec", "target", "scope"); ok && sc != "node" {
		return nil
	}
	if nn, ok, _ := unstructured.NestedString(obj.Object, "spec", "target", "nodeName"); ok && nn != nodeName {
		return nil
	}
	_ = unstructured.SetNestedField(obj.Object, backend, "status", "control", "gpu", "backend")
	_ = unstructured.SetNestedField(obj.Object, result, "status", "control", "gpu", "result")
	_ = unstructured.SetNestedField(obj.Object, message, "status", "control", "gpu", "message")
	if observed != nil {
		_ = unstructured.SetNestedField(obj.Object, observed, "status", "control", "gpu", "observed")
	}
	_ = unstructured.SetNestedField(obj.Object, time.Now().UTC().Format(time.RFC3339), "status", "control", "gpu", "updatedAt")
	_, err = res.UpdateStatus(ctx, obj, metav1.UpdateOptions{})
	if err == nil {
		return nil
	}
	_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

func updateTelemetryHardwareStatus(ctx context.Context, dyn dynamic.Interface, cfg *TelemetryConfig, nodeName string, nodeLabels map[string]string, hw HardwareInfo) error {
	if cfg == nil || cfg.Name == "" {
		return nil
	}
	res := dyn.Resource(telemetryNodeGVR)
	obj, err := res.Get(ctx, cfg.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if sc, ok, _ := unstructured.NestedString(obj.Object, "spec", "target", "scope"); ok && sc != "node" {
		return nil
	}
	if nn, ok, _ := unstructured.NestedString(obj.Object, "spec", "target", "nodeName"); ok && nn != nodeName {
		return nil
	}
	driver := detectCPUDriverFamily()
	gpuVendor := detectGPUVendor(ctx, nodeLabels)
	gpuDevices, _ := listGPUDevices(ctx, gpuVendor)
	gpuCaps := map[string]any{
		"vendor":      gpuVendor,
		"deviceCount": int64(len(gpuDevices)),
	}
	if len(gpuDevices) > 0 {
		gpuCaps["product"] = gpuDevices[0].Product
		gpuCaps["minCapWatts"] = gpuDevices[0].MinCapWatts
		gpuCaps["maxCapWatts"] = gpuDevices[0].MaxCapWatts
		gpuCaps["currentCapWatts"] = gpuDevices[0].CurrentCapWatts
	}
	cpuRangeMax, cpuRangeMin, haveRange := readRAPLPackageCapRangeWatts()
	hwStatus := map[string]any{
		"cpuVendor":       hw.CPUVendor,
		"cpuDriverFamily": driver,
		"gpu":             gpuCaps,
	}
	if haveRange {
		hwStatus["cpuCapMinWatts"] = cpuRangeMin
		hwStatus["cpuCapMaxWatts"] = cpuRangeMax
	}
	_ = unstructured.SetNestedField(obj.Object, hwStatus, "status", "hardware")
	_ = unstructured.SetNestedField(obj.Object, time.Now().UTC().Format(time.RFC3339), "status", "hardware", "updatedAt")
	_, err = res.UpdateStatus(ctx, obj, metav1.UpdateOptions{})
	if err == nil {
		return nil
	}
	_, err = res.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

func detectCPUDriverFamily() string {
	b, err := os.ReadFile("/host-sys/devices/system/cpu/cpufreq/policy0/scaling_driver")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func extractFloat(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch vv := v.(type) {
	case float64:
		return vv, true
	case int:
		return float64(vv), true
	case int64:
		return float64(vv), true
	case json.Number:
		f, err := vv.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

func extractFloatAny(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch vv := v.(type) {
	case float64:
		return vv, true
	case int:
		return float64(vv), true
	case int64:
		return float64(vv), true
	case string:
		s := strings.TrimSpace(strings.TrimSuffix(vv, "W"))
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

func extractStringAny(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

func readInt64(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

func durationEnv(key string, def time.Duration) time.Duration {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		if v, err := time.ParseDuration(s); err == nil {
			return v
		}
	}
	return def
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

func intEnv(key string, def int) int {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return def
}

func int64Env(key string, def int64) int64 {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v
		}
	}
	return def
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
