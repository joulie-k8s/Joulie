package scenario

import "github.com/matbun/joulie/pkg/otel/semconv"

// latencyValues returns the metric of the completed jobs f selects. Slowdown
// is run time over ideal run time.
func latencyValues(ev *evaluation, f JobFilter, metric LatencyMetric) []float64 {
	var out []float64
	for _, j := range ev.jobs {
		if !j.Due || j.Outcome != semconv.JobOutcomeCompleted || !f.matches(j) {
			continue
		}
		var v *float64
		switch metric {
		case LatencyDuration:
			v = j.Duration
		case LatencyQueueWait:
			v = j.QueueWait
		case LatencyRunTime:
			v = j.RunTime
		case LatencySlowdown:
			if j.RunTime != nil && j.IdealRunTime != nil && *j.IdealRunTime > 0 {
				s := *j.RunTime / *j.IdealRunTime
				v = &s
			}
		}
		if v != nil {
			out = append(out, *v)
		}
	}
	return out
}

// observeLatency takes the quantile of the metric over the selected
// completed jobs, in seconds for a time, or its ratio to the same quantile
// over the reference jobs.
func observeLatency(ev *evaluation, a Assertion) map[string]observation {
	l := a.Latency
	q, err := ParseQuantile(l.Quantile)
	if err != nil {
		return map[string]observation{"": noData("%v", err)}
	}
	vals := latencyValues(ev, l.JobFilter, l.Metric)
	if len(vals) == 0 {
		return map[string]observation{"": noData("no completed job matches the filter")}
	}
	v := Quantile(vals, q)
	if l.Reference == nil {
		return map[string]observation{"": observed(v, "%s quantile %s over %d completed jobs", l.Metric, l.Quantile, len(vals))}
	}
	ref := latencyValues(ev, *l.Reference, l.Metric)
	if len(ref) == 0 {
		return map[string]observation{"": noData("no completed job matches the reference")}
	}
	rv := Quantile(ref, q)
	if rv == 0 {
		return map[string]observation{"": noData("the reference %s quantile is 0", l.Metric)}
	}
	return map[string]observation{"": observed(v/rv, "%s quantile %s: %s over %d jobs against %s over %d reference jobs",
		l.Metric, l.Quantile, formatFloat(v), len(vals), formatFloat(rv), len(ref))}
}
