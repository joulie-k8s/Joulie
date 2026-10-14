package control

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

const otlpTestSec = uint64(1e9)

// otlpTestServer answers the i-th request with bodies[i], repeating the last
// one, and records the paths it was asked for. The readers of its endpoints
// start without state, even when an earlier server of the test binary had
// the same address.
func otlpTestServer(t *testing.T, bodies ...[]byte) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := len(paths)
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if i >= len(bodies) {
			i = len(bodies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bodies[i])
	}))
	t.Cleanup(srv.Close)
	otlpTestForget(srv.URL)
	t.Cleanup(func() { otlpTestForget(srv.URL) })
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// otlpTestForget drops the reader state of base and of every endpoint below
// it, and of no other server: http://127.0.0.1:4000 leaves
// http://127.0.0.1:40001 alone.
func otlpTestForget(base string) {
	httpPowerStates.Range(func(k, _ any) bool {
		if e := k.(httpPowerKey).endpoint; e == base || strings.HasPrefix(e, base+"/") {
			httpPowerStates.Delete(k)
		}
		return true
	})
}

// otlpTestBody encodes one resource of node with the given metrics; an empty
// node leaves k8s.node.name out.
func otlpTestBody(t *testing.T, node string, metrics ...otlpjson.Metric) []byte {
	t.Helper()
	return otlpTestBodyOf(t, otlpTestResource(node, metrics...))
}

// otlpTestBodyOf encodes one export with the given resources.
func otlpTestBodyOf(t *testing.T, rs ...otlpjson.ResourceMetrics) []byte {
	t.Helper()
	b, err := json.Marshal(otlpjson.MetricsData{ResourceMetrics: rs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// otlpTestResource is one resource of node with the given metrics; an empty
// node leaves k8s.node.name out.
func otlpTestResource(node string, metrics ...otlpjson.Metric) otlpjson.ResourceMetrics {
	var attrs []otlpjson.KeyValue
	if node != "" {
		attrs = append(attrs, otlpjson.String(semconv.AttrK8sNodeName, node), otlpjson.String(semconv.AttrServiceInstanceID, node))
	}
	return otlpjson.ResourceMetrics{
		Resource:     otlpjson.Resource{Attributes: attrs},
		ScopeMetrics: []otlpjson.ScopeMetrics{{Scope: otlpjson.Scope{Name: "test"}, Metrics: metrics}},
	}
}

func otlpTestComp(hwType, id string) []otlpjson.KeyValue {
	return []otlpjson.KeyValue{otlpjson.String(semconv.AttrHwID, id), otlpjson.String(semconv.AttrHwType, hwType)}
}

// otlpTestEnergy is a cumulative hw.energy point of one component, counted
// from 1 s.
func otlpTestEnergy(hwType, id string, atSec uint64, joules float64) otlpjson.Metric {
	return otlpTestEnergyFrom(hwType, id, 1, atSec, joules)
}

// otlpTestEnergyFrom is a cumulative hw.energy point of one component,
// counted from startSec.
func otlpTestEnergyFrom(hwType, id string, startSec, atSec uint64, joules float64) otlpjson.Metric {
	return otlpjson.Metric{Name: semconv.HwEnergy.Name, Unit: "J", Sum: &otlpjson.Sum{
		AggregationTemporality: otlpjson.AggregationTemporalityCumulative, IsMonotonic: true,
		DataPoints: []otlpjson.NumberDataPoint{{
			Attributes:        otlpTestComp(hwType, id),
			StartTimeUnixNano: otlpjson.Uint64String(startSec * otlpTestSec),
			TimeUnixNano:      otlpjson.Uint64String(atSec * otlpTestSec),
			AsDouble:          &joules,
		}},
	}}
}

// otlpTestPower is an hw.power gauge point of one component.
func otlpTestPower(hwType, id string, watts float64) otlpjson.Metric {
	return otlpjson.Metric{Name: semconv.HwPower.Name, Unit: "W", Gauge: &otlpjson.Gauge{DataPoints: []otlpjson.NumberDataPoint{{
		Attributes: otlpTestComp(hwType, id), TimeUnixNano: otlpjson.Uint64String(otlpTestSec), AsDouble: &watts,
	}}}}
}

// otlpTestRead reads once the way the agent does: cmd/agent builds a new
// HTTPPowerReader on every reconcile (setDVFSPowerReaderForTelemetry), so
// each read goes through a fresh reader with the same endpoint and node.
func otlpTestRead(t *testing.T, srv *httptest.Server, endpoint, node, step string, wantW float64, wantOK bool) {
	t.Helper()
	r := &HTTPPowerReader{Endpoint: endpoint, NodeName: node, Client: srv.Client()}
	w, ok, err := r.ReadPowerWatts()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", step, err)
	}
	if ok != wantOK || (wantOK && w != wantW) {
		t.Errorf("%s: ReadPowerWatts = (%v, %v), want (%v, %v)", step, w, ok, wantW, wantOK)
	}
}

// Successive energy counters give the rate between the producer's
// timestamps: 1000 J then 2500 J ten seconds later is 150 W. The first point
// and the point after a reset return not ok, a reset being a decrease or a
// new start time, also from a producer that restarted with its clock behind.
// Every read uses a fresh reader, as the agent's reconciles do, so an
// energy-only producer warms up only if the previous points outlive the
// reader.
func TestHTTPPowerReaderOTLPEnergyDelta(t *testing.T) {
	cpu := semconv.HwTypeCPU
	srv, _ := otlpTestServer(t,
		otlpTestBody(t, "node-a", otlpTestEnergy(cpu, "cpu0", 100, 1000)),
		otlpTestBody(t, "node-a", otlpTestEnergy(cpu, "cpu0", 110, 2500)),
		otlpTestBody(t, "node-a", otlpTestEnergy(cpu, "cpu0", 120, 400)),
		otlpTestBody(t, "node-a", otlpTestEnergy(cpu, "cpu0", 130, 1400)),
		otlpTestBody(t, "node-a", otlpTestEnergyFrom(cpu, "cpu0", 120, 125, 10)),
		otlpTestBody(t, "node-a", otlpTestEnergyFrom(cpu, "cpu0", 120, 128, 40)),
	)
	endpoint := srv.URL + "/otlp/telemetry/{node}"
	otlpTestRead(t, srv, endpoint, "node-a", "warm-up", 0, false)
	otlpTestRead(t, srv, endpoint, "node-a", "rate", 150, true)
	otlpTestRead(t, srv, endpoint, "node-a", "reset", 0, false)
	otlpTestRead(t, srv, endpoint, "node-a", "after the reset", 100, true)
	otlpTestRead(t, srv, endpoint, "node-a", "restart with the clock behind", 0, false)
	otlpTestRead(t, srv, endpoint, "node-a", "after the restart", 10, true)
}

// A producer that sends hw.energy and hw.power, as the emulated simulator
// does, is read from the gauge on the first reconcile and from the energy
// rate after: 1000 J then 2500 J over 10 s is 150 W, not the 170 W gauge.
// With the energy points lost between reconciles it would read the gauge
// forever.
func TestHTTPPowerReaderOTLPEnergyRateOverGauge(t *testing.T) {
	cpu := semconv.HwTypeCPU
	srv, _ := otlpTestServer(t,
		otlpTestBody(t, "node-a", otlpTestEnergy(cpu, "cpu0", 100, 1000), otlpTestPower(cpu, "cpu0", 140)),
		otlpTestBody(t, "node-a", otlpTestEnergy(cpu, "cpu0", 110, 2500), otlpTestPower(cpu, "cpu0", 170)),
	)
	otlpTestRead(t, srv, srv.URL, "node-a", "gauge during warm-up", 140, true)
	otlpTestRead(t, srv, srv.URL, "node-a", "energy rate", 150, true)
}

// One component with a gauge and one whose energy series is still cold give
// not ok: a partial sum would under-read the node.
func TestHTTPPowerReaderOTLPPartialWarmupNotOK(t *testing.T) {
	cpu := semconv.HwTypeCPU
	srv, _ := otlpTestServer(t,
		otlpTestBody(t, "node-a", otlpTestPower(cpu, "cpu0", 100), otlpTestEnergy(cpu, "cpu1", 100, 500)),
		otlpTestBody(t, "node-a", otlpTestPower(cpu, "cpu0", 100), otlpTestEnergy(cpu, "cpu1", 110, 1500)),
	)
	otlpTestRead(t, srv, srv.URL, "node-a", "cpu1 cold", 0, false)
	otlpTestRead(t, srv, srv.URL, "node-a", "both", 200, true)
}

// A resource named for another node is not read; one without a node name is.
func TestHTTPPowerReaderFiltersByNodeResource(t *testing.T) {
	cpu := semconv.HwTypeCPU
	both, _ := otlpTestServer(t, otlpTestBodyOf(t,
		otlpTestResource("node-a", otlpTestPower(cpu, "cpu0", 150)),
		otlpTestResource("node-b", otlpTestPower(cpu, "cpu0", 999)),
	))
	otlpTestRead(t, both, both.URL, "node-a", "own and other node", 150, true)

	other, _ := otlpTestServer(t, otlpTestBody(t, "node-b", otlpTestPower(cpu, "cpu0", 999)))
	otlpTestRead(t, other, other.URL, "node-a", "other node", 0, false)

	unnamed, paths := otlpTestServer(t, otlpTestBody(t, "", otlpTestPower(cpu, "cpu0", 120)))
	otlpTestRead(t, unnamed, unnamed.URL+"/otlp/telemetry/{node}", "node-a", "no node name", 120, true)
	if got := paths(); len(got) != 1 || got[0] != "/otlp/telemetry/node-a" {
		t.Errorf("requested paths %v, want [/otlp/telemetry/node-a]", got)
	}
}

// A body longer than otlpjson.MaxTelemetryBody fails, so a misbehaving
// endpoint cannot make the agent hold an unbounded body: here a valid export
// behind more blank space than the limit.
func TestHTTPPowerReaderBodyOverLimitFails(t *testing.T) {
	body := append(bytes.Repeat([]byte(" "), otlpjson.MaxTelemetryBody), otlpTestBody(t, "node-a", otlpTestPower(semconv.HwTypeCPU, "cpu0", 120))...)
	srv, _ := otlpTestServer(t, body)
	r := &HTTPPowerReader{Endpoint: srv.URL, NodeName: "node-a", Client: srv.Client()}
	if w, ok, err := r.ReadPowerWatts(); err == nil || ok {
		t.Errorf("ReadPowerWatts on a %d byte body = (%v, %v, %v), want an error", len(body), w, ok, err)
	}
}

// The legacy packagePowerWatts body is still read, with one deprecation
// warning per endpoint and node, although the agent reads through a new
// reader on every reconcile; an OTLP body logs nothing.
func TestHTTPPowerReaderLegacyJSONWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	legacy, _ := otlpTestServer(t, []byte(`{"packagePowerWatts": 125.5}`))
	for i := 0; i < 3; i++ {
		otlpTestRead(t, legacy, legacy.URL, "node-a", "legacy", 125.5, true)
	}
	if n := strings.Count(buf.String(), "deprecated"); n != 1 {
		t.Errorf("legacy body logged %d deprecation warnings over 3 reconciles, want 1:\n%s", n, buf.String())
	}

	buf.Reset()
	otlp, _ := otlpTestServer(t, otlpTestBody(t, "node-a", otlpTestPower(semconv.HwTypeCPU, "cpu0", 80)))
	otlpTestRead(t, otlp, otlp.URL, "node-a", "otlp", 80, true)
	if buf.Len() != 0 {
		t.Errorf("OTLP body logged %q, want nothing", buf.String())
	}
}

// The top-level resourceMetrics key alone tells the formats apart: a body
// with it is OTLP even next to a legacy key, and a body without it is the
// legacy JSON, with its warning, even when it has no legacy key either.
func TestHTTPPowerReaderDetectsOTLPByResourceMetrics(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	otlp := otlpTestBody(t, "node-a", otlpTestPower(semconv.HwTypeCPU, "cpu0", 150))
	mixed := append(bytes.TrimSuffix(otlp, []byte("}")), []byte(`,"packagePowerWatts":999}`)...)
	srv, _ := otlpTestServer(t, mixed)
	otlpTestRead(t, srv, srv.URL, "node-a", "resourceMetrics next to packagePowerWatts", 150, true)
	if buf.Len() != 0 {
		t.Errorf("OTLP body with a legacy key logged %q, want nothing", buf.String())
	}

	other, _ := otlpTestServer(t, []byte(`{"node": "node-a"}`))
	otlpTestRead(t, other, other.URL, "node-a", "neither key", 0, false)
	if !strings.Contains(buf.String(), "deprecated") {
		t.Errorf("body without resourceMetrics logged %q, want the deprecation warning", buf.String())
	}
}
