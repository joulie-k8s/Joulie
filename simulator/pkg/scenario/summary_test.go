package scenario_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/scenario"
)

const summaryGolden = "testdata/summary.golden.json"

// The fixture's summary.json is pinned: the semantic conventions' test.*
// keys, every check with its status, the jobs, classes and energy inside
// the boundary, and success with the one warning.
func TestSummaryGolden(t *testing.T) {
	rd, env := fxRunData(t)
	s := scenario.VariantSummary(rd, env, scenario.Evaluate(rd, env))
	s.Finish(nil)
	got, err := scenario.MarshalSummary(s)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile(summaryGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(summaryGolden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("summary differs from %s (run with -update after checking):\n%s", summaryGolden, got)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(got, &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{semconv.AttrTestSuiteName, semconv.AttrTestSuiteRunStatus, "exitCode", "failures", "warnings", "knownDefects",
		"assertions", "jobs", "classes", "energy", "scenario", "runId", "startTime", "git", "semconvSchemaUrl"} {
		if _, ok := top[k]; !ok {
			t.Errorf("summary has no %q", k)
		}
	}
	var checks []map[string]any
	if err := json.Unmarshal(top["assertions"], &checks); err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if _, ok := c[semconv.AttrTestCaseName].(string); !ok || (c[semconv.AttrTestCaseResultStatus] != "pass" && c[semconv.AttrTestCaseResultStatus] != "fail") {
			t.Errorf("check %v lacks %s or a pass/fail %s", c, semconv.AttrTestCaseName, semconv.AttrTestCaseResultStatus)
		}
	}
	if s.Status != semconv.TestSuiteRunStatusSuccess || s.ExitCode != scenario.ExitPass || len(s.Warnings) != 1 || len(s.Failures) != 0 {
		t.Errorf("status %s exit %d warnings %v failures %v", s.Status, s.ExitCode, s.Warnings, s.Failures)
	}
	if e := s.Energy; e.TotalJoules != 144000 || e.IdleJoules != 27000 || e.Boundary != scenario.EnergyBoundary {
		t.Errorf("energy %+v", e)
	}
	if j := s.Jobs; j.Total != 6 || j.NotDue != 1 || j.Completed != 3 || j.Failed != 1 || j.Incomplete != 1 || j.StalledEver != 1 {
		t.Errorf("jobs %+v", j)
	}
}

// Finish maps the checks to the exit code: a failed fail-severity check
// fails, a failed warn-severity check warns, a run error aborts, and a run
// that ran out of time times out.
func TestSummaryFinish(t *testing.T) {
	v := 1.0
	fail := scenario.AssertionResult{Name: "a", Status: semconv.TestCaseResultStatusFail, Severity: scenario.SeverityFail, Observed: &v, Expected: "== 0"}
	warn := scenario.AssertionResult{Name: "b", Status: semconv.TestCaseResultStatusFail, Severity: scenario.SeverityWarn, Expected: "== 0", Message: "no data: x"}
	ok := scenario.AssertionResult{Name: "c", Status: semconv.TestCaseResultStatusPass, Severity: scenario.SeverityFail}
	for _, c := range []struct {
		results []scenario.AssertionResult
		runErr  error
		status  string
		exit    int
	}{
		{[]scenario.AssertionResult{ok}, nil, semconv.TestSuiteRunStatusSuccess, scenario.ExitPass},
		{[]scenario.AssertionResult{ok, warn}, nil, semconv.TestSuiteRunStatusSuccess, scenario.ExitPass},
		{[]scenario.AssertionResult{ok, warn, fail}, nil, semconv.TestSuiteRunStatusFailure, scenario.ExitFail},
		{[]scenario.AssertionResult{ok}, errors.New("simulator down"), semconv.TestSuiteRunStatusAborted, scenario.ExitError},
		{[]scenario.AssertionResult{ok}, fmt.Errorf("wait for the run: %w", context.DeadlineExceeded), semconv.TestSuiteRunStatusTimedOut, scenario.ExitError},
	} {
		s := scenario.NewSummary("x")
		s.AddAssertions(c.results...)
		s.Finish(c.runErr)
		if s.Status != c.status || s.ExitCode != c.exit {
			t.Errorf("%v %v: %s %d, want %s %d", c.results, c.runErr, s.Status, s.ExitCode, c.status, c.exit)
		}
	}
	s := scenario.NewSummary("x")
	s.AddAssertions(fail, warn)
	s.Finish(nil)
	if s.Failures[0] != "a: observed 1, want == 0" || s.Warnings[0] != "b: no data: x, want == 0" {
		t.Errorf("failures %q warnings %q", s.Failures, s.Warnings)
	}
}

// The harness records of a result: one evaluated event, ERROR with
// error.type when it failed, and the observed and expected value points
// keyed by the check's name; a promql observation is not written twice.
func TestResultRecords(t *testing.T) {
	v := 0.97
	at := time.Date(2026, 10, 4, 20, 10, 0, 0, time.UTC)
	results := []scenario.AssertionResult{
		{Name: "all.completedFraction", Type: scenario.AssertJobs, Status: semconv.TestCaseResultStatusPass, Observed: &v, Expected: ">= 0.95"},
		{Name: "targets", Type: scenario.AssertPromQL, Status: semconv.TestCaseResultStatusFail, Observed: &v, Expected: ">= 3"},
		{Name: "twin", Type: scenario.AssertTwinPower, Status: semconv.TestCaseResultStatusFail, Expected: "<= 0.1", Message: "no data"},
	}
	recs, m := scenario.ResultRecords(results, at, map[string]bool{"targets": true})
	if len(recs) != 3 || m == nil || m.Name != semconv.JoulieScenarioAssertionValue.Name || m.Gauge == nil {
		t.Fatalf("records %d metric %+v", len(recs), m)
	}
	if recs[0].SeverityNumber != semconv.SeverityInfo || recs[1].SeverityNumber != semconv.SeverityError || recs[0].EventName != semconv.EventScenarioAssertionEvaluated.Name {
		t.Errorf("severities %d %d", recs[0].SeverityNumber, recs[1].SeverityNumber)
	}
	flat := scenario.FlattenLogs(otlpjson.LogsData{ResourceLogs: []otlpjson.ResourceLogs{{ScopeLogs: []otlpjson.ScopeLogs{{LogRecords: recs}}}}}, "x")
	if et, _ := flat[1].Attributes.String(semconv.AttrErrorType); et != semconv.ErrorTypeAssertionFailed {
		t.Errorf("failed record attributes %v", flat[1].Attributes)
	}
	if _, ok := flat[2].Attributes.Float(semconv.AttrJoulieAssertionObserved); ok {
		t.Errorf("a no-data record carries an observed value: %v", flat[2].Attributes)
	}
	points := scenario.FlattenMetrics(otlpjson.MetricsData{ResourceMetrics: []otlpjson.ResourceMetrics{{ScopeMetrics: []otlpjson.ScopeMetrics{{Metrics: []otlpjson.Metric{*m}}}}}}, "x")
	var got []string
	for _, p := range points {
		name, _ := p.Attributes.String(semconv.AttrJoulieAssertionName)
		src, _ := p.Attributes.String(semconv.AttrJoulieAssertionSource)
		got = append(got, name+"/"+src+"="+strings.TrimRight(strings.TrimRight(jsonNum(p.Value), "0"), "."))
	}
	want := "all.completedFraction/observed=0.97 all.completedFraction/expected=0.95 targets/expected=3 twin/expected=0.1"
	if strings.Join(got, " ") != want {
		t.Errorf("points %v, want %s", got, want)
	}
}

func jsonNum(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The harness writes one MetricsData or LogsData per line under otlp/,
// with its service identity and the run id on the resource, which reads
// back through the same path as the simulator's files.
func TestHarnessWritesOTLPLines(t *testing.T) {
	dir := t.TempDir()
	h := scenario.Harness{Dir: dir, ClusterName: "smoke", RunID: "r1", Scenario: "s", Variant: "v"}
	at := time.Date(2026, 10, 4, 20, 0, 0, 0, time.UTC)
	if err := h.WriteLogs(h.ScenarioEvent(semconv.EventScenarioVariantStarted, at)); err != nil {
		t.Fatal(err)
	}
	if err := h.WriteLogs(h.ScenarioEvent(semconv.EventScenarioFinished, at, otlpjson.String(semconv.AttrTestSuiteRunStatus, "success"))); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "otlp", scenario.HarnessLogsFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines", len(lines))
	}
	var ld otlpjson.LogsData
	dec := json.NewDecoder(strings.NewReader(lines[1]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ld); err != nil {
		t.Fatal(err)
	}
	ev := scenario.FlattenLogs(ld, scenario.HarnessLogsFile)[0]
	if svc, _ := ev.Resource.String(semconv.AttrServiceName); svc != scenario.ServiceName || ev.Name != semconv.EventScenarioFinished.Name {
		t.Errorf("event %+v", ev)
	}
	if run, _ := ev.Resource.String(semconv.AttrSimRunID); run != "r1" {
		t.Errorf("resource %v", ev.Resource)
	}
	if v, _ := ev.Attributes.String(semconv.AttrJoulieScenarioVariant); v != "v" || ld.ResourceLogs[0].SchemaURL != semconv.SchemaURL {
		t.Errorf("attributes %v schema %q", ev.Attributes, ld.ResourceLogs[0].SchemaURL)
	}
	if !strings.Contains(lines[0], `"eventName":"joulie.scenario.variant.started"`) || !strings.Contains(lines[0], `"timeUnixNano":"`) {
		t.Errorf("line %s", lines[0])
	}
}

// summary.json writes comparators as they read, and text that only looks
// like an escape stays as it was.
func TestMarshalSummaryKeepsComparators(t *testing.T) {
	s := scenario.NewSummary(`a<b & c>d <`)
	s.Failures = []string{`x: observed 1, want >= 2`}
	b, err := scenario.MarshalSummary(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `want >= 2"`) {
		t.Errorf("summary %s escapes the comparator", b)
	}
	var back scenario.Summary
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.SuiteName != `a<b & c>d <` || back.Failures[0] != s.Failures[0] {
		t.Errorf("read back %q %q", back.SuiteName, back.Failures)
	}
}
