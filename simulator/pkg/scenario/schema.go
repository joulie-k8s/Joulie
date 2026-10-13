package scenario

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// SchemaVersion is the schemaVersion of a scenario file.
const SchemaVersion = 1

// Scenario is one scenario file. Durations are Go duration strings, such as
// 4m or 90s. A nil pointer is unset and takes the default the comment names.
type Scenario struct {
	SchemaVersion int `json:"schemaVersion"`
	// Name equals the file stem.
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Cluster     Cluster `json:"cluster"`
	// Seed seeds every random choice of the plan, workloadgen's included.
	Seed int64 `json:"seed,omitempty"`
	// Duration is the submission window, at most 6h; jobs due later are
	// never submitted. Drain is the extra time for submitted jobs to finish.
	Duration metav1.Duration  `json:"duration"`
	Drain    *metav1.Duration `json:"drain,omitempty"`
	// AbortOnRunStalled ends the run early once joulie.sim.run.stalled has
	// lasted this long; unset, a stalled run waits for its deadline.
	AbortOnRunStalled *metav1.Duration `json:"abortOnRunStalled,omitempty"`
	// Isolation, unset, is IsolationCluster with more than one variant and
	// IsolationReuse with one; IsolationReuse is refused with more than one.
	Isolation Isolation `json:"isolation,omitempty"`
	Workload  Workload  `json:"workload"`
	Joulie    Joulie    `json:"joulie"`
	Simulator Simulator `json:"simulator"`
	// Variants, when listed, are what runs: the top level is their shared
	// base and never runs by itself. Without variants the top level runs
	// once, as the unnamed variant "" (see Variant).
	Variants   []Variant   `json:"variants,omitempty"`
	Capture    Capture     `json:"capture"`
	Assertions []Assertion `json:"assertions,omitempty"`
	Compare    []Compare   `json:"compare,omitempty"`
}

// Cluster names the cluster spec, a path relative to the repository root,
// and the harness timing profile: dev or production.
type Cluster struct {
	Spec   string `json:"spec"`
	Timing string `json:"timing,omitempty"`
}

// Isolation says whether variants share one cluster.
type Isolation string

const (
	// IsolationCluster runs every variant on a fresh cluster, so no
	// variant sees the limits another wrote.
	IsolationCluster Isolation = "cluster"
	// IsolationReuse keeps the cluster; it is allowed with one variant only.
	IsolationReuse Isolation = "reuse"
)

// Workload is the workload of a run: the run options and exactly one
// source, Jobs, Generate or Trace. Namespace, Owner, Intent, Completion,
// JobTTL, StallReport, BaseSpeedPerCore and Placement become the run's
// workload.RunOptions; unset, the simulator's settings apply, except for
// BaseSpeedPerCore, which is always sent.
type Workload struct {
	Namespace   string              `json:"namespace,omitempty"`
	Owner       workload.Owner      `json:"owner,omitempty"`
	Intent      workload.Intent     `json:"intent,omitempty"`
	Completion  workload.Completion `json:"completion,omitempty"`
	JobTTL      *metav1.Duration    `json:"jobTTL,omitempty"`
	StallReport *metav1.Duration    `json:"stallReport,omitempty"`
	// BaseSpeedPerCore is the work units one core does per second at full
	// speed, 1 when unset. The runner sizes the trace with it (workloadgen
	// -base-speed-per-core, or the work of inline Jobs; a Trace file must
	// have been generated with it) and sends the same value as
	// RunOptions.BaseSpeedPerCore, so the simulator converts work to time at
	// that speed whatever SIM_BASE_SPEED_PER_CORE it was deployed with, and
	// ideal runtimes match.
	BaseSpeedPerCore *float64 `json:"baseSpeedPerCore,omitempty"`
	// Placement replaces the simulator's default node selector and
	// tolerations for every pod of the run.
	Placement *Placement `json:"placement,omitempty"`
	Jobs      []JobSpec  `json:"jobs,omitempty"`
	Generate  *Generate  `json:"generate,omitempty"`
	// Trace is a v3 trace file, relative to the repository root.
	Trace string `json:"trace,omitempty"`
}

// Placement is the node selection of a run's pods. A nil field keeps the
// simulator's default and an empty one ({} or []) clears it, so neither is
// omitted when written.
type Placement struct {
	NodeSelector map[string]string   `json:"nodeSelector"`
	Tolerations  []corev1.Toleration `json:"tolerations"`
}

// JobSpec is one kind of inline job, rendered to v3 trace records: Count
// groups submitted Every apart from the run start, each of Pods pods (a gang
// when Gang is set). Runtime is the ideal runtime: the work units are
// Runtime x request x base speed per core.
type JobSpec struct {
	Name string `json:"name"`
	// Class is the intentClass: performance or standard.
	Class string `json:"class"`
	// Count is 1 and Pods is 1 when unset.
	Count int              `json:"count,omitempty"`
	Every *metav1.Duration `json:"every,omitempty"`
	Pods  int              `json:"pods,omitempty"`
	Gang  bool             `json:"gang,omitempty"`
	// Requests holds cpu and memory; GPUs go in GPUs.
	Requests corev1.ResourceList `json:"requests"`
	GPUs     *GPURequest         `json:"gpus,omitempty"`
	Runtime  metav1.Duration     `json:"runtime"`
	// WorkClass is the trace's workloadClass.
	WorkClass    WorkClass           `json:"workClass,omitempty"`
	Profile      Profile             `json:"profile,omitempty"`
	Sensitivity  *Sensitivity        `json:"sensitivity,omitempty"`
	Affinity     *corev1.Affinity    `json:"affinity,omitempty"`
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`
}

// GPURequest is Count units of one integer extended resource:
// nvidia.com/gpu, amd.com/gpu, nvidia.com/mig-<profile> or
// nvidia.com/gpu.shared.
type GPURequest struct {
	Resource string `json:"resource"`
	Count    int    `json:"count"`
}

// WorkClass is the physics class of each kind of work, such as
// cpu.compute_bound or gpu.memory_bound.
type WorkClass struct {
	CPU string `json:"cpu,omitempty"`
	GPU string `json:"gpu,omitempty"`
}

// Profile is the trace's workloadProfile, each value in 0..1. An unset value
// takes the renderer's default; 0 is a value, as for CPUFeedIntensityGPU,
// where it turns the CPU feed factor off. PeerTransferIntensity is the
// share of a job's GPU time spent moving data between the GPUs it holds on
// one node; unset, like 0, means no transfers.
type Profile struct {
	CPUUtilization        *float64 `json:"cpuUtilization,omitempty"`
	GPUUtilization        *float64 `json:"gpuUtilization,omitempty"`
	MemoryIntensity       *float64 `json:"memoryIntensity,omitempty"`
	IOIntensity           *float64 `json:"ioIntensity,omitempty"`
	CPUFeedIntensityGPU   *float64 `json:"cpuFeedIntensityGpu,omitempty"`
	PeerTransferIntensity *float64 `json:"peerTransferIntensity,omitempty"`
}

// Sensitivity is the trace's sensitivity, each in 0..1; unset means 1.
type Sensitivity struct {
	CPU *float64 `json:"cpu,omitempty"`
	GPU *float64 `json:"gpu,omitempty"`
}

// Generate runs workloadgen with the given flags, named without the dash.
// The flags come from an allowlist; schema-version is always v3 and may not
// be given.
type Generate struct {
	Workloadgen ScalarMap `json:"workloadgen"`
}

// Joulie is the Joulie release of a run. Enabled is true unless set false,
// in which case Joulie and its objects are removed before the run.
// InputPreset selects a Joulie input preset (the only way a scenario reaches
// one); Set may not hold NODE_POWER_* or FACILITY_* settings. Set is passed
// to Helm as key=value, Values as values files.
type Joulie struct {
	Enabled     *bool     `json:"enabled,omitempty"`
	InputPreset string    `json:"inputPreset,omitempty"`
	Set         ScalarMap `json:"set,omitempty"`
	Values      []string  `json:"values,omitempty"`
}

// Simulator holds extra Helm settings for the simulator release.
type Simulator struct {
	Set ScalarMap `json:"set,omitempty"`
}

// Variant is one variant of a scenario: the top level with its Joulie and
// Simulator merged in, never replaced whole. Set merges by key, the
// variant's value winning; Values are applied after the top level's;
// Enabled and InputPreset replace the top level's when the variant sets
// them. A variant cannot remove a top-level key or preset, so what differs
// between variants belongs in the variants.
//
// When a scenario lists variants, each runs in its own directory
// <run>/<Name>/ and only they run. A scenario without variants runs the top
// level once as the unnamed variant "": the run directory is its variant
// directory, as for a smoke run, and joulie.scenario.variant is empty.
type Variant struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Joulie      *Joulie    `json:"joulie,omitempty"`
	Simulator   *Simulator `json:"simulator,omitempty"`
}

// Capture is what the runner records during a run besides the simulator's
// own files: the NodeTwin samples every Interval, and each Prometheus query.
type Capture struct {
	Interval   *metav1.Duration `json:"interval,omitempty"`
	Prometheus []PromCapture    `json:"prometheus,omitempty"`
}

// PromCapture is a PromQL query recorded as a Gauge named Name, which starts
// with semconv.CaptureMetricPrefix, with the series' labels as attributes.
type PromCapture struct {
	Name  string `json:"name"`
	Unit  string `json:"unit,omitempty"`
	Query string `json:"query"`
}

// Scalar is a YAML scalar as text: a string, a number or a boolean. It
// marshals as a string. A quoted value keeps its text. An unquoted one is
// read by YAML 1.1 rules first, as Helm reads a values file, and written
// back in its shortest float64 form: 600 gives "600", 1.10 gives "1.1", 1e3
// gives "1000", 010 (octal) gives "8" and on gives "true". Quote a value
// whose text matters, such as an image tag or a version.
type Scalar string

// UnmarshalJSON accepts a JSON string, number or boolean and keeps its text.
func (s *Scalar) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*s = Scalar(str)
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch v.(type) {
	case float64, bool:
		*s = Scalar(b)
		return nil
	}
	return fmt.Errorf("want a string, number or boolean, got %s", b)
}

// MarshalJSON writes s as a JSON string.
func (s Scalar) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(s))
}

// ScalarMap is a map of Scalars, such as Helm --set values or workloadgen
// flags. It decodes itself, so sigs.k8s.io/yaml hands it unquoted numbers
// unchanged; into a plain map[string]Scalar it formats them at float32
// precision first (0.1234567891234 becomes 0.12345679).
type ScalarMap map[string]Scalar

// UnmarshalJSON decodes a JSON object of Scalars; null leaves m nil.
func (m *ScalarMap) UnmarshalJSON(b []byte) error {
	var out map[string]Scalar
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	*m = out
	return nil
}

// Comparator is an expectation such as ">= 0.95", "== 0" or "< 90s": an
// operator, a number and an optional unit, matching ComparatorPattern. A
// time unit compares against seconds and an energy unit against joules.
type Comparator string

// ComparatorPattern is the grammar of a Comparator.
const ComparatorPattern = `^(<=|>=|<|>|==|!=)\s*-?[0-9]+(\.[0-9]+)?(e[+-]?[0-9]+)?\s*(ms|s|min|h|J|kJ|MJ|W)?$`

// Severity is what a failed assertion does to the run: SeverityFail (the
// default when unset) fails it, SeverityWarn adds a warning.
type Severity string

const (
	SeverityFail Severity = "fail"
	SeverityWarn Severity = "warn"
)

// AssertionType selects the fields of an assertion.
type AssertionType string

const (
	AssertJobs      AssertionType = "jobs"
	AssertLatency   AssertionType = "latency"
	AssertStall     AssertionType = "stall"
	AssertPlacement AssertionType = "placement"
	AssertEnergy    AssertionType = "energy"
	AssertEvents    AssertionType = "events"
	AssertObjects   AssertionType = "objects"
	AssertTwinPower AssertionType = "twinPower"
	AssertPromQL    AssertionType = "promql"
	// AssertCompare is the type of a Compare result in the summary. It is
	// not an assertion type: comparisons are the top-level compare list.
	AssertCompare AssertionType = "compare"
)

// AssertionTypes returns the types an assertion may have, sorted.
func AssertionTypes() []AssertionType {
	out := make([]AssertionType, 0, len(newAssertionSpec))
	for t := range newAssertionSpec {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Assertion is one check of a run. In the file it is one flat object: name,
// type and severity, plus the fields of its type, which decode strictly into
// the one pointer below that the type selects; the others stay nil. An
// assertion with no data to evaluate fails with "no data".
type Assertion struct {
	Name     string        `json:"-"`
	Type     AssertionType `json:"-"`
	Severity Severity      `json:"-"`

	Jobs      *JobsAssertion      `json:"-"`
	Latency   *LatencyAssertion   `json:"-"`
	Stall     *StallAssertion     `json:"-"`
	Placement *PlacementAssertion `json:"-"`
	Energy    *EnergyAssertion    `json:"-"`
	Events    *EventsAssertion    `json:"-"`
	Objects   *ObjectsAssertion   `json:"-"`
	TwinPower *TwinPowerAssertion `json:"-"`
	PromQL    *PromQLAssertion    `json:"-"`
}

// newAssertionSpec sets the pointer of a type to a new value and returns it.
var newAssertionSpec = map[AssertionType]func(a *Assertion) any{
	AssertJobs:      func(a *Assertion) any { a.Jobs = &JobsAssertion{}; return a.Jobs },
	AssertLatency:   func(a *Assertion) any { a.Latency = &LatencyAssertion{}; return a.Latency },
	AssertStall:     func(a *Assertion) any { a.Stall = &StallAssertion{}; return a.Stall },
	AssertPlacement: func(a *Assertion) any { a.Placement = &PlacementAssertion{}; return a.Placement },
	AssertEnergy:    func(a *Assertion) any { a.Energy = &EnergyAssertion{}; return a.Energy },
	AssertEvents:    func(a *Assertion) any { a.Events = &EventsAssertion{}; return a.Events },
	AssertObjects:   func(a *Assertion) any { a.Objects = &ObjectsAssertion{}; return a.Objects },
	AssertTwinPower: func(a *Assertion) any { a.TwinPower = &TwinPowerAssertion{}; return a.TwinPower },
	AssertPromQL:    func(a *Assertion) any { a.PromQL = &PromQLAssertion{}; return a.PromQL },
}

// Spec returns the type's fields, the one non-nil pointer, or nil.
func (a Assertion) Spec() any {
	switch {
	case a.Jobs != nil:
		return a.Jobs
	case a.Latency != nil:
		return a.Latency
	case a.Stall != nil:
		return a.Stall
	case a.Placement != nil:
		return a.Placement
	case a.Energy != nil:
		return a.Energy
	case a.Events != nil:
		return a.Events
	case a.Objects != nil:
		return a.Objects
	case a.TwinPower != nil:
		return a.TwinPower
	case a.PromQL != nil:
		return a.PromQL
	}
	return nil
}

// AssertionCheck is one comparator of an assertion and the test.case.name
// of its result (see AssertionResult).
type AssertionCheck struct {
	Name   string
	Expect Comparator
}

// Checks returns the checks of a, in the order its type declares them. An
// assertion whose Expect is one Comparator is one check named a.Name. The
// jobs, stall and objects types hold one comparator per key of their
// Expect: each set key is a check named <a.Name>.<key>, with the key as the
// file spells it, such as clean.strayPodsOnFakeNodes. An Expect with no key
// set gives no check, which validation refuses.
func (a Assertion) Checks() []AssertionCheck {
	spec := a.Spec()
	if spec == nil {
		return nil
	}
	expect := reflect.ValueOf(spec).Elem().FieldByName("Expect")
	if !expect.IsValid() {
		return nil
	}
	if expect.Kind() == reflect.String {
		return []AssertionCheck{{Name: a.Name, Expect: Comparator(expect.String())}}
	}
	var out []AssertionCheck
	for i := 0; i < expect.NumField(); i++ {
		c := Comparator(expect.Field(i).String())
		if c == "" {
			continue
		}
		key, _, _ := strings.Cut(expect.Type().Field(i).Tag.Get("json"), ",")
		out = append(out, AssertionCheck{Name: a.Name + "." + key, Expect: c})
	}
	return out
}

// UnmarshalJSON decodes the flat object: name, type and severity, then the
// remaining keys strictly into the type's struct. An unknown type, a missing
// type or a key the type does not have is an error.
func (a *Assertion) UnmarshalJSON(b []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	var out Assertion
	rest := map[string]json.RawMessage{}
	for k, v := range fields {
		var err error
		switch k {
		case "name":
			err = json.Unmarshal(v, &out.Name)
		case "type":
			err = json.Unmarshal(v, &out.Type)
		case "severity":
			err = json.Unmarshal(v, &out.Severity)
		default:
			rest[k] = v
		}
		if err != nil {
			return fmt.Errorf("assertion %s: %w", k, err)
		}
	}
	if out.Type == "" {
		return fmt.Errorf("assertion %q: no type", out.Name)
	}
	newSpec, ok := newAssertionSpec[out.Type]
	if !ok {
		names := make([]string, 0, len(newAssertionSpec))
		for _, t := range AssertionTypes() {
			names = append(names, string(t))
		}
		return fmt.Errorf("assertion %q: unknown type %q, want one of %s (compare is the top-level list)",
			out.Name, out.Type, strings.Join(names, ", "))
	}
	spec := newSpec(&out)
	restJSON, err := json.Marshal(rest)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(restJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(spec); err != nil {
		return fmt.Errorf("assertion %q (type %s): %w", out.Name, out.Type, err)
	}
	*a = out
	return nil
}

// MarshalJSON writes the flat object UnmarshalJSON reads.
func (a Assertion) MarshalJSON() ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if spec := a.Spec(); spec != nil {
		b, err := json.Marshal(spec)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &fields); err != nil {
			return nil, err
		}
	}
	common := map[string]string{"name": a.Name, "type": string(a.Type)}
	if a.Severity != "" {
		common["severity"] = string(a.Severity)
	}
	for k, v := range common {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		fields[k] = b
	}
	return json.Marshal(fields)
}

// JobFilter selects jobs. An empty field selects everything. NodeProfile is
// the node's joulie.io/power-profile when the job's pod was bound.
type JobFilter struct {
	Class       string `json:"class,omitempty"`
	Workload    string `json:"workload,omitempty"`
	NodeProfile string `json:"nodeProfile,omitempty"`
}

// JobsAssertion counts the selected jobs that were due before the scenario's
// duration; notDue jobs are reported, not counted.
type JobsAssertion struct {
	JobFilter
	Expect JobsExpect `json:"expect"`
}

// JobsExpect holds one comparator per count; an empty one is not checked.
// CompletedFraction is completed over due jobs.
type JobsExpect struct {
	CompletedFraction Comparator `json:"completedFraction,omitempty"`
	Completed         Comparator `json:"completed,omitempty"`
	Failed            Comparator `json:"failed,omitempty"`
	Incomplete        Comparator `json:"incomplete,omitempty"`
}

// LatencyMetric is a per-job time or ratio of completed jobs.
type LatencyMetric string

const (
	LatencyDuration  LatencyMetric = "duration"
	LatencyQueueWait LatencyMetric = "queueWait"
	LatencyRunTime   LatencyMetric = "runTime"
	LatencySlowdown  LatencyMetric = "slowdown"
)

// LatencyAssertion takes Quantile, a number in 0.5..1 or max, of Metric over
// the selected jobs. With Reference set, the observed value is that
// quantile over the quantile of the jobs Reference selects, for a ratio
// within one variant.
type LatencyAssertion struct {
	JobFilter
	Metric    LatencyMetric `json:"metric"`
	Quantile  Scalar        `json:"quantile"`
	Reference *JobFilter    `json:"reference,omitempty"`
	Expect    Comparator    `json:"expect"`
}

// StallAssertion counts stall events of each reason.
type StallAssertion struct {
	Expect StallExpect `json:"expect"`
}

// StallExpect holds one comparator per stall reason; an empty one is not
// checked.
type StallExpect struct {
	GangIncomplete Comparator `json:"gangIncomplete,omitempty"`
	NoProgress     Comparator `json:"noProgress,omitempty"`
	RunStalled     Comparator `json:"runStalled,omitempty"`
}

// PlacementAssertion counts pods of Class bound to a node whose schedulable
// class at the bind (the power-profile label when the NodeTwin has none) is
// in Forbidden. A bind counts only when the node had held that class for at
// least GraceSeconds, by default the extender's CACHE_TTL plus the timing's
// reconcile interval.
type PlacementAssertion struct {
	Class        string     `json:"class"`
	Forbidden    []string   `json:"forbidden"`
	GraceSeconds *float64   `json:"graceSeconds,omitempty"`
	Expect       Comparator `json:"expect"`
}

// EnergyAssertion reads joulie.sim.workload.energy at the last snapshot.
// Scope is total, class:<class> or node:<node>; HWType is cpu, gpu or all
// (the default); PerJob divides by the completed jobs of the scope.
type EnergyAssertion struct {
	Scope  string     `json:"scope"`
	HWType string     `json:"hwType,omitempty"`
	PerJob bool       `json:"perJob,omitempty"`
	Expect Comparator `json:"expect"`
}

// EventsAssertion counts the records of Event whose attributes equal every
// Where value, in every logs file of the run.
type EventsAssertion struct {
	Event  string     `json:"event"`
	Where  ScalarMap  `json:"where,omitempty"`
	Expect Comparator `json:"expect"`
}

// ObjectsAssertion checks the Kubernetes objects at the end of the run.
type ObjectsAssertion struct {
	Expect ObjectsExpect `json:"expect"`
}

// ObjectsExpect holds one comparator per count; an empty one is not checked.
type ObjectsExpect struct {
	JobsNotComplete      Comparator `json:"jobsNotComplete,omitempty"`
	StrayPodsOnFakeNodes Comparator `json:"strayPodsOnFakeNodes,omitempty"`
	NodeHardwares        Comparator `json:"nodeHardwares,omitempty"`
	ControlBlocked       Comparator `json:"controlBlocked,omitempty"`
}

// TwinPowerAssertion compares each NodeTwin's measured power with the
// node's energy rate in joulie.sim.workload.energy, after Warmup (by default
// twice the timing's rate window). Tolerance is relative; the observed
// value is the Quantile (default 0.95) of the relative error over the
// samples of Nodes (default all).
type TwinPowerAssertion struct {
	Warmup    *metav1.Duration `json:"warmup,omitempty"`
	Tolerance *float64         `json:"tolerance,omitempty"`
	Quantile  *float64         `json:"quantile,omitempty"`
	Nodes     []string         `json:"nodes,omitempty"`
	Expect    Comparator       `json:"expect"`
}

// PromQLAssertion runs Query, an instant query, At the end of the run (the
// only value), and records the result as joulie.scenario.assertion.value.
type PromQLAssertion struct {
	Query  string     `json:"query"`
	At     string     `json:"at,omitempty"`
	Expect Comparator `json:"expect"`
}

// Compare is a ratio across variants: Metric of Variant over Metric of
// Reference, two names from the scenario's Variants (the top level is not a
// variant once variants are listed, and a scenario without variants has
// nothing to compare). Metric is energy.total, class.<class>.energy,
// class.<class>.<latency metric>.<quantile> or class.<class>.throughput.
type Compare struct {
	Name      string     `json:"name"`
	Metric    string     `json:"metric"`
	Variant   string     `json:"variant"`
	Reference string     `json:"reference"`
	Expect    Comparator `json:"expect"`
	Severity  Severity   `json:"severity,omitempty"`
}
