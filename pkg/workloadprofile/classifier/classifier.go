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
	"math/rand"
	"strconv"
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
	// SimAnnotationFallback enables reading sim.joulie.io/* annotations as a
	// fallback when Prometheus metrics are unavailable. This is used in
	// simulation environments where the simulator exposes per-pod utilization
	// data via pod annotations instead of real Prometheus metrics.
	SimAnnotationFallback bool
	// SimNoisePct adds Gaussian noise to sim annotation values to simulate
	// measurement error. A value of 10 means +/-10% noise on utilization
	// values. Zero disables noise. This makes the classifier occasionally
	// cross heuristic thresholds and produce realistic misclassifications.
	SimNoisePct float64
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
	// WorkloadClass: "performance" | "standard"
	// Set via joulie.io/workload-class annotation.
	WorkloadClass string
	// Reschedulable: pod can be safely restarted/rescheduled.
	// Set via joulie.io/reschedulable=true annotation.
	Reschedulable bool
}

// ParsePodHints extracts classification hints from pod labels and annotations.
//
// Supported annotations:
//
//	joulie.io/workload-class     = "performance" | "standard"
//	joulie.io/reschedulable      = "true" | "false"
//
// Supported labels (for compatibility with existing workload labels):
//
//	joulie.io/power-profile      = "eco" → maps to standard
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
		hints.WorkloadClass = "standard"
	}

	hints.Reschedulable = annotations["joulie.io/reschedulable"] == "true"

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

	// Fetch metrics from Prometheus.
	m, err := c.metrics.FetchPodMetrics(ctx, namespace, podName, c.cfg.MetricsWindow)
	if err != nil {
		// Fallback: read sim annotations when Prometheus is unavailable.
		if c.cfg.SimAnnotationFallback {
			if simM, ok := ParseSimAnnotations(annotations, c.cfg.SimNoisePct); ok {
				simM.PodName = podName
				simM.Namespace = namespace
				wp := c.classify(hints, simM)
				// Derive criticality class from utilization if no explicit hint.
				if hints.WorkloadClass == "" || hints.WorkloadClass == "standard" {
					wp.Criticality.Class = deriveClassFromMetrics(simM)
				}
				return wp, nil
			}
		}
		log.Printf("classifier: metrics unavailable for %s/%s (%v), using hints only", namespace, podName, err)
		return fromHintsOnly(hints), nil
	}

	wp := c.classify(hints, m)
	// Derive criticality class from utilization if no explicit hint.
	if hints.WorkloadClass == "" || hints.WorkloadClass == "standard" {
		if c.cfg.SimAnnotationFallback {
			wp.Criticality.Class = deriveClassFromMetrics(m)
		}
	}
	return wp, nil
}

// ParseSimAnnotations reads simulated utilization data from pod annotations.
// The simulator sets these annotations from the trace workloadProfile data,
// allowing the classifier to classify pods without real Prometheus metrics.
// Returns (PodMetrics, true) if at least one sim annotation was found.
func ParseSimAnnotations(annotations map[string]string, noisePct float64) (PodMetrics, bool) {
	if annotations == nil {
		return PodMetrics{}, false
	}
	var m PodMetrics
	found := false

	if v, err := strconv.ParseFloat(annotations["sim.joulie.io/cpu-util-pct"], 64); err == nil {
		m.CPUUtilPct = addNoise(v, noisePct)
		m.CPUUsageCores = m.CPUUtilPct / 100.0 // normalized
		m.CPURequestCores = 1.0
		found = true
	}
	if v, err := strconv.ParseFloat(annotations["sim.joulie.io/gpu-util-pct"], 64); err == nil {
		m.GPUUtilPct = addNoise(v, noisePct)
		found = true
	}
	if v, err := strconv.ParseFloat(annotations["sim.joulie.io/memory-pressure-pct"], 64); err == nil {
		m.MemoryPressurePct = addNoise(v, noisePct)
		found = true
	}

	// Derive classification ratios (same logic as FetchPodMetrics).
	if m.CPUUtilPct > 0 {
		maxOther := m.GPUUtilPct
		if m.MemoryPressurePct > maxOther {
			maxOther = m.MemoryPressurePct
		}
		if m.CPUUtilPct > 60 && maxOther < 30 {
			m.CPUBoundRatio = m.CPUUtilPct / 100.0
		}
	}
	if m.MemoryPressurePct > 50 && m.CPUUtilPct < 60 {
		m.MemoryBoundRatio = m.MemoryPressurePct / 100.0
	}
	m.GPUDominant = m.GPUUtilPct > 40 && m.GPUUtilPct > m.CPUUtilPct

	return m, found
}

// addNoise adds Gaussian noise to a value. noisePct=10 means +/-10% noise.
// Result is clamped to [0, 100].
func addNoise(val, noisePct float64) float64 {
	if noisePct <= 0 {
		return val
	}
	noise := rand.NormFloat64() * (noisePct / 100.0) * val
	v := val + noise
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return v
}

// deriveClassFromMetrics infers "performance" or "standard" from utilization
// data when no explicit workload-class annotation is present. This simulates
// what a real online classifier would do: observe the workload and decide.
//
// A pod is classified as "performance" if it is compute-intensive enough that
// power capping would materially degrade its performance:
//   - High CPU utilization (>65%) and compute-bound (low memory pressure)
//   - High GPU utilization (>50%) indicating active GPU compute
//   - GPU-dominant workload (GPU util > CPU util)
func deriveClassFromMetrics(m PodMetrics) string {
	// GPU-intensive workloads are performance-sensitive.
	if m.GPUDominant || m.GPUUtilPct > 50 {
		return "performance"
	}
	// CPU compute-bound at high utilization.
	if m.CPUUtilPct > 65 && m.MemoryPressurePct < 50 {
		return "performance"
	}
	return "standard"
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

	wp.GPU = joulie.WorkloadGPUProfile{
		Intensity:         gpuIntensity,
		Bound:             gpuBound,
		AvgUtilizationPct: m.GPUUtilPct, // GPU util from nvidia-dcgm or Kepler GPU metrics
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
	return joulie.WorkloadProfileStatus{
		Criticality: joulie.WorkloadCriticality{Class: hints.WorkloadClass},
		Migratability: joulie.WorkloadMigratability{
			Reschedulable:  hints.Reschedulable,
		},
		CPU: joulie.WorkloadCPUProfile{
			Intensity:      "medium",
			Bound:          "mixed",
			CapSensitivity: "medium",
		},
		GPU: joulie.WorkloadGPUProfile{
			Intensity:      "none",
			Bound:          "none",
			CapSensitivity: "none",
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
