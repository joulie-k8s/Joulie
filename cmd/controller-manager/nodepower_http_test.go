package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/controller/inputs"
)

// otlpNodePowerBody is an export of two nodes as hw.power and hw.host.power
// gauges. Node n1 has a CPU package at 150 W, two GPUs at 200 W, a DRAM zone
// at 30 W and a host at 700 W; node n2, which must not count for n1, has a
// CPU package at 999 W and a host at 999 W.
const otlpNodePowerBody = `{"resourceMetrics":[
 {"resource":{"attributes":[
    {"key":"k8s.node.name","value":{"stringValue":"n1"}},
    {"key":"service.instance.id","value":{"stringValue":"n1"}}]},
  "scopeMetrics":[{"scope":{"name":"test"},"metrics":[
    {"name":"hw.power","unit":"W","gauge":{"dataPoints":[
      {"attributes":[{"key":"hw.id","value":{"stringValue":"cpu0"}},{"key":"hw.type","value":{"stringValue":"cpu"}}],
       "timeUnixNano":"1760000000000000000","asDouble":150},
      {"attributes":[{"key":"hw.id","value":{"stringValue":"gpu0"}},{"key":"hw.type","value":{"stringValue":"gpu"}}],
       "timeUnixNano":"1760000000000000000","asDouble":200},
      {"attributes":[{"key":"hw.id","value":{"stringValue":"gpu1"}},{"key":"hw.type","value":{"stringValue":"gpu"}}],
       "timeUnixNano":"1760000000000000000","asDouble":200},
      {"attributes":[{"key":"hw.id","value":{"stringValue":"memory0"}},{"key":"hw.type","value":{"stringValue":"memory"}},{"key":"hw.parent","value":{"stringValue":"cpu0"}}],
       "timeUnixNano":"1760000000000000000","asDouble":30}]}},
    {"name":"hw.host.power","unit":"W","gauge":{"dataPoints":[
      {"attributes":[{"key":"hw.id","value":{"stringValue":"host"}}],
       "timeUnixNano":"1760000000000000000","asDouble":700}]}}]}]},
 {"resource":{"attributes":[
    {"key":"k8s.node.name","value":{"stringValue":"n2"}},
    {"key":"service.instance.id","value":{"stringValue":"n2"}}]},
  "scopeMetrics":[{"scope":{"name":"test"},"metrics":[
    {"name":"hw.power","unit":"W","gauge":{"dataPoints":[
      {"attributes":[{"key":"hw.id","value":{"stringValue":"cpu0"}},{"key":"hw.type","value":{"stringValue":"cpu"}}],
       "timeUnixNano":"1760000000000000000","asDouble":999}]}},
    {"name":"hw.host.power","unit":"W","gauge":{"dataPoints":[
      {"attributes":[{"key":"hw.id","value":{"stringValue":"host"}}],
       "timeUnixNano":"1760000000000000000","asDouble":999}]}}]}]}]}`

// otlpNodeEnergyBody is an export of node n1 with cumulative hw.energy
// counters only, a CPU package at cpuJ and a GPU at gpuJ, at sec seconds.
func otlpNodeEnergyBody(sec int, cpuJ, gpuJ float64) string {
	point := func(id, typ string, j float64) string {
		return fmt.Sprintf(`{"attributes":[{"key":"hw.id","value":{"stringValue":%q}},{"key":"hw.type","value":{"stringValue":%q}}],
       "startTimeUnixNano":"1700000000000000000","timeUnixNano":"%d000000000","asDouble":%g}`, id, typ, 1760000000+sec, j)
	}
	return `{"resourceMetrics":[{"resource":{"attributes":[
    {"key":"k8s.node.name","value":{"stringValue":"n1"}}]},
  "scopeMetrics":[{"scope":{"name":"test"},"metrics":[
    {"name":"hw.energy","unit":"J","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[
      ` + point("cpu0", "cpu", cpuJ) + `,
      ` + point("gpu0", "gpu", gpuJ) + `]}}]}]}]}`
}

// serveNodePowerHTTPs counts the servers serveNodePowerHTTP started, so
// that each one has an endpoint of its own.
var serveNodePowerHTTPs atomic.Int64

// serveNodePowerHTTP points the http source at a server that answers the
// i-th request with bodies[i], repeating the last one, and restores the
// source after the test. The endpoint path names the test and the server,
// so no reader state or warning of another test, or of an earlier run of
// this one under -count, applies to it, even on a reused port.
func serveNodePowerHTTP(t *testing.T, bodies ...string) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := n
		n++
		mu.Unlock()
		if i >= len(bodies) {
			i = len(bodies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bodies[i]))
	}))
	t.Cleanup(srv.Close)

	prevSource, prevEndpoint := nodePowerSource, nodePowerHTTPEndpoint
	nodePowerSource = "http"
	nodePowerHTTPEndpoint = fmt.Sprintf("%s/%s/%d/otlp/telemetry/{node}", srv.URL, url.PathEscape(t.Name()), serveNodePowerHTTPs.Add(1))
	t.Cleanup(func() { nodePowerSource, nodePowerHTTPEndpoint = prevSource, prevEndpoint })
}

// withNodePowerSpec sets the node power settings for the test.
func withNodePowerSpec(t *testing.T, spec inputs.NodePowerSpec) {
	t.Helper()
	prev := nodePowerSpec
	nodePowerSpec = spec
	t.Cleanup(func() { nodePowerSpec = prev })
}

// The http source reads an OTLP export by NODE_POWER_SCOPE: the CPU packages
// and the GPUs by default, 150 + 2 x 200 = 550, which is the boundary of the
// twin's budget, without the DRAM zone or another node; the host's 700 under
// host. Node n2 of the same export reads its own 999. The old reader looked
// only for packagePowerWatts and read 0.
func TestQueryNodePowerHTTPReadsOTLPComponents(t *testing.T) {
	for _, tc := range []struct {
		scope inputs.Scope
		want  float64
	}{
		{"", 550},
		{inputs.ScopeComponents, 550},
		{inputs.ScopeHost, 700},
	} {
		t.Run(fmt.Sprintf("scope=%q", tc.scope), func(t *testing.T) {
			serveNodePowerHTTP(t, otlpNodePowerBody)
			withNodePowerSpec(t, inputs.NodePowerSpec{Scope: tc.scope})
			for _, node := range []struct {
				name string
				want float64
			}{{"n1", tc.want}, {"n2", 999}} {
				got, err := queryNodePowerHTTP(context.Background(), node.name)
				if err != nil {
					t.Fatal(err)
				}
				if got != node.want {
					t.Fatalf("queryNodePowerHTTP(%s) = %v, want %v", node.name, got, node.want)
				}
			}
			if w, source := resolveNodePower(context.Background(), nil, "n1", joulie.NodeHardware{}); w != tc.want || source != joulie.PowerSourceHTTP {
				t.Fatalf("resolveNodePower(n1) = (%v, %q), want (%v, %q)", w, source, tc.want, joulie.PowerSourceHTTP)
			}
		})
	}
}

// Energy counters without power gauges give a rate between two reconciles,
// measured on the producer's clock: (2500 - 1000) / 10 s for the CPU plus
// (6000 - 4000) / 10 s for the GPU. The first reconcile has no rate yet and
// reads 0 without an error.
func TestQueryNodePowerHTTPEnergyRateAcrossReconciles(t *testing.T) {
	serveNodePowerHTTP(t, otlpNodeEnergyBody(0, 1000, 4000), otlpNodeEnergyBody(10, 2500, 6000))
	withNodePowerSpec(t, inputs.NodePowerSpec{})
	for i, want := range []float64{0, 350} {
		got, err := queryNodePowerHTTP(context.Background(), "n1")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("read %d: queryNodePowerHTTP(n1) = %v, want %v", i+1, got, want)
		}
	}
}

// PROMETHEUS_RATE_WINDOW bounds the interval an energy rate is measured
// over, as rate(energy[window]) does: two exports 20 s apart give
// (2500 - 1000) / 20 s + (6000 - 4000) / 20 s = 175 under a 30 s window, and
// no rate under a 10 s one, where the second export warms up again.
func TestQueryNodePowerHTTPRateWindowIsPrometheusRateWindow(t *testing.T) {
	for _, tc := range []struct {
		window time.Duration
		want   float64
	}{
		{30 * time.Second, 175},
		{10 * time.Second, 0},
	} {
		t.Run(tc.window.String(), func(t *testing.T) {
			serveNodePowerHTTP(t, otlpNodeEnergyBody(0, 1000, 4000), otlpNodeEnergyBody(20, 2500, 6000))
			withNodePowerSpec(t, inputs.NodePowerSpec{Window: tc.window})
			for i, want := range []float64{0, tc.want} {
				got, err := queryNodePowerHTTP(context.Background(), "n1")
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("read %d: queryNodePowerHTTP(n1) = %v, want %v", i+1, got, want)
				}
			}
		})
	}
}

// The legacy packagePowerWatts JSON is still read, top level or under cpu,
// with one deprecation warning per endpoint.
func TestQueryNodePowerHTTPLegacyJSONWarnsOnce(t *testing.T) {
	serveNodePowerHTTP(t, `{"packagePowerWatts": 145}`, `{"cpu": {"packagePowerWatts": 160}}`)
	resetDeprecatedEnvWarnings()
	t.Cleanup(resetDeprecatedEnvWarnings)
	buf := captureLog(t)
	for i, want := range []float64{145, 160} {
		got, err := queryNodePowerHTTP(context.Background(), "n1")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("read %d: queryNodePowerHTTP(n1) = %v, want %v", i+1, got, want)
		}
	}
	if n := strings.Count(buf.String(), "legacy packagePowerWatts JSON"); n != 1 {
		t.Fatalf("got %d deprecation warnings, want 1:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), inputAliasesRemovedIn) {
		t.Fatalf("warning does not name the removal release %s:\n%s", inputAliasesRemovedIn, buf.String())
	}
}
