package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
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
	flag.IntVar(&jobs, "jobs", 50, "number of jobs")
	flag.StringVar(&outPath, "out", "trace.jsonl", "output JSONL path")
	flag.Float64Var(&meanInterArrival, "mean-inter-arrival-sec", 5, "mean inter-arrival seconds")
	flag.Int64Var(&seed, "seed", time.Now().UnixNano(), "rng seed")
	flag.Parse()

	f, err := os.Create(outPath)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	rng := rand.New(rand.NewSource(seed))
	classes := []string{"performance", "eco"}
	offset := 0.0
	for i := 0; i < jobs; i++ {
		offset += rng.ExpFloat64() * meanInterArrival
		cpu := 1 + rng.Intn(8)
		units := 600 + rng.Float64()*3000
		class := classes[rng.Intn(len(classes))]
		rec := jobRecord{
			Type:                "job",
			SchemaVersion:       "v1",
			JobID:               fmt.Sprintf("job-%06d", i+1),
			SubmitTimeOffsetSec: offset,
			Namespace:           "default",
			PodTemplate: podTemplateRec{
				Affinity: affinityForClass(class),
				Requests: map[string]string{"cpu": fmt.Sprintf("%d", cpu), "memory": "1Gi"},
			},
			Work: workRec{CPUUnits: units, GPUUnits: 0},
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
									"operator": "In",
									"values":   []string{"performance"},
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
							},
						},
					},
				},
			},
		}
	default:
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
							},
						},
					},
				},
			},
		}
	}
}
