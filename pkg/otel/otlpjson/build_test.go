package otlpjson

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
)

// The goldens pin what a v1 run and a scenario write, line for line. They are
// generated, never hand edited:
//
//	go test ./pkg/otel/otlpjson/ -run OJ07 -update
var buildUpdateGolden = flag.Bool("update", false, "rewrite pkg/otel/otlpjson/testdata/golden/*.golden.jsonl from the builders")

const (
	buildGoldenMetricsFile = "testdata/golden/metrics.golden.jsonl"
	buildGoldenLogsFile    = "testdata/golden/logs.golden.jsonl"
)

// buildT0 is the start of the golden run.
var buildT0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func buildAt(sec int) time.Time { return buildT0.Add(time.Duration(sec) * time.Second) }

// buildGoldenMetrics is the content of the metrics golden: two cumulative
// snapshots of a run, then one scenario capture line. points[i] is the number
// of points line i passed to the builders, all of them encodable.
func buildGoldenMetrics() (lines []MetricsData, points []int) {
	sim := Resource{Attributes: []KeyValue{
		String(semconv.AttrServiceName, "joulie-simulator"),
		String(semconv.AttrServiceNamespace, "joulie"),
		String(semconv.AttrServiceInstanceID, "joulie-simulator-0"),
		String(semconv.AttrK8sClusterName, "smoke"),
		String(semconv.AttrSimRunID, "run-1"),
	}}
	scope := Scope{Name: "github.com/matbun/joulie/simulator/pkg/workload", Version: "test"}
	perf := String(semconv.AttrJoulieWorkloadClass, "performance")
	std := String(semconv.AttrJoulieWorkloadClass, "standard")
	idle := String(semconv.AttrJoulieWorkloadClass, "idle")
	node := String(semconv.AttrK8sNodeName, "sim-cpu-0")
	profile := String(semconv.AttrSimProfile, "intel-xeon-2s-rapl")
	cpu := String(semconv.AttrHwType, semconv.HwTypeCPU)

	duration := NewExplicitHistogram(60, 300, 900, 3600)
	slowdown := NewExplicitHistogram(1, 1.1, 1.25, 1.5, 2)
	snapshot := func(at time.Time, submitted, completed, running, pending int64, work, energy, idleEnergy float64) (MetricsData, int) {
		ms := []Metric{
			SumMetric(semconv.JoulieSimWorkloadJobsSubmitted, IntPoint(buildT0, at, submitted, perf), IntPoint(buildT0, at, 2, std)),
			SumMetric(semconv.JoulieSimWorkloadJobsFinished,
				IntPoint(buildT0, at, completed, perf, String(semconv.AttrSimJobOutcome, semconv.JobOutcomeCompleted))),
			SumMetric(semconv.JoulieSimWorkloadJobsActive,
				IntPoint(buildT0, at, running, perf, String(semconv.AttrSimJobState, semconv.JobStateRunning)),
				IntPoint(buildT0, at, pending, std, String(semconv.AttrSimJobState, semconv.JobStatePending))),
			SumMetric(semconv.JoulieSimWorkloadWork, DoublePoint(buildT0, at, work, perf, String(semconv.AttrSimWorkKind, semconv.WorkKindCPU))),
			SumMetric(semconv.JoulieSimWorkloadEnergy,
				DoublePoint(buildT0, at, energy, perf, cpu, node, profile),
				DoublePoint(buildT0, at, idleEnergy, idle, cpu, node, profile)),
			HistogramMetric(semconv.JoulieSimWorkloadJobDuration, duration.Point(buildT0, at, perf)),
			HistogramMetric(semconv.JoulieSimWorkloadJobSlowdown, slowdown.Point(buildT0, at, perf)),
		}
		// Points per metric, in order: 2, 1, 2, 1, 2, 1, 1.
		return NewMetricsData(sim, scope, ms...), 10
	}

	duration.Record(240)
	slowdown.Record(1.05)
	l1, n1 := snapshot(buildAt(10), 3, 1, 2, 2, 1200.5, 15234.25, 4100)
	duration.Record(75)
	duration.Record(1200)
	slowdown.Record(1)
	slowdown.Record(1.6)
	l2, n2 := snapshot(buildAt(20), 3, 3, 0, 2, 3600, 30468.5, 8200.125)

	harness := Resource{Attributes: []KeyValue{
		String(semconv.AttrServiceName, "simscenario"),
		String(semconv.AttrServiceNamespace, "joulie"),
		String(semconv.AttrServiceInstanceID, "cap-cost-eco"),
	}}
	hscope := Scope{Name: "github.com/matbun/joulie/simulator/pkg/scenario", Version: "test"}
	check := String(semconv.AttrJoulieAssertionName, "jobs.completedFraction")
	l3 := NewMetricsData(harness, hscope,
		GaugeMetric(semconv.JoulieCaptureNodeTwinMeasuredPower,
			DoublePoint(time.Time{}, buildAt(20), 598.59, node, String(semconv.AttrJoulieNodeTwinPowerSource, "prometheus"))),
		SumMetric(semconv.JoulieCaptureNodeTwinSchedulableClass,
			IntPoint(buildT0, buildAt(20), 1, node, String(semconv.AttrJoulieNodeSchedulableClass, "performance"))),
		GaugeMetric(semconv.JoulieScenarioAssertionValue,
			DoublePoint(time.Time{}, buildAt(30), 1, check, String(semconv.AttrJoulieAssertionSource, semconv.AssertionSourceObserved)),
			DoublePoint(time.Time{}, buildAt(30), 0.95, check, String(semconv.AttrJoulieAssertionSource, semconv.AssertionSourceExpected))),
	)
	// The capture line: 1, 1 and 2 points.
	return []MetricsData{l1, l2, l3}, []int{n1, n2, 4}
}

// buildGoldenLogs is the content of the logs golden: two ticks of a run's
// events, then one line of scenario events with a failed assertion.
// records[i] is the number of records line i passed to the builders.
func buildGoldenLogs() (lines []LogsData, records []int) {
	sim := Resource{Attributes: []KeyValue{
		String(semconv.AttrServiceName, "joulie-simulator"),
		String(semconv.AttrServiceInstanceID, "joulie-simulator-0"),
		String(semconv.AttrSimRunID, "run-1"),
	}}
	scope := Scope{Name: "github.com/matbun/joulie/simulator/pkg/workload", Version: "test"}
	job := []KeyValue{
		String(semconv.AttrSimJobID, "j-0001"),
		String(semconv.AttrSimWorkloadID, "w-01"),
		String(semconv.AttrSimWorkloadType, "batch"),
		String(semconv.AttrSimPodRole, "worker"),
		String(semconv.AttrJoulieWorkloadClass, "performance"),
		String(semconv.AttrK8sNamespaceName, "sim-run-1"),
	}
	with := func(extra ...KeyValue) []KeyValue { return append(append([]KeyValue(nil), job...), extra...) }
	node := String(semconv.AttrK8sNodeName, "sim-gpu-0")

	l1 := NewLogsData(sim, scope,
		EventRecord(semconv.EventSimRunStarted, buildAt(0), buildAt(1), "run run-1 started with 2 jobs",
			Int(semconv.AttrSimRunJobs, 2)),
		EventRecord(semconv.EventSimJobSubmitted, buildAt(1), time.Time{}, "", with(Bool(semconv.AttrSimGang, false))...),
		EventRecord(semconv.EventSimJobScheduled, buildAt(3), buildAt(4), "", with(
			node,
			String(semconv.AttrJoulieNodePowerProfile, "performance"),
			String(semconv.AttrJoulieNodeSchedulableClass, "performance"),
			Double(semconv.AttrJoulieNodeClassSince, 412.5),
			String(semconv.AttrSimProfile, "nvidia-h100-nvl-8gpu"),
			Double(semconv.AttrSimJobQueueWait, 2.25))...),
		EventRecord(semconv.EventSimJobStarted, buildAt(5), buildAt(5), "", with(node, IntArray(semconv.AttrSimGPUIndices, 0, 1))...),
	)
	// j-0002 is a second worker of the same workload.
	j2 := append([]KeyValue{String(semconv.AttrSimJobID, "j-0002")}, job[1:]...)
	failed := EventRecord(semconv.EventSimJobFailed, buildAt(18), buildAt(19), "job j-0002 failed: pod deleted",
		append(j2, String(semconv.AttrErrorType, semconv.ErrorTypePodDeleted))...)
	l2 := NewLogsData(sim, scope,
		EventRecord(semconv.EventSimJobCompleted, buildAt(15), buildAt(16), "job j-0001 completed on sim-gpu-0", with(
			String(semconv.AttrK8sPodName, "sim-w-01-worker-0a1b2c3d-0"),
			Double(semconv.AttrSimJobDuration, 14),
			Double(semconv.AttrSimJobRunTime, 10),
			Double(semconv.AttrSimJobIdealRunTime, 9.5),
			Double(semconv.AttrSimJobEnergy, 7012.75),
			Double(semconv.AttrSimWorkCPU, 32),
			Double(semconv.AttrSimWorkGPU, 19))...),
		failed,
		EventRecord(semconv.EventSimRunStalled, buildAt(19), buildAt(19), "",
			String(semconv.AttrSimStallReason, semconv.StallReasonAllBlocked), Double(semconv.AttrSimStallAge, 300)),
		EventRecord(semconv.EventSimRunFinished, buildAt(20), buildAt(20), "",
			Int(semconv.AttrSimRunJobs, 2), Int(semconv.AttrSimRunCompletedJobs, 1),
			Int(semconv.AttrSimRunFailedJobs, 1), Int(semconv.AttrSimRunIncompleteJobs, 0)),
	)

	harness := Resource{Attributes: []KeyValue{
		String(semconv.AttrServiceName, "simscenario"),
		String(semconv.AttrServiceInstanceID, "cap-cost-eco"),
	}}
	hscope := Scope{Name: "github.com/matbun/joulie/simulator/pkg/scenario", Version: "test"}
	scen := []KeyValue{String(semconv.AttrJoulieScenarioName, "cap-cost"), String(semconv.AttrJoulieScenarioVariant, "eco")}
	failedCheck := EventRecord(semconv.EventScenarioAssertionEvaluated, buildAt(30), buildAt(31), "energy.total: 9.1e+04 J, want <= 80 kJ",
		String(semconv.AttrJoulieAssertionName, "energy.total"),
		String(semconv.AttrJoulieAssertionType, "energy"),
		Bool(semconv.AttrJoulieAssertionPassed, false),
		Double(semconv.AttrJoulieAssertionObserved, 91000),
		String(semconv.AttrJoulieAssertionExpected, "<= 80 kJ"),
		String(semconv.AttrErrorType, semconv.ErrorTypeAssertionFailed))
	failedCheck.SeverityNumber, failedCheck.SeverityText = semconv.SeverityError, semconv.SeverityTextError
	l3 := NewLogsData(harness, hscope,
		EventRecord(semconv.EventScenarioStarted, buildAt(0), time.Time{}, "", scen...),
		EventRecord(semconv.EventScenarioAssertionEvaluated, buildAt(30), buildAt(31), "",
			String(semconv.AttrJoulieAssertionName, "jobs.completedFraction"),
			String(semconv.AttrJoulieAssertionType, "jobs"),
			Bool(semconv.AttrJoulieAssertionPassed, true),
			Double(semconv.AttrJoulieAssertionObserved, 1),
			String(semconv.AttrJoulieAssertionExpected, ">= 0.95")),
		failedCheck,
		EventRecord(semconv.EventScenarioFinished, buildAt(32), buildAt(32), "",
			append(scen, String(semconv.AttrTestSuiteRunStatus, semconv.TestSuiteRunStatusFailure))...),
	)
	return []LogsData{l1, l2, l3}, []int{4, 4, 4}
}

// buildLines encodes each value as one JSON line, as a .jsonl file holds
// them. HTML escaping is off so that comparators such as ">=" read as
// written; either form is valid JSON.
func buildLines[T any](t *testing.T, lines []T) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// buildCompareGolden compares got with the golden file, or rewrites the file
// under -update.
func buildCompareGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *buildUpdateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v. Generate it with: go test ./pkg/otel/otlpjson/ -run OJ07 -update", err)
	}
	if bytes.Equal(got, want) {
		return
	}
	g, w := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			t.Fatalf("%s line %d differs from the builders' output.\n got %s\nwant %s\n"+
				"If the change is intended, review it and run: go test ./pkg/otel/otlpjson/ -run OJ07 -update", path, i+1, gl, wl)
		}
	}
}

// buildReadLines returns the non-empty lines of a golden file.
func buildReadLines(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) > 0 {
			out = append(out, append([]byte(nil), sc.Bytes()...))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// buildDecodeStrict decodes one OTLP JSON object into v the way the strict
// decoders are specified (types.go): an unknown key, which includes every
// snake_case spelling, fails, and so does anything after the object. It
// stands in for DecodeMetricsStrict and DecodeLogsStrict, which package
// decode.go provides; once that file is in the package, the calls switch to
// those and these stand-ins go.
func buildDecodeStrict(b []byte, v any) error {
	// encoding/json matches field names without regard to case, so
	// DisallowUnknownFields alone accepts "ResourceMetrics": every key must
	// first be spelled exactly as a field of the model.
	var generic any
	if err := json.Unmarshal(b, &generic); err == nil {
		if err := buildCheckModelKeys(generic); err != nil {
			return err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("data after the object: %v", err)
	}
	return nil
}

// buildModelKeys holds the JSON tag names of every field reachable from
// MetricsData and LogsData.
var buildModelKeys = func() map[string]bool {
	keys := map[string]bool{}
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || seen[t] {
			return
		}
		seen[t] = true
		for i := 0; i < t.NumField(); i++ {
			if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
				keys[name] = true
			}
			walk(t.Field(i).Type)
		}
	}
	walk(reflect.TypeOf(MetricsData{}))
	walk(reflect.TypeOf(LogsData{}))
	return keys
}()

// buildCheckModelKeys fails on the first object key in v that is not in
// buildModelKeys.
func buildCheckModelKeys(v any) error {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if !buildModelKeys[k] {
				return fmt.Errorf("key %q is not an OTLP JSON field name", k)
			}
			if err := buildCheckModelKeys(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range x {
			if err := buildCheckModelKeys(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// buildDecodeMetricsStrict and buildDecodeLogsStrict decode one line
// strictly, with the signatures of DecodeMetricsStrict and DecodeLogsStrict.
func buildDecodeMetricsStrict(b []byte) (MetricsData, error) {
	var d MetricsData
	return d, buildDecodeStrict(b, &d)
}

func buildDecodeLogsStrict(b []byte) (LogsData, error) {
	var d LogsData
	return d, buildDecodeStrict(b, &d)
}

// buildCountPoints counts the data points of every metric in d.
func buildCountPoints(d MetricsData) int {
	n := 0
	for _, rm := range d.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch {
				case m.Sum != nil:
					n += len(m.Sum.DataPoints)
				case m.Gauge != nil:
					n += len(m.Gauge.DataPoints)
				case m.Histogram != nil:
					n += len(m.Histogram.DataPoints)
				}
			}
		}
	}
	return n
}

// buildCountRecords counts the log records in d.
func buildCountRecords(d LogsData) int {
	n := 0
	for _, rl := range d.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			n += len(sl.LogRecords)
		}
	}
	return n
}

// buildTableMetric returns the semconv entry named name.
func buildTableMetric(t *testing.T, name string) semconv.Metric {
	t.Helper()
	for _, m := range semconv.Metrics() {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("metric %q is not in the semconv table", name)
	return semconv.Metric{}
}

// OJ07: the goldens are what the builders write, and they encode enums as
// integers (aggregationTemporality, severityNumber), every 64-bit integer as
// a decimal string (times, asInt, intValue, a histogram's count and
// bucketCounts), and each event's eventName and severity from the semconv
// table. Each metric's instrument, value type, unit and point attributes
// follow its table entry, as each event's attributes do, and every job event
// carries the job's identity.
func TestOJ07GoldenEncodesEnumsInt64StringsAndEventName(t *testing.T) {
	metrics, _ := buildGoldenMetrics()
	logs, _ := buildGoldenLogs()
	mb, lb := buildLines(t, metrics), buildLines(t, logs)

	t0, t10 := buildUnixNano(buildT0), buildUnixNano(buildAt(10))
	for _, c := range []struct {
		text string
		want []string
	}{
		{string(mb), []string{
			`"schemaUrl":"https://opentelemetry.io/schemas/1.44.0"`,
			`"aggregationTemporality":2,"isMonotonic":true`,
			fmt.Sprintf(`"startTimeUnixNano":"%d","timeUnixNano":"%d","asInt":"3"`, t0, t10),
			`"asDouble":15234.25`, `"count":"1"`, `"bucketCounts":["0","1","0","0","0"]`, `"explicitBounds":[60,300,900,3600]`,
			`"name":"joulie.sim.workload.jobs.active","description":`,
		}},
		{string(lb), []string{
			`"eventName":"joulie.sim.job.completed"`, `"eventName":"joulie.scenario.assertion.evaluated"`,
			`"severityNumber":9,"severityText":"INFO"`, `"severityNumber":13,"severityText":"WARN"`,
			`"severityNumber":17,"severityText":"ERROR"`,
			fmt.Sprintf(`"timeUnixNano":"%d","observedTimeUnixNano":"%d"`, t0, buildUnixNano(buildAt(1))),
			`{"key":"joulie.sim.run.jobs","value":{"intValue":"2"}}`,
			`{"key":"joulie.sim.gpu.indices","value":{"arrayValue":{"values":[{"intValue":"0"},{"intValue":"1"}]}}}`,
			`"body":{"stringValue":"job j-0001 completed on sim-gpu-0"}`,
		}},
	} {
		for _, w := range c.want {
			if !strings.Contains(c.text, w) {
				t.Errorf("golden lacks %s", w)
			}
		}
	}
	// No 64-bit integer as a JSON number, no enum as a string.
	numeric := regexp.MustCompile(`"(timeUnixNano|startTimeUnixNano|observedTimeUnixNano|asInt|intValue|count)":-?[0-9]`)
	quotedEnum := regexp.MustCompile(`"(aggregationTemporality|severityNumber)":"`)
	for _, text := range []string{string(mb), string(lb)} {
		if m := numeric.FindString(text); m != "" {
			t.Errorf("a 64-bit integer is a JSON number: %s", m)
		}
		if m := quotedEnum.FindString(text); m != "" {
			t.Errorf("an enum is a string: %s", m)
		}
		var generic any
		for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
			if err := json.Unmarshal([]byte(line), &generic); err != nil {
				t.Fatal(err)
			}
			otlpCheckKeys(t, generic, "$")
		}
	}

	for _, d := range metrics {
		for _, m := range d.ResourceMetrics[0].ScopeMetrics[0].Metrics {
			entry := buildTableMetric(t, m.Name)
			if m.Unit != entry.Unit || m.Description != entry.Brief {
				t.Errorf("%s: unit %q and description %q, want the table's %q and brief", m.Name, m.Unit, m.Description, entry.Unit)
			}
			var points []NumberDataPoint
			switch entry.Kind {
			case semconv.Counter, semconv.UpDownCounter:
				if m.Sum == nil || m.Sum.AggregationTemporality != AggregationTemporalityCumulative ||
					m.Sum.IsMonotonic != (entry.Kind == semconv.Counter) {
					t.Errorf("%s: a %v is encoded as %+v", m.Name, entry.Kind, m)
					continue
				}
				points = m.Sum.DataPoints
			case semconv.Gauge:
				if m.Gauge == nil {
					t.Errorf("%s: a gauge is encoded as %+v", m.Name, m)
					continue
				}
				points = m.Gauge.DataPoints
			case semconv.Histogram:
				if m.Histogram == nil || m.Histogram.AggregationTemporality != AggregationTemporalityCumulative {
					t.Errorf("%s: a histogram is encoded as %+v", m.Name, m)
				}
			}
			var attrs [][]KeyValue
			for _, p := range points {
				if (entry.Value == semconv.Int) != (p.AsInt != nil && p.AsDouble == nil) {
					t.Errorf("%s: a %v point is encoded as %+v", m.Name, entry.Value, p)
				}
				attrs = append(attrs, p.Attributes)
			}
			if m.Histogram != nil {
				for _, p := range m.Histogram.DataPoints {
					attrs = append(attrs, p.Attributes)
				}
			}
			for _, kvs := range attrs {
				for _, kv := range kvs {
					if !entry.HasAttribute(kv.Key) {
						t.Errorf("%s carries %s, which the table does not list", m.Name, kv.Key)
					}
				}
			}
		}
	}
	events := map[string]semconv.Event{}
	for _, e := range semconv.Events() {
		events[e.Name] = e
	}
	// The table lists these first for every job event, as always present;
	// the Kubernetes job and pod names and the pod UID follow once known.
	jobIdentity := []string{semconv.AttrSimJobID, semconv.AttrSimWorkloadID, semconv.AttrSimWorkloadType,
		semconv.AttrSimPodRole, semconv.AttrJoulieWorkloadClass, semconv.AttrK8sNamespaceName}
	for _, d := range logs {
		for _, r := range d.ResourceLogs[0].ScopeLogs[0].LogRecords {
			e, ok := events[r.EventName]
			if !ok {
				t.Errorf("eventName %q is not in the semconv table", r.EventName)
				continue
			}
			failed := false
			for _, kv := range r.Attributes {
				if !e.HasAttribute(kv.Key) {
					t.Errorf("%s carries %s, which the table does not list", r.EventName, kv.Key)
				}
				if kv.Key == semconv.AttrErrorType && *kv.Value.String == semconv.ErrorTypeAssertionFailed {
					failed = true
				}
			}
			wantNum, wantText := e.SeverityNumber, e.SeverityText
			if failed {
				wantNum, wantText = semconv.SeverityError, semconv.SeverityTextError
			}
			if r.SeverityNumber != wantNum || r.SeverityText != wantText {
				t.Errorf("%s: severity %d %s, want %d %s", r.EventName, r.SeverityNumber, r.SeverityText, wantNum, wantText)
			}
			if !strings.HasPrefix(r.EventName, "joulie.sim.job.") {
				continue
			}
			have := map[string]bool{}
			for _, kv := range r.Attributes {
				have[kv.Key] = true
			}
			for _, a := range jobIdentity {
				if !have[a] {
					t.Errorf("%s lacks %s, which every job event carries", r.EventName, a)
				}
			}
		}
	}

	// The goldens are compared last, so that a change that breaks a check
	// above reports that check and not only a line diff, and -update never
	// writes a golden that fails them.
	if *buildUpdateGolden && t.Failed() {
		t.Fatal("the goldens are not rewritten while the checks above fail")
	}
	buildCompareGolden(t, buildGoldenMetricsFile, mb)
	buildCompareGolden(t, buildGoldenLogsFile, lb)
}

// OJ08: a histogram point's count, sum and bucketCounts survive the
// encoding; bucketCounts has one more element than explicitBounds, a value
// equal to a bound counts in that bound's bucket, and the counts add up to
// the count. The histogram keeps its own bounds: neither the caller's slice
// nor a point's explicitBounds shares them.
func TestOJ08HistogramRoundTrips(t *testing.T) {
	bounds := []float64{1, 5, 10}
	h := NewExplicitHistogram(bounds...)
	bounds[0] = 100 // the caller reuses its slice
	// Neither extreme comes first or last, so min and max are found by
	// comparison, not taken from the first or the latest value.
	values := []float64{5, 0.5, 1, 250, 1.25, 11, 10, 7}
	sum := 0.0
	for _, v := range values {
		if !h.Record(v) {
			t.Fatalf("Record(%v) refused a finite value", v)
		}
		sum += v
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if h.Record(v) {
			t.Errorf("Record(%v) accepted a value no sum can hold", v)
		}
	}
	class := String(semconv.AttrJoulieWorkloadClass, "performance")
	p := h.Point(buildT0, buildAt(60), class)
	h.Record(3) // a later value must not reach the point already taken
	md := NewMetricsData(Resource{}, Scope{Name: "test"}, HistogramMetric(semconv.JoulieSimWorkloadJobDuration, p))

	b, err := json.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"count":"8"`, fmt.Sprintf(`"sum":%v`, sum), `"bucketCounts":["2","2","2","2"]`, `"explicitBounds":[1,5,10]`,
		`"min":0.5`, `"max":250`, `"aggregationTemporality":2`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("marshal lacks %s\n%s", want, b)
		}
	}
	back, err := buildDecodeMetricsStrict(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, md) {
		t.Fatalf("round trip changed the histogram:\n got %+v\nwant %+v", back, md)
	}
	got := back.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Histogram.DataPoints[0]
	if got.Count != 8 || got.Sum == nil || *got.Sum != sum {
		t.Errorf("count %d sum %v, want 8 and %v", got.Count, got.Sum, sum)
	}
	if len(got.BucketCounts) != len(got.ExplicitBounds)+1 {
		t.Errorf("%d bucket counts for %d bounds", len(got.BucketCounts), len(got.ExplicitBounds))
	}
	var total Uint64String
	for _, c := range got.BucketCounts {
		total += c
	}
	if total != got.Count {
		t.Errorf("bucket counts add up to %d, count is %d", total, got.Count)
	}

	// The aggregation is cumulative: the next point holds every value. A
	// change to the point already taken moves no bound, so 2 still lands in
	// (1,5].
	p.ExplicitBounds[0] = 4
	h.Record(2)
	if next := h.Point(buildT0, buildAt(70)); next.Count != 10 || *next.Sum != sum+5 || next.BucketCounts[1] != 4 ||
		!reflect.DeepEqual(next.ExplicitBounds, []float64{1, 5, 10}) {
		t.Errorf("the next point is %+v, want count 10 with 4 in (1,5] and bounds [1 5 10]", next)
	}

	// No value yet: zero counts in every bucket, a zero sum, no min or max.
	empty := NewExplicitHistogram(1, 2).Point(buildT0, buildAt(1))
	if empty.Count != 0 || !reflect.DeepEqual(empty.BucketCounts, []Uint64String{0, 0, 0}) || empty.Sum == nil || *empty.Sum != 0 ||
		empty.Min != nil || empty.Max != nil {
		t.Errorf("empty histogram point is %+v", empty)
	}
	// No bounds: one bucket for everything, and no explicitBounds key.
	one := NewExplicitHistogram()
	one.Record(3)
	if p := one.Point(buildT0, buildAt(1)); !reflect.DeepEqual(p.BucketCounts, []Uint64String{1}) || p.ExplicitBounds != nil {
		t.Errorf("histogram without bounds gives %+v", p)
	}
	// A negative value drops the sum, as opentelemetry-proto asks.
	neg := NewExplicitHistogram(0)
	neg.Record(2)
	neg.Record(-1)
	if p := neg.Point(buildT0, buildAt(1)); p.Sum != nil || p.Count != 2 || *p.Min != -1 {
		t.Errorf("histogram with a negative value gives %+v", p)
	}

	// HistogramMetric keeps only points that follow the bucket rules.
	f := func(v float64) *float64 { return &v }
	valid := HistogramDataPoint{Count: 3, Sum: f(6), BucketCounts: []Uint64String{1, 2}, ExplicitBounds: []float64{2}}
	invalid := []HistogramDataPoint{
		{Count: 4, Sum: f(6), BucketCounts: []Uint64String{1, 2}, ExplicitBounds: []float64{2}},
		{Count: 3, Sum: f(6), BucketCounts: []Uint64String{1, 2}, ExplicitBounds: []float64{2, 4}},
		{Count: 3, Sum: f(6), BucketCounts: []Uint64String{1, 1, 1}, ExplicitBounds: []float64{4, 2}},
		{Count: 3, Sum: f(math.NaN()), BucketCounts: []Uint64String{1, 2}, ExplicitBounds: []float64{2}},
		{Count: 3, Max: f(math.Inf(1)), BucketCounts: []Uint64String{1, 2}, ExplicitBounds: []float64{2}},
		{Count: 3, ExplicitBounds: []float64{2}},
	}
	m := HistogramMetric(semconv.JoulieSimWorkloadJobDuration, append([]HistogramDataPoint{valid}, invalid...)...)
	if len(m.Histogram.DataPoints) != 1 || !reflect.DeepEqual(m.Histogram.DataPoints[0], valid) {
		t.Errorf("HistogramMetric kept %+v, want only the valid point", m.Histogram.DataPoints)
	}
	for _, bounds := range [][]float64{{2, 1}, {1, 1}, {math.NaN()}, {0, math.Inf(1)}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewExplicitHistogram(%v) did not panic", bounds)
				}
			}()
			NewExplicitHistogram(bounds...)
		}()
	}
}

// OJ09: the strict decoder reads back every golden line, unchanged, and the
// number of points and records per line equals the number passed to the
// builders.
func TestOJ09StrictDecodeReadsEveryGoldenLine(t *testing.T) {
	// The decoder used here must be strict, or this test proves nothing.
	// encoding/json alone matches keys without regard to case, so a key
	// spelled in another case must fail too.
	for _, bad := range []string{
		`{"resource_metrics":[]}`,
		`{"ResourceMetrics":[]}`,
		`{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"x","gauge":{"data_points":[]}}]}]}]}`,
		`{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"Name":"x"}]}]}]}`,
		`{"resourceMetrics":[]} {"resourceMetrics":[]}`,
	} {
		if _, err := buildDecodeMetricsStrict([]byte(bad)); err == nil {
			t.Fatalf("the strict decoder accepted %s", bad)
		}
	}
	for _, bad := range []string{
		`{"resource_logs":[]}`,
		`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"EventName":"x"}]}]}]}`,
	} {
		if _, err := buildDecodeLogsStrict([]byte(bad)); err == nil {
			t.Fatalf("the strict decoder accepted %s", bad)
		}
	}

	metrics, points := buildGoldenMetrics()
	lines := buildReadLines(t, buildGoldenMetricsFile)
	if len(lines) != len(metrics) {
		t.Fatalf("%s has %d lines, the builders wrote %d", buildGoldenMetricsFile, len(lines), len(metrics))
	}
	for i, line := range lines {
		got, err := buildDecodeMetricsStrict(line)
		if err != nil {
			t.Fatalf("%s line %d: %v", buildGoldenMetricsFile, i+1, err)
		}
		if n := buildCountPoints(got); n != points[i] || n != buildCountPoints(metrics[i]) {
			t.Errorf("%s line %d: %d points decoded, %d encoded, %d passed to the builders",
				buildGoldenMetricsFile, i+1, n, buildCountPoints(metrics[i]), points[i])
		}
		if !reflect.DeepEqual(got, metrics[i]) {
			t.Errorf("%s line %d decodes to\n%+v\nwant\n%+v", buildGoldenMetricsFile, i+1, got, metrics[i])
		}
	}

	logs, records := buildGoldenLogs()
	lines = buildReadLines(t, buildGoldenLogsFile)
	if len(lines) != len(logs) {
		t.Fatalf("%s has %d lines, the builders wrote %d", buildGoldenLogsFile, len(lines), len(logs))
	}
	for i, line := range lines {
		got, err := buildDecodeLogsStrict(line)
		if err != nil {
			t.Fatalf("%s line %d: %v", buildGoldenLogsFile, i+1, err)
		}
		if n := buildCountRecords(got); n != records[i] || n != buildCountRecords(logs[i]) {
			t.Errorf("%s line %d: %d records decoded, %d encoded, %d passed to the builders",
				buildGoldenLogsFile, i+1, n, buildCountRecords(logs[i]), records[i])
		}
		if !reflect.DeepEqual(got, logs[i]) {
			t.Errorf("%s line %d decodes to\n%+v\nwant\n%+v", buildGoldenLogsFile, i+1, got, logs[i])
		}
	}
}

// The table's value type decides the encoding of a point: an Int metric
// rounds a double to asInt, a Double metric widens an int to asDouble, and a
// point OTLP JSON cannot carry is left out rather than failing the export.
func TestBuildNumberPointsFollowTheTableValueType(t *testing.T) {
	at := buildAt(1)
	// An int64 holds -2^63 but not 2^63, and both are exact doubles: the
	// first is kept, the second and anything beyond either end is left out.
	m := GaugeMetric(semconv.HwCPUSpeed, // Int
		DoublePoint(time.Time{}, at, 2.4999996e9), DoublePoint(time.Time{}, at, -2.5), IntPoint(time.Time{}, at, 7),
		DoublePoint(time.Time{}, at, math.NaN()), DoublePoint(time.Time{}, at, math.Inf(1)), DoublePoint(time.Time{}, at, 1e19),
		DoublePoint(time.Time{}, at, -1e19), DoublePoint(time.Time{}, at, 9223372036854775808.0),
		DoublePoint(time.Time{}, at, -9223372036854775808.0),
		NumberDataPoint{TimeUnixNano: 1})
	var ints []int64
	for _, p := range m.Gauge.DataPoints {
		if p.AsDouble != nil || p.AsInt == nil {
			t.Fatalf("Int metric point encoded as %+v", p)
		}
		ints = append(ints, int64(*p.AsInt))
	}
	if want := []int64{2499999600, -3, 7, math.MinInt64}; !reflect.DeepEqual(ints, want) {
		t.Errorf("Int metric points are %v, want %v", ints, want)
	}

	d := SumMetric(semconv.JoulieSimWorkloadEnergy, IntPoint(buildT0, at, 5), DoublePoint(buildT0, at, 0)) // Double
	if len(d.Sum.DataPoints) != 2 || *d.Sum.DataPoints[0].AsDouble != 5 || d.Sum.DataPoints[0].AsInt != nil ||
		*d.Sum.DataPoints[1].AsDouble != 0 {
		t.Errorf("Double metric points are %+v", d.Sum.DataPoints)
	}

	// Both values set: the declared type's wins.
	i, f := Int64String(4), 4.5
	both := NumberDataPoint{AsInt: &i, AsDouble: &f}
	if p := GaugeMetric(semconv.HwCPUSpeed, both).Gauge.DataPoints[0]; p.AsDouble != nil || *p.AsInt != 4 {
		t.Errorf("Int metric kept %+v", p)
	}
	if p := GaugeMetric(semconv.JoulieScenarioAssertionValue, both).Gauge.DataPoints[0]; p.AsInt != nil || *p.AsDouble != 4.5 {
		t.Errorf("Double metric kept %+v", p)
	}

	// Nothing left: the metric still encodes, with an empty list.
	empty := SumMetric(semconv.JoulieSimWorkloadWork, DoublePoint(buildT0, at, math.NaN()))
	b, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"dataPoints":[]`) {
		t.Errorf("empty sum encodes as %s", b)
	}

	// A metric outside the table, with zero Kind and Value, keeps its points.
	capture := semconv.Metric{Name: semconv.CaptureMetricPrefix + "node_power", Unit: "W"}
	g := GaugeMetric(capture, IntPoint(time.Time{}, at, 3), DoublePoint(time.Time{}, at, 3.5))
	if p := g.Gauge.DataPoints; len(p) != 2 || p[0].AsInt == nil || p[1].AsDouble == nil {
		t.Errorf("untyped gauge points are %+v", p)
	}
	if s := SumMetric(capture, IntPoint(buildT0, at, 1)); s.Sum.IsMonotonic {
		t.Errorf("a sum of unknown kind is marked monotonic")
	}
}

// An Int counter rounds a double down, as a whole-joule reading of a finer
// counter does and as the simulator's telemetry encoder writes it, so the
// same reading encodes to the same asInt through either path. An up-down
// counter, like a gauge, rounds to the nearest.
func TestBuildIntCounterRoundsDown(t *testing.T) {
	at := buildAt(1)
	ints := func(ps []NumberDataPoint) []int64 {
		var out []int64
		for _, p := range ps {
			if p.AsDouble != nil || p.AsInt == nil {
				t.Fatalf("Int metric point encoded as %+v", p)
			}
			out = append(out, int64(*p.AsInt))
		}
		return out
	}
	c := SumMetric(semconv.HwHostEnergy, // Counter, Int
		DoublePoint(buildT0, at, 1234.9), DoublePoint(buildT0, at, 1234.5), DoublePoint(buildT0, at, 7), IntPoint(buildT0, at, 9))
	if got, want := ints(c.Sum.DataPoints), []int64{1234, 1234, 7, 9}; !reflect.DeepEqual(got, want) {
		t.Errorf("Int counter points are %v, want %v", got, want)
	}
	u := SumMetric(semconv.JoulieSimWorkloadJobsActive, // UpDownCounter, Int
		DoublePoint(buildT0, at, 2.5), DoublePoint(buildT0, at, 2.4), DoublePoint(buildT0, at, -2.5))
	if got, want := ints(u.Sum.DataPoints), []int64{3, 2, -3}; !reflect.DeepEqual(got, want) {
		t.Errorf("Int up-down counter points are %v, want %v", got, want)
	}
}

// Sums are cumulative; only a Counter is monotonic. A builder given the
// wrong instrument panics, as semconv.Register does on an invalid entry.
func TestBuildMetricKindMismatchPanics(t *testing.T) {
	at := buildAt(1)
	if s := SumMetric(semconv.JoulieSimWorkloadJobsActive, IntPoint(buildT0, at, 1)).Sum; s.IsMonotonic ||
		s.AggregationTemporality != AggregationTemporalityCumulative {
		t.Errorf("UpDownCounter encodes as %+v", s)
	}
	if s := SumMetric(semconv.JoulieSimWorkloadJobsSubmitted, IntPoint(buildT0, at, 1)).Sum; !s.IsMonotonic ||
		s.AggregationTemporality != AggregationTemporalityCumulative {
		t.Errorf("Counter encodes as %+v", s)
	}
	for name, build := range map[string]func(){
		"SumMetric(gauge)":         func() { SumMetric(semconv.JoulieScenarioAssertionValue) },
		"SumMetric(histogram)":     func() { SumMetric(semconv.JoulieSimWorkloadJobDuration) },
		"GaugeMetric(counter)":     func() { GaugeMetric(semconv.JoulieSimWorkloadEnergy) },
		"HistogramMetric(counter)": func() { HistogramMetric(semconv.JoulieSimWorkloadJobsSubmitted) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", name)
				}
			}()
			build()
		}()
	}
}

// An event record takes its name and severity from the table, its times
// from the caller (observed defaults to the occurrence), and leaves out an
// empty body.
func TestBuildEventRecord(t *testing.T) {
	at := buildAt(5)
	r := EventRecord(semconv.EventSimJobStalled, at, time.Time{}, "", String(semconv.AttrSimStallReason, semconv.StallReasonNoProgress))
	if r.EventName != semconv.EventSimJobStalled.Name || r.SeverityNumber != semconv.SeverityWarn || r.SeverityText != semconv.SeverityTextWarn {
		t.Errorf("record is %+v", r)
	}
	if r.TimeUnixNano != Uint64String(at.UnixNano()) || r.ObservedTimeUnixNano != r.TimeUnixNano || r.Body != nil {
		t.Errorf("record times or body are %+v", r)
	}
	if r := EventRecord(semconv.EventSimRunStarted, at, buildAt(6), "started"); r.ObservedTimeUnixNano != Uint64String(buildAt(6).UnixNano()) ||
		r.Body == nil || *r.Body.String != "started" || r.Attributes != nil {
		t.Errorf("record is %+v", r)
	}
}

// Builders copy the caller's attributes, so one slice can be reused for the
// next point; times before the epoch, or zero, are left unset.
func TestBuildPointsCopyAttributesAndTimes(t *testing.T) {
	attrs := []KeyValue{String(semconv.AttrK8sNodeName, "a")}
	p := DoublePoint(time.Time{}, time.Unix(-1, 0), 1, attrs...)
	h := NewExplicitHistogram(1)
	hp := h.Point(time.Time{}, buildT0, attrs...)
	r := EventRecord(semconv.EventSimRunStarted, buildT0, time.Time{}, "", attrs...)
	attrs[0] = String(semconv.AttrK8sNodeName, "b")
	for _, got := range [][]KeyValue{p.Attributes, hp.Attributes, r.Attributes} {
		if *got[0].Value.String != "a" {
			t.Errorf("a builder kept the caller's slice: %+v", got)
		}
	}
	if p.StartTimeUnixNano != 0 || p.TimeUnixNano != 0 || hp.StartTimeUnixNano != 0 {
		t.Errorf("zero or pre-epoch times encode as %d, %d, %d", p.StartTimeUnixNano, p.TimeUnixNano, hp.StartTimeUnixNano)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "UnixNano") {
		t.Errorf("unset times are written: %s", b)
	}
}

// A double attribute that is NaN or infinite, alone or in an array, would
// make encoding/json refuse the whole line: every builder leaves that
// attribute out and keeps the others, in order.
func TestBuildDropsNonFiniteAttributes(t *testing.T) {
	keep := String(semconv.AttrK8sNodeName, "a")
	nested := KeyValue{Key: "nested", Value: AnyValue{Array: &ArrayValue{Values: []AnyValue{
		{Array: &ArrayValue{Values: []AnyValue{Double("", 1).Value, Double("", math.Inf(-1)).Value}}},
	}}}}
	nonFinite := []KeyValue{
		Double(semconv.AttrJoulieAssertionObserved, math.NaN()),
		Double(semconv.AttrSimJobQueueWait, math.Inf(1)),
		{Key: "array", Value: AnyValue{Array: &ArrayValue{Values: []AnyValue{Double("", 2).Value, Double("", math.NaN()).Value}}}},
		nested,
	}
	finite := Double(semconv.AttrSimJobEnergy, 7.5)
	bad := func(extra ...KeyValue) []KeyValue {
		return append(append([]KeyValue{nonFinite[0], keep}, nonFinite[1:]...), extra...)
	}
	want := []KeyValue{keep, finite}

	h := NewExplicitHistogram(1)
	h.Record(0.5)
	md := NewMetricsData(Resource{Attributes: bad(finite)}, Scope{Name: "s", Attributes: bad(finite)},
		GaugeMetric(semconv.JoulieScenarioAssertionValue, DoublePoint(time.Time{}, buildT0, 1, bad(finite)...)),
		SumMetric(semconv.JoulieSimWorkloadJobsSubmitted, IntPoint(buildT0, buildT0, 1, bad(finite)...)),
		HistogramMetric(semconv.JoulieSimWorkloadJobDuration, h.Point(buildT0, buildT0, bad(finite)...)))
	ld := NewLogsData(Resource{Attributes: bad(finite)}, Scope{Name: "s", Attributes: bad(finite)},
		EventRecord(semconv.EventScenarioAssertionEvaluated, buildT0, time.Time{}, "", bad(finite)...),
		EventRecord(semconv.EventSimRunStarted, buildT0, time.Time{}, "", nonFinite...))

	rm, rl := md.ResourceMetrics[0], ld.ResourceLogs[0]
	ms, lr := rm.ScopeMetrics[0].Metrics, rl.ScopeLogs[0].LogRecords
	for name, got := range map[string][]KeyValue{
		"metrics resource": rm.Resource.Attributes, "metrics scope": rm.ScopeMetrics[0].Scope.Attributes,
		"gauge point": ms[0].Gauge.DataPoints[0].Attributes, "sum point": ms[1].Sum.DataPoints[0].Attributes,
		"histogram point": ms[2].Histogram.DataPoints[0].Attributes,
		"logs resource":   rl.Resource.Attributes, "logs scope": rl.ScopeLogs[0].Scope.Attributes,
		"event record": lr[0].Attributes,
	} {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s attributes are %+v, want %+v", name, got, want)
		}
	}
	if lr[1].Attributes != nil {
		t.Errorf("a record whose every attribute is left out keeps %+v, want none", lr[1].Attributes)
	}
	for _, v := range []any{md, ld} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("a non-finite attribute fails the export: %v", err)
		}
		if strings.Contains(string(b), semconv.AttrJoulieAssertionObserved) || strings.Contains(string(b), `"array"`) ||
			strings.Contains(string(b), `"nested"`) {
			t.Errorf("a non-finite attribute is written: %s", b)
		}
	}
}

// Array attributes encode as arrayValue, an empty list as an empty array.
func TestBuildArrayAttributes(t *testing.T) {
	for _, c := range []struct {
		kv   KeyValue
		want string
	}{
		{IntArray("k", 0, -1), `{"key":"k","value":{"arrayValue":{"values":[{"intValue":"0"},{"intValue":"-1"}]}}}`},
		{IntArray("k"), `{"key":"k","value":{"arrayValue":{"values":[]}}}`},
		{StringArray("k", "a", ""), `{"key":"k","value":{"arrayValue":{"values":[{"stringValue":"a"},{"stringValue":""}]}}}`},
		{StringArray("k"), `{"key":"k","value":{"arrayValue":{"values":[]}}}`},
	} {
		b, err := json.Marshal(c.kv)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("got %s, want %s", b, c.want)
		}
	}
}

// One-resource exports pin the semconv schema on the resource and the scope,
// and write an empty list rather than null.
func TestBuildExportsPinSchemaURL(t *testing.T) {
	md := NewMetricsData(Resource{}, Scope{Name: "s"})
	ld := NewLogsData(Resource{}, Scope{Name: "s"})
	if md.ResourceMetrics[0].SchemaURL != semconv.SchemaURL || md.ResourceMetrics[0].ScopeMetrics[0].SchemaURL != semconv.SchemaURL ||
		ld.ResourceLogs[0].SchemaURL != semconv.SchemaURL || ld.ResourceLogs[0].ScopeLogs[0].SchemaURL != semconv.SchemaURL {
		t.Errorf("schemaUrl not pinned: %+v %+v", md, ld)
	}
	for _, v := range []any{md, ld} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "null") {
			t.Errorf("empty export writes null: %s", b)
		}
	}
}
