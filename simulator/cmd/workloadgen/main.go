package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

type jobRecord struct {
	Type                string             `json:"type"`
	SchemaVersion       string             `json:"schemaVersion"`
	JobID               string             `json:"jobId"`
	WorkloadID          string             `json:"workloadId,omitempty"`
	WorkloadType        string             `json:"workloadType,omitempty"`
	PodRole             string             `json:"podRole,omitempty"`
	Gang                bool               `json:"gang,omitempty"`
	SubmitTimeOffsetSec float64            `json:"submitTimeOffsetSec"`
	Namespace           string             `json:"namespace"`
	PodTemplate         podTemplateRec     `json:"podTemplate"`
	Work                workRec            `json:"work"`
	Sensitivity         sensitivityRec     `json:"sensitivity"`
	WorkloadClass       workloadClass      `json:"workloadClass"`
	WorkloadProfile     workloadProfileRec `json:"workloadProfile"`
	Tags                []string           `json:"tags,omitempty"`
}

type workloadRecord struct {
	Type                string             `json:"type"`
	SchemaVersion       string             `json:"schemaVersion"`
	WorkloadID          string             `json:"workloadId"`
	SubmitTimeOffsetSec float64            `json:"submitTimeOffsetSec"`
	Namespace           string             `json:"namespace"`
	WorkloadType        string             `json:"workloadType"`
	Gang                bool               `json:"gang,omitempty"`
	DurationSec         float64            `json:"durationSec"`
	WorkloadClass       workloadClass      `json:"workloadClass"`
	SharedIntensity     workloadProfileRec `json:"sharedIntensityProfile"`
	Pods                []workloadPodRec   `json:"pods"`
	Tags                []string           `json:"tags,omitempty"`
}

type workloadPodRec struct {
	Role      string            `json:"role"`
	Replicas  int               `json:"replicas"`
	Requests  map[string]string `json:"requests"`
	Gang      bool              `json:"gang,omitempty"`
	NodeClass string            `json:"nodeClass,omitempty"`
}

type podTemplateRec struct {
	Affinity map[string]any    `json:"affinity,omitempty"`
	Requests map[string]string `json:"requests"`
}

type workRec struct {
	CPUUnits float64 `json:"cpuUnits"`
	GPUUnits float64 `json:"gpuUnits"`
}

type sensitivityRec struct {
	CPU float64 `json:"cpu"`
	GPU float64 `json:"gpu"`
}

type workloadClass struct {
	CPU string `json:"cpu,omitempty"`
	GPU string `json:"gpu,omitempty"`
}

type workloadProfileRec struct {
	CPUUtilization      float64 `json:"cpuUtilization,omitempty"`
	GPUUtilization      float64 `json:"gpuUtilization,omitempty"`
	MemoryIntensity     float64 `json:"memoryIntensity,omitempty"`
	IOIntensity         float64 `json:"ioIntensity,omitempty"`
	CPUFeedIntensityGPU float64 `json:"cpuFeedIntensityGpu,omitempty"`
}

type generatorConfig struct {
	Jobs                int
	Namespace           string
	MeanInterArrival    float64
	Seed                int64
	PerfRatio           float64
	EcoRatio            float64
	NoAffinityOnly      bool
	GPURatio            float64
	GPURequestPerJob    float64
	CPURatePerCore      float64
	GPURatePerGPU       float64
	EmitWorkloadRecords bool
	BurstDayProbability float64
	BurstMeanJobs       float64
	BurstMultiplier     float64
}

type logicalWorkload struct {
	ID          string
	Type        string
	Class       string
	SubmitSec   float64
	Namespace   string
	Gang        bool
	DurationSec float64
	CPUClass    string
	GPUClass    string
	Profile     workloadProfileRec
	Tags        []string
	Pods        []logicalPod
}

type logicalPod struct {
	Role        string
	Replicas    int
	CPURequest  float64
	MemoryGiB   float64
	GPURequest  int
	GPUResource string
	CPUUnits    float64
	GPUUnits    float64
	CPUSense    float64
	GPUSense    float64
	IntentClass string
}

type arrivalState struct {
	burstHourByDay map[int]int
}

func main() {
	cfg := parseFlags()
	if err := generateTrace(cfg); err != nil {
		panic(err)
	}
}

func parseFlags() generatorConfig {
	cfg := generatorConfig{}
	var outPath string
	flag.IntVar(&cfg.Jobs, "jobs", 50, "number of logical workloads")
	flag.StringVar(&outPath, "out", "trace.jsonl", "output JSONL path")
	flag.StringVar(&cfg.Namespace, "namespace", "default", "namespace for generated workloads")
	flag.Float64Var(&cfg.MeanInterArrival, "mean-inter-arrival-sec", 5, "baseline mean inter-arrival seconds before hourly seasonality")
	flag.Int64Var(&cfg.Seed, "seed", time.Now().UnixNano(), "rng seed")
	flag.Float64Var(&cfg.PerfRatio, "perf-ratio", 0.30, "ratio of performance-constrained workloads")
	flag.Float64Var(&cfg.EcoRatio, "eco-ratio", 0.00, "ratio of eco-constrained workloads")
	flag.BoolVar(&cfg.NoAffinityOnly, "no-affinity-only", false, "if true, do not emit power-profile affinity")
	flag.Float64Var(&cfg.GPURatio, "gpu-ratio", 0.80, "ratio of logical workloads that use GPUs")
	flag.Float64Var(&cfg.GPURequestPerJob, "gpu-request", 1, "GPU request per worker/trial pod when using the legacy single-job mode semantics")
	flag.Float64Var(&cfg.CPURatePerCore, "cpu-work-rate-per-core", 1.0, "CPU work units produced per core-second at full speed")
	flag.Float64Var(&cfg.GPURatePerGPU, "gpu-work-rate-per-gpu", 1.0, "GPU work units produced per GPU-second at full speed")
	flag.BoolVar(&cfg.EmitWorkloadRecords, "emit-workload-records", true, "emit type=workload metadata records in addition to pod-expanded type=job records")
	flag.Float64Var(&cfg.BurstDayProbability, "burst-day-probability", 0.25, "probability of a daily burst window")
	flag.Float64Var(&cfg.BurstMeanJobs, "burst-mean-jobs", 8.0, "mean extra burst intensity used to scale arrival rate during burst windows")
	flag.Float64Var(&cfg.BurstMultiplier, "burst-multiplier", 2.0, "arrival-rate multiplier during the selected burst hour")
	flag.Parse()
	if cfg.MeanInterArrival <= 0 {
		cfg.MeanInterArrival = 1
	}
	if cfg.CPURatePerCore <= 0 {
		cfg.CPURatePerCore = 1
	}
	if cfg.GPURatePerGPU <= 0 {
		cfg.GPURatePerGPU = 1
	}
	if cfg.GPURatio < 0 {
		cfg.GPURatio = 0
	}
	if cfg.GPURatio > 1 {
		cfg.GPURatio = 1
	}
	if cfg.PerfRatio < 0 {
		cfg.PerfRatio = 0
	}
	if cfg.EcoRatio < 0 {
		cfg.EcoRatio = 0
	}
	if cfg.PerfRatio+cfg.EcoRatio > 1 {
		total := cfg.PerfRatio + cfg.EcoRatio
		cfg.PerfRatio /= total
		cfg.EcoRatio /= total
	}
	_ = outPath
	setOutputPath(outPath)
	return cfg
}

var outputPath = "trace.jsonl"

func setOutputPath(v string) { outputPath = v }

func generateTrace(cfg generatorConfig) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	rng := rand.New(rand.NewSource(cfg.Seed))
	arrivals := arrivalState{burstHourByDay: map[int]int{}}
	offset := 0.0
	jobOrdinal := 0
	for i := 0; i < cfg.Jobs; i++ {
		offset = nextArrivalOffset(rng, offset, cfg, &arrivals)
		workload := sampleLogicalWorkload(rng, i+1, offset, cfg)
		if cfg.EmitWorkloadRecords {
			meta := workloadToRecord(workload)
			if err := writeJSONL(w, meta); err != nil {
				return err
			}
		}
		jobs := expandLogicalWorkload(workload, cfg, &jobOrdinal)
		for _, rec := range jobs {
			if err := writeJSONL(w, rec); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeJSONL(w *bufio.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func workloadToRecord(wl logicalWorkload) workloadRecord {
	pods := make([]workloadPodRec, 0, len(wl.Pods))
	for _, p := range wl.Pods {
		requests := map[string]string{
			"cpu":    formatCPU(p.CPURequest),
			"memory": formatMemoryGi(p.MemoryGiB),
		}
		if p.GPURequest > 0 {
			key := p.GPUResource
			if key == "" {
				key = "nvidia.com/gpu"
			}
			requests[key] = strconv.Itoa(p.GPURequest)
		}
		pods = append(pods, workloadPodRec{Role: p.Role, Replicas: p.Replicas, Requests: requests, Gang: wl.Gang})
	}
	return workloadRecord{
		Type:                "workload",
		SchemaVersion:       "v2",
		WorkloadID:          wl.ID,
		SubmitTimeOffsetSec: wl.SubmitSec,
		Namespace:           wl.Namespace,
		WorkloadType:        wl.Type,
		Gang:                wl.Gang,
		DurationSec:         wl.DurationSec,
		WorkloadClass:       workloadClass{CPU: wl.CPUClass, GPU: wl.GPUClass},
		SharedIntensity:     wl.Profile,
		Pods:                pods,
		Tags:                wl.Tags,
	}
}

func expandLogicalWorkload(wl logicalWorkload, cfg generatorConfig, ordinal *int) []jobRecord {
	var out []jobRecord
	roleSeq := map[string]int{}
	for _, p := range wl.Pods {
		for rep := 0; rep < maxInt(1, p.Replicas); rep++ {
			*ordinal++
			roleSeq[p.Role]++
			jobID := fmt.Sprintf("%s-%s-%02d", wl.ID, sanitizeName(p.Role), roleSeq[p.Role])
			requests := map[string]string{
				"cpu":    formatCPU(p.CPURequest),
				"memory": formatMemoryGi(p.MemoryGiB),
			}
			if p.GPURequest > 0 {
				key := p.GPUResource
				if key == "" {
					key = "nvidia.com/gpu"
				}
				requests[key] = strconv.Itoa(p.GPURequest)
			}
			intent := p.IntentClass
			if intent == "" {
				intent = sampleIntentClass(rand.New(rand.NewSource(int64(len(jobID))+13)), cfg, false)
			}
			out = append(out, jobRecord{
				Type:                "job",
				SchemaVersion:       "v2",
				JobID:               jobID,
				WorkloadID:          wl.ID,
				WorkloadType:        wl.Type,
				PodRole:             p.Role,
				Gang:                wl.Gang,
				SubmitTimeOffsetSec: wl.SubmitSec,
				Namespace:           wl.Namespace,
				PodTemplate: podTemplateRec{
					Affinity: affinityForClass(intent),
					Requests: requests,
				},
				Work:            workRec{CPUUnits: p.CPUUnits, GPUUnits: p.GPUUnits},
				Sensitivity:     sensitivityRec{CPU: p.CPUSense, GPU: p.GPUSense},
				WorkloadClass:   workloadClass{CPU: wl.CPUClass, GPU: wl.GPUClass},
				WorkloadProfile: wl.Profile,
				Tags:            append([]string{}, wl.Tags...),
			})
		}
	}
	return out
}

func sampleLogicalWorkload(rng *rand.Rand, idx int, submitSec float64, cfg generatorConfig) logicalWorkload {
	hasGPU := rng.Float64() < cfg.GPURatio
	intent := sampleIntentClass(rng, cfg, cfg.NoAffinityOnly)
	wlType := sampleWorkloadType(rng, hasGPU)
	totalGPUs := 0
	if hasGPU {
		totalGPUs = sampleTotalGPUs(rng)
	}
	cpuClass, gpuClass := classesForType(wlType)
	profile := sampleSharedProfile(rng, wlType, totalGPUs, cpuClass, gpuClass)
	durationSec := sampleDurationSec(rng, wlType, totalGPUs)
	wl := logicalWorkload{
		ID:          fmt.Sprintf("workload-%06d", idx),
		Type:        wlType,
		SubmitSec:   submitSec,
		Namespace:   cfg.Namespace,
		Gang:        wlType == "distributed_training" || wlType == "parameter_server_training",
		DurationSec: durationSec,
		CPUClass:    cpuClass,
		GPUClass:    gpuClass,
		Profile:     profile,
		Tags:        tagsForType(wlType, totalGPUs),
	}
	wl.Pods = buildPodsForWorkload(rng, wl, totalGPUs, intent, cfg)
	return wl
}

func sampleIntentClass(rng *rand.Rand, cfg generatorConfig, noAffinity bool) string {
	if noAffinity {
		return "general"
	}
	p := rng.Float64()
	if p < cfg.PerfRatio {
		return "performance"
	}
	if p < cfg.PerfRatio+cfg.EcoRatio {
		return "eco"
	}
	return "general"
}

func sampleWorkloadType(rng *rand.Rand, hasGPU bool) string {
	if !hasGPU {
		if rng.Float64() < 0.75 {
			return "cpu_preprocess"
		}
		return "cpu_analytics"
	}
	p := rng.Float64()
	switch {
	case p < 0.45:
		return "debug_eval"
	case p < 0.70:
		return "single_gpu_training"
	case p < 0.84:
		return "distributed_training"
	case p < 0.91:
		return "parameter_server_training"
	default:
		return "hpo_experiment"
	}
}

func sampleTotalGPUs(rng *rand.Rand) int {
	p := rng.Float64()
	switch {
	case p < 0.80:
		return 1
	case p < 0.90:
		return 2
	case p < 0.97:
		return 4
	default:
		return 8
	}
}

func classesForType(wlType string) (string, string) {
	switch wlType {
	case "cpu_preprocess":
		return "cpu.memory_bound", ""
	case "cpu_analytics":
		return "cpu.compute_bound", ""
	case "debug_eval":
		return "cpu.mixed", "gpu.memory_bound"
	case "single_gpu_training":
		return "cpu.mixed", "gpu.compute_bound"
	case "distributed_training":
		return "cpu.mixed", "gpu.compute_bound"
	case "parameter_server_training":
		return "cpu.memory_bound", "gpu.compute_bound"
	case "hpo_experiment":
		return "cpu.mixed", "gpu.mixed"
	default:
		return "cpu.mixed", "gpu.mixed"
	}
}

func sampleSharedProfile(rng *rand.Rand, wlType string, totalGPUs int, cpuClass, gpuClass string) workloadProfileRec {
	gpuMean := gpuUtilMeanBySize(totalGPUs)
	switch wlType {
	case "debug_eval":
		gpuMean -= 0.10
	case "distributed_training":
		gpuMean -= 0.08
	case "parameter_server_training":
		gpuMean -= 0.10
	case "hpo_experiment":
		gpuMean -= 0.05
	}
	gpuMean = clamp01(gpuMean)
	cpuUtil := 0.0
	switch wlType {
	case "cpu_preprocess":
		cpuUtil = sampleRange(rng, 0.50, 0.75)
	case "cpu_analytics":
		cpuUtil = sampleRange(rng, 0.75, 0.95)
	case "debug_eval":
		cpuUtil = sampleRange(rng, 0.20, 0.40)
	case "single_gpu_training":
		cpuUtil = sampleRange(rng, 0.20, 0.45)
	case "distributed_training":
		cpuUtil = sampleRange(rng, 0.15, 0.35)
	case "parameter_server_training":
		cpuUtil = sampleRange(rng, 0.20, 0.40)
	case "hpo_experiment":
		cpuUtil = sampleRange(rng, 0.15, 0.35)
	default:
		cpuUtil = defaultCPUUtilization(cpuClass, rng)
	}
	gpuUtil := 0.0
	if totalGPUs > 0 {
		spread := 0.12
		if totalGPUs >= 4 {
			spread = 0.16
		}
		gpuUtil = clamp01(gpuMean + (rng.Float64()*2-1)*spread)
	}
	memoryIntensity := sampleMemoryIntensity(rng, wlType, cpuClass, gpuClass)
	ioIntensity := sampleIOIntensity(rng, wlType, cpuClass)
	cpuFeed := 0.0
	if totalGPUs > 0 {
		cpuFeed = sampleCPUFeedIntensity(rng, wlType)
	}
	return workloadProfileRec{
		CPUUtilization:      clamp01(cpuUtil),
		GPUUtilization:      clamp01(gpuUtil),
		MemoryIntensity:     clamp01(memoryIntensity),
		IOIntensity:         clamp01(ioIntensity),
		CPUFeedIntensityGPU: clamp01(cpuFeed),
	}
}

func sampleDurationSec(rng *rand.Rand, wlType string, totalGPUs int) float64 {
	switch wlType {
	case "debug_eval":
		return clampRange(logNormalApprox(rng, math.Log(206), 1.0), 30, 1000)
	case "single_gpu_training":
		return clampRange(logNormalApprox(rng, math.Log(7200), 1.0), 900, 7*24*3600)
	case "distributed_training":
		base := logNormalApprox(rng, math.Log(4*3600), 1.2)
		if totalGPUs >= 4 {
			base *= 1.2
		}
		return clampRange(base, 1200, 14*24*3600)
	case "parameter_server_training":
		return clampRange(logNormalApprox(rng, math.Log(3*3600), 1.1), 900, 7*24*3600)
	case "hpo_experiment":
		return clampRange(logNormalApprox(rng, math.Log(2*3600), 1.0), 1200, 3*24*3600)
	case "cpu_preprocess":
		return clampRange(logNormalApprox(rng, math.Log(900), 0.8), 120, 8*3600)
	case "cpu_analytics":
		return clampRange(logNormalApprox(rng, math.Log(1800), 0.9), 300, 24*3600)
	default:
		if rng.Float64() < 0.75 {
			return clampRange(logNormalApprox(rng, math.Log(206), 1.0), 30, 1000)
		}
		return clampRange(logNormalApprox(rng, math.Log(2*3600), 1.5), 600, 14*24*3600)
	}
}

func buildPodsForWorkload(rng *rand.Rand, wl logicalWorkload, totalGPUs int, intent string, cfg generatorConfig) []logicalPod {
	duration := wl.DurationSec
	profile := wl.Profile
	gpuResource := defaultGPUResourceName(totalGPUs)
	pods := []logicalPod{}
	appendWorker := func(role string, replicas int, gpuEach int, cpuEach float64, memEach float64, cpuFrac float64, gpuFrac float64) {
		cpuUnits := duration * cpuEach * math.Max(0.10, profile.CPUUtilization) * cfg.CPURatePerCore * cpuFrac
		gpuUnits := duration * float64(gpuEach) * math.Max(0.10, profile.GPUUtilization) * cfg.GPURatePerGPU * gpuFrac
		pods = append(pods, logicalPod{
			Role:        role,
			Replicas:    replicas,
			CPURequest:  cpuEach,
			MemoryGiB:   memEach,
			GPURequest:  gpuEach,
			GPUResource: gpuResource,
			CPUUnits:    cpuUnits,
			GPUUnits:    gpuUnits,
			CPUSense:    cpuSensitivityForType(wl.Type, role),
			GPUSense:    gpuSensitivityForType(wl.Type),
			IntentClass: intent,
		})
	}

	switch wl.Type {
	case "cpu_preprocess":
		appendWorker("preprocess", 1, 0, sampleCPUPerGPU(rng, wl.Type, 0), sampleMemoryPerGPU(rng, wl.Type, 0), 1.0, 0)
	case "cpu_analytics":
		appendWorker("analytics", 1, 0, sampleCPUPerGPU(rng, wl.Type, 0), sampleMemoryPerGPU(rng, wl.Type, 0), 1.0, 0)
	case "debug_eval":
		appendWorker("worker", 1, maxInt(1, totalGPUs), sampleCPUPerGPU(rng, wl.Type, totalGPUs), sampleMemoryPerGPU(rng, wl.Type, totalGPUs), 1.0, 1.0)
	case "single_gpu_training":
		appendWorker("worker", 1, maxInt(1, totalGPUs), sampleCPUPerGPU(rng, wl.Type, totalGPUs), sampleMemoryPerGPU(rng, wl.Type, totalGPUs), 1.0, 1.0)
	case "distributed_training":
		workers := maxInt(2, totalGPUs)
		if workers > totalGPUs {
			workers = totalGPUs
		}
		cpuPerWorker := sampleCPUPerGPU(rng, wl.Type, 1)
		memPerWorker := sampleMemoryPerGPU(rng, wl.Type, 1)
		appendWorker("launcher", 1, 0, clampRange(cpuPerWorker*0.5, 1, 4), clampRange(memPerWorker*0.4, 1, 8), 0.25, 0)
		for i := 0; i < workers; i++ {
			appendWorker("worker", 1, 1, cpuPerWorker, memPerWorker, 0.9/float64(workers), 1.0/float64(workers))
		}
	case "parameter_server_training":
		workers := maxInt(2, totalGPUs)
		if workers > totalGPUs {
			workers = totalGPUs
		}
		psCount := 1
		if workers >= 4 {
			psCount = 2
		}
		cpuPerWorker := sampleCPUPerGPU(rng, wl.Type, 1)
		memPerWorker := sampleMemoryPerGPU(rng, wl.Type, 1)
		for i := 0; i < psCount; i++ {
			appendWorker("ps", 1, 0, clampRange(cpuPerWorker*0.75, 1, 6), clampRange(memPerWorker, 2, 16), 0.35/float64(psCount), 0)
		}
		for i := 0; i < workers; i++ {
			appendWorker("worker", 1, 1, cpuPerWorker, memPerWorker, 0.85/float64(workers), 1.0/float64(workers))
		}
	case "hpo_experiment":
		trials := sampleHPOTrialCount(rng, totalGPUs)
		appendWorker("controller", 1, 0, 1, 1, 0.10, 0)
		for i := 0; i < trials; i++ {
			gpuEach := 0
			if totalGPUs > 0 {
				gpuEach = 1
			}
			appendWorker("trial", 1, gpuEach, sampleCPUPerGPU(rng, wl.Type, maxInt(1, gpuEach)), sampleMemoryPerGPU(rng, wl.Type, maxInt(1, gpuEach)), 0.90/float64(trials), 1.0/float64(maxInt(1, trials)))
		}
	default:
		appendWorker("worker", 1, maxInt(0, totalGPUs), sampleCPUPerGPU(rng, wl.Type, totalGPUs), sampleMemoryPerGPU(rng, wl.Type, totalGPUs), 1.0, 1.0)
	}
	return pods
}

func sampleCPUPerGPU(rng *rand.Rand, wlType string, g int) float64 {
	if g <= 0 {
		switch wlType {
		case "cpu_preprocess":
			return clampRange(logNormalApprox(rng, math.Log(4), 0.35), 1, 16)
		case "cpu_analytics":
			return clampRange(logNormalApprox(rng, math.Log(8), 0.4), 2, 32)
		default:
			return clampRange(logNormalApprox(rng, math.Log(2), 0.4), 1, 8)
		}
	}
	median := 4.0
	switch wlType {
	case "debug_eval":
		median = 2.0
	case "single_gpu_training":
		median = 4.0
	case "distributed_training":
		median = 4.0
	case "parameter_server_training":
		median = 5.0
	case "hpo_experiment":
		median = 2.0
	}
	return clampRange(logNormalApprox(rng, math.Log(median), 0.35), 1, 16)
}

func sampleMemoryPerGPU(rng *rand.Rand, wlType string, g int) float64 {
	base := 2.0
	perGPU := 8.0
	switch wlType {
	case "debug_eval":
		perGPU = 4.0
	case "single_gpu_training":
		perGPU = 10.0
	case "distributed_training":
		perGPU = 12.0
	case "parameter_server_training":
		perGPU = 14.0
	case "hpo_experiment":
		perGPU = 6.0
	case "cpu_preprocess":
		return clampRange(logNormalApprox(rng, math.Log(6), 0.45), 1, 64)
	case "cpu_analytics":
		return clampRange(logNormalApprox(rng, math.Log(10), 0.5), 2, 128)
	}
	v := base + float64(maxInt(1, g))*clampRange(logNormalApprox(rng, math.Log(perGPU), 0.35), 2, 32)
	return clampRange(v, 1, 256)
}

func cpuSensitivityForType(wlType, role string) float64 {
	switch {
	case role == "ps":
		return 0.45
	case role == "launcher" || role == "controller":
		return 0.25
	case wlType == "cpu_preprocess":
		return 0.75
	case wlType == "cpu_analytics":
		return 0.90
	default:
		return 0.65
	}
}

func gpuSensitivityForType(wlType string) float64 {
	switch wlType {
	case "debug_eval":
		return 0.55
	case "hpo_experiment":
		return 0.65
	case "single_gpu_training":
		return 0.85
	case "distributed_training", "parameter_server_training":
		return 0.90
	default:
		return 1.0
	}
}

func gpuUtilMeanBySize(totalGPUs int) float64 {
	switch {
	case totalGPUs <= 0:
		return 0
	case totalGPUs == 1:
		return 0.5238
	case totalGPUs <= 2:
		return 0.50
	case totalGPUs <= 4:
		return 0.4518
	case totalGPUs <= 8:
		return 0.5899
	default:
		return 0.4039
	}
}

func sampleMemoryIntensity(rng *rand.Rand, wlType, cpuClass, gpuClass string) float64 {
	switch wlType {
	case "cpu_preprocess":
		return sampleRange(rng, 0.70, 0.92)
	case "parameter_server_training":
		return sampleRange(rng, 0.70, 0.92)
	case "debug_eval":
		return sampleRange(rng, 0.55, 0.85)
	case "distributed_training":
		return sampleRange(rng, 0.45, 0.75)
	case "single_gpu_training":
		return sampleRange(rng, 0.30, 0.60)
	case "hpo_experiment":
		return sampleRange(rng, 0.35, 0.70)
	case "cpu_analytics":
		return sampleRange(rng, 0.20, 0.45)
	default:
		return defaultMemoryIntensity(cpuClass, gpuClass, rng)
	}
}

func sampleIOIntensity(rng *rand.Rand, wlType, cpuClass string) float64 {
	switch wlType {
	case "cpu_preprocess":
		return sampleRange(rng, 0.10, 0.30)
	case "debug_eval":
		return sampleRange(rng, 0.05, 0.20)
	default:
		return defaultIOIntensity(cpuClass, rng)
	}
}

func sampleCPUFeedIntensity(rng *rand.Rand, wlType string) float64 {
	switch wlType {
	case "debug_eval":
		return sampleRange(rng, 0.15, 0.35)
	case "single_gpu_training":
		return sampleRange(rng, 0.25, 0.55)
	case "distributed_training":
		return sampleRange(rng, 0.35, 0.70)
	case "parameter_server_training":
		return sampleRange(rng, 0.40, 0.75)
	case "hpo_experiment":
		return sampleRange(rng, 0.20, 0.45)
	default:
		return sampleRange(rng, 0.20, 0.50)
	}
}

func sampleHPOTrialCount(rng *rand.Rand, totalGPUs int) int {
	maxTrials := 4
	if totalGPUs >= 4 {
		maxTrials = 8
	}
	if totalGPUs <= 0 {
		maxTrials = 6
	}
	return 2 + rng.Intn(maxTrials-1)
}

func nextArrivalOffset(rng *rand.Rand, current float64, cfg generatorConfig, st *arrivalState) float64 {
	hour := hourOfOffset(current)
	day := dayOfOffset(current)
	mult := hourlyArrivalMultiplier(hour)
	if isBurstHour(rng, st, day, hour, cfg) {
		mult *= cfg.BurstMultiplier
		mult += cfg.BurstMeanJobs / 16.0
	}
	if mult <= 0.05 {
		mult = 0.05
	}
	delta := rng.ExpFloat64() * cfg.MeanInterArrival / mult
	return current + delta
}

func hourlyArrivalMultiplier(hour int) float64 {
	switch {
	case hour >= 0 && hour < 8:
		return 0.70
	case hour == 12 || hour == 18:
		return 0.85
	case hour >= 9 && hour <= 11:
		return 1.20
	case hour >= 13 && hour <= 17:
		return 1.15
	case hour >= 20 && hour <= 22:
		return 1.00
	default:
		return 0.95
	}
}

func isBurstHour(rng *rand.Rand, st *arrivalState, day, hour int, cfg generatorConfig) bool {
	burstHour, ok := st.burstHourByDay[day]
	if !ok {
		if rng.Float64() < cfg.BurstDayProbability {
			candidates := []int{10, 11, 14, 15, 16}
			burstHour = candidates[rng.Intn(len(candidates))]
		} else {
			burstHour = -1
		}
		st.burstHourByDay[day] = burstHour
	}
	return hour == burstHour
}

func dayOfOffset(offset float64) int  { return int(math.Floor(offset / 86400.0)) }
func hourOfOffset(offset float64) int { return int(math.Floor(math.Mod(offset, 86400.0) / 3600.0)) }

func defaultGPUResourceName(totalGPUs int) string {
	if totalGPUs <= 0 {
		return ""
	}
	return "nvidia.com/gpu"
}

func tagsForType(wlType string, totalGPUs int) []string {
	tags := []string{"ai", strings.ReplaceAll(wlType, "_", "-")}
	if totalGPUs > 0 {
		tags = append(tags, fmt.Sprintf("gpus-%d", totalGPUs))
	} else {
		tags = append(tags, "cpu-only")
	}
	return tags
}

func formatCPU(v float64) string {
	if math.Abs(v-math.Round(v)) < 1e-9 {
		return strconv.Itoa(int(math.Round(v)))
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func formatMemoryGi(v float64) string {
	return fmt.Sprintf("%dGi", int(math.Max(1, math.Round(v))))
}

func sanitizeName(in string) string {
	in = strings.ToLower(strings.TrimSpace(in))
	if in == "" {
		return "pod"
	}
	in = strings.ReplaceAll(in, "_", "-")
	return in
}

func sampleRange(rng *rand.Rand, lo, hi float64) float64 {
	if hi < lo {
		lo, hi = hi, lo
	}
	return lo + rng.Float64()*(hi-lo)
}

func clampRange(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clamp01(v float64) float64 { return clampRange(v, 0, 1) }

func logNormalApprox(rng *rand.Rand, mu, sigma float64) float64 {
	return math.Exp(mu + sigma*rng.NormFloat64())
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func randomCPUWorkClass(rng *rand.Rand) string {
	p := rng.Float64()
	switch {
	case p < 0.45:
		return "cpu.compute_bound"
	case p < 0.75:
		return "cpu.memory_bound"
	case p < 0.90:
		return "cpu.io_bound"
	default:
		return "cpu.mixed"
	}
}

func randomGPUWorkClass(rng *rand.Rand) string {
	if rng.Float64() < 0.7 {
		return "gpu.compute_bound"
	}
	if rng.Float64() < 0.85 {
		return "gpu.memory_bound"
	}
	return "gpu.mixed"
}

func defaultCPUUtilization(class string, rng *rand.Rand) float64 {
	switch class {
	case "cpu.compute_bound":
		return 0.85 + rng.Float64()*0.12
	case "cpu.memory_bound":
		return 0.45 + rng.Float64()*0.25
	case "cpu.io_bound":
		return 0.10 + rng.Float64()*0.20
	default:
		return 0.45 + rng.Float64()*0.35
	}
}

func defaultGPUUtilization(class string, hasGPU bool, rng *rand.Rand) float64 {
	if !hasGPU {
		return 0
	}
	switch class {
	case "gpu.compute_bound":
		return 0.85 + rng.Float64()*0.12
	case "gpu.memory_bound":
		return 0.55 + rng.Float64()*0.20
	default:
		return 0.60 + rng.Float64()*0.25
	}
}

func defaultMemoryIntensity(cpuClass, gpuClass string, rng *rand.Rand) float64 {
	switch {
	case cpuClass == "cpu.memory_bound" || gpuClass == "gpu.memory_bound":
		return 0.75 + rng.Float64()*0.20
	case cpuClass == "cpu.io_bound":
		return 0.20 + rng.Float64()*0.20
	default:
		return 0.35 + rng.Float64()*0.35
	}
}

func defaultIOIntensity(cpuClass string, rng *rand.Rand) float64 {
	if cpuClass == "cpu.io_bound" {
		return 0.75 + rng.Float64()*0.20
	}
	return 0.05 + rng.Float64()*0.20
}

func defaultCPUFeedIntensity(hasGPU bool, rng *rand.Rand) float64 {
	if !hasGPU {
		return 0
	}
	return 0.20 + rng.Float64()*0.50
}

func affinityForClass(class string) map[string]any {
	switch class {
	case "performance":
		return map[string]any{
			"nodeAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
					"nodeSelectorTerms": []map[string]any{{
						"matchExpressions": []map[string]any{{
							"key":      "joulie.io/power-profile",
							"operator": "NotIn",
							"values":   []string{"eco"},
						}},
					}},
				},
			},
		}
	case "eco":
		return map[string]any{
			"nodeAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
					"nodeSelectorTerms": []map[string]any{{
						"matchExpressions": []map[string]any{{
							"key":      "joulie.io/power-profile",
							"operator": "In",
							"values":   []string{"eco"},
						}, {
							"key":      "joulie.io/draining",
							"operator": "In",
							"values":   []string{"false"},
						}},
					}},
				},
			},
		}
	default:
		return nil
	}
}
