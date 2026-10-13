package scenario

import "github.com/matbun/joulie/pkg/otel/semconv"

// observePromQL reads the value the runner recorded for the assertion at
// the end of the run: the last joulie.scenario.assertion.value point of
// source observed with the assertion's name. The runner records a query
// only when it returned exactly one sample.
func observePromQL(ev *evaluation, a Assertion) map[string]observation {
	var last *Point
	for i := range ev.rd.Points {
		p := &ev.rd.Points[i]
		if p.Metric != semconv.JoulieScenarioAssertionValue.Name {
			continue
		}
		name, _ := p.Attributes.String(semconv.AttrJoulieAssertionName)
		src, _ := p.Attributes.String(semconv.AttrJoulieAssertionSource)
		if name == a.Name && src == semconv.AssertionSourceObserved && (last == nil || !p.Time.Before(last.Time)) {
			last = p
		}
	}
	if last == nil {
		return map[string]observation{"": noData("the run recorded no single-sample result of the query")}
	}
	return map[string]observation{"": observed(last.Value, "query %s at the end of the run", a.PromQL.Query)}
}
