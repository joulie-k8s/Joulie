//go:build promlive

package inputs

// The live tests push OTLP JSON to a real Prometheus and read it back with
// the queries of this package. They read the receiver's otlp settings from
// /api/v1/status/config, so one run covers whatever configuration the
// server has; CI runs them against prom/prometheus:v3.15.0 in four:
//
//	promoted:          promote_resource_attributes [k8s.node.name, host.name]
//	unpromoted:        no promotion
//	utf8:              promoted, translation_strategy NoUTF8EscapingWithSuffixes
//	utf8-unpromoted:   no promotion, NoUTF8EscapingWithSuffixes
//
// Locally, with a prometheus.yml holding one of those otlp blocks and
// storage.tsdb.out_of_order_time_window: 30m:
//
//	docker run -d --name joulie-prom -p 127.0.0.1:19190:9090 \
//	  -v $PWD/prometheus.yml:/etc/prometheus/prometheus.yml:ro \
//	  prom/prometheus:v3.15.0 --config.file=/etc/prometheus/prometheus.yml --web.enable-otlp-receiver
//	JOULIE_PROM_URL=http://127.0.0.1:19190 go test -tags promlive ./pkg/controller/inputs/ -run Live -v
//
// Each test reports nodes of its own, named after the run, so repeated runs
// against one server do not interfere. Every series is backdated to cover
// the rate window plus four export intervals: a series younger than the
// window under-reads.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// liveStep is the export interval the pushed series imitate.
const liveStep = 15 * time.Second

// liveServer is the Prometheus under test and its otlp settings.
type liveServer struct {
	url      string
	promoted bool
	naming   promname.Strategy
}

func newLiveServer(t *testing.T) liveServer {
	t.Helper()
	u := os.Getenv("JOULIE_PROM_URL")
	if u == "" {
		t.Skip("JOULIE_PROM_URL is not set")
	}
	u = strings.TrimRight(u, "/")
	resp, err := http.Get(u + "/api/v1/status/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var cfg struct {
		Data struct {
			YAML string `json:"yaml"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatalf("reading %s/api/v1/status/config: %v", u, err)
	}
	s := liveServer{url: u}
	s.promoted, s.naming = otlpSettings(t, cfg.Data.YAML)
	t.Logf("Prometheus %s: k8s.node.name promoted %t, translation strategy %s", u, s.promoted, s.naming)
	return s
}

// otlpSettings reads the otlp block of the configuration Prometheus
// prints: whether k8s.node.name is promoted, and the translation strategy.
func otlpSettings(t *testing.T, yaml string) (promoted bool, naming promname.Strategy) {
	t.Helper()
	naming = promname.Default
	in, list := false, false
	for _, line := range strings.Split(yaml, "\n") {
		if !strings.HasPrefix(line, " ") {
			in = strings.TrimSpace(line) == "otlp:"
			continue
		}
		if !in {
			continue
		}
		f := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(f, "- ") && list:
			if strings.Trim(strings.TrimPrefix(f, "- "), `"'`) == semconv.AttrK8sNodeName {
				promoted = true
			}
		case strings.HasPrefix(f, "promote_resource_attributes:"):
			list = true
			if rest := strings.TrimSpace(strings.TrimPrefix(f, "promote_resource_attributes:")); strings.Contains(rest, semconv.AttrK8sNodeName) {
				promoted = true
			}
		case strings.HasPrefix(f, "promote_all_resource_attributes:"):
			list = false
			if strings.TrimSpace(strings.TrimPrefix(f, "promote_all_resource_attributes:")) == "true" {
				promoted = true
			}
		case strings.HasPrefix(f, "translation_strategy:"):
			list = false
			st, err := promname.ParseStrategy(strings.TrimSpace(strings.TrimPrefix(f, "translation_strategy:")))
			if err != nil {
				t.Fatal(err)
			}
			naming = st
		default:
			list = false
		}
	}
	return promoted, naming
}

// liveNode is one node a test reports: its component readings in W and its
// inlet temperature in Celsius.
type liveNode struct {
	name                   string
	cpu0, cpu1, dram, host float64
	gpuEnergy, gpuGauge    float64
	inlet                  float64
}

// components is what scope components reads: CPU packages and GPUs.
func (n liveNode) components() float64 { return n.cpu0 + n.cpu1 + n.gpuEnergy + n.gpuGauge }

// liveNodes returns two nodes named after the test and the run. Node a has
// a GPU with only a power gauge; node b a GPU with an energy counter.
func liveNodes(t *testing.T) (a, b liveNode) {
	run := fmt.Sprintf("live-%d-%s", time.Now().UnixNano()%1e9, strings.ToLower(strings.TrimPrefix(t.Name(), "TestLive")))
	return liveNode{name: run + "-a", cpu0: 200, cpu1: 150, dram: 40, gpuGauge: 300, host: 900, inlet: 24.5},
		liveNode{name: run + "-b", cpu0: 100, gpuEnergy: 250, host: 500, inlet: -3}
}

// nodeResource is the OTLP document of one node at times ts, energy counted
// from start. version is its service.version, the attribute a producer
// restart changes.
func nodeResource(n liveNode, version string, start time.Time, ts []time.Time) otlpjson.ResourceMetrics {
	energy := func(watts float64, attrs ...otlpjson.KeyValue) []otlpjson.NumberDataPoint {
		var pts []otlpjson.NumberDataPoint
		for _, at := range ts {
			v := watts * at.Sub(start).Seconds()
			pts = append(pts, otlpjson.NumberDataPoint{Attributes: attrs, StartTimeUnixNano: otlpjson.Uint64String(start.UnixNano()), TimeUnixNano: otlpjson.Uint64String(at.UnixNano()), AsDouble: &v})
		}
		return pts
	}
	gauge := func(value float64, attrs ...otlpjson.KeyValue) []otlpjson.NumberDataPoint {
		var pts []otlpjson.NumberDataPoint
		for _, at := range ts {
			v := value
			pts = append(pts, otlpjson.NumberDataPoint{Attributes: attrs, TimeUnixNano: otlpjson.Uint64String(at.UnixNano()), AsDouble: &v})
		}
		return pts
	}
	comp := func(hwType, id string) []otlpjson.KeyValue {
		return []otlpjson.KeyValue{otlpjson.String(semconv.AttrHwID, id), otlpjson.String(semconv.AttrHwType, hwType)}
	}
	var energyPts []otlpjson.NumberDataPoint
	energyPts = append(energyPts, energy(n.cpu0, comp(semconv.HwTypeCPU, "cpu0")...)...)
	if n.cpu1 > 0 {
		energyPts = append(energyPts, energy(n.cpu1, comp(semconv.HwTypeCPU, "cpu1")...)...)
	}
	if n.dram > 0 {
		energyPts = append(energyPts, energy(n.dram, comp(semconv.HwTypeMemory, "memory0")...)...)
	}
	if n.gpuEnergy > 0 {
		energyPts = append(energyPts, energy(n.gpuEnergy, comp(semconv.HwTypeGPU, "gpu0")...)...)
	}
	metrics := []otlpjson.Metric{
		{Name: semconv.HwEnergy.Name, Unit: semconv.HwEnergy.Unit, Sum: &otlpjson.Sum{
			DataPoints: energyPts, AggregationTemporality: otlpjson.AggregationTemporalityCumulative, IsMonotonic: true}},
		{Name: semconv.HwHostEnergy.Name, Unit: semconv.HwHostEnergy.Unit, Sum: &otlpjson.Sum{
			DataPoints: energy(n.host, otlpjson.String(semconv.AttrHwID, semconv.HwIDHost)), AggregationTemporality: otlpjson.AggregationTemporalityCumulative, IsMonotonic: true}},
		{Name: semconv.HwHostAmbientTemperature.Name, Unit: semconv.HwHostAmbientTemperature.Unit, Gauge: &otlpjson.Gauge{
			DataPoints: gauge(n.inlet, otlpjson.String(semconv.AttrHwID, semconv.HwIDHost))}},
	}
	if n.gpuGauge > 0 {
		metrics = append(metrics, otlpjson.Metric{Name: semconv.HwPower.Name, Unit: semconv.HwPower.Unit, Gauge: &otlpjson.Gauge{
			DataPoints: gauge(n.gpuGauge, comp(semconv.HwTypeGPU, "gpu0")...)}})
	}
	return otlpjson.ResourceMetrics{
		Resource: otlpjson.Resource{Attributes: []otlpjson.KeyValue{
			otlpjson.String(semconv.AttrServiceName, "joulie-live"),
			otlpjson.String(semconv.AttrServiceNamespace, "joulie"),
			otlpjson.String(semconv.AttrServiceInstanceID, n.name),
			otlpjson.String(semconv.AttrServiceVersion, version),
			otlpjson.String(semconv.AttrK8sNodeName, n.name),
			otlpjson.String(semconv.AttrHostName, n.name),
		}},
		ScopeMetrics: []otlpjson.ScopeMetrics{{
			Scope:     otlpjson.Scope{Name: "github.com/matbun/joulie/pkg/controller/inputs"},
			Metrics:   metrics,
			SchemaURL: semconv.SchemaURL,
		}},
		SchemaURL: semconv.SchemaURL,
	}
}

// push POSTs one export request to the receiver.
func (s liveServer) push(t *testing.T, rms ...otlpjson.ResourceMetrics) {
	t.Helper()
	body, err := json.Marshal(otlpjson.ExportMetricsServiceRequest{ResourceMetrics: rms})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(s.url+"/api/v1/otlp/v1/metrics", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("OTLP push: HTTP %d %s (is Prometheus running with --web.enable-otlp-receiver?)", resp.StatusCode, msg)
	}
}

// pushHistory pushes nodes with samples every liveStep over the rate window
// plus four steps, ending now, and returns the start of their counters.
func (s liveServer) pushHistory(t *testing.T, version string, nodes ...liveNode) time.Time {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	start := now.Add(-DefaultWindow - 4*liveStep)
	var ts []time.Time
	for at := start; !at.After(now); at = at.Add(liveStep) {
		ts = append(ts, at)
	}
	var rms []otlpjson.ResourceMetrics
	for _, n := range nodes {
		rms = append(rms, nodeResource(n, version, start, ts))
	}
	s.push(t, rms...)
	return start
}

func (s liveServer) query(t *testing.T, q Query) (map[string]float64, Issues) {
	t.Helper()
	res, err := Client{Address: s.url}.Query(context.Background(), q.PromQL)
	if err != nil {
		t.Fatalf("%s: %v", q.PromQL, err)
	}
	got, iss, err := res.ByLabel(q.GroupLabel)
	if err != nil {
		t.Fatalf("%s: %v", q.PromQL, err)
	}
	return got, iss
}

func assertNear(t *testing.T, what string, got map[string]float64, node string, want float64) {
	t.Helper()
	v, ok := got[node]
	if !ok {
		t.Errorf("%s: no value for node %s", what, node)
		return
	}
	if math.Abs(v-want) > 0.01*want {
		t.Errorf("%s: node %s reads %.2f W, want %.2f W within 1%%", what, node, v, want)
	}
}

// TestLiveLabelsFormReadsPromotedNodes: with k8s.node.name promoted, the
// labels form returns every series with the node label, the condition on
// which the controller manager's auto mode keeps that form, and it reads
// the components and the host of each node, as the target_info form does.
// This package builds both forms; the choice between them is the
// controller manager's.
func TestLiveLabelsFormReadsPromotedNodes(t *testing.T) {
	s := newLiveServer(t)
	if !s.promoted {
		t.Skip("the server does not promote k8s.node.name")
	}
	a, b := liveNodes(t)
	s.pushHistory(t, "1", a, b)
	for _, scope := range []Scope{ScopeComponents, ScopeHost} {
		primary, fallback, err := NodePowerSpec{Scope: scope, Naming: s.naming}.Query()
		if err != nil {
			t.Fatal(err)
		}
		got, iss := s.query(t, primary)
		if iss.MissingLabel != 0 {
			t.Errorf("%s labels form: %d series without %q; auto would fall back", scope, iss.MissingLabel, primary.GroupLabel)
		}
		want := map[string]float64{a.name: a.components(), b.name: b.components()}
		if scope == ScopeHost {
			want = map[string]float64{a.name: a.host, b.name: b.host}
		}
		for node, w := range want {
			assertNear(t, string(scope)+" labels form", got, node, w)
		}
		got, _ = s.query(t, fallback)
		for node, w := range want {
			assertNear(t, string(scope)+" target_info form", got, node, w)
		}
	}
}

// TestLiveLabelsFormLacksNodeWithoutPromotion: without promotion the
// labels form returns series without the node label, the condition on
// which the controller manager's auto mode switches to the target_info
// form, and the target_info form reads each node.
func TestLiveLabelsFormLacksNodeWithoutPromotion(t *testing.T) {
	s := newLiveServer(t)
	if s.promoted {
		t.Skip("the server promotes k8s.node.name")
	}
	a, b := liveNodes(t)
	s.pushHistory(t, "1", a, b)
	for _, scope := range []Scope{ScopeComponents, ScopeHost} {
		primary, fallback, err := NodePowerSpec{Scope: scope, Naming: s.naming}.Query()
		if err != nil {
			t.Fatal(err)
		}
		got, iss := s.query(t, primary)
		if iss.MissingLabel == 0 {
			t.Errorf("%s labels form: every series has %q (%v); want the unlabelled series that make auto fall back", scope, primary.GroupLabel, got)
		}
		if _, ok := got[a.name]; ok {
			t.Errorf("%s labels form: node %s found without promotion", scope, a.name)
		}
		got, iss = s.query(t, fallback)
		want := map[string]float64{a.name: a.components(), b.name: b.components()}
		if scope == ScopeHost {
			want = map[string]float64{a.name: a.host, b.name: b.host}
		}
		for node, w := range want {
			assertNear(t, string(scope)+" target_info form", got, node, w)
		}
	}
}

// TestLiveTargetInfoJoinSurvivesChurn: after a producer restart with a new
// service.version, its job and instance have two target_info series for
// the lookback period. A plain join then fails with HTTP 422 many-to-many;
// the max by join of the target_info form still reads each node.
func TestLiveTargetInfoJoinSurvivesChurn(t *testing.T) {
	s := newLiveServer(t)
	a, b := liveNodes(t)
	start := s.pushHistory(t, "1", a, b)
	// One more sample per series under the new version, after the history
	// and not in the future, or the query would not see it yet.
	now := []time.Time{time.Now()}
	s.push(t, nodeResource(a, "2", start, now), nodeResource(b, "2", start, now))

	n := namesFor(s.naming)
	window, _ := PromDuration(DefaultWindow)
	// The instance label holds service.instance.id: selecting this test's
	// node by it keeps other runs' data out of the join.
	plain := fmt.Sprintf("sum by %s (%s * on %s group_left %s %s)", labelList(n.node),
		rate(selector(n.energy, matcher{n.hwType, "=", semconv.HwTypeCPU}, matcher{promname.LabelInstance, "=", a.name}), window),
		labelList(promname.LabelJob, promname.LabelInstance), labelList(n.node), promname.TargetInfo)
	_, err := Client{Address: s.url}.Query(context.Background(), plain)
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != http.StatusUnprocessableEntity || !strings.Contains(pe.Message, "many-to-many") {
		t.Errorf("plain join after churn: %v, want HTTP 422 many-to-many", err)
	}

	_, fallback, err := NodePowerSpec{Naming: s.naming}.Query()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.query(t, fallback)
	assertNear(t, "target_info form after churn", got, a.name, a.components())
	assertNear(t, "target_info form after churn", got, b.name, b.components())
}

// foreignResource is the OTLP document of an application on node that
// reports no power and sets service.name but no service.instance.id: the
// receiver gives its target_info a job and no instance.
func foreignResource(service, node string, at time.Time) otlpjson.ResourceMetrics {
	v := 1.0
	return otlpjson.ResourceMetrics{
		Resource: otlpjson.Resource{Attributes: []otlpjson.KeyValue{
			otlpjson.String(semconv.AttrServiceName, service),
			otlpjson.String(semconv.AttrK8sNodeName, node),
			otlpjson.String(semconv.AttrHostName, node),
		}},
		ScopeMetrics: []otlpjson.ScopeMetrics{{
			Scope: otlpjson.Scope{Name: "example.com/app"},
			Metrics: []otlpjson.Metric{{Name: "app.requests.active", Unit: "{request}", Gauge: &otlpjson.Gauge{
				DataPoints: []otlpjson.NumberDataPoint{{TimeUnixNano: otlpjson.Uint64String(at.UnixNano()), AsDouble: &v}}}}},
		}},
	}
}

// TestLiveForeignProducerLeavesJoinsAlone: an application without
// service.instance.id that runs on both nodes leaves two target_info
// series, one per node, under one job and no instance. Prometheus checks
// every right-hand series of a join for duplicates, so the target_info form
// and the node inlet query must keep such series out of their joins to
// read each node.
func TestLiveForeignProducerLeavesJoinsAlone(t *testing.T) {
	s := newLiveServer(t)
	a, b := liveNodes(t)
	s.pushHistory(t, "1", a, b)
	app := "app-" + a.name
	now := time.Now()
	s.push(t, foreignResource(app, a.name, now), foreignResource(app, b.name, now))

	res, err := Client{Address: s.url}.Query(context.Background(), selector(promname.TargetInfo, matcher{promname.LabelJob, "=", app}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Samples) != 2 {
		t.Fatalf("target_info of %s: %+v, want one series per node", app, res.Samples)
	}
	for _, x := range res.Samples {
		if x.Labels[promname.LabelInstance] != "" {
			t.Fatalf("target_info of %s has instance %q; the case needs a job alone", app, x.Labels[promname.LabelInstance])
		}
	}

	for _, scope := range []Scope{ScopeComponents, ScopeHost} {
		_, fallback, err := NodePowerSpec{Scope: scope, Naming: s.naming}.Query()
		if err != nil {
			t.Fatal(err)
		}
		got, _ := s.query(t, fallback)
		want := map[string]float64{a.name: a.components(), b.name: b.components()}
		if scope == ScopeHost {
			want = map[string]float64{a.name: a.host, b.name: b.host}
		}
		for node, w := range want {
			assertNear(t, string(scope)+" target_info form", got, node, w)
		}
	}
	qs, err := FacilitySpec{Naming: s.naming}.Queries()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.query(t, qs[FacilityInputNodeInletTemp])
	for _, n := range []liveNode{a, b} {
		if v, ok := got[n.name]; !ok || math.Abs(v-n.inlet) > 1e-9 {
			t.Errorf("node inlet of %s: %v (found %t), want %v", n.name, v, ok, n.inlet)
		}
	}
}

// TestLiveNodeInletReadsEveryNode: the facility node inlet query reads the
// host ambient temperature of each node whether or not the server promotes
// k8s.node.name, including a reading below 0.
func TestLiveNodeInletReadsEveryNode(t *testing.T) {
	s := newLiveServer(t)
	a, b := liveNodes(t)
	s.pushHistory(t, "1", a, b)
	qs, err := FacilitySpec{Naming: s.naming}.Queries()
	if err != nil {
		t.Fatal(err)
	}
	got, iss := s.query(t, qs[FacilityInputNodeInletTemp])
	if iss.MissingLabel != 0 {
		t.Errorf("node inlet: %d series without %q", iss.MissingLabel, qs[FacilityInputNodeInletTemp].GroupLabel)
	}
	for _, n := range []liveNode{a, b} {
		if v, ok := got[n.name]; !ok || math.Abs(v-n.inlet) > 1e-9 {
			t.Errorf("node inlet of %s: %v (found %t), want %v", n.name, v, ok, n.inlet)
		}
	}
}

// TestLiveReceiverNamesMatchPromname: the metric and label names the
// receiver stored for a pushed node are the ones promname computes for its
// translation strategy.
func TestLiveReceiverNamesMatchPromname(t *testing.T) {
	s := newLiveServer(t)
	a, _ := liveNodes(t)
	s.pushHistory(t, "1", a)

	u := s.url + "/api/v1/series?" + url.Values{"match[]": {fmt.Sprintf(`{instance=%q}`, a.name)}}.Encode()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	names := map[string]map[string]bool{}
	for _, series := range out.Data {
		labels := map[string]bool{}
		for k := range series {
			labels[k] = true
		}
		names[series["__name__"]] = labels
	}
	for _, m := range []semconv.Metric{semconv.HwEnergy, semconv.HwPower, semconv.HwHostEnergy} {
		want := s.naming.MetricName(m)
		labels, ok := names[want]
		if !ok {
			t.Errorf("%s: no series named %q among %v", m.Name, want, keys(names))
			continue
		}
		wantLabels := []string{s.naming.LabelName(semconv.AttrHwID)}
		if m.HasAttribute(semconv.AttrHwType) {
			wantLabels = append(wantLabels, s.naming.LabelName(semconv.AttrHwType))
		}
		if s.promoted {
			wantLabels = append(wantLabels, s.naming.LabelName(semconv.AttrK8sNodeName))
		}
		for _, l := range wantLabels {
			if !labels[l] {
				t.Errorf("%s: series %q has no label %q (labels %v)", m.Name, want, l, keys(labels))
			}
		}
	}
	ti, ok := names[promname.TargetInfo]
	if !ok || !ti[s.naming.LabelName(semconv.AttrK8sNodeName)] {
		t.Errorf("target_info %v, want it with %q", ti, s.naming.LabelName(semconv.AttrK8sNodeName))
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
