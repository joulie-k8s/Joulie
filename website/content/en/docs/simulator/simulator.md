---
title: "KWOK and Standalone Modes"
weight: 18
---

Outside [emulate mode]({{< relref "/docs/simulator/emulate-mode.md" >}}) the simulator models each node from its labels: it keeps per-node power, cap and thermal state in memory and serves it over HTTP, with no files and no GPU tools. It runs in one of two ways:

- **KWOK mode**: in a Kubernetes cluster whose API server and scheduler are real and whose worker nodes are [KWOK](https://kwok.sigs.k8s.io/) fake nodes. The real controller manager plans, the real scheduler extender scores, and the agent in [pool mode]({{< relref "/docs/getting-started/02-agent-runtime-modes.md" >}}) reads telemetry from and sends caps to the simulator's [HTTP API](#http-api). The benchmark experiments run this way.
- **Standalone mode** (`SIM_STANDALONE=true`): no Kubernetes at all. The simulator places the jobs of a trace and runs the policy baselines itself, in simulated time, as fast as the CPU allows.

<img src='{{< relURL "images/joulie-arch-simulator.png" >}}' alt="Joulie simulator architecture overview">

## How KWOK mode works

Pod placement and pod lifetime stay real in Kubernetes, the one source of truth for where work runs. The simulator only derives each node's hardware state from what runs there:

1. Node labels give each fake node its simulated hardware.
2. The controller manager resolves that hardware and writes the desired profile and caps into `NodeTwin.spec`.
3. The agent (pool mode) reads `NodeTwin.spec` and sends control intents to `/control/{node}`.
4. The simulator applies them to its model of the node, advances power, energy and job progress, and serves telemetry on `/otlp/telemetry/{node}`.
5. The next reconcile reacts to the new state; the scheduler extender scores pods from `NodeTwin.status`.

Set it up:

1. Create a cluster (for example [kind](https://kind.sigs.k8s.io/)) and add KWOK fake nodes labelled `type=kwok` and `joulie.io/managed=true`, tainted `kwok.x-k8s.io/node=fake:NoSchedule`.
2. Install the simulator ([Installation]({{< relref "/docs/simulator/installation.md" >}})) and Joulie with the agent in pool mode, pointed at the simulator (below).
3. Give the simulator a trace (`SIM_WORKLOAD_TRACE_PATH`), or create pods that tolerate the KWOK taint and select `type=kwok`.
4. Watch power, caps and job completion in [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}).

Runnable versions: [`examples/06-simulator-kwok`](https://github.com/joulie-k8s/Joulie/tree/main/examples/06-simulator-kwok), [`examples/07-simulator-gpu-powercaps`](https://github.com/joulie-k8s/Joulie/tree/main/examples/07-simulator-gpu-powercaps), and the experiment scripts `experiments/01-cpu-only-benchmark/scripts/10_setup_cluster.sh` and `20_run_benchmark.sh` ([CPU-Only Benchmark]({{< relref "/docs/experiments/cpu-only-benchmark.md" >}})).

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

- **`SIM_NODE_SELECTOR`**: the label selector of the nodes the simulator models. Empty, the default, models every Node; the chart sets `joulie.io/managed=true`.
- **Labels**: the hardware of a node comes from its labels, resolved against the hardware catalogue: `joulie.io/hw.cpu-model`, `joulie.io/hw.cpu-sockets`, `joulie.io/hw.gpu-model`, `joulie.io/hw.gpu-count`, and the vendor presence labels `feature.node.kubernetes.io/pci-10de.present=true` (NVIDIA) or `feature.node.kubernetes.io/pci-1002.present=true` (AMD). The controller manager also counts GPUs from GPU feature discovery's `nvidia.com/gpu.count`, from Intel's `gpu.intel.com/device.count` and from the node's GPU resources, MIG devices and shared replicas included ([Shared and partitioned GPUs]({{< relref "/docs/hardware/gpus.md#shared-and-partitioned-gpus" >}})). The CPU model is the first label set of `joulie.io/hw.cpu-model` and `feature.node.kubernetes.io/cpu-model.name`. The GPU model is `joulie.io/hw.gpu-model`, else the first set of the controller manager's default `GPU_PRODUCT_LABEL_KEYS`, in its order: `joulie.io/gpu.product`, `nvidia.com/gpu.product` (without GPU feature discovery's `-MIG-<profile>` and `-SHARED` suffixes), `amd.com/gpu.product-name`, `beta.amd.com/gpu.product-name`, `amd.com/gpu.product`, `gpu.intel.com/product`, `amd.com/gpu.family`.
- **`SIM_HARDWARE_CATALOG_PATH`**: a hardware catalogue YAML that replaces the one embedded in the binary; empty means the embedded one. The chart points it at the file the `joulie-simulator-hardware-catalog` ConfigMap mounts, and an empty file loads no catalogue, so clear the path (`--set env.SIM_HARDWARE_CATALOG_PATH=`) when that ConfigMap is empty.
- **`SIM_NODE_CLASS_CONFIG`**: a YAML file of label-matched classes that override model parameters per node, for a scenario or a calibration tweak.

[Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}) has every model parameter and where it comes from; [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}}) how a node's state advances.

## Facility model

The simulator models three facility signals, which the controller manager reads into `NodeTwin.status` and the scheduler extender scores with:

| Signal | Description |
|---|---|
| **PSU stress** | Fraction of node PSU capacity in use: `nodeP / psuCapacityW * 100` |
| **Cooling stress** | Thermal load proxy from CPU/GPU junction temperatures relative to throttle thresholds |
| **PUE proxy** | Per-tick estimate: `(IT load + cooling overhead) / IT load` |

They are exported as [Prometheus metrics]({{< relref "/docs/simulator/metrics.md#facility-metrics" >}}) and through `/state/{node}`. The outdoor ambient temperature follows a sine around `SIM_FACILITY_AMBIENT_TEMP_C`.

The `joulie_sim_facility_*` gauges update every `SIM_PHYSICS_TICK` in KWOK mode without a trace, every workload tick (`SIM_WORKLOAD_TICK`) with a legacy trace, every emulator tick (`SIM_EMULATE_TICK`) in emulate mode, and every virtual step (`SIM_SA_DT`) in standalone mode.

### Fake Prometheus query endpoint

`/api/v1/query` answers facility queries in the Prometheus instant-query format, so the controller manager's facility poller can read the simulator with no Prometheus: set `FACILITY_PROMETHEUS_ADDRESS` to `<sim-url>` and `ENABLE_FACILITY_METRICS=true`. A query that names a metric of the controller manager's facility presets is answered as that input, whatever the rest of the query: the `otel` preset (the default) under all four `PROMETHEUS_NAME_TRANSLATION` strategies, and the deprecated `legacy` preset.

| Facility input | Names it answers | Value |
|---|---|---|
| outdoor ambient temperature | `joulie_facility_ambient_temperature_celsius` and its spellings under the other strategies, `datacenter_ambient_temperature_celsius` | Sinusoidal ambient temperature |
| IT power | `joulie_facility_it_energy_joules_total`, `joulie_facility_it_power_watts` and their spellings, `datacenter_total_it_power_watts` | Sum of all simulated node power |
| cooling power | `joulie_facility_cooling_energy_joules_total`, `joulie_facility_cooling_power_watts` and their spellings, `datacenter_cooling_power_watts` | Derived from IT power and PUE |
| cooling zone ambient, rack power, node inlet | `joulie_cooling_zone_ambient_temperature_celsius`, `joulie_rack_energy_joules_total`, `joulie_rack_power_watts`, `hw_host_ambient_temperature_celsius` and their spellings | An empty vector: this model has no cooling zones, racks or inlet sensors, so the controller manager logs `no_data` for them once and falls back to the outdoor ambient |

A query that names none of these falls back to matching words: one containing `ambient` and `temp`, `it_power`, `cooling` and `power`, or `pue` returns that gauge (the last one the simulated PUE), and any other query an empty vector. The answer is always one series without labels, so a custom `FACILITY_*_QUERY` that groups by a key label reads nothing.

Emulate mode does not serve this endpoint: it exports the facility as OpenTelemetry metrics to a real Prometheus.

## Workload traces

`SIM_WORKLOAD_TRACE_PATH=/path/to/trace.jsonl` replays a trace from startup. The simulator loads the `type=job` records, creates one pod per job at its submit offset, advances each job's work every tick, and deletes the pod when the work is done. A cap or a frequency limit on the node slows the job, so completion times show what the policy costs. [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}}) describes the traces and [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}) how they run.

- `simulator/cmd/workloadgen` generates synthetic traces.
- `simulator/cmd/traceextract` normalizes other JSONL into the trace schema.

## Standalone mode

`SIM_STANDALONE=true` runs without Kubernetes and needs `SIM_STANDALONE_INVENTORY` (the nodes, below) and `SIM_WORKLOAD_TRACE_PATH`. `SIM_STANDALONE_BASELINE` picks the policy: `A` (no Joulie, every node at full power, the default), `B` (static partition) or `C` (queue-aware). `SIM_SA_DT` (default 5) is the simulated seconds per tick, and `timeseries.csv` in `SIM_DEBUG_PERSIST_DIR` records the run. `scripts/standalone_sweep.py` runs the baselines over an experiment configuration.

| Variable | Default | Effect |
|---|---|---|
| `SIM_SA_CPU_ECO_PCT` | `60` | Eco CPU frequency scale, in percent; on a profile node with a writable PL1, the eco CPU power cap as a share of the PL1 maximum |
| `SIM_SA_GPU_ECO_PCT` | `70` | Eco GPU cap, in percent of the maximum |
| `SIM_SA_HP_FRAC` | `0.30` | Baseline B: fraction of performance nodes |
| `SIM_SA_HP_BASE_FRAC`, `SIM_SA_HP_MIN`, `SIM_SA_HP_MAX` | `0.30`, `1`, `25` | Baseline C: base fraction and bounds of the performance nodes |
| `SIM_SA_PERF_PER_HP_NODE` | `10` | Baseline C: performance pods per performance node |
| `SIM_SA_RECONCILE_INTERVAL` | `300` | Baseline C: simulated seconds between policy decisions |
| `SIM_SA_SUBSTEP` | `SIM_SA_DT` | Longest physics substep ([Simulator physics step]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-physics-step" >}})) |

The inventory's `nodes` list describes the cluster. Each entry gives `node_name` (one node) or `node_name_prefix` and `replicas` (nodes `<prefix>-0`, `<prefix>-1`, and so on), and then either hardware keys or a `profile`:

```yaml
nodes:
  # Hardware keys: the model comes from the labels, as for a Kubernetes node.
  - node_name_prefix: gpu
    replicas: 4
    vendor: nvidia
    product: NVIDIA H100 NVL
    cpu: INTEL(R) XEON(R) GOLD 6530
    cpu_sockets: 2
    cpu_cores: 64
    memory_gib: 1024
    gpu_count: 8
    gpu_min_cap_watts: 200
    gpu_max_cap_watts: 400
  # Profile: the hardware of an emulate-mode profile.
  - node_name_prefix: rapl
    replicas: 8
    profile: intel-xeon-2s-rapl
```

**Hardware keys.** The simulator turns the keys into node labels (`joulie.io/hw.cpu-model`, `joulie.io/hw.gpu-model`, the CPU and GPU counts) and resolves the model from them as it does for a Kubernetes node: each value comes from the matching `SIM_NODE_CLASS_CONFIG` class, else the environment, else the hardware catalogue, else the built-in constant. `gpu_min_cap_watts` and `gpu_max_cap_watts`, when above 0, then set the node's GPU board power range. A maximum below the resolved minimum lowers the minimum to it; a minimum above the maximum stops the run.

**Profiles.** `profile` names an hwemu profile: a builtin one (`simulator/pkg/hwemu/profiles/`) or a local one in `SIM_PROFILES_DIR`. The hardware keys must then stay unset, and an unknown name stops the run with the list of known ones. The node gets the labels of the Node a real node of the profile registers as, plus `type=kwok`, `joulie.io/managed=true`, `joulie.io/node-name`, and `joulie.io/hw.cpu-cores` and `joulie.io/hw.gpu-count` from the profile's logical CPUs and GPUs. Its model is the profile's, whatever `SIM_NODE_SELECTOR`, the node classes and the catalogue say:

| Model value | From the profile |
|---|---|
| idle and peak CPU power | sockets x `physics.cpu.idlePkgW` and sockets x `physics.cpu.maxPkgW`; a `catalog-curve` CPU runs the catalogue's two-socket curve scaled by sockets/2 |
| frequency range and knee | `cpuinfoMinKHz` and `cpuinfoMaxKHz` of `cpufreq`; knee x the maximum |
| CPU cap range and default | the sum over the packages of the PL1 (`long_term`) maximum and limit, else of the HSMP caps; a node with neither is capped at its peak, which never binds; the minimum is 1 W |
| CPU cap settling time | the PL1 time window, or `physics.rapl.settleTau` when it is a duration, or `physics.hsmp.settleTauMS` |
| GPU range | `gpus.limits.minW` (1 W when it is 0) to `physics.gpu.naturalMaxW` |
| thermal throttling | off, as in emulate mode |

A profile with no CPU package power (`maxPkgW` 0, such as `vm-no-powercap`), one with an `intel-rapl-mmio` control type, and one whose `naturalMaxW` differs from `gpus.limits.maxW` stop the run: this model has one CPU cap and one GPU maximum per node.

**Eco.** Under baselines B and C an eco node gets the eco GPU cap on its GPUs. Its CPU:

- on a GPU node runs as on a performance node, as the controller manager plans it;
- on a CPU-only profile node with a PL1 the agent can write gets a CPU power cap of `SIM_SA_CPU_ECO_PCT` of its PL1 maximum, as the agent writes one: 60 % of 2 x 165 W is 198 W on `intel-xeon-2s-rapl`;
- on every other node, HSMP profiles included since the agent has no HSMP path, runs at the frequency scale `SIM_SA_CPU_ECO_PCT`.

## HTTP API

The simulator listens on `SIM_ADDR` (default `127.0.0.1:18080`, and `:18080` in a pod). The API has no authentication: expose it only to trusted clients.

| Endpoint | Method | Purpose |
|---|---|---|
| `/otlp/telemetry/{node}` | GET | One node's telemetry as an OTLP JSON `ExportMetricsServiceRequest`, with the names of [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}) |
| `/telemetry/{node}` | GET | The same OTLP JSON. `?format=legacy` serves the deprecated `packagePowerWatts` JSON until v0.4.0, with one warning per process; `?format=otlp` asks for the default, any other `format` answers 400 |
| `/control/{node}` | POST | Control actions (`rapl.set_power_cap_watts`, `dvfs.set_throttle_pct`, `gpu.set_power_cap_watts`). Returns `result=applied\|blocked\|error`. An emulated node answers 409: its limits are written to its files |
| `/state/{node}` | GET | Current internal node state; for an emulated node `{node, emulated: true, snapshot}` |
| `/metrics` | GET | [Prometheus metrics]({{< relref "/docs/simulator/metrics.md" >}}) |
| `/healthz` | GET | Health check |
| `/readyz` | GET | Emulate mode: 503 until the first snapshot is published, then 200, so every node tree exists |
| `/api/v1/query` | GET | [Fake Prometheus](#fake-prometheus-query-endpoint) instant query for facility metrics; not served in emulate mode |
| `/exporters/<name>/metrics` | GET | Emulate mode: the [compatibility exporters]({{< relref "/docs/simulator/metrics.md#compatibility-exporters-emulate-mode" >}}) listed in `SIM_EXPORTERS` |
| `/sim/v1/runs`, `/sim/v1/runs/current`, `/sim/v1/runs/current/finish` | POST, GET, POST | Emulate mode: the [run API]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-workloads-and-scenarios" >}}) that starts, reads and ends a workload run |
| `/debug/nodes` | GET | Node selection, matched node class, resolved model hints and current state of every node |
| `/debug/jobs` | GET | The jobs of the trace replayed from startup (`SIM_WORKLOAD_TRACE_PATH` outside emulate mode), unfinished first: class, node, requested CPUs and GPUs, work total and remaining, and seconds without progress. `{"count": 0, "jobs": []}` without such a trace |
| `/debug/events` | GET | Recent telemetry and control events, newest last (`SIM_DEBUG_EVENT_LIMIT`, default 200) |
| `/debug/energy` | GET | Integrated simulated energy per node and for the cluster |
| `/debug/timeseries` | GET | `timeseries.csv` from `SIM_DEBUG_PERSIST_DIR`: one row per sample with IT, CPU, GPU, cooling and facility power, PUE, ambient temperature, cluster CPU and GPU utilization, active nodes, running pods and cumulative energy. 404 when `SIM_DEBUG_PERSIST_DIR` is empty |
| `/debug/emulate` | GET | Emulate mode: the latest snapshot of every emulated node (host, each CPU package, DRAM zone and GPU with power, energy, limits and speeds) and of the facility, the values every export is computed from |
