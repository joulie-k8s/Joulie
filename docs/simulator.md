# Simulator Architecture and Integration

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

### Power model parameters

Current simulated power model:

`power = baseIdleW + (runningPods * podW) - (throttlePct * dvfsDropWPerPct)`

then clamped to `[20W, capWatts + raplHeadW]`.

- `baseIdleW`: baseline node power.
- `podW`: per-running-pod incremental power.
- `dvfsDropWPerPct`: per-percent DVFS power reduction.
- `raplHeadW`: temporary cap headroom.
- `defaultCapW`: initial node cap before control actions.

## Next iteration scope (agreed design)

### 1. Cluster + fake hardware bootstrap

- Start from a scenario file (node count + per-node HW profile).
- Create virtual cluster (`kind`/`k3d`/`k3s`).
- Inject node capabilities (labels + extended resources), including fake GPUs.
- Goal: scheduler must see realistic allocatable resources (for example GPU requests).

### 2. Workload simulation/replay

- Run real Kubernetes pods/jobs (so placement is real).
- Initial mode: synthetic workload generator.
- Future mode: replay workload traces from telemetry.

### 3. Runtime closed loop

- Simulator watches pod allocation per node.
- Simulator updates node telemetry state based on:
  - current pod load on that node,
  - latest control actions (`rapl.*`, `dvfs.*`).
- Agent consumes telemetry via HTTP and writes controls via HTTP.

### 4. Batch-duration feedback model

For batch workloads, duration must depend on throttling:

- jobs have virtual progress managed by simulator,
- effective progress rate depends on simulated node performance,
- stronger throttling -> slower progress -> longer completion time.

This keeps scheduling real while making control impact visible on job completion.
