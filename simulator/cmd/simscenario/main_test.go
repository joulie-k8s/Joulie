package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/kube"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/scenario"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// fixture is the scenario package's committed variant directory of a
// scenario without variants, and repo its mini repository.
const (
	fixture = "../../pkg/scenario/testdata/variant"
	repo    = "../../pkg/scenario/testdata/repo"
)

func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

// copyDir copies the fixture into a temporary directory.
func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "20261004T200000Z-fixture-0123456789ab")
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// withAssertions rewrites the effective scenario of dir with the
// fixture's assertions plus extra ones.
func withAssertions(t *testing.T, dir string, extra ...string) {
	t.Helper()
	s, _, err := scenario.ReadEffectiveScenario(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range extra {
		var a scenario.Assertion
		if err := yaml.UnmarshalStrict([]byte(doc), &a); err != nil {
			t.Fatal(err)
		}
		s.Assertions = append(s.Assertions, a)
	}
	b, err := yaml.Marshal(scenario.Effective(s))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, scenario.ScenarioEffectiveFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// evaluate exits 0 when every check of severity fail passes, even with a
// failed warning; 1 when one of severity fail fails; 2 when the results
// cannot be read or the run never started. Each writes summary.json with
// the matching status and the harness records of its checks.
func TestEvaluateExitCodes(t *testing.T) {
	for _, c := range []struct {
		name   string
		extra  []string
		remove string
		code   int
		status string
	}{
		{"pass with a warning", nil, "", scenario.ExitPass, semconv.TestSuiteRunStatusSuccess},
		{"failed warning only", []string{`{name: w2, type: jobs, expect: {failed: "== 0"}, severity: warn}`}, "", scenario.ExitPass, semconv.TestSuiteRunStatusSuccess},
		{"failed check", []string{`{name: f, type: jobs, expect: {failed: "== 0"}}`}, "", scenario.ExitFail, semconv.TestSuiteRunStatusFailure},
		{"no data", []string{`{name: nodata, type: energy, scope: "node:nope", expect: ">= 0"}`}, "", scenario.ExitFail, semconv.TestSuiteRunStatusFailure},
		{"never started", nil, scenario.RunStatusFile, scenario.ExitError, semconv.TestSuiteRunStatusAborted},
		{"no variant.env", nil, scenario.VariantEnvFile, scenario.ExitError, semconv.TestSuiteRunStatusAborted},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := copyDir(t, fixture)
			if len(c.extra) > 0 {
				withAssertions(t, dir, c.extra...)
			}
			if c.remove != "" {
				if err := os.Remove(filepath.Join(dir, c.remove)); err != nil {
					t.Fatal(err)
				}
			}
			code, out, errOut := runCmd(t, "evaluate", dir)
			if code != c.code {
				t.Fatalf("exit %d, want %d\n%s%s", code, c.code, out, errOut)
			}
			sum, err := scenario.ReadSummary(filepath.Join(dir, scenario.SummaryFile))
			if err != nil {
				t.Fatal(err)
			}
			if sum.Status != c.status || sum.ExitCode != c.code {
				t.Errorf("summary %s exit %d, want %s %d", sum.Status, sum.ExitCode, c.status, c.code)
			}
			if c.code == scenario.ExitError && c.remove == scenario.VariantEnvFile {
				return
			}
			logs, err := os.ReadFile(filepath.Join(dir, workload.OTLPDir, scenario.HarnessLogsFile))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{semconv.EventScenarioAssertionEvaluated.Name, semconv.EventScenarioFinished.Name, `"stringValue":"` + c.status + `"`} {
				if !strings.Contains(string(logs), want) {
					t.Errorf("harness logs lack %s", want)
				}
			}
			// summarize of a scenario without variants reports the one
			// summary's code.
			if code, _, _ := runCmd(t, "summarize", dir); code != c.code {
				t.Errorf("summarize exit %d, want %d", code, c.code)
			}
		})
	}
	if code, _, _ := runCmd(t, "evaluate", filepath.Join(t.TempDir(), "missing")); code != scenario.ExitError {
		t.Errorf("a missing directory: exit %d, want %d", code, scenario.ExitError)
	}
}

// evaluate appends its records to the harness files it also reads, so a
// second evaluate must not see the first's: the joulie.scenario.finished
// record it wrote would otherwise turn "== 0" into 1.
func TestEvaluateTwiceGivesTheSameResults(t *testing.T) {
	dir := copyDir(t, fixture)
	withAssertions(t, dir, `{name: fin, type: events, event: joulie.scenario.finished, expect: "== 0"}`,
		`{name: evaluated, type: events, event: joulie.scenario.assertion.evaluated, expect: "== 0"}`)
	var first []byte
	for i := 0; i < 2; i++ {
		code, out, errOut := runCmd(t, "evaluate", dir)
		if code != scenario.ExitPass {
			t.Fatalf("evaluate %d: exit %d\n%s%s", i+1, code, out, errOut)
		}
		sum, err := scenario.ReadSummary(filepath.Join(dir, scenario.SummaryFile))
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(sum.Assertions)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = got
		} else if !bytes.Equal(got, first) {
			t.Errorf("the second evaluate differs:\n%s\nfirst:\n%s", got, first)
		}
	}
}

// Usage errors exit 2 and print the usage.
func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"bogus"}, {"validate"}, {"plan", "x.yaml"}, {"run"}, {"evaluate", "a", "b"}, {"validate", "-nope", "x"}} {
		code, _, errOut := runCmd(t, args...)
		if code != scenario.ExitError || errOut == "" {
			t.Errorf("%v: exit %d stderr %q", args, code, errOut)
		}
	}
}

// validate and plan read a scenario file against a repository; plan writes
// the run directory and prints the offered load.
func TestValidateAndPlanCommands(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "valid.yaml")
	doc := `schemaVersion: 1
name: valid
cluster: {spec: tests/simcluster/specs/mini.yaml}
duration: 10m
workload:
  jobs: [{name: cpu, class: performance, count: 2, every: 60s, requests: {cpu: "4"}, runtime: 2m}]
assertions: [{name: done, type: jobs, expect: {completedFraction: ">= 0.9"}}]
`
	if err := os.WriteFile(file, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runCmd(t, "validate", file, "-repo", repo); code != 0 || !strings.Contains(out, "scenario valid is valid") {
		t.Fatalf("validate: %d %s %s", code, out, errOut)
	}
	runDir := filepath.Join(dir, "runs", "r1")
	code, out, errOut := runCmd(t, "plan", file, "-repo", repo, "-out", runDir)
	if code != 0 || !strings.Contains(out, "2 jobs due within 10m0s") || !strings.Contains(out, "cpu") {
		t.Fatalf("plan: %d\n%s%s", code, out, errOut)
	}
	for _, f := range []string{scenario.ScenarioEffectiveFile, workload.TraceFile, scenario.VariantEnvFile} {
		if _, err := os.Stat(filepath.Join(runDir, f)); err != nil {
			t.Errorf("plan wrote no %s: %v", f, err)
		}
	}
	bad := filepath.Join(dir, "other.yaml")
	if err := os.WriteFile(bad, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCmd(t, "validate", bad, "-repo", repo); code != scenario.ExitError || !strings.Contains(errOut, "file stem") {
		t.Errorf("validate of a misnamed file: %d %s", code, errOut)
	}
}

// summarize of a scenario with variants writes the top summary with each
// variant's result and the comparisons, and exits with the worst code.
func TestSummarizeVariants(t *testing.T) {
	runDir := t.TempDir()
	s := scenario.Effective(scenario.Scenario{SchemaVersion: 1, Name: "cmp", Cluster: scenario.Cluster{Spec: "x"},
		Variants: []scenario.Variant{{Name: "a"}, {Name: "b"}},
		Compare:  []scenario.Compare{{Name: "energy", Metric: scenario.CompareEnergyTotal, Variant: "b", Reference: "a", Expect: "<= 1", Severity: scenario.SeverityWarn}}})
	b, err := yaml.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, scenario.ScenarioEffectiveFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, joules := range map[string]float64{"a": 100, "b": 120} {
		sum := scenario.NewSummary("cmp/" + name)
		sum.Energy = &scenario.EnergySummary{TotalJoules: joules}
		sum.Finish(nil)
		if err := os.MkdirAll(filepath.Join(runDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := scenario.WriteSummary(filepath.Join(runDir, name, scenario.SummaryFile), sum); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errOut := runCmd(t, "summarize", runDir)
	if code != scenario.ExitPass {
		t.Fatalf("summarize: %d\n%s%s", code, out, errOut)
	}
	top, err := scenario.ReadSummary(filepath.Join(runDir, scenario.SummaryFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(top.Variants) != 2 || len(top.Comparisons) != 1 || *top.Comparisons[0].Observed != 1.2 || len(top.Warnings) != 1 {
		t.Errorf("top summary %+v", top)
	}
	if err := os.Remove(filepath.Join(runDir, "b", scenario.SummaryFile)); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCmd(t, "summarize", runDir); code != scenario.ExitError {
		t.Errorf("a variant without a summary: exit %d, want %d", code, scenario.ExitError)
	}
}

// run drives one variant against a fake cluster: the Stage preflight, the
// start and end snapshots, the run request, NodeTwin captures until the
// simulator reports the run finished, the Prometheus captures and the
// promql assertion, the run status, the run end and the logs. evaluate
// then reads what it wrote. A run still going at its deadline is finished
// with cleanup and evaluates as timed_out; an interrupted one as aborted.
// Against KWOK's upstream pod-complete Stage, which completes Job pods on
// its own timer, run refuses before it starts anything.
func TestRunVariantAgainstFakes(t *testing.T) {
	for _, mode := range []string{"finished", "deadline", "interrupted", "upstream-stage"} {
		t.Run(mode, func(t *testing.T) { testRunVariant(t, mode) })
	}
}

func testRunVariant(t *testing.T, mode string) {
	dir := copyDir(t, fixture)
	for _, f := range []string{scenario.RunStatusFile, scenario.KubeDir, filepath.Join(workload.OTLPDir, scenario.HarnessMetricsFile),
		filepath.Join(workload.OTLPDir, scenario.HarnessLogsFile)} {
		if err := os.RemoveAll(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	s, _, err := scenario.ReadEffectiveScenario(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Capture.Interval = &metav1.Duration{Duration: 10 * time.Millisecond}
	neverFinish := mode == "deadline" || mode == "interrupted"
	if mode == "deadline" {
		s.Duration.Duration, s.Drain.Duration = 30*time.Millisecond, 0
	}
	s.Capture.Prometheus = []scenario.PromCapture{{Name: "joulie.capture.node.power", Unit: "W", Query: "sum by (k8s_node_name) (hw_power_watts)"}}
	b, err := yaml.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, scenario.ScenarioEffectiveFile), b, 0o644); err != nil {
		t.Fatal(err)
	}

	scheme, err := kube.NewScheme(v1alpha1.AddToScheme, batchv1.AddToScheme)
	if err != nil {
		t.Fatal(err)
	}
	selector := `{"key":".metadata.annotations[\"sim.joulie.io/complete\"]","operator":"In","values":["true"]}`
	if mode == "upstream-stage" {
		selector = `{"key":".metadata.ownerReferences.[].kind","operator":"In","values":["Job"]}`
	}
	stage := &unstructured.Unstructured{}
	if err := stage.UnmarshalJSON([]byte(`{"apiVersion":"kwok.x-k8s.io/v1alpha1","kind":"Stage","metadata":{"name":"pod-complete"},
"spec":{"selector":{"matchExpressions":[` + selector + `]}}}`)); err != nil {
		t.Fatal(err)
	}
	twin := &v1alpha1.NodeTwin{ObjectMeta: metav1.ObjectMeta{Name: "intel-0"}, Spec: v1alpha1.NodeTwinSpec{NodeName: "intel-0"},
		Status: v1alpha1.NodeTwinStatus{SchedulableClass: "performance", PowerMeasurement: &v1alpha1.PowerMeasurement{Source: "prometheus", MeasuredNodePowerW: 200}}}
	kc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stage, twin,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "intel-0"}}).Build()
	logs := k8sfake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "joulie-controller-manager-0", Namespace: scenario.NamespaceJoulie}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "joulie-agent-intel-0-x", Namespace: scenario.NamespaceJoulie, Labels: map[string]string{"sim.joulie.io/node": "intel-0"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "joulie-telemetry-sim-0", Namespace: scenario.NamespaceSimulator}},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var req workload.RunRequest
	var finishQuery string
	requests, polls, finishes := 0, 0, 0
	sim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case workload.PathRuns:
			json.NewDecoder(r.Body).Decode(&req)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(workload.RunStarted{RunID: req.RunID, Jobs: 6, Groups: 6})
		case workload.PathRunCurrent:
			polls++
			phase := workload.PhaseRunning
			if polls >= 3 && !neverFinish {
				phase = workload.PhaseFinished
			}
			if polls >= 3 && mode == "interrupted" {
				cancel()
			}
			json.NewEncoder(w).Encode(workload.RunStatus{RunID: req.RunID, Phase: phase, ElapsedSeconds: 1})
		case workload.PathRunFinish:
			finishes++
			finishQuery = r.URL.RawQuery
			json.NewEncoder(w).Encode(workload.RunStatus{RunID: req.RunID, Phase: workload.PhaseAborted})
		}
	}))
	defer sim.Close()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query_range" {
			w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"k8s_node_name":"intel-0"},"values":[[1759608000,"200"]]}]}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1759608000,"4"]}]}}`))
	}))
	defer prom.Close()
	c := &cluster{kube: kc, logs: logs, prom: &scenario.PromClient{HTTP: prom.Client(), BaseURL: prom.URL},
		sim: &scenario.SimClient{HTTP: sim.Client(), BaseURL: sim.URL}}
	var out, errOut bytes.Buffer
	err = runVariant(ctx, dir, c, &out, &errOut)
	switch {
	case mode == "upstream-stage":
		if err == nil || !strings.Contains(err.Error(), "stage-pod-complete.yaml") || requests != 0 {
			t.Errorf("run against the upstream Stage: %v after %d simulator requests, want a refusal and none", err, requests)
		}
		if _, err := os.Stat(filepath.Join(dir, scenario.RunStatusFile)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused run wrote %s: %v", scenario.RunStatusFile, err)
		}
		return
	case mode == "interrupted":
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted run: %v, want context.Canceled\n%s%s", err, out.String(), errOut.String())
		}
	case err != nil:
		t.Fatalf("run: %v\n%s%s", err, out.String(), errOut.String())
	}
	if req.RunID != "20261004T200000Z-fixture-0123456789ab" || req.ClusterName != "smoke" {
		t.Errorf("request %+v", req)
	}
	wantPhase := workload.PhaseFinished
	if neverFinish {
		wantPhase = workload.PhaseAborted
		if finishes != 1 || finishQuery != "cleanup=true" || (mode == "deadline") != strings.Contains(out.String(), "deadline") {
			t.Errorf("%d finishes with %q; output %s", finishes, finishQuery, out.String())
		}
	} else if finishes != 0 || polls != 3 {
		t.Errorf("%d finishes, %d polls, want 0 and 3", finishes, polls)
	}
	st, err := os.ReadFile(filepath.Join(dir, scenario.RunStatusFile))
	if err != nil || !strings.Contains(string(st), `"phase": "`+string(wantPhase)+`"`) {
		t.Errorf("run-status.json %s %v, want phase %s", st, err, wantPhase)
	}
	if end, err := scenario.ReadRunEnd(dir); err != nil || end.Reason != mode || end.At.IsZero() {
		t.Errorf("run end %+v %v, want reason %s", end, err, mode)
	}
	for _, f := range []string{"k8s/nodetwins-start.yaml", "k8s/pods-end.yaml", "logs/controller-manager.log", "logs/agent-intel-0.log", "logs/simulator.log"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("run wrote no %s: %v", f, err)
		}
	}
	metrics, _ := os.ReadFile(filepath.Join(dir, workload.OTLPDir, scenario.HarnessMetricsFile))
	for _, want := range []string{semconv.JoulieCaptureNodeTwinMeasuredPower.Name, "joulie.capture.node.power", semconv.JoulieScenarioAssertionValue.Name} {
		if !strings.Contains(string(metrics), want) {
			t.Errorf("harness metrics lack %s", want)
		}
	}
	harnessLogs, _ := os.ReadFile(filepath.Join(dir, workload.OTLPDir, scenario.HarnessLogsFile))
	if !strings.Contains(string(harnessLogs), semconv.EventScenarioStarted.Name) {
		t.Errorf("harness logs lack %s", semconv.EventScenarioStarted.Name)
	}
	rd, _, err := scenario.ReadRunData(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range scenario.Evaluate(rd, fxEnvOf(t, dir)) {
		if r.Name == "targets" && (r.Observed == nil || *r.Observed != 4) {
			t.Errorf("the promql assertion reads %v, want the recorded 4", r.Observed)
		}
	}
	code, evalOut, _ := runCmd(t, "evaluate", dir)
	sum, err := scenario.ReadSummary(filepath.Join(dir, scenario.SummaryFile))
	if err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "deadline":
		if code != scenario.ExitError || sum.Status != semconv.TestSuiteRunStatusTimedOut {
			t.Errorf("evaluate after the deadline: exit %d status %s, want %d %s\n%s", code, sum.Status, scenario.ExitError, semconv.TestSuiteRunStatusTimedOut, evalOut)
		}
	case "interrupted":
		if code != scenario.ExitError || sum.Status != semconv.TestSuiteRunStatusAborted {
			t.Errorf("evaluate after an interrupt: exit %d status %s, want %d %s\n%s", code, sum.Status, scenario.ExitError, semconv.TestSuiteRunStatusAborted, evalOut)
		}
	default:
		if code == scenario.ExitError || sum.Status == semconv.TestSuiteRunStatusTimedOut || sum.Status == semconv.TestSuiteRunStatusAborted {
			t.Errorf("evaluate after a finished run: exit %d status %s, want the checks' verdict\n%s", code, sum.Status, evalOut)
		}
	}
}

func fxEnvOf(t *testing.T, dir string) scenario.VariantEnv {
	t.Helper()
	env, err := scenario.ReadVariantEnv(dir)
	if err != nil {
		t.Fatal(err)
	}
	return env
}
