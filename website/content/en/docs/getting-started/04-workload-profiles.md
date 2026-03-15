+++
title = "WorkloadProfile Guide"
linkTitle = "WorkloadProfile"
slug = "workload-profiles"
weight = 4
+++

A `WorkloadProfile` describes the power and performance characteristics of a workload.
It is a namespaced CRD (`joulie.io/v1alpha1`) that the operator's classifier populates automatically and that both the operator and the scheduler extender consume for placement and policy decisions.

## What WorkloadProfile is

`WorkloadProfile` captures:

- how critical a workload is (determines scheduler preference),
- whether the workload can be migrated (determines transition guard behavior),
- how intensively the workload uses CPU and GPU resources (determines power draw classification),
- how sensitive the workload is to CPU and GPU frequency reduction (determines slowdown risk under caps),
- why the classifier reached its conclusion (`classificationReason`).

Profiles are created automatically by the classifier for each observed workload. Users interact with them indirectly via pod annotations (`joulie.io/workload-class`, sensitivity annotations) or by inspecting the profile status.

## CRD structure

The `spec` identifies the workload. The `status` holds classification output:

```yaml
apiVersion: joulie.io/v1alpha1
kind: WorkloadProfile
metadata:
  name: llm-training-job
  namespace: default
spec:
  workloadRef:
    kind: Job
    namespace: default
    name: llm-training-v2
status:
  criticality:
    class: standard            # performance | standard | best-effort
  migratability:
    reschedulable: true
  cpu:
    intensity: medium          # high | medium | low
    bound: memory              # compute | memory | io | mixed
    avgUtilizationPct: 45
    capSensitivity: low        # high | medium | low
  gpu:
    intensity: high            # high | medium | low | none
    bound: compute             # compute | memory | mixed | none
    avgUtilizationPct: 92
    capSensitivity: high       # high | medium | low | none
  classificationReason: "cpu-intensity=medium (util 45%); cpu-bound=memory (mem-pressure 68%>50%); gpu-intensity=high (util 92%≥70%)"
  confidence: 0.85
  lastUpdated: "2026-03-13T12:00:00Z"
```

## Status field semantics

**criticality.class**

- `performance`: the scheduler extender hard-rejects eco nodes for this workload and prefers high-headroom performance nodes.
- `standard`: neutral treatment; the extender scores normally without class-specific adjustments. Prefers performance nodes, tolerates eco.
- `best-effort`: the extender prefers eco nodes for this workload, concentrating it away from performance nodes.

**migratability.reschedulable**

When `true`, the operator treats running pods as safe to restart on another node, shortening the transition window. When `false`, the node waits for these pods to finish before completing the eco transition.

**cpu / gpu intensity**

Used by the operator for demand weighting.
A node with many `high` CPU-intensity pods counts more strongly toward performance demand.

**cpu / gpu capSensitivity**

Used by the scheduler extender to scale the score penalty on capped nodes.
A workload with `gpu.capSensitivity: high` receives a larger penalty on nodes where the GPU is currently under a power cap.

**cpu.bound**

Indicates the dominant resource constraint for the workload's CPU usage:

| Value | Meaning |
|-------|---------|
| `compute` | CPU-bound: high CPU utilization, sensitive to frequency/power cap |
| `memory` | Memory-bound: high DRAM pressure, less sensitive to CPU cap |
| `io` | IO-bound: low CPU utilization, cap has minimal impact |
| `mixed` | No single resource dominates |

**classificationReason**

A human-readable audit trail explaining each classification decision. The reason tracks:

- which intensity thresholds were triggered (e.g., `cpu-intensity=high (util 90%≥75%)`),
- which boundness heuristic matched (e.g., `cpu-bound=compute, cap-sensitivity=high (util 90%>70%)`),
- whether Kepler energy ratios overrode the utilization-based classification (e.g., `kepler override: cpu-bound=memory (mem-energy ratio 0.45>0.40)`),
- whether user annotations overrode the dynamic classification (e.g., `cpu-cap-sensitivity=low (annotation override)`).

When no metrics are available, the reason reads `hints only (no metrics available)`.

**confidence**

A score from 0 to 1 indicating how much data the classifier had:

- `0.3`: hints only (no metrics)
- `0.5`: utilization data available
- `0.7+`: Kepler energy data + explicit annotations

A confidence below `0.5` (configurable via `MinConfidence`) means the classifier is working from static hints only and the profile should be treated as a best-effort estimate.

## How the operator consumes WorkloadProfile

The operator reads `WorkloadProfile` objects to:

1. classify active pods into demand buckets with a finer grain than plain scheduling-constraint inference,
2. weight performance demand from high-intensity profiles more strongly when computing `hpCount`,
3. honor `reschedulable: false` in the downgrade guard, treating matching pods as blocking the eco transition regardless of their scheduling affinity.

If no `WorkloadProfile` matches a pod, the operator falls back to the scheduling-constraint classification described in [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}}).

## How the scheduler extender consumes WorkloadProfile

At scheduling time, the extender:

1. looks for pod annotations (`joulie.io/workload-class`, `joulie.io/cpu-sensitivity`, `joulie.io/gpu-sensitivity`),
2. if annotations are absent, checks for a matching `WorkloadProfile`,
3. uses the profile's status fields to drive filter and score logic.

Explicit pod annotations always win over profile-derived values.

## Example: annotating a pod for scheduler steering

You can drive extender behavior directly with annotations, without waiting for the classifier:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-gpu-inference
  annotations:
    joulie.io/workload-class: performance
    joulie.io/gpu-sensitivity: high
    joulie.io/cpu-sensitivity: low
spec:
  containers:
  - name: inference
    image: ghcr.io/example/inference:latest
    resources:
      limits:
        nvidia.com/gpu: "1"
```

With `workload-class: performance` and `gpu-sensitivity: high`, the extender will:

- hard-reject eco nodes during filtering,
- apply a larger score penalty on nodes where the GPU is currently capped,
- prefer nodes with high power headroom and low facility stress.

## Automatic vs manual classification

**Automatic** (default):

- The classifier watches running pods, fetches Prometheus/Kepler metrics, and creates or updates `WorkloadProfile` CRs automatically.
- Pod annotations (`joulie.io/workload-class`, sensitivity annotations) seed the classifier with high-confidence hints.
- No manual `WorkloadProfile` creation is needed.

**Manual** (useful for testing or overriding):

- Add `joulie.io/workload-class`, `joulie.io/cpu-sensitivity`, and `joulie.io/gpu-sensitivity` annotations directly to the pod or pod template.
- The classifier will incorporate these as high-confidence hints that override metric-derived values.

## Automated classification with Kepler

Joulie includes a workload profile classifier in `pkg/workloadprofile/classifier/` that automatically derives `WorkloadProfile` status fields from live metrics.

### How it works

Classification uses a two-phase approach:

1. **Static hints**: pod labels and annotations are parsed first. These are fast and zero-overhead but require the user or deployment tooling to supply them.
2. **Dynamic metrics**: Prometheus/Kepler metrics measured while the workload runs are used to infer CPU/GPU intensity and boundness automatically.

### Classification rules

The classifier uses utilization percentage as the primary signal, with Kepler energy ratios as enrichment:

**CPU intensity** (from `CPUUtilPct`):

| Utilization | Intensity |
|-------------|-----------|
| ≥ 75% | `high` |
| ≥ 30% | `medium` |
| < 30% | `low` |

**CPU boundness** (primary: utilization + memory pressure; enrichment: Kepler energy ratios):

| Condition | Bound | Cap sensitivity |
|-----------|-------|----------------|
| GPU dominant (GPUUtilPct > 40 and > CPUUtilPct) | `mixed` | `low` |
| CPUUtilPct > 65 and MemoryPressurePct < 50 | `compute` | `high` (if util > 70%) |
| MemoryPressurePct > 50 and CPUUtilPct < 60 | `memory` | `low` |
| CPUUtilPct < 20 | `io` | `low` |

When Kepler is available, energy ratios can override the utilization-based classification:

- `CPUBoundRatio > 0.70` → override to `compute`
- `MemoryBoundRatio > 0.40` → override to `memory`

**GPU intensity** (from `GPUUtilPct` or Kepler GPU energy):

| Utilization | Intensity |
|-------------|-----------|
| ≥ 70% | `high` |
| ≥ 25% | `medium` |
| < 25% | `low` |

### Kepler metrics used

[Kepler](https://github.com/sustainable-computing-io/kepler) (Kubernetes-based Efficient Power Level Exporter) instruments the kernel via eBPF to produce per-container energy counters, scraped by Prometheus. The classifier reads three Kepler metrics over a configurable window (default 10 minutes):

| Kepler metric | Used for |
|---|---|
| `kepler_container_package_joules_total` | CPU package energy → CPU-bound ratio |
| `kepler_container_dram_joules_total` | DRAM energy → memory-bound ratio |
| `kepler_container_gpu_joules_total` | GPU energy → GPU intensity and GPU-bound ratio |

### Confidence without Kepler

When Kepler is not available, utilization % signals are still used and classification remains functional. Confidence is slightly lower because Kepler energy ratios provide a stronger signal for compute-vs-memory distinction when CPU and memory utilization are both elevated.

### Installing Kepler

```bash
helm repo add kepler https://sustainable-computing-io.github.io/kepler-helm-chart
helm repo update
helm install kepler kepler/kepler \
  --namespace monitoring \
  --create-namespace \
  --set serviceMonitor.enabled=true
```

Verify Kepler is scraping by querying Prometheus:

```promql
sum(rate(kepler_container_package_joules_total[5m])) by (pod_name, namespace)
```

### Supported pod annotations for seeding the classifier

The classifier reads the following annotations as high-confidence hints that override metric-derived values:

| Annotation | Values |
|---|---|
| `joulie.io/workload-class` | `performance`, `standard`, `best-effort` |
| `joulie.io/reschedulable` | `true`, `false` |
| `joulie.io/cpu-sensitivity` | `high`, `medium`, `low` |
| `joulie.io/gpu-sensitivity` | `high`, `medium`, `low` |

### Current status and future work

The current implementation uses **threshold-based heuristic rules**, effectively a hand-coded decision tree over utilization percentages and energy ratios. This is intentional: the rules are simple to audit and replace.

The `classificationReason` field provides a built-in audit trail, making it possible to verify and debug classification decisions without reading source code.

A future ML model would:

- Collect a labeled training dataset pairing Kepler metrics with manually-verified `WorkloadProfile` values.
- Train a lightweight multi-class classifier (Random Forest or XGBoost) on features: `CPUBoundRatio`, `MemoryBoundRatio`, `GPUEnergyFraction`, `CPUUtilPct`, job duration.
- Export as ONNX and embed in the binary, or serve via a sidecar inference service.

The classifier's `classify(hints, metrics)` function is the only code that would need to change; the rest of the pipeline (metrics reading, hint parsing, confidence computation, reason tracking) stays the same.

## Inspecting WorkloadProfiles

Use `kubectl` to view classification results:

```bash
# List all profiles with key columns
kubectl get workloadprofiles

# View full status including classification reason
kubectl get wp llm-training-job -o yaml

# View just the classification reason
kubectl get wp llm-training-job -o jsonpath='{.status.classificationReason}'
```

The `wp` short name is registered for convenience.

## What to read next

1. [Pod Compatibility]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}})
2. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
3. [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}})
