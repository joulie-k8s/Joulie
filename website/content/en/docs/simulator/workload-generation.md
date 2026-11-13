---
title: "Workload Generation"
weight: 20
aliases:
  - "/docs/simulator/workload-distributions/"
  - "/docs/simulator/kubernetes-ai-workloads/"
---

`workloadgen` (`simulator/cmd/workloadgen`) writes synthetic traces of AI cluster workloads for the simulator to run. [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}) says how the simulator runs them.

```bash
go run ./simulator/cmd/workloadgen --out trace.jsonl --jobs 200 --seed 7
```

The traces hold CPU and GPU jobs, memory-bound jobs, and workloads of several pods, such as distributed training and hyperparameter searches. Network, disk and framework communication are not modelled: their cost is folded into each workload's utilisation and bottleneck signals.

## Trace format

A trace is JSON Lines. A `type=workload` record is one logical workload with its pods by role, for inspection only; a `type=job` record is one runnable pod, and the simulator runs these.

```json
{"type":"job","schemaVersion":"v3","jobId":"workload-000001-worker-01","workloadId":"workload-000001","workloadType":"distributed_training","podRole":"worker","gang":true,"submitTimeOffsetSec":12.4,"namespace":"default","intentClass":"performance","podTemplate":{"requests":{"cpu":"4","memory":"12Gi","nvidia.com/gpu":"1"}},"work":{"cpuUnits":57600,"gpuUnits":14400},"workloadClass":{"cpu":"cpu.mixed","gpu":"gpu.compute_bound"},"workloadProfile":{"cpuUtilization":0.28,"gpuUtilization":0.47,"memoryIntensity":0.58,"ioIntensity":0.08,"cpuFeedIntensityGpu":0.56}}
```

| Field | What it drives |
|---|---|
| `intentClass` | `performance` or `standard`, the class the pod declares to Joulie |
| `workloadClass.cpu`, `workloadClass.gpu` | the qualitative response to a cap: `cpu.compute_bound`, `cpu.memory_bound`, `cpu.io_bound`, `cpu.mixed`; `gpu.compute_bound`, `gpu.memory_bound`, `gpu.bandwidth_bound`, `gpu.mixed` |
| `workloadProfile` | `cpuUtilization`, `gpuUtilization`, `memoryIntensity`, `ioIntensity`, `cpuFeedIntensityGpu`, and `peerTransferIntensity` with `--peer-transfer-intensity`: the power a job draws and how much a cap slows it |
| `work` | the CPU and GPU [work units](#work-units) |
| `gang` | the pods of the workload advance only while all of them run |
| `submitTimeOffsetSec` | when the simulator submits the pod |

### Schema versions

`--schema-version` picks `v3` (the default: no `sensitivity` key, so a cap slows a job in full, and work sized so each pod runs for its workload's `durationSec`) or `v2` (the format of the benchmark experiments: work scaled by utilisation, role and rate flags). Each version rejects the flags of the other.

## How a workload is sampled

The default mode samples each workload's arrival time (exponential, shaped by a working-day pattern with random bursts and dips), its family (`debug_eval`, `single_gpu_training`, `distributed_training`, `parameter_server_training`, `hpo_experiment`, `cpu_preprocess`, `cpu_analytics`), its GPU count, a heavy-tailed duration, its utilisation and intensities, and its pods. The priors follow public studies of production GPU clusters; they are not a fit to one cluster. `--mode diurnal` samples single-pod CPU, training and inference jobs on a day and night rate instead.

## Work units

A pod's work is its duration times its request times `--base-speed-per-core` (default 1.0), so a trace must run on a simulator with the same `SIM_BASE_SPEED_PER_CORE`. `--arrival-window-sec` and `--max-duration-sec` fit a `v3` trace into a run of fixed length.

## Flags

| Flag | Default | Effect |
|---|---|---|
| `--out` | `trace.jsonl` | Output file |
| `--jobs` | `50` | Logical workloads |
| `--seed` | the current time | Random seed; set it for a reproducible trace |
| `--mode` | `default` | `default` or `diurnal` |
| `--schema-version` | `v3` | `v3` or `v2` |
| `--mean-inter-arrival-sec` | `5` | Mean inter-arrival before the hourly multiplier |
| `--gpu-ratio`, `--amd-gpu-ratio`, `--min-gpu-request` | `0.80`, `0`, `0` | GPU workloads, their AMD share, and the least GPUs per GPU workload |
| `--perf-ratio` | `0.30` | Base share of `performance` workloads |
| `--burst-day-probability`, `--burst-multiplier`, `--burst-mean-jobs` | `0.25`, `2.0`, `8` | Daily bursts |
| `--dip-day-probability`, `--dip-multiplier` | `0.30`, `0.08` | Daily dips |
| `--emit-workload-records` | `true` | Write the `type=workload` records |
| `--base-speed-per-core` | `1.0` in `v3` | Work units per core, or GPU, per second |
| `--cpu-sensitivity`, `--gpu-sensitivity` | unset | `v3`: the `sensitivity` of every job, in [0, 1] |
| `--arrival-window-sec`, `--max-duration-sec` | `0` | `v3`: compress arrivals into a window, clamp durations |
| `--peer-transfer-intensity` | `0` | `v3`: the share of GPU time a job with two or more GPUs spends copying between them ([Peer transfers]({{< relref "/docs/simulator/workload-simulator.md#peer-transfers" >}})) |

`go run ./simulator/cmd/workloadgen -h` lists every flag.

Every distribution, its parameters, the studies behind them and what is not modelled: [Workload generation](https://github.com/joulie-k8s/Joulie/wiki/Workload-Generation) on the wiki.
