package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"time"
)

type jobRecord struct {
	Type                string         `json:"type"`
	SchemaVersion       string         `json:"schemaVersion"`
	JobID               string         `json:"jobId"`
	SubmitTimeOffsetSec float64        `json:"submitTimeOffsetSec"`
	Namespace           string         `json:"namespace"`
	PodTemplate         podTemplateRec `json:"podTemplate"`
	Work                workRec        `json:"work"`
	Sensitivity         sensitivityRec `json:"sensitivity"`
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

func main() {
	var jobs int
	var outPath string
	var meanInterArrival float64
	var seed int64
	var perfRatio float64
	var ecoRatio float64
	var noAffinityOnly bool
	var cpuUnitsMin float64
	var cpuUnitsMax float64
	var gpuRatio float64
	var gpuUnitsMin float64
	var gpuUnitsMax float64
	var gpuRequestPerJob float64
	flag.IntVar(&jobs, "jobs", 50, "number of jobs")
	flag.StringVar(&outPath, "out", "trace.jsonl", "output JSONL path")
	flag.Float64Var(&meanInterArrival, "mean-inter-arrival-sec", 5, "mean inter-arrival seconds")
	flag.Int64Var(&seed, "seed", time.Now().UnixNano(), "rng seed")
	flag.Float64Var(&perfRatio, "perf-ratio", 0.30, "ratio of performance-constrained jobs")
	flag.Float64Var(&ecoRatio, "eco-ratio", 0.00, "ratio of eco-constrained jobs (advanced; default disabled)")
	flag.BoolVar(&noAffinityOnly, "no-affinity-only", false, "if true, all jobs are generated without power-profile affinity")
	flag.Float64Var(&cpuUnitsMin, "cpu-units-min", 600, "minimum cpu work units per job")
	flag.Float64Var(&cpuUnitsMax, "cpu-units-max", 3600, "maximum cpu work units per job")
	flag.Float64Var(&gpuRatio, "gpu-ratio", 0.0, "ratio of jobs with GPU work")
	flag.Float64Var(&gpuUnitsMin, "gpu-units-min", 500, "minimum gpu work units per GPU job")
	flag.Float64Var(&gpuUnitsMax, "gpu-units-max", 2500, "maximum gpu work units per GPU job")
	flag.Float64Var(&gpuRequestPerJob, "gpu-request", 1, "GPU request for GPU jobs")
	flag.Parse()
	if perfRatio < 0 {
		perfRatio = 0
	}
	if ecoRatio < 0 {
		ecoRatio = 0
	}
	if perfRatio+ecoRatio > 1 {
		total := perfRatio + ecoRatio
		perfRatio = perfRatio / total
		ecoRatio = ecoRatio / total
	}
	if cpuUnitsMin <= 0 {
		cpuUnitsMin = 1
	}
	if cpuUnitsMax < cpuUnitsMin {
		cpuUnitsMax = cpuUnitsMin
	}

	f, err := os.Create(outPath)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	rng := rand.New(rand.NewSource(seed))
	offset := 0.0
	for i := 0; i < jobs; i++ {
		offset += rng.ExpFloat64() * meanInterArrival
		cpu := 1 + rng.Intn(8)
		units := cpuUnitsMin + rng.Float64()*(cpuUnitsMax-cpuUnitsMin)
		gpuUnits := 0.0
		requests := map[string]string{"cpu": fmt.Sprintf("%d", cpu), "memory": "1Gi"}
		if rng.Float64() < gpuRatio {
			gpuUnits = gpuUnitsMin + rng.Float64()*(gpuUnitsMax-gpuUnitsMin)
			requests["nvidia.com/gpu"] = strconv.FormatFloat(gpuRequestPerJob, 'f', -1, 64)
		}
		class := "general"
		if !noAffinityOnly {
			p := rng.Float64()
			if p < perfRatio {
				class = "performance"
			} else if p < perfRatio+ecoRatio {
				class = "eco"
			}
		}
		rec := jobRecord{
			Type:                "job",
			SchemaVersion:       "v1",
			JobID:               fmt.Sprintf("job-%06d", i+1),
			SubmitTimeOffsetSec: offset,
			Namespace:           "default",
			PodTemplate: podTemplateRec{
				Affinity: affinityForClass(class),
				Requests: requests,
			},
			Work: workRec{CPUUnits: units, GPUUnits: gpuUnits},
			Sensitivity: sensitivityRec{
				CPU: 0.8 + rng.Float64()*0.2,
				GPU: 1,
			},
		}
		b, _ := json.Marshal(rec)
		_, _ = w.Write(append(b, '\n'))
	}
}

func affinityForClass(class string) map[string]any {
	switch class {
	case "performance":
		return map[string]any{
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
	case "eco":
		return map[string]any{
			"nodeAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
					"nodeSelectorTerms": []map[string]any{
						{
							"matchExpressions": []map[string]any{
								{
									"key":      "joulie.io/power-profile",
									"operator": "In",
									"values":   []string{"eco"},
								},
								{
									"key":      "joulie.io/draining",
									"operator": "In",
									"values":   []string{"false"},
								},
							},
						},
					},
				},
			},
		}
	default:
		return nil
	}
}
