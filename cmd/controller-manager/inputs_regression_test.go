package main

// Regression tests of the measured inputs. Each one fails on the code before
// the OpenTelemetry inputs (a94f345), and uses only names that code has, so
// it can be copied there to show the failure.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

// regProm is a fake Prometheus and node telemetry endpoint: GET
// /api/v1/query answers with answer(query), and GET /telemetry/<node>
// reports 100 W of package power for any node. It records every query.
type regProm struct {
	*httptest.Server
	mu      sync.Mutex
	queries []string
}

func newRegProm(t *testing.T, answer func(query string) (status int, body string)) *regProm {
	t.Helper()
	p := &regProm{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/telemetry/") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"packagePowerWatts": 100}`)
			return
		}
		q := r.URL.Query().Get("query")
		p.mu.Lock()
		p.queries = append(p.queries, q)
		p.mu.Unlock()
		status, body := answer(q)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(p.Close)
	return p
}

// seen returns the queries received so far.
func (p *regProm) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.queries...)
}

// regVector is the body of a successful instant query returning series,
// each a JSON {"metric": ..., "value": [...]} object.
func regVector(series ...string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(series, ",") + `]}}`
}

// regSeries is one series with labels (JSON object) and value.
func regSeries(labels, value string) string {
	return fmt.Sprintf(`{"metric":%s,"value":[1759600000,%q]}`, labels, value)
}

// regWithNodePowerVars sets the node power variables for one test and
// restores them after it.
func regWithNodePowerVars(t *testing.T, source, httpEndpoint, promAddress, promQuery string) {
	t.Helper()
	prev := []string{nodePowerSource, nodePowerHTTPEndpoint, nodePowerPromAddress, nodePowerPromQuery}
	t.Cleanup(func() {
		nodePowerSource, nodePowerHTTPEndpoint, nodePowerPromAddress, nodePowerPromQuery = prev[0], prev[1], prev[2], prev[3]
	})
	nodePowerSource, nodePowerHTTPEndpoint, nodePowerPromAddress, nodePowerPromQuery = source, httpEndpoint, promAddress, promQuery
}

// regZoneNode is a managed CPU-only node in cooling zone z1, 200 W of TDP.
func regZoneNode(t *testing.T) []runtime.Object {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "zone-node", Labels: map[string]string{
		"joulie.io/managed": "true", "joulie.io/cooling-zone": "z1",
	}}}
	return []runtime.Object{node, nodeHardwareFromJSON(t, `{
  "apiVersion": "joulie.io/v1alpha1", "kind": "NodeHardware",
  "metadata": {"name": "zone-node"}, "spec": {"nodeName": "zone-node"},
  "status": {"cpu": {"sockets": 1, "totalCores": 32, "capRange": {"type": "package", "maxWattsPerSocket": 200}}}
}`)}
}

// regCoolingStress reconciles objs once with fm as the facility and returns
// the cooling stress the NodeTwin of node got. Afterwards it reconciles an
// empty cluster with an empty facility, so the next test starts from
// unknown facility readings.
func regCoolingStress(t *testing.T, fm *facilityMetrics, node string, objs ...runtime.Object) float64 {
	t.Helper()
	reconcileFacility := func(fm *facilityMetrics, objs ...runtime.Object) *unstructured.Unstructured {
		reader, client, dyn := fakeClients(t, objs...)
		selector, err := labels.Parse("joulie.io/managed=true")
		if err != nil {
			t.Fatalf("parse selector: %v", err)
		}
		if err := reconcileWithCatalogAndFacility(
			context.Background(), reader, client, dyn, selector,
			"joulie.io/reserved", "joulie.io/power-profile", time.Minute,
			5000, 120, "static_partition", 0.5, 0.5, 1, 5, 10,
			100, 60, false, 100, 60, false,
			map[string]GPUModelCaps{}, []string{"joulie.io/gpu.product"}, loadHardwareCatalog(),
			fm,
		); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if node == "" || len(objs) == 0 {
			return nil
		}
		got, err := dyn.Resource(nodeTwinGVR).Get(context.Background(), node, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get NodeTwin %s: %v", node, err)
		}
		return got
	}
	got := reconcileFacility(fm, objs...)
	reconcileFacility(&facilityMetrics{})

	status, _ := got.Object["status"].(map[string]interface{})
	measured, _ := status["powerMeasurement"].(map[string]interface{})
	if w := regNumber(measured["measuredNodePowerW"]); w != 100 {
		t.Fatalf("measuredNodePowerW=%v, want the 100 W the HTTP source reports (status %v)", w, status)
	}
	return regNumber(status["predictedCoolingStressScore"])
}

// regNumber reads a number as the fake stores it: int64 when whole.
func regNumber(v interface{}) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return math.NaN()
}

// A per-node query whose answer splits the node, here by RAPL package, must
// read the whole node. The old code kept the first series: 150 of 200 W.
func TestResolveNodePowerSumsEverySeriesOfTheNode(t *testing.T) {
	prom := newRegProm(t, func(string) (int, string) {
		return 200, regVector(
			regSeries(`{"node":"n1","zone":"package-0"}`, "150"),
			regSeries(`{"node":"n1","zone":"package-1"}`, "50"),
		)
	})
	regWithNodePowerVars(t, "prometheus", "", prom.URL, `rate(node_package_joules_total{node="{node}"}[5m])`)

	got, source := resolveNodePower(context.Background(), nil, "n1", joulie.NodeHardware{})
	if got != 200 || source != joulie.PowerSourcePrometheus {
		t.Fatalf("resolveNodePower = %v W (%s), want 200 W (prometheus): every series of the node counts, not the first", got, source)
	}
	if q := prom.seen(); len(q) != 1 || q[0] != `rate(node_package_joules_total{node="n1"}[5m])` {
		t.Fatalf("queries %q, want the query once with {node} replaced", q)
	}
}

func TestQueryPrometheusScalarAcceptsScalarResult(t *testing.T) {
	prom := newRegProm(t, func(string) (int, string) {
		return 200, `{"status":"success","data":{"resultType":"scalar","result":[1759600000,"21.5"]}}`
	})
	v, err := queryPrometheusScalar(context.Background(), prom.Client(), prom.URL, "scalar(avg(site_ambient_celsius))")
	if err != nil || v != 21.5 {
		t.Fatalf("queryPrometheusScalar = %v, %v; want 21.5 from a scalar result", v, err)
	}
}

func TestQueryPrometheusScalarRejectsMultipleSeries(t *testing.T) {
	prom := newRegProm(t, func(string) (int, string) {
		return 200, regVector(regSeries(`{"rack":"r1"}`, "10000"), regSeries(`{"rack":"r2"}`, "7000"))
	})
	v, err := queryPrometheusScalar(context.Background(), prom.Client(), prom.URL, "rack_power_watts")
	if err == nil {
		t.Fatalf("queryPrometheusScalar = %v with two series, want an error: one of them is not the answer", v)
	}
}

func TestQueryPrometheusScalarRejectsNonFinite(t *testing.T) {
	for _, value := range []string{"NaN", "+Inf", "-Inf"} {
		prom := newRegProm(t, func(string) (int, string) {
			return 200, regVector(regSeries(`{}`, value))
		})
		v, err := queryPrometheusScalar(context.Background(), prom.Client(), prom.URL, "sum(x) / sum(y)")
		if err == nil {
			t.Fatalf("queryPrometheusScalar = %v for %s, want an error", v, value)
		}
	}
}

// The error names Prometheus' errorType and message, which say what is
// wrong with the query; "no data" would send the reader looking for a
// missing exporter.
func TestQueryPrometheusScalarReportsPrometheusError(t *testing.T) {
	prom := newRegProm(t, func(string) (int, string) {
		return 400, `{"status":"error","errorType":"bad_data","error":"invalid parameter \"query\": 1:6: parse error: unexpected identifier \"y\""}`
	})
	_, err := queryPrometheusScalar(context.Background(), prom.Client(), prom.URL, "rate(x y)")
	if err == nil || !strings.Contains(err.Error(), "bad_data") || !strings.Contains(err.Error(), "parse error") {
		t.Fatalf("queryPrometheusScalar error %v, want Prometheus' errorType bad_data and its message", err)
	}
}

// A measured cooling zone temperature reaches the twin of a node in that
// zone. The old code stored zone readings but read only the facility-wide
// ambient, so a 35 C zone stressed the node as much as 20 C outside.
func TestZoneAmbientFromFacilityReachesTwin(t *testing.T) {
	prom := newRegProm(t, func(string) (int, string) { return 200, regVector() })
	regWithNodePowerVars(t, "http", prom.URL+"/telemetry/{node}", "", "")
	objs := regZoneNode(t)

	fm := &facilityMetrics{}
	fm.set(20, 0, 0)
	at20 := regCoolingStress(t, fm, "zone-node", objs...)
	fm.setTopology(map[string]float64{"z1": 35}, nil)
	inZone := regCoolingStress(t, fm, "zone-node", objs...)

	if !(inZone > at20) {
		t.Fatalf("cooling stress %v with zone z1 at 35 C, %v with 20 C outside: the zone reading must raise it", inZone, at20)
	}
}

// An outdoor ambient of -3 C is a reading, and a cold one: the node is
// easier to cool than with an unknown ambient. The old code took any value
// of 0 or below for "unknown".
func TestFacilitySubZeroAmbientChangesCoolingStress(t *testing.T) {
	prom := newRegProm(t, func(q string) (int, string) {
		if strings.Contains(q, "ambient_temperature") && !strings.Contains(q, "cooling_zone") && !strings.Contains(q, "hw_host") {
			return 200, regVector(regSeries(`{}`, "-3"))
		}
		return 200, regVector()
	})
	regWithNodePowerVars(t, "http", prom.URL+"/telemetry/{node}", "", "")
	objs := regZoneNode(t)

	cfg := defaultFacilityConfig()
	cfg.enabled = true
	cfg.prometheusAddress = prom.URL
	fm := &facilityMetrics{}
	fetchFacilityMetrics(context.Background(), prom.Client(), fm, cfg)
	if ambient, _, _ := fm.get(); ambient != -3 {
		t.Fatalf("facility ambient %v, want -3 from Prometheus", ambient)
	}

	cold := regCoolingStress(t, fm, "zone-node", objs...)
	unknown := regCoolingStress(t, &facilityMetrics{}, "zone-node", objs...)
	if !(cold < unknown) {
		t.Fatalf("cooling stress %v at -3 C, %v with the ambient unknown: a sub-zero reading must lower it", cold, unknown)
	}
}
