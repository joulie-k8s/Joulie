package scenario_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/scenario"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

func fxRunData(t *testing.T) (*scenario.RunData, scenario.VariantEnv) {
	t.Helper()
	rd, env, err := scenario.ReadRunData(fixtureDir)
	if err != nil {
		t.Fatalf("read %s: %v", fixtureDir, err)
	}
	return rd, env
}

// evalOne evaluates one assertion, given as a flow YAML object, against rd
// and returns its results by check name.
func evalOne(t *testing.T, rd *scenario.RunData, env scenario.VariantEnv, assertion string) map[string]scenario.AssertionResult {
	t.Helper()
	var a scenario.Assertion
	if err := yaml.UnmarshalStrict([]byte(assertion), &a); err != nil {
		t.Fatalf("%s: %v", assertion, err)
	}
	copy := *rd
	copy.Scenario.Assertions = []scenario.Assertion{a}
	out := map[string]scenario.AssertionResult{}
	for _, r := range scenario.Evaluate(&copy, env) {
		out[r.Name] = r
	}
	return out
}

// checkResult fails unless the result has the status and, when want is
// not NaN, the observed value.
func checkResult(t *testing.T, r scenario.AssertionResult, status string, want float64) {
	t.Helper()
	if r.Status != status {
		t.Errorf("%s: status %s, want %s (%s)", r.Name, r.Status, status, r.Message)
	}
	if math.IsNaN(want) {
		if r.Observed != nil {
			t.Errorf("%s: observed %g, want no data", r.Name, *r.Observed)
		}
		return
	}
	if r.Observed == nil || math.Abs(*r.Observed-want) > 1e-9*math.Max(1, math.Abs(want)) {
		got := "nil"
		if r.Observed != nil {
			got = strings.TrimSpace(strings.Split(r.Message, "\n")[0])
		}
		t.Errorf("%s: observed %v (%s), want %g", r.Name, r.Observed, got, want)
	}
}

const pass, failed = semconv.TestCaseResultStatusPass, semconv.TestCaseResultStatusFail

var noData = math.NaN()

// The fixture's assertions pass, except slow-warn, which fails as a
// warning: one of each type evaluated end to end from the directory.
func TestEvaluateFixture(t *testing.T) {
	rd, env := fxRunData(t)
	results := scenario.Evaluate(rd, env)
	if len(results) != 18 {
		t.Errorf("%d results, want 18 (one per check)", len(results))
	}
	for _, r := range results {
		want := pass
		if r.Name == "slow-warn" {
			want = failed
		}
		if r.Status != want || r.Severity == "" || r.Type == "" {
			t.Errorf("%s: %+v, want status %s", r.Name, r, want)
		}
	}
}

// jobs counts due jobs only: the late job due after the run ended is not
// due, and a job without a terminal event is incomplete.
func TestAssertJobs(t *testing.T) {
	rd, env := fxRunData(t)
	got := evalOne(t, rd, env, `{name: j, type: jobs, expect: {completedFraction: "== 0.6", completed: "== 3", failed: "== 1", incomplete: "== 1"}}`)
	checkResult(t, got["j.completedFraction"], pass, 0.6)
	checkResult(t, got["j.completed"], pass, 3)
	checkResult(t, got["j.failed"], pass, 1)
	checkResult(t, got["j.incomplete"], pass, 1)
	if msg := got["j.completed"].Message; !strings.Contains(msg, "5 due jobs (1 not due)") {
		t.Errorf("message %q does not report the not-due job", msg)
	}
	got = evalOne(t, rd, env, `{name: j, type: jobs, workload: perf, expect: {completedFraction: ">= 1"}}`)
	checkResult(t, got["j.completedFraction"], pass, 1)
	// A run that stopped after 20 s submitted only the jobs due by then.
	early := *rd
	st := *rd.Status
	st.ElapsedSeconds = 20
	early.Status = &st
	got = evalOne(t, &early, env, `{name: j, type: jobs, class: performance, expect: {completed: "== 1"}}`)
	checkResult(t, got["j.completed"], pass, 1)
}

// latency takes a quantile over completed jobs, or its ratio to the
// reference jobs' quantile.
func TestAssertLatency(t *testing.T) {
	rd, env := fxRunData(t)
	got := evalOne(t, rd, env, `{name: l, type: latency, class: performance, metric: slowdown, quantile: 0.5, expect: "< 1.2"}`)
	checkResult(t, got["l"], pass, 1.15)
	got = evalOne(t, rd, env, `{name: l, type: latency, metric: duration, quantile: max, expect: "<= 80s"}`)
	checkResult(t, got["l"], pass, 80)
	got = evalOne(t, rd, env, `{name: l, type: latency, metric: queueWait, quantile: 0.5, nodeProfile: performance, expect: "== 2s"}`)
	checkResult(t, got["l"], pass, 2)
	got = evalOne(t, rd, env, `{name: l, type: latency, class: performance, metric: duration, quantile: max, reference: {class: standard}, expect: "> 1.2"}`)
	checkResult(t, got["l"], pass, 80.0/65)
	got = evalOne(t, rd, env, `{name: l, type: latency, class: performance, metric: duration, quantile: max, reference: {workload: late}, expect: "> 1"}`)
	checkResult(t, got["l"], failed, noData)
}

// stall counts job stalls by reason and run stalls.
func TestAssertStall(t *testing.T) {
	rd, env := fxRunData(t)
	got := evalOne(t, rd, env, `{name: s, type: stall, expect: {gangIncomplete: "== 0", noProgress: "== 1", runStalled: ">= 1"}}`)
	checkResult(t, got["s.gangIncomplete"], pass, 0)
	checkResult(t, got["s.noProgress"], pass, 1)
	checkResult(t, got["s.runStalled"], pass, 1)
}

// placement counts a bind to a forbidden class only once the node held
// the class for the grace: the extender's cache TTL plus the controller's
// reconcile interval by default.
func TestAssertPlacement(t *testing.T) {
	rd, env := fxRunData(t)
	got := evalOne(t, rd, env, `{name: p, type: placement, class: standard, forbidden: [eco], expect: "== 2"}`)
	checkResult(t, got["p"], pass, 2)
	got = evalOne(t, rd, env, `{name: p, type: placement, class: standard, forbidden: [draining], expect: "== 0"}`)
	checkResult(t, got["p"], pass, 0)
}

// The performance pod bound 5 s after its node turned eco is the cache's
// lag inside the 40 s default grace, not a violation; with no grace it
// counts. Judging by the class at the bind alone would flake on it.
func TestPlacementGraceExcludesRecentFlip(t *testing.T) {
	rd, env := fxRunData(t)
	got := evalOne(t, rd, env, `{name: p, type: placement, class: performance, forbidden: [eco], expect: "== 0"}`)
	checkResult(t, got["p"], pass, 0)
	if !strings.Contains(got["p"].Message, "1 more within the grace") {
		t.Errorf("message %q does not report the excluded bind", got["p"].Message)
	}
	got = evalOne(t, rd, env, `{name: p, type: placement, class: performance, forbidden: [eco], graceSeconds: 0, expect: "== 0"}`)
	checkResult(t, got["p"], failed, 1)
	short := env
	short.CacheTTL, short.ControllerReconcile = 2*time.Second, 2*time.Second
	got = evalOne(t, rd, short, `{name: p, type: placement, class: performance, forbidden: [eco], expect: "== 0"}`)
	checkResult(t, got["p"], failed, 1)
}

// energy reads the last snapshot of the attributed energy, by scope and
// hardware type, and per completed job.
func TestAssertEnergy(t *testing.T) {
	rd, env := fxRunData(t)
	for _, c := range []struct {
		assertion string
		want      float64
	}{
		{`{name: e, type: energy, scope: total, expect: "== 144 kJ"}`, 144000},
		{`{name: e, type: energy, scope: total, hwType: gpu, expect: "> 0"}`, 90000},
		{`{name: e, type: energy, scope: "class:idle", expect: "> 0"}`, 27000},
		{`{name: e, type: energy, scope: "node:nvl-0", hwType: cpu, expect: "> 0"}`, 18000},
		{`{name: e, type: energy, scope: "class:performance", perJob: true, expect: "> 0"}`, 13500},
		{`{name: e, type: energy, scope: "node:intel-0", perJob: true, expect: "> 0"}`, 36000 / 2},
	} {
		checkResult(t, evalOne(t, rd, env, c.assertion)["e"], pass, c.want)
	}
	checkResult(t, evalOne(t, rd, env, `{name: e, type: energy, scope: "node:nope", expect: ">= 0"}`)["e"], failed, noData)
}

// events counts records whose attributes equal every where value, a number
// matching numerically and a boolean by value.
func TestAssertEvents(t *testing.T) {
	rd, env := fxRunData(t)
	for _, c := range []struct {
		assertion string
		want      float64
	}{
		{`{name: e, type: events, event: joulie.sim.job.completed, expect: "== 3"}`, 3},
		{`{name: e, type: events, event: joulie.sim.job.scheduled, where: {joulie.sim.job.queue_wait: 1}, expect: "== 2"}`, 2},
		{`{name: e, type: events, event: joulie.sim.job.scheduled, where: {joulie.node.class_since: 5.0, k8s.node.name: intel-0}, expect: "== 1"}`, 1},
		{`{name: e, type: events, event: joulie.sim.job.failed, where: {error.type: pod_deleted}, expect: "== 0"}`, 0},
		{`{name: e, type: events, event: joulie.scenario.started, expect: "== 1"}`, 1},
	} {
		checkResult(t, evalOne(t, rd, env, c.assertion)["e"], pass, c.want)
	}
}

// objects counts the end snapshot: the failed Job, the running pod on a
// fake Node, the hardware objects and the twin with a blocked GPU.
func TestAssertObjects(t *testing.T) {
	rd, env := fxRunData(t)
	got := evalOne(t, rd, env, `{name: o, type: objects, expect: {jobsNotComplete: "== 1", strayPodsOnFakeNodes: "== 1", nodeHardwares: "== 2", controlBlocked: "== 1"}}`)
	checkResult(t, got["o.jobsNotComplete"], pass, 1)
	checkResult(t, got["o.strayPodsOnFakeNodes"], pass, 1)
	checkResult(t, got["o.nodeHardwares"], pass, 2)
	checkResult(t, got["o.controlBlocked"], pass, 1)
	// Another run's Jobs are not this run's.
	other := *rd
	other.RunID = "another-run"
	checkResult(t, evalOne(t, &other, env, `{name: o, type: objects, expect: {jobsNotComplete: "== 0"}}`)["o.jobsNotComplete"], pass, 0)
}

// twinPower compares each twin sample after the warmup with the node's
// energy rate over the rate window: intel-0 reads 5 % high, nvl-0 exactly.
func TestAssertTwinPower(t *testing.T) {
	rd, env := fxRunData(t)
	got := evalOne(t, rd, env, `{name: w, type: twinPower, tolerance: 0.02, expect: "<= 0.05"}`)
	checkResult(t, got["w"], pass, 0.05)
	if !strings.Contains(got["w"].Message, "4 samples") || !strings.Contains(got["w"].Message, "2 above the tolerance") {
		t.Errorf("message %q", got["w"].Message)
	}
	checkResult(t, evalOne(t, rd, env, `{name: w, type: twinPower, nodes: [nvl-0], expect: "== 0"}`)["w"], pass, 0)
	checkResult(t, evalOne(t, rd, env, `{name: w, type: twinPower, quantile: 0.5, expect: "<= 0.05"}`)["w"], pass, 0.025)
	// The 60 s sample counts with a 1 m warmup.
	got = evalOne(t, rd, env, `{name: w, type: twinPower, warmup: 1m, expect: "<= 0.05"}`)
	if !strings.Contains(got["w"].Message, "6 samples") {
		t.Errorf("message %q, want 6 samples", got["w"].Message)
	}
	checkResult(t, evalOne(t, rd, env, `{name: w, type: twinPower, warmup: 10m, expect: "<= 1"}`)["w"], failed, noData)
}

// promql reads the value the runner recorded at the end of the run.
func TestAssertPromQL(t *testing.T) {
	rd, env := fxRunData(t)
	checkResult(t, evalOne(t, rd, env, `{name: targets, type: promql, query: count(up), expect: "== 3"}`)["targets"], pass, 3)
	checkResult(t, evalOne(t, rd, env, `{name: other, type: promql, query: count(up), expect: ">= 0"}`)["other"], failed, noData)
}

// With no records at all, every check of every type fails with no data,
// even one whose comparator would hold for zero: a missing file never
// passes as "nothing went wrong".
func TestAssertNoDataFails(t *testing.T) {
	rd, env := fxRunData(t)
	empty := &scenario.RunData{Dir: t.TempDir(), Scenario: rd.Scenario, RunID: rd.RunID}
	empty.Scenario.Assertions = append(empty.Scenario.Assertions, mustAssertion(t,
		`{name: zero-ok, type: stall, expect: {runStalled: "== 0"}}`), mustAssertion(t,
		`{name: none-ok, type: events, event: joulie.sim.job.failed, expect: "== 0"}`), mustAssertion(t,
		`{name: clean-ok, type: objects, expect: {strayPodsOnFakeNodes: "== 0"}}`))
	results := scenario.Evaluate(empty, env)
	if len(results) != 21 {
		t.Errorf("%d results, want 21", len(results))
	}
	types := map[scenario.AssertionType]bool{}
	for _, r := range results {
		types[r.Type] = true
		if r.Status != failed || r.Observed != nil || !strings.HasPrefix(r.Message, scenario.NoData) {
			t.Errorf("%s (%s): %s, observed %v, message %q; want fail with no data", r.Name, r.Type, r.Status, r.Observed, r.Message)
		}
	}
	if len(types) != len(scenario.AssertionTypes()) {
		t.Errorf("covered %d types, want all %d", len(types), len(scenario.AssertionTypes()))
	}
}

func mustAssertion(t *testing.T, doc string) scenario.Assertion {
	t.Helper()
	var a scenario.Assertion
	if err := yaml.UnmarshalStrict([]byte(doc), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// The directory reads into RunData: the scenario with defaults, the run's
// bounds, every record of the four OTLP files and both snapshots.
func TestReadRunData(t *testing.T) {
	rd, env := fxRunData(t)
	if rd.RunID != env.RunID || rd.Variant != "" || !rd.Start.Equal(fxStart) || !rd.End.Equal(fxAt(200)) || rd.Status == nil {
		t.Errorf("run %q %q %s..%s status %v", rd.RunID, rd.Variant, rd.Start, rd.End, rd.Status)
	}
	files := map[string]int{}
	for _, e := range rd.Events {
		files[e.File]++
	}
	for _, p := range rd.Points {
		files[p.File]++
	}
	if files["otlp/simulator.logs.jsonl"] != 19 || files["otlp/harness.logs.jsonl"] != 1 ||
		files["otlp/simulator.metrics.jsonl"] != 17 || files["otlp/harness.metrics.jsonl"] != 7 {
		t.Errorf("records per file %v", files)
	}
	for i := 1; i < len(rd.Events); i++ {
		if rd.Events[i].Time.Before(rd.Events[i-1].Time) {
			t.Fatalf("events are not ordered by time at %d", i)
		}
	}
	p := rd.Points[0]
	if p.Kind != semconv.Counter || p.Unit != "J" || !p.StartTime.Equal(fxStart) {
		t.Errorf("first point %+v", p)
	}
	if len(rd.KubeStart.Nodes) != 2 || len(rd.KubeStart.Pods) != 0 || len(rd.KubeEnd.Pods) != 2 || !rd.KubeEnd.Time.Equal(fxAt(200)) {
		t.Errorf("snapshots: start %d nodes %d pods, end %d pods at %s", len(rd.KubeStart.Nodes), len(rd.KubeStart.Pods), len(rd.KubeEnd.Pods), rd.KubeEnd.Time)
	}
	if rd.Scenario.Assertions[0].Severity != scenario.SeverityFail {
		t.Errorf("the effective scenario lost its defaults")
	}
}

// A jobs filter that selects no due job has no data for every count, so a
// misspelt class cannot pass "failed == 0".
func TestAssertJobsFilterMatchingNothingHasNoData(t *testing.T) {
	rd, env := fxRunData(t)
	got := evalOne(t, rd, env, `{name: j, type: jobs, workload: nope, expect: {failed: "== 0", completed: ">= 0"}}`)
	checkResult(t, got["j.failed"], failed, noData)
	checkResult(t, got["j.completed"], failed, noData)
}

// fxReadWithout writes the fixture into a temporary directory, removes the
// files rel names and reads what is left.
func fxReadWithout(t *testing.T, rel ...string) *scenario.RunData {
	t.Helper()
	dir := t.TempDir()
	writeFixture(t, dir)
	for _, r := range rel {
		if err := os.Remove(filepath.Join(dir, r)); err != nil {
			t.Fatal(err)
		}
	}
	rd, _, err := scenario.ReadRunData(dir)
	if err != nil {
		t.Fatal(err)
	}
	return rd
}

// The harness writes its logs whatever happens to the simulator's, so a
// count of joulie.sim.* records without the simulator's logs (a failed
// copy-out) is no data, not a pass of "== 0"; a count of joulie.scenario.*
// records without the harness's logs is no data the same way. The jobs
// counts, built from the trace, need the simulator's logs too: without
// them every due job would read incomplete and none failed.
func TestAssertEventsMissingLogsHaveNoData(t *testing.T) {
	_, env := fxRunData(t)
	noSim := fxReadWithout(t, filepath.Join(workload.OTLPDir, workload.SimulatorLogsFile))
	checkResult(t, evalOne(t, noSim, env, `{name: e, type: events, event: joulie.sim.job.failed, expect: "== 0"}`)["e"], failed, noData)
	checkResult(t, evalOne(t, noSim, env, `{name: e, type: events, event: joulie.scenario.started, expect: "== 1"}`)["e"], pass, 1)
	checkResult(t, evalOne(t, noSim, env, `{name: j, type: jobs, expect: {failed: "== 0"}}`)["j.failed"], failed, noData)
	noHarness := fxReadWithout(t, filepath.Join(workload.OTLPDir, scenario.HarnessLogsFile))
	checkResult(t, evalOne(t, noHarness, env, `{name: e, type: events, event: joulie.scenario.variant.started, expect: "== 0"}`)["e"], failed, noData)
	checkResult(t, evalOne(t, noHarness, env, `{name: e, type: events, event: joulie.sim.job.completed, expect: "== 3"}`)["e"], pass, 3)
}

// A missing end file is no data for the checks that read its kind, while
// the other checks still count: without pods-end.yaml no pod is stray,
// which must not pass "== 0".
func TestAssertObjectsMissingKindHasNoData(t *testing.T) {
	_, env := fxRunData(t)
	full := map[string]float64{"jobsNotComplete": 1, "strayPodsOnFakeNodes": 1, "nodeHardwares": 2, "controlBlocked": 1}
	for _, c := range []struct {
		kind   string
		noData []string
	}{
		{scenario.KindPods, []string{"strayPodsOnFakeNodes"}},
		{scenario.KindNodes, []string{"strayPodsOnFakeNodes"}},
		{scenario.KindJobs, []string{"jobsNotComplete"}},
		{scenario.KindNodeHardwares, []string{"nodeHardwares"}},
		{scenario.KindNodeTwins, []string{"controlBlocked"}},
	} {
		rd := fxReadWithout(t, scenario.KubeSnapshotFile(c.kind, scenario.SnapshotEnd))
		got := evalOne(t, rd, env, `{name: o, type: objects, expect: {jobsNotComplete: ">= 0", strayPodsOnFakeNodes: "== 0", nodeHardwares: ">= 0", controlBlocked: ">= 0"}}`)
		for key, want := range full {
			r := got["o."+key]
			switch {
			case contains(c.noData, key):
				checkResult(t, r, failed, noData)
			case r.Observed == nil || *r.Observed != want:
				t.Errorf("without %s: %s observed %v (%s), want %g", c.kind, r.Name, r.Observed, r.Message, want)
			}
		}
	}
}

// stripAttributes returns a copy of rd whose events lack keys; only lacks
// limits it to the events of that job.
func stripAttributes(rd *scenario.RunData, only string, keys ...string) *scenario.RunData {
	c := *rd
	c.Events = make([]scenario.Event, len(rd.Events))
	for i, e := range rd.Events {
		if id, _ := e.Attributes.String(semconv.AttrSimJobID); only == "" || id == only {
			attrs := scenario.Attributes{}
			for k, v := range e.Attributes {
				if !contains(keys, k) {
					attrs[k] = v
				}
			}
			e.Attributes = attrs
		}
		c.Events[i] = e
	}
	return &c
}

// A bind on a forbidden class without joulie.node.class_since is a
// violation: only a bind known to be recent is the cache's lag. A bind
// with no node class cannot be judged; it is reported, and with no bind
// judged there is no data.
func TestPlacementWithoutClassAttributes(t *testing.T) {
	rd, env := fxRunData(t)
	noSince := stripAttributes(rd, "", semconv.AttrJoulieNodeClassSince)
	got := evalOne(t, noSince, env, `{name: p, type: placement, class: standard, forbidden: [eco], expect: "== 0"}`)
	checkResult(t, got["p"], failed, 2)
	if !strings.Contains(got["p"].Message, "2 without "+semconv.AttrJoulieNodeClassSince) {
		t.Errorf("message %q does not report the binds without class_since", got["p"].Message)
	}
	got = evalOne(t, noSince, env, `{name: p, type: placement, class: performance, forbidden: [eco], expect: "== 0"}`)
	checkResult(t, got["p"], failed, 1)
	noClass := stripAttributes(rd, "", semconv.AttrJoulieNodeSchedulableClass, semconv.AttrJoulieNodePowerProfile)
	checkResult(t, evalOne(t, noClass, env, `{name: p, type: placement, class: standard, forbidden: [eco], expect: "== 0"}`)["p"], failed, noData)
	oneNoClass := stripAttributes(rd, "std-0-0", semconv.AttrJoulieNodeSchedulableClass, semconv.AttrJoulieNodePowerProfile)
	got = evalOne(t, oneNoClass, env, `{name: p, type: placement, class: standard, forbidden: [eco], expect: "== 1"}`)
	checkResult(t, got["p"], pass, 1)
	if !strings.Contains(got["p"].Message, "1 without a node class") {
		t.Errorf("message %q does not report the bind without a class", got["p"].Message)
	}
}

func fxEnergyPoint(node string, start, at, joules float64) scenario.Point {
	return scenario.Point{Metric: semconv.JoulieSimWorkloadEnergy.Name, Unit: "J", Kind: semconv.Counter, StartTime: fxAt(start), Time: fxAt(at), Value: joules,
		Attributes: scenario.Attributes{semconv.AttrK8sNodeName: node, semconv.AttrJoulieWorkloadClass: "standard", semconv.AttrHwType: semconv.HwTypeCPU}}
}

func fxTwinPoint(node string, at, watts float64) scenario.Point {
	return scenario.Point{Metric: semconv.JoulieCaptureNodeTwinMeasuredPower.Name, Unit: "W", Kind: semconv.Gauge, Time: fxAt(at), Value: watts,
		Attributes: scenario.Attributes{semconv.AttrK8sNodeName: node}}
}

// The node's energy rate sums each series' change over the window. A
// series that starts mid-run reads 0 only for itself, and a capture between
// the simulator's snapshots reads the others' values between theirs, so
// intel-0 still reads 5 % high. Power that rises over the run makes the
// error depend on the window: step-0 draws 100 W, then 300 W from 120 s.
func TestAssertTwinPowerUnalignedSeries(t *testing.T) {
	rd, env := fxRunData(t)
	c := *rd
	c.Points = append(append([]scenario.Point(nil), rd.Points...),
		fxEnergyPoint("intel-0", 125, 200, 1), fxTwinPoint("intel-0", 190, 210),
		fxEnergyPoint("step-0", 0, 60, 6000), fxEnergyPoint("step-0", 0, 120, 12000), fxEnergyPoint("step-0", 0, 180, 30000),
		fxTwinPoint("step-0", 180, 300))
	got := evalOne(t, &c, env, `{name: w, type: twinPower, nodes: [intel-0], quantile: 1, expect: "<= 0.06"}`)
	checkResult(t, got["w"], pass, 0.05)
	if !strings.Contains(got["w"].Message, "3 samples") {
		t.Errorf("message %q, want 3 samples", got["w"].Message)
	}
	// The 190 s sample alone: 36000.73 J at 180 s less 25000 J at 125 s.
	got = evalOne(t, &c, env, `{name: w, type: twinPower, nodes: [intel-0], warmup: 185s, expect: "<= 0.06"}`)
	checkResult(t, got["w"], pass, 210/(11000+55.0/75)*55-1)
	// Over the 1 m window ending at 180 s step-0 drew 300 W; over 2 m it
	// would read 200 W, an error of 0.5.
	checkResult(t, evalOne(t, &c, env, `{name: w, type: twinPower, nodes: [step-0], expect: "== 0"}`)["w"], pass, 0)
}

// A job is due only when its offset is before duration: the trace may
// hold later records (a generated trace is cut by FilterTrace, an edited
// one need not be), and a run still draining past duration never submits
// them. Here the run went on to 300 s, so late-1-0 (210 s) is due and
// incomplete, while the record at 240 s, the duration, is not due.
func TestAssertJobsDueBeforeDurationOnly(t *testing.T) {
	_, env := fxRunData(t)
	dir := t.TempDir()
	writeFixture(t, dir)
	tracePath := filepath.Join(dir, workload.TraceFile)
	b, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatal(err)
	}
	rec["jobId"], rec["workloadId"], rec["submitTimeOffsetSec"] = "late-2-0", "late-2", 240
	extra, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracePath, append(append(b, extra...), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(dir, scenario.RunStatusFile)
	st, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath, []byte(strings.Replace(string(st), `"elapsedSeconds": 200`, `"elapsedSeconds": 300`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	rd, _, err := scenario.ReadRunData(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := evalOne(t, rd, env, `{name: j, type: jobs, expect: {incomplete: "== 2", completed: "== 3"}}`)
	checkResult(t, got["j.incomplete"], pass, 2)
	checkResult(t, got["j.completed"], pass, 3)
	if !strings.Contains(got["j.incomplete"].Message, "6 due jobs (1 not due)") {
		t.Errorf("message %q, want 6 due jobs and the 240 s record not due", got["j.incomplete"].Message)
	}
}
