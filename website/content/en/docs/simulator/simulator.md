---
title: "Workload and Power Simulator"
weight: 10
---


This document defines the Joulie simulator design and how it integrates with Joulie.

## Architecture at a glance

The simulator extends the same control path used on real nodes:

1. Operator writes desired node profile (`NodePowerProfile`).
2. Agent reads desired state and sends control intents.
3. Simulator emulates telemetry/control behavior per node and exposes HTTP endpoints.
4. Next reconcile loop reacts to updated simulated state.

<img src='{{< relURL "images/joulie-arch-simulator.png" >}}' alt="Joulie simulator architecture overview">

The diagram shows the end-to-end loop:

- Kubernetes keeps scheduling and pod lifecycle as source of truth.
- Joulie operator writes desired node states (`NodePowerProfile`).
- Agent (pool or daemonset mode) translates desired state into control intents.
- Simulator receives control intents and updates per-node hardware model state.
- Simulator exposes telemetry back to the agent through HTTP, closing the loop.

This separation lets you validate control policies with realistic scheduler behavior while simulating hardware dynamics.

## Goals

- Keep Kubernetes scheduling real (real pod placement/lifecycle).
- Simulate hardware telemetry and control interfaces (CPU and GPU).
- Provide reproducible, comparable experiments across Joulie and WAO.

## Validation disclaimer

GPU support has been validated in simulator mode only (no bare-metal GPU access yet).
Host GPU code paths are designed for NVIDIA/AMD nodes and become fully testable once real GPU nodes are available.

## Design choice: hybrid simulation

The simulator is not a fake scheduler.

- Pod placement and pod lifetime stay real in Kubernetes.
- Simulator reads cluster state (pods/nodes) and derives synthetic hardware state per node.
- Joulie reads telemetry and sends control intents through HTTP endpoints.

This gives one source of truth for workload location: Kubernetes API.

In [KWOK](https://kwok.sigs.k8s.io/) mode:

- API server and scheduler are real.
- fake nodes and fake workload pods are API objects.
- simulator drives telemetry and batch completion.
- agent runs in `pool` mode with one logical loop per simulated node.

## Large virtual clusters with [kind](https://kind.sigs.k8s.io/) + [KWOK](https://kwok.sigs.k8s.io/)

You are not constrained to a large real hardware cluster to evaluate Joulie policies.

With [kind](https://kind.sigs.k8s.io/) + [KWOK](https://kwok.sigs.k8s.io/) you can:

- keep a real Kubernetes control plane and scheduler,
- attach many fake worker nodes,
- run real operator/agent/simulator control loops,
- scale experiments to many nodes and pods with low hardware cost.

This is the model used in the benchmark experiment:

- [KWOK Benchmark Experiment]({{< relref "/docs/experiments/kwok-benchmark.md" >}})

Typical flow:

1. Create [kind](https://kind.sigs.k8s.io/) cluster (real control-plane + worker runtime nodes).
2. Add many [KWOK](https://kwok.sigs.k8s.io/) fake nodes labeled `joulie.io/managed=true`.
3. Deploy simulator + agent pool + operator.
4. Run workload traces and observe throughput/energy behavior.

Practical scripts are in:

- `experiments/01-kwok-benchmark/scripts/10_setup_cluster.sh`
- `experiments/01-kwok-benchmark/scripts/20_run_benchmark.sh`

Example run:

```bash
source experiments/01-kwok-benchmark/.venv/bin/activate
experiments/01-kwok-benchmark/scripts/10_setup_cluster.sh
experiments/01-kwok-benchmark/scripts/20_run_benchmark.sh
```

## Integration with Joulie

### `NodePowerProfile` (what)

- Set by Joulie operator.
- Defines desired per-node target (profile/cap).

### `TelemetryProfile` (how)

- Read by Joulie agent.
- Routes input signals and control sinks:
  - telemetry source (`host`, `http`, ...)
  - control backend (`host`, `http`, ...)

In simulator mode:

- `spec.sources.cpu.type=http` -> agent reads `/telemetry/{node}`.
- `spec.controls.cpu.type=http` -> agent writes `/control/{node}`.
- `spec.controls.gpu.type=http` -> agent writes GPU power-cap intents to `/control/{node}`.

## Simulator HTTP API

- `GET /telemetry/{node}`
  - returns simulated per-node telemetry (`cpu.*`, `gpu.*`, pod counters).
- `POST /control/{node}`
  - accepts actions like `rapl.set_power_cap_watts`, `dvfs.set_throttle_pct`, `gpu.set_power_cap_watts`.
  - returns `result=applied|blocked|error`.
- `GET /state/{node}`
  - returns current internal node state.
- `GET /metrics`
  - Prometheus metrics.
- `GET /healthz`
  - health check.

## Simulator observability

The simulator exposes Prometheus metrics and debug endpoints:

- `GET /debug/nodes`: node selection/class/model + current node state.
- `GET /debug/events`: recent telemetry/control events (ring buffer).
- `GET /debug/energy`: integrated simulated energy totals.

Detailed metric names and labels are documented in:

- [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}})

## Installation

Use a separate simulator deployment (`joulie-telemetry-sim`) in namespace `joulie-sim-demo`.

### Build and push image

From repo root:

```bash
make simulator-build TAG=<tag>
make simulator-push TAG=<tag>
```

### Deploy to cluster

Use the default manifest:

```bash
kubectl apply -f simulator/deploy/simulator.yaml
kubectl -n joulie-sim-demo rollout status deploy/joulie-telemetry-sim
```

Or install with dynamic image tag override:

```bash
make simulator-install TAG=<tag>
```

This keeps simulator lifecycle independent from operator/agent lifecycle.

### Node scope and class mapping

Current simulator supports:

- `SIM_NODE_SELECTOR`:
  - only nodes matching this label selector are simulated.
  - default in deploy manifest: `joulie.io/managed=true`.
- `SIM_NODE_CLASS_CONFIG`:
  - YAML file with classes (`matchLabels`) and model overrides.
  - used to map dynamic cluster node names to stable simulator behavior profiles.

### Hardware profile parameters

Class model overrides now support:

- `baseIdleW`, `pMaxW`
- `alphaUtil`, `betaFreq`
- `fMinMHz`, `fMaxMHz`
- `raplCapMinW`, `raplCapMaxW`
- `dvfsRampMs`

Hardware profile parsing and validation are implemented in:

- `simulator/pkg/hw/profile.go`

Invalid class/base profiles fail fast at simulator startup.

### Power model

The simulator computes:

`P = P_idle + (P_max - P_idle) * util^alpha * freqScale^beta`

Then applies:

- DVFS ramp dynamics (`dvfsRampMs`) from target throttle to effective `freqScale`.
- RAPL cap clamp via cap-aware `freqScale` solve.
- cap saturation flag when cap is below achievable minimum.

Utilization comes from trace-driven workload engine.

### Workload trace and execution

Enable with:

- `SIM_WORKLOAD_TRACE_PATH=/path/to/trace.jsonl`

The simulator loads `type=job` records, injects pods, and advances per-job CPU work units every tick.
Completion time increases when DVFS/RAPL reduce node effective speed.

Pod lifecycle currently uses delete-on-complete.

Helper tools:

- `simulator/cmd/workloadgen`: generate synthetic JSONL traces from distributions.
- `simulator/cmd/traceextract`: normalize/extract input JSONL into simulator trace schema.

## KWOK flow summary

1. Create [KWOK](https://kwok.sigs.k8s.io/) fake nodes with `type=kwok` and `joulie.io/managed=true`.
2. Taint fake nodes with `kwok.x-k8s.io/node=fake:NoSchedule`.
3. Run operator + simulator + agent pool on real node(s).
4. Route `TelemetryProfile` to simulator HTTP.
5. Inject trace workload (pods tolerate kwok taint + select `type=kwok`).
6. Observe power/control/job-completion metrics.

Algorithm details are split in:

- [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}})
- [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})
- [Hardware Modeling]({{< relref "/docs/simulator/hardware-modeling.md" >}})

Related example:

- `examples/07 - simulator-gpu-powercaps/`
