package classifier

import (
	"strings"
	"testing"
	"time"
)

func TestParsePodHints(t *testing.T) {
	labels := map[string]string{"joulie.io/power-profile": "eco"}
	annotations := map[string]string{
		"joulie.io/reschedulable": "true",
	}
	hints := ParsePodHints(labels, annotations)
	if hints.WorkloadClass != "standard" {
		t.Errorf("expected standard from eco label, got %s", hints.WorkloadClass)
	}
	if !hints.Reschedulable {
		t.Error("expected reschedulable=true")
	}
}

func TestParsePodHintsExplicitClass(t *testing.T) {
	annotations := map[string]string{
		"joulie.io/workload-class": "performance",
	}
	hints := ParsePodHints(nil, annotations)
	if hints.WorkloadClass != "performance" {
		t.Errorf("expected performance, got %s", hints.WorkloadClass)
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
	hints := PodHints{WorkloadClass: "standard"}
	m := PodMetrics{
		CPUUtilPct:        40,
		CPUBoundRatio:     0.10,
		MemoryBoundRatio:  0.10,
		GPUEnergyJoules:   80,
		GPUUtilPct:        85,
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
		t.Errorf("expected high GPU cap sensitivity for compute+high GPU, got %s", wp.GPU.CapSensitivity)
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
		WorkloadClass: "standard",
		Reschedulable: true,
	}
	wp := fromHintsOnly(hints)
	if wp.Criticality.Class != "standard" {
		t.Errorf("expected standard, got %s", wp.Criticality.Class)
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
	if wp.ClassificationReason == "" {
		t.Error("expected classificationReason to be set for hints-only")
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

func TestClassificationReasonPopulated(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	hints := PodHints{WorkloadClass: "standard"}
	m := PodMetrics{
		CPUUtilPct:        90,
		CPUBoundRatio:     0.80,
		MemoryBoundRatio:  0.10,
		KeplerUsed:        true,
		TotalEnergyJoules: 100,
		CPUEnergyJoules:   80,
		DRAMEnergyJoules:  10,
	}
	wp := c.classify(hints, m)
	if wp.ClassificationReason == "" {
		t.Error("expected classificationReason to be populated")
	}
	if !strings.Contains(wp.ClassificationReason, "cpu-intensity=high") {
		t.Errorf("expected reason to mention cpu-intensity=high, got %q", wp.ClassificationReason)
	}
	if !strings.Contains(wp.ClassificationReason, "cpu-bound=compute") {
		t.Errorf("expected reason to mention compute-bound, got %q", wp.ClassificationReason)
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

func TestClassifyGPUUtilizationUsesGPUMetric(t *testing.T) {
	// Regression test for bug: GPU profile was using CPUUtilPct instead of GPUUtilPct
	c := NewClassifier(DefaultClassifierConfig())
	hints := PodHints{WorkloadClass: "standard"}
	m := PodMetrics{
		CPUUtilPct: 20,
		GPUUtilPct: 85,
	}
	wp := c.classify(hints, m)
	if wp.GPU.AvgUtilizationPct != 85 {
		t.Errorf("GPU AvgUtilizationPct should use GPUUtilPct (85), got %f", wp.GPU.AvgUtilizationPct)
	}
	if wp.CPU.AvgUtilizationPct != 20 {
		t.Errorf("CPU AvgUtilizationPct should use CPUUtilPct (20), got %f", wp.CPU.AvgUtilizationPct)
	}
}

func TestClassifyZeroMetrics(t *testing.T) {
	// All-zero metrics: should not panic, should produce reasonable defaults
	c := NewClassifier(DefaultClassifierConfig())
	wp := c.classify(PodHints{}, PodMetrics{})
	if wp.CPU.Intensity != "low" {
		t.Errorf("expected low cpu intensity for zero metrics, got %s", wp.CPU.Intensity)
	}
	if wp.GPU.Intensity != "none" {
		t.Errorf("expected none gpu intensity for zero metrics, got %s", wp.GPU.Intensity)
	}
	// Empty hints.WorkloadClass passes through as-is (classify doesn't default it)
	// ParsePodHints defaults to "standard" but classify just uses what it gets
	if wp.Criticality.Class != "" {
		t.Errorf("expected empty class for empty hints, got %s", wp.Criticality.Class)
	}
}

func TestClassifyIOBound(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	m := PodMetrics{CPUUtilPct: 10, MemoryPressurePct: 20}
	wp := c.classify(PodHints{WorkloadClass: "standard"}, m)
	if wp.CPU.Bound != "io" {
		t.Errorf("expected io-bound for very low CPU util, got %s", wp.CPU.Bound)
	}
	if wp.CPU.CapSensitivity != "low" {
		t.Errorf("expected low cap sensitivity for io-bound, got %s", wp.CPU.CapSensitivity)
	}
}

func TestClassifyGPUDominant(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	m := PodMetrics{
		CPUUtilPct:  50,
		GPUUtilPct:  90,
		GPUDominant: true,
	}
	wp := c.classify(PodHints{WorkloadClass: "standard"}, m)
	// When GPU dominant, CPU should be marked as mixed with low sensitivity
	if wp.CPU.Bound != "mixed" {
		t.Errorf("expected cpu-bound=mixed when GPU dominant, got %s", wp.CPU.Bound)
	}
	if wp.CPU.CapSensitivity != "low" {
		t.Errorf("expected low CPU cap sensitivity when GPU dominant, got %s", wp.CPU.CapSensitivity)
	}
	if wp.GPU.Intensity != "high" {
		t.Errorf("expected high GPU intensity at 90%% util, got %s", wp.GPU.Intensity)
	}
}

func TestClassifyGPUHighIntensityComputeBound(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	m := PodMetrics{GPUUtilPct: 80, MemoryPressurePct: 10}
	wp := c.classify(PodHints{}, m)
	if wp.GPU.Bound != "compute" {
		t.Errorf("expected gpu-bound=compute for high GPU util low mem, got %s", wp.GPU.Bound)
	}
	if wp.GPU.CapSensitivity != "high" {
		t.Errorf("expected high GPU cap sensitivity for compute+high intensity, got %s", wp.GPU.CapSensitivity)
	}
}

func TestClassifyGPUMemoryBound(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	m := PodMetrics{GPUUtilPct: 50, MemoryPressurePct: 70}
	wp := c.classify(PodHints{}, m)
	if wp.GPU.Bound != "memory" {
		t.Errorf("expected gpu-bound=memory when mem pressure > 60, got %s", wp.GPU.Bound)
	}
}

func TestClassifyGPULowIntensity(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	m := PodMetrics{GPUUtilPct: 15}
	wp := c.classify(PodHints{}, m)
	if wp.GPU.Intensity != "low" {
		t.Errorf("expected low GPU intensity at 15%%, got %s", wp.GPU.Intensity)
	}
	if wp.GPU.CapSensitivity != "low" {
		t.Errorf("expected low GPU cap sensitivity for low intensity, got %s", wp.GPU.CapSensitivity)
	}
}

func TestClassifyKeplerOverrideToCompute(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	// Moderate CPU util (not compute-bound by util alone) but Kepler says 80% CPU energy
	m := PodMetrics{
		CPUUtilPct:        50,
		MemoryPressurePct: 60, // would be memory-bound by pressure
		CPUBoundRatio:     0.80,
		KeplerUsed:        true,
		TotalEnergyJoules: 100,
	}
	wp := c.classify(PodHints{}, m)
	if wp.CPU.Bound != "compute" {
		t.Errorf("expected kepler override to compute-bound, got %s", wp.CPU.Bound)
	}
	if !strings.Contains(wp.ClassificationReason, "kepler override") {
		t.Errorf("expected reason to mention kepler override, got %q", wp.ClassificationReason)
	}
}

func TestClassifyKeplerOverrideToMemory(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	// High CPU util but Kepler shows high memory energy
	m := PodMetrics{
		CPUUtilPct:        80,
		MemoryBoundRatio:  0.50,
		CPUBoundRatio:     0.20,
		KeplerUsed:        true,
		TotalEnergyJoules: 100,
	}
	wp := c.classify(PodHints{}, m)
	if wp.CPU.Bound != "memory" {
		t.Errorf("expected kepler override to memory-bound, got %s", wp.CPU.Bound)
	}
}

func TestConfidenceClampedToOne(t *testing.T) {
	// All signals present: should not exceed 1.0
	hints := PodHints{WorkloadClass: "performance"}
	m := PodMetrics{CPUUtilPct: 90, KeplerUsed: true, TotalEnergyJoules: 100}
	conf := computeConfidence(hints, m)
	if conf > 1.0 {
		t.Errorf("confidence should be clamped to 1.0, got %f", conf)
	}
}

func TestConfidenceBaseMinimum(t *testing.T) {
	conf := computeConfidence(PodHints{}, PodMetrics{})
	if conf < 0.3 {
		t.Errorf("base confidence should be at least 0.3, got %f", conf)
	}
}

func TestFromHintsOnlyDefaults(t *testing.T) {
	// No sensitivity hints: should use defaults
	wp := fromHintsOnly(PodHints{WorkloadClass: "standard"})
	if wp.CPU.CapSensitivity != "medium" {
		t.Errorf("expected default cpu sensitivity=medium, got %s", wp.CPU.CapSensitivity)
	}
	if wp.GPU.CapSensitivity != "none" {
		t.Errorf("expected default gpu sensitivity=none, got %s", wp.GPU.CapSensitivity)
	}
}

func TestParsePodHintsNilInputs(t *testing.T) {
	hints := ParsePodHints(nil, nil)
	if hints.WorkloadClass != "standard" {
		t.Errorf("expected standard default class, got %s", hints.WorkloadClass)
	}
	if hints.Reschedulable {
		t.Error("expected reschedulable=false by default")
	}
}

func TestClassifyGPUMediumIntensity(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	m := PodMetrics{GPUUtilPct: 40}
	wp := c.classify(PodHints{}, m)
	if wp.GPU.Intensity != "medium" {
		t.Errorf("expected medium GPU intensity at 40%%, got %s", wp.GPU.Intensity)
	}
}

func TestClassifyCPUMediumIntensity(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	m := PodMetrics{CPUUtilPct: 50}
	wp := c.classify(PodHints{}, m)
	if wp.CPU.Intensity != "medium" {
		t.Errorf("expected medium CPU intensity at 50%%, got %s", wp.CPU.Intensity)
	}
}

func TestClassificationSummaryHighConfidence(t *testing.T) {
	c := NewClassifier(DefaultClassifierConfig())
	m := PodMetrics{CPUUtilPct: 90, GPUUtilPct: 80, KeplerUsed: true, TotalEnergyJoules: 100}
	wp := c.classify(PodHints{WorkloadClass: "performance"}, m)
	s := ClassificationSummary(wp)
	if !strings.Contains(s, "high-confidence") {
		t.Errorf("expected high-confidence in summary, got %q", s)
	}
	if !strings.Contains(s, "gpu=") {
		t.Errorf("expected GPU info in summary when GPU active, got %q", s)
	}
}
