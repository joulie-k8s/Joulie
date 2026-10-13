package simcluster

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
	goyaml "sigs.k8s.io/yaml/goyaml.v3"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/compat"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

var updateGoldens = flag.Bool("update", false, "rewrite the goldens under simulator/pkg/simcluster/testdata/ from the current code")

// testTimings reads both committed timing files.
func testTimings(t *testing.T) []Timing {
	t.Helper()
	var out []Timing
	for _, name := range []string{TimingDev, TimingProduction} {
		tm, err := ReadTiming(testRepo, name)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tm)
	}
	return out
}

func testTelemetry(t *testing.T, m Mode, sc Scope, tm Timing) TelemetryParts {
	t.Helper()
	parts, err := Telemetry(m, sc, tm)
	if err != nil {
		t.Fatalf("Telemetry(%s, %s, %s): %v", m, sc, tm.Name, err)
	}
	return parts
}

// testScopes returns the scopes mode supports.
func testScopes(m Mode) []Scope {
	if telKepler(m) {
		return []Scope{ScopeComponents}
	}
	return Scopes()
}

// testQueries returns every query a mode generates under its scopes: the
// checks' and the wait query.
func testQueries(t *testing.T, m Mode, tm Timing) []string {
	t.Helper()
	var out []string
	for _, sc := range testScopes(m) {
		parts := testTelemetry(t, m, sc, tm)
		for _, c := range parts.Checks.Checks {
			out = append(out, c.Query)
		}
		out = append(out, parts.Wait.Query)
	}
	return out
}

// testTelemetryGolden is every part of a mode and timing under scope
// components, then the parts scope host changes, as YAML documents.
func testTelemetryGolden(t *testing.T, m Mode, tm Timing) []byte {
	t.Helper()
	var b bytes.Buffer
	components := testTelemetry(t, m, ScopeComponents, tm)
	for _, sc := range Scopes() {
		parts, err := Telemetry(m, sc, tm)
		if err != nil {
			fmt.Fprintf(&b, "---\n# scope %s: %v\n", sc, err)
			continue
		}
		for _, p := range Parts() {
			out, err := parts.Part(p)
			if sc == ScopeHost && p != PartJoulieValues && p != PartChecks {
				// Scope host changes Joulie's settings and the checks only.
				if was, _ := components.Part(p); !bytes.Equal(testDropHead(out), testDropHead(was)) {
					t.Errorf("mode %s timing %s: scope host changes part %s", m, tm.Name, p)
				}
				continue
			}
			b.WriteString("---\n")
			switch {
			case err != nil:
				fmt.Fprintf(&b, "# %s, scope %s: %v\n", p, sc, err)
			case !bytes.HasPrefix(out, []byte("#")):
				fmt.Fprintf(&b, "# %s, scope %s:\n", p, sc)
				fallthrough
			default:
				b.Write(out)
			}
		}
	}
	return b.Bytes()
}

// testDropHead drops a part's first line, the comment naming its scope.
func testDropHead(b []byte) []byte {
	if bytes.HasPrefix(b, []byte("#")) {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			return b[i+1:]
		}
	}
	return b
}

// T01: every part of every mode and timing equals its golden.
func TestT01TelemetryGoldens(t *testing.T) {
	for _, tm := range testTimings(t) {
		for _, m := range Modes() {
			name := fmt.Sprintf("%s-%s", m, tm.Name)
			t.Run(name, func(t *testing.T) {
				testGolden(t, filepath.Join("testdata", "telemetry", name+".golden.yaml"), testTelemetryGolden(t, m, tm))
			})
		}
	}
}

// T02: every metric and label name in a generated query is promname's
// spelling, under the mode's translation strategy, of a semconv entry, or
// a pinned Kepler name.
func TestT02QueryNamesComeFromPromname(t *testing.T) {
	for _, m := range Modes() {
		s := telStrategy(m)
		metrics := map[string]bool{promname.TargetInfo: true, keplerCPUJoules: true, keplerGPUJoules: true, kepler07PackageJoules: true, kepler07PlatformJoules: true}
		for _, sm := range semconv.Metrics() {
			metrics[s.MetricName(sm)] = true
		}
		labels := map[string]bool{promname.LabelJob: true, promname.LabelInstance: true, keplerNodeLabel: true, keplerZoneLabel: true, kepler07NodeLabel: true}
		for _, a := range semconv.Attributes() {
			labels[s.LabelName(a)] = true
		}
		for _, tm := range testTimings(t) {
			for _, q := range testQueries(t, m, tm) {
				names, err := testPromQLNames(q)
				if err != nil {
					t.Errorf("mode %s: %v in %s", m, err, q)
					continue
				}
				if len(names.metrics) == 0 {
					t.Errorf("mode %s: no metric found in %s", m, q)
				}
				for _, n := range names.metrics {
					if !metrics[n] {
						t.Errorf("mode %s: metric %q is no promname %s spelling of a semconv metric and no pinned Kepler name, in %s", m, n, s, q)
					}
				}
				for _, n := range names.labels {
					if !labels[n] {
						t.Errorf("mode %s: label %q is no promname %s spelling of a semconv attribute and no pinned Kepler label, in %s", m, n, s, q)
					}
				}
			}
		}
	}
	// The checker itself: names quoted or bare, in selectors and label
	// lists, and a name outside the sets.
	names, err := testPromQLNames(`sum by ("k8s.node.name", hw_id) (rate({"hw.energy_joules_total", "hw.type"=~"cpu|gpu", job="a/b"}[5m]) * on (job) group_left (x) max by (job) (target_info{y="z"}))`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"hw.energy_joules_total", "target_info"}; !reflect.DeepEqual(names.metrics, want) {
		t.Errorf("checker metrics %v, want %v", names.metrics, want)
	}
	if want := []string{"k8s.node.name", "hw_id", "hw.type", "job", "job", "x", "job", "y"}; !reflect.DeepEqual(names.labels, want) {
		t.Errorf("checker labels %v, want %v", names.labels, want)
	}
	if want := []string{"5m"}; !reflect.DeepEqual(names.windows, want) {
		t.Errorf("checker windows %v, want %v", names.windows, want)
	}
}

// testPromQLKeywords are the PromQL words the generated queries use that
// name no metric.
var testPromQLKeywords = map[string]bool{
	"sum": true, "max": true, "min": true, "avg": true, "count": true, "rate": true,
	"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true,
	"or": true, "and": true, "unless": true,
}

var testPromQLListWords = map[string]bool{"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true}

type testNames struct {
	metrics, labels, windows []string
}

// testPromQLNames lists the metric names, label names and range windows of
// a query, in order. It reads the PromQL subset the harness writes:
// selectors with or without a quoted name, label lists after by, on and
// group_left, ranges, and the operators between them.
func testPromQLNames(q string) (testNames, error) {
	var out testNames
	braces, inList, listNext := 0, false, false
	afterOp := false
	i := 0
	next := func() byte {
		j := i
		for j < len(q) && q[j] == ' ' {
			j++
		}
		if j < len(q) {
			return q[j]
		}
		return 0
	}
	for i < len(q) {
		c := q[i]
		switch {
		case c == ' ':
			i++
		case c == '"':
			j := i + 1
			for j < len(q) && q[j] != '"' {
				if q[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(q) {
				return out, fmt.Errorf("unterminated string at %d", i)
			}
			s, err := strconv.Unquote(q[i : j+1])
			if err != nil {
				return out, err
			}
			i = j + 1
			switch {
			case inList:
				out.labels = append(out.labels, s)
			case braces > 0 && afterOp:
				afterOp = false // a matcher value
			case braces > 0 && (next() == '=' || next() == '!'):
				out.labels = append(out.labels, s)
			case braces > 0:
				out.metrics = append(out.metrics, s)
			default:
				return out, fmt.Errorf("string %q outside a selector or label list", s)
			}
		case c == '_' || c == ':' || (c|0x20 >= 'a' && c|0x20 <= 'z'):
			j := i
			for j < len(q) && (q[j] == '_' || q[j] == ':' || (q[j]|0x20 >= 'a' && q[j]|0x20 <= 'z') || (q[j] >= '0' && q[j] <= '9')) {
				j++
			}
			word := q[i:j]
			i = j
			switch {
			case inList:
				out.labels = append(out.labels, word)
			case braces > 0:
				out.labels = append(out.labels, word)
			case testPromQLKeywords[word]:
				listNext = testPromQLListWords[word]
			default:
				out.metrics = append(out.metrics, word)
			}
		case c == '[':
			j := strings.IndexByte(q[i:], ']')
			if j < 0 {
				return out, fmt.Errorf("unterminated range at %d", i)
			}
			out.windows = append(out.windows, q[i+1:i+j])
			i += j + 1
		case c == '(':
			if listNext {
				inList = true
			}
			listNext = false
			i++
		case c == ')':
			inList = false
			i++
		case c == '{':
			braces++
			i++
		case c == '}':
			braces--
			i++
		case c == '=' || c == '!' || c == '~':
			if braces > 0 {
				afterOp = true
			}
			i++
		case c == ',' || c == '*' || c == '+' || c == '-' || c == '/':
			i++
		default:
			return out, fmt.Errorf("unexpected %q at %d", c, i)
		}
	}
	return out, nil
}

// T03: no generated query holds the per-node placeholder of a custom
// Joulie query.
func TestT03NoNodePlaceholder(t *testing.T) {
	for _, m := range Modes() {
		for _, tm := range testTimings(t) {
			for _, q := range testQueries(t, m, tm) {
				if strings.Contains(q, inputs.NodePlaceholder) {
					t.Errorf("mode %s: %s holds %s", m, q, inputs.NodePlaceholder)
				}
			}
		}
	}
}

// T04: every range in the checks is the timing's rate window, and the wait
// queries have none.
func TestT04WindowSubstituted(t *testing.T) {
	odd := testTimings(t)[0]
	odd.Name, odd.RateWindow = "odd", 2*time.Minute+30*time.Second
	for _, tm := range append(testTimings(t), odd) {
		want, err := inputs.PromDuration(tm.RateWindow)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range Modes() {
			for _, sc := range testScopes(m) {
				parts := testTelemetry(t, m, sc, tm)
				for _, c := range parts.Checks.Checks {
					names, err := testPromQLNames(c.Query)
					if err != nil {
						t.Fatal(err)
					}
					if len(names.windows) == 0 || len(names.windows) != strings.Count(c.Query, "rate(") {
						t.Errorf("mode %s timing %s check %s: ranges %v for %d rate() in %s", m, tm.Name, c.ID, names.windows, strings.Count(c.Query, "rate("), c.Query)
					}
					for _, w := range names.windows {
						if w != want {
							t.Errorf("mode %s timing %s check %s: range [%s], want [%s]", m, tm.Name, c.ID, w, want)
						}
					}
				}
				if strings.Contains(parts.Wait.Query, "[") {
					t.Errorf("mode %s: wait query %s has a range", m, parts.Wait.Query)
				}
			}
		}
	}
}

// T05: promname spells the names the harness relies on as otlptranslator
// v1.0.0, the translator of Prometheus v3.15.0, does under each strategy.
func TestT05PromnameRows(t *testing.T) {
	strategies := []promname.Strategy{promname.UnderscoreEscapingWithSuffixes, promname.NoUTF8EscapingWithSuffixes, promname.UnderscoreEscapingWithoutSuffixes, promname.NoTranslation}
	for _, row := range []struct {
		m    semconv.Metric
		want [4]string
	}{
		{semconv.HwEnergy, [4]string{"hw_energy_joules_total", "hw.energy_joules_total", "hw_energy", "hw.energy"}},
		{semconv.HwPower, [4]string{"hw_power_watts", "hw.power_watts", "hw_power", "hw.power"}},
		{semconv.HwGPUUtilization, [4]string{"hw_gpu_utilization_ratio", "hw.gpu.utilization_ratio", "hw_gpu_utilization", "hw.gpu.utilization"}},
		{semconv.HwHostAmbientTemperature, [4]string{"hw_host_ambient_temperature_celsius", "hw.host.ambient_temperature_celsius", "hw_host_ambient_temperature", "hw.host.ambient_temperature"}},
	} {
		for i, s := range strategies {
			if got := s.MetricName(row.m); got != row.want[i] {
				t.Errorf("%s under %s: %q, want %q", row.m.Name, s, got, row.want[i])
			}
		}
	}
	want := [4]string{"k8s_node_name", "k8s.node.name", "k8s_node_name", "k8s.node.name"}
	for i, s := range strategies {
		if got := s.LabelName(semconv.AttrK8sNodeName); got != want[i] {
			t.Errorf("%s under %s: %q, want %q", semconv.AttrK8sNodeName, s, got, want[i])
		}
	}
}

// The pinned Kepler names are the ones the simulator's compatibility
// exporters serve: the truth fixture, observed twice 10 s apart, is served
// by each Kepler mode's exporter, and every metric and label of the mode's
// queries is in that exposition (exported_instance is Kepler 0.7's instance
// before the scrape renames it). The Nodes served are those whose Kepler
// truth is not zero, and Kepler 0.7 serves as many platform series per Node
// as TruthKepler07PlatformSeries says.
func TestKeplerNamesMatchTheCompatExporters(t *testing.T) {
	from := testSnapshot(t)
	to := testSnapshot(t)
	to.Seq++
	to.Time = from.Time.Add(10 * time.Second)
	for i := range to.Nodes {
		n := &to.Nodes[i]
		n.Host.EnergyJ += 10 * n.Host.PowerW
		for j := range n.Components {
			n.Components[j].EnergyJ += 10 * n.Components[j].PowerW
		}
	}
	truths := NodeTruths(to)
	tm := testTimings(t)[0]
	for _, m := range []Mode{ModeKepler, ModeKeplerLegacy} {
		name := telExporter(m)
		set, err := compat.NewSet([]string{name}, compat.Options{})
		if err != nil {
			t.Fatal(err)
		}
		set.Observe(from)
		set.Observe(to)
		rec := httptest.NewRecorder()
		set.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, compat.Path(name), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d", name, rec.Code)
		}
		series := testExposition(t, rec.Body.String())
		metrics, labels := map[string]bool{}, map[string]bool{}
		for _, s := range series {
			metrics[s.name] = true
			for k := range s.labels {
				labels[k] = true
			}
		}

		parts := testTelemetry(t, m, ScopeComponents, tm)
		queries := []string{parts.Wait.Query}
		for _, c := range parts.Checks.Checks {
			queries = append(queries, c.Query)
		}
		for _, q := range queries {
			names, err := testPromQLNames(q)
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range names.metrics {
				if !metrics[n] {
					t.Errorf("mode %s: %s serves no metric %s, which %s reads", m, name, n, q)
				}
			}
			for _, l := range names.labels {
				if l == kepler07NodeLabel {
					l = promname.LabelInstance
				}
				if !labels[l] {
					t.Errorf("mode %s: %s serves no label %s, which %s reads", m, name, l, q)
				}
			}
		}

		node := parts.Checks.NodeLabel
		if node == kepler07NodeLabel {
			node = promname.LabelInstance
		}
		platform, served := map[string]float64{}, map[string]bool{}
		for _, s := range series {
			n, ok := s.labels[node]
			if !ok {
				t.Errorf("mode %s: %s series %s has no label %s", m, name, s.name, node)
				continue
			}
			served[n] = true
			if want, err := EvalTruth(TruthPackagesNVIDIAGPUs, truths[n]); err != nil || want == 0 {
				t.Errorf("mode %s: %s serves Node %s, whose Kepler truth is %g (%v)", m, name, n, want, err)
			}
			if s.name == kepler07PlatformJoules {
				platform[n]++
			}
		}
		for n, nt := range truths {
			if want, _ := EvalTruth(TruthPackagesNVIDIAGPUs, nt); want != 0 && !served[n] {
				t.Errorf("mode %s: %s serves nothing for Node %s, whose Kepler truth is %g", m, name, n, want)
			}
		}
		if m != ModeKeplerLegacy {
			continue
		}
		for n, nt := range truths {
			want, err := EvalTruth(TruthKepler07PlatformSeries, nt)
			if err != nil {
				t.Fatal(err)
			}
			if platform[n] != want {
				t.Errorf("%s serves %g %s series for Node %s, truth %s is %g", name, platform[n], kepler07PlatformJoules, n, TruthKepler07PlatformSeries, want)
			}
		}
	}
}

type testSeries struct {
	name   string
	labels map[string]string
}

var testSeriesLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})? \S+$`)
var testSeriesLabel = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

// testExposition reads the series of a Prometheus text exposition.
func testExposition(t *testing.T, text string) []testSeries {
	t.Helper()
	var out []testSeries
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := testSeriesLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("exposition line %q", line)
		}
		s := testSeries{name: m[1], labels: map[string]string{}}
		for _, l := range testSeriesLabel.FindAllStringSubmatch(m[2], -1) {
			s.labels[l[1]] = l[2]
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		t.Fatal("the exposition holds no series")
	}
	return out
}

// testPromConfig decodes a mode's prom-config.
func testPromConfig(t *testing.T, parts TelemetryParts) promConfig {
	t.Helper()
	var c promConfig
	dec := goyaml.NewDecoder(bytes.NewReader(parts.PromConfig))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("prom-config of mode %s: %v", parts.Mode, err)
	}
	return c
}

// T06: Prometheus keeps a 30 minute out-of-order window in every mode,
// translates names to UTF-8 in mode otel-utf8 only, and leaves the node
// attributes unpromoted in the two unpromoted modes only.
func TestT06PrometheusSettingsPerMode(t *testing.T) {
	for _, tm := range testTimings(t) {
		for _, m := range Modes() {
			c := testPromConfig(t, testTelemetry(t, m, ScopeComponents, tm))
			if c.Storage.TSDB.OutOfOrderTimeWindow != "30m" {
				t.Errorf("mode %s: out_of_order_time_window %q, want 30m", m, c.Storage.TSDB.OutOfOrderTimeWindow)
			}
			wantStrategy := string(promname.UnderscoreEscapingWithSuffixes)
			if m == ModeOTelUTF8 {
				wantStrategy = string(promname.NoUTF8EscapingWithSuffixes)
			}
			if c.OTLP.TranslationStrategy != wantStrategy {
				t.Errorf("mode %s: translation_strategy %q, want %q", m, c.OTLP.TranslationStrategy, wantStrategy)
			}
			wantPromoted := []string{semconv.AttrK8sNodeName, semconv.AttrHostName}
			if m == ModeOTelUnpromoted || m == ModeOTelLabelsUnpromoted {
				wantPromoted = []string{}
			}
			if !reflect.DeepEqual(c.OTLP.PromoteResourceAttributes, wantPromoted) {
				t.Errorf("mode %s: promote_resource_attributes %v, want %v", m, c.OTLP.PromoteResourceAttributes, wantPromoted)
			}
			if c.Global.ScrapeInterval != telDuration(tm.ScrapeInterval) {
				t.Errorf("mode %s timing %s: scrape_interval %s, want %s", m, tm.Name, c.Global.ScrapeInterval, telDuration(tm.ScrapeInterval))
			}
		}
	}
}

// T07: with docker and the pinned Prometheus image present, promtool
// accepts every prom-config (it rejects unknown keys) and every generated
// query as the expression of a recording rule. Skipped otherwise.
func TestT07PromtoolAcceptsConfigsAndQueries(t *testing.T) {
	image := testVersion(t, "PROMETHEUS_IMAGE")
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Skipf("image %s is not present (docker pull %s)", image, image)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tm := range testTimings(t) {
		for _, m := range Modes() {
			for _, sc := range testScopes(m) {
				parts := testTelemetry(t, m, sc, tm)
				base := fmt.Sprintf("%s-%s-%s", m, sc, tm.Name)
				type rule struct {
					Record string `json:"record"`
					Expr   string `json:"expr"`
				}
				var rules []rule
				for i, c := range parts.Checks.Checks {
					rules = append(rules, rule{Record: fmt.Sprintf("simcluster:check_%d", i), Expr: c.Query})
				}
				rules = append(rules, rule{Record: "simcluster:wait", Expr: parts.Wait.Query})
				doc, err := yaml.Marshal(map[string]any{"groups": []any{map[string]any{"name": base, "rules": rules}}})
				if err != nil {
					t.Fatal(err)
				}
				for name, b := range map[string][]byte{base + ".config.yml": parts.PromConfig, base + ".rules.yml": doc} {
					if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	script := `rc=0
for f in /w/*.config.yml; do promtool check config "$f" >/tmp/out 2>&1 || { echo "FAIL $f"; cat /tmp/out; rc=1; }; done
for f in /w/*.rules.yml; do promtool check rules "$f" >/tmp/out 2>&1 || { echo "FAIL $f"; cat /tmp/out; rc=1; }; done
exit $rc`
	out, err := exec.Command("docker", "run", "--rm", "-v", dir+":/w:ro", "--entrypoint", "/bin/sh", image, "-c", script).CombinedOutput()
	if err != nil {
		t.Errorf("promtool: %v\n%s", err, out)
	}
}

// testVersion reads a key of scripts/simcluster/versions.env.
func testVersion(t *testing.T, key string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(testRepo, "scripts", "simcluster", "versions.env"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), key+"="); ok {
			return v
		}
	}
	t.Fatalf("versions.env has no %s", key)
	return ""
}

// T08: joulie-values maps the timing onto the variables Joulie reads,
// RECONCILE_INTERVAL and SPEC_READ_TIMEOUT, turns the facility metrics on,
// reads node power from Prometheus without a custom query, and sets no
// harness name (CONTROLLER_*, AGENT_*).
func TestT08JoulieValuesUseRealVariables(t *testing.T) {
	for _, tm := range testTimings(t) {
		for _, m := range Modes() {
			for _, sc := range testScopes(m) {
				parts := testTelemetry(t, m, sc, tm)
				var v struct {
					ControllerManager struct {
						Env map[string]string `json:"env"`
					} `json:"controllerManager"`
					Agent struct {
						Env map[string]string `json:"env"`
					} `json:"agent"`
				}
				if err := yaml.UnmarshalStrict(parts.JoulieValues, &v); err != nil {
					t.Fatalf("mode %s: %v", m, err)
				}
				cm, agent := v.ControllerManager.Env, v.Agent.Env
				want := map[string]string{
					"RECONCILE_INTERVAL": telDuration(tm.ControllerReconcile), "ENABLE_FACILITY_METRICS": "true", "NODE_POWER_SOURCE": "prometheus",
					"FACILITY_POLL_INTERVAL": telDuration(tm.FacilityPoll), "PROMETHEUS_RATE_WINDOW": telDuration(tm.RateWindow),
				}
				for k, w := range want {
					if cm[k] != w {
						t.Errorf("mode %s timing %s: controllerManager.env.%s %q, want %q", m, tm.Name, k, cm[k], w)
					}
				}
				if agent["RECONCILE_INTERVAL"] != telDuration(tm.AgentReconcile) || agent["SPEC_READ_TIMEOUT"] != telDuration(tm.SpecReadTimeout) {
					t.Errorf("mode %s timing %s: agent.env %v, want RECONCILE_INTERVAL %s and SPEC_READ_TIMEOUT %s", m, tm.Name, agent, telDuration(tm.AgentReconcile), telDuration(tm.SpecReadTimeout))
				}
				for _, env := range []map[string]string{cm, agent} {
					for k := range env {
						if strings.HasPrefix(k, "CONTROLLER_") || strings.HasPrefix(k, "AGENT_") || k == "NODE_POWER_PROMETHEUS_QUERY" {
							t.Errorf("mode %s: joulie-values sets %s", m, k)
						}
					}
				}
				if (cm["NODE_POWER_SCOPE"] == string(inputs.ScopeHost)) != (sc == ScopeHost) {
					t.Errorf("mode %s scope %s: NODE_POWER_SCOPE %q", m, sc, cm["NODE_POWER_SCOPE"])
				}
			}
		}
	}
}

// The scopes a mode supports, and what each check compares with.
func TestTelemetryChecksPerModeAndScope(t *testing.T) {
	tm := testTimings(t)[0]
	for _, m := range []Mode{ModeKepler, ModeKeplerLegacy} {
		if _, err := Telemetry(m, ScopeHost, tm); err == nil {
			t.Errorf("mode %s accepts scope host", m)
		}
	}
	type want struct {
		label, joulie string
		ids           []string
	}
	for m, w := range map[Mode]want{
		ModeOTel:                 {"k8s_node_name", TruthPackagesGPUs, []string{"otel-components-labels", "otel-components-target-info", "otel-host-labels", "otel-host-target-info"}},
		ModeOTelUnpromoted:       {"k8s_node_name", TruthPackagesGPUs, []string{"otel-components-target-info", "otel-host-target-info"}},
		ModeOTelLabelsUnpromoted: {"k8s_node_name", "", []string{"otel-components-target-info", "otel-host-target-info"}},
		ModeOTelUTF8:             {"k8s.node.name", TruthPackagesGPUs, []string{"otel-components-labels", "otel-components-target-info", "otel-host-labels", "otel-host-target-info"}},
		ModeOTelCollector:        {"k8s_node_name", TruthPackagesGPUs, []string{"otel-components-labels", "otel-components-target-info", "otel-host-labels", "otel-host-target-info"}},
		ModeKepler:               {"node_name", TruthPackagesNVIDIAGPUs, []string{"kepler-0.12-components"}},
		ModeKeplerLegacy:         {"exported_instance", TruthPackagesNVIDIAGPUs, []string{"kepler-0.7-components", "kepler-0.7-platform-series", "kepler-0.7-platform-sum"}},
	} {
		set := testTelemetry(t, m, ScopeComponents, tm).Checks
		var ids []string
		for _, c := range set.Checks {
			ids = append(ids, c.ID)
			if _, err := EvalTruth(c.Truth, NodeTruth{}); err != nil {
				t.Errorf("mode %s check %s: %v", m, c.ID, err)
			}
			if !strings.Contains(c.Query, telLabel(set.NodeLabel)) {
				t.Errorf("mode %s check %s does not group by %s: %s", m, c.ID, set.NodeLabel, c.Query)
			}
		}
		if set.NodeLabel != w.label || set.JoulieTruth != w.joulie || !reflect.DeepEqual(ids, w.ids) {
			t.Errorf("mode %s: checks %+v, want label %s, Joulie truth %q, ids %v", m, set, w.label, w.joulie, w.ids)
		}
		if !telKepler(m) {
			host := testTelemetry(t, m, ScopeHost, tm).Checks
			if host.Checks[0].Truth != TruthHost || (m != ModeOTelLabelsUnpromoted && host.JoulieTruth != TruthHost) {
				t.Errorf("mode %s scope host: Joulie truth %q, first check %+v; want host first", m, host.JoulieTruth, host.Checks[0])
			}
		}
	}

	parts := testTelemetry(t, ModeOTel, ScopeComponents, tm)
	if _, err := parts.Part(PartOtelcolConfig); err == nil {
		t.Error("mode otel renders an otelcol-config")
	}
	if _, err := parts.Part("bogus"); err == nil {
		t.Error("an unknown part renders")
	}
	short := tm
	short.RateWindow = 15 * time.Second
	if _, err := Telemetry(ModeOTel, ScopeComponents, short); err == nil || !strings.Contains(err.Error(), "fewer than 4 OTLP export intervals") {
		t.Errorf("a rate window of 3 export intervals: error %v", err)
	}
	slowScrape := tm
	slowScrape.ScrapeInterval = 10 * time.Second
	if _, err := Telemetry(ModeKepler, ScopeComponents, slowScrape); err == nil || !strings.Contains(err.Error(), "scrape intervals") {
		t.Errorf("a rate window of 3 scrape intervals in mode kepler: error %v", err)
	}
	if _, err := Telemetry(ModeOTel, ScopeComponents, slowScrape); err != nil {
		t.Errorf("mode otel is not scraped, so the scrape interval does not bound its window: %v", err)
	}
}

// The wait set is every spec Node with a measured CPU package, which
// leaves out the VM.
func TestWaitNodes(t *testing.T) {
	got := WaitNodes(testExpand(t, testSmokeSpec))
	want := []string{"epyc-0", "hsmp-0", "intel-0", "intel-tainted-0", "mi300x-0", "nvl-0", "sxm-nopm-0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wait nodes %v, want %v", got, want)
	}
	if got := WaitNodes(nil); got == nil || len(got) != 0 {
		t.Errorf("wait nodes of no member %#v, want an empty list", got)
	}
}

// measuredCPU agrees with the emulator on every builtin profile: a profile
// has a measured CPU package exactly when hwemu's state has one.
func TestMeasuredCPUAgreesWithHwemu(t *testing.T) {
	profiles, err := hwemu.BuiltinProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range profiles {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tree, err := hwemu.Render(p, filepath.Join(t.TempDir(), "node"), hwemu.RenderOptions{NodeName: "simcluster-test"})
			if err != nil {
				t.Fatal(err)
			}
			n, err := hwemu.NewNode(p, tree, hwemu.Options{Start: time.Unix(1_700_000_000, 0)})
			if err != nil {
				t.Fatal(err)
			}
			if err := n.Step(time.Second); err != nil {
				t.Fatal(err)
			}
			want := false
			for _, c := range n.State().Components {
				want = want || (c.Type == semconv.HwTypeCPU && c.Measured)
			}
			if got := measuredCPU(p); got != want {
				t.Errorf("measuredCPU %v, hwemu has a measured cpu component: %v", got, want)
			}
		})
	}
}

// ParseMode and ParseScope round-trip every name, list the default first
// and reject others.
func TestParseModeAndScope(t *testing.T) {
	if Modes()[0] != ModeOTel || Scopes()[0] != ScopeComponents {
		t.Errorf("defaults %s and %s are not first", Modes()[0], Scopes()[0])
	}
	for _, m := range Modes() {
		if got, err := ParseMode(string(m)); err != nil || got != m {
			t.Errorf("ParseMode(%s) = %s, %v", m, got, err)
		}
	}
	for _, s := range Scopes() {
		if got, err := ParseScope(string(s)); err != nil || got != s {
			t.Errorf("ParseScope(%s) = %s, %v", s, got, err)
		}
	}
	if _, err := ParseMode("kepler-0.7"); err == nil || !strings.Contains(err.Error(), "otel, otel-unpromoted") {
		t.Errorf("ParseMode(kepler-0.7): %v, want an error listing the modes", err)
	}
	if _, err := ParseScope("node"); err == nil || !strings.Contains(err.Error(), "components, host") {
		t.Errorf("ParseScope(node): %v, want an error listing the scopes", err)
	}
}

// ReadTiming reads the committed timing files; ParseTiming refuses what
// the scripts would read wrongly.
func TestTiming(t *testing.T) {
	ts := testTimings(t)
	dev := Timing{Name: TimingDev, ControllerReconcile: 10 * time.Second, AgentReconcile: 5 * time.Second, SpecReadTimeout: time.Minute,
		ScrapeInterval: 5 * time.Second, OTLPExportInterval: 5 * time.Second, RateWindow: 30 * time.Second, FacilityPoll: 10 * time.Second}
	if ts[0] != dev {
		t.Errorf("dev timing %+v, want %+v", ts[0], dev)
	}
	if ts[1].OTLPExportInterval != time.Minute || ts[1].RateWindow != 5*time.Minute {
		t.Errorf("production timing %+v", ts[1])
	}
	if _, err := ReadTiming(testRepo, "../dev"); err == nil {
		t.Error("a timing name with a path is accepted")
	}
	if _, err := ReadTiming(testRepo, "missing"); err == nil {
		t.Error("a missing timing file is accepted")
	}

	good := "# comment\nCONTROLLER_RECONCILE_INTERVAL=10s\nAGENT_RECONCILE_INTERVAL='5s'\nAGENT_SPEC_READ_TIMEOUT=\"60s\"\n\nPROM_SCRAPE_INTERVAL=5s\nOTLP_EXPORT_INTERVAL_MS=5000\nRATE_WINDOW=30s\nFACILITY_POLL_INTERVAL=10s\n"
	got, err := ParseTiming(TimingDev, []byte(good))
	if err != nil || got != dev {
		t.Errorf("ParseTiming: %+v, %v; want %+v", got, err, dev)
	}
	for name, tc := range map[string]struct{ text, want string }{
		"missing":   {strings.Replace(good, "RATE_WINDOW=30s\n", "", 1), "RATE_WINDOW is not set"},
		"unknown":   {good + "SCRAPE=5s\n", "unknown key SCRAPE"},
		"twice":     {good + "RATE_WINDOW=1m\n", "RATE_WINDOW is set twice"},
		"duration":  {strings.Replace(good, "=10s\nAGENT", "=10\nAGENT", 1), "not a positive duration"},
		"ms":        {strings.Replace(good, "=5000", "=5s", 1), "not a positive number of milliseconds"},
		"line":      {good + "export X\n", "is not KEY=VALUE"},
		"window":    {strings.Replace(good, "RATE_WINDOW=30s", "RATE_WINDOW=15s", 1), "fewer than 4 OTLP export intervals"},
		"zero poll": {strings.Replace(good, "FACILITY_POLL_INTERVAL=10s", "FACILITY_POLL_INTERVAL=0s", 1), "not a positive duration"},
	} {
		if _, err := ParseTiming("x", []byte(tc.text)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one containing %q", name, err, tc.want)
		}
	}
}

// telDuration writes what Prometheus and Go both parse.
func TestTelDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Second: "5s", time.Minute: "1m", 90 * time.Second: "90s", 2 * time.Hour: "2h", 1500 * time.Millisecond: "1500ms"} {
		got := telDuration(d)
		if got != want {
			t.Errorf("telDuration(%s) = %s, want %s", d, got, want)
		}
		if back, err := time.ParseDuration(got); err != nil || back != d {
			t.Errorf("%s does not parse back to %s: %v", got, d, err)
		}
	}
	if !regexp.MustCompile(`^\d+(ms|s|m|h)$`).MatchString(telDuration(telOutOfOrderWindow)) {
		t.Errorf("out-of-order window %s", telDuration(telOutOfOrderWindow))
	}
}
