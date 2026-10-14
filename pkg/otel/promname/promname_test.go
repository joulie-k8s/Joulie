package promname

import (
	"bufio"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

const goldenPath = "testdata/prometheus-names.tsv"

// goldenRow is one row of the golden table: a metric (name, unit, kind) or
// a label (name), and the name otlptranslator v1.0.0 gave it under each
// strategy, in Strategies() order.
type goldenRow struct {
	row, name, unit, kind string
	want                  []string
	line                  int
}

// loadGolden reads the golden table. Its column header must list the
// strategies in Strategies() order.
func loadGolden(t *testing.T) []goldenRow {
	t.Helper()
	f, err := os.Open(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	header := "# row\tname\tunit\ttype"
	for _, s := range Strategies() {
		header += "\t" + string(s)
	}
	var rows []goldenRow
	sawHeader := false
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			sawHeader = sawHeader || line == header
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4+len(Strategies()) {
			t.Fatalf("%s:%d: %d fields, want %d", goldenPath, n, len(fields), 4+len(Strategies()))
		}
		rows = append(rows, goldenRow{row: fields[0], name: fields[1], unit: fields[2], kind: fields[3], want: fields[4:], line: n})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if !sawHeader {
		t.Fatalf("%s: no column header %q", goldenPath, header)
	}
	return rows
}

// kindOf is the semconv Kind whose String is s.
func kindOf(t *testing.T, s string) semconv.Kind {
	t.Helper()
	for _, k := range []semconv.Kind{semconv.Counter, semconv.Gauge, semconv.UpDownCounter, semconv.Histogram} {
		if k.String() == s {
			return k
		}
	}
	t.Fatalf("unknown kind %q", s)
	return 0
}

func metricKey(name, unit, kind string) string { return name + "\t" + unit + "\t" + kind }

// suggestedRow is the golden row promname computes for a metric or label the
// table lacks, for the failure message. tests/promql checks it against
// otlptranslator.
func suggestedRow(row, name, unit, kind string, names func(Strategy) string) string {
	out := []string{row, name, unit, kind}
	for _, s := range Strategies() {
		out = append(out, names(s))
	}
	return strings.Join(out, "\t")
}

// PN01: MetricName equals the golden table, which otlptranslator v1.0.0
// produced, for every registered semconv metric and every extra unit row,
// under all four strategies.
func TestPN01MetricNameMatchesGolden(t *testing.T) {
	rows := loadGolden(t)
	byKey := map[string]goldenRow{}
	for _, r := range rows {
		if r.row != "metric" {
			continue
		}
		k := metricKey(r.name, r.unit, r.kind)
		if _, dup := byKey[k]; dup {
			t.Errorf("%s:%d: metric %q unit %q kind %s listed twice", goldenPath, r.line, r.name, r.unit, r.kind)
		}
		byKey[k] = r
	}

	for _, m := range semconv.Metrics() {
		if _, ok := byKey[metricKey(m.Name, m.Unit, m.Kind.String())]; !ok {
			m := m
			t.Errorf("semconv metric %q (unit %q, %s) has no row in %s; add this row and check it with tests/promql:\n%s",
				m.Name, m.Unit, m.Kind, goldenPath, suggestedRow("metric", m.Name, m.Unit, m.Kind.String(), func(s Strategy) string { return s.MetricName(m) }))
		}
	}

	for _, r := range rows {
		if r.row != "metric" {
			continue
		}
		m := semconv.Metric{Name: r.name, Unit: r.unit, Kind: kindOf(t, r.kind)}
		for i, s := range Strategies() {
			if got := s.MetricName(m); got != r.want[i] {
				t.Errorf("%s:%d: %s.MetricName(%q, unit %q, %s) = %q, want %q", goldenPath, r.line, s, r.name, r.unit, r.kind, got, r.want[i])
			}
		}
	}
}

// PN02: LabelName equals the golden table for every registered attribute and
// every extra label row, under all four strategies.
func TestPN02LabelNameMatchesGolden(t *testing.T) {
	rows := loadGolden(t)
	byName := map[string]goldenRow{}
	for _, r := range rows {
		if r.row != "label" {
			continue
		}
		if r.unit != "" || r.kind != "" {
			t.Errorf("%s:%d: label %q has a unit or type", goldenPath, r.line, r.name)
		}
		if _, dup := byName[r.name]; dup {
			t.Errorf("%s:%d: label %q listed twice", goldenPath, r.line, r.name)
		}
		byName[r.name] = r
	}

	for _, a := range semconv.Attributes() {
		if _, ok := byName[a]; !ok {
			a := a
			t.Errorf("semconv attribute %q has no row in %s; add this row and check it with tests/promql:\n%s",
				a, goldenPath, suggestedRow("label", a, "", "", func(s Strategy) string { return s.LabelName(a) }))
		}
	}

	for _, r := range rows {
		if r.row != "label" {
			continue
		}
		for i, s := range Strategies() {
			if got := s.LabelName(r.name); got != r.want[i] {
				t.Errorf("%s:%d: %s.LabelName(%q) = %q, want %q", goldenPath, r.line, s, r.name, got, r.want[i])
			}
		}
	}
}

// PN03: ParseStrategy accepts the four strategy names in any case and
// returns the canonical spelling; it rejects every other string, the empty
// one included. Only the two Underscore strategies escape.
func TestPN03ParseStrategyIgnoresCaseAndRejectsOthers(t *testing.T) {
	escapes := map[Strategy]bool{
		UnderscoreEscapingWithSuffixes:    true,
		NoUTF8EscapingWithSuffixes:        false,
		UnderscoreEscapingWithoutSuffixes: true,
		NoTranslation:                     false,
	}
	if len(Strategies()) != len(escapes) {
		t.Fatalf("Strategies() = %v, want the 4 strategies", Strategies())
	}
	if Strategies()[0] != Default || Default != UnderscoreEscapingWithSuffixes {
		t.Errorf("Default = %q, first of Strategies() = %q, want UnderscoreEscapingWithSuffixes for both", Default, Strategies()[0])
	}
	for _, s := range Strategies() {
		want, ok := escapes[s]
		if !ok {
			t.Errorf("unexpected strategy %q", s)
			continue
		}
		if s.Escapes() != want {
			t.Errorf("%s.Escapes() = %v, want %v", s, s.Escapes(), want)
		}
		for _, in := range []string{string(s), strings.ToLower(string(s)), strings.ToUpper(string(s)), swapCase(string(s))} {
			got, err := ParseStrategy(in)
			if err != nil || got != s {
				t.Errorf("ParseStrategy(%q) = %q, %v, want %q", in, got, err, s)
			}
		}
	}
	for _, in := range []string{"", " NoTranslation", "NoTranslation ", "UnderscoreEscaping", "underscore_escaping_with_suffixes", "NoUTF8Escaping", "Legacy", "utf8"} {
		if got, err := ParseStrategy(in); err == nil {
			t.Errorf("ParseStrategy(%q) = %q, want an error", in, got)
		} else if !strings.Contains(err.Error(), string(UnderscoreEscapingWithSuffixes)) {
			t.Errorf("ParseStrategy(%q) error %q does not list the valid strategies", in, err)
		}
	}
}

func swapCase(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		if r >= 'A' && r <= 'Z' {
			return r - 'A' + 'a'
		}
		return r
	}, s)
}

// PN04: a unit outside the table changes the suffix. A producer that writes
// hw.energy with unit joule, or with no unit, lands in a series the default
// queries never select.
func TestPN04NonStandardUnitChangesTheSuffix(t *testing.T) {
	for _, c := range []struct{ unit, want string }{
		{"J", "hw_energy_joules_total"},
		{"joule", "hw_energy_joule_total"},
		{"", "hw_energy_total"},
	} {
		m := semconv.HwEnergy
		m.Unit = c.unit
		if got := UnderscoreEscapingWithSuffixes.MetricName(m); got != c.want {
			t.Errorf("MetricName(hw.energy, unit %q) = %q, want %q", c.unit, got, c.want)
		}
	}
}

// The zero Strategy, and any value outside the four, spells every name as
// Prometheus' default does, which is what Prometheus applies when
// otlp.translation_strategy is absent.
func TestZeroStrategyIsPrometheusDefault(t *testing.T) {
	for _, s := range []Strategy{"", "Unknown"} {
		if !s.Escapes() {
			t.Errorf("Strategy(%q).Escapes() = false, want true", s)
		}
		for _, m := range semconv.Metrics() {
			if got, want := s.MetricName(m), Default.MetricName(m); got != want {
				t.Errorf("Strategy(%q).MetricName(%q) = %q, want %q", s, m.Name, got, want)
			}
		}
		for _, a := range semconv.Attributes() {
			if got, want := s.LabelName(a), Default.LabelName(a); got != want {
				t.Errorf("Strategy(%q).LabelName(%q) = %q, want %q", s, a, got, want)
			}
		}
	}
}

// The inputs the controller manager reads are registered metrics, and under
// the default strategy they carry the names and grouping labels of the
// golden queries Joulie runs by default. A change here changes those
// queries.
func TestInputMetricsHaveTheGoldenQueryNames(t *testing.T) {
	registered := map[string]semconv.Metric{}
	for _, m := range semconv.Metrics() {
		registered[m.Name] = m
	}
	for _, c := range []struct {
		set  string
		got  []semconv.Metric
		want []string
	}{
		{"NodeInputs", semconv.NodeInputs(), []string{
			"hw_energy_joules_total", "hw_power_watts", "hw_host_energy_joules_total", "hw_host_power_watts",
		}},
		{"FacilityInputs", semconv.FacilityInputs(), []string{
			"joulie_facility_ambient_temperature_celsius",
			"joulie_facility_it_energy_joules_total", "joulie_facility_it_power_watts",
			"joulie_facility_cooling_energy_joules_total", "joulie_facility_cooling_power_watts",
			"joulie_cooling_zone_ambient_temperature_celsius",
			"joulie_rack_energy_joules_total", "joulie_rack_power_watts",
			"hw_host_ambient_temperature_celsius",
		}},
	} {
		var names []string
		for _, m := range c.got {
			names = append(names, Default.MetricName(m))
			if r, ok := registered[m.Name]; !ok || !reflect.DeepEqual(r, m) {
				t.Errorf("%s: %q is not the registered metric %+v", c.set, m.Name, r)
			}
		}
		if !reflect.DeepEqual(names, c.want) {
			t.Errorf("%s default names = %v, want %v", c.set, names, c.want)
		}
	}

	labels := map[string]string{
		semconv.AttrK8sNodeName: "k8s_node_name", semconv.AttrHwType: "hw_type", semconv.AttrHwID: "hw_id",
		semconv.AttrJoulieCoolingZoneName: "joulie_cooling_zone_name", semconv.AttrJoulieRackName: "joulie_rack_name",
	}
	for attr, want := range labels {
		if got := Default.LabelName(attr); got != want {
			t.Errorf("LabelName(%q) = %q, want %q", attr, got, want)
		}
	}

	// Each call returns its own Attributes: a caller's write reaches neither
	// a later call nor the package variables the sets are built from.
	for _, c := range []struct {
		set string
		get func() []semconv.Metric
	}{{"NodeInputs", semconv.NodeInputs}, {"FacilityInputs", semconv.FacilityInputs}} {
		got := c.get()
		var restore []func()
		for i := range got {
			if len(got[i].Attributes) == 0 {
				continue
			}
			attrs, orig := got[i].Attributes, got[i].Attributes[0]
			attrs[0] = "changed"
			restore = append(restore, func() { attrs[0] = orig })
		}
		if len(restore) == 0 {
			t.Errorf("%s: no metric with attributes to write to", c.set)
		}
		for _, m := range c.get() {
			if !reflect.DeepEqual(m, registered[m.Name]) {
				t.Errorf("%s shares its Attributes with the table: after a caller's write, %q is %+v", c.set, m.Name, m)
			}
		}
		for _, r := range restore {
			r()
		}
	}
}
