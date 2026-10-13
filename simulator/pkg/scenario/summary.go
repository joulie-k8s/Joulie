package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// RunEndFile, next to RunStatusFile, records why the harness stopped
// following a variant's run.
const RunEndFile = "run-end.json"

// Reasons a run ended, as the harness saw it.
const (
	// RunEndFinished: the simulator ended the run, or every job was
	// terminal when the harness finished it.
	RunEndFinished = "finished"
	// RunEndDeadline: duration plus drain passed with jobs left.
	RunEndDeadline = "deadline"
	// RunEndStalled: the run stayed stalled for abortOnRunStalled.
	RunEndStalled = "stalled"
	// RunEndInterrupted: the harness was stopped (a signal) mid-run.
	RunEndInterrupted = "interrupted"
)

// RunEnd is the content of RunEndFile.
type RunEnd struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// ReadRunEnd reads RunEndFile of the variant directory dir; a missing file
// is a zero RunEnd.
func ReadRunEnd(dir string) (RunEnd, error) {
	var e RunEnd
	b, err := os.ReadFile(filepath.Join(dir, RunEndFile))
	if errors.Is(err, os.ErrNotExist) {
		return e, nil
	}
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, fmt.Errorf("%s: %w", RunEndFile, err)
	}
	return e, nil
}

// Err is the run error the end gives Summary.Finish: a timeout (wrapping
// context.DeadlineExceeded) when the deadline passed with jobs left, an
// abort when the harness was interrupted, and nil otherwise. A run aborted
// for staying stalled is judged by its assertions, such as a stall check.
func (e RunEnd) Err() error {
	switch e.Reason {
	case RunEndDeadline:
		return fmt.Errorf("jobs were left when the run's deadline (duration plus drain) passed: %w", context.DeadlineExceeded)
	case RunEndInterrupted:
		return errors.New("the harness was interrupted before the run ended")
	}
	return nil
}

// AddAssertions appends results to s.Assertions.
func (s *Summary) AddAssertions(results ...AssertionResult) {
	s.Assertions = append(s.Assertions, results...)
}

// Finish sets Failures, Warnings, Status and ExitCode from the assertions,
// comparisons and variants of s. runErr, when not nil, is a run that could
// not be carried out or evaluated: the exit code is ExitError whatever the
// checks say, and the status timed_out when runErr is a
// context.DeadlineExceeded, aborted otherwise. A failed check of severity fail fails
// the summary; one of severity warn adds a warning. Lines already in
// Failures and Warnings, such as the smoke test's known defects, are kept.
func (s *Summary) Finish(runErr error) {
	exit := ExitPass
	for _, a := range s.Assertions {
		if a.Status == semconv.TestCaseResultStatusPass {
			continue
		}
		line := checkLine(a.Name, a.Observed, a.Expected, a.Message)
		if a.Severity == SeverityWarn {
			s.Warnings = append(s.Warnings, line)
			continue
		}
		s.Failures = append(s.Failures, line)
		exit = ExitFail
	}
	for _, c := range s.Comparisons {
		if c.Status == semconv.TestCaseResultStatusPass {
			continue
		}
		line := checkLine(c.Name, c.Observed, c.Expected, c.Message)
		if c.Severity == SeverityWarn {
			s.Warnings = append(s.Warnings, line)
			continue
		}
		s.Failures = append(s.Failures, line)
		exit = ExitFail
	}
	for _, v := range s.Variants {
		if v.ExitCode > exit {
			exit = v.ExitCode
		}
		if v.ExitCode != ExitPass {
			s.Failures = append(s.Failures, fmt.Sprintf("variant %s: %s (exit %d), see %s", v.Name, v.Status, v.ExitCode, v.Summary))
		}
	}
	if len(s.Failures) > 0 && exit == ExitPass {
		exit = ExitFail
	}
	if runErr != nil {
		s.Failures = append(s.Failures, "run: "+runErr.Error())
		exit = ExitError
	}
	s.ExitCode = exit
	switch {
	case exit == ExitPass:
		s.Status = semconv.TestSuiteRunStatusSuccess
	case exit == ExitFail:
		s.Status = semconv.TestSuiteRunStatusFailure
	case errors.Is(runErr, context.DeadlineExceeded):
		s.Status = semconv.TestSuiteRunStatusTimedOut
	default:
		s.Status = semconv.TestSuiteRunStatusAborted
	}
}

// checkLine is one Failures or Warnings line of a check.
func checkLine(name string, observed *float64, expected, msg string) string {
	if observed == nil {
		if msg == "" {
			msg = NoData
		}
		return fmt.Sprintf("%s: %s, want %s", name, msg, expected)
	}
	line := fmt.Sprintf("%s: observed %s, want %s", name, formatFloat(*observed), expected)
	if msg != "" {
		line += " (" + msg + ")"
	}
	return line
}

// NewSummary returns a summary with the schema and convention fields set.
func NewSummary(suite string) Summary {
	return Summary{Schema: semconv.SimclusterSummarySchema, SuiteName: suite, SemconvSchemaURL: semconv.SchemaURL}
}

// MarshalSummary returns s as indented JSON, with comparators such as
// ">= 0.95" written as they read rather than HTML-escaped.
func MarshalSummary(s Summary) ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, unescapeHTML(b), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// unescapeHTML turns the \u003c, \u003e and \u0026 escapes encoding/json
// writes for <, > and & back into the characters. An escape starts at a
// backslash preceded by an even number of backslashes; a literal
// backslash in the text is itself escaped, so it is never mistaken for one.
func unescapeHTML(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			out = append(out, b[i])
			continue
		}
		if i+5 < len(b) && b[i+1] == 'u' && b[i+2] == '0' && b[i+3] == '0' {
			switch string(b[i+4 : i+6]) {
			case "3c":
				out = append(out, '<')
				i += 5
				continue
			case "3e":
				out = append(out, '>')
				i += 5
				continue
			case "26":
				out = append(out, '&')
				i += 5
				continue
			}
		}
		// Any other escape is two characters or more; copy its backslash
		// and the character after it, so an escaped backslash is skipped
		// whole.
		out = append(out, b[i])
		if i+1 < len(b) {
			i++
			out = append(out, b[i])
		}
	}
	return out
}

// WriteSummary writes s to path as MarshalSummary formats it.
func WriteSummary(path string, s Summary) error {
	b, err := MarshalSummary(s)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// ReadSummary reads a summary.json.
func ReadSummary(path string) (*Summary, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Summary
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// SuiteName is the test.suite.name of a scenario run, and of one of its
// named variants.
func SuiteName(scenario, variant string) string {
	if variant == "" {
		return scenario
	}
	return scenario + "/" + variant
}

// VariantSummary returns the summary of one variant from its data and
// evaluated checks: the jobs, the classes and the energy inside
// EnergyBoundary, plus the settings env records. Images are read from the
// variant's images.env when present, and the run's duration from its
// status; the caller adds the durations of the phases around the run and
// calls Finish.
func VariantSummary(rd *RunData, env VariantEnv, results []AssertionResult) Summary {
	s := NewSummary(SuiteName(env.Scenario, env.Variant))
	s.Scenario, s.Variant, s.RunID = env.Scenario, env.Variant, rd.RunID
	s.Spec, s.Timing, s.TelemetryMode, s.PowerScope, s.InputPreset = env.Spec, env.Timing, env.Telemetry, env.PowerScope, env.InputPreset
	s.Git = Git{SHA: env.GitSHA, Dirty: env.GitDirty}
	if rd.Status != nil {
		start, end := rd.Start, rd.End
		s.StartTime, s.EndTime = &start, &end
		s.DurationsSeconds = map[string]float64{"run": rd.Status.ElapsedSeconds}
	}
	if b, err := os.ReadFile(filepath.Join(rd.Dir, ImagesEnvFile)); err == nil {
		if kv, err := parseEnvFile(b); err == nil {
			s.Images = map[string]string{}
			for k, v := range kv {
				s.Images[k] = strings.Join(v, " ")
			}
		}
	}
	s.AddAssertions(results...)
	jobs := indexJobs(rd)
	js := &JobsSummary{}
	type classAcc struct {
		sum                       ClassSummary
		duration, queue, slowdown []float64
	}
	classes := map[string]*classAcc{}
	hours := rd.End.Sub(rd.Start).Hours()
	for _, j := range jobs {
		js.Total++
		if !j.Due {
			js.NotDue++
			continue
		}
		c := classes[j.Class]
		if c == nil {
			c = &classAcc{}
			classes[j.Class] = c
		}
		c.sum.Jobs++
		if j.Stalled {
			js.StalledEver++
		}
		if !j.Scheduled {
			js.NeverScheduled++
		}
		switch j.Outcome {
		case semconv.JobOutcomeCompleted:
			js.Completed++
			c.sum.Completed++
			if j.Duration != nil {
				c.duration = append(c.duration, *j.Duration)
			}
			if j.QueueWait != nil {
				c.queue = append(c.queue, *j.QueueWait)
			}
			if j.RunTime != nil && j.IdealRunTime != nil && *j.IdealRunTime > 0 {
				c.slowdown = append(c.slowdown, *j.RunTime / *j.IdealRunTime)
			}
		case semconv.JobOutcomeFailed:
			js.Failed++
		default:
			js.Incomplete++
		}
	}
	energy := &EnergySummary{Boundary: EnergyBoundary, IdleSplit: EnergyIdleSplit}
	for _, p := range latestPerSeries(rd.Points, semconv.JoulieSimWorkloadEnergy.Name) {
		class, _ := p.Attributes.String(semconv.AttrJoulieWorkloadClass)
		if class == workload.ClassIdle {
			energy.IdleJoules += p.Value
			continue
		}
		energy.AttributedJoules += p.Value
		c := classes[class]
		if c == nil {
			c = &classAcc{}
			classes[class] = c
		}
		switch hw, _ := p.Attributes.String(semconv.AttrHwType); hw {
		case semconv.HwTypeCPU:
			c.sum.EnergyJoules.CPU += p.Value
		case semconv.HwTypeGPU:
			c.sum.EnergyJoules.GPU += p.Value
		}
		c.sum.EnergyJoules.Total += p.Value
	}
	energy.TotalJoules = energy.AttributedJoules + energy.IdleJoules
	for _, p := range latestPerSeries(rd.Points, semconv.JoulieSimWorkloadWork.Name) {
		class, _ := p.Attributes.String(semconv.AttrJoulieWorkloadClass)
		c := classes[class]
		if c == nil {
			continue
		}
		switch kind, _ := p.Attributes.String(semconv.AttrSimWorkKind); kind {
		case semconv.WorkKindCPU:
			c.sum.WorkUnits.CPU += p.Value
		case semconv.WorkKindGPU:
			c.sum.WorkUnits.GPU += p.Value
		}
	}
	s.Jobs, s.Energy = js, energy
	s.Classes = map[string]ClassSummary{}
	for name, c := range classes {
		if name == "" {
			continue
		}
		if hours > 0 {
			c.sum.ThroughputJobsPerHour = float64(c.sum.Completed) / hours
		}
		if c.sum.Completed > 0 {
			c.sum.EnergyPerJobJoules = c.sum.EnergyJoules.Total / float64(c.sum.Completed)
		}
		c.sum.Duration, c.sum.QueueWait, c.sum.Slowdown = quantiles(c.duration), quantiles(c.queue), quantiles(c.slowdown)
		s.Classes[name] = c.sum
	}
	return s
}

func quantiles(v []float64) Quantiles {
	if len(v) == 0 {
		return Quantiles{}
	}
	return Quantiles{P50: Quantile(v, 0.5), P95: Quantile(v, 0.95), Max: Quantile(v, 1)}
}

// TopSummary returns the top-level summary of a scenario with variants:
// each variant's result and the comparisons across them. The caller calls
// Finish.
func TopSummary(s Scenario, variants map[string]*Summary) Summary {
	top := NewSummary(s.Name)
	top.Scenario, top.Spec, top.Timing = s.Name, s.Cluster.Spec, s.Cluster.Timing
	for _, name := range s.RunVariants() {
		rel := filepath.Join(name, SummaryFile)
		r := VariantResult{Name: name, Summary: rel, Status: semconv.TestSuiteRunStatusAborted, ExitCode: ExitError}
		if v := variants[name]; v != nil {
			r.Status, r.ExitCode = v.Status, v.ExitCode
			if top.Git.SHA == "" {
				top.Git = v.Git
			}
			if top.StartTime == nil || (v.StartTime != nil && v.StartTime.Before(*top.StartTime)) {
				top.StartTime = v.StartTime
			}
			if top.EndTime == nil || (v.EndTime != nil && v.EndTime.After(*top.EndTime)) {
				top.EndTime = v.EndTime
			}
		}
		top.Variants = append(top.Variants, r)
	}
	top.Comparisons = EvaluateComparisons(s, variants)
	return top
}

// Harness is the runner's telemetry identity: the resource of its records
// in a variant's harness files.
type Harness struct {
	// Dir is the variant directory.
	Dir string
	// ClusterName is the cluster spec's name, RunID the simulator run id.
	ClusterName, RunID string
	Scenario, Variant  string
}

// resource is the harness resource: the runner's service identity, the
// cluster and the run.
func (h Harness) resource() otlpjson.Resource {
	host, _ := os.Hostname()
	version := "unknown"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		version = bi.Main.Version
	}
	return otlpjson.Resource{Attributes: []otlpjson.KeyValue{
		otlpjson.String(semconv.AttrServiceName, ServiceName),
		otlpjson.String(semconv.AttrServiceNamespace, "joulie"),
		otlpjson.String(semconv.AttrServiceInstanceID, host),
		otlpjson.String(semconv.AttrServiceVersion, version),
		otlpjson.String(semconv.AttrK8sClusterName, h.ClusterName),
		otlpjson.String(semconv.AttrSimRunID, h.RunID),
	}}
}

func (h Harness) appendLine(file string, v any) error {
	dir := filepath.Join(h.Dir, workload.OTLPDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// WriteMetrics appends one MetricsData line holding metrics to the
// variant's HarnessMetricsFile.
func (h Harness) WriteMetrics(metrics ...otlpjson.Metric) error {
	if len(metrics) == 0 {
		return nil
	}
	return h.appendLine(HarnessMetricsFile, otlpjson.MetricsData{ResourceMetrics: []otlpjson.ResourceMetrics{{
		Resource: h.resource(), SchemaURL: semconv.SchemaURL,
		ScopeMetrics: []otlpjson.ScopeMetrics{{Scope: otlpjson.Scope{Name: ScopeName}, SchemaURL: semconv.SchemaURL, Metrics: metrics}},
	}}})
}

// WriteLogs appends one LogsData line holding records to the variant's
// HarnessLogsFile.
func (h Harness) WriteLogs(records ...otlpjson.LogRecord) error {
	if len(records) == 0 {
		return nil
	}
	return h.appendLine(HarnessLogsFile, otlpjson.LogsData{ResourceLogs: []otlpjson.ResourceLogs{{
		Resource: h.resource(), SchemaURL: semconv.SchemaURL,
		ScopeLogs: []otlpjson.ScopeLogs{{Scope: otlpjson.Scope{Name: ScopeName}, SchemaURL: semconv.SchemaURL, LogRecords: records}},
	}}})
}

// EventRecord is one record of event e that occurred at, with attrs.
func EventRecord(e semconv.Event, at time.Time, body string, attrs ...otlpjson.KeyValue) otlpjson.LogRecord {
	return otlpjson.LogRecord{
		TimeUnixNano: otlpjson.Uint64String(at.UnixNano()), ObservedTimeUnixNano: otlpjson.Uint64String(time.Now().UnixNano()),
		SeverityNumber: e.SeverityNumber, SeverityText: e.SeverityText, Body: &otlpjson.AnyValue{String: &body},
		Attributes: attrs, EventName: e.Name,
	}
}

// ScenarioEvent is a joulie.scenario.* event of h's scenario and variant,
// with extra attributes.
func (h Harness) ScenarioEvent(e semconv.Event, at time.Time, extra ...otlpjson.KeyValue) otlpjson.LogRecord {
	attrs := append([]otlpjson.KeyValue{
		otlpjson.String(semconv.AttrJoulieScenarioName, h.Scenario),
		otlpjson.String(semconv.AttrJoulieScenarioVariant, h.Variant),
	}, extra...)
	return EventRecord(e, at, fmt.Sprintf("%s %s", e.Name, SuiteName(h.Scenario, h.Variant)), attrs...)
}

// ResultRecords returns, for each result, one
// joulie.scenario.assertion.evaluated record (ERROR with error.type
// assertion_failed when it failed) and the joulie.scenario.assertion.value
// points of its observed value and of its expected number, keyed by the
// check's name. skipObserved names the checks whose observed value is
// already recorded, as a promql assertion's is.
func ResultRecords(results []AssertionResult, at time.Time, skipObserved map[string]bool) ([]otlpjson.LogRecord, *otlpjson.Metric) {
	var recs []otlpjson.LogRecord
	gauge := &otlpjson.Gauge{}
	ts := otlpjson.Uint64String(at.UnixNano())
	point := func(name, source string, v float64) {
		gauge.DataPoints = append(gauge.DataPoints, otlpjson.NumberDataPoint{
			Attributes: []otlpjson.KeyValue{otlpjson.String(semconv.AttrJoulieAssertionName, name),
				otlpjson.String(semconv.AttrJoulieAssertionSource, source)},
			TimeUnixNano: ts, AsDouble: &v,
		})
	}
	for _, r := range results {
		passed := r.Status == semconv.TestCaseResultStatusPass
		attrs := []otlpjson.KeyValue{
			otlpjson.String(semconv.AttrJoulieAssertionName, r.Name),
			otlpjson.String(semconv.AttrJoulieAssertionType, string(r.Type)),
			otlpjson.Bool(semconv.AttrJoulieAssertionPassed, passed),
		}
		if r.Observed != nil {
			attrs = append(attrs, otlpjson.Double(semconv.AttrJoulieAssertionObserved, *r.Observed))
		}
		attrs = append(attrs, otlpjson.String(semconv.AttrJoulieAssertionExpected, r.Expected))
		ev := semconv.EventScenarioAssertionEvaluated
		if !passed {
			attrs = append(attrs, otlpjson.String(semconv.AttrErrorType, semconv.ErrorTypeAssertionFailed))
			ev.SeverityNumber, ev.SeverityText = semconv.SeverityError, semconv.SeverityTextError
		}
		recs = append(recs, EventRecord(ev, at, checkLine(r.Name, r.Observed, r.Expected, r.Message), attrs...))
		if r.Observed != nil && !skipObserved[r.Name] {
			point(r.Name, semconv.AssertionSourceObserved, *r.Observed)
		}
		if e, err := Comparator(r.Expected).Parse(); err == nil {
			point(r.Name, semconv.AssertionSourceExpected, e.Value)
		}
	}
	if len(gauge.DataPoints) == 0 {
		return recs, nil
	}
	m := semconv.JoulieScenarioAssertionValue
	return recs, &otlpjson.Metric{Name: m.Name, Unit: m.Unit, Gauge: gauge}
}
