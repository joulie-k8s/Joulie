package promql_test

// The alerting rules the chart ships (charts/joulie/alerts/joulie-alerts.yaml)
// are checked the way Prometheus loads them, with rulefmt of the pinned
// release, and run in its engine against series shaped like the ones the
// agent and the controller manager export: each alert fires on the failure
// it names, after its for: duration, and stays quiet on a transient one.

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/rulefmt"
	"github.com/prometheus/prometheus/promql"
	promparser "github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
)

// alertRulesFile is the rules file of the chart, relative to the
// repository root.
const alertRulesFile = "charts/joulie/alerts/joulie-alerts.yaml"

// alertRules parses the chart's rules file as Prometheus loads a rule file,
// failing the test on any error rulefmt reports.
func alertRules(t *testing.T) map[string]rulefmt.Rule {
	t.Helper()
	groups, errs := rulefmt.ParseFile(filepath.Join(repoRoot, alertRulesFile), false,
		model.UTF8Validation, promparser.NewParser(promparser.Options{}), slog.New(slog.DiscardHandler))
	for _, err := range errs {
		t.Errorf("%v", err)
	}
	if groups == nil {
		t.FailNow()
	}
	out := map[string]rulefmt.Rule{}
	for _, g := range groups.Groups {
		for _, r := range g.Rules {
			out[r.Alert] = r
		}
	}
	return out
}

// TestAL01AlertRulesLoadAndSelectExportedMetrics: the rules file loads as a
// Prometheus rule file, every rule is an alert with a for: duration, a
// severity and a runbook, and every metric it selects is one Joulie
// exports, so a renamed metric cannot leave an alert that never fires.
func TestAL01AlertRulesLoadAndSelectExportedMetrics(t *testing.T) {
	exported := exportedMetricNames(t)
	rules := alertRules(t)
	want := []string{"JoulieAgentWritesBlocked", "JoulieCapNotApplied", "JoulieNodePowerNoData", "JoulieTwinStale"}
	var got []string
	for name := range rules {
		got = append(got, name)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("alerts %v, want %v", got, want)
	}
	for name, r := range rules {
		if r.For <= 0 {
			t.Errorf("%s: no for: duration; a single bad evaluation would page", name)
		}
		if r.Labels["severity"] == "" {
			t.Errorf("%s: no severity label", name)
		}
		if !strings.HasPrefix(r.Annotations["runbook_url"], "https://joulie-k8s.github.io/Joulie/docs/") {
			t.Errorf("%s: runbook_url %q is not a page of the docs site", name, r.Annotations["runbook_url"])
		}
		names, err := selectedMetrics(r.Expr)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		for _, n := range names {
			if !exported[n] {
				t.Errorf("%s: %s is not a metric Joulie exports", name, n)
			}
		}
	}
}

// evalVectorAt runs q at ts and returns its samples.
func evalVectorAt(t *testing.T, st storage.Storage, q string, ts time.Time) promql.Vector {
	t.Helper()
	engine := promqltest.NewTestEngine(t, false, 0, promqltest.DefaultMaxSamplesPerQuery)
	qry, err := engine.NewInstantQuery(context.Background(), st, nil, q, ts)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer qry.Close()
	res := qry.Exec(context.Background())
	if res.Err != nil {
		t.Fatalf("%s: %v", q, res.Err)
	}
	v, ok := res.Value.(promql.Vector)
	if !ok {
		t.Fatalf("%s: result type %s, want vector", q, res.Value.Type())
	}
	return v
}

// firstFiring evaluates rule once a minute from 0 to end minutes, as a rule
// group with a 1m interval does, and returns the minute each label set of
// its result first fires: the expression has held at every evaluation for
// the rule's for: duration.
func firstFiring(t *testing.T, st storage.Storage, rule rulefmt.Rule, end int) map[string]int {
	t.Helper()
	forMin := int(time.Duration(rule.For) / time.Minute)
	activeAt := map[string]int{}
	fired := map[string]int{}
	for m := 0; m <= end; m++ {
		now := map[string]bool{}
		for _, s := range evalVectorAt(t, st, rule.Expr, time.Unix(int64(m*60), 0)) {
			key := s.Metric.String()
			now[key] = true
			if _, ok := activeAt[key]; !ok {
				activeAt[key] = m
			}
			if _, ok := fired[key]; !ok && m-activeAt[key] >= forMin {
				fired[key] = m
			}
		}
		for key := range activeAt {
			if !now[key] {
				delete(activeAt, key)
			}
		}
	}
	return fired
}

// steps writes 61 samples, one a minute for an hour: value(m) at minute m.
func steps(value func(m int) float64) string {
	var parts []string
	for m := 0; m <= 60; m++ {
		parts = append(parts, fmt.Sprintf("%g", value(m)))
	}
	return strings.Join(parts, " ")
}

// TestAL02AlertsFireOnAPersistentFailureOnly: each alert fires, at the
// minute its for: duration sets, on a failure that persists, and never on a
// transient one or a healthy series. The label sets name what the runbooks
// read: node and component, node and object, node, source.
func TestAL02AlertsFireOnAPersistentFailureOnly(t *testing.T) {
	rules := alertRules(t)
	agent := func(node string) map[string]string {
		return map[string]string{"node": node, "job": "joulie-agent", "instance": node}
	}
	with := func(base map[string]string, kv ...string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	cm := map[string]string{"job": "joulie-controller-manager", "instance": "cm"}
	// capResult is joulie_agent_cap_result of node's component, with result
	// in effect from minute from on and applied before.
	capResult := func(node, component, result string, from int) []series {
		var out []series
		for _, r := range []string{"applied", "blocked", "error", "none"} {
			out = append(out, series{"joulie_agent_cap_result", with(agent(node), "component", component, "result", r), steps(func(m int) float64 {
				if (m >= from && r == result) || (m < from && r == "applied") {
					return 1
				}
				return 0
			})})
		}
		return out
	}
	// writeErrors is joulie_agent_status_write_errors_total of node's object,
	// one failed write at each minute of at.
	writeErrors := func(node, object string, at func(m int) bool) series {
		return series{"joulie_agent_status_write_errors_total", with(agent(node), "object", object), steps(func(m int) float64 {
			n := 0
			for i := 0; i <= m; i++ {
				if at(i) {
					n++
				}
			}
			return float64(n)
		})}
	}
	// twinWritten is joulie_twin_status_updated_timestamp_seconds of node,
	// written every minute for which written is true.
	twinWritten := func(node string, written func(m int) bool) series {
		last := 0
		return series{"joulie_twin_status_updated_timestamp_seconds", with(cm, "node", node), steps(func(m int) float64 {
			if written(m) {
				last = m
			}
			return float64(last * 60)
		})}
	}
	powerSource := func(source string, nodes func(m int) float64) series {
		return series{"joulie_twin_nodes_power_source", with(cm, "source", source), steps(nodes)}
	}

	cases := []struct {
		alert string
		ss    []series
		want  map[string]int
	}{
		{"JoulieCapNotApplied", slices.Concat(
			capResult("refused", "gpu", "error", 0),
			capResult("no-rapl", "cpu", "blocked", 20),
			capResult("healthy", "cpu", "applied", 0),
			capResult("uncapped", "gpu", "none", 0),
		), map[string]int{
			`{component="gpu", node="refused", result="error"}`:   15,
			`{component="cpu", node="no-rapl", result="blocked"}`: 35,
		}},
		{"JoulieCapNotApplied", func() []series {
			// An error that clears within 15 minutes, twice.
			var out []series
			for _, r := range []string{"applied", "error"} {
				out = append(out, series{"joulie_agent_cap_result", with(agent("flaky"), "component", "gpu", "result", r), steps(func(m int) float64 {
					failing := (m >= 10 && m < 24) || (m >= 30 && m < 44)
					if failing == (r == "error") {
						return 1
					}
					return 0
				})})
			}
			return out
		}(), map[string]int{}},
		{"JoulieAgentWritesBlocked", []series{
			// Refused on every write: the status is rewritten every 5 minutes.
			writeErrors("forbidden", "NodeTwin", func(m int) bool { return m%5 == 0 }),
			// One conflict at minute 30.
			writeErrors("conflict", "NodeHardware", func(m int) bool { return m == 0 || m == 30 }),
			writeErrors("healthy", "NodeHardware", func(m int) bool { return m == 0 }),
		}, map[string]int{
			`{node="forbidden", object="NodeTwin"}`: 25,
		}},
		{"JoulieTwinStale", []series{
			twinWritten("fresh", func(int) bool { return true }),
			// Not written since minute 20.
			twinWritten("stuck", func(m int) bool { return m <= 20 }),
			// A 7 minute gap, a slow reconcile or a restart.
			twinWritten("hiccup", func(m int) bool { return m <= 30 || m >= 37 }),
		}, map[string]int{
			`{node="stuck"}`: 31,
		}},
		{"JoulieNodePowerNoData", []series{
			powerSource("prometheus", func(int) float64 { return 3 }),
			powerSource("prometheus-no-data", func(m int) float64 {
				if m >= 10 {
					return 2
				}
				return 0
			}),
			// A node that joined at minute 50, before its first rate window.
			powerSource("http-error", func(m int) float64 {
				if m >= 50 {
					return 1
				}
				return 0
			}),
			powerSource("static", func(int) float64 { return 1 }),
		}, map[string]int{
			`{source="prometheus-no-data"}`: 25,
		}},
	}
	for _, c := range cases {
		rule, ok := rules[c.alert]
		if !ok {
			t.Errorf("no alert %s", c.alert)
			continue
		}
		got := firstFiring(t, load(t, c.ss), rule, 60)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s fired %v, want %v", c.alert, got, c.want)
		}
	}
}
