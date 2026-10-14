package scenario

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/pkg/controller/inputs"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/simcluster"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// Limits of a scenario.
const (
	MaxDuration = 6 * time.Hour
	MaxVariants = 4
)

// dnsLabelRE is a DNS-1123 label, the rule for scenario, variant, job,
// assertion and comparison names; each is also at most 63 characters.
var dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// gpuResourceRE is the extended resource of a GPU request: whole GPUs of
// either vendor, a MIG profile, or a shared GPU.
var gpuResourceRE = regexp.MustCompile(`^(nvidia\.com/gpu|amd\.com/gpu|nvidia\.com/gpu\.shared|nvidia\.com/mig-[0-9]+g\.[0-9]+gb(\+me)?)$`)

// powerInputPrefixes start the Joulie settings a scenario may not set: the
// node power and facility inputs are reached only through
// joulie.inputPreset, so a scenario always tests one of Joulie's presets.
var powerInputPrefixes = []string{"NODE_POWER_", "FACILITY_"}

// InputPresets are the joulie.inputPreset values a scenario may name; the
// custom preset needs a query, which a scenario may not set.
var InputPresets = []string{"", string(inputs.NodePowerPresetOTel), string(inputs.NodePowerPresetKepler), string(inputs.NodePowerPresetKeplerLegacy)}

// jobClasses are the workload classes a job, filter or placement names.
var jobClasses = []string{workload.ClassPerformance, workload.ClassStandard}

// ReadScenario decodes the scenario file path strictly: an unknown key
// anywhere, inside an assertion included, is an error.
func ReadScenario(path string) (Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, err
	}
	var s Scenario
	if err := yaml.UnmarshalStrict(b, &s); err != nil {
		return Scenario{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// ValidateOptions locate the scenario's files.
type ValidateOptions struct {
	// File is the scenario's path; the name must equal its stem. Empty
	// skips that check.
	File string
	// RepoRoot is the directory the cluster spec, trace and values paths
	// are relative to. Empty skips every check that reads one of them.
	RepoRoot string
}

// Validate returns every violation of the scenario rules in s, joined, or
// nil.
func Validate(s Scenario, o ValidateOptions) error {
	v := &validator{}
	v.top(s, o)
	v.workload(s, o)
	v.joulie("joulie", s.Joulie, o)
	v.scalars("simulator.set", s.Simulator.Set)
	v.variants(s, o)
	v.capture(s.Capture)
	names := v.assertions(s.Assertions)
	v.compare(s, names)
	return errors.Join(v.errs...)
}

type validator struct {
	errs []error
	// specRead is true once the cluster spec file was found, so the checks
	// that read it run.
	specRead bool
}

func (v *validator) add(format string, args ...any) {
	v.errs = append(v.errs, fmt.Errorf(format, args...))
}

func (v *validator) name(at, name string) {
	switch {
	case len(name) > 63 || !dnsLabelRE.MatchString(name):
		v.add("%s: name %q is not a DNS-1123 label of at most 63 characters", at, name)
	case name == "true" || name == "false":
		// YAML 1.1 reads an unquoted on, off, yes or no as a boolean, which
		// reaches a string field as true or false.
		v.add("%s: name %q is a YAML boolean; quote the name if the file says on, off, yes or no", at, name)
	}
}

func oneOf(s string, valid []string) bool {
	for _, x := range valid {
		if s == x {
			return true
		}
	}
	return false
}

func (v *validator) top(s Scenario, o ValidateOptions) {
	if s.SchemaVersion != SchemaVersion {
		v.add("schemaVersion %d, want %d", s.SchemaVersion, SchemaVersion)
	}
	v.name("scenario", s.Name)
	if o.File != "" {
		if stem := strings.TrimSuffix(filepath.Base(o.File), filepath.Ext(o.File)); stem != s.Name {
			v.add("scenario name %q differs from the file stem %q", s.Name, stem)
		}
	}
	if s.Cluster.Spec == "" {
		v.add("cluster.spec is not set")
	} else if o.RepoRoot != "" {
		if _, err := os.Stat(filepath.Join(o.RepoRoot, s.Cluster.Spec)); err != nil {
			v.add("cluster.spec: %v", err)
		} else {
			v.specRead = true
		}
	}
	if !oneOf(s.Cluster.Timing, []string{"", simcluster.TimingDev, simcluster.TimingProduction}) {
		v.add("cluster.timing %q is not %s or %s", s.Cluster.Timing, simcluster.TimingDev, simcluster.TimingProduction)
	}
	if d := s.Duration.Duration; d <= 0 || d > MaxDuration {
		v.add("duration %s is not in (0, %s]", d, MaxDuration)
	}
	if s.Drain != nil && s.Drain.Duration < 0 {
		v.add("drain %s is negative", s.Drain.Duration)
	}
	if s.AbortOnRunStalled != nil && s.AbortOnRunStalled.Duration <= 0 {
		v.add("abortOnRunStalled %s is not positive", s.AbortOnRunStalled.Duration)
	}
	switch s.Isolation {
	case "", IsolationCluster:
	case IsolationReuse:
		if len(s.Variants) > 1 {
			v.add("isolation reuse with %d variants: each variant must start from a fresh cluster, or it inherits the limits the previous one wrote", len(s.Variants))
		}
	default:
		v.add("isolation %q is not %s or %s", s.Isolation, IsolationCluster, IsolationReuse)
	}
}

func (v *validator) workload(s Scenario, o ValidateOptions) {
	w := s.Workload
	sources := 0
	for _, set := range []bool{len(w.Jobs) > 0, w.Generate != nil, w.Trace != ""} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		v.add("workload: %d sources among jobs, generate and trace, want exactly one", sources)
	}
	if w.Owner != "" {
		if _, err := workload.ParseOwner(string(w.Owner)); err != nil {
			v.add("workload.owner: %v", err)
		}
	}
	if w.Intent != "" {
		if _, err := workload.ParseIntent(string(w.Intent)); err != nil {
			v.add("workload.intent: %v", err)
		}
	}
	if w.Completion != "" {
		if _, err := workload.ParseCompletion(string(w.Completion)); err != nil {
			v.add("workload.completion: %v", err)
		}
	}
	for _, d := range []struct {
		key string
		d   *metav1.Duration
	}{{"jobTTL", w.JobTTL}, {"stallReport", w.StallReport}} {
		if d.d != nil && d.d.Duration <= 0 {
			v.add("workload.%s %s is not positive", d.key, d.d.Duration)
		}
	}
	if w.BaseSpeedPerCore != nil && *w.BaseSpeedPerCore <= 0 {
		v.add("workload.baseSpeedPerCore %g is not positive", *w.BaseSpeedPerCore)
	}
	jobNames := map[string]bool{}
	for _, j := range w.Jobs {
		v.job(s, j, jobNames)
	}
	if g := w.Generate; g != nil {
		v.workloadgen(g.Workloadgen)
	}
	var trace []TraceJob
	if w.Trace != "" && o.RepoRoot != "" {
		var err error
		if trace, err = ReadTrace(filepath.Join(o.RepoRoot, w.Trace)); err != nil {
			v.add("workload.trace: %v", err)
		}
		for _, j := range trace {
			if j.SchemaVersion != workload.TraceSchemaV3 {
				v.add("workload.trace: job %s has schemaVersion %q, want %s", j.JobID, j.SchemaVersion, workload.TraceSchemaV3)
				break
			}
		}
	}
	if (len(w.Jobs) > 0 || len(trace) > 0) && v.specRead {
		v.jobResources(s, o, trace)
	}
}

func (v *validator) job(s Scenario, j JobSpec, seen map[string]bool) {
	at := "workload.jobs " + j.Name
	v.name(at, j.Name)
	if seen[j.Name] {
		v.add("%s: a second job has this name", at)
	}
	seen[j.Name] = true
	if !oneOf(j.Class, jobClasses) {
		v.add("%s: class %q is not %s", at, j.Class, strings.Join(jobClasses, " or "))
	}
	if j.Count < 0 || j.Pods < 0 {
		v.add("%s: count %d and pods %d may not be negative", at, j.Count, j.Pods)
	}
	for name := range j.Requests {
		if name != corev1.ResourceCPU && name != corev1.ResourceMemory {
			v.add("%s: requests.%s: requests hold cpu and memory, GPUs go in gpus", at, name)
		}
	}
	if q, ok := j.Requests[corev1.ResourceCPU]; !ok || q.Sign() <= 0 {
		v.add("%s: requests.cpu must be positive", at)
	}
	if g := j.GPUs; g != nil {
		if !gpuResourceRE.MatchString(g.Resource) {
			v.add("%s: gpus.resource %q is not nvidia.com/gpu, amd.com/gpu, nvidia.com/gpu.shared or nvidia.com/mig-<profile>", at, g.Resource)
		}
		if g.Count <= 0 {
			v.add("%s: gpus.count %d is not positive", at, g.Count)
		}
	}
	if j.Runtime.Duration <= 0 {
		v.add("%s: runtime %s is not positive", at, j.Runtime.Duration)
	}
	every := time.Duration(0)
	if j.Every != nil {
		every = j.Every.Duration
		if every < 0 {
			v.add("%s: every %s is negative", at, every)
		}
	}
	count := j.Count
	if count == 0 {
		count = 1
	}
	if last := time.Duration(count-1) * every; s.Duration.Duration > 0 && last >= s.Duration.Duration {
		v.add("%s: group %d is due at %s, not before the duration %s, so it would never be submitted", at, count-1, last, s.Duration.Duration)
	}
	p := j.Profile
	v.unit(at+": profile.cpuUtilization", p.CPUUtilization)
	v.unit(at+": profile.gpuUtilization", p.GPUUtilization)
	v.unit(at+": profile.memoryIntensity", p.MemoryIntensity)
	v.unit(at+": profile.ioIntensity", p.IOIntensity)
	v.unit(at+": profile.cpuFeedIntensityGpu", p.CPUFeedIntensityGPU)
	v.unit(at+": profile.peerTransferIntensity", p.PeerTransferIntensity)
	if sens := j.Sensitivity; sens != nil {
		v.unit(at+": sensitivity.cpu", sens.CPU)
		v.unit(at+": sensitivity.gpu", sens.GPU)
	}
}

// unit checks that a set value is in 0..1.
func (v *validator) unit(at string, val *float64) {
	if val != nil && (*val < 0 || *val > 1) {
		v.add("%s %g is not in 0..1", at, *val)
	}
}

// jobResources checks that every resource an inline job or a trace job
// requests exists in some pool of the spec. plan checks a generated trace,
// and checks every trace against the pools the run's pods can use.
func (v *validator) jobResources(s Scenario, o ValidateOptions, trace []TraceJob) {
	_, pools, err := ReadSpecPools(o.RepoRoot, s.Cluster.Spec)
	if err != nil {
		v.add("cluster.spec: %v", err)
		return
	}
	have := map[string]bool{}
	for _, p := range pools {
		for _, n := range p.Nodes {
			for name, q := range n.Status.Allocatable {
				if q.Sign() > 0 {
					have[string(name)] = true
				}
			}
		}
	}
	for _, j := range s.Workload.Jobs {
		if j.GPUs != nil && !have[j.GPUs.Resource] {
			v.add("workload.jobs %s: no pool of %s has %s", j.Name, s.Cluster.Spec, j.GPUs.Resource)
		}
	}
	for _, m := range missingResources(trace, func(name string) bool { return have[name] }) {
		v.add("workload.trace: no pool of %s has %s", s.Cluster.Spec, m)
	}
}

func (v *validator) workloadgen(flags ScalarMap) {
	for _, k := range sortedKeys(flags) {
		switch {
		case k == "schema-version":
			v.add("workload.generate.workloadgen.%s: the schema is always %s", k, workload.TraceSchemaV3)
		case oneOf(k, RunnerWorkloadgenFlags):
			v.add("workload.generate.workloadgen.%s: the runner sets it (seed from seed, base-speed-per-core from workload.baseSpeedPerCore)", k)
		case RefusedWorkloadgenFlags[k] != "":
			v.add("workload.generate.workloadgen.%s: %s", k, RefusedWorkloadgenFlags[k])
		case !oneOf(k, WorkloadgenFlags):
			v.add("workload.generate.workloadgen.%s is not an allowed workloadgen flag; allowed: %s", k, strings.Join(WorkloadgenFlags, ", "))
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isPowerInputKey reports whether a Helm key or values key names a node
// power or facility input setting.
func isPowerInputKey(key string) bool {
	for _, seg := range strings.Split(key, ".") {
		for _, p := range powerInputPrefixes {
			if strings.HasPrefix(seg, p) {
				return true
			}
		}
	}
	return false
}

func (v *validator) joulie(at string, j Joulie, o ValidateOptions) {
	if !oneOf(j.InputPreset, InputPresets) {
		v.add("%s.inputPreset %q is not one of %s", at, j.InputPreset, strings.Join(InputPresets[1:], ", "))
	}
	for _, k := range sortedKeys(j.Set) {
		if isPowerInputKey(k) {
			v.add("%s.set.%s: scenarios may not set NODE_POWER_* or FACILITY_*; select an input preset with inputPreset", at, k)
		}
	}
	v.scalars(at+".set", j.Set)
	for _, f := range j.Values {
		if o.RepoRoot == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(o.RepoRoot, f))
		if err != nil {
			v.add("%s.values: %v", at, err)
			continue
		}
		var doc any
		if err := yaml.Unmarshal(b, &doc); err != nil {
			v.add("%s.values %s: %v", at, f, err)
			continue
		}
		for _, k := range powerInputKeys("", doc) {
			v.add("%s.values %s: %s: scenarios may not set NODE_POWER_* or FACILITY_*; select an input preset with inputPreset", at, f, k)
		}
	}
}

// powerInputKeys returns the paths of the power input keys in a values
// document.
func powerInputKeys(prefix string, doc any) []string {
	m, ok := doc.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for _, k := range sortedKeys(m) {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if isPowerInputKey(k) {
			out = append(out, path)
		}
		out = append(out, powerInputKeys(path, m[k])...)
	}
	return out
}

// scalars checks that Helm values fit one --set argument each.
func (v *validator) scalars(at string, m ScalarMap) {
	for _, k := range sortedKeys(m) {
		if strings.ContainsAny(string(m[k]), "\n\r") || strings.ContainsAny(k, "=\n\r") {
			v.add("%s.%s: a key or value with a line break or a key with = cannot be passed to helm --set", at, k)
		}
	}
}

func (v *validator) variants(s Scenario, o ValidateOptions) {
	if len(s.Variants) > MaxVariants {
		v.add("%d variants, at most %d", len(s.Variants), MaxVariants)
	}
	seen := map[string]bool{}
	for _, vr := range s.Variants {
		at := "variants " + vr.Name
		v.name(at, vr.Name)
		if seen[vr.Name] {
			v.add("%s: a second variant has this name", at)
		}
		seen[vr.Name] = true
		if vr.Joulie != nil {
			v.joulie(at+": joulie", *vr.Joulie, o)
		}
		if vr.Simulator != nil {
			v.scalars(at+": simulator.set", vr.Simulator.Set)
		}
	}
}

// nodeTwinCaptures are the metric names the runner records itself.
var nodeTwinCaptures = []string{
	semconv.JoulieCaptureNodeTwinMeasuredPower.Name, semconv.JoulieCaptureNodeTwinSchedulableClass.Name,
	semconv.JoulieCaptureNodeTwinControlResult.Name,
}

func (v *validator) capture(c Capture) {
	if c.Interval != nil && c.Interval.Duration < time.Second {
		v.add("capture.interval %s is shorter than 1s", c.Interval.Duration)
	}
	seen := map[string]bool{}
	for _, p := range c.Prometheus {
		at := "capture.prometheus " + p.Name
		if !strings.HasPrefix(p.Name, semconv.CaptureMetricPrefix) || len(p.Name) == len(semconv.CaptureMetricPrefix) {
			v.add("%s: the name must start with %s", at, semconv.CaptureMetricPrefix)
		}
		if oneOf(p.Name, nodeTwinCaptures) {
			v.add("%s: the runner records this name itself", at)
		}
		if seen[p.Name] {
			v.add("%s: a second capture has this name", at)
		}
		seen[p.Name] = true
		if strings.TrimSpace(p.Query) == "" {
			v.add("%s: query is empty", at)
		}
	}
}

// assertionDims are the comparator units each assertion type accepts.
func assertionDims(a Assertion) []Dimension {
	switch a.Type {
	case AssertLatency:
		if a.Latency.Metric == LatencySlowdown || a.Latency.Reference != nil {
			return []Dimension{DimensionNone}
		}
		return []Dimension{DimensionNone, DimensionTime}
	case AssertEnergy:
		return []Dimension{DimensionNone, DimensionEnergy}
	case AssertPromQL:
		return []Dimension{DimensionNone, DimensionTime, DimensionEnergy, DimensionPower}
	}
	return []Dimension{DimensionNone}
}

func (v *validator) assertions(as []Assertion) map[string]bool {
	seen := map[string]bool{}
	for _, a := range as {
		at := "assertions " + a.Name
		v.name(at, a.Name)
		if seen[a.Name] {
			v.add("%s: a second assertion has this name", at)
		}
		seen[a.Name] = true
		if !oneOf(string(a.Severity), []string{"", string(SeverityFail), string(SeverityWarn)}) {
			v.add("%s: severity %q is not %s or %s", at, a.Severity, SeverityFail, SeverityWarn)
		}
		if a.Spec() == nil {
			v.add("%s: no type", at)
			continue
		}
		checks := a.Checks()
		if len(checks) == 0 {
			v.add("%s: expect sets no comparator", at)
		}
		for _, c := range checks {
			e, err := c.Expect.Parse()
			if err != nil {
				v.add("%s: %v", at, err)
				continue
			}
			if dims := assertionDims(a); !dimIn(e.Dim, dims) {
				v.add("%s: comparator %q has unit %s, which a %s assertion does not measure", at, c.Expect, e.Unit, a.Type)
			}
		}
		v.assertionFields(at, a)
	}
	return seen
}

func dimIn(d Dimension, ds []Dimension) bool {
	for _, x := range ds {
		if d == x {
			return true
		}
	}
	return false
}

func (v *validator) filter(at string, f JobFilter) {
	if f.Class != "" && !oneOf(f.Class, jobClasses) {
		v.add("%s: class %q is not %s", at, f.Class, strings.Join(jobClasses, " or "))
	}
}

// ParseQuantile reads a latency quantile: a number in 0.5..1, or max (1).
func ParseQuantile(q Scalar) (float64, error) {
	if q == "max" {
		return 1, nil
	}
	f, err := strconv.ParseFloat(string(q), 64)
	if err != nil || f < 0.5 || f > 1 {
		return 0, fmt.Errorf("quantile %q is not a number in 0.5..1 or max", string(q))
	}
	return f, nil
}

// latencyMetrics are the LatencyMetric values.
var latencyMetrics = []string{string(LatencyDuration), string(LatencyQueueWait), string(LatencyRunTime), string(LatencySlowdown)}

func (v *validator) assertionFields(at string, a Assertion) {
	switch {
	case a.Jobs != nil:
		v.filter(at, a.Jobs.JobFilter)
	case a.Latency != nil:
		l := a.Latency
		v.filter(at, l.JobFilter)
		if l.Reference != nil {
			v.filter(at+": reference", *l.Reference)
		}
		if !oneOf(string(l.Metric), latencyMetrics) {
			v.add("%s: metric %q is not one of %s", at, l.Metric, strings.Join(latencyMetrics, ", "))
		}
		if _, err := ParseQuantile(l.Quantile); err != nil {
			v.add("%s: %v", at, err)
		}
	case a.Placement != nil:
		p := a.Placement
		if !oneOf(p.Class, jobClasses) {
			v.add("%s: class %q is not %s", at, p.Class, strings.Join(jobClasses, " or "))
		}
		if len(p.Forbidden) == 0 {
			v.add("%s: forbidden lists no schedulable class", at)
		}
		if p.GraceSeconds != nil && *p.GraceSeconds < 0 {
			v.add("%s: graceSeconds %g is negative", at, *p.GraceSeconds)
		}
	case a.Energy != nil:
		e := a.Energy
		if _, _, err := ParseEnergyScope(e.Scope); err != nil {
			v.add("%s: %v", at, err)
		}
		if !oneOf(e.HWType, []string{"", "all", semconv.HwTypeCPU, semconv.HwTypeGPU}) {
			v.add("%s: hwType %q is not cpu, gpu or all", at, e.HWType)
		}
	case a.Events != nil:
		ev, ok := eventByName(a.Events.Event)
		if !ok {
			v.add("%s: event %q is not a registered event", at, a.Events.Event)
			break
		}
		if evaluationEvents[ev.Name] {
			v.add("%s: event %s is written by evaluate after the assertions, so no assertion sees it", at, ev.Name)
		}
		for _, k := range sortedKeys(a.Events.Where) {
			if !ev.HasAttribute(k) {
				v.add("%s: where.%s: %s records never carry it", at, k, ev.Name)
			}
		}
	case a.TwinPower != nil:
		tp := a.TwinPower
		if tp.Tolerance != nil && *tp.Tolerance <= 0 {
			v.add("%s: tolerance %g is not positive", at, *tp.Tolerance)
		}
		if tp.Quantile != nil && (*tp.Quantile <= 0 || *tp.Quantile > 1) {
			v.add("%s: quantile %g is not in (0, 1]", at, *tp.Quantile)
		}
		if tp.Warmup != nil && tp.Warmup.Duration < 0 {
			v.add("%s: warmup %s is negative", at, tp.Warmup.Duration)
		}
	case a.PromQL != nil:
		if strings.TrimSpace(a.PromQL.Query) == "" {
			v.add("%s: query is empty", at)
		}
		if a.PromQL.At != "" && a.PromQL.At != PromQLAtEnd {
			v.add("%s: at %q: the only value is %s", at, a.PromQL.At, PromQLAtEnd)
		}
	}
}

// PromQLAtEnd is the only time a promql assertion is evaluated at.
const PromQLAtEnd = "end"

func eventByName(name string) (semconv.Event, bool) {
	for _, e := range semconv.Events() {
		if e.Name == name {
			return e, true
		}
	}
	return semconv.Event{}, false
}

// ParseEnergyScope reads an energy scope: total, class:<class> or
// node:<node>. It returns the kind and the name.
func ParseEnergyScope(scope string) (kind, name string, err error) {
	if scope == EnergyScopeTotal {
		return EnergyScopeTotal, "", nil
	}
	kind, name, ok := strings.Cut(scope, ":")
	switch {
	case ok && kind == EnergyScopeClass && oneOf(name, append(append([]string(nil), jobClasses...), workload.ClassIdle)):
		return kind, name, nil
	case ok && kind == EnergyScopeNode && name != "":
		return kind, name, nil
	}
	return "", "", fmt.Errorf("energy scope %q is not total, class:<performance|standard|idle> or node:<node>", scope)
}

// Energy scope kinds.
const (
	EnergyScopeTotal = "total"
	EnergyScopeClass = "class"
	EnergyScopeNode  = "node"
)

func (v *validator) compare(s Scenario, assertionNames map[string]bool) {
	variants := map[string]bool{}
	for _, vr := range s.Variants {
		variants[vr.Name] = true
	}
	seen := map[string]bool{}
	for _, c := range s.Compare {
		at := "compare " + c.Name
		v.name(at, c.Name)
		if seen[c.Name] || assertionNames[c.Name] {
			v.add("%s: an assertion or a second comparison has this name", at)
		}
		seen[c.Name] = true
		if len(s.Variants) == 0 {
			v.add("%s: the scenario lists no variants to compare", at)
		}
		for _, name := range []string{c.Variant, c.Reference} {
			if !variants[name] {
				v.add("%s: %q is not a variant of the scenario", at, name)
			}
		}
		if c.Variant == c.Reference {
			v.add("%s: variant and reference are both %q", at, c.Variant)
		}
		if err := ValidCompareMetric(c.Metric); err != nil {
			v.add("%s: %v", at, err)
		}
		if e, err := c.Expect.Parse(); err != nil {
			v.add("%s: %v", at, err)
		} else if e.Dim != DimensionNone {
			v.add("%s: comparator %q has a unit, but a comparison is a ratio", at, c.Expect)
		}
		if !oneOf(string(c.Severity), []string{"", string(SeverityFail), string(SeverityWarn)}) {
			v.add("%s: severity %q is not %s or %s", at, c.Severity, SeverityFail, SeverityWarn)
		}
	}
}
