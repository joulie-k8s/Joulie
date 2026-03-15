package classifier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PrometheusConfig configures the Prometheus metrics source.
type PrometheusConfig struct {
	// Address is the Prometheus server URL, e.g. "http://prometheus.monitoring:9090"
	Address string
	// Timeout for metric queries.
	Timeout time.Duration
	// KeplerAvailable indicates whether Kepler energy metrics are available.
	// When false, energy fields in PodMetrics are zero; classification falls back
	// to utilization % only (which is the primary signal anyway).
	KeplerAvailable bool
}

// DefaultPrometheusConfig returns a default Prometheus config.
func DefaultPrometheusConfig() PrometheusConfig {
	return PrometheusConfig{
		Address:         "http://prometheus-operated.monitoring:9090",
		Timeout:         5 * time.Second,
		KeplerAvailable: true,
	}
}

// PodMetrics holds the metrics collected for a single pod over a measurement window.
//
// Primary classification signals (always fetched when Prometheus is available):
//   - CPUUtilPct: fraction of CPU request used (cAdvisor) - identifies compute-bound
//   - GPUUtilPct: GPU utilization % (DCGM or Kepler) - identifies GPU-bound
//   - MemoryPressurePct: memory working set / limit - identifies memory-bound
//
// These utilization % signals directly indicate which resource is the bottleneck.
//
// Kepler energy counters are optional enrichment for energy accounting.
// They are NOT required for classification; util % signals are sufficient.
type PodMetrics struct {
	PodName   string
	Namespace string
	Window    time.Duration

	// Primary signals
	CPUUsageCores     float64 // avg cores in use
	CPURequestCores   float64 // requested CPU cores
	CPUUtilPct        float64 // 0-100: usage / request
	GPUUtilPct        float64 // 0-100: GPU compute utilization (DCGM preferred, Kepler fallback)
	MemoryPressurePct float64 // 0-100: working_set / (limit or request)

	// Optional Kepler energy signals (for PUE attribution and billing)
	CPUEnergyJoules   float64
	DRAMEnergyJoules  float64
	GPUEnergyJoules   float64
	TotalEnergyJoules float64
	KeplerUsed        bool

	// Derived: computed from primary signals in FetchPodMetrics
	CPUBoundRatio    float64 // high CPU util + low GPU/mem = CPU compute-bound
	MemoryBoundRatio float64 // high mem pressure with moderate CPU = memory-bound
	GPUDominant      bool    // GPU util is the dominant resource signal
}

// MetricsReader fetches workload metrics from Prometheus.
type MetricsReader struct {
	cfg    PrometheusConfig
	client *http.Client
}

// NewMetricsReader creates a new MetricsReader.
func NewMetricsReader(cfg PrometheusConfig) *MetricsReader {
	return &MetricsReader{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
	}
}

// FetchPodMetrics fetches metrics for a specific pod over the given window.
//
// Classification is primarily based on utilization % signals (CPU/GPU/memory).
// Kepler energy metrics are fetched as additional enrichment when available.
func (r *MetricsReader) FetchPodMetrics(ctx context.Context, namespace, podName string, window time.Duration) (PodMetrics, error) {
	m := PodMetrics{
		PodName:   podName,
		Namespace: namespace,
		Window:    window,
	}

	windowStr := fmt.Sprintf("%.0fs", window.Seconds())

	// --- Primary signal: CPU utilization ---
	cpuRate, err := r.queryScalar(ctx, fmt.Sprintf(
		`sum(rate(container_cpu_usage_seconds_total{pod=%q, namespace=%q, container!="POD"}[%s]))`,
		podName, namespace, windowStr))
	if err == nil {
		m.CPUUsageCores = cpuRate
	}

	cpuReq, err := r.queryScalar(ctx, fmt.Sprintf(
		`sum(kube_pod_container_resource_requests{pod=%q, namespace=%q, resource="cpu"})`,
		podName, namespace))
	if err == nil && cpuReq > 0 {
		m.CPURequestCores = cpuReq
		m.CPUUtilPct = (m.CPUUsageCores / cpuReq) * 100
		if m.CPUUtilPct > 100 {
			m.CPUUtilPct = 100
		}
	}

	// --- Primary signal: Memory pressure ---
	memWS, err := r.queryScalar(ctx, fmt.Sprintf(
		`sum(container_memory_working_set_bytes{pod=%q, namespace=%q, container!="POD"})`,
		podName, namespace))
	if err == nil {
		memLimit, lerr := r.queryScalar(ctx, fmt.Sprintf(
			`sum(kube_pod_container_resource_limits{pod=%q, namespace=%q, resource="memory"})`,
			podName, namespace))
		if lerr == nil && memLimit > 0 {
			m.MemoryPressurePct = (memWS / memLimit) * 100
			if m.MemoryPressurePct > 100 {
				m.MemoryPressurePct = 100
			}
		}
	}

	// --- Primary signal: GPU utilization ---
	// Try DCGM first (most accurate for NVIDIA), then Kepler GPU metric as fallback.
	gpuUtil, err := r.queryScalar(ctx, fmt.Sprintf(
		`avg(DCGM_FI_DEV_GPU_UTIL{pod=%q, namespace=%q})`,
		podName, namespace))
	if err == nil {
		m.GPUUtilPct = gpuUtil
	} else if r.cfg.KeplerAvailable {
		// Kepler GPU energy rate as proxy for GPU utilization
		gpuERate, kerr := r.queryScalar(ctx, fmt.Sprintf(
			`sum(rate(kepler_container_gpu_joules_total{pod_name=%q, namespace=%q}[%s]))`,
			podName, namespace, windowStr))
		if kerr == nil && gpuERate > 0 {
			// Rough proxy: 400W reference GPU → 1J/s ≈ 0.25% util per watt
			m.GPUUtilPct = (gpuERate / 400.0) * 100
			if m.GPUUtilPct > 100 {
				m.GPUUtilPct = 100
			}
		}
	}

	// --- Optional Kepler energy signals ---
	if r.cfg.KeplerAvailable {
		cpuE, err := r.queryScalar(ctx, fmt.Sprintf(
			`sum(rate(kepler_container_package_joules_total{pod_name=%q, namespace=%q}[%s]))`,
			podName, namespace, windowStr))
		if err == nil {
			m.CPUEnergyJoules = cpuE * window.Seconds()
		}
		dramE, err := r.queryScalar(ctx, fmt.Sprintf(
			`sum(rate(kepler_container_dram_joules_total{pod_name=%q, namespace=%q}[%s]))`,
			podName, namespace, windowStr))
		if err == nil {
			m.DRAMEnergyJoules = dramE * window.Seconds()
		}
		gpuE, err := r.queryScalar(ctx, fmt.Sprintf(
			`sum(rate(kepler_container_gpu_joules_total{pod_name=%q, namespace=%q}[%s]))`,
			podName, namespace, windowStr))
		if err == nil {
			m.GPUEnergyJoules = gpuE * window.Seconds()
		}
		m.TotalEnergyJoules = m.CPUEnergyJoules + m.DRAMEnergyJoules + m.GPUEnergyJoules
		m.KeplerUsed = true
	}

	// --- Derive classification ratios from primary util signals ---
	// CPUBoundRatio: CPU is heavily used and GPU/memory are not
	if m.CPUUtilPct > 0 {
		gpuLoad := m.GPUUtilPct
		memLoad := m.MemoryPressurePct
		maxOther := gpuLoad
		if memLoad > maxOther {
			maxOther = memLoad
		}
		if m.CPUUtilPct > 60 && maxOther < 30 {
			m.CPUBoundRatio = m.CPUUtilPct / 100.0
		}
	}
	// MemoryBoundRatio: high memory pressure with moderate CPU
	if m.MemoryPressurePct > 50 && m.CPUUtilPct < 60 {
		m.MemoryBoundRatio = m.MemoryPressurePct / 100.0
	}
	// GPUDominant: GPU utilization is the primary signal
	m.GPUDominant = m.GPUUtilPct > 40 && m.GPUUtilPct > m.CPUUtilPct

	return m, nil
}

// queryScalar executes a PromQL instant query and returns the first scalar value.
func (r *MetricsReader) queryScalar(ctx context.Context, query string) (float64, error) {
	url := fmt.Sprintf("%s/api/v1/query?query=%s", r.cfg.Address, encodeQuery(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var result promResult
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}
	if result.Status != "success" || len(result.Data.Result) == 0 {
		return 0, fmt.Errorf("no data for query")
	}
	if len(result.Data.Result[0].Value) < 2 {
		return 0, fmt.Errorf("unexpected value format")
	}
	valStr, ok := result.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("value not a string")
	}
	return strconv.ParseFloat(valStr, 64)
}

type promResult struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Value []interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func encodeQuery(q string) string {
	return strings.NewReplacer(
		" ", "%20", "{", "%7B", "}", "%7D",
		`"`, "%22", "=", "%3D", "[", "%5B", "]", "%5D",
		"(", "%28", ")", "%29", "/", "%2F",
	).Replace(q)
}
