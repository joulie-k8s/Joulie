package classifier

import (
	"testing"
	"time"
)

func TestParsePodHints(t *testing.T) {
	labels := map[string]string{"joulie.io/power-profile": "eco"}
	annotations := map[string]string{
		"joulie.io/reschedulable":  "true",
		"joulie.io/cpu-sensitivity": "low",
	}
	hints := ParsePodHints(labels, annotations)
	if hints.WorkloadClass != "best-effort" {
		t.Errorf("expected best-effort from eco label, got %s", hints.WorkloadClass)
	}
	if !hints.Reschedulable {
		t.Error("expected reschedulable=true")
	}
	if hints.CPUSensitivity != "low" {
		t.Errorf("expected cpu-sensitivity=low, got %s", hints.CPUSensitivity)
	}
}

func TestParsePodHintsExplicitClass(t *testing.T) {
	annotations := map[string]string{
		"joulie.io/workload-class":  "performance",
		"joulie.io/gpu-sensitivity": "high",
	}
	hints := ParsePodHints(nil, annotations)
	if hints.WorkloadClass != "performance" {
		t.Errorf("expected performance, got %s", hints.WorkloadClass)
	}
	if hints.GPUSensitivity != "high" {
		t.Errorf("expected gpu-sensitivity=high, got %s", hints.GPUSensitivity)
	}
}

func TestClassifyComputeBound(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	hints := PodHints{WorkloadClass: "standard"}
	m := PodMetrics{
		CPUUtilPct:        90,
		CPUBoundRatio:     0.80, // high CPU energy fraction
		MemoryBoundRatio:  0.10,
		KeplerUsed:        true,
		TotalEnergyJoules: 100,
		CPUEnergyJoules:   80,
		DRAMEnergyJoules:  10,
	}
	wp := c.classify(hints, m)
	if wp.CPU.Bound != "compute" {
		t.Errorf("expected compute-bound, got %s", wp.CPU.Bound)
	}
	if wp.CPU.CapSensitivity != "high" {
		t.Errorf("expected high cap sensitivity for compute-bound, got %s", wp.CPU.CapSensitivity)
	}
}

func TestClassifyMemoryBound(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	hints := PodHints{WorkloadClass: "standard"}
	m := PodMetrics{
		CPUUtilPct:        55,
		CPUBoundRatio:     0.30,
		MemoryBoundRatio:  0.50, // high DRAM energy fraction
		KeplerUsed:        true,
		TotalEnergyJoules: 100,
		CPUEnergyJoules:   30,
		DRAMEnergyJoules:  50,
	}
	wp := c.classify(hints, m)
	if wp.CPU.Bound != "memory" {
		t.Errorf("expected memory-bound, got %s", wp.CPU.Bound)
	}
	if wp.CPU.CapSensitivity != "low" {
		t.Errorf("expected low CPU cap sensitivity for memory-bound, got %s", wp.CPU.CapSensitivity)
	}
}

func TestClassifyGPUBound(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	hints := PodHints{WorkloadClass: "standard", GPUSensitivity: "high"}
	m := PodMetrics{
		CPUUtilPct:        40,
		CPUBoundRatio:     0.10,
		MemoryBoundRatio:  0.10,
		GPUEnergyJoules:   80,
		TotalEnergyJoules: 100,
		CPUEnergyJoules:   10,
		DRAMEnergyJoules:  10,
		KeplerUsed:        true,
	}
	wp := c.classify(hints, m)
	if wp.GPU.Intensity == "none" {
		t.Error("expected GPU intensity to be set for GPU-heavy workload")
	}
	if wp.GPU.CapSensitivity != "high" {
		t.Errorf("expected high GPU cap sensitivity from hint, got %s", wp.GPU.CapSensitivity)
	}
}

func TestClassifyNoKepler(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	hints := PodHints{WorkloadClass: "performance"}
	m := PodMetrics{
		CPUUtilPct: 85,
		KeplerUsed: false,
	}
	wp := c.classify(hints, m)
	if wp.CPU.Bound != "compute" {
		t.Errorf("expected compute-bound fallback for high CPU util without Kepler, got %s", wp.CPU.Bound)
	}
	if wp.Confidence < 0.3 {
		t.Errorf("expected confidence >= 0.3, got %f", wp.Confidence)
	}
}

func TestFromHintsOnly(t *testing.T) {
	hints := PodHints{
		WorkloadClass:  "best-effort",
		Reschedulable:  true,
		CPUSensitivity: "low",
	}
	wp := fromHintsOnly(hints)
	if wp.Criticality.Class != "best-effort" {
		t.Errorf("expected best-effort, got %s", wp.Criticality.Class)
	}
	if !wp.Migratability.Reschedulable {
		t.Error("expected reschedulable=true")
	}
	if wp.Confidence > 0.4 {
		t.Errorf("hints-only confidence should be low, got %f", wp.Confidence)
	}
	if wp.LastUpdated.IsZero() {
		t.Error("expected LastUpdated to be set")
	}
	_ = time.Now() // just ensure time package is used
}

func TestComputeConfidenceKepler(t *testing.T) {
	hints := PodHints{WorkloadClass: "performance"} // explicit class
	m := PodMetrics{
		CPUUtilPct:        70,
		KeplerUsed:        true,
		TotalEnergyJoules: 50,
	}
	conf := computeConfidence(hints, m)
	if conf < 0.7 {
		t.Errorf("expected high confidence with Kepler + explicit class, got %f", conf)
	}
}

func TestClassificationSummary(t *testing.T) {
	// Just verify it doesn't panic and returns a non-empty string
	wp := fromHintsOnly(PodHints{WorkloadClass: "standard"})
	s := ClassificationSummary(wp)
	if s == "" {
		t.Error("expected non-empty summary")
	}
}
