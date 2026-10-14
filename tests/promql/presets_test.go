// Package promql_test evaluates the queries of pkg/controller/inputs in the
// PromQL engine of Prometheus v3.15.0 (github.com/prometheus/prometheus
// v0.315.0), against series stored the way the Prometheus OTLP receiver
// stores them, and checks pkg/otel/promname against
// github.com/prometheus/otlptranslator v1.0.0, the translator that
// Prometheus version uses. It is a module of its own, with go 1.26 and
// replace github.com/matbun/joulie => ../.., so neither Prometheus nor its
// Go version reaches the root module; go test ./... at the root skips it.
// Run it with make test-promql, or cd tests/promql && go test ./...
//
// When a package registers a new name in pkg/otel/semconv, the promname
// table needs its row. Regenerate it from otlptranslator and review the
// diff:
//
//	cd tests/promql && go test -run TestPromNameAgreesWithOTLPTranslator -update
//
// The same queries run against a real Prometheus with the OTLP receiver in
// pkg/controller/inputs/live_test.go (build tag promlive); its header has
// the command.
package promql_test

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
)

// Every series is loaded at one sample a minute from 0 to 10 minutes, and
// every query is evaluated at 10 minutes, so the 5m rate window is covered.
var evalTime = time.Unix(600, 0)

// series is one stored series: a metric name and labels as Prometheus
// stores them, and its samples in promqltest notation (0+12000x10 is a
// counter rising 12000 a minute, 200 W from joules).
type series struct {
	name   string
	labels map[string]string
	values string
}

var (
	legacyName  = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	legacyLabel = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// descriptor writes s in the promqltest load syntax, quoting a metric or
// label name outside the legacy character set.
func (s series) descriptor() string {
	keys := make([]string, 0, len(s.labels))
	for k := range s.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	quoted := !legacyName.MatchString(s.name)
	if quoted {
		parts = append(parts, strconv.Quote(s.name))
	}
	for _, k := range keys {
		name := k
		if !legacyLabel.MatchString(k) {
			name = strconv.Quote(k)
		}
		parts = append(parts, name+"="+strconv.Quote(s.labels[k]))
	}
	if quoted {
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return s.name + "{" + strings.Join(parts, ", ") + "}"
}

// load stores ss in a fresh test storage.
func load(t *testing.T, ss []series) storage.Storage {
	t.Helper()
	var b strings.Builder
	b.WriteString("load 1m\n")
	for _, s := range ss {
		fmt.Fprintf(&b, "  %s %s\n", s.descriptor(), s.values)
	}
	st := promqltest.LoadedStorage(t, b.String())
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// eval runs q at evalTime in the PromQL engine and returns what
// inputs.Client.Query would return for it, so the package's readers run on
// the engine's answer.
func eval(t *testing.T, st storage.Storage, q string) (inputs.Result, error) {
	t.Helper()
	engine := promqltest.NewTestEngine(t, false, 0, promqltest.DefaultMaxSamplesPerQuery)
	qry, err := engine.NewInstantQuery(context.Background(), st, nil, q, evalTime)
	if err != nil {
		return inputs.Result{}, fmt.Errorf("parse: %w", err)
	}
	defer qry.Close()
	res := qry.Exec(context.Background())
	if res.Err != nil {
		return inputs.Result{}, res.Err
	}
	warnings, _ := res.Warnings.AsStrings(q, 0, 0)
	out := inputs.Result{Warnings: warnings}
	switch v := res.Value.(type) {
	case promql.Vector:
		out.Type = "vector"
		for _, s := range v {
			out.Samples = append(out.Samples, inputs.Sample{Labels: s.Metric.Map(), Value: s.F})
		}
	case promql.Scalar:
		out.Type = "scalar"
		out.Samples = []inputs.Sample{{Value: v.V}}
	default:
		return inputs.Result{}, fmt.Errorf("result type %s, want vector or scalar", res.Value.Type())
	}
	return out, nil
}

// evalByLabel runs q and keys its result by q's group label, failing the
// test on any error.
func evalByLabel(t *testing.T, st storage.Storage, q inputs.Query) (map[string]float64, inputs.Issues) {
	t.Helper()
	res, err := eval(t, st, q.PromQL)
	if err != nil {
		t.Fatalf("%s: %v", q.PromQL, err)
	}
	got, iss, err := res.ByLabel(q.GroupLabel)
	if err != nil {
		t.Fatalf("%s: %v", q.PromQL, err)
	}
	return got, iss
}

func assertValues(t *testing.T, what string, got, want map[string]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
		return
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok || math.Abs(g-w) > 1e-6*math.Max(1, math.Abs(w)) {
			t.Errorf("%s: got %v, want %v", what, got, want)
			return
		}
	}
}

// otelSource is one producer of OTel series: the job and instance the
// receiver derives from its resource, and the node of that resource.
type otelSource struct {
	job, instance, node string
}

// The DCGM bridge runs one collector per node with the node name as its
// service.instance.id, as the simulator does: its instance equals the
// simulator's under another job, so a target_info join that matches on
// instance alone finds two target_info series and fails.
var (
	simA   = otelSource{"joulie/joulie-simulator", "a", "a"}
	otherA = otelSource{"monitoring/collector", "x", "a"}
	dcgmA  = otelSource{"monitoring/dcgm", "a", "a"}
	simB   = otelSource{"joulie/joulie-simulator", "b", "b"}
	otherB = otelSource{"monitoring/collector", "y", "b"}
	dcgmB  = otelSource{"monitoring/dcgm", "b", "b"}
)

// otelSeries is one OTel metric point stream of a source, before the
// receiver translates it.
type otelSeries struct {
	src    otelSource
	metric semconv.Metric
	attrs  map[string]string
	values string
}

// translate stores ss as a Prometheus OTLP receiver with strategy s does.
// With promoted, k8s.node.name and host.name are promoted onto every data
// series of a source that has a node (the facility source has none).
// Either way each source gets a target_info series carrying them; sources
// in churn get a second one with another service.version, as a producer
// restart leaves behind.
func translate(s promname.Strategy, promoted bool, ss []otelSeries, churn ...otelSource) []series {
	var out []series
	seen := map[otelSource]bool{}
	var sources []otelSource
	for _, x := range ss {
		l := map[string]string{promname.LabelJob: x.src.job, promname.LabelInstance: x.src.instance}
		for k, v := range x.attrs {
			l[s.LabelName(k)] = v
		}
		if promoted && x.src.node != "" {
			l[s.LabelName(semconv.AttrK8sNodeName)] = x.src.node
			l[s.LabelName(semconv.AttrHostName)] = x.src.node
		}
		out = append(out, series{name: s.MetricName(x.metric), labels: l, values: x.values})
		if !seen[x.src] {
			seen[x.src] = true
			sources = append(sources, x.src)
		}
	}
	targetInfo := func(src otelSource, version string) series {
		l := map[string]string{
			promname.LabelJob: src.job, promname.LabelInstance: src.instance,
			s.LabelName(semconv.AttrServiceVersion): version,
		}
		if src.node != "" {
			l[s.LabelName(semconv.AttrK8sNodeName)] = src.node
			l[s.LabelName(semconv.AttrHostName)] = src.node
		}
		return series{name: promname.TargetInfo, values: "1x10", labels: l}
	}
	for _, src := range sources {
		out = append(out, targetInfo(src, "1"))
	}
	for _, src := range churn {
		out = append(out, targetInfo(src, "2"))
	}
	return out
}

func hw(hwType, hwID string) map[string]string {
	return map[string]string{semconv.AttrHwType: hwType, semconv.AttrHwID: hwID}
}

// nodeData is the node power data set. Node a: CPU packages at 200 and
// 150 W, the first reported by two producers; DRAM at 20 W, which is not
// counted; a GPU with an energy counter at 400 W (its 395 W gauge is not
// read) and a GPU with only a 300 W gauge from another producer: 1050 W.
// Node b: a CPU at 100 W and a GPU gauge at 250 W that share hw.id "0",
// which hw.type in the key keeps apart: 350 W. Host: counters at 1500 and 1450 W from two producers on a
// (its gauge is not read), gauges alone at 700 and 650 W from two producers
// on b; the host scope reads the larger, so a sum or an average of the
// producers reads another value.
func nodeData() []otelSeries {
	host := map[string]string{semconv.AttrHwID: semconv.HwIDHost}
	return []otelSeries{
		{simA, semconv.HwEnergy, hw("cpu", "cpu0"), "0+12000x10"},
		{otherA, semconv.HwEnergy, hw("cpu", "cpu0"), "0+12000x10"},
		{simA, semconv.HwEnergy, hw("cpu", "cpu1"), "0+9000x10"},
		{simA, semconv.HwEnergy, hw("memory", "memory0"), "0+1200x10"},
		{simA, semconv.HwEnergy, hw("gpu", "gpu0"), "0+24000x10"},
		{simA, semconv.HwPower, hw("gpu", "gpu0"), "395x10"},
		{dcgmA, semconv.HwPower, hw("gpu", "gpu1"), "300x10"},
		{simB, semconv.HwEnergy, hw("cpu", "0"), "0+6000x10"},
		{dcgmB, semconv.HwPower, hw("gpu", "0"), "250x10"},
		{simA, semconv.HwHostEnergy, host, "0+90000x10"},
		{otherA, semconv.HwHostEnergy, host, "0+87000x10"},
		{simA, semconv.HwHostPower, host, "1400x10"},
		{simB, semconv.HwHostPower, host, "700x10"},
		{otherB, semconv.HwHostPower, host, "650x10"},
	}
}

var nodeWant = map[inputs.Scope]map[string]float64{
	inputs.ScopeComponents: {"a": 1050, "b": 350},
	inputs.ScopeHost:       {"a": 1500, "b": 700},
}

// TestOTelNodePowerMatrix evaluates the otel preset's query under every
// strategy, both resource attribute forms and both scopes, against data
// stored with and without promotion and with a target_info churn. The
// labels form reads the promoted data and, on unpromoted data, returns
// series without the node label, which is what makes auto switch; the
// target_info form reads both.
func TestOTelNodePowerMatrix(t *testing.T) {
	for _, s := range promname.Strategies() {
		for _, promoted := range []bool{true, false} {
			st := load(t, translate(s, promoted, nodeData(), simA))
			for _, scope := range []inputs.Scope{inputs.ScopeComponents, inputs.ScopeHost} {
				for _, ra := range []inputs.ResourceAttributes{inputs.ResourceAttributesLabels, inputs.ResourceAttributesTargetInfo} {
					name := fmt.Sprintf("%s promoted=%t %s %s", s, promoted, scope, ra)
					q, _, err := inputs.NodePowerSpec{Scope: scope, Naming: s, ResourceAttributes: ra}.Query()
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					got, iss := evalByLabel(t, st, q)
					if ra == inputs.ResourceAttributesLabels && !promoted {
						if len(got) != 0 || iss.MissingLabel == 0 {
							t.Errorf("%s: got %v %+v, want no node and series without %q", name, got, iss, q.GroupLabel)
						}
						continue
					}
					assertValues(t, name, got, nodeWant[scope])
					if iss.MissingLabel != 0 || iss.NonFinite != 0 {
						t.Errorf("%s: issues %+v", name, iss)
					}
				}
			}
		}
	}
}

// TestE2HwTypeInComponentKey: on node b a CPU and a GPU share hw.id "0".
// The preset keys components by node, hw.type and hw.id and reads 350 W; a
// key without hw.type lets the CPU's energy hide the GPU's gauge, 100 W.
func TestE2HwTypeInComponentKey(t *testing.T) {
	st := load(t, translate(promname.Default, true, nodeData()))
	q, _, err := inputs.NodePowerSpec{ResourceAttributes: inputs.ResourceAttributesLabels}.Query()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := evalByLabel(t, st, q)
	assertValues(t, "preset", got, nodeWant[inputs.ScopeComponents])

	without := q
	without.PromQL = strings.ReplaceAll(q.PromQL, "(k8s_node_name, hw_type, hw_id)", "(k8s_node_name, hw_id)")
	if without.PromQL == q.PromQL {
		t.Fatal("the preset query has no (k8s_node_name, hw_type, hw_id) key to remove")
	}
	got, _ = evalByLabel(t, st, without)
	if got["b"] != 100 {
		t.Errorf("without hw_type in the key node b reads %v, want 100 (the collision this data set exercises)", got["b"])
	}
}

// TestE19SharedInstanceFailsLoudly: two nodes reported under one
// service.instance.id give one job and instance two target_info series
// with different nodes. The target_info form then fails rather than
// attributing the energy to either node.
func TestE19SharedInstanceFailsLoudly(t *testing.T) {
	shared := otelSource{"joulie/joulie-simulator", "sim-0", "a"}
	for _, s := range promname.Strategies() {
		data := translate(s, false, []otelSeries{{shared, semconv.HwEnergy, hw("cpu", "cpu0"), "0+12000x10"}})
		data = append(data, translate(s, false, []otelSeries{{otelSource{shared.job, shared.instance, "b"}, semconv.HwEnergy, hw("cpu", "cpu0"), "0+12000x10"}})...)
		st := load(t, dedupe(data))
		q, _, err := inputs.NodePowerSpec{Naming: s, ResourceAttributes: inputs.ResourceAttributesTargetInfo}.Query()
		if err != nil {
			t.Fatal(err)
		}
		res, err := eval(t, st, q.PromQL)
		if err == nil {
			t.Errorf("%s: got %+v, want an error: two nodes behind one instance", s, res)
		} else if !strings.Contains(err.Error(), "many-to-many") && !strings.Contains(err.Error(), "duplicate series") {
			t.Errorf("%s: error %v, want a many-to-many matching error", s, err)
		}
	}
}

// dedupe drops repeated series of the same name and labels, keeping the
// first.
func dedupe(ss []series) []series {
	seen := map[string]bool{}
	var out []series
	for _, s := range ss {
		k := s.descriptor()
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

// TestKeplerPresets evaluates both Kepler presets on the series the two
// schemas export. Kepler 0.12 has, per zone, the RAPL counter joules_total
// and the active and idle counters, which add up to the zone's energy:
// node a has the package zone summed over its sockets (path
// aggregated-package), core and DRAM zones that are not counted, and a GPU;
// node b has one zone per die, each named after its RAPL name file,
// package-<socket>-die-<die>, and psys and uncore zones that are not
// counted. Kepler 0.7: packages and platform per mode, dynamic and idle,
// which are summed.
func TestKeplerPresets(t *testing.T) {
	k012 := func(node, zone, path, active, idle string) []series {
		l := map[string]string{"zone": zone, "path": path, "node_name": node}
		return []series{
			{inputs.KeplerCPUActiveJoules, l, active},
			{inputs.KeplerCPUIdleJoules, l, idle},
		}
	}
	var ss []series
	for _, zs := range [][]series{
		k012("a", "package", "aggregated-package", "0+9000x10", "0+3000x10"),
		k012("a", "core", "aggregated-core", "0+6000x10", "0+600x10"),
		k012("a", "dram", "aggregated-dram", "0+1200x10", "0+600x10"),
		k012("b", "package-0-die-0", "/host/sys/class/powercap/intel-rapl:0", "0+3000x10", "0+1500x10"),
		k012("b", "package-0-die-1", "/host/sys/class/powercap/intel-rapl:1", "0+3000x10", "0+1500x10"),
		k012("b", "package-1-die-0", "/host/sys/class/powercap/intel-rapl:2", "0+3000x10", "0+1500x10"),
		k012("b", "package-1-die-1", "/host/sys/class/powercap/intel-rapl:3", "0+3000x10", "0+1500x10"),
		k012("b", "psys", "/host/sys/class/powercap/intel-rapl:4", "0+30000x10", "0+3000x10"),
		k012("b", "uncore", "aggregated-uncore", "0+600x10", "0+600x10"),
	} {
		ss = append(ss, zs...)
	}
	ss = append(ss, series{inputs.KeplerGPUJoules, map[string]string{"gpu": "0", "gpu_uuid": "GPU-2", "gpu_name": "H100", "vendor": "nvidia", "node_name": "a"}, "0+24000x10"})
	st := load(t, ss)
	q, _, err := inputs.NodePowerSpec{Preset: inputs.NodePowerPresetKepler}.Query()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := evalByLabel(t, st, q)
	assertValues(t, "kepler", got, map[string]float64{"a": 600, "b": 300})

	legacy := func(name, pkg, mode string, values string) series {
		return series{name, map[string]string{"package": pkg, "exported_instance": "a", "mode": mode, "source": "rapl-msr", "job": "kepler", "instance": "10.0.0.1:9102"}, values}
	}
	st = load(t, []series{
		legacy("kepler_node_package_joules_total", "0", "dynamic", "0+6000x10"),
		legacy("kepler_node_package_joules_total", "0", "idle", "0+3000x10"),
		legacy("kepler_node_package_joules_total", "1", "dynamic", "0+6000x10"),
		legacy("kepler_node_gpu_joules_total", "0", "dynamic", "0+12000x10"),
		legacy("kepler_node_platform_joules_total", "0", "dynamic", "0+30000x10"),
		legacy("kepler_node_platform_joules_total", "0", "idle", "0+15000x10"),
	})
	q, _, err = inputs.NodePowerSpec{Preset: inputs.NodePowerPresetKeplerLegacy}.Query()
	if err != nil {
		t.Fatal(err)
	}
	got, _ = evalByLabel(t, st, q)
	assertValues(t, "kepler-legacy components", got, map[string]float64{"a": 450})
	q, _, err = inputs.NodePowerSpec{Preset: inputs.NodePowerPresetKeplerLegacy, Scope: inputs.ScopeHost}.Query()
	if err != nil {
		t.Fatal(err)
	}
	got, _ = evalByLabel(t, st, q)
	assertValues(t, "kepler-legacy host", got, map[string]float64{"a": 750})
}

// TestKeplerPresetReadsAcrossACounterWrap: Kepler 0.12's
// kepler_node_cpu_joules_total is the RAPL counter, which wraps at
// max_energy_range_uj (262143 J here, a 61 uJ unit); the active and idle
// counters accumulate Kepler's wrap-corrected increments. With a wrap inside
// the window, rate() of the RAPL counter reads it as a reset and loses the
// energy up to the wrap, while the preset reads the zone's 200 W.
func TestKeplerPresetReadsAcrossACounterWrap(t *testing.T) {
	l := map[string]string{"zone": "package", "path": "/host/sys/class/powercap/intel-rapl:0", "node_name": "a"}
	st := load(t, []series{
		// 12000 J a minute from 170000 J, wrapping at 262143 J between the
		// samples at 7 and 8 minutes.
		{inputs.KeplerCPUJoules, l, "170000 182000 194000 206000 218000 230000 242000 254000 3857 15857 27857"},
		{inputs.KeplerCPUActiveJoules, l, "0+9000x10"},
		{inputs.KeplerCPUIdleJoules, l, "0+3000x10"},
	})
	q, _, err := inputs.NodePowerSpec{Preset: inputs.NodePowerPresetKepler}.Query()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := evalByLabel(t, st, q)
	assertValues(t, "kepler across a wrap", got, map[string]float64{"a": 200})

	raw := inputs.Query{GroupLabel: inputs.KeplerNodeLabel, PromQL: fmt.Sprintf(`sum by (%s) (rate(%s{%s="package"}[5m]))`, inputs.KeplerNodeLabel, inputs.KeplerCPUJoules, inputs.KeplerZoneLabel)}
	got, _ = evalByLabel(t, st, raw)
	if !(got["a"] < 190) {
		t.Errorf("rate() of the RAPL counter reads %v W across the wrap, want well under 200 W (the loss this data set exercises)", got["a"])
	}
}

// TestKeplerPackageZoneMatcher pins which Kepler 0.12 zone names the kepler
// preset counts: the package zone and every die zone, nothing else. PromQL
// anchors a regular expression at both ends.
func TestKeplerPackageZoneMatcher(t *testing.T) {
	zones := map[string]bool{
		"package": true, "package-0-die-0": true, "package-1-die-3": true, "package-12-die-10": true,
		"psys": false, "dram": false, "core": false, "uncore": false, "package-0": false, "package-0-die-": false,
		"xpackage": false, "package-0-die-0-x": false,
	}
	var ss []series
	for zone := range zones {
		ss = append(ss, series{inputs.KeplerCPUActiveJoules, map[string]string{"zone": zone, "path": "p", "node_name": zone}, "0+6000x10"})
	}
	st := load(t, ss)
	q, _, err := inputs.NodePowerSpec{Preset: inputs.NodePowerPresetKepler}.Query()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := evalByLabel(t, st, q)
	for zone, counted := range zones {
		if _, ok := got[zone]; ok != counted {
			t.Errorf("zone %q counted %t, want %t", zone, ok, counted)
		}
	}
}

// TestE3KeplerOrKeepsBothComponents: Kepler 0.7 gives a node's package and
// GPU counters the same labels. After rate() drops the names, a plain "or"
// keeps only the package series (100 W); the presets' label_replace keeps
// both (300 W).
func TestE3KeplerOrKeepsBothComponents(t *testing.T) {
	l := func(node string) map[string]string {
		return map[string]string{"package": "0", "exported_instance": node, "mode": "dynamic", "source": "x"}
	}
	st := load(t, []series{
		{"kepler_node_package_joules_total", l("a"), "0+6000x10"},
		{"kepler_node_gpu_joules_total", l("a"), "0+12000x10"},
		{"kepler_node_package_joules_total", l("b"), "0+6000x10"},
	})
	q, _, err := inputs.NodePowerSpec{Preset: inputs.NodePowerPresetKeplerLegacy}.Query()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := evalByLabel(t, st, q)
	assertValues(t, "preset", got, map[string]float64{"a": 300, "b": 100})

	plain := inputs.Query{GroupLabel: q.GroupLabel, PromQL: `sum by (exported_instance) (rate(kepler_node_package_joules_total[5m]) or rate(kepler_node_gpu_joules_total[5m]))`}
	got, _ = evalByLabel(t, st, plain)
	if got["a"] != 100 {
		t.Errorf("plain or reads %v on node a, want 100 (the drop this data set exercises)", got["a"])
	}
}

// TestCustomNodePowerQueries: a custom query with the node placeholder is
// run for the node and every series it returns is summed (150 + 50, where
// reading the first series alone gave 150); without the placeholder the
// query is summed by the node label, also when it ends in a comment.
func TestCustomNodePowerQueries(t *testing.T) {
	st := load(t, []series{
		{"node_energy_joules_total", map[string]string{"node": "a", "socket": "0"}, "0+9000x10"},
		{"node_energy_joules_total", map[string]string{"node": "a", "socket": "1"}, "0+3000x10"},
		{"node_energy_joules_total", map[string]string{"node": "b", "socket": "0"}, "0+6000x10"},
	})
	q, _, err := inputs.NodePowerSpec{CustomQuery: `rate(node_energy_joules_total{node="{node}"}[5m])`}.Query()
	if err != nil {
		t.Fatal(err)
	}
	if !q.PerNode {
		t.Fatalf("%+v: want a per-node query", q)
	}
	res, err := eval(t, st, strings.ReplaceAll(q.PromQL, inputs.NodePlaceholder, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Samples) != 2 || res.Sum() != 200 {
		t.Errorf("node a: %d series summing to %v, want 2 summing to 200", len(res.Samples), res.Sum())
	}

	q, _, err = inputs.NodePowerSpec{CustomQuery: `rate(node_energy_joules_total[5m])`, NodeLabel: "node"}.Query()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := evalByLabel(t, st, q)
	assertValues(t, "grouped", got, map[string]float64{"a": 200, "b": 100})

	// A query ending in a comment would swallow the closing parenthesis of
	// the grouping wrapped around it.
	q, _, err = inputs.NodePowerSpec{CustomQuery: "rate(node_energy_joules_total[5m]) # joules per second", NodeLabel: "node"}.Query()
	if err != nil {
		t.Fatal(err)
	}
	got, _ = evalByLabel(t, st, q)
	assertValues(t, "grouped, ending in a comment", got, map[string]float64{"a": 200, "b": 100})
}

// facilityData is the facility data set: two outdoor sensors at -4 and -2
// C, two IT meters with energy counters at 10000 and 5000 W, a cooling
// gauge at 4000 W, a zone with two sensors at 24 and 26 C, a rack with two
// PDU counters at 5000 W each, a rack with only a 7000 W gauge, a rack with
// a 1000 W counter whose 999 W gauge is not read, and a node with two inlet
// sensors at 21 and 23 C.
func facilityData() []otelSeries {
	fac := otelSource{"joulie/joulie-simulator", "facility", ""}
	node := simA
	a := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	zone, rack := semconv.AttrJoulieCoolingZoneName, semconv.AttrJoulieRackName
	return []otelSeries{
		{fac, semconv.JoulieFacilityAmbientTemperature, a("sensor", "n"), "-4x10"},
		{fac, semconv.JoulieFacilityAmbientTemperature, a("sensor", "s"), "-2x10"},
		{fac, semconv.JoulieFacilityITEnergy, a("meter", "ups-1"), "0+600000x10"},
		{fac, semconv.JoulieFacilityITEnergy, a("meter", "ups-2"), "0+300000x10"},
		{fac, semconv.JoulieFacilityCoolingPower, a("meter", "chiller"), "4000x10"},
		{fac, semconv.JoulieCoolingZoneAmbientTemperature, a(zone, "z1", "sensor", "1"), "24x10"},
		{fac, semconv.JoulieCoolingZoneAmbientTemperature, a(zone, "z1", "sensor", "2"), "26x10"},
		{fac, semconv.JoulieRackEnergy, a(rack, "r1", "pdu", "a"), "0+300000x10"},
		{fac, semconv.JoulieRackEnergy, a(rack, "r1", "pdu", "b"), "0+300000x10"},
		{fac, semconv.JoulieRackPower, a(rack, "r2"), "7000x10"},
		{fac, semconv.JoulieRackEnergy, a(rack, "r3"), "0+60000x10"},
		{fac, semconv.JoulieRackPower, a(rack, "r3"), "999x10"},
		{node, semconv.HwHostAmbientTemperature, a(semconv.AttrHwID, "inlet-1"), "21x10"},
		{node, semconv.HwHostAmbientTemperature, a(semconv.AttrHwID, "inlet-2"), "23x10"},
	}
}

// TestFacilityPresets evaluates every facility query of the otel preset
// under every strategy, on data stored with and without promotion and with
// a target_info churn of the node, and of the legacy preset. The node inlet
// is a resource's reading, so without promotion only target_info names its
// node; the other inputs carry their keys as point attributes.
func TestFacilityPresets(t *testing.T) {
	want := map[inputs.FacilityInput]map[string]float64{
		inputs.FacilityInputAmbientTemp:     {"": -3},
		inputs.FacilityInputITPower:         {"": 15000},
		inputs.FacilityInputCoolingPower:    {"": 4000},
		inputs.FacilityInputZoneAmbientTemp: {"z1": 25},
		inputs.FacilityInputRackPower:       {"r1": 10000, "r2": 7000, "r3": 1000},
		inputs.FacilityInputNodeInletTemp:   {"a": 23},
	}
	for _, s := range promname.Strategies() {
		qs, err := inputs.FacilitySpec{Naming: s}.Queries()
		if err != nil {
			t.Fatal(err)
		}
		if len(qs) != len(want) {
			t.Errorf("%s: %d queries, want %d", s, len(qs), len(want))
		}
		for _, promoted := range []bool{true, false} {
			st := load(t, translate(s, promoted, facilityData(), simA))
			for input, w := range want {
				checkFacilityInput(t, fmt.Sprintf("%s promoted=%t %s", s, promoted, input), st, qs[input], w)
			}
		}
	}

	st := load(t, []series{
		{"datacenter_ambient_temperature_celsius", map[string]string{"sensor": "n"}, "-4x10"},
		{"datacenter_ambient_temperature_celsius", map[string]string{"sensor": "s"}, "-2x10"},
		{"datacenter_total_it_power_watts", map[string]string{"meter": "ups-1"}, "10000x10"},
		{"datacenter_total_it_power_watts", map[string]string{"meter": "ups-2"}, "5000x10"},
		{"datacenter_cooling_power_watts", map[string]string{}, "4000x10"},
	})
	qs, err := inputs.FacilitySpec{Preset: inputs.FacilityPresetLegacy}.Queries()
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 3 {
		t.Errorf("legacy: %d queries, want 3", len(qs))
	}
	for _, input := range []inputs.FacilityInput{inputs.FacilityInputAmbientTemp, inputs.FacilityInputITPower, inputs.FacilityInputCoolingPower} {
		checkFacilityInput(t, "legacy "+string(input), st, qs[input], want[input])
	}
}

// checkFacilityInput reads q as the controller manager will: a single
// value when q has no group label, keyed by the label otherwise, where
// every series must carry the label.
func checkFacilityInput(t *testing.T, name string, st storage.Storage, q inputs.Query, want map[string]float64) {
	t.Helper()
	if q.PromQL == "" {
		t.Errorf("%s: no query", name)
		return
	}
	if q.GroupLabel != "" {
		got, iss := evalByLabel(t, st, q)
		assertValues(t, name, got, want)
		if iss.MissingLabel != 0 || iss.NonFinite != 0 {
			t.Errorf("%s: issues %+v", name, iss)
		}
		return
	}
	res, err := eval(t, st, q.PromQL)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	v, err := res.Single()
	if err != nil {
		t.Errorf("%s: %v (%+v)", name, err, res)
		return
	}
	assertValues(t, name, map[string]float64{"": v}, want)
}

// foreignTargetInfo is the target_info series of a producer that is not a
// power source, under strategy s: an application on node with no
// service.instance.id gets a job alone (instance ""), a StatefulSet pod
// gets its pod name as instance.
func foreignTargetInfo(s promname.Strategy, job, instance, node string) series {
	l := map[string]string{
		promname.LabelJob:                    job,
		s.LabelName(semconv.AttrK8sNodeName): node,
		s.LabelName(semconv.AttrHostName):    node,
	}
	if instance != "" {
		l[promname.LabelInstance] = instance
	}
	return series{name: promname.TargetInfo, values: "1x10", labels: l}
}

// foreignProducers are the target_info series of two producers in the same
// Prometheus that report no power: an application without
// service.instance.id running on nodes a and b, and a StatefulSet pod
// rescheduled from a to b within the lookback window. Each gives one job
// and instance two target_info series with different nodes, so a join
// whose right side keeps every target_info series fails as many-to-many
// on every node, although no power series matches them.
func foreignProducers(s promname.Strategy) []series {
	return []series{
		foreignTargetInfo(s, "default/app", "", "a"),
		foreignTargetInfo(s, "default/app", "", "b"),
		foreignTargetInfo(s, "default/db", "db-0", "a"),
		foreignTargetInfo(s, "default/db", "db-0", "b"),
	}
}

// TestForeignTargetInfoLeavesJoinsAlone: target_info series of producers
// that report no power must not reach the join, under either resource
// attribute form, either scope and both receiver settings, nor in the node
// inlet query.
func TestForeignTargetInfoLeavesJoinsAlone(t *testing.T) {
	for _, s := range promname.Strategies() {
		for _, promoted := range []bool{true, false} {
			st := load(t, append(translate(s, promoted, nodeData(), simA), foreignProducers(s)...))
			for _, scope := range []inputs.Scope{inputs.ScopeComponents, inputs.ScopeHost} {
				name := fmt.Sprintf("%s promoted=%t %s", s, promoted, scope)
				q, _, err := inputs.NodePowerSpec{Scope: scope, Naming: s, ResourceAttributes: inputs.ResourceAttributesTargetInfo}.Query()
				if err != nil {
					t.Fatal(err)
				}
				got, _ := evalByLabel(t, st, q)
				assertValues(t, name+" target_info", got, nodeWant[scope])
			}

			qs, err := inputs.FacilitySpec{Naming: s}.Queries()
			if err != nil {
				t.Fatal(err)
			}
			st = load(t, append(translate(s, promoted, facilityData(), simA), foreignProducers(s)...))
			got, _ := evalByLabel(t, st, qs[inputs.FacilityInputNodeInletTemp])
			assertValues(t, fmt.Sprintf("%s promoted=%t node inlet", s, promoted), got, map[string]float64{"a": 23})
		}
	}
}

// TestNodeInletJoinsOnlyUnlabelledSeries: two nodes reported under one
// service.instance.id give one job and instance two target_info series.
// With promotion every inlet series carries its node, the query
// never joins target_info and reads both nodes; without promotion it must
// join, and fails rather than attributing a reading to either node.
func TestNodeInletJoinsOnlyUnlabelledSeries(t *testing.T) {
	src := func(node string) otelSource { return otelSource{"joulie/joulie-simulator", "sim-0", node} }
	inlet := func(node, values string) otelSeries {
		return otelSeries{src(node), semconv.HwHostAmbientTemperature, map[string]string{semconv.AttrHwID: "inlet"}, values}
	}
	for _, s := range promname.Strategies() {
		qs, err := inputs.FacilitySpec{Naming: s}.Queries()
		if err != nil {
			t.Fatal(err)
		}
		q := qs[inputs.FacilityInputNodeInletTemp]

		data := append(translate(s, true, []otelSeries{inlet("a", "21x10")}), translate(s, true, []otelSeries{inlet("b", "25x10")})...)
		got, _ := evalByLabel(t, load(t, dedupe(data)), q)
		assertValues(t, fmt.Sprintf("%s promoted", s), got, map[string]float64{"a": 21, "b": 25})

		data = append(translate(s, false, []otelSeries{inlet("a", "21x10")}), translate(s, false, []otelSeries{inlet("b", "25x10")})...)
		if res, err := eval(t, load(t, dedupe(data)), q.PromQL); err == nil {
			t.Errorf("%s unpromoted: got %+v, want an error: two nodes behind one instance", s, res)
		}
	}
}

// metricNames returns the metric names q selects, in the order the parser
// visits them, without repeats. Every selector of a preset query names its
// metric with an equality matcher.
func metricNames(t *testing.T, q string) []string {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	var out []string
	seen := map[string]bool{}
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		vs, ok := n.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		name := ""
		for _, m := range vs.LabelMatchers {
			if m.Name == labels.MetricName && m.Type == labels.MatchEqual {
				name = m.Value
			}
		}
		if name == "" {
			t.Errorf("%s: selector %s names no metric", q, vs)
		} else if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		return nil
	})
	return out
}

// sameSet reports whether a and b hold the same strings, ignoring order.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// TestPresetQueriesSelectTheSemconvInputs: parsed by the PromQL parser, the
// otel presets select exactly the metrics semconv lists as inputs, spelled
// by promname under each strategy: semconv.NodeInputs across both scopes
// (plus target_info in the target_info form) and semconv.FacilityInputs
// across the six facility inputs (plus target_info, which the node inlet
// joins for series without the node label). A metric added to either list
// without a query, or a query reading a name outside them, fails here.
func TestPresetQueriesSelectTheSemconvInputs(t *testing.T) {
	for _, s := range promname.Strategies() {
		var want []string
		for _, m := range semconv.NodeInputs() {
			want = append(want, s.MetricName(m))
		}
		for _, ra := range []inputs.ResourceAttributes{inputs.ResourceAttributesLabels, inputs.ResourceAttributesTargetInfo} {
			var got []string
			for _, scope := range []inputs.Scope{inputs.ScopeComponents, inputs.ScopeHost} {
				q, _, err := inputs.NodePowerSpec{Scope: scope, Naming: s, ResourceAttributes: ra}.Query()
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, metricNames(t, q.PromQL)...)
			}
			w := want
			if ra == inputs.ResourceAttributesTargetInfo {
				w = append(append([]string(nil), want...), promname.TargetInfo)
			}
			if !sameSet(dedupeStrings(got), w) {
				t.Errorf("%s %s node power selects %v, want %v", s, ra, dedupeStrings(got), w)
			}
		}

		want = []string{promname.TargetInfo}
		for _, m := range semconv.FacilityInputs() {
			want = append(want, s.MetricName(m))
		}
		qs, err := inputs.FacilitySpec{Naming: s}.Queries()
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, input := range inputs.FacilityInputs() {
			got = append(got, metricNames(t, qs[input].PromQL)...)
		}
		if !sameSet(dedupeStrings(got), want) {
			t.Errorf("%s facility selects %v, want %v", s, dedupeStrings(got), want)
		}
	}
}

// TestFacilityMetricNamesMatchPresetQueries: for each facility input,
// FacilityMetricNames is exactly the set of metrics its otel query selects
// under the four strategies and its legacy query selects, as the PromQL
// parser reads them, without target_info. The simulator's fake Prometheus
// answers by these names, so a name missing here leaves a preset query
// unanswered; target_info is the node inlet's join, not a reading, and
// node power queries select it too.
func TestFacilityMetricNamesMatchPresetQueries(t *testing.T) {
	legacy, err := inputs.FacilitySpec{Preset: inputs.FacilityPresetLegacy}.Queries()
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range inputs.FacilityInputs() {
		var want []string
		for _, s := range promname.Strategies() {
			qs, err := inputs.FacilitySpec{Naming: s}.Queries()
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, metricNames(t, qs[input].PromQL)...)
		}
		if q, ok := legacy[input]; ok {
			want = append(want, metricNames(t, q.PromQL)...)
		}
		want = withoutString(dedupeStrings(want), promname.TargetInfo)
		if got := inputs.FacilityMetricNames(input); !sameSet(got, want) {
			t.Errorf("%s: FacilityMetricNames %v, the preset queries select %v", input, got, want)
		}
	}
}

// withoutString returns ss without x.
func withoutString(ss []string, x string) []string {
	var out []string
	for _, s := range ss {
		if s != x {
			out = append(out, s)
		}
	}
	return out
}

func dedupeStrings(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
