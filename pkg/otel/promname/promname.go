// Package promname computes the Prometheus spelling of the OpenTelemetry
// names in package semconv: the metric and label names a Prometheus OTLP
// receiver stores under each of its four otlp.translation_strategy values.
// The controller manager builds its PromQL from these names, and the
// simulator's harness checks its queries against them, so no Prometheus name
// of a semconv entry is written by hand.
//
// The rules reproduce github.com/prometheus/otlptranslator v1.0.0, the
// translator of Prometheus v3.15.0 (prometheus v0.315.0 go.mod), with the
// standard library only: the unit tables, the metric name normalisation and
// the label sanitisation follow its metric_namer.go, unit_namer.go,
// label_namer.go and strconv.go (Apache-2.0, Copyright The Prometheus
// Authors). Label names follow the two LabelNamer settings that Prometheus
// turns on by default (DefaultOTLPConfig in its config.go), the otlp keys
// label_name_underscore_sanitization and
// label_name_preserve_multiple_underscores. A receiver that sets either to
// false stores another name for an attribute holding a run of characters
// outside [a-zA-Z0-9], or exactly one at its start; no semconv attribute
// does. testdata/prometheus-names.tsv holds the names otlptranslator v1.0.0
// produced with those settings for every semconv entry, a few extra units
// and edge cases; the package tests pin this code to it, and tests/promql
// compares both with the translator itself.
package promname

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// Strategy is a Prometheus OTLP translation strategy, spelled as Prometheus
// spells it in otlp.translation_strategy. A Strategy outside the four,
// including the zero value, behaves as UnderscoreEscapingWithSuffixes,
// which is also what Prometheus uses when the setting is absent.
type Strategy string

const (
	// UnderscoreEscapingWithSuffixes replaces every character outside
	// [a-zA-Z0-9:] with an underscore and appends the unit and type
	// suffixes: hw.energy in J becomes hw_energy_joules_total.
	UnderscoreEscapingWithSuffixes Strategy = "UnderscoreEscapingWithSuffixes"
	// NoUTF8EscapingWithSuffixes keeps the name and appends the suffixes:
	// hw.energy_joules_total. PromQL must quote such names.
	NoUTF8EscapingWithSuffixes Strategy = "NoUTF8EscapingWithSuffixes"
	// UnderscoreEscapingWithoutSuffixes escapes as the default does and
	// appends nothing: hw_energy.
	UnderscoreEscapingWithoutSuffixes Strategy = "UnderscoreEscapingWithoutSuffixes"
	// NoTranslation keeps every name as it is: hw.energy. Prometheus marks
	// it experimental.
	NoTranslation Strategy = "NoTranslation"
)

// Default is the strategy Prometheus applies when none is configured.
const Default = UnderscoreEscapingWithSuffixes

// TargetInfo is the metric that carries a resource's attributes when the
// receiver does not promote them onto the data series. Its name is the same
// under every strategy.
const TargetInfo = "target_info"

// Labels the receiver derives from the resource of every series: job is
// service.namespace/service.name (or service.name alone), instance is
// service.instance.id. A join with TargetInfo matches on both.
const (
	LabelJob      = "job"
	LabelInstance = "instance"
)

// Strategies returns the four strategies, the default first.
func Strategies() []Strategy {
	return []Strategy{UnderscoreEscapingWithSuffixes, NoUTF8EscapingWithSuffixes, UnderscoreEscapingWithoutSuffixes, NoTranslation}
}

// ParseStrategy returns the strategy s names, ignoring case. Any other
// string, the empty one included, is an error: a caller that wants the
// default for an unset value applies Default itself.
func ParseStrategy(s string) (Strategy, error) {
	for _, st := range Strategies() {
		if strings.EqualFold(s, string(st)) {
			return st, nil
		}
	}
	names := make([]string, 0, 4)
	for _, st := range Strategies() {
		names = append(names, string(st))
	}
	return "", fmt.Errorf("promname: unknown translation strategy %q, want one of %s", s, strings.Join(names, ", "))
}

// Escapes reports whether s rewrites names to the legacy Prometheus
// character set. When it is false, metric and label names keep their dots,
// and a PromQL selector must quote them, as in {"<name>"}.
func (s Strategy) Escapes() bool {
	switch s {
	case NoUTF8EscapingWithSuffixes, NoTranslation:
		return false
	}
	return true
}

// suffixes reports whether s appends unit and type suffixes.
func (s Strategy) suffixes() bool {
	switch s {
	case UnderscoreEscapingWithoutSuffixes, NoTranslation:
		return false
	}
	return true
}

// MetricName returns the name Prometheus stores for m under s. With
// suffixes, the unit becomes a word (J joules, W watts, Cel celsius, Hz
// hertz, By bytes, s seconds; a unit after a slash becomes per_<unit>), a
// unit of 1 adds _ratio to a gauge, an annotation such as {job} adds
// nothing, a unit outside the table is used as it is (joule stays joule),
// and a Counter, the monotonic Sum, ends in _total. A histogram's name is
// the base of its _bucket, _sum and _count series.
//
// It returns the empty string for a name the translator rejects, one that
// escapes to nothing or to underscores only; no semconv name does.
func (s Strategy) MetricName(m semconv.Metric) string {
	counter := m.Kind == semconv.Counter
	gauge := m.Kind == semconv.Gauge
	if !s.Escapes() {
		if !s.suffixes() {
			return m.Name
		}
		return utf8MetricName(m.Name, m.Unit, counter, gauge)
	}
	var out string
	if s.suffixes() {
		out = escapedMetricName(m.Name, m.Unit, counter, gauge)
	} else {
		out = strings.Join(strings.FieldsFunc(m.Name, func(r rune) bool {
			return !compliantMetricChar(r) && r != '_'
		}), "_")
		if out != "" && unicode.IsDigit(rune(out[0])) {
			out = "_" + out
		}
	}
	if out == "" || (out != m.Name && strings.Trim(out, "_") == "") {
		return ""
	}
	return out
}

// LabelName returns the label name Prometheus v3.15.0 stores for attribute
// attr under s with its default otlp settings: unchanged when s does not
// escape, otherwise with every character outside [a-zA-Z0-9] replaced by an
// underscore, runs kept (k8s.node.name becomes k8s_node_name, a..b becomes
// a__b), key_ prepended to a leading digit and key to a leading single
// underscore (_x becomes key_x; __x stays). It returns the empty string for
// a name the translator rejects: an empty one, or one that is, or escapes
// to, underscores only.
func (s Strategy) LabelName(attr string) string {
	if attr == "" {
		return ""
	}
	if !s.Escapes() {
		if strings.Trim(attr, "_") == "" {
			return ""
		}
		return attr
	}
	out := sanitizeLabelName(attr)
	if unicode.IsDigit(rune(out[0])) {
		out = "key_" + out
	} else if strings.HasPrefix(out, "_") && !strings.HasPrefix(out, "__") {
		out = "key" + out
	}
	if strings.Trim(out, "_") == "" {
		return ""
	}
	return out
}

// unitWords translates a UCUM unit into the word Prometheus appends
// (otlptranslator unitMap). A unit outside the table is used as it is.
var unitWords = map[string]string{
	"d": "days", "h": "hours", "min": "minutes", "s": "seconds",
	"ms": "milliseconds", "us": "microseconds", "ns": "nanoseconds",

	"By": "bytes", "KiBy": "kibibytes", "MiBy": "mebibytes", "GiBy": "gibibytes", "TiBy": "tibibytes",
	"KBy": "kilobytes", "MBy": "megabytes", "GBy": "gigabytes", "TBy": "terabytes",

	"m": "meters", "V": "volts", "A": "amperes", "J": "joules", "W": "watts", "g": "grams",

	"Cel": "celsius", "Hz": "hertz", "1": "", "%": "percent",
}

// perUnitWords translates the unit after a slash (otlptranslator
// perUnitMap): s gives per_second.
var perUnitWords = map[string]string{
	"s": "second", "m": "minute", "h": "hour", "d": "day", "w": "week", "mo": "month", "y": "year",
}

// unitSuffixes splits unit at its first slash and returns the main and the
// per suffix. A part that is blank or holds braces adds nothing.
func unitSuffixes(unit string) (main, per string) {
	parts := strings.SplitN(unit, "/", 2)
	if u := strings.TrimSpace(parts[0]); u != "" && !strings.ContainsAny(u, "{}") {
		main = u
		if w, ok := unitWords[u]; ok {
			main = w
		}
	}
	if len(parts) > 1 && parts[1] != "" {
		if u := strings.TrimSpace(parts[1]); u != "" && !strings.ContainsAny(u, "{}") {
			per = u
			if w, ok := perUnitWords[u]; ok {
				per = w
			}
		}
		if per != "" {
			per = "per_" + per
		}
	}
	return main, per
}

// utf8MetricName appends the suffixes to a name kept as it is. Each suffix
// already at the end of the name is moved, not repeated.
func utf8MetricName(name, unit string, counter, gauge bool) string {
	main, per := unitSuffixes(unit)
	ratio := unit == "1" && gauge
	if ratio {
		name = trimSuffixAndDelimiter(name, "ratio")
	}
	if counter {
		name = trimSuffixAndDelimiter(name, "total")
	}
	if per != "" {
		name = trimSuffixAndDelimiter(name, per)
	}
	if main != "" && !strings.HasSuffix(name, main) {
		name += "_" + main
	}
	if per != "" {
		name += "_" + per
	}
	if counter {
		name += "_total"
	}
	if ratio {
		name += "_ratio"
	}
	return name
}

// escapedMetricName splits the name into tokens of [a-zA-Z0-9:], adds the
// unit tokens the name lacks, moves total and ratio to the end, and joins
// the tokens with underscores.
func escapedMetricName(name, unit string, counter, gauge bool) string {
	tokens := strings.FieldsFunc(name, func(r rune) bool { return !compliantMetricChar(r) })
	main, per := unitSuffixes(unit)
	tokens = addUnitTokens(tokens, cleanUpUnit(main), cleanUpUnit(per))
	if counter {
		tokens = append(removeToken(tokens, "total"), "total")
	}
	if unit == "1" && gauge {
		tokens = append(removeToken(tokens, "ratio"), "ratio")
	}
	out := strings.Join(tokens, "_")
	if out != "" && unicode.IsDigit(rune(out[0])) {
		out = "_" + out
	}
	return out
}

func addUnitTokens(tokens []string, main, per string) []string {
	if containsToken(tokens, main) {
		main = ""
	}
	if per == "per_" {
		per = ""
	} else {
		per = strings.TrimSuffix(per, "_")
		if containsToken(tokens, per) {
			per = ""
		}
	}
	if per != "" {
		main = strings.TrimSuffix(main, "_")
	}
	if main != "" {
		tokens = append(tokens, main)
	}
	if per != "" {
		tokens = append(tokens, per)
	}
	return tokens
}

func containsToken(tokens []string, v string) bool {
	for _, t := range tokens {
		if t == v {
			return true
		}
	}
	return false
}

func removeToken(tokens []string, v string) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if t != v {
			out = append(out, t)
		}
	}
	return out
}

// trimSuffixAndDelimiter removes suffix and the one character before it,
// when the name is longer than both.
func trimSuffixAndDelimiter(name, suffix string) string {
	if strings.HasSuffix(name, suffix) && len(name) > len(suffix)+1 {
		return name[:len(name)-(len(suffix)+1)]
	}
	return name
}

// cleanUpUnit escapes a unit word for a metric name: invalid characters
// become underscores, runs of them collapse, and a leading one is dropped.
func cleanUpUnit(unit string) string {
	escaped := strings.Map(func(r rune) rune {
		if compliantMetricChar(r) {
			return r
		}
		return '_'
	}, unit)
	return strings.TrimPrefix(collapseUnderscores(escaped), "_")
}

func collapseUnderscores(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prev := false
	for _, r := range s {
		if r == '_' {
			if !prev {
				b.WriteRune(r)
			}
			prev = true
			continue
		}
		b.WriteRune(r)
		prev = false
	}
	return b.String()
}

// sanitizeLabelName replaces every character outside [a-zA-Z0-9],
// underscores included, with one underscore each, so runs are kept and a
// reserved name __x__ keeps its double underscores.
func sanitizeLabelName(name string) string {
	return strings.Map(func(r rune) rune {
		if compliantLabelChar(r) {
			return r
		}
		return '_'
	}, name)
}

func compliantMetricChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == ':'
}

func compliantLabelChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
