+++
title = "WorkloadProfile Guide"
linkTitle = "WorkloadProfile"
slug = "workload-profiles"
weight = 4
+++

A `WorkloadProfile` describes the power and performance characteristics of a workload class.
It is a CRD that both the operator and the scheduler extender consume to make placement and policy decisions without requiring every pod to carry a full set of annotations.

## What WorkloadProfile is

`WorkloadProfile` is a cluster-scoped CRD (`joulie.io/v1alpha1`) that captures:

- how critical a workload class is (determines scheduler preference),
- whether workloads in this class can be migrated (determines transition guard behavior),
- how intensively the workload uses CPU and GPU resources (determines power draw classification),
- how sensitive the workload is to CPU and GPU frequency reduction (determines slowdown risk under caps).

Profiles are referenced by name or matched by label selector to pods.
They are advisory, not enforced: a pod without a matching profile still runs, it just gets less-specific treatment from the extender and operator.

## Fields

```yaml
apiVersion: joulie.io/v1alpha1
kind: WorkloadProfile
metadata:
  name: gpu-compute-bound
spec:
  # criticality: performance | standard | best-effort
  # Controls scheduler extender preference and operator demand weighting.
  criticality: performance

  # reschedulable: true if this workload class can be safely restarted on another node.
  reschedulable: false

  # cpuIntensity: high | medium | low
  # How much CPU compute this workload class consumes relative to node capacity.
  cpuIntensity: medium

  # gpuIntensity: high | medium | low
  # How much GPU compute this workload class consumes relative to node GPU capacity.
  gpuIntensity: high

  # cpuSensitivity: high | medium | low
  # How strongly completion time degrades when CPU frequency is reduced.
  cpuSensitivity: low

  # gpuSensitivity: high | medium | low
  # How strongly throughput degrades when GPU power cap is reduced.
  gpuSensitivity: high

  # podSelector: optional label selector to match pods automatically.
  podSelector:
    matchLabels:
      workload-type: gpu-compute
```

### Field semantics

**criticality**

- `performance`: the scheduler extender hard-rejects eco nodes for this workload class and prefers high-headroom performance nodes.
- `standard`: neutral treatment; the extender scores normally without class-specific adjustments. Prefers performance nodes, tolerates eco.
- `best-effort`: the extender prefers eco nodes for this class, concentrating it away from performance nodes.

**reschedulable**

When `reschedulable: true`, the operator treats running pods of this class as safe to restart on another node, shortening the transition window. When false, the node waits for these pods to finish before completing the eco transition.

**cpuIntensity / gpuIntensity**

Used by the operator for demand weighting.
A node with many `high` CPU-intensity pods counts more strongly toward performance demand.

**cpuSensitivity / gpuSensitivity**

Used by the scheduler extender to scale the score penalty on capped nodes.
A workload with `gpuSensitivity: high` receives a larger penalty on nodes where the GPU is currently under a power cap.

## How the operator consumes WorkloadProfile

The operator reads `WorkloadProfile` objects to:

1. classify active pods into demand buckets with a finer grain than plain scheduling-constraint inference,
2. weight performance demand from high-intensity profiles more strongly when computing `hpCount`,
3. honor `reschedulable: false` in the downgrade guard, treating matching pods as blocking the eco transition regardless of their scheduling affinity.

If no `WorkloadProfile` matches a pod, the operator falls back to the scheduling-constraint classification described in [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}}).

## How the scheduler extender consumes WorkloadProfile

At scheduling time, the extender:

1. looks for pod annotations (`joulie.io/workload-class`, `joulie.io/cpu-sensitivity`, `joulie.io/gpu-sensitivity`),
2. if annotations are absent, checks whether the pod matches any `WorkloadProfile` `podSelector`,
3. uses the matched profile's fields to drive filter and score logic.

Explicit pod annotations always win over profile-derived values.

## Example: annotating a pod for scheduler steering

You can bypass profile matching and drive extender behavior directly with annotations:

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

## Automatic vs manual profiles

**Automatic** (recommended for production):

- Create a `WorkloadProfile` with a `podSelector` that matches your workload's labels.
- All pods matching that selector automatically get profile-based treatment without per-pod annotations.

**Manual** (useful for testing or one-off jobs):

- Add the `joulie.io/workload-class`, `joulie.io/cpu-sensitivity`, and `joulie.io/gpu-sensitivity` annotations directly to the pod or pod template.
- No `WorkloadProfile` object is needed.

## Creating a WorkloadProfile for testing

Apply the following to test extender behavior with a specific profile:

```yaml
apiVersion: joulie.io/v1alpha1
kind: WorkloadProfile
metadata:
  name: test-performance
spec:
  criticality: performance
  reschedulable: false
  cpuIntensity: high
  gpuIntensity: high
  cpuSensitivity: high
  gpuSensitivity: high
  podSelector:
    matchLabels:
      test-profile: performance
```

Label your test pod with `test-profile: performance`.
The extender will then apply `performance` scoring and the operator will block eco transitions while the pod is running on its node.

## Automated classification with Kepler

Joulie includes a workload profile classifier in `pkg/workloadprofile/classifier` that can automatically derive `WorkloadProfile` fields from live metrics rather than relying entirely on manual annotation.

### How it works

Classification uses a two-phase approach:

1. **Static hints** — pod labels and annotations are parsed first. These are fast and zero-overhead but require the user or deployment tooling to supply them.
2. **Dynamic metrics** — Prometheus/Kepler metrics measured while the workload runs are used to infer CPU/GPU intensity and boundness automatically.

### Primary signals: utilization %

[Kepler](https://github.com/sustainable-computing-io/kepler) (Kubernetes-based Efficient Power Level Exporter) instruments the kernel via eBPF to produce per-container energy counters, scraped by Prometheus. The classifier reads three Kepler metrics over a configurable window (default 10 minutes):

| Kepler metric | Used for |
|---|---|
| `kepler_container_package_joules_total` | CPU package energy → CPU-bound ratio |
| `kepler_container_dram_joules_total` | DRAM energy → memory-bound ratio |
| `kepler_container_gpu_joules_total` | GPU energy → GPU intensity and GPU-bound ratio |

**CPU-bound vs memory-bound detection** is based on the ratio of CPU package energy to DRAM energy:

- `CPUBoundRatio = CPUEnergyJoules / TotalEnergyJoules` — high (> 0.65) → `bound: compute`, `capSensitivity: high`
- `MemoryBoundRatio = DRAMEnergyJoules / TotalEnergyJoules` — high (> 0.35) → `bound: memory`, `capSensitivity: low`

**GPU energy fraction** (`GPUEnergyJoules / TotalEnergyJoules`) sets `gpuIntensity` and `gpuBound` automatically. A fraction above 0.7 maps to `intensity: high`.

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

### Confidence score

Each classification result carries a `confidence` field (0–1). The score is built up from:

- Base: `0.3`
- Explicit workload-class annotation: `+0.2`
- Sensitivity annotations present: `+0.1`
- CPU utilization data available: `+0.2`
- Kepler energy data available: `+0.2`

A confidence below `0.5` (configurable via `MinConfidence`) means the classifier is working from static hints only and the profile should be treated as a best-effort estimate.

### Current status and future work

The current implementation uses **threshold-based heuristic rules** — effectively a hand-coded decision tree over the energy ratios described above. This is intentional: the rules are simple to audit and replace.

A future ML model would:

- Collect a labeled training dataset pairing Kepler metrics with manually-verified `WorkloadProfile` values.
- Train a lightweight multi-class classifier (Random Forest or XGBoost) on features: `CPUBoundRatio`, `MemoryBoundRatio`, `GPUEnergyFraction`, `CPUUtilPct`, job duration.
- Export as ONNX and embed in the binary, or serve via a sidecar inference service.

The classifier's `classify(hints, metrics)` function is the only code that would need to change; the rest of the pipeline (metrics reading, hint parsing, confidence computation) stays the same.

## What to read next

1. [Pod Compatibility]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}})
2. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
3. [Joulie Operator]({{< relref "/docs/architecture/operator.md" >}})
