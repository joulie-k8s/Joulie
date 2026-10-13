package semconv

// Names of the S4 workload runs and scenarios: the simulator's v1 workload
// engine writes them to <runId>/otlp/simulator.{metrics,logs}.jsonl, and the
// scenario runner writes the harness side to otlp/harness.{metrics,logs}.jsonl
// and keys summary.json by the test.* attributes. None of the joulie.*
// attributes below is a semantic convention; each is documented here as
// Joulie's own.

// Attributes of the simulator's workload events and metrics. Durations are
// doubles in seconds, energies doubles in joules, work doubles in the
// trace's work units.
const (
	// AttrSimRunID is the run id given to POST /sim/v1/runs, a resource
	// attribute of every record a run writes.
	AttrSimRunID = "joulie.sim.run.id"
	// AttrSimRunJobs is the number of jobs in the run's trace.
	AttrSimRunJobs = "joulie.sim.run.jobs"
	// AttrSimRunCompletedJobs, AttrSimRunFailedJobs and
	// AttrSimRunIncompleteJobs count the jobs that ended with each outcome.
	AttrSimRunCompletedJobs  = "joulie.sim.run.completed_jobs"
	AttrSimRunFailedJobs     = "joulie.sim.run.failed_jobs"
	AttrSimRunIncompleteJobs = "joulie.sim.run.incomplete_jobs"

	// AttrSimJobID is the trace jobId: one pod of the run.
	AttrSimJobID = "joulie.sim.job.id"
	// AttrSimWorkloadID, AttrSimWorkloadType and AttrSimPodRole are the
	// trace's workloadId, workloadType and podRole of the job.
	AttrSimWorkloadID   = "joulie.sim.workload.id"
	AttrSimWorkloadType = "joulie.sim.workload.type"
	AttrSimPodRole      = "joulie.sim.pod.role"
	// AttrSimGang is true when the job belongs to a gang: none of its pods
	// progresses until all of them run.
	AttrSimGang = "joulie.sim.gang"
	// AttrSimGPUIndices lists the device indices a started job holds, an int
	// array.
	AttrSimGPUIndices = "joulie.sim.gpu.indices"

	// AttrSimJobQueueWait is the time from the Job object's creation to the
	// pod's bind.
	AttrSimJobQueueWait = "joulie.sim.job.queue_wait"
	// AttrSimJobDuration is the time from submission to completion,
	// AttrSimJobRunTime the time from Running to completion, and
	// AttrSimJobIdealRunTime the run time at full speed with no cap.
	AttrSimJobDuration     = "joulie.sim.job.duration"
	AttrSimJobRunTime      = "joulie.sim.job.run_time"
	AttrSimJobIdealRunTime = "joulie.sim.job.ideal_run_time"
	// AttrSimJobEnergy is the energy attributed to the job over its run.
	AttrSimJobEnergy = "joulie.sim.job.energy"
	// AttrSimJobOutcome is how a job ended: JobOutcomeCompleted,
	// JobOutcomeFailed or JobOutcomeIncomplete.
	AttrSimJobOutcome = "joulie.sim.job.outcome"
	// AttrSimJobState is the state of an active job: JobStatePending,
	// JobStateRunning or JobStateStalled.
	AttrSimJobState = "joulie.sim.job.state"

	// AttrSimWorkCPU and AttrSimWorkGPU are the work a job did, and
	// AttrSimWorkCPURemaining and AttrSimWorkGPURemaining the work left when
	// it ended incomplete.
	AttrSimWorkCPU          = "joulie.sim.work.cpu"
	AttrSimWorkGPU          = "joulie.sim.work.gpu"
	AttrSimWorkCPURemaining = "joulie.sim.work.cpu_remaining"
	AttrSimWorkGPURemaining = "joulie.sim.work.gpu_remaining"
	// AttrSimWorkKind says which work a joulie.sim.workload.work point
	// counts: WorkKindCPU or WorkKindGPU.
	AttrSimWorkKind = "joulie.sim.work.kind"

	// AttrSimStallReason is why a job or the run stalled: one of the
	// StallReason values.
	AttrSimStallReason = "joulie.sim.stall.reason"
	// AttrSimStallAge is how long the stall had lasted when the record was
	// emitted.
	AttrSimStallAge = "joulie.sim.stall.age"
)

// Attributes of a node as the simulator observes it, and of the NodeTwin
// samples the scenario runner captures. The simulator only reads these from
// Joulie's objects; it never acts on them.
const (
	// AttrJoulieNodePowerProfile is the node's joulie.io/power-profile label.
	AttrJoulieNodePowerProfile = "joulie.node.power_profile"
	// AttrJoulieNodeSchedulableClass is the node's NodeTwin
	// status.schedulableClass.
	AttrJoulieNodeSchedulableClass = "joulie.node.schedulable_class"
	// AttrJoulieNodeClassSince is how long, in seconds, the node had held
	// that schedulable class when the pod was bound to it.
	AttrJoulieNodeClassSince = "joulie.node.class_since"
	// AttrJoulieNodeTwinPowerSource is the NodeTwin
	// status.powerMeasurement.source of a measured power sample.
	AttrJoulieNodeTwinPowerSource = "joulie.nodetwin.power_source"
	// AttrJoulieNodeTwinControlBackend and AttrJoulieNodeTwinControlResult
	// are the backend and result of one component of the NodeTwin
	// status.controlStatus; hw.type names the component.
	AttrJoulieNodeTwinControlBackend = "joulie.nodetwin.control.backend"
	AttrJoulieNodeTwinControlResult  = "joulie.nodetwin.control.result"
)

// Attributes of the scenario runner's records.
const (
	// AttrJoulieScenarioName is the scenario name, the file stem.
	AttrJoulieScenarioName = "joulie.scenario.name"
	// AttrJoulieScenarioVariant is the variant name; empty for a scenario
	// without variants.
	AttrJoulieScenarioVariant = "joulie.scenario.variant"
	// AttrJoulieAssertionName is the name of one check, the test.case.name
	// of its summary result: the assertion's name or, for an assertion with
	// one comparator per key (jobs, stall, objects), <assertion>.<key>, such
	// as clean.strayPodsOnFakeNodes. Each check is therefore its own
	// joulie.scenario.assertion.value series and its own evaluated record.
	// AttrJoulieAssertionType is the assertion's type as the scenario file
	// gives it.
	AttrJoulieAssertionName = "joulie.assertion.name"
	AttrJoulieAssertionType = "joulie.assertion.type"
	// AttrJoulieAssertionPassed is true when the observed value meets the
	// expectation.
	AttrJoulieAssertionPassed = "joulie.assertion.passed"
	// AttrJoulieAssertionObserved is the observed value, a double, and
	// AttrJoulieAssertionExpected the comparator text, such as ">= 0.95".
	AttrJoulieAssertionObserved = "joulie.assertion.observed"
	AttrJoulieAssertionExpected = "joulie.assertion.expected"
	// AttrJoulieAssertionSource says which side of an assertion a
	// joulie.scenario.assertion.value point is: AssertionSourceObserved or
	// AssertionSourceExpected.
	AttrJoulieAssertionSource = "joulie.assertion.source"
)

// Attributes from the semantic conventions v1.44.0 test registry
// (model/test/registry.yaml), the keys of summary.json.
const (
	AttrTestSuiteName        = "test.suite.name"
	AttrTestSuiteRunStatus   = "test.suite.run.status"
	AttrTestCaseName         = "test.case.name"
	AttrTestCaseResultStatus = "test.case.result.status"
)

// Attribute values.
const (
	// joulie.sim.stall.reason: a gang member waits for the others, a
	// Running job made no progress, or, for the run, every submitted job is
	// blocked.
	StallReasonGangIncomplete = "gang_incomplete"
	StallReasonNoProgress     = "no_progress"
	StallReasonAllBlocked     = "all_blocked"
	// joulie.sim.job.outcome.
	JobOutcomeCompleted  = "completed"
	JobOutcomeFailed     = "failed"
	JobOutcomeIncomplete = "incomplete"
	// joulie.sim.job.state.
	JobStatePending = "pending"
	JobStateRunning = "running"
	JobStateStalled = "stalled"
	// joulie.sim.work.kind.
	WorkKindCPU = "cpu"
	WorkKindGPU = "gpu"
	// joulie.assertion.source.
	AssertionSourceObserved = "observed"
	AssertionSourceExpected = "expected"
	// test.suite.run.status values summary.json uses (the convention also
	// defines skipped and in_progress).
	TestSuiteRunStatusSuccess  = "success"
	TestSuiteRunStatusFailure  = "failure"
	TestSuiteRunStatusAborted  = "aborted"
	TestSuiteRunStatusTimedOut = "timed_out"
	// test.case.result.status.
	TestCaseResultStatusPass = "pass"
	TestCaseResultStatusFail = "fail"
)

// error.type values of the workload and scenario events.
const (
	ErrorTypePodDeleted      = "pod_deleted"
	ErrorTypePodFailed       = "pod_failed"
	ErrorTypeCreateRejected  = "create_rejected"
	ErrorTypePatchFailed     = "patch_failed"
	ErrorTypeAssertionFailed = "assertion_failed"
)

// CaptureMetricPrefix starts the name of every metric the scenario runner
// captures, the NodeTwin samples below and each PromQL capture a scenario
// names. A capture's name is the scenario's, so it is not in the table.
const CaptureMetricPrefix = "joulie.capture."

// SimclusterSummarySchema is the "schema" value of a run's summary.json. It
// is not an OTel name, but it is a joulie. name, and those are spelled only
// in this package.
const SimclusterSummarySchema = "joulie.simcluster.summary/v1"

func init() {
	registerAttributes(
		AttrSimRunID, AttrSimRunJobs, AttrSimRunCompletedJobs, AttrSimRunFailedJobs, AttrSimRunIncompleteJobs,
		AttrSimJobID, AttrSimWorkloadID, AttrSimWorkloadType, AttrSimPodRole, AttrSimGang, AttrSimGPUIndices,
		AttrSimJobQueueWait, AttrSimJobDuration, AttrSimJobRunTime, AttrSimJobIdealRunTime, AttrSimJobEnergy,
		AttrSimJobOutcome, AttrSimJobState,
		AttrSimWorkCPU, AttrSimWorkGPU, AttrSimWorkCPURemaining, AttrSimWorkGPURemaining, AttrSimWorkKind,
		AttrSimStallReason, AttrSimStallAge,
		AttrJoulieNodePowerProfile, AttrJoulieNodeSchedulableClass, AttrJoulieNodeClassSince,
		AttrJoulieNodeTwinPowerSource, AttrJoulieNodeTwinControlBackend, AttrJoulieNodeTwinControlResult,
		AttrJoulieScenarioName, AttrJoulieScenarioVariant,
		AttrJoulieAssertionName, AttrJoulieAssertionType, AttrJoulieAssertionPassed,
		AttrJoulieAssertionObserved, AttrJoulieAssertionExpected, AttrJoulieAssertionSource,
		AttrTestSuiteName, AttrTestSuiteRunStatus, AttrTestCaseName, AttrTestCaseResultStatus,
	)
}

// simJobAttributes are carried by every job event. The k8s.job.name,
// k8s.pod.name and k8s.pod.uid attributes are set once they are known.
var simJobAttributes = []string{
	AttrSimJobID, AttrSimWorkloadID, AttrSimWorkloadType, AttrSimPodRole, AttrJoulieWorkloadClass,
	AttrK8sNamespaceName, AttrK8sJobName, AttrK8sPodName, AttrK8sPodUID,
}

// withSimJob returns the job attributes followed by extra.
func withSimJob(extra ...string) []string {
	return append(append([]string(nil), simJobAttributes...), extra...)
}

// Events of a v1 workload run, on the simulator's resource, which carries
// joulie.sim.run.id. Each name is fixed; the job, node and reason are
// attributes.
var (
	EventSimRunStarted = RegisterEvent(Event{
		Name: "joulie.sim.run.started", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: []string{AttrSimRunJobs},
		Brief:      "A workload run started: its trace loaded and its first tick is due.",
	})
	EventSimRunFinished = RegisterEvent(Event{
		Name: "joulie.sim.run.finished", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: []string{AttrSimRunJobs, AttrSimRunCompletedJobs, AttrSimRunFailedJobs, AttrSimRunIncompleteJobs},
		Brief:      "A workload run ended, every job terminal or the run finished by request, with the count per outcome.",
	})
	EventSimRunStalled = RegisterEvent(Event{
		Name: "joulie.sim.run.stalled", SeverityNumber: SeverityWarn, SeverityText: SeverityTextWarn,
		Attributes: []string{AttrSimStallReason, AttrSimStallAge},
		Brief:      "No submitted job progressed or started and every pending pod is unschedulable, for the stall report interval; emitted once until the run resumes.",
	})
	EventSimRunResumed = RegisterEvent(Event{
		Name: "joulie.sim.run.resumed", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: []string{AttrSimStallReason, AttrSimStallAge},
		Brief:      "A stalled run made progress again; the age is the stall's length.",
	})
	EventSimJobSubmitted = RegisterEvent(Event{
		Name: "joulie.sim.job.submitted", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: withSimJob(AttrSimGang),
		Brief:      "The Job object or pod of a job was created.",
	})
	EventSimJobAdopted = RegisterEvent(Event{
		Name: "joulie.sim.job.adopted", SeverityNumber: SeverityWarn, SeverityText: SeverityTextWarn,
		Attributes: withSimJob(),
		Brief:      "A job's object already existed and was taken over; its work restarts from full.",
	})
	EventSimJobScheduled = RegisterEvent(Event{
		Name: "joulie.sim.job.scheduled", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: withSimJob(AttrK8sNodeName, AttrJoulieNodePowerProfile, AttrJoulieNodeSchedulableClass,
			AttrJoulieNodeClassSince, AttrSimProfile, AttrSimJobQueueWait),
		Brief: "A job's pod was bound to a node; the node attributes are those in effect at the bind.",
	})
	EventSimJobStarted = RegisterEvent(Event{
		Name: "joulie.sim.job.started", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: withSimJob(AttrK8sNodeName, AttrSimGPUIndices),
		Brief:      "A job's pod reached phase Running and took its CPUs and GPU units.",
	})
	EventSimJobStalled = RegisterEvent(Event{
		Name: "joulie.sim.job.stalled", SeverityNumber: SeverityWarn, SeverityText: SeverityTextWarn,
		Attributes: withSimJob(AttrSimStallReason, AttrSimStallAge),
		Brief:      "A Running job made no progress for the stall report interval; it is reported, never completed.",
	})
	EventSimJobResumed = RegisterEvent(Event{
		Name: "joulie.sim.job.resumed", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: withSimJob(AttrSimStallReason, AttrSimStallAge),
		Brief:      "A stalled job made progress again; the age is the stall's length.",
	})
	EventSimJobCompleted = RegisterEvent(Event{
		Name: "joulie.sim.job.completed", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: withSimJob(AttrSimJobDuration, AttrSimJobRunTime, AttrSimJobIdealRunTime, AttrSimJobEnergy,
			AttrSimWorkCPU, AttrSimWorkGPU),
		Brief: "A job did all its work and its pod was marked complete.",
	})
	EventSimJobFailed = RegisterEvent(Event{
		Name: "joulie.sim.job.failed", SeverityNumber: SeverityError, SeverityText: SeverityTextError,
		Attributes: withSimJob(AttrErrorType),
		Brief:      "A job ended without completing: its pod was deleted or failed, its create was rejected, or its completion patch failed.",
	})
	EventSimJobIncomplete = RegisterEvent(Event{
		Name: "joulie.sim.job.incomplete", SeverityNumber: SeverityWarn, SeverityText: SeverityTextWarn,
		Attributes: withSimJob(AttrSimWorkCPURemaining, AttrSimWorkGPURemaining),
		Brief:      "A job was still unfinished when its run ended.",
	})
)

// Events of the scenario runner, on its own resource. The smoke test reuses
// joulie.scenario.assertion.evaluated for its checks.
var (
	EventScenarioStarted = RegisterEvent(Event{
		Name: "joulie.scenario.started", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: []string{AttrJoulieScenarioName, AttrJoulieScenarioVariant},
		Brief:      "A scenario run started.",
	})
	EventScenarioVariantStarted = RegisterEvent(Event{
		Name: "joulie.scenario.variant.started", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: []string{AttrJoulieScenarioName, AttrJoulieScenarioVariant},
		Brief:      "One variant of a scenario started on its cluster.",
	})
	EventScenarioFinished = RegisterEvent(Event{
		Name: "joulie.scenario.finished", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: []string{AttrJoulieScenarioName, AttrJoulieScenarioVariant, AttrTestSuiteRunStatus},
		Brief:      "A scenario run, or one variant of it, ended; the status is that of summary.json.",
	})
	// EventScenarioAssertionEvaluated is registered with severity INFO; a
	// record of a failed assertion carries SeverityError and error.type
	// assertion_failed instead.
	EventScenarioAssertionEvaluated = RegisterEvent(Event{
		Name: "joulie.scenario.assertion.evaluated", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: []string{AttrJoulieAssertionName, AttrJoulieAssertionType, AttrJoulieAssertionPassed,
			AttrJoulieAssertionObserved, AttrJoulieAssertionExpected, AttrErrorType},
		Brief: "One check of an assertion, one comparator, was evaluated against a run's records.",
	})
)

// Metrics of a v1 workload run, on the simulator's resource. Each is
// cumulative from the run's start. The class is the pod's
// joulie.io/workload-class; joulie.sim.workload.energy also uses idle for
// energy no job holds.
var (
	JoulieSimWorkloadJobsSubmitted = Register(Metric{
		Name: "joulie.sim.workload.jobs.submitted", Unit: "{job}", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Counter, Value: Int, Attributes: []string{AttrJoulieWorkloadClass},
		Brief: "Jobs whose Job object or pod was created.",
	})
	JoulieSimWorkloadJobsFinished = Register(Metric{
		Name: "joulie.sim.workload.jobs.finished", Unit: "{job}", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Counter, Value: Int, Attributes: []string{AttrJoulieWorkloadClass, AttrSimJobOutcome},
		Brief: "Jobs that ended, by outcome.",
	})
	JoulieSimWorkloadJobsActive = Register(Metric{
		Name: "joulie.sim.workload.jobs.active", Unit: "{job}", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: UpDownCounter, Value: Int, Attributes: []string{AttrJoulieWorkloadClass, AttrSimJobState},
		Brief: "Submitted jobs that have not ended, by state.",
	})
	JoulieSimWorkloadJobDuration = Register(Metric{
		Name: "joulie.sim.workload.job.duration", Unit: "s", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Histogram, Value: Double, Attributes: []string{AttrJoulieWorkloadClass},
		Brief: "Time from submission to completion of completed jobs.",
	})
	JoulieSimWorkloadJobQueueWait = Register(Metric{
		Name: "joulie.sim.workload.job.queue_wait", Unit: "s", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Histogram, Value: Double, Attributes: []string{AttrJoulieWorkloadClass},
		Brief: "Time from a job's creation to its pod's bind.",
	})
	JoulieSimWorkloadJobSlowdown = Register(Metric{
		Name: "joulie.sim.workload.job.slowdown", Unit: "1", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Histogram, Value: Double, Attributes: []string{AttrJoulieWorkloadClass},
		Brief: "Run time over ideal run time of completed jobs.",
	})
	JoulieSimWorkloadWork = Register(Metric{
		Name: "joulie.sim.workload.work", Unit: "{unit}", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Counter, Value: Double, Attributes: []string{AttrJoulieWorkloadClass, AttrSimWorkKind},
		Brief: "Work done by jobs, in trace work units.",
	})
	JoulieSimWorkloadEnergy = Register(Metric{
		Name: "joulie.sim.workload.energy", Unit: "J", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Counter, Value: Double,
		Attributes: []string{AttrJoulieWorkloadClass, AttrHwType, AttrK8sNodeName, AttrSimProfile},
		Brief:      "Energy of CPU packages and GPUs attributed to jobs by class, the unheld share as idle.",
	})
)

// Metrics of the scenario runner, on its own resource. The NodeTwin samples
// follow hw.status: an UpDownCounter of value 1 for the state in effect.
var (
	JoulieCaptureNodeTwinMeasuredPower = Register(Metric{
		Name: "joulie.capture.nodetwin.measured_power", Unit: "W", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double, Attributes: []string{AttrK8sNodeName, AttrJoulieNodeTwinPowerSource},
		Brief: "The NodeTwin's measured node power at each capture.",
	})
	JoulieCaptureNodeTwinSchedulableClass = Register(Metric{
		Name: "joulie.capture.nodetwin.schedulable_class", Unit: "1", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: UpDownCounter, Value: Int, Attributes: []string{AttrK8sNodeName, AttrJoulieNodeSchedulableClass},
		Brief: "1 for the NodeTwin's schedulable class at each capture.",
	})
	JoulieCaptureNodeTwinControlResult = Register(Metric{
		Name: "joulie.capture.nodetwin.control_result", Unit: "1", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: UpDownCounter, Value: Int,
		Attributes: []string{AttrK8sNodeName, AttrHwType, AttrJoulieNodeTwinControlBackend, AttrJoulieNodeTwinControlResult},
		Brief:      "1 for the backend and result of each component in the NodeTwin's control status at each capture.",
	})
	JoulieScenarioAssertionValue = Register(Metric{
		Name: "joulie.scenario.assertion.value", Unit: "{value}", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double, Attributes: []string{AttrJoulieAssertionName, AttrJoulieAssertionSource},
		Brief: "The observed value of one check of an assertion and, where it is a number, the expected one.",
	})
)
