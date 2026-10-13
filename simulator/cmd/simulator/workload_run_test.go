package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
	"github.com/matbun/joulie/simulator/pkg/hwemu/smi"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// v1TwoNodeSpec is two identical Intel hosts with no base load, so a job
// alone loads its node.
const v1TwoNodeSpec = `apiVersion: sim.joulie.io/v1alpha1
kind: ClusterSpec
metadata: {name: v1test}
spec:
  facility: {ambientC: 20, amplitudeC: 0, periodH: 24}
  pools:
  - {name: a, profile: intel-xeon-2s-rapl, count: 1}
  - {name: b, profile: intel-xeon-2s-rapl, count: 1}
`

// v1GPUSpec is one H100 NVL host with no base load.
const v1GPUSpec = `apiVersion: sim.joulie.io/v1alpha1
kind: ClusterSpec
metadata: {name: v1gpu}
spec:
  facility: {ambientC: 20, amplitudeC: 0, periodH: 24}
  pools:
  - {name: nvl, profile: nvidia-h100-nvl-8gpu, count: 1}
`

// snapNode is node's entry of snap.
func snapNode(t *testing.T, snap *emulate.Snapshot, node string) emulate.NodeSnapshot {
	t.Helper()
	for _, n := range snap.Nodes {
		if n.Name == node {
			return n
		}
	}
	t.Fatalf("no node %s in snapshot %d", node, snap.Seq)
	return emulate.NodeSnapshot{}
}

// cpuUtil is the mean utilization of node's CPU packages in snap.
func cpuUtil(t *testing.T, snap *emulate.Snapshot, node string) float64 {
	t.Helper()
	sum, k := 0.0, 0
	for _, c := range snapNode(t, snap, node).Components {
		if c.Type == semconv.HwTypeCPU {
			sum += c.Utilization
			k++
		}
	}
	if k == 0 {
		t.Fatalf("node %s has no CPU package", node)
	}
	return sum / float64(k)
}

// TestEmulateDefaultModelIsV1: in emulate mode the workload model is v1
// with its defaults, a trace with the model unset passes the mode check,
// and the legacy model is refused there.
func TestEmulateDefaultModelIsV1(t *testing.T) {
	env := map[string]string{envEmulate: "true", envClusterSpec: "spec.yaml", envWorkloadTrace: "trace.jsonl"}
	o, _, err := loadWorkloadOptions(func(k string) string { return env[k] }, true)
	if err != nil {
		t.Fatal(err)
	}
	if o.model() != workload.ModelV1 || o.Owner != workload.OwnerJob || o.Completion != workload.CompletionKWOK || o.maxCreatesPerTick() != workload.Unlimited {
		t.Errorf("emulate options %+v, want model v1, owner job, completion kwok, no create cap", o)
	}
	if err := checkModes(func(k string) string { return env[k] }); err != nil {
		t.Errorf("emulate with a trace and the model unset: %v", err)
	}
	env[workload.EnvModel] = string(workload.ModelLegacy)
	if err := checkModes(func(k string) string { return env[k] }); err == nil {
		t.Error("emulate with a legacy trace passed the mode check")
	}
	if _, _, err := loadWorkloadOptions(func(k string) string { return env[k] }, true); err == nil {
		t.Error("loadWorkloadOptions accepted the legacy model in emulate mode")
	}
}

// runAPIDo sends a request with an optional JSON body to h.
func runAPIDo(t *testing.T, h http.Handler, method, path string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	w := emulateTestDo(t, h, method, path, &buf)
	return w.Code, w.Body.Bytes()
}

// TestRunAPILifecycle drives the run API: 412 outside v1 emulate mode;
// 400 naming the line of a bad trace; 201 for a run; 409 for a second
// while it is active; the status as the run ticks; a finish with cleanup
// that ends the jobs incomplete, closes the files and deletes the Job
// objects; and a new run after it.
func TestRunAPILifecycle(t *testing.T) {
	legacy := newSimulatorWithRegisterer(defaultBaseModelFromEnv(), nil, nil, 10, prometheus.NewRegistry())
	lmux := http.NewServeMux()
	legacy.registerRunHandlers(lmux, workloadOptions{})
	for _, p := range []string{workload.PathRuns, workload.PathRunCurrent, workload.PathRunFinish} {
		if code, _ := runAPIDo(t, lmux, http.MethodPost, p, nil); code != http.StatusPreconditionFailed {
			t.Errorf("POST %s without emulate mode: %d, want 412", p, code)
		}
	}

	fake := k8sfake.NewSimpleClientset()
	prev := kubeClientFactory
	kubeClientFactory = func(float32, int, time.Duration) (kubernetes.Interface, error) { return fake, nil }
	t.Cleanup(func() { kubeClientFactory = prev })
	et := newEmulateTest(t, emulateTestConfig{})
	runs := t.TempDir()
	opts := v1Options(t, map[string]string{workload.EnvRunsDir: runs})
	mux := http.NewServeMux()
	et.s.registerRunHandlers(mux, opts)
	writeRunTrace := func(id string, lines ...string) {
		dir := filepath.Join(runs, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, workload.TraceFile), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if code, _ := runAPIDo(t, mux, http.MethodGet, workload.PathRunCurrent, nil); code != http.StatusNotFound {
		t.Errorf("GET current before any run: %d, want 404", code)
	}
	if code, _ := runAPIDo(t, mux, http.MethodGet, workload.PathRuns, nil); code != http.StatusMethodNotAllowed {
		t.Errorf("GET %s: %d, want 405", workload.PathRuns, code)
	}
	writeRunTrace("bad", traceJob("a", nil), `{"type":"job",`)
	code, body := runAPIDo(t, mux, http.MethodPost, workload.PathRuns, workload.RunRequest{RunID: "bad"})
	var rerr workload.RunError
	if err := json.Unmarshal(body, &rerr); err != nil || code != http.StatusBadRequest || rerr.Line != 2 {
		t.Errorf("POST a trace broken on line 2: %d %s, want 400 naming line 2", code, body)
	}

	writeRunTrace("r1", traceJob("a", nil), traceJob("b", nil))
	code, body = runAPIDo(t, mux, http.MethodPost, workload.PathRuns, workload.RunRequest{RunID: "r1"})
	var started workload.RunStarted
	if err := json.Unmarshal(body, &started); err != nil || code != http.StatusCreated || started.Jobs != 2 || started.Groups != 2 {
		t.Fatalf("POST r1: %d %s, want 201 with 2 jobs in 2 groups", code, body)
	}
	if code, _ := runAPIDo(t, mux, http.MethodPost, workload.PathRuns, workload.RunRequest{RunID: "r2"}); code != http.StatusConflict {
		t.Errorf("POST r2 while r1 runs: %d, want 409", code)
	}
	et.tick(t, emulateTestStart.Add(time.Second))
	waitFor(t, "the run's two Job objects", func() bool {
		list, _ := fake.BatchV1().Jobs("").List(context.Background(), metav1.ListOptions{})
		return list != nil && len(list.Items) == 2
	})
	et.tick(t, emulateTestStart.Add(3*time.Second))
	code, body = runAPIDo(t, mux, http.MethodGet, workload.PathRunCurrent, nil)
	var st workload.RunStatus
	if err := json.Unmarshal(body, &st); err != nil || code != http.StatusOK || st.Phase != workload.PhaseRunning || st.ElapsedSeconds != 2 || !st.AllSubmitted || st.Jobs.Pending != 2 {
		t.Errorf("GET current: %d %s, want running for 2 s, both submitted and pending", code, body)
	}

	code, body = runAPIDo(t, mux, http.MethodPost, workload.PathRunFinish+"?"+workload.QueryCleanup+"=true", nil)
	if err := json.Unmarshal(body, &st); err != nil || code != http.StatusOK || st.Phase != workload.PhaseAborted || st.Jobs.Incomplete != 2 {
		t.Errorf("finish: %d %s, want 200, aborted, 2 incomplete", code, body)
	}
	evs := readRunLogs(t, filepath.Join(runs, "r1"))
	if len(named(evs, semconv.EventSimRunFinished.Name, "")) != 1 || len(named(evs, semconv.EventSimJobIncomplete.Name, "")) != 2 {
		t.Errorf("r1 logs hold %d run.finished and %d job.incomplete, want 1 and 2",
			len(named(evs, semconv.EventSimRunFinished.Name, "")), len(named(evs, semconv.EventSimJobIncomplete.Name, "")))
	}
	waitFor(t, "the cleanup of r1's Job objects", func() bool {
		list, _ := fake.BatchV1().Jobs("").List(context.Background(), metav1.ListOptions{})
		return list != nil && len(list.Items) == 0
	})
	if loads, base := et.s.emulateLoads(); !base || len(loads) != 0 {
		t.Errorf("after the run: loads %v, base %v; want the base load back", loads, base)
	}
	if code, _ := runAPIDo(t, mux, http.MethodPost, workload.PathRuns, workload.RunRequest{RunID: "r1"}); code != http.StatusConflict {
		t.Errorf("POST r1 again over its results: %d, want 409", code)
	}
	writeRunTrace("r2", traceJob("c", nil))
	if code, body := runAPIDo(t, mux, http.MethodPost, workload.PathRuns, workload.RunRequest{RunID: "r2"}); code != http.StatusCreated {
		t.Errorf("POST r2 after r1 ended: %d %s, want 201", code, body)
	}
	et.s.run.Load().finish(false)
}

// waitFor polls cond for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestV1BaseLoadSuppressedDuringRun: the spec's base load drives the nodes
// until a run starts; while it is active only its jobs load them, and the
// base load returns once it ends (X9).
func TestV1BaseLoadSuppressedDuringRun(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{})
	intel := et.node(t, "intel")
	snap := et.tick(t, emulateTestStart.Add(time.Second))
	if u := cpuUtil(t, snap, intel); math.Abs(u-0.6) > 1e-6 {
		t.Fatalf("before the run, %s runs at %v, want the 0.6 base load", intel, u)
	}
	later := traceJob("a", func(r map[string]any) { r["submitTimeOffsetSec"] = 3600.0 })
	h := newV1Harness(t, et.s, nil, et, []string{later}, workload.RunOptions{})
	h.step(2)
	snap = h.step(3)
	if u := cpuUtil(t, snap, intel); u != 0 {
		t.Errorf("during a run with no job running, %s runs at %v, want 0", intel, u)
	}
	h.run.finish(false)
	snap = h.step(4)
	if u := cpuUtil(t, snap, intel); math.Abs(u-0.6) > 1e-6 {
		t.Errorf("after the run, %s runs at %v, want the 0.6 base load", intel, u)
	}
}

// v1CPUJob is a compute-bound job that holds 24 CPUs, one socket's first
// threads on the Intel hosts, and never finishes in a test.
func v1CPUJob(id string) string {
	return traceJob(id, func(r map[string]any) {
		r["podTemplate"] = map[string]any{"requests": map[string]any{"cpu": "24"}}
		r["work"] = map[string]any{"cpuUnits": 1e9}
		r["workloadClass"] = map[string]any{"cpu": "cpu.compute_bound"}
		r["workloadProfile"] = map[string]any{"cpuUtilization": 1.0, "memoryIntensity": 0.1, "ioIntensity": 0.0}
	})
}

// TestV1CPUCapSlowsCPUJob: two identical jobs on two identical hosts; a
// package power limit written into one host's powercap tree, as the agent
// writes it, slows the job on that host and only that one. The job on the
// uncapped host runs at its ideal rate.
func TestV1CPUCapSlowsCPUJob(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{spec: v1TwoNodeSpec})
	a, b := et.node(t, "a"), et.node(t, "b")
	h := newV1Harness(t, et.s, nil, et, []string{v1CPUJob("ja"), v1CPUJob("jb")}, workload.RunOptions{})
	h.step(0)
	h.step(1)
	h.runOn("ja", a)
	h.runOn("jb", b)
	h.step(2)
	pl1 := filepath.Join(et.root, a, layout.SysDir, layout.PowercapZoneDir("intel-rapl", 0), "constraint_0_power_limit_uw")
	if err := os.WriteFile(pl1, []byte("60000000\n"), 0); err != nil {
		t.Fatal(err)
	}
	for sec := 12.0; sec <= 62; sec += 10 {
		h.step(sec)
	}
	// Measure once the cap has settled.
	ja0, jb0 := h.job("ja").workCPU, h.job("jb").workCPU
	for sec := 72.0; sec <= 122; sec += 10 {
		h.step(sec)
	}
	da, db := h.job("ja").workCPU-ja0, h.job("jb").workCPU-jb0
	t.Logf("work in 60 s: %v under the limit, %v uncapped (ratio %.4f)", da, db, da/db)
	if math.Abs(db-24*60) > 1e-6*24*60 {
		t.Errorf("uncapped job did %v units in 60 s, want the ideal %v", db, 24*60)
	}
	if !(da < 0.98*db) || da <= 0 {
		t.Errorf("job under a 60 W package limit did %v units in 60 s, the uncapped one %v; want it slower", da, db)
	}
}

// TestV1ProgressUsesEnforcedLimitOfAssignedGPU: two 1-GPU jobs on an H100
// NVL host; nvidia-smi lowers the limit of the GPU one of them holds, and
// only that job slows.
func TestV1ProgressUsesEnforcedLimitOfAssignedGPU(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{spec: v1GPUSpec})
	node := et.node(t, "nvl")
	gpuJob := func(id string) string {
		return traceJob(id, func(r map[string]any) {
			r["podTemplate"] = map[string]any{"requests": map[string]any{"cpu": "1", "nvidia.com/gpu": "1"}}
			r["work"] = map[string]any{"cpuUnits": 1e9, "gpuUnits": 1e9}
			r["workloadClass"] = map[string]any{"gpu": "gpu.compute_bound"}
			r["workloadProfile"] = map[string]any{"cpuUtilization": 0.2, "gpuUtilization": 1.0, "cpuFeedIntensityGpu": 0.0}
		})
	}
	h := newV1Harness(t, et.s, nil, et, []string{gpuJob("a"), gpuJob("b")}, workload.RunOptions{})
	h.step(0)
	h.step(1)
	h.runOn("a", node)
	h.runOn("b", node)
	h.step(2)
	held := map[int64]string{}
	for _, e := range named(h.events(), semconv.EventSimJobStarted.Name, "") {
		id, _ := e.Attributes.String(semconv.AttrSimJobID)
		devs, _ := e.Attributes[semconv.AttrSimGPUIndices].([]any)
		if len(devs) != 1 {
			t.Fatalf("job %s holds GPUs %v, want one", id, e.Attributes[semconv.AttrSimGPUIndices])
		}
		held[devs[0].(int64)] = id
	}
	capped, free := held[1], held[0]
	if capped == "" || free == "" {
		t.Fatalf("GPUs held %v, want GPUs 0 and 1", held)
	}
	runner := smi.Runner{Env: smi.Env{SysRoot: filepath.Join(et.root, node, layout.SysDir), StateRoot: filepath.Join(et.root, node, layout.StateDir)}}
	if out, err := runner.Run(context.Background(), "nvidia-smi", "-i", "1", "-pl", "200"); err != nil {
		t.Fatalf("nvidia-smi -i 1 -pl 200: %v (%q)", err, out)
	}
	for sec := 12.0; sec <= 32; sec += 10 {
		h.step(sec)
	}
	c0, f0 := h.job(capped).workGPU, h.job(free).workGPU
	for sec := 42.0; sec <= 72; sec += 10 {
		h.step(sec)
	}
	dc, df := h.job(capped).workGPU-c0, h.job(free).workGPU-f0
	t.Logf("GPU work in 40 s: %v on GPU 1 under 200 W, %v on GPU 0 (ratio %.4f)", dc, df, dc/df)
	if df <= 0 || !(dc < 0.98*df) {
		t.Errorf("job on GPU 1 under 200 W did %v GPU units in 40 s, the job on GPU 0 %v; want it slower", dc, df)
	}
}

// TestV1EnergyAttributionMatchesSnapshotLedger: over a run, the energy
// attributed per node, every class and both hardware types summed, equals
// the growth of the node's CPU package and GPU ledgers in the snapshots.
func TestV1EnergyAttributionMatchesSnapshotLedger(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{})
	intel, nvl := et.node(t, "intel"), et.node(t, "nvl")
	gpuJob := traceJob("g", func(r map[string]any) {
		r["intentClass"] = workload.ClassPerformance
		r["podTemplate"] = map[string]any{"requests": map[string]any{"cpu": "4", "nvidia.com/gpu": "2"}}
		r["work"] = map[string]any{"cpuUnits": 1e9, "gpuUnits": 1e9}
		r["workloadProfile"] = map[string]any{"cpuUtilization": 0.5, "gpuUtilization": 0.9}
	})
	h := newV1Harness(t, et.s, nil, et, []string{v1CPUJob("c"), gpuJob}, workload.RunOptions{})
	first := h.step(0)
	h.step(1)
	h.runOn("c", intel)
	h.runOn("g", nvl)
	var last *emulate.Snapshot
	for sec := 2.0; sec <= 20; sec += 3 {
		last = h.step(sec)
	}
	h.run.finish(false)
	ledger := func(snap *emulate.Snapshot, node string) float64 {
		sum := 0.0
		for _, c := range snapNode(t, snap, node).Components {
			if c.Type == semconv.HwTypeCPU || c.Type == semconv.HwTypeGPU {
				sum += c.EnergyJ
			}
		}
		return sum
	}
	_, points := readRunMetrics(t, h.run.dir)
	byNode := map[string]float64{}
	byClass := map[string]float64{}
	for _, p := range points {
		if p.Metric != semconv.JoulieSimWorkloadEnergy.Name {
			continue
		}
		node, _ := p.Attributes.String(semconv.AttrK8sNodeName)
		class, _ := p.Attributes.String(semconv.AttrJoulieWorkloadClass)
		byNode[node] += p.Value
		byClass[class] += p.Value
	}
	for _, node := range []string{intel, nvl} {
		want := ledger(last, node) - ledger(first, node)
		if want <= 0 || math.Abs(byNode[node]-want) > 1e-6*want {
			t.Errorf("%s: %v J attributed, the ledgers grew %v J", node, byNode[node], want)
		}
	}
	for _, c := range []string{workload.ClassPerformance, workload.ClassStandard, workload.ClassIdle} {
		if byClass[c] <= 0 {
			t.Errorf("class %s got %v J, want some (classes %v)", c, byClass[c], byClass)
		}
	}
}

// TestV1HardwareEventsReachRunLogs: a package limit written during a run
// shows in the run's own logs as cap.applied on the node's resource, which
// carries the run id.
func TestV1HardwareEventsReachRunLogs(t *testing.T) {
	et := newEmulateTest(t, emulateTestConfig{})
	intel := et.node(t, "intel")
	h := newV1Harness(t, et.s, nil, et, []string{traceJob("a", nil)}, workload.RunOptions{})
	h.step(0)
	pl1 := filepath.Join(et.root, intel, layout.SysDir, layout.PowercapZoneDir("intel-rapl", 0), "constraint_0_power_limit_uw")
	if err := os.WriteFile(pl1, []byte("120000000\n"), 0); err != nil {
		t.Fatal(err)
	}
	h.step(1)
	h.step(2)
	caps := named(h.events(), semconv.EventSimCapApplied.Name, "")
	if len(caps) == 0 {
		t.Fatal("no cap.applied in the run's logs")
	}
	for _, e := range caps {
		node, _ := e.Resource.String(semconv.AttrK8sNodeName)
		run, _ := e.Resource.String(semconv.AttrSimRunID)
		if node != intel || run != "r1" {
			t.Errorf("cap.applied on resource node %q run %q, want %s and r1", node, run, intel)
		}
	}
}

// TestRunTraceErrorsNameTheLineOfTheirFile: in a trace of several parts, a
// bad line and a jobId that repeats one of an earlier part are named by
// their line in their own part, in the RunError and in its message. Before
// the fix a parse error's message counted the lines of every earlier part,
// and a repeat of an earlier part's jobId named no line.
func TestRunTraceErrorsNameTheLineOfTheirFile(t *testing.T) {
	write := func(dir, name string, lines ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	opts := v1Options(t, nil)

	broken := t.TempDir()
	write(broken, "a.jsonl", traceJob("a1", nil), traceJob("a2", nil), traceJob("a3", nil))
	write(broken, "b.jsonl", traceJob("b1", nil), `{"type":"job",`)
	_, err := loadRunTrace(broken, opts)
	var re *workload.RunError
	if !errors.As(err, &re) || re.Line != 2 || !strings.Contains(re.Message, "line 2 ") || !strings.Contains(re.Message, "b.jsonl") {
		t.Errorf("parse error on line 2 of b.jsonl: %v, want a RunError on line 2 whose message names line 2 of b.jsonl", err)
	}

	repeated := t.TempDir()
	write(repeated, "a.jsonl", traceJob("x", nil))
	write(repeated, "b.jsonl", traceJob("y", nil), traceJob("x", nil))
	_, err = loadRunTrace(repeated, opts)
	if !errors.As(err, &re) || re.Line != 2 || !strings.Contains(re.Message, "b.jsonl") {
		t.Errorf("jobId x of a.jsonl repeated on line 2 of b.jsonl: %v, want a RunError on line 2 naming b.jsonl", err)
	}

	same := t.TempDir()
	write(same, "a.jsonl", traceJob("x", nil), traceJob("y", nil), traceJob("x", nil))
	_, err = loadRunTrace(same, opts)
	if !errors.As(err, &re) || re.Line != 3 {
		t.Errorf("jobId x repeated on line 3 of the same part: %v, want a RunError on line 3", err)
	}
}
