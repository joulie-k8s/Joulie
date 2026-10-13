package scenario_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/scenario"
)

func jsonKeys(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return m
}

// summary.json is keyed by the semantic conventions' test.* attributes, and
// the keys the smoke test and jq read are always present.
func TestRunDataSummaryKeysFollowTestSemconv(t *testing.T) {
	observed := 0.97
	s := scenario.Summary{
		Schema: semconv.SimclusterSummarySchema, SuiteName: "weekly-compare", Status: semconv.TestSuiteRunStatusSuccess,
		Assertions: []scenario.AssertionResult{{Name: "all-complete.completedFraction", Status: semconv.TestCaseResultStatusPass,
			Type: scenario.AssertJobs, Severity: scenario.SeverityFail, Observed: &observed, Expected: ">= 0.95"}},
		Variants:         []scenario.VariantResult{{Name: "joulie-off", Status: semconv.TestSuiteRunStatusSuccess, Summary: "joulie-off/summary.json"}},
		Comparisons:      []scenario.ComparisonResult{{Name: "energy-ratio", Status: semconv.TestCaseResultStatusFail, Severity: scenario.SeverityWarn}},
		SemconvSchemaURL: semconv.SchemaURL,
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	top := jsonKeys(t, b)
	for _, k := range []string{"schema", semconv.AttrTestSuiteName, semconv.AttrTestSuiteRunStatus, "exitCode", "failures",
		"warnings", "knownDefects", "assertions", "git", "semconvSchemaUrl"} {
		if _, ok := top[k]; !ok {
			t.Errorf("summary %s has no %q", b, k)
		}
	}
	if string(top["schema"]) != `"joulie.simcluster.summary/v1"` {
		t.Errorf("schema %s", top["schema"])
	}
	for _, list := range []string{"assertions", "comparisons"} {
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(top[list], &items); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{semconv.AttrTestCaseName, semconv.AttrTestCaseResultStatus, "severity"} {
			if _, ok := items[0][k]; !ok {
				t.Errorf("%s item %v has no %q", list, items[0], k)
			}
		}
	}
	var variants []map[string]json.RawMessage
	if err := json.Unmarshal(top["variants"], &variants); err != nil {
		t.Fatal(err)
	}
	if _, ok := variants[0][semconv.AttrTestSuiteRunStatus]; !ok {
		t.Errorf("variant result %v has no %q", variants[0], semconv.AttrTestSuiteRunStatus)
	}
	// The scenario-only keys are absent from a smoke summary.
	for _, k := range []string{"jobs", "classes", "energy", "scenario", "runId", "startTime"} {
		if _, ok := top[k]; ok {
			t.Errorf("smoke summary %s has scenario key %q", b, k)
		}
	}
}

// The lists jq iterates are [] when nothing was recorded, never null, so
// .knownDefects[] works on a clean run.
func TestRunDataSummaryEmptyListsAreArrays(t *testing.T) {
	b, err := json.Marshal(scenario.Summary{})
	if err != nil {
		t.Fatal(err)
	}
	top := jsonKeys(t, b)
	for _, k := range []string{"failures", "warnings", "knownDefects", "assertions"} {
		if string(top[k]) != "[]" {
			t.Errorf("%q of a zero summary is %s, want []", k, top[k])
		}
	}
	// A pointer marshals the same, and set lists are kept.
	s := &scenario.Summary{Failures: []string{"all-complete.failed: 1, want == 0"}}
	if b, err = json.Marshal(s); err != nil {
		t.Fatal(err)
	}
	if top = jsonKeys(t, b); string(top["failures"]) != `["all-complete.failed: 1, want == 0"]` || string(top["warnings"]) != "[]" {
		t.Errorf("summary %s", b)
	}
}

// Run directories sort by start time and name the scenario and the commit.
func TestRunDataDirNameAndPaths(t *testing.T) {
	start := time.Date(2026, 10, 4, 22, 15, 0, 0, time.FixedZone("CEST", 2*3600))
	if got, want := scenario.RunDirName(start, "smoke", "0123456789abcdef0123", true), "20261004T201500Z-smoke-0123456789ab-dirty"; got != want {
		t.Errorf("RunDirName = %q, want %q", got, want)
	}
	if got, want := scenario.RunDirName(start, "nightly-mixed", "abc", false), "20261004T201500Z-nightly-mixed-abc"; got != want {
		t.Errorf("RunDirName = %q, want %q", got, want)
	}
	if got, want := scenario.KubeSnapshotFile(scenario.KindNodeTwins, scenario.SnapshotStart), "k8s/nodetwins-start.yaml"; got != want {
		t.Errorf("KubeSnapshotFile = %q, want %q", got, want)
	}
}

// Attribute accessors return a value only for its own type, except that an
// int reads as a float.
func TestRunDataAttributes(t *testing.T) {
	a := scenario.Attributes{"s": "eco", "b": true, "i": int64(7), "f": 1.5}
	if v, ok := a.String("s"); !ok || v != "eco" {
		t.Errorf("String = %q, %v", v, ok)
	}
	if v, ok := a.Bool("b"); !ok || !v {
		t.Errorf("Bool = %v, %v", v, ok)
	}
	if v, ok := a.Int("i"); !ok || v != 7 {
		t.Errorf("Int = %d, %v", v, ok)
	}
	if _, ok := a.Int("f"); ok {
		t.Error("Int read a double")
	}
	if v, ok := a.Float("i"); !ok || v != 7 {
		t.Errorf("Float of an int = %v, %v", v, ok)
	}
	if v, ok := a.Float("f"); !ok || v != 1.5 {
		t.Errorf("Float = %v, %v", v, ok)
	}
	if _, ok := a.String("missing"); ok {
		t.Error("String found a missing key")
	}
	if _, ok := a.Float("s"); ok {
		t.Error("Float read a string")
	}
}
