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

// Every alias of both groups is read when its replacement is unset, and its
// one warning names the replacement and the group's own removal release.
func TestEnvOrDeprecatedFallsBackToLegacyNameWithWarning(t *testing.T) {
	groups := []struct {
		aliases   map[string]string
		removedIn string
	}{
		{legacyEnvAliases, legacyEnvAliasesRemovedIn},
		{inputEnvAliases, inputAliasesRemovedIn},
	}
	for _, g := range groups {
		for newKey, oldKey := range g.aliases {
			newKey, oldKey, removedIn := newKey, oldKey, g.removedIn
			t.Run(newKey, func(t *testing.T) {
				resetDeprecatedEnvWarnings()
				buf := captureLog(t)
				t.Setenv(newKey, "")
				t.Setenv(oldKey, "legacy-value")

				if got := envOrDeprecated(newKey, "default"); got != "legacy-value" {
					t.Fatalf("envOrDeprecated(%s) = %q, want legacy value", newKey, got)
				}
				out := buf.String()
				if !strings.Contains(out, "warning") || !strings.Contains(out, oldKey) || !strings.Contains(out, newKey) || !strings.Contains(out, removedIn) {
					t.Fatalf("expected one warning naming %s, %s and %q, got %q", oldKey, newKey, removedIn, out)
				}
				buf.Reset()
				envOrDeprecated(newKey, "default")
				if buf.Len() != 0 {
					t.Fatalf("expected the warning once per variable, got second log %q", buf.String())
				}
			})
		}
	}
}

// A name in both groups would make the warning name the wrong release.
func TestDeprecatedEnvAliasGroupsAreDisjoint(t *testing.T) {
	for k, v := range inputEnvAliases {
		if _, ok := legacyEnvAliases[k]; ok {
			t.Errorf("%s is aliased in both groups", k)
		}
		for lk, lv := range legacyEnvAliases {
			if v == lv || v == lk {
				t.Errorf("old name %s of %s is also in legacyEnvAliases", v, k)
			}
		}
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
