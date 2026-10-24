package main

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/controller/policy"
	"github.com/matbun/joulie/pkg/kube"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/labels"
)

// An operator who exports grid carbon intensity points
// FACILITY_CARBON_INTENSITY_QUERY at it. A WattTime MOER of 1000 lbs CO2/MWh
// is read, converted to 453.59 gCO2eq/kWh, counted in
// joulie_twin_input_queries_total{input="carbon_intensity"} and exported
// under its OpenTelemetry name with its signal; a poll that reads nothing
// leaves no series. Before, the variable was ignored: no query ran and
// nothing was exported (#41). The test names only what existed before, so
// it compiles and fails on the old code.
func TestCarbonIntensityQueryIsReadConvertedAndExported(t *testing.T) {
	clearInputEnv(t)
	const query = `watttime_co2_moer{region="CAISO_NORTH"}`
	t.Setenv("ENABLE_FACILITY_METRICS", "true")
	t.Setenv("FACILITY_PRESET", "custom")
	t.Setenv("FACILITY_CARBON_INTENSITY_QUERY", query)
	t.Setenv("FACILITY_CARBON_INTENSITY_UNIT", "lbsCO2/MWh")
	t.Setenv("FACILITY_CARBON_INTENSITY_SIGNAL", "marginal")
	cfg, err := loadInputConfig()
	if err != nil {
		t.Fatal(err)
	}
	prom := newRegProm(t, func(q string) (int, string) {
		if q == query {
			return 200, regVector(regSeries(`{"region":"CAISO_NORTH"}`, "1000"))
		}
		return 200, regVector()
	})
	reg := prometheus.NewPedanticRegistry()
	registerMetrics(reg)
	count := func(result string) float64 {
		return testutil.ToFloat64(twinInputQueries.WithLabelValues("carbon_intensity", result))
	}
	const name, signalLabel = "joulie_grid_carbon_intensity_grams_per_kW_h", "joulie_grid_carbon_intensity_signal"
	fm := &facilityMetrics{}
	ok0, noData0 := count(inputResultSuccess), count(inputResultNoData)

	cfg.facility.prometheusAddress = prom.URL
	fetchFacilityMetrics(context.Background(), prom.Client(), fm, cfg.facility)

	if got := prom.seen(); len(got) != 1 || got[0] != query {
		t.Fatalf("queries %q, want FACILITY_CARBON_INTENSITY_QUERY alone", got)
	}
	if d := count(inputResultSuccess) - ok0; d != 1 {
		t.Errorf("carbon_intensity success counter moved by %v, want 1", d)
	}
	v, ok := gatherValue(t, reg, name, map[string]string{signalLabel: "marginal"})
	if !ok || math.Abs(v-453.59237) > 1e-9 {
		t.Fatalf("%s{%s=marginal} = %v (exported %v), want 453.59237 gCO2eq/kWh", name, signalLabel, v, ok)
	}

	empty := newRegProm(t, func(string) (int, string) { return 200, regVector() })
	cfg.facility.prometheusAddress = empty.URL
	fetchFacilityMetrics(context.Background(), empty.Client(), fm, cfg.facility)
	if _, ok := gatherValue(t, reg, name, map[string]string{signalLabel: "marginal"}); ok {
		t.Error("a poll that read no carbon intensity still exports the last one")
	}
	if d := count(inputResultNoData) - noData0; d != 1 {
		t.Errorf("carbon_intensity no_data counter moved by %v, want 1", d)
	}
}

// The snapshot holds the converted reading with its signal, a failed query
// makes it unknown, and the gauge carries one series, of the signal last
// read.
func TestFacilityCarbonIntensitySnapshotAndGauge(t *testing.T) {
	healthy := true
	prom := newRegProm(t, func(q string) (int, string) {
		if !healthy {
			return 503, `{"status":"error","errorType":"unavailable","error":"down"}`
		}
		return 200, regVector(regSeries(`{}`, "0.212"))
	})
	cfg := defaultFacilityConfig()
	cfg.enabled = true
	cfg.prometheusAddress = prom.URL
	cfg.spec.Preset = inputs.FacilityPresetCustom
	cfg.spec.Overrides = map[inputs.FacilityInput]string{inputs.FacilityInputCarbonIntensity: "grid_carbon_intensity_kg_per_kwh"}
	cfg.spec.CarbonIntensity = inputs.CarbonIntensitySpec{Unit: "kgCO2eq/kWh", Signal: inputs.CarbonSignalAverage}
	fm := &facilityMetrics{}
	t.Cleanup(func() { setCarbonIntensityGauge(inputs.CarbonIntensity{}, false) })

	fetchFacilityMetrics(context.Background(), prom.Client(), fm, cfg)
	s := fm.snapshot()
	if !s.carbonKnown || s.carbon.Signal != inputs.CarbonSignalAverage || math.Abs(s.carbon.GramsPerKWh-212) > 1e-9 {
		t.Fatalf("carbon %+v known=%v, want 212 gCO2eq/kWh average from 0.212 kg/kWh; queries %q", s.carbon, s.carbonKnown, prom.seen())
	}
	if got := prom.seen(); len(got) != 1 || got[0] != "avg(grid_carbon_intensity_kg_per_kwh)" {
		t.Errorf("queries %q, want the bare name averaged", got)
	}

	errors0 := testutil.ToFloat64(twinInputQueries.WithLabelValues(string(inputs.FacilityInputCarbonIntensity), inputResultError))
	healthy = false
	fetchFacilityMetrics(context.Background(), prom.Client(), fm, cfg)
	if s := fm.snapshot(); s.carbonKnown || s.carbon != (inputs.CarbonIntensity{}) {
		t.Errorf("after a failed query: carbon %+v known=%v, want unknown", s.carbon, s.carbonKnown)
	}
	if d := testutil.ToFloat64(twinInputQueries.WithLabelValues(string(inputs.FacilityInputCarbonIntensity), inputResultError)) - errors0; d != 1 {
		t.Errorf("carbon_intensity error counter moved by %v, want 1", d)
	}
	if n := testutil.CollectAndCount(gridCarbonIntensity); n != 0 {
		t.Errorf("the gauge has %d series while the reading is unknown", n)
	}

	setCarbonIntensityGauge(inputs.CarbonIntensity{GramsPerKWh: 400, Signal: inputs.CarbonSignalMarginal}, true)
	setCarbonIntensityGauge(inputs.CarbonIntensity{GramsPerKWh: 300, Signal: inputs.CarbonSignalAverage}, true)
	if n := testutil.CollectAndCount(gridCarbonIntensity); n != 1 {
		t.Errorf("the gauge has %d series, want only the signal last read", n)
	}
	if v := testutil.ToFloat64(gridCarbonIntensity.WithLabelValues("average")); v != 300 {
		t.Errorf("average series %v, want 300", v)
	}
}

// A negative carbon intensity, which no grid has by either signal, comes
// from a broken exporter or query: the reading is unknown, the query counts
// as an error, and no series is exported. Before, it reached the gauge and
// the policy as a known reading.
func TestFacilityNegativeCarbonIntensityIsUnknown(t *testing.T) {
	prom := newRegProm(t, func(string) (int, string) {
		return 200, regVector(regSeries(`{}`, "-250"))
	})
	cfg := defaultFacilityConfig()
	cfg.enabled = true
	cfg.prometheusAddress = prom.URL
	cfg.spec.Preset = inputs.FacilityPresetCustom
	cfg.spec.Overrides = map[inputs.FacilityInput]string{inputs.FacilityInputCarbonIntensity: "grid_carbon_intensity"}
	cfg.spec.CarbonIntensity = inputs.CarbonIntensitySpec{Signal: inputs.CarbonSignalMarginal}
	fm := &facilityMetrics{}
	t.Cleanup(func() { setCarbonIntensityGauge(inputs.CarbonIntensity{}, false) })
	setCarbonIntensityGauge(inputs.CarbonIntensity{GramsPerKWh: 300, Signal: inputs.CarbonSignalMarginal}, true)
	errors0 := testutil.ToFloat64(twinInputQueries.WithLabelValues(string(inputs.FacilityInputCarbonIntensity), inputResultError))

	fetchFacilityMetrics(context.Background(), prom.Client(), fm, cfg)

	s := fm.snapshot()
	if s.carbonKnown {
		t.Errorf("carbon %+v known after a reply of -250, want unknown", s.carbon)
	}
	if p := s.forPolicy(); p.CarbonIntensityKnown {
		t.Errorf("the policy gets carbon %+v as known", p.CarbonIntensity)
	}
	if d := testutil.ToFloat64(twinInputQueries.WithLabelValues(string(inputs.FacilityInputCarbonIntensity), inputResultError)) - errors0; d != 1 {
		t.Errorf("carbon_intensity error counter moved by %v, want 1", d)
	}
	if n := testutil.CollectAndCount(gridCarbonIntensity); n != 0 {
		t.Errorf("the gauge has %d series after a negative reading, want none", n)
	}
}

// FACILITY_CARBON_INTENSITY_QUERY needs a signal and a known unit; the unit
// defaults to gCO2eq/kWh; a unit or signal without the query is only a
// warning; the start-up line names the unit and the signal.
func TestLoadInputConfigCarbonIntensity(t *testing.T) {
	clearInputEnv(t)
	t.Setenv("ENABLE_FACILITY_METRICS", "true")
	t.Setenv("FACILITY_CARBON_INTENSITY_QUERY", "grid_carbon_intensity")
	t.Setenv("FACILITY_CARBON_INTENSITY_SIGNAL", " Average ")
	cfg, err := loadInputConfig()
	if err != nil {
		t.Fatal(err)
	}
	if want := (inputs.CarbonIntensitySpec{Signal: inputs.CarbonSignalAverage}); cfg.facility.spec.CarbonIntensity != want {
		t.Errorf("carbon spec %+v, want %+v", cfg.facility.spec.CarbonIntensity, want)
	}
	qs, _ := cfg.facility.spec.Queries()
	if q := qs[inputs.FacilityInputCarbonIntensity]; q.PromQL != "avg(grid_carbon_intensity)" || q.GroupLabel != "" {
		t.Errorf("carbon query %+v, want avg(grid_carbon_intensity), one value", q)
	}
	if len(qs) != 7 {
		t.Errorf("%d facility queries, want the six of the otel preset and the carbon intensity", len(qs))
	}
	buf := captureLog(t)
	cfg.logSummary()
	if out := buf.String(); !strings.Contains(out, `input facility carbon_intensity:`) || !strings.Contains(out, `unit="gCO2eq/kWh" signal=average`) {
		t.Errorf("start-up log %q, want the carbon intensity line with its unit and signal", out)
	}

	for _, c := range []struct{ unit, signal, mention string }{
		{"", "", "FACILITY_CARBON_INTENSITY_SIGNAL"},
		{"", "forecast", "FACILITY_CARBON_INTENSITY_SIGNAL"},
		{"gCO2eq/MJ", "marginal", "FACILITY_CARBON_INTENSITY_UNIT"},
	} {
		t.Setenv("FACILITY_CARBON_INTENSITY_UNIT", c.unit)
		t.Setenv("FACILITY_CARBON_INTENSITY_SIGNAL", c.signal)
		if _, err := loadInputConfig(); err == nil || !strings.Contains(err.Error(), c.mention) {
			t.Errorf("unit %q signal %q: error %v, want one naming %s", c.unit, c.signal, err, c.mention)
		}
	}

	t.Setenv("FACILITY_CARBON_INTENSITY_QUERY", "")
	t.Setenv("FACILITY_CARBON_INTENSITY_UNIT", "furlongs")
	t.Setenv("FACILITY_CARBON_INTENSITY_SIGNAL", "")
	buf = captureLog(t)
	cfg, err = loadInputConfig()
	if err != nil {
		t.Fatalf("a unit without a query: %v", err)
	}
	if _, ok := mustQueries(t, cfg.facility.spec)[inputs.FacilityInputCarbonIntensity]; ok {
		t.Error("a carbon intensity query without FACILITY_CARBON_INTENSITY_QUERY")
	}
	if !strings.Contains(buf.String(), "without FACILITY_CARBON_INTENSITY_QUERY") {
		t.Errorf("log %q, want a warning that the unit is unused", buf.String())
	}
}

func mustQueries(t *testing.T, s inputs.FacilitySpec) map[inputs.FacilityInput]inputs.Query {
	t.Helper()
	qs, err := s.Queries()
	if err != nil {
		t.Fatal(err)
	}
	return qs
}

// fullFacilitySnapshot has every reading known and non-zero.
func fullFacilitySnapshot() facilitySnapshot {
	return facilitySnapshot{
		ambientC: 18.5, ambientKnown: true,
		itPowerW: 15000, itKnown: true,
		coolingPowerW: 4000, coolingKnown: true,
		carbon:       inputs.CarbonIntensity{GramsPerKWh: 412, Signal: inputs.CarbonSignalMarginal},
		carbonKnown:  true,
		zoneAmbientC: map[string]float64{"z1": 25},
		rackPowerW:   map[string]float64{"r1": 9000},
		nodeInletC:   map[string]float64{"zone-node": 23},
	}
}

// Every reading of the snapshot reaches the policy, and the policy's maps
// are its own. A reading added to the snapshot without the policy's view
// fails the field count.
func TestFacilityForPolicyCarriesEveryReading(t *testing.T) {
	s := fullFacilitySnapshot()
	p := s.forPolicy()
	if ns, np := reflect.TypeOf(s).NumField(), reflect.TypeOf(p).NumField(); ns != np {
		t.Fatalf("facilitySnapshot has %d fields, policy.Facility %d: give the policy every reading", ns, np)
	}
	v := reflect.ValueOf(p)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("policy.Facility.%s is zero from a snapshot with every reading", v.Type().Field(i).Name)
		}
	}
	want := policy.Facility{
		CarbonIntensity: inputs.CarbonIntensity{GramsPerKWh: 412, Signal: inputs.CarbonSignalMarginal}, CarbonIntensityKnown: true,
		AmbientC: 18.5, AmbientKnown: true, ITPowerW: 15000, ITPowerKnown: true, CoolingPowerW: 4000, CoolingPowerKnown: true,
		ZoneAmbientC: map[string]float64{"z1": 25}, RackPowerW: map[string]float64{"r1": 9000}, NodeInletC: map[string]float64{"zone-node": 23},
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("forPolicy %+v, want %+v", p, want)
	}
	p.RackPowerW["r1"] = 0
	if s.rackPowerW["r1"] != 9000 {
		t.Error("a write to the policy's map reached the snapshot")
	}
}

// The policy of a reconcile plans with the facility snapshot the twins of
// that reconcile read, carbon intensity included, taken before it plans.
// Before, the plan step got no facility: buildPlanByPolicy ran before the
// reconcile read the snapshot (#41).
func TestPolicyPlansWithTheFacilitySnapshotOfItsReconcile(t *testing.T) {
	var got []policy.Facility
	prev := planByPolicy
	planByPolicy = func(ctx context.Context, reader kube.Reader, policyType string, nodes []string,
		hw map[string]NodeHardware, current map[string]string, facility policy.Facility, interval time.Duration,
		perfCap, ecoCap, staticHPFrac, queueHPBaseFrac float64, queueHPMin, queueHPMax, queuePerfPerHPNode int,
	) []NodeAssignment {
		got = append(got, facility)
		return prev(ctx, reader, policyType, nodes, hw, current, facility, interval,
			perfCap, ecoCap, staticHPFrac, queueHPBaseFrac, queueHPMin, queueHPMax, queuePerfPerHPNode)
	}
	t.Cleanup(func() {
		planByPolicy = prev
		setTwinFacility(facilitySnapshot{})
	})
	fm := &facilityMetrics{}
	fm.store(fullFacilitySnapshot())

	reader, client, dyn := fakeClients(t, regZoneNode(t)...)
	selector, err := labels.Parse("joulie.io/managed=true")
	if err != nil {
		t.Fatal(err)
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
	if len(got) != 1 {
		t.Fatalf("the policy planned %d times in one reconcile, want 1", len(got))
	}
	if want := fullFacilitySnapshot().forPolicy(); !reflect.DeepEqual(got[0], want) {
		t.Fatalf("the policy planned with %+v, want the reconcile's snapshot %+v", got[0], want)
	}
}

// No policy reads the facility yet: every policy plans the same with no
// facility and with every reading known.
func TestPolicyPlansIgnoreTheFacility(t *testing.T) {
	reader := fakeReader(t,
		podWithRequiredPowerProfile("perf-1", "node-a", "performance"),
		podWithRequiredPowerProfile("perf-2", "node-b", "performance"),
	)
	nodes := []string{"node-a", "node-b", "node-c", "node-d"}
	hw := map[string]NodeHardware{}
	for _, n := range nodes {
		hw[n] = NodeHardware{CPUModel: "same-cpu"}
	}
	current := map[string]string{"node-a": "performance"}
	full := fullFacilitySnapshot().forPolicy()
	for _, policyType := range []string{"static_partition", "queue_aware_v1", "rule_swap_v1"} {
		plan := func(f policy.Facility) []NodeAssignment {
			return buildPlanByPolicy(context.Background(), reader, policyType, nodes, hw, current, f,
				time.Hour, 5000, 120, 0.5, 0.25, 1, 4, 1)
		}
		if without, with := plan(policy.Facility{}), plan(full); !reflect.DeepEqual(without, with) {
			t.Errorf("%s plans %+v without a facility and %+v with one", policyType, without, with)
		}
	}
}
