package workload

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Model is the workload model, SIM_WORKLOAD_MODEL.
type Model string

const (
	// ModelLegacy keeps the behaviour the experiments were measured with.
	// It is the default outside emulate mode and an error inside it.
	ModelLegacy Model = "legacy"
	// ModelV1 is the default in emulate mode and an error outside it.
	ModelV1 Model = "v1"
)

// Owner is what owns a v1 run's pods, SIM_WORKLOAD_OWNER. Legacy always
// creates bare pods.
type Owner string

const (
	// OwnerJob creates one batch/v1 Job per group of trace jobs, Indexed when
	// the group has more than one pod.
	OwnerJob Owner = "job"
	// OwnerPod creates one bare pod per trace job.
	OwnerPod Owner = "pod"
)

// Intent is how a v1 pod declares its workload class, SIM_WORKLOAD_INTENT.
// Legacy always renders the affinity.
type Intent string

const (
	// IntentAnnotation sets AnnotationWorkloadClass, the declaration the
	// scheduler extender and the controller read.
	IntentAnnotation Intent = "annotation"
	// IntentAffinity ANDs a NotIn-eco term on LabelPowerProfile into every
	// required node selector term of a performance pod.
	IntentAffinity Intent = "affinity"
	// IntentBoth does both.
	IntentBoth Intent = "both"
	// IntentNone declares nothing, as a user who does not know Joulie.
	IntentNone Intent = "none"
)

// Completion is how a finished v1 pod ends, SIM_WORKLOAD_COMPLETION. Legacy
// always deletes.
type Completion string

const (
	// CompletionKWOK patches AnnotationComplete onto the pod; the KWOK Stage
	// StagePodComplete then moves it to Succeeded, and the Job controller and
	// the Job's TTL clean up.
	CompletionKWOK Completion = "kwok"
	// CompletionDelete deletes the pod with grace period 0.
	CompletionDelete Completion = "delete"
)

// ParseModel returns the Model s names, or an error listing the valid values.
func ParseModel(s string) (Model, error) {
	return parseEnum(EnvModel, s, ModelLegacy, ModelV1)
}

// ParseOwner returns the Owner s names, or an error listing the valid values.
func ParseOwner(s string) (Owner, error) {
	return parseEnum(EnvOwner, s, OwnerJob, OwnerPod)
}

// ParseIntent returns the Intent s names, or an error listing the valid
// values.
func ParseIntent(s string) (Intent, error) {
	return parseEnum(EnvIntent, s, IntentAnnotation, IntentAffinity, IntentBoth, IntentNone)
}

// ParseCompletion returns the Completion s names, or an error listing the
// valid values.
func ParseCompletion(s string) (Completion, error) {
	return parseEnum(EnvCompletion, s, CompletionKWOK, CompletionDelete)
}

// parseEnum matches s exactly, after trimming spaces, against valid. The
// error names the setting, so it reads the same from the environment and
// from a scenario file.
func parseEnum[T ~string](setting, s string, valid ...T) (T, error) {
	s = strings.TrimSpace(s)
	names := make([]string, len(valid))
	for i, v := range valid {
		if string(v) == s {
			return v, nil
		}
		names[i] = string(v)
	}
	return "", fmt.Errorf("%s: invalid value %q, want one of %s", setting, s, strings.Join(names, ", "))
}

// Workload classes: the values of AnnotationWorkloadClass and of a trace
// job's intentClass. ClassIdle is not a pod's class; it labels the energy no
// job holds in joulie.sim.workload.energy.
const (
	ClassPerformance = "performance"
	ClassStandard    = "standard"
	ClassIdle        = "idle"
)

// Labels on a run's Job objects and pods. The pod-to-job mapping also uses
// batchv1.JobNameLabel and batchv1.JobCompletionIndexAnnotation, which the
// Job controller sets.
const (
	// LabelPartOf with value PartOfWorkload marks every workload pod, legacy
	// and v1; the harness's stray-pod check selects it.
	LabelPartOf    = "app.kubernetes.io/part-of"
	PartOfWorkload = "joulie-sim-workload"
	// LabelRunID is RunLabelValue of the run id. The simulator's informer
	// selects a run's pods by it.
	LabelRunID = "sim.joulie.io/run-id"
	// LabelWorkloadType and LabelPodRole are the trace's workloadType and
	// podRole, sanitized to label values.
	LabelWorkloadType = "sim.joulie.io/workload-type"
	LabelPodRole      = "sim.joulie.io/pod-role"
	// LabelPowerProfile is the node label Joulie sets; the simulator reads it
	// for event attributes and never writes it.
	LabelPowerProfile = "joulie.io/power-profile"
)

// Annotations on a run's Job objects and pods.
const (
	// AnnotationRun is the run id, unhashed, on Job objects.
	AnnotationRun = "sim.joulie.io/run"
	// AnnotationJobIDs lists a Job object's trace jobIds, comma separated, in
	// completion index order.
	AnnotationJobIDs = "sim.joulie.io/job-ids"
	// AnnotationGroup is a pod's group key.
	AnnotationGroup = "sim.joulie.io/group"
	// AnnotationJobID, AnnotationWorkloadID, AnnotationWorkloadType and
	// AnnotationPodRole are the trace fields on a legacy pod and, for
	// AnnotationJobID, on a v1 pod with OwnerPod.
	AnnotationJobID        = "sim.joulie.io/jobId"
	AnnotationWorkloadID   = "sim.joulie.io/workloadId"
	AnnotationWorkloadType = "sim.joulie.io/workloadType"
	AnnotationPodRole      = "sim.joulie.io/podRole"
	// The trace's workload profile on every pod: utilizations and memory
	// pressure in percent with one decimal, I/O intensity in 0..1 with two.
	AnnotationCPUUtilPct        = "sim.joulie.io/cpu-util-pct"
	AnnotationGPUUtilPct        = "sim.joulie.io/gpu-util-pct"
	AnnotationMemoryPressurePct = "sim.joulie.io/memory-pressure-pct"
	AnnotationIOIntensity       = "sim.joulie.io/io-intensity"
	// AnnotationComplete with value CompleteValue is the completion patch
	// the StagePodComplete Stage selects; AnnotationCompletedAt, an
	// RFC3339Nano time, goes in the same patch.
	AnnotationComplete    = "sim.joulie.io/complete"
	CompleteValue         = "true"
	AnnotationCompletedAt = "sim.joulie.io/completed-at"
	// AnnotationWorkloadClass is the user's declaration of a pod's class,
	// ClassPerformance or ClassStandard; it equals the controller's
	// fsm.WorkloadClassAnnotation.
	AnnotationWorkloadClass = "joulie.io/workload-class"
)

// Pod shape.
const (
	// StagePodComplete names the KWOK Stage that moves an annotated Running
	// pod to Succeeded. It replaces upstream's timer-driven Stage of the same
	// name.
	StagePodComplete = "pod-complete"
	// ContainerName is the one container of a workload pod.
	ContainerName = "work"
	// DefaultImage is the container image unless SIM_WORKLOAD_IMAGE is set.
	DefaultImage = "registry.k8s.io/pause:3.9"
)

// Trace schema. Every record carries "type" and "schemaVersion".
const (
	// TraceSchemaV2 is the schema the legacy callers pin with workloadgen
	// -schema-version v2.
	TraceSchemaV2 = "v2"
	// TraceSchemaV3 omits sensitivity unless it is given, writes no
	// power-profile affinity, and sizes work so the ideal runtime equals
	// durationSec at the base speed per core.
	TraceSchemaV3 = "v3"
	// TraceRecordJob is one pod; TraceRecordWorkload describes the group
	// its jobs belong to.
	TraceRecordJob      = "job"
	TraceRecordWorkload = "workload"
)

// Environment variables of the workload engine. The comment says which model
// reads each; a v1-only variable set in legacy mode logs one warning.
const (
	// EnvModel selects the Model; unset, it is ModelV1 in emulate mode and
	// ModelLegacy otherwise.
	EnvModel = "SIM_WORKLOAD_MODEL"
	// EnvTracePath is a trace loaded at startup; in v1 it starts an implicit
	// run with id EnvRunID.
	EnvTracePath = "SIM_WORKLOAD_TRACE_PATH"
	// EnvTick is the period of the legacy loop, legacy only: a v1 run ticks
	// on the emulator's tick, and v1 warns when it is set. A value of 0 or
	// less warns and takes the default, 1s.
	EnvTick = "SIM_WORKLOAD_TICK"
	// EnvImage is the container image, both models.
	EnvImage = "SIM_WORKLOAD_IMAGE"
	// EnvBaseSpeedPerCore is the work units one core does per second at
	// full speed, both models; RunOptions.BaseSpeedPerCore replaces it for
	// one v1 run.
	EnvBaseSpeedPerCore = "SIM_BASE_SPEED_PER_CORE"
	// EnvOwner, EnvIntent and EnvCompletion are read in v1 only.
	EnvOwner      = "SIM_WORKLOAD_OWNER"
	EnvIntent     = "SIM_WORKLOAD_INTENT"
	EnvCompletion = "SIM_WORKLOAD_COMPLETION"
	// EnvNamespace overrides the trace's namespace, v1 only.
	EnvNamespace = "SIM_WORKLOAD_NAMESPACE"
	// EnvNodeSelector is k=v pairs, comma separated, v1 only. Unset, it is
	// sim.joulie.io/cluster=<spec name>, so no pod lands on the infra node.
	EnvNodeSelector = "SIM_WORKLOAD_NODE_SELECTOR"
	// EnvTolerations is a JSON list of corev1.Toleration, v1 only. Unset, it
	// is the KWOK taint.
	EnvTolerations = "SIM_WORKLOAD_TOLERATIONS"
	// EnvJobTTL is the Job objects' ttlSecondsAfterFinished, v1 only.
	EnvJobTTL = "SIM_WORKLOAD_JOB_TTL"
	// EnvMaxCreatesPerTick caps creates per tick; Unlimited removes the
	// cap. Both models.
	EnvMaxCreatesPerTick = "SIM_WORKLOAD_MAX_CREATES_PER_TICK"
	// EnvMaxDeletesPerTick caps completion deletes per tick, legacy only.
	EnvMaxDeletesPerTick = "SIM_WORKLOAD_MAX_DELETES_PER_TICK"
	// EnvKubeQPS and EnvKubeBurst set the workload client's rate limits,
	// both models.
	EnvKubeQPS   = "SIM_WORKLOAD_KUBE_QPS"
	EnvKubeBurst = "SIM_WORKLOAD_KUBE_BURST"
	// EnvStallReport is how long a job, or the whole run, must make no
	// progress before a stall event, v1 only.
	EnvStallReport = "SIM_WORKLOAD_STALL_REPORT"
	// EnvStallForceComplete is how long a legacy job may stall before it is
	// completed by force; Unlimited disables it. Legacy only.
	EnvStallForceComplete = "SIM_WORKLOAD_STALL_FORCE_COMPLETE"
	// EnvRunsDir holds the run directories, v1 only. Unset, it is
	// <SIM_HWEMU_ROOT>/RunsDirName.
	EnvRunsDir = "SIM_RUNS_DIR"
	// EnvRunID is the id of the implicit run of a startup trace, v1 only.
	// Unset, one is generated.
	EnvRunID = "SIM_RUN_ID"
	// EnvOTLPFileInterval is the period of the metrics snapshot written to
	// SimulatorMetricsFile, v1 only.
	EnvOTLPFileInterval = "SIM_OTLP_FILE_INTERVAL"
)

// EnvNames returns every environment variable above, sorted.
func EnvNames() []string {
	out := []string{
		EnvModel, EnvTracePath, EnvTick, EnvImage, EnvBaseSpeedPerCore,
		EnvOwner, EnvIntent, EnvCompletion, EnvNamespace, EnvNodeSelector, EnvTolerations, EnvJobTTL,
		EnvMaxCreatesPerTick, EnvMaxDeletesPerTick, EnvKubeQPS, EnvKubeBurst,
		EnvStallReport, EnvStallForceComplete, EnvRunsDir, EnvRunID, EnvOTLPFileInterval,
	}
	sort.Strings(out)
	return out
}

// Unlimited is the explicit "no cap" of a per-tick limit and the "disabled"
// of a force-complete timeout. 0 is never used for either, so the zero value
// of the options keeps the legacy limits.
const Unlimited = -1

// Defaults of ModelV1.
const (
	DefaultOwner             = OwnerJob
	DefaultIntent            = IntentAnnotation
	DefaultCompletion        = CompletionKWOK
	DefaultJobTTL            = 10 * time.Minute
	DefaultMaxCreatesPerTick = Unlimited
	DefaultKubeQPS           = 100
	DefaultKubeBurst         = 200
	DefaultStallReport       = 5 * time.Minute
	DefaultOTLPFileInterval  = 10 * time.Second
)

// Values of ModelLegacy: the limits the legacy experiments ran with, and the
// legacy defaults of EnvMaxCreatesPerTick, EnvMaxDeletesPerTick,
// EnvStallForceComplete, EnvKubeQPS and EnvKubeBurst.
const (
	LegacyMaxCreatesPerTick  = 20
	LegacyMaxDeletesPerTick  = 10
	LegacyStallForceComplete = 120 * time.Second
	LegacyKubeQPS            = 30
	LegacyKubeBurst          = 50
)

// Run directory of a v1 run: <runs dir>/<runId>/ holds TraceFile and the
// OTLP files under OTLPDir.
const (
	// RunsDirName is the runs directory under SIM_HWEMU_ROOT. The underscore
	// keeps it apart from node trees, since a node name has none.
	RunsDirName = "_runs"
	// TraceFile is the run's trace unless the run request names another.
	TraceFile = "trace.jsonl"
	// OTLPDir holds the run's OTLP JSON Lines files.
	OTLPDir = "otlp"
	// SimulatorMetricsFile holds one MetricsData per line, a cumulative
	// snapshot every SIM_OTLP_FILE_INTERVAL; SimulatorLogsFile one LogsData
	// per tick that has records.
	SimulatorMetricsFile = "simulator.metrics.jsonl"
	SimulatorLogsFile    = "simulator.logs.jsonl"
)

// ScopeName is the instrumentation scope of a run's records.
const ScopeName = "github.com/matbun/joulie/simulator/pkg/workload"

// RunLabelValue is the LabelRunID value of a run: the first 10 hex
// characters of the SHA-256 of runID, so any run id fits a label value.
func RunLabelValue(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return hex.EncodeToString(sum[:])[:10]
}

// Run API, served by the simulator in v1 and emulate mode only. Every other
// mode answers 412.
const (
	// PathRuns: POST a RunRequest. 201 with RunStarted; 409 while a run is
	// active; 400 with a RunError naming the trace line.
	PathRuns = "/sim/v1/runs"
	// PathRunCurrent: GET the RunStatus of the active or last run.
	PathRunCurrent = "/sim/v1/runs/current"
	// PathRunFinish: POST to end the run; the remaining jobs end incomplete.
	// With QueryCleanup=true the run's Job objects are deleted in the
	// background. 200 with the final RunStatus once the files are closed.
	PathRunFinish = "/sim/v1/runs/current/finish"
	QueryCleanup  = "cleanup"
)

// Phase is the state of a run.
type Phase string

const (
	PhaseRunning Phase = "running"
	// PhaseFinished: every job is terminal.
	PhaseFinished Phase = "finished"
	// PhaseAborted: the run was finished by request with jobs remaining, or
	// stopped on an error.
	PhaseAborted Phase = "aborted"
)

// RunRequest starts a run.
type RunRequest struct {
	RunID string `json:"runId"`
	// Trace is a path inside the simulator, by default
	// <runs dir>/<RunID>/TraceFile.
	Trace string `json:"trace,omitempty"`
	// ClusterName is the k8s.cluster.name of the run's records, by default
	// the cluster spec's name.
	ClusterName string     `json:"clusterName,omitempty"`
	Options     RunOptions `json:"options"`
}

// RunOptions override the simulator's settings for one run. A nil or empty
// field keeps the setting, except that an empty, non-nil NodeSelector or
// Tolerations clears it; those two are therefore never omitted, and nil is
// written as null.
type RunOptions struct {
	Owner             Owner               `json:"owner,omitempty"`
	Intent            Intent              `json:"intent,omitempty"`
	Completion        Completion          `json:"completion,omitempty"`
	Namespace         string              `json:"namespace,omitempty"`
	JobTTL            *metav1.Duration    `json:"jobTTL,omitempty"`
	NodeSelector      map[string]string   `json:"nodeSelector"`
	Tolerations       []corev1.Toleration `json:"tolerations"`
	MaxCreatesPerTick *int                `json:"maxCreatesPerTick,omitempty"`
	StallReport       *metav1.Duration    `json:"stallReport,omitempty"`
	// BaseSpeedPerCore replaces SIM_BASE_SPEED_PER_CORE for the run, so work
	// converts to time at the speed the run's trace was sized with.
	BaseSpeedPerCore *float64 `json:"baseSpeedPerCore,omitempty"`
}

// RunStarted answers a successful RunRequest.
type RunStarted struct {
	RunID string `json:"runId"`
	// Jobs is the number of trace jobs and Groups the number of Job objects
	// (with OwnerPod, pods) they become.
	Jobs      int       `json:"jobs"`
	Groups    int       `json:"groups"`
	StartedAt time.Time `json:"startedAt"`
}

// RunStatus is the state of a run.
type RunStatus struct {
	RunID          string    `json:"runId"`
	Phase          Phase     `json:"phase"`
	StartedAt      time.Time `json:"startedAt"`
	ElapsedSeconds float64   `json:"elapsedSeconds"`
	// AllSubmitted is true once every job's object was created.
	AllSubmitted bool         `json:"allSubmitted"`
	Jobs         RunJobCounts `json:"jobs"`
	// RunStalled is true while a joulie.sim.run.stalled has no resume.
	RunStalled bool `json:"runStalled"`
}

// RunJobCounts counts a run's jobs by state. Total is NotDue + Pending +
// Scheduled + Running + Completed + Failed + Incomplete; Stalled counts the
// Running jobs that are stalled now.
type RunJobCounts struct {
	Total int `json:"total"`
	// NotDue jobs have a submit offset still in the future.
	NotDue int `json:"notDue"`
	// Pending jobs are due and not yet bound to a node.
	Pending int `json:"pending"`
	// Scheduled jobs are bound and not yet Running.
	Scheduled  int `json:"scheduled"`
	Running    int `json:"running"`
	Completed  int `json:"completed"`
	Failed     int `json:"failed"`
	Incomplete int `json:"incomplete"`
	Stalled    int `json:"stalled"`
}

// RunError is the body of a 4xx answer. Line is the 1-based trace line of a
// trace error, 0 otherwise.
type RunError struct {
	Message string `json:"error"`
	Line    int    `json:"line,omitempty"`
}

func (e *RunError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("trace line %d: %s", e.Line, e.Message)
	}
	return e.Message
}
