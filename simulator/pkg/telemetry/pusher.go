package telemetry

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// pusherFinalTimeout bounds the export Run makes as it stops.
const pusherFinalTimeout = 5 * time.Second

// PusherConfig configures a Pusher.
type PusherConfig struct {
	// Latest returns the newest published snapshot, nil before the first.
	Latest   func() *emulate.Snapshot
	Resource Resource
	// Exporter POSTs to its metrics endpoint; nil, or no endpoint, POSTs
	// nothing.
	Exporter *otlpjson.Exporter
	// Sink receives every export for its MetricsFile; nil writes none.
	Sink *Sink
	// Interval is the export interval, OTEL_METRIC_EXPORT_INTERVAL; 0 means
	// otlpjson.DefaultExportInterval.
	Interval time.Duration
	// MaxNodes bounds the node resources of one request; 0 means
	// MaxNodesPerRequest.
	MaxNodes   int
	Registerer prometheus.Registerer
	// Logf logs failed exports, at most once a minute; nil means log.Printf.
	Logf func(string, ...any)
}

// Pusher exports the latest snapshot once per interval, as a periodic
// metric reader does, with cumulative temporality: at most MaxNodes node
// resources per request plus the facility resource in the first, and
// nothing before the first snapshot. Like a reader that collects and
// exports at once, every point carries the time of the export with the
// values of the latest snapshot, which is what a reader of the hardware
// sees at that moment; a snapshot that has not changed since the last
// export is exported again at the later time.
type Pusher struct {
	c         PusherConfig
	m         *telemetryMetrics
	logf      func(string, ...any)
	newTicker func(time.Duration) (<-chan time.Time, func())
	// now is the clock that stamps the points of an export.
	now func() time.Time

	mu      sync.Mutex
	lastLog time.Time
}

// NewPusher checks c and returns its Pusher.
func NewPusher(c PusherConfig) (*Pusher, error) {
	if c.Latest == nil {
		return nil, errors.New("telemetry: PusherConfig.Latest is nil")
	}
	if c.Interval <= 0 {
		c.Interval = otlpjson.DefaultExportInterval
	}
	if c.MaxNodes <= 0 {
		c.MaxNodes = MaxNodesPerRequest
	}
	p := &Pusher{c: c, m: newTelemetryMetrics(c.Registerer), logf: c.Logf, newTicker: pusherTicker, now: time.Now}
	if p.logf == nil {
		p.logf = log.Printf
	}
	return p, nil
}

func pusherTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Run exports once per interval until ctx ends, then once more with a
// fresh deadline, so the last snapshot before a shutdown is not lost.
func (p *Pusher) Run(ctx context.Context) {
	tick, stop := p.newTicker(p.c.Interval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), pusherFinalTimeout)
			p.report(p.PushOnce(fctx))
			cancel()
			return
		case <-tick:
			p.report(p.PushOnce(ctx))
		}
	}
}

// PushOnce exports the latest snapshot, unless there is none yet, with
// every point at the time it builds the export (E16): points stamped with
// the snapshot time reach the receiver up to a tick old, and Prometheus
// then extrapolates rate() less far past the newest sample, so a pushed
// counter reads low. Counters keep their ledger's start. Each request goes
// to the Sink's MetricsFile and to the metrics endpoint; with neither,
// nothing is built. The error joins every failure.
func (p *Pusher) PushOnce(ctx context.Context) error {
	push := p.c.Exporter.Enabled(otlpjson.SignalMetrics)
	if !push && (p.c.Sink == nil || p.c.Sink.metrics == nil) {
		return nil
	}
	s := p.c.Latest()
	if s == nil {
		return nil
	}
	reqs, dropped := metricsRequests(s, p.c.Resource, p.c.MaxNodes, p.now())
	if dropped > 0 {
		p.m.dropped.WithLabelValues(string(otlpjson.SignalMetrics)).Add(float64(dropped))
	}
	var errs []error
	for _, req := range reqs {
		if err := p.c.Sink.writeMetrics(req); err != nil {
			errs = append(errs, err)
		}
		if !push {
			continue
		}
		err := p.c.Exporter.ExportMetrics(ctx, req)
		p.m.recordExport(otlpjson.SignalMetrics, countPoints(req), err)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// report logs an export error at most once a minute.
func (p *Pusher) report(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	now := time.Now()
	ok := now.Sub(p.lastLog) >= time.Minute
	if ok {
		p.lastLog = now
	}
	p.mu.Unlock()
	if ok {
		p.logf("warning: telemetry: metrics export: %v", err)
	}
}
