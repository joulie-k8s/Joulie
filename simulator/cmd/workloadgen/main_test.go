package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Original tests (preserved)
// ---------------------------------------------------------------------------

func TestSampleLogicalWorkloadDistributedHasMultiplePods(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	cfg := generatorConfig{
		Namespace:        "default",
		GPURatio:         1,
		CPURatePerCore:   1,
		GPURatePerGPU:    1,
		MeanInterArrival: 1,
	}
	found := false
	for i := 0; i < 100; i++ {
		wl := sampleLogicalWorkload(rng, i+1, float64(i), cfg)
		if wl.Type != "distributed_training" && wl.Type != "parameter_server_training" && wl.Type != "hpo_experiment" {
			continue
		}
		found = true
		if len(wl.Pods) < 2 {
			t.Fatalf("expected multi-pod workload for %s, got %d pods", wl.Type, len(wl.Pods))
		}
		if wl.Type == "distributed_training" && !wl.Gang {
			t.Fatalf("expected distributed training workload to be gang-scheduled")
		}
		break
	}
	if !found {
		t.Fatalf("did not sample a multi-pod workload in 100 attempts")
	}
}

func TestExpandLogicalWorkloadAddsWorkloadMetadata(t *testing.T) {
	wl := logicalWorkload{
		ID:        "workload-1",
		Type:      "distributed_training",
		Namespace: "default",
		Gang:      true,
		CPUClass:  "cpu.mixed",
		GPUClass:  "gpu.compute_bound",
		Profile: workloadProfileRec{
			CPUUtilization: 0.3,
			GPUUtilization: 0.5,
		},
		Pods: []logicalPod{{
			Role:        "worker",
			Replicas:    2,
			CPURequest:  4,
			MemoryGiB:   8,
			GPURequest:  1,
			GPUResource: "nvidia.com/gpu",
			CPUUnits:    100,
			GPUUnits:    200,
			IntentClass: "performance",
		}},
	}
	cfg := generatorConfig{}
	ord := 0
	jobs := expandLogicalWorkload(wl, cfg, &ord)
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
	for _, j := range jobs {
		if j.WorkloadID != wl.ID || j.WorkloadType != wl.Type || j.PodRole != "worker" {
			t.Fatalf("missing workload metadata in expanded job: %+v", j)
		}
		if !j.Gang {
			t.Fatalf("expected gang metadata on expanded job")
		}
	}
}

func TestHourlyArrivalMultiplierHasNightDip(t *testing.T) {
	if !(hourlyArrivalMultiplier(2) < hourlyArrivalMultiplier(10)) {
		t.Fatalf("expected night arrival multiplier to be lower than daytime")
	}
	if !(hourlyArrivalMultiplier(12) < hourlyArrivalMultiplier(10)) {
		t.Fatalf("expected lunch dip to be lower than daytime")
	}
}

// ---------------------------------------------------------------------------
// Affinity tests
// ---------------------------------------------------------------------------

func TestAffinityForClassPerformance(t *testing.T) {
	aff := affinityForClass("performance")
	if aff == nil {
		t.Fatal("expected non-nil affinity for performance class")
	}
	na, ok := aff["nodeAffinity"]
	if !ok {
		t.Fatal("missing nodeAffinity key")
	}
	naMap, ok := na.(map[string]any)
	if !ok {
		t.Fatal("nodeAffinity is not a map")
	}
	req, ok := naMap["requiredDuringSchedulingIgnoredDuringExecution"]
	if !ok {
		t.Fatal("missing requiredDuringSchedulingIgnoredDuringExecution")
	}
	reqMap := req.(map[string]any)
	terms := reqMap["nodeSelectorTerms"].([]map[string]any)
	if len(terms) == 0 {
		t.Fatal("empty nodeSelectorTerms")
	}
	exprs := terms[0]["matchExpressions"].([]map[string]any)
	if len(exprs) == 0 {
		t.Fatal("empty matchExpressions")
	}
	expr := exprs[0]
	if expr["key"] != "joulie.io/power-profile" {
		t.Fatalf("unexpected key: %v", expr["key"])
	}
	if expr["operator"] != "NotIn" {
		t.Fatalf("unexpected operator: %v", expr["operator"])
	}
	vals := expr["values"].([]string)
	if len(vals) != 1 || vals[0] != "eco" {
		t.Fatalf("unexpected values: %v", vals)
	}
}

func TestAffinityForClassStandard(t *testing.T) {
	aff := affinityForClass("standard")
	if aff != nil {
		t.Fatalf("expected nil affinity for standard class, got %v", aff)
	}
}

func TestAffinityForClassNeverContainsNodeName(t *testing.T) {
	for _, class := range []string{"performance", "standard", "unknown"} {
		aff := affinityForClass(class)
		if aff == nil {
			continue
		}
		b, _ := json.Marshal(aff)
		s := string(b)
		for _, forbidden := range []string{"nodeName", "\"node\""} {
			if strings.Contains(s, forbidden) {
				t.Fatalf("affinity for class %q contains forbidden key %q: %s", class, forbidden, s)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// sampleIntentClass tests
// ---------------------------------------------------------------------------

func TestSampleIntentClassOnlyReturnsValidValues(t *testing.T) {
	cfg := generatorConfig{PerfRatio: 0.5, ComputeBoundPerfBoost: 2.0}
	valid := map[string]bool{"performance": true, "standard": true}
	for i := 0; i < 1000; i++ {
		rng := rand.New(rand.NewSource(int64(i)))
		cls := sampleIntentClass(rng, cfg, "cpu.mixed", "gpu.compute_bound")
		if !valid[cls] {
			t.Fatalf("unexpected intent class: %q", cls)
		}
	}
}

func TestSampleIntentClassNoAffinityOnlyAlwaysStandard(t *testing.T) {
	cfg := generatorConfig{PerfRatio: 1.0, ComputeBoundPerfBoost: 10.0, NoAffinityOnly: true}
	for i := 0; i < 100; i++ {
		rng := rand.New(rand.NewSource(int64(i)))
		cls := sampleIntentClass(rng, cfg, "cpu.compute_bound", "gpu.compute_bound")
		if cls != "standard" {
			t.Fatalf("expected standard with NoAffinityOnly=true, got %q", cls)
		}
	}
}

func TestSampleIntentClassComputeBoundBoost(t *testing.T) {
	// With high boost, compute-bound classes should yield more performance
	cfgBoosted := generatorConfig{PerfRatio: 0.2, ComputeBoundPerfBoost: 5.0}
	cfgNoBoosted := generatorConfig{PerfRatio: 0.2, ComputeBoundPerfBoost: 1.0}
	perfBoosted, perfNoBoosted := 0, 0
	n := 2000
	for i := 0; i < n; i++ {
		rng1 := rand.New(rand.NewSource(int64(i)))
		rng2 := rand.New(rand.NewSource(int64(i)))
		if sampleIntentClass(rng1, cfgBoosted, "cpu.compute_bound", "") == "performance" {
			perfBoosted++
		}
		if sampleIntentClass(rng2, cfgNoBoosted, "cpu.compute_bound", "") == "performance" {
			perfNoBoosted++
		}
	}
	if perfBoosted <= perfNoBoosted {
		t.Fatalf("expected boosted compute-bound to yield more performance (%d) than unboosted (%d)", perfBoosted, perfNoBoosted)
	}
}

func TestSampleIntentClassPerfRatioZeroAlwaysStandard(t *testing.T) {
	cfg := generatorConfig{PerfRatio: 0, ComputeBoundPerfBoost: 1.0}
	for i := 0; i < 200; i++ {
		rng := rand.New(rand.NewSource(int64(i)))
		cls := sampleIntentClass(rng, cfg, "cpu.mixed", "gpu.mixed")
		if cls != "standard" {
			t.Fatalf("expected standard with PerfRatio=0, got %q", cls)
		}
	}
}

func TestSampleIntentClassPerfRatioOneAlwaysPerformance(t *testing.T) {
	cfg := generatorConfig{PerfRatio: 1.0, ComputeBoundPerfBoost: 1.0}
	for i := 0; i < 200; i++ {
		rng := rand.New(rand.NewSource(int64(i)))
		cls := sampleIntentClass(rng, cfg, "cpu.mixed", "gpu.mixed")
		if cls != "performance" {
			t.Fatalf("expected performance with PerfRatio=1, got %q", cls)
		}
	}
}

// ---------------------------------------------------------------------------
// classesForType tests
// ---------------------------------------------------------------------------

func TestClassesForTypeReturnsValidClasses(t *testing.T) {
	validCPU := map[string]bool{
		"cpu.compute_bound": true,
		"cpu.memory_bound":  true,
		"cpu.io_bound":      true,
		"cpu.mixed":         true,
	}
	validGPU := map[string]bool{
		"":                  true,
		"gpu.compute_bound": true,
		"gpu.memory_bound":  true,
		"gpu.mixed":         true,
	}
	types := []string{
		"cpu_preprocess", "cpu_analytics", "debug_eval",
		"single_gpu_training", "distributed_training",
		"parameter_server_training", "hpo_experiment",
		"unknown_type",
	}
	for _, wlType := range types {
		cpuC, gpuC := classesForType(wlType)
		if !validCPU[cpuC] {
			t.Errorf("invalid CPU class %q for type %q", cpuC, wlType)
		}
		if !validGPU[gpuC] {
			t.Errorf("invalid GPU class %q for type %q", gpuC, wlType)
		}
	}
}

// ---------------------------------------------------------------------------
// Expanded job record tests
// ---------------------------------------------------------------------------

func TestExpandedJobRecordsHaveIntentClass(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	cfg := defaultTestConfig()
	for i := 0; i < 50; i++ {
		wl := sampleLogicalWorkload(rng, i+1, float64(i*10), cfg)
		ord := 0
		jobs := expandLogicalWorkload(wl, cfg, &ord)
		for _, j := range jobs {
			if j.IntentClass != "performance" && j.IntentClass != "standard" {
				t.Fatalf("job %s has invalid intentClass %q", j.JobID, j.IntentClass)
			}
		}
	}
}

func TestExpandedJobRecordsHaveConsistentIntentAndAffinity(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	cfg := defaultTestConfig()
	for i := 0; i < 50; i++ {
		wl := sampleLogicalWorkload(rng, i+1, float64(i*10), cfg)
		ord := 0
		jobs := expandLogicalWorkload(wl, cfg, &ord)
		for _, j := range jobs {
			if j.IntentClass == "performance" {
				if j.PodTemplate.Affinity == nil {
					t.Fatalf("job %s is performance but has nil affinity", j.JobID)
				}
			} else if j.IntentClass == "standard" {
				if j.PodTemplate.Affinity != nil {
					t.Fatalf("job %s is standard but has non-nil affinity: %v", j.JobID, j.PodTemplate.Affinity)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// workloadToRecord tests
// ---------------------------------------------------------------------------

func TestWorkloadRecordHasIntentClass(t *testing.T) {
	for _, cls := range []string{"performance", "standard"} {
		wl := logicalWorkload{
			ID:        "wl-1",
			Type:      "debug_eval",
			Class:     cls,
			Namespace: "default",
			CPUClass:  "cpu.mixed",
			GPUClass:  "gpu.memory_bound",
			Pods: []logicalPod{{
				Role: "worker", Replicas: 1, CPURequest: 2, MemoryGiB: 4,
			}},
		}
		rec := workloadToRecord(wl)
		if rec.IntentClass != cls {
			t.Fatalf("expected intentClass=%q, got %q", cls, rec.IntentClass)
		}
		if rec.Type != "workload" {
			t.Fatalf("expected type=workload, got %q", rec.Type)
		}
	}
}

// ---------------------------------------------------------------------------
// End-to-end trace generation tests
// ---------------------------------------------------------------------------

func defaultTestConfig() generatorConfig {
	return generatorConfig{
		Namespace:             "default",
		Jobs:                  30,
		MeanInterArrival:      2,
		Seed:                  42,
		PerfRatio:             0.30,
		ComputeBoundPerfBoost: 3.5,
		GPURatio:              0.80,
		GPURequestPerJob:      1,
		CPURatePerCore:        1,
		GPURatePerGPU:         1,
		EmitWorkloadRecords:   true,
		BurstDayProbability:   0.25,
		BurstMeanJobs:         8.0,
		BurstMultiplier:       2.0,
	}
}

// generateTraceToFile is a helper that generates a trace to a temp file and
// returns the parsed lines as raw JSON maps plus the file path.
func generateTraceToFile(t *testing.T, cfg generatorConfig) ([]map[string]any, string) {
	t.Helper()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "trace.jsonl")
	setOutputPath(outFile)
	if err := generateTrace(cfg); err != nil {
		t.Fatalf("generateTrace failed: %v", err)
	}
	f, err := os.Open(outFile)
	if err != nil {
		t.Fatalf("open trace file: %v", err)
	}
	defer f.Close()
	var records []map[string]any
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var m map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			t.Fatalf("unmarshal line: %v\nline: %s", err, scanner.Text())
		}
		records = append(records, m)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("trace file is empty")
	}
	return records, outFile
}

func TestGenerateTraceEndToEnd(t *testing.T) {
	cfg := defaultTestConfig()
	records, _ := generateTraceToFile(t, cfg)

	jobCount, wlCount := 0, 0
	for _, r := range records {
		typ, _ := r["type"].(string)
		switch typ {
		case "job":
			jobCount++
			for _, reqField := range []string{"jobId", "type", "schemaVersion", "podTemplate", "work", "namespace", "intentClass"} {
				if _, ok := r[reqField]; !ok {
					t.Errorf("job record missing field %q: %v", reqField, r)
				}
			}
		case "workload":
			wlCount++
			for _, reqField := range []string{"workloadId", "type", "schemaVersion", "durationSec", "namespace", "intentClass", "workloadType"} {
				if _, ok := r[reqField]; !ok {
					t.Errorf("workload record missing field %q: %v", reqField, r)
				}
			}
		default:
			t.Errorf("unexpected record type: %q", typ)
		}
	}
	if jobCount == 0 {
		t.Error("no job records in trace")
	}
	if wlCount == 0 {
		t.Error("no workload records in trace")
	}
	if wlCount != cfg.Jobs {
		t.Errorf("expected %d workload records, got %d", cfg.Jobs, wlCount)
	}
}

func TestGenerateTraceNoNodeNames(t *testing.T) {
	cfg := defaultTestConfig()
	records, _ := generateTraceToFile(t, cfg)
	for i, r := range records {
		b, _ := json.Marshal(r)
		s := string(b)
		if strings.Contains(s, "nodeName") {
			t.Fatalf("record %d contains 'nodeName': %s", i, s)
		}
		// Check inside podTemplate specifically
		if pt, ok := r["podTemplate"]; ok {
			ptMap, _ := pt.(map[string]any)
			if _, found := ptMap["nodeName"]; found {
				t.Fatalf("podTemplate in record %d contains 'nodeName'", i)
			}
		}
	}
}

func TestGenerateTraceOnlyValidIntentClasses(t *testing.T) {
	cfg := defaultTestConfig()
	records, _ := generateTraceToFile(t, cfg)
	valid := map[string]bool{"performance": true, "standard": true}
	stale := []string{"general", "eco", "batch", "best-effort"}
	for i, r := range records {
		ic, ok := r["intentClass"].(string)
		if !ok {
			t.Fatalf("record %d missing intentClass string", i)
		}
		if !valid[ic] {
			t.Fatalf("record %d has invalid intentClass %q", i, ic)
		}
		for _, s := range stale {
			if ic == s {
				t.Fatalf("record %d has stale intent class %q", i, ic)
			}
		}
	}
}

func TestGenerateTraceResourceFormats(t *testing.T) {
	cfg := defaultTestConfig()
	records, _ := generateTraceToFile(t, cfg)

	cpuPattern := regexp.MustCompile(`^[0-9]+(\.[0-9]{1,2})?$`)
	memPattern := regexp.MustCompile(`^[0-9]+Gi$`)
	gpuPattern := regexp.MustCompile(`^[0-9]+$`)

	for i, r := range records {
		typ, _ := r["type"].(string)
		if typ != "job" {
			continue
		}
		pt, _ := r["podTemplate"].(map[string]any)
		reqs, _ := pt["requests"].(map[string]any)
		if reqs == nil {
			t.Fatalf("job record %d has nil requests", i)
		}
		cpu, _ := reqs["cpu"].(string)
		if !cpuPattern.MatchString(cpu) {
			t.Errorf("record %d: invalid CPU format %q", i, cpu)
		}
		mem, _ := reqs["memory"].(string)
		if !memPattern.MatchString(mem) {
			t.Errorf("record %d: invalid memory format %q", i, mem)
		}
		// GPU resources are optional
		for k, v := range reqs {
			if strings.Contains(k, "gpu") {
				gpuStr, _ := v.(string)
				if !gpuPattern.MatchString(gpuStr) {
					t.Errorf("record %d: invalid GPU format %q for key %q", i, gpuStr, k)
				}
				gpuVal, err := strconv.Atoi(gpuStr)
				if err != nil || gpuVal < 1 {
					t.Errorf("record %d: GPU value must be positive integer, got %q", i, gpuStr)
				}
			}
		}
	}
}

func TestGenerateTraceWorkloadClassStructure(t *testing.T) {
	cfg := defaultTestConfig()
	records, _ := generateTraceToFile(t, cfg)
	for i, r := range records {
		wc, ok := r["workloadClass"].(map[string]any)
		if !ok {
			t.Fatalf("record %d missing workloadClass map", i)
		}
		cpu, hasCPU := wc["cpu"].(string)
		if !hasCPU || cpu == "" {
			t.Errorf("record %d: workloadClass missing cpu field", i)
		}
		// GPU field is present (possibly empty) for all records
		if _, hasGPUKey := wc["gpu"]; !hasGPUKey {
			// gpu may be omitted via omitempty when empty string
			// That is acceptable -- only check that when present it is valid
		}
	}
}

func TestGenerateTraceJobRecordsHaveRequiredFields(t *testing.T) {
	cfg := defaultTestConfig()
	records, _ := generateTraceToFile(t, cfg)
	required := []string{"jobId", "type", "schemaVersion", "podTemplate", "work", "namespace", "intentClass", "workloadClass", "workloadProfile"}
	for i, r := range records {
		typ, _ := r["type"].(string)
		if typ != "job" {
			continue
		}
		for _, field := range required {
			if _, ok := r[field]; !ok {
				t.Errorf("job record %d missing required field %q", i, field)
			}
		}
		if r["schemaVersion"] != "v2" {
			t.Errorf("job record %d: expected schemaVersion=v2, got %v", i, r["schemaVersion"])
		}
	}
}

func TestGenerateTraceWorkloadRecordsHaveRequiredFields(t *testing.T) {
	cfg := defaultTestConfig()
	records, _ := generateTraceToFile(t, cfg)
	required := []string{"workloadId", "type", "schemaVersion", "durationSec", "namespace", "intentClass", "workloadType", "workloadClass", "pods"}
	for i, r := range records {
		typ, _ := r["type"].(string)
		if typ != "workload" {
			continue
		}
		for _, field := range required {
			if _, ok := r[field]; !ok {
				t.Errorf("workload record %d missing required field %q", i, field)
			}
		}
		dur, _ := r["durationSec"].(float64)
		if dur <= 0 {
			t.Errorf("workload record %d: durationSec must be positive, got %v", i, dur)
		}
	}
}

// ---------------------------------------------------------------------------
// buildPodsForWorkload tests
// ---------------------------------------------------------------------------

func TestBuildPodsForWorkloadNeverEmpty(t *testing.T) {
	types := []string{
		"cpu_preprocess", "cpu_analytics", "debug_eval",
		"single_gpu_training", "distributed_training",
		"parameter_server_training", "hpo_experiment",
	}
	for _, wlType := range types {
		t.Run(wlType, func(t *testing.T) {
			rng := rand.New(rand.NewSource(7))
			totalGPUs := 0
			if wlType != "cpu_preprocess" && wlType != "cpu_analytics" {
				totalGPUs = 2
			}
			cpuC, gpuC := classesForType(wlType)
			wl := logicalWorkload{
				ID:          "test-wl",
				Type:        wlType,
				Namespace:   "default",
				Gang:        wlType == "distributed_training" || wlType == "parameter_server_training",
				DurationSec: 3600,
				CPUClass:    cpuC,
				GPUClass:    gpuC,
				Profile:     workloadProfileRec{CPUUtilization: 0.5, GPUUtilization: 0.5},
			}
			cfg := defaultTestConfig()
			pods := buildPodsForWorkload(rng, wl, totalGPUs, "standard", cfg)
			if len(pods) == 0 {
				t.Fatalf("no pods built for workload type %s", wlType)
			}
		})
	}
}

func TestCPUOnlyWorkloadsHaveNoGPU(t *testing.T) {
	cpuTypes := []string{"cpu_preprocess", "cpu_analytics"}
	for _, wlType := range cpuTypes {
		t.Run(wlType, func(t *testing.T) {
			rng := rand.New(rand.NewSource(11))
			cpuC, gpuC := classesForType(wlType)
			wl := logicalWorkload{
				ID:          "test-wl",
				Type:        wlType,
				Namespace:   "default",
				DurationSec: 1800,
				CPUClass:    cpuC,
				GPUClass:    gpuC,
				Profile:     workloadProfileRec{CPUUtilization: 0.6},
			}
			cfg := defaultTestConfig()
			pods := buildPodsForWorkload(rng, wl, 0, "standard", cfg)
			for _, p := range pods {
				if p.GPURequest != 0 {
					t.Fatalf("CPU-only workload type %s has GPURequest=%d", wlType, p.GPURequest)
				}
			}
		})
	}
}

func TestGPUWorkloadsHavePositiveGPU(t *testing.T) {
	gpuTypes := []string{"debug_eval", "single_gpu_training", "distributed_training", "parameter_server_training", "hpo_experiment"}
	for _, wlType := range gpuTypes {
		t.Run(wlType, func(t *testing.T) {
			rng := rand.New(rand.NewSource(13))
			cpuC, gpuC := classesForType(wlType)
			totalGPUs := 4
			wl := logicalWorkload{
				ID:          "test-wl",
				Type:        wlType,
				Namespace:   "default",
				Gang:        wlType == "distributed_training" || wlType == "parameter_server_training",
				DurationSec: 7200,
				CPUClass:    cpuC,
				GPUClass:    gpuC,
				Profile:     workloadProfileRec{CPUUtilization: 0.4, GPUUtilization: 0.6},
			}
			cfg := defaultTestConfig()
			pods := buildPodsForWorkload(rng, wl, totalGPUs, "standard", cfg)
			hasGPUPod := false
			for _, p := range pods {
				if p.GPURequest > 0 {
					hasGPUPod = true
				}
			}
			if !hasGPUPod {
				t.Fatalf("GPU workload type %s has no pod with GPURequest > 0", wlType)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Profile and duration tests
// ---------------------------------------------------------------------------

func TestSampleDurationSecPositive(t *testing.T) {
	types := []string{
		"cpu_preprocess", "cpu_analytics", "debug_eval",
		"single_gpu_training", "distributed_training",
		"parameter_server_training", "hpo_experiment",
	}
	for _, wlType := range types {
		t.Run(wlType, func(t *testing.T) {
			rng := rand.New(rand.NewSource(55))
			for i := 0; i < 100; i++ {
				d := sampleDurationSec(rng, wlType, 2)
				if d <= 0 {
					t.Fatalf("duration for %s is non-positive: %f", wlType, d)
				}
			}
		})
	}
}

func TestProfileValuesInRange(t *testing.T) {
	rng := rand.New(rand.NewSource(77))
	cfg := defaultTestConfig()
	for i := 0; i < 100; i++ {
		wl := sampleLogicalWorkload(rng, i+1, float64(i*5), cfg)
		p := wl.Profile
		fields := map[string]float64{
			"CPUUtilization":      p.CPUUtilization,
			"GPUUtilization":      p.GPUUtilization,
			"MemoryIntensity":     p.MemoryIntensity,
			"IOIntensity":         p.IOIntensity,
			"CPUFeedIntensityGPU": p.CPUFeedIntensityGPU,
		}
		for name, val := range fields {
			if val < 0 || val > 1 {
				t.Errorf("workload %d (%s): %s = %f out of [0,1]", i, wl.Type, name, val)
			}
		}
	}
}

func TestCPUFeedIntensityZeroWithoutGPU(t *testing.T) {
	rng := rand.New(rand.NewSource(88))
	cfg := defaultTestConfig()
	cfg.GPURatio = 0 // force CPU-only
	for i := 0; i < 50; i++ {
		wl := sampleLogicalWorkload(rng, i+1, float64(i*5), cfg)
		if wl.Profile.CPUFeedIntensityGPU != 0 {
			t.Fatalf("CPU-only workload %s has CPUFeedIntensityGPU=%f", wl.Type, wl.Profile.CPUFeedIntensityGPU)
		}
	}
}

// ---------------------------------------------------------------------------
// Work units tests
// ---------------------------------------------------------------------------

func TestWorkUnitsPositive(t *testing.T) {
	rng := rand.New(rand.NewSource(111))
	cfg := defaultTestConfig()
	for i := 0; i < 50; i++ {
		wl := sampleLogicalWorkload(rng, i+1, float64(i*5), cfg)
		for _, p := range wl.Pods {
			if p.CPURequest > 0 && p.CPUUnits <= 0 {
				t.Errorf("workload %s pod %s: has CPU request but CPUUnits=%f", wl.Type, p.Role, p.CPUUnits)
			}
			if p.GPURequest > 0 && p.GPUUnits <= 0 {
				t.Errorf("workload %s pod %s: has GPU request but GPUUnits=%f", wl.Type, p.Role, p.GPUUnits)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// No stale class names
// ---------------------------------------------------------------------------

func TestNoStaleClassNames(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.Jobs = 50
	records, _ := generateTraceToFile(t, cfg)
	stale := []string{"general", "eco", "batch", "best-effort"}
	for i, r := range records {
		b, _ := json.Marshal(r)
		s := string(b)
		for _, bad := range stale {
			// Check as a standalone value (not substring of joulie.io/power-profile NotIn eco)
			// "eco" can appear in the affinity values list -- that is expected.
			// We check intentClass specifically.
			ic, _ := r["intentClass"].(string)
			if ic == bad {
				t.Fatalf("record %d has stale intentClass %q", i, bad)
			}
		}
		// Also check workloadClass fields
		if wc, ok := r["workloadClass"].(map[string]any); ok {
			for _, v := range wc {
				vs, _ := v.(string)
				for _, bad := range stale {
					if vs == bad {
						t.Fatalf("record %d has stale workloadClass value %q in %s", i, bad, s)
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Gang scheduling metadata tests
// ---------------------------------------------------------------------------

func TestGangSchedulingMetadata(t *testing.T) {
	rng := rand.New(rand.NewSource(200))
	cfg := defaultTestConfig()
	cfg.GPURatio = 1.0 // ensure GPU workloads
	gangTypes := map[string]bool{"distributed_training": true, "parameter_server_training": true}

	for i := 0; i < 200; i++ {
		wl := sampleLogicalWorkload(rng, i+1, float64(i*5), cfg)
		if gangTypes[wl.Type] {
			if !wl.Gang {
				t.Fatalf("workload type %s should have gang=true", wl.Type)
			}
			ord := 0
			jobs := expandLogicalWorkload(wl, cfg, &ord)
			for _, j := range jobs {
				if !j.Gang {
					t.Fatalf("expanded job %s from gang workload %s should have gang=true", j.JobID, wl.Type)
				}
			}
		} else {
			if wl.Gang {
				t.Fatalf("workload type %s should not have gang=true", wl.Type)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// sampleWorkloadType tests
// ---------------------------------------------------------------------------

func TestSampleWorkloadTypeGPU(t *testing.T) {
	validGPU := map[string]bool{
		"debug_eval": true, "single_gpu_training": true,
		"distributed_training": true, "parameter_server_training": true,
		"hpo_experiment": true,
	}
	rng := rand.New(rand.NewSource(5))
	for i := 0; i < 500; i++ {
		wt := sampleWorkloadType(rng, true)
		if !validGPU[wt] {
			t.Fatalf("unexpected GPU workload type: %q", wt)
		}
	}
}

func TestSampleWorkloadTypeCPUOnly(t *testing.T) {
	validCPU := map[string]bool{
		"cpu_preprocess": true, "cpu_analytics": true,
	}
	rng := rand.New(rand.NewSource(5))
	for i := 0; i < 500; i++ {
		wt := sampleWorkloadType(rng, false)
		if !validCPU[wt] {
			t.Fatalf("unexpected CPU-only workload type: %q", wt)
		}
	}
}

// ---------------------------------------------------------------------------
// Format helpers tests
// ---------------------------------------------------------------------------

func TestFormatCPU(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{1.0, "1"},
		{4.0, "4"},
		{1.5, "1.50"},
		{2.75, "2.75"},
		{0.5, "0.50"},
	}
	for _, tc := range tests {
		got := formatCPU(tc.in)
		if got != tc.want {
			t.Errorf("formatCPU(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatMemoryGi(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{1.0, "1Gi"},
		{3.7, "4Gi"},
		{16.0, "16Gi"},
		{0.3, "1Gi"}, // minimum 1
	}
	for _, tc := range tests {
		got := formatMemoryGi(tc.in)
		if got != tc.want {
			t.Errorf("formatMemoryGi(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Trace with EmitWorkloadRecords disabled
// ---------------------------------------------------------------------------

func TestGenerateTraceWithoutWorkloadRecords(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.EmitWorkloadRecords = false
	records, _ := generateTraceToFile(t, cfg)
	for i, r := range records {
		typ, _ := r["type"].(string)
		if typ == "workload" {
			t.Fatalf("record %d: found workload record when EmitWorkloadRecords=false", i)
		}
		if typ != "job" {
			t.Fatalf("record %d: unexpected type %q", i, typ)
		}
	}
}

// ---------------------------------------------------------------------------
// CPU-only trace (GPURatio=0)
// ---------------------------------------------------------------------------

func TestGenerateTraceCPUOnly(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.GPURatio = 0
	records, _ := generateTraceToFile(t, cfg)
	for i, r := range records {
		typ, _ := r["type"].(string)
		if typ != "job" {
			continue
		}
		pt, _ := r["podTemplate"].(map[string]any)
		reqs, _ := pt["requests"].(map[string]any)
		for k := range reqs {
			if strings.Contains(k, "gpu") {
				t.Fatalf("record %d: CPU-only trace has GPU resource key %q", i, k)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// All-GPU trace (GPURatio=1)
// ---------------------------------------------------------------------------

func TestGenerateTraceAllGPU(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.GPURatio = 1.0
	records, _ := generateTraceToFile(t, cfg)
	gpuJobs := 0
	for _, r := range records {
		typ, _ := r["type"].(string)
		if typ != "job" {
			continue
		}
		pt, _ := r["podTemplate"].(map[string]any)
		reqs, _ := pt["requests"].(map[string]any)
		for k := range reqs {
			if strings.Contains(k, "gpu") {
				gpuJobs++
				break
			}
		}
	}
	// Some jobs in multi-pod workloads may be launchers/ps without GPUs, but
	// at least some should have GPUs.
	if gpuJobs == 0 {
		t.Fatal("GPURatio=1 but no job records have GPU resources")
	}
}

// ---------------------------------------------------------------------------
// NoAffinityOnly trace
// ---------------------------------------------------------------------------

func TestGenerateTraceNoAffinityOnly(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.NoAffinityOnly = true
	records, _ := generateTraceToFile(t, cfg)
	for i, r := range records {
		typ, _ := r["type"].(string)
		if typ != "job" {
			continue
		}
		ic, _ := r["intentClass"].(string)
		if ic != "standard" {
			t.Fatalf("record %d: NoAffinityOnly=true but intentClass=%q", i, ic)
		}
		pt, _ := r["podTemplate"].(map[string]any)
		if aff, ok := pt["affinity"]; ok && aff != nil {
			t.Fatalf("record %d: NoAffinityOnly=true but has affinity: %v", i, aff)
		}
	}
}

// ---------------------------------------------------------------------------
// sanitizeName
// ---------------------------------------------------------------------------

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"Worker", "worker"},
		{"my_role", "my-role"},
		{"  Trimmed  ", "trimmed"},
		{"", "pod"},
	}
	for _, tc := range tests {
		got := sanitizeName(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// clamp helpers
// ---------------------------------------------------------------------------

func TestClamp01(t *testing.T) {
	tests := []struct {
		in, want float64
	}{
		{-0.5, 0},
		{0, 0},
		{0.5, 0.5},
		{1.0, 1.0},
		{1.5, 1.0},
	}
	for _, tc := range tests {
		got := clamp01(tc.in)
		if got != tc.want {
			t.Errorf("clamp01(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestClampRange(t *testing.T) {
	tests := []struct {
		v, lo, hi, want float64
	}{
		{5, 1, 10, 5},
		{0, 1, 10, 1},
		{15, 1, 10, 10},
	}
	for _, tc := range tests {
		got := clampRange(tc.v, tc.lo, tc.hi)
		if got != tc.want {
			t.Errorf("clampRange(%v, %v, %v) = %v, want %v", tc.v, tc.lo, tc.hi, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Determinism: same seed yields same trace
// ---------------------------------------------------------------------------

func TestGenerateTraceDeterministic(t *testing.T) {
	cfg := defaultTestConfig()
	records1, _ := generateTraceToFile(t, cfg)
	records2, _ := generateTraceToFile(t, cfg)
	if len(records1) != len(records2) {
		t.Fatalf("different record counts: %d vs %d", len(records1), len(records2))
	}
	for i := range records1 {
		b1, _ := json.Marshal(records1[i])
		b2, _ := json.Marshal(records2[i])
		if string(b1) != string(b2) {
			t.Fatalf("record %d differs between runs:\n  %s\n  %s", i, b1, b2)
		}
	}
}

// ---------------------------------------------------------------------------
// Workload record pods match logical workload
// ---------------------------------------------------------------------------

func TestWorkloadRecordPodsHaveRequests(t *testing.T) {
	rng := rand.New(rand.NewSource(321))
	cfg := defaultTestConfig()
	for i := 0; i < 30; i++ {
		wl := sampleLogicalWorkload(rng, i+1, float64(i*5), cfg)
		rec := workloadToRecord(wl)
		if len(rec.Pods) == 0 {
			t.Fatalf("workload record for %s has no pods", wl.Type)
		}
		for _, pod := range rec.Pods {
			if pod.Role == "" {
				t.Errorf("workload %s: pod has empty role", wl.Type)
			}
			if _, ok := pod.Requests["cpu"]; !ok {
				t.Errorf("workload %s pod %s: missing cpu request", wl.Type, pod.Role)
			}
			if _, ok := pod.Requests["memory"]; !ok {
				t.Errorf("workload %s pod %s: missing memory request", wl.Type, pod.Role)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// JSON round-trip: every record parses cleanly
// ---------------------------------------------------------------------------

func TestGenerateTraceAllRecordsParseAsJSON(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.Jobs = 100
	dir := t.TempDir()
	outFile := filepath.Join(dir, "trace.jsonl")
	setOutputPath(outFile)
	if err := generateTrace(cfg); err != nil {
		t.Fatalf("generateTrace: %v", err)
	}
	f, err := os.Open(outFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		var m map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			t.Fatalf("line %d: invalid JSON: %v", lineNo, err)
		}
	}
	if lineNo == 0 {
		t.Fatal("trace file is empty")
	}
}

// ---------------------------------------------------------------------------
// Tags are non-empty
// ---------------------------------------------------------------------------

func TestTagsForType(t *testing.T) {
	types := []struct {
		wlType    string
		totalGPUs int
	}{
		{"cpu_preprocess", 0},
		{"debug_eval", 1},
		{"distributed_training", 4},
	}
	for _, tc := range types {
		tags := tagsForType(tc.wlType, tc.totalGPUs)
		if len(tags) < 2 {
			t.Errorf("tagsForType(%q, %d) returned fewer than 2 tags: %v", tc.wlType, tc.totalGPUs, tags)
		}
		if tags[0] != "ai" {
			t.Errorf("first tag should be 'ai', got %q", tags[0])
		}
		if tc.totalGPUs > 0 {
			found := false
			for _, tag := range tags {
				if tag == fmt.Sprintf("gpus-%d", tc.totalGPUs) {
					found = true
				}
			}
			if !found {
				t.Errorf("expected gpus-%d tag, got %v", tc.totalGPUs, tags)
			}
		} else {
			found := false
			for _, tag := range tags {
				if tag == "cpu-only" {
					found = true
				}
			}
			if !found {
				t.Errorf("expected cpu-only tag for 0 GPUs, got %v", tags)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Distributed training has launcher + workers
// ---------------------------------------------------------------------------

func TestDistributedTrainingHasLauncherAndWorkers(t *testing.T) {
	rng := rand.New(rand.NewSource(400))
	cpuC, gpuC := classesForType("distributed_training")
	wl := logicalWorkload{
		ID:          "dt-test",
		Type:        "distributed_training",
		Namespace:   "default",
		Gang:        true,
		DurationSec: 7200,
		CPUClass:    cpuC,
		GPUClass:    gpuC,
		Profile:     workloadProfileRec{CPUUtilization: 0.3, GPUUtilization: 0.6},
	}
	cfg := defaultTestConfig()
	pods := buildPodsForWorkload(rng, wl, 4, "standard", cfg)
	hasLauncher, hasWorker := false, false
	for _, p := range pods {
		if p.Role == "launcher" {
			hasLauncher = true
			if p.GPURequest != 0 {
				t.Errorf("launcher should have 0 GPUs, got %d", p.GPURequest)
			}
		}
		if p.Role == "worker" {
			hasWorker = true
			if p.GPURequest < 1 {
				t.Errorf("worker should have >= 1 GPU, got %d", p.GPURequest)
			}
		}
	}
	if !hasLauncher {
		t.Error("distributed_training should have a launcher pod")
	}
	if !hasWorker {
		t.Error("distributed_training should have worker pods")
	}
}

// ---------------------------------------------------------------------------
// Parameter server training has ps + workers
// ---------------------------------------------------------------------------

func TestParameterServerTrainingHasPSAndWorkers(t *testing.T) {
	rng := rand.New(rand.NewSource(401))
	cpuC, gpuC := classesForType("parameter_server_training")
	wl := logicalWorkload{
		ID:          "ps-test",
		Type:        "parameter_server_training",
		Namespace:   "default",
		Gang:        true,
		DurationSec: 7200,
		CPUClass:    cpuC,
		GPUClass:    gpuC,
		Profile:     workloadProfileRec{CPUUtilization: 0.3, GPUUtilization: 0.6},
	}
	cfg := defaultTestConfig()
	pods := buildPodsForWorkload(rng, wl, 4, "standard", cfg)
	hasPS, hasWorker := false, false
	for _, p := range pods {
		if p.Role == "ps" {
			hasPS = true
			if p.GPURequest != 0 {
				t.Errorf("ps should have 0 GPUs, got %d", p.GPURequest)
			}
		}
		if p.Role == "worker" {
			hasWorker = true
			if p.GPURequest < 1 {
				t.Errorf("worker should have >= 1 GPU, got %d", p.GPURequest)
			}
		}
	}
	if !hasPS {
		t.Error("parameter_server_training should have a ps pod")
	}
	if !hasWorker {
		t.Error("parameter_server_training should have worker pods")
	}
}

// ---------------------------------------------------------------------------
// HPO experiment has controller + trials
// ---------------------------------------------------------------------------

func TestHPOExperimentHasControllerAndTrials(t *testing.T) {
	rng := rand.New(rand.NewSource(402))
	cpuC, gpuC := classesForType("hpo_experiment")
	wl := logicalWorkload{
		ID:          "hpo-test",
		Type:        "hpo_experiment",
		Namespace:   "default",
		DurationSec: 3600,
		CPUClass:    cpuC,
		GPUClass:    gpuC,
		Profile:     workloadProfileRec{CPUUtilization: 0.25, GPUUtilization: 0.5},
	}
	cfg := defaultTestConfig()
	pods := buildPodsForWorkload(rng, wl, 2, "standard", cfg)
	hasController, trialCount := false, 0
	for _, p := range pods {
		if p.Role == "controller" {
			hasController = true
			if p.GPURequest != 0 {
				t.Errorf("controller should have 0 GPUs, got %d", p.GPURequest)
			}
		}
		if p.Role == "trial" {
			trialCount++
		}
	}
	if !hasController {
		t.Error("hpo_experiment should have a controller pod")
	}
	if trialCount < 2 {
		t.Errorf("hpo_experiment should have at least 2 trials, got %d", trialCount)
	}
}

// ---------------------------------------------------------------------------
// Hourly arrival multiplier basic properties
// ---------------------------------------------------------------------------

func TestHourlyArrivalMultiplierAllPositive(t *testing.T) {
	for h := 0; h < 24; h++ {
		m := hourlyArrivalMultiplier(h)
		if m <= 0 {
			t.Errorf("hourlyArrivalMultiplier(%d) = %f, should be positive", h, m)
		}
	}
}

// ---------------------------------------------------------------------------
// maxInt
// ---------------------------------------------------------------------------

func TestMaxInt(t *testing.T) {
	tests := []struct{ a, b, want int }{
		{1, 2, 2},
		{5, 3, 5},
		{0, 0, 0},
		{-1, -5, -1},
	}
	for _, tc := range tests {
		got := maxInt(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("maxInt(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
