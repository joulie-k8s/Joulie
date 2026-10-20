---
title: "Emulate Mode"
weight: 16
---

Emulate mode emulates the bare-metal nodes of a [cluster spec](#the-cluster-spec) at the boundary the agent sees: the files it reads and writes, and the GPU tools it runs. An unmodified agent then runs its real discovery and capping code against each node, while Kubernetes scheduling stays real. The [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) runs it; to run it yourself, see [Installation]({{< relref "/docs/simulator/installation.md#emulate-mode-in-a-cluster" >}}). Nodes the spec does not list keep the label-based model of [KWOK and standalone modes]({{< relref "/docs/simulator/simulator.md" >}}), in the same simulator.

Turn it on with `SIM_EMULATE=true` and `SIM_CLUSTER_SPEC`; every variable is under [Simulator emulate mode]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-emulate-mode" >}}).

## Node trees

For each spec node the simulator keeps a tree under `SIM_HWEMU_ROOT/<node>/`, rendered from a hardware profile (`simulator/pkg/hwemu/profiles/`):

- `sys/`: the powercap, cpufreq, hwmon and PCI attributes as the kernel nests them. An agent pod mounts it as its `/host-sys`.
- `cpuinfo`: mounted as the pod's `/proc/cpuinfo`. One block per logical CPU with the profile's model and topology, and a `flags` line with the flags its facts imply: `hwp`, and `hwp_epp` under intel_pstate, where `cpufreq.hwp` is true, and `hypervisor` on a virtual machine. A profile with none of them has no `flags` line; a corpus machine keeps its captured cpuinfo verbatim.
- `state/`: what the fake GPU tools read and write, such as NVML limits and energy, and the node's energy ledger.
- `bin/`: `nvidia-smi`, `rocm-smi` and `amd-smi`, links to `fakesmi`, which the simulator installs in `SIM_HWEMU_ROOT/lib/`. The agent pod puts `bin/` first on its `PATH`.

One tick loop steps every node to wall time each `SIM_EMULATE_TICK` (default 1 s), on `SIM_EMULATE_WORKERS` workers: it reads back the limits the agent wrote, advances power, energy and frequency, and publishes an immutable snapshot. Requests read the latest snapshot and never step a node, so reading `/telemetry/<node>` any number of times changes nothing. With plain files, a value the hardware would refuse, out of range or unparsable, lands in the file and is reverted at the next tick, where a real driver would have failed the write; in [FUSE mode](#fuse-mode) the write fails at once. Either way it is reported as an [event]({{< relref "/docs/simulator/metrics.md#events-emulate-mode" >}}).

## FUSE mode

With `SIM_HWEMU_FUSE=true` (chart value `emulate.fuse`) the simulator serves each node's `sys/` and `state/` through FUSE at `SIM_HWEMU_ROOT/fuse/<node>/`, and the emulated agents mount those (`agent.emulated.fuse`); `cpuinfo`, `bin/` and `dev/nvidiactl` stay on the tree. The tree on disk becomes the simulator's own copy, kept up to date for a restart to resume from. The simulated cluster runs this mode unless `SIMCLUSTER_HWEMU=files`.

- **A read is computed when it is made.** The node steps to the time of the read, on the same substep grid a tick uses, so an energy counter moves between two reads within a tick, at 1 ms resolution, never goes back, and at a tick's time reads what the tick computes. A value a reader saw is on disk before the read returns, so a restart never resumes below it.
- **A write is answered when it is made.** A value the driver refuses fails with the kernel's errno (`ENODATA` for a locked RAPL limit, `EINVAL`, `EBUSY`, ...) and changes nothing; any other value is in effect, and reads back normalized, before the write returns. Each `write` call is one store, as on sysfs, and every one is an event of the next tick.
- **Files look as sysfs shows them.** An attribute is 4096 bytes whatever its content, opening a file without write permission for writing fails with `EACCES` even for root, a write to a writable file the emulator does not model (such as `intel_pstate/status`) is taken and changes nothing, and nothing can be created, removed or renamed.
- **A load change** takes effect from the node's last read when a read stepped the node past the previous tick, where plain files take it from the previous tick.

It needs Linux and `/dev/fuse`. As root, as the chart runs it, the simulator mounts the file system in its own `/tmp` and bind-mounts each view at `fuse/<node>/sys` and `fuse/<node>/state`: that takes `CAP_SYS_ADMIN`, and the views reach the agents only with the root on a shared mount, propagation `Bidirectional` into the simulator and `HostToContainer` into each agent. The bind mounts are never unmounted: a restarted simulator binds its views over them, which reaches the running agents through that propagation, so they need no restart, and their reads fail with `ENOTCONN` until it does. Removing them takes `umount -l` on the host, or the end of the host's mount namespace (`make simcluster-down` for the simulated cluster). As another user one FUSE mount at `fuse/`, through `fusermount3`, serves the same paths and is unmounted when the simulator stops; the unit tests run that way.

Plain files need none of this. They stay the default of the simulator, of the hwemu tests and of any host without FUSE; how they differ from a real node is under [Deviations from a real cluster]({{< relref "/docs/simulator/simulated-cluster.md#deviations-from-a-real-cluster" >}}).

`/control/<node>` answers 409 for an emulated node: its limits are written to its files.

## Governors and clocks

A tree also holds the CPU and GPU governor files of its profile, so the agent's governor discovery and control run against them. File classes: R is rendered and read-only, S is written by the emulator, A is writable by the agent, where a value the kernel would refuse is reported as a `rejected` event with its errno (`joulie.sim.write.rejected`, see [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}})): put back at the next tick with plain files, refused at the write in [FUSE mode](#fuse-mode).

| Files, under `sys/` or `state/` | Class | Where | What the emulator does |
|---|---|---|---|
| `devices/system/cpu/cpufreq/policyN/scaling_governor`, `scaling_min_freq`, `scaling_max_freq` | A | every profile with cpufreq | runs the governor as the kernel's does, see [governor and clock physics]({{< relref "/docs/hardware/hardware-modeling.md#68-emulated-nodes-governor-and-clock-physics" >}}) |
| `policyN/energy_performance_preference`, `energy_performance_available_preferences` | A, R (S on amd-pstate-epp) | intel_pstate and intel_cpufreq with `hwp`, amd-pstate-epp | refuses a non-zero EPP under performance on intel_pstate and amd-pstate-epp with `EBUSY` |
| `policyN/scaling_setspeed` | A | profiles offering userspace | the frequency userspace holds, within the policy's limits; refused under any other governor |
| `devices/system/cpu/cpufreq/{schedutil,ondemand,conservative}/<tunable>` | A | profiles offering those governors | `rate_limit_us`, `up_threshold`, `sampling_rate`, `freq_step`, `down_threshold`, checked as the kernel checks them |
| `devices/system/cpu/cpufreq/boost`, `policyN/boost` | A, A | acpi-cpufreq | boost above the top table entry, from that entry only. Every `policyN/boost` reads the profile's `cpufreq.policyBoost` at boot (default 0) while `cpufreq/boost` reads 1 and the CPUs boost, as on the nodes captured; a policy write equal to what its file shows changes nothing, one is refused with `EINVAL` while the global boost is off, and a global write of a new state sets every policy |
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
| `nvidia-smi -i ID -lgc MIN,MAX`, `-rgc`, `-lmc MIN,MAX`, `-rmc` | set or clear a lock, one line per GPU then `All done.`; on a GPU without the lock, such as `-lmc` on Hopper, which has no run-time memory lock, it prints `... is not supported for GPU <bus id>.` and `Treating as warning and moving on.`, writes nothing there and exits 0, or exits 3 with `--error-on-warning` (`-eow`) |
| `nvidia-smi --query-gpu=clocks.sm,clocks.mem,clocks.max.sm,clocks.max.mem,pstate,clocks_event_reasons.active` (or `clocks_throttle_reasons.active`, the same file); `nvidia-smi -q -d SUPPORTED_CLOCKS` | read the clock files |
| `nvidia-smi -ac`, `-rac` | refused as deprecated, non-zero exit |
| `nvidia-smi -pm 0`, `1`, `DISABLED`, `ENABLED` | persistence mode off or on |
| `nvidia-smi --query-gpu=... --format=csv` | the header names each field by its canonical name, whichever alias was asked for (`clocks.sm` is `clocks.current.sm`, `clocks_throttle_reasons.active` is `clocks_event_reasons.active`) |
| any option `nvidia-smi` does not have | `Invalid combination of input arguments. Please run 'nvidia-smi -h' for help.` on stdout, exit 255 |

`HWEMU_UNPRIVILEGED=1` in the tool's environment runs it as a user without root: `rocm-smi --setperflevel` prints nothing, exits 0 and changes nothing on a profile with `unprivilegedSetIsNoop`, `amd-smi set` fails with `Command requires elevation`, and the `nvidia-smi` lock commands fail with `Insufficient Permissions` and exit 4.

Load from the workload can include peer transfers between the GPUs of a node (`hwemu.Load.PeerTransfers`): they share each receiver's `gpus.transfer.peerLinkGBps`, and the bytes each GPU received and sent are counted in State (`PeerRxBytes`, `PeerTxBytes`).

## Builtin hardware profiles

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
| `intel-flex-170-4gpu` | 4x Data Center GPU Flex 170 on the 6530 host | i915 hwmon, no tool, up to 150 W per GPU (`power1_max`, `power1_rated_max`), with a floor the hardware clamps to that sysfs does not show; display class, NFD `pci-0380_8086` and Intel's NFD rule labels |
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
| `intel-flex-170-4gpu` | as the 6530 | as the 6530 | none |
| `vm-no-powercap` | no cpufreq | none | none |

No builtin profile turns the receiver gate on.

Profile facts that shape what the agent reads:

- `physics.cpu.model`: `analytic`, the power formula of `idlePkgW` and `maxPkgW`, or `catalog-curve`, the measured curve of the CPU's catalog entry split over the packages. The 2x EPYC 9654 profiles (`amd-epyc-2s-energy-only` and `amd-instinct-mi300x-8gpu`, which extends it) run the curve of SPECpower_ssj2008 result 01203; the 9534 and HSMP profiles are analytic.
- `limitBits` of a powercap zone: the width of its power limit field, in raw power units. A limit past it wraps in the register, as on hardware. Unset, it is the MSR interface's: 15 bits for PL1 and PL2, 17 on psys, 13 for PL4; `intel-xeon6-2s-tpmi` sets 18, the TPMI field.
- `gpus.clocks.supported.smMaxByMem`: memory clocks under which the GPU offers only the lower graphics clocks, each with the highest one. `nvidia-smi -q -d SUPPORTED_CLOCKS` lists the graphics clocks of such a memory clock up to that ceiling, and a memory clock lock holds the graphics clock at or below the ceiling of its memory clock, in the GPU's power and speed and in `clocks.sm` (`nvidia-l40s-4gpu`).
- `gpus.fieldSupport`: an `nvidia-smi` query field and the token the tool prints in its place, such as `power.min_limit: "[N/A]"`. The emulator never writes a value into that field's file. `nvidia-smi -pl` on a GPU whose limit or range holds a token prints `Changing power management limit is not supported for GPU: <bus id>.` and `Treating as warning and moving on.`, writes nothing, sets the other GPUs it was given and exits 0 after `All done.`; with `--error-on-warning` (`-eow`) it writes nothing and exits 3. No builtin profile sets it.
- `base_frequency` is rendered on the Intel profiles with HWP, so the agent's `cpu.landmarks.nominalFreqMHz` is set.
- `cpuinfo_max_freq` follows turbo: it shows the top non-turbo frequency while `intel_pstate/no_turbo` or the amd-pstate boost turns turbo off.

The W7900's cap is pinned as measured: hwmon and both tools report minimum, maximum, default and current at 241 W, so the driver refuses every other value written to `power1_cap` (a [`joulie.sim.write.rejected` event]({{< relref "/docs/simulator/metrics.md#events-emulate-mode" >}}) with `error.type` `einval`), `rocm-smi --setpoweroverdrive` prints the bound it violates and exits 0, and `amd-smi set -o ppt0` prints "Power cap must be between 241W and 241W". AMD publishes 295 W for the board; that is total board power, while 241 W is the package power limit the driver enforces and reports.

The Flex 170's `power1_max` (class A) takes any value, as i915 does: a value in whole watts is kept, one between the hardware's bounds is enforced, and one outside them reads back, after the next tick, as the bound the hardware clamps it to, 40 W at the bottom and 150 W at the top, so the agent's readback reports it as an error. Writing 0 turns the limit off. The 40 W floor is assumed: sysfs does not show it, and Intel publishes none. `gpus.intel.driver: xe` renders the xe layout instead, with the limit on the package channel, `power2_max`.

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

Each pool expands to `count` nodes named `<pool>-<i>` (or `prefix` and `startIndex`), with the profile's labels, the defaults, then the pool's labels, rack and cooling zone, plus `sim.joulie.io/cluster` and `sim.joulie.io/pool`. The annotation `sim.joulie.io/profile` names the profile. `baseLoad` is the utilization every emulated node runs at while no workload drives it; a pool's `baseLoad` overrides the defaults field by field. The `clusterspec` command validates a spec and renders its Nodes:

```bash
go run ./simulator/cmd/clusterspec validate -spec tests/simcluster/specs/smoke.yaml
go run ./simulator/cmd/clusterspec render-nodes -spec tests/simcluster/specs/smoke.yaml | kubectl apply -f -
```

`clusterspec validate|render-nodes|list -spec <file> [-profiles-dir <dir>]`: `-profiles-dir` adds the local profiles of a directory to the builtin ones, and defaults to `SIM_PROFILES_DIR`. The `simcluster` command of the [simulated cluster]({{< relref "/docs/simulator/simulated-cluster.md" >}}) takes the same flag with the same default.

The simulator checks that each spec node exists in the cluster and names the same profile, and emits `joulie.sim.node.mismatch` once per change when it does not.

An NVIDIA profile can run with persistence mode off (`overrides.gpus.persistenceMode: disabled`): an idle GPU then drops the limit the agent wrote back to its default, as a GPU does when the driver unloads with no client, and the simulator records `joulie.sim.cap.reset`.

## Restarts and recovery

An existing tree is resumed, not rendered again: energy counters, the limits agents wrote and NVML state continue from disk, and the tree is completed from the layout, every missing entry created and nothing present touched, so a restart never replaces a file an agent pod holds open. Leftover `state/.tmp/ensure-*` directories are removed. Exported energy counters never decrease: they continue from a ledger in each node's `state/`, so a restart shows in Prometheus as a gap in the rate, never as a counter reset. The ledger is rewritten each tick, in place and whole (padded with spaces) up to 2 KB, by rename above, so it holds the last whole tick also when the simulator is killed during a write. No energy accrues while the simulator is down.

A tree rendered for another profile, node or profile values, or one with an entry of the wrong kind (a file where a directory belongs, or a locked limit that is a directory, as an older simulator rendered it), stops the start with an error that names the node. To recover, make sure no agent pod mounts that node, remove `SIM_HWEMU_ROOT/<node>`, and start the simulator again: the node is rendered fresh, with its counters back at zero.

## Telemetry

The simulator pushes OpenTelemetry metrics (OTLP JSON over HTTP) for every node and for the facility, named after the hardware semantic conventions: `hw.host.energy` for the whole host, `hw.energy` per CPU package, DRAM zone and GPU. Only readings a real collector could measure are exported: a virtual machine node exports none. A Prometheus started with `--web.enable-otlp-receiver` takes the push directly, with no collector in between (see [Prometheus settings]({{< relref "/docs/architecture/telemetry.md#prometheus-settings" >}})). Events go to `events.jsonl` and, when a logs endpoint is set, are pushed as OpenTelemetry log records.

- Names, units and Prometheus spellings: [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}).
- Variables, and where the export deviates from the OpenTelemetry exporter specification: [Simulator telemetry export]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-telemetry-export" >}}).
- For Joulie inputs that expect a real exporter, `SIM_EXPORTERS` also serves Kepler 0.7, Kepler 0.12, dcgm-exporter and ROCm device-metrics-exporter formats, computed from the same counters.

## Workload runs

A workload runs on the v1 engine, one run at a time, started through the [run API]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-workloads-and-scenarios" >}}) or from `SIM_WORKLOAD_TRACE_PATH` at startup. Each group of trace jobs becomes one `batch/v1` Job, scheduled by the real scheduler onto the spec's KWOK Nodes.

- Every tick, the engine hands each node the load of the jobs running on it (each job holds as many logical CPUs as it requests, and its GPUs), so the node's emulated power follows the work.
- Each job advances at the throughput the node's limits leave it.
- The node's energy delta is attributed per class: `performance`, `standard`, and `idle` for the unrequested share of the packages' idle power.
- While a run is active the spec's `baseLoad` does not apply.
- A finished pod is annotated `sim.joulie.io/complete: "true"`, and a KWOK Stage moves it to `Succeeded`. A job that cannot progress, such as a gang that never runs whole, is reported as stalled, never completed by force.
- The run's events and metrics go to `<SIM_RUNS_DIR>/<runId>/otlp/` as OTLP JSON Lines.

[Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}) compares the two engines; [Workload Scenarios]({{< relref "/docs/simulator/scenarios.md" >}}) drive runs on the simulated cluster and evaluate them.

## Tick cost

A tick is mostly system calls on the nodes' files. The readback does one `lstat` per agent-writable file and reads only the files written since its last read, and the emulator rewrites only the files whose content changed. In [FUSE mode](#fuse-mode) the tick reads back no file, since every write reached the node through its view, and the reads of the views step the nodes between ticks, a cost the agents' reads pay instead. `SIM_EMULATE_WORKERS` defaults to `GOMAXPROCS`, at most 12, because those calls contend past about 12 workers.

The median tick of the benchmark below on a shared machine, approximate and machine-dependent:

| Nodes | Median tick |
|---|---|
| 16 `intel-xeon-2s-rapl` | about 11 to 13 ms |
| 200 `intel-xeon-2s-rapl` | about 105 to 115 ms |
| 16 `amd-epyc-2s-energy-only` | about 50 to 80 ms |

So 200 Intel nodes fit in the default 1 s tick without tmpfs. A start on an existing root compares the live tree with the layout and creates only the missing entries: resuming 200 Intel nodes takes about 0.7 s, rendering them on a fresh root about 12 s.

To measure on your machine (the trees go to `TMPDIR`; `p50-ms` is the median tick, the `-live` cases are the tick of FUSE mode, and a run as root matches the simulator's container, since a non-root run pays two `chmod` calls per write of a file it may not write):

```bash
go test ./simulator/pkg/emulate/ -run '^$' -bench EM01Tick -benchtime 10x
```

In a running simulator, `joulie_sim_emulate_tick_duration_seconds` and `joulie_sim_emulate_tick_overruns_total` ([Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}})) show the cost.
