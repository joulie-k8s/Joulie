package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// get, set and setTopology read and write facilityMetrics in the tests;
// the controller manager stores whole polls with store.
func (f *facilityMetrics) get() (ambientC, itPowerW, coolingW float64) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.readings.ambientC, f.readings.itPowerW, f.readings.coolingPowerW
}

// set stores the three cluster-wide readings, all known.
func (f *facilityMetrics) set(ambientC, itPowerW, coolingW float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readings.ambientC, f.readings.ambientKnown = ambientC, true
	f.readings.itPowerW, f.readings.itKnown = itPowerW, true
	f.readings.coolingPowerW, f.readings.coolingKnown = coolingW, true
	f.lastUpdated = time.Now()
}

// setTopology stores per-zone ambient temperatures and per-rack power.
func (f *facilityMetrics) setTopology(zoneTemps map[string]float64, rackPower map[string]float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readings.zoneAmbientC = cloneFloatMap(zoneTemps)
	f.readings.rackPowerW = cloneFloatMap(rackPower)
}

func TestFacilityMetricsGetReturnsZerosWhenNotSet(t *testing.T) {
	t.Parallel()
	fm := &facilityMetrics{}
	ambient, itPower, cooling := fm.get()
	if ambient != 0 || itPower != 0 || cooling != 0 {
		t.Fatalf("expected zeros, got ambient=%v itPower=%v cooling=%v", ambient, itPower, cooling)
	}
}

func TestFacilityMetricsSetAndGet(t *testing.T) {
	t.Parallel()
	fm := &facilityMetrics{}
	fm.set(25.5, 10000, 3000)

	ambient, itPower, cooling := fm.get()
	if ambient != 25.5 {
		t.Fatalf("ambient=%v want=25.5", ambient)
	}
	if itPower != 10000 {
		t.Fatalf("itPower=%v want=10000", itPower)
	}
	if cooling != 3000 {
		t.Fatalf("cooling=%v want=3000", cooling)
	}
}

func TestFacilityMetricsOverwrite(t *testing.T) {
	t.Parallel()
	fm := &facilityMetrics{}
	fm.set(20, 5000, 1000)
	fm.set(30, 8000, 2500)

	ambient, itPower, cooling := fm.get()
	if ambient != 30 || itPower != 8000 || cooling != 2500 {
		t.Fatalf("overwrite failed: ambient=%v itPower=%v cooling=%v", ambient, itPower, cooling)
	}
}

func TestFacilityMetricsConcurrentAccess(t *testing.T) {
	t.Parallel()
	fm := &facilityMetrics{}
	var wg sync.WaitGroup

	// Concurrent writers.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(v float64) {
			defer wg.Done()
			fm.set(v, v*100, v*50)
		}(float64(i))
	}

	// Concurrent readers.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fm.get()
		}()
	}

	wg.Wait()
	// If we get here without a race panic, the mutex is working.
	// Do a final read to confirm the data is consistent.
	ambient, itPower, cooling := fm.get()
	if itPower != ambient*100 || cooling != ambient*50 {
		t.Fatalf("inconsistent state: ambient=%v itPower=%v cooling=%v", ambient, itPower, cooling)
	}
}

func TestQueryPrometheusScalarSuccess(t *testing.T) {
	t.Parallel()
	response := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"resultType": "vector",
			"result": []interface{}{
				map[string]interface{}{
					"value": []interface{}{1234567890.0, "42.5"},
				},
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := server.Client()
	val, err := queryPrometheusScalar(context.Background(), client, server.URL, "test_metric")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != 42.5 {
		t.Fatalf("val=%v want=42.5", val)
	}
}

func TestQueryPrometheusScalarEmptyResult(t *testing.T) {
	t.Parallel()
	response := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"resultType": "vector",
			"result":     []interface{}{},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := server.Client()
	_, err := queryPrometheusScalar(context.Background(), client, server.URL, "missing_metric")
	if err == nil {
		t.Fatal("expected error for empty result")
	}
}

func TestQueryPrometheusScalarErrorStatus(t *testing.T) {
	t.Parallel()
	response := map[string]interface{}{
		"status": "error",
		"data": map[string]interface{}{
			"resultType": "vector",
			"result":     []interface{}{},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := server.Client()
	_, err := queryPrometheusScalar(context.Background(), client, server.URL, "test_metric")
	if err == nil {
		t.Fatal("expected error for error status")
	}
}

func TestQueryPrometheusScalarInvalidJSON(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	defer server.Close()

	client := server.Client()
	_, err := queryPrometheusScalar(context.Background(), client, server.URL, "test_metric")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestQueryPrometheusScalarHTTPError(t *testing.T) {
	t.Parallel()
	// Use a URL that will fail to connect.
	client := &http.Client{}
	_, err := queryPrometheusScalar(context.Background(), client, "http://127.0.0.1:0", "test_metric")
	if err == nil {
		t.Fatal("expected error for connection failure")
	}
}

func TestQueryPrometheusScalarNonStringValue(t *testing.T) {
	t.Parallel()
	response := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"resultType": "vector",
			"result": []interface{}{
				map[string]interface{}{
					"value": []interface{}{1234567890.0, 42.5},
				},
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := server.Client()
	_, err := queryPrometheusScalar(context.Background(), client, server.URL, "test_metric")
	if err == nil {
		t.Fatal("expected error when value is not a string")
	}
}

// otelFacilityProm answers each query of the otel facility preset (default
// naming) with the body of its input, and refuses any other query.
func otelFacilityProm(t *testing.T, answers map[inputs.FacilityInput]string) *regProm {
	t.Helper()
	queries, err := inputs.FacilitySpec{}.Queries()
	if err != nil {
		t.Fatal(err)
	}
	byQuery := map[string]string{}
	for in, q := range queries {
		body, ok := answers[in]
		if !ok {
			body = regVector()
		}
		byQuery[q.PromQL] = body
	}
	return newRegProm(t, func(q string) (int, string) {
		if body, ok := byQuery[q]; ok {
			return 200, body
		}
		return 400, `{"status":"error","errorType":"bad_data","error":"not a query of the otel preset"}`
	})
}

// The otel preset reads all six inputs with one query each, keyed by zone,
// rack and node, and exports the cluster-wide readings as gauges. A reading
// a later poll loses becomes unknown.
func TestFetchFacilityMetricsOTelPreset(t *testing.T) {
	prom := otelFacilityProm(t, map[inputs.FacilityInput]string{
		inputs.FacilityInputAmbientTemp:  regVector(regSeries(`{}`, "18.5")),
		inputs.FacilityInputITPower:      regVector(regSeries(`{}`, "15000")),
		inputs.FacilityInputCoolingPower: regVector(regSeries(`{}`, "4000")),
		inputs.FacilityInputZoneAmbientTemp: regVector(regSeries(`{"joulie_cooling_zone_name":"z1"}`, "25"),
			regSeries(`{"joulie_cooling_zone_name":"z2"}`, "27")),
		inputs.FacilityInputRackPower: regVector(regSeries(`{"joulie_rack_name":"r1"}`, "10000"),
			regSeries(`{"joulie_rack_name":"r2"}`, "7000")),
		inputs.FacilityInputNodeInletTemp: regVector(regSeries(`{"k8s_node_name":"n1"}`, "23")),
	})
	cfg := defaultFacilityConfig()
	cfg.enabled = true
	cfg.prometheusAddress = prom.URL
	fm := &facilityMetrics{}
	rackOK := testutil.ToFloat64(twinInputQueries.WithLabelValues(string(inputs.FacilityInputRackPower), inputResultSuccess))

	fetchFacilityMetrics(context.Background(), prom.Client(), fm, cfg)

	want := facilitySnapshot{
		ambientC: 18.5, ambientKnown: true,
		itPowerW: 15000, itKnown: true,
		coolingPowerW: 4000, coolingKnown: true,
		zoneAmbientC: map[string]float64{"z1": 25, "z2": 27},
		rackPowerW:   map[string]float64{"r1": 10000, "r2": 7000},
		nodeInletC:   map[string]float64{"n1": 23},
	}
	if got := fm.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot %+v, want %+v", got, want)
	}
	if n := len(prom.seen()); n != 6 {
		t.Fatalf("%d queries, want one per input: %q", n, prom.seen())
	}
	if v := testutil.ToFloat64(twinFacilityAmbientTemp.WithLabelValues("otel")); v != 18.5 {
		t.Errorf("ambient gauge %v, want 18.5", v)
	}
	if v := testutil.ToFloat64(twinFacilityITPower.WithLabelValues("otel")); v != 15000 {
		t.Errorf("IT power gauge %v, want 15000", v)
	}
	if d := testutil.ToFloat64(twinInputQueries.WithLabelValues(string(inputs.FacilityInputRackPower), inputResultSuccess)) - rackOK; d != 1 {
		t.Errorf("rack_power success counter moved by %v, want 1", d)
	}

	empty := otelFacilityProm(t, nil)
	cfg.prometheusAddress = empty.URL
	fetchFacilityMetrics(context.Background(), empty.Client(), fm, cfg)
	if got := fm.snapshot(); !reflect.DeepEqual(got, facilitySnapshot{}) {
		t.Fatalf("after a poll without data the snapshot is %+v, want every reading unknown", got)
	}
	if n := testutil.CollectAndCount(twinFacilityAmbientTemp); n != 0 {
		t.Errorf("ambient gauge still has %d series after the reading was lost", n)
	}

	// A poll replaces the previous one whole: an input it does not query
	// (the legacy preset has no zone, rack or inlet query) is unknown
	// afterwards, not the earlier reading.
	cfg.prometheusAddress = prom.URL
	fetchFacilityMetrics(context.Background(), prom.Client(), fm, cfg)
	cfg.spec.Preset = inputs.FacilityPresetLegacy
	cfg.prometheusAddress = empty.URL
	fetchFacilityMetrics(context.Background(), empty.Client(), fm, cfg)
	if got := fm.snapshot(); !reflect.DeepEqual(got, facilitySnapshot{}) {
		t.Fatalf("after a legacy poll the snapshot is %+v, want every reading unknown", got)
	}
}

// A FACILITY_*_METRIC set for the old reader holds a bare metric name, which
// may have one series per sensor. Through its alias it is aggregated (avg
// for a temperature), so it reads one value instead of failing on several.
func TestFacilityLegacyMetricAliasBareNameIsAggregated(t *testing.T) {
	clearInputEnv(t)
	t.Setenv("ENABLE_FACILITY_METRICS", "true")
	t.Setenv("FACILITY_AMBIENT_TEMP_METRIC", "site_ambient_celsius")
	buf := captureLog(t)
	cfg, err := loadInputConfig()
	if err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "FACILITY_AMBIENT_TEMP_METRIC") || !strings.Contains(out, "FACILITY_AMBIENT_TEMP_QUERY") || !strings.Contains(out, inputAliasesRemovedIn) {
		t.Fatalf("deprecation warning %q, want the old and new names and %s", out, inputAliasesRemovedIn)
	}

	prom := newRegProm(t, func(q string) (int, string) {
		switch q {
		case "avg(site_ambient_celsius)":
			return 200, regVector(regSeries(`{}`, "21"))
		case "site_ambient_celsius":
			return 200, regVector(regSeries(`{"sensor":"a"}`, "20"), regSeries(`{"sensor":"b"}`, "22"))
		}
		return 200, regVector()
	})
	cfg.facility.prometheusAddress = prom.URL
	fm := &facilityMetrics{}
	fetchFacilityMetrics(context.Background(), prom.Client(), fm, cfg.facility)
	if got := fm.snapshot(); !got.ambientKnown || got.ambientC != 21 {
		t.Fatalf("ambient %v known=%v, want 21 from avg(site_ambient_celsius); queries %q", got.ambientC, got.ambientKnown, prom.seen())
	}
}

// An override that is not a bare name is queried as written, so a sensor
// selector may match several series. One of them is not the facility's
// ambient: the reading is unknown and counted as an error.
func TestFetchFacilityMetricsRejectsSeveralSeriesForOneValue(t *testing.T) {
	const query = `site_ambient_celsius{sensor=~".+"}`
	prom := newRegProm(t, func(q string) (int, string) {
		if q == query {
			return 200, regVector(regSeries(`{"sensor":"a"}`, "20"), regSeries(`{"sensor":"b"}`, "35"))
		}
		return 200, regVector()
	})
	cfg := defaultFacilityConfig()
	cfg.enabled = true
	cfg.prometheusAddress = prom.URL
	cfg.spec.Overrides = map[inputs.FacilityInput]string{inputs.FacilityInputAmbientTemp: query}
	fm := &facilityMetrics{}
	errorCount := func() float64 {
		return testutil.ToFloat64(twinInputQueries.WithLabelValues(string(inputs.FacilityInputAmbientTemp), inputResultError))
	}
	before := errorCount()

	fetchFacilityMetrics(context.Background(), prom.Client(), fm, cfg)

	if got := fm.snapshot(); got.ambientKnown {
		t.Fatalf("ambient %v known from two series, want unknown; queries %q", got.ambientC, prom.seen())
	}
	if d := errorCount() - before; d != 1 {
		t.Fatalf("ambient_temp error counter moved by %v, want 1", d)
	}
}

// The twin uses the most local ambient temperature it knows: the node's
// inlet, then its cooling zone's, then the facility's.
func TestNodeInletOverridesZoneAndFacility(t *testing.T) {
	prom := otelFacilityProm(t, map[inputs.FacilityInput]string{
		inputs.FacilityInputAmbientTemp:     regVector(regSeries(`{}`, "10")),
		inputs.FacilityInputZoneAmbientTemp: regVector(regSeries(`{"joulie_cooling_zone_name":"z1"}`, "30")),
		inputs.FacilityInputNodeInletTemp:   regVector(regSeries(`{"k8s_node_name":"zone-node"}`, "40")),
	})
	regWithNodePowerVars(t, "http", prom.URL+"/telemetry/{node}", "", "")
	objs := regZoneNode(t)
	cfg := defaultFacilityConfig()
	cfg.enabled = true
	cfg.prometheusAddress = prom.URL
	fm := &facilityMetrics{}
	fetchFacilityMetrics(context.Background(), prom.Client(), fm, cfg)

	// 100 W of 200 W TDP: 50 / max(0.5, 1 - (T - 20) * 0.02).
	stress := func(tempC float64) float64 { return 50 / math.Max(0.5, 1-(tempC-20)*0.02) }
	if got := regCoolingStress(t, fm, "zone-node", objs...); math.Abs(got-stress(40)) > 1e-6 {
		t.Fatalf("cooling stress %v, want %v from the 40 C inlet", got, stress(40))
	}
	s := fm.snapshot()
	s.nodeInletC = nil
	fm.store(s)
	if got := regCoolingStress(t, fm, "zone-node", objs...); math.Abs(got-stress(30)) > 1e-6 {
		t.Fatalf("without an inlet reading: cooling stress %v, want %v from the 30 C zone", got, stress(30))
	}
	s.zoneAmbientC = nil
	fm.store(s)
	if got := regCoolingStress(t, fm, "zone-node", objs...); math.Abs(got-stress(10)) > 1e-6 {
		t.Fatalf("without zone and inlet readings: cooling stress %v, want %v from the 10 C facility", got, stress(10))
	}
}

// A rack the facility measures gets its reading; another keeps the sum of
// its nodes' estimates.
func TestNodeTopologyPrefersMeasuredRackPower(t *testing.T) {
	facility := facilitySnapshot{rackPowerW: map[string]float64{"r1": 9000}}
	estimates := map[string]float64{"r1": 4000, "r2": 3000}
	if topo := nodeTopologyFor("n1", "r1", "", estimates, facility); topo == nil || topo.rackTotalPowerW != 9000 {
		t.Fatalf("rack r1 topology %+v, want the measured 9000 W", topo)
	}
	if topo := nodeTopologyFor("n2", "r2", "", estimates, facility); topo == nil || topo.rackTotalPowerW != 3000 {
		t.Fatalf("rack r2 topology %+v, want the 3000 W estimate", topo)
	}
	if topo := nodeTopologyFor("n3", "", "", estimates, facility); topo != nil {
		t.Fatalf("a node without rack, zone or inlet got topology %+v", topo)
	}
	facility.nodeInletC = map[string]float64{"n3": 0}
	if topo := nodeTopologyFor("n3", "", "", estimates, facility); topo == nil || !topo.inletKnown || topo.inletC != 0 {
		t.Fatalf("a node with a 0 C inlet reading got topology %+v, want the reading known", topo)
	}
}

// The loop reads the facility at start instead of one interval later.
func TestFacilityMetricsLoopPollsAtStart(t *testing.T) {
	prom := otelFacilityProm(t, map[inputs.FacilityInput]string{
		inputs.FacilityInputAmbientTemp: regVector(regSeries(`{}`, "12")),
	})
	cfg := defaultFacilityConfig()
	cfg.enabled = true
	cfg.prometheusAddress = prom.URL
	cfg.pollInterval = time.Hour
	fm := &facilityMetrics{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		facilityMetricsLoop(ctx, fm, cfg)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !fm.snapshot().ambientKnown {
		if time.Now().After(deadline) {
			t.Fatal("no facility reading 5 s after start with a 1 h poll interval")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// When the otel facility preset reads nothing, the hint names
// FACILITY_PRESET=legacy for the inputs the legacy preset reads, and only
// for those.
func TestFacilityNoDataHintNamesTheLegacyPreset(t *testing.T) {
	for input, legacy := range map[inputs.FacilityInput]bool{
		inputs.FacilityInputAmbientTemp:     true,
		inputs.FacilityInputITPower:         true,
		inputs.FacilityInputCoolingPower:    true,
		inputs.FacilityInputZoneAmbientTemp: false,
		inputs.FacilityInputRackPower:       false,
		inputs.FacilityInputNodeInletTemp:   false,
	} {
		h := facilityHint(input, string(inputs.FacilityPresetOTel), inputs.ErrNoData)
		if got := strings.Contains(h, "FACILITY_PRESET=legacy"); got != legacy {
			t.Errorf("%s no-data hint %q names FACILITY_PRESET=legacy: %t, want %t", input, h, got, legacy)
		}
		if !strings.Contains(h, facilityQueryEnv(input)) {
			t.Errorf("%s no-data hint %q does not name %s", input, h, facilityQueryEnv(input))
		}
	}
}
