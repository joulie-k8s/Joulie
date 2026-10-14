package main

import (
	"math"
	"testing"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// TestRunRecorderFilesDecode: every line of a finished run's two files
// decodes with the strict OTLP decoders; the logs hold the job's events in
// order and every resource carries the run id; the last metrics line holds
// every run metric with the run's final values.
func TestRunRecorderFilesDecode(t *testing.T) {
	short := traceJob("a", func(r map[string]any) { r["work"] = map[string]any{"cpuUnits": 25.0} })
	h := newV1FakeHarness(t, fakeNodes{"n1": fakeCPUNode()}, []string{short}, workload.RunOptions{})
	h.tick(0)
	h.step(1)
	h.runOn("a", "n1")
	for sec := 2.0; sec <= 30; sec++ {
		h.step(sec)
	}
	if st := h.run.status(); st.Phase != workload.PhaseFinished {
		t.Fatalf("run %+v, want finished", st)
	}

	evs := readRunLogs(t, h.run.dir)
	want := []string{
		semconv.EventSimRunStarted.Name, semconv.EventSimJobSubmitted.Name, semconv.EventSimJobScheduled.Name,
		semconv.EventSimJobStarted.Name, semconv.EventSimJobCompleted.Name, semconv.EventSimRunFinished.Name,
	}
	var got []string
	for _, e := range evs {
		got = append(got, e.Name)
		if id, _ := e.Resource.String(semconv.AttrSimRunID); id != "r1" {
			t.Errorf("%s on a resource with run id %q, want r1", e.Name, id)
		}
		if svc, _ := e.Resource.String(semconv.AttrServiceName); svc == "" {
			t.Errorf("%s on a resource without service.name", e.Name)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events %v, want %v", got, want)
		}
	}

	lines, points := readRunMetrics(t, h.run.dir)
	if lines < 3 {
		t.Errorf("%d metrics lines over 30 s at a 10 s interval plus the final one, want at least 3", lines)
	}
	seen := map[string]bool{}
	value := func(metric string, attrs map[string]string) (float64, bool) {
		for _, p := range points {
			if p.Metric != metric {
				continue
			}
			ok := true
			for k, v := range attrs {
				if s, _ := p.Attributes.String(k); s != v {
					ok = false
				}
			}
			if ok {
				if p.Histogram != nil {
					return float64(p.Histogram.Count), true
				}
				return p.Value, true
			}
		}
		return 0, false
	}
	for _, p := range points {
		seen[p.Metric] = true
	}
	for _, m := range []semconv.Metric{
		semconv.JoulieSimWorkloadJobsSubmitted, semconv.JoulieSimWorkloadJobsFinished, semconv.JoulieSimWorkloadJobsActive,
		semconv.JoulieSimWorkloadJobDuration, semconv.JoulieSimWorkloadJobQueueWait, semconv.JoulieSimWorkloadJobSlowdown,
		semconv.JoulieSimWorkloadWork,
	} {
		if !seen[m.Name] {
			t.Errorf("the last metrics line has no %s point", m.Name)
		}
	}
	std := map[string]string{semconv.AttrJoulieWorkloadClass: workload.ClassStandard}
	checks := []struct {
		metric string
		attrs  map[string]string
		want   float64
	}{
		{semconv.JoulieSimWorkloadJobsSubmitted.Name, std, 1},
		{semconv.JoulieSimWorkloadJobsFinished.Name, map[string]string{semconv.AttrJoulieWorkloadClass: workload.ClassStandard, semconv.AttrSimJobOutcome: semconv.JobOutcomeCompleted}, 1},
		{semconv.JoulieSimWorkloadJobsActive.Name, map[string]string{semconv.AttrJoulieWorkloadClass: workload.ClassStandard, semconv.AttrSimJobState: semconv.JobStateRunning}, 0},
		{semconv.JoulieSimWorkloadWork.Name, map[string]string{semconv.AttrJoulieWorkloadClass: workload.ClassStandard, semconv.AttrSimWorkKind: semconv.WorkKindCPU}, 25},
		{semconv.JoulieSimWorkloadJobDuration.Name, std, 1},
		{semconv.JoulieSimWorkloadJobSlowdown.Name, std, 1},
	}
	for _, c := range checks {
		v, ok := value(c.metric, c.attrs)
		if !ok || math.Abs(v-c.want) > 1e-9 {
			t.Errorf("%s%v = %v (found %v), want %v", c.metric, c.attrs, v, ok, c.want)
		}
	}
}
