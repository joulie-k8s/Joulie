---
title: "Workload Generation"
weight: 20
aliases:
  - "/docs/simulator/workload-distributions/"
  - "/docs/simulator/kubernetes-ai-workloads/"
---

`workloadgen` (`simulator/cmd/workloadgen`) writes synthetic traces of AI cluster workloads for the simulator. This page says what a trace contains and how the generator samples it; [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}) says how the simulator runs it.

```bash
go run ./simulator/cmd/workloadgen --out trace.jsonl --jobs 200 --seed 7
```

The generator models AI-oriented Kubernetes clusters: CPU and GPU jobs, memory-bound jobs, and workloads made of several pods, such as distributed training and hyperparameter searches. It does not model network bandwidth, disk throughput or framework communication protocols: their cost is folded into the utilisation and bottleneck signals of each workload (`gpuUtilization`, `memoryIntensity`, `cpuFeedIntensityGpu`).

## Trace format

A trace is JSON Lines with two kinds of records:

- `type=workload`: one logical workload, with its pods by role (`--emit-workload-records`, on by default). The simulator ignores it; it is there for inspection.
- `type=job`: one runnable pod, expanded from a workload. The simulator runs these.

```json
{"type":"workload","schemaVersion":"v3","workloadId":"workload-000001","submitTimeOffsetSec":12.4,"namespace":"default","workloadType":"distributed_training","gang":true,"durationSec":14400,"workloadClass":{"cpu":"cpu.mixed","gpu":"gpu.compute_bound"},"sharedIntensityProfile":{"cpuUtilization":0.28,"gpuUtilization":0.47,"memoryIntensity":0.58,"ioIntensity":0.08,"cpuFeedIntensityGpu":0.56},"pods":[{"role":"launcher","replicas":1,"requests":{"cpu":"2","memory":"4Gi"},"gang":true},{"role":"worker","replicas":4,"requests":{"cpu":"4","memory":"12Gi","nvidia.com/gpu":"1"},"gang":true}]}
```

A job record of that workload (base speed 1.0: the worker runs for the workload's 14400 s on its 4 cores and its GPU):

```json
{"type":"job","schemaVersion":"v3","jobId":"workload-000001-worker-01","workloadId":"workload-000001","workloadType":"distributed_training","podRole":"worker","gang":true,"submitTimeOffsetSec":12.4,"namespace":"default","intentClass":"performance","podTemplate":{"requests":{"cpu":"4","memory":"12Gi","nvidia.com/gpu":"1"}},"work":{"cpuUnits":57600,"gpuUnits":14400},"workloadClass":{"cpu":"cpu.mixed","gpu":"gpu.compute_bound"},"workloadProfile":{"cpuUtilization":0.28,"gpuUtilization":0.47,"memoryIntensity":0.58,"ioIntensity":0.08,"cpuFeedIntensityGpu":0.56}}
```

The fields that drive the simulation:

- `intentClass`: `performance` or `standard`, the class the pod declares to Joulie.
- `workloadClass.cpu` and `workloadClass.gpu`: the qualitative response to a cap (`cpu.compute_bound`, `cpu.memory_bound`, `cpu.io_bound`, `cpu.mixed`; `gpu.compute_bound`, `gpu.memory_bound`, `gpu.bandwidth_bound`, `gpu.mixed`).
- `workloadProfile`: the quantitative signals, `cpuUtilization`, `gpuUtilization`, `memoryIntensity`, `ioIntensity`, `cpuFeedIntensityGpu`, and `peerTransferIntensity` with `--peer-transfer-intensity`. They set the power a job draws and how much a cap slows it.
- `work`: the CPU and GPU work units, see [Work units](#work-units).
- `gang`: the pods of the workload advance only while all of them run.

### Schema versions

`--schema-version` selects the schema, which every record carries as `schemaVersion`.

| | `v3` (default) | `v2` |
|---|---|---|
| `sensitivity` on job records | absent, so the simulator reads 1 (a cap slows the job in full); `--cpu-sensitivity` and `--gpu-sensitivity` write the keys they name on every job | always present: `{"cpu":0,"gpu":0}` in default mode, `0.5` on performance jobs in diurnal mode |
| power-profile affinity | none; the class is `intentClass`, which the simulator renders as its placement intent (`SIM_WORKLOAD_INTENT`) | performance jobs carry `NotIn [eco]` on `joulie.io/power-profile` |
| work units | every pod's ideal runtime equals its workload's `durationSec` | scaled by utilisation, role and rate flags |
| diurnal GPU class | `gpu.compute_bound` | `gpu.compute`, which the physics model treats as `gpu.mixed` |
| `--arrival-window-sec`, `--max-duration-sec`, `--peer-transfer-intensity` | available | rejected |
| `--cpu-work-rate-per-core`, `--gpu-work-rate-per-gpu`, `--work-scale` | rejected | available |

Pass `--schema-version v2` for traces in the v2 format.

## How a workload is sampled

The default mode (`--mode default`) samples each logical workload in this order:

1. its arrival time,
2. its family,
3. its GPU count,
4. its duration,
5. its shared profile (classes, utilisation, intensities),
6. its pods and their requests,
7. the job records, one per pod.

The priors follow public studies of production GPU clusters: most jobs use one GPU while multi-GPU jobs dominate GPU time, durations are heavy-tailed, arrivals follow the working day, many workloads span several pods, and accelerator jobs use far less host CPU than they request. They are a prior shaped by those studies, not a fit to one cluster.

`--mode diurnal` samples instead single-pod CPU jobs, GPU training (1 to 8 GPUs) and GPU inference (1 or 2 GPUs) on a cosine day and night rate that peaks at `--diurnal-peak-rate` jobs per simulated minute over `--sim-hours`, with a 10 % chance of a burst of `--diurnal-burst-min` to `--diurnal-burst-max` jobs.

### Arrivals

Inter-arrival times are exponential, with a mean of `--mean-inter-arrival-sec` divided by the multiplier of the hour of day:

| Hours | 00 to 05 | 06 | 07 | 08 | 09, 10 | 11 | 12 | 13 | 14, 15 | 16 | 17 | 18 | 19 | 20 | 21 | 22 | 23 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| Multiplier | 0.10 to 0.20 | 0.40 | 0.65 | 0.85 | 1.00 | 0.95 | 0.80 | 0.85 | 1.00 | 0.95 | 0.85 | 0.65 | 0.50 | 0.40 | 0.30 | 0.22 | 0.18 |

On a day drawn with `--burst-day-probability` (default 0.25), one hour among 10, 11, 14, 15 and 16 is a burst: its multiplier is scaled by `--burst-multiplier` (2.0) and raised by `--burst-mean-jobs` / 16. On a day drawn with `--dip-day-probability` (0.30), one off-peak hour (3, 4, 5, 19, 20 or 21) is a dip, scaled by `--dip-multiplier` (0.08), as during a maintenance window. `--time-scale` maps trace seconds to simulated seconds for the hour of day.

### Families

A workload uses GPUs with probability `--gpu-ratio` (default 0.80). Its family, and the classes it gets:

| Family | Share | CPU class | GPU class |
|---|---|---|---|
| `debug_eval` | 45 % of GPU workloads | `cpu.mixed` | `gpu.memory_bound` |
| `single_gpu_training` | 25 % | `cpu.mixed` | `gpu.compute_bound` |
| `distributed_training` | 14 % | `cpu.mixed` | `gpu.compute_bound` |
| `parameter_server_training` | 7 % | `cpu.memory_bound` | `gpu.compute_bound` |
| `hpo_experiment` | 9 % | `cpu.mixed` | `gpu.mixed` |
| `cpu_preprocess` | 75 % of CPU-only workloads | `cpu.memory_bound` | none |
| `cpu_analytics` | 25 % | `cpu.compute_bound` | none |

### GPU count

`P(G=1) = 0.80`, `P(G=2) = 0.10`, `P(G=4) = 0.07`, `P(G=8) = 0.03`; `--min-gpu-request` raises the floor. `--amd-gpu-ratio` requests `amd.com/gpu` instead of `nvidia.com/gpu` for that share of GPU workloads.

### Duration

Lognormal per family, clamped:

| Family | Median | Range |
|---|---|---|
| `debug_eval` | 206 s | 30 s to 1000 s |
| `single_gpu_training` | 2 h | 15 min to 7 d |
| `distributed_training` | 4 h, x1.2 with 4 or more GPUs | 20 min to 14 d |
| `parameter_server_training` | 3 h | 15 min to 7 d |
| `hpo_experiment` | 2 h | 20 min to 3 d |
| `cpu_preprocess` | 15 min | 2 min to 8 h |
| `cpu_analytics` | 30 min | 5 min to 24 h |

### Utilisation and bottlenecks

Requests decide placement; the profile decides power and slowdown. The two are sampled apart, because production studies report GPUs well below full utilisation and host CPUs underused even when CPU and memory are requested in proportion to GPUs.

- **GPU utilisation**: a mean by GPU count, from the Philly study (1 GPU 0.5238, 2 GPUs 0.50, 4 GPUs 0.4518, 8 GPUs 0.5899, more 0.4039), lowered by 0.10 for `debug_eval` and `parameter_server_training`, 0.08 for `distributed_training` and 0.05 for `hpo_experiment`, then spread by up to 0.12 (0.16 from 4 GPUs).
- **CPU utilisation**, uniform: `cpu_preprocess` 0.50 to 0.75, `cpu_analytics` 0.75 to 0.95, `debug_eval` 0.20 to 0.40, `single_gpu_training` 0.20 to 0.45, `distributed_training` 0.15 to 0.35, `parameter_server_training` 0.20 to 0.40, `hpo_experiment` 0.15 to 0.35.
- **`memoryIntensity`**: highest for `cpu_preprocess` and `parameter_server_training` (0.70 to 0.92), then `debug_eval` (0.55 to 0.85), `distributed_training` (0.45 to 0.75), `hpo_experiment` (0.35 to 0.70), `single_gpu_training` (0.30 to 0.60) and `cpu_analytics` (0.20 to 0.45). A memory-bound job draws less power per unit of utilisation and slows less under a CPU cap; on a GPU it moves the job toward the memory-bound regime, where a moderate cap costs less.
- **`ioIntensity`**: `cpu_preprocess` 0.10 to 0.30, `debug_eval` 0.05 to 0.20. It softens the CPU slowdown further.
- **`cpuFeedIntensityGpu`**: from 0.15 to 0.35 (`debug_eval`) up to 0.40 to 0.75 (`parameter_server_training`). It makes a GPU job sensitive to a CPU cap, since the CPU feeds the GPUs.

[Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}) turns these signals into power and throughput.

### Pods

Each family expands into the pods of the Kubernetes pattern it stands for. The generator writes job records only; it renders no `PyTorchJob`, `MPIJob` or Katib `Experiment`.

| Family | Pods | Gang | Kubernetes pattern |
|---|---|---|---|
| `single_gpu_training`, `debug_eval` | 1 worker with all the GPUs | no | a single training or evaluation pod |
| `distributed_training` | 1 CPU-only launcher, `G` workers with 1 GPU each | yes | PyTorch distributed training, Kubeflow Trainer, MPI worker sets |
| `parameter_server_training` | 1 CPU-only parameter server (2 from 4 workers), `G` workers with 1 GPU each | yes | TensorFlow-style parameter servers |
| `hpo_experiment` | 1 controller, 2 to 4 trials (up to 8 from 4 GPUs) with 1 GPU each | no | a Katib experiment and its trials |
| `cpu_preprocess`, `cpu_analytics` | 1 CPU-only pod | no | a batch job |

The pods of a workload share its profile. A gang workload makes no progress until all its pods run, as a distributed job waits for its last worker.

## Work units

The simulator runs on abstract work units, so the generator converts a duration into work. In `v3` every pod of a workload gets:

```text
CPUUnits = durationSec * cpu_request * base_speed_per_core
GPUUnits = durationSec * gpu_request * base_speed_per_core
```

At full speed the simulator does `request * SIM_BASE_SPEED_PER_CORE` units per second, so a pod's ideal runtime, with no cap and no contention, is its workload's `durationSec`. `--base-speed-per-core` defaults to 1.0, as `SIM_BASE_SPEED_PER_CORE` does; a trace generated with another value must run on a simulator set to the same speed. The CPU request is taken as written to the trace (two decimals). Utilisation and intensity stay in the profile, where they drive power and slowdown, not the amount of work.

`v2` maps duration to work through utilisation (floored at 0.10), the role of the pod and `--cpu-work-rate-per-core`, `--gpu-work-rate-per-gpu` and, in diurnal mode, `--base-speed-per-core` (default 2.0) and `--work-scale`.

Two `v3` flags fit a trace into a run of fixed length:

- `--arrival-window-sec S` scales every submit offset so the last arrival is at `S` seconds, keeping the arrival pattern's shape.
- `--max-duration-sec D` clamps each workload's `durationSec` to `D` before its work is sized.

Both default to 0, which keeps the sampled values.

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
| `--arrival-window-sec`, `--max-duration-sec` | `0` | `v3`: fit the trace into a run |
| `--peer-transfer-intensity` | `0` | `v3`: `workloadProfile.peerTransferIntensity` of every job with two or more GPUs, the share of its GPU time spent copying between its GPUs ([Peer transfers]({{< relref "/docs/simulator/workload-simulator.md#peer-transfers" >}})) |

`go run ./simulator/cmd/workloadgen -h` lists every flag.

A benchmark may rewrite the placement of a generated trace to fit its cluster: experiment 02 keeps the logical workloads, work and timing, and points each pod's placement constraints at the KWOK nodes of the benchmark.

## Not modelled

- no fit of the priors to Helios, Philly or Alibaba traces inside the generator,
- no network or storage bandwidth,
- one shared profile per workload, with no phases over time,
- no rendering of `PyTorchJob`, `MPIJob`, Katib or Volcano and Kueue objects.

## References

Traces and workload studies:

- [HeliosData repository (SenseTime traces)](https://github.com/S-Lab-System-Group/HeliosData)
- [Characterization and Prediction of Deep Learning Workloads in Large-Scale GPU Datacenters](https://tianweiz07.github.io/Papers/21-sc.pdf), SC'21
- [Philly traces repository](https://github.com/msr-fiddle/philly-traces)
- Myeongjae Jeon et al., [Analysis of Large-Scale Multi-Tenant GPU Clusters for DNN Training Workloads](https://www.usenix.org/system/files/atc19-jeon.pdf), USENIX ATC'19
- [Alibaba cluster-trace-gpu-v2020](https://github.com/alibaba/clusterdata/blob/master/cluster-trace-gpu-v2020/README.md)

Modelling:

- Samuel Williams et al., [Roofline: an insightful visual performance model for multicore architectures](https://dl.acm.org/doi/10.1145/1498765.1498785)
- [Blox: A Modular Toolkit for Scheduling and Optimizing DAG-based Workflows in ML Clusters](https://arxiv.org/html/2312.12621v1)
- David Meisner et al., [Memory Performance at Reduced CPU Clock Speeds](https://www.usenix.org/system/files/conference/hotpower12/hotpower12-final21.pdf), HotPower'12
- [Characterizing the Impact of GPU Power Management on HPC Applications](https://arxiv.org/abs/2501.16371), PMBS 2025 preprint
- Tapasya Patki et al., [Comparing GPU Power and Frequency Capping for Energy Savings in Scientific Applications](https://ieeexplore.ieee.org/document/8944989), SC Workshops 2019

Kubernetes multi-pod workloads:

- [Kubeflow Trainer distributed training reference](https://www.kubeflow.org/docs/components/trainer/legacy-v1/reference/distributed-training/)
- [Kubeflow PyTorchJob guide](https://www.kubeflow.org/docs/components/trainer/legacy-v1/user-guides/pytorch/)
- [Kubeflow Trainer job scheduling with Volcano](https://www.kubeflow.org/docs/components/trainer/operator-guides/job-scheduling/volcano/)
- [Kubeflow legacy job scheduling guide](https://www.kubeflow.org/docs/components/trainer/legacy-v1/user-guides/job-scheduling/)
- [Kueue introduction](https://kubernetes.io/blog/2022/10/04/introducing-kueue/)
- [Kubeflow Katib overview](https://www.kubeflow.org/docs/components/katib/overview/)
- [Katib experiment configuration guide](https://www.kubeflow.org/docs/components/katib/user-guides/hp-tuning/configure-experiment/)
