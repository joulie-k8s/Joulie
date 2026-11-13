---
title: "Emulate Mode"
weight: 16
---

Emulate mode emulates the bare-metal nodes of a [cluster spec](#the-cluster-spec) at the boundary the agent sees: the files it reads and writes, and the GPU tools it runs. An unmodified agent then runs its real discovery and capping code against each node, while Kubernetes scheduling stays real. The [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) runs it; to run it yourself, see [Installation]({{< relref "/docs/simulator/installation.md#emulate-mode-in-a-cluster" >}}). Nodes the spec does not list keep the label-based model of [KWOK and standalone modes]({{< relref "/docs/simulator/simulator.md" >}}).

Turn it on with `SIM_EMULATE=true` and `SIM_CLUSTER_SPEC`; every variable is under [Simulator emulate mode]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-emulate-mode" >}}).

## Node trees

For each spec node the simulator keeps a tree under `SIM_HWEMU_ROOT/<node>/`, rendered from a hardware profile:

| Path | What the agent sees |
|---|---|
| `sys/` | powercap (RAPL), cpufreq, hwmon and PCI attributes, mounted as the pod's `/host-sys` |
| `cpuinfo` | the pod's `/proc/cpuinfo`, with the profile's model and topology |
| `state/` | what the fake GPU tools read and write (NVML limits and energy), and the node's energy ledger |
| `bin/` | `nvidia-smi`, `rocm-smi` and `amd-smi`: links to `fakesmi`, first on the agent's `PATH` |

One tick loop steps every node each `SIM_EMULATE_TICK` (default 1 s): it reads back the limits the agent wrote, advances power, energy and frequency, and publishes a snapshot that every request reads. A value the hardware would refuse is reverted at the next tick with plain files, and fails at once in [FUSE mode](#fuse-mode); either way it is reported as an [event]({{< relref "/docs/simulator/metrics.md#events-emulate-mode" >}}). `/control/<node>` answers 409 for an emulated node: its limits are written to its files.

## FUSE mode

With `SIM_HWEMU_FUSE=true` (chart value `emulate.fuse`) the simulator serves each node's `sys/` and `state/` through FUSE at `SIM_HWEMU_ROOT/fuse/<node>/`, and the emulated agents mount those (`agent.emulated.fuse`). The simulated cluster runs this mode unless `SIMCLUSTER_HWEMU=files`.

- A read is computed when it is made, so energy counters move between ticks.
- A write the driver refuses fails with the kernel's errno (`ENODATA` for a locked RAPL limit, `EINVAL`, `EBUSY`, ...); any other value is in effect before the write returns.
- Files look as sysfs shows them.

It needs Linux and `/dev/fuse`; as root the simulator also needs `CAP_SYS_ADMIN` and the root on a shared mount. Plain files need none of this and stay the default; how they differ from a real node is under [Deviations from a real cluster]({{< relref "/docs/simulator/simulated-cluster.md#deviations-from-a-real-cluster" >}}).

## Governors and clocks

A tree holds the CPU governor files (cpufreq, intel_pstate, amd_pstate, uncore) and the GPU clock files (amdgpu DPM levels, NVML clocks and locks) of its profile, and the fake tools (`rocm-smi`, `amd-smi`, `nvidia-smi -lgc`, `-lmc`) act on them, so the agent's governor discovery and control run as on a real node. The CPU governors follow the kernel's rules ([Governors](https://github.com/joulie-k8s/Joulie/wiki/Governors#cpu-how-emulated-nodes-run-a-governor) on the wiki) and the GPU clocks follow the cap and the governor ([GPU power model](https://github.com/joulie-k8s/Joulie/wiki/GPU-Power-Model#the-clock-model-of-emulated-nodes)). Every file, who writes it, and every tool command: [Emulate mode](https://github.com/joulie-k8s/Joulie/wiki/Emulate-Mode#governor-and-clock-files).

## Builtin hardware profiles

The profiles are in `simulator/pkg/hwemu/profiles/`; each names the source of every value.

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
| `amd-instinct-mi300x-8gpu` | 8x Instinct MI300X on the 9654 host | amdgpu hwmon, rocm-smi and amd-smi, 0 to 750 W per GPU |
| `amd-radeon-pro-w7900-4gpu` | 4x Radeon PRO W7900 on the 9534 host | amdgpu hwmon, rocm-smi and amd-smi; the cap is pinned at 241 W per GPU |
| `intel-flex-170-4gpu` | 4x Data Center GPU Flex 170 on the 6530 host | i915 hwmon, no tool, up to 150 W per GPU |
| `vm-no-powercap` | KVM guest | none: no powercap, no cpufreq |

The governor each profile ships with, the presets it accepts, and the profile facts that shape what the agent reads are in [Emulate mode](https://github.com/joulie-k8s/Joulie/wiki/Emulate-Mode#builtin-hardware-profiles) on the wiki.

## The cluster spec

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

Each pool expands to `count` nodes named `<pool>-<i>`, labelled `sim.joulie.io/cluster` and `sim.joulie.io/pool`. `baseLoad` is the utilization every node runs at while no workload drives it. Validate a spec and render its Nodes:

```bash
go run ./simulator/cmd/clusterspec validate -spec tests/simcluster/specs/smoke.yaml
go run ./simulator/cmd/clusterspec render-nodes -spec tests/simcluster/specs/smoke.yaml | kubectl apply -f -
```

`-profiles-dir <dir>` (default `SIM_PROFILES_DIR`) adds local profiles to the builtin ones.

## Restarts and recovery

An existing tree is resumed, not rendered again: energy counters, the limits agents wrote and NVML state continue from disk, and exported counters never decrease. A tree rendered for another profile, or with an entry of the wrong kind, stops the start with an error that names the node; to recover, make sure no agent pod mounts that node, remove `SIM_HWEMU_ROOT/<node>` and start again (its counters restart at zero).

## Telemetry

The simulator pushes OpenTelemetry metrics (OTLP JSON over HTTP) for every node and for the facility, which a Prometheus with `--web.enable-otlp-receiver` takes directly ([Prometheus settings]({{< relref "/docs/architecture/telemetry.md#prometheus-settings" >}})). Events go to `events.jsonl`. `SIM_EXPORTERS` also serves Kepler 0.7, Kepler 0.12, dcgm-exporter and ROCm device-metrics-exporter formats, computed from the same counters. Names and units: [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}); variables: [Simulator telemetry export]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-telemetry-export" >}}).

## Workload runs

A workload runs on the v1 engine, one run at a time, started through the [run API]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-workloads-and-scenarios" >}}) or from `SIM_WORKLOAD_TRACE_PATH`. Each job loads the node it runs on, advances at the throughput the node's limits leave it, and has its energy attributed to its class; results go to `<SIM_RUNS_DIR>/<runId>/otlp/`. [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}) compares the engines; [Workload Scenarios]({{< relref "/docs/simulator/scenarios.md" >}}) drive runs on the simulated cluster.

## Tick cost

200 `intel-xeon-2s-rapl` nodes take about 0.1 s per tick, so they fit in the default 1 s tick without tmpfs. `SIM_EMULATE_WORKERS` defaults to `GOMAXPROCS`, at most 12. Measure on your machine:

```bash
go test ./simulator/pkg/emulate/ -run '^$' -bench EM01Tick -benchtime 10x
```

In a running simulator, `joulie_sim_emulate_tick_duration_seconds` and `joulie_sim_emulate_tick_overruns_total` ([Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}})) show the cost.

The tree layout, FUSE internals, every governor and tool behaviour, the profile facts, the recovery sequence and the tick measurements: [Emulate mode](https://github.com/joulie-k8s/Joulie/wiki/Emulate-Mode) on the wiki.
