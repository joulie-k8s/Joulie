---
title: "Workload and Power Simulator"
weight: 10
---

The Joulie simulator lets you run full control-loop experiments on virtual clusters without real hardware. It keeps Kubernetes scheduling real while simulating hardware telemetry, power dynamics, and thermal behavior per node.

This page covers the simulator's architecture, HTTP API, and integration points. Detailed subsystems are documented on dedicated pages linked throughout.

## Architecture at a glance

The simulator extends the same control path used on real nodes:

1. Node labels define simulated hardware identity.
2. The controller manager resolves hardware from `NodeHardware` when available, otherwise from labels/inventory fallback.
3. The controller manager writes desired node profile (`NodeTwin.spec`).
4. Agent reads desired state and sends control intents.
5. Simulator emulates telemetry/control behavior per node and exposes HTTP endpoints.
6. Next reconcile loop reacts to updated simulated state.

<img src='{{< relURL "images/joulie-arch-simulator.png" >}}' alt="Joulie simulator architecture overview">

The diagram shows the end-to-end loop:

- Kubernetes keeps scheduling and pod lifecycle as source of truth.
- controller manager writes desired node states (`NodeTwin.spec`).
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

- **Policy controller** (in the controller manager): runs the full policy algorithm (static partition, queue-aware) with simulated `NodeHardware` and `NodeTwin.status`.
- **Scheduler extender**: participates in scheduling decisions for simulated KWOK pods; filter and score logic is identical to the production path.

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

These signals are exported as Prometheus metrics and through the `/state/{node}` HTTP endpoint. The scheduler extender reads them from `NodeTwin.status`, populated by the controller manager's twin controller from simulator-sourced telemetry.

For the hardware model parameters behind these signals, see [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}).

### Fake Prometheus query endpoint

The simulator serves a `/api/v1/query` endpoint that returns facility metrics in standard Prometheus instant-query format. This lets the controller manager's facility metrics poller query the simulator directly without a real Prometheus instance.

A query that names a metric of the controller manager's facility presets is answered as that input, whatever the rest of the query: the `otel` preset (the default) under all four `PROMETHEUS_NAME_TRANSLATION` strategies, and the deprecated `legacy` preset.

| Facility input | Names it answers | Value |
|---|---|---|
| outdoor ambient temperature | `joulie_facility_ambient_temperature_celsius` and its spellings under the other strategies, `datacenter_ambient_temperature_celsius` | Sinusoidal ambient temperature |
| IT power | `joulie_facility_it_energy_joules_total`, `joulie_facility_it_power_watts` and their spellings, `datacenter_total_it_power_watts` | Sum of all simulated node power |
| cooling power | `joulie_facility_cooling_energy_joules_total`, `joulie_facility_cooling_power_watts` and their spellings, `datacenter_cooling_power_watts` | Derived from IT power and PUE |
| cooling zone ambient, rack power, node inlet | `joulie_cooling_zone_ambient_temperature_celsius`, `joulie_rack_energy_joules_total`, `joulie_rack_power_watts`, `hw_host_ambient_temperature_celsius` and their spellings | An empty vector: this model has no cooling zones, racks or inlet sensors, so the controller manager logs `no_data` for them once and falls back to the outdoor ambient |

A query that names none of these falls back to matching words: one containing `ambient` and `temp`, `it_power`, `cooling` and `power`, or `pue` returns that gauge (the last one the simulated PUE), and any other query an empty vector. The answer is always one series without labels, so a custom `FACILITY_*_QUERY` that groups by a key label reads nothing.

To use it, set `FACILITY_PROMETHEUS_ADDRESS` on the controller manager to point at the simulator (e.g., `http://joulie-telemetry-sim.joulie-sim-demo.svc.cluster.local:18080`).

In [emulate mode](#emulate-mode) this endpoint is not served: the facility is exported as OpenTelemetry metrics to a real Prometheus instead.

## Simulator HTTP API

| Endpoint | Method | Purpose |
|---|---|---|
| `/telemetry/{node}` | GET | The same OTLP JSON as `/otlp/telemetry/{node}`. `?format=legacy` serves the per-node legacy JSON (`cpu.*`, `gpu.*`, pod counters), deprecated and removed in v0.4.0, with one warning per process |
| `/otlp/telemetry/{node}` | GET | The same node's telemetry as one OTLP JSON `ExportMetricsServiceRequest`, with the names of [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}) |
| `/control/{node}` | POST | Control actions (`rapl.set_power_cap_watts`, `dvfs.set_throttle_pct`, `gpu.set_power_cap_watts`). Returns `result=applied\|blocked\|error`. An emulated node answers 409: its limits are written to its files |
| `/state/{node}` | GET | Current internal node state; for an emulated node `{node, emulated: true, snapshot}` |
| `/metrics` | GET | Prometheus metrics |
| `/healthz` | GET | Health check |
| `/readyz` | GET | Emulate mode: 503 until the first snapshot is published, then 200, so every node tree exists |
| `/exporters/<name>/metrics` | GET | Emulate mode: the compatibility exporters listed in `SIM_EXPORTERS` |
| `/api/v1/query` | GET | Fake Prometheus instant query for facility metrics; not served in emulate mode |
| `/sim/v1/runs` | POST | Emulate mode, v1 engine: start a workload run from a trace inside the simulator; 409 while one is active |
| `/sim/v1/runs/current` | GET | Emulate mode, v1 engine: the active or last run's phase and its jobs per state |
| `/sim/v1/runs/current/finish` | POST | Emulate mode, v1 engine: end the run; `?cleanup=true` deletes its Job objects |

## Simulator observability

The simulator exposes debug endpoints alongside Prometheus metrics:

| Endpoint | Content |
|---|---|
| `/debug/nodes` | Node selection, matched override profile, resolved model hints, current state |
| `/debug/events` | Recent telemetry/control events (ring buffer) |
| `/debug/energy` | Integrated simulated energy totals |
| `/debug/emulate` | Emulate mode: the latest snapshot of every emulated node (host, each CPU package, DRAM zone and GPU with power, energy, limits and speeds) and of the facility, the values every export is computed from |

Detailed metric names and labels: [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}})

## Emulate mode

Emulate mode emulates the bare-metal nodes of a cluster spec at the boundary the agent sees: the files it reads and writes, and the GPU tools it runs. An unmodified agent pod then runs its real discovery and capping code against each node, while Kubernetes scheduling stays real. For each spec node the simulator keeps a tree under `SIM_HWEMU_ROOT/<node>/`, rendered from a hardware profile (`simulator/pkg/hwemu/profiles/`):

- `sys/`: the powercap, cpufreq, hwmon and PCI attributes as the kernel nests them. An agent pod mounts it as its `/host-sys`.
- `cpuinfo`: mounted as the pod's `/proc/cpuinfo`.
- `state/`: what the fake GPU tools read and write, such as NVML limits and energy, and the node's energy ledger.
- `bin/`: `nvidia-smi`, `rocm-smi` and `amd-smi`, links to `fakesmi`, which the simulator installs in `SIM_HWEMU_ROOT/lib/`. The agent pod puts `bin/` first on its `PATH`.

One tick loop steps every node to wall time each `SIM_EMULATE_TICK` (default 1 s), on `SIM_EMULATE_WORKERS` workers: it reads back the limits the agent wrote, advances power, energy and frequency, and publishes an immutable snapshot. Requests read the latest snapshot and never step a node, so reading `/telemetry/<node>` any number of times changes nothing. A value the hardware would refuse, out of range or unparsable, is reverted at the next tick where a real driver would have rejected the write, and reported as an [event]({{< relref "/docs/simulator/metrics.md#events-emulate-mode" >}}).

Nodes the spec does not list keep the label-based model of the rest of this page, in the same simulator.

### Governors and clocks

A tree also holds the CPU and GPU governor files of its profile, so the agent's governor discovery and control run against them. File classes: R is rendered and read-only, S is written by the emulator, A is writable by the agent and read back each tick, where a value the kernel would refuse is put back and reported as a `rejected` event with its errno (`joulie.sim.write.rejected`, see [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}})).

| Files, under `sys/` or `state/` | Class | Where | What the emulator does |
|---|---|---|---|
| `devices/system/cpu/cpufreq/policyN/scaling_governor`, `scaling_min_freq`, `scaling_max_freq` | A | every profile with cpufreq | runs the governor as the kernel's does, see [governor and clock physics]({{< relref "/docs/hardware/hardware-modeling.md#68-emulated-nodes-governor-and-clock-physics" >}}) |
| `policyN/energy_performance_preference`, `energy_performance_available_preferences` | A, R (S on amd-pstate-epp) | intel_pstate and intel_cpufreq with `hwp`, amd-pstate-epp | refuses a non-zero EPP under performance on intel_pstate and amd-pstate-epp with `EBUSY` |
| `policyN/scaling_setspeed` | A | profiles offering userspace | the frequency userspace holds, within the policy's limits; refused under any other governor |
| `devices/system/cpu/cpufreq/{schedutil,ondemand,conservative}/<tunable>` | A | profiles offering those governors | `rate_limit_us`, `up_threshold`, `sampling_rate`, `freq_step`, `down_threshold`, checked as the kernel checks them |
| `devices/system/cpu/cpufreq/boost` | A | acpi-cpufreq | boost above the top table entry, from that entry only |
| `devices/system/cpu/intel_pstate/{status,no_turbo,max_perf_pct,min_perf_pct,hwp_dynamic_boost}` | R, A, A, A, A | intel_pstate, intel_cpufreq | `no_turbo` lowers each policy's maximum to the top non-turbo frequency |
| `devices/system/cpu/amd_pstate/status`, `policyN/boost` | R, A | amd-pstate-epp | boost per policy |
| `devices/system/cpu/intel_uncore_frequency/uncoreNN/` | A, R, S | `intel-xeon6-2s-tpmi`, `intel-xeon-2s-multidie` | limits stored and checked; `current_freq_khz` follows the package load |
| `bus/pci/devices/<bdf>/power_dpm_force_performance_level` | A | AMD GPUs | takes the levels the profile accepts; a refused level goes back with the profile's errno |
| `<bdf>/pp_dpm_{sclk,mclk,fclk,socclk,pcie}` | S | AMD GPUs | the DPM tables, with `*` on the level each domain runs at |
| `<bdf>/pp_od_clk_voltage` | A | MI300X | overdrive edits `s 0`, `s 1`, `m 1` and `c`, only under `manual` or `perf_determinism` |
| `<bdf>/pp_power_profile_mode`, `<bdf>/pm_policy/{soc_pstate,xgmi_plpd}` | A | W7900, MI300X | stored and shown as the driver shows them |
| `nvml/gpuN/{clocks_sm_mhz,clocks_mem_mhz,pstate,clocks_event_reasons}` | S | NVIDIA GPUs | clocks, performance state and event reasons after each tick |
| `nvml/gpuN/{clocks_max_sm_mhz,clocks_max_mem_mhz,supported_clocks}` | R | NVIDIA GPUs | from the profile's `gpus.clocks` |
| `nvml/gpuN/{locked_gpu_clocks,locked_mem_clocks}` | A | NVIDIA GPUs | written by `nvidia-smi -lgc` and `-lmc`; no query prints them, as NVML has no getter for a lock |

The fake tools act on the same files:

| Command | Effect |
|---|---|
| `rocm-smi --showperflevel`, `--setperflevel LEVEL`, `--showprofile`, `--setprofile N` | read or write the level; `--setprofile` switches to `manual` first |
| `amd-smi set -g GPU --perf-level LEVEL`, `--clk-limit sclk max MHZ`; `amd-smi metric --perf-level` | set or read the level, or the overdrive ceiling under `manual`; `set --profile` prints `Not Yet Implemented` |
| `nvidia-smi -i ID -lgc MIN,MAX`, `-rgc`, `-lmc MIN,MAX`, `-rmc` | set or clear a lock, one line per GPU then `All done.`; `-lmc` fails on Hopper, which has no run-time memory lock |
| `nvidia-smi --query-gpu=clocks.sm,clocks.mem,clocks.max.sm,clocks.max.mem,pstate,clocks_event_reasons.active`; `nvidia-smi -q -d SUPPORTED_CLOCKS` | read the clock files |
| `nvidia-smi -ac`, `-rac` | refused as deprecated, non-zero exit |

`HWEMU_UNPRIVILEGED=1` in the tool's environment runs it as a user without root: `rocm-smi --setperflevel` prints nothing, exits 0 and changes nothing on a profile with `unprivilegedSetIsNoop`, `amd-smi set` fails with `Command requires elevation`, and the `nvidia-smi` lock commands fail with `Insufficient Permissions` and exit 4.

Load from the workload can include peer transfers between the GPUs of a node (`hwemu.Load.PeerTransfers`): they share each receiver's `gpus.transfer.peerLinkGBps`, and the bytes each GPU received and sent are counted in State (`PeerRxBytes`, `PeerTxBytes`).

### Builtin hardware profiles

Each profile in `simulator/pkg/hwemu/profiles/` names the source of every value: a published document, a kernel or tool source line, a measurement described by its hardware only, or an assumption. A GPU profile extends a CPU host profile and keeps its CPU block.

| Profile | Hardware | Control surface |
|---|---|---|
| `intel-xeon-2s-rapl` | 2x Xeon Platinum 8260 | intel_pstate; RAPL PL1 and PL2; DRAM zones that refuse writes |
| `intel-xeon-6530-2s-rapl` | 2x Xeon Gold 6530 | intel_cpufreq (intel_pstate passive); RAPL PL1 and PL2 |
| `intel-xeon-2s-psys` | 2x Xeon Gold 6530 | intel_pstate; RAPL PL1 and PL2, plus a psys zone between the packages |
| `intel-xeon-2s-multidie` | 2x Xeon Platinum 9282, two dies per package | one RAPL zone per die |
| `intel-xeon6-2s-no-powercap` | 2x Xeon 6787P | intel_pstate only, no powercap |
| `intel-xeon6-2s-tpmi` | 2x Xeon 6787P | TPMI RAPL with PL1, PL2 and PL4 |
| `amd-epyc-2s-energy-only` | 2x EPYC 9654 | acpi-cpufreq; RAPL energy counters, no limits |
| `amd-epyc-9534-2s-energy-only` | 2x EPYC 9534 | as above |
| `amd-epyc-2s-hsmp` | 2x EPYC 9655 | amd-pstate-epp; HSMP socket caps in hwmon |
| `nvidia-h100-nvl-8gpu` | 8x H100 NVL on the 8260 host | nvidia-smi, 200 to 400 W per GPU |
| `nvidia-h100-sxm-8gpu` | 8x H100 SXM on the 8260 host | nvidia-smi, 200 to 700 W per GPU |
| `nvidia-h100-sxm-4gpu` | 4x H100 SXM5 on the 6530 host | nvidia-smi, 200 to 700 W per GPU |
| `nvidia-l40s-4gpu` | 4x L40S on the 9534 host | nvidia-smi, 100 to 350 W per GPU |
| `amd-instinct-mi300x-8gpu` | 8x Instinct MI300X on the 9654 host | amdgpu hwmon, rocm-smi and amd-smi, 0 to 750 W per GPU; accelerator class, NFD `pci-1200_1002` |
| `amd-radeon-pro-w7900-4gpu` | 4x Radeon PRO W7900 on the 9534 host | amdgpu hwmon, rocm-smi and amd-smi; display class, NFD `pci-0300_1002`; the cap is pinned at 241 W per GPU |
| `vm-no-powercap` | KVM guest | none: no powercap, no cpufreq |

The governor each profile ships with and the presets it can express (CPU `performance`, `balanced`, `efficiency`; GPU `auto`, `sustained`, `efficient`, `low`, `peak`, as `pkg/governor` maps them):

| Profile | CPU driver, shipped governor | CPU presets | GPU mechanism, presets |
|---|---|---|---|
| `amd-epyc-2s-energy-only`, `amd-epyc-9534-2s-energy-only` | acpi-cpufreq, schedutil | all three (schedutil, conservative), global boost | none |
| `amd-epyc-2s-hsmp` | amd-pstate-epp, performance | all three through EPP, boost per policy | none |
| `intel-xeon-2s-rapl`, `intel-xeon-2s-psys`, `intel-xeon-2s-multidie`, `intel-xeon6-2s-no-powercap`, `intel-xeon6-2s-tpmi` | intel_pstate with HWP, powersave | all three through EPP, `no_turbo` | none |
| `intel-xeon-6530-2s-rapl` | intel_cpufreq, schedutil | all three (schedutil, conservative), `no_turbo` | none |
| `amd-radeon-pro-w7900-4gpu` | as the 9534 | as the 9534 | amdgpu sysfs: `auto`, `sustained` (`high`), `low`, `peak` |
| `amd-instinct-mi300x-8gpu` | as the 9654 | as the 9654 | amdgpu sysfs: `auto`, `efficient` |
| `nvidia-h100-nvl-8gpu`, `nvidia-h100-sxm-8gpu` | as the 8260 | as the 8260 | nvidia-smi: every preset, `sustained` equivalent to `auto` |
| `nvidia-h100-sxm-4gpu` | as the 6530 | as the 6530 | as the H100 above |
| `nvidia-l40s-4gpu` | as the 9534 | as the 9534 | nvidia-smi: every preset, `sustained` locks the memory clock at its maximum |
| `vm-no-powercap` | no cpufreq | none | none |

No builtin profile turns the receiver gate on.

The W7900's cap is pinned as measured: hwmon and both tools report minimum, maximum, default and current at 241 W, so the driver refuses every other value (a [`reverted-out-of-range` event]({{< relref "/docs/simulator/metrics.md#events-emulate-mode" >}})), `rocm-smi --setpoweroverdrive` prints the bound it violates and exits 0, and `amd-smi set -o ppt0` prints "Power cap must be between 241W and 241W". AMD publishes 295 W for the board; that is total board power, while 241 W is the package power limit the driver enforces and reports.

### The cluster spec

```yaml
apiVersion: sim.joulie.io/v1alpha1
kind: ClusterSpec
metadata: {name: smoke}
spec:
  defaults:
    kwok: true                                   # KWOK annotation and taint on every node (the default)
    labels: {joulie.io/managed: "true"}
    baseLoad: {cpuUtil: 1.0, gpuUtil: 1.0, memoryIntensity: 0.2, cpuClass: cpu.compute_bound, gpuClass: gpu.compute_bound}
  facility: {ambientC: 20, amplitudeC: 5, periodH: 24, coolingZones: [{name: zone-a, offsetC: 0}, {name: zone-b, offsetC: 3}]}
  pools:
  - {name: intel, profile: intel-xeon-2s-rapl, count: 1, rack: rack-1, coolingZone: zone-a}
  - {name: sxm-nopm, profile: nvidia-h100-sxm-8gpu, count: 1, rack: rack-2, coolingZone: zone-b, overrides: {gpus: {persistenceMode: disabled}}}
  - {name: vm, profile: vm-no-powercap, count: 1}
```

Each pool expands to `count` nodes named `<pool>-<i>` (or `prefix` and `startIndex`), with the profile's labels, the defaults, then the pool's labels, rack and cooling zone, plus `sim.joulie.io/cluster` and `sim.joulie.io/pool`. The annotation `sim.joulie.io/profile` names the profile. `baseLoad` is the utilization every emulated node runs at while no workload drives it; a pool's `baseLoad` overrides the defaults field by field. The `clusterspec` command validates a spec and renders its Nodes:

```bash
go run ./simulator/cmd/clusterspec validate -spec tests/simcluster/specs/smoke.yaml
go run ./simulator/cmd/clusterspec render-nodes -spec tests/simcluster/specs/smoke.yaml | kubectl apply -f -
```

The simulator checks that each spec node exists in the cluster and names the same profile, and emits `joulie.sim.node.mismatch` once per change when it does not.

An NVIDIA profile can run with persistence mode off (`overrides.gpus.persistenceMode: disabled`): an idle GPU then drops the limit the agent wrote back to its default, as a GPU does when the driver unloads with no client, and the simulator records `joulie.sim.cap.reset`.

### Restarts

An existing tree is resumed: energy counters, the limits agents wrote and NVML state continue from disk, and only missing files are put back, so a restart never replaces a file an agent pod holds open. Exported energy counters never decrease: they continue from a ledger in each node's `state/`, so a restart shows in Prometheus as a gap in the rate, never as a counter reset. No energy accrues while the simulator is down, a known simplification. A tree that no longer matches its spec node, such as one rendered for another profile, stops the start; the recovery is in the [configuration reference]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-emulate-mode" >}}).

### Telemetry

In emulate mode the simulator pushes OpenTelemetry metrics (OTLP JSON over HTTP) for every node and for the facility, named after the hardware semantic conventions: `hw.host.energy` for the whole host, `hw.energy` per CPU package, DRAM zone and GPU. Only readings a real collector could measure are exported: a virtual machine node exports none. A Prometheus started with `--web.enable-otlp-receiver` takes the push directly, with no collector in between; it should promote `k8s.node.name` and `host.name` onto the data series (`otlp.promote_resource_attributes`), otherwise queries join `target_info` for the node name. Events go to `events.jsonl` and, when a logs endpoint is set, are pushed as OpenTelemetry log records. The names, units and Prometheus spellings are in [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}); the variables, and where the export deviates from the OpenTelemetry exporter specification, in [Simulator telemetry export]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-telemetry-export" >}}).

For Joulie inputs that expect a real exporter, `SIM_EXPORTERS` also serves Kepler 0.7, Kepler 0.12, dcgm-exporter and ROCm device-metrics-exporter formats, computed from the same counters.

### Workload runs

In emulate mode a workload runs on the v1 engine (`SIM_WORKLOAD_MODEL=v1`), one run at a time, started through the run API or from `SIM_WORKLOAD_TRACE_PATH` at startup. Each group of trace jobs becomes one `batch/v1` Job, scheduled by the real scheduler onto the spec's KWOK Nodes. Every tick, on the emulator's own goroutine, the engine hands each node the load of the jobs running on it (each holds as many logical CPUs as it requests, and its GPUs), so the node's emulated power follows the work; it then advances each job at the throughput the node's limits leave it, and attributes the node's energy delta per class (`performance`, `standard`, and `idle` for the unrequested share of the packages' idle power). While a run is active the spec's `baseLoad` does not apply. A finished pod is annotated `sim.joulie.io/complete: "true"`, and a KWOK Stage moves it to `Succeeded`; a job that cannot progress, such as a gang that never runs whole, is reported as stalled, never completed by force. The run's events and metrics go to `<SIM_RUNS_DIR>/<runId>/otlp/` as OTLP JSON Lines.

The settings, the run API and the KWOK Stage are in [Simulator workloads and scenarios]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-workloads-and-scenarios" >}}); the scenario runner that drives runs on the simulated cluster and evaluates them is in [Workload Scenarios]({{< relref "/docs/simulator/scenarios.md" >}}).

### Running it in a cluster

The chart runs the simulator as root with one replica and strategy `Recreate`, on the node labelled `joulie.io/hwemu-host=true`, with `emulate.root` as a hostPath:

```bash
helm upgrade --install joulie-sim charts/joulie-simulator -n joulie-sim --create-namespace \
  --set emulate.enabled=true \
  --set-file emulate.clusterSpec=tests/simcluster/specs/smoke.yaml \
  --set otel.metricsEndpoint=http://prometheus-operated.monitoring:9090/api/v1/otlp/v1/metrics
```

The pod is Ready once every tree exists, so agent pods that mount the trees can start after it.

### Running it from a workstation

The simulator also runs outside the cluster, against any cluster a kubeconfig reaches, which is the fastest loop while changing it. Use a throwaway cluster: the Nodes it renders are fake and carry the KWOK taint.

```bash
export KUBECONFIG=<kubeconfig of a throwaway cluster>
go build -o /tmp/sim/bin/ ./simulator/cmd/simulator ./simulator/cmd/clusterspec
/tmp/sim/bin/clusterspec render-nodes -spec tests/simcluster/specs/smoke.yaml | kubectl apply -f -
SIM_ADDR=127.0.0.1:18080 SIM_EMULATE=true SIM_CLUSTER_SPEC=tests/simcluster/specs/smoke.yaml \
  SIM_HWEMU_ROOT=/tmp/sim/hwemu SIM_FAKESMI_SOURCE= SIM_DEBUG_PERSIST_DIR=/tmp/sim/run \
  SIM_EXPORTERS=kepler-0.7,dcgm-4.8.4 \
  /tmp/sim/bin/simulator
```

`SIM_FAKESMI_SOURCE=` skips the fakesmi install, which only agent pods need. The log names the API server in use. Then:

```bash
curl -s 127.0.0.1:18080/debug/emulate | jq -c '[.nodes[] | [.name, .host.powerW]]'
curl -s 127.0.0.1:18080/exporters/kepler-0.7/metrics | grep kepler_node_platform_joules_total
head -1 /tmp/sim/run/events.jsonl | jq -r '.resourceLogs[0].scopeLogs[0].logRecords[0].eventName'
```

Add `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=http://127.0.0.1:9090/api/v1/otlp/v1/metrics` to push to a local Prometheus. Stopping and starting the simulator over the same `SIM_HWEMU_ROOT` resumes the trees.

### Tick cost

A tick is mostly system calls on the trees. Each node reads back every file an agent may write (299 on `intel-xeon-2s-rapl`) and writes every emulator-owned file whose value changed: while its frequencies move, its 96 `scaling_cur_freq` and 4 `energy_uj`. An emulator-owned file is written in place, in one write, under a write lease. The kernel grants the lease only while nobody else has the file open, and holds back any other open until the write is done, so a reader sees the old value or the new one, whole. A value shorter than the file, or a file a reader holds open, is replaced by a temp file and a rename instead. Before, every changed file went through a temp file and a rename: an inode created and one freed per file, which on a disk-backed root was nearly all of a tick.

`SIM_EMULATE_WORKERS` defaults to `GOMAXPROCS`, at most 12: past about 12 workers the system calls contend and a tick gets slower (intel-200 on the volume: 205 ms with 8 or 12 workers, 240 ms with 16, 290 ms with 28).

`BenchmarkEM01Tick` reports the median of ten 1 s ticks right after start, at a 0.6 CPU base load, with the default workers and render check. Medians in ms, measured on a 28-thread x86 VM shared with other jobs (load average 3 to 17), so expect about 20 % between runs:

| Nodes | Docker volume (xfs), before | after | tmpfs, before | after |
|---|---|---|---|---|
| 16 `intel-xeon-2s-rapl` | 102 | 23 | 30 | 21 |
| 200 `intel-xeon-2s-rapl` | 1403 | 214 | 412 | 207 |
| 16 `amd-epyc-2s-energy-only` | 392 | 79 | 116 | 77 |

These runs are as root in a container, as the simulator runs in the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}), with the root on a Docker named volume, the harness's placement. A non-root `go test` also pays two `chmod` per in-place write, to make the attribute writable around its open. Its medians on xfs are 87, 2597 and 626 ms before, 30, 269 and 95 ms after; on tmpfs 32, 472 and 116 before, 22, 209 and 87 after. A root on tmpfs is therefore not needed for 200 nodes. The harness keeps the trees on the volume, so a restart of its container loses nothing: the trees, the limits agents wrote and the energy ledgers survive it. To measure:

```bash
go test ./simulator/pkg/emulate/ -run '^$' -bench EM01Tick -benchtime 10x                    # TMPDIR's file system
TMPDIR=/dev/shm go test ./simulator/pkg/emulate/ -run '^$' -bench EM01Tick -benchtime 10x    # tmpfs
CGO_ENABLED=0 go test -c -o /tmp/em.test ./simulator/pkg/emulate/                             # as root, on a volume
docker run --rm --user 0 -v em-bench:/bench -e TMPDIR=/bench -v /tmp/em.test:/em.test:ro \
  debian:stable-slim /em.test -test.run '^$' -test.bench EM01Tick -test.benchtime 10x
docker volume rm em-bench
```

## Installation

See the dedicated [Installation]({{< relref "/docs/simulator/installation.md" >}}) page for Helm and from-source installation instructions.

### Node scope and class mapping

- **`SIM_NODE_SELECTOR`**: only nodes matching this label selector are simulated. Default: `joulie.io/managed=true`.
- **`SIM_NODE_CLASS_CONFIG`**: YAML file with label-matched model overrides, applied on top of inventory/label-based hardware identity.
- **`SIM_HARDWARE_CATALOG_PATH`**: path to a hardware inventory catalog YAML that replaces the one embedded in the binary. Default: empty, meaning the embedded catalog. The chart sets it to the file the `joulie-simulator-hardware-catalog` ConfigMap mounts; an empty file there loads no catalog, so clear the path (`--set env.SIM_HARDWARE_CATALOG_PATH=`) when that ConfigMap is empty. The experiments do this and read the embedded catalog.

The preferred hardware bootstrap flow:

1. Put CPU/GPU identity on node labels.
2. Let the simulator/controller manager resolve that identity against the shared inventory.
3. Use `SIM_NODE_CLASS_CONFIG` only for scenario-specific overrides or calibration tweaks.

For full details on hardware profile parameters and the power model, see [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}).

### Standalone inventory

Standalone mode (`SIM_STANDALONE=true`) runs without Kubernetes. `SIM_STANDALONE_INVENTORY` names a YAML file whose `nodes` list describes the cluster. Each entry gives `node_name` (one node) or `node_name_prefix` and `replicas` (nodes `<prefix>-0`, `<prefix>-1`, and so on), and then either legacy hardware keys or a `profile`.

```yaml
nodes:
  # Legacy entry: the model comes from the labels, as for a Kubernetes node.
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
  # Profile entry: the hardware emulate mode emulates.
  - node_name_prefix: rapl
    replicas: 8
    profile: intel-xeon-2s-rapl
```

**Legacy entries.** The simulator turns the keys into node labels (`joulie.io/hw.cpu-model`, `joulie.io/hw.gpu-model`, the CPU and GPU counts) and resolves the model from them the way it does for a Kubernetes node: each value comes from the matching `SIM_NODE_CLASS_CONFIG` class, else the environment, else the hardware catalogue, else the built-in constant. `gpu_min_cap_watts` and `gpu_max_cap_watts`, when above 0, then set the node's GPU board power range over all of them, since the entry describes that node. A maximum below the resolved minimum lowers the minimum to it; a minimum above the maximum stops the run.

**Profile entries.** `profile` names an hwemu profile: a builtin one (`simulator/pkg/hwemu/profiles/`) or a local one in `SIM_PROFILES_DIR`. The profile sets the hardware, so the hardware keys above must stay unset, and an unknown name stops the run with the list of known ones. The node gets the labels of the Node a real node of the profile registers as, plus `type=kwok`, `joulie.io/managed=true`, `joulie.io/node-name` and `joulie.io/hw.cpu-cores` and `joulie.io/hw.gpu-count` from the profile's logical CPUs and GPUs. Its model is the profile's, whatever `SIM_NODE_SELECTOR`, the node classes and the catalogue say:

| Model value | From the profile |
|---|---|
| idle and peak CPU power | sockets x `physics.cpu.idlePkgW` and sockets x `physics.cpu.maxPkgW`; a `catalog-curve` CPU runs the catalogue's two-socket curve scaled by sockets/2 |
| frequency range and knee | `cpuinfoMinKHz` and `cpuinfoMaxKHz` of `cpufreq`; knee x the maximum |
| CPU cap range and default | the sum over the packages of the PL1 (`long_term`) maximum and limit, else of the HSMP caps; a node with neither is capped at its peak, which never binds; the minimum is 1 W |
| CPU cap settling time | the PL1 time window, or `physics.rapl.settleTau` when it is a duration, or `physics.hsmp.settleTauMS` |
| GPU range | `gpus.limits.minW` (1 W when it is 0) to `physics.gpu.naturalMaxW` |
| thermal throttling | off, as in emulate mode |

A profile with no CPU package power (`maxPkgW` 0, such as `vm-no-powercap`), one with an `intel-rapl-mmio` control type, and one whose `naturalMaxW` differs from `gpus.limits.maxW` stop the run: the legacy model has one CPU cap and one GPU maximum per node.

**Eco on a profile node.** Under baselines B and C, an eco node whose profile has a PL1 the agent can write gets a CPU power cap of `SIM_SA_CPU_ECO_PCT` of its PL1 maximum, as the agent writes one: 60 % of 2 x 165 W is 198 W on `intel-xeon-2s-rapl`. A performance node goes back to the PL1 limit it started with. Every other node, HSMP profiles included because the agent has no HSMP path, gets the frequency scale `SIM_SA_CPU_ECO_PCT` as before.

## Workload trace and execution

Enable workload trace replay with:

```
SIM_WORKLOAD_TRACE_PATH=/path/to/trace.jsonl
```

The simulator loads `type=job` records, injects pods, and advances per-job CPU work units every tick. Completion time increases when DVFS/RAPL reduce node effective speed. Outside emulate mode the legacy engine runs the trace: bare pods, deleted when their work is done. In emulate mode the v1 engine runs it as one Job object per group, completed through KWOK ([Workload runs](#workload-runs)).

For the full trace format, workload profile fields, and physical effects of CPU/GPU intensity and sensitivity settings, see:

- [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}}): how realistic AI traces are built
- [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}): how traces are consumed and progressed at runtime

Helper tools:

- `simulator/cmd/workloadgen`: generate synthetic JSONL traces from distributions
- `simulator/cmd/traceextract`: normalize/extract input JSONL into simulator trace schema

## KWOK flow summary

1. Create [KWOK](https://kwok.sigs.k8s.io/) fake nodes with `type=kwok` and `joulie.io/managed=true`.
2. Taint fake nodes with `kwok.x-k8s.io/node=fake:NoSchedule`.
3. Run controller manager + simulator + agent pool on real node(s).
4. Set agent telemetry env vars to route to simulator HTTP.
5. Inject trace workload (pods tolerate kwok taint + select `type=kwok`).
6. Observe power/control/job-completion metrics.

Detailed algorithm docs:

- [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}})
- [Power Simulator]({{< relref "/docs/simulator/power-simulator.md" >}})
- [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}})

Related example: `examples/07-simulator-gpu-powercaps/`

## Large virtual clusters with kind + KWOK

You do not need a large real hardware cluster to evaluate Joulie policies. With [kind](https://kind.sigs.k8s.io/) + [KWOK](https://kwok.sigs.k8s.io/) you can:

- Keep a real Kubernetes control plane and scheduler.
- Attach many fake worker nodes.
- Run real controller manager/agent/simulator control loops.
- Scale experiments to many nodes and pods with low hardware cost.

Typical flow:

1. Create a kind cluster (real control-plane + worker runtime nodes).
2. Add many KWOK fake nodes labeled `joulie.io/managed=true`.
3. Deploy simulator + agent pool + controller manager.
4. Run workload traces and observe throughput/energy behavior.

Practical scripts are in:

- `experiments/01-cpu-only-benchmark/scripts/10_setup_cluster.sh`
- `experiments/01-cpu-only-benchmark/scripts/20_run_benchmark.sh`

This is the model used in the benchmark experiments:

- [CPU-Only Benchmark]({{< relref "/docs/experiments/cpu-only-benchmark.md" >}})
- [Homogeneous H100 Benchmark]({{< relref "/docs/experiments/homogeneous-h100-benchmark.md" >}})

## Integration with Joulie

### `NodeTwin.spec` (desired state)

Set by the controller manager. Defines the desired per-node target (profile/cap).

### Telemetry backend selection

Configured via environment variables on the agent. Routes input signals and control sinks to the simulator:

| Env var | Value | Effect |
|---|---|---|
| `TELEMETRY_CPU_SOURCE` | `http` | Agent reads telemetry from simulator |
| `TELEMETRY_CPU_HTTP_ENDPOINT` | `<sim-url>/otlp/telemetry/{node}` | Telemetry read URL: one OTLP JSON document per node. `<sim-url>/telemetry/{node}` serves the same document; `?format=legacy` serves the legacy JSON, read until v0.4.0 |
| `TELEMETRY_CPU_CONTROL` | `http` | Agent sends CPU control via simulator |
| `TELEMETRY_CPU_CONTROL_HTTP_ENDPOINT` | `<sim-url>/control/{node}` | CPU control write URL |
| `TELEMETRY_GPU_CONTROL` | `http` | Agent sends GPU control via simulator |
| `TELEMETRY_GPU_CONTROL_HTTP_ENDPOINT` | `<sim-url>/control/{node}` | GPU power-cap write URL |

### `NodeHardware` (node identity)

Published automatically by the agent. Describes discovered CPU/GPU identity and control capability, not normally authored by hand in simulator examples.

For simulator bootstrap, node labels remain the lightweight source of hardware identity. The most useful bootstrap labels:

- `joulie.io/hw.cpu-model`
- `joulie.io/hw.cpu-sockets`
- `joulie.io/hw.gpu-model`
- `joulie.io/hw.gpu-count`
- Vendor presence: `feature.node.kubernetes.io/pci-10de.present=true` (NVIDIA) or `feature.node.kubernetes.io/pci-1002.present=true` (AMD)

The controller manager can also infer GPU presence from allocatable extended resources like `nvidia.com/gpu` or `amd.com/gpu`.

## Validation disclaimer

GPU support has been validated in simulator mode only (no bare-metal GPU access yet). Host GPU code paths are designed for NVIDIA/AMD nodes and become fully testable once real GPU nodes are available.
