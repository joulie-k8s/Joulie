---
title: Documentation
linkTitle: Docs
menu:
  main:
    weight: 10
---

Joulie is a Kubernetes-native energy management system that uses per-node digital twins to optimize data center power consumption.
It ingests real-time telemetry from every node (CPU/GPU power draw, thermal state, per-pod utilization) to maintain a continuously updated model of the cluster's energy state.
That model drives two things: power cap enforcement (via RAPL and NVML) and scheduling decisions that steer workloads toward the most energy-efficient nodes.

## What can Joulie do?

The results below come from the simulator (Kind + KWOK clusters with modelled power and cooling), Joulie's test bed for its behaviour: caps applied, placement, policies and governors. Their energy figures rest on hardware behaviour the simulator assumes, so they do not show what Joulie saves on real hardware. What meaningful experiments need is tracked in the milestone [Evidence-based evaluation](https://github.com/joulie-k8s/Joulie/milestone/1).

- **Trade power for throughput at about the same energy per unit**: on 33 H100 NVL nodes, the two policies drew 6 to 9 % less power and did 6 to 10 % less GPU work than no management, at about the same GPU-node energy per unit of work (experiment 03, at regime, 2 seeds). The CPU-only and mixed-GPU experiments (01, 02) depend on a simulator assumption, that every core and every GPU does the same work per second whatever its model ([per-device speed](https://github.com/joulie-k8s/Joulie/issues/107)); their reports give the numbers.
- **Performance pods stay uncapped**: workload-class annotations keep performance-critical pods on uncapped nodes, where they wait when those nodes are full (experiment 03: 9 to 10 performance jobs waiting on average under the policies, against 0.7 with no management).
- **Full observability** -- `kubectl joulie` (cluster status, live dashboard, node details, pod placement), Grafana dashboards, and Prometheus metrics give immediate visibility into per-node energy state.

## Where to start

If you are completely new, the smoothest path is:

1. [Getting Started]({{< relref "/docs/getting-started/_index.md" >}})
2. [Architecture]({{< relref "/docs/architecture/_index.md" >}})
3. [Hardware]({{< relref "/docs/hardware/_index.md" >}})
4. [Simulator]({{< relref "/docs/simulator/_index.md" >}})
5. [Experiments]({{< relref "/docs/experiments/_index.md" >}})

Core mental model:

- telemetry feeds the digital twin,
- the twin drives controller manager decisions (power caps, node profiles),
- the scheduler extender reads twin state to steer new pod placement,
- feedback from new placements updates telemetry, closing the loop.

## Section guide

- [Getting Started]({{< relref "/docs/getting-started/_index.md" >}})
  - core concepts, Helm-based install, workload class annotations, agent runtime modes, full configuration reference, upgrading
- [Architecture]({{< relref "/docs/architecture/_index.md" >}})
  - controller manager, agent, digital twin, and scheduler extender roles; CRD definitions; policy algorithms; telemetry and actuation interfaces; kubectl plugin
- [Hardware]({{< relref "/docs/hardware/_index.md" >}})
  - CPU (RAPL) and GPU (NVML) support, heterogeneous node strategies, cap range discovery, hardware modeling for simulation
- [Simulator]({{< relref "/docs/simulator/_index.md" >}})
  - a simulated cluster on emulated nodes, KWOK and standalone modes, workload traces and scenarios, power and facility models
- [Experiments]({{< relref "/docs/experiments/_index.md" >}})
  - benchmark design and baseline comparisons on simulated clusters, and what their energy figures rest on

## What to expect

- **Per-node digital twins**: telemetry → twin state → cap decisions and scheduling.
- **Kubernetes-native contracts**: 2 user-facing CRDs (`NodeHardware`, `NodeTwin`) + scheduling constraints as intent/supply language.
- **Observability tooling**: `kubectl joulie` plugin, Grafana dashboard, Prometheus metrics.
- **Practical path to adoption**: quickstart first, then progressive deep dives.
