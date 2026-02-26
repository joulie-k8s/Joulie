package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	policyGVR = schema.GroupVersionResource{Group: "joulie.io", Version: "v1alpha1", Resource: "powerpolicies"}
)

type HardwareInfo struct {
	CPUVendor  string
	GPUVendors []string
}

type PowerPolicy struct {
	Name       string
	Priority   int64
	Selector   map[string]string
	PowerWatts *float64
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
}

func newAgentMetrics(node string) *AgentMetrics {
	m := &AgentMetrics{
		node: node,
		backendMode: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_backend_mode",
			Help: "Current backend mode (1 active) per node and mode",
		}, []string{"node", "mode"}),
		policyCapWatts: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_policy_cap_watts",
			Help: "Current policy cap watts selected by the agent",
		}, []string{"node", "policy"}),
		raplEnergyUJ: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_rapl_energy_uj",
			Help: "Latest RAPL energy reading in microjoules",
		}, []string{"node", "zone"}),
		raplPowerWatts: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_rapl_estimated_power_watts",
			Help: "Estimated per-zone RAPL power in watts",
		}, []string{"node", "zone"}),
		raplPackageTotalW: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_rapl_package_total_power_watts",
			Help: "Estimated total package power watts (sum of package zones)",
		}, []string{"node"}),
		dvfsObservedPowerW: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_observed_power_watts",
			Help: "Observed package power used by DVFS controller",
		}, []string{"node"}),
		dvfsEMAPowerW: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_ema_power_watts",
			Help: "EMA package power used by DVFS controller",
		}, []string{"node"}),
		dvfsThrottlePct: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_throttle_pct",
			Help: "Current DVFS throttle percent",
		}, []string{"node"}),
		dvfsTripAbove: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_above_trip_count",
			Help: "Consecutive above-threshold samples",
		}, []string{"node"}),
		dvfsTripBelow: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_below_trip_count",
			Help: "Consecutive below-threshold samples",
		}, []string{"node"}),
		dvfsCPUCurFreqKHz: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_cpu_cur_freq_khz",
			Help: "Current CPU/policy frequency in kHz",
		}, []string{"node", "cpu"}),
		dvfsCPUMaxFreqKHz: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "joulie_dvfs_cpu_max_freq_khz",
			Help: "Current CPU/policy max frequency cap in kHz",
		}, []string{"node", "cpu"}),
		dvfsActionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_dvfs_actions_total",
			Help: "Total number of DVFS control actions",
		}, []string{"node", "action"}),
		reconcileErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_reconcile_errors_total",
			Help: "Total reconcile errors",
		}, []string{"node"}),
	}

	prometheus.MustRegister(
		m.backendMode, m.policyCapWatts, m.raplEnergyUJ, m.raplPowerWatts, m.raplPackageTotalW,
		m.dvfsObservedPowerW, m.dvfsEMAPowerW, m.dvfsThrottlePct, m.dvfsTripAbove, m.dvfsTripBelow,
		m.dvfsCPUCurFreqKHz, m.dvfsCPUMaxFreqKHz, m.dvfsActionsTotal, m.reconcileErrorsTotal,
	)
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

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		log.Fatal("NODE_NAME env var is required")
	}

	metrics := newAgentMetrics(nodeName)
	startMetricsServer()

	reconcileEvery := durationEnv("RECONCILE_INTERVAL", 20*time.Second)
	dvfs, err := newDVFSControllerFromEnv(metrics)
	if err != nil {
		log.Fatalf("dvfs init: %v", err)
	}

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

	var lastRaplKey string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := reconcileOnce(ctx, kube, dyn, nodeName, dvfs, metrics, &lastRaplKey)
		cancel()
		if err != nil {
			metrics.reconcileErrorsTotal.WithLabelValues(nodeName).Inc()
			log.Printf("reconcile failed: %v", err)
		}
		time.Sleep(reconcileEvery)
	}
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
	kube *kubernetes.Clientset,
	dyn dynamic.Interface,
	nodeName string,
	dvfs *DVFSController,
	metrics *AgentMetrics,
	lastRaplKey *string,
) error {
	node, err := kube.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node: %w", err)
	}

	hw := discoverHardware(node.Labels)
	policies, err := listPolicies(ctx, dyn)
	if err != nil {
		return fmt.Errorf("list powerpolicies: %w", err)
	}

	selected := selectPolicyForNode(policies, node.Labels)
	if selected == nil {
		metrics.setBackendMode("none")
		if *lastRaplKey != "" {
			log.Printf("no matching policy for node %s; leaving current settings untouched", nodeName)
			*lastRaplKey = ""
		}
		return nil
	}
	if selected.PowerWatts == nil {
		log.Printf("policy %s has no cpu.packagePowerCapWatts; nothing to enforce", selected.Name)
		return nil
	}
	metrics.policyCapWatts.WithLabelValues(nodeName, selected.Name).Set(*selected.PowerWatts)
	if len(hw.GPUVendors) > 0 {
		log.Printf("discovered GPUs %v on node %s; GPU caps are not implemented yet", hw.GPUVendors, nodeName)
	}

	appliedRapl, raplFiles, err := applyRAPLPackageCap(hw, *selected.PowerWatts)
	if err != nil {
		return err
	}
	if appliedRapl {
		metrics.setBackendMode("rapl")
		key := fmt.Sprintf("%s|rapl|%.2f", selected.Name, *selected.PowerWatts)
		if key != *lastRaplKey {
			log.Printf("applied policy=%s backend=rapl cap=%.2fW files=%d cpuVendor=%s", selected.Name, *selected.PowerWatts, raplFiles, hw.CPUVendor)
			*lastRaplKey = key
		}
		dvfs.warned = false
		if dvfs.Active() {
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
	if raplFiles == 0 && !dvfs.warned {
		log.Printf("warning: RAPL power-limit files not available on node %s (vendor=%s); using DVFS fallback controller", nodeName, hw.CPUVendor)
		dvfs.warned = true
	}
	action, err := dvfs.Reconcile(*selected.PowerWatts)
	if err != nil {
		return fmt.Errorf("dvfs fallback failed: %w", err)
	}
	if action != "" {
		log.Printf("dvfs-control node=%s policy=%s cap=%.2fW %s", nodeName, selected.Name, *selected.PowerWatts, action)
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
		return nil, fmt.Errorf("no cpufreq scaling_max_freq files found under /host-sys/devices/system/cpu")
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

func (d *DVFSController) Reconcile(capWatts float64) (string, error) {
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
		written, err := d.applyThrottlePct(d.throttlePct)
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
		written, err := d.applyThrottlePct(d.throttlePct)
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

func (d *DVFSController) RestoreAllMax() (int, error) {
	written, err := d.applyThrottlePct(0)
	if err == nil {
		d.throttlePct = 0
		d.metrics.dvfsThrottlePct.WithLabelValues(d.metrics.node).Set(0)
		d.updateCPUFreqMetrics()
	}
	return written, err
}

func (d *DVFSController) applyThrottlePct(pct int) (int, error) {
	if pct < 0 || pct > 100 {
		return 0, fmt.Errorf("invalid throttle pct %d", pct)
	}
	count := len(d.cpus)
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

func listPolicies(ctx context.Context, dyn dynamic.Interface) ([]PowerPolicy, error) {
	ul, err := dyn.Resource(policyGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]PowerPolicy, 0, len(ul.Items))
	for _, item := range ul.Items {
		out = append(out, parsePolicy(item))
	}
	return out, nil
}

func parsePolicy(u unstructured.Unstructured) PowerPolicy {
	p := PowerPolicy{Name: u.GetName(), Selector: map[string]string{}}

	if v, ok, _ := unstructured.NestedInt64(u.Object, "spec", "priority"); ok {
		p.Priority = v
	}
	if m, ok, _ := unstructured.NestedStringMap(u.Object, "spec", "selector", "matchLabels"); ok {
		p.Selector = m
	}
	if w, ok, _ := unstructured.NestedFloat64(u.Object, "spec", "cpu", "packagePowerCapWatts"); ok {
		p.PowerWatts = &w
	} else if wi, ok, _ := unstructured.NestedInt64(u.Object, "spec", "cpu", "packagePowerCapWatts"); ok {
		w := float64(wi)
		p.PowerWatts = &w
	}

	return p
}

func selectPolicyForNode(policies []PowerPolicy, nodeLabels map[string]string) *PowerPolicy {
	matches := make([]PowerPolicy, 0)
	for _, p := range policies {
		sel := labels.SelectorFromSet(labels.Set(p.Selector))
		if sel.Matches(labels.Set(nodeLabels)) {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Priority == matches[j].Priority {
			return matches[i].Name < matches[j].Name
		}
		return matches[i].Priority > matches[j].Priority
	})
	return &matches[0]
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
