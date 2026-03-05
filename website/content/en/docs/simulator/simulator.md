---
title: "Workload and Power Simulator"
---


This document defines the Joulie simulator design and how it integrates with Joulie.

## Goals

- Keep Kubernetes scheduling real (real pod placement/lifecycle).
- Simulate hardware telemetry and control interfaces (RAPL/DVFS now, GPU later).
- Provide reproducible, comparable experiments across Joulie and WAO.

## Design choice: hybrid simulation

The simulator is not a fake scheduler.

- Pod placement and pod lifetime stay real in Kubernetes.
- Simulator reads cluster state (pods/nodes) and derives synthetic hardware state per node.
- Joulie reads telemetry and sends control intents through HTTP endpoints.

This gives one source of truth for workload location: Kubernetes API.

In KWOK mode:

- API server and scheduler are real.
- fake nodes and fake workload pods are API objects.
- simulator drives telemetry and batch completion.
- agent runs in `pool` mode with one logical loop per simulated node.

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

## Simulator HTTP API

- `GET /telemetry/{node}`
  - returns simulated per-node telemetry (`cpu.packagePowerWatts`, throttle, cap, pod count).
- `POST /control/{node}`
  - accepts actions like `rapl.set_power_cap_watts`, `dvfs.set_throttle_pct`.
  - returns `result=applied|blocked|error`.
- `GET /state/{node}`
  - returns current internal node state.
- `GET /metrics`
  - Prometheus metrics.
- `GET /healthz`
  - health check.

## Simulator observability

The simulator exports:

- request counters and latency by route/method/status,
- control action counters by node/action,
- per-node simulated cap/throttle/power,
- per-node running pod count observed from Kubernetes.
- per-node class assignment metric (`joulie_sim_node_class_info{node,class}`).
- node utilization/frequency/cap metrics (`joulie_sim_node_cpu_util`, `joulie_sim_node_freq_scale`, `joulie_sim_node_rapl_cap_watts`).
- batch metrics (`joulie_sim_job_submitted_total`, `joulie_sim_job_completed_total`, `joulie_sim_job_completion_seconds`).

It also exposes debug endpoints:

- `GET /debug/nodes`: node selection/class/model + current node state.
- `GET /debug/events`: recent telemetry/control events (ring buffer).

## Code layout

- `simulator/cmd/simulator/main.go`: simulator binary
- `simulator/Dockerfile`: simulator container build
- `simulator/deploy/simulator.yaml`: namespace, RBAC, deployment, service
- `simulator/deploy/servicemonitor.yaml`: optional Prometheus scraping

## Deployment model

Use a separate simulator image and deployment.

This keeps Joulie runtime clean while enabling controlled experiments.

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

1. Create KWOK fake nodes with `type=kwok` and `joulie.io/managed=true`.
2. Taint fake nodes with `kwok.x-k8s.io/node=fake:NoSchedule`.
3. Run operator + simulator + agent pool on real node(s).
4. Route `TelemetryProfile` to simulator HTTP.
5. Inject trace workload (pods tolerate kwok taint + select `type=kwok`).
6. Observe power/control/job-completion metrics.

For exact equations and control/workload update math:

- [Simulator Algorithms](./simulator-algorithms/)
