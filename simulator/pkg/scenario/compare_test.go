package scenario_test

import (
	"math"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/scenario"
)

func compareSummary(energy, perfP95 float64, completed int) *scenario.Summary {
	s := scenario.NewSummary("x")
	s.Energy = &scenario.EnergySummary{TotalJoules: energy}
	s.Classes = map[string]scenario.ClassSummary{"performance": {Jobs: completed, Completed: completed, Duration: scenario.Quantiles{P95: perfP95},
		EnergyJoules: scenario.EnergyJoules{Total: energy / 2}, ThroughputJobsPerHour: 10}}
	return &s
}

// A comparison is the variant's metric over the reference's; a summary
// without the metric, or a zero reference, is no data and fails.
func TestCompareAcrossVariants(t *testing.T) {
	s := fxDecode(t, `
name: c
variants: [{name: "off"}, {name: aware}]
compare:
- {name: energy, metric: energy.total, variant: aware, reference: "off", expect: "<= 1.0", severity: warn}
- {name: perf, metric: class.performance.duration.p95, variant: aware, reference: "off", expect: "<= 1.10"}
- {name: thr, metric: class.performance.throughput, variant: aware, reference: "off", expect: "== 1"}
- {name: std, metric: class.standard.energy, variant: aware, reference: "off", expect: "<= 1"}
`)
	summaries := map[string]*scenario.Summary{"off": compareSummary(1000, 100, 5), "aware": compareSummary(900, 120, 5)}
	got := map[string]scenario.ComparisonResult{}
	for _, r := range scenario.EvaluateComparisons(s, summaries) {
		got[r.Name] = r
	}
	if r := got["energy"]; r.Status != semconv.TestCaseResultStatusPass || math.Abs(*r.Observed-0.9) > 1e-12 || r.Severity != scenario.SeverityWarn {
		t.Errorf("energy %+v", r)
	}
	if r := got["perf"]; r.Status != semconv.TestCaseResultStatusFail || math.Abs(*r.Observed-1.2) > 1e-12 || r.Severity != scenario.SeverityFail {
		t.Errorf("perf %+v", r)
	}
	if r := got["thr"]; r.Status != semconv.TestCaseResultStatusPass {
		t.Errorf("thr %+v", r)
	}
	if r := got["std"]; r.Status != semconv.TestCaseResultStatusFail || r.Observed != nil {
		t.Errorf("std (no standard class) %+v", r)
	}
	top := scenario.TopSummary(s, summaries)
	top.Finish(nil)
	if top.ExitCode != scenario.ExitFail || len(top.Variants) != 2 || top.Variants[1].Summary != "aware/summary.json" || len(top.Warnings) != 0 {
		t.Errorf("top %+v", top)
	}
	delete(summaries, "aware")
	top = scenario.TopSummary(s, summaries)
	top.Finish(nil)
	if top.ExitCode != scenario.ExitError || top.Variants[1].ExitCode != scenario.ExitError {
		t.Errorf("a variant without a summary: top %+v", top)
	}
}

// A variant without joulie.sim.workload.energy series reads 0 J, and a
// class with no due job reads a throughput of 0: each is no data, since
// its ratio of 0 would otherwise pass "<= 1".
func TestCompareWithoutDataFails(t *testing.T) {
	s := fxDecode(t, `
name: c
variants: [{name: "off"}, {name: aware}, {name: early}]
compare:
- {name: energy, metric: energy.total, variant: aware, reference: "off", expect: "<= 1.0"}
- {name: perf-energy, metric: class.performance.energy, variant: aware, reference: "off", expect: "<= 1.0"}
- {name: perf-thr, metric: class.performance.throughput, variant: aware, reference: "off", expect: "== 1"}
- {name: early-thr, metric: class.performance.throughput, variant: early, reference: "off", expect: "<= 1.0"}
`)
	rd, env := fxRunData(t)
	off := scenario.VariantSummary(rd, env, nil)
	noEnergy := *rd
	noEnergy.Points = nil
	aware := scenario.VariantSummary(&noEnergy, env, nil)
	early := scenario.NewSummary("c/early")
	early.Classes = map[string]scenario.ClassSummary{"performance": {EnergyJoules: scenario.EnergyJoules{Total: 5000}}}
	got := map[string]scenario.ComparisonResult{}
	for _, r := range scenario.EvaluateComparisons(s, map[string]*scenario.Summary{"off": &off, "aware": &aware, "early": &early}) {
		got[r.Name] = r
	}
	for _, name := range []string{"energy", "perf-energy", "early-thr"} {
		if r := got[name]; r.Status != semconv.TestCaseResultStatusFail || r.Observed != nil || !strings.HasPrefix(r.Message, scenario.NoData) {
			t.Errorf("%s: %s observed %v (%s), want fail with no data", name, r.Status, r.Observed, r.Message)
		}
	}
	if r := got["perf-thr"]; r.Status != semconv.TestCaseResultStatusPass {
		t.Errorf("perf-thr %+v: a class with due jobs still compares", r)
	}
}
