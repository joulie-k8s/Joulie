package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
)

// testBigSnapshot is a snapshot of n copies of the Intel node of
// testSnapshot, named node-000 on.
func testBigSnapshot(n int, seq uint64) *emulate.Snapshot {
	s := testSnapshot()
	intel := s.Nodes[0]
	s.Seq = seq
	s.Nodes = nil
	for i := 0; i < n; i++ {
		node := intel
		node.Name = fmt.Sprintf("node-%03d", i)
		s.Nodes = append(s.Nodes, node)
	}
	return s
}

// TM10: the Pusher exports nothing before the first snapshot, then once per
// interval at most 64 node resources per request with the facility in the
// first; it exports the latest snapshot on every tick, an unchanged one
// too, as a periodic reader with cumulative temporality does; each request
// also lands in metrics.jsonl when the Sink writes it.
func TestTM10PusherBatchesOncePerInterval(t *testing.T) {
	recv, srv := newTestReceiver(http.StatusOK)
	defer srv.Close()
	var mu sync.Mutex
	var latest *emulate.Snapshot
	calls := 0 // of Latest, which every tick reads
	dir := t.TempDir()
	reg := prometheus.NewRegistry()
	sink, err := NewSink(SinkConfig{Dir: dir, Metrics: true, Cluster: "smoke", Resource: testResource(), Registerer: reg})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPusher(PusherConfig{
		Latest:     func() *emulate.Snapshot { mu.Lock(); defer mu.Unlock(); calls++; return latest },
		Resource:   testResource(),
		Exporter:   testExporter(t, srv.URL+"/api/v1/otlp/v1/metrics", ""),
		Sink:       sink,
		Interval:   time.Hour,
		Registerer: reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	tick := make(chan time.Time)
	p.newTicker = func(d time.Duration) (<-chan time.Time, func()) {
		if d != time.Hour {
			t.Errorf("ticker interval %v, want the configured hour", d)
		}
		return tick, func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	const path = "/api/v1/otlp/v1/metrics"

	tick <- testNow
	tick <- testNow
	// The second tick has read no snapshot before the first is published.
	testWait(t, "two ticks", func() bool { mu.Lock(); defer mu.Unlock(); return calls == 2 })
	if n := len(recv.got(path)); n != 0 {
		t.Fatalf("%d requests before the first snapshot", n)
	}

	// step ticks once and waits until the requests of that tick have been
	// answered, so none is in flight when the test cancels Run.
	step := func(want int, what string) {
		t.Helper()
		tick <- testNow
		testWait(t, what, func() bool {
			return testutil.ToFloat64(p.m.requests.WithLabelValues("metrics", resultSuccess)) >= float64(want)
		})
	}
	mu.Lock()
	latest = testBigSnapshot(130, 1)
	mu.Unlock()
	step(3, "the 3 requests of 130 nodes")
	bodies := recv.got(path)
	if len(bodies) != 3 {
		t.Fatalf("%d requests for 130 nodes, want 3", len(bodies))
	}
	for i, want := range []struct{ nodes, facility int }{{64, 1}, {64, 0}, {2, 0}} {
		d, err := otlpjson.DecodeMetricsStrict(bodies[i])
		if err != nil {
			t.Fatal(err)
		}
		nodes, facility := 0, 0
		for _, rm := range d.ResourceMetrics {
			if testAttrs(rm.Resource.Attributes)[semconv.AttrServiceInstanceID] == FacilityInstance {
				facility++
			} else {
				nodes++
			}
		}
		if nodes != want.nodes || facility != want.facility {
			t.Errorf("request %d: %d nodes and %d facility resources, want %d and %d", i+1, nodes, facility, want.nodes, want.facility)
		}
	}

	step(6, "the same snapshot again")
	mu.Lock()
	latest = testBigSnapshot(130, 2)
	mu.Unlock()
	step(9, "a second snapshot")
	cancel()
	<-done
	if n := len(recv.got(path)); n != 12 {
		t.Errorf("%d requests after Run stopped, want 12: 3 per tick and 3 more as it stops", n)
	}
	sink.Close()
	lines := testLines(t, filepath.Join(dir, MetricsFile))
	if len(lines) != 12 {
		t.Errorf("metrics.jsonl has %d lines, want 12", len(lines))
	}
	for i, l := range lines {
		if _, err := otlpjson.DecodeMetricsStrict(l); err != nil {
			t.Errorf("metrics.jsonl line %d: %v", i+1, err)
		}
	}
	if n := testutil.ToFloat64(p.m.requests.WithLabelValues("metrics", resultSuccess)); n != 12 {
		t.Errorf("joulie_sim_otlp_requests_total{signal=metrics,result=success} = %v, want 12", n)
	}
	all, _ := MetricsRequest(testBigSnapshot(130, 1), testResource())
	if n, want := testutil.ToFloat64(p.m.points.WithLabelValues("metrics")), 4*countPoints(all); n != float64(want) {
		t.Errorf("joulie_sim_otlp_points_total{signal=metrics} = %v, want %d", n, want)
	}
}

// The Pusher counts the values it drops, and an export that fails is
// counted as an error and reported by PushOnce; without an endpoint it
// still writes the file.
func TestPusherCountsDroppedAndFailed(t *testing.T) {
	recv, srv := newTestReceiver(http.StatusBadRequest)
	defer srv.Close()
	snap := testSnapshot()
	snap.Nodes[0].Host.PowerW = math.NaN()
	reg := prometheus.NewRegistry()
	p, err := NewPusher(PusherConfig{Latest: func() *emulate.Snapshot { return snap }, Resource: testResource(),
		Exporter: testExporter(t, srv.URL+"/v1/metrics", ""), Registerer: reg})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.PushOnce(context.Background()); err == nil {
		t.Error("PushOnce hid a 400")
	}
	if n := testutil.ToFloat64(p.m.dropped.WithLabelValues("metrics")); n != 1 {
		t.Errorf("joulie_sim_otlp_dropped_points_total = %v, want 1", n)
	}
	if n := testutil.ToFloat64(p.m.requests.WithLabelValues("metrics", resultError)); n != 1 || len(recv.got("/v1/metrics")) != 1 {
		t.Errorf("error count %v after %d requests, want 1 and 1", n, len(recv.got("/v1/metrics")))
	}

	dir := t.TempDir()
	sink, err := NewSink(SinkConfig{Dir: dir, Metrics: true, Registerer: reg})
	if err != nil {
		t.Fatal(err)
	}
	p, err = NewPusher(PusherConfig{Latest: func() *emulate.Snapshot { return snap }, Resource: testResource(), Sink: sink, Registerer: reg})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.PushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	sink.Close()
	if n := len(testLines(t, filepath.Join(dir, MetricsFile))); n != 1 {
		t.Errorf("metrics.jsonl has %d lines without an endpoint, want 1", n)
	}
	if _, err := NewPusher(PusherConfig{}); err == nil {
		t.Error("NewPusher accepted a config without Latest")
	}
}

// Run exports once more as it stops, with a deadline of its own, so a
// snapshot published after the last tick still leaves; a context that
// already ended would send nothing.
func TestPusherExportsOnceMoreAsItStops(t *testing.T) {
	recv, srv := newTestReceiver(http.StatusOK)
	defer srv.Close()
	snap := testSnapshot()
	p, err := NewPusher(PusherConfig{Latest: func() *emulate.Snapshot { return snap }, Resource: testResource(),
		Exporter: testExporter(t, srv.URL+"/v1/metrics", ""), Interval: time.Hour, Registerer: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	p.newTicker = func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Run(ctx)
	if n := len(recv.got("/v1/metrics")); n != 1 {
		t.Errorf("%d requests after Run stopped without a tick, want the final 1", n)
	}
}

// A partial success counts as result=partial, and only the points the
// receiver accepted reach joulie_sim_otlp_points_total.
func TestPusherCountsPartialSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"partialSuccess":{"rejectedDataPoints":"3","errorMessage":"out of order"}}`)
	}))
	defer srv.Close()
	snap := testSnapshot()
	p, err := NewPusher(PusherConfig{Latest: func() *emulate.Snapshot { return snap }, Resource: testResource(),
		Exporter: testExporter(t, srv.URL+"/v1/metrics", ""), Registerer: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	var perr *otlpjson.PartialSuccessError
	if err := p.PushOnce(context.Background()); !errors.As(err, &perr) {
		t.Fatalf("PushOnce = %v, want the partial success", err)
	}
	req, _ := MetricsRequest(snap, testResource())
	if n, want := testutil.ToFloat64(p.m.points.WithLabelValues("metrics")), countPoints(req)-3; n != float64(want) {
		t.Errorf("joulie_sim_otlp_points_total{signal=metrics} = %v, want %d accepted", n, want)
	}
	partial := testutil.ToFloat64(p.m.requests.WithLabelValues("metrics", resultPartial))
	success := testutil.ToFloat64(p.m.requests.WithLabelValues("metrics", resultSuccess))
	if partial != 1 || success != 0 {
		t.Errorf("requests partial %v, success %v; want 1 and 0", partial, success)
	}
}

// A failing export is logged at most once a minute, however often it fails.
func TestPusherLogsFailuresOnceAMinute(t *testing.T) {
	var logs []string
	p, err := NewPusher(PusherConfig{Latest: func() *emulate.Snapshot { return nil },
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	if err != nil {
		t.Fatal(err)
	}
	p.report(nil)
	for i := 0; i < 3; i++ {
		p.report(errors.New("receiver down"))
	}
	if len(logs) != 1 {
		t.Fatalf("%d logs for 3 failures within a minute, want 1: %q", len(logs), logs)
	}
	p.lastLog = time.Now().Add(-time.Minute)
	p.report(errors.New("receiver down"))
	if len(logs) != 2 {
		t.Errorf("%d logs after a minute, want 2", len(logs))
	}
}

// testPushedSums decodes one export and indexes its Sum points by resource,
// metric and attributes.
func testPushedSums(t *testing.T, body []byte) map[string]otlpjson.NumberDataPoint {
	t.Helper()
	d, err := otlpjson.DecodeMetricsStrict(body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]otlpjson.NumberDataPoint{}
	for id, rm := range testResourceByInstance(t, d) {
		for name, m := range testMetrics(t, rm) {
			if m.Sum == nil {
				continue
			}
			for _, p := range m.Sum.DataPoints {
				out[id+" "+name+" "+mustJSON(testAttrs(p.Attributes))] = p
			}
		}
	}
	return out
}

func testPointValue(p otlpjson.NumberDataPoint) float64 {
	if p.AsInt != nil {
		return float64(*p.AsInt)
	}
	if p.AsDouble != nil {
		return *p.AsDouble
	}
	return math.NaN()
}

// TM13: with the Pusher's clock, a snapshot taken at t0 and pushed at
// t0+900ms carries every point, node and facility, at t0+900ms; the export
// is otherwise the snapshot's own export: the same values, and counters
// from their ledger's start.
func TestTM13PusherPointsCarryThePushTime(t *testing.T) {
	recv, srv := newTestReceiver(http.StatusOK)
	defer srv.Close()
	snap := testSnapshot()
	p, err := NewPusher(PusherConfig{Latest: func() *emulate.Snapshot { return snap }, Resource: testResource(),
		Exporter: testExporter(t, srv.URL+"/v1/metrics", ""), Registerer: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	pushed := snap.Time.Add(900 * time.Millisecond)
	p.now = func() time.Time { return pushed }
	if err := p.PushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	bodies := recv.got("/v1/metrics")
	if len(bodies) != 1 {
		t.Fatalf("%d requests, want 1", len(bodies))
	}
	got, err := otlpjson.DecodeMetricsStrict(bodies[0])
	if err != nil {
		t.Fatal(err)
	}
	want, _ := MetricsRequest(snap, testResource())
	for _, rm := range want.ResourceMetrics {
		for _, m := range rm.ScopeMetrics[0].Metrics {
			ps := testPoints(m)
			for k := range ps {
				if ps[k].TimeUnixNano != metricsUnixNano(snap.Time) {
					t.Fatalf("MetricsRequest %s: timeUnixNano %d, want the snapshot time", m.Name, ps[k].TimeUnixNano)
				}
				ps[k].TimeUnixNano = metricsUnixNano(pushed)
			}
		}
	}
	// Compare what goes on the wire on both sides.
	want, err = otlpjson.DecodeMetricsStrict([]byte(mustJSON(want)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pushed export differs from the snapshot's export at the push time:\n got %s\nwant %s", mustJSON(got), mustJSON(want))
	}
	fac := testMetrics(t, testResourceByInstance(t, got)[FacilityInstance])[semconv.JoulieFacilityITEnergy.Name]
	if pts := testPoints(fac); len(pts) != 1 || pts[0].TimeUnixNano != metricsUnixNano(pushed) || pts[0].StartTimeUnixNano != metricsUnixNano(testFacStart) {
		t.Errorf("facility IT energy points %+v, want one at the push time from the facility start", pts)
	}
}

// TM14: counters stay monotonic when the push time advances and the
// snapshot does not: an unchanged snapshot is exported again at the later
// time with the same values and start, and the next snapshot's counters
// are not lower.
func TestTM14PusherCountersMonotonicAcrossPushes(t *testing.T) {
	recv, srv := newTestReceiver(http.StatusOK)
	defer srv.Close()
	var mu sync.Mutex
	latest := testSnapshot()
	p, err := NewPusher(PusherConfig{Latest: func() *emulate.Snapshot { mu.Lock(); defer mu.Unlock(); return latest },
		Resource: testResource(), Exporter: testExporter(t, srv.URL+"/v1/metrics", ""), Registerer: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	t0 := latest.Time
	var now time.Time
	p.now = func() time.Time { return now }
	for _, at := range []time.Duration{900 * time.Millisecond, 5900 * time.Millisecond, 10900 * time.Millisecond} {
		now = t0.Add(at)
		if at == 10900*time.Millisecond {
			next := testSnapshot()
			next.Seq, next.Time = latest.Seq+1, t0.Add(5*time.Second)
			for i := range next.Nodes {
				next.Nodes[i].Host.EnergyJ += 2000
				for k := range next.Nodes[i].Components {
					next.Nodes[i].Components[k].EnergyJ += 900
				}
			}
			next.Facility.ITEnergyJ += 8000
			next.Facility.CoolingEnergyJ += 1500
			for i := range next.Facility.Racks {
				next.Facility.Racks[i].EnergyJ += 4000
			}
			mu.Lock()
			latest = next
			mu.Unlock()
		}
		if err := p.PushOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	bodies := recv.got("/v1/metrics")
	if len(bodies) != 3 {
		t.Fatalf("%d requests for 3 pushes, want 3: an unchanged snapshot is exported again", len(bodies))
	}
	pushes := make([]map[string]otlpjson.NumberDataPoint, len(bodies))
	for i, b := range bodies {
		pushes[i] = testPushedSums(t, b)
	}
	if len(pushes[0]) == 0 {
		t.Fatal("the export has no counter")
	}
	for i := 1; i < len(pushes); i++ {
		if len(pushes[i]) != len(pushes[0]) {
			t.Fatalf("push %d has %d counter points, push 1 has %d", i+1, len(pushes[i]), len(pushes[0]))
		}
		for k, cur := range pushes[i] {
			old, ok := pushes[i-1][k]
			if !ok {
				t.Fatalf("push %d: %s is new", i+1, k)
			}
			if cur.TimeUnixNano <= old.TimeUnixNano {
				t.Errorf("push %d: %s at %d, not after the previous push's %d", i+1, k, cur.TimeUnixNano, old.TimeUnixNano)
			}
			if cur.StartTimeUnixNano != old.StartTimeUnixNano {
				t.Errorf("push %d: %s starts at %d, was %d", i+1, k, cur.StartTimeUnixNano, old.StartTimeUnixNano)
			}
			if v, w := testPointValue(cur), testPointValue(old); v < w || (i == 1 && v != w) {
				t.Errorf("push %d: %s = %v after %v; want the same value for the same snapshot and never lower", i+1, k, v, w)
			}
		}
	}
}
