package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// testReceiver is an OTLP/HTTP receiver that records each body by path and
// answers status.
type testReceiver struct {
	mu     sync.Mutex
	status int
	bodies map[string][][]byte
}

func newTestReceiver(status int) (*testReceiver, *httptest.Server) {
	r := &testReceiver{status: status, bodies: map[string][][]byte{}}
	return r, httptest.NewServer(r)
}

func (r *testReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies[req.URL.Path] = append(r.bodies[req.URL.Path], b)
	status := r.status
	r.mu.Unlock()
	w.WriteHeader(status)
}

func (r *testReceiver) got(path string) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.bodies[path]...)
}

func testExporter(t *testing.T, metricsURL, logsURL string) *otlpjson.Exporter {
	t.Helper()
	e, err := otlpjson.NewExporter(otlpjson.ExporterConfig{
		Metrics: otlpjson.SignalConfig{Endpoint: metricsURL},
		Logs:    otlpjson.SignalConfig{Endpoint: logsURL},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// testLines reads the lines of a .jsonl file.
func testLines(t *testing.T, path string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		return nil
	}
	return bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
}

// testWait polls cond for up to five seconds.
func testWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// EV04: events.jsonl holds one LogsData per Emit, each line decodes
// strictly, timeUnixNano is the time hwemu saw the event and
// observedTimeUnixNano the time of the Emit.
func TestEV04EventsFileDecodesStrictly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	emitted := testNow.Add(time.Second)
	reg := prometheus.NewRegistry()
	s, err := NewSink(SinkConfig{Dir: dir, Cluster: "smoke", Resource: testResource(), Registerer: reg, Now: func() time.Time { return emitted }})
	if err != nil {
		t.Fatal(err)
	}
	snap := testEventSnapshot()
	recs := HardwareEvents(snap)
	if err := s.Emit(recs...); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit(NodeMismatch("vm-0", semconv.ErrorTypeNodeMissing, "vm-no-powercap", "", testNow)); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, MetricsFile)); !os.IsNotExist(err) {
		t.Errorf("metrics.jsonl exists without SinkConfig.Metrics: %v", err)
	}
	lines := testLines(t, filepath.Join(dir, EventsFile))
	if len(lines) != 2 {
		t.Fatalf("%d lines, want one per non-empty Emit", len(lines))
	}
	var times []otlpjson.Uint64String
	for i, l := range lines {
		d, err := otlpjson.DecodeLogsStrict(l)
		if err != nil {
			t.Fatalf("line %d: %v\n%s", i+1, err, l)
		}
		for _, rl := range d.ResourceLogs {
			for _, lr := range rl.ScopeLogs[0].LogRecords {
				times = append(times, lr.TimeUnixNano)
				if lr.ObservedTimeUnixNano != otlpjson.Uint64String(emitted.UnixNano()) {
					t.Errorf("observedTimeUnixNano %d, want the Emit time", lr.ObservedTimeUnixNano)
				}
			}
		}
	}
	var want []otlpjson.Uint64String
	for _, r := range recs {
		want = append(want, otlpjson.Uint64String(r.Time.UnixNano()))
	}
	want = append(want, otlpjson.Uint64String(testNow.UnixNano()))
	if mustJSON(times) != mustJSON(want) {
		t.Errorf("timeUnixNano %v, want %v", times, want)
	}
	if got := times[1]; got != otlpjson.Uint64String(snap.Nodes[0].Events[0].Time.UnixNano()) {
		t.Errorf("the PL1 event has timeUnixNano %d, want the hwemu event time", got)
	}
	if n := testutil.ToFloat64(s.m.events.WithLabelValues(semconv.EventSimWriteRejected.Name)); n != 2 {
		t.Errorf("joulie_sim_events_total{event=write.rejected} = %v, want 2", n)
	}
	if err := s.Close(); err != nil {
		t.Errorf("a second Close = %v", err)
	}
}

// EV05: logs are pushed only when the exporter has a logs URL: without one
// Run returns at once and nothing reaches the receiver; with one the
// receiver gets exactly what the file holds.
func TestEV05LogsPushedOnlyWithALogsURL(t *testing.T) {
	recv, srv := newTestReceiver(http.StatusOK)
	defer srv.Close()

	dir := t.TempDir()
	s, err := NewSink(SinkConfig{Dir: dir, Cluster: "smoke", Resource: testResource(), Exporter: testExporter(t, srv.URL+"/v1/metrics", "")})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run without a logs URL did not return")
	}
	if err := s.Emit(HardwareEvents(testEventSnapshot())...); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if n := len(testLines(t, filepath.Join(dir, EventsFile))); n != 1 {
		t.Errorf("events.jsonl has %d lines, want 1", n)
	}
	if b := recv.got("/v1/logs"); len(b) != 0 {
		t.Errorf("the receiver got %d logs exports without a logs URL", len(b))
	}

	dir = t.TempDir()
	reg := prometheus.NewRegistry()
	s, err = NewSink(SinkConfig{Dir: dir, Cluster: "smoke", Resource: testResource(), Exporter: testExporter(t, "", srv.URL+"/v1/logs"), Registerer: reg})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	for i := 0; i < 3; i++ {
		if err := s.Emit(HardwareEvents(testEventSnapshot())...); err != nil {
			t.Fatal(err)
		}
	}
	// The receiver sees an export before Run counts it; wait for both.
	testWait(t, "three logs exports", func() bool {
		return len(recv.got("/v1/logs")) == 3 && testutil.ToFloat64(s.m.requests.WithLabelValues("logs", resultSuccess)) == 3
	})
	s.Close()
	lines := testLines(t, filepath.Join(dir, EventsFile))
	for i, b := range recv.got("/v1/logs") {
		if !bytes.Equal(bytes.TrimSpace(b), lines[i]) {
			t.Errorf("export %d differs from line %d of events.jsonl", i+1, i+1)
		}
	}
	if n := testutil.ToFloat64(s.m.requests.WithLabelValues("logs", resultSuccess)); n != 3 {
		t.Errorf("joulie_sim_otlp_requests_total{signal=logs,result=success} = %v, want 3", n)
	}
	if n := testutil.ToFloat64(s.m.points.WithLabelValues("logs")); n != 3*7 {
		t.Errorf("joulie_sim_otlp_points_total{signal=logs} = %v, want 21", n)
	}
}

// TM11: a logs endpoint that answers 404, as Prometheus does, turns the
// logs push off after one warning; the file keeps every event.
func TestTM11LogsNotFoundDisablesPushAfterOneWarning(t *testing.T) {
	recv, srv := newTestReceiver(http.StatusNotFound)
	defer srv.Close()
	var mu sync.Mutex
	var warnings []string
	logf := func(f string, a ...any) {
		mu.Lock()
		warnings = append(warnings, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	dir := t.TempDir()
	reg := prometheus.NewRegistry()
	s, err := NewSink(SinkConfig{Dir: dir, Cluster: "smoke", Resource: testResource(),
		Exporter: testExporter(t, "", srv.URL+"/api/v1/otlp/v1/logs"), Registerer: reg, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	if err := s.Emit(HardwareEvents(testEventSnapshot())...); err != nil {
		t.Fatal(err)
	}
	testWait(t, "the push to turn off with a warning", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return s.pushDisabled() && len(warnings) > 0
	})
	for i := 0; i < 2; i++ {
		if err := s.Emit(HardwareEvents(testEventSnapshot())...); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(s.queue); n != 0 {
		t.Errorf("%d exports queued after the 404", n)
	}
	cancel()
	s.Close()
	if n := len(recv.got("/api/v1/otlp/v1/logs")); n != 1 {
		t.Errorf("the receiver got %d logs exports, want only the first", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warnings) != 1 || !bytes.Contains([]byte(warnings[0]), []byte("404")) {
		t.Errorf("warnings %q, want exactly one about the 404", warnings)
	}
	if n := len(testLines(t, filepath.Join(dir, EventsFile))); n != 3 {
		t.Errorf("events.jsonl has %d lines, want all 3", n)
	}
	if n := testutil.ToFloat64(s.m.requests.WithLabelValues("logs", resultError)); n != 1 {
		t.Errorf("joulie_sim_otlp_requests_total{signal=logs,result=error} = %v, want 1", n)
	}
}

// A full push queue drops the export from the push only, counts it and
// warns once.
func TestSinkFullQueueDropsFromPushOnly(t *testing.T) {
	var warnings int
	dir := t.TempDir()
	s, err := NewSink(SinkConfig{Dir: dir, Exporter: testExporter(t, "", "http://127.0.0.1:1/v1/logs"), Queue: 1,
		Registerer: prometheus.NewRegistry(), Logf: func(string, ...any) { warnings++ }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Emit(NodeMismatch("n", semconv.ErrorTypeNodeMissing, "p", "", testNow)); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if n := testutil.ToFloat64(s.m.requests.WithLabelValues("logs", resultDropped)); n != 2 || warnings != 1 {
		t.Errorf("dropped %v with %d warnings, want 2 and 1", n, warnings)
	}
	if n := len(testLines(t, filepath.Join(dir, EventsFile))); n != 3 {
		t.Errorf("events.jsonl has %d lines, want 3", n)
	}
}
