package scenario

import (
	"encoding/json"
	"os/exec"
	"path"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// Run directory layout. Paths are relative to the run directory, or to a
// variant directory inside it; the simulator's files are workload.TraceFile
// and workload.OTLPDir/workload.Simulator{Metrics,Logs}File.
const (
	// RunsDir holds the run directories under SIMCLUSTER_DIR, and LatestLink
	// is the symlink in it to the newest run.
	RunsDir    = "runs"
	LatestLink = "latest"
	// SummaryFile is the Summary of the run, and of each variant.
	SummaryFile = "summary.json"
	// ScenarioEffectiveFile is the scenario with its defaults filled in.
	ScenarioEffectiveFile = "scenario.effective.yaml"
	// ValuesEffectiveFile, ImagesEnvFile and VariantEnvFile record a
	// variant's Helm values, image references and harness settings.
	ValuesEffectiveFile = "values.effective.yaml"
	ImagesEnvFile       = "images.env"
	VariantEnvFile      = "variant.env"
	// RunStatusFile is the workload.RunStatus the run's finish returned.
	RunStatusFile = "run-status.json"
	// HarnessMetricsFile and HarnessLogsFile are the runner's OTLP JSON
	// Lines, next to the simulator's under workload.OTLPDir.
	HarnessMetricsFile = "harness.metrics.jsonl"
	HarnessLogsFile    = "harness.logs.jsonl"
	// KubeDir holds the Kubernetes snapshots (KubeSnapshotFile) and LogsDir
	// the component logs.
	KubeDir = "k8s"
	LogsDir = "logs"
)

// Kubernetes snapshots: every kind at SnapshotEnd, the nodes and Joulie's
// objects also at SnapshotStart.
const (
	SnapshotStart = "start"
	SnapshotEnd   = "end"

	KindNodeTwins     = "nodetwins"
	KindNodeHardwares = "nodehardwares"
	KindNodes         = "nodes"
	KindJobs          = "jobs"
	KindPods          = "pods"
	KindEvents        = "events"
)

// KubeSnapshotFile is the path of the snapshot of kind at phase, such as
// k8s/nodetwins-start.yaml.
func KubeSnapshotFile(kind, phase string) string {
	return path.Join(KubeDir, kind+"-"+phase+".yaml")
}

// RunDirName is the name of a run directory:
// <UTC start yyyymmddThhmmssZ>-<scenario>-<first 12 characters of the git
// sha>, with -dirty when the tree had changes. A smoke run passes the spec
// name as scenario.
func RunDirName(start time.Time, scenario, gitSHA string, dirty bool) string {
	sha := gitSHA
	if len(sha) > 12 {
		sha = sha[:12]
	}
	name := start.UTC().Format("20060102T150405Z") + "-" + scenario + "-" + sha
	if dirty {
		name += "-dirty"
	}
	return name
}

// The runner's own telemetry identity: service.name of the harness
// resource, and the instrumentation scope of its records.
const (
	ServiceName = "joulie-simscenario"
	ScopeName   = "github.com/matbun/joulie/simulator/pkg/scenario"
)

// Exit codes of the runner and of the smoke test, and Summary.ExitCode.
const (
	// ExitPass: every assertion of severity fail passed.
	ExitPass = 0
	// ExitFail: an assertion of severity fail failed, or a known defect no
	// longer reproduces.
	ExitFail = 1
	// ExitError: the run could not be carried out or evaluated: an invalid
	// scenario, a cluster or API failure, unreadable results.
	ExitError = 2
)

// Energy boundary of a scenario summary: the CPU packages and GPUs, the
// boundary of Joulie's default node power input (hw.energy with hw.type cpu
// or gpu). Idle package energy is split by requested CPUs over allocatable
// CPUs; the unrequested share is class idle.
const (
	EnergyBoundary  = "cpu packages + gpus"
	EnergyIdleSplit = "requested-cpu share"
)

// Summary is summary.json, schema semconv.SimclusterSummarySchema, for a
// smoke run, a scenario run and each scenario variant. The keys with dots
// are the semantic conventions' test.* attributes. Failures, Warnings,
// KnownDefects and Assertions are written as [] when nil (MarshalJSON), so a
// reader can iterate them without a null check.
type Summary struct {
	Schema string `json:"schema"`
	// SuiteName is the spec and telemetry mode of a smoke run, the scenario
	// of a scenario run (with or without variants), and <scenario>/<variant>
	// in a variant's own summary.
	SuiteName string `json:"test.suite.name"`
	// Status is a semconv.TestSuiteRunStatus value: success with exit code
	// ExitPass, failure with ExitFail, aborted or timed_out with ExitError.
	Status   string `json:"test.suite.run.status"`
	ExitCode int    `json:"exitCode"`
	// Failures and Warnings are one line each, for failed assertions of
	// severity fail and warn and for run errors.
	Failures     []string          `json:"failures"`
	Warnings     []string          `json:"warnings"`
	KnownDefects []KnownDefect     `json:"knownDefects"`
	Assertions   []AssertionResult `json:"assertions"`

	// Spec is the cluster spec path; TelemetryMode, PowerScope, InputPreset
	// and Timing are the harness settings of the run.
	Spec          string `json:"spec,omitempty"`
	TelemetryMode string `json:"telemetryMode,omitempty"`
	PowerScope    string `json:"powerScope,omitempty"`
	InputPreset   string `json:"inputPreset,omitempty"`
	Timing        string `json:"timing,omitempty"`
	Git           Git    `json:"git"`
	// Images maps each images.env name to its reference.
	Images map[string]string `json:"images,omitempty"`
	// SemconvSchemaURL is semconv.SchemaURL.
	SemconvSchemaURL string `json:"semconvSchemaUrl"`
	// DurationsSeconds maps each phase of the run (such as up, deploy, run,
	// evaluate) to its wall time.
	DurationsSeconds map[string]float64 `json:"durationsSeconds,omitempty"`

	// Scenario runs only. The summary of a scenario without variants, and
	// each variant's own, has Jobs, Classes and Energy; the top-level summary
	// of a scenario with variants has Variants and Comparisons instead.
	Scenario    string                  `json:"scenario,omitempty"`
	Variant     string                  `json:"variant,omitempty"`
	RunID       string                  `json:"runId,omitempty"`
	StartTime   *time.Time              `json:"startTime,omitempty"`
	EndTime     *time.Time              `json:"endTime,omitempty"`
	Jobs        *JobsSummary            `json:"jobs,omitempty"`
	Classes     map[string]ClassSummary `json:"classes,omitempty"`
	Energy      *EnergySummary          `json:"energy,omitempty"`
	Variants    []VariantResult         `json:"variants,omitempty"`
	Comparisons []ComparisonResult      `json:"comparisons,omitempty"`
}

// MarshalJSON writes s with its nil Failures, Warnings, KnownDefects and
// Assertions as empty lists.
func (s Summary) MarshalJSON() ([]byte, error) {
	type plain Summary
	p := plain(s)
	if p.Failures == nil {
		p.Failures = []string{}
	}
	if p.Warnings == nil {
		p.Warnings = []string{}
	}
	if p.KnownDefects == nil {
		p.KnownDefects = []KnownDefect{}
	}
	if p.Assertions == nil {
		p.Assertions = []AssertionResult{}
	}
	return json.Marshal(p)
}

// Git is the commit the run was built from.
type Git struct {
	SHA   string `json:"sha"`
	Dirty bool   `json:"dirty"`
}

// ReadGit returns the commit of the working tree at repo and whether the
// tree has changes, as the scripts name a run directory (sim_git_state in
// scripts/simcluster/lib.sh); the zero Git outside a git checkout.
func ReadGit(repo string) Git {
	sha, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		return Git{}
	}
	g := Git{SHA: strings.TrimSpace(string(sha))}
	if status, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output(); err == nil {
		g.Dirty = len(strings.TrimSpace(string(status))) > 0
	}
	return g
}

// KnownDefect is a check that reproduced a registered defect: ID is the
// registry's id (such as D1), Assertion the check's test.case.name, Observed
// what was seen.
type KnownDefect struct {
	ID        string `json:"id"`
	Assertion string `json:"assertion"`
	Observed  string `json:"observed"`
}

// AssertionResult is one evaluated check, an AssertionCheck of an
// assertion: Name is the check's name, which is the assertion's name or,
// for an assertion with one comparator per key, <assertion>.<key>, and
// Expected is its comparator. An assertion of several checks gives one
// result per check. For each result the runner, and the smoke test for its
// own checks, writes one joulie.scenario.assertion.evaluated record and one
// joulie.scenario.assertion.value point per source, keyed by the same name,
// so no two observed values share a series.
//
// Status is a semconv.TestCaseResultStatus value; a failed check of
// severity warn has status fail and does not change the exit code. Type and
// Severity are the assertion's. Observed is unset when there was no data,
// which fails the check.
type AssertionResult struct {
	Name     string        `json:"test.case.name"`
	Status   string        `json:"test.case.result.status"`
	Type     AssertionType `json:"type"`
	Severity Severity      `json:"severity"`
	Observed *float64      `json:"observed,omitempty"`
	Expected string        `json:"expected,omitempty"`
	Message  string        `json:"message,omitempty"`
}

// ComparisonResult is one evaluated Compare; Observed is the ratio.
type ComparisonResult struct {
	Name      string   `json:"test.case.name"`
	Status    string   `json:"test.case.result.status"`
	Metric    string   `json:"metric"`
	Variant   string   `json:"variant"`
	Reference string   `json:"reference"`
	Severity  Severity `json:"severity"`
	Observed  *float64 `json:"observed,omitempty"`
	Expected  string   `json:"expected,omitempty"`
	Message   string   `json:"message,omitempty"`
}

// VariantResult points the top-level summary at a variant's own: Summary is
// the path relative to the run directory.
type VariantResult struct {
	Name     string `json:"name"`
	Status   string `json:"test.suite.run.status"`
	ExitCode int    `json:"exitCode"`
	Summary  string `json:"summary"`
}

// JobsSummary counts a variant's jobs. StalledEver counts jobs with at
// least one stall event; NeverScheduled counts due jobs never bound.
type JobsSummary struct {
	Total          int `json:"total"`
	NotDue         int `json:"notDue"`
	Completed      int `json:"completed"`
	Failed         int `json:"failed"`
	Incomplete     int `json:"incomplete"`
	StalledEver    int `json:"stalledEver"`
	NeverScheduled int `json:"neverScheduled"`
}

// ClassSummary is the result of one workload class. Durations are in
// seconds, over completed jobs.
type ClassSummary struct {
	Jobs                  int          `json:"jobs"`
	Completed             int          `json:"completed"`
	ThroughputJobsPerHour float64      `json:"throughputJobsPerHour"`
	WorkUnits             WorkUnits    `json:"workUnits"`
	EnergyJoules          EnergyJoules `json:"energyJoules"`
	EnergyPerJobJoules    float64      `json:"energyPerJobJoules"`
	Duration              Quantiles    `json:"duration"`
	QueueWait             Quantiles    `json:"queueWait"`
	Slowdown              Quantiles    `json:"slowdown"`
}

// WorkUnits is work done, in trace work units.
type WorkUnits struct {
	CPU float64 `json:"cpu"`
	GPU float64 `json:"gpu"`
}

// EnergyJoules is attributed energy.
type EnergyJoules struct {
	CPU   float64 `json:"cpu"`
	GPU   float64 `json:"gpu"`
	Total float64 `json:"total"`
}

// Quantiles summarises a per-job value.
type Quantiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

// EnergySummary is the energy of a variant inside EnergyBoundary:
// TotalJoules is AttributedJoules plus IdleJoules.
type EnergySummary struct {
	AttributedJoules float64 `json:"attributedJoules"`
	IdleJoules       float64 `json:"idleJoules"`
	TotalJoules      float64 `json:"totalJoules"`
	Boundary         string  `json:"boundary"`
	IdleSplit        string  `json:"idleSplit"`
}

// RunData is one variant's results as the evaluator reads them from its
// directory: every OTLP record of the simulator and harness files,
// flattened, and the Kubernetes snapshots. It lives in memory only.
type RunData struct {
	// Dir is the variant directory.
	Dir string
	// Scenario is the scenario with this variant applied and Variants
	// cleared. Variant is "" for a scenario without variants.
	Scenario Scenario
	Variant  string
	RunID    string
	// Start and End bound the run: the run request and the finish.
	Start, End time.Time
	// Status is the content of RunStatusFile; nil when the run never
	// started.
	Status *workload.RunStatus
	// Events are the log records of every logs file, ordered by Time.
	Events []Event
	// Points are the metric points of every metrics file, in file order.
	Points []Point
	// KubeStart and KubeEnd are the snapshots; KubeStart holds only the
	// kinds taken at SnapshotStart.
	KubeStart, KubeEnd KubeSnapshot
}

// Event is one OTLP LogRecord with an eventName.
type Event struct {
	// Name is the eventName, one of the semconv Event names.
	Name string
	// Time is timeUnixNano, when the event occurred; ObservedTime is
	// observedTimeUnixNano, when it was emitted.
	Time, ObservedTime time.Time
	Severity           int
	Body               string
	// File is the logs file, relative to the variant directory.
	File       string
	Resource   Attributes
	Attributes Attributes
}

// Point is one metric data point.
type Point struct {
	Metric string
	Unit   string
	// Kind is semconv.Counter for a monotonic Sum, semconv.UpDownCounter for
	// another Sum, semconv.Gauge or semconv.Histogram.
	Kind semconv.Kind
	// StartTime is startTimeUnixNano (zero for a Gauge) and Time
	// timeUnixNano.
	StartTime, Time time.Time
	// Value is the point of a Sum or Gauge, an int converted to float64.
	Value float64
	// Histogram is set for a Histogram point instead of Value.
	Histogram *HistogramValue
	// File is the metrics file, relative to the variant directory.
	File       string
	Resource   Attributes
	Attributes Attributes
}

// HistogramValue is an explicit-bucket histogram point: Counts has one more
// element than Bounds.
type HistogramValue struct {
	Count  uint64
	Sum    float64
	Bounds []float64
	Counts []uint64
}

// Attributes are OTLP attributes by key. A value is a string, bool, int64,
// float64, or a []any of those.
type Attributes map[string]any

// String returns the string value of key.
func (a Attributes) String(key string) (string, bool) {
	v, ok := a[key].(string)
	return v, ok
}

// Bool returns the bool value of key.
func (a Attributes) Bool(key string) (bool, bool) {
	v, ok := a[key].(bool)
	return v, ok
}

// Int returns the int64 value of key; a double is not converted.
func (a Attributes) Int(key string) (int64, bool) {
	v, ok := a[key].(int64)
	return v, ok
}

// Float returns the value of key as a float64, an int64 converted.
func (a Attributes) Float(key string) (float64, bool) {
	switch v := a[key].(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// KubeSnapshot is the Kubernetes objects at one moment, read through the
// typed structs.
type KubeSnapshot struct {
	Time          time.Time
	NodeTwins     []v1alpha1.NodeTwin
	NodeHardwares []v1alpha1.NodeHardware
	Nodes         []corev1.Node
	Pods          []corev1.Pod
	Jobs          []batchv1.Job
	Events        []corev1.Event
}
