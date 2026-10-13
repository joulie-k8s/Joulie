package semconv

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The S4 names the simulator's run recorder writes and the scenario
// evaluator reads. A change here changes the run files both sides and the
// stored results depend on.

var frozenWorkloadMetrics = []struct {
	v     Metric
	name  string
	unit  string
	kind  Kind
	value ValueType
	attrs []string
}{
	{JoulieSimWorkloadJobsSubmitted, "joulie.sim.workload.jobs.submitted", "{job}", Counter, Int, []string{"joulie.workload.class"}},
	{JoulieSimWorkloadJobsFinished, "joulie.sim.workload.jobs.finished", "{job}", Counter, Int, []string{"joulie.workload.class", "joulie.sim.job.outcome"}},
	{JoulieSimWorkloadJobsActive, "joulie.sim.workload.jobs.active", "{job}", UpDownCounter, Int, []string{"joulie.workload.class", "joulie.sim.job.state"}},
	{JoulieSimWorkloadJobDuration, "joulie.sim.workload.job.duration", "s", Histogram, Double, []string{"joulie.workload.class"}},
	{JoulieSimWorkloadJobQueueWait, "joulie.sim.workload.job.queue_wait", "s", Histogram, Double, []string{"joulie.workload.class"}},
	{JoulieSimWorkloadJobSlowdown, "joulie.sim.workload.job.slowdown", "1", Histogram, Double, []string{"joulie.workload.class"}},
	{JoulieSimWorkloadWork, "joulie.sim.workload.work", "{unit}", Counter, Double, []string{"joulie.workload.class", "joulie.sim.work.kind"}},
	{JoulieSimWorkloadEnergy, "joulie.sim.workload.energy", "J", Counter, Double, []string{"joulie.workload.class", "hw.type", "k8s.node.name", "joulie.sim.profile"}},
	{JoulieCaptureNodeTwinMeasuredPower, "joulie.capture.nodetwin.measured_power", "W", Gauge, Double, []string{"k8s.node.name", "joulie.nodetwin.power_source"}},
	{JoulieCaptureNodeTwinSchedulableClass, "joulie.capture.nodetwin.schedulable_class", "1", UpDownCounter, Int, []string{"k8s.node.name", "joulie.node.schedulable_class"}},
	{JoulieCaptureNodeTwinControlResult, "joulie.capture.nodetwin.control_result", "1", UpDownCounter, Int, []string{"k8s.node.name", "hw.type", "joulie.nodetwin.control.backend", "joulie.nodetwin.control.result"}},
	{JoulieScenarioAssertionValue, "joulie.scenario.assertion.value", "{value}", Gauge, Double, []string{"joulie.assertion.name", "joulie.assertion.source"}},
}

var frozenWorkloadJobAttributes = []string{
	"joulie.sim.job.id", "joulie.sim.workload.id", "joulie.sim.workload.type", "joulie.sim.pod.role",
	"joulie.workload.class", "k8s.namespace.name", "k8s.job.name", "k8s.pod.name", "k8s.pod.uid",
}

func withFrozenJob(extra ...string) []string {
	return append(append([]string(nil), frozenWorkloadJobAttributes...), extra...)
}

var frozenWorkloadEvents = []struct {
	v        Event
	name     string
	severity int
	attrs    []string
}{
	{EventSimRunStarted, "joulie.sim.run.started", 9, []string{"joulie.sim.run.jobs"}},
	{EventSimRunFinished, "joulie.sim.run.finished", 9, []string{"joulie.sim.run.jobs", "joulie.sim.run.completed_jobs", "joulie.sim.run.failed_jobs", "joulie.sim.run.incomplete_jobs"}},
	{EventSimRunStalled, "joulie.sim.run.stalled", 13, []string{"joulie.sim.stall.reason", "joulie.sim.stall.age"}},
	{EventSimRunResumed, "joulie.sim.run.resumed", 9, []string{"joulie.sim.stall.reason", "joulie.sim.stall.age"}},
	{EventSimJobSubmitted, "joulie.sim.job.submitted", 9, withFrozenJob("joulie.sim.gang")},
	{EventSimJobAdopted, "joulie.sim.job.adopted", 13, withFrozenJob()},
	{EventSimJobScheduled, "joulie.sim.job.scheduled", 9, withFrozenJob("k8s.node.name", "joulie.node.power_profile",
		"joulie.node.schedulable_class", "joulie.node.class_since", "joulie.sim.profile", "joulie.sim.job.queue_wait")},
	{EventSimJobStarted, "joulie.sim.job.started", 9, withFrozenJob("k8s.node.name", "joulie.sim.gpu.indices")},
	{EventSimJobStalled, "joulie.sim.job.stalled", 13, withFrozenJob("joulie.sim.stall.reason", "joulie.sim.stall.age")},
	{EventSimJobResumed, "joulie.sim.job.resumed", 9, withFrozenJob("joulie.sim.stall.reason", "joulie.sim.stall.age")},
	{EventSimJobCompleted, "joulie.sim.job.completed", 9, withFrozenJob("joulie.sim.job.duration", "joulie.sim.job.run_time",
		"joulie.sim.job.ideal_run_time", "joulie.sim.job.energy", "joulie.sim.work.cpu", "joulie.sim.work.gpu")},
	{EventSimJobFailed, "joulie.sim.job.failed", 17, withFrozenJob("error.type")},
	{EventSimJobIncomplete, "joulie.sim.job.incomplete", 13, withFrozenJob("joulie.sim.work.cpu_remaining", "joulie.sim.work.gpu_remaining")},
	{EventScenarioStarted, "joulie.scenario.started", 9, []string{"joulie.scenario.name", "joulie.scenario.variant"}},
	{EventScenarioVariantStarted, "joulie.scenario.variant.started", 9, []string{"joulie.scenario.name", "joulie.scenario.variant"}},
	{EventScenarioFinished, "joulie.scenario.finished", 9, []string{"joulie.scenario.name", "joulie.scenario.variant", "test.suite.run.status"}},
	{EventScenarioAssertionEvaluated, "joulie.scenario.assertion.evaluated", 9, []string{"joulie.assertion.name", "joulie.assertion.type",
		"joulie.assertion.passed", "joulie.assertion.observed", "joulie.assertion.expected", "error.type"}},
}

var frozenWorkloadConstants = map[string]string{
	AttrSimRunID: "joulie.sim.run.id", AttrSimRunJobs: "joulie.sim.run.jobs",
	AttrSimRunCompletedJobs: "joulie.sim.run.completed_jobs", AttrSimRunFailedJobs: "joulie.sim.run.failed_jobs",
	AttrSimRunIncompleteJobs: "joulie.sim.run.incomplete_jobs",
	AttrSimJobID:             "joulie.sim.job.id", AttrSimWorkloadID: "joulie.sim.workload.id",
	AttrSimWorkloadType: "joulie.sim.workload.type", AttrSimPodRole: "joulie.sim.pod.role",
	AttrSimGang: "joulie.sim.gang", AttrSimGPUIndices: "joulie.sim.gpu.indices",
	AttrSimJobQueueWait: "joulie.sim.job.queue_wait", AttrSimJobDuration: "joulie.sim.job.duration",
	AttrSimJobRunTime: "joulie.sim.job.run_time", AttrSimJobIdealRunTime: "joulie.sim.job.ideal_run_time",
	AttrSimJobEnergy: "joulie.sim.job.energy", AttrSimJobOutcome: "joulie.sim.job.outcome", AttrSimJobState: "joulie.sim.job.state",
	AttrSimWorkCPU: "joulie.sim.work.cpu", AttrSimWorkGPU: "joulie.sim.work.gpu",
	AttrSimWorkCPURemaining: "joulie.sim.work.cpu_remaining", AttrSimWorkGPURemaining: "joulie.sim.work.gpu_remaining",
	AttrSimWorkKind: "joulie.sim.work.kind", AttrSimStallReason: "joulie.sim.stall.reason", AttrSimStallAge: "joulie.sim.stall.age",
	AttrJoulieNodePowerProfile: "joulie.node.power_profile", AttrJoulieNodeSchedulableClass: "joulie.node.schedulable_class",
	AttrJoulieNodeClassSince: "joulie.node.class_since", AttrJoulieNodeTwinPowerSource: "joulie.nodetwin.power_source",
	AttrJoulieNodeTwinControlBackend: "joulie.nodetwin.control.backend", AttrJoulieNodeTwinControlResult: "joulie.nodetwin.control.result",
	AttrJoulieScenarioName: "joulie.scenario.name", AttrJoulieScenarioVariant: "joulie.scenario.variant",
	AttrJoulieAssertionName: "joulie.assertion.name", AttrJoulieAssertionType: "joulie.assertion.type",
	AttrJoulieAssertionPassed: "joulie.assertion.passed", AttrJoulieAssertionObserved: "joulie.assertion.observed",
	AttrJoulieAssertionExpected: "joulie.assertion.expected", AttrJoulieAssertionSource: "joulie.assertion.source",
	AttrTestSuiteName: "test.suite.name", AttrTestSuiteRunStatus: "test.suite.run.status",
	AttrTestCaseName: "test.case.name", AttrTestCaseResultStatus: "test.case.result.status",
}

var frozenWorkloadValues = map[string]string{
	StallReasonGangIncomplete: "gang_incomplete", StallReasonNoProgress: "no_progress", StallReasonAllBlocked: "all_blocked",
	JobOutcomeCompleted: "completed", JobOutcomeFailed: "failed", JobOutcomeIncomplete: "incomplete",
	JobStatePending: "pending", JobStateRunning: "running", JobStateStalled: "stalled",
	AssertionSourceObserved: "observed", AssertionSourceExpected: "expected",
	TestSuiteRunStatusSuccess: "success", TestSuiteRunStatusFailure: "failure",
	TestSuiteRunStatusAborted: "aborted", TestSuiteRunStatusTimedOut: "timed_out",
	TestCaseResultStatusPass: "pass", TestCaseResultStatusFail: "fail",
	ErrorTypePodDeleted: "pod_deleted", ErrorTypePodFailed: "pod_failed", ErrorTypeCreateRejected: "create_rejected",
	ErrorTypePatchFailed: "patch_failed", ErrorTypeAssertionFailed: "assertion_failed",
	CaptureMetricPrefix: "joulie.capture.", SimclusterSummarySchema: "joulie.simcluster.summary/v1",
}

func sortedWorkloadCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// The S4 metrics, events, attributes and values are registered exactly as
// pinned, so a rename shows up here and not in a stored run.
func TestWorkloadNamesFrozen(t *testing.T) {
	metrics := map[string]Metric{}
	for _, m := range Metrics() {
		metrics[m.Name] = m
	}
	for _, f := range frozenWorkloadMetrics {
		if f.v.Name != f.name || f.v.Unit != f.unit || f.v.Kind != f.kind || f.v.Value != f.value || f.v.Owner != OwnerJoulie {
			t.Errorf("%s: got (%q, %q, %v, %v, %q), want (%q, %q, %v, %v, joulie)", f.name,
				f.v.Name, f.v.Unit, f.v.Kind, f.v.Value, f.v.Owner, f.name, f.unit, f.kind, f.value)
		}
		if !reflect.DeepEqual(sortedWorkloadCopy(f.v.Attributes), sortedWorkloadCopy(f.attrs)) {
			t.Errorf("%s: attributes %v, want %v", f.name, f.v.Attributes, f.attrs)
		}
		if got, ok := metrics[f.name]; !ok || !reflect.DeepEqual(got, f.v) {
			t.Errorf("%s: registered %+v (%v), exported variable %+v", f.name, got, ok, f.v)
		}
	}
	events := map[string]Event{}
	for _, e := range Events() {
		events[e.Name] = e
	}
	for _, f := range frozenWorkloadEvents {
		if f.v.Name != f.name || f.v.SeverityNumber != f.severity {
			t.Errorf("%s: got (%q, %d), want (%q, %d)", f.name, f.v.Name, f.v.SeverityNumber, f.name, f.severity)
		}
		if !reflect.DeepEqual(sortedWorkloadCopy(f.v.Attributes), sortedWorkloadCopy(f.attrs)) {
			t.Errorf("%s: attributes %v, want %v", f.name, f.v.Attributes, f.attrs)
		}
		if got, ok := events[f.name]; !ok || !reflect.DeepEqual(got, f.v) {
			t.Errorf("%s: registered %+v (%v), exported variable %+v", f.name, got, ok, f.v)
		}
	}
	registered := map[string]bool{}
	for _, a := range Attributes() {
		registered[a] = true
	}
	for got, want := range frozenWorkloadConstants {
		if got != want {
			t.Errorf("attribute constant %q, want %q", got, want)
		}
		if !registered[got] {
			t.Errorf("attribute %q is not registered", got)
		}
	}
	for got, want := range frozenWorkloadValues {
		if got != want {
			t.Errorf("value constant %q, want %q", got, want)
		}
	}
}

// Every job event carries the job's identity, so the evaluator can join the
// events of one job without knowing which event it is reading.
func TestWorkloadJobEventsCarryJobIdentity(t *testing.T) {
	n := 0
	for _, e := range Events() {
		if !strings.HasPrefix(e.Name, "joulie.sim.job.") {
			continue
		}
		n++
		for _, a := range frozenWorkloadJobAttributes {
			if !e.HasAttribute(a) {
				t.Errorf("%s does not list %s", e.Name, a)
			}
		}
	}
	if n != 9 {
		t.Errorf("found %d joulie.sim.job.* events, want 9", n)
	}
}

// A scenario names its own PromQL captures under CaptureMetricPrefix, so
// the only registered metrics there are the runner's three NodeTwin
// samples; a metric another package registers under the prefix could
// collide with a capture a scenario names.
func TestWorkloadCapturePrefixHoldsOnlyNodeTwinSamples(t *testing.T) {
	var got []string
	for _, m := range Metrics() {
		if strings.HasPrefix(m.Name, CaptureMetricPrefix) {
			got = append(got, m.Name)
		}
	}
	sort.Strings(got)
	want := []string{"joulie.capture.nodetwin.control_result", "joulie.capture.nodetwin.measured_power", "joulie.capture.nodetwin.schedulable_class"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("registered metrics under %q: %v, want %v", CaptureMetricPrefix, got, want)
	}
}
