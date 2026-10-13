package workload_test

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/matbun/joulie/pkg/controller/fsm"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// Each Parse function accepts exactly its values, after trimming spaces, and
// names the setting and every valid value when it refuses one, so a bad
// environment variable or scenario key is a readable startup error.
func TestKeysParseEnums(t *testing.T) {
	cases := []struct {
		setting string
		valid   []string
		parse   func(string) (string, error)
	}{
		{"SIM_WORKLOAD_MODEL", []string{"legacy", "v1"},
			func(s string) (string, error) { v, err := workload.ParseModel(s); return string(v), err }},
		{"SIM_WORKLOAD_OWNER", []string{"job", "pod"},
			func(s string) (string, error) { v, err := workload.ParseOwner(s); return string(v), err }},
		{"SIM_WORKLOAD_INTENT", []string{"annotation", "affinity", "both", "none"},
			func(s string) (string, error) { v, err := workload.ParseIntent(s); return string(v), err }},
		{"SIM_WORKLOAD_COMPLETION", []string{"kwok", "delete"},
			func(s string) (string, error) { v, err := workload.ParseCompletion(s); return string(v), err }},
	}
	for _, c := range cases {
		for _, v := range c.valid {
			if got, err := c.parse(" " + v + "\n"); err != nil || got != v {
				t.Errorf("%s: parse %q = %q, %v; want %q", c.setting, v, got, err, v)
			}
		}
		for _, bad := range []string{"", "V1", "Job", "bogus"} {
			got, err := c.parse(bad)
			if err == nil {
				t.Errorf("%s: parse %q = %q, want an error", c.setting, bad, got)
				continue
			}
			if !strings.Contains(err.Error(), c.setting) || !strings.Contains(err.Error(), strings.Join(c.valid, ", ")) {
				t.Errorf("%s: error %q does not name the setting and %v", c.setting, err, c.valid)
			}
		}
	}
}

// The keys the simulator writes are the keys Joulie's components read: the
// class annotation and its values, and the power-profile label.
func TestKeysMatchTheController(t *testing.T) {
	if workload.AnnotationWorkloadClass != fsm.WorkloadClassAnnotation {
		t.Errorf("AnnotationWorkloadClass %q, controller reads %q", workload.AnnotationWorkloadClass, fsm.WorkloadClassAnnotation)
	}
	if workload.ClassPerformance != fsm.ProfilePerformance {
		t.Errorf("ClassPerformance %q, controller compares with %q", workload.ClassPerformance, fsm.ProfilePerformance)
	}
	if workload.LabelPowerProfile != fsm.PowerProfileLabelKey {
		t.Errorf("LabelPowerProfile %q, controller writes %q", workload.LabelPowerProfile, fsm.PowerProfileLabelKey)
	}
}

// The zero value of a limit is never "no limit": Unlimited is negative, and
// the legacy values are the simulator's constants before S4.
func TestKeysLimitsAndDefaults(t *testing.T) {
	if workload.Unlimited >= 0 {
		t.Errorf("Unlimited = %d, want a negative sentinel", workload.Unlimited)
	}
	if workload.DefaultMaxCreatesPerTick != workload.Unlimited {
		t.Errorf("v1 creates per tick %d, want Unlimited", workload.DefaultMaxCreatesPerTick)
	}
	legacy := []int{workload.LegacyMaxCreatesPerTick, workload.LegacyMaxDeletesPerTick, workload.LegacyKubeQPS, workload.LegacyKubeBurst}
	if !reflect.DeepEqual(legacy, []int{20, 10, 30, 50}) || workload.LegacyStallForceComplete != 120*time.Second {
		t.Errorf("legacy limits %v and %v, want [20 10 30 50] and 2m0s", legacy, workload.LegacyStallForceComplete)
	}
	if workload.DefaultOwner != workload.OwnerJob || workload.DefaultIntent != workload.IntentAnnotation ||
		workload.DefaultCompletion != workload.CompletionKWOK {
		t.Errorf("v1 defaults %q %q %q, want job annotation kwok", workload.DefaultOwner, workload.DefaultIntent, workload.DefaultCompletion)
	}
}

// Each variable has the name of the plan's v1 settings table, or the name
// the simulator already reads (trace path, tick, image, base speed), and
// EnvNames lists exactly these, sorted.
func TestKeysEnvNames(t *testing.T) {
	frozen := map[string]string{
		workload.EnvModel:              "SIM_WORKLOAD_MODEL",
		workload.EnvTracePath:          "SIM_WORKLOAD_TRACE_PATH",
		workload.EnvTick:               "SIM_WORKLOAD_TICK",
		workload.EnvImage:              "SIM_WORKLOAD_IMAGE",
		workload.EnvBaseSpeedPerCore:   "SIM_BASE_SPEED_PER_CORE",
		workload.EnvOwner:              "SIM_WORKLOAD_OWNER",
		workload.EnvIntent:             "SIM_WORKLOAD_INTENT",
		workload.EnvCompletion:         "SIM_WORKLOAD_COMPLETION",
		workload.EnvNamespace:          "SIM_WORKLOAD_NAMESPACE",
		workload.EnvNodeSelector:       "SIM_WORKLOAD_NODE_SELECTOR",
		workload.EnvTolerations:        "SIM_WORKLOAD_TOLERATIONS",
		workload.EnvJobTTL:             "SIM_WORKLOAD_JOB_TTL",
		workload.EnvMaxCreatesPerTick:  "SIM_WORKLOAD_MAX_CREATES_PER_TICK",
		workload.EnvMaxDeletesPerTick:  "SIM_WORKLOAD_MAX_DELETES_PER_TICK",
		workload.EnvKubeQPS:            "SIM_WORKLOAD_KUBE_QPS",
		workload.EnvKubeBurst:          "SIM_WORKLOAD_KUBE_BURST",
		workload.EnvStallReport:        "SIM_WORKLOAD_STALL_REPORT",
		workload.EnvStallForceComplete: "SIM_WORKLOAD_STALL_FORCE_COMPLETE",
		workload.EnvRunsDir:            "SIM_RUNS_DIR",
		workload.EnvRunID:              "SIM_RUN_ID",
		workload.EnvOTLPFileInterval:   "SIM_OTLP_FILE_INTERVAL",
	}
	want := make([]string, 0, len(frozen))
	for got, name := range frozen {
		if got != name {
			t.Errorf("variable %q, want %q", got, name)
		}
		want = append(want, name)
	}
	sort.Strings(want)
	if names := workload.EnvNames(); !reflect.DeepEqual(names, want) {
		t.Errorf("EnvNames() = %v, want %v", names, want)
	}
}

// The run label is 10 hex characters of the run id's SHA-256, so any run id
// fits a label value and two ids get different labels.
func TestKeysRunLabelValue(t *testing.T) {
	got := workload.RunLabelValue("run-1")
	// printf run-1 | sha256sum
	if want := "4e65d3fbe8"; got != want {
		t.Errorf("RunLabelValue(run-1) = %q, want %q", got, want)
	}
	long := workload.RunLabelValue(strings.Repeat("x", 300) + "/not a label value")
	if !regexp.MustCompile(`^[0-9a-f]{10}$`).MatchString(long) {
		t.Errorf("RunLabelValue of a long id = %q, want 10 hex characters", long)
	}
	if workload.RunLabelValue("run-2") == got {
		t.Error("run-1 and run-2 share a label value")
	}
}

// The run API bodies have the lowerCamel keys the scenario runner sends and
// reads, and the options decode durations and tolerations as Kubernetes
// writes them.
func TestKeysRunAPIJSON(t *testing.T) {
	var req workload.RunRequest
	body := `{"runId":"r1","trace":"/t.jsonl","clusterName":"smoke","options":{"owner":"job","intent":"both",
		"completion":"kwok","namespace":"ns","jobTTL":"10m","nodeSelector":{},"maxCreatesPerTick":-1,"stallReport":"60s","baseSpeedPerCore":2,
		"tolerations":[{"key":"kwok.x-k8s.io/node","operator":"Equal","value":"fake","effect":"NoSchedule"}]}}`
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	o := req.Options
	if req.RunID != "r1" || o.Owner != workload.OwnerJob || o.Intent != workload.IntentBoth || o.JobTTL.Duration != 10*time.Minute ||
		o.StallReport.Duration != time.Minute || *o.MaxCreatesPerTick != workload.Unlimited || *o.BaseSpeedPerCore != 2 {
		t.Errorf("decoded %+v", req)
	}
	if o.NodeSelector == nil || len(o.NodeSelector) != 0 {
		t.Errorf("an empty nodeSelector must decode to an empty, non-nil map, got %#v", o.NodeSelector)
	}
	if len(o.Tolerations) != 1 || o.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("tolerations %+v", o.Tolerations)
	}
	// Written back, an empty selector or toleration list stays empty, which
	// clears the default, and an unset one stays unset, which keeps it.
	for _, o := range []workload.RunOptions{
		{NodeSelector: map[string]string{}, Tolerations: []corev1.Toleration{}},
		{},
		{NodeSelector: map[string]string{"a": "b"}},
		{Tolerations: []corev1.Toleration{}},
	} {
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		var back workload.RunOptions
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if (back.NodeSelector == nil) != (o.NodeSelector == nil) || (back.Tolerations == nil) != (o.Tolerations == nil) {
			t.Errorf("options %#v written as %s read back as %#v", o, b, back)
		}
	}

	status := workload.RunStatus{RunID: "r1", Phase: workload.PhaseRunning, Jobs: workload.RunJobCounts{Total: 3}}
	b, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"runId", "phase", "startedAt", "elapsedSeconds", "allSubmitted", "jobs", "runStalled"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("RunStatus JSON %s has no %q", b, k)
		}
	}
	var counts map[string]int
	if err := json.Unmarshal(keys["jobs"], &counts); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"total", "notDue", "pending", "scheduled", "running", "completed", "failed", "incomplete", "stalled"} {
		if _, ok := counts[k]; !ok {
			t.Errorf("RunJobCounts JSON %s has no %q", keys["jobs"], k)
		}
	}

	e := &workload.RunError{Message: "bad intentClass", Line: 7}
	if e.Error() != "trace line 7: bad intentClass" {
		t.Errorf("RunError = %q", e.Error())
	}
}
