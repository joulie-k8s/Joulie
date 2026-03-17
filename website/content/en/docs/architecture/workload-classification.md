---
title: "Workload Classification"
weight: 42
description: "How Joulie's classifier combines static hints and dynamic metrics to produce WorkloadProfile CRs that drive scheduling and twin decisions."
---

Joulie automatically classifies running workloads by analyzing pod annotations, resource utilization, and Kepler energy counters. The output is a `WorkloadProfile` CR per workload, consumed by the digital twin and scheduler extender.

## Why classify workloads

Kubernetes scheduling uses resource requests and limits to bin-pack pods onto nodes. These are static declarations that tell the scheduler how much CPU and memory a pod claims, but nothing about:

- whether the workload is compute-bound or memory-bound,
- how sensitive it is to power capping,
- how much GPU energy it actually consumes vs. what it reserves,
- whether it can tolerate being rescheduled.

Without this information, energy-aware scheduling is limited to node-level heuristics. Joulie's classifier fills this gap by producing structured workload profiles from observed behavior.

## Two-phase classification

The classifier uses a two-phase approach: static hints first, then dynamic metrics.

### Phase 1: Static hints

When a pod is first observed, the classifier reads pod annotations as high-confidence hints:

| Annotation | Effect |
|------------|--------|
| `joulie.io/workload-class` | Sets criticality class (`performance` or `standard`) |
| `joulie.io/reschedulable` | Sets migratability |
| `joulie.io/cpu-sensitivity` | Sets CPU cap sensitivity (`high`, `medium`, `low`) |
| `joulie.io/gpu-sensitivity` | Sets GPU cap sensitivity (`high`, `medium`, `low`) |

Static hints are available immediately at pod creation. They require zero metrics and produce a `WorkloadProfile` with confidence 0.3 (hints only). This ensures that even newly-created pods have a classification the scheduler can use.

When annotations are present, they always take precedence over metric-derived values. This lets operators override the classifier for workloads whose behavior is well-understood.

### Phase 2: Dynamic metrics

After the initial measurement window (default 10 minutes), the classifier queries Prometheus for utilization and energy metrics. It then applies threshold-based rules to infer intensity, boundness, and cap sensitivity.

**Primary signal: utilization percentage.** CPU and GPU utilization are the most reliable and universally available metrics. They do not require Kepler or any special instrumentation.

**Enrichment signal: Kepler energy ratios.** When Kepler is deployed, the classifier reads per-container energy counters and computes ratios (CPU-bound ratio, memory-bound ratio, GPU energy fraction). These ratios can override the utilization-based classification when they provide a clearer signal, particularly for distinguishing compute-bound from memory-bound workloads when both CPU and memory utilization are elevated.

The two-phase design means classification degrades gracefully:

| Available data | Confidence | Classification quality |
|----------------|------------|----------------------|
| Annotations only | 0.3 | User-specified hints, no dynamic data |
| Utilization metrics | 0.5 | Intensity and basic boundness |
| Utilization + Kepler energy | 0.7+ | Full classification with energy-ratio overrides |

## Kepler integration

[Kepler](https://github.com/sustainable-computing-io/kepler) (Kubernetes-based Efficient Power Level Exporter) uses eBPF probes attached to kernel scheduling events to attribute node-level energy consumption to individual containers. The classifier reads three Kepler metrics:

| Metric | Used for |
|--------|----------|
| `kepler_container_package_joules_total` | CPU package energy, used to compute CPU-bound ratio |
| `kepler_container_dram_joules_total` | DRAM energy, used to compute memory-bound ratio |
| `kepler_container_gpu_joules_total` | GPU energy, used to compute GPU intensity and GPU-bound ratio |

### Energy ratios

The classifier computes two ratios from Kepler data:

```
CPUBoundRatio = CPUPackageEnergy / (CPUPackageEnergy + DRAMEnergy + GPUEnergy)
MemoryBoundRatio = DRAMEnergy / (CPUPackageEnergy + DRAMEnergy)
```

These ratios provide a direct measurement of where energy is being consumed, which is more reliable than utilization alone for workloads with mixed resource profiles.

### Override rules

When Kepler data is available, the classifier applies override rules:

- `CPUBoundRatio > 0.70`: override boundness to `compute` regardless of utilization-based classification.
- `MemoryBoundRatio > 0.40`: override boundness to `memory`.

Overrides are logged in the `classificationReason` field for auditability (e.g., `kepler override: cpu-bound=memory (mem-energy ratio 0.45>0.40)`).

### Without Kepler

When Kepler is not deployed, the classifier falls back to utilization-based rules only. Classification still works; the main loss is the ability to distinguish compute-bound from memory-bound workloads when both CPU and memory utilization are moderate (30-60%). In this ambiguous range, the classifier defaults to `mixed` boundness.

### Simulator fallback (sim annotations)

When running in simulation mode without a real Prometheus, the classifier can read utilization data from `sim.joulie.io/*` pod annotations set by the simulator. Enable with `CLASSIFY_SIM_ANNOTATION_FALLBACK=true`. The classifier applies its normal heuristic rules to the annotation values, with optional Gaussian noise (`CLASSIFY_SIM_NOISE_PCT`, default 10%) to simulate measurement error. See [Online workload classification]({{< relref "/docs/simulator/simulator.md" >}}) for details.

## Confidence scoring

Every `WorkloadProfile` carries a `confidence` field (0 to 1) that indicates how much data backs the classification:

| Score | Meaning |
|-------|---------|
| 0.3 | Hints only, no metrics observed yet |
| 0.5 | Utilization metrics available |
| 0.7 | Utilization + Kepler energy data |
| 0.85+ | Utilization + Kepler + explicit user annotations |

Consumers use confidence to gate decisions. The configurable `MinConfidence` threshold (default 0.5) determines when a profile is considered reliable enough to influence scheduling. Below this threshold, the profile is treated as a best-effort estimate.

Confidence increases over time as the classifier accumulates more measurement windows. It decreases if metrics become unavailable (e.g., Kepler is uninstalled or Prometheus stops scraping a target).

## Reclassification

Workload behavior changes over time. A training job may start with data loading (IO-bound) before transitioning to GPU-intensive computation. The classifier reclassifies every 15 minutes to track these transitions.

Reclassification updates the `WorkloadProfile` status fields in place. The `lastUpdated` timestamp records when the most recent classification occurred. The `classificationReason` field is overwritten with the new reasoning.

User-provided annotations are re-read on each reclassification cycle, so annotation changes take effect within 15 minutes.

## How WorkloadProfile feeds into the twin

The digital twin consumes `WorkloadProfile` data in two ways:

### Demand weighting

The twin weights performance demand based on workload intensity. A node running three `high` CPU-intensity pods contributes more to the cluster's performance demand signal than a node running three `low` intensity pods. This weighted demand drives the operator's policy decisions about which nodes should be in performance mode vs. eco mode.

### GPU slicing recommendations

For GPU nodes, the twin reads `WorkloadProfile.status.gpu.intensity` across all workloads on a node to determine the optimal GPU slicing configuration. If most GPU workloads are low-intensity, the twin recommends fine-grained MIG slicing; if most are high-intensity, it recommends whole-GPU allocation. See [GPU Slicing Recommendations]({{< relref "/docs/architecture/dra.md" >}}).

### Scheduler extender consumption

The scheduler extender reads `WorkloadProfile` data (or equivalent pod annotations) to:

1. Determine the workload class for filter decisions (performance pods are rejected from eco nodes).
2. Apply cap-sensitivity penalties on nodes where the GPU or CPU is currently capped.
3. Steer standard workloads toward eco nodes when performance nodes are congested.

## Classification rules reference

### CPU intensity

| CPUUtilPct | Intensity |
|------------|-----------|
| >= 75% | `high` |
| >= 30% | `medium` |
| < 30% | `low` |

### CPU boundness

| Condition | Bound | Cap sensitivity |
|-----------|-------|----------------|
| GPUUtilPct > 40 and > CPUUtilPct | `mixed` | `low` |
| CPUUtilPct > 65 and MemoryPressurePct < 50 | `compute` | `high` (if util > 70%) |
| MemoryPressurePct > 50 and CPUUtilPct < 60 | `memory` | `low` |
| CPUUtilPct < 20 | `io` | `low` |

### GPU intensity

| GPUUtilPct | Intensity |
|------------|-----------|
| >= 70% | `high` |
| >= 25% | `medium` |
| < 25% | `low` |

## State of the art

Joulie's workload classification draws on research in workload characterization for large-scale clusters.

### Workload heterogeneity in cloud environments

Reiss et al. (2012) analyzed Google's cluster traces and found extreme heterogeneity in workload resource consumption patterns, with orders-of-magnitude variation in CPU and memory usage across tasks. Their analysis demonstrated that workloads cannot be treated as homogeneous for scheduling purposes. Joulie's intensity classification (high/medium/low) and boundness detection (compute/memory/io/mixed) address this heterogeneity by giving the scheduler a structured vocabulary for workload differences.

> C. Reiss, A. Tumanov, G.R. Ganger, R.H. Katz, and M.A. Kozuch. "Heterogeneity and Dynamicity of Clouds at Scale: Google Trace Analysis." Proceedings of the ACM Symposium on Cloud Computing (SoCC), 2012.

### Workload characterization methodology

Moreno et al. (2014) developed systematic methods for analyzing and modeling workload characteristics using hardware performance counters. Their approach of decomposing workloads by resource bottleneck (CPU, memory, IO) directly informs Joulie's boundness classification. The key insight is that workloads can be classified along a small number of resource dimensions, and this classification is stable enough to be useful for scheduling.

> I.S. Moreno, P. Garraghan, P. Townend, and J. Xu. "Analysis, Modeling and Simulation of Workload Patterns in a Large-Scale Utility Cloud." IEEE Transactions on Cloud Computing, 2014.

### Resource-efficient cluster management

Delimitrou and Kozyrakis (2014) showed that collaborative filtering techniques can predict workload resource needs from partial observations, enabling cluster managers to place workloads on suitable machines before full characterization is complete. Joulie follows a similar progressive-refinement philosophy: static hints provide immediate (low-confidence) classification, and dynamic metrics refine the profile over time. The confidence score explicitly tracks this progression.

> C. Delimitrou and C. Kozyrakis. "Quasar: Resource-Efficient and QoS-Aware Cluster Management." Proceedings of the International Conference on Architectural Support for Programming Languages and Operating Systems (ASPLOS), 2014.

### What Joulie adds

Existing workload classification systems either require offline trace analysis or operate within proprietary cluster managers. Joulie's classifier is:

1. **Kubernetes-native**: it reads standard Prometheus metrics and Kubernetes annotations, requiring no custom instrumentation beyond optional Kepler deployment.
2. **Progressive**: classification starts from static hints at pod creation and improves as dynamic metrics become available, with an explicit confidence score tracking data quality.
3. **Energy-aware**: by incorporating Kepler energy ratios alongside utilization metrics, it distinguishes compute-bound from memory-bound workloads based on where energy is actually consumed, not just where utilization appears high.
4. **Auditable**: the `classificationReason` field provides a human-readable explanation of every classification decision, making it straightforward to verify and debug.

## What to read next

1. [Energy-Aware Scheduling]({{< relref "/docs/architecture/energy-aware-scheduling.md" >}})
2. [WorkloadProfile Guide]({{< relref "/docs/getting-started/04-workload-profiles.md" >}})
3. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
4. [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}})
