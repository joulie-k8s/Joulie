package scenario

import (
	"fmt"
	"strings"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// Compare metric names: energy.total, class.<class>.energy,
// class.<class>.throughput and class.<class>.<latency>.<quantile>, where
// latency is one of the ClassSummary quantile sets and quantile one of its
// fields.
const (
	CompareEnergyTotal = "energy.total"
	compareClassPrefix = "class."
)

var (
	compareLatencies = []string{string(LatencyDuration), string(LatencyQueueWait), string(LatencySlowdown)}
	compareQuantiles = []string{"p50", "p95", "max"}
)

// ValidCompareMetric returns an error naming the grammar when metric is
// not a compare metric.
func ValidCompareMetric(metric string) error {
	if metric == CompareEnergyTotal {
		return nil
	}
	parts := strings.Split(metric, ".")
	ok := len(parts) >= 3 && parts[0] == "class" && oneOf(parts[1], jobClasses)
	switch {
	case ok && len(parts) == 3:
		ok = parts[2] == "energy" || parts[2] == "throughput"
	case ok && len(parts) == 4:
		ok = oneOf(parts[2], compareLatencies) && oneOf(parts[3], compareQuantiles)
	default:
		ok = false
	}
	if !ok {
		return fmt.Errorf("metric %q is not energy.total, class.<class>.energy, class.<class>.throughput or class.<class>.<%s>.<%s>",
			metric, strings.Join(compareLatencies, "|"), strings.Join(compareQuantiles, "|"))
	}
	return nil
}

// CompareValue reads metric from a variant's summary; ok is false when
// the summary does not hold it. An energy of 0 J is not held: the
// summary of a variant or class without joulie.sim.workload.energy series
// reads 0 J, and its ratio of 0 would pass any upper bound. Nor is the
// throughput of a class with no due job, or a latency of a class with no
// completed job.
func CompareValue(s *Summary, metric string) (float64, bool) {
	if s == nil {
		return 0, false
	}
	if metric == CompareEnergyTotal {
		if s.Energy == nil || s.Energy.TotalJoules == 0 {
			return 0, false
		}
		return s.Energy.TotalJoules, true
	}
	parts := strings.Split(metric, ".")
	if len(parts) < 3 {
		return 0, false
	}
	c, ok := s.Classes[parts[1]]
	if !ok {
		return 0, false
	}
	switch {
	case len(parts) == 3 && parts[2] == "energy":
		return c.EnergyJoules.Total, c.EnergyJoules.Total != 0
	case len(parts) == 3 && parts[2] == "throughput":
		return c.ThroughputJobsPerHour, c.Jobs > 0
	case len(parts) == 4:
		if c.Completed == 0 {
			return 0, false
		}
		var q Quantiles
		switch LatencyMetric(parts[2]) {
		case LatencyDuration:
			q = c.Duration
		case LatencyQueueWait:
			q = c.QueueWait
		case LatencySlowdown:
			q = c.Slowdown
		default:
			return 0, false
		}
		switch parts[3] {
		case "p50":
			return q.P50, true
		case "p95":
			return q.P95, true
		case "max":
			return q.Max, true
		}
	}
	return 0, false
}

// EvaluateComparisons evaluates each Compare of s against the variants'
// summaries, by variant name. The observed value is the variant's metric
// over the reference's; a missing summary, a missing value or a zero
// reference is no data, which fails.
func EvaluateComparisons(s Scenario, variants map[string]*Summary) []ComparisonResult {
	var out []ComparisonResult
	for _, c := range s.Compare {
		r := ComparisonResult{Name: c.Name, Metric: c.Metric, Variant: c.Variant, Reference: c.Reference,
			Severity: c.Severity, Expected: string(c.Expect), Status: semconv.TestCaseResultStatusFail}
		if r.Severity == "" {
			r.Severity = SeverityFail
		}
		v, okV := CompareValue(variants[c.Variant], c.Metric)
		ref, okR := CompareValue(variants[c.Reference], c.Metric)
		e, err := c.Expect.Parse()
		switch {
		case err != nil:
			r.Message = err.Error()
		case !okV || !okR:
			r.Message = fmt.Sprintf("%s: %s has no %s in its summary", NoData, missingSide(okV, c), c.Metric)
		case ref == 0:
			r.Message = fmt.Sprintf("%s: %s of %s is 0", NoData, c.Metric, c.Reference)
		default:
			ratio := v / ref
			r.Observed = &ratio
			r.Message = fmt.Sprintf("%s %s over %s %s", formatFloat(v), c.Variant, formatFloat(ref), c.Reference)
			if e.Holds(ratio) {
				r.Status = semconv.TestCaseResultStatusPass
			}
		}
		out = append(out, r)
	}
	return out
}

func missingSide(okVariant bool, c Compare) string {
	if !okVariant {
		return c.Variant
	}
	return c.Reference
}
