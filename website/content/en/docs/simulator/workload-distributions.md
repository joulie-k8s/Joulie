---
title: "Workload Distributions"
weight: 16
---

This page documents the **statistical distributions and priors** behind the current workload generator.

Use it together with:

- [Workload Generation]({{< relref "/docs/simulator/workload-generation.md" >}})
- [Workload Simulator]({{< relref "/docs/simulator/workload-simulator.md" >}})
- [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}})

## What this page is for

The generator is no longer just a flat random-job emitter.
It now uses explicit priors for:

- arrival timing,
- GPU-count skew,
- duration shape,
- utilization,
- memory pressure,
- multi-pod workload structure.

This page makes those priors visible and explains why they are reasonable.

## 1. Arrival model

The current implementation uses a lightweight **NHPP-like** process:

- a baseline exponential inter-arrival sampler,
- modulated by an hourly multiplier,
- with optional burst windows.

That is not yet a full trace-fit pipeline, but it is designed to capture the main structure reported in Helios and in scheduler-evaluation work:

- workday-heavy submission activity,
- visible midday dips,
- lower submission activity overnight,
- and occasional bursty periods.

### Current hourly multipliers

- `00:00-07:59`: `0.70`
- `09:00-11:59`: `1.20`
- `12:00`: `0.85`
- `13:00-17:59`: `1.15`
- `18:00`: `0.85`
- `20:00-22:59`: `1.00`
- other hours: `0.95`

### Burst overlay

Optional burst parameters:

- `--burst-day-probability`
- `--burst-multiplier`
- `--burst-mean-jobs`

This is inspired by Blox-style simulator methodology, where trace-derived rates are combined with controlled spikes for stress testing.

## 2. GPU-count prior

The generator follows the trace-backed pattern that:

- single-GPU jobs dominate **job count**,
- larger jobs dominate **GPU-time**.

### Current default categorical prior

- `P(G=1) = 0.80`
- `P(G=2) = 0.10`
- `P(G=4) = 0.07`
- `P(G=8) = 0.03`

This is a practical HEP-oriented prior derived from the shape reported in Helios rather than a literal replay of one production cluster.

## 3. Duration model

The generator uses **heavy-tailed duration priors**, because public AI cluster traces consistently show that durations are not well represented by narrow or symmetric distributions.

### Current family-level approximations

- `debug_eval`
  - short-biased lognormal
  - clamped to `30s .. 1000s`
- `single_gpu_training`
  - long-tail lognormal
  - clamped to `15min .. 7d`
- `distributed_training`
  - longer-tail lognormal
  - clamped to `20min .. 14d`
- `parameter_server_training`
  - clamped to `15min .. 7d`
- `hpo_experiment`
  - clamped to `20min .. 3d`
- `cpu_preprocess`
  - clamped to `2min .. 8h`
- `cpu_analytics`
  - clamped to `5min .. 24h`

These choices are intended to preserve the Helios-style pattern:

- many short exploratory/evaluation jobs,
- fewer long-running training jobs,
- a strong long tail extending far beyond the median.

## 4. Requested resources vs used resources

One of the most important modeling choices in the generator is that it separates:

- **requested resources**
  - used by the scheduler,
- **used resources / intensity profile**
  - used by the physical model.

This separation matters because public studies report that:

- CPU and memory are often requested proportionally to GPUs,
- but host CPU compute utilization can still be low,
- while memory pressure can remain high,
- and GPU utilization is often substantially below 100% in production training clusters.

## 5. GPU utilization prior

The current implementation seeds GPU utilization from the ATC'19 Philly mean GPU-utilization values by GPU count.

### Reference points used in the current generator

- `1 GPU`: `0.5238`
- `4 GPUs`: `0.4518`
- `8 GPUs`: `0.5899`
- `16 GPUs`: `0.4039`

The generator then interpolates this into a small-cluster prior and adjusts by workload family:

- `debug_eval`: lower effective GPU utilization
- `distributed_training`: lower effective GPU utilization than single-GPU training
- `parameter_server_training`: similar downward adjustment

This is meant to reflect the well-known difference between:

- GPU allocation,
- GPU utilization,
- and actual useful throughput.

## 6. CPU utilization prior

The generator samples CPU utilization independently from CPU request size.

Examples from the current implementation:

- `cpu_preprocess`: `0.50 .. 0.75`
- `cpu_analytics`: `0.75 .. 0.95`
- `debug_eval`: `0.20 .. 0.40`
- `single_gpu_training`: `0.20 .. 0.45`
- `distributed_training`: `0.15 .. 0.35`
- `parameter_server_training`: `0.20 .. 0.40`

This reflects the observed pattern that many accelerator-heavy jobs are not CPU-compute-bound even though they still require host CPU resources.

## 7. Memory / IO / CPU-feed priors

The current generator samples three explicit bottleneck signals:

- `memoryIntensity`
- `ioIntensity`
- `cpuFeedIntensityGpu`

These are the bridge from workload generation into the throttling model.

### Interpretation

- high `memoryIntensity`
  - more memory-dominated behavior
  - softer slowdown under CPU throttling
- high `ioIntensity`
  - even softer CPU-side slowdown
- high `cpuFeedIntensityGpu`
  - GPU throughput becomes more sensitive to CPU throttling

This is deliberately aligned with the more realistic class-aware slowdown semantics described in [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}).

## 8. Structure prior

The current generator samples a **logical workload family** first, then derives pod structure from it.

This is directly motivated by the Alibaba PAI trace and Kubernetes-native ML systems, where a single logical workload may span multiple pods.

### Structure classes used today

- single-pod GPU job
- distributed training
- parameter-server style training
- HPO experiment
- CPU-only preprocessing / analytics jobs

### Current expansion behavior

- `distributed_training`
  - launcher + worker pods
  - `gang=true`
- `parameter_server_training`
  - parameter-server pods + worker pods
  - `gang=true`
- `hpo_experiment`
  - controller + multiple trial pods
  - no gang requirement by default in the current simulator path

## 9. Throttling-sensitivity motivation

The workload report does not just motivate arrival and resource distributions; it also motivates **class-aware slowdown**.

That is why Joulie models compute-bound, memory-bound, and mixed regimes rather than using a single slowdown curve for every job.

The core literature-backed idea is:

- compute-bound workloads should slow down more under reduced compute roof,
- memory-bound workloads should often slow down less,
- but the exact attenuation depends on how bandwidth, clocks, and control surfaces interact.

The workload generator therefore emits priors that support this model rather than fighting it.

## 10. What is implemented vs not yet implemented

### Implemented today

- day-shaped arrivals with bursts
- heavy-tailed durations
- GPU-count skew
- workload-family-based generation
- multi-pod logical workloads
- gang metadata for distributed/PS workloads
- shared workload intensity profiles
- explicit resource-vs-utilization separation

### Not yet implemented

- direct fitting from Helios / Philly / Alibaba traces inside `workloadgen`
- profile-bundle loading from a fitted YAML/JSON file
- phased time-series workload profiles
- explicit network and disk models
- calibrated per-framework workload templates

So the current implementation is **research-informed and structure-aware**, but not yet a full trace-fitting platform.

## References

- [WD1] HeliosData repository  
  <https://github.com/S-Lab-System-Group/HeliosData>
- [WD2] Tianwei Zhang et al., Characterization and Prediction of Deep Learning Workloads in Large-Scale GPU Datacenters (SC'21)  
  <https://tianweiz07.github.io/Papers/21-sc.pdf>
- [WD3] Philly traces repository  
  <https://github.com/msr-fiddle/philly-traces>
- [WD4] Myeongjae Jeon et al., Analysis of Large-Scale Multi-Tenant GPU Clusters for DNN Training Workloads (USENIX ATC'19)  
  <https://www.usenix.org/system/files/atc19-jeon.pdf>
- [WD5] Alibaba cluster-trace-gpu-v2020 README  
  <https://github.com/alibaba/clusterdata/blob/master/cluster-trace-gpu-v2020/README.md>
- [WD6] Samuel Williams et al., Roofline: an insightful visual performance model for multicore architectures  
  <https://dl.acm.org/doi/10.1145/1498765.1498785>
- [WD7] Blox arrival/burst evaluation reference  
  <https://arxiv.org/html/2312.12621v1>
- [WD8] David Meisner et al., Memory Performance at Reduced CPU Clock Speeds, HotPower'12  
  <https://www.usenix.org/system/files/conference/hotpower12/hotpower12-final21.pdf>
- [WD9] Characterizing the Impact of GPU Power Management on HPC Applications, PMBS 2025 preprint used in the workload report  
  <https://arxiv.org/abs/2501.16371>
- [WD10] Tapasya Patki et al., Comparing GPU Power and Frequency Capping for Energy Savings in Scientific Applications, SC Workshops 2019  
  <https://ieeexplore.ieee.org/document/8944989>
