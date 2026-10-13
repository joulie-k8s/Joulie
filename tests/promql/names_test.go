package promql_test

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/prometheus/otlptranslator"
)

var update = flag.Bool("update", false, "rewrite "+tsvPath+" from otlptranslator")

// tsvPath is the golden table of package promname.
const tsvPath = "../../pkg/otel/promname/testdata/prometheus-names.tsv"

// translatorStrategies are the strategies in promname.Strategies order,
// which is the column order of the table.
var translatorStrategies = []otlptranslator.TranslationStrategyOption{
	otlptranslator.UnderscoreEscapingWithSuffixes,
	otlptranslator.NoUTF8EscapingWithSuffixes,
	otlptranslator.UnderscoreEscapingWithoutSuffixes,
	otlptranslator.NoTranslation,
}

// nameRow is a metric of the table: a semconv metric or an extra row.
type nameRow struct {
	name, unit string
	kind       semconv.Kind
}

// extraMetricRows exercise what no semconv metric does: target_info, the
// controller manager's own counter, the units of PN04 (joule and none),
// other units, suffixes the name already has, a leading digit, and names
// the translator rejects. The example.* and 2xx.* names are synthetic.
var extraMetricRows = []nameRow{
	{"target_info", "", semconv.Gauge},
	{"joulie.twin.input.queries", "{query}", semconv.Counter},
	{"hw.energy", "joule", semconv.Counter},
	{"hw.energy", "", semconv.Counter},
	{"example.duration", "s", semconv.Histogram},
	{"example.memory.usage", "By", semconv.UpDownCounter},
	{"example.throughput", "By/s", semconv.Gauge},
	{"example.transfer", "By/s", semconv.Counter},
	{"example.read_per_second", "By/s", semconv.Counter},
	{"example.requests_total", "{request}", semconv.Counter},
	{"example.utilization_ratio", "1", semconv.Gauge},
	{"example.errors", "1", semconv.Counter},
	{"example.voltage", "V", semconv.Gauge},
	{"2xx.responses", "{response}", semconv.Counter},
	{"_", "", semconv.Gauge},
	{"._", "", semconv.Gauge},
}

// extraLabelRows exercise label rules no semconv attribute does: a leading
// digit, reserved double underscores, runs of separators, a leading single
// underscore, and names the translator rejects.
var extraLabelRows = []string{
	"2xx.code", "__example.reserved__", "example..double__underscore",
	"_example.leading", "__example.leading", "_", "___", "",
}

func translatorType(k semconv.Kind) otlptranslator.MetricType {
	switch k {
	case semconv.Counter:
		return otlptranslator.MetricTypeMonotonicCounter
	case semconv.Gauge:
		return otlptranslator.MetricTypeGauge
	case semconv.UpDownCounter:
		return otlptranslator.MetricTypeNonMonotonicCounter
	case semconv.Histogram:
		return otlptranslator.MetricTypeHistogram
	}
	panic(fmt.Sprintf("no otlptranslator type for %v", k))
}

// translatorMetricName is the name otlptranslator gives r under s; a
// rejected name is "".
func translatorMetricName(r nameRow, s otlptranslator.TranslationStrategyOption) string {
	mn := otlptranslator.NewMetricNamer("", s)
	n, err := mn.Build(otlptranslator.Metric{Name: r.name, Unit: r.unit, Type: translatorType(r.kind)})
	if err != nil {
		return ""
	}
	return n
}

// translatorLabelName is the label name otlptranslator gives attr under s
// with the two settings Prometheus' DefaultOTLPConfig turns on.
func translatorLabelName(attr string, s otlptranslator.TranslationStrategyOption) string {
	ln := otlptranslator.LabelNamer{UTF8Allowed: !s.ShouldEscape(), UnderscoreLabelSanitization: true, PreserveMultipleUnderscores: true}
	n, err := ln.Build(attr)
	if err != nil {
		return ""
	}
	return n
}

func metricRows() []nameRow {
	var rows []nameRow
	for _, m := range semconv.Metrics() {
		rows = append(rows, nameRow{m.Name, m.Unit, m.Kind})
	}
	return append(rows, extraMetricRows...)
}

func labelRows() []string {
	return append(semconv.Attributes(), extraLabelRows...)
}

// generateTSV writes the golden table from otlptranslator alone.
func generateTSV() string {
	var b strings.Builder
	b.WriteString(`# Prometheus names of the semconv table under the four OTLP translation strategies, as Prometheus v3.15.0
# stores them with its default otlp settings, produced by github.com/prometheus/otlptranslator v1.0.0
# (prometheus v0.315.0 go.mod): MetricNamer.Build with NewMetricNamer("", strategy), and LabelNamer{UTF8Allowed:
# !strategy.ShouldEscape(), UnderscoreLabelSanitization: true, PreserveMultipleUnderscores: true}.Build, the two
# label settings Prometheus' DefaultOTLPConfig turns on (label_name_underscore_sanitization and
# label_name_preserve_multiple_underscores). An empty name cell is a name the translator rejects with an error.
# One row per semconv metric and attribute, plus rows that exercise units outside the table
# (joule, empty, s, By, By/s, V, annotations) and edge cases; example.*, 2xx.* and underscore names are synthetic.
# Columns, tab separated; unit and type are empty on label rows, and unit is empty for an empty unit:
`)
	names := make([]string, len(translatorStrategies))
	for i, s := range translatorStrategies {
		names[i] = string(s)
	}
	fmt.Fprintf(&b, "# row\tname\tunit\ttype\t%s\n", strings.Join(names, "\t"))
	for _, r := range metricRows() {
		out := make([]string, len(translatorStrategies))
		for i, s := range translatorStrategies {
			out[i] = translatorMetricName(r, s)
		}
		fmt.Fprintf(&b, "metric\t%s\t%s\t%s\t%s\n", r.name, r.unit, r.kind, strings.Join(out, "\t"))
	}
	for _, l := range labelRows() {
		out := make([]string, len(translatorStrategies))
		for i, s := range translatorStrategies {
			out[i] = translatorLabelName(l, s)
		}
		fmt.Fprintf(&b, "label\t%s\t\t\t%s\n", l, strings.Join(out, "\t"))
	}
	return b.String()
}

// TestPromNameAgreesWithOTLPTranslator: for every semconv metric and
// attribute, and every extra row, under all four strategies, promname gives
// the name otlptranslator v1.0.0 gives, and the golden table of package
// promname is what otlptranslator writes. A semconv entry without a row
// fails here and in promname's PN01 and PN02; -update rewrites the table,
// which is then reviewed like any golden.
func TestPromNameAgreesWithOTLPTranslator(t *testing.T) {
	if len(translatorStrategies) != len(promname.Strategies()) {
		t.Fatalf("%d translator strategies, promname has %d", len(translatorStrategies), len(promname.Strategies()))
	}
	for i, s := range promname.Strategies() {
		if string(s) != string(translatorStrategies[i]) {
			t.Fatalf("strategy %d: promname %q, otlptranslator %q", i, s, translatorStrategies[i])
		}
		if s.Escapes() != translatorStrategies[i].ShouldEscape() {
			t.Errorf("%s: promname Escapes %t, otlptranslator ShouldEscape %t", s, s.Escapes(), translatorStrategies[i].ShouldEscape())
		}
	}

	for _, r := range metricRows() {
		m := semconv.Metric{Name: r.name, Unit: r.unit, Kind: r.kind}
		for i, s := range promname.Strategies() {
			if got, want := s.MetricName(m), translatorMetricName(r, translatorStrategies[i]); got != want {
				t.Errorf("%s.MetricName(%q, unit %q, %s) = %q, otlptranslator %q", s, r.name, r.unit, r.kind, got, want)
			}
		}
	}
	for _, l := range labelRows() {
		for i, s := range promname.Strategies() {
			if got, want := s.LabelName(l), translatorLabelName(l, translatorStrategies[i]); got != want {
				t.Errorf("%s.LabelName(%q) = %q, otlptranslator %q", s, l, got, want)
			}
		}
	}

	got := generateTSV()
	if *update {
		if err := os.WriteFile(tsvPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(tsvPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) == got {
		return
	}
	wl, gl := strings.Split(string(want), "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			t.Fatalf("%s differs from otlptranslator at line %d:\notlptranslator: %q\n         table: %q\nRegenerate it with (cd tests/promql && go test -run TestPromNameAgreesWithOTLPTranslator -update) and review the diff.", tsvPath, i+1, g, w)
		}
	}
}
