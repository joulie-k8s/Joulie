---
title: "Digital Twin"
weight: 35
---

The digital twin turns each node's measured power and hardware limits into a few scores the scheduler and the policy use. It is a closed-form model in the controller manager (`pkg/controller/twin`), computed every reconcile, and its output is `NodeTwin.status`.

## What the digital twin computes

| Field in `NodeTwin.status` | Meaning | Range | Read by |
|---|---|---|---|
| `predictedPowerHeadroomScore` | share of the node's capped power budget still unused | up to 100, negative when over budget | scheduler (fallback) |
| `predictedCoolingStressScore` | measured power over node TDP, scaled up in warm ambient air | 0 to 100 | scheduler |
| `predictedPsuStressScore` | rack (or cluster) power over a 50 kW reference capacity | 0 to 100 | nobody yet |
| `estimatedPUE` | cooling stress restated on a PUE scale | 1.05 to 1.40 | nobody yet |
| `schedulableClass` | `performance`, `eco` or `draining` | | scheduler, controller manager |
| `powerMeasurement` | measured power, TDP, capped budget and trend, per node | watts | scheduler |
| `conditions` | `AgentReporting`: whether an agent enforces the caps | | the `Agent` column of `kubectl get nodetwins` |

Every power score starts from the measured node power `powerMeasurement.measuredNodePowerW`, not from the caps.

```bash
kubectl get nodetwin <node> -o jsonpath='{.status.powerMeasurement}'
```

## Where the power reading comes from

`NODE_POWER_SOURCE` on the controller manager selects it, and `powerMeasurement.source` says which one answered:

| `NODE_POWER_SOURCE` | Reads | `source` on failure |
|---|---|---|
| `prometheus` (chart default) | one query per reconcile: `hw.energy` and `hw.power` of the CPU packages and GPUs ([Input metrics (OpenTelemetry)]({{< relref "/docs/architecture/telemetry.md#input-metrics-opentelemetry" >}})), or Kepler with `NODE_POWER_PRESET` | `prometheus-no-data` |
| `http` | `NODE_POWER_HTTP_ENDPOINT`, one OTLP JSON document per node (the simulator) | `http-error`, `http-no-endpoint` |
| `static` (code default) | nothing: 0 W, so every node reads headroom 100 and stress 0 | |

`NODE_POWER_SCOPE=components` (default) covers the CPU packages and GPUs, the same boundary as the TDP; `host` reads the whole host. Configure a source before treating the scores as a ranking signal.

## Node TDP and the capped budget

The twin derives two budgets from `NodeHardware`: the node TDP (CPU max watts per socket times sockets, plus GPU max watts times GPU power domains) and the capped budget (the same with the caps of `NodeTwin.spec` applied). A GPU cap counts only where it applies (not when the agent reports it `blocked` or `error`), and the CPU budget never goes below the bottom of the CPU cap range. Both appear in `powerMeasurement` as `nodeTdpW` and `nodeCappedPowerW`.

## Cooling stress

Measured power over node TDP, times an ambient multiplier that is 1 at 20 °C or with no reading and rises to 2 at 45 °C. Facility metrics are off by default (`ENABLE_FACILITY_METRICS=false`), so the multiplier is usually 1; with them on, the twin uses the node's inlet temperature, then its cooling zone's, then the outdoor one ([Facility metrics]({{< relref "/docs/getting-started/05-configuration-reference.md#facility-metrics" >}})).

## PSU stress and estimated PUE

PSU stress is per rack when nodes carry `joulie.io/rack` and cluster-wide otherwise; it is 0 on a default install. Estimated PUE is 1.05 plus 0.35 times cooling stress over 100. Neither enters the scheduler's score.

## How it feeds the scheduler

The scheduler extender caches `NodeTwin` (30 s) and uses `schedulableClass` to filter and `powerMeasurement` plus cooling stress to score ([Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})).

## How it feeds the controller manager

`schedulableClass` is `draining` while a node moving from performance to eco still runs performance pods. The twin reads no governor field ([Governor policy]({{< relref "/docs/architecture/policy.md#governor-policy" >}})).

## What to read next

1. [Scheduler Extender]({{< relref "/docs/architecture/scheduler.md" >}})
2. [Joulie Controller Manager]({{< relref "/docs/architecture/controller-manager.md" >}})
3. [CRD and Policy Model]({{< relref "/docs/architecture/policy.md" >}})
4. [Hardware Modeling]({{< relref "/docs/hardware/hardware-modeling.md" >}}): the power profiles behind the TDP

Formulas, worked examples and the field-by-field `powerMeasurement` reference: [Digital twin model](https://github.com/joulie-k8s/Joulie/wiki/Digital-Twin-Model) and [Cooling and PUE](https://github.com/joulie-k8s/Joulie/wiki/Cooling-and-PUE) on the wiki.
