package scenario_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/simulator/pkg/scenario"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// schemaExample uses every top-level key and every assertion type.
const schemaExample = `
schemaVersion: 1
name: smoke
description: Job-owned pods complete through KWOK and the run writes the results contract.
cluster: {spec: tests/simcluster/specs/smoke.yaml, timing: dev}
seed: 1
duration: 4m
drain: 3m
abortOnRunStalled: 2m
isolation: cluster
workload:
  namespace: sim-workload
  owner: job
  intent: annotation
  completion: kwok
  jobTTL: 10m
  baseSpeedPerCore: 1
  stallReport: 60s
  placement: {nodeSelector: {}, tolerations: [{key: kwok.x-k8s.io/node, operator: Equal, value: fake, effect: NoSchedule}]}
  jobs:
  - name: perf-cpu
    class: performance
    count: 3
    every: 10s
    pods: 2
    gang: true
    requests: {cpu: "4", memory: 4Gi}
    gpus: {resource: nvidia.com/gpu, count: 1}
    runtime: 90s
    workClass: {cpu: cpu.compute_bound}
    profile: {cpuUtilization: 0.9, gpuUtilization: 0.6, memoryIntensity: 0.2, ioIntensity: 0.05, cpuFeedIntensityGpu: 0}
    sensitivity: {cpu: 1}
    nodeSelector: {kubernetes.io/hostname: nvl-0}
joulie: {enabled: true, inputPreset: "", set: {controllerManager.env.POLICY_TYPE: queue_aware_v1, controllerManager.env.STATIC_HP_FRAC: 1}}
simulator: {set: {emulate.exporters: "{kepler-0.12}"}}
variants:
- {name: queue-aware, joulie: {set: {controllerManager.env.POLICY_TYPE: queue_aware_v1}}}
- {name: joulie-off, joulie: {enabled: false}}
capture:
  interval: 15s
  prometheus: [{name: joulie.capture.node.component_power, unit: W, query: 'sum(hw_power_watts)'}]
assertions:
- {name: all-complete, type: jobs, expect: {completedFraction: ">= 1", failed: "== 0"}}
- {name: slow-on-eco, type: latency, workload: perf-cpu, nodeProfile: eco, metric: slowdown, quantile: 0.5, reference: {nodeProfile: performance}, expect: "> 1.05", severity: warn}
- {name: no-stall, type: stall, expect: {runStalled: "== 0"}}
- {name: isolation, type: placement, class: performance, forbidden: [eco, draining], graceSeconds: 40, expect: "== 0"}
- {name: energy, type: energy, scope: total, hwType: all, perJob: false, expect: "> 0 J"}
- {name: completed-events, type: events, event: joulie.sim.job.completed, where: {joulie.workload.class: performance, joulie.sim.gang: true}, expect: "== 6"}
- {name: clean, type: objects, expect: {jobsNotComplete: "== 0", strayPodsOnFakeNodes: "== 0"}}
- {name: twin, type: twinPower, warmup: 2m, tolerance: 0.15, quantile: 0.95, nodes: [nvl-0], expect: "<= 0.15"}
- {name: targets, type: promql, query: 'count(up)', at: end, expect: ">= 1"}
compare:
- {name: energy-ratio, metric: energy.total, variant: joulie-off, reference: queue-aware, expect: "<= 1.0", severity: warn}
`

func decodeScenario(t *testing.T, doc string) (scenario.Scenario, error) {
	t.Helper()
	var s scenario.Scenario
	err := yaml.UnmarshalStrict([]byte(doc), &s)
	return s, err
}

// A file with every key decodes strictly; each assertion lands in the
// struct its type selects, with the other pointers nil.
func TestSchemaDecodesEveryKeyStrictly(t *testing.T) {
	s, err := decodeScenario(t, schemaExample)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Name != "smoke" || s.Duration.Duration != 4*time.Minute || s.AbortOnRunStalled.Duration != 2*time.Minute ||
		s.Workload.Owner != workload.OwnerJob || s.Workload.StallReport.Duration != time.Minute {
		t.Errorf("top level decoded as %+v", s)
	}
	j := s.Workload.Jobs[0]
	if j.Requests.Cpu().String() != "4" || j.Requests.Memory().String() != "4Gi" || j.GPUs.Count != 1 ||
		j.Runtime.Duration != 90*time.Second || *j.Profile.CPUFeedIntensityGPU != 0 || *j.Sensitivity.CPU != 1 {
		t.Errorf("job decoded as %+v", j)
	}
	if got := s.Joulie.Set["controllerManager.env.STATIC_HP_FRAC"]; got != "1" {
		t.Errorf("a numeric Helm value decoded as %q, want its text 1", got)
	}
	if v := s.Variants[1]; v.Joulie == nil || *v.Joulie.Enabled || v.Simulator != nil {
		t.Errorf("variant decoded as %+v", v)
	}
	types := []scenario.AssertionType{}
	for _, a := range s.Assertions {
		types = append(types, a.Type)
		set := 0
		for _, p := range []any{a.Jobs, a.Latency, a.Stall, a.Placement, a.Energy, a.Events, a.Objects, a.TwinPower, a.PromQL} {
			if !reflect.ValueOf(p).IsNil() {
				set++
			}
		}
		if set != 1 || a.Spec() == nil {
			t.Errorf("assertion %s: %d type structs set, want 1", a.Name, set)
		}
	}
	if want := scenario.AssertionTypes(); len(types) != len(want) {
		t.Errorf("example covers %v, want every type %v", types, want)
	}
	lat := s.Assertions[1].Latency
	if lat.Workload != "perf-cpu" || lat.NodeProfile != "eco" || lat.Metric != scenario.LatencySlowdown || lat.Quantile != "0.5" ||
		lat.Reference.NodeProfile != "performance" || s.Assertions[1].Severity != scenario.SeverityWarn {
		t.Errorf("latency decoded as %+v", s.Assertions[1])
	}
	if got := s.Assertions[5].Events.Where["joulie.sim.gang"]; got != "true" {
		t.Errorf("a boolean where value decoded as %q, want its text true", got)
	}
	if s.Compare[0].Variant != "joulie-off" || s.Compare[0].Reference != "queue-aware" || s.Compare[0].Expect != "<= 1.0" {
		t.Errorf("compare decoded as %+v", s.Compare[0])
	}
}

// Strictness reaches inside an assertion: a key of another type, an unknown
// type, a missing type and the top-level compare used as a type all fail.
func TestSchemaAssertionDecodeIsStrictPerType(t *testing.T) {
	for _, c := range []struct{ assertion, want string }{
		{`{name: a, type: jobs, forbidden: [eco], expect: {completed: "== 1"}}`, `unknown field "forbidden"`},
		{`{name: a, type: placement, class: performance, forbidden: [eco], expect: "== 0", grace: 1}`, `unknown field "grace"`},
		{`{name: a, type: bogus, expect: "== 0"}`, `unknown type "bogus"`},
		{`{name: a, type: compare, metric: energy.total, expect: "<= 1"}`, `unknown type "compare"`},
		{`{name: a, expect: "== 0"}`, `no type`},
		{`{name: a, type: stall, expect: {runStalled: "== 0", deadlock: "== 0"}}`, `unknown field "deadlock"`},
	} {
		doc := "schemaVersion: 1\nname: x\nassertions:\n- " + c.assertion + "\n"
		_, err := decodeScenario(t, doc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want one containing %q", c.assertion, err, c.want)
		}
	}
	if _, err := decodeScenario(t, "schemaVersion: 1\nname: x\nworkload: {jobz: []}\n"); err == nil {
		t.Error("an unknown workload key decoded")
	}
}

// An assertion marshals back to the flat object it was read from, so
// scenario.effective.yaml decodes to the same scenario.
func TestSchemaRoundTrip(t *testing.T) {
	s, err := decodeScenario(t, schemaExample)
	if err != nil {
		t.Fatal(err)
	}
	b, err := yaml.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	again, err := decodeScenario(t, string(b))
	if err != nil {
		t.Fatalf("re-decode of\n%s\n%v", b, err)
	}
	if !reflect.DeepEqual(again, s) {
		t.Errorf("round trip changed the scenario:\n%s", b)
	}
	if ns := again.Workload.Placement.NodeSelector; ns == nil || len(ns) != 0 {
		t.Errorf("an empty nodeSelector, which clears the default, came back as %#v", ns)
	}
	// Empty clears a default and absent keeps it, for both placement fields,
	// so neither may be omitted when scenario.effective.yaml is written.
	for _, p := range []string{`{nodeSelector: {}, tolerations: []}`, `{}`, `{nodeSelector: {a: b}}`} {
		var in scenario.Scenario
		if err := yaml.UnmarshalStrict([]byte("workload: {placement: "+p+"}"), &in); err != nil {
			t.Fatal(err)
		}
		b, err := yaml.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var back scenario.Scenario
		if err := yaml.UnmarshalStrict(b, &back); err != nil {
			t.Fatal(err)
		}
		want, got := in.Workload.Placement, back.Workload.Placement
		if (got.NodeSelector == nil) != (want.NodeSelector == nil) || (got.Tolerations == nil) != (want.Tolerations == nil) {
			t.Errorf("placement %s written as\n%s reads back as %#v", p, b, got)
		}
	}
	flat, err := json.Marshal(s.Assertions[1])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(flat, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"name": "slow-on-eco", "type": "latency", "severity": "warn", "workload": "perf-cpu",
		"nodeProfile": "eco", "metric": "slowdown", "quantile": "0.5", "reference": map[string]any{"nodeProfile": "performance"},
		"expect": "> 1.05"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("latency assertion marshals as %s, want the flat object %v", flat, want)
	}
}

// A Scalar reaches the command line as the YAML decoder reads it: a quoted
// value keeps its text, and an unquoted number or boolean is read by YAML
// 1.1 rules and written in its shortest float64 form, in every map of the
// scenario file. Anything else is refused.
func TestSchemaScalarKeepsText(t *testing.T) {
	const values = `{q1: "1.10", q2: "010", q3: "on", q4: "0.30", n1: 600, n2: 1.10, n3: 0.30, n4: 1e3, n5: 010, n6: on,
  n7: 1.0, n8: 0.1234567891234, n9: -1, b1: true, s1: queue_aware_v1}`
	want := map[string]scenario.Scalar{"q1": "1.10", "q2": "010", "q3": "on", "q4": "0.30",
		"n1": "600", "n2": "1.1", "n3": "0.3", "n4": "1000", "n5": "8", "n6": "true",
		"n7": "1", "n8": "0.1234567891234", "n9": "-1", "b1": "true", "s1": "queue_aware_v1"}
	doc := "joulie: {set: " + values + "}\nsimulator: {set: " + values + "}\nworkload: {generate: {workloadgen: " + values + "}}\n" +
		"variants: [{name: v, joulie: {set: " + values + "}, simulator: {set: " + values + "}}]\n" +
		"assertions: [{name: e, type: events, event: x, expect: \"== 1\", where: " + values + "}]\n"
	var s scenario.Scenario
	if err := yaml.UnmarshalStrict([]byte(doc), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for where, got := range map[string]scenario.ScalarMap{
		"joulie.set": s.Joulie.Set, "simulator.set": s.Simulator.Set, "workloadgen": s.Workload.Generate.Workloadgen,
		"variant joulie.set": s.Variants[0].Joulie.Set, "variant simulator.set": s.Variants[0].Simulator.Set,
		"events where": s.Assertions[0].Events.Where,
	} {
		if !reflect.DeepEqual(map[string]scenario.Scalar(got), want) {
			t.Errorf("%s decoded as %v, want %v", where, got, want)
		}
	}
	for _, bad := range []string{`{a: ~}`, `{a: [1]}`, `{a: {b: 1}}`} {
		var s scenario.Scenario
		if err := yaml.UnmarshalStrict([]byte("joulie: {set: "+bad+"}"), &s); err == nil {
			t.Errorf("set %s decoded as %v, want an error", bad, s.Joulie.Set)
		}
	}
	if b, _ := json.Marshal(scenario.Scalar("600")); string(b) != `"600"` {
		t.Errorf("Scalar marshals as %s, want a string", b)
	}
}

// Each comparator of an assertion is one check: an Expect that is one
// comparator is named after the assertion, and each set key of a jobs,
// stall or objects Expect is <assertion>.<key>, in declaration order.
func TestSchemaAssertionChecks(t *testing.T) {
	s, err := decodeScenario(t, schemaExample)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]scenario.AssertionCheck{}
	for _, a := range s.Assertions {
		got[a.Name] = a.Checks()
	}
	want := map[string][]scenario.AssertionCheck{
		"all-complete":     {{Name: "all-complete.completedFraction", Expect: ">= 1"}, {Name: "all-complete.failed", Expect: "== 0"}},
		"slow-on-eco":      {{Name: "slow-on-eco", Expect: "> 1.05"}},
		"no-stall":         {{Name: "no-stall.runStalled", Expect: "== 0"}},
		"isolation":        {{Name: "isolation", Expect: "== 0"}},
		"energy":           {{Name: "energy", Expect: "> 0 J"}},
		"completed-events": {{Name: "completed-events", Expect: "== 6"}},
		"clean":            {{Name: "clean.jobsNotComplete", Expect: "== 0"}, {Name: "clean.strayPodsOnFakeNodes", Expect: "== 0"}},
		"twin":             {{Name: "twin", Expect: "<= 0.15"}},
		"targets":          {{Name: "targets", Expect: ">= 1"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("checks\n%v\nwant\n%v", got, want)
	}
	if c := (scenario.Assertion{Name: "x"}).Checks(); c != nil {
		t.Errorf("an assertion with no type has checks %v", c)
	}
}
