package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
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

func TestEnvOrDeprecatedFallsBackToLegacyNameWithWarning(t *testing.T) {
	for newKey, oldKey := range legacyEnvAliases {
		t.Run(newKey, func(t *testing.T) {
			resetDeprecatedEnvWarnings()
			buf := captureLog(t)
			t.Setenv(newKey, "")
			t.Setenv(oldKey, "legacy-value")

			if got := envOrDeprecated(newKey, "default"); got != "legacy-value" {
				t.Fatalf("envOrDeprecated(%s) = %q, want legacy value", newKey, got)
			}
			out := buf.String()
			if !strings.Contains(out, "warning") || !strings.Contains(out, oldKey) || !strings.Contains(out, newKey) {
				t.Fatalf("expected one warning naming %s and %s, got %q", oldKey, newKey, out)
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
	t.Setenv("NODE_POWER_SOURCE", "http")
	t.Setenv("OPERATOR_NODE_POWER_SOURCE", "prometheus")
	if got := envOrDeprecated("NODE_POWER_SOURCE", "static"); got != "http" {
		t.Fatalf("new name must win, got %q", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("no warning expected when the new name is set, got %q", buf.String())
	}

	t.Setenv("NODE_POWER_SOURCE", "")
	t.Setenv("OPERATOR_NODE_POWER_SOURCE", "")
	if got := envOrDeprecated("NODE_POWER_SOURCE", "static"); got != "static" {
		t.Fatalf("default expected when neither is set, got %q", got)
	}
	if got := envOrDeprecated("METRICS_ADDR", ":1"); got != ":1" {
		t.Fatalf("unaliased key must behave like envOrDefault, got %q", got)
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

func TestPolicyMetricsRegisteredUnderBothNames(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	registerMetrics(reg)

	recordNodeStateMetrics("dual-n1", "ActiveEco")
	recordNodeProfileLabelMetrics("dual-n1", profileEco)
	policyNodeDensity.Set(42.5, "dual-n1", "cpu")
	recordTransitionMetrics(NodeAssignment{
		NodeName: "dual-n1", SourceProfile: profilePerformance, Profile: profileEco, State: "ActiveEco",
	})

	cases := []struct {
		name, oldName string
		labels        map[string]string
		want          float64
	}{
		{"joulie_policy_node_state", "joulie_operator_node_state", map[string]string{"node": "dual-n1", "state": "ActiveEco"}, 1},
		{"joulie_policy_node_state", "joulie_operator_node_state", map[string]string{"node": "dual-n1", "state": "ActivePerformance"}, 0},
		{"joulie_policy_node_profile_label", "joulie_operator_node_profile_label", map[string]string{"node": "dual-n1", "profile": "eco"}, 1},
		{"joulie_policy_node_compute_density", "joulie_operator_node_compute_density", map[string]string{"node": "dual-n1", "component": "cpu"}, 42.5},
		{"joulie_policy_state_transitions_total", "joulie_operator_state_transitions_total", map[string]string{"node": "dual-n1", "from_state": "ActivePerformance", "to_state": "ActiveEco", "result": "applied"}, 1},
	}
	for _, c := range cases {
		cur, ok := gatherValue(t, reg, c.name, c.labels)
		if !ok {
			t.Fatalf("%s %v not registered", c.name, c.labels)
		}
		old, ok := gatherValue(t, reg, c.oldName, c.labels)
		if !ok {
			t.Fatalf("deprecated alias %s %v not registered", c.oldName, c.labels)
		}
		if cur != c.want || old != c.want {
			t.Fatalf("%s=%v %s=%v, want both %v", c.name, cur, c.oldName, old, c.want)
		}
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if strings.HasPrefix(mf.GetName(), "joulie_operator_") && !strings.HasPrefix(mf.GetHelp(), "Deprecated, use joulie_policy_") {
			t.Fatalf("%s help must point at the replacement, got %q", mf.GetName(), mf.GetHelp())
		}
	}
}
