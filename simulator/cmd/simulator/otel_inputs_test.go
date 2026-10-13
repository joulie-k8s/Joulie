package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"

	"github.com/matbun/joulie/pkg/agent/control"
	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hw"
	"github.com/matbun/joulie/simulator/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// otlpTestModel is a legacy node model with gpus L40S boards; baseIdleW is
// the CPU's idle power and the node's starting average. The 10 s telemetry
// windows keep the averages apart from the instantaneous powers.
func otlpTestModel(baseIdleW float64, gpus int) simModel {
	m := simModel{BaseIdleW: baseIdleW, RaplHeadW: 5, DefaultCapW: 5000, CPUTelemetryWindowMS: 10000}
	if gpus > 0 {
		m.GPU = hw.GPUProfile{
			Vendor: "nvidia", Product: "L40S", Count: gpus,
			IdleWattsPerGPU: 30, MaxWattsPerGPU: 350, MinCapWattsPerGPU: 200, CapApplyTauMS: 300,
			ComputeGamma: 1.0, MemoryEpsilon: 0.2, MemoryGamma: 1.2, TelemetryWindowMS: 10000,
		}
	}
	return m
}

// otlpLoad gives node a CPU and GPU load and steps it two seconds in one
// substep, so its averages are part way between idle and the loaded power
// and the busy GPUs differ from the idle ones.
func otlpLoad(s *simulator, node string) {
	model := s.modelForNode(node)
	st := s.getNode(node)
	s.mu.Lock()
	st.CPUUtil, st.GPUUtil, st.PodsRunning = 0.7, 0.5, 3
	stepOnce(st, model, 2)
	s.mu.Unlock()
}

// otlpNoTimePassed steps node by no time: its instantaneous readings follow
// its load and its averages stay where they start.
func otlpNoTimePassed(s *simulator, node string) {
	model := s.modelForNode(node)
	st := s.getNode(node)
	s.mu.Lock()
	stepOnce(st, model, 0)
	s.mu.Unlock()
}

// otlpGet reads /otlp/telemetry/{node} through handleOTLPTelemetry, decodes
// it strictly and checks the node's resource and the scope.
func otlpGet(t *testing.T, s *simulator, node string) otlpjson.MetricsData {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleOTLPTelemetry(w, httptest.NewRequest(http.MethodGet, "/otlp/telemetry/"+node, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	md, err := otlpjson.DecodeMetricsStrict(w.Body.Bytes())
	if err != nil {
		t.Fatalf("%v\n%s", err, w.Body.String())
	}
	if len(md.ResourceMetrics) != 1 || len(md.ResourceMetrics[0].ScopeMetrics) != 1 {
		t.Fatalf("want one resource with one scope, got %+v", md)
	}
	rm := md.ResourceMetrics[0]
	attrs := map[string]string{}
	for _, kv := range rm.Resource.Attributes {
		if kv.Value.String != nil {
			attrs[kv.Key] = *kv.Value.String
		}
	}
	for k, want := range map[string]string{
		semconv.AttrServiceNamespace:  telemetry.ServiceNamespace,
		semconv.AttrServiceInstanceID: node,
		semconv.AttrK8sNodeName:       node,
		semconv.AttrHostName:          node,
		semconv.AttrHostID:            telemetry.HostID(node),
	} {
		if attrs[k] != want {
			t.Errorf("resource %s=%q, want %q", k, attrs[k], want)
		}
	}
	if rm.SchemaURL != semconv.SchemaURL || rm.ScopeMetrics[0].SchemaURL != semconv.SchemaURL {
		t.Errorf("schemaUrl resource=%q scope=%q, want %q", rm.SchemaURL, rm.ScopeMetrics[0].SchemaURL, semconv.SchemaURL)
	}
	if got := rm.ScopeMetrics[0].Scope.Name; got != telemetry.ScopeName {
		t.Errorf("scope %q, want %q", got, telemetry.ScopeName)
	}
	return md
}

// otlpComponent is one hw.power or hw.host.power point.
type otlpComponent struct {
	metric, id, typ, name string
	watts                 float64
}

// otlpComponents lists every point of md, which must be double gauges.
func otlpComponents(t *testing.T, md otlpjson.MetricsData) []otlpComponent {
	t.Helper()
	var out []otlpComponent
	for _, m := range md.ResourceMetrics[0].ScopeMetrics[0].Metrics {
		if m.Gauge == nil {
			t.Fatalf("%s is not a gauge", m.Name)
		}
		if unit, ok := map[string]string{
			semconv.HwPower.Name:     semconv.HwPower.Unit,
			semconv.HwHostPower.Name: semconv.HwHostPower.Unit,
		}[m.Name]; !ok || unit != m.Unit {
			t.Errorf("metric %s with unit %q, want hw.power or hw.host.power in W", m.Name, m.Unit)
		}
		for _, p := range m.Gauge.DataPoints {
			if p.AsDouble == nil {
				t.Fatalf("%s point without asDouble", m.Name)
			}
			c := otlpComponent{metric: m.Name, watts: *p.AsDouble}
			for _, kv := range p.Attributes {
				switch kv.Key {
				case semconv.AttrHwID:
					c.id = *kv.Value.String
				case semconv.AttrHwType:
					c.typ = *kv.Value.String
				case semconv.AttrHwName:
					c.name = *kv.Value.String
				default:
					t.Errorf("%s point has attribute %s", m.Name, kv.Key)
				}
			}
			out = append(out, c)
		}
	}
	return out
}

// lastTelemetryEvent is the payload of the newest "telemetry" event.
func lastTelemetryEvent(t *testing.T, s *simulator) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.events[i].Kind == "telemetry" {
			return s.events[i].Payload
		}
	}
	t.Fatal("no telemetry event recorded")
	return nil
}

// The OTLP components of a sample add up to the legacy packagePowerWatts of
// the same sample, which the "telemetry" event records: the CPU and GPU
// hw.power points, and hw.host.power on its own. The legacy value is
// rounded to 0.01 W.
func TestOTLPTelemetryComponentsEqualLegacyNodePower(t *testing.T) {
	cases := []struct {
		name    string
		model   simModel
		prepare func(s *simulator, node string)
		gpus    []string // hw.id of each GPU point
	}{
		{name: "cpu only, loaded", model: otlpTestModel(120, 0), prepare: otlpLoad},
		{name: "four gpus, half busy", model: otlpTestModel(90, 4), prepare: otlpLoad, gpus: []string{"gpu0", "gpu1", "gpu2", "gpu3"}},
		// A CPU-only model idling at 0 W starts with a 0 W average, and
		// while no time has passed (a step of 0 s) the legacy value is the
		// instantaneous power.
		{name: "before the average moves", model: otlpTestModel(0, 0), prepare: otlpNoTimePassed},
		// The same with busy GPUs whose averages are still 0: each GPU
		// counts its instantaneous power too.
		{name: "busy gpus before the average moves", model: otlpTestModel(0, 2), prepare: func(s *simulator, node string) {
			st := s.getNode(node)
			s.mu.Lock()
			st.GPUUtil = 0.5
			st.CPUAvgPowerWatts, st.TotalAvgPowerWatts = 0, 0
			for i := range st.GPUDevices {
				st.GPUDevices[i].AvgPowerWatts = 0
			}
			s.mu.Unlock()
			otlpNoTimePassed(s, node)
		}, gpus: []string{"gpu0", "gpu1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSimulatorWithRegisterer(tc.model, nil, nil, 10, prometheus.NewRegistry())
			const node = "node-a"
			tc.prepare(s, node)
			comps := otlpComponents(t, otlpGet(t, s, node))
			legacy, ok := lastTelemetryEvent(t, s)["packagePowerWatts"].(float64)
			if !ok || legacy <= 0 {
				t.Fatalf("legacy packagePowerWatts %v, want a positive number", legacy)
			}

			var cpuW, gpuW, hostW float64
			var cpus, hosts int
			var gpus []string
			for _, c := range comps {
				switch {
				case c.metric == semconv.HwHostPower.Name && c.id == semconv.HwIDHost && c.typ == "":
					hosts++
					hostW = c.watts
				case c.metric == semconv.HwPower.Name && c.typ == semconv.HwTypeCPU && c.id == semconv.HwTypeCPU:
					cpus++
					cpuW = c.watts
				case c.metric == semconv.HwPower.Name && c.typ == semconv.HwTypeGPU:
					if c.name != tc.model.GPU.Product {
						t.Errorf("%s hw.name %q, want %q", c.id, c.name, tc.model.GPU.Product)
					}
					gpus = append(gpus, c.id)
					gpuW += c.watts
				default:
					t.Errorf("unexpected point %+v", c)
				}
			}
			sort.Strings(gpus)
			if cpus != 1 || hosts != 1 || len(gpus) != len(tc.gpus) {
				t.Fatalf("points: %d cpu, %d host, gpus %v; want 1, 1, %v", cpus, hosts, gpus, tc.gpus)
			}
			for i := range gpus {
				if gpus[i] != tc.gpus[i] {
					t.Fatalf("gpu points %v, want %v", gpus, tc.gpus)
				}
			}
			if math.Abs(cpuW+gpuW-legacy) > 0.01 {
				t.Errorf("cpu %.4f W + gpus %.4f W = %.4f W, legacy node power %.2f W", cpuW, gpuW, cpuW+gpuW, legacy)
			}
			if math.Abs(hostW-legacy) > 0.01 {
				t.Errorf("hw.host.power %.4f W, legacy node power %.2f W", hostW, legacy)
			}
			if math.Abs(hostW-(cpuW+gpuW)) > 1e-9 {
				t.Errorf("hw.host.power %.6f W is not the components' %.6f W", hostW, cpuW+gpuW)
			}
		})
	}
}

// The agent's real HTTP power reader reads the route as OTLP and counts the
// CPU only, as the RAPL path it stands in for does: a reader that took the
// legacy JSON would return the node's CPU plus GPU power.
func TestAgentHTTPReaderReadsSimulatorOTLPTelemetry(t *testing.T) {
	s := newSimulatorWithRegisterer(otlpTestModel(90, 4), nil, nil, 10, prometheus.NewRegistry())
	const node = "node-gpu"
	otlpLoad(s, node)
	mux := http.NewServeMux()
	mux.HandleFunc("/otlp/telemetry/", s.handleOTLPTelemetry)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &control.HTTPPowerReader{Endpoint: srv.URL + "/otlp/telemetry/{node}", NodeName: node, Client: srv.Client()}
	got, ok, err := r.ReadPowerWatts()
	if err != nil || !ok {
		t.Fatalf("ReadPowerWatts = %v, ok=%v, err=%v; want a reading", got, ok, err)
	}
	st := s.getNode(node)
	s.mu.Lock()
	want, gpuAvg := st.CPUAvgPowerWatts, gpuAvgPower(st)
	s.mu.Unlock()
	if gpuAvg <= 0 {
		t.Fatalf("the node's GPUs draw %v W; the test needs GPU power to tell CPU from node power", gpuAvg)
	}
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("agent read %.4f W, want the CPU's %.4f W (the GPUs draw %.4f W more)", got, want, gpuAvg)
	}
}

// A read of /otlp/telemetry/{node} counts as a read of the legacy JSON
// (/telemetry/{node}?format=legacy): the same "telemetry" event and gauges,
// and its own request route.
func TestOTLPTelemetryRecordsLegacyTelemetryEvent(t *testing.T) {
	s := newSimulatorWithRegisterer(otlpTestModel(90, 2), nil, nil, 10, prometheus.NewRegistry())
	const node = "node-a"

	w := httptest.NewRecorder()
	s.handleTelemetry(w, httptest.NewRequest(http.MethodGet, "/telemetry/"+node+"?format=legacy", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("legacy status=%d", w.Code)
	}
	otlpGet(t, s, node)

	s.mu.Lock()
	events := append([]simEvent(nil), s.events...)
	s.mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("got %d events, want one per read: %+v", len(events), events)
	}
	legacy, otlp := events[0], events[1]
	if otlp.Kind != "telemetry" || otlp.Node != node {
		t.Fatalf("OTLP read recorded kind=%q node=%q, want telemetry and %s", otlp.Kind, otlp.Node, node)
	}
	if len(otlp.Payload) != len(legacy.Payload) {
		t.Errorf("payload keys: OTLP %d, legacy %d", len(otlp.Payload), len(legacy.Payload))
	}
	for k := range legacy.Payload {
		if _, ok := otlp.Payload[k]; !ok {
			t.Errorf("OTLP read's event lacks %s", k)
		}
	}
	if got, want := readGaugeValue(s.nodePowerW.WithLabelValues(node)), otlp.Payload["packagePowerWatts"].(float64); got != want {
		t.Errorf("node power gauge %v, want the OTLP read's %v", got, want)
	}
	m := &dto.Metric{}
	if err := s.requestsTotal.WithLabelValues("otlp_telemetry", http.MethodGet, "200").Write(m); err != nil {
		t.Fatal(err)
	}
	if got := m.GetCounter().GetValue(); got != 1 {
		t.Errorf("requests{route=otlp_telemetry,method=GET,status=200} = %v, want 1", got)
	}

	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/otlp/telemetry/" + node, http.StatusMethodNotAllowed},
		{http.MethodGet, "/otlp/telemetry/", http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		s.handleOTLPTelemetry(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Errorf("%s %s: status %d, want %d", tc.method, tc.path, w.Code, tc.status)
		}
	}
	s.mu.Lock()
	n := len(s.events)
	s.mu.Unlock()
	if n != 2 {
		t.Errorf("a refused request recorded an event: %d events, want 2", n)
	}
}

// The fake Prometheus answers each query the controller manager's otel and
// legacy facility presets send, under every translation strategy: ambient,
// IT and cooling from the facility gauges, and an empty vector for the zone,
// rack and node inlet inputs, which the legacy model does not have.
func TestFakePrometheusAnswersEveryFacilityPresetQuery(t *testing.T) {
	s := newSimulatorWithRegisterer(simModel{BaseIdleW: 100, DefaultCapW: 5000}, nil, nil, 10, prometheus.NewRegistry())
	s.facilityAmbientTemp.Set(18.25)
	s.facilityITPowerW.Set(3000.5)
	s.facilityCoolingPowerW.Set(512.75)
	s.facilityPUE.Set(1.17)
	want := map[inputs.FacilityInput]float64{
		inputs.FacilityInputAmbientTemp:  18.25,
		inputs.FacilityInputITPower:      3000.5,
		inputs.FacilityInputCoolingPower: 512.75,
	}
	presetInputs := map[inputs.FacilityPreset]int{inputs.FacilityPresetOTel: 6, inputs.FacilityPresetLegacy: 3}

	for preset, n := range presetInputs {
		for _, strategy := range promname.Strategies() {
			queries, err := inputs.FacilitySpec{Preset: preset, Naming: strategy}.Queries()
			if err != nil {
				t.Fatalf("%s/%s: %v", preset, strategy, err)
			}
			if len(queries) != n {
				t.Fatalf("%s/%s: %d queries, want %d", preset, strategy, len(queries), n)
			}
			for input, q := range queries {
				w := httptest.NewRecorder()
				s.handleFakePrometheusQuery(w, httptest.NewRequest(http.MethodGet, "/api/v1/query?"+url.Values{"query": {q.PromQL}}.Encode(), nil))
				if w.Code != http.StatusOK {
					t.Fatalf("%s/%s %s: status %d", preset, strategy, input, w.Code)
				}
				v, answered := want[input]
				if !answered {
					if got := fakeQuerySeries(t, w); got != 0 {
						t.Errorf("%s/%s %s: %d series, want an empty vector\nquery: %s", preset, strategy, input, got, q.PromQL)
					}
					continue
				}
				if got := parsePrometheusScalarResponse(t, w.Body.Bytes()); got != v {
					t.Errorf("%s/%s %s = %v, want %v\nquery: %s", preset, strategy, input, got, v, q.PromQL)
				}
			}
		}
	}
}

// The regression tests spell the default preset's zone and node inlet
// queries out, so they compile against the old code; they must stay the
// queries the controller manager sends.
func TestRegressionQueriesAreTheControllerQueries(t *testing.T) {
	queries, err := inputs.FacilitySpec{}.Queries()
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range map[inputs.FacilityInput]string{
		inputs.FacilityInputZoneAmbientTemp: regressionZoneAmbientQuery,
		inputs.FacilityInputNodeInletTemp:   regressionNodeInletQuery,
	} {
		if got := queries[input].PromQL; got != want {
			t.Errorf("%s query changed; update the regression constant:\n got: %s\nwant: %s", input, got, want)
		}
	}
}
