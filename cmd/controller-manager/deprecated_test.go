package main

import (
	"bytes"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func resetDeprecatedEnvWarnings() {
	deprecatedEnvWarnMu.Lock()
	deprecatedEnvWarnSeen = map[string]struct{}{}
	deprecatedEnvWarnMu.Unlock()
}

// Every alias is read when its replacement is unset, and its one warning
// names the replacement and the removal release.
func TestEnvOrDeprecatedFallsBackToLegacyNameWithWarning(t *testing.T) {
	for newKey, oldKey := range inputEnvAliases {
		newKey, oldKey := newKey, oldKey
		t.Run(newKey, func(t *testing.T) {
			resetDeprecatedEnvWarnings()
			buf := captureLog(t)
			t.Setenv(newKey, "")
			t.Setenv(oldKey, "legacy-value")

			if got := envOrDeprecated(newKey, "default"); got != "legacy-value" {
				t.Fatalf("envOrDeprecated(%s) = %q, want legacy value", newKey, got)
			}
			out := buf.String()
			if !strings.Contains(out, "warning") || !strings.Contains(out, oldKey) || !strings.Contains(out, newKey) || !strings.Contains(out, inputAliasesRemovedIn) {
				t.Fatalf("expected one warning naming %s, %s and %q, got %q", oldKey, newKey, inputAliasesRemovedIn, out)
			}
			buf.Reset()
			envOrDeprecated(newKey, "default")
			if buf.Len() != 0 {
				t.Fatalf("expected the warning once per variable, got second log %q", buf.String())
			}
		})
	}
}

func TestEnvOrDeprecatedNewNameWinsAndDefaultApplies(t *testing.T) {
	resetDeprecatedEnvWarnings()
	buf := captureLog(t)
	t.Setenv("FACILITY_AMBIENT_TEMP_QUERY", "avg(temp)")
	t.Setenv("FACILITY_AMBIENT_TEMP_METRIC", "temp")
	if got := envOrDeprecated("FACILITY_AMBIENT_TEMP_QUERY", "def"); got != "avg(temp)" {
		t.Fatalf("new name must win, got %q", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("no warning expected when the new name is set, got %q", buf.String())
	}

	t.Setenv("FACILITY_AMBIENT_TEMP_QUERY", "")
	t.Setenv("FACILITY_AMBIENT_TEMP_METRIC", "")
	if got := envOrDeprecated("FACILITY_AMBIENT_TEMP_QUERY", "def"); got != "def" {
		t.Fatalf("default expected when neither is set, got %q", got)
	}
	if got := envOrDeprecated("METRICS_ADDR", ":1"); got != ":1" {
		t.Fatalf("unaliased key must behave like envOrDefault, got %q", got)
	}
}

// The OPERATOR_NODE_POWER_* names of the controller manager's old binary
// were read for one release after the rename (v0.2.0) and are gone in
// v0.3.0: setting them changes nothing.
func TestOperatorEnvNamesAreNoLongerRead(t *testing.T) {
	clearInputEnv(t)
	want, err := loadInputConfig()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPERATOR_NODE_POWER_SOURCE", "http")
	t.Setenv("OPERATOR_NODE_POWER_HTTP_ENDPOINT", "http://alias.invalid/power")
	t.Setenv("OPERATOR_NODE_POWER_PROMETHEUS_ADDRESS", "http://alias.invalid:9090")
	t.Setenv("OPERATOR_NODE_POWER_PROMETHEUS_QUERY", "alias_query")
	got, err := loadInputConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.nodePower, want.nodePower) {
		t.Errorf("an OPERATOR_* name changed the node power input:\n got  %+v\n want %+v", got.nodePower, want.nodePower)
	}
}

// gatherValue returns the sample of metric name whose labels match want.
func gatherValue(t *testing.T, reg prometheus.Gatherer, name string, want map[string]string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			match := true
			for k, v := range want {
				if labels[k] != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			switch mf.GetType() {
			case dto.MetricType_GAUGE:
				return m.GetGauge().GetValue(), true
			case dto.MetricType_COUNTER:
				return m.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

// The policy metrics are exported under their joulie_policy_* names only:
// the joulie_operator_* aliases of the old binary were exported for one
// release after the rename (v0.2.0) and are gone in v0.3.0.
func TestPolicyMetricsHaveNoOperatorAlias(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	registerMetrics(reg)

	recordNodeStateMetrics("policy-n1", "ActiveEco")
	recordNodeProfileLabelMetrics("policy-n1", profileEco)
	recordTransitionMetrics(NodeAssignment{
		NodeName: "policy-n1", SourceProfile: profilePerformance, Profile: profileEco, State: "ActiveEco",
	})

	cases := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"joulie_policy_node_state", map[string]string{"node": "policy-n1", "state": "ActiveEco"}, 1},
		{"joulie_policy_node_state", map[string]string{"node": "policy-n1", "state": "ActivePerformance"}, 0},
		{"joulie_policy_node_profile_label", map[string]string{"node": "policy-n1", "profile": "eco"}, 1},
		{"joulie_policy_state_transitions_total", map[string]string{"node": "policy-n1", "from_state": "ActivePerformance", "to_state": "ActiveEco", "result": "applied"}, 1},
	}
	for _, c := range cases {
		got, ok := gatherValue(t, reg, c.name, c.labels)
		if !ok {
			t.Fatalf("%s %v not registered", c.name, c.labels)
		}
		if got != c.want {
			t.Fatalf("%s %v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if strings.HasPrefix(mf.GetName(), "joulie_operator_") {
			t.Errorf("%s is still exported", mf.GetName())
		}
	}
}

// The input aliases go in the release that removes the legacy formats every
// component reads, so the controller manager's warnings and the agent's
// name one release.
func TestInputAliasesGoWithTheLegacyFormats(t *testing.T) {
	if inputAliasesRemovedIn != otlpjson.LegacyRemovedIn {
		t.Errorf("inputAliasesRemovedIn = %q, otlpjson.LegacyRemovedIn = %q", inputAliasesRemovedIn, otlpjson.LegacyRemovedIn)
	}
}
