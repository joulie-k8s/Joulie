---
title: "KWOK and Standalone Modes"
weight: 18
---

Outside [emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}}) the simulator models each node from its labels: it keeps per-node power, cap and thermal state in memory and serves it over HTTP, with no files and no GPU tools. It runs in one of two ways:

- **KWOK mode**: a Kubernetes cluster whose worker nodes are [KWOK](https://kwok.sigs.k8s.io/) fake nodes. The real controller manager, scheduler extender and agent in [pool mode]({{< relref "/docs/getting-started/02-agent-runtime-modes.md" >}}) run against the simulator's [HTTP API](#http-api). The benchmark experiments run this way.
- **Standalone mode** (`SIM_STANDALONE=true`): no Kubernetes. The simulator places the jobs of a trace and runs the policy baselines itself, in simulated time.

<img src='{{< relURL "images/joulie-arch-simulator.png" >}}' alt="Joulie simulator architecture overview">

## How KWOK mode works

1. Node labels give each fake node its simulated hardware.
2. The controller manager resolves that hardware and writes the desired profile and caps into `NodeTwin.spec`.
3. The agent (pool mode) reads `NodeTwin.spec` and sends control intents to `/control/{node}`.
4. The simulator applies them, advances power, energy and job progress, and serves telemetry on `/otlp/telemetry/{node}`.
5. The next reconcile reacts to the new state; the scheduler extender scores pods from `NodeTwin.status`.

Set it up:

1. Create a cluster (for example [kind](https://kind.sigs.k8s.io/)) and add KWOK fake nodes labelled `type=kwok` and `joulie.io/managed=true`, tainted `kwok.x-k8s.io/node=fake:NoSchedule`.
2. Install the simulator ([Installation]({{< relref "/docs/simulator/installation.md" >}})) and Joulie with the agent in pool mode, pointed at the simulator (below).
3. Give the simulator a trace (`SIM_WORKLOAD_TRACE_PATH`), or create pods that tolerate the KWOK taint and select `type=kwok`.
4. Watch power, caps and job completion in [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}).

Runnable versions: [`experiments/kwok-cluster`](https://github.com/joulie-k8s/Joulie/tree/main/experiments/kwok-cluster), and the experiment scripts `experiments/01-cpu-only-benchmark/scripts/10_setup_cluster.sh` and `20_run_benchmark.sh` ([CPU-Only Benchmark]({{< relref "/docs/experiments/cpu-only-benchmark.md" >}})).

### Pointing Joulie at the simulator

`<sim-url>` is the simulator's Service, for example `http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local`.

| Component | Variable | Value |
|---|---|---|
| agent | `TELEMETRY_CPU_SOURCE` | `http` |
| agent | `TELEMETRY_CPU_HTTP_ENDPOINT` | `<sim-url>/otlp/telemetry/{node}` |
| agent | `TELEMETRY_CPU_CONTROL`, `TELEMETRY_GPU_CONTROL` | `http` |
| agent | `TELEMETRY_CPU_CONTROL_HTTP_ENDPOINT`, `TELEMETRY_GPU_CONTROL_HTTP_ENDPOINT` | `<sim-url>/control/{node}` |
| controller manager | `NODE_POWER_SOURCE` | `http`, or `static` for 0 W |
| controller manager | `NODE_POWER_HTTP_ENDPOINT` | `<sim-url>/otlp/telemetry/{node}` |
| controller manager | `FACILITY_PROMETHEUS_ADDRESS` | `<sim-url>`, to read the [facility model](#facility-model) |

The chart's default `NODE_POWER_SOURCE=prometheus` reads a Prometheus that a KWOK cluster usually lacks, so every node would read 0 W. Every variable is in the [Configuration Reference]({{< relref "/docs/getting-started/05-configuration-reference.md" >}}).

## Node scope and hardware identity

| Setting | Effect |
|---|---|
| `SIM_NODE_SELECTOR` | the label selector of the modelled nodes; empty models every Node, the chart sets `joulie.io/managed=true` |
| node labels | the hardware: `joulie.io/hw.cpu-model`, `joulie.io/hw.cpu-sockets`, `joulie.io/hw.gpu-model`, `joulie.io/hw.gpu-count`, resolved against the hardware catalogue |
| `SIM_HARDWARE_CATALOG_PATH` | a catalogue YAML that replaces the embedded one; clear it (`--set env.SIM_HARDWARE_CATALOG_PATH=`) when the chart's catalogue ConfigMap is empty |
| `SIM_NODE_CLASS_CONFIG` | label-matched classes that override model parameters per node |

[Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}) has every model parameter; [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}}) says how a node's state advances.

## Facility model

The simulator models PSU stress, cooling stress and a PUE proxy, which the controller manager reads into `NodeTwin.status` and the scheduler extender scores with. They are exported as [Prometheus metrics]({{< relref "/docs/simulator/metrics.md#facility-metrics" >}}) and through `/state/{node}`; the outdoor ambient follows a sine around `SIM_FACILITY_AMBIENT_TEMP_C`.

Outside emulate mode `/api/v1/query` answers the controller manager's facility queries as a Prometheus would: set `FACILITY_PROMETHEUS_ADDRESS=<sim-url>` and `ENABLE_FACILITY_METRICS=true`. Its answers carry no labels, so a custom `FACILITY_*_QUERY` that groups by a label reads nothing.

## Workload traces

`SIM_WORKLOAD_TRACE_PATH=/path/to/trace.jsonl` replays a trace from startup: one pod per job at its submit offset, deleted when its work is done, slowed by the node's caps. [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}}) makes traces (`simulator/cmd/workloadgen`; `simulator/cmd/traceextract` converts other JSONL) and [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}) says how they run.

## Standalone mode

Standalone mode runs a trace on an inventory of nodes with no Kubernetes, as fast as the CPU allows, under one of three policy baselines: `A` (no Joulie, every node at full power, the default), `B` (static partition) or `C` (queue-aware).

```bash
SIM_STANDALONE=true SIM_STANDALONE_INVENTORY=inventory.yaml \
  SIM_WORKLOAD_TRACE_PATH=trace.jsonl SIM_STANDALONE_BASELINE=C \
  SIM_DEBUG_PERSIST_DIR=/tmp/sa go run ./simulator/cmd/simulator
```

| Variable | Default | Effect |
|---|---|---|
| `SIM_STANDALONE_INVENTORY` | none | YAML `nodes` list: `node_name`, or `node_name_prefix` and `replicas`, then hardware keys (`cpu`, `cpu_sockets`, `product`, `gpu_count`, ...) or an emulate-mode `profile` |
| `SIM_STANDALONE_BASELINE` | `A` | The policy baseline |
| `SIM_SA_DT` | `5` | Simulated seconds per tick |
| `SIM_SA_CPU_ECO_PCT` | `60` | Eco CPU setting, in percent: a frequency scale, or a CPU power cap where the node has one |
| `SIM_SA_GPU_ECO_PCT` | `70` | Eco GPU cap, in percent of the maximum |
| `SIM_SA_HP_FRAC` | `0.30` | Baseline B: fraction of performance nodes |
| `SIM_SA_HP_BASE_FRAC`, `SIM_SA_HP_MIN`, `SIM_SA_HP_MAX` | `0.30`, `1`, `25` | Baseline C: base fraction and bounds of the performance nodes |
| `SIM_SA_PERF_PER_HP_NODE` | `10` | Baseline C: performance pods per performance node |
| `SIM_SA_RECONCILE_INTERVAL` | `300` | Baseline C: simulated seconds between policy decisions |
| `SIM_SA_SUBSTEP` | `SIM_SA_DT` | Longest physics substep ([Simulator physics step]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-physics-step" >}})) |

`timeseries.csv` in `SIM_DEBUG_PERSIST_DIR` records the run; `scripts/standalone_sweep.py` runs the baselines over an experiment configuration.

## HTTP API

The simulator listens on `SIM_ADDR` (default `127.0.0.1:18080`, and `:18080` in a pod). The API has no authentication: expose it only to trusted clients.

| Method | Path | Purpose |
|---|---|---|
| GET | `/otlp/telemetry/{node}` | One node's telemetry as OTLP JSON, with the names of [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}) |
| GET | `/telemetry/{node}` | The same; `?format=legacy` serves the deprecated JSON until v0.4.0 |
| POST | `/control/{node}` | Control actions (`rapl.set_power_cap_watts`, `dvfs.set_throttle_pct`, `gpu.set_power_cap_watts`); an emulated node answers 409 |
| GET | `/state/{node}` | Current internal node state |
| GET | `/metrics` | [Prometheus metrics]({{< relref "/docs/simulator/metrics.md" >}}) |
| GET | `/healthz` | Health check |
| GET | `/readyz` | Emulate mode: 200 once every node tree exists |
| GET | `/api/v1/query` | Facility metrics as a Prometheus instant query; not in emulate mode |
| GET | `/exporters/<name>/metrics` | Emulate mode: the [compatibility exporters]({{< relref "/docs/simulator/metrics.md#compatibility-exporters-emulate-mode" >}}) listed in `SIM_EXPORTERS` |
| POST, GET, POST | `/sim/v1/runs`, `/sim/v1/runs/current`, `/sim/v1/runs/current/finish` | Emulate mode: the [run API]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-workloads-and-scenarios" >}}) that starts, reads and ends a workload run |
| GET | `/debug/nodes` | Every node's selection, node class, model and state |
| GET | `/debug/jobs` | The jobs of the trace replayed from startup |
| GET | `/debug/events` | Recent telemetry and control events, newest last (`SIM_DEBUG_EVENT_LIMIT`, default 200) |
| GET | `/debug/energy` | Integrated energy per node and for the cluster |
| GET | `/debug/timeseries` | `timeseries.csv` from `SIM_DEBUG_PERSIST_DIR` |
| GET | `/debug/emulate` | Emulate mode: the latest snapshot of every emulated node and of the facility |

Label resolution, facility formulas and queries, the standalone inventory and eco rules, and each endpoint's request and response: [Simulator modes](https://github.com/joulie-k8s/Joulie/wiki/Simulator-Modes) on the wiki.
