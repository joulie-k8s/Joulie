package scenario

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/simulator/pkg/simcluster"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// Defaults the effective scenario fills in.
const (
	// DefaultCaptureInterval is the NodeTwin capture period.
	DefaultCaptureInterval = 15 * time.Second
	// DefaultBaseSpeedPerCore is workload.baseSpeedPerCore unset, the
	// simulator's own default.
	DefaultBaseSpeedPerCore = 1.0
	// DefaultTwinPowerQuantile is the quantile of a twinPower assertion.
	DefaultTwinPowerQuantile = 0.95
	// DefaultCacheTTL is the scheduler extender's CACHE_TTL unset
	// (cmd/scheduler), one half of the default placement grace.
	DefaultCacheTTL = 30 * time.Second
	// DefaultTraceNamespace is the namespace of rendered trace records when
	// workload.namespace is unset, as workloadgen's -namespace default.
	DefaultTraceNamespace = "default"
	// TracePodRole is the podRole of the pods of an inline job.
	TracePodRole = "worker"
)

// Helm keys the runner reads from or adds to a variant's Joulie settings.
const (
	helmControllerEnv = "controllerManager.env."
	helmExtenderEnv   = "schedulerExtender.env."
	// HelmNodePowerPreset carries joulie.inputPreset, the only way a
	// scenario reaches a node power preset.
	HelmNodePowerPreset = helmControllerEnv + "NODE_POWER_PRESET"
)

// RunVariants returns the variants that run: the names of s.Variants, or
// the unnamed variant "" when there are none.
func (s Scenario) RunVariants() []string {
	if len(s.Variants) == 0 {
		return []string{""}
	}
	out := make([]string, len(s.Variants))
	for i, v := range s.Variants {
		out[i] = v.Name
	}
	return out
}

// ForVariant returns s with variant name applied (see Variant) and Variants
// cleared: the scenario one variant runs. The name "" returns s itself,
// which is valid only when s has no variants.
func (s Scenario) ForVariant(name string) (Scenario, error) {
	out := s
	out.Variants = nil
	out.Joulie = copyJoulie(s.Joulie)
	out.Simulator = Simulator{Set: mergeScalars(s.Simulator.Set, nil)}
	if name == "" {
		if len(s.Variants) > 0 {
			return Scenario{}, fmt.Errorf("scenario %s lists variants; the unnamed variant does not run", s.Name)
		}
		return out, nil
	}
	for _, v := range s.Variants {
		if v.Name != name {
			continue
		}
		if j := v.Joulie; j != nil {
			if j.Enabled != nil {
				enabled := *j.Enabled
				out.Joulie.Enabled = &enabled
			}
			if j.InputPreset != "" {
				out.Joulie.InputPreset = j.InputPreset
			}
			out.Joulie.Set = mergeScalars(out.Joulie.Set, j.Set)
			out.Joulie.Values = append(out.Joulie.Values, j.Values...)
		}
		if v.Simulator != nil {
			out.Simulator.Set = mergeScalars(out.Simulator.Set, v.Simulator.Set)
		}
		return out, nil
	}
	return Scenario{}, fmt.Errorf("scenario %s has no variant %q", s.Name, name)
}

func copyJoulie(j Joulie) Joulie {
	out := j
	if j.Enabled != nil {
		enabled := *j.Enabled
		out.Enabled = &enabled
	}
	out.Set = mergeScalars(j.Set, nil)
	out.Values = append([]string(nil), j.Values...)
	return out
}

// mergeScalars returns a copy of base with over's keys set; nil when both
// are empty.
func mergeScalars(base, over ScalarMap) ScalarMap {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := ScalarMap{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// Effective returns s with the defaults filled in, as plan writes it to
// ScenarioEffectiveFile: timing dev, isolation by variant count, drain 0,
// base speed 1, one group of one pod per inline job, Joulie enabled, a 15 s
// capture interval, severity fail and the twinPower quantile. The defaults
// that depend on a variant's settings (placement grace, twinPower warmup)
// are not filled in; the evaluator takes them from VariantEnv.
func Effective(s Scenario) Scenario {
	out := s
	if out.SchemaVersion == 0 {
		out.SchemaVersion = SchemaVersion
	}
	if out.Cluster.Timing == "" {
		out.Cluster.Timing = simcluster.TimingDev
	}
	if out.Isolation == "" {
		out.Isolation = IsolationReuse
		if len(s.Variants) > 1 {
			out.Isolation = IsolationCluster
		}
	}
	if out.Drain == nil {
		out.Drain = &metav1.Duration{}
	}
	w := &out.Workload
	if w.BaseSpeedPerCore == nil {
		v := DefaultBaseSpeedPerCore
		w.BaseSpeedPerCore = &v
	}
	w.Jobs = append([]JobSpec(nil), s.Workload.Jobs...)
	for i := range w.Jobs {
		if w.Jobs[i].Count == 0 {
			w.Jobs[i].Count = 1
		}
		if w.Jobs[i].Pods == 0 {
			w.Jobs[i].Pods = 1
		}
	}
	out.Joulie = copyJoulie(s.Joulie)
	if out.Joulie.Enabled == nil {
		enabled := true
		out.Joulie.Enabled = &enabled
	}
	if out.Capture.Interval == nil {
		out.Capture.Interval = &metav1.Duration{Duration: DefaultCaptureInterval}
	}
	out.Assertions = append([]Assertion(nil), s.Assertions...)
	for i := range out.Assertions {
		if out.Assertions[i].Severity == "" {
			out.Assertions[i].Severity = SeverityFail
		}
		if tp := out.Assertions[i].TwinPower; tp != nil && tp.Quantile == nil {
			c := *tp
			q := DefaultTwinPowerQuantile
			c.Quantile = &q
			out.Assertions[i].TwinPower = &c
		}
	}
	out.Compare = append([]Compare(nil), s.Compare...)
	for i := range out.Compare {
		if out.Compare[i].Severity == "" {
			out.Compare[i].Severity = SeverityFail
		}
	}
	return out
}

// baseSpeed is the effective workload.baseSpeedPerCore.
func (s Scenario) baseSpeed() float64 {
	if s.Workload.BaseSpeedPerCore != nil {
		return *s.Workload.BaseSpeedPerCore
	}
	return DefaultBaseSpeedPerCore
}

// TraceJob is one type=job record of a v3 trace: the fields the runner
// writes for inline jobs and reads back for planning and evaluation.
// Unknown keys are ignored on read, so a workloadgen record decodes too.
type TraceJob struct {
	Type                string           `json:"type"`
	SchemaVersion       string           `json:"schemaVersion"`
	JobID               string           `json:"jobId"`
	WorkloadID          string           `json:"workloadId,omitempty"`
	WorkloadType        string           `json:"workloadType,omitempty"`
	PodRole             string           `json:"podRole,omitempty"`
	Gang                bool             `json:"gang,omitempty"`
	SubmitTimeOffsetSec float64          `json:"submitTimeOffsetSec"`
	Namespace           string           `json:"namespace"`
	IntentClass         string           `json:"intentClass"`
	PodTemplate         TracePodTemplate `json:"podTemplate"`
	Work                TraceWork        `json:"work"`
	Sensitivity         *Sensitivity     `json:"sensitivity,omitempty"`
	WorkloadClass       WorkClass        `json:"workloadClass"`
	WorkloadProfile     Profile          `json:"workloadProfile"`
}

// TracePodTemplate is a trace job's requests and placement, in corev1 JSON.
type TracePodTemplate struct {
	Requests     map[string]string   `json:"requests"`
	Affinity     *corev1.Affinity    `json:"affinity,omitempty"`
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`
}

// TraceWork is a trace job's work in units: at full speed a job does
// request x base speed per core units per second of each kind.
type TraceWork struct {
	CPUUnits float64 `json:"cpuUnits"`
	GPUUnits float64 `json:"gpuUnits"`
}

// ReadTrace returns the job records of a trace file, in file order. A
// record of another type is skipped; a line that is not JSON is an error
// naming it.
func ReadTrace(path string) ([]TraceJob, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []TraceJob
	err = scanJSONLines(f, func(line int, b []byte) error {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &head); err != nil {
			return fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if head.Type != workload.TraceRecordJob {
			return nil
		}
		var j TraceJob
		if err := json.Unmarshal(b, &j); err != nil {
			return fmt.Errorf("%s line %d: %w", path, line, err)
		}
		out = append(out, j)
		return nil
	})
	return out, err
}

// scanJSONLines calls fn with each non-blank line of r and its 1-based
// number. Lines may be as long as a trace record of a large gang.
func scanJSONLines(r io.Reader, fn func(line int, b []byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		if err := fn(n, b); err != nil {
			return err
		}
	}
	return sc.Err()
}

// FilterTrace copies the records of r that are due before the end of the
// submission window to w, unchanged, and counts the job records kept and
// dropped. A record with a schemaVersion other than v3 is an error naming
// its line: the simulator's v1 engine and the evaluator read v3 only.
func FilterTrace(r io.Reader, w io.Writer, window time.Duration) (kept, dropped int, err error) {
	err = scanJSONLines(r, func(line int, b []byte) error {
		var head struct {
			Type          string  `json:"type"`
			SchemaVersion string  `json:"schemaVersion"`
			Offset        float64 `json:"submitTimeOffsetSec"`
		}
		if err := json.Unmarshal(b, &head); err != nil {
			return fmt.Errorf("trace line %d: %w", line, err)
		}
		if head.SchemaVersion != workload.TraceSchemaV3 {
			return fmt.Errorf("trace line %d: schemaVersion %q, want %s", line, head.SchemaVersion, workload.TraceSchemaV3)
		}
		due := head.Offset < window.Seconds()
		if head.Type == workload.TraceRecordJob {
			if due {
				kept++
			} else {
				dropped++
			}
		}
		if !due {
			return nil
		}
		_, err := w.Write(append(append([]byte(nil), b...), '\n'))
		return err
	})
	return kept, dropped, err
}

// RenderJobs renders the inline jobs of s as v3 trace records, sorted by
// submit offset. Group k of job spec <name> is workload <name>-<k> of type
// <name>, submitted at k x every, and its pod p is job <name>-<k>-<p>. The
// work of each pod is its runtime x request x the base speed per core, so
// its ideal runtime is the runtime the spec gives; a GPU pod gets as many
// GPU units per GPU. An unset profile value is left out, and the simulator
// takes it from the work class.
func RenderJobs(s Scenario) ([]byte, error) {
	base := s.baseSpeed()
	ns := s.Workload.Namespace
	if ns == "" {
		ns = DefaultTraceNamespace
	}
	var recs []TraceJob
	for _, js := range s.Workload.Jobs {
		count, pods := js.Count, js.Pods
		if count == 0 {
			count = 1
		}
		if pods == 0 {
			pods = 1
		}
		cpu, ok := js.Requests[corev1.ResourceCPU]
		if !ok || cpu.Sign() <= 0 {
			return nil, fmt.Errorf("job %s: requests.cpu must be positive", js.Name)
		}
		requests := map[string]string{}
		for name, q := range js.Requests {
			requests[string(name)] = q.String()
		}
		gpus := 0
		if js.GPUs != nil {
			gpus = js.GPUs.Count
			requests[js.GPUs.Resource] = strconv.Itoa(gpus)
		}
		runtime := js.Runtime.Seconds()
		every := 0.0
		if js.Every != nil {
			every = js.Every.Seconds()
		}
		for k := 0; k < count; k++ {
			for p := 0; p < pods; p++ {
				rec := TraceJob{
					Type:                workload.TraceRecordJob,
					SchemaVersion:       workload.TraceSchemaV3,
					JobID:               fmt.Sprintf("%s-%d-%d", js.Name, k, p),
					WorkloadID:          fmt.Sprintf("%s-%d", js.Name, k),
					WorkloadType:        js.Name,
					PodRole:             TracePodRole,
					Gang:                js.Gang,
					SubmitTimeOffsetSec: float64(k) * every,
					Namespace:           ns,
					IntentClass:         js.Class,
					PodTemplate: TracePodTemplate{
						Requests:     requests,
						Affinity:     js.Affinity,
						NodeSelector: js.NodeSelector,
						Tolerations:  js.Tolerations,
					},
					Work:            TraceWork{CPUUnits: runtime * cpu.AsApproximateFloat64() * base, GPUUnits: runtime * float64(gpus) * base},
					Sensitivity:     js.Sensitivity,
					WorkloadClass:   js.WorkClass,
					WorkloadProfile: js.Profile,
				}
				recs = append(recs, rec)
			}
		}
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].SubmitTimeOffsetSec < recs[j].SubmitTimeOffsetSec })
	var buf bytes.Buffer
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// RunnerWorkloadgenFlags are the workloadgen flags the runner sets itself,
// so a scenario may not give them: the output, the seed (Scenario.Seed),
// the base speed (workload.baseSpeedPerCore) and the schema, which is
// always v3.
var RunnerWorkloadgenFlags = []string{"base-speed-per-core", "out", "schema-version", "seed"}

// WorkloadgenFlags is the allowlist of workloadgen flags a scenario may set
// under workload.generate.workloadgen, by name without the dash. The flags
// the binary has that are in neither list are refused: no-affinity-only
// (v3 writes no power-profile affinity anyway) and the default mode's
// work rates (v3 sizes work with the base speed per core in both modes).
var WorkloadgenFlags = []string{
	"amd-gpu-ratio", "arrival-window-sec", "burst-day-probability", "burst-mean-jobs", "burst-multiplier",
	"compute-bound-perf-boost", "cpu-sensitivity", "dip-day-probability", "dip-multiplier",
	"diurnal-burst-max", "diurnal-burst-min", "diurnal-peak-rate", "emit-workload-records",
	"gpu-ratio", "gpu-sensitivity", "jobs", "max-duration-sec", "mean-inter-arrival-sec",
	"min-gpu-request", "mode", "namespace", "peer-transfer-intensity", "perf-ratio", "sim-hours", "time-scale", "work-scale",
}

// RefusedWorkloadgenFlags are the binary's flags a scenario may not set,
// with the reason.
var RefusedWorkloadgenFlags = map[string]string{
	"no-affinity-only":       "v3 traces carry no power-profile affinity",
	"cpu-work-rate-per-core": "v3 sizes work with workload.baseSpeedPerCore",
	"gpu-work-rate-per-gpu":  "v3 sizes work with workload.baseSpeedPerCore",
}

// WorkloadgenArgs returns the workloadgen arguments of s: its flags sorted
// by name, then the runner's (seed, base speed, v3 schema, out).
func WorkloadgenArgs(s Scenario, out string) []string {
	var flags ScalarMap
	if s.Workload.Generate != nil {
		flags = s.Workload.Generate.Workloadgen
	}
	keys := make([]string, 0, len(flags))
	for k := range flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var args []string
	for _, k := range keys {
		args = append(args, "-"+k+"="+string(flags[k]))
	}
	return append(args,
		"-seed="+strconv.FormatInt(s.Seed, 10),
		"-base-speed-per-core="+strconv.FormatFloat(s.baseSpeed(), 'g', -1, 64),
		"-schema-version="+workload.TraceSchemaV3,
		"-out="+out,
	)
}

// GenerateTrace runs the workloadgen binary bin for s into out.
func GenerateTrace(ctx context.Context, bin string, s Scenario, out string) error {
	if bin == "" {
		return fmt.Errorf("scenario %s generates its trace, and no workloadgen binary was given", s.Name)
	}
	cmd := exec.CommandContext(ctx, bin, WorkloadgenArgs(s, out)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("workloadgen %s: %w\n%s", strings.Join(cmd.Args[1:], " "), err, b)
	}
	return nil
}

// WriteTrace writes the trace of s to path: the rendered inline jobs, the
// workloadgen output, or the trace file (relative to repoRoot), keeping only
// the records due before the end of the submission window. It returns the
// job records kept and dropped.
func WriteTrace(ctx context.Context, s Scenario, repoRoot, workloadgenBin, path string) (kept, dropped int, err error) {
	var src []byte
	switch w := s.Workload; {
	case len(w.Jobs) > 0:
		src, err = RenderJobs(s)
	case w.Generate != nil:
		tmp := path + ".workloadgen"
		defer os.Remove(tmp)
		if err = GenerateTrace(ctx, workloadgenBin, s, tmp); err == nil {
			src, err = os.ReadFile(tmp)
		}
	case w.Trace != "":
		src, err = os.ReadFile(filepath.Join(repoRoot, w.Trace))
	default:
		err = fmt.Errorf("scenario %s has no workload source", s.Name)
	}
	if err != nil {
		return 0, 0, err
	}
	var buf bytes.Buffer
	if kept, dropped, err = FilterTrace(bytes.NewReader(src), &buf, s.Duration.Duration); err != nil {
		return 0, 0, err
	}
	return kept, dropped, os.WriteFile(path, buf.Bytes(), 0o644)
}

// VariantEnv is variant.env (VariantEnvFile): the settings plan computes for
// one variant, for scenario.sh to source and for run and evaluate to read.
// Lists are bash arrays; every value is single-quoted.
type VariantEnv struct {
	Scenario string // SCENARIO_NAME
	Variant  string // SCENARIO_VARIANT
	RunID    string // SCENARIO_RUN_ID, the run id given to the simulator
	// ClusterName is the cluster spec's metadata.name, the k8s.cluster.name
	// of the run's records.
	ClusterName string // SCENARIO_CLUSTER_NAME
	// Spec, Timing, Telemetry and PowerScope are the harness's
	// SIMCLUSTER_SPEC, SIMCLUSTER_TIMING, SIMCLUSTER_TELEMETRY and
	// SIMCLUSTER_POWER_SCOPE. A scenario always runs Joulie's default scope.
	Spec, Timing, Telemetry, PowerScope string
	Isolation                           Isolation // SCENARIO_ISOLATION
	JoulieEnabled                       bool      // JOULIE_ENABLED
	InputPreset                         string    // JOULIE_INPUT_PRESET
	// JoulieSet and SimulatorSet are key=value Helm --set arguments, sorted
	// by key; JoulieValues are values files for the Joulie release.
	JoulieSet, JoulieValues, SimulatorSet []string // JOULIE_SET, JOULIE_VALUES, SIM_SET
	// ControllerReconcile, CacheTTL and RateWindow are the controller
	// reconcile interval, the extender's CACHE_TTL and the controller's rate
	// window in effect: the timing's, unless joulie.set overrides them.
	ControllerReconcile time.Duration // SCENARIO_CONTROLLER_RECONCILE
	CacheTTL            time.Duration // SCENARIO_CACHE_TTL
	RateWindow          time.Duration // SCENARIO_RATE_WINDOW
	GitSHA              string        // SCENARIO_GIT_SHA
	GitDirty            bool          // SCENARIO_GIT_DIRTY
}

// PlacementGrace is the default graceSeconds of a placement assertion: the
// extender's cache TTL plus the controller's reconcile interval, the time a
// node's class change takes to reach scoring.
func (e VariantEnv) PlacementGrace() time.Duration {
	return e.CacheTTL + e.ControllerReconcile
}

// TwinPowerWarmup is the default warmup of a twinPower assertion: twice
// the rate window, so the twin's rate covers run time only.
func (e VariantEnv) TwinPowerWarmup() time.Duration {
	return 2 * e.RateWindow
}

// Marshal writes e in the variant.env format.
func (e VariantEnv) Marshal() []byte {
	var b bytes.Buffer
	b.WriteString("# Written by simscenario plan; sourced by scripts/simcluster/scenario.sh.\n")
	str := func(k, v string) { fmt.Fprintf(&b, "%s=%s\n", k, simcluster.ShellQuote(v)) }
	list := func(k string, vs []string) {
		q := make([]string, len(vs))
		for i, v := range vs {
			q[i] = simcluster.ShellQuote(v)
		}
		fmt.Fprintf(&b, "%s=(%s)\n", k, strings.Join(q, " "))
	}
	str("SCENARIO_NAME", e.Scenario)
	str("SCENARIO_VARIANT", e.Variant)
	str("SCENARIO_RUN_ID", e.RunID)
	str("SCENARIO_CLUSTER_NAME", e.ClusterName)
	str("SIMCLUSTER_SPEC", e.Spec)
	str("SIMCLUSTER_TIMING", e.Timing)
	str("SIMCLUSTER_TELEMETRY", e.Telemetry)
	str("SIMCLUSTER_POWER_SCOPE", e.PowerScope)
	str("SCENARIO_ISOLATION", string(e.Isolation))
	str("JOULIE_ENABLED", strconv.FormatBool(e.JoulieEnabled))
	str("JOULIE_INPUT_PRESET", e.InputPreset)
	list("JOULIE_SET", e.JoulieSet)
	list("JOULIE_VALUES", e.JoulieValues)
	list("SIM_SET", e.SimulatorSet)
	str("SCENARIO_CONTROLLER_RECONCILE", e.ControllerReconcile.String())
	str("SCENARIO_CACHE_TTL", e.CacheTTL.String())
	str("SCENARIO_RATE_WINDOW", e.RateWindow.String())
	str("SCENARIO_GIT_SHA", e.GitSHA)
	str("SCENARIO_GIT_DIRTY", strconv.FormatBool(e.GitDirty))
	return b.Bytes()
}

// ParseVariantEnv reads what Marshal writes.
func ParseVariantEnv(data []byte) (VariantEnv, error) {
	kv, err := envWords(data)
	if err != nil {
		return VariantEnv{}, err
	}
	var e VariantEnv
	one := func(k string) string {
		if v := kv[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	var errs []string
	dur := func(k string) time.Duration {
		d, err := time.ParseDuration(one(k))
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", k, err))
		}
		return d
	}
	e.Scenario, e.Variant, e.RunID = one("SCENARIO_NAME"), one("SCENARIO_VARIANT"), one("SCENARIO_RUN_ID")
	e.ClusterName = one("SCENARIO_CLUSTER_NAME")
	e.Spec, e.Timing, e.Telemetry = one("SIMCLUSTER_SPEC"), one("SIMCLUSTER_TIMING"), one("SIMCLUSTER_TELEMETRY")
	e.PowerScope = one("SIMCLUSTER_POWER_SCOPE")
	e.Isolation = Isolation(one("SCENARIO_ISOLATION"))
	e.JoulieEnabled = one("JOULIE_ENABLED") == "true"
	e.InputPreset = one("JOULIE_INPUT_PRESET")
	e.JoulieSet, e.JoulieValues, e.SimulatorSet = kv["JOULIE_SET"], kv["JOULIE_VALUES"], kv["SIM_SET"]
	e.ControllerReconcile = dur("SCENARIO_CONTROLLER_RECONCILE")
	e.CacheTTL = dur("SCENARIO_CACHE_TTL")
	e.RateWindow = dur("SCENARIO_RATE_WINDOW")
	e.GitSHA = one("SCENARIO_GIT_SHA")
	e.GitDirty = one("SCENARIO_GIT_DIRTY") == "true"
	if len(errs) > 0 {
		return e, fmt.Errorf("variant.env: %s", strings.Join(errs, "; "))
	}
	return e, nil
}

// envWords reads KEY=VALUE lines as simcluster.ParseEnvFile does, as
// VariantEnv.Marshal and the harness's images.env write them, and returns
// each key's words. A scalar must be one word.
func envWords(data []byte) (map[string][]string, error) {
	vars, err := simcluster.ParseEnvFile(data)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(vars))
	for _, v := range vars {
		if !v.Array && len(v.Words) > 1 {
			return nil, fmt.Errorf("line %d: %s has %d words, want one", v.Line, v.Key, len(v.Words))
		}
		out[v.Key] = v.Words
	}
	return out, nil
}

// setArgs returns m as sorted key=value arguments.
func setArgs(m ScalarMap) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+string(v))
	}
	sort.Strings(out)
	return out
}

// setDuration returns the duration Set holds under key, or def.
func setDuration(set ScalarMap, key string, def time.Duration) (time.Duration, error) {
	v, ok := set[key]
	if !ok {
		return def, nil
	}
	d, err := time.ParseDuration(string(v))
	if err != nil {
		return 0, fmt.Errorf("joulie.set %s: %w", key, err)
	}
	return d, nil
}

// NewVariantEnv computes the variant.env of variant name of the effective
// scenario s, on the cluster clusterName with timing t.
func NewVariantEnv(s Scenario, name, runID, clusterName string, t simcluster.Timing) (VariantEnv, error) {
	v, err := s.ForVariant(name)
	if err != nil {
		return VariantEnv{}, err
	}
	set := v.Joulie.Set
	if v.Joulie.InputPreset != "" {
		set = mergeScalars(set, ScalarMap{HelmNodePowerPreset: Scalar(v.Joulie.InputPreset)})
	}
	e := VariantEnv{
		Scenario: s.Name, Variant: name, RunID: runID, ClusterName: clusterName,
		Spec: s.Cluster.Spec, Timing: t.Name, Telemetry: string(simcluster.ModeForPreset(v.Joulie.InputPreset)),
		PowerScope:    string(simcluster.ScopeComponents),
		Isolation:     s.Isolation,
		JoulieEnabled: v.Joulie.Enabled == nil || *v.Joulie.Enabled,
		InputPreset:   v.Joulie.InputPreset,
		JoulieSet:     setArgs(set),
		JoulieValues:  append([]string{}, v.Joulie.Values...),
		SimulatorSet:  setArgs(v.Simulator.Set),
	}
	if e.ControllerReconcile, err = setDuration(set, helmControllerEnv+"RECONCILE_INTERVAL", t.ControllerReconcile); err != nil {
		return e, err
	}
	if e.CacheTTL, err = setDuration(set, helmExtenderEnv+"CACHE_TTL", DefaultCacheTTL); err != nil {
		return e, err
	}
	if e.RateWindow, err = setDuration(set, helmControllerEnv+"PROMETHEUS_RATE_WINDOW", t.RateWindow); err != nil {
		return e, err
	}
	return e, nil
}

// VariantDir is the directory of variant name inside the run directory:
// the run directory itself for the unnamed variant.
func VariantDir(runDir, name string) string {
	if name == "" {
		return runDir
	}
	return filepath.Join(runDir, name)
}

// RunID is the simulator run id of variant name of the run directory
// runDir: the run directory's name, with the variant appended.
func RunID(runDir, name string) string {
	id := filepath.Base(filepath.Clean(runDir))
	if name != "" {
		id += "-" + name
	}
	return id
}

// PlanOptions are the inputs of MakePlan beside the scenario.
type PlanOptions struct {
	// RepoRoot is the repository the scenario's paths are relative to.
	RepoRoot string
	// RunDir is the run directory to write.
	RunDir string
	// Workloadgen is the workloadgen binary, needed by a generated trace.
	Workloadgen string
	// GitSHA and GitDirty describe the commit the run is built from.
	GitSHA   string
	GitDirty bool
}

// Plan is what MakePlan wrote: the effective scenario, each variant's
// settings, and the load of the trace.
type Plan struct {
	Effective Scenario
	Variants  []VariantEnv
	// Jobs and Dropped count the trace's job records due inside the
	// submission window, and those dropped for being due later.
	Jobs, Dropped int
	// Load is the offered load of each resource on every Node the jobs
	// can use, EarliestEnd when their work can end there at the earliest
	// (the function EarliestEnd), and Deadline the duration plus the drain.
	Load                  []OfferedLoad
	EarliestEnd, Deadline time.Duration
	// Classes are the loads of the classes a variant's policy confines
	// (ConfinedLoads).
	Classes []ClassLoad
}

// MakePlan writes the run directory of the valid scenario s:
// ScenarioEffectiveFile, and for each variant its directory with TraceFile
// and VariantEnvFile. It returns an *OverloadError, after writing
// everything, when the offered load of a resource exceeds MaxOfferedLoad or
// the earliest end of the work takes more than MaxOfferedLoad of the
// deadline, on every usable Node or, for a class a variant's policy
// confines, on the Nodes left to it.
func MakePlan(ctx context.Context, s Scenario, o PlanOptions) (*Plan, error) {
	eff := Effective(s)
	timing, err := simcluster.ReadTiming(o.RepoRoot, eff.Cluster.Timing)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.RunDir, 0o755); err != nil {
		return nil, err
	}
	b, err := yaml.Marshal(eff)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(o.RunDir, ScenarioEffectiveFile), b, 0o644); err != nil {
		return nil, err
	}
	clusterName, pools, err := ReadSpecPools(o.RepoRoot, eff.Cluster.Spec)
	if err != nil {
		return nil, err
	}
	p := &Plan{Effective: eff}
	var tracePath string
	for i, name := range eff.RunVariants() {
		dir := VariantDir(o.RunDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, workload.TraceFile)
		if i == 0 {
			if p.Jobs, p.Dropped, err = WriteTrace(ctx, eff, o.RepoRoot, o.Workloadgen, path); err != nil {
				return nil, err
			}
			tracePath = path
		} else if err := copyFile(tracePath, path); err != nil {
			return nil, err
		}
		env, err := NewVariantEnv(eff, name, RunID(o.RunDir, name), clusterName, timing)
		if err != nil {
			return nil, err
		}
		env.GitSHA, env.GitDirty = o.GitSHA, o.GitDirty
		if err := os.WriteFile(filepath.Join(dir, VariantEnvFile), env.Marshal(), 0o644); err != nil {
			return nil, err
		}
		p.Variants = append(p.Variants, env)
	}
	jobs, err := ReadTrace(tracePath)
	if err != nil {
		return nil, err
	}
	var selector map[string]string
	if pl := eff.Workload.Placement; pl != nil {
		selector = pl.NodeSelector
	}
	capacity := Capacity(pools, jobs, selector, runTolerations(eff))
	if err := checkTraceResources(jobs, capacity); err != nil {
		return p, err
	}
	p.Load = ComputeOfferedLoad(jobs, capacity, eff.Duration.Duration, eff.baseSpeed())
	p.EarliestEnd, p.Deadline = EarliestEnd(jobs, capacity, eff.baseSpeed()), eff.deadline()
	if p.Classes, err = ConfinedLoads(eff, pools, jobs, selector, runTolerations(eff)); err != nil {
		return p, err
	}
	over := &OverloadError{}
	for _, l := range p.Load {
		if l.Load > MaxOfferedLoad || math.IsInf(l.Load, 1) {
			over.Over = append(over.Over, l)
		}
	}
	if pastDeadline(p.EarliestEnd, p.Deadline) {
		over.EarliestEnd, over.Deadline = p.EarliestEnd, p.Deadline
	}
	for _, c := range p.Classes {
		if c.Over() {
			over.Classes = append(over.Classes, c)
		}
	}
	if len(over.Over) > 0 || over.Deadline > 0 || len(over.Classes) > 0 {
		return p, over
	}
	return p, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
