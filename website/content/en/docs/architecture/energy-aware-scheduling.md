---
title: "Energy-Aware Scheduling"
weight: 45
description: "How Joulie combines measured power (OpenTelemetry hardware metrics or Kepler), workload classification, digital twin predictions, and cooling-aware scoring to make energy-aware scheduling decisions."
---

This page follows one placement decision from the hardware counters to the chosen node. Each stage is a separate component that hands its result to the next through Prometheus or a CRD, and each links to the page that documents it.

## End-to-end pipeline

```
RAPL/NVML readings as OpenTelemetry metrics (or Kepler)
  -> Prometheus (OTLP receiver, or scrape)
    -> Digital twin (NodeTwin.status)
      -> Scheduler extender (filter + score)
        -> Placement decision
```

### Stage 1: Telemetry collection

CPU (RAPL) and GPU (NVML, ROCm SMI) power reach Prometheus as the OpenTelemetry metrics `hw.energy` and `hw.power`, or through Kepler: [Input metrics (OpenTelemetry)]({{< relref "/docs/architecture/telemetry.md#input-metrics-opentelemetry" >}}).

### Stage 2: Prometheus aggregation

The controller manager runs one grouped query per input every reconcile, over a `rate()` window of `PROMETHEUS_RATE_WINDOW` (5 min): [Prometheus settings]({{< relref "/docs/architecture/telemetry.md#prometheus-settings" >}}).

### Stage 3: Digital twin computation

The twin turns measured power and `NodeHardware` into headroom, cooling stress and PSU stress per node in `NodeTwin.status`: [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}}).

### Stage 4: Scheduler filter and scoring

The extender keeps performance pods off eco and draining nodes, then scores each node by the headroom left after this pod's estimated watts, cooling stress and power trend: [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}}).

### Stage 5: Placement

kube-scheduler adds the extender's priority to its own scores and binds the pod; resource fits stay Kubernetes' own: [Pod Compatibility]({{< relref "/docs/getting-started/03-pod-compatibility.md" >}}).

## Facility signals

Cooling stress is the score's only facility term: a node near its cooling limit loses up to 15 points even with spare headroom. The extender does not weight power by PUE, and PSU stress is computed but not scored yet.

[Scoring Formula Validation]({{< relref "/docs/experiments/scoring-formula-validation.md" >}}) tests the formula at scale; its results do not stand yet, and it is being redone.

## What to read next

1. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
2. [Digital Twin]({{< relref "/docs/architecture/digital-twin.md" >}})

The research this design builds on, with citations, and what Joulie adds: [Related work](https://github.com/joulie-k8s/Joulie/wiki/Related-Work) on the wiki; the scoring detail is on [Scheduler scoring](https://github.com/joulie-k8s/Joulie/wiki/Scheduler-Scoring).
