---
title: "Energy-Aware Scheduling"
weight: 45
description: "How Joulie combines measured power (OpenTelemetry hardware metrics or Kepler), workload classification, digital twin predictions, and PUE-weighted scoring to make energy-aware scheduling decisions."
---

Joulie's scheduler extender makes placement decisions informed by real-time energy telemetry, workload characteristics, and facility-level power conditions. This page describes the full pipeline from metrics collection through scoring and optional rescheduling.

## End-to-end pipeline

The energy-aware scheduling pipeline has five stages:

```
RAPL/NVML readings as OpenTelemetry metrics (or Kepler)
  -> Prometheus (OTLP receiver, or scrape)
    -> Digital twin (NodeTwin.status)
      -> Scheduler extender (filter + score)
        -> Placement decision
```

Each stage runs independently and communicates through Kubernetes CRDs or Prometheus queries. There is no monolithic scheduling engine; each component does one thing and feeds the next.

### Stage 1: Telemetry collection

The twin's measured power comes from the hardware energy interfaces (Intel and AMD RAPL for CPU packages, NVML or ROCm SMI for GPUs), published per component as the OpenTelemetry hardware metrics `hw.energy` and `hw.power` and pushed over OTLP to Prometheus. The simulator in emulate mode publishes them this way, and an OpenTelemetry Collector can translate other exporters into them. The [input contract]({{< relref "/docs/architecture/telemetry.md#input-metrics-opentelemetry" >}}) lists every name.

[Kepler](https://github.com/sustainable-computing-io/kepler) is the other supported source. It instruments the Linux kernel via eBPF, reads the same hardware interfaces and attributes energy to individual containers using kernel tracepoints. The controller manager reads its node counters with `NODE_POWER_PRESET=kepler` (Kepler 0.10 and later) or `kepler-legacy` (0.7 to 0.9). Kepler reports node GPU energy only from 0.12, behind an experimental feature that is off by default.

The agent also reads RAPL and NVML directly on its node, for cap enforcement.

Together, these produce three categories of signal:

| Source | Granularity | Metrics |
|--------|-------------|---------|
| OpenTelemetry producer | Per component (CPU package, GPU) and host | `hw.energy`, `hw.power`, `hw.host.energy`, `hw.host.power` (`hw_energy_joules_total` and so on in Prometheus) |
| Kepler | Per node and per container | `kepler_node_cpu_active_joules_total`, `kepler_node_cpu_idle_joules_total` and `kepler_node_gpu_joules_total` (0.10 and later), `kepler_node_package_joules_total` (0.7 to 0.9), `kepler_container_*_joules_total` |
| RAPL, NVML/DCGM on the node | Per socket, per GPU | CPU package power for the agent's caps; GPU power draw, temperature, utilization |

### Stage 2: Prometheus aggregation

OpenTelemetry producers push into Prometheus' OTLP receiver, and exporters such as Kepler are scraped into the same store. The controller manager queries it with one grouped query per input every reconcile, over a `rate()` window of 5 minutes by default (`PROMETHEUS_RATE_WINDOW`). This decouples data collection from decision-making and lets each consumer query at its own cadence.

### Stage 3: Digital twin computation

The controller manager's twin controller ingests `NodeHardware` (static capabilities) and Prometheus telemetry to compute three scores per node, written to `NodeTwin.status`:

- **Power headroom** (0-100): remaining power budget before hitting thermal or PSU limits.
- **CoolingStress** (0-100): predicted fraction of cooling capacity in use.
- **PSUStress** (0-100): predicted fraction of rack power capacity in use (tracked but reserved for future use in scoring).

### Stage 4: Scheduler filter and scoring

When kube-scheduler has a pending pod, it calls the Joulie extender at two endpoints:

**Filter** (`POST /filter`): The extender reads the pod's `joulie.io/workload-class` annotation. Performance pods are rejected from nodes whose `NodeTwin.status.schedulableClass` is `eco` or `draining`. Standard pods pass all nodes. This is a hard constraint: rejected nodes are excluded from scoring.

**Prioritize** (`POST /prioritize`): The extender scores each surviving node 0-100. The score is **pod-specific**: it accounts for the power this particular pod would add to the node:

```
projectedPower = measuredPower + podMarginalPower
headroomScore  = (cappedPower - projectedPower) / cappedPower * 100
clusterTrend   = sum of all per-node powerTrend (W/min)
trendScale     = 2.0 if |clusterTrend| > 500 else 6.0
trendBonus     = -clamp(powerTrend / trendScale, -25, 25)

score = headroomScore * 0.7
      + (100 - coolingStress) * 0.15
      + trendBonus
      + profileBonus       (+10 for standard pods on eco nodes)
      + pressureRelief     (up to -30 for standard pods on congested perf nodes)
```

Key properties:
- **Headroom is relative to capped power**, not TDP. Eco nodes (capped lower) run out of headroom sooner.
- **Pod marginal power** is estimated from the pod's CPU/GPU requests and the node's hardware profile. A GPU-heavy pod reduces headroom more than a CPU-only pod.
- **headroomScore can go negative** if the pod would exceed the node's cap; such nodes get score 0.
- **Trend bonus** uses adaptive scaling (±25 pts): trendScale=6.0 at steady state, trendScale=2.0 during cluster-wide bursts (|clusterTrend| > 500 W/min). This provides strong smoothing during normal operation while avoiding over-reaction during coordinated ramps.

See [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}}) for the full scoring logic, worked examples, and environment variable reference.

### Stage 5: Placement

The Kubernetes scheduler combines the extender's scores with its own resource-fit checks (CPU/memory requests, taints, affinity) and places the pod on the highest-scoring node. The extender never overrides Kubernetes resource accounting; it only participates in the filter and prioritize hooks.

## PUE-weighted marginal scoring

Power Usage Effectiveness (PUE) measures the ratio of total facility power to IT equipment power. A PUE of 1.4 means the facility consumes 40% overhead for cooling, power distribution, and lighting. Joulie's scoring accounts for this overhead.

### Why PUE matters for scheduling

Two nodes drawing identical IT power can have different total energy costs if one is in a rack with worse cooling efficiency. Scheduling purely on IT power ignores the facility multiplier and can lead to suboptimal placement.

### How Joulie incorporates PUE

The CoolingStress score in `NodeTwin.status` serves as a proxy for marginal PUE impact. When cooling stress is high on a node, placing additional workloads there increases the cooling system's marginal power draw disproportionately. The scoring formula penalizes high cooling stress:

```
coolingPenalty = (100 - coolingStress) * 0.15
```

At coolingStress = 80, this contributes only 3 points (vs. 15 at coolingStress = 0). The effect is that nodes near their cooling capacity become less attractive even if they have spare compute headroom. This steers workloads toward nodes where the marginal PUE impact is lower.

The cooling stress term (0.15 weight) provides a facility-level energy efficiency signal, while headroom relative to the capped power budget dominates scoring at 70% weight. PSU stress is tracked but reserved for future use in scoring. A trend bonus (up to ±10 points) rewards nodes whose power draw is falling and penalizes nodes whose power is rising, improving responsiveness to dynamic workloads.

## State of the art

Joulie's energy-aware scheduling builds on several lines of research. This section places the design in context and identifies the relevant prior work.

### Energy measurement

**Kepler** (Amaral et al., 2023) provides the per-container energy attribution that underpins Joulie's classification pipeline. By attaching eBPF probes to kernel scheduling events and reading hardware energy counters, Kepler disaggregates node-level power into per-container contributions without requiring hardware modifications or hypervisor-level instrumentation.

**RAPL** (David et al., 2010) is the Intel hardware interface for reading and capping CPU and DRAM energy consumption. RAPL's Running Average Power Limit model exposes per-socket energy counters and power capping through MSRs. Joulie's agent reads RAPL for node-level telemetry and writes RAPL power limits for cap enforcement.

> H. David, E. Gorbatov, U.R. Hanebutte, R. Khanna, and C. Le. "RAPL: Memory Power Estimation and Capping." Proceedings of the International Symposium on Computer Architecture (ISCA), 2010.

> M. Amaral et al. "Kepler: A Framework for Energy-Efficient Kubernetes Clusters." Proceedings of the ACM/SPEC International Conference on Performance Engineering, 2023.

### Data center energy modeling

**PUE modeling** has been studied extensively in the data center literature. Dayarathna et al. (2016) survey energy consumption models for data centers, covering thermal models, workload-dependent cooling, and facility-level power distribution. Joulie's CoolingStress and PSUStress scores are simplified parametric models in this tradition, designed for O(1) per-node evaluation rather than full CFD simulation.

> M. Dayarathna, Y. Wen, and R. Fan. "Data Center Energy Consumption Modeling: A Survey." IEEE Communications Surveys & Tutorials, vol. 18, no. 1, pp. 732-794, 2016.

### Energy-proportional computing

Barroso and Holzle (2007) argued that servers should consume power proportional to their utilization, noting that real servers are far from energy-proportional: an idle server still draws 50-60% of peak power. This observation motivates Joulie's approach of consolidating workloads onto fewer, more-utilized nodes and capping idle nodes, rather than spreading load uniformly.

> L.A. Barroso and U. Holzle. "The Case for Energy-Proportional Computing." IEEE Computer, vol. 40, no. 12, pp. 33-37, 2007.

### Power-aware scheduling

Fan, Weber, and Barroso (2007) demonstrated that aggregate power consumption in warehouse-scale computers can be managed through power budgeting at the cluster level. Their work on power provisioning showed that statistical multiplexing of power across many machines allows significant oversubscription of power capacity. Joulie's PSUStress scoring applies a similar principle: it penalizes nodes that would push rack power consumption toward the provisioned limit.

> X. Fan, W.-D. Weber, and L.A. Barroso. "Power Provisioning for a Warehouse-Sized Computer." Proceedings of the International Symposium on Computer Architecture (ISCA), 2007.

### Digital twin for data centers

The concept of a digital twin for thermal-aware provisioning was introduced by Patel, Bash, and Sharma (2003), who proposed using thermal models to guide server placement in data centers. Joulie extends this idea to Kubernetes: the digital twin is a lightweight parametric model embedded in the controller manager, continuously updated from telemetry, and consumed by the scheduler in real time.

> C.D. Patel, C.E. Bash, and R. Sharma. "Thermal Considerations in Cooling Large Scale High Compute Density Data Centers." Proceedings of the International Symposium on High Performance Computer Architecture (HPCA), 2003.

### Kubernetes scheduling

Burns et al. (2016) trace the evolution from Borg through Omega to Kubernetes, describing how scheduling evolved from a monolithic model to an extensible, API-driven architecture. Joulie's scheduler extender leverages this extensibility: it participates in the standard scheduling cycle through HTTP hooks without forking or replacing the Kubernetes scheduler.

> B. Burns, B. Grant, D. Oppenheimer, E. Brewer, and J. Wilkes. "Borg, Omega, and Kubernetes." ACM Queue, vol. 14, no. 1, 2016.

### What Joulie adds

Prior work has addressed energy measurement, power-aware scheduling, and data center thermal modeling independently. Joulie's contribution is integrating these into a single Kubernetes-native feedback loop:

1. **RAPL and NVML telemetry** as OpenTelemetry metrics, or through Kepler, provides node-level power measurement, and Kepler adds per-container attribution.
2. **A digital twin** predicts the facility-level impact (cooling, PSU) of placement decisions, not just IT power.
3. **PUE-weighted scoring** makes the scheduler aware of marginal facility energy cost, not just compute availability.

No existing Kubernetes scheduler plugin combines real-time eBPF energy telemetry, a digital twin with facility-stress modeling, and workload classification into a single scheduling pipeline.

[Scoring Formula Validation]({{< relref "/docs/experiments/scoring-formula-validation.md" >}}) tests the formula at scale; its results do not stand yet, and it is being redone.

## What to read next

1. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
2. [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}})
