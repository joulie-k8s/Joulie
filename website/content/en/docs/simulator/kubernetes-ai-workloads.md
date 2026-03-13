---
title: "Kubernetes AI Workloads"
weight: 17
---

This page explains how the logical workload structures used by Joulie map onto common Kubernetes-native AI workload patterns.

It is mainly a documentation page today.
The current simulator generator emits the **structure metadata and pod-expanded jobs**, but it does **not yet** render `PyTorchJob`, `MPIJob`, or `Katib Experiment` manifests directly.

## Why this page exists

The workload-generation report makes an important point:

- realistic AI workloads are often **not single pods**,
- and a single logical workload may map to:
  - a launcher + workers,
  - parameter servers + workers,
  - or a controller + many HPO trial pods.

That distinction matters even in a simulator, because power and slowdown should often be understood at the **logical workload** level, not only at the pod level.

## Current Joulie mapping

The current generator emits:

- a logical `type=workload` record,
- and one or more `type=job` records derived from it.

The simulator consumes the expanded pod-level records, while keeping workload-level metadata such as:

- `workloadId`
- `workloadType`
- `podRole`
- `gang`

## Distributed training

### Current Joulie representation

- one `launcher` pod
- `G` `worker` pods
- `gang=true`

This is meant to approximate the common pattern used by:

- PyTorch distributed training,
- Kubeflow Trainer / Training Operator,
- MPI-style worker sets.

### Why gang semantics matter

A distributed job is not realistically represented as “some workers are running, so useful progress should continue normally.”

That is why the simulator now treats `gang=true` workloads specially:

- workload progress waits until all pods in the gang are running.

This is a practical approximation of real distributed-training startup and co-scheduling behavior.

## Parameter-server training

### Current Joulie representation

- `1-2` CPU-only `ps` pods
- `G` GPU `worker` pods
- shared workload profile
- `gang=true`

This is inspired by Alibaba PAI-style role hierarchy and older TF-style parameter-server deployments.

## Hyperparameter optimisation

### Current Joulie representation

- one `controller` pod
- multiple `trial` pods
- shared workload-level prior
- no gang requirement by default

This is meant to capture the idea that one logical HPO experiment can fan out into several trial pods while still being one experiment-level workload.

## What Joulie does not do yet

The current implementation does **not yet** include:

- direct manifest rendering to `PyTorchJob`
- direct manifest rendering to `MPIJob`
- direct manifest rendering to `Katib Experiment`
- integration with Volcano / Kueue objects

So this page is partly architectural guidance for the next step, not a claim that those rendering paths already exist.

## Why these references still matter now

Even before manifest rendering exists, these references are useful because they justify the logical structures already present in the generator:

- multi-worker distributed training,
- role-based pod sets,
- gang-like startup semantics,
- HPO as one experiment with many trial pods.

## References

- [KW1] Kubeflow Trainer distributed training reference  
  <https://www.kubeflow.org/docs/components/trainer/legacy-v1/reference/distributed-training/>
- [KW2] Kubeflow PyTorchJob guide  
  <https://www.kubeflow.org/docs/components/trainer/legacy-v1/user-guides/pytorch/>
- [KW3] Kubeflow Trainer scheduling with Volcano  
  <https://www.kubeflow.org/docs/components/trainer/operator-guides/job-scheduling/volcano/>
- [KW4] Kubeflow legacy job scheduling guide  
  <https://www.kubeflow.org/docs/components/trainer/legacy-v1/user-guides/job-scheduling/>
- [KW5] Kueue introduction  
  <https://kubernetes.io/blog/2022/10/04/introducing-kueue/>
- [KW6] Kubeflow Katib overview  
  <https://www.kubeflow.org/docs/components/katib/overview/>
- [KW7] Katib experiment configuration guide  
  <https://www.kubeflow.org/docs/components/katib/user-guides/hp-tuning/configure-experiment/>
- [KW8] Alibaba cluster-trace-gpu-v2020 README  
  <https://github.com/alibaba/clusterdata/blob/master/cluster-trace-gpu-v2020/README.md>
