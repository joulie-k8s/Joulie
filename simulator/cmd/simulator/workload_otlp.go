package main

// The run recorder: the results of a v1 run as OTLP JSON Lines, in the
// format of the OTel file exporter, under <runs dir>/<runId>/otlp/ (D10,
// D12). simulator.logs.jsonl holds one LogsData per tick that has records:
// the run's and its jobs' events on the run's resource, and copies of the
// emulated nodes' hardware events on each node's resource. Every resource
// carries joulie.sim.run.id. simulator.metrics.jsonl holds one MetricsData
// every SIM_OTLP_FILE_INTERVAL and one when the run ends: the run's
// metrics, cumulative from its first tick, so the last line of the file
// holds every final value.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// Explicit bounds of the run's histograms. assumed: chosen to resolve the
// job lengths of the scenario library (seconds to an hour) and slowdowns
// from none to a few times.
var (
	recorderSecondsBounds  = []float64{1, 5, 10, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200}
	recorderSlowdownBounds = []float64{1, 1.01, 1.02, 1.05, 1.1, 1.2, 1.5, 2, 3, 5}
)

// recorderPair keys a series by two attribute values.
type recorderPair struct{ a, b string }

// recorderEnergyKey keys a joulie.sim.workload.energy series.
type recorderEnergyKey struct{ class, hwType, node, profile string }

// recorderLog is one record before encoding and the node whose resource it
// goes on, "" for the run's own.
type recorderLog struct {
	node string
	rec  otlpjson.LogRecord
}

// runRecorder writes one run's files. It is used under the run's lock.
type runRecorder struct {
	metrics, logs *otlpjson.FileWriter
	res           telemetry.Resource
	cluster       string
	runID         string
	scope         otlpjson.Scope
	start         time.Time
	interval      time.Duration
	lastWrite     time.Time
	pending       []recorderLog
	lastErr       string

	submitted  map[string]int64
	finished   map[recorderPair]int64
	activeSeen map[recorderPair]bool
	work       map[recorderPair]float64
	energy     map[recorderEnergyKey]float64
	duration   map[string]*otlpjson.ExplicitHistogram
	queueWait  map[string]*otlpjson.ExplicitHistogram
	slowdown   map[string]*otlpjson.ExplicitHistogram
}

// newRunRecorder creates dir/OTLPDir and opens the run's two files there.
// interval is the metrics snapshot period.
func newRunRecorder(dir string, res telemetry.Resource, cluster, runID string, interval time.Duration) (*runRecorder, error) {
	otlpDir := filepath.Join(dir, workload.OTLPDir)
	if err := os.MkdirAll(otlpDir, 0o755); err != nil {
		return nil, fmt.Errorf("run %s: %w", runID, err)
	}
	m, err := otlpjson.OpenFile(filepath.Join(otlpDir, workload.SimulatorMetricsFile), otlpjson.SignalMetrics)
	if err != nil {
		return nil, err
	}
	l, err := otlpjson.OpenFile(filepath.Join(otlpDir, workload.SimulatorLogsFile), otlpjson.SignalLogs)
	if err != nil {
		_ = m.Close()
		return nil, err
	}
	if interval <= 0 {
		interval = workload.DefaultOTLPFileInterval
	}
	return &runRecorder{
		metrics: m, logs: l, res: res, cluster: cluster, runID: runID,
		scope:      otlpjson.Scope{Name: workload.ScopeName, Version: res.ServiceVersion},
		interval:   interval,
		submitted:  map[string]int64{},
		finished:   map[recorderPair]int64{},
		activeSeen: map[recorderPair]bool{},
		work:       map[recorderPair]float64{},
		energy:     map[recorderEnergyKey]float64{},
		duration:   map[string]*otlpjson.ExplicitHistogram{},
		queueWait:  map[string]*otlpjson.ExplicitHistogram{},
		slowdown:   map[string]*otlpjson.ExplicitHistogram{},
	}, nil
}

// resource is the resource of node's records, the run's own for "", with
// joulie.sim.run.id added.
func (r *runRecorder) resource(node string) otlpjson.Resource {
	res := r.res.Simulator(r.cluster)
	if node != "" {
		res = r.res.Node(node, r.cluster)
	}
	res.Attributes = append(res.Attributes, otlpjson.String(semconv.AttrSimRunID, r.runID))
	return res
}

// event queues one record of e at time at on the run's resource.
func (r *runRecorder) event(e semconv.Event, at time.Time, body string, attrs ...otlpjson.KeyValue) {
	r.pending = append(r.pending, recorderLog{rec: otlpjson.EventRecord(e, at, at, body, attrs...)})
}

// hardware queues copies of the emulated nodes' hardware events, each on
// its node's resource.
func (r *runRecorder) hardware(recs []telemetry.EventRecord, observed time.Time) {
	for _, h := range recs {
		r.pending = append(r.pending, recorderLog{node: h.Node, rec: otlpjson.EventRecord(h.Event, h.Time, observed, h.Body, h.Attributes...)})
	}
}

// flushLogs writes the queued records as one LogsData, one resource per
// node in the order the nodes first appear.
func (r *runRecorder) flushLogs() {
	if len(r.pending) == 0 {
		return
	}
	var order []string
	byNode := map[string][]otlpjson.LogRecord{}
	for _, p := range r.pending {
		if _, ok := byNode[p.node]; !ok {
			order = append(order, p.node)
		}
		byNode[p.node] = append(byNode[p.node], p.rec)
	}
	d := otlpjson.LogsData{ResourceLogs: []otlpjson.ResourceLogs{}}
	for _, node := range order {
		d.ResourceLogs = append(d.ResourceLogs, otlpjson.NewLogsData(r.resource(node), r.scope, byNode[node]...).ResourceLogs...)
	}
	r.pending = r.pending[:0]
	r.report(r.logs.WriteLogs(d))
}

func (r *runRecorder) addSubmitted(class string) { r.submitted[class]++ }

func (r *runRecorder) addFinished(class, outcome string) {
	r.finished[recorderPair{class, outcome}]++
}

func (r *runRecorder) addWork(class, kind string, v float64) {
	if v > 0 {
		r.work[recorderPair{class, kind}] += v
	}
}

func (r *runRecorder) addEnergy(class, hwType, node, profile string, j float64) {
	if j > 0 {
		r.energy[recorderEnergyKey{class, hwType, node, profile}] += j
	}
}

// observe records v in the histogram of class in h.
func (r *runRecorder) observe(h map[string]*otlpjson.ExplicitHistogram, bounds []float64, class string, v float64) {
	x := h[class]
	if x == nil {
		x = otlpjson.NewExplicitHistogram(bounds...)
		h[class] = x
	}
	x.Record(v)
}

// maybeWriteMetrics writes a metrics snapshot at now when the interval has
// passed since the last one, or when force is set. active is the number of
// active jobs by class and state at now.
func (r *runRecorder) maybeWriteMetrics(now time.Time, active map[recorderPair]int64, force bool) {
	if !force && !r.lastWrite.IsZero() && now.Sub(r.lastWrite) < r.interval {
		return
	}
	r.lastWrite = now
	r.report(r.metrics.WriteMetrics(otlpjson.NewMetricsData(r.resource(""), r.scope, r.snapshot(now, active)...)))
}

// snapshot is every metric of the run at now, with points in a fixed order.
func (r *runRecorder) snapshot(now time.Time, active map[recorderPair]int64) []otlpjson.Metric {
	start := r.start
	class := func(c string) otlpjson.KeyValue { return otlpjson.String(semconv.AttrJoulieWorkloadClass, c) }

	var submitted []otlpjson.NumberDataPoint
	for _, c := range recorderSortedKeys(r.submitted) {
		submitted = append(submitted, otlpjson.IntPoint(start, now, r.submitted[c], class(c)))
	}
	var finished []otlpjson.NumberDataPoint
	for _, k := range recorderSortedPairs(r.finished) {
		finished = append(finished, otlpjson.IntPoint(start, now, r.finished[k], class(k.a), otlpjson.String(semconv.AttrSimJobOutcome, k.b)))
	}
	for k := range active {
		r.activeSeen[k] = true
	}
	var act []otlpjson.NumberDataPoint
	for _, k := range recorderSortedPairs(r.activeSeen) {
		act = append(act, otlpjson.IntPoint(start, now, active[k], class(k.a), otlpjson.String(semconv.AttrSimJobState, k.b)))
	}
	var work []otlpjson.NumberDataPoint
	for _, k := range recorderSortedPairs(r.work) {
		work = append(work, otlpjson.DoublePoint(start, now, r.work[k], class(k.a), otlpjson.String(semconv.AttrSimWorkKind, k.b)))
	}
	ekeys := make([]recorderEnergyKey, 0, len(r.energy))
	for k := range r.energy {
		ekeys = append(ekeys, k)
	}
	sort.Slice(ekeys, func(i, j int) bool {
		a, b := ekeys[i], ekeys[j]
		if a.node != b.node {
			return a.node < b.node
		}
		if a.class != b.class {
			return a.class < b.class
		}
		return a.hwType < b.hwType
	})
	var energy []otlpjson.NumberDataPoint
	for _, k := range ekeys {
		energy = append(energy, otlpjson.DoublePoint(start, now, r.energy[k], class(k.class),
			otlpjson.String(semconv.AttrHwType, k.hwType), otlpjson.String(semconv.AttrK8sNodeName, k.node),
			otlpjson.String(semconv.AttrSimProfile, k.profile)))
	}
	hist := func(h map[string]*otlpjson.ExplicitHistogram) []otlpjson.HistogramDataPoint {
		var out []otlpjson.HistogramDataPoint
		for _, c := range recorderSortedKeys(h) {
			out = append(out, h[c].Point(start, now, class(c)))
		}
		return out
	}
	return []otlpjson.Metric{
		otlpjson.SumMetric(semconv.JoulieSimWorkloadJobsSubmitted, submitted...),
		otlpjson.SumMetric(semconv.JoulieSimWorkloadJobsFinished, finished...),
		otlpjson.SumMetric(semconv.JoulieSimWorkloadJobsActive, act...),
		otlpjson.HistogramMetric(semconv.JoulieSimWorkloadJobDuration, hist(r.duration)...),
		otlpjson.HistogramMetric(semconv.JoulieSimWorkloadJobQueueWait, hist(r.queueWait)...),
		otlpjson.HistogramMetric(semconv.JoulieSimWorkloadJobSlowdown, hist(r.slowdown)...),
		otlpjson.SumMetric(semconv.JoulieSimWorkloadWork, work...),
		otlpjson.SumMetric(semconv.JoulieSimWorkloadEnergy, energy...),
	}
}

// close writes nothing more and closes both files.
func (r *runRecorder) close() error {
	err1 := r.metrics.Close()
	err2 := r.logs.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// report logs a write failure when it differs from the last one, and the
// recovery.
func (r *runRecorder) report(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg == r.lastErr {
		return
	}
	if msg != "" {
		log.Printf("warning: run %s results: %v (logged when it changes)", r.runID, err)
	} else {
		log.Printf("run %s results are written again", r.runID)
	}
	r.lastErr = msg
}

func recorderSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func recorderSortedPairs[V any](m map[recorderPair]V) []recorderPair {
	out := make([]recorderPair, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].a != out[j].a {
			return out[i].a < out[j].a
		}
		return out[i].b < out[j].b
	})
	return out
}
