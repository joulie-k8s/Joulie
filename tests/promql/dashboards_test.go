package promql_test

// The Grafana dashboards the repository ships select metrics by name, and a
// metric that is renamed or removed empties its panels without any error:
// Grafana shows "No data". These tests parse every query of every dashboard
// in the PromQL parser of the Prometheus release the project pins, and check
// each metric a query selects against the names Joulie exports: the names
// its Go code registers with client_golang, and the Prometheus spelling
// (pkg/otel/promname) of every OpenTelemetry metric of pkg/otel/semconv under
// every translation strategy.

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"
)

// repoRoot is the repository root, relative to this package.
const repoRoot = "../.."

// dashboardGlobs are the dashboards the repository ships or shows, relative
// to the repository root. A dashboard added anywhere else is not checked.
var dashboardGlobs = []string{"charts/*/dashboards/*.json", "examples/*/*.json"}

// metricSourceDirs hold the Go code that registers Joulie's own metrics,
// relative to the repository root.
var metricSourceDirs = []string{"cmd", "pkg", "simulator"}

// seriesSuffixes are the suffixes of the series a client_golang metric of
// each option type is exported as.
var seriesSuffixes = map[string][]string{
	"GaugeOpts":     {""},
	"CounterOpts":   {""},
	"HistogramOpts": {"", "_bucket", "_sum", "_count"},
	"SummaryOpts":   {"", "_sum", "_count"},
}

// exportedMetricNames returns every metric name a dashboard may select, read
// from the registrations in the non-test Go files under metricSourceDirs:
// the Name of a prometheus.GaugeOpts, CounterOpts, HistogramOpts or
// SummaryOpts literal with the series of its type, the name of a
// prometheus.NewDesc. A string
// that is only a label or a constant is not a metric. The Prometheus names of
// the semconv metrics are added.
func exportedMetricNames(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	fset := token.NewFileSet()
	for _, dir := range metricSourceDirs {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CompositeLit:
					suffixes, ok := seriesSuffixes[prometheusName(n.Type)]
					if !ok {
						return true
					}
					for _, el := range n.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok || fmt.Sprint(kv.Key) != "Name" {
							continue
						}
						if s, ok := stringLiteral(kv.Value); ok {
							for _, suffix := range suffixes {
								names[s+suffix] = true
							}
						}
					}
				case *ast.CallExpr:
					if prometheusName(n.Fun) == "NewDesc" && len(n.Args) > 0 {
						if s, ok := stringLiteral(n.Args[0]); ok {
							names[s] = true
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range semconv.Metrics() {
		for _, s := range promname.Strategies() {
			names[s.MetricName(m)] = true
		}
	}
	return names
}

// prometheusName is the name of e when e is prometheus.<name>, the
// client_golang package under its usual import name; "" otherwise.
func prometheusName(e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "prometheus" {
		return ""
	}
	return sel.Sel.Name
}

// stringLiteral is the value of e when e is a string literal.
func stringLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// dashboardQuery is one PromQL query of a dashboard and where it is.
type dashboardQuery struct {
	where string
	expr  string
}

// grafanaPanel is the part of a Grafana panel the tests read. A row keeps
// its panels in panels while it is collapsed.
type grafanaPanel struct {
	Title   string `json:"title"`
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
	Panels []grafanaPanel `json:"panels"`
}

type grafanaDashboard struct {
	Panels     []grafanaPanel `json:"panels"`
	Templating struct {
		List []struct {
			Name  string          `json:"name"`
			Type  string          `json:"type"`
			Query json.RawMessage `json:"query"`
		} `json:"list"`
	} `json:"templating"`
}

// labelValues is the variable query label_values(selector, label).
var labelValues = regexp.MustCompile(`^\s*label_values\((.+),\s*[a-zA-Z_][a-zA-Z0-9_]*\s*\)\s*$`)

// queryResult is the variable query query_result(expr).
var queryResult = regexp.MustCompile(`^\s*query_result\((.+)\)\s*$`)

// dashboardQueries returns the PromQL of every panel target, rows included,
// and of every query variable that selects a metric.
func dashboardQueries(path string) ([]dashboardQuery, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d grafanaDashboard
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	var out []dashboardQuery
	var walk func([]grafanaPanel)
	walk = func(panels []grafanaPanel) {
		for _, p := range panels {
			for i, target := range p.Targets {
				if strings.TrimSpace(target.Expr) != "" {
					out = append(out, dashboardQuery{fmt.Sprintf("panel %q target %d", p.Title, i), target.Expr})
				}
			}
			walk(p.Panels)
		}
	}
	walk(d.Panels)
	for _, v := range d.Templating.List {
		if v.Type != "query" {
			continue
		}
		// Grafana stores the query as a string, or as an object with a
		// query field.
		var q string
		if err := json.Unmarshal(v.Query, &q); err != nil {
			var obj struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(v.Query, &obj); err != nil {
				return nil, fmt.Errorf("variable %s: %v", v.Name, err)
			}
			q = obj.Query
		}
		if m := labelValues.FindStringSubmatch(q); m != nil {
			out = append(out, dashboardQuery{fmt.Sprintf("variable %s", v.Name), m[1]})
		} else if m := queryResult.FindStringSubmatch(q); m != nil {
			out = append(out, dashboardQuery{fmt.Sprintf("variable %s", v.Name), m[1]})
		}
	}
	return out, nil
}

// grafanaVariable is a Grafana variable reference: $name, ${name},
// ${name:format} or [[name]].
var grafanaVariable = regexp.MustCompile(`\$\{[^}]+\}|\$[a-zA-Z_][a-zA-Z0-9_]*|\[\[[^\]]+\]\]`)

// grafanaToPromQL replaces each Grafana variable with a value that keeps
// the query valid PromQL: a duration inside a range or subquery bracket, a
// word inside a quoted string, a number anywhere else.
func grafanaToPromQL(expr string) string {
	var b strings.Builder
	var quote rune
	brackets := 0
	for i := 0; i < len(expr); {
		if loc := grafanaVariable.FindStringIndex(expr[i:]); loc != nil && loc[0] == 0 {
			switch {
			case quote != 0:
				b.WriteString("x")
			case brackets > 0:
				b.WriteString("5m")
			default:
				b.WriteString("1")
			}
			i += loc[1]
			continue
		}
		c := rune(expr[i])
		switch {
		case quote != 0 && c == '\\' && i+1 < len(expr):
			b.WriteByte(expr[i])
			i++
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\'' || c == '`'):
			quote = c
		case quote == 0 && c == '[':
			brackets++
		case quote == 0 && c == ']':
			brackets--
		}
		b.WriteByte(expr[i])
		i++
	}
	return b.String()
}

// selectedMetrics returns the metric names the selectors of expr name, in
// the order the parser visits them, without repeats.
func selectedMetrics(expr string) ([]string, error) {
	parsed, err := promparser.NewParser(promparser.Options{}).ParseExpr(expr)
	if err != nil {
		return nil, err
	}
	var out []string
	promparser.Inspect(parsed, func(n promparser.Node, _ []promparser.Node) error {
		vs, ok := n.(*promparser.VectorSelector)
		if !ok {
			return nil
		}
		name := vs.Name
		for _, m := range vs.LabelMatchers {
			if m.Name == labels.MetricName && m.Type == labels.MatchEqual {
				name = m.Value
			}
		}
		if name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
		return nil
	})
	return out, nil
}

// dashboardProblems returns one line for every query of the dashboard at
// path that does not parse, selects no metric by name, or selects a metric
// not in exported.
func dashboardProblems(path string, exported map[string]bool) ([]string, error) {
	queries, err := dashboardQueries(path)
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, q := range queries {
		names, err := selectedMetrics(grafanaToPromQL(q.expr))
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", q.where, err))
		case len(names) == 0:
			problems = append(problems, fmt.Sprintf("%s: %s selects no metric by name", q.where, q.expr))
		}
		for _, name := range names {
			if !exported[name] {
				problems = append(problems, fmt.Sprintf("%s: %s is not a metric Joulie exports", q.where, name))
			}
		}
	}
	return problems, nil
}

// dashboards returns the dashboards dashboardGlobs match, relative to the
// repository root.
func dashboards(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, g := range dashboardGlobs {
		matches, err := filepath.Glob(filepath.Join(repoRoot, g))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			rel, err := filepath.Rel(repoRoot, m)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// TestDB01DashboardQueriesSelectExportedMetrics: every query of every
// dashboard parses and selects only metrics Joulie exports. A failure names
// the dashboard, the panel and the metric: rename the metric in the
// dashboard, or, for a metric another exporter serves, drop the panel.
func TestDB01DashboardQueriesSelectExportedMetrics(t *testing.T) {
	exported := exportedMetricNames(t)
	paths := dashboards(t)
	if !slices.Contains(paths, "charts/joulie/dashboards/joulie-overview.json") {
		t.Fatalf("the dashboards %v miss the one the chart ships; were they moved?", paths)
	}
	for _, path := range paths {
		problems, err := dashboardProblems(filepath.Join(repoRoot, path), exported)
		if err != nil {
			t.Errorf("%s: %v", path, err)
		}
		for _, p := range problems {
			t.Errorf("%s: %s", path, p)
		}
	}
}

// TestDB02DashboardCheckCatchesARemovedMetric: the check reports a metric
// Joulie no longer exports, joulie_policy_node_compute_density (removed in
// #67), in a panel inside a collapsed row and in a variable, and a name the
// code spells only as a label (joulie_component, in pkg/controller/inputs);
// it reports the joulie_operator_* alias removed in v0.3.0, and accepts what
// the dashboards use: Grafana variables, a histogram's _bucket series and an
// OpenTelemetry name.
func TestDB02DashboardCheckCatchesARemovedMetric(t *testing.T) {
	problems, err := dashboardProblems("testdata/dashboard-removed-metric.json", exportedMetricNames(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`panel "Removed alias" target 0: joulie_operator_node_state is not a metric Joulie exports`,
		`panel "A label, not a metric" target 0: joulie_component is not a metric Joulie exports`,
		`panel "Compute density" target 0: joulie_policy_node_compute_density is not a metric Joulie exports`,
		`variable dense: joulie_policy_node_compute_density is not a metric Joulie exports`,
	}
	if !slices.Equal(problems, want) {
		t.Errorf("problems:\n  %s\nwant:\n  %s", strings.Join(problems, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestDB03GrafanaVariablesBecomeValidPromQL: each kind of variable is
// replaced by a value valid where it stands.
func TestDB03GrafanaVariablesBecomeValidPromQL(t *testing.T) {
	cases := map[string]string{
		`rate(joulie_dvfs_actions_total{node=~"$node"}[$__rate_interval])`:                     `rate(joulie_dvfs_actions_total{node=~"x"}[5m])`,
		`max_over_time(joulie_policy_cap_watts{node="${node:regex}"}[${__range}:$__interval])`: `max_over_time(joulie_policy_cap_watts{node="x"}[5m:5m])`,
		`topk($top, joulie_sim_node_power_watts{node=~"[[node]]"})`:                            `topk(1, joulie_sim_node_power_watts{node=~"x"})`,
		`joulie_policy_node_state{state="a[$b]"}`:                                              `joulie_policy_node_state{state="a[x]"}`,
	}
	for in, want := range cases {
		if got := grafanaToPromQL(in); got != want {
			t.Errorf("grafanaToPromQL(%s) = %s, want %s", in, got, want)
		}
		if _, err := selectedMetrics(want); err != nil {
			t.Errorf("%s: %v", want, err)
		}
	}
}

// TestDB04SimulatedClusterMatrixLabelsEveryNode: the node matrix of the
// simulator chart's dashboard has one cell per node the policy controller
// plans or the simulator measures. Its value is the node's inlet
// temperature, NaN for a node that has none (a hypervisor guest exports no
// hw.* metric), and its label the policy controller's state of the node:
// performance, draining (planned in eco, still on performance caps) or eco,
// or unmanaged for a node the controller does not plan. A state a node left
// (0) never shows. When two controller manager pods disagree on a node, as
// during a rolling update, the matrix and the profile timeline both show
// the state further toward eco, instead of failing on the duplicate or
// adding the codes. The inlet history and the node variable list every node
// of the matrix.
func TestDB04SimulatedClusterMatrixLabelsEveryNode(t *testing.T) {
	const path = "charts/joulie-simulator/dashboards/joulie-simulated-cluster.json"
	queries, err := dashboardQueries(filepath.Join(repoRoot, path))
	if err != nil {
		t.Fatal(err)
	}
	inlet := func(node, values string) series {
		return series{"hw_host_ambient_temperature_celsius", map[string]string{
			"k8s_node_name": node, "instance": node, "job": "joulie/joulie-simulator", "hw_id": "host"}, values}
	}
	// controller is what one controller manager pod exports for a node in
	// state, as recordNodeStateMetrics and recordNodeProfileLabelMetrics
	// write it: the profile label of a draining node is eco.
	controller := func(pod, node, state string) []series {
		var out []series
		for _, s := range []string{"ActivePerformance", "DrainingPerformance", "ActiveEco"} {
			v := "0+0x10"
			if s == state {
				v = "1+0x10"
			}
			out = append(out, series{"joulie_policy_node_state", map[string]string{
				"node": node, "state": s, "pod": pod, "job": "joulie"}, v})
		}
		profile := "eco"
		if state == "ActivePerformance" {
			profile = "performance"
		}
		for _, p := range []string{"performance", "eco"} {
			v := "0+0x10"
			if p == profile {
				v = "1+0x10"
			}
			out = append(out, series{"joulie_policy_node_profile_label", map[string]string{
				"node": node, "profile": p, "pod": pod, "job": "joulie"}, v})
		}
		return out
	}
	ss := []series{inlet("cpu-0", "22+0x10"), inlet("gpu-0", "26+0x10"), inlet("drain-0", "24+0x10"), inlet("other-0", "22.5+0x10")}
	for _, c := range [][3]string{
		{"cm-a", "cpu-0", "ActivePerformance"}, {"cm-b", "cpu-0", "ActivePerformance"},
		{"cm-a", "gpu-0", "ActivePerformance"}, {"cm-b", "gpu-0", "ActiveEco"},
		{"cm-a", "drain-0", "DrainingPerformance"},
		{"cm-a", "vm-0", "ActiveEco"},
	} {
		ss = append(ss, controller(c[0], c[1], c[2])...)
	}
	st := load(t, ss)

	// byNode evaluates the query at where, with every node selected, and
	// returns its samples by the node label: profile and value. It returns
	// nil for a query that fails.
	byNode := func(where, label string) map[string]string {
		t.Helper()
		i := slices.IndexFunc(queries, func(q dashboardQuery) bool { return q.where == where })
		if i < 0 {
			t.Fatalf("%s has no %s", path, where)
		}
		res, err := eval(t, st, strings.ReplaceAll(queries[i].expr, "$node", ".*"))
		if err != nil {
			t.Errorf("%s: %v", where, err)
			return nil
		}
		got := map[string]string{}
		for _, s := range res.Samples {
			got[s.Labels[label]] += strings.TrimSpace(fmt.Sprintf("%s %g", s.Labels["profile"], s.Value)) + ";"
		}
		return got
	}
	for _, c := range []struct {
		where, label string
		want         map[string]string
	}{
		{fmt.Sprintf("panel %q target 0", "Inlet temperature and power profile"), "k8s_node_name", map[string]string{
			"cpu-0": "performance 22;", "gpu-0": "eco 26;", "drain-0": "draining 24;", "vm-0": "eco NaN;", "other-0": "unmanaged 22.5;"}},
		{fmt.Sprintf("panel %q target 0", "Power profile per node"), "node", map[string]string{
			"cpu-0": "1;", "gpu-0": "3;", "drain-0": "2;", "vm-0": "3;"}},
		{fmt.Sprintf("panel %q target 0", "Inlet temperature per node"), "k8s_node_name", map[string]string{
			"cpu-0": "22;", "gpu-0": "26;", "drain-0": "24;", "vm-0": "NaN;", "other-0": "22.5;"}},
	} {
		if got := byNode(c.where, c.label); !maps.Equal(got, c.want) {
			t.Errorf("%s by %s = %v, want %v", c.where, c.label, got, c.want)
		}
	}
	nodes := slices.Sorted(maps.Keys(byNode("variable node", "k8s_node_name")))
	if want := []string{"cpu-0", "drain-0", "gpu-0", "other-0", "vm-0"}; !slices.Equal(nodes, want) {
		t.Errorf("variable node lists %v, want %v", nodes, want)
	}
}
