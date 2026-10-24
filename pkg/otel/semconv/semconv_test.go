package semconv

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// frozenMetrics pins every metric in the table: what the simulator emits
// and the controller manager reads, what a calibration sweep records and
// hwcalib fits, and what a run records and the scenario evaluator reads. A
// change here is a change of the contract between a writer and its reader
// and of every stored series or run, so it needs both sides and the docs.
var frozenMetrics = []struct {
	v     Metric
	name  string
	unit  string
	kind  Kind
	value ValueType
	owner string
	attrs []string
}{
	{HwHostEnergy, "hw.host.energy", "J", Counter, Int, OwnerSemconv, []string{"hw.id"}},
	{HwHostPower, "hw.host.power", "W", Gauge, Double, OwnerSemconv, []string{"hw.id"}},
	{HwHostAmbientTemperature, "hw.host.ambient_temperature", "Cel", Gauge, Double, OwnerSemconv, []string{"hw.id"}},
	{HwEnergy, "hw.energy", "J", Counter, Double, OwnerSemconv, []string{"hw.id", "hw.type", "hw.name", "hw.parent"}},
	{HwPower, "hw.power", "W", Gauge, Double, OwnerSemconv, []string{"hw.id", "hw.type", "hw.name", "hw.parent"}},
	{HwCPUSpeed, "hw.cpu.speed", "Hz", Gauge, Int, OwnerSemconv, []string{"hw.id", "hw.name", "hw.vendor", "hw.model"}},
	{HwCPUSpeedLimit, "hw.cpu.speed.limit", "Hz", Gauge, Int, OwnerSemconv, []string{"hw.id", "hw.name", "hw.vendor", "hw.model", "hw.limit_type"}},
	{HwGPUUtilization, "hw.gpu.utilization", "1", Gauge, Double, OwnerSemconv, []string{"hw.id", "hw.name", "hw.vendor", "hw.model", "hw.gpu.task"}},
	{JoulieHwPowerLimit, "joulie.hw.power.limit", "W", Gauge, Double, OwnerJoulie, []string{"hw.id", "hw.type", "joulie.hw.limit.state"}},
	{JoulieFacilityAmbientTemperature, "joulie.facility.ambient_temperature", "Cel", Gauge, Double, OwnerJoulie, nil},
	{JoulieFacilityITEnergy, "joulie.facility.it.energy", "J", Counter, Double, OwnerJoulie, nil},
	{JoulieFacilityITPower, "joulie.facility.it.power", "W", Gauge, Double, OwnerJoulie, nil},
	{JoulieFacilityCoolingEnergy, "joulie.facility.cooling.energy", "J", Counter, Double, OwnerJoulie, nil},
	{JoulieFacilityCoolingPower, "joulie.facility.cooling.power", "W", Gauge, Double, OwnerJoulie, nil},
	{JoulieFacilityPUE, "joulie.facility.pue", "1", Gauge, Double, OwnerJoulie, nil},
	{JoulieCoolingZoneAmbientTemperature, "joulie.cooling_zone.ambient_temperature", "Cel", Gauge, Double, OwnerJoulie, []string{"joulie.cooling_zone.name"}},
	{JoulieRackEnergy, "joulie.rack.energy", "J", Counter, Double, OwnerJoulie, []string{"joulie.rack.name"}},
	{JoulieRackPower, "joulie.rack.power", "W", Gauge, Double, OwnerJoulie, []string{"joulie.rack.name"}},

	// The governor stage.
	{HwGPUIO, "hw.gpu.io", "By", Counter, Int, OwnerSemconv,
		[]string{"hw.id", "hw.name", "hw.vendor", "hw.model", "network.io.direction", "joulie.hw.gpu.io.path"}},
	{JoulieHwGPUSpeed, "joulie.hw.gpu.speed", "Hz", Gauge, Int, OwnerJoulie,
		[]string{"hw.id", "hw.vendor", "hw.model", "joulie.hw.clock.domain"}},
	{JoulieHwGPUSpeedLimit, "joulie.hw.gpu.speed.limit", "Hz", Gauge, Int, OwnerJoulie,
		[]string{"hw.id", "hw.vendor", "hw.model", "joulie.hw.clock.domain", "hw.limit_type"}},
	{JoulieHwGovernor, "joulie.hw.governor", "1", UpDownCounter, Int, OwnerJoulie,
		[]string{"hw.id", "hw.type", "joulie.hw.governor.preset", "joulie.hw.governor.native"}},

	// A calibration sweep: system.cpu.utilization is semconv v1.44.0
	// model/system/metrics.yaml, the setpoint Joulie's own.
	{SystemCPUUtilization, "system.cpu.utilization", "1", Gauge, Double, OwnerSemconv, []string{"cpu.mode", "cpu.logical_number"}},
	{JoulieCalibrationSetpoint, "joulie.calibration.setpoint", "{setpoint}", Gauge, Double, OwnerJoulie,
		[]string{"joulie.calibration.knob", "joulie.calibration.class", "joulie.calibration.memory_intensity"}},

	// A workload run and the scenario runner.
	{JoulieSimWorkloadJobsSubmitted, "joulie.sim.workload.jobs.submitted", "{job}", Counter, Int, OwnerJoulie, []string{"joulie.workload.class"}},
	{JoulieSimWorkloadJobsFinished, "joulie.sim.workload.jobs.finished", "{job}", Counter, Int, OwnerJoulie, []string{"joulie.workload.class", "joulie.sim.job.outcome"}},
	{JoulieSimWorkloadJobsActive, "joulie.sim.workload.jobs.active", "{job}", UpDownCounter, Int, OwnerJoulie, []string{"joulie.workload.class", "joulie.sim.job.state"}},
	{JoulieSimWorkloadJobDuration, "joulie.sim.workload.job.duration", "s", Histogram, Double, OwnerJoulie, []string{"joulie.workload.class"}},
	{JoulieSimWorkloadJobQueueWait, "joulie.sim.workload.job.queue_wait", "s", Histogram, Double, OwnerJoulie, []string{"joulie.workload.class"}},
	{JoulieSimWorkloadJobSlowdown, "joulie.sim.workload.job.slowdown", "1", Histogram, Double, OwnerJoulie, []string{"joulie.workload.class"}},
	{JoulieSimWorkloadWork, "joulie.sim.workload.work", "{unit}", Counter, Double, OwnerJoulie, []string{"joulie.workload.class", "joulie.sim.work.kind"}},
	{JoulieSimWorkloadEnergy, "joulie.sim.workload.energy", "J", Counter, Double, OwnerJoulie, []string{"joulie.workload.class", "hw.type", "k8s.node.name", "joulie.sim.profile"}},
	{JoulieCaptureNodeTwinMeasuredPower, "joulie.capture.nodetwin.measured_power", "W", Gauge, Double, OwnerJoulie, []string{"k8s.node.name", "joulie.nodetwin.power_source"}},
	{JoulieCaptureNodeTwinSchedulableClass, "joulie.capture.nodetwin.schedulable_class", "1", UpDownCounter, Int, OwnerJoulie, []string{"k8s.node.name", "joulie.node.schedulable_class"}},
	{JoulieCaptureNodeTwinControlResult, "joulie.capture.nodetwin.control_result", "1", UpDownCounter, Int, OwnerJoulie, []string{"k8s.node.name", "hw.type", "joulie.nodetwin.control.backend", "joulie.nodetwin.control.result"}},
	{JoulieScenarioAssertionValue, "joulie.scenario.assertion.value", "{value}", Gauge, Double, OwnerJoulie, []string{"joulie.assertion.name", "joulie.assertion.source"}},

	// The carbon stage: what the controller manager's facility input read.
	{JoulieGridCarbonIntensity, "joulie.grid.carbon_intensity", "g/(kW.h)", Gauge, Double, OwnerJoulie, []string{"joulie.grid.carbon_intensity.signal"}},
}

// frozenAttributes pins every attribute constant, each of which must be
// registered.
var frozenAttributes = map[string]string{
	AttrHwID: "hw.id", AttrHwType: "hw.type", AttrHwName: "hw.name", AttrHwParent: "hw.parent",
	AttrHwVendor: "hw.vendor", AttrHwModel: "hw.model", AttrHwLimitType: "hw.limit_type", AttrHwGPUTask: "hw.gpu.task",
	AttrK8sClusterName: "k8s.cluster.name", AttrK8sNodeName: "k8s.node.name", AttrK8sNamespaceName: "k8s.namespace.name",
	AttrK8sPodName: "k8s.pod.name", AttrK8sPodUID: "k8s.pod.uid", AttrK8sJobName: "k8s.job.name",
	AttrHostName: "host.name", AttrHostID: "host.id",
	AttrServiceName: "service.name", AttrServiceNamespace: "service.namespace",
	AttrServiceInstanceID: "service.instance.id", AttrServiceVersion: "service.version",
	AttrErrorType:        "error.type",
	AttrJoulieLimitState: "joulie.hw.limit.state", AttrJoulieCoolingZoneName: "joulie.cooling_zone.name",
	AttrJoulieRackName: "joulie.rack.name", AttrJoulieWorkloadClass: "joulie.workload.class",
	AttrSimProfile: "joulie.sim.profile", AttrSimPool: "joulie.sim.pool",
	AttrSimTreeResumed: "joulie.sim.tree.resumed", AttrSimTreeAdded: "joulie.sim.tree.added",
	AttrSimNodeProfile: "joulie.sim.node.profile", AttrSimFile: "joulie.sim.file", AttrSimOutcome: "joulie.sim.outcome",
	AttrSimValueWritten: "joulie.sim.value.written", AttrSimValueEffective: "joulie.sim.value.effective",

	AttrNetworkIODirection: "network.io.direction", AttrJoulieGPUIOPath: "joulie.hw.gpu.io.path",
	AttrJoulieClockDomain: "joulie.hw.clock.domain", AttrJoulieGovernorPreset: "joulie.hw.governor.preset",
	AttrJoulieGovernorNative: "joulie.hw.governor.native",

	AttrCPUMode: "cpu.mode", AttrCPULogicalNumber: "cpu.logical_number",
	AttrCalibrationKnob: "joulie.calibration.knob", AttrCalibrationClass: "joulie.calibration.class",
	AttrCalibrationMemoryIntensity: "joulie.calibration.memory_intensity",

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

	AttrJoulieGridCarbonIntensitySignal: "joulie.grid.carbon_intensity.signal",
}

// frozenValues pins every value constant: the constant, then its spelling.
// Two constants may share a spelling, as hw.id host and the GPU I/O path
// host do.
var frozenValues = [][2]string{
	{HwTypeCPU, "cpu"}, {HwTypeMemory, "memory"}, {HwTypeGPU, "gpu"},
	{HwIDHost, "host"}, {LimitTypeMax, "max"}, {LimitTypeThrottled, "throttled"},
	{LimitStateRequested, "requested"}, {LimitStateEnforced, "enforced"}, {GPUTaskGeneral, "general"},
	{SchemaURL, "https://opentelemetry.io/schemas/1.44.0"}, {GPUIOPathPeer, "peer"}, {GPUIOPathHost, "host"},
	{GPUIOPathNIC, "nic"}, {NetworkIODirectionReceive, "receive"}, {NetworkIODirectionTransmit, "transmit"},
	{ErrorTypeEINVAL, "einval"}, {ErrorTypeEOPNOTSUPP, "eopnotsupp"}, {ErrorTypeEBUSY, "ebusy"}, {ErrorTypeENODATA, "enodata"},
	{KnobCPUUtilization, "cpu.utilization"}, {KnobCPUFrequencyMax, "cpu.frequency.max"}, {KnobCPUPowerLimit, "cpu.power.limit"},
	{KnobGPUUtilization, "gpu.utilization"}, {KnobGPUPowerLimit, "gpu.power.limit"}, {CPUModeIdle, "idle"},
	{StallReasonGangIncomplete, "gang_incomplete"}, {StallReasonNoProgress, "no_progress"}, {StallReasonAllBlocked, "all_blocked"},
	{JobOutcomeCompleted, "completed"}, {JobOutcomeFailed, "failed"}, {JobOutcomeIncomplete, "incomplete"},
	{JobStatePending, "pending"}, {JobStateRunning, "running"}, {JobStateStalled, "stalled"},
	{AssertionSourceObserved, "observed"}, {AssertionSourceExpected, "expected"}, {TestSuiteRunStatusSuccess, "success"},
	{TestSuiteRunStatusFailure, "failure"}, {TestSuiteRunStatusAborted, "aborted"}, {TestSuiteRunStatusTimedOut, "timed_out"},
	{TestCaseResultStatusPass, "pass"}, {TestCaseResultStatusFail, "fail"}, {ErrorTypePodDeleted, "pod_deleted"},
	{ErrorTypePodFailed, "pod_failed"}, {ErrorTypeCreateRejected, "create_rejected"}, {ErrorTypePatchFailed, "patch_failed"},
	{ErrorTypeAssertionFailed, "assertion_failed"}, {CaptureMetricPrefix, "joulie.capture."}, {SimclusterSummarySchema, "joulie.simcluster.summary/v1"},
	{CarbonIntensitySignalMarginal, "marginal"}, {CarbonIntensitySignalAverage, "average"},
}

// frozenEvents pins every event: the simulator's node and hardware events,
// a workload run's and the scenario runner's.
var frozenEvents = []struct {
	v        Event
	name     string
	severity int
	attrs    []string
}{
	{EventSimNodeReady, "joulie.sim.node.ready", 9, []string{"joulie.sim.profile", "joulie.sim.pool", "joulie.sim.tree.resumed", "joulie.sim.tree.added"}},
	{EventSimNodeMismatch, "joulie.sim.node.mismatch", 13, []string{"error.type", "joulie.sim.profile", "joulie.sim.node.profile"}},
	{EventSimCapApplied, "joulie.sim.cap.applied", 9, []string{"hw.id", "hw.type", "joulie.sim.file", "joulie.sim.outcome", "joulie.sim.value.written", "joulie.sim.value.effective"}},
	{EventSimSettingApplied, "joulie.sim.setting.applied", 9, []string{"hw.id", "hw.type", "joulie.sim.file", "joulie.sim.outcome", "joulie.sim.value.written", "joulie.sim.value.effective"}},
	{EventSimWriteRejected, "joulie.sim.write.rejected", 13, []string{"hw.id", "hw.type", "joulie.sim.file", "joulie.sim.outcome", "joulie.sim.value.written", "joulie.sim.value.effective", "error.type"}},
	{EventSimCapReset, "joulie.sim.cap.reset", 13, []string{"hw.id", "hw.type", "joulie.sim.file", "joulie.sim.outcome", "joulie.sim.value.written", "joulie.sim.value.effective"}},

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

// semconvDefinitions is what the semantic conventions v1.44.0 define for the
// hw.* metrics Joulie uses (model/hardware/{common,host-metrics,
// common-metrics,cpu-metrics,gpu-metrics}.yaml): the instrument, the unit,
// the metric_value_type annotation and every attribute the definition
// allows. A semconv metric added to the table must be added here, with the
// definition it is checked against.
var semconvDefinitions = map[string]struct {
	kind  Kind
	unit  string
	value ValueType
	attrs []string
}{
	"hw.host.energy":              {Counter, "J", Int, []string{"hw.id", "hw.name", "hw.parent"}},
	"hw.host.power":               {Gauge, "W", Double, []string{"hw.id", "hw.name", "hw.parent"}},
	"hw.host.ambient_temperature": {Gauge, "Cel", Double, []string{"hw.id", "hw.name", "hw.parent"}},
	"hw.energy":                   {Counter, "J", Double, []string{"hw.id", "hw.name", "hw.parent", "hw.type"}},
	"hw.power":                    {Gauge, "W", Double, []string{"hw.id", "hw.name", "hw.parent", "hw.type"}},
	"hw.cpu.speed":                {Gauge, "Hz", Int, []string{"hw.id", "hw.name", "hw.parent", "hw.model", "hw.vendor"}},
	"hw.cpu.speed.limit":          {Gauge, "Hz", Int, []string{"hw.id", "hw.name", "hw.parent", "hw.model", "hw.vendor", "hw.limit_type"}},
	"hw.gpu.utilization": {Gauge, "1", Double, []string{"hw.id", "hw.name", "hw.parent", "hw.driver_version", "hw.firmware_version",
		"hw.model", "hw.serial_number", "hw.vendor", "hw.gpu.task"}},
	"hw.gpu.io": {Counter, "By", Int, []string{"hw.id", "hw.name", "hw.parent", "hw.driver_version", "hw.firmware_version",
		"hw.model", "hw.serial_number", "hw.vendor", "network.io.direction"}},
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func indexMetrics() map[string]Metric {
	out := map[string]Metric{}
	for _, m := range Metrics() {
		out[m.Name] = m
	}
	return out
}

// SV01: metric, attribute and event names are unique and the accessors
// return them sorted; the frozen names are registered exactly as pinned; a
// second registration of a name panics and leaves the table as it was.
func TestSV01NamesAreUniqueSortedAndFrozen(t *testing.T) {
	t.Run("unique and sorted", func(t *testing.T) {
		ms := Metrics()
		for i := 1; i < len(ms); i++ {
			if ms[i-1].Name >= ms[i].Name {
				t.Errorf("Metrics() not strictly sorted at %d: %q then %q", i, ms[i-1].Name, ms[i].Name)
			}
		}
		es := Events()
		for i := 1; i < len(es); i++ {
			if es[i-1].Name >= es[i].Name {
				t.Errorf("Events() not strictly sorted at %d: %q then %q", i, es[i-1].Name, es[i].Name)
			}
		}
		as := Attributes()
		for i := 1; i < len(as); i++ {
			if as[i-1] >= as[i] {
				t.Errorf("Attributes() not strictly sorted at %d: %q then %q", i, as[i-1], as[i])
			}
		}
		metricNames := map[string]bool{}
		for _, m := range ms {
			metricNames[m.Name] = true
		}
		for _, e := range es {
			if metricNames[e.Name] {
				t.Errorf("%q is both a metric and an event", e.Name)
			}
		}
	})

	t.Run("every listed attribute is registered once per list", func(t *testing.T) {
		known := map[string]bool{}
		for _, a := range Attributes() {
			known[a] = true
		}
		check := func(owner string, attrs []string) {
			seen := map[string]bool{}
			for _, a := range attrs {
				if !known[a] {
					t.Errorf("%s lists attribute %q, which no file registers", owner, a)
				}
				if seen[a] {
					t.Errorf("%s lists attribute %q twice", owner, a)
				}
				seen[a] = true
			}
		}
		for _, m := range Metrics() {
			check("metric "+m.Name, m.Attributes)
		}
		for _, e := range Events() {
			check("event "+e.Name, e.Attributes)
		}
	})

	t.Run("frozen metrics", func(t *testing.T) {
		reg := indexMetrics()
		if len(frozenMetrics) != len(reg) {
			t.Errorf("%d frozen metrics, %d registered: pin every metric", len(frozenMetrics), len(reg))
		}
		for _, f := range frozenMetrics {
			if f.v.Name != f.name || f.v.Unit != f.unit || f.v.Kind != f.kind || f.v.Value != f.value || f.v.Owner != f.owner {
				t.Errorf("%s: got (%q, %q, %v, %v, %q), want (%q, %q, %v, %v, %q)", f.name,
					f.v.Name, f.v.Unit, f.v.Kind, f.v.Value, f.v.Owner, f.name, f.unit, f.kind, f.value, f.owner)
			}
			if !reflect.DeepEqual(sortedCopy(f.v.Attributes), sortedCopy(f.attrs)) {
				t.Errorf("%s: attributes %v, want %v", f.name, f.v.Attributes, f.attrs)
			}
			if f.v.Brief == "" {
				t.Errorf("%s has no brief", f.name)
			}
			got, ok := reg[f.name]
			if !ok {
				t.Errorf("%s is not registered", f.name)
				continue
			}
			if !reflect.DeepEqual(got, f.v) {
				t.Errorf("%s: registered %+v, exported variable %+v", f.name, got, f.v)
			}
		}
	})

	t.Run("frozen attributes and values", func(t *testing.T) {
		known := map[string]bool{}
		for _, a := range Attributes() {
			known[a] = true
		}
		for got, want := range frozenAttributes {
			if got != want {
				t.Errorf("attribute constant %q, want %q", got, want)
			}
			if !known[got] {
				t.Errorf("attribute %q is not registered", got)
			}
		}
		for _, v := range frozenValues {
			if got, want := v[0], v[1]; got != want {
				t.Errorf("value constant %q, want %q", got, want)
			}
		}
	})

	t.Run("frozen events", func(t *testing.T) {
		reg := map[string]Event{}
		for _, e := range Events() {
			reg[e.Name] = e
		}
		if len(frozenEvents) != len(reg) {
			t.Errorf("%d frozen events, %d registered: pin every event", len(frozenEvents), len(reg))
		}
		for _, f := range frozenEvents {
			if f.v.Name != f.name || f.v.SeverityNumber != f.severity {
				t.Errorf("%s: got (%q, %d), want (%q, %d)", f.name, f.v.Name, f.v.SeverityNumber, f.name, f.severity)
			}
			if !reflect.DeepEqual(sortedCopy(f.v.Attributes), sortedCopy(f.attrs)) {
				t.Errorf("%s: attributes %v, want %v", f.name, f.v.Attributes, f.attrs)
			}
			if f.v.Brief == "" {
				t.Errorf("%s has no brief", f.name)
			}
			if got, ok := reg[f.name]; !ok || !reflect.DeepEqual(got, f.v) {
				t.Errorf("%s: registered %+v (%v), exported variable %+v", f.name, got, ok, f.v)
			}
		}
	})

	t.Run("duplicates panic and change nothing", func(t *testing.T) {
		metrics, events, attrs := Metrics(), Events(), Attributes()
		// Each refused entry differs from the registered one, so an entry
		// overwritten before the panic shows in the comparison below.
		metric := HwEnergy
		metric.Unit, metric.Brief, metric.Attributes = "W", "a second hw.energy", []string{AttrHwID}
		mustPanic(t, "Register", func() { Register(metric) })
		event := EventSimNodeReady
		event.SeverityNumber, event.Brief, event.Attributes = SeverityError, "a second node.ready", nil
		mustPanic(t, "RegisterEvent", func() { RegisterEvent(event) })
		// A list with one duplicate registers none of its names.
		mustPanic(t, "registerAttributes", func() { registerAttributes("joulie.test.fresh", AttrHwID) })
		mustPanic(t, "registerAttributes", func() { registerAttributes("joulie.test.twice", "joulie.test.twice") })
		if got := Metrics(); !reflect.DeepEqual(got, metrics) {
			t.Errorf("a refused Register changed the metrics:\n got %+v\nwant %+v", got, metrics)
		}
		if got := Events(); !reflect.DeepEqual(got, events) {
			t.Errorf("a refused RegisterEvent changed the events:\n got %+v\nwant %+v", got, events)
		}
		if got := Attributes(); !reflect.DeepEqual(got, attrs) {
			t.Errorf("a refused registerAttributes changed the attributes:\n got %v\nwant %v", got, attrs)
		}
	})

	t.Run("accessors and exported variables do not share the table", func(t *testing.T) {
		for _, m := range Metrics() {
			if len(m.Attributes) > 0 {
				m.Attributes[0] = "mutated"
			}
		}
		for _, e := range Events() {
			if len(e.Attributes) > 0 {
				e.Attributes[0] = "mutated"
			}
		}
		// A caller writing through an exported variable reaches only that
		// variable. The writes are undone so the other tests see the table.
		metricAttr, eventAttr := HwEnergy.Attributes[0], EventSimCapApplied.Attributes[0]
		HwEnergy.Attributes[0], EventSimCapApplied.Attributes[0] = "mutated", "mutated"
		defer func() { HwEnergy.Attributes[0], EventSimCapApplied.Attributes[0] = metricAttr, eventAttr }()
		for _, m := range Metrics() {
			if m.HasAttribute("mutated") {
				t.Errorf("a write to Metrics() or %s's variable changed the table: %s %v", HwEnergy.Name, m.Name, m.Attributes)
			}
		}
		for _, e := range Events() {
			if e.HasAttribute("mutated") {
				t.Errorf("a write to Events() or %s's variable changed the table: %s %v", EventSimCapApplied.Name, e.Name, e.Attributes)
			}
		}
	})
}

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s of a registered name did not panic", name)
		}
	}()
	f()
}

// SV02: whole-host readings are hw.host.* only and carry no hw.type (the
// MUST in docs/hardware/host.md), hw.energy and hw.power list exactly the
// attributes v1.44.0 defines for them, and no semconv metric lists an
// attribute its definition does not allow.
func TestSV02HostMetricsHaveNoTypeAndComponentMetricsFollowSemconv(t *testing.T) {
	for _, m := range Metrics() {
		if strings.HasPrefix(m.Name, "hw.host.") && m.HasAttribute(AttrHwType) {
			t.Errorf("%s lists hw.type; host readings must not, so they never sum with component readings", m.Name)
		}
		if m.Owner != OwnerSemconv {
			continue
		}
		def, ok := semconvDefinitions[m.Name]
		if !ok {
			continue // SV04 reports it
		}
		allowed := map[string]bool{}
		for _, a := range append(append([]string(nil), def.attrs...), joulieExtensions[m.Name]...) {
			allowed[a] = true
		}
		for _, a := range m.Attributes {
			if !allowed[a] {
				t.Errorf("%s lists %q, which semconv v1.44.0 does not define for it", m.Name, a)
			}
		}
	}
	want := []string{"hw.id", "hw.name", "hw.parent", "hw.type"}
	for _, m := range []Metric{HwEnergy, HwPower} {
		if got := sortedCopy(m.Attributes); !reflect.DeepEqual(got, want) {
			t.Errorf("%s lists %v, want exactly %v", m.Name, got, want)
		}
	}
}

// SV03: names Joulie defines carry the application prefix joulie. and never a
// semconv namespace; names owned by the conventions never carry joulie.
func TestSV03JoulieNamesUseTheApplicationPrefix(t *testing.T) {
	semconvNamespaces := []string{"hw.", "k8s.", "host.", "service."}
	checkJoulie := func(what, name string) {
		if !strings.HasPrefix(name, "joulie.") {
			t.Errorf("%s %q is Joulie's and must start with joulie.", what, name)
		}
		for _, ns := range semconvNamespaces {
			if strings.HasPrefix(name, ns) {
				t.Errorf("%s %q is Joulie's but uses the semconv namespace %s", what, name, ns)
			}
		}
	}
	for _, m := range Metrics() {
		switch m.Owner {
		case OwnerJoulie:
			checkJoulie("metric", m.Name)
		case OwnerSemconv:
			if strings.HasPrefix(m.Name, "joulie.") {
				t.Errorf("metric %q is owned by semconv but uses Joulie's prefix", m.Name)
			}
		}
	}
	for _, e := range Events() {
		checkJoulie("event", e.Name)
	}
	for _, a := range Attributes() {
		if !strings.HasPrefix(a, "joulie.") && strings.Contains(a, "joulie") {
			t.Errorf("attribute %q names Joulie outside the joulie. prefix", a)
		}
	}
}

// SV04: no name ends in _total, which Prometheus appends to counters; units
// are the UCUM units the table uses or an annotation; every hw.* metric has
// the instrument, unit and value type of its v1.44.0 definition, so host
// energy and both CPU speeds are Int. g/(kW.h) is the carbon intensity's
// grams per kilowatt-hour, which Prometheus spells _grams_per_kW_h. UCUM
// reads the period and the solidus left to right with equal precedence, so
// a product after a solidus is parenthesised: g/kW.h is (g/kW).h, gram-hours
// per kilowatt.
func TestSV04UnitsSuffixesAndValueTypes(t *testing.T) {
	units := map[string]bool{"J": true, "W": true, "Cel": true, "Hz": true, "1": true, "s": true, "By": true, "g/(kW.h)": true}
	annotation := regexp.MustCompile(`^\{[a-z][a-z0-9_.]*\}$`)
	for _, m := range Metrics() {
		if strings.HasSuffix(m.Name, "_total") {
			t.Errorf("metric %q ends in _total; Prometheus adds the suffix to counters itself", m.Name)
		}
		if !units[m.Unit] && !annotation.MatchString(m.Unit) {
			t.Errorf("metric %q has unit %q, want one of J, W, Cel, Hz, 1, s, By, g/(kW.h) or an annotation {...}", m.Name, m.Unit)
		}
		if ucumProductAfterSolidus(m.Unit) {
			t.Errorf("metric %q has unit %q: UCUM multiplies a period after a solidus into the numerator; parenthesise the denominator, such as g/(kW.h)", m.Name, m.Unit)
		}
		if !strings.HasPrefix(m.Name, "hw.") {
			continue
		}
		def, ok := semconvDefinitions[m.Name]
		if !ok {
			t.Errorf("hw.* metric %q has no entry in semconvDefinitions; add its v1.44.0 definition there", m.Name)
			continue
		}
		if m.Kind != def.kind || m.Unit != def.unit || m.Value != def.value {
			t.Errorf("%s is (%v, %q, %v), semconv v1.44.0 defines (%v, %q, %v)", m.Name, m.Kind, m.Unit, m.Value, def.kind, def.unit, def.value)
		}
	}
	for _, e := range Events() {
		if strings.HasSuffix(e.Name, "_total") {
			t.Errorf("event %q ends in _total", e.Name)
		}
	}
	for _, a := range Attributes() {
		if strings.HasSuffix(a, "_total") {
			t.Errorf("attribute %q ends in _total", a)
		}
	}
	for name, want := range map[string]ValueType{"hw.host.energy": Int, "hw.cpu.speed": Int, "hw.cpu.speed.limit": Int} {
		if got := indexMetrics()[name].Value; got != want {
			t.Errorf("%s value type %v, want %v", name, got, want)
		}
	}
}

// SV05: event names are lowercase dotted names and every event has a severity
// whose text is the short name of its range in the logs data model.
func TestSV05EventNamesAndSeverities(t *testing.T) {
	name := regexp.MustCompile(`^[a-z0-9_.]+$`)
	shortName := func(n int) string {
		switch {
		case n >= 1 && n <= 4:
			return "TRACE"
		case n <= 8:
			return "DEBUG"
		case n <= 12:
			return "INFO"
		case n <= 16:
			return "WARN"
		case n <= 20:
			return "ERROR"
		case n <= 24:
			return "FATAL"
		}
		return ""
	}
	if len(Events()) == 0 {
		t.Fatal("no events registered")
	}
	for _, e := range Events() {
		if !name.MatchString(e.Name) {
			t.Errorf("event name %q does not match %s", e.Name, name)
		}
		if e.SeverityNumber < 1 || e.SeverityNumber > 24 {
			t.Errorf("event %q has severity %d, want 1 to 24", e.Name, e.SeverityNumber)
			continue
		}
		if want := shortName(e.SeverityNumber); e.SeverityText != want {
			t.Errorf("event %q has severity text %q for %d, want %q", e.Name, e.SeverityText, e.SeverityNumber, want)
		}
	}
}

// ucumProductAfterSolidus reports a period that follows a solidus outside
// parentheses and annotations. UCUM evaluates both operators left to right
// with equal precedence, so such a unit multiplies by the term after the
// period instead of dividing by it.
func ucumProductAfterSolidus(unit string) bool {
	depth, annotation, solidus := 0, false, false
	for _, r := range unit {
		switch {
		case annotation:
			annotation = r != '}'
		case r == '{':
			annotation = true
		case r == '(':
			depth++
		case r == ')':
			depth--
		case depth == 0 && r == '/':
			solidus = true
		case depth == 0 && r == '.' && solidus:
			return true
		}
	}
	return false
}

// TestSV04bProductAfterSolidus: the check SV04 runs on every unit tells a
// parenthesised denominator from a product after a solidus.
func TestSV04bProductAfterSolidus(t *testing.T) {
	for unit, want := range map[string]bool{
		"g/kW.h": true, "m/s.s": true, "{x}/s.m": true,
		"g/(kW.h)": false, "g/kW/h": false, "kW.h": false, "kW.h/g": false, "W": false, "{a.b}/s": false, "By/{a.b}": false,
	} {
		if got := ucumProductAfterSolidus(unit); got != want {
			t.Errorf("ucumProductAfterSolidus(%q) = %v, want %v", unit, got, want)
		}
	}
}
