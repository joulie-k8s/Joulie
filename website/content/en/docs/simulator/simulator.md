---
title: "Workload and Power Simulator"
weight: 10
---

The Joulie simulator lets you run full control-loop experiments on virtual clusters without real hardware. It keeps Kubernetes scheduling real while simulating hardware telemetry, power dynamics, and thermal behavior per node.

This page covers the simulator's architecture, HTTP API, and integration points. Detailed subsystems are documented on dedicated pages linked throughout.

## Architecture at a glance

The simulator extends the same control path used on real nodes:

1. Node labels define simulated hardware identity.
2. Operator resolves hardware from `NodeHardware` when available, otherwise from labels/inventory fallback.
3. Operator writes desired node profile (`NodeTwin.spec`).
4. Agent reads desired state and sends control intents.
5. Simulator emulates telemetry/control behavior per node and exposes HTTP endpoints.
6. Next reconcile loop reacts to updated simulated state.

<img src='{{< relURL "images/joulie-arch-simulator.png" >}}' alt="Joulie simulator architecture overview">

The diagram shows the end-to-end loop:

- Kubernetes keeps scheduling and pod lifecycle as source of truth.
- Joulie operator writes desired node states (`NodeTwin.spec`).
- Agent (pool or daemonset mode) translates desired state into control intents.
- Simulator receives control intents and updates per-node hardware model state.
- Simulator exposes telemetry back to the agent through HTTP, closing the loop.

This separation lets you validate control policies with realistic scheduler behavior while simulating hardware dynamics.

## Goals

- Keep Kubernetes scheduling real (real pod placement/lifecycle).
- Simulate hardware telemetry and control interfaces (CPU and GPU).
- Provide reproducible, comparable experiments across Joulie and WAO.

## Full architecture mirroring

The simulator mirrors the complete Joulie control architecture. All three control layers run in simulation:

- **Operator policy controller** — runs the full policy algorithm (static partition, queue-aware) with simulated `NodeHardware` and `NodeTwin.status`.
- **Scheduler extender** — participates in scheduling decisions for simulated KWOK pods; filter and score logic is identical to the production path.

This means experiments can exercise all scenarios (baseline, caps-only, caps + scheduler) entirely in simulation before any bare-metal deployment. For validation of the scheduler scoring formula specifically, see:

- [Scoring Formula Validation]({{< relref "/docs/experiments/scoring-formula-validation.md" >}})

## Design choice: hybrid simulation

The simulator is not a fake scheduler.

- Pod placement and pod lifetime stay real in Kubernetes.
- Simulator reads cluster state (pods/nodes) and derives synthetic hardware state per node.
- Joulie reads telemetry and sends control intents through HTTP endpoints.

This gives one source of truth for workload location: the Kubernetes API.

In [KWOK](https://kwok.sigs.k8s.io/) mode:

- API server and scheduler are real.
- Fake nodes and fake workload pods are API objects.
- Simulator drives telemetry and batch completion.
- Agent runs in `pool` mode with one logical loop per simulated node.

## Facility model

The simulator models three facility-level signals that feed `NodeTwin.status` and the scheduler extender scoring formula:

| Signal | Description |
|---|---|
| **PSU stress** | Fraction of node PSU capacity in use: `nodeP / psuCapacityW * 100` |
| **Cooling stress** | Thermal load proxy from CPU/GPU junction temperatures relative to throttle thresholds |
| **PUE proxy** | Per-tick estimate: `(IT load + cooling overhead) / IT load` |

These signals are exported as Prometheus metrics and through the `/state/{node}` HTTP endpoint. The scheduler extender reads them from `NodeTwin.status`, populated by the operator twin controller from simulator-sourced telemetry.

For the hardware model parameters behind these signals, see [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}).

### Fake Prometheus query endpoint

The simulator serves a `/api/v1/query` endpoint that returns facility metrics in standard Prometheus instant-query format. This lets the operator's facility metrics poller query the simulator directly without a real Prometheus instance.

| Query | Gauge |
|---|---|
| `datacenter_ambient_temperature_celsius` | Sinusoidal ambient temperature |
| `datacenter_total_it_power_watts` | Sum of all simulated node power |
| `datacenter_cooling_power_watts` | Derived from IT power and PUE |
| Any query containing "pue" | Simulated PUE |

To use it, set `FACILITY_PROMETHEUS_ADDRESS` on the operator to point at the simulator (e.g., `http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local:18080`).

## Simulator HTTP API

| Endpoint | Method | Purpose |
|---|---|---|
| `/telemetry/{node}` | GET | Per-node telemetry (`cpu.*`, `gpu.*`, pod counters) |
| `/control/{node}` | POST | Control actions (`rapl.set_power_cap_watts`, `dvfs.set_throttle_pct`, `gpu.set_power_cap_watts`). Returns `result=applied\|blocked\|error` |
| `/state/{node}` | GET | Current internal node state |
| `/metrics` | GET | Prometheus metrics |
| `/healthz` | GET | Health check |

## Simulator observability

The simulator exposes debug endpoints alongside Prometheus metrics:

| Endpoint | Content |
|---|---|
| `/debug/nodes` | Node selection, matched override profile, resolved model hints, current state |
| `/debug/events` | Recent telemetry/control events (ring buffer) |
| `/debug/energy` | Integrated simulated energy totals |

Detailed metric names and labels: [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}})

## Installation

See the dedicated [Installation]({{< relref "/docs/simulator/installation.md" >}}) page for Helm and from-source installation instructions.

### Node scope and class mapping

- **`SIM_NODE_SELECTOR`** — Only nodes matching this label selector are simulated. Default: `joulie.io/managed=true`.
- **`SIM_NODE_CLASS_CONFIG`** — YAML file with label-matched model overrides, applied on top of inventory/label-based hardware identity.

The preferred hardware bootstrap flow:

1. Put CPU/GPU identity on node labels.
2. Let the simulator/operator resolve that identity against the shared inventory.
3. Use `SIM_NODE_CLASS_CONFIG` only for scenario-specific overrides or calibration tweaks.

For full details on hardware profile parameters and the power model, see [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}).

## Workload trace and execution

Enable workload trace replay with:

```
SIM_WORKLOAD_TRACE_PATH=/path/to/trace.jsonl
```

The simulator loads `type=job` records, injects pods, and advances per-job CPU work units every tick. Completion time increases when DVFS/RAPL reduce node effective speed. Pod lifecycle uses delete-on-complete.

For the full trace format, workload profile fields, and physical effects of CPU/GPU intensity and sensitivity settings, see:

- [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}}) — how realistic AI traces are built
- [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}) — how traces are consumed and progressed at runtime

Helper tools:

- `simulator/cmd/workloadgen` — generate synthetic JSONL traces from distributions
- `simulator/cmd/traceextract` — normalize/extract input JSONL into simulator trace schema

## KWOK flow summary

1. Create [KWOK](https://kwok.sigs.k8s.io/) fake nodes with `type=kwok` and `joulie.io/managed=true`.
2. Taint fake nodes with `kwok.x-k8s.io/node=fake:NoSchedule`.
3. Run operator + simulator + agent pool on real node(s).
4. Set agent telemetry env vars to route to simulator HTTP.
5. Inject trace workload (pods tolerate kwok taint + select `type=kwok`).
6. Observe power/control/job-completion metrics.

Detailed algorithm docs:

- [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}})
- [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})
- [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}})

Related example: `examples/07 - simulator-gpu-powercaps/`

## Large virtual clusters with kind + KWOK

You do not need a large real hardware cluster to evaluate Joulie policies. With [kind](https://kind.sigs.k8s.io/) + [KWOK](https://kwok.sigs.k8s.io/) you can:

- Keep a real Kubernetes control plane and scheduler.
- Attach many fake worker nodes.
- Run real operator/agent/simulator control loops.
- Scale experiments to many nodes and pods with low hardware cost.

Typical flow:

1. Create a kind cluster (real control-plane + worker runtime nodes).
2. Add many KWOK fake nodes labeled `joulie.io/managed=true`.
3. Deploy simulator + agent pool + operator.
4. Run workload traces and observe throughput/energy behavior.

Practical scripts are in:

- `experiments/01-cpu-only-benchmark/scripts/10_setup_cluster.sh`
- `experiments/01-cpu-only-benchmark/scripts/20_run_benchmark.sh`

This is the model used in the benchmark experiments:

- [CPU-Only Benchmark]({{< relref "/docs/experiments/cpu-only-benchmark.md" >}})
- [Homogeneous H100 Benchmark]({{< relref "/docs/experiments/homogeneous-h100-benchmark.md" >}})

## Integration with Joulie

### `NodeTwin.spec` (desired state)

Set by the Joulie operator. Defines the desired per-node target (profile/cap).

### Telemetry backend selection

Configured via environment variables on the agent. Routes input signals and control sinks to the simulator:

| Env var | Value | Effect |
|---|---|---|
| `TELEMETRY_CPU_SOURCE` | `http` | Agent reads telemetry from simulator |
| `TELEMETRY_CPU_HTTP_ENDPOINT` | `<sim-url>/telemetry/{node}` | Telemetry read URL |
| `TELEMETRY_CPU_CONTROL` | `http` | Agent sends CPU control via simulator |
| `TELEMETRY_CPU_CONTROL_HTTP_ENDPOINT` | `<sim-url>/control/{node}` | CPU control write URL |
| `TELEMETRY_GPU_CONTROL` | `http` | Agent sends GPU control via simulator |
| `TELEMETRY_GPU_CONTROL_HTTP_ENDPOINT` | `<sim-url>/control/{node}` | GPU power-cap write URL |

### `NodeHardware` (node identity)

Published automatically by the agent. Describes discovered CPU/GPU identity and control capability — not normally authored by hand in simulator examples.

For simulator bootstrap, node labels remain the lightweight source of hardware identity. The most useful bootstrap labels:

- `joulie.io/hw.cpu-model`
- `joulie.io/hw.cpu-sockets`
- `joulie.io/hw.gpu-model`
- `joulie.io/hw.gpu-count`
- Vendor presence: `feature.node.kubernetes.io/pci-10de.present=true` (NVIDIA) or `feature.node.kubernetes.io/pci-1002.present=true` (AMD)

The operator can also infer GPU presence from allocatable extended resources like `nvidia.com/gpu` or `amd.com/gpu`.

## Validation disclaimer

GPU support has been validated in simulator mode only (no bare-metal GPU access yet). Host GPU code paths are designed for NVIDIA/AMD nodes and become fully testable once real GPU nodes are available.
