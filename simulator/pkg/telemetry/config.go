// Package telemetry maps the emulator's snapshots to OpenTelemetry and
// exports them: hardware metrics under the hw.* names of the semantic
// conventions v1.44.0 and Joulie's facility metrics as OTLP JSON
// (MetricsRequest, Pusher), and hardware and node events as log records
// with an eventName (HardwareEvents, Sink). Names come from pkg/otel/semconv
// and the encoding from pkg/otel/otlpjson; no OpenTelemetry SDK is used.
//
// Each emulated node is one resource, identified by service.instance.id,
// k8s.node.name and host.name, all equal to the node name, so that
// Prometheus' OTLP receiver gives its series instance=<node>. The facility
// is a resource of its own. Only readings a collector on a real machine
// could measure are exported: a hypervisor guest exports no hw.* metric.
package telemetry

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
)

// Environment variables the simulator's telemetry reads, besides the OTEL_*
// variables of otlpjson.ConfigFromEnv and ResourceFromEnv.
const (
	// EnvFileDir is where events.jsonl, and metrics.jsonl with
	// EnvFileMetrics, are appended. Unset, it is the simulator's debug
	// persistence directory; set and empty, no file is written.
	EnvFileDir = "SIM_OTEL_FILE_DIR"
	// EnvFileMetrics also appends every metrics export to metrics.jsonl.
	EnvFileMetrics = "SIM_OTEL_FILE_METRICS"
	// EnvPodName is the simulator's own service.instance.id; the hostname
	// when unset.
	EnvPodName = "POD_NAME"
)

// Config is the simulator's telemetry configuration: where OTLP goes and
// which files are written.
type Config struct {
	Exporter    otlpjson.ExporterConfig
	FileDir     string
	FileMetrics bool
}

// ConfigFromEnv reads the telemetry configuration through lookup, normally
// os.LookupEnv. persistDir is the directory the simulator resolved for
// SIM_DEBUG_PERSIST_DIR, which is where the files go when EnvFileDir is
// unset. The warnings are those of otlpjson.ConfigFromEnv, values that are
// ignored, for the caller to log; every invalid variable is joined in err,
// which the caller treats as fatal.
func ConfigFromEnv(lookup func(string) (string, bool), persistDir string) (Config, []string, error) {
	getenv := func(k string) string {
		v, _ := lookup(k)
		return v
	}
	var errs []error
	exp, warnings, err := otlpjson.ConfigFromEnv(getenv)
	if err != nil {
		errs = append(errs, err)
	}
	c := Config{Exporter: exp, FileDir: strings.TrimSpace(persistDir)}
	if v, ok := lookup(EnvFileDir); ok {
		c.FileDir = strings.TrimSpace(v)
	}
	if v := strings.TrimSpace(getenv(EnvFileMetrics)); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("telemetry: %s=%q is not a boolean", EnvFileMetrics, v))
		}
		c.FileMetrics = b
	}
	return c, warnings, errors.Join(errs...)
}

// Values of the result label of joulie_sim_otlp_requests_total.
const (
	resultSuccess = "success"
	resultPartial = "partial"
	resultError   = "error"
	resultDropped = "dropped"
)

// telemetryMetrics are the export's own metrics. The Pusher and the Sink
// share them: the second registration finds the first one's collectors.
type telemetryMetrics struct {
	requests *prometheus.CounterVec
	points   *prometheus.CounterVec
	dropped  *prometheus.CounterVec
	events   *prometheus.CounterVec
}

func newTelemetryMetrics(reg prometheus.Registerer) *telemetryMetrics {
	m := &telemetryMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_sim_otlp_requests_total",
			Help: "OTLP exports by signal and result: success, partial (some points rejected), error, or dropped (logs queue full)",
		}, []string{"signal", "result"}),
		points: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_sim_otlp_points_total",
			Help: "Data points (metrics) or log records (logs) in OTLP exports the receiver accepted",
		}, []string{"signal"}),
		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_sim_otlp_dropped_points_total",
			Help: "Values left out of OTLP exports because they were NaN, infinite or out of the int64 range",
		}, []string{"signal"}),
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "joulie_sim_events_total",
			Help: "Events the simulator emitted, by eventName",
		}, []string{"event"}),
	}
	if reg != nil {
		m.requests = telemetryRegister(reg, m.requests)
		m.points = telemetryRegister(reg, m.points)
		m.dropped = telemetryRegister(reg, m.dropped)
		m.events = telemetryRegister(reg, m.events)
	}
	return m
}

// telemetryRegister registers c, or returns the collector already
// registered under its name.
func telemetryRegister(reg prometheus.Registerer, c *prometheus.CounterVec) *prometheus.CounterVec {
	err := reg.Register(c)
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		if existing, ok := are.ExistingCollector.(*prometheus.CounterVec); ok {
			return existing
		}
	}
	return c
}

// countPoints is the number of data points in d.
func countPoints(d otlpjson.MetricsData) int {
	n := 0
	for _, rm := range d.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch {
				case m.Sum != nil:
					n += len(m.Sum.DataPoints)
				case m.Gauge != nil:
					n += len(m.Gauge.DataPoints)
				case m.Histogram != nil:
					n += len(m.Histogram.DataPoints)
				}
			}
		}
	}
	return n
}

// countRecords is the number of log records in d.
func countRecords(d otlpjson.LogsData) int {
	n := 0
	for _, rl := range d.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			n += len(sl.LogRecords)
		}
	}
	return n
}

// recordExport counts one export of n points and its result.
func (m *telemetryMetrics) recordExport(s otlpjson.Signal, n int, err error) {
	var perr *otlpjson.PartialSuccessError
	switch {
	case err == nil:
		m.requests.WithLabelValues(string(s), resultSuccess).Inc()
		m.points.WithLabelValues(string(s)).Add(float64(n))
	case errors.As(err, &perr):
		m.requests.WithLabelValues(string(s), resultPartial).Inc()
		if accepted := int64(n) - perr.Rejected; accepted > 0 {
			m.points.WithLabelValues(string(s)).Add(float64(accepted))
		}
	default:
		m.requests.WithLabelValues(string(s), resultError).Inc()
	}
}
