// Package classifier implements workload profile classification for Joulie.
//
// # Classification approach
//
// Classification uses a two-phase approach:
//
// 1. Static hints: pod labels/annotations set by the user or deployment tooling.
//    Fast, zero overhead, but requires manual annotation.
//
// 2. Dynamic metrics: Prometheus/Kepler metrics measured while the workload runs.
//    Automatic, but requires Prometheus + Kepler to be deployed.
//    Kepler (https://github.com/sustainable-computing-io/kepler) provides
//    per-container energy breakdown (CPU/DRAM/GPU), which is the primary signal
//    for CPU-bound vs memory-bound vs GPU-bound classification.
//
// Without Kepler, the classifier falls back to cAdvisor CPU utilization metrics
// and applies conservative heuristics (assumes compute-bound at high utilization).
//
// # Where an AI/ML model fits
//
// The current classifier uses threshold-based rules (a form of decision tree).
// A future ML model would:
//   - Collect a training dataset from Kepler metrics + manually-labelled profiles
//   - Train a lightweight multi-class classifier (Random Forest or XGBoost)
//   - Export as ONNX and embed in the binary, or call a sidecar inference service
//   - Use features: CPUBoundRatio, MemoryBoundRatio, GPUEnergyFraction,
//     CPUUtilPct, MemoryPressureRatio, JobDurationSeconds
//
// The heuristic rules in this file are designed to be replaced by such a model
// with minimal code change (same input/output interface).
//
// # Kepler integration
//
// Kepler exposes per-container energy counters scraped by Prometheus.
// Install Kepler with:
//
//	helm repo add kepler https://sustainable-computing-io.github.io/kepler-helm-chart
//	helm install kepler kepler/kepler -n monitoring
//
// The classifier will auto-detect Kepler availability at startup.
package classifier

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	joulie "github.com/matbun/joulie/pkg/api"
)

// ClassifierConfig controls classification behavior.
type ClassifierConfig struct {
	// MetricsWindow is the lookback window for metric averaging.
	// 10 minutes gives a stable representative sample for most workloads.
	MetricsWindow time.Duration
	// ReclassifyInterval is how often to re-assess running pods.
	// 15 minutes balances responsiveness to workload phase changes with stability.
	ReclassifyInterval time.Duration
	// Prometheus config for metric fetching.
	Prometheus PrometheusConfig
	// MinConfidence is the minimum confidence to publish a profile.
	// Below this, static/default values are used.
	MinConfidence float64
}

// DefaultClassifierConfig returns sensible defaults.
func DefaultClassifierConfig() ClassifierConfig {
	return ClassifierConfig{
		MetricsWindow:      10 * time.Minute,
		ReclassifyInterval: 15 * time.Minute,
		Prometheus:         DefaultPrometheusConfig(),
		MinConfidence:      0.5,
	}
}

// PodHints are static classification hints from pod labels/annotations.
// These override or seed the dynamic classification.
type PodHints struct {
	// WorkloadClass: "performance" | "standard" | "best-effort"
	// Set via joulie.io/workload-class annotation.
	WorkloadClass string
	// Reschedulable: pod can be safely restarted/rescheduled.
	// Set via joulie.io/reschedulable=true annotation.
	Reschedulable bool
	// CPUSensitivity: "high" | "medium" | "low" - overrides dynamic classification.
	CPUSensitivity string
	// GPUSensitivity: "high" | "medium" | "low" - overrides dynamic classification.
	GPUSensitivity string
}

// ParsePodHints extracts classification hints from pod labels and annotations.
//
// Supported annotations:
//
//	joulie.io/workload-class     = "performance" | "standard" | "best-effort"
//	joulie.io/reschedulable      = "true" | "false"
//	joulie.io/cpu-sensitivity    = "high" | "medium" | "low"   (optional override)
//	joulie.io/gpu-sensitivity    = "high" | "medium" | "low"   (optional override)
//
// Supported labels (for compatibility with existing workload labels):
//
//	joulie.io/power-profile      = "eco" → maps to best-effort
func ParsePodHints(labels, annotations map[string]string) PodHints {
	hints := PodHints{
		WorkloadClass: "standard", // default
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	if labels == nil {
		labels = map[string]string{}
	}

	if v := annotations["joulie.io/workload-class"]; v != "" {
		hints.WorkloadClass = v
	} else if labels["joulie.io/power-profile"] == "eco" {
		hints.WorkloadClass = "best-effort"
	}

	hints.Reschedulable = annotations["joulie.io/reschedulable"] == "true"

	if v := annotations["joulie.io/cpu-sensitivity"]; v != "" {
		hints.CPUSensitivity = v
	}
	if v := annotations["joulie.io/gpu-sensitivity"]; v != "" {
		hints.GPUSensitivity = v
	}
	return hints
}

// Classifier classifies workloads and produces WorkloadProfile statuses.
type Classifier struct {
	cfg     ClassifierConfig
	metrics *MetricsReader
}

// NewClassifier creates a new Classifier.
func NewClassifier(cfg ClassifierConfig) *Classifier {
	return &Classifier{
		cfg:     cfg,
		metrics: NewMetricsReader(cfg.Prometheus),
	}
}

// ClassifyPod produces a WorkloadProfileStatus for the given pod.
//
// Classification flow:
//  1. Parse static hints from labels/annotations (high confidence).
//  2. Fetch Kepler/cAdvisor metrics for the pod.
//  3. Apply heuristic rules to determine CPU/GPU intensity and boundness.
//  4. Merge static hints with dynamic classification.
//  5. Compute final confidence score.
func (c *Classifier) ClassifyPod(ctx context.Context, namespace, podName string, labels, annotations map[string]string) (joulie.WorkloadProfileStatus, error) {
	hints := ParsePodHints(labels, annotations)

	// Fetch metrics
	m, err := c.metrics.FetchPodMetrics(ctx, namespace, podName, c.cfg.MetricsWindow)
	if err != nil {
		log.Printf("classifier: metrics unavailable for %s/%s (%v), using hints only", namespace, podName, err)
		return fromHintsOnly(hints), nil
	}

	return c.classify(hints, m), nil
}

// classify applies heuristic rules to produce a WorkloadProfileStatus.
//
// Classification is primarily based on utilization % signals:
//
//	GPU-dominant:   GPUUtilPct > 40 AND GPUUtilPct > CPUUtilPct
//	CPU compute:    CPUUtilPct > 65 AND NOT GPU-dominant AND MemoryPressurePct < 50
//	Memory-bound:   MemoryPressurePct > 50 AND CPUUtilPct < 60
//	Mixed:          none of the above clearly dominate
//
// Kepler energy ratios (CPUBoundRatio, MemoryBoundRatio) are used as enrichment
// when Kepler is available, but not required.
//
// Cap sensitivity:
//
//	high:   CPU compute-bound at > 70% util → sensitive to RAPL cap
//	medium: moderate utilization
//	low:    memory/IO bound → RAPL cap has less impact
//
// This function is designed to be replaced by an ML model (same interface).
func (c *Classifier) classify(hints PodHints, m PodMetrics) joulie.WorkloadProfileStatus {
	wp := joulie.WorkloadProfileStatus{
		Criticality: joulie.WorkloadCriticality{Class: hints.WorkloadClass},
		Migratability: joulie.WorkloadMigratability{
			Reschedulable:  hints.Reschedulable,
		},
		LastUpdated: time.Now().UTC(),
	}

	var reasons []string

	// --- CPU profile (primary: utilization %) ---
	cpuIntensity := "low"
	cpuBound := "mixed"
	cpuCapSensitivity := "medium"

	switch {
	case m.CPUUtilPct >= 75:
		cpuIntensity = "high"
		reasons = append(reasons, fmt.Sprintf("cpu-intensity=high (util %.0f%%≥75%%)", m.CPUUtilPct))
	case m.CPUUtilPct >= 30:
		cpuIntensity = "medium"
		reasons = append(reasons, fmt.Sprintf("cpu-intensity=medium (util %.0f%%)", m.CPUUtilPct))
	default:
		reasons = append(reasons, fmt.Sprintf("cpu-intensity=low (util %.0f%%<30%%)", m.CPUUtilPct))
	}

	// Boundness: util % as primary signal, Kepler ratios as enrichment
	switch {
	case m.GPUDominant:
		cpuBound = "mixed" // GPU is the primary resource; CPU is secondary
		cpuCapSensitivity = "low"
		reasons = append(reasons, "cpu-bound=mixed (GPU dominant)")
	case m.CPUUtilPct > 65 && m.MemoryPressurePct < 50:
		cpuBound = "compute"
		if m.CPUUtilPct > 70 {
			cpuCapSensitivity = "high" // CPU compute at high util = sensitive to RAPL cap
			reasons = append(reasons, fmt.Sprintf("cpu-bound=compute, cap-sensitivity=high (util %.0f%%>70%%)", m.CPUUtilPct))
		} else {
			reasons = append(reasons, "cpu-bound=compute (high CPU, low mem pressure)")
		}
	case m.MemoryPressurePct > 50 && m.CPUUtilPct < 60:
		cpuBound = "memory"
		cpuCapSensitivity = "low" // memory-bound: RAPL cap has less impact
		reasons = append(reasons, fmt.Sprintf("cpu-bound=memory (mem-pressure %.0f%%>50%%)", m.MemoryPressurePct))
	case m.CPUUtilPct < 20:
		cpuBound = "io"
		cpuCapSensitivity = "low"
		reasons = append(reasons, "cpu-bound=io (util <20%)")
	}

	// Kepler enrichment: override if Kepler gives a clearer signal
	if m.KeplerUsed && m.TotalEnergyJoules > 0 {
		if m.CPUBoundRatio > 0.70 && cpuBound != "compute" {
			cpuBound = "compute"
			cpuCapSensitivity = "high"
			reasons = append(reasons, fmt.Sprintf("kepler override: cpu-bound=compute (energy ratio %.2f>0.70)", m.CPUBoundRatio))
		} else if m.MemoryBoundRatio > 0.40 && cpuBound != "memory" {
			cpuBound = "memory"
			cpuCapSensitivity = "low"
			reasons = append(reasons, fmt.Sprintf("kepler override: cpu-bound=memory (mem-energy ratio %.2f>0.40)", m.MemoryBoundRatio))
		}
	}

	// Override from hints
	if hints.CPUSensitivity != "" {
		cpuCapSensitivity = hints.CPUSensitivity
		reasons = append(reasons, fmt.Sprintf("cpu-cap-sensitivity=%s (annotation override)", hints.CPUSensitivity))
	}

	wp.CPU = joulie.WorkloadCPUProfile{
		Intensity:         cpuIntensity,
		Bound:             cpuBound,
		AvgUtilizationPct: m.CPUUtilPct,
		CapSensitivity:    cpuCapSensitivity,
	}

	// --- GPU profile (primary: GPUUtilPct) ---
	gpuIntensity := "none"
	gpuBound := "none"
	gpuCapSensitivity := "none"

	if m.GPUUtilPct > 0 || (m.KeplerUsed && m.GPUEnergyJoules > 0) {
		switch {
		case m.GPUUtilPct >= 70:
			gpuIntensity = "high"
			reasons = append(reasons, fmt.Sprintf("gpu-intensity=high (util %.0f%%≥70%%)", m.GPUUtilPct))
		case m.GPUUtilPct >= 25:
			gpuIntensity = "medium"
			reasons = append(reasons, fmt.Sprintf("gpu-intensity=medium (util %.0f%%)", m.GPUUtilPct))
		default:
			gpuIntensity = "low"
			reasons = append(reasons, fmt.Sprintf("gpu-intensity=low (util %.0f%%<25%%)", m.GPUUtilPct))
		}

		// GPU boundness: memory-bound if high memory pressure, else compute
		if m.MemoryPressurePct > 60 {
			gpuBound = "memory"
		} else if m.GPUUtilPct > 40 {
			gpuBound = "compute"
		} else {
			gpuBound = "mixed"
		}

		// GPU cap sensitivity
		switch {
		case gpuBound == "compute" && gpuIntensity == "high":
			gpuCapSensitivity = "high"
		case gpuIntensity == "low":
			gpuCapSensitivity = "low"
		default:
			gpuCapSensitivity = "medium"
		}
	}

	// Override from hints
	if hints.GPUSensitivity != "" {
		gpuCapSensitivity = hints.GPUSensitivity
		if gpuCapSensitivity != "none" && gpuIntensity == "none" {
			gpuIntensity = "medium" // user says sensitive, assume medium presence
		}
		reasons = append(reasons, fmt.Sprintf("gpu-cap-sensitivity=%s (annotation override)", hints.GPUSensitivity))
	}

	wp.GPU = joulie.WorkloadGPUProfile{
		Intensity:         gpuIntensity,
		Bound:             gpuBound,
		AvgUtilizationPct: m.CPUUtilPct, // GPU util would come from nvidia-dcgm or Kepler GPU metrics
		CapSensitivity:    gpuCapSensitivity,
	}

	// --- Confidence ---
	wp.Confidence = computeConfidence(hints, m)

	// --- Classification reason ---
	wp.ClassificationReason = strings.Join(reasons, "; ")

	return wp
}

// fromHintsOnly builds a profile from static hints only (no metrics available).
// Confidence is low.
func fromHintsOnly(hints PodHints) joulie.WorkloadProfileStatus {
	cpuSens := hints.CPUSensitivity
	if cpuSens == "" {
		cpuSens = "medium"
	}
	gpuSens := hints.GPUSensitivity
	if gpuSens == "" {
		gpuSens = "none"
	}

	return joulie.WorkloadProfileStatus{
		Criticality: joulie.WorkloadCriticality{Class: hints.WorkloadClass},
		Migratability: joulie.WorkloadMigratability{
			Reschedulable:  hints.Reschedulable,
		},
		CPU: joulie.WorkloadCPUProfile{
			Intensity:      "medium",
			Bound:          "mixed",
			CapSensitivity: cpuSens,
		},
		GPU: joulie.WorkloadGPUProfile{
			Intensity:      "none",
			Bound:          "none",
			CapSensitivity: gpuSens,
		},
		ClassificationReason: "hints only (no metrics available)",
		Confidence:           0.3, // low confidence without metrics
		LastUpdated:          time.Now().UTC(),
	}
}


func computeConfidence(hints PodHints, m PodMetrics) float64 {
	confidence := 0.3 // base

	// Static hints add confidence
	if hints.WorkloadClass != "" && hints.WorkloadClass != "standard" {
		confidence += 0.2 // explicit class annotation
	}
	if hints.CPUSensitivity != "" || hints.GPUSensitivity != "" {
		confidence += 0.1
	}

	// Metrics availability adds confidence
	if m.CPUUtilPct > 0 {
		confidence += 0.2
	}
	if m.KeplerUsed && m.TotalEnergyJoules > 0 {
		confidence += 0.2 // Kepler gives strong signal
	}

	if confidence > 1.0 {
		confidence = 1.0
	}
	return confidence
}

// ClassificationSummary describes what classification method was used.
func ClassificationSummary(wp joulie.WorkloadProfileStatus) string {
	parts := []string{}
	if wp.Confidence >= 0.7 {
		parts = append(parts, "high-confidence")
	} else if wp.Confidence >= 0.5 {
		parts = append(parts, "medium-confidence")
	} else {
		parts = append(parts, "low-confidence (hints only)")
	}

	if wp.CPU.Bound != "" {
		parts = append(parts, fmt.Sprintf("cpu=%s/%s", wp.CPU.Intensity, wp.CPU.Bound))
	}
	if wp.GPU.Intensity != "none" {
		parts = append(parts, fmt.Sprintf("gpu=%s/%s", wp.GPU.Intensity, wp.GPU.Bound))
	}
	return strings.Join(parts, " ")
}
