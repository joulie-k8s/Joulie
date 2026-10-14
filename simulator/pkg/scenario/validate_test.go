package scenario_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/matbun/joulie/simulator/pkg/scenario"
)

// repoRoot is the mini repository the planner tests read: a cluster spec,
// the dev timing, values files and traces.
const repoRoot = "testdata/repo"

// validScenario passes every rule against repoRoot as valid.yaml.
const validScenario = `
schemaVersion: 1
name: valid
cluster: {spec: tests/simcluster/specs/mini.yaml, timing: dev}
duration: 10m
workload:
  jobs:
  - {name: cpu, class: performance, count: 3, every: 60s, requests: {cpu: "4"}, runtime: 2m}
  - {name: gpu, class: standard, requests: {cpu: "2"}, gpus: {resource: nvidia.com/gpu, count: 2}, runtime: 5m}
joulie: {set: {controllerManager.env.POLICY_TYPE: queue_aware_v1}, values: [values/policy.yaml]}
assertions:
- {name: done, type: jobs, expect: {completedFraction: ">= 0.9"}}
`

var validOpts = scenario.ValidateOptions{File: "tests/simcluster/scenarios/valid.yaml", RepoRoot: repoRoot}

func writeScenario(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The scenario file is decoded strictly: an unknown key at the top, inside
// the workload, inside a job or inside an assertion fails, naming it.
func TestStrictDecodeRejectsUnknownField(t *testing.T) {
	if _, err := scenario.ReadScenario(writeScenario(t, validScenario)); err != nil {
		t.Fatalf("the valid scenario does not decode: %v", err)
	}
	for _, c := range []struct{ from, to, want string }{
		{"duration: 10m", "duration: 10m\ndurations: 5m", `unknown field "durations"`},
		{"workload:", "workload:\n  jobz: []", `unknown field "jobz"`},
		{"runtime: 2m}", "runtime: 2m, cpus: 4}", `unknown field "cpus"`},
		{`expect: {completedFraction: ">= 0.9"}}`, `expect: {completedFraction: ">= 0.9"}, forbidden: [eco]}`, `unknown field "forbidden"`},
		{`expect: {completedFraction: ">= 0.9"}}`, `expect: {completed_fraction: ">= 0.9"}}`, `unknown field "completed_fraction"`},
		{"cluster: {spec:", "cluster: {timeing: dev, spec:", `unknown field "timeing"`},
	} {
		doc := strings.Replace(validScenario, c.from, c.to, 1)
		if doc == validScenario {
			t.Fatalf("case %q does not apply", c.from)
		}
		_, err := scenario.ReadScenario(writeScenario(t, doc))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want one containing %s", c.to, err, c.want)
		}
	}
}

func validBase(t *testing.T) scenario.Scenario {
	t.Helper()
	s := fxDecode(t, validScenario)
	if err := scenario.Validate(s, validOpts); err != nil {
		t.Fatalf("the valid scenario fails: %v", err)
	}
	return s
}

// Joulie's node power and facility inputs are reached only through
// joulie.inputPreset: a NODE_POWER_* or FACILITY_* key is refused in the
// top-level and the variant settings and inside a values file, and the
// custom preset, which needs a query, is refused too.
func TestValidateRejectsPowerInputOverrides(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*scenario.Scenario)
		want   string
	}{
		{"query", func(s *scenario.Scenario) {
			s.Joulie.Set["controllerManager.env.NODE_POWER_PROMETHEUS_QUERY"] = "sum(x)"
		}, "joulie.set.controllerManager.env.NODE_POWER_PROMETHEUS_QUERY"},
		{"facility in a variant", func(s *scenario.Scenario) {
			s.Variants = []scenario.Variant{{Name: "a", Joulie: &scenario.Joulie{Set: scenario.ScalarMap{"controllerManager.env.FACILITY_IT_POWER_QUERY": "x"}}}}
		}, "variants a: joulie.set.controllerManager.env.FACILITY_IT_POWER_QUERY"},
		{"values file", func(s *scenario.Scenario) {
			s.Joulie.Values = append(s.Joulie.Values, "values/power-input.yaml")
		}, "values/power-input.yaml: controllerManager.env.NODE_POWER_SOURCE"},
		{"custom preset", func(s *scenario.Scenario) { s.Joulie.InputPreset = "custom" }, `inputPreset "custom"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := validBase(t)
			c.mutate(&s)
			err := scenario.Validate(s, validOpts)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "inputPreset") {
				t.Errorf("error %v, want one naming %s and pointing at inputPreset", err, c.want)
			}
		})
	}
	for _, preset := range []string{"", "otel", "kepler", "kepler-legacy"} {
		s := validBase(t)
		s.Joulie.InputPreset = preset
		s.Joulie.Set["controllerManager.env.ENABLE_FACILITY_METRICS"] = "true"
		if err := scenario.Validate(s, validOpts); err != nil {
			t.Errorf("preset %q with ENABLE_FACILITY_METRICS: %v", preset, err)
		}
	}
}

// Each rule of the scenario format refuses what it names.
func TestValidateRules(t *testing.T) {
	dur := func(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }
	for _, c := range []struct {
		name   string
		mutate func(*scenario.Scenario)
		want   string
	}{
		{"stem", func(s *scenario.Scenario) { s.Name = "other" }, `differs from the file stem "valid"`},
		{"dns name", func(s *scenario.Scenario) { s.Name = "Valid_1" }, "not a DNS-1123 label"},
		{"yaml boolean name", func(s *scenario.Scenario) {
			s.Variants = []scenario.Variant{fxDecode(t, "variants: [{name: off}]").Variants[0]}
		}, `name "false" is a YAML boolean`},
		{"duration", func(s *scenario.Scenario) { s.Duration.Duration = 7 * time.Hour }, "duration 7h0m0s is not in (0, 6h0m0s]"},
		{"spec missing", func(s *scenario.Scenario) { s.Cluster.Spec = "tests/simcluster/specs/nope.yaml" }, "nope.yaml"},
		{"timing", func(s *scenario.Scenario) { s.Cluster.Timing = "fast" }, `cluster.timing "fast"`},
		{"two sources", func(s *scenario.Scenario) { s.Workload.Trace = "traces/v3.jsonl" }, "2 sources"},
		{"no source", func(s *scenario.Scenario) { s.Workload.Jobs = nil }, "0 sources"},
		{"v2 trace", func(s *scenario.Scenario) { s.Workload.Jobs, s.Workload.Trace = nil, "traces/v2.jsonl" }, `schemaVersion "v2", want v3`},
		{"trace resource", func(s *scenario.Scenario) { s.Workload.Jobs, s.Workload.Trace = nil, "traces/v3-amd.jsonl" },
			"workload.trace: no pool of tests/simcluster/specs/mini.yaml has amd.com/gpu (jobs a0)"},
		{"schema-version", func(s *scenario.Scenario) {
			s.Workload.Jobs, s.Workload.Generate = nil, &scenario.Generate{Workloadgen: scenario.ScalarMap{"schema-version": "v2"}}
		}, "schema is always v3"},
		{"runner flag", func(s *scenario.Scenario) {
			s.Workload.Jobs, s.Workload.Generate = nil, &scenario.Generate{Workloadgen: scenario.ScalarMap{"seed": "7"}}
		}, "the runner sets it"},
		{"unknown flag", func(s *scenario.Scenario) {
			s.Workload.Jobs, s.Workload.Generate = nil, &scenario.Generate{Workloadgen: scenario.ScalarMap{"bogus": "1"}}
		}, "bogus is not an allowed workloadgen flag"},
		{"refused flag", func(s *scenario.Scenario) {
			s.Workload.Jobs, s.Workload.Generate = nil, &scenario.Generate{Workloadgen: scenario.ScalarMap{"no-affinity-only": "true"}}
		}, "no power-profile affinity"},
		{"too many variants", func(s *scenario.Scenario) {
			for _, n := range []string{"a", "b", "c", "d", "e"} {
				s.Variants = append(s.Variants, scenario.Variant{Name: n})
			}
		}, "5 variants, at most 4"},
		{"reuse", func(s *scenario.Scenario) {
			s.Isolation = scenario.IsolationReuse
			s.Variants = []scenario.Variant{{Name: "a"}, {Name: "b"}}
		}, "isolation reuse with 2 variants"},
		{"missing resource", func(s *scenario.Scenario) { s.Workload.Jobs[1].GPUs.Resource = "amd.com/gpu" }, "no pool of tests/simcluster/specs/mini.yaml has amd.com/gpu"},
		{"gpu in requests", func(s *scenario.Scenario) {
			s.Workload.Jobs[1].Requests["nvidia.com/gpu"] = s.Workload.Jobs[1].Requests["cpu"]
		}, "requests.nvidia.com/gpu"},
		{"group after duration", func(s *scenario.Scenario) { s.Workload.Jobs[0].Every = dur(5 * time.Minute) }, "never be submitted"},
		{"profile range", func(s *scenario.Scenario) { v := 1.5; s.Workload.Jobs[0].Profile.CPUUtilization = &v }, "profile.cpuUtilization 1.5"},
		{"comparator", func(s *scenario.Scenario) { s.Assertions[0].Jobs.Expect.CompletedFraction = "about 1" }, `"about 1" does not match`},
		{"unit", func(s *scenario.Scenario) { s.Assertions[0].Jobs.Expect.CompletedFraction = ">= 1 J" }, "has unit J"},
		{"duplicate assertion", func(s *scenario.Scenario) { s.Assertions = append(s.Assertions, s.Assertions[0]) }, "a second assertion"},
		{"event attribute", func(s *scenario.Scenario) {
			s.Assertions = append(s.Assertions, scenario.Assertion{Name: "e", Type: scenario.AssertEvents, Events: &scenario.EventsAssertion{
				Event: "joulie.sim.job.completed", Where: scenario.ScalarMap{"joulie.sim.gang": "true"}, Expect: "== 1"}})
		}, "joulie.sim.job.completed records never carry it"},
		{"unknown event", func(s *scenario.Scenario) {
			s.Assertions = append(s.Assertions, scenario.Assertion{Name: "e", Type: scenario.AssertEvents, Events: &scenario.EventsAssertion{Event: "job.done", Expect: "== 1"}})
		}, `event "job.done" is not a registered event`},
		{"evaluation event", func(s *scenario.Scenario) {
			s.Assertions = append(s.Assertions, scenario.Assertion{Name: "e", Type: scenario.AssertEvents, Events: &scenario.EventsAssertion{
				Event: "joulie.scenario.finished", Expect: "== 1"}})
		}, "is written by evaluate after the assertions"},
		{"quantile", func(s *scenario.Scenario) {
			s.Assertions = append(s.Assertions, scenario.Assertion{Name: "l", Type: scenario.AssertLatency, Latency: &scenario.LatencyAssertion{
				Metric: scenario.LatencyDuration, Quantile: "0.3", Expect: "< 1h"}})
		}, `quantile "0.3"`},
		{"energy scope", func(s *scenario.Scenario) {
			s.Assertions = append(s.Assertions, scenario.Assertion{Name: "en", Type: scenario.AssertEnergy, Energy: &scenario.EnergyAssertion{Scope: "rack:r1", Expect: "> 0"}})
		}, `energy scope "rack:r1"`},
		{"capture prefix", func(s *scenario.Scenario) {
			s.Capture.Prometheus = []scenario.PromCapture{{Name: "node.power", Query: "x"}}
		}, "must start with joulie.capture."},
		{"compare without variants", func(s *scenario.Scenario) {
			s.Compare = []scenario.Compare{{Name: "c", Metric: "energy.total", Variant: "a", Reference: "b", Expect: "<= 1"}}
		}, "lists no variants"},
		{"compare metric", func(s *scenario.Scenario) {
			s.Variants = []scenario.Variant{{Name: "a"}, {Name: "b"}}
			s.Compare = []scenario.Compare{{Name: "c", Metric: "class.performance.runTime.p95", Variant: "a", Reference: "b", Expect: "<= 1"}}
		}, `metric "class.performance.runTime.p95"`},
		{"helm value line break", func(s *scenario.Scenario) { s.Simulator.Set = scenario.ScalarMap{"a": "x\ny"} }, "line break"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := validBase(t)
			c.mutate(&s)
			err := scenario.Validate(s, validOpts)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want one containing %s", err, c.want)
			}
		})
	}
	// A v3 trace whose GPU resource the spec has is valid.
	s := validBase(t)
	s.Workload.Jobs, s.Workload.Trace = nil, "traces/v3.jsonl"
	if err := scenario.Validate(s, validOpts); err != nil {
		t.Errorf("a v3 trace of nvidia.com/gpu jobs: %v", err)
	}
}
