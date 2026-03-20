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

Results from simulated benchmark experiments (Kind + KWOK clusters with physics-based power and cooling models):

- **20-29% cluster power savings** in heterogeneous GPU/CPU workloads through combined capping and scheduling.
- **6.4% savings from scheduling alone** -- energy-aware pod placement reduces consumption without any power cap enforcement (validated on a simulated 2,500-node cluster).
- **Zero application impact** -- workload-class annotations let performance-critical pods bypass power constraints while background jobs absorb savings.
- **Full observability** -- `kubectl joulie status`, Grafana dashboards, and Prometheus metrics give immediate visibility into per-node energy state.

## Where to start

If you are completely new, the smoothest path is:

1. [Getting Started]({{< relref "/docs/getting-started/_index.md" >}})
2. [Architecture]({{< relref "/docs/architecture/_index.md" >}})
3. [Hardware]({{< relref "/docs/hardware/_index.md" >}})
4. [Simulator]({{< relref "/docs/simulator/_index.md" >}})
5. [Experiments]({{< relref "/docs/experiments/_index.md" >}})

Core mental model:

- telemetry feeds the digital twin,
- the twin drives operator decisions (power caps, node profiles),
- the scheduler extender reads twin state to steer new pod placement,
- feedback from new placements updates telemetry, closing the loop.

## Section guide

- [Getting Started]({{< relref "/docs/getting-started/_index.md" >}})
  - core concepts, Helm-based install, workload class annotations, agent runtime modes, full configuration reference
- [Architecture]({{< relref "/docs/architecture/_index.md" >}})
  - operator, agent, digital twin, and scheduler extender roles; CRD definitions; policy algorithms; telemetry and actuation interfaces; kubectl plugin
- [Hardware]({{< relref "/docs/hardware/_index.md" >}})
  - CPU (RAPL) and GPU (NVML) support, heterogeneous node strategies, cap range discovery, hardware modeling for simulation
- [Simulator]({{< relref "/docs/simulator/_index.md" >}})
  - trace-driven workload simulation, power and cooling models, facility stress testing, workload distribution profiles
- [Experiments]({{< relref "/docs/experiments/_index.md" >}})
  - benchmark design, baseline comparisons, and measured power savings across heterogeneous clusters

## What to expect

- **Per-node digital twins**: telemetry → twin state → cap decisions and scheduling.
- **Kubernetes-native contracts**: 2 user-facing CRDs (`NodeHardware`, `NodeTwin`) + scheduling constraints as intent/supply language.
- **Observability tooling**: `kubectl joulie` plugin, Grafana dashboard, Prometheus metrics.
- **Practical path to adoption**: quickstart first, then progressive deep dives.
