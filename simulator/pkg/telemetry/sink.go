package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
)

// Files the Sink writes in its directory, OTLP JSON Lines as the OTel file
// exporter writes them: one LogsData or MetricsData object per line.
const (
	EventsFile  = "events.jsonl"
	MetricsFile = "metrics.jsonl"
)

// defaultLogsQueue is how many log exports wait for the push at most.
const defaultLogsQueue = 256

// SinkConfig configures a Sink.
type SinkConfig struct {
	// Dir receives EventsFile, and MetricsFile when Metrics is set; it is
	// created if needed. Empty writes no file.
	Dir     string
	Metrics bool
	// Cluster is the k8s.cluster.name of every resource.
	Cluster  string
	Resource Resource
	// Exporter pushes the events when it has a logs endpoint; nil pushes
	// nothing.
	Exporter   *otlpjson.Exporter
	Registerer prometheus.Registerer
	// Logf logs warnings; nil means log.Printf.
	Logf func(string, ...any)
	// Now is the observed time of a record; nil means time.Now.
	Now func() time.Time
	// Queue bounds the log exports waiting for the push; 0 means 256.
	Queue int
}

// Sink writes events: every Emit appends one LogsData line to EventsFile
// and, with a logs endpoint, queues it for Run to push. Prometheus has no
// logs receiver, so a 404 from the logs endpoint turns the push off after
// one warning; the file keeps every event. The Pusher appends its metrics
// exports to MetricsFile through the Sink when Metrics is set. A Sink is
// safe for concurrent use.
type Sink struct {
	c       SinkConfig
	m       *telemetryMetrics
	logf    func(string, ...any)
	now     func() time.Time
	events  *otlpjson.FileWriter
	metrics *otlpjson.FileWriter
	queue   chan otlpjson.LogsData

	mu        sync.Mutex
	pushOff   bool // the logs endpoint answered 404
	lastDrop  time.Time
	closeOnce sync.Once
	closeErr  error
}

// NewSink opens the files of c.
func NewSink(c SinkConfig) (*Sink, error) {
	s := &Sink{c: c, m: newTelemetryMetrics(c.Registerer), logf: c.Logf, now: c.Now}
	if s.logf == nil {
		s.logf = log.Printf
	}
	if s.now == nil {
		s.now = time.Now
	}
	if c.Dir != "" {
		if err := os.MkdirAll(c.Dir, 0o755); err != nil {
			return nil, fmt.Errorf("telemetry: %w", err)
		}
		var err error
		if s.events, err = otlpjson.OpenFile(filepath.Join(c.Dir, EventsFile), otlpjson.SignalLogs); err != nil {
			return nil, fmt.Errorf("telemetry: %w", err)
		}
		if c.Metrics {
			if s.metrics, err = otlpjson.OpenFile(filepath.Join(c.Dir, MetricsFile), otlpjson.SignalMetrics); err != nil {
				s.events.Close()
				return nil, fmt.Errorf("telemetry: %w", err)
			}
		}
	}
	if c.Exporter.Enabled(otlpjson.SignalLogs) {
		n := c.Queue
		if n <= 0 {
			n = defaultLogsQueue
		}
		s.queue = make(chan otlpjson.LogsData, n)
	}
	return s, nil
}

// Emit writes recs as one LogsData line and queues it for the push. It
// counts every record in joulie_sim_events_total. The error is the file's;
// a full push queue drops the export from the push only, with a warning at
// most once a minute.
func (s *Sink) Emit(recs ...EventRecord) error {
	if len(recs) == 0 {
		return nil
	}
	d := logsRequest(recs, s.c.Cluster, s.now(), s.c.Resource)
	for _, r := range recs {
		s.m.events.WithLabelValues(r.Event.Name).Inc()
	}
	var err error
	if s.events != nil {
		err = s.events.WriteLogs(d)
	}
	if s.queue != nil && !s.pushDisabled() {
		select {
		case s.queue <- d:
		default:
			s.m.requests.WithLabelValues(string(otlpjson.SignalLogs), resultDropped).Inc()
			s.mu.Lock()
			warn := s.now().Sub(s.lastDrop) >= time.Minute
			if warn {
				s.lastDrop = s.now()
			}
			s.mu.Unlock()
			if warn {
				s.logf("warning: telemetry: logs push queue full, %d records not pushed (still in %s)", len(recs), EventsFile)
			}
		}
	}
	return err
}

// Run pushes queued log exports until ctx ends. Without a logs endpoint it
// returns at once.
func (s *Sink) Run(ctx context.Context) {
	if s.queue == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-s.queue:
			if s.pushDisabled() {
				continue
			}
			err := s.c.Exporter.ExportLogs(ctx, d)
			s.m.recordExport(otlpjson.SignalLogs, countRecords(d), err)
			var herr *otlpjson.HTTPError
			switch {
			case errors.As(err, &herr) && herr.StatusCode == http.StatusNotFound:
				s.mu.Lock()
				s.pushOff = true
				s.mu.Unlock()
				s.logf("warning: telemetry: logs endpoint %s answered 404, so it has no OTLP logs receiver (Prometheus has none); logs push turned off, events stay in %s", herr.URL, EventsFile)
			case err != nil && ctx.Err() == nil:
				s.logf("warning: telemetry: %v", err)
			}
		}
	}
}

func (s *Sink) pushDisabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pushOff
}

// writeMetrics appends a metrics export to MetricsFile, when the Sink
// writes it.
func (s *Sink) writeMetrics(d otlpjson.MetricsData) error {
	if s == nil || s.metrics == nil {
		return nil
	}
	return s.metrics.WriteMetrics(d)
}

// Close closes the files. Exports still queued are not pushed.
func (s *Sink) Close() error {
	s.closeOnce.Do(func() {
		var errs []error
		if s.events != nil {
			errs = append(errs, s.events.Close())
		}
		if s.metrics != nil {
			errs = append(errs, s.metrics.Close())
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}
