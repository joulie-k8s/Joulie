---
title: "GPU Support (NVIDIA + AMD)"
weight: 10
---

Joulie supports node-level GPU power-cap intents for NVIDIA and AMD.

## Validation status

GPU support has been validated in simulator mode only (no bare-metal GPU access yet).
The host code paths are designed to work on bare metal (NVIDIA + AMD) when GPU nodes are available.

## Contract model

`NodePowerProfile.spec.gpu.powerCap` defines a per-GPU cap intent:

- `scope: perGpu`
- `capWattsPerGpu` (absolute, optional)
- `capPctOfMax` (percentage, optional)

Precedence:

1. `capWattsPerGpu` if present
2. otherwise `capPctOfMax`

The same cap is applied uniformly to all GPUs on the node.

## Heterogeneous nodes

Joulie supports heterogeneous GPU fleets by profile percentages:

- performance profile: `capPctOfMax=100`
- eco profile: `capPctOfMax` lower than 100 (for example 60)

Optional deterministic mode (simulator-oriented): operator can resolve percentages to absolute watts using model mapping (`GPU_MODEL_CAPS_JSON`) and write `capWattsPerGpu`.

## Agent host backends

- NVIDIA: host backend uses NVIDIA tooling (power-limit set per device).
- AMD: host backend uses ROCm SMI tooling (`rocm-smi`) where supported.

When capabilities are unavailable/unsupported, status is reported as `blocked` rather than failing the whole reconcile loop.

## Simulator mode

Simulator control endpoint accepts `gpu.set_power_cap_watts` with `capWattsPerGpu`.

Simulator telemetry includes:

- `gpu.present`
- `gpu.vendor`
- `gpu.count`
- `gpu.powerWattsTotal`
- `gpu.capWattsPerGpuApplied`
- `gpu.utilization`

## Scheduling guidance

Keep workload intent guidance unchanged:

- performance-sensitive pods: prefer `NotIn ["eco"]`
- eco-only (advanced): `In ["eco"]` and optionally `draining=false`

GPU resource requests (`nvidia.com/gpu`, `amd.com/gpu`) are orthogonal to Joulie power-profile labels.
Joulie GPU capping is node-level and not a GPU slicing API.

## Example

See:

- `examples/07 - simulator-gpu-powercaps/`
