---
title: "Hardware Calibration"
weight: 26
---

`hwcalib` (`simulator/cmd/hwcalib`) fits the physics of a hardware profile to a sweep measured on one of your nodes, so the simulator draws what your machine draws instead of the builtin profiles' vendor figures and placeholders. `hwcalib sweep` records the sweep on the node; `hwcalib fit` writes the result as a local profile the simulator loads from `SIM_PROFILES_DIR`.

## Recording a sweep

A sweep holds the node at a series of setpoints and records, at every sample, the node's readings and the setpoints it held. Each setpoint needs at least 5 samples after it settles: with a 2 s sample period, 22 s or more.

### Recording on the node with hwcalib sweep

Run it on a drained node, as root (reading `energy_uj` and writing limits need root):

```bash
go build -o hwcalib ./simulator/cmd/hwcalib
sudo ./hwcalib sweep -node <node> -write-limits -freq-max-mhz 2400,1900,1500 -out sweep.jsonl
./hwcalib fit -profile amd-epyc-2s-energy-only -node <node> sweep.jsonl
```

The load is a duty-cycled spin on every logical CPU. The grid nests power limits, then frequency ceilings, then loads. Every file the sweep wrote is put back when it ends, also on an error or a signal. GPUs and host power are not swept.

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
| `-sys-root`, `-proc-root` | `/sys`, `/proc` | the trees to read and write |
| `-out` | `-` | the file to write, `-` for standard output; an existing file is never replaced |

It exits 0 when the sweep completes, 1 on a read, write or interruption error, and 2 on a usage error.

### Setpoints

A sweep from another collector records each knob it holds as `joulie.calibration.setpoint`, one point per knob:

| `joulie.calibration.knob` | Value |
|---|---|
| `cpu.utilization` | busy fraction of every logical CPU, 0 to 1 |
| `cpu.frequency.max` | `scaling_max_freq` written to every cpufreq policy, in Hz |
| `cpu.power.limit` | package limit written to every package (RAPL PL1 or the HSMP socket cap), in W |
| `gpu.utilization` | busy fraction of every GPU, 0 to 1 |
| `gpu.power.limit` | limit written to every GPU, in W |

A utilization knob also carries `joulie.calibration.class` (such as `cpu.compute_bound`) and `joulie.calibration.memory_intensity`; the GPU fit needs them. The readings use the names of [Simulator Metrics]({{< relref "/docs/simulator/metrics.md" >}}): `hw.energy` or `hw.power` per CPU package, DRAM zone and GPU, `hw.host.energy`, `hw.cpu.speed`, `joulie.hw.power.limit` and `system.cpu.utilization`.

### File formats

| `-format` | Input |
|---|---|
| `otlp` (default) | OTLP JSON Lines, one `MetricsData` per line, as the OpenTelemetry file exporter writes them; the resource names the node in `k8s.node.name` or `host.name` |
| `csv` | one node: a header row, then one row per point with `time_unix_nano`, `metric`, `value`, optional `unit` and `start_time_unix_nano`, and one column per point attribute |

## Running a fit

```bash
go build -o hwcalib ./simulator/cmd/hwcalib
export SIM_PROFILES_DIR=/srv/joulie/profiles   # outside any clone of this repository
./hwcalib fit -profile amd-epyc-2s-energy-only -node <node> sweep.jsonl
```

| Flag | Default | Meaning |
|---|---|---|
| `-profile` | (required) | the profile the node is described by, builtin or local; it gives the topology and every parameter the sweep cannot fit |
| `-node` | (the only node) | the node whose resource to read |
| `-format` | `otlp` | `otlp` or `csv` |
| `-out-dir` | `SIM_PROFILES_DIR` | where to write the fitted profile |
| `-name` | `<profile>-measured` | the fitted profile's name and file name |
| `-substep` | `0` | the fixed physics substep of a simulated source, such as `100ms`; `0` for hardware |

It prints one row per parameter (base value, fitted value, method, samples, or why it was not fitted), then what it assumed. It fits idle and peak CPU power and the analytic model's exponents, DRAM and platform power, GPU idle and maximum power, and settling times. Exit codes: 0 success, 1 a sweep that cannot be read or fitted, 2 a usage error.

## The fitted profile

The fit writes `<name>.yaml`, which `extends` the base profile and holds only what the sweep measured. It is refused inside this repository (a measured profile may name your machine), never replaces an existing file, and is loaded back to check it. Name it in a cluster spec pool as any local profile ([Simulator hardware profiles]({{< relref "/docs/getting-started/05-configuration-reference.md#simulator-hardware-profiles" >}})):

```bash
go run ./simulator/cmd/clusterspec validate -spec <spec.yaml> -profiles-dir /srv/joulie/profiles
```

What a sweep reads and writes, how the sweep is cut into steady windows, every fit method and the substep correction: [Hardware calibration](https://github.com/joulie-k8s/Joulie/wiki/Hardware-Calibration) on the wiki.
