package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/prometheus/client_golang/prometheus"
)

// clearInputEnv unsets every variable loadInputConfig reads, aliases
// included, for the duration of the test.
func clearInputEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"PROMETHEUS_ADDRESS", "PROMETHEUS_NAME_TRANSLATION", "PROMETHEUS_RESOURCE_ATTRIBUTES", "PROMETHEUS_RATE_WINDOW",
		"NODE_POWER_SOURCE", "NODE_POWER_PRESET", "NODE_POWER_SCOPE", "NODE_POWER_NODE_LABEL",
		"NODE_POWER_PROMETHEUS_ADDRESS", "NODE_POWER_PROMETHEUS_QUERY", "NODE_POWER_HTTP_ENDPOINT",
		"ENABLE_FACILITY_METRICS", "FACILITY_PROMETHEUS_ADDRESS", "FACILITY_POLL_INTERVAL", "FACILITY_PRESET",
	}
	for _, in := range inputs.FacilityInputs() {
		keys = append(keys, facilityQueryEnv(in))
	}
	for _, aliases := range []map[string]string{legacyEnvAliases, inputEnvAliases} {
		for _, old := range aliases {
			keys = append(keys, old)
		}
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
	resetDeprecatedEnvWarnings()
}

func TestLoadInputConfigDefaultsAndValidation(t *testing.T) {
	clearInputEnv(t)
	cfg, err := loadInputConfig()
	if err != nil {
		t.Fatal(err)
	}
	wantNode := nodePowerConfig{
		source: "static",
		spec: inputs.NodePowerSpec{
			Preset: inputs.NodePowerPresetOTel, Scope: inputs.ScopeComponents, Naming: promname.Default,
			ResourceAttributes: inputs.ResourceAttributesAuto, Window: 5 * time.Minute,
		},
	}
	if !reflect.DeepEqual(cfg.nodePower, wantNode) {
		t.Errorf("node power defaults %+v, want %+v", cfg.nodePower, wantNode)
	}
	wantFacility := facilityConfig{
		prometheusAddress: "http://prometheus-operated.monitoring:9090",
		pollInterval:      30 * time.Second,
		spec: inputs.FacilitySpec{
			Preset: inputs.FacilityPresetOTel, Naming: promname.Default, Window: 5 * time.Minute,
			Overrides: map[inputs.FacilityInput]string{},
		},
	}
	if !reflect.DeepEqual(cfg.facility, wantFacility) {
		t.Errorf("facility defaults %+v, want %+v", cfg.facility, wantFacility)
	}

	// PROMETHEUS_ADDRESS is the default of both addresses; each input's own
	// variable wins. Shared settings reach both specs.
	t.Setenv("PROMETHEUS_ADDRESS", "http://prom:9090")
	t.Setenv("PROMETHEUS_NAME_TRANSLATION", "nOuTf8EscapingWithSuffixes")
	t.Setenv("PROMETHEUS_RATE_WINDOW", "90s")
	t.Setenv("NODE_POWER_SCOPE", "HOST")
	cfg, err = loadInputConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.nodePower.promAddress != "http://prom:9090" || cfg.facility.prometheusAddress != "http://prom:9090" {
		t.Errorf("addresses %q and %q, want PROMETHEUS_ADDRESS for both", cfg.nodePower.promAddress, cfg.facility.prometheusAddress)
	}
	if cfg.nodePower.spec.Naming != promname.NoUTF8EscapingWithSuffixes || cfg.facility.spec.Naming != promname.NoUTF8EscapingWithSuffixes {
		t.Errorf("naming %q and %q, want NoUTF8EscapingWithSuffixes for both", cfg.nodePower.spec.Naming, cfg.facility.spec.Naming)
	}
	if cfg.nodePower.spec.Window != 90*time.Second || cfg.facility.spec.Window != 90*time.Second {
		t.Errorf("windows %s and %s, want 90s for both", cfg.nodePower.spec.Window, cfg.facility.spec.Window)
	}
	if cfg.nodePower.spec.Scope != inputs.ScopeHost {
		t.Errorf("scope %q, want host", cfg.nodePower.spec.Scope)
	}
	t.Setenv("NODE_POWER_PROMETHEUS_ADDRESS", "http://node-prom:9090")
	t.Setenv("FACILITY_PROMETHEUS_ADDRESS", "http://facility-prom:9090")
	if cfg, err = loadInputConfig(); err != nil {
		t.Fatal(err)
	}
	if cfg.nodePower.promAddress != "http://node-prom:9090" || cfg.facility.prometheusAddress != "http://facility-prom:9090" {
		t.Errorf("addresses %q and %q, want each input's own", cfg.nodePower.promAddress, cfg.facility.prometheusAddress)
	}

	// An unknown source is static with a warning, as before.
	buf := captureLog(t)
	t.Setenv("NODE_POWER_SOURCE", "kepler")
	if cfg, err = loadInputConfig(); err != nil || cfg.nodePower.source != "static" {
		t.Fatalf("NODE_POWER_SOURCE=kepler gave source %q, err %v; want static", cfg.nodePower.source, err)
	}
	if !strings.Contains(buf.String(), `NODE_POWER_SOURCE="kepler"`) {
		t.Errorf("no warning for an unknown source: %q", buf.String())
	}

	// The legacy facility preset still loads, with its removal release.
	t.Setenv("FACILITY_PRESET", "legacy")
	if cfg, err = loadInputConfig(); err != nil || cfg.facility.spec.Preset != inputs.FacilityPresetLegacy {
		t.Fatalf("FACILITY_PRESET=legacy gave %+v, %v", cfg.facility.spec, err)
	}
	if !strings.Contains(buf.String(), "FACILITY_PRESET=legacy") || !strings.Contains(buf.String(), inputAliasesRemovedIn) {
		t.Errorf("no deprecation warning naming %s for the legacy preset: %q", inputAliasesRemovedIn, buf.String())
	}

	invalid := []struct{ env, value, wantInErr string }{
		{"PROMETHEUS_NAME_TRANSLATION", "Underscores", "PROMETHEUS_NAME_TRANSLATION"},
		{"PROMETHEUS_RESOURCE_ATTRIBUTES", "promoted", "promoted"},
		{"PROMETHEUS_RATE_WINDOW", "5s", "PROMETHEUS_RATE_WINDOW"},
		{"PROMETHEUS_RATE_WINDOW", "five minutes", "PROMETHEUS_RATE_WINDOW"},
		{"NODE_POWER_SCOPE", "wall", "wall"},
		{"NODE_POWER_PRESET", "dcgm", "dcgm"},
		{"NODE_POWER_PRESET", "custom", "needs a query"},
		{"FACILITY_PRESET", "datacenter", "datacenter"},
		{"FACILITY_POLL_INTERVAL", "-30s", "FACILITY_POLL_INTERVAL"},
	}
	for _, c := range invalid {
		t.Run(c.env+"="+c.value, func(t *testing.T) {
			clearInputEnv(t)
			t.Setenv(c.env, c.value)
			if _, err := loadInputConfig(); err == nil || !strings.Contains(err.Error(), c.wantInErr) {
				t.Fatalf("loadInputConfig error %v, want one naming %q", err, c.wantInErr)
			}
		})
	}
	t.Run("kepler with scope host", func(t *testing.T) {
		clearInputEnv(t)
		t.Setenv("NODE_POWER_PRESET", "kepler")
		t.Setenv("NODE_POWER_SCOPE", "host")
		if _, err := loadInputConfig(); err == nil || !strings.Contains(err.Error(), "host") {
			t.Fatalf("loadInputConfig error %v, want kepler with host refused", err)
		}
	})
}

// A preset other than custom would ignore the query; that combination
// stops the controller manager rather than read something else silently.
func TestLoadInputConfigRejectsPresetWithQuery(t *testing.T) {
	for _, preset := range []string{"otel", "kepler", "kepler-legacy"} {
		for _, queryEnv := range []string{"NODE_POWER_PROMETHEUS_QUERY", "OPERATOR_NODE_POWER_PROMETHEUS_QUERY"} {
			t.Run(preset+"/"+queryEnv, func(t *testing.T) {
				clearInputEnv(t)
				t.Setenv("NODE_POWER_PRESET", preset)
				t.Setenv(queryEnv, `sum by (instance) (rate(node_power_joules_total[5m]))`)
				if _, err := loadInputConfig(); err == nil || !strings.Contains(err.Error(), "custom") {
					t.Fatalf("loadInputConfig error %v, want the preset and query combination refused", err)
				}
			})
		}
	}
}

// An adopter's existing NODE_POWER_PROMETHEUS_QUERY keeps working: with no
// preset set it selects the custom preset, and {node} still runs it once
// per node.
func TestLoadInputConfigQueryImpliesCustom(t *testing.T) {
	clearInputEnv(t)
	t.Setenv("NODE_POWER_SOURCE", "prometheus")
	t.Setenv("NODE_POWER_PROMETHEUS_ADDRESS", "http://prom:9090")
	t.Setenv("NODE_POWER_PROMETHEUS_QUERY", `rate(kepler_node_platform_joules_total{exported_instance="{node}"}[5m])`)
	cfg, err := loadInputConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.nodePower.spec.Preset != inputs.NodePowerPresetCustom {
		t.Fatalf("preset %q, want custom", cfg.nodePower.spec.Preset)
	}
	primary, fallback, err := cfg.nodePower.query()
	if err != nil || !primary.PerNode || fallback.PromQL != "" {
		t.Fatalf("query %+v fallback %+v err %v, want the per-node query alone", primary, fallback, err)
	}

	t.Setenv("NODE_POWER_PRESET", "custom")
	if cfg, err = loadInputConfig(); err != nil || cfg.nodePower.spec.Preset != inputs.NodePowerPresetCustom {
		t.Fatalf("explicit custom preset: %+v, %v", cfg.nodePower.spec, err)
	}

	prevSpec, prevSource, prevAddr := nodePowerSpec, nodePowerSource, nodePowerPromAddress
	t.Cleanup(func() {
		nodePowerSpec, nodePowerSource, nodePowerPromAddress = prevSpec, prevSource, prevAddr
	})
	cfg.install()
	if nodePowerSource != nodePowerSourcePrometheus || nodePowerPromAddress != "http://prom:9090" || nodePowerSpec.CustomQuery == "" || nodePowerSpec.Preset != inputs.NodePowerPresetCustom {
		t.Fatalf("install set source=%q address=%q spec=%+v", nodePowerSource, nodePowerPromAddress, nodePowerSpec)
	}
}

// The input metrics are registered with the policy metrics, under the
// default Prometheus translation of their OpenTelemetry names.
func TestInputMetricsRegistered(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	registerMetrics(reg)
	recordInputQuery(inputNodePower, inputResultSuccess)
	setFacilityGauge(twinFacilityAmbientTemp, "otel", -3, true)
	setFacilityGauge(twinFacilityITPower, "otel", 15000, true)
	setFacilityGauge(twinFacilityCoolingPower, "otel", 4000, true)
	t.Cleanup(func() {
		for _, g := range []*prometheus.GaugeVec{twinFacilityAmbientTemp, twinFacilityITPower, twinFacilityCoolingPower} {
			g.DeleteLabelValues("otel")
		}
	})

	cases := []struct {
		otel   semconv.Metric
		labels map[string]string
		want   float64
		value  bool // false for the counter, which other tests also count
	}{
		{semconv.Metric{Name: "joulie.twin.input.queries", Unit: "{query}", Kind: semconv.Counter},
			map[string]string{"input": inputNodePower, "result": inputResultSuccess}, 0, false},
		{semconv.Metric{Name: "joulie.twin.facility.ambient_temperature", Unit: "Cel", Kind: semconv.Gauge},
			map[string]string{"preset": "otel"}, -3, true},
		{semconv.Metric{Name: "joulie.twin.facility.it_power", Unit: "W", Kind: semconv.Gauge},
			map[string]string{"preset": "otel"}, 15000, true},
		{semconv.Metric{Name: "joulie.twin.facility.cooling_power", Unit: "W", Kind: semconv.Gauge},
			map[string]string{"preset": "otel"}, 4000, true},
	}
	for _, c := range cases {
		name := promname.Default.MetricName(c.otel)
		v, ok := gatherValue(t, reg, name, c.labels)
		if !ok {
			t.Fatalf("%s %v not registered", name, c.labels)
		}
		if c.value && v != c.want {
			t.Errorf("%s=%v, want %v", name, v, c.want)
		}
	}

	// A reading that becomes unknown leaves no stale series behind.
	setFacilityGauge(twinFacilityAmbientTemp, "otel", 0, false)
	if _, ok := gatherValue(t, reg, "joulie_twin_facility_ambient_temperature_celsius", map[string]string{"preset": "otel"}); ok {
		t.Fatal("an unknown ambient still exports its last value")
	}
}
