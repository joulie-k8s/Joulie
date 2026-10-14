---
title: "Hardware Calibration"
weight: 26
---

`hwcalib fit` (`simulator/cmd/hwcalib`, package `simulator/pkg/calib`) fits the physics of a hardware profile to a sweep measured on one of your nodes, and writes the result as a local profile the simulator loads from `SIM_PROFILES_DIR`. The builtin profiles keep vendor figures and placeholders; a calibrated profile replaces them with what your machine draws. `hwcalib sweep` records such a sweep on the node itself (see [Recording on the node](#recording-on-the-node-with-hwcalib-sweep)).

## Recording a sweep

A sweep holds the node at a series of setpoints and records, at every sample, the node's readings and the setpoints it held. Each setpoint should last long enough to settle: the fit drops the start of every setpoint (see [Segments and steady windows](#segments-and-steady-windows)) and needs at least 5 samples after that, so with a 2 s sample period a setpoint of 22 s or more is enough.

The readings use the names the simulator emits (see [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}})):

| What | Name | Used for |
|---|---|---|
| CPU package and DRAM power | `hw.energy` (preferred) or `hw.power`, with `hw.id` `cpu<socket>` or `memory<socket>` and `hw.type` | CPU and DRAM fit |
| GPU power and load | `hw.energy` or `hw.power` with `hw.type=gpu`, `hw.gpu.utilization` | GPU fit |
| Host power | `hw.host.energy` or `hw.host.power` | platform power |
| CPU frequency | `hw.cpu.speed` and `hw.cpu.speed.limit` with `hw.limit_type=max` | CPU fit, frequency ramp |
| Power limits | `joulie.hw.power.limit`, points `requested` and `enforced` | capped samples, limit settling |
| CPU load | `system.cpu.utilization{cpu.mode=idle}`, per logical CPU with `cpu.logical_number` or for the whole host | CPU fit |
| Setpoints | `joulie.calibration.setpoint`, one point per knob | segments |

No common exporter emits `hw.energy{hw.type=cpu}` from RAPL today, so the sweep comes from a collector on the node that reads powercap `energy_uj` (which needs root, as the file is mode 0400), such as `hwcalib sweep`, or from a converter of another exporter's output.

Utilization is one minus the idle share. Per logical CPU, it is mapped to packages by the profile's topology (`socket-major-smt-last`: CPU k sits on package (k mod sockets x cores) / cores). Without `system.cpu.utilization`, the `cpu.utilization` setpoint stands in for it.

### Setpoints

`joulie.calibration.setpoint` carries one point per knob the sweep holds, at every sample. Its unit is `{setpoint}`, because each knob has its own:

| `joulie.calibration.knob` | Value |
|---|---|
| `cpu.utilization` | busy fraction of every logical CPU, 0 to 1 |
| `cpu.frequency.max` | `scaling_max_freq` written to every cpufreq policy, in Hz |
| `cpu.power.limit` | package limit written to every package (RAPL PL1 or the HSMP socket cap), in W |
| `gpu.utilization` | busy fraction of every GPU, 0 to 1 |
| `gpu.power.limit` | limit written to every GPU, in W |

A utilization knob also carries `joulie.calibration.class`, the class of the load as the simulator's power models name it (such as `cpu.compute_bound` or `gpu.memory_bound`; empty for the default weights), and `joulie.calibration.memory_intensity`, 0 to 1. The GPU fit inverts the model at that class, so record it.

### File formats

The default input is OTLP JSON Lines: one `MetricsData` object per line, as the OpenTelemetry file exporter writes them. Int64 values and times may be strings or numbers, sums may be cumulative or delta, and a counter that falls or restarts its series is a reset whose interval is dropped. The resource names the node in `k8s.node.name` or `host.name`; `-node` picks it when the file holds several.

`-format csv` reads one node with the same names: a header row, then one row per point. The columns are `time_unix_nano`, `metric`, `value` (required), `unit` and `start_time_unix_nano` (optional), and every other column is a point attribute named by its header: `hw.id`, `hw.type`, `hw.limit_type`, `cpu.logical_number`, `cpu.mode`, `joulie.calibration.knob`, `joulie.calibration.class`, `joulie.calibration.memory_intensity`, and `joulie.hw.limit.state` when the sweep has limit points. An empty cell is an absent attribute. A value must be a finite number; `NaN` or `Inf` is an error naming its line, so leave out a reading the collector missed rather than write it as `NaN`. Counters are the metrics the name table registers as counters, with cumulative values.

### Recording on the node with hwcalib sweep

`hwcalib sweep` holds the node's CPUs at each setpoint of a grid and writes the sweep as OTLP JSON Lines that `hwcalib fit` reads as they are. Run it on the node, as root, with nothing else running there (drain the node first): reading `energy_uj` needs root since Linux commit 949dd0104c49 made it mode 0400, writing limits needs root, and other work would disturb both the load and the readings.

```bash
go build -o hwcalib ./simulator/cmd/hwcalib
sudo ./hwcalib sweep -node <node> -write-limits -freq-max-mhz 2400,1900,1500 -out sweep.jsonl
./hwcalib fit -profile amd-epyc-2s-energy-only -node <node> sweep.jsonl
```

The load is one thread per logical CPU that `/proc/stat` lists, each locked to its OS thread and pinned to its CPU (`sched_setaffinity`), busy for the setpoint's share of every `-duty-period` and asleep for the rest. It spins on registers with no memory traffic, so the setpoint records no `joulie.calibration.class` (the models' default weights) and memory intensity 0. The grid nests power limits outermost, then frequency ceilings, then loads; each setpoint lasts `-hold` and is sampled every `-period`, the first sample one period in.

It writes to the host only with `-write-limits`: `scaling_max_freq` of every cpufreq policy for `-freq-max-mhz`, and for `-power-limit-w` the limit of every package, the HSMP `power1_cap` of each socket when the `amd_hsmp_hwmon` devices are present, else the `long_term` constraint of each RAPL package zone. Without `-write-limits`, either list is a usage error. Every file it wrote is put back as it was found when the sweep ends, also on an error, SIGINT, SIGTERM or SIGHUP, so a dropped SSH session does not leave the node capped. Under `nohup` a hangup stays ignored and the sweep goes on.

| Reading | Read from | Written as |
|---|---|---|
| Package energy | `energy_uj` of each `package-N` zone of the `intel-rapl` control type (its `package-N-die-M` zones summed when it has no package zone), a wrap at `max_energy_range_uj` counted | `hw.energy{hw.id=cpuN, hw.type=cpu}`, cumulative from the sweep's start |
| DRAM energy | `energy_uj` of the `dram` child of each package zone | `hw.energy{hw.id=memoryN, hw.type=memory, hw.parent=cpuN}` |
| Socket power | `power1_input` of each `amd_hsmp_hwmon` device, socket by socket in hwmon order | `hw.power{hw.id=cpuN, hw.type=cpu}` |
| Package limit | HSMP `power1_cap`, or the RAPL `long_term` `power_limit_uw` of an enabled zone | `joulie.hw.power.limit`, point `enforced`, and `requested` once the sweep wrote a limit |
| Frequency | `scaling_cur_freq`, `scaling_max_freq` and `cpuinfo_max_freq` of the policies of each package | `hw.cpu.speed`, `hw.cpu.speed.limit` points `throttled` and `max` |
| CPU load | the idle share of each `cpuN` line of `/proc/stat` | `system.cpu.utilization{cpu.mode=idle}` with `cpu.logical_number` |
| Setpoints | the setpoint held | `joulie.calibration.setpoint` |

A CPU's package is its `topology/physical_package_id`, else the `physical id` of `/proc/cpuinfo`, which also gives `hw.vendor` and `hw.model`. Core, uncore and psys zones are never read, since none is a package or DRAM. GPUs are not swept: the load is CPU only. Host power is not read either, since no kernel file measures the whole host; the platform fit needs `hw.host.energy` or `hw.host.power`, which a converter can add from a BMC or a metered PDU. The resource has `service.name=hwcalib`, `service.namespace=joulie`, and `service.instance.id` and `host.name` set to `-node`; the scope is `github.com/matbun/joulie/simulator/pkg/calib`, and both carry the pinned `schemaUrl`.

| Flag | Default | Meaning |
|---|---|---|
| `-node` | the host name | the node name in the resource, which `hwcalib fit -node` picks |
| `-util` | `0,0.2,0.4,0.6,0.8,1` | the loads, busy fractions of every CPU |
| `-freq-max-mhz` | (none) | the `scaling_max_freq` ceilings, in MHz; needs `-write-limits` |
| `-power-limit-w` | (none) | the package power limits, in W; needs `-write-limits` |
| `-write-limits` | `false` | allow writing frequency ceilings and power limits |
| `-hold` | `30s` | how long each setpoint lasts |
| `-period` | `2s` | the sample period |
| `-duty-period` | `100ms` | the period of the duty-cycled load |
| `-sys-root`, `-proc-root` | `/sys`, `/proc` | the trees to read and write; the load still runs on this machine's CPUs |
| `-out` | `-` | the file to write, `-` for standard output; an existing file is never replaced |

It exits 0 when the sweep completes, 1 when the host cannot be read, a file cannot be written or the sweep is interrupted (the lines written so far stay whole, and a file with no sample is removed), and 2 on a usage error.

## Running a fit

```bash
go build -o hwcalib ./simulator/cmd/hwcalib
export SIM_PROFILES_DIR=/srv/joulie/profiles   # outside any clone of this repository
./hwcalib fit -profile amd-epyc-2s-energy-only -node <node> sweep.jsonl
```

| Flag | Default | Meaning |
|---|---|---|
| `-profile` | (required) | the profile the node is described by: a builtin, or a local one in `SIM_PROFILES_DIR`. It gives the topology, the settling times that size the steady windows, and the value of every parameter the sweep cannot fit |
| `-node` | (the only node) | the node whose resource to read |
| `-format` | `otlp` | `otlp` or `csv` |
| `-out-dir` | `SIM_PROFILES_DIR` | where to write the fitted profile |
| `-name` | `<profile>-measured` | the fitted profile's name and file name |
| `-substep` | `0` | the fixed physics substep of a simulated source, such as `100ms`; `0` for hardware |

The input file may be `-` for standard input. The command prints one row per parameter (base value, fitted value, method, samples behind it, or why it was not fitted), then notes on what it assumed. It exits 0 on success, 1 when the sweep cannot be read or fitted or the profile cannot be written, and 2 on a usage error.

## What is fitted

| Parameter | Method |
|---|---|
| `physics.cpu.idlePkgW` | mean package power below 2 % utilization |
| `physics.cpu.maxPkgW`, `alphaUtil`, `betaFreq` | least squares of ln(P - idle) on [1, ln(U x activity), ln F] for the analytic model P = idle + (max - idle) (U x activity)^alpha F^beta, where F is `hw.cpu.speed` over the hardware maximum and activity the model's memory intensity factor; packages are pooled |
| `physics.dram.wattsPerSocket` | mean DRAM power per socket |
| `physics.platform.otherW` | mean of host power minus every component over the idle segments (every package and GPU below 2 %); unmeasured DRAM counts with the base value |
| `physics.gpu.idleW` | mean GPU power below 2 % utilization |
| `physics.gpu.naturalMaxW` | the board model inverted at the recorded class and memory intensity: P - idle = (max - idle) x shape(U), least squares through the origin |
| `physics.cpu.freqRampMS`, `physics.hsmp.settleTauMS`, `physics.gpu.capSettleTauMS` | the decay of `hw.cpu.speed` and of the enforced `joulie.hw.power.limit` after setpoint steps |

Beta is not fitted when the sweep held one frequency (var(ln F) near 0, as under acpi-cpufreq powersave) or when frequency followed load (|corr(ln U, ln F)| > 0.95, as under intel_pstate powersave): the two exponents cannot be told apart, so the base beta stands (a base beta of 0 counts as 1, as in the model) and the fit solves alpha and the maximum with it. Sweep `cpu.frequency.max` under the performance governor to fit beta. Alpha and beta must lie in (0.3, 3). Samples drawing 98 % or more of their enforced limit are capped and left out of the CPU and GPU fits.

The GPU inversion matters: a full-load `gpu.compute_bound` GPU at memory intensity 0 draws only idle + 0.958 of its range (weights 0.88 compute and 0.12 memory, the memory curve reaching 0.65), so the raw full-load mean understates the natural maximum. A profile fitted to a CPU model other than `analytic` switches to `analytic`, since the fitted facts describe that model.

RAPL PL1 settling is reported but never written, because `physics.rapl.settleTau` is a plain setting without provenance.

## Segments and steady windows

The sweep is split where any setpoint changes: knob, value, class or memory intensity. Each segment drops max(10 s, 3 x the base profile's largest settling time) after its first sample, per domain (frequency ramp, HSMP and RAPL windows for the CPU; limit settling and the telemetry window for the GPU), and is used only when at least 5 samples remain. Counters give the mean power over that window from their increments; gauges their mean.

## Settling times and the substep correction

A settling time comes from the samples after a setpoint change: ln|v - final| falls on a line of slope -1/tau_eff, fitted until the response comes within 0.1 % of the step. The samples must be faster than the settling time; a step that settles within one or two samples is not fitted.

The simulator moves a limit or a frequency by one Euler step (target - v) x h/tau per substep h, which decays like exp(-t/tau_eff) with exp(-h/tau_eff) = 1 - h/tau. For such a source, `-substep` applies tau = h / (1 - exp(-h/tau_eff)): a GPU limit of tau 150 ms under the 100 ms substep shows tau_eff = 91 ms. Hardware settles continuously; leave `-substep` at 0 there.

## The fitted profile

The fit is written as `<name>.yaml`, a local profile that `extends` the base and holds only what the sweep measured: a source `sweep` of kind `measured`, whose ref names the input file and node, and each fitted fact with the provenance `measured:sweep#<fit>`, where `<fit>` is `cpu-idle`, `cpu-ols`, `dram-mean`, `platform-idle`, `gpu-idle`, `gpu-inversion`, `cpu-frequency-step`, `cpu-limit-step` or `gpu-limit-step`. Powers are rounded to 0.01 W, exponents to 0.001 and settling times to whole milliseconds.

- **Outside the repository only.** A directory inside this repository (any directory under a `go.mod` of module `github.com/matbun/joulie`, by its own path or through a link) is refused before the fit starts, and there is no override: a measured profile may name the machine it was measured on, and it belongs to your site, in `SIM_PROFILES_DIR`.
- **Never replaced.** An existing file of that name is an error; remove it or pick another `-name`.
- **Checked.** The profile is loaded back with the others of its directory (`hwemu.LoadProfileSet`) and removed when it does not load. A local base profile must therefore live in the output directory too.

Point the simulator at the directory with `SIM_PROFILES_DIR` and name the profile in a cluster spec pool, as for any local profile (see [Configuration Reference]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-hardware-profiles" >}})). `clusterspec` and `simcluster` read the same directory through their `-profiles-dir` flag, which defaults to `SIM_PROFILES_DIR`:

```bash
go run ./simulator/cmd/clusterspec validate -spec <spec.yaml> -profiles-dir /srv/joulie/profiles
```
