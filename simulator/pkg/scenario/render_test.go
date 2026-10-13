package scenario_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/scenario"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// An inline job's work is sized so its ideal runtime is the runtime the
// scenario gives, for CPU and GPU work, at the scenario's base speed.
func TestRenderJobsIdealRuntimeEqualsRuntime(t *testing.T) {
	s := fxDecode(t, `
name: r
duration: 10m
workload:
  baseSpeedPerCore: 2
  jobs:
  - {name: cpu, class: performance, count: 2, every: 30s, pods: 2, gang: true, requests: {cpu: 1500m, memory: 1Gi}, runtime: 90s, profile: {cpuFeedIntensityGpu: 0}}
  - {name: gpu, class: standard, requests: {cpu: "2"}, gpus: {resource: nvidia.com/gpu, count: 4}, runtime: 5m}
`)
	b, err := scenario.RenderJobs(s)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	jobs, err := scenario.ReadTrace(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, j := range jobs {
		ids = append(ids, j.JobID)
		// Work is runtime x request x base (trace v3), written out here so
		// a formula error RenderJobs and IdealRuntime share is still caught.
		want, cpuUnits := 90*time.Second, 90*1.5*2.0
		if j.WorkloadType == "gpu" {
			want, cpuUnits = 5*time.Minute, 300*2*2.0
			if j.Work.GPUUnits != 300*4*2 {
				t.Errorf("%s: gpuUnits %g, want 2400", j.JobID, j.Work.GPUUnits)
			}
		}
		if j.Work.CPUUnits != cpuUnits {
			t.Errorf("%s: cpuUnits %g, want %g", j.JobID, j.Work.CPUUnits, cpuUnits)
		}
		if got := scenario.IdealRuntime(j, 2); got != want {
			t.Errorf("%s: ideal runtime %s, want %s", j.JobID, got, want)
		}
		if j.SchemaVersion != workload.TraceSchemaV3 || j.Type != workload.TraceRecordJob {
			t.Errorf("%s: record %s %s", j.JobID, j.Type, j.SchemaVersion)
		}
	}
	if want := []string{"cpu-0-0", "cpu-0-1", "gpu-0-0", "cpu-1-0", "cpu-1-1"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("job order %v, want %v (by submit offset, file order within one)", ids, want)
	}
	if j := jobs[0]; !j.Gang || j.WorkloadID != "cpu-0" || j.PodRole != scenario.TracePodRole || j.SubmitTimeOffsetSec != 0 ||
		j.WorkloadProfile.CPUFeedIntensityGPU == nil || *j.WorkloadProfile.CPUFeedIntensityGPU != 0 || j.Sensitivity != nil {
		t.Errorf("first record %+v", j)
	}
	if !bytes.Contains(b, []byte(`"cpuFeedIntensityGpu":0`)) || bytes.Contains(b, []byte(`"sensitivity"`)) {
		t.Errorf("an explicit 0 must be written and an unset sensitivity left out:\n%s", b)
	}
}

// A trace keeps only the records due inside the submission window, and a
// record of another schema is an error naming its line.
func TestFilterTraceDropsLateJobsAndRefusesOtherSchemas(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot, "traces/v3.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	kept, dropped, err := scenario.FilterTrace(bytes.NewReader(src), &out, 5*time.Minute)
	if err != nil || kept != 2 || dropped != 1 {
		t.Fatalf("kept %d dropped %d err %v, want 2, 1", kept, dropped, err)
	}
	if strings.Contains(out.String(), `"j2"`) || strings.Count(out.String(), "\n") != 3 {
		t.Errorf("filtered trace:\n%s", out.String())
	}
	v2, _ := os.ReadFile(filepath.Join(repoRoot, "traces/v2.jsonl"))
	if _, _, err := scenario.FilterTrace(bytes.NewReader(v2), &out, time.Hour); err == nil || !strings.Contains(err.Error(), "trace line 1") {
		t.Errorf("a v2 trace: %v", err)
	}
}

// variant.env reads back as it was written, quotes and lists included.
func TestVariantEnvRoundTrip(t *testing.T) {
	e := fxEnv()
	e.Variant, e.InputPreset, e.Telemetry = "kepler-compat", "kepler", "kepler"
	e.JoulieSet = []string{"controllerManager.env.NODE_POWER_PRESET=kepler", "a.b=it's {x,y}"}
	e.JoulieValues = []string{"values/a b.yaml"}
	e.SimulatorSet = []string{"emulate.exporters={kepler-0.12}"}
	e.GitDirty = true
	got, err := scenario.ParseVariantEnv(e.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, e) {
		t.Errorf("round trip\n%+v\nwant\n%+v\nfrom\n%s", got, e, e.Marshal())
	}
	if e.PlacementGrace() != 40*time.Second || e.TwinPowerWarmup() != 2*time.Minute {
		t.Errorf("grace %s warmup %s", e.PlacementGrace(), e.TwinPowerWarmup())
	}
	// bash reads it the same way.
	if _, err := exec.LookPath("bash"); err == nil {
		path := filepath.Join(t.TempDir(), "variant.env")
		if err := os.WriteFile(path, e.Marshal(), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("bash", "-c", `source "$1" && printf '%s|' "${JOULIE_SET[@]}" "$JOULIE_INPUT_PRESET" "${JOULIE_VALUES[@]}"`, "x", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		if want := "controllerManager.env.NODE_POWER_PRESET=kepler|a.b=it's {x,y}|kepler|values/a b.yaml|"; string(out) != want {
			t.Errorf("bash read %q, want %q", out, want)
		}
	}
}

// A variant merges into the top level key by key: Set by key with the
// variant winning, Values appended, Enabled and InputPreset replaced only
// when the variant sets them.
func TestForVariantMerges(t *testing.T) {
	s := fxDecode(t, `
name: m
joulie: {inputPreset: otel, set: {a: "1", b: "2"}, values: [v1.yaml]}
simulator: {set: {x: "1"}}
variants:
- {name: joulie-off, joulie: {enabled: false}}
- {name: kepler, joulie: {inputPreset: kepler, set: {b: "3", c: "4"}, values: [v2.yaml]}, simulator: {set: {w: "2"}}}
`)
	off, err := s.ForVariant("joulie-off")
	if err != nil {
		t.Fatal(err)
	}
	if *off.Joulie.Enabled || off.Joulie.InputPreset != "otel" || off.Joulie.Set["b"] != "2" || len(off.Variants) != 0 {
		t.Errorf("off: %+v", off.Joulie)
	}
	k, err := s.ForVariant("kepler")
	if err != nil {
		t.Fatal(err)
	}
	want := scenario.ScalarMap{"a": "1", "b": "3", "c": "4"}
	if k.Joulie.InputPreset != "kepler" || !reflect.DeepEqual(k.Joulie.Set, want) || !reflect.DeepEqual(k.Joulie.Values, []string{"v1.yaml", "v2.yaml"}) ||
		!reflect.DeepEqual(k.Simulator.Set, scenario.ScalarMap{"x": "1", "w": "2"}) || k.Joulie.Enabled != nil {
		t.Errorf("kepler: %+v %+v", k.Joulie, k.Simulator)
	}
	if s.Joulie.Set["b"] != "2" || len(s.Joulie.Values) != 1 {
		t.Errorf("merging changed the top level: %+v", s.Joulie)
	}
	if _, err := s.ForVariant(""); err == nil {
		t.Error("the unnamed variant of a scenario with variants")
	}
	if _, err := s.ForVariant("nope"); err == nil {
		t.Error("an unknown variant")
	}
}

// workloadgen gets the scenario's flags sorted, then the runner's: the
// seed, the base speed and the v3 schema.
func TestWorkloadgenArgs(t *testing.T) {
	s := fxDecode(t, "name: g\nseed: 7\nworkload: {baseSpeedPerCore: 2, generate: {workloadgen: {jobs: 40, arrival-window-sec: 3300, mode: default}}}\n")
	got := scenario.WorkloadgenArgs(s, "/tmp/t.jsonl")
	want := []string{"-arrival-window-sec=3300", "-jobs=40", "-mode=default", "-seed=7", "-base-speed-per-core=2", "-schema-version=v3", "-out=/tmp/t.jsonl"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args %v, want %v", got, want)
	}
}

func goTool() string {
	if p := filepath.Join(runtime.GOROOT(), "bin", "go"); fileExists(p) {
		return p
	}
	return "go"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// moduleRoot is the repository root, found from the test's directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for !fileExists(filepath.Join(dir, "go.mod")) {
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
	return dir
}

// v3WorkloadgenFlags are the flags the trace v3 change adds to workloadgen.
var v3WorkloadgenFlags = []string{"arrival-window-sec", "cpu-sensitivity", "gpu-sensitivity", "max-duration-sec", "schema-version"}

// Every flag workloadgen has is in exactly one list: allowed for
// scenarios, set by the runner, or refused with a reason; and every listed
// flag exists. A new workloadgen flag fails here until it is placed. While
// the binary predates trace v3, the v3 flags are reported, not required.
func TestWorkloadgenFlagAllowlistMatchesBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "workloadgen")
	build := exec.Command(goTool(), "build", "-o", bin, "./simulator/cmd/workloadgen")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build workloadgen: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "-h").CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("workloadgen -h: %v", err)
	}
	have := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+-([a-z0-9-]+)`).FindAllStringSubmatch(string(out), -1) {
		have[m[1]] = true
	}
	if len(have) < 10 {
		t.Fatalf("parsed %d flags from workloadgen -h:\n%s", len(have), out)
	}
	listed := map[string]string{}
	place := func(list string, names ...string) {
		for _, n := range names {
			if prev, dup := listed[n]; dup {
				t.Errorf("flag %s is in both %s and %s", n, prev, list)
			}
			listed[n] = list
		}
	}
	place("WorkloadgenFlags", scenario.WorkloadgenFlags...)
	place("RunnerWorkloadgenFlags", scenario.RunnerWorkloadgenFlags...)
	for n := range scenario.RefusedWorkloadgenFlags {
		place("RefusedWorkloadgenFlags", n)
	}
	pendingV3 := !have["schema-version"]
	var missing []string
	for n, list := range listed {
		if have[n] {
			continue
		}
		if pendingV3 && contains(v3WorkloadgenFlags, n) {
			t.Logf("flag %s (%s) is not in this workloadgen yet: it comes with trace v3", n, list)
			continue
		}
		missing = append(missing, n+" ("+list+")")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("listed flags workloadgen does not have: %s", strings.Join(missing, ", "))
	}
	for n := range have {
		if _, ok := listed[n]; !ok && n != "h" && n != "help" {
			t.Errorf("workloadgen flag %s is in no list: allow it, let the runner set it, or refuse it with a reason", n)
		}
	}
	if !sort.StringsAreSorted(scenario.WorkloadgenFlags) {
		t.Error("WorkloadgenFlags is not sorted")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// plan refuses a workload whose offered load exceeds 0.9 for a resource,
// over the Nodes its pods can use, after writing the run directory. A load
// below that plans, and the tainted pool's CPUs do not count.
func TestPlanRejectsOverload(t *testing.T) {
	plan := func(doc string) (*scenario.Plan, string, error) {
		t.Helper()
		s := fxDecode(t, doc)
		if err := scenario.Validate(s, scenario.ValidateOptions{RepoRoot: repoRoot}); err != nil {
			t.Fatalf("invalid: %v", err)
		}
		dir := filepath.Join(t.TempDir(), "20261005T100000Z-load-abc")
		p, err := scenario.MakePlan(context.Background(), s, scenario.PlanOptions{RepoRoot: repoRoot, RunDir: dir, GitSHA: "abc"})
		return p, dir, err
	}
	// 2 x 95 allocatable CPUs (intel; nvl's vendor taint is tolerated only
	// by pods requesting its GPUs, and intel-tainted's by none) over 10 m:
	// 190 x 600 = 114000 core-seconds. 20 jobs x 60 cores x 100 s =
	// 120000, a load of 1.05.
	head := "schemaVersion: 1\nname: load\ncluster: {spec: tests/simcluster/specs/mini.yaml}\nduration: 10m\nworkload:\n  jobs:\n"
	p, dir, err := plan(head + "  - {name: big, class: standard, count: 20, every: 25s, requests: {cpu: \"60\"}, runtime: 100s}\n")
	var over *scenario.OverloadError
	if !errors.As(err, &over) || len(over.Over) != 1 || over.Over[0].Resource != "cpu" {
		t.Fatalf("error %v, want an overload of cpu", err)
	}
	if l := over.Over[0]; l.Capacity != 190 || l.Demand != 120000 || l.Load < 1.05 || l.Load > 1.06 {
		t.Errorf("cpu load %+v, want 120000 over 190 x 600 s", l)
	}
	if p == nil || !fileExists(filepath.Join(dir, workload.TraceFile)) || !fileExists(filepath.Join(dir, scenario.ScenarioEffectiveFile)) {
		t.Errorf("an overloaded plan still writes the run directory for inspection")
	}
	// A GPU job placed on the intel pool validates, since the nvl pool has
	// GPUs, but no pool its pods can use has one: plan names the resource
	// rather than reporting an infinite load.
	_, _, err = plan(head + "  - {name: g, class: standard, count: 1, every: 60s, requests: {cpu: \"1\"}, gpus: {resource: nvidia.com/gpu, count: 1}, runtime: 60s}\n" +
		"  placement: {nodeSelector: {" + clusterspec.LabelPool + ": intel}}\n")
	if err == nil || errors.As(err, &over) || !strings.Contains(err.Error(), "no usable pool has the requested resource nvidia.com/gpu (jobs g-0-0)") {
		t.Errorf("error %v, want no usable pool for nvidia.com/gpu", err)
	}
	// GPU: 1 node x 8 GPUs, usable through the toleration a pod requesting
	// nvidia.com/gpu carries; 10 jobs x 8 GPUs x 60 s over 600 s is 1.0.
	_, _, err = plan(head + "  - {name: g, class: standard, count: 10, every: 50s, requests: {cpu: \"1\"}, gpus: {resource: nvidia.com/gpu, count: 8}, runtime: 60s}\n")
	if !errors.As(err, &over) || over.Over[0].Resource != "nvidia.com/gpu" {
		t.Errorf("error %v, want an overload of nvidia.com/gpu", err)
	}
	p, dir, err = plan(head + "  - {name: ok, class: performance, count: 10, every: 50s, requests: {cpu: \"90\"}, gpus: {resource: nvidia.com/gpu, count: 2}, runtime: 100s}\n" +
		"joulie: {inputPreset: kepler, set: {controllerManager.env.PROMETHEUS_RATE_WINDOW: 2m}}\nvariants: [{name: a}, {name: b, joulie: {enabled: false}}]\n")
	if err != nil {
		t.Fatalf("a load of %v: %v", p.Load, err)
	}
	for _, l := range p.Load {
		if l.Load > scenario.MaxOfferedLoad {
			t.Errorf("load %+v", l)
		}
	}
	if p.Jobs != 10 || len(p.Variants) != 2 {
		t.Errorf("plan %+v", p)
	}
	for i, name := range []string{"a", "b"} {
		env, err := scenario.ReadVariantEnv(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if env.RunID != "20261005T100000Z-load-abc-"+name || env.ClusterName != "mini" || env.Telemetry != "kepler" ||
			env.JoulieEnabled != (i == 0) || env.RateWindow != 2*time.Minute || env.ControllerReconcile != 10*time.Second ||
			!contains(env.JoulieSet, "controllerManager.env.NODE_POWER_PRESET=kepler") || env.Isolation != scenario.IsolationCluster {
			t.Errorf("variant %s env %+v", name, env)
		}
		if !fileExists(filepath.Join(dir, name, workload.TraceFile)) {
			t.Errorf("variant %s has no trace", name)
		}
	}
}
