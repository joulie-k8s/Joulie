// Command workloadgen writes a synthetic workload trace for the simulator:
// JSON Lines with one type=workload record per logical workload and one
// type=job record per pod.
//
// -schema-version selects the trace schema. v3, the default, writes no
// sensitivity unless -cpu-sensitivity or -gpu-sensitivity is given and no
// power-profile affinity, and sizes every pod's work so that its ideal
// runtime, work over request times -base-speed-per-core, equals its
// workload's durationSec. v2 is the schema experiments 01 to 03 were measured
// with: its output is byte-identical to the generator before v3, and every
// legacy caller pins it.
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

	"github.com/matbun/joulie/simulator/pkg/workload"
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
	IntentClass         string             `json:"intentClass"`
	PodTemplate         podTemplateRec     `json:"podTemplate"`
	Work                workRec            `json:"work"`
	Sensitivity         *sensitivityRec    `json:"sensitivity,omitempty"`
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
	IntentClass         string             `json:"intentClass"`
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

// sensitivityRec is how strongly a job's progress follows a cap, per
// resource, in [0,1]. v2 writes it on every job ({0, 0} in default mode).
// v3 omits it, and the simulator then reads 1, unless -cpu-sensitivity or
// -gpu-sensitivity is given; it then holds only the keys given. The
// scheduler does not read it: it uses only joulie.io/workload-class.
type sensitivityRec struct {
	CPU *float64 `json:"cpu,omitempty"`
	GPU *float64 `json:"gpu,omitempty"`
}

type workloadClass struct {
	CPU string `json:"cpu,omitempty"`
	GPU string `json:"gpu,omitempty"`
}

// workloadProfileRec is a job's workloadProfile. PeerTransferIntensity is
// written only with -peer-transfer-intensity (v3), on the jobs that request
// two or more GPUs.
type workloadProfileRec struct {
	CPUUtilization        float64 `json:"cpuUtilization,omitempty"`
	GPUUtilization        float64 `json:"gpuUtilization,omitempty"`
	MemoryIntensity       float64 `json:"memoryIntensity,omitempty"`
	IOIntensity           float64 `json:"ioIntensity,omitempty"`
	CPUFeedIntensityGPU   float64 `json:"cpuFeedIntensityGpu,omitempty"`
	PeerTransferIntensity float64 `json:"peerTransferIntensity,omitempty"`
}

type generatorConfig struct {
	Jobs                  int
	Namespace             string
	MeanInterArrival      float64
	Seed                  int64
	PerfRatio             float64
	ComputeBoundPerfBoost float64
	NoAffinityOnly        bool
	GPURatio              float64
	CPURatePerCore        float64 // v2 only
	GPURatePerGPU         float64 // v2 only
	EmitWorkloadRecords   bool
	BurstDayProbability   float64
	BurstMeanJobs         float64
	BurstMultiplier       float64
	DipDayProbability     float64
	DipMultiplier         float64
	TimeScale             float64
	Mode                  string   // "default" or "diurnal"
	SimHours              float64  // total sim-hours for diurnal mode
	WorkScale             float64  // work unit multiplier (v2 diurnal mode only)
	BaseSpeedPerCore      float64  // work units per core-second; see baseSpeedPerCore
	DiurnalBurstMin       int      // min burst size in diurnal mode
	DiurnalBurstMax       int      // max burst size in diurnal mode
	DiurnalPeakRate       float64  // peak arrival rate (jobs/min in sim-time)
	MinGPURequest         int      // minimum GPUs per GPU job
	AMDGPURatio           float64  // fraction of GPU workloads using amd.com/gpu (rest use nvidia.com/gpu)
	SchemaVersion         string   // workload.TraceSchemaV3 (also when empty) or TraceSchemaV2
	CPUSensitivity        *float64 // v3 only: sensitivity.cpu of every job; nil omits it
	GPUSensitivity        *float64 // v3 only: sensitivity.gpu of every job; nil omits it
	ArrivalWindowSec      float64  // v3 only: the last arrival is at most this; 0 keeps the offsets
	MaxDurationSec        float64  // v3 only: durationSec is at most this; 0 keeps the durations
	PeerTransferIntensity float64  // v3 only: peerTransferIntensity of every job with two or more GPUs; 0 omits it
}

// v3BaseSpeedPerCore is the v3 default of -base-speed-per-core. It equals the
// simulator's SIM_BASE_SPEED_PER_CORE default, so a v3 trace's ideal
// runtimes hold on a simulator deployed with defaults.
const v3BaseSpeedPerCore = 1.0

// v2DiurnalBaseSpeedPerCore is the v2 default, read only by diurnal mode.
const v2DiurnalBaseSpeedPerCore = 2.0

// v3 reports whether cfg writes schema v3, the default, which an empty
// SchemaVersion also selects.
func (c generatorConfig) v3() bool { return c.SchemaVersion != workload.TraceSchemaV2 }

// schema is the schemaVersion every record carries.
func (c generatorConfig) schema() string {
	if c.v3() {
		return workload.TraceSchemaV3
	}
	return workload.TraceSchemaV2
}

// baseSpeedPerCore is -base-speed-per-core, or the schema's default when it
// is not positive: 1.0 in v3, 2.0 in v2, where only diurnal mode reads it.
func (c generatorConfig) baseSpeedPerCore() float64 {
	switch {
	case c.BaseSpeedPerCore > 0:
		return c.BaseSpeedPerCore
	case c.v3():
		return v3BaseSpeedPerCore
	default:
		return v2DiurnalBaseSpeedPerCore
	}
}

// jobProfile is the workloadProfile of a job requesting gpus GPUs: the
// workload's shared profile, plus -peer-transfer-intensity when the job
// requests two or more GPUs, the jobs that can move data between their
// GPUs on one node.
func (c generatorConfig) jobProfile(shared workloadProfileRec, gpus int) workloadProfileRec {
	if gpus >= 2 {
		shared.PeerTransferIntensity = c.PeerTransferIntensity
	}
	return shared
}

// sensitivity is a job's sensitivity record. v2 writes both values given;
// v3 writes only the -cpu-sensitivity and -gpu-sensitivity flags given, and
// without them returns nil, which omits the key.
func (c generatorConfig) sensitivity(cpu, gpu float64) *sensitivityRec {
	if c.v3() {
		if c.CPUSensitivity == nil && c.GPUSensitivity == nil {
			return nil
		}
		return &sensitivityRec{CPU: c.CPUSensitivity, GPU: c.GPUSensitivity}
	}
	return &sensitivityRec{CPU: &cpu, GPU: &gpu}
}

// affinity is a default-mode job's pod affinity: the class's NotIn-eco rule
// in v2, none in v3, whose consumers declare the class by annotation.
func (c generatorConfig) affinity(class string) map[string]any {
	if c.v3() {
		return nil
	}
	return affinityForClass(class)
}

// clampDuration applies -max-duration-sec to a sampled durationSec.
func (c generatorConfig) clampDuration(d float64) float64 {
	if c.MaxDurationSec > 0 && d > c.MaxDurationSec {
		return c.MaxDurationSec
	}
	return d
}

// v3Work sizes one pod's work so that its ideal runtime, units over request
// times base speed (the simulator's rate at full speed), equals durationSec.
// The CPU request is the one formatCPU writes, so the simulator reads back
// the request the units were sized with.
func v3Work(durationSec, cpuRequest float64, gpus int, baseSpeed float64) (cpuUnits, gpuUnits float64) {
	cpu, err := strconv.ParseFloat(formatCPU(cpuRequest), 64)
	if err != nil {
		cpu = cpuRequest
	}
	return durationSec * cpu * baseSpeed, durationSec * float64(gpus) * baseSpeed
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
	IntentClass string
}

type arrivalState struct {
	burstHourByDay map[int]int
	dipHourByDay   map[int]int
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "workloadgen:", err)
		os.Exit(2)
	}
}

// run parses args and writes the trace.
func run(args []string) error {
	cfg, err := parseFlags(args)
	if err != nil {
		return err
	}
	if cfg.Mode == "diurnal" {
		return generateDiurnalTrace(cfg)
	}
	return generateTrace(cfg)
}

// v3OnlyFlags shape only v3 output and v2OnlyFlags only v2 output. Giving
// one with the other schema is an error rather than a flag that does
// nothing, or one that breaks v3's ideal runtime.
var (
	v3OnlyFlags = []string{"cpu-sensitivity", "gpu-sensitivity", "arrival-window-sec", "max-duration-sec", "peer-transfer-intensity"}
	v2OnlyFlags = []string{"cpu-work-rate-per-core", "gpu-work-rate-per-gpu", "work-scale"}
)

// parseFlags reads the command line. A malformed flag exits 2 and -h exits
// 0, as the flag package does; a value the schema rejects is an error.
func parseFlags(args []string) (generatorConfig, error) {
	cfg := generatorConfig{}
	fs := flag.NewFlagSet("workloadgen", flag.ExitOnError)
	var outPath string
	var cpuSensitivity, gpuSensitivity float64
	fs.IntVar(&cfg.Jobs, "jobs", 50, "number of logical workloads")
	fs.StringVar(&outPath, "out", "trace.jsonl", "output JSONL path")
	fs.StringVar(&cfg.Namespace, "namespace", "default", "namespace for generated workloads")
	fs.Float64Var(&cfg.MeanInterArrival, "mean-inter-arrival-sec", 5, "baseline mean inter-arrival seconds before hourly seasonality")
	fs.Int64Var(&cfg.Seed, "seed", time.Now().UnixNano(), "rng seed")
	fs.Float64Var(&cfg.PerfRatio, "perf-ratio", 0.30, "base ratio of performance-constrained workloads")
	fs.Float64Var(&cfg.ComputeBoundPerfBoost, "compute-bound-perf-boost", 3.5, "multiplier for performance probability of compute-bound workloads")
	fs.BoolVar(&cfg.NoAffinityOnly, "no-affinity-only", false, "if true, do not emit power-profile affinity")
	fs.Float64Var(&cfg.GPURatio, "gpu-ratio", 0.80, "ratio of logical workloads that use GPUs")
	fs.Float64Var(&cfg.CPURatePerCore, "cpu-work-rate-per-core", 1.0, "v2 only: CPU work units produced per core-second at full speed (default mode)")
	fs.Float64Var(&cfg.GPURatePerGPU, "gpu-work-rate-per-gpu", 1.0, "v2 only: GPU work units produced per GPU-second at full speed (default mode)")
	fs.BoolVar(&cfg.EmitWorkloadRecords, "emit-workload-records", true, "emit type=workload metadata records in addition to pod-expanded type=job records")
	fs.Float64Var(&cfg.BurstDayProbability, "burst-day-probability", 0.25, "probability of a daily burst window")
	fs.Float64Var(&cfg.BurstMeanJobs, "burst-mean-jobs", 8.0, "mean extra burst intensity used to scale arrival rate during burst windows")
	fs.Float64Var(&cfg.BurstMultiplier, "burst-multiplier", 2.0, "arrival-rate multiplier during the selected burst hour")
	fs.Float64Var(&cfg.DipDayProbability, "dip-day-probability", 0.30, "probability of a daily dip (negative spike) window")
	fs.Float64Var(&cfg.DipMultiplier, "dip-multiplier", 0.08, "arrival-rate multiplier during the selected dip hour (e.g., 0.08 = 92% reduction)")
	fs.Float64Var(&cfg.TimeScale, "time-scale", 1.0, "simulation time scale: offset*timeScale = simulated seconds (used for day/night hour calculation)")
	fs.StringVar(&cfg.Mode, "mode", "default", "trace generation mode: 'default' (original) or 'diurnal' (exp04-style NHPP cosine day/night)")
	fs.Float64Var(&cfg.SimHours, "sim-hours", 48, "total simulation hours (diurnal mode only)")
	fs.Float64Var(&cfg.WorkScale, "work-scale", 1.0, "v2 only: work unit multiplier (diurnal mode only)")
	fs.Float64Var(&cfg.BaseSpeedPerCore, "base-speed-per-core", 0, "work units one core, or one GPU, does per second at full speed; it sizes work so the ideal runtime is durationSec. v3: both modes, default 1.0 (the simulator's SIM_BASE_SPEED_PER_CORE default). v2: diurnal mode only, default 2.0")
	fs.IntVar(&cfg.DiurnalBurstMin, "diurnal-burst-min", 20, "min burst size in diurnal mode")
	fs.IntVar(&cfg.DiurnalBurstMax, "diurnal-burst-max", 80, "max burst size in diurnal mode")
	fs.Float64Var(&cfg.DiurnalPeakRate, "diurnal-peak-rate", 80.0, "peak arrival rate in jobs/min sim-time (diurnal mode)")
	fs.IntVar(&cfg.MinGPURequest, "min-gpu-request", 0, "minimum GPUs per GPU job (0 = use default sampling)")
	fs.Float64Var(&cfg.AMDGPURatio, "amd-gpu-ratio", 0.0, "fraction of GPU workloads using amd.com/gpu (0.0 = all nvidia, 1.0 = all amd)")
	fs.StringVar(&cfg.SchemaVersion, "schema-version", workload.TraceSchemaV3, "trace schema: v3, or v2, the schema experiments 01 to 03 were measured with, written byte for byte as before v3")
	fs.Float64Var(&cpuSensitivity, "cpu-sensitivity", 0, "v3 only: sensitivity.cpu, in [0,1], of every job; unset, the key is absent and the simulator uses 1")
	fs.Float64Var(&gpuSensitivity, "gpu-sensitivity", 0, "v3 only: sensitivity.gpu, in [0,1], of every job; unset, the key is absent and the simulator uses 1")
	fs.Float64Var(&cfg.ArrivalWindowSec, "arrival-window-sec", 0, "v3 only: scale submit offsets linearly so the last arrival is at most this many seconds (0 keeps them)")
	fs.Float64Var(&cfg.MaxDurationSec, "max-duration-sec", 0, "v3 only: clamp each workload's durationSec to this many seconds before its work is sized (0 keeps them)")
	fs.Float64Var(&cfg.PeerTransferIntensity, "peer-transfer-intensity", 0, "v3 only: workloadProfile.peerTransferIntensity, in [0,1], of every job that requests two or more GPUs: the share of its GPU time spent moving data between its GPUs (0 omits the key)")
	_ = fs.Parse(args) // ExitOnError: never returns an error
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	switch cfg.SchemaVersion {
	case workload.TraceSchemaV2:
		for _, name := range v3OnlyFlags {
			if given[name] {
				return cfg, fmt.Errorf("-%s needs -schema-version %s", name, workload.TraceSchemaV3)
			}
		}
	case workload.TraceSchemaV3:
		for _, name := range v2OnlyFlags {
			if given[name] {
				return cfg, fmt.Errorf("-%s is %s only: %s sizes work with -base-speed-per-core", name, workload.TraceSchemaV2, workload.TraceSchemaV3)
			}
		}
		if given["base-speed-per-core"] && !(cfg.BaseSpeedPerCore > 0) {
			return cfg, fmt.Errorf("-base-speed-per-core %v: must be positive", cfg.BaseSpeedPerCore)
		}
		for _, s := range []struct {
			name  string
			value float64
			dst   **float64
		}{
			{"cpu-sensitivity", cpuSensitivity, &cfg.CPUSensitivity},
			{"gpu-sensitivity", gpuSensitivity, &cfg.GPUSensitivity},
		} {
			if !given[s.name] {
				continue
			}
			if !(s.value >= 0 && s.value <= 1) {
				return cfg, fmt.Errorf("-%s %v: must be in [0,1]", s.name, s.value)
			}
			v := s.value
			*s.dst = &v
		}
		if !(cfg.ArrivalWindowSec >= 0) || !(cfg.MaxDurationSec >= 0) {
			return cfg, fmt.Errorf("-arrival-window-sec %v, -max-duration-sec %v: must not be negative", cfg.ArrivalWindowSec, cfg.MaxDurationSec)
		}
		if !(cfg.PeerTransferIntensity >= 0 && cfg.PeerTransferIntensity <= 1) {
			return cfg, fmt.Errorf("-peer-transfer-intensity %v: must be in [0,1]", cfg.PeerTransferIntensity)
		}
	default:
		return cfg, fmt.Errorf("-schema-version %q: want %s or %s", cfg.SchemaVersion, workload.TraceSchemaV3, workload.TraceSchemaV2)
	}

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
	if cfg.PerfRatio > 1 {
		cfg.PerfRatio = 1
	}
	if cfg.ComputeBoundPerfBoost < 1 {
		cfg.ComputeBoundPerfBoost = 1
	}
	if cfg.TimeScale <= 0 {
		cfg.TimeScale = 1.0
	}
	setOutputPath(outPath)
	return cfg, nil
}

var outputPath = "trace.jsonl"

func setOutputPath(v string) { outputPath = v }

// A generator samples a whole trace from cfg.Seed and hands every record to
// emit in output order. at maps a sampled submit offset to the offset
// written. It returns the last sampled offset, before at.
type generator func(cfg generatorConfig, at func(float64) float64, emit func(any) error) (float64, error)

// writeTrace writes gen's trace to outputPath and returns the number of job
// records. With -arrival-window-sec it samples twice from the same seed: the
// first pass only measures the last arrival, the second scales every offset
// written by window/last, so the arrival pattern keeps its shape.
func writeTrace(cfg generatorConfig, gen generator) (int, error) {
	at := func(off float64) float64 { return off }
	if window := cfg.ArrivalWindowSec; window > 0 {
		last, err := gen(cfg, at, func(any) error { return nil })
		if err != nil {
			return 0, err
		}
		if last > window {
			scale := window / last
			at = func(off float64) float64 { return math.Min(window, off*scale) }
		}
	}
	f, err := os.Create(outputPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	jobs := 0
	if _, err := gen(cfg, at, func(v any) error {
		if _, ok := v.(jobRecord); ok {
			jobs++
		}
		return writeJSONL(w, v)
	}); err != nil {
		return jobs, err
	}
	return jobs, w.Flush()
}

func generateTrace(cfg generatorConfig) error {
	_, err := writeTrace(cfg, defaultRecords)
	return err
}

// defaultRecords is the default mode's generator.
func defaultRecords(cfg generatorConfig, at func(float64) float64, emit func(any) error) (float64, error) {
	rng := rand.New(rand.NewSource(cfg.Seed))
	arrivals := arrivalState{burstHourByDay: map[int]int{}, dipHourByDay: map[int]int{}}
	offset := 0.0
	jobOrdinal := 0
	for i := 0; i < cfg.Jobs; i++ {
		offset = nextArrivalOffset(rng, offset, cfg, &arrivals)
		wl := sampleLogicalWorkload(rng, i+1, at(offset), cfg)
		if cfg.EmitWorkloadRecords {
			if err := emit(workloadToRecord(wl, cfg)); err != nil {
				return 0, err
			}
		}
		for _, rec := range expandLogicalWorkload(wl, cfg, &jobOrdinal) {
			if err := emit(rec); err != nil {
				return 0, err
			}
		}
	}
	return offset, nil
}

func generateDiurnalTrace(cfg generatorConfig) error {
	jobs, err := writeTrace(cfg, diurnalRecords)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "diurnal trace: %d jobs, %.0f wall-sec, %.0f sim-hours\n", jobs, cfg.SimHours*3600.0/cfg.TimeScale, cfg.SimHours)
	return nil
}

// diurnalRecords is the diurnal mode's generator. It samples workloads using
// an NHPP cosine diurnal arrival pattern matching experiment-04's
// sim_24h_pue.py:
//   - Cosine day/night cycle (trough at 4 AM, peak at 4 PM)
//   - 10% burst probability (50-200 simultaneous jobs)
//   - 5 job categories: small CPU, medium CPU, GPU training (1-8 GPUs),
//     GPU inference (1-2 GPUs), large CPU
func diurnalRecords(cfg generatorConfig, at func(float64) float64, emit func(any) error) (float64, error) {
	rng := rand.New(rand.NewSource(cfg.Seed))
	wallSec := cfg.SimHours * 3600.0 / cfg.TimeScale
	if cfg.WorkScale <= 0 {
		cfg.WorkScale = 1.0
	}
	baseSpeed := cfg.baseSpeedPerCore()

	cpuClasses := []string{"cpu.compute_bound", "cpu.memory_bound", "cpu.mixed"}
	gpuClasses := []string{"gpu.compute", "gpu.mixed", "gpu.memory_bound"}
	if cfg.v3() {
		// The physics model knows gpu.compute_bound; v2 keeps "gpu.compute",
		// which it treats as gpu.mixed.
		gpuClasses[0] = "gpu.compute_bound"
	}

	jobID := 0
	t := 0.0
	last := 0.0
	for t < wallSec {
		// Current sim-hour for diurnal pattern.
		simH := (t * cfg.TimeScale) / 3600.0
		hourOfDay := math.Mod(simH, 24.0)
		phase := 2 * math.Pi * (hourOfDay - 4.0) / 24.0
		rate := 0.25 + 0.70*(1-math.Cos(phase))/2.0
		// Peak rate in jobs/min sim-time, convert to wall-time.
		baseRateWallSec := rate * cfg.DiurnalPeakRate / 60.0 * cfg.TimeScale
		interArrival := rng.ExpFloat64() / math.Max(baseRateWallSec, 0.001)
		t += interArrival
		if t >= wallSec {
			break
		}
		last = t

		// 10% burst chance.
		burstSize := 1
		burstRange := cfg.DiurnalBurstMax - cfg.DiurnalBurstMin + 1
		if burstRange < 1 {
			burstRange = 1
		}
		if rng.Float64() < 0.10 {
			burstSize = cfg.DiurnalBurstMin + rng.Intn(burstRange)
		}

		for b := 0; b < burstSize; b++ {
			jobID++
			wlID := fmt.Sprintf("workload-%06d", jobID)
			r := rng.Float64()

			var cpuReq float64
			var gpuReq int
			var isPerf bool
			var cpuUtil, gpuUtil, duration float64
			var wlType, cpuClass, gpuClass string

			gpuThreshold := 1.0 - cfg.GPURatio*0.70

			switch {
			case r < 0.35*(1.0-cfg.GPURatio):
				// Small CPU job.
				cpuReq = float64([]int{1, 2, 4, 8}[rng.Intn(4)])
				gpuReq = 0
				isPerf = false
				cpuUtil = 0.3 + rng.Float64()*0.5
				duration = 60 + rng.Float64()*540
				wlType = "cpu_preprocess"
				cpuClass = cpuClasses[rng.Intn(len(cpuClasses))]
			case r < 0.55*(1.0-cfg.GPURatio*0.3):
				// Medium CPU job.
				cpuReq = float64([]int{16, 32, 48, 64}[rng.Intn(4)])
				gpuReq = 0
				isPerf = rng.Float64() < 0.3
				cpuUtil = 0.4 + rng.Float64()*0.5
				duration = 300 + rng.Float64()*3300
				wlType = "cpu_analytics"
				cpuClass = cpuClasses[rng.Intn(len(cpuClasses))]
			case r < 0.80:
				// GPU training (1-8 GPUs).
				cpuReq = float64([]int{16, 32, 64}[rng.Intn(3)])
				gpuReq = maxInt(cfg.MinGPURequest, []int{1, 2, 4, 8}[rng.Intn(4)])
				isPerf = true
				cpuUtil = 0.3 + rng.Float64()*0.3
				gpuUtil = 0.6 + rng.Float64()*0.35
				duration = 3600 + rng.Float64()*25200
				if gpuReq >= 2 {
					wlType = "distributed_training"
				} else {
					wlType = "single_gpu_training"
				}
				cpuClass = "cpu.mixed"
				gpuClass = gpuClasses[rng.Intn(len(gpuClasses))]
			case r < 0.92:
				// GPU inference (1-2 GPUs).
				cpuReq = float64([]int{4, 8, 16}[rng.Intn(3)])
				gpuReq = maxInt(cfg.MinGPURequest, []int{1, 2}[rng.Intn(2)])
				isPerf = false
				cpuUtil = 0.3 + rng.Float64()*0.3
				gpuUtil = 0.3 + rng.Float64()*0.4
				duration = 60 + rng.Float64()*840
				wlType = "debug_eval"
				cpuClass = "cpu.mixed"
				gpuClass = gpuClasses[rng.Intn(len(gpuClasses))]
			default:
				// Large CPU job.
				cpuReq = float64([]int{64, 96}[rng.Intn(2)])
				gpuReq = 0
				isPerf = true
				cpuUtil = 0.5 + rng.Float64()*0.45
				duration = 1800 + rng.Float64()*5400
				wlType = "cpu_analytics"
				cpuClass = cpuClasses[rng.Intn(len(cpuClasses))]
			}

			// Override GPU to 0 for CPU-only experiments.
			if cfg.GPURatio == 0 || r < gpuThreshold && gpuReq > 0 {
				if cfg.GPURatio == 0 {
					gpuReq = 0
					gpuUtil = 0
					gpuClass = ""
					isPerf = rng.Float64() < 0.20
					if rng.Float64() < 0.5 {
						wlType = "cpu_preprocess"
					} else {
						wlType = "cpu_analytics"
					}
				}
			}

			intent := "standard"
			if isPerf {
				intent = "performance"
			}
			memIntensity := 0.3 + rng.Float64()*0.6
			ioIntensity := 0.05 + rng.Float64()*0.25

			// Compute work units.
			duration = cfg.clampDuration(duration)
			var cpuUnits, gpuUnits float64
			if cfg.v3() {
				cpuUnits, gpuUnits = v3Work(duration, cpuReq, gpuReq, baseSpeed)
			} else {
				cpuUnits = duration * cpuReq * math.Max(0.10, cpuUtil) * baseSpeed * cfg.WorkScale
				if gpuReq > 0 {
					gpuUnits = duration * float64(gpuReq) * math.Max(0.10, gpuUtil) * baseSpeed * cfg.WorkScale
				}
			}

			memGiB := math.Max(1, math.Floor(cpuReq*(0.5+rng.Float64()*1.5)))

			requests := map[string]string{
				"cpu":    formatCPU(cpuReq),
				"memory": formatMemoryGi(memGiB),
			}
			if gpuReq > 0 {
				requests["nvidia.com/gpu"] = strconv.Itoa(gpuReq)
			}

			// Build affinity for performance jobs, v2 only.
			var affinity map[string]any
			if isPerf && !cfg.NoAffinityOnly && !cfg.v3() {
				affinity = map[string]any{
					"nodeAffinity": map[string]any{
						"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
							"nodeSelectorTerms": []map[string]any{
								{
									"matchExpressions": []map[string]any{
										{
											"key":      "joulie.io/power-profile",
											"operator": "NotIn",
											"values":   []string{"eco"},
										},
									},
								},
							},
						},
					},
				}
			}

			profile := workloadProfileRec{
				CPUUtilization:  cpuUtil,
				GPUUtilization:  gpuUtil,
				MemoryIntensity: memIntensity,
				IOIntensity:     ioIntensity,
			}
			wlCls := workloadClass{CPU: cpuClass, GPU: gpuClass}

			sensCPU := 0.0
			sensGPU := 0.0
			if isPerf {
				sensCPU = 0.5
				if gpuReq > 0 {
					sensGPU = 0.5
				}
			}

			// Emit workload record.
			if cfg.EmitWorkloadRecords {
				wlRec := workloadRecord{
					Type:                workload.TraceRecordWorkload,
					SchemaVersion:       cfg.schema(),
					WorkloadID:          wlID,
					SubmitTimeOffsetSec: at(t),
					Namespace:           cfg.Namespace,
					WorkloadType:        wlType,
					DurationSec:         duration,
					IntentClass:         intent,
					WorkloadClass:       wlCls,
					SharedIntensity:     profile,
					Pods:                []workloadPodRec{{Role: "worker", Replicas: 1, Requests: requests}},
				}
				if err := emit(wlRec); err != nil {
					return 0, err
				}
			}

			// Emit job record.
			jobRec := jobRecord{
				Type:                workload.TraceRecordJob,
				SchemaVersion:       cfg.schema(),
				JobID:               fmt.Sprintf("%s-worker-01", wlID),
				WorkloadID:          wlID,
				WorkloadType:        wlType,
				PodRole:             "worker",
				SubmitTimeOffsetSec: at(t),
				Namespace:           cfg.Namespace,
				IntentClass:         intent,
				PodTemplate:         podTemplateRec{Affinity: affinity, Requests: requests},
				Work:                workRec{CPUUnits: cpuUnits, GPUUnits: gpuUnits},
				Sensitivity:         cfg.sensitivity(sensCPU, sensGPU),
				WorkloadClass:       wlCls,
				WorkloadProfile:     cfg.jobProfile(profile, gpuReq),
			}
			if err := emit(jobRec); err != nil {
				return 0, err
			}
		}
	}
	return last, nil
}

func writeJSONL(w *bufio.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func workloadToRecord(wl logicalWorkload, cfg generatorConfig) workloadRecord {
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
		Type:                workload.TraceRecordWorkload,
		SchemaVersion:       cfg.schema(),
		WorkloadID:          wl.ID,
		SubmitTimeOffsetSec: wl.SubmitSec,
		Namespace:           wl.Namespace,
		WorkloadType:        wl.Type,
		Gang:                wl.Gang,
		DurationSec:         wl.DurationSec,
		IntentClass:         wl.Class,
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
				intent = sampleIntentClass(rand.New(rand.NewSource(int64(len(jobID))+13)), cfg, wl.CPUClass, wl.GPUClass)
			}
			out = append(out, jobRecord{
				Type:                workload.TraceRecordJob,
				SchemaVersion:       cfg.schema(),
				JobID:               jobID,
				WorkloadID:          wl.ID,
				WorkloadType:        wl.Type,
				PodRole:             p.Role,
				Gang:                wl.Gang,
				SubmitTimeOffsetSec: wl.SubmitSec,
				Namespace:           wl.Namespace,
				IntentClass:         intent,
				PodTemplate: podTemplateRec{
					Affinity: cfg.affinity(intent),
					Requests: requests,
				},
				Work:            workRec{CPUUnits: p.CPUUnits, GPUUnits: p.GPUUnits},
				Sensitivity:     cfg.sensitivity(0, 0),
				WorkloadClass:   workloadClass{CPU: wl.CPUClass, GPU: wl.GPUClass},
				WorkloadProfile: cfg.jobProfile(wl.Profile, p.GPURequest),
				Tags:            append([]string{}, wl.Tags...),
			})
		}
	}
	return out
}

func sampleLogicalWorkload(rng *rand.Rand, idx int, submitSec float64, cfg generatorConfig) logicalWorkload {
	hasGPU := rng.Float64() < cfg.GPURatio
	wlType := sampleWorkloadType(rng, hasGPU)
	totalGPUs := 0
	if hasGPU {
		totalGPUs = sampleTotalGPUs(rng, cfg.MinGPURequest)
	}
	cpuClass, gpuClass := classesForType(wlType)
	intent := sampleIntentClass(rng, cfg, cpuClass, gpuClass)
	profile := sampleSharedProfile(rng, wlType, totalGPUs, cpuClass, gpuClass)
	durationSec := cfg.clampDuration(sampleDurationSec(rng, wlType, totalGPUs))
	wl := logicalWorkload{
		ID:          fmt.Sprintf("workload-%06d", idx),
		Type:        wlType,
		Class:       intent,
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

func sampleIntentClass(rng *rand.Rand, cfg generatorConfig, cpuClass, gpuClass string) string {
	if cfg.NoAffinityOnly {
		return "standard"
	}

	isComputeBound := strings.Contains(cpuClass, "compute") ||
		strings.Contains(gpuClass, "compute")

	ratio := cfg.PerfRatio
	if isComputeBound {
		ratio = math.Min(1.0, ratio*cfg.ComputeBoundPerfBoost)
	}

	if rng.Float64() < ratio {
		return "performance"
	}
	return "standard"
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

func sampleTotalGPUs(rng *rand.Rand, minGPU int) int {
	p := rng.Float64()
	var v int
	switch {
	case p < 0.80:
		v = 1
	case p < 0.90:
		v = 2
	case p < 0.97:
		v = 4
	default:
		v = 8
	}
	if minGPU > 0 && v < minGPU {
		v = minGPU
	}
	return v
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
	gpuResource := pickGPUResourceName(rng, totalGPUs, cfg.AMDGPURatio)
	pods := []logicalPod{}
	appendWorker := func(role string, replicas int, gpuEach int, cpuEach float64, memEach float64, cpuFrac float64, gpuFrac float64) {
		cpuUnits := duration * cpuEach * math.Max(0.10, profile.CPUUtilization) * cfg.CPURatePerCore * cpuFrac
		gpuUnits := duration * float64(gpuEach) * math.Max(0.10, profile.GPUUtilization) * cfg.GPURatePerGPU * gpuFrac
		if cfg.v3() {
			// Every pod of the workload runs for its durationSec at full
			// speed, whatever its role, utilization or rate flags.
			cpuUnits, gpuUnits = v3Work(duration, cpuEach, gpuEach, cfg.baseSpeedPerCore())
		}
		pods = append(pods, logicalPod{
			Role:        role,
			Replicas:    replicas,
			CPURequest:  cpuEach,
			MemoryGiB:   memEach,
			GPURequest:  gpuEach,
			GPUResource: gpuResource,
			CPUUnits:    cpuUnits,
			GPUUnits:    gpuUnits,
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
			return clampRange(logNormalApprox(rng, math.Log(16), 0.45), 4, 64)
		case "cpu_analytics":
			return clampRange(logNormalApprox(rng, math.Log(32), 0.5), 8, 128)
		default:
			return clampRange(logNormalApprox(rng, math.Log(8), 0.4), 2, 32)
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

// cpuSensitivityForType and gpuSensitivityForType were removed.
// The scheduler now uses only joulie.io/workload-class (performance|standard)
// for routing decisions, not per-resource sensitivity annotations.

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
	// Convert wall-clock offset to simulated seconds for day/night calculation.
	simSec := current * cfg.TimeScale
	hour := hourOfOffset(simSec)
	day := dayOfOffset(simSec)
	mult := hourlyArrivalMultiplier(hour)
	if isBurstHour(rng, st, day, hour, cfg) {
		mult *= cfg.BurstMultiplier
		mult += cfg.BurstMeanJobs / 16.0
	}
	if isDipHour(rng, st, day, hour, cfg) {
		mult *= cfg.DipMultiplier
	}
	if mult <= 0.05 {
		mult = 0.05
	}
	delta := rng.ExpFloat64() * cfg.MeanInterArrival / mult
	return current + delta
}

// hourlyArrivalMultiplier returns a multiplier for the job arrival rate at each
// hour of the day.  The pattern models a realistic datacenter load cycle:
//   - Night (00-05): low batch/maintenance traffic (~15-25% of peak)
//   - Morning ramp (06-08): engineers arrive, CI kicks in
//   - Morning peak (09-11): peak utilization
//   - Lunch dip (12-13): slight dip
//   - Afternoon peak (14-17): second sustained peak
//   - Evening wind-down (18-21): gradual decrease
//   - Late night (22-23): approaching overnight lows
func hourlyArrivalMultiplier(hour int) float64 {
	// index by hour 0-23
	pattern := [24]float64{
		0.15, // 00
		0.12, // 01
		0.10, // 02
		0.10, // 03
		0.12, // 04
		0.20, // 05
		0.40, // 06
		0.65, // 07
		0.85, // 08
		1.00, // 09, peak
		1.00, // 10, peak
		0.95, // 11
		0.80, // 12, lunch
		0.85, // 13
		1.00, // 14, afternoon peak
		1.00, // 15
		0.95, // 16
		0.85, // 17
		0.65, // 18
		0.50, // 19
		0.40, // 20
		0.30, // 21
		0.22, // 22
		0.18, // 23
	}
	if hour < 0 || hour > 23 {
		return 0.15
	}
	return pattern[hour]
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

// isDipHour returns true if the current hour is a "negative spike": a sudden
// drop in arrival rate.  This models maintenance windows, planned drains, or
// mass job completions that temporarily suppress new submissions.
func isDipHour(rng *rand.Rand, st *arrivalState, day, hour int, cfg generatorConfig) bool {
	if cfg.DipDayProbability <= 0 || cfg.DipMultiplier <= 0 {
		return false
	}
	dipHour, ok := st.dipHourByDay[day]
	if !ok {
		if rng.Float64() < cfg.DipDayProbability {
			// Dips happen in off-peak hours (early morning or evening).
			candidates := []int{3, 4, 5, 19, 20, 21}
			dipHour = candidates[rng.Intn(len(candidates))]
		} else {
			dipHour = -1
		}
		st.dipHourByDay[day] = dipHour
	}
	return hour == dipHour
}

func dayOfOffset(offset float64) int  { return int(math.Floor(offset / 86400.0)) }
func hourOfOffset(offset float64) int { return int(math.Floor(math.Mod(offset, 86400.0) / 3600.0)) }

func pickGPUResourceName(rng *rand.Rand, totalGPUs int, amdRatio float64) string {
	if totalGPUs <= 0 {
		return ""
	}
	if amdRatio > 0 && rng.Float64() < amdRatio {
		return "amd.com/gpu"
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
	if class == "performance" {
		// Safety net: even without the scheduler extender, performance pods
		// avoid eco nodes via native Kubernetes node affinity.
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
	}
	// "standard" and anything else: no affinity constraint.
	return nil
}
