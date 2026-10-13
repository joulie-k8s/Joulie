package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// NoData is the message of a check with nothing to evaluate. Such a check
// fails, so an assertion can never pass vacuously.
const NoData = "no data"

// ReadEffectiveScenario reads ScenarioEffectiveFile from the variant
// directory dir or, for a named variant, from the run directory above it.
func ReadEffectiveScenario(dir string) (Scenario, string, error) {
	for _, d := range []string{dir, filepath.Dir(filepath.Clean(dir))} {
		path := filepath.Join(d, ScenarioEffectiveFile)
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Scenario{}, "", err
		}
		var s Scenario
		if err := yaml.UnmarshalStrict(b, &s); err != nil {
			return Scenario{}, "", fmt.Errorf("%s: %w", path, err)
		}
		return s, d, nil
	}
	return Scenario{}, "", fmt.Errorf("no %s in %s or its parent", ScenarioEffectiveFile, dir)
}

// ReadVariantEnv reads VariantEnvFile of the variant directory dir.
func ReadVariantEnv(dir string) (VariantEnv, error) {
	b, err := os.ReadFile(filepath.Join(dir, VariantEnvFile))
	if err != nil {
		return VariantEnv{}, err
	}
	return ParseVariantEnv(b)
}

// ReadRunData reads the variant directory dir: its variant.env, the
// effective scenario with the variant applied, run-status.json when the run
// started, every record of the simulator's and the harness's OTLP files
// except what an earlier evaluate wrote (dropEvaluations), and the
// Kubernetes snapshots. A missing results file leaves its part
// empty, so the checks that need it report no data; a file that does not
// decode is an error.
func ReadRunData(dir string) (*RunData, VariantEnv, error) {
	env, err := ReadVariantEnv(dir)
	if err != nil {
		return nil, env, err
	}
	s, _, err := ReadEffectiveScenario(dir)
	if err != nil {
		return nil, env, err
	}
	rd := &RunData{Dir: dir, Variant: env.Variant, RunID: env.RunID}
	if rd.Scenario, err = s.ForVariant(env.Variant); err != nil {
		return nil, env, err
	}
	if b, err := os.ReadFile(filepath.Join(dir, RunStatusFile)); err == nil {
		var st workload.RunStatus
		if err := json.Unmarshal(b, &st); err != nil {
			return nil, env, fmt.Errorf("%s: %w", RunStatusFile, err)
		}
		rd.Status = &st
		rd.Start = st.StartedAt
		rd.End = st.StartedAt.Add(time.Duration(st.ElapsedSeconds * float64(time.Second)))
		if rd.RunID == "" {
			rd.RunID = st.RunID
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, env, err
	}
	for _, f := range []string{workload.SimulatorMetricsFile, HarnessMetricsFile} {
		if err := readMetricsFile(rd, filepath.Join(workload.OTLPDir, f)); err != nil {
			return nil, env, err
		}
	}
	for _, f := range []string{workload.SimulatorLogsFile, HarnessLogsFile} {
		if err := readLogsFile(rd, filepath.Join(workload.OTLPDir, f)); err != nil {
			return nil, env, err
		}
	}
	dropEvaluations(rd)
	sort.SliceStable(rd.Events, func(i, j int) bool { return rd.Events[i].Time.Before(rd.Events[j].Time) })
	if rd.KubeStart, err = ReadKubeSnapshot(dir, SnapshotStart); err != nil {
		return nil, env, err
	}
	if rd.KubeEnd, err = ReadKubeSnapshot(dir, SnapshotEnd); err != nil {
		return nil, env, err
	}
	return rd, env, nil
}

// evaluationEvents are the records evaluate appends to HarnessLogsFile.
var evaluationEvents = map[string]bool{
	semconv.EventScenarioAssertionEvaluated.Name: true,
	semconv.EventScenarioFinished.Name:           true,
}

// dropEvaluations removes from rd what an earlier evaluate appended to the
// harness files: its evaluationEvents and its assertion value points, which
// are all but the observed values of promql assertions that run records.
// Evaluating a directory again then reads the run's records only, and
// gives the same results.
func dropEvaluations(rd *RunData) {
	promql := map[string]bool{}
	for _, a := range rd.Scenario.Assertions {
		if a.Type == AssertPromQL {
			promql[a.Name] = true
		}
	}
	events := rd.Events[:0]
	for _, e := range rd.Events {
		if !evaluationEvents[e.Name] {
			events = append(events, e)
		}
	}
	rd.Events = events
	points := rd.Points[:0]
	for _, p := range rd.Points {
		if p.Metric == semconv.JoulieScenarioAssertionValue.Name {
			name, _ := p.Attributes.String(semconv.AttrJoulieAssertionName)
			src, _ := p.Attributes.String(semconv.AttrJoulieAssertionSource)
			if src != semconv.AssertionSourceObserved || !promql[name] {
				continue
			}
		}
		points = append(points, p)
	}
	rd.Points = points
}

// readJSONL calls fn with each line of the OTLP file rel under rd.Dir; a
// missing file is skipped.
func readJSONL(rd *RunData, rel string, fn func(line int, b []byte) error) error {
	f, err := os.Open(filepath.Join(rd.Dir, rel))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := scanJSONLines(f, fn); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	return nil
}

func readMetricsFile(rd *RunData, rel string) error {
	return readJSONL(rd, rel, func(line int, b []byte) error {
		var md otlpjson.MetricsData
		if err := json.Unmarshal(b, &md); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		rd.Points = append(rd.Points, FlattenMetrics(md, rel)...)
		return nil
	})
}

func readLogsFile(rd *RunData, rel string) error {
	return readJSONL(rd, rel, func(line int, b []byte) error {
		var ld otlpjson.LogsData
		if err := json.Unmarshal(b, &ld); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		rd.Events = append(rd.Events, FlattenLogs(ld, rel)...)
		return nil
	})
}

// FlattenMetrics returns every data point of md, read from file.
func FlattenMetrics(md otlpjson.MetricsData, file string) []Point {
	var out []Point
	for _, rm := range md.ResourceMetrics {
		res := attributes(rm.Resource.Attributes)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				base := Point{Metric: m.Name, Unit: m.Unit, File: file, Resource: res}
				var points []otlpjson.NumberDataPoint
				switch {
				case m.Sum != nil:
					base.Kind = semconv.UpDownCounter
					if m.Sum.IsMonotonic {
						base.Kind = semconv.Counter
					}
					points = m.Sum.DataPoints
				case m.Gauge != nil:
					base.Kind = semconv.Gauge
					points = m.Gauge.DataPoints
				case m.Histogram != nil:
					base.Kind = semconv.Histogram
					for _, hp := range m.Histogram.DataPoints {
						p := base
						p.StartTime, p.Time = unixNano(hp.StartTimeUnixNano), unixNano(hp.TimeUnixNano)
						p.Attributes = attributes(hp.Attributes)
						h := &HistogramValue{Count: uint64(hp.Count), Bounds: hp.ExplicitBounds}
						if hp.Sum != nil {
							h.Sum = *hp.Sum
						}
						for _, c := range hp.BucketCounts {
							h.Counts = append(h.Counts, uint64(c))
						}
						p.Histogram = h
						out = append(out, p)
					}
				}
				for _, np := range points {
					p := base
					p.StartTime, p.Time = unixNano(np.StartTimeUnixNano), unixNano(np.TimeUnixNano)
					p.Attributes = attributes(np.Attributes)
					switch {
					case np.AsDouble != nil:
						p.Value = *np.AsDouble
					case np.AsInt != nil:
						p.Value = float64(*np.AsInt)
					}
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// FlattenLogs returns every record of ld with an eventName, read from file.
func FlattenLogs(ld otlpjson.LogsData, file string) []Event {
	var out []Event
	for _, rl := range ld.ResourceLogs {
		res := attributes(rl.Resource.Attributes)
		for _, sl := range rl.ScopeLogs {
			for _, r := range sl.LogRecords {
				if r.EventName == "" {
					continue
				}
				e := Event{
					Name: r.EventName, Time: unixNano(r.TimeUnixNano), ObservedTime: unixNano(r.ObservedTimeUnixNano),
					Severity: r.SeverityNumber, File: file, Resource: res, Attributes: attributes(r.Attributes),
				}
				if r.Body != nil && r.Body.String != nil {
					e.Body = *r.Body.String
				}
				out = append(out, e)
			}
		}
	}
	return out
}

func unixNano(v otlpjson.Uint64String) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(v)).UTC()
}

func attributes(kvs []otlpjson.KeyValue) Attributes {
	out := Attributes{}
	for _, kv := range kvs {
		if v, ok := anyValue(kv.Value); ok {
			out[kv.Key] = v
		}
	}
	return out
}

func anyValue(v otlpjson.AnyValue) (any, bool) {
	switch {
	case v.String != nil:
		return *v.String, true
	case v.Bool != nil:
		return *v.Bool, true
	case v.Int != nil:
		return int64(*v.Int), true
	case v.Double != nil:
		return *v.Double, true
	case v.Array != nil:
		out := make([]any, 0, len(v.Array.Values))
		for _, x := range v.Array.Values {
			if y, ok := anyValue(x); ok {
				out = append(out, y)
			}
		}
		return out, true
	}
	return nil, false
}

// observation is what one check of an assertion saw: Value is nil when
// there was no data, and Msg explains the value.
type observation struct {
	Value *float64
	Msg   string
}

func observed(v float64, format string, args ...any) observation {
	return observation{Value: &v, Msg: fmt.Sprintf(format, args...)}
}

func noData(format string, args ...any) observation {
	msg := NoData
	if format != "" {
		msg += ": " + fmt.Sprintf(format, args...)
	}
	return observation{Msg: msg}
}

// observers return the observation of each check of an assertion, keyed by
// the check's key: "" for an assertion with one comparator, the expect key
// (such as completedFraction) otherwise.
var observers = map[AssertionType]func(ev *evaluation, a Assertion) map[string]observation{
	AssertJobs:      observeJobs,
	AssertLatency:   observeLatency,
	AssertStall:     observeStall,
	AssertPlacement: observePlacement,
	AssertEnergy:    observeEnergy,
	AssertEvents:    observeEvents,
	AssertObjects:   observeObjects,
	AssertTwinPower: observeTwinPower,
	AssertPromQL:    observePromQL,
}

// evaluation is one variant's data with the indexes the observers share.
type evaluation struct {
	rd   *RunData
	env  VariantEnv
	jobs []*jobState
}

// Evaluate evaluates every check of every assertion of rd.Scenario against
// rd, in order, with the defaults env supplies. A check passes when it has
// data and its comparator holds.
func Evaluate(rd *RunData, env VariantEnv) []AssertionResult {
	ev := &evaluation{rd: rd, env: env, jobs: indexJobs(rd)}
	var out []AssertionResult
	for _, a := range rd.Scenario.Assertions {
		obs := map[string]observation{}
		if f := observers[a.Type]; f != nil && a.Spec() != nil {
			obs = f(ev, a)
		}
		sev := a.Severity
		if sev == "" {
			sev = SeverityFail
		}
		for _, c := range a.Checks() {
			key := strings.TrimPrefix(strings.TrimPrefix(c.Name, a.Name), ".")
			o, ok := obs[key]
			if !ok {
				o = noData("")
			}
			r := AssertionResult{Name: c.Name, Type: a.Type, Severity: sev, Observed: o.Value, Expected: string(c.Expect),
				Message: o.Msg, Status: semconv.TestCaseResultStatusFail}
			e, err := c.Expect.Parse()
			switch {
			case err != nil:
				r.Message = err.Error()
			case o.Value != nil && e.Holds(*o.Value):
				r.Status = semconv.TestCaseResultStatusPass
			}
			out = append(out, r)
		}
	}
	return out
}

// jobState is one trace job as the run's records tell it.
type jobState struct {
	ID, WorkloadID, WorkloadType, Class string
	Offset                              float64
	Due                                 bool
	// Outcome is the job's terminal event: completed, failed, incomplete,
	// or "" when the records end before one.
	Outcome      string
	Scheduled    bool
	Node         string
	NodeProfile  string
	NodeClass    string
	ClassSince   float64
	QueueWait    *float64
	Duration     *float64
	RunTime      *float64
	IdealRunTime *float64
	Stalled      bool
}

// indexJobs builds one jobState per trace job of the variant directory, in
// trace order, plus any job the events name that the trace does not. A job
// is due when its offset is inside the submission window and, once the run
// has a status, inside the run's elapsed time: a run that ended early never
// submitted the later jobs.
func indexJobs(rd *RunData) []*jobState {
	var out []*jobState
	byID := map[string]*jobState{}
	window := rd.Scenario.Duration.Duration.Seconds()
	elapsed := math.Inf(1)
	if rd.Status != nil {
		elapsed = rd.Status.ElapsedSeconds
	}
	if trace, err := ReadTrace(filepath.Join(rd.Dir, workload.TraceFile)); err == nil {
		for _, t := range trace {
			j := &jobState{ID: t.JobID, WorkloadID: t.WorkloadID, WorkloadType: t.WorkloadType, Class: t.IntentClass,
				Offset: t.SubmitTimeOffsetSec}
			j.Due = j.Offset < window && j.Offset <= elapsed
			if _, dup := byID[j.ID]; !dup {
				byID[j.ID] = j
				out = append(out, j)
			}
		}
	}
	for _, e := range rd.Events {
		id, ok := e.Attributes.String(semconv.AttrSimJobID)
		if !ok {
			continue
		}
		j := byID[id]
		if j == nil {
			j = &jobState{ID: id, Due: true}
			j.WorkloadID, _ = e.Attributes.String(semconv.AttrSimWorkloadID)
			j.WorkloadType, _ = e.Attributes.String(semconv.AttrSimWorkloadType)
			byID[id] = j
			out = append(out, j)
		}
		if j.Class == "" {
			j.Class, _ = e.Attributes.String(semconv.AttrJoulieWorkloadClass)
		}
		switch e.Name {
		case semconv.EventSimJobScheduled.Name:
			j.Scheduled = true
			j.Node, _ = e.Attributes.String(semconv.AttrK8sNodeName)
			j.NodeProfile, _ = e.Attributes.String(semconv.AttrJoulieNodePowerProfile)
			j.NodeClass, _ = e.Attributes.String(semconv.AttrJoulieNodeSchedulableClass)
			j.ClassSince, _ = e.Attributes.Float(semconv.AttrJoulieNodeClassSince)
			if v, ok := e.Attributes.Float(semconv.AttrSimJobQueueWait); ok {
				j.QueueWait = &v
			}
		case semconv.EventSimJobStalled.Name:
			j.Stalled = true
		case semconv.EventSimJobCompleted.Name:
			j.Outcome = semconv.JobOutcomeCompleted
			j.Duration = floatAttr(e.Attributes, semconv.AttrSimJobDuration)
			j.RunTime = floatAttr(e.Attributes, semconv.AttrSimJobRunTime)
			j.IdealRunTime = floatAttr(e.Attributes, semconv.AttrSimJobIdealRunTime)
		case semconv.EventSimJobFailed.Name:
			j.Outcome = semconv.JobOutcomeFailed
		case semconv.EventSimJobIncomplete.Name:
			j.Outcome = semconv.JobOutcomeIncomplete
		}
	}
	return out
}

func floatAttr(a Attributes, key string) *float64 {
	if v, ok := a.Float(key); ok {
		return &v
	}
	return nil
}

// matches reports whether f selects j. Workload matches the job's
// workload id or its workload type, which for an inline job is the job
// spec's name.
func (f JobFilter) matches(j *jobState) bool {
	return (f.Class == "" || f.Class == j.Class) &&
		(f.Workload == "" || f.Workload == j.WorkloadID || f.Workload == j.WorkloadType) &&
		(f.NodeProfile == "" || f.NodeProfile == j.NodeProfile)
}

// hasRunRecords reports whether the simulator's logs hold the run's start:
// without it, a count of zero events means the records are missing, not
// that nothing happened.
func (ev *evaluation) hasRunRecords() bool {
	for _, e := range ev.rd.Events {
		if e.Name == semconv.EventSimRunStarted.Name {
			return true
		}
	}
	return false
}

// Quantile returns the q-quantile of values by linear interpolation
// between order statistics (the default of numpy and of R type 7); q = 1 is
// the maximum. values is sorted in place.
func Quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sort.Float64s(values)
	pos := q * float64(len(values)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return values[lo] + (values[hi]-values[lo])*(pos-float64(lo))
}

// formatFloat writes v in its shortest form.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// latestPerSeries returns, for metric, the point with the latest time of
// each series (attribute set and resource), so a cumulative counter reads
// its last value whatever the snapshot cadence.
func latestPerSeries(points []Point, metric string) []Point {
	idx := map[string]int{}
	var out []Point
	for _, p := range points {
		if p.Metric != metric {
			continue
		}
		key := seriesKey(p)
		if i, ok := idx[key]; ok {
			if !p.Time.Before(out[i].Time) {
				out[i] = p
			}
			continue
		}
		idx[key] = len(out)
		out = append(out, p)
	}
	return out
}

func seriesKey(p Point) string {
	keys := make([]string, 0, len(p.Attributes)+len(p.Resource))
	for k, v := range p.Attributes {
		keys = append(keys, fmt.Sprintf("a:%s=%v", k, v))
	}
	for k, v := range p.Resource {
		keys = append(keys, fmt.Sprintf("r:%s=%v", k, v))
	}
	sort.Strings(keys)
	return strings.Join(keys, "\x00")
}
