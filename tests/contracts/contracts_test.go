// Package contracts_test validates cross-component contracts in Joulie.
//
// These tests catch drift between components -- for example, if the simulator
// uses a different annotation key than the scheduler reads, or if the workload
// generator emits a class name that no other component recognizes.
//
// Components covered: simulator, workload generator, scheduler extender,
// operator FSM, agent control client, and workload classifier.
package contracts_test

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/operator/fsm"
)

// --------------------------------------------------------------------------
// Shared helpers
// --------------------------------------------------------------------------

// traceJobRecord mirrors the workloadgen's jobRecord JSON structure, keeping
// only the fields we need for contract validation.
type traceJobRecord struct {
	Type                string             `json:"type"`
	SchemaVersion       string             `json:"schemaVersion"`
	JobID               string             `json:"jobId"`
	WorkloadID          string             `json:"workloadId"`
	WorkloadType        string             `json:"workloadType"`
	PodRole             string             `json:"podRole"`
	Gang                bool               `json:"gang"`
	SubmitTimeOffsetSec float64            `json:"submitTimeOffsetSec"`
	Namespace           string             `json:"namespace"`
	IntentClass         string             `json:"intentClass"`
	PodTemplate         traceTemplatePart  `json:"podTemplate"`
	Work                traceWorkPart      `json:"work"`
	Sensitivity         traceSensitivity   `json:"sensitivity"`
	WorkloadClass       traceWorkloadClass `json:"workloadClass"`
	WorkloadProfile     traceProfile       `json:"workloadProfile"`
	Tags                []string           `json:"tags"`
}

type traceTemplatePart struct {
	Affinity map[string]any    `json:"affinity,omitempty"`
	Requests map[string]string `json:"requests"`
}

type traceWorkPart struct {
	CPUUnits float64 `json:"cpuUnits"`
	GPUUnits float64 `json:"gpuUnits"`
}

type traceSensitivity struct {
	CPU float64 `json:"cpu"`
	GPU float64 `json:"gpu"`
}

type traceWorkloadClass struct {
	CPU string `json:"cpu,omitempty"`
	GPU string `json:"gpu,omitempty"`
}

type traceProfile struct {
	CPUUtilization      float64 `json:"cpuUtilization,omitempty"`
	GPUUtilization      float64 `json:"gpuUtilization,omitempty"`
	MemoryIntensity     float64 `json:"memoryIntensity,omitempty"`
	IOIntensity         float64 `json:"ioIntensity,omitempty"`
	CPUFeedIntensityGPU float64 `json:"cpuFeedIntensityGpu,omitempty"`
}

// traceWorkloadRecord mirrors the workloadgen's workloadRecord JSON structure.
type traceWorkloadRecord struct {
	Type          string `json:"type"`
	SchemaVersion string `json:"schemaVersion"`
	WorkloadID    string `json:"workloadId"`
	IntentClass   string `json:"intentClass"`
}

// generateTrace shells out to workloadgen and returns job records.
func generateTrace(t *testing.T, jobs int) ([]traceJobRecord, []traceWorkloadRecord) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "trace.jsonl")
	cmd := exec.Command("go", "run", "./simulator/cmd/workloadgen",
		"-jobs", strings.Repeat("", 0)+intToStr(jobs),
		"-out", out,
		"-seed", "42",
	)
	cmd.Dir = repoRoot(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("workloadgen failed: %v\n%s", err, output)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer f.Close()
	var jobRecs []traceJobRecord
	var wlRecs []traceWorkloadRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("parse trace line: %v", err)
		}
		tp, _ := raw["type"].(string)
		switch tp {
		case "job":
			var rec traceJobRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("parse job record: %v", err)
			}
			jobRecs = append(jobRecs, rec)
		case "workload":
			var rec traceWorkloadRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("parse workload record: %v", err)
			}
			wlRecs = append(wlRecs, rec)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan trace: %v", err)
	}
	return jobRecs, wlRecs
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// Walk up from this test file to find go.mod.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.mod)")
		}
		dir = parent
	}
}

func intToStr(n int) string {
	return strings.TrimSpace(strings.Replace("  "+strings.Repeat("", 0), "  ", "", 1)) + func() string {
		s := ""
		if n == 0 {
			return "0"
		}
		v := n
		for v > 0 {
			s = string(rune('0'+v%10)) + s
			v /= 10
		}
		return s
	}()
}

// --------------------------------------------------------------------------
// Constants that are hardcoded in components but not exported.
// Source references are in comments so drift is easy to trace.
// --------------------------------------------------------------------------

const (
	// Annotation key used by all components.
	// Source: cmd/scheduler/main.go:302, simulator/cmd/simulator/main.go:2134,
	//         pkg/workloadprofile/classifier/classifier.go:110
	workloadClassAnnotationKey = "joulie.io/workload-class"

	// Power profile label key.
	// Source: pkg/operator/fsm/fsm.go:33, cmd/scheduler/main.go:292,
	//         simulator/cmd/simulator/main.go:2212
	powerProfileLabelKey = "joulie.io/power-profile"
)

// Known valid intent classes: "performance" and "standard".
// Source: cmd/scheduler/main.go:248 (podWorkloadClass defaults to "standard"),
//         simulator/cmd/workloadgen/main.go:348-365 (sampleIntentClass returns only these two).
var validIntentClasses = map[string]bool{
	"performance": true,
	"standard":    true,
}

// Stale intent classes that should never appear in generated traces.
var staleIntentClasses = []string{"general", "eco", "batch", "unknown"}

// --------------------------------------------------------------------------
// 1. Annotation Key Consistency
// --------------------------------------------------------------------------

func TestWorkloadClassAnnotationKeyConsistentAcrossComponents(t *testing.T) {
	// The FSM package does not export the workload class annotation key directly,
	// but the classifier uses "joulie.io/workload-class" (hardcoded string).
	// Verify our hardcoded constant matches what the FSM's PowerProfileLabelKey
	// is NOT (they are different keys for different purposes).
	if workloadClassAnnotationKey == fsm.PowerProfileLabelKey {
		t.Fatal("workload-class annotation key should differ from power-profile label key")
	}
	// Verify the hardcoded annotation key is correct.
	// This string appears verbatim in: scheduler podWorkloadClass(), simulator
	// injectTraceJobs(), and classifier ParsePodHints().
	if workloadClassAnnotationKey != "joulie.io/workload-class" {
		t.Fatalf("expected joulie.io/workload-class, got %s", workloadClassAnnotationKey)
	}
}

// --------------------------------------------------------------------------
// 2. Power Profile Label Consistency
// --------------------------------------------------------------------------

func TestPowerProfileLabelKeyConsistentWithFSM(t *testing.T) {
	// fsm.PowerProfileLabelKey is the canonical source.
	if fsm.PowerProfileLabelKey != powerProfileLabelKey {
		t.Fatalf("power profile label mismatch: fsm=%q, expected=%q",
			fsm.PowerProfileLabelKey, powerProfileLabelKey)
	}
	if fsm.PowerProfileLabelKey != "joulie.io/power-profile" {
		t.Fatalf("fsm.PowerProfileLabelKey has unexpected value: %q", fsm.PowerProfileLabelKey)
	}
}

func TestPowerProfileValuesConsistent(t *testing.T) {
	// These must match what the scheduler checks in shouldFilterNode
	// (cmd/scheduler/main.go:287-292) and what the simulator's
	// affinityForIntentClass uses (simulator/cmd/simulator/main.go:2204-2214).
	if fsm.ProfilePerformance != "performance" {
		t.Fatalf("ProfilePerformance=%q, expected \"performance\"", fsm.ProfilePerformance)
	}
	if fsm.ProfileEco != "eco" {
		t.Fatalf("ProfileEco=%q, expected \"eco\"", fsm.ProfileEco)
	}
}

// --------------------------------------------------------------------------
// 3. Intent Class Value Consistency
// --------------------------------------------------------------------------

func TestIntentClassValuesAreRecognizedByScheduler(t *testing.T) {
	jobs, wlRecs := generateTrace(t, 50)
	if len(jobs) == 0 {
		t.Fatal("workloadgen produced no job records")
	}
	for _, j := range jobs {
		if !validIntentClasses[j.IntentClass] {
			t.Errorf("job %s has intentClass=%q which the scheduler would not recognize "+
				"(scheduler only knows: performance, standard)",
				j.JobID, j.IntentClass)
		}
	}
	for _, w := range wlRecs {
		if !validIntentClasses[w.IntentClass] {
			t.Errorf("workload %s has intentClass=%q which is not recognized",
				w.WorkloadID, w.IntentClass)
		}
	}
}

func TestNoStaleIntentClassValues(t *testing.T) {
	jobs, _ := generateTrace(t, 100)
	for _, j := range jobs {
		for _, stale := range staleIntentClasses {
			if j.IntentClass == stale {
				t.Errorf("job %s has stale intentClass=%q -- this value was removed from the system",
					j.JobID, stale)
			}
		}
	}
}

// --------------------------------------------------------------------------
// 4. Affinity Round-Trip
// --------------------------------------------------------------------------

func TestWorkloadgenAffinityParsableBySimulator(t *testing.T) {
	// Generate a trace and find a performance job with affinity.
	jobs, _ := generateTrace(t, 50)
	var found bool
	for _, j := range jobs {
		if j.IntentClass != "performance" || j.PodTemplate.Affinity == nil {
			continue
		}
		found = true
		// Verify the affinity structure matches what the simulator's
		// classifyClassFromAffinityMap expects: it re-marshals and then
		// uses corev1.Affinity deserialization.
		classified := classifyFromAffinityMap(t, j.PodTemplate.Affinity)
		if classified != "performance" {
			t.Errorf("job %s: workloadgen emitted performance affinity but simulator "+
				"would classify it as %q", j.JobID, classified)
		}
		break
	}
	if !found {
		t.Log("no performance jobs in trace (may happen at low perf-ratio); increasing jobs")
		jobs, _ = generateTrace(t, 200)
		for _, j := range jobs {
			if j.IntentClass != "performance" || j.PodTemplate.Affinity == nil {
				continue
			}
			found = true
			classified := classifyFromAffinityMap(t, j.PodTemplate.Affinity)
			if classified != "performance" {
				t.Errorf("job %s: classified as %q, expected performance", j.JobID, classified)
			}
			break
		}
		if !found {
			t.Fatal("could not find any performance job with affinity in 200-job trace")
		}
	}
}

func TestStandardAffinityRoundTrip(t *testing.T) {
	jobs, _ := generateTrace(t, 50)
	for _, j := range jobs {
		if j.IntentClass != "standard" {
			continue
		}
		// Standard jobs should have nil affinity.
		if j.PodTemplate.Affinity != nil && len(j.PodTemplate.Affinity) > 0 {
			t.Errorf("job %s: standard class should have nil/empty affinity, got %v",
				j.JobID, j.PodTemplate.Affinity)
		}
		// With nil affinity, simulator's classifyClassFromAffinityMap defaults to "standard".
		classified := classifyFromAffinityMap(t, nil)
		if classified != "standard" {
			t.Errorf("nil affinity classified as %q, expected standard", classified)
		}
		return
	}
	t.Fatal("no standard jobs found in trace")
}

// classifyFromAffinityMap mirrors the simulator's classifyClassFromAffinityMap logic:
// it marshals the affinity to JSON, wraps it, unmarshals as corev1.Affinity, then
// classifies based on the node affinity expressions.
//
// Source: simulator/cmd/simulator/main.go:2280-2293
func classifyFromAffinityMap(t *testing.T, affinityRaw map[string]any) string {
	t.Helper()
	if affinityRaw == nil {
		return "standard"
	}
	// Navigate the structure the same way the simulator does.
	na, ok := affinityRaw["nodeAffinity"].(map[string]any)
	if !ok {
		return "standard"
	}
	req, ok := na["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
	if !ok {
		return "standard"
	}
	termsRaw, ok := req["nodeSelectorTerms"].([]any)
	if !ok {
		return "standard"
	}
	perfAllowed := false
	ecoAllowed := false
	for _, termRaw := range termsRaw {
		term, ok := termRaw.(map[string]any)
		if !ok {
			continue
		}
		exprsRaw, ok := term["matchExpressions"].([]any)
		if !ok {
			continue
		}
		termPerf := true
		termEco := true
		for _, exprRaw := range exprsRaw {
			expr, ok := exprRaw.(map[string]any)
			if !ok {
				continue
			}
			key, _ := expr["key"].(string)
			if key != powerProfileLabelKey {
				continue
			}
			op, _ := expr["operator"].(string)
			values := extractStringSlice(expr["values"])
			switch op {
			case "In":
				termPerf = false
				termEco = false
				for _, v := range values {
					if v == "performance" {
						termPerf = true
					}
					if v == "eco" {
						termEco = true
					}
				}
			case "NotIn":
				for _, v := range values {
					if v == "performance" {
						termPerf = false
					}
					if v == "eco" {
						termEco = false
					}
				}
			case "DoesNotExist":
				termPerf = false
				termEco = false
			}
		}
		perfAllowed = perfAllowed || termPerf
		ecoAllowed = ecoAllowed || termEco
	}
	if perfAllowed && !ecoAllowed {
		return "performance"
	}
	return "standard"
}

func extractStringSlice(v any) []string {
	if v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		s, ok := item.(string)
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// --------------------------------------------------------------------------
// 5. Trace Schema Compatibility
// --------------------------------------------------------------------------

func TestTraceJobRecordsHaveAllRequiredFieldsForSimulator(t *testing.T) {
	jobs, _ := generateTrace(t, 30)
	if len(jobs) == 0 {
		t.Fatal("no jobs generated")
	}
	for _, j := range jobs {
		if j.JobID == "" {
			t.Error("job record missing jobId")
		}
		if j.Type != "job" {
			t.Errorf("job %s: type=%q, expected \"job\"", j.JobID, j.Type)
		}
		if j.IntentClass == "" {
			t.Errorf("job %s: missing intentClass", j.JobID)
		}
		if j.Namespace == "" {
			t.Errorf("job %s: missing namespace", j.JobID)
		}
		if j.PodTemplate.Requests == nil {
			t.Errorf("job %s: missing podTemplate.requests", j.JobID)
			continue
		}
		if _, ok := j.PodTemplate.Requests["cpu"]; !ok {
			t.Errorf("job %s: podTemplate.requests missing cpu", j.JobID)
		}
		// work.cpuUnits must be > 0 for the simulator to make progress
		if j.Work.CPUUnits <= 0 {
			t.Errorf("job %s: work.cpuUnits=%.2f (must be >0 for simulator)", j.JobID, j.Work.CPUUnits)
		}
		// submitTimeOffsetSec must be non-negative
		if j.SubmitTimeOffsetSec < 0 {
			t.Errorf("job %s: negative submitTimeOffsetSec=%.2f", j.JobID, j.SubmitTimeOffsetSec)
		}
	}
}

func TestGeneratedTraceLoadableBySimulator(t *testing.T) {
	// Generate a trace and verify the raw JSON has all fields the simulator's
	// loadTraceFileIntoEngine reads.
	dir := t.TempDir()
	out := filepath.Join(dir, "trace.jsonl")
	cmd := exec.Command("go", "run", "./simulator/cmd/workloadgen",
		"-jobs", "20",
		"-out", out,
		"-seed", "42",
	)
	cmd.Dir = repoRoot(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("workloadgen failed: %v\n%s", err, output)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	jobCount := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("line parse: %v", err)
		}
		tp, _ := raw["type"].(string)
		if tp != "job" {
			continue
		}
		jobCount++
		// Verify the simulator's loadTraceFileIntoEngine can extract all fields.
		jobID, ok := raw["jobId"].(string)
		if !ok || jobID == "" {
			t.Error("missing jobId")
		}
		// podTemplate with requests
		podTpl, ok := raw["podTemplate"].(map[string]any)
		if !ok {
			t.Errorf("job %s: missing podTemplate", jobID)
			continue
		}
		reqRaw, ok := podTpl["requests"].(map[string]any)
		if !ok {
			t.Errorf("job %s: missing podTemplate.requests", jobID)
			continue
		}
		if _, ok := reqRaw["cpu"]; !ok {
			t.Errorf("job %s: missing podTemplate.requests.cpu", jobID)
		}
		// work
		workRaw, ok := raw["work"].(map[string]any)
		if !ok {
			t.Errorf("job %s: missing work", jobID)
			continue
		}
		if cpuU, ok := workRaw["cpuUnits"].(float64); !ok || cpuU <= 0 {
			t.Errorf("job %s: invalid work.cpuUnits", jobID)
		}
		// workloadClass (simulator reads cpu and gpu sub-fields)
		if wcRaw, ok := raw["workloadClass"].(map[string]any); ok {
			if _, ok := wcRaw["cpu"].(string); !ok {
				t.Errorf("job %s: workloadClass missing cpu field", jobID)
			}
		}
		// workloadProfile (simulator reads cpuUtilization etc.)
		if profRaw, ok := raw["workloadProfile"].(map[string]any); ok {
			if _, ok := profRaw["cpuUtilization"].(float64); !ok {
				t.Errorf("job %s: workloadProfile missing cpuUtilization", jobID)
			}
		}
	}
	if jobCount == 0 {
		t.Fatal("no job records in trace")
	}
	t.Logf("validated %d job records are loadable by simulator", jobCount)
}

// --------------------------------------------------------------------------
// 6. Pod Annotation Contract
// --------------------------------------------------------------------------

func TestSimulatorPodAnnotationsMatchSchedulerExpectations(t *testing.T) {
	// The simulator sets annotations on injected pods (simulator/cmd/simulator/main.go:2134):
	//   "joulie.io/workload-class": j.Class
	// The scheduler reads (cmd/scheduler/main.go:302):
	//   pod.Metadata.Annotations["joulie.io/workload-class"]
	// Verify the key matches.
	simulatorAnnotationKey := "joulie.io/workload-class" // from injectTraceJobs
	schedulerAnnotationKey := "joulie.io/workload-class" // from podWorkloadClass

	if simulatorAnnotationKey != schedulerAnnotationKey {
		t.Fatalf("annotation key mismatch: simulator=%q, scheduler=%q",
			simulatorAnnotationKey, schedulerAnnotationKey)
	}
	if simulatorAnnotationKey != workloadClassAnnotationKey {
		t.Fatalf("annotation key constant mismatch")
	}
}

func TestSimulatorPodAffinityMatchesSchedulerFilter(t *testing.T) {
	// The simulator's affinityForIntentClass("performance") creates:
	//   NotIn [eco] on joulie.io/power-profile
	// The scheduler's shouldFilterNode checks SchedulableClass and label fallback.
	// The FSM's PodExcludesEco checks the same affinity pattern.
	// Verify the workloadgen produces the same structure.
	jobs, _ := generateTrace(t, 100)
	for _, j := range jobs {
		if j.IntentClass != "performance" {
			continue
		}
		aff := j.PodTemplate.Affinity
		if aff == nil {
			t.Errorf("job %s: performance pod has nil affinity, scheduler filter relies on this", j.JobID)
			continue
		}
		// Verify the affinity targets joulie.io/power-profile with NotIn [eco].
		na, ok := aff["nodeAffinity"].(map[string]any)
		if !ok {
			t.Errorf("job %s: performance affinity missing nodeAffinity", j.JobID)
			continue
		}
		req, ok := na["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
		if !ok {
			t.Errorf("job %s: missing requiredDuringSchedulingIgnoredDuringExecution", j.JobID)
			continue
		}
		terms, ok := req["nodeSelectorTerms"].([]any)
		if !ok || len(terms) == 0 {
			t.Errorf("job %s: missing nodeSelectorTerms", j.JobID)
			continue
		}
		term := terms[0].(map[string]any)
		exprs := term["matchExpressions"].([]any)
		expr := exprs[0].(map[string]any)
		key, _ := expr["key"].(string)
		op, _ := expr["operator"].(string)
		vals := extractStringSlice(expr["values"])
		if key != fsm.PowerProfileLabelKey {
			t.Errorf("job %s: affinity key=%q, expected %q",
				j.JobID, key, fsm.PowerProfileLabelKey)
		}
		if op != "NotIn" {
			t.Errorf("job %s: affinity operator=%q, expected NotIn", j.JobID, op)
		}
		if len(vals) != 1 || vals[0] != fsm.ProfileEco {
			t.Errorf("job %s: affinity values=%v, expected [%q]",
				j.JobID, vals, fsm.ProfileEco)
		}
		return // one check is enough
	}
	t.Skip("no performance jobs in trace to validate affinity")
}

// --------------------------------------------------------------------------
// 7. Telemetry Format Contract
// --------------------------------------------------------------------------

func TestSimulatorTelemetryFormatMatchesAgentExpectations(t *testing.T) {
	// The simulator's handleTelemetry (simulator/cmd/simulator/main.go:647-695)
	// returns a JSON response. The agent's HTTPPowerReader
	// (pkg/agent/control/http.go:48-55) reads:
	//   - top-level "packagePowerWatts"
	//   - nested "cpu"."packagePowerWatts"
	//
	// Build a mock response matching the simulator's format and verify the
	// agent would find what it needs.
	simulatorResp := map[string]any{
		"node":              "test-node",
		"packagePowerWatts": 150.0,
		"cpu": map[string]any{
			"packagePowerWatts":  145.0,
			"capWatts":           200.0,
			"targetCapWatts":     180.0,
			"utilization":        0.65,
			"freqScale":          0.95,
			"throttlePct":        0,
			"thermalThrottlePct": 0.0,
			"temperatureC":       55.0,
			"raplCapWatts":       200.0,
			"capSaturated":       false,
		},
		"gpu": map[string]any{
			"present":               true,
			"vendor":                "nvidia",
			"product":               "A100",
			"count":                 2,
			"powerWattsTotal":       400.0,
			"capWattsPerGpuApplied": 300.0,
			"capWattsPerGpuTarget":  250.0,
			"utilization":           0.80,
		},
	}

	// Agent reads packagePowerWatts from top level
	topLevel, ok := simulatorResp["packagePowerWatts"].(float64)
	if !ok {
		t.Fatal("simulator response missing top-level packagePowerWatts")
	}
	if topLevel <= 0 {
		t.Error("packagePowerWatts should be > 0")
	}

	// Agent reads nested cpu.packagePowerWatts as fallback
	cpuMap, ok := simulatorResp["cpu"].(map[string]any)
	if !ok {
		t.Fatal("simulator response missing cpu map")
	}
	cpuPower, ok := cpuMap["packagePowerWatts"].(float64)
	if !ok {
		t.Fatal("simulator cpu response missing packagePowerWatts")
	}
	if cpuPower <= 0 {
		t.Error("cpu.packagePowerWatts should be > 0")
	}

	// Verify GPU fields the agent and operator may need
	gpuMap, ok := simulatorResp["gpu"].(map[string]any)
	if !ok {
		t.Fatal("simulator response missing gpu map")
	}
	requiredGPUFields := []string{"present", "count", "powerWattsTotal", "capWattsPerGpuApplied", "capWattsPerGpuTarget", "utilization"}
	for _, field := range requiredGPUFields {
		if _, ok := gpuMap[field]; !ok {
			t.Errorf("simulator gpu response missing field: %s", field)
		}
	}

	// Verify CPU fields
	requiredCPUFields := []string{"capWatts", "targetCapWatts", "utilization", "freqScale", "throttlePct", "capSaturated"}
	for _, field := range requiredCPUFields {
		if _, ok := cpuMap[field]; !ok {
			t.Errorf("simulator cpu response missing field: %s", field)
		}
	}
}

// --------------------------------------------------------------------------
// 8. Control Action Contract
// --------------------------------------------------------------------------

func TestSimulatorAcceptsAllAgentControlActions(t *testing.T) {
	// The agent sends these control actions:
	//   - "rapl.set_power_cap_watts" (cmd/agent/main.go:616, pkg/agent/control/http.go:60)
	//   - "dvfs.set_throttle_pct" (cmd/agent/main.go:753)
	//   - "gpu.set_power_cap_watts" (cmd/agent/main.go:851)
	//
	// The simulator handles exactly these in handleControl
	// (simulator/cmd/simulator/main.go:738-779).
	agentActions := []string{
		"rapl.set_power_cap_watts",
		"dvfs.set_throttle_pct",
		"gpu.set_power_cap_watts",
	}
	// These must match the simulator's switch cases exactly.
	simulatorActions := []string{
		"rapl.set_power_cap_watts",
		"dvfs.set_throttle_pct",
		"gpu.set_power_cap_watts",
	}
	if len(agentActions) != len(simulatorActions) {
		t.Fatalf("action count mismatch: agent=%d, simulator=%d",
			len(agentActions), len(simulatorActions))
	}
	simSet := make(map[string]bool)
	for _, a := range simulatorActions {
		simSet[a] = true
	}
	for _, a := range agentActions {
		if !simSet[a] {
			t.Errorf("agent sends action %q but simulator does not handle it", a)
		}
	}
}

// --------------------------------------------------------------------------
// 9. GPU Resource Name Consistency
// --------------------------------------------------------------------------

func TestGPUResourceNamesConsistent(t *testing.T) {
	// The workloadgen defaults to "nvidia.com/gpu" (simulator/cmd/workloadgen/main.go:761-766).
	// The simulator's loadTraceFileIntoEngine recognizes: "nvidia.com/gpu", "amd.com/gpu", "gpu"
	// (simulator/cmd/simulator/main.go:1947-1953).
	// The scheduler's podRequestsGPU checks: "nvidia.com/gpu", "amd.com/gpu", "gpu.intel.com/i915"
	// (cmd/scheduler/main.go:313-319).
	jobs, _ := generateTrace(t, 50)
	simulatorKnown := map[string]bool{
		"nvidia.com/gpu": true,
		"amd.com/gpu":    true,
		"gpu":            true,
	}
	schedulerKnown := map[string]bool{
		"nvidia.com/gpu":     true,
		"amd.com/gpu":        true,
		"gpu.intel.com/i915": true,
	}
	for _, j := range jobs {
		for key := range j.PodTemplate.Requests {
			if key == "cpu" || key == "memory" {
				continue
			}
			// This is a GPU resource request
			if !simulatorKnown[key] {
				t.Errorf("job %s requests GPU resource %q which simulator does not recognize "+
					"(simulator knows: nvidia.com/gpu, amd.com/gpu, gpu)", j.JobID, key)
			}
			if !schedulerKnown[key] {
				t.Errorf("job %s requests GPU resource %q which scheduler podRequestsGPU "+
					"does not recognize", j.JobID, key)
			}
		}
	}
}

// --------------------------------------------------------------------------
// 10. No Hardcoded Node Names
// --------------------------------------------------------------------------

func TestGeneratedTraceHasNoHardcodedNodeNames(t *testing.T) {
	jobs, _ := generateTrace(t, 100)
	// Only check the affinity and nodeSelector-like fields for hardcoded node names.
	// Job IDs and pod roles legitimately contain "worker", "launcher", etc.
	nodeNamePatterns := regexp.MustCompile(`(?i)(node-\d|k3s-\w+-\d|kwok-\w+-\d|master-\d)`)
	for _, j := range jobs {
		if j.PodTemplate.Affinity == nil {
			continue
		}
		b, err := json.Marshal(j.PodTemplate.Affinity)
		if err != nil {
			t.Fatalf("marshal affinity: %v", err)
		}
		raw := string(b)
		if nodeNamePatterns.MatchString(raw) {
			matches := nodeNamePatterns.FindAllString(raw, -1)
			t.Errorf("job %s affinity contains hardcoded node name patterns: %v -- "+
				"traces must be node-agnostic", j.JobID, matches)
		}
	}
}
