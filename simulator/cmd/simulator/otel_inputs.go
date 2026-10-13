package main

// The simulator as a source of the OpenTelemetry inputs Joulie reads: the
// OTLP JSON node telemetry the agent's and the controller manager's HTTP
// power readers take (/otlp/telemetry/{node}, and /telemetry/{node} by
// default), and the fake Prometheus that answers the controller manager's
// facility queries (/api/v1/query) without a real Prometheus.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
)

// telemetrySnapshot is one node's reading, taken under the simulator lock,
// that both telemetry routes write: OTLP on /otlp/telemetry/{node} and, by
// default, /telemetry/{node}; the legacy JSON on /telemetry/{node} with
// ?format=legacy.
type telemetrySnapshot struct {
	node  string
	model simModel
	// power is the legacy packagePowerWatts: the node's average power
	// rounded to 0.01 W, its instantaneous power while the average is 0.
	power float64

	capW, throttlePct, cpuUtil, freqScale, cpuAvgPower, cpuInstantPower float64
	cpuWorkClass, gpuWorkClass                                          string
	capSaturated                                                        bool
	podsRunning                                                         int
	gpuPowerW, gpuCapPerGpu, gpuTargetCapPerGpu, gpuUtil                float64
	gpuPerfMul, memIntensity, ioIntensity, cpuFeedIntensity             float64
	cpuThermalThrottle, cpuTempC, targetCapW                            float64
	byIntentClass                                                       map[string]int
	gpuAvgPower                                                         float64

	// The components the OTLP route reports, unrounded and from the same
	// readings as power: the averages, or the instantaneous powers while the
	// node's average is 0, as observe chooses. hostPowerW is cpuPowerW plus
	// every GPU.
	cpuPowerW, hostPowerW float64
	gpus                  []telemetryGPU
}

// telemetryGPU is one GPU device's power in a telemetrySnapshot.
type telemetryGPU struct {
	index  int
	powerW float64
}

// telemetrySample reads node for a telemetry route: it observes the node as
// the last physics step left it (a read never advances it), sets the
// per-node gauges, records the "telemetry" event and logs one line, so a
// read of either route counts the same.
func (s *simulator) telemetrySample(node string) telemetrySnapshot {
	st := s.getNode(node)
	modelForTelemetry := s.modelForNode(node)

	// Snapshot state under lock, then build response outside lock to minimize
	// contention with the physics tick, the workload loop and pod polling.
	s.mu.Lock()
	obs := observe(st)
	power := obs.powerW
	snap := telemetrySnapshot{
		node: node, model: modelForTelemetry, power: power,
		capW: st.CapWatts, throttlePct: float64(st.ThrottlePct),
		cpuUtil: st.CPUUtil, freqScale: st.FreqScale,
		cpuAvgPower: st.CPUAvgPowerWatts, cpuInstantPower: st.CPUInstantPowerWatts,
		cpuWorkClass: st.CPUWorkClass, gpuWorkClass: st.GPUWorkClass,
		capSaturated: st.CapSaturated, podsRunning: st.PodsRunning,
		gpuPowerW: st.GPUPowerWatts, gpuCapPerGpu: st.GPUCapWattsPerGpu,
		gpuTargetCapPerGpu: st.GPUTargetCapWattsPerGpu, gpuUtil: st.GPUUtil,
		gpuPerfMul: st.GPUPerfMultiplier, memIntensity: st.MemoryIntensity,
		ioIntensity: st.IOIntensity, cpuFeedIntensity: st.CPUFeedIntensity,
		cpuThermalThrottle: st.CPUThermalThrottle, cpuTempC: st.CPUTemperatureC,
		targetCapW: st.TargetCapWatts, byIntentClass: st.ByIntentClass,
		gpuAvgPower: gpuAvgPower(st),
	}
	snap.cpuPowerW, snap.hostPowerW = obs.cpuW, obs.hostW
	for i, d := range st.GPUDevices {
		snap.gpus = append(snap.gpus, telemetryGPU{index: d.Index, powerW: obs.gpuW[i]})
	}
	s.mu.Unlock()

	// Prometheus metric updates are thread-safe, no lock needed.
	s.nodeCapW.WithLabelValues(node).Set(snap.capW)
	s.nodeThrottlePct.WithLabelValues(node).Set(snap.throttlePct)
	s.nodePowerW.WithLabelValues(node).Set(power)
	s.nodePods.WithLabelValues(node).Set(float64(snap.podsRunning))
	s.nodeUtilCPU.WithLabelValues(node).Set(snap.cpuUtil)
	s.nodeFreqScale.WithLabelValues(node).Set(snap.freqScale)
	s.nodeRaplCapW.WithLabelValues(node).Set(snap.capW)
	eventPayload := map[string]any{
		"packagePowerWatts":        power,
		"instantPackagePowerWatts": snap.cpuInstantPower + snap.gpuPowerW,
		"capWatts":                 snap.capW,
		"throttlePct":              snap.throttlePct,
		"freqScale":                snap.freqScale,
		"cpuUtil":                  snap.cpuUtil,
		"cpuWorkClass":             snap.cpuWorkClass,
		"capSaturated":             snap.capSaturated,
		"podsRunning":              snap.podsRunning,
		"gpuPowerWatts":            snap.gpuPowerW,
		"gpuCapWattsPerGpu":        snap.gpuCapPerGpu,
		"gpuUtil":                  snap.gpuUtil,
		"gpuWorkClass":             snap.gpuWorkClass,
		"gpuPerfMultiplier":        snap.gpuPerfMul,
		"memoryIntensity":          snap.memIntensity,
		"ioIntensity":              snap.ioIntensity,
		"cpuFeedIntensity":         snap.cpuFeedIntensity,
	}
	s.recordEvent("telemetry", node, eventPayload)
	log.Printf("telemetry node=%s powerW=%.2f capW=%.2f throttlePct=%.0f pods=%d", node, power, snap.capW, snap.throttlePct, snap.podsRunning)
	return snap
}

// handleOTLPTelemetry serves /otlp/telemetry/{node}: the node's power as
// one OTLP JSON ExportMetricsServiceRequest (otlpTelemetryDocument), the
// format the agent's and the controller manager's HTTP power readers read.
// An emulated node answers with its part of the latest snapshot's export
// (handleEmulatedOTLPTelemetry).
func (s *simulator) handleOTLPTelemetry(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	defer s.observeRequest("otlp_telemetry", r.Method, &status, start)
	if r.Method != http.MethodGet {
		status = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", status)
		return
	}
	node := strings.TrimPrefix(r.URL.Path, "/otlp/telemetry/")
	if node == "" {
		status = http.StatusBadRequest
		http.Error(w, "missing node", status)
		return
	}
	s.serveOTLPTelemetry(w, node, &status)
}

// serveOTLPTelemetry writes node's OTLP JSON document, the answer of
// /otlp/telemetry/{node} and, by default, of /telemetry/{node}.
func (s *simulator) serveOTLPTelemetry(w http.ResponseWriter, node string, status *int) {
	if s.emulated(node) {
		s.handleEmulatedOTLPTelemetry(w, node, status)
		return
	}
	snap := s.telemetrySample(node)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(otlpTelemetryDocument(snap, otlpTelemetryResource(), time.Now()))
}

var (
	otlpResourceOnce sync.Once
	otlpResource     telemetry.Resource
)

// otlpTelemetryResource is what every node's resource shares, read once
// from OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES and POD_NAME. A malformed
// OTEL_RESOURCE_ATTRIBUTES is logged and left out.
func otlpTelemetryResource() telemetry.Resource {
	otlpResourceOnce.Do(func() {
		r, err := telemetry.ResourceFromEnv(os.Getenv)
		if err != nil {
			log.Printf("warning: %v", err)
		}
		otlpResource = r
	})
	return otlpResource
}

// otlpTelemetryDocument is snap as one OTLP export from the node's resource,
// all gauges read at at:
//   - hw.power of the CPU, hw.id=cpu and hw.type=cpu: the model has one CPU
//     for all packages;
//   - hw.power of each GPU device, hw.id=gpu<index> and hw.type=gpu, with
//     the product as hw.name when the model names one;
//   - hw.host.power of the node, hw.id=host.
//
// The legacy model keeps no energy counters, so the readers take the power
// gauges and are ok from the first read. The node's cluster has no name.
func otlpTelemetryDocument(snap telemetrySnapshot, res telemetry.Resource, at time.Time) otlpjson.MetricsData {
	var gauge time.Time // a gauge has no start time
	points := []otlpjson.NumberDataPoint{
		otlpjson.DoublePoint(gauge, at, snap.cpuPowerW,
			otlpjson.String(semconv.AttrHwID, semconv.HwTypeCPU),
			otlpjson.String(semconv.AttrHwType, semconv.HwTypeCPU)),
	}
	for _, g := range snap.gpus {
		attrs := []otlpjson.KeyValue{
			otlpjson.String(semconv.AttrHwID, fmt.Sprintf("%s%d", semconv.HwTypeGPU, g.index)),
			otlpjson.String(semconv.AttrHwType, semconv.HwTypeGPU),
		}
		if p := strings.TrimSpace(snap.model.GPU.Product); p != "" {
			attrs = append(attrs, otlpjson.String(semconv.AttrHwName, p))
		}
		points = append(points, otlpjson.DoublePoint(gauge, at, g.powerW, attrs...))
	}
	return otlpjson.NewMetricsData(
		res.Node(snap.node, ""),
		otlpjson.Scope{Name: telemetry.ScopeName, Version: res.ServiceVersion},
		otlpjson.GaugeMetric(semconv.HwPower, points...),
		otlpjson.GaugeMetric(semconv.HwHostPower,
			otlpjson.DoublePoint(gauge, at, snap.hostPowerW, otlpjson.String(semconv.AttrHwID, semconv.HwIDHost))),
	)
}

// handleFakePrometheusQuery serves a minimal /api/v1/query endpoint that
// returns facility metrics in the Prometheus instant-query response format.
// This allows the controller manager's facility metrics poller to query the simulator
// directly, without needing a real Prometheus instance in simulation.
func (s *simulator) handleFakePrometheusQuery(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	if query == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "missing query parameter"})
		return
	}

	val, ok := s.facilityMetricValue(query)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result":     []any{},
			},
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result": []any{
				map[string]any{
					"metric": map[string]string{},
					"value":  []any{float64(time.Now().Unix()), strconv.FormatFloat(val, 'f', -1, 64)},
				},
			},
		},
	})
}

// facilityMetricValue resolves a query string to the current facility
// metric value. A query that names a metric of inputs.FacilityMetricNames
// is the controller manager's query of that input, under any preset and
// translation strategy: ambient temperature, IT power and cooling power
// read the facility gauges, and the zone ambient, rack power and node inlet
// inputs have no data, since the legacy model has no cooling zones, racks
// or inlet sensors. Only a query that names none of them falls back to
// matching words of the name.
func (s *simulator) facilityMetricValue(query string) (float64, bool) {
	// Strip surrounding whitespace for robustness.
	q := strings.TrimSpace(query)
	if input, ok := facilityQueryInput(q); ok {
		switch input {
		case inputs.FacilityInputAmbientTemp:
			return gaugeValue(s.facilityAmbientTemp), true
		case inputs.FacilityInputITPower:
			return gaugeValue(s.facilityITPowerW), true
		case inputs.FacilityInputCoolingPower:
			return gaugeValue(s.facilityCoolingPowerW), true
		default:
			return 0, false
		}
	}
	switch {
	case strings.Contains(q, "ambient") && strings.Contains(q, "temp"),
		q == "datacenter_ambient_temperature_celsius",
		q == "joulie_sim_facility_ambient_temp_celsius":
		return gaugeValue(s.facilityAmbientTemp), true
	case strings.Contains(q, "it_power") || strings.Contains(q, "it_power_watts"),
		q == "datacenter_total_it_power_watts",
		q == "joulie_sim_facility_it_power_watts":
		return gaugeValue(s.facilityITPowerW), true
	case strings.Contains(q, "cooling") && strings.Contains(q, "power"),
		q == "datacenter_cooling_power_watts",
		q == "joulie_sim_facility_cooling_power_watts":
		return gaugeValue(s.facilityCoolingPowerW), true
	case strings.Contains(q, "pue"),
		q == "joulie_sim_facility_pue":
		return gaugeValue(s.facilityPUE), true
	default:
		return 0, false
	}
}

var (
	facilityNamesOnce   sync.Once
	facilityNameToInput map[string]inputs.FacilityInput
)

// facilityQueryInput returns the facility input of the first metric name in
// q that inputs.FacilityMetricNames lists.
func facilityQueryInput(q string) (inputs.FacilityInput, bool) {
	facilityNamesOnce.Do(func() {
		facilityNameToInput = map[string]inputs.FacilityInput{}
		for _, in := range inputs.FacilityInputs() {
			for _, n := range inputs.FacilityMetricNames(in) {
				if _, dup := facilityNameToInput[n]; !dup {
					facilityNameToInput[n] = in
				}
			}
		}
	})
	for _, name := range promQLNames(q) {
		if in, ok := facilityNameToInput[name]; ok {
			return in, true
		}
	}
	return "", false
}

// promQLToken matches, in order of precedence, a string in double quotes,
// single quotes or backticks (a UTF-8 metric or label name, or a label
// value), a comment, a number or duration, which is matched whole so its
// unit is not read as a name, and an identifier.
var promQLToken = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|` + "`[^`]*`" + `|#[^\n]*|[0-9][0-9a-zA-Z_.]*|[a-zA-Z_:][a-zA-Z0-9_:]*`)

// promQLNames returns the identifiers and unquoted strings of q in order:
// every token that can name a metric. Comments and numbers are left out.
func promQLNames(q string) []string {
	var out []string
	for _, tok := range promQLToken.FindAllString(q, -1) {
		switch c := tok[0]; {
		case c == '#', c >= '0' && c <= '9':
		case c == '"', c == '`':
			if v, err := strconv.Unquote(tok); err == nil {
				out = append(out, v)
			}
		case c == '\'':
			out = append(out, tok[1:len(tok)-1])
		default:
			out = append(out, tok)
		}
	}
	return out
}
