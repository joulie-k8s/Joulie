package contracts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/otel/promname"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// The OpenTelemetry names the simulator emits and Joulie reads are spelled
// once, in pkg/otel/semconv, and their Prometheus spelling is computed by
// pkg/otel/promname. The simulator's metrics page is where an operator looks
// both up, to write a query or a dashboard. Every hw.* name is at Development
// stability upstream and may be renamed, so a name added to the table, or
// renamed there, without the page following would leave the page describing
// series that do not exist.

// otelDocsPage is the page that documents every name of the table.
const otelDocsPage = "website/content/en/docs/simulator/metrics.md"

// otelDocsSelfMetrics is the heading of the page's section on the
// simulator's own Prometheus series (joulie_sim_*), which are registered in
// simulator/cmd/simulator and not in pkg/otel/semconv.
const otelDocsSelfMetrics = "## Prometheus metrics at `/metrics`"

// otelDocsCodeSpan matches one inline code span of a Markdown line.
var otelDocsCodeSpan = regexp.MustCompile("`([^`]+)`")

// otelDocsName matches a code span that starts with a name of the table's two
// namespaces, dotted as OTel spells it (hw.energy) or underscored as
// Prometheus does (hw_energy_joules_total, hw_type), followed by nothing, an
// attribute value (=), a selector ({ or [) or a space. A span such as
// `joulie.io/rack` (a label key), `hw.host.*` or `joulie_sim_*` (a glob) is
// not one name and is left alone.
var otelDocsName = regexp.MustCompile(`^((?:hw|joulie)(?:[._][a-z0-9_]+)+)(?:$|[={\[ ])`)

// otelDocsTableRule matches the delimiter row under a table's header.
var otelDocsTableRule = regexp.MustCompile(`^\|(\s*:?-+:?\s*\|)+\s*$`)

// otelDocsTable is one Markdown table of the page: its header cells and its
// body rows.
type otelDocsTable struct {
	header []string
	rows   []otelDocsRow
}

// otelDocsRow is one body row and the 1-based line it is on.
type otelDocsRow struct {
	line  int
	cells []string
}

// column returns the index of the header cell that is exactly name, or -1.
func (tb otelDocsTable) column(name string) int {
	for i, h := range tb.header {
		if h == name {
			return i
		}
	}
	return -1
}

func (r otelDocsRow) cell(i int) string {
	if i < 0 || i >= len(r.cells) {
		return ""
	}
	return r.cells[i]
}

// otelDocsLines returns the page's lines with every fenced code block
// blanked: a backtick there is not a code span and a | is not a table.
func otelDocsLines(raw string) []string {
	lines := strings.Split(raw, "\n")
	fenced := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			lines[i] = ""
		} else if fenced {
			lines[i] = ""
		}
	}
	return lines
}

// otelDocsTables returns every table of the page, in order.
func otelDocsTables(lines []string) []otelDocsTable {
	var tables []otelDocsTable
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "|") || !otelDocsTableRule.MatchString(lines[i+1]) {
			continue
		}
		tb := otelDocsTable{header: otelDocsCells(lines[i])}
		j := i + 2
		for ; j < len(lines) && strings.HasPrefix(lines[j], "|"); j++ {
			tb.rows = append(tb.rows, otelDocsRow{line: j + 1, cells: otelDocsCells(lines[j])})
		}
		tables = append(tables, tb)
		i = j - 1
	}
	return tables
}

// otelDocsCells splits a table line into trimmed cells. As in GitHub
// Flavored Markdown, which Hugo renders, only an escaped \| stays inside a
// cell, code span or not.
func otelDocsCells(line string) []string {
	line = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "|"), "|")
	var cells []string
	start := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '|' && (i == 0 || line[i-1] != '\\') {
			cells = append(cells, strings.TrimSpace(line[start:i]))
			start = i + 1
		}
	}
	return append(cells, strings.TrimSpace(line[start:]))
}

// otelDocsSpans returns the contents of every code span of s.
func otelDocsSpans(s string) []string {
	var spans []string
	for _, m := range otelDocsCodeSpan.FindAllStringSubmatch(s, -1) {
		spans = append(spans, m[1])
	}
	return spans
}

// otelDocsExpand expands brace families: joulie.facility.it.{energy,power}
// becomes joulie.facility.it.energy and joulie.facility.it.power. Only a
// brace that follows a dot, an underscore or another family is one; a brace
// after a name is a selector, such as hw_energy_joules_total{hw_type="cpu"}.
func otelDocsExpand(s string) []string {
	for i := 1; i < len(s); i++ {
		if s[i] != '{' || !strings.ContainsRune("._}", rune(s[i-1])) {
			continue
		}
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			return []string{s}
		}
		var out []string
		for _, alt := range strings.Split(s[i+1:i+end], ",") {
			out = append(out, otelDocsExpand(s[:i]+alt+s[i+end+1:])...)
		}
		return out
	}
	return []string{s}
}

// otelDocsAttributeNames returns the sorted attribute names a cell lists: the
// dotted code spans, cut at an attribute value (hw.id=host is hw.id). Value
// spans such as `node_missing` are not names.
func otelDocsAttributeNames(cell string) []string {
	var names []string
	for _, s := range otelDocsSpans(cell) {
		name, _, _ := strings.Cut(s, "=")
		if strings.Contains(name, ".") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// TestCO03MetricsPageNamesEveryOTelName checks the page against the table in
// both directions.
//   - Every metric has exactly one row in a metrics table (header OTel name,
//     Unit, Instrument, Point attributes, Prometheus name) whose unit,
//     instrument, int value type, point attributes and Prometheus name under
//     the default translation strategy are those of pkg/otel/semconv and
//     pkg/otel/promname. A line elsewhere naming the metric does not count.
//   - Every event has exactly one row in an events table (header `eventName`,
//     Severity) with its severity. When the table has an Attributes column,
//     the row lists exactly the event's attributes, a cell that starts with
//     "the same" taking those of the row above as well.
//   - The strategy comparison table spells each name as promname does under
//     the strategy its column names.
//   - Every OTel or Prometheus name of the two namespaces the page spells in a
//     code span, brace families expanded, is in the table, so a renamed or
//     misspelled name cannot linger. The simulator's own joulie_sim_* series
//     are exempt in their section.
func TestCO03MetricsPageNamesEveryOTelName(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), otelDocsPage))
	if err != nil {
		t.Fatalf("read %s: %v", otelDocsPage, err)
	}
	lines := otelDocsLines(string(raw))
	tables := otelDocsTables(lines)

	metrics := map[string]semconv.Metric{}
	for _, m := range semconv.Metrics() {
		metrics[m.Name] = m
	}
	events := map[string]semconv.Event{}
	for _, e := range semconv.Events() {
		events[e.Name] = e
	}
	attributes := map[string]bool{}
	for _, a := range semconv.Attributes() {
		attributes[a] = true
	}

	metricRows := map[string][]int{}
	eventRows := map[string][]int{}
	for _, tb := range tables {
		name, unit, inst := tb.column("OTel name"), tb.column("Unit"), tb.column("Instrument")
		attrs, prom := tb.column("Point attributes"), tb.column("Prometheus name")
		if name >= 0 && unit >= 0 && inst >= 0 && attrs >= 0 && prom >= 0 {
			for _, r := range tb.rows {
				checkOTelDocsMetricRow(t, metrics, metricRows, r, name, unit, inst, attrs, prom)
			}
		}
		if sev := tb.column("Severity"); tb.column("`eventName`") == 0 && sev >= 0 {
			var above []string
			for _, r := range tb.rows {
				above = checkOTelDocsEventRow(t, events, eventRows, r, sev, tb.column("Attributes"), above)
			}
		}
		checkOTelDocsStrategyTable(t, metrics, attributes, tb)
	}
	for _, m := range semconv.Metrics() {
		switch rows := metricRows[m.Name]; len(rows) {
		case 0:
			t.Errorf("%s has no metrics table row for %s (%s, %s value). Fix: add | `%s` | %s | <instrument> | ... | `%s` | to a metrics table.",
				otelDocsPage, m.Name, m.Kind, m.Value, m.Name, m.Unit, promname.Default.MetricName(m))
		case 1:
		default:
			t.Errorf("%s has %d metrics table rows for %s, on lines %v. Fix: keep one.", otelDocsPage, len(rows), m.Name, rows)
		}
	}
	for _, e := range semconv.Events() {
		switch rows := eventRows[e.Name]; len(rows) {
		case 0:
			t.Errorf("%s has no events table row for %s (severity %d %s). Fix: add it to an events table.",
				otelDocsPage, e.Name, e.SeverityNumber, e.SeverityText)
		case 1:
		default:
			t.Errorf("%s has %d events table rows for %s, on lines %v. Fix: keep one.", otelDocsPage, len(rows), e.Name, rows)
		}
	}

	known := map[string]bool{}
	for _, m := range semconv.Metrics() {
		known[m.Name] = true
		for _, s := range promname.Strategies() {
			n := s.MetricName(m)
			known[n] = true
			if m.Kind == semconv.Histogram {
				known[n+"_bucket"], known[n+"_sum"], known[n+"_count"] = true, true, true
			}
		}
	}
	for _, e := range semconv.Events() {
		known[e.Name] = true
	}
	for _, a := range semconv.Attributes() {
		known[a] = true
		for _, s := range promname.Strategies() {
			known[s.LabelName(a)] = true
		}
	}
	section := ""
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimSpace(line)
		}
		for _, span := range otelDocsSpans(line) {
			for _, s := range otelDocsExpand(span) {
				m := otelDocsName.FindStringSubmatch(s)
				if m == nil || known[m[1]] {
					continue
				}
				if section == otelDocsSelfMetrics && strings.HasPrefix(m[1], "joulie_sim_") {
					continue
				}
				t.Errorf("%s:%d names %s, which is not a metric, event or attribute of pkg/otel/semconv, nor its "+
					"Prometheus spelling under any strategy. Fix: correct the name, register it in pkg/otel/semconv "+
					"first, or, for a simulator series of its own, list it under %q.", otelDocsPage, i+1, m[1], otelDocsSelfMetrics)
			}
		}
	}
}

// checkOTelDocsMetricRow checks one metrics table row against the metric its
// first cell names and records the row's line under that name.
func checkOTelDocsMetricRow(t *testing.T, metrics map[string]semconv.Metric, rows map[string][]int,
	r otelDocsRow, name, unit, inst, attrs, prom int) {
	t.Helper()
	where := fmt.Sprintf("%s:%d", otelDocsPage, r.line)
	spans := otelDocsSpans(r.cell(name))
	if len(spans) != 1 || r.cell(name) != "`"+spans[0]+"`" {
		t.Errorf("%s: the OTel name cell %q must be one code span. Fix: one metric per row.", where, r.cell(name))
		return
	}
	m, ok := metrics[spans[0]]
	if !ok {
		t.Errorf("%s: %s is not a metric of pkg/otel/semconv. Fix: correct the name, or register it first.", where, spans[0])
		return
	}
	rows[m.Name] = append(rows[m.Name], r.line)
	if got := strings.Trim(r.cell(unit), "`"); got != m.Unit {
		t.Errorf("%s: %s has unit %q, semconv says %q.", where, m.Name, got, m.Unit)
	}
	kind := r.cell(inst)
	if i := strings.IndexAny(kind, " ,("); i >= 0 {
		kind = kind[:i]
	}
	if !strings.EqualFold(kind, m.Kind.String()) {
		t.Errorf("%s: %s has instrument %q, semconv says %s.", where, m.Name, kind, m.Kind)
	}
	if isInt := strings.Contains(r.cell(inst), "(int)"); isInt != (m.Value == semconv.Int) {
		t.Errorf("%s: %s has value type %s in semconv. Fix: write \"(int)\" after the instrument exactly when it is int.",
			where, m.Name, m.Value)
	}
	if want := "`" + promname.Default.MetricName(m) + "`"; r.cell(prom) != want {
		t.Errorf("%s: %s has Prometheus name %s, promname says %s.", where, m.Name, r.cell(prom), want)
	}
	want := append([]string(nil), m.Attributes...)
	sort.Strings(want)
	if got := otelDocsAttributeNames(r.cell(attrs)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: %s lists point attributes %v, semconv says %v. Fix: list each one by name.", where, m.Name, got, want)
	}
}

// checkOTelDocsEventRow checks one events table row, which may name several
// events of one severity, and records the row's line under each name. With an
// attributes column (attrs >= 0) the row must list every attribute of each
// event and no other; a cell that starts with "the same" also lists above,
// the attributes of the row above. It returns the attributes the row lists.
func checkOTelDocsEventRow(t *testing.T, events map[string]semconv.Event, rows map[string][]int,
	r otelDocsRow, sev, attrs int, above []string) []string {
	t.Helper()
	where := fmt.Sprintf("%s:%d", otelDocsPage, r.line)
	listed := map[string]bool{}
	if strings.HasPrefix(r.cell(attrs), "the same") {
		for _, a := range above {
			listed[a] = true
		}
	}
	for _, a := range otelDocsAttributeNames(r.cell(attrs)) {
		listed[a] = true
	}
	var names []string
	for a := range listed {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, name := range otelDocsSpans(r.cell(0)) {
		e, ok := events[name]
		if !ok {
			t.Errorf("%s: %s is not an event of pkg/otel/semconv. Fix: correct the name, or register it first.", where, name)
			continue
		}
		rows[e.Name] = append(rows[e.Name], r.line)
		if want := fmt.Sprintf("%d %s", e.SeverityNumber, e.SeverityText); !strings.HasPrefix(r.cell(sev), want) {
			t.Errorf("%s: %s has severity %q, semconv says %s.", where, e.Name, r.cell(sev), want)
		}
		for _, a := range names {
			if !e.HasAttribute(a) {
				t.Errorf("%s: %s lists attribute %s, which semconv does not give the event.", where, e.Name, a)
			}
		}
		if attrs < 0 {
			continue
		}
		for _, a := range e.Attributes {
			if !listed[a] {
				t.Errorf("%s: %s does not list attribute %s, which semconv gives the event. Fix: list each one by name.",
					where, e.Name, a)
			}
		}
	}
	return names
}

// checkOTelDocsStrategyTable checks a table whose columns after the first are
// headed by translation strategies: each row's first code span is a metric
// or an attribute, and each column spells it as promname does under that
// strategy. A UTF-8 label may be quoted, as PromQL needs it. Any other table
// is left alone.
func checkOTelDocsStrategyTable(t *testing.T, metrics map[string]semconv.Metric, attributes map[string]bool, tb otelDocsTable) {
	t.Helper()
	if len(tb.header) < 2 {
		return
	}
	strategies := make([]promname.Strategy, len(tb.header))
	for k := 1; k < len(tb.header); k++ {
		spans := otelDocsSpans(tb.header[k])
		if len(spans) == 0 {
			return
		}
		st, err := promname.ParseStrategy(spans[0])
		if err != nil {
			return
		}
		strategies[k] = st
	}
	for _, r := range tb.rows {
		where := fmt.Sprintf("%s:%d", otelDocsPage, r.line)
		first := otelDocsSpans(r.cell(0))
		if len(first) == 0 {
			t.Errorf("%s: a strategy table row must start with a metric or attribute in a code span.", where)
			continue
		}
		m, isMetric := metrics[first[0]]
		if !isMetric && !attributes[first[0]] {
			t.Errorf("%s: %s is not a metric or attribute of pkg/otel/semconv.", where, first[0])
			continue
		}
		if isMetric && !strings.Contains(r.cell(0), fmt.Sprintf("(%s, %s)", m.Unit, m.Kind)) {
			t.Errorf("%s: %s must be annotated (%s, %s), its unit and instrument.", where, m.Name, m.Unit, m.Kind)
		}
		for k := 1; k < len(tb.header); k++ {
			want := strategies[k].LabelName(first[0])
			if isMetric {
				want = strategies[k].MetricName(m)
			}
			got := otelDocsSpans(r.cell(k))
			if len(got) != 1 || (got[0] != want && got[0] != `"`+want+`"`) {
				t.Errorf("%s: %s under %s is %q, promname says `%s`.", where, first[0], strategies[k], r.cell(k), want)
			}
		}
	}
}
