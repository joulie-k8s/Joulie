---
title: "Workload Generation"
weight: 15
---

This page documents how Joulie generates **realistic AI workload traces** for the simulator.

It is separate from [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}}):

- this page explains how traces are **generated**,
- the workload-simulator page explains how those traces are **consumed at runtime**.

## Scope

The current generator is designed to be realistic for:

- AI-oriented Kubernetes clusters,
- CPU + GPU workloads,
- memory-pressure-sensitive jobs,
- multi-pod logical workloads such as distributed training and HPO-style experiments.

The current generator **does not** explicitly model:

- network bandwidth,
- disk IO throughput,
- exact framework-level communication protocols.

Instead, distributed overhead is folded into workload structure and bottleneck signals such as:

- `gpuUtilization`,
- `memoryIntensity`,
- `cpuFeedIntensityGpu`.

## Why the generator changed

The original synthetic generator was intentionally simple: it sampled single jobs with coarse CPU/GPU classes.
That was fine for basic scheduler and power-loop testing, but it was too weak for realistic AI experiments.

The current generator now reflects the main public observations from production GPU clusters:

- many jobs are **single-GPU**,
- a smaller fraction of **multi-GPU jobs dominates GPU-time**,
- job durations are **heavy-tailed**,
- arrivals follow a **daily pattern**,
- multi-pod logical workloads are common,
- and accelerator jobs often have **lower host CPU compute utilisation than their CPU requests would suggest**.

## Evidence base

The current defaults are grounded in the following public sources.

### Production traces and workload studies

- [HeliosData repository (SenseTime traces)](https://github.com/S-Lab-System-Group/HeliosData)
- [Characterization and Prediction of Deep Learning Workloads in Large-Scale GPU Datacenters, SC'21](https://tianweiz07.github.io/Papers/21-sc.pdf)
- [Philly traces repository](https://github.com/msr-fiddle/philly-traces)
- [Analysis of Large-Scale Multi-Tenant GPU Clusters for DNN Training Workloads, USENIX ATC'19](https://www.usenix.org/system/files/atc19-jeon.pdf)
- [Alibaba cluster-trace-gpu-v2020 README](https://github.com/alibaba/clusterdata/blob/master/cluster-trace-gpu-v2020/README.md)

### Modelling and scheduler references

- [Roofline: an insightful visual performance model for multicore architectures](https://dl.acm.org/doi/10.1145/1498765.1498785)
- [Blox: A Modular Toolkit for Scheduling and Optimizing DAG-based Workflows in ML Clusters](https://arxiv.org/html/2312.12621v1)

### Kubernetes-native multi-pod workload references

- [Kubeflow Trainer distributed training reference](https://www.kubeflow.org/docs/components/trainer/legacy-v1/reference/distributed-training/)
- [Kubeflow PyTorchJob guide](https://www.kubeflow.org/docs/components/trainer/legacy-v1/user-guides/pytorch/)
- [Kubeflow Trainer job scheduling with Volcano](https://www.kubeflow.org/docs/components/trainer/operator-guides/job-scheduling/volcano/)
- [Kueue introduction](https://kubernetes.io/blog/2022/10/04/introducing-kueue/)
- [Kubeflow Katib overview](https://www.kubeflow.org/docs/components/katib/overview/)
- [Katib experiment configuration guide](https://www.kubeflow.org/docs/components/katib/user-guides/hp-tuning/configure-experiment/)

## Trace format

The generator now emits two kinds of JSONL records:

- `type=workload`: logical workload metadata
- `type=job`: pod-expanded runnable records consumed by the simulator

The simulator currently consumes the `type=job` records directly and ignores `type=workload` metadata records.
The workload metadata is still useful for inspection, debugging, and future calibration tooling.

### Logical workload record

Example:

```json
{"type":"workload","schemaVersion":"v2","workloadId":"workload-000001","submitTimeOffsetSec":12.4,"namespace":"default","workloadType":"distributed_training","gang":true,"durationSec":14400,"workloadClass":{"cpu":"cpu.mixed","gpu":"gpu.compute_bound"},"sharedIntensityProfile":{"cpuUtilization":0.28,"gpuUtilization":0.47,"memoryIntensity":0.58,"ioIntensity":0.08,"cpuFeedIntensityGpu":0.56},"pods":[{"role":"launcher","replicas":1,"requests":{"cpu":"2","memory":"4Gi"},"gang":true},{"role":"worker","replicas":4,"requests":{"cpu":"4","memory":"12Gi","nvidia.com/gpu":"1"},"gang":true}]}
```

### Pod-expanded job record

Example:

```json
{"type":"job","schemaVersion":"v2","jobId":"workload-000001-worker-01","workloadId":"workload-000001","workloadType":"distributed_training","podRole":"worker","gang":true,"submitTimeOffsetSec":12.4,"namespace":"default","podTemplate":{"requests":{"cpu":"4","memory":"12Gi","nvidia.com/gpu":"1"}},"work":{"cpuUnits":1024,"gpuUnits":6800},"sensitivity":{"cpu":0.65,"gpu":0.90},"workloadClass":{"cpu":"cpu.mixed","gpu":"gpu.compute_bound"},"workloadProfile":{"cpuUtilization":0.28,"gpuUtilization":0.47,"memoryIntensity":0.58,"ioIntensity":0.08,"cpuFeedIntensityGpu":0.56}}
```

## Statistical model

The generator follows a hierarchical sampling path:

1. sample arrival time,
2. sample workload family,
3. sample GPU footprint,
4. sample duration,
5. sample shared workload profile,
6. sample pod structure and per-role resource requests,
7. expand the logical workload into runnable pod-level jobs.

## Arrival model

Joulie now uses a simple **NHPP-like day-shaped arrival process** with an hourly rate multiplier and optional bursts.

The current built-in hourly prior reflects Helios-style behavior:

- lower rate at night (`00:00-08:00`),
- visible lunch/dinner dips,
- higher submission activity during working hours.

Current built-in multipliers:

- `00:00-07:59`: `0.70`
- `12:00`: `0.85`
- `18:00`: `0.85`
- `09:00-11:59`: `1.20`
- `13:00-17:59`: `1.15`
- `20:00-22:59`: `1.00`
- remaining hours: `0.95`

Inter-arrival is sampled as:

```text
delta ~ Exp(mean_inter_arrival / hourly_multiplier)
```

Optional burst behavior is also supported:

- `--burst-day-probability`
- `--burst-multiplier`
- `--burst-mean-jobs`

This is intentionally lightweight, but it captures the key point from Helios and Blox-style evaluation work: realistic clusters are not homogeneous Poisson streams with a single flat rate.

## Workload families

The current generator samples from these workload families.

### GPU-backed logical workloads

Conditional on `--gpu-ratio`, the generator currently uses:

- `debug_eval`: 45%
- `single_gpu_training`: 25%
- `distributed_training`: 14%
- `parameter_server_training`: 7%
- `hpo_experiment`: 9%

### CPU-only logical workloads

For non-GPU logical workloads:

- `cpu_preprocess`: 75%
- `cpu_analytics`: 25%

These are not claimed to be universal production truth. They are the current **HEP-friendly prior** inspired by the public traces:

- many short exploratory jobs,
- many single-GPU runs,
- fewer but more expensive multi-GPU jobs,
- some multi-pod training and HPO structure.

## GPU-count distribution

The current default categorical prior is:

- `P(G=1) = 0.80`
- `P(G=2) = 0.10`
- `P(G=4) = 0.07`
- `P(G=8) = 0.03`

This follows the key shape reported in Helios:

- single-GPU jobs dominate job count,
- but larger jobs dominate GPU-time.

## Duration model

Durations are heavy-tailed and workload-family-specific.

Current built-in approximations:

- `debug_eval`: lognormal around Helios-like short-job behavior, clamped to `30s .. 1000s`
- `single_gpu_training`: long-tail lognormal, clamped to `15min .. 7d`
- `distributed_training`: longer-tail lognormal, clamped to `20min .. 14d`
- `parameter_server_training`: clamped to `15min .. 7d`
- `hpo_experiment`: clamped to `20min .. 3d`
- `cpu_preprocess`: clamped to `2min .. 8h`
- `cpu_analytics`: clamped to `5min .. 24h`

This is meant to preserve the public observation that AI job durations are not well represented by a narrow bell curve.

## Utilisation and bottleneck priors

The generator explicitly separates:

- **requested resources**, used for placement,
- **utilisation/intensity profile**, used for slowdown and power.

That separation matters because ATC'19 reports that many training jobs underutilise GPU cycles, and also that host CPU cycles are often underutilised even when CPU and memory are allocated proportionally to requested GPUs.

### GPU utilisation prior

The built-in `gpuUtilization` prior is seeded from the ATC'19 Philly means by total GPU count:

- `1 GPU`: `0.5238`
- `4 GPUs`: `0.4518`
- `8 GPUs`: `0.5899`
- `16 GPUs`: `0.4039`

The generator interpolates this into a practical small-cluster prior:

- `1 GPU`: `0.5238`
- `2 GPUs`: `0.50`
- `4 GPUs`: `0.4518`
- `8 GPUs`: `0.5899`

Then it adjusts by workload type:

- `debug_eval`: lower effective GPU utilisation
- `distributed_training`: lower effective GPU utilisation because coordination overhead is folded into the shared profile
- `parameter_server_training`: similar reduction

### CPU utilisation prior

CPU utilisation is sampled independently from CPU requests.

Examples from the current generator:

- `cpu_preprocess`: `0.50 .. 0.75`
- `cpu_analytics`: `0.75 .. 0.95`
- `debug_eval`: `0.20 .. 0.40`
- `single_gpu_training`: `0.20 .. 0.45`
- `distributed_training`: `0.15 .. 0.35`

This reflects the public observation that many accelerator-heavy jobs are not CPU-compute-bound even when they still consume CPU and memory resources.

### Memory / IO / CPU-feed intensities

The shared profile also samples:

- `memoryIntensity`
- `ioIntensity`
- `cpuFeedIntensityGpu`

Examples:

- `parameter_server_training`: high `memoryIntensity`
- `distributed_training`: moderate-to-high `cpuFeedIntensityGpu`
- `cpu_preprocess`: high `memoryIntensity`, low-to-moderate `ioIntensity`
- `debug_eval`: more memory-heavy and less GPU-saturated than long training

These fields are what connect the workload generator to the physical model documented in [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}).

## Multi-pod workload structures

The generator now emits realistic pod structures instead of only single pods.

### `single_gpu_training`

- 1 worker pod
- all GPUs requested by that pod

### `distributed_training`

- 1 launcher pod (CPU-only)
- `G` worker pods, typically `1 GPU` each
- `gang=true`

### `parameter_server_training`

- `G` worker pods (`1 GPU` each)
- `1-2` CPU-only parameter-server pods
- `gang=true`

### `hpo_experiment`

- 1 controller pod
- `K` trial pods
- each trial usually requests `1 GPU`
- not gang-coupled by default in the current simulator path

This is a good fit for the report requirement that a single logical workload may span multiple pods while preserving a shared performance and power profile.

## How work units are derived

The simulator still runs on abstract work units, so the generator maps duration and utilization into work.

Current mapping:

```text
CPUUnits ~= duration_sec * cpu_request * cpu_utilization * cpu_work_rate_per_core
GPUUnits ~= duration_sec * gpu_request * gpu_utilization * gpu_work_rate_per_gpu
```

with small role-specific scaling factors for:

- launchers,
- controllers,
- parameter servers,
- workers,
- trials.

This is not a trace-replay of exact instruction counts. It is a practical way to make requested duration, requested resources, and throttling sensitivity line up inside the simulator.

## Gang semantics in the simulator

The generator now marks distributed and parameter-server workloads with `gang=true`.

The simulator uses that metadata to avoid advancing workload progress until all pods in the gang are running.
This makes multi-pod workloads behave more like real distributed jobs rather than a bag of unrelated pods.

## CLI summary

The main generator remains:

```bash
go run ./simulator/cmd/workloadgen --out trace.jsonl
```

Useful knobs:

- `--jobs`
- `--mean-inter-arrival-sec`
- `--seed`
- `--gpu-ratio`
- `--perf-ratio`
- `--eco-ratio`
- `--burst-day-probability`
- `--burst-multiplier`
- `--burst-mean-jobs`
- `--emit-workload-records`
- `--cpu-work-rate-per-core`
- `--gpu-work-rate-per-gpu`

## What is still intentionally simplified

A few things are still simplified compared with a full production-trace replay system:

- no direct fitting from Helios / Philly / Alibaba traces inside the generator yet
- no explicit network model
- no explicit storage-bandwidth model
- no phased time-series profile yet; profiles are currently shared averages per logical workload
- no manifest renderer yet for `PyTorchJob`, `MPIJob`, or `Katib Experiment`

That said, the current implementation is a large step up from a toy generator:

- arrivals are no longer flat,
- job sizes are no longer uniform,
- durations are no longer narrow,
- multi-pod AI workloads are first-class,
- and utilisation/bottleneck priors are tied to public workload studies.

## References

### Trace datasets and workload studies

- [WG1] HeliosData repository (SenseTime traces)  
  <https://github.com/S-Lab-System-Group/HeliosData>
- [WG2] Tianwei Zhang et al., Characterization and Prediction of Deep Learning Workloads in Large-Scale GPU Datacenters (SC'21)  
  <https://tianweiz07.github.io/Papers/21-sc.pdf>
- [WG3] Philly traces repository  
  <https://github.com/msr-fiddle/philly-traces>
- [WG4] Myeongjae Jeon et al., Analysis of Large-Scale Multi-Tenant GPU Clusters for DNN Training Workloads (USENIX ATC'19)  
  <https://www.usenix.org/system/files/atc19-jeon.pdf>
- [WG5] Alibaba cluster-trace-gpu-v2020 README  
  <https://github.com/alibaba/clusterdata/blob/master/cluster-trace-gpu-v2020/README.md>

### Modelling references

- [WG6] Samuel Williams et al., Roofline: an insightful visual performance model for multicore architectures  
  <https://dl.acm.org/doi/10.1145/1498765.1498785>
- [WG7] Blox workflow/scheduler evaluation reference used for burst-style arrival thinking  
  <https://arxiv.org/html/2312.12621v1>

### Kubernetes multi-pod AI workload references

- [WG8] Kubeflow Trainer distributed training reference  
  <https://www.kubeflow.org/docs/components/trainer/legacy-v1/reference/distributed-training/>
- [WG9] Kubeflow PyTorchJob guide  
  <https://www.kubeflow.org/docs/components/trainer/legacy-v1/user-guides/pytorch/>
- [WG10] Kubeflow Trainer scheduling with Volcano  
  <https://www.kubeflow.org/docs/components/trainer/operator-guides/job-scheduling/volcano/>
- [WG11] Kueue introduction  
  <https://kubernetes.io/blog/2022/10/04/introducing-kueue/>
- [WG12] Kubeflow Katib overview  
  <https://www.kubeflow.org/docs/components/katib/overview/>
- [WG13] Katib experiment configuration guide  
  <https://www.kubeflow.org/docs/components/katib/user-guides/hp-tuning/configure-experiment/>

These references support the current Joulie workload generator in four ways:

- trace-shaped arrival and duration priors,
- realistic GPU-count skew and utilisation priors,
- multi-pod workload structure,
- and Kubernetes-native execution patterns for distributed training and HPO.


## Gap analysis against the workload-generation report

The current implementation covers the **core generator/runtime path** described in the workload report:

- realistic AI-oriented workload families
- daily arrival shaping with bursts
- heavy-tailed durations
- GPU-count skew
- multi-pod logical workloads
- gang-style progress for distributed/PS workloads
- explicit utilization and bottleneck priors

However, a few report items are still not implemented and should be read as **future work**, not current behavior:

- direct fitting from Helios / Philly / Alibaba traces inside `workloadgen`
- profile-bundle import/export for fitted priors
- phase/time-series intensity profiles instead of only shared averages
- direct manifest rendering for Kubeflow / Katib objects
- calibration-mode telemetry ingestion loop

So the implementation is now clearly beyond a toy generator, but it is not yet the full end-state envisioned by the report.
