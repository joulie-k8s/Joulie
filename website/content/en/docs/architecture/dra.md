---
title: "GPU Slicing Recommendations"
weight: 36
---

Joulie's digital twin analyzes historical workload patterns and recommends optimal GPU slicing configurations to cluster administrators. This is a recommendation-only system: the operator never changes GPU slicing at runtime.

## Why recommendations, not automation

NVIDIA MIG reconfiguration requires a GPU reset: all pods using the GPU must be evicted first. Automating this at runtime would be equivalent to draining nodes, adding complexity and disruption for marginal gains. Instead, Joulie follows a **suggest-and-review** model:

1. The twin observes GPU workload patterns over time (via WorkloadProfile CRs).
2. It computes the optimal slicing configuration for each GPU node.
3. It writes the recommendation to `NodeTwin.status.gpuSlicingRecommendation`.
4. The cluster admin reviews and applies the configuration during a planned maintenance window.

This keeps the operator simple and avoids surprise pod evictions.

## Recommendation model

The twin classifies GPU workloads by intensity (from WorkloadProfile `.status.gpu.intensity`):

| GPU intensity | Typical workloads | Recommended slicing |
|---------------|-------------------|---------------------|
| **low** | Monitoring, light inference, data preprocessing | MIG `1g.10gb` (7 slices/GPU) |
| **medium** | Fine-tuning, batch inference, medium training | MIG `3g.40gb` (2 slices/GPU) |
| **high** | Large-model training, full-GPU inference | Whole GPU (`none`) |

The algorithm counts workloads in each bucket and recommends the configuration that matches the dominant pattern:

- **Majority low-intensity**: small MIG slices maximize GPU sharing and power efficiency.
- **Majority medium-intensity**: medium MIG slices balance throughput and sharing.
- **Majority high-intensity**: whole-GPU allocation avoids MIG overhead.
- **No GPU workloads observed**: time-slicing as a safe, zero-disruption default.

## NodeTwin.status output

The recommendation appears in `NodeTwin.status.gpuSlicingRecommendation`:

```yaml
status:
  gpuSlicingRecommendation:
    mode: mig                # "mig", "time-slicing", or "none"
    sliceType: "3g.40gb"     # MIG profile (empty for time-slicing/none)
    slicesPerGPU: 2
    totalSlices: 16          # slicesPerGPU * GPU count
    reason: "majority of GPU workloads are medium-intensity; ..."
    estimatedUtilizationGain: 25   # predicted improvement (0-100 pct points)
    confidence: 0.85               # 0-1, based on data volume
```

The `confidence` field reflects how much data the twin has seen. Fewer than 10 GPU workloads yields low confidence (< 1.0), signaling that the admin should wait before acting.

## Prerequisites

GPU slicing recommendations require:

1. **GPU hardware with slicing support**: the agent reports `slicing.supported: true` and available modes in the `NodeHardware` CR.
2. **WorkloadProfile data**: the classifier must have produced WorkloadProfile CRs with GPU intensity data for workloads on the node. Without GPU workload data, the twin falls back to a generic time-slicing suggestion.

Nodes without GPUs or without slicing support receive no recommendation (the field is absent from their `NodeTwin.status`).

## Applying recommendations

Joulie does not apply GPU slicing changes automatically. To act on a recommendation:

1. **Review** the recommendation:
   ```bash
   kubectl get nts <node-name> -o jsonpath='{.status.gpuSlicingRecommendation}' | jq .
   ```

2. **Schedule a maintenance window**: cordon the node and drain GPU workloads.

3. **Apply the MIG configuration** using `nvidia-smi`:
   ```bash
   # Example: configure 3g.40gb profile
   sudo nvidia-smi mig -cgi 9,9 -C    # creates 2x 3g.40gb instances per GPU
   ```

4. **Uncordon the node** and let the scheduler place new workloads.

For time-slicing, no GPU reset is needed. Configure the NVIDIA device plugin with the desired replica count.

## Relationship to DRA (Dynamic Resource Allocation)

Kubernetes DRA (GA in 1.34) provides a framework for advertising and consuming device resources. In the future, Joulie may use DRA to:

- Publish GPU slices as `ResourceSlice` objects so the scheduler can match pods to specific slice sizes.
- Coordinate with the NVIDIA DRA driver for automated slice lifecycle.

For now, GPU slicing is a recommendation workflow that complements DRA rather than depending on it. The recommendation model ensures admins have actionable data regardless of whether their cluster uses DRA.

## What to read next

1. [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}})
2. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
3. [Core Concepts]({{< relref "/docs/getting-started/00-core-concepts.md" >}})
